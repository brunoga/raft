package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brunoga/raft"
	"github.com/brunoga/raft/storage/filestore"
	"github.com/brunoga/raft/transport/memtransport"
)

// countSM records every command it applies, so a recovered node can be asked
// whether it still has the history it had before the outage.
//
// The lock is not decoration: Raft applies on its own goroutine, and the test
// reads what was applied from its own.
type countSM struct {
	mu      sync.Mutex
	applied []string
}

func (s *countSM) Apply(_ context.Context, e raft.LogEntry) ([]byte, error) {
	if len(e.Command) == 0 {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.applied = append(s.applied, string(e.Command))
	return nil, nil
}

func (s *countSM) Snapshot(_ context.Context, w io.Writer) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := io.WriteString(w, strings.Join(s.applied, "\n"))
	return err
}

func (s *countSM) Restore(_ context.Context, _ raft.SnapshotMeta, r io.Reader) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.applied = nil
	if len(b) > 0 {
		s.applied = strings.Split(string(b), "\n")
	}
	return nil
}

// appliedCount is how many commands the state machine has recorded.
func (s *countSM) appliedCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.applied)
}

// appliedCommands returns a copy of what has been applied.
func (s *countSM) appliedCommands() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.applied...)
}

// startCluster brings up a three-node cluster on real file-backed storage and
// returns the data directories, so a test can kill it and work on what is left
// the way an operator would.
//
// snapshot controls whether the cluster compacts before it dies. It matters
// for what an operator can be told afterwards: membership reaches storage in a
// snapshot or in a configuration entry, and a cluster bootstrapped from
// Config.Peers that has done neither has never written down who its members
// are.
func startCluster(t *testing.T, snapshot bool) (dirs []string, ids []raft.NodeID) {
	t.Helper()

	ids = []raft.NodeID{"n1", "n2", "n3"}
	net := memtransport.NewNetwork()
	nodes := make([]*raft.Node, len(ids))
	stores := make([]*filestore.FileStore, len(ids))
	root := t.TempDir()

	for i, id := range ids {
		dir := filepath.Join(root, string(id))
		dirs = append(dirs, dir)

		store, err := filestore.Open(dir)
		if err != nil {
			t.Fatalf("open %s: %v", dir, err)
		}
		stores[i] = store

		peers := make([]raft.PeerConfig, 0, len(ids)-1)
		for _, other := range ids {
			if other != id {
				peers = append(peers, raft.PeerConfig{ID: other, Voter: true})
			}
		}

		cfg := raft.DefaultConfig()
		cfg.ID = id
		cfg.Peers = peers
		cfg.Storage = store
		cfg.StateMachine = &countSM{}
		cfg.Transport = net.NewTransport(id)
		cfg.TickInterval = 0
		if snapshot {
			cfg.SnapshotThreshold = 2
		} else {
			cfg.SnapshotThreshold = 0
		}
		cfg.HeartbeatInterval = 100 * time.Millisecond
		cfg.ElectionTimeoutMin = 1000 * time.Millisecond
		cfg.ElectionTimeoutMax = 2000 * time.Millisecond

		node, err := raft.New(&cfg)
		if err != nil {
			t.Fatalf("new %s: %v", id, err)
		}
		net.Register(id, node.Handler())
		node.Start()
		nodes[i] = node
	}

	tick := func() {
		for _, n := range nodes {
			n.Tick()
		}
		time.Sleep(time.Millisecond)
	}

	leader := -1
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && leader < 0 {
		for i, n := range nodes {
			if n.State() == raft.Leader {
				leader = i
			}
		}
		tick()
	}
	if leader < 0 {
		t.Fatal("no leader elected")
	}

	// Commit a few entries so the survivors have history worth keeping.
	ctx := context.Background()
	for i := range 3 {
		done := make(chan error, 1)
		go func() {
			_, err := nodes[leader].Propose(ctx, fmt.Appendf(nil, "cmd%d", i))
			done <- err
		}()
	wait:
		for {
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("propose: %v", err)
				}
				break wait
			default:
				tick()
			}
		}
	}

	if snapshot {
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) && nodes[leader].SnapshotIndex() == 0 {
			tick()
		}
		if nodes[leader].SnapshotIndex() == 0 {
			t.Fatal("no snapshot was taken")
		}
	}

	// The whole cluster dies, and its storage is released.
	for _, n := range nodes {
		n.Stop()
	}
	for _, s := range stores {
		_ = s.Close()
	}
	return dirs, ids
}

// TestRecover_BringsBackACluster walks the procedure the README documents, in
// the order it documents, and checks the cluster serves again afterwards.
//
// This is the only operation in the library that can lose data, and the only
// one whose correctness depends on the operator doing several things in the
// right order. A worked path through it that is actually executed is worth
// more than the prose it accompanies.
func TestRecover_BringsBackACluster(t *testing.T) {
	dirs, ids := startCluster(t, true)

	// Two of the three are gone for good. The operator inspects what is left.
	survivor := dirs[0]
	info, err := inspect(survivor)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if len(info.Members) != 3 {
		t.Fatalf("inspect reported %d members, want the 3 the cluster had", len(info.Members))
	}

	// Recover it as a single-voter cluster, naming the others as learners so
	// they rejoin without a separate step.
	if rerr := run([]string{
		"recover",
		"--data-dir", survivor,
		"--id", string(ids[0]),
		"--learner", string(ids[1]),
		"--learner", string(ids[2]),
		"--confirm",
	}); rerr != nil {
		t.Fatalf("recover: %v", rerr)
	}

	// The membership it will restart with is the one just written.
	after, err := inspect(survivor)
	if err != nil {
		t.Fatalf("inspect after recovery: %v", err)
	}
	voters := 0
	for _, m := range after.Members {
		if m.Voter {
			voters++
		}
	}
	if voters != 1 || len(after.Members) != 3 {
		t.Fatalf("after recovery: %d members, %d voters; want 3 members and 1 voter: %v",
			len(after.Members), voters, after.Members)
	}

	// Restarting it, it elects itself and still has what it had.
	store, err := filestore.Open(survivor)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = store.Close() }()

	sm := &countSM{}
	net := memtransport.NewNetwork()
	cfg := raft.DefaultConfig()
	cfg.ID = ids[0]
	cfg.Storage = store
	cfg.StateMachine = sm
	cfg.Transport = net.NewTransport(ids[0])
	cfg.TickInterval = 0
	cfg.HeartbeatInterval = 100 * time.Millisecond
	cfg.ElectionTimeoutMin = 1000 * time.Millisecond
	cfg.ElectionTimeoutMax = 2000 * time.Millisecond

	node, err := raft.New(&cfg)
	if err != nil {
		t.Fatalf("new after recovery: %v", err)
	}
	net.Register(ids[0], node.Handler())
	node.Start()
	defer node.Stop()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && node.State() != raft.Leader {
		node.Tick()
		time.Sleep(time.Millisecond)
	}
	if node.State() != raft.Leader {
		t.Fatal("recovered node did not elect itself")
	}

	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && sm.appliedCount() < 3 {
		node.Tick()
		time.Sleep(time.Millisecond)
	}
	if got := sm.appliedCommands(); len(got) < 3 {
		t.Errorf("recovered node replayed %v, want the 3 entries committed before the outage", got)
	}
}

// TestRecover_RefusesWithoutConfirm checks the guard on the destructive path.
// An operator reaching for this is having a bad day; making them type --confirm
// is the cheapest possible way to ensure they meant this directory.
func TestRecover_RefusesWithoutConfirm(t *testing.T) {
	dirs, ids := startCluster(t, true)

	err := run([]string{"recover", "--data-dir", dirs[0], "--id", string(ids[0])})
	if err == nil {
		t.Fatal("recover rewrote durable state without --confirm")
	}
	if !strings.Contains(err.Error(), "--confirm") {
		t.Errorf("error = %v, want it to name the flag", err)
	}

	// Nothing was written: the term is untouched.
	info, ierr := inspect(dirs[0])
	if ierr != nil {
		t.Fatalf("inspect: %v", ierr)
	}
	if len(info.Members) != 3 {
		t.Errorf("a refused recovery changed the membership: %v", info.Members)
	}
}

// TestInspect_RefusesADirectoryInUse checks that the tool cannot be pointed at
// a running node.
//
// "Stop the node first" is the first line of the procedure, and the one an
// operator under pressure is most likely to skip. The store's lock turns it
// from an instruction into something that is checked.
func TestInspect_RefusesADirectoryInUse(t *testing.T) {
	dir := t.TempDir()
	held, err := filestore.Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = held.Close() }()

	_, err = inspect(dir)
	if err == nil {
		t.Fatal("inspect opened a directory another store holds")
	}
	if !strings.Contains(err.Error(), "stop the node") {
		t.Errorf("error = %v, want it to say what to do", err)
	}
}

// TestCompare_PicksTheMostRecentSurvivor checks the choice the operator is
// least equipped to make by eye: which of several logs to keep.
func TestCompare_PicksTheMostRecentSurvivor(t *testing.T) {
	dirs, _ := startCluster(t, true)

	out := captureStdout(t, func() {
		if err := run([]string{"compare", "--data-dir", dirs[0], "--data-dir", dirs[1]}); err != nil {
			t.Fatalf("compare: %v", err)
		}
	})

	a, err := inspect(dirs[0])
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	b, err := inspect(dirs[1])
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	want := dirs[1]
	if !b.MoreRecentThan(&a) {
		want = dirs[0]
	}
	if !strings.Contains(out, "Recover "+want) {
		t.Errorf("compare recommended something other than %s:\n%s", want, out)
	}
	if !strings.Contains(out, "Erase the storage of every other survivor") {
		t.Error("compare did not tell the operator to erase the others, which is the step " +
			"that makes recovering exactly one node safe")
	}
}

// captureStdout runs fn with os.Stdout redirected and returns what it wrote.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	saved := os.Stdout
	os.Stdout = w

	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()

	fn()

	os.Stdout = saved
	_ = w.Close()
	out := <-done
	_ = r.Close()
	return out
}

// TestInspect_SaysWhenTheMembershipWasNeverWrittenDown covers the cluster that
// has never snapshotted and never reconfigured.
//
// Membership reaches storage in a snapshot or in a configuration entry. A
// cluster bootstrapped from Config.Peers that has done neither knows its
// members only from the peer list each node was started with, which no tool
// reading the disk can recover. Reporting the empty list as though it were the
// membership would tell an operator their cluster had no members.
func TestInspect_SaysWhenTheMembershipWasNeverWrittenDown(t *testing.T) {
	dirs, _ := startCluster(t, false)

	info, err := inspect(dirs[0])
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if info.MembersComplete {
		t.Errorf("MembersComplete = true for a cluster that never wrote its membership down; "+
			"members = %v", info.Members)
	}

	out := captureStdout(t, func() {
		if perr := printInfo(os.Stdout, dirs[0], &info); perr != nil {
			t.Errorf("printInfo: %v", perr)
		}
	})
	if !strings.Contains(out, "is not the") {
		t.Errorf("inspect did not say the membership is incomplete:\n%s", out)
	}
}
