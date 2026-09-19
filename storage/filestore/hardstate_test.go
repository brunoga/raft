package filestore_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/brunoga/raft"
	"github.com/brunoga/raft/storage/filestore"
)

func metaPath(dir string) string { return filepath.Join(dir, "meta") }

// loadHardStateFrom opens the store at dir and reads its hard state, returning
// any error from either step. Both are failure paths a caller has to see.
func loadHardStateFrom(t *testing.T, dir string) (raft.HardState, error) {
	t.Helper()
	fs, err := filestore.Open(dir)
	if err != nil {
		return raft.HardState{}, err
	}
	defer func() { _ = fs.Close() }()
	return fs.LoadHardState(context.Background())
}

// TestHardState_DamagedRecordIsReportedNotTreatedAsEmpty covers the safety rule
// that makes hard state different from every other record in the store.
//
// A hard state that cannot be authenticated must never be reported as "nothing
// saved yet". A zero hard state says the node is in term 0 and has voted for
// nobody, so accepting one silently resurrects a lower term and forgets a vote
// that was already granted — which lets the node vote twice in the same term
// and elect two leaders.
func TestHardState_DamagedRecordIsReportedNotTreatedAsEmpty(t *testing.T) {
	saved := raft.HardState{CurrentTerm: 9, VotedFor: "n2"}

	tests := []struct {
		name   string
		damage func(t *testing.T, path string)
	}{
		{
			name: "record cut short",
			damage: func(t *testing.T, path string) {
				if err := os.Truncate(path, 100); err != nil {
					t.Fatalf("truncate meta: %v", err)
				}
			},
		},
		{
			name: "record overwritten with junk",
			damage: func(t *testing.T, path string) {
				st, err := os.Stat(path)
				if err != nil {
					t.Fatalf("stat meta: %v", err)
				}
				junk := bytes.Repeat([]byte{0xA5}, int(st.Size()))
				if err = os.WriteFile(path, junk, 0o600); err != nil {
					t.Fatalf("overwrite meta: %v", err)
				}
			},
		},
		{
			name: "single bit flipped",
			damage: func(t *testing.T, path string) {
				content, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("read meta: %v", err)
				}
				content[20] ^= 0x01
				if err = os.WriteFile(path, content, 0o600); err != nil {
					t.Fatalf("rewrite meta: %v", err)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			ctx := context.Background()

			fs, err := filestore.Open(dir)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			if err = fs.SaveHardState(ctx, saved); err != nil {
				t.Fatalf("SaveHardState: %v", err)
			}
			if err = fs.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}

			tc.damage(t, metaPath(dir))

			hs, err := loadHardStateFrom(t, dir)
			if err != nil {
				return // the damage was reported, which is what must happen
			}
			if hs == (raft.HardState{}) {
				t.Fatal("a damaged hard-state record was reported as an empty one: " +
					"the node would forget its term and its vote")
			}
			if hs != saved {
				t.Fatalf("hard state = %+v, want %+v or an error", hs, saved)
			}
		})
	}
}

// TestHardState_TornWriteKeepsThePreviousRecord checks the two-slot layout: a
// write that does not complete must leave the previously persisted state
// readable rather than destroying it.
func TestHardState_TornWriteKeepsThePreviousRecord(t *testing.T) {
	tests := []struct {
		name   string
		writes []raft.HardState
		// half says which half of the meta file to destroy. Records alternate
		// between the two halves, so destroying one leaves the other.
		destroySecondHalf bool
		want              raft.HardState
	}{
		{
			name: "newest record destroyed",
			writes: []raft.HardState{
				{CurrentTerm: 4, VotedFor: "n1"},
				{CurrentTerm: 5, VotedFor: "n2"},
			},
			destroySecondHalf: true,
			want:              raft.HardState{CurrentTerm: 4, VotedFor: "n1"},
		},
		{
			name: "older slot destroyed",
			writes: []raft.HardState{
				{CurrentTerm: 4, VotedFor: "n1"},
				{CurrentTerm: 5, VotedFor: "n2"},
			},
			destroySecondHalf: false,
			want:              raft.HardState{CurrentTerm: 5, VotedFor: "n2"},
		},
		{
			name: "third write lands back in the first slot",
			writes: []raft.HardState{
				{CurrentTerm: 4, VotedFor: "n1"},
				{CurrentTerm: 5, VotedFor: "n2"},
				{CurrentTerm: 6, VotedFor: "n3"},
			},
			destroySecondHalf: false,
			want:              raft.HardState{CurrentTerm: 5, VotedFor: "n2"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			ctx := context.Background()

			fs, err := filestore.Open(dir)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			for _, hs := range tc.writes {
				if err = fs.SaveHardState(ctx, hs); err != nil {
					t.Fatalf("SaveHardState(%+v): %v", hs, err)
				}
			}
			if err = fs.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}

			content, err := os.ReadFile(metaPath(dir))
			if err != nil {
				t.Fatalf("read meta: %v", err)
			}
			half := len(content) / 2
			at := 0
			if tc.destroySecondHalf {
				at = half
			}
			copy(content[at:at+half], bytes.Repeat([]byte{0xA5}, half))
			if err = os.WriteFile(metaPath(dir), content, 0o600); err != nil {
				t.Fatalf("rewrite meta: %v", err)
			}

			got, err := loadHardStateFrom(t, dir)
			if err != nil {
				t.Fatalf("a half-destroyed meta file must still yield the other "+
					"slot, got error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("hard state = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestHardState_LegacyRecordIsReadAndMigrated checks that a hard state written
// in the older checksum-free layout is still honoured, and is rewritten in the
// current layout without losing it if the process dies during the migration.
func TestHardState_LegacyRecordIsReadAndMigrated(t *testing.T) {
	const (
		legacySize = 266
		term       = 7
		votedFor   = "peer-9"
	)

	dir := t.TempDir()
	ctx := context.Background()

	rec := make([]byte, legacySize)
	binary.LittleEndian.PutUint64(rec[0:8], term)
	binary.LittleEndian.PutUint16(rec[8:10], uint16(len(votedFor)))
	copy(rec[10:], votedFor)
	if err := os.WriteFile(metaPath(dir), rec, 0o600); err != nil {
		t.Fatalf("write legacy meta: %v", err)
	}

	want := raft.HardState{CurrentTerm: term, VotedFor: votedFor}

	fs, err := filestore.Open(dir)
	if err != nil {
		t.Fatalf("Open on a legacy meta file: %v", err)
	}
	got, err := fs.LoadHardState(ctx)
	if err != nil {
		t.Fatalf("LoadHardState: %v", err)
	}
	if got != want {
		t.Fatalf("hard state = %+v, want %+v", got, want)
	}

	// The migration must have happened on open, before anything else was
	// written, so the record is protected from that point on.
	st, err := os.Stat(metaPath(dir))
	if err != nil {
		t.Fatalf("stat meta: %v", err)
	}
	if st.Size() == legacySize {
		t.Error("the legacy record was read but never migrated")
	}

	// The legacy bytes must survive the migration untouched, so that a crash
	// during it leaves a record that is still readable one way or the other.
	migrated, err := os.ReadFile(metaPath(dir))
	if err != nil {
		t.Fatalf("read meta: %v", err)
	}
	if !bytes.Equal(migrated[:legacySize], rec) {
		t.Error("the migration overwrote the legacy record instead of writing beside it")
	}

	// Reopening must find the migrated record, and further saves must work.
	if err = fs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got, err = loadHardStateFrom(t, dir); err != nil || got != want {
		t.Fatalf("after migration hard state = %+v (%v), want %+v", got, err, want)
	}

	fs2, err := filestore.Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	next := raft.HardState{CurrentTerm: 8, VotedFor: "peer-1"}
	if err = fs2.SaveHardState(ctx, next); err != nil {
		t.Fatalf("SaveHardState after migration: %v", err)
	}
	if err = fs2.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got, err = loadHardStateFrom(t, dir); err != nil || got != next {
		t.Fatalf("after a post-migration save hard state = %+v (%v), want %+v", got, err, next)
	}
}

// TestHardState_NeverGoesBackwardsAcrossReopens walks a long sequence of terms,
// reopening the store between each, and checks that what comes back is always
// exactly what was last written.
func TestHardState_NeverGoesBackwardsAcrossReopens(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	var last raft.HardState
	for term := raft.Term(1); term <= 25; term++ {
		fs, err := filestore.Open(dir)
		if err != nil {
			t.Fatalf("Open at term %d: %v", term, err)
		}

		got, err := fs.LoadHardState(ctx)
		if err != nil {
			t.Fatalf("LoadHardState at term %d: %v", term, err)
		}
		if got != last {
			t.Fatalf("at term %d the store reported %+v, want %+v", term, got, last)
		}
		if got.CurrentTerm > term {
			t.Fatalf("hard state is from the future: %+v at term %d", got, term)
		}

		last = raft.HardState{CurrentTerm: term, VotedFor: raft.NodeID("n" + string(rune('0'+term%10)))}
		if err = fs.SaveHardState(ctx, last); err != nil {
			t.Fatalf("SaveHardState at term %d: %v", term, err)
		}
		if err = fs.Close(); err != nil {
			t.Fatalf("Close at term %d: %v", term, err)
		}
	}
}

// TestHardState_EmptyStoreReportsNothingSaved keeps the "never saved" case
// distinct from the "damaged" case: a store that has never had a hard state
// written must still report a zero one without complaint.
func TestHardState_EmptyStoreReportsNothingSaved(t *testing.T) {
	dir := t.TempDir()

	got, err := loadHardStateFrom(t, dir)
	if err != nil {
		t.Fatalf("LoadHardState on a fresh store: %v", err)
	}
	if got != (raft.HardState{}) {
		t.Fatalf("fresh store reported %+v, want a zero hard state", got)
	}
}

// TestHardState_VotedForTooLongIsRejected checks the bound on the node ID
// field, which is fixed-width on disk.
func TestHardState_VotedForTooLongIsRejected(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	fs, err := filestore.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = fs.Close() }()

	long := raft.NodeID(bytes.Repeat([]byte("x"), 257))
	if err = fs.SaveHardState(ctx, raft.HardState{CurrentTerm: 1, VotedFor: long}); err == nil {
		t.Fatal("SaveHardState accepted a node ID that does not fit the record")
	}
}
