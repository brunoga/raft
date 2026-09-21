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
//	[8-byte snapFrameMagicV2][4-byte tableLen][4-byte membershipLen]
//	[table bytes][membership bytes][smData bytes]
//
// Table format:
//
//	[4-byte N] repeated N times: [2-byte idLen][id][8-byte seqNum][4-byte resLen][res]
//
// snapFrameMagicV1 is the original layout, which had no membership section. It
// is still read so that a node can start on a snapshot written by an older
// build; such a snapshot yields no membership and the node falls back to the
// peer list supplied in its Config.
const (
	snapFrameMagicV1 uint64 = 0xCAFEDEAD_BEEFD00D
	snapFrameMagicV2 uint64 = 0xCAFEDEAD_BEEFD00E
)

// writeWrappedSnapshot writes the client dedup table and the cluster
// membership, followed by the state-machine data (via smSnapshot), to w.
func writeWrappedSnapshot(w io.Writer, table []clientRecord, ms *membershipState, smSnapshot func(io.Writer) error) error {
	tableBytes := encodeClientTable(table)
	membershipBytes := encodeMembership(ms)

	var hdr [16]byte
	binary.LittleEndian.PutUint64(hdr[:8], snapFrameMagicV2)
	binary.LittleEndian.PutUint32(hdr[8:12], uint32(len(tableBytes)))
	binary.LittleEndian.PutUint32(hdr[12:], uint32(len(membershipBytes)))

	if _, err := w.Write(hdr[:]); err != nil {
		return fmt.Errorf("write snap header: %w", err)
	}
	if _, err := w.Write(tableBytes); err != nil {
		return fmt.Errorf("write snap table: %w", err)
	}
	if _, err := w.Write(membershipBytes); err != nil {
		return fmt.Errorf("write snap membership: %w", err)
	}
	if err := smSnapshot(w); err != nil {
		return fmt.Errorf("write snap sm data: %w", err)
	}
	return nil
}

// readWrappedSnapshot reads the client table and membership from r and returns
// them along with a reader positioned at the state-machine data.
//
// hasMembership is false for a snapshot written before the membership section
// existed; the caller must then keep whatever membership it already has rather
// than treating the zero value as an empty cluster.
func readWrappedSnapshot(r io.Reader) (table []clientRecord, ms membershipState, hasMembership bool, smDataReader io.Reader, err error) {
	var hdr [16]byte

	// The two layouts share a leading magic and table length; only V2 has the
	// membership length, so read the common prefix first.
	if _, readErr := io.ReadFull(r, hdr[:12]); readErr != nil {
		if readErr == io.EOF {
			return nil, membershipState{}, false, nil, fmt.Errorf("read snap header: empty file")
		}
		return nil, membershipState{}, false, nil, fmt.Errorf("read snap header: %w", readErr)
	}

	magic := binary.LittleEndian.Uint64(hdr[:8])
	if magic != snapFrameMagicV1 && magic != snapFrameMagicV2 {
		// A snapshot from a different producer entirely: treat the whole
		// reader as state-machine data, putting back the bytes consumed.
		return nil, membershipState{}, false,
			io.MultiReader(bytes.NewReader(hdr[:12]), r), nil
	}

	membershipLen := 0
	if magic == snapFrameMagicV2 {
		if _, readErr := io.ReadFull(r, hdr[12:]); readErr != nil {
			return nil, membershipState{}, false, nil, fmt.Errorf("read snap header: %w", readErr)
		}
		membershipLen = int(binary.LittleEndian.Uint32(hdr[12:]))
	}

	tableLen := int(binary.LittleEndian.Uint32(hdr[8:12]))
	tableBytes := make([]byte, tableLen)
	if _, readErr := io.ReadFull(r, tableBytes); readErr != nil {
		return nil, membershipState{}, false, nil, fmt.Errorf("read snap table: %w", readErr)
	}
	table, err = decodeClientTable(tableBytes)
	if err != nil {
		return nil, membershipState{}, false, nil, fmt.Errorf("decode snap table: %w", err)
	}

	if membershipLen > 0 {
		membershipBytes := make([]byte, membershipLen)
		if _, readErr := io.ReadFull(r, membershipBytes); readErr != nil {
			return nil, membershipState{}, false, nil, fmt.Errorf("read snap membership: %w", readErr)
		}
		decoded, ok := decodeMembership(membershipBytes)
		if !ok {
			return nil, membershipState{}, false, nil, fmt.Errorf("decode snap membership: malformed")
		}
		ms, hasMembership = decoded, true
	}

	return table, ms, hasMembership, r, nil
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
