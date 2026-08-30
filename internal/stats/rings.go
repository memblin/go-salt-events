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
func (r *Rings) MarkGap(from, to time.Time) {
	for s := from.Unix(); s <= to.Unix(); s++ {
		r.advance(s, s/60)

		if d := r.secEpoch - s; d >= 0 && d < SecondBuckets {
			b := &r.secs[s%SecondBuckets]
			if b.Count == 0 {
				b.Gap = true
			}
		}
	}

	for m := from.Unix() / 60; m <= to.Unix()/60; m++ {
		if d := r.minEpoch - m; d >= 0 && d < MinuteBuckets {
			b := &r.mins[m%MinuteBuckets]
			if b.Count == 0 {
				b.Gap = true
			}
		}
	}
}

// advance rolls the rings forward to the given bucket indices, clearing any
// buckets the clock has skipped over.
func (r *Rings) advance(si, mi int64) {
	if !r.started {
		r.secEpoch, r.minEpoch, r.started = si, mi, true

		return
	}

	if si > r.secEpoch {
		for s := r.secEpoch + 1; s <= si; s++ {
			r.secs[s%SecondBuckets] = Bucket{}
		}

		r.secEpoch = si
	}

	if mi > r.minEpoch {
		for m := r.minEpoch + 1; m <= mi; m++ {
			r.mins[m%MinuteBuckets] = Bucket{}
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
type Summary struct {
	Now  float64
	Peak float64
	Mean float64
}

// SummarySeconds summarises the events/sec window.
func (r *Rings) SummarySeconds() Summary { return summarise(r.Seconds()) }

// SummaryMinutes summarises the events/min window.
func (r *Rings) SummaryMinutes() Summary { return summarise(r.Minutes()) }

// summarise computes now/peak/mean, ignoring gap buckets so a disconnection
// does not drag the mean toward zero.
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
		s.Now = float64(bs[len(bs)-1].Count)
	}

	if live > 0 {
		s.Mean = total / live
	}

	return s
}
