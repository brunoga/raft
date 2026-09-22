package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/brunoga/raft/v2"
	"github.com/brunoga/raft/v2/easyraft"
)

// maxErrorBody is how much of an error response is read before giving up on
// it. An error message is a line; anything longer is a server misbehaving and
// is not worth buffering.
const maxErrorBody = 8 << 10

// request is one call to the cluster, before any retrying or redirecting.
type request struct {
	method string
	// path is everything after the group prefix, starting with a slash.
	path string
	// query is appended to path when non-empty, without its leading "?".
	query string
	// body is encoded as JSON when non-nil. A json.RawMessage is sent as-is.
	body any
	// header carries per-request headers: conditions, exactly-once identity.
	header map[string]string
	// idempotent says this request may be attempted more than once. See the
	// package documentation for what earns that.
	idempotent bool
}

// response is what a call returns, before decoding.
type response struct {
	status int
	body   []byte
	header http.Header
}

// streamedResponse is a response whose body is handed over unread, for the
// one call where reading it into memory would defeat the point.
type streamedResponse struct {
	status int
	body   io.ReadCloser
	header http.Header
}

// do runs a request, following redirects to the leader and retrying what may
// be retried, and returns the first response that is neither.
func (c *Client) do(ctx context.Context, req *request) (*response, error) {
	attempts := 1
	if req.idempotent {
		attempts = c.attempts
	}

	backoff := c.backoff
	var lastErr error
	for attempt := range attempts {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff):
			}
			if backoff < time.Second {
				backoff *= 2
			}
		}

		resp, err := c.attempt(ctx, req)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if !retryable(err) {
			return nil, err
		}
	}
	return nil, lastErr
}

// attempt tries every endpoint once, starting with the one last seen leading,
// and follows leader redirects. It returns the first usable answer.
//
// A redirect is not a retry. It is the cluster saying where the leader is,
// and following it is how a write reaches the node that can take it -- so it
// is followed for every request, idempotent or not.
func (c *Client) attempt(ctx context.Context, req *request) (*response, error) {
	var lastErr error
	for _, endpoint := range c.order() {
		resp, err := c.callFollowingLeader(ctx, endpoint, req)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// A definite answer from a reachable node is the answer. Only the
		// conditions that another node might not have are worth asking one.
		if !worthAnotherEndpoint(err) {
			return nil, err
		}
	}
	if lastErr == nil {
		lastErr = errors.New("easyraft/client: no endpoints configured")
	}
	return nil, fmt.Errorf("%w: %w", ErrNoEndpoint, lastErr)
}

// maxRedirects bounds how many times one attempt follows a leader redirect.
// Two is a leader change landing mid-request; more than a few is a loop.
const maxRedirects = 5

func (c *Client) callFollowingLeader(ctx context.Context, endpoint string, req *request) (*response, error) {
	for range maxRedirects {
		resp, redirect, err := c.call(ctx, endpoint, req)
		if err != nil {
			return nil, err
		}
		if redirect == "" {
			c.rememberLeader(endpoint)
			return resp, nil
		}
		next, err := normalizeEndpoint(redirect)
		if err != nil {
			return nil, fmt.Errorf("easyraft/client: %s redirected to an unusable address: %w", endpoint, err)
		}
		if next == endpoint {
			return nil, fmt.Errorf("easyraft/client: %s redirected to itself", endpoint)
		}
		endpoint = next
	}
	return nil, fmt.Errorf("easyraft/client: more than %d leader redirects in one attempt", maxRedirects)
}

// call makes one HTTP request and reads the response. It returns a redirect
// target instead of a response when the node points at the leader.
func (c *Client) call(ctx context.Context, endpoint string, req *request) (*response, string, error) {
	httpResp, redirect, err := c.send(ctx, endpoint, req)
	if err != nil || redirect != "" {
		return nil, redirect, err
	}
	defer func() { _ = httpResp.Body.Close() }()

	raw, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, "", fmt.Errorf("easyraft/client: %s: read response: %w", endpoint, err)
	}
	if httpResp.StatusCode >= 400 {
		return nil, "", errorFor(endpoint, httpResp.StatusCode, raw)
	}
	return &response{status: httpResp.StatusCode, body: raw, header: httpResp.Header}, "", nil
}

// callStreaming is call without reading the body, for a response the caller
// means to copy rather than hold. An error response is still read, because an
// error message is a line and the caller has nowhere to put it.
func (c *Client) callStreaming(ctx context.Context, endpoint string, req *request) (*streamedResponse, string, error) {
	httpResp, redirect, err := c.send(ctx, endpoint, req)
	if err != nil || redirect != "" {
		return nil, redirect, err
	}
	if httpResp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(httpResp.Body, maxErrorBody))
		_ = httpResp.Body.Close()
		return nil, "", errorFor(endpoint, httpResp.StatusCode, raw)
	}
	return &streamedResponse{
		status: httpResp.StatusCode,
		body:   httpResp.Body,
		header: httpResp.Header,
	}, "", nil
}

// send builds and performs one request, and reports a leader redirect rather
// than following it. The response body is left unread and open unless a
// redirect was returned, in which case it is drained and closed here.
func (c *Client) send(ctx context.Context, endpoint string, req *request) (*http.Response, string, error) {
	body, err := requestBody(req)
	if err != nil {
		return nil, "", err
	}

	target := endpoint + c.group + req.path
	if req.query != "" {
		target += "?" + req.query
	}
	httpReq, err := http.NewRequestWithContext(ctx, req.method, target, body)
	if err != nil {
		return nil, "", fmt.Errorf("easyraft/client: build request: %w", err)
	}
	if req.body != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		httpReq.Header.Set("Authorization", c.token)
	}
	for k, v := range req.header {
		httpReq.Header.Set(k, v)
	}

	httpResp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, "", fmt.Errorf("easyraft/client: %s: %w", endpoint, err)
	}

	if httpResp.StatusCode == http.StatusTemporaryRedirect ||
		httpResp.StatusCode == http.StatusPermanentRedirect {
		location := httpResp.Header.Get("Location")
		_, _ = io.Copy(io.Discard, io.LimitReader(httpResp.Body, maxErrorBody))
		_ = httpResp.Body.Close()
		if location == "" {
			return nil, "", fmt.Errorf("easyraft/client: %s redirected with no Location", endpoint)
		}
		// Only the origin is taken from the redirect; the path is the one
		// this request already has. A Location that tried to point somewhere
		// else in the API cannot redirect a read into a write.
		origin, originErr := originOf(location)
		if originErr != nil {
			return nil, "", fmt.Errorf("easyraft/client: %s: %w", endpoint, originErr)
		}
		return nil, origin, nil
	}
	return httpResp, "", nil
}

// requestBody turns a request's body into something to send: nothing, a
// reader handed over as-is, or a value encoded as JSON.
func requestBody(req *request) (io.Reader, error) {
	switch body := req.body.(type) {
	case nil:
		return http.NoBody, nil
	case readerBody:
		return body.r, nil
	default:
		encoded, err := encodeBody(req.body)
		if err != nil {
			return nil, err
		}
		return bytes.NewReader(encoded), nil
	}
}

// stream runs a request and hands back the body unread, so the caller can
// copy it somewhere rather than hold it.
//
// It follows leader redirects and moves on to another endpoint the same way
// do does, but it does not retry: a retry would have to discard however much
// of the body the caller has already consumed, which is not this function's
// to decide.
func (c *Client) stream(ctx context.Context, req *request) (*streamedResponse, error) {
	var lastErr error
	for _, endpoint := range c.order() {
		resp, err := c.streamFollowingLeader(ctx, endpoint, req)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if !worthAnotherEndpoint(err) {
			return nil, err
		}
	}
	if lastErr == nil {
		lastErr = errors.New("easyraft/client: no endpoints configured")
	}
	return nil, fmt.Errorf("%w: %w", ErrNoEndpoint, lastErr)
}

func (c *Client) streamFollowingLeader(ctx context.Context, endpoint string, req *request) (
	*streamedResponse, error,
) {
	for range maxRedirects {
		resp, redirect, err := c.callStreaming(ctx, endpoint, req)
		if err != nil {
			return nil, err
		}
		if redirect == "" {
			c.rememberLeader(endpoint)
			return resp, nil
		}
		next, err := normalizeEndpoint(redirect)
		if err != nil {
			return nil, fmt.Errorf("easyraft/client: %s redirected to an unusable address: %w", endpoint, err)
		}
		if next == endpoint {
			return nil, fmt.Errorf("easyraft/client: %s redirected to itself", endpoint)
		}
		endpoint = next
	}
	return nil, fmt.Errorf("easyraft/client: more than %d leader redirects in one attempt", maxRedirects)
}

// encodeBody turns a request body into JSON, passing through anything that is
// already encoded.
func encodeBody(body any) ([]byte, error) {
	if raw, ok := body.(json.RawMessage); ok {
		return raw, nil
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("easyraft/client: encode request body: %w", err)
	}
	return encoded, nil
}

// order returns the endpoints to try, the last known leader first.
func (c *Client) order() []string {
	c.mu.RLock()
	leader := c.leader
	c.mu.RUnlock()

	if leader == "" {
		return c.endpoints
	}
	out := make([]string, 0, len(c.endpoints)+1)
	out = append(out, leader)
	for _, e := range c.endpoints {
		if e != leader {
			out = append(out, e)
		}
	}
	return out
}

func (c *Client) rememberLeader(endpoint string) {
	c.mu.Lock()
	c.leader = endpoint
	c.mu.Unlock()
}

// originOf extracts scheme://host:port from a redirect target, refusing
// anything that is not an absolute URL.
func originOf(location string) (string, error) {
	if !strings.Contains(location, "://") {
		return "", fmt.Errorf("redirect target %q is not an absolute URL", location)
	}
	return normalizeOrigin(location)
}

// errorBody is the shape easyraft's HTTP layer reports failures in.
type errorBody struct {
	Error string `json:"error"`
}

// errorFor turns a status code and body into the error an in-process caller
// would have got, so that code moving between the two does not change which
// errors it tests for.
func errorFor(endpoint string, status int, raw []byte) error {
	message := strings.TrimSpace(string(raw))
	var decoded errorBody
	if json.Unmarshal(raw, &decoded) == nil && decoded.Error != "" {
		message = decoded.Error
	}
	if len(message) > 512 {
		message = message[:512] + "..."
	}

	// The body names the error where easyraft wrote one, because several
	// conditions share a status: 404 is a missing key or a missing lease,
	// and 409 is an existing key or an obsolete sequence number.
	for _, known := range []error{
		easyraft.ErrKeyNotFound, easyraft.ErrKeyExists, easyraft.ErrRevisionMismatch,
		easyraft.ErrLeaseNotFound, easyraft.ErrReservedCollection, easyraft.ErrWitness,
		easyraft.ErrUnauthorized, easyraft.ErrForbidden,
	} {
		if strings.Contains(message, known.Error()) {
			return known
		}
	}
	if strings.Contains(message, raft.ErrObsoleteSeqNum.Error()) {
		return raft.ErrObsoleteSeqNum
	}

	// Failing that, the status. These are the mappings easyraft's HTTP layer
	// makes in only one direction each.
	switch status {
	case http.StatusPreconditionFailed:
		return easyraft.ErrRevisionMismatch
	case http.StatusUnauthorized:
		return easyraft.ErrUnauthorized
	case http.StatusForbidden:
		return easyraft.ErrForbidden
	case http.StatusNotFound:
		return easyraft.ErrKeyNotFound
	}
	return &Error{Status: status, Message: message, Endpoint: endpoint}
}

// retryable says whether another attempt could plausibly do better.
//
// 503 is the one status easyraft promises a client may retry unchanged: a
// backlog drains, a lease is renewed, an election finishes. A transport
// failure qualifies too -- the request may never have arrived, and for the
// requests this package retries it does not matter if it did.
func retryable(err error) bool {
	var apiErr *Error
	if errors.As(err, &apiErr) {
		return apiErr.Status == http.StatusServiceUnavailable ||
			apiErr.Status == http.StatusRequestTimeout ||
			apiErr.Status == http.StatusTooManyRequests ||
			apiErr.Status >= 500
	}
	// Anything that is not an answer from the server: a dial failure, a reset
	// connection, a body that stopped mid-read.
	return !isKnownError(err)
}

// worthAnotherEndpoint says whether a different node might answer differently.
// A missing key is missing everywhere; an unreachable node is not.
func worthAnotherEndpoint(err error) bool {
	return retryable(err) || errors.Is(err, ErrNoEndpoint)
}

// isKnownError reports whether err is one of the conditions the cluster
// describes, rather than a failure to reach it.
func isKnownError(err error) bool {
	for _, known := range []error{
		easyraft.ErrKeyNotFound, easyraft.ErrKeyExists, easyraft.ErrRevisionMismatch,
		easyraft.ErrLeaseNotFound, easyraft.ErrReservedCollection, easyraft.ErrWitness,
		easyraft.ErrUnauthorized, easyraft.ErrForbidden, raft.ErrObsoleteSeqNum,
	} {
		if errors.Is(err, known) {
			return true
		}
	}
	var apiErr *Error
	return errors.As(err, &apiErr)
}
