package easyraft

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// size returns the store's running total and count.
func size(t *testing.T, s *Store) (total int64, keys int) {
	t.Helper()
	return s.StateBytes(), s.KeyCount()
}

// recounted measures the state from scratch, which is what the running total
// has to agree with.
func recounted(s *Store) (total int64, keys int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, coll := range s.collections {
		for key, value := range coll {
			keys++
			total += int64(len(key)) + int64(len(value))
		}
	}
	return total, keys
}

// checkAgrees is the assertion every test here makes: the number kept as
// writes happen matches the number a full walk would produce. A running total
// that drifts is worse than no total, because it looks like an answer.
func checkAgrees(t *testing.T, s *Store, after string) {
	t.Helper()
	gotBytes, gotKeys := size(t, s)
	wantBytes, wantKeys := recounted(s)
	if gotBytes != wantBytes || gotKeys != wantKeys {
		t.Errorf("after %s the running total is %d bytes/%d keys, a full walk says %d/%d",
			after, gotBytes, gotKeys, wantBytes, wantKeys)
	}
}

// TestStateSize_TracksEveryKindOfWrite walks every operation that can change
// the state and checks the running total against a full walk after each.
func TestStateSize_TracksEveryKindOfWrite(t *testing.T) {
	s := newTestStore(t, &config{})
	s.registerMutation("c", "grow", func(_, _ json.RawMessage) (json.RawMessage, json.RawMessage, error) {
		return json.RawMessage(`"a much longer value than before"`), nil, nil
	})

	steps := []struct {
		what string
		cmd  command
	}{
		{"create", command{Op: opCreate, Collection: "c", Key: "k1", Value: jsonValue(t, "one")}},
		{"a second create", command{Op: opCreate, Collection: "c", Key: "k2", Value: jsonValue(t, "two")}},
		{"update to a longer value", command{Op: opUpdate, Collection: "c", Key: "k1", Value: jsonValue(t, strings.Repeat("x", 100))}},
		{"update to a shorter value", command{Op: opUpdate, Collection: "c", Key: "k1", Value: jsonValue(t, "x")}},
		{"upsert of a new key", command{Op: opUpsert, Collection: "other", Key: "k3", Value: jsonValue(t, "three")}},
		{"upsert of an existing key", command{Op: opUpsert, Collection: "other", Key: "k3", Value: jsonValue(t, "three-ish")}},
		{"a mutation", command{Op: opMutate, Collection: "c", Key: "k1", MutateName: "grow"}},
		{"delete", command{Op: opDelete, Collection: "c", Key: "k2"}},
	}
	for i, step := range steps {
		cmd := step.cmd
		if _, err := applyAt(t, s, uint64(i+1), &cmd); err != nil {
			t.Fatalf("%s: %v", step.what, err)
		}
		checkAgrees(t, s, step.what)
	}

	if gotBytes, gotKeys := size(t, s); gotBytes == 0 || gotKeys != 2 {
		t.Errorf("after eight writes the store reports %d bytes across %d keys", gotBytes, gotKeys)
	}

	// Deleting everything returns both to zero, which a total that only ever
	// grew would not.
	for i, spec := range [][2]string{{"c", "k1"}, {"other", "k3"}} {
		if _, err := applyAt(t, s, uint64(20+i), &command{
			Op: opDelete, Collection: spec[0], Key: spec[1],
		}); err != nil {
			t.Fatal(err)
		}
	}
	if gotBytes, gotKeys := size(t, s); gotBytes != 0 || gotKeys != 0 {
		t.Errorf("an empty store reports %d bytes across %d keys", gotBytes, gotKeys)
	}
}

// TestStateSize_RolledBackBatchLeavesItUnchanged pins the case a running
// total is most likely to get wrong: writes that happened and then did not.
func TestStateSize_RolledBackBatchLeavesItUnchanged(t *testing.T) {
	s := newTestStore(t, &config{})
	if _, err := applyAt(t, s, 1, &command{
		Op: opCreate, Collection: "c", Key: "keep", Value: jsonValue(t, "original"),
	}); err != nil {
		t.Fatal(err)
	}
	beforeBytes, beforeKeys := size(t, s)

	_, err := applyAt(t, s, 2, &command{Op: opBatch, Batch: []command{
		{Op: opUpdate, Collection: "c", Key: "keep", Value: jsonValue(t, strings.Repeat("y", 500))},
		{Op: opCreate, Collection: "fresh", Key: "new", Value: jsonValue(t, "x")},
		{Op: opDelete, Collection: "c", Key: "keep"},
		{Op: opUpdate, Collection: "c", Key: "absent", Value: jsonValue(t, "z")},
	}})
	if err == nil {
		t.Fatal("the batch was expected to fail on its last operation")
	}

	checkAgrees(t, s, "a rolled-back batch")
	afterBytes, afterKeys := size(t, s)
	if afterBytes != beforeBytes || afterKeys != beforeKeys {
		t.Errorf("a rolled-back batch left %d bytes/%d keys, want the %d/%d it started with",
			afterBytes, afterKeys, beforeBytes, beforeKeys)
	}
}

// TestStateSize_LeaseRevocationSubtracts covers the other path that deletes
// keys without going through opDelete.
func TestStateSize_LeaseRevocationSubtracts(t *testing.T) {
	s := newTestStore(t, &config{})
	lease := grantLease(t, s, 1, 60_000, 0)

	if _, err := applyAt(t, s, 2, &command{
		Op: opUpsert, Collection: "c", Key: "permanent", Value: jsonValue(t, "stays"),
	}); err != nil {
		t.Fatal(err)
	}
	afterPermanent, _ := size(t, s)

	for i, key := range []string{"a", "b", "c"} {
		if _, err := applyAt(t, s, uint64(10+i), &command{
			Op: opUpsert, Collection: "c", Key: key,
			Value: jsonValue(t, strings.Repeat("z", 50)), Lease: uint64(lease),
		}); err != nil {
			t.Fatal(err)
		}
	}
	checkAgrees(t, s, "three leased writes")

	if _, err := applyAt(t, s, 20, &command{Op: opLeaseRevoke, Lease: uint64(lease)}); err != nil {
		t.Fatal(err)
	}
	checkAgrees(t, s, "a lease revocation")
	if got, _ := size(t, s); got != afterPermanent {
		t.Errorf("after revoking the lease the store reports %d bytes, want the %d it held "+
			"before the leased keys were written", got, afterPermanent)
	}
}

// TestStateSize_RestoreRecounts checks that a replica restored from a
// snapshot reports the same size as the one that wrote it, since a restore
// replaces everything at once and has nothing to adjust from.
func TestStateSize_RestoreRecounts(t *testing.T) {
	s := newTestStore(t, &config{})
	for i := range 50 {
		if _, err := applyAt(t, s, uint64(i+1), &command{
			Op: opUpsert, Collection: fmt.Sprintf("c%d", i%3),
			Key: fmt.Sprintf("key-%d", i), Value: jsonValue(t, strings.Repeat("v", i)),
		}); err != nil {
			t.Fatal(err)
		}
	}
	wantBytes, wantKeys := size(t, s)

	var buf bytes.Buffer
	if err := s.snapshot(&buf); err != nil {
		t.Fatal(err)
	}
	restored := newTestStore(t, &config{})
	if err := restored.restore(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatal(err)
	}

	gotBytes, gotKeys := size(t, restored)
	if gotBytes != wantBytes || gotKeys != wantKeys {
		t.Errorf("a restored replica reports %d bytes/%d keys, the original %d/%d",
			gotBytes, gotKeys, wantBytes, wantKeys)
	}
	checkAgrees(t, restored, "a restore")

	// And it keeps counting from there rather than starting over.
	if _, err := applyAt(t, restored, 100, &command{
		Op: opUpsert, Collection: "c0", Key: "after", Value: jsonValue(t, "restore"),
	}); err != nil {
		t.Fatal(err)
	}
	checkAgrees(t, restored, "a write after a restore")
}

// TestStateSize_CountsTheKeyAsWellAsTheValue pins what the number means, so
// that a change to it is a deliberate one.
func TestStateSize_CountsTheKeyAsWellAsTheValue(t *testing.T) {
	s := newTestStore(t, &config{})
	value := jsonValue(t, "v")
	key := "a-key-of-known-length"

	if _, err := applyAt(t, s, 1, &command{
		Op: opCreate, Collection: "c", Key: key, Value: value,
	}); err != nil {
		t.Fatal(err)
	}
	want := int64(len(key) + len(value))
	if got, _ := size(t, s); got != want {
		t.Errorf("a store holding one %d-byte key and one %d-byte value reports %d, want %d",
			len(key), len(value), got, want)
	}
}
