package main

import (
	"testing"

	"github.com/brunoga/raft/examples/internal/shutdowncheck"
)

// TestShutdown_SignalReachesTheDeferredStop is a regression test for a
// shutdown path that had never run.
//
// main blocked in ListenAndServe and installed no signal handler, so ^C
// terminated the process through the default disposition and every deferred
// call was skipped -- including the store.Stop() that stops the Raft node and
// closes the log. Nothing about that is fatal, since the log exists so a node
// can come back from being killed, but an example is a thing people copy.
//
// From outside, a process killed by the default handler and one that shut down
// cleanly both simply stop, which is why this asserts on the line Stop logs
// rather than on the exit alone.
func TestShutdown_SignalReachesTheDeferredStop(t *testing.T) {
	t.Parallel()
	shutdowncheck.Run(t, shutdowncheck.Options{
		Package:   ".",
		ReadyPath: "/members",
		// /watch is a server-sent-event stream with no natural end. Shutdown
		// waits for in-flight requests rather than cancelling them, so one
		// attached watcher would hold the process until the shutdown timeout.
		StreamPath:  "/watch",
		WantLogLine: "stopped",
		Args: func(raftAddr, httpAddr, dataDir string) []string {
			return []string{
				"--id", "n1",
				"--raft-addr", raftAddr,
				"--http-addr", httpAddr,
				"--data-dir", dataDir,
			}
		},
	})
}
