// Package client provides a high-level Go client for the ratelimiter service.
// It handles leader discovery, automatic retries, and provides a simple API
// for quota management and token consumption with deterministic timestamping.
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

var (
	ErrNotFound  = errors.New("quota not found")
	ErrNotLeader = errors.New("node is not the leader")
)

// Quota represents the rate limit configuration for an API key.
type Quota struct {
	MaxTokens      int64 `json:"max_tokens"`
	CurrentTokens  int64 `json:"current_tokens"`
	RefillRate     int64 `json:"refill_rate"`
	LastRefillTime int64 `json:"last_refill_time"`
}

// TakeResponse is the result of a Take operation.
type TakeResponse struct {
	Allowed bool  `json:"allowed"`
	Remains int64 `json:"remains"`
}

// Client is a high-level client for the ratelimiter service.
type Client struct {
	addrs      []string
	httpClient *http.Client

	mu         sync.RWMutex
	leaderAddr string
}

// New returns a new ratelimiter client.
func New(addrs []string) *Client {
	return &Client{
		addrs: addrs,
		httpClient: &http.Client{
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// Take attempts to consume tokens for the given key.
func (c *Client) Take(ctx context.Context, key string, requested int64) (TakeResponse, error) {
	args := struct {
		Requested int64 `json:"requested"`
		Now       int64 `json:"now"`
	}{
		Requested: requested,
		Now:       time.Now().Unix(),
	}

	body, err := json.Marshal(struct {
		Name string `json:"name"`
		Args any    `json:"args"`
	}{
		Name: "take",
		Args: args,
	})
	if err != nil {
		return TakeResponse{}, err
	}

	var resp TakeResponse
	err = c.doWithRetry(ctx, http.MethodPost, "/quotas/"+key+"/mutate", bytes.NewReader(body), &resp)
	return resp, err
}

// GetQuota retrieves the current quota for a key.
func (c *Client) GetQuota(ctx context.Context, key string, stale bool) (Quota, error) {
	path := "/quotas/" + key
	if stale {
		path += "?consistency=stale"
	}

	var q Quota
	err := c.doWithRetry(ctx, http.MethodGet, path, nil, &q)
	return q, err
}

// SetQuota creates or updates a quota for a key.
func (c *Client) SetQuota(ctx context.Context, key string, q Quota) error {
	body, err := json.Marshal(q)
	if err != nil {
		return err
	}

	return c.doWithRetry(ctx, http.MethodPut, "/quotas/"+key, bytes.NewReader(body), nil)
}

// DeleteQuota removes a quota.
func (c *Client) DeleteQuota(ctx context.Context, key string) error {
	return c.doWithRetry(ctx, http.MethodDelete, "/quotas/"+key, nil, nil)
}

func (c *Client) doWithRetry(ctx context.Context, method, path string, body io.Reader, result any) error {
	var bodyBytes []byte
	if body != nil {
		var err error
		bodyBytes, err = io.ReadAll(body)
		if err != nil {
			return err
		}
	}

	c.mu.RLock()
	leader := c.leaderAddr
	c.mu.RUnlock()

	if leader != "" {
		err := c.doOnce(ctx, leader, method, path, bodyBytes, result)
		if err == nil {
			return nil
		}
		if errors.Is(err, ErrNotFound) {
			return err
		}
	}

	var lastErr error
	for _, addr := range c.addrs {
		err := c.doOnce(ctx, addr, method, path, bodyBytes, result)
		if err == nil {
			return nil
		}
		if errors.Is(err, ErrNotFound) {
			return err
		}
		lastErr = err
	}

	return lastErr
}

func (c *Client) doOnce(ctx context.Context, addr, method, path string, bodyBytes []byte, result any) error {
	urlStr := strings.TrimSuffix(addr, "/") + path
	var reqBody io.Reader
	if bodyBytes != nil {
		reqBody = bytes.NewReader(bodyBytes)
	}

	req, err := http.NewRequestWithContext(ctx, method, urlStr, reqBody)
	if err != nil {
		return err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusTemporaryRedirect {
		location := resp.Header.Get("Location")
		if location == "" {
			return errors.New("redirect without location")
		}

		u, err := url.Parse(location)
		if err != nil {
			return fmt.Errorf("invalid redirect location: %w", err)
		}
		newBase := fmt.Sprintf("%s://%s", u.Scheme, u.Host)

		c.mu.Lock()
		c.leaderAddr = newBase
		c.mu.Unlock()

		return c.doOnce(ctx, newBase, method, path, bodyBytes, result)
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if result != nil {
			return json.NewDecoder(resp.Body).Decode(result)
		}
		return nil
	}

	if resp.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}

	if resp.StatusCode == http.StatusServiceUnavailable {
		return ErrNotLeader
	}

	return fmt.Errorf("unexpected status: %s", resp.Status)
}
