package raft_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/brunoga/raft/v2"
	"github.com/brunoga/raft/v2/storage/memstore"
)

// seedRecoveryLog writes a hard state and a run of entries straight into a store, the
// way a node that died would have left them.
func seedRecoveryLog(t *testing.T, store raft.Storage, term raft.Term, n int) {
	t.Helper()
	ctx := context.Background()
	if err := store.SaveHardState(ctx, raft.HardState{CurrentTerm: term}); err != nil {
		t.Fatalf("SaveHardState: %v", err)
	}
	entries := make([]raft.LogEntry, 0, n)
	for i := 1; i <= n; i++ {
		entries = append(entries, raft.LogEntry{
			Index:   raft.Index(i),
			Term:    term,
			Command: fmt.Appendf(nil, "e%d", i),
		})
	}
	if err := store.AppendLogEntries(ctx, entries); err != nil {
		t.Fatalf("AppendLogEntries: %v", err)
	}
}

// TestRecoveryInfo_UncommittedBandIsReported checks that an operator is shown
// which entries recovery is about to guess about, rather than a warning that it
// will guess about some.
//
// The band is the whole of the risk. Told "1..400 are committed, 401..438 are
// not known either way", an operator can look at what is in those 38 entries
// and decide. Told only that recovery "may promote uncommitted entries", they
// can do nothing but hope.
func TestRecoveryInfo_UncommittedBandIsReported(t *testing.T) {
	ctx := context.Background()
	store := memstore.New()
	seedRecoveryLog(t, store, 4, 10)

	info, err := raft.InspectStorage(ctx, store)
	if err != nil {
		t.Fatalf("InspectStorage: %v", err)
	}

	// No snapshot, so storage proves nothing: the whole log is the band.
	if info.KnownCommittedIndex != 0 {
		t.Errorf("KnownCommittedIndex = %d, want 0: without a snapshot nothing is proven committed",
			info.KnownCommittedIndex)
	}
	from, to, ok := info.UncommittedBand()
	if !ok || from != 1 || to != 10 {
		t.Errorf("UncommittedBand() = (%d, %d, %v), want (1, 10, true)", from, to, ok)
	}
}

// TestRecoveryInfo_NoBandWhenTheLogIsFullyProven covers the node whose log ends
// where its snapshot does: everything it holds was applied, so there is nothing
// for recovery to guess about and the operator should be told so plainly.
func TestRecoveryInfo_NoBandWhenTheLogIsFullyProven(t *testing.T) {
	info := raft.RecoveryInfo{
		LastIndex:           40,
		KnownCommittedIndex: 40,
	}
	if _, _, ok := info.UncommittedBand(); ok {
		t.Error("UncommittedBand() reported a band for a log that is entirely proven committed")
	}
}

// TestRecoverCluster_ReportsWhatItPromoted checks that the default mode says
// which entries it turned into committed history.
//
// These are the writes a client may have been told failed. A cluster that comes
// back after a recovery looks like any other cluster, and without a record made
// at the time there is no way, later, to explain a write that reappeared.
func TestRecoverCluster_ReportsWhatItPromoted(t *testing.T) {
	ctx := context.Background()
	store := memstore.New()
	seedRecoveryLog(t, store, 4, 10)

	report, err := raft.RecoverCluster(ctx, store, "n1", []raft.PeerConfig{{ID: "n1", Voter: true}},
		raft.WithKnownCommitted(6))
	if err != nil {
		t.Fatalf("RecoverCluster: %v", err)
	}
	if report.KnownCommittedIndex != 6 {
		t.Errorf("KnownCommittedIndex = %d, want 6", report.KnownCommittedIndex)
	}
	if report.PromotedFrom != 7 || report.PromotedTo != 10 {
		t.Errorf("promoted [%d,%d], want [7,10]: entries above the proven floor become committed",
			report.PromotedFrom, report.PromotedTo)
	}
	if report.DiscardedFrom != 0 || report.DiscardedTo != 0 {
		t.Errorf("discarded [%d,%d], want nothing: the default keeps the whole log",
			report.DiscardedFrom, report.DiscardedTo)
	}
	if report.Index != 11 {
		t.Errorf("recovery entry at %d, want 11 (after the whole log)", report.Index)
	}

	// The log is intact and the recovery entry is on the end.
	last, err := store.LastIndex()
	if err != nil {
		t.Fatalf("LastIndex: %v", err)
	}
	if last != 11 {
		t.Errorf("LastIndex = %d, want 11: nothing should have been removed", last)
	}
}

// TestRecoverCluster_DiscardUncommittedTruncatesToTheProvenFloor checks the
// other side of the trade: nothing that was not proven committed survives.
//
// This is what a system wants when a write it reported as failed must stay
// failed -- when the client has already compensated for it, and having it
// silently take effect is worse than losing a write that did commit.
func TestRecoverCluster_DiscardUncommittedTruncatesToTheProvenFloor(t *testing.T) {
	ctx := context.Background()
	store := memstore.New()
	seedRecoveryLog(t, store, 4, 10)

	report, err := raft.RecoverCluster(ctx, store, "n1", []raft.PeerConfig{{ID: "n1", Voter: true}},
		raft.WithKnownCommitted(6), raft.DiscardUncommitted())
	if err != nil {
		t.Fatalf("RecoverCluster: %v", err)
	}
	if report.DiscardedFrom != 7 || report.DiscardedTo != 10 {
		t.Errorf("discarded [%d,%d], want [7,10]", report.DiscardedFrom, report.DiscardedTo)
	}
	if report.PromotedFrom != 0 || report.PromotedTo != 0 {
		t.Errorf("promoted [%d,%d], want nothing: discarding is the point of the option",
			report.PromotedFrom, report.PromotedTo)
	}
	if report.Index != 7 {
		t.Errorf("recovery entry at %d, want 7 (straight after the proven floor)", report.Index)
	}

	last, err := store.LastIndex()
	if err != nil {
		t.Fatalf("LastIndex: %v", err)
	}
	if last != 7 {
		t.Fatalf("LastIndex = %d, want 7: entries 7..10 should be gone and the recovery entry in their place", last)
	}
	// Everything at or below the floor is untouched.
	for i := raft.Index(1); i <= 6; i++ {
		e, getErr := store.GetLogEntry(ctx, i)
		if getErr != nil {
			t.Fatalf("GetLogEntry(%d): %v", i, getErr)
		}
		if string(e.Command) != fmt.Sprintf("e%d", i) {
			t.Errorf("entry %d = %q, want the committed entry that was there", i, e.Command)
		}
	}
	// And the entry at the floor+1 is the recovery entry, not the old e7.
	e, err := store.GetLogEntry(ctx, 7)
	if err != nil {
		t.Fatalf("GetLogEntry(7): %v", err)
	}
	if string(e.Command) == "e7" {
		t.Error("entry 7 is still the old uncommitted entry; the truncation did not happen")
	}
	if e.Term != report.Term {
		t.Errorf("entry 7 term = %d, want the recovery term %d", e.Term, report.Term)
	}
}

// TestRecoverCluster_RefusesToDiscardEverything checks the guard on a node that
// can prove nothing.
//
// The conservative answer for a log with no snapshot behind it is "keep none of
// it", and that answer is correct and catastrophic. An operator who adds one
// option should not lose their entire log to it without being told that is what
// the option means here.
func TestRecoverCluster_RefusesToDiscardEverything(t *testing.T) {
	ctx := context.Background()
	store := memstore.New()
	seedRecoveryLog(t, store, 4, 10)

	_, err := raft.RecoverCluster(ctx, store, "n1", []raft.PeerConfig{{ID: "n1", Voter: true}},
		raft.DiscardUncommitted())
	if err == nil {
		t.Fatal("RecoverCluster discarded the whole log without a word")
	}
	if !strings.Contains(err.Error(), "WithKnownCommitted") {
		t.Errorf("error = %v, want one naming the option that would make this safe", err)
	}

	// Nothing was written: the refusal has to leave the node recoverable.
	last, err := store.LastIndex()
	if err != nil {
		t.Fatalf("LastIndex: %v", err)
	}
	if last != 10 {
		t.Errorf("LastIndex = %d after a refused recovery, want the 10 entries left alone", last)
	}
	hs, err := store.LoadHardState(ctx)
	if err != nil {
		t.Fatalf("LoadHardState: %v", err)
	}
	if hs.CurrentTerm != 4 {
		t.Errorf("term = %d after a refused recovery, want the original 4", hs.CurrentTerm)
	}
}

// TestRecoverCluster_RejectsAFloorPastTheLog checks that evidence which cannot
// be true is refused. A state machine cannot have applied an entry the log does
// not hold, so a floor above the last index means the state machine and the log
// are from different nodes -- a directory mixed up during a recovery, which is
// exactly when directories get mixed up.
func TestRecoverCluster_RejectsAFloorPastTheLog(t *testing.T) {
	ctx := context.Background()
	store := memstore.New()
	seedRecoveryLog(t, store, 4, 10)

	_, err := raft.RecoverCluster(ctx, store, "n1", []raft.PeerConfig{{ID: "n1", Voter: true}},
		raft.WithKnownCommitted(99))
	if err == nil {
		t.Fatal("RecoverCluster accepted a committed floor past the end of the log")
	}

	last, err := store.LastIndex()
	if err != nil {
		t.Fatalf("LastIndex: %v", err)
	}
	if last != 10 {
		t.Errorf("LastIndex = %d after a refused recovery, want 10", last)
	}
}

// TestRecoverCluster_EvidenceOnlyRaisesTheFloor checks that WithKnownCommitted
// below what storage already proves is ignored rather than lowering the floor.
// A caller passing a stale applied index must not make recovery discard
// entries that a snapshot already proved were committed.
func TestRecoverCluster_EvidenceOnlyRaisesTheFloor(t *testing.T) {
	ctx := context.Background()
	store := memstore.New()
	seedRecoveryLog(t, store, 4, 10)
	// A snapshot at 8 proves 1..8 committed, whatever the caller claims. The
	// payload is opaque state-machine bytes; recovery only reads its metadata.
	if err := store.SaveSnapshot(ctx, raft.SnapshotMeta{LastIncludedIndex: 8, LastIncludedTerm: 4},
		strings.NewReader("state-machine bytes")); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}

	report, err := raft.RecoverCluster(ctx, store, "n1", []raft.PeerConfig{{ID: "n1", Voter: true}},
		raft.WithKnownCommitted(3), raft.DiscardUncommitted())
	if err != nil {
		t.Fatalf("RecoverCluster: %v", err)
	}
	if report.KnownCommittedIndex != 8 {
		t.Errorf("KnownCommittedIndex = %d, want 8: the snapshot proves more than the caller claimed",
			report.KnownCommittedIndex)
	}
	if report.DiscardedFrom != 9 || report.DiscardedTo != 10 {
		t.Errorf("discarded [%d,%d], want [9,10]", report.DiscardedFrom, report.DiscardedTo)
	}
}
