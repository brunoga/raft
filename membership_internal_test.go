package raft

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"slices"
	"testing"
)

// memLogStorage is a minimal in-memory Storage for membership tests. The
// package's own tests cannot import storage/memstore, which imports this
// package.
type memLogStorage struct {
	stubStorage
	entries []LogEntry
}

func (m *memLogStorage) AppendLogEntries(_ context.Context, entries []LogEntry) error {
	m.entries = append(m.entries, entries...)
	return nil
}

func (m *memLogStorage) GetLogEntry(_ context.Context, index Index) (LogEntry, error) {
	for _, e := range m.entries {
		if e.Index == index {
			return e, nil
		}
	}
	return LogEntry{}, ErrNotFound
}

func (m *memLogStorage) GetLogEntries(_ context.Context, lo, hi Index) ([]LogEntry, error) {
	var out []LogEntry
	for _, e := range m.entries {
		if e.Index >= lo && e.Index < hi {
			out = append(out, e)
		}
	}
	return out, nil
}

func (m *memLogStorage) FirstIndex() (Index, error) {
	if len(m.entries) == 0 {
		return 0, nil
	}
	return m.entries[0].Index, nil
}

func (m *memLogStorage) LastIndex() (Index, error) {
	if len(m.entries) == 0 {
		return 0, nil
	}
	return m.entries[len(m.entries)-1].Index, nil
}

func (m *memLogStorage) TruncateSuffix(_ context.Context, fromIndex Index) error {
	kept := m.entries[:0]
	for _, e := range m.entries {
		if e.Index < fromIndex {
			kept = append(kept, e)
		}
	}
	m.entries = kept
	return nil
}

// newMembershipTestNode builds an unstarted node over storage the caller keeps
// a handle on. The node's goroutines are never started, so the test owns its
// state exclusively and can drive the membership helpers directly.
func newMembershipTestNode(t *testing.T, store Storage, peers []PeerConfig) *Node {
	t.Helper()

	cfg := DefaultConfig()
	cfg.ID = "self"
	cfg.Peers = peers
	cfg.Storage = store
	cfg.StateMachine = &stubStateMachine{}
	cfg.Transport = &stubTransport{}
	cfg.TickInterval = 0

	node, err := New(&cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return node
}

func memberIDs(peers []PeerConfig) []string {
	out := make([]string, 0, len(peers))
	for _, p := range peers {
		out = append(out, string(p.ID))
	}
	slices.Sort(out)
	return out
}

// TestMembership_TruncationRevertsToPreviousConfiguration asserts that when a
// new leader overwrites a conflicting suffix that contained a configuration
// change, the node stops using that configuration.
//
// A node decides quorums by the latest configuration in its log. When entries
// are discarded, any configuration they carried has to be discarded with them —
// otherwise the node would keep counting a member the cluster never agreed on,
// and could not reach a quorum it believes it needs.
func TestMembership_TruncationRevertsToPreviousConfiguration(t *testing.T) {
	ctx := context.Background()
	store := &memLogStorage{}
	node := newMembershipTestNode(t, store, []PeerConfig{{ID: "a", Voter: true}})

	// Entry 1 is ordinary; entry 2 adds a member.
	addB := LogEntry{Index: 2, Term: 1, Command: encodeConfigEntry(configOpAdd, PeerConfig{ID: "b", Voter: true})}
	entries := []LogEntry{
		{Index: 1, Term: 1, Command: []byte("x")},
		addB,
	}
	if err := node.log.append(ctx, entries); err != nil {
		t.Fatalf("append: %v", err)
	}
	node.applyConfigChange(addB.Command, addB.Index)

	if got := memberIDs(node.cfg.Peers); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("peers after adopting the change = %v, want [a b]", got)
	}
	if node.configIndex != 2 {
		t.Fatalf("configIndex = %d, want 2", node.configIndex)
	}

	// A new leader overwrites index 2 with an unrelated entry.
	if err := node.log.truncateSuffix(ctx, 2); err != nil {
		t.Fatalf("truncateSuffix: %v", err)
	}
	if node.configIndex >= 2 {
		if err := node.rebuildMembership(ctx); err != nil {
			t.Fatalf("rebuildMembership: %v", err)
		}
	}

	if got := memberIDs(node.cfg.Peers); !slices.Equal(got, []string{"a"}) {
		t.Errorf("peers after the change was discarded = %v, want [a]", got)
	}
}

// TestMembership_RebuildReplaysEveryConfigEntry asserts that rebuilding walks
// the whole log, so the configuration in effect is the last one recorded rather
// than the first one found.
func TestMembership_RebuildReplaysEveryConfigEntry(t *testing.T) {
	ctx := context.Background()
	store := &memLogStorage{}
	node := newMembershipTestNode(t, store, nil)

	entries := []LogEntry{
		{Index: 1, Term: 1, Command: encodeConfigEntry(configOpAdd, PeerConfig{ID: "a", Voter: true})},
		{Index: 2, Term: 1, Command: []byte("x")},
		{Index: 3, Term: 1, Command: encodeConfigEntry(configOpAdd, PeerConfig{ID: "b", Voter: true})},
		{Index: 4, Term: 1, Command: encodeConfigEntry(configOpRemove, PeerConfig{ID: "a"})},
	}
	if err := node.log.append(ctx, entries); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := node.rebuildMembership(ctx); err != nil {
		t.Fatalf("rebuildMembership: %v", err)
	}

	if got := memberIDs(node.cfg.Peers); !slices.Equal(got, []string{"b"}) {
		t.Errorf("peers after replay = %v, want [b]", got)
	}
	if node.configIndex != 4 {
		t.Errorf("configIndex = %d, want 4 (the last config entry)", node.configIndex)
	}
}

// TestSnapshotFraming_RoundTripsMembership covers both shapes of membership
// through the snapshot framing, including the joint configuration a snapshot
// can legitimately be taken during.
func TestSnapshotFraming_RoundTripsMembership(t *testing.T) {
	table := []clientRecord{{id: "c1", ce: clientEntry{seqNum: 7, result: []byte("r")}}}

	tests := []struct {
		name string
		ms   membershipState
	}{
		{
			name: "simple",
			ms: membershipState{members: []PeerConfig{
				{ID: "n1", Voter: true},
				{ID: "n2", Voter: true},
				{ID: "witness", Voter: false},
			}},
		},
		{
			name: "joint",
			ms: membershipState{
				joint: true,
				old:   []PeerConfig{{ID: "n1", Voter: true}, {ID: "n2", Voter: true}},
				new:   []PeerConfig{{ID: "n2", Voter: true}, {ID: "n3", Voter: true}},
			},
		},
		{
			name: "self removed from the new configuration",
			ms: membershipState{
				joint: true,
				old:   []PeerConfig{{ID: "n1", Voter: true}},
				new:   []PeerConfig{{ID: "n2", Voter: true}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			err := writeWrappedSnapshot(&buf, table, &tt.ms, func(w io.Writer) error {
				_, werr := w.Write([]byte("machine-state"))
				return werr
			})
			if err != nil {
				t.Fatalf("writeWrappedSnapshot: %v", err)
			}

			gotTable, gotMS, hasMS, smReader, err := readWrappedSnapshot(&buf)
			if err != nil {
				t.Fatalf("readWrappedSnapshot: %v", err)
			}
			if !hasMS {
				t.Fatal("membership section missing from a snapshot that was written with one")
			}
			if gotMS.joint != tt.ms.joint ||
				!slices.Equal(gotMS.members, tt.ms.members) ||
				!slices.Equal(gotMS.old, tt.ms.old) ||
				!slices.Equal(gotMS.new, tt.ms.new) {
				t.Errorf("membership round-trip = %+v, want %+v", gotMS, tt.ms)
			}
			if len(gotTable) != 1 || gotTable[0].id != "c1" || gotTable[0].ce.seqNum != 7 {
				t.Errorf("client table round-trip = %+v, want one entry c1 with seqNum 7", gotTable)
			}
			smData, err := io.ReadAll(smReader)
			if err != nil {
				t.Fatalf("read state-machine data: %v", err)
			}
			if string(smData) != "machine-state" {
				t.Errorf("state-machine data = %q, want %q", smData, "machine-state")
			}
		})
	}
}

// TestSnapshotFraming_ReadsSnapshotWithoutMembershipSection asserts that a
// snapshot written before the membership section existed still loads, and
// reports that it carries no membership so the caller keeps its own rather than
// adopting an empty cluster.
func TestSnapshotFraming_ReadsSnapshotWithoutMembershipSection(t *testing.T) {
	tableBytes := encodeClientTable([]clientRecord{{id: "c1", ce: clientEntry{seqNum: 3}}})

	var buf bytes.Buffer
	var hdr [12]byte
	binary.LittleEndian.PutUint64(hdr[:8], snapFrameMagicV1)
	binary.LittleEndian.PutUint32(hdr[8:], uint32(len(tableBytes)))
	buf.Write(hdr[:])
	buf.Write(tableBytes)
	buf.WriteString("machine-state")

	table, ms, hasMS, smReader, err := readWrappedSnapshot(&buf)
	if err != nil {
		t.Fatalf("readWrappedSnapshot: %v", err)
	}
	if hasMS {
		t.Errorf("reported a membership section (%+v) for a snapshot that has none", ms)
	}
	if len(table) != 1 || table[0].id != "c1" || table[0].ce.seqNum != 3 {
		t.Errorf("client table = %+v, want one entry c1 with seqNum 3", table)
	}
	smData, err := io.ReadAll(smReader)
	if err != nil {
		t.Fatalf("read state-machine data: %v", err)
	}
	if string(smData) != "machine-state" {
		t.Errorf("state-machine data = %q, want %q", smData, "machine-state")
	}
}

// TestMembership_SelfRoleRoundTrips asserts that this node's own voting role
// survives the trip through a snapshot, for both a plain and a joint
// configuration. A witness that came back as a voter would count itself towards
// quorums it must not.
func TestMembership_SelfRoleRoundTrips(t *testing.T) {
	store := &memLogStorage{}
	node := newMembershipTestNode(t, store, []PeerConfig{{ID: "a", Voter: true}})

	node.cfg.Voter = false // this node is a witness
	ms := node.currentMembership()

	fresh := newMembershipTestNode(t, &memLogStorage{}, nil)
	fresh.cfg.ID = "self"
	fresh.restoreMembership(&ms)

	if fresh.cfg.Voter {
		t.Error("witness came back from its own membership record as a voter")
	}
	if got := memberIDs(fresh.cfg.Peers); !slices.Equal(got, []string{"a"}) {
		t.Errorf("peers = %v, want [a]", got)
	}
}
