package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"slices"

	"github.com/brunoga/raft"
)

// command is the payload of a log entry.
type command struct {
	Op    string `json:"op"` // "put" or "delete"
	Key   string `json:"key"`
	Value string `json:"value,omitempty"`
}

// The store implements four of the library's interfaces. Only the first is
// required; the other three are what a state machine with its own durable
// storage can offer, and each is detected by a type assertion at construction.
var (
	_ raft.StateMachine        = (*kvStore)(nil)
	_ raft.DurableStateMachine = (*kvStore)(nil)
	_ raft.BatchApplier        = (*kvStore)(nil)
	_ raft.SnapshotCapturer    = (*kvStore)(nil)
)

// Apply implements raft.StateMachine.
//
// Reached only when the engine has a single entry to apply; anything more
// arrives through ApplyBatch. Both end in the same place, because the
// difference that matters is how many records share a sync, not how the call
// arrived.
func (s *kvStore) Apply(_ context.Context, entry raft.LogEntry) ([]byte, error) {
	rec, skip, err := decodeCommand(entry)
	if err != nil || skip {
		return nil, err
	}
	if err := s.commit([]record{rec}); err != nil {
		return nil, err
	}
	return nil, nil
}

// ApplyBatch implements raft.BatchApplier.
//
// Every entry in the batch is written and synced together. A command that does
// not decode is reported as that entry's own outcome rather than failing the
// batch: one malformed payload is the proposer's problem, and failing the batch
// would make it everyone's.
func (s *kvStore) ApplyBatch(_ context.Context, entries []raft.LogEntry) ([]raft.ApplyOutcome, error) {
	outcomes := make([]raft.ApplyOutcome, len(entries))
	recs := make([]record, 0, len(entries))

	for i, entry := range entries {
		rec, skip, err := decodeCommand(entry)
		if err != nil {
			outcomes[i] = raft.ApplyOutcome{Err: err}
			continue
		}
		if skip {
			continue
		}
		recs = append(recs, rec)
	}

	// One write, one sync, however many entries.
	if err := s.commit(recs); err != nil {
		return nil, err
	}
	return outcomes, nil
}

// decodeCommand turns a log entry into a record, reporting skip for entries
// that carry no command of ours.
//
// A newly elected leader appends an entry with an empty payload, to commit
// anything left over from earlier terms, and that entry reaches the state
// machine like any other. It is not an error and it is not a write: a state
// machine that treats it as either will reject something the engine produced
// for its own purposes, on every election.
func decodeCommand(entry raft.LogEntry) (rec record, skip bool, err error) {
	if len(entry.Command) == 0 {
		return record{}, true, nil
	}

	var cmd command
	if uerr := json.Unmarshal(entry.Command, &cmd); uerr != nil {
		return record{}, false, fmt.Errorf("durablekv: decode command: %w", uerr)
	}
	switch cmd.Op {
	case "put":
		return record{index: uint64(entry.Index), op: opPut, key: cmd.Key, value: cmd.Value}, false, nil
	case "delete":
		return record{index: uint64(entry.Index), op: opDel, key: cmd.Key}, false, nil
	default:
		return record{}, false, fmt.Errorf("durablekv: unknown op %q", cmd.Op)
	}
}

// AppliedIndex implements raft.DurableStateMachine.
//
// This is the whole point of the example. The engine calls it once while the
// node is being built and replays nothing at or below what it returns, so a
// restart costs a scan of this store's own file rather than a snapshot restore
// plus every entry since.
//
// It is safe to answer because every record carries the index that produced it
// and is written in the same sync: the value below cannot be ahead of the data
// it describes.
func (s *kvStore) AppliedIndex(_ context.Context) (raft.Index, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return raft.Index(s.applied), nil
}

// Capture implements raft.SnapshotCapturer.
//
// Called on the apply goroutine, so it does the cheap thing -- copy the map --
// and leaves serialising to Write, which runs on a background goroutine while
// entries keep applying. Without this, every proposal made during a snapshot
// waits for the whole state to be encoded.
//
// Copying is cheap here because the state is small enough to hold in memory. A
// store over a real engine would take that engine's own snapshot instead; the
// shape of the interface is the same.
func (s *kvStore) Capture(_ context.Context) (raft.Snapshot, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return &kvSnapshot{data: maps.Clone(s.data)}, nil
}

// Snapshot implements raft.StateMachine.
//
// The fallback for an engine that does not use Capture. It holds the read lock
// for the whole serialisation, which is the pause Capture exists to avoid.
func (s *kvStore) Snapshot(ctx context.Context, w io.Writer) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return writeSnapshot(ctx, w, s.data)
}

// Restore implements raft.StateMachine.
//
// The state is replaced wholesale, so the file is rewritten rather than
// appended to -- and the applied index it reports afterwards has to be the
// snapshot's, because that is what the new state is the effect of. Every record
// written here carries it, so the store comes back from a crash mid-restore
// either as it was or as the snapshot, never as a mixture reporting an index
// belonging to neither.
func (s *kvStore) Restore(_ context.Context, meta raft.SnapshotMeta, r io.Reader) error {
	var data map[string]string
	if err := json.NewDecoder(r).Decode(&data); err != nil {
		return fmt.Errorf("durablekv: decode snapshot: %w", err)
	}
	if data == nil {
		data = make(map[string]string)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.data = data
	s.applied = uint64(meta.LastIncludedIndex)
	return s.rewriteLocked()
}

// kvSnapshot is a captured state, serialised off the apply path.
type kvSnapshot struct {
	data map[string]string
}

func (c *kvSnapshot) Write(_ context.Context, w io.Writer) error {
	return writeSnapshot(context.Background(), w, c.data)
}

// Release drops the captured copy. Nothing else holds it, so letting it go is
// all there is to do; a store over a real engine would close its snapshot here.
func (c *kvSnapshot) Release() { c.data = nil }

// writeSnapshot encodes state in a stable order, so that two snapshots of
// identical state are identical bytes.
func writeSnapshot(_ context.Context, w io.Writer, data map[string]string) error {
	enc := json.NewEncoder(w)
	ordered := make(map[string]string, len(data))
	for _, k := range slices.Sorted(maps.Keys(data)) {
		ordered[k] = data[k]
	}
	if err := enc.Encode(ordered); err != nil {
		return fmt.Errorf("durablekv: encode snapshot: %w", err)
	}
	return nil
}
