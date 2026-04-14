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

func TestClientLRU_GetMovesFront(t *testing.T) {
	c := newClientLRU(10)
	c.put("a", clientEntry{seqNum: 1})
	c.put("b", clientEntry{seqNum: 2})
	// 'b' is MRU now. Read 'a' — should move it to front.
	c.get("a")
	front := c.l.Front().Value.(*lruItem)
	if front.id != "a" {
		t.Fatalf("expected 'a' at front after get, got %q", front.id)
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

func TestClientLRU_GetRescuesFromEviction(t *testing.T) {
	// Accessing 'a' before inserting 'c' should push 'b' to the eviction slot.
	c := newClientLRU(2)
	c.put("a", clientEntry{seqNum: 1})
	c.put("b", clientEntry{seqNum: 2})
	c.get("a") // 'a' → front; 'b' → back (LRU)

	c.put("c", clientEntry{seqNum: 3}) // should evict 'b'

	if _, ok := c.get("a"); !ok {
		t.Fatal("'a' should survive (it was accessed recently)")
	}
	if _, ok := c.get("b"); ok {
		t.Fatal("'b' should have been evicted (it was LRU)")
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

func TestClientLRU_ToMap(t *testing.T) {
	c := newClientLRU(10)
	c.put("x", clientEntry{seqNum: 7, result: []byte("res")})
	c.put("y", clientEntry{seqNum: 8})

	m := c.toMap()
	if len(m) != 2 {
		t.Fatalf("toMap: expected 2 entries, got %d", len(m))
	}
	if m["x"].seqNum != 7 || string(m["x"].result) != "res" {
		t.Fatalf("toMap: wrong entry for 'x': %+v", m["x"])
	}
}

func TestClientLRU_LoadFrom(t *testing.T) {
	src := map[NodeID]clientEntry{
		"a": {seqNum: 1, result: []byte("r1")},
		"b": {seqNum: 2, result: []byte("r2")},
	}
	c := newClientLRU(10)
	c.loadFrom(src)

	if c.l.Len() != 2 {
		t.Fatalf("expected 2 entries after loadFrom, got %d", c.l.Len())
	}
	for id, want := range src {
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

	c.loadFrom(map[NodeID]clientEntry{"fresh": {seqNum: 1}})

	if _, ok := c.get("stale"); ok {
		t.Fatal("loadFrom should replace existing entries; 'stale' should be gone")
	}
	if _, ok := c.get("fresh"); !ok {
		t.Fatal("'fresh' should be present after loadFrom")
	}
}
