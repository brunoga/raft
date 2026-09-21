package sharedwal

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"

	"github.com/brunoga/raft"
)

// ---- Record format ------------------------------------------------------------
//
//	[4 crc32c of everything after it][4 body length][8 group][1 kind][body]
//
// The CRC covers the length, group, kind and body, so a torn write of any
// part of a record fails the check.

const (
	recordHeaderSize = 4 + 4 + 8 + 1
	recordCRCSize    = 4

	kindEntries        byte = 1 // body: [4 count] then per entry [8 index][8 term][4 len][command]
	kindHardState      byte = 2 // body: [8 term][2 idLen][id]
	kindTruncateSuffix byte = 3 // body: [8 fromIndex]
	kindTruncatePrefix byte = 4 // body: [8 toIndex]
	kindSnapshot       byte = 5 // body: [8 index][8 term]
	kindCommitIndex    byte = 6 // body: [8 index]
	kindRemoveGroup    byte = 7 // body: empty

	entryHeaderSize = 8 + 8 + 4
)

var crcTable = crc32.MakeTable(crc32.Castagnoli)

// errBadRecord marks a record whose framing or checksum does not hold.
var errBadRecord = errors.New("sharedwal: bad record")

// appendRecord appends one framed record to buf and returns it.
func appendRecord(buf []byte, group uint64, kind byte, body []byte) []byte {
	start := len(buf)
	buf = append(buf, 0, 0, 0, 0) // crc, filled below
	buf = binary.LittleEndian.AppendUint32(buf, uint32(len(body)))
	buf = binary.LittleEndian.AppendUint64(buf, group)
	buf = append(buf, kind)
	buf = append(buf, body...)
	crc := crc32.Checksum(buf[start+recordCRCSize:], crcTable)
	binary.LittleEndian.PutUint32(buf[start:], crc)
	return buf
}

// recordSize returns the on-disk size of a record with a body of bodyLen.
func recordSize(bodyLen int) int64 {
	return int64(recordHeaderSize + bodyLen)
}

// parsedRecord is one record read back from a segment.
type parsedRecord struct {
	group uint64
	kind  byte
	body  []byte
	// off is where the record starts in its segment; size its full length.
	off, size int64
}

// parseRecord reads one record from data, which starts at a record boundary.
// It returns errBadRecord for a record that is truncated or fails its
// checksum, and the number of bytes consumed on success.
func parseRecord(data []byte) (rec parsedRecord, n int, err error) {
	if len(data) < recordHeaderSize {
		return parsedRecord{}, 0, errBadRecord
	}
	bodyLen := int(binary.LittleEndian.Uint32(data[4:8]))
	if bodyLen < 0 || len(data) < recordHeaderSize+bodyLen {
		return parsedRecord{}, 0, errBadRecord
	}
	total := recordHeaderSize + bodyLen
	want := binary.LittleEndian.Uint32(data[0:4])
	if crc32.Checksum(data[recordCRCSize:total], crcTable) != want {
		return parsedRecord{}, 0, errBadRecord
	}
	return parsedRecord{
		group: binary.LittleEndian.Uint64(data[8:16]),
		kind:  data[16],
		body:  data[recordHeaderSize:total],
		size:  int64(total),
	}, total, nil
}

// ---- Bodies -------------------------------------------------------------------

// encodeEntriesBody encodes a run of entries. offsets receives, for each
// entry, its offset within the body, which is what the in-memory index keeps
// so that one entry can be read back with a single positioned read.
func encodeEntriesBody(entries []raft.LogEntry) (body []byte, offsets []int64) {
	size := 4
	for i := range entries {
		size += entryHeaderSize + len(entries[i].Command)
	}
	body = make([]byte, 0, size)
	body = binary.LittleEndian.AppendUint32(body, uint32(len(entries)))
	offsets = make([]int64, len(entries))
	for i := range entries {
		offsets[i] = int64(len(body))
		body = binary.LittleEndian.AppendUint64(body, uint64(entries[i].Index))
		body = binary.LittleEndian.AppendUint64(body, uint64(entries[i].Term))
		body = binary.LittleEndian.AppendUint32(body, uint32(len(entries[i].Command)))
		body = append(body, entries[i].Command...)
	}
	return body, offsets
}

// decodeEntriesBody parses a run of entries, returning them with the offset
// of each within the body.
func decodeEntriesBody(body []byte) (entries []raft.LogEntry, offsets []int64, err error) {
	if len(body) < 4 {
		return nil, nil, errBadRecord
	}
	count := int(binary.LittleEndian.Uint32(body))
	off := 4
	entries = make([]raft.LogEntry, 0, count)
	offsets = make([]int64, 0, count)
	for range count {
		e, n, derr := decodeEntryAt(body, off)
		if derr != nil {
			return nil, nil, derr
		}
		entries = append(entries, e)
		offsets = append(offsets, int64(off))
		off += n
	}
	return entries, offsets, nil
}

// decodeEntryAt parses one entry at off within data and returns it with the
// number of bytes it occupies. The command is copied, never aliased.
func decodeEntryAt(data []byte, off int) (raft.LogEntry, int, error) {
	if len(data) < off+entryHeaderSize {
		return raft.LogEntry{}, 0, errBadRecord
	}
	index := binary.LittleEndian.Uint64(data[off:])
	term := binary.LittleEndian.Uint64(data[off+8:])
	cmdLen := int(binary.LittleEndian.Uint32(data[off+16:]))
	if cmdLen < 0 || len(data) < off+entryHeaderSize+cmdLen {
		return raft.LogEntry{}, 0, errBadRecord
	}
	var cmd []byte
	if cmdLen > 0 {
		cmd = make([]byte, cmdLen)
		copy(cmd, data[off+entryHeaderSize:off+entryHeaderSize+cmdLen])
	}
	return raft.LogEntry{Index: raft.Index(index), Term: raft.Term(term), Command: cmd},
		entryHeaderSize + cmdLen, nil
}

func encodeHardStateBody(hs raft.HardState) []byte {
	body := make([]byte, 0, 8+2+len(hs.VotedFor))
	body = binary.LittleEndian.AppendUint64(body, uint64(hs.CurrentTerm))
	body = binary.LittleEndian.AppendUint16(body, uint16(len(hs.VotedFor)))
	return append(body, hs.VotedFor...)
}

func decodeHardStateBody(body []byte) (raft.HardState, error) {
	if len(body) < 10 {
		return raft.HardState{}, errBadRecord
	}
	idLen := int(binary.LittleEndian.Uint16(body[8:]))
	if len(body) < 10+idLen {
		return raft.HardState{}, errBadRecord
	}
	return raft.HardState{
		CurrentTerm: raft.Term(binary.LittleEndian.Uint64(body)),
		VotedFor:    raft.NodeID(body[10 : 10+idLen]),
	}, nil
}

func encodeIndexBody(index raft.Index) []byte {
	return binary.LittleEndian.AppendUint64(nil, uint64(index))
}

func decodeIndexBody(body []byte) (raft.Index, error) {
	if len(body) < 8 {
		return 0, errBadRecord
	}
	return raft.Index(binary.LittleEndian.Uint64(body)), nil
}

func encodeSnapshotBody(meta raft.SnapshotMeta) []byte {
	body := binary.LittleEndian.AppendUint64(nil, uint64(meta.LastIncludedIndex))
	return binary.LittleEndian.AppendUint64(body, uint64(meta.LastIncludedTerm))
}

func decodeSnapshotBody(body []byte) (raft.SnapshotMeta, error) {
	if len(body) < 16 {
		return raft.SnapshotMeta{}, errBadRecord
	}
	return raft.SnapshotMeta{
		LastIncludedIndex: raft.Index(binary.LittleEndian.Uint64(body)),
		LastIncludedTerm:  raft.Term(binary.LittleEndian.Uint64(body[8:])),
	}, nil
}

// kindName names a record kind for error messages.
func kindName(k byte) string {
	switch k {
	case kindEntries:
		return "entries"
	case kindHardState:
		return "hardstate"
	case kindTruncateSuffix:
		return "truncate-suffix"
	case kindTruncatePrefix:
		return "truncate-prefix"
	case kindSnapshot:
		return "snapshot"
	case kindCommitIndex:
		return "commit-index"
	case kindRemoveGroup:
		return "remove-group"
	default:
		return fmt.Sprintf("kind-%d", k)
	}
}
