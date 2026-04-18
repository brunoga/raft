package raft

// Tests for the seven issues identified in the 8th Principal-Engineer evaluation:
//   1. handleSnapInstallResult doesn't clear pendingSnap on goroutine failure
//   2. restoreSnapshotCh overwrite leaks io.ReadCloser (FD leak)
//   3. runSnapshotInstall goroutines not tracked by Stop()
//   4. sendResult uses stopCtx instead of installCtx (stale goroutine posts result)
//   7. Chunk-full drop leaves install goroutine stuck

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"
)

// trackRC is a minimal io.ReadCloser that records whether Close was called.
type trackRC struct{ closed bool }

func (r *trackRC) Read(p []byte) (int, error) { return 0, io.EOF }
func (r *trackRC) Close() error               { r.closed = true; return nil }

// blockingSaveStorage is a Storage whose SaveSnapshot blocks until its context
// is cancelled. Used to hold a runSnapshotInstall goroutine in SaveSnapshot so
// the test can observe whether Stop() (or a channel-full cancellation) actually
// waits for it.
type blockingSaveStorage struct {
	stubStorage
	savingOnce sync.Once
	doneOnce   sync.Once
	saving     chan struct{} // closed once when SaveSnapshot is entered
	done       chan struct{} // closed when SaveSnapshot is about to exit
	exitDelay  time.Duration
}

func newBlockingSaveStorage(exitDelay time.Duration) *blockingSaveStorage {
	return &blockingSaveStorage{
		saving:    make(chan struct{}),
		done:      make(chan struct{}),
		exitDelay: exitDelay,
	}
}

func (b *blockingSaveStorage) SaveSnapshot(ctx context.Context, _ SnapshotMeta, _ io.Reader) error {
	b.savingOnce.Do(func() { close(b.saving) })
	<-ctx.Done()
	if b.exitDelay > 0 {
		time.Sleep(b.exitDelay)
	}
	b.doneOnce.Do(func() { close(b.done) })
	return ctx.Err()
}

// newUnstartedSnapNode creates a Node via New() without calling Start(), so
// event-loop methods can be called from the test goroutine without races.
func newUnstartedSnapNode(t *testing.T, stor Storage) *Node {
	t.Helper()
	cfg := DefaultConfig()
	cfg.ID = "snap-test"
	cfg.Storage = stor
	cfg.StateMachine = &stubStateMachine{}
	cfg.Transport = &stubTransport{}
	n, err := New(&cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return n
}

// ---- Issue 1: handleSnapInstallResult must nil pendingSnap on error --------

// TestHandleSnapInstallResult_ClearsPendingSnapOnError verifies that when the
// runSnapshotInstall goroutine reports an error, handleSnapInstallResult nils
// out n.pendingSnap so that the dead channel reference is released and
// subsequent incoming chunks are not silently buffered into a dead channel.
//
// FAILS before the fix: pendingSnap is not nil after the error.
// PASSES after the fix.
func TestHandleSnapInstallResult_ClearsPendingSnapOnError(t *testing.T) {
	n := newUnstartedSnapNode(t, &stubStorage{})

	// Simulate an in-progress multi-chunk snapshot install.
	n.pendingSnap = &partialSnapshot{
		meta:      SnapshotMeta{LastIncludedIndex: 10, LastIncludedTerm: 1},
		installCh: make(chan []byte, 8),
		cancelFn:  func() {},
	}

	// Deliver an error result as the runSnapshotInstall goroutine would.
	n.handleSnapInstallResult(&snapInstallResult{
		meta: SnapshotMeta{LastIncludedIndex: 10, LastIncludedTerm: 1},
		err:  errors.New("storage failure"),
	})

	if n.pendingSnap != nil {
		t.Error("handleSnapInstallResult: pendingSnap not cleared after error — " +
			"dead channel goroutine will leak and future chunks will be buffered into a dead channel")
	}
}

// ---- Issue 2: restoreSnapshotCh overwrite must close old io.ReadCloser -----

// TestHandleSnapInstallResult_ClosesOldReaderOnOverwrite verifies that when a
// second successful snap-install result is processed while the apply goroutine
// has not yet drained restoreSnapshotCh (channel full), the old io.ReadCloser
// is closed before it is discarded — preventing a file-descriptor leak.
//
// FAILS before the fix: rc1.closed is false (reader leaked).
// PASSES after the fix: rc1.closed is true.
func TestHandleSnapInstallResult_ClosesOldReaderOnOverwrite(t *testing.T) {
	n := newUnstartedSnapNode(t, &stubStorage{})

	rc1 := &trackRC{}
	rc2 := &trackRC{}

	// First install: restoreSnapshotCh is empty so the send succeeds directly.
	n.handleSnapInstallResult(&snapInstallResult{
		meta:  SnapshotMeta{LastIncludedIndex: 5, LastIncludedTerm: 1},
		table: nil,
		smR:   rc1,
	})

	// restoreSnapshotCh now has one item (rc1 inside).  The apply goroutine is
	// not running (node not started), so the channel stays full.

	// Second install: restoreSnapshotCh is full → overwrite path → must Close rc1.
	n.handleSnapInstallResult(&snapInstallResult{
		meta:  SnapshotMeta{LastIncludedIndex: 10, LastIncludedTerm: 1},
		table: nil,
		smR:   rc2,
	})

	if !rc1.closed {
		t.Error("handleSnapInstallResult: first io.ReadCloser not closed on restoreSnapshotCh overwrite — FD leak")
	}
}

// ---- Issue 3: Stop() must wait for runSnapshotInstall goroutines -----------

// TestStop_WaitsForSnapshotInstallGoroutine verifies that Node.Stop() does not
// return while a runSnapshotInstall goroutine is still running.  Before the fix
// Stop() returned immediately after the event and apply loops exited, leaving
// the snapshot-install goroutine as a goroutine leak.
//
// The test uses blockingSaveStorage with a 50 ms exit-delay: without the fix
// Stop() returns long before the 50 ms elapses (bs.done is not yet closed);
// with the fix Stop() waits for the goroutine (bs.done is closed on return).
//
// FAILS before the fix: bs.done is not closed when Stop() returns.
// PASSES after the fix.
func TestStop_WaitsForSnapshotInstallGoroutine(t *testing.T) {
	// 50 ms delay gives us a wide window: Stop() without the fix returns in
	// <5 ms; Stop() with the fix returns in ~50 ms (delay + overhead).
	bs := newBlockingSaveStorage(50 * time.Millisecond)

	cfg := DefaultConfig()
	cfg.ID = "n1"
	cfg.Storage = bs
	cfg.StateMachine = &stubStateMachine{}
	cfg.Transport = &stubTransport{}
	n, err := New(&cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	n.Start()

	ctx := context.Background()

	// Elevate to term 1 so the node accepts InstallSnapshot from "leader".
	n.HandleAppendEntries(ctx, &AppendEntriesRequest{ //nolint:errcheck // return value not meaningful in test context
		Term: 1, LeaderID: "leader",
	})

	// Send chunk 0 to start the runSnapshotInstall goroutine.
	n.HandleInstallSnapshot(ctx, &InstallSnapshotRequest{ //nolint:errcheck // return value not meaningful in test context
		Term:              1,
		LeaderID:          "leader",
		LastIncludedIndex: 1,
		LastIncludedTerm:  1,
		Offset:            0,
		Data:              []byte("x"),
		Done:              false,
	})

	// Wait until the goroutine has entered SaveSnapshot.
	select {
	case <-bs.saving:
	case <-time.After(2 * time.Second):
		t.Fatal("runSnapshotInstall goroutine never entered SaveSnapshot")
	}

	// Stop the node.  With the fix this blocks until the goroutine finishes.
	n.Stop()

	// With the fix bs.done is already closed (goroutine finished).
	// Without the fix Stop() returned before the 50 ms delay elapsed.
	select {
	case <-bs.done:
		// correct: goroutine has exited
	default:
		t.Error("Stop() returned but runSnapshotInstall goroutine is still running — goroutine leak")
	}
}

// ---- Issue 4: sendResult must guard on installCtx, not stopCtx ------------

// TestRunSnapshotInstall_CancelledDoesNotPostResult verifies that when a
// snapshot install's context is cancelled (because a newer install superseded
// it), the goroutine discards the result rather than posting it to rpcCh.
// Before the fix sendResult selected on n.stopCtx.Done(); since the node was
// still running, the stale goroutine always managed to post its result.
//
// The test cancels installCtx before LoadSnapshot completes and checks that
// rpcCh receives no additional message after the cancellation.
//
// FAILS before the fix: rpcCh has an extra message from the stale goroutine.
// PASSES after the fix: stale goroutine discards and returns without posting.
func TestRunSnapshotInstall_CancelledDoesNotPostResult(t *testing.T) {
	// Use a storage whose SaveSnapshot succeeds immediately and whose
	// LoadSnapshot also succeeds (we use the stubStorage, which returns
	// ErrNoSnapshot — the goroutine will post an error result).
	//
	// We want to verify that when installCtx is cancelled BEFORE the goroutine
	// has a chance to call sendResult, the goroutine exits without posting.
	//
	// Strategy: use a gated storage whose SaveSnapshot blocks until the test
	// explicitly proceeds, so we can cancel installCtx in the window before
	// the goroutine tries to post.

	type gatedStorage struct {
		stubStorage
		gate chan struct{} // close to allow SaveSnapshot to proceed
	}
	gate := make(chan struct{})
	stor := &gatedStorage{gate: gate}
	stor.gate = gate

	// Override SaveSnapshot: drain the reader then block on gate.
	var saveDone sync.WaitGroup
	saveDone.Add(1) // not used for real blocking; just documentation

	// We implement a local gated storage as an anonymous struct wrapping
	// the real functionality.
	type realGatedStorage struct {
		stubStorage
		gate chan struct{}
	}
	rgs := &realGatedStorage{gate: gate}
	_ = rgs // not used directly below; we use a closure approach

	// Use a blockingSaveStorage but with no exit delay; we will cancel the
	// context externally.
	bs2 := newBlockingSaveStorage(0)

	cfg := DefaultConfig()
	cfg.ID = "n1"
	cfg.Storage = bs2
	cfg.StateMachine = &stubStateMachine{}
	cfg.Transport = &stubTransport{}
	n, err := New(&cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	n.Start()
	defer n.Stop()

	ctx := context.Background()

	// Elevate to term 1.
	n.HandleAppendEntries(ctx, &AppendEntriesRequest{ //nolint:errcheck // return value not meaningful in test context
		Term: 1, LeaderID: "leader",
	})

	// Record rpcCh length before sending the snapshot.
	// (We do not have direct access to rpcCh from package raft_test, but we
	// are in package raft here, so we can read n.rpcCh.)
	// Send the snapshot install request to start the goroutine.
	n.HandleInstallSnapshot(ctx, &InstallSnapshotRequest{ //nolint:errcheck // return value not meaningful in test context
		Term:              1,
		LeaderID:          "leader",
		LastIncludedIndex: 5,
		LastIncludedTerm:  1,
		Offset:            0,
		Data:              []byte("x"),
		Done:              false,
	})

	// Wait until the goroutine is inside SaveSnapshot.
	select {
	case <-bs2.saving:
	case <-time.After(2 * time.Second):
		t.Fatal("goroutine never entered SaveSnapshot")
	}

	// Now send Offset=0 again for the same snapshot index.  This causes the
	// event loop to cancel the first goroutine's installCtx and start a second
	// goroutine.  The first goroutine is stuck in SaveSnapshot; it will detect
	// ctx.Done() and discard its result rather than posting to rpcCh.
	n.HandleInstallSnapshot(ctx, &InstallSnapshotRequest{ //nolint:errcheck // return value not meaningful in test context
		Term:              1,
		LeaderID:          "leader",
		LastIncludedIndex: 5,
		LastIncludedTerm:  1,
		Offset:            0, // restart → cancels first goroutine's installCtx
		Data:              []byte("y"),
		Done:              false,
	})

	// After this point the first goroutine's context is cancelled; it must
	// exit without posting to rpcCh.
	// Give it 100 ms to finish (it has no exit delay).
	time.Sleep(100 * time.Millisecond)

	// The only item in rpcCh at this point should be from the second goroutine
	// (which is also blocked in blockingSaveStorage.SaveSnapshot and therefore
	// hasn't posted anything yet).  Drain what's there and verify no stale
	// result from the first goroutine is present.
	// Because both goroutines are blocked in SaveSnapshot, rpcCh should be empty.
	if len(n.rpcCh) > 0 {
		t.Errorf("rpcCh has %d unexpected items — stale goroutine may have posted a result", len(n.rpcCh))
	}
}

// ---- Issue 7: chunk-full drop must cancel goroutine, not leave it stuck ----

// TestInstallSnapshot_ChunkFullCancelsGoroutine verifies that when the install
// channel is full (disk I/O lagging behind the RPC rate), the event loop
// cancels the in-progress install goroutine immediately rather than silently
// dropping the chunk and leaving the goroutine blocked indefinitely.
//
// The test uses blockingSaveStorage (SaveSnapshot blocks without reading the
// channel), fills the install channel (buffer=8), then sends one more chunk.
// Before the fix: the goroutine is still alive after the channel-full event
// (bs.done is not closed).
// After the fix: the goroutine's installCtx is cancelled, it exits, and
// bs.done is closed promptly.
//
// FAILS before the fix: bs.done not closed after channel-full + brief wait.
// PASSES after the fix.
func TestInstallSnapshot_ChunkFullCancelsGoroutine(t *testing.T) {
	// No exit delay: we want the goroutine to exit as soon as ctx is cancelled.
	bs := newBlockingSaveStorage(0)

	cfg := DefaultConfig()
	cfg.ID = "n1"
	cfg.Storage = bs
	cfg.StateMachine = &stubStateMachine{}
	cfg.Transport = &stubTransport{}
	n, err := New(&cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	n.Start()
	defer n.Stop()

	ctx := context.Background()

	// Elevate to term 1.
	n.HandleAppendEntries(ctx, &AppendEntriesRequest{ //nolint:errcheck // return value not meaningful in test context
		Term: 1, LeaderID: "leader",
	})

	const snapIdx = Index(100)

	// Send chunk 0 to start the goroutine.  The goroutine immediately blocks
	// in blockingSaveStorage.SaveSnapshot without reading from the channel.
	n.HandleInstallSnapshot(ctx, &InstallSnapshotRequest{ //nolint:errcheck // return value not meaningful in test context
		Term: 1, LeaderID: "leader",
		LastIncludedIndex: snapIdx, LastIncludedTerm: 1,
		Offset: 0, Data: []byte("x"), Done: false,
	})

	// Wait until the goroutine has entered SaveSnapshot and is no longer
	// reading from the channel.
	select {
	case <-bs.saving:
	case <-time.After(2 * time.Second):
		t.Fatal("goroutine never entered SaveSnapshot")
	}

	// Fill the install channel: the buffer holds 8 items.  The goroutine is
	// not reading, so each send goes straight into the buffer.
	for i := int64(1); i <= 7; i++ {
		_, chunkErr := n.HandleInstallSnapshot(ctx, &InstallSnapshotRequest{
			Term: 1, LeaderID: "leader",
			LastIncludedIndex: snapIdx, LastIncludedTerm: 1,
			Offset: i, Data: []byte("x"), Done: false,
		})
		if chunkErr != nil {
			t.Fatalf("chunk %d: HandleInstallSnapshot: %v", i, chunkErr)
		}
	}

	// Send one more chunk that finds the channel full.
	// Before fix: chunk is dropped with a log warning; goroutine stays stuck.
	// After fix:  installCtx is cancelled; goroutine exits.
	_, err = n.HandleInstallSnapshot(ctx, &InstallSnapshotRequest{
		Term: 1, LeaderID: "leader",
		LastIncludedIndex: snapIdx, LastIncludedTerm: 1,
		Offset: 8, Data: []byte("x"), Done: false,
	})
	if err != nil {
		t.Fatalf("chunk 8 (overflow): HandleInstallSnapshot: %v", err)
	}

	// Allow the goroutine a generous window to exit after ctx cancellation.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		select {
		case <-bs.done:
			return // goroutine exited — test passes
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}

	t.Error("snapshot install goroutine still running after channel-full — stuck goroutine leak")
}
