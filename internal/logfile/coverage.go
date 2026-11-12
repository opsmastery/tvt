package logfile

import "time"

// Coverage estimates how complete a log is over [since, now). Entries are
// added with Add; heartbeat entries (rule evaluations, written at a steady
// rate by a working backend) let it find gaps.
//
// With heartbeats in the window, coverage is the share of Bucket-sized
// intervals that contain any entry. Without them an empty interval may only
// mean nobody queried, so coverage is the share of the window after the
// log's first entry, assuming no gaps; a log that starts before the window
// covers all of it. Entries stamped after now are ignored.
type Coverage struct {
	since, now time.Time
	bucket     time.Duration
	seen       []bool
	first      time.Time
	heartbeat  bool
}

// NewCoverage returns a Coverage for the window; bucket defaults to 10
// minutes and should be longer than the slowest heartbeat interval.
func NewCoverage(since, now time.Time, bucket time.Duration) *Coverage {
	if bucket <= 0 {
		bucket = 10 * time.Minute
	}
	n := 0
	if since.Before(now) {
		n = int((now.Sub(since) + bucket - 1) / bucket)
	}
	return &Coverage{since: since, now: now, bucket: bucket, seen: make([]bool, n)}
}

// Add records one entry. Entries before the window still show where the
// log starts.
func (c *Coverage) Add(at time.Time, heartbeat bool) {
	if at.After(c.now) {
		return
	}
	if c.first.IsZero() || at.Before(c.first) {
		c.first = at
	}
	if at.Before(c.since) {
		return
	}
	if heartbeat {
		c.heartbeat = true
	}
	if i := int(at.Sub(c.since) / c.bucket); i < len(c.seen) {
		c.seen[i] = true
	}
}

// Value is the coverage, 0..1.
func (c *Coverage) Value() float64 {
	if c.first.IsZero() || len(c.seen) == 0 {
		return 0
	}
	if !c.heartbeat {
		if !c.first.After(c.since) {
			return 1
		}
		return float64(c.now.Sub(c.first)) / float64(c.now.Sub(c.since))
	}
	covered := 0
	for _, s := range c.seen {
		if s {
			covered++
		}
	}
	return float64(covered) / float64(len(c.seen))
}
