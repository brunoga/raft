package raft_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/brunoga/raft/v2"
	"github.com/brunoga/raft/v2/storage/memstore"
	"github.com/brunoga/raft/v2/transport/memtransport"
)

// These tests are about configurations that are accepted, start cleanly, and
// then do not work. A Raft node that refuses to start tells you what is wrong;
// one that starts and quietly never elects a leader, or serves a read it
// should not have, tells you nothing at all.

func safeBaseConfig(t *testing.T, id raft.NodeID) raft.Config {
	t.Helper()
	cfg := raft.DefaultConfig()
	cfg.ID = id
	cfg.Storage = memstore.New()
	cfg.StateMachine = idleSM{}
	cfg.Transport = memtransport.NewNetwork().NewTransport(id)
	cfg.TickInterval = 0
	return cfg
}

// TestConfig_RejectsAClusterWithNoVoters covers the shape a peer list takes
// when the roles are left out.
//
// Voter is a bool, so its zero value is false, and a peer list written as
// []PeerConfig{{ID: "n2"}, {ID: "n3"}} is a list of learners. Nothing about
// the deployment then looks broken: every node starts, every node reports
// itself a follower, and no election is ever held. The cluster simply never
// does anything, and the logs give no reason.
func TestConfig_RejectsAClusterWithNoVoters(t *testing.T) {
	cfg := safeBaseConfig(t, "n1")
	cfg.Voter = false
	cfg.Peers = []raft.PeerConfig{{ID: "n2"}, {ID: "n3"}}

	if err := cfg.Validate(); err == nil {
		t.Fatal("a configuration in which nothing votes was accepted")
	}
	if _, err := raft.New(&cfg); err == nil {
		t.Error("New accepted a configuration in which nothing votes")
	}

	// One voter anywhere is enough to make it a cluster that can work.
	cfg.Peers = []raft.PeerConfig{{ID: "n2", Voter: true}, {ID: "n3"}}
	if err := cfg.Validate(); err != nil {
		t.Errorf("a learner joining a cluster that has a voter was rejected: %v", err)
	}
}

// TestConfig_AllowsALearnerWithNoPeersYet pins the case the rule above must
// not catch. A node started with no peers is waiting to be added to a cluster
// it has not been told about, which is how joining works; refusing it would
// refuse the join.
func TestConfig_AllowsALearnerWithNoPeersYet(t *testing.T) {
	cfg := safeBaseConfig(t, "joiner")
	cfg.Voter = false
	cfg.Peers = nil

	if err := cfg.Validate(); err != nil {
		t.Fatalf("a node waiting to be added to a cluster was rejected: %v", err)
	}
}

// TestConfig_LeaseReadsRequireCheckQuorum covers a combination that is not
// broken so much as silently unsafe.
//
// A lease read is only as good as the promise that a leader which has lost
// contact with its cluster stops being one, and check-quorum is what makes
// that true. Without it a partitioned leader keeps its lease, keeps believing
// it leads, and answers reads from a state machine the rest of the cluster has
// moved past -- for as long as the partition lasts, with nothing logged.
//
// Refusing the read makes the combination impossible to hold by accident. The
// caller is told which of the two to change.
func TestConfig_LeaseReadsRequireCheckQuorum(t *testing.T) {
	cfg := safeBaseConfig(t, "solo")
	cfg.CheckQuorum = false

	node, err := raft.New(&cfg)
	if err != nil {
		t.Fatalf("raft.New: %v", err)
	}
	node.Start()
	t.Cleanup(node.Stop)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := node.ReadIndexLease(ctx); !errors.Is(err, raft.ErrLeaseReadUnavailable) {
		t.Errorf("ReadIndexLease with check-quorum disabled returned %v, want raft.ErrLeaseReadUnavailable", err)
	}

	// The read that needs no such assumption is still available.
	stop := tickWhile(node)
	deadline := time.Now().Add(5 * time.Second)
	for node.State() != raft.Leader {
		if time.Now().After(deadline) {
			stop()
			t.Fatal("the node never became leader")
		}
		time.Sleep(time.Millisecond)
	}
	stop()
	if _, err := node.ReadIndex(ctx); err != nil {
		t.Errorf("ReadIndex with check-quorum disabled returned %v, want it to work", err)
	}
}
