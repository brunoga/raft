package raft_test

// Tests for the invariant checker.
//
// The checker is the thing that decides whether the chaos runs mean anything.
// A checker that cannot detect a violation turns every green run into a false
// negative, and a checker that is wrong about what a property says — which this
// one was, about Leader Completeness, until these tests were written — turns
// green runs into noise instead. So each property is fed a cluster state that
// breaks it, and the checker has to notice.

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/brunoga/raft/v2"
	"github.com/brunoga/raft/v2/internal/simnet"
	"github.com/brunoga/raft/v2/storage/memstore"
)

// recorder captures what the checker reports instead of failing the test.
type recorder struct {
	errors []string
	logs   []string
}

func (r *recorder) Helper() {}

func (r *recorder) Errorf(format string, args ...any) {
	r.errors = append(r.errors, sprintf(format, args...))
}

func (r *recorder) Logf(format string, args ...any) {
	r.logs = append(r.logs, sprintf(format, args...))
}

func sprintf(format string, args ...any) string { return fmt.Sprintf(format, args...) }

// reported reports whether any recorded failure mentions substr.
func (r *recorder) reported(substr string) bool {
	for _, e := range r.errors {
		if strings.Contains(e, substr) {
			return true
		}
	}
	return false
}

func (r *recorder) joined() string { return strings.Join(r.errors, "\n") }

// seedStore builds a FaultStore holding the given entries.
func seedStore(t *testing.T, entries ...raft.LogEntry) *simnet.FaultStore {
	t.Helper()
	inner := memstore.New()
	if len(entries) > 0 {
		if err := inner.AppendLogEntries(context.Background(), entries); err != nil {
			t.Fatalf("AppendLogEntries: %v", err)
		}
	}
	return simnet.NewFaultStore(inner)
}

func TestInvariantChecker_DetectsElectionSafety(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := &recorder{}
		c := newInvariantChecker(r, nil)
		m := c.metrics()

		m.StateChange("n1", raft.Candidate, raft.Leader, 4)
		m.StateChange("n2", raft.Candidate, raft.Leader, 4)

		if !r.reported("Election Safety") {
			t.Errorf("two leaders in term 4 were not reported; got: %s", r.joined())
		}
		if !c.violated() {
			t.Error("checker does not consider the cluster violated")
		}
	})
}

func TestInvariantChecker_AllowsOneLeaderPerTerm(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := &recorder{}
		c := newInvariantChecker(r, nil)
		m := c.metrics()

		m.StateChange("n1", raft.Candidate, raft.Leader, 4)
		m.StateChange("n1", raft.Candidate, raft.Leader, 4) // idempotent re-report
		m.StateChange("n2", raft.Candidate, raft.Leader, 5)
		m.StateChange("n1", raft.Candidate, raft.Leader, 6)

		if c.violated() {
			t.Errorf("a legal sequence of elections was reported as a violation: %s", r.joined())
		}
	})
}

func TestInvariantChecker_DetectsStateMachineSafety(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := &recorder{}
		c := newInvariantChecker(r, nil)

		// Same index, same term, different commands: two nodes handed their state
		// machines different work at one log position, which is the violation in
		// its purest form.
		c.recordApply("n1", entry(7, 3, encodePut("k", "a")))
		c.recordApply("n2", entry(7, 3, encodePut("k", "b")))

		if !r.reported("State Machine Safety") {
			t.Errorf("two nodes applying different commands at index 7 were not reported; got: %s", r.joined())
		}
	})
}

func TestInvariantChecker_AllowsIdenticalApplies(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := &recorder{}
		c := newInvariantChecker(r, nil)

		for _, id := range []raft.NodeID{"n1", "n2", "n3"} {
			c.recordApply(id, entry(7, 3, encodePut("k", "a")))
			c.recordApply(id, entry(8, 3, nil))
		}
		if c.violated() {
			t.Errorf("identical applies across nodes were reported as a violation: %s", r.joined())
		}
	})
}

func TestInvariantChecker_DetectsCommitIndexGoingBackwards(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := &recorder{}
		c := newInvariantChecker(r, nil)
		m := c.metrics()

		m.CommitAdvanced("n1", 5)
		m.CommitAdvanced("n1", 9)
		m.CommitAdvanced("n1", 4)

		if !r.reported("Monotonic commit") {
			t.Errorf("a commit index going 9 → 4 was not reported; got: %s", r.joined())
		}
	})
}

func TestInvariantChecker_AllowsCommitIndexResetOnRestart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := &recorder{}
		c := newInvariantChecker(r, nil)
		store := seedStore(t)
		c.addNode("n1", store)
		m := c.metrics()

		m.CommitAdvanced("n1", 9)
		// A restart: commit index is volatile, so coming back at 0 and relearning
		// it from the leader is correct behaviour, not a violation.
		c.detach("n1")
		c.attach("n1", nil)
		m.CommitAdvanced("n1", 1)

		if c.violated() {
			t.Errorf("a commit index reset across a restart was reported as a violation: %s", r.joined())
		}
	})
}

func TestInvariantChecker_DetectsLogMatching(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := &recorder{}
		c := newInvariantChecker(r, nil)

		// The two logs agree on index 3 (term 3), so by Log Matching everything
		// below index 3 must be identical. Index 2 is not.
		c.addNode("n1", seedStore(t,
			entry(1, 1, encodePut("k", "a")),
			entry(2, 2, encodePut("k", "x")),
			entry(3, 3, encodePut("k", "z")),
		))
		c.addNode("n2", seedStore(t,
			entry(1, 1, encodePut("k", "a")),
			entry(2, 2, encodePut("k", "y")),
			entry(3, 3, encodePut("k", "z")),
		))

		c.sweep()

		if !r.reported("Log Matching") {
			t.Errorf("logs agreeing at index 3 but differing at index 2 were not reported; got: %s", r.joined())
		}
		if !strings.Contains(r.joined(), "index 2") {
			t.Errorf("the report does not name the offending index; got: %s", r.joined())
		}
	})
}

func TestInvariantChecker_AllowsDivergentUncommittedTails(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := &recorder{}
		c := newInvariantChecker(r, nil)

		// Same prefix, different tails from different terms. This is what every
		// partition produces and it is entirely legal: the tails are uncommitted
		// and the terms differ, so Log Matching says nothing about them.
		c.addNode("n1", seedStore(t,
			entry(1, 1, encodePut("k", "a")),
			entry(2, 2, encodePut("k", "x")),
		))
		c.addNode("n2", seedStore(t,
			entry(1, 1, encodePut("k", "a")),
			entry(2, 3, encodePut("k", "y")),
		))

		c.sweep()

		if c.violated() {
			t.Errorf("divergent uncommitted tails were reported as a violation: %s", r.joined())
		}
	})
}

func TestInvariantChecker_DetectsLeaderCompleteness(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := &recorder{}
		c := newInvariantChecker(r, nil)

		committedLog := []raft.LogEntry{
			entry(1, 1, encodePut("k", "a")),
			entry(2, 2, encodePut("k", "b")),
		}
		// n1 was leader in term 3 and committed up to index 2.
		c.addNode("n1", seedStore(t, committedLog...))
		// n2 becomes leader in term 5 without index 2.
		c.addNode("n2", seedStore(t, entry(1, 1, encodePut("k", "a"))))

		c.recordCommitted("n1", 3, committedLog, 2)
		c.metrics().StateChange("n2", raft.Candidate, raft.Leader, 5)

		c.sweep()

		if !r.reported("Leader Completeness") {
			t.Errorf("a term-5 leader missing an entry committed in term 3 was not reported; got: %s", r.joined())
		}
	})
}

func TestInvariantChecker_AllowsFigure8Shape(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := &recorder{}
		c := newInvariantChecker(r, nil)

		// The Figure 8 state: the entry at index 2 exists from term 2, is not
		// committed until term 5, and the leader of term 4 does not have it. That
		// is legal, and stating Leader Completeness over the entry's own term
		// rather than over the term it was committed in would wrongly reject it.
		winning := []raft.LogEntry{
			entry(1, 1, encodePut("k", "a")),
			entry(2, 2, encodePut("k", "b")),
		}
		c.addNode("n1", seedStore(t, winning...))
		c.addNode("n2", seedStore(t, entry(1, 1, encodePut("k", "a"))))

		c.metrics().StateChange("n2", raft.Candidate, raft.Leader, 4)
		c.recordCommitted("n1", 5, winning, 2) // committed in term 5, after n2's term

		c.sweep()

		if c.violated() {
			t.Errorf("the Figure 8 state was reported as a Leader Completeness violation: %s", r.joined())
		}
	})
}

func TestInvariantChecker_DetectsConflictingCommits(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := &recorder{}
		c := newInvariantChecker(r, nil)

		first := []raft.LogEntry{entry(1, 1, nil), entry(2, 2, encodePut("k", "b"))}
		second := []raft.LogEntry{entry(1, 1, nil), entry(2, 2, encodePut("k", "c"))}

		c.recordCommitted("n1", 2, first, 2)
		c.recordCommitted("n2", 3, second, 2)

		if !r.reported("State Machine Safety") {
			t.Errorf("two leaders committing different entries at index 2 were not reported; got: %s", r.joined())
		}
	})
}

// ---- Torn reads of a store that is being written to -------------------------
//
// The checker reads the durable log directly, in three calls that are not one
// atomic read, while the node keeps writing. A soak run turned that into 41
// reported Leader Completeness violations against a node that had lost
// nothing: an AppendEntries replacing a conflicting suffix truncated the log
// between the checker's LastIndex and GetLogEntries calls, the store rejected
// the now-overlong range, and the failed read was taken to mean an empty log.

// flakyReads is a store whose entry reads fail for the first n calls, which is
// what a range invalidated by a concurrent truncation looks like from here.
type flakyReads struct {
	raft.Storage
	remaining int
}

func (f *flakyReads) GetLogEntries(ctx context.Context, lo, hi raft.Index) ([]raft.LogEntry, error) {
	if f.remaining > 0 {
		f.remaining--
		return nil, fmt.Errorf("%w: range [%d,%d) not within the log", raft.ErrNotFound, lo, hi)
	}
	return f.Storage.GetLogEntries(ctx, lo, hi)
}

func TestReadLog_RetriesAFailedRead(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		inner := memstore.New()
		log := []raft.LogEntry{entry(1, 1, encodePut("k", "a")), entry(2, 2, encodePut("k", "b"))}
		if err := inner.AppendLogEntries(context.Background(), log); err != nil {
			t.Fatalf("AppendLogEntries: %v", err)
		}
		store := simnet.NewFaultStore(&flakyReads{Storage: inner, remaining: 2})

		entries, _, ok := readLog(store)
		if !ok {
			t.Fatal("readLog gave up on a read that fails twice and then succeeds")
		}
		if len(entries) != len(log) {
			t.Errorf("readLog returned %d entries, want %d", len(entries), len(log))
		}
	})
}

func TestReadLog_ReportsAnUnreadableLog(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		inner := memstore.New()
		if err := inner.AppendLogEntries(context.Background(), []raft.LogEntry{entry(1, 1, nil)}); err != nil {
			t.Fatalf("AppendLogEntries: %v", err)
		}
		store := simnet.NewFaultStore(&flakyReads{Storage: inner, remaining: 1000})

		entries, _, ok := readLog(store)
		if ok {
			t.Errorf("readLog reported success for a log it never managed to read, returning %d entries", len(entries))
		}
	})
}

func TestInvariantChecker_IgnoresAnUnreadableLog(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := &recorder{}
		c := newInvariantChecker(r, nil)

		committedLog := []raft.LogEntry{
			entry(1, 1, encodePut("k", "a")),
			entry(2, 2, encodePut("k", "b")),
		}
		c.addNode("n1", seedStore(t, committedLog...))

		// n2 holds the same log, but every read of it fails. That is not evidence
		// that n2 is missing anything.
		inner := memstore.New()
		if err := inner.AppendLogEntries(context.Background(), committedLog); err != nil {
			t.Fatalf("AppendLogEntries: %v", err)
		}
		c.addNode("n2", simnet.NewFaultStore(&flakyReads{Storage: inner, remaining: 1000}))

		c.recordCommitted("n1", 3, committedLog, 2)
		c.metrics().StateChange("n2", raft.Candidate, raft.Leader, 5)

		c.sweep()

		if c.violated() {
			t.Errorf("a log the checker could not read was reported as a violation: %s", r.joined())
		}
	})
}

func TestDescribesApplied_RejectsALogBehindTheAppliedIndex(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		committedLog := []raft.LogEntry{
			entry(1, 1, encodePut("k", "a")),
			entry(2, 2, encodePut("k", "b")),
		}
		// The store has been truncated to nothing and the replacement entries have
		// not landed yet -- the window between the two writes an AppendEntries
		// carrying a conflicting suffix queues.
		mid := nodeSnapshot{log: nil, snapAt: 0, applied: 2}
		if describesApplied(mid) {
			t.Error("a log holding nothing was accepted as describing 2 applied entries")
		}
		// The same store once the entries are back.
		whole := nodeSnapshot{log: committedLog, snapAt: 0, applied: 2}
		if !describesApplied(whole) {
			t.Error("a log holding both applied entries was rejected")
		}
		// A log compacted into a snapshot describes what it no longer holds.
		compacted := nodeSnapshot{log: nil, snapAt: 2, applied: 2}
		if !describesApplied(compacted) {
			t.Error("a log compacted past the applied index was rejected")
		}
	})
}

func TestInvariantChecker_AllowsACommitIndexReadRacingTheHook(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// The sweep and the node's own CommitAdvanced hook write the same
		// monotonicity baseline. A sweep that reads the commit index and is then
		// overtaken by the hook holds a stale value, and reporting that value as a
		// commit index going backwards is a lie: setCommitIndex only ever moves
		// forward. The two paths have to be ordered against each other, so this
		// runs them against each other on a real node and fails on the report.
		cfg := defaultSimConfig(0x5eed)
		cfg.nodes = 1
		c := newSimCluster(t, &cfg)
		if c.waitLeader(5*time.Second) < 0 {
			t.Fatalf("no leader after startup\n%s", c.diagnostics())
		}
		cn := c.ck.nodeList()[0]

		done := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				default:
					c.ck.readVolatile(cn)
				}
			}
		}()

		for i := range 200 {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			c.put(ctx, "k", fmt.Sprintf("v%d", i))
			cancel()
		}
		close(done)
		wg.Wait()

		if c.ck.violated() {
			t.Error("a commit-index read racing the commit hook was reported as a violation")
		}
	})
}
