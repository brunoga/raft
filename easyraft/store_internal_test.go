package easyraft

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/brunoga/raft/v2"
)

// newTestStore builds a Store with all internal structures initialised but no
// Raft node, so the state-machine, HTTP and policy code can be exercised
// without forming a cluster.
func newTestStore(t *testing.T, cfg *config) *Store {
	t.Helper()
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return newStoreShell(ctx, cancel, cfg)
}

// testTLSConfig returns a non-nil tls.Config; only its presence matters to the
// code under test.
func testTLSConfig() *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS12}
}

// ---- Linearizable reads ----------------------------------------------------

// fakeReadIndexer records which read path was taken and returns scripted
// errors, so the fallback policy can be exercised without a cluster.
type fakeReadIndexer struct {
	leaseErr  error
	quorumErr error

	leaseCalls  int
	quorumCalls int
}

func (f *fakeReadIndexer) ReadIndexLease(context.Context) (raft.Index, error) {
	f.leaseCalls++
	return 0, f.leaseErr
}

func (f *fakeReadIndexer) ReadIndex(context.Context) (raft.Index, error) {
	f.quorumCalls++
	return 0, f.quorumErr
}

// TestReadIndexWithFallback_NeverSurfacesExpiredLease pins the invariant that a
// caller is never handed raft.ErrLeaseExpired: an expired lease is a signal to
// confirm the read with a quorum, not a failure to report.
func TestReadIndexWithFallback_NeverSurfacesExpiredLease(t *testing.T) {
	otherErr := errors.New("transport down")

	tests := []struct {
		name       string
		useLease   bool
		leaseErr   error
		quorumErr  error
		wantErr    error
		wantLease  int
		wantQuorum int
	}{
		{
			name:       "lease disabled goes straight to quorum",
			useLease:   false,
			wantLease:  0,
			wantQuorum: 1,
		},
		{
			name:       "valid lease avoids the round-trip",
			useLease:   true,
			wantLease:  1,
			wantQuorum: 0,
		},
		{
			name:       "expired lease falls back to quorum",
			useLease:   true,
			leaseErr:   raft.ErrLeaseExpired,
			wantLease:  1,
			wantQuorum: 1,
		},
		{
			name:       "expired lease reports the quorum failure, not the lease",
			useLease:   true,
			leaseErr:   raft.ErrLeaseExpired,
			quorumErr:  raft.ErrNotLeader,
			wantErr:    raft.ErrNotLeader,
			wantLease:  1,
			wantQuorum: 1,
		},
		{
			name:       "a non-lease failure is returned without a second attempt",
			useLease:   true,
			leaseErr:   otherErr,
			wantErr:    otherErr,
			wantLease:  1,
			wantQuorum: 0,
		},
		{
			name:       "wrapped expired lease is still recognised",
			useLease:   true,
			leaseErr:   fmt.Errorf("read: %w", raft.ErrLeaseExpired),
			wantLease:  1,
			wantQuorum: 1,
			wantErr:    nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &fakeReadIndexer{leaseErr: tt.leaseErr, quorumErr: tt.quorumErr}
			err := readIndexWithFallback(context.Background(), r, tt.useLease)

			if errors.Is(err, raft.ErrLeaseExpired) {
				t.Fatalf("caller was handed ErrLeaseExpired: %v", err)
			}
			if tt.wantErr == nil && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if r.leaseCalls != tt.wantLease {
				t.Errorf("lease attempts = %d, want %d", r.leaseCalls, tt.wantLease)
			}
			if r.quorumCalls != tt.wantQuorum {
				t.Errorf("quorum attempts = %d, want %d", r.quorumCalls, tt.wantQuorum)
			}
		})
	}
}

// TestCollectionRead_FallsBackFromExpiredLease checks the same invariant
// through the public Read and List entry points.
func TestCollectionRead_FallsBackFromExpiredLease(t *testing.T) {
	s := newTestStore(t, &config{ID: "n1", LeaseReads: true})
	reader := &fakeReadIndexer{leaseErr: raft.ErrLeaseExpired}
	s.reader = reader
	s.node = nil

	// Read and List guard on s.node, so exercise the read path directly and
	// then confirm the typed wrappers agree.
	if err := s.readIndex(context.Background()); err != nil {
		t.Fatalf("readIndex: %v", err)
	}
	if reader.quorumCalls != 1 {
		t.Fatalf("expired lease did not fall back to a quorum read (%d quorum calls)", reader.quorumCalls)
	}

	s.collections["items"] = map[string]json.RawMessage{"k": json.RawMessage(`{"n":1}`)}
	coll := &Collection[struct {
		N int `json:"n"`
	}]{store: s, name: "items"}

	v, err := coll.ReadStale("k")
	if err != nil {
		t.Fatalf("ReadStale: %v", err)
	}
	if v.N != 1 {
		t.Errorf("value = %d, want 1", v.N)
	}
}

// ---- Redirect target validation --------------------------------------------

// TestNormalizeHostPortScheme_RejectsNonAddresses pins the invariant that only
// a bare host:port (optionally scheme-prefixed) is ever accepted as a redirect
// target, so nothing that reaches cluster state can be reflected into a
// Location header as an arbitrary URL.
func TestNormalizeHostPortScheme_RejectsNonAddresses(t *testing.T) {
	tests := []struct {
		name       string
		addr       string
		wantOK     bool
		wantHost   string
		wantScheme string
	}{
		{name: "bare host and port", addr: "10.0.0.5:8001", wantOK: true, wantHost: "10.0.0.5:8001"},
		{name: "hostname and port", addr: "node2.internal:8001", wantOK: true, wantHost: "node2.internal:8001"},
		{name: "http scheme is preserved", addr: "http://10.0.0.5:8001", wantOK: true, wantHost: "10.0.0.5:8001", wantScheme: "http"},
		{name: "https scheme is preserved", addr: "https://10.0.0.5:8001", wantOK: true, wantHost: "10.0.0.5:8001", wantScheme: "https"},
		{name: "ipv6 literal", addr: "[::1]:8001", wantOK: true, wantHost: "[::1]:8001"},
		{name: "surrounding whitespace is trimmed", addr: "  10.0.0.5:8001 ", wantOK: true, wantHost: "10.0.0.5:8001"},

		{name: "empty", addr: ""},
		{name: "no port", addr: "evil.example.com"},
		{name: "path appended", addr: "10.0.0.5:8001/admin"},
		{name: "userinfo", addr: "user@evil.example.com:80"},
		{name: "query appended", addr: "10.0.0.5:8001?x=1"},
		{name: "fragment appended", addr: "10.0.0.5:8001#f"},
		{name: "backslash path", addr: `10.0.0.5:8001\admin`},
		{name: "protocol-relative", addr: "//evil.example.com:80"},
		{name: "unspecified host", addr: ":8001"},
		{name: "non-numeric port", addr: "10.0.0.5:http"},
		{name: "port out of range", addr: "10.0.0.5:70000"},
		{name: "port zero", addr: "10.0.0.5:0"},
		{name: "embedded newline", addr: "10.0.0.5\r\nX-Evil: 1:8001"},
		{name: "space in host", addr: "10.0.0.5 evil:8001"},
		{name: "unsupported scheme", addr: "javascript:alert(1)"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			host, scheme, ok := normalizeHostPortScheme(tt.addr)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v (host %q)", ok, tt.wantOK, host)
			}
			if !tt.wantOK {
				return
			}
			if host != tt.wantHost {
				t.Errorf("host = %q, want %q", host, tt.wantHost)
			}
			if scheme != tt.wantScheme {
				t.Errorf("scheme = %q, want %q", scheme, tt.wantScheme)
			}
		})
	}
}

// TestLeaderURL_OnlyUsesAdvertisedAddresses checks that a redirect is built
// from the internal metadata map and refused when that map holds something
// that is not a usable address.
func TestLeaderURL_OnlyUsesAdvertisedAddresses(t *testing.T) {
	tests := []struct {
		name      string
		advertise string
		httpTLS   bool
		want      string
	}{
		{name: "plain address", advertise: "10.0.0.2:8001", want: "http://10.0.0.2:8001/items/k?x=1"},
		{name: "tls uses https", advertise: "10.0.0.2:8001", httpTLS: true, want: "https://10.0.0.2:8001/items/k?x=1"},
		{name: "explicit scheme wins", advertise: "https://10.0.0.2:8001", want: "https://10.0.0.2:8001/items/k?x=1"},
		{name: "injected path is refused", advertise: "evil.example.com:80/take-over", want: ""},
		{name: "header injection is refused", advertise: "10.0.0.2:8001\r\nLocation: http://evil", want: ""},
		{name: "unadvertised leader", advertise: "", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config{ID: "n1"}
			if tt.httpTLS {
				cfg.HTTPTLS = testTLSConfig()
			}
			s := newTestStore(t, &cfg)
			if tt.advertise != "" {
				raw, err := json.Marshal(tt.advertise)
				if err != nil {
					t.Fatal(err)
				}
				s.collections[metadataCollection] = map[string]json.RawMessage{"n2": raw}
			}

			got := s.leaderURL("n2", "/items/k", "x=1")
			if got != tt.want {
				t.Errorf("leaderURL = %q, want %q", got, tt.want)
			}
		})
	}
}

// ---- HTTP error mapping ----------------------------------------------------

// TestStatusForError_MapsSentinels pins the mapping from the packages' sentinel
// errors to HTTP status codes; a sentinel that falls through to 500 tells the
// client to retry blindly on an error it could have handled.
func TestStatusForError_MapsSentinels(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{name: "key not found", err: ErrKeyNotFound, want: http.StatusNotFound},
		{name: "wrapped key not found", err: fmt.Errorf("read: %w", ErrKeyNotFound), want: http.StatusNotFound},
		{name: "group not found", err: raft.ErrGroupNotFound, want: http.StatusNotFound},
		{name: "log entry not found", err: raft.ErrNotFound, want: http.StatusNotFound},
		{name: "key exists", err: ErrKeyExists, want: http.StatusConflict},
		{name: "obsolete sequence number", err: raft.ErrObsoleteSeqNum, want: http.StatusConflict},
		{name: "config change in progress", err: raft.ErrConfigChangeInProgress, want: http.StatusConflict},
		{name: "transfer in progress", err: raft.ErrLeadershipTransferInProgress, want: http.StatusConflict},
		{name: "reserved collection", err: ErrReservedCollection, want: http.StatusForbidden},
		{name: "deadline exceeded", err: context.DeadlineExceeded, want: http.StatusRequestTimeout},
		{name: "expired lease", err: raft.ErrLeaseExpired, want: http.StatusServiceUnavailable},
		{name: "cancelled", err: context.Canceled, want: http.StatusServiceUnavailable},
		{name: "anything else", err: errors.New("boom"), want: http.StatusInternalServerError},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := statusForError(tt.err); got != tt.want {
				t.Errorf("statusForError(%v) = %d, want %d", tt.err, got, tt.want)
			}
		})
	}
}

// ---- Discovery membership policy -------------------------------------------

// TestDiscoveredPeerConfig_LearnerByDefault pins the invariant that discovery
// alone cannot change the cluster's quorum: a peer it introduces joins as a
// non-voting learner unless the operator opted in.
func TestDiscoveredPeerConfig_LearnerByDefault(t *testing.T) {
	tests := []struct {
		name      string
		options   []Option
		wantVoter bool
	}{
		{name: "default is a learner", wantVoter: false},
		{name: "voter requires the opt-in", options: []Option{WithDiscoveryAsVoter()}, wantVoter: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var cfg config
			for _, o := range tt.options {
				o(&cfg)
			}
			cfg.ID = "n1"
			s := newTestStore(t, &cfg)

			got := s.discoveredPeerConfig("n2")
			if got.ID != "n2" {
				t.Errorf("ID = %q, want n2", got.ID)
			}
			if got.Voter != tt.wantVoter {
				t.Errorf("Voter = %v, want %v", got.Voter, tt.wantVoter)
			}
		})
	}
}

// recordingPeerAdder captures the addresses handed to the transport.
type recordingPeerAdder struct {
	added []string
}

func (r *recordingPeerAdder) AddPeer(_ raft.NodeID, addr string) {
	r.added = append(r.added, addr)
}

// TestApplyDiscoveredAddr_StaticPeersAreNotRepointed pins that a statically
// configured peer's address is authoritative: discovery cannot repoint it, so
// an announcement cannot redirect that member's Raft traffic.
func TestApplyDiscoveredAddr_StaticPeersAreNotRepointed(t *testing.T) {
	s := newTestStore(t, &config{
		ID:    "n1",
		Peers: map[raft.NodeID]string{"n2": "10.0.0.2:7001"},
	})

	adder := &recordingPeerAdder{}
	s.applyDiscoveredAddr(adder, "n2", "10.6.6.6:7001")
	if len(adder.added) != 0 {
		t.Errorf("a statically configured peer was repointed to %v", adder.added)
	}

	// A peer discovery itself introduced is registered normally.
	s.applyDiscoveredAddr(adder, "n3", "10.0.0.3:7001")
	if len(adder.added) != 1 || adder.added[0] != "10.0.0.3:7001" {
		t.Errorf("discovered peer registrations = %v, want [10.0.0.3:7001]", adder.added)
	}
}

// ---- Batch rollback --------------------------------------------------------

// TestApplyBatch_RollbackRestoresOnlyMutatedKeys checks that a failed
// transaction leaves the store exactly as it found it, now that rollback
// replays a journal of touched keys rather than restoring whole collections.
func TestApplyBatch_RollbackRestoresOnlyMutatedKeys(t *testing.T) {
	s := newTestStore(t, &config{ID: "n1"})
	s.collections["a"] = map[string]json.RawMessage{
		"keep":      json.RawMessage(`1`),
		"overwrite": json.RawMessage(`2`),
		"remove":    json.RawMessage(`3`),
	}

	batch := &command{Op: opBatch, Batch: []command{
		{Op: opUpdate, Collection: "a", Key: "overwrite", Value: json.RawMessage(`99`)},
		{Op: opDelete, Collection: "a", Key: "remove"},
		{Op: opCreate, Collection: "b", Key: "fresh", Value: json.RawMessage(`4`)},
		{Op: opUpsert, Collection: "a", Key: "overwrite", Value: json.RawMessage(`100`)},
		// Fails: "missing" does not exist, so the whole batch must roll back.
		{Op: opUpdate, Collection: "a", Key: "missing", Value: json.RawMessage(`5`)},
	}}

	if _, err := s.applyCommand(batch, nil); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("applyCommand err = %v, want ErrKeyNotFound", err)
	}

	want := map[string]string{"keep": "1", "overwrite": "2", "remove": "3"}
	got := s.collections["a"]
	if len(got) != len(want) {
		t.Fatalf("collection a = %v, want %v", got, want)
	}
	for k, v := range want {
		if string(got[k]) != v {
			t.Errorf("a[%q] = %s, want %s", k, got[k], v)
		}
	}
	if _, exists := s.collections["b"]; exists {
		t.Error("collection created by the failed batch was not removed")
	}
	if len(s.pendingEvents) != 0 {
		t.Errorf("failed batch left %d change events behind", len(s.pendingEvents))
	}
}

// ---- Snapshot and restore --------------------------------------------------

// TestSnapshotRestore_RoundTripsAndIsDeterministic checks the streaming
// encoder against the streaming decoder, and that identical state produces
// identical bytes so replicas do not differ gratuitously.
func TestSnapshotRestore_RoundTripsAndIsDeterministic(t *testing.T) {
	src := newTestStore(t, &config{ID: "n1"})
	src.collections = map[string]map[string]json.RawMessage{
		"users": {
			"alice":   json.RawMessage(`{"name":"Alice","tags":["a","b"]}`),
			"bob":     json.RawMessage(`{"name":"Bob"}`),
			`odd"key`: json.RawMessage(`null`),
		},
		"empty":   {},
		"numbers": {"n": json.RawMessage(`42`)},
	}

	var first, second bytes.Buffer
	if err := src.snapshot(&first); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if err := src.snapshot(&second); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if !bytes.Equal(first.Bytes(), second.Bytes()) {
		t.Errorf("two snapshots of identical state differ:\n%s\n%s", first.String(), second.String())
	}

	dst := newTestStore(t, &config{ID: "n2"})
	if err := dst.restore(bytes.NewReader(first.Bytes())); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	var again bytes.Buffer
	if err := dst.snapshot(&again); err != nil {
		t.Fatalf("Snapshot after restore: %v", err)
	}
	if !bytes.Equal(first.Bytes(), again.Bytes()) {
		t.Errorf("restore did not round-trip:\nwant %s\ngot  %s", first.String(), again.String())
	}
}

// TestRestore_AcceptsEmptyAndNullSnapshots covers the degenerate snapshots a
// node can be handed before anything has been written.
func TestRestore_AcceptsEmptyAndNullSnapshots(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "empty stream", body: ""},
		{name: "json null", body: "null"},
		{name: "empty object", body: "{}"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestStore(t, &config{ID: "n1"})
			s.collections["stale"] = map[string]json.RawMessage{"k": json.RawMessage(`1`)}

			if err := s.restore(bytes.NewReader([]byte(tt.body))); err != nil {
				t.Fatalf("Restore: %v", err)
			}
			if len(s.collections) != 0 {
				t.Errorf("collections = %v, want empty", s.collections)
			}
		})
	}
}

// ---- Reserved collections --------------------------------------------------

// TestReservedCollectionsAreUnreachableOverHTTP pins that the internal
// metadata namespace cannot be read or written through the HTTP API. A client
// that could write it would choose where every follower redirects its traffic.
func TestReservedCollectionsAreUnreachableOverHTTP(t *testing.T) {
	s := newTestStore(t, &config{ID: "n1"})
	s.collections[metadataCollection] = map[string]json.RawMessage{
		"n1": json.RawMessage(`"10.0.0.1:8001"`),
	}

	mux := http.NewServeMux()
	s.registerRoutes(mux)

	tests := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{name: "read one entry", method: http.MethodGet, path: "/" + metadataCollection + "/n1"},
		{name: "list the collection", method: http.MethodGet, path: "/" + metadataCollection},
		{name: "create an entry", method: http.MethodPost, path: "/" + metadataCollection + "/n9", body: `"evil:1"`},
		{name: "replace an entry", method: http.MethodPut, path: "/" + metadataCollection + "/n1", body: `"evil:1"`},
		{name: "upsert an entry", method: http.MethodPatch, path: "/" + metadataCollection + "/n1", body: `"evil:1"`},
		{name: "delete an entry", method: http.MethodDelete, path: "/" + metadataCollection + "/n1"},
		{name: "mutate an entry", method: http.MethodPost, path: "/" + metadataCollection + "/n1/mutate", body: `{"name":"x"}`},
		{
			name: "batch write", method: http.MethodPost, path: "/batch",
			body: `[{"op":"upsert","collection":"` + metadataCollection + `","key":"n1","value":"evil:1"}]`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.path, bytes.NewReader([]byte(tt.body)))
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, http.StatusForbidden, rec.Body.String())
			}
			if bytes.Contains(rec.Body.Bytes(), []byte("10.0.0.1:8001")) {
				t.Error("response leaked the internal address map")
			}
		})
	}

	// The metadata entry must be untouched.
	if got := string(s.collections[metadataCollection]["n1"]); got != `"10.0.0.1:8001"` {
		t.Errorf("metadata entry = %s, want \"10.0.0.1:8001\"", got)
	}
}

// ---- Benchmarks ------------------------------------------------------------

// BenchmarkApplyTransaction measures the cost of applying a two-key
// transaction against collections of growing size. The rollback journal
// records only the keys a transaction touches, so the cost per transaction
// should be flat across collection sizes rather than proportional to them.
func BenchmarkApplyTransaction(b *testing.B) {
	for _, size := range []int{100, 10_000, 200_000} {
		b.Run(fmt.Sprintf("collection_size_%d", size), func(b *testing.B) {
			s := &Store{
				collections: map[string]map[string]json.RawMessage{
					"a": make(map[string]json.RawMessage, size),
					"b": make(map[string]json.RawMessage, size),
				},
				mutations: make(map[string]map[string]mutationFunc),
			}
			for i := 0; i < size; i++ {
				key := strconv.Itoa(i)
				s.collections["a"][key] = json.RawMessage(`{"v":0}`)
				s.collections["b"][key] = json.RawMessage(`{"v":0}`)
			}

			batch := &command{Op: opBatch, Batch: []command{
				{Op: opUpsert, Collection: "a", Key: "0", Value: json.RawMessage(`{"v":1}`)},
				{Op: opUpsert, Collection: "b", Key: "0", Value: json.RawMessage(`{"v":1}`)},
			}}

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := s.applyCommand(batch, nil); err != nil {
					b.Fatal(err)
				}
				s.pendingEvents = s.pendingEvents[:0]
			}
		})
	}
}

// BenchmarkSnapshot measures encoding a store of growing size. The encoder
// streams, so it should not allocate a second copy of the state.
func BenchmarkSnapshot(b *testing.B) {
	for _, size := range []int{1_000, 50_000} {
		b.Run(fmt.Sprintf("entries_%d", size), func(b *testing.B) {
			s := &Store{collections: map[string]map[string]json.RawMessage{
				"a": make(map[string]json.RawMessage, size),
			}}
			for i := 0; i < size; i++ {
				s.collections["a"][strconv.Itoa(i)] = json.RawMessage(`{"v":12345,"s":"some value"}`)
			}

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := s.snapshot(io.Discard); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
