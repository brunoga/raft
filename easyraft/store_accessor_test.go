package easyraft_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/brunoga/raft/v2"
	"github.com/brunoga/raft/v2/easyraft"
)

// AuditRecord is a second entity type, of no interest to the wrapper's own
// collection.
type AuditRecord struct {
	Actor string `json:"actor"`
}

// TestEasyRaft_StoreIsReachable checks that a service which started out with
// the single-collection wrapper can do everything the Store underneath it can,
// without being rebuilt as NewStore plus AddCollection.
//
// Outgrowing the wrapper is the normal path, not an edge case: the second
// entity type arrives, or two keys have to change together. Without a way
// through to the Store, that costs a rewrite of the construction and of every
// call site that went through the wrapper -- for a capability the object was
// holding the whole time.
func TestEasyRaft_StoreIsReachable(t *testing.T) {
	addr := freePort(t)
	tmpDir := t.TempDir()

	app, err := easyraft.New[Counter](
		easyraft.WithID("n1"),
		easyraft.WithRaftAddr(addr),
		easyraft.WithDataDir(filepath.Join(tmpDir, "n1")),
		easyraft.WithPeers(map[raft.NodeID]string{"n1": addr}),
		easyraft.WithInsecureTransportAcknowledged(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if startErr := app.Start(); startErr != nil {
		t.Fatalf("Start: %v", startErr)
	}
	defer func() { _ = app.Stop() }()

	store := app.Store()
	if store == nil {
		t.Fatal("Store() returned nil")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if readyErr := app.Ready(ctx); readyErr != nil {
		t.Fatalf("Ready: %v", readyErr)
	}

	// A second collection, added after Start, through the Store the wrapper
	// was already running on.
	audit := easyraft.AddCollection[AuditRecord](store, "audit")
	if createErr := audit.Create(ctx, "a1", AuditRecord{Actor: "operator"}); createErr != nil {
		t.Fatalf("audit Create: %v", createErr)
	}
	got, err := audit.Read(ctx, "a1")
	if err != nil {
		t.Fatalf("audit Read: %v", err)
	}
	if got.Actor != "operator" {
		t.Errorf("audit record = %+v, want Actor \"operator\"", got)
	}

	// A transaction spanning the wrapper's own collection and the new one,
	// committed as a single log entry.
	_, err = store.Txn(ctx, func(tx *easyraft.Txn) error {
		if txErr := tx.Create("default", "c1", Counter{Value: 7}); txErr != nil {
			return txErr
		}
		return tx.Create("audit", "a2", AuditRecord{Actor: "txn"})
	})
	if err != nil {
		t.Fatalf("Txn: %v", err)
	}

	// The wrapper sees what the transaction wrote to its own collection: it is
	// the same store, not a second view of the same data.
	c, err := app.Read(ctx, "c1")
	if err != nil {
		t.Fatalf("Read after Txn: %v", err)
	}
	if c.Value != 7 {
		t.Errorf("counter = %d after Txn, want 7", c.Value)
	}
	if _, err := audit.Read(ctx, "a2"); err != nil {
		t.Fatalf("audit Read after Txn: %v", err)
	}
}
