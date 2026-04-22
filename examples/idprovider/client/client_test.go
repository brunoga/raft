package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newTestServer registers handlers on a fresh ServeMux and returns both the
// mux (for adding handlers) and the test server URL.
func newTestServer(t *testing.T) (*http.ServeMux, string) {
	t.Helper()
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return mux, srv.URL
}

func TestClient_CreateDomain(t *testing.T) {
	mux, url := newTestServer(t)

	mux.HandleFunc("POST /domains/{name}", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Client-ID") != "test-client" {
			t.Errorf("X-Client-ID = %q, want test-client", r.Header.Get("X-Client-ID"))
		}
		if r.Header.Get("X-Seq-Num") == "" {
			t.Error("X-Seq-Num header missing")
		}
		w.WriteHeader(http.StatusOK)
	})

	c := New([]string{url}, "test-client")
	if err := c.CreateDomain(context.Background(), "orders"); err != nil {
		t.Fatal(err)
	}
}

func TestClient_DeleteDomain_NotFound(t *testing.T) {
	mux, url := newTestServer(t)

	mux.HandleFunc("DELETE /domains/{name}", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "domain not found", http.StatusNotFound)
	})

	c := New([]string{url}, "test-client")
	err := c.DeleteDomain(context.Background(), "missing")
	if !errors.Is(err, ErrDomainNotFound) {
		t.Errorf("err = %v, want ErrDomainNotFound", err)
	}
}

func TestClient_ListDomains(t *testing.T) {
	mux, url := newTestServer(t)

	mux.HandleFunc("GET /domains", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]uint64{"orders": 100, "users": 50})
	})

	c := New([]string{url}, "test-client")
	domains, err := c.ListDomains(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(domains) != 2 {
		t.Errorf("got %d domains, want 2", len(domains))
	}
	if domains["orders"] != 100 {
		t.Errorf("orders = %d, want 100", domains["orders"])
	}
}

func TestClient_Next(t *testing.T) {
	mux, url := newTestServer(t)

	mux.HandleFunc("POST /domains/{name}/next", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("count") != "10" {
			t.Errorf("count = %q, want 10", r.URL.Query().Get("count"))
		}
		if r.Header.Get("X-Client-ID") != "test-client" {
			t.Error("missing idempotency headers")
		}
		_ = json.NewEncoder(w).Encode(AllocResult{Start: 100, Count: 10})
	})

	c := New([]string{url}, "test-client")
	res, err := c.Next(context.Background(), "orders", 10)
	if err != nil {
		t.Fatal(err)
	}
	if res.Start != 100 || res.Count != 10 {
		t.Errorf("unexpected result: %+v", res)
	}
}

func TestClient_Next_DomainNotFound(t *testing.T) {
	mux, url := newTestServer(t)

	mux.HandleFunc("POST /domains/{name}/next", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "domain not found", http.StatusNotFound)
	})

	c := New([]string{url}, "test-client")
	_, err := c.Next(context.Background(), "missing", 1)
	if !errors.Is(err, ErrDomainNotFound) {
		t.Errorf("err = %v, want ErrDomainNotFound", err)
	}
}

func TestClient_Current(t *testing.T) {
	mux, url := newTestServer(t)

	mux.HandleFunc("GET /domains/{name}/current", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(DomainInfo{Domain: "orders", Current: 110})
	})

	c := New([]string{url}, "test-client")
	info, err := c.Current(context.Background(), "orders", false)
	if err != nil {
		t.Fatal(err)
	}
	if info.Current != 110 {
		t.Errorf("Current = %d, want 110", info.Current)
	}
}

func TestClient_Idempotency_DistinctSeqPerCall(t *testing.T) {
	var seqs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seqs = append(seqs, r.Header.Get("X-Seq-Num"))
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New([]string{srv.URL}, "test-client")
	_ = c.CreateDomain(context.Background(), "d1")
	_ = c.CreateDomain(context.Background(), "d2")

	if len(seqs) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(seqs))
	}
	if seqs[0] == seqs[1] {
		t.Errorf("expected distinct seq numbers, got %v and %v", seqs[0], seqs[1])
	}
}

func TestClient_Redirect_UpdatesLeaderCache(t *testing.T) {
	// server2 is the actual leader.
	mux2, url2 := newTestServer(t)
	var leaderCalled int
	mux2.HandleFunc("POST /domains/{name}", func(w http.ResponseWriter, r *http.Request) {
		leaderCalled++
		w.WriteHeader(http.StatusOK)
	})

	// server1 is a follower that redirects.
	mux1, url1 := newTestServer(t)
	mux1.HandleFunc("POST /domains/{name}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, url2+r.URL.Path, http.StatusTemporaryRedirect)
	})

	c := New([]string{url1}, "test-client")

	if err := c.CreateDomain(context.Background(), "d1"); err != nil {
		t.Fatal(err)
	}
	if leaderCalled != 1 {
		t.Errorf("leaderCalled = %d, want 1", leaderCalled)
	}

	if cached := c.inner.GetLeader(0); cached != url2 {
		t.Errorf("leaderAddr = %q, want %q", cached, url2)
	}

	// Second call hits leader directly.
	leaderCalled = 0
	if err := c.CreateDomain(context.Background(), "d2"); err != nil {
		t.Fatal(err)
	}
	if leaderCalled != 1 {
		t.Errorf("second call: leaderCalled = %d, want 1", leaderCalled)
	}
}

func TestClient_StaleLeader_ClearedOnFailure(t *testing.T) {
	mux, url := newTestServer(t)
	mux.HandleFunc("GET /domains", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]uint64{"d": 1})
	})

	c := New([]string{url}, "test-client")
	c.inner.SetLeader(0, "http://127.0.0.1:0") // guaranteed dead

	_, err := c.ListDomains(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}

	if c.inner.GetLeader(0) == "http://127.0.0.1:0" {
		t.Error("stale leader address was not cleared after failure")
	}
}

func TestClient_ServerError_IncludesBody(t *testing.T) {
	mux, url := newTestServer(t)

	mux.HandleFunc("POST /domains/{name}/next", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "counter overflow for this domain", http.StatusInternalServerError)
	})

	c := New([]string{url}, "test-client")
	_, err := c.Next(context.Background(), "d", 1)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "counter overflow for this domain") {
		t.Errorf("error %q does not contain server message", err.Error())
	}
}

func TestClient_ContextCancelled_StopsRetries(t *testing.T) {
	c := New([]string{
		"http://127.0.0.1:0",
		"http://127.0.0.1:0",
	}, "test-client")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := c.ListDomains(ctx, false)
	if err == nil {
		t.Fatal("expected error with cancelled context")
	}
}
