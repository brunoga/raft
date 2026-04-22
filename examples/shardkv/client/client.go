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
	"strings"

	"github.com/brunoga/raft/examples/internal/exampleutil"
)

// Sentinel errors returned by the client.
var (
	// ErrKeyNotFound is returned when the requested key does not exist.
	ErrKeyNotFound = errors.New("key not found")

	// ErrNotLeader is returned when the request reaches a follower and no
	// redirect is available. This is transient; retrying usually succeeds.
	ErrNotLeader = exampleutil.ErrNotLeader
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
	inner     *exampleutil.Client
	numShards uint64
}

// New returns a new shardkv client. addrs should be the HTTP addresses of one
// or more cluster nodes (e.g. "http://localhost:8001"). numShards must match
// the --shards value used when starting the cluster.
func New(addrs []string, numShards uint64) *Client {
	c := exampleutil.NewClient(addrs)
	c.ErrorMapper = func(status int, _ string) error {
		if status == http.StatusNotFound {
			return ErrKeyNotFound
		}
		return nil
	}
	return &Client{inner: c, numShards: numShards}
}

// Put sets key to value.
func (c *Client) Put(ctx context.Context, key, value string) error {
	gid := c.shardForKey(key)
	return c.inner.Do(ctx, gid, http.MethodPut, "/keys/"+key, []byte(value), nil, isTerminal)
}

// Get retrieves the value for key. Returns ErrKeyNotFound if the key does not
// exist.
func (c *Client) Get(ctx context.Context, key string) (string, error) {
	gid := c.shardForKey(key)
	var buf bytes.Buffer
	if err := c.inner.Do(ctx, gid, http.MethodGet, "/keys/"+key, nil, &buf, isTerminal); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// Delete removes key. Returns ErrKeyNotFound if the key does not exist.
func (c *Client) Delete(ctx context.Context, key string) error {
	gid := c.shardForKey(key)
	return c.inner.Do(ctx, gid, http.MethodDelete, "/keys/"+key, nil, nil, isTerminal)
}

// Shards returns the status of all shards as reported by the given node
// address. Any node can serve this request; it does not require the leader.
func (c *Client) Shards(ctx context.Context, addr string) ([]ShardStatus, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(addr, "/")+"/shards", http.NoBody)
	if err != nil {
		return nil, err
	}
	resp, err := c.inner.HTTPClient.Do(req)
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

// isTerminal reports whether err should stop the retry loop immediately.
// Domain errors are terminal; network errors and ErrNotLeader are retried.
func isTerminal(err error) bool {
	return errors.Is(err, ErrKeyNotFound)
}
