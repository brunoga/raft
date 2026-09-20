package raft

import "time"

const (
	// applySaturationWindow is how much recent history one reported ratio
	// covers. Long enough that a single slow Apply does not read as a
	// saturated node, short enough that the number still describes what the
	// node is doing now rather than what it did this morning.
	applySaturationWindow = 5 * time.Second
	// applySaturationBuckets is how many slices the window is cut into. The
	// window advances one slice at a time, which is what makes it roll rather
	// than reset: a ratio is never computed from a window that has just been
	// emptied.
	applySaturationBuckets = 10
)

// applySaturation measures what fraction of its time the apply loop spends
// working rather than waiting for something to do, over a rolling window.
//
// It answers the question that commit and apply latency cannot: when writes
// are slow, is the state machine the thing that is slow, or is it Raft? A
// saturation near 1 says the apply loop never gets to wait, so the state
// machine is the constraint and no amount of faster consensus will help. A
// saturation near 0 with slow writes says the opposite, and points at
// replication or the disk instead.
//
// It is owned by the apply goroutine and every method must be called from it.
// A nil *applySaturation is a working no-op, which is what a node whose
// Metrics does not implement ApplyMetrics gets, so that measuring costs
// nothing when nobody is looking.
type applySaturation struct {
	now    func() time.Time
	report func(ratio float64)

	// busy and total are the per-bucket accounting. total is the elapsed time
	// the bucket covers and busy the part of it spent working, kept separately
	// so that a window that is not yet full still reports a true ratio rather
	// than one diluted by buckets that never happened.
	busy  [applySaturationBuckets]time.Duration
	total [applySaturationBuckets]time.Duration
	idx   int

	// bucket is how long each of them covers, bucketEnd when the current one
	// closes, mark the start of the stretch of time not yet accounted for, and
	// busyNow whether that stretch is being spent working.
	bucket    time.Duration
	bucketEnd time.Time
	mark      time.Time
	busyNow   bool

	// ticker wakes the apply loop when it has been idle for a whole bucket, so
	// that a gauge which says "busy" does not stay saying it after the work
	// stopped. It exists only when something is listening.
	ticker *time.Ticker
}

// newApplySaturation returns a tracker that calls report with the ratio for
// the whole window each time a bucket of length bucket closes, or nil when
// report is nil. It starts in the working state, because the apply loop's
// first act is to restore from whatever it found on disk.
func newApplySaturation(now func() time.Time, bucket time.Duration, report func(ratio float64)) *applySaturation {
	if report == nil {
		return nil
	}
	start := now()
	return &applySaturation{
		now:       now,
		report:    report,
		bucket:    bucket,
		bucketEnd: start.Add(bucket),
		mark:      start,
		busyNow:   true,
		ticker:    time.NewTicker(bucket),
	}
}

// applySaturationTracker returns the tracker for this node's apply loop, or
// nil when nothing is collecting the measurement.
func (n *Node) applySaturationTracker() *applySaturation {
	if n.cfg.Metrics == nil {
		return nil
	}
	am, isApplyMetrics := n.cfg.Metrics.(ApplyMetrics)
	if !isApplyMetrics {
		return nil
	}
	bucket := applySaturationWindow / applySaturationBuckets
	return newApplySaturation(n.now, bucket, func(ratio float64) {
		am.ApplySaturation(n.cfg.ID, ratio)
	})
}

// samples is the channel the apply loop selects on to be woken for a sample it
// would otherwise not take. It is nil when there is no tracker, which makes
// that case of the select block for ever, which is exactly the cost wanted: a
// node nobody is measuring is never woken.
func (s *applySaturation) samples() <-chan time.Time {
	if s == nil {
		return nil
	}
	return s.ticker.C
}

// working records that the apply loop has started doing something.
func (s *applySaturation) working() {
	s.transition(true)
}

// waiting records that the apply loop is about to block waiting for work.
func (s *applySaturation) waiting() {
	s.transition(false)
}

// sample brings the accounting up to date without changing what the loop is
// doing, so that a loop that has been waiting for a long time still reports.
func (s *applySaturation) sample() {
	if s == nil {
		return
	}
	s.advance(s.now())
}

// stop releases the ticker. The tracker must not be used afterwards.
func (s *applySaturation) stop() {
	if s == nil {
		return
	}
	s.ticker.Stop()
}

// transition accounts for the time since the last transition and records what
// the loop is doing now.
func (s *applySaturation) transition(working bool) {
	if s == nil {
		return
	}
	s.advance(s.now())
	s.busyNow = working
}

// advance credits everything between mark and now to the buckets it falls in,
// closing and reporting each bucket it crosses.
func (s *applySaturation) advance(now time.Time) {
	if now.Before(s.mark) {
		// A clock that went backwards would otherwise credit negative time and
		// make the ratio meaningless for a whole window. Restart the current
		// bucket instead: one lost sample is cheaper than a lying one.
		s.mark = now
		return
	}
	for !now.Before(s.bucketEnd) {
		s.credit(s.bucketEnd.Sub(s.mark))
		s.mark = s.bucketEnd
		s.bucketEnd = s.bucketEnd.Add(s.bucket)
		s.closeBucket()
	}
	s.credit(now.Sub(s.mark))
	s.mark = now
}

// credit adds d to the bucket being filled.
func (s *applySaturation) credit(d time.Duration) {
	if d <= 0 {
		return
	}
	s.total[s.idx] += d
	if s.busyNow {
		s.busy[s.idx] += d
	}
}

// closeBucket reports the ratio across the whole window and moves on to the
// next bucket, discarding the oldest one it overwrites.
func (s *applySaturation) closeBucket() {
	var busy, total time.Duration
	for i := range s.total {
		busy += s.busy[i]
		total += s.total[i]
	}
	if total > 0 {
		s.report(float64(busy) / float64(total))
	}
	s.idx = (s.idx + 1) % applySaturationBuckets
	s.busy[s.idx] = 0
	s.total[s.idx] = 0
}
