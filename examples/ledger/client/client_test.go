package client

import (
	"context"
	"encoding/json"
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

func TestClient_CreateAccount(t *testing.T) {
	mux, url := newTestServer(t)

	mux.HandleFunc("POST /accounts", func(w http.ResponseWriter, r *http.Request) {
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(Account{ID: "alice", Balance: 1000})
	})

	c := New([]string{url})
	acc, err := c.CreateAccount(context.Background(), "alice", 1000)
	if err != nil {
		t.Fatal(err)
	}
	if acc.ID != "alice" || acc.Balance != 1000 {
		t.Errorf("unexpected account: %+v", acc)
	}
}

func TestClient_CreateAccount_Conflict(t *testing.T) {
	mux, url := newTestServer(t)

	mux.HandleFunc("POST /accounts", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "account already exists", http.StatusConflict)
	})

	c := New([]string{url})
	_, err := c.CreateAccount(context.Background(), "alice", 1000)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if err != ErrConflict {
		t.Errorf("err = %v, want ErrConflict", err)
	}
}

func TestClient_GetAccount(t *testing.T) {
	mux, url := newTestServer(t)

	mux.HandleFunc("GET /accounts/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") != "alice" {
			t.Errorf("unexpected id %q", r.PathValue("id"))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(Account{ID: "alice", Balance: 900})
	})

	c := New([]string{url})
	acc, err := c.GetAccount(context.Background(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	if acc.Balance != 900 {
		t.Errorf("Balance = %d, want 900", acc.Balance)
	}
}

func TestClient_GetAccount_NotFound(t *testing.T) {
	mux, url := newTestServer(t)

	mux.HandleFunc("GET /accounts/{id}", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	})

	c := New([]string{url})
	_, err := c.GetAccount(context.Background(), "nobody")
	if err != ErrNotFound {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestClient_ListAccounts(t *testing.T) {
	mux, url := newTestServer(t)

	mux.HandleFunc("GET /accounts", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]Account{
			"alice": {ID: "alice", Balance: 900},
			"bob":   {ID: "bob", Balance: 100},
		})
	})

	c := New([]string{url})
	all, err := c.ListAccounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Errorf("got %d accounts, want 2", len(all))
	}
	if all["alice"].Balance != 900 {
		t.Errorf("alice.Balance = %d, want 900", all["alice"].Balance)
	}
}

func TestClient_Transfer(t *testing.T) {
	mux, url := newTestServer(t)

	mux.HandleFunc("POST /transfers", func(w http.ResponseWriter, r *http.Request) {
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["from"] != "alice" || body["to"] != "bob" {
			t.Errorf("unexpected from/to: %v / %v", body["from"], body["to"])
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(Transfer{
			ID: "cli1:1", From: "alice", To: "bob", Amount: 100,
		})
	})

	c := New([]string{url})
	tr, err := c.Transfer(context.Background(), "alice", "bob", 100, "cli1", 1)
	if err != nil {
		t.Fatal(err)
	}
	if tr.ID != "cli1:1" || tr.Amount != 100 {
		t.Errorf("unexpected transfer: %+v", tr)
	}
}

func TestClient_Transfer_InsufficientFunds(t *testing.T) {
	mux, url := newTestServer(t)

	mux.HandleFunc("POST /transfers", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "insufficient funds: balance 100, requested 9999", http.StatusUnprocessableEntity)
	})

	c := New([]string{url})
	_, err := c.Transfer(context.Background(), "alice", "bob", 9999, "cli1", 1)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !isTerminal(err) {
		t.Errorf("expected terminal error, got %v", err)
	}
	// The error should wrap ErrInsufficientFunds.
	if err == ErrInsufficientFunds {
		t.Error("expected wrapped error, got bare sentinel")
	}
	// unwrap should reach ErrInsufficientFunds
	unwrapped := err
	for {
		if unwrapped == ErrInsufficientFunds {
			break
		}
		u, ok := unwrapped.(interface{ Unwrap() error })
		if !ok {
			t.Fatalf("error chain does not contain ErrInsufficientFunds: %v", err)
		}
		unwrapped = u.Unwrap()
	}
}

func TestClient_GetTransfer_NotFound(t *testing.T) {
	mux, url := newTestServer(t)

	mux.HandleFunc("GET /transfers/{id}", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	})

	c := New([]string{url})
	_, err := c.GetTransfer(context.Background(), "cli1:99")
	if err != ErrNotFound {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestClient_Redirect_UpdatesLeaderCache(t *testing.T) {
	// server2 is the actual leader.
	mux2, url2 := newTestServer(t)
	var leaderCalled int
	mux2.HandleFunc("POST /accounts", func(w http.ResponseWriter, r *http.Request) {
		leaderCalled++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(Account{ID: "alice", Balance: 0})
	})

	// server1 is a follower that redirects to server2.
	mux1, url1 := newTestServer(t)
	mux1.HandleFunc("POST /accounts", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, url2+r.URL.Path, http.StatusTemporaryRedirect)
	})

	c := New([]string{url1})

	// First call: hits follower, follows redirect to leader, caches leader.
	if _, err := c.CreateAccount(context.Background(), "alice", 0); err != nil {
		t.Fatal(err)
	}
	if leaderCalled != 1 {
		t.Errorf("leaderCalled = %d, want 1", leaderCalled)
	}

	// Verify the leader address was cached.
	if cached := c.inner.GetLeader(0); cached != url2 {
		t.Errorf("leaderAddr = %q, want %q", cached, url2)
	}

	// Second call: should go straight to leader without touching the follower.
	leaderCalled = 0
	if _, err := c.CreateAccount(context.Background(), "bob", 0); err != nil {
		t.Fatal(err)
	}
	if leaderCalled != 1 {
		t.Errorf("second call: leaderCalled = %d, want 1 (cache miss)", leaderCalled)
	}
}

func TestClient_StaleLeader_ClearedOnFailure(t *testing.T) {
	// server2 is working and will be used as a fallback seed.
	mux2, url2 := newTestServer(t)
	mux2.HandleFunc("GET /accounts/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(Account{ID: "alice", Balance: 42})
	})

	c := New([]string{url2})

	// Pre-populate the leader cache with a dead address.
	c.inner.SetLeader(0, "http://127.0.0.1:0") // guaranteed to refuse connections

	acc, err := c.GetAccount(context.Background(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	if acc.Balance != 42 {
		t.Errorf("Balance = %d, want 42", acc.Balance)
	}

	// The dead leader should have been cleared.
	if c.inner.GetLeader(0) == "http://127.0.0.1:0" {
		t.Error("stale leader was not cleared after failure")
	}
}

func TestClient_ServerError_IncludesBody(t *testing.T) {
	mux, url := newTestServer(t)

	mux.HandleFunc("POST /transfers", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "something went wrong internally", http.StatusInternalServerError)
	})

	c := New([]string{url})
	_, err := c.Transfer(context.Background(), "a", "b", 1, "c", 1)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "something went wrong internally") {
		t.Errorf("error %q does not contain server message", err.Error())
	}
}

func TestClient_ContextCancelled_StopsRetries(t *testing.T) {
	// All seeds refuse to connect.
	c := New([]string{
		"http://127.0.0.1:0",
		"http://127.0.0.1:0",
		"http://127.0.0.1:0",
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	_, err := c.GetAccount(ctx, "alice")
	if err == nil {
		t.Fatal("expected error with cancelled context")
	}
}

