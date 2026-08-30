package stats

import "time"

// Window sizes from spec §7.3.
const (
	SecondBuckets = 120
	MinuteBuckets = 60
)

// Bucket is one time slice.
//
// Gap and a zero Count are different facts: Gap means "we were not connected",
// zero means "the master was quiet". Merging them would make a master restart
// look like calm (spec §8.2).
type Bucket struct {
	Count uint64
	Gap   bool
}

// Rings holds the events/sec and events/min histories.
//
// Both are fixed-size ring buffers indexed by absolute time, so advancing the
// clock expires old buckets without any background goroutine.
//
// Rings is NOT safe for concurrent use. The architecture serialises access
// externally: a reader goroutine feeds it under the hub's mutex (Task 21),
// and the UI reads a snapshot assembled under that same lock. Do not add a
// mutex here — that would nest a second lock inside one already held on
// every ingest.
type Rings struct {
	clock Clock

	secs [SecondBuckets]Bucket
	mins [MinuteBuckets]Bucket

	// secEpoch and minEpoch are the absolute bucket indices currently at the
	// head of each ring.
	secEpoch int64
	minEpoch int64

	started bool
}

// NewRings returns empty rings driven by c.
func NewRings(c Clock) *Rings { return &Rings{clock: c} }

// Add counts one event at arrival time t.
//
// t is the ARRIVAL time, never Salt's _stamp: _stamp is set by whichever
// process fired the event, so a skewed minion clock would push events into the
// wrong bucket or the future (spec §4.3, invariant 2).
func (r *Rings) Add(t time.Time) {
	si, mi := t.Unix(), t.Unix()/60

	r.advance(si, mi)

	if d := r.secEpoch - si; d >= 0 && d < SecondBuckets {
		b := &r.secs[si%SecondBuckets]
		b.Count++
		b.Gap = false
	}

	if d := r.minEpoch - mi; d >= 0 && d < MinuteBuckets {
		b := &r.mins[mi%MinuteBuckets]
		b.Count++
		b.Gap = false
	}
}

// MarkGap records that we were disconnected between from and to.
//
// Cost is bounded at the ring size, not at (to - from): jump the epoch to to
// in one bounded step (advance already clamps that), then only walk the
// range that can still be visible in a SecondBuckets/MinuteBuckets window —
// anything older than that has already aged out of every ring, so marking it
// would do work nothing can ever observe (a multi-year gap must not become a
// multi-year loop).
func (r *Rings) MarkGap(from, to time.Time) {
	r.advance(to.Unix(), to.Unix()/60)

	markGapRing(r.secs[:], r.secEpoch, from.Unix(), to.Unix(), SecondBuckets)
	markGapRing(r.mins[:], r.minEpoch, from.Unix()/60, to.Unix()/60, MinuteBuckets)
}

// markGapRing marks every empty bucket in [from, to] as a gap on one ring.
//
// from is clamped to the start of the visible window before the walk: only
// the range that can still be visible in an n-bucket window matters, since
// anything older than that has already aged out of the ring, and marking it
// would do work nothing can ever observe (a multi-year gap must not become a
// multi-year loop). A bucket only becomes a gap if its Count is still 0, so a
// bucket that already saw a real event is never overwritten.
func markGapRing(ring []Bucket, epoch, from, to, n int64) {
	if start := epoch - (n - 1); from < start {
		from = start
	}

	for i := from; i <= to; i++ {
		if d := epoch - i; d >= 0 && d < n {
			b := &ring[i%n]
			if b.Count == 0 {
				b.Gap = true
			}
		}
	}
}

// advance rolls the rings forward to the given bucket indices, clearing any
// buckets the clock has skipped over.
//
// The clearing work is bounded at the ring size: a step larger than the ring
// touches every bucket anyway (a suspended host, a slow startup, or a corrupt
// timestamp must not turn a single Add/Seconds/MarkGap call into a loop over
// however much wall-clock time has passed).
func (r *Rings) advance(si, mi int64) {
	if !r.started {
		r.secEpoch, r.minEpoch, r.started = si, mi, true

		return
	}

	if si > r.secEpoch {
		if si-r.secEpoch >= SecondBuckets {
			r.secs = [SecondBuckets]Bucket{}
		} else {
			for s := r.secEpoch + 1; s <= si; s++ {
				r.secs[s%SecondBuckets] = Bucket{}
			}
		}

		r.secEpoch = si
	}

	if mi > r.minEpoch {
		if mi-r.minEpoch >= MinuteBuckets {
			r.mins = [MinuteBuckets]Bucket{}
		} else {
			for m := r.minEpoch + 1; m <= mi; m++ {
				r.mins[m%MinuteBuckets] = Bucket{}
			}
		}

		r.minEpoch = mi
	}
}

// Seconds returns the last 120 one-second buckets, oldest first.
func (r *Rings) Seconds() []Bucket {
	r.syncToNow()

	return window(r.secs[:], r.secEpoch, SecondBuckets)
}

// Minutes returns the last 60 one-minute buckets, oldest first.
func (r *Rings) Minutes() []Bucket {
	r.syncToNow()

	return window(r.mins[:], r.minEpoch, MinuteBuckets)
}

// syncToNow expires buckets that have aged out since the last Add, so a quiet
// period shows as zeros rather than as stale counts.
func (r *Rings) syncToNow() {
	now := r.clock.Now()
	r.advance(now.Unix(), now.Unix()/60)
}

// window copies a ring into oldest-first order.
func window(ring []Bucket, epoch int64, n int) []Bucket {
	out := make([]Bucket, n)

	for i := range n {
		idx := epoch - int64(n-1-i)
		if idx < 0 {
			continue
		}

		out[i] = ring[idx%int64(n)]
	}

	return out
}

// Summary is the numeric callout beside a sparkline. It is mandatory, not
// decorative: an autoscaled sparkline renders a 5/sec blip and a 5000/sec
// storm identically, so these numbers are the only thing carrying scale
// (spec §9).
//
// A consumer must check NowIsGap and HasData before printing Now/Peak/Mean:
// gap-vs-zero is a fact about the data (spec §8.2, same distinction as
// Bucket.Gap), not a rendering choice, so it is carried here rather than
// left for every future consumer — including the planned Prometheus
// exporter (spec §14) — to re-derive from raw buckets.
type Summary struct {
	Now  float64
	Peak float64
	Mean float64

	// NowIsGap is true when the newest bucket is a gap: Now is not
	// meaningful and must not be printed as "0".
	NowIsGap bool

	// HasData is false when every bucket in the window is a gap: Peak and
	// Mean are not meaningful and must not be printed as "0".
	HasData bool
}

// SummarySeconds summarises the events/sec window.
func (r *Rings) SummarySeconds() Summary { return summarise(r.Seconds()) }

// SummaryMinutes summarises the events/min window.
func (r *Rings) SummaryMinutes() Summary { return summarise(r.Minutes()) }

// summarise computes now/peak/mean, ignoring gap buckets so a disconnection
// does not drag the mean toward zero — and flags NowIsGap/HasData so a
// gapped window is never reported as a genuine zero (spec §8.2).
func summarise(bs []Bucket) Summary {
	var s Summary

	var total, live float64

	for _, b := range bs {
		if b.Gap {
			continue
		}

		live++
		total += float64(b.Count)

		if float64(b.Count) > s.Peak {
			s.Peak = float64(b.Count)
		}
	}

	if len(bs) > 0 {
		if last := bs[len(bs)-1]; last.Gap {
			s.NowIsGap = true
		} else {
			s.Now = float64(last.Count)
		}
	}

	if live > 0 {
		s.Mean = total / live
		s.HasData = true
	}

	return s
}
