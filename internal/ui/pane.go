package ui

import (
	tea "github.com/charmbracelet/bubbletea"

	"github.com/TKC-Labs/go-salt-events/internal/model"
	"github.com/TKC-Labs/go-salt-events/internal/theme"
)

// Pane is implemented by every feature pane. The panes live in SUBPACKAGES
// (internal/ui/live, ui/rate, ui/summary, ui/jobs) and code against this
// interface, so changing it means touching all of them.
//
// The theming contract is the load-bearing part:
//
//   - View receives the style set as a parameter, so a pane holds no style
//     state and reads the current theme every frame. Switching themes costs
//     nothing and loses nothing — no refetch, no lost scroll position. A pane
//     must never call theme.StylesFor itself; the root is the sole place a
//     *theme.Styles is obtained, and TestOnlyTheRootObtainsStyles enforces it.
//
//   - SetStyles is mandatory even though most panes implement it as a no-op.
//     bubbles components each hold an internal Styles struct fixed at
//     construction that the View parameter does not reach. Making the method
//     mandatory means a pane that later grows a bubbles table cannot silently
//     forget to restyle it — the gap becomes a visible one-line body rather
//     than an absence.
//
// A pane must NEVER construct a lipgloss.Style from a colour literal, and must
// NOT draw its own border: the root owns the frame and passes the CONTENT box,
// border already subtracted. Three of five panes in the sibling project
// go-saltext-valkey-tui once drew no border at all because that half of the
// contract was left implicit.
//
// View is also called with a content box that can be as small as 1x1 during a
// resize, and with a Snapshot whose slices are all nil before the first tick.
// Neither may panic.
type Pane interface {
	// Title is the pane's name in the tab strip. Tests use it as a subtest
	// name, so it must be stable.
	Title() string

	// Update handles a message. Panes are stored by interface value, so a
	// value receiver may return a modified copy.
	Update(msg tea.Msg, s Snapshot) (Pane, tea.Cmd)

	// View renders into a w×h CONTENT box using st for every colour. It must
	// not vary its LAYOUT with the theme — the theme guard asserts that
	// ANSI-stripped output is byte-identical across two themes.
	View(w, h int, s Snapshot, st *theme.Styles) string

	// SetStyles re-styles any component caching its own styles.
	SetStyles(st *theme.Styles)

	// Keys lists the keys THIS pane owns, for the hint line the root renders
	// under the frame. The global keys (pane jumps, theme, filter, pause,
	// help, quit) are already on that line from the keymap, so repeating them
	// here only makes the pane's own keys harder to pick out.
	//
	// It is called every frame, so a pane whose bindings depend on its state
	// should report what is true NOW rather than a fixed list.
	//
	// Returning nil is legitimate for a pane that binds nothing, and is the
	// point of the method being mandatory rather than optional: a pane that
	// never grew a hint line is otherwise indistinguishable from one that has
	// no keys, and the difference cannot be tested. A method makes the empty a
	// deliberate, reviewable, testable choice — and the root renders whatever
	// comes back, so the Rate pane's `F` toggle (spec §9) cannot ship
	// undiscoverable.
	Keys() []KeyHint
}

// EventViewer is implemented by the pane that can display a single event —
// Detail, and only Detail (spec §7.2, invariant 4: it is the one place a
// payload is fully decoded).
//
// The root discovers it by type assertion rather than by index, so a build that
// omits Detail loses the drill-through and nothing else, and so no pane needs
// to know which slot another pane occupies.
type EventViewer interface {
	Pane

	// SetEvent selects the event to display.
	SetEvent(e model.Event)
}

// JobPinner is implemented by a pane that is holding one job open and needs it
// kept out of the job index's eviction path — Jobs, while it is drilled in.
//
// It reports the state every tick rather than emitting pin and unpin events:
// the pin is a level, not an edge, so a missed message would leave a job pinned
// forever and quietly shrink the index by one.
type JobPinner interface {
	// PinnedJID is the job to protect, or "" when nothing is pinned.
	PinnedJID() string
}

// KeyHint is one pane-owned key and what it does, as shown on the hint line.
//
// Two fields, not more. The root owns the separator, the styling, and the
// truncation; the pane owns only the order. A pane that could pass a style or
// a width through here would drift from its neighbours, which is exactly how
// the sibling project's per-pane footers ended up formatted three different
// ways.
//
// Key is written the way the operator types it — "F", "enter", "←/→" for a
// pair that does one job. Label is the verb phrase, lowercase, no trailing
// punctuation: "fit to window", "next job".
type KeyHint struct {
	// Key is the keystroke, as printed.
	Key string
	// Label is what pressing it does.
	Label string
}
