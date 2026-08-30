// Package jobs renders the job correlation view: a list of recent jobs, and a
// per-minion drill-down for one of them (spec §7.5).
//
// This pane is where invariant 10 is seen. Everything upstream already gates
// on model.ExpectedState — model.Missing and model.ExpectedCount both refuse
// to answer when the expected set is not known — and the rendering here must
// not undo that at the last step. A confident "0 missing" on a job whose
// expected set was never observed reads as "everything returned" and sends an
// operator away from broken machines, so the unknown case is rendered as the
// word "unknown" in place of a number, never as zero.
//
// The same rule applies one level up: stats.Lookup distinguishes a job that
// was never observed from one the index evicted under pressure. Those have
// different fixes (attach sooner versus raise --max-jobs), so they are
// rendered differently and a job is never invented for a JID we do not hold.
package jobs

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/TKC-Labs/go-salt-events/internal/model"
	"github.com/TKC-Labs/go-salt-events/internal/stats"
	"github.com/TKC-Labs/go-salt-events/internal/theme"
	"github.com/TKC-Labs/go-salt-events/internal/ui"
	"github.com/TKC-Labs/go-salt-events/internal/ui/components"
)

// view selects the drill-down's row filter. The order is spec §7.5's `f`
// cycle: needs-attention (default), failed only, missing only, all.
type view uint8

// The four drill-down filters, in `f` cycle order. viewCount is the modulus,
// not a filter, so adding a fifth view cannot leave the cycle stale.
const (
	// viewAttention is the default: failed rows, then missing. At a thousand
	// targets the operator cannot read every row, so the top of the screen
	// must always be the actionable part.
	viewAttention view = iota
	viewFailed
	viewMissing
	viewAll
	viewCount
)

func (v view) String() string {
	switch v {
	case viewFailed:
		return "failed only"
	case viewMissing:
		return "missing only"
	case viewAll:
		return "all"
	case viewAttention, viewCount:
		return "needs attention"
	default:
		return "needs attention"
	}
}

// rowState is one drill-down row's status. It is an enum rather than a string
// so the renderer's switch cannot drift from the builder's spelling.
type rowState uint8

// Drill-down row statuses.
const (
	stateFailed rowState = iota
	stateMissing
	stateOK
)

// Chrome line counts, named so the row-window arithmetic reads as intent.
const (
	// listChrome is the index header plus the table's own header row.
	listChrome = 2
	// drillChrome is the title, a blank, the counts, the rule, a blank, and
	// the key hint line.
	drillChrome = 6
)

// minionColumn is the width the minion name is padded to in the drill-down.
// Minion IDs are FQDN-shaped and minion-supplied, so anything longer is
// truncated rather than allowed to push the note off the line.
const minionColumn = 28

// Pane renders the Jobs view.
type Pane struct {
	cursor int

	drilled string // JID, empty when showing the list
	view    view
	offset  int
}

// New returns a Jobs pane.
func New() *Pane { return &Pane{} }

// Title implements ui.Pane.
func (p *Pane) Title() string { return "Jobs" }

// SetStyles implements ui.Pane. This pane caches no styles: it renders with
// the *theme.Styles handed to View every frame, so a theme switch needs no
// action here.
func (p *Pane) SetStyles(*theme.Styles) {}

// PinnedJID is the job the operator is reading, which the index must not
// evict. It is empty while the list is showing.
func (p *Pane) PinnedJID() string { return p.drilled }

// Keys implements ui.Pane. The bindings differ between the list and the
// drill-down, so what is reported is what is true NOW — a hint line offering
// `f` on the list would be advertising a key that does nothing.
func (p *Pane) Keys() []ui.KeyHint {
	if p.drilled != "" {
		return []ui.KeyHint{
			{Key: "↑/↓", Label: "scroll"},
			{Key: "f", Label: "cycle view (" + p.view.String() + ")"},
			{Key: "esc", Label: "back to the job list"},
		}
	}

	return []ui.KeyHint{
		{Key: "↑/↓", Label: "select job"},
		{Key: "enter", Label: "drill into job"},
	}
}

// Update handles navigation and the drill-down.
func (p *Pane) Update(msg tea.Msg, s ui.Snapshot) (ui.Pane, tea.Cmd) {
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return p, nil
	}

	if p.drilled != "" {
		return p.updateDrilled(key), nil
	}

	switch key.String() {
	case "down", "j":
		p.cursor = min(p.cursor+1, max(0, len(s.Jobs)-1))
	case "up", "k":
		p.cursor = max(p.cursor-1, 0)
	case "enter":
		if p.cursor < len(s.Jobs) {
			p.drilled, p.view, p.offset = s.Jobs[p.cursor].JID, viewAttention, 0
		}
	}

	return p, nil
}

// updateDrilled handles keys inside the drill-down.
func (p *Pane) updateDrilled(key tea.KeyMsg) *Pane {
	switch key.String() {
	case "esc":
		p.drilled = ""
	case "f":
		p.view = (p.view + 1) % viewCount
		p.offset = 0
	case "down", "j":
		p.offset++
	case "up", "k":
		p.offset = max(0, p.offset-1)
	}

	return p
}

// View renders either the list or the drill-down into the w×h CONTENT box.
// The root owns the border; nothing here draws one.
func (p *Pane) View(w, h int, s ui.Snapshot, st *theme.Styles) string {
	if h <= 0 || w <= 0 {
		return ""
	}

	if p.drilled != "" {
		return p.viewDrilled(w, h, s, st)
	}

	return p.viewList(w, h, s, st)
}

var listColumns = []components.Column{
	{Title: "JID", Width: 16, Flex: false},
	{Title: "FUN", Width: 16, Flex: false},
	{Title: "TGT", Width: 12, Flex: false},
	{Title: "RET", Width: 10, Flex: false},
	{Title: "DUR", Width: 8, Flex: false},
	{Title: "FAIL", Width: 5, Flex: false},
	{Title: "", Width: 0, Flex: true},
}

// viewList renders the job list.
//
// Only the rows that fit on screen are formatted, so render cost is
// O(visible rows) and does not grow with --max-jobs (invariant 6).
func (p *Pane) viewList(w, h int, s ui.Snapshot, st *theme.Styles) string {
	head := indexHeader(s.JobStats, st)

	if len(s.Jobs) == 0 {
		return clamp([]string{head, st.Muted.Render("no jobs seen yet")}, w, h)
	}

	// Jobs are evicted between frames, so the cursor is re-clamped here
	// rather than trusted from Update.
	cursor := min(max(p.cursor, 0), len(s.Jobs)-1)

	window := max(1, h-listChrome)
	start := scrollStart(cursor, window, len(s.Jobs))
	end := min(start+window, len(s.Jobs))

	rows := make([][]string, 0, end-start)

	for _, j := range s.Jobs[start:end] {
		rows = append(rows, []string{
			truncJID(j.JID),
			j.Fun,
			j.Tgt,
			retLabel(j),
			duration(j),
			strconv.Itoa(j.Failed()),
			"",
		})
	}

	return clamp(append([]string{head}, components.RenderTable(
		listColumns, rows, cursor-start, w, st)...), w, h)
}

// indexHeader reports occupancy and evictions.
//
// Eviction is never silent: --max-jobs 500 is a starting value chosen to be
// safe rather than sufficient, so the design's job is to make a wrong number
// visible and name the value to raise it to (spec §7.5).
func indexHeader(is stats.IndexStats, st *theme.Styles) string {
	base := st.KeyLabel.Render("jobs ") +
		st.Value.Render(strconv.Itoa(is.Tracked)+"/"+strconv.Itoa(is.Cap))

	if is.Evicted == 0 {
		return base
	}

	return base + st.Warn.Render(fmt.Sprintf(
		" · %d evicted — raise --max-jobs (peak concurrent was %d)",
		is.Evicted, is.HighWater))
}

// retLabel renders returned/expected in the three states of spec §5.3 case B.
//
// These MUST stay visually distinct. "trimmed" is fixed by raising
// max_event_size on the master; "unseen" by attaching sooner or raising
// --max-jobs. Collapsing them sends the operator after the wrong thing, and
// printing either as a number would fabricate a denominator (invariant 10).
func retLabel(j *model.Job) string {
	n, state := j.ExpectedCount()
	returned := strconv.Itoa(j.Returned())

	switch state {
	case model.ExpectedKnown:
		return returned + "/" + strconv.Itoa(n)
	case model.ExpectedTrimmed:
		return returned + "/⚠"
	case model.ExpectedUnseen:
		return returned + "/?"
	default:
		return returned + "/?"
	}
}

// expectedNote spells out what the ret column's glyph means, for the
// drill-down title where there is room for words.
func expectedNote(j *model.Job) string {
	_, state := j.ExpectedCount()

	switch state {
	case model.ExpectedKnown:
		if j.Complete() {
			return "complete"
		}

		return "running"
	case model.ExpectedTrimmed:
		return "expected set unknown — trimmed by the master (raise max_event_size)"
	case model.ExpectedUnseen:
		return "expected set unknown — no job/new event seen (attach sooner)"
	default:
		return "expected set unknown"
	}
}

// duration renders elapsed time.
func duration(j *model.Job) string {
	if j.Start.IsZero() {
		return "—"
	}

	end := j.LastRet
	if end.IsZero() {
		end = j.Start
	}

	d := end.Sub(j.Start)

	switch {
	case d < time.Second:
		return strconv.FormatInt(d.Milliseconds(), 10) + "ms"
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	default:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
}

// row is one line in the drill-down.
type row struct {
	minion string
	state  rowState
	note   string
}

// viewDrilled renders one job's per-minion breakdown.
func (p *Pane) viewDrilled(w, h int, s ui.Snapshot, st *theme.Styles) string {
	// JobLookup is nil before the first tick (see ui.Snapshot). A pane that
	// called it anyway would panic on the resize that arrives first.
	if s.JobLookup == nil {
		return clamp([]string{st.Muted.Render("waiting for the first snapshot")}, w, h)
	}

	job, lookup := s.JobLookup(p.drilled)

	// Evicted and unseen are rendered differently on purpose: the first is
	// fixed by raising --max-jobs, the second by attaching sooner. Neither
	// invents a job, which would fabricate every count on this screen.
	if lookup == stats.LookupEvicted {
		return clamp([]string{st.Warn.Render(fit(
			"job "+p.drilled+" was evicted from the job index — raise --max-jobs to retain more",
			w))}, w, h)
	}

	if lookup != stats.LookupFound || job == nil {
		return clamp([]string{st.Muted.Render(fit(
			"job "+p.drilled+" was never seen on the bus — attach sooner to catch its job/new event",
			w))}, w, h)
	}

	return clamp(p.drillLines(w, h, job, st), w, h)
}

// drillLines assembles the drill-down, chrome first and then the visible slice
// of the row window.
func (p *Pane) drillLines(w, h int, job *model.Job, st *theme.Styles) []string {
	lines := []string{
		st.Header.Render(fit(truncJID(job.JID)+"  "+job.Fun+"  "+job.Tgt+"  "+
			duration(job)+"  "+expectedNote(job), w)),
		"",
		counts(job, st),
		st.Muted.Render(strings.Repeat("─", max(0, w))),
	}

	rows := p.rows(job)

	// Only the rows on screen are formatted and styled (invariant 6).
	window := max(1, h-drillChrome)
	start := min(p.offset, max(0, len(rows)-1))
	end := min(start+window, len(rows))

	for _, r := range rows[start:end] {
		lines = append(lines, renderRow(r, w, st))
	}

	return append(lines, "", st.Muted.Render(fit(
		"f: "+p.view.String()+"   esc: back   "+
			strconv.Itoa(len(rows))+" rows", w)))
}

// counts renders the header tallies.
//
// The counts always describe the WHOLE job, never the current filter — a
// filtered count would make "23 failed" mean something different depending on
// which view happened to be active (spec §7.5).
func counts(job *model.Job, st *theme.Styles) string {
	failed := job.Failed()
	ok := job.Returned() - failed

	out := st.Err.Render(fmt.Sprintf("  ✗ failed %5d", failed)) + "    "

	missing, known := job.Missing()

	if known {
		out += st.Warn.Render(fmt.Sprintf("⧗ missing %5d", len(missing))) + "    "
	} else {
		// Invariant 10: never fabricate a missing count. "0 missing" reads as
		// "everything returned", which is the most dangerous wrong answer
		// this tool can give, so the word replaces the number entirely.
		out += st.Warn.Render("⧗ missing unknown") + "    "
	}

	return out + st.Ok.Render(fmt.Sprintf("✓ ok %5d", ok))
}

// rows builds the drill-down rows in the current view, actionable first.
//
// The missing half is populated only when model.Missing reports the expected
// set is known; an unknown set yields no missing rows rather than an empty
// list that would read as "none missing".
func (p *Pane) rows(job *model.Job) []row {
	var failed, okRows []row

	// Returns() is already sorted by minion, so the two halves stay sorted.
	for _, r := range job.Returns() {
		if r.RetCode != 0 || !r.Success {
			failed = append(failed, row{
				minion: r.Minion,
				state:  stateFailed,
				note:   "retcode " + strconv.Itoa(r.RetCode),
			})

			continue
		}

		okRows = append(okRows, row{minion: r.Minion, state: stateOK, note: ""})
	}

	var missingRows []row

	if missing, known := job.Missing(); known {
		note := "no return after " + duration(job)
		for _, m := range missing {
			missingRows = append(missingRows, row{minion: m, state: stateMissing, note: note})
		}
	}

	switch p.view {
	case viewFailed:
		return failed
	case viewMissing:
		return missingRows
	case viewAll:
		return concat(failed, missingRows, okRows)
	case viewAttention, viewCount:
		return concat(failed, missingRows)
	default:
		return concat(failed, missingRows)
	}
}

// concat joins row slices without aliasing any of them; appending onto the
// first would scribble over the caller's backing array.
func concat(parts ...[]row) []row {
	n := 0
	for _, p := range parts {
		n += len(p)
	}

	out := make([]row, 0, n)
	for _, p := range parts {
		out = append(out, p...)
	}

	return out
}

// renderRow renders one drill-down row.
//
// Status colours are used here because these ARE statuses — pass/fail — which
// is the one job Ok/Warn/Err are reserved for (spec §9). Each also carries a
// glyph and text, so identity is never colour-alone and the row still reads
// under the mono palette.
func renderRow(r row, w int, st *theme.Styles) string {
	line := "  " + glyph(r.state) + " " + cell(r.minion, minionColumn) + " " + r.note

	switch r.state {
	case stateFailed:
		return st.Err.Render(fit(line, w))
	case stateMissing:
		return st.Warn.Render(fit(line, w))
	case stateOK:
		return st.Ok.Render(fit(line, w))
	default:
		return st.Value.Render(fit(line, w))
	}
}

// glyph is the row's status mark.
func glyph(s rowState) string {
	switch s {
	case stateFailed:
		return "✗"
	case stateMissing:
		return "⧗"
	case stateOK:
		return "✓"
	default:
		return " "
	}
}

// truncJID shortens a JID for display, keeping it recognisable.
func truncJID(jid string) string {
	const keep = 14

	if len(jid) <= keep {
		return jid
	}

	return jid[:keep] + "…"
}

// scrollStart is the first visible index that keeps cursor on screen.
func scrollStart(cursor, window, n int) int {
	if cursor < window {
		return 0
	}

	return min(cursor-window+1, max(0, n-window))
}

// clamp fits every line to w cells and joins at most h of them.
//
// The WIDTH pass is not decoration. Three lines here — the index header, the
// empty-list message and the drill-down counts — are composed at a fixed
// natural width (70, 16 and 49 cells) with no reference to the box at all, and
// an over-long line is not merely clipped by the root: lipgloss word-WRAPS it,
// and because the root's frame is Height(contentH) and lipgloss Height is a
// MINIMUM, the frame grows a row per wrap and pushes the status bar off the
// bottom of the terminal. Doing it once, here, is also why a fourth such line
// cannot be added without being fitted — every path out of View goes through
// this function. The other four panes each run the same last-step pass.
//
// components.Fit rather than this package's fit: these lines are already
// STYLED, and sanitising them would replace the ESC introducing each SGR
// sequence and print the escape codes as literal garbage. Bus-derived text is
// sanitised earlier, on its way in, by fit and by components.RenderTable.
func clamp(lines []string, w, h int) string {
	if h < 0 {
		h = 0
	}

	if len(lines) > h {
		lines = lines[:h]
	}

	for i, l := range lines {
		lines[i] = components.Fit(l, w)
	}

	return strings.Join(lines, "\n")
}

// cell renders s into exactly w display cells.
func cell(s string, w int) string { return components.PadTo(fit(s, w), w) }

// fit sanitises UNSTYLED, bus-derived text and truncates it to at most w
// display cells.
//
// The sanitising is the load-bearing half and it is components.Sanitise by
// ruling rather than a local copy: minion IDs and function names are
// minion-supplied, a newline here would split one row across two and
// desynchronise the frame, and a raw ESC would let event data drive the
// terminal of an operator running this as root on a production master.
// components.RenderTable does this for the list view, but the drill-down lays
// its rows out itself in order to colour them per status, so it must do it too.
//
// It carries no Ellipsis marker: unlike a ranked top-N label, every string
// reaching here is either a fixed-width column, whose boundary is itself the
// cue, or a whole sentence the reader can see running to the edge of the box.
func fit(s string, w int) string { return components.Fit(components.Sanitise(s), w) }
