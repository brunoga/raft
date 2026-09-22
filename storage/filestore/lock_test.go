package filestore_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/brunoga/raft/v2"
	"github.com/brunoga/raft/v2/storage/filestore"
)

// TestOpen_RefusesADirectoryAlreadyOpen is the guarantee the lock exists for.
//
// Two FileStores on one directory share nothing: each keeps its own segment
// list, its own hard-state sequence number, and its own idea of where the log
// ends. They overwrite each other's records, and the result is a log with two
// writers' entries interleaved or a hard state that has gone backwards --
// which is how a node votes twice in one term. Nothing reports it when it
// happens; it is found on the next restart, or never.
//
// Before the lock, the second Open succeeded and both stores wrote.
func TestOpen_RefusesADirectoryAlreadyOpen(t *testing.T) {
	dir := t.TempDir()

	first, err := filestore.Open(dir)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	defer func() { _ = first.Close() }()

	second, err := filestore.Open(dir)
	if err == nil {
		_ = second.Close()
		t.Fatal("second Open succeeded: two stores can now write to one directory")
	}
	if !errors.Is(err, filestore.ErrLocked) {
		t.Errorf("second Open error = %v, want one wrapping ErrLocked so callers can tell "+
			"this apart from a corrupt or unreadable directory", err)
	}
}

// TestOpen_SucceedsAfterClose checks that the lock is released, so that a node
// can be restarted and a recovery tool can run once it is stopped. A lock that
// outlives its store turns a restart into an outage.
func TestOpen_SucceedsAfterClose(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	first, err := filestore.Open(dir)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if saveErr := first.SaveHardState(ctx, raft.HardState{CurrentTerm: 7, VotedFor: "n1"}); saveErr != nil {
		t.Fatalf("SaveHardState: %v", saveErr)
	}
	if closeErr := first.Close(); closeErr != nil {
		t.Fatalf("Close: %v", closeErr)
	}

	second, err := filestore.Open(dir)
	if err != nil {
		t.Fatalf("Open after Close: %v", err)
	}
	defer func() { _ = second.Close() }()

	hs, err := second.LoadHardState(ctx)
	if err != nil {
		t.Fatalf("LoadHardState: %v", err)
	}
	if hs.CurrentTerm != 7 || hs.VotedFor != "n1" {
		t.Errorf("hard state = %+v after reopen, want term 7 voted n1", hs)
	}
}

// TestOpen_LockSurvivesAnUncleanExit checks that a lock left by a process that
// died is not honoured by the next one. The kernel drops the lock when the
// descriptor closes, which it does on exit however the process ended, so the
// lock file left behind on disk must not itself be treated as a lock.
//
// A store that refused to open because of a stale file would turn every crash
// into an outage requiring manual cleanup, which is worse than the problem the
// lock solves.
func TestOpen_LockSurvivesAnUncleanExit(t *testing.T) {
	dir := t.TempDir()

	first, err := filestore.Open(dir)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if closeErr := first.Close(); closeErr != nil {
		t.Fatalf("Close: %v", closeErr)
	}

	// The file is still there, as it would be after a crash.
	if _, statErr := os.Stat(filepath.Join(dir, "LOCK")); statErr != nil {
		t.Fatalf("lock file missing after Close: %v", statErr)
	}

	second, err := filestore.Open(dir)
	if err != nil {
		t.Fatalf("Open with a leftover lock file: %v", err)
	}
	_ = second.Close()
}
