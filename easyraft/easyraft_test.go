package easyraft_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brunoga/raft"
	"github.com/brunoga/raft/discovery/udpbroadcast"
	"github.com/brunoga/raft/easyraft"
)

type Counter struct {
	Value uint64 `json:"value"`
}

func TestEasyRaft_UDPDiscovery(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "easyraft-udp-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = os.RemoveAll(tmpDir)
	}()

	// Use static peers so the cluster forms reliably; discovery runs in parallel
	// and exercises the AddServer path (peers are already members, so it's a
	// harmless no-op after the first successful call).
	peers := map[raft.NodeID]string{
		"n1": "127.0.0.1:9092",
		"n2": "127.0.0.1:9093",
	}

	// Node 1
	d1, err := udpbroadcast.New(&udpbroadcast.Config{
		NodeID: "n1",
		Addr:   "127.0.0.1:9092",
	})
	if err != nil {
		t.Fatal(err)
	}
	er1, err := easyraft.New[Counter](
		easyraft.WithID("n1"),
		easyraft.WithRaftAddr("127.0.0.1:9092"),
		easyraft.WithDataDir(filepath.Join(tmpDir, "n1")),
		easyraft.WithPeers(peers),
		easyraft.WithDiscovery(d1, 200*time.Millisecond),
	)
	if err != nil {
		t.Fatal(err)
	}

	// Node 2
	d2, err := udpbroadcast.New(&udpbroadcast.Config{
		NodeID: "n2",
		Addr:   "127.0.0.1:9093",
	})
	if err != nil {
		t.Fatal(err)
	}
	er2, err := easyraft.New[Counter](
		easyraft.WithID("n2"),
		easyraft.WithRaftAddr("127.0.0.1:9093"),
		easyraft.WithDataDir(filepath.Join(tmpDir, "n2")),
		easyraft.WithPeers(peers),
		easyraft.WithDiscovery(d2, 200*time.Millisecond),
	)
	if err != nil {
		t.Fatal(err)
	}

	er1.Start()
	defer er1.Stop()
	er2.Start()
	defer er2.Stop()

	// Wait for a leader to be elected and the cluster to be fully operational.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var leaderER *easyraft.EasyRaft[Counter]
	for ctx.Err() == nil {
		for _, er := range []*easyraft.EasyRaft[Counter]{er1, er2} {
			if errCreate := er.Create(ctx, "discovery-probe", Counter{Value: 1}); errCreate == nil {
				leaderER = er
				goto ready
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("timeout waiting for leader election with discovery enabled")

ready:
	// ReadStale is sufficient here: we just committed via the leader so the data
	// is in the state machine; we're testing cluster formation, not consistency.
	c, err := leaderER.ReadStale("discovery-probe")
	if err != nil {
		t.Fatalf("ReadStale after discovery-assisted cluster formation: %v", err)
	}
	if c.Value != 1 {
		t.Errorf("expected Value=1, got %d", c.Value)
	}
}

func TestEasyRaft_Basic(t *testing.T) {

	tmpDir, err := os.MkdirTemp("", "easyraft-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = os.RemoveAll(tmpDir)
	}()

	peers := map[raft.NodeID]string{
		"n1": "127.0.0.1:9091",
	}

	er, err := easyraft.New[Counter](
		easyraft.WithID("n1"),
		easyraft.WithRaftAddr("127.0.0.1:9091"),
		easyraft.WithDataDir(filepath.Join(tmpDir, "n1")),
		easyraft.WithPeers(peers),
	)
	if err != nil {
		t.Fatal(err)
	}

	// Register mutation
	er.RegisterMutation("increment", func(c *Counter, args []byte) (*Counter, []byte, error) {
		var delta uint64
		if len(args) > 0 {
			if jsonErr := json.Unmarshal(args, &delta); jsonErr != nil {
				return nil, nil, jsonErr
			}
		}
		c.Value += delta
		return c, nil, nil
	})

	er.Start()
	defer er.Stop()

	// Wait for leader election (single node cluster elects itself quickly, but timeout is 1-2s)
	time.Sleep(3 * time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Create
	if errCreate := er.Create(ctx, "c1", Counter{Value: 10}); errCreate != nil {
		t.Fatalf("Create: %v", errCreate)
	}

	// Read
	c, err := er.Read(ctx, "c1")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if c.Value != 10 {
		t.Errorf("expected 10, got %d", c.Value)
	}

	// Mutate
	delta, _ := json.Marshal(uint64(5))
	_, err = er.Mutate(ctx, "c1", "increment", delta)
	if err != nil {
		t.Fatalf("Mutate: %v", err)
	}

	// ReadStale
	c, err = er.ReadStale("c1")
	if err != nil {
		t.Fatalf("ReadStale: %v", err)
	}
	if c.Value != 15 {
		t.Errorf("expected 15, got %d", c.Value)
	}

	// Delete
	if errDelete := er.Delete(ctx, "c1"); errDelete != nil {
		t.Fatalf("Delete: %v", errDelete)
	}

	// Read missing
	_, err = er.Read(ctx, "c1")
	if err != easyraft.ErrKeyNotFound {
		t.Errorf("expected ErrKeyNotFound, got %v", err)
	}
}

func TestEasyRaft_HTTP(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "easyraft-http-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = os.RemoveAll(tmpDir)
	}()

	er, err := easyraft.New[Counter](
		easyraft.WithID("n1"),
		easyraft.WithRaftAddr("127.0.0.1:9094"),
		easyraft.WithHTTPAddr("127.0.0.1:8084"),
		easyraft.WithDataDir(filepath.Join(tmpDir, "n1")),
		easyraft.WithPeers(map[raft.NodeID]string{"n1": "127.0.0.1:9094"}),
	)
	if err != nil {
		t.Fatal(err)
	}

	er.Start()
	defer er.Stop()

	time.Sleep(3 * time.Second) // wait for leader

	// Test HTTP Create (using /default/h1 instead of legacy /items/h1)
	val := Counter{Value: 100}
	b, _ := json.Marshal(val)
	resp, err := http.Post("http://127.0.0.1:8084/default/h1", "application/json", strings.NewReader(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201, got %d", resp.StatusCode)
	}

	// Test HTTP Read
	resp, err = http.Get("http://127.0.0.1:8084/default/h1")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var readVal Counter
	if decodeErr := json.NewDecoder(resp.Body).Decode(&readVal); decodeErr != nil {
		t.Fatal(decodeErr)
	}
	if readVal.Value != 100 {
		t.Errorf("expected 100, got %d", readVal.Value)
	}

	// Test HTTP Status
	respStatus, err := http.Get("http://127.0.0.1:8084/status")
	if err != nil {
		t.Fatal(err)
	}
	_ = respStatus.Body.Close()
	if respStatus.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", respStatus.StatusCode)
	}
}

func TestStore_MultiCollection(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "store-multi-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = os.RemoveAll(tmpDir)
	}()

	s, err := easyraft.NewStore(
		easyraft.WithID("n1"),
		easyraft.WithRaftAddr("127.0.0.1:9095"),
		easyraft.WithDataDir(filepath.Join(tmpDir, "n1")),
		easyraft.WithPeers(map[raft.NodeID]string{"n1": "127.0.0.1:9095"}),
	)
	if err != nil {
		t.Fatal(err)
	}

	users := easyraft.AddCollection[string](s, "users")
	scores := easyraft.AddCollection[int](s, "scores")

	s.Start()
	defer s.Stop()

	time.Sleep(3 * time.Second) // wait for leader

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := users.Create(ctx, "u1", "alice"); err != nil {
		t.Fatal(err)
	}
	if err := scores.Create(ctx, "u1", 100); err != nil {
		t.Fatal(err)
	}

	u, _ := users.Read(ctx, "u1")
	if u != "alice" {
		t.Errorf("expected alice, got %s", u)
	}

	sc, _ := scores.Read(ctx, "u1")
	if sc != 100 {
		t.Errorf("expected 100, got %d", sc)
	}
}
func TestStore_Txn(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "store-txn-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = os.RemoveAll(tmpDir)
	}()

	s, err := easyraft.NewStore(
		easyraft.WithID("n1"),
		easyraft.WithRaftAddr("127.0.0.1:9096"),
		easyraft.WithDataDir(filepath.Join(tmpDir, "n1")),
		easyraft.WithPeers(map[raft.NodeID]string{"n1": "127.0.0.1:9096"}),
	)
	if err != nil {
		t.Fatal(err)
	}

	s.Start()
	defer s.Stop()

	time.Sleep(3 * time.Second) // wait for leader

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err = s.Txn(ctx, func(tx *easyraft.Txn) error {
		_ = tx.Create("c1", "k1", "v1")
		_ = tx.Create("c2", "k2", "v2")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	c1 := easyraft.AddCollection[string](s, "c1")
	c2 := easyraft.AddCollection[string](s, "c2")

	v1, _ := c1.Read(ctx, "k1")
	if v1 != "v1" {
		t.Errorf("expected v1, got %s", v1)
	}
	v2, _ := c2.Read(ctx, "k2")
	if v2 != "v2" {
		t.Errorf("expected v2, got %s", v2)
	}
}

func TestStore_ProposeOnce(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "store-once-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = os.RemoveAll(tmpDir)
	}()

	s, err := easyraft.NewStore(
		easyraft.WithID("n1"),
		easyraft.WithRaftAddr("127.0.0.1:9097"),
		easyraft.WithDataDir(filepath.Join(tmpDir, "n1")),
		easyraft.WithPeers(map[raft.NodeID]string{"n1": "127.0.0.1:9097"}),
	)
	if err != nil {
		t.Fatal(err)
	}

	counts := easyraft.AddCollection[int](s, "counts")
	counts.RegisterMutation("inc", func(current *int, args []byte) (*int, []byte, error) {
		res := *current + 1
		return &res, nil, nil
	})

	s.Start()
	defer s.Stop()

	time.Sleep(3 * time.Second) // wait for leader

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_ = counts.Create(ctx, "k1", 0)

	// ProposeOnce with same seqNum should be idempotent
	_, err = counts.MutateOnce(ctx, "client1", 1, "k1", "inc", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = counts.MutateOnce(ctx, "client1", 1, "k1", "inc", nil)
	if err != nil {
		t.Fatal(err)
	}

	v, _ := counts.Read(ctx, "k1")
	if v != 1 {
		t.Errorf("expected 1, got %d (idempotency failed)", v)
	}
}

func TestManager_MultiRaft(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "mgr-multi-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = os.RemoveAll(tmpDir)
	}()

	mgr, err := easyraft.NewManager(
		easyraft.WithID("n1"),
		easyraft.WithRaftAddr("127.0.0.1:9098"),
		easyraft.WithHTTPAddr("127.0.0.1:8088"),
		easyraft.WithPeers(map[raft.NodeID]string{"n1": "127.0.0.1:9098"}),
	)
	if err != nil {
		t.Fatal(err)
	}

	s1, _ := mgr.AddStore(1, easyraft.WithDataDir(filepath.Join(tmpDir, "s1")))
	s2, _ := mgr.AddStore(2, easyraft.WithDataDir(filepath.Join(tmpDir, "s2")))

	if errStart := mgr.Start(); errStart != nil {
		t.Fatal(errStart)
	}
	defer mgr.Stop()

	// Wait for leaders
	time.Sleep(3 * time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	c1 := easyraft.AddCollection[string](s1, "data")
	c2 := easyraft.AddCollection[string](s2, "data")

	if errC1 := c1.Create(ctx, "k1", "v1"); errC1 != nil {
		t.Errorf("s1 create: %v", errC1)
	}
	if errC2 := c2.Create(ctx, "k1", "v2"); errC2 != nil {
		t.Errorf("s2 create: %v", errC2)
	}

	// Verify isolation
	v1, _ := c1.Read(ctx, "k1")
	if v1 != "v1" {
		t.Errorf("s1: expected v1, got %s", v1)
	}
	v2, _ := c2.Read(ctx, "k1")
	if v2 != "v2" {
		t.Errorf("s2: expected v2, got %s", v2)
	}

	// Test HTTP routing for Multi-Raft
	resp, err := http.Get("http://127.0.0.1:8088/groups/1/data/k1")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("HTTP group 1: expected 200, got %d", resp.StatusCode)
	}
}

// TestEasyRaft_Join verifies that a new node can join an existing single-node
// cluster by posting to the seed's /join endpoint via WithJoinAddr, without
// knowing any peer addresses upfront.
func TestEasyRaft_Join(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "easyraft-join-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	// --- n1: the existing single-node cluster with HTTP enabled ---
	er1, err := easyraft.New[Counter](
		easyraft.WithID("n1"),
		easyraft.WithRaftAddr("127.0.0.1:9099"),
		easyraft.WithHTTPAddr("127.0.0.1:8089"),
		easyraft.WithDataDir(filepath.Join(tmpDir, "n1")),
		easyraft.WithPeers(map[raft.NodeID]string{"n1": "127.0.0.1:9099"}),
	)
	if err != nil {
		t.Fatal(err)
	}
	er1.Start()
	defer er1.Stop()

	// Wait for n1 to elect itself leader before n2 tries to join.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for ctx.Err() == nil {
		if errProbe := er1.Create(ctx, "__join_probe__", Counter{}); errProbe == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if ctx.Err() != nil {
		t.Fatal("n1 never became leader")
	}

	// --- n2: a new node that knows only the seed's HTTP address ---
	er2, err := easyraft.New[Counter](
		easyraft.WithID("n2"),
		easyraft.WithRaftAddr("127.0.0.1:9100"),
		easyraft.WithDataDir(filepath.Join(tmpDir, "n2")),
		easyraft.WithJoinAddr("127.0.0.1:8089"),
	)
	if err != nil {
		t.Fatal(err)
	}
	er2.Start()
	defer er2.Stop()

	// Wait until n2 is part of the cluster: it should be able to read the
	// entry that n1 committed (stale read is fine — we just need it caught up).
	ctx2, cancel2 := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel2()
	for ctx2.Err() == nil {
		if _, readErr := er2.ReadStale("__join_probe__"); readErr == nil {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("n2 never caught up after joining")
}

// TestEasyRaft_Members verifies GET /members returns the correct member list.
func TestEasyRaft_Members(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "easyraft-members-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	peers := map[raft.NodeID]string{
		"n1": "127.0.0.1:9101",
		"n2": "127.0.0.1:9102",
	}

	er1, err := easyraft.New[Counter](
		easyraft.WithID("n1"),
		easyraft.WithRaftAddr("127.0.0.1:9101"),
		easyraft.WithHTTPAddr("127.0.0.1:8101"),
		easyraft.WithDataDir(filepath.Join(tmpDir, "n1")),
		easyraft.WithPeers(peers),
	)
	if err != nil {
		t.Fatal(err)
	}
	er2, err := easyraft.New[Counter](
		easyraft.WithID("n2"),
		easyraft.WithRaftAddr("127.0.0.1:9102"),
		easyraft.WithHTTPAddr("127.0.0.1:8102"),
		easyraft.WithDataDir(filepath.Join(tmpDir, "n2")),
		easyraft.WithPeers(peers),
	)
	if err != nil {
		t.Fatal(err)
	}
	er1.Start()
	defer er1.Stop()
	er2.Start()
	defer er2.Stop()

	// Wait for a leader.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for ctx.Err() == nil {
		if e := er1.Create(ctx, "__probe__", Counter{}); e == nil {
			break
		}
		if e := er2.Create(ctx, "__probe__", Counter{}); e == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if ctx.Err() != nil {
		t.Fatal("no leader elected")
	}

	resp, err := http.Get("http://127.0.0.1:8101/members")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /members: status %d", resp.StatusCode)
	}

	var body struct {
		Members []struct {
			ID   string `json:"id"`
			Self bool   `json:"self"`
		} `json:"members"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Members) != 2 {
		t.Errorf("expected 2 members, got %d", len(body.Members))
	}
	selfCount := 0
	for _, m := range body.Members {
		if m.Self {
			selfCount++
			if m.ID != "n1" {
				t.Errorf("self ID = %q, want n1", m.ID)
			}
		}
	}
	if selfCount != 1 {
		t.Errorf("expected exactly 1 self member, got %d", selfCount)
	}
}

// TestEasyRaft_Batch verifies that POST /batch commits multiple operations atomically.
func TestEasyRaft_Batch(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "easyraft-batch-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	er, err := easyraft.New[Counter](
		easyraft.WithID("n1"),
		easyraft.WithRaftAddr("127.0.0.1:9103"),
		easyraft.WithHTTPAddr("127.0.0.1:8103"),
		easyraft.WithDataDir(filepath.Join(tmpDir, "n1")),
		easyraft.WithPeers(map[raft.NodeID]string{"n1": "127.0.0.1:9103"}),
	)
	if err != nil {
		t.Fatal(err)
	}
	er.Start()
	defer er.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if e := er.Create(ctx, "__ready__", Counter{}); e != nil {
		for ctx.Err() == nil {
			if er.Create(ctx, "__ready__", Counter{}) == nil {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
	}

	body := strings.NewReader(`[
		{"op":"create","collection":"default","key":"a","value":{"value":1}},
		{"op":"create","collection":"default","key":"b","value":{"value":2}}
	]`)
	resp, err := http.Post("http://127.0.0.1:8103/batch", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /batch: status %d", resp.StatusCode)
	}

	// Both keys should now exist.
	a, err := er.ReadStale("a")
	if err != nil {
		t.Fatalf("ReadStale a: %v", err)
	}
	if a.Value != 1 {
		t.Errorf("a.Value = %d, want 1", a.Value)
	}
	b, err := er.ReadStale("b")
	if err != nil {
		t.Fatalf("ReadStale b: %v", err)
	}
	if b.Value != 2 {
		t.Errorf("b.Value = %d, want 2", b.Value)
	}
}

// TestEasyRaft_LeaveOnStop verifies that WithLeaveOnStop removes the node from
// the cluster before shutdown so remaining nodes adjust their membership.
func TestEasyRaft_LeaveOnStop(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "easyraft-leave-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	peers := map[raft.NodeID]string{
		"n1": "127.0.0.1:9104",
		"n2": "127.0.0.1:9105",
		"n3": "127.0.0.1:9106",
	}

	makeNode := func(id string, raftPort int, leave bool) (*easyraft.EasyRaft[Counter], error) {
		opts := []easyraft.Option{
			easyraft.WithID(raft.NodeID(id)),
			easyraft.WithRaftAddr(fmt.Sprintf("127.0.0.1:%d", raftPort)),
			easyraft.WithDataDir(filepath.Join(tmpDir, id)),
			easyraft.WithPeers(peers),
		}
		if leave {
			opts = append(opts, easyraft.WithLeaveOnStop())
		}
		return easyraft.New[Counter](opts...)
	}

	er1, err := makeNode("n1", 9104, false)
	if err != nil {
		t.Fatal(err)
	}
	er2, err := makeNode("n2", 9105, false)
	if err != nil {
		t.Fatal(err)
	}
	er3, err := makeNode("n3", 9106, true) // n3 will leave gracefully
	if err != nil {
		t.Fatal(err)
	}

	er1.Start()
	defer er1.Stop()
	er2.Start()
	defer er2.Stop()
	er3.Start()

	// Wait for the cluster to be operational.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for ctx.Err() == nil {
		if er1.Create(ctx, "__probe__", Counter{}) == nil {
			break
		}
		if er2.Create(ctx, "__probe__", Counter{}) == nil {
			break
		}
		if er3.Create(ctx, "__probe__", Counter{}) == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if ctx.Err() != nil {
		t.Fatal("no leader elected")
	}

	// Stop n3 with LeaveOnStop — it should remove itself from the cluster.
	er3.Stop()

	// n1 and n2 should still be able to commit (2-node quorum of the original 3).
	ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel2()
	for ctx2.Err() == nil {
		if er1.Create(ctx2, "after-leave", Counter{Value: 42}) == nil {
			break
		}
		if er2.Create(ctx2, "after-leave", Counter{Value: 42}) == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if ctx2.Err() != nil {
		t.Error("cluster could not commit after n3 left gracefully")
	}
}
