package easyraft

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
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

// ---- Authorization middleware ----------------------------------------------

// authorized wraps h with the configured authorization hook. Every easyraft
// route goes through it, so a single hook covers reads, writes, and the
// cluster-mutating endpoints alike.
//
// A nil hook admits everything; that configuration is reported once at startup
// by warnIfHTTPUnauthenticated.
func authorized(auth func(*http.Request) error, logger *slog.Logger, h http.HandlerFunc) http.HandlerFunc {
	if auth == nil {
		return h
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if err := auth(r); err != nil {
			code := authStatus(err)
			logger.Warn("easyraft: HTTP request denied",
				"method", r.Method, "path", r.URL.Path, "status", code, "err", err)
			// Report the status only: the hook's message may describe why the
			// credential failed, which is not the caller's business.
			writeError(w, code, http.StatusText(code), logger)
			return
		}
		h(w, r)
	}
}

// warnIfHTTPUnauthenticated logs, once per store or manager, that the HTTP API
// is being served without an authorization hook. The API can add and remove
// cluster members, so an unauthenticated listener is a cluster-control plane
// open to anyone who can reach it.
func warnIfHTTPUnauthenticated(cfg *config, logger *slog.Logger) {
	if cfg.HTTPAuth != nil || cfg.AcknowledgeInsecureHTTP {
		return
	}
	logger.Warn("easyraft: the HTTP API is being served WITHOUT authentication. "+
		"Anyone who can reach this listener can add or remove cluster members, "+
		"transfer leadership, and write to every collection. "+
		"Configure easyraft.WithHTTPAuth or easyraft.WithBearerTokenAuth, "+
		"or acknowledge the exposure with easyraft.WithInsecureHTTPAcknowledged.",
		"addr", cfg.HTTPAddr)
}

// warnIfHTTPUnreachable logs that this node's HTTP API is served on an address
// no peer can redirect a client to.
//
// A follower that receives a write answers 307 pointing at the leader's
// advertised HTTP address. If that address names no host, there is no URL to
// point at and the follower answers 503 instead -- so every write sent to a
// follower fails, and only a client that already knows which node leads gets
// through. It works on one node and stops working on three, which is the worst
// time to find out.
func warnIfHTTPUnreachable(addr string, logger *slog.Logger) {
	if addr == "" || advertisableHost(addr) {
		return
	}
	logger.Warn("easyraft: the HTTP API is advertised on an address peers cannot redirect to, "+
		"so writes sent to a follower will fail with 503 rather than being forwarded to the "+
		"leader. Give WithHTTPAddr a reachable host:port, or set WithAdvertiseHTTPAddr.",
		"advertised", addr)
}

// ---- Store HTTP server -----------------------------------------------------

// registerRoutes registers all Store management and CRUD routes on mux.
// Every route is wrapped with the configured authorization hook.
func (s *Store) registerRoutes(mux *http.ServeMux) {
	logger := s.logger()
	guard := func(h http.HandlerFunc) http.HandlerFunc {
		return authorized(s.cfg.HTTPAuth, logger, h)
	}

	// Cluster management — registered before wildcards to take priority.
	mux.HandleFunc("POST /join", guard(s.handleJoin))
	mux.HandleFunc("GET /members", guard(s.handleMembers))
	mux.HandleFunc("DELETE /members/{id}", guard(s.handleRemoveMember))
	mux.HandleFunc("POST /transfer-leadership", guard(s.handleTransferLeadership))
	mux.HandleFunc("POST /batch", guard(s.handleBatch))

	// Multi-collection routing: /{collection}/{key}
	mux.HandleFunc("POST /{collection}/{key}", guard(s.handleCreate))
	mux.HandleFunc("GET /{collection}/{key}", guard(s.handleRead))
	mux.HandleFunc("PUT /{collection}/{key}", guard(s.handleUpdate))
	mux.HandleFunc("PATCH /{collection}/{key}", guard(s.handleUpsert))
	mux.HandleFunc("DELETE /{collection}/{key}", guard(s.handleDelete))
	mux.HandleFunc("GET /{collection}", guard(s.handleList))
	mux.HandleFunc("POST /{collection}/{key}/mutate", guard(s.handleMutate))

	mux.HandleFunc("GET /status", guard(s.handleStatus))
	mux.HandleFunc("GET /health", guard(s.handleHealth))
	mux.Handle("GET /metrics", guard(promhttp.Handler().ServeHTTP))
}

// initHTTP binds the HTTP listener so a bad or busy address fails at
// construction time rather than disappearing into a background goroutine.
// It does nothing when the caller supplied their own mux or no address.
func (s *Store) initHTTP() error {
	if s.cfg.HTTPMux != nil || s.cfg.HTTPAddr == "" {
		return nil
	}
	ln, err := net.Listen("tcp", s.cfg.HTTPAddr)
	if err != nil {
		return fmt.Errorf("easyraft: listen http %s: %w", s.cfg.HTTPAddr, err)
	}
	s.httpListener = ln
	return nil
}

// serveHTTP starts serving the routes registered for this store.
//
// It returns nothing because there is nothing left to report: the listener was
// already bound by initHTTP, so a bad or occupied address failed [NewStore].
// What remains can only go wrong once the server is accepting connections,
// which is after [Store.Start] has returned, so it is logged.
func (s *Store) serveHTTP() {
	logger := s.logger()

	// If the caller provided their own mux, register routes there and let them
	// start the server. This avoids a port conflict when the application runs
	// its own HTTP server on the same address.
	if s.cfg.HTTPMux != nil {
		warnIfHTTPUnauthenticated(&s.cfg, logger)
		warnIfHTTPUnreachable(s.resolvedHTTPAddr(), logger)
		s.registerRoutes(s.cfg.HTTPMux)
		return
	}

	if s.httpListener == nil {
		return
	}
	warnIfHTTPUnauthenticated(&s.cfg, logger)
	warnIfHTTPUnreachable(s.resolvedHTTPAddr(), logger)

	mux := http.NewServeMux()
	s.registerRoutes(mux)

	s.httpServer = &http.Server{
		Addr:         s.cfg.HTTPAddr,
		Handler:      mux,
		TLSConfig:    s.cfg.HTTPTLS,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  30 * time.Second,
	}

	ln := s.httpListener
	server := s.httpServer
	go func() {
		if err := serveOn(server, ln, s.cfg.HTTPTLS); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("easyraft: HTTP server stopped", "addr", server.Addr, "err", err)
		}
	}()
}

// serveOn serves srv on ln, over TLS when a config is supplied. Certificates
// come from the tls.Config, so no certificate file paths are needed.
func serveOn(srv *http.Server, ln net.Listener, tlsCfg *tls.Config) error {
	if tlsCfg != nil {
		return srv.ServeTLS(ln, "", "")
	}
	return srv.Serve(ln)
}

// collectionParam returns the validated {collection} path value. Reserved
// collections are refused outright: they hold easyraft's own cluster metadata,
// and letting a client write one would let it choose where leader redirects
// point.
func (s *Store) collectionParam(w http.ResponseWriter, r *http.Request) (string, bool) {
	name := r.PathValue("collection")
	if isReservedCollection(name) {
		writeError(w, http.StatusForbidden, ErrReservedCollection.Error(), s.logger())
		return "", false
	}
	return name, true
}

func (s *Store) handleCreate(w http.ResponseWriter, r *http.Request) {
	collection, ok := s.collectionParam(w, r)
	if !ok {
		return
	}
	key := r.PathValue("key")

	var val json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&val); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error(), s.logger())
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
	collection, ok := s.collectionParam(w, r)
	if !ok {
		return
	}
	key := r.PathValue("key")
	stale := r.URL.Query().Get("consistency") == "stale"

	if !stale {
		if err := s.readIndex(r.Context()); err != nil {
			s.handleRPCError(w, r, err)
			return
		}
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	coll := s.collections[collection]
	if coll == nil {
		writeError(w, http.StatusNotFound, "collection not found", s.logger())
		return
	}
	raw, found := coll[key]
	if !found {
		writeError(w, http.StatusNotFound, ErrKeyNotFound.Error(), s.logger())
		return
	}

	writeJSON(w, http.StatusOK, raw, s.logger())
}

func (s *Store) handleUpdate(w http.ResponseWriter, r *http.Request) {
	collection, ok := s.collectionParam(w, r)
	if !ok {
		return
	}
	key := r.PathValue("key")

	var val json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&val); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error(), s.logger())
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

func (s *Store) handleUpsert(w http.ResponseWriter, r *http.Request) {
	collection, ok := s.collectionParam(w, r)
	if !ok {
		return
	}
	key := r.PathValue("key")

	var val json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&val); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error(), s.logger())
		return
	}

	_, err := s.propose(r.Context(), &command{
		Op:         opUpsert,
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
	collection, ok := s.collectionParam(w, r)
	if !ok {
		return
	}
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
	collection, ok := s.collectionParam(w, r)
	if !ok {
		return
	}
	stale := r.URL.Query().Get("consistency") == "stale"

	if !stale {
		if err := s.readIndex(r.Context()); err != nil {
			s.handleRPCError(w, r, err)
			return
		}
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	coll := s.collections[collection]
	if coll == nil {
		writeJSON(w, http.StatusOK, make(map[string]json.RawMessage), s.logger())
		return
	}

	writeJSON(w, http.StatusOK, coll, s.logger())
}

type mutateRequest struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

func (s *Store) handleMutate(w http.ResponseWriter, r *http.Request) {
	collection, ok := s.collectionParam(w, r)
	if !ok {
		return
	}
	key := r.PathValue("key")

	var req mutateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error(), s.logger())
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
// Adding a member changes the quorum, so this endpoint must be protected by an
// authorization hook; see [WithHTTPAuth].
//
// If this node is not the leader the request is redirected to the leader
// exactly like any other write operation.
func (s *Store) handleJoin(w http.ResponseWriter, r *http.Request) {
	var req joinRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error(), s.logger())
		return
	}
	if req.ID == "" || req.RaftAddr == "" {
		writeError(w, http.StatusBadRequest, "id and raft_addr are required", s.logger())
		return
	}
	if _, ok := normalizeHostPort(req.RaftAddr); !ok {
		writeError(w, http.StatusBadRequest, "raft_addr must be host:port", s.logger())
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

	s.logger().Info("easyraft: member joined", "id", req.ID, "raft_addr", req.RaftAddr, "voter", voter)
	writeJSON(w, http.StatusOK, joinResponse{Peers: peers}, s.logger())
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

// handleMembers handles GET /members. Returns all committed cluster members
// (from Node.Members(), which reflects the last applied config entry) with
// their Raft addresses, voter status, and whether each is the current leader.
func (s *Store) handleMembers(w http.ResponseWriter, _ *http.Request) {
	leaderID := s.node.Leader()
	raftMembers := s.node.Members() // authoritative: committed membership only

	s.mu.RLock()
	members := make([]memberInfo, 0, len(raftMembers))
	for _, p := range raftMembers {
		var addr string
		if p.ID == s.cfg.ID {
			addr = s.cfg.RaftAddr
		} else if info, ok := s.raftPeers[p.ID]; ok {
			addr = info.addr
		}
		members = append(members, memberInfo{
			ID:       p.ID,
			RaftAddr: addr,
			Voter:    p.Voter,
			Leader:   p.ID == leaderID,
			Self:     p.ID == s.cfg.ID,
		})
	}
	s.mu.RUnlock()

	writeJSON(w, http.StatusOK, membersResponse{Members: members}, s.logger())
}

// handleRemoveMember handles DELETE /members/{id}. Removes the named node from
// the Raft cluster. Must be called on the leader; followers redirect.
//
// Removing a member shrinks the quorum, so this endpoint must be protected by
// an authorization hook; see [WithHTTPAuth].
func (s *Store) handleRemoveMember(w http.ResponseWriter, r *http.Request) {
	id := raft.NodeID(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "id is required", s.logger())
		return
	}

	rmCtx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if err := s.RemoveServer(rmCtx, id); err != nil {
		s.handleRPCError(w, r, err)
		return
	}
	s.logger().Info("easyraft: member removed", "id", id)
	w.WriteHeader(http.StatusNoContent)
}

// handleTransferLeadership handles POST /transfer-leadership.
// Body: {"to": "<nodeID>"}. Gracefully hands off leadership to the named node.
func (s *Store) handleTransferLeadership(w http.ResponseWriter, r *http.Request) {
	var req transferLeadershipRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error(), s.logger())
		return
	}
	if req.To == "" {
		writeError(w, http.StatusBadRequest, "to is required", s.logger())
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
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error(), s.logger())
		return
	}
	if len(ops) == 0 {
		writeJSON(w, http.StatusOK, []json.RawMessage{}, s.logger())
		return
	}

	cmds := make([]command, len(ops))
	for i, op := range ops {
		if isReservedCollection(op.Collection) {
			writeError(w, http.StatusForbidden, ErrReservedCollection.Error(), s.logger())
			return
		}
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
	writeJSON(w, http.StatusOK, status, s.logger())
}

func (s *Store) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("OK"))
}

// normalizeHostPort parses addr as an optionally scheme-prefixed host:port and
// returns the canonical "host:port" form plus the scheme found, if any.
// It rejects anything that is not a bare address — no path, no userinfo, no
// stray whitespace — so an address learned from cluster state can never be
// turned into an arbitrary URL.
func normalizeHostPort(addr string) (hostPort string, ok bool) {
	hostPort, _, ok = normalizeHostPortScheme(addr)
	return hostPort, ok
}

func normalizeHostPortScheme(addr string) (hostPort, scheme string, ok bool) {
	addr = strings.TrimSpace(addr)
	switch {
	case strings.HasPrefix(addr, "http://"):
		scheme = "http"
		addr = strings.TrimPrefix(addr, "http://")
	case strings.HasPrefix(addr, "https://"):
		scheme = "https"
		addr = strings.TrimPrefix(addr, "https://")
	}
	if addr == "" || strings.ContainsAny(addr, "/\\@?#") {
		return "", "", false
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil || host == "" || port == "" {
		return "", "", false
	}
	// A redirect target must name a host; ":8001" is not routable by a client.
	portNum, err := strconv.Atoi(port)
	if err != nil || portNum < 1 || portNum > 65535 {
		return "", "", false
	}
	for _, r := range host {
		if r < 0x21 || r > 0x7e {
			return "", "", false // control characters, spaces, non-ASCII
		}
	}
	return net.JoinHostPort(host, port), scheme, true
}

// advertisableHost reports whether addr names a host a remote peer could dial.
//
// The port is not checked: a node may legitimately bind port 0 and learn its
// real port from the listener. The host is what cannot be fixed up later --
// an empty one or a wildcard says "every interface on whichever machine is
// asking", which is never an answer to "where do I find you".
func advertisableHost(addr string) bool {
	addr = strings.TrimSpace(addr)
	addr = strings.TrimPrefix(addr, "http://")
	addr = strings.TrimPrefix(addr, "https://")
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		return false
	}
	return true
}

// leaderURL builds an absolute URL for path on leaderID's advertised HTTP
// address, or "" when no usable address is known.
//
// The host comes only from the internal metadata collection, which HTTP
// clients cannot write, and is re-validated as a bare host:port before use, so
// no caller-supplied text ever reaches a Location header or an outbound
// request. Path and query are carried verbatim from the request being
// redirected; the host is never taken from it.
func (s *Store) leaderURL(leaderID raft.NodeID, path, rawQuery string) string {
	s.mu.RLock()
	var advertised string
	if coll := s.collections[metadataCollection]; coll != nil {
		if raw, ok := coll[string(leaderID)]; ok {
			// A decode failure leaves advertised empty, which is handled just
			// below as "the leader has no usable address" — the same outcome
			// as no entry at all.
			_ = json.Unmarshal(raw, &advertised)
		}
	}
	s.mu.RUnlock()

	if advertised == "" {
		return ""
	}
	hostPort, scheme, ok := normalizeHostPortScheme(advertised)
	if !ok {
		s.logger().Warn("easyraft: refusing to use an unusable leader address",
			"leader", leaderID, "advertised", advertised)
		return ""
	}
	if scheme == "" {
		scheme = "http"
		if s.cfg.HTTPTLS != nil {
			scheme = "https"
		}
	}
	u := url.URL{
		Scheme:   scheme,
		Host:     hostPort,
		Path:     path,
		RawQuery: rawQuery,
	}
	return u.String()
}

// leaderRedirectURL is leaderURL for the path and query of r.
func (s *Store) leaderRedirectURL(r *http.Request, leaderID raft.NodeID) string {
	return s.leaderURL(leaderID, r.URL.Path, r.URL.RawQuery)
}

func (s *Store) handleRPCError(w http.ResponseWriter, r *http.Request, err error) {
	logger := s.logger()

	if errors.Is(err, ErrNotLeader) {
		leaderID := s.node.Leader()
		if leaderID == "" {
			writeError(w, http.StatusServiceUnavailable, "no leader currently elected", logger)
			return
		}

		if target := s.leaderRedirectURL(r, leaderID); target != "" {
			w.Header().Set("Location", target)
			writeError(w, http.StatusTemporaryRedirect,
				fmt.Sprintf("not leader; redirecting to %s", leaderID), logger)
			return
		}

		// Leader is known but its HTTP address has not been advertised yet.
		writeError(w, http.StatusServiceUnavailable,
			fmt.Sprintf("not leader; leader is %s but its HTTP address is not yet known", leaderID), logger)
		return
	}

	writeError(w, statusForError(err), err.Error(), logger)
}

// statusForError maps an error returned by the Raft or easyraft layers to an
// HTTP status code. Matching is done with errors.Is against the sentinels the
// two packages export, so wrapped errors are classified correctly.
func statusForError(err error) int {
	switch {
	case errors.Is(err, ErrKeyNotFound), errors.Is(err, raft.ErrGroupNotFound),
		errors.Is(err, raft.ErrNotFound), errors.Is(err, raft.ErrNotMember):
		return http.StatusNotFound

	case errors.Is(err, ErrKeyExists), errors.Is(err, raft.ErrObsoleteSeqNum),
		errors.Is(err, raft.ErrConfigChangeInProgress),
		errors.Is(err, raft.ErrLeadershipTransferInProgress),
		errors.Is(err, raft.ErrMemberNotCaughtUp),
		errors.Is(err, raft.ErrGroupExists):
		return http.StatusConflict

	case errors.Is(err, ErrReservedCollection):
		return http.StatusForbidden

	case errors.Is(err, raft.ErrProposalTooLarge):
		return http.StatusRequestEntityTooLarge

	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusRequestTimeout

	// Everything the node will recover from on its own. 503 is the one status
	// a client may retry unchanged, and these are exactly the conditions where
	// retrying is the right thing: a backlog drains, a lease is renewed, a
	// stopped node is restarted.
	case errors.Is(err, raft.ErrLeaseExpired), errors.Is(err, raft.ErrStopped),
		errors.Is(err, raft.ErrWriteBacklogFull), errors.Is(err, raft.ErrNodeFailed),
		errors.Is(err, raft.ErrManagerStopping),
		errors.Is(err, context.Canceled):
		return http.StatusServiceUnavailable

	default:
		return http.StatusInternalServerError
	}
}

// ---- Manager HTTP server ---------------------------------------------------

func (m *Manager) serveHTTP() error {
	if m.cfg.HTTPAddr == "" {
		return nil
	}
	logger := m.logger()
	warnIfHTTPUnauthenticated(&m.cfg, logger)

	guard := func(h http.HandlerFunc) http.HandlerFunc {
		return authorized(m.cfg.HTTPAuth, logger, h)
	}

	mux := http.NewServeMux()

	// Cluster management per Raft group.
	mux.HandleFunc("POST /groups/{groupID}/join", guard(m.handleJoin))
	mux.HandleFunc("GET /groups/{groupID}/members", guard(m.handleMembers))
	mux.HandleFunc("DELETE /groups/{groupID}/members/{id}", guard(m.handleRemoveMember))
	mux.HandleFunc("POST /groups/{groupID}/transfer-leadership", guard(m.handleTransferLeadership))
	mux.HandleFunc("POST /groups/{groupID}/batch", guard(m.handleBatch))

	// Multi-Raft routing: /groups/{groupID}/{collection}/{key}
	mux.HandleFunc("POST /groups/{groupID}/{collection}/{key}", guard(m.handleCreate))
	mux.HandleFunc("GET /groups/{groupID}/{collection}/{key}", guard(m.handleRead))
	mux.HandleFunc("PUT /groups/{groupID}/{collection}/{key}", guard(m.handleUpdate))
	mux.HandleFunc("PATCH /groups/{groupID}/{collection}/{key}", guard(m.handleUpsert))
	mux.HandleFunc("DELETE /groups/{groupID}/{collection}/{key}", guard(m.handleDelete))
	mux.HandleFunc("GET /groups/{groupID}/{collection}", guard(m.handleList))
	mux.HandleFunc("POST /groups/{groupID}/{collection}/{key}/mutate", guard(m.handleMutate))

	mux.HandleFunc("GET /status", guard(m.handleStatus))
	mux.HandleFunc("GET /health", guard(m.handleHealth))
	mux.Handle("GET /metrics", guard(promhttp.Handler().ServeHTTP))

	ln, err := net.Listen("tcp", m.cfg.HTTPAddr)
	if err != nil {
		return fmt.Errorf("easyraft: listen http %s: %w", m.cfg.HTTPAddr, err)
	}

	server := &http.Server{
		Addr:         m.cfg.HTTPAddr,
		Handler:      mux,
		TLSConfig:    m.cfg.HTTPTLS,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  30 * time.Second,
	}
	// Stop reads httpServer under the lock, so publish it under the lock too.
	m.mu.Lock()
	m.httpServer = server
	m.mu.Unlock()

	tlsCfg := m.cfg.HTTPTLS
	go func() {
		if serveErr := serveOn(server, ln, tlsCfg); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			logger.Error("easyraft: Manager HTTP server stopped", "addr", server.Addr, "err", serveErr)
		}
	}()

	return nil
}

func (m *Manager) getStore(r *http.Request) (*Store, error) {
	gidStr := r.PathValue("groupID")
	gid, err := strconv.ParseUint(gidStr, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid groupID %q: %w", gidStr, err)
	}

	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.stores[gid]
	if !ok {
		return nil, raft.ErrGroupNotFound
	}
	return s, nil
}

// withStore resolves the group from the request path and hands off to the
// matching Store handler.
func (m *Manager) withStore(w http.ResponseWriter, r *http.Request, h func(*Store, http.ResponseWriter, *http.Request)) {
	s, err := m.getStore(r)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error(), m.logger())
		return
	}
	h(s, w, r)
}

func (m *Manager) handleCreate(w http.ResponseWriter, r *http.Request) {
	m.withStore(w, r, (*Store).handleCreate)
}

func (m *Manager) handleRead(w http.ResponseWriter, r *http.Request) {
	m.withStore(w, r, (*Store).handleRead)
}

func (m *Manager) handleUpdate(w http.ResponseWriter, r *http.Request) {
	m.withStore(w, r, (*Store).handleUpdate)
}

func (m *Manager) handleUpsert(w http.ResponseWriter, r *http.Request) {
	m.withStore(w, r, (*Store).handleUpsert)
}

func (m *Manager) handleDelete(w http.ResponseWriter, r *http.Request) {
	m.withStore(w, r, (*Store).handleDelete)
}

func (m *Manager) handleList(w http.ResponseWriter, r *http.Request) {
	m.withStore(w, r, (*Store).handleList)
}

func (m *Manager) handleMutate(w http.ResponseWriter, r *http.Request) {
	m.withStore(w, r, (*Store).handleMutate)
}

func (m *Manager) handleJoin(w http.ResponseWriter, r *http.Request) {
	m.withStore(w, r, (*Store).handleJoin)
}

func (m *Manager) handleMembers(w http.ResponseWriter, r *http.Request) {
	m.withStore(w, r, (*Store).handleMembers)
}

func (m *Manager) handleRemoveMember(w http.ResponseWriter, r *http.Request) {
	m.withStore(w, r, (*Store).handleRemoveMember)
}

func (m *Manager) handleTransferLeadership(w http.ResponseWriter, r *http.Request) {
	m.withStore(w, r, (*Store).handleTransferLeadership)
}

func (m *Manager) handleBatch(w http.ResponseWriter, r *http.Request) {
	m.withStore(w, r, (*Store).handleBatch)
}

func (m *Manager) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, m.StatusAll(r.Context()), m.logger())
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
		logger.Error("easyraft: HTTP encode failed", "err", err)
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
	_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
}
