// Package summary renders the session-wide aggregations: the three ranked
// breakdowns (tags, minions, functions) and the job-index pressure readout
// (spec §7.4, §7.5).
//
// Two things here are less obvious than they look.
//
// An EMPTY Category is normal, not a bug. Roughly a fifth of real traffic
// (measured: 6 of 32 frames off a live Salt 3006.27 master) is a bare-JID tag
// with no salt/ prefix — the master's job publish-ack. It carries an extracted
// JID and a deliberately empty Category, because a bare JID has no category
// segments: "none" is the honest value and "any" would be a lie. An earlier
// attempt stored "*" for it and was rejected — that collides with a
// minion-sendable literal "*" tag, folding two unrelated event classes into
// one top-N bucket, and would print a row labelled with the character that
// means "any" everywhere else in this tool. So the DATA keeps the empty
// string, and this package — the only place it is ever shown to a human —
// renders it as what it is. Nothing here invents a value.
//
// Category has NO length bound, and truncation is this package's job. A minion
// can event.send an arbitrarily long tag and Category derives from it.
// Bounding it upstream would truncate legitimate tags at the source and need a
// marker that could collide with real tag text exactly as "*" did, so the
// stored value stays faithful and components.RankedLabel shortens it to fit the
// column, with a visible indicator. A 50,000-character category costs this pane
// O(column width), not O(length).
package summary

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/TKC-Labs/go-salt-events/internal/stats"
	"github.com/TKC-Labs/go-salt-events/internal/theme"
	"github.com/TKC-Labs/go-salt-events/internal/ui"
	"github.com/TKC-Labs/go-salt-events/internal/ui/components"
)

// Pane renders the Summary view.
type Pane struct{}

var _ ui.Pane = (*Pane)(nil)

// New returns a Summary pane.
func New() *Pane { return &Pane{} }

// Title implements ui.Pane.
func (p *Pane) Title() string { return "Summary" }

// SetStyles implements ui.Pane. This pane caches no styles: every colour comes
// from the *theme.Styles handed to View, so a theme switch costs it nothing.
func (p *Pane) SetStyles(*theme.Styles) {}

// Update implements ui.Pane. Summary has no interaction of its own: it renders
// aggregates the ingest layer already maintains, and there is nothing here to
// select, scroll or toggle.
func (p *Pane) Update(tea.Msg, ui.Snapshot) (ui.Pane, tea.Cmd) { return p, nil }

// Keys implements ui.Pane, and returns nil deliberately.
//
// This pane binds no key, so nil is the correct answer rather than an
// oversight — the contract makes the method mandatory precisely so that
// "binds nothing" is a stated, reviewable choice instead of an absence. If a
// window selector (spec §7.4) is ever added, its key belongs here in the same
// commit, or it ships undiscoverable.
func (p *Pane) Keys() []ui.KeyHint { return nil }

// Block titles. The two substitutes for an unnameable key live in
// internal/ui/components (PublishAck, NoKey) because the Rate pane renders the
// same ranked slices and an operator compares the two screens.
const (
	tagsTitle    = "Top tags"
	minionsTitle = "Top minions"
	funcsTitle   = "Top functions"

	nothingYet = "(nothing yet)"
)

// Row geometry. pctCells matches "%5.1f%%" and countCells matches "%7d".
const (
	rowIndent  = 2
	barCells   = 12
	pctCells   = 6
	countCells = 7

	// minLabel is the narrowest label worth keeping a companion field for.
	// Below it the identity of the row — the only thing colour cannot carry
	// (spec §9) — starts losing to decoration.
	minLabel = 8

	// topRows bounds how many entries a block shows. Render cost is O(visible
	// rows) regardless of how many keys the counter tracks (invariant 6).
	topRows = 8
)

// View renders the pane into a w×h CONTENT box.
//
// The box can be as small as 1x1 during a resize and every Snapshot slice is
// nil before the first tick; neither may panic. The root subtracts the border
// before calling, so nothing here draws one, and every line is clamped to w
// display cells so the frame cannot be pushed apart.
//
// Cost is O(topRows) per block, independent of event rate and of how many
// distinct keys the counters hold (invariant 6). Nothing is recomputed from
// Snapshot.Events: the rankings are fed at ingest and only rendered here
// (invariant 3).
func (p *Pane) View(w, h int, s ui.Snapshot, st *theme.Styles) string {
	if w <= 0 || h <= 0 {
		return ""
	}

	// The pressure readout is anchored to the bottom rather than simply
	// appended, so a short box drops ranked rows — which the Rate pane also
	// shows — before it drops the high-water mark, which is shown ONLY here
	// and is the number an operator reads off to size --max-jobs (spec §7.5).
	// Its blank separator is part of that block so it is the first thing to go,
	// and keeping the LAST lines means the actionable half survives longest.
	tail := append([]string{""}, indexLines(s.JobStats, w, st)...)
	if len(tail) > h {
		tail = tail[len(tail)-h:]
	}

	lines := make([]string, 0, h)

	if budget := h - len(tail); budget > 0 {
		body := p.blocks(w, s, st)
		if len(body) > budget {
			body = body[:budget]
		}

		lines = append(lines, body...)
	}

	lines = append(lines, tail...)

	for i, l := range lines {
		lines[i] = components.Fit(l, w)
	}

	return strings.Join(lines, "\n")
}

// blocks renders the three ranked breakdowns, blank-separated.
func (p *Pane) blocks(w int, s ui.Snapshot, st *theme.Styles) []string {
	lay := layoutRow(w)

	out := block(tagsTitle, components.PublishAck, s.TopCategories, lay, st)
	out = append(out, "")
	out = append(out, block(minionsTitle, components.NoKey, s.TopMinions, lay, st)...)
	out = append(out, "")
	out = append(out, block(funcsTitle, components.NoKey, s.TopFunctions, lay, st)...)

	return out
}

// block renders one ranked list: a title, then up to topRows entries.
//
// empty is the label used for an entry whose Key is the empty string. It is
// passed in rather than hardcoded because the correct wording depends on which
// breakdown is being drawn — an empty Category is a specific, identifiable
// event class, an empty minion is merely absent.
func block(title, empty string, entries []stats.Entry, lay rowLayout, st *theme.Styles) []string {
	out := make([]string, 0, topRows+1)
	out = append(out, st.Header.Render(title))

	if len(entries) == 0 {
		return append(out, st.Muted.Render(indent()+nothingYet))
	}

	for i, e := range entries {
		if i >= topRows {
			break
		}

		out = append(out, entryRow(e, empty, lay, st))
	}

	return out
}

// rowLayout is one ranked row's field widths, already fitted to the box.
type rowLayout struct {
	label int
	bar   int
	pct   bool
	count bool
}

// layoutRow sizes a ranked row for a w-cell box.
//
// Fields are dropped least-informative first as the box narrows: the raw count
// goes before the bar, and the bar before the label. The label is last to go
// because it is the only field carrying identity — spec §9 puts identity in
// the text and magnitude in the bar's length, so a row reduced to a bar says
// nothing at all.
func layoutRow(w int) rowLayout {
	lay := rowLayout{label: w - rowIndent, bar: 0, pct: false, count: false}

	if lay.label-(pctCells+1) >= minLabel {
		lay.pct = true
		lay.label -= pctCells + 1
	}

	if bar := min(barCells, w/4); lay.pct && bar > 0 && lay.label-(bar+1) >= minLabel {
		lay.bar = bar
		lay.label -= bar + 1
	}

	if lay.bar > 0 && lay.label-(countCells+1) >= minLabel {
		lay.count = true
		lay.label -= countCells + 1
	}

	return lay
}

// entryRow renders one ranked entry.
//
// A box too narrow for even a clipped label renders an empty line rather than
// a misleading fragment.
func entryRow(e stats.Entry, empty string, lay rowLayout, st *theme.Styles) string {
	if lay.label < 1 {
		return ""
	}

	row := indent() + st.Value.Render(components.RankedLabel(e.Key, empty, lay.label))

	if lay.bar > 0 {
		row += " " + components.Bar(e.Pct, lay.bar, st)
	}

	if lay.pct {
		row += " " + st.Muted.Render(fmt.Sprintf("%5.1f%%", e.Pct))
	}

	if lay.count {
		row += " " + st.Muted.Render(fmt.Sprintf("%*d", countCells, e.Count))
	}

	return row
}

// indexLines reports job-index pressure (spec §7.5) as one line, or as two
// when the box is too narrow to hold both halves.
//
// It wraps rather than letting the line be clipped because the half that would
// be lost is the guidance — the knob's name and the number to give it. On an
// 80-column terminal that is exactly the half that falls off the end, and a
// readout whose advice is cut short at the default width is a readout that
// does not work.
//
// Eviction is never silent: a non-zero count is the signal to raise
// --max-jobs, and the knob is named whether or not anything has been evicted,
// so the value can be sized from a representative session instead of guessed
// twice.
//
// The wording is "above" the high-water mark, not "to" it. IndexStats.HighWater
// is recorded on insertion, before eviction runs, so once the index saturates
// it clamps at Cap+1 and reports only "saturated" — the demand above the cap
// is not observable from inside a bounded index. "Raise it to 501" would be
// advice this pane cannot support.
func indexLines(is stats.IndexStats, w int, st *theme.Styles) []string {
	occupancy := st.KeyLabel.Render("jobs ") +
		st.Value.Render(fmt.Sprintf("%d/%d", is.Tracked, is.Cap)) +
		st.KeyLabel.Render(" tracked  peak concurrent ") +
		st.Value.Render(fmt.Sprintf("%d", is.HighWater))

	guidance := guidanceFor(is, w, st)

	if lipgloss.Width(occupancy)+lipgloss.Width(guidance)+2 <= w {
		return []string{occupancy + "  " + guidance}
	}

	return []string{occupancy, guidance}
}

// guidanceFor returns the advice half of the pressure readout, shortened when
// the sentence does not fit the box.
//
// What shrinks is the prose, never the figures: the knob's name and the number
// to give it are the whole point of the readout, and a clipped sentence would
// take the number with it.
//
// Warn is status, not a series colour, and index saturation is exactly the
// kind of state spec §9 reserves it for.
func guidanceFor(is stats.IndexStats, w int, st *theme.Styles) string {
	if is.Evicted == 0 {
		quiet := fmt.Sprintf("--max-jobs %d, nothing evicted", is.Cap)
		if lipgloss.Width(quiet) > w {
			quiet = fmt.Sprintf("--max-jobs %d", is.Cap)
		}

		return st.Muted.Render(quiet)
	}

	full := fmt.Sprintf("%d evicted — raise --max-jobs above %d", is.Evicted, is.HighWater)
	if lipgloss.Width(full) > w {
		full = fmt.Sprintf("%d evicted, --max-jobs >%d", is.Evicted, is.HighWater)
	}

	return st.Warn.Render(full)
}

// indent is the leading gutter shared by every row under a block title.
func indent() string { return strings.Repeat(" ", rowIndent) }
