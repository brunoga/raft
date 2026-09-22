package easyraft

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
)

// grantLease applies a grant at the index given, which becomes the lease ID.
func grantLease(t *testing.T, s *Store, index, ttlMillis uint64, nowMillis int64) LeaseID {
	t.Helper()
	raw, err := applyAt(t, s, index, &command{
		Op:             opLeaseGrant,
		LeaseTTLMillis: ttlMillis,
		LeaseNowMillis: nowMillis,
	})
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if len(raw) == 0 {
		t.Fatal("grant returned no lease id")
	}
	var id uint64
	if err := json.Unmarshal(raw, &id); err != nil {
		t.Fatalf("decode lease id: %v", err)
	}
	if id != index {
		t.Fatalf("lease id is %d, want the granting entry's index %d", id, index)
	}
	return LeaseID(id)
}

func keyExists(t *testing.T, s *Store, collection, key string) bool {
	t.Helper()
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.collections[collection][key]
	return ok
}

// TestLease_RevokeDeletesEveryKeyItHolds is the whole point of a lease: the
// keys go when it does, in one entry, across every collection it touched.
func TestLease_RevokeDeletesEveryKeyItHolds(t *testing.T) {
	s := newTestStore(t, &config{})
	lease := grantLease(t, s, 5, 30_000, 1_000)

	for i, spec := range [][2]string{{"services", "a"}, {"services", "b"}, {"routes", "r1"}} {
		if _, err := applyAt(t, s, uint64(10+i), &command{
			Op: opUpsert, Collection: spec[0], Key: spec[1],
			Value: jsonValue(t, spec[1]), Lease: uint64(lease),
		}); err != nil {
			t.Fatalf("write %v: %v", spec, err)
		}
	}
	// One key with no lease, which must survive.
	if _, err := applyAt(t, s, 20, &command{
		Op: opUpsert, Collection: "services", Key: "permanent", Value: jsonValue(t, "p"),
	}); err != nil {
		t.Fatal(err)
	}

	info, err := s.Lease(lease)
	if err != nil {
		t.Fatalf("Lease: %v", err)
	}
	if len(info.Keys) != 3 {
		t.Fatalf("lease holds %d keys, want 3: %v", len(info.Keys), info.Keys)
	}

	if _, err := applyAt(t, s, 25, &command{Op: opLeaseRevoke, Lease: uint64(lease)}); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	for _, spec := range [][2]string{{"services", "a"}, {"services", "b"}, {"routes", "r1"}} {
		if keyExists(t, s, spec[0], spec[1]) {
			t.Errorf("%v survived the revocation of its lease", spec)
		}
	}
	if !keyExists(t, s, "services", "permanent") {
		t.Error("a key with no lease was deleted with the lease")
	}
	if _, err := s.Lease(lease); !errors.Is(err, ErrLeaseNotFound) {
		t.Errorf("Lease after revoke: %v, want ErrLeaseNotFound", err)
	}

	// Revoking again is a no-op rather than a failed entry: a leader change
	// can put two revocations for one lease in the log.
	if _, err := applyAt(t, s, 26, &command{Op: opLeaseRevoke, Lease: uint64(lease)}); err != nil {
		t.Errorf("revoking an already-revoked lease: %v", err)
	}
}

// TestLease_WriteToAMissingLeaseWritesNothing pins that a key is never left
// behind with nothing to remove it.
func TestLease_WriteToAMissingLeaseWritesNothing(t *testing.T) {
	s := newTestStore(t, &config{})

	_, err := applyAt(t, s, 4, &command{
		Op: opUpsert, Collection: "c", Key: "k", Value: jsonValue(t, 1), Lease: 999,
	})
	if !errors.Is(err, ErrLeaseNotFound) {
		t.Fatalf("write under an unknown lease: %v, want ErrLeaseNotFound", err)
	}
	if keyExists(t, s, "c", "k") {
		t.Error("the key was written anyway")
	}
}

// TestLease_AKeyBelongsToOneLease covers moving a key between leases and
// detaching it. Two leases each believing they may delete a key would let the
// second to expire remove a value the first had already replaced.
func TestLease_AKeyBelongsToOneLease(t *testing.T) {
	s := newTestStore(t, &config{})
	first := grantLease(t, s, 2, 30_000, 0)
	second := grantLease(t, s, 3, 30_000, 0)

	write := func(index uint64, lease LeaseID) {
		t.Helper()
		if _, err := applyAt(t, s, index, &command{
			Op: opUpsert, Collection: "c", Key: "k", Value: jsonValue(t, index), Lease: uint64(lease),
		}); err != nil {
			t.Fatalf("write at %d: %v", index, err)
		}
	}

	write(10, first)
	if got := AddCollection[uint64](s, "c").LeaseOf("k"); got != first {
		t.Fatalf("key is on lease %d, want %d", got, first)
	}

	write(11, second)
	if got := AddCollection[uint64](s, "c").LeaseOf("k"); got != second {
		t.Fatalf("after moving, key is on lease %d, want %d", got, second)
	}
	info, err := s.Lease(first)
	if err != nil {
		t.Fatal(err)
	}
	if len(info.Keys) != 0 {
		t.Errorf("the first lease still holds %v", info.Keys)
	}

	// Revoking the lease the key left must not take the key with it.
	if _, err := applyAt(t, s, 12, &command{Op: opLeaseRevoke, Lease: uint64(first)}); err != nil {
		t.Fatal(err)
	}
	if !keyExists(t, s, "c", "k") {
		t.Fatal("revoking a lease the key had left deleted the key")
	}

	// A write with no lease detaches, so the remaining lease cannot take it.
	write(13, 0)
	if got := AddCollection[uint64](s, "c").LeaseOf("k"); got != 0 {
		t.Errorf("after a write with no lease the key is on lease %d", got)
	}
	if _, err := applyAt(t, s, 14, &command{Op: opLeaseRevoke, Lease: uint64(second)}); err != nil {
		t.Fatal(err)
	}
	if !keyExists(t, s, "c", "k") {
		t.Error("a detached key was deleted with the lease that used to hold it")
	}
}

// TestLease_DeleteReleasesTheKey checks the lease does not keep a record of a
// key that was deleted under it, which would make a later revocation delete
// whatever was written in its place.
func TestLease_DeleteReleasesTheKey(t *testing.T) {
	s := newTestStore(t, &config{})
	lease := grantLease(t, s, 2, 30_000, 0)

	if _, err := applyAt(t, s, 10, &command{
		Op: opUpsert, Collection: "c", Key: "k", Value: jsonValue(t, 1), Lease: uint64(lease),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := applyAt(t, s, 11, &command{Op: opDelete, Collection: "c", Key: "k"}); err != nil {
		t.Fatal(err)
	}
	info, err := s.Lease(lease)
	if err != nil {
		t.Fatal(err)
	}
	if len(info.Keys) != 0 {
		t.Fatalf("the lease still holds %v after the key was deleted", info.Keys)
	}

	// Somebody else takes the key.
	if _, err := applyAt(t, s, 12, &command{
		Op: opCreate, Collection: "c", Key: "k", Value: jsonValue(t, 2),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := applyAt(t, s, 13, &command{Op: opLeaseRevoke, Lease: uint64(lease)}); err != nil {
		t.Fatal(err)
	}
	if !keyExists(t, s, "c", "k") {
		t.Error("revoking the lease deleted a key written after the leased one was removed")
	}
}

// TestLease_KeepAliveMovesTheDeadline covers renewal and what the deadline is
// computed from: the clock reading in the command, never one read in Apply.
func TestLease_KeepAliveMovesTheDeadline(t *testing.T) {
	s := newTestStore(t, &config{})
	lease := grantLease(t, s, 2, 10_000, 1_000)

	info, infoErr := s.Lease(lease)
	if infoErr != nil {
		t.Fatal(infoErr)
	}
	if got := info.ExpiresAt.UnixMilli(); got != 11_000 {
		t.Fatalf("deadline is %d, want the command's 1000 plus the TTL", got)
	}

	if _, err := applyAt(t, s, 3, &command{
		Op: opLeaseKeepAlive, Lease: uint64(lease), LeaseNowMillis: 5_000,
	}); err != nil {
		t.Fatalf("keepalive: %v", err)
	}
	info, renewedErr := s.Lease(lease)
	if renewedErr != nil {
		t.Fatal(renewedErr)
	}
	if got := info.ExpiresAt.UnixMilli(); got != 15_000 {
		t.Errorf("deadline after renewal is %d, want 15000", got)
	}

	if _, err := applyAt(t, s, 4, &command{
		Op: opLeaseKeepAlive, Lease: 777, LeaseNowMillis: 5_000,
	}); !errors.Is(err, ErrLeaseNotFound) {
		t.Errorf("renewing an unknown lease: %v, want ErrLeaseNotFound", err)
	}
}

// TestLease_DueLeasesPicksExactlyTheExpiredOnes checks what the sweeper acts
// on, without running the sweeper's clock.
func TestLease_DueLeasesPicksExactlyTheExpiredOnes(t *testing.T) {
	s := newTestStore(t, &config{})
	early := grantLease(t, s, 2, 1_000, 0)  // due at 1000
	late := grantLease(t, s, 3, 10_000, 0)  // due at 10000
	_ = grantLease(t, s, 4, 100_000, 5_000) // due at 105000

	if got := s.dueLeases(500); len(got) != 0 {
		t.Errorf("leases due at 500: %v, want none", got)
	}
	if got := s.dueLeases(1_000); len(got) != 1 || got[0] != uint64(early) {
		t.Errorf("leases due at 1000: %v, want just %d", got, early)
	}
	got := s.dueLeases(20_000)
	if len(got) != 2 || got[0] != uint64(early) || got[1] != uint64(late) {
		t.Errorf("leases due at 20000: %v, want %d and %d in that order", got, early, late)
	}
}

// TestLease_RefusedInsideATransaction pins that lease bookkeeping cannot be
// half-applied: a batch can roll back its writes, but not a grant.
func TestLease_RefusedInsideATransaction(t *testing.T) {
	s := newTestStore(t, &config{})
	_, err := applyAt(t, s, 5, &command{Op: opBatch, Batch: []command{
		{Op: opLeaseGrant, LeaseTTLMillis: 1_000, LeaseNowMillis: 0},
	}})
	if err == nil {
		t.Fatal("a lease grant inside a transaction was accepted")
	}
	if len(s.leases) != 0 {
		t.Errorf("the refused grant left %d leases behind", len(s.leases))
	}
}

// TestLease_RollbackRestoresAttachment covers a batch that attaches a key to
// a lease and then fails. The lease must not keep a key the batch did not
// write, or revoking it would delete a value it never owned.
func TestLease_RollbackRestoresAttachment(t *testing.T) {
	s := newTestStore(t, &config{})
	lease := grantLease(t, s, 2, 30_000, 0)

	if _, err := applyAt(t, s, 10, &command{
		Op: opUpsert, Collection: "c", Key: "keep", Value: jsonValue(t, "original"),
	}); err != nil {
		t.Fatal(err)
	}

	_, err := applyAt(t, s, 11, &command{Op: opBatch, Batch: []command{
		{Op: opUpsert, Collection: "c", Key: "keep", Value: jsonValue(t, "leased"), Lease: uint64(lease)},
		{Op: opUpdate, Collection: "c", Key: "absent", Value: jsonValue(t, "x")},
	}})
	if !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("batch: %v, want ErrKeyNotFound", err)
	}

	if got := AddCollection[string](s, "c").LeaseOf("keep"); got != 0 {
		t.Errorf("a rolled-back write left the key on lease %d", got)
	}
	info, err := s.Lease(lease)
	if err != nil {
		t.Fatal(err)
	}
	if len(info.Keys) != 0 {
		t.Errorf("the lease kept %v from a rolled-back batch", info.Keys)
	}
	if _, err := applyAt(t, s, 12, &command{Op: opLeaseRevoke, Lease: uint64(lease)}); err != nil {
		t.Fatal(err)
	}
	if !keyExists(t, s, "c", "keep") {
		t.Error("revoking the lease deleted a key whose attachment was rolled back")
	}
}

// TestLease_SurviveASnapshot checks that a replica restored from a snapshot
// expires the same keys at the same deadlines as the one that wrote it.
func TestLease_SurviveASnapshot(t *testing.T) {
	s := newTestStore(t, &config{})
	first := grantLease(t, s, 2, 30_000, 1_000)
	second := grantLease(t, s, 3, 60_000, 1_000)
	writes := []struct {
		index      uint64
		collection string
		key        string
		lease      LeaseID
	}{
		{10, "services", "a", first},
		{11, "services", "b", second},
		{12, "routes", "r", first},
		{13, "routes", "plain", 0},
	}
	for _, w := range writes {
		if _, err := applyAt(t, s, w.index, &command{
			Op: opUpsert, Collection: w.collection, Key: w.key,
			Value: jsonValue(t, w.key), Lease: uint64(w.lease),
		}); err != nil {
			t.Fatalf("write %s: %v", w.key, err)
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

	leases := restored.Leases()
	if len(leases) != 2 {
		t.Fatalf("restored %d leases, want 2", len(leases))
	}
	if leases[0].ID != first || leases[0].ExpiresAt.UnixMilli() != 31_000 {
		t.Errorf("first lease restored as %+v", leases[0])
	}
	if leases[1].ID != second || leases[1].TTL.Milliseconds() != 60_000 {
		t.Errorf("second lease restored as %+v", leases[1])
	}
	if got := AddCollection[string](restored, "routes").LeaseOf("r"); got != first {
		t.Errorf("restored key is on lease %d, want %d", got, first)
	}

	// The restored store removes exactly what the original would have.
	if _, err := applyAt(t, restored, 30, &command{Op: opLeaseRevoke, Lease: uint64(first)}); err != nil {
		t.Fatal(err)
	}
	for _, spec := range [][2]string{{"services", "a"}, {"routes", "r"}} {
		if keyExists(t, restored, spec[0], spec[1]) {
			t.Errorf("%v survived revocation on a restored store", spec)
		}
	}
	for _, spec := range [][2]string{{"services", "b"}, {"routes", "plain"}} {
		if !keyExists(t, restored, spec[0], spec[1]) {
			t.Errorf("%v was deleted on a restored store", spec)
		}
	}

	// Two snapshots of the same state are still identical.
	var second2 bytes.Buffer
	if err := s.snapshot(&second2); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf.Bytes(), second2.Bytes()) {
		t.Error("two snapshots of the same leases differ")
	}
}

// TestLease_SnapshotWithoutLeasesStillRestores is the upgrade path: a
// snapshot written before leases existed has no lease key at all.
func TestLease_SnapshotWithoutLeasesStillRestores(t *testing.T) {
	old := []byte(`{"coll":{"a":"one"},"__easyraft_revisions__":{"coll":{"a":7}},"__easyraft_revision__":7}` + "\n")
	s := newTestStore(t, &config{})
	if err := s.restore(bytes.NewReader(old)); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := len(s.Leases()); got != 0 {
		t.Errorf("restored %d leases from a snapshot that has none", got)
	}
	// And the store still works: a lease granted now attaches normally.
	lease := grantLease(t, s, 20, 1_000, 0)
	if _, err := applyAt(t, s, 21, &command{
		Op: opUpsert, Collection: "coll", Key: "b", Value: jsonValue(t, "two"), Lease: uint64(lease),
	}); err != nil {
		t.Fatalf("write under a new lease: %v", err)
	}
	if _, err := applyAt(t, s, 22, &command{Op: opLeaseRevoke, Lease: uint64(lease)}); err != nil {
		t.Fatal(err)
	}
	if keyExists(t, s, "coll", "b") {
		t.Error("the leased key survived revocation")
	}
	if !keyExists(t, s, "coll", "a") {
		t.Error("a key restored from the old snapshot was deleted")
	}
}

// TestLease_RevocationEmitsDeleteEvents checks a watcher sees the keys go.
// Silent removal would leave every subscriber's copy of the state wrong.
func TestLease_RevocationEmitsDeleteEvents(t *testing.T) {
	s := newTestStore(t, &config{})
	seen := make(chan string, 8)
	s.onChangeFns["services"] = func(ev rawChangeEvent) {
		if ev.Deleted {
			seen <- ev.Key
		}
	}

	lease := grantLease(t, s, 2, 30_000, 0)
	for i, key := range []string{"b", "a"} {
		if _, err := applyAt(t, s, uint64(10+i), &command{
			Op: opUpsert, Collection: "services", Key: key,
			Value: jsonValue(t, key), Lease: uint64(lease),
		}); err != nil {
			t.Fatal(err)
		}
	}

	s.mu.Lock()
	s.applyRev = 20
	s.pendingEvents = nil
	if _, err := s.applyLeaseCommand(&command{Op: opLeaseRevoke, Lease: uint64(lease)}); err != nil {
		s.mu.Unlock()
		t.Fatalf("revoke: %v", err)
	}
	events := s.pendingEvents
	s.mu.Unlock()

	if len(events) != 2 {
		t.Fatalf("revocation produced %d events, want 2", len(events))
	}
	// Sorted, so every replica emits them in the same order.
	if events[0].key != "a" || events[1].key != "b" {
		t.Errorf("events are %q then %q, want a then b", events[0].key, events[1].key)
	}
	for _, ev := range events {
		if !ev.deleted {
			t.Errorf("event for %q is not a delete", ev.key)
		}
	}
}
