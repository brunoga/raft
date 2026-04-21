// Package client provides a high-level Go client for the configsvc example.
// It handles cluster bootstrapping, leader discovery, and provides a type-safe
// API for configuration management and real-time watches.
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

// ConfigEntry is the value stored in the "configs" collection.
type ConfigEntry struct {
	Value   string `json:"value"`
	Version int64  `json:"version"`
}

// ChangeEvent is delivered to subscribers when a config entry changes.
type ChangeEvent struct {
	Key     string       `json:"key"`
	Value   *ConfigEntry `json:"value,omitempty"`
	Deleted bool         `json:"deleted,omitempty"`
	Type    string       `json:"type"` // "snapshot", "change", or "delete"
}

var (
	ErrNotFound  = errors.New("config key not found")
	ErrNotLeader = errors.New("node is not the leader")
)

// Client is a high-level client for the configsvc.
type Client struct {
	addrs      []string
	httpClient *http.Client

	mu         sync.RWMutex
	leaderAddr string
}

// New returns a new configsvc client. addrs should be a list of HTTP addresses
// of the nodes in the cluster (e.g., "http://localhost:8001").
func New(addrs []string) *Client {
	return &Client{
		addrs: addrs,
		httpClient: &http.Client{
			// Don't follow redirects automatically. We want to catch the 307
			// ourselves so we can update the cached leaderAddr.
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// Set upserts a config entry.
func (c *Client) Set(ctx context.Context, key, value string) error {
	body, err := json.Marshal(map[string]string{"value": value})
	if err != nil {
		return err
	}

	return c.doWithRetry(ctx, http.MethodPut, "/configs/"+key, bytes.NewReader(body), nil)
}

// Get retrieves a config entry. If consistency is "stale", it performs a local
// read from any node. Otherwise, it performs a linearizable read.
func (c *Client) Get(ctx context.Context, key string, stale bool) (ConfigEntry, error) {
	path := "/configs/" + key
	if stale {
		path += "?consistency=stale"
	}

	var entry ConfigEntry
	err := c.doWithRetry(ctx, http.MethodGet, path, nil, &entry)
	return entry, err
}

// Delete removes a config entry.
func (c *Client) Delete(ctx context.Context, key string) error {
	return c.doWithRetry(ctx, http.MethodDelete, "/configs/"+key, nil, nil)
}

// List retrieves all config entries.
func (c *Client) List(ctx context.Context, stale bool) (map[string]ConfigEntry, error) {
	path := "/configs"
	if stale {
		path += "?consistency=stale"
	}

	var all map[string]ConfigEntry
	err := c.doWithRetry(ctx, http.MethodGet, path, nil, &all)
	return all, err
}

// Watch subscribes to changes for a given key (or all keys if key is empty).
// It returns a channel of ChangeEvents and an error if the connection fails.
// The channel is closed when the context is cancelled or the connection is lost.
func (c *Client) Watch(ctx context.Context, key string) (<-chan ChangeEvent, error) {
	path := "/watch"
	if key != "" {
		path += "/" + key
	}

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
	urlStr := strings.TrimSuffix(addr, "/") + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlStr, nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode == http.StatusTemporaryRedirect {
		resp.Body.Close()
		location := resp.Header.Get("Location")
		if location == "" {
			return nil, errors.New("redirect without location")
		}

		u, err := url.Parse(location)
		if err != nil {
			return nil, fmt.Errorf("invalid redirect location: %w", err)
		}
		newBase := fmt.Sprintf("%s://%s", u.Scheme, u.Host)

		c.mu.Lock()
		c.leaderAddr = newBase
		c.mu.Unlock()

		return c.watchOnce(ctx, newBase, path)
	}

	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("unexpected status: %s", resp.Status)
	}

	ch := make(chan ChangeEvent, 64)
	go c.streamSSE(ctx, resp.Body, ch)

	return ch, nil
}

func (c *Client) streamSSE(ctx context.Context, body io.ReadCloser, ch chan<- ChangeEvent) {
	defer body.Close()
	defer close(ch)

	scanner := bufio.NewScanner(body)
	var currentEvent ChangeEvent

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			// Empty line marks the end of an event.
			if currentEvent.Type != "" {
				select {
				case ch <- currentEvent:
				case <-ctx.Done():
					return
				}
				currentEvent = ChangeEvent{}
			}
			continue
		}

		parts := strings.SplitN(line, ": ", 2)
		if len(parts) < 2 {
			continue
		}

		key, value := parts[0], parts[1]
		switch key {
		case "event":
			currentEvent.Type = value
		case "data":
			if err := json.Unmarshal([]byte(value), &currentEvent); err != nil {
				// Log or handle error? For now just skip.
				continue
			}
		}
	}
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

	// 1. Try the cached leader first.
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
		// Leader might have changed or node is down.
	}

	// 2. Fall back to seed nodes.
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

		// Extract base URL from redirect location.
		u, err := url.Parse(location)
		if err != nil {
			return fmt.Errorf("invalid redirect location: %w", err)
		}
		newBase := fmt.Sprintf("%s://%s", u.Scheme, u.Host)

		c.mu.Lock()
		c.leaderAddr = newBase
		c.mu.Unlock()

		// Immediate retry against the new leader.
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
