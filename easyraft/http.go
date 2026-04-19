package easyraft

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

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
		s.writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
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
		s.writeError(w, http.StatusNotFound, "collection not found")
		return
	}
	raw, ok := coll[key]
	if !ok {
		s.writeError(w, http.StatusNotFound, ErrKeyNotFound.Error())
		return
	}

	s.writeJSON(w, http.StatusOK, raw)
}

func (s *Store) handleUpdate(w http.ResponseWriter, r *http.Request) {
	collection := r.PathValue("collection")
	key := r.PathValue("key")

	var val json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&val); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
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
		s.writeJSON(w, http.StatusOK, make(map[string]json.RawMessage))
		return
	}

	s.writeJSON(w, http.StatusOK, coll)
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
		s.writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
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
	s.writeJSON(w, http.StatusOK, status)
}

func (s *Store) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("OK"))
}

func (s *Store) handleRPCError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, ErrNotLeader) {
		leaderID := s.node.Leader()
		if leaderID == "" {
			s.writeError(w, http.StatusServiceUnavailable, "no leader currently elected")
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
			// If we have a functional address, use it for a real redirect.
			// We only redirect if it looks like a full URL or host:port.
			url := leaderAddr
			if !strings.HasPrefix(url, "http") {
				url = "http://" + url
			}
			// Append the current path and query
			url += r.URL.Path
			if r.URL.RawQuery != "" {
				url += "?" + r.URL.RawQuery
			}
			w.Header().Set("Location", url)
			s.writeError(w, http.StatusTemporaryRedirect, fmt.Sprintf("not leader; redirecting to %s", url))
			return
		}

		s.writeError(w, http.StatusTemporaryRedirect, fmt.Sprintf("not leader; leader is %s", leaderID))
		return
	}

	if errors.Is(err, ErrKeyNotFound) {
		s.writeError(w, http.StatusNotFound, err.Error())
		return
	}

	if errors.Is(err, ErrKeyExists) {
		s.writeError(w, http.StatusConflict, err.Error())
		return
	}

	if errors.Is(err, context.DeadlineExceeded) {
		s.writeError(w, http.StatusRequestTimeout, err.Error())
		return
	}

	s.writeError(w, http.StatusInternalServerError, err.Error())
}

func (s *Store) writeError(w http.ResponseWriter, code int, msg string) {
	s.writeJSON(w, code, struct {
		Error string `json:"error"`
	}{Error: msg})
}

func (s *Store) writeJSON(w http.ResponseWriter, code int, val any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(val); err != nil {
		if s.cfg.Logger != nil {
			s.cfg.Logger.Error("HTTP encode failed", "err", err)
		}
	}
}
