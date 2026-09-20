package raft

import (
	"context"
	"fmt"
)

// dedupNote is what the apply loop remembers about a queued ProposeOnce entry
// so that the client table can be updated once the entry has been applied.
// The zero value means the entry was not a deduplicated one.
type dedupNote struct {
	set      bool
	clientID NodeID
	seqNum   uint64
}

// applyPending hands a run of committed entries to the state machine and
// reports each result back to the event loop, in order.
//
// It returns false when the node is stopping and the caller should return.
//
// entries are what the state machine sees, which for a ProposeOnce command is
// the entry with its dedup header stripped. raw are the entries as they appear
// in the log, which is what the event loop needs in order to recognise them.
// The two differ only in the command bytes and are the same length.
func (n *Node) applyPending(
	ctx context.Context,
	entries, raw []LogEntry,
	notes []dedupNote,
	clients *clientLRU,
	lastApplied *Index,
) bool {
	outcomes, err := n.applyEntries(ctx, entries)
	if err != nil {
		// A batch that failed as a whole failed for every entry in it. Each
		// proposer is told, so none is left waiting on a command that will
		// never be answered.
		outcomes = make([]ApplyOutcome, len(entries))
		for i := range outcomes {
			outcomes[i].Err = err
		}
	}

	for i := range entries {
		out := outcomes[i]
		if out.Err == nil && notes[i].set {
			// Recorded here rather than before the call, so that a command the
			// state machine rejected is not remembered as having succeeded and
			// answered from the table on the retry.
			clients.put(notes[i].clientID, clientEntry{
				seqNum: notes[i].seqNum,
				result: out.Value,
			})
		}
		select {
		case n.applyResultCh <- applyResult{
			index: raw[i].Index,
			val:   out.Value,
			err:   out.Err,
			cmd:   raw[i].Command,
		}:
			*lastApplied = raw[i].Index
		case <-n.stopCh:
			return false
		}
	}
	return true
}

// applyEntries applies a run of entries, in one call when the state machine
// can take them that way and one at a time when it cannot.
//
// A state machine backed by storage almost always has a way to group work --
// one transaction, one write batch, one fsync -- and applying entries
// separately denies it that. The entries already arrive in runs: the apply
// loop is handed everything committed since it last looked, which under load
// is dozens at a time.
func (n *Node) applyEntries(ctx context.Context, entries []LogEntry) ([]ApplyOutcome, error) {
	if batcher, ok := n.cfg.StateMachine.(BatchApplier); ok {
		outcomes, err := batcher.ApplyBatch(ctx, entries)
		if err != nil {
			return nil, err
		}
		if len(outcomes) != len(entries) {
			// Guessing which result belongs to which entry would answer
			// proposers with other proposers' results, and record those
			// answers in the client table as though they were right. There is
			// no safe way to carry on.
			err := fmt.Errorf("raft: StateMachine.ApplyBatch returned %d outcomes for %d entries",
				len(outcomes), len(entries))
			n.fail(err, "apply batch")
			return nil, err
		}
		return outcomes, nil
	}

	outcomes := make([]ApplyOutcome, len(entries))
	for i := range entries {
		val, err := n.cfg.StateMachine.Apply(ctx, entries[i])
		outcomes[i] = ApplyOutcome{Value: val, Err: err}
	}
	return outcomes, nil
}
