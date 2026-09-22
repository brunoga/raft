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
	"strconv"
	"strings"

	"github.com/brunoga/raft/v2/examples/internal/exampleutil"
)

// Sentinel errors returned by the client.
var (
	// ErrNotFound is returned when the requested config key does not exist.
	ErrNotFound = errors.New("config key not found")

	// ErrNotLeader is returned when the request reaches a follower and no
	// redirect is available. This is transient; retrying usually succeeds.
	ErrNotLeader = exampleutil.ErrNotLeader

	// ErrRevisionMismatch is returned by a conditional write whose key has
	// been written since the revision given. Read it again, decide whether
	// the change still applies, and retry -- that loop is the whole point of
	// the condition.
	ErrRevisionMismatch = errors.New("config key has changed since the revision given")
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
	c := exampleutil.NewClient(addrs)
	c.ErrorMapper = func(status int, _ string) error {
		switch status {
		case http.StatusNotFound:
			return ErrNotFound
		case http.StatusPreconditionFailed:
			return ErrRevisionMismatch
		}
		return nil
	}
	return &Client{inner: c}
}

// Set upserts a config entry with an updated version timestamp. It is
// last-writer-wins: whatever was there is replaced.
func (c *Client) Set(ctx context.Context, key, value string) error {
	body, err := json.Marshal(map[string]string{"value": value})
	if err != nil {
		return err
	}
	return c.inner.Do(ctx, 0, http.MethodPut, "/configs/"+key, body, nil, isTerminal)
}

// SetIf writes a value only while the key is still at the revision given,
// which is the one [Client.GetRev] returned. It returns [ErrRevisionMismatch]
// if the key has been written since.
//
// This is what turns a read and a write into a read-modify-write that loses
// nothing: without it the two are separate log entries, and a write that
// lands between them is overwritten with no error.
//
// A revision of zero means the key has never been written, so SetIf(ctx, key,
// value, 0) is create-if-absent.
func (c *Client) SetIf(ctx context.Context, key, value string, rev uint64) error {
	body, err := json.Marshal(map[string]string{"value": value})
	if err != nil {
		return err
	}
	return c.inner.Do(ctx, 0, http.MethodPut, "/configs/"+key, body, nil, isTerminal,
		ifMatch(rev))
}

// DeleteIf removes a key only while it is still at the revision given.
func (c *Client) DeleteIf(ctx context.Context, key string, rev uint64) error {
	return c.inner.Do(ctx, 0, http.MethodDelete, "/configs/"+key, nil, nil, isTerminal,
		ifMatch(rev))
}

// ifMatch builds the conditional header for a revision. Zero is sent as
// If-None-Match: * -- "the key does not exist" -- because that is what a
// revision of zero means and what the server accepts for it.
func ifMatch(rev uint64) exampleutil.RequestOption {
	return func(r *http.Request) {
		if rev == 0 {
			r.Header.Set("If-None-Match", "*")
			return
		}
		r.Header.Set("If-Match", strconv.FormatUint(rev, 10))
	}
}

// Get retrieves a config entry. If stale is true it performs a local read from
// any node; otherwise it performs a linearizable read.
// Returns ErrNotFound if the key does not exist.
func (c *Client) Get(ctx context.Context, key string, stale bool) (ConfigEntry, error) {
	entry, _, err := c.GetRev(ctx, key, stale)
	return entry, err
}

// GetRev retrieves a config entry and the revision at which it was last
// written. Pass the revision to [Client.SetIf] or [Client.DeleteIf] to make
// the next write conditional on nothing having changed in between.
//
// The revision is the index of the Raft entry that wrote the key, which the
// server returns as an ETag. It is not the Version field on the entry: that
// is a timestamp this service chose to expose, and a timestamp is the wrong
// thing to compare against, since two writers in the same nanosecond get the
// same one.
//
// A stale read still returns a usable revision. It is either current or
// behind, and a conditional write on a revision that has moved is refused --
// so reading stale costs a retry, never a lost update.
func (c *Client) GetRev(ctx context.Context, key string, stale bool) (ConfigEntry, uint64, error) {
	path := "/configs/" + key
	if stale {
		path += "?consistency=stale"
	}

	var (
		entry ConfigEntry
		rev   uint64
		parse error
	)
	capture := exampleutil.ObserveResponse(func(resp *http.Response) {
		etag := strings.Trim(resp.Header.Get("ETag"), `"`)
		if etag == "" {
			return
		}
		parsed, err := strconv.ParseUint(etag, 10, 64)
		if err != nil {
			parse = fmt.Errorf("configsvc: unusable ETag %q: %w", etag, err)
			return
		}
		rev = parsed
	})

	if err := c.inner.Do(ctx, 0, http.MethodGet, path, nil, &entry, isTerminal, capture); err != nil {
		return ConfigEntry{}, 0, err
	}
	if parse != nil {
		return ConfigEntry{}, 0, parse
	}
	return entry, rev, nil
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
	// A failed condition is an answer, not a failure to reach the cluster.
	// Retrying it unchanged would fail identically every time, and the retry
	// that is wanted is a fresh read followed by a fresh decision -- which is
	// the caller's to make, not this loop's.
	return errors.Is(err, ErrNotFound) || errors.Is(err, ErrRevisionMismatch)
}
