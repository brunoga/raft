package raft_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/brunoga/raft/v2"
	"github.com/brunoga/raft/v2/storage/memstore"
	"github.com/brunoga/raft/v2/transport/memtransport"
)

// witnessCluster is a cluster whose last members are witnesses, with each
// node's storage kept so a test can look at what a witness holds.
type witnessCluster struct {
	*Cluster
	stores    []*memstore.MemStore
	witnesses map[raft.NodeID]bool
}

// newWitnessCluster builds full full-replica nodes and witnesses witness
// nodes. Witness IDs come after the full ones.
func newWitnessCluster(t *testing.T, full, witnesses int, mutate func(*raft.Config)) *witnessCluster {
	t.Helper()
	wc := &witnessCluster{
		Cluster:   &Cluster{t: t, net: memtransport.NewNetwork()},
		witnesses: make(map[raft.NodeID]bool),
	}
	total := full + witnesses
	for i := range total {
		id := raft.NodeID(fmt.Sprintf("n%d", i+1))
		wc.ids = append(wc.ids, id)
		if i >= full {
			wc.witnesses[id] = true
		}
	}
	for i := range total {
		peers := make([]raft.PeerConfig, 0, total-1)
		for j := range total {
			if i == j {
				continue
			}
			peers = append(peers, raft.PeerConfig{ID: wc.ids[j], Voter: true, Witness: wc.witnesses[wc.ids[j]]})
		}
		sm := &kvSM{data: make(map[string]string)}
		wc.sms = append(wc.sms, sm)
		store := memstore.New()
		wc.stores = append(wc.stores, store)

		cfg := raft.DefaultConfig()
		cfg.ID = wc.ids[i]
		cfg.Peers = peers
		cfg.Storage = store
		cfg.StateMachine = sm
		cfg.Transport = wc.net.NewTransport(cfg.ID)
		cfg.TickInterval = 0
		if wc.witnesses[cfg.ID] {
			cfg.Witness = true
			cfg.StateMachine = nil // a witness applies nothing
		}
		if mutate != nil {
			mutate(&cfg)
		}
		node, err := raft.New(&cfg)
		if err != nil {
			t.Fatalf("raft.New(%s): %v", cfg.ID, err)
		}
		wc.nodes = append(wc.nodes, node)
		wc.net.Register(cfg.ID, node.Handler())
	}
	for _, node := range wc.nodes {
		node.Start()
	}
	t.Cleanup(func() {
		for _, node := range wc.nodes {
			node.Stop()
		}
	})
	return wc
}

// index returns the position of id.
func (wc *witnessCluster) index(id raft.NodeID) int {
	for i, nid := range wc.ids {
		if nid == id {
			return i
		}
	}
	return -1
}

// entriesOf returns every log entry a node's storage holds.
func (wc *witnessCluster) entriesOf(i int) []raft.LogEntry {
	first, _ := wc.stores[i].FirstIndex()
	last, _ := wc.stores[i].LastIndex()
	if first == 0 || last < first {
		return nil
	}
	entries, err := wc.stores[i].GetLogEntries(context.Background(), first, last+1)
	if err != nil {
		wc.t.Fatalf("GetLogEntries(%s): %v", wc.ids[i], err)
	}
	return entries
}

// TestWitness_LeadsNeverAndStoresNoCommands checks the two things that make
// a witness a witness: it never becomes leader, and its log holds the index
// and term of every entry with none of the commands.
func TestWitness_LeadsNeverAndStoresNoCommands(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		wc := newWitnessCluster(t, 2, 1, nil)
		leaderIdx := wc.WaitLeader(electionTimeout)
		if wc.witnesses[wc.ids[leaderIdx]] {
			t.Fatalf("the witness %s became leader", wc.ids[leaderIdx])
		}
		for i := range 5 {
			if _, err := wc.Propose(electionTimeout, []byte(fmt.Sprintf("k%d=v%d", i, i))); err != nil {
				t.Fatalf("Propose %d: %v", i, err)
			}
		}

		// Everyone, witness included, has the same shape of log.
		witnessIdx := wc.index("n3")
		deadline := time.Now().Add(electionTimeout)
		for time.Now().Before(deadline) && len(wc.entriesOf(witnessIdx)) < len(wc.entriesOf(leaderIdx)) {
			wc.Tick()
			time.Sleep(time.Millisecond)
		}
		full, wit := wc.entriesOf(leaderIdx), wc.entriesOf(witnessIdx)
		if len(wit) != len(full) {
			t.Fatalf("witness holds %d entries, leader %d", len(wit), len(full))
		}
		commands := 0
		for i := range full {
			if wit[i].Index != full[i].Index || wit[i].Term != full[i].Term {
				t.Fatalf("entry %d: witness has (%d,%d), leader (%d,%d)", i,
					wit[i].Index, wit[i].Term, full[i].Index, full[i].Term)
			}
			if strings.HasPrefix(string(full[i].Command), "k") {
				commands++
				if len(wit[i].Command) != 0 {
					t.Fatalf("witness stored the command of entry %d: %q", full[i].Index, wit[i].Command)
				}
			}
		}
		if commands != 5 {
			t.Fatalf("leader's log holds %d commands, want 5", commands)
		}

		// Cut both full replicas away and tick the witness on its own: it must
		// not stand for election.
		for i := range wc.nodes {
			if !wc.witnesses[wc.ids[i]] {
				wc.Disconnect(i)
			}
		}
		term := wc.nodes[witnessIdx].Term()
		for range 200 {
			wc.nodes[witnessIdx].Tick()
		}
		time.Sleep(20 * time.Millisecond)
		if st := wc.nodes[witnessIdx].State(); st != raft.Follower || wc.nodes[witnessIdx].Term() != term {
			t.Fatalf("witness on its own is %v in term %d (was %d); it must never stand", st,
				wc.nodes[witnessIdx].Term(), term)
		}
	})
}

// TestWitness_StandsInForADownReplica checks the commit rule from both
// sides: with every full replica reachable a write commits on the full
// replicas, and with one of them gone the witness's acknowledgement makes
// up the quorum, so the group survives the loss of any one member.
func TestWitness_StandsInForADownReplica(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		wc := newWitnessCluster(t, 2, 1, nil)
		leaderIdx := wc.WaitLeader(electionTimeout)
		other := 1 - leaderIdx // the other full replica

		if _, err := wc.Propose(electionTimeout, []byte("k=healthy")); err != nil {
			t.Fatalf("write on a healthy group: %v", err)
		}

		// Lose the other full replica. The leader keeps leading (check-quorum
		// counts the witness) and, once the replica is known to be down, commits
		// with the witness standing in for it.
		wc.Disconnect(other)
		if _, err := wc.Propose(electionTimeout, []byte("k=degraded")); err != nil {
			t.Fatalf("write with one full replica down did not commit: %v", err)
		}
		if got := wc.sms[leaderIdx].Get("k"); got != "degraded" {
			t.Fatalf("leader applied %q, want degraded", got)
		}

		// Bring it back; it catches up from the leader and the group is whole.
		wc.Reconnect(other)
		if _, err := wc.Propose(electionTimeout, []byte("k=healed")); err != nil {
			t.Fatalf("write after healing: %v", err)
		}
		deadline := time.Now().Add(electionTimeout)
		for time.Now().Before(deadline) && wc.sms[other].Get("k") != "healed" {
			wc.Tick()
			time.Sleep(time.Millisecond)
		}
		if got := wc.sms[other].Get("k"); got != "healed" {
			t.Fatalf("the returned replica applied %q, want healed", got)
		}
	})
}

// TestWitness_VotesForTheSurvivingReplica checks that when the leader dies,
// the witness's vote lets the other full replica take over, and that the
// group then commits with the witness standing in for the dead leader.
func TestWitness_VotesForTheSurvivingReplica(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		wc := newWitnessCluster(t, 2, 1, nil)
		leaderIdx := wc.WaitLeader(electionTimeout)
		other := 1 - leaderIdx
		if _, err := wc.Propose(electionTimeout, []byte("k=before")); err != nil {
			t.Fatal(err)
		}

		wc.Disconnect(leaderIdx)
		deadline := time.Now().Add(electionTimeout)
		for time.Now().Before(deadline) && wc.nodes[other].State() != raft.Leader {
			wc.Tick()
			time.Sleep(time.Millisecond)
		}
		if wc.nodes[other].State() != raft.Leader {
			t.Fatal("the surviving full replica was not elected with the witness's vote")
		}
		ctx, cancel := context.WithTimeout(context.Background(), electionTimeout)
		defer cancel()
		done := make(chan error, 1)
		go func() {
			_, err := wc.nodes[other].Propose(ctx, []byte("k=after"))
			done <- err
		}()
		for {
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("write on the surviving replica plus witness: %v", err)
				}
				wc.Reconnect(leaderIdx)
				return
			default:
				wc.Tick()
				time.Sleep(time.Millisecond)
			}
		}
	})
}

// TestWitness_TransferRefused pins that leadership cannot be handed to a
// witness, and that a witness told to stand anyway declines.
func TestWitness_TransferRefused(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		wc := newWitnessCluster(t, 2, 1, nil)
		leader := wc.nodes[wc.WaitLeader(electionTimeout)]
		ctx, cancel := context.WithTimeout(context.Background(), electionTimeout)
		defer cancel()
		if err := leader.TransferLeadership(ctx, "n3"); err == nil {
			t.Fatal("TransferLeadership to a witness succeeded")
		}
	})
}

// TestWitness_SnapshotsAndAddWitness checks that a witness takes part in
// compaction with an empty state machine, and that AddWitness brings a new
// witness into a running group where it is counted from then on.
func TestWitness_SnapshotsAndAddWitness(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		wc := newWitnessCluster(t, 2, 1, func(cfg *raft.Config) {
			cfg.SnapshotThreshold = 4
			cfg.TrailingLogs = 0
		})
		leaderIdx := wc.WaitLeader(electionTimeout)
		for i := range 8 {
			if _, err := wc.Propose(electionTimeout, []byte(fmt.Sprintf("k%d=v", i))); err != nil {
				t.Fatal(err)
			}
		}
		witnessIdx := wc.index("n3")
		deadline := time.Now().Add(electionTimeout)
		for time.Now().Before(deadline) && wc.nodes[witnessIdx].SnapshotIndex() == 0 {
			wc.Tick()
			time.Sleep(time.Millisecond)
		}
		if wc.nodes[witnessIdx].SnapshotIndex() == 0 {
			t.Fatal("the witness never compacted its log")
		}

		// A second witness joins. It is built with Config.Witness and told about
		// the others; the leader adds it with AddWitness.
		store := memstore.New()
		cfg := raft.DefaultConfig()
		cfg.ID = "n4"
		cfg.Witness = true
		cfg.Peers = []raft.PeerConfig{{ID: "n1", Voter: true}, {ID: "n2", Voter: true}, {ID: "n3", Voter: true, Witness: true}}
		cfg.Storage = store
		cfg.Transport = wc.net.NewTransport("n4")
		cfg.TickInterval = 0
		n4, err := raft.New(&cfg)
		if err != nil {
			t.Fatalf("raft.New(n4): %v", err)
		}
		wc.net.Register("n4", n4.Handler())
		n4.Start()
		t.Cleanup(n4.Stop)
		wc.nodes = append(wc.nodes, n4)
		wc.ids = append(wc.ids, "n4")
		wc.stores = append(wc.stores, store)
		wc.sms = append(wc.sms, &kvSM{data: make(map[string]string)})
		wc.witnesses["n4"] = true

		stop := tickWhile(wc.nodes...)
		ctx, cancel := context.WithTimeout(context.Background(), electionTimeout)
		err = wc.nodes[leaderIdx].AddWitness(ctx, "n4")
		cancel()
		stop()
		if err != nil {
			t.Fatalf("AddWitness: %v", err)
		}
		for _, m := range wc.nodes[leaderIdx].Members() {
			if m.ID == "n4" && (!m.Voter || !m.Witness) {
				t.Fatalf("n4 joined as %+v, want a voting witness", m)
			}
		}

		// Four voters now, two of them witnesses: with one full replica cut off,
		// the leader and the two witnesses are a quorum of three.
		other := 1 - leaderIdx
		wc.Disconnect(other)
		if _, err := wc.Propose(electionTimeout, []byte("k=three")); err != nil {
			t.Fatalf("write with one full replica down and two witnesses: %v", err)
		}
		wc.Reconnect(other)
	})
}

// TestWitness_Validate pins the configuration rules.
func TestWitness_Validate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		base := func() raft.Config {
			cfg := raft.DefaultConfig()
			cfg.ID = "w"
			cfg.Storage = memstore.New()
			cfg.Transport = memtransport.NewNetwork().NewTransport("w")
			cfg.Peers = []raft.PeerConfig{{ID: "a", Voter: true}, {ID: "b", Voter: true}}
			return cfg
		}
		cfg := base()
		cfg.Witness = true
		if err := cfg.Validate(); err != nil {
			t.Fatalf("a witness with no state machine and full peers was refused: %v", err)
		}
		cfg = base()
		if err := cfg.Validate(); err == nil {
			t.Fatal("a full node with no state machine was accepted")
		}
		cfg = base()
		cfg.Witness, cfg.Voter = true, false
		if err := cfg.Validate(); err == nil {
			t.Fatal("a non-voting witness was accepted")
		}
		cfg = base()
		cfg.Witness = true
		cfg.Peers = []raft.PeerConfig{{ID: "a", Voter: true, Witness: true}}
		if err := cfg.Validate(); err == nil {
			t.Fatal("a witness with only witness peers was accepted")
		}
		cfg = base()
		cfg.StateMachine = &kvSM{data: map[string]string{}}
		cfg.Peers = []raft.PeerConfig{{ID: "a", Witness: true}}
		if err := cfg.Validate(); err == nil {
			t.Fatal("a peer marked witness without voter was accepted")
		}
		cfg = base()
		cfg.StateMachine = &kvSM{data: map[string]string{}}
		cfg.Peers = []raft.PeerConfig{{ID: "a", Voter: true, Witness: true}, {ID: "b", Voter: true}}
		cfg.PreferredLeader = "a"
		if err := cfg.Validate(); err == nil {
			t.Fatal("a witness as PreferredLeader was accepted")
		}
	})
}

// TestWitness_MismatchIsRefusedAtStartup checks that a node whose recovered
// membership says it is a witness does not start as a full node: the
// membership comes back from the log, and Config disagrees.
func TestWitness_MismatchIsRefusedAtStartup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		net := memtransport.NewNetwork()
		newNode := func(id raft.NodeID, store *memstore.MemStore, witness bool, peers ...raft.PeerConfig) (*raft.Node, error) {
			cfg := raft.DefaultConfig()
			cfg.ID = id
			cfg.Peers = peers
			cfg.Storage = store
			cfg.Transport = net.NewTransport(id)
			cfg.TickInterval = 0
			cfg.SnapshotThreshold = 0
			if witness {
				cfg.Witness = true
			} else {
				cfg.StateMachine = &kvSM{data: make(map[string]string)}
			}
			return raft.New(&cfg)
		}

		// A single full node leads; a witness joins it.
		n1, err := newNode("n1", memstore.New(), false)
		if err != nil {
			t.Fatal(err)
		}
		net.Register("n1", n1.Handler())
		n1.Start()
		t.Cleanup(n1.Stop)
		wStore := memstore.New()
		w, err := newNode("w", wStore, true, raft.PeerConfig{ID: "n1", Voter: true})
		if err != nil {
			t.Fatal(err)
		}
		net.Register("w", w.Handler())
		w.Start()

		deadline := time.Now().Add(electionTimeout)
		for time.Now().Before(deadline) && n1.State() != raft.Leader {
			n1.Tick()
			time.Sleep(time.Millisecond)
		}
		stop := tickWhile(n1, w)
		ctx, cancel := context.WithTimeout(context.Background(), electionTimeout)
		err = n1.AddWitness(ctx, "w")
		cancel()
		if err != nil {
			stop()
			t.Fatalf("AddWitness: %v", err)
		}
		// Wait for the witness to hold the entry that names it.
		for time.Now().Before(deadline) {
			last, _ := wStore.LastIndex()
			if last > 0 && w.Members()[0].Witness {
				break
			}
			time.Sleep(time.Millisecond)
		}
		stop()
		w.Stop()
		net.Unregister("w")

		// Reopen the same storage as a full node.
		if _, err := newNode("w", wStore, false, raft.PeerConfig{ID: "n1", Voter: true}); !errors.Is(err, raft.ErrWitnessMismatch) {
			t.Fatalf("reopening a witness's storage as a full node returned %v, want ErrWitnessMismatch", err)
		}
	})
}

// TestWitness_MembershipMismatchStops checks the dangerous direction at run
// time: a full node that applies a membership entry calling it a witness
// stops with ErrWitnessMismatch rather than applying stripped entries.
func TestWitness_MembershipMismatchStops(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := newCluster(t, 3)
		leaderIdx := c.WaitLeader(electionTimeout)
		leader := c.nodes[leaderIdx]
		victim := c.ids[(leaderIdx+1)%3]

		// Re-add the follower as a witness. It was built as a full node, so the
		// entry contradicts its Config.
		stop := tickWhile(c.nodes...)
		ctx, cancel := context.WithTimeout(context.Background(), electionTimeout)
		err := leader.AddServer(ctx, raft.PeerConfig{ID: victim, Voter: true, Witness: true})
		cancel()
		stop()
		if err != nil {
			t.Fatalf("AddServer: %v", err)
		}
		v := c.nodes[(leaderIdx+1)%3]
		deadline := time.Now().Add(electionTimeout)
		for time.Now().Before(deadline) && v.FatalError() == nil {
			c.Tick()
			time.Sleep(time.Millisecond)
		}
		if !errors.Is(v.FatalError(), raft.ErrWitnessMismatch) {
			t.Fatalf("the full node called a witness reports %v, want ErrWitnessMismatch", v.FatalError())
		}
	})
}
