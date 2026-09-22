package raft_test

import (
	"time"

	"github.com/brunoga/raft/v2"
)

// ---- Timings for tests that drive their own ticks ---------------------------
//
// A node converts its timeouts to tick counts once, when it is constructed,
// against Config.TickInterval -- or against 10ms when that is left at zero,
// which is what a test does when it means to drive ticks itself.
//
// The conversion is the whole of the problem. A test that then calls Tick
// every millisecond is running the cluster ten times faster than it was
// configured for: the default 150-300ms election timeout becomes 15-30ms of
// wall clock, and a heartbeat round-trip under -race on a loaded machine does
// not reliably fit in that. Check-quorum steps a perfectly healthy leader
// down, a proposal comes back ErrNotLeader, and the test fails for a reason
// that has nothing to do with what it was testing.
//
// It has happened three times in this repository, each time found by a test
// failing in CI on an unrelated change:
//
//   - TestRestart_FollowerCatchesUpAfterRestart
//   - TestInstallSnapshot_Chunked, which had the same shape and had not lost
//     the race yet
//   - TestCommitFloor_NarrowsTheBandARecoveryGuessesAbout, the only one that
//     also used a real filestore, so every election waited on an fsync the
//     next election did not wait for
//
// So this is the default for a manual-tick test rather than something to
// remember after the third time. A test that needs different timings -- one
// that is about election behaviour itself -- sets them after calling this.

// tuneForManualTicks sizes the timeouts for a test that calls Tick roughly
// every millisecond.
//
// At that rate these give an election window of 100-200ms of wall clock and a
// heartbeat every 10ms, which is the headroom the defaults were meant to have
// at their own tick rate. Elections still resolve in a fraction of a second,
// so nothing waits noticeably longer than before.
func tuneForManualTicks(cfg *raft.Config) {
	cfg.HeartbeatInterval = 100 * time.Millisecond
	cfg.ElectionTimeoutMin = 1000 * time.Millisecond
	cfg.ElectionTimeoutMax = 2000 * time.Millisecond
}
