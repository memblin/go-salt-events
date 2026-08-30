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
	Events []model.Event

	Cache cache.Stats

	Seconds []stats.Bucket
	Minutes []stats.Bucket
	SecSum  stats.Summary
	MinSum  stats.Summary

	TopCategories []stats.Entry
	TopMinions    []stats.Entry
	TopFunctions  []stats.Entry

	Jobs     []*model.Job
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
