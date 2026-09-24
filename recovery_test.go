package raft_test

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brunoga/raft/v2"
	"github.com/brunoga/raft/v2/storage/memstore"
	"github.com/brunoga/raft/v2/transport/memtransport"
)

// ---- Harness ----------------------------------------------------------------

// recoverySM records the commands it is asked to apply, so a test can tell
// whether a recovered node still holds the history it had before. Leader no-op
// entries arrive with an empty command and are not part of that history.
type recoverySM struct {
	mu      sync.Mutex
	applied []string
}

func (s *recoverySM) Apply(_ context.Context, e raft.LogEntry) ([]byte, error) {
	if len(e.Command) == 0 {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.applied = append(s.applied, string(e.Command))
	return e.Command, nil
}

func (s *recoverySM) Snapshot(_ context.Context, w io.Writer) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := io.WriteString(w, strings.Join(s.applied, "\n"))
	return err
}

func (s *recoverySM) Restore(_ context.Context, _ raft.SnapshotMeta, r io.Reader) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.applied = nil
	if len(data) > 0 {
		s.applied = strings.Split(string(data), "\n")
	}
	return nil
}

func (s *recoverySM) snapshotApplied() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.applied...)
}

// recoveryNode starts a node over storage and a state machine the caller owns,
// so both can outlive it and be handed to a replacement.
func recoveryNode(t *testing.T, net *memtransport.Network, store raft.Storage, sm raft.StateMachine, id raft.NodeID, peers []raft.PeerConfig, opts ...func(*raft.Config)) *raft.Node {
	t.Helper()

	cfg := raft.DefaultConfig()
	cfg.ID = id
	cfg.Peers = peers
	cfg.Storage = store
	cfg.StateMachine = sm
	cfg.Transport = net.NewTransport(id)
	cfg.TickInterval = 0
	tuneForManualTicks(&cfg)
	for _, opt := range opts {
		opt(&cfg)
	}

	node, err := raft.New(&cfg)
	if err != nil {
		t.Fatalf("raft.New(%s): %v", id, err)
	}
	net.Register(id, node.Handler())
	node.Start()
	return node
}

// peersExcept returns the voting peer list for id in a cluster of all.
func peersExcept(all []raft.NodeID, id raft.NodeID) []raft.PeerConfig {
	peers := make([]raft.PeerConfig, 0, len(all)-1)
	for _, other := range all {
		if other != id {
			peers = append(peers, raft.PeerConfig{ID: other, Voter: true})
		}
	}
	return peers
}

// ---- The headline case ------------------------------------------------------

// TestRecoverCluster_RestoresAvailabilityAfterQuorumLoss is the whole point of
// the feature. A three-node cluster loses two nodes for good. The survivor has
// every committed entry, but it is one voter out of three, so it can never win
// an election, can never commit, and therefore can never commit the membership
// change that would shrink the cluster to a size it is a majority of. Without a
// way to rewrite its durable state from outside, the data is intact and
// permanently unreachable.
//
// The test asserts both halves: that the survivor is stuck on its own, and that
// after recovery it serves again with its history intact.
func TestRecoverCluster_RestoresAvailabilityAfterQuorumLoss(t *testing.T) {
	ctx := context.Background()
	ids := []raft.NodeID{"n1", "n2", "n3"}

	stores := make([]*memstore.MemStore, len(ids))
	nodes := make([]*raft.Node, len(ids))
	net := memtransport.NewNetwork()
	for i, id := range ids {
		stores[i] = memstore.New()
		// Snapshot eagerly, so that the membership reaches storage: a cluster
		// that has never snapshotted knows its members only from the peer list
		// its operator passes to New, which is the case
		// TestInspectStorage_FlagsMembershipItCannotKnow covers.
		nodes[i] = recoveryNode(t, net, stores[i], &recoverySM{}, id, peersExcept(ids, id),
			func(cfg *raft.Config) { cfg.SnapshotThreshold, cfg.TrailingLogs = 2, 1 })
	}

	tickAll := func() {
		for _, n := range nodes {
			n.Tick()
		}
		time.Sleep(time.Millisecond)
	}

	tickUntilAll := func(timeout time.Duration, cond func() bool) bool {
		deadline := time.Now().Add(timeout)
		for !cond() {
			if time.Now().After(deadline) {
				return cond()
			}
			tickAll()
		}
		return true
	}

	// Elect a leader and commit a few entries through it.
	leader := -1
	findLeader := func() bool {
		for i, n := range nodes {
			if n.State() == raft.Leader {
				leader = i
				return true
			}
		}
		return false
	}
	if !tickUntilAll(10*time.Second, findLeader) {
		t.Fatal("no leader elected")
	}

	want := []string{"a", "b", "c"}
	for _, cmd := range want {
		done := make(chan error, 1)
		go func() {
			_, err := nodes[leader].Propose(ctx, []byte(cmd))
			done <- err
		}()
	propose:
		for {
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("propose %q: %v", cmd, err)
				}
				break propose
			default:
				tickAll()
			}
		}
	}

	if !tickUntilAll(10*time.Second, func() bool { return nodes[leader].SnapshotIndex() > 0 }) {
		t.Fatal("leader never took a snapshot")
	}

	// The whole cluster goes down; only the leader's storage survives.
	for _, n := range nodes {
		n.Stop()
	}
	survivor, survivorStore := ids[leader], stores[leader]

	// Half one: on its own, with the membership it recorded, it is stuck. A
	// fresh network stands in for the two nodes being gone rather than merely
	// unreachable: nothing answers, and nothing ever will.
	isolated := memtransport.NewNetwork()
	stuck := recoveryNode(t, isolated, survivorStore, &recoverySM{}, survivor, peersExcept(ids, survivor))
	for range 500 {
		if stuck.State() == raft.Leader {
			t.Fatalf("%s became leader as one voter out of three; a minority must not be able to elect", survivor)
		}
		stuck.Tick()
		time.Sleep(time.Millisecond)
	}
	stuck.Stop()

	// Half two: rewrite the membership to one the survivor is a majority of.
	info, err := raft.InspectStorage(ctx, survivorStore)
	if err != nil {
		t.Fatalf("InspectStorage: %v", err)
	}
	if len(info.Members) != 3 || !info.MembersComplete {
		t.Fatalf("InspectStorage reported %d members (complete=%v), want the 3 the cluster had: %v",
			len(info.Members), info.MembersComplete, info.Members)
	}
	report, err := raft.RecoverCluster(ctx, survivorStore, survivor,
		[]raft.PeerConfig{{ID: survivor, Voter: true}})
	if err != nil {
		t.Fatalf("RecoverCluster: %v", err)
	}
	if report.DiscardedFrom != 0 || report.DiscardedTo != 0 {
		t.Errorf("report discarded [%d,%d]; the default keeps the whole log",
			report.DiscardedFrom, report.DiscardedTo)
	}

	recoveredNet := memtransport.NewNetwork()
	sm := &recoverySM{}
	recovered := recoveryNode(t, recoveredNet, survivorStore, sm, survivor, nil)
	t.Cleanup(recovered.Stop)

	if !tickUntil(recovered, 5*time.Second, func() bool { return recovered.State() == raft.Leader }) {
		t.Fatal("recovered node never became leader")
	}
	if got := recovered.Members(); len(got) != 1 || got[0].ID != survivor {
		t.Errorf("recovered membership = %v, want just %s", got, survivor)
	}

	// Its history is the one it had: the entries committed before the outage
	// are still there, replayed into a state machine that starts empty.
	if !tickUntil(recovered, 5*time.Second, func() bool { return len(sm.snapshotApplied()) >= len(want) }) {
		t.Fatalf("recovered node applied %v, want the %d entries committed before the outage",
			sm.snapshotApplied(), len(want))
	}
	got := sm.snapshotApplied()
	for i, cmd := range want {
		if got[i] != cmd {
			t.Errorf("applied[%d] = %q, want %q (full: %v)", i, got[i], cmd, got)
		}
	}

	// And it accepts new work, which is what being stuck prevented.
	done := make(chan error, 1)
	go func() {
		_, err := recovered.Propose(ctx, []byte("d"))
		done <- err
	}()
	for {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("propose after recovery: %v", err)
			}
			return
		default:
			recovered.Tick()
			time.Sleep(time.Millisecond)
		}
	}
}

// ---- InspectStorage ---------------------------------------------------------

// TestInspectStorage_ReportsMembershipFromLog checks that the membership an
// operator is shown is the one the node would restart with, which per §4.1 is
// the last configuration in the log whether or not it committed. Reporting only
// committed configuration would hide exactly the case an operator is most
// likely to be recovering from: a reconfiguration that was in flight when the
// cluster died.
func TestInspectStorage_ReportsMembershipFromLog(t *testing.T) {
	ctx := context.Background()
	store := memstore.New()
	net := memtransport.NewNetwork()

	node := recoveryNode(t, net, store, &recoverySM{}, "n1", nil,
		func(cfg *raft.Config) { cfg.SnapshotThreshold = 0 }) // never compact
	if !tickUntil(node, 3*time.Second, func() bool { return node.State() == raft.Leader }) {
		t.Fatal("node never became leader")
	}
	// A reconfiguration rather than a bare add, so the log records a whole
	// configuration and nothing has to be assumed about what came before it.
	err := node.ReconfigureCluster(ctx, []raft.PeerConfig{
		{ID: "n1", Voter: true},
		{ID: "n2", Voter: false},
	})
	if err != nil {
		t.Fatalf("ReconfigureCluster: %v", err)
	}
	if !tickUntil(node, 3*time.Second, func() bool { return hasMember(node.Members(), "n2") }) {
		t.Fatalf("reconfiguration never took effect: %v", node.Members())
	}
	node.Stop()

	info, err := raft.InspectStorage(ctx, store)
	if err != nil {
		t.Fatalf("InspectStorage: %v", err)
	}
	if info.SnapshotIndex != 0 {
		t.Fatalf("SnapshotIndex = %d, want 0: this test is about the log", info.SnapshotIndex)
	}
	if !hasMember(info.Members, "n1") || !hasMember(info.Members, "n2") || len(info.Members) != 2 {
		t.Errorf("Members = %v, want exactly n1 and n2", info.Members)
	}
	if !info.MembersComplete {
		t.Error("MembersComplete = false, want true: the log records the whole configuration")
	}
	if info.JointMembers != nil {
		t.Errorf("JointMembers = %v, want nil for a completed reconfiguration", info.JointMembers)
	}
	if info.Term == 0 {
		t.Error("Term = 0, want the term the node had reached")
	}
	if info.LastIndex == 0 {
		t.Error("LastIndex = 0, want the last entry the node holds")
	}
	if info.LastTerm != info.Term {
		t.Errorf("LastTerm = %d, want %d: the last entry was written by this node as leader",
			info.LastTerm, info.Term)
	}
}

// TestInspectStorage_FlagsMembershipItCannotKnow covers the node whose storage
// simply does not record who the cluster is: no snapshot, and a log holding
// only adjustments to a configuration that was never written down. Its
// membership comes from the peer list its operator passes to New, and no tool
// reading its disk can reconstruct that.
//
// Reporting the adjustments as though they were the membership would be worse
// than reporting nothing: an operator recovering a cluster would be shown a
// members list missing the very node they are recovering.
func TestInspectStorage_FlagsMembershipItCannotKnow(t *testing.T) {
	ctx := context.Background()
	store := memstore.New()
	net := memtransport.NewNetwork()

	node := recoveryNode(t, net, store, &recoverySM{}, "n1", nil,
		func(cfg *raft.Config) { cfg.SnapshotThreshold = 0 }) // never compact
	if !tickUntil(node, 3*time.Second, func() bool { return node.State() == raft.Leader }) {
		t.Fatal("node never became leader")
	}
	if err := node.AddServer(ctx, raft.PeerConfig{ID: "n2", Voter: false}); err != nil {
		t.Fatalf("AddServer: %v", err)
	}
	node.Stop()

	info, err := raft.InspectStorage(ctx, store)
	if err != nil {
		t.Fatalf("InspectStorage: %v", err)
	}
	if info.MembersComplete {
		t.Errorf("MembersComplete = true for a node whose own membership was never written to storage; Members = %v",
			info.Members)
	}
}

// TestInspectStorage_ReportsMembershipFromSnapshot covers the node whose log has
// been compacted past the configuration entries. The membership is then only in
// the snapshot, and LastIndex has to come from there too: a node whose log is
// empty still accounts for everything up to its snapshot, and an operator
// comparing survivors on LastIndex alone would otherwise rank the node with the
// most compaction as the one with the least data.
func TestInspectStorage_ReportsMembershipFromSnapshot(t *testing.T) {
	ctx := context.Background()
	store := memstore.New()
	net := memtransport.NewNetwork()

	cfg := raft.DefaultConfig()
	cfg.ID = "n1"
	cfg.Storage = store
	cfg.StateMachine = &recoverySM{}
	cfg.Transport = net.NewTransport("n1")
	cfg.TickInterval = 0
	cfg.SnapshotThreshold = 2
	cfg.TrailingLogs = 0
	tuneForManualTicks(&cfg)

	node, newErr := raft.New(&cfg)
	if newErr != nil {
		t.Fatalf("raft.New: %v", newErr)
	}
	net.Register("n1", node.Handler())
	node.Start()

	if !tickUntil(node, 3*time.Second, func() bool { return node.State() == raft.Leader }) {
		t.Fatal("node never became leader")
	}
	if err := node.AddServer(ctx, raft.PeerConfig{ID: "n2", Voter: false}); err != nil {
		t.Fatalf("AddServer: %v", err)
	}
	for range 8 {
		if _, err := node.Propose(ctx, []byte("x")); err != nil {
			t.Fatalf("propose: %v", err)
		}
	}
	if !tickUntil(node, 5*time.Second, func() bool { return node.SnapshotIndex() > 0 }) {
		t.Fatal("node never took a snapshot")
	}
	node.Stop()

	info, err := raft.InspectStorage(ctx, store)
	if err != nil {
		t.Fatalf("InspectStorage: %v", err)
	}
	if info.SnapshotIndex == 0 {
		t.Fatal("SnapshotIndex = 0, want the snapshot the node took")
	}
	if !hasMember(info.Members, "n1") || !hasMember(info.Members, "n2") {
		t.Errorf("Members = %v, want n1 and n2 recovered from the snapshot", info.Members)
	}
	if !info.MembersComplete {
		t.Error("MembersComplete = false, want true: the snapshot records the whole membership")
	}
	if info.LastIndex < info.SnapshotIndex {
		t.Errorf("LastIndex = %d, below SnapshotIndex %d: a compacted node still accounts for its snapshot",
			info.LastIndex, info.SnapshotIndex)
	}
}

// TestRecoveryInfo_MoreRecentThan pins the comparison an operator uses to pick
// which survivor's history to keep. It is the §5.4.1 up-to-date rule: a higher
// last term always wins, and only then does length matter. Comparing length
// first would pick a node with a long run of stale entries over one that has
// seen a later leader.
func TestRecoveryInfo_MoreRecentThan(t *testing.T) {
	info := func(term raft.Term, index raft.Index) raft.RecoveryInfo {
		return raft.RecoveryInfo{LastTerm: term, LastIndex: index}
	}
	cases := []struct {
		name  string
		a, b  raft.RecoveryInfo
		wantA bool
	}{
		{"higher term wins despite a shorter log", info(5, 10), info(4, 99), true},
		{"lower term loses despite a longer log", info(4, 99), info(5, 10), false},
		{"same term, longer log wins", info(5, 20), info(5, 10), true},
		{"same term, shorter log loses", info(5, 10), info(5, 20), false},
		{"identical is not more recent", info(5, 10), info(5, 10), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.a.MoreRecentThan(&tc.b); got != tc.wantA {
				t.Errorf("MoreRecentThan = %v, want %v", got, tc.wantA)
			}
		})
	}
}

// ---- Validation -------------------------------------------------------------

// TestRecoverCluster_RejectsUnusableMembership checks that a membership which
// cannot produce a working cluster is refused before anything is written.
// Recovery is not reversible: it promotes uncommitted entries to committed and
// is run against storage whose peers are gone. Discovering only on restart that
// the new membership still has no quorum would leave the operator with a
// rewritten log and the same outage.
func TestRecoverCluster_RejectsUnusableMembership(t *testing.T) {
	ctx := context.Background()
	voter := func(id raft.NodeID) raft.PeerConfig { return raft.PeerConfig{ID: id, Voter: true} }

	cases := []struct {
		name    string
		self    raft.NodeID
		members []raft.PeerConfig
	}{
		{"empty self", "", []raft.PeerConfig{voter("n1")}},
		{"empty membership", "n1", nil},
		{"self absent", "n1", []raft.PeerConfig{voter("n2")}},
		{"self is not a voter", "n1", []raft.PeerConfig{{ID: "n1", Voter: false}, voter("n2")}},
		{"self cannot reach quorum alone", "n1", []raft.PeerConfig{voter("n1"), voter("n2")}},
		{"duplicate member", "n1", []raft.PeerConfig{voter("n1"), voter("n1")}},
		{"empty member ID", "n1", []raft.PeerConfig{voter("n1"), {ID: "", Voter: false}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := memstore.New()
			if _, err := raft.RecoverCluster(ctx, store, tc.self, tc.members); err == nil {
				t.Fatal("RecoverCluster accepted a membership that cannot form a cluster")
			}
			// Nothing may have been written: a refused recovery must leave the
			// storage exactly as it was, so the operator can try again.
			last, err := store.LastIndex()
			if err != nil {
				t.Fatalf("LastIndex: %v", err)
			}
			if last != 0 {
				t.Errorf("a rejected recovery wrote %d log entries, want 0", last)
			}
			hs, err := store.LoadHardState(ctx)
			if err != nil {
				t.Fatalf("LoadHardState: %v", err)
			}
			if hs.CurrentTerm != 0 {
				t.Errorf("a rejected recovery advanced the term to %d, want 0", hs.CurrentTerm)
			}
		})
	}
}

// TestRecoverCluster_AllowsLearners checks that the nodes which are to rejoin
// can be named in the recovery membership as non-voters. They do not count
// towards the quorum the recovered node has to reach on its own, so naming them
// costs nothing, and it saves an AddMember call per node once the cluster is
// back.
func TestRecoverCluster_AllowsLearners(t *testing.T) {
	ctx := context.Background()
	store := memstore.New()
	members := []raft.PeerConfig{
		{ID: "n1", Voter: true},
		{ID: "n2", Voter: false},
		{ID: "n3", Voter: false},
	}
	if _, err := raft.RecoverCluster(ctx, store, "n1", members); err != nil {
		t.Fatalf("RecoverCluster: %v", err)
	}
	info, err := raft.InspectStorage(ctx, store)
	if err != nil {
		t.Fatalf("InspectStorage: %v", err)
	}
	if len(info.Members) != 3 {
		t.Fatalf("Members = %v, want all three named", info.Members)
	}
	for _, m := range info.Members {
		wantVoter := m.ID == "n1"
		if m.Voter != wantVoter {
			t.Errorf("member %s: Voter = %v, want %v", m.ID, m.Voter, wantVoter)
		}
	}
}

// TestRecoverCluster_WritesAboveEveryTermItHasSeen checks the term the recovery
// entry is written in. It has to be above both the hard state's term and the
// term of the last entry in the log, for two reasons: an entry from a term the
// node has not reached is the state a crashed write leaves behind and must not
// be created deliberately, and the recovered node has to out-vote any stale
// peer that is brought back by mistake.
func TestRecoverCluster_WritesAboveEveryTermItHasSeen(t *testing.T) {
	ctx := context.Background()
	store := memstore.New()

	const logTerm raft.Term = 7
	if err := store.SaveHardState(ctx, raft.HardState{CurrentTerm: 4, VotedFor: "n2"}); err != nil {
		t.Fatalf("SaveHardState: %v", err)
	}
	entries := make([]raft.LogEntry, 0, 3)
	for i := raft.Index(1); i <= 3; i++ {
		entries = append(entries, raft.LogEntry{Index: i, Term: logTerm, Command: fmt.Appendf(nil, "e%d", i)})
	}
	if err := store.AppendLogEntries(ctx, entries); err != nil {
		t.Fatalf("AppendLogEntries: %v", err)
	}

	if _, err := raft.RecoverCluster(ctx, store, "n1", []raft.PeerConfig{{ID: "n1", Voter: true}}); err != nil {
		t.Fatalf("RecoverCluster: %v", err)
	}

	hs, err := store.LoadHardState(ctx)
	if err != nil {
		t.Fatalf("LoadHardState: %v", err)
	}
	if hs.CurrentTerm <= logTerm {
		t.Errorf("hard state term = %d, want above the log's last term %d", hs.CurrentTerm, logTerm)
	}
	if hs.VotedFor != "" {
		t.Errorf("VotedFor = %q, want cleared: the new term has had no vote cast in it", hs.VotedFor)
	}

	last, err := store.LastIndex()
	if err != nil {
		t.Fatalf("LastIndex: %v", err)
	}
	if last != 4 {
		t.Fatalf("LastIndex = %d, want the recovery entry appended at 4", last)
	}
	entry, err := store.GetLogEntry(ctx, last)
	if err != nil {
		t.Fatalf("GetLogEntry(%d): %v", last, err)
	}
	if entry.Term != hs.CurrentTerm {
		t.Errorf("recovery entry term = %d, want %d: the log must never hold a term the node has not reached",
			entry.Term, hs.CurrentTerm)
	}

	// The entries that were already there are untouched.
	for i := raft.Index(1); i <= 3; i++ {
		got, err := store.GetLogEntry(ctx, i)
		if err != nil {
			t.Fatalf("GetLogEntry(%d): %v", i, err)
		}
		if got.Term != logTerm || string(got.Command) != fmt.Sprintf("e%d", i) {
			t.Errorf("entry %d = %+v, want the one recovery found there", i, got)
		}
	}
}
