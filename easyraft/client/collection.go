package client

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/brunoga/raft/v2/easyraft"
)

// Coll is a typed view of one collection, and the type most calls go through.
//
// It mirrors [easyraft.Collection] method for method, so a service that moves
// from embedding a node to talking to one changes where its handle comes from
// and nothing else.
type Coll[T any] struct {
	client *Client
	name   string
	// once, when set, deduplicates every write made through this view.
	once *easyraft.OnceID
}

// Collection returns a typed view of the named collection.
// Names beginning with "__" are reserved by the server and are refused.
func Collection[T any](c *Client, name string) *Coll[T] {
	return &Coll[T]{client: c, name: name}
}

// Exactly returns a view of this collection whose writes carry id, so that a
// retry after a lost response applies the write once rather than twice.
//
// Because the identity makes a write safe to repeat, writes through the
// returned view are retried like reads. Hold the same id across the retries
// of one logical write -- that is what makes it exactly-once -- and take the
// next one from a [easyraft.Session]:
//
//	session := easyraft.NewSession("worker-7")
//	id := session.Next()
//	err := orders.Exactly(id).Create(ctx, key, order)
func (c *Coll[T]) Exactly(id easyraft.OnceID) *Coll[T] {
	next := *c
	next.once = &id
	return &next
}

// keyPath builds the path for one key. Both segments are escaped, so a key
// containing a slash addresses that key rather than a different route.
func (c *Coll[T]) keyPath(key string) string {
	return "/" + url.PathEscape(c.name) + "/" + url.PathEscape(key)
}

func (c *Coll[T]) collectionPath() string {
	return "/" + url.PathEscape(c.name)
}

// writeHeaders assembles the per-request headers for a write, and says
// whether the result is safe to attempt more than once.
//
// A write is safe to repeat when the cluster will deduplicate it, or when
// repeating it cannot apply it twice: a conditional write's second attempt
// is refused by the revision that its first attempt moved.
func (c *Coll[T]) writeHeaders(condition *uint64) (header map[string]string, idempotent bool) {
	header = map[string]string{}
	if c.once != nil {
		header[clientIDHeader] = string(c.once.ClientID)
		header[seqNumHeader] = strconv.FormatUint(c.once.SeqNum, 10)
		idempotent = true
	}
	if condition != nil {
		header["If-Match"] = strconv.FormatUint(*condition, 10)
		idempotent = true
	}
	return header, idempotent
}

// Headers the server reads an exactly-once identity from.
const (
	clientIDHeader = "X-Raft-Client-Id"
	seqNumHeader   = "X-Raft-Seq"
)

// leaseQuery renders the lease a write attaches to, if any.
func leaseQuery(lease easyraft.LeaseID) string {
	if lease == 0 {
		return ""
	}
	return "lease=" + strconv.FormatUint(uint64(lease), 10)
}

// Create inserts a new item. Returns [easyraft.ErrKeyExists] if the key is
// taken.
func (c *Coll[T]) Create(ctx context.Context, key string, value T) error {
	return c.write(ctx, http.MethodPost, key, value, nil, 0)
}

// CreateWithLease inserts a new item attached to a lease, so that it is
// deleted when the lease expires or is revoked.
func (c *Coll[T]) CreateWithLease(ctx context.Context, key string, value T, lease easyraft.LeaseID) error {
	return c.write(ctx, http.MethodPost, key, value, nil, lease)
}

// Update replaces an existing item. Returns [easyraft.ErrKeyNotFound] if it
// does not exist.
func (c *Coll[T]) Update(ctx context.Context, key string, value T) error {
	return c.write(ctx, http.MethodPut, key, value, nil, 0)
}

// UpdateIf replaces an existing item only if its revision is still rev.
// Returns [easyraft.ErrRevisionMismatch] if it is not.
func (c *Coll[T]) UpdateIf(ctx context.Context, key string, value T, rev uint64) error {
	return c.write(ctx, http.MethodPut, key, value, &rev, 0)
}

// Upsert inserts or replaces an item.
func (c *Coll[T]) Upsert(ctx context.Context, key string, value T) error {
	return c.write(ctx, http.MethodPatch, key, value, nil, 0)
}

// UpsertIf writes an item only if its revision is rev. Zero asserts that the
// key does not exist.
func (c *Coll[T]) UpsertIf(ctx context.Context, key string, value T, rev uint64) error {
	return c.write(ctx, http.MethodPatch, key, value, &rev, 0)
}

// UpsertWithLease writes an item attached to a lease, creating it if absent.
// This is the usual way to register something that should disappear when its
// owner stops renewing.
func (c *Coll[T]) UpsertWithLease(ctx context.Context, key string, value T, lease easyraft.LeaseID) error {
	return c.write(ctx, http.MethodPatch, key, value, nil, lease)
}

// Delete removes an existing item.
func (c *Coll[T]) Delete(ctx context.Context, key string) error {
	return c.write(ctx, http.MethodDelete, key, nil, nil, 0)
}

// DeleteIf removes an item only if its revision is still rev.
func (c *Coll[T]) DeleteIf(ctx context.Context, key string, rev uint64) error {
	return c.write(ctx, http.MethodDelete, key, nil, &rev, 0)
}

// write is the one path every single-key write takes.
func (c *Coll[T]) write(ctx context.Context, method, key string, value any, condition *uint64,
	lease easyraft.LeaseID,
) error {
	header, idempotent := c.writeHeaders(condition)
	var body any
	if value != nil {
		body = value
	}
	_, err := c.client.do(ctx, &request{
		method:     method,
		path:       c.keyPath(key),
		query:      leaseQuery(lease),
		body:       body,
		header:     header,
		idempotent: idempotent,
	})
	return err
}

// Read returns an item with linearizable consistency: the node answering
// confirms it is current with the leader before serving.
func (c *Coll[T]) Read(ctx context.Context, key string) (T, error) {
	value, _, err := c.readWithRevision(ctx, key, false)
	return value, err
}

// ReadStale returns an item from whichever node answers, without that node
// confirming it is current. It may lag the cluster by up to a heartbeat.
func (c *Coll[T]) ReadStale(ctx context.Context, key string) (T, error) {
	value, _, err := c.readWithRevision(ctx, key, true)
	return value, err
}

// ReadRev returns an item and the revision at which it was last written, with
// linearizable consistency. Pass the revision to [Coll.UpdateIf] to make the
// next write conditional on nothing having changed in between.
func (c *Coll[T]) ReadRev(ctx context.Context, key string) (value T, rev uint64, err error) {
	return c.readWithRevision(ctx, key, false)
}

// ReadStaleRev returns an item and its revision without a leader round-trip.
func (c *Coll[T]) ReadStaleRev(ctx context.Context, key string) (value T, rev uint64, err error) {
	return c.readWithRevision(ctx, key, true)
}

func (c *Coll[T]) readWithRevision(ctx context.Context, key string, stale bool) (value T, rev uint64, err error) {
	query := ""
	if stale {
		query = "consistency=stale"
	}
	resp, err := c.client.do(ctx, &request{
		method:     http.MethodGet,
		path:       c.keyPath(key),
		query:      query,
		idempotent: true,
	})
	if err != nil {
		return value, 0, err
	}
	if err := json.Unmarshal(resp.body, &value); err != nil {
		return value, 0, fmt.Errorf("easyraft/client: decode %s/%s: %w", c.name, key, err)
	}
	if etag := resp.header.Get("ETag"); etag != "" {
		parsed, parseErr := strconv.ParseUint(strings.Trim(etag, `"`), 10, 64)
		if parseErr != nil {
			return value, 0, fmt.Errorf("easyraft/client: %s/%s returned an unusable ETag %q",
				c.name, key, etag)
		}
		rev = parsed
	}
	return value, rev, nil
}

// List returns every item in the collection with linearizable consistency.
func (c *Coll[T]) List(ctx context.Context) (map[string]T, error) {
	page, err := c.Scan(ctx, ScanOptions{})
	if err != nil {
		return nil, err
	}
	out := make(map[string]T, len(page.Items))
	for _, item := range page.Items {
		out[item.Key] = item.Value
	}
	return out, nil
}

// ScanOptions narrows and paginates a [Coll.Scan], matching
// [easyraft.ScanOptions] field for field, plus the staleness a remote caller
// has to choose for itself.
type ScanOptions struct {
	// Prefix keeps only keys that begin with it.
	Prefix string
	// After resumes strictly after this key: the Next of the previous page.
	After string
	// Limit caps the page. Zero means no cap.
	Limit int
	// Stale reads from whichever node answers without it confirming that it
	// is current.
	Stale bool
}

// Item is one key with its value and the revision it was last written at.
type Item[T any] struct {
	Key      string
	Value    T
	Revision uint64
}

// Page is one page of a scan, in ascending key order.
type Page[T any] struct {
	// Items are the matches in this page.
	Items []Item[T]
	// Next is the cursor for the page after this one, empty when the scan
	// reached the end of the collection. A short page is not the end; this is.
	Next string
	// Revision is how far the answering node had applied, from the
	// X-Raft-Revision header.
	Revision uint64
}

// Scan returns one page of a collection in ascending key order.
//
// The server answers a page as a JSON object, which carries no order, so this
// sorts the keys before returning them. Paging over a large collection is
// therefore a sort per page on both sides -- pagination bounds the size of
// each answer, not the work behind it.
//
// Per-key revisions are not part of a list response, so [Item.Revision] is
// zero here. Use [Coll.ReadRev] for the revision of a key you mean to write.
func (c *Coll[T]) Scan(ctx context.Context, opts ScanOptions) (Page[T], error) {
	if opts.Limit < 0 {
		return Page[T]{}, fmt.Errorf("easyraft/client: scan limit %d is negative", opts.Limit)
	}
	query := url.Values{}
	if opts.Prefix != "" {
		query.Set("prefix", opts.Prefix)
	}
	if opts.After != "" {
		query.Set("after", opts.After)
	}
	if opts.Limit > 0 {
		query.Set("limit", strconv.Itoa(opts.Limit))
	}
	if opts.Stale {
		query.Set("consistency", "stale")
	}

	resp, err := c.client.do(ctx, &request{
		method:     http.MethodGet,
		path:       c.collectionPath(),
		query:      query.Encode(),
		idempotent: true,
	})
	if err != nil {
		return Page[T]{}, err
	}

	var decoded map[string]T
	if err := json.Unmarshal(resp.body, &decoded); err != nil {
		return Page[T]{}, fmt.Errorf("easyraft/client: decode %s: %w", c.name, err)
	}
	page := Page[T]{Next: resp.header.Get("X-Raft-Next-Cursor")}
	for _, key := range sortedKeys(decoded) {
		page.Items = append(page.Items, Item[T]{Key: key, Value: decoded[key]})
	}
	if raw := resp.header.Get("X-Raft-Revision"); raw != "" {
		page.Revision, _ = strconv.ParseUint(raw, 10, 64)
	}
	return page, nil
}

// Mutate runs a mutation registered on the server against one key, and
// returns whatever the mutation returned.
//
// A mutation is not idempotent in general, so this is attempted once unless
// the view carries an exactly-once identity. Use [Coll.Exactly] for a
// mutation that must survive a lost response.
func (c *Coll[T]) Mutate(ctx context.Context, key, name string, args any) ([]byte, error) {
	encodedArgs, err := encodeMutationArgs(args)
	if err != nil {
		return nil, err
	}
	header, idempotent := c.writeHeaders(nil)
	resp, err := c.client.do(ctx, &request{
		method: http.MethodPost,
		path:   c.keyPath(key) + "/mutate",
		body: mutateRequest{
			Name: name,
			Args: encodedArgs,
		},
		header:     header,
		idempotent: idempotent,
	})
	if err != nil {
		return nil, err
	}
	return resp.body, nil
}

// MutateIf runs a mutation only if the key's revision is still rev.
func (c *Coll[T]) MutateIf(ctx context.Context, key, name string, args any, rev uint64) ([]byte, error) {
	encodedArgs, err := encodeMutationArgs(args)
	if err != nil {
		return nil, err
	}
	header, _ := c.writeHeaders(&rev)
	resp, err := c.client.do(ctx, &request{
		method: http.MethodPost,
		path:   c.keyPath(key) + "/mutate",
		body: mutateRequest{
			Name: name,
			Args: encodedArgs,
		},
		header:     header,
		idempotent: true,
	})
	if err != nil {
		return nil, err
	}
	return resp.body, nil
}

// sortedKeys is the order a page is returned in, since a JSON object has none.
func sortedKeys[T any](m map[string]T) []string {
	return slices.Sorted(maps.Keys(m))
}

// mutateRequest is the body the server expects for a mutation.
type mutateRequest struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args,omitempty"`
}

// encodeMutationArgs passes already-encoded arguments through and encodes
// anything else, so a caller may hand over either.
func encodeMutationArgs(args any) (json.RawMessage, error) {
	switch typed := args.(type) {
	case nil:
		return nil, nil
	case json.RawMessage:
		return typed, nil
	case []byte:
		return typed, nil
	}
	encoded, err := json.Marshal(args)
	if err != nil {
		return nil, fmt.Errorf("easyraft/client: encode mutation args: %w", err)
	}
	return encoded, nil
}
