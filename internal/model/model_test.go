package model_test

import (
	"strconv"
	"testing"
	"time"

	"github.com/TKC-Labs/go-salt-events/internal/model"
)

func TestJobMissingIsOnlyReportedWhenExpectedIsKnown(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		state       model.ExpectedState
		expected    []string
		returned    []string
		wantMissing []string
		wantOK      bool
	}{
		{
			name:        "known expected set yields the difference",
			state:       model.ExpectedKnown,
			expected:    []string{"web-1", "web-2", "web-3"},
			returned:    []string{"web-1", "web-3"},
			wantMissing: []string{"web-2"},
			wantOK:      true,
		},
		{
			// Invariant 10: never fabricate a missing count. Returning
			// ok=false is what stops the pane rendering "0 missing", which
			// reads as "everything returned" — the most dangerous wrong
			// answer this tool can give (spec §5.3 case B).
			name:        "master-trimmed expected set is not computable",
			state:       model.ExpectedTrimmed,
			expected:    nil,
			returned:    []string{"web-1"},
			wantMissing: nil,
			wantOK:      false,
		},
		{
			name:        "never-seen expected set is not computable",
			state:       model.ExpectedUnseen,
			expected:    nil,
			returned:    []string{"web-1"},
			wantMissing: nil,
			wantOK:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			j := model.NewJob("20260830081402123456")
			j.ExpectedState = tt.state

			for _, m := range tt.expected {
				j.AddExpected(m)
			}

			for _, m := range tt.returned {
				j.AddReturn(model.RetInfo{Minion: m, Success: true, Arrival: time.Now()})
			}

			got, ok := j.Missing()
			if ok != tt.wantOK {
				t.Fatalf("Missing() ok = %v, want %v", ok, tt.wantOK)
			}

			if len(got) != len(tt.wantMissing) {
				t.Fatalf("Missing() = %v, want %v", got, tt.wantMissing)
			}

			for i := range got {
				if got[i] != tt.wantMissing[i] {
					t.Errorf("Missing()[%d] = %q, want %q", i, got[i], tt.wantMissing[i])
				}
			}
		})
	}
}

func TestJobReturnsAreDeduplicatedByMinion(t *testing.T) {
	t.Parallel()

	// A minion can return twice for the same JID (a retry, or a duplicate on
	// the bus). Counting it twice would push Returned() past ExpectedCount()
	// and render "849/847", which reads as a bug in the tool.
	j := model.NewJob("20260830081402123456")
	j.ExpectedState = model.ExpectedKnown
	j.AddExpected("web-1")

	j.AddReturn(model.RetInfo{Minion: "web-1", Success: true, Arrival: time.Now()})
	j.AddReturn(model.RetInfo{Minion: "web-1", Success: true, Arrival: time.Now()})

	if got := j.Returned(); got != 1 {
		t.Errorf("Returned() = %d, want 1", got)
	}
}

func TestJobFailedCountsRetcodeAndSuccess(t *testing.T) {
	t.Parallel()

	j := model.NewJob("20260830081402123456")
	j.AddReturn(model.RetInfo{Minion: "a", RetCode: 0, Success: true})
	j.AddReturn(model.RetInfo{Minion: "b", RetCode: 1, Success: true})
	j.AddReturn(model.RetInfo{Minion: "c", RetCode: 0, Success: false})

	if got := j.Failed(); got != 2 {
		t.Errorf("Failed() = %d, want 2", got)
	}
}

// TestJobCloneSharesNothingWithTheOriginal is the guard on the one property
// cmd/salt-events depends on: the UI renders from clones while the reader
// goroutine keeps mutating the originals, so a clone that shared either map
// would be a concurrent map access — a crash, not a race.
func TestJobCloneSharesNothingWithTheOriginal(t *testing.T) {
	t.Parallel()

	j := model.NewJob("20260830081402123456")
	j.ExpectedState = model.ExpectedKnown
	j.Fun = "state.apply"
	j.AddExpected("web-1")
	j.AddExpected("web-2")
	j.AddReturn(model.RetInfo{Minion: "web-1", Success: true})

	clone := j.Clone()

	// Everything the original knew must survive the copy, including the
	// denominator: a clone that lost it would render 1/? for a job we hold a
	// full expected set for (invariant 10, in reverse).
	if n, state := clone.ExpectedCount(); n != 2 || state != model.ExpectedKnown {
		t.Errorf("clone ExpectedCount() = (%d, %v), want (2, known)", n, state)
	}

	if clone.Fun != "state.apply" || clone.Returned() != 1 {
		t.Errorf("clone lost scalar state: fun=%q returned=%d", clone.Fun, clone.Returned())
	}

	// Now mutate the original the way ingest does.
	j.AddExpected("web-3")
	j.AddReturn(model.RetInfo{Minion: "web-2", Success: false})

	if n, _ := clone.ExpectedCount(); n != 2 {
		t.Errorf("clone expected set = %d after the original grew, want 2", n)
	}

	if got := clone.Returned(); got != 1 {
		t.Errorf("clone Returned() = %d after the original grew, want 1", got)
	}

	if got := clone.Failed(); got != 0 {
		t.Errorf("clone Failed() = %d after the original gained a failure, want 0", got)
	}
}

// TestJobCountersMatchTheMapsUnderAnAdversarialSequence is the guard on the
// incremental counters behind Failed, Complete and Row.
//
// Failed and Complete used to walk the two maps on every call, which is
// self-evidently consistent and was also the cost that made a snapshot hold the
// ingest lock for 22 ms. They are now maintained as events arrive, and the
// price of that is that they can DRIFT — silently, and in the direction that
// matters, since Complete decides whether a job stops counting its duration up
// and whether the index may evict it.
//
// So the counters are re-derived from the maps after every single step of a
// deliberately hostile sequence: returns before their job/new, duplicate
// targets, a repeat return that flips a failure into a success and back, and a
// return for a minion that was never targeted.
func TestJobCountersMatchTheMapsUnderAnAdversarialSequence(t *testing.T) {
	t.Parallel()

	type step struct {
		name string
		do   func(*model.Job)
	}

	ret := func(minion string, code int) step {
		return step{
			name: minion + " returns " + strconv.Itoa(code),
			do: func(j *model.Job) {
				j.AddReturn(model.RetInfo{
					Minion: minion, RetCode: code, Success: code == 0,
					Arrival: time.Unix(1_800_000_000, 0),
				})
			},
		}
	}

	exp := func(minion string) step {
		return step{
			name: "target " + minion,
			do:   func(j *model.Job) { j.AddExpected(minion) },
		}
	}

	steps := []step{
		ret("web-1", 0), // a return arriving before the job/new
		exp("web-1"),    // ... whose target is only learned afterwards
		exp("web-2"),    //
		exp("web-2"),    // duplicate target: must not double-count missing
		ret("web-2", 1), // a failure
		ret("web-2", 0), // the same minion returns again, now succeeding
		ret("web-2", 2), // and again, failing
		ret("web-3", 1), // a return from a minion that was never targeted
		exp("web-4"),    // still missing at the end
		ret("web-1", 3), // a repeat return that turns a success into a failure
	}

	job := model.NewJob("20260830081402123456")
	job.ExpectedState = model.ExpectedKnown

	for _, s := range steps {
		s.do(job)

		wantFailed := 0
		for _, r := range job.Returns() {
			if r.RetCode != 0 || !r.Success {
				wantFailed++
			}
		}

		if got := job.Failed(); got != wantFailed {
			t.Fatalf("after %q: Failed() = %d, want %d (recomputed from Returns())",
				s.name, got, wantFailed)
		}

		missing, known := job.Missing()
		if !known {
			t.Fatalf("after %q: Missing() reported the expected set unknown", s.name)
		}

		if got, want := job.Complete(), len(missing) == 0; got != want {
			t.Fatalf("after %q: Complete() = %v, want %v (%d still missing: %v)",
				s.name, got, want, len(missing), missing)
		}

		row := job.Row()

		if row.Failed != wantFailed || row.Returned != job.Returned() ||
			row.Complete != job.Complete() {
			t.Fatalf("after %q: Row() = %+v, disagrees with the job it came from "+
				"(failed %d, returned %d, complete %v)",
				s.name, row, wantFailed, job.Returned(), job.Complete())
		}

		n, state := row.ExpectedCount()
		jn, jstate := job.ExpectedCount()

		if n != jn || state != jstate {
			t.Fatalf("after %q: Row().ExpectedCount() = (%d, %v), want (%d, %v)",
				s.name, n, state, jn, jstate)
		}
	}
}

// TestJobRowRefusesAnUnknownDenominator is invariant 10 at the new type. A row
// is what the list renders from, so if it answered with a bare 0 the pane would
// print "3/0" — a job that over-returned — for every job whose job/new was
// never seen.
func TestJobRowRefusesAnUnknownDenominator(t *testing.T) {
	t.Parallel()

	job := model.NewJob("20260830081402123456")
	job.AddReturn(model.RetInfo{Minion: "web-1", Success: true})

	n, state := job.Row().ExpectedCount()

	if state != model.ExpectedUnseen {
		t.Errorf("Row().ExpectedCount() state = %v, want ExpectedUnseen", state)
	}

	if n != 0 {
		t.Errorf("Row().ExpectedCount() = %d for a job with no job/new, want 0 "+
			"alongside the state the caller must branch on", n)
	}

	if job.Row().Complete {
		t.Error("Row().Complete is true for a job whose expected set is unknown; " +
			"an unknown denominator can never prove completeness")
	}

	// The zero JobRow is what a test or a pane gets before the first tick. It
	// must read as "unknown", never as "targeted nobody".
	if _, zeroState := (model.JobRow{}).ExpectedCount(); zeroState != model.ExpectedUnseen {
		t.Errorf("the zero JobRow reports state %v, want ExpectedUnseen", zeroState)
	}
}
