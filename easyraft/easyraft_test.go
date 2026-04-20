package easyraft_test

import (
	"context"
	"encoding/json"
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
			if err := er.Create(ctx, "discovery-probe", Counter{Value: 1}); err == nil {
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
