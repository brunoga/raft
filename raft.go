package raft

import "fmt"

// Raft is the actual Raft algorithm implementation.
type Raft struct {
	id int32 // 2,147,483,647 should be anough for anybody.
}

// New creates a new Raft instance iwth the given id.
func New(id int32) *Raft {
	return &Raft{
		id,
	}
}

// Start starts the Raft instance.
func (r *Raft) Start() error {
	return fmt.Errorf("not implemented")
}

// Stop stops the Raft instance.
func (r *Raft) Stop() error {
	return fmt.Errorf("not implemented")
}
