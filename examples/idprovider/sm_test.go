package main

// Unit tests for IDSM. These run without any Raft cluster — they call
// Apply/Snapshot/Restore directly, which is sufficient to verify all SM logic.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/brunoga/raft"
)

// helpers

func newSM() *IDSM { return &IDSM{domains: make(map[string]uint64)} }

func applyCmd(t *testing.T, sm *IDSM, cmd domainCmd) ([]byte, error) {
	t.Helper()
	b, err := json.Marshal(cmd)
	if err != nil {
		t.Fatalf("marshal domainCmd: %v", err)
	}
	return sm.Apply(context.Background(), raft.LogEntry{Command: b})
}

func mustApply(t *testing.T, sm *IDSM, cmd domainCmd) []byte {
	t.Helper()
	b, err := applyCmd(t, sm, cmd)
	if err != nil {
		t.Fatalf("Apply(%+v): %v", cmd, err)
	}
	return b
}

// --- Create ---

func TestIDSM_CreateDomain(t *testing.T) {
	sm := newSM()
	mustApply(t, sm, domainCmd{Op: opCreate, Domain: "orders"})

	v, ok := sm.Current("orders")
	if !ok {
		t.Fatal("domain 'orders' should exist after create")
	}
	if v != 0 {
		t.Errorf("initial counter = %d, want 0", v)
	}
}

func TestIDSM_CreateDomainIdempotent(t *testing.T) {
	sm := newSM()
	mustApply(t, sm, domainCmd{Op: opCreate, Domain: "x"})
	mustApply(t, sm, domainCmd{Op: opAlloc, Domain: "x", Count: 5})

	// Second create must not reset the counter.
	mustApply(t, sm, domainCmd{Op: opCreate, Domain: "x"})

	v, _ := sm.Current("x")
	if v != 5 {
		t.Errorf("counter after idempotent create = %d, want 5", v)
	}
}

func TestIDSM_CreateEmptyDomainName(t *testing.T) {
	sm := newSM()
	_, err := applyCmd(t, sm, domainCmd{Op: opCreate, Domain: ""})
	if err == nil {
		t.Fatal("expected error for empty domain name")
	}
}

// --- Delete ---

func TestIDSM_DeleteDomain(t *testing.T) {
	sm := newSM()
	mustApply(t, sm, domainCmd{Op: opCreate, Domain: "tmp"})
	mustApply(t, sm, domainCmd{Op: opDelete, Domain: "tmp"})

	if _, ok := sm.Current("tmp"); ok {
		t.Error("domain 'tmp' should not exist after delete")
	}
}

func TestIDSM_DeleteNonExistent(t *testing.T) {
	sm := newSM()
	_, err := applyCmd(t, sm, domainCmd{Op: opDelete, Domain: "absent"})
	if !errors.Is(err, ErrDomainNotFound) {
		t.Fatalf("expected ErrDomainNotFound, got %v", err)
	}
}

func TestIDSM_DeleteRemovedFromDomainsList(t *testing.T) {
	sm := newSM()
	mustApply(t, sm, domainCmd{Op: opCreate, Domain: "a"})
	mustApply(t, sm, domainCmd{Op: opCreate, Domain: "b"})
	mustApply(t, sm, domainCmd{Op: opDelete, Domain: "a"})

	all := sm.Domains()
	if _, ok := all["a"]; ok {
		t.Error("'a' should be absent from Domains() after delete")
	}
	if _, ok := all["b"]; !ok {
		t.Error("'b' should still be in Domains()")
	}
}

// --- Alloc ---

func TestIDSM_AllocBasic(t *testing.T) {
	sm := newSM()
	mustApply(t, sm, domainCmd{Op: opCreate, Domain: "ids"})

	raw := mustApply(t, sm, domainCmd{Op: opAlloc, Domain: "ids", Count: 1})
	var r allocResult
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatalf("decode allocResult: %v", err)
	}
	if r.Start != 1 {
		t.Errorf("start = %d, want 1", r.Start)
	}
	if r.Count != 1 {
		t.Errorf("count = %d, want 1", r.Count)
	}
}

func TestIDSM_AllocBatch(t *testing.T) {
	sm := newSM()
	mustApply(t, sm, domainCmd{Op: opCreate, Domain: "batch"})

	raw := mustApply(t, sm, domainCmd{Op: opAlloc, Domain: "batch", Count: 100})
	var r allocResult
	json.Unmarshal(raw, &r)
	if r.Start != 1 || r.Count != 100 {
		t.Errorf("alloc(100) = {%d, %d}, want {1, 100}", r.Start, r.Count)
	}
}

func TestIDSM_AllocSequentialRangesAreContiguous(t *testing.T) {
	sm := newSM()
	mustApply(t, sm, domainCmd{Op: opCreate, Domain: "seq"})

	var r1, r2 allocResult
	json.Unmarshal(mustApply(t, sm, domainCmd{Op: opAlloc, Domain: "seq", Count: 10}), &r1)
	json.Unmarshal(mustApply(t, sm, domainCmd{Op: opAlloc, Domain: "seq", Count: 5}), &r2)

	if r1.Start+r1.Count != r2.Start {
		t.Errorf("ranges not contiguous: [%d,%d) then [%d,%d)",
			r1.Start, r1.Start+r1.Count, r2.Start, r2.Start+r2.Count)
	}
}

func TestIDSM_AllocNonExistentDomain(t *testing.T) {
	sm := newSM()
	_, err := applyCmd(t, sm, domainCmd{Op: opAlloc, Domain: "ghost", Count: 1})
	if !errors.Is(err, ErrDomainNotFound) {
		t.Fatalf("expected ErrDomainNotFound, got %v", err)
	}
}

func TestIDSM_AllocZeroCount(t *testing.T) {
	sm := newSM()
	mustApply(t, sm, domainCmd{Op: opCreate, Domain: "z"})
	_, err := applyCmd(t, sm, domainCmd{Op: opAlloc, Domain: "z", Count: 0})
	if err == nil {
		t.Fatal("expected error for count=0")
	}
}

// --- Multi-domain isolation ---

func TestIDSM_MultiDomainIsolation(t *testing.T) {
	sm := newSM()
	mustApply(t, sm, domainCmd{Op: opCreate, Domain: "alpha"})
	mustApply(t, sm, domainCmd{Op: opCreate, Domain: "beta"})

	// Alloc different amounts; counters must not cross-contaminate.
	mustApply(t, sm, domainCmd{Op: opAlloc, Domain: "alpha", Count: 100})
	mustApply(t, sm, domainCmd{Op: opAlloc, Domain: "beta", Count: 7})

	a, _ := sm.Current("alpha")
	b, _ := sm.Current("beta")
	if a != 100 {
		t.Errorf("alpha = %d, want 100", a)
	}
	if b != 7 {
		t.Errorf("beta = %d, want 7", b)
	}
}

// --- Domains ---

func TestIDSM_DomainsEmpty(t *testing.T) {
	sm := newSM()
	if d := sm.Domains(); len(d) != 0 {
		t.Errorf("Domains() on empty SM = %v, want empty map", d)
	}
}

func TestIDSM_DomainsReturnsAllAndIsCopy(t *testing.T) {
	sm := newSM()
	mustApply(t, sm, domainCmd{Op: opCreate, Domain: "p"})
	mustApply(t, sm, domainCmd{Op: opCreate, Domain: "q"})
	mustApply(t, sm, domainCmd{Op: opAlloc, Domain: "p", Count: 3})

	d := sm.Domains()
	if len(d) != 2 {
		t.Fatalf("Domains() len = %d, want 2", len(d))
	}
	if d["p"] != 3 {
		t.Errorf("p = %d, want 3", d["p"])
	}
	if d["q"] != 0 {
		t.Errorf("q = %d, want 0", d["q"])
	}

	// Mutating the returned map must not affect the SM.
	d["p"] = 9999
	if v, _ := sm.Current("p"); v != 3 {
		t.Error("Domains() returned a live reference, not a copy")
	}
}

// --- Snapshot / Restore ---

func TestIDSM_SnapshotRestore(t *testing.T) {
	src := newSM()
	mustApply(t, src, domainCmd{Op: opCreate, Domain: "a"})
	mustApply(t, src, domainCmd{Op: opCreate, Domain: "b"})
	mustApply(t, src, domainCmd{Op: opAlloc, Domain: "a", Count: 42})
	mustApply(t, src, domainCmd{Op: opAlloc, Domain: "b", Count: 7})

	var buf bytes.Buffer
	if err := src.Snapshot(context.Background(), &buf); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	dst := newSM()
	if err := dst.Restore(context.Background(), raft.SnapshotMeta{}, &buf); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	for domain, want := range map[string]uint64{"a": 42, "b": 7} {
		got, ok := dst.Current(domain)
		if !ok {
			t.Fatalf("domain %q missing after restore", domain)
		}
		if got != want {
			t.Errorf("domain %q: counter = %d, want %d", domain, got, want)
		}
	}
}

func TestIDSM_RestoreClearsExistingState(t *testing.T) {
	sm := newSM()
	mustApply(t, sm, domainCmd{Op: opCreate, Domain: "stale"})

	// Restore from a snapshot that contains only "fresh".
	fresh := newSM()
	mustApply(t, fresh, domainCmd{Op: opCreate, Domain: "fresh"})
	var buf bytes.Buffer
	_ = fresh.Snapshot(context.Background(), &buf)

	if err := sm.Restore(context.Background(), raft.SnapshotMeta{}, &buf); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	if _, ok := sm.Current("stale"); ok {
		t.Error("'stale' should be gone after Restore")
	}
	if _, ok := sm.Current("fresh"); !ok {
		t.Error("'fresh' should exist after Restore")
	}
}

func TestIDSM_RestoreEmptySnapshot(t *testing.T) {
	// An empty SM snapshots to "{}"; restoring that should yield an empty SM.
	sm := newSM()
	var buf bytes.Buffer
	_ = sm.Snapshot(context.Background(), &buf)

	dst := newSM()
	mustApply(t, dst, domainCmd{Op: opCreate, Domain: "old"})
	if err := dst.Restore(context.Background(), raft.SnapshotMeta{}, &buf); err != nil {
		t.Fatalf("Restore from empty: %v", err)
	}
	if _, ok := dst.Current("old"); ok {
		t.Error("'old' should be gone after restoring empty snapshot")
	}
}
