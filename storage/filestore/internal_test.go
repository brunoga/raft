package filestore

import (
	"encoding/binary"
	"testing"

	"github.com/brunoga/raft"
)

// TestParseSegSeq covers the segment-name parser. Names are matched by parsing
// the sequence number rather than by a fixed-width glob, so that files written
// with a different zero-padding width stay readable and so that the sequence
// number can grow past the padding width without silently disappearing.
func TestParseSegSeq(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantSeq int
		wantOK  bool
	}{
		{"current width", "seg-0000000042", 42, true},
		{"narrow legacy width", "seg-00042", 42, true},
		{"single digit", "seg-7", 7, true},
		{"wider than the padding width", "seg-12345678901", 12345678901, true},
		{"zero", "seg-0000000000", 0, true},
		{"missing prefix", "log-00001", 0, false},
		{"no digits", "seg-", 0, false},
		{"non numeric", "seg-abcde", 0, false},
		{"trailing junk", "seg-00001x", 0, false},
		{"negative", "seg--1", 0, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			seq, ok := parseSegSeq(tc.in)
			if ok != tc.wantOK {
				t.Fatalf("parseSegSeq(%q) ok = %v, want %v", tc.in, ok, tc.wantOK)
			}
			if ok && seq != tc.wantSeq {
				t.Fatalf("parseSegSeq(%q) = %d, want %d", tc.in, seq, tc.wantSeq)
			}
		})
	}
}

// TestFindSegIdx covers the segment lookup, with particular attention to
// segments that hold no entries. An empty segment carries no index range, so
// probing one during the binary search yields no ordering information.
// Treating it as "search to the left" discards every segment to its right and
// makes live entries unreachable.
func TestFindSegIdx(t *testing.T) {
	seg := func(first, last raft.Index) *segment {
		return &segment{firstID: first, lastID: last}
	}
	empty := func() *segment { return &segment{} }

	tests := []struct {
		name  string
		segs  []*segment
		index raft.Index
		want  int
	}{
		{"no segments", nil, 1, -1},
		{"single segment, hit", []*segment{seg(1, 5)}, 3, 0},
		{"single segment, past the end", []*segment{seg(1, 5)}, 6, -1},
		{"single segment, before the start", []*segment{seg(5, 8)}, 4, -1},
		{"three segments, first", []*segment{seg(1, 4), seg(5, 8), seg(9, 12)}, 2, 0},
		{"three segments, middle", []*segment{seg(1, 4), seg(5, 8), seg(9, 12)}, 7, 1},
		{"three segments, last", []*segment{seg(1, 4), seg(5, 8), seg(9, 12)}, 10, 2},
		{"empty leading segment", []*segment{empty(), seg(5, 8)}, 6, 1},
		{"empty leading segment, deeper", []*segment{empty(), seg(5, 8), seg(9, 12)}, 11, 2},
		{"empty middle segment", []*segment{seg(1, 4), empty(), seg(9, 12)}, 11, 2},
		{"empty middle segment, hit to the left", []*segment{seg(1, 4), empty(), seg(9, 12)}, 2, 0},
		{"empty trailing segment", []*segment{seg(1, 4), seg(5, 8), empty()}, 6, 1},
		{"run of empty segments", []*segment{seg(1, 4), empty(), empty(), seg(9, 12)}, 10, 3},
		{"run of empty segments, gap index", []*segment{seg(1, 4), empty(), empty(), seg(9, 12)}, 6, -1},
		{"all empty", []*segment{empty(), empty()}, 3, -1},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fs := &FileStore{segs: tc.segs}
			if got := fs.findSegIdx(tc.index); got != tc.want {
				t.Fatalf("findSegIdx(%d) = %d, want %d", tc.index, got, tc.want)
			}
		})
	}
}

// TestDecodeLegacyMetaRecord checks that the pre-checksum hard-state layout is
// only accepted when the bytes really look like one, so that a damaged record
// in the current layout is never mistaken for an old one.
func TestDecodeLegacyMetaRecord(t *testing.T) {
	putLegacy := func(term uint64, id string) []byte {
		rec := make([]byte, legacyMetaSize)
		binary.LittleEndian.PutUint64(rec[0:8], term)
		binary.LittleEndian.PutUint16(rec[8:10], uint16(len(id)))
		copy(rec[10:], id)
		return rec
	}
	good := putLegacy(7, "peer-9")

	if hs, ok := decodeLegacyMetaRecord(good); !ok ||
		hs.CurrentTerm != 7 || hs.VotedFor != "peer-9" {
		t.Fatalf("decodeLegacyMetaRecord(good) = %+v, %v", hs, ok)
	}

	short := good[:len(good)-1]
	if _, ok := decodeLegacyMetaRecord(short); ok {
		t.Error("a record of the wrong length was accepted")
	}

	// The writer always copied the node ID into a zeroed buffer, so a non-zero
	// byte past the ID means these bytes are not a legacy record.
	dirty := putLegacy(7, "peer-9")
	dirty[len(dirty)-1] = 0xFF
	if _, ok := decodeLegacyMetaRecord(dirty); ok {
		t.Error("a record with non-zero padding was accepted")
	}

	tooLong := putLegacy(7, "peer-9")
	tooLong[8], tooLong[9] = 0xFF, 0xFF // votedForLen far past the field
	if _, ok := decodeLegacyMetaRecord(tooLong); ok {
		t.Error("a record with an out-of-range length was accepted")
	}
}

// TestMetaRecordRoundTrip checks the checksummed hard-state slot encoding,
// including that a single flipped bit anywhere in the record is rejected.
func TestMetaRecordRoundTrip(t *testing.T) {
	dir := t.TempDir()
	fs, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = fs.Close() }()

	want := raft.HardState{CurrentTerm: 11, VotedFor: "node-abc"}
	if err = fs.writeMeta(want); err != nil {
		t.Fatalf("writeMeta: %v", err)
	}

	rec := make([]byte, metaRecordSize)
	if _, err = fs.metaF.ReadAt(rec, 0); err != nil {
		t.Fatalf("read slot 0: %v", err)
	}
	got, seq, ok := decodeMetaRecord(rec)
	if !ok {
		t.Fatal("freshly written record failed verification")
	}
	if seq != 1 {
		t.Fatalf("sequence = %d, want 1", seq)
	}
	if got != want {
		t.Fatalf("decoded %+v, want %+v", got, want)
	}

	for _, pos := range []int{0, 4, 12, 22, metaRecordSize - 1} {
		damaged := make([]byte, metaRecordSize)
		copy(damaged, rec)
		damaged[pos] ^= 0x01
		if _, _, ok := decodeMetaRecord(damaged); ok {
			t.Errorf("a record with a flipped bit at byte %d was accepted", pos)
		}
	}
}
