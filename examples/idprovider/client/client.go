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
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync"

	"github.com/brunoga/raft/examples/internal/exampleutil"
)

// Sentinel errors returned by the client.
var (
	// ErrDomainNotFound is returned when the requested domain does not exist.
	ErrDomainNotFound = errors.New("domain not found")

	// ErrNotLeader is returned when the request reaches a follower and no
	// redirect is available. This is transient; retrying usually succeeds.
	ErrNotLeader = exampleutil.ErrNotLeader
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
	inner    *exampleutil.Client
	clientID string // stable identifier used for idempotency headers

	mu     sync.Mutex
	seqNum uint64 // monotonically increasing, protected by mu
}

// New returns a new idprovider client. addrs should be the HTTP addresses of
// one or more cluster nodes (e.g. "http://localhost:8001"). clientID is a
// stable, unique identifier for this client instance used for idempotent
// exactly-once semantics; it must not be empty.
func New(addrs []string, clientID string) *Client {
	return &Client{
		inner:    exampleutil.NewClient(addrs),
		clientID: clientID,
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
	var opts []exampleutil.RequestOption
	if idempotent {
		c.mu.Lock()
		c.seqNum++
		seq := c.seqNum
		c.mu.Unlock()

		opts = append(opts, func(req *http.Request) {
			if c.clientID != "" {
				req.Header.Set("X-Client-ID", c.clientID)
				req.Header.Set("X-Seq-Num", strconv.FormatUint(seq, 10))
			}
		})
	}

	return c.inner.Do(ctx, 0, method, path, body, result, isTerminal, opts...)
}

// isTerminal reports whether err should stop the retry loop immediately.
// Domain errors are terminal; network errors and ErrNotLeader are retried.
func isTerminal(err error) bool {
	return errors.Is(err, ErrDomainNotFound)
}
