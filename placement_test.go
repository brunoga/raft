package raft_test

import (
	"context"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/brunoga/raft/v2"
)

// A majority says nothing about where the replicas are. Three replicas in one
// availability zone are a quorum, and losing that zone loses every write they
// acknowledged. These tests are about the check that turns "a majority has it"
// into "a majority has it, and not all in the same place".

// TestPlacement_RefusesAClusterThatCouldNeverSatisfyIt covers the
// half-configured cluster, which is the way this feature goes wrong.
//
// Asking for two zones in a cluster whose voters are all in one, or in none,
// produces a cluster that elects a leader, accepts proposals, and commits
// nothing at all. Nothing about it looks broken. Refusing at construction is
// the only point at which the mistake is cheap.
func TestPlacement_RefusesAClusterThatCouldNeverSatisfyIt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := safeBaseConfig(t, "n1")
		cfg.Peers = []raft.PeerConfig{{ID: "n2", Voter: true}, {ID: "n3", Voter: true}}
		cfg.MinCommitZones = 2
		cfg.Zones = map[raft.NodeID]raft.ZoneID{
			"n1": "eu-west-1a", "n2": "eu-west-1a", "n3": "eu-west-1a",
		}

		if err := cfg.Validate(); err == nil {
			t.Fatal("a cluster whose voters are all in one zone was accepted with MinCommitZones 2")
		} else if !strings.Contains(err.Error(), "span") {
			t.Errorf("Validate returned %v, want it to say how many zones the voters span", err)
		}

		// Spreading them makes it satisfiable.
		cfg.Zones["n3"] = "eu-west-1b"
		if err := cfg.Validate(); err != nil {
			t.Errorf("a cluster spanning two zones was refused: %v", err)
		}
	})
}

// TestPlacement_AnUnplacedNodeIsNotEvidence pins the conservative reading of a
// node missing from the map.
//
// Counting it as its own zone would mean that adding a node nobody had placed
// silently satisfied the requirement, which is exactly the outcome the
// requirement exists to prevent. A node nobody placed cannot be evidence that
// a write survived the loss of a zone.
func TestPlacement_AnUnplacedNodeIsNotEvidence(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := safeBaseConfig(t, "n1")
		cfg.Peers = []raft.PeerConfig{{ID: "n2", Voter: true}}
		cfg.MinCommitZones = 2
		cfg.Zones = map[raft.NodeID]raft.ZoneID{"n1": "a"} // n2 unplaced

		if err := cfg.Validate(); err == nil {
			t.Error("an unplaced voter was counted as a second zone")
		}
	})
}

// TestPlacement_DoesNotCommitUntilTheWriteHasLeftTheZone is the property, and
// the setup is the whole point of it.
//
// Three voters, two of them beside the leader in zone a and one alone in zone
// b. A majority is two, so the leader and its neighbour are a quorum on their
// own: without this requirement the write is acknowledged having never left
// zone a, and losing that zone loses it. With the requirement the entry must
// reach zone b first, and zone b is not answering.
func TestPlacement_DoesNotCommitUntilTheWriteHasLeftTheZone(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := safeBaseConfig(t, "n1")
		cfg.MinCommitZones = 2
		cfg.Zones = map[raft.NodeID]raft.ZoneID{"n1": "a", "n2": "a", "n3": "b"}
		cfg.Peers = []raft.PeerConfig{{ID: "n2", Voter: true}, {ID: "n3", Voter: true}}
		cfg.Transport = &selectiveTransport{silent: "n3"}

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

		ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
		defer cancel()
		if _, err := node.Propose(ctx, []byte("x")); err == nil {
			t.Fatal("a write was acknowledged by a quorum that sat entirely in one zone")
		}
	})
}

// selectiveTransport answers for every peer except one, which is silent.
type selectiveTransport struct {
	echoTransport
	silent raft.NodeID
}

func (t *selectiveTransport) RequestVote(ctx context.Context, to raft.NodeID, req *raft.RequestVoteRequest) (*raft.RequestVoteResponse, error) {
	if to == t.silent {
		return nil, context.DeadlineExceeded
	}
	return t.echoTransport.RequestVote(ctx, to, req)
}

func (t *selectiveTransport) AppendEntries(ctx context.Context, to raft.NodeID, req *raft.AppendEntriesRequest) (*raft.AppendEntriesResponse, error) {
	if to == t.silent {
		return nil, context.DeadlineExceeded
	}
	return t.echoTransport.AppendEntries(ctx, to, req)
}

// TestPlacement_CommitsOnceTheWriteReachesASecondZone is the other half: the
// requirement must not be a permanent stall when the cluster is healthy.
func TestPlacement_CommitsOnceTheWriteReachesASecondZone(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := safeBaseConfig(t, "n1")
		cfg.MinCommitZones = 2
		cfg.Zones = map[raft.NodeID]raft.ZoneID{"n1": "a", "n2": "a", "n3": "b"}
		cfg.Peers = []raft.PeerConfig{{ID: "n2", Voter: true}, {ID: "n3", Voter: true}}
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

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := node.Propose(ctx, []byte("x")); err != nil {
			t.Errorf("a write acknowledged by a second zone was not committed: %v", err)
		}
	})
}

// TestPlacement_UnsetIsAPlainMajority pins that nothing changes for a cluster
// that has not asked for this.
func TestPlacement_UnsetIsAPlainMajority(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := safeBaseConfig(t, "n1")
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

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := node.Propose(ctx, []byte("x")); err != nil {
			t.Errorf("a single-voter cluster with no zones configured did not commit: %v", err)
		}
	})
}
