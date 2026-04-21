// Package client provides a high-level Go client for the ratelimiter service.
//
// The client handles leader discovery and automatic redirection transparently.
// Any node address can be used as a seed; write requests are routed to the
// current leader and the leader address is cached and revalidated on failure.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Sentinel errors returned by the client.
var (
	// ErrNotFound is returned when the requested quota key does not exist.
	ErrNotFound = errors.New("quota not found")

	// ErrConflict is returned by CreateQuota when the key already exists.
	ErrConflict = errors.New("quota already exists")

	// ErrNotLeader is returned when the request reaches a follower and no
	// redirect is available. This is transient; retrying usually succeeds.
	ErrNotLeader = errors.New("node is not the leader")
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
	addrs []string // seed node addresses, e.g. "http://host:8001"

	// httpClient does not follow redirects so we can intercept 307 responses
	// and update the cached leader address ourselves.
	httpClient *http.Client

	mu         sync.RWMutex
	leaderAddr string // cached leader; empty means unknown
}

// New returns a new ratelimiter client. addrs should be the HTTP addresses of
// one or more cluster nodes (e.g. "http://localhost:8001"). At least one
// reachable address is required; the rest serve as fallbacks.
func New(addrs []string) *Client {
	return &Client{
		addrs: addrs,
		httpClient: &http.Client{
			// Disable automatic redirect following so we can intercept 307
			// responses and update the cached leader address.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// CreateQuota creates a new quota entry. Returns ErrConflict if the key
// already exists — use SetQuota for a create-or-update operation.
func (c *Client) CreateQuota(ctx context.Context, key string, q Quota) error {
	body, err := json.Marshal(q)
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodPost, "/quotas/"+key, body, nil)
}

// SetQuota creates or updates the quota for a key. It first attempts a create;
// if the key already exists it performs an update instead.
func (c *Client) SetQuota(ctx context.Context, key string, q Quota) error {
	body, err := json.Marshal(q)
	if err != nil {
		return err
	}
	if err := c.do(ctx, http.MethodPost, "/quotas/"+key, body, nil); err != nil {
		if !errors.Is(err, ErrConflict) {
			return err
		}
		// Key exists — update in place.
		return c.do(ctx, http.MethodPut, "/quotas/"+key, body, nil)
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
	if err := c.do(ctx, http.MethodGet, path, nil, &q); err != nil {
		return Quota{}, err
	}
	return q, nil
}

// DeleteQuota removes a quota. Returns ErrNotFound if the key does not exist.
func (c *Client) DeleteQuota(ctx context.Context, key string) error {
	return c.do(ctx, http.MethodDelete, "/quotas/"+key, nil, nil)
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
	if err := c.do(ctx, http.MethodPost, "/quotas/"+key+"/mutate", body, &resp); err != nil {
		return TakeResponse{}, err
	}
	return resp, nil
}

// do executes a request, trying the cached leader first and falling back to
// seed nodes. It refreshes the leader cache on 307 redirects and invalidates
// it when the cached leader stops responding.
func (c *Client) do(ctx context.Context, method, path string, body []byte, result any) error {
	// 1. Try the cached leader first.
	c.mu.RLock()
	leader := c.leaderAddr
	c.mu.RUnlock()

	if leader != "" {
		err := c.doOnce(ctx, leader, method, path, body, result, false)
		if err == nil {
			return nil
		}
		if isTerminal(err) {
			return err
		}
		// Leader is stale (down or re-elected). Clear the cache so future
		// calls skip straight to the seed fan-out.
		c.mu.Lock()
		if c.leaderAddr == leader {
			c.leaderAddr = ""
		}
		c.mu.Unlock()
	}

	// 2. Fan out to seed nodes until one succeeds.
	var lastErr error
	for _, addr := range c.addrs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		err := c.doOnce(ctx, addr, method, path, body, result, false)
		if err == nil {
			return nil
		}
		if isTerminal(err) {
			return err
		}
		lastErr = err
	}

	if lastErr != nil {
		return lastErr
	}
	return errors.New("no nodes available")
}

// doOnce performs a single HTTP request against addr. If the server responds
// with a 307 redirect and followed is false, it updates the leader cache and
// retries once against the new address. The followed flag prevents redirect
// loops.
func (c *Client) doOnce(ctx context.Context, addr, method, path string, body []byte, result any, followed bool) error {
	urlStr := strings.TrimSuffix(addr, "/") + path

	var reqBody io.Reader
	if body != nil {
		reqBody = bytes.NewReader(body)
	}

	req, err := http.NewRequestWithContext(ctx, method, urlStr, reqBody)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusTemporaryRedirect {
		if followed {
			return errors.New("redirect loop detected")
		}
		location := resp.Header.Get("Location")
		if location == "" {
			return errors.New("redirect with empty Location")
		}
		u, err := url.Parse(location)
		if err != nil {
			return fmt.Errorf("bad redirect location %q: %w", location, err)
		}
		newBase := fmt.Sprintf("%s://%s", u.Scheme, u.Host)

		c.mu.Lock()
		c.leaderAddr = newBase
		c.mu.Unlock()

		return c.doOnce(ctx, newBase, method, path, body, result, true)
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if result != nil {
			return json.NewDecoder(resp.Body).Decode(result)
		}
		return nil
	}

	// Read the response body for a meaningful error message (capped to avoid
	// consuming runaway responses).
	errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	msg := strings.TrimSpace(string(errBody))

	switch resp.StatusCode {
	case http.StatusNotFound:
		return ErrNotFound
	case http.StatusConflict:
		return ErrConflict
	case http.StatusServiceUnavailable:
		return ErrNotLeader
	default:
		if msg != "" {
			return fmt.Errorf("server error %d: %s", resp.StatusCode, msg)
		}
		return fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
}

// isTerminal reports whether err should stop the retry loop immediately.
// Domain errors are terminal; network errors and ErrNotLeader are retried.
func isTerminal(err error) bool {
	return errors.Is(err, ErrNotFound) || errors.Is(err, ErrConflict)
}
