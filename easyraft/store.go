// Package easyraft provides a high-level abstraction over the brunoga/raft
// package that lets you build strongly-consistent, replicated services without
// managing Raft internals.
//
// # Core types
//
// [Store] is the primary type. It owns one Raft node, one gRPC transport, and
// one file-backed storage directory. It implements [raft.StateMachine]
// internally — callers never touch Apply, Snapshot, or Restore directly.
//
// [Collection] is a type-safe namespace within a Store. Multiple collections
// can share a single Store (and thus a single Raft group). Each collection
// provides Create, Read, Update, Delete, List, and Mutate operations, plus
// exactly-once variants (CreateOnce, UpdateOnce, DeleteOnce, MutateOnce) for
// idempotent retries across network failures.
//
// [EasyRaft] is a convenience wrapper that binds one Store to one Collection
// named "default". Use it when your service manages a single entity type.
//
// [Manager] runs multiple independent Raft groups (shards) on one physical
// node, sharing a single gRPC transport and HTTP server.
//
// # Determinism requirement
//
// Mutation functions registered with [Collection.RegisterMutation] execute
// inside [raft.StateMachine].Apply on every replica during both normal
// operation and log replay after a restart. They must be purely deterministic:
// do not call time.Now, rand, or any external I/O inside a mutation. If a
// mutation needs the current time, encode the timestamp in the args before
// proposing — all nodes will then apply the same value.
//
// # Exactly-once semantics
//
// Every write method on [Collection] has a corresponding *Once variant
// ([Collection.CreateOnce], [Collection.UpdateOnce], [Collection.DeleteOnce],
// [Collection.MutateOnce]). The *Once variants accept a (clientID, seqNum)
// pair and deduplicate retries: if a network failure causes the caller to
// retry with the same (clientID, seqNum), the cluster applies the command
// exactly once and returns the cached result to the retrying caller.
//
// seqNum must increase monotonically per clientID. Use the *Once variants
// whenever retrying a write on ErrNotLeader or context timeout to avoid
// duplicate application. [Collection.Exactly] offers the same guarantee with
// named fields instead of positional arguments; see [Session] and [OnceID].
//
// # Security
//
// The HTTP API enabled by [WithHTTPAddr] can add and remove cluster members.
// Protect it with [WithHTTPAuth] or [WithBearerTokenAuth], and bind it to an
// interface untrusted clients cannot reach. Peer discovery is only as
// trustworthy as its source, which is why discovered peers join as non-voting
// learners unless [WithDiscoveryAsVoter] says otherwise. See the package
// README for the full picture.
//
// # Getting started
//
//	er, err := easyraft.New[MyType](
//	    easyraft.WithID("n1"),
//	    easyraft.WithRaftAddr(":7001"),
//	    easyraft.WithDataDir("/data/n1"),
//	    easyraft.WithPeers(map[raft.NodeID]string{"n2": "host2:7001"}),
//	)
//	if err != nil {
//	    return err
//	}
//	if err := er.Start(); err != nil {
//	    return err // e.g. this node was told to join a cluster and could not
//	}
//	defer func() {
//	    if err := er.Stop(); err != nil {
//	        log.Printf("easyraft shutdown: %v", err)
//	    }
//	}()
//
// See the package README and [examples/ratelimiter] for complete examples.
package easyraft

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/brunoga/raft/v2"
	"github.com/brunoga/raft/v2/metrics/prommetrics"
	"github.com/brunoga/raft/v2/storage/filestore"
	"github.com/brunoga/raft/v2/transport/grpctransport"
)

var (
	// ErrKeyNotFound is returned by Read, Update, Delete, and Mutate when the
	// requested key does not exist in the collection.
	ErrKeyNotFound = errors.New("easyraft: key not found")

	// ErrKeyExists is returned by Create when a key already exists in the
	// collection.
	ErrKeyExists = errors.New("easyraft: key already exists")

	// ErrWitness is returned by every read against a store built with
	// [WithWitness]. A witness votes and holds no data, so its collections
	// are empty by construction; answering a read from them would report
	// "not found" for a key the cluster holds.
	ErrWitness = errors.New("easyraft: this node is a witness and holds no data")

	// ErrNotLeader is an alias for [raft.ErrNotLeader]. It is returned by any
	// write or linearizable-read operation when this node is not the current
	// Raft leader. The HTTP layer translates this into a 307 redirect (if the
	// leader's address is known) or a 503 response.
	// Use errors.As(err, &easyraft.NotLeaderError{}) to extract the leader hint.
	ErrNotLeader = raft.ErrNotLeader
)

// NotLeaderError is an alias for [raft.NotLeaderError]. It is returned when a
// write or linearizable-read is attempted on a non-leader node. The Leader
// field carries a hint about who the current leader is; it may be empty if
// this node does not know.
//
// Callers can use either the raft or easyraft package name interchangeably:
//
//	var nle *easyraft.NotLeaderError        // same type as *raft.NotLeaderError
//	if errors.As(err, &nle) { ... nle.Leader ... }
type NotLeaderError = raft.NotLeaderError

// metadataCollection holds one entry per node: its advertised HTTP address.
// It is written only by advertiseMetadata and is never reachable over HTTP —
// its contents decide where leader redirects point.
const metadataCollection = "__easyraft_metadata__"

// notifyQueueDepth is how many change events may be in flight between Apply
// and the dispatcher before events start being dropped (and a gap reported).
const notifyQueueDepth = 1024

// collectionQueueDepth is the per-collection backlog held for a single
// OnChange handler. Each collection gets its own queue and goroutine so a slow
// handler delays only its own collection.
const collectionQueueDepth = 256

type opType string

const (
	opCreate opType = "create"
	opUpdate opType = "update"
	opDelete opType = "delete"
	opUpsert opType = "upsert"
	opMutate opType = "mutate"
	opBatch  opType = "batch"
)

// changeEvent is an internal notification emitted after each successful write
// and delivered to change handlers outside the state-machine lock.
type changeEvent struct {
	collection string
	key        string
	value      json.RawMessage // nil when deleted
	deleted    bool

	// seq is the store-wide, monotonically increasing number of this event.
	seq uint64

	// gap marks a synthetic event reporting that one or more real events for
	// this collection were dropped because a queue was full. The receiver must
	// resynchronise from the collection's current state.
	gap bool
}

// rawChangeEvent is the untyped form handed to per-collection handlers.
type rawChangeEvent struct {
	Seq     uint64
	Key     string
	Value   json.RawMessage
	Deleted bool
	Gap     bool
}

type command struct {
	Op         opType          `json:"op"`
	Collection string          `json:"collection"`
	Key        string          `json:"key"`
	Value      json.RawMessage `json:"value,omitempty"`
	MutateName string          `json:"mutate_name,omitempty"`
	MutateArgs json.RawMessage `json:"mutate_args,omitempty"`
	Batch      []command       `json:"batch,omitempty"`
}

// raftPeerInfo holds the transport-level details for one cluster member.
type raftPeerInfo struct {
	addr  string
	voter bool
}

// readIndexer is the subset of [raft.Node] used to establish read
// linearizability. Having it as an interface keeps the fallback policy
// testable without a running cluster.
type readIndexer interface {
	ReadIndex(ctx context.Context) (raft.Index, error)
	ReadIndexLease(ctx context.Context) (raft.Index, error)
}

// Store is a Raft-replicated key-value store that can hold multiple typed
// collections. Each Store owns exactly one Raft node, one gRPC transport, and
// one persistent storage directory.
//
// Create a Store with [NewStore], add typed views with [AddCollection], then
// call [Store.Start]. Writes are forwarded to the leader automatically via
// ErrNotLeader; the HTTP layer (enabled by [WithHTTPAddr]) additionally issues
// 307 redirects to the leader's HTTP address.
//
// A Store is safe to use from multiple goroutines after Start returns.
type Store struct {
	mu          sync.RWMutex
	collections map[string]map[string]json.RawMessage
	mutations   map[string]map[string]mutationFunc
	node        *raft.Node

	// reader establishes read linearizability. It is the Raft node in
	// production and is overridable in tests.
	reader readIndexer

	// raftPeers maps known peer IDs to their Raft gRPC address and voter
	// status. Seeded from cfg.Peers at init; updated by handleJoin, discovery,
	// AddServer, and RemoveServer. Protected by mu. Used to serve GET /members
	// and to build join responses.
	raftPeers map[raft.NodeID]raftPeerInfo

	// onChangeFns holds per-collection callbacks registered via OnChange.
	// Protected by mu. Each key is a collection name; values are called
	// outside the lock by that collection's dispatcher goroutine.
	onChangeFns map[string]func(rawChangeEvent)

	// notifyCh carries change events from Apply (state-machine goroutine) to
	// the notification dispatcher (started by Start). Buffered so Apply never
	// blocks; a full queue produces a gap event rather than silent loss.
	notifyCh chan changeEvent

	// eventSeq numbers every change event this store emits, so a consumer can
	// tell contiguous delivery from a gap.
	eventSeq atomic.Uint64

	// gapMu guards pendingGaps, the set of collections owed a gap marker
	// because notifyCh was full when one of their events was produced.
	gapMu       sync.Mutex
	pendingGaps map[string]struct{}

	// pendingEvents accumulates events during a single applyCommand call.
	// Reset at the start of each Apply; only accessed under mu.
	pendingEvents []changeEvent

	// discoveredAddrs remembers the address discovery last reported for each
	// peer, so a changed address can be reported instead of applied silently.
	discoveredAddrs map[raft.NodeID]string

	// started records that the Raft event loop was launched. Node.Stop waits
	// for goroutines that only Node.Start creates, so Stop must not call it on
	// a store that was constructed but never started.
	started atomic.Bool

	// stopOnce and stopErr make Stop idempotent. Shutdown closes file handles
	// and listeners, which report an error the second time round, so the first
	// call does the work and every later call repeats its verdict.
	stopOnce sync.Once
	stopErr  error

	cfg          config
	cancel       context.CancelFunc
	stopCtx      context.Context
	httpListener net.Listener
	httpServer   *http.Server
	transport    raft.Transport
	storage      raft.Storage
}

// NewStore creates a Store that can manage multiple typed collections.
// [WithID], [WithRaftAddr], and [WithDataDir] are required; all other options
// are optional. Call [Store.Start] after registering collections.
//
// The Raft and (when configured) HTTP listeners are bound here, so an address
// that is malformed or already in use is reported as an error rather than
// failing later inside a background goroutine.
func NewStore(opts ...Option) (*Store, error) {
	var c config
	for _, o := range opts {
		o(&c)
	}

	if c.ID == "" {
		return nil, fmt.Errorf("easyraft: WithID is required")
	}
	if c.DataDir == "" {
		return nil, fmt.Errorf("easyraft: WithDataDir is required")
	}
	if err := validateAdvertised(&c); err != nil {
		return nil, err
	}
	if err := validateSecurity(&c, true); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	s := &Store{
		collections:     make(map[string]map[string]json.RawMessage),
		mutations:       make(map[string]map[string]mutationFunc),
		raftPeers:       make(map[raft.NodeID]raftPeerInfo),
		onChangeFns:     make(map[string]func(rawChangeEvent)),
		notifyCh:        make(chan changeEvent, notifyQueueDepth),
		pendingGaps:     make(map[string]struct{}),
		discoveredAddrs: make(map[raft.NodeID]string),
		cfg:             c,
		cancel:          cancel,
		stopCtx:         ctx,
	}

	if err := s.initRaft(); err != nil {
		cancel()
		return nil, err
	}
	if err := s.initHTTP(); err != nil {
		cancel()
		s.closeAfterFailedInit()
		return nil, err
	}

	return s, nil
}

// closeAfterFailedInit releases the resources acquired before a later step of
// construction failed, so a caller that only sees an error leaks nothing.
//
// The Raft node is deliberately not stopped: it has not been started, and
// Node.Stop waits for goroutines that Node.Start would have created. Closing
// the transport and the storage releases everything the node actually holds.
//
// Close errors are discarded rather than returned. [NewStore] already has the
// error that made construction fail, and that is the one the caller needs;
// replacing or padding it with a failure from unwinding would bury the cause.
func (s *Store) closeAfterFailedInit() {
	if closer, ok := s.transport.(io.Closer); ok {
		_ = closer.Close()
	}
	if closer, ok := s.storage.(io.Closer); ok {
		_ = closer.Close()
	}
}

// logger returns the configured logger, or slog.Default() so that problems are
// reported somewhere rather than dropped when no logger was supplied.
// validateAdvertised refuses a configuration whose node could not be reached by
// the peers it is about to tell about itself.
//
// A joining node hands the cluster an address and is then dialled at it. If
// that address names no host -- ":7002", or "0.0.0.0:7002" -- there is nothing
// a remote peer can do with it, and the join is rejected at the far end with a
// 400 that surfaces, thirty seconds later, as a timeout. Refusing here says
// what is wrong while the operator is still looking at the command they typed.
func validateAdvertised(c *config) error {
	if len(c.JoinAddrs) == 0 {
		// Nothing is being told where to find this node.
		return nil
	}
	addr := c.advertiseRaftAddr()
	if addr == "" {
		return fmt.Errorf("easyraft: WithJoinAddr needs a Raft address to advertise; " +
			"set WithRaftAddr")
	}
	if !advertisableHost(addr) {
		return fmt.Errorf("easyraft: cannot join a cluster while advertising %q: it names no "+
			"host, so the nodes this one joins have no address to dial it back on. Give "+
			"WithRaftAddr a reachable host:port (127.0.0.1:7002 for a local cluster), or "+
			"keep the bind address and set WithAdvertiseRaftAddr to what peers should use",
			addr)
	}
	return nil
}

// validateSecurity refuses a configuration that would serve an open Raft port
// or an open HTTP API without having been told to. A Raft peer is fully
// trusted and the HTTP API reshapes the cluster, so either left open by
// omission is a cluster anyone can take; each has an option that says the
// exposure is understood, and the absence of both is treated as the mistake
// it almost always is.
//
// servesHTTP says whether this configuration will serve the HTTP API at all;
// a Manager's stores share the manager's listener and are checked there.
func validateSecurity(c *config, servesHTTP bool) error {
	if c.TLS == nil && !c.AcknowledgeInsecureTransport {
		return fmt.Errorf("easyraft: the Raft transport has no TLS configuration; pass WithTLS, " +
			"or WithInsecureTransportAcknowledged to run in plaintext on a trusted network")
	}
	if servesHTTP && (c.HTTPAddr != "" || c.HTTPMux != nil) &&
		c.HTTPAuth == nil && !c.AcknowledgeInsecureHTTP {
		return fmt.Errorf("easyraft: the HTTP API has no authorization hook; pass WithHTTPAuth or " +
			"WithBearerTokenAuth, or WithInsecureHTTPAcknowledged if the listener is only " +
			"reachable inside a trusted boundary")
	}
	return nil
}

// transportOptions builds the grpctransport options a configuration calls for.
// validateSecurity has already established that one of the two branches
// applies.
func transportOptions(c *config) []grpctransport.Option {
	if c.TLS == nil {
		return []grpctransport.Option{grpctransport.WithInsecure()}
	}
	opts := []grpctransport.Option{grpctransport.WithTLSConfig(c.TLS)}
	if auth := peerAuthorizerFor(c); auth != nil {
		opts = append(opts, grpctransport.WithPeerAuthorizer(auth))
	}
	return opts
}

// peerAuthorizerFor returns the check that binds the node ID an RPC claims to
// the certificate that carried it, or nil when there is nothing to bind it
// to.
//
// An explicit one always wins. Otherwise the default applies exactly when it
// can work: the certificate has to be there and have been verified, which is
// what tls.RequireAndVerifyClientCert guarantees and what every weaker
// setting does not. Installing it against a configuration that does not
// require client certificates would refuse every inbound RPC, which is a
// worse failure than the one it is guarding against.
func peerAuthorizerFor(c *config) grpctransport.PeerAuthorizer {
	if c.PeerAuthorizer != nil {
		return c.PeerAuthorizer
	}
	if c.TLS != nil && c.TLS.ClientAuth == tls.RequireAndVerifyClientCert {
		return grpctransport.MTLSPeerAuthorizer(nil)
	}
	return nil
}

// warnIfPeersUnauthorized logs, once per store or manager, that Raft RPCs are
// encrypted but the peers sending them are not being identified.
//
// It is a warning rather than a refusal because the identification may be
// happening somewhere this package cannot see -- a service mesh terminating
// mTLS, a network that only peers can reach. What it must not do is stay
// quiet: TLS without client certificates leaves the Raft port open to anyone
// who can reach it, and one AppendEntries carrying a high term from anyone at
// all makes every node step down.
func warnIfPeersUnauthorized(c *config, logger *slog.Logger) {
	if c.TLS == nil || peerAuthorizerFor(c) != nil {
		return
	}
	logger.Warn("easyraft: Raft RPCs are encrypted but their senders are not identified: "+
		"the TLS configuration does not require and verify client certificates, so any "+
		"host that can reach this port can claim to be any node. Set "+
		"ClientAuth: tls.RequireAndVerifyClientCert, or pass WithPeerAuthorizer if peers "+
		"are identified elsewhere.",
		"addr", c.RaftAddr, "client_auth", c.TLS.ClientAuth.String())
}

// advertiseRaftAddr is what peers are told to dial to reach this node's Raft
// port: the explicit advertise address when one was given, otherwise the bind
// address.
func (c *config) advertiseRaftAddr() string {
	if c.AdvertiseRaftAddr != "" {
		return c.AdvertiseRaftAddr
	}
	return c.RaftAddr
}

// resolvedRaftAddr is advertiseRaftAddr with an ephemeral port filled in.
//
// Binding port 0 is how a test, or anything that cannot reserve a port in
// advance, gets one; the real port is only known once the listener exists.
// Telling a peer to dial port 0 would be useless, so the bound port is
// substituted. An explicit advertise address is never rewritten: the operator
// said what they meant.
func (s *Store) resolvedRaftAddr() string {
	return resolvePort(s.cfg.AdvertiseRaftAddr, s.cfg.RaftAddr, func() (string, bool) {
		a, ok := s.transport.(interface{ Addr() string })
		if !ok || a == nil {
			return "", false
		}
		return a.Addr(), true
	})
}

// resolvedHTTPAddr is advertiseHTTPAddr with an ephemeral port filled in.
func (s *Store) resolvedHTTPAddr() string {
	return resolvePort(s.cfg.AdvertiseHTTPAddr, s.cfg.HTTPAddr, func() (string, bool) {
		if s.httpListener == nil {
			return "", false
		}
		return s.httpListener.Addr().String(), true
	})
}

// resolvePort returns explicit when it is set, and otherwise bind with a zero
// port replaced by the one the listener actually got.
func resolvePort(explicit, bind string, bound func() (string, bool)) string {
	if explicit != "" {
		return explicit
	}
	host, port, err := net.SplitHostPort(bind)
	if err != nil || port != "0" {
		return bind
	}
	addr, ok := bound()
	if !ok {
		return bind
	}
	_, boundPort, berr := net.SplitHostPort(addr)
	if berr != nil || boundPort == "0" {
		return bind
	}
	return net.JoinHostPort(host, boundPort)
}

func (s *Store) logger() *slog.Logger {
	if s.cfg.Logger != nil {
		return s.cfg.Logger
	}
	return slog.Default()
}

// TxnResults is the ordered result set returned by [Store.Txn]. Each element
// corresponds to one operation added to the transaction, in the order it was
// added. CRUD operations (Create, Update, Delete) produce a nil entry; Mutate
// operations produce the []byte returned by the mutation function.
//
// Use [TxnResults.Decode] to decode a single result by index into a Go value.
type TxnResults []json.RawMessage

// Decode unmarshals the result at position index into dst, which must be a
// non-nil pointer. It returns an error if index is out of range, if the result
// is nil (the operation was a non-returning CRUD), or if JSON decoding fails.
func (r TxnResults) Decode(index int, dst any) error {
	if index < 0 || index >= len(r) {
		return fmt.Errorf("easyraft: TxnResults index %d out of range [0, %d)", index, len(r))
	}
	if r[index] == nil {
		return fmt.Errorf("easyraft: TxnResults[%d] is nil (non-returning operation)", index)
	}
	return json.Unmarshal(r[index], dst)
}

// Txn accumulates operations across multiple collections to be committed as a
// single atomic log entry. Build one via [Store.Txn]; do not construct directly.
type Txn struct {
	store *Store
	cmds  []command
}

// Create adds a create operation to the transaction.
func (t *Txn) Create(collection, key string, value any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	t.cmds = append(t.cmds, command{
		Op:         opCreate,
		Collection: collection,
		Key:        key,
		Value:      b,
	})
	return nil
}

// Update adds an update operation to the transaction.
func (t *Txn) Update(collection, key string, value any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	t.cmds = append(t.cmds, command{
		Op:         opUpdate,
		Collection: collection,
		Key:        key,
		Value:      b,
	})
	return nil
}

// Delete adds a delete operation to the transaction.
func (t *Txn) Delete(collection, key string) error {
	t.cmds = append(t.cmds, command{
		Op:         opDelete,
		Collection: collection,
		Key:        key,
	})
	return nil
}

// Upsert adds an upsert operation to the transaction. It inserts key if it
// does not exist, or replaces it if it does — without ever returning
// [ErrKeyExists] or [ErrKeyNotFound].
func (t *Txn) Upsert(collection, key string, value any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	t.cmds = append(t.cmds, command{
		Op:         opUpsert,
		Collection: collection,
		Key:        key,
		Value:      b,
	})
	return nil
}

// Mutate adds a mutation operation to the transaction.
func (t *Txn) Mutate(collection, key, name string, args any) error {
	var b []byte
	if args != nil {
		var err error
		b, err = json.Marshal(args)
		if err != nil {
			return err
		}
	}
	t.cmds = append(t.cmds, command{
		Op:         opMutate,
		Collection: collection,
		Key:        key,
		MutateName: name,
		MutateArgs: b,
	})
	return nil
}

// Txn calls fn to build a batch of operations and then proposes the entire
// batch as a single Raft log entry. Either all operations succeed or the whole
// entry fails — there is no partial application.
//
// The returned [TxnResults] has one entry per operation in the order they were
// added. Mutation results carry the bytes returned by the mutation function;
// CRUD results are nil. Use [TxnResults.Decode] to decode a mutation result by
// its positional index. An empty transaction (no operations added) returns nil, nil.
func (s *Store) Txn(ctx context.Context, fn func(tx *Txn) error) (TxnResults, error) {
	tx := &Txn{store: s}
	if err := fn(tx); err != nil {
		return nil, err
	}

	if len(tx.cmds) == 0 {
		return nil, nil
	}

	res, err := s.propose(ctx, &command{
		Op:    opBatch,
		Batch: tx.cmds,
	})
	if err != nil {
		return nil, err
	}

	var results TxnResults
	if err := json.Unmarshal(res, &results); err != nil {
		return nil, fmt.Errorf("easyraft: decode txn results: %w", err)
	}
	return results, nil
}

// raftConfig turns this store's configuration into the engine's, which is
// the one place that mapping happens.
//
// Both ways of building a store go through it -- one node with its own
// transport, and one group of many sharing a manager's -- and when they each
// did it themselves the manager's copy quietly fell behind by every field
// added to the other. A setting that exists and does nothing is worse than
// one that does not exist.
func (s *Store) raftConfig(peers []raft.PeerConfig, tr raft.Transport, st raft.Storage) raft.Config {
	cfg := raft.DefaultConfig()
	cfg.ID = s.cfg.ID
	cfg.Peers = peers
	cfg.Transport = tr
	cfg.Storage = st
	// A witness applies nothing, so it is given no state machine: the engine
	// installs one that discards what it is handed, and this store's
	// collections stay empty by construction rather than by accident. Reads
	// against them report ErrWitness rather than an empty answer.
	if !s.cfg.Witness {
		cfg.StateMachine = storeFSM{s: s}
	}
	cfg.Witness = s.cfg.Witness
	cfg.CommitQuorum = s.cfg.CommitQuorum
	cfg.Zones = s.cfg.Zones
	cfg.MinCommitZones = s.cfg.MinCommitZones
	cfg.PreferredLeader = s.cfg.PreferredLeader
	cfg.LeaseSafetyMargin = s.cfg.LeaseSafetyMargin
	cfg.ProposalQueueSize = s.cfg.ProposalQueueSize
	cfg.ProposalOverflow = s.cfg.ProposalOverflow
	cfg.OnRemoved = s.cfg.OnRemoved
	if s.cfg.MaxClientTableSize > 0 {
		cfg.MaxClientTableSize = s.cfg.MaxClientTableSize
	}
	cfg.Logger = s.cfg.Logger
	cfg.TickInterval = s.raftTickInterval()
	cfg.ElectionTimeoutMin = s.raftElectionTimeoutMin()
	cfg.ElectionTimeoutMax = s.raftElectionTimeoutMax()
	cfg.HeartbeatInterval = s.raftHeartbeatInterval()
	applySnapshotSettings(&cfg, s.cfg.SnapCount)
	return cfg
}

func (s *Store) initRaft() error {
	// 1. Storage
	// filestore.Open creates the directory itself, and makes the creation
	// durable by fsyncing the parents it had to create. Creating it here first
	// would leave the store nothing to create, so that durability would be
	// skipped and the directory's own name would never be fsynced.
	st, err := filestore.Open(s.cfg.DataDir)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}

	// 2. Transport
	if s.cfg.RaftAddr == "" {
		return fmt.Errorf("easyraft: WithRaftAddr is required")
	}

	warnIfPeersUnauthorized(&s.cfg, s.logger())
	tr, err := grpctransport.Listen(s.cfg.RaftAddr, transportOptions(&s.cfg)...)
	if err != nil {
		return fmt.Errorf("listen grpc: %w", err)
	}

	var peerConfigs []raft.PeerConfig
	for id, addr := range s.cfg.Peers {
		if id != s.cfg.ID {
			tr.AddPeer(id, addr)
			peerConfigs = append(peerConfigs, raft.PeerConfig{
				ID: id, Voter: true, Witness: s.cfg.Witnesses[id],
			})
			s.raftPeers[id] = raftPeerInfo{addr: addr, voter: true}
		}
	}

	// 3. Raft config
	rCfg := s.raftConfig(peerConfigs, tr, st)

	// 4. Metrics — must be set before raft.New so the node is constructed with metrics wired in.
	if s.cfg.PromRegisterer != nil {
		rCfg.Metrics = prommetrics.New(s.cfg.PromRegisterer)
	}

	node, err := raft.New(&rCfg)
	if err != nil {
		return fmt.Errorf("new raft node: %w", err)
	}

	tr.Register(s.cfg.ID, node.Handler())
	s.transport = tr
	s.storage = st

	// 5. Discovery: wire both transport-level connectivity and Raft membership.
	// We run our own polling loop rather than using DiscoveryAgent so we can
	// call node.AddServer for each newly seen peer (not just tr.AddPeer).
	if s.cfg.Discovery != nil {
		s.startDiscovery(node, tr)
	}

	s.node = node
	s.reader = node
	return nil
}

// peerAdder is the subset of Transport that can register peer addresses.
// grpctransport.GRPCTransport satisfies this interface.
type peerAdder interface {
	AddPeer(raft.NodeID, string)
}

// startDiscovery launches the background goroutines that keep the transport
// peer table and Raft membership in sync with the discovery output.
// It uses s.stopCtx so everything is torn down cleanly on Store.Stop.
//
// Discovered peers are added as non-voting learners by default: a discovery
// announcement is a network-level claim, and honouring it as a voter would let
// that claim change the cluster's quorum. [WithDiscoveryAsVoter] opts out.
//
// Failures in these loops are logged rather than reported to [Store.Start].
// Discovery is a continuous background process, not a startup step: a lookup
// or an AddServer that fails on one poll is retried on the next, and the node
// keeps replicating with the peers it already has in the meantime. There is no
// point at which such a failure is final enough to hand a caller.
func (s *Store) startDiscovery(node *raft.Node, tr peerAdder) {
	logger := s.logger()

	// Some Discovery implementations (e.g. udpbroadcast) need their own Run loop
	// to receive incoming peer announcements.
	if runner, ok := s.cfg.Discovery.(interface{ Run(context.Context) error }); ok {
		go func() {
			if err := runner.Run(s.stopCtx); err != nil && !errors.Is(err, context.Canceled) {
				logger.Error("easyraft: discovery runner failed", "err", err)
			}
		}()
	}

	interval := s.cfg.DiscoveryInterval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	voter := s.cfg.DiscoveryAsVoter

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		// knownMembers tracks peers that have been successfully added to the Raft
		// cluster via AddServer.  Pre-populate with statically configured peers so
		// we never issue a redundant membership-change RPC for them.
		knownMembers := make(map[raft.NodeID]struct{})
		for id := range s.cfg.Peers {
			knownMembers[id] = struct{}{}
		}

		for {
			peers, err := s.cfg.Discovery.Discover(s.stopCtx)
			if err != nil && !errors.Is(err, context.Canceled) {
				logger.Warn("easyraft: discovery lookup failed", "err", err)
			}
			for _, p := range peers {
				if p.ID == s.cfg.ID {
					continue
				}
				s.applyDiscoveredAddr(tr, p.ID, p.Addr)
				if _, seen := knownMembers[p.ID]; seen {
					continue
				}
				addCtx, cancel := context.WithTimeout(s.stopCtx, 5*time.Second)
				if err := node.AddServer(addCtx, s.discoveredPeerConfig(p.ID)); err != nil {
					logger.Warn("easyraft: discovery AddServer failed (will retry)",
						"peer", p.ID, "err", err)
				} else {
					logger.Info("easyraft: added discovered peer",
						"peer", p.ID, "addr", p.Addr, "voter", voter)
					knownMembers[p.ID] = struct{}{}
					s.mu.Lock()
					s.raftPeers[p.ID] = raftPeerInfo{addr: p.Addr, voter: voter}
					s.mu.Unlock()
				}
				cancel()
			}

			select {
			case <-s.stopCtx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

// discoveredPeerConfig is the membership entry used when discovery introduces
// a peer. It is a learner unless [WithDiscoveryAsVoter] was set: a discovery
// announcement is a claim made over the network, and honouring it as a voter
// would let that claim change the cluster's quorum.
func (s *Store) discoveredPeerConfig(id raft.NodeID) raft.PeerConfig {
	return raft.PeerConfig{ID: id, Voter: s.cfg.DiscoveryAsVoter}
}

// applyDiscoveredAddr registers addr for id with the transport.
//
// A statically configured peer ([WithPeers]) is authoritative: discovery never
// repoints it, because doing so would redirect that member's Raft traffic to
// whoever made the announcement. An address change for a peer discovery itself
// introduced is applied — a restarted container legitimately moves — but it is
// always logged, never silent.
func (s *Store) applyDiscoveredAddr(tr peerAdder, id raft.NodeID, addr string) {
	if static, ok := s.cfg.Peers[id]; ok {
		if static != addr {
			s.logger().Warn("easyraft: ignoring discovery address for a statically configured peer",
				"peer", id, "configured", static, "announced", addr)
		}
		return
	}

	s.mu.Lock()
	previous, known := s.discoveredAddrs[id]
	s.discoveredAddrs[id] = addr
	s.mu.Unlock()

	if known && previous != addr {
		s.logger().Warn("easyraft: discovered peer changed address; Raft traffic for it will follow",
			"peer", id, "from", previous, "to", addr)
	}
	tr.AddPeer(id, addr)
}

// Start launches the Raft event loop, begins advertising this node's HTTP
// address to the cluster (if WithHTTPAddr was set), and starts serving the
// HTTP API on the listener bound by [NewStore].
// Register all collections and mutations before calling Start.
//
// If [WithJoinAddr] was set, Start contacts the seed nodes first and returns an
// error if it could not join before the join deadline expires. That failure is
// fatal on purpose: a node that was told to join a cluster and did not is not a
// replica of anything. It holds an empty log, no existing member knows to
// replicate to it, and every write it accepts will fail. Returning the error
// lets the caller fail its own startup, retry, or report unreadiness, instead
// of running a process that looks healthy and is not in the cluster. Nothing is
// started when the join fails, so the only thing left for the caller to do is
// call [Store.Stop] to release the listeners bound by [NewStore].
//
// Two failures deliberately stay out of the return value, because neither
// decides whether this node participates in consensus and neither is settled
// by the time Start returns. Advertising this node's HTTP address retries in
// the background until it succeeds — it only affects whether other nodes can
// redirect clients here. Serving the HTTP API can only fail after the listener
// bound by [NewStore] is already accepting, so a serve error arrives later and
// is logged. Both are reported through the configured logger.
func (s *Store) Start() error {
	if len(s.cfg.JoinAddrs) > 0 {
		if err := s.joinCluster(s.stopCtx); err != nil {
			return fmt.Errorf("easyraft: node %s did not join the cluster: %w", s.cfg.ID, err)
		}
	}

	if s.node != nil {
		s.node.Start()
		s.started.Store(true)
	}

	go s.dispatchChanges()

	// Register our own HTTP address in the metadata collection so others can redirect to us.
	if s.cfg.HTTPAddr != "" {
		go s.advertiseMetadata()
	}

	s.serveHTTP()

	return nil
}

// advertiseMetadata publishes this node's HTTP address to the cluster so that
// other members can redirect clients here.
//
// It retries until it succeeds or the store stops, which is why Start does not
// wait for it or report its failures: the address is only needed for client
// redirection, a node without one still replicates normally, and a leader
// election in progress at startup makes an early failure the common case
// rather than an exceptional one.
func (s *Store) advertiseMetadata() {
	// Retry until we successfully register our HTTP address with the leader.
	// This ensures that after a leadership rotation, every node's URL is known.
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		if s.node == nil {
			return
		}

		b, _ := json.Marshal(s.resolvedHTTPAddr())
		cmd := &command{
			Collection: metadataCollection,
			Key:        string(s.cfg.ID),
			Value:      b,
		}

		// Try create first: succeeds on fresh start (one round-trip).
		cmd.Op = opCreate
		_, err := s.propose(s.stopCtx, cmd)
		if errors.Is(err, ErrKeyExists) {
			// Key already exists — update our address in-place.
			cmd.Op = opUpdate
			_, err = s.propose(s.stopCtx, cmd)
		}

		if err == nil {
			s.logger().Info("easyraft: advertised HTTP address", "addr", s.resolvedHTTPAddr())
			return
		}

		select {
		case <-s.stopCtx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Ready blocks until this node has a known leader and has applied at least one
// entry in the current term, confirming the local state machine is caught up.
// Call it after Start before issuing your first writes to avoid spurious
// ErrNotLeader errors during leader election.
// Returns ctx.Err() if the context expires, or an error if the store is stopped.
func (s *Store) Ready(ctx context.Context) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		if s.node != nil {
			if s.node.Leader() != "" {
				// Once we have a leader, do a ReadIndex to ensure we are caught up.
				_, err := s.node.ReadIndex(ctx)
				if err == nil {
					return nil
				}
			}
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.stopCtx.Done():
			return errors.New("easyraft: store stopped")
		case <-ticker.C:
		}
	}
}

// readIndex establishes that the local state machine is current enough to
// serve a linearizable read.
//
// With [WithLeaseReads] it first tries the leader's clock-based lease, which
// costs no network round-trip. An expired lease is not an error the caller
// should see: it falls through to the quorum-confirmed path, which makes no
// assumption about clock drift. Without the option, only the quorum path runs.
func (s *Store) readIndex(ctx context.Context) error {
	r := s.reader
	if r == nil {
		if s.node == nil {
			return errors.New("easyraft: node not started")
		}
		r = s.node
	}
	return readIndexWithFallback(ctx, r, s.cfg.LeaseReads)
}

func readIndexWithFallback(ctx context.Context, r readIndexer, useLease bool) error {
	if useLease {
		_, err := r.ReadIndexLease(ctx)
		if err == nil {
			return nil
		}
		if !errors.Is(err, raft.ErrLeaseExpired) {
			return err
		}
		// The lease lapsed — most likely a missed heartbeat round. Confirm the
		// read with a quorum instead of failing the caller.
	}
	_, err := r.ReadIndex(ctx)
	return err
}

// onChangeEventRaw registers fn to be called on this node whenever a committed
// write modifies collection, or whenever events for it had to be dropped.
//
// Only one handler per collection is supported; a second call for the same
// collection replaces the first. Call before Start.
func (s *Store) onChangeEventRaw(collection string, fn func(rawChangeEvent)) {
	s.mu.Lock()
	s.onChangeFns[collection] = fn
	s.mu.Unlock()
}

// collectionDispatcher owns one collection's handler goroutine and backlog.
// Giving each collection its own queue means a handler that blocks delays only
// the collection it was registered for.
type collectionDispatcher struct {
	ch chan changeEvent

	// gapOwed records that an event for this collection was dropped and the
	// handler still has to be told. Touched only by the routing goroutine.
	gapOwed bool
}

// offer queues ev, emitting a gap marker first when one is owed. Neither send
// blocks: a backlogged collection loses events and is told that it did.
func (d *collectionDispatcher) offer(ev *changeEvent) {
	if d.gapOwed {
		select {
		case d.ch <- changeEvent{collection: ev.collection, seq: ev.seq, gap: true}:
			d.gapOwed = false
		default:
			return // still backed up; the gap stays owed
		}
	}
	select {
	case d.ch <- *ev:
	default:
		d.gapOwed = true
	}
}

// dispatchChanges routes change events to per-collection dispatcher
// goroutines, each of which runs that collection's OnChange handler. Giving
// every collection its own queue and goroutine is what keeps one slow handler
// from starving the rest.
//
// Only collections with a registered handler reach here, so the number of
// dispatchers is bounded by the number of handlers, not by the number of
// collection names a client can invent. Runs until stopCtx is done.
func (s *Store) dispatchChanges() {
	dispatchers := make(map[string]*collectionDispatcher)
	var wg sync.WaitGroup

	defer func() {
		for _, d := range dispatchers {
			close(d.ch)
		}
		wg.Wait()
	}()

	for {
		select {
		case <-s.stopCtx.Done():
			// Drain remaining events so Apply never blocks.
			for {
				select {
				case <-s.notifyCh:
				default:
					return
				}
			}
		case ev := <-s.notifyCh:
			d, ok := dispatchers[ev.collection]
			if !ok {
				d = &collectionDispatcher{ch: make(chan changeEvent, collectionQueueDepth)}
				dispatchers[ev.collection] = d
				wg.Add(1)
				go func(collection string, d *collectionDispatcher) {
					defer wg.Done()
					s.runDispatcher(collection, d)
				}(ev.collection, d)
			}
			d.offer(&ev)
		}
	}
}

// runDispatcher calls the handler registered for collection for every event
// queued to d, until d.ch is closed.
func (s *Store) runDispatcher(collection string, d *collectionDispatcher) {
	for ev := range d.ch {
		s.mu.RLock()
		fn := s.onChangeFns[collection]
		s.mu.RUnlock()
		if fn == nil {
			continue
		}
		fn(rawChangeEvent{
			Seq:     ev.seq,
			Key:     ev.key,
			Value:   ev.value,
			Deleted: ev.deleted,
			Gap:     ev.gap,
		})
	}
}

// enqueueEvent hands ev to the dispatcher. A full queue never blocks Apply;
// instead the affected collection is recorded as owing a gap marker, which is
// delivered as soon as there is room again.
func (s *Store) enqueueEvent(ev *changeEvent) {
	s.flushGaps()
	select {
	case s.notifyCh <- *ev:
	default:
		s.recordGap(ev.collection)
	}
}

func (s *Store) recordGap(collection string) {
	s.gapMu.Lock()
	s.pendingGaps[collection] = struct{}{}
	s.gapMu.Unlock()
}

// flushGaps tries to deliver one gap marker per collection that lost an event.
// Markers that still do not fit remain pending for the next attempt.
func (s *Store) flushGaps() {
	s.gapMu.Lock()
	defer s.gapMu.Unlock()
	if len(s.pendingGaps) == 0 {
		return
	}
	seq := s.eventSeq.Load()
	for collection := range s.pendingGaps {
		select {
		case s.notifyCh <- changeEvent{collection: collection, seq: seq, gap: true}:
			delete(s.pendingGaps, collection)
		default:
			return
		}
	}
}

// joinCluster contacts the configured seed HTTP addresses in order, posting
// this node's ID and Raft address to each one until a join succeeds. On
// success it registers the returned peer list with the local transport so
// this node can route Raft RPCs to the existing members.
//
// If all seeds are unreachable or return a transient error (connection refused,
// no leader elected yet), joinCluster retries with a 500 ms interval for up to
// 30 seconds so that callers do not need to guarantee the seeds are fully
// bootstrapped before starting the joining node.
func (s *Store) joinCluster(ctx context.Context) error {
	voter := !s.cfg.JoinAsLearner
	req := joinRequest{ID: s.cfg.ID, RaftAddr: s.resolvedRaftAddr(), Voter: &voter}
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("easyraft: marshal join request: %w", err)
	}

	client := &http.Client{}

	const retryInterval = 500 * time.Millisecond
	const joinTimeout = 30 * time.Second

	joinCtx, cancel := context.WithTimeout(ctx, joinTimeout)
	defer cancel()

	ticker := time.NewTicker(retryInterval)
	defer ticker.Stop()

	var lastErr error
	for {
		for _, seed := range s.cfg.JoinAddrs {
			url := seed
			if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
				url = "http://" + url
			}
			url += "/join"

			httpReq, err := http.NewRequestWithContext(joinCtx, http.MethodPost, url, strings.NewReader(string(body)))
			if err != nil {
				lastErr = err
				continue
			}
			httpReq.Header.Set("Content-Type", "application/json")
			if s.cfg.HTTPCredential != "" {
				httpReq.Header.Set("Authorization", s.cfg.HTTPCredential)
			}

			resp, err := client.Do(httpReq)
			if err != nil {
				lastErr = err
				continue
			}

			var joinResp joinResponse
			decodeErr := json.NewDecoder(resp.Body).Decode(&joinResp)
			_ = resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				lastErr = fmt.Errorf("seed %s returned status %d", seed, resp.StatusCode)
				continue
			}
			if decodeErr != nil {
				lastErr = fmt.Errorf("seed %s: decode response: %w", seed, decodeErr)
				continue
			}

			// Register all returned peers with the local transport.
			pa, hasPeerAdder := s.transport.(peerAdder)
			s.mu.Lock()
			for _, p := range joinResp.Peers {
				if p.ID == s.cfg.ID {
					continue
				}
				s.raftPeers[p.ID] = raftPeerInfo{addr: p.RaftAddr, voter: p.Voter}
				if hasPeerAdder {
					pa.AddPeer(p.ID, p.RaftAddr)
				}
			}
			s.mu.Unlock()

			s.logger().Info("easyraft: joined cluster", "seed", seed, "peers", len(joinResp.Peers))
			return nil
		}

		// All seeds failed this round; wait and retry unless time is up.
		s.logger().Debug("easyraft: join attempt failed, retrying", "err", lastErr)
		select {
		case <-joinCtx.Done():
			if lastErr != nil {
				return fmt.Errorf("easyraft: join timed out after %s (last error: %w)", joinTimeout, lastErr)
			}
			return fmt.Errorf("easyraft: no join seed addresses configured")
		case <-ticker.C:
		}
	}
}

// Status returns the current status of the underlying Raft node.
func (s *Store) Status() raft.GroupStatus {
	return s.node.Status()
}

// Members returns the current cluster membership as seen by this node.
// The list includes this node and reflects the last committed configuration
// change. Safe for concurrent use.
func (s *Store) Members() []raft.PeerConfig {
	if s.node == nil {
		return nil
	}
	return s.node.Members()
}

// Leader returns the NodeID of the node this node believes to be the current
// Raft leader, or empty string if the leader is unknown. Safe for concurrent use.
func (s *Store) Leader() raft.NodeID {
	if s.node == nil {
		return ""
	}
	return s.node.Leader()
}

// Stop cancels the store's context (terminating discovery goroutines), shuts
// down the HTTP server, stops the Raft node, and closes persistent storage.
// It is safe to call Stop more than once — later calls repeat the first call's
// verdict without redoing the work — and on a store that was created but never
// started.
//
// Every step runs whatever the earlier ones reported: a shutdown that gave up
// halfway would strand listeners and file handles. The returned error joins
// whatever did fail, so a caller can act on it rather than read about it in a
// log. Two of them matter in practice. A failed departure under
// [WithLeaveOnStop] leaves this node in the cluster's membership, where it
// still counts towards quorum until an operator removes it — see [Store.Leave].
// A storage close failure is the last opportunity to learn that the on-disk
// Raft log may not be intact, which decides whether this node can be restarted
// or has to be rebuilt from a peer.
// Shutdown stops the store like [Store.Stop], but gives up waiting when ctx is
// done and returns ctx.Err().
//
// It exists because [Store.Stop] finishes what it has started: storage writes the
// node already accepted are carried out rather than abandoned, which is what
// makes an orderly restart keep the tail of its log instead of fetching it
// back from a peer, and a departure under [WithLeaveOnStop] is waited for.
// A disk that has hung rather than failed, or a leader that cannot be reached
// to accept the departure, holds [Store.Stop] there. A process that has to come
// down on a deadline needs a way to say so.
//
// Giving up does not cancel the shutdown. It carries on in the background, so
// the store is then neither running nor finished, and its data directory
// must not be reopened by another process. Prefer [Store.Stop] where there is no
// deadline to meet.
func (s *Store) Shutdown(ctx context.Context) error {
	done := make(chan error, 1)
	go func() { done <- s.Stop() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		s.logger().Warn("easyraft: shutdown deadline passed; the store is still stopping")
		return ctx.Err()
	}
}

func (s *Store) Stop() error {
	s.stopOnce.Do(func() { s.stopErr = s.stop() })
	return s.stopErr
}

func (s *Store) stop() error {
	running := s.started.Load()

	var errs []error

	if s.cfg.LeaveOnStop && running {
		leaveCtx, leaveCancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := s.Leave(leaveCtx); err != nil {
			errs = append(errs, fmt.Errorf("easyraft: node %s did not leave the cluster: %w", s.cfg.ID, err))
		}
		leaveCancel()
	}

	if s.cancel != nil {
		s.cancel()
	}
	if s.httpServer != nil {
		if err := s.httpServer.Shutdown(context.Background()); err != nil {
			errs = append(errs, fmt.Errorf("easyraft: shut down http server: %w", err))
		}
	} else if s.httpListener != nil {
		// Bound at construction but never served: release the port anyway.
		if err := s.httpListener.Close(); err != nil {
			errs = append(errs, fmt.Errorf("easyraft: close http listener: %w", err))
		}
	}
	if running {
		s.node.Stop()
	} else if closer, ok := s.transport.(io.Closer); ok {
		// Never started: the node holds nothing, but the transport is already
		// listening and must be released.
		if err := closer.Close(); err != nil {
			errs = append(errs, fmt.Errorf("easyraft: close transport: %w", err))
		}
	}
	if s.storage != nil {
		if closer, ok := s.storage.(io.Closer); ok {
			if err := closer.Close(); err != nil {
				errs = append(errs, fmt.Errorf("easyraft: close storage: %w", err))
			}
		}
	}

	return errors.Join(errs...)
}

// Leave removes this node from the cluster membership.
//
// On the leader the membership change is proposed directly. A follower cannot
// commit one, so the removal is forwarded to the leader's advertised HTTP API
// instead, carrying the credential from [WithBearerTokenAuth] when one is
// configured. If the leader is unknown, has not advertised an HTTP address, or
// rejects the request, Leave returns an error describing which step failed —
// the node is then still a member and an operator must remove it.
//
// [WithLeaveOnStop] calls Leave automatically during [Store.Stop].
func (s *Store) Leave(ctx context.Context) error {
	if s.node == nil {
		return errors.New("easyraft: node not started")
	}

	err := s.node.RemoveServer(ctx, s.cfg.ID)
	if err == nil {
		return nil
	}
	if !errors.Is(err, ErrNotLeader) {
		return fmt.Errorf("easyraft: remove self from cluster: %w", err)
	}

	leaderID := s.node.Leader()
	if leaderID == "" {
		return fmt.Errorf("easyraft: cannot leave: this node is not the leader and no leader is known")
	}
	target := s.leaderURL(leaderID, "/members/"+string(s.cfg.ID), "")
	if target == "" {
		return fmt.Errorf("easyraft: cannot leave: leader %s has not advertised a usable HTTP address", leaderID)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, target, http.NoBody)
	if err != nil {
		return fmt.Errorf("easyraft: build leave request: %w", err)
	}
	if s.cfg.HTTPCredential != "" {
		req.Header.Set("Authorization", s.cfg.HTTPCredential)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("easyraft: forward leave request to leader %s: %w", leaderID, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))

	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("easyraft: leader %s rejected the leave request with status %d",
			leaderID, resp.StatusCode)
	}
	s.logger().Info("easyraft: left cluster via leader", "leader", leaderID, "node", s.cfg.ID)
	return nil
}

// Collection is a type-safe view into a named namespace within a [Store].
// All keys within a collection are independent of keys in other collections.
// T must be JSON-serialisable.
//
// A Collection is a lightweight handle — it is safe to create multiple
// Collection values pointing at the same name and store.
//
// # Write operations
//
// Most write methods have both a standard and an exactly-once variant:
//   - Standard: [Collection.Create], [Collection.Update], [Collection.Delete],
//     [Collection.Upsert], [Collection.Mutate] — at-least-once on retry.
//   - Exactly-once: [Collection.CreateOnce], [Collection.UpdateOnce],
//     [Collection.DeleteOnce], [Collection.MutateOnce] — deduplicated by
//     (clientID, seqNum); safe to retry after a network failure without
//     risk of double-application. [Collection.Exactly] wraps the same
//     guarantee in named fields.
//
// [Collection.Upsert] has no *Once variant because it is already idempotent:
// applying the same upsert twice produces the same final state.
type Collection[T any] struct {
	store *Store
	name  string
}

// AddCollection returns a typed Collection handle for the given name within s.
// It can be called at any time — before or after Start — and is idempotent for
// the same name and type.
// Collection names starting with "__" are reserved for internal use and will
// panic if used.
func AddCollection[T any](s *Store, name string) *Collection[T] {
	if isReservedCollection(name) {
		panic("easyraft: collection names starting with '__' are reserved")
	}
	return &Collection[T]{
		store: s,
		name:  name,
	}
}

// RegisterMutation registers a named read-modify-write function for this
// collection. Mutations are the correct way to update a value based on its
// current state — a Read followed by Update is not atomic.
//
// fn is called inside [raft.StateMachine].Apply, which runs on every replica
// during both normal operation and log replay after a restart or snapshot
// restore. fn MUST be purely deterministic: do not call time.Now, rand, or
// any external I/O inside fn. If different replicas produce different results
// for the same log entry, the cluster state will silently diverge.
//
// If fn needs the current time or any external value, encode it in the args
// before calling Mutate — all replicas will then receive and apply the same bytes.
//
// If fn returns an error, the state machine is not updated and the error is
// returned to the proposing caller.
//
// Call RegisterMutation before Start.
func (c *Collection[T]) RegisterMutation(name string, fn func(current *T, args []byte) (*T, []byte, error)) {
	c.store.registerMutation(c.name, name, func(currentRaw json.RawMessage, args json.RawMessage) (json.RawMessage, json.RawMessage, error) {
		var current T
		if len(currentRaw) > 0 {
			if err := json.Unmarshal(currentRaw, &current); err != nil {
				return nil, nil, fmt.Errorf("easyraft: unmarshal current: %w", err)
			}
		}

		next, resp, err := fn(&current, args)
		if err != nil {
			return nil, nil, err
		}

		nextRaw, err := json.Marshal(next)
		if err != nil {
			return nil, nil, fmt.Errorf("easyraft: marshal next: %w", err)
		}

		return nextRaw, resp, nil
	})
}

// Create inserts a new item into the collection.
// Returns [ErrKeyExists] if an item with that key already exists.
// Returns [ErrNotLeader] if this node is not the current Raft leader.
func (c *Collection[T]) Create(ctx context.Context, key string, value T) error {
	b, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("easyraft: encode value: %w", err)
	}
	_, err = c.store.propose(ctx, &command{
		Op:         opCreate,
		Collection: c.name,
		Key:        key,
		Value:      b,
	})
	return err
}

// Update replaces an existing item.
// Returns [ErrKeyNotFound] if the key does not exist.
// Returns [ErrNotLeader] if this node is not the current Raft leader.
func (c *Collection[T]) Update(ctx context.Context, key string, value T) error {
	b, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("easyraft: encode value: %w", err)
	}
	_, err = c.store.propose(ctx, &command{
		Op:         opUpdate,
		Collection: c.name,
		Key:        key,
		Value:      b,
	})
	return err
}

// Delete removes an existing item.
// Returns [ErrKeyNotFound] if the key does not exist.
// Returns [ErrNotLeader] if this node is not the current Raft leader.
func (c *Collection[T]) Delete(ctx context.Context, key string) error {
	_, err := c.store.propose(ctx, &command{
		Op:         opDelete,
		Collection: c.name,
		Key:        key,
	})
	return err
}

// Upsert inserts key if it does not exist, or replaces it if it does.
// Unlike [Create] + [Update], Upsert is a single atomic operation that never
// returns [ErrKeyExists] or [ErrKeyNotFound].
// Returns [ErrNotLeader] if this node is not the current Raft leader.
func (c *Collection[T]) Upsert(ctx context.Context, key string, value T) error {
	b, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("easyraft: encode value: %w", err)
	}
	_, err = c.store.propose(ctx, &command{
		Op:         opUpsert,
		Collection: c.name,
		Key:        key,
		Value:      b,
	})
	return err
}

// OnChange registers fn to be called on this node whenever a committed write
// modifies this collection. fn receives the key, the new typed value (nil
// on delete), and a deleted flag. It is called outside the state-machine lock
// after every committed write that touches this collection — on every replica,
// including during log replay after a restart or snapshot restore.
//
// Each collection gets its own dispatcher goroutine, so a slow handler delays
// only its own collection. If it falls far enough behind, events for that
// collection are dropped; OnChange cannot report that. Use
// [Collection.OnChangeEvent] when losing an event silently is not acceptable.
//
// Call before [Store.Start]. Only one handler per collection is supported.
func (c *Collection[T]) OnChange(fn func(key string, value *T, deleted bool)) {
	c.OnChangeEvent(func(ev ChangeEvent[T]) {
		if ev.Gap {
			return // reported only through OnChangeEvent
		}
		fn(ev.Key, ev.Value, ev.Deleted)
	})
}

// OnChangeEvent registers fn to receive the full [ChangeEvent] for every
// committed write to this collection, including the event's Seq and any Gap
// marker.
//
// Seq increases by one per event emitted by this store, so a handler that
// tracks the last Seq it saw can detect a discontinuity. A Gap event reports
// directly that events were dropped because the handler fell behind: its Key
// and Value are empty, and the correct response is to re-read the collection
// rather than to assume nothing was missed.
//
// Call before [Store.Start]. Only one handler per collection is supported; it
// replaces any handler registered by [Collection.OnChange].
func (c *Collection[T]) OnChangeEvent(fn func(ChangeEvent[T])) {
	c.store.onChangeEventRaw(c.name, func(ev rawChangeEvent) {
		out := ChangeEvent[T]{
			Seq:     ev.Seq,
			Key:     ev.Key,
			Deleted: ev.Deleted,
			Gap:     ev.Gap,
		}
		if !ev.Gap && !ev.Deleted {
			var v T
			if err := json.Unmarshal(ev.Value, &v); err != nil {
				return
			}
			out.Value = &v
		}
		fn(out)
	})
}

// CreateOnce inserts a new item with exactly-once semantics. If the network
// drops the response and the caller retries with the same (clientID, seqNum),
// the cached result is returned without applying the command a second time.
// seqNum must increase monotonically per clientID.
//
// [Collection.Exactly] takes the same identity as a named-field [OnceID],
// which cannot be transposed.
func (c *Collection[T]) CreateOnce(ctx context.Context, clientID raft.NodeID, seqNum uint64, key string, value T) error {
	return c.Exactly(OnceID{ClientID: clientID, SeqNum: seqNum}).Create(ctx, key, value)
}

// UpdateOnce replaces an existing item with exactly-once semantics.
// See [Collection.CreateOnce] for the seqNum contract.
func (c *Collection[T]) UpdateOnce(ctx context.Context, clientID raft.NodeID, seqNum uint64, key string, value T) error {
	return c.Exactly(OnceID{ClientID: clientID, SeqNum: seqNum}).Update(ctx, key, value)
}

// DeleteOnce removes an existing item with exactly-once semantics.
// See [Collection.CreateOnce] for the seqNum contract.
func (c *Collection[T]) DeleteOnce(ctx context.Context, clientID raft.NodeID, seqNum uint64, key string) error {
	return c.Exactly(OnceID{ClientID: clientID, SeqNum: seqNum}).Delete(ctx, key)
}

// Mutate executes a named mutation registered with [Collection.RegisterMutation].
// The mutation runs as a single Raft log entry — it is an atomic read-modify-write.
// args is passed verbatim to the mutation function and may be nil.
// Returns the response bytes returned by the mutation function, or an error.
// Returns [ErrKeyNotFound] if the key does not exist.
// Returns [ErrNotLeader] if this node is not the current Raft leader.
func (c *Collection[T]) Mutate(ctx context.Context, key, name string, args []byte) ([]byte, error) {
	return c.store.propose(ctx, &command{
		Op:         opMutate,
		Collection: c.name,
		Key:        key,
		MutateName: name,
		MutateArgs: args,
	})
}

// MutateOnce executes a registered mutation with exactly-once semantics.
// See [Collection.CreateOnce] for the seqNum contract.
func (c *Collection[T]) MutateOnce(ctx context.Context, clientID raft.NodeID, seqNum uint64, key, name string, args []byte) ([]byte, error) {
	return c.Exactly(OnceID{ClientID: clientID, SeqNum: seqNum}).Mutate(ctx, key, name, args)
}

// Read returns an item by key with linearizable consistency. It confirms the
// local state machine is up to date before serving from the local map — with a
// quorum heartbeat by default, or from the leader's read lease when
// [WithLeaseReads] is set.
// Returns [ErrKeyNotFound] if the key does not exist.
// Returns [ErrNotLeader] if this node cannot reach the leader.
func (c *Collection[T]) Read(ctx context.Context, key string) (T, error) {
	var empty T
	if c.store.cfg.Witness {
		return empty, ErrWitness
	}
	if c.store.node == nil {
		return empty, fmt.Errorf("easyraft: node not started")
	}
	if err := c.store.readIndex(ctx); err != nil {
		return empty, fmt.Errorf("easyraft: read index: %w", err)
	}
	return c.ReadStale(key)
}

// ReadStale returns an item by key directly from the local state machine,
// without a leader round-trip. The result may lag behind the cluster by up to
// one heartbeat interval. Use this for read-heavy workloads where bounded
// staleness is acceptable.
// Returns [ErrKeyNotFound] if the key does not exist locally.
func (c *Collection[T]) ReadStale(key string) (T, error) {
	var val T
	if c.store.cfg.Witness {
		return val, ErrWitness
	}
	c.store.mu.RLock()
	defer c.store.mu.RUnlock()

	coll := c.store.collections[c.name]
	if coll == nil {
		return val, ErrKeyNotFound
	}
	raw, ok := coll[key]
	if !ok {
		return val, ErrKeyNotFound
	}

	if err := json.Unmarshal(raw, &val); err != nil {
		return val, fmt.Errorf("easyraft: decode value: %w", err)
	}
	return val, nil
}

// ListStale returns all items in the collection from the local state machine
// without a leader round-trip. The result may be stale by up to one heartbeat.
// Returns an empty map (not nil) if the collection has no items.
func (c *Collection[T]) ListStale() (map[string]T, error) {
	if c.store.cfg.Witness {
		return nil, ErrWitness
	}
	c.store.mu.RLock()
	defer c.store.mu.RUnlock()
	return c.listLocked()
}

// listLocked decodes the whole collection. The caller must hold store.mu.
func (c *Collection[T]) listLocked() (map[string]T, error) {
	out := make(map[string]T)
	coll := c.store.collections[c.name]
	if coll == nil {
		return out, nil
	}

	for k, raw := range coll {
		var v T
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, fmt.Errorf("easyraft: decode key %q: %w", k, err)
		}
		out[k] = v
	}
	return out, nil
}

// List returns all items in the collection with linearizable consistency.
// It confirms the local state machine is current before reading — with a
// quorum heartbeat by default, or from the leader's read lease when
// [WithLeaseReads] is set.
// Returns an empty map (not nil) if the collection has no items.
func (c *Collection[T]) List(ctx context.Context) (map[string]T, error) {
	if c.store.cfg.Witness {
		return nil, ErrWitness
	}
	if c.store.node == nil {
		return nil, fmt.Errorf("easyraft: node not started")
	}
	if err := c.store.readIndex(ctx); err != nil {
		return nil, fmt.Errorf("easyraft: read index: %w", err)
	}

	c.store.mu.RLock()
	defer c.store.mu.RUnlock()
	return c.listLocked()
}

// AddServer registers addr with the transport and adds id as a voting member
// of the Raft cluster. This must be called on the leader; it returns
// [ErrNotLeader] otherwise. Blocks until the membership change is committed.
//
// Use it to promote a learner — one added by [WithDiscovery] or
// [WithJoinAsLearner] — once an operator has verified it.
func (s *Store) AddServer(ctx context.Context, id raft.NodeID, addr string) error {
	if s.node == nil {
		return errors.New("easyraft: node not started")
	}
	if adder, ok := s.transport.(peerAdder); ok {
		adder.AddPeer(id, addr)
	}
	if err := s.node.AddServer(ctx, raft.PeerConfig{ID: id, Voter: true}); err != nil {
		return err
	}
	s.mu.Lock()
	s.raftPeers[id] = raftPeerInfo{addr: addr, voter: true}
	s.mu.Unlock()
	return nil
}

// RemoveServer removes id from the Raft cluster membership. Must be called on
// the leader. If id is the current leader, it commits the removal and then
// steps down; the caller is responsible for stopping the removed node.
// AddWitness brings a witness into the cluster as a voter: it votes and
// counts towards every quorum, keeps the shape of the log, and holds none of
// the data. See [WithWitness], which is how the node at addr must have been
// built.
//
// Unlike a full replica, a witness is added in one step rather than staged in
// as a learner first, because it has almost nothing to catch up on: the index
// and term of each entry, which the leader sends without the entries
// themselves.
func (s *Store) AddWitness(ctx context.Context, id raft.NodeID, addr string) error {
	if s.node == nil {
		return errors.New("easyraft: node not started")
	}
	if adder, ok := s.transport.(peerAdder); ok {
		adder.AddPeer(id, addr)
	}
	if err := s.node.AddWitness(ctx, id); err != nil {
		return err
	}
	s.mu.Lock()
	s.raftPeers[id] = raftPeerInfo{addr: addr, voter: true}
	s.mu.Unlock()
	return nil
}

// CommitQuorum returns how many voters an entry must currently reach to
// commit, or 0 while the group uses a simple majority. It is the group's
// agreed value, not this node's [WithCommitQuorum].
func (s *Store) CommitQuorum() int {
	if s.node == nil {
		return 0
	}
	return s.node.CommitQuorum()
}

// SetCommitQuorum sets how many voters an entry must reach to commit, for the
// whole group, and with it the election quorum. A quorum of 0 restores a
// simple majority. It must be called on the leader.
//
// Writing to every replica means no acknowledged write is ever on fewer than
// that many disks, at the cost of stalling writes when any one voter is down.
// A quorum below a majority makes writes need fewer acknowledgements than a
// leader needs votes. Neither changes what is safe.
func (s *Store) SetCommitQuorum(ctx context.Context, quorum int) error {
	if s.node == nil {
		return errors.New("easyraft: node not started")
	}
	return s.node.SetCommitQuorum(ctx, quorum)
}

// MaxClientTableSize returns the bound the exactly-once table behind
// [Session] is kept under, 0 meaning none. It is the group's agreed value.
func (s *Store) MaxClientTableSize() int {
	if s.node == nil {
		return 0
	}
	return s.node.MaxClientTableSize()
}

// SetMaxClientTableSize changes that bound for the whole group. It must be
// called on the leader.
//
// A smaller bound forgets the oldest clients everywhere at the same point in
// the log, and a forgotten client's retry runs a second time. A larger one
// takes effect from here on and recovers nothing already forgotten.
func (s *Store) SetMaxClientTableSize(ctx context.Context, size int) error {
	if s.node == nil {
		return errors.New("easyraft: node not started")
	}
	return s.node.SetMaxClientTableSize(ctx, size)
}

// Events returns a channel reporting what happens to this node -- leadership
// changes, peers arriving and leaving, snapshots, a client dropped from the
// exactly-once table, a durable write that failed -- and a function that ends
// the subscription.
//
// The channel is bounded and lossy: a consumer that falls behind is told how
// many events it missed in Event.Dropped rather than slowing the node down.
// The subscription must be released with the returned function, which never
// blocks and may be called more than once.
func (s *Store) Events() (events <-chan raft.Event, stop func()) {
	if s.node == nil {
		ch := make(chan raft.Event)
		close(ch)
		return ch, func() {}
	}
	return s.node.Events()
}

func (s *Store) RemoveServer(ctx context.Context, id raft.NodeID) error {
	if s.node == nil {
		return errors.New("easyraft: node not started")
	}
	if err := s.node.RemoveServer(ctx, id); err != nil {
		return err
	}
	s.mu.Lock()
	delete(s.raftPeers, id)
	s.mu.Unlock()
	return nil
}

// TransferLeadership gracefully hands off leadership to to. The leader waits
// for to to catch up on the log, then sends a TimeoutNow RPC that causes it
// to start an election immediately. Blocks until this node steps down or the
// context expires.
func (s *Store) TransferLeadership(ctx context.Context, to raft.NodeID) error {
	if s.node == nil {
		return errors.New("easyraft: node not started")
	}
	return s.node.TransferLeadership(ctx, to)
}

// mutationFunc is the low-level, untyped mutation callback stored in the
// registry. It receives the current value and args as raw JSON and must return
// the new value and an optional response, both as raw JSON.
//
// Use [Collection.RegisterMutation] to register a typed wrapper instead of
// implementing mutationFunc directly.
//
// If an error is returned, the state machine is NOT updated.
// mutationFunc MUST be purely deterministic — see [Collection.RegisterMutation].
type mutationFunc func(currentValue json.RawMessage, args json.RawMessage) (newValue json.RawMessage, response json.RawMessage, err error)

func (s *Store) registerMutation(collection, name string, fn mutationFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mutations[collection] == nil {
		s.mutations[collection] = make(map[string]mutationFunc)
	}
	s.mutations[collection][name] = fn
}

// storeFSM is the raft.StateMachine a Store hands to its Raft node.
//
// It exists so that Store does not implement the interface itself. Apply,
// Snapshot and Restore have to be exported to satisfy it, and on Store they
// would sit among the methods an application is meant to call, with nothing to
// tell them apart -- while calling any of them directly applies a change
// consensus never agreed to, on this replica alone, leaving this node's state
// permanently different from every other node's and nothing in the log to
// explain it. A method that dangerous should not be reachable by autocomplete.
//
// The adapter costs one struct and gives Store a surface where everything
// exported is safe to call. Use Txn or the Collection API to change state.
type storeFSM struct {
	s *Store
}

func (f storeFSM) Apply(_ context.Context, entry raft.LogEntry) ([]byte, error) {
	return f.s.applyEntry(entry)
}

func (f storeFSM) Snapshot(_ context.Context, w io.Writer) error {
	return f.s.snapshot(w)
}

func (f storeFSM) Restore(_ context.Context, _ raft.SnapshotMeta, r io.Reader) error {
	return f.s.restore(r)
}

// applyEntry puts one committed entry into effect. Reached only through
// storeFSM, which is what the Raft layer holds.
func (s *Store) applyEntry(entry raft.LogEntry) ([]byte, error) {
	var cmd command
	if err := json.Unmarshal(entry.Command, &cmd); err != nil {
		return nil, fmt.Errorf("easyraft: decode cmd: %w", err)
	}

	s.mu.Lock()
	s.pendingEvents = s.pendingEvents[:0]
	result, err := s.applyCommand(&cmd, nil)
	// Drop events for collections nobody is listening to — the internal
	// metadata collection, most of all — so they cannot take up queue room
	// that a watched collection's events need, or spawn a dispatcher for a
	// collection that will never have a handler.
	events := s.pendingEvents[:0]
	for _, ev := range s.pendingEvents {
		if s.onChangeFns[ev.collection] != nil {
			events = append(events, ev)
		}
	}
	// Steal the pending events slice before releasing the lock so the
	// dispatcher sees a consistent snapshot. Assign nil so the next Apply
	// starts with a fresh allocation rather than aliasing this one.
	s.pendingEvents = nil
	s.mu.Unlock()

	for i := range events {
		events[i].seq = s.eventSeq.Add(1)
		s.enqueueEvent(&events[i])
	}
	return result, err
}

// undoEntry records one key's value before a batch touched it, so a failed
// batch can be rolled back without copying whole collections.
type undoEntry struct {
	collection string
	key        string
	previous   json.RawMessage
	existed    bool
}

// batchUndo is the rollback journal for one transaction. It grows with the
// number of keys the transaction touches, not with the size of the collections
// it touches.
type batchUndo struct {
	entries []undoEntry
	// created lists collections the batch brought into existence, which must
	// disappear again if it is rolled back.
	created []string
}

// record captures the pre-batch value of key so it can be restored. Repeated
// writes to the same key each append an entry; replaying the journal in
// reverse therefore restores the earliest value.
func (u *batchUndo) record(collection, key string, coll map[string]json.RawMessage) {
	previous, existed := coll[key]
	u.entries = append(u.entries, undoEntry{
		collection: collection,
		key:        key,
		previous:   previous,
		existed:    existed,
	})
}

// rollback undoes every recorded mutation, most recent first.
func (u *batchUndo) rollback(collections map[string]map[string]json.RawMessage) {
	for i := len(u.entries) - 1; i >= 0; i-- {
		e := u.entries[i]
		coll := collections[e.collection]
		if coll == nil {
			continue
		}
		if e.existed {
			coll[e.key] = e.previous
		} else {
			delete(coll, e.key)
		}
	}
	for _, name := range u.created {
		delete(collections, name)
	}
}

// applyCommand applies cmd to the state machine. undo is non-nil while a batch
// is in flight; each mutation then journals the single key it is about to
// overwrite, which is what makes a rollback cost O(keys touched) rather than
// O(collection size).
func (s *Store) applyCommand(cmd *command, undo *batchUndo) ([]byte, error) {
	if cmd.Op == opBatch {
		// A nested batch shares the enclosing journal: the outermost batch owns
		// the rollback.
		if undo != nil {
			results := make([]json.RawMessage, 0, len(cmd.Batch))
			for i := range cmd.Batch {
				res, err := s.applyCommand(&cmd.Batch[i], undo)
				if err != nil {
					return nil, err
				}
				results = append(results, res)
			}
			return json.Marshal(results)
		}

		journal := &batchUndo{}
		preBatchEventsLen := len(s.pendingEvents)

		results := make([]json.RawMessage, 0, len(cmd.Batch))
		for i := range cmd.Batch {
			res, err := s.applyCommand(&cmd.Batch[i], journal)
			if err != nil {
				journal.rollback(s.collections)
				// Discard any partial change events emitted during the batch.
				s.pendingEvents = s.pendingEvents[:preBatchEventsLen]
				return nil, err
			}
			results = append(results, res)
		}
		return json.Marshal(results)
	}

	coll, exists := s.collections[cmd.Collection]
	if !exists {
		coll = make(map[string]json.RawMessage)
		s.collections[cmd.Collection] = coll
		if undo != nil {
			undo.created = append(undo.created, cmd.Collection)
		}
	}

	switch cmd.Op {
	case opCreate:
		if _, ok := coll[cmd.Key]; ok {
			return nil, ErrKeyExists
		}
		if undo != nil {
			undo.record(cmd.Collection, cmd.Key, coll)
		}
		coll[cmd.Key] = cmd.Value
		s.pendingEvents = append(s.pendingEvents, changeEvent{collection: cmd.Collection, key: cmd.Key, value: cmd.Value})
		return nil, nil

	case opUpdate:
		if _, ok := coll[cmd.Key]; !ok {
			return nil, ErrKeyNotFound
		}
		if undo != nil {
			undo.record(cmd.Collection, cmd.Key, coll)
		}
		coll[cmd.Key] = cmd.Value
		s.pendingEvents = append(s.pendingEvents, changeEvent{collection: cmd.Collection, key: cmd.Key, value: cmd.Value})
		return nil, nil

	case opUpsert:
		if undo != nil {
			undo.record(cmd.Collection, cmd.Key, coll)
		}
		coll[cmd.Key] = cmd.Value
		s.pendingEvents = append(s.pendingEvents, changeEvent{collection: cmd.Collection, key: cmd.Key, value: cmd.Value})
		return nil, nil

	case opDelete:
		if _, ok := coll[cmd.Key]; !ok {
			return nil, ErrKeyNotFound
		}
		if undo != nil {
			undo.record(cmd.Collection, cmd.Key, coll)
		}
		delete(coll, cmd.Key)
		s.pendingEvents = append(s.pendingEvents, changeEvent{collection: cmd.Collection, key: cmd.Key, deleted: true})
		return nil, nil

	case opMutate:
		currentVal, ok := coll[cmd.Key]
		if !ok {
			return nil, ErrKeyNotFound
		}

		collMutations := s.mutations[cmd.Collection]
		if collMutations == nil {
			return nil, fmt.Errorf("easyraft: unknown mutation collection %q", cmd.Collection)
		}
		fn, ok := collMutations[cmd.MutateName]
		if !ok {
			return nil, fmt.Errorf("easyraft: unknown mutation %q in collection %q", cmd.MutateName, cmd.Collection)
		}

		newVal, resp, err := fn(currentVal, cmd.MutateArgs)
		if err != nil {
			return nil, err
		}
		if undo != nil {
			undo.record(cmd.Collection, cmd.Key, coll)
		}
		coll[cmd.Key] = newVal
		s.pendingEvents = append(s.pendingEvents, changeEvent{collection: cmd.Collection, key: cmd.Key, value: newVal})
		return resp, nil

	default:
		return nil, fmt.Errorf("easyraft: unknown op %q", cmd.Op)
	}
}

// snapshot streams the whole store to w as a JSON object of collections.
//
// The encoding is written incrementally, so a snapshot costs one pass over the
// state rather than a full in-memory copy plus a full encoded buffer. Keys are
// emitted in sorted order, which keeps snapshots of identical state
// byte-identical.
//
// The read lock is held for the whole pass, so local reads wait while a
// snapshot is written; that is the price of not duplicating the state. Raft
// calls Snapshot from the same goroutine that applies entries, so writes are
// not additionally delayed by it.
func (s *Store) snapshot(w io.Writer) error {
	bw := bufio.NewWriterSize(w, 64<<10)

	s.mu.RLock()
	err := streamCollections(bw, s.collections)
	s.mu.RUnlock()

	if err != nil {
		return fmt.Errorf("easyraft: snapshot encode: %w", err)
	}
	if err := bw.Flush(); err != nil {
		return fmt.Errorf("easyraft: snapshot flush: %w", err)
	}
	return nil
}

// streamCollections writes collections as a JSON object without materialising
// an intermediate copy of the data.
func streamCollections(w *bufio.Writer, collections map[string]map[string]json.RawMessage) error {
	if _, err := w.WriteString("{"); err != nil {
		return err
	}
	for i, name := range slices.Sorted(maps.Keys(collections)) {
		if i > 0 {
			if _, err := w.WriteString(","); err != nil {
				return err
			}
		}
		if err := writeJSONString(w, name); err != nil {
			return err
		}
		if _, err := w.WriteString(":{"); err != nil {
			return err
		}
		items := collections[name]
		for j, key := range slices.Sorted(maps.Keys(items)) {
			if j > 0 {
				if _, err := w.WriteString(","); err != nil {
					return err
				}
			}
			if err := writeJSONString(w, key); err != nil {
				return err
			}
			if _, err := w.WriteString(":"); err != nil {
				return err
			}
			value := items[key]
			if len(value) == 0 {
				value = json.RawMessage("null")
			}
			if _, err := w.Write(value); err != nil {
				return err
			}
		}
		if _, err := w.WriteString("}"); err != nil {
			return err
		}
	}
	_, err := w.WriteString("}\n")
	return err
}

// writeJSONString writes s as a JSON string literal.
func writeJSONString(w *bufio.Writer, s string) error {
	encoded, err := json.Marshal(s)
	if err != nil {
		return err
	}
	_, err = w.Write(encoded)
	return err
}

// restore replaces the state machine contents with the snapshot in r.
//
// The snapshot is decoded one collection at a time rather than as a single
// value, so peak memory tracks the largest collection instead of the whole
// store plus its encoded form.
func (s *Store) restore(r io.Reader) error {
	dec := json.NewDecoder(bufio.NewReaderSize(r, 64<<10))

	collections, err := decodeCollections(dec)
	if err != nil {
		return fmt.Errorf("easyraft: restore decode: %w", err)
	}

	s.mu.Lock()
	s.collections = collections
	s.mu.Unlock()
	return nil
}

// decodeCollections reads a snapshot body, tolerating both an empty stream and
// an explicit JSON null (either of which means "no state").
func decodeCollections(dec *json.Decoder) (map[string]map[string]json.RawMessage, error) {
	collections := make(map[string]map[string]json.RawMessage)

	tok, err := dec.Token()
	if errors.Is(err, io.EOF) {
		return collections, nil
	}
	if err != nil {
		return nil, err
	}
	if tok == nil {
		return collections, nil // JSON null
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return nil, fmt.Errorf("expected a JSON object, found %v", tok)
	}

	for dec.More() {
		nameTok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		name, ok := nameTok.(string)
		if !ok {
			return nil, fmt.Errorf("expected a collection name, found %v", nameTok)
		}
		var items map[string]json.RawMessage
		if err := dec.Decode(&items); err != nil {
			return nil, fmt.Errorf("collection %q: %w", name, err)
		}
		if items == nil {
			items = make(map[string]json.RawMessage)
		}
		collections[name] = items
	}

	if _, err := dec.Token(); err != nil { // closing brace
		return nil, err
	}
	return collections, nil
}

// raftTickInterval returns the configured tick interval, falling back to the
// easyraft default of 100 ms (more conservative than the raft package default).
func (s *Store) raftTickInterval() time.Duration {
	if s.cfg.TickInterval > 0 {
		return s.cfg.TickInterval
	}
	return 100 * time.Millisecond
}

func (s *Store) raftHeartbeatInterval() time.Duration {
	if s.cfg.HeartbeatInterval > 0 {
		return s.cfg.HeartbeatInterval
	}
	return 100 * time.Millisecond
}

func (s *Store) raftElectionTimeoutMin() time.Duration {
	if s.cfg.ElectionTimeoutMin > 0 {
		return s.cfg.ElectionTimeoutMin
	}
	return 1 * time.Second
}

func (s *Store) raftElectionTimeoutMax() time.Duration {
	if s.cfg.ElectionTimeoutMax > 0 {
		return s.cfg.ElectionTimeoutMax
	}
	return 2 * time.Second
}

// marshalValue encodes a collection value, wrapping the failure so the caller
// sees which layer rejected it.
func marshalValue(value any) (json.RawMessage, error) {
	b, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("easyraft: encode value: %w", err)
	}
	return b, nil
}

// propose encodes a command and proposes it to the Raft node.
func (s *Store) propose(ctx context.Context, cmd *command) ([]byte, error) {
	if s.node == nil {
		return nil, errors.New("easyraft: node not started")
	}

	b, err := json.Marshal(cmd)
	if err != nil {
		return nil, fmt.Errorf("easyraft: encode cmd: %w", err)
	}

	return s.node.Propose(ctx, b)
}

// proposeOnce submits a command for replication with exactly-once semantics.
func (s *Store) proposeOnce(ctx context.Context, clientID raft.NodeID, seqNum uint64, cmd *command) ([]byte, error) {
	if s.node == nil {
		return nil, errors.New("easyraft: node not started")
	}

	b, err := json.Marshal(cmd)
	if err != nil {
		return nil, fmt.Errorf("easyraft: encode cmd: %w", err)
	}

	return s.node.ProposeOnce(ctx, clientID, seqNum, b)
}
