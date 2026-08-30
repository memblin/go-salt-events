// Package stats aggregates the event stream.
//
// Everything here is fed at ingest and NEVER derived from the cache
// (spec §5.4, invariant 3). That is what keeps an hours-long session's
// events/min graph correct after the events themselves have been evicted —
// and it is what lets the future exporter reuse this package without a cache
// or a viewport (spec §14).
package stats

import "time"

// Clock is injected so no test in this package ever sleeps.
type Clock interface {
	Now() time.Time
}

// RealClock is the production Clock.
type RealClock struct{}

// Now returns the current time.
func (RealClock) Now() time.Time { return time.Now() }

// FakeClock is a test Clock with an explicitly advanced time.
type FakeClock struct {
	t time.Time
}

// NewFakeClock returns a FakeClock starting at t.
func NewFakeClock(t time.Time) *FakeClock { return &FakeClock{t: t} }

// Now returns the current fake time.
func (c *FakeClock) Now() time.Time { return c.t }

// Advance moves the fake clock forward.
func (c *FakeClock) Advance(d time.Duration) { c.t = c.t.Add(d) }

// Set moves the fake clock to t.
//
// Advance composes a time out of increments, which is exactly what a test of
// PHASE cannot use: the offsets that expose a gap-vs-zero flicker are absolute
// instants relative to a bucket boundary, not deltas from wherever the clock
// happens to have arrived.
func (c *FakeClock) Set(t time.Time) { c.t = t }
