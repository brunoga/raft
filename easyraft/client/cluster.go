package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/brunoga/raft/v2"
	"github.com/brunoga/raft/v2/easyraft"
)

// ---- Leases ----------------------------------------------------------------

// Lease describes one lease held by the cluster.
type Lease struct {
	ID        easyraft.LeaseID
	TTL       time.Duration
	ExpiresAt time.Time
	Keys      []easyraft.LeaseKey
}

// leaseResponse is the wire shape of a lease.
type leaseResponse struct {
	ID         uint64              `json:"id"`
	TTLSeconds float64             `json:"ttl_seconds"`
	ExpiresAt  time.Time           `json:"expires_at"`
	Keys       []easyraft.LeaseKey `json:"keys,omitempty"`
}

func (r leaseResponse) lease() Lease {
	return Lease{
		ID:        easyraft.LeaseID(r.ID),
		TTL:       time.Duration(r.TTLSeconds * float64(time.Second)),
		ExpiresAt: r.ExpiresAt,
		Keys:      r.Keys,
	}
}

// GrantLease creates a lease with the given time to live. Keys written with
// [Coll.CreateWithLease] or [Coll.UpsertWithLease] under it are deleted
// together when it expires or is revoked.
//
// A grant is attempted once, because a repeat would create a second lease
// nothing will ever renew. Pass an [easyraft.OnceID] to [Client.GrantLeaseOnce]
// when the grant itself has to survive a lost response.
func (c *Client) GrantLease(ctx context.Context, ttl time.Duration) (easyraft.LeaseID, error) {
	return c.grantLease(ctx, ttl, nil)
}

// GrantLeaseOnce creates a lease, deduplicated under id: a retry with the
// same id returns the lease the first attempt granted rather than a second
// one.
func (c *Client) GrantLeaseOnce(ctx context.Context, ttl time.Duration, id easyraft.OnceID) (easyraft.LeaseID, error) {
	return c.grantLease(ctx, ttl, &id)
}

func (c *Client) grantLease(ctx context.Context, ttl time.Duration, id *easyraft.OnceID) (easyraft.LeaseID, error) {
	if ttl <= 0 {
		return 0, fmt.Errorf("easyraft/client: lease TTL %v is not positive", ttl)
	}
	header := map[string]string{}
	idempotent := false
	if id != nil {
		header[clientIDHeader] = string(id.ClientID)
		header[seqNumHeader] = strconv.FormatUint(id.SeqNum, 10)
		idempotent = true
	}

	resp, err := c.do(ctx, &request{
		method:     http.MethodPost,
		path:       "/__leases",
		body:       map[string]float64{"ttl_seconds": ttl.Seconds()},
		header:     header,
		idempotent: idempotent,
	})
	if err != nil {
		return 0, err
	}
	var decoded leaseResponse
	if err := json.Unmarshal(resp.body, &decoded); err != nil {
		return 0, fmt.Errorf("easyraft/client: decode lease: %w", err)
	}
	return easyraft.LeaseID(decoded.ID), nil
}

// KeepAlive restarts a lease's time to live and reports when it now falls
// due. Returns [easyraft.ErrLeaseNotFound] if the lease is already gone,
// which means granting a new one and registering again rather than retrying.
//
// Renewal is idempotent -- it sets a deadline rather than moving one -- so it
// is retried like a read.
func (c *Client) KeepAlive(ctx context.Context, id easyraft.LeaseID) (time.Time, error) {
	resp, err := c.do(ctx, &request{
		method:     http.MethodPost,
		path:       "/__leases/" + strconv.FormatUint(uint64(id), 10) + "/keepalive",
		idempotent: true,
	})
	if err != nil {
		return time.Time{}, err
	}
	var decoded leaseResponse
	if err := json.Unmarshal(resp.body, &decoded); err != nil {
		return time.Time{}, fmt.Errorf("easyraft/client: decode lease: %w", err)
	}
	return decoded.ExpiresAt, nil
}

// KeepAliveLoop renews a lease until ctx is done, and returns why it stopped.
//
// It renews every third of the TTL, so two consecutive failures still leave
// time for a third attempt before the lease falls due. It returns
// [easyraft.ErrLeaseNotFound] as soon as the lease is gone -- the keys it
// held have already been deleted, so the answer is a new lease and a fresh
// registration, not another renewal -- and ctx.Err() when the caller is
// finished. Everything else is retried.
//
// One goroutine per registration:
//
//	lease, err := c.GrantLease(ctx, 15*time.Second)
//	if err != nil {
//		return err
//	}
//	if err := services.UpsertWithLease(ctx, myID, me, lease); err != nil {
//		return err
//	}
//	go func() { _ = c.KeepAliveLoop(ctx, lease) }()
func (c *Client) KeepAliveLoop(ctx context.Context, id easyraft.LeaseID) error {
	lease, err := c.Lease(ctx, id)
	if err != nil {
		return err
	}
	interval := lease.TTL / 3
	if interval < time.Millisecond {
		interval = time.Millisecond
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
		if _, err := c.KeepAlive(ctx, id); err != nil {
			if errors.Is(err, easyraft.ErrLeaseNotFound) {
				return err
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// Anything else is a cluster in motion rather than a lost lease,
			// and there are two more chances before the deadline.
		}
	}
}

// RevokeLease deletes a lease and every key it holds. Revoking one that is
// already gone succeeds: the outcome asked for is the outcome.
func (c *Client) RevokeLease(ctx context.Context, id easyraft.LeaseID) error {
	_, err := c.do(ctx, &request{
		method:     http.MethodDelete,
		path:       "/__leases/" + strconv.FormatUint(uint64(id), 10),
		idempotent: true,
	})
	if errors.Is(err, easyraft.ErrLeaseNotFound) {
		return nil
	}
	return err
}

// Lease returns what the answering node knows about one lease. Read from that
// node's local state, so on a follower it is as current as that follower is.
func (c *Client) Lease(ctx context.Context, id easyraft.LeaseID) (Lease, error) {
	resp, err := c.do(ctx, &request{
		method:     http.MethodGet,
		path:       "/__leases/" + strconv.FormatUint(uint64(id), 10),
		idempotent: true,
	})
	if err != nil {
		return Lease{}, err
	}
	var decoded leaseResponse
	if err := json.Unmarshal(resp.body, &decoded); err != nil {
		return Lease{}, fmt.Errorf("easyraft/client: decode lease: %w", err)
	}
	return decoded.lease(), nil
}

// Leases returns every lease the answering node holds.
func (c *Client) Leases(ctx context.Context) ([]Lease, error) {
	resp, err := c.do(ctx, &request{
		method:     http.MethodGet,
		path:       "/__leases",
		idempotent: true,
	})
	if err != nil {
		return nil, err
	}
	var decoded []leaseResponse
	if err := json.Unmarshal(resp.body, &decoded); err != nil {
		return nil, fmt.Errorf("easyraft/client: decode leases: %w", err)
	}
	out := make([]Lease, 0, len(decoded))
	for _, r := range decoded {
		out = append(out, r.lease())
	}
	return out, nil
}

// ---- Cluster ---------------------------------------------------------------

// Member is one node of the cluster as the server reports it.
type Member struct {
	ID       raft.NodeID `json:"id"`
	RaftAddr string      `json:"raft_addr"`
	Voter    bool        `json:"voter"`
	Leader   bool        `json:"leader"`
	Self     bool        `json:"self"`
}

// Members returns the cluster's membership.
func (c *Client) Members(ctx context.Context) ([]Member, error) {
	resp, err := c.do(ctx, &request{
		method:     http.MethodGet,
		path:       "/members",
		idempotent: true,
	})
	if err != nil {
		return nil, err
	}
	var decoded struct {
		Members []Member `json:"members"`
	}
	if err := json.Unmarshal(resp.body, &decoded); err != nil {
		return nil, fmt.Errorf("easyraft/client: decode members: %w", err)
	}
	return decoded.Members, nil
}

// Status returns the answering node's view of the group.
func (c *Client) Status(ctx context.Context) (raft.GroupStatus, error) {
	var status raft.GroupStatus
	resp, err := c.do(ctx, &request{
		method:     http.MethodGet,
		path:       "/status",
		idempotent: true,
	})
	if err != nil {
		return status, err
	}
	if err := json.Unmarshal(resp.body, &status); err != nil {
		return status, fmt.Errorf("easyraft/client: decode status: %w", err)
	}
	return status, nil
}

// Health reports whether any endpoint is serving. It is a liveness check on
// a node, not a readiness check on the cluster: use [Client.Members] or
// [Client.Status] to ask whether there is a leader.
func (c *Client) Health(ctx context.Context) error {
	_, err := c.do(ctx, &request{
		method:     http.MethodGet,
		path:       "/health",
		idempotent: true,
	})
	return err
}

// TransferLeadership asks the current leader to hand leadership to the node
// given.
func (c *Client) TransferLeadership(ctx context.Context, to raft.NodeID) error {
	_, err := c.do(ctx, &request{
		method: http.MethodPost,
		path:   "/transfer-leadership",
		body:   map[string]raft.NodeID{"to": to},
	})
	return err
}

// RemoveServer removes a node from the cluster.
func (c *Client) RemoveServer(ctx context.Context, id raft.NodeID) error {
	_, err := c.do(ctx, &request{
		method: http.MethodDelete,
		path:   "/members/" + string(id),
	})
	return err
}

// ---- Batches ---------------------------------------------------------------

// BatchOp is one operation of a [Client.Batch]. Build them with [Create],
// [Update], [Upsert], [Delete], [Mutate] and [Check] rather than by hand.
type BatchOp struct {
	Op         string          `json:"op"`
	Collection string          `json:"collection"`
	Key        string          `json:"key"`
	Value      json.RawMessage `json:"value,omitempty"`
	MutateName string          `json:"mutate_name,omitempty"`
	MutateArgs json.RawMessage `json:"mutate_args,omitempty"`
	IfRev      *uint64         `json:"if_rev,omitempty"`
	Lease      uint64          `json:"lease,omitempty"`
}

// Create returns a create operation for a batch.
func Create(collection, key string, value any) (BatchOp, error) {
	return valueOp("create", collection, key, value)
}

// Update returns an update operation for a batch.
func Update(collection, key string, value any) (BatchOp, error) {
	return valueOp("update", collection, key, value)
}

// Upsert returns an upsert operation for a batch.
func Upsert(collection, key string, value any) (BatchOp, error) {
	return valueOp("upsert", collection, key, value)
}

// Delete returns a delete operation for a batch.
func Delete(collection, key string) BatchOp {
	return BatchOp{Op: "delete", Collection: collection, Key: key}
}

// Mutate returns a mutation operation for a batch.
func Mutate(collection, key, name string, args any) (BatchOp, error) {
	encoded, err := encodeMutationArgs(args)
	if err != nil {
		return BatchOp{}, err
	}
	return BatchOp{
		Op: "mutate", Collection: collection, Key: key,
		MutateName: name, MutateArgs: encoded,
	}, nil
}

// Check returns a guard: the batch applies only if the key is at rev. It
// writes nothing itself, and is how a batch is made conditional on a key it
// does not write. A rev of zero asserts that the key does not exist.
func Check(collection, key string, rev uint64) BatchOp {
	return BatchOp{Op: "check", Collection: collection, Key: key, IfRev: &rev}
}

// If makes an operation conditional on the key's revision, so that the whole
// batch fails with [easyraft.ErrRevisionMismatch] if the key has moved.
//
// It reads as a modifier on the operation it follows:
//
//	c.Batch(ctx, client.Check("leases", "owner", rev), work.If(workRev))
func (op *BatchOp) If(rev uint64) BatchOp {
	next := *op
	next.IfRev = &rev
	return next
}

// WithLease attaches the key this operation writes to a lease.
func (op *BatchOp) WithLease(lease easyraft.LeaseID) BatchOp {
	next := *op
	next.Lease = uint64(lease)
	return next
}

func valueOp(op, collection, key string, value any) (BatchOp, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return BatchOp{}, fmt.Errorf("easyraft/client: encode %s %s/%s: %w", op, collection, key, err)
	}
	return BatchOp{Op: op, Collection: collection, Key: key, Value: encoded}, nil
}

// BatchResults holds one entry per operation, in order: null for the
// operations that return nothing, and the mutation's own result for a
// mutation.
type BatchResults []json.RawMessage

// Decode unmarshals the result at index into dst.
func (r BatchResults) Decode(index int, dst any) error {
	if index < 0 || index >= len(r) {
		return fmt.Errorf("easyraft/client: batch result %d out of range [0, %d)", index, len(r))
	}
	if len(r[index]) == 0 || string(r[index]) == "null" {
		return fmt.Errorf("easyraft/client: batch result %d is null; that operation returns nothing", index)
	}
	return json.Unmarshal(r[index], dst)
}

// Batch applies several operations as a single atomic entry: either all of
// them are written or none is.
//
// A batch is attempted once, since repeating one could apply it twice. Use
// [Client.BatchOnce] when it has to survive a lost response.
func (c *Client) Batch(ctx context.Context, ops ...BatchOp) (BatchResults, error) {
	return c.batch(ctx, ops, nil)
}

// BatchOnce applies a batch deduplicated under id, so a retry with the same
// id returns the first attempt's result rather than applying it again.
func (c *Client) BatchOnce(ctx context.Context, id easyraft.OnceID, ops ...BatchOp) (BatchResults, error) {
	return c.batch(ctx, ops, &id)
}

func (c *Client) batch(ctx context.Context, ops []BatchOp, id *easyraft.OnceID) (BatchResults, error) {
	if len(ops) == 0 {
		return nil, nil
	}
	header := map[string]string{}
	idempotent := false
	if id != nil {
		header[clientIDHeader] = string(id.ClientID)
		header[seqNumHeader] = strconv.FormatUint(id.SeqNum, 10)
		idempotent = true
	}

	resp, err := c.do(ctx, &request{
		method:     http.MethodPost,
		path:       "/batch",
		body:       ops,
		header:     header,
		idempotent: idempotent,
	})
	if err != nil {
		return nil, err
	}
	var results BatchResults
	if err := json.Unmarshal(resp.body, &results); err != nil {
		return nil, fmt.Errorf("easyraft/client: decode batch results: %w", err)
	}
	return results, nil
}
