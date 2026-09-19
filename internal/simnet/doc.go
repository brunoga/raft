// Package simnet provides a seeded, fault-injecting simulation of the two
// seams a Raft node talks to: the network (raft.Transport) and stable storage
// (raft.Storage).
//
// It exists because the plain in-memory transport delivers every RPC by a
// direct, immediate function call. That makes tests fast but it also means the
// only failure a test can express is "this link is down". Real networks lose,
// duplicate, delay and reorder individual messages, and real disks fail; a test
// suite that cannot express those faults cannot claim to have exercised the
// parts of Raft that exist to survive them.
//
// # Determinism
//
// Every decision the simulator makes — whether a message is lost, whether it is
// duplicated, how long each leg of the round-trip takes — is drawn from a
// single math/rand/v2 generator seeded from one uint64. Re-running with the
// same seed replays the same decision sequence for the same sequence of
// submitted messages.
//
// That is reproducibility in practice, not in principle, and the distinction
// matters. The Raft node under test is a multi-goroutine program driven by a
// real clock: its event loop, its per-peer heartbeat pumps, its apply loop and
// its snapshot workers are scheduled by the Go runtime, not by this package.
// The simulator therefore cannot control the *order in which messages are
// submitted to it*, only what happens to each message once submitted. Two runs
// with the same seed will usually, but not always, follow the same path.
// Achieving full determinism would require the Raft engine itself to be
// restructured around a single-threaded, injectable scheduler.
//
// In exchange for that caveat, a seed is cheap and usually enough: a failure
// found by a randomized test is far more likely to reproduce from its seed than
// from nothing at all. Tests in this repository print the seed on failure and
// accept one from the RAFT_SIM_SEED environment variable.
//
// # Simulated time
//
// Latency is simulated by sleeping the calling goroutine, so it is wall-clock
// time and should be kept to single-digit milliseconds. The Raft node's logical
// clock is separate: tests drive it with Node.Tick (Config.TickInterval == 0).
package simnet
