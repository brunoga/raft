// Package client provides a high-level Go client for the configsvc example.
//
// The client handles leader discovery and automatic redirection transparently.
// Any node address can be used as a seed; write requests are routed to the
// current leader and the leader address is cached and revalidated on failure.
// Watch connections can be established to any node — each node fires SSE
// events independently from its own OnChange callback.
package client

import (
	"bufio"
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

// Sentinel errors returned by the client.
var (
	// ErrNotFound is returned when the requested config key does not exist.
	ErrNotFound = errors.New("config key not found")

	// ErrNotLeader is returned when the request reaches a follower and no
	// redirect is available. This is transient; retrying usually succeeds.
	ErrNotLeader = errors.New("node is not the leader")
)

// ConfigEntry is the value stored in the "configs" collection.
type ConfigEntry struct {
	Value   string `json:"value"`
	Version int64  `json:"version"` // Unix nanoseconds
}

// ChangeEvent is delivered to Watch subscribers when a config entry changes.
// Type is one of "snapshot", "change", or "delete".
type ChangeEvent struct {
	Type    string       `json:"type"`
	Key     string       `json:"key"`
	Value   *ConfigEntry `json:"value,omitempty"`
	Deleted bool         `json:"deleted,omitempty"`
}

// Client is a thread-safe, high-level client for the configsvc.
type Client struct {
	addrs []string // seed node addresses, e.g. "http://host:8001"

	// httpClient does not follow redirects so we can intercept 307 responses
	// and update the cached leader address ourselves.
	httpClient *http.Client

	mu         sync.RWMutex
	leaderAddr string // cached leader; empty means unknown
}

// New returns a new configsvc client. addrs should be the HTTP addresses of
// one or more cluster nodes (e.g. "http://localhost:8001"). At least one
// reachable address is required; the rest serve as fallbacks.
func New(addrs []string) *Client {
	return &Client{
		addrs: addrs,
		httpClient: &http.Client{
			// Disable automatic redirect following so we can intercept 307
			// responses and update the cached leader address ourselves.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// Set upserts a config entry with an updated version timestamp.
func (c *Client) Set(ctx context.Context, key, value string) error {
	body, err := json.Marshal(map[string]string{"value": value})
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodPut, "/configs/"+key, body, nil)
}

// Get retrieves a config entry. If stale is true it performs a local read from
// any node; otherwise it performs a linearizable read.
// Returns ErrNotFound if the key does not exist.
func (c *Client) Get(ctx context.Context, key string, stale bool) (ConfigEntry, error) {
	path := "/configs/" + key
	if stale {
		path += "?consistency=stale"
	}
	var entry ConfigEntry
	if err := c.do(ctx, http.MethodGet, path, nil, &entry); err != nil {
		return ConfigEntry{}, err
	}
	return entry, nil
}

// Delete removes a config entry. Returns ErrNotFound if the key does not exist.
func (c *Client) Delete(ctx context.Context, key string) error {
	return c.do(ctx, http.MethodDelete, "/configs/"+key, nil, nil)
}

// List retrieves all config entries. If stale is true it performs a local read
// from any node; otherwise it performs a linearizable read.
func (c *Client) List(ctx context.Context, stale bool) (map[string]ConfigEntry, error) {
	path := "/configs"
	if stale {
		path += "?consistency=stale"
	}
	var all map[string]ConfigEntry
	if err := c.do(ctx, http.MethodGet, path, nil, &all); err != nil {
		return nil, err
	}
	return all, nil
}

// Watch subscribes to changes for the given key. If key is empty, all changes
// are delivered. The returned channel is closed when the context is cancelled
// or the connection is lost.
//
// Each new connection starts with one "snapshot" event per matching key
// already in the collection, followed by "change" and "delete" events as they
// are committed. Watch can connect to any node — watches do not require the
// leader.
func (c *Client) Watch(ctx context.Context, key string) (<-chan ChangeEvent, error) {
	path := "/watch"
	if key != "" {
		path += "/" + key
	}

	// Watches work on any node, but try the cached leader first since it is
	// likely still reachable.
	c.mu.RLock()
	leader := c.leaderAddr
	c.mu.RUnlock()

	if leader != "" {
		ch, err := c.watchOnce(ctx, leader, path)
		if err == nil {
			return ch, nil
		}
	}

	for _, addr := range c.addrs {
		ch, err := c.watchOnce(ctx, addr, path)
		if err == nil {
			return ch, nil
		}
	}
	return nil, errors.New("failed to connect to any node for watch")
}

func (c *Client) watchOnce(ctx context.Context, addr, path string) (<-chan ChangeEvent, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(addr, "/")+path, http.NoBody)
	if err != nil {
		return nil, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode == http.StatusTemporaryRedirect {
		_ = resp.Body.Close()
		location := resp.Header.Get("Location")
		if location == "" {
			return nil, errors.New("redirect with empty Location")
		}
		u, err := url.Parse(location)
		if err != nil {
			return nil, fmt.Errorf("bad redirect location %q: %w", location, err)
		}
		newBase := fmt.Sprintf("%s://%s", u.Scheme, u.Host)

		c.mu.Lock()
		c.leaderAddr = newBase
		c.mu.Unlock()

		return c.watchOnce(ctx, newBase, path)
	}

	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}

	ch := make(chan ChangeEvent, 64)
	go c.streamSSE(ctx, resp.Body, ch)
	return ch, nil
}

func (c *Client) streamSSE(ctx context.Context, body io.ReadCloser, ch chan<- ChangeEvent) {
	defer func() { _ = body.Close() }()
	defer close(ch)

	scanner := bufio.NewScanner(body)
	var current ChangeEvent

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			// Empty line marks end of one event.
			if current.Type != "" {
				select {
				case ch <- current:
				case <-ctx.Done():
					return
				}
				current = ChangeEvent{}
			}
			continue
		}
		field, value, ok := strings.Cut(line, ": ")
		if !ok {
			continue
		}
		switch field {
		case "event":
			current.Type = value
		case "data":
			// The data field is a JSON-encoded ChangeEvent; merge it into
			// current so the Type we set from "event:" is preserved.
			var ev ChangeEvent
			if err := json.Unmarshal([]byte(value), &ev); err == nil {
				ev.Type = current.Type
				current = ev
			}
		}
	}
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
	return errors.Is(err, ErrNotFound)
}
