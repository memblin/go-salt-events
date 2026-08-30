package model_test

import (
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
