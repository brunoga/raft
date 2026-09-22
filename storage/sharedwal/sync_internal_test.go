package sharedwal

import (
	"context"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/brunoga/raft/v2"
)

// TestGroupCommit_ManyGroupsFewSyncs pins the reason the package exists: a
// burst of appends from many groups costs far fewer fsyncs than appends.
//
// The batching comes from the sync taking time: while one is in progress
// every request that arrives queues behind it and goes out in the next batch.
// A temporary directory on a fast disk syncs in microseconds and batches
// little, so the sync is made to take what a real disk takes, which is what
// makes the count meaningful rather than lucky.
func TestGroupCommit_ManyGroupsFewSyncs(t *testing.T) {
	var syncs atomic.Int64
	orig := syncFile
	syncFile = func(f *os.File) error {
		syncs.Add(1)
		time.Sleep(2 * time.Millisecond)
		return f.Sync()
	}
	t.Cleanup(func() { syncFile = orig })

	w, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()

	const groups, rounds = 64, 20
	// Hold every writer at a barrier per round so the appends really are
	// concurrent, then count.
	for r := range rounds {
		var start, done sync.WaitGroup
		start.Add(1)
		for g := range groups {
			done.Add(1)
			go func(id uint64) {
				defer done.Done()
				start.Wait()
				s := w.Storage(id)
				e := []raft.LogEntry{{Index: raft.Index(r + 1), Term: 1, Command: []byte("x")}}
				if err := s.AppendLogEntries(context.Background(), e); err != nil {
					t.Error(err)
				}
			}(uint64(g))
		}
		start.Done()
		done.Wait()
	}
	appends := int64(groups * rounds)
	got := syncs.Load()
	t.Logf("%d appends across %d groups cost %d fsyncs", appends, groups, got)
	// Each round can take a sync for the first arrival and one for the rest;
	// a few more are allowed for scheduling noise.
	if got > 4*rounds {
		t.Fatalf("%d fsyncs for %d concurrent appends; group commit is not amortising them", got, appends)
	}
	if got < rounds {
		t.Fatalf("%d fsyncs for %d rounds; something was acknowledged without a sync", got, rounds)
	}
}

// TestSyncFailure_FailsEverythingAfter pins that a log that could not be
// made durable stops saying anything is.
func TestSyncFailure_FailsEverythingAfter(t *testing.T) {
	orig := syncFile
	failing := false
	syncFile = func(f *os.File) error {
		if failing {
			return os.ErrClosed
		}
		return f.Sync()
	}
	t.Cleanup(func() { syncFile = orig })

	w, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	s := w.Storage(1)
	if err := s.AppendLogEntries(context.Background(), []raft.LogEntry{{Index: 1, Term: 1}}); err != nil {
		t.Fatal(err)
	}
	failing = true
	if err := s.AppendLogEntries(context.Background(), []raft.LogEntry{{Index: 2, Term: 1}}); err == nil {
		t.Fatal("append succeeded although the sync failed")
	}
	failing = false
	if err := s.AppendLogEntries(context.Background(), []raft.LogEntry{{Index: 2, Term: 1}}); err == nil {
		t.Fatal("append succeeded after an earlier sync failure")
	}
	if last, _ := s.LastIndex(); last != 1 {
		t.Fatalf("an entry from a failed batch is visible: last %d", last)
	}
}
