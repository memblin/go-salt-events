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
