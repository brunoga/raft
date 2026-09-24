package raft_test

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	"github.com/brunoga/raft/v2"
	"github.com/brunoga/raft/v2/storage/memstore"
	"github.com/brunoga/raft/v2/transport/memtransport"
)

// echoSM records the last snapshot it restored.
type echoSM struct{ restored []byte }

func (s *echoSM) Apply(context.Context, raft.LogEntry) ([]byte, error) { return nil, nil }
func (s *echoSM) Snapshot(_ context.Context, w io.Writer) error {
	_, err := w.Write([]byte("state"))
	return err
}
func (s *echoSM) Restore(_ context.Context, _ raft.SnapshotMeta, r io.Reader) error {
	b, err := io.ReadAll(r)
	s.restored = b
	return err
}

// snapshotFollower builds a follower over a pre-seeded log.
func snapshotFollower(t *testing.T, entries []raft.LogEntry) (*raft.Node, raft.Storage) {
	t.Helper()

	ctx := context.Background()
	store := memstore.New()
	if len(entries) > 0 {
		if err := store.AppendLogEntries(ctx, entries); err != nil {
			t.Fatalf("seed log: %v", err)
		}
	}
	if err := store.SaveHardState(ctx, raft.HardState{CurrentTerm: 2}); err != nil {
		t.Fatalf("seed hard state: %v", err)
	}

	cfg := raft.DefaultConfig()
	cfg.ID = "f1"
	cfg.Peers = []raft.PeerConfig{{ID: "l1", Voter: true}, {ID: "f2", Voter: true}}
	cfg.Storage = store
	cfg.StateMachine = &echoSM{}
	cfg.Transport = memtransport.NewNetwork().NewTransport("f1")
	cfg.TickInterval = 0

	node, err := raft.New(&cfg)
	if err != nil {
		t.Fatalf("raft.New: %v", err)
	}
	node.Start()
	t.Cleanup(node.Stop)
	return node, store
}

// sendSnapshot delivers a one-chunk snapshot whose payload is the wrapped
// framing the node writes itself, obtained by letting a throwaway leader
// produce one.
func sendSnapshot(t *testing.T, node *raft.Node, meta raft.SnapshotMeta, payload []byte) {
	t.Helper()

	resp, err := node.Handler().HandleInstallSnapshot(context.Background(), &raft.InstallSnapshotRequest{
		Term:              3,
		LeaderID:          "l1",
		LastIncludedIndex: meta.LastIncludedIndex,
		LastIncludedTerm:  meta.LastIncludedTerm,
		Offset:            0,
		Data:              payload,
		Done:              true,
	})
	if err != nil {
		t.Fatalf("HandleInstallSnapshot: %v", err)
	}
	if resp == nil {
		t.Fatal("HandleInstallSnapshot returned no response")
	}
}

// snapshotPayload produces a snapshot body in the node's own framing by taking
// a real snapshot on a throwaway single-node cluster.
func snapshotPayload(t *testing.T) []byte {
	t.Helper()

	store := memstore.New()
	cfg := raft.DefaultConfig()
	cfg.ID = "src"
	cfg.Storage = store
	cfg.StateMachine = &echoSM{}
	cfg.Transport = memtransport.NewNetwork().NewTransport("src")
	cfg.TickInterval = 0
	cfg.SnapshotThreshold = 1
	cfg.TrailingLogs = 0
	tuneForManualTicks(&cfg)

	node, err := raft.New(&cfg)
	if err != nil {
		t.Fatalf("raft.New: %v", err)
	}
	node.Start()
	defer node.Stop()

	deadline := time.Now().Add(3 * time.Second)
	for node.State() != raft.Leader {
		if time.Now().After(deadline) {
			t.Fatal("source node never became leader")
		}
		node.Tick()
		time.Sleep(time.Millisecond)
	}
	for range 3 {
		if _, proposeErr := node.Propose(context.Background(), []byte("x")); proposeErr != nil {
			t.Fatalf("propose: %v", proposeErr)
		}
	}
	for node.SnapshotIndex() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("source node never took a snapshot")
		}
		node.Tick()
		time.Sleep(time.Millisecond)
	}

	_, rc, err := store.LoadSnapshot(context.Background())
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	defer func() { _ = rc.Close() }()
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, rc); err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	return buf.Bytes()
}

func lastIndexOf(t *testing.T, store raft.Storage) raft.Index {
	t.Helper()
	idx, err := store.LastIndex()
	if err != nil {
		t.Fatalf("LastIndex: %v", err)
	}
	return idx
}

// awaitLastIndexAtMost waits for the store to hold nothing past want.
//
// Log writes are carried out behind the event loop, so a truncation takes
// effect in the node's own view of its log before it reaches the disk. A test
// reading the store directly is looking at the disk and has to wait for it;
// everything that reads the node instead sees the truncation immediately.
func awaitLastIndexAtMost(t *testing.T, store raft.Storage, want raft.Index, timeout time.Duration) raft.Index {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		got := lastIndexOf(t, store)
		if got <= want || time.Now().After(deadline) {
			return got
		}
		time.Sleep(time.Millisecond)
	}
}

// TestInstallSnapshot_DiscardsLogThatDivergesFromTheSnapshot asserts that when
// the follower's entry at the snapshot's last-included index does not match the
// snapshot, the whole log is discarded rather than just the prefix.
//
// Raft section 7: entries after the snapshot point may be kept only when the
// follower's log agrees with the snapshot at that point. If it does not, those
// entries belong to a history the cluster abandoned. Keeping them leaves a log
// that starts with the leader's state and continues with somebody else's, and
// the follower will happily serve and replicate it.
func TestInstallSnapshot_DiscardsLogThatDivergesFromTheSnapshot(t *testing.T) {
	payload := snapshotPayload(t)

	// The follower's entries 1-5 are from term 2. The leader's snapshot covers
	// through index 3 at term 3, so the follower's index-3 entry is from a
	// different history, and so is everything after it.
	node, store := snapshotFollower(t, []raft.LogEntry{
		{Index: 1, Term: 2, Command: []byte("a")},
		{Index: 2, Term: 2, Command: []byte("b")},
		{Index: 3, Term: 2, Command: []byte("diverged")},
		{Index: 4, Term: 2, Command: []byte("diverged")},
		{Index: 5, Term: 2, Command: []byte("diverged")},
	})

	sendSnapshot(t, node, raft.SnapshotMeta{LastIncludedIndex: 3, LastIncludedTerm: 3}, payload)

	deadline := time.Now().Add(3 * time.Second)
	for node.LastApplied() < 3 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}

	if got := awaitLastIndexAtMost(t, store, 3, 3*time.Second); got > 3 {
		t.Errorf("log still ends at index %d after installing a snapshot through index 3 "+
			"that disagrees with it; entries from the abandoned history were kept", got)
	}
}

// TestInstallSnapshot_KeepsLogThatAgreesWithTheSnapshot is the companion case:
// when the follower's entry at the snapshot point matches, the entries after it
// are part of the same history and must be kept, so the follower does not have
// to be sent them again.
func TestInstallSnapshot_KeepsLogThatAgreesWithTheSnapshot(t *testing.T) {
	payload := snapshotPayload(t)

	node, store := snapshotFollower(t, []raft.LogEntry{
		{Index: 1, Term: 2, Command: []byte("a")},
		{Index: 2, Term: 2, Command: []byte("b")},
		{Index: 3, Term: 3, Command: []byte("agrees")},
		{Index: 4, Term: 3, Command: []byte("keep-me")},
		{Index: 5, Term: 3, Command: []byte("keep-me")},
	})

	sendSnapshot(t, node, raft.SnapshotMeta{LastIncludedIndex: 3, LastIncludedTerm: 3}, payload)

	deadline := time.Now().Add(3 * time.Second)
	for node.LastApplied() < 3 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}

	if got := lastIndexOf(t, store); got != 5 {
		t.Errorf("log ends at index %d after installing a snapshot through index 3 that "+
			"agrees with it; entries 4 and 5 should have been kept", got)
	}
}

// TestInstallSnapshot_ReportsTheInstalledSnapshotIndex asserts that a node
// reports the compaction boundary it actually has. A follower brought up to
// date by a snapshot answers SnapshotIndex with what it installed, not zero.
func TestInstallSnapshot_ReportsTheInstalledSnapshotIndex(t *testing.T) {
	payload := snapshotPayload(t)
	node, _ := snapshotFollower(t, nil)

	sendSnapshot(t, node, raft.SnapshotMeta{LastIncludedIndex: 7, LastIncludedTerm: 3}, payload)

	deadline := time.Now().Add(3 * time.Second)
	for node.LastApplied() < 7 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := node.SnapshotIndex(); got != 7 {
		t.Errorf("SnapshotIndex() = %d after installing a snapshot through index 7, want 7", got)
	}
}

// TestInstallSnapshot_ReportsTheLeaderThatSentIt asserts that an
// InstallSnapshot RPC updates the node's idea of who the leader is, the same
// way an AppendEntries does. A client redirected by this follower needs the
// right address.
func TestInstallSnapshot_ReportsTheLeaderThatSentIt(t *testing.T) {
	payload := snapshotPayload(t)
	node, _ := snapshotFollower(t, nil)

	sendSnapshot(t, node, raft.SnapshotMeta{LastIncludedIndex: 7, LastIncludedTerm: 3}, payload)

	deadline := time.Now().Add(3 * time.Second)
	for node.Leader() == "" && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := node.Leader(); got != "l1" {
		t.Fatalf("Leader() = %q after an InstallSnapshot from l1, want %q", got, "l1")
	}

	// A second request in the same term, from a different leader. The term did
	// not change, so nothing else updates the node's view of who leads; the
	// snapshot handler has to.
	if _, err := node.Handler().HandleInstallSnapshot(context.Background(), &raft.InstallSnapshotRequest{
		Term:              3,
		LeaderID:          "l2",
		LastIncludedIndex: 2, // already covered, so the install itself is a no-op
		LastIncludedTerm:  3,
		Offset:            0,
		Data:              payload,
		Done:              true,
	}); err != nil {
		t.Fatalf("second HandleInstallSnapshot: %v", err)
	}
	if got := node.Leader(); got != "l2" {
		t.Errorf("Leader() = %q after an InstallSnapshot from l2 in the same term, want %q",
			got, "l2")
	}
}
