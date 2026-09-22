package raft_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/brunoga/raft/v2"
)

// TestAddVoter_StagesThroughLearnerRatherThanWeakeningTheQuorum is the reason
// this exists.
//
// Adding a node straight in as a voter makes it count towards every quorum
// from the moment the change commits, while its log is still empty. A
// three-node cluster becomes a four-node cluster needing three votes, one of
// which cannot be given until the new node has caught up, so for the length of
// that catch-up the cluster tolerates no failures at all. On a large state
// machine that is minutes, and it is the worst possible moment to have spent
// the cluster's redundancy.
//
// AddVoter never does that: the node is a learner, counting towards nothing,
// until it is caught up.
func TestAddVoter_StagesThroughLearnerRatherThanWeakeningTheQuorum(t *testing.T) {
	cfg := safeBaseConfig(t, "n1")
	// A peer that answers, so the learner can actually catch up. Without one
	// this would block for ever, which is itself the correct behaviour: a
	// learner that never catches up is never promoted.
	cfg.Transport = &echoTransport{sent: make(chan raft.Index, 64)}
	node, err := raft.New(&cfg)
	if err != nil {
		t.Fatalf("raft.New: %v", err)
	}
	node.Start()
	t.Cleanup(node.Stop)

	events, stopEvents := node.Events()
	defer stopEvents()

	stop := tickWhile(node)
	defer stop()
	deadline := time.Now().Add(5 * time.Second)
	for node.State() != raft.Leader {
		if time.Now().After(deadline) {
			t.Fatal("the node never became leader")
		}
		time.Sleep(time.Millisecond)
	}

	// A lag budget the new member is inside from the start, so this test is
	// about the order the two steps happen in rather than about how long a
	// catch-up takes. The waiting is covered by the next test.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := node.AddVoter(ctx, "n2", 1024); err != nil {
		t.Fatalf("AddVoter: %v", err)
	}

	// It must have arrived as a learner and been promoted afterwards, in that
	// order. Arriving as a voter is the thing this prevents.
	var sawAdd, sawPromote bool
	giveUp := time.After(5 * time.Second)
	for !sawPromote {
		select {
		case ev := <-events:
			switch ev.Type {
			case raft.EventPeerAdded:
				if ev.Peer != "n2" {
					continue
				}
				if ev.Voter {
					t.Fatal("the new node was added as a voter; the quorum was weakened while it caught up")
				}
				sawAdd = true
			case raft.EventPeerPromoted:
				if ev.Peer != "n2" {
					continue
				}
				if !sawAdd {
					t.Error("the node was promoted before it was ever added as a learner")
				}
				sawPromote = true
			}
		case <-giveUp:
			t.Fatalf("no promotion event (added=%v)", sawAdd)
		}
	}

	for _, m := range node.Members() {
		if m.ID == "n2" && !m.Voter {
			t.Error("the node is still a learner after AddVoter returned")
		}
	}
}

// TestAddVoter_IsANoOpForAnExistingVoter pins that it can be called again
// safely, which matters because the caller is told to retry it on the new
// leader when leadership moves mid-way.
func TestAddVoter_IsANoOpForAnExistingVoter(t *testing.T) {
	cfg := safeBaseConfig(t, "n1")
	cfg.Peers = []raft.PeerConfig{{ID: "n2", Voter: true}}
	node, err := raft.New(&cfg)
	if err != nil {
		t.Fatalf("raft.New: %v", err)
	}
	node.Start()
	t.Cleanup(node.Stop)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := node.AddVoter(ctx, "n2", 0); err != nil {
		t.Errorf("AddVoter on an existing voter returned %v, want nil", err)
	}
}

// TestAddVoter_RefusesAnEmptyID pins the obvious mistake, because a config
// entry naming nobody is one that cannot be undone by naming nobody again.
func TestAddVoter_RefusesAnEmptyID(t *testing.T) {
	cfg := safeBaseConfig(t, "n1")
	node, err := raft.New(&cfg)
	if err != nil {
		t.Fatalf("raft.New: %v", err)
	}
	node.Start()
	t.Cleanup(node.Stop)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := node.AddVoter(ctx, "", 0); err == nil {
		t.Error("AddVoter accepted an empty node ID")
	} else if errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("AddVoter blocked on an empty node ID rather than refusing it: %v", err)
	}
}

// TestAddVoter_WaitsForALearnerThatIsBehind is the half that matters.
//
// Staging through a learner is only worth anything if the promotion actually
// waits. A node that is added as a learner and promoted a moment later, before
// it has caught up, has cost the cluster exactly as much redundancy as adding
// it as a voter would have: it counts towards the quorum and cannot answer.
func TestAddVoter_WaitsForALearnerThatIsBehind(t *testing.T) {
	cfg := safeBaseConfig(t, "n1")
	cfg.Transport = &echoTransport{sent: make(chan raft.Index, 64)}
	node, err := raft.New(&cfg)
	if err != nil {
		t.Fatalf("raft.New: %v", err)
	}
	node.Start()
	t.Cleanup(node.Stop)

	stop := tickWhile(node)
	defer stop()
	deadline := time.Now().Add(5 * time.Second)
	for node.State() != raft.Leader {
		if time.Now().After(deadline) {
			t.Fatal("the node never became leader")
		}
		time.Sleep(time.Millisecond)
	}

	// maxLag of zero: the member must have replicated everything. This peer
	// never will, so AddVoter must keep waiting rather than promote it.
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	err = node.AddVoter(ctx, "n2", 0)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("AddVoter returned %v for a member that never caught up, want the context deadline", err)
	}

	// It is a member, and it is still a learner.
	found := false
	for _, m := range node.Members() {
		if m.ID != "n2" {
			continue
		}
		found = true
		if m.Voter {
			t.Error("a member that never caught up was promoted to voter anyway")
		}
	}
	if !found {
		t.Error("the member was not added as a learner at all")
	}
}
