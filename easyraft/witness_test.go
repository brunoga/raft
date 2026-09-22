package easyraft_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/brunoga/raft/v2"
	"github.com/brunoga/raft/v2/easyraft"
)

// TestWitness_VotesWithoutHoldingData builds the deployment a witness exists
// for -- two full replicas and a witness -- and checks the three things that
// make it worth having: the cluster works, the witness holds nothing, and it
// says so rather than answering an empty "not found".
func TestWitness_VotesWithoutHoldingData(t *testing.T) {
	dir := t.TempDir()
	addrs := map[raft.NodeID]string{"n1": freePort(t), "n2": freePort(t), "w1": freePort(t)}

	build := func(id raft.NodeID, witness bool) *easyraft.Store {
		t.Helper()
		opts := []easyraft.Option{
			easyraft.WithID(id),
			easyraft.WithRaftAddr(addrs[id]),
			easyraft.WithDataDir(filepath.Join(dir, string(id))),
			easyraft.WithPeers(addrs),
			easyraft.WithWitnessPeers("w1"),
			easyraft.WithInsecureTransportAcknowledged(),
		}
		if witness {
			opts = append(opts, easyraft.WithWitness())
		}
		s, err := easyraft.NewStore(opts...)
		if err != nil {
			t.Fatalf("NewStore(%s): %v", id, err)
		}
		return s
	}

	n1, n2, w1 := build("n1", false), build("n2", false), build("w1", true)
	counters := map[*easyraft.Store]*easyraft.Collection[Counter]{
		n1: easyraft.AddCollection[Counter](n1, "counters"),
		n2: easyraft.AddCollection[Counter](n2, "counters"),
		w1: easyraft.AddCollection[Counter](w1, "counters"),
	}
	for _, s := range []*easyraft.Store{n1, n2, w1} {
		if err := s.Start(); err != nil {
			t.Fatalf("Start: %v", err)
		}
		t.Cleanup(func() { _ = s.Stop() })
	}

	// A write commits: the witness is a voter, so two of the three members
	// that must agree can be one full replica and it.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var leader *easyraft.Store
	for ctx.Err() == nil && leader == nil {
		for _, s := range []*easyraft.Store{n1, n2} {
			if err := counters[s].Create(ctx, "k", Counter{Value: 1}); err == nil {
				leader = s
				break
			}
		}
		if leader == nil {
			time.Sleep(100 * time.Millisecond)
		}
	}
	if leader == nil {
		t.Fatal("no write committed in a cluster of two replicas and a witness")
	}

	// The witness never leads, whoever else does.
	if w1.Status().State == raft.Leader {
		t.Error("the witness became leader")
	}
	if !w1.Status().Witness {
		t.Error("the witness does not report itself as one")
	}

	// It votes and counts, so the membership is three voters.
	voters := 0
	for _, m := range leader.Members() {
		if m.Voter {
			voters++
		}
	}
	if voters != 3 {
		t.Errorf("membership has %d voters, want 3", voters)
	}

	// And it holds nothing, and says so. An empty answer here would report
	// "not found" for a key the cluster holds.
	if _, err := counters[w1].Read(ctx, "k"); !errors.Is(err, easyraft.ErrWitness) {
		t.Errorf("Read on a witness returned %v, want ErrWitness", err)
	}
	if _, err := counters[w1].ReadStale("k"); !errors.Is(err, easyraft.ErrWitness) {
		t.Errorf("ReadStale on a witness returned %v, want ErrWitness", err)
	}
	if _, err := counters[w1].List(ctx); !errors.Is(err, easyraft.ErrWitness) {
		t.Errorf("List on a witness returned %v, want ErrWitness", err)
	}
	if _, err := counters[w1].ListStale(); !errors.Is(err, easyraft.ErrWitness) {
		t.Errorf("ListStale on a witness returned %v, want ErrWitness", err)
	}

	// The full replicas do hold it.
	other := n2
	if leader == n2 {
		other = n1
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if v, err := counters[other].ReadStale("k"); err == nil && v.Value == 1 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Error("the other full replica never saw the committed write")
}

// TestStore_ExposesGroupPolicies checks that the knobs which are group state
// rather than node configuration are readable and settable through the store.
func TestStore_ExposesGroupPolicies(t *testing.T) {
	s, err := easyraft.NewStore(
		easyraft.WithID("n1"),
		easyraft.WithRaftAddr(freePort(t)),
		easyraft.WithDataDir(t.TempDir()),
		easyraft.WithInsecureTransportAcknowledged(),
		easyraft.WithMaxClientTableSize(4096),
		easyraft.WithLeaseSafetyMargin(20*time.Millisecond),
		easyraft.WithProposalQueue(64, raft.ProposalOverflowReject),
	)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	// Subscribed before the node starts, so the leadership change it is about
	// to make is delivered rather than raced for: the stream carries what
	// happens after a subscription, not what happened before it.
	events, stop := s.Events()
	defer stop()

	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = s.Stop() })

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := s.Ready(ctx); err != nil {
		t.Fatalf("Ready: %v", err)
	}

	if got := s.MaxClientTableSize(); got != 4096 {
		t.Errorf("MaxClientTableSize = %d, want the configured 4096", got)
	}
	if got := s.CommitQuorum(); got != 0 {
		t.Errorf("CommitQuorum = %d, want 0 for a majority", got)
	}
	if err := s.SetCommitQuorum(ctx, 1); err != nil {
		t.Fatalf("SetCommitQuorum: %v", err)
	}
	if got := s.CommitQuorum(); got != 1 {
		t.Errorf("CommitQuorum = %d after setting it to 1", got)
	}
	if err := s.SetMaxClientTableSize(ctx, 128); err != nil {
		t.Fatalf("SetMaxClientTableSize: %v", err)
	}
	if got := s.MaxClientTableSize(); got != 128 {
		t.Errorf("MaxClientTableSize = %d after setting it to 128", got)
	}

	// The event stream is the other thing a service needs and could not reach
	// through this layer at all.
	deadline := time.After(10 * time.Second)
	for {
		select {
		case ev := <-events:
			if ev.Type == raft.EventLeadershipChanged && ev.IsLeader {
				return
			}
		case <-deadline:
			t.Fatal("the node became leader but no leadership event arrived")
		}
	}
}
