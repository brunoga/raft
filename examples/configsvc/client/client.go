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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/brunoga/raft/examples/internal/exampleutil"
)

// Sentinel errors returned by the client.
var (
	// ErrNotFound is returned when the requested config key does not exist.
	ErrNotFound = errors.New("config key not found")

	// ErrNotLeader is returned when the request reaches a follower and no
	// redirect is available. This is transient; retrying usually succeeds.
	ErrNotLeader = exampleutil.ErrNotLeader
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
	inner *exampleutil.Client
}

// New returns a new configsvc client. addrs should be the HTTP addresses of
// one or more cluster nodes (e.g. "http://localhost:8001"). At least one
// reachable address is required; the rest serve as fallbacks.
func New(addrs []string) *Client {
	return &Client{
		inner: exampleutil.NewClient(addrs),
	}
}

// Set upserts a config entry with an updated version timestamp.
func (c *Client) Set(ctx context.Context, key, value string) error {
	body, err := json.Marshal(map[string]string{"value": value})
	if err != nil {
		return err
	}
	return c.inner.Do(ctx, 0, http.MethodPut, "/configs/"+key, body, nil, isTerminal)
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
	if err := c.inner.Do(ctx, 0, http.MethodGet, path, nil, &entry, isTerminal); err != nil {
		return ConfigEntry{}, err
	}
	return entry, nil
}

// Delete removes a config entry. Returns ErrNotFound if the key does not exist.
func (c *Client) Delete(ctx context.Context, key string) error {
	return c.inner.Do(ctx, 0, http.MethodDelete, "/configs/"+key, nil, nil, isTerminal)
}

// List retrieves all config entries. If stale is true it performs a local read
// from any node; otherwise it performs a linearizable read.
func (c *Client) List(ctx context.Context, stale bool) (map[string]ConfigEntry, error) {
	path := "/configs"
	if stale {
		path += "?consistency=stale"
	}
	var all map[string]ConfigEntry
	if err := c.inner.Do(ctx, 0, http.MethodGet, path, nil, &all, isTerminal); err != nil {
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
	leader := c.inner.GetLeader(0)
	if leader != "" {
		ch, err := c.watchOnce(ctx, leader, path)
		if err == nil {
			return ch, nil
		}
	}

	for _, addr := range c.inner.Addrs {
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

	resp, err := c.inner.HTTPClient.Do(req)
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

		c.inner.SetLeader(0, newBase)
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

// isTerminal reports whether err should stop the retry loop immediately.
// Domain errors are terminal; network errors and ErrNotLeader are retried.
func isTerminal(err error) bool {
	return errors.Is(err, ErrNotFound)
}
