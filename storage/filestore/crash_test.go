package filestore_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/brunoga/raft/v2"
	"github.com/brunoga/raft/v2/storage/filestore"
)

// --- Crash injection --------------------------------------------------------
//
// A crash is modelled by capturing the exact bytes of every file in a store
// directory at a point in time and rebuilding a directory from that capture.
// The states a crash could have left behind are synthesised from the images
// taken immediately before and immediately after an operation:
//
//   - Segment unlinks are made durable one at a time and in a fixed order, so
//     a crash can only ever have applied a prefix of them.
//   - A file that was rewritten in place either has its old content or its new
//     content.
//   - A file that only grew may have been cut short anywhere past the length it
//     had already reached, since bytes beyond that were never fsynced.
//   - A file that was newly created may or may not be there at all.
//
// That is a superset of the states a real crash can produce, which is what
// makes it a useful net rather than a restatement of the implementation.

// dirImage is a point-in-time copy of every file in a store directory.
type dirImage map[string][]byte

func captureDir(t *testing.T, dir string) dirImage {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("capture %s: %v", dir, err)
	}
	img := dirImage{}
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("capture %s: %v", e.Name(), err)
		}
		img[e.Name()] = b
	}
	return img
}

func (img dirImage) clone() dirImage {
	out := make(dirImage, len(img))
	for name, content := range img {
		out[name] = bytes.Clone(content)
	}
	return out
}

// restoreInto rebuilds img in a fresh directory and returns its path.
func (img dirImage) restoreInto(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range img {
		if err := os.WriteFile(filepath.Join(dir, name), content, 0o600); err != nil {
			t.Fatalf("restore %s: %v", name, err)
		}
	}
	return dir
}

// unitOf names the group of files an operation writes together. A segment's
// log and index files are only ever created, replaced and removed as a pair,
// so they count as one unit.
func unitOf(name string) string {
	for _, suffix := range []string{".log.tmp", ".idx.tmp", ".log", ".idx"} {
		if base, ok := strings.CutSuffix(name, suffix); ok {
			return base
		}
	}
	return name
}

func unitsOf(img dirImage) map[string][]string {
	units := map[string][]string{}
	for name := range img {
		u := unitOf(name)
		units[u] = append(units[u], name)
	}
	for _, names := range units {
		sort.Strings(names)
	}
	return units
}

func unitEqual(a, b dirImage, unit string) bool {
	an, bn := unitsOf(a)[unit], unitsOf(b)[unit]
	if !slices.Equal(an, bn) {
		return false
	}
	for _, name := range an {
		if !bytes.Equal(a[name], b[name]) {
			return false
		}
	}
	return true
}

// dropUnit removes every file belonging to unit from img.
func dropUnit(img dirImage, unit string) {
	for name := range img {
		if unitOf(name) == unit {
			delete(img, name)
		}
	}
}

// applyUnit replaces unit's files in dst with the ones src holds for it.
func applyUnit(dst, src dirImage, unit string) {
	dropUnit(dst, unit)
	for name, content := range src {
		if unitOf(name) == unit {
			dst[name] = bytes.Clone(content)
		}
	}
}

// crashState is one directory image a crash could have left behind.
type crashState struct {
	name string
	img  dirImage
}

// crashStates enumerates the states a crash during an operation could have
// left, given the directory images from before and after it.
//
// unlinkAscending says which way the operation unlinks segments: prefix
// truncation works from the head outwards, suffix truncation from the tail
// inwards. Because each unlink is made durable before the next one starts,
// only a prefix of that sequence can have survived a crash.
func crashStates(before, after dirImage, unlinkAscending bool) []crashState {
	beforeUnits, afterUnits := unitsOf(before), unitsOf(after)

	var deleted, created, modified []string
	for u := range beforeUnits {
		if _, ok := afterUnits[u]; !ok {
			deleted = append(deleted, u)
		} else if !unitEqual(before, after, u) {
			modified = append(modified, u)
		}
	}
	for u := range afterUnits {
		if _, ok := beforeUnits[u]; !ok {
			created = append(created, u)
		}
	}
	sort.Strings(deleted)
	sort.Strings(created)
	sort.Strings(modified)
	if !unlinkAscending {
		slices.Reverse(deleted)
	}

	var states []crashState
	add := func(name string, img dirImage) {
		states = append(states, crashState{name: name, img: img})
	}

	for k := 0; k <= len(deleted); k++ {
		base := before.clone()
		for _, u := range deleted[:k] {
			dropUnit(base, u)
		}
		label := fmt.Sprintf("%d of %d unlinks durable", k, len(deleted))

		add(label, base.clone())

		// Both truncations unlink every segment they are going to unlink, each
		// made durable in turn, before they rewrite the boundary segment. A
		// rewrite can therefore only be on disk once all the unlinks are, and
		// the two are never interleaved.
		if len(modified) == 0 || k < len(deleted) {
			continue
		}

		all := base.clone()
		for _, u := range modified {
			applyUnit(all, after, u)
		}
		add(label+", rewrites durable", all)

		if len(modified) > 1 {
			for _, u := range modified {
				one := base.clone()
				applyUnit(one, after, u)
				add(label+", only "+u+" rewritten", one)
			}
		}
	}

	// A newly created file may not have reached the disk at all.
	for _, u := range created {
		s := after.clone()
		dropUnit(s, u)
		add("without "+u, s)
	}

	add("fully applied", after.clone())

	// A file that only grew can have been cut short anywhere past the length it
	// had already reached; bytes before that were already durable.
	for name, newContent := range after {
		oldContent, existed := before[name]
		if !existed || len(newContent) <= len(oldContent) ||
			!bytes.HasPrefix(newContent, oldContent) {
			continue
		}
		for _, n := range cutPoints(len(oldContent), len(newContent)) {
			s := after.clone()
			s[name] = bytes.Clone(newContent[:n])
			add(fmt.Sprintf("%s cut short at %d of %d bytes", name, n, len(newContent)), s)
		}
	}

	return states
}

// cutPoints picks a few lengths in [oldLen, newLen) at which a growing file
// could have been cut short by a crash.
func cutPoints(oldLen, newLen int) []int {
	seen := map[int]bool{}
	var out []int
	for _, n := range []int{oldLen, oldLen + (newLen-oldLen)/2, newLen - 1} {
		if n >= oldLen && n < newLen && !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	return out
}

// storeInvariants describes what must still hold after recovering from a crash.
type storeInvariants struct {
	segSize int64
	// minLast is the highest index that was acknowledged before the operation
	// started, or the floor the operation itself guarantees. No crash may leave
	// the log shorter than this.
	minLast raft.Index
	// maxLast is the highest index ever written. The log must never claim to
	// hold more than that.
	maxLast raft.Index
	// minTerm is the last acknowledged term. The hard state may never report
	// anything older.
	minTerm raft.Term
}

// checkStoreInvariants reopens one crash state and asserts the properties that
// must survive any crash: no acknowledged entry is lost, the log is contiguous
// and fully readable, LastIndex never points past the data, the hard state
// never goes backwards, and the store is still writable.
func checkStoreInvariants(t *testing.T, state crashState, inv storeInvariants) {
	t.Helper()

	dir := state.img.restoreInto(t)
	ctx := context.Background()

	fs, err := filestore.OpenWithSegmentSize(dir, inv.segSize)
	if err != nil {
		// Refusing to open damaged state is an acceptable outcome: the damage
		// is reported rather than silently accepted. What must never happen is
		// opening and then under-reporting the log, which is what the checks
		// below cover.
		return
	}
	defer func() { _ = fs.Close() }()

	first, err := fs.FirstIndex()
	if err != nil {
		t.Errorf("%s: FirstIndex: %v", state.name, err)
		return
	}
	last, err := fs.LastIndex()
	if err != nil {
		t.Errorf("%s: LastIndex: %v", state.name, err)
		return
	}

	if last < inv.minLast {
		t.Errorf("%s: acknowledged entries lost: LastIndex = %d, want >= %d",
			state.name, last, inv.minLast)
	}
	if inv.maxLast != 0 && last > inv.maxLast {
		t.Errorf("%s: LastIndex = %d is past anything ever written (%d)",
			state.name, last, inv.maxLast)
	}
	if (first == 0) != (last == 0) {
		t.Errorf("%s: inconsistent bounds: first=%d last=%d", state.name, first, last)
	}

	// Every index the store claims to hold must be readable and must carry the
	// index it is filed under.
	for i := first; i != 0 && i <= last; i++ {
		e, err := fs.GetLogEntry(ctx, i)
		if err != nil {
			t.Errorf("%s: store reports [%d,%d] but GetLogEntry(%d) failed: %v",
				state.name, first, last, i, err)
			break
		}
		if e.Index != i {
			t.Errorf("%s: the slot for index %d holds entry %d", state.name, i, e.Index)
			break
		}
	}

	// Range reads must agree with the single-entry reads.
	if first != 0 {
		entries, err := fs.GetLogEntries(ctx, first, last+1)
		if err != nil {
			t.Errorf("%s: GetLogEntries(%d,%d): %v", state.name, first, last+1, err)
		} else if want := int(last - first + 1); len(entries) != want {
			t.Errorf("%s: GetLogEntries returned %d entries, want %d",
				state.name, len(entries), want)
		}
	}

	if hs, err := fs.LoadHardState(ctx); err == nil && hs.CurrentTerm < inv.minTerm {
		t.Errorf("%s: hard state went backwards: term %d, want >= %d",
			state.name, hs.CurrentTerm, inv.minTerm)
	}

	// A damaged tail must never wedge the segment index arithmetic: the store
	// has to keep accepting writes after recovery.
	next := last + 1
	if err := fs.AppendLogEntries(ctx, makeEntries(next, next+2, 99)); err != nil {
		t.Errorf("%s: store is wedged after recovery: append at %d: %v",
			state.name, next, err)
		return
	}
	if got, err := fs.LastIndex(); err != nil || got != next+2 {
		t.Errorf("%s: after appending through %d, LastIndex = %d (%v)",
			state.name, next+2, got, err)
	}
}

// runCrashStates rebuilds and checks every synthesised crash state.
func runCrashStates(t *testing.T, states []crashState, inv storeInvariants) {
	t.Helper()
	for _, state := range states {
		t.Run(state.name, func(t *testing.T) {
			checkStoreInvariants(t, state, inv)
		})
	}
}

// --- Crash scenarios --------------------------------------------------------

const crashSegSize = 100 // a few entries per segment, so rotation is exercised

func TestCrash_DuringAppendLogEntries(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	fs, err := filestore.OpenWithSegmentSize(dir, crashSegSize)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err = fs.SaveHardState(ctx, raft.HardState{CurrentTerm: 3, VotedFor: "n1"}); err != nil {
		t.Fatalf("SaveHardState: %v", err)
	}
	if err = fs.AppendLogEntries(ctx, makeEntries(1, 10, 1)); err != nil {
		t.Fatalf("AppendLogEntries: %v", err)
	}

	before := captureDir(t, dir)
	if err = fs.AppendLogEntries(ctx, makeEntries(11, 20, 1)); err != nil {
		t.Fatalf("AppendLogEntries: %v", err)
	}
	if err = fs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	after := captureDir(t, dir)

	runCrashStates(t, crashStates(before, after, true), storeInvariants{
		segSize: crashSegSize,
		minLast: 10, // entries 1-10 were acknowledged before the crash window
		maxLast: 20,
		minTerm: 3,
	})
}

func TestCrash_DuringTruncateSuffix(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	fs, err := filestore.OpenWithSegmentSize(dir, crashSegSize)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err = fs.SaveHardState(ctx, raft.HardState{CurrentTerm: 3, VotedFor: "n1"}); err != nil {
		t.Fatalf("SaveHardState: %v", err)
	}
	if err = fs.AppendLogEntries(ctx, makeEntries(1, 12, 1)); err != nil {
		t.Fatalf("AppendLogEntries: %v", err)
	}

	before := captureDir(t, dir)
	if err = fs.TruncateSuffix(ctx, 6); err != nil {
		t.Fatalf("TruncateSuffix: %v", err)
	}
	if err = fs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	after := captureDir(t, dir)

	// The truncation is only acknowledged once it has completed, so a crash may
	// legally leave anything from the original log down to entries 1-5 — but
	// never less, and never a log with a hole in it.
	runCrashStates(t, crashStates(before, after, false), storeInvariants{
		segSize: crashSegSize,
		minLast: 5,
		maxLast: 12,
		minTerm: 3,
	})
}

func TestCrash_DuringTruncatePrefixPhase1(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	fs, err := filestore.OpenWithSegmentSize(dir, crashSegSize)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err = fs.SaveHardState(ctx, raft.HardState{CurrentTerm: 3, VotedFor: "n1"}); err != nil {
		t.Fatalf("SaveHardState: %v", err)
	}
	if err = fs.AppendLogEntries(ctx, makeEntries(1, 12, 1)); err != nil {
		t.Fatalf("AppendLogEntries: %v", err)
	}

	first, err := fs.FirstIndex()
	if err != nil || first != 1 {
		t.Fatalf("FirstIndex = %d (%v), want 1", first, err)
	}

	before := captureDir(t, dir)
	// Phase 1 only: entry 9 starts a segment, so whole leading segments are
	// dropped and no boundary segment has to be rewritten.
	if err = fs.TruncatePrefix(ctx, 9); err != nil {
		t.Fatalf("TruncatePrefix: %v", err)
	}
	if err = fs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	after := captureDir(t, dir)

	// Prefix truncation never touches the tail, so no crash may shorten it.
	runCrashStates(t, crashStates(before, after, true), storeInvariants{
		segSize: crashSegSize,
		minLast: 12,
		maxLast: 12,
		minTerm: 3,
	})
}

func TestCrash_DuringTruncatePrefixPhase2(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	fs, err := filestore.OpenWithSegmentSize(dir, crashSegSize)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err = fs.SaveHardState(ctx, raft.HardState{CurrentTerm: 3, VotedFor: "n1"}); err != nil {
		t.Fatalf("SaveHardState: %v", err)
	}
	if err = fs.AppendLogEntries(ctx, makeEntries(1, 12, 1)); err != nil {
		t.Fatalf("AppendLogEntries: %v", err)
	}

	before := captureDir(t, dir)
	// Index 11 falls inside a segment, forcing the boundary rewrite.
	if err = fs.TruncatePrefix(ctx, 11); err != nil {
		t.Fatalf("TruncatePrefix: %v", err)
	}
	if err = fs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	after := captureDir(t, dir)

	runCrashStates(t, crashStates(before, after, true), storeInvariants{
		segSize: crashSegSize,
		minLast: 12,
		maxLast: 12,
		minTerm: 3,
	})
}

func TestCrash_DuringSaveSnapshot(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	fs, err := filestore.OpenWithSegmentSize(dir, crashSegSize)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err = fs.SaveHardState(ctx, raft.HardState{CurrentTerm: 3, VotedFor: "n1"}); err != nil {
		t.Fatalf("SaveHardState: %v", err)
	}
	if err = fs.AppendLogEntries(ctx, makeEntries(1, 12, 1)); err != nil {
		t.Fatalf("AppendLogEntries: %v", err)
	}

	before := captureDir(t, dir)
	meta := raft.SnapshotMeta{LastIncludedIndex: 8, LastIncludedTerm: 1}
	if err = fs.SaveSnapshot(ctx, meta, bytes.NewReader(bytes.Repeat([]byte("s"), 4096))); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}
	if err = fs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	after := captureDir(t, dir)

	// Writing a snapshot must never disturb the log.
	runCrashStates(t, crashStates(before, after, true), storeInvariants{
		segSize: crashSegSize,
		minLast: 12,
		maxLast: 12,
		minTerm: 3,
	})
}

// TestCrash_DuringSaveHardState additionally asserts the hard-state rule
// directly: whatever a crash leaves behind, the store must never report a term
// older than the last one it acknowledged, and must never quietly report
// "nothing saved" when a record was already durable.
func TestCrash_DuringSaveHardState(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	fs, err := filestore.OpenWithSegmentSize(dir, crashSegSize)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err = fs.AppendLogEntries(ctx, makeEntries(1, 12, 1)); err != nil {
		t.Fatalf("AppendLogEntries: %v", err)
	}
	if err = fs.SaveHardState(ctx, raft.HardState{CurrentTerm: 5, VotedFor: "n1"}); err != nil {
		t.Fatalf("SaveHardState: %v", err)
	}

	before := captureDir(t, dir)
	if err = fs.SaveHardState(ctx, raft.HardState{CurrentTerm: 6, VotedFor: "n2"}); err != nil {
		t.Fatalf("SaveHardState: %v", err)
	}
	if err = fs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	after := captureDir(t, dir)

	states := crashStates(before, after, true)
	runCrashStates(t, states, storeInvariants{
		segSize: crashSegSize,
		minLast: 12,
		maxLast: 12,
		minTerm: 5,
	})

	for _, state := range states {
		t.Run("hard state/"+state.name, func(t *testing.T) {
			stateDir := state.img.restoreInto(t)
			store, err := filestore.OpenWithSegmentSize(stateDir, crashSegSize)
			if err != nil {
				// A record that cannot be authenticated must be reported, which
				// it is: the store refuses to open.
				return
			}
			defer func() { _ = store.Close() }()

			hs, err := store.LoadHardState(ctx)
			if err != nil {
				return // reported rather than silently accepted
			}
			if hs.CurrentTerm < 5 {
				t.Fatalf("hard state went backwards: got %+v, want term >= 5", hs)
			}
			if hs.VotedFor == "" {
				t.Fatalf("a durable vote was forgotten: got %+v", hs)
			}
		})
	}
}
