package raft

// Internal tests for clientLRU. These live in package raft (not raft_test) so
// they can access the unexported clientLRU type directly.

import (
	"bytes"
	"testing"
)

func TestClientLRU_GetMiss(t *testing.T) {
	c := newClientLRU(10)
	if _, ok := c.get("absent"); ok {
		t.Fatal("expected miss for key not in LRU")
	}
}

func TestClientLRU_PutAndGet(t *testing.T) {
	c := newClientLRU(10)
	c.put("a", clientEntry{seqNum: 1, result: []byte("r1")})

	ce, ok := c.get("a")
	if !ok {
		t.Fatal("expected hit for key 'a'")
	}
	if ce.seqNum != 1 || string(ce.result) != "r1" {
		t.Fatalf("got %+v, want seqNum=1 result=r1", ce)
	}
}

func TestClientLRU_UpdateMovesFront(t *testing.T) {
	c := newClientLRU(10)
	c.put("a", clientEntry{seqNum: 1})
	c.put("b", clientEntry{seqNum: 2})

	// Update a — it should move to the front (MRU).
	c.put("a", clientEntry{seqNum: 3})
	front := c.l.Front().Value.(*lruItem)
	if front.id != "a" {
		t.Fatalf("expected 'a' at front after update, got %q", front.id)
	}
}

// TestClientLRU_GetDoesNotReorder pins the property that makes this table safe
// to replicate: a lookup must not change eviction order.
//
// Every replica builds the table from the same log entries in the same order,
// so write order is identical everywhere. Lookups are not: only the leader
// serves them. If a lookup reordered the table, the leader would evict a
// different entry from its followers, and a client retry would then be
// re-executed on some replicas and skipped on others.
func TestClientLRU_GetDoesNotReorder(t *testing.T) {
	c := newClientLRU(10)
	c.put("a", clientEntry{seqNum: 1})
	c.put("b", clientEntry{seqNum: 2})

	c.get("a")

	front := c.l.Front().Value.(*lruItem)
	if front.id != "b" {
		t.Fatalf("front is %q after reading 'a'; a lookup must not change eviction order", front.id)
	}
}

func TestClientLRU_EvictsLRUOnOverflow(t *testing.T) {
	c := newClientLRU(2)
	c.put("a", clientEntry{seqNum: 1})
	c.put("b", clientEntry{seqNum: 2})
	// Adding 'c' should evict 'a' (LRU).
	c.put("c", clientEntry{seqNum: 3})

	if c.l.Len() != 2 {
		t.Fatalf("expected 2 entries after eviction, got %d", c.l.Len())
	}
	if _, ok := c.get("a"); ok {
		t.Fatal("'a' should have been evicted")
	}
	if _, ok := c.get("b"); !ok {
		t.Fatal("'b' should still be present")
	}
	if _, ok := c.get("c"); !ok {
		t.Fatal("'c' should be present")
	}
}

// TestClientLRU_GetDoesNotRescueFromEviction is the same invariant seen through
// eviction: reading an entry must not save it, because a replica that never
// served that read would evict it anyway.
func TestClientLRU_GetDoesNotRescueFromEviction(t *testing.T) {
	c := newClientLRU(2)
	c.put("a", clientEntry{seqNum: 1})
	c.put("b", clientEntry{seqNum: 2})
	c.get("a")

	c.put("c", clientEntry{seqNum: 3})

	if _, ok := c.get("a"); ok {
		t.Error("'a' survived because it was read; eviction must depend only on writes")
	}
	if _, ok := c.get("b"); !ok {
		t.Error("'b' was evicted; the least recently written entry is 'a'")
	}
}

// TestClientLRU_LoadFromPreservesOrderAndCap asserts that a table restored from
// a snapshot keeps the eviction order it was saved with, and honours the cap of
// the node restoring it.
func TestClientLRU_LoadFromPreservesOrderAndCap(t *testing.T) {
	src := []clientRecord{
		{id: "newest", ce: clientEntry{seqNum: 3}},
		{id: "middle", ce: clientEntry{seqNum: 2}},
		{id: "oldest", ce: clientEntry{seqNum: 1}},
	}

	c := newClientLRU(3)
	c.loadFrom(src)
	c.put("fresh", clientEntry{seqNum: 4})
	if _, ok := c.get("oldest"); ok {
		t.Error("the entry restored as oldest was not the one evicted")
	}
	if _, ok := c.get("newest"); !ok {
		t.Error("the entry restored as newest was evicted")
	}

	// A node with a smaller cap keeps the newest entries, dropping the tail.
	small := newClientLRU(2)
	small.loadFrom(src)
	if small.len() != 2 {
		t.Fatalf("restored %d entries into a table capped at 2", small.len())
	}
	if _, ok := small.get("oldest"); ok {
		t.Error("restoring into a smaller table kept the oldest entry")
	}
}

func TestClientLRU_UnlimitedCap(t *testing.T) {
	c := newClientLRU(0) // cap=0 means unlimited
	for i := range 10_000 {
		c.put(NodeID(string(rune('a'+i%26))+string(rune(i))), clientEntry{seqNum: uint64(i)})
	}
	if c.l.Len() != 10_000 {
		t.Fatalf("expected 10000 entries with unlimited cap, got %d", c.l.Len())
	}
}

func TestClientLRU_Records(t *testing.T) {
	c := newClientLRU(10)
	c.put("x", clientEntry{seqNum: 7, result: []byte("res")})
	c.put("y", clientEntry{seqNum: 8})

	recs := c.records()
	if len(recs) != 2 {
		t.Fatalf("records: expected 2 entries, got %d", len(recs))
	}
	// Most recently updated first.
	if recs[0].id != "y" || recs[1].id != "x" {
		t.Fatalf("records: order = %q, %q; want y, x (most recently updated first)",
			recs[0].id, recs[1].id)
	}
	if recs[1].ce.seqNum != 7 || string(recs[1].ce.result) != "res" {
		t.Fatalf("records: wrong entry for 'x': %+v", recs[1].ce)
	}
}

func TestClientLRU_LoadFrom(t *testing.T) {
	src := []clientRecord{
		{id: "a", ce: clientEntry{seqNum: 1, result: []byte("r1")}},
		{id: "b", ce: clientEntry{seqNum: 2, result: []byte("r2")}},
	}
	c := newClientLRU(10)
	c.loadFrom(src)

	if c.l.Len() != 2 {
		t.Fatalf("expected 2 entries after loadFrom, got %d", c.l.Len())
	}
	for _, rec := range src {
		id, want := rec.id, rec.ce
		got, ok := c.get(id)
		if !ok {
			t.Fatalf("loadFrom: key %q missing", id)
		}
		if got.seqNum != want.seqNum || !bytes.Equal(got.result, want.result) {
			t.Fatalf("loadFrom: key %q: got %+v, want %+v", id, got, want)
		}
	}
}

func TestClientLRU_LoadFromReplacesExisting(t *testing.T) {
	c := newClientLRU(10)
	c.put("stale", clientEntry{seqNum: 99})

	c.loadFrom([]clientRecord{{id: "fresh", ce: clientEntry{seqNum: 1}}})

	if _, ok := c.get("stale"); ok {
		t.Fatal("loadFrom should replace existing entries; 'stale' should be gone")
	}
	if _, ok := c.get("fresh"); !ok {
		t.Fatal("'fresh' should be present after loadFrom")
	}
}
