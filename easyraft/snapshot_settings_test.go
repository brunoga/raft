package easyraft

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/brunoga/raft"
)

// TestApplySnapshotSettings_NeverLeavesThePairInconsistent checks the
// relationship between the two settings that govern compaction.
//
// SnapshotThreshold is how many entries pass before a snapshot is taken;
// TrailingLogs is how many are kept after one. If the second is not smaller
// than the first, compacting reclaims nothing and the engine warns and caps it.
//
// That warning used to fire on every easyraft node with default settings, which
// is the worst kind of warning: one everybody sees on a correct configuration,
// and therefore one everybody learns to scroll past.
func TestApplySnapshotSettings_NeverLeavesThePairInconsistent(t *testing.T) {
	cases := []struct {
		name      string
		snapCount uint64
	}{
		{"default", 0},
		{"smaller than the engine's trailing logs", 100},
		{"exactly the engine's trailing logs", 1024},
		{"larger", 50_000},
		{"tiny, as a test would set it", 2},
		{"one", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := raft.DefaultConfig()
			applySnapshotSettings(&cfg, tc.snapCount)

			if cfg.SnapshotThreshold == 0 {
				t.Fatal("snapshots disabled; the log would grow without bound")
			}
			if cfg.TrailingLogs >= cfg.SnapshotThreshold {
				t.Errorf("TrailingLogs %d >= SnapshotThreshold %d: compaction reclaims nothing "+
					"and the engine warns on every startup",
					cfg.TrailingLogs, cfg.SnapshotThreshold)
			}
			if cfg.TrailingLogs == 0 && cfg.SnapshotThreshold > 1 {
				t.Error("TrailingLogs 0: a follower one entry behind needs a whole snapshot")
			}
			if tc.snapCount > 0 && cfg.SnapshotThreshold != tc.snapCount {
				t.Errorf("SnapshotThreshold = %d, want the %d that was asked for",
					cfg.SnapshotThreshold, tc.snapCount)
			}
		})
	}
}

// TestApplySnapshotSettings_LeavesAConsistentPairAlone checks that the engine's
// own defaults are not second-guessed. easyraft overrode them with a threshold
// of its own and created the inconsistency; the fix is to stop.
func TestApplySnapshotSettings_LeavesAConsistentPairAlone(t *testing.T) {
	cfg := raft.DefaultConfig()
	before := cfg
	applySnapshotSettings(&cfg, 0)

	if cfg.SnapshotThreshold != before.SnapshotThreshold {
		t.Errorf("SnapshotThreshold = %d, want the engine's default %d",
			cfg.SnapshotThreshold, before.SnapshotThreshold)
	}
	if cfg.TrailingLogs != before.TrailingLogs {
		t.Errorf("TrailingLogs = %d, want the engine's default %d",
			cfg.TrailingLogs, before.TrailingLogs)
	}
}

// TestStore_DefaultConfigurationDoesNotWarn is the same property from the
// outside: build a node the way the quick start does and read its log.
//
// The unit test above can be satisfied by a function nobody calls. This one
// fails if the wiring is wrong, which is how the defect got in -- the
// relationship was fine in principle and the two settings were assigned in two
// places that did not know about each other.
func TestStore_DefaultConfigurationDoesNotWarn(t *testing.T) {
	var buf bytes.Buffer
	s, err := NewStore(
		WithID("n1"),
		WithRaftAddr("127.0.0.1:0"),
		WithDataDir(t.TempDir()),
		WithLogger(slog.New(slog.NewTextHandler(&buf, nil))),
	)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	_ = s.Stop()

	if strings.Contains(buf.String(), "TrailingLogs") {
		t.Errorf("a node built with default settings warns at startup:\n%s", buf.String())
	}
}

// TestStore_ExplicitSnapCountDoesNotWarn covers the other way in: a caller who
// sets WithSnapCount below the engine's default TrailingLogs, which is most
// values anyone would pick for a test or a small deployment.
func TestStore_ExplicitSnapCountDoesNotWarn(t *testing.T) {
	var buf bytes.Buffer
	s, err := NewStore(
		WithID("n1"),
		WithRaftAddr("127.0.0.1:0"),
		WithDataDir(t.TempDir()),
		WithSnapCount(50),
		WithLogger(slog.New(slog.NewTextHandler(&buf, nil))),
	)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	_ = s.Stop()

	if strings.Contains(buf.String(), "TrailingLogs") {
		t.Errorf("WithSnapCount(50) warns at startup:\n%s", buf.String())
	}
}
