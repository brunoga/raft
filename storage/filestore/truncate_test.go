package filestore_test

import (
	"bytes"
	"context"
	"errors"
	"runtime"
	"sync"
	"testing"

	"github.com/brunoga/raft/v2"
	"github.com/brunoga/raft/v2/storage/filestore"
)

// bigEntries builds entries with a payload large enough that a segment rewrite
// has to loop over several copy chunks. Each payload is distinct so a rewrite
// that mixes up offsets is caught rather than passing on uniform bytes.
func bigEntries(from, to raft.Index, term raft.Term, size int) []raft.LogEntry {
	entries := make([]raft.LogEntry, 0, int(to-from+1))
	for i := from; i <= to; i++ {
		cmd := bytes.Repeat([]byte{byte(i), byte(i >> 8)}, size/2)
		entries = append(entries, raft.LogEntry{Index: i, Term: term, Command: cmd})
	}
	return entries
}

func wantBigCommand(i raft.Index, size int) []byte {
	return bytes.Repeat([]byte{byte(i), byte(i >> 8)}, size/2)
}

// TestTruncatePrefix_RewritesSegmentsLargerThanOneCopyChunk exercises the
// boundary-segment rewrite on a segment far larger than the copy buffer.
//
// The rewrite used to read the whole kept tail of the segment into memory in
// one allocation — up to a full segment, 64 MiB by default — while holding the
// store lock, which both spikes memory and pauses the Raft loop for the whole
// copy. Streaming it in bounded chunks has to produce byte-identical results,
// including the index offsets, which are rebased against the new start of the
// segment.
func TestTruncatePrefix_RewritesSegmentsLargerThanOneCopyChunk(t *testing.T) {
	const (
		payload = 64 * 1024
		count   = 128 // 8 MiB in one segment
		keepAt  = 33
	)

	dir := t.TempDir()
	ctx := context.Background()

	fs, err := filestore.Open(dir) // default segment size: everything in one segment
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err = fs.AppendLogEntries(ctx, bigEntries(1, count, 1, payload)); err != nil {
		t.Fatalf("AppendLogEntries: %v", err)
	}

	if err = fs.TruncatePrefix(ctx, keepAt); err != nil {
		t.Fatalf("TruncatePrefix: %v", err)
	}

	verify := func(t *testing.T, fs *filestore.FileStore, where string) {
		t.Helper()
		assertBounds(t, fs, keepAt, count)
		for i := raft.Index(keepAt); i <= count; i++ {
			e, readErr := fs.GetLogEntry(ctx, i)
			if readErr != nil {
				t.Fatalf("%s: GetLogEntry(%d): %v", where, i, readErr)
			}
			if e.Index != i {
				t.Fatalf("%s: the slot for index %d holds entry %d", where, i, e.Index)
			}
			if !bytes.Equal(e.Command, wantBigCommand(i, payload)) {
				t.Fatalf("%s: entry %d came back with the wrong payload", where, i)
			}
		}
		if _, readErr := fs.GetLogEntry(ctx, keepAt-1); !errors.Is(readErr, raft.ErrNotFound) {
			t.Errorf("%s: GetLogEntry(%d) = %v, want ErrNotFound", where, keepAt-1, readErr)
		}
	}

	verify(t, fs, "after truncation")

	// Appending must continue from the rewritten segment.
	if err = fs.AppendLogEntries(ctx, bigEntries(count+1, count+4, 2, payload)); err != nil {
		t.Fatalf("append after truncation: %v", err)
	}
	assertBounds(t, fs, keepAt, count+4)

	if err = fs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	fs2, err := filestore.Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = fs2.Close() }()

	assertBounds(t, fs2, keepAt, count+4)
	for i := raft.Index(keepAt); i <= count; i++ {
		e, err := fs2.GetLogEntry(ctx, i)
		if err != nil {
			t.Fatalf("after reopen: GetLogEntry(%d): %v", i, err)
		}
		if !bytes.Equal(e.Command, wantBigCommand(i, payload)) {
			t.Fatalf("after reopen: entry %d came back with the wrong payload", i)
		}
	}
}

// TestTruncatePrefix_CopyMemoryIsBounded checks that the boundary rewrite does
// not scale its memory use with the size of the segment it is rewriting.
func TestTruncatePrefix_CopyMemoryIsBounded(t *testing.T) {
	const (
		payload = 64 * 1024
		count   = 128 // 8 MiB in one segment
		keepAt  = 33
	)

	dir := t.TempDir()
	ctx := context.Background()

	fs, err := filestore.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = fs.Close() }()

	if err = fs.AppendLogEntries(ctx, bigEntries(1, count, 1, payload)); err != nil {
		t.Fatalf("AppendLogEntries: %v", err)
	}

	keptBytes := uint64(count-keepAt+1) * payload

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)

	if err = fs.TruncatePrefix(ctx, keepAt); err != nil {
		t.Fatalf("TruncatePrefix: %v", err)
	}

	runtime.ReadMemStats(&after)
	allocated := after.TotalAlloc - before.TotalAlloc

	// Reading the kept tail in one piece would allocate at least keptBytes.
	// A chunked copy allocates one buffer and reuses it, so anything close to
	// the size of the data means the streaming was lost.
	if limit := keptBytes / 2; allocated > limit {
		t.Errorf("rewriting %d bytes of kept log allocated %d bytes, want at most %d: "+
			"the copy is buffering the whole segment", keptBytes, allocated, limit)
	}
}

// TestTruncatePrefix_ConcurrentReadsStaySafe runs reads against the kept part
// of the log while the boundary segment is being rewritten. Entries at or
// after toIndex are never removed, so every one of these reads must succeed,
// and the race detector must stay quiet even though the rewrite deliberately
// works outside the store lock.
func TestTruncatePrefix_ConcurrentReadsStaySafe(t *testing.T) {
	const (
		payload = 16 * 1024
		count   = 192
		keepAt  = 64
	)

	dir := t.TempDir()
	ctx := context.Background()

	fs, err := filestore.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = fs.Close() }()

	if err = fs.AppendLogEntries(ctx, bigEntries(1, count, 1, payload)); err != nil {
		t.Fatalf("AppendLogEntries: %v", err)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				for i := raft.Index(keepAt); i <= count; i++ {
					e, readErr := fs.GetLogEntry(ctx, i)
					if readErr != nil {
						t.Errorf("GetLogEntry(%d) during truncation: %v", i, readErr)
						return
					}
					if e.Index != i {
						t.Errorf("the slot for index %d holds entry %d", i, e.Index)
						return
					}
				}
			}
		}()
	}

	if err = fs.TruncatePrefix(ctx, keepAt); err != nil {
		t.Errorf("TruncatePrefix: %v", err)
	}
	close(stop)
	wg.Wait()

	assertBounds(t, fs, keepAt, count)
}

// TestTruncatePrefix_AcrossSegmentsThenSuffixTruncation walks a store through
// both truncations and a reopen, checking the log stays contiguous and
// readable throughout.
func TestTruncatePrefix_AcrossSegmentsThenSuffixTruncation(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	fs, err := filestore.OpenWithSegmentSize(dir, smallSegSize)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	mustAppend(t, fs, 1, 20, 1) // segments 1-4, 5-8, 9-12, 13-16, 17-20

	if err = fs.TruncatePrefix(ctx, 7); err != nil { // drops one segment, rewrites one
		t.Fatalf("TruncatePrefix: %v", err)
	}
	assertBounds(t, fs, 7, 20)
	assertReadable(t, fs, 7, 20)

	if err = fs.TruncateSuffix(ctx, 15); err != nil {
		t.Fatalf("TruncateSuffix: %v", err)
	}
	assertBounds(t, fs, 7, 14)
	assertReadable(t, fs, 7, 14)

	mustAppend(t, fs, 15, 22, 2)
	assertBounds(t, fs, 7, 22)
	assertReadable(t, fs, 7, 22)

	if err = fs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	fs2, err := filestore.OpenWithSegmentSize(dir, smallSegSize)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = fs2.Close() }()

	assertBounds(t, fs2, 7, 22)
	assertReadable(t, fs2, 7, 22)

	if _, err = fs2.GetLogEntry(ctx, 6); !errors.Is(err, raft.ErrNotFound) {
		t.Errorf("GetLogEntry(6) after compaction = %v, want ErrNotFound", err)
	}
}

// TestTruncateSuffix_BelowFirstIndexIsReportedAsCompacted pins the error the
// engine relies on to tell "already gone" apart from "not there yet".
func TestTruncateSuffix_BelowFirstIndexIsReportedAsCompacted(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	fs, err := filestore.OpenWithSegmentSize(dir, smallSegSize)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = fs.Close() }()

	mustAppend(t, fs, 1, 12, 1)
	if err = fs.TruncatePrefix(ctx, 7); err != nil {
		t.Fatalf("TruncatePrefix: %v", err)
	}

	if err = fs.TruncateSuffix(ctx, 3); !errors.Is(err, raft.ErrCompacted) {
		t.Fatalf("TruncateSuffix below the first index = %v, want ErrCompacted", err)
	}

	// The log must be untouched by the rejected call.
	assertBounds(t, fs, 7, 12)
	assertReadable(t, fs, 7, 12)
}
