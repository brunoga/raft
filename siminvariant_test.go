package raft_test

// A continuously running checker for the five safety properties a Raft
// implementation is obliged to preserve. It watches the whole cluster while a
// test runs and fails that test the moment any of them is broken, naming the
// nodes and indices involved so the failure is actionable rather than a
// mystery.
//
// The properties, in the numbering used by the Raft paper (§5.2, §5.3, §5.4):
//
//	Election Safety      at most one leader is elected in a given term
//	Log Matching         if two logs hold an entry with the same index and
//	                     term, the logs are identical in every preceding entry
//	Leader Completeness  an entry committed in some term is present in the log
//	                     of every leader of every later term
//	State Machine Safety no two nodes apply a different command at the same
//	                     log index
//	Monotonic commit     a node's commit index never moves backwards while it
//	                     is running, and no index is re-applied with different
//	                     content
//
// Log Matching and Leader Completeness are checked by a background sweep over
// every node's on-disk log. Election Safety and the leader-side commit advance
// are observed exactly, through the raft.Metrics hook that the event loop calls
// on every role transition and every commit advance. State Machine Safety is
// checked where it is defined: at the state machine, which reports every entry
// it is handed.

import (
	"bytes"
	"context"
	"fmt"
	"hash/fnv"
	"sort"
	"sync"
	"time"

	"github.com/brunoga/raft"
	"github.com/brunoga/raft/internal/simnet"
)

// entryID is the identity of a log entry: its term and a hash of its command.
// Two entries with the same index and the same entryID are the same entry.
type entryID struct {
	term raft.Term
	hash uint64
}

func (e entryID) String() string { return fmt.Sprintf("term=%d cmd=%#x", e.term, e.hash) }

func hashCmd(cmd []byte) uint64 {
	h := fnv.New64a()
	_, _ = h.Write(cmd)
	return h.Sum64()
}

func idOf(e raft.LogEntry) entryID { return entryID{term: e.Term, hash: hashCmd(e.Command)} }

// checkedNode is the checker's handle on one cluster member. The store outlives
// any individual Node: a crash-restart replaces node but keeps store, which is
// exactly what makes the restart a restart and not a fresh member.
type checkedNode struct {
	id    raft.NodeID
	store *simnet.FaultStore

	mu   sync.Mutex
	node *raft.Node // nil while the node is crashed
}

func (cn *checkedNode) current() *raft.Node {
	cn.mu.Lock()
	defer cn.mu.Unlock()
	return cn.node
}

// committedEntry is an entry a leader has declared committed, plus the term in
// which it did so.
//
// The distinction between an entry's own term and the term it was committed in
// is not pedantry: it is the whole content of Figure 8. An entry written in
// term 2 can sit uncommitted for several terms and only become committed when a
// much later leader commits an entry of its own term above it. Leader
// Completeness is a statement about the commit, not about the write, so it
// constrains the leaders of terms after the commit — not the leaders of terms
// after the entry was first appended.
type committedEntry struct {
	id entryID
	// commitTerm is the term of the leader observed to have committed it. It is
	// an upper bound: the commit may have happened in an earlier term that no
	// sweep caught, which makes the check conservative but never wrong.
	commitTerm raft.Term
}

// applyRecord is what some node applied at a given log index.
type applyRecord struct {
	id  entryID
	by  raft.NodeID
	cmd []byte
}

// invariantChecker watches a cluster for safety violations.
//
// Safe for concurrent use: the Metrics hooks are called from each node's event
// loop, recordApply from each node's apply loop, and sweep from the checker's
// own goroutine.
type invariantChecker struct {
	t violationReporter
	// diag returns the reproduction footer (seed, fault schedule) appended to
	// every violation. It is a func so the schedule is captured at failure time.
	diag func() string

	mu    sync.Mutex
	nodes map[raft.NodeID]*checkedNode
	order []raft.NodeID

	// leaders records the leader observed for each term.
	leaders map[raft.Term]raft.NodeID
	// leaderTerms is leaders' key set, kept sorted for deterministic reporting.
	leaderTerms []raft.Term
	// committed records entries a leader has declared committed, together with
	// the term of the leader that declared it.
	committed map[raft.Index]committedEntry
	// applied records, per log index, the first command any node applied there.
	applied map[raft.Index]applyRecord
	// lastCommit is the highest commit index seen for each node since it last
	// started. Commit index is volatile state, so a restart resets it.
	lastCommit map[raft.NodeID]raft.Index
	seen       map[string]bool // violation dedup
	violations []string

	stopCh chan struct{}
	doneCh chan struct{}
}

// violationReporter is the slice of testing.TB the checker needs. Keeping it to
// an interface lets the checker's own tests capture what it reports instead of
// failing the test that is exercising it.
type violationReporter interface {
	Helper()
	Errorf(format string, args ...any)
	Logf(format string, args ...any)
}

func newInvariantChecker(t violationReporter, diag func() string) *invariantChecker {
	return &invariantChecker{
		t:          t,
		diag:       diag,
		nodes:      make(map[raft.NodeID]*checkedNode),
		leaders:    make(map[raft.Term]raft.NodeID),
		committed:  make(map[raft.Index]committedEntry),
		applied:    make(map[raft.Index]applyRecord),
		lastCommit: make(map[raft.NodeID]raft.Index),
		seen:       make(map[string]bool),
		stopCh:     make(chan struct{}),
		doneCh:     make(chan struct{}),
	}
}

// addNode registers a cluster member and the store that survives its restarts.
func (c *invariantChecker) addNode(id raft.NodeID, store *simnet.FaultStore) *checkedNode {
	c.mu.Lock()
	defer c.mu.Unlock()
	cn := &checkedNode{id: id, store: store}
	c.nodes[id] = cn
	c.order = append(c.order, id)
	return cn
}

// attach points the checker at a freshly started Node for id. Commit index is
// volatile, so the monotonicity baseline is reset here: a node that restarts
// legitimately comes back with commitIndex 0 and relearns it from the leader.
func (c *invariantChecker) attach(id raft.NodeID, n *raft.Node) {
	c.mu.Lock()
	cn := c.nodes[id]
	delete(c.lastCommit, id)
	c.mu.Unlock()
	if cn == nil {
		return
	}
	cn.mu.Lock()
	cn.node = n
	cn.mu.Unlock()
}

// detach tells the checker that id is down. Its log is still checked; its
// volatile state is not.
func (c *invariantChecker) detach(id raft.NodeID) {
	c.mu.Lock()
	cn := c.nodes[id]
	delete(c.lastCommit, id)
	c.mu.Unlock()
	if cn == nil {
		return
	}
	cn.mu.Lock()
	cn.node = nil
	cn.mu.Unlock()
}

// fail records a violation. Violations are deduplicated by message so that one
// broken invariant does not bury the output in thousands of copies of the same
// line.
func (c *invariantChecker) fail(invariant, format string, args ...any) {
	msg := fmt.Sprintf("%s violated: %s", invariant, fmt.Sprintf(format, args...))

	c.mu.Lock()
	if c.seen[msg] {
		c.mu.Unlock()
		return
	}
	c.seen[msg] = true
	c.violations = append(c.violations, msg)
	c.mu.Unlock()

	c.t.Helper()
	if c.diag != nil {
		c.t.Errorf("%s\n%s", msg, c.diag())
	} else {
		c.t.Errorf("%s", msg)
	}
}

// violated reports whether any invariant has been broken. Test loops poll it so
// they can stop piling work onto an already-broken cluster.
func (c *invariantChecker) violated() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.violations) > 0
}

// ---- Exact observations via the raft.Metrics hook --------------------------

// checkerMetrics adapts the checker to raft.Metrics. The event loop calls these
// synchronously, so they must be cheap; both are.
type checkerMetrics struct{ c *invariantChecker }

func (m checkerMetrics) StateChange(id raft.NodeID, _, to raft.State, term raft.Term) {
	if to != raft.Leader {
		return
	}
	m.c.mu.Lock()
	prev, ok := m.c.leaders[term]
	if !ok {
		m.c.leaders[term] = id
		m.c.leaderTerms = append(m.c.leaderTerms, term)
	}
	m.c.mu.Unlock()

	if ok && prev != id {
		m.c.fail("Election Safety",
			"term %d has two leaders: %s and %s", term, prev, id)
	}
}

func (m checkerMetrics) CommitAdvanced(id raft.NodeID, commitIndex raft.Index) {
	m.c.noteCommit(id, commitIndex)
}

func (m checkerMetrics) SnapshotTaken(raft.NodeID, raft.Index, int) {}

// metrics returns the raft.Metrics implementation to install in a node Config.
func (c *invariantChecker) metrics() raft.Metrics { return checkerMetrics{c: c} }

// noteCommit enforces that a running node's commit index never decreases.
func (c *invariantChecker) noteCommit(id raft.NodeID, idx raft.Index) {
	c.mu.Lock()
	prev, ok := c.lastCommit[id]
	if !ok || idx >= prev {
		c.lastCommit[id] = idx
	}
	c.mu.Unlock()
	if ok && idx < prev {
		c.fail("Monotonic commit",
			"node %s commit index went backwards: %d → %d", id, prev, idx)
	}
}

// recordApply is called by the test state machine for every entry it applies.
// This is the direct check of State Machine Safety: the state machine is what
// the outside world sees, so the property is stated over what it was told to
// do, not over what happens to be in some log.
func (c *invariantChecker) recordApply(id raft.NodeID, e raft.LogEntry) {
	want := applyRecord{id: idOf(e), by: id, cmd: e.Command}
	c.mu.Lock()
	got, ok := c.applied[e.Index]
	if !ok {
		c.applied[e.Index] = want
	}
	c.mu.Unlock()
	if !ok || got.id == want.id {
		return
	}
	if got.by == id {
		c.fail("Monotonic commit",
			"node %s re-applied index %d with different content: first %s (%q), now %s (%q)",
			id, e.Index, got.id, truncateCmd(got.cmd), want.id, truncateCmd(want.cmd))
		return
	}
	c.fail("State Machine Safety",
		"index %d applied as %s (%q) by %s and as %s (%q) by %s",
		e.Index, got.id, truncateCmd(got.cmd), got.by, want.id, truncateCmd(want.cmd), id)
}

func truncateCmd(cmd []byte) string {
	const limit = 48
	if len(cmd) > limit {
		return string(cmd[:limit]) + "..."
	}
	return string(cmd)
}

// ---- Periodic sweep over the durable logs ----------------------------------

// start launches the sweep goroutine. It must be stopped before the test
// function returns, because it reports failures through the test's own
// *testing.T, which stops being usable once the function has returned.
func (c *invariantChecker) start(interval time.Duration) {
	go func() {
		defer close(c.doneCh)
		tk := time.NewTicker(interval)
		defer tk.Stop()
		for {
			select {
			case <-c.stopCh:
				return
			case <-tk.C:
				c.sweep()
			}
		}
	}()
}

func (c *invariantChecker) stop() {
	select {
	case <-c.stopCh:
	default:
		close(c.stopCh)
	}
	<-c.doneCh
}

// nodeList returns the members in registration order.
func (c *invariantChecker) nodeList() []*checkedNode {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]*checkedNode, 0, len(c.order))
	for _, id := range c.order {
		out = append(out, c.nodes[id])
	}
	return out
}

// readLog returns everything currently on a node's log, plus the index its
// snapshot covers. Reading straight from the store rather than from the Node
// deliberately bypasses every injected fault: the checker wants the truth.
func readLog(store *simnet.FaultStore) ([]raft.LogEntry, raft.Index) {
	snap := store.SnapshotIndex()
	first, err := store.FirstIndex()
	if err != nil || first == 0 {
		return nil, snap
	}
	last, err := store.LastIndex()
	if err != nil || last < first {
		return nil, snap
	}
	entries, err := store.GetLogEntries(context.Background(), first, last+1)
	if err != nil {
		return nil, snap
	}
	return entries, snap
}

// entryAt returns the entry at index from a contiguous log slice.
func entryAt(log []raft.LogEntry, idx raft.Index) (raft.LogEntry, bool) {
	if len(log) == 0 {
		return raft.LogEntry{}, false
	}
	first := log[0].Index
	if idx < first || idx > log[len(log)-1].Index {
		return raft.LogEntry{}, false
	}
	return log[idx-first], true
}

// nodeSnapshot is one node's state as read by a single sweep.
type nodeSnapshot struct {
	cn      *checkedNode
	log     []raft.LogEntry
	snapAt  raft.Index
	commit  raft.Index
	applied raft.Index
	term    raft.Term
	leader  bool
}

// sweep performs one full pass: refresh every log, then check the properties
// that are statements about whole logs rather than about single events.
func (c *invariantChecker) sweep() {
	nodes := c.nodeList()
	views := make([]nodeSnapshot, 0, len(nodes))
	for _, cn := range nodes {
		// Read the volatile state first and the log second, so the log can
		// only be newer than the indices recorded with it, never older.
		//
		// The log on disk is not a statement about how far the node has got.
		// Writes are carried out behind the event loop, so an entry can be in
		// the log, replicated and counted as committed while the disk still
		// holds what it replaced. Only what the node has applied is bounded by
		// what the disk holds, which is why the checks below are stated over
		// that rather than over the commit index.
		var commit, applied raft.Index
		var term raft.Term
		leader := false
		if n := cn.current(); n != nil {
			// Read what has been applied before what has been committed, so
			// the pair can only understate how far the node has got, never
			// overstate it.
			applied = n.LastApplied()
			commit = n.CommitIndex()
			term = n.Term()
			leader = n.State() == raft.Leader
			c.noteCommit(cn.id, commit)
		}
		log, snapAt := readLog(cn.store)
		views = append(views, nodeSnapshot{
			cn: cn, log: log, snapAt: snapAt,
			commit: commit, applied: applied, term: term, leader: leader,
		})
	}

	// Log Matching.
	for i := range views {
		for j := i + 1; j < len(views); j++ {
			c.checkLogMatching(views[i].cn.id, views[i].log, views[j].cn.id, views[j].log)
		}
	}

	// Record what leaders consider committed, then hold every other node's
	// committed prefix against it.
	for _, v := range views {
		if v.leader {
			c.recordCommitted(v.cn.id, v.term, v.log, v.commit)
		}
	}
	for _, v := range views {
		if !v.leader {
			c.checkCommittedPrefix(v.cn.id, v.log, v.commit, v.applied)
		}
	}

	c.checkLeaderCompleteness(views)
}

// checkLogMatching enforces Raft's Log Matching Property between two logs.
//
// Why it matters: every other log property is built on it. It is what lets a
// leader conclude, from one matching (index, term) pair, that a follower's
// entire prefix agrees with its own — which is the only reason AppendEntries
// can be a single consistency check rather than a full log comparison.
func (c *invariantChecker) checkLogMatching(idA raft.NodeID, a []raft.LogEntry, idB raft.NodeID, b []raft.LogEntry) {
	if len(a) == 0 || len(b) == 0 {
		return
	}
	lo := max(a[0].Index, b[0].Index)
	hi := min(a[len(a)-1].Index, b[len(b)-1].Index)
	if lo > hi {
		return
	}

	// The highest index at which the two logs agree on the term. If the logs
	// are identical up to there, they are identical at every lower index where
	// the terms agree too, so this single prefix check covers the property.
	agree := raft.Index(0)
	for idx := hi; idx >= lo; idx-- {
		ea, _ := entryAt(a, idx)
		eb, _ := entryAt(b, idx)
		if ea.Term == eb.Term {
			agree = idx
			break
		}
		if idx == lo {
			break
		}
	}
	if agree == 0 {
		return
	}
	for idx := lo; idx <= agree; idx++ {
		ea, _ := entryAt(a, idx)
		eb, _ := entryAt(b, idx)
		if ea.Term == eb.Term && bytes.Equal(ea.Command, eb.Command) {
			continue
		}
		c.fail("Log Matching",
			"%s and %s agree at index %d (term %d) but differ at index %d: %s has %s (%q), %s has %s (%q)",
			idA, idB, agree, mustTerm(a, agree), idx,
			idA, idOf(ea), truncateCmd(ea.Command),
			idB, idOf(eb), truncateCmd(eb.Command))
		return
	}
}

func mustTerm(log []raft.LogEntry, idx raft.Index) raft.Term {
	e, _ := entryAt(log, idx)
	return e.Term
}

// recordCommitted files away what a leader has declared committed. A leader's
// commit index is authoritative: it is only ever advanced after a quorum of
// voters has acknowledged the entry, and only for an entry of the leader's own
// term.
func (c *invariantChecker) recordCommitted(id raft.NodeID, term raft.Term, log []raft.LogEntry, commit raft.Index) {
	for idx := commit; idx >= 1; idx-- {
		e, ok := entryAt(log, idx)
		if !ok {
			break
		}
		want := committedEntry{id: idOf(e), commitTerm: term}
		c.mu.Lock()
		got, seen := c.committed[idx]
		if !seen {
			c.committed[idx] = want
		}
		c.mu.Unlock()
		if seen {
			if got.id != want.id {
				c.fail("State Machine Safety",
					"index %d committed as %s by an earlier leader and as %s by leader %s",
					idx, got.id, want.id, id)
			}
			// Everything below an index we have already recorded is recorded.
			return
		}
	}
}

// checkCommittedPrefix holds a follower's committed prefix against what leaders
// have committed. A follower must never hand its state machine anything but
// the entry the leader committed at that index.
//
// The bound is what the node has applied, not what it considers committed, and
// the difference is the whole of what asynchronous log writes changed. A
// follower takes an entry that replaces one it already had and learns in the
// same breath that the new entry is committed; the commit index moves at once,
// because the entry is in the log at once, while the disk still holds the
// entry that was replaced until the queued truncation and append reach it.
// Reading only the disk and the commit index, that node looks as though it
// considers committed an entry it does not hold.
//
// Nothing acts on that view. Applying reads from storage and stops at the
// point storage is known to hold the log's current entries, so the replaced
// entry is never handed over; and a commit index is not persisted, so a crash
// in that window brings the node back with none at all. What must be true,
// and is what this checks, is that everything it did hand over was the
// committed entry.
func (c *invariantChecker) checkCommittedPrefix(id raft.NodeID, log []raft.LogEntry, commit, applied raft.Index) {
	if applied < commit {
		commit = applied
	}
	for idx := commit; idx >= 1; idx-- {
		e, ok := entryAt(log, idx)
		if !ok {
			break
		}
		c.mu.Lock()
		got, seen := c.committed[idx]
		c.mu.Unlock()
		if !seen {
			continue
		}
		if got.id != idOf(e) {
			c.fail("State Machine Safety",
				"node %s applied index %d holding %s (%q), but the committed entry there is %s",
				id, idx, idOf(e), truncateCmd(e.Command), got.id)
			return
		}
	}
}

// checkLeaderCompleteness enforces that a leader of term T holds every entry
// committed in a term earlier than T.
//
// Why it matters: this is the property that makes "the leader has all the
// data" true, and therefore the property that lets a new leader serve reads and
// overwrite follower logs without first asking anyone what was committed. It is
// enforced by the up-to-date check in RequestVote; a bug there shows up here.
//
// Checking it after the fact is sound. A commit made while term T was current
// requires a majority of voters still in term T, so no leader of a term greater
// than T can have been elected before it. An entry that is missing from such a
// leader's log now is therefore either missing since its election or truncated
// since — both are violations.
func (c *invariantChecker) checkLeaderCompleteness(views []nodeSnapshot) {
	c.mu.Lock()
	terms := make([]raft.Term, len(c.leaderTerms))
	copy(terms, c.leaderTerms)
	leaders := make(map[raft.Term]raft.NodeID, len(c.leaders))
	for k, v := range c.leaders {
		leaders[k] = v
	}
	committed := make(map[raft.Index]committedEntry, len(c.committed))
	for k, v := range c.committed {
		committed[k] = v
	}
	c.mu.Unlock()

	sort.Slice(terms, func(i, j int) bool { return terms[i] < terms[j] })
	byID := make(map[raft.NodeID]nodeSnapshot, len(views))
	for _, v := range views {
		byID[v.cn.id] = v
	}

	for _, term := range terms {
		lid := leaders[term]
		lv, ok := byID[lid]
		if !ok {
			continue
		}
		for idx, want := range committed {
			if want.commitTerm >= term {
				// Committed in this term or a later one. A leader cannot be
				// required to hold an entry that was not yet committed when it
				// was elected — that is precisely the Figure 8 state.
				continue
			}
			if idx <= lv.snapAt {
				continue // compacted into a snapshot, therefore present
			}
			e, ok := entryAt(lv.log, idx)
			if !ok {
				if len(lv.log) > 0 && idx < lv.log[0].Index {
					continue // compacted away without a snapshot record
				}
				c.fail("Leader Completeness",
					"%s was leader in term %d but is missing index %d, which was committed as %s in term %d",
					lid, term, idx, want.id, want.commitTerm)
				continue
			}
			if idOf(e) != want.id {
				c.fail("Leader Completeness",
					"%s was leader in term %d but holds %s (%q) at index %d, where %s was committed in term %d",
					lid, term, idOf(e), truncateCmd(e.Command), idx, want.id, want.commitTerm)
			}
		}
	}
}

// finish runs one last sweep after the cluster has stopped and summarises.
func (c *invariantChecker) finish() {
	c.sweep()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.violations) == 0 {
		return
	}
	c.t.Helper()
	c.t.Logf("%d safety violation(s) recorded:", len(c.violations))
	for _, v := range c.violations {
		c.t.Logf("  %s", v)
	}
}
