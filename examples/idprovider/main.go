// idprovider is a distributed monotonic ID allocation service built on the
// raft package. It supports multiple independent ID domains, each with its own
// counter. Allocations within a domain are unique, monotonically increasing,
// and never reused — even across leader failovers, network partitions, and
// process restarts.
//
// # Allocation model
//
// Each domain holds a single uint64 counter. A committed allocation reserves
// [start, start+count) and advances the counter atomically. Ranges are
// disjoint and ordered: if allocation A commits before allocation B in the same
// domain, A.start+A.count <= B.start.
//
// # Usage (three-node example)
//
//	idprovider --id n1 --raft-addr :7001 --http-addr :8001 --data-dir /tmp/n1 \
//	           --peer n2=localhost:7002 --peer n3=localhost:7003
//
//	# Create a domain:
//	curl -s -X POST http://localhost:8001/domains/orders
//
//	# Allocate a single ID (idempotent with client headers):
//	curl -s -X POST http://localhost:8001/domains/orders/next \
//	     -H 'X-Client-ID: myservice' -H 'X-Seq-Num: 1'
//	# → {"start":1,"count":1}
//
//	# Allocate 100 IDs at once:
//	curl -s -X POST 'http://localhost:8001/domains/orders/next?count=100' \
//	     -H 'X-Client-ID: myservice' -H 'X-Seq-Num: 2'
//	# → {"start":2,"count":100}
//
//	# Linearizable read of the current high-water mark:
//	curl -s http://localhost:8001/domains/orders/current
//	# → {"domain":"orders","current":101}
//
//	# List all domains:
//	curl -s http://localhost:8001/domains
//	# → {"orders":101}
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/brunoga/raft"
	"github.com/brunoga/raft/storage/filestore"
	"github.com/brunoga/raft/transport/grpctransport"
)

// peerFlag accumulates --peer id=raft_addr[,http_addr] flags.
type peerFlag []string

func (p *peerFlag) String() string { return strings.Join(*p, ",") }
func (p *peerFlag) Set(v string) error {
	*p = append(*p, v)
	return nil
}

// badRequestError signals that the HTTP request carries invalid parameters.
// Handlers convert this to 400 Bad Request.
type badRequestError struct{ msg string }

func (e *badRequestError) Error() string { return e.msg }

func main() {
	var (
		id       = flag.String("id", "", "this node's unique ID (required)")
		raftAddr = flag.String("raft-addr", ":7001", "gRPC listen address for Raft RPCs")
		httpAddr = flag.String("http-addr", ":8001", "HTTP listen address for client API")
		dataDir  = flag.String("data-dir", "", "directory for persistent Raft state (required)")
		peers    peerFlag
	)
	flag.Var(&peers, "peer", "peer in the form id=raft_addr[,http_addr] (repeatable)")
	flag.Parse()

	if *id == "" || *dataDir == "" {
		fmt.Fprintln(os.Stderr, "idprovider: --id and --data-dir are required")
		flag.Usage()
		os.Exit(1)
	}

	// Parse peers.
	peerIDs := make([]raft.NodeID, 0, len(peers))
	peerRaftAddrs := make(map[raft.NodeID]string, len(peers))
	peerHTTPAddrs := make(map[raft.NodeID]string, len(peers))
	for _, p := range peers {
		parts := strings.SplitN(p, "=", 2)
		if len(parts) != 2 {
			fmt.Fprintf(os.Stderr, "idprovider: invalid --peer %q (want id=raft_addr[,http_addr])\n", p)
			os.Exit(1)
		}
		pid := raft.NodeID(parts[0])
		if pid == raft.NodeID(*id) {
			continue
		}
		peerIDs = append(peerIDs, pid)

		addrs := strings.SplitN(parts[1], ",", 2)
		peerRaftAddrs[pid] = addrs[0]
		if len(addrs) > 1 {
			peerHTTPAddrs[pid] = addrs[1]
		}
	}

	// Open persistent storage.
	if err := os.MkdirAll(*dataDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "idprovider: mkdir %s: %v\n", *dataDir, err)
		os.Exit(1)
	}
	store, err := filestore.Open(*dataDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "idprovider: filestore.Open: %v\n", err)
		os.Exit(1)
	}
	defer store.Close()

	// Set up gRPC transport.
	tr, err := grpctransport.Listen(*raftAddr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "idprovider: grpctransport.Listen: %v\n", err)
		os.Exit(1)
	}
	defer tr.Close()
	for pid, addr := range peerRaftAddrs {
		tr.AddPeer(pid, addr)
	}

	// Build and start the Raft node.
	sm := &IDSM{domains: make(map[string]uint64)}
	cfg := raft.DefaultConfig()
	cfg.ID = raft.NodeID(*id)
	cfg.Peers = peerIDs
	cfg.Storage = store
	cfg.StateMachine = sm
	cfg.Transport = tr
	cfg.Logger = slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	node, err := raft.New(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "idprovider: raft.New: %v\n", err)
		os.Exit(1)
	}
	node.Start()
	defer node.Stop()

	srv := &http.Server{
		Addr:         *httpAddr,
		Handler:      buildMux(*id, node, sm, peerHTTPAddrs),
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		slog.Info("idprovider: HTTP listening", "addr", *httpAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("idprovider: HTTP server error", "err", err)
		}
	}()

	<-ctx.Done()
	slog.Info("idprovider: shutting down")
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.Shutdown(shutCtx)
}

// buildMux constructs the HTTP handler for a single Raft node.
// It is extracted from main so that tests can wire up nodes without binding
// real TCP ports.
func buildMux(nodeID string, node *raft.Node, sm *IDSM, httpAddrMap map[raft.NodeID]string) http.Handler {
	mux := http.NewServeMux()

	// POST /domains/{name}
	//
	// Creates a domain. Idempotent: succeeds whether or not the domain already
	// exists. Clients that supply X-Client-ID + X-Seq-Num get exactly-once
	// semantics (safe to retry on lost responses).
	mux.HandleFunc("POST /domains/{name}", func(w http.ResponseWriter, r *http.Request) {
		domain := r.PathValue("name")
		cmd, _ := json.Marshal(domainCmd{Op: opCreate, Domain: domain})
		if _, err := doPropose(r.Context(), node, r, cmd); err != nil {
			writeProposalError(w, r, err, httpAddrMap)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	// DELETE /domains/{name}
	//
	// Deletes a domain and its counter. Returns 404 if the domain does not exist.
	// Outstanding IDs already allocated from the domain remain valid.
	mux.HandleFunc("DELETE /domains/{name}", func(w http.ResponseWriter, r *http.Request) {
		domain := r.PathValue("name")
		cmd, _ := json.Marshal(domainCmd{Op: opDelete, Domain: domain})
		if _, err := doPropose(r.Context(), node, r, cmd); err != nil {
			if errors.Is(err, ErrDomainNotFound) {
				http.Error(w, "domain not found", http.StatusNotFound)
				return
			}
			writeProposalError(w, r, err, httpAddrMap)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	// GET /domains[?consistency=stale]
	//
	// Returns all domains and their current counters.
	// Default (no query param): linearizable — waits for ReadIndex confirmation.
	// ?consistency=stale: serves directly from this node's local state machine
	//   with no leader round-trip; may lag by up to one replication interval.
	mux.HandleFunc("GET /domains", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("consistency") != "stale" {
			if err := waitReadIndex(r.Context(), node); err != nil {
				writeProposalError(w, r, err, httpAddrMap)
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Raft-Applied-Index", fmt.Sprintf("%d", node.ReadStale()))
		if err := json.NewEncoder(w).Encode(sm.Domains()); err != nil {
			slog.Warn("idprovider: encode domains", "err", err)
		}
	})

	// POST /domains/{name}/next[?count=N]
	//
	// Allocates count (default 1) IDs from the named domain.
	// Returns {"start": X, "count": N} meaning [X, X+N) is yours.
	// Returns 404 if the domain does not exist.
	//
	// Idempotency: supply X-Client-ID and X-Seq-Num headers.
	mux.HandleFunc("POST /domains/{name}/next", func(w http.ResponseWriter, r *http.Request) {
		domain := r.PathValue("name")

		count := uint64(1)
		if s := r.URL.Query().Get("count"); s != "" {
			n, err := strconv.ParseUint(s, 10, 64)
			if err != nil || n == 0 {
				http.Error(w, "count must be a positive integer", http.StatusBadRequest)
				return
			}
			count = n
		}

		cmd, _ := json.Marshal(domainCmd{Op: opAlloc, Domain: domain, Count: count})
		result, err := doPropose(r.Context(), node, r, cmd)
		if err != nil {
			if errors.Is(err, ErrDomainNotFound) {
				http.Error(w, "domain not found", http.StatusNotFound)
				return
			}
			writeProposalError(w, r, err, httpAddrMap)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write(result); err != nil {
			slog.Warn("idprovider: write response", "err", err)
		}
	})

	// GET /domains/{name}/current[?consistency=stale]
	//
	// Returns the current high-water mark for the named domain.
	// All IDs ≤ current have been allocated to some client.
	// Returns 404 if the domain does not exist.
	// Default (no query param): linearizable — waits for ReadIndex confirmation.
	// ?consistency=stale: serves directly from this node's local state machine
	//   with no leader round-trip; may lag by up to one replication interval.
	mux.HandleFunc("GET /domains/{name}/current", func(w http.ResponseWriter, r *http.Request) {
		domain := r.PathValue("name")
		if r.URL.Query().Get("consistency") != "stale" {
			if err := waitReadIndex(r.Context(), node); err != nil {
				writeProposalError(w, r, err, httpAddrMap)
				return
			}
		}
		current, ok := sm.Current(domain)
		if !ok {
			http.Error(w, "domain not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Raft-Applied-Index", fmt.Sprintf("%d", node.ReadStale()))
		if err := json.NewEncoder(w).Encode(map[string]any{
			"domain":  domain,
			"current": current,
		}); err != nil {
			slog.Warn("idprovider: encode current", "err", err)
		}
	})

	// GET /status
	//
	// Returns cluster status: node state, current leader, and last applied index.
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{
			"id":           nodeID,
			"state":        node.StateSnapshot().String(),
			"leader":       string(node.Leader()),
			"last_applied": node.LastApplied(),
		}); err != nil {
			slog.Warn("idprovider: encode status", "err", err)
		}
	})

	return mux
}

// waitReadIndex performs a lease read (fast path) falling back to a full
// barrier ReadIndex, then waits for the local SM to catch up to that index.
func waitReadIndex(ctx context.Context, node *raft.Node) error {
	idx, err := node.ReadIndexLease(ctx)
	if errors.Is(err, raft.ErrLeaseExpired) {
		idx, err = node.ReadIndex(ctx)
	}
	if err != nil {
		return err
	}
	_, err = node.WaitApplied(ctx, idx)
	return err
}

// doPropose submits cmd via ProposeOnce when X-Client-ID and X-Seq-Num headers
// are present, or via plain Propose otherwise. Returns *badRequestError if the
// X-Seq-Num header is present but cannot be parsed.
func doPropose(ctx context.Context, node *raft.Node, r *http.Request, cmd []byte) ([]byte, error) {
	clientID := r.Header.Get("X-Client-ID")
	seqStr := r.Header.Get("X-Seq-Num")
	if clientID != "" && seqStr != "" {
		seq, err := strconv.ParseUint(seqStr, 10, 64)
		if err != nil {
			return nil, &badRequestError{"X-Seq-Num must be a non-negative integer"}
		}
		return node.ProposeOnce(ctx, raft.NodeID(clientID), seq, cmd)
	}
	return node.Propose(ctx, cmd)
}

// writeProposalError maps errors from doPropose / waitReadIndex to HTTP status codes:
//   - *badRequestError → 400 Bad Request
//   - *raft.NotLeaderError → 307 Temporary Redirect (if leader address known) or 503
//   - anything else → 500 Internal Server Error
func writeProposalError(w http.ResponseWriter, r *http.Request, err error, httpAddrMap map[raft.NodeID]string) {
	var bre *badRequestError
	if errors.As(err, &bre) {
		http.Error(w, bre.Error(), http.StatusBadRequest)
		return
	}
	var nle *raft.NotLeaderError
	if errors.As(err, &nle) {
		if nle.Leader != "" {
			if addr, ok := httpAddrMap[nle.Leader]; ok && addr != "" {
				// Redirect to the leader. Use 307 to preserve method and body.
				u := *r.URL
				u.Host = addr
				if !strings.HasPrefix(addr, "http://") && !strings.HasPrefix(addr, "https://") {
					u.Scheme = "http"
				} else {
					// If addr already has a scheme, parse it properly.
					parsed, _ := url.Parse(addr)
					u.Scheme = parsed.Scheme
					u.Host = parsed.Host
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

