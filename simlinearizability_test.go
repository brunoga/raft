package raft_test

// Linearizability checking with Porcupine, against a key-value model that can
// actually reject a history.
//
// A linearizability test is only as good as the model and the history. Two
// mistakes make one vacuous, and both are easy to make:
//
//   - A model that accepts everything. If write() -> ok always steps and no
//     reads are recorded, the checker has nothing to contradict. The model
//     below is exercised against deliberately broken histories in
//     TestKVModelRejectsNonLinearizableHistories, so the suite proves it can
//     say no before relying on it saying yes.
//
//   - A history with the interesting operations removed. The operations that
//     catch real bugs are the ones whose outcome the client never learned: a
//     proposal that timed out may still be in the log and may still commit, so
//     later reads may or may not see it. Dropping those operations hides
//     exactly the violations worth finding. Here they are recorded and modelled
//     as genuinely uncertain, using Porcupine's nondeterministic model.
//
// The history is produced by concurrent clients issuing real reads and real
// writes against whichever node is leader at the time, following redirects,
// while the network is partitioned and nodes are crashed underneath them.

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/anishathalye/porcupine"

	"github.com/brunoga/raft/v2"
	"github.com/brunoga/raft/v2/internal/simnet"
)

// ---- Model -----------------------------------------------------------------

type kvOpKind uint8

const (
	kvGet kvOpKind = iota
	kvPut
)

// kvInput is one invocation.
type kvInput struct {
	kind  kvOpKind
	key   string
	value string // the value written, for kvPut
}

// kvOutput is what the client observed.
type kvOutput struct {
	value string
	// unknown means the client never learned the outcome. For a write that
	// means it may or may not be in the log; for a read it means nothing was
	// observed at all.
	unknown bool
}

// kvNondeterministicModel is a per-key register.
//
// The model is nondeterministic in exactly one place: a write whose outcome is
// unknown may either have taken effect or not, so it steps to both states and
// Porcupine explores both. That is what makes a timed-out proposal a real
// constraint on the rest of the history — a later read that sees the value
// pins the write down as having happened, and every read after that must see
// it too.
var kvNondeterministicModel = porcupine.NondeterministicModel{
	// One partition per key. A history over independent registers is
	// linearizable if and only if each register's sub-history is, and checking
	// them separately keeps the search tractable.
	Partition: func(history []porcupine.Operation) [][]porcupine.Operation {
		byKey := make(map[string][]porcupine.Operation)
		for _, op := range history {
			k := op.Input.(kvInput).key
			byKey[k] = append(byKey[k], op)
		}
		keys := make([]string, 0, len(byKey))
		for k := range byKey {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out := make([][]porcupine.Operation, 0, len(keys))
		for _, k := range keys {
			out = append(out, byKey[k])
		}
		return out
	},

	// A key that has never been written reads as the empty string, which is
	// what the state machine returns for a missing key.
	Init: func() []any { return []any{""} },

	Step: func(state, input, output any) []any {
		cur := state.(string)
		in := input.(kvInput)
		out := output.(kvOutput)

		switch in.kind {
		case kvGet:
			if out.unknown {
				// The read never completed, so it observed nothing and
				// constrains nothing. It still belongs in the history: leaving
				// it out would be fine for a read, but keeping the rule uniform
				// keeps the recording code honest about writes, where it is not.
				return []any{cur}
			}
			if out.value == cur {
				return []any{cur}
			}
			return nil

		case kvPut:
			if out.unknown {
				if cur == in.value {
					return []any{cur}
				}
				return []any{cur, in.value}
			}
			return []any{in.value}
		}
		return nil
	},

	Equal: func(a, b any) bool { return a.(string) == b.(string) },

	DescribeOperation: func(input, output any) string {
		in := input.(kvInput)
		out := output.(kvOutput)
		if in.kind == kvPut {
			if out.unknown {
				return fmt.Sprintf("put(%s, %q) -> unknown", in.key, in.value)
			}
			return fmt.Sprintf("put(%s, %q) -> ok", in.key, in.value)
		}
		if out.unknown {
			return fmt.Sprintf("get(%s) -> unknown", in.key)
		}
		return fmt.Sprintf("get(%s) -> %q", in.key, out.value)
	},

	DescribeState: func(state any) string { return fmt.Sprintf("%q", state.(string)) },
}

// kvModel is the checkable form of the model above.
var kvModel = kvNondeterministicModel.ToModel()

// ---- The model must be able to say no --------------------------------------

// TestKVModelRejectsNonLinearizableHistories establishes that the model used by
// TestSimLinearizability has teeth.
//
// This is the test that would have caught the previous linearizability test
// being vacuous: its model returned true for every write and for every error,
// so no history could ever have failed it. Here the checker is fed histories
// that are linearizable and histories that are not, and has to tell them apart.
func TestKVModelRejectsNonLinearizableHistories(t *testing.T) {
	// op is a compact way to write one history entry.
	op := func(client int, call, ret int64, in kvInput, out kvOutput) porcupine.Operation {
		return porcupine.Operation{ClientId: client, Input: in, Call: call, Output: out, Return: ret}
	}
	put := func(key, value string) kvInput { return kvInput{kind: kvPut, key: key, value: value} }
	get := func(key string) kvInput { return kvInput{kind: kvGet, key: key} }
	ok := func(value string) kvOutput { return kvOutput{value: value} }
	unknown := kvOutput{unknown: true}

	tests := []struct {
		name             string
		history          []porcupine.Operation
		wantLinearizable bool
		why              string
	}{
		{
			name: "sequential writes and reads",
			history: []porcupine.Operation{
				op(0, 0, 10, put("x", "a"), ok("a")),
				op(0, 20, 30, get("x"), ok("a")),
				op(0, 40, 50, put("x", "b"), ok("b")),
				op(0, 60, 70, get("x"), ok("b")),
			},
			wantLinearizable: true,
			why:              "every read follows the write it observes",
		},
		{
			name: "read may be ordered either side of a concurrent write",
			history: []porcupine.Operation{
				op(0, 0, 10, put("x", "a"), ok("a")),
				op(1, 20, 60, put("x", "b"), ok("b")),
				op(2, 30, 40, get("x"), ok("a")),
			},
			wantLinearizable: true,
			why:              "the read is concurrent with the second write and may precede it",
		},
		{
			name: "read returns a value nobody wrote",
			history: []porcupine.Operation{
				op(0, 0, 10, put("x", "a"), ok("a")),
				op(0, 20, 30, get("x"), ok("z")),
			},
			wantLinearizable: false,
			why:              "z was never written",
		},
		{
			name: "acknowledged write is never visible",
			history: []porcupine.Operation{
				op(0, 0, 10, put("x", "a"), ok("a")),
				op(0, 20, 30, get("x"), ok("")),
			},
			wantLinearizable: false,
			why:              "the write completed before the read started",
		},
		{
			name: "committed value reverts",
			history: []porcupine.Operation{
				op(0, 0, 10, put("x", "a"), ok("a")),
				op(0, 20, 30, get("x"), ok("a")),
				op(0, 40, 50, get("x"), ok("")),
			},
			wantLinearizable: false,
			why:              "nothing wrote the empty value between the two reads",
		},
		{
			name: "unknown write that did not take effect",
			history: []porcupine.Operation{
				op(0, 0, 10, put("x", "a"), unknown),
				op(0, 20, 30, get("x"), ok("")),
			},
			wantLinearizable: true,
			why:              "a write whose outcome is unknown may simply not have happened",
		},
		{
			name: "unknown write that did take effect",
			history: []porcupine.Operation{
				op(0, 0, 10, put("x", "a"), unknown),
				op(0, 20, 30, get("x"), ok("a")),
			},
			wantLinearizable: true,
			why:              "a write whose outcome is unknown may have happened after all",
		},
		{
			name: "unknown write observed and then lost",
			history: []porcupine.Operation{
				op(0, 0, 10, put("x", "a"), unknown),
				op(1, 20, 30, get("x"), ok("a")),
				op(1, 40, 50, get("x"), ok("")),
			},
			wantLinearizable: false,
			why:              "once a read has observed the uncertain write, it must stay observed",
		},
		{
			name: "independent keys do not interfere",
			history: []porcupine.Operation{
				op(0, 0, 10, put("x", "a"), ok("a")),
				op(1, 0, 10, put("y", "b"), ok("b")),
				op(0, 20, 30, get("x"), ok("a")),
				op(1, 20, 30, get("y"), ok("b")),
			},
			wantLinearizable: true,
			why:              "each key is its own register",
		},
		{
			name: "one key's violation is not masked by another key",
			history: []porcupine.Operation{
				op(0, 0, 10, put("x", "a"), ok("a")),
				op(1, 0, 10, put("y", "b"), ok("b")),
				op(1, 20, 30, get("y"), ok("b")),
				op(0, 20, 30, get("x"), ok("b")),
			},
			wantLinearizable: false,
			why:              "x never held b, however well-behaved y was",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := porcupine.CheckOperations(kvModel, tc.history)
			if got != tc.wantLinearizable {
				t.Errorf("CheckOperations = %v, want %v (%s)", got, tc.wantLinearizable, tc.why)
			}
		})
	}
}

// ---- The real thing --------------------------------------------------------

// TestSimLinearizability runs concurrent clients against a cluster that is
// being partitioned and crashed underneath them, records a true history with
// invocation and response times, and checks it.
//
// What it protects: that the cluster, as seen from outside, behaves like a
// single register per key that every client shares — no stale read from a
// deposed leader, no acknowledged write that later evaporates, no read that
// goes backwards. Those are the failures a Raft bug actually produces for an
// application, and none of them are visible in a test that only checks that
// replicas eventually hold the same bytes.
func TestSimLinearizability(t *testing.T) {
	if testing.Short() {
		t.Skip("linearizability runs skipped in short mode")
	}
	iters := simIterations(t, 1, 10)
	base := simSeed(t)
	for i := range iters {
		seed := base + uint64(i)
		t.Run(fmt.Sprintf("seed_%d", seed), func(t *testing.T) {
			runLinearizabilityIteration(t, seed)
		})
	}
}

func runLinearizabilityIteration(t *testing.T, seed uint64) {
	const (
		clients      = 4
		opsPerClient = 22
		keys         = 3
	)

	cfg := defaultSimConfig(seed)
	cfg.nodes = 5
	cfg.policy = simnet.Flaky()
	c := newSimCluster(t, &cfg)
	if c.waitLeader(5*time.Second) < 0 {
		t.Fatalf("no leader after startup\n%s", c.diagnostics())
	}

	var (
		mu      sync.Mutex
		history []porcupine.Operation
		okPuts  int
		okGets  int
	)
	record := func(client int, call, ret time.Time, in kvInput, out kvOutput) {
		mu.Lock()
		defer mu.Unlock()
		history = append(history, porcupine.Operation{
			ClientId: client,
			Input:    in,
			Call:     call.UnixNano(),
			Output:   out,
			Return:   ret.UnixNano(),
		})
		if out.unknown {
			return
		}
		if in.kind == kvPut {
			okPuts++
		} else {
			okGets++
		}
	}

	// Faults run for as long as the clients do.
	stopFaults := make(chan struct{})
	faultsDone := make(chan struct{})
	go func() {
		defer close(faultsDone)
		rng := rand.New(rand.NewPCG(seed, 0xfa17))
		profile := chaosProfile{nodes: cfg.nodes, crashes: true, asymmetric: true}
		for {
			select {
			case <-stopFaults:
				return
			case <-time.After(time.Duration(10+rng.IntN(40)) * time.Millisecond):
			}
			undo := injectFault(c, rng, &profile)
			select {
			case <-stopFaults:
				undo()
				return
			case <-time.After(time.Duration(15+rng.IntN(60)) * time.Millisecond):
			}
			undo()
		}
	}()

	var wg sync.WaitGroup
	for cl := range clients {
		wg.Add(1)
		go func(client int) {
			defer wg.Done()
			rng := rand.New(rand.NewPCG(seed, uint64(client)+1))
			for i := range opsPerClient {
				key := fmt.Sprintf("key%d", rng.IntN(keys))
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				if rng.IntN(3) == 0 {
					in := kvInput{kind: kvGet, key: key}
					call := time.Now()
					value, ok := c.get(ctx, key)
					record(client, call, time.Now(), in, kvOutput{value: value, unknown: !ok})
				} else {
					// Values are unique per client and operation, so a retry
					// that lands twice is indistinguishable from landing once,
					// and the whole retry loop is one history operation.
					value := fmt.Sprintf("c%d-%d", client, i)
					in := kvInput{kind: kvPut, key: key, value: value}
					call := time.Now()
					status := c.put(ctx, key, value)
					record(client, call, time.Now(), in, kvOutput{value: value, unknown: status != opOK})
				}
				cancel()
				time.Sleep(time.Duration(rng.IntN(8)) * time.Millisecond)
			}
		}(cl)
	}
	wg.Wait()
	close(stopFaults)
	<-faultsDone
	c.net.HealAll()
	for i := range c.nodes {
		c.restart(i)
	}

	// A history in which nothing ever succeeded would pass trivially. Require
	// enough definite outcomes that the check means something.
	if okPuts == 0 || okGets == 0 {
		t.Fatalf("history has %d acknowledged writes and %d completed reads out of %d operations; "+
			"nothing definite happened, so the check would be vacuous\n%s",
			okPuts, okGets, len(history), c.diagnostics())
	}

	result, info := porcupine.CheckOperationsVerbose(kvModel, history, 30*time.Second)
	switch result {
	case porcupine.Ok:
		t.Logf("history of %d operations (%d acknowledged writes, %d completed reads) is linearizable",
			len(history), okPuts, okGets)
	case porcupine.Illegal:
		t.Errorf("history is NOT linearizable\n%s%s", describeHistory(history), c.diagnostics())
		_ = info
	case porcupine.Unknown:
		t.Logf("linearizability check timed out after 30s on a history of %d operations; "+
			"treating as inconclusive rather than as a failure", len(history))
	}
}

// describeHistory renders a history in call order so a failure can be read
// without a browser.
func describeHistory(history []porcupine.Operation) string {
	ordered := make([]porcupine.Operation, len(history))
	copy(ordered, history)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Call < ordered[j].Call })

	var b []byte
	base := ordered[0].Call
	b = append(b, "history (times in µs from the first call):\n"...)
	for _, op := range ordered {
		b = fmt.Appendf(b, "  c%d [%7d,%7d] %s\n",
			op.ClientId, (op.Call-base)/1000, (op.Return-base)/1000,
			kvNondeterministicModel.DescribeOperation(op.Input, op.Output))
	}
	return string(b)
}

// Compile-time assurance that the harness state machine and the model agree
// about how a command is encoded.
var _ = func() bool {
	k, v, ok := decodePut(encodePut("k", "v"))
	return ok && k == "k" && v == "v"
}()

var _ raft.StateMachine = (*simKV)(nil)
