package raft

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// This file holds the goroutine that owns every mutating call into Storage.
//
// Raft's event loop must never wait on a disk. A node that blocks in fsync
// stops counting election ticks, stops answering heartbeats and stops reading
// its own inbound queue, so a storage backend having a slow moment is
// indistinguishable from the node being down: followers time out and call an
// election against a leader that is alive and healthy apart from one pending
// write. The cure is to move the write off the loop, which is what this file
// does, and then to hold back only the things whose meaning depends on the
// write having landed, which is what the deferred continuations in node.go do.
//
// Everything here exists to make that safe, and safety rests on one property:
// operations reach Storage in exactly the order the event loop issued them.
// A conflicting suffix truncated and then overwritten is two operations whose
// order is the whole point, and a queue that reordered them would leave a log
// that silently keeps entries the cluster abandoned. One goroutine draining
// one FIFO gives that ordering for free.

// writeKind identifies what a queued storage operation does. It also names the
// operation in [StorageMetrics] reports.
type writeKind uint8

const (
	writeAppend writeKind = iota
	writeTruncateSuffix
	writeTruncatePrefix
	writeHardState
)

func (k writeKind) String() string {
	switch k {
	case writeAppend:
		return "append"
	case writeTruncateSuffix:
		return "truncate_suffix"
	case writeTruncatePrefix:
		return "truncate_prefix"
	case writeHardState:
		return "hardstate"
	default:
		return "unknown"
	}
}

// writeOp is one unit of durable work handed to the writer.
//
// Only the fields the kind uses are set; the rest are zero.
type writeOp struct {
	// seq orders operations and identifies this one in completions. It is
	// assigned by the raftLog, increases by one per operation, and never
	// restarts.
	seq  uint64
	kind writeKind

	entries []LogEntry // writeAppend
	index   Index      // writeTruncateSuffix, writeTruncatePrefix
	hs      HardState  // writeHardState

	// durableAfter is the highest log index that is on stable storage once
	// this operation has completed, given that every operation queued before
	// it also completed. It is computed when the operation is queued, which is
	// the only moment at which the log's own bookkeeping and the queue agree
	// about what the log looks like.
	durableAfter Index
}

// writeDone reports one completed operation back to the event loop.
type writeDone struct {
	seq          uint64
	kind         writeKind
	durableAfter Index
	err          error
	took         time.Duration
}

// storageWriter serialises every mutating Storage call onto one goroutine.
//
// Neither side can block the other. The event loop hands work over by
// appending to a queue it does not wait on, and the writer hands completions
// back by appending to a list it does not wait on, waking the other side with
// a one-slot channel in each direction. Two bounded channels would eventually
// deadlock against each other under sustained load: the loop waiting for room
// in the work queue while the writer waits for room in the completion queue.
// Memory is bounded instead where the growth actually is, by the limit on
// unacknowledged log entries in raftLog.
type storageWriter struct {
	storage Storage
	// batch is storage again when it can write the hard state and a run of
	// entries as one durable operation, and nil when it cannot. See
	// BatchWriter.
	batch BatchWriter

	// maxBatchEntries caps how many entries a single coalesced append may
	// carry. See take.
	maxBatchEntries int

	mu sync.Mutex
	// started reports whether the writer goroutine is running. It is started
	// on the first operation queued rather than at construction, so a Node
	// that is built and never used starts no goroutine.
	started bool
	// queue holds operations that have not been executed yet, oldest first.
	queue []writeOp
	// done holds completions the event loop has not collected yet.
	done []writeDone
	// executing reports whether the writer is inside a storage call right now.
	// Only tests read it, through busy, to tell "the queue is empty" from
	// "there is nothing left to wait for".
	executing bool
	// failed is the first error any operation returned. Once set, no further
	// operation is executed: after a failed append or truncation the contents
	// of the log are unknown, and continuing to write to it would turn a
	// reported failure into a corrupted log.
	failed error
	closed bool

	wake   chan struct{} // writer ← event loop: work is waiting
	notify chan struct{} // writer → event loop: completions are waiting
	exited chan struct{} // closed when the writer goroutine has returned
}

// defaultWriteBatchEntries caps a coalesced append. It is large enough that
// the cap is never the limiting factor for a normal proposal batch and small
// enough that one batch cannot be arbitrarily large in memory.
const defaultWriteBatchEntries = 4096

func newStorageWriter(s Storage) *storageWriter {
	batch, _ := s.(BatchWriter)
	return &storageWriter{
		storage:         s,
		batch:           batch,
		maxBatchEntries: defaultWriteBatchEntries,
		wake:            make(chan struct{}, 1),
		notify:          make(chan struct{}, 1),
		exited:          make(chan struct{}),
	}
}

// completions returns the channel the event loop selects on to learn that
// operations have finished. It carries no value: the loop calls takeDone.
func (w *storageWriter) completions() <-chan struct{} { return w.notify }

// enqueue hands one operation to the writer. It never blocks. The operation is
// copied into the queue, so the caller's copy is its own afterwards.
//
// A closed writer drops it. That only happens once the node is stopping, when
// nothing is left to acknowledge on its behalf.
func (w *storageWriter) enqueue(op *writeOp) {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return
	}
	w.queue = append(w.queue, *op)
	start := !w.started
	w.started = true
	w.mu.Unlock()

	if start {
		go w.loop()
	}

	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// takeDone collects every completion reported since the last call, in the
// order the operations were queued.
func (w *storageWriter) takeDone() []writeDone {
	w.mu.Lock()
	defer w.mu.Unlock()
	done := w.done
	w.done = nil
	return done
}

// busy reports whether any operation is queued or running.
func (w *storageWriter) busy() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.queue) > 0 || w.executing
}

// close stops the writer once it has finished everything already queued, and
// waits for it to return.
//
// Draining rather than abandoning is what makes a graceful stop graceful. A
// node that is asked to shut down has entries in memory that it accepted and
// has not written; abandoning them would be correct -- nothing was
// acknowledged on their behalf, so it is the crash this design is already
// safe under -- but it would mean an orderly restart routinely threw away the
// tail of the log and had to fetch it again from the leader. When storage is
// failing the drain is immediate: the first error stops any further storage
// call, and the rest of the queue is failed without touching the disk.
func (w *storageWriter) close() {
	w.mu.Lock()
	alreadyClosed := w.closed
	w.closed = true
	running := w.started
	// Claim the goroutine slot so that an enqueue racing with this call
	// cannot start a writer nobody is waiting for.
	w.started = true
	w.mu.Unlock()
	if alreadyClosed {
		<-w.exited
		return
	}

	if !running {
		// Nothing was ever queued, so there is no goroutine to wait for.
		close(w.exited)
		return
	}

	select {
	case w.wake <- struct{}{}:
	default:
	}
	<-w.exited
}

// loop is the writer goroutine.
func (w *storageWriter) loop() {
	defer close(w.exited)
	for {
		batch, stop := w.take()
		if len(batch) > 0 {
			w.execute(batch)
		}
		if stop {
			return
		}
		if len(batch) > 0 {
			continue // there may be more waiting
		}
		<-w.wake
	}
}

// take removes the next run of operations to execute, and reports whether the
// writer should stop afterwards.
//
// A run is either a single non-append operation or a maximal run of appends.
// Coalescing appends is what makes moving the write off the event loop worth
// doing rather than merely safe: consecutive appends are contiguous by
// construction, so a run of them is one AppendLogEntries call and therefore
// one fsync, however many proposals arrived while the previous write was in
// flight. Under load the queue absorbs the disk's latency and the number of
// fsyncs per second stops tracking the number of proposals per second.
func (w *storageWriter) take() (batch []writeOp, stop bool) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if len(w.queue) == 0 {
		return nil, w.closed
	}

	w.executing = true

	// A hard-state write followed by appends is the shape a follower makes
	// every time it learns of a new term and takes entries in the same
	// message. A store that can write both as one record is given the chance
	// to; one that cannot sees them separately, as before.
	//
	// Opportunistic, like the append coalescing below it: the two are queued
	// a moment apart, so a writer that happens to look in between takes the
	// hard state on its own and the entries follow. Nothing depends on the
	// pairing, which is why the ordering rather than the grouping is what the
	// interface documents.
	first := 0
	if w.batch != nil && w.queue[0].kind == writeHardState &&
		len(w.queue) > 1 && w.queue[1].kind == writeAppend {
		first = 1
	}

	if first == 0 && w.queue[0].kind != writeAppend {
		op := w.queue[0]
		w.queue = w.queue[1:]
		return []writeOp{op}, false
	}

	entries := 0
	end := first
	for end < len(w.queue) && w.queue[end].kind == writeAppend {
		next := entries + len(w.queue[end].entries)
		if end > first && next > w.maxBatchEntries {
			break
		}
		entries = next
		end++
	}
	batch = w.queue[:end:end]
	w.queue = w.queue[end:]
	return batch, false
}

// execute runs one run of operations and records a completion for each.
func (w *storageWriter) execute(batch []writeOp) {
	started := time.Now()
	err := w.run(batch)
	took := time.Since(started)

	w.mu.Lock()
	w.executing = false
	if err != nil && w.failed == nil {
		w.failed = err
	}
	for i := range batch {
		w.done = append(w.done, writeDone{
			seq:          batch[i].seq,
			kind:         batch[i].kind,
			durableAfter: batch[i].durableAfter,
			err:          err,
			// A coalesced append is one write; charging its full cost to each
			// operation in it would report a latency no caller experienced.
			took: took / time.Duration(len(batch)),
		})
	}
	w.mu.Unlock()

	select {
	case w.notify <- struct{}{}:
	default:
	}
}

// run performs the storage calls for one run of operations.
//
// The context is deliberately not the node's: cancelling it is how the node is
// stopped, and a write that is already under way at that moment is one the
// node would rather finish than abandon. Shutdown is close, which lets the
// queue drain.
func (w *storageWriter) run(batch []writeOp) error {
	w.mu.Lock()
	failed := w.failed
	w.mu.Unlock()
	if failed != nil {
		// The log's contents are already unknown. Report the original failure
		// rather than layering a second one on top of it.
		return failed
	}

	// A leading hard state means the store implements BatchWriter and take
	// put the two together; without one, a run of appends is still one call.
	var hs *HardState
	appends := batch
	if batch[0].kind == writeHardState && len(batch) > 1 {
		hs = &batch[0].hs
		appends = batch[1:]
	}

	if appends[0].kind == writeAppend {
		entries := appends[0].entries
		if len(appends) > 1 {
			total := 0
			for i := range appends {
				total += len(appends[i].entries)
			}
			entries = make([]LogEntry, 0, total)
			for i := range appends {
				entries = append(entries, appends[i].entries...)
			}
		}
		if hs != nil {
			return w.batch.SaveState(context.Background(), hs, entries)
		}
		if len(entries) == 0 {
			return nil
		}
		return w.storage.AppendLogEntries(context.Background(), entries)
	}

	op := batch[0]
	switch op.kind {
	case writeTruncateSuffix:
		return w.storage.TruncateSuffix(context.Background(), op.index)
	case writeTruncatePrefix:
		return w.storage.TruncatePrefix(context.Background(), op.index)
	case writeHardState:
		return w.storage.SaveHardState(context.Background(), op.hs)
	default:
		return fmt.Errorf("raft: unknown storage write kind %d", op.kind)
	}
}
