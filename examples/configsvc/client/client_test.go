package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newTestServer registers handlers on a fresh ServeMux and returns both the
// mux (for adding handlers) and the test server URL.
func newTestServer(t *testing.T) (mux *http.ServeMux, baseURL string) {
	t.Helper()
	mux = http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return mux, srv.URL
}

func TestClient_Set(t *testing.T) {
	mux, url := newTestServer(t)

	mux.HandleFunc("PUT /configs/{key}", func(w http.ResponseWriter, r *http.Request) {
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}
		if r.PathValue("key") != "mykey" {
			t.Errorf("key = %q, want mykey", r.PathValue("key"))
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["value"] != "myvalue" {
			t.Errorf("value = %q, want myvalue", body["value"])
		}
		w.WriteHeader(http.StatusNoContent)
	})

	c := New([]string{url})
	if err := c.Set(context.Background(), "mykey", "myvalue"); err != nil {
		t.Fatal(err)
	}
}

func TestClient_Get(t *testing.T) {
	mux, url := newTestServer(t)

	mux.HandleFunc("GET /configs/{key}", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(ConfigEntry{Value: "myvalue", Version: 12345})
	})

	c := New([]string{url})
	entry, err := c.Get(context.Background(), "mykey", false)
	if err != nil {
		t.Fatal(err)
	}
	if entry.Value != "myvalue" || entry.Version != 12345 {
		t.Errorf("unexpected entry: %+v", entry)
	}
}

func TestClient_Get_NotFound(t *testing.T) {
	mux, url := newTestServer(t)

	mux.HandleFunc("GET /configs/{key}", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	})

	c := New([]string{url})
	_, err := c.Get(context.Background(), "missing", false)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestClient_Delete(t *testing.T) {
	mux, url := newTestServer(t)

	mux.HandleFunc("DELETE /configs/{key}", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	c := New([]string{url})
	if err := c.Delete(context.Background(), "mykey"); err != nil {
		t.Fatal(err)
	}
}

func TestClient_List(t *testing.T) {
	mux, url := newTestServer(t)

	mux.HandleFunc("GET /configs", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]ConfigEntry{
			"k1": {Value: "v1", Version: 1},
			"k2": {Value: "v2", Version: 2},
		})
	})

	c := New([]string{url})
	all, err := c.List(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Errorf("got %d entries, want 2", len(all))
	}
}

func TestClient_Watch(t *testing.T) {
	mux, url := newTestServer(t)

	mux.HandleFunc("GET /watch/{key}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		flusher := w.(http.Flusher)

		// Write errors here mean the client hung up, which the test detects
		// through the client side of the stream.
		_, _ = fmt.Fprintf(w, "event: snapshot\ndata: %s\n\n",
			`{"key":"mykey","value":{"value":"init","version":1}}`)
		flusher.Flush()

		_, _ = fmt.Fprintf(w, "event: change\ndata: %s\n\n",
			`{"key":"mykey","value":{"value":"updated","version":2}}`)
		flusher.Flush()

		<-r.Context().Done()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	c := New([]string{url})
	ch, err := c.Watch(ctx, "mykey")
	if err != nil {
		t.Fatal(err)
	}

	// Snapshot event.
	select {
	case ev := <-ch:
		if ev.Type != "snapshot" || ev.Key != "mykey" || ev.Value == nil || ev.Value.Value != "init" {
			t.Errorf("unexpected snapshot: %+v", ev)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timeout waiting for snapshot")
	}

	// Change event.
	select {
	case ev := <-ch:
		if ev.Type != "change" || ev.Key != "mykey" || ev.Value == nil || ev.Value.Value != "updated" {
			t.Errorf("unexpected change: %+v", ev)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timeout waiting for change")
	}
}

func TestClient_Redirect_UpdatesLeaderCache(t *testing.T) {
	// server2 is the actual leader.
	mux2, url2 := newTestServer(t)
	var leaderCalled int
	mux2.HandleFunc("PUT /configs/{key}", func(w http.ResponseWriter, r *http.Request) {
		leaderCalled++
		w.WriteHeader(http.StatusNoContent)
	})

	// server1 is a follower that redirects to the leader.
	mux1, url1 := newTestServer(t)
	mux1.HandleFunc("PUT /configs/{key}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, url2+r.URL.Path, http.StatusTemporaryRedirect)
	})

	c := New([]string{url1})

	// First call hits follower, follows redirect, caches leader.
	if err := c.Set(context.Background(), "k", "v"); err != nil {
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
	if err := c.Set(context.Background(), "k2", "v2"); err != nil {
		t.Fatal(err)
	}
	if leaderCalled != 1 {
		t.Errorf("second call: leaderCalled = %d, want 1", leaderCalled)
	}
}

func TestClient_StaleLeader_ClearedOnFailure(t *testing.T) {
	mux, url := newTestServer(t)
	mux.HandleFunc("GET /configs/{key}", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(ConfigEntry{Value: "v", Version: 1})
	})

	c := New([]string{url})
	c.inner.SetLeader(0, "http://127.0.0.1:0") // guaranteed dead

	_, err := c.Get(context.Background(), "k", false)
	if err != nil {
		t.Fatal(err)
	}

	if c.inner.GetLeader(0) == "http://127.0.0.1:0" {
		t.Error("stale leader address was not cleared after failure")
	}
}

func TestClient_ServerError_IncludesBody(t *testing.T) {
	mux, url := newTestServer(t)

	mux.HandleFunc("PUT /configs/{key}", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "disk full on this node", http.StatusInternalServerError)
	})

	c := New([]string{url})
	err := c.Set(context.Background(), "k", "v")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "disk full on this node") {
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

	_, err := c.Get(ctx, "k", false)
	if err == nil {
		t.Fatal("expected error with cancelled context")
	}
}

// TestClient_GetRev reads the revision out of the ETag, which is what a
// conditional write has to name.
func TestClient_GetRev(t *testing.T) {
	mux, baseURL := newTestServer(t)
	mux.HandleFunc("GET /configs/{key}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", "412")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ConfigEntry{Value: "localhost", Version: 7})
	})

	c := New([]string{baseURL})
	entry, rev, err := c.GetRev(context.Background(), "db.host", false)
	if err != nil {
		t.Fatalf("GetRev: %v", err)
	}
	if entry.Value != "localhost" {
		t.Errorf("GetRev returned %+v", entry)
	}
	if rev != 412 {
		t.Errorf("GetRev returned revision %d, want the ETag's 412", rev)
	}

	// Get is GetRev without the revision, so it must still work.
	if got, getErr := c.Get(context.Background(), "db.host", false); getErr != nil || got.Value != "localhost" {
		t.Errorf("Get returned %+v, %v", got, getErr)
	}
}

// TestClient_GetRev_QuotedAndMissingETag covers the two shapes a server can
// answer with that are not a bare number.
func TestClient_GetRev_QuotedAndMissingETag(t *testing.T) {
	mux, baseURL := newTestServer(t)
	mux.HandleFunc("GET /configs/{key}", func(w http.ResponseWriter, r *http.Request) {
		switch r.PathValue("key") {
		case "quoted":
			w.Header().Set("ETag", `"9"`)
		case "unusable":
			w.Header().Set("ETag", "not-a-number")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ConfigEntry{Value: "v"})
	})

	c := New([]string{baseURL})
	if _, rev, err := c.GetRev(context.Background(), "quoted", false); err != nil || rev != 9 {
		t.Errorf("a quoted ETag gave revision %d, %v", rev, err)
	}
	// No ETag at all is revision zero rather than an error: zero is the
	// revision of a key that has never been written, and a conditional write
	// naming it is refused rather than misapplied.
	if _, rev, err := c.GetRev(context.Background(), "none", false); err != nil || rev != 0 {
		t.Errorf("a missing ETag gave revision %d, %v", rev, err)
	}
	// An ETag that is not a revision is an error, because silently treating
	// it as zero would turn a conditional write into a create-if-absent.
	if _, _, err := c.GetRev(context.Background(), "unusable", false); err == nil {
		t.Error("an unusable ETag was accepted")
	}
}

// TestClient_SetIf sends the condition and reports a refused one as
// ErrRevisionMismatch rather than retrying it.
func TestClient_SetIf(t *testing.T) {
	mux, baseURL := newTestServer(t)
	var attempts int
	mux.HandleFunc("PUT /configs/{key}", func(w http.ResponseWriter, r *http.Request) {
		attempts++
		switch {
		case r.Header.Get("If-Match") == "7":
			w.WriteHeader(http.StatusNoContent)
		case r.Header.Get("If-None-Match") == "*":
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusPreconditionFailed)
		}
	})

	c := New([]string{baseURL})
	ctx := context.Background()

	if err := c.SetIf(ctx, "db.host", "localhost", 7); err != nil {
		t.Fatalf("SetIf on a matching revision: %v", err)
	}
	// Zero is create-if-absent, which travels as If-None-Match.
	if err := c.SetIf(ctx, "db.host", "localhost", 0); err != nil {
		t.Fatalf("SetIf(rev 0): %v", err)
	}

	attempts = 0
	err := c.SetIf(ctx, "db.host", "localhost", 99)
	if !errors.Is(err, ErrRevisionMismatch) {
		t.Fatalf("SetIf on a stale revision: %v, want ErrRevisionMismatch", err)
	}
	if attempts != 1 {
		t.Errorf("a refused condition was retried %d times; it would fail identically", attempts)
	}
}

// TestClient_DeleteIf covers the conditional delete.
func TestClient_DeleteIf(t *testing.T) {
	mux, baseURL := newTestServer(t)
	mux.HandleFunc("DELETE /configs/{key}", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-Match") == "5" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusPreconditionFailed)
	})

	c := New([]string{baseURL})
	if err := c.DeleteIf(context.Background(), "db.host", 5); err != nil {
		t.Fatalf("DeleteIf on a matching revision: %v", err)
	}
	if err := c.DeleteIf(context.Background(), "db.host", 6); !errors.Is(err, ErrRevisionMismatch) {
		t.Errorf("DeleteIf on a stale revision: %v, want ErrRevisionMismatch", err)
	}
}
