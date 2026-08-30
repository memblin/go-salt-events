package stats_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/TKC-Labs/go-salt-events/internal/model"
	"github.com/TKC-Labs/go-salt-events/internal/stats"
)

func newEvent(kind model.Kind, jid, minion string, retcode int, success bool) model.Event {
	return model.Event{
		Arrival: time.Date(2026, 8, 30, 8, 14, 2, 0, time.UTC),
		Kind:    kind,
		JID:     jid,
		Minion:  minion,
		RetCode: retcode,
		Success: success,
	}
}

func TestJobIndexCorrelatesNewWithReturns(t *testing.T) {
	t.Parallel()

	idx := stats.NewJobIndex(100, 1<<20, stats.NewFakeClock(time.Now()))

	idx.Observe(newEvent(model.KindNew, "20260830081402123456", "", 0, true),
		[]string{"web-1", "web-2", "web-3"}, false)

	idx.Observe(newEvent(model.KindRet, "20260830081402123456", "web-1", 0, true), nil, false)
	idx.Observe(newEvent(model.KindRet, "20260830081402123456", "web-2", 1, true), nil, false)

	job, lookup := idx.Job("20260830081402123456")
	if lookup != stats.LookupFound {
		t.Fatalf("lookup = %v, want LookupFound", lookup)
	}

	if got := job.Returned(); got != 2 {
		t.Errorf("Returned() = %d, want 2", got)
	}

	if got := job.Failed(); got != 1 {
		t.Errorf("Failed() = %d, want 1", got)
	}

	n, state := job.ExpectedCount()
	if state != model.ExpectedKnown || n != 3 {
		t.Errorf("ExpectedCount() = %d, %v; want 3, ExpectedKnown", n, state)
	}

	missing, ok := job.Missing()
	if !ok || len(missing) != 1 || missing[0] != "web-3" {
		t.Errorf("Missing() = %v, %v; want [web-3], true", missing, ok)
	}
}

func TestJobIndexDistinguishesTheThreeExpectedStates(t *testing.T) {
	t.Parallel()

	// Spec §5.3 case B. These are three DIFFERENT renderings with three
	// different fixes, and the bug this guards is two of them collapsing into
	// one. Asserting them as separate cases is the point.
	tests := []struct {
		name      string
		observe   func(*stats.JobIndex)
		wantState model.ExpectedState
	}{
		{
			name: "known",
			observe: func(i *stats.JobIndex) {
				i.Observe(newEvent(model.KindNew, "20260830081402123456", "", 0, true),
					[]string{"web-1"}, false)
			},
			wantState: model.ExpectedKnown,
		},
		{
			name: "trimmed by the master",
			observe: func(i *stats.JobIndex) {
				i.Observe(newEvent(model.KindNew, "20260830081402123456", "", 0, true),
					nil, true)
			},
			wantState: model.ExpectedTrimmed,
		},
		{
			name: "never seen — only returns observed",
			observe: func(i *stats.JobIndex) {
				i.Observe(newEvent(model.KindRet, "20260830081402123456", "web-1", 0, true),
					nil, false)
			},
			wantState: model.ExpectedUnseen,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			idx := stats.NewJobIndex(100, 1<<20, stats.NewFakeClock(time.Now()))
			tt.observe(idx)

			job, lookup := idx.Job("20260830081402123456")
			if lookup != stats.LookupFound {
				t.Fatalf("lookup = %v", lookup)
			}

			_, state := job.ExpectedCount()
			if state != tt.wantState {
				t.Errorf("ExpectedState = %v, want %v", state, tt.wantState)
			}
		})
	}
}

func TestJobIndexNeverReportsMissingWhenExpectedIsUnknown(t *testing.T) {
	t.Parallel()

	// Invariant 10. "0 missing" reads as "everything returned" and is the most
	// dangerous wrong answer this tool can give.
	idx := stats.NewJobIndex(100, 1<<20, stats.NewFakeClock(time.Now()))
	idx.Observe(newEvent(model.KindRet, "20260830081402123456", "web-1", 0, true), nil, false)

	job, _ := idx.Job("20260830081402123456")

	if _, ok := job.Missing(); ok {
		t.Error("Missing() reported ok=true with an unknown expected set")
	}
}

func TestJobIndexDistinguishesEvictedFromNeverSeen(t *testing.T) {
	t.Parallel()

	// Same principle as the three expected states: an evicted job's fix is
	// --max-jobs, a never-seen job's fix is attaching sooner (spec §7.5).
	idx := stats.NewJobIndex(2, 1<<20, stats.NewFakeClock(time.Now()))

	for i := range 5 {
		jid := fmt.Sprintf("2026083008140212345%d", i)
		idx.Observe(newEvent(model.KindNew, jid, "", 0, true), []string{"web-1"}, false)
		idx.Observe(newEvent(model.KindRet, jid, "web-1", 0, true), nil, false)
	}

	if _, lookup := idx.Job("20260830081402123450"); lookup != stats.LookupEvicted {
		t.Errorf("lookup for an evicted job = %v, want LookupEvicted", lookup)
	}

	if _, lookup := idx.Job("99999999999999999999"); lookup != stats.LookupUnseen {
		t.Errorf("lookup for a never-seen job = %v, want LookupUnseen", lookup)
	}
}

func TestJobIndexNeverEvictsAnIncompleteJob(t *testing.T) {
	t.Parallel()

	// A job still receiving returns is the one most likely being watched.
	idx := stats.NewJobIndex(2, 1<<20, stats.NewFakeClock(time.Now()))

	idx.Observe(newEvent(model.KindNew, "20260830081402100000", "", 0, true),
		[]string{"web-1", "web-2"}, false)
	idx.Observe(newEvent(model.KindRet, "20260830081402100000", "web-1", 0, true), nil, false)

	for i := range 5 {
		jid := fmt.Sprintf("2026083008140220000%d", i)
		idx.Observe(newEvent(model.KindNew, jid, "", 0, true), []string{"w"}, false)
		idx.Observe(newEvent(model.KindRet, jid, "w", 0, true), nil, false)
	}

	if _, lookup := idx.Job("20260830081402100000"); lookup != stats.LookupFound {
		t.Error("an incomplete job was evicted; it is the one most likely being watched")
	}
}

func TestJobIndexNeverEvictsThePinnedJob(t *testing.T) {
	t.Parallel()

	// A job vanishing out from under the cursor while being read is the worst
	// possible moment to lose it (spec §7.5).
	idx := stats.NewJobIndex(2, 1<<20, stats.NewFakeClock(time.Now()))

	idx.Observe(newEvent(model.KindNew, "20260830081402100000", "", 0, true),
		[]string{"web-1"}, false)
	idx.Observe(newEvent(model.KindRet, "20260830081402100000", "web-1", 0, true), nil, false)

	idx.Pin("20260830081402100000")

	for i := range 10 {
		jid := fmt.Sprintf("2026083008140230000%d", i)
		idx.Observe(newEvent(model.KindNew, jid, "", 0, true), []string{"w"}, false)
		idx.Observe(newEvent(model.KindRet, jid, "w", 0, true), nil, false)
	}

	if _, lookup := idx.Job("20260830081402100000"); lookup != stats.LookupFound {
		t.Error("the pinned job was evicted")
	}
}

func TestJobIndexReportsPressure(t *testing.T) {
	t.Parallel()

	// 500 is a starting value, not a sufficient one. The design's job is to
	// make a wrong number VISIBLE so it can be tuned from evidence (spec §7.5).
	idx := stats.NewJobIndex(2, 1<<20, stats.NewFakeClock(time.Now()))

	for i := range 6 {
		jid := fmt.Sprintf("2026083008140240000%d", i)
		idx.Observe(newEvent(model.KindNew, jid, "", 0, true), []string{"w"}, false)
		idx.Observe(newEvent(model.KindRet, jid, "w", 0, true), nil, false)
	}

	got := idx.Stats()

	if got.Evicted == 0 {
		t.Error("Evicted = 0; eviction must never be silent")
	}

	if got.Cap != 2 {
		t.Errorf("Cap = %d, want 2", got.Cap)
	}

	if got.HighWater < 2 {
		t.Errorf("HighWater = %d, want at least 2", got.HighWater)
	}
}

func TestJobIndexDeduplicatesRepeatedReturns(t *testing.T) {
	t.Parallel()

	idx := stats.NewJobIndex(100, 1<<20, stats.NewFakeClock(time.Now()))
	idx.Observe(newEvent(model.KindNew, "20260830081402123456", "", 0, true),
		[]string{"web-1"}, false)

	idx.Observe(newEvent(model.KindRet, "20260830081402123456", "web-1", 0, true), nil, false)
	idx.Observe(newEvent(model.KindRet, "20260830081402123456", "web-1", 0, true), nil, false)

	job, _ := idx.Job("20260830081402123456")
	if got := job.Returned(); got != 1 {
		t.Errorf("Returned() = %d, want 1 — a duplicate return must not render 2/1", got)
	}
}

// The tests below are additions to the brief. Each guards a way the index
// could stop being bounded, which would turn --max-jobs and the memory ceiling
// into decoration (spec §7.5: "the ceiling only exists so the knob cannot be
// turned into an OOM").

func TestJobIndexStaysBoundedWhenNoJobCanEverComplete(t *testing.T) {
	t.Parallel()

	// Attaching to a busy master mid-stream is the ordinary case: every
	// in-flight job's `new` event is already gone, so its expected set is
	// ExpectedUnseen and Complete() can never become true. If "never evict an
	// incomplete job" were absolute, this session would grow without bound and
	// neither --max-jobs nor the ceiling could stop it.
	idx := stats.NewJobIndex(2, 1<<20, stats.NewFakeClock(time.Now()))

	for i := range 20 {
		jid := fmt.Sprintf("2026083008140250000%d", i)
		idx.Observe(newEvent(model.KindRet, jid, "web-1", 0, true), nil, false)
	}

	got := idx.Stats()

	if got.Tracked > got.Cap {
		t.Errorf("Tracked = %d with Cap = %d; the index is not bounded", got.Tracked, got.Cap)
	}

	if got.Evicted == 0 {
		t.Error("Evicted = 0; the count bound was silently abandoned rather than enforced")
	}
}

func TestJobIndexHonoursTheMemoryCeiling(t *testing.T) {
	t.Parallel()

	// The ceiling is the backstop that bounds the damage when --max-jobs is
	// raised into the tens of thousands against large targets (spec §7.5).
	idx := stats.NewJobIndex(1_000_000, 4096, stats.NewFakeClock(time.Now()))

	for i := range 200 {
		jid := fmt.Sprintf("202608300814026%05d", i)

		targets := make([]string, 0, 32)
		for m := range 32 {
			targets = append(targets, fmt.Sprintf("web-%d-%d", i, m))
		}

		idx.Observe(newEvent(model.KindNew, jid, "", 0, true), targets, false)

		for _, m := range targets {
			idx.Observe(newEvent(model.KindRet, jid, m, 0, true), nil, false)
		}
	}

	got := idx.Stats()

	if got.Evicted == 0 {
		t.Error("Evicted = 0; the memory ceiling was never enforced")
	}

	if got.Tracked > 100 {
		t.Errorf("Tracked = %d; the ceiling should have held this far below the count cap", got.Tracked)
	}
}

func TestJobIndexMemoryAccountingDoesNotDriftOnDuplicateReturns(t *testing.T) {
	t.Parallel()

	// A minion can return twice for the same JID. Charging the ceiling for
	// every repeat rather than for the state actually held would make the
	// index's accounting climb forever and evict live jobs for memory it is
	// not using.
	idx := stats.NewJobIndex(100, 4096, stats.NewFakeClock(time.Now()))

	idx.Observe(newEvent(model.KindNew, "20260830081402700000", "", 0, true),
		[]string{"web-1"}, false)

	for range 1000 {
		idx.Observe(newEvent(model.KindRet, "20260830081402700000", "web-1", 0, true), nil, false)
	}

	if _, lookup := idx.Job("20260830081402700000"); lookup != stats.LookupFound {
		t.Error("a one-minion job was evicted by repeated returns; the byte accounting drifts")
	}

	if got := idx.Stats().Evicted; got != 0 {
		t.Errorf("Evicted = %d, want 0 — nothing here should have been under pressure", got)
	}
}

func TestJobIndexEvictionMemoryIsItselfBounded(t *testing.T) {
	t.Parallel()

	// Remembering every JID ever evicted is an unbounded map on a
	// long-running session — the leak the index exists to prevent, moved one
	// level up. The memory is therefore a bounded FIFO, and this test pins the
	// consequence: past the horizon a very old eviction degrades to
	// LookupUnseen rather than being remembered forever.
	idx := stats.NewJobIndex(1, 1<<20, stats.NewFakeClock(time.Now()))

	for i := range 5000 {
		jid := fmt.Sprintf("202608300814028%05d", i)
		idx.Observe(newEvent(model.KindNew, jid, "", 0, true), []string{"w"}, false)
		idx.Observe(newEvent(model.KindRet, jid, "w", 0, true), nil, false)
	}

	if _, lookup := idx.Job("20260830081402800000"); lookup != stats.LookupUnseen {
		t.Errorf("lookup for a long-forgotten eviction = %v; the eviction memory is unbounded", lookup)
	}

	recent := fmt.Sprintf("202608300814028%05d", 4990)
	if _, lookup := idx.Job(recent); lookup != stats.LookupEvicted {
		t.Errorf("lookup for a recent eviction = %v, want LookupEvicted", lookup)
	}
}

func TestJobIndexUnpinRestoresEvictability(t *testing.T) {
	t.Parallel()

	idx := stats.NewJobIndex(2, 1<<20, stats.NewFakeClock(time.Now()))

	idx.Observe(newEvent(model.KindNew, "20260830081402900000", "", 0, true),
		[]string{"web-1"}, false)
	idx.Observe(newEvent(model.KindRet, "20260830081402900000", "web-1", 0, true), nil, false)

	idx.Pin("20260830081402900000")
	idx.Unpin()

	for i := range 10 {
		jid := fmt.Sprintf("2026083008140291000%d", i)
		idx.Observe(newEvent(model.KindNew, jid, "", 0, true), []string{"w"}, false)
		idx.Observe(newEvent(model.KindRet, jid, "w", 0, true), nil, false)
	}

	if _, lookup := idx.Job("20260830081402900000"); lookup != stats.LookupEvicted {
		t.Errorf("lookup after Unpin = %v, want LookupEvicted — the pin was never released", lookup)
	}
}

func TestJobIndexIgnoresEventsItCannotCorrelate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		event model.Event
	}{
		{
			name:  "no jid",
			event: newEvent(model.KindNew, "", "", 0, true),
		},
		{
			name:  "a return with no minion",
			event: newEvent(model.KindRet, "20260830081402123456", "", 0, true),
		},
		{
			name:  "an auth event that happens to carry a jid",
			event: newEvent(model.KindAuth, "20260830081402123456", "web-1", 0, true),
		},
		{
			name:  "a presence event",
			event: newEvent(model.KindPresence, "20260830081402123456", "web-1", 0, true),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			idx := stats.NewJobIndex(100, 1<<20, stats.NewFakeClock(time.Now()))
			idx.Observe(tt.event, nil, false)

			if got := idx.Stats().Tracked; got != 0 {
				t.Errorf("Tracked = %d, want 0 — an uncorrelatable event created a phantom job", got)
			}
		})
	}
}

func TestJobIndexListReturnsTheMostRecentJobsNewestFirst(t *testing.T) {
	t.Parallel()

	idx := stats.NewJobIndex(100, 1<<20, stats.NewFakeClock(time.Now()))

	for i := range 5 {
		jid := fmt.Sprintf("2026083008140295000%d", i)
		idx.Observe(newEvent(model.KindNew, jid, "", 0, true), []string{"w"}, false)
	}

	got := idx.List(3)
	if len(got) != 3 {
		t.Fatalf("List(3) returned %d jobs, want 3", len(got))
	}

	want := []string{
		"20260830081402950004",
		"20260830081402950003",
		"20260830081402950002",
	}

	for i := range want {
		if got[i].JID != want[i] {
			t.Errorf("List(3)[%d].JID = %s, want %s", i, got[i].JID, want[i])
		}
	}
}

func TestJobIndexListDoesNotOverAllocateForAnAbsurdN(t *testing.T) {
	t.Parallel()

	// n comes from a viewport height, but a bug upstream must not turn it into
	// a multi-gigabyte allocation.
	idx := stats.NewJobIndex(100, 1<<20, stats.NewFakeClock(time.Now()))
	idx.Observe(newEvent(model.KindNew, "20260830081402960000", "", 0, true), []string{"w"}, false)

	if got := idx.List(1 << 30); len(got) != 1 {
		t.Errorf("List(1<<30) returned %d jobs, want 1", len(got))
	}

	if got := idx.List(0); got != nil {
		t.Errorf("List(0) = %v, want nil", got)
	}

	if got := idx.List(-1); got != nil {
		t.Errorf("List(-1) = %v, want nil", got)
	}
}
