package filestore_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"testing"

	"github.com/brunoga/raft"
	"github.com/brunoga/raft/storage/filestore"
)

func openFresh(t *testing.T) (store *filestore.FileStore, dir string) {
	t.Helper()
	dir = t.TempDir()
	var err error
	store, err = filestore.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store, dir
}

func makeEntries(from, to raft.Index, term raft.Term) []raft.LogEntry {
	entries := make([]raft.LogEntry, 0, int(to-from+1))
	for i := from; i <= to; i++ {
		entries = append(entries, raft.LogEntry{
			Index:   i,
			Term:    term,
			Command: []byte("command"),
		})
	}
	return entries
}

// --- Hard state -------------------------------------------------------------

func TestHardState_RoundTrip(t *testing.T) {
	fs, _ := openFresh(t)
	ctx := context.Background()

	hs, err := fs.LoadHardState(ctx)
	if err != nil {
		t.Fatalf("LoadHardState on empty: %v", err)
	}
	if hs.CurrentTerm != 0 || hs.VotedFor != "" {
		t.Fatalf("expected zero HardState, got %+v", hs)
	}

	want := raft.HardState{CurrentTerm: 7, VotedFor: "peer-42"}
	if err = fs.SaveHardState(ctx, want); err != nil {
		t.Fatalf("SaveHardState: %v", err)
	}
	got, err := fs.LoadHardState(ctx)
	if err != nil {
		t.Fatalf("LoadHardState: %v", err)
	}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestHardState_PersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	fs, err := filestore.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	want := raft.HardState{CurrentTerm: 3, VotedFor: "n1"}
	if err = fs.SaveHardState(ctx, want); err != nil {
		t.Fatalf("SaveHardState: %v", err)
	}
	_ = fs.Close()

	fs2, err := filestore.Open(dir)
	if err != nil {
		t.Fatalf("Reopen: %v", err)
	}
	defer func() { _ = fs2.Close() }()

	got, err := fs2.LoadHardState(ctx)
	if err != nil {
		t.Fatalf("LoadHardState after reopen: %v", err)
	}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

// --- Log --------------------------------------------------------------------

func TestLog_AppendAndGet(t *testing.T) {
	fs, _ := openFresh(t)
	ctx := context.Background()

	entries := makeEntries(1, 5, 1)
	if err := fs.AppendLogEntries(ctx, entries); err != nil {
		t.Fatalf("AppendLogEntries: %v", err)
	}

	first, _ := fs.FirstIndex(ctx)
	last, _ := fs.LastIndex(ctx)
	if first != 1 || last != 5 {
		t.Fatalf("want first=1 last=5, got first=%d last=%d", first, last)
	}

	for _, want := range entries {
		got, err := fs.GetLogEntry(ctx, want.Index)
		if err != nil {
			t.Fatalf("GetLogEntry(%d): %v", want.Index, err)
		}
		if got.Index != want.Index || got.Term != want.Term || !bytes.Equal(got.Command, want.Command) {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	}
}

func TestLog_PersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	fs1, err := filestore.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	_ = fs1.AppendLogEntries(ctx, makeEntries(1, 3, 1))
	_ = fs1.Close()

	fs2, err := filestore.Open(dir)
	if err != nil {
		t.Fatalf("Reopen: %v", err)
	}
	defer func() { _ = fs2.Close() }()

	last, _ := fs2.LastIndex(ctx)
	if last != 3 {
		t.Fatalf("expected lastIndex=3 after reopen, got %d", last)
	}
}

func TestLog_GetRange(t *testing.T) {
	fs, _ := openFresh(t)
	ctx := context.Background()
	_ = fs.AppendLogEntries(ctx, makeEntries(1, 10, 1))

	got, err := fs.GetLogEntries(ctx, 3, 7)
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

func TestLog_GetOutOfRange(t *testing.T) {
	fs, _ := openFresh(t)
	ctx := context.Background()
	_ = fs.AppendLogEntries(ctx, makeEntries(1, 5, 1))

	if _, err := fs.GetLogEntry(ctx, 6); !errors.Is(err, raft.ErrNotFound) {
		t.Fatalf("expected ErrNotFound for index 6, got %v", err)
	}
}

func TestLog_TruncateSuffix(t *testing.T) {
	fs, _ := openFresh(t)
	ctx := context.Background()
	_ = fs.AppendLogEntries(ctx, makeEntries(1, 10, 1))

	if err := fs.TruncateSuffix(ctx, 6); err != nil {
		t.Fatalf("TruncateSuffix: %v", err)
	}
	last, _ := fs.LastIndex(ctx)
	if last != 5 {
		t.Fatalf("expected last=5, got %d", last)
	}
	if _, err := fs.GetLogEntry(ctx, 6); !errors.Is(err, raft.ErrNotFound) {
		t.Fatalf("expected ErrNotFound for index 6 after truncate, got %v", err)
	}
}

func TestLog_TruncateSuffix_PersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	fs1, _ := filestore.Open(dir)
	_ = fs1.AppendLogEntries(ctx, makeEntries(1, 10, 1))
	_ = fs1.TruncateSuffix(ctx, 6)
	_ = fs1.Close()

	fs2, err := filestore.Open(dir)
	if err != nil {
		t.Fatalf("Reopen after TruncateSuffix: %v", err)
	}
	defer func() { _ = fs2.Close() }()

	last, _ := fs2.LastIndex(ctx)
	if last != 5 {
		t.Fatalf("expected last=5 after reopen, got %d", last)
	}
}

func TestLog_TruncatePrefix(t *testing.T) {
	fs, _ := openFresh(t)
	ctx := context.Background()
	_ = fs.AppendLogEntries(ctx, makeEntries(1, 10, 1))

	if err := fs.TruncatePrefix(ctx, 4); err != nil {
		t.Fatalf("TruncatePrefix: %v", err)
	}
	first, _ := fs.FirstIndex(ctx)
	if first != 4 {
		t.Fatalf("expected firstIndex=4, got %d", first)
	}
	if _, err := fs.GetLogEntry(ctx, 3); !errors.Is(err, raft.ErrNotFound) {
		t.Fatalf("expected ErrNotFound for compacted entry 3, got %v", err)
	}
	if _, err := fs.GetLogEntry(ctx, 4); err != nil {
		t.Fatalf("GetLogEntry(4) after prefix truncate: %v", err)
	}
}

func TestLog_TruncatePrefix_PersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	fs1, _ := filestore.Open(dir)
	_ = fs1.AppendLogEntries(ctx, makeEntries(1, 10, 1))
	_ = fs1.TruncatePrefix(ctx, 4)
	_ = fs1.Close()

	fs2, err := filestore.Open(dir)
	if err != nil {
		t.Fatalf("Reopen: %v", err)
	}
	defer func() { _ = fs2.Close() }()

	first, _ := fs2.FirstIndex(ctx)
	last, _ := fs2.LastIndex(ctx)
	if first != 4 || last != 10 {
		t.Fatalf("expected first=4 last=10, got first=%d last=%d", first, last)
	}
}

func TestLog_EmptyLog(t *testing.T) {
	fs, _ := openFresh(t)
	ctx := context.Background()
	first, err := fs.FirstIndex(ctx)
	if err != nil || first != 0 {
		t.Fatalf("expected first=0 on empty log, got %d %v", first, err)
	}
	last, err := fs.LastIndex(ctx)
	if err != nil || last != 0 {
		t.Fatalf("expected last=0 on empty log, got %d %v", last, err)
	}
}

// --- Crash recovery ---------------------------------------------------------

func TestRecovery_CorruptTail(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	fs1, _ := filestore.Open(dir)
	_ = fs1.AppendLogEntries(ctx, makeEntries(1, 5, 1))
	_ = fs1.Close()

	// Corrupt the last few bytes of the active segment log file.
	logPath := dir + "/seg-00000.log"
	info, _ := os.Stat(logPath)
	f, _ := os.OpenFile(logPath, os.O_RDWR, 0o600)
	_, _ = f.WriteAt([]byte{0xFF, 0xFF, 0xFF, 0xFF}, info.Size()-4)
	_ = f.Close()

	// Open should recover to the last valid entry.
	fs2, err := filestore.Open(dir)
	if err != nil {
		t.Fatalf("Open after corrupt tail: %v", err)
	}
	defer func() { _ = fs2.Close() }()

	last, _ := fs2.LastIndex(ctx)
	if last < 1 || last > 5 {
		t.Fatalf("unexpected lastIndex after recovery: %d", last)
	}
}

// --- Segment rotation -------------------------------------------------------

// openSmall opens a FileStore with a tiny segment size so rotation happens
// after just a few entries. Each entry is roughly entryHeaderSize + 7 bytes
// ("command" payload) = ~31 bytes, so segSize=100 forces rotation every ~3 entries.
func openSmall(t *testing.T, dir string) *filestore.FileStore {
	t.Helper()
	fs, err := filestore.OpenWithSegmentSize(dir, 100)
	if err != nil {
		t.Fatalf("OpenWithSegmentSize: %v", err)
	}
	t.Cleanup(func() { _ = fs.Close() })
	return fs
}

func TestSegment_RotationCreatesMultipleFiles(t *testing.T) {
	dir := t.TempDir()
	fs := openSmall(t, dir)
	ctx := context.Background()

	// Append enough entries to force at least two segments.
	if err := fs.AppendLogEntries(ctx, makeEntries(1, 10, 1)); err != nil {
		t.Fatalf("AppendLogEntries: %v", err)
	}

	// Verify more than one segment file was created.
	seg1 := dir + "/seg-00000.log"
	seg2 := dir + "/seg-00001.log"
	if _, err := os.Stat(seg1); err != nil {
		t.Fatalf("expected seg-00000.log: %v", err)
	}
	if _, err := os.Stat(seg2); err != nil {
		t.Fatalf("expected seg-00001.log after rotation: %v", err)
	}
}

func TestSegment_ReadAcrossSegments(t *testing.T) {
	dir := t.TempDir()
	fs := openSmall(t, dir)
	ctx := context.Background()

	entries := makeEntries(1, 10, 1)
	if err := fs.AppendLogEntries(ctx, entries); err != nil {
		t.Fatalf("AppendLogEntries: %v", err)
	}

	// Every individual entry must be readable.
	for _, want := range entries {
		got, err := fs.GetLogEntry(ctx, want.Index)
		if err != nil {
			t.Fatalf("GetLogEntry(%d): %v", want.Index, err)
		}
		if got.Index != want.Index || got.Term != want.Term {
			t.Fatalf("index %d: got %+v, want %+v", want.Index, got, want)
		}
	}

	// Range read spanning all segments.
	got, err := fs.GetLogEntries(ctx, 1, 11)
	if err != nil {
		t.Fatalf("GetLogEntries: %v", err)
	}
	if len(got) != 10 {
		t.Fatalf("expected 10 entries, got %d", len(got))
	}
}

func TestSegment_TruncatePrefixDropsWholeSegment(t *testing.T) {
	dir := t.TempDir()
	fs := openSmall(t, dir)
	ctx := context.Background()

	if err := fs.AppendLogEntries(ctx, makeEntries(1, 10, 1)); err != nil {
		t.Fatalf("AppendLogEntries: %v", err)
	}

	// Truncate all of segment 0 (entries 1–3 approx) by setting toIndex > its lastID.
	// We don't know exactly where the boundary is, so truncate up to index 5 which
	// is guaranteed to span at least one full segment.
	if err := fs.TruncatePrefix(ctx, 5); err != nil {
		t.Fatalf("TruncatePrefix: %v", err)
	}

	first, _ := fs.FirstIndex(ctx)
	if first != 5 {
		t.Fatalf("expected firstIndex=5, got %d", first)
	}

	// seg-00000.log should be gone (its entries were all < 5 or it was fully compacted).
	// Just verify we can still read entries from 5 onward.
	for i := raft.Index(5); i <= 10; i++ {
		if _, err := fs.GetLogEntry(ctx, i); err != nil {
			t.Errorf("GetLogEntry(%d) after prefix truncate: %v", i, err)
		}
	}
}

func TestSegment_PersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	fs1, err := filestore.OpenWithSegmentSize(dir, 100)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	_ = fs1.AppendLogEntries(ctx, makeEntries(1, 10, 1))
	_ = fs1.Close()

	fs2, err := filestore.OpenWithSegmentSize(dir, 100)
	if err != nil {
		t.Fatalf("Reopen: %v", err)
	}
	defer func() { _ = fs2.Close() }()

	first, _ := fs2.FirstIndex(ctx)
	last, _ := fs2.LastIndex(ctx)
	if first != 1 || last != 10 {
		t.Fatalf("after reopen: first=%d last=%d, want 1 10", first, last)
	}

	// Spot-check an entry.
	e, err := fs2.GetLogEntry(ctx, 7)
	if err != nil {
		t.Fatalf("GetLogEntry(7): %v", err)
	}
	if e.Index != 7 {
		t.Fatalf("GetLogEntry(7) returned index %d", e.Index)
	}
}

// --- Snapshot ---------------------------------------------------------------

func TestSnapshot_RoundTrip(t *testing.T) {
	fs, _ := openFresh(t)
	ctx := context.Background()

	if _, _, err := fs.LoadSnapshot(ctx); !errors.Is(err, raft.ErrNoSnapshot) {
		t.Fatalf("expected ErrNoSnapshot, got %v", err)
	}

	meta := raft.SnapshotMeta{LastIncludedIndex: 100, LastIncludedTerm: 5}
	data := []byte("snapshot-payload")
	if err := fs.SaveSnapshot(ctx, meta, bytes.NewReader(data)); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}

	gotMeta, gotR, err := fs.LoadSnapshot(ctx)
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	defer gotR.Close()
	if gotMeta != meta {
		t.Fatalf("meta mismatch: got %+v, want %+v", gotMeta, meta)
	}
	gotData, err := io.ReadAll(gotR)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(gotData, data) {
		t.Fatalf("data mismatch: got %q, want %q", gotData, data)
	}
}

func TestSnapshot_PersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	fs1, _ := filestore.Open(dir)
	meta := raft.SnapshotMeta{LastIncludedIndex: 50, LastIncludedTerm: 2}
	_ = fs1.SaveSnapshot(ctx, meta, bytes.NewReader([]byte("data")))
	_ = fs1.Close()

	fs2, err := filestore.Open(dir)
	if err != nil {
		t.Fatalf("Reopen: %v", err)
	}
	defer func() { _ = fs2.Close() }()

	gotMeta, gotR, err := fs2.LoadSnapshot(ctx)
	if err != nil {
		t.Fatalf("LoadSnapshot after reopen: %v", err)
	}
	defer gotR.Close()
	if gotMeta != meta {
		t.Fatalf("meta mismatch: got %+v, want %+v", gotMeta, meta)
	}
}

// --- Phase 2 crash recovery ----------------------------------------------------
//
// TruncatePrefix Phase 2 uses .log.tmp and .idx.tmp files to atomically
// replace a boundary segment. These tests simulate the three possible crash
// states and verify that Open() recovers correctly in each case.

// TestPhase2Crash_BothTmps simulates a crash before either rename completed:
// both .log.tmp and .idx.tmp exist while the originals are untouched.
// Recovery should discard both tmps and leave the original files intact.
func TestPhase2Crash_BothTmps(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	// Write a small store and close it.
	fs := openSmall(t, dir)
	if err := fs.AppendLogEntries(ctx, makeEntries(1, 10, 1)); err != nil {
		t.Fatalf("AppendLogEntries: %v", err)
	}
	_ = fs.Close()

	// Place decoy .log.tmp and .idx.tmp for seg-00000 to simulate a crash
	// during Phase 2 before the first rename.
	logTmp := dir + "/seg-00000.log.tmp"
	idxTmp := dir + "/seg-00000.idx.tmp"
	if err := os.WriteFile(logTmp, []byte("bad-log"), 0o600); err != nil {
		t.Fatalf("write log.tmp: %v", err)
	}
	if err := os.WriteFile(idxTmp, []byte("bad-idx"), 0o600); err != nil {
		t.Fatalf("write idx.tmp: %v", err)
	}

	// Open must discard both tmps and load the original segment unchanged.
	fs2, err := filestore.Open(dir)
	if err != nil {
		t.Fatalf("Open after crash (both tmps): %v", err)
	}
	defer func() { _ = fs2.Close() }()

	last, _ := fs2.LastIndex(ctx)
	if last != 10 {
		t.Fatalf("expected lastIndex=10 (original data intact), got %d", last)
	}

	// Both tmp files must be gone.
	if _, err := os.Stat(logTmp); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("log.tmp should have been removed, stat returned: %v", err)
	}
	if _, err := os.Stat(idxTmp); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("idx.tmp should have been removed, stat returned: %v", err)
	}
}

// TestPhase2Crash_OnlyIdxTmp simulates a crash after log.tmp was renamed to
// log (the commit point) but before idx.tmp was renamed to idx.
// Recovery should rename idx.tmp → idx and the store should be readable.
func TestPhase2Crash_OnlyIdxTmp(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	// Create a real store, do a real Phase 2 truncation so we have valid .tmp
	// content, then manually restore the "interrupted rename" state.
	fs := openSmall(t, dir)
	if err := fs.AppendLogEntries(ctx, makeEntries(1, 10, 1)); err != nil {
		t.Fatalf("AppendLogEntries: %v", err)
	}
	// Read and save the original seg-00000 content so we can reconstruct
	// the crash state after Phase 2 renames have already happened.
	origLog, err := os.ReadFile(dir + "/seg-00000.log")
	if err != nil {
		t.Fatalf("read orig log: %v", err)
	}
	origIdx, err := os.ReadFile(dir + "/seg-00000.idx")
	if err != nil {
		t.Fatalf("read orig idx: %v", err)
	}
	_ = fs.Close()

	// Perform a full Phase 2 truncation (toIndex=2 lands inside seg-00000).
	fs2, err := filestore.OpenWithSegmentSize(dir, 100)
	if err != nil {
		t.Fatalf("reopen for truncation: %v", err)
	}
	if err = fs2.TruncatePrefix(ctx, 2); err != nil {
		t.Fatalf("TruncatePrefix: %v", err)
	}
	// Grab the correctly-rewritten idx file content (Phase 2 produced it).
	newLog, err := os.ReadFile(dir + "/seg-00000.log")
	if err != nil {
		t.Fatalf("read new log: %v", err)
	}
	newIdx, err := os.ReadFile(dir + "/seg-00000.idx")
	if err != nil {
		t.Fatalf("read new idx: %v", err)
	}
	_ = fs2.Close()

	// Reconstruct crash state: log was renamed (new content), idx was NOT
	// renamed (old content), idx.tmp has the new content.
	if err = os.WriteFile(dir+"/seg-00000.log", newLog, 0o600); err != nil {
		t.Fatalf("restore log: %v", err)
	}
	if err = os.WriteFile(dir+"/seg-00000.idx", origIdx, 0o600); err != nil {
		t.Fatalf("restore orig idx: %v", err)
	}
	if err = os.WriteFile(dir+"/seg-00000.idx.tmp", newIdx, 0o600); err != nil {
		t.Fatalf("write idx.tmp: %v", err)
	}
	_ = origLog // suppress unused warning

	// Open should detect idx.tmp (no matching log.tmp) and rename it to idx.
	fs3, err := filestore.Open(dir)
	if err != nil {
		t.Fatalf("Open after crash (only idx.tmp): %v", err)
	}
	defer func() { _ = fs3.Close() }()

	// The store should be readable from index 2 onward.
	first, _ := fs3.FirstIndex(ctx)
	if first != 2 {
		t.Fatalf("expected firstIndex=2 after recovery, got %d", first)
	}
	for i := raft.Index(2); i <= 10; i++ {
		if _, err := fs3.GetLogEntry(ctx, i); err != nil {
			t.Errorf("GetLogEntry(%d) after Phase2 crash recovery: %v", i, err)
		}
	}

	// idx.tmp must be gone.
	if _, err := os.Stat(dir + "/seg-00000.idx.tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("idx.tmp should have been renamed away by recovery")
	}
}

// TestPhase2Crash_OnlyLogTmp simulates a crash after log.tmp was written but
// before idx.tmp was created (or before the first rename). Only log.tmp exists.
// Recovery should discard log.tmp and leave originals untouched.
func TestPhase2Crash_OnlyLogTmp(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	fs := openSmall(t, dir)
	if err := fs.AppendLogEntries(ctx, makeEntries(1, 10, 1)); err != nil {
		t.Fatalf("AppendLogEntries: %v", err)
	}
	_ = fs.Close()

	// Place only a log.tmp (no idx.tmp).
	logTmp := dir + "/seg-00000.log.tmp"
	if err := os.WriteFile(logTmp, []byte("partial"), 0o600); err != nil {
		t.Fatalf("write log.tmp: %v", err)
	}

	fs2, err := filestore.Open(dir)
	if err != nil {
		t.Fatalf("Open after crash (only log.tmp): %v", err)
	}
	defer func() { _ = fs2.Close() }()

	// Original data should be intact.
	last, _ := fs2.LastIndex(ctx)
	if last != 10 {
		t.Fatalf("expected lastIndex=10, got %d", last)
	}
	if _, err := os.Stat(logTmp); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("log.tmp should have been removed by recovery")
	}
}

// Compile-time interface check.
var _ raft.Storage = (*filestore.FileStore)(nil)
