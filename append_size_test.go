package raft_test

import (
	"context"
	"fmt"
	"io"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/brunoga/raft/v2"
	"github.com/brunoga/raft/v2/storage/memstore"
	"github.com/brunoga/raft/v2/transport/memtransport"
)

// sizeRecordingTransport records the payload size of every AppendEntries it
// carries, so a test can assert what the leader actually puts on the wire.
type sizeRecordingTransport struct {
	raft.Transport

	mu       sync.Mutex
	payloads []int
	counts   []int
}

func (t *sizeRecordingTransport) AppendEntries(ctx context.Context, to raft.NodeID, req *raft.AppendEntriesRequest) (*raft.AppendEntriesResponse, error) {
	if len(req.Entries) > 0 {
		size := 0
		for _, e := range req.Entries {
			size += len(e.Command)
		}
		t.mu.Lock()
		t.payloads = append(t.payloads, size)
		t.counts = append(t.counts, len(req.Entries))
		t.mu.Unlock()
	}
	return t.Transport.AppendEntries(ctx, to, req)
}

func (t *sizeRecordingTransport) largest() (size, entries int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for i, s := range t.payloads {
		if s > size {
			size, entries = s, t.counts[i]
		}
	}
	return size, entries
}

type sizeSM struct{}

func (sizeSM) Apply(context.Context, raft.LogEntry) ([]byte, error) { return nil, nil }
func (sizeSM) Snapshot(_ context.Context, w io.Writer) error {
	_, err := w.Write([]byte("s"))
	return err
}
func (sizeSM) Restore(_ context.Context, _ raft.SnapshotMeta, r io.Reader) error {
	_, err := io.Copy(io.Discard, r)
	return err
}

// twoNodeWithRecorder starts a leader whose transport records outbound
// AppendEntries sizes, plus one follower that is initially unreachable so a
// backlog builds up and the leader has something to batch.
func twoNodeWithRecorder(t *testing.T, tune func(*raft.Config)) (*raft.Node, *sizeRecordingTransport, func()) {
	t.Helper()

	net := memtransport.NewNetwork()
	rec := &sizeRecordingTransport{Transport: net.NewTransport("n1")}

	mk := func(id raft.NodeID, tr raft.Transport, peers []raft.PeerConfig) *raft.Node {
		cfg := raft.DefaultConfig()
		cfg.ID = id
		cfg.Peers = peers
		cfg.Storage = memstore.New()
		cfg.StateMachine = sizeSM{}
		cfg.Transport = tr
		cfg.TickInterval = 0
		cfg.SnapshotThreshold = 0
		tuneForManualTicks(&cfg)
		if tune != nil {
			tune(&cfg)
		}
		node, err := raft.New(&cfg)
		if err != nil {
			t.Fatalf("raft.New(%s): %v", id, err)
		}
		net.Register(id, node.Handler())
		node.Start()
		return node
	}

	// The follower is a non-voter, so the leader stays leader on its own vote
	// while the follower is unreachable.
	leader := mk("n1", rec, []raft.PeerConfig{{ID: "n2", Voter: false}})
	follower := mk("n2", net.NewTransport("n2"), []raft.PeerConfig{{ID: "n1", Voter: true}})

	deadline := time.Now().Add(3 * time.Second)
	for leader.State() != raft.Leader {
		if time.Now().After(deadline) {
			t.Fatal("node never became leader")
		}
		leader.Tick()
		follower.Tick()
		time.Sleep(time.Millisecond)
	}

	tick := func() {
		for range 40 {
			leader.Tick()
			follower.Tick()
			time.Sleep(time.Millisecond)
		}
	}
	t.Cleanup(func() {
		leader.Stop()
		follower.Stop()
	})
	return leader, rec, tick
}

// TestAppendEntries_PayloadIsBoundedByBytes asserts that the leader caps an
// AppendEntries by the size of its payload, not only by the number of entries.
//
// A cap on count alone says nothing about the size of a message: sixty-four
// entries of a megabyte each is a sixty-four megabyte RPC. Transports reject
// oversized messages, and the leader has no smaller batch to fall back on, so
// it re-sends the same rejected batch forever and that follower never catches
// up again.
func TestAppendEntries_PayloadIsBoundedByBytes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		const budget = 64 * 1024
		const entrySize = 8 * 1024

		leader, rec, tick := twoNodeWithRecorder(t, func(cfg *raft.Config) {
			cfg.MaxBytesPerRPC = budget
			cfg.MaxLogEntriesPerRPC = 64 // would otherwise allow 64 * 8 KiB
		})

		for i := range 40 {
			// A fresh slice per proposal: Propose retains the command it is given.
			payload := make([]byte, entrySize)
			payload[0] = byte(i)
			if _, err := leader.Propose(ctx, payload); err != nil {
				t.Fatalf("propose: %v", err)
			}
		}
		tick()

		size, entries := rec.largest()
		if size == 0 {
			t.Fatal("no AppendEntries carrying entries was observed")
		}
		if size > budget {
			t.Errorf("largest AppendEntries carried %d bytes in %d entries, over the %d byte budget",
				size, entries, budget)
		}
	})
}

// TestAppendEntries_SingleOversizedEntryIsStillSent asserts that an entry
// larger than the budget is sent on its own rather than never being sent at
// all. Refusing to send it would stall replication permanently, which is worse
// than one oversized message the transport may still accept.
func TestAppendEntries_SingleOversizedEntryIsStillSent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		const budget = 4 * 1024

		leader, rec, tick := twoNodeWithRecorder(t, func(cfg *raft.Config) {
			cfg.MaxBytesPerRPC = budget
		})

		big := make([]byte, 32*1024)
		if _, err := leader.Propose(ctx, big); err != nil {
			t.Fatalf("propose: %v", err)
		}
		tick()

		size, entries := rec.largest()
		if size == 0 {
			t.Fatal("an entry larger than the budget was never sent")
		}
		if entries != 1 {
			t.Errorf("oversized entry was sent alongside %d others; it should travel alone", entries-1)
		}
	})
}

// TestAppendEntries_UnsetBudgetKeepsCountLimit asserts the count limit still
// applies when no byte budget is configured, so existing deployments behave as
// before.
func TestAppendEntries_UnsetBudgetKeepsCountLimit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()

		leader, rec, tick := twoNodeWithRecorder(t, func(cfg *raft.Config) {
			cfg.MaxBytesPerRPC = 0 // unlimited
			cfg.MaxLogEntriesPerRPC = 4
		})

		for i := range 40 {
			if _, err := leader.Propose(ctx, fmt.Appendf(nil, "entry-%d", i)); err != nil {
				t.Fatalf("propose: %v", err)
			}
		}
		tick()

		if _, entries := rec.largest(); entries > 4 {
			t.Errorf("AppendEntries carried %d entries with MaxLogEntriesPerRPC set to 4", entries)
		}
	})
}
