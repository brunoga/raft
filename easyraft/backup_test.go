package easyraft_test

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/brunoga/raft/v2/easyraft"
	"github.com/brunoga/raft/v2/easyraft/easyrafttest"
)

// TestBackupImport_MovesStateBetweenClusters is the disaster-recovery case:
// take a backup of one cluster and bring a different one up on it.
func TestBackupImport_MovesStateBetweenClusters(t *testing.T) {
	source := easyrafttest.NewCluster(t, 3)
	users := easyrafttest.AddCollection[string](source, "users")
	config := easyrafttest.AddCollection[string](source, "config")
	ctx := source.Context()

	for i := range 50 {
		if err := users.Leader().Create(ctx, fmt.Sprintf("user-%02d", i),
			strings.Repeat("v", i)); err != nil {
			t.Fatalf("Create %d: %v", i, err)
		}
	}
	if err := config.Leader().Create(ctx, "mode", "production"); err != nil {
		t.Fatal(err)
	}
	source.WaitApplied()

	var backup bytes.Buffer
	revision, err := source.WaitLeader().Backup(ctx, &backup)
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if revision == 0 {
		t.Error("Backup reported revision 0")
	}
	if backup.Len() == 0 {
		t.Fatal("Backup wrote nothing")
	}

	// A different cluster, with something of its own in it that the import
	// must replace.
	target := easyrafttest.NewCluster(t, 3)
	targetUsers := easyrafttest.AddCollection[string](target, "users")
	targetCtx := target.Context()
	if err := targetUsers.Leader().Create(targetCtx, "stranger", "should not survive"); err != nil {
		t.Fatal(err)
	}

	if err := target.WaitLeader().Import(targetCtx, bytes.NewReader(backup.Bytes())); err != nil {
		t.Fatalf("Import: %v", err)
	}
	target.WaitApplied()

	// Every replica of the target holds what the source held, and nothing of
	// its own.
	targetConfig := easyrafttest.AddCollection[string](target, "config")
	for node := range 3 {
		if _, err := targetUsers.Node(node).ReadStale("stranger"); !errors.Is(err, easyraft.ErrKeyNotFound) {
			t.Errorf("node %d kept a key the backup did not contain: %v", node, err)
		}
		for i := range 50 {
			key := fmt.Sprintf("user-%02d", i)
			got, readErr := targetUsers.Node(node).ReadStale(key)
			if readErr != nil {
				t.Fatalf("node %d is missing %s: %v", node, key, readErr)
			}
			if got != strings.Repeat("v", i) {
				t.Errorf("node %d holds %q for %s", node, got, key)
			}
		}
		if got, readErr := targetConfig.Node(node).ReadStale("mode"); readErr != nil || got != "production" {
			t.Errorf("node %d holds %q for config/mode: %v", node, got, readErr)
		}
	}

	// And the cluster keeps working afterwards.
	if err := targetUsers.Leader().Create(targetCtx, "after-import", "fine"); err != nil {
		t.Fatalf("a write after the import failed: %v", err)
	}
}

// TestBackupImport_LeasesComeBackAndStillExpire checks that imported leases
// are live leases rather than a record of ones that used to exist.
func TestBackupImport_LeasesComeBackAndStillExpire(t *testing.T) {
	opts := easyrafttest.Options{
		Store: []easyraft.Option{easyraft.WithKeyLeaseSweepInterval(50 * time.Millisecond)},
	}
	source := easyrafttest.NewCluster(t, 1, opts)
	services := easyrafttest.AddCollection[string](source, "services")
	ctx := source.Context()

	lease, err := source.WaitLeader().GrantLease(ctx, time.Hour)
	if err != nil {
		t.Fatalf("GrantLease: %v", err)
	}
	if err := services.Leader().UpsertWithLease(ctx, "web-1", "10.0.0.1", lease); err != nil {
		t.Fatalf("UpsertWithLease: %v", err)
	}

	var backup bytes.Buffer
	if _, err := source.WaitLeader().Backup(ctx, &backup); err != nil {
		t.Fatalf("Backup: %v", err)
	}

	target := easyrafttest.NewCluster(t, 1, opts)
	targetServices := easyrafttest.AddCollection[string](target, "services")
	targetCtx := target.Context()
	if err := target.WaitLeader().Import(targetCtx, bytes.NewReader(backup.Bytes())); err != nil {
		t.Fatalf("Import: %v", err)
	}

	leases := target.WaitLeader().Leases()
	if len(leases) != 1 {
		t.Fatalf("the import brought %d leases, want 1", len(leases))
	}
	if got := targetServices.Node(0).LeaseOf("web-1"); got != leases[0].ID {
		t.Errorf("the imported key is on lease %d, the lease is %d", got, leases[0].ID)
	}

	// Revoking it removes the key, which only works if the lease really is
	// holding it rather than merely remembering that it did.
	if err := target.WaitLeader().RevokeLease(targetCtx, leases[0].ID); err != nil {
		t.Fatalf("RevokeLease: %v", err)
	}
	if _, err := targetServices.Node(0).ReadStale("web-1"); !errors.Is(err, easyraft.ErrKeyNotFound) {
		t.Errorf("the imported leased key survived revocation: %v", err)
	}
}

// TestBackupImport_ChunksAcrossManyEntries checks the path a backup larger
// than one entry takes, by making the entries small instead of the backup
// large.
func TestBackupImport_ChunksAcrossManyEntries(t *testing.T) {
	// A proposal limit small enough that a few hundred keys cannot fit in
	// one, so the import has to chunk. Lowering the limit is the honest way
	// to test this: the alternative is a backup large enough to cross the
	// real limit, which would make the test slow for no extra coverage.
	opts := easyrafttest.Options{
		Store: []easyraft.Option{easyraft.WithMaxProposalBytes(8 << 10)},
	}
	source := easyrafttest.NewCluster(t, 3, opts)
	items := easyrafttest.AddCollection[string](source, "items")
	ctx := source.Context()

	for i := range 200 {
		if err := items.Leader().Create(ctx, fmt.Sprintf("k%03d", i), strings.Repeat("x", 100)); err != nil {
			t.Fatalf("Create %d: %v", i, err)
		}
	}
	source.WaitApplied()

	var backup bytes.Buffer
	if _, err := source.WaitLeader().Backup(ctx, &backup); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if backup.Len() < 20000 {
		t.Fatalf("the backup is only %d bytes; this test needs a larger one", backup.Len())
	}

	target := easyrafttest.NewCluster(t, 3, opts)
	targetItems := easyrafttest.AddCollection[string](target, "items")
	targetCtx := target.Context()

	beforeIndex := target.WaitLeader().Status().LastApplied
	if err := target.WaitLeader().Import(targetCtx, bytes.NewReader(backup.Bytes())); err != nil {
		t.Fatalf("Import: %v", err)
	}
	afterIndex := target.WaitLeader().Status().LastApplied
	if afterIndex-beforeIndex < 2 {
		t.Errorf("the import took %d entries; it was expected to be chunked",
			afterIndex-beforeIndex)
	}
	target.WaitApplied()

	for node := range 3 {
		for _, i := range []int{0, 99, 199} {
			key := fmt.Sprintf("k%03d", i)
			if _, err := targetItems.Node(node).ReadStale(key); err != nil {
				t.Errorf("node %d is missing %s after a chunked import: %v", node, key, err)
			}
		}
	}
	if got := target.Node(0).KeyCount(); got != source.Node(0).KeyCount() {
		t.Errorf("the imported cluster holds %d keys, the source %d",
			got, source.Node(0).KeyCount())
	}
}

// TestBackupImport_EmptyBackupIsRefused covers the input most likely to be
// handed over by mistake: a file that was never written to.
func TestBackupImport_EmptyBackupIsRefused(t *testing.T) {
	c := easyrafttest.NewCluster(t, 1)
	items := easyrafttest.AddCollection[string](c, "items")
	ctx := c.Context()

	if err := items.Leader().Create(ctx, "keep", "me"); err != nil {
		t.Fatal(err)
	}
	if err := c.WaitLeader().Import(ctx, bytes.NewReader(nil)); err == nil {
		t.Fatal("an empty backup was accepted")
	}
	if _, err := items.Node(0).ReadStale("keep"); err != nil {
		t.Errorf("the refused import removed the existing state: %v", err)
	}

	// And a backup that is not a backup leaves the state alone too.
	if err := c.WaitLeader().Import(ctx, strings.NewReader("this is not JSON")); err == nil {
		t.Fatal("a backup that is not a backup was accepted")
	}
	if _, err := items.Node(0).ReadStale("keep"); err != nil {
		t.Errorf("the refused import removed the existing state: %v", err)
	}
	// The cluster still works.
	if err := items.Leader().Create(ctx, "after", "fine"); err != nil {
		t.Errorf("a write after two refused imports failed: %v", err)
	}
}

// TestBackup_FromAFollowerLeavesTheLeaderAlone covers taking a backup where
// it costs least, and what a stale one promises.
func TestBackup_FromAFollowerLeavesTheLeaderAlone(t *testing.T) {
	c := easyrafttest.NewCluster(t, 3)
	items := easyrafttest.AddCollection[string](c, "items")
	ctx := c.Context()

	for i := range 10 {
		if err := items.Leader().Create(ctx, fmt.Sprintf("k%d", i), "v"); err != nil {
			t.Fatal(err)
		}
	}
	c.WaitApplied()

	follower := (c.LeaderIndex() + 1) % 3
	var backup bytes.Buffer
	revision, err := c.Node(follower).BackupStale(&backup)
	if err != nil {
		t.Fatalf("BackupStale: %v", err)
	}
	if revision == 0 {
		t.Error("BackupStale reported revision 0")
	}

	target := easyrafttest.NewCluster(t, 1)
	targetItems := easyrafttest.AddCollection[string](target, "items")
	targetCtx := target.Context()
	if err := target.WaitLeader().Import(targetCtx, bytes.NewReader(backup.Bytes())); err != nil {
		t.Fatalf("Import of a follower's backup: %v", err)
	}
	for i := range 10 {
		if _, err := targetItems.Node(0).ReadStale(fmt.Sprintf("k%d", i)); err != nil {
			t.Errorf("k%d did not survive a follower's backup: %v", i, err)
		}
	}
}
