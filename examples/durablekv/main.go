// Command durablekv is a replicated key-value store whose state machine keeps
// its own state on disk.
//
// It exists to show the three optional interfaces a state machine implements
// when it is backed by storage rather than rebuilt in memory:
//
//	raft.DurableStateMachine  report what is already applied, so a restart
//	                          replays nothing it has already done
//	raft.BatchApplier         apply a run of entries in one write and one sync
//	raft.SnapshotCapturer     take a snapshot without pausing the apply loop
//
// None is required. A state machine that implements only raft.StateMachine
// works exactly as before, which is what every other example here does. The
// difference this one shows is what a restart costs and what a snapshot costs.
//
//	durablekv --id n1 --raft-addr 127.0.0.1:7001 --http-addr 127.0.0.1:8001 \
//	          --data-dir /tmp/n1 \
//	          --peer n2=127.0.0.1:7002,127.0.0.1:8002 \
//	          --peer n3=127.0.0.1:7003,127.0.0.1:8003
//
// See README.md.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/brunoga/raft"
	"github.com/brunoga/raft/storage/filestore"
	"github.com/brunoga/raft/transport/grpctransport"
)

type peerList []string

func (p *peerList) String() string     { return strings.Join(*p, ",") }
func (p *peerList) Set(v string) error { *p = append(*p, v); return nil }

func main() {
	if err := run(); err != nil {
		slog.Error("durablekv: exiting", "err", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		id       = flag.String("id", "", "this node's unique ID (required)")
		raftAddr = flag.String("raft-addr", "127.0.0.1:7001", "gRPC listen address for Raft RPCs")
		httpAddr = flag.String("http-addr", "127.0.0.1:8001", "HTTP listen address for the client API")
		dataDir  = flag.String("data-dir", "", "directory for the Raft log and the state machine (required)")
		peers    peerList
	)
	flag.Var(&peers, "peer", "peer as id=raft_addr[,http_addr] (repeatable)")
	flag.Parse()

	if *id == "" || *dataDir == "" {
		flag.Usage()
		return errors.New("--id and --data-dir are required")
	}

	peerConfigs, raftAddrs, httpAddrs, err := parsePeers(peers, raft.NodeID(*id))
	if err != nil {
		return err
	}

	// The Raft log and the state machine keep their own directories. They are
	// separate durable stores with separate lifetimes -- the log is compacted,
	// the state machine is rewritten -- and mixing them in one directory only
	// makes it harder to see which is which.
	logStore, err := filestore.Open(filepath.Join(*dataDir, "raft"))
	if err != nil {
		return fmt.Errorf("open raft log: %w", err)
	}
	defer func() { _ = logStore.Close() }()

	sm, err := openStore(filepath.Join(*dataDir, "state"))
	if err != nil {
		return err
	}
	defer func() { _ = sm.Close() }()

	applied, err := sm.AppliedIndex(context.Background())
	if err != nil {
		return err
	}
	slog.Info("durablekv: state machine opened", "applied_index", applied, "keys", len(sm.all()))

	tr, err := grpctransport.Listen(*raftAddr)
	if err != nil {
		return fmt.Errorf("listen raft: %w", err)
	}
	defer func() { _ = tr.Close() }()
	for pid, addr := range raftAddrs {
		tr.AddPeer(pid, addr)
	}

	cfg := raft.DefaultConfig()
	cfg.ID = raft.NodeID(*id)
	cfg.Peers = peerConfigs
	cfg.Storage = logStore
	cfg.StateMachine = sm
	cfg.Transport = tr
	// Snapshot often enough that the example actually takes one. TrailingLogs
	// has to come down with it: kept at the default it would be larger than
	// the threshold, compaction would reclaim nothing, and the engine would
	// say so on every start.
	cfg.SnapshotThreshold = 512
	cfg.TrailingLogs = 128

	node, err := raft.New(&cfg)
	if err != nil {
		return fmt.Errorf("new node: %w", err)
	}
	tr.Register(cfg.ID, node.Handler())
	node.Start()
	defer node.Stop()

	srv := &http.Server{
		Addr:              *httpAddr,
		Handler:           buildMux(node, sm, httpAddrs),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		slog.Info("durablekv: serving", "id", *id, "raft", *raftAddr, "http", *httpAddr)
		if serveErr := srv.ListenAndServe(); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			errCh <- serveErr
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	select {
	case err := <-errCh:
		return err
	case <-sig:
		slog.Info("durablekv: shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

func parsePeers(peers peerList, self raft.NodeID) (
	configs []raft.PeerConfig, raftAddrs, httpAddrs map[raft.NodeID]string, err error,
) {
	raftAddrs = make(map[raft.NodeID]string)
	httpAddrs = make(map[raft.NodeID]string)

	for _, p := range peers {
		name, addrs, ok := strings.Cut(p, "=")
		if !ok {
			return nil, nil, nil, fmt.Errorf("invalid --peer %q (want id=raft_addr[,http_addr])", p)
		}
		pid := raft.NodeID(name)
		if pid == self {
			continue
		}
		configs = append(configs, raft.PeerConfig{ID: pid, Voter: true})

		rAddr, hAddr, hasHTTP := strings.Cut(addrs, ",")
		raftAddrs[pid] = rAddr
		if hasHTTP {
			httpAddrs[pid] = hAddr
		}
	}
	return configs, raftAddrs, httpAddrs, nil
}

// buildMux wires the HTTP API.
func buildMux(node *raft.Node, sm *kvStore, httpAddrs map[raft.NodeID]string) http.Handler {
	mux := http.NewServeMux()

	// PUT /keys/{key} — replicate a write.
	mux.HandleFunc("PUT /keys/{key}", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Value string `json:"value"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return
		}
		cmd, err := json.Marshal(command{Op: "put", Key: r.PathValue("key"), Value: body.Value})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if _, err := node.Propose(r.Context(), cmd); err != nil {
			writeRaftError(w, r, err, httpAddrs)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	// DELETE /keys/{key}
	mux.HandleFunc("DELETE /keys/{key}", func(w http.ResponseWriter, r *http.Request) {
		cmd, err := json.Marshal(command{Op: "delete", Key: r.PathValue("key")})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if _, err := node.Propose(r.Context(), cmd); err != nil {
			writeRaftError(w, r, err, httpAddrs)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	// GET /keys/{key}[?consistency=stale] — linearizable unless asked otherwise.
	mux.HandleFunc("GET /keys/{key}", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("consistency") != "stale" {
			if err := awaitRead(r.Context(), node); err != nil {
				writeRaftError(w, r, err, httpAddrs)
				return
			}
		}
		value, ok := sm.get(r.PathValue("key"))
		if !ok {
			http.Error(w, "key not found", http.StatusNotFound)
			return
		}
		writeJSON(w, map[string]string{"value": value})
	})

	// GET /keys — the whole state.
	mux.HandleFunc("GET /keys", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("consistency") != "stale" {
			if err := awaitRead(r.Context(), node); err != nil {
				writeRaftError(w, r, err, httpAddrs)
				return
			}
		}
		writeJSON(w, sm.all())
	})

	// GET /status — Raft state plus what the state machine has durably applied.
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		applied, err := sm.AppliedIndex(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		status := node.Status()
		writeJSON(w, map[string]any{
			"node_id":       status.NodeID,
			"state":         status.State.String(),
			"term":          status.Term,
			"last_applied":  status.LastApplied,
			"sm_applied":    applied,
			"snapshot_at":   node.SnapshotIndex(),
			"keys":          len(sm.all()),
			"commit_index":  node.CommitIndex(),
			"leader":        node.Leader(),
			"is_leader":     status.State == raft.Leader,
			"durable_state": true,
		})
	})

	return mux
}

// awaitRead makes the next read linearizable.
//
// ReadIndex confirms leadership and then waits for this node's state machine to
// have applied everything committed as of that moment, so the read that follows
// sees a linearizable snapshot without the write having to go through the log.
func awaitRead(ctx context.Context, node *raft.Node) error {
	_, err := node.ReadIndex(ctx)
	return err
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Warn("durablekv: write response", "err", err)
	}
}

// writeRaftError answers a proposal failure, redirecting to the leader when its
// HTTP address is known.
func writeRaftError(w http.ResponseWriter, r *http.Request, err error, httpAddrs map[raft.NodeID]string) {
	var nle *raft.NotLeaderError
	if errors.As(err, &nle) {
		if addr, ok := httpAddrs[nle.Leader]; ok && addr != "" {
			target := *r.URL
			target.Scheme, target.Host = "http", addr
			http.Redirect(w, r, target.String(), http.StatusTemporaryRedirect)
			return
		}
		if nle.Leader != "" {
			w.Header().Set("X-Raft-Leader", string(nle.Leader))
		}
		http.Error(w, "not leader", http.StatusServiceUnavailable)
		return
	}

	switch {
	case errors.Is(err, raft.ErrProposalTooLarge):
		http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
	case errors.Is(err, raft.ErrWriteBacklogFull), errors.Is(err, raft.ErrStopped),
		errors.Is(err, raft.ErrNodeFailed), errors.Is(err, raft.ErrLeaseExpired):
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
	case errors.Is(err, context.DeadlineExceeded):
		http.Error(w, err.Error(), http.StatusGatewayTimeout)
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
