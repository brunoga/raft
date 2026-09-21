package main

import (
	"testing"

	"github.com/brunoga/raft/examples/internal/shutdowncheck"
)

// TestShutdown_SignalStopsTheProcess is a regression test for a shutdown that
// never completed.
//
// The observer goroutine returns when the event channel closes, and only the
// stop function returned by Node.Events closes it. Both were deferred, and
// deferred calls run last-registered-first, so the wait for the goroutine ran
// before the call that lets the goroutine finish. The process printed
// "shutting down" and stayed there forever; pressing ^C again did nothing,
// because signal.Notify had already replaced the default handler that would
// have killed it.
//
// The open-stream half of the check covers the second fault the first one was
// hiding: /events has no natural end, and http.Server.Shutdown waits for
// in-flight requests rather than cancelling them, so one attached viewer held
// the process for the whole shutdown timeout and then exited non-zero.
func TestShutdown_SignalStopsTheProcess(t *testing.T) {
	t.Parallel()
	shutdowncheck.Run(t, shutdowncheck.Options{
		Package:     ".",
		ReadyPath:   "/health",
		StreamPath:  "/events",
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
