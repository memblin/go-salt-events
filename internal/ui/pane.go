package ui

import (
	tea "github.com/charmbracelet/bubbletea"

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
}
