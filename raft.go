package raft

import (
	"fmt"
	"sync"
)

// Raft is the actual Raft algorithm implementation. There is usually a single
// Raft instance per node (i.e. service instance).
type Raft struct {
	id int32 // 2,147,483,647 should be anough for anybody.

	m     sync.Mutex
	state State  // The current state of the Raft instance.
	term  uint64 // The current term known to this instance.
}

// New creates a new Raft instance iwth the given id.
func New(id int32) *Raft {
	return &Raft{
		id:    id,
		m:     sync.Mutex{},
		state: StateFollower,
		term:  0,
	}
}

// Start starts the Raft instance.
func (r *Raft) Start() error {
	r.m.Lock()
	defer r.m.Unlock()

	if r.state != StateStopped {
		return fmt.Errorf("raft: instance %d is already started", r.id)
	}

	r.state = StateFollower

	// TODO(@brunoga): Start the Raft instance loop.

	return nil
}

// Stop stops the Raft instance.
func (r *Raft) Stop() error {
	r.m.Lock()
	defer r.m.Unlock()

	if r.state == StateStopped {
		return fmt.Errorf("raft: instance %d is already stopped", r.id)
	}

	r.state = StateStopped
	r.term = 0

	return nil
}
