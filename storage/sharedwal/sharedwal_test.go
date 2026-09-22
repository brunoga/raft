package sharedwal_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/brunoga/raft"
	"github.com/brunoga/raft/storage/sharedwal"
)

var ctx = context.Background()

func entries(from, to raft.Index, term raft.Term) []raft.LogEntry {
	out := make([]raft.LogEntry, 0, to-from+1)
	for i := from; i <= to; i++ {
		out = append(out, raft.LogEntry{Index: i, Term: term, Command: []byte(fmt.Sprintf("cmd-%d", i))})
	}
	return out
}

func mustOpen(t *testing.T, dir string, opts ...sharedwal.Option) *sharedwal.WAL {
	t.Helper()
	w, err := sharedwal.Open(dir, opts...)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })
	return w
}

func expectEntries(t *testing.T, s raft.Storage, lo, hi raft.Index, term raft.Term) {
	t.Helper()
	got, err := s.GetLogEntries(ctx, lo, hi)
	if err != nil {
		t.Fatalf("GetLogEntries(%d,%d): %v", lo, hi, err)
	}
	if len(got) != int(hi-lo) {
		t.Fatalf("GetLogEntries(%d,%d) returned %d entries", lo, hi, len(got))
	}
	for i, e := range got {
		want := raft.LogEntry{Index: lo + raft.Index(i), Term: term, Command: []byte(fmt.Sprintf("cmd-%d", lo+raft.Index(i)))}
		if e.Index != want.Index || e.Term != want.Term || !bytes.Equal(e.Command, want.Command) {
			t.Fatalf("entry %d = %+v, want %+v", lo+raft.Index(i), e, want)
		}
	}
}

func TestStorage_LogAndHardState(t *testing.T) {
	w := mustOpen(t, t.TempDir())
	s := w.Storage(7)

	if first, _ := s.FirstIndex(); first != 0 {
		t.Fatalf("FirstIndex on an empty log = %d", first)
	}
	if hs, err := s.LoadHardState(ctx); err != nil || hs != (raft.HardState{}) {
		t.Fatalf("LoadHardState on an empty log = %+v, %v", hs, err)
	}
	if err := s.SaveHardState(ctx, raft.HardState{CurrentTerm: 3, VotedFor: "n2"}); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendLogEntries(ctx, entries(1, 10, 1)); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendLogEntries(ctx, entries(11, 15, 2)); err != nil {
		t.Fatal(err)
	}
	first, _ := s.FirstIndex()
	last, _ := s.LastIndex()
	if first != 1 || last != 15 {
		t.Fatalf("bounds = [%d, %d], want [1, 15]", first, last)
	}
	expectEntries(t, s, 1, 11, 1)
	expectEntries(t, s, 11, 16, 2)
	// A range crossing two records reads as one run.
	crossing, err := s.GetLogEntries(ctx, 8, 13)
	if err != nil || len(crossing) != 5 || crossing[0].Term != 1 || crossing[4].Term != 2 {
		t.Fatalf("GetLogEntries(8,13) = %+v, %v", crossing, err)
	}
	e, err := s.GetLogEntry(ctx, 12)
	if err != nil || e.Term != 2 {
		t.Fatalf("GetLogEntry(12) = %+v, %v", e, err)
	}
	if _, err := s.GetLogEntry(ctx, 16); !errors.Is(err, raft.ErrNotFound) {
		t.Fatalf("GetLogEntry(16) = %v, want ErrNotFound", err)
	}
	if _, err := s.GetLogEntries(ctx, 0, 3); !errors.Is(err, raft.ErrNotFound) {
		t.Fatalf("GetLogEntries(0,3) = %v, want ErrNotFound", err)
	}
	hs, _ := s.LoadHardState(ctx)
	if hs.CurrentTerm != 3 || hs.VotedFor != "n2" {
		t.Fatalf("hard state = %+v", hs)
	}

	// Truncations.
	if err := s.TruncateSuffix(ctx, 13); err != nil {
		t.Fatal(err)
	}
	if last, _ = s.LastIndex(); last != 12 {
		t.Fatalf("LastIndex after TruncateSuffix(13) = %d", last)
	}
	if err := s.AppendLogEntries(ctx, entries(13, 14, 3)); err != nil {
		t.Fatal(err)
	}
	expectEntries(t, s, 13, 15, 3)
	if err := s.TruncatePrefix(ctx, 5); err != nil {
		t.Fatal(err)
	}
	if first, _ = s.FirstIndex(); first != 5 {
		t.Fatalf("FirstIndex after TruncatePrefix(5) = %d", first)
	}
	if _, err := s.GetLogEntry(ctx, 4); !errors.Is(err, raft.ErrNotFound) {
		t.Fatalf("compacted entry read as %v", err)
	}
	if err := s.TruncatePrefix(ctx, 100); err != nil {
		t.Fatal(err)
	}
	first, _ = s.FirstIndex()
	last, _ = s.LastIndex()
	if first != 0 || last != 0 {
		t.Fatalf("bounds after compacting everything = [%d, %d]", first, last)
	}
}

func TestStorage_ReadsAreCopies(t *testing.T) {
	w := mustOpen(t, t.TempDir())
	s := w.Storage(1)
	in := entries(1, 3, 1)
	if err := s.AppendLogEntries(ctx, in); err != nil {
		t.Fatal(err)
	}
	for i := range in {
		for j := range in[i].Command {
			in[i].Command[j] = 0xFF
		}
	}
	expectEntries(t, s, 1, 4, 1)
	got, _ := s.GetLogEntry(ctx, 2)
	got.Command[0] = 'X'
	expectEntries(t, s, 2, 3, 1)
}

func TestStorage_AppendOverwritesFromItsFirstIndex(t *testing.T) {
	w := mustOpen(t, t.TempDir())
	s := w.Storage(1)
	if err := s.AppendLogEntries(ctx, entries(1, 5, 1)); err != nil {
		t.Fatal(err)
	}
	// A new leader's entries replace from index 3 on.
	if err := s.AppendLogEntries(ctx, entries(3, 4, 2)); err != nil {
		t.Fatal(err)
	}
	last, _ := s.LastIndex()
	if last != 4 {
		t.Fatalf("LastIndex = %d, want 4", last)
	}
	expectEntries(t, s, 1, 3, 1)
	expectEntries(t, s, 3, 5, 2)
}

func TestGroups_AreIsolated(t *testing.T) {
	w := mustOpen(t, t.TempDir())
	a, b := w.Storage(1), w.Storage(2)
	if err := a.AppendLogEntries(ctx, entries(1, 3, 1)); err != nil {
		t.Fatal(err)
	}
	if err := b.AppendLogEntries(ctx, entries(1, 2, 5)); err != nil {
		t.Fatal(err)
	}
	if err := a.AppendLogEntries(ctx, entries(4, 6, 1)); err != nil {
		t.Fatal(err)
	}
	if err := b.SaveHardState(ctx, raft.HardState{CurrentTerm: 9}); err != nil {
		t.Fatal(err)
	}
	expectEntries(t, a, 1, 7, 1)
	expectEntries(t, b, 1, 3, 5)
	if hs, _ := a.LoadHardState(ctx); hs.CurrentTerm != 0 {
		t.Fatalf("group 1 saw group 2's hard state: %+v", hs)
	}
	if err := b.TruncatePrefix(ctx, 3); err != nil {
		t.Fatal(err)
	}
	if first, _ := a.FirstIndex(); first != 1 {
		t.Fatalf("group 2's compaction moved group 1's first index to %d", first)
	}
	if got := w.Groups(); len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("Groups = %v", got)
	}
}

func TestSnapshot_RoundTrip(t *testing.T) {
	w := mustOpen(t, t.TempDir())
	s := w.Storage(3)
	if _, _, err := s.LoadSnapshot(ctx); !errors.Is(err, raft.ErrNoSnapshot) {
		t.Fatalf("LoadSnapshot on nothing = %v", err)
	}
	data := bytes.Repeat([]byte("snapshot-data-"), 1000)
	meta := raft.SnapshotMeta{LastIncludedIndex: 40, LastIncludedTerm: 2}
	if err := s.SaveSnapshot(ctx, meta, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	got, rc, err := s.LoadSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got != meta {
		t.Fatalf("meta = %+v, want %+v", got, meta)
	}
	body, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil || !bytes.Equal(body, data) {
		t.Fatalf("snapshot body mismatch (%d bytes, err %v)", len(body), err)
	}
	// A newer one replaces it and the old file goes.
	meta2 := raft.SnapshotMeta{LastIncludedIndex: 80, LastIncludedTerm: 3}
	if err = s.SaveSnapshot(ctx, meta2, bytes.NewReader([]byte("v2"))); err != nil {
		t.Fatal(err)
	}
	got, rc, err = s.LoadSnapshot(ctx)
	if err != nil || got != meta2 {
		t.Fatalf("second LoadSnapshot = %+v, %v", got, err)
	}
	body, _ = io.ReadAll(rc)
	_ = rc.Close()
	if string(body) != "v2" {
		t.Fatalf("second snapshot body = %q", body)
	}
	files, _ := os.ReadDir(filepath.Join(w.Dir(), "snap"))
	if len(files) != 1 || files[0].Name() != "3-80" {
		names := []string{}
		for _, f := range files {
			names = append(names, f.Name())
		}
		t.Fatalf("snapshot files = %v, want only 3-80", names)
	}
	// An empty snapshot is a valid one.
	meta3 := raft.SnapshotMeta{LastIncludedIndex: 90, LastIncludedTerm: 3}
	if err = s.SaveSnapshot(ctx, meta3, bytes.NewReader(nil)); err != nil {
		t.Fatal(err)
	}
	_, rc, err = s.LoadSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	body, err = io.ReadAll(rc)
	_ = rc.Close()
	if err != nil || len(body) != 0 {
		t.Fatalf("empty snapshot read back as %d bytes, %v", len(body), err)
	}
}

func TestSnapshot_CorruptionIsDetected(t *testing.T) {
	w := mustOpen(t, t.TempDir())
	s := w.Storage(3)
	meta := raft.SnapshotMeta{LastIncludedIndex: 4, LastIncludedTerm: 1}
	if err := s.SaveSnapshot(ctx, meta, bytes.NewReader([]byte("hello world"))); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(w.Dir(), "snap", "3-4")
	raw, _ := os.ReadFile(path)
	raw[len(raw)-20] ^= 0xFF // inside the data
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	_, rc, err := s.LoadSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadAll(rc)
	_ = rc.Close()
	if err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("corrupted snapshot read without error: %v", err)
	}
}

func TestRestart_RecoversEveryGroup(t *testing.T) {
	dir := t.TempDir()
	w := mustOpen(t, dir, sharedwal.WithSegmentSize(4096))
	a, b := w.Storage(1), w.Storage(2)
	if err := a.SaveState(ctx, &raft.HardState{CurrentTerm: 4, VotedFor: "x"}, entries(1, 50, 1)); err != nil {
		t.Fatal(err)
	}
	if err := b.AppendLogEntries(ctx, entries(1, 30, 2)); err != nil {
		t.Fatal(err)
	}
	if err := a.TruncateSuffix(ctx, 41); err != nil {
		t.Fatal(err)
	}
	if err := a.AppendLogEntries(ctx, entries(41, 45, 3)); err != nil {
		t.Fatal(err)
	}
	if err := b.TruncatePrefix(ctx, 10); err != nil {
		t.Fatal(err)
	}
	if err := b.SaveSnapshot(ctx, raft.SnapshotMeta{LastIncludedIndex: 9, LastIncludedTerm: 2}, bytes.NewReader([]byte("b"))); err != nil {
		t.Fatal(err)
	}
	if err := a.SaveCommitIndex(ctx, 33); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := a.LastIndex(); !errors.Is(err, sharedwal.ErrClosed) {
		t.Fatalf("operation after Close returned %v", err)
	}

	w = mustOpen(t, dir, sharedwal.WithSegmentSize(4096))
	a, b = w.Storage(1), w.Storage(2)
	if hs, _ := a.LoadHardState(ctx); hs.CurrentTerm != 4 || hs.VotedFor != "x" {
		t.Fatalf("group 1 hard state after restart = %+v", hs)
	}
	first, _ := a.FirstIndex()
	last, _ := a.LastIndex()
	if first != 1 || last != 45 {
		t.Fatalf("group 1 bounds after restart = [%d, %d]", first, last)
	}
	expectEntries(t, a, 1, 41, 1)
	expectEntries(t, a, 41, 46, 3)
	first, _ = b.FirstIndex()
	last, _ = b.LastIndex()
	if first != 10 || last != 30 {
		t.Fatalf("group 2 bounds after restart = [%d, %d]", first, last)
	}
	expectEntries(t, b, 10, 31, 2)
	meta, rc, err := b.LoadSnapshot(ctx)
	if err != nil || meta.LastIncludedIndex != 9 {
		t.Fatalf("group 2 snapshot after restart = %+v, %v", meta, err)
	}
	_ = rc.Close()
	if c, _ := a.LoadCommitIndex(ctx); c != 33 {
		t.Fatalf("group 1 commit index after restart = %d", c)
	}
	if got := w.Groups(); len(got) != 2 {
		t.Fatalf("Groups after restart = %v", got)
	}
}

func TestRestart_TornTailIsCutOff(t *testing.T) {
	dir := t.TempDir()
	w, err := sharedwal.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s := w.Storage(1)
	if err := s.AppendLogEntries(ctx, entries(1, 5, 1)); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendLogEntries(ctx, entries(6, 8, 1)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	// Chop the last record in half, as a crash mid-write would.
	seg := filepath.Join(dir, "wal-00000000.log")
	raw, _ := os.ReadFile(seg)
	if err := os.WriteFile(seg, raw[:len(raw)-7], 0o600); err != nil {
		t.Fatal(err)
	}
	w = mustOpen(t, dir)
	s = w.Storage(1)
	last, _ := s.LastIndex()
	if last != 5 {
		t.Fatalf("LastIndex after a torn tail = %d, want 5", last)
	}
	expectEntries(t, s, 1, 6, 1)
	// The log is writable again from the cut.
	if err := s.AppendLogEntries(ctx, entries(6, 7, 2)); err != nil {
		t.Fatal(err)
	}
	expectEntries(t, s, 6, 8, 2)
}

func TestReclaim_DeletesSegmentsNothingNeeds(t *testing.T) {
	dir := t.TempDir()
	w := mustOpen(t, dir, sharedwal.WithSegmentSize(2048))
	s := w.Storage(1)
	for i := raft.Index(1); i <= 200; i += 10 {
		if err := s.AppendLogEntries(ctx, entries(i, i+9, 1)); err != nil {
			t.Fatal(err)
		}
	}
	segs := func() int {
		n := 0
		files, _ := os.ReadDir(dir)
		for _, f := range files {
			if strings.HasPrefix(f.Name(), "wal-") {
				n++
			}
		}
		return n
	}
	if segs() < 3 {
		t.Fatalf("only %d segments after 200 entries at a 2 KiB segment size", segs())
	}
	if err := s.SaveHardState(ctx, raft.HardState{CurrentTerm: 2}); err != nil {
		t.Fatal(err)
	}
	// Compact everything but the last twenty entries: the older segments
	// hold nothing live and go, the hard state having been rewritten forward.
	if err := s.TruncatePrefix(ctx, 181); err != nil {
		t.Fatal(err)
	}
	if err := w.Reclaim(); err != nil {
		t.Fatal(err)
	}
	if n := segs(); n > 2 {
		t.Fatalf("%d segments remain after compacting to the last 20 entries", n)
	}
	expectEntries(t, s, 181, 201, 1)
	if hs, _ := s.LoadHardState(ctx); hs.CurrentTerm != 2 {
		t.Fatalf("hard state lost in reclaim: %+v", hs)
	}
	// And all of it survives a restart.
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	w = mustOpen(t, dir, sharedwal.WithSegmentSize(2048))
	s = w.Storage(1)
	expectEntries(t, s, 181, 201, 1)
	if hs, _ := s.LoadHardState(ctx); hs.CurrentTerm != 2 {
		t.Fatalf("hard state after restart: %+v", hs)
	}
}

func TestRemove_ForgetsAGroup(t *testing.T) {
	dir := t.TempDir()
	w := mustOpen(t, dir)
	s := w.Storage(5)
	if err := s.AppendLogEntries(ctx, entries(1, 3, 1)); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveSnapshot(ctx, raft.SnapshotMeta{LastIncludedIndex: 2, LastIncludedTerm: 1}, bytes.NewReader([]byte("x"))); err != nil {
		t.Fatal(err)
	}
	if err := w.Remove(5); err != nil {
		t.Fatal(err)
	}
	if got := w.Groups(); len(got) != 0 {
		t.Fatalf("Groups after Remove = %v", got)
	}
	if last, _ := w.Storage(5).LastIndex(); last != 0 {
		t.Fatalf("removed group still has entries: last %d", last)
	}
	if _, _, err := w.Storage(5).LoadSnapshot(ctx); !errors.Is(err, raft.ErrNoSnapshot) {
		t.Fatalf("removed group still has a snapshot: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	w = mustOpen(t, dir)
	if got := w.Groups(); len(got) != 0 {
		t.Fatalf("Groups after Remove and restart = %v", got)
	}
}

func TestOpen_RefusesASecondOpener(t *testing.T) {
	dir := t.TempDir()
	_ = mustOpen(t, dir)
	if _, err := sharedwal.Open(dir); !errors.Is(err, sharedwal.ErrLocked) {
		t.Fatalf("second Open = %v, want ErrLocked", err)
	}
}

func TestConcurrentGroups_AllDurable(t *testing.T) {
	dir := t.TempDir()
	w, err := sharedwal.Open(dir, sharedwal.WithSegmentSize(64<<10))
	if err != nil {
		t.Fatal(err)
	}
	const groups, perGroup = 16, 40
	var wg sync.WaitGroup
	errs := make(chan error, groups)
	for g := range groups {
		wg.Add(1)
		go func(id uint64) {
			defer wg.Done()
			s := w.Storage(id)
			for i := raft.Index(1); i <= perGroup; i++ {
				if err := s.SaveState(ctx, &raft.HardState{CurrentTerm: raft.Term(i)}, entries(i, i, raft.Term(id))); err != nil {
					errs <- err
					return
				}
			}
		}(uint64(g + 1))
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	w = mustOpen(t, dir, sharedwal.WithSegmentSize(64<<10))
	for g := 1; g <= groups; g++ {
		s := w.Storage(uint64(g))
		last, _ := s.LastIndex()
		if last != perGroup {
			t.Fatalf("group %d: last %d after restart, want %d", g, last, perGroup)
		}
		got, err := s.GetLogEntries(ctx, 1, perGroup+1)
		if err != nil || len(got) != perGroup {
			t.Fatalf("group %d: %d entries, %v", g, len(got), err)
		}
		for _, e := range got {
			if e.Term != raft.Term(g) {
				t.Fatalf("group %d holds an entry of term %d", g, e.Term)
			}
		}
		if hs, _ := s.LoadHardState(ctx); hs.CurrentTerm != perGroup {
			t.Fatalf("group %d: hard state %+v", g, hs)
		}
	}
}
