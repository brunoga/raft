package memstore_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/brunoga/raft/v2"
	"github.com/brunoga/raft/v2/storage/memstore"
)

// MemStore is the reference implementation the rest of the test suite runs
// against, so it has to model the storage contract faithfully rather than
// merely well enough to pass. A backend that serialises entries to disk hands
// out fresh memory on every read and keeps nothing of what the caller passed
// in; one that hands back its own slices lets a caller quietly corrupt the log
// and lets bugs hide in tests that a real backend would catch.

func commandFor(i raft.Index) []byte {
	return []byte{byte(i), 'c', 'm', 'd'}
}

func entriesWithCommands(from, to raft.Index, term raft.Term) []raft.LogEntry {
	entries := make([]raft.LogEntry, 0, int(to-from+1))
	for i := from; i <= to; i++ {
		entries = append(entries, raft.LogEntry{Index: i, Term: term, Command: commandFor(i)})
	}
	return entries
}

// TestAppendLogEntries_CopiesCallerBuffers checks that the store does not keep
// a reference to the caller's command buffers, which callers are free to reuse.
func TestAppendLogEntries_CopiesCallerBuffers(t *testing.T) {
	m := memstore.New()
	ctx := context.Background()

	entries := entriesWithCommands(1, 5, 1)
	if err := m.AppendLogEntries(ctx, entries); err != nil {
		t.Fatalf("AppendLogEntries: %v", err)
	}

	// Scribble over everything the caller handed in.
	for i := range entries {
		for j := range entries[i].Command {
			entries[i].Command[j] = 0xFF
		}
	}

	for i := raft.Index(1); i <= 5; i++ {
		got, err := m.GetLogEntry(ctx, i)
		if err != nil {
			t.Fatalf("GetLogEntry(%d): %v", i, err)
		}
		if !bytes.Equal(got.Command, commandFor(i)) {
			t.Fatalf("entry %d was changed by the caller reusing its buffer: got %v, want %v",
				i, got.Command, commandFor(i))
		}
	}
}

// TestGetLogEntry_ReturnsAnIndependentCopy checks that a caller cannot reach
// into the stored log through an entry it was handed.
func TestGetLogEntry_ReturnsAnIndependentCopy(t *testing.T) {
	m := memstore.New()
	ctx := context.Background()

	if err := m.AppendLogEntries(ctx, entriesWithCommands(1, 3, 1)); err != nil {
		t.Fatalf("AppendLogEntries: %v", err)
	}

	got, err := m.GetLogEntry(ctx, 2)
	if err != nil {
		t.Fatalf("GetLogEntry(2): %v", err)
	}
	for j := range got.Command {
		got.Command[j] = 0xFF
	}

	again, err := m.GetLogEntry(ctx, 2)
	if err != nil {
		t.Fatalf("GetLogEntry(2): %v", err)
	}
	if !bytes.Equal(again.Command, commandFor(2)) {
		t.Fatalf("the stored entry was changed through a returned command slice: got %v, want %v",
			again.Command, commandFor(2))
	}
}

// TestGetLogEntries_ReturnIndependentCopies is the range-read counterpart.
func TestGetLogEntries_ReturnIndependentCopies(t *testing.T) {
	m := memstore.New()
	ctx := context.Background()

	if err := m.AppendLogEntries(ctx, entriesWithCommands(1, 6, 1)); err != nil {
		t.Fatalf("AppendLogEntries: %v", err)
	}

	got, err := m.GetLogEntries(ctx, 2, 5)
	if err != nil {
		t.Fatalf("GetLogEntries: %v", err)
	}
	for i := range got {
		for j := range got[i].Command {
			got[i].Command[j] = 0xFF
		}
	}

	for i := raft.Index(2); i < 5; i++ {
		again, err := m.GetLogEntry(ctx, i)
		if err != nil {
			t.Fatalf("GetLogEntry(%d): %v", i, err)
		}
		if !bytes.Equal(again.Command, commandFor(i)) {
			t.Fatalf("entry %d was changed through a returned command slice: got %v, want %v",
				i, again.Command, commandFor(i))
		}
	}
}

// TestSnapshot_LoadReturnsAnIndependentCopy checks the same ownership rule for
// snapshot data.
func TestSnapshot_LoadReturnsAnIndependentCopy(t *testing.T) {
	m := memstore.New()
	ctx := context.Background()

	body := []byte("snapshot-body")
	meta := raft.SnapshotMeta{LastIncludedIndex: 9, LastIncludedTerm: 2}
	if err := m.SaveSnapshot(ctx, meta, bytes.NewReader(body)); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}

	_, rc, err := m.LoadSnapshot(ctx)
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	_ = rc.Close()
	for j := range got {
		got[j] = 0xFF
	}

	_, rc2, err := m.LoadSnapshot(ctx)
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	again, err := io.ReadAll(rc2)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	_ = rc2.Close()
	if !bytes.Equal(again, body) {
		t.Fatalf("the stored snapshot was changed through a returned reader: got %q, want %q",
			again, body)
	}
}

// TestTruncateSuffix_BeforeFirstIndexIsCompacted pins the error the file-backed
// backend returns for the same call, so that the two do not disagree about a
// case the engine has to distinguish.
func TestTruncateSuffix_BeforeFirstIndexIsCompacted(t *testing.T) {
	m := memstore.New()
	ctx := context.Background()

	if err := m.AppendLogEntries(ctx, entriesWithCommands(1, 10, 1)); err != nil {
		t.Fatalf("AppendLogEntries: %v", err)
	}
	if err := m.TruncatePrefix(ctx, 5); err != nil {
		t.Fatalf("TruncatePrefix: %v", err)
	}

	if err := m.TruncateSuffix(ctx, 3); !errors.Is(err, raft.ErrCompacted) {
		t.Fatalf("TruncateSuffix below the first index = %v, want ErrCompacted", err)
	}

	// The rejected call must not have changed anything.
	first, err := m.FirstIndex()
	if err != nil {
		t.Fatalf("FirstIndex: %v", err)
	}
	last, err := m.LastIndex()
	if err != nil {
		t.Fatalf("LastIndex: %v", err)
	}
	if first != 5 || last != 10 {
		t.Fatalf("log bounds are [%d,%d], want [5,10]", first, last)
	}

	// Truncating exactly at the first index is still a whole-log wipe.
	if err = m.TruncateSuffix(ctx, 5); err != nil {
		t.Fatalf("TruncateSuffix at the first index: %v", err)
	}
	if first, _ = m.FirstIndex(); first != 0 {
		t.Fatalf("FirstIndex after wiping the log = %d, want 0", first)
	}
}

// TestReadErrorsAreNotFound checks the error the contract names for reads
// outside the log, including after compaction.
func TestReadErrorsAreNotFound(t *testing.T) {
	m := memstore.New()
	ctx := context.Background()

	if _, err := m.GetLogEntry(ctx, 1); !errors.Is(err, raft.ErrNotFound) {
		t.Errorf("GetLogEntry on an empty log = %v, want ErrNotFound", err)
	}
	if _, err := m.GetLogEntries(ctx, 1, 3); !errors.Is(err, raft.ErrNotFound) {
		t.Errorf("GetLogEntries on an empty log = %v, want ErrNotFound", err)
	}

	if err := m.AppendLogEntries(ctx, entriesWithCommands(1, 10, 1)); err != nil {
		t.Fatalf("AppendLogEntries: %v", err)
	}
	if err := m.TruncatePrefix(ctx, 5); err != nil {
		t.Fatalf("TruncatePrefix: %v", err)
	}

	for _, index := range []raft.Index{1, 4, 11} {
		if _, err := m.GetLogEntry(ctx, index); !errors.Is(err, raft.ErrNotFound) {
			t.Errorf("GetLogEntry(%d) = %v, want ErrNotFound", index, err)
		}
	}
	if _, err := m.GetLogEntries(ctx, 4, 8); !errors.Is(err, raft.ErrNotFound) {
		t.Errorf("GetLogEntries spanning compacted entries = %v, want ErrNotFound", err)
	}
	if _, err := m.GetLogEntries(ctx, 8, 12); !errors.Is(err, raft.ErrNotFound) {
		t.Errorf("GetLogEntries past the end = %v, want ErrNotFound", err)
	}

	// An empty range is not an error, matching the file-backed backend.
	if got, err := m.GetLogEntries(ctx, 6, 6); err != nil || got != nil {
		t.Errorf("GetLogEntries on an empty range = %v, %v, want nil, nil", got, err)
	}
}
