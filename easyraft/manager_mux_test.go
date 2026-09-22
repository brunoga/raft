package easyraft_test

import (
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/brunoga/raft/v2"
	"github.com/brunoga/raft/v2/easyraft"
)

// TestManager_ServesOnACallerMux pins that a Manager registers its routes on
// [easyraft.WithHTTPMux] like a Store does.
//
// It did not. The option compiled, read as set, and was ignored: the Manager
// always built a mux of its own. An application that wanted its own routes on
// the same port had no way to say so, and no way to find out that it had
// failed to -- the Manager's routes simply were not there.
func TestManager_ServesOnACallerMux(t *testing.T) {
	raftAddr, httpAddr := freePort(t), freePort(t)
	mux := http.NewServeMux()

	// The application's own route, on the same mux and therefore the same
	// port as easyraft's.
	mux.HandleFunc("GET /app/hello", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("hello"))
	})

	m, err := easyraft.NewManager(
		easyraft.WithID("n1"),
		easyraft.WithRaftAddr(raftAddr),
		easyraft.WithHTTPAddr(httpAddr),
		easyraft.WithHTTPMux(mux),
		easyraft.WithInsecureTransportAcknowledged(),
		easyraft.WithInsecureHTTPAcknowledged(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, addErr := m.AddStore(1,
		easyraft.WithDataDir(filepath.Join(t.TempDir(), "g1")),
		easyraft.WithPeers(map[raft.NodeID]string{"n1": raftAddr}),
	); addErr != nil {
		t.Fatal(addErr)
	}
	if startErr := m.Start(); startErr != nil {
		t.Fatalf("Start: %v", startErr)
	}
	t.Cleanup(func() { _ = m.Stop() })

	// The caller serves; easyraft did not listen.
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	ln, err := net.Listen("tcp", httpAddr)
	if err != nil {
		t.Fatalf("the Manager listened on %s itself, leaving the caller nothing to serve on: %v",
			httpAddr, err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	base := "http://" + httpAddr

	// The application's route works.
	if status, body, _ := doRequest(t, http.MethodGet, base+"/app/hello", "", nil); status != http.StatusOK || body != "hello" {
		t.Errorf("the application's own route answered %d %q", status, body)
	}

	// And so do easyraft's, on the same port.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if status, _, _ := doRequest(t, http.MethodGet, base+"/groups/1/members", "", nil); status == http.StatusOK {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	status, body, _ := doRequest(t, http.MethodGet, base+"/groups/1/members", "", nil)
	if status != http.StatusOK {
		t.Fatalf("GET /groups/1/members on the caller's mux: %d %s", status, body)
	}
	var members struct {
		Members []struct {
			ID raft.NodeID `json:"id"`
		} `json:"members"`
	}
	if err := json.Unmarshal([]byte(body), &members); err != nil {
		t.Fatalf("decode members: %v", err)
	}
	if len(members.Members) != 1 || members.Members[0].ID != "n1" {
		t.Errorf("members are %+v", members.Members)
	}

	// A write through the Manager's own CRUD routes, so the data path is
	// registered too rather than only the management endpoints.
	for time.Now().Before(deadline) {
		if status, _, _ := doRequest(t, http.MethodPatch, base+"/groups/1/items/k",
			`{"value":1}`, nil); status == http.StatusNoContent {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if status, body, _ := doRequest(t, http.MethodGet, base+"/groups/1/items/k", "", nil); status != http.StatusOK {
		t.Errorf("GET a key written through the caller's mux: %d %s", status, body)
	}
	if status, _, _ := doRequest(t, http.MethodGet, base+"/__balance/status", "", nil); status != http.StatusOK {
		t.Errorf("the balance status route is missing from the caller's mux: %d", status)
	}
}
