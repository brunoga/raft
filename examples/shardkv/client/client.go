// Package client provides a high-level Go client for the shardkv service.
//
// The client performs client-side sharding using FNV-32a hashing to map keys
// to Raft groups, and automatically manages leader discovery and redirection
// independently for each shard. The leader address for each shard is cached
// and revalidated on failure.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

// Sentinel errors returned by the client.
var (
	// ErrKeyNotFound is returned when the requested key does not exist.
	ErrKeyNotFound = errors.New("key not found")

	// ErrNotLeader is returned when the request reaches a follower and no
	// redirect is available. This is transient; retrying usually succeeds.
	ErrNotLeader = errors.New("node is not the leader for this shard")
)

// ShardStatus is returned by Shards.
type ShardStatus struct {
	GroupID     uint64 `json:"group_id"`
	State       string `json:"state"`
	Term        uint64 `json:"term"`
	LastApplied uint64 `json:"last_applied"`
}

// Client is a thread-safe, high-level client for the shardkv service.
type Client struct {
	addrs     []string // seed node addresses, e.g. "http://host:8001"
	numShards uint64

	// httpClient does not follow redirects so we can intercept 307 responses
	// and update the cached leader address for the relevant shard.
	httpClient *http.Client

	mu          sync.RWMutex
	leaderAddrs map[uint64]string // shard group ID → cached leader address
}

// New returns a new shardkv client. addrs should be the HTTP addresses of one
// or more cluster nodes (e.g. "http://localhost:8001"). numShards must match
// the --shards value used when starting the cluster.
func New(addrs []string, numShards uint64) *Client {
	return &Client{
		addrs:     addrs,
		numShards: numShards,
		leaderAddrs: make(map[uint64]string),
		httpClient: &http.Client{
			// Disable automatic redirect following so we can intercept 307
			// responses and update the per-shard leader cache ourselves.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// Put sets key to value.
func (c *Client) Put(ctx context.Context, key, value string) error {
	gid := c.shardForKey(key)
	return c.do(ctx, gid, http.MethodPut, "/keys/"+key, []byte(value), nil)
}

// Get retrieves the value for key. Returns ErrKeyNotFound if the key does not
// exist.
func (c *Client) Get(ctx context.Context, key string) (string, error) {
	gid := c.shardForKey(key)
	var buf bytes.Buffer
	if err := c.do(ctx, gid, http.MethodGet, "/keys/"+key, nil, &buf); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// Delete removes key. Returns ErrKeyNotFound if the key does not exist.
func (c *Client) Delete(ctx context.Context, key string) error {
	gid := c.shardForKey(key)
	return c.do(ctx, gid, http.MethodDelete, "/keys/"+key, nil, nil)
}

// Shards returns the status of all shards as reported by the given node
// address. Any node can serve this request; it does not require the leader.
func (c *Client) Shards(ctx context.Context, addr string) ([]ShardStatus, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(addr, "/")+"/shards", http.NoBody)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		msg := strings.TrimSpace(string(errBody))
		if msg != "" {
			return nil, fmt.Errorf("server error %d: %s", resp.StatusCode, msg)
		}
		return nil, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}

	var res []ShardStatus
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return nil, fmt.Errorf("decode shards response: %w", err)
	}
	return res, nil
}

// shardForKey maps a key to its shard group ID using FNV-32a hashing.
func (c *Client) shardForKey(key string) uint64 {
	h := fnv.New32a()
	_, _ = io.WriteString(h, key)
	return uint64(h.Sum32())%c.numShards + 1
}

// do executes a request for the given shard, trying the cached shard leader
// first and falling back to seed nodes. It refreshes the leader cache on 307
// redirects and invalidates it when the cached leader stops responding.
func (c *Client) do(ctx context.Context, gid uint64, method, path string, body []byte, result any) error {
	// 1. Try the cached leader for this shard.
	c.mu.RLock()
	leader := c.leaderAddrs[gid]
	c.mu.RUnlock()

	if leader != "" {
		err := c.doOnce(ctx, gid, leader, method, path, body, result, false)
		if err == nil {
			return nil
		}
		if isTerminal(err) {
			return err
		}
		// Leader is stale (down or re-elected). Clear the per-shard cache.
		c.mu.Lock()
		if c.leaderAddrs[gid] == leader {
			delete(c.leaderAddrs, gid)
		}
		c.mu.Unlock()
	}

	// 2. Fan out to seed nodes until one succeeds.
	var lastErr error
	for _, addr := range c.addrs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		err := c.doOnce(ctx, gid, addr, method, path, body, result, false)
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

// doOnce performs a single HTTP request against addr for the given shard. If
// the server responds with a 307 redirect and followed is false, it updates
// the per-shard leader cache and retries once. The followed flag prevents
// redirect loops.
//
// result may be *bytes.Buffer (raw body copy) or any JSON-decodable type.
func (c *Client) doOnce(ctx context.Context, gid uint64, addr, method, path string, body []byte, result any, followed bool) error {
	urlStr := strings.TrimSuffix(addr, "/") + path

	var reqBody io.Reader
	if body != nil {
		reqBody = bytes.NewReader(body)
	}

	req, err := http.NewRequestWithContext(ctx, method, urlStr, reqBody)
	if err != nil {
		return err
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
		c.leaderAddrs[gid] = newBase
		c.mu.Unlock()

		return c.doOnce(ctx, gid, newBase, method, path, body, result, true)
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

	// Read the response body for a meaningful error message (capped to avoid
	// consuming runaway responses).
	errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	msg := strings.TrimSpace(string(errBody))

	switch resp.StatusCode {
	case http.StatusNotFound:
		return ErrKeyNotFound
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
	return errors.Is(err, ErrKeyNotFound)
}
