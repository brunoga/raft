// Package client talks to an easyraft cluster over HTTP, from a process that
// is not part of it.
//
// The HTTP API is small enough to call with net/http directly, and doing so
// means reimplementing the same four things in every service: find the
// leader, follow it when it moves, decide which failures are worth retrying,
// and turn status codes back into errors worth branching on. This package
// does those four things and nothing else.
//
//	c, err := client.New(client.WithEndpoints("node1:8001", "node2:8001", "node3:8001"))
//	if err != nil {
//		return err
//	}
//	users := client.Collection[User](c, "users")
//
//	if err := users.Create(ctx, "alice", User{Name: "Alice"}); err != nil {
//		return err
//	}
//
// # Errors
//
// Failures come back as the same values an in-process caller sees --
// [easyraft.ErrKeyNotFound], [easyraft.ErrKeyExists],
// [easyraft.ErrRevisionMismatch], [easyraft.ErrLeaseNotFound] and the rest --
// so code that moves between the two does not change which errors it tests
// for. Anything unrecognised arrives as an [Error] carrying the status and
// the body.
//
// # Retries
//
// A write that gets no answer is the hard case: the caller cannot tell a
// request that never arrived from a response that was lost, and retrying
// blindly can apply it twice. This package retries only what is safe:
//
//   - reads, always;
//   - writes that name an exactly-once [easyraft.OnceID], which the cluster
//     deduplicates -- see [Client.Exactly];
//   - writes that are idempotent by construction: a conditional write, whose
//     second attempt is refused with [easyraft.ErrRevisionMismatch] rather
//     than applied again.
//
// An ordinary Create or Mutate with no identity is attempted once against
// the leader. A redirect to a new leader is not a retry and is always
// followed.
package client

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Error is a response this package could not turn into a known error.
type Error struct {
	// Status is the HTTP status code.
	Status int
	// Message is whatever the server said, trimmed to something loggable.
	Message string
	// Endpoint is the node that answered.
	Endpoint string
}

func (e *Error) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("easyraft/client: %s answered %d %s",
			e.Endpoint, e.Status, http.StatusText(e.Status))
	}
	return fmt.Sprintf("easyraft/client: %s answered %d: %s", e.Endpoint, e.Status, e.Message)
}

// ErrNoEndpoint is returned when every endpoint failed. It wraps the last
// failure, so errors.Is still finds what went wrong.
var ErrNoEndpoint = errors.New("easyraft/client: no endpoint could serve the request")

// Client is a connection to an easyraft cluster. It is safe for concurrent
// use, and holds no resources that must be released.
type Client struct {
	endpoints []string
	http      *http.Client
	token     string
	group     string // "/groups/N" prefix, empty for a single-group Store
	attempts  int
	backoff   time.Duration

	// leader is the endpoint that most recently answered as leader, tried
	// first next time. A hint, never a fact: it is corrected by the redirect
	// that proves it wrong.
	mu     sync.RWMutex
	leader string
}

// Option configures a [Client].
type Option func(*Client)

// WithEndpoints sets the addresses of cluster members. Give as many as are
// known: any of them can be asked, and the one that is leader is found from
// there. An address may be a bare host:port, in which case it is reached over
// http, or a full URL.
func WithEndpoints(addrs ...string) Option {
	return func(c *Client) { c.endpoints = append(c.endpoints, addrs...) }
}

// WithHTTPClient replaces the underlying [http.Client].
//
// The replacement must not follow redirects itself: a 307 to the leader is
// how this package learns where the leader is, and a client that follows it
// silently would leave the next request going back to a follower.
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) { c.http = h }
}

// WithBearerToken sends token as the Authorization header, matching
// [easyraft.WithBearerTokenAuth] on the server.
func WithBearerToken(token string) Option {
	return func(c *Client) { c.token = "Bearer " + token }
}

// WithCredential sends credential as the Authorization header verbatim, for a
// server whose [easyraft.WithHTTPAuth] hook expects something other than a
// bearer token.
func WithCredential(credential string) Option {
	return func(c *Client) { c.token = credential }
}

// WithTLS dials endpoints over https with cfg.
func WithTLS(cfg *tls.Config) Option {
	return func(c *Client) {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.TLSClientConfig = cfg
		c.http = &http.Client{
			Transport:     transport,
			Timeout:       c.http.Timeout,
			CheckRedirect: noRedirect,
		}
	}
}

// WithGroup addresses one group of a [easyraft.Manager] rather than a
// single-group [easyraft.Store].
func WithGroup(groupID uint64) Option {
	return func(c *Client) { c.group = "/groups/" + strconv.FormatUint(groupID, 10) }
}

// WithTimeout bounds each individual HTTP request. It is not a bound on a
// call: a call that is retried spends this on each attempt, and the context
// the caller passes is what bounds the whole thing.
func WithTimeout(d time.Duration) Option {
	return func(c *Client) { c.http.Timeout = d }
}

// WithRetry sets how many times a retryable request is attempted and the
// delay between attempts, which doubles up to a second. Attempts below one
// mean one attempt. The default is four attempts starting at 50ms.
func WithRetry(attempts int, initialBackoff time.Duration) Option {
	return func(c *Client) {
		c.attempts = attempts
		c.backoff = initialBackoff
	}
}

// noRedirect stops the HTTP client following a redirect on its own, because
// a 307 carries the information this package exists to track.
func noRedirect(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}

// New returns a Client. [WithEndpoints] is required.
func New(opts ...Option) (*Client, error) {
	c := &Client{
		http:     &http.Client{Timeout: 10 * time.Second, CheckRedirect: noRedirect},
		attempts: 4,
		backoff:  50 * time.Millisecond,
	}
	for _, o := range opts {
		o(c)
	}
	if len(c.endpoints) == 0 {
		return nil, errors.New("easyraft/client: WithEndpoints is required")
	}

	normalized := make([]string, 0, len(c.endpoints))
	for _, e := range c.endpoints {
		n, err := normalizeEndpoint(e)
		if err != nil {
			return nil, err
		}
		normalized = append(normalized, n)
	}
	c.endpoints = normalized

	if c.http.CheckRedirect == nil {
		// A supplied client that follows redirects would hide the leader
		// change this package is here to notice.
		c.http.CheckRedirect = noRedirect
	}
	if c.attempts < 1 {
		c.attempts = 1
	}
	if c.backoff <= 0 {
		c.backoff = 50 * time.Millisecond
	}
	return c, nil
}

// normalizeEndpoint turns an address into a scheme://host:port base URL, and
// refuses anything that is not one. An endpoint with a path or a query would
// silently prefix or truncate every request built from it.
func normalizeEndpoint(addr string) (string, error) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return "", errors.New("easyraft/client: empty endpoint")
	}
	if !strings.Contains(addr, "://") {
		addr = "http://" + addr
	}
	u, err := url.Parse(addr)
	if err != nil {
		return "", fmt.Errorf("easyraft/client: endpoint %q: %w", addr, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("easyraft/client: endpoint %q: scheme must be http or https", addr)
	}
	if u.Host == "" {
		return "", fmt.Errorf("easyraft/client: endpoint %q names no host", addr)
	}
	if u.User != nil || u.Path != "" && u.Path != "/" || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("easyraft/client: endpoint %q must be a bare host:port or scheme://host:port", addr)
	}
	return u.Scheme + "://" + u.Host, nil
}

// normalizeOrigin reduces an absolute URL to scheme://host:port, dropping
// everything a redirect might have carried beyond where to go.
func normalizeOrigin(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("unusable address %q: %w", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("address %q: scheme must be http or https", raw)
	}
	if u.Host == "" {
		return "", fmt.Errorf("address %q names no host", raw)
	}
	return u.Scheme + "://" + u.Host, nil
}

// Endpoints returns the addresses this client was given, normalized.
func (c *Client) Endpoints() []string {
	out := make([]string, len(c.endpoints))
	copy(out, c.endpoints)
	return out
}

// Leader returns the endpoint this client last saw act as leader, or "" if it
// has not seen one yet. A hint for logging; the next request corrects it.
func (c *Client) Leader() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.leader
}
