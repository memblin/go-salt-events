// Package ui holds the bubbletea root model, the Pane contract every feature
// pane implements, and the surrounding chrome.
//
// Two properties are load-bearing here and are the reason the package is
// shaped the way it is:
//
//   - The UI receives no per-event messages (spec §4.1, invariant 6). A reader
//     goroutine folds events into the cache and the stats under a mutex; the
//     root asks for one Snapshot per tick and every pane renders from that.
//     Render cost is O(visible rows) and independent of event rate, so a
//     5,000 event/sec storm cannot wedge the console.
//
//   - The root owns the single active *theme.Styles and the pane border. Panes
//     receive styles as a View parameter and never obtain their own — that is
//     what makes a theme switch one assignment that loses no scroll position,
//     and it is asserted by TestOnlyTheRootObtainsStyles.
package ui

import (
	"time"

	"github.com/TKC-Labs/go-salt-events/internal/cache"
	"github.com/TKC-Labs/go-salt-events/internal/filter"
	"github.com/TKC-Labs/go-salt-events/internal/model"
	"github.com/TKC-Labs/go-salt-events/internal/stats"
)

// Snapshot is an immutable view of everything the panes may render, assembled
// once per tick under the ingest lock and then read without it.
//
// Panes must not hold a reference to the cache or the stats: that is what
// keeps rendering independent of ingest rate (spec §4.1, invariant 6).
type Snapshot struct {
	// Now is the moment this snapshot was assembled, read under the ingest lock
	// from the SAME clock that stamps event arrival. It is what lets a job that
	// is still returning count its duration up live between ticks (spec §7.5)
	// without any pane reaching for the wall clock itself.
	//
	// It is never Salt's _stamp, which is set by whichever process fired the
	// event: mixing a skewed minion clock into a duration computed against
	// arrival times would produce negative and jumping durations (spec §4.3,
	// invariant 2).
	//
	// It is the zero Time in a snapshot assembled without a clock — including
	// every snapshot a pane sees before the first tick — and consumers must
	// treat that as "no reading available" rather than as the epoch.
	Now time.Time

	Events []model.Event

	// Scanned is how many retained events the cache examined to build Events.
	//
	// It is here because the scan is BOUNDED (see cache.Snapshot): a selective
	// filter stops after a fixed multiple of limit rather than walking the
	// whole ring, which is what keeps render cost O(visible rows) and off the
	// ingest lock. That bound is a real loss of reach, so it is reported
	// rather than hidden — a pane drawing nothing must be able to say "the
	// filter looked back this far" instead of implying "there are no such
	// events".
	//
	// Compare it with Cache.Events: equal means the scan reached the oldest
	// retained event and the view is complete for the active query.
	Scanned int

	Cache cache.Stats

	Seconds []stats.Bucket
	Minutes []stats.Bucket
	SecSum  stats.Summary
	MinSum  stats.Summary

	TopCategories []stats.Entry
	TopMinions    []stats.Entry
	TopFunctions  []stats.Entry

	// Jobs is the job LIST, as rows rather than as jobs.
	//
	// A row carries no minion sets, which is what keeps a tick's cost O(visible
	// rows) rather than O(the index): the list renders returned/expected, a
	// failure count and a duration, and never a minion name. The per-minion
	// breakdown is JobLookup's job, and it clones exactly one job on demand.
	// Handing whole jobs here cost 22.55 ms of held ingest lock per tick at 200
	// jobs x 1,000 minions (invariant 6).
	Jobs     []model.JobRow
	JobStats stats.IndexStats

	// JobLookup resolves a JID against the job index. It is a function rather
	// than a map because a miss has to distinguish "never seen" from "evicted"
	// (stats.Lookup), and inventing a job for an evicted JID would violate
	// invariant 10. It may be nil before the first tick; callers must check.
	JobLookup func(jid string) (*model.Job, stats.Lookup)

	Query filter.Query

	Connected    bool
	DecodeErrors int
}

// Source produces snapshots. The root owns one; panes never see it.
//
// limit bounds how many events a snapshot carries: copying the whole cache
// every tick would make render cost scale with cache size rather than viewport
// size.
type Source interface {
	Snapshot(q filter.Query, limit int) Snapshot
}

// Pinner is the optional half of Source: a source whose job index can hold one
// job out of the eviction path.
//
// It is separate from Source, and asserted for rather than required, because
// pinning is a property of the ingest side that a test double has no reason to
// implement. The root calls it once per tick with whatever a pane reports
// through JobPinner — a job disappearing out from under the cursor while it is
// being read is the worst possible moment to lose it (spec §7.5), and the job
// index cannot know which job that is.
type Pinner interface {
	// PinJob pins jid, or clears the pin when jid is empty.
	PinJob(jid string)
}
