package client

import (
	"context"
	"encoding/json"
	"errors"
	"io"
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

func TestClient_Put(t *testing.T) {
	mux, url := newTestServer(t)

	mux.HandleFunc("PUT /keys/{key}", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if string(body) != "myvalue" {
			t.Errorf("body = %q, want myvalue", string(body))
		}
		w.WriteHeader(http.StatusNoContent)
	})

	c := New([]string{url}, 4)
	if err := c.Put(context.Background(), "mykey", "myvalue"); err != nil {
		t.Fatal(err)
	}
}

func TestClient_Get(t *testing.T) {
	mux, url := newTestServer(t)

	mux.HandleFunc("GET /keys/{key}", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("myvalue"))
	})

	c := New([]string{url}, 4)
	val, err := c.Get(context.Background(), "mykey")
	if err != nil {
		t.Fatal(err)
	}
	if val != "myvalue" {
		t.Errorf("val = %q, want myvalue", val)
	}
}

func TestClient_Get_NotFound(t *testing.T) {
	mux, url := newTestServer(t)

	mux.HandleFunc("GET /keys/{key}", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "key not found", http.StatusNotFound)
	})

	c := New([]string{url}, 4)
	_, err := c.Get(context.Background(), "missing")
	if !errors.Is(err, ErrKeyNotFound) {
		t.Errorf("err = %v, want ErrKeyNotFound", err)
	}
}

func TestClient_Delete(t *testing.T) {
	mux, url := newTestServer(t)

	mux.HandleFunc("DELETE /keys/{key}", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	c := New([]string{url}, 4)
	if err := c.Delete(context.Background(), "mykey"); err != nil {
		t.Fatal(err)
	}
}

func TestClient_Shards(t *testing.T) {
	mux, url := newTestServer(t)

	mux.HandleFunc("GET /shards", func(w http.ResponseWriter, r *http.Request) {
		shards := []ShardStatus{
			{GroupID: 1, State: "leader"},
			{GroupID: 2, State: "follower"},
		}
		writeJSON(w, shards)
	})

	c := New([]string{url}, 4)
	shards, err := c.Shards(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	if len(shards) != 2 {
		t.Errorf("got %d shards, want 2", len(shards))
	}
}

func TestClient_ShardRouting_Consistent(t *testing.T) {
	c := New(nil, 4)

	// Same key must always map to the same shard.
	g1 := c.shardForKey("alpha")
	if c.shardForKey("alpha") != g1 {
		t.Error("shard routing is not consistent")
	}

	// Shard IDs must be in [1, numShards].
	for _, key := range []string{"a", "b", "c", "d", "hello", "world"} {
		gid := c.shardForKey(key)
		if gid < 1 || gid > 4 {
			t.Errorf("shardForKey(%q) = %d, out of [1,4]", key, gid)
		}
	}
}

func TestClient_Redirect_UpdatesLeaderCache(t *testing.T) {
	// server2 is the shard leader.
	mux2, url2 := newTestServer(t)
	var leaderCalled int
	mux2.HandleFunc("PUT /keys/{key}", func(w http.ResponseWriter, r *http.Request) {
		leaderCalled++
		w.WriteHeader(http.StatusNoContent)
	})

	// server1 is a follower that redirects.
	mux1, url1 := newTestServer(t)
	mux1.HandleFunc("PUT /keys/{key}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, url2+r.URL.Path, http.StatusTemporaryRedirect)
	})

	c := New([]string{url1}, 4)
	gid := c.shardForKey("mykey")

	if err := c.Put(context.Background(), "mykey", "v"); err != nil {
		t.Fatal(err)
	}
	if leaderCalled != 1 {
		t.Errorf("leaderCalled = %d, want 1", leaderCalled)
	}

	c.mu.RLock()
	cached := c.leaderAddrs[gid]
	c.mu.RUnlock()
	if cached != url2 {
		t.Errorf("leaderAddrs[%d] = %q, want %q", gid, cached, url2)
	}

	// Second call hits leader directly.
	leaderCalled = 0
	if err := c.Put(context.Background(), "mykey", "v2"); err != nil {
		t.Fatal(err)
	}
	if leaderCalled != 1 {
		t.Errorf("second call: leaderCalled = %d, want 1", leaderCalled)
	}
}

func TestClient_StaleLeader_ClearedOnFailure(t *testing.T) {
	mux, url := newTestServer(t)
	mux.HandleFunc("GET /keys/{key}", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("val"))
	})

	c := New([]string{url}, 4)
	gid := c.shardForKey("mykey")

	c.mu.Lock()
	c.leaderAddrs[gid] = "http://127.0.0.1:0" // guaranteed dead
	c.mu.Unlock()

	val, err := c.Get(context.Background(), "mykey")
	if err != nil {
		t.Fatal(err)
	}
	if val != "val" {
		t.Errorf("val = %q, want val", val)
	}

	c.mu.RLock()
	_, still := c.leaderAddrs[gid]
	c.mu.RUnlock()
	if still && c.leaderAddrs[gid] == "http://127.0.0.1:0" {
		t.Error("stale leader address was not cleared after failure")
	}
}

func TestClient_ServerError_IncludesBody(t *testing.T) {
	mux, url := newTestServer(t)

	mux.HandleFunc("PUT /keys/{key}", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "shard storage error", http.StatusInternalServerError)
	})

	c := New([]string{url}, 4)
	err := c.Put(context.Background(), "mykey", "v")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "shard storage error") {
		t.Errorf("error %q does not contain server message", err.Error())
	}
}

func TestClient_ContextCancelled_StopsRetries(t *testing.T) {
	c := New([]string{
		"http://127.0.0.1:0",
		"http://127.0.0.1:0",
	}, 4)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := c.Get(ctx, "mykey")
	if err == nil {
		t.Fatal("expected error with cancelled context")
	}
}

// writeJSON is a test helper that encodes v as JSON to w.
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
