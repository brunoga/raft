package easyraft

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// importAll feeds a backup into a store as chunks of the given size, and
// returns the error the last one produced.
func importAll(t *testing.T, s *Store, index uint64, token string, backup []byte, chunk int) error {
	t.Helper()
	for seq := 0; ; seq++ {
		start := seq * chunk
		end := min(start+chunk, len(backup))
		final := end >= len(backup)
		_, err := applyAt(t, s, index+uint64(seq), &command{
			Op: opImport,
			Import: &importChunk{
				Token: token,
				Seq:   seq,
				Data:  backup[start:end],
				Final: final,
			},
		})
		if err != nil || final {
			return err
		}
	}
}

// seedStore fills a store with a few collections, a lease and its keys, so a
// backup of it has every kind of state in it.
func seedStore(t *testing.T, s *Store) {
	t.Helper()
	lease := grantLease(t, s, 1, 60_000, 0)
	writes := []command{
		{Op: opUpsert, Collection: "users", Key: "alice", Value: jsonValue(t, "Alice")},
		{Op: opUpsert, Collection: "users", Key: "bob", Value: jsonValue(t, "Bob")},
		{Op: opUpsert, Collection: "config", Key: "mode", Value: jsonValue(t, "on")},
		{Op: opUpsert, Collection: "services", Key: "web-1",
			Value: jsonValue(t, "10.0.0.1"), Lease: uint64(lease)},
	}
	for i := range writes {
		if _, err := applyAt(t, s, uint64(10+i), &writes[i]); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}
}

// TestImport_ReplacesTheWholeState pins what an import is: a replacement, not
// a merge. A key the cluster holds that the backup does not is gone.
func TestImport_ReplacesTheWholeState(t *testing.T) {
	source := newTestStore(t, &config{})
	seedStore(t, source)

	var backup bytes.Buffer
	if err := source.snapshot(&backup); err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	target := newTestStore(t, &config{})
	if _, err := applyAt(t, target, 1, &command{
		Op: opUpsert, Collection: "users", Key: "carol", Value: jsonValue(t, "Carol"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := applyAt(t, target, 2, &command{
		Op: opUpsert, Collection: "leftovers", Key: "x", Value: jsonValue(t, "gone soon"),
	}); err != nil {
		t.Fatal(err)
	}

	if err := importAll(t, target, 100, "tok", backup.Bytes(), 64); err != nil {
		t.Fatalf("import: %v", err)
	}

	target.mu.RLock()
	defer target.mu.RUnlock()
	if _, ok := target.collections["users"]["carol"]; ok {
		t.Error("a key the backup did not contain survived the import")
	}
	if _, ok := target.collections["leftovers"]; ok {
		t.Error("a collection the backup did not contain survived the import")
	}
	if got := string(target.collections["users"]["alice"]); got != `"Alice"` {
		t.Errorf("users/alice is %s after the import", got)
	}
	if got := string(target.collections["config"]["mode"]); got != `"on"` {
		t.Errorf("config/mode is %s after the import", got)
	}
	if len(target.leases) != 1 {
		t.Errorf("the import brought %d leases, want 1", len(target.leases))
	}
	for _, held := range target.leases {
		if _, ok := held.Keys["services"]["web-1"]; !ok {
			t.Errorf("the imported lease holds %v", held.Keys)
		}
	}
	if target.keyLeases["services"]["web-1"] == 0 {
		t.Error("the imported key is not attached to its lease")
	}
	if target.stateBytes == 0 || target.keyCount != 4 {
		t.Errorf("after the import the store reports %d bytes across %d keys",
			target.stateBytes, target.keyCount)
	}
}

// TestImport_IsOneEntry is the guarantee the chunking exists to preserve: the
// state is untouched until the entry carrying the last chunk applies.
func TestImport_IsOneEntry(t *testing.T) {
	source := newTestStore(t, &config{})
	seedStore(t, source)
	var backup bytes.Buffer
	if err := source.snapshot(&backup); err != nil {
		t.Fatal(err)
	}

	target := newTestStore(t, &config{})
	if _, err := applyAt(t, target, 1, &command{
		Op: opUpsert, Collection: "before", Key: "k", Value: jsonValue(t, "untouched"),
	}); err != nil {
		t.Fatal(err)
	}

	raw := backup.Bytes()
	chunk := 32
	chunks := (len(raw) + chunk - 1) / chunk
	if chunks < 3 {
		t.Fatalf("the backup is only %d chunks; this test needs several", chunks)
	}

	// Every chunk but the last.
	for seq := range chunks - 1 {
		start := seq * chunk
		if _, err := applyAt(t, target, uint64(10+seq), &command{
			Op:     opImport,
			Import: &importChunk{Token: "tok", Seq: seq, Data: raw[start : start+chunk]},
		}); err != nil {
			t.Fatalf("chunk %d: %v", seq, err)
		}
		target.mu.RLock()
		_, stillThere := target.collections["before"]["k"]
		_, imported := target.collections["users"]
		target.mu.RUnlock()
		if !stillThere {
			t.Fatalf("the old state was gone after chunk %d of %d", seq, chunks)
		}
		if imported {
			t.Fatalf("the new state appeared after chunk %d of %d", seq, chunks)
		}
	}

	// The last one swaps everything at once.
	last := (chunks - 1) * chunk
	if _, err := applyAt(t, target, 999, &command{
		Op:     opImport,
		Import: &importChunk{Token: "tok", Seq: chunks - 1, Data: raw[last:], Final: true},
	}); err != nil {
		t.Fatalf("final chunk: %v", err)
	}
	target.mu.RLock()
	defer target.mu.RUnlock()
	if _, ok := target.collections["before"]; ok {
		t.Error("the old state survived the final chunk")
	}
	if _, ok := target.collections["users"]["alice"]; !ok {
		t.Error("the new state did not arrive with the final chunk")
	}
	if target.importing != nil {
		t.Error("the staging buffer was kept after the import finished")
	}
}

// TestImport_ACorruptBackupChangesNothing checks the failure that matters
// most: a backup that decodes to nothing usable must leave the cluster as it
// was rather than empty.
func TestImport_ACorruptBackupChangesNothing(t *testing.T) {
	target := newTestStore(t, &config{})
	seedStore(t, target)
	before := target.KeyCount()

	err := importAll(t, target, 100, "tok", []byte(`{"users":{"alice":`), 8)
	if err == nil {
		t.Fatal("a truncated backup was accepted")
	}
	if got := target.KeyCount(); got != before {
		t.Errorf("after a failed import the store holds %d keys, held %d before", got, before)
	}
	target.mu.RLock()
	staged := target.importing
	target.mu.RUnlock()
	if staged != nil {
		t.Error("a failed import left its staging buffer behind")
	}
}

// TestImport_OutOfOrderOrUnknownChunksAreRefused covers the two ways a chunk
// can arrive wrong. Writing one to the wrong place would build a backup that
// decodes to something nobody ever took.
func TestImport_OutOfOrderOrUnknownChunksAreRefused(t *testing.T) {
	s := newTestStore(t, &config{})

	if _, err := applyAt(t, s, 1, &command{
		Op: opImport, Import: &importChunk{Token: "tok", Seq: 0, Data: []byte("{")},
	}); err != nil {
		t.Fatalf("first chunk: %v", err)
	}
	if _, err := applyAt(t, s, 2, &command{
		Op: opImport, Import: &importChunk{Token: "tok", Seq: 5, Data: []byte("}")},
	}); err == nil {
		t.Error("a chunk out of order was accepted")
	}
	if _, err := applyAt(t, s, 3, &command{
		Op: opImport, Import: &importChunk{Token: "other", Seq: 1, Data: []byte("}")},
	}); err == nil {
		t.Error("a chunk of an import that is not in progress was accepted")
	}
	if _, err := applyAt(t, s, 4, &command{Op: opImport}); err == nil {
		t.Error("an import entry carrying no chunk was accepted")
	}

	// A second import starting from zero replaces the first, so an abandoned
	// one never has to be cleaned up before the next attempt.
	if _, err := applyAt(t, s, 5, &command{
		Op: opImport, Import: &importChunk{Token: "second", Seq: 0, Data: []byte("x")},
	}); err != nil {
		t.Fatalf("a second import: %v", err)
	}
	s.mu.RLock()
	staged := s.importing
	s.mu.RUnlock()
	if staged == nil || staged.token != "second" || string(staged.buf) != "x" {
		t.Errorf("the second import staged %+v", staged)
	}

	// Abort discards it.
	if _, err := applyAt(t, s, 6, &command{
		Op: opImport, Import: &importChunk{Token: "second", Abort: true},
	}); err != nil {
		t.Fatalf("abort: %v", err)
	}
	s.mu.RLock()
	staged = s.importing
	s.mu.RUnlock()
	if staged != nil {
		t.Error("abort left the staging buffer behind")
	}
}

// TestImport_RefusedInsideATransaction pins that an import cannot be rolled
// back as part of a batch, so it is refused rather than half-applied.
func TestImport_RefusedInsideATransaction(t *testing.T) {
	s := newTestStore(t, &config{})
	if _, err := applyAt(t, s, 1, &command{Op: opBatch, Batch: []command{
		{Op: opImport, Import: &importChunk{Token: "tok", Seq: 0, Data: []byte("{}")}},
	}}); err == nil {
		t.Fatal("an import inside a transaction was accepted")
	}
	if s.importing != nil {
		t.Error("the refused import staged something")
	}
}

// TestImport_StagingTravelsInSnapshots is the divergence this would otherwise
// cause. A replica that restored from a snapshot mid-import and came back
// with nothing staged would apply the final chunk differently from every
// other replica -- silently, and with nothing in the log to explain it.
func TestImport_StagingTravelsInSnapshots(t *testing.T) {
	source := newTestStore(t, &config{})
	seedStore(t, source)
	var backup bytes.Buffer
	if err := source.snapshot(&backup); err != nil {
		t.Fatal(err)
	}
	raw := backup.Bytes()
	half := len(raw) / 2

	// A replica part-way through an import.
	a := newTestStore(t, &config{})
	if _, err := applyAt(t, a, 1, &command{
		Op: opImport, Import: &importChunk{Token: "tok", Seq: 0, Data: raw[:half]},
	}); err != nil {
		t.Fatal(err)
	}

	// Another replica catches up from its snapshot instead of its log.
	var mid bytes.Buffer
	if err := a.snapshot(&mid); err != nil {
		t.Fatal(err)
	}
	b := newTestStore(t, &config{})
	if err := b.restore(bytes.NewReader(mid.Bytes())); err != nil {
		t.Fatalf("restore: %v", err)
	}
	b.mu.RLock()
	staged := b.importing
	b.mu.RUnlock()
	if staged == nil {
		t.Fatal("the restored replica has no staged import; the final chunk would apply " +
			"differently on it than on every other replica")
	}
	if staged.token != "tok" || staged.seq != 1 || !bytes.Equal(staged.buf, raw[:half]) {
		t.Fatalf("the restored replica staged %q at seq %d under token %q",
			staged.buf, staged.seq, staged.token)
	}

	// Both finish the import and end up identical.
	final := &command{
		Op: opImport, Import: &importChunk{Token: "tok", Seq: 1, Data: raw[half:], Final: true},
	}
	for name, store := range map[string]*Store{"a": a, "b": b} {
		if _, err := applyAt(t, store, 2, final); err != nil {
			t.Fatalf("replica %s: final chunk: %v", name, err)
		}
	}
	var afterA, afterB bytes.Buffer
	if err := a.snapshot(&afterA); err != nil {
		t.Fatal(err)
	}
	if err := b.snapshot(&afterB); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(afterA.Bytes(), afterB.Bytes()) {
		t.Error("the two replicas hold different state after the same import")
	}
}

// TestImport_RevisionNeverGoesBackwards pins the one thing an import does not
// take from the backup. A conditional write built on a revision read before
// the import must still be refused after it.
func TestImport_RevisionNeverGoesBackwards(t *testing.T) {
	source := newTestStore(t, &config{})
	if _, err := applyAt(t, source, 5, &command{
		Op: opUpsert, Collection: "c", Key: "k", Value: jsonValue(t, "from the backup"),
	}); err != nil {
		t.Fatal(err)
	}
	var backup bytes.Buffer
	if err := source.snapshot(&backup); err != nil {
		t.Fatal(err)
	}

	// A target that is already well past the backup's revision.
	target := newTestStore(t, &config{})
	if _, err := applyAt(t, target, 900, &command{
		Op: opUpsert, Collection: "c", Key: "k", Value: jsonValue(t, "newer"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := importAll(t, target, 1000, "tok", backup.Bytes(), 64); err != nil {
		t.Fatalf("import: %v", err)
	}

	if got := target.Revision(); got < 900 {
		t.Errorf("after importing an older backup the store revision is %d, was 900", got)
	}
	// And a write after the import takes a revision above everything before it.
	if _, err := applyAt(t, target, 2000, &command{
		Op: opUpsert, Collection: "c", Key: "after", Value: jsonValue(t, "x"),
	}); err != nil {
		t.Fatal(err)
	}
	if got := revOf(t, target, "c", "after"); got != 2000 {
		t.Errorf("a write after the import took revision %d", got)
	}
}

// TestBackup_RoundTripsEverything checks that what comes out goes back in,
// for a store large enough that the chunking has to work.
func TestBackup_RoundTripsEverything(t *testing.T) {
	source := newTestStore(t, &config{})
	for i := range 300 {
		if _, err := applyAt(t, source, uint64(i+1), &command{
			Op:         opUpsert,
			Collection: fmt.Sprintf("c%d", i%5),
			Key:        fmt.Sprintf("key-%03d", i),
			Value:      jsonValue(t, strings.Repeat("v", i%40)),
		}); err != nil {
			t.Fatal(err)
		}
	}
	var backup bytes.Buffer
	if err := source.snapshot(&backup); err != nil {
		t.Fatal(err)
	}

	target := newTestStore(t, &config{})
	if err := importAll(t, target, 1000, "tok", backup.Bytes(), 128); err != nil {
		t.Fatalf("import: %v", err)
	}

	var after bytes.Buffer
	if err := target.snapshot(&after); err != nil {
		t.Fatal(err)
	}

	// Everything but the store's own revision, which the import deliberately
	// carries forward from the log it applied in rather than from the backup.
	before, now := map[string]json.RawMessage{}, map[string]json.RawMessage{}
	if err := json.Unmarshal(bytes.TrimSpace(backup.Bytes()), &before); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(bytes.TrimSpace(after.Bytes()), &now); err != nil {
		t.Fatal(err)
	}
	delete(before, snapshotRevisionKey)
	delete(now, snapshotRevisionKey)
	if len(before) != len(now) {
		t.Fatalf("the imported store has %d top-level keys, the backup had %d", len(now), len(before))
	}
	for key, want := range before {
		if got := string(now[key]); got != string(want) {
			t.Errorf("%s after the import is %s, was %s", key, got, want)
		}
	}
	if target.KeyCount() != source.KeyCount() || target.StateBytes() != source.StateBytes() {
		t.Errorf("the imported store reports %d bytes/%d keys, the source %d/%d",
			target.StateBytes(), target.KeyCount(), source.StateBytes(), source.KeyCount())
	}
}

// TestBackup_IsTheSnapshotFormat keeps the two from drifting apart, since an
// import decodes a backup with the snapshot decoder.
func TestBackup_IsTheSnapshotFormat(t *testing.T) {
	s := newTestStore(t, &config{})
	seedStore(t, s)

	var backup bytes.Buffer
	revision, err := s.BackupStale(&backup)
	if err != nil {
		t.Fatalf("BackupStale: %v", err)
	}
	if revision != s.Revision() {
		t.Errorf("BackupStale reported revision %d, the store is at %d", revision, s.Revision())
	}

	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(bytes.TrimSpace(backup.Bytes()), &decoded); err != nil {
		t.Fatalf("a backup is not valid JSON: %v", err)
	}
	for _, key := range []string{"users", "config", snapshotRevisionsKey, snapshotLeasesKey} {
		if _, ok := decoded[key]; !ok {
			t.Errorf("the backup has no %s", key)
		}
	}

	// A witness holds no data, so it has no backup to give.
	witness := newTestStore(t, &config{Witness: true})
	if _, err := witness.BackupStale(&backup); !errors.Is(err, ErrWitness) {
		t.Errorf("BackupStale on a witness: %v, want ErrWitness", err)
	}
}
