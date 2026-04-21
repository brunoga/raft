package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClient_DomainManagement(t *testing.T) {
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	defer server.Close()

	c := New([]string{server.URL}, "test-client")

	// 1. CreateDomain
	mux.HandleFunc("POST /domains/{name}", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Client-ID") != "test-client" {
			t.Error("missing client ID header")
		}
		w.WriteHeader(http.StatusOK)
	})

	err := c.CreateDomain(context.Background(), "orders")
	if err != nil {
		t.Fatal(err)
	}

	// 2. Next
	mux.HandleFunc("POST /domains/{name}/next", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("count") != "10" {
			t.Errorf("expected count 10, got %s", r.URL.Query().Get("count"))
		}
		json.NewEncoder(w).Encode(AllocResult{Start: 100, Count: 10})
	})

	res, err := c.Next(context.Background(), "orders", 10)
	if err != nil {
		t.Fatal(err)
	}
	if res.Start != 100 {
		t.Errorf("expected start 100, got %d", res.Start)
	}

	// 3. Current
	mux.HandleFunc("GET /domains/{name}/current", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(DomainInfo{Domain: "orders", Current: 110})
	})

	info, err := c.Current(context.Background(), "orders", false)
	if err != nil {
		t.Fatal(err)
	}
	if info.Current != 110 {
		t.Errorf("expected current 110, got %d", info.Current)
	}
}

func TestClient_Idempotency(t *testing.T) {
	var seqs []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seqs = append(seqs, r.Header.Get("X-Seq-Num"))
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	c := New([]string{server.URL}, "test-client")

	// Call 1
	_ = c.CreateDomain(context.Background(), "d1")
	// Call 2
	_ = c.CreateDomain(context.Background(), "d2")

	if len(seqs) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(seqs))
	}
	if seqs[0] == seqs[1] {
		t.Errorf("sequence numbers should be different: %v", seqs)
	}
}
