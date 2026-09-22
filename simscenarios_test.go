package raft_test

// Deliberately constructed scenarios for the parts of Raft that randomized
// testing is unlikely to reach on its own.
//
// Every one of these is a situation the Raft paper singles out as a place where
// a plausible-looking implementation is wrong. Waiting for chaos to stumble
// into them is not a plan: the window in which, say, an entry from an earlier
// term sits on a majority while its leader is still alive is a few milliseconds
// wide. So each test builds the state it needs — by pre-seeding logs, by
// choosing who is allowed to campaign, and by filtering individual messages on
// their content — and then asserts the one thing that must be true.

import (
	"bytes"
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/brunoga/raft/v2"
	"github.com/brunoga/raft/v2/internal/simnet"
	"github.com/brunoga/raft/v2/storage/memstore"
)

const scenarioTimeout = 5 * time.Second

// ---- Figure 8 --------------------------------------------------------------

// TestFigure8_OldTermEntryIsNotCommittedByReplicaCount protects the rule that
// makes Figure 8 of the Raft paper safe: a leader may only advance its commit
// index for an entry of its OWN term. Counting replicas is not enough.
//
// The scenario, built directly:
//
//	s1  1:t1  2:t2         ← about to become leader in term 4
//	s2  1:t1
//	s3  1:t1
//	s4  1:t1
//	s5  1:t1  2:t3         ← isolated, holds a different entry at index 2
//
// s1 wins term 4 and replicates its term-2 entry at index 2 to s2 and s3, so
// index 2 is durably on three of five nodes — a majority. It must still not be
// committed, because s5 could yet be elected and overwrite it. The only thing
// that makes index 2 safe is committing the term-4 entry that follows it, which
// this test withholds by dropping every AppendEntries that carries a term-4
// entry, and then allows.
//
// An implementation that committed on replica count alone would advance to
// index 2 during the first phase and would then be caught out by the second
// Figure 8 test below, which lets s5 back in.
func TestFigure8_OldTermEntryIsNotCommittedByReplicaCount(t *testing.T) {
	c, blockTerm4 := newFigure8Cluster(t)

	// s5 must not hear anything: in the paper it is crashed at this point, and
	// if it heard from s1 it would have its term-3 entry overwritten, which is
	// the very thing that makes index 2 unsafe.
	c.net.Isolate(c.ids[4])

	c.electLeader(0, scenarioTimeout, blockTerm4.rule)
	if got := c.node(0).Term(); got != 4 {
		t.Fatalf("s1 became leader in term %d, want 4; the scenario depends on the term numbering\n%s",
			got, c.diagnostics())
	}

	// Wait for the term-2 entry at index 2 to reach a majority of the five.
	deadline := time.Now().Add(scenarioTimeout)
	for time.Now().Before(deadline) && c.countStoresWith(2, 2) < 3 {
		time.Sleep(time.Millisecond)
	}
	if n := c.countStoresWith(2, 2); n < 3 {
		t.Fatalf("index 2 (term 2) reached only %d of 5 logs; the scenario needs a majority\n%s",
			n, c.diagnostics())
	}

	// The entry is on a majority and its leader is alive. It must not be
	// committed, now or after any amount of further heartbeating.
	c.tickFor(100 * time.Millisecond)
	if got := c.node(0).CommitIndex(); got >= 2 {
		t.Errorf("leader s1 committed up to index %d while index 2 holds a term-2 entry and no term-4 entry has committed; "+
			"that is exactly the Figure 8 hazard\n%s", got, c.diagnostics())
	}
	if got := c.nodes[4].sm.Get("fig8"); got != "" {
		t.Errorf("isolated s5 applied %q; it should have applied nothing\n%s", got, c.diagnostics())
	}

	// Now let the term-4 entry through. Committing it commits everything before
	// it, index 2 included, and only now is that safe.
	blockTerm4.off()
	deadline = time.Now().Add(scenarioTimeout)
	for time.Now().Before(deadline) && c.node(0).CommitIndex() < 3 {
		time.Sleep(time.Millisecond)
	}
	if got := c.node(0).CommitIndex(); got < 3 {
		t.Errorf("leader s1 stalled at commit index %d after the term-4 entry was allowed through\n%s",
			got, c.diagnostics())
	}
}

// TestFigure8_UncommittedOldTermEntryMayBeOverwritten is the other half of
// Figure 8. Having established that the term-2 entry on a majority is not
// committed, this test lets the node that holds a different entry at that index
// back into the cluster and kills the leader.
//
// Whatever the cluster decides — keep the term-2 entry or replace it with the
// term-3 one — is legal, precisely because neither was ever committed. What
// must not happen is that the cluster disagrees with itself, or that a node
// that had already told a client the term-2 entry was committed sees it
// replaced. The invariant checker is watching for both.
func TestFigure8_UncommittedOldTermEntryMayBeOverwritten(t *testing.T) {
	c, blockTerm4 := newFigure8Cluster(t)
	c.net.Isolate(c.ids[4])
	c.electLeader(0, scenarioTimeout, blockTerm4.rule)

	deadline := time.Now().Add(scenarioTimeout)
	for time.Now().Before(deadline) && c.countStoresWith(2, 2) < 3 {
		time.Sleep(time.Millisecond)
	}
	if n := c.countStoresWith(2, 2); n < 3 {
		t.Skipf("index 2 (term 2) reached only %d of 5 logs; scenario not set up\n%s", n, c.diagnostics())
	}
	if got := c.node(0).CommitIndex(); got >= 2 {
		t.Fatalf("leader s1 committed index %d before any term-4 entry; see the companion Figure 8 test\n%s",
			got, c.diagnostics())
	}

	// s1 dies without ever committing its own term's entry; s5 comes back.
	c.crash(0)
	c.setFilters()
	c.net.HealAll()

	// Let the survivors sort it out.
	deadline = time.Now().Add(scenarioTimeout)
	var converged bool
	for time.Now().Before(deadline) {
		if c.logsAgreeAt(2, []int{1, 2, 3, 4}) {
			converged = true
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if !converged {
		t.Errorf("the four survivors never agreed on index 2\n%s", c.diagnostics())
	}
}

// term4Block is the content filter that withholds s1's own-term entries.
type term4Block struct {
	on   atomic.Bool
	rule func(simnet.Message) bool
}

func (b *term4Block) off() { b.on.Store(false) }

// newFigure8Cluster builds the five pre-seeded logs of Figure 8 and the filter
// that lets the test decide when the leader's own-term entry may replicate.
//
// MaxLogEntriesPerRPC is pinned to 1 so that the old-term entry and the new
// one travel in separate messages; otherwise they would arrive together and
// there would be no window in which only the old one is on a majority.
func newFigure8Cluster(t *testing.T) (*simCluster, *term4Block) {
	t.Helper()
	logs := [][]raft.LogEntry{
		{entry(1, 1, encodePut("fig8", "t1")), entry(2, 2, encodePut("fig8", "t2"))},
		{entry(1, 1, encodePut("fig8", "t1"))},
		{entry(1, 1, encodePut("fig8", "t1"))},
		{entry(1, 1, encodePut("fig8", "t1"))},
		{entry(1, 1, encodePut("fig8", "t1")), entry(2, 3, encodePut("fig8", "t3"))},
	}

	cfg := defaultSimConfig(simSeed(t))
	cfg.nodes = 5
	cfg.preseed = func(i int, store raft.Storage) {
		seedLog(t, store, raft.HardState{CurrentTerm: 3}, logs[i]...)
	}
	cfg.mutate = func(_ int, rc *raft.Config) { rc.MaxLogEntriesPerRPC = 1 }

	block := &term4Block{}
	block.on.Store(true)
	block.rule = func(m simnet.Message) bool {
		if !block.on.Load() || m.From != "s1" {
			return true
		}
		ae, ok := m.Req.(*raft.AppendEntriesRequest)
		if !ok {
			return true
		}
		for _, e := range ae.Entries {
			if e.Term == 4 {
				return false
			}
		}
		return true
	}
	return newSimCluster(t, &cfg), block
}

// logsAgreeAt reports whether every listed member holds the same entry at idx.
func (c *simCluster) logsAgreeAt(idx raft.Index, members []int) bool {
	var want raft.LogEntry
	first := true
	for _, i := range members {
		e, err := c.nodes[i].store.GetLogEntry(context.Background(), idx)
		if err != nil {
			return false
		}
		if first {
			want, first = e, false
			continue
		}
		if e.Term != want.Term || !bytes.Equal(e.Command, want.Command) {
			return false
		}
	}
	return !first
}

// ---- Commit index on a bare heartbeat --------------------------------------

// TestFollowerCommitIndexFromHeartbeat protects AppendEntries rule 5: a
// follower may only advance its commit index as far as the last entry the
// leader actually sent it, never as far as its own last log index.
//
// Why the distinction matters. A follower that comes back from a partition can
// hold a tail of entries from an earlier term that no longer exist on the
// leader. Until the leader has walked nextIndex back far enough to overwrite
// that tail, those indices hold the wrong entries. If a heartbeat carrying a
// high leaderCommit arrives in that window and the follower clamps it to its
// own last log index rather than to what it was just sent, it will mark its
// stale entries committed and hand them to its state machine — entries that
// were never committed by anybody, at indices where the cluster committed
// something else.
//
// The scenario:
//
//	s2 holds 1:t1 "stale", left over from a term-1 leader that nobody else
//	heard from. s1 is elected in term 2 with an empty log, so its no-op lands
//	at index 1 too, and commits with s3's vote. Every AppendEntries from s1 to
//	s2 that carries entries is dropped, so s2 never learns the real index 1 —
//	but the heartbeats still arrive, and after the usual nextIndex back-off
//	they arrive with PrevLogIndex 0 and LeaderCommit 1.
//
// At that point s2 must still have commit index 0 and must have applied
// nothing. This was once wrong: the clamp used the follower's own last log
// index, so s2 committed and applied its stale entry, permanently — an applied
// index is never revisited, so no later repair of the log could undo it.
func TestFollowerCommitIndexFromHeartbeat(t *testing.T) {
	cfg := defaultSimConfig(simSeed(t))
	cfg.nodes = 3
	cfg.preseed = func(i int, store raft.Storage) {
		hs := raft.HardState{CurrentTerm: 1}
		if i == 1 {
			seedLog(t, store, hs, entry(1, 1, encodePut("x", "stale")))
			return
		}
		seedLog(t, store, hs)
	}
	c := newSimCluster(t, &cfg)

	// Withhold every entry-carrying message to s2, so the only thing it ever
	// receives from the leader is a heartbeat.
	starve := func(m simnet.Message) bool {
		if m.To != "s2" {
			return true
		}
		ae, ok := m.Req.(*raft.AppendEntriesRequest)
		return !ok || len(ae.Entries) == 0
	}
	c.electLeader(0, scenarioTimeout, starve)

	// The leader's own no-op commits on s1+s3.
	deadline := time.Now().Add(scenarioTimeout)
	for time.Now().Before(deadline) && c.node(0).CommitIndex() < 1 {
		time.Sleep(time.Millisecond)
	}
	if c.node(0).CommitIndex() < 1 {
		t.Fatalf("leader never committed its no-op\n%s", c.diagnostics())
	}

	// Give the leader time to back nextIndex[s2] down to 0 and heartbeat there.
	c.tickFor(200 * time.Millisecond)

	if commit := c.node(1).CommitIndex(); commit != 0 {
		t.Errorf("s2 has commit index %d, want 0: it was never sent the entry at index 1, "+
			"so nothing in its log has been committed as far as it can know\n%s",
			commit, c.diagnostics())
	}
	if applied := c.nodes[1].sm.Get("x"); applied != "" {
		t.Errorf("s2 applied %q at index 1, where the cluster committed the leader's no-op; "+
			"that entry came from a term-1 leader and was never committed by anybody\n%s",
			applied, c.diagnostics())
	}

	// The leader's own view must be unaffected by any of this.
	if commit := c.node(0).CommitIndex(); commit < 1 {
		t.Errorf("leader commit index fell back to %d\n%s", commit, c.diagnostics())
	}
}

func itoa(v uint64) string {
	if v == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}

// ---- Log conflict across a snapshot boundary -------------------------------

// TestSnapshotBoundary_ConflictingTailIsRepaired protects the repair path when
// a follower's divergent entries sit below the leader's first log index.
//
// The normal repair is nextIndex back-off: the leader walks backwards until it
// finds an index where the follower agrees, then overwrites forward. That only
// works while the leader still has the entries to walk back over. Once it has
// snapshotted and compacted past the divergence point there is nothing to walk
// back to, and the leader must switch to InstallSnapshot instead. Getting the
// hand-off between the two wrong leaves a follower permanently stuck — and, if
// the snapshot is installed without discarding the conflicting tail, silently
// corrupt.
//
// The divergence is produced naturally rather than fabricated: an isolated
// leader keeps accepting proposals it can never commit, building a tail of
// entries in its own term that the rest of the cluster has never seen. The
// majority meanwhile elects a new leader and commits enough entries to snapshot
// past that point.
func TestSnapshotBoundary_ConflictingTailIsRepaired(t *testing.T) {
	cfg := defaultSimConfig(simSeed(t))
	cfg.nodes = 5
	cfg.snapshotThreshold = 8
	c := newSimCluster(t, &cfg)

	c.electLeader(0, scenarioTimeout)
	ctx, cancel := context.WithTimeout(context.Background(), scenarioTimeout)
	defer cancel()
	if c.put(ctx, "base", "0") != opOK {
		t.Fatalf("initial write did not commit\n%s", c.diagnostics())
	}

	// Cut the leader off and keep feeding it. Each proposal is appended to its
	// log and then never commits, which is exactly the divergent tail we want.
	c.net.Isolate(c.ids[0])
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			pctx, pcancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			if n := c.node(0); n != nil {
				_, _ = n.Propose(pctx, encodePut("orphan", itoa(uint64(i))))
			}
			pcancel()
		}
	}()

	// The majority elects someone else and commits enough to snapshot well past
	// the point where the old leader's log diverges.
	c.electLeader(1, scenarioTimeout)
	for i := range 30 {
		wctx, wcancel := context.WithTimeout(context.Background(), time.Second)
		c.put(wctx, "k", itoa(uint64(i)))
		wcancel()
	}
	close(stop)
	wg.Wait()

	if snap := c.nodes[1].store.SnapshotIndex(); snap == 0 {
		t.Skipf("the new leader never snapshotted; nothing to test\n%s", c.diagnostics())
	}

	// Let the old leader back in. Its tail must be discarded and its state
	// machine rebuilt, whether that happens through log repair or through a
	// snapshot install.
	c.net.HealAll()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if c.nodes[0].sm.Get("k") == itoa(29) && c.nodes[0].sm.Get("orphan") == "" {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Errorf("the rejoining node did not converge: k=%q (want %q), orphan=%q (want empty)\n%s",
		c.nodes[0].sm.Get("k"), itoa(29), c.nodes[0].sm.Get("orphan"), c.diagnostics())
}

// ---- Leadership transfer during a configuration change ---------------------

// TestLeadershipTransferDuringConfigChange protects the interaction between two
// features that each move the cluster's notion of "who decides" and that are
// rarely exercised together.
//
// A configuration change alters the quorum a commit needs; a leadership
// transfer hands the term to a node that may not yet have the config entry.
// Overlapping them is where an implementation can end up committing under one
// quorum and counting votes under another. The cluster is allowed to reject
// either request while the other is outstanding — that is the safe answer — but
// it must not end up with two leaders, a split membership view, or a log that
// fails any of the safety properties.
func TestLeadershipTransferDuringConfigChange(t *testing.T) {
	cfg := defaultSimConfig(simSeed(t))
	cfg.nodes = 5
	c := newSimCluster(t, &cfg)

	c.electLeader(0, scenarioTimeout)
	ctx, cancel := context.WithTimeout(context.Background(), scenarioTimeout)
	defer cancel()
	if c.put(ctx, "before", "1") != opOK {
		t.Fatalf("write before the config change did not commit\n%s", c.diagnostics())
	}

	leader := c.node(0)
	var wg sync.WaitGroup
	var removeErr, transferErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		rctx, rcancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer rcancel()
		removeErr = leader.RemoveServer(rctx, c.ids[4])
	}()
	go func() {
		defer wg.Done()
		tctx, tcancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer tcancel()
		transferErr = leader.TransferLeadership(tctx, c.ids[1])
	}()
	wg.Wait()
	t.Logf("RemoveServer(%s) → %v; TransferLeadership(%s) → %v", c.ids[4], removeErr, c.ids[1], transferErr)

	// Whatever happened, the cluster must settle on exactly one leader that can
	// still commit.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if countLeaders(c) == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if n := countLeaders(c); n != 1 {
		t.Fatalf("cluster settled with %d leaders, want exactly 1\n%s", n, c.diagnostics())
	}

	wctx, wcancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer wcancel()
	if c.put(wctx, "after", "2") != opOK {
		t.Errorf("no write committed after the overlapping transfer and config change\n%s", c.diagnostics())
	}

	// Every member that still believes it is in the cluster must eventually
	// agree on who else is, or the next election will be decided by a quorum
	// nobody agrees on. This is an eventual property: a follower only drops the
	// removed member when the configuration entry reaches it and commits.
	if removeErr == nil {
		deadline = time.Now().Add(10 * time.Second)
		var stale raft.NodeID
		for time.Now().Before(deadline) {
			stale = ""
			for i := range 4 {
				n := c.node(i)
				if n == nil {
					continue
				}
				for _, m := range n.Members() {
					if m.ID == c.ids[4] {
						stale = c.ids[i]
					}
				}
			}
			if stale == "" {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		if stale != "" {
			t.Errorf("%s still lists the removed member %s after the change committed\n%s",
				stale, c.ids[4], c.diagnostics())
		}
	}
}

func countLeaders(c *simCluster) int {
	n := 0
	for i := range c.nodes {
		if node := c.node(i); node != nil && node.State() == raft.Leader {
			n++
		}
	}
	return n
}

// ---- Election restriction --------------------------------------------------

// TestVoteDeniedToStaleCandidate protects Raft's election restriction (§5.4.1):
// a voter must refuse any candidate whose log is not at least as up to date as
// its own, comparing last term first and only then last index.
//
// This is the single rule that makes Leader Completeness hold. Without it a
// node that missed the last few commits could be elected and would then
// overwrite them on everybody else.
func TestVoteDeniedToStaleCandidate(t *testing.T) {
	// Voter's log: index 3, term 5.
	voterLog := []raft.LogEntry{
		entry(1, 1, encodePut("a", "1")),
		entry(2, 5, encodePut("b", "2")),
		entry(3, 5, encodePut("c", "3")),
	}

	tests := []struct {
		name         string
		lastLogIndex raft.Index
		lastLogTerm  raft.Term
		wantGranted  bool
	}{
		{"older last term, longer log", 9, 4, false},
		{"older last term, same length", 3, 4, false},
		{"same last term, shorter log", 2, 5, false},
		{"same last term, same length", 3, 5, true},
		{"same last term, longer log", 4, 5, true},
		{"newer last term, shorter log", 1, 6, true},
		{"empty candidate log", 0, 0, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := memstore.New()
			seedLog(t, store, raft.HardState{CurrentTerm: 5}, voterLog...)

			cfg := raft.DefaultConfig()
			cfg.ID = "voter"
			cfg.Peers = []raft.PeerConfig{{ID: "candidate", Voter: true}, {ID: "other", Voter: true}}
			cfg.Storage = store
			cfg.StateMachine = newSimKV("voter", nil)
			cfg.Transport = &noopTransport{}
			cfg.TickInterval = 0
			cfg.Logger = discardLogger()
			n, err := raft.New(&cfg)
			if err != nil {
				t.Fatalf("raft.New: %v", err)
			}
			n.Start()
			defer n.Stop()

			resp, err := n.Handler().HandleRequestVote(context.Background(), &raft.RequestVoteRequest{
				Term:         6, // higher term, so only the log check can deny
				CandidateID:  "candidate",
				LastLogIndex: tc.lastLogIndex,
				LastLogTerm:  tc.lastLogTerm,
			})
			if err != nil {
				t.Fatalf("HandleRequestVote: %v", err)
			}
			if resp.VoteGranted != tc.wantGranted {
				t.Errorf("candidate with last log %d/%d against voter's 3/5: granted=%v, want %v",
					tc.lastLogIndex, tc.lastLogTerm, resp.VoteGranted, tc.wantGranted)
			}
		})
	}
}

// TestStaleNodeCannotStealLeadership is the cluster-level form of the same
// rule. A node is cut off while the rest of the cluster commits a long run of
// entries, then let back in. It must not be able to take the term from a
// healthy leader, because a leader elected on its short log would have to
// discard everything committed in its absence.
func TestStaleNodeCannotStealLeadership(t *testing.T) {
	cfg := defaultSimConfig(simSeed(t))
	cfg.nodes = 5
	c := newSimCluster(t, &cfg)

	c.electLeader(0, scenarioTimeout)
	c.net.Isolate(c.ids[4])

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for i := range 20 {
		if c.put(ctx, "k", itoa(uint64(i))) != opOK {
			t.Fatalf("write %d did not commit while s5 was isolated\n%s", i, c.diagnostics())
		}
	}
	commit := c.node(0).CommitIndex()

	// Let the stale node campaign as hard as it likes.
	c.net.HealAll()
	c.setFilters(onlyCampaigner(c.ids[4]))
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if st := c.node(4).State(); st == raft.Leader {
			t.Fatalf("stale node %s became leader with commit index %d behind the cluster's %d\n%s",
				c.ids[4], c.node(4).CommitIndex(), commit, c.diagnostics())
		}
		time.Sleep(time.Millisecond)
	}

	// And it must catch up once it stops trying to lead.
	c.setFilters()
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if c.nodes[4].sm.Get("k") == itoa(19) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Errorf("stale node never caught up: k=%q, want %q\n%s", c.nodes[4].sm.Get("k"), itoa(19), c.diagnostics())
}

// ---- Durability of the vote ------------------------------------------------

// TestVoteSurvivesRestartWithinTerm protects the persistence half of "one vote
// per term".
//
// The in-memory check — "have I already voted in this term?" — is worthless if
// the answer is lost on restart. A node that votes for A in term 7, restarts,
// and then votes for B in term 7 has let two candidates each collect a majority
// in the same term, which is how two leaders in one term happen. Raft requires
// currentTerm and votedFor to be on stable storage before the vote is
// acknowledged, and this test restarts the node between the two requests to
// check that they were.
func TestVoteSurvivesRestartWithinTerm(t *testing.T) {
	tests := []struct {
		name          string
		secondVoterID raft.NodeID
		wantGranted   bool
	}{
		{"different candidate is denied", "candidate-b", false},
		{"same candidate is granted again", "candidate-a", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := simnet.NewFaultStore(memstore.New())
			net := simnet.New(simSeed(t))
			t.Cleanup(func() { _ = net.Close() })

			build := func() *raft.Node {
				cfg := raft.DefaultConfig()
				cfg.ID = "voter"
				cfg.Peers = []raft.PeerConfig{
					{ID: "candidate-a", Voter: true},
					{ID: "candidate-b", Voter: true},
				}
				cfg.Storage = store
				cfg.StateMachine = newSimKV("voter", nil)
				cfg.Transport = net.NewTransport("voter")
				cfg.TickInterval = 0
				cfg.Logger = discardLogger()
				n, err := raft.New(&cfg)
				if err != nil {
					t.Fatalf("raft.New: %v", err)
				}
				n.Start()
				return n
			}

			n := build()
			resp, err := n.Handler().HandleRequestVote(context.Background(), &raft.RequestVoteRequest{
				Term:        7,
				CandidateID: "candidate-a",
			})
			if err != nil {
				t.Fatalf("first HandleRequestVote: %v", err)
			}
			if !resp.VoteGranted {
				t.Fatalf("first vote was not granted; the test needs it to be")
			}

			// Power cut: the node goes away, the storage keeps only what it had
			// made durable, and a new node starts on it.
			n.Stop()
			if crashErr := store.Crash(context.Background()); crashErr != nil {
				t.Fatalf("store.Crash: %v", crashErr)
			}
			n = build()
			defer n.Stop()

			if got := n.Term(); got != 7 {
				t.Errorf("term after restart is %d, want 7: the term was not persisted with the vote", got)
			}

			resp, err = n.Handler().HandleRequestVote(context.Background(), &raft.RequestVoteRequest{
				Term:        7,
				CandidateID: tc.secondVoterID,
			})
			if err != nil {
				t.Fatalf("second HandleRequestVote: %v", err)
			}
			if resp.VoteGranted != tc.wantGranted {
				t.Errorf("after restart, vote for %s in term 7: granted=%v, want %v (first vote went to candidate-a)",
					tc.secondVoterID, resp.VoteGranted, tc.wantGranted)
			}
		})
	}
}

// ---- Lease reads under clock skew ------------------------------------------

// TestLeaseReadUnderClockSkew protects the one part of this implementation whose
// safety argument rests on clocks rather than on messages.
//
// ReadIndexLease lets a leader answer a linearizable read out of its own state
// machine, with no round-trip, on the strength of a lease that runs for
// ElectionTimeoutMin from the last heartbeat it sent. The argument is that no
// follower can start an election before its own election timer expires, so
// within that window the leader is still the leader. The argument is stated in
// the leader's clock. If that clock runs slow, the window the leader believes
// in is longer than the one the followers are actually keeping, and a leader
// that has already been replaced can answer a read from stale data.
//
// What stops that from being unbounded is CheckQuorum: a leader that has not
// heard from a majority within one election timeout steps down, and stepping
// down clears the lease. The election timer is counted in ticks, not in the
// leader's skewed clock, so the bound holds however wrong the clock is. This
// test gives the leader clocks from a quarter speed to four times speed and
// requires, for every one of them, that an isolated leader stops answering
// lease reads.
func TestLeaseReadUnderClockSkew(t *testing.T) {
	tests := []struct {
		name string
		rate float64
	}{
		{"leader clock at quarter speed", 0.25},
		{"leader clock in step", 1.0},
		{"leader clock at quadruple speed", 4.0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := defaultSimConfig(simSeed(t))
			cfg.nodes = 3
			// One logical tick per 10ms of wall clock is what Config converts
			// durations with when TickInterval is 0. Matching it here is what
			// makes the comparison between the lease (measured in wall clock)
			// and the election timeout (counted in ticks) mean anything: with
			// the 1ms ticks the other tests use, logical time runs ten times
			// faster than the lease's clock and the test would prove nothing.
			cfg.tickEvery = 10 * time.Millisecond
			cfg.mutate = func(i int, rc *raft.Config) {
				rc.ElectionTimeoutMin = 50 * time.Millisecond
				rc.ElectionTimeoutMax = 100 * time.Millisecond
				rc.HeartbeatInterval = 20 * time.Millisecond
				rate := 1.0
				if i == 0 {
					rate = tc.rate
				}
				rc.Clock = simnet.NewSkewClock(rate, 0)
			}
			c := newSimCluster(t, &cfg)
			c.electLeader(0, scenarioTimeout)

			ctx, cancel := context.WithTimeout(context.Background(), scenarioTimeout)
			defer cancel()
			if c.put(ctx, "lease", "before") != opOK {
				t.Fatalf("write before isolation did not commit\n%s", c.diagnostics())
			}

			// A lease is only granted once a full ReadIndex barrier round has
			// confirmed leadership; that round is what the lease then lets
			// subsequent reads skip.
			if _, err := c.node(0).ReadIndex(ctx); err != nil {
				t.Fatalf("ReadIndex on the healthy leader: %v\n%s", err, c.diagnostics())
			}

			// A healthy leader must be able to use its lease. The point of the
			// mechanism is availability, and a test that only checked safety
			// would pass on an implementation that never granted one.
			leaseCtx, leaseCancel := context.WithTimeout(context.Background(), time.Second)
			idx, err := c.node(0).ReadIndexLease(leaseCtx)
			leaseCancel()
			if err != nil {
				t.Fatalf("healthy leader refused a lease read: %v\n%s", err, c.diagnostics())
			}
			if idx == 0 {
				t.Errorf("healthy leader returned read index 0\n%s", c.diagnostics())
			}

			// Now cut it off. It keeps ticking, so CheckQuorum applies. The
			// window during which it still believes in its lease is the window
			// in which it could answer a read from data the rest of the cluster
			// has already moved past, so it must be bounded — and bounded by
			// something other than its own clock, which is wrong by construction.
			c.net.Isolate(c.ids[0])
			start := time.Now()

			deadline := start.Add(5 * time.Second)
			var lastErr error
			for time.Now().Before(deadline) {
				rctx, rcancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
				_, lastErr = c.node(0).ReadIndexLease(rctx)
				rcancel()
				if lastErr != nil {
					break
				}
				time.Sleep(2 * time.Millisecond)
			}
			window := time.Since(start)
			if lastErr == nil {
				t.Fatalf("isolated leader kept serving lease reads for %s with a clock at %.2fx speed; "+
					"CheckQuorum should have made it step down\n%s", window, tc.rate, c.diagnostics())
			}
			t.Logf("isolated leader stopped serving lease reads after %s with: %v", window, lastErr)
			// CheckQuorum gives up after one election timeout (at most 100ms
			// here) counted in ticks, so the bound holds however far the
			// leader's clock has drifted. The allowance is generous because the
			// harness ticks from wall clock under the race detector.
			if window > 2*time.Second {
				t.Errorf("stale-read window was %s with a clock at %.2fx speed; it must be bounded by "+
					"CheckQuorum (one election timeout), not by the leader's own clock\n%s",
					window, tc.rate, c.diagnostics())
			}

			// The remaining two must now elect someone and commit, which is
			// what would have made any read the old leader served stale.
			wctx, wcancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer wcancel()
			if c.put(wctx, "lease", "after") != opOK {
				t.Errorf("the majority never committed after isolating the leader\n%s", c.diagnostics())
			}

			// And the old leader must still refuse: answering now would return
			// "before" for a key whose committed value is "after".
			rctx, rcancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			_, err = c.node(0).ReadIndexLease(rctx)
			rcancel()
			if err == nil && c.nodes[0].sm.Get("lease") != "after" {
				t.Errorf("isolated leader served a lease read returning %q while the cluster has committed %q\n%s",
					c.nodes[0].sm.Get("lease"), "after", c.diagnostics())
			}
		})
	}
}

// ---- Storage failure -------------------------------------------------------

// TestStorageFailure_ProposalIsNotAcknowledged protects the contract between
// the Raft engine and stable storage: nothing may be acknowledged to a client
// that has not been persisted.
//
// A disk that fills up or goes read-only is an ordinary operational event, and
// the only safe response is to fail the proposal. Acknowledging it would be
// worse than a crash: the client believes the write is durable, and the entry
// is gone the moment the process restarts. The node is allowed to fail, to step
// down, or to keep serving reads — it is not allowed to say yes.
func TestStorageFailure_ProposalIsNotAcknowledged(t *testing.T) {
	store := simnet.NewFaultStore(memstore.New())
	net := simnet.New(simSeed(t))
	t.Cleanup(func() { _ = net.Close() })

	sm := newSimKV("solo", nil)
	cfg := raft.DefaultConfig()
	cfg.ID = "solo"
	cfg.Storage = store
	cfg.StateMachine = sm
	cfg.Transport = net.NewTransport("solo")
	cfg.TickInterval = 0
	cfg.Logger = discardLogger()
	n, err := raft.New(&cfg)
	if err != nil {
		t.Fatalf("raft.New: %v", err)
	}
	n.Start()
	defer n.Stop()

	deadline := time.Now().Add(scenarioTimeout)
	for time.Now().Before(deadline) && n.State() != raft.Leader {
		n.Tick()
		time.Sleep(time.Millisecond)
	}
	if n.State() != raft.Leader {
		t.Fatalf("the sole voter did not elect itself")
	}

	ctx, cancel := context.WithTimeout(context.Background(), scenarioTimeout)
	defer cancel()
	if _, proposeErr := n.Propose(ctx, encodePut("disk", "healthy")); proposeErr != nil {
		t.Fatalf("write on a healthy disk: %v", proposeErr)
	}

	store.FailWrites()
	if _, proposeErr := n.Propose(ctx, encodePut("disk", "failed")); proposeErr == nil {
		t.Errorf("Propose succeeded while every storage write was failing; " +
			"the entry cannot be on stable storage, so the client must not be told it is")
	}
	if got := sm.Get("disk"); got != "healthy" {
		t.Errorf("state machine holds %q after a failed write, want %q", got, "healthy")
	}

	// Recovery is a restart, not a resumption. A node that could not complete a
	// durable write has no way to know what it did and did not write, so the
	// contract is that it stops and comes back on storage that works. Asserting
	// in-place recovery here would pin an implementation detail rather than the
	// guarantee, which is that nothing acknowledged is ever lost.
	store.AllowWrites()
	n.Stop()

	restartedSM := newSimKV("solo", nil)
	restartCfg := cfg
	restartCfg.StateMachine = restartedSM
	restartCfg.Transport = net.NewTransport("solo")
	restarted, err := raft.New(&restartCfg)
	if err != nil {
		t.Fatalf("raft.New after recovery: %v", err)
	}
	restarted.Start()
	defer restarted.Stop()

	deadline = time.Now().Add(scenarioTimeout)
	for time.Now().Before(deadline) && restarted.State() != raft.Leader {
		restarted.Tick()
		time.Sleep(time.Millisecond)
	}
	if restarted.State() != raft.Leader {
		t.Fatalf("the sole voter did not elect itself after restarting on a healthy disk")
	}

	// The write that failed must not have survived, and the one before it must.
	if got := restartedSM.Get("disk"); got != "healthy" {
		t.Errorf("state machine holds %q after restarting, want %q: a write that was "+
			"never acknowledged came back, or one that was acknowledged was lost",
			got, "healthy")
	}

	rctx, rcancel := context.WithTimeout(context.Background(), scenarioTimeout)
	defer rcancel()
	if _, err := restarted.Propose(rctx, encodePut("disk", "recovered")); err != nil {
		t.Errorf("Propose after restarting on a healthy disk: %v", err)
	}
	if got := restartedSM.Get("disk"); got != "recovered" {
		t.Errorf("state machine holds %q after recovery, want %q", got, "recovered")
	}
}
