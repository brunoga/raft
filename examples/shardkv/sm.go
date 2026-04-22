package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/brunoga/flexspace-go/flexdb"
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

// snapshotEntry is one record in a KvSM snapshot (newline-delimited JSON).
type snapshotEntry struct {
	K string `json:"k"`
	V string `json:"v"`
}

const (
	kvTableName = "kv"
	kvCacheCapMB = 64 // per-shard flexdb cache capacity
)

// KvSM is the deterministic state machine for one key-value shard.
//
// State is persisted to disk via flexdb. Reads and writes go directly to the
// database; the Raft log provides durability and replication guarantees, while
// flexdb provides persistence beyond the lifetime of a single process.
//
// Snapshot format: newline-delimited JSON, one {"k":"...","v":"..."} object per
// line. Using a streaming format avoids loading the entire shard into memory
// during both Snapshot and Restore.
type KvSM struct {
	db    *flexdb.DB
	table *flexdb.Table
}

// NewKvSM opens (or creates) a flexdb database at path and returns a KvSM
// backed by it. The caller must call Close when the state machine is no longer
// needed.
func NewKvSM(path string) (*KvSM, error) {
	db, err := flexdb.Open(path, kvCacheCapMB)
	if err != nil {
		return nil, fmt.Errorf("shardkv: open kvdb at %s: %w", path, err)
	}
	table, err := db.Table(kvTableName)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("shardkv: open kv table: %w", err)
	}
	return &KvSM{db: db, table: table}, nil
}

// Close flushes pending writes and releases all database resources.
func (s *KvSM) Close() error {
	return s.db.Close()
}

// Apply processes a committed kvCmd and updates the persistent store.
//
// opSet: sets key to value (always succeeds).
// opDel: removes key; returns ErrKeyNotFound when the key is absent.
func (s *KvSM) Apply(_ context.Context, entry raft.LogEntry) ([]byte, error) {
	var cmd kvCmd
	if err := json.Unmarshal(entry.Command, &cmd); err != nil {
		return nil, fmt.Errorf("shardkv: decode cmd: %w", err)
	}

	ref := s.table.NewRef()

	switch cmd.Op {
	case opSet:
		if err := ref.Put([]byte(cmd.Key), []byte(cmd.Value)); err != nil {
			return nil, fmt.Errorf("shardkv: put %q: %w", cmd.Key, err)
		}
		return nil, nil

	case opDel:
		ok, err := ref.Probe([]byte(cmd.Key))
		if err != nil {
			return nil, fmt.Errorf("shardkv: probe %q: %w", cmd.Key, err)
		}
		if !ok {
			return nil, ErrKeyNotFound
		}
		if err := ref.Delete([]byte(cmd.Key)); err != nil {
			return nil, fmt.Errorf("shardkv: delete %q: %w", cmd.Key, err)
		}
		return nil, nil

	default:
		return nil, fmt.Errorf("shardkv: unknown op %q", cmd.Op)
	}
}

// Snapshot streams the entire shard state as newline-delimited JSON.
// Each line is a {"k":"...","v":"..."} object. The format is streaming so
// large shards do not require the entire dataset to be held in memory at once.
func (s *KvSM) Snapshot(_ context.Context, w io.Writer) error {
	ref := s.table.NewRef()
	it := ref.NewIterator()
	defer it.Close()

	enc := json.NewEncoder(w)
	for it.Seek(nil); it.Valid(); it.Next() {
		kv := it.Current()
		if err := enc.Encode(snapshotEntry{K: string(kv.Key), V: string(kv.Value)}); err != nil {
			return fmt.Errorf("shardkv: snapshot encode: %w", err)
		}
	}
	return it.Err()
}

// Restore replaces the entire shard state from a snapshot produced by Snapshot.
// The existing table is dropped (O(1) regardless of size) and a fresh one is
// populated from the snapshot stream.
func (s *KvSM) Restore(_ context.Context, _ raft.SnapshotMeta, r io.Reader) error {
	// Drop the existing table (O(1) in data size) and open a fresh one.
	if err := s.db.DropTable(kvTableName); err != nil {
		return fmt.Errorf("shardkv: drop table: %w", err)
	}
	table, err := s.db.Table(kvTableName)
	if err != nil {
		return fmt.Errorf("shardkv: recreate table: %w", err)
	}
	s.table = table

	// Ingest all entries from the snapshot stream.
	// Buffer size matches the 1 MiB HTTP body limit for values.
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 2<<20), 2<<20)

	ref := s.table.NewRef()
	for scanner.Scan() {
		var e snapshotEntry
		if err := json.Unmarshal(scanner.Bytes(), &e); err != nil {
			return fmt.Errorf("shardkv: restore decode: %w", err)
		}
		if err := ref.Put([]byte(e.K), []byte(e.V)); err != nil {
			return fmt.Errorf("shardkv: restore put %q: %w", e.K, err)
		}
	}
	return scanner.Err()
}

// Get returns the current value for key and true, or ("", false) when absent.
// Safe to call concurrently with Apply.
func (s *KvSM) Get(key string) (string, bool) {
	ref := s.table.NewRef()
	v, err := ref.Get([]byte(key))
	if err != nil || v == nil {
		return "", false
	}
	return string(v), true
}
