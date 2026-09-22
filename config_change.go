package raft

import "encoding/binary"

// configOpAdd and configOpRemove are the one-byte opcodes encoded inside a
// cluster-membership log entry.
const (
	configOpAdd      byte = 0x01
	configOpRemove   byte = 0x02
	configOpJoint    byte = 0x03 // joint-consensus: C_old ∪ C_new
	configOpFinalise byte = 0x04 // finalise: commit C_new only
	// configOpClientTableCap sets the size of the exactly-once client table
	// for the whole group. It is a config entry rather than a user command
	// because, like membership, it is state every replica must agree on: a
	// replica with a different bound evicts different clients and diverges.
	// It changes no membership.
	configOpClientTableCap byte = 0x05
	// configOpCommitQuorum sets how many voters an entry must reach to
	// commit, for the whole group; the election quorum follows from it. Like
	// membership it is group state, since a node that counted differently
	// could elect a leader without the entries another node had committed.
	configOpCommitQuorum byte = 0x06
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
func encodeConfigEntry(op byte, peer PeerConfig) []byte {
	b := make([]byte, 6+len(peer.ID))
	copy(b[:4], configMagic[:])
	b[4] = op
	if peer.Voter {
		b[5] = 1
	} else {
		b[5] = 0
	}
	copy(b[6:], peer.ID)
	return b
}

// decodeConfigEntry parses a configuration command. ok is false if cmd is not
// a valid configuration entry.
func decodeConfigEntry(cmd []byte) (op byte, peer PeerConfig, ok bool) {
	if !isConfigEntry(cmd) || len(cmd) < 6 {
		return 0, PeerConfig{}, false
	}
	voter := cmd[5] == 1
	return cmd[4], PeerConfig{ID: NodeID(cmd[6:]), Voter: voter}, true
}

// ---- Joint consensus encoding ----------------------------------------------
//
// Wire format for configOpJoint (opcode 0x03):
//
//	[4 magic][0x03][4-byte N_old][N_old × (2-byte len + 1-byte voter + NodeID bytes)]
//	                             [4-byte N_new][N_new × (2-byte len + 1-byte voter + NodeID bytes)]
//
// Wire format for configOpFinalise (opcode 0x04):
//
//	[4 magic][0x04][4-byte N][N × (2-byte len + 1-byte voter + NodeID bytes)]

// encodeJointConfigEntry encodes a joint-consensus entry carrying both the
// old membership list (oldPeers) and the desired new membership (newPeers).
// oldPeers must not include the local node's own ID. newPeers may include
// the local node's own ID when the leader is retained in the new cluster;
// omitting it signals that the leader should remove itself.
func encodeJointConfigEntry(oldPeers, newPeers []PeerConfig) []byte {
	return encodePeerLists(configOpJoint, oldPeers, newPeers)
}

// decodeJointConfigEntry parses a joint-consensus entry.
func decodeJointConfigEntry(cmd []byte) (old, new_ []PeerConfig, ok bool) {
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
func encodeFinaliseConfigEntry(newPeers []PeerConfig) []byte {
	return encodePeerLists(configOpFinalise, newPeers)
}

// decodeFinaliseConfigEntry parses a finalise entry.
func decodeFinaliseConfigEntry(cmd []byte) (members []PeerConfig, ok bool) {
	if !isConfigEntry(cmd) || cmd[4] != configOpFinalise {
		return nil, false
	}
	members, _, ok = decodePeerList(cmd[5:])
	return members, ok
}

// ---- Commit quorum encoding -------------------------------------------------
//
// Wire format for configOpCommitQuorum (opcode 0x06):
//
//	[4 magic][0x06][8-byte quorum, big endian]
//
// A quorum of 0 means a simple majority.

// encodeCommitQuorumEntry encodes a config entry that sets the group's commit
// quorum.
func encodeCommitQuorumEntry(quorum int) []byte {
	b := make([]byte, 5, 13)
	copy(b[:4], configMagic[:])
	b[4] = configOpCommitQuorum
	return binary.BigEndian.AppendUint64(b, uint64(quorum))
}

// decodeCommitQuorumEntry parses a commit quorum entry. ok is false if cmd is
// not one.
func decodeCommitQuorumEntry(cmd []byte) (quorum int, ok bool) {
	if !isConfigEntry(cmd) || cmd[4] != configOpCommitQuorum || len(cmd) < 13 {
		return 0, false
	}
	v := binary.BigEndian.Uint64(cmd[5:13])
	if v > uint64(int(^uint(0)>>1)) {
		return 0, false
	}
	return int(v), true
}

// ---- Membership snapshot encoding ------------------------------------------
//
// The membership in effect at a snapshot's last-included index is stored inside
// the snapshot, so that a node restarting from that snapshot recovers the
// cluster it belongs to rather than falling back to the peer list its operator
// happened to pass to New.
//
// Wire format:
//
//	[1-byte kind] 0 = simple, 1 = joint
//	simple: [peer list]                  — every member, including self
//	joint:  [peer list][peer list]       — C_old then C_new, each including self
//	then, optionally: [8-byte commit quorum]
//
// The commit quorum trails the lists so that a reader from before it existed,
// which stops after the lists, still reads the membership; it then takes the
// quorum to be a majority, which is what the absence of the field means.
const (
	membershipKindSimple byte = 0
	membershipKindJoint  byte = 1
)

// membershipState is the cluster membership in effect at a point in the log.
// Every list includes the local node: unlike Config.Peers, this representation
// is independent of which node holds it, which is what makes it safe to store
// in a snapshot and to compare across nodes.
type membershipState struct {
	// joint reports whether a joint-consensus reconfiguration is in progress.
	// While it is, a decision needs a majority of both old and new.
	joint bool

	// members is the membership when joint is false.
	members []PeerConfig

	// old and new are the two configurations when joint is true.
	old, new []PeerConfig

	// commitQuorum is how many voters an entry must reach to commit, 0
	// meaning a majority. See quorum.go.
	commitQuorum int
}

// encodeMembership serialises a membershipState.
func encodeMembership(ms *membershipState) []byte {
	var b []byte
	if ms.joint {
		b = []byte{membershipKindJoint}
		b = appendPeerList(b, ms.old)
		b = appendPeerList(b, ms.new)
	} else {
		b = appendPeerList([]byte{membershipKindSimple}, ms.members)
	}
	return binary.BigEndian.AppendUint64(b, uint64(ms.commitQuorum))
}

// decodeMembership parses a membershipState. ok is false if buf is malformed.
func decodeMembership(buf []byte) (ms membershipState, ok bool) {
	if len(buf) < 1 {
		return membershipState{}, false
	}
	var rest []byte
	switch buf[0] {
	case membershipKindSimple:
		members, r, ok := decodePeerList(buf[1:])
		if !ok {
			return membershipState{}, false
		}
		ms, rest = membershipState{members: members}, r
	case membershipKindJoint:
		old, r, ok := decodePeerList(buf[1:])
		if !ok {
			return membershipState{}, false
		}
		new_, r, ok := decodePeerList(r)
		if !ok {
			return membershipState{}, false
		}
		ms, rest = membershipState{joint: true, old: old, new: new_}, r
	default:
		return membershipState{}, false
	}
	if len(rest) >= 8 {
		v := binary.BigEndian.Uint64(rest)
		if v > uint64(int(^uint(0)>>1)) {
			return membershipState{}, false
		}
		ms.commitQuorum = int(v)
	}
	return ms, true
}

// encodePeerLists encodes [magic][op] followed by one or more peer lists.
func encodePeerLists(op byte, lists ...[]PeerConfig) []byte {
	// Calculate total size.
	size := 5 // 4-byte magic + 1-byte opcode
	for _, list := range lists {
		size += 4 // 4-byte count
		for _, p := range list {
			size += 3 + len(p.ID) // 2-byte length + 1-byte voter + bytes
		}
	}
	b := make([]byte, 5, size)
	copy(b[:4], configMagic[:])
	b[4] = op
	for _, list := range lists {
		b = appendPeerList(b, list)
	}
	return b
}

// appendPeerList appends a length-prefixed peer list to b and returns it.
func appendPeerList(b []byte, list []PeerConfig) []byte {
	b = binary.BigEndian.AppendUint32(b, uint32(len(list)))
	for _, p := range list {
		b = binary.BigEndian.AppendUint16(b, uint16(len(p.ID)))
		if p.Voter {
			b = append(b, 1)
		} else {
			b = append(b, 0)
		}
		b = append(b, p.ID...)
	}
	return b
}

// decodePeerList reads a length-prefixed list of PeerConfigs from buf and
// returns the list plus the remaining bytes.
func decodePeerList(buf []byte) (peers []PeerConfig, rest []byte, ok bool) {
	if len(buf) < 4 {
		return nil, nil, false
	}
	n := int(binary.BigEndian.Uint32(buf))
	buf = buf[4:]
	peers = make([]PeerConfig, 0, n)
	for range n {
		if len(buf) < 3 {
			return nil, nil, false
		}
		idLen := int(binary.BigEndian.Uint16(buf))
		buf = buf[2:]
		voter := buf[0] == 1
		buf = buf[1:]
		if len(buf) < idLen {
			return nil, nil, false
		}
		peers = append(peers, PeerConfig{ID: NodeID(buf[:idLen]), Voter: voter})
		buf = buf[idLen:]
	}
	return peers, buf, true
}

// ---- Client table cap encoding ----------------------------------------------
//
// Wire format for configOpClientTableCap (opcode 0x05):
//
//	[4 magic][0x05][8-byte cap, big endian]
//
// A cap of 0 means unlimited, as it does in Config.MaxClientTableSize.

// encodeClientTableCapEntry encodes a config entry that sets the client table
// cap for the group.
func encodeClientTableCapEntry(capacity int) []byte {
	b := make([]byte, 5, 13)
	copy(b[:4], configMagic[:])
	b[4] = configOpClientTableCap
	return binary.BigEndian.AppendUint64(b, uint64(capacity))
}

// decodeClientTableCapEntry parses a client table cap entry. ok is false if
// cmd is not one.
func decodeClientTableCapEntry(cmd []byte) (capacity int, ok bool) {
	if !isConfigEntry(cmd) || cmd[4] != configOpClientTableCap || len(cmd) < 13 {
		return 0, false
	}
	v := binary.BigEndian.Uint64(cmd[5:13])
	if v > uint64(int(^uint(0)>>1)) {
		return 0, false
	}
	return int(v), true
}
