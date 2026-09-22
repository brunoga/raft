package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/brunoga/raft/v2/easyraft"
	"github.com/brunoga/raft/v2/easyraft/client"
	"github.com/brunoga/raft/v2/easyraft/easyrafttest"
	registry "github.com/brunoga/raft/v2/examples/serviceregistry/registry"
)

// discardLogger keeps the registry's own change log out of the test output;
// what the handlers answer is what these tests are about.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// freePort returns an address nothing is listening on.
func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if closeErr := ln.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	return addr
}

// startRegistry brings up a three-node registry serving this example's own
// routes, and returns the HTTP addresses.
//
// It uses easyrafttest for the cluster, so the Raft traffic runs on an
// in-memory network and an election takes milliseconds. The HTTP the test
// exercises is real, because that is the part this example is.
func startRegistry(t *testing.T, ttlSweep time.Duration) (cluster *easyrafttest.Cluster, addrs []string) {
	t.Helper()

	addrs = make([]string, 3)
	for i := range addrs {
		addrs[i] = freePort(t)
	}

	// One mux per node, built before the cluster so the routes are registered
	// before anything is served.
	muxes := make([]*http.ServeMux, 3)
	for i := range muxes {
		muxes[i] = http.NewServeMux()
	}

	c := easyrafttest.New(t, 3, easyrafttest.Options{
		Store: []easyraft.Option{
			easyraft.WithInsecureHTTPAcknowledged(),
			easyraft.WithKeyLeaseSweepInterval(ttlSweep),
		},
		PerNode: func(i int) []easyraft.Option {
			return []easyraft.Option{
				easyraft.WithHTTPAddr(addrs[i]),
				easyraft.WithHTTPMux(muxes[i]),
			}
		},
	})

	for i, store := range c.Stores {
		instances := easyraft.AddCollection[registry.Instance](store, registry.CollectionName)
		srv := newServer(store, instances, discardLogger())
		muxes[i].HandleFunc("GET /services", srv.handleList)
		muxes[i].HandleFunc("GET /services/{name}", srv.handleListService)
		muxes[i].HandleFunc("GET /services/{name}/{instance}", srv.handleGet)

		httpSrv := &http.Server{Addr: addrs[i], Handler: muxes[i], ReadHeaderTimeout: 5 * time.Second}
		ln, err := net.Listen("tcp", addrs[i])
		if err != nil {
			t.Fatalf("listen %s: %v", addrs[i], err)
		}
		go func() { _ = httpSrv.Serve(ln) }()
		t.Cleanup(func() { _ = httpSrv.Close() })
	}

	c.Start()
	return c, addrs
}

// getJSON reads one of the registry's own endpoints.
func getJSON(t *testing.T, url string, dst any) int {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode == http.StatusOK && dst != nil {
		if err := json.Unmarshal(body, dst); err != nil {
			t.Fatalf("GET %s: decode %s: %v", url, body, err)
		}
	}
	return resp.StatusCode
}

// registerInstance does what the agent does: its own lease, its own entry.
func registerInstance(t *testing.T, c *client.Client, service, id, addr string,
	ttl time.Duration,
) easyraft.LeaseID {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	lease, err := c.GrantLease(ctx, ttl)
	if err != nil {
		t.Fatalf("GrantLease: %v", err)
	}
	instances := client.Collection[registry.Instance](c, registry.CollectionName)
	if err := instances.UpsertWithLease(ctx, registry.Key(service, id), registry.Instance{
		Service: service, ID: id, Addr: addr,
	}, lease); err != nil {
		t.Fatalf("UpsertWithLease: %v", err)
	}
	return lease
}

type serviceList struct {
	Service   string              `json:"service"`
	Instances []registry.Instance `json:"instances"`
}

type fullList struct {
	Instances []registry.Instance `json:"instances"`
	Next      string              `json:"next"`
}

// waitInstances polls until a service lists the number of instances wanted.
func waitInstances(t *testing.T, url string, want int) []registry.Instance {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var last serviceList
	for time.Now().Before(deadline) {
		last = serviceList{}
		if getJSON(t, url, &last) == http.StatusOK && len(last.Instances) == want {
			return last.Instances
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s lists %d instances, want %d", url, len(last.Instances), want)
	return nil
}

// TestRegistry_AnInstanceOutlivesNothing is the example's whole claim: an
// entry that is there while its owner renews, and gone once it stops.
func TestRegistry_AnInstanceOutlivesNothing(t *testing.T) {
	_, addrs := startRegistry(t, 50*time.Millisecond)

	api, err := client.New(client.WithEndpoints(addrs...))
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// api-1 is the one under test: a short TTL, held alive by a loop. The
	// other two have hour-long leases and are here to show that what happens
	// to api-1 happens to api-1 alone -- an expiry that took the whole
	// collection with it would pass a test that only watched one key.
	held := registerInstance(t, api, "api", "api-1", "10.0.0.1:9000", 400*time.Millisecond)
	loopCtx, stopLoop := context.WithCancel(ctx)
	loopDone := make(chan error, 1)
	go func() { loopDone <- api.KeepAliveLoop(loopCtx, held) }()

	registerInstance(t, api, "api", "api-2", "10.0.0.2:9000", time.Hour)
	registerInstance(t, api, "worker", "w-1", "10.0.0.3:9000", time.Hour)

	// Every node answers, not just the leader: this is a local read of
	// replicated state.
	for _, addr := range addrs {
		found := waitInstances(t, "http://"+addr+"/services/api", 2)
		for _, instance := range found {
			if instance.Service != "api" {
				t.Errorf("%s returned an instance of %q under /services/api", addr, instance.Service)
			}
		}
	}

	// Three times its own TTL later, the renewed registration is still there.
	time.Sleep(1200 * time.Millisecond)
	waitInstances(t, "http://"+addrs[0]+"/services/api", 2)

	// Stop renewing and it goes -- and only it.
	stopLoop()
	select {
	case <-loopDone:
	case <-time.After(30 * time.Second):
		t.Fatal("KeepAliveLoop did not return after its context was cancelled")
	}

	remaining := waitInstances(t, "http://"+addrs[0]+"/services/api", 1)
	if remaining[0].ID != "api-2" {
		t.Errorf("the instance left behind is %q, want api-2", remaining[0].ID)
	}
	waitInstances(t, "http://"+addrs[0]+"/services/worker", 1)

	if status := getJSON(t, "http://"+addrs[0]+"/services/api/api-1", nil); status != http.StatusNotFound {
		t.Errorf("a departed instance answers %d, want 404", status)
	}

	// The lease is gone with it, so registering under it again is refused
	// rather than writing a key nothing would ever remove.
	instances := client.Collection[registry.Instance](api, registry.CollectionName)
	err = instances.UpsertWithLease(ctx, registry.Key("api", "api-1"),
		registry.Instance{Service: "api", ID: "api-1"}, held)
	if !errors.Is(err, easyraft.ErrLeaseNotFound) {
		t.Errorf("registering under the expired lease: %v, want ErrLeaseNotFound", err)
	}
}

// TestRegistry_RevokeRemovesItAtOnce covers the clean-shutdown path the agent
// takes, which should not wait out a TTL.
func TestRegistry_RevokeRemovesItAtOnce(t *testing.T) {
	_, addrs := startRegistry(t, 50*time.Millisecond)

	api, err := client.New(client.WithEndpoints(addrs...))
	if err != nil {
		t.Fatal(err)
	}
	lease := registerInstance(t, api, "api", "api-1", "10.0.0.1:9000", time.Hour)
	waitInstances(t, "http://"+addrs[0]+"/services/api", 1)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := api.RevokeLease(ctx, lease); err != nil {
		t.Fatalf("RevokeLease: %v", err)
	}
	waitInstances(t, "http://"+addrs[0]+"/services/api", 0)
}

// TestRegistry_ListsAndPages covers the two read shapes, including the cursor
// a caller carries between pages.
func TestRegistry_ListsAndPages(t *testing.T) {
	_, addrs := startRegistry(t, time.Second)

	api, err := client.New(client.WithEndpoints(addrs...))
	if err != nil {
		t.Fatal(err)
	}
	for i := range 7 {
		registerInstance(t, api, "api", fmt.Sprintf("api-%02d", i), "10.0.0.1:9000", time.Hour)
	}
	registerInstance(t, api, "worker", "w-1", "10.0.0.9:9000", time.Hour)

	var all fullList
	if status := getJSON(t, "http://"+addrs[0]+"/services", &all); status != http.StatusOK {
		t.Fatalf("GET /services: %d", status)
	}
	if len(all.Instances) != 8 {
		t.Errorf("the registry lists %d instances, want 8", len(all.Instances))
	}
	if all.Next != "" {
		t.Errorf("an unpaginated list returned the cursor %q", all.Next)
	}

	// Paged, following the cursor until it is empty.
	var walked []string
	next := ""
	for pages := 0; ; pages++ {
		if pages > 10 {
			t.Fatal("the paged listing did not end")
		}
		url := "http://" + addrs[0] + "/services?limit=3"
		if next != "" {
			url += "&after=" + next
		}
		var page fullList
		if status := getJSON(t, url, &page); status != http.StatusOK {
			t.Fatalf("GET %s: %d", url, status)
		}
		if len(page.Instances) > 3 {
			t.Fatalf("a page held %d instances under limit=3", len(page.Instances))
		}
		for _, instance := range page.Instances {
			walked = append(walked, registry.Key(instance.Service, instance.ID))
		}
		if page.Next == "" {
			break
		}
		next = page.Next
	}
	if len(walked) != 8 {
		t.Errorf("paging walked %d instances, want 8: %v", len(walked), walked)
	}

	// A limit that is not a number is refused rather than ignored.
	if status := getJSON(t, "http://"+addrs[0]+"/services?limit=lots", nil); status != http.StatusBadRequest {
		t.Errorf("limit=lots answered %d, want 400", status)
	}

	// And an unknown instance is a 404 rather than an empty object.
	if status := getJSON(t, "http://"+addrs[0]+"/services/api/nobody", nil); status != http.StatusNotFound {
		t.Errorf("an unregistered instance answers %d, want 404", status)
	}
}

// TestRegistry_ReRegisteringIsTheSameCall covers an agent coming back after a
// crash while its old entry is still inside the TTL.
func TestRegistry_ReRegisteringIsTheSameCall(t *testing.T) {
	_, addrs := startRegistry(t, time.Second)

	api, err := client.New(client.WithEndpoints(addrs...))
	if err != nil {
		t.Fatal(err)
	}
	registerInstance(t, api, "api", "api-1", "10.0.0.1:9000", time.Hour)
	waitInstances(t, "http://"+addrs[0]+"/services/api", 1)

	// Same instance, new address, new lease: an upsert, not a create, so it
	// does not fail on the entry it left behind.
	registerInstance(t, api, "api", "api-1", "10.0.0.99:9000", time.Hour)
	found := waitInstances(t, "http://"+addrs[0]+"/services/api", 1)
	if found[0].Addr != "10.0.0.99:9000" {
		t.Errorf("re-registering left the address at %q", found[0].Addr)
	}

	// The first lease no longer holds the key, so revoking it must not take
	// the new registration with it.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	leases, err := api.Leases(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, lease := range leases {
		if len(lease.Keys) == 0 {
			if err := api.RevokeLease(ctx, lease.ID); err != nil {
				t.Fatalf("RevokeLease: %v", err)
			}
		}
	}
	if got := len(waitInstances(t, "http://"+addrs[0]+"/services/api", 1)); got != 1 {
		t.Errorf("revoking the abandoned lease removed the live registration")
	}
	if _, err := api.Lease(ctx, 0); !errors.Is(err, easyraft.ErrLeaseNotFound) {
		t.Errorf("Lease(0): %v, want ErrLeaseNotFound", err)
	}
}
