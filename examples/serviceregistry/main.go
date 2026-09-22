// serviceregistry is a service registry built on EasyRaft.
//
// Instances register themselves under a lease and keep it alive while they
// run. When one stops -- cleanly, by crashing, or by being partitioned from
// the cluster -- it stops renewing, the lease expires, and its entry is
// deleted on every replica. Nothing else has to notice that it died, which is
// the whole reason a registry uses leases rather than a heartbeat table it
// has to sweep itself.
//
// This process is the registry. The thing that registers is
// [serviceregistry/agent], a separate program that talks to the cluster from
// outside it using the easyraft/client package: it grants its own lease,
// writes its own entry under it, and holds it with a keep-alive loop. There
// is no bespoke registration endpoint here, because none is needed.
//
// # What this example demonstrates
//
//   - [easyraft.Store.GrantLease] and [easyraft.Collection.UpsertWithLease]
//     (in the agent): an entry that outlives nothing.
//   - [easyraft.Store.KeepAliveLoop] (in the agent): one goroutine holding a
//     registration for as long as its context lives.
//   - [easyraft.Collection.ListPrefix]: every instance of one service, from a
//     collection keyed "<service>/<instance>".
//   - [easyraft.Collection.Scan]: the whole registry a page at a time, for
//     the case where listing everything at once is not wanted.
//   - [easyraft.Collection.OnChange]: registrations and expiries logged as
//     they are applied, on every replica. A lease expiring produces ordinary
//     delete events, so a watcher sees instances leave exactly as it sees
//     them arrive.
//
// # HTTP API
//
//	GET /services                      — every instance, sorted
//	GET /services?limit=N&after=KEY    — one page of them
//	GET /services/{name}               — every instance of one service
//	GET /services/{name}/{instance}    — one instance
//
// Registration is not here: an agent does it directly against the easyraft
// API this store already serves. See the README.
//
// # Usage
//
//	# A three-node registry
//	./serviceregistry --id n1 --raft-addr :7001 --http-addr :8001 --data-dir /tmp/reg/n1
//	./serviceregistry --id n2 --raft-addr :7002 --http-addr :8002 --data-dir /tmp/reg/n2 --join localhost:8001
//	./serviceregistry --id n3 --raft-addr :7003 --http-addr :8003 --data-dir /tmp/reg/n3 --join localhost:8001
//
//	# Two instances of a service, registering themselves
//	./agent --service api --id api-1 --addr 10.0.0.1:9000 --endpoints localhost:8001,localhost:8002,localhost:8003
//	./agent --service api --id api-2 --addr 10.0.0.2:9000 --endpoints localhost:8001,localhost:8002,localhost:8003
//
//	# Who is up? Ask any node.
//	curl -s localhost:8003/services/api
//
//	# Kill one agent and ask again a few seconds later: it is gone.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/brunoga/raft/v2"
	"github.com/brunoga/raft/v2/easyraft"
	registry "github.com/brunoga/raft/v2/examples/serviceregistry/registry"
	"github.com/prometheus/client_golang/prometheus"
)

// defaultPageLimit caps a page when a caller asks to paginate without saying
// how large a page should be.
const defaultPageLimit = 100

type server struct {
	store     *easyraft.Store
	instances *easyraft.Collection[registry.Instance]
	logger    *slog.Logger
}

func newServer(store *easyraft.Store, instances *easyraft.Collection[registry.Instance], logger *slog.Logger) *server {
	s := &server{store: store, instances: instances, logger: logger}

	// Fires on every replica after each committed write, outside the Raft
	// lock. A lease expiring deletes its keys through the same path an
	// explicit delete takes, so an instance leaving looks exactly like an
	// instance being removed -- which is what lets a watcher keep a local
	// view without knowing anything about leases.
	instances.OnChange(func(key string, value *registry.Instance, deleted bool) {
		if deleted {
			logger.Info("instance left", "key", key)
			return
		}
		logger.Info("instance registered", "key", key, "addr", value.Addr)
	})
	return s
}

// handleList serves GET /services, optionally a page at a time.
//
// Without a limit it answers the whole registry, which is what a small one
// wants. With one it pages: the cursor to carry into the next request comes
// back in the body, and it is empty exactly when the scan reached the end --
// a short page is not the end, because nothing stops a page being short.
func (s *server) handleList(w http.ResponseWriter, r *http.Request) {
	limit, err := intParam(r, "limit")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if limit == 0 && r.URL.Query().Has("after") {
		limit = defaultPageLimit
	}

	page, err := s.instances.Scan(r.Context(), easyraft.ScanOptions{
		After: r.URL.Query().Get("after"),
		Limit: limit,
	})
	if err != nil {
		s.store.WriteHTTPError(w, r, err)
		return
	}

	out := struct {
		Instances []registry.Instance `json:"instances"`
		Next      string              `json:"next,omitempty"`
	}{Instances: make([]registry.Instance, 0, len(page.Items)), Next: page.Next}
	for _, item := range page.Items {
		out.Instances = append(out.Instances, item.Value)
	}
	writeJSON(w, out)
}

// handleListService serves GET /services/{name}: every instance of one
// service, which is one prefix scan because of how the keys are shaped.
func (s *server) handleListService(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		http.Error(w, "missing service name", http.StatusBadRequest)
		return
	}

	found, err := s.instances.ListPrefix(r.Context(), registry.Prefix(name))
	if err != nil {
		s.store.WriteHTTPError(w, r, err)
		return
	}

	out := struct {
		Service   string              `json:"service"`
		Instances []registry.Instance `json:"instances"`
	}{Service: name, Instances: make([]registry.Instance, 0, len(found))}
	for _, instance := range found {
		out.Instances = append(out.Instances, instance)
	}
	writeJSON(w, out)
}

// handleGet serves GET /services/{name}/{instance}.
func (s *server) handleGet(w http.ResponseWriter, r *http.Request) {
	name, id := r.PathValue("name"), r.PathValue("instance")
	if name == "" || id == "" {
		http.Error(w, "missing service or instance", http.StatusBadRequest)
		return
	}

	instance, err := s.instances.Read(r.Context(), registry.Key(name, id))
	if err != nil {
		if errors.Is(err, easyraft.ErrKeyNotFound) {
			http.Error(w, "not registered", http.StatusNotFound)
			return
		}
		s.store.WriteHTTPError(w, r, err)
		return
	}
	writeJSON(w, instance)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// intParam reads a non-negative integer query parameter, or zero if absent.
func intParam(r *http.Request, name string) (int, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s must be a non-negative number", name)
	}
	return n, nil
}

// main keeps nothing but the exit code, so that the shutdown work in run is
// actually performed rather than skipped by os.Exit past a defer.
func main() {
	if err := run(); err != nil {
		slog.Error("serviceregistry: exiting", "err", err)
		os.Exit(1)
	}
}

func run() error {
	id := flag.String("id", "", "Raft node ID (required)")
	raftAddr := flag.String("raft-addr", ":7001", "Raft gRPC listen address")
	httpAddr := flag.String("http-addr", ":8001", "HTTP listen address")
	dataDir := flag.String("data-dir", "", "Persistent data directory (required)")
	join := flag.String("join", "", "Comma-separated HTTP addresses of seed nodes to join")
	sweep := flag.Duration("lease-sweep", 0, "How often the leader looks for expired leases (0 = default)")
	flag.Parse()

	if *id == "" || *dataDir == "" {
		fmt.Fprintln(os.Stderr, "usage: serviceregistry --id <id> --data-dir <dir> "+
			"[--raft-addr :7001] [--http-addr :8001] [--join host:port,...]")
		return errors.New("--id and --data-dir are required")
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	// One mux for both easyraft's routes and this service's, so there is one
	// port and no chance of the two disagreeing about which node is leader.
	mux := http.NewServeMux()

	opts := []easyraft.Option{
		easyraft.WithID(raft.NodeID(*id)),
		easyraft.WithRaftAddr(*raftAddr),
		// A local demo cluster: plaintext transport and open HTTP API, on purpose.
		easyraft.WithInsecureTransportAcknowledged(),
		easyraft.WithInsecureHTTPAcknowledged(),
		easyraft.WithHTTPAddr(*httpAddr),
		easyraft.WithHTTPMux(mux),
		easyraft.WithDataDir(*dataDir),
		easyraft.WithLogger(logger),
		easyraft.WithPrometheus(prometheus.DefaultRegisterer),
	}
	if *sweep > 0 {
		// How long a registration can outlive the agent holding it, on top of
		// the TTL itself. A registry usually wants this shorter than the
		// default second, since the point is noticing quickly.
		opts = append(opts, easyraft.WithKeyLeaseSweepInterval(*sweep))
	}
	if *join != "" {
		opts = append(opts, easyraft.WithJoinAddr(strings.Split(*join, ",")...))
	}

	store, err := easyraft.NewStore(opts...)
	if err != nil {
		return fmt.Errorf("cannot build store: %w", err)
	}

	instances := easyraft.AddCollection[registry.Instance](store, registry.CollectionName)
	srv := newServer(store, instances, logger)

	// Registered before Start, so everything is wired before the listener
	// accepts anything.
	mux.HandleFunc("GET /services", srv.handleList)
	mux.HandleFunc("GET /services/{name}", srv.handleListService)
	mux.HandleFunc("GET /services/{name}/{instance}", srv.handleGet)

	baseCtx, cancelBase := context.WithCancel(context.Background())
	defer cancelBase()

	httpSrv := &http.Server{
		Addr:        *httpAddr,
		Handler:     mux,
		BaseContext: func(net.Listener) context.Context { return baseCtx },
	}

	if err := store.Start(); err != nil {
		_ = store.Stop()
		return fmt.Errorf("cannot start: %w", err)
	}
	defer func() {
		if stopErr := store.Stop(); stopErr != nil {
			logger.Error("serviceregistry: unclean shutdown", "err", stopErr)
		}
	}()

	logger.Info("serviceregistry started", "id", *id, "raft", *raftAddr, "http", *httpAddr)

	// Without a handler, ^C terminates the process outright and every
	// deferred call above is skipped -- including the one that stops the Raft
	// node. An example is a thing people copy.
	sigCtx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	serveErr := make(chan error, 1)
	go func() {
		if serveError := httpSrv.ListenAndServe(); serveError != nil &&
			!errors.Is(serveError, http.ErrServerClosed) {
			serveErr <- serveError
		}
	}()

	select {
	case err := <-serveErr:
		return fmt.Errorf("http server: %w", err)
	case <-sigCtx.Done():
		logger.Info("serviceregistry: shutting down")
	}

	cancelBase()
	shutCtx, cancelShut := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelShut()
	if err := httpSrv.Shutdown(shutCtx); err != nil {
		return fmt.Errorf("http shutdown: %w", err)
	}
	return nil
}
