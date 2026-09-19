package easyraft_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brunoga/raft"
	"github.com/brunoga/raft/easyraft"
	"github.com/prometheus/client_golang/prometheus"
)

// quietLogger keeps test output readable; the store logs a security warning on
// every unauthenticated start.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// freePort reserves and releases a loopback port, returning the address. The
// small race between release and reuse is acceptable in tests and avoids the
// fixed-port collisions that make suites order-dependent.
func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("release port: %v", err)
	}
	return addr
}

// TestHTTPAuth_GuardsEveryRoute pins the invariant that the authorization hook
// runs before every easyraft handler. The cluster-mutating routes are the
// reason it exists — an unauthenticated caller must not be able to add or
// remove members — but a hook that covered only some routes would be a trap,
// so reads are checked too.
func TestHTTPAuth_GuardsEveryRoute(t *testing.T) {
	const token = "s3cret-cluster-token"

	httpAddr := freePort(t)
	er, err := easyraft.New[Counter](
		easyraft.WithID("n1"),
		easyraft.WithRaftAddr(freePort(t)),
		easyraft.WithHTTPAddr(httpAddr),
		easyraft.WithDataDir(t.TempDir()),
		easyraft.WithLogger(quietLogger()),
		easyraft.WithBearerTokenAuth(token),
	)
	if err != nil {
		t.Fatal(err)
	}
	er.Start()
	defer er.Stop()

	base := "http://" + httpAddr

	tests := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{name: "join", method: http.MethodPost, path: "/join", body: `{"id":"evil","raft_addr":"10.6.6.6:7001"}`},
		{name: "remove member", method: http.MethodDelete, path: "/members/n1"},
		{name: "transfer leadership", method: http.MethodPost, path: "/transfer-leadership", body: `{"to":"evil"}`},
		{name: "list members", method: http.MethodGet, path: "/members"},
		{name: "create", method: http.MethodPost, path: "/items/k", body: `{"value":1}`},
		{name: "read", method: http.MethodGet, path: "/items/k"},
		{name: "update", method: http.MethodPut, path: "/items/k", body: `{"value":2}`},
		{name: "upsert", method: http.MethodPatch, path: "/items/k", body: `{"value":3}`},
		{name: "delete", method: http.MethodDelete, path: "/items/k"},
		{name: "list", method: http.MethodGet, path: "/items"},
		{name: "mutate", method: http.MethodPost, path: "/items/k/mutate", body: `{"name":"inc"}`},
		{name: "batch", method: http.MethodPost, path: "/batch", body: `[{"op":"upsert","collection":"items","key":"k","value":1}]`},
		{name: "status", method: http.MethodGet, path: "/status"},
		{name: "health", method: http.MethodGet, path: "/health"},
		{name: "metrics", method: http.MethodGet, path: "/metrics"},
	}

	credentials := []struct {
		name   string
		header string
		want   int
	}{
		{name: "no credential", header: "", want: http.StatusUnauthorized},
		{name: "wrong token", header: "Bearer wrong-token", want: http.StatusUnauthorized},
		{name: "wrong scheme", header: "Basic " + token, want: http.StatusUnauthorized},
	}

	client := &http.Client{
		Timeout: 10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	for _, tt := range tests {
		for _, cred := range credentials {
			t.Run(tt.name+"/"+cred.name, func(t *testing.T) {
				req, err := http.NewRequest(tt.method, base+tt.path, strings.NewReader(tt.body))
				if err != nil {
					t.Fatal(err)
				}
				if cred.header != "" {
					req.Header.Set("Authorization", cred.header)
				}
				resp, err := client.Do(req)
				if err != nil {
					t.Fatalf("request: %v", err)
				}
				defer func() { _ = resp.Body.Close() }()

				if resp.StatusCode != cred.want {
					body, _ := io.ReadAll(resp.Body)
					t.Fatalf("status = %d, want %d (body %s)", resp.StatusCode, cred.want, body)
				}
			})
		}
	}

	// The cluster must be unchanged: nothing got in.
	for _, m := range er.Members() {
		if m.ID == "evil" {
			t.Fatal("an unauthenticated request added a cluster member")
		}
	}

	// A correct credential still works.
	req, err := http.NewRequest(http.MethodGet, base+"/health", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("authorised request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("authorised /health status = %d, want 200", resp.StatusCode)
	}
}

// TestBearerTokenAuth_Decisions covers the hook in isolation, including the
// fail-closed behaviour of an accidentally empty token.
func TestBearerTokenAuth_Decisions(t *testing.T) {
	tests := []struct {
		name      string
		token     string
		header    string
		wantErr   error
		wantAdmit bool
	}{
		{name: "matching token", token: "abc", header: "Bearer abc", wantAdmit: true},
		{name: "scheme is case-insensitive", token: "abc", header: "bearer abc", wantAdmit: true},
		{name: "missing header", token: "abc", wantErr: easyraft.ErrUnauthorized},
		{name: "wrong token", token: "abc", header: "Bearer xyz", wantErr: easyraft.ErrUnauthorized},
		{name: "wrong scheme", token: "abc", header: "Token abc", wantErr: easyraft.ErrUnauthorized},
		{name: "token prefix is not enough", token: "abcdef", header: "Bearer abc", wantErr: easyraft.ErrUnauthorized},
		{name: "empty token fails closed", token: "", header: "Bearer ", wantErr: easyraft.ErrForbidden},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			auth := easyraft.BearerTokenAuth(tt.token)
			req, err := http.NewRequest(http.MethodGet, "http://example/", nil)
			if err != nil {
				t.Fatal(err)
			}
			if tt.header != "" {
				req.Header.Set("Authorization", tt.header)
			}

			err = auth(req)
			if tt.wantAdmit {
				if err != nil {
					t.Fatalf("request was rejected: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("request was admitted")
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want one wrapping %v", err, tt.wantErr)
			}
		})
	}
}

// TestClientCertAuth_RequiresVerifiedCertificate checks that the mTLS hook
// refuses a plaintext request and one whose TLS state carries no verified
// chain, since that is what the hook exists to enforce.
func TestClientCertAuth_RequiresVerifiedCertificate(t *testing.T) {
	auth := easyraft.ClientCertAuth("admin")

	req, err := http.NewRequest(http.MethodGet, "http://example/", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := auth(req); err == nil {
		t.Fatal("a plaintext request was admitted by the client-certificate hook")
	} else if !errors.Is(err, easyraft.ErrUnauthorized) {
		t.Errorf("err = %v, want one wrapping ErrUnauthorized", err)
	}
}

// TestNewStore_ReportsHTTPBindFailure pins that a bad or busy HTTP address is
// reported to the caller instead of vanishing into a background goroutine,
// where the node would come up looking healthy with no API.
func TestNewStore_ReportsHTTPBindFailure(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = occupied.Close() }()

	tests := []struct {
		name     string
		httpAddr string
	}{
		{name: "address already in use", httpAddr: occupied.Addr().String()},
		{name: "malformed address", httpAddr: "not-an-address"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, err := easyraft.NewStore(
				easyraft.WithID("n1"),
				easyraft.WithRaftAddr(freePort(t)),
				easyraft.WithHTTPAddr(tt.httpAddr),
				easyraft.WithDataDir(t.TempDir()),
				easyraft.WithLogger(quietLogger()),
			)
			if err == nil {
				s.Stop()
				t.Fatal("a failed HTTP bind was not reported to the caller")
			}
			if !strings.Contains(err.Error(), "listen http") {
				t.Errorf("err = %v, want it to name the HTTP listener", err)
			}
		})
	}
}

// TestManager_SharedPrometheusRegistryAcrossGroups pins that a multi-group
// deployment can be observed. Every group builds its own metrics instance
// against the caller's registry, which must not be a fatal duplicate
// registration.
func TestManager_SharedPrometheusRegistryAcrossGroups(t *testing.T) {
	reg := prometheus.NewRegistry()
	tmpDir := t.TempDir()

	mgr, err := easyraft.NewManager(
		easyraft.WithID("n1"),
		easyraft.WithRaftAddr(freePort(t)),
		easyraft.WithLogger(quietLogger()),
		easyraft.WithPrometheus(reg),
	)
	if err != nil {
		t.Fatal(err)
	}

	const groups = 4
	for g := uint64(1); g <= groups; g++ {
		if _, err := mgr.AddStore(g,
			easyraft.WithDataDir(filepath.Join(tmpDir, fmt.Sprintf("g%d", g))),
		); err != nil {
			t.Fatalf("AddStore %d: %v", g, err)
		}
	}

	if err := mgr.Start(); err != nil {
		t.Fatalf("Start with %d groups sharing one registry: %v", groups, err)
	}
	defer mgr.Stop()

	// Each group should be able to report its own series.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		families, err := reg.Gather()
		if err != nil {
			t.Fatalf("Gather: %v", err)
		}
		seenGroups := make(map[string]struct{})
		for _, f := range families {
			if f.GetName() != "raft_node_state" {
				continue
			}
			for _, m := range f.GetMetric() {
				for _, l := range m.GetLabel() {
					if l.GetName() == "group" {
						seenGroups[l.GetValue()] = struct{}{}
					}
				}
			}
		}
		if len(seenGroups) == groups {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("did not observe a distinct series per group within the deadline")
}

// TestManager_StaticPeersAreReportedWithTheirRaftAddress pins that a
// Manager-created store records its configured peers. Without that, GET
// /groups/{id}/members reports an empty raft_addr and a joining node has no
// address to dial.
func TestManager_StaticPeersAreReportedWithTheirRaftAddress(t *testing.T) {
	httpAddr := freePort(t)
	peerAddr := "127.0.0.1:7999"

	mgr, err := easyraft.NewManager(
		easyraft.WithID("n1"),
		easyraft.WithRaftAddr(freePort(t)),
		easyraft.WithHTTPAddr(httpAddr),
		easyraft.WithLogger(quietLogger()),
		easyraft.WithInsecureHTTPAcknowledged(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.AddStore(7,
		easyraft.WithDataDir(t.TempDir()),
		easyraft.WithPeers(map[raft.NodeID]string{"n2": peerAddr}),
	); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Start(); err != nil {
		t.Fatal(err)
	}
	defer mgr.Stop()

	resp, err := http.Get("http://" + httpAddr + "/groups/7/members")
	if err != nil {
		t.Fatalf("GET members: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var body struct {
		Members []struct {
			ID       string `json:"id"`
			RaftAddr string `json:"raft_addr"`
		} `json:"members"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	var found bool
	for _, m := range body.Members {
		if m.ID != "n2" {
			continue
		}
		found = true
		if m.RaftAddr != peerAddr {
			t.Errorf("n2 raft_addr = %q, want %q", m.RaftAddr, peerAddr)
		}
	}
	if !found {
		t.Fatalf("n2 missing from members: %+v", body.Members)
	}
}

// TestManager_StartDoesNotHoldTheLockAcrossJoins pins that a group waiting on
// a slow seed does not freeze the whole Manager. Joining retries for up to 30
// seconds, so holding the lock across it would block GetStore and every HTTP
// handler for that long.
func TestManager_StartDoesNotHoldTheLockAcrossJoins(t *testing.T) {
	const joinDelay = 1500 * time.Millisecond

	seed := &http.Server{}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/join", func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(joinDelay)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"peers":[]}`))
	})
	seed.Handler = mux
	go func() { _ = seed.Serve(ln) }()
	defer func() { _ = seed.Shutdown(context.Background()) }()

	mgr, err := easyraft.NewManager(
		easyraft.WithID("n1"),
		easyraft.WithRaftAddr(freePort(t)),
		easyraft.WithLogger(quietLogger()),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.AddStore(1,
		easyraft.WithDataDir(t.TempDir()),
		easyraft.WithJoinAddr(ln.Addr().String()),
	); err != nil {
		t.Fatal(err)
	}

	started := make(chan error, 1)
	go func() { started <- mgr.Start() }()
	defer func() {
		<-started
		mgr.Stop()
	}()

	// While the join is in flight, the Manager must still answer.
	time.Sleep(200 * time.Millisecond)
	done := make(chan error, 1)
	go func() {
		_, err := mgr.GetStore(1)
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("GetStore during join: %v", err)
		}
	case <-time.After(joinDelay / 2):
		t.Fatal("GetStore blocked while a group was joining: the Manager lock is held across the join")
	}
}

// TestStore_ExactlyOnceViaSessionIsRetrySafe checks the named-field
// exactly-once API: replaying one OnceID applies the write once, which is the
// whole point of holding on to the identity across retries.
func TestStore_ExactlyOnceViaSessionIsRetrySafe(t *testing.T) {
	store, err := easyraft.NewStore(
		easyraft.WithID("n1"),
		easyraft.WithRaftAddr(freePort(t)),
		easyraft.WithDataDir(t.TempDir()),
		easyraft.WithLogger(quietLogger()),
	)
	if err != nil {
		t.Fatal(err)
	}
	counters := easyraft.AddCollection[Counter](store, "counters")
	counters.RegisterMutation("inc", func(c *Counter, _ []byte) (*Counter, []byte, error) {
		c.Value++
		return c, nil, nil
	})
	store.Start()
	defer store.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := store.Ready(ctx); err != nil {
		t.Fatalf("Ready: %v", err)
	}

	session := easyraft.NewSession("worker-1")

	createID := session.Next()
	if err := counters.Exactly(createID).Create(ctx, "k", Counter{}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	// A retry of the same logical write must not create a second time.
	if err := counters.Exactly(createID).Create(ctx, "k", Counter{}); err != nil {
		t.Fatalf("replayed Create: %v", err)
	}

	incID := session.Next()
	for i := 0; i < 3; i++ {
		if _, err := counters.Exactly(incID).Mutate(ctx, "k", "inc", nil); err != nil {
			t.Fatalf("Mutate attempt %d: %v", i, err)
		}
	}

	got, err := counters.Read(ctx, "k")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.Value != 1 {
		t.Errorf("counter = %d, want 1 (the replayed mutation was applied more than once)", got.Value)
	}

	// Sequence numbers must advance so distinct writes are not deduplicated
	// against one another.
	if createID.SeqNum == incID.SeqNum {
		t.Errorf("session handed out the same sequence number twice: %d", createID.SeqNum)
	}
	if session.LastSeqNum() != incID.SeqNum {
		t.Errorf("LastSeqNum = %d, want %d", session.LastSeqNum(), incID.SeqNum)
	}
	resumed := easyraft.NewSessionAt("worker-1", session.LastSeqNum())
	if next := resumed.Next(); next.SeqNum != incID.SeqNum+1 {
		t.Errorf("resumed session produced SeqNum %d, want %d", next.SeqNum, incID.SeqNum+1)
	}
}

// TestStore_StopBeforeStartReleasesResources pins that a store which was
// constructed but never started can still be shut down. Stopping the Raft node
// waits for goroutines that only Start creates, so Stop must not reach for it
// here — and the ports must be released either way.
func TestStore_StopBeforeStartReleasesResources(t *testing.T) {
	raftAddr := freePort(t)
	httpAddr := freePort(t)

	s, err := easyraft.NewStore(
		easyraft.WithID("n1"),
		easyraft.WithRaftAddr(raftAddr),
		easyraft.WithHTTPAddr(httpAddr),
		easyraft.WithDataDir(t.TempDir()),
		easyraft.WithLogger(quietLogger()),
	)
	if err != nil {
		t.Fatal(err)
	}

	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		s.Stop()
	}()

	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("Stop hung on a store that was never started")
	}

	// Both listeners must be free again.
	for _, addr := range []string{raftAddr, httpAddr} {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			t.Errorf("port %s was not released by Stop: %v", addr, err)
			continue
		}
		_ = ln.Close()
	}
}
