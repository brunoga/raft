package raft

import (
	"container/list"
	"encoding/binary"
	"fmt"
)

// ---- LRU client table ---------------------------------------------------------

// clientLRU is an O(1) LRU cache for the client dedup table.
// The front of the list is the most-recently-used entry; the back is evicted
// when the table exceeds cap (0 = unlimited).
type clientLRU struct {
	l   *list.List
	m   map[NodeID]*list.Element
	cap int // 0 means no eviction
}

type lruItem struct {
	id NodeID
	ce clientEntry
}

func newClientLRU(cap int) *clientLRU {
	return &clientLRU{
		l:   list.New(),
		m:   make(map[NodeID]*list.Element),
		cap: cap,
	}
}

// get returns the entry for id and moves it to MRU position. O(1).
func (c *clientLRU) get(id NodeID) (clientEntry, bool) {
	e, ok := c.m[id]
	if !ok {
		return clientEntry{}, false
	}
	c.l.MoveToFront(e)
	return e.Value.(*lruItem).ce, true
}

// put inserts or updates id and evicts the LRU entry if over cap. O(1).
func (c *clientLRU) put(id NodeID, ce clientEntry) {
	if e, ok := c.m[id]; ok {
		e.Value.(*lruItem).ce = ce
		c.l.MoveToFront(e)
		return
	}
	elem := c.l.PushFront(&lruItem{id: id, ce: ce})
	c.m[id] = elem
	if c.cap > 0 && c.l.Len() > c.cap {
		back := c.l.Back()
		if back != nil {
			item := c.l.Remove(back).(*lruItem)
			delete(c.m, item.id)
		}
	}
}

// toMap returns a plain map snapshot of the current table. O(n).
func (c *clientLRU) toMap() map[NodeID]clientEntry {
	result := make(map[NodeID]clientEntry, c.l.Len())
	for e := c.l.Front(); e != nil; e = e.Next() {
		item := e.Value.(*lruItem)
		result[item.id] = item.ce
	}
	return result
}

// loadFrom replaces the LRU contents with the entries from m. O(n).
func (c *clientLRU) loadFrom(m map[NodeID]clientEntry) {
	c.l = list.New()
	c.m = make(map[NodeID]*list.Element, len(m))
	for id, ce := range m {
		elem := c.l.PushBack(&lruItem{id: id, ce: ce})
		c.m[id] = elem
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
	copy(buf[off:], dedupMagic[:]); off += 4
	binary.LittleEndian.PutUint16(buf[off:], uint16(len(idBytes))); off += 2
	copy(buf[off:], idBytes); off += len(idBytes)
	binary.LittleEndian.PutUint64(buf[off:], seqNum); off += 8
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
	idLen := int(binary.LittleEndian.Uint16(cmd[off:])); off += 2
	if off+idLen+8 > len(cmd) {
		return "", 0, nil, fmt.Errorf("dedup cmd truncated at clientID")
	}
	clientID = NodeID(cmd[off : off+idLen]); off += idLen
	seqNum = binary.LittleEndian.Uint64(cmd[off:]); off += 8
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
// When taking a snapshot, the event loop wraps the state-machine data with the
// client dedup table so that both can be restored atomically on restart or after
// an InstallSnapshot RPC.
//
// Wire format:
//   [8-byte snapFrameMagic][4-byte tableLen][table bytes][smData bytes]
//
// Table format:
//   [4-byte N] repeated N times: [2-byte idLen][id][8-byte seqNum][4-byte resLen][res]

const snapFrameMagic uint64 = 0xCAFEDEAD_BEEFD00D

// wrapSnapshot prepends the client dedup table to smData.
func wrapSnapshot(table map[NodeID]clientEntry, smData []byte) []byte {
	tableBytes := encodeClientTable(table)
	buf := make([]byte, 8+4+len(tableBytes)+len(smData))
	off := 0
	binary.LittleEndian.PutUint64(buf[off:], snapFrameMagic); off += 8
	binary.LittleEndian.PutUint32(buf[off:], uint32(len(tableBytes))); off += 4
	copy(buf[off:], tableBytes); off += len(tableBytes)
	copy(buf[off:], smData)
	return buf
}

// unwrapSnapshot extracts the client table and SM data from a wrapped snapshot.
// If the data is not in the wrapped format (e.g. an older snapshot), it returns
// an empty table and the full data slice so the SM restore still succeeds.
func unwrapSnapshot(data []byte) (map[NodeID]clientEntry, []byte) {
	empty := make(map[NodeID]clientEntry)
	if len(data) < 12 {
		return empty, data
	}
	if binary.LittleEndian.Uint64(data) != snapFrameMagic {
		return empty, data
	}
	tableLen := int(binary.LittleEndian.Uint32(data[8:]))
	start := 12
	if start+tableLen > len(data) {
		// The framing header is present but the table is truncated — corrupt
		// snapshot. Return the bytes after the header (skipping magic+tableLen)
		// so the SM at least doesn't receive the raw framing bytes as its state.
		// Both outcomes are garbage; skipping the header avoids misinterpreting
		// the magic constant as a length or opcode in the SM protocol.
		return empty, data[start:]
	}
	table, err := decodeClientTable(data[start : start+tableLen])
	if err != nil {
		return empty, data[start+tableLen:]
	}
	return table, data[start+tableLen:]
}

func encodeClientTable(table map[NodeID]clientEntry) []byte {
	// Pre-compute size.
	size := 4 // N
	for id, e := range table {
		size += 2 + len(id) + 8 + 4 + len(e.result)
	}
	buf := make([]byte, size)
	off := 0
	binary.LittleEndian.PutUint32(buf[off:], uint32(len(table))); off += 4
	for id, e := range table {
		idBytes := []byte(id)
		binary.LittleEndian.PutUint16(buf[off:], uint16(len(idBytes))); off += 2
		copy(buf[off:], idBytes); off += len(idBytes)
		binary.LittleEndian.PutUint64(buf[off:], e.seqNum); off += 8
		binary.LittleEndian.PutUint32(buf[off:], uint32(len(e.result))); off += 4
		copy(buf[off:], e.result); off += len(e.result)
	}
	return buf[:off]
}

func decodeClientTable(buf []byte) (map[NodeID]clientEntry, error) {
	if len(buf) < 4 {
		return nil, fmt.Errorf("client table: buf too short")
	}
	n := int(binary.LittleEndian.Uint32(buf)); buf = buf[4:]
	table := make(map[NodeID]clientEntry, n)
	for i := range n {
		if len(buf) < 2 {
			return nil, fmt.Errorf("client table: entry %d: truncated idLen", i)
		}
		idLen := int(binary.LittleEndian.Uint16(buf)); buf = buf[2:]
		if len(buf) < idLen+12 {
			return nil, fmt.Errorf("client table: entry %d: truncated id+seq+resLen", i)
		}
		id := NodeID(make([]byte, idLen))
		copy([]byte(id), buf[:idLen]); buf = buf[idLen:]
		seqNum := binary.LittleEndian.Uint64(buf); buf = buf[8:]
		resLen := int(binary.LittleEndian.Uint32(buf)); buf = buf[4:]
		if len(buf) < resLen {
			return nil, fmt.Errorf("client table: entry %d: truncated result", i)
		}
		var result []byte
		if resLen > 0 {
			result = make([]byte, resLen)
			copy(result, buf[:resLen])
		}
		buf = buf[resLen:]
		table[id] = clientEntry{seqNum: seqNum, result: result}
	}
	return table, nil
}
