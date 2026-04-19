package easyraft_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/brunoga/raft"
	"github.com/brunoga/raft/easyraft"
)

type Counter struct {
	Value uint64 `json:"value"`
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
