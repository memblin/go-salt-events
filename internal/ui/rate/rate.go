// Package rate renders the live events/sec and events/min view: two
// independently scaled sparklines, their mandatory numeric callouts, and the
// inline top-N breakdowns (spec §7.3, §9).
//
// The distinction this pane exists to preserve is gap-vs-zero. A gap bucket
// carries Count == 0, so reading a count naively prints "0" underneath a
// sparkline that is correctly drawing a break — telling the operator "nothing
// happened" when the truth is "nobody was recording", which is exactly
// backwards during an incident (spec §8.2). stats.Summary carries NowIsGap and
// HasData so this pane never has to re-derive that from raw buckets, and
// nothing here prints a number without checking them first.
package rate

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

// Pane renders the rate view.
type Pane struct {
	// fixedScale pins both sparklines' vertical scale so two periods can be
	// compared. Autoscaling is the default because it shows shape; the pin is
	// what shows magnitude.
	fixedScale bool
}

var _ ui.Pane = (*Pane)(nil)

// New returns a Rate pane.
func New() *Pane { return &Pane{} }

// Title implements ui.Pane.
func (p *Pane) Title() string { return "Rate" }

// SetStyles implements ui.Pane. This pane caches no styles: it reads every
// colour from the *theme.Styles handed to View, so a theme switch costs it
// nothing.
func (p *Pane) SetStyles(*theme.Styles) {}

// toggleKey pins and unpins the vertical scale (spec §9).
const toggleKey = "F"

// Update toggles the fixed scale.
func (p *Pane) Update(msg tea.Msg, _ ui.Snapshot) (ui.Pane, tea.Cmd) {
	if key, ok := msg.(tea.KeyMsg); ok && key.String() == toggleKey {
		p.fixedScale = !p.fixedScale
	}

	return p, nil
}

// Keys implements ui.Pane.
//
// The label reports what pressing the key will do NEXT, not what the pane is
// doing now, because that is what an operator reading a hint line needs. The
// root renders whatever comes back, so omitting the toggle here would ship it
// undiscoverable — the reason Keys is part of the contract at all.
func (p *Pane) Keys() []ui.KeyHint {
	label := "pin the scale"
	if p.fixedScale {
		label = "autoscale"
	}

	return []ui.KeyHint{{Key: toggleKey, Label: label}}
}

const (
	// topRows is how many entries each breakdown shows. Past roughly seven
	// classes adjacent rows blur and the reader stops distinguishing them.
	topRows = 5

	// noData is what a gap prints where a rate would go. It is deliberately
	// not "0", not "-" and not blank: the operator must be able to tell a
	// master that was quiet from a window nobody was recording (spec §8.2).
	noData = "no data"

	secTitle = "Events/sec  last 120s"
	minTitle = "Events/min  last 60m "

	// barCells and pctCells size a top-N row; gapCells is the two single
	// spaces separating label, bar and percentage.
	barCells = 10
	pctCells = 5
	gapCells = 2

	// sideBySide is the narrowest half-width at which two breakdown columns
	// still hold a readable label; below it they stack.
	sideBySide = 18
)

// View renders the pane into a w×h CONTENT box.
//
// Cost is O(w) per sparkline plus O(topRows), independent of event rate
// (invariant 6). Every line is clamped to w display cells and the whole block
// to h rows: the root subtracts the border before calling, and a pane that
// overflowed its content box would push the frame apart.
func (p *Pane) View(w, h int, s ui.Snapshot, st *theme.Styles) string {
	if w <= 0 || h <= 0 {
		return ""
	}

	lines := make([]string, 0, h)
	lines = append(lines, p.series(secTitle, s.Seconds, s.SecSum, w, st)...)
	lines = append(lines, "")
	lines = append(lines, p.series(minTitle, s.Minutes, s.MinSum, w, st)...)
	lines = append(lines, "")
	lines = append(lines, p.breakdowns(w, s, st)...)

	if len(lines) > h {
		lines = lines[:h]
	}

	for i, l := range lines {
		lines[i] = fit(l, w)
	}

	return strings.Join(lines, "\n")
}

// series renders one titled sparkline and its callout.
func (p *Pane) series(title string, bs []stats.Bucket, sum stats.Summary, w int, st *theme.Styles) []string {
	head := st.Header.Render(title) + "  " + callout(sum, st)
	if p.fixedScale {
		head += st.Muted.Render("  [fixed scale]")
	}

	return []string{head, components.Sparkline(bs, w, p.scale(sum), st)}
}

// scale returns the pinned vertical scale, or 0 for autoscale.
//
// A window with no live bucket has no meaningful Peak (HasData is false), so
// pinning to it would pin the axis to a zero that was never measured.
func (p *Pane) scale(sum stats.Summary) uint64 {
	if !p.fixedScale || !sum.HasData || sum.Peak <= 0 {
		return 0
	}

	return uint64(sum.Peak)
}

// callout renders now/peak/mean.
//
// This is never conditional. Without it an autoscaled sparkline conveys shape
// but no magnitude at all, and a calm period is indistinguishable from a storm
// (spec §9). What IS conditional is whether each figure is a number: see now
// and aggregate.
func callout(sum stats.Summary, st *theme.Styles) string {
	return st.KeyLabel.Render("now ") + now(sum, st) +
		st.KeyLabel.Render("  peak ") + aggregate(sum.Peak, sum.HasData, st) +
		st.KeyLabel.Render("  mean ") + aggregate(sum.Mean, sum.HasData, st)
}

// now renders the newest bucket's rate, or says we were not recording.
//
// A gap bucket carries Count == 0, so printing Now without checking NowIsGap
// reports "0 events/sec" for a window nobody was watching. Warn is legitimate
// here and is not a series colour: spec §9 reserves Ok/Warn/Err for status
// INCLUDING connection state, and a gap is precisely connection state.
func now(sum stats.Summary, st *theme.Styles) string {
	if sum.NowIsGap {
		return st.Warn.Render(noData)
	}

	return st.Value.Render(human(sum.Now))
}

// aggregate renders peak or mean, which are only meaningless when the WHOLE
// window is a gap — summarise already skips gap buckets, so a partial outage
// leaves both as real measurements of the buckets we did see.
func aggregate(v float64, hasData bool, st *theme.Styles) string {
	if !hasData {
		return st.Warn.Render(noData)
	}

	return st.Value.Render(human(v))
}

// breakdowns renders the two top-N tables side by side, or stacked when the
// box is too narrow for two readable columns.
func (p *Pane) breakdowns(w int, s ui.Snapshot, st *theme.Styles) []string {
	half := w/2 - 1
	if half < sideBySide {
		return append(
			topBlock("Top tags", s.TopCategories, w, st),
			topBlock("Top minions", s.TopMinions, w, st)...,
		)
	}

	left := topBlock("Top tags", s.TopCategories, half, st)
	right := topBlock("Top minions", s.TopMinions, half, st)

	rows := max(len(left), len(right))
	out := make([]string, 0, rows)

	for i := range rows {
		l, r := "", ""

		if i < len(left) {
			l = left[i]
		}

		if i < len(right) {
			r = right[i]
		}

		out = append(out, padTo(l, half)+" "+r)
	}

	return out
}

// topBlock renders one ranked list into exactly w cells.
//
// One hue, bar length carries magnitude, the text label carries identity. No
// colour-by-rank: a reshuffling ranking must not repaint rows (spec §9).
func topBlock(title string, entries []stats.Entry, w int, st *theme.Styles) []string {
	out := make([]string, 0, topRows+1)
	out = append(out, st.Header.Render(fit(title, w)))

	barW := min(barCells, w/3)
	labelW := w - barW - pctCells - gapCells

	for i, e := range entries {
		if i >= topRows {
			break
		}

		out = append(out, entryRow(e, labelW, barW, st))
	}

	return out
}

// entryRow renders one ranked entry. A box too narrow for even a truncated
// label renders blank rather than a misleading fragment.
func entryRow(e stats.Entry, labelW, barW int, st *theme.Styles) string {
	if labelW < 1 {
		return ""
	}

	row := st.Value.Render(padTo(e.Key, labelW))

	if barW > 0 {
		row += " " + components.Bar(e.Pct, barW, st)
	}

	return row + " " + st.Muted.Render(fmt.Sprintf("%4.0f%%", e.Pct))
}

// human renders a rate compactly: 2500 becomes 2.5k.
func human(v float64) string {
	switch {
	case v >= 1_000_000:
		return fmt.Sprintf("%.1fM", v/1_000_000)
	case v >= 1_000:
		return fmt.Sprintf("%.1fk", v/1_000)
	default:
		return fmt.Sprintf("%.0f", v)
	}
}

// fit truncates s to at most w DISPLAY cells.
//
// Display cells, not bytes or runes: these strings carry ANSI styling, and
// cutting one by rune index would slice an escape sequence in half — and would
// also make the truncation point depend on the theme, which the layout guard
// forbids.
func fit(s string, w int) string {
	if w <= 0 {
		return ""
	}

	return lipgloss.NewStyle().MaxWidth(w).Render(s)
}

// padTo fits s to exactly w display cells, right-padding with spaces.
func padTo(s string, w int) string {
	if w <= 0 {
		return ""
	}

	s = fit(s, w)

	if pad := w - lipgloss.Width(s); pad > 0 {
		s += strings.Repeat(" ", pad)
	}

	return s
}
