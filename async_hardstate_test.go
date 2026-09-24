package raft_test

import (
	"context"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/brunoga/raft/v2"
)

// These tests are about the other write the event loop used to wait on: the
// term and the vote.
//
// It is the write Raft's election safety rests on entirely. A node that votes,
// crashes, and comes back having forgotten the vote is free to vote again in
// the same term, and two leaders in one term follows -- so the write cannot
// simply be moved off the loop and forgotten about. What moves off the loop is
// the waiting. The term takes effect in memory at once, and what is held back
// is every message that would tell another node about it, because a message
// outlives the crash and the memory does not.
//
// The distinction that makes it worth doing is the pre-vote, which changes no
// persistent state and is answered without touching the disk at all. A cluster
// whose disks are busy can still work out who should stand for election.

// TestAsyncHardState_APreVoteIsAnsweredWhileAVoteIsStillBeingWritten is both
// halves of the point at once: the loop keeps running, and the one message
// that is allowed not to wait does not wait.
//
// Elections are when a cluster can least afford its nodes to stop answering,
// because every node is writing a term at the same time, and a node that
// blocks while doing so cannot tell anyone it is alive -- which starts the
// next election on top of the one already in progress.
//
// A pre-vote is the answer that can be given immediately, and it is safe
// precisely because it promises nothing: a pre-vote grant is recorded nowhere,
// so a crash has nothing to lose. It is also the answer that most needs not to
// wait. Pre-vote exists to stop a node that cannot win from disrupting a
// healthy cluster, and it can only do that if a cluster whose disks are busy
// can still work out who should stand.
func TestAsyncHardState_APreVoteIsAnsweredWhileAVoteIsStillBeingWritten(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		node, store := newGatedFollower(t, nil)

		release := store.holdHardState(t)
		defer release()

		// A vote request at a higher term forces a hard-state write, which the
		// store now holds open.
		go func() {
			_, _ = node.Handler().HandleRequestVote(context.Background(), &raft.RequestVoteRequest{
				Term:        5,
				CandidateID: "n2",
			})
		}()
		store.awaitHeld(t)

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		resp, err := node.Handler().HandleRequestVote(ctx, &raft.RequestVoteRequest{
			Term:        6,
			CandidateID: "n3",
			PreVote:     true,
		})
		if err != nil {
			t.Fatalf("a pre-vote was not answered while a vote was being written: %v", err)
		}
		if resp == nil {
			t.Fatal("nil pre-vote response while a vote was being written")
		}
		if ops := store.operations(); len(ops) != 0 {
			t.Errorf("storage recorded %v; a pre-vote must change nothing and the held vote must not have landed", ops)
		}

		// And the node is still tracking time, which is what elections depend on.
		node.Tick()
		if err := node.FatalError(); err != nil {
			t.Fatalf("node failed while a write was merely slow: %v", err)
		}
		if got := node.Term(); got != 5 {
			t.Errorf("Term() = %d while the write was outstanding, want 5: the term takes effect immediately", got)
		}
	})
}

// TestAsyncHardState_AVoteIsNotGrantedUntilItIsOnDisk is the safety property.
//
// A granted vote is a promise not to vote for anyone else in this term, and
// the only thing that can hold that promise across a crash is the write. A
// node that answered first and crashed second would have promised something it
// no longer remembers, and the candidate it promised may already have counted
// it towards a majority.
func TestAsyncHardState_AVoteIsNotGrantedUntilItIsOnDisk(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		node, store := newGatedFollower(t, nil)

		release := store.holdHardState(t)

		type vote struct {
			resp *raft.RequestVoteResponse
			err  error
		}
		answered := make(chan vote, 1)
		go func() {
			resp, err := node.Handler().HandleRequestVote(context.Background(), &raft.RequestVoteRequest{
				Term:        5,
				CandidateID: "n2",
			})
			answered <- vote{resp, err}
		}()
		store.awaitHeld(t)

		select {
		case got := <-answered:
			t.Fatalf("the vote was answered before it was written: %+v (err %v)", got.resp, got.err)
		case <-time.After(150 * time.Millisecond):
		}

		release()

		select {
		case got := <-answered:
			if got.err != nil {
				t.Fatalf("vote response returned an error: %v", got.err)
			}
			if !got.resp.VoteGranted {
				t.Fatalf("the vote was not granted: %+v", got.resp)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("no vote response after the write was released")
		}

		ops := store.operations()
		if len(ops) == 0 || !strings.HasPrefix(ops[0], "hardstate(term=5,vote=n2)") {
			t.Fatalf("storage operations = %v, want the vote written before it was granted", ops)
		}
	})
}

// TestAsyncHardState_ACandidateWaitsForItsOwnVoteBeforeSolicitingOthers covers
// the message that is easiest to forget, because nothing asked for it.
//
// Standing for election is a vote for oneself. A node that sends RequestVote
// for a term, crashes, and comes back having forgotten it can vote for someone
// else in that same term -- so the request has to wait for the write just as a
// granted vote does.
func TestAsyncHardState_ACandidateWaitsForItsOwnVoteBeforeSolicitingOthers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		transport := &countingVoteTransport{realVotes: make(chan raft.Term, 8)}
		store := newGateStore()

		cfg := raft.DefaultConfig()
		cfg.ID = "n1"
		cfg.Peers = []raft.PeerConfig{{ID: "n2", Voter: true}, {ID: "n3", Voter: true}}
		cfg.Storage = store
		cfg.StateMachine = idleSM{}
		cfg.Transport = transport
		cfg.TickInterval = 0

		node, err := raft.New(&cfg)
		if err != nil {
			t.Fatalf("raft.New: %v", err)
		}
		node.Start()
		t.Cleanup(node.Stop)

		release := store.holdHardState(t)

		// Tick past the election timeout. The pre-vote round needs no write and
		// will be granted, which takes the node into a real election.
		stop := tickWhile(node)
		store.awaitHeld(t)

		// Pre-votes may have gone out; a real vote request must not have.
		select {
		case term := <-transport.realVotes:
			stop()
			t.Fatalf("the node solicited votes for term %d before its own vote was written", term)
		case <-time.After(200 * time.Millisecond):
		}

		release()

		select {
		case <-transport.realVotes:
		case <-time.After(5 * time.Second):
			stop()
			t.Fatal("the node never solicited votes after its own vote was written")
		}
		stop()

		if ops := store.operations(); len(ops) == 0 || !strings.HasPrefix(ops[0], "hardstate(term=1,vote=n1)") {
			t.Errorf("storage operations = %v, want the self-vote written first", ops)
		}
	})
}

// TestAsyncHardState_AnAcknowledgementWaitsForTheTermThatMadeIt covers the case
// with no log write to hide behind.
//
// A heartbeat carries no entries, so there is nothing to append and nothing to
// wait for on that account. It can still move this node to a new term, and the
// acknowledgement says so: a leader counts it, and a node that forgot the term
// after a crash could go on to vote for a different candidate in it.
func TestAsyncHardState_AnAcknowledgementWaitsForTheTermThatMadeIt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		node, store := newGatedFollower(t, nil)

		release := store.holdHardState(t)

		acked := make(chan *raft.AppendEntriesResponse, 1)
		go func() {
			resp, _ := node.Handler().HandleAppendEntries(context.Background(), &raft.AppendEntriesRequest{
				Term:     7,
				LeaderID: "n2",
			})
			acked <- resp
		}()
		store.awaitHeld(t)

		select {
		case got := <-acked:
			t.Fatalf("a heartbeat was acknowledged at term 7 before that term was written: %+v", got)
		case <-time.After(150 * time.Millisecond):
		}

		release()

		select {
		case got := <-acked:
			if got == nil || !got.Success {
				t.Fatalf("heartbeat acknowledgement = %+v, want success", got)
			}
			if got.Term != 7 {
				t.Errorf("acknowledgement carried term %d, want 7", got.Term)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("no acknowledgement after the write was released")
		}
	})
}

// TestAsyncHardState_TermsReachStorageBeforeTheEntriesWrittenInThem pins the
// ordering between the two kinds of write.
//
// A log recovered with entries from a term the node does not believe it ever
// reached describes a node that cannot have written them. The two writes go
// through one queue in the order they were issued, and the term is always
// issued first, so that cannot happen.
func TestAsyncHardState_TermsReachStorageBeforeTheEntriesWrittenInThem(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		node, store := newGatedFollower(t, nil)

		resp, err := node.Handler().HandleAppendEntries(context.Background(), appendFrom(3, 1, 2, 0))
		if err != nil {
			t.Fatalf("append: %v", err)
		}
		if !resp.Success {
			t.Fatalf("append was rejected: %+v", resp)
		}

		ops := store.operations()
		if len(ops) < 2 {
			t.Fatalf("storage operations = %v, want a hard-state write and an append", ops)
		}
		if !strings.HasPrefix(ops[0], "hardstate(term=3") {
			t.Errorf("storage operations = %v, want the term written first", ops)
		}
		if ops[1] != "append(1..2)" {
			t.Errorf("storage operations = %v, want the entries written after the term", ops)
		}
	})
}

// countingVoteTransport reports the terms it was asked to solicit real votes
// for, and grants everything so an election can complete.
type countingVoteTransport struct {
	realVotes chan raft.Term
}

func (t *countingVoteTransport) RequestVote(_ context.Context, _ raft.NodeID, req *raft.RequestVoteRequest) (*raft.RequestVoteResponse, error) {
	term := req.Term
	if req.PreVote {
		// A follower answering a pre-vote has not moved to the new term.
		return &raft.RequestVoteResponse{Term: term - 1, VoteGranted: true}, nil
	}
	select {
	case t.realVotes <- term:
	default:
	}
	return &raft.RequestVoteResponse{Term: term, VoteGranted: true}, nil
}

func (t *countingVoteTransport) AppendEntries(_ context.Context, _ raft.NodeID, req *raft.AppendEntriesRequest) (*raft.AppendEntriesResponse, error) {
	return &raft.AppendEntriesResponse{Term: req.Term, Success: true}, nil
}

func (t *countingVoteTransport) InstallSnapshot(_ context.Context, _ raft.NodeID, req *raft.InstallSnapshotRequest) (*raft.InstallSnapshotResponse, error) {
	return &raft.InstallSnapshotResponse{Term: req.Term}, nil
}

func (t *countingVoteTransport) TimeoutNow(_ context.Context, _ raft.NodeID, req *raft.TimeoutNowRequest) (*raft.TimeoutNowResponse, error) {
	return &raft.TimeoutNowResponse{Term: req.Term}, nil
}

func (t *countingVoteTransport) ReadIndex(_ context.Context, _ raft.NodeID, _ *raft.ReadIndexRequest) (*raft.ReadIndexResponse, error) {
	return &raft.ReadIndexResponse{}, nil
}

func (t *countingVoteTransport) Register(_ raft.NodeID, _ raft.Handler) {}
func (t *countingVoteTransport) Unregister(_ raft.NodeID)               {}
func (t *countingVoteTransport) Close() error                           { return nil }

// TestAsyncHardState_ASingleVoterDoesNotLeadInATermItHasNotWritten covers the
// candidacy that never sends a vote request, and so is easy to miss.
//
// A single voter wins its election outright, in the same turn it raises its
// term. If it started leading straight away it would replicate in a term it
// might forget: crashing before the write lands takes it back to the previous
// term, and it then wins the very same term again and appends *different*
// entries at the same indices with the same term. Two different entries at one
// index and term is a Log Matching violation, and no later replication repairs
// it — the conflict detection that would catch it works by comparing terms.
func TestAsyncHardState_ASingleVoterDoesNotLeadInATermItHasNotWritten(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := newGateStore()

		cfg := raft.DefaultConfig()
		cfg.ID = "solo"
		cfg.Storage = store
		cfg.StateMachine = idleSM{}
		cfg.Transport = &echoTransport{sent: make(chan raft.Index, 8)}
		cfg.TickInterval = 0

		node, err := raft.New(&cfg)
		if err != nil {
			t.Fatalf("raft.New: %v", err)
		}
		node.Start()
		t.Cleanup(node.Stop)

		release := store.holdHardState(t)

		stop := tickWhile(node)
		defer stop()
		store.awaitHeld(t)

		time.Sleep(200 * time.Millisecond)
		if got := node.State(); got == raft.Leader {
			t.Fatal("the node started leading in a term it had not written")
		}

		release()

		deadline := time.Now().Add(5 * time.Second)
		for node.State() != raft.Leader {
			if time.Now().After(deadline) {
				t.Fatal("the node never became leader after its term was written")
			}
			time.Sleep(time.Millisecond)
		}

		if ops := store.operations(); len(ops) == 0 || !strings.HasPrefix(ops[0], "hardstate(term=1,vote=solo)") {
			t.Errorf("storage operations = %v, want the term and self-vote written first", ops)
		}
	})
}

// TestAsyncHardState_ASettledTermCostsNothing pins that the gate is not
// permanently on.
//
// Every reply waits for the term it carries, and in the steady state that term
// was written long ago, so there is nothing to wait for. If the gate stayed
// armed after the write landed, every heartbeat in a settled cluster would
// queue behind the disk and the change would have made things worse rather
// than better: this is the case that happens millions of times for each one
// the gate exists for.
func TestAsyncHardState_ASettledTermCostsNothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		node, store := newGatedFollower(t, nil)

		// Adopt term 4 and let the write land.
		if _, err := node.Handler().HandleAppendEntries(context.Background(), &raft.AppendEntriesRequest{
			Term:     4,
			LeaderID: "n2",
		}); err != nil {
			t.Fatalf("first heartbeat: %v", err)
		}

		// Now hold every kind of write open. A heartbeat in the same term needs
		// none of them.
		store.hold(t)
		store.holdHardState(t)

		for i := range 5 {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			resp, err := node.Handler().HandleAppendEntries(ctx, &raft.AppendEntriesRequest{
				Term:     4,
				LeaderID: "n2",
			})
			cancel()
			if err != nil {
				t.Fatalf("heartbeat %d was not answered with every storage write held: %v", i, err)
			}
			if !resp.Success {
				t.Fatalf("heartbeat %d was rejected: %+v", i, resp)
			}
		}
	})
}

// TestAsyncHardState_AForwardedReadCarriesOnlyADurableTerm closes the one
// message built off the event loop.
//
// A follower forwards a read to its leader from the caller's goroutine, so
// there is no write for the answer to be deferred against and the ordinary
// gate cannot apply. The message still carries a term, and the receiver steps
// down when it sees one above its own. A term this node might forget in a
// crash would make a healthy leader abandon its own for one that never
// existed, so what goes out is the term on disk rather than the term in use.
func TestAsyncHardState_AForwardedReadCarriesOnlyADurableTerm(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		seen := make(chan raft.Term, 8)
		store := newGateStore()

		cfg := raft.DefaultConfig()
		cfg.ID = "n1"
		cfg.Peers = []raft.PeerConfig{{ID: "n2", Voter: true}, {ID: "n3", Voter: true}}
		cfg.Storage = store
		cfg.StateMachine = idleSM{}
		cfg.Transport = &readIndexSpyTransport{seen: seen}
		cfg.TickInterval = 0

		node, err := raft.New(&cfg)
		if err != nil {
			t.Fatalf("raft.New: %v", err)
		}
		node.Start()
		t.Cleanup(node.Stop)

		// Adopt term 2 from a leader and let the write land, so the node has a
		// durable term and a known leader to forward to.
		if _, err := node.Handler().HandleAppendEntries(context.Background(), &raft.AppendEntriesRequest{
			Term:     2,
			LeaderID: "n2",
		}); err != nil {
			t.Fatalf("first heartbeat: %v", err)
		}

		// Now move to term 7 with the hard-state write held open.
		release := store.holdHardState(t)
		go func() {
			_, _ = node.Handler().HandleAppendEntries(context.Background(), &raft.AppendEntriesRequest{
				Term:     7,
				LeaderID: "n2",
			})
		}()
		store.awaitHeld(t)

		// The write reaching the gate does not mean the term is visible yet.
		// saveTerm queues the write before it publishes the term, so the writer
		// goroutine can already be blocked at the gate while the event loop has
		// not yet stored the new term in the mirror Term() reads. That ordering is
		// the conservative one -- the write is on its way before anything
		// announces the term -- so waiting is what the test has to do about it.
		// Asserting the instant awaitHeld returns fails about one run in three
		// hundred on a loaded machine.
		if !awaitTerm(node, 7, 5*time.Second) {
			t.Fatalf("Term() = %d, want 7: the term takes effect once the event loop "+
				"has processed the request", node.Term())
		}

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, _ = node.ReadIndex(ctx)
		cancel()

		select {
		case got := <-seen:
			if got != 2 {
				t.Errorf("the forwarded read carried term %d, which is not on disk; want the durable term 2", got)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("no read was forwarded to the leader")
		}

		release()
	})
}

// readIndexSpyTransport reports the term on each forwarded read.
type readIndexSpyTransport struct {
	echoTransport
	seen chan raft.Term
}

func (t *readIndexSpyTransport) ReadIndex(_ context.Context, _ raft.NodeID, req *raft.ReadIndexRequest) (*raft.ReadIndexResponse, error) {
	select {
	case t.seen <- req.Term:
	default:
	}
	return &raft.ReadIndexResponse{Index: 0}, nil
}

// awaitTerm waits for the node to report term, which happens a moment after
// the hard-state write carrying it is queued. See the note at the call site.
func awaitTerm(node *raft.Node, term raft.Term, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for node.Term() != term {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(time.Millisecond)
	}
	return true
}
