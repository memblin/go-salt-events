// Package live renders the streaming event tail: the events the cache is
// holding, oldest at the top, with a cursor the operator can scroll back
// through while the stream keeps arriving (spec §7.1).
//
// The pane holds no events of its own. Every frame renders from the Snapshot
// it is handed, and it only ever touches the slice window that is on screen,
// so render cost is O(visible rows) whatever the event rate (invariant 6).
package live

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/TKC-Labs/go-salt-events/internal/model"
	"github.com/TKC-Labs/go-salt-events/internal/theme"
	"github.com/TKC-Labs/go-salt-events/internal/ui"
	"github.com/TKC-Labs/go-salt-events/internal/ui/components"
)

// Pane is the Live tail.
type Pane struct {
	cursor int

	// follow keeps the cursor pinned to the newest event. Any manual movement
	// releases it, so scrolling back does not fight the incoming stream; `G`
	// takes it back (spec §7.1, "autoscroll resumes on jumping to the tail").
	follow bool
}

// New returns a Live pane following the tail.
func New() *Pane { return &Pane{cursor: 0, follow: true} }

// Title implements ui.Pane.
func (p *Pane) Title() string { return "Live" }

// SetStyles implements ui.Pane. Nothing here caches styles: every colour comes
// from the *theme.Styles handed to View, so a theme switch needs no work.
func (p *Pane) SetStyles(*theme.Styles) {}

// Keys implements ui.Pane.
//
// The follow hint reports the state the pane is in NOW rather than a fixed
// string: "resume following" on a pane that is already following would read as
// a broken key, and the operator has no other indication that scrolling back
// stopped the autoscroll.
func (p *Pane) Keys() []ui.KeyHint {
	follow := ui.KeyHint{Key: "G", Label: "resume following the tail"}
	if p.follow {
		follow = ui.KeyHint{Key: "G", Label: "following the tail"}
	}

	return []ui.KeyHint{
		{Key: "↑/↓", Label: "move cursor"},
		{Key: "g", Label: "jump to oldest"},
		follow,
	}
}

// Selected returns the currently highlighted event, if any.
//
// This is how the Detail pane is fed: the wiring reads the selection and calls
// detail.SetEvent, so the payload of exactly one event is ever decoded
// (spec §4.2, invariant 4).
func (p *Pane) Selected(s ui.Snapshot) (model.Event, bool) {
	if len(s.Events) == 0 {
		return model.Event{}, false
	}

	return s.Events[p.cursorIn(s)], true
}

// Update handles cursor movement.
func (p *Pane) Update(msg tea.Msg, s ui.Snapshot) (ui.Pane, tea.Cmd) {
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return p, nil
	}

	last := max(0, len(s.Events)-1)

	switch key.String() {
	case "down", "j":
		p.cursor, p.follow = min(p.cursorIn(s)+1, last), false
	case "up", "k":
		p.cursor, p.follow = max(p.cursorIn(s)-1, 0), false
	case "G", "end":
		p.cursor, p.follow = last, true
	case "g", "home":
		p.cursor, p.follow = 0, false
	}

	return p, nil
}

// cursorIn resolves the cursor against s.
//
// It is resolved per frame rather than stored, because the snapshot shrinks
// under the cursor whenever the filter changes or the cache sheds events
// (spec §5.2) — a stored index would then point past the end.
func (p *Pane) cursorIn(s ui.Snapshot) int {
	if len(s.Events) == 0 {
		return 0
	}

	if p.follow {
		return len(s.Events) - 1
	}

	return min(max(0, p.cursor), len(s.Events)-1)
}

// columns for the tail. TAG flexes: it is the field with no natural width and
// the one an operator scans, so it gets whatever is left.
var columns = []components.Column{
	{Title: "AGE", Width: 6, Flex: false},
	{Title: "KIND", Width: 8, Flex: false},
	{Title: "MINION", Width: 18, Flex: false},
	{Title: "TAG", Width: 0, Flex: true},
}

// headerRows is the one line RenderTable spends on the column header.
const headerRows = 1

// View renders the tail into the w×h content box.
func (p *Pane) View(w, h int, s ui.Snapshot, st *theme.Styles) string {
	if h < 1 || w < 1 {
		return ""
	}

	if len(s.Events) == 0 {
		// Clipped like every other line: mid-resize the box can be narrower
		// than this sentence, and a pane that overflows only when it has
		// nothing to show is the case nobody exercises by hand.
		return st.Muted.Render(clip("waiting for events…", w))
	}

	rows := max(1, h-headerRows)
	cursor := p.cursorIn(s)
	start := windowStart(cursor, rows, len(s.Events))
	end := min(len(s.Events), start+rows)

	now := time.Now()
	body := make([][]string, 0, end-start)

	for _, e := range s.Events[start:end] {
		body = append(body, []string{
			age(now, e.Arrival),
			e.Kind.String(),
			e.Minion,
			e.Tag + marker(e),
		})
	}

	sel := cursor - start
	if sel < 0 || sel >= len(body) {
		sel = components.NoSelection
	}

	lines := components.RenderTable(columns, body, sel, w, st)

	// The box can be a single row mid-resize, and RenderTable always returns
	// the header plus one line per row. Overflowing here would push the root's
	// border off its own frame.
	if len(lines) > h {
		lines = lines[:max(0, h)]
	}

	return strings.Join(lines, "\n")
}

// clip truncates s to at most w display cells. The style carries no colour, so
// it is not a palette decision escaping into a pane (spec §3.2).
func clip(s string, w int) string {
	if lipgloss.Width(s) <= w {
		return s
	}

	return lipgloss.NewStyle().MaxWidth(w).Render(s)
}

// windowStart picks the first visible row: the tail by default, scrolled back
// only far enough to keep the cursor on screen. It is O(1) — no walk of the
// event slice (invariant 6).
func windowStart(cursor, rows, n int) int {
	start := max(0, n-rows)
	if cursor < start {
		start = cursor
	}

	return start
}

// The markers for an event whose payload is not intact. They are DIFFERENT
// words on purpose, and the words are the whole point (spec §5.3 case A):
// "trimmed@master" is fixed by raising max_event_size on the master, "shed" by
// raising --max-memory here. One generic marker sends the operator to turn our
// knob for data the master destroyed before we ever saw it.
//
// They are plain text, not styled text, and that is deliberate too:
// components.RenderTable replaces every control character in a cell — a raw
// ESC off the bus must never reach a root operator's terminal — which would
// also eat the ESC introducing any ANSI sequence embedded here, printing the
// escape codes as literal garbage. Words rather than colour is also what
// spec §9 requires: under the mono theme colour carries nothing at all.
const (
	markerMasterTrimmed = " [trimmed@master]"
	markerShed          = " [shed]"
)

// marker annotates an event whose payload is not intact.
func marker(e model.Event) string {
	switch {
	case e.MasterTrimmed:
		return markerMasterTrimmed
	case e.Shed:
		return markerShed
	default:
		return ""
	}
}

// age renders a compact relative time, in arrival terms — never _stamp, which
// is set by whichever process fired the event (spec §4.3, invariant 2).
func age(now, then time.Time) string {
	d := now.Sub(then)

	switch {
	case d < 0:
		return "0s"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
}
