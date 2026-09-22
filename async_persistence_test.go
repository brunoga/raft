package raft_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brunoga/raft/v2"
	"github.com/brunoga/raft/v2/storage/memstore"
	"github.com/brunoga/raft/v2/transport/memtransport"
)

// These tests are about one change of contract: a storage write no longer
// happens on the event loop, so an entry is in the log before it is on disk.
//
// Everything that follows from that divides into two halves, and every test
// here belongs to one of them.
//
// The first half is what the change is for. The loop must stay responsive
// while a write is outstanding, and the leader must be able to overlap its own
// write with the round trip to its followers rather than doing one after the
// other.
//
// The second half is what the change must not cost. Raft's safety argument
// assumes that a node's log survives its own crash, so every statement whose
// meaning is "this is on disk" has to wait for the disk: the acknowledgement a
// follower returns, the leader counting its own log towards a commit quorum,
// and the commit index handed to the apply loop, which reads entries back from
// storage and would otherwise read entries that are not there.

// gateStore wraps a store and can hold its log writes open.
//
// Holding the write rather than failing it is the point: a failing disk is
// already covered elsewhere, and the interesting state is the one in between,
// where the node has accepted entries, cannot yet vouch for them, and has to
// keep working regardless.
type gateStore struct {
	raft.Storage
	// commits is the inner store's raft.CommitRecorder. Embedding a Storage
	// hides the optional interfaces the concrete store implements, and a
	// harness that quietly dropped one would test a node running without it.
	commits raft.CommitRecorder

	mu      sync.Mutex
	gate    chan struct{} // non-nil while log writes are held
	hsGate  chan struct{} // non-nil while hard-state writes are held
	ops     []string      // ordered log of the mutating calls that got through
	entered chan struct{} // signalled each time a held call starts waiting
}

func newGateStore() *gateStore {
	inner := memstore.New()
	return &gateStore{Storage: inner, commits: inner, entered: make(chan struct{}, 64)}
}

func (s *gateStore) SaveCommitIndex(ctx context.Context, index raft.Index) error {
	return s.commits.SaveCommitIndex(ctx, index)
}

func (s *gateStore) LoadCommitIndex(ctx context.Context) (raft.Index, error) {
	return s.commits.LoadCommitIndex(ctx)
}

// hold makes every subsequent log write block until the returned function is
// called. Calling it twice is harmless.
//
// The release is also registered as test cleanup, because a node cannot be
// stopped while a write it accepted is still held: stopping lets the queue
// drain, and a held write never drains. Without this, a test that failed an
// assertion early would hang in its own teardown instead of reporting.
func (s *gateStore) hold(t *testing.T) (release func()) {
	t.Helper()

	gate := make(chan struct{})
	s.mu.Lock()
	s.gate = gate
	s.mu.Unlock()

	var once sync.Once
	release = func() {
		once.Do(func() {
			s.mu.Lock()
			if s.gate == gate {
				s.gate = nil
			}
			s.mu.Unlock()
			close(gate)
		})
	}
	t.Cleanup(release)
	return release
}

// holdHardState does for SaveHardState what hold does for the log.
func (s *gateStore) holdHardState(t *testing.T) (release func()) {
	t.Helper()

	gate := make(chan struct{})
	s.mu.Lock()
	s.hsGate = gate
	s.mu.Unlock()

	var once sync.Once
	release = func() {
		once.Do(func() {
			s.mu.Lock()
			if s.hsGate == gate {
				s.hsGate = nil
			}
			s.mu.Unlock()
			close(gate)
		})
	}
	t.Cleanup(release)
	return release
}

func (s *gateStore) SaveHardState(ctx context.Context, hs raft.HardState) error {
	s.mu.Lock()
	gate := s.hsGate
	s.mu.Unlock()
	if gate != nil {
		select {
		case s.entered <- struct{}{}:
		default:
		}
		<-gate
	}
	if err := s.Storage.SaveHardState(ctx, hs); err != nil {
		return err
	}
	s.record(fmt.Sprintf("hardstate(term=%d,vote=%s)", hs.CurrentTerm, hs.VotedFor))
	return nil
}

// wait blocks the calling storage operation for as long as log writes are held.
func (s *gateStore) wait() {
	s.mu.Lock()
	gate := s.gate
	s.mu.Unlock()
	if gate == nil {
		return
	}
	select {
	case s.entered <- struct{}{}:
	default:
	}
	<-gate
}

// awaitHeld waits until a storage write has actually reached the gate, so that
// a test never races ahead of the node it is observing.
func (s *gateStore) awaitHeld(t *testing.T) {
	t.Helper()
	select {
	case <-s.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("no storage write arrived at the gate")
	}
}

func (s *gateStore) record(op string) {
	s.mu.Lock()
	s.ops = append(s.ops, op)
	s.mu.Unlock()
}

// operations returns the mutating calls the store has completed, in order.
func (s *gateStore) operations() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.ops...)
}

// logOperations returns only the calls that touched the log, for assertions
// about log ordering that should not have to restate every term change.
func (s *gateStore) logOperations() []string {
	var out []string
	for _, op := range s.operations() {
		if !strings.HasPrefix(op, "hardstate(") {
			out = append(out, op)
		}
	}
	return out
}

func (s *gateStore) AppendLogEntries(ctx context.Context, entries []raft.LogEntry) error {
	s.wait()
	if err := s.Storage.AppendLogEntries(ctx, entries); err != nil {
		return err
	}
	first, last := entries[0].Index, entries[len(entries)-1].Index
	s.record(fmt.Sprintf("append(%d..%d)", first, last))
	return nil
}

func (s *gateStore) TruncateSuffix(ctx context.Context, from raft.Index) error {
	s.wait()
	if err := s.Storage.TruncateSuffix(ctx, from); err != nil {
		return err
	}
	s.record(fmt.Sprintf("truncate_suffix(%d)", from))
	return nil
}

func (s *gateStore) TruncatePrefix(ctx context.Context, to raft.Index) error {
	if err := s.Storage.TruncatePrefix(ctx, to); err != nil {
		return err
	}
	s.record(fmt.Sprintf("truncate_prefix(%d)", to))
	return nil
}

// newGatedFollower starts a follower with two peers, so it never elects itself
// while the test is setting up.
func newGatedFollower(t *testing.T, tune func(*raft.Config)) (*raft.Node, *gateStore) {
	t.Helper()

	store := newGateStore()
	cfg := raft.DefaultConfig()
	cfg.ID = "n1"
	cfg.Peers = []raft.PeerConfig{{ID: "n2", Voter: true}, {ID: "n3", Voter: true}}
	cfg.Storage = store
	cfg.StateMachine = idleSM{}
	cfg.Transport = memtransport.NewNetwork().NewTransport("n1")
	cfg.TickInterval = 0
	if tune != nil {
		tune(&cfg)
	}

	node, err := raft.New(&cfg)
	if err != nil {
		t.Fatalf("raft.New: %v", err)
	}
	node.Start()
	t.Cleanup(node.Stop)
	return node, store
}

// appendFrom builds an AppendEntries carrying entries at [from, from+count).
func appendFrom(term raft.Term, from raft.Index, count int, commit raft.Index) *raft.AppendEntriesRequest {
	entries := make([]raft.LogEntry, 0, count)
	for i := range count {
		entries = append(entries, raft.LogEntry{
			Index:   from + raft.Index(i),
			Term:    term,
			Command: []byte("x"),
		})
	}
	return &raft.AppendEntriesRequest{
		Term:         term,
		LeaderID:     "n2",
		PrevLogIndex: from - 1,
		PrevLogTerm:  term,
		Entries:      entries,
		LeaderCommit: commit,
	}
}

// TestAsyncPersistence_TheEventLoopKeepsRunningWhileAWriteIsOutstanding is the
// reason the change exists.
//
// A node that waits for fsync on its event loop stops counting election ticks,
// stops answering heartbeats and stops reading its inbound queue, so a disk
// having a slow moment is indistinguishable from the node being down: its
// followers time out and call an election against a leader that is alive and
// healthy apart from one pending write. Here the write is held open for as
// long as the test likes, and the node has to keep answering anyway.
func TestAsyncPersistence_TheEventLoopKeepsRunningWhileAWriteIsOutstanding(t *testing.T) {
	node, store := newGatedFollower(t, nil)

	release := store.hold(t)
	defer release()

	// Give the node entries to write. The acknowledgement will not come back
	// until the write does, so this cannot be waited on here.
	go func() {
		_, _ = node.Handler().HandleAppendEntries(context.Background(), appendFrom(1, 1, 3, 0))
	}()
	store.awaitHeld(t)

	// With the write still outstanding, the loop must answer as usual. A vote
	// request is the sharpest test available: it is what a follower sends when
	// it has decided the leader is gone, and answering it is what stops a
	// healthy cluster from tearing itself apart over a slow disk.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	resp, err := node.Handler().HandleRequestVote(ctx, &raft.RequestVoteRequest{
		Term:         1,
		CandidateID:  "n3",
		LastLogIndex: 0,
		LastLogTerm:  0,
		PreVote:      true,
	})
	if err != nil {
		t.Fatalf("the node did not answer a vote request while a log write was outstanding: %v", err)
	}
	if resp == nil {
		t.Fatal("nil vote response while a log write was outstanding")
	}

	// And it is still tracking time, which is what elections depend on.
	node.Tick()
	if node.FatalError() != nil {
		t.Fatalf("node failed while a write was merely slow: %v", node.FatalError())
	}
}

// TestAsyncPersistence_AFollowerAcknowledgesOnlyAfterTheEntriesAreOnDisk is the
// safety property the whole design turns on.
//
// The acknowledgement is what lets a leader count this node towards the quorum
// that commits an entry, and a committed entry is applied and never revisited.
// So it has to mean "these entries will survive my crash" and nothing weaker.
// A node that crashes before the write lands comes back without the entries
// and without having acknowledged them, which is the case Raft already handles
// by retrying; a node that acknowledged first and crashed second would have
// told the cluster something that is no longer true.
func TestAsyncPersistence_AFollowerAcknowledgesOnlyAfterTheEntriesAreOnDisk(t *testing.T) {
	node, store := newGatedFollower(t, nil)

	release := store.hold(t)

	type ack struct {
		resp *raft.AppendEntriesResponse
		err  error
	}
	acked := make(chan ack, 1)
	go func() {
		resp, err := node.Handler().HandleAppendEntries(context.Background(), appendFrom(1, 1, 3, 0))
		acked <- ack{resp, err}
	}()
	store.awaitHeld(t)

	select {
	case got := <-acked:
		t.Fatalf("follower acknowledged entries before they were written: %+v (err %v)", got.resp, got.err)
	case <-time.After(150 * time.Millisecond):
	}

	if ops := store.logOperations(); len(ops) != 0 {
		t.Fatalf("storage recorded %v before the write was released", ops)
	}

	release()

	select {
	case got := <-acked:
		if got.err != nil {
			t.Fatalf("acknowledgement returned an error: %v", got.err)
		}
		if !got.resp.Success {
			t.Fatalf("acknowledgement was not successful: %+v", got.resp)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no acknowledgement after the write was released")
	}

	if ops := store.logOperations(); len(ops) == 0 || !strings.HasPrefix(ops[0], "append(1..3)") {
		t.Fatalf("storage operations = %v, want the append to have happened before the acknowledgement", ops)
	}
}

// TestAsyncPersistence_NothingIsAppliedBeforeItIsOnDisk covers the quietest way
// this could have gone wrong.
//
// A follower is told its commit index by the leader and may hold the entries
// it covers in memory only. The apply loop runs on its own goroutine and reads
// entries back from storage, so handing it that commit index unchanged would
// have it read indices that are not there -- and the apply path advances past
// an entry it cannot read, because the alternative is to stall for ever. The
// result would be a state machine that skipped a committed entry, which no
// later replication repairs.
func TestAsyncPersistence_NothingIsAppliedBeforeItIsOnDisk(t *testing.T) {
	applied := make(chan raft.Index, 16)
	node, store := newGatedFollower(t, func(cfg *raft.Config) {
		cfg.StateMachine = &indexReportingSM{applied: applied}
	})

	release := store.hold(t)

	// LeaderCommit covers every entry in the request, so the follower's commit
	// index moves to 3 immediately while the entries are still only in memory.
	go func() {
		_, _ = node.Handler().HandleAppendEntries(context.Background(), appendFrom(1, 1, 3, 3))
	}()
	store.awaitHeld(t)

	select {
	case idx := <-applied:
		t.Fatalf("entry %d was applied before it was on disk", idx)
	case <-time.After(200 * time.Millisecond):
	}

	release()

	deadline := time.After(5 * time.Second)
	for want := raft.Index(1); want <= 3; want++ {
		select {
		case got := <-applied:
			if got != want {
				t.Fatalf("applied index %d, want %d", got, want)
			}
		case <-deadline:
			t.Fatalf("entry %d was never applied after the write was released", want)
		}
	}
}

// TestAsyncPersistence_ALeaderDoesNotCommitOnItsOwnUnwrittenLog holds the
// leader to the same standard as a follower.
//
// A leader's log is one of the replicas the commit quorum is drawn from. It is
// no more entitled than a follower to vouch for an entry it has not written:
// committing on a quorum that includes an entry living only in this leader's
// memory would apply it across the cluster, and then lose it here if the
// machine died before the write landed. In a single-node cluster the leader is
// the entire quorum, which makes the rule visible on its own.
func TestAsyncPersistence_ALeaderDoesNotCommitOnItsOwnUnwrittenLog(t *testing.T) {
	store := newGateStore()
	cfg := raft.DefaultConfig()
	cfg.ID = "solo"
	cfg.Storage = store
	cfg.StateMachine = idleSM{}
	cfg.Transport = memtransport.NewNetwork().NewTransport("solo")
	cfg.TickInterval = 0

	node, err := raft.New(&cfg)
	if err != nil {
		t.Fatalf("raft.New: %v", err)
	}
	node.Start()
	// Registered before the gate below, so cleanup releases the gate first:
	// a stop cannot complete while a write it accepted is still held.
	t.Cleanup(node.Stop)

	// Elect it, with writes flowing: the leadership no-op has to be written
	// before anything can commit at all.
	stop := tickWhile(node)
	deadline := time.Now().Add(5 * time.Second)
	for node.State() != raft.Leader {
		if time.Now().After(deadline) {
			stop()
			t.Fatal("the node never became leader")
		}
		time.Sleep(time.Millisecond)
	}
	stop()

	before := node.CommitIndex()
	release := store.hold(t)

	proposed := make(chan error, 1)
	go func() {
		_, perr := node.Propose(context.Background(), []byte("v"))
		proposed <- perr
	}()
	store.awaitHeld(t)

	// The entry is in the leader's log and it is the only replica there is,
	// yet it must not be committed while the write is outstanding.
	time.Sleep(200 * time.Millisecond)
	if got := node.CommitIndex(); got != before {
		t.Fatalf("commit index moved from %d to %d while the leader's own write was outstanding", before, got)
	}

	release()

	select {
	case perr := <-proposed:
		if perr != nil {
			t.Fatalf("Propose: %v", perr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the proposal never completed after the write was released")
	}
	if got := node.CommitIndex(); got <= before {
		t.Fatalf("commit index = %d after the write landed, want more than %d", got, before)
	}
}

// TestAsyncPersistence_AConflictingSuffixIsRemovedBeforeItIsReplaced is the
// ordering property.
//
// A follower whose log diverges from a new leader's truncates the conflicting
// tail and writes the leader's entries in its place. Those are two separate
// storage operations, and their order is the whole point: run the other way
// round, the truncation would delete the entries that had just replaced the
// old ones, and the follower would come back from a crash missing a run of the
// leader's log while believing it had acknowledged it. Queueing them is only
// safe because one goroutine drains that queue in order.
func TestAsyncPersistence_AConflictingSuffixIsRemovedBeforeItIsReplaced(t *testing.T) {
	node, store := newGatedFollower(t, nil)

	// Three entries from the leader of term 1.
	if _, err := node.Handler().HandleAppendEntries(context.Background(), appendFrom(1, 1, 3, 0)); err != nil {
		t.Fatalf("first append: %v", err)
	}

	// A leader of term 2 overwrites from index 2. Its PrevLogIndex of 1 still
	// matches, so the follower truncates and appends in one exchange.
	conflicting := appendFrom(2, 2, 2, 0)
	conflicting.PrevLogTerm = 1
	resp, err := node.Handler().HandleAppendEntries(context.Background(), conflicting)
	if err != nil {
		t.Fatalf("conflicting append: %v", err)
	}
	if !resp.Success {
		t.Fatalf("conflicting append was rejected: %+v", resp)
	}

	want := []string{"append(1..3)", "truncate_suffix(2)", "append(2..3)"}
	got := store.logOperations()
	if len(got) != len(want) {
		t.Fatalf("storage operations = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("storage operations = %v, want %v", got, want)
		}
	}
}

// TestAsyncPersistence_ProposalsAreRefusedWhenTheBacklogIsFull covers what
// replaces waiting for the disk.
//
// Blocking on the write used to be the backpressure: a leader could not accept
// a proposal faster than it could write one. Without a limit, a leader whose
// storage has stalled would accept proposals until the process ran out of
// memory, turning a slow node into a dead one and taking the cluster's leader
// with it. Refusing the proposal keeps the node alive and hands the decision
// to the caller.
func TestAsyncPersistence_ProposalsAreRefusedWhenTheBacklogIsFull(t *testing.T) {
	store := newGateStore()
	cfg := raft.DefaultConfig()
	cfg.ID = "solo"
	cfg.Storage = store
	cfg.StateMachine = idleSM{}
	cfg.Transport = memtransport.NewNetwork().NewTransport("solo")
	cfg.TickInterval = 0
	cfg.MaxUnstableLogBytes = 4 << 10 // 4 KiB

	node, err := raft.New(&cfg)
	if err != nil {
		t.Fatalf("raft.New: %v", err)
	}
	node.Start()
	// Registered before the gate below, so cleanup releases the gate first:
	// a stop cannot complete while a write it accepted is still held.
	t.Cleanup(node.Stop)

	stop := tickWhile(node)
	deadline := time.Now().Add(5 * time.Second)
	for node.State() != raft.Leader {
		if time.Now().After(deadline) {
			stop()
			t.Fatal("the node never became leader")
		}
		time.Sleep(time.Millisecond)
	}
	stop()

	release := store.hold(t)
	defer release()

	const (
		proposals = 32
		cmdSize   = 1 << 10 // 1 KiB, so a handful of these fills the budget
	)
	cmd := make([]byte, cmdSize)
	results := make(chan error, proposals)
	for range proposals {
		go func() {
			_, perr := node.Propose(context.Background(), cmd)
			results <- perr
		}()
	}

	// The proposals that fit are appended and then wait for a commit that
	// cannot happen while the write is held, so they do not come back at all.
	// The ones that do not fit are refused straight away, which is the whole
	// point: the node answers rather than absorbing.
	var refused, other int
	giveUp := time.After(10 * time.Second)
	for refused == 0 {
		select {
		case perr := <-results:
			switch {
			case errors.Is(perr, raft.ErrWriteBacklogFull):
				refused++
			default:
				other++
			}
		case <-giveUp:
			t.Fatalf("no proposal was refused with ErrWriteBacklogFull: %d KiB offered against a %d KiB budget, %d other results",
				proposals*cmdSize>>10, cfg.MaxUnstableLogBytes>>10, other)
		}
	}
	if other > 0 {
		t.Errorf("%d proposals came back for a reason other than the backlog being full", other)
	}
}

// TestAsyncPersistence_AGracefulStopWritesWhatItAccepted pins that shutting a
// node down finishes the writes it has taken on rather than dropping them.
//
// Dropping them would be correct -- nothing was acknowledged on their behalf,
// so it is the crash this design is already safe under -- but it would mean
// every orderly restart threw away the tail of the log and fetched it again
// from the leader.
func TestAsyncPersistence_AGracefulStopWritesWhatItAccepted(t *testing.T) {
	store := newGateStore()
	cfg := raft.DefaultConfig()
	cfg.ID = "n1"
	cfg.Peers = []raft.PeerConfig{{ID: "n2", Voter: true}, {ID: "n3", Voter: true}}
	cfg.Storage = store
	cfg.StateMachine = idleSM{}
	cfg.Transport = memtransport.NewNetwork().NewTransport("n1")
	cfg.TickInterval = 0

	node, err := raft.New(&cfg)
	if err != nil {
		t.Fatalf("raft.New: %v", err)
	}
	node.Start()
	t.Cleanup(node.Stop)

	release := store.hold(t)
	go func() {
		_, _ = node.Handler().HandleAppendEntries(context.Background(), appendFrom(1, 1, 3, 0))
	}()
	store.awaitHeld(t)

	// Release and stop: the entries were accepted, so stopping must not lose
	// them.
	release()
	node.Stop()

	last, err := store.LastIndex()
	if err != nil {
		t.Fatalf("LastIndex: %v", err)
	}
	if last != 3 {
		t.Errorf("after a graceful stop the store holds up to index %d, want 3", last)
	}
}

// indexReportingSM reports the index of every entry it applies, as it applies
// it, so a test can assert that nothing was applied during a window.
type indexReportingSM struct {
	applied chan raft.Index
}

func (s *indexReportingSM) Apply(_ context.Context, e raft.LogEntry) ([]byte, error) {
	select {
	case s.applied <- e.Index:
	default:
	}
	return nil, nil
}

func (s *indexReportingSM) Snapshot(_ context.Context, w io.Writer) error {
	_, err := w.Write([]byte("s"))
	return err
}

func (s *indexReportingSM) Restore(_ context.Context, _ raft.SnapshotMeta, r io.Reader) error {
	_, err := io.Copy(io.Discard, r)
	return err
}

// echoTransport answers every AppendEntries as a healthy follower would, and
// reports which entry indices it was asked to carry.
type echoTransport struct {
	sent chan raft.Index // the last index of each AppendEntries that carried entries
}

func (t *echoTransport) AppendEntries(_ context.Context, _ raft.NodeID, req *raft.AppendEntriesRequest) (*raft.AppendEntriesResponse, error) {
	if len(req.Entries) > 0 {
		select {
		case t.sent <- req.Entries[len(req.Entries)-1].Index:
		default:
		}
	}
	return &raft.AppendEntriesResponse{Term: req.Term, Success: true}, nil
}

func (t *echoTransport) RequestVote(_ context.Context, _ raft.NodeID, req *raft.RequestVoteRequest) (*raft.RequestVoteResponse, error) {
	// A pre-vote is asked in the term the candidate *would* move to, and a
	// follower answering one has not moved: it replies with its own term,
	// which is the candidate's. Replying with the requested term instead would
	// look like a higher term and make the candidate stand down.
	term := req.Term
	if req.PreVote {
		term--
	}
	return &raft.RequestVoteResponse{Term: term, VoteGranted: true}, nil
}

func (t *echoTransport) InstallSnapshot(_ context.Context, _ raft.NodeID, req *raft.InstallSnapshotRequest) (*raft.InstallSnapshotResponse, error) {
	return &raft.InstallSnapshotResponse{Term: req.Term}, nil
}

func (t *echoTransport) TimeoutNow(_ context.Context, _ raft.NodeID, req *raft.TimeoutNowRequest) (*raft.TimeoutNowResponse, error) {
	return &raft.TimeoutNowResponse{Term: req.Term}, nil
}

func (t *echoTransport) ReadIndex(_ context.Context, _ raft.NodeID, _ *raft.ReadIndexRequest) (*raft.ReadIndexResponse, error) {
	return &raft.ReadIndexResponse{}, nil
}

func (t *echoTransport) Register(_ raft.NodeID, _ raft.Handler) {}
func (t *echoTransport) Unregister(_ raft.NodeID)               {}
func (t *echoTransport) Close() error                           { return nil }

// TestAsyncPersistence_ALeaderReplicatesWhileItsOwnWriteIsStillOutstanding is
// the other half of what the change buys, and the reason it is worth the
// machinery rather than just moving the write to another goroutine and waiting
// for it there.
//
// A leader used to write its entries and only then send them, so the time to
// commit was the disk plus the network. Sending first, and counting its own
// log towards the quorum only once the write lands, makes it the larger of the
// two instead. Nothing is committed any earlier than it was safe to commit:
// the leader still needs its own entry on disk, it just no longer spends the
// round trip waiting for it.
func TestAsyncPersistence_ALeaderReplicatesWhileItsOwnWriteIsStillOutstanding(t *testing.T) {
	transport := &echoTransport{sent: make(chan raft.Index, 32)}
	store := newGateStore()

	cfg := raft.DefaultConfig()
	cfg.ID = "leader"
	cfg.Peers = []raft.PeerConfig{{ID: "follower", Voter: true}}
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

	stop := tickWhile(node)
	deadline := time.Now().Add(5 * time.Second)
	for node.State() != raft.Leader {
		if time.Now().After(deadline) {
			stop()
			t.Fatal("the node never became leader")
		}
		time.Sleep(time.Millisecond)
	}
	stop()
	// Drain whatever replication the election itself produced.
	for len(transport.sent) > 0 {
		<-transport.sent
	}

	release := store.hold(t)
	before := node.CommitIndex()

	go func() {
		_, _ = node.Propose(context.Background(), []byte("v"))
	}()
	store.awaitHeld(t)

	// The entry is on its way to the follower even though this leader has not
	// written it.
	select {
	case idx := <-transport.sent:
		if idx <= before {
			t.Fatalf("the leader replicated up to index %d, want something past %d", idx, before)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the leader waited for its own write before replicating")
	}

	// And it still has not committed, because its own copy is not on disk.
	if got := node.CommitIndex(); got != before {
		t.Fatalf("commit index moved to %d before the leader's own write landed", got)
	}

	release()
	deadline = time.Now().Add(5 * time.Second)
	for node.CommitIndex() == before {
		if time.Now().After(deadline) {
			t.Fatal("the entry never committed after the write landed")
		}
		time.Sleep(time.Millisecond)
	}
}

// snapGateStore holds SaveSnapshot open, so a test can watch what a node says
// while a snapshot it has been sent is still being written.
type snapGateStore struct {
	raft.Storage

	mu      sync.Mutex
	gate    chan struct{}
	entered chan struct{}
}

func newSnapGateStore() *snapGateStore {
	return &snapGateStore{Storage: memstore.New(), entered: make(chan struct{}, 8)}
}

func (s *snapGateStore) hold(t *testing.T) (release func()) {
	t.Helper()

	gate := make(chan struct{})
	s.mu.Lock()
	s.gate = gate
	s.mu.Unlock()

	var once sync.Once
	release = func() {
		once.Do(func() {
			s.mu.Lock()
			if s.gate == gate {
				s.gate = nil
			}
			s.mu.Unlock()
			close(gate)
		})
	}
	t.Cleanup(release)
	return release
}

func (s *snapGateStore) SaveSnapshot(ctx context.Context, meta raft.SnapshotMeta, r io.Reader) error {
	s.mu.Lock()
	gate := s.gate
	s.mu.Unlock()
	if gate != nil {
		select {
		case s.entered <- struct{}{}:
		default:
		}
		<-gate
	}
	return s.Storage.SaveSnapshot(ctx, meta, r)
}

func (s *snapGateStore) awaitHeld(t *testing.T) {
	t.Helper()
	select {
	case <-s.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("no snapshot write arrived at the gate")
	}
}

// TestAsyncPersistence_ASnapshotIsAcknowledgedOnlyAfterItIsWritten holds the
// snapshot path to the same standard as the log.
//
// The leader turns the response to a snapshot's final chunk into this node's
// match index, which is its statement of what this node durably holds. So the
// response has to wait for the write. Answering when the bytes merely arrived
// makes that statement false for as long as the write takes, and permanently
// false if this node dies during it, because a match index is never lowered.
//
// Nothing unsafe follows from the stale figure, since a snapshot never covers
// anything past the leader's commit index and so cannot advance one. But the
// same number decides whether a learner has caught up enough to be made a
// voter, and a cluster that promotes one which has not has given itself a
// voter that cannot vote.
func TestAsyncPersistence_ASnapshotIsAcknowledgedOnlyAfterItIsWritten(t *testing.T) {
	payload := snapshotPayload(t)
	store := newSnapGateStore()

	cfg := raft.DefaultConfig()
	cfg.ID = "f1"
	cfg.Peers = []raft.PeerConfig{{ID: "l1", Voter: true}, {ID: "f2", Voter: true}}
	cfg.Storage = store
	cfg.StateMachine = &echoSM{}
	cfg.Transport = memtransport.NewNetwork().NewTransport("f1")
	cfg.TickInterval = 0

	node, err := raft.New(&cfg)
	if err != nil {
		t.Fatalf("raft.New: %v", err)
	}
	node.Start()
	t.Cleanup(node.Stop)

	release := store.hold(t)

	type ack struct {
		resp *raft.InstallSnapshotResponse
		err  error
	}
	acked := make(chan ack, 1)
	go func() {
		resp, ackErr := node.Handler().HandleInstallSnapshot(context.Background(), &raft.InstallSnapshotRequest{
			Term:              3,
			LeaderID:          "l1",
			LastIncludedIndex: 3,
			LastIncludedTerm:  3,
			Offset:            0,
			Data:              payload,
			Done:              true,
		})
		acked <- ack{resp, ackErr}
	}()
	store.awaitHeld(t)

	select {
	case got := <-acked:
		t.Fatalf("the snapshot was acknowledged before it was written: %+v (err %v)", got.resp, got.err)
	case <-time.After(200 * time.Millisecond):
	}

	release()

	select {
	case got := <-acked:
		if got.err != nil {
			t.Fatalf("snapshot acknowledgement returned an error: %v", got.err)
		}
		if got.resp == nil {
			t.Fatal("nil snapshot response")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no snapshot acknowledgement after the write was released")
	}

	deadline := time.Now().Add(5 * time.Second)
	for node.SnapshotIndex() < 3 {
		if time.Now().After(deadline) {
			t.Fatal("the snapshot was never installed after it was written")
		}
		time.Sleep(time.Millisecond)
	}
}

// TestAsyncPersistence_AFollowerRefusesEntriesItCannotBuffer is the follower's
// half of the backpressure.
//
// A follower holds entries in memory until storage has written them, just as a
// leader does. Its backlog is normally kept small by what the leader will send
// before being acknowledged, but that is the leader's restraint and not this
// node's: a node should not be able to be driven out of memory by a peer.
//
// It refuses outright rather than rejecting the append, because a rejection is
// a statement about this node's log and would send the leader hunting
// backwards through it for a disagreement that does not exist.
func TestAsyncPersistence_AFollowerRefusesEntriesItCannotBuffer(t *testing.T) {
	node, store := newGatedFollower(t, func(cfg *raft.Config) {
		cfg.MaxUnstableLogBytes = 1 << 10 // 1 KiB, so one batch overshoots it
	})

	release := store.hold(t)
	defer release()

	cmd := make([]byte, 1<<10) // 1 KiB per entry
	batch := func(from raft.Index) *raft.AppendEntriesRequest {
		entries := make([]raft.LogEntry, 0, 4)
		for i := range 4 {
			entries = append(entries, raft.LogEntry{Index: from + raft.Index(i), Term: 1, Command: cmd})
		}
		req := &raft.AppendEntriesRequest{
			Term:         1,
			LeaderID:     "n2",
			PrevLogIndex: from - 1,
			PrevLogTerm:  1,
			Entries:      entries,
		}
		if from == 1 {
			req.PrevLogTerm = 0
		}
		return req
	}

	// The first batch is taken: a node with nothing outstanding always accepts,
	// whatever the size, so that an entry larger than the budget is never
	// refused for ever. Its acknowledgement waits for the write, which is held,
	// so this call does not return.
	go func() {
		_, _ = node.Handler().HandleAppendEntries(context.Background(), batch(1))
	}()
	store.awaitHeld(t)

	// The second finds four kilobytes already waiting against a one-kilobyte
	// budget and is refused outright, rather than queued or rejected.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	resp, err := node.Handler().HandleAppendEntries(ctx, batch(5))
	if !errors.Is(err, raft.ErrWriteBacklogFull) {
		t.Fatalf("second batch returned (%+v, %v); want raft.ErrWriteBacklogFull", resp, err)
	}
	if node.FatalError() != nil {
		t.Errorf("refusing entries stopped the node: %v", node.FatalError())
	}

	// Once the backlog clears the follower takes entries again.
	release()
	deadline := time.Now().Add(5 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		resp, err := node.Handler().HandleAppendEntries(ctx, batch(5))
		cancel()
		if err == nil && resp != nil && resp.Success {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the follower never took entries again after its backlog drained: (%+v, %v)", resp, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestAsyncPersistence_ARetransmittedRangeIsNotAcknowledgedEarly closes the
// gap between "this request queued a write" and "these entries are on disk".
//
// A leader that has not been acknowledged re-sends the same entries: its own
// RPC timed out, or the per-request entry cap makes the next batch identical
// to the last. A follower still holding those entries unwritten finds every
// one of them already in its log and has nothing to append, so there is no
// write of its own to wait for. Answering at that point reports entries as
// durable when nothing has reached the disk, and the leader turns that into a
// match index and a commit.
//
// The entries are already in the log, so the answer is not wrong about what
// this node holds. It is wrong about what this node would still hold after a
// crash, and that is the only thing the answer is for.
func TestAsyncPersistence_ARetransmittedRangeIsNotAcknowledgedEarly(t *testing.T) {
	node, store := newGatedFollower(t, nil)

	release := store.hold(t)

	first := make(chan *raft.AppendEntriesResponse, 1)
	go func() {
		resp, _ := node.Handler().HandleAppendEntries(context.Background(), appendFrom(1, 1, 3, 0))
		first <- resp
	}()
	store.awaitHeld(t)

	// The same range again, while the first delivery's write is still held.
	second := make(chan *raft.AppendEntriesResponse, 1)
	go func() {
		resp, _ := node.Handler().HandleAppendEntries(context.Background(), appendFrom(1, 1, 3, 0))
		second <- resp
	}()

	select {
	case got := <-second:
		t.Fatalf("the retransmission was acknowledged with nothing written: %+v; storage has %v",
			got, store.logOperations())
	case <-time.After(200 * time.Millisecond):
	}

	release()

	for _, ch := range []chan *raft.AppendEntriesResponse{first, second} {
		select {
		case got := <-ch:
			if got == nil || !got.Success {
				t.Fatalf("acknowledgement after the write landed = %+v, want success", got)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("no acknowledgement after the write was released")
		}
	}

	if ops := store.logOperations(); len(ops) != 1 || ops[0] != "append(1..3)" {
		t.Errorf("storage operations = %v, want a single append of 1..3", ops)
	}
}

// TestAsyncPersistence_AHeartbeatIsNotGatedOnTheFollowersDisk is the limit on
// how far the durability gate reaches.
//
// A heartbeat carries no entries and claims nothing about any, and a leader
// never turns its acknowledgement into a match index. Making it wait for a
// write would put a follower's disk inside every heartbeat and every read
// barrier -- and after an election or a snapshot install the index a heartbeat
// names can sit in the part of the log still being written, so this is not a
// rare shape. A follower that cannot answer a heartbeat is one its leader
// stands down for, which is the failure this whole design exists to prevent.
func TestAsyncPersistence_AHeartbeatIsNotGatedOnTheFollowersDisk(t *testing.T) {
	node, store := newGatedFollower(t, nil)

	release := store.hold(t)
	defer release()

	// Entries 1..3 go into the log and their write is held open.
	go func() {
		_, _ = node.Handler().HandleAppendEntries(context.Background(), appendFrom(1, 1, 3, 0))
	}()
	store.awaitHeld(t)

	// A heartbeat naming the last of those entries must still be answered,
	// even though that entry is not on disk.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	resp, err := node.Handler().HandleAppendEntries(ctx, &raft.AppendEntriesRequest{
		Term:         1,
		LeaderID:     "n2",
		PrevLogIndex: 3,
		PrevLogTerm:  1,
	})
	if err != nil {
		t.Fatalf("a heartbeat was not answered while a log write was outstanding: %v", err)
	}
	if resp == nil || !resp.Success {
		t.Fatalf("heartbeat response = %+v, want success", resp)
	}
}

// TestAsyncPersistence_ACommitIndexMayOutrunTheDisk pins the transient state
// that asynchronous writes make possible, and the reason it is harmless.
//
// A follower takes an entry that replaces one it already had, and its leader
// tells it in the same breath that the new entry is committed. The commit
// index moves at once, because the entry is in the log at once. The disk still
// holds the entry that was replaced, and will until the queued truncation and
// append reach it.
//
// So a reader looking only at the disk and the commit index sees a node that
// "considers committed" an entry it does not hold. Nothing acts on that view.
// The node applies from storage and only up to the point storage is known to
// hold the log's current entries, so the replaced entry is never applied; and
// the commit index is not persisted, so a crash here brings the node back with
// no commit index at all and the leader brings it up to date again.
func TestAsyncPersistence_ACommitIndexMayOutrunTheDisk(t *testing.T) {
	applied := make(chan raft.Index, 16)
	node, store := newGatedFollower(t, func(cfg *raft.Config) {
		cfg.StateMachine = &indexReportingSM{applied: applied}
	})

	// Entries 1 and 2 in term 1, written and applied.
	if _, err := node.Handler().HandleAppendEntries(context.Background(), appendFrom(1, 1, 2, 1)); err != nil {
		t.Fatalf("first append: %v", err)
	}
	select {
	case idx := <-applied:
		if idx != 1 {
			t.Fatalf("applied index %d, want 1", idx)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("entry 1 was never applied")
	}

	// A leader in term 2 replaces index 2 and says it is committed. Its write
	// is held, so the disk keeps the term-1 entry there.
	release := store.hold(t)

	replace := &raft.AppendEntriesRequest{
		Term:         2,
		LeaderID:     "n2",
		PrevLogIndex: 1,
		PrevLogTerm:  1,
		Entries:      []raft.LogEntry{{Index: 2, Term: 2, Command: []byte("replacement")}},
		LeaderCommit: 2,
	}
	go func() {
		_, _ = node.Handler().HandleAppendEntries(context.Background(), replace)
	}()
	store.awaitHeld(t)

	// The commit index has moved past what the disk holds. That is the state
	// the durable log alone cannot explain.
	deadline := time.Now().Add(5 * time.Second)
	for node.CommitIndex() < 2 {
		if time.Now().After(deadline) {
			t.Fatal("the commit index never reached 2")
		}
		time.Sleep(time.Millisecond)
	}
	got, err := store.GetLogEntry(context.Background(), 2)
	if err != nil {
		t.Fatalf("read index 2 from the store: %v", err)
	}
	if got.Term != 1 {
		t.Fatalf("the store holds term %d at index 2; the test needs the replacement to still be pending", got.Term)
	}

	// Nothing is applied there while that is true.
	select {
	case idx := <-applied:
		t.Fatalf("index %d was applied while the disk still held the entry it replaced", idx)
	case <-time.After(200 * time.Millisecond):
	}

	release()

	select {
	case idx := <-applied:
		if idx != 2 {
			t.Fatalf("applied index %d, want 2", idx)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("index 2 was never applied after the write landed")
	}
	final, err := store.GetLogEntry(context.Background(), 2)
	if err != nil {
		t.Fatalf("re-read index 2: %v", err)
	}
	if final.Term != 2 {
		t.Errorf("the store holds term %d at index 2 after the write landed, want 2", final.Term)
	}
}
