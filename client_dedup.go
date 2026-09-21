package raft

import (
	"bytes"
	"container/list"
	"encoding/binary"
	"fmt"
	"io"
)

// ---- LRU client table ---------------------------------------------------------

// clientLRU is an O(1) bounded table of per-client results, evicting the
// least-recently-UPDATED entry when it exceeds cap (0 = unlimited).
//
// Recency here means the order in which entries were written, never the order
// in which they were read. That distinction is a correctness requirement, not a
// preference: every replica builds this table by applying the same log entries
// in the same order, so as long as only writes reorder it, every replica evicts
// the same entry at the same point and they all agree on which requests they
// have already seen. If a lookup reordered the table, the leader — the only
// node that serves lookups — would drift from its followers, and a client retry
// would then be re-executed on some replicas and skipped on others, diverging
// their state machines with nothing to detect it.
type clientLRU struct {
	l   *list.List
	m   map[NodeID]*list.Element
	cap int // 0 means no eviction
}

type lruItem struct {
	id NodeID
	ce clientEntry
}

func newClientLRU(maxSize int) *clientLRU {
	return &clientLRU{
		l:   list.New(),
		m:   make(map[NodeID]*list.Element),
		cap: maxSize,
	}
}

// get returns the entry for id. It does not change eviction order; see the
// type comment for why that matters. O(1).
func (c *clientLRU) get(id NodeID) (clientEntry, bool) {
	e, ok := c.m[id]
	if !ok {
		return clientEntry{}, false
	}
	return e.Value.(*lruItem).ce, true
}

// len returns the number of entries currently held.
func (c *clientLRU) len() int { return c.l.Len() }

// put inserts or updates id and evicts the LRU entry if over cap. O(1).
//
// It returns the client that was dropped to make room, if any. That return is
// the only moment at which the exactly-once guarantee is lost for a client:
// from here on the table cannot tell that client's retry from a first attempt,
// and will run its command again. Nothing downstream can detect it afterwards,
// so a caller that has any way to report it should.
func (c *clientLRU) put(id NodeID, ce clientEntry) (evicted NodeID, didEvict bool) {
	if e, ok := c.m[id]; ok {
		e.Value.(*lruItem).ce = ce
		c.l.MoveToFront(e)
		return "", false
	}
	elem := c.l.PushFront(&lruItem{id: id, ce: ce})
	c.m[id] = elem
	if c.cap > 0 && c.l.Len() > c.cap {
		back := c.l.Back()
		if back != nil {
			item := c.l.Remove(back).(*lruItem)
			delete(c.m, item.id)
			return item.id, true
		}
	}
	return "", false
}

// setCap changes the bound and evicts from the tail until the table fits it.
// The evicted clients are returned oldest first, so a caller can report each
// one the way put's eviction is reported. A cap of 0 lifts the bound. O(k) in
// the number evicted.
func (c *clientLRU) setCap(capacity int) (evicted []NodeID) {
	c.cap = capacity
	for c.cap > 0 && c.l.Len() > c.cap {
		item := c.l.Remove(c.l.Back()).(*lruItem)
		delete(c.m, item.id)
		evicted = append(evicted, item.id)
	}
	return evicted
}

// capacity returns the bound, 0 meaning none.
func (c *clientLRU) capacity() int { return c.cap }

// clientRecord is one table entry in eviction order. The table is carried
// between goroutines and into snapshots as a slice rather than a map because
// the order is part of the state: rebuilt in a different order, two replicas
// would go on to evict different entries.
type clientRecord struct {
	id NodeID
	ce clientEntry
}

// records returns the table contents ordered most-recently-updated first. O(n).
func (c *clientLRU) records() []clientRecord {
	result := make([]clientRecord, 0, c.l.Len())
	for e := c.l.Front(); e != nil; e = e.Next() {
		item := e.Value.(*lruItem)
		result = append(result, clientRecord{id: item.id, ce: item.ce})
	}
	return result
}

// loadFrom replaces the table contents with records, which must be ordered
// most-recently-updated first. Entries beyond cap are dropped from the tail,
// which is where the oldest entries are. O(n).
func (c *clientLRU) loadFrom(records []clientRecord) {
	c.l = list.New()
	c.m = make(map[NodeID]*list.Element, len(records))
	for _, r := range records {
		if c.cap > 0 && c.l.Len() >= c.cap {
			break
		}
		if _, dup := c.m[r.id]; dup {
			continue
		}
		c.m[r.id] = c.l.PushBack(&lruItem{id: r.id, ce: r.ce})
	}
}

// dedupMagic is a 4-byte sentinel that marks a log entry as a
// ProposeOnce (client-dedup) command. The leading NUL byte matches the
// convention used by configMagic, making collision with ordinary application
// commands unlikely.
var dedupMagic = [4]byte{0x00, 0x52, 0x61, 0x44} // \x00RaD

// isDedupCmd reports whether cmd was encoded by encodeDedupCmd.
func isDedupCmd(cmd []byte) bool {
	return len(cmd) >= 4 &&
		cmd[0] == dedupMagic[0] &&
		cmd[1] == dedupMagic[1] &&
		cmd[2] == dedupMagic[2] &&
		cmd[3] == dedupMagic[3]
}

// encodeDedupCmd encodes a ProposeOnce command as:
//
//	[4-byte dedupMagic][2-byte clientIDLen][clientID][8-byte seqNum][payload]
func encodeDedupCmd(clientID NodeID, seqNum uint64, payload []byte) []byte {
	idBytes := []byte(clientID)
	buf := make([]byte, 4+2+len(idBytes)+8+len(payload))
	off := 0
	copy(buf[off:], dedupMagic[:])
	off += 4
	binary.LittleEndian.PutUint16(buf[off:], uint16(len(idBytes)))
	off += 2
	copy(buf[off:], idBytes)
	off += len(idBytes)
	binary.LittleEndian.PutUint64(buf[off:], seqNum)
	off += 8
	copy(buf[off:], payload)
	return buf
}

// decodeDedupCmd parses a dedup-encoded command. The caller must have verified
// isDedupCmd(cmd) first.
func decodeDedupCmd(cmd []byte) (clientID NodeID, seqNum uint64, payload []byte, err error) {
	// Minimum: 4 magic + 2 idLen + 0 id + 8 seqNum = 14 bytes
	if len(cmd) < 14 {
		return "", 0, nil, fmt.Errorf("dedup cmd too short: %d bytes", len(cmd))
	}
	off := 4 // skip magic
	idLen := int(binary.LittleEndian.Uint16(cmd[off:]))
	off += 2
	if off+idLen+8 > len(cmd) {
		return "", 0, nil, fmt.Errorf("dedup cmd truncated at clientID")
	}
	clientID = NodeID(cmd[off : off+idLen])
	off += idLen
	seqNum = binary.LittleEndian.Uint64(cmd[off:])
	off += 8
	payload = cmd[off:]
	return clientID, seqNum, payload, nil
}

// clientEntry records the latest sequence number seen from a given client
// and the result that was returned for it.
type clientEntry struct {
	seqNum uint64
	result []byte
}

// ---- Snapshot framing ---------------------------------------------------------
//
// A snapshot replaces the log prefix it covers, so everything that prefix
// carried has to be inside it. That is the state machine, the client dedup
// table, and the cluster membership agreed by the config entries the snapshot
// subsumes. The event loop wraps all three so they are restored atomically on
// restart or after an InstallSnapshot RPC.
//
// Wire format:
//
//	[8-byte snapFrameMagicV3][4-byte tableLen][4-byte membershipLen][4-byte limitsLen]
//	[table bytes][membership bytes][limits bytes][smData bytes]
//
// Table format:
//
//	[4-byte N] repeated N times: [2-byte idLen][id][8-byte seqNum][4-byte resLen][res]
//
// Limits format, when limitsLen > 0:
//
//	[8-byte client table cap]
//
// snapFrameMagicV1 is the original layout, which had no membership section,
// and snapFrameMagicV2 added it; V3 adds the limits section. Both older
// layouts are still read so that a node can start on a snapshot written by an
// older build. A V1 snapshot yields no membership and the node falls back to
// the peer list supplied in its Config; a V1 or V2 snapshot yields no table
// cap and the node keeps the one it has.
const (
	snapFrameMagicV1 uint64 = 0xCAFEDEAD_BEEFD00D
	snapFrameMagicV2 uint64 = 0xCAFEDEAD_BEEFD00E
	snapFrameMagicV3 uint64 = 0xCAFEDEAD_BEEFD00F
)

// snapshotFrame is everything a snapshot carries besides the state machine's
// own data: what the log prefix it replaces had established.
type snapshotFrame struct {
	table      []clientRecord
	membership membershipState
	// hasMembership is false for a snapshot written before the membership
	// section existed; the reader then keeps the membership it already has
	// rather than treating the zero value as an empty cluster.
	hasMembership bool
	// clientTableCap is the bound the client table was kept under at the
	// snapshot's index, and hasClientTableCap whether the snapshot recorded
	// one. 0 with hasClientTableCap true means unlimited.
	clientTableCap    int
	hasClientTableCap bool
}

// writeSnapshotFrame writes frame, followed by the state-machine data (via
// smSnapshot), to w.
func writeSnapshotFrame(w io.Writer, frame *snapshotFrame, smSnapshot func(io.Writer) error) error {
	tableBytes := encodeClientTable(frame.table)
	membershipBytes := encodeMembership(&frame.membership)
	var limitsBytes []byte
	if frame.hasClientTableCap {
		limitsBytes = binary.LittleEndian.AppendUint64(nil, uint64(frame.clientTableCap))
	}

	var hdr [20]byte
	binary.LittleEndian.PutUint64(hdr[:8], snapFrameMagicV3)
	binary.LittleEndian.PutUint32(hdr[8:12], uint32(len(tableBytes)))
	binary.LittleEndian.PutUint32(hdr[12:16], uint32(len(membershipBytes)))
	binary.LittleEndian.PutUint32(hdr[16:], uint32(len(limitsBytes)))

	if _, err := w.Write(hdr[:]); err != nil {
		return fmt.Errorf("write snap header: %w", err)
	}
	if _, err := w.Write(tableBytes); err != nil {
		return fmt.Errorf("write snap table: %w", err)
	}
	if _, err := w.Write(membershipBytes); err != nil {
		return fmt.Errorf("write snap membership: %w", err)
	}
	if _, err := w.Write(limitsBytes); err != nil {
		return fmt.Errorf("write snap limits: %w", err)
	}
	if err := smSnapshot(w); err != nil {
		return fmt.Errorf("write snap sm data: %w", err)
	}
	return nil
}

// writeWrappedSnapshot writes the client dedup table and the cluster
// membership, followed by the state-machine data (via smSnapshot), to w. It
// records no client table cap; see writeSnapshotFrame.
func writeWrappedSnapshot(w io.Writer, table []clientRecord, ms *membershipState, smSnapshot func(io.Writer) error) error {
	return writeSnapshotFrame(w, &snapshotFrame{table: table, membership: *ms}, smSnapshot)
}

// readSnapshotFrame reads the framing from r and returns it along with a
// reader positioned at the state-machine data.
func readSnapshotFrame(r io.Reader) (frame snapshotFrame, smDataReader io.Reader, err error) {
	var hdr [20]byte

	// The layouts share a leading magic and table length; V2 adds the
	// membership length and V3 the limits length, so read the common prefix
	// first.
	if _, readErr := io.ReadFull(r, hdr[:12]); readErr != nil {
		if readErr == io.EOF {
			return snapshotFrame{}, nil, fmt.Errorf("read snap header: empty file")
		}
		return snapshotFrame{}, nil, fmt.Errorf("read snap header: %w", readErr)
	}

	magic := binary.LittleEndian.Uint64(hdr[:8])
	if magic != snapFrameMagicV1 && magic != snapFrameMagicV2 && magic != snapFrameMagicV3 {
		// A snapshot from a different producer entirely: treat the whole
		// reader as state-machine data, putting back the bytes consumed.
		return snapshotFrame{}, io.MultiReader(bytes.NewReader(hdr[:12]), r), nil
	}

	membershipLen, limitsLen := 0, 0
	if magic == snapFrameMagicV2 || magic == snapFrameMagicV3 {
		if _, readErr := io.ReadFull(r, hdr[12:16]); readErr != nil {
			return snapshotFrame{}, nil, fmt.Errorf("read snap header: %w", readErr)
		}
		membershipLen = int(binary.LittleEndian.Uint32(hdr[12:16]))
	}
	if magic == snapFrameMagicV3 {
		if _, readErr := io.ReadFull(r, hdr[16:]); readErr != nil {
			return snapshotFrame{}, nil, fmt.Errorf("read snap header: %w", readErr)
		}
		limitsLen = int(binary.LittleEndian.Uint32(hdr[16:]))
	}

	tableLen := int(binary.LittleEndian.Uint32(hdr[8:12]))
	tableBytes := make([]byte, tableLen)
	if _, readErr := io.ReadFull(r, tableBytes); readErr != nil {
		return snapshotFrame{}, nil, fmt.Errorf("read snap table: %w", readErr)
	}
	frame.table, err = decodeClientTable(tableBytes)
	if err != nil {
		return snapshotFrame{}, nil, fmt.Errorf("decode snap table: %w", err)
	}

	if membershipLen > 0 {
		membershipBytes := make([]byte, membershipLen)
		if _, readErr := io.ReadFull(r, membershipBytes); readErr != nil {
			return snapshotFrame{}, nil, fmt.Errorf("read snap membership: %w", readErr)
		}
		decoded, ok := decodeMembership(membershipBytes)
		if !ok {
			return snapshotFrame{}, nil, fmt.Errorf("decode snap membership: malformed")
		}
		frame.membership, frame.hasMembership = decoded, true
	}

	if limitsLen > 0 {
		limitsBytes := make([]byte, limitsLen)
		if _, readErr := io.ReadFull(r, limitsBytes); readErr != nil {
			return snapshotFrame{}, nil, fmt.Errorf("read snap limits: %w", readErr)
		}
		if len(limitsBytes) < 8 {
			return snapshotFrame{}, nil, fmt.Errorf("decode snap limits: malformed")
		}
		v := binary.LittleEndian.Uint64(limitsBytes)
		if v > uint64(int(^uint(0)>>1)) {
			return snapshotFrame{}, nil, fmt.Errorf("decode snap limits: client table cap out of range")
		}
		frame.clientTableCap, frame.hasClientTableCap = int(v), true
	}

	return frame, r, nil
}

// readWrappedSnapshot reads the client table and membership from r and returns
// them along with a reader positioned at the state-machine data. It is
// readSnapshotFrame for a caller that does not need the limits section.
func readWrappedSnapshot(r io.Reader) (table []clientRecord, ms membershipState, hasMembership bool, smDataReader io.Reader, err error) {
	frame, smDataReader, err := readSnapshotFrame(r)
	if err != nil {
		return nil, membershipState{}, false, nil, err
	}
	return frame.table, frame.membership, frame.hasMembership, smDataReader, nil
}

func encodeClientTable(table []clientRecord) []byte {
	// Pre-compute size.
	size := 4 // N
	for _, r := range table {
		size += 2 + len(r.id) + 8 + 4 + len(r.ce.result)
	}
	buf := make([]byte, size)
	off := 0
	binary.LittleEndian.PutUint32(buf[off:], uint32(len(table)))
	off += 4
	for _, rec := range table {
		id, e := rec.id, rec.ce
		idBytes := []byte(id)
		binary.LittleEndian.PutUint16(buf[off:], uint16(len(idBytes)))
		off += 2
		copy(buf[off:], idBytes)
		off += len(idBytes)
		binary.LittleEndian.PutUint64(buf[off:], e.seqNum)
		off += 8
		binary.LittleEndian.PutUint32(buf[off:], uint32(len(e.result)))
		off += 4
		copy(buf[off:], e.result)
		off += len(e.result)
	}
	return buf[:off]
}

func decodeClientTable(buf []byte) ([]clientRecord, error) {
	if len(buf) < 4 {
		return nil, fmt.Errorf("client table: buf too short")
	}
	n := int(binary.LittleEndian.Uint32(buf))
	buf = buf[4:]
	table := make([]clientRecord, 0, min(n, 4096))
	for i := range n {
		if len(buf) < 2 {
			return nil, fmt.Errorf("client table: entry %d: truncated idLen", i)
		}
		idLen := int(binary.LittleEndian.Uint16(buf))
		buf = buf[2:]
		if len(buf) < idLen+12 {
			return nil, fmt.Errorf("client table: entry %d: truncated id+seq+resLen", i)
		}
		// NodeID is a string type, so this conversion copies; the bytes are not
		// aliased to buf.
		id := NodeID(buf[:idLen])
		buf = buf[idLen:]
		seqNum := binary.LittleEndian.Uint64(buf)
		buf = buf[8:]
		resLen := int(binary.LittleEndian.Uint32(buf))
		buf = buf[4:]
		if len(buf) < resLen {
			return nil, fmt.Errorf("client table: entry %d: truncated result", i)
		}
		var result []byte
		if resLen > 0 {
			result = make([]byte, resLen)
			copy(result, buf[:resLen])
		}
		buf = buf[resLen:]
		table = append(table, clientRecord{id: id, ce: clientEntry{seqNum: seqNum, result: result}})
	}
	return table, nil
}
