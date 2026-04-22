// Package client provides a high-level Go client for the ratelimiter service.
//
// The client handles leader discovery and automatic redirection transparently.
// Any node address can be used as a seed; write requests are routed to the
// current leader and the leader address is cached and revalidated on failure.
package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/brunoga/raft/examples/internal/exampleutil"
)

// Sentinel errors returned by the client.
var (
	// ErrNotFound is returned when the requested quota key does not exist.
	ErrNotFound = errors.New("quota not found")

	// ErrConflict is returned by CreateQuota when the key already exists.
	ErrConflict = errors.New("quota already exists")

	// ErrNotLeader is returned when the request reaches a follower and no
	// redirect is available. This is transient; retrying usually succeeds.
	ErrNotLeader = exampleutil.ErrNotLeader
)

// Quota represents the rate limit configuration for an API key.
type Quota struct {
	MaxTokens      int64 `json:"max_tokens"`
	CurrentTokens  int64 `json:"current_tokens"`
	RefillRate     int64 `json:"refill_rate"` // tokens per second
	LastRefillTime int64 `json:"last_refill_time"`
}

// TakeResponse is the result of a Take operation.
type TakeResponse struct {
	Allowed bool  `json:"allowed"`
	Remains int64 `json:"remains"`
}

// Client is a thread-safe, high-level client for the ratelimiter service.
type Client struct {
	inner *exampleutil.Client
}

// New returns a new ratelimiter client. addrs should be the HTTP addresses of
// one or more cluster nodes (e.g. "http://localhost:8001"). At least one
// reachable address is required; the rest serve as fallbacks.
func New(addrs []string) *Client {
	c := exampleutil.NewClient(addrs)
	c.ErrorMapper = func(status int, _ string) error {
		switch status {
		case http.StatusNotFound:
			return ErrNotFound
		case http.StatusConflict:
			return ErrConflict
		}
		return nil
	}
	return &Client{inner: c}
}

// CreateQuota creates a new quota entry. Returns ErrConflict if the key
// already exists — use SetQuota for a create-or-update operation.
func (c *Client) CreateQuota(ctx context.Context, key string, q Quota) error {
	body, err := json.Marshal(q)
	if err != nil {
		return err
	}
	return c.inner.Do(ctx, 0, http.MethodPost, "/quotas/"+key, body, nil, isTerminal)
}

// SetQuota creates or updates the quota for a key. It first attempts a create;
// if the key already exists it performs an update instead.
func (c *Client) SetQuota(ctx context.Context, key string, q Quota) error {
	body, err := json.Marshal(q)
	if err != nil {
		return err
	}
	if err := c.inner.Do(ctx, 0, http.MethodPost, "/quotas/"+key, body, nil, isTerminal); err != nil {
		if !errors.Is(err, ErrConflict) {
			return err
		}
		// Key exists — update in place.
		return c.inner.Do(ctx, 0, http.MethodPut, "/quotas/"+key, body, nil, isTerminal)
	}
	return nil
}

// GetQuota retrieves the current quota for a key. If stale is true it performs
// a local read from any node; otherwise it performs a linearizable read.
// Returns ErrNotFound if the key does not exist.
func (c *Client) GetQuota(ctx context.Context, key string, stale bool) (Quota, error) {
	path := "/quotas/" + key
	if stale {
		path += "?consistency=stale"
	}
	var q Quota
	if err := c.inner.Do(ctx, 0, http.MethodGet, path, nil, &q, isTerminal); err != nil {
		return Quota{}, err
	}
	return q, nil
}

// DeleteQuota removes a quota. Returns ErrNotFound if the key does not exist.
func (c *Client) DeleteQuota(ctx context.Context, key string) error {
	return c.inner.Do(ctx, 0, http.MethodDelete, "/quotas/"+key, nil, nil, isTerminal)
}

// Take attempts to consume n tokens for the given key. The Now timestamp must
// be the current Unix time; it is encoded in the request so that all replicas
// apply the exact same refill delta (deterministic mutation requirement).
//
// Returns ErrNotFound if the key does not exist.
func (c *Client) Take(ctx context.Context, key string, n int64) (TakeResponse, error) {
	body, err := json.Marshal(struct {
		Name string `json:"name"`
		Args any    `json:"args"`
	}{
		Name: "take",
		Args: struct {
			Requested int64 `json:"requested"`
			Now       int64 `json:"now"`
		}{Requested: n, Now: time.Now().Unix()},
	})
	if err != nil {
		return TakeResponse{}, err
	}
	var resp TakeResponse
	if err := c.inner.Do(ctx, 0, http.MethodPost, "/quotas/"+key+"/mutate", body, &resp, isTerminal); err != nil {
		return TakeResponse{}, err
	}
	return resp, nil
}

// isTerminal reports whether err should stop the retry loop immediately.
// Domain errors are terminal; network errors and ErrNotLeader are retried.
func isTerminal(err error) bool {
	return errors.Is(err, ErrNotFound) || errors.Is(err, ErrConflict)
}
