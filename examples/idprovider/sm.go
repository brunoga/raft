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

// ErrDomainNotFound is returned by Apply when the target domain does not exist.
var ErrDomainNotFound = errors.New("domain not found")

// op identifies the kind of command written to the Raft log.
type op string

const (
	opCreate op = "create"
	opDelete op = "delete"
	opAlloc  op = "alloc"
)

// domainCmd is the command written to the Raft log for every mutating operation.
type domainCmd struct {
	Op     op     `json:"op"`
	Domain string `json:"domain"`
	Count  uint64 `json:"count,omitempty"` // only meaningful for opAlloc
}

// allocResult is the Apply return value for opAlloc operations.
// It is serialised as JSON and returned directly to the HTTP client.
type allocResult struct {
	Start uint64 `json:"start"`
	Count uint64 `json:"count"`
}

// IDSM is the deterministic state machine for the multi-domain ID provider.
//
// State is a map from domain name to a monotonically increasing counter.
// Each committed opAlloc advances the domain counter by Count and the caller
// receives the exclusive range [start, start+count).
//
// Snapshot format: JSON-encoded map[string]uint64.
type IDSM struct {
	mu      sync.RWMutex
	domains map[string]uint64
}

// Apply processes a committed domainCmd and updates state accordingly.
//
// opCreate: creates the domain if it does not exist (idempotent; never errors).
// opDelete: removes the domain; returns ErrDomainNotFound if absent.
// opAlloc:  reserves [counter+1, counter+Count) in the domain; returns
//
//	ErrDomainNotFound if the domain does not exist.
func (s *IDSM) Apply(_ context.Context, entry raft.LogEntry) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var cmd domainCmd
	if err := json.Unmarshal(entry.Command, &cmd); err != nil {
		return nil, fmt.Errorf("idprovider: decode cmd: %w", err)
	}
	if cmd.Domain == "" {
		return nil, fmt.Errorf("idprovider: empty domain name")
	}

	switch cmd.Op {
	case opCreate:
		if _, exists := s.domains[cmd.Domain]; !exists {
			s.domains[cmd.Domain] = 0
		}
		return json.Marshal(struct{}{})

	case opDelete:
		if _, exists := s.domains[cmd.Domain]; !exists {
			return nil, ErrDomainNotFound
		}
		delete(s.domains, cmd.Domain)
		return json.Marshal(struct{}{})

	case opAlloc:
		counter, exists := s.domains[cmd.Domain]
		if !exists {
			return nil, ErrDomainNotFound
		}
		if cmd.Count == 0 {
			return nil, fmt.Errorf("idprovider: count must be > 0")
		}
		start := counter + 1
		s.domains[cmd.Domain] = counter + cmd.Count
		return json.Marshal(allocResult{Start: start, Count: cmd.Count})

	default:
		return nil, fmt.Errorf("idprovider: unknown op %q", cmd.Op)
	}
}

// Snapshot serialises the domain map as JSON.
func (s *IDSM) Snapshot(_ context.Context, w io.Writer) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return json.NewEncoder(w).Encode(s.domains)
}

// Restore replaces the domain map from a snapshot produced by Snapshot.
func (s *IDSM) Restore(_ context.Context, _ raft.SnapshotMeta, r io.Reader) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	var domains map[string]uint64
	if err := json.NewDecoder(r).Decode(&domains); err != nil {
		return fmt.Errorf("idprovider: restore: %w", err)
	}
	if domains == nil {
		domains = make(map[string]uint64)
	}
	s.domains = domains
	return nil
}

// Current returns the current counter value for the named domain.
// ok is false when the domain does not exist.
func (s *IDSM) Current(domain string) (uint64, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.domains[domain]
	return v, ok
}

// Domains returns a snapshot of all domain names and their current counters.
// The returned map is a copy; the caller may modify it freely.
func (s *IDSM) Domains() map[string]uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]uint64, len(s.domains))
	for k, v := range s.domains {
		out[k] = v
	}
	return out
}
