package filestore_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/brunoga/raft"
	"github.com/brunoga/raft/storage/filestore"
)

func snapPath(dir string) string    { return filepath.Join(dir, "snap") }
func snapTmpPath(dir string) string { return filepath.Join(dir, "snap.tmp") }

// readSnapshot loads a snapshot and drains it, returning the first error from
// either step. Verification happens as the body is consumed, so a caller that
// only checks LoadSnapshot would miss it.
func readSnapshot(t *testing.T, fs *filestore.FileStore) (raft.SnapshotMeta, []byte, error) {
	t.Helper()
	meta, rc, err := fs.LoadSnapshot(context.Background())
	if err != nil {
		return raft.SnapshotMeta{}, nil, err
	}
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(rc)
	return meta, data, err
}

// --- Streaming without the store lock ---------------------------------------

// gatedReader yields head, then blocks until gate is closed, then yields tail.
// It stands in for the reader SaveSnapshot is given in production: an io.Pipe
// fed by the state machine, or a chunk reader fed one InstallSnapshot message
// at a time by the Raft loop.
type gatedReader struct {
	head, tail []byte
	pos        int
	gate       chan struct{}
	reached    chan struct{}
	once       sync.Once
}

func (g *gatedReader) Read(p []byte) (int, error) {
	if g.pos < len(g.head) {
		n := copy(p, g.head[g.pos:])
		g.pos += n
		return n, nil
	}
	g.once.Do(func() { close(g.reached) })
	<-g.gate

	at := g.pos - len(g.head)
	if at >= len(g.tail) {
		return 0, io.EOF
	}
	n := copy(p, g.tail[at:])
	g.pos += n
	return n, nil
}

// TestSaveSnapshot_DoesNotBlockLogOperations covers the availability rule for
// snapshot writes.
//
// SaveSnapshot copies from a reader the caller controls. On a follower
// installing a snapshot that reader is fed one chunk at a time by the Raft
// loop, and the Raft loop touches storage before it can deliver the next chunk
// — to read a log term, or to persist a new term. Holding the store-wide lock
// across the copy therefore deadlocks the node outright, and on a leader it
// stalls heartbeats for the whole serialisation. Only snapshot writers may be
// serialised while the data is streamed.
func TestSaveSnapshot_DoesNotBlockLogOperations(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	fs, err := filestore.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = fs.Close() }()
	mustAppend(t, fs, 1, 20, 1)

	r := &gatedReader{
		head:    []byte("first-half-"),
		tail:    []byte("second-half"),
		gate:    make(chan struct{}),
		reached: make(chan struct{}),
	}
	meta := raft.SnapshotMeta{LastIncludedIndex: 20, LastIncludedTerm: 1}

	saved := make(chan error, 1)
	go func() { saved <- fs.SaveSnapshot(ctx, meta, r) }()

	select {
	case <-r.reached:
	case <-time.After(10 * time.Second):
		t.Fatal("SaveSnapshot never started reading")
	}

	// The snapshot write is now parked mid-copy. Everything the Raft loop does
	// against storage has to keep working.
	progress := make(chan error, 1)
	go func() {
		if _, readErr := fs.GetLogEntry(ctx, 10); readErr != nil {
			progress <- readErr
			return
		}
		if _, readErr := fs.LastIndex(); readErr != nil {
			progress <- readErr
			return
		}
		if _, readErr := fs.GetLogEntries(ctx, 1, 5); readErr != nil {
			progress <- readErr
			return
		}
		progress <- fs.SaveHardState(ctx, raft.HardState{CurrentTerm: 3, VotedFor: "n1"})
	}()

	var blocked bool
	select {
	case err = <-progress:
		if err != nil {
			t.Errorf("log operation during SaveSnapshot: %v", err)
		}
	case <-time.After(10 * time.Second):
		blocked = true
	}

	close(r.gate)
	if saveErr := <-saved; saveErr != nil {
		t.Fatalf("SaveSnapshot: %v", saveErr)
	}
	if blocked {
		t.Fatal("log operations were blocked while snapshot data was being copied: " +
			"a follower installing a snapshot would deadlock here")
	}

	gotMeta, data, err := readSnapshot(t, fs)
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	if gotMeta != meta {
		t.Fatalf("snapshot meta = %+v, want %+v", gotMeta, meta)
	}
	if want := "first-half-second-half"; string(data) != want {
		t.Fatalf("snapshot body = %q, want %q", data, want)
	}
}

// TestSaveSnapshot_ConcurrentWritersAreSerialised checks that dropping the
// store-wide lock did not make concurrent snapshot writes race: whichever one
// wins, the committed snapshot must be one complete snapshot, never a mix.
func TestSaveSnapshot_ConcurrentWritersAreSerialised(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	fs, err := filestore.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = fs.Close() }()

	bodies := map[raft.Index]string{
		10: "body-from-the-first-writer",
		20: "body-from-the-second-writer",
	}

	var wg sync.WaitGroup
	for index, body := range bodies {
		wg.Add(1)
		go func() {
			defer wg.Done()
			meta := raft.SnapshotMeta{LastIncludedIndex: index, LastIncludedTerm: 1}
			if saveErr := fs.SaveSnapshot(ctx, meta, bytes.NewReader([]byte(body))); saveErr != nil {
				t.Errorf("SaveSnapshot(%d): %v", index, saveErr)
			}
		}()
	}
	wg.Wait()

	meta, data, err := readSnapshot(t, fs)
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	want, ok := bodies[meta.LastIncludedIndex]
	if !ok {
		t.Fatalf("snapshot meta = %+v, which matches neither writer", meta)
	}
	if string(data) != want {
		t.Fatalf("snapshot for index %d has body %q, want %q",
			meta.LastIncludedIndex, data, want)
	}
}

// --- Framing and verification -----------------------------------------------

func saveSnapshotFixture(t *testing.T, dir string, body []byte) raft.SnapshotMeta {
	t.Helper()
	fs, err := filestore.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	meta := raft.SnapshotMeta{LastIncludedIndex: 42, LastIncludedTerm: 3}
	if err = fs.SaveSnapshot(context.Background(), meta, bytes.NewReader(body)); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}
	if err = fs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return meta
}

// TestSnapshot_DamageIsDetected covers the snapshot body framing. Without a
// length and a checksum stored alongside the data, a snapshot truncated after
// it was committed, or one that suffered a bit flip at rest, is industinguishable
// from a good one and gets fed to the state machine as if it were intact.
func TestSnapshot_DamageIsDetected(t *testing.T) {
	body := bytes.Repeat([]byte("payload-"), 512)

	tests := []struct {
		name   string
		damage func(t *testing.T, path string)
	}{
		{
			name: "body cut short after the snapshot was committed",
			damage: func(t *testing.T, path string) {
				st, err := os.Stat(path)
				if err != nil {
					t.Fatalf("stat snap: %v", err)
				}
				if err = os.Truncate(path, st.Size()-64); err != nil {
					t.Fatalf("truncate snap: %v", err)
				}
			},
		},
		{
			name: "single byte of the body flipped",
			damage: func(t *testing.T, path string) {
				content, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("read snap: %v", err)
				}
				content[len(content)/2] ^= 0xFF
				if err = os.WriteFile(path, content, 0o600); err != nil {
					t.Fatalf("rewrite snap: %v", err)
				}
			},
		},
		{
			name: "trailing bytes appended",
			damage: func(t *testing.T, path string) {
				f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
				if err != nil {
					t.Fatalf("open snap: %v", err)
				}
				if _, err = f.WriteString("extra"); err != nil {
					t.Fatalf("append to snap: %v", err)
				}
				if err = f.Close(); err != nil {
					t.Fatalf("close snap: %v", err)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			saveSnapshotFixture(t, dir, body)
			tc.damage(t, snapPath(dir))

			fs, err := filestore.Open(dir)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer func() { _ = fs.Close() }()

			_, data, err := readSnapshot(t, fs)
			if err == nil {
				t.Fatalf("a damaged snapshot was returned as valid (%d bytes)", len(data))
			}
		})
	}
}

// TestSnapshot_IntactSnapshotVerifies guards against the verification being so
// strict that a good snapshot fails it.
func TestSnapshot_IntactSnapshotVerifies(t *testing.T) {
	for _, size := range []int{0, 1, 4096, 1 << 20} {
		dir := t.TempDir()
		body := make([]byte, size)
		for i := range body {
			body[i] = byte(i)
		}
		meta := saveSnapshotFixture(t, dir, body)

		fs, err := filestore.Open(dir)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		gotMeta, data, err := readSnapshot(t, fs)
		if err != nil {
			t.Fatalf("size %d: LoadSnapshot: %v", size, err)
		}
		if gotMeta != meta {
			t.Errorf("size %d: meta = %+v, want %+v", size, gotMeta, meta)
		}
		if !bytes.Equal(data, body) {
			t.Errorf("size %d: body round-trip mismatch (%d bytes back)", size, len(data))
		}
		_ = fs.Close()
	}
}

// TestSaveSnapshot_RemovesTheTemporaryFileOnError checks that a failed
// snapshot write cleans up after itself and leaves the previous snapshot
// alone. A leftover snap.tmp wastes the space of a whole snapshot until the
// store is next opened.
func TestSaveSnapshot_RemovesTheTemporaryFileOnError(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	good := []byte("the snapshot that should survive")
	saveSnapshotFixture(t, dir, good)

	fs, err := filestore.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = fs.Close() }()

	boom := errors.New("state machine failed mid-snapshot")
	failing := io.MultiReader(
		bytes.NewReader(bytes.Repeat([]byte("partial"), 1024)),
		&errReader{err: boom},
	)

	err = fs.SaveSnapshot(ctx, raft.SnapshotMeta{LastIncludedIndex: 99}, failing)
	if err == nil {
		t.Fatal("SaveSnapshot reported success although the reader failed")
	}
	if !errors.Is(err, boom) {
		t.Errorf("SaveSnapshot error = %v, want it to wrap the reader's error", err)
	}

	if _, statErr := os.Stat(snapTmpPath(dir)); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("snap.tmp was left behind: stat returned %v", statErr)
	}

	// The previously committed snapshot must be untouched.
	_, data, err := readSnapshot(t, fs)
	if err != nil {
		t.Fatalf("the committed snapshot was damaged by the failed write: %v", err)
	}
	if !bytes.Equal(data, good) {
		t.Fatalf("committed snapshot body = %q, want %q", data, good)
	}
}

type errReader struct{ err error }

func (r *errReader) Read([]byte) (int, error) { return 0, r.err }

// TestOpen_RemovesAStaleTemporarySnapshot checks that a snapshot interrupted by
// a crash does not linger. Only the file under its final name is meaningful.
func TestOpen_RemovesAStaleTemporarySnapshot(t *testing.T) {
	dir := t.TempDir()

	good := []byte("committed")
	saveSnapshotFixture(t, dir, good)

	if err := os.WriteFile(snapTmpPath(dir), bytes.Repeat([]byte("junk"), 4096), 0o600); err != nil {
		t.Fatalf("write snap.tmp: %v", err)
	}

	fs, err := filestore.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = fs.Close() }()

	if _, statErr := os.Stat(snapTmpPath(dir)); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("a stale snap.tmp survived Open: stat returned %v", statErr)
	}

	_, data, err := readSnapshot(t, fs)
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	if !bytes.Equal(data, good) {
		t.Fatalf("snapshot body = %q, want %q", data, good)
	}
}

// TestSnapshot_LegacyUnframedFileIsStillReadable checks that snapshots written
// before the length and checksum were added still load. They cannot be
// verified, but refusing to read them would strand an upgrading node.
func TestSnapshot_LegacyUnframedFileIsStillReadable(t *testing.T) {
	dir := t.TempDir()
	body := []byte("a snapshot from an older release")

	var hdr [16]byte
	binary.LittleEndian.PutUint64(hdr[0:8], 77)
	binary.LittleEndian.PutUint64(hdr[8:16], 4)
	if err := os.WriteFile(snapPath(dir), append(hdr[:], body...), 0o600); err != nil {
		t.Fatalf("write legacy snap: %v", err)
	}

	fs, err := filestore.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = fs.Close() }()

	meta, data, err := readSnapshot(t, fs)
	if err != nil {
		t.Fatalf("LoadSnapshot on a legacy snapshot: %v", err)
	}
	want := raft.SnapshotMeta{LastIncludedIndex: 77, LastIncludedTerm: 4}
	if meta != want {
		t.Fatalf("meta = %+v, want %+v", meta, want)
	}
	if !bytes.Equal(data, body) {
		t.Fatalf("body = %q, want %q", data, body)
	}

	// Saving over it must produce a snapshot in the current, verifiable format.
	replacement := []byte("replacement written by this release")
	if err = fs.SaveSnapshot(context.Background(),
		raft.SnapshotMeta{LastIncludedIndex: 80, LastIncludedTerm: 5},
		bytes.NewReader(replacement)); err != nil {
		t.Fatalf("SaveSnapshot over a legacy snapshot: %v", err)
	}

	content, err := os.ReadFile(snapPath(dir))
	if err != nil {
		t.Fatalf("read snap: %v", err)
	}
	content[len(content)/2] ^= 0xFF
	if err = os.WriteFile(snapPath(dir), content, 0o600); err != nil {
		t.Fatalf("rewrite snap: %v", err)
	}
	if _, _, err = readSnapshot(t, fs); err == nil {
		t.Error("the rewritten snapshot is not verified, so it kept the legacy format")
	}
}

// TestLoadSnapshot_NoSnapshotIsDistinctFromADamagedOne keeps the "nothing
// saved" signal separate from a read failure.
func TestLoadSnapshot_NoSnapshotIsDistinctFromADamagedOne(t *testing.T) {
	dir := t.TempDir()

	fs, err := filestore.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = fs.Close() }()

	if _, _, err = fs.LoadSnapshot(context.Background()); !errors.Is(err, raft.ErrNoSnapshot) {
		t.Fatalf("LoadSnapshot on an empty store = %v, want ErrNoSnapshot", err)
	}
}
