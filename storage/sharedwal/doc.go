// Package sharedwal provides a raft.Storage backend in which every Raft group
// on a host shares one write-ahead log, so that a burst of appends from many
// groups costs one fsync rather than one per group.
//
// # Why
//
// With a storage directory per group, G groups appending at once issue G
// fsyncs, and fsync is the expensive part: on NVMe a few hundred concurrent
// writers is the practical ceiling before accumulated latency spikes cause
// election timeouts, and on network-attached storage it is far lower. The
// groups do not need separate files. What they need is that their own records
// are durable, in order, before they are told so -- and one log, synced once
// per batch, gives every group in the batch exactly that.
//
// # Layout
//
// Under the data directory:
//
//	LOCK                   held while the log is open; a second opener fails
//	wal-00000000.log       segments, appended in sequence and never rewritten
//	snap/<group>-<index>   one snapshot file per group, replaced as a whole
//
// A segment is a sequence of records, each carrying the group it belongs to:
// a run of entries, a hard state, a truncation, a snapshot's position, a
// recorded commit index, or a group's removal. Every record has a CRC, so a
// crash in the middle of a write leaves a tail that fails its check and is
// cut off on the next open, never a record that reads as something else.
//
// # Writes
//
// Every mutating call builds its record and hands it to one goroutine, which
// drains everything waiting, writes it all to the active segment, syncs once,
// and only then lets the callers return. The order of records within a group
// is the order the group's calls were made in, which is what the engine
// requires; across groups there is no order to keep. The state each call
// describes takes effect in memory only after the sync, so a reader never
// sees an entry the disk does not hold.
//
// # Space
//
// A segment is deleted once nothing in it is still needed: every entry it
// holds has been compacted or truncated, and no group's latest hard state,
// snapshot position or commit index lives in it. Those last three are tiny,
// so a segment kept alive only by them is reclaimed by appending fresh copies
// to the active segment first; a segment kept alive by a small number of live
// entries is treated the same way, up to a size bound, so that a quiet group
// with a short log cannot pin an old segment for ever.
//
// # Use
//
//	wal, err := sharedwal.Open(dir)
//	// ...
//	cfg.Storage = wal.Storage(groupID)
//
// The value returned by Storage implements raft.Storage, raft.BatchWriter and
// raft.CommitRecorder. Its Close is a no-op: the log is shared, and closing it
// is the WAL's job. Groups the log knows about are listed by Groups, which is
// how a host that runs many groups finds them again after a restart, and a
// group that has been decommissioned is dropped with Remove.
package sharedwal
