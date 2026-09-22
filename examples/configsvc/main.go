// configsvc is a distributed configuration service built on EasyRaft.
//
// It stores string key-value pairs in a strongly-consistent, replicated
// collection and lets clients watch individual keys (or all keys) for changes
// over Server-Sent Events (SSE). Every node in the cluster fires watch events
// independently — watchers do not need to be connected to the leader.
//
// # Key design points
//
// Config entries carry a Version field (Unix nanoseconds, supplied by the
// caller before proposing). This follows the same determinism pattern as the
// ratelimiter example: the timestamp is encoded in the request args so every
// replica applies the same value rather than reading the clock inside Apply.
//
// Writes are last-writer-wins by default, and compare-and-swap when the
// caller says so. A GET returns the key's revision as an ETag; handing it
// back as If-Match makes the next write apply only while the key is still at
// it, and 412 otherwise. That is the difference between setting a value
// somebody typed and setting one computed from the value already there --
// the second is a read and a write with a window between them, and a write
// that lands in the window is silently overwritten unless the second is
// conditional.
//
// Note which number the condition uses: the revision, not Version. A
// timestamp is the wrong thing to compare against, because two writers in the
// same nanosecond get the same one and a clock that steps back produces one
// that has already been used. The revision is the index of the entry that
// wrote the key: unique, agreed by the cluster, and never backwards.
//
// Change notifications use [easyraft.Collection.OnChange], which fires on
// every replica immediately after each committed write is applied to the local
// state machine — outside the Raft lock. Watchers never need to poll; they
// simply block on a channel that the OnChange handler populates.
//
// # HTTP API
//
//	PUT    /configs/{key}   — set a value (body: {"value":"..."})
//	                          If-Match: <etag>   apply only at that revision
//	                          If-None-Match: *   apply only if absent
//	GET    /configs/{key}   — linearizable read; revision in the ETag header
//	GET    /configs/{key}?consistency=stale  — local read
//	DELETE /configs/{key}   — delete, honouring If-Match
//	GET    /configs         — list all (linearizable)
//	GET    /watch/{key}     — SSE stream for a single key
//	GET    /watch           — SSE stream for all keys
//
// # Usage
//
//	# Node 1 — bootstrap
//	./configsvc --id n1 --raft-addr :7001 --http-addr :8001 --data-dir /tmp/cfg/n1
//
//	# Node 2 — joins node 1
//	./configsvc --id n2 --raft-addr :7002 --http-addr :8002 --data-dir /tmp/cfg/n2 \
//	            --join localhost:8001
//
//	# Watch all keys on node 2 (any node works)
//	curl -N http://localhost:8002/watch
//
//	# Set a value (must hit leader, or follow the 307 redirect)
//	curl -L -X PUT http://localhost:8001/configs/db.host \
//	     -H 'Content-Type: application/json' -d '{"value":"localhost"}'
//
//	# Change it only if nobody else did first
//	etag=$(curl -sS -D- -o/dev/null http://localhost:8002/configs/db.host \
//	       | awk '/[Ee][Tt]ag:/ {print $2}' | tr -d '\r')
//	curl -L -X PUT -H "If-Match: $etag" \
//	     -H 'Content-Type: application/json' -d '{"value":"db.internal"}' \
//	     http://localhost:8001/configs/db.host
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
	"github.com/prometheus/client_golang/prometheus"
)

// ConfigEntry is the value stored in the "configs" collection.
type ConfigEntry struct {
	Value   string `json:"value"`
	Version int64  `json:"version"` // Unix nanoseconds; set by caller, not inside Apply
}

// server wires together EasyRaft and the HTTP handlers.
type server struct {
	store   *easyraft.Store
	configs *easyraft.Collection[ConfigEntry]
	watcher *easyraft.Watcher[ConfigEntry]
}

func newServer(store *easyraft.Store, configs *easyraft.Collection[ConfigEntry]) *server {
	w := easyraft.NewWatcher[ConfigEntry]()
	// Wire up the OnChange hook. This fires on every replica after each
	// committed write — outside the Raft lock, in a dedicated dispatcher
	// goroutine. We just fan out to local SSE subscribers.
	configs.OnChange(w.Notify)
	return &server{store: store, configs: configs, watcher: w}
}

// setConfig writes a config entry with the current timestamp as version.
//
// rev makes the write conditional: it applies only while the key is still at
// that revision, and is refused with easyraft.ErrRevisionMismatch otherwise.
// A nil rev writes unconditionally, which is last-writer-wins.
func (s *server) setConfig(ctx context.Context, key, value string, rev *uint64) error {
	entry := ConfigEntry{
		Value:   value,
		Version: time.Now().UnixNano(),
	}
	if rev == nil {
		return s.configs.Upsert(ctx, key, entry)
	}
	return s.configs.UpsertIf(ctx, key, entry, *rev)
}

// handleSet serves PUT /configs/{key}.
//
// Without a condition it is last-writer-wins, which is what a configuration
// service usually wants for a value somebody typed. With If-Match it is a
// compare-and-swap against the revision a GET returned, which is what it
// wants for a value something computed from the one already there --
// incrementing a counter, adding an entry to a list, flipping a flag based on
// its current state. Those are two log entries with a window between them,
// and a write that lands in the window is silently overwritten unless the
// second one is conditional.
func (s *server) handleSet(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	if key == "" {
		http.Error(w, "missing key", http.StatusBadRequest)
		return
	}

	rev, ok := conditionFrom(w, r)
	if !ok {
		return
	}

	var body struct {
		Value string `json:"value"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	if err := s.setConfig(r.Context(), key, body.Value, rev); err != nil {
		if errors.Is(err, easyraft.ErrRevisionMismatch) {
			http.Error(w, "the key has changed since the revision given",
				http.StatusPreconditionFailed)
			return
		}
		// The store knows the leader's advertised address and answers with a
		// redirect, so a write that reached a follower still lands.
		s.store.WriteHTTPError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// conditionFrom reads the revision a conditional write must match, from the
// headers HTTP already has for the job.
//
//	If-Match: "7"       apply only if the key is still at revision 7
//	If-None-Match: *    apply only if the key does not exist
//
// Nothing else is accepted. A weak validator or a list of ETags would have to
// be answered with a guess about which one the caller meant, and a guess here
// is the lost update the condition was asked for to prevent.
//
// A nil revision with ok true means the caller asked for no condition.
func conditionFrom(w http.ResponseWriter, r *http.Request) (rev *uint64, ok bool) {
	ifMatch := strings.TrimSpace(r.Header.Get("If-Match"))
	ifNone := strings.TrimSpace(r.Header.Get("If-None-Match"))

	switch {
	case ifMatch == "" && ifNone == "":
		return nil, true

	case ifMatch != "" && ifNone != "":
		http.Error(w, "If-Match and If-None-Match cannot both be given", http.StatusBadRequest)
		return nil, false

	case ifNone != "":
		if ifNone != "*" {
			http.Error(w, `If-None-Match accepts only "*"`, http.StatusBadRequest)
			return nil, false
		}
		// Revision zero is the revision of a key that has never been written,
		// so this is create-if-absent.
		var zero uint64
		return &zero, true

	default:
		parsed, err := strconv.ParseUint(strings.Trim(ifMatch, `"`), 10, 64)
		if err != nil {
			http.Error(w, "If-Match must be the ETag of a read", http.StatusBadRequest)
			return nil, false
		}
		return &parsed, true
	}
}

func (s *server) handleGet(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	if key == "" {
		http.Error(w, "missing key", http.StatusBadRequest)
		return
	}

	var (
		entry ConfigEntry
		rev   uint64
		err   error
	)
	if r.URL.Query().Get("consistency") == "stale" {
		entry, rev, err = s.configs.ReadStaleRev(key)
	} else {
		entry, rev, err = s.configs.ReadRev(r.Context(), key)
	}

	if err != nil {
		if errors.Is(err, easyraft.ErrKeyNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// The revision, not the Version field below it. Version is this service's
	// own data -- a timestamp it chose to expose -- and a timestamp is the
	// wrong thing to compare against: two writers in the same nanosecond get
	// the same one, and a clock that steps back produces one that has already
	// been used. The revision is the index of the entry that wrote the key,
	// so it is unique, agreed by the cluster, and never goes backwards.
	w.Header().Set("ETag", strconv.FormatUint(rev, 10))
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(entry)
}

func (s *server) handleDelete(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	if key == "" {
		http.Error(w, "missing key", http.StatusBadRequest)
		return
	}

	rev, ok := conditionFrom(w, r)
	if !ok {
		return
	}

	var err error
	if rev == nil {
		err = s.configs.Delete(r.Context(), key)
	} else {
		err = s.configs.DeleteIf(r.Context(), key, *rev)
	}
	if err != nil {
		if errors.Is(err, easyraft.ErrKeyNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if errors.Is(err, easyraft.ErrRevisionMismatch) {
			http.Error(w, "the key has changed since the revision given",
				http.StatusPreconditionFailed)
			return
		}
		// The store knows the leader's advertised address and answers with a
		// redirect, so a write that reached a follower still lands.
		s.store.WriteHTTPError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) handleList(w http.ResponseWriter, r *http.Request) {
	var (
		all map[string]ConfigEntry
		err error
	)
	if r.URL.Query().Get("consistency") == "stale" {
		all, err = s.configs.ListStale()
	} else {
		all, err = s.configs.List(r.Context())
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(all)
}

// handleWatch streams SSE events to the client. Path /watch/{key} scopes to
// one key; path /watch (no key) streams all changes.
//
// handleWatch streams SSE events to the client. Path /watch/{key} scopes to
// one key; path /watch (no key) streams all changes.
//
// SSE format: see [easyraft.ChangeEvent].
func (s *server) handleWatch(w http.ResponseWriter, r *http.Request) {
	snap, _ := s.configs.ListStale()
	s.watcher.ServeSSE(w, r, r.PathValue("key"), snap)
}

// main keeps nothing but the exit code. Everything else is in run, because a
// process that calls os.Exit past a defer skips it -- which is the same fault
// this file was fixed for, one level up: shutdown work that is written down
// but never performed.
func main() {
	if err := run(); err != nil {
		slog.Error("configsvc: exiting", "err", err)
		os.Exit(1)
	}
}

func run() error {
	id := flag.String("id", "", "Raft node ID (required)")
	raftAddr := flag.String("raft-addr", ":7001", "Raft gRPC listen address")
	httpAddr := flag.String("http-addr", ":8001", "HTTP listen address")
	dataDir := flag.String("data-dir", "", "Persistent data directory (required)")
	join := flag.String("join", "", "Comma-separated HTTP addresses of seed nodes to join")
	flag.Parse()

	if *id == "" || *dataDir == "" {
		fmt.Fprintln(os.Stderr, "usage: configsvc --id <id> --data-dir <dir> [--raft-addr :7001] [--http-addr :8001] [--join host:port,...]")
		return errors.New("--id and --data-dir are required")
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	// Build the mux first so WithHTTPMux can hand it to the store.
	// easyraft registers its management routes (/join, /members, CRUD, etc.)
	// on this mux during Start; we add the app routes below. One HTTP server
	// serves everything — no port conflict.
	mux := http.NewServeMux()

	opts := []easyraft.Option{
		easyraft.WithID(raft.NodeID(*id)),
		easyraft.WithRaftAddr(*raftAddr),
		// A local demo cluster: plaintext transport and open HTTP API, on purpose.
		easyraft.WithInsecureTransportAcknowledged(),
		easyraft.WithInsecureHTTPAcknowledged(),
		easyraft.WithHTTPAddr(*httpAddr), // advertise URL for leader redirect
		easyraft.WithHTTPMux(mux),        // register easyraft routes on our mux
		easyraft.WithDataDir(*dataDir),
		easyraft.WithLogger(logger),
		easyraft.WithPrometheus(prometheus.DefaultRegisterer),
	}

	if *join != "" {
		addrs := strings.Split(*join, ",")
		opts = append(opts, easyraft.WithJoinAddr(addrs...))
	}

	store, err := easyraft.NewStore(opts...)
	if err != nil {
		logger.Error("failed to create store", "err", err)
		os.Exit(1)
	}

	configs := easyraft.AddCollection[ConfigEntry](store, "configs")
	srv := newServer(store, configs)

	// App-specific routes (registered before store.Start so everything is
	// wired before the server accepts connections).
	mux.HandleFunc("PUT /configs/{key}", srv.handleSet)
	mux.HandleFunc("GET /configs/{key}", srv.handleGet)
	mux.HandleFunc("DELETE /configs/{key}", srv.handleDelete)
	mux.HandleFunc("GET /configs", srv.handleList)
	mux.HandleFunc("GET /watch/{key}", srv.handleWatch)
	mux.HandleFunc("GET /watch", srv.handleWatch)

	// Every request context descends from this one, so cancelling it is what
	// ends the /watch streams at shutdown.
	baseCtx, cancelBase := context.WithCancel(context.Background())
	defer cancelBase()

	httpSrv := &http.Server{
		Addr:        *httpAddr,
		Handler:     mux,
		BaseContext: func(net.Listener) context.Context { return baseCtx },
	}

	// Start registers the easyraft routes on mux. It fails if this node was
	// told to join a cluster and could not: better to exit than to serve an
	// endpoint that is not part of any cluster.
	if err := store.Start(); err != nil {
		_ = store.Stop()
		return fmt.Errorf("cannot start: %w", err)
	}
	defer func() {
		if err := store.Stop(); err != nil {
			logger.Error("configsvc: unclean shutdown", "err", err)
		}
	}()

	logger.Info("configsvc started", "id", *id, "raft", *raftAddr, "http", *httpAddr)

	// ^C has to reach the deferred Stop above, and by default it does not: with
	// no handler installed the signal terminates the process outright and every
	// deferred call is skipped, so the Raft node is never stopped and the store
	// is never closed. Nothing about that is fatal -- the log is there precisely
	// so a node can come back from being killed -- but an example is a thing
	// people copy, and this one was modelling the wrong shutdown.
	sigCtx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	serveErr := make(chan error, 1)
	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
	}()

	select {
	case err := <-serveErr:
		return fmt.Errorf("http server: %w", err)
	case <-sigCtx.Done():
		logger.Info("configsvc: shutting down")
	}

	// Before Shutdown, not after. Shutdown waits for in-flight requests rather
	// than cancelling them, and /watch is an event stream with no natural end,
	// so one attached watcher would otherwise hold the process until the
	// timeout below expired.
	cancelBase()

	shutCtx, cancelShut := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelShut()
	if err := httpSrv.Shutdown(shutCtx); err != nil {
		return fmt.Errorf("http shutdown: %w", err)
	}
	return nil
}
