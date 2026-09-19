package filestore

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/brunoga/raft"
)

// These tests cover the one durability rule that leaves no trace in the files
// themselves: after creating, renaming or removing a name in the data
// directory, the directory has to be fsynced as well.
//
// fsync on a file descriptor makes the file's *contents* durable. On most
// filesystems it says nothing about the directory entry that names the file.
// So a store that writes a segment, fsyncs its descriptors and returns success
// to the Raft engine has told the engine "these entries survive a crash" while
// the name of the file holding them may still exist only in the page cache. A
// crash at that point brings the node back with the segment simply absent and
// the acknowledged entries gone — a lost write, which is exactly what Raft's
// safety argument assumes storage never does.
//
// The crash-injection tests elsewhere in this package model lost writes inside
// files, not lost directory entries, so they cannot see this. These tests
// observe the calls directly through the package's syncDir hook.

// dirSync is one observed call to the directory-fsync hook: the directory it
// was asked to sync, and the names that directory held at that moment.
//
// Recording the listing is what makes the assertions meaningful. A sync of the
// right directory taken *before* the new file was created makes the new name
// no more durable than no sync at all, so a test that only counted calls would
// pass on a store with the fsync in the wrong place.
type dirSync struct {
	dir   string
	names map[string]bool
}

func (d dirSync) String() string {
	return fmt.Sprintf("sync(%s) with [%s]", d.dir,
		strings.Join(slices.Sorted(maps.Keys(d.names)), " "))
}

// dirSyncRecorder collects the calls made while it is installed.
type dirSyncRecorder struct {
	mu    sync.Mutex
	calls []dirSync
}

// recordDirSyncs wraps the package's directory-fsync hook so that a test can
// see every call, and restores the original when the test ends. The real
// fsync still runs underneath: this observes the production path rather than
// replacing it.
func recordDirSyncs(t *testing.T) *dirSyncRecorder {
	t.Helper()

	rec := &dirSyncRecorder{}
	prev := syncDir
	syncDir = func(dir string) error {
		rec.record(dir)
		return prev(dir)
	}
	t.Cleanup(func() { syncDir = prev })

	return rec
}

func (r *dirSyncRecorder) record(dir string) {
	names := make(map[string]bool)
	if ents, err := os.ReadDir(dir); err == nil {
		for _, e := range ents {
			names[e.Name()] = true
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, dirSync{dir: dir, names: names})
}

// reset drops everything recorded so far, so that a test can set up a store
// and then assert only about the operation it is actually exercising.
func (r *dirSyncRecorder) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = nil
}

// saw reports whether dir was fsynced at a moment when it already held every
// name in present and none of the names in absent.
func (r *dirSyncRecorder) saw(dir string, present, absent []string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, c := range r.calls {
		if c.dir != dir {
			continue
		}
		if slices.ContainsFunc(present, func(n string) bool { return !c.names[n] }) {
			continue
		}
		if slices.ContainsFunc(absent, func(n string) bool { return c.names[n] }) {
			continue
		}
		return true
	}

	return false
}

func (r *dirSyncRecorder) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()

	if len(r.calls) == 0 {
		return "no directory syncs recorded"
	}
	parts := make([]string, 0, len(r.calls))
	for _, c := range r.calls {
		parts = append(parts, c.String())
	}

	return strings.Join(parts, "\n  ")
}

// requireSync fails the test unless dir was fsynced while holding every name
// in present and none of the names in absent.
func requireSync(t *testing.T, rec *dirSyncRecorder, what, dir string, present, absent []string) {
	t.Helper()

	if !rec.saw(dir, present, absent) {
		t.Fatalf("%s: no fsync of %s observed with %v present and %v absent;"+
			" a crash here loses acknowledged entries.\nrecorded:\n  %s",
			what, dir, present, absent, rec)
	}
}

// makeTestEntries builds a contiguous run of entries in term 1.
func makeTestEntries(from, to raft.Index) []raft.LogEntry {
	entries := make([]raft.LogEntry, 0, int(to-from+1))
	for i := from; i <= to; i++ {
		entries = append(entries, raft.LogEntry{
			Index:   i,
			Term:    1,
			Command: []byte("command"),
		})
	}
	return entries
}

// TestMetaFileCreationSyncsDirectory checks that creating the hard-state file
// for the first time is followed by an fsync of the data directory.
//
// The meta file holds currentTerm and votedFor. SaveHardState fsyncs the file
// before returning, and the engine relies on that before it sends a vote or a
// response in a new term. If the directory entry for a freshly created meta
// file is not durable, a crash can bring the node back with no meta file at
// all: term 0, no vote recorded. The node then happily votes a second time in
// a term it already voted in, which can elect two leaders for the same term.
func TestMetaFileCreationSyncsDirectory(t *testing.T) {
	dir := t.TempDir()
	rec := recordDirSyncs(t)

	fs, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = fs.Close() }()

	requireSync(t, rec, "first open", dir, []string{"meta"}, nil)
}

// TestSegmentRotationSyncsDirectory checks that every newly created log
// segment — the first one and each one produced by rotation — is followed by
// an fsync of the data directory before AppendLogEntries returns.
//
// AppendLogEntries fsyncs the segment's log and index descriptors and then
// reports success, at which point the Raft engine counts those entries as
// durable: a leader may advance commitIndex on the strength of them, and a
// follower may acknowledge them to its leader. fsync on those descriptors does
// not publish the segment's *name*, so without the directory fsync a crash can
// bring the node back with the new segment missing entirely and the entries it
// held silently absent — entries the cluster has already been told are safe.
func TestSegmentRotationSyncsDirectory(t *testing.T) {
	dir := t.TempDir()

	// A tiny segment size makes every append after the first roll over.
	fs, err := OpenWithSegmentSize(dir, 1)
	if err != nil {
		t.Fatalf("OpenWithSegmentSize: %v", err)
	}
	defer func() { _ = fs.Close() }()

	ctx := context.Background()
	rec := recordDirSyncs(t)

	// The first append creates segment 0; the second rotates into segment 1.
	for i := raft.Index(1); i <= 2; i++ {
		rec.reset()
		if err = fs.AppendLogEntries(ctx, []raft.LogEntry{
			{Index: i, Term: 1, Command: []byte("command")},
		}); err != nil {
			t.Fatalf("AppendLogEntries(%d): %v", i, err)
		}

		seg := fmt.Sprintf(segNameFormat, int(i-1))
		requireSync(t, rec, "append creating "+seg, dir,
			[]string{seg + ".log", seg + ".idx"}, nil)
	}

	// Guard the premise: the appends really did land in two distinct segments.
	if len(fs.segs) != 2 {
		t.Fatalf("expected 2 segments after rotation, got %d", len(fs.segs))
	}
}

// TestSnapshotPromotionSyncsDirectory checks that promoting snap.tmp to snap
// is followed by an fsync of the data directory.
//
// SaveSnapshot returns only once the snapshot is durable, and the engine is
// then free to discard the log prefix the snapshot covers. A rename is atomic
// with respect to readers but not durable on its own: without the directory
// fsync a crash can leave the directory entry pointing back at nothing, or at
// a snap.tmp that the next Open deliberately deletes as abandoned work. Either
// way the snapshot is gone while the entries it replaced have already been
// truncated away, and the node can no longer reconstruct its own state.
func TestSnapshotPromotionSyncsDirectory(t *testing.T) {
	dir := t.TempDir()

	fs, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = fs.Close() }()

	rec := recordDirSyncs(t)

	meta := raft.SnapshotMeta{LastIncludedIndex: 5, LastIncludedTerm: 2}
	if err = fs.SaveSnapshot(context.Background(), meta,
		bytes.NewReader([]byte("state"))); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}

	// The tmp name must already be gone at sync time: syncing before the
	// rename would make the temporary entry durable and the final one not.
	requireSync(t, rec, "snapshot promotion", dir,
		[]string{"snap"}, []string{"snap.tmp"})
}

// TestTruncatePrefixUnlinkSyncsDirectory checks that removing a segment that
// a prefix truncation has made entirely superseded is followed by an fsync of
// the data directory.
//
// TruncatePrefix is called once a snapshot has captured the entries it
// discards, and the caller treats its return as final. Removals in a directory
// are not ordered with respect to one another unless the directory is fsynced
// between them, so without these syncs a crash part-way through can leave a
// hole in the middle of the segment sequence — say segment 0 and segment 2
// present with segment 1 gone. Nothing on disk says whether the live entries
// are the run before the hole or the run after it, so recovery keeps only the
// leading run and everything past the hole is discarded, acknowledged or not.
func TestTruncatePrefixUnlinkSyncsDirectory(t *testing.T) {
	dir := t.TempDir()

	fs, err := OpenWithSegmentSize(dir, 1)
	if err != nil {
		t.Fatalf("OpenWithSegmentSize: %v", err)
	}
	defer func() { _ = fs.Close() }()

	// One entry per segment, so truncating past the first few discards whole
	// segments rather than rewriting one.
	ctx := context.Background()
	for i := raft.Index(1); i <= 4; i++ {
		if err = fs.AppendLogEntries(ctx, []raft.LogEntry{
			{Index: i, Term: 1, Command: []byte("command")},
		}); err != nil {
			t.Fatalf("AppendLogEntries(%d): %v", i, err)
		}
	}

	dropped := fmt.Sprintf(segNameFormat, 0)
	rec := recordDirSyncs(t)

	if err = fs.TruncatePrefix(ctx, 4); err != nil {
		t.Fatalf("TruncatePrefix: %v", err)
	}

	requireSync(t, rec, "unlink of superseded "+dropped, dir,
		nil, []string{dropped + ".log", dropped + ".idx"})

	// Guard the premise: the truncation really did drop whole segments.
	first, err := fs.FirstIndex()
	if err != nil {
		t.Fatalf("FirstIndex: %v", err)
	}
	if first != 4 {
		t.Fatalf("FirstIndex after TruncatePrefix(4) = %d, want 4", first)
	}
}

// TestTruncatePrefixRewriteSyncsDirectory checks that the rename which commits
// the rewritten boundary segment is followed by an fsync of the data
// directory.
//
// The segment straddling the truncation point is rewritten into .log.tmp and
// .idx.tmp and then renamed over the originals. A rename is atomic for
// readers, but the new directory entries are no more durable than any other:
// without the fsync a crash can bring back the pre-truncation file, or leave
// only one of the two renames visible. In the second case the next Open sees a
// segment whose log and index disagree about which entries it holds, and
// recovery resolves that by truncating the segment back to the last slot the
// pair agrees on — dropping entries the engine was told were durable.
//
// The store is opened with a segment size large enough that the whole log
// lives in one segment, so the rewrite is the only thing in this test that can
// sync the directory.
func TestTruncatePrefixRewriteSyncsDirectory(t *testing.T) {
	dir := t.TempDir()

	fs, err := OpenWithSegmentSize(dir, 1<<20)
	if err != nil {
		t.Fatalf("OpenWithSegmentSize: %v", err)
	}
	defer func() { _ = fs.Close() }()

	ctx := context.Background()
	if err = fs.AppendLogEntries(ctx, makeTestEntries(1, 5)); err != nil {
		t.Fatalf("AppendLogEntries: %v", err)
	}
	if len(fs.segs) != 1 {
		t.Fatalf("expected a single segment, got %d", len(fs.segs))
	}
	boundary := fs.segs[0].name

	rec := recordDirSyncs(t)
	if err = fs.TruncatePrefix(ctx, 3); err != nil {
		t.Fatalf("TruncatePrefix: %v", err)
	}

	// The tmp names must already be gone at sync time: a sync taken before the
	// renames makes the temporaries durable and the final names not.
	requireSync(t, rec, "rewrite of boundary segment "+boundary, dir,
		[]string{boundary + ".log", boundary + ".idx"},
		[]string{boundary + ".log.tmp", boundary + ".idx.tmp"})

	// Guard the premise: the truncation rewrote the segment in place rather
	// than unlinking it.
	first, err := fs.FirstIndex()
	if err != nil {
		t.Fatalf("FirstIndex: %v", err)
	}
	if first != 3 {
		t.Fatalf("FirstIndex after TruncatePrefix(3) = %d, want 3", first)
	}
}

// TestTruncateSuffixSyncsDirectory checks that unlinking segments made empty
// by a suffix truncation is followed by an fsync of the data directory.
//
// A suffix truncation runs when a follower is told its tail conflicts with the
// leader's. It has to be durable before the follower accepts the replacement
// entries: if the removal of a discarded segment is not durable, a crash can
// bring the file back alongside the entries that replaced it, leaving two
// different entries claiming the same index — precisely the divergence the
// truncation existed to remove.
func TestTruncateSuffixSyncsDirectory(t *testing.T) {
	dir := t.TempDir()

	fs, err := OpenWithSegmentSize(dir, 1)
	if err != nil {
		t.Fatalf("OpenWithSegmentSize: %v", err)
	}
	defer func() { _ = fs.Close() }()

	ctx := context.Background()
	for i := raft.Index(1); i <= 3; i++ {
		if err = fs.AppendLogEntries(ctx, []raft.LogEntry{
			{Index: i, Term: 1, Command: []byte("command")},
		}); err != nil {
			t.Fatalf("AppendLogEntries(%d): %v", i, err)
		}
	}
	last := fmt.Sprintf(segNameFormat, len(fs.segs)-1)

	rec := recordDirSyncs(t)
	if err = fs.TruncateSuffix(ctx, 3); err != nil {
		t.Fatalf("TruncateSuffix: %v", err)
	}

	requireSync(t, rec, "unlink of truncated "+last, dir,
		nil, []string{last + ".log", last + ".idx"})
}

// TestDataDirectoryCreationSyncsItsParents checks the level nobody thinks
// about: the data directory's own name.
//
// Every other test here covers a name created *inside* the data directory. The
// data directory is itself a name inside its parent, created on first start by
// the store, and subject to the same rule. If it is not made durable, a node
// can create it, persist a vote and a run of entries, fsync each of those
// files, acknowledge them to the cluster, and then crash to find the whole
// directory gone — rejoining with the same ID, an empty log and no memory of
// its vote. Fsyncing files inside a directory whose own name is not durable
// buys nothing at all.
//
// The store is opened at a path two levels below an existing directory, so the
// assertion covers the whole created chain rather than just the last link.
func TestDataDirectoryCreationSyncsItsParents(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "cluster", "node1", "raft")

	rec := recordDirSyncs(t)

	fs, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = fs.Close() }()

	// Each created directory is durable only once the directory holding its
	// name has been fsynced, so walk the chain: base must have been synced
	// holding "cluster", and so on down to the data directory itself.
	requireSync(t, rec, "creating the data directory",
		base, []string{"cluster"}, nil)
	requireSync(t, rec, "creating the data directory",
		filepath.Join(base, "cluster"), []string{"node1"}, nil)
	requireSync(t, rec, "creating the data directory",
		filepath.Join(base, "cluster", "node1"), []string{"raft"}, nil)
}

// TestOpeningAnExistingDataDirectorySyncsNoParent pins the other half: the
// fsyncs above are the cost of *creating* the directory, not a cost paid on
// every open. Reopening an existing store must not walk back up the path.
func TestOpeningAnExistingDataDirectorySyncsNoParent(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "raft")

	fs, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if closeErr := fs.Close(); closeErr != nil {
		t.Fatalf("Close: %v", closeErr)
	}

	rec := recordDirSyncs(t)

	fs2, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = fs2.Close() }()

	if rec.saw(base, nil, nil) {
		t.Errorf("reopening an existing store fsynced its parent %s;"+
			" that work belongs to the open that created the directory.\nrecorded:\n  %s",
			base, rec)
	}
}
