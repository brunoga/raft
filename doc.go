// Package raft is an implementation of the Raft consensus algorithm.
//
// A cluster of nodes agrees on an ordered log of commands and applies them to
// a replicated state machine, so that every node ends up in the same state and
// a majority surviving is enough to keep serving. The implementation follows
// the Raft dissertation, including the parts the paper leaves optional:
// pre-vote, check-quorum, leadership transfer, linearizable reads, snapshots,
// single-server and joint-consensus membership changes, and an exactly-once
// client protocol.
//
// Everywhere it departs from the dissertation, and every limitation it knows
// about, is listed in docs/divergence.md.
//
// # Getting started
//
// A node needs three things supplied: somewhere to keep its log, something to
// talk to its peers with, and the state machine to apply entries to.
//
//	cfg := raft.DefaultConfig()
//	cfg.ID = "n1"
//	cfg.Peers = []raft.PeerConfig{{ID: "n2", Voter: true}, {ID: "n3", Voter: true}}
//	cfg.Storage = store            // storage/filestore, or your own
//	cfg.StateMachine = sm          // yours
//	cfg.Transport = tr             // transport/grpctransport, or your own
//
//	node, err := raft.New(&cfg)
//	if err != nil {
//	    return err
//	}
//	tr.Register(cfg.ID, node.Handler())  // inbound RPCs reach this node
//	node.Start()
//	defer node.Stop()
//
//	result, err := node.Propose(ctx, cmd)  // returns once applied
//
// [Node.Propose] returns what [StateMachine.Apply] returned, on the node that
// proposed it, once the entry has committed and been applied. It returns
// [ErrNotLeader] on a follower, wrapped in a [NotLeaderError] carrying the
// leader's ID so a client can be redirected.
//
// For a service rather than a library -- transport, storage, snapshots, an
// HTTP API and cluster joining already wired together -- see the easyraft
// subpackage.
//
// # The three interfaces
//
// [StateMachine] is the application. Apply must be deterministic: the same
// entry must produce the same result on every node, so no wall clock, no
// randomness, no reading anything outside the state machine. Snapshot and
// Restore stream through an [io.Writer] and an [io.Reader] rather than
// buffering.
//
// [Storage] is the durable log, the term and the vote. Every mutating method
// must be durable before it returns, because Raft's safety argument assumes a
// node cannot forget what it has promised. storage/filestore implements it;
// storage/memstore does not survive the process and is for tests.
//
// [Transport] carries the RPCs. transport/grpctransport is the production
// implementation; transport/memtransport runs a cluster in one process.
//
// # Optional interfaces
//
// A Storage or StateMachine that can do better than the minimum says so by
// implementing another interface, which the engine detects. None is required
// and none changes the contract of the one it extends.
//
//   - [BatchWriter]: write the hard state and a run of entries as one durable
//     operation, for one fsync instead of two.
//   - [CommitRecorder]: remember how far the log committed, so that recovering
//     from a permanent quorum loss has more to go on than the snapshot
//     boundary.
//   - [DurableStateMachine]: report the index whose effects are already durable
//     in the state machine's own storage, so a restart replays nothing it has
//     already applied.
//   - [BatchApplier]: apply a run of committed entries in one call.
//   - [SnapshotCapturer]: hand over a cheap point-in-time copy, so serialising
//     a snapshot does not hold up the apply loop.
//
// # What waits for the disk
//
// Log writes do not block the event loop: they are queued and carried out
// behind it, so a slow disk delays only what depends on the disk rather than
// stopping the node from counting election ticks or answering its peers. What
// still waits is anything another node could rely on -- a vote, an
// acknowledgement a leader may count towards a commit, a commit index. A node
// that cannot complete a durable write stops rather than continuing with state
// that may not survive a restart; see [ErrNodeFailed].
//
// # Reads
//
// Reading through [Node.Propose] works but costs a log entry. [Node.ReadIndex]
// is the linearizable read without one: it confirms leadership with a round of
// heartbeats and returns the index a reader must wait for.
// [Node.ReadIndexLease] skips the round trip using a clock-based lease, which
// is sound only under bounded clock drift and requires Config.CheckQuorum.
// Prefer ReadIndex unless you have measured your clocks.
//
// # Beyond one group
//
// [Manager] runs many groups on shared infrastructure -- one transport, one
// ticker, heartbeats between the same pair of nodes coalesced into one RPC --
// which is what makes thousands of groups per process practical.
// [BalanceController] keeps their leaders spread evenly across machines.
//
// # Concurrency
//
// Every exported method of [Node] is safe to call from any goroutine. The
// engine itself is single-threaded: one event loop owns the consensus state,
// and everything else reaches it through channels or atomic mirrors. The
// interfaces you implement are called as documented on each one --
// StateMachine's three methods are never called concurrently with each other.
package raft
