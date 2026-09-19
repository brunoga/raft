package raft

import (
	"testing"
	"time"
)

// flushWrites waits until the storage writer has nothing left to do and then
// processes every completion it produced.
//
// It stands in for the event loop, which collects completions itself as part
// of its select. Tests that drive a Node by hand without starting the loop
// need it wherever they append to the log and then assert on something that
// depends on the entries being on disk.
func (n *Node) flushWrites(t *testing.T) {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	for {
		n.handleWriteCompletions()
		if !n.writer.busy() && len(n.log.segs) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("storage writes did not drain: %d queued, %d segments in memory",
				len(n.writer.queue), len(n.log.segs))
		}
		time.Sleep(200 * time.Microsecond)
	}
}
