// Package client provides a high-level Go client for the idprovider service.
// It handles leader discovery and provides exactly-once semantics for ID
// allocation using stable client identifiers and sequence numbers.
package client

import (
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

var (
	ErrDomainNotFound = errors.New("domain not found")
	ErrNotLeader      = errors.New("node is not the leader")
)

// AllocResult is the response from a Next ID allocation.
type AllocResult struct {
	Start uint64 `json:"start"`
	Count uint64 `json:"count"`
}

// DomainInfo is returned by Current.
type DomainInfo struct {
	Domain  string `json:"domain"`
	Current uint64 `json:"current"`
}

// Client is a high-level client for the idprovider service.
type Client struct {
	addrs      []string
	httpClient *http.Client

	mu         sync.RWMutex
	leaderAddr string

	clientID string
	seqNum   uint64
}

// New returns a new idprovider client. clientID is used for idempotent operations.
func New(addrs []string, clientID string) *Client {
	return &Client{
		addrs: addrs,
		httpClient: &http.Client{
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		clientID: clientID,
	}
}

// CreateDomain creates a new ID domain.
func (c *Client) CreateDomain(ctx context.Context, domain string) error {
	return c.doWithRetry(ctx, http.MethodPost, "/domains/"+domain, nil, nil, true)
}

// DeleteDomain removes a domain.
func (c *Client) DeleteDomain(ctx context.Context, domain string) error {
	return c.doWithRetry(ctx, http.MethodDelete, "/domains/"+domain, nil, nil, true)
}

// ListDomains returns all domains and their current counters.
func (c *Client) ListDomains(ctx context.Context, stale bool) (map[string]uint64, error) {
	path := "/domains"
	if stale {
		path += "?consistency=stale"
	}
	var res map[string]uint64
	err := c.doWithRetry(ctx, http.MethodGet, path, nil, &res, false)
	return res, err
}

// Next allocates count IDs from a domain.
func (c *Client) Next(ctx context.Context, domain string, count uint64) (AllocResult, error) {
	path := fmt.Sprintf("/domains/%s/next?count=%d", domain, count)
	var res AllocResult
	err := c.doWithRetry(ctx, http.MethodPost, path, nil, &res, true)
	return res, err
}

// Current returns the current high-water mark for a domain.
func (c *Client) Current(ctx context.Context, domain string, stale bool) (DomainInfo, error) {
	path := "/domains/" + domain + "/current"
	if stale {
		path += "?consistency=stale"
	}
	var res DomainInfo
	err := c.doWithRetry(ctx, http.MethodGet, path, nil, &res, false)
	return res, err
}

func (c *Client) doWithRetry(ctx context.Context, method, path string, body io.Reader, result any, idempotent bool) error {
	c.mu.RLock()
	leader := c.leaderAddr
	c.mu.RUnlock()

	// If idempotent, we increment seqNum once for the whole retry loop.
	// This ensures that all retries use the SAME seqNum.
	var currentSeq uint64
	if idempotent {
		c.mu.Lock()
		c.seqNum++
		currentSeq = c.seqNum
		c.mu.Unlock()
	}

	if leader != "" {
		err := c.doOnce(ctx, leader, method, path, body, result, currentSeq)
		if err == nil {
			return nil
		}
		if errors.Is(err, ErrDomainNotFound) {
			return err
		}
	}

	var lastErr error
	for _, addr := range c.addrs {
		err := c.doOnce(ctx, addr, method, path, body, result, currentSeq)
		if err == nil {
			return nil
		}
		if errors.Is(err, ErrDomainNotFound) {
			return err
		}
		lastErr = err
	}

	return lastErr
}

func (c *Client) doOnce(ctx context.Context, addr, method, path string, body io.Reader, result any, seq uint64) error {
	urlStr := strings.TrimSuffix(addr, "/") + path
	req, err := http.NewRequestWithContext(ctx, method, urlStr, body)
	if err != nil {
		return err
	}

	if c.clientID != "" && seq > 0 {
		req.Header.Set("X-Client-ID", c.clientID)
		req.Header.Set("X-Seq-Num", strconv.FormatUint(seq, 10))
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

		return c.doOnce(ctx, newBase, method, path, body, result, seq)
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if result != nil {
			return json.NewDecoder(resp.Body).Decode(result)
		}
		return nil
	}

	if resp.StatusCode == http.StatusNotFound {
		return ErrDomainNotFound
	}

	if resp.StatusCode == http.StatusServiceUnavailable {
		return ErrNotLeader
	}

	return fmt.Errorf("unexpected status: %s", resp.Status)
}
