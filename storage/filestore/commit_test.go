package filestore_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/brunoga/raft"
	"github.com/brunoga/raft/storage/filestore"
)

// TestCommitIndex_RoundTrips checks the basic contract: what was recorded is
// what comes back, across a close and reopen.
func TestCommitIndex_RoundTrips(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	fs, err := filestore.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	got, err := fs.LoadCommitIndex(ctx)
	if err != nil {
		t.Fatalf("LoadCommitIndex: %v", err)
	}
	if got != 0 {
		t.Errorf("LoadCommitIndex on a fresh store = %d, want 0", got)
	}
	if sErr := fs.SaveCommitIndex(ctx, 4242); sErr != nil {
		t.Fatalf("SaveCommitIndex: %v", sErr)
	}
	if cErr := fs.Close(); cErr != nil {
		t.Fatalf("Close: %v", cErr)
	}

	reopened, err := filestore.Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = reopened.Close() }()
	got, err = reopened.LoadCommitIndex(ctx)
	if err != nil {
		t.Fatalf("LoadCommitIndex after reopen: %v", err)
	}
	if got != 4242 {
		t.Errorf("LoadCommitIndex after reopen = %d, want 4242", got)
	}
}

// TestCommitIndex_NeverMovesBackwards checks that a lower value is not written
// over a higher one.
//
// A recorded index is a claim that everything at or below it committed, and
// that does not stop being true. A node restarting begins with a commit index
// of zero and climbs; if that could overwrite the record, every restart would
// hand a later disaster recovery a worse answer than it had before.
func TestCommitIndex_NeverMovesBackwards(t *testing.T) {
	ctx := context.Background()
	fs, err := filestore.Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = fs.Close() }()

	if sErr := fs.SaveCommitIndex(ctx, 900); sErr != nil {
		t.Fatalf("SaveCommitIndex(900): %v", sErr)
	}
	if sErr := fs.SaveCommitIndex(ctx, 12); sErr != nil {
		t.Fatalf("SaveCommitIndex(12): %v", sErr)
	}
	got, err := fs.LoadCommitIndex(ctx)
	if err != nil {
		t.Fatalf("LoadCommitIndex: %v", err)
	}
	if got != 900 {
		t.Errorf("LoadCommitIndex = %d after recording 12 over 900, want 900", got)
	}
}

// TestCommitIndex_TornRecordReadsAsNothing checks the one failure that would be
// actively harmful.
//
// The record is written without an fsync, so a crash can leave it half
// updated. A half-written index could read back larger than anything that ever
// committed, and a recovery trusting it would truncate away entries that had
// committed, or report as proven entries nobody has. The checksum turns that
// into "nothing recorded", which is always safe: a floor that is too low costs
// only a wider band.
func TestCommitIndex_TornRecordReadsAsNothing(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	fs, err := filestore.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if sErr := fs.SaveCommitIndex(ctx, 4242); sErr != nil {
		t.Fatalf("SaveCommitIndex: %v", sErr)
	}
	if cErr := fs.Close(); cErr != nil {
		t.Fatalf("Close: %v", cErr)
	}

	// Corrupt the index without touching the checksum, as a torn write would.
	path := filepath.Join(dir, "commit")
	rec, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read commit file: %v", err)
	}
	rec[len(rec)-1] ^= 0xFF
	if wErr := os.WriteFile(path, rec, 0o600); wErr != nil {
		t.Fatalf("write commit file: %v", wErr)
	}

	reopened, err := filestore.Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = reopened.Close() }()

	got, err := reopened.LoadCommitIndex(ctx)
	if err != nil {
		t.Fatalf("LoadCommitIndex: %v", err)
	}
	if got != 0 {
		t.Errorf("LoadCommitIndex on a torn record = %d, want 0: a damaged record must not "+
			"be read as proof that anything committed", got)
	}
}

// FileStore must satisfy the optional interface, or the engine silently falls
// back to the snapshot index and nothing else notices.
var _ raft.CommitRecorder = (*filestore.FileStore)(nil)
