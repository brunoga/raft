package simnet_test

// Tests for the simulator itself.
//
// A fault-injection harness that quietly fails to inject faults is worse than
// no harness at all: every test that depends on it reports a clean bill of
// health it never earned. These tests hold the simulator to the behaviour its
// documentation promises — that it really loses, duplicates, delays and
// reorders messages, that a one-way cut is one-way, and that the same seed
// replays the same decisions.

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/brunoga/raft"
	"github.com/brunoga/raft/internal/simnet"
	"github.com/brunoga/raft/storage/memstore"
)

// countingHandler records every RPC delivered to it.
type countingHandler struct {
	deliveries atomic.Int64

	mu     sync.Mutex
	orders []raft.Index // PrevLogIndex of each AppendEntries, in arrival order
}

func (h *countingHandler) HandleRequestVote(context.Context, *raft.RequestVoteRequest) (*raft.RequestVoteResponse, error) {
	h.deliveries.Add(1)
	return &raft.RequestVoteResponse{}, nil
}

func (h *countingHandler) HandleAppendEntries(_ context.Context, req *raft.AppendEntriesRequest) (*raft.AppendEntriesResponse, error) {
	h.deliveries.Add(1)
	h.mu.Lock()
	h.orders = append(h.orders, req.PrevLogIndex)
	h.mu.Unlock()
	return &raft.AppendEntriesResponse{Success: true}, nil
}

func (h *countingHandler) HandleInstallSnapshot(context.Context, *raft.InstallSnapshotRequest) (*raft.InstallSnapshotResponse, error) {
	h.deliveries.Add(1)
	return &raft.InstallSnapshotResponse{}, nil
}

func (h *countingHandler) HandleTimeoutNow(context.Context, *raft.TimeoutNowRequest) (*raft.TimeoutNowResponse, error) {
	h.deliveries.Add(1)
	return &raft.TimeoutNowResponse{}, nil
}

func (h *countingHandler) HandleReadIndex(context.Context, *raft.ReadIndexRequest) (*raft.ReadIndexResponse, error) {
	h.deliveries.Add(1)
	return &raft.ReadIndexResponse{}, nil
}

func (h *countingHandler) arrivalOrder() []raft.Index {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]raft.Index(nil), h.orders...)
}

// sendN sends n AppendEntries from a to b, tagging each with its sequence
// number in PrevLogIndex, and reports how many completed successfully.
func sendN(t *testing.T, tr raft.Transport, to raft.NodeID, n int) (ok int) {
	t.Helper()
	for i := range n {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, err := tr.AppendEntries(ctx, to, &raft.AppendEntriesRequest{PrevLogIndex: raft.Index(i)})
		cancel()
		if err == nil {
			ok++
		}
	}
	return ok
}

func TestNetwork_LosesMessages(t *testing.T) {
	net := simnet.New(1)
	defer func() { _ = net.Close() }()
	net.SetDefaultPolicy(simnet.LinkPolicy{Loss: 0.3})

	h := &countingHandler{}
	net.Register("b", h)
	tr := net.NewTransport("a")

	const n = 200
	ok := sendN(t, tr, "b", n)
	if ok == n {
		t.Errorf("all %d messages completed with a 30%% loss rate on both legs", n)
	}
	if ok == 0 {
		t.Errorf("no message completed with a 30%% loss rate; the simulator is dropping everything")
	}
	if int(h.deliveries.Load()) >= n {
		t.Errorf("handler saw %d deliveries out of %d sends; requests are not being lost",
			h.deliveries.Load(), n)
	}
}

func TestNetwork_DuplicatesMessages(t *testing.T) {
	net := simnet.New(2)
	net.SetDefaultPolicy(simnet.LinkPolicy{Duplicate: 0.5})

	h := &countingHandler{}
	net.Register("b", h)
	tr := net.NewTransport("a")

	const n = 100
	if ok := sendN(t, tr, "b", n); ok != n {
		t.Fatalf("%d of %d sends failed on a lossless link", n-ok, n)
	}
	// Close waits for the duplicate deliveries still in flight.
	_ = net.Close()

	if got := h.deliveries.Load(); got <= n {
		t.Errorf("handler saw %d deliveries for %d sends; a 50%% duplication rate should produce more", got, n)
	}
}

func TestNetwork_ReordersMessages(t *testing.T) {
	net := simnet.New(3)
	defer func() { _ = net.Close() }()
	// Every message has a coin-flip chance of being held back long enough for
	// the next one to overtake it.
	net.SetDefaultPolicy(simnet.LinkPolicy{Reorder: 0.5, ReorderDelay: 20 * time.Millisecond})

	h := &countingHandler{}
	net.Register("b", h)
	tr := net.NewTransport("a")

	// The sends have to be concurrent: a synchronous request/response transport
	// cannot reorder a caller's own sequential calls, only the traffic of
	// different callers, which is exactly what a real Raft leader produces from
	// its per-peer goroutines.
	var wg sync.WaitGroup
	for i := range 40 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, _ = tr.AppendEntries(ctx, "b", &raft.AppendEntriesRequest{PrevLogIndex: raft.Index(i)})
		}(i)
		time.Sleep(time.Millisecond)
	}
	wg.Wait()

	order := h.arrivalOrder()
	inversions := 0
	for i := 1; i < len(order); i++ {
		if order[i] < order[i-1] {
			inversions++
		}
	}
	if inversions == 0 {
		t.Errorf("messages arrived in send order every time; the simulator is not reordering (order: %v)", order)
	}
}

func TestNetwork_AsymmetricCutIsOneWay(t *testing.T) {
	net := simnet.New(4)
	defer func() { _ = net.Close() }()

	ha, hb := &countingHandler{}, &countingHandler{}
	net.Register("a", ha)
	net.Register("b", hb)
	ta, tb := net.NewTransport("a"), net.NewTransport("b")

	net.Cut("a", "b")

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if _, err := ta.AppendEntries(ctx, "b", &raft.AppendEntriesRequest{}); !errors.Is(err, simnet.ErrDropped) {
		t.Errorf("a→b after Cut(a, b): err = %v, want %v", err, simnet.ErrDropped)
	}
	if _, err := tb.AppendEntries(ctx, "a", &raft.AppendEntriesRequest{}); err != nil {
		t.Errorf("b→a after Cut(a, b): err = %v, want nil — the reverse direction must be unaffected", err)
	}

	net.Restore("a", "b")
	if _, err := ta.AppendEntries(ctx, "b", &raft.AppendEntriesRequest{}); err != nil {
		t.Errorf("a→b after Restore: err = %v, want nil", err)
	}
}

func TestNetwork_UnregisteredDestinationIsUnreachable(t *testing.T) {
	net := simnet.New(5)
	defer func() { _ = net.Close() }()
	tr := net.NewTransport("a")

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := tr.AppendEntries(ctx, "gone", &raft.AppendEntriesRequest{})
	if !errors.Is(err, simnet.ErrUnreachable) {
		t.Errorf("send to an unregistered node: err = %v, want %v", err, simnet.ErrUnreachable)
	}
}

// TestNetwork_SameSeedSameDecisions is the claim the whole harness rests on: a
// seed is worth printing because it replays the run.
//
// It is stated here over a sequential send pattern, where the order in which
// messages reach the simulator is fixed. Under a live cluster that order is
// decided by the Go scheduler and the guarantee weakens to "usually"; see the
// package documentation.
func TestNetwork_SameSeedSameDecisions(t *testing.T) {
	run := func(seed uint64) []bool {
		net := simnet.New(seed)
		defer func() { _ = net.Close() }()
		net.SetDefaultPolicy(simnet.LinkPolicy{Loss: 0.25, Duplicate: 0.25})
		net.Register("b", &countingHandler{})
		tr := net.NewTransport("a")

		outcomes := make([]bool, 0, 300)
		for i := range 300 {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			_, err := tr.AppendEntries(ctx, "b", &raft.AppendEntriesRequest{PrevLogIndex: raft.Index(i)})
			cancel()
			outcomes = append(outcomes, err == nil)
		}
		return outcomes
	}

	a, b := run(0xdecafbad), run(0xdecafbad)
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("two runs with seed 0xdecafbad diverged at message %d: %v vs %v", i, a[i], b[i])
		}
	}
	if c := run(0xfeedface); sameOutcomes(a, c) {
		t.Errorf("two different seeds produced identical outcomes for 300 messages; "+
			"the seed is not reaching the generator (%d messages)", len(a))
	}
}

func sameOutcomes(a, b []bool) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestNetwork_FilterSeesMessageContent covers the hook the scenario tests use
// to build states that random faults would almost never produce.
func TestNetwork_FilterSeesMessageContent(t *testing.T) {
	net := simnet.New(6)
	defer func() { _ = net.Close() }()

	h := &countingHandler{}
	net.Register("b", h)
	tr := net.NewTransport("a")

	net.SetFilter(func(m simnet.Message) bool {
		ae, ok := m.Req.(*raft.AppendEntriesRequest)
		if !ok {
			return true
		}
		for _, e := range ae.Entries {
			if e.Term == 7 {
				return false
			}
		}
		return true
	})

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if _, err := tr.AppendEntries(ctx, "b", &raft.AppendEntriesRequest{
		Entries: []raft.LogEntry{{Index: 1, Term: 6}},
	}); err != nil {
		t.Errorf("term-6 entry was dropped: %v", err)
	}
	if _, err := tr.AppendEntries(ctx, "b", &raft.AppendEntriesRequest{
		Entries: []raft.LogEntry{{Index: 2, Term: 7}},
	}); !errors.Is(err, simnet.ErrDropped) {
		t.Errorf("term-7 entry was delivered: err = %v, want %v", err, simnet.ErrDropped)
	}
	if got := h.deliveries.Load(); got != 1 {
		t.Errorf("handler saw %d deliveries, want 1", got)
	}
}

// ---- FaultStore ------------------------------------------------------------

func entries(from, to raft.Index, term raft.Term) []raft.LogEntry {
	out := make([]raft.LogEntry, 0, int(to-from)+1)
	for i := from; i <= to; i++ {
		out = append(out, raft.LogEntry{Index: i, Term: term, Command: []byte("cmd")})
	}
	return out
}

func TestFaultStore_CrashKeepsEverythingWriteThrough(t *testing.T) {
	ctx := context.Background()
	fs := simnet.NewFaultStore(memstore.New())

	if err := fs.AppendLogEntries(ctx, entries(1, 5, 1)); err != nil {
		t.Fatalf("AppendLogEntries: %v", err)
	}
	if err := fs.SaveHardState(ctx, raft.HardState{CurrentTerm: 3, VotedFor: "a"}); err != nil {
		t.Fatalf("SaveHardState: %v", err)
	}
	if err := fs.Crash(ctx); err != nil {
		t.Fatalf("Crash: %v", err)
	}

	last, _ := fs.LastIndex()
	if last != 5 {
		t.Errorf("last index after crash = %d, want 5: a write-through store must lose nothing", last)
	}
	hs, _ := fs.LoadHardState(ctx)
	if hs != (raft.HardState{CurrentTerm: 3, VotedFor: "a"}) {
		t.Errorf("hard state after crash = %+v, want term 3 voted for a", hs)
	}
}

func TestFaultStore_CrashLosesUnsyncedTail(t *testing.T) {
	ctx := context.Background()
	fs := simnet.NewFaultStore(memstore.New())

	if err := fs.AppendLogEntries(ctx, entries(1, 3, 1)); err != nil {
		t.Fatalf("durable append: %v", err)
	}
	if err := fs.SaveHardState(ctx, raft.HardState{CurrentTerm: 1, VotedFor: "a"}); err != nil {
		t.Fatalf("durable hard state: %v", err)
	}

	// From here on the store acknowledges writes it has not made durable.
	fs.SetBuffered(true)
	if err := fs.AppendLogEntries(ctx, entries(4, 8, 2)); err != nil {
		t.Fatalf("buffered append: %v", err)
	}
	if err := fs.SaveHardState(ctx, raft.HardState{CurrentTerm: 2, VotedFor: "b"}); err != nil {
		t.Fatalf("buffered hard state: %v", err)
	}
	if last, _ := fs.LastIndex(); last != 8 {
		t.Fatalf("last index before crash = %d, want 8: buffered writes must still be readable", last)
	}

	if err := fs.Crash(ctx); err != nil {
		t.Fatalf("Crash: %v", err)
	}
	if last, _ := fs.LastIndex(); last != 3 {
		t.Errorf("last index after crash = %d, want 3: the unsynced tail should be gone", last)
	}
	if hs, _ := fs.LoadHardState(ctx); hs != (raft.HardState{CurrentTerm: 1, VotedFor: "a"}) {
		t.Errorf("hard state after crash = %+v, want the last durable one (term 1, voted for a)", hs)
	}
}

func TestFaultStore_SyncMakesBufferedWritesDurable(t *testing.T) {
	ctx := context.Background()
	fs := simnet.NewFaultStore(memstore.New())
	fs.SetBuffered(true)

	if err := fs.AppendLogEntries(ctx, entries(1, 4, 1)); err != nil {
		t.Fatalf("AppendLogEntries: %v", err)
	}
	fs.Sync()
	if err := fs.AppendLogEntries(ctx, entries(5, 6, 1)); err != nil {
		t.Fatalf("AppendLogEntries: %v", err)
	}
	if err := fs.Crash(ctx); err != nil {
		t.Fatalf("Crash: %v", err)
	}
	if last, _ := fs.LastIndex(); last != 4 {
		t.Errorf("last index after crash = %d, want 4: Sync should have kept the first four", last)
	}
}

func TestFaultStore_FailWrites(t *testing.T) {
	ctx := context.Background()
	fs := simnet.NewFaultStore(memstore.New())
	if err := fs.AppendLogEntries(ctx, entries(1, 3, 1)); err != nil {
		t.Fatalf("setup append: %v", err)
	}

	fs.FailWrites()

	tests := []struct {
		name string
		call func() error
	}{
		{"SaveHardState", func() error { return fs.SaveHardState(ctx, raft.HardState{CurrentTerm: 9}) }},
		{"AppendLogEntries", func() error { return fs.AppendLogEntries(ctx, entries(4, 4, 2)) }},
		{"TruncateSuffix", func() error { return fs.TruncateSuffix(ctx, 2) }},
		{"TruncatePrefix", func() error { return fs.TruncatePrefix(ctx, 2) }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(); !errors.Is(err, simnet.ErrDiskFailure) {
				t.Errorf("%s while writes are failing: err = %v, want %v", tc.name, err, simnet.ErrDiskFailure)
			}
		})
	}

	// Reads keep working: a full disk does not stop you reading what is on it.
	if _, err := fs.GetLogEntry(ctx, 2); err != nil {
		t.Errorf("GetLogEntry while writes are failing: %v", err)
	}
	if last, _ := fs.LastIndex(); last != 3 {
		t.Errorf("last index = %d, want 3: a failed write must not have taken effect", last)
	}

	fs.AllowWrites()
	if err := fs.AppendLogEntries(ctx, entries(4, 4, 2)); err != nil {
		t.Errorf("AppendLogEntries after AllowWrites: %v", err)
	}
	if writes, failures := fs.Stats(); failures == 0 || writes <= failures {
		t.Errorf("Stats() = (%d writes, %d failures), want both non-zero with writes > failures", writes, failures)
	}
}

func TestSkewClock(t *testing.T) {
	slow := simnet.NewSkewClock(0.0, 0)
	start := slow.Now()
	time.Sleep(20 * time.Millisecond)
	if drift := slow.Now().Sub(start); drift > time.Millisecond {
		t.Errorf("a stopped clock advanced by %s", drift)
	}

	slow.Advance(time.Hour)
	if drift := slow.Now().Sub(start); drift < time.Hour {
		t.Errorf("Advance(1h) moved the clock by %s, want at least an hour", drift)
	}
}

// TestNetwork_PerLinkPolicy covers configuring one link differently from the
// rest, which is how a test models a single slow or lossy replica rather than a
// uniformly bad network.
func TestNetwork_PerLinkPolicy(t *testing.T) {
	net := simnet.New(7)
	defer func() { _ = net.Close() }()

	net.SetDefaultPolicy(simnet.LinkPolicy{})
	net.SetLinkPolicy("a", "bad", simnet.LinkPolicy{Loss: 1.0})

	net.Register("good", &countingHandler{})
	net.Register("bad", &countingHandler{})
	tr := net.NewTransport("a")

	if ok := sendN(t, tr, "good", 20); ok != 20 {
		t.Errorf("%d of 20 messages failed on the default (perfect) link", 20-ok)
	}
	if ok := sendN(t, tr, "bad", 20); ok != 0 {
		t.Errorf("%d of 20 messages completed on a link configured to lose everything", ok)
	}
}
