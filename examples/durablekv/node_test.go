package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/brunoga/raft/v2"
	"github.com/brunoga/raft/v2/storage/filestore"
	"github.com/brunoga/raft/v2/transport/memtransport"
)

// countingStore is the durable store with a counter on the apply path, so a
// test can tell what the engine asked it to do rather than only what the state
// ended up as.
type countingStore struct {
	*kvStore
	applied atomic.Int64
	batches atomic.Int64
}

// Only entries carrying a command are counted. A newly elected leader appends
// an empty one to commit anything left from earlier terms, and counting that as
// a replay would make every restart look like one.
func (c *countingStore) Apply(ctx context.Context, e raft.LogEntry) ([]byte, error) {
	if len(e.Command) > 0 {
		c.applied.Add(1)
		c.batches.Add(1)
	}
	return c.kvStore.Apply(ctx, e)
}

func (c *countingStore) ApplyBatch(ctx context.Context, entries []raft.LogEntry) ([]raft.ApplyOutcome, error) {
	n := 0
	for _, e := range entries {
		if len(e.Command) > 0 {
			n++
		}
	}
	if n > 0 {
		c.applied.Add(int64(n))
		c.batches.Add(1)
	}
	return c.kvStore.ApplyBatch(ctx, entries)
}

// startNode brings up a single-node cluster over the given directories.
func startNode(t *testing.T, dir string, net *memtransport.Network) (*raft.Node, *countingStore, func()) {
	t.Helper()

	logStore, err := filestore.Open(filepath.Join(dir, "raft"))
	if err != nil {
		t.Fatalf("open raft log: %v", err)
	}
	inner, err := openStore(filepath.Join(dir, "state"))
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	sm := &countingStore{kvStore: inner}

	cfg := raft.DefaultConfig()
	cfg.ID = "n1"
	cfg.Storage = logStore
	cfg.StateMachine = sm
	cfg.Transport = net.NewTransport("n1")
	cfg.TickInterval = 0
	cfg.SnapshotThreshold = 0 // no compaction: this is about replay, not snapshots
	cfg.HeartbeatInterval = 100 * time.Millisecond
	cfg.ElectionTimeoutMin = 1000 * time.Millisecond
	cfg.ElectionTimeoutMax = 2000 * time.Millisecond

	node, err := raft.New(&cfg)
	if err != nil {
		t.Fatalf("raft.New: %v", err)
	}
	net.Register("n1", node.Handler())
	node.Start()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && node.State() != raft.Leader {
		node.Tick()
		time.Sleep(time.Millisecond)
	}
	if node.State() != raft.Leader {
		t.Fatal("node never became leader")
	}

	stop := func() {
		node.Stop()
		_ = sm.Close()
		_ = logStore.Close()
	}
	return node, sm, stop
}

func put(t *testing.T, node *raft.Node, k, v string) {
	t.Helper()
	cmd, err := json.Marshal(command{Op: "put", Key: k, Value: v})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := node.Propose(context.Background(), cmd); err != nil {
		t.Fatalf("propose %s: %v", k, err)
	}
}

// TestRestart_ReplaysNothingItHasAlreadyApplied is the reason
// raft.DurableStateMachine exists.
//
// Without it, a node coming back has to rebuild the state machine from a
// snapshot and replay every entry after it, because the engine has no way to
// know what the state machine already has. A state machine that is itself a
// database has already done that work and it is sitting on the disk.
//
// The counter here is what makes the difference visible: after a restart the
// engine must not call Apply at all for entries the store already reports as
// applied.
func TestRestart_ReplaysNothingItHasAlreadyApplied(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	net := memtransport.NewNetwork()

	node, sm, stop := startNode(t, dir, net)
	const writes = 25
	for i := range writes {
		put(t, node, fmt.Sprintf("k%d", i), fmt.Sprintf("v%d", i))
	}
	appliedBefore := sm.applied.Load()
	if appliedBefore < writes {
		t.Fatalf("applied %d entries, want at least the %d written", appliedBefore, writes)
	}
	durableIndex, err := sm.AppliedIndex(ctx)
	if err != nil {
		t.Fatalf("applied index: %v", err)
	}
	stop()

	// Restart against the same directories.
	restarted, sm2, stop2 := startNode(t, dir, memtransport.NewNetwork())
	defer stop2()

	if replayed := sm2.applied.Load(); replayed != 0 {
		t.Errorf("the engine replayed %d entries into a state machine that reported index %d "+
			"as already applied; the point of DurableStateMachine is that it does not",
			replayed, durableIndex)
	}

	// And the state is there, without having been replayed.
	for i := range writes {
		key := fmt.Sprintf("k%d", i)
		want := fmt.Sprintf("v%d", i)
		if got, ok := sm2.get(key); !ok || got != want {
			t.Fatalf("%s = %q (%v) after restart, want %q", key, got, ok, want)
		}
	}

	// A write after the restart still lands, so the node is not merely quiet.
	put(t, restarted, "after", "restart")
	if got, _ := sm2.get("after"); got != "restart" {
		t.Errorf("write after restart did not apply: got %q", got)
	}
}

// TestApplyBatch_IsUsedForRunsOfEntries checks that the engine takes the batch
// path when there is a batch to take.
//
// The saving is per sync, not per entry: fifty entries in one batch cost one
// fsync instead of fifty. That only happens if the engine actually calls
// ApplyBatch, which it does by type assertion -- silently, so a state machine
// that stops satisfying the interface loses the batching with no other sign.
func TestApplyBatch_IsUsedForRunsOfEntries(t *testing.T) {
	dir := t.TempDir()
	net := memtransport.NewNetwork()
	node, sm, stop := startNode(t, dir, net)
	defer stop()

	// Propose concurrently so entries commit in runs rather than one at a time.
	const writes = 200
	done := make(chan error, writes)
	for i := range writes {
		go func() {
			cmd, err := json.Marshal(command{Op: "put", Key: fmt.Sprintf("k%d", i), Value: "v"})
			if err != nil {
				done <- err
				return
			}
			_, err = node.Propose(context.Background(), cmd)
			done <- err
		}()
	}
	for range writes {
		if err := <-done; err != nil {
			t.Fatalf("propose: %v", err)
		}
	}

	applied := sm.applied.Load()
	batches := sm.batches.Load()
	if applied < writes {
		t.Fatalf("applied %d, want at least %d", applied, writes)
	}
	if batches >= applied {
		t.Errorf("%d entries arrived in %d calls: the engine is applying one at a time, "+
			"so every entry pays for its own fsync", applied, batches)
	}
	t.Logf("%d entries in %d calls (%.1f per call)", applied, batches, float64(applied)/float64(batches))
}

// TestSnapshot_IsCapturedNotSerialisedOnTheApplyLoop checks that the engine
// uses Capture when the state machine offers it.
//
// Without it, Snapshot runs on the apply goroutine and everything committed
// during the serialisation waits for it -- a pause in apply, and so in the
// latency of every proposal, once per SnapshotThreshold entries.
func TestSnapshot_IsCapturedNotSerialisedOnTheApplyLoop(t *testing.T) {
	dir := t.TempDir()
	net := memtransport.NewNetwork()

	logStore, err := filestore.Open(filepath.Join(dir, "raft"))
	if err != nil {
		t.Fatalf("open raft log: %v", err)
	}
	defer func() { _ = logStore.Close() }()
	inner, err := openStore(filepath.Join(dir, "state"))
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	defer func() { _ = inner.Close() }()

	sm := &capturingStore{kvStore: inner}

	cfg := raft.DefaultConfig()
	cfg.ID = "n1"
	cfg.Storage = logStore
	cfg.StateMachine = sm
	cfg.Transport = net.NewTransport("n1")
	cfg.TickInterval = 0
	cfg.SnapshotThreshold = 8
	cfg.TrailingLogs = 4
	cfg.HeartbeatInterval = 100 * time.Millisecond
	cfg.ElectionTimeoutMin = 1000 * time.Millisecond
	cfg.ElectionTimeoutMax = 2000 * time.Millisecond

	node, err := raft.New(&cfg)
	if err != nil {
		t.Fatalf("raft.New: %v", err)
	}
	net.Register("n1", node.Handler())
	node.Start()
	defer node.Stop()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && node.State() != raft.Leader {
		node.Tick()
		time.Sleep(time.Millisecond)
	}
	if node.State() != raft.Leader {
		t.Fatal("node never became leader")
	}

	for i := range 40 {
		put(t, node, fmt.Sprintf("k%d", i), "v")
	}
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && node.SnapshotIndex() == 0 {
		node.Tick()
		time.Sleep(time.Millisecond)
	}
	if node.SnapshotIndex() == 0 {
		t.Fatal("no snapshot was taken")
	}

	if sm.captures.Load() == 0 {
		t.Error("the engine serialised on the apply loop instead of capturing; " +
			"SnapshotCapturer is detected by type assertion, so losing it is silent")
	}
	if sm.inlineSnapshots.Load() != 0 {
		t.Errorf("Snapshot was called %d times even though Capture is implemented",
			sm.inlineSnapshots.Load())
	}
}

// capturingStore counts which snapshot path the engine took.
type capturingStore struct {
	*kvStore
	captures        atomic.Int64
	inlineSnapshots atomic.Int64
}

func (c *capturingStore) Capture(ctx context.Context) (raft.Snapshot, error) {
	c.captures.Add(1)
	var capturer raft.SnapshotCapturer = c.kvStore
	return capturer.Capture(ctx)
}

func (c *capturingStore) Snapshot(ctx context.Context, w io.Writer) error {
	c.inlineSnapshots.Add(1)
	return c.kvStore.Snapshot(ctx, w)
}
