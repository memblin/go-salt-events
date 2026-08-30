package main

import (
	"strings"
	"sync"
	"time"

	"github.com/TKC-Labs/go-salt-events/internal/cache"
	"github.com/TKC-Labs/go-salt-events/internal/filter"
	"github.com/TKC-Labs/go-salt-events/internal/model"
	"github.com/TKC-Labs/go-salt-events/internal/saltipc"
	"github.com/TKC-Labs/go-salt-events/internal/stats"
	"github.com/TKC-Labs/go-salt-events/internal/ui"
)

// maxTopKeys bounds top-N cardinality. Minion counts are bounded in practice;
// categories are bounded by salttag's normalisation. This is the backstop.
const maxTopKeys = 256

// listJobs is how many jobs the list view can show.
const listJobs = 200

// topEntries is how many ranked entries a snapshot carries.
const topEntries = 10

// jobMemFraction is the share of --max-memory the job index may use. The count
// is the knob an operator tunes; this ceiling only exists so the knob cannot be
// turned into an OOM (spec §7.5).
const jobMemFraction = 10

// maxLabelBytes bounds a job's target and user strings.
//
// Both are minion- or master-supplied and are retained for the life of the job
// index, and every visible row is sanitised and measured in full on every one
// of the ten frames drawn each second. Cutting them once, here, keeps a
// hostile 1 MiB `tgt` from costing that per frame and per job.
const maxLabelBytes = 256

// hubConfig configures the hub.
type hubConfig struct {
	MaxMemory int64
	MaxJobs   int
	Clock     stats.Clock
	Decode    func([]byte) (any, error)
}

// hub is the single place the reader, the cache, and the stats meet.
//
// It implements saltipc.Sink (written by the reader goroutine) and ui.Source
// (read by the render tick). Everything is behind ONE mutex, which is what lets
// the UI take a consistent snapshot without per-event messages (spec §4.1,
// invariant 6).
//
// # Why the lock lives here and not in the packages it guards
//
// internal/cache, internal/stats' Rings, Counter and JobIndex are all
// documented as NOT safe for concurrent use, deliberately: an internal mutex
// would nest a second lock inside one already held on every ingest, and it
// still would not make a read-modify-write ACROSS the cache and the stats
// atomic, which is the property a single lock here does provide. Concurrent map
// access on Counter.counts or on a Job's returns crashes the process outright
// rather than merely racing, so this is not a theoretical concern.
//
// # Why sync.Mutex and not sync.RWMutex
//
// Snapshot is a WRITER. Rings.Seconds/Minutes call syncToNow, which advances
// the ring epoch and clears buckets the clock has passed, so two "readers"
// running concurrently under an RLock would race on the ring array. The read
// path is once per render tick; there is nothing to gain from a shared lock and
// a data race to lose.
//
// # What escapes the lock
//
// Only values and freshly built slices. Every ranked entry, bucket window and
// cache stat is copied on the way out. The one that is NOT naturally a copy is
// a job — a *model.Job is mutated in place as returns arrive — so the snapshot
// carries model.JobRows for the list (see jobList) and JobLookup Clone()s the
// single drilled job under the lock. Handing out the live pointer would let a
// pane read a job's map while the reader goroutine writes it, which is a
// concurrent map access: a process-killing crash, not merely a race.
type hub struct {
	mu sync.Mutex

	clock stats.Clock

	cache   *cache.Cache
	rings   *stats.Rings
	cats    *stats.Counter
	minions *stats.Counter
	funs    *stats.Counter
	jobs    *stats.JobIndex

	decode func([]byte) (any, error)

	connected    bool
	decodeErrors int
}

// newHub builds the hub. A nil Clock falls back to the real one: a hub with no
// clock would stamp every snapshot with the zero time, which the Jobs pane
// reads as "no reading available" and would silently freeze every duration.
func newHub(cfg hubConfig) *hub {
	clock := cfg.Clock
	if clock == nil {
		clock = stats.RealClock{}
	}

	return &hub{
		clock:   clock,
		cache:   cache.New(cfg.MaxMemory),
		rings:   stats.NewRings(clock),
		cats:    stats.NewCounter(maxTopKeys),
		minions: stats.NewCounter(maxTopKeys),
		funs:    stats.NewCounter(maxTopKeys),
		jobs:    stats.NewJobIndex(cfg.MaxJobs, cfg.MaxMemory/jobMemFraction, clock),
		decode:  cfg.Decode,
	}
}

// Event implements saltipc.Sink. Called from the reader goroutine.
//
// It runs on every event at up to thousands per second, so everything it does
// is O(1) in the size of the cache and the index — and it never blocks on the
// UI, which only ever pulls.
func (h *hub) Event(e model.Event) {
	// The one msgpack decode at ingest runs BEFORE the lock is taken. It is the
	// most expensive thing on this path by an order of magnitude — 5.2 ms for a
	// 20,000-minion `minions` list — and it needs nothing the lock guards: it
	// reads e, which the reader goroutine owns until this returns, and h.decode,
	// which newHub sets once and nothing ever writes again. Decoding under the
	// mutex made that whole cost a stall on the render tick on a tool that runs
	// as root on a production master.
	//
	// Invariant 4 is unchanged by the move: factsFor still refuses everything
	// but job/new, so no return payload is decoded here and the per-event cost
	// of ingest is still the shallow ExtractFields pass. Hoisting must not turn
	// into decoding MORE than before, and it does not — the same events are
	// decoded, in the same place in the sequence, just not behind the lock.
	facts := h.factsFor(e)

	h.mu.Lock()
	defer h.mu.Unlock()

	// Stats FIRST and unconditionally: they are fed at ingest and never derived
	// from the cache, so they stay correct after eviction (spec §5.4,
	// invariant 3). e.Arrival, never e.Stamp — a minion with a skewed clock must
	// not be able to write into a past bucket (spec §4.3, invariant 2).
	h.rings.Add(e.Arrival)
	h.cats.Add(e.Category)

	if e.Minion != "" {
		h.minions.Add(e.Minion)
	}

	if e.Fun != "" {
		h.funs.Add(e.Fun)
	}

	h.observeJob(e, facts)

	// The cache is LAST because it takes ownership of the payload and may shed
	// it immediately to stay in budget. Everything above reads only the eagerly
	// extracted fields, which is what makes invariant 9 true: shedding a payload
	// cannot change a job count or an aggregate, because nothing above ever
	// looked at one.
	h.cache.Add(e)
}

// observeJob folds an event into the job index.
//
// facts is decoded by the caller BEFORE h.mu is taken; see Event.
//
// It keeps no per-job bookkeeping of its own. It used to maintain a dirty set
// and a cache of the clone last handed to a snapshot, so that a tick re-copied
// only the jobs that had moved. That cache is gone with the clones it existed
// to avoid: jobList now builds a model.JobRow per listed job in O(1), so there
// is nothing left worth caching — and nothing left to leak while a PAUSED
// console takes no snapshots (invariant 7), which is the failure mode the dirty
// set had to be bounded against.
func (h *hub) observeJob(e model.Event, facts newJobFacts) {
	if e.Kind != model.KindNew && e.Kind != model.KindRet {
		return
	}

	h.jobs.Observe(e, facts.expected, facts.trimmed)

	if e.JID == "" {
		return
	}

	h.describeJob(e.JID, facts)
}

// describeJob records the descriptive fields a job/new event carries beyond the
// indexed ones — target, targeting type and the user who ran it (spec §7.5).
//
// They are written through the index's own job rather than passed to Observe
// because they are display text, not correlation state: nothing counts them and
// nothing branches on them. Each is written only when it adds information, for
// the same reason observeNew does: a second sighting must never blank a name an
// earlier event taught us.
func (h *hub) describeJob(jid string, facts newJobFacts) {
	if facts.tgt == "" && facts.user == "" && facts.tgtType == "" {
		return
	}

	job, lookup := h.jobs.Job(jid)
	if lookup != stats.LookupFound || job == nil {
		return
	}

	if job.Tgt == "" {
		job.Tgt = facts.tgt
	}

	if job.TgtType == "" {
		job.TgtType = facts.tgtType
	}

	if job.User == "" {
		job.User = facts.user
	}
}

// newJobFacts is everything a job/new payload contributes beyond the eagerly
// extracted fields.
//
// expected is nil when the minion list was not readable, and that nil is
// load-bearing: JobIndex.Observe records a non-nil empty slice as a confident
// "the master told us: none", which would fabricate the "0 missing" invariant 10
// exists to prevent. trimmed is the separate, third state — the master replaced
// the list with VALUE_TRIMMED (spec §5.3 case B).
type newJobFacts struct {
	expected []string
	trimmed  bool
	tgt      string
	tgtType  string
	user     string
}

// factsFor decodes a job/new payload.
//
// This is the ONE decode at ingest beyond the shallow field extraction, and it
// runs only on job/new events — a handful per session rather than per event —
// so invariant 4's intent holds: the per-event cost of ingest is still the
// shallow ExtractFields pass, and no return payload is ever decoded here.
//
// It must be called with h.mu NOT held, and it is safe there because it touches
// no hub state that the lock guards: h.decode is written once by newHub and
// read-only afterwards, and e belongs to the reader goroutine. Moving this call
// back inside the lock would return a multi-millisecond stall to the render
// tick; see Event.
func (h *hub) factsFor(e model.Event) newJobFacts {
	if e.Kind != model.KindNew || len(e.Payload) == 0 || h.decode == nil {
		return newJobFacts{}
	}

	v, err := h.decode(e.Payload)
	if err != nil {
		// Counted through DecodeError only for frames the reader could not
		// read; a job/new we cannot decode simply leaves the denominator
		// unknown, which the Jobs pane renders as "?" rather than as a number.
		return newJobFacts{}
	}

	expected, trimmed := expectedFrom(v)

	return newJobFacts{
		expected: expected,
		trimmed:  trimmed,
		tgt:      label(field(v, "tgt")),
		tgtType:  label(field(v, "tgt_type")),
		user:     label(field(v, "user")),
	}
}

// expectedFrom reads the `minions` list off a decoded job/new payload.
//
// It returns (nil, false) — "we do not know" — for anything it cannot read in
// full, INCLUDING a list with an element that is not a string. A partial list
// would be worse than no list: it is a denominator that looks authoritative and
// is too small, which renders as a job that has over-returned or as minions
// that are not missing when they are (invariant 10).
func expectedFrom(v any) ([]string, bool) {
	raw, present := field(v, "minions")
	if !present {
		return nil, false
	}

	// Salt replaces an oversize value with this literal. That is a DIFFERENT
	// state from "absent" and must be reported as such (spec §5.3 case B).
	if s, isStr := text(raw); isStr && s == saltipc.TrimmedMarker {
		return nil, true
	}

	list, isList := raw.([]interface{})
	if !isList {
		return nil, false
	}

	out := make([]string, 0, len(list))

	for _, item := range list {
		s, ok := text(item)
		if !ok {
			return nil, false
		}

		out = append(out, s)
	}

	return out, false
}

// field reads one top-level key from a decoded payload.
//
// Both map shapes are handled and the map[interface{}]interface{} one is the
// case that matters: saltipc.DecodeValue sets DecodeUntypedMap, so EVERY map it
// returns — at every nesting level — has interface keys and it never returns a
// map[string]any. Asserting only the latter would make this silently find
// nothing on every real event off the bus, leaving every job's denominator
// unknown while the tests passed against hand-built maps. That is not
// hypothetical: internal/ui/detail documents the same mistake, found against
// frames captured from a live master.
func field(v any, key string) (any, bool) {
	switch t := v.(type) {
	case map[interface{}]interface{}:
		out, ok := t[key]

		return out, ok
	case map[string]interface{}:
		out, ok := t[key]

		return out, ok
	default:
		return nil, false
	}
}

// text renders a decoded scalar as a string, reporting false for anything that
// is not textual. Salt packs with use_bin_type=True, so a Python str arrives as
// a msgpack str and decodes to string — but bytes arrive as bin and decode to
// []byte, and a minion ID that took that route is still a minion ID.
func text(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, true
	case []byte:
		return string(t), true
	default:
		return "", false
	}
}

// label renders a job's descriptive field for display, bounded.
//
// A list-valued target (Salt's list targeting) is joined rather than dropped:
// `-L web-1,web-2` is exactly the case where the operator most wants to see
// what was targeted.
func label(v any, present bool) string {
	if !present {
		return ""
	}

	if s, ok := text(v); ok {
		return clampLabel(s)
	}

	list, ok := v.([]interface{})
	if !ok {
		return ""
	}

	parts := make([]string, 0, len(list))

	for _, item := range list {
		if s, isText := text(item); isText {
			parts = append(parts, s)
		}
	}

	return clampLabel(strings.Join(parts, ","))
}

// clampLabel bounds a stored label by BYTES. It is deliberately not a display
// fit: this value is stored, not rendered, and the panes own their own width.
func clampLabel(s string) string {
	if len(s) <= maxLabelBytes {
		return s
	}

	// ToValidUTF8 removes the partial rune a byte cut can leave behind.
	return strings.ToValidUTF8(s[:maxLabelBytes], "")
}

// Gap implements saltipc.Sink.
//
// It does NOT touch h.connected. Reader.Run closes an outage window by calling
// Gap on the RECONNECT path, so clearing the flag here made the one call that
// means "we are back" the call that said "we are gone". Connectedness comes
// from Attached and from nowhere else.
func (h *hub) Gap(from, to time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.rings.MarkGap(from, to)
}

// Attached implements saltipc.Sink: the reader tells us whether it holds the
// publish socket, and that is the ONLY source of the status bar's connection
// state.
//
// Deriving it from event arrival instead — which is what this did — made a
// healthy but quiet master read DISCONNECTED in capitals until the first event
// landed. Spec §8.2 is about an outage not rendering as a quiet master; this
// was the inverse, and during an incident it is worse.
func (h *hub) Attached(attached bool) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.connected = attached
}

// DecodeError implements saltipc.Sink.
func (h *hub) DecodeError(error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.decodeErrors++
}

// PinJob implements ui.Pinner: it keeps the job the operator is reading out of
// the eviction path, and clears the pin when jid is empty.
//
// The root calls this every tick with whatever the Jobs pane reports, so the
// pin is a level rather than an edge and cannot be left set by a missed
// message.
func (h *hub) PinJob(jid string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if jid == "" {
		h.jobs.Unpin()

		return
	}

	h.jobs.Pin(jid)
}

// Snapshot implements ui.Source. Called from the render tick.
//
// Everything is assembled here, under the lock, and read without it — which is
// what keeps render cost O(visible rows) and independent of event rate
// (spec §4.1, invariant 6).
func (h *hub) Snapshot(q filter.Query, limit int) ui.Snapshot {
	h.mu.Lock()
	defer h.mu.Unlock()

	events, scanned := h.cache.Snapshot(q, limit)

	return ui.Snapshot{
		// The snapshot's clock reading is the SAME clock the reader stamps
		// arrival with, so a duration computed between the two is meaningful.
		// Never _stamp (invariant 2).
		Now:           h.clock.Now(),
		Events:        events,
		Cache:         h.cache.Stats(),
		Seconds:       h.rings.Seconds(),
		Minutes:       h.rings.Minutes(),
		SecSum:        h.rings.SummarySeconds(),
		MinSum:        h.rings.SummaryMinutes(),
		TopCategories: h.cats.Top(topEntries),
		TopMinions:    h.minions.Top(topEntries),
		TopFunctions:  h.funs.Top(topEntries),
		Jobs:          h.jobList(),
		JobStats:      h.jobs.Stats(),
		JobLookup:     h.lookupJob,
		Scanned:       scanned,
		Query:         q,
		Connected:     h.connected,
		DecodeErrors:  h.decodeErrors,
	}
}

// jobList returns the listed jobs as rows that ingest cannot mutate.
//
// This runs under h.mu on every render tick, so its cost IS the reader
// goroutine's stall. It is O(listJobs) and nothing more: a model.JobRow is a
// fixed set of value fields, and every count on it is either a map length or an
// incrementally maintained counter, so the size of a job's minion sets does not
// enter into it.
//
// It used to hand out job.Clone()s, deep-copying both minion maps of every job
// that had moved since the last tick — 22.55 ms of held lock at 200 listed jobs
// x 1,000 minions against a 100 ms render interval, i.e. ~22% of wall clock
// blocked for as long as that load persisted. That is the same magnitude and
// the same shape as cache.Snapshot's unbounded scan, which was bounded for the
// same reason. A clone cache filtered by a dirty set took the ordinary case off
// that path but not the busy one — under sustained load every listed job is
// dirty every tick, so the cache saved nothing at exactly the load it was
// written for, and past listJobs dirty entries it was DISCARDED, re-cloning the
// whole list. Bounding by what the viewport needs rather than by what changed
// removes the cost instead of deferring it.
//
// Caller must hold h.mu.
func (h *hub) jobList() []model.JobRow {
	live := h.jobs.List(listJobs)

	out := make([]model.JobRow, 0, len(live))
	for _, job := range live {
		out = append(out, job.Row())
	}

	return out
}

// lookupJob resolves a JID for the Jobs pane's drill-down.
//
// It takes the lock itself because it is called from the RENDER goroutine,
// long after Snapshot returned — the function travels in the snapshot, so the
// snapshot's lock is not held when it runs. It returns a clone for the same
// reason jobList does: the index keeps mutating the original as returns arrive.
func (h *hub) lookupJob(jid string) (*model.Job, stats.Lookup) {
	h.mu.Lock()
	defer h.mu.Unlock()

	job, lookup := h.jobs.Job(jid)
	if job == nil {
		return nil, lookup
	}

	return job.Clone(), lookup
}

// AllEvents returns everything retained that matches q, for export.
//
// # What holds the ingest lock, exactly
//
// Only the copy. The exporter's decode, marshal and write — the
// multi-hundred-megabyte part — run from the returned slice with nothing held
// (spec §10.3), and so does the filtering, which used to be inside the lock
// and cost more than the copy did.
//
// The copy itself is O(retained) and there is no honest way around that: an
// export is of the whole retained set by definition, and the alternative —
// releasing the lock between chunks — would hand the operator a torn file
// spanning several instants rather than one. Measured at ~13 ms for 500,000
// retained events, scaling linearly with --max-memory. Unlike Snapshot, which
// pays its cost ten times a second forever, this is one operator keystroke,
// and the kernel's socket buffer absorbs a pause of that length. That is the
// trade; it is written down here rather than claimed away, because the earlier
// version of this comment said a big write "never holds up ingest" — true of
// the write, false of the copy, which is the expensive half.
func (h *hub) AllEvents(q filter.Query) []model.Event {
	all := h.copyAll()

	if q.IsZero() {
		return all
	}

	// Filtered in place: the slice is already ours alone, and writing behind
	// the read cursor cannot overtake it.
	out := all[:0]

	for _, e := range all {
		if q.Match(e) {
			out = append(out, e)
		}
	}

	return out
}

// copyAll takes the whole retained set under the lock and nothing else, so the
// hold is one memmove rather than a memmove plus a match per event.
func (h *hub) copyAll() []model.Event {
	h.mu.Lock()
	defer h.mu.Unlock()

	return h.cache.All()
}
