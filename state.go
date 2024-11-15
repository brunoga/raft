package raft

// State represents the state of a Raft instance.
type State uint8

const (
	// StateStopped is the state of a Raft instance when it is stopped. This is
	// the initial state of a Raft instance.
	StateStopped State = iota
	// StateFollower is the state of a Raft instance when it is a follower. This
	// can be because it just started or because there is already a leader.
	StateFollower
	// StateCandidate is the state of a Raft instance when it is a candidate. This
	// means the instance is offering itself to become a leader.
	StateCandidate
	// StateLeader is the state of a Raft instance when it is a leader. There is
	// only one leader in given election term.
	StateLeader
	// StateCount is the number of states in the Raft algorithm.
	StateCount
)

// String returns the string representation of the state.
func (s State) String() string {
	switch s {
	case StateStopped:
		return "Stopped"
	case StateFollower:
		return "Follower"
	case StateCandidate:
		return "Candidate"
	case StateLeader:
		return "Leader"
	default:
		return "Invalid"
	}
}

// IsValid returns true if the state is valid.
func (s State) IsValid() bool {
	return s < StateCount
}
