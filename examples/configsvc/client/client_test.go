package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestClient_CRUD(t *testing.T) {
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	defer server.Close()

	c := New([]string{server.URL})

	// 1. Set
	mux.HandleFunc("PUT /configs/{key}", func(w http.ResponseWriter, r *http.Request) {
		key := r.PathValue("key")
		if key != "mykey" {
			t.Errorf("expected key 'mykey', got %q", key)
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["value"] != "myvalue" {
			t.Errorf("expected value 'myvalue', got %q", body["value"])
		}
		w.WriteHeader(http.StatusNoContent)
	})

	err := c.Set(context.Background(), "mykey", "myvalue")
	if err != nil {
		t.Fatal(err)
	}

	// 2. Get
	mux.HandleFunc("GET /configs/{key}", func(w http.ResponseWriter, r *http.Request) {
		key := r.PathValue("key")
		if key != "mykey" {
			t.Errorf("expected key 'mykey', got %q", key)
		}
		json.NewEncoder(w).Encode(ConfigEntry{
			Value:   "myvalue",
			Version: 12345,
		})
	})

	entry, err := c.Get(context.Background(), "mykey", false)
	if err != nil {
		t.Fatal(err)
	}
	if entry.Value != "myvalue" || entry.Version != 12345 {
		t.Errorf("unexpected entry: %+v", entry)
	}

	// 3. Delete
	mux.HandleFunc("DELETE /configs/{key}", func(w http.ResponseWriter, r *http.Request) {
		key := r.PathValue("key")
		if key != "mykey" {
			t.Errorf("expected key 'mykey', got %q", key)
		}
		w.WriteHeader(http.StatusNoContent)
	})

	err = c.Delete(context.Background(), "mykey")
	if err != nil {
		t.Fatal(err)
	}
}

func TestClient_Watch(t *testing.T) {
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	defer server.Close()

	c := New([]string{server.URL})

	mux.HandleFunc("GET /watch/{key}", func(w http.ResponseWriter, r *http.Request) {
		key := r.PathValue("key")
		if key != "mykey" {
			t.Errorf("expected key 'mykey', got %q", key)
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		flusher, _ := w.(http.Flusher)

		// Send snapshot
		fmt.Fprintf(w, "event: snapshot\ndata: %s\n\n", `{"key":"mykey","value":{"value":"init","version":1}}`)
		flusher.Flush()

		// Send change
		fmt.Fprintf(w, "event: change\ndata: %s\n\n", `{"key":"mykey","value":{"value":"updated","version":2}}`)
		flusher.Flush()

		// Wait for context done
		<-r.Context().Done()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	ch, err := c.Watch(ctx, "mykey")
	if err != nil {
		t.Fatal(err)
	}

	// Expect snapshot
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("channel closed unexpectedly")
		}
		if ev.Type != "snapshot" || ev.Key != "mykey" || ev.Value.Value != "init" {
			t.Errorf("unexpected snapshot: %+v", ev)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("timeout waiting for snapshot")
	}

	// Expect change
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("channel closed unexpectedly")
		}
		if ev.Type != "change" || ev.Key != "mykey" || ev.Value.Value != "updated" {
			t.Errorf("unexpected change: %+v", ev)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("timeout waiting for change")
	}
}

func TestClient_Redirect(t *testing.T) {
	mux2 := http.NewServeMux()
	server2 := httptest.NewServer(mux2) // The real leader
	defer server2.Close()

	mux1 := http.NewServeMux()
	server1 := httptest.NewServer(mux1) // A follower
	defer server1.Close()

	c := New([]string{server1.URL})

	// 1. Follower redirects to leader.
	mux1.HandleFunc("PUT /configs/{key}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", server2.URL+"/configs/"+r.PathValue("key"))
		w.WriteHeader(http.StatusTemporaryRedirect)
	})

	// 2. Leader succeeds.
	var leaderCalled bool
	mux2.HandleFunc("PUT /configs/{key}", func(w http.ResponseWriter, r *http.Request) {
		leaderCalled = true
		w.WriteHeader(http.StatusNoContent)
	})

	err := c.Set(context.Background(), "k", "v")
	if err != nil {
		t.Fatal(err)
	}

	if !leaderCalled {
		t.Error("expected leader to be called")
	}

	c.mu.RLock()
	cached := c.leaderAddr
	c.mu.RUnlock()
	if cached != server2.URL {
		t.Errorf("expected leader addr %q to be cached, got %q", server2.URL, cached)
	}

	// 3. Second call should hit leader directly.
	leaderCalled = false
	err = c.Set(context.Background(), "k2", "v2")
	if err != nil {
		t.Fatal(err)
	}
	if !leaderCalled {
		t.Error("expected leader to be called directly from cache")
	}
}
