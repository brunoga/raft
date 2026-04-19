package main

// Unit tests for KvSM. These run without any Raft cluster — they call
// Apply/Snapshot/Restore directly, which is sufficient to exercise all SM logic.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/brunoga/raft"
)

func newKvSM() *KvSM { return &KvSM{data: make(map[string]string)} }

func applyKv(t *testing.T, sm *KvSM, cmd kvCmd) error {
	t.Helper()
	b, err := json.Marshal(cmd)
	if err != nil {
		t.Fatalf("marshal kvCmd: %v", err)
	}
	_, err = sm.Apply(context.Background(), raft.LogEntry{Command: b})
	return err
}

// --- Set ---

func TestKvSM_Set(t *testing.T) {
	sm := newKvSM()
	if err := applyKv(t, sm, kvCmd{Op: opSet, Key: "k", Value: "v"}); err != nil {
		t.Fatalf("set: %v", err)
	}
	got, ok := sm.Get("k")
	if !ok || got != "v" {
		t.Errorf("Get(k) = (%q, %v), want (v, true)", got, ok)
	}
}

func TestKvSM_SetOverwrites(t *testing.T) {
	sm := newKvSM()
	applyKv(t, sm, kvCmd{Op: opSet, Key: "k", Value: "first"})
	applyKv(t, sm, kvCmd{Op: opSet, Key: "k", Value: "second"})
	if v, _ := sm.Get("k"); v != "second" {
		t.Errorf("overwrite: Get = %q, want second", v)
	}
}

// --- Delete ---

func TestKvSM_Delete(t *testing.T) {
	sm := newKvSM()
	applyKv(t, sm, kvCmd{Op: opSet, Key: "k", Value: "v"})
	if err := applyKv(t, sm, kvCmd{Op: opDel, Key: "k"}); err != nil {
		t.Fatalf("del: %v", err)
	}
	if _, ok := sm.Get("k"); ok {
		t.Error("key still present after delete")
	}
}

func TestKvSM_DeleteMissing(t *testing.T) {
	sm := newKvSM()
	err := applyKv(t, sm, kvCmd{Op: opDel, Key: "absent"})
	if !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("expected ErrKeyNotFound, got %v", err)
	}
}

// --- Get ---

func TestKvSM_GetMissing(t *testing.T) {
	sm := newKvSM()
	if _, ok := sm.Get("nope"); ok {
		t.Error("Get on empty SM should return false")
	}
}

// --- Key isolation ---

func TestKvSM_KeysAreIsolated(t *testing.T) {
	sm := newKvSM()
	applyKv(t, sm, kvCmd{Op: opSet, Key: "a", Value: "1"})
	applyKv(t, sm, kvCmd{Op: opSet, Key: "b", Value: "2"})
	applyKv(t, sm, kvCmd{Op: opDel, Key: "a"})

	if _, ok := sm.Get("a"); ok {
		t.Error("a should be absent after delete")
	}
	if v, _ := sm.Get("b"); v != "2" {
		t.Errorf("b = %q, want 2", v)
	}
}

// --- Unknown op ---

func TestKvSM_UnknownOp(t *testing.T) {
	sm := newKvSM()
	err := applyKv(t, sm, kvCmd{Op: "bogus", Key: "k"})
	if err == nil {
		t.Fatal("expected error for unknown op")
	}
}

// --- Snapshot / Restore ---

func TestKvSM_SnapshotRestore(t *testing.T) {
	src := newKvSM()
	applyKv(t, src, kvCmd{Op: opSet, Key: "x", Value: "10"})
	applyKv(t, src, kvCmd{Op: opSet, Key: "y", Value: "20"})

	var buf bytes.Buffer
	if err := src.Snapshot(context.Background(), &buf); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	dst := newKvSM()
	if err := dst.Restore(context.Background(), raft.SnapshotMeta{}, &buf); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	for key, want := range map[string]string{"x": "10", "y": "20"} {
		if got, ok := dst.Get(key); !ok || got != want {
			t.Errorf("Get(%q) after restore = (%q, %v), want (%q, true)", key, got, ok, want)
		}
	}
}

func TestKvSM_RestoreClearsExistingState(t *testing.T) {
	sm := newKvSM()
	applyKv(t, sm, kvCmd{Op: opSet, Key: "stale", Value: "old"})

	fresh := newKvSM()
	applyKv(t, fresh, kvCmd{Op: opSet, Key: "fresh", Value: "new"})
	var buf bytes.Buffer
	_ = fresh.Snapshot(context.Background(), &buf)

	if err := sm.Restore(context.Background(), raft.SnapshotMeta{}, &buf); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if _, ok := sm.Get("stale"); ok {
		t.Error("stale key should be gone after restore")
	}
	if v, _ := sm.Get("fresh"); v != "new" {
		t.Errorf("fresh = %q, want new", v)
	}
}

func TestKvSM_RestoreEmptySnapshot(t *testing.T) {
	sm := newKvSM()
	var buf bytes.Buffer
	_ = sm.Snapshot(context.Background(), &buf)

	dst := newKvSM()
	applyKv(t, dst, kvCmd{Op: opSet, Key: "old", Value: "v"})
	if err := dst.Restore(context.Background(), raft.SnapshotMeta{}, &buf); err != nil {
		t.Fatalf("Restore from empty: %v", err)
	}
	if _, ok := dst.Get("old"); ok {
		t.Error("old key should be gone after restoring empty snapshot")
	}
}
