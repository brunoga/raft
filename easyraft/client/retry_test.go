package client_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/brunoga/raft/v2/easyraft"
	"github.com/brunoga/raft/v2/easyraft/client"
)

// flakyServer answers 503 for the first n requests and then serves body,
// counting what it saw. It stands in for a cluster between leaders, which is
// what the retry budget exists for.
type flakyServer struct {
	unavailable int32
	seen        atomic.Int32
	body        string
	status      int
}

func (f *flakyServer) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	if f.seen.Add(1) <= f.unavailable {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"no leader currently elected"}`))
		return
	}
	status := f.status
	if status == 0 {
		status = http.StatusOK
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(f.body))
}

// TestRetry_OutlastsALeaderElection pins the reason the default budget is the
// size it is.
//
// The condition worth retrying is a cluster between leaders, and an election
// takes an election timeout -- one to two seconds with easyraft's default
// Raft timings. A client whose budget is shorter than that fails at exactly
// the moment it exists to paper over, which is how this was found: a test
// asking for ErrKeyExists got "no leader currently elected" instead, because
// the old default gave up after 350ms.
func TestRetry_OutlastsALeaderElection(t *testing.T) {
	// Five 503s, then the answer. Under the default budget the sixth attempt
	// is the last one, so this is the worst case that must still work.
	flaky := &flakyServer{unavailable: client.DefaultRetryAttempts - 1, body: `{"name":"Alice"}`}
	srv := httptest.NewServer(flaky)
	defer srv.Close()

	c, err := client.New(client.WithEndpoints(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	got, err := client.Collection[User](c, "users").Read(ctx, "alice")
	if err != nil {
		t.Fatalf("a read that needed every attempt failed: %v", err)
	}
	if got.Name != "Alice" {
		t.Errorf("read returned %+v", got)
	}
	if seen := flaky.seen.Load(); seen != int32(client.DefaultRetryAttempts) {
		t.Errorf("the server saw %d attempts, want %d", seen, client.DefaultRetryAttempts)
	}

	// And the budget really does span an election rather than merely counting
	// to six: doubling from 100ms and capped at a second, the waits before
	// six attempts add up to about two and a half seconds.
	const wantAtLeast = 2 * time.Second
	var total time.Duration
	backoff := client.DefaultRetryBackoff
	for range client.DefaultRetryAttempts - 1 {
		total += backoff
		if backoff < time.Second {
			backoff *= 2
		}
	}
	if total < wantAtLeast {
		t.Errorf("the default budget waits %v in total, which is shorter than an election", total)
	}
}

// TestRetry_GivesUpWhenAskedTo checks the escape hatch, since a caller who
// would rather hear about a cluster in motion has to be able to say so.
func TestRetry_GivesUpWhenAskedTo(t *testing.T) {
	flaky := &flakyServer{unavailable: 1000, body: ""}
	srv := httptest.NewServer(flaky)
	defer srv.Close()

	c, err := client.New(
		client.WithEndpoints(srv.URL),
		client.WithRetry(1, time.Millisecond),
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	start := time.Now()
	if _, err := client.Collection[User](c, "users").Read(ctx, "alice"); err == nil {
		t.Fatal("a read against a server that is never available succeeded")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("a single-attempt read took %v", elapsed)
	}
	if seen := flaky.seen.Load(); seen != 1 {
		t.Errorf("the server saw %d attempts under WithRetry(1, ...)", seen)
	}
}

// TestRetry_DoesNotRepeatADefiniteAnswer keeps the budget from turning a
// answer the cluster gave into six of them. A missing key is missing on the
// first attempt and on every one after it.
func TestRetry_DoesNotRepeatADefiniteAnswer(t *testing.T) {
	flaky := &flakyServer{
		status: http.StatusNotFound,
		body:   `{"error":"easyraft: key not found"}`,
	}
	srv := httptest.NewServer(flaky)
	defer srv.Close()

	c, err := client.New(client.WithEndpoints(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err = client.Collection[User](c, "users").Read(ctx, "nobody")
	if !errors.Is(err, easyraft.ErrKeyNotFound) {
		t.Fatalf("read of a missing key: %v, want ErrKeyNotFound", err)
	}
	if seen := flaky.seen.Load(); seen != 1 {
		t.Errorf("a 404 was retried: the server saw %d attempts", seen)
	}
}

// TestRetry_StopsWhenTheContextDoes checks that the budget never outlives
// what the caller allowed, since it is a bound on attempts rather than time.
func TestRetry_StopsWhenTheContextDoes(t *testing.T) {
	flaky := &flakyServer{unavailable: 1000}
	srv := httptest.NewServer(flaky)
	defer srv.Close()

	c, err := client.New(client.WithEndpoints(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err = client.Collection[User](c, "users").Read(ctx, "alice")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("read against an unavailable server: %v, want the context's deadline", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("the read ran for %v past a 250ms context", elapsed)
	}
}
