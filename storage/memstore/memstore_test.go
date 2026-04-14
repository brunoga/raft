package memstore_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/brunoga/raft"
	"github.com/brunoga/raft/storage/memstore"
)

func makeEntries(from, to raft.Index, term raft.Term) []raft.LogEntry {
	entries := make([]raft.LogEntry, 0, int(to-from+1))
	for i := from; i <= to; i++ {
		entries = append(entries, raft.LogEntry{Index: i, Term: term, Command: []byte("cmd")})
	}
	return entries
}

func TestHardState(t *testing.T) {
	m := memstore.New()

	ctx := context.Background()
	// Zero value on first load.
	hs, err := m.LoadHardState(ctx)
	if err != nil {
		t.Fatalf("LoadHardState: %v", err)
	}
	if hs.CurrentTerm != 0 || hs.VotedFor != "" {
		t.Fatalf("expected zero HardState, got %+v", hs)
	}

	// Round-trip.
	want := raft.HardState{CurrentTerm: 3, VotedFor: "node2"}
	if err := m.SaveHardState(ctx, want); err != nil {
		t.Fatalf("SaveHardState: %v", err)
	}
	got, err := m.LoadHardState(ctx)
	if err != nil {
		t.Fatalf("LoadHardState: %v", err)
	}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestAppendAndGet(t *testing.T) {
	m := memstore.New()
	ctx := context.Background()

	entries := makeEntries(1, 5, 1)
	if err := m.AppendLogEntries(ctx, entries); err != nil {
		t.Fatalf("AppendLogEntries: %v", err)
	}

	first, _ := m.FirstIndex(ctx)
	last, _ := m.LastIndex(ctx)
	if first != 1 || last != 5 {
		t.Fatalf("first=%d last=%d, want 1,5", first, last)
	}

	for _, e := range entries {
		got, err := m.GetLogEntry(ctx, e.Index)
		if err != nil {
			t.Fatalf("GetLogEntry(%d): %v", e.Index, err)
		}
		if got.Index != e.Index || got.Term != e.Term || !bytes.Equal(got.Command, e.Command) {
			t.Fatalf("got %+v, want %+v", got, e)
		}
	}
}

func TestGetLogEntries_Range(t *testing.T) {
	m := memstore.New()
	ctx := context.Background()
	_ = m.AppendLogEntries(ctx, makeEntries(1, 10, 1))

	got, err := m.GetLogEntries(ctx, 3, 7)
	if err != nil {
		t.Fatalf("GetLogEntries: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("expected 4 entries [3,7), got %d", len(got))
	}
	if got[0].Index != 3 || got[3].Index != 6 {
		t.Fatalf("unexpected range: %v", got)
	}
}

func TestGetLogEntry_OutOfRange(t *testing.T) {
	m := memstore.New()
	ctx := context.Background()
	_ = m.AppendLogEntries(ctx, makeEntries(1, 3, 1))

	if _, err := m.GetLogEntry(ctx, 0); !errors.Is(err, raft.ErrCompacted) {
		t.Fatalf("expected ErrCompacted for index 0, got %v", err)
	}
	if _, err := m.GetLogEntry(ctx, 4); !errors.Is(err, raft.ErrNotFound) {
		t.Fatalf("expected ErrNotFound for index 4, got %v", err)
	}
}

func TestTruncateSuffix(t *testing.T) {
	m := memstore.New()
	ctx := context.Background()
	_ = m.AppendLogEntries(ctx, makeEntries(1, 10, 1))

	if err := m.TruncateSuffix(ctx, 6); err != nil {
		t.Fatalf("TruncateSuffix: %v", err)
	}
	last, _ := m.LastIndex(ctx)
	if last != 5 {
		t.Fatalf("expected last=5 after truncate at 6, got %d", last)
	}
	if _, err := m.GetLogEntry(ctx, 6); !errors.Is(err, raft.ErrNotFound) {
		t.Fatalf("expected ErrNotFound for truncated entry, got %v", err)
	}
}

func TestTruncateSuffix_All(t *testing.T) {
	m := memstore.New()
	ctx := context.Background()
	_ = m.AppendLogEntries(ctx, makeEntries(1, 5, 1))
	if err := m.TruncateSuffix(ctx, 1); err != nil {
		t.Fatalf("TruncateSuffix: %v", err)
	}
	last, _ := m.LastIndex(ctx)
	first, _ := m.FirstIndex(ctx)
	if first != 0 || last != 0 {
		t.Fatalf("expected empty log (first=0, last=0), got first=%d last=%d", first, last)
	}
}

func TestTruncatePrefix(t *testing.T) {
	m := memstore.New()
	ctx := context.Background()
	_ = m.AppendLogEntries(ctx, makeEntries(1, 10, 1))

	if err := m.TruncatePrefix(ctx, 4); err != nil {
		t.Fatalf("TruncatePrefix: %v", err)
	}
	first, _ := m.FirstIndex(ctx)
	if first != 4 {
		t.Fatalf("expected firstIndex=4, got %d", first)
	}
	if _, err := m.GetLogEntry(ctx, 3); !errors.Is(err, raft.ErrCompacted) {
		t.Fatalf("expected ErrCompacted for compacted entry, got %v", err)
	}
	if _, err := m.GetLogEntry(ctx, 4); err != nil {
		t.Fatalf("GetLogEntry(4) after prefix truncate: %v", err)
	}
}

func TestTruncatePrefix_All(t *testing.T) {
	m := memstore.New()
	ctx := context.Background()
	_ = m.AppendLogEntries(ctx, makeEntries(1, 5, 1))
	if err := m.TruncatePrefix(ctx, 6); err != nil {
		t.Fatalf("TruncatePrefix: %v", err)
	}
	first, _ := m.FirstIndex(ctx)
	last, _ := m.LastIndex(ctx)
	if first != 0 || last != 0 {
		t.Fatalf("expected empty log, got first=%d last=%d", first, last)
	}
}

func TestSnapshot(t *testing.T) {
	m := memstore.New()
	ctx := context.Background()

	// No snapshot yet.
	if _, _, err := m.LoadSnapshot(ctx); !errors.Is(err, raft.ErrNoSnapshot) {
		t.Fatalf("expected ErrNoSnapshot, got %v", err)
	}

	meta := raft.SnapshotMeta{LastIncludedIndex: 100, LastIncludedTerm: 5}
	data := []byte("snapshot-data")
	if err := m.SaveSnapshot(ctx, meta, data); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}

	gotMeta, gotData, err := m.LoadSnapshot(ctx)
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	if gotMeta != meta {
		t.Fatalf("meta mismatch: got %+v, want %+v", gotMeta, meta)
	}
	if string(gotData) != string(data) {
		t.Fatalf("data mismatch: got %q, want %q", gotData, data)
	}
}

func TestSnapshot_DataIsolated(t *testing.T) {
	// Mutating the original slice after Save must not affect the stored copy.
	m := memstore.New()
	ctx := context.Background()
	data := []byte("original")
	_ = m.SaveSnapshot(ctx, raft.SnapshotMeta{LastIncludedIndex: 1}, data)
	data[0] = 'X'

	_, got, _ := m.LoadSnapshot(ctx)
	if got[0] != 'o' {
		t.Fatalf("SaveSnapshot should copy data; got %q", got)
	}

	// Mutating the loaded slice must not affect stored copy.
	got[0] = 'Y'
	_, got2, _ := m.LoadSnapshot(ctx)
	if got2[0] != 'o' {
		t.Fatalf("LoadSnapshot should return a copy; got %q", got2)
	}
}

func TestEmptyLog(t *testing.T) {
	m := memstore.New()
	ctx := context.Background()
	first, err := m.FirstIndex(ctx)
	if err != nil || first != 0 {
		t.Fatalf("empty FirstIndex: got %d, %v", first, err)
	}
	last, err := m.LastIndex(ctx)
	if err != nil || last != 0 {
		t.Fatalf("empty LastIndex: got %d, %v", last, err)
	}
}

func TestAppendEmpty(t *testing.T) {
	m := memstore.New()
	ctx := context.Background()
	if err := m.AppendLogEntries(ctx, nil); err != nil {
		t.Fatalf("AppendLogEntries(nil): %v", err)
	}
	last, _ := m.LastIndex(ctx)
	if last != 0 {
		t.Fatalf("expected last=0 after appending nothing, got %d", last)
	}
}

// Verify MemStore satisfies the raft.Storage interface at compile time.
var _ raft.Storage = (*memstore.MemStore)(nil)
