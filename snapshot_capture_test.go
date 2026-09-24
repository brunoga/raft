package raft_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/brunoga/raft/v2"
	"github.com/brunoga/raft/v2/storage/memstore"
	"github.com/brunoga/raft/v2/transport/memtransport"
)

// blockingSM serialises slowly, so a test can see whether applying entries
// waits for it. It implements Capture only when capture is true.
type blockingSM struct {
	capture bool

	release  chan struct{}
	writing  chan struct{}
	writeOne sync.Once

	applied  atomic.Int64
	captures atomic.Int64
	released atomic.Int64
}

func newBlockingSM(capture bool) *blockingSM {
	return &blockingSM{
		capture: capture,
		release: make(chan struct{}),
		writing: make(chan struct{}),
	}
}

func (s *blockingSM) Apply(context.Context, raft.LogEntry) ([]byte, error) {
	s.applied.Add(1)
	return nil, nil
}

// slowWrite blocks until the test releases it, so the serialisation is still
// in progress while the test looks at whether apply has moved on.
func (s *blockingSM) slowWrite(ctx context.Context, w io.Writer) error {
	s.writeOne.Do(func() { close(s.writing) })
	select {
	case <-s.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	_, err := w.Write([]byte("state"))
	return err
}

func (s *blockingSM) Snapshot(ctx context.Context, w io.Writer) error {
	return s.slowWrite(ctx, w)
}

func (s *blockingSM) Restore(_ context.Context, _ raft.SnapshotMeta, r io.Reader) error {
	_, err := io.Copy(io.Discard, r)
	return err
}

func (s *blockingSM) Capture(context.Context) (raft.Snapshot, error) {
	s.captures.Add(1)
	return &blockingCapture{sm: s}, nil
}

type blockingCapture struct{ sm *blockingSM }

func (c *blockingCapture) Write(ctx context.Context, w io.Writer) error {
	return c.sm.slowWrite(ctx, w)
}

func (c *blockingCapture) Release() { c.sm.released.Add(1) }

// snapshotNode starts a single-voter leader whose snapshots are slow.
func snapshotNode(t *testing.T, sm raft.StateMachine) *raft.Node {
	t.Helper()

	cfg := raft.DefaultConfig()
	cfg.ID = "n1"
	cfg.Storage = memstore.New()
	cfg.StateMachine = sm
	cfg.Transport = memtransport.NewNetwork().NewTransport("n1")
	cfg.TickInterval = 0
	cfg.SnapshotThreshold = 4
	cfg.TrailingLogs = 2
	tuneForManualTicks(&cfg)

	node, err := raft.New(&cfg)
	if err != nil {
		t.Fatalf("raft.New: %v", err)
	}
	node.Start()

	deadline := time.Now().Add(3 * time.Second)
	for node.State() != raft.Leader {
		if time.Now().After(deadline) {
			t.Fatal("node never became leader")
		}
		node.Tick()
		time.Sleep(time.Millisecond)
	}
	return node
}

// TestSnapshotCapturer_ApplyContinuesWhileTheSnapshotIsWritten asserts that a
// state machine which can hand over a point-in-time capture keeps applying
// entries while the snapshot is serialised.
//
// Snapshot runs on the goroutine that applies entries, because the two must not
// overlap. Everything committed during serialisation therefore waits for it:
// on a large state machine that is a pause in apply, and so in the latency of
// every proposal, once per SnapshotThreshold entries.
func TestSnapshotCapturer_ApplyContinuesWhileTheSnapshotIsWritten(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()

		sm := newBlockingSM(true)
		node := snapshotNode(t, sm)
		t.Cleanup(func() {
			close(sm.release)
			node.Stop()
		})

		// Cross the snapshot threshold.
		for i := range 6 {
			if _, err := node.Propose(ctx, fmt.Appendf(nil, "entry-%d", i)); err != nil {
				t.Fatalf("propose: %v", err)
			}
		}

		select {
		case <-sm.writing:
		case <-time.After(5 * time.Second):
			t.Fatal("the snapshot never started being written")
		}

		// The snapshot is parked mid-write. Applying must carry on regardless.
		before := sm.applied.Load()
		done := make(chan error, 1)
		go func() {
			_, err := node.Propose(ctx, []byte("while-snapshotting"))
			done <- err
		}()

		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("proposal during a snapshot: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("a proposal made during snapshot serialisation never applied; "+
				"apply is blocked behind the snapshot (applied %d)", sm.applied.Load())
		}

		if got := sm.applied.Load(); got <= before {
			t.Errorf("applied count did not move during serialisation: %d then %d", before, got)
		}
		if sm.captures.Load() == 0 {
			t.Error("Capture was never called on a state machine that implements it")
		}
	})
}

// TestSnapshot_WithoutCaptureApplyWaits pins the documented behaviour for a
// state machine that cannot capture: serialisation happens on the apply
// goroutine, so applying waits. This is the contract those state machines
// already rely on, and the change must not quietly break it.
func TestSnapshot_WithoutCaptureApplyWaits(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()

		sm := newBlockingSM(false)
		// Hide Capture: this state machine is a plain StateMachine.
		node := snapshotNode(t, struct{ raft.StateMachine }{sm})
		t.Cleanup(func() {
			close(sm.release)
			node.Stop()
		})

		// Propose in the background: once serialisation starts, apply stops, so
		// the proposals that cross the threshold never come back. That is the
		// behaviour under test, not a failure.
		proposals := make(chan error, 16)
		go func() {
			for i := range 8 {
				_, err := node.Propose(ctx, fmt.Appendf(nil, "entry-%d", i))
				proposals <- err
			}
		}()

		select {
		case <-sm.writing:
		case <-time.After(5 * time.Second):
			t.Fatal("the snapshot never started being written")
		}

		proposeCtx, proposeCancel := context.WithTimeout(ctx, 500*time.Millisecond)
		defer proposeCancel()
		_, err := node.Propose(proposeCtx, []byte("while-snapshotting"))
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("proposal during serialisation returned %v; apply was expected to "+
				"wait for a state machine that cannot capture", err)
		}
		if sm.captures.Load() != 0 {
			t.Error("Capture was called on a state machine that does not implement it")
		}
	})
}

// TestSnapshotCapturer_CaptureIsAlwaysReleased asserts that the handle a
// capture holds is given back, so a state machine can free whatever it pinned.
func TestSnapshotCapturer_CaptureIsAlwaysReleased(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()

		sm := newBlockingSM(true)
		close(sm.release) // serialise immediately
		node := snapshotNode(t, sm)
		t.Cleanup(node.Stop)

		for i := range 8 {
			if _, err := node.Propose(ctx, fmt.Appendf(nil, "entry-%d", i)); err != nil {
				t.Fatalf("propose: %v", err)
			}
		}
		deadline := time.Now().Add(5 * time.Second)
		for node.SnapshotIndex() == 0 && time.Now().Before(deadline) {
			node.Tick()
			time.Sleep(time.Millisecond)
		}
		if node.SnapshotIndex() == 0 {
			t.Fatal("no snapshot was taken")
		}

		for time.Now().Before(deadline) {
			if sm.released.Load() >= sm.captures.Load() && sm.captures.Load() > 0 {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Errorf("%d captures taken but only %d released", sm.captures.Load(), sm.released.Load())
	})
}
