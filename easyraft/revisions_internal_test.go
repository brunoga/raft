package easyraft

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/brunoga/raft/v2"
)

// applyAt puts one command into effect at the given log index, which is the
// revision it stamps on whatever it writes.
func applyAt(t *testing.T, s *Store, index uint64, cmd *command) ([]byte, error) {
	t.Helper()
	raw, err := json.Marshal(cmd)
	if err != nil {
		t.Fatalf("encode command: %v", err)
	}
	return s.applyEntry(raft.LogEntry{Index: raft.Index(index), Term: 1, Command: raw})
}

func revOf(t *testing.T, s *Store, collection, key string) uint64 {
	t.Helper()
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.revisions[collection][key]
}

func jsonValue(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("encode value: %v", err)
	}
	return b
}

func ptrTo[T any](v T) *T { return &v }

// TestRevisions_StampTheApplyingEntryIndex pins what a revision is: the index
// of the entry that last wrote the key. Not a per-key counter, and not the
// number of writes -- both of those would have to be agreed separately, while
// the index is already agreed by the time Apply sees it.
func TestRevisions_StampTheApplyingEntryIndex(t *testing.T) {
	s := newTestStore(t, &config{})

	if _, err := applyAt(t, s, 4, &command{
		Op: opCreate, Collection: "c", Key: "k", Value: jsonValue(t, 1),
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if got := revOf(t, s, "c", "k"); got != 4 {
		t.Errorf("revision after the entry at index 4 is %d, want 4", got)
	}
	if got := s.Revision(); got != 4 {
		t.Errorf("store revision is %d, want 4", got)
	}

	if _, err := applyAt(t, s, 9, &command{
		Op: opUpdate, Collection: "c", Key: "k", Value: jsonValue(t, 2),
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if got := revOf(t, s, "c", "k"); got != 9 {
		t.Errorf("revision after the entry at index 9 is %d, want 9", got)
	}

	// An entry that writes nothing leaves the key's revision where it was.
	if _, err := applyAt(t, s, 11, &command{
		Op: opUpdate, Collection: "c", Key: "missing", Value: jsonValue(t, 3),
	}); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("update of a missing key: %v, want ErrKeyNotFound", err)
	}
	if got := revOf(t, s, "c", "k"); got != 9 {
		t.Errorf("an unrelated failed entry moved the revision to %d", got)
	}
	if got := s.Revision(); got != 9 {
		t.Errorf("a failed entry advanced the store revision to %d, want 9", got)
	}
}

// TestRevisions_ConditionalWriteRefusesAMovedKey is the compare-and-swap the
// whole feature exists for: an update built on a revision that has since been
// overwritten is refused rather than applied on top.
func TestRevisions_ConditionalWriteRefusesAMovedKey(t *testing.T) {
	s := newTestStore(t, &config{})

	if _, err := applyAt(t, s, 2, &command{
		Op: opCreate, Collection: "c", Key: "k", Value: jsonValue(t, "first"),
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	// Somebody else writes the key between the read and the conditional write.
	if _, err := applyAt(t, s, 5, &command{
		Op: opUpdate, Collection: "c", Key: "k", Value: jsonValue(t, "second"),
	}); err != nil {
		t.Fatalf("interleaved update: %v", err)
	}

	_, err := applyAt(t, s, 6, &command{
		Op: opUpdate, Collection: "c", Key: "k", Value: jsonValue(t, "lost"),
		IfRev: ptrTo(uint64(2)),
	})
	if !errors.Is(err, ErrRevisionMismatch) {
		t.Fatalf("conditional update on a stale revision: %v, want ErrRevisionMismatch", err)
	}

	s.mu.RLock()
	got := string(s.collections["c"]["k"])
	s.mu.RUnlock()
	if got != `"second"` {
		t.Errorf("the refused write left %s behind, want the interleaved value", got)
	}

	// The same write against the revision that is actually current applies.
	if _, err := applyAt(t, s, 7, &command{
		Op: opUpdate, Collection: "c", Key: "k", Value: jsonValue(t, "third"),
		IfRev: ptrTo(uint64(5)),
	}); err != nil {
		t.Fatalf("conditional update on the current revision: %v", err)
	}
	if got := revOf(t, s, "c", "k"); got != 7 {
		t.Errorf("revision after a conditional update is %d, want 7", got)
	}
}

// TestRevisions_ZeroMeansAbsent covers the one revision a caller can name
// without having read anything: a key that has never been written.
func TestRevisions_ZeroMeansAbsent(t *testing.T) {
	s := newTestStore(t, &config{})

	if _, err := applyAt(t, s, 3, &command{
		Op: opUpsert, Collection: "c", Key: "k", Value: jsonValue(t, 1), IfRev: ptrTo(uint64(0)),
	}); err != nil {
		t.Fatalf("conditional upsert of an absent key: %v", err)
	}
	if _, err := applyAt(t, s, 4, &command{
		Op: opUpsert, Collection: "c", Key: "k", Value: jsonValue(t, 2), IfRev: ptrTo(uint64(0)),
	}); !errors.Is(err, ErrRevisionMismatch) {
		t.Fatal("a second create-if-absent on the same key succeeded")
	}

	// A delete takes the revision with it, so the key is absent again --
	// which a conditional write on the old revision must notice.
	if _, err := applyAt(t, s, 5, &command{Op: opDelete, Collection: "c", Key: "k"}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if got := revOf(t, s, "c", "k"); got != 0 {
		t.Errorf("revision of a deleted key is %d, want 0", got)
	}
	if _, err := applyAt(t, s, 6, &command{
		Op: opUpdate, Collection: "c", Key: "k", Value: jsonValue(t, 3), IfRev: ptrTo(uint64(3)),
	}); !errors.Is(err, ErrRevisionMismatch) {
		t.Fatalf("conditional update after a delete and recreate window: %v, want ErrRevisionMismatch", err)
	}
}

// TestRevisions_DeleteIfMatchesTheRevisionRead checks the other half of
// compare-and-swap: removing a key only while it is still the one that was
// read.
func TestRevisions_DeleteIfMatchesTheRevisionRead(t *testing.T) {
	s := newTestStore(t, &config{})

	if _, err := applyAt(t, s, 2, &command{
		Op: opCreate, Collection: "c", Key: "k", Value: jsonValue(t, 1),
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := applyAt(t, s, 3, &command{
		Op: opUpdate, Collection: "c", Key: "k", Value: jsonValue(t, 2),
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if _, err := applyAt(t, s, 4, &command{
		Op: opDelete, Collection: "c", Key: "k", IfRev: ptrTo(uint64(2)),
	}); !errors.Is(err, ErrRevisionMismatch) {
		t.Fatal("a conditional delete on a stale revision removed the key")
	}
	if _, err := applyAt(t, s, 5, &command{
		Op: opDelete, Collection: "c", Key: "k", IfRev: ptrTo(uint64(3)),
	}); err != nil {
		t.Fatalf("conditional delete on the current revision: %v", err)
	}
	s.mu.RLock()
	_, stillThere := s.collections["c"]["k"]
	s.mu.RUnlock()
	if stillThere {
		t.Error("the key survived a conditional delete that matched")
	}
}

// TestRevisions_CheckGuardsAWholeBatch covers the operation that writes
// nothing: a batch conditional on a key it does not touch.
func TestRevisions_CheckGuardsAWholeBatch(t *testing.T) {
	s := newTestStore(t, &config{})

	if _, err := applyAt(t, s, 2, &command{
		Op: opCreate, Collection: "leases", Key: "owner", Value: jsonValue(t, "node-a"),
	}); err != nil {
		t.Fatalf("create lease: %v", err)
	}

	batch := func(index, guardRev uint64, value any) error {
		_, err := applyAt(t, s, index, &command{Op: opBatch, Batch: []command{
			{Op: opCheck, Collection: "leases", Key: "owner", IfRev: ptrTo(guardRev)},
			{Op: opUpsert, Collection: "work", Key: "item", Value: jsonValue(t, value)},
		}})
		return err
	}

	if err := batch(3, 2, "done-by-a"); err != nil {
		t.Fatalf("batch under a held lease: %v", err)
	}

	// The lease moves, so the same batch must no longer apply.
	if _, err := applyAt(t, s, 4, &command{
		Op: opUpdate, Collection: "leases", Key: "owner", Value: jsonValue(t, "node-b"),
	}); err != nil {
		t.Fatalf("lease handover: %v", err)
	}
	if err := batch(5, 2, "done-by-a-again"); !errors.Is(err, ErrRevisionMismatch) {
		t.Fatalf("batch under a lost lease: %v, want ErrRevisionMismatch", err)
	}

	s.mu.RLock()
	got := string(s.collections["work"]["item"])
	s.mu.RUnlock()
	if got != `"done-by-a"` {
		t.Errorf("the refused batch wrote %s", got)
	}

	// A check with nothing to check is a mistake, not a no-op.
	if _, err := applyAt(t, s, 6, &command{Op: opBatch, Batch: []command{
		{Op: opCheck, Collection: "leases", Key: "owner"},
	}}); err == nil {
		t.Error("a check with no revision was accepted")
	}
}

// TestRevisions_RollbackRestoresThem pins that a failed batch leaves no
// revision behind. A revision left at the value a rolled-back write stamped
// would refuse the next conditional write against a key whose value never
// changed -- a lost update reported as a conflict.
func TestRevisions_RollbackRestoresThem(t *testing.T) {
	s := newTestStore(t, &config{})

	if _, err := applyAt(t, s, 2, &command{
		Op: opCreate, Collection: "c", Key: "keep", Value: jsonValue(t, "original"),
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	before := s.Revision()

	// The batch writes two keys and then fails on a third.
	_, err := applyAt(t, s, 8, &command{Op: opBatch, Batch: []command{
		{Op: opUpdate, Collection: "c", Key: "keep", Value: jsonValue(t, "overwritten")},
		{Op: opCreate, Collection: "fresh", Key: "new", Value: jsonValue(t, "x")},
		{Op: opUpdate, Collection: "c", Key: "absent", Value: jsonValue(t, "y")},
	}})
	if !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("batch: %v, want ErrKeyNotFound", err)
	}

	if got := revOf(t, s, "c", "keep"); got != 2 {
		t.Errorf("revision of a rolled-back key is %d, want the 2 it had before the batch", got)
	}
	if got := s.Revision(); got != before {
		t.Errorf("store revision is %d after a rolled-back batch, want %d", got, before)
	}
	s.mu.RLock()
	_, collLeft := s.revisions["fresh"]
	s.mu.RUnlock()
	if collLeft {
		t.Error("a collection the rolled-back batch created still has a revision map")
	}

	// And the key is still writable on the revision it was read at.
	if _, err := applyAt(t, s, 9, &command{
		Op: opUpdate, Collection: "c", Key: "keep", Value: jsonValue(t, "next"), IfRev: ptrTo(uint64(2)),
	}); err != nil {
		t.Fatalf("conditional update after a rolled-back batch: %v", err)
	}
}

// TestRevisions_SurviveASnapshot checks that a replica restored from a
// snapshot refuses and accepts exactly the conditional writes the replica that
// wrote it would have. Revisions that did not survive a snapshot would make
// compare-and-swap depend on how recently a node was restarted.
func TestRevisions_SurviveASnapshot(t *testing.T) {
	s := newTestStore(t, &config{})
	for i, key := range []string{"a", "b", "c"} {
		if _, err := applyAt(t, s, uint64(10+i), &command{
			Op: opCreate, Collection: "coll", Key: key, Value: jsonValue(t, key),
		}); err != nil {
			t.Fatalf("create %s: %v", key, err)
		}
	}

	var buf bytes.Buffer
	if err := s.snapshot(&buf); err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	restored := newTestStore(t, &config{})
	if err := restored.restore(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("restore: %v", err)
	}

	for i, key := range []string{"a", "b", "c"} {
		want := uint64(10 + i)
		if got := revOf(t, restored, "coll", key); got != want {
			t.Errorf("restored revision of %s is %d, want %d", key, got, want)
		}
	}
	if got := restored.Revision(); got != 12 {
		t.Errorf("restored store revision is %d, want 12", got)
	}

	if _, err := applyAt(t, restored, 20, &command{
		Op: opUpdate, Collection: "coll", Key: "a", Value: jsonValue(t, "z"), IfRev: ptrTo(uint64(11)),
	}); !errors.Is(err, ErrRevisionMismatch) {
		t.Fatalf("stale conditional update against a restored store: %v", err)
	}
	if _, err := applyAt(t, restored, 21, &command{
		Op: opUpdate, Collection: "coll", Key: "a", Value: jsonValue(t, "z"), IfRev: ptrTo(uint64(10)),
	}); err != nil {
		t.Fatalf("current conditional update against a restored store: %v", err)
	}

	// The snapshot is still byte-identical for identical state, which is what
	// lets two replicas be compared by their snapshots.
	var second bytes.Buffer
	if err := s.snapshot(&second); err != nil {
		t.Fatalf("second snapshot: %v", err)
	}
	if !bytes.Equal(buf.Bytes(), second.Bytes()) {
		t.Error("two snapshots of the same state differ")
	}
}

// TestRevisions_RestoreASnapshotWrittenBeforeThem is the upgrade path. A
// snapshot from a version that did not track revisions has neither reserved
// key, and must restore as a store whose keys have simply never been written
// -- not fail, and not leave a nil map for the next write to panic on.
func TestRevisions_RestoreASnapshotWrittenBeforeThem(t *testing.T) {
	old := []byte(`{"coll":{"a":"one","b":"two"}}` + "\n")

	s := newTestStore(t, &config{})
	if err := s.restore(bytes.NewReader(old)); err != nil {
		t.Fatalf("restore a pre-revision snapshot: %v", err)
	}
	if got := s.Revision(); got != 0 {
		t.Errorf("store revision from a pre-revision snapshot is %d, want 0", got)
	}
	if got := revOf(t, s, "coll", "a"); got != 0 {
		t.Errorf("key revision from a pre-revision snapshot is %d, want 0", got)
	}
	s.mu.RLock()
	value := string(s.collections["coll"]["b"])
	s.mu.RUnlock()
	if value != `"two"` {
		t.Errorf("value from a pre-revision snapshot is %s", value)
	}

	// Writing works, and starts stamping from the entry that does it.
	if _, err := applyAt(t, s, 30, &command{
		Op: opUpdate, Collection: "coll", Key: "a", Value: jsonValue(t, "three"), IfRev: ptrTo(uint64(0)),
	}); err != nil {
		t.Fatalf("conditional update against a pre-revision snapshot: %v", err)
	}
	if got := revOf(t, s, "coll", "a"); got != 30 {
		t.Errorf("revision after the first write is %d, want 30", got)
	}
}

// TestRevisions_SnapshotReservedKeysAreNotCollections guards the choice of
// encoding: the two reserved names sit beside the collections rather than in a
// wrapper around them, which only works while no collection can be called
// either of them.
func TestRevisions_SnapshotReservedKeysAreNotCollections(t *testing.T) {
	for _, name := range []string{snapshotRevisionsKey, snapshotRevisionKey} {
		if !isReservedCollection(name) {
			t.Errorf("%q is not a reserved collection name, so a caller could create a collection "+
				"that collides with it in a snapshot", name)
		}
	}

	s := newTestStore(t, &config{})
	var buf bytes.Buffer
	if err := s.snapshot(&buf); err != nil {
		t.Fatalf("snapshot of an empty store: %v", err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &decoded); err != nil {
		t.Fatalf("an empty store's snapshot is not valid JSON: %v\n%s", err, buf.String())
	}
	if _, ok := decoded[snapshotRevisionsKey]; !ok {
		t.Errorf("snapshot has no %s: %s", snapshotRevisionsKey, buf.String())
	}

	restored := newTestStore(t, &config{})
	if err := restored.restore(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("restore an empty store's snapshot: %v", err)
	}
	if n := len(restored.collections); n != 0 {
		t.Errorf("restoring an empty snapshot produced %d collections: %v", n, restored.collections)
	}
}

// TestRevisions_ManyKeysRoundTrip runs enough keys through a snapshot that a
// mistake in the incremental encoding -- a missing comma, a stray brace -- has
// somewhere to show itself.
func TestRevisions_ManyKeysRoundTrip(t *testing.T) {
	s := newTestStore(t, &config{})
	for i := range 200 {
		coll := fmt.Sprintf("c%d", i%7)
		if _, err := applyAt(t, s, uint64(i+1), &command{
			Op: opUpsert, Collection: coll, Key: fmt.Sprintf("k%d", i), Value: jsonValue(t, i),
		}); err != nil {
			t.Fatalf("upsert %d: %v", i, err)
		}
	}

	var buf bytes.Buffer
	if err := s.snapshot(&buf); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	restored := newTestStore(t, &config{})
	if err := restored.restore(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("restore: %v", err)
	}

	for i := range 200 {
		coll := fmt.Sprintf("c%d", i%7)
		key := fmt.Sprintf("k%d", i)
		if got, want := revOf(t, restored, coll, key), uint64(i+1); got != want {
			t.Fatalf("%s/%s revision %d, want %d", coll, key, got, want)
		}
	}
	if got := restored.Revision(); got != 200 {
		t.Errorf("restored store revision %d, want 200", got)
	}
}
