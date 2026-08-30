package stats

import (
	"container/list"

	"github.com/TKC-Labs/go-salt-events/internal/model"
)

// Lookup reports why a job is or is not available.
//
// LookupEvicted and LookupUnseen must stay distinct: the first is fixed by
// raising --max-jobs, the second by attaching sooner. Collapsing them sends
// the operator after the wrong thing, the same failure mode as the three
// expected-count states (spec §7.5).
type Lookup uint8

// Lookup values: Unseen (never observed), Found, or Evicted (observed, then dropped under pressure).
const (
	LookupUnseen Lookup = iota
	LookupFound
	LookupEvicted
)

func (l Lookup) String() string {
	switch l {
	case LookupFound:
		return "found"
	case LookupEvicted:
		return "evicted"
	case LookupUnseen:
		return "unseen"
	default:
		return "unseen"
	}
}

// IndexStats is the pressure readout the Jobs pane header renders.
//
// It exists because --max-jobs defaults to a value chosen to be safe rather
// than sufficient: the design's job is not to guess the right number but to
// make a wrong one visible so it can be tuned from evidence (spec §7.5).
type IndexStats struct {
	Tracked   int
	Cap       int
	HighWater int
	Evicted   uint64
}

// perMinionBytes is a rough accounting weight for the memory ceiling: one
// short string per target, plus map overhead.
const perMinionBytes = 64

// The index remembers evictedFactor × maxJobs recent evictions, never fewer
// than minEvictedMemory. A JID is ~20 bytes, so the default cap costs a few
// hundred KB — cheap enough to keep "evicted" answerable for far longer than
// the index itself holds jobs, and bounded so an hours-long session on a busy
// master cannot turn the memory of evictions into the very leak the index
// exists to prevent.
const (
	evictedFactor    = 8
	minEvictedMemory = 64
)

// JobIndex correlates job/new events with job/ret events.
//
// It is capped independently of the event cache so a payload storm cannot
// evict job state and job state cannot crowd out events (spec §7.5).
//
// JobIndex is NOT safe for concurrent use — concurrent access to its maps
// would crash outright, not just race. The architecture serialises access
// externally: a reader goroutine feeds it under the hub's mutex (Task 21),
// and the UI reads a snapshot assembled under that same lock. Do not add a
// mutex here — that would nest a second lock inside one already held on
// every ingest.
type JobIndex struct {
	maxJobs    int
	memCeiling int64

	// clock is held, not yet read: the Jobs pane's `dur` column counts up
	// live from the `new` event for a job that is still returning (spec §7.5),
	// and that reading must come from the injected clock so no test in this
	// package ever sleeps.
	clock Clock

	jobs  map[string]*list.Element
	order *list.List // front = oldest, back = newest, by first observation

	pinned string

	evicted *evictedSet

	stats IndexStats
	bytes int64
}

// entry is one job in the eviction-ordered list, alongside the byte weight
// currently charged against the ceiling for it. Carrying the weight here (as
// opposed to re-deriving it at eviction time) is what keeps the accounting
// exact: a minion that returns twice mutates the job in place, so charging
// per call would drift the total upward forever and evict live jobs for
// memory the index is not using.
type entry struct {
	job   *model.Job
	bytes int64
}

// NewJobIndex returns an index holding at most maxJobs jobs, additionally
// bounded by memCeiling bytes.
//
// The count is the knob an operator tunes; the ceiling exists only so raising
// the count cannot be turned into an OOM. A memCeiling of zero or less
// disables the ceiling rather than shrinking the index to nothing: a caller
// that forgot to derive it should lose the backstop, not the pane.
func NewJobIndex(maxJobs int, memCeiling int64, c Clock) *JobIndex {
	return &JobIndex{
		maxJobs:    maxJobs,
		memCeiling: memCeiling,
		clock:      c,
		jobs:       make(map[string]*list.Element),
		order:      list.New(),
		evicted:    newEvictedSet(evictedCapacity(maxJobs)),
	}
}

// Observe folds one event into the index.
//
// expected is the decoded `minions` list from a job/new payload, and
// expectedTrimmed reports that Salt replaced that list with VALUE_TRIMMED.
// Both are supplied by the caller because decoding belongs at the ingest
// boundary, not here (invariant 4).
//
// It reads ONLY indexed fields — never a cached payload. That is what keeps
// invariant 9 true: shedding payloads cannot change a job count.
func (idx *JobIndex) Observe(e model.Event, expected []string, expectedTrimmed bool) {
	switch e.Kind {
	case model.KindNew:
		idx.observeNew(e, expected, expectedTrimmed)
	case model.KindRet:
		idx.observeRet(e)
	case model.KindOther, model.KindProg, model.KindStart,
		model.KindAuth, model.KindKey, model.KindPresence:
	}
}

// observeNew records a job's publication and, when the master gave us one, its
// expected minion set.
func (idx *JobIndex) observeNew(e model.Event, expected []string, expectedTrimmed bool) {
	if e.JID == "" {
		return
	}

	el := idx.touch(e.JID)
	job := entryOf(el).job

	job.Fun = e.Fun
	job.Start = e.Arrival

	// A `new` event that carries neither a list nor a trim marker leaves the
	// state alone: a second sighting of the same job must never downgrade a
	// known denominator back to unknown (spec §5.3 case B).
	switch {
	case expectedTrimmed:
		job.ExpectedState = model.ExpectedTrimmed

	case expected != nil:
		job.ExpectedState = model.ExpectedKnown

		for _, m := range expected {
			job.AddExpected(m)
		}
	}

	idx.settle(el)
}

// observeRet records one minion's return.
//
// A return without a minion is dropped rather than counted: it would inflate
// the numerator of ret/expected against a minion we cannot name, and the JID
// alone is not enough to create a job worth showing.
func (idx *JobIndex) observeRet(e model.Event) {
	if e.JID == "" || e.Minion == "" {
		return
	}

	el := idx.touch(e.JID)
	job := entryOf(el).job

	job.AddReturn(model.RetInfo{
		Minion:  e.Minion,
		RetCode: e.RetCode,
		Success: e.Success,
		Arrival: e.Arrival,
	})

	// Returns routinely arrive before (or without) the `new` event when we
	// attach to a master mid-job, so this is the only chance to learn `fun`.
	if job.Fun == "" {
		job.Fun = e.Fun
	}

	idx.settle(el)
}

// touch returns the list element for jid, creating it and recording the
// high-water mark if it is new.
func (idx *JobIndex) touch(jid string) *list.Element {
	if el, ok := idx.jobs[jid]; ok {
		return el
	}

	el := idx.order.PushBack(&entry{job: model.NewJob(jid)})
	idx.jobs[jid] = el

	if tracked := len(idx.jobs); tracked > idx.stats.HighWater {
		idx.stats.HighWater = tracked
	}

	return el
}

// settle re-weighs a job after it was mutated and evicts if the index is now
// over either bound.
func (idx *JobIndex) settle(el *list.Element) {
	en := entryOf(el)

	weight := jobBytes(en.job)
	idx.bytes += weight - en.bytes
	en.bytes = weight

	idx.evict()
}

// jobBytes is a job's accounting weight: one short string per expected target
// and per recorded return.
//
// The expected half composes through Job.ExpectedCount rather than reaching
// for the set directly, so it inherits that method's invariant-10 gate: an
// unknown denominator weighs nothing here for the same reason it renders as
// "?" rather than "0" (spec §5.3).
func jobBytes(j *model.Job) int64 {
	expected, _ := j.ExpectedCount()

	return int64(expected+j.Returned()) * perMinionBytes
}

// evict drops jobs, oldest first, until the index fits both bounds.
func (idx *JobIndex) evict() {
	for idx.overCapacity() {
		victim := idx.oldestEvictable()
		if victim == nil {
			return
		}

		idx.drop(victim)
	}
}

// overCapacity reports whether either bound is exceeded. A non-positive
// ceiling means "no ceiling" (see NewJobIndex).
func (idx *JobIndex) overCapacity() bool {
	if len(idx.jobs) > idx.maxJobs {
		return true
	}

	return idx.memCeiling > 0 && idx.bytes > idx.memCeiling
}

// oldestEvictable picks a victim: the oldest complete job, or — only when no
// complete job exists at all — the oldest incomplete one. The pinned job is
// never a victim.
//
// The preference for complete jobs is spec §7.5: a job still receiving returns
// is the one most likely being watched. The fallback exists because
// Job.Complete() is false whenever the expected set is unknown, and attaching
// to a busy master mid-stream produces nothing but such jobs — every in-flight
// job's `new` event is already gone. Without the fallback that ordinary case
// would make --max-jobs and the ceiling unenforceable and grow the index
// without bound, which is a worse outcome than evicting: an eviction is
// counted, reported in IndexStats, and answered honestly by Job() as
// LookupEvicted, whereas an OOM is silent.
func (idx *JobIndex) oldestEvictable() *list.Element {
	var fallback *list.Element

	for el := idx.order.Front(); el != nil; el = el.Next() {
		job := entryOf(el).job

		if job.JID == idx.pinned {
			continue
		}

		if job.Complete() {
			return el
		}

		if fallback == nil {
			fallback = el
		}
	}

	return fallback
}

// drop removes one job from the index and records the eviction.
func (idx *JobIndex) drop(el *list.Element) {
	en := entryOf(el)

	idx.order.Remove(el)
	delete(idx.jobs, en.job.JID)
	idx.evicted.add(en.job.JID)

	idx.bytes -= en.bytes
	if idx.bytes < 0 {
		idx.bytes = 0
	}

	idx.stats.Evicted++
}

// Pin marks a job as never-evictable. The UI pins whatever the operator has
// drilled into.
func (idx *JobIndex) Pin(jid string) { idx.pinned = jid }

// Unpin clears the pin.
func (idx *JobIndex) Unpin() { idx.pinned = "" }

// Job looks up a job and reports why it is unavailable when it is.
func (idx *JobIndex) Job(jid string) (*model.Job, Lookup) {
	if el, ok := idx.jobs[jid]; ok {
		return entryOf(el).job, LookupFound
	}

	if idx.evicted.has(jid) {
		return nil, LookupEvicted
	}

	return nil, LookupUnseen
}

// List returns the n most recent jobs, newest first.
func (idx *JobIndex) List(n int) []*model.Job {
	if n <= 0 {
		return nil
	}

	if n > len(idx.jobs) {
		n = len(idx.jobs)
	}

	out := make([]*model.Job, 0, n)

	for el := idx.order.Back(); el != nil && len(out) < n; el = el.Prev() {
		out = append(out, entryOf(el).job)
	}

	return out
}

// Stats returns the pressure readout.
func (idx *JobIndex) Stats() IndexStats {
	s := idx.stats
	s.Cap = idx.maxJobs
	s.Tracked = len(idx.jobs)

	return s
}

// entryOf unwraps a list element. The list is private to this file and only
// ever holds *entry, so the assertion cannot fail on bus data.
func entryOf(el *list.Element) *entry { return el.Value.(*entry) }

// evictedCapacity sizes the eviction memory from the index cap.
func evictedCapacity(maxJobs int) int {
	if maxJobs > minEvictedMemory/evictedFactor {
		return maxJobs * evictedFactor
	}

	return minEvictedMemory
}

// evictedSet remembers recently evicted JIDs so Job() can tell "evicted from
// the index — raise --max-jobs" apart from "never seen — attach sooner"
// (spec §7.5).
//
// It is a bounded FIFO, not a growing set. Past the horizon a very old
// eviction degrades to LookupUnseen; that is a deliberate trade of precision
// on ancient history for a hard memory bound, since an unbounded set would
// reintroduce at this level exactly the leak the index caps at the level below.
type evictedSet struct {
	cap   int
	seen  map[string]struct{}
	order []string
	next  int
}

// newEvictedSet returns a FIFO remembering at most capacity JIDs.
func newEvictedSet(capacity int) *evictedSet {
	if capacity < 1 {
		capacity = 1
	}

	return &evictedSet{
		cap:   capacity,
		seen:  make(map[string]struct{}, capacity),
		order: make([]string, 0, capacity),
	}
}

// add records jid, forgetting the oldest remembered eviction when full.
func (s *evictedSet) add(jid string) {
	if _, known := s.seen[jid]; known {
		return
	}

	if len(s.order) < s.cap {
		s.order = append(s.order, jid)
	} else {
		delete(s.seen, s.order[s.next])
		s.order[s.next] = jid
		s.next = (s.next + 1) % s.cap
	}

	s.seen[jid] = struct{}{}
}

// has reports whether jid is a remembered eviction.
func (s *evictedSet) has(jid string) bool {
	_, ok := s.seen[jid]

	return ok
}
