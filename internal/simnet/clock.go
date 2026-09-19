package simnet

import (
	"sync"
	"time"
)

// SkewClock is a raft.Clock whose reading differs from real time by a constant
// offset and whose rate differs by a constant factor.
//
// Leader leases are the one part of Raft that trades a network round-trip for
// an assumption about clocks, so they are the one part whose safety argument
// depends on how far apart two nodes' clocks can drift. Giving each node its
// own SkewClock is how a test states that assumption out loud and then breaks
// it on purpose.
//
// Rate < 1 makes the clock run slow, which is the dangerous direction for a
// leader: a slow clock makes a lease that has really expired still look valid.
// Rate > 1 makes it run fast, which only costs availability.
//
// Safe for concurrent use.
type SkewClock struct {
	mu     sync.Mutex
	base   time.Time
	origin time.Time
	rate   float64
	offset time.Duration
}

// NewSkewClock returns a clock that starts at the current instant, advances at
// rate times real speed, and reads offset ahead of (or, when negative, behind)
// where it would otherwise be.
func NewSkewClock(rate float64, offset time.Duration) *SkewClock {
	now := time.Now()
	return &SkewClock{base: now, origin: now, rate: rate, offset: offset}
}

// Now returns this clock's idea of the current time.
func (c *SkewClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	elapsed := time.Since(c.origin)
	return c.base.Add(time.Duration(float64(elapsed) * c.rate)).Add(c.offset)
}

// Advance jumps the clock forward by d without waiting, so a test can make a
// lease expire (or fail to) without sleeping for a real election timeout.
func (c *SkewClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.offset += d
}
