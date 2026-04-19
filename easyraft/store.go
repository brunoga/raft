package easyraft

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/brunoga/raft"
)

var (
	ErrKeyNotFound = errors.New("easyraft: key not found")
	ErrKeyExists   = errors.New("easyraft: key already exists")
	ErrNotLeader   = errors.New("easyraft: not leader")
)

type opType string

const (
	opCreate opType = "create"
	opUpdate opType = "update"
	opDelete opType = "delete"
	opMutate opType = "mutate"
)

type command struct {
	Op         opType          `json:"op"`
	Collection string          `json:"collection"`
	Key        string          `json:"key"`
	Value      json.RawMessage `json:"value,omitempty"`
	MutateName string          `json:"mutate_name,omitempty"`
	MutateArgs json.RawMessage `json:"mutate_args,omitempty"`
}

type Store struct {
	mu           sync.RWMutex
	collections  map[string]map[string]json.RawMessage
	mutations    map[string]map[string]MutationFunc
	node         *raft.Node
	waitForApply time.Duration
}

// MutationFunc receives the raw current value and arbitrary raw arguments,
// and returns the raw new value, an optional raw response, and an error.
// The new value will be saved back into the state machine.
// If an error is returned, the state machine is NOT updated.
type MutationFunc func(currentValue json.RawMessage, args json.RawMessage) (newValue json.RawMessage, response json.RawMessage, err error)

func newStore() *Store {
	return &Store{
		collections:  make(map[string]map[string]json.RawMessage),
		mutations:    make(map[string]map[string]MutationFunc),
		waitForApply: 5 * time.Second,
	}
}

func (s *Store) registerMutation(collection, name string, fn MutationFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mutations[collection] == nil {
		s.mutations[collection] = make(map[string]MutationFunc)
	}
	s.mutations[collection][name] = fn
}

func (s *Store) Apply(_ context.Context, entry raft.LogEntry) ([]byte, error) {
	var cmd command
	if err := json.Unmarshal(entry.Command, &cmd); err != nil {
		return nil, fmt.Errorf("easyraft: decode cmd: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	coll, exists := s.collections[cmd.Collection]
	if !exists {
		coll = make(map[string]json.RawMessage)
		s.collections[cmd.Collection] = coll
	}

	switch cmd.Op {
	case opCreate:
		if _, ok := coll[cmd.Key]; ok {
			return nil, ErrKeyExists
		}
		coll[cmd.Key] = cmd.Value
		return nil, nil

	case opUpdate:
		if _, ok := coll[cmd.Key]; !ok {
			return nil, ErrKeyNotFound
		}
		coll[cmd.Key] = cmd.Value
		return nil, nil

	case opDelete:
		if _, ok := coll[cmd.Key]; !ok {
			return nil, ErrKeyNotFound
		}
		delete(coll, cmd.Key)
		return nil, nil

	case opMutate:
		currentVal, ok := coll[cmd.Key]
		if !ok {
			return nil, ErrKeyNotFound
		}

		collMutations := s.mutations[cmd.Collection]
		if collMutations == nil {
			return nil, fmt.Errorf("easyraft: unknown mutation collection %q", cmd.Collection)
		}
		fn, ok := collMutations[cmd.MutateName]
		if !ok {
			return nil, fmt.Errorf("easyraft: unknown mutation %q in collection %q", cmd.MutateName, cmd.Collection)
		}

		newVal, resp, err := fn(currentVal, cmd.MutateArgs)
		if err != nil {
			return nil, err
		}
		coll[cmd.Key] = newVal
		return resp, nil

	default:
		return nil, fmt.Errorf("easyraft: unknown op %q", cmd.Op)
	}
}

func (s *Store) Snapshot(_ context.Context, w io.Writer) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return json.NewEncoder(w).Encode(s.collections)
}

func (s *Store) Restore(_ context.Context, _ raft.SnapshotMeta, r io.Reader) error {
	var collections map[string]map[string]json.RawMessage
	if err := json.NewDecoder(r).Decode(&collections); err != nil {
		return fmt.Errorf("easyraft: restore decode: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if collections == nil {
		collections = make(map[string]map[string]json.RawMessage)
	}
	s.collections = collections
	return nil
}

// setNode connects the internal state machine back to its driving Raft node.
func (s *Store) setNode(n *raft.Node) {
	s.node = n
}

// propose encodes a command and proposes it to the Raft node.
func (s *Store) propose(ctx context.Context, cmd *command) ([]byte, error) {
	if s.node == nil {
		return nil, errors.New("easyraft: node not started")
	}

	b, err := json.Marshal(cmd)
	if err != nil {
		return nil, fmt.Errorf("easyraft: encode cmd: %w", err)
	}

	res, err := s.node.Propose(ctx, b)
	if err != nil {
		if errors.Is(err, raft.ErrNotLeader) {
			return nil, ErrNotLeader
		}
		return nil, err
	}
	return res, nil
}
