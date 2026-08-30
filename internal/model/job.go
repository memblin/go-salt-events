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

	// failed and missing are maintained INCREMENTALLY, as returns and expected
	// minions arrive, rather than recomputed by walking the two maps.
	//
	// That is what makes Row O(1) and it is the whole point of Row's existence:
	// the ingest hub assembles one row per listed job on every render tick, and
	// a count that walked the maps would put O(listed jobs x minion set) back on
	// the ingest lock — 22.55 ms at 200 jobs x 1,000 minions, which is the cost
	// Row was introduced to remove.
	//
	// They are only ever written by AddReturn and AddExpected, which are the
	// only two methods that can change either answer, so they cannot drift from
	// the maps unless a third writer is added. Missing() is still computed from
	// the maps, because it returns the NAMES, and TestJobCountersMatchTheMaps
	// pins the two against each other over a randomised sequence.
	failed  int
	missing int
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
	if _, dup := j.expected[minion]; dup {
		return
	}

	j.expected[minion] = struct{}{}

	// A return can arrive before the job/new that announced the target, so a
	// newly expected minion is only missing if it has not already answered.
	if _, answered := j.returns[minion]; !answered {
		j.missing++
	}
}

// AddReturn records a minion's return, replacing any earlier one. A minion can
// return twice for the same JID; counting both would render "849/847", which
// reads as a bug in this tool rather than a fact about the job.
func (j *Job) AddReturn(r RetInfo) {
	prev, repeat := j.returns[r.Minion]

	if repeat && prev.failed() {
		j.failed--
	}

	if r.failed() {
		j.failed++
	}

	// Only the FIRST return for a minion closes it out of the missing set; a
	// replacement return would otherwise drive the count negative.
	if !repeat {
		if _, targeted := j.expected[r.Minion]; targeted {
			j.missing--
		}
	}

	j.returns[r.Minion] = r

	if r.Arrival.After(j.LastRet) {
		j.LastRet = r.Arrival
	}
}

// failed reports whether this return is a failure. Salt says so two ways and
// either one counts.
func (r RetInfo) failed() bool { return r.RetCode != 0 || !r.Success }

// Returned is the count of distinct minions that have returned.
func (j *Job) Returned() int { return len(j.returns) }

// Failed counts returns with a non-zero retcode or success=false.
func (j *Job) Failed() int { return j.failed }

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

// Clone returns a deep copy that shares no state with j.
//
// This exists for the ingest hub (cmd/salt-events). A Job is mutated in place
// by the reader goroutine as returns arrive, so handing the live pointer to the
// UI would let a pane read `returns` while ingest writes it — a concurrent map
// access, which crashes the process outright rather than merely racing. The
// snapshot the UI renders from must therefore carry copies (spec §4.1).
//
// It lives here because the two sets are unexported: no caller outside this
// package can reconstruct `expected` — Missing() only reveals the part of it
// that has not returned — so a copy assembled from the accessors would silently
// invent a denominator, which is exactly what invariant 10 forbids.
func (j *Job) Clone() *Job {
	out := *j

	out.expected = make(map[string]struct{}, len(j.expected))
	for m := range j.expected {
		out.expected[m] = struct{}{}
	}

	out.returns = make(map[string]RetInfo, len(j.returns))
	for m, r := range j.returns {
		out.returns[m] = r
	}

	return &out
}

// Complete reports whether every expected minion has returned. It is false
// whenever the expected set is unknown — an unknown denominator can never
// prove completeness.
func (j *Job) Complete() bool {
	return j.ExpectedState == ExpectedKnown && j.missing == 0
}

// JobRow is a Job reduced to exactly what the job LIST renders: identity,
// timing and counts. It carries no minion sets.
//
// It exists because those two facts are in tension on the ingest lock. A Job is
// mutated in place by the reader goroutine as returns arrive, so the UI can
// only ever be handed a copy — and copying a Job means copying both of its
// minion maps, which for a screen of two hundred fleet-wide jobs is hundreds of
// thousands of map inserts on every one of the ten ticks a second, all of it
// inside the ingest mutex (measured: 22.55 ms per tick at 200 jobs x 1,000
// minions, against a 100 ms tick). The list never renders a minion NAME: it
// renders returned/expected, a failure count and a duration. The per-minion
// breakdown is the drill-down, which resolves ONE job on demand through a full
// Clone. So the list is given rows and the drill-down is given jobs, and the
// tick's cost stops scaling with the size of the index (invariant 6).
//
// Every field is a value type, so a JobRow cannot alias anything ingest still
// mutates. That is a stronger guarantee than Clone provides, not a weaker one:
// there is no map to share by accident, and no future reference-typed field can
// be added without changing this type's documented contract.
type JobRow struct {
	JID     string
	Fun     string
	Tgt     string
	TgtType string
	User    string

	Start   time.Time
	LastRet time.Time

	// Returned and Failed are plain counts and are always meaningful: they
	// describe returns we actually saw, which needs no denominator.
	Returned int
	Failed   int

	// Complete is Job.Complete: false whenever the expected set is unknown,
	// because an unknown denominator can never prove completeness.
	Complete bool

	// expected and expectedState are unexported for the same reason
	// Job.ExpectedCount returns a pair: a bare count field could be read
	// without checking whether it means anything, and "0 expected" renders as a
	// job that targeted nothing (invariant 10). ExpectedCount is the only way
	// to read them and it refuses to answer unless the state is Known.
	expected      int
	expectedState ExpectedState
}

// ExpectedCount mirrors Job.ExpectedCount, with the same obligation on the
// caller: branch on the state rather than using the count unconditionally.
func (r JobRow) ExpectedCount() (int, ExpectedState) {
	if r.expectedState != ExpectedKnown {
		return 0, r.expectedState
	}

	return r.expected, ExpectedKnown
}

// Row reduces j to its list row. It is O(1): every count it reports is either a
// map length or one of the incrementally maintained counters.
func (j *Job) Row() JobRow {
	return JobRow{
		JID:           j.JID,
		Fun:           j.Fun,
		Tgt:           j.Tgt,
		TgtType:       j.TgtType,
		User:          j.User,
		Start:         j.Start,
		LastRet:       j.LastRet,
		Returned:      len(j.returns),
		Failed:        j.failed,
		Complete:      j.Complete(),
		expected:      len(j.expected),
		expectedState: j.ExpectedState,
	}
}
