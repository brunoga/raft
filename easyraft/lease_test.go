package easyraft_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/brunoga/raft/v2"
	"github.com/brunoga/raft/v2/easyraft"
)

// startLeaseNode brings up a one-node cluster that sweeps expired leases
// often, so a test can watch a key go without waiting a second for it.
func startLeaseNode(t *testing.T) (er *easyraft.EasyRaft[Counter], httpAddr string) {
	t.Helper()
	raftAddr, httpAddr := freePort(t), freePort(t)

	er, err := easyraft.New[Counter](
		easyraft.WithID("n1"),
		easyraft.WithRaftAddr(raftAddr),
		easyraft.WithHTTPAddr(httpAddr),
		easyraft.WithDataDir(filepath.Join(t.TempDir(), "n1")),
		easyraft.WithPeers(map[raft.NodeID]string{"n1": raftAddr}),
		easyraft.WithKeyLeaseSweepInterval(50*time.Millisecond),
		easyraft.WithInsecureTransportAcknowledged(),
		easyraft.WithInsecureHTTPAcknowledged(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if startErr := er.Start(); startErr != nil {
		t.Fatalf("Start: %v", startErr)
	}
	t.Cleanup(func() { _ = er.Stop() })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if readyErr := er.Ready(ctx); readyErr != nil {
		t.Fatalf("Ready: %v", readyErr)
	}
	return er, httpAddr
}

// waitGone polls until the key is gone, or fails. Generous on purpose: what
// is being waited for is a sweep and one log entry, and a failure here should
// mean the key is never removed rather than that the machine was busy.
func waitGone(t *testing.T, er *easyraft.EasyRaft[Counter], key string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := er.ReadStale(key); errors.Is(err, easyraft.ErrKeyNotFound) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s was still there after its lease should have expired", key)
}

// TestLease_KeyOutlivesNothing is the service-registry case end to end: a key
// registered under a lease disappears once nothing renews it, with no other
// process having to notice that its owner died.
func TestLease_KeyOutlivesNothing(t *testing.T) {
	er, _ := startLeaseNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	lease, err := er.GrantLease(ctx, 300*time.Millisecond)
	if err != nil {
		t.Fatalf("GrantLease: %v", err)
	}
	if lease == 0 {
		t.Fatal("GrantLease returned lease 0")
	}
	if err := er.UpsertWithLease(ctx, "instance-1", Counter{Value: 1}, lease); err != nil {
		t.Fatalf("UpsertWithLease: %v", err)
	}
	if err := er.Create(ctx, "permanent", Counter{Value: 2}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got := er.LeaseOf("instance-1"); got != lease {
		t.Errorf("LeaseOf returned %d, want %d", got, lease)
	}

	// Renewing keeps it alive well past its own TTL.
	renewUntil := time.Now().Add(time.Second)
	for time.Now().Before(renewUntil) {
		if _, err := er.KeepAlive(ctx, lease); err != nil {
			t.Fatalf("KeepAlive: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := er.ReadStale("instance-1"); err != nil {
		t.Fatalf("the key went while it was still being renewed: %v", err)
	}

	// Stop renewing and it goes.
	waitGone(t, er, "instance-1")
	if _, err := er.ReadStale("permanent"); err != nil {
		t.Errorf("a key with no lease was removed: %v", err)
	}
	if _, err := er.Lease(lease); !errors.Is(err, easyraft.ErrLeaseNotFound) {
		t.Errorf("the expired lease is still there: %v", err)
	}
	if err := er.UpsertWithLease(ctx, "late", Counter{}, lease); !errors.Is(err, easyraft.ErrLeaseNotFound) {
		t.Errorf("writing under an expired lease: %v, want ErrLeaseNotFound", err)
	}
}

// TestLease_KeepAliveLoopHoldsItAndReleasesIt covers the helper a service
// actually uses: one goroutine, and the registration lives exactly as long as
// the context it was given.
func TestLease_KeepAliveLoopHoldsItAndReleasesIt(t *testing.T) {
	er, _ := startLeaseNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	lease, err := er.GrantLease(ctx, 400*time.Millisecond)
	if err != nil {
		t.Fatalf("GrantLease: %v", err)
	}
	if err := er.UpsertWithLease(ctx, "held", Counter{Value: 1}, lease); err != nil {
		t.Fatalf("UpsertWithLease: %v", err)
	}

	loopCtx, stopLoop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- er.KeepAliveLoop(loopCtx, lease) }()

	// Well past the TTL, still there.
	time.Sleep(1200 * time.Millisecond)
	if _, err := er.ReadStale("held"); err != nil {
		t.Fatalf("the key went while the keep-alive loop was running: %v", err)
	}

	stopLoop()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("KeepAliveLoop returned %v, want context.Canceled", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("KeepAliveLoop did not return after its context was cancelled")
	}
	waitGone(t, er, "held")

	// A loop for a lease that does not exist says so rather than spinning.
	if err := er.KeepAliveLoop(ctx, lease); !errors.Is(err, easyraft.ErrLeaseNotFound) {
		t.Errorf("KeepAliveLoop on a dead lease: %v, want ErrLeaseNotFound", err)
	}
}

// TestLease_RevokeIsImmediate covers giving a registration up on purpose,
// which a process should do on a clean shutdown rather than leaving a stale
// entry for a whole TTL.
func TestLease_RevokeIsImmediate(t *testing.T) {
	er, _ := startLeaseNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	lease, err := er.GrantLease(ctx, time.Hour)
	if err != nil {
		t.Fatalf("GrantLease: %v", err)
	}
	if err := er.UpsertWithLease(ctx, "leaving", Counter{Value: 1}, lease); err != nil {
		t.Fatalf("UpsertWithLease: %v", err)
	}
	if err := er.RevokeLease(ctx, lease); err != nil {
		t.Fatalf("RevokeLease: %v", err)
	}
	if _, err := er.ReadStale("leaving"); !errors.Is(err, easyraft.ErrKeyNotFound) {
		t.Errorf("the key survived an explicit revocation: %v", err)
	}
	// Revoking again succeeds: the outcome asked for is the outcome.
	if err := er.RevokeLease(ctx, lease); err != nil {
		t.Errorf("revoking twice: %v", err)
	}

	// A TTL that is not positive is refused before anything is proposed.
	if _, err := er.GrantLease(ctx, 0); err == nil {
		t.Error("GrantLease(0) succeeded")
	}
	if _, err := er.GrantLease(ctx, -time.Second); err == nil {
		t.Error("GrantLease(-1s) succeeded")
	}
}

// TestLease_HTTP covers the same contract for a client that is not in Go.
func TestLease_HTTP(t *testing.T) {
	er, httpAddr := startLeaseNode(t)
	base := "http://" + httpAddr

	grantStatus, grantBody, _ := doRequest(t, http.MethodPost, base+"/__leases", `{"ttl_seconds":0.3}`, nil)
	if grantStatus != http.StatusCreated {
		t.Fatalf("grant: %d %s", grantStatus, grantBody)
	}
	var granted struct {
		ID         uint64  `json:"id"`
		TTLSeconds float64 `json:"ttl_seconds"`
	}
	if err := json.Unmarshal([]byte(grantBody), &granted); err != nil {
		t.Fatalf("decode grant: %v", err)
	}
	if granted.ID == 0 {
		t.Fatalf("grant returned id 0: %s", grantBody)
	}

	leaseParam := "?lease=" + strconv.FormatUint(granted.ID, 10)
	if status, body, _ := doRequest(t, http.MethodPatch,
		base+"/default/web-1"+leaseParam, `{"value":1}`, nil); status != http.StatusNoContent {
		t.Fatalf("leased upsert: %d %s", status, body)
	}
	if status, body, _ := doRequest(t, http.MethodGet,
		base+"/__leases/"+strconv.FormatUint(granted.ID, 10), "", nil); status != http.StatusOK {
		t.Fatalf("read lease: %d %s", status, body)
	} else if !strings.Contains(body, "web-1") {
		t.Errorf("the lease does not list its key: %s", body)
	}
	if status, body, _ := doRequest(t, http.MethodGet, base+"/__leases", "", nil); status != http.StatusOK {
		t.Fatalf("list leases: %d %s", status, body)
	}

	// Renewing over HTTP holds it past its TTL.
	renewUntil := time.Now().Add(time.Second)
	for time.Now().Before(renewUntil) {
		if status, body, _ := doRequest(t, http.MethodPost,
			base+"/__leases/"+strconv.FormatUint(granted.ID, 10)+"/keepalive", "", nil); status != http.StatusOK {
			t.Fatalf("keepalive: %d %s", status, body)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := er.ReadStale("web-1"); err != nil {
		t.Fatalf("the key went while it was being renewed over HTTP: %v", err)
	}

	waitGone(t, er, "web-1")

	// The lease is gone, and so are the endpoints that name it.
	if status, _, _ := doRequest(t, http.MethodGet,
		base+"/__leases/"+strconv.FormatUint(granted.ID, 10), "", nil); status != http.StatusNotFound {
		t.Errorf("reading an expired lease answered %d, want 404", status)
	}
	if status, _, _ := doRequest(t, http.MethodPost,
		base+"/__leases/"+strconv.FormatUint(granted.ID, 10)+"/keepalive", "", nil); status != http.StatusNotFound {
		t.Errorf("renewing an expired lease answered %d, want 404", status)
	}
	if status, _, _ := doRequest(t, http.MethodPatch,
		base+"/default/late"+leaseParam, `{"value":1}`, nil); status != http.StatusNotFound {
		t.Errorf("writing under an expired lease answered %d, want 404", status)
	}

	// Malformed input is refused rather than treated as "no lease".
	for _, bad := range []struct{ method, path, body string }{
		{http.MethodPost, "/__leases", `{"ttl_seconds":0}`},
		{http.MethodPost, "/__leases", `{"ttl_seconds":-5}`},
		{http.MethodPatch, "/default/x?lease=nope", `{"value":1}`},
		{http.MethodPatch, "/default/x?lease=0", `{"value":1}`},
		{http.MethodGet, "/__leases/notanumber", ""},
	} {
		if status, body, _ := doRequest(t, bad.method, base+bad.path, bad.body, nil); status != http.StatusBadRequest {
			t.Errorf("%s %s answered %d %s, want 400", bad.method, bad.path, status, body)
		}
	}

	// An explicit revocation over HTTP.
	grantStatus, grantBody, _ = doRequest(t, http.MethodPost, base+"/__leases", `{"ttl_seconds":3600}`, nil)
	if grantStatus != http.StatusCreated {
		t.Fatalf("second grant: %d %s", grantStatus, grantBody)
	}
	if err := json.Unmarshal([]byte(grantBody), &granted); err != nil {
		t.Fatal(err)
	}
	if status, body, _ := doRequest(t, http.MethodPatch,
		base+"/default/web-2?lease="+strconv.FormatUint(granted.ID, 10), `{"value":2}`, nil); status != http.StatusNoContent {
		t.Fatalf("second leased upsert: %d %s", status, body)
	}
	if status, body, _ := doRequest(t, http.MethodDelete,
		base+"/__leases/"+strconv.FormatUint(granted.ID, 10), "", nil); status != http.StatusNoContent {
		t.Fatalf("revoke: %d %s", status, body)
	}
	if _, err := er.ReadStale("web-2"); !errors.Is(err, easyraft.ErrKeyNotFound) {
		t.Errorf("the key survived revocation over HTTP: %v", err)
	}
}
