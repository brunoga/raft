package raft_test

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/brunoga/raft/v2"
	"github.com/brunoga/raft/v2/storage/filestore"
	"github.com/brunoga/raft/v2/storage/memstore"
	"github.com/brunoga/raft/v2/transport/memtransport"
)

// TestCommitFloor_NarrowsTheBandARecoveryGuessesAbout is what the recorded
// commit index is for.
//
// Without it the only proof of commitment left on a survivor's disk is its
// snapshot, so everything since the last compaction is a coin toss: with a
// default SnapshotThreshold that is thousands of entries an operator has to
// decide about blind. With it, the band is what the disk had not caught up
// with, which is close to nothing.
func TestCommitFloor_NarrowsTheBandARecoveryGuessesAbout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		dir := t.TempDir()
		store, err := filestore.Open(filepath.Join(dir, "n1"))
		if err != nil {
			t.Fatalf("filestore.Open: %v", err)
		}

		net := memtransport.NewNetwork()
		cfg := raft.DefaultConfig()
		cfg.ID = "n1"
		cfg.Storage = store
		cfg.StateMachine = &recoverySM{}
		cfg.Transport = net.NewTransport("n1")
		cfg.TickInterval = 0
		cfg.SnapshotThreshold = 0 // never compact: the snapshot must prove nothing
		// This test drives ticks itself, once a millisecond, against timeouts that
		// were converted to tick counts at a 10ms tick. Left at 20-40ms the
		// election window is 2-4ms of wall clock, and this is the one test here
		// that also does real disk I/O: on a loaded machine it starts elections
		// faster than the fsyncs behind them complete, and never settles on a
		// leader. Squeezing the window further fails it every time.
		tuneForManualTicks(&cfg)

		node, err := raft.New(&cfg)
		if err != nil {
			t.Fatalf("raft.New: %v", err)
		}
		net.Register("n1", node.Handler())
		node.Start()
		if !tickUntil(node, 5*time.Second, func() bool { return node.State() == raft.Leader }) {
			t.Fatal("node never became leader")
		}

		// Enough entries that the commit index passes the recording interval more
		// than once.
		const entries = 3000
		for i := range entries {
			if _, pErr := node.Propose(ctx, fmt.Appendf(nil, "e%d", i)); pErr != nil {
				t.Fatalf("propose %d: %v", i, pErr)
			}
		}
		node.Stop()
		if cErr := store.Close(); cErr != nil {
			t.Fatalf("store.Close: %v", cErr)
		}

		// The node is gone; an operator inspects what it left behind.
		reopened, err := filestore.Open(filepath.Join(dir, "n1"))
		if err != nil {
			t.Fatalf("reopen: %v", err)
		}
		defer func() { _ = reopened.Close() }()

		info, err := raft.InspectStorage(ctx, reopened)
		if err != nil {
			t.Fatalf("InspectStorage: %v", err)
		}
		if info.SnapshotIndex != 0 {
			t.Fatalf("SnapshotIndex = %d, want 0: this test is about what the snapshot cannot prove",
				info.SnapshotIndex)
		}
		if info.KnownCommittedIndex == 0 {
			t.Fatal("KnownCommittedIndex = 0: with no snapshot and no recorded commit index, " +
				"every one of the 3000 entries would be a guess")
		}
		if info.KnownCommittedIndex > info.LastIndex {
			t.Fatalf("KnownCommittedIndex = %d is past LastIndex %d: a floor must be a floor",
				info.KnownCommittedIndex, info.LastIndex)
		}

		// The band left over is bounded by how often the index is written down,
		// plus whatever had not reached the disk when the node stopped. It must be
		// a small fraction of the log rather than all of it.
		from, to, ok := info.UncommittedBand()
		if !ok {
			return // nothing in doubt at all
		}
		// The band is bounded by how often the index is written down, plus
		// whatever had not reached the disk when the node stopped. What matters is
		// that it does not grow with the log: 3000 entries and 300000 leave the
		// same handful in doubt.
		const recordInterval = 256
		band := to - from + 1
		if band > 2*recordInterval {
			t.Errorf("band is %d entries (indices %d..%d) out of a log of %d; it should be bounded "+
				"by the recording interval of %d, not by the length of the log",
				band, from, to, info.LastIndex, recordInterval)
		}
		t.Logf("log of %d entries, %d proven committed, %d in doubt", info.LastIndex, info.KnownCommittedIndex, band)
	})
}

// TestCommitFloor_NeverClaimsMoreThanTheLogHolds checks the one way a recorded
// floor could do harm.
//
// A floor is a promise that everything at or below it committed. If a stale or
// corrupt record read back above the end of the log, DiscardUncommitted would
// truncate to an index that does not exist and the report would name entries
// nobody has. Clamping to the log makes the worst case a floor that is merely
// too low, which costs nothing but a wider band.
func TestCommitFloor_NeverClaimsMoreThanTheLogHolds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		store := memstore.New()
		seedRecoveryLog(t, store, 4, 10)

		// A record from before a truncation: larger than anything in the log.
		if err := store.SaveCommitIndex(ctx, 500); err != nil {
			t.Fatalf("SaveCommitIndex: %v", err)
		}

		info, err := raft.InspectStorage(ctx, store)
		if err != nil {
			t.Fatalf("InspectStorage: %v", err)
		}
		if info.KnownCommittedIndex != 10 {
			t.Errorf("KnownCommittedIndex = %d, want it clamped to LastIndex 10", info.KnownCommittedIndex)
		}
		if _, _, ok := info.UncommittedBand(); ok {
			t.Error("UncommittedBand() reported a band above a floor that is already the end of the log")
		}
	})
}

// TestCommitFloor_RestartDoesNotLoseGround checks that a node which restarts
// and starts committing again from a low index does not overwrite a higher
// recorded value with a lower one.
//
// The record is proof, and proof does not expire. A restarted node's commit
// index begins at zero and climbs; if it wrote that down it would erase what
// the previous run had established and hand a later recovery a worse answer
// than it had before the restart.
func TestCommitFloor_RestartDoesNotLoseGround(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		store := memstore.New()
		seedRecoveryLog(t, store, 4, 10)
		if err := store.SaveCommitIndex(ctx, 9); err != nil {
			t.Fatalf("SaveCommitIndex: %v", err)
		}

		net := memtransport.NewNetwork()
		sm := &recoverySM{}
		node := recoveryNode(t, net, store, sm, "n1", nil,
			func(cfg *raft.Config) { cfg.SnapshotThreshold = 0 })
		if !tickUntil(node, 5*time.Second, func() bool { return node.State() == raft.Leader }) {
			t.Fatal("node never became leader")
		}
		if _, err := node.Propose(ctx, []byte("after-restart")); err != nil {
			t.Fatalf("propose: %v", err)
		}
		node.Stop()

		got, err := store.LoadCommitIndex(ctx)
		if err != nil {
			t.Fatalf("LoadCommitIndex: %v", err)
		}
		if got < 9 {
			t.Errorf("recorded commit index = %d after a restart, want at least the 9 that was "+
				"already proven; a restart must not erase what an earlier run established", got)
		}
	})
}

// TestCommitFloor_NothingIsRecordedAheadOfTheDisk is the invariant the recorded
// index rests on.
//
// A follower's commit index runs ahead of its own writes: an entry is in the
// log the moment it arrives, the leader's commit point comes with it, and the
// write is still queued. A recorded index above what the disk holds would
// claim, to a recovery that happens after a crash, that entries the disk never
// received had committed -- and a recovery believing it would truncate past
// the end of the surviving log, or report as proven entries nobody has.
//
// Two things prevent it, and this exercises both together because from outside
// they are one behaviour: the value is clamped to the log's durable point, and
// the operation carrying it is queued behind the writes it vouches for, so it
// cannot reach storage before they do.
func TestCommitFloor_NothingIsRecordedAheadOfTheDisk(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		node, store := newGatedFollower(t, nil)

		release := store.hold(t)
		defer release()

		// Well past the recording interval, all of it committed as far as this
		// follower is concerned, and not one entry of it on disk.
		const entries = 1000
		go func() {
			_, _ = node.Handler().HandleAppendEntries(ctx, appendFrom(1, 1, entries, entries))
		}()
		store.awaitHeld(t)

		deadline := time.Now().Add(5 * time.Second)
		for node.CommitIndex() < entries && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		if node.CommitIndex() != entries {
			t.Fatalf("commit index = %d, want %d: the test needs a follower that has committed "+
				"past its own disk", node.CommitIndex(), entries)
		}

		recorded, err := store.LoadCommitIndex(ctx)
		if err != nil {
			t.Fatalf("LoadCommitIndex: %v", err)
		}
		if recorded != 0 {
			t.Errorf("recorded commit index = %d while nothing has reached the disk; a recorded "+
				"index must be one this node could prove from its own storage", recorded)
		}

		// Once the write lands, the same index is worth recording.
		release()
		deadline = time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			recorded, err = store.LoadCommitIndex(ctx)
			if err != nil {
				t.Fatalf("LoadCommitIndex: %v", err)
			}
			if recorded > 0 {
				break
			}
			node.Tick()
			time.Sleep(time.Millisecond)
		}
		if recorded == 0 {
			t.Error("nothing was recorded after the write landed; the index is only useful if it " +
				"catches up once the disk does")
		}
		if recorded > entries {
			t.Errorf("recorded %d, which is past the %d entries the log holds", recorded, entries)
		}
	})
}
