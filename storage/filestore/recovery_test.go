package filestore_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/brunoga/raft/v2"
	"github.com/brunoga/raft/v2/storage/filestore"
)

// With a 100-byte segment threshold and the 7-byte commands makeEntries
// produces, each entry occupies 31 bytes and segments hold four entries:
// 1-4, 5-8, 9-12, and so on.
const smallSegSize = 100

func mustAppend(t *testing.T, fs *filestore.FileStore, from, to raft.Index, term raft.Term) {
	t.Helper()
	if err := fs.AppendLogEntries(context.Background(), makeEntries(from, to, term)); err != nil {
		t.Fatalf("AppendLogEntries(%d..%d): %v", from, to, err)
	}
}

// assertReadable checks that every index in [from, to] can be read back and
// carries the index it is filed under.
func assertReadable(t *testing.T, fs *filestore.FileStore, from, to raft.Index) {
	t.Helper()
	ctx := context.Background()
	for i := from; i <= to; i++ {
		e, err := fs.GetLogEntry(ctx, i)
		if err != nil {
			t.Fatalf("GetLogEntry(%d): %v", i, err)
		}
		if e.Index != i {
			t.Fatalf("the slot for index %d holds entry %d", i, e.Index)
		}
	}
}

func assertBounds(t *testing.T, fs *filestore.FileStore, wantFirst, wantLast raft.Index) {
	t.Helper()
	first, err := fs.FirstIndex()
	if err != nil {
		t.Fatalf("FirstIndex: %v", err)
	}
	last, err := fs.LastIndex()
	if err != nil {
		t.Fatalf("LastIndex: %v", err)
	}
	if first != wantFirst || last != wantLast {
		t.Fatalf("log bounds are [%d,%d], want [%d,%d]", first, last, wantFirst, wantLast)
	}
}

// --- Suffix truncation across segments --------------------------------------

// TestTruncateSuffix_LeftoverLaterSegmentIsDiscardedOnOpen covers the crash
// window in a suffix truncation that spans segments.
//
// The truncation shortens the segment that straddles fromIndex and unlinks
// every segment after it. If those unlinks are not durable before the boundary
// segment is shortened, a crash can leave a shortened boundary segment still
// followed by segments that were supposed to be gone. Nothing in the segment
// files themselves says they are stale, so recovery has to notice that they no
// longer chain onto the boundary and discard them — otherwise it reports
// entries the engine believes it discarded, and the log has a hole in it.
func TestTruncateSuffix_LeftoverLaterSegmentIsDiscardedOnOpen(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	fs, err := filestore.OpenWithSegmentSize(dir, smallSegSize)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	mustAppend(t, fs, 1, 12, 1) // segments 1-4, 5-8, 9-12

	// Everything on disk while the log is still intact.
	intact := captureDir(t, dir)

	if err = fs.TruncateSuffix(ctx, 6); err != nil {
		t.Fatalf("TruncateSuffix: %v", err)
	}
	if err = fs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Put back exactly the files the truncation unlinked, leaving the boundary
	// segment shortened: the state a crash between the two steps would leave.
	truncated := captureDir(t, dir)
	restored := 0
	for name, content := range intact {
		if _, stillThere := truncated[name]; stillThere {
			continue
		}
		if err = os.WriteFile(filepath.Join(dir, name), content, 0o600); err != nil {
			t.Fatalf("restore %s: %v", name, err)
		}
		restored++
	}
	if restored == 0 {
		t.Fatal("the truncation unlinked no files, so this scenario was not reproduced")
	}

	fs2, err := filestore.OpenWithSegmentSize(dir, smallSegSize)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = fs2.Close() }()

	assertBounds(t, fs2, 1, 5)
	assertReadable(t, fs2, 1, 5)

	if _, err = fs2.GetLogEntry(ctx, 6); !errors.Is(err, raft.ErrNotFound) {
		t.Fatalf("GetLogEntry(6) after recovery = %v, want ErrNotFound", err)
	}

	// The leftover files must be gone, not merely ignored, or the next open
	// would have to rediscover them.
	for name := range intact {
		if _, stillThere := truncated[name]; stillThere {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s survived recovery: stat returned %v", name, err)
		}
	}

	// The store must stay writable. Computing an index file position from an
	// entry that precedes the segment underflows the unsigned index type and
	// yields a negative file offset, which wedges the store for good.
	mustAppend(t, fs2, 6, 9, 2)
	assertBounds(t, fs2, 1, 9)
	assertReadable(t, fs2, 1, 9)
}

// TestTruncateSuffix_DiscardsBoundarySegmentWholeAndStaysWritable covers the
// case where fromIndex lands exactly on a segment boundary, so no part of that
// segment survives and it is unlinked along with the ones after it.
func TestTruncateSuffix_DiscardsBoundarySegmentWholeAndStaysWritable(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	fs, err := filestore.OpenWithSegmentSize(dir, smallSegSize)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	mustAppend(t, fs, 1, 12, 1)

	if err = fs.TruncateSuffix(ctx, 5); err != nil { // 5 is the start of a segment
		t.Fatalf("TruncateSuffix: %v", err)
	}
	assertBounds(t, fs, 1, 4)
	assertReadable(t, fs, 1, 4)

	mustAppend(t, fs, 5, 8, 2)
	assertBounds(t, fs, 1, 8)
	if err = fs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	fs2, err := filestore.OpenWithSegmentSize(dir, smallSegSize)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = fs2.Close() }()

	assertBounds(t, fs2, 1, 8)
	assertReadable(t, fs2, 1, 8)

	e, err := fs2.GetLogEntry(ctx, 5)
	if err != nil {
		t.Fatalf("GetLogEntry(5): %v", err)
	}
	if e.Term != 2 {
		t.Fatalf("index 5 has term %d, want 2 (the re-appended entry)", e.Term)
	}
}

// TestOpen_DiscardsSegmentsAfterAnEmptyOne checks that a segment holding no
// entries is dropped wherever it sits, not just at the end of the log, and
// that the segments after the resulting hole are dropped with it. Only the
// last segment used to be considered, so an empty segment in the middle
// survived and left the log reporting a range it could not read across.
func TestOpen_DiscardsSegmentsAfterAnEmptyOne(t *testing.T) {
	dir := t.TempDir()

	fs, err := filestore.OpenWithSegmentSize(dir, smallSegSize)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	mustAppend(t, fs, 1, 12, 1) // segments 1-4, 5-8, 9-12
	if err = fs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Empty the middle segment, as an interrupted compaction would.
	for _, ext := range []string{".log", ".idx"} {
		if err = os.Truncate(segPath(dir, 1, ext), 0); err != nil {
			t.Fatalf("empty middle segment%s: %v", ext, err)
		}
	}

	fs2, err := filestore.OpenWithSegmentSize(dir, smallSegSize)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = fs2.Close() }()

	assertBounds(t, fs2, 1, 4)
	assertReadable(t, fs2, 1, 4)

	// Both the empty segment and the now-unreachable one after it must be gone.
	for _, seq := range []int{1, 2} {
		for _, ext := range []string{".log", ".idx"} {
			if _, err := os.Stat(segPath(dir, seq, ext)); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("segment %d%s survived recovery: stat returned %v", seq, ext, err)
			}
		}
	}

	mustAppend(t, fs2, 5, 8, 2)
	assertBounds(t, fs2, 1, 8)
	assertReadable(t, fs2, 1, 8)
}

// --- Index-file validation --------------------------------------------------

// TestRecovery_IndexSlotThatWasNeverWrittenStopsTheWalk covers recovery of an
// index file whose slots did not all reach the disk.
//
// The index file is a dense array of byte offsets. A slot that was allocated
// but never written reads back as offset 0 — the offset of the segment's very
// first entry — so decoding it yields a real entry whose checksum verifies.
// Accepting it makes recovery believe the segment ends at that entry and
// silently drops every acknowledged entry after it. Slot i must therefore be
// required to hold the entry with index firstID+i, at an offset strictly
// greater than the previous slot's.
func TestRecovery_IndexSlotThatWasNeverWrittenStopsTheWalk(t *testing.T) {
	const total = 100

	tests := []struct {
		name     string
		zeroSlot int64
		wantLast raft.Index
	}{
		{"final slot", total - 1, total - 1},
		{"middle slot", 50, 50},
		{"second slot", 1, 1},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			ctx := context.Background()

			fs, err := filestore.Open(dir)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			mustAppend(t, fs, 1, total, 1)
			if err = fs.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}

			idxPath := segPath(dir, 0, ".idx")
			f, err := os.OpenFile(idxPath, os.O_RDWR, 0o600)
			if err != nil {
				t.Fatalf("open index file: %v", err)
			}
			var zeroed [8]byte
			if _, err = f.WriteAt(zeroed[:], tc.zeroSlot*8); err != nil {
				t.Fatalf("zero index slot %d: %v", tc.zeroSlot, err)
			}
			if err = f.Close(); err != nil {
				t.Fatalf("close index file: %v", err)
			}

			fs2, err := filestore.Open(dir)
			if err != nil {
				t.Fatalf("reopen: %v", err)
			}
			defer func() { _ = fs2.Close() }()

			assertBounds(t, fs2, 1, tc.wantLast)
			assertReadable(t, fs2, 1, tc.wantLast)

			if _, err = fs2.GetLogEntry(ctx, tc.wantLast+1); !errors.Is(err, raft.ErrNotFound) {
				t.Errorf("GetLogEntry(%d) = %v, want ErrNotFound", tc.wantLast+1, err)
			}

			// Recovery must actually shorten the index file and fsync it, not
			// just ignore the tail in memory; otherwise the next open sees the
			// damaged slots all over again.
			st, err := os.Stat(idxPath)
			if err != nil {
				t.Fatalf("stat index file: %v", err)
			}
			if want := int64(tc.wantLast) * 8; st.Size() != want {
				t.Errorf("index file is %d bytes after recovery, want %d", st.Size(), want)
			}

			// And the repair must survive another open untouched.
			if err = fs2.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			fs3, err := filestore.Open(dir)
			if err != nil {
				t.Fatalf("third open: %v", err)
			}
			defer func() { _ = fs3.Close() }()
			assertBounds(t, fs3, 1, tc.wantLast)
		})
	}
}

// TestRecovery_TrailingLogBytesWithoutAnIndexSlotAreTruncated covers the other
// half of a torn append: the log write landed but the index write did not.
// The orphaned bytes must be trimmed so the next append does not write on top
// of a partially written entry.
func TestRecovery_TrailingLogBytesWithoutAnIndexSlotAreTruncated(t *testing.T) {
	dir := t.TempDir()

	fs, err := filestore.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	mustAppend(t, fs, 1, 10, 1)
	if err = fs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	logPath := segPath(dir, 0, ".log")
	logStat, err := os.Stat(logPath)
	if err != nil {
		t.Fatalf("stat log: %v", err)
	}

	// Drop the last index slot, leaving its log bytes behind.
	idxPath := segPath(dir, 0, ".idx")
	idxStat, err := os.Stat(idxPath)
	if err != nil {
		t.Fatalf("stat index file: %v", err)
	}
	if err = os.Truncate(idxPath, idxStat.Size()-8); err != nil {
		t.Fatalf("shorten index file: %v", err)
	}

	fs2, err := filestore.Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = fs2.Close() }()

	assertBounds(t, fs2, 1, 9)
	assertReadable(t, fs2, 1, 9)

	newLogStat, err := os.Stat(logPath)
	if err != nil {
		t.Fatalf("stat log after recovery: %v", err)
	}
	if newLogStat.Size() >= logStat.Size() {
		t.Errorf("log file is still %d bytes after recovery, want less than %d",
			newLogStat.Size(), logStat.Size())
	}

	// Appending must reuse the trimmed space and stay readable.
	mustAppend(t, fs2, 10, 12, 2)
	assertBounds(t, fs2, 1, 12)
	assertReadable(t, fs2, 1, 12)
}

// --- Segment file naming ----------------------------------------------------

// renameSegment moves a segment's files to a different base name, standing in
// for files written by a release that padded sequence numbers differently.
func renameSegment(t *testing.T, dir string, seq int, newBase string) {
	t.Helper()
	for _, ext := range []string{".log", ".idx"} {
		if err := os.Rename(segPath(dir, seq, ext), filepath.Join(dir, newBase+ext)); err != nil {
			t.Fatalf("rename segment %d%s: %v", seq, ext, err)
		}
	}
}

// TestSegments_NamesAreOrderedNumericallyNotLexicographically checks that
// segments are discovered and ordered by parsing their sequence number.
//
// Once the sequence number needs more digits than the name is padded to, a
// fixed-width glob stops matching the new files altogether and sorting the
// names as text puts "seg-100000" before "seg-99999". Either one reorders the
// log, and since segments that do not chain onto their predecessor are
// discarded, it silently throws entries away.
func TestSegments_NamesAreOrderedNumericallyNotLexicographically(t *testing.T) {
	dir := t.TempDir()

	fs, err := filestore.OpenWithSegmentSize(dir, smallSegSize)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	mustAppend(t, fs, 1, 8, 1) // two segments: 1-4 and 5-8
	if err = fs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Straddle a digit-width boundary, where text order and numeric order
	// disagree.
	renameSegment(t, dir, 0, "seg-99999")
	renameSegment(t, dir, 1, "seg-100000")

	fs2, err := filestore.OpenWithSegmentSize(dir, smallSegSize)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = fs2.Close() }()

	assertBounds(t, fs2, 1, 8)
	assertReadable(t, fs2, 1, 8)

	// Rotation must continue from the highest sequence number present.
	mustAppend(t, fs2, 9, 16, 2)
	assertBounds(t, fs2, 1, 16)
	assertReadable(t, fs2, 1, 16)
}

// TestSegments_NarrowLegacyNamesStillLoad checks that segment files written
// with a narrower zero-padding width remain readable and writable.
func TestSegments_NarrowLegacyNamesStillLoad(t *testing.T) {
	dir := t.TempDir()

	fs, err := filestore.OpenWithSegmentSize(dir, smallSegSize)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	mustAppend(t, fs, 1, 8, 1)
	if err = fs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	renameSegment(t, dir, 0, "seg-00000")
	renameSegment(t, dir, 1, "seg-00001")

	fs2, err := filestore.OpenWithSegmentSize(dir, smallSegSize)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = fs2.Close() }()

	assertBounds(t, fs2, 1, 8)
	assertReadable(t, fs2, 1, 8)

	mustAppend(t, fs2, 9, 12, 2)
	assertBounds(t, fs2, 1, 12)
	assertReadable(t, fs2, 1, 12)

	// A prefix truncation has to find the narrow-named files too.
	if err = fs2.TruncatePrefix(context.Background(), 6); err != nil {
		t.Fatalf("TruncatePrefix: %v", err)
	}
	assertBounds(t, fs2, 6, 12)
	assertReadable(t, fs2, 6, 12)
}

// TestOpen_IgnoresUnrelatedFilesSharingThePrefix checks that discovery does not
// choke on a file that merely starts like a segment name.
func TestOpen_IgnoresUnrelatedFilesSharingThePrefix(t *testing.T) {
	dir := t.TempDir()

	fs, err := filestore.OpenWithSegmentSize(dir, smallSegSize)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	mustAppend(t, fs, 1, 8, 1)
	if err = fs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	for _, name := range []string{"seg-notanumber.log", "seg-.log", "segments.log"} {
		if err = os.WriteFile(filepath.Join(dir, name), []byte("unrelated"), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	fs2, err := filestore.OpenWithSegmentSize(dir, smallSegSize)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = fs2.Close() }()

	assertBounds(t, fs2, 1, 8)
	assertReadable(t, fs2, 1, 8)
}
