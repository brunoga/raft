package exampleutil

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
)

var (
	// ErrNotLeader is returned when a request reaches a non-leader node and
	// no redirect is available.
	ErrNotLeader = errors.New("node is not the leader")
)

// RequestOption is a callback that can modify an outgoing HTTP request.
type RequestOption func(*http.Request)

// Client is a generic thread-safe client that handles leader discovery,
// 307 redirects, and retries against seed nodes.
type Client struct {
	Addrs      []string
	HTTPClient *http.Client

	mu          sync.RWMutex
	leaderAddrs map[uint64]string // shardID -> leaderAddr (0 for single-shard)
}

// NewClient returns a new Client with the given seed addresses.
func NewClient(addrs []string) *Client {
	return &Client{
		Addrs:       addrs,
		leaderAddrs: make(map[uint64]string),
		HTTPClient: &http.Client{
			// Intercept 307 redirects to update the leader cache manually.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// GetLeader returns the cached leader address for the given shardID.
func (c *Client) GetLeader(shardID uint64) string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.leaderAddrs[shardID]
}

// SetLeader updates the cached leader address for the given shardID.
func (c *Client) SetLeader(shardID uint64, addr string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.leaderAddrs[shardID] = addr
}

// Do executes a request, trying the cached leader for the given shardID first.

// It handles 307 redirects by updating the leader cache and retrying once.
// result may be nil, *bytes.Buffer, or any JSON-decodable type.
func (c *Client) Do(ctx context.Context, shardID uint64, method, path string, body []byte, result any, isTerminal func(error) bool, opts ...RequestOption) error {
	// 1. Try the cached leader first.
	c.mu.RLock()
	leader := c.leaderAddrs[shardID]
	c.mu.RUnlock()

	if leader != "" {
		err := c.doOnce(ctx, shardID, leader, method, path, body, result, false, opts...)
		if err == nil {
			return nil
		}
		if isTerminal != nil && isTerminal(err) {
			return err
		}
		// Leader is stale (down or re-elected). Clear the cache.
		c.mu.Lock()
		if c.leaderAddrs[shardID] == leader {
			delete(c.leaderAddrs, shardID)
		}
		c.mu.Unlock()
	}

	// 2. Fan out to seed nodes.
	var lastErr error
	for _, addr := range c.Addrs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		err := c.doOnce(ctx, shardID, addr, method, path, body, result, false, opts...)
		if err == nil {
			return nil
		}
		if isTerminal != nil && isTerminal(err) {
			return err
		}
		lastErr = err
	}

	if lastErr != nil {
		return lastErr
	}
	return errors.New("no nodes available")
}

func (c *Client) doOnce(ctx context.Context, shardID uint64, addr, method, path string, body []byte, result any, followed bool, opts ...RequestOption) error {
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
	for _, opt := range opts {
		opt(req)
	}

	resp, err := c.HTTPClient.Do(req)
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
		c.leaderAddrs[shardID] = newBase
		c.mu.Unlock()

		return c.doOnce(ctx, shardID, newBase, method, path, body, result, true, opts...)
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if result != nil {
			switch dst := result.(type) {
			case *bytes.Buffer:
				_, err = io.Copy(dst, resp.Body)
				return err
			default:
				return json.NewDecoder(resp.Body).Decode(result)
			}
		}
		return nil
	}

	// Read error message from body.
	errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	msg := strings.TrimSpace(string(errBody))

	if resp.StatusCode == http.StatusServiceUnavailable {
		return ErrNotLeader
	}

	if msg != "" {
		return fmt.Errorf("server error %d: %s", resp.StatusCode, msg)
	}
	return fmt.Errorf("unexpected status %d", resp.StatusCode)
}
