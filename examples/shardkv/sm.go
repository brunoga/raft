package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/brunoga/raft"
)

// ErrKeyNotFound is returned by Apply when a "del" command targets an absent key.
var ErrKeyNotFound = errors.New("key not found")

// op identifies the kind of command written to the Raft log.
type op string

const (
	opSet op = "set"
	opDel op = "del"
)

// kvCmd is the command written to the Raft log for every mutating operation.
type kvCmd struct {
	Op    op     `json:"op"`
	Key   string `json:"key"`
	Value string `json:"value,omitempty"` // meaningful for opSet only
}

// KvSM is the deterministic state machine for one key-value shard.
//
// State is a flat map[string]string. Every instance is owned by exactly one
// Raft group; the Manager never shares a state machine across groups.
//
// Snapshot format: a JSON-encoded map[string]string.
type KvSM struct {
	mu   sync.RWMutex
	data map[string]string
}

// Apply processes a committed kvCmd and updates state accordingly.
//
// opSet: sets key to value (always succeeds).
// opDel: removes key; returns ErrKeyNotFound when the key is absent.
func (s *KvSM) Apply(_ context.Context, entry raft.LogEntry) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var cmd kvCmd
	if err := json.Unmarshal(entry.Command, &cmd); err != nil {
		return nil, fmt.Errorf("shardkv: decode cmd: %w", err)
	}

	switch cmd.Op {
	case opSet:
		s.data[cmd.Key] = cmd.Value
		return nil, nil

	case opDel:
		if _, ok := s.data[cmd.Key]; !ok {
			return nil, ErrKeyNotFound
		}
		delete(s.data, cmd.Key)
		return nil, nil

	default:
		return nil, fmt.Errorf("shardkv: unknown op %q", cmd.Op)
	}
}

// Snapshot serialises the data map as JSON.
func (s *KvSM) Snapshot(_ context.Context, w io.Writer) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return json.NewEncoder(w).Encode(s.data)
}

// Restore replaces the data map from a snapshot produced by Snapshot.
func (s *KvSM) Restore(_ context.Context, _ raft.SnapshotMeta, r io.Reader) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	var data map[string]string
	if err := json.NewDecoder(r).Decode(&data); err != nil {
		return fmt.Errorf("shardkv: restore: %w", err)
	}
	if data == nil {
		data = make(map[string]string)
	}
	s.data = data
	return nil
}

// Get returns the current value for key and true, or ("", false) when absent.
func (s *KvSM) Get(key string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.data[key]
	return v, ok
}
