package easyraft

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/brunoga/raft"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func (s *Store) serveHTTP() error {
	if s.cfg.HTTPAddr == "" {
		return nil
	}

	mux := http.NewServeMux()

	// Multi-collection routing: /{collection}/{key}
	mux.HandleFunc("POST /{collection}/{key}", s.handleCreate)
	mux.HandleFunc("GET /{collection}/{key}", s.handleRead)
	mux.HandleFunc("PUT /{collection}/{key}", s.handleUpdate)
	mux.HandleFunc("DELETE /{collection}/{key}", s.handleDelete)
	mux.HandleFunc("GET /{collection}", s.handleList)
	mux.HandleFunc("POST /{collection}/{key}/mutate", s.handleMutate)

	mux.HandleFunc("GET /status", s.handleStatus)
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.Handle("GET /metrics", promhttp.Handler())

	s.httpServer = &http.Server{
		Addr:         s.cfg.HTTPAddr,
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  30 * time.Second,
	}

	go func() {
		if err := s.httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			if s.cfg.Logger != nil {
				s.cfg.Logger.Error("HTTP server failed", "err", err)
			}
		}
	}()

	return nil
}

func (s *Store) handleCreate(w http.ResponseWriter, r *http.Request) {
	collection := r.PathValue("collection")
	key := r.PathValue("key")

	var val json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&val); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error(), s.cfg.Logger)
		return
	}

	_, err := s.propose(r.Context(), &command{
		Op:         opCreate,
		Collection: collection,
		Key:        key,
		Value:      val,
	})
	if err != nil {
		s.handleRPCError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

func (s *Store) handleRead(w http.ResponseWriter, r *http.Request) {
	collection := r.PathValue("collection")
	key := r.PathValue("key")
	stale := r.URL.Query().Get("consistency") == "stale"

	if !stale {
		if _, err := s.node.ReadIndexLease(r.Context()); err != nil {
			s.handleRPCError(w, r, err)
			return
		}
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	coll := s.collections[collection]
	if coll == nil {
		writeError(w, http.StatusNotFound, "collection not found", s.cfg.Logger)
		return
	}
	raw, ok := coll[key]
	if !ok {
		writeError(w, http.StatusNotFound, ErrKeyNotFound.Error(), s.cfg.Logger)
		return
	}

	writeJSON(w, http.StatusOK, raw, s.cfg.Logger)
}

func (s *Store) handleUpdate(w http.ResponseWriter, r *http.Request) {
	collection := r.PathValue("collection")
	key := r.PathValue("key")

	var val json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&val); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error(), s.cfg.Logger)
		return
	}

	_, err := s.propose(r.Context(), &command{
		Op:         opUpdate,
		Collection: collection,
		Key:        key,
		Value:      val,
	})
	if err != nil {
		s.handleRPCError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Store) handleDelete(w http.ResponseWriter, r *http.Request) {
	collection := r.PathValue("collection")
	key := r.PathValue("key")

	_, err := s.propose(r.Context(), &command{
		Op:         opDelete,
		Collection: collection,
		Key:        key,
	})
	if err != nil {
		s.handleRPCError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Store) handleList(w http.ResponseWriter, r *http.Request) {
	collection := r.PathValue("collection")
	stale := r.URL.Query().Get("consistency") == "stale"

	if !stale {
		if _, err := s.node.ReadIndexLease(r.Context()); err != nil {
			s.handleRPCError(w, r, err)
			return
		}
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	coll := s.collections[collection]
	if coll == nil {
		writeJSON(w, http.StatusOK, make(map[string]json.RawMessage), s.cfg.Logger)
		return
	}

	writeJSON(w, http.StatusOK, coll, s.cfg.Logger)
}

type mutateRequest struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

func (s *Store) handleMutate(w http.ResponseWriter, r *http.Request) {
	collection := r.PathValue("collection")
	key := r.PathValue("key")

	var req mutateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error(), s.cfg.Logger)
		return
	}

	resp, err := s.propose(r.Context(), &command{
		Op:         opMutate,
		Collection: collection,
		Key:        key,
		MutateName: req.Name,
		MutateArgs: req.Args,
	})
	if err != nil {
		s.handleRPCError(w, r, err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(resp)
}

func (s *Store) handleStatus(w http.ResponseWriter, _ *http.Request) {
	status := s.node.Status()
	writeJSON(w, http.StatusOK, status, s.cfg.Logger)
}

func (s *Store) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("OK"))
}

func (s *Store) handleRPCError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, ErrNotLeader) {
		leaderID := s.node.Leader()
		if leaderID == "" {
			writeError(w, http.StatusServiceUnavailable, "no leader currently elected", s.cfg.Logger)
			return
		}

		s.mu.RLock()
		var leaderAddr string
		if coll := s.collections["__easyraft_metadata__"]; coll != nil {
			if raw, ok := coll[string(leaderID)]; ok {
				_ = json.Unmarshal(raw, &leaderAddr)
			}
		}
		s.mu.RUnlock()

		if leaderAddr != "" {
			url := leaderAddr
			if !strings.HasPrefix(url, "http") {
				url = "http://" + url
			}
			url += r.URL.Path
			if r.URL.RawQuery != "" {
				url += "?" + r.URL.RawQuery
			}
			w.Header().Set("Location", url)
			writeError(w, http.StatusTemporaryRedirect, fmt.Sprintf("not leader; redirecting to %s", url), s.cfg.Logger)
			return
		}

		// Leader is known but its HTTP address has not been advertised yet.
		writeError(w, http.StatusServiceUnavailable, fmt.Sprintf("not leader; leader is %s but its HTTP address is not yet known", leaderID), s.cfg.Logger)
		return
	}

	if errors.Is(err, ErrKeyNotFound) {
		writeError(w, http.StatusNotFound, err.Error(), s.cfg.Logger)
		return
	}

	if errors.Is(err, ErrKeyExists) {
		writeError(w, http.StatusConflict, err.Error(), s.cfg.Logger)
		return
	}

	if errors.Is(err, context.DeadlineExceeded) {
		writeError(w, http.StatusRequestTimeout, err.Error(), s.cfg.Logger)
		return
	}

	writeError(w, http.StatusInternalServerError, err.Error(), s.cfg.Logger)
}

func (m *Manager) serveHTTP() error {
	if m.cfg.HTTPAddr == "" {
		return nil
	}

	mux := http.NewServeMux()

	// Multi-Raft routing: /groups/{groupID}/{collection}/{key}
	mux.HandleFunc("POST /groups/{groupID}/{collection}/{key}", m.handleCreate)
	mux.HandleFunc("GET /groups/{groupID}/{collection}/{key}", m.handleRead)
	mux.HandleFunc("PUT /groups/{groupID}/{collection}/{key}", m.handleUpdate)
	mux.HandleFunc("DELETE /groups/{groupID}/{collection}/{key}", m.handleDelete)
	mux.HandleFunc("GET /groups/{groupID}/{collection}", m.handleList)
	mux.HandleFunc("POST /groups/{groupID}/{collection}/{key}/mutate", m.handleMutate)

	mux.HandleFunc("GET /status", m.handleStatus)
	mux.HandleFunc("GET /health", m.handleHealth)
	mux.Handle("GET /metrics", promhttp.Handler())

	m.httpServer = &http.Server{
		Addr:         m.cfg.HTTPAddr,
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  30 * time.Second,
	}

	go func() {
		if err := m.httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			if m.cfg.Logger != nil {
				m.cfg.Logger.Error("Manager HTTP server failed", "err", err)
			}
		}
	}()

	return nil
}

func (m *Manager) getStore(r *http.Request) (*Store, error) {
	gidStr := r.PathValue("groupID")
	var gid uint64
	if _, err := fmt.Sscanf(gidStr, "%d", &gid); err != nil {
		return nil, fmt.Errorf("invalid groupID: %w", err)
	}

	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.stores[gid]
	if !ok {
		return nil, raft.ErrGroupNotFound
	}
	return s, nil
}

func (m *Manager) handleCreate(w http.ResponseWriter, r *http.Request) {
	s, err := m.getStore(r)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error(), m.cfg.Logger)
		return
	}
	s.handleCreate(w, r)
}

func (m *Manager) handleRead(w http.ResponseWriter, r *http.Request) {
	s, err := m.getStore(r)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error(), m.cfg.Logger)
		return
	}
	s.handleRead(w, r)
}

func (m *Manager) handleUpdate(w http.ResponseWriter, r *http.Request) {
	s, err := m.getStore(r)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error(), m.cfg.Logger)
		return
	}
	s.handleUpdate(w, r)
}

func (m *Manager) handleDelete(w http.ResponseWriter, r *http.Request) {
	s, err := m.getStore(r)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error(), m.cfg.Logger)
		return
	}
	s.handleDelete(w, r)
}

func (m *Manager) handleList(w http.ResponseWriter, r *http.Request) {
	s, err := m.getStore(r)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error(), m.cfg.Logger)
		return
	}
	s.handleList(w, r)
}

func (m *Manager) handleMutate(w http.ResponseWriter, r *http.Request) {
	s, err := m.getStore(r)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error(), m.cfg.Logger)
		return
	}
	s.handleMutate(w, r)
}

func (m *Manager) handleStatus(w http.ResponseWriter, r *http.Request) {
	status := m.mgr.StatusAll()
	writeJSON(w, http.StatusOK, status, m.cfg.Logger)
}

func (m *Manager) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("OK"))
}

// writeJSON sends a JSON-encoded response with the given status code.
func writeJSON(w http.ResponseWriter, code int, val any, logger *slog.Logger) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(val); err != nil && logger != nil {
		logger.Error("HTTP encode failed", "err", err)
	}
}

// writeError sends a JSON error response with the given status code.
func writeError(w http.ResponseWriter, code int, msg string, logger *slog.Logger) {
	writeJSON(w, code, struct {
		Error string `json:"error"`
	}{Error: msg}, logger)
}
