// Package theme owns every colour literal in the application. It is the only
// package permitted to call lipgloss.Color(...) — enforced by the forbidigo
// rule in .golangci.yml (spec §3.2).
//
// Deliberately absent: a categorical series palette. Top-N bars use a single
// accent hue with bar length carrying magnitude and the row label carrying
// identity, because a 256-colour terminal cannot be meaningfully validated for
// colourblind separation, and colouring by rank would repaint rows as the
// ranking reshuffles (spec §9).
package theme

// Color is a hex literal in "#rrggbb" form, or "" meaning "leave it to the
// terminal". Only the mono palette uses the empty form.
type Color string

// Palette is one named colour scheme.
type Palette struct {
	Name string
	Dark bool

	Base        Color // primary pane background
	Surface     Color // status bar, table header row
	Border      Color // unfocused pane border
	BorderFocus Color // focused pane border
	Text        Color // primary foreground
	Muted       Color // labels, help text, recessive chrome
	Accent      Color // headers, sparklines, top-N bars

	// Warn, Err, and Ok are RESERVED for status — job pass/fail, connection
	// state. They are never reused as series colours (spec §9).
	Warn Color
	Err  Color
	Ok   Color
}
