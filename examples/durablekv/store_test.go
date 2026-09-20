package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/brunoga/raft"
)

func putCmd(t *testing.T, index uint64, k, v string) raft.LogEntry {
	t.Helper()
	b, err := json.Marshal(command{Op: "put", Key: k, Value: v})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raft.LogEntry{Index: raft.Index(index), Term: 1, Command: b}
}

func delCmd(t *testing.T, index uint64, k string) raft.LogEntry {
	t.Helper()
	b, err := json.Marshal(command{Op: "delete", Key: k})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raft.LogEntry{Index: raft.Index(index), Term: 1, Command: b}
}

// TestAppliedIndex_NeverRunsAheadOfTheData is the promise the whole example
// rests on.
//
// The engine replays nothing at or below the reported index. If that index can
// be ahead of the data it describes, the entries in between are never applied
// by anyone: this node is permanently different from every other replica, and
// the log says nothing about why, because the engine was told the work was
// done.
//
// Every record carries the index that produced it and is written in the same
// sync, so the two cannot disagree. This checks that after every prefix of a
// run of writes, reopening the store reports an index whose data is all there.
func TestAppliedIndex_NeverRunsAheadOfTheData(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	s, err := openStore(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for i := uint64(1); i <= 20; i++ {
		if _, aerr := s.Apply(ctx, putCmd(t, i, fmt.Sprintf("k%d", i), fmt.Sprintf("v%d", i))); aerr != nil {
			t.Fatalf("apply %d: %v", i, aerr)
		}
	}
	if cerr := s.Close(); cerr != nil {
		t.Fatalf("close: %v", cerr)
	}

	// Truncating the file mid-record is what a crash during a write looks
	// like. However much is lost, what is reported must be backed by data.
	full, err := os.ReadFile(filepath.Join(dir, "state"))
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	for cut := len(full); cut >= 0; cut -= 7 {
		truncated := filepath.Join(t.TempDir(), "d")
		if err := os.MkdirAll(truncated, 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(truncated, "state"), full[:cut], 0o600); err != nil {
			t.Fatalf("write truncated state: %v", err)
		}

		reopened, err := openStore(truncated)
		if err != nil {
			t.Fatalf("open truncated at %d: %v", cut, err)
		}
		applied, err := reopened.AppliedIndex(ctx)
		if err != nil {
			t.Fatalf("applied index: %v", err)
		}
		for i := uint64(1); i <= uint64(applied); i++ {
			key := fmt.Sprintf("k%d", i)
			if _, ok := reopened.get(key); !ok {
				t.Fatalf("truncated at %d: reports applied through %d but %s is missing; "+
					"the engine will never replay it", cut, applied, key)
			}
		}
		_ = reopened.Close()
	}

	// The other half of a bad write is a record that is all there but wrong:
	// a torn sector, a bit flip. Without a checksum the length fields in a
	// corrupt record are believed, and the index in it is reported as applied.
	for pos := 8; pos < len(full); pos += 5 {
		corrupted := filepath.Join(t.TempDir(), "d")
		if err := os.MkdirAll(corrupted, 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		damaged := bytes.Clone(full)
		damaged[pos] ^= 0xFF
		if err := os.WriteFile(filepath.Join(corrupted, "state"), damaged, 0o600); err != nil {
			t.Fatalf("write corrupted state: %v", err)
		}

		reopened, err := openStore(corrupted)
		if err != nil {
			t.Fatalf("open corrupted at %d: %v", pos, err)
		}
		applied, err := reopened.AppliedIndex(ctx)
		if err != nil {
			t.Fatalf("applied index: %v", err)
		}
		if applied > 20 {
			t.Fatalf("corrupted at %d: reports applied through %d, which is past anything "+
				"ever written; a damaged record was believed", pos, applied)
		}
		for i := uint64(1); i <= uint64(applied); i++ {
			key := fmt.Sprintf("k%d", i)
			if _, ok := reopened.get(key); !ok {
				t.Fatalf("corrupted at %d: reports applied through %d but %s is missing",
					pos, applied, key)
			}
		}
		_ = reopened.Close()
	}
}

// TestReopen_RecoversStateAndIndex checks the ordinary restart: everything
// written is there, and the index is the last one applied.
func TestReopen_RecoversStateAndIndex(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	s, err := openStore(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for _, e := range []raft.LogEntry{putCmd(t, 1, "a", "1"), putCmd(t, 2, "b", "2"), delCmd(t, 3, "a")} {
		if _, aerr := s.Apply(ctx, e); aerr != nil {
			t.Fatalf("apply: %v", aerr)
		}
	}
	_ = s.Close()

	reopened, err := openStore(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = reopened.Close() }()

	if _, ok := reopened.get("a"); ok {
		t.Error("deleted key came back after a restart")
	}
	if v, ok := reopened.get("b"); !ok || v != "2" {
		t.Errorf("b = %q (%v) after restart, want \"2\"", v, ok)
	}
	applied, err := reopened.AppliedIndex(ctx)
	if err != nil {
		t.Fatalf("applied index: %v", err)
	}
	if applied != 3 {
		t.Errorf("applied index = %d after restart, want 3 -- including the delete, which "+
			"leaves no live key carrying it", applied)
	}
}

// TestApplyBatch_ReportsOnePerEntryAndIsolatesBadCommands checks the
// BatchApplier contract: one outcome per entry, in order, and a command the
// state machine rejects is that entry's outcome rather than the batch's.
func TestApplyBatch_ReportsOnePerEntryAndIsolatesBadCommands(t *testing.T) {
	ctx := context.Background()
	s, err := openStore(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = s.Close() }()

	entries := []raft.LogEntry{
		putCmd(t, 1, "a", "1"),
		{Index: 2, Term: 1, Command: []byte("not json")},
		putCmd(t, 3, "c", "3"),
	}
	outcomes, err := s.ApplyBatch(ctx, entries)
	if err != nil {
		t.Fatalf("ApplyBatch returned a batch error for one bad command: %v", err)
	}
	if len(outcomes) != len(entries) {
		t.Fatalf("got %d outcomes for %d entries; the engine stops the node over this",
			len(outcomes), len(entries))
	}
	if outcomes[0].Err != nil || outcomes[2].Err != nil {
		t.Errorf("good commands reported errors: %v, %v", outcomes[0].Err, outcomes[2].Err)
	}
	if outcomes[1].Err == nil {
		t.Error("the malformed command was reported as succeeding")
	}
	if v, ok := s.get("c"); !ok || v != "3" {
		t.Error("an entry after the malformed one was not applied")
	}
}

// TestCapture_IsNotAffectedByLaterWrites checks the SnapshotCapturer contract.
//
// Capture returns on the apply goroutine and its Write runs later, while
// entries keep applying. If the capture shared state with the store, the
// snapshot would be of neither the index it claims nor any other, and a
// follower restoring it would diverge.
func TestCapture_IsNotAffectedByLaterWrites(t *testing.T) {
	ctx := context.Background()
	s, err := openStore(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = s.Close() }()

	if _, aerr := s.Apply(ctx, putCmd(t, 1, "a", "before")); aerr != nil {
		t.Fatalf("apply: %v", aerr)
	}

	captured, err := s.Capture(ctx)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	defer captured.Release()

	// Everything that happens between Capture and Write must be invisible.
	for _, e := range []raft.LogEntry{putCmd(t, 2, "a", "after"), putCmd(t, 3, "b", "new")} {
		if _, aerr := s.Apply(ctx, e); aerr != nil {
			t.Fatalf("apply: %v", aerr)
		}
	}

	var buf bytes.Buffer
	if werr := captured.Write(ctx, &buf); werr != nil {
		t.Fatalf("write: %v", werr)
	}

	var got map[string]string
	if derr := json.Unmarshal(buf.Bytes(), &got); derr != nil {
		t.Fatalf("decode snapshot: %v", derr)
	}
	if got["a"] != "before" {
		t.Errorf("snapshot has a=%q; a write after Capture changed the captured state", got["a"])
	}
	if _, ok := got["b"]; ok {
		t.Error("snapshot contains a key written after Capture")
	}
}

// TestRestore_ReportsTheSnapshotsIndex checks that a restored store reports the
// index the snapshot was taken at.
//
// Reporting anything lower replays entries the snapshot already contains;
// reporting anything higher skips entries nobody applied. Restore is also the
// one path where the state changes without a record per entry, so the index has
// to be written deliberately.
func TestRestore_ReportsTheSnapshotsIndex(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := openStore(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	if _, aerr := s.Apply(ctx, putCmd(t, 1, "old", "value")); aerr != nil {
		t.Fatalf("apply: %v", aerr)
	}

	snapshot := []byte(`{"x":"1","y":"2"}`)
	meta := raft.SnapshotMeta{LastIncludedIndex: 900, LastIncludedTerm: 4}
	if rerr := s.Restore(ctx, meta, bytes.NewReader(snapshot)); rerr != nil {
		t.Fatalf("restore: %v", rerr)
	}

	applied, err := s.AppliedIndex(ctx)
	if err != nil {
		t.Fatalf("applied index: %v", err)
	}
	if applied != 900 {
		t.Errorf("applied index = %d after restore, want the snapshot's 900", applied)
	}
	if _, ok := s.get("old"); ok {
		t.Error("state from before the restore survived it")
	}
	_ = s.Close()

	// And it survives a restart, because Restore rewrote the file.
	reopened, err := openStore(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = reopened.Close() }()

	applied, err = reopened.AppliedIndex(ctx)
	if err != nil {
		t.Fatalf("applied index: %v", err)
	}
	if applied != 900 {
		t.Errorf("applied index = %d after restart, want 900", applied)
	}
	if v, _ := reopened.get("x"); v != "1" {
		t.Errorf("restored state lost after restart: x = %q", v)
	}
}

// TestCompaction_BoundsTheFileBySizeOfStateNotNumberOfWrites checks that
// rewriting the same key does not grow the file without limit, and that the
// applied index survives the rewrite.
func TestCompaction_BoundsTheFileBySizeOfStateNotNumberOfWrites(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := openStore(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	const writes = 6000
	for i := uint64(1); i <= writes; i++ {
		if _, aerr := s.Apply(ctx, putCmd(t, i, "hot", fmt.Sprintf("v%d", i))); aerr != nil {
			t.Fatalf("apply %d: %v", i, aerr)
		}
	}
	applied, err := s.AppliedIndex(ctx)
	if err != nil {
		t.Fatalf("applied index: %v", err)
	}
	if applied != writes {
		t.Fatalf("applied index = %d, want %d", applied, writes)
	}
	_ = s.Close()

	st, err := os.Stat(filepath.Join(dir, "state"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	// One live key. Without compaction the file would hold 6000 records.
	if st.Size() > 200*compactMinRecords {
		t.Errorf("state file is %d bytes for one key after %d writes; compaction is not running",
			st.Size(), writes)
	}

	reopened, err := openStore(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = reopened.Close() }()
	applied, err = reopened.AppliedIndex(ctx)
	if err != nil {
		t.Fatalf("applied index: %v", err)
	}
	if applied != writes {
		t.Errorf("applied index = %d after compaction and restart, want %d", applied, writes)
	}
	if v, _ := reopened.get("hot"); v != fmt.Sprintf("v%d", writes) {
		t.Errorf("hot = %q after compaction, want the last value written", v)
	}
}
