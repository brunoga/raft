package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClient_QuotaCRUD(t *testing.T) {
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	defer server.Close()

	c := New([]string{server.URL})

	// 1. SetQuota
	mux.HandleFunc("PUT /quotas/{key}", func(w http.ResponseWriter, r *http.Request) {
		var q Quota
		if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
			t.Fatal(err)
		}
		if q.MaxTokens != 100 {
			t.Errorf("expected MaxTokens 100, got %d", q.MaxTokens)
		}
		w.WriteHeader(http.StatusNoContent)
	})

	err := c.SetQuota(context.Background(), "user1", Quota{MaxTokens: 100})
	if err != nil {
		t.Fatal(err)
	}

	// 2. GetQuota
	mux.HandleFunc("GET /quotas/{key}", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(Quota{MaxTokens: 100, CurrentTokens: 50})
	})

	q, err := c.GetQuota(context.Background(), "user1", false)
	if err != nil {
		t.Fatal(err)
	}
	if q.CurrentTokens != 50 {
		t.Errorf("expected 50 tokens, got %d", q.CurrentTokens)
	}

	// 3. DeleteQuota
	mux.HandleFunc("DELETE /quotas/{key}", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	err = c.DeleteQuota(context.Background(), "user1")
	if err != nil {
		t.Fatal(err)
	}
}

func TestClient_Take(t *testing.T) {
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	defer server.Close()

	c := New([]string{server.URL})

	mux.HandleFunc("POST /quotas/{key}/mutate", func(w http.ResponseWriter, r *http.Request) {
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

		json.NewEncoder(w).Encode(TakeResponse{Allowed: true, Remains: 45})
	})

	resp, err := c.Take(context.Background(), "user1", 5)
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Allowed || resp.Remains != 45 {
		t.Errorf("unexpected response: %+v", resp)
	}
}
