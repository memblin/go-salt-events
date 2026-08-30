package stats_test

import (
	"testing"
	"time"

	"github.com/TKC-Labs/go-salt-events/internal/stats"
)

func TestRingsCountEventsIntoSecondBuckets(t *testing.T) {
	t.Parallel()

	clk := stats.NewFakeClock(time.Date(2026, 8, 30, 8, 0, 0, 0, time.UTC))
	r := stats.NewRings(clk)

	for range 5 {
		r.Add(clk.Now())
	}

	clk.Advance(time.Second)

	for range 3 {
		r.Add(clk.Now())
	}

	secs := r.Seconds()
	if len(secs) != 120 {
		t.Fatalf("Seconds() length = %d, want 120", len(secs))
	}

	// The newest bucket is last.
	if got := secs[len(secs)-1].Count; got != 3 {
		t.Errorf("newest bucket = %d, want 3", got)
	}

	if got := secs[len(secs)-2].Count; got != 5 {
		t.Errorf("previous bucket = %d, want 5", got)
	}
}

func TestRingsSummaryReportsPeak(t *testing.T) {
	t.Parallel()

	// Peak is not decoration. An autoscaled sparkline renders a 5/sec blip and
	// a 5000/sec storm identically, so the numeric callout is the only thing
	// carrying scale (spec §9).
	clk := stats.NewFakeClock(time.Date(2026, 8, 30, 8, 0, 0, 0, time.UTC))
	r := stats.NewRings(clk)

	for range 10 {
		r.Add(clk.Now())
	}

	clk.Advance(time.Second)

	for range 300 {
		r.Add(clk.Now())
	}

	clk.Advance(time.Second)
	r.Add(clk.Now())

	got := r.SummarySeconds()
	if got.Peak != 300 {
		t.Errorf("Peak = %v, want 300", got.Peak)
	}

	if got.Now != 1 {
		t.Errorf("Now = %v, want 1", got.Now)
	}
}

func TestRingsMarkGapIsDistinctFromZero(t *testing.T) {
	t.Parallel()

	// A disconnection that renders as a flat line at zero is indistinguishable
	// from a quiet master — exactly backwards during an incident (spec §8.2).
	clk := stats.NewFakeClock(time.Date(2026, 8, 30, 8, 0, 0, 0, time.UTC))
	r := stats.NewRings(clk)

	r.Add(clk.Now())

	from := clk.Now()
	clk.Advance(5 * time.Second)
	r.MarkGap(from, clk.Now())

	secs := r.Seconds()

	gapped := 0

	for _, b := range secs {
		if b.Gap {
			gapped++
		}
	}

	if gapped == 0 {
		t.Fatal("no buckets marked as gap; a disconnection must be visible")
	}

	for _, b := range secs {
		if b.Gap && b.Count != 0 {
			t.Error("a gap bucket must not also carry a count")
		}
	}
}

func TestRingsExpireOldBuckets(t *testing.T) {
	t.Parallel()

	clk := stats.NewFakeClock(time.Date(2026, 8, 30, 8, 0, 0, 0, time.UTC))
	r := stats.NewRings(clk)

	for range 50 {
		r.Add(clk.Now())
	}

	// Advance well past the 120-second window.
	clk.Advance(200 * time.Second)

	got := r.SummarySeconds()
	if got.Peak != 0 {
		t.Errorf("Peak = %v after the window expired, want 0", got.Peak)
	}
}

func TestRingsRollIntoMinuteBuckets(t *testing.T) {
	t.Parallel()

	clk := stats.NewFakeClock(time.Date(2026, 8, 30, 8, 0, 0, 0, time.UTC))
	r := stats.NewRings(clk)

	for range 120 {
		r.Add(clk.Now())
	}

	mins := r.Minutes()
	if len(mins) != 60 {
		t.Fatalf("Minutes() length = %d, want 60", len(mins))
	}

	if got := mins[len(mins)-1].Count; got != 120 {
		t.Errorf("newest minute bucket = %d, want 120", got)
	}
}

func TestRingsSummaryNowIsGapWhenNewestBucketIsAGap(t *testing.T) {
	t.Parallel()

	// A gap in the newest second must not read as "0 events/sec" — that is
	// bit-for-bit indistinguishable from a genuinely quiet master and is
	// exactly backwards during an incident (spec §8.2).
	clk := stats.NewFakeClock(time.Date(2026, 8, 30, 8, 0, 0, 0, time.UTC))
	r := stats.NewRings(clk)

	r.Add(clk.Now())

	from := clk.Now()
	clk.Advance(time.Second)
	r.MarkGap(from, clk.Now())

	got := r.SummarySeconds()
	if !got.NowIsGap {
		t.Error("NowIsGap = false, want true when the newest bucket is a gap")
	}

	if got.Now != 0 {
		t.Errorf("Now = %v while NowIsGap, want the zero value (callers must check NowIsGap first)", got.Now)
	}
}

func TestRingsSummaryHasNoDataWhenTheWholeWindowIsGapped(t *testing.T) {
	t.Parallel()

	// Peak/Mean over a fully gapped window must be distinguishable from a
	// window that genuinely saw zero events throughout.
	clk := stats.NewFakeClock(time.Date(2026, 8, 30, 8, 0, 0, 0, time.UTC))
	r := stats.NewRings(clk)

	from := clk.Now()
	clk.Advance(secondBucketsGapSpan)
	r.MarkGap(from, clk.Now())

	got := r.SummarySeconds()
	if got.HasData {
		t.Error("HasData = true, want false when every bucket in the window is a gap")
	}

	if got.Peak != 0 || got.Mean != 0 {
		t.Errorf("Peak/Mean = %v/%v while HasData is false, want the zero value", got.Peak, got.Mean)
	}

	if !got.NowIsGap {
		t.Error("NowIsGap = false, want true — the newest bucket is also gapped")
	}
}

func TestRingsSurvivesAClockJumpFarBeyondTheRing(t *testing.T) {
	t.Parallel()

	// advance() must be bounded at the ring size, not at elapsed wall-clock
	// time: a suspended host, a slow startup, or a corrupt timestamp must
	// not turn one Add/Seconds/MarkGap call into a loop over years of ticks.
	clk := stats.NewFakeClock(time.Date(2026, 8, 30, 8, 0, 0, 0, time.UTC))
	r := stats.NewRings(clk)

	for range 50 {
		r.Add(clk.Now())
	}

	from := clk.Now()
	clk.Advance(50 * 365 * 24 * time.Hour) // ~50 years — far beyond both rings.
	r.MarkGap(from, clk.Now())

	secs := r.Seconds()
	if len(secs) != 120 {
		t.Fatalf("Seconds() length = %d, want 120", len(secs))
	}

	for i, b := range secs {
		if b.Count != 0 {
			t.Errorf("secs[%d].Count = %d, want 0 after a jump far beyond the ring", i, b.Count)
		}
	}

	mins := r.Minutes()
	if len(mins) != 60 {
		t.Fatalf("Minutes() length = %d, want 60", len(mins))
	}

	for i, b := range mins {
		if b.Count != 0 {
			t.Errorf("mins[%d].Count = %d, want 0 after a jump far beyond the ring", i, b.Count)
		}
	}

	got := r.SummarySeconds()
	if got.Peak != 0 {
		t.Errorf("Peak = %v after a jump far beyond the ring, want 0", got.Peak)
	}
}

// secondBucketsGapSpan spans the whole 120-bucket second window so a MarkGap
// call over it gaps every bucket, none left over from before the window.
const secondBucketsGapSpan = stats.SecondBuckets * time.Second

// TestGapReportingConstantsCannotDriftApart pins the RELATIONSHIP, not the
// values.
//
// This is the fourth recurrence of gap-vs-zero in this project, and the reason
// it keeps coming back is that the coupling between how often a gap is reported
// and how the rings age has never been anything but reasoning in a comment. The
// values were equal — gapRefresh 1 s against a 1 s bucket — and equal was never
// sufficient: two 1 Hz processes with an arbitrary phase offset cannot cover
// each other, so the head bucket read a genuine `0` for part of every second of
// a live outage.
//
// Three orderings have to hold, and each fails a different way:
//
//   - GapReportInterval well inside GapValidity, so a report that arrives a
//     little late — the reader dials between waits, and a dial is not free —
//     still lands while the previous one is authoritative. Equal periods leave
//     no slack at all, which is exactly the mistake this replaces.
//   - GapValidity strictly less than BucketWidth, so when reports STOP (the
//     master came back and the bus is merely quiet) the ring returns to honest
//     zeros inside one bucket. Extrapolating a gap for longer than a bucket
//     would invert the error and render a quiet master as an outage.
//   - Both positive, because a zero interval is a busy loop and a zero validity
//     silently restores the original bug.
func TestGapReportingConstantsCannotDriftApart(t *testing.T) {
	t.Parallel()

	if stats.GapReportInterval <= 0 || stats.GapValidity <= 0 {
		t.Fatalf("GapReportInterval = %v, GapValidity = %v; both must be positive",
			stats.GapReportInterval, stats.GapValidity)
	}

	if 2*stats.GapReportInterval > stats.GapValidity {
		t.Errorf("GapReportInterval = %v against GapValidity = %v: a reporter that "+
			"runs a little late leaves the head bucket reading a genuine 0 during "+
			"an outage, which is the §8.2 inversion this pairing exists to prevent",
			stats.GapReportInterval, stats.GapValidity)
	}

	if stats.GapValidity >= stats.BucketWidth {
		t.Errorf("GapValidity = %v against BucketWidth = %v: a gap report stays "+
			"authoritative for a whole bucket or more, so a master that came back "+
			"and went quiet renders as an outage",
			stats.GapValidity, stats.BucketWidth)
	}
}

// TestBucketWidthIsTheWidthTheRingActuallyUses stops BucketWidth from being a
// decorative constant. The ring indexes by Unix SECOND arithmetic, not by this
// value, so nothing else would notice the two disagreeing — and the constant
// above is only meaningful if it is the real bucket width.
func TestBucketWidthIsTheWidthTheRingActuallyUses(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 8, 30, 8, 14, 2, 0, time.UTC)

	r := stats.NewRings(stats.NewFakeClock(at))
	r.Add(at)
	r.Add(at.Add(stats.BucketWidth))

	bs := r.Seconds()

	if n := len(bs); n < 2 {
		t.Fatalf("Seconds() returned %d buckets", n)
	}

	newest, before := bs[len(bs)-1], bs[len(bs)-2]

	if newest.Count != 1 || before.Count != 1 {
		t.Errorf("two events one BucketWidth (%v) apart landed as %d and %d: "+
			"BucketWidth does not describe the ring's real bucket",
			stats.BucketWidth, before.Count, newest.Count)
	}
}

// TestAnOutageNeverRendersAsAZeroAcrossBucketRollovers is the regression this
// whole pairing exists for, driven the way the real program drives it: a
// reporter calling MarkGap on its own period, a renderer calling Seconds() on
// the render tick, and a PHASE OFFSET between them and the bucket boundary.
//
// Task 7's test sampled immediately after a MarkGap call — phase zero, the one
// instant the bug cannot be observed — which is why three reviews passed over
// it. Here the offsets are deliberately awkward and the run crosses several
// bucket rollovers, because the failure IS the rollover: advance() opened each
// new head bucket as Bucket{}, a genuine Count 0 with Gap false, and
// Summary.NowIsGap reads exactly that flag. On a live outage the Rate pane's
// `now` callout flipped between "no data" and "0" eleven times.
func TestAnOutageNeverRendersAsAZeroAcrossBucketRollovers(t *testing.T) {
	t.Parallel()

	// renderInterval mirrors config.RenderInterval. internal/stats must not
	// import internal/config (it is the layer below), so it is restated.
	const renderInterval = 100 * time.Millisecond

	// Offsets chosen so neither the reporter nor the renderer is aligned with a
	// bucket boundary or with each other.
	for _, offset := range []time.Duration{
		0, 37 * time.Millisecond, 249 * time.Millisecond, 501 * time.Millisecond,
		870 * time.Millisecond, 999 * time.Millisecond,
	} {
		t.Run(offset.String(), func(t *testing.T) {
			t.Parallel()

			base := time.Date(2026, 8, 30, 8, 14, 2, 0, time.UTC)
			lostAt := base.Add(offset)

			clk := stats.NewFakeClock(lostAt)
			r := stats.NewRings(clk)

			// A live bus, several buckets before the outage, so the buckets the
			// outage runs through are genuinely empty. A bucket that saw a real
			// event is never a gap and must not be — markGapRing refuses to
			// overwrite a count, which is correct.
			r.Add(lostAt.Add(-5 * time.Second))

			// The reader's very first report of the outage.
			r.MarkGap(lostAt, lostAt)

			var (
				lastReport = lostAt
				elapsed    time.Duration
			)

			// Five seconds of outage: five bucket rollovers, forty render ticks.
			for elapsed = renderInterval; elapsed <= 5*time.Second; elapsed += renderInterval {
				now := lostAt.Add(elapsed)

				// The reporter runs on its own period, not on the render tick.
				for !now.Before(lastReport.Add(stats.GapReportInterval)) {
					lastReport = lastReport.Add(stats.GapReportInterval)
					clk.Set(lastReport)
					r.MarkGap(lostAt, lastReport)
				}

				clk.Set(now)

				sum := r.SummarySeconds()
				if !sum.NowIsGap {
					t.Fatalf("at +%v (offset %v) the head bucket reads a genuine 0: "+
						"the Rate pane prints `now 0` and the operator reads a quiet "+
						"master where the bus is gone (spec §8.2)", elapsed, offset)
				}
			}
		})
	}
}

// TestTheRingReturnsToHonestZerosWhenTheGapReportsStop is the other half, and
// it is why GapValidity is bounded rather than open-ended.
//
// A gap report is extrapolated forwards precisely because the reporter promises
// to repeat it. When the master comes back the reports stop, and the ring must
// stop claiming an outage — otherwise a reconnected but QUIET master renders as
// a lost bus, which is spec §8.2's inversion pointed the other way, and the
// reader's own history: connection state used to be inferred from event arrival
// and a healthy quiet master read DISCONNECTED in capitals.
func TestTheRingReturnsToHonestZerosWhenTheGapReportsStop(t *testing.T) {
	t.Parallel()

	lostAt := time.Date(2026, 8, 30, 8, 14, 2, 0, time.UTC)
	clk := stats.NewFakeClock(lostAt)
	r := stats.NewRings(clk)

	r.MarkGap(lostAt, lostAt)

	// One bucket after the last report, nothing may still be extrapolated.
	clk.Set(lostAt.Add(2 * stats.BucketWidth))

	if r.SummarySeconds().NowIsGap {
		t.Error("the head bucket still reads as a gap two buckets after the last " +
			"report; a quiet master that has come back renders as a lost bus")
	}
}
