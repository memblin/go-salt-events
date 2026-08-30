package stats_test

import (
	"bytes"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/TKC-Labs/go-salt-events/internal/cache"
	"github.com/TKC-Labs/go-salt-events/internal/model"
	"github.com/TKC-Labs/go-salt-events/internal/stats"
)

// This file is invariant 9, expressed at the layer that actually matters:
// whether the Jobs pane still answers correctly once the cache has shed the
// payloads underneath it (spec §5.2).
//
// The cache-side half — "no eagerly-extracted field is disturbed by shedding" —
// lives in internal/cache as TestCacheSheddingNeverAltersAnIndexedField. This
// is the stats-side half: "which of my 1,000 minions failed, and which never
// came back" must be answerable EXACTLY after a gigabyte of returns has blown
// through the budget. Only the individual return bodies are expendable.
//
// The shape of the test is a differential. The same event sequence is fed twice
// — once through a cache small enough that every payload is shed, once through
// one large enough that nothing degrades — and the two runs must produce
// identical answers. That is much harder to fool than a hardcoded number, which
// stays green whenever a bug happens to move both runs together.
//
// The index is deliberately fed from the CACHE's copy of each event rather than
// the caller's. Spec §5.2 names two ways to break the guarantee: shedding an
// indexed field along with the payload, and recomputing job state by re-reading
// cached payloads. Replaying out of the cache is what puts both within this
// test's reach; feeding the index from a pristine local variable would make it
// pass no matter what degradation did.

const (
	largeJobJID = "20260830081402123456"

	// Return bodies of this size are the case the design is built around: a
	// thousand of them is far past any sane budget (spec §5.2).
	retPayloadBytes = 8192
	newPayloadBytes = 4096
)

// jobAnswers is every question the Jobs pane asks a job. Invariant 9 says each
// field here must come out identical whether or not the payloads survived.
type jobAnswers struct {
	returned      int
	failed        int
	expectedN     int
	expectedState model.ExpectedState
	missing       []string
	missingOK     bool
}

// largeJobCase is one job, built twice.
type largeJobCase struct {
	name string

	// sawNew reports whether the job/new event was observed at all. Attaching
	// to a master mid-job is the ordinary case, and it is the one that leaves
	// the denominator unknown.
	sawNew bool

	// masterTrimmed reports that Salt's trim_dict replaced the minions list
	// before we ever saw it — a different cause with a different fix from our
	// own shedding (spec §5.3 case A).
	masterTrimmed bool

	targets   int
	returning int
	failing   int

	want jobAnswers
}

func TestLargeJobStaysReadableAfterTheCacheShedsEveryPayload(t *testing.T) {
	t.Parallel()

	const (
		targets   = 1000
		returning = 812
		failing   = 23
	)

	tests := []largeJobCase{
		{
			name:      "the expected set is known",
			sawNew:    true,
			targets:   targets,
			returning: returning,
			failing:   failing,
			want: jobAnswers{
				returned:      returning,
				failed:        failing,
				expectedN:     targets,
				expectedState: model.ExpectedKnown,
				// Minions return in name order, so everyone from web-0812
				// onward is still outstanding. "Which ones" is the actual
				// question during an incident, not "how many".
				missing:   largeJobTargets(targets)[returning:],
				missingOK: true,
			},
		},
		{
			name:          "the master trimmed the expected set",
			sawNew:        true,
			masterTrimmed: true,
			targets:       targets,
			returning:     returning,
			failing:       failing,
			// Invariant 10 under memory pressure. Shedding must not convert an
			// unknown denominator into a confident zero: "812 returned, 0
			// missing" reads as "everything is fine" and walks the operator
			// away from 188 broken machines (spec §5.3 case B).
			want: jobAnswers{
				returned:      returning,
				failed:        failing,
				expectedN:     0,
				expectedState: model.ExpectedTrimmed,
				missing:       nil,
				missingOK:     false,
			},
		},
		{
			name:      "the new event was never seen",
			sawNew:    false,
			targets:   targets,
			returning: returning,
			failing:   failing,
			want: jobAnswers{
				returned:      returning,
				failed:        failing,
				expectedN:     0,
				expectedState: model.ExpectedUnseen,
				missing:       nil,
				missingOK:     false,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			events := largeJobEvents(tt)

			// A budget that holds every event's tag and per-event overhead and
			// not one byte of payload. Degradation must therefore shed every
			// payload, and — because the metadata alone exactly fills the
			// budget — must drop no event at all. Isolating shedding from event
			// loss is the point: a dropped event legitimately changes a count,
			// and would make a failure here ambiguous.
			shed, shedStats := replayLargeJob(t, tt, largeJobMetadataBytes(events))
			intact, intactStats := replayLargeJob(t, tt, 1<<30)

			// Premise: the small run really did shed, all of it, and lost
			// nothing. Without this the whole test is vacuous and passes for
			// the wrong reason.
			if shedStats.Shed != len(events) {
				t.Fatalf("premise failed: Shed = %d, want %d (used=%d budget=%d)",
					shedStats.Shed, len(events), shedStats.Used, shedStats.Budget)
			}

			if shedStats.Dropped != 0 {
				t.Fatalf("premise failed: Dropped = %d; this test must isolate shedding from event loss",
					shedStats.Dropped)
			}

			if shedStats.Events != len(events) {
				t.Fatalf("premise failed: Events = %d, want %d", shedStats.Events, len(events))
			}

			// Premise: the control run is a genuine control — it did not
			// degrade, so the two runs differ in exactly one thing.
			if intactStats.Shed != 0 || intactStats.Dropped != 0 {
				t.Fatalf("premise failed: the control run degraded (Shed=%d Dropped=%d)",
					intactStats.Shed, intactStats.Dropped)
			}

			// The differential. This is what catches degradation disturbing the
			// job index, however it does so.
			checkJobAnswers(t, "after shedding, versus the same job unshed", shed, intact)

			// And the absolute, because a break that moved BOTH runs together
			// would satisfy the differential while rendering plausible,
			// confident, wrong numbers — the worst failure this tool has
			// (spec §13).
			checkJobAnswers(t, "after shedding, versus the truth", shed, tt.want)
		})
	}
}

// replayLargeJob feeds the case's events through a cache bounded by budget,
// then rebuilds the job index from what the cache retained.
//
// The expected-minion list is passed to Observe alongside the event rather than
// read back out of it: that list is decoded once at ingest and never re-read
// from a payload (invariant 4). Keeping the two apart here is exactly the
// separation invariant 9 protects.
func replayLargeJob(t *testing.T, tc largeJobCase, budget int64) (jobAnswers, cache.Stats) {
	t.Helper()

	c := cache.New(budget)
	for _, e := range largeJobEvents(tc) {
		c.Add(e)
	}

	var expected []string
	if tc.sawNew && !tc.masterTrimmed {
		expected = largeJobTargets(tc.targets)
	}

	idx := stats.NewJobIndex(100, 1<<20, stats.NewFakeClock(time.Now()))

	for _, e := range c.All() {
		if e.Kind == model.KindNew {
			idx.Observe(e, expected, tc.masterTrimmed)

			continue
		}

		idx.Observe(e, nil, false)
	}

	job, lookup := idx.Job(largeJobJID)
	if lookup != stats.LookupFound {
		t.Fatalf("lookup = %v, want LookupFound (budget=%d)", lookup, budget)
	}

	n, state := job.ExpectedCount()
	missing, ok := job.Missing()

	return jobAnswers{
		returned:      job.Returned(),
		failed:        job.Failed(),
		expectedN:     n,
		expectedState: state,
		missing:       missing,
		missingOK:     ok,
	}, c.Stats()
}

// checkJobAnswers reports every field of got that differs from want.
func checkJobAnswers(t *testing.T, what string, got, want jobAnswers) {
	t.Helper()

	if got.returned != want.returned {
		t.Errorf("%s: Returned() = %d, want %d", what, got.returned, want.returned)
	}

	if got.failed != want.failed {
		t.Errorf("%s: Failed() = %d, want %d", what, got.failed, want.failed)
	}

	if got.expectedState != want.expectedState {
		t.Errorf("%s: ExpectedCount() state = %v, want %v",
			what, got.expectedState, want.expectedState)
	}

	if got.expectedN != want.expectedN {
		t.Errorf("%s: ExpectedCount() = %d, want %d", what, got.expectedN, want.expectedN)
	}

	if got.missingOK != want.missingOK {
		t.Errorf("%s: Missing() ok = %v, want %v", what, got.missingOK, want.missingOK)
	}

	if len(got.missing) != len(want.missing) {
		t.Errorf("%s: len(Missing()) = %d, want %d", what, len(got.missing), len(want.missing))

		return
	}

	if !slices.Equal(got.missing, want.missing) {
		t.Errorf("%s: Missing() differs; first mismatch at %d", what,
			firstDifference(got.missing, want.missing))
	}
}

// firstDifference returns the index of the first differing element, for an
// error message that names a minion rather than dumping a thousand of them.
func firstDifference(a, b []string) int {
	for i := range a {
		if a[i] != b[i] {
			return i
		}
	}

	return -1
}

// largeJobEvents builds the case's event sequence in ingest order, every event
// carrying a payload big enough that a real cache has to shed it.
func largeJobEvents(tc largeJobCase) []model.Event {
	arrival := time.Date(2026, 8, 30, 8, 14, 2, 0, time.UTC)

	out := make([]model.Event, 0, tc.returning+1)

	if tc.sawNew {
		out = append(out, model.Event{
			Arrival:       arrival,
			Kind:          model.KindNew,
			JID:           largeJobJID,
			Fun:           "state.apply",
			Tag:           "salt/job/" + largeJobJID + "/new",
			MasterTrimmed: tc.masterTrimmed,
			Payload:       bytes.Repeat([]byte("x"), newPayloadBytes),
		})
	}

	for i := range tc.returning {
		retcode := 0
		if i < tc.failing {
			retcode = 1
		}

		out = append(out, model.Event{
			Arrival: arrival.Add(time.Duration(i+1) * time.Millisecond),
			Kind:    model.KindRet,
			JID:     largeJobJID,
			Minion:  largeJobMinion(i),
			Fun:     "state.apply",
			RetCode: retcode,
			Success: retcode == 0,
			HasRet:  true,
			Tag:     fmt.Sprintf("salt/job/%s/ret/%s", largeJobJID, largeJobMinion(i)),
			Payload: bytes.Repeat([]byte("y"), retPayloadBytes),
		})
	}

	return out
}

// largeJobMetadataBytes is what these events cost the cache once every payload
// is gone. Used as a budget, it forces shedding to completion while leaving no
// room for — and no need of — dropping a whole event.
func largeJobMetadataBytes(events []model.Event) int64 {
	var n int64

	for _, e := range events {
		stripped := e
		stripped.Payload = nil

		n += stripped.Size()
	}

	return n
}

// largeJobTargets is the job's expected minion set, in the sorted order
// Job.Missing returns.
func largeJobTargets(n int) []string {
	out := make([]string, 0, n)
	for i := range n {
		out = append(out, largeJobMinion(i))
	}

	return out
}

func largeJobMinion(i int) string { return fmt.Sprintf("web-%04d", i) }
