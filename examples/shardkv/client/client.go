// Package client provides a high-level Go client for the shardkv service.
// It handles client-side sharding using FNV-32a hashing to map keys to Raft
// groups, and automatically manages leader discovery and redirection for each
// individual shard.
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

var (
	ErrKeyNotFound = errors.New("key not found")
	ErrNotLeader   = errors.New("node is not the leader for this shard")
)

// ShardStatus is returned by Shards.
type ShardStatus struct {
	GroupID     uint64 `json:"group_id"`
	State       string `json:"state"`
	Term        uint64 `json:"term"`
	LastApplied uint64 `json:"last_applied"`
}

// Client is a high-level client for the shardkv service.
type Client struct {
	addrs      []string // Physical node addresses
	httpClient *http.Client

	numShards uint64

	mu         sync.RWMutex
	leaderAddr map[uint64]string // shard gid -> physical node addr
}

// New returns a new shardkv client.
func New(addrs []string, numShards uint64) *Client {
	return &Client{
		addrs:      addrs,
		numShards:  numShards,
		leaderAddr: make(map[uint64]string),
		httpClient: &http.Client{
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// Put sets a key to the given value.
func (c *Client) Put(ctx context.Context, key, value string) error {
	gid := c.shardForKey(key)
	return c.doWithRetry(ctx, gid, http.MethodPut, "/keys/"+key, bytes.NewReader([]byte(value)), nil)
}

// Get retrieves a key's value.
func (c *Client) Get(ctx context.Context, key string) (string, error) {
	gid := c.shardForKey(key)
	var body bytes.Buffer
	err := c.doWithRetry(ctx, gid, http.MethodGet, "/keys/"+key, nil, &body)
	if err != nil {
		return "", err
	}
	return body.String(), nil
}

// Delete removes a key.
func (c *Client) Delete(ctx context.Context, key string) error {
	gid := c.shardForKey(key)
	return c.doWithRetry(ctx, gid, http.MethodDelete, "/keys/"+key, nil, nil)
}

// Shards returns the status of all shards on a physical node.
func (c *Client) Shards(ctx context.Context, addr string) ([]ShardStatus, error) {
	var res []ShardStatus
	err := c.doWithRetryRaw(ctx, addr, http.MethodGet, "/shards", nil, &res)
	return res, err
}

func (c *Client) shardForKey(key string) uint64 {
	h := fnv.New32a()
	_, _ = io.WriteString(h, key)
	return uint64(h.Sum32())%c.numShards + 1
}

func (c *Client) doWithRetry(ctx context.Context, gid uint64, method, path string, body io.Reader, result any) error {
	var bodyBytes []byte
	if body != nil {
		var err error
		bodyBytes, err = io.ReadAll(body)
		if err != nil {
			return err
		}
	}

	c.mu.RLock()
	leader := c.leaderAddr[gid]
	c.mu.RUnlock()

	if leader != "" {
		err := c.doOnce(ctx, gid, leader, method, path, bodyBytes, result)
		if err == nil {
			return nil
		}
		if errors.Is(err, ErrKeyNotFound) {
			return err
		}
	}

	var lastErr error
	for _, addr := range c.addrs {
		err := c.doOnce(ctx, gid, addr, method, path, bodyBytes, result)
		if err == nil {
			return nil
		}
		if errors.Is(err, ErrKeyNotFound) {
			return err
		}
		lastErr = err
	}

	return lastErr
}

func (c *Client) doOnce(ctx context.Context, gid uint64, addr, method, path string, bodyBytes []byte, result any) error {
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
		c.leaderAddr[gid] = newBase
		c.mu.Unlock()

		return c.doOnce(ctx, gid, newBase, method, path, bodyBytes, result)
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if result != nil {
			if w, ok := result.(io.Writer); ok {
				_, err = io.Copy(w, resp.Body)
				return err
			}
			return json.NewDecoder(resp.Body).Decode(result)
		}
		return nil
	}

	if resp.StatusCode == http.StatusNotFound {
		return ErrKeyNotFound
	}

	if resp.StatusCode == http.StatusServiceUnavailable {
		return ErrNotLeader
	}

	return fmt.Errorf("unexpected status: %s", resp.Status)
}

func (c *Client) doWithRetryRaw(ctx context.Context, addr, method, path string, body io.Reader, result any) error {
	urlStr := strings.TrimSuffix(addr, "/") + path
	req, err := http.NewRequestWithContext(ctx, method, urlStr, body)
	if err != nil {
		return err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if result != nil {
			return json.NewDecoder(resp.Body).Decode(result)
		}
		return nil
	}

	return fmt.Errorf("unexpected status: %s", resp.Status)
}
