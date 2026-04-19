// shardkv is a horizontally-sharded key-value store that demonstrates the
// multi-Raft pattern: N independent Raft groups (shards) running on each
// physical node, all sharing one gRPC transport and driven by one central
// Manager ticker.  A BalanceController runs on every node and uses
// HTTPNodeProvider to query all peers' leader distributions, then transfers
// leaders to keep the count roughly equal across physical nodes.
//
// Each key is deterministically mapped to a shard by FNV-32a hash. Shards
// elect independent leaders and replicate concurrently; an outage in one shard
// does not affect the others. All nodes on the same physical host share a
// single gRPC port — the shared transport routes inbound RPCs to the correct
// shard node by the GroupID embedded in each RPC.
//
// # Setup
//
// Start every physical node with the same --shards value and one --peer entry
// per other node. Nodes may be started in any order.
//
// # Usage (3 nodes, 4 shards)
//
//	shardkv --id p1 --shards 4 --raft-addr :7001 --http-addr :8001 \
//	        --data-dir /tmp/sk-p1 \
//	        --peer p2=localhost:7002,localhost:8002 \
//	        --peer p3=localhost:7003,localhost:8003
//
// # HTTP API
//
//	PUT    /keys/{key}      — set key to request body (text/plain)
//	GET    /keys/{key}      — linearizable read (404 when absent)
//	DELETE /keys/{key}      — delete key (404 when absent)
//	GET    /shards          — live status of all shards on this node
//	GET    /status          — this node's physical configuration
//	GET    /raft/status     — leader-balance status (JSON array of GroupStatus)
//	POST   /raft/transfer   — trigger a leadership transfer ({"group_id":N,"to":"nodeID"})
//
// Writes that land on a follower for the key's shard are redirected (307) to
// the shard leader. Reads use ReadIndex and succeed on any replica.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"hash/fnv"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/brunoga/raft"
	"github.com/brunoga/raft/storage/filestore"
	"github.com/brunoga/raft/transport/grpctransport"
)

// peerFlag accumulates --peer physID=raftAddr[,httpAddr] flags.
type peerFlag []string

func (p *peerFlag) String() string { return strings.Join(*p, ",") }
func (p *peerFlag) Set(v string) error {
	*p = append(*p, v)
	return nil
}

// peerInfo holds the network addresses of a peer physical node.
type peerInfo struct {
	raftAddr string // gRPC address for Raft RPCs
	httpAddr string // HTTP address for client redirects (may be empty)
}

// shardNodeID returns the Raft NodeID for shard gid on physical node physID.
// Format: "g{gid}-{physID}", e.g. "g1-p1".
func shardNodeID(gid uint64, physID string) raft.NodeID {
	return raft.NodeID(fmt.Sprintf("g%d-%s", gid, physID))
}

// shardForKey maps a key to its 1-indexed shard group ID.
func shardForKey(key string, numShards uint64) uint64 {
	h := fnv.New32a()
	_, _ = io.WriteString(h, key)
	return uint64(h.Sum32())%numShards + 1
}

func main() {
	var (
		id              = flag.String("id", "", "this physical node's unique ID (required)")
		numShards       = flag.Uint64("shards", 4, "number of Raft shard groups (same on every node)")
		raftAddr        = flag.String("raft-addr", ":7001", "gRPC listen address for Raft RPCs (shared across all shards)")
		httpAddr        = flag.String("http-addr", ":8001", "HTTP listen address for client API")
		dataDir         = flag.String("data-dir", "", "root directory for persistent shard state (required)")
		balanceInterval = flag.Duration("balance-interval", 30*time.Second, "how often the leader-balance controller runs; 0 disables balancing")
		peers           peerFlag
	)
	flag.Var(&peers, "peer", "peer in the form physID=raftAddr[,httpAddr] (repeatable)")
	flag.Parse()

	if *id == "" || *dataDir == "" {
		fmt.Fprintln(os.Stderr, "shardkv: --id and --data-dir are required")
		flag.Usage()
		os.Exit(1)
	}
	if *numShards == 0 {
		fmt.Fprintln(os.Stderr, "shardkv: --shards must be > 0")
		os.Exit(1)
	}

	// Parse --peer flags into a physID → peerInfo map.
	peerMap := make(map[string]peerInfo)
	for _, p := range peers {
		parts := strings.SplitN(p, "=", 2)
		if len(parts) != 2 {
			fmt.Fprintf(os.Stderr, "shardkv: invalid --peer %q (want physID=raftAddr[,httpAddr])\n", p)
			os.Exit(1)
		}
		physID := parts[0]
		addrs := strings.SplitN(parts[1], ",", 2)
		pi := peerInfo{raftAddr: addrs[0]}
		if len(addrs) > 1 {
			pi.httpAddr = addrs[1]
		}
		peerMap[physID] = pi
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if err := os.MkdirAll(*dataDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "shardkv: mkdir %s: %v\n", *dataDir, err)
		os.Exit(1)
	}

	// One gRPC transport for all shards on this physical node.
	// SetGroupLookup:
	//   1. Routes inbound RPCs to the correct shard node by GroupID.
	//   2. Enables heartbeat batching: all G heartbeats per peer per tick are
	//      coalesced into one BatchHeartbeats RPC, reducing network cost from
	//      O(G×P) to O(P) per tick interval.
	tr, err := grpctransport.Listen(*raftAddr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "shardkv: grpctransport.Listen: %v\n", err)
		os.Exit(1)
	}
	defer tr.Close()

	mgr := raft.NewManager()
	defer mgr.Close()

	// nodeHTTPAddr maps each peer shard NodeID to that peer's HTTP address,
	// used to build 307 redirects when a write lands on a follower.
	nodeHTTPAddr := make(map[raft.NodeID]string)
	for peerPhysID, pi := range peerMap {
		for gid := uint64(1); gid <= *numShards; gid++ {
			tr.AddPeer(shardNodeID(gid, peerPhysID), pi.raftAddr)
		}
		if pi.httpAddr != "" {
			for gid := uint64(1); gid <= *numShards; gid++ {
				nodeHTTPAddr[shardNodeID(gid, peerPhysID)] = pi.httpAddr
			}
		}
	}

	// Install group-lookup BEFORE nodes start so the first inbound RPC is
	// already routed correctly.
	tr.SetGroupLookup(mgr.Lookup)

	// Create one Raft node per shard and register each with the Manager.
	// All nodes share the single transport tr; cfg.GroupID stamps every outbound
	// RPC so the remote transport can route the response back to the right node.
	sms := make(map[uint64]*KvSM, *numShards)
	for gid := uint64(1); gid <= *numShards; gid++ {
		shardDir := fmt.Sprintf("%s/shards/%d", *dataDir, gid)
		if err := os.MkdirAll(shardDir, 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "shardkv: mkdir %s: %v\n", shardDir, err)
			os.Exit(1)
		}
		store, err := filestore.Open(shardDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "shardkv: filestore.Open(%s): %v\n", shardDir, err)
			os.Exit(1)
		}

		peerIDs := make([]raft.PeerConfig, 0, len(peerMap))
		for peerPhysID := range peerMap {
			peerIDs = append(peerIDs, raft.PeerConfig{ID: shardNodeID(gid, peerPhysID), Voter: true})
		}

		sm := &KvSM{data: make(map[string]string)}
		sms[gid] = sm

		cfg := raft.DefaultConfig()
		cfg.GroupID = gid                       // stamped on every outbound RPC
		cfg.ID = shardNodeID(gid, *id)          // unique within the group
		cfg.Peers = peerIDs
		cfg.Storage = store
		cfg.StateMachine = sm
		cfg.Transport = tr                       // shared: one port for all shards
		cfg.TickInterval = 0                     // driven by mgr.RunTicker below
		cfg.Logger = logger

		node, err := raft.New(&cfg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "shardkv: raft.New(shard %d): %v\n", gid, err)
			os.Exit(1)
		}
		if err := mgr.AddAndStart(gid, node); err != nil {
			fmt.Fprintf(os.Stderr, "shardkv: mgr.AddAndStart(shard %d): %v\n", gid, err)
			os.Exit(1)
		}
	}

	// Single ticker drives all shard nodes. Setting TickInterval=0 on every
	// node ensures RunTicker is the sole clock source; the manager fans out
	// ticks in parallel using a worker pool sized to min(GOMAXPROCS, numShards).
	go mgr.RunTicker(ctx, 10*time.Millisecond)

	// BalanceController: keep shard leaders spread evenly across physical nodes.
	// The local node contributes its Manager directly; remote peers are reached
	// via HTTP using the /raft/status and /raft/transfer endpoints that
	// Manager.Handler() exposes (mounted at /raft/ in buildMux below).
	if *balanceInterval > 0 {
		providers := make(map[raft.NodeID]raft.NodeProvider, 1+len(peerMap))
		providers[raft.NodeID(*id)] = mgr // local: in-process, no HTTP hop
		for physID, pi := range peerMap {
			if pi.httpAddr != "" {
				base := pi.httpAddr
				if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
					base = "http://" + base
				}
				providers[raft.NodeID(physID)] = raft.NewHTTPNodeProvider(base+"/raft", nil)
			}
		}
		if len(providers) > 1 {
			ctrl := raft.NewBalanceController(providers, raft.LeastLeadersBalancer{}, *balanceInterval)
			go ctrl.Run(ctx)
			logger.Info("shardkv: leader balance controller started",
				"interval", *balanceInterval, "nodes", len(providers))
		}
	}

	srv := &http.Server{
		Addr:         *httpAddr,
		Handler:      buildMux(*id, *numShards, mgr, sms, nodeHTTPAddr),
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	go func() {
		slog.Info("shardkv: HTTP listening", "addr", *httpAddr, "shards", *numShards)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("shardkv: HTTP server error", "err", err)
		}
	}()

	<-ctx.Done()
	slog.Info("shardkv: shutting down")
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.Shutdown(shutCtx) //nolint:errcheck // best-effort shutdown
}

// buildMux constructs the HTTP handler for one physical shardkv node. It is
// extracted from main so that tests can wire nodes without binding real ports.
//
// nodeHTTPAddr maps each peer shard NodeID to the peer's HTTP host:port. It is
// consulted when a write returns NotLeaderError to build the redirect URL. The
// map may be populated after buildMux is called because handlers only read it
// at request time.
func buildMux(
	physID string,
	numShards uint64,
	mgr *raft.Manager,
	sms map[uint64]*KvSM,
	nodeHTTPAddr map[raft.NodeID]string,
) http.Handler {
	mux := http.NewServeMux()

	// /raft/ — leader-balance HTTP endpoints exposed by Manager.Handler().
	// GET  /raft/status   → Manager.StatusAll() as JSON (used by HTTPNodeProvider)
	// POST /raft/transfer → Manager.TransferGroupLeadership (used by BalanceController)
	mux.Handle("/raft/", http.StripPrefix("/raft", mgr.Handler()))

	// PUT /keys/{key}
	//
	// Sets the key to the request body (up to 1 MiB). Requests that land on a
	// follower for the key's shard are redirected (307) to the shard leader.
	mux.HandleFunc("PUT /keys/{key}", func(w http.ResponseWriter, r *http.Request) {
		key := r.PathValue("key")
		value, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
			return
		}
		cmd, _ := json.Marshal(kvCmd{Op: opSet, Key: key, Value: string(value)})
		node, ok := shardNode(mgr, numShards, key)
		if !ok {
			http.Error(w, "shard unavailable", http.StatusServiceUnavailable)
			return
		}
		if _, err := node.Propose(r.Context(), cmd); err != nil {
			writeProposalError(w, r, err, nodeHTTPAddr)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	// GET /keys/{key}
	//
	// Returns the current value for key as text/plain. Uses ReadIndex for
	// linearizability: reads reflect all writes that completed before this
	// request, regardless of which physical node serves it. Returns 404 when
	// the key is absent.
	mux.HandleFunc("GET /keys/{key}", func(w http.ResponseWriter, r *http.Request) {
		key := r.PathValue("key")
		node, ok := shardNode(mgr, numShards, key)
		if !ok {
			http.Error(w, "shard unavailable", http.StatusServiceUnavailable)
			return
		}
		if err := waitReadIndex(r.Context(), node); err != nil {
			writeProposalError(w, r, err, nodeHTTPAddr)
			return
		}
		sm := sms[shardForKey(key, numShards)]
		v, exists := sm.Get(key)
		if !exists {
			http.Error(w, "key not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprint(w, v)
	})

	// DELETE /keys/{key}
	//
	// Removes the key. Returns 404 when the key is absent. Redirects (307) to
	// the shard leader when this node is a follower for that shard.
	mux.HandleFunc("DELETE /keys/{key}", func(w http.ResponseWriter, r *http.Request) {
		key := r.PathValue("key")
		cmd, _ := json.Marshal(kvCmd{Op: opDel, Key: key})
		node, ok := shardNode(mgr, numShards, key)
		if !ok {
			http.Error(w, "shard unavailable", http.StatusServiceUnavailable)
			return
		}
		if _, err := node.Propose(r.Context(), cmd); err != nil {
			if errors.Is(err, ErrKeyNotFound) {
				http.Error(w, "key not found", http.StatusNotFound)
				return
			}
			writeProposalError(w, r, err, nodeHTTPAddr)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	// GET /shards
	//
	// Returns a JSON array with the live status of every shard on this physical
	// node: group ID, Raft state (Leader/Follower/Candidate), current term, and
	// last applied log index.
	mux.HandleFunc("GET /shards", func(w http.ResponseWriter, r *http.Request) {
		type shardStatus struct {
			GroupID     uint64 `json:"group_id"`
			State       string `json:"state"`
			Term        uint64 `json:"term"`
			LastApplied uint64 `json:"last_applied"`
		}
		statuses := mgr.StatusAll()
		out := make([]shardStatus, 0, len(statuses))
		for _, s := range statuses {
			out = append(out, shardStatus{
				GroupID:     s.GroupID,
				State:       s.State.String(),
				Term:        uint64(s.Term),
				LastApplied: uint64(s.LastApplied),
			})
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(out); err != nil {
			slog.Warn("shardkv: encode shards", "err", err)
		}
	})

	// GET /status
	//
	// Returns this physical node's static configuration.
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		shardIDs := make([]string, 0, numShards)
		for gid := uint64(1); gid <= numShards; gid++ {
			shardIDs = append(shardIDs, string(shardNodeID(gid, physID)))
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{
			"id":         physID,
			"num_shards": numShards,
			"shard_ids":  shardIDs,
		}); err != nil {
			slog.Warn("shardkv: encode status", "err", err)
		}
	})

	return mux
}

// shardNode returns the raft.Node for the shard that owns key on this physical
// node. The second return value is false only if the shard has been removed
// from the manager (not expected in normal operation).
func shardNode(mgr *raft.Manager, numShards uint64, key string) (*raft.Node, bool) {
	gid := shardForKey(key, numShards)
	node, err := mgr.Get(gid)
	return node, err == nil
}

// waitReadIndex performs a lease read (fast path) falling back to a full
// ReadIndex barrier. Because ReadIndex/ReadIndexLease already wait for the
// local state machine to catch up to the returned index, no separate
// WaitApplied call is needed.
func waitReadIndex(ctx context.Context, node *raft.Node) error {
	_, err := node.ReadIndexLease(ctx)
	if errors.Is(err, raft.ErrLeaseExpired) {
		_, err = node.ReadIndex(ctx)
	}
	return err
}

// writeProposalError maps errors from Propose / waitReadIndex to HTTP status
// codes:
//
//   - *raft.NotLeaderError → 307 Temporary Redirect (if leader HTTP address
//     is known) or 503 Service Unavailable
//   - anything else → 500 Internal Server Error
func writeProposalError(
	w http.ResponseWriter,
	r *http.Request,
	err error,
	nodeHTTPAddr map[raft.NodeID]string,
) {
	var nle *raft.NotLeaderError
	if errors.As(err, &nle) {
		if nle.Leader != "" {
			if addr, ok := nodeHTTPAddr[nle.Leader]; ok && addr != "" {
				u := *r.URL
				if strings.HasPrefix(addr, "http://") || strings.HasPrefix(addr, "https://") {
					parsed, _ := url.Parse(addr)
					u.Scheme = parsed.Scheme
					u.Host = parsed.Host
				} else {
					u.Scheme = "http"
					u.Host = addr
				}
				http.Redirect(w, r, u.String(), http.StatusTemporaryRedirect)
				return
			}
			w.Header().Set("X-Raft-Leader", string(nle.Leader))
		}
		http.Error(w, "not leader", http.StatusServiceUnavailable)
		return
	}
	http.Error(w, err.Error(), http.StatusInternalServerError)
}
