package easyraft

import (
	"context"
	"sync/atomic"

	"github.com/brunoga/raft"
)

// OnceID identifies a single logical write for exactly-once deduplication.
//
// It is the named-field form of the (clientID, seqNum) pair taken by
// [Collection.CreateOnce] and friends, which is easy to transpose because both
// arguments sit before the key. Build one directly, or let a [Session] hand
// them out:
//
//	id := session.Next()
//	err := accounts.Exactly(id).Update(ctx, "alice", updated)
//
// Hold on to the value across retries: replaying the same OnceID is what makes
// the write exactly-once. Allocating a fresh one for a retry would apply the
// command twice.
type OnceID struct {
	// ClientID names the writer. Every writer must use an ID of its own;
	// deduplication state is tracked per client.
	ClientID raft.NodeID

	// SeqNum must increase monotonically for a given ClientID. A sequence
	// number below one already recorded is rejected with
	// [raft.ErrObsoleteSeqNum].
	SeqNum uint64
}

// Session hands out monotonically increasing [OnceID] values for one client.
// It is safe for concurrent use.
//
//	session := easyraft.NewSession("worker-7")
//	id := session.Next()
//	for {
//	    err := orders.Exactly(id).Create(ctx, key, order)
//	    if err == nil || !retryable(err) {
//	        break // the same id on every attempt: applied at most once
//	    }
//	}
//
// Sequence numbers restart at one for a new Session, so a process that
// restarts and keeps the same client ID must resume from where it left off —
// use [NewSessionAt] with the last sequence number it durably recorded.
type Session struct {
	clientID raft.NodeID
	seq      atomic.Uint64
}

// NewSession returns a Session for clientID whose first [Session.Next] yields
// sequence number 1.
func NewSession(clientID raft.NodeID) *Session {
	return &Session{clientID: clientID}
}

// NewSessionAt returns a Session for clientID that resumes numbering after
// lastSeqNum, so the first [Session.Next] yields lastSeqNum+1. Use it when a
// restarted process reuses a client ID whose progress it recorded elsewhere.
func NewSessionAt(clientID raft.NodeID, lastSeqNum uint64) *Session {
	s := &Session{clientID: clientID}
	s.seq.Store(lastSeqNum)
	return s
}

// ClientID returns the client this Session numbers writes for.
func (s *Session) ClientID() raft.NodeID { return s.clientID }

// Next allocates the identity of one logical write. Reuse the returned value
// for every retry of that write.
func (s *Session) Next() OnceID {
	return OnceID{ClientID: s.clientID, SeqNum: s.seq.Add(1)}
}

// LastSeqNum returns the highest sequence number handed out so far. Record it
// if the client may restart and resume with [NewSessionAt].
func (s *Session) LastSeqNum() uint64 { return s.seq.Load() }

// ExactlyOnce is a view of a [Collection] whose writes all carry one [OnceID].
// Obtain one with [Collection.Exactly].
type ExactlyOnce[T any] struct {
	collection *Collection[T]
	id         OnceID
}

// Exactly returns a view of c whose writes are deduplicated under id. It is
// the readable spelling of the *Once methods: the identity is named rather
// than positional, so it cannot be transposed with the key.
//
//	err := users.Exactly(id).Create(ctx, "alice", user)
//	// equivalent to users.CreateOnce(ctx, id.ClientID, id.SeqNum, "alice", user)
func (c *Collection[T]) Exactly(id OnceID) ExactlyOnce[T] {
	return ExactlyOnce[T]{collection: c, id: id}
}

// Create inserts a new item exactly once. See [Collection.CreateOnce].
func (e ExactlyOnce[T]) Create(ctx context.Context, key string, value T) error {
	b, err := marshalValue(value)
	if err != nil {
		return err
	}
	_, err = e.propose(ctx, &command{
		Op:         opCreate,
		Collection: e.collection.name,
		Key:        key,
		Value:      b,
	})
	return err
}

// Update replaces an existing item exactly once. See [Collection.UpdateOnce].
func (e ExactlyOnce[T]) Update(ctx context.Context, key string, value T) error {
	b, err := marshalValue(value)
	if err != nil {
		return err
	}
	_, err = e.propose(ctx, &command{
		Op:         opUpdate,
		Collection: e.collection.name,
		Key:        key,
		Value:      b,
	})
	return err
}

// Upsert inserts or replaces an item exactly once. Upsert is idempotent on its
// own; the deduplication matters only when it shares a sequence space with
// other writes from the same client.
func (e ExactlyOnce[T]) Upsert(ctx context.Context, key string, value T) error {
	b, err := marshalValue(value)
	if err != nil {
		return err
	}
	_, err = e.propose(ctx, &command{
		Op:         opUpsert,
		Collection: e.collection.name,
		Key:        key,
		Value:      b,
	})
	return err
}

// Delete removes an existing item exactly once. See [Collection.DeleteOnce].
func (e ExactlyOnce[T]) Delete(ctx context.Context, key string) error {
	_, err := e.propose(ctx, &command{
		Op:         opDelete,
		Collection: e.collection.name,
		Key:        key,
	})
	return err
}

// Mutate runs a registered mutation exactly once. See [Collection.MutateOnce].
func (e ExactlyOnce[T]) Mutate(ctx context.Context, key, name string, args []byte) ([]byte, error) {
	return e.propose(ctx, &command{
		Op:         opMutate,
		Collection: e.collection.name,
		Key:        key,
		MutateName: name,
		MutateArgs: args,
	})
}

func (e ExactlyOnce[T]) propose(ctx context.Context, cmd *command) ([]byte, error) {
	return e.collection.store.proposeOnce(ctx, e.id.ClientID, e.id.SeqNum, cmd)
}
