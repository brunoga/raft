package memstore

import (
	"context"
	"testing"

	"github.com/brunoga/raft"
)

func bigEntries(from, to raft.Index) []raft.LogEntry {
	entries := make([]raft.LogEntry, 0, int(to-from+1))
	for i := from; i <= to; i++ {
		entries = append(entries, raft.LogEntry{
			Index:   i,
			Term:    1,
			Command: make([]byte, 4096),
		})
	}
	return entries
}

// TestTruncatePrefix_DoesNotRetainCompactedEntries checks that compaction
// actually releases what it drops.
//
// Re-slicing the entry list from the front leaves the kept tail pointing into
// the middle of the original backing array, so every compacted entry — and the
// command buffer it holds — stays reachable for as long as the store lives.
// The point of compaction is to release exactly that memory, and a long run
// that compacts repeatedly grows without bound.
func TestTruncatePrefix_DoesNotRetainCompactedEntries(t *testing.T) {
	m := New()
	ctx := context.Background()

	if err := m.AppendLogEntries(ctx, bigEntries(1, 64)); err != nil {
		t.Fatalf("AppendLogEntries: %v", err)
	}
	original := m.entries

	if err := m.TruncatePrefix(ctx, 33); err != nil {
		t.Fatalf("TruncatePrefix: %v", err)
	}
	if len(m.entries) != 32 {
		t.Fatalf("kept %d entries, want 32", len(m.entries))
	}

	kept := &m.entries[0]
	for i := range original {
		if &original[i] == kept {
			t.Fatalf("the kept tail still starts at element %d of the original backing "+
				"array, so the %d compacted entries before it are still reachable", i, i)
		}
	}
}

// TestTruncateSuffix_ReleasesTheDiscardedTail is the mirror case: entries past
// the new end must not stay reachable through spare capacity in the backing
// array.
func TestTruncateSuffix_ReleasesTheDiscardedTail(t *testing.T) {
	m := New()
	ctx := context.Background()

	if err := m.AppendLogEntries(ctx, bigEntries(1, 64)); err != nil {
		t.Fatalf("AppendLogEntries: %v", err)
	}

	if err := m.TruncateSuffix(ctx, 33); err != nil {
		t.Fatalf("TruncateSuffix: %v", err)
	}
	if len(m.entries) != 32 {
		t.Fatalf("kept %d entries, want 32", len(m.entries))
	}
	if extra := cap(m.entries) - len(m.entries); extra != 0 {
		t.Fatalf("the backing array still has room for %d entries past the end of the "+
			"log, so the discarded entries are still reachable", extra)
	}
}

// TestTruncate_WholeLogReleasesEverything covers the paths that drop the log
// entirely.
func TestTruncate_WholeLogReleasesEverything(t *testing.T) {
	tests := []struct {
		name     string
		truncate func(*MemStore, context.Context) error
	}{
		{
			name: "suffix from the first index",
			truncate: func(m *MemStore, ctx context.Context) error {
				return m.TruncateSuffix(ctx, 1)
			},
		},
		{
			name: "prefix past the last index",
			truncate: func(m *MemStore, ctx context.Context) error {
				return m.TruncatePrefix(ctx, 65)
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := New()
			ctx := context.Background()
			if err := m.AppendLogEntries(ctx, bigEntries(1, 64)); err != nil {
				t.Fatalf("AppendLogEntries: %v", err)
			}
			if err := tc.truncate(m, ctx); err != nil {
				t.Fatalf("truncate: %v", err)
			}
			if m.entries != nil {
				t.Fatalf("the entry list still holds %d entries with capacity %d, want it released",
					len(m.entries), cap(m.entries))
			}
		})
	}
}
