// Package cache holds the session's events in a memory-bounded ring.
//
// It degrades rather than growing without limit, because this program runs as
// root on a production salt-master and a single highstate against a thousand
// minions can produce a gigabyte of returns (spec §5.2).
package cache

import (
	"github.com/TKC-Labs/go-salt-events/internal/model"
)

// Matcher decides whether an event belongs in a snapshot.
//
// This is the seam between the cache and internal/filter, and it points this
// way round on purpose: the cache does NOT import internal/filter. Keeping the
// abstraction here is what lets the ring be tested without a query parser, and
// it means a compiled *filter.Query satisfies Snapshot's parameter without the
// two packages knowing anything about each other beyond this one method.
//
// A nil Matcher means "match everything", so the unfiltered view costs no
// wrapper and the UI need not synthesise a match-all query.
type Matcher interface {
	Match(model.Event) bool
}

// Stats is the budget readout the status bar renders.
//
// Shed and Dropped are separate counters because they are different degrees of
// loss and the bar must not blur them (spec §5.2): a shed event still answers
// "what happened and when", a dropped one is gone entirely.
type Stats struct {
	// Used is the sum of Size() over every retained event; Budget is the
	// ceiling it is held under.
	Used   int64
	Budget int64

	// Events is how many events are currently retained.
	Events int

	// Shed counts payloads this cache discarded to stay in budget, over the
	// life of the session. It is not decremented when the event is later
	// dropped, because it reports cumulative loss, not current state.
	Shed int

	// Dropped counts whole events this cache discarded, over the life of the
	// session.
	Dropped int
}

// Cache is a memory-bounded ring of events, newest last.
//
// Cache is NOT safe for concurrent use. The architecture serialises access
// externally: a reader goroutine folds each event into the cache and the stats
// aggregator under the hub's mutex (spec §4.1, Task 21), and the UI reads a
// snapshot assembled under that same lock. Do not add a mutex here — that
// would nest a second lock inside one already held on every ingest, and it
// would still not make a read-modify-write across cache and stats atomic,
// which is the property the hub's single lock actually provides.
type Cache struct {
	budget int64
	used   int64

	events []model.Event

	// shedIdx is the index of the next event whose payload will be shed. It
	// only moves forward, so shedding is O(1) amortised rather than a scan.
	shedIdx int

	shed    int
	dropped int
}

// New returns a Cache bounded by budget bytes.
//
// A non-positive budget is accepted rather than rejected: it degrades to a
// cache that retains nothing, which is a bad configuration but not a crash.
// Refusing it here would turn a typo in a config file into a failure to start.
func New(budget int64) *Cache {
	return &Cache{
		budget:  budget,
		used:    0,
		events:  nil,
		shedIdx: 0,
		shed:    0,
		dropped: 0,
	}
}

// Add folds one event into the cache, degrading as needed to stay in budget.
//
// The cache takes ownership of e.Payload's backing array and may drop its
// reference; callers must not retain or mutate it afterwards.
func (c *Cache) Add(e model.Event) {
	c.events = append(c.events, e)
	c.used += e.Size()

	c.degrade()
}

// degrade brings the cache back under budget.
//
// Order matters and is the spec's, not an implementation detail: shed payloads
// oldest-first FIRST, and only drop whole events once shedding is exhausted.
// Tag and timing are what most questions need and they are tiny; the payload
// is the expensive, rarely-read part.
//
// Invariant 9 depends on this: shedding must never touch the indexed fields,
// because job counts and every aggregate read only those. An implementation
// that cleared, say, Minion alongside Payload would leave the Jobs pane
// rendering plausible, wrong numbers instead of failing visibly (spec §5.2).
func (c *Cache) degrade() {
	for c.used > c.budget && c.shedIdx < len(c.events) {
		e := &c.events[c.shedIdx]

		if e.Payload != nil {
			c.used -= int64(len(e.Payload))
			e.Payload = nil

			// Shed is OURS. MasterTrimmed is Salt's, and is left untouched in
			// both directions — never set, never cleared. Identical symptom,
			// opposite fixes: raise --max-memory versus raise max_event_size
			// on the master (spec §5.3 case A).
			e.Shed = true
			c.shed++
		}

		c.shedIdx++
	}

	// Only reached once every retained payload is already gone, so a dropped
	// event costs nothing but its tag and indexed fields.
	for c.used > c.budget && len(c.events) > 0 {
		c.used -= c.events[0].Size()
		c.events = c.events[1:]
		c.dropped++

		if c.shedIdx > 0 {
			c.shedIdx--
		}
	}
}

// scanBudget is how far back Snapshot looks for matches, as a multiple of the
// caller's limit.
//
// It is what makes the scan O(limit) rather than O(cache), and it exists
// because the caller (the hub) runs Snapshot under the INGEST mutex: every
// event examined here is an event the reader goroutine is blocked for, ten
// times a second. Without the bound, a filter matching few or no retained
// events walked the entire ring on every tick — 22.1 ms of blocked ingest at
// the default 256 MiB budget and roughly 88 ms at --max-memory 1G, which is
// the whole render interval. That is precisely the storm-wedging failure
// invariant 6 exists to prevent, and it appeared in the one situation the
// console exists for: an operator narrowing the filter during a storm.
//
// 8 is a deliberate compromise, not a tuned number. It is large enough that
// the bound never bites on a filter that matches anything like the viewport's
// worth of recent events, and small enough that the worst case stays under a
// millisecond. Raising it trades ingest headroom for reach; the honest way to
// widen the reach is the export (`w`), which scans everything.
const scanBudget = 8

// Snapshot returns up to limit events matching m, oldest first, taking the
// newest matches when there are more than limit of them, together with how
// many retained events it examined. A nil m matches everything; a limit of
// zero or less returns nothing.
//
// It copies, so the UI can render from it without holding the hub's lock —
// which is what keeps render cost O(visible rows) and independent of ingest
// rate (spec §4.1, invariant 6). The copy is shallow: Payload's bytes are
// shared with the cache and must be treated as read-only.
//
// # The scan is bounded, and that is visible
//
// The walk stops after scanBudget × limit events even if it has not filled
// limit, so the cost of a tick is a function of the viewport and not of the
// cache. That is a real loss of reach: a matching event further back than the
// budget will not appear, even though it is still retained and will still be
// exported. The returned count is how a caller tells the two apart — equal to
// Stats().Events means the scan reached the oldest retained event and the view
// is complete for this query; less means "the filter looked back this far".
// Drawing an empty pane without saying which of those happened would read as
// "there are no such events", which is a different and much worse message.
func (c *Cache) Snapshot(m Matcher, limit int) ([]model.Event, int) {
	if limit <= 0 {
		return nil, 0
	}

	out := make([]model.Event, 0, min(limit, len(c.events)))

	// limit reaches this from a caller, so the multiplication is checked: a
	// limit near maxInt would wrap to a negative budget and return nothing at
	// all, turning a silly argument into an empty console.
	budget := limit * scanBudget
	if budget < limit || budget > len(c.events) {
		budget = len(c.events)
	}

	scanned := 0

	for i := len(c.events) - 1; i >= 0 && len(out) < limit && scanned < budget; i-- {
		scanned++

		if m == nil || m.Match(c.events[i]) {
			out = append(out, c.events[i])
		}
	}

	// Reverse into oldest-first order for display.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}

	return out, scanned
}

// All returns every retained event, oldest first. Used by export and tests.
//
// As with Snapshot the copy is shallow, so Payload's bytes must be treated as
// read-only; the slice itself is the caller's.
func (c *Cache) All() []model.Event {
	out := make([]model.Event, len(c.events))
	copy(out, c.events)

	return out
}

// Stats returns the budget readout. It is O(1): nothing here walks the ring,
// and nothing here computes a statistic — those are fed at ingest and are
// never derived from the cache (spec §5.4, invariant 3).
func (c *Cache) Stats() Stats {
	return Stats{
		Used:    c.used,
		Budget:  c.budget,
		Events:  len(c.events),
		Shed:    c.shed,
		Dropped: c.dropped,
	}
}
