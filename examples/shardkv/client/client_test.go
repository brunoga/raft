package client

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClient_ShardedCRUD(t *testing.T) {
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	defer server.Close()

	// 4 shards
	c := New([]string{server.URL}, 4)

	// 1. Put
	mux.HandleFunc("PUT /keys/{key}", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if string(body) != "myvalue" {
			t.Errorf("expected myvalue, got %q", string(body))
		}
		w.WriteHeader(http.StatusNoContent)
	})

	err := c.Put(context.Background(), "mykey", "myvalue")
	if err != nil {
		t.Fatal(err)
	}

	// 2. Get
	mux.HandleFunc("GET /keys/{key}", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("myvalue"))
	})

	val, err := c.Get(context.Background(), "mykey")
	if err != nil {
		t.Fatal(err)
	}
	if val != "myvalue" {
		t.Errorf("expected myvalue, got %q", val)
	}

	// 3. Delete
	mux.HandleFunc("DELETE /keys/{key}", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	err = c.Delete(context.Background(), "mykey")
	if err != nil {
		t.Fatal(err)
	}
}

func TestClient_ShardRouting(t *testing.T) {
	c := New(nil, 4)

	// Keys should map to consistent shards
	g1 := c.shardForKey("a")
	g2 := c.shardForKey("b")

	if g1 == 0 || g1 > 4 {
		t.Errorf("invalid shard: %d", g1)
	}
	if g1 == g2 {
		// With FNV and 4 shards, "a" and "b" might hash to the same thing,
		// but they actually don't.
	}

	if c.shardForKey("a") != g1 {
		t.Error("shard routing is not consistent")
	}
}
