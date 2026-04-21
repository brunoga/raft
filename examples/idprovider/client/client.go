// Package client provides a high-level Go client for the idprovider service.
//
// The client handles leader discovery and automatic redirection transparently.
// Any node address can be used as a seed; write requests are routed to the
// current leader and the leader address is cached and revalidated on failure.
//
// Idempotency: the client tracks a monotonically increasing sequence number
// per instance. Each mutating request carries X-Client-ID and X-Seq-Num
// headers so the server can deduplicate retries. If the network drops a reply
// after the proposal commits, retrying with the same (clientID, seq) returns
// the original result without re-executing the mutation.
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
	"strconv"
	"strings"
	"sync"
)

// Sentinel errors returned by the client.
var (
	// ErrDomainNotFound is returned when the requested domain does not exist.
	ErrDomainNotFound = errors.New("domain not found")

	// ErrNotLeader is returned when the request reaches a follower and no
	// redirect is available. This is transient; retrying usually succeeds.
	ErrNotLeader = errors.New("node is not the leader")
)

// AllocResult is returned by Next.
type AllocResult struct {
	Start uint64 `json:"start"`
	Count uint64 `json:"count"`
}

// DomainInfo is returned by Current.
type DomainInfo struct {
	Domain  string `json:"domain"`
	Current uint64 `json:"current"`
}

// Client is a thread-safe, high-level client for the idprovider service.
type Client struct {
	addrs    []string // seed node addresses, e.g. "http://host:8001"
	clientID string   // stable identifier used for idempotency headers

	// httpClient does not follow redirects so we can intercept 307 responses
	// and update the cached leader address ourselves.
	httpClient *http.Client

	mu         sync.Mutex
	leaderAddr string // cached leader; empty means unknown
	seqNum     uint64 // monotonically increasing, protected by mu
}

// New returns a new idprovider client. addrs should be the HTTP addresses of
// one or more cluster nodes (e.g. "http://localhost:8001"). clientID is a
// stable, unique identifier for this client instance used for idempotent
// exactly-once semantics; it must not be empty.
func New(addrs []string, clientID string) *Client {
	return &Client{
		addrs:    addrs,
		clientID: clientID,
		httpClient: &http.Client{
			// Disable automatic redirect following so we can intercept 307
			// responses and update the cached leader address ourselves.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// CreateDomain creates a new ID domain. The operation is idempotent: calling
// it again with an already-existing domain name succeeds without error.
func (c *Client) CreateDomain(ctx context.Context, domain string) error {
	return c.do(ctx, http.MethodPost, "/domains/"+domain, nil, nil, true)
}

// DeleteDomain removes a domain. Returns ErrDomainNotFound if it does not
// exist.
func (c *Client) DeleteDomain(ctx context.Context, domain string) error {
	return c.do(ctx, http.MethodDelete, "/domains/"+domain, nil, nil, true)
}

// ListDomains returns all domains and their current counters. If stale is
// true it performs a local read from any node; otherwise linearizable.
func (c *Client) ListDomains(ctx context.Context, stale bool) (map[string]uint64, error) {
	path := "/domains"
	if stale {
		path += "?consistency=stale"
	}
	var res map[string]uint64
	if err := c.do(ctx, http.MethodGet, path, nil, &res, false); err != nil {
		return nil, err
	}
	return res, nil
}

// Next allocates count IDs from the named domain and returns the range
// [Start, Start+Count). Returns ErrDomainNotFound if the domain does not
// exist. The allocation is idempotent: if the reply is lost the caller can
// retry with the same client instance; the server returns the same range.
func (c *Client) Next(ctx context.Context, domain string, count uint64) (AllocResult, error) {
	path := fmt.Sprintf("/domains/%s/next?count=%d", domain, count)
	var res AllocResult
	if err := c.do(ctx, http.MethodPost, path, nil, &res, true); err != nil {
		return AllocResult{}, err
	}
	return res, nil
}

// Current returns the current high-water mark for a domain. All IDs ≤
// Current have already been allocated. Returns ErrDomainNotFound if the
// domain does not exist. If stale is true it performs a local read.
func (c *Client) Current(ctx context.Context, domain string, stale bool) (DomainInfo, error) {
	path := "/domains/" + domain + "/current"
	if stale {
		path += "?consistency=stale"
	}
	var res DomainInfo
	if err := c.do(ctx, http.MethodGet, path, nil, &res, false); err != nil {
		return DomainInfo{}, err
	}
	return res, nil
}

// do executes a request, trying the cached leader first and falling back to
// seed nodes. Idempotent operations carry X-Client-ID / X-Seq-Num headers;
// the sequence number is incremented once per logical call (not per retry).
func (c *Client) do(ctx context.Context, method, path string, body []byte, result any, idempotent bool) error {
	// Increment the sequence number exactly once per logical call so all
	// retries of the same call share the same seq.
	var seq uint64
	if idempotent {
		c.mu.Lock()
		c.seqNum++
		seq = c.seqNum
		c.mu.Unlock()
	}

	// 1. Try the cached leader first.
	c.mu.Lock()
	leader := c.leaderAddr
	c.mu.Unlock()

	if leader != "" {
		err := c.doOnce(ctx, leader, method, path, body, result, seq, false)
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
		err := c.doOnce(ctx, addr, method, path, body, result, seq, false)
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

// doOnce performs a single HTTP request against addr. Idempotency headers are
// set when seq > 0. If the server responds with a 307 redirect and followed is
// false, it updates the leader cache and retries once. The followed flag
// prevents redirect loops.
func (c *Client) doOnce(ctx context.Context, addr, method, path string, body []byte, result any, seq uint64, followed bool) error {
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
	if seq > 0 && c.clientID != "" {
		req.Header.Set("X-Client-ID", c.clientID)
		req.Header.Set("X-Seq-Num", strconv.FormatUint(seq, 10))
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

		return c.doOnce(ctx, newBase, method, path, body, result, seq, true)
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
		return ErrDomainNotFound
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
	return errors.Is(err, ErrDomainNotFound)
}
