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

// ---- Join types ------------------------------------------------------------

// joinRequest is the body of a POST /join request. The joining node sends its
// own Raft node ID, gRPC address, and voter preference.
type joinRequest struct {
	ID       raft.NodeID `json:"id"`
	RaftAddr string      `json:"raft_addr"`
	// Voter indicates whether the joining node should be a voting member.
	// Omit or set true for a normal voter; set false for a learner/observer.
	// Defaults to true when omitted.
	Voter *bool `json:"voter,omitempty"`
}

// joinPeer describes one cluster member returned in a join response.
type joinPeer struct {
	ID       raft.NodeID `json:"id"`
	RaftAddr string      `json:"raft_addr"`
	Voter    bool        `json:"voter"`
}

// joinResponse is returned by POST /join on success.
type joinResponse struct {
	Peers []joinPeer `json:"peers"`
}

// ---- Member types ----------------------------------------------------------

// memberInfo describes one cluster member as returned by GET /members.
type memberInfo struct {
	ID       raft.NodeID `json:"id"`
	RaftAddr string      `json:"raft_addr"`
	Voter    bool        `json:"voter"`
	Leader   bool        `json:"leader"`
	Self     bool        `json:"self"`
}

// membersResponse is the body of a GET /members response.
type membersResponse struct {
	Members []memberInfo `json:"members"`
}

// ---- Transfer-leadership type ----------------------------------------------

// transferLeadershipRequest is the body of a POST /transfer-leadership request.
type transferLeadershipRequest struct {
	To raft.NodeID `json:"to"`
}

// ---- Batch types -----------------------------------------------------------

// batchOp is one operation in a POST /batch request.
type batchOp struct {
	Op         opType          `json:"op"`
	Collection string          `json:"collection"`
	Key        string          `json:"key"`
	Value      json.RawMessage `json:"value,omitempty"`
	MutateName string          `json:"mutate_name,omitempty"`
	MutateArgs json.RawMessage `json:"mutate_args,omitempty"`
}

// ---- Store HTTP server -----------------------------------------------------

// registerRoutes registers all Store management and CRUD routes on mux.
func (s *Store) registerRoutes(mux *http.ServeMux) {
	// Cluster management — registered before wildcards to take priority.
	mux.HandleFunc("POST /join", s.handleJoin)
	mux.HandleFunc("GET /members", s.handleMembers)
	mux.HandleFunc("DELETE /members/{id}", s.handleRemoveMember)
	mux.HandleFunc("POST /transfer-leadership", s.handleTransferLeadership)
	mux.HandleFunc("POST /batch", s.handleBatch)

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
}

func (s *Store) serveHTTP() error {
	// If the caller provided their own mux, register routes there and let them
	// start the server. This avoids a port conflict when the application runs
	// its own HTTP server on the same address.
	if s.cfg.HTTPMux != nil {
		s.registerRoutes(s.cfg.HTTPMux)
		return nil
	}

	if s.cfg.HTTPAddr == "" {
		return nil
	}

	mux := http.NewServeMux()
	s.registerRoutes(mux)

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

// handleJoin handles POST /join. A new node posts its ID and Raft address;
// this node registers it with the transport and calls AddServer. On success
// the current cluster peer list (IDs + Raft addresses) is returned so the
// joiner can bootstrap its own transport.
//
// If this node is not the leader the request is redirected to the leader
// exactly like any other write operation.
func (s *Store) handleJoin(w http.ResponseWriter, r *http.Request) {
	var req joinRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error(), s.cfg.Logger)
		return
	}
	if req.ID == "" || req.RaftAddr == "" {
		writeError(w, http.StatusBadRequest, "id and raft_addr are required", s.cfg.Logger)
		return
	}

	voter := true
	if req.Voter != nil {
		voter = *req.Voter
	}

	// Register the joiner's Raft address with our transport so RPCs can be routed.
	if pa, ok := s.transport.(peerAdder); ok {
		pa.AddPeer(req.ID, req.RaftAddr)
	}

	// Propose the membership change. Only the leader can commit this; all other
	// nodes will return ErrNotLeader and the client will follow the redirect.
	addCtx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if err := s.node.AddServer(addCtx, raft.PeerConfig{ID: req.ID, Voter: voter}); err != nil {
		s.handleRPCError(w, r, err)
		return
	}

	// Record the new peer so future join responses and GET /members include it.
	s.mu.Lock()
	s.raftPeers[req.ID] = raftPeerInfo{addr: req.RaftAddr, voter: voter}
	peers := s.currentPeers()
	s.mu.Unlock()

	writeJSON(w, http.StatusOK, joinResponse{Peers: peers}, s.cfg.Logger)
}

// currentPeers returns a snapshot of all known peers including self.
// Must be called with s.mu held (at least RLock).
func (s *Store) currentPeers() []joinPeer {
	peers := make([]joinPeer, 0, len(s.raftPeers)+1)
	for id, p := range s.raftPeers {
		if id == s.cfg.ID {
			continue
		}
		peers = append(peers, joinPeer{ID: id, RaftAddr: p.addr, Voter: p.voter})
	}
	// Always include self so the joining node can route RPCs back to us.
	if s.cfg.RaftAddr != "" {
		peers = append(peers, joinPeer{ID: s.cfg.ID, RaftAddr: s.cfg.RaftAddr, Voter: true})
	}
	return peers
}

// handleMembers handles GET /members. Returns all known cluster members with
// their Raft addresses, voter status, and whether each is the current leader.
func (s *Store) handleMembers(w http.ResponseWriter, r *http.Request) {
	leaderID := s.node.Leader()

	s.mu.RLock()
	members := make([]memberInfo, 0, len(s.raftPeers)+1)
	for id, p := range s.raftPeers {
		if id == s.cfg.ID {
			continue
		}
		members = append(members, memberInfo{
			ID:       id,
			RaftAddr: p.addr,
			Voter:    p.voter,
			Leader:   id == leaderID,
		})
	}
	s.mu.RUnlock()

	// Always include self.
	members = append(members, memberInfo{
		ID:       s.cfg.ID,
		RaftAddr: s.cfg.RaftAddr,
		Voter:    true,
		Leader:   s.cfg.ID == leaderID,
		Self:     true,
	})

	writeJSON(w, http.StatusOK, membersResponse{Members: members}, s.cfg.Logger)
}

// handleRemoveMember handles DELETE /members/{id}. Removes the named node from
// the Raft cluster. Must be called on the leader; followers redirect.
func (s *Store) handleRemoveMember(w http.ResponseWriter, r *http.Request) {
	id := raft.NodeID(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "id is required", s.cfg.Logger)
		return
	}

	rmCtx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if err := s.RemoveServer(rmCtx, id); err != nil {
		s.handleRPCError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleTransferLeadership handles POST /transfer-leadership.
// Body: {"to": "<nodeID>"}. Gracefully hands off leadership to the named node.
func (s *Store) handleTransferLeadership(w http.ResponseWriter, r *http.Request) {
	var req transferLeadershipRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error(), s.cfg.Logger)
		return
	}
	if req.To == "" {
		writeError(w, http.StatusBadRequest, "to is required", s.cfg.Logger)
		return
	}

	tlCtx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if err := s.TransferLeadership(tlCtx, req.To); err != nil {
		s.handleRPCError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleBatch handles POST /batch. Accepts a JSON array of operations that are
// committed as a single atomic log entry. Returns a JSON array of results in
// the same order: null for CRUD operations, the mutation result for mutates.
//
// Request body:
//
//	[{"op":"create","collection":"col","key":"k","value":{...}}, ...]
func (s *Store) handleBatch(w http.ResponseWriter, r *http.Request) {
	var ops []batchOp
	if err := json.NewDecoder(r.Body).Decode(&ops); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error(), s.cfg.Logger)
		return
	}
	if len(ops) == 0 {
		writeJSON(w, http.StatusOK, []json.RawMessage{}, s.cfg.Logger)
		return
	}

	cmds := make([]command, len(ops))
	for i, op := range ops {
		cmds[i] = command{
			Op:         op.Op,
			Collection: op.Collection,
			Key:        op.Key,
			Value:      op.Value,
			MutateName: op.MutateName,
			MutateArgs: op.MutateArgs,
		}
	}

	raw, err := s.propose(r.Context(), &command{Op: opBatch, Batch: cmds})
	if err != nil {
		s.handleRPCError(w, r, err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
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

// ---- Manager HTTP server ---------------------------------------------------

func (m *Manager) serveHTTP() error {
	if m.cfg.HTTPAddr == "" {
		return nil
	}

	mux := http.NewServeMux()

	// Cluster management per Raft group.
	mux.HandleFunc("POST /groups/{groupID}/join", m.handleJoin)
	mux.HandleFunc("GET /groups/{groupID}/members", m.handleMembers)
	mux.HandleFunc("DELETE /groups/{groupID}/members/{id}", m.handleRemoveMember)
	mux.HandleFunc("POST /groups/{groupID}/transfer-leadership", m.handleTransferLeadership)
	mux.HandleFunc("POST /groups/{groupID}/batch", m.handleBatch)

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

func (m *Manager) handleJoin(w http.ResponseWriter, r *http.Request) {
	s, err := m.getStore(r)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error(), m.cfg.Logger)
		return
	}
	s.handleJoin(w, r)
}

func (m *Manager) handleMembers(w http.ResponseWriter, r *http.Request) {
	s, err := m.getStore(r)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error(), m.cfg.Logger)
		return
	}
	s.handleMembers(w, r)
}

func (m *Manager) handleRemoveMember(w http.ResponseWriter, r *http.Request) {
	s, err := m.getStore(r)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error(), m.cfg.Logger)
		return
	}
	s.handleRemoveMember(w, r)
}

func (m *Manager) handleTransferLeadership(w http.ResponseWriter, r *http.Request) {
	s, err := m.getStore(r)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error(), m.cfg.Logger)
		return
	}
	s.handleTransferLeadership(w, r)
}

func (m *Manager) handleBatch(w http.ResponseWriter, r *http.Request) {
	s, err := m.getStore(r)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error(), m.cfg.Logger)
		return
	}
	s.handleBatch(w, r)
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

// writeSSEEvent writes a single Server-Sent Event to w. v is JSON-encoded as
// the event data. Errors encoding v are silently ignored (the connection will
// be detected as stale on the next write).
func writeSSEEvent(w http.ResponseWriter, event string, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
}
