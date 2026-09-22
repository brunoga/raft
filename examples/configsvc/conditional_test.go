package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/brunoga/raft/v2/easyraft"
	"github.com/brunoga/raft/v2/easyraft/easyrafttest"
)

// startConfigsvc brings up a three-node configsvc serving this example's own
// routes, and returns the HTTP addresses.
//
// The Raft traffic runs on easyrafttest's in-memory network so an election
// takes milliseconds; the HTTP is real, because that is the part these tests
// are about.
func startConfigsvc(t *testing.T) (addrs []string) {
	t.Helper()

	addrs = make([]string, 3)
	muxes := make([]*http.ServeMux, 3)
	for i := range addrs {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addrs[i] = ln.Addr().String()
		if closeErr := ln.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
		muxes[i] = http.NewServeMux()
	}

	c := easyrafttest.New(t, 3, easyrafttest.Options{
		Store: []easyraft.Option{easyraft.WithInsecureHTTPAcknowledged()},
		PerNode: func(i int) []easyraft.Option {
			return []easyraft.Option{
				easyraft.WithHTTPAddr(addrs[i]),
				easyraft.WithHTTPMux(muxes[i]),
			}
		},
	})

	for i, store := range c.Stores {
		configs := easyraft.AddCollection[ConfigEntry](store, "configs")
		srv := newServer(store, configs)
		muxes[i].HandleFunc("PUT /configs/{key}", srv.handleSet)
		muxes[i].HandleFunc("GET /configs/{key}", srv.handleGet)
		muxes[i].HandleFunc("DELETE /configs/{key}", srv.handleDelete)
		muxes[i].HandleFunc("GET /configs", srv.handleList)

		httpSrv := &http.Server{Handler: muxes[i], ReadHeaderTimeout: 5 * time.Second}
		ln, err := net.Listen("tcp", addrs[i])
		if err != nil {
			t.Fatalf("listen %s: %v", addrs[i], err)
		}
		go func() { _ = httpSrv.Serve(ln) }()
		t.Cleanup(func() { _ = httpSrv.Close() })
	}

	c.Start()
	return addrs
}

// call makes one request against the leader, following the redirect a
// follower answers a write with.
func call(t *testing.T, method, url, body string, header map[string]string) (status int, resp, etag string) {
	t.Helper()
	var reader io.Reader = http.NoBody
	if body != "" {
		reader = bytes.NewReader([]byte(body))
	}
	req, err := http.NewRequestWithContext(context.Background(), method, url, reader)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range header {
		req.Header.Set(k, v)
	}
	// 307 preserves the method and the body, so a write that reached a
	// follower lands on the leader without the caller rebuilding it.
	httpResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer func() { _ = httpResp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(httpResp.Body, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	return httpResp.StatusCode, string(raw), httpResp.Header.Get("ETag")
}

// waitSet writes a key, retrying while the cluster is still electing.
func waitSet(t *testing.T, addr, key, value string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		status, _, _ := call(t, http.MethodPut, "http://"+addr+"/configs/"+key,
			`{"value":"`+value+`"}`, nil)
		if status == http.StatusNoContent {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("could not set %s on %s", key, addr)
}

// TestConditional_CompareAndSwap covers the loop the README describes: read a
// value with its ETag, write it back only if nothing moved in between.
func TestConditional_CompareAndSwap(t *testing.T) {
	addrs := startConfigsvc(t)
	base := "http://" + addrs[0]

	waitSet(t, addrs[0], "db.host", "localhost")

	status, body, etag := call(t, http.MethodGet, base+"/configs/db.host", "", nil)
	if status != http.StatusOK {
		t.Fatalf("GET: %d %s", status, body)
	}
	if etag == "" {
		t.Fatal("GET returned no ETag")
	}
	if _, err := strconv.ParseUint(etag, 10, 64); err != nil {
		t.Fatalf("ETag %q is not a revision: %v", etag, err)
	}

	// The ETag just read applies.
	if status, body, _ := call(t, http.MethodPut, base+"/configs/db.host",
		`{"value":"db.internal"}`, map[string]string{"If-Match": etag}); status != http.StatusNoContent {
		t.Fatalf("conditional PUT with a current ETag: %d %s", status, body)
	}

	// The same one is now stale, and the write is refused rather than
	// overwriting what the first one wrote.
	if status, _, _ := call(t, http.MethodPut, base+"/configs/db.host",
		`{"value":"wrong"}`, map[string]string{"If-Match": etag}); status != http.StatusPreconditionFailed {
		t.Fatalf("conditional PUT with a stale ETag: %d, want 412", status)
	}

	var entry ConfigEntry
	_, after, _ := call(t, http.MethodGet, base+"/configs/db.host", "", nil)
	if err := json.Unmarshal([]byte(after), &entry); err != nil {
		t.Fatal(err)
	}
	if entry.Value != "db.internal" {
		t.Errorf("the refused write left %q behind", entry.Value)
	}

	// A quoted ETag is what an HTTP client is most likely to send back.
	_, _, current := call(t, http.MethodGet, base+"/configs/db.host", "", nil)
	if status, body, _ := call(t, http.MethodPut, base+"/configs/db.host",
		`{"value":"quoted"}`, map[string]string{"If-Match": `"` + current + `"`}); status != http.StatusNoContent {
		t.Fatalf("conditional PUT with a quoted ETag: %d %s", status, body)
	}
}

// TestConditional_CreateIfAbsent covers If-None-Match, which is how a caller
// claims a key without overwriting whoever got there first.
func TestConditional_CreateIfAbsent(t *testing.T) {
	addrs := startConfigsvc(t)
	base := "http://" + addrs[0]

	waitSet(t, addrs[0], "warmup", "x")

	create := map[string]string{"If-None-Match": "*"}
	if status, body, _ := call(t, http.MethodPut, base+"/configs/owner",
		`{"value":"first"}`, create); status != http.StatusNoContent {
		t.Fatalf("create-if-absent on a free key: %d %s", status, body)
	}
	if status, _, _ := call(t, http.MethodPut, base+"/configs/owner",
		`{"value":"second"}`, create); status != http.StatusPreconditionFailed {
		t.Fatalf("create-if-absent on a taken key: %d, want 412", status)
	}

	var entry ConfigEntry
	_, body, _ := call(t, http.MethodGet, base+"/configs/owner", "", nil)
	if err := json.Unmarshal([]byte(body), &entry); err != nil {
		t.Fatal(err)
	}
	if entry.Value != "first" {
		t.Errorf("the second claim won: %q", entry.Value)
	}
}

// TestConditional_DeleteAndBadHeaders covers the rest of the surface: a
// conditional delete, and every malformed condition refused rather than
// silently applied without one.
func TestConditional_DeleteAndBadHeaders(t *testing.T) {
	addrs := startConfigsvc(t)
	base := "http://" + addrs[0]

	waitSet(t, addrs[0], "doomed", "value")
	_, _, etag := call(t, http.MethodGet, base+"/configs/doomed", "", nil)

	stale := strconv.FormatUint(mustUint(t, etag)-1, 10)
	if status, _, _ := call(t, http.MethodDelete, base+"/configs/doomed", "",
		map[string]string{"If-Match": stale}); status != http.StatusPreconditionFailed {
		t.Errorf("conditional DELETE with a stale ETag: %d, want 412", status)
	}
	if status, body, _ := call(t, http.MethodDelete, base+"/configs/doomed", "",
		map[string]string{"If-Match": etag}); status != http.StatusNoContent {
		t.Fatalf("conditional DELETE with a current ETag: %d %s", status, body)
	}
	if status, _, _ := call(t, http.MethodGet, base+"/configs/doomed", "", nil); status != http.StatusNotFound {
		t.Errorf("the key survived a matching conditional DELETE: %d", status)
	}

	waitSet(t, addrs[0], "guarded", "value")
	for _, bad := range []map[string]string{
		{"If-Match": "not-a-number"},
		{"If-None-Match": `"7"`},
		{"If-Match": "1", "If-None-Match": "*"},
	} {
		if status, body, _ := call(t, http.MethodPut, base+"/configs/guarded",
			`{"value":"whatever"}`, bad); status != http.StatusBadRequest {
			t.Errorf("PUT with %v: %d %s, want 400", bad, status, body)
		}
	}
	// None of those were applied.
	var entry ConfigEntry
	_, body, _ := call(t, http.MethodGet, base+"/configs/guarded", "", nil)
	if err := json.Unmarshal([]byte(body), &entry); err != nil {
		t.Fatal(err)
	}
	if entry.Value != "value" {
		t.Errorf("a refused condition was applied anyway: %q", entry.Value)
	}
}

// TestConditional_ConcurrentWritersLoseNothing is the property the whole
// thing is for. Several writers run read-modify-write on one key with no
// lock; each either lands or is told to retry, so every increment survives.
//
// Without the condition this is the classic lost update: both read 4, both
// write 5, and one increment is gone with nothing having reported an error.
func TestConditional_ConcurrentWritersLoseNothing(t *testing.T) {
	addrs := startConfigsvc(t)
	base := "http://" + addrs[0]

	waitSet(t, addrs[0], "counter", "0")

	const writers, each = 4, 10
	var wg sync.WaitGroup
	for range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range each {
				for {
					status, body, etag := call(t, http.MethodGet, base+"/configs/counter", "", nil)
					if status != http.StatusOK {
						t.Errorf("GET: %d %s", status, body)
						return
					}
					var entry ConfigEntry
					if err := json.Unmarshal([]byte(body), &entry); err != nil {
						t.Errorf("decode: %v", err)
						return
					}
					n, err := strconv.Atoi(entry.Value)
					if err != nil {
						t.Errorf("counter is %q: %v", entry.Value, err)
						return
					}

					next := strconv.Itoa(n + 1)
					status, body, _ = call(t, http.MethodPut, base+"/configs/counter",
						`{"value":"`+next+`"}`, map[string]string{"If-Match": etag})
					if status == http.StatusNoContent {
						break
					}
					if status != http.StatusPreconditionFailed {
						t.Errorf("conditional PUT: %d %s", status, body)
						return
					}
				}
			}
		}()
	}
	wg.Wait()

	var entry ConfigEntry
	_, body, _ := call(t, http.MethodGet, base+"/configs/counter", "", nil)
	if err := json.Unmarshal([]byte(body), &entry); err != nil {
		t.Fatal(err)
	}
	if entry.Value != strconv.Itoa(writers*each) {
		t.Errorf("the counter is %q after %d increments; a write was lost",
			entry.Value, writers*each)
	}
}

func mustUint(t *testing.T, s string) uint64 {
	t.Helper()
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return v
}
