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

func TestClient_CreateQuota(t *testing.T) {
	mux, url := newTestServer(t)

	mux.HandleFunc("POST /quotas/{key}", func(w http.ResponseWriter, r *http.Request) {
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}
		var q Quota
		if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
			t.Fatal(err)
		}
		if q.MaxTokens != 100 {
			t.Errorf("MaxTokens = %d, want 100", q.MaxTokens)
		}
		w.WriteHeader(http.StatusCreated)
	})

	c := New([]string{url})
	if err := c.CreateQuota(context.Background(), "user1", Quota{MaxTokens: 100}); err != nil {
		t.Fatal(err)
	}
}

func TestClient_CreateQuota_Conflict(t *testing.T) {
	mux, url := newTestServer(t)

	mux.HandleFunc("POST /quotas/{key}", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "key already exists", http.StatusConflict)
	})

	c := New([]string{url})
	err := c.CreateQuota(context.Background(), "user1", Quota{})
	if !errors.Is(err, ErrConflict) {
		t.Errorf("err = %v, want ErrConflict", err)
	}
}

func TestClient_SetQuota_Create(t *testing.T) {
	mux, url := newTestServer(t)

	// SetQuota tries POST first.
	mux.HandleFunc("POST /quotas/{key}", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
	})

	c := New([]string{url})
	if err := c.SetQuota(context.Background(), "user1", Quota{MaxTokens: 50}); err != nil {
		t.Fatal(err)
	}
}

func TestClient_SetQuota_Update(t *testing.T) {
	mux, url := newTestServer(t)

	// POST returns 409 — key already exists.
	mux.HandleFunc("POST /quotas/{key}", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "conflict", http.StatusConflict)
	})
	// SetQuota falls back to PUT.
	var putCalled bool
	mux.HandleFunc("PUT /quotas/{key}", func(w http.ResponseWriter, r *http.Request) {
		putCalled = true
		w.WriteHeader(http.StatusNoContent)
	})

	c := New([]string{url})
	if err := c.SetQuota(context.Background(), "user1", Quota{MaxTokens: 50}); err != nil {
		t.Fatal(err)
	}
	if !putCalled {
		t.Error("expected PUT to be called as fallback")
	}
}

func TestClient_GetQuota(t *testing.T) {
	mux, url := newTestServer(t)

	mux.HandleFunc("GET /quotas/{key}", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(Quota{MaxTokens: 100, CurrentTokens: 50})
	})

	c := New([]string{url})
	q, err := c.GetQuota(context.Background(), "user1", false)
	if err != nil {
		t.Fatal(err)
	}
	if q.CurrentTokens != 50 {
		t.Errorf("CurrentTokens = %d, want 50", q.CurrentTokens)
	}
}

func TestClient_GetQuota_NotFound(t *testing.T) {
	mux, url := newTestServer(t)

	mux.HandleFunc("GET /quotas/{key}", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	})

	c := New([]string{url})
	_, err := c.GetQuota(context.Background(), "nobody", false)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestClient_DeleteQuota(t *testing.T) {
	mux, url := newTestServer(t)

	mux.HandleFunc("DELETE /quotas/{key}", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	c := New([]string{url})
	if err := c.DeleteQuota(context.Background(), "user1"); err != nil {
		t.Fatal(err)
	}
}

func TestClient_Take(t *testing.T) {
	mux, url := newTestServer(t)

	mux.HandleFunc("POST /quotas/{key}/mutate", func(w http.ResponseWriter, r *http.Request) {
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}
		var req struct {
			Name string `json:"name"`
			Args struct {
				Requested int64 `json:"requested"`
				Now       int64 `json:"now"`
			} `json:"args"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		if req.Name != "take" || req.Args.Requested != 5 {
			t.Errorf("unexpected request: %+v", req)
		}
		if req.Args.Now <= 0 {
			t.Error("Now must be a positive Unix timestamp")
		}
		_ = json.NewEncoder(w).Encode(TakeResponse{Allowed: true, Remains: 45})
	})

	c := New([]string{url})
	resp, err := c.Take(context.Background(), "user1", 5)
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Allowed || resp.Remains != 45 {
		t.Errorf("unexpected response: %+v", resp)
	}
}

func TestClient_Redirect_UpdatesLeaderCache(t *testing.T) {
	// server2 is the actual leader.
	mux2, url2 := newTestServer(t)
	var leaderCalled int
	mux2.HandleFunc("POST /quotas/{key}", func(w http.ResponseWriter, r *http.Request) {
		leaderCalled++
		w.WriteHeader(http.StatusCreated)
	})

	// server1 is a follower that redirects to the leader.
	mux1, url1 := newTestServer(t)
	mux1.HandleFunc("POST /quotas/{key}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, url2+r.URL.Path, http.StatusTemporaryRedirect)
	})

	c := New([]string{url1})

	// First call hits follower, follows redirect, caches leader.
	if err := c.CreateQuota(context.Background(), "u1", Quota{}); err != nil {
		t.Fatal(err)
	}
	if leaderCalled != 1 {
		t.Errorf("leaderCalled = %d, want 1", leaderCalled)
	}

	if cached := c.inner.GetLeader(0); cached != url2 {
		t.Errorf("leaderAddr = %q, want %q", cached, url2)
	}

	// Second call goes straight to the cached leader.
	leaderCalled = 0
	if err := c.CreateQuota(context.Background(), "u2", Quota{}); err != nil {
		t.Fatal(err)
	}
	if leaderCalled != 1 {
		t.Errorf("second call: leaderCalled = %d, want 1", leaderCalled)
	}
}

func TestClient_StaleLeader_ClearedOnFailure(t *testing.T) {
	mux, url := newTestServer(t)
	mux.HandleFunc("GET /quotas/{key}", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(Quota{MaxTokens: 10})
	})

	c := New([]string{url})
	c.inner.SetLeader(0, "http://127.0.0.1:0") // guaranteed dead

	q, err := c.GetQuota(context.Background(), "u1", false)
	if err != nil {
		t.Fatal(err)
	}
	if q.MaxTokens != 10 {
		t.Errorf("MaxTokens = %d, want 10", q.MaxTokens)
	}

	if c.inner.GetLeader(0) == "http://127.0.0.1:0" {
		t.Error("stale leader address was not cleared after failure")
	}
}

func TestClient_ServerError_IncludesBody(t *testing.T) {
	mux, url := newTestServer(t)

	mux.HandleFunc("POST /quotas/{key}/mutate", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "mutation panic: nil pointer", http.StatusInternalServerError)
	})

	c := New([]string{url})
	_, err := c.Take(context.Background(), "u1", 1)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "mutation panic: nil pointer") {
		t.Errorf("error %q does not contain server message", err.Error())
	}
}

func TestClient_ContextCancelled_StopsRetries(t *testing.T) {
	c := New([]string{
		"http://127.0.0.1:0",
		"http://127.0.0.1:0",
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := c.GetQuota(ctx, "u1", false)
	if err == nil {
		t.Fatal("expected error with cancelled context")
	}
}
