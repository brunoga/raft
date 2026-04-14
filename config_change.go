package raft

import "encoding/binary"

// configOpAdd and configOpRemove are the one-byte opcodes encoded inside a
// cluster-membership log entry.
const (
	configOpAdd      byte = 0x01
	configOpRemove   byte = 0x02
	configOpJoint    byte = 0x03 // joint-consensus: C_old ∪ C_new
	configOpFinalise byte = 0x04 // finalise: commit C_new only
)

// configMagic is a 4-byte sentinel that marks a log entry as a Raft
// configuration change rather than a user command. The leading NUL byte makes
// collision with normal application commands unlikely.
var configMagic = [4]byte{0x00, 0x52, 0x61, 0x43} // \x00RaC

// isConfigEntry reports whether cmd encodes a cluster-membership change.
func isConfigEntry(cmd []byte) bool {
	return len(cmd) >= 5 &&
		cmd[0] == configMagic[0] &&
		cmd[1] == configMagic[1] &&
		cmd[2] == configMagic[2] &&
		cmd[3] == configMagic[3]
}

// encodeConfigEntry serialises a membership-change operation into a log-entry
// command byte slice.
func encodeConfigEntry(op byte, id NodeID) []byte {
	b := make([]byte, 5+len(id))
	copy(b[:4], configMagic[:])
	b[4] = op
	copy(b[5:], id)
	return b
}

// decodeConfigEntry parses a configuration command. ok is false if cmd is not
// a valid configuration entry.
func decodeConfigEntry(cmd []byte) (op byte, id NodeID, ok bool) {
	if !isConfigEntry(cmd) {
		return 0, "", false
	}
	return cmd[4], NodeID(cmd[5:]), true
}

// ---- Joint consensus encoding ----------------------------------------------
//
// Wire format for configOpJoint (opcode 0x03):
//
//	[4 magic][0x03][4-byte N_old][N_old × (2-byte len + NodeID bytes)]
//	                             [4-byte N_new][N_new × (2-byte len + NodeID bytes)]
//
// Wire format for configOpFinalise (opcode 0x04):
//
//	[4 magic][0x04][4-byte N][N × (2-byte len + NodeID bytes)]

// encodeJointConfigEntry encodes a joint-consensus entry carrying both the
// old membership list (oldPeers) and the desired new membership (newPeers).
// oldPeers must not include the local node's own ID. newPeers may include
// the local node's own ID when the leader is retained in the new cluster;
// omitting it signals that the leader should remove itself.
func encodeJointConfigEntry(oldPeers, newPeers []NodeID) []byte {
	return encodePeerLists(configOpJoint, oldPeers, newPeers)
}

// decodeJointConfigEntry parses a joint-consensus entry.
func decodeJointConfigEntry(cmd []byte) (old, new_ []NodeID, ok bool) {
	if !isConfigEntry(cmd) || cmd[4] != configOpJoint {
		return nil, nil, false
	}
	old, rest, ok := decodePeerList(cmd[5:])
	if !ok {
		return nil, nil, false
	}
	new_, _, ok = decodePeerList(rest)
	if !ok {
		return nil, nil, false
	}
	return old, new_, true
}

// encodeFinaliseConfigEntry encodes a finalise entry that commits C_new as the
// sole cluster membership.
func encodeFinaliseConfigEntry(newPeers []NodeID) []byte {
	return encodePeerLists(configOpFinalise, newPeers)
}

// decodeFinaliseConfigEntry parses a finalise entry.
func decodeFinaliseConfigEntry(cmd []byte) (members []NodeID, ok bool) {
	if !isConfigEntry(cmd) || cmd[4] != configOpFinalise {
		return nil, false
	}
	members, _, ok = decodePeerList(cmd[5:])
	return members, ok
}

// encodePeerLists encodes [magic][op] followed by one or more peer lists.
func encodePeerLists(op byte, lists ...[]NodeID) []byte {
	// Calculate total size.
	size := 5 // 4-byte magic + 1-byte opcode
	for _, list := range lists {
		size += 4 // 4-byte count
		for _, id := range list {
			size += 2 + len(id) // 2-byte length + bytes
		}
	}
	b := make([]byte, size)
	copy(b[:4], configMagic[:])
	b[4] = op
	off := 5
	for _, list := range lists {
		binary.BigEndian.PutUint32(b[off:], uint32(len(list)))
		off += 4
		for _, id := range list {
			binary.BigEndian.PutUint16(b[off:], uint16(len(id)))
			off += 2
			copy(b[off:], id)
			off += len(id)
		}
	}
	return b
}

// decodePeerList reads a length-prefixed list of NodeIDs from buf and returns
// the list plus the remaining bytes.
func decodePeerList(buf []byte) (peers []NodeID, rest []byte, ok bool) {
	if len(buf) < 4 {
		return nil, nil, false
	}
	n := int(binary.BigEndian.Uint32(buf))
	buf = buf[4:]
	peers = make([]NodeID, 0, n)
	for range n {
		if len(buf) < 2 {
			return nil, nil, false
		}
		idLen := int(binary.BigEndian.Uint16(buf))
		buf = buf[2:]
		if len(buf) < idLen {
			return nil, nil, false
		}
		peers = append(peers, NodeID(buf[:idLen]))
		buf = buf[idLen:]
	}
	return peers, buf, true
}
