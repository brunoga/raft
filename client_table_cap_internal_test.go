package raft

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"
)

// TestClientTableCapEntry_RoundTrip pins the encoding of the cap entry and
// that it is recognised as a config entry with its own opcode.
func TestClientTableCapEntry_RoundTrip(t *testing.T) {
	for _, capacity := range []int{0, 1, 100_000, 1 << 40} {
		cmd := encodeClientTableCapEntry(capacity)
		if !isConfigEntry(cmd) {
			t.Fatalf("cap entry for %d is not a config entry", capacity)
		}
		op, _, ok := decodeConfigEntry(cmd)
		if !ok || op != configOpClientTableCap {
			t.Fatalf("cap entry for %d decodes as op %#x ok=%v", capacity, op, ok)
		}
		got, ok := decodeClientTableCapEntry(cmd)
		if !ok || got != capacity {
			t.Fatalf("decodeClientTableCapEntry = (%d, %v), want (%d, true)", got, ok, capacity)
		}
	}
	if _, ok := decodeClientTableCapEntry(encodeConfigEntry(configOpAdd, PeerConfig{ID: "x"})); ok {
		t.Fatal("a membership entry decoded as a cap entry")
	}
	if _, ok := decodeClientTableCapEntry(encodeClientTableCapEntry(5)[:10]); ok {
		t.Fatal("a truncated cap entry decoded")
	}
}

// TestSnapshotFrame_CarriesTheCap pins the V3 framing round trip, and that
// V2 and V1 snapshots still read, reporting no cap.
func TestSnapshotFrame_CarriesTheCap(t *testing.T) {
	table := []clientRecord{{id: "a", ce: clientEntry{seqNum: 3, result: []byte("r")}}}
	ms := membershipState{members: []PeerConfig{{ID: "n1", Voter: true}}}

	var buf bytes.Buffer
	err := writeSnapshotFrame(&buf, &snapshotFrame{
		table: table, membership: ms, clientTableCap: 42, hasClientTableCap: true,
	}, func(w io.Writer) error { _, err := w.Write([]byte("SM")); return err })
	if err != nil {
		t.Fatal(err)
	}
	frame, smR, err := readSnapshotFrame(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if !frame.hasClientTableCap || frame.clientTableCap != 42 {
		t.Fatalf("cap = (%d, %v), want (42, true)", frame.clientTableCap, frame.hasClientTableCap)
	}
	if !frame.hasMembership || len(frame.membership.members) != 1 || len(frame.table) != 1 {
		t.Fatalf("membership/table lost: %+v", frame)
	}
	sm, _ := io.ReadAll(smR)
	if string(sm) != "SM" {
		t.Fatalf("sm data = %q", sm)
	}

	// A cap of zero is a recorded bound of "unlimited", not an absent one.
	buf.Reset()
	if err := writeSnapshotFrame(&buf, &snapshotFrame{hasClientTableCap: true},
		func(io.Writer) error { return nil }); err != nil {
		t.Fatal(err)
	}
	frame, _, err = readSnapshotFrame(&buf)
	if err != nil || !frame.hasClientTableCap || frame.clientTableCap != 0 {
		t.Fatalf("unlimited cap round trip: %+v %v", frame, err)
	}

	// The wrapper that records no cap still writes a readable frame.
	buf.Reset()
	if err := writeWrappedSnapshot(&buf, table, &ms, func(io.Writer) error { return nil }); err != nil {
		t.Fatal(err)
	}
	frame, _, err = readSnapshotFrame(&buf)
	if err != nil || frame.hasClientTableCap {
		t.Fatalf("frame without cap: %+v %v", frame, err)
	}

	// A V2 layout, as an older build wrote it.
	buf.Reset()
	tableBytes := encodeClientTable(table)
	msBytes := encodeMembership(&ms)
	var hdr [16]byte
	binary.LittleEndian.PutUint64(hdr[:8], snapFrameMagicV2)
	binary.LittleEndian.PutUint32(hdr[8:12], uint32(len(tableBytes)))
	binary.LittleEndian.PutUint32(hdr[12:], uint32(len(msBytes)))
	buf.Write(hdr[:])
	buf.Write(tableBytes)
	buf.Write(msBytes)
	buf.WriteString("SM")
	frame, smR, err = readSnapshotFrame(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if frame.hasClientTableCap || !frame.hasMembership || len(frame.table) != 1 {
		t.Fatalf("V2 frame: %+v", frame)
	}
	if sm, _ := io.ReadAll(smR); string(sm) != "SM" {
		t.Fatalf("V2 sm data = %q", sm)
	}
}

// TestClientLRU_SetCap pins that shrinking evicts from the tail, oldest
// first, and that growing or lifting the bound evicts nothing.
func TestClientLRU_SetCap(t *testing.T) {
	c := newClientLRU(0)
	for _, id := range []NodeID{"a", "b", "c", "d"} {
		c.put(id, clientEntry{seqNum: 1})
	}
	if ev := c.setCap(10); len(ev) != 0 || c.len() != 4 {
		t.Fatalf("setCap(10) evicted %v, len %d", ev, c.len())
	}
	ev := c.setCap(2)
	if len(ev) != 2 || ev[0] != "a" || ev[1] != "b" {
		t.Fatalf("setCap(2) evicted %v, want [a b] oldest first", ev)
	}
	if _, ok := c.get("c"); !ok {
		t.Fatal("c was evicted")
	}
	if ev := c.setCap(0); len(ev) != 0 || c.capacity() != 0 {
		t.Fatalf("setCap(0) evicted %v, cap %d", ev, c.capacity())
	}
	c.put("e", clientEntry{seqNum: 1})
	if c.len() != 3 {
		t.Fatalf("len %d after lifting the bound and adding one, want 3", c.len())
	}
}
