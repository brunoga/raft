// Command watchtower is a replicated key-value service that reports everything
// the library can tell you about itself.
//
// It exists for the two observability seams nothing else here uses:
//
//	raft.Node.Events   what happened, to whom, and when -- the facts a metric
//	                   cannot carry, delivered on a bounded, lossy channel
//	raft.ApplyMetrics  how much of its time the apply loop spends applying
//	                   rather than waiting, which is the number that says
//	                   whether a slow write is consensus or your state machine
//
//	watchtower --id n1 --raft-addr 127.0.0.1:7001 --http-addr 127.0.0.1:8001 \
//	           --data-dir /tmp/n1 \
//	           --peer n2=127.0.0.1:7002,127.0.0.1:8002 \
//	           --peer n3=127.0.0.1:7003,127.0.0.1:8003
//
// --apply-delay makes the state machine slow on purpose, so the saturation
// metric has something to show. See README.md.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/brunoga/raft"
	"github.com/brunoga/raft/metrics/prommetrics"
	"github.com/brunoga/raft/storage/filestore"
	"github.com/brunoga/raft/transport/grpctransport"
)

type peerList []string

func (p *peerList) String() string     { return strings.Join(*p, ",") }
func (p *peerList) Set(v string) error { *p = append(*p, v); return nil }

func main() {
	if err := run(); err != nil {
		slog.Error("watchtower: exiting", "err", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		id         = flag.String("id", "", "this node's unique ID (required)")
		raftAddr   = flag.String("raft-addr", "127.0.0.1:7001", "gRPC listen address for Raft RPCs")
		httpAddr   = flag.String("http-addr", "127.0.0.1:8001", "HTTP listen address")
		dataDir    = flag.String("data-dir", "", "directory for the Raft log (required)")
		applyDelay = flag.Duration("apply-delay", 0,
			"artificial delay per applied entry, to make apply saturation visible")
		peers peerList
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

	store, err := filestore.Open(*dataDir)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = store.Close() }()

	tr, err := grpctransport.Listen(*raftAddr)
	if err != nil {
		return fmt.Errorf("listen raft: %w", err)
	}
	defer func() { _ = tr.Close() }()
	for pid, addr := range raftAddrs {
		tr.AddPeer(pid, addr)
	}

	reg := prometheus.NewRegistry()
	// prommetrics implements every optional metrics interface the engine looks
	// for, including ApplyMetrics. Each is found by a type assertion, so a
	// implementation that satisfies fewer of them simply reports less, with no
	// error to notice.
	promMetrics := prommetrics.New(reg)

	sm := &kvSM{applyDelay: *applyDelay}

	// Reporting to Prometheus and keeping the latest value where /health can
	// read it are not alternatives. Embedding the Prometheus implementation
	// and overriding one method composes the two, and is the general shape of
	// adding your own handling to an optional interface.
	metrics := metricsSink{Metrics: promMetrics, sm: sm}

	cfg := raft.DefaultConfig()
	cfg.ID = raft.NodeID(*id)
	cfg.Peers = peerConfigs
	cfg.Storage = store
	cfg.StateMachine = sm
	cfg.Transport = tr
	cfg.Metrics = metrics
	cfg.SnapshotThreshold = 512
	cfg.TrailingLogs = 128

	node, err := raft.New(&cfg)
	if err != nil {
		return fmt.Errorf("new node: %w", err)
	}
	tr.Register(cfg.ID, node.Handler())
	promMetrics.Track(node)
	node.Start()
	defer node.Stop()

	// Subscribe before anything interesting can happen. The channel is bounded
	// and the node never blocks on it, so the cost of subscribing is bounded
	// too -- and the cost of consuming slowly is lost events, not a stalled
	// cluster.
	events, stopEvents := node.Events()
	defer stopEvents()

	obs := newObserver(slog.Default().With("node", *id))
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		obs.run(events)
	}()
	defer wg.Wait()

	srv := &http.Server{
		Addr:              *httpAddr,
		Handler:           buildMux(node, sm, obs, reg, httpAddrs),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       10 * time.Second,
		// No write timeout: /events is a stream that stays open.
		IdleTimeout: 120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		slog.Info("watchtower: serving", "id", *id, "raft", *raftAddr, "http", *httpAddr,
			"apply_delay", *applyDelay)
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
		slog.Info("watchtower: shutting down")
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

func buildMux(node *raft.Node, sm *kvSM, obs *observer, reg *prometheus.Registry,
	httpAddrs map[raft.NodeID]string,
) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("PUT /keys/{key}", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Value string `json:"value"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return
		}
		cmd, err := json.Marshal(kvCommand{Key: r.PathValue("key"), Value: body.Value})
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

	mux.HandleFunc("GET /keys/{key}", func(w http.ResponseWriter, r *http.Request) {
		value, ok := sm.get(r.PathValue("key"))
		if !ok {
			http.Error(w, "key not found", http.StatusNotFound)
			return
		}
		writeJSON(w, map[string]string{"value": value})
	})

	// GET /observed — everything the event stream has told this node.
	mux.HandleFunc("GET /observed", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, obs.snapshotState())
	})

	// GET /health — the shape a load balancer or an alert wants: one status
	// code, and the reason next to it.
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		unreachable := obs.unreachablePeers()
		status := node.Status()
		body := map[string]any{
			"node":                 status.NodeID,
			"state":                status.State.String(),
			"term":                 status.Term,
			"unreachable_peers":    unreachable,
			"apply_saturation":     sm.saturation(),
			"last_applied":         status.LastApplied,
			"events_dropped":       obs.snapshotState()["events_dropped"],
			"degraded_but_serving": len(unreachable) > 0,
		}
		// Unreachable peers are not an outage while a quorum remains, so this
		// stays 200 and says so. Returning 503 here would take a healthy node
		// out of rotation for a problem on a different machine.
		writeJSON(w, body)
	})

	// GET /events — the event stream as server-sent events.
	mux.HandleFunc("GET /events", func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		ch, unsubscribe := obs.subscribe()
		defer unsubscribe()
		flusher.Flush()

		for {
			select {
			case <-r.Context().Done():
				return
			case ev := <-ch:
				payload, err := json.Marshal(ev)
				if err != nil {
					continue
				}
				if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
					return
				}
				flusher.Flush()
			}
		}
	})

	mux.Handle("GET /metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	return mux
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Warn("watchtower: write response", "err", err)
	}
}

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

// metricsSink reports to Prometheus and to the state machine, so the number is
// available to a scrape and to a human reading /health.
type metricsSink struct {
	*prommetrics.Metrics
	sm *kvSM
}

// ApplySaturation implements raft.ApplyMetrics.
func (m metricsSink) ApplySaturation(id raft.NodeID, saturation float64) {
	m.Metrics.ApplySaturation(id, saturation)
	m.sm.setSaturation(saturation)
}

// ---- A state machine that can be made slow on purpose -----------------------

type kvCommand struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// kvSM is an ordinary in-memory state machine with a knob. The delay is what
// makes apply saturation observable: without something slow to apply, the loop
// spends all its time waiting and the number is near zero however hard the
// cluster is pushed.
type kvSM struct {
	applyDelay time.Duration

	mu      sync.RWMutex
	data    map[string]string
	applied int
	lastSat float64
	haveSat bool
}

func (s *kvSM) Apply(_ context.Context, entry raft.LogEntry) ([]byte, error) {
	if len(entry.Command) == 0 {
		return nil, nil // the entry a new leader appends
	}
	if s.applyDelay > 0 {
		time.Sleep(s.applyDelay)
	}
	var cmd kvCommand
	if err := json.Unmarshal(entry.Command, &cmd); err != nil {
		return nil, fmt.Errorf("watchtower: decode command: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data == nil {
		s.data = make(map[string]string)
	}
	s.data[cmd.Key] = cmd.Value
	s.applied++
	return nil, nil
}

func (s *kvSM) Snapshot(_ context.Context, w io.Writer) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return json.NewEncoder(w).Encode(s.data)
}

func (s *kvSM) Restore(_ context.Context, _ raft.SnapshotMeta, r io.Reader) error {
	var data map[string]string
	if err := json.NewDecoder(r).Decode(&data); err != nil {
		return fmt.Errorf("watchtower: decode snapshot: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data = data
	return nil
}

func (s *kvSM) get(key string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.data[key]
	return v, ok
}

// saturation is the most recent value reported, or -1 before the first window
// has closed. Reporting -1 rather than 0 matters: an apply loop that has not
// been measured yet and one that has been idle are not the same thing.
func (s *kvSM) saturation() float64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.haveSat {
		return -1
	}
	return s.lastSat
}

func (s *kvSM) setSaturation(v float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastSat, s.haveSat = v, true
}
