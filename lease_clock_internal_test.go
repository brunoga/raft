package raft

import (
	"log/slog"
	"testing"
	"time"
)

// TestLeaseClockJumped pins the rule that turns wall-clock and monotonic
// elapsed times into a suspend verdict.
func TestLeaseClockJumped(t *testing.T) {
	const margin = 15 * time.Millisecond
	cases := []struct {
		name       string
		wall, mono time.Duration
		want       bool
	}{
		{"clocks agree", 100 * time.Millisecond, 100 * time.Millisecond, false},
		{"no monotonic reading, so both figures are wall", 100 * time.Millisecond, 100 * time.Millisecond, false},
		{"ntp slew within margin", 100 * time.Millisecond, 90 * time.Millisecond, false},
		{"exactly the margin", 115 * time.Millisecond, 100 * time.Millisecond, false},
		{"suspend: wall ran ahead", 2 * time.Second, 100 * time.Millisecond, true},
		{"just over the margin", 116 * time.Millisecond, 100 * time.Millisecond, true},
		{"wall set backwards is not a jump", 50 * time.Millisecond, 100 * time.Millisecond, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := leaseClockJumped(tc.wall, tc.mono, margin); got != tc.want {
				t.Fatalf("leaseClockJumped(%v, %v, %v) = %v, want %v",
					tc.wall, tc.mono, margin, got, tc.want)
			}
		})
	}
}

// TestLeaseValid_DetectsWallClockRunningAhead drives leaseValid with times
// whose wall and monotonic readings disagree the way they do after a
// suspend: the monotonic reading barely moves while the wall clock jumps.
// Go does not let a test forge a monotonic reading, so the base carries a
// real one and the "now" is built without: Sub then falls back to wall clock
// for both figures, which is the no-detection case, checked here as well.
func TestLeaseValid_NoFalsePositiveWithoutMonotonic(t *testing.T) {
	n := &Node{logger: slog.New(slog.DiscardHandler)}
	n.cfg.ElectionTimeoutMin = 150 * time.Millisecond
	n.cfg.LeaseSafetyMargin = 15 * time.Millisecond
	base := time.Now()
	n.leaseBase = base
	n.leaseExpiry = base.Add(n.cfg.ElectionTimeoutMin - n.cfg.LeaseSafetyMargin)

	// A wall-only time well inside the lease: valid, and no divergence can be
	// measured because now has no monotonic reading.
	now := time.Unix(0, base.UnixNano()+int64(50*time.Millisecond))
	if !n.leaseValid(now) {
		t.Fatal("lease judged invalid 50ms in with no monotonic reading to disagree")
	}
	// Past the expiry: invalid on the expiry alone.
	now = time.Unix(0, base.UnixNano()+int64(140*time.Millisecond))
	if n.leaseValid(now) {
		t.Fatal("lease judged valid past its shortened expiry")
	}
}
