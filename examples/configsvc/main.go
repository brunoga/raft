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
// Change notifications use [easyraft.Collection.OnChange], which fires on
// every replica immediately after each committed write is applied to the local
// state machine — outside the Raft lock. Watchers never need to poll; they
// simply block on a channel that the OnChange handler populates.
//
// # HTTP API
//
//	PUT    /configs/{key}   — set a value (body: {"value":"..."})
//	GET    /configs/{key}   — linearizable read
//	GET    /configs/{key}?consistency=stale  — local read
//	DELETE /configs/{key}   — delete
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
	"strings"
	"sync"
	"time"

	"github.com/brunoga/raft"
	"github.com/brunoga/raft/easyraft"
)

// ConfigEntry is the value stored in the "configs" collection.
type ConfigEntry struct {
	Value   string `json:"value"`
	Version int64  `json:"version"` // Unix nanoseconds; set by caller, not inside Apply
}

// WatchEvent is what SSE clients receive.
type WatchEvent struct {
	Key     string `json:"key"`
	Value   string `json:"value,omitempty"`
	Version int64  `json:"version,omitempty"`
	Deleted bool   `json:"deleted,omitempty"`
}

// WatcherRegistry manages SSE subscriptions. It is entirely local — subscriptions
// are not replicated. OnChange fires on every node, so every node independently
// notifies its own subscribers.
type WatcherRegistry struct {
	mu       sync.Mutex
	all      []chan WatchEvent            // subscriptions to all keys
	byKey    map[string][]chan WatchEvent // subscriptions to a specific key
}

func newWatcherRegistry() *WatcherRegistry {
	return &WatcherRegistry{byKey: make(map[string][]chan WatchEvent)}
}

// subscribe returns a buffered channel that will receive events for key.
// Pass key="" to receive events for all keys.
func (r *WatcherRegistry) subscribe(key string) chan WatchEvent {
	ch := make(chan WatchEvent, 64)
	r.mu.Lock()
	if key == "" {
		r.all = append(r.all, ch)
	} else {
		r.byKey[key] = append(r.byKey[key], ch)
	}
	r.mu.Unlock()
	return ch
}

// unsubscribe removes ch from the registry.
func (r *WatcherRegistry) unsubscribe(key string, ch chan WatchEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if key == "" {
		r.all = removeCh(r.all, ch)
	} else {
		r.byKey[key] = removeCh(r.byKey[key], ch)
	}
}

func removeCh(s []chan WatchEvent, ch chan WatchEvent) []chan WatchEvent {
	for i, c := range s {
		if c == ch {
			return append(s[:i], s[i+1:]...)
		}
	}
	return s
}

// notify dispatches ev to all matching subscribers. Called from the OnChange
// handler — must not block. Full subscriber channels drop the event.
func (r *WatcherRegistry) notify(ev WatchEvent) {
	r.mu.Lock()
	targets := append(append([]chan WatchEvent(nil), r.all...), r.byKey[ev.Key]...)
	r.mu.Unlock()
	for _, ch := range targets {
		select {
		case ch <- ev:
		default:
		}
	}
}

// server wires together EasyRaft and the HTTP handlers.
type server struct {
	configs *easyraft.Collection[ConfigEntry]
	reg     *WatcherRegistry
}

func newServer(configs *easyraft.Collection[ConfigEntry]) *server {
	reg := newWatcherRegistry()

	// Wire up the OnChange hook. This fires on every replica after each
	// committed write — outside the Raft lock, in a dedicated dispatcher
	// goroutine. We just fan out to local SSE subscribers.
	configs.OnChange(func(key string, entry *ConfigEntry, deleted bool) {
		ev := WatchEvent{Key: key, Deleted: deleted}
		if !deleted && entry != nil {
			ev.Value = entry.Value
			ev.Version = entry.Version
		}
		reg.notify(ev)
	})

	return &server{configs: configs, reg: reg}
}

// setConfig upserts a config entry with the current timestamp as version.
func (s *server) setConfig(ctx context.Context, key, value string) error {
	return s.configs.Upsert(ctx, key, ConfigEntry{
		Value:   value,
		Version: time.Now().UnixNano(),
	})
}

func (s *server) handleSet(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	if key == "" {
		http.Error(w, "missing key", http.StatusBadRequest)
		return
	}

	var body struct {
		Value string `json:"value"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	if err := s.setConfig(r.Context(), key, body.Value); err != nil {
		if errors.Is(err, easyraft.ErrNotLeader) {
			http.Error(w, "not leader", http.StatusServiceUnavailable)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) handleGet(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	if key == "" {
		http.Error(w, "missing key", http.StatusBadRequest)
		return
	}

	var (
		entry ConfigEntry
		err   error
	)
	if r.URL.Query().Get("consistency") == "stale" {
		entry, err = s.configs.ReadStale(key)
	} else {
		entry, err = s.configs.Read(r.Context(), key)
	}

	if err != nil {
		if errors.Is(err, easyraft.ErrKeyNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(entry)
}

func (s *server) handleDelete(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	if key == "" {
		http.Error(w, "missing key", http.StatusBadRequest)
		return
	}

	if err := s.configs.Delete(r.Context(), key); err != nil {
		if errors.Is(err, easyraft.ErrKeyNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if errors.Is(err, easyraft.ErrNotLeader) {
			http.Error(w, "not leader", http.StatusServiceUnavailable)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
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
// SSE format:
//
//	event: change
//	data: {"key":"db.host","value":"localhost","version":1712345678901234567}
//
//	event: delete
//	data: {"key":"db.host","deleted":true}
func (s *server) handleWatch(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key") // empty for /watch

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // disable nginx buffering
	flusher.Flush()

	ch := s.reg.subscribe(key)
	defer s.reg.unsubscribe(key, ch)

	// Send current snapshot on connect so the client starts from a known state.
	if all, err := s.configs.ListStale(); err == nil {
		for k, entry := range all {
			if key != "" && k != key {
				continue
			}
			ev := WatchEvent{Key: k, Value: entry.Value, Version: entry.Version}
			sendSSE(w, "snapshot", ev)
		}
		flusher.Flush()
	}

	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			eventType := "change"
			if ev.Deleted {
				eventType = "delete"
			}
			sendSSE(w, eventType, ev)
			flusher.Flush()
		}
	}
}

func sendSSE(w http.ResponseWriter, event string, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
}

func main() {
	id := flag.String("id", "", "Raft node ID (required)")
	raftAddr := flag.String("raft-addr", ":7001", "Raft gRPC listen address")
	httpAddr := flag.String("http-addr", ":8001", "HTTP listen address")
	dataDir := flag.String("data-dir", "", "Persistent data directory (required)")
	join := flag.String("join", "", "Comma-separated HTTP addresses of seed nodes to join")
	flag.Parse()

	if *id == "" || *dataDir == "" {
		fmt.Fprintln(os.Stderr, "usage: configsvc --id <id> --data-dir <dir> [--raft-addr :7001] [--http-addr :8001] [--join host:port,...]")
		os.Exit(1)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	opts := []easyraft.Option{
		easyraft.WithID(raft.NodeID(*id)),
		easyraft.WithRaftAddr(*raftAddr),
		easyraft.WithHTTPAddr(*httpAddr),
		easyraft.WithDataDir(*dataDir),
		easyraft.WithLogger(logger),
	}

	if *join != "" {
		addrs := strings.Split(*join, ",")
		opts = append(opts, easyraft.WithJoinAddr(addrs...))
	}

	// Build the store and typed collection handle separately so we can
	// register the OnChange hook before Start is called.
	store, err := easyraft.NewStore(opts...)
	if err != nil {
		logger.Error("failed to create store", "err", err)
		os.Exit(1)
	}

	configs := easyraft.AddCollection[ConfigEntry](store, "configs")

	srv := newServer(configs)

	// HTTP routes
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /configs/{key}", srv.handleSet)
	mux.HandleFunc("GET /configs/{key}", srv.handleGet)
	mux.HandleFunc("DELETE /configs/{key}", srv.handleDelete)
	mux.HandleFunc("GET /configs", srv.handleList)
	mux.HandleFunc("GET /watch/{key}", srv.handleWatch)
	mux.HandleFunc("GET /watch", srv.handleWatch)

	httpSrv := &http.Server{
		Addr:    *httpAddr,
		Handler: mux,
	}

	store.Start()
	defer store.Stop()

	logger.Info("configsvc started", "id", *id, "raft", *raftAddr, "http", *httpAddr)

	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("HTTP server error", "err", err)
		os.Exit(1)
	}
}
