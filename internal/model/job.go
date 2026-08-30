package model

import (
	"sort"
	"time"
)

// ExpectedState records why a job's expected-minion count is or is not known.
// There are three states, not two: "the master trimmed the list" and "we never
// saw the job" have different fixes, and collapsing them sends an operator
// hunting for a job that started before they attached when the real fix is a
// master config change (spec §5.3 case B).
type ExpectedState uint8

// Expected state values: Unseen (zero, unknown), Known (full minion list), or Trimmed (master truncated the list).
const (
	// ExpectedUnseen is the zero value on purpose: a job we have only ever
	// seen returns for starts out not knowing its denominator.
	ExpectedUnseen ExpectedState = iota
	ExpectedKnown
	ExpectedTrimmed
)

// RetInfo is one minion's return for a job.
type RetInfo struct {
	Minion  string
	RetCode int
	Success bool
	Arrival time.Time
}

// Job is the correlation of a salt/job/<jid>/new event with its
// salt/job/<jid>/ret/<minion> events (spec §7.5).
type Job struct {
	JID     string
	Fun     string
	Tgt     string
	TgtType string
	User    string

	Start   time.Time
	LastRet time.Time

	ExpectedState ExpectedState

	expected map[string]struct{}
	returns  map[string]RetInfo
}

// NewJob creates an empty job. ExpectedState starts at ExpectedUnseen because
// a job is frequently observed via its returns before (or without) its new event.
func NewJob(jid string) *Job {
	return &Job{
		JID:      jid,
		expected: make(map[string]struct{}),
		returns:  make(map[string]RetInfo),
	}
}

// AddExpected records a minion the job targeted.
func (j *Job) AddExpected(minion string) {
	j.expected[minion] = struct{}{}
}

// AddReturn records a minion's return, replacing any earlier one. A minion can
// return twice for the same JID; counting both would render "849/847", which
// reads as a bug in this tool rather than a fact about the job.
func (j *Job) AddReturn(r RetInfo) {
	j.returns[r.Minion] = r

	if r.Arrival.After(j.LastRet) {
		j.LastRet = r.Arrival
	}
}

// Returned is the count of distinct minions that have returned.
func (j *Job) Returned() int { return len(j.returns) }

// Failed counts returns with a non-zero retcode or success=false.
func (j *Job) Failed() int {
	n := 0

	for _, r := range j.returns {
		if r.RetCode != 0 || !r.Success {
			n++
		}
	}

	return n
}

// ExpectedCount reports how many minions were targeted, and whether that
// number can be trusted. Callers MUST branch on the state rather than using
// the count unconditionally (invariant 10).
func (j *Job) ExpectedCount() (int, ExpectedState) {
	if j.ExpectedState != ExpectedKnown {
		return 0, j.ExpectedState
	}

	return len(j.expected), ExpectedKnown
}

// Missing returns the minions that were expected but have not returned, sorted.
//
// ok is false when the expected set is not known — the caller must render that
// as "unknown", never as zero. "0 missing" reads as "everything returned",
// which is the most dangerous wrong answer this tool can produce (invariant 10).
func (j *Job) Missing() ([]string, bool) {
	if j.ExpectedState != ExpectedKnown {
		return nil, false
	}

	out := make([]string, 0, len(j.expected))

	for m := range j.expected {
		if _, done := j.returns[m]; !done {
			out = append(out, m)
		}
	}

	sort.Strings(out)

	return out, true
}

// Returns yields every recorded return, sorted by minion, for the drill-down.
func (j *Job) Returns() []RetInfo {
	out := make([]RetInfo, 0, len(j.returns))
	for _, r := range j.returns {
		out = append(out, r)
	}

	sort.Slice(out, func(a, b int) bool { return out[a].Minion < out[b].Minion })

	return out
}

// Complete reports whether every expected minion has returned. It is false
// whenever the expected set is unknown — an unknown denominator can never
// prove completeness.
func (j *Job) Complete() bool {
	missing, ok := j.Missing()

	return ok && len(missing) == 0
}
