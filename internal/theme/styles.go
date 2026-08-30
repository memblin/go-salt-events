package theme

import "github.com/charmbracelet/lipgloss"

// monoName is the mono palette's Name. Comparing against this constant rather
// than the "mono" literal keeps the string's occurrence count in this file at
// zero, so goconst's cross-package literal count (registry.go's map key and
// its Name field are the other two) never trips here.
const monoName = "mono"

// Styles is the compiled form of a Palette. The root model owns exactly one at
// a time and passes it into every pane's View at render time, so a pane holds
// no style state and reads the current theme every frame. Switching themes
// therefore costs nothing and loses nothing.
type Styles struct {
	Palette Palette

	Pane      lipgloss.Style
	PaneFocus lipgloss.Style
	Header    lipgloss.Style
	StatusBar lipgloss.Style
	KeyLabel  lipgloss.Style

	Value       lipgloss.Style
	TableHeader lipgloss.Style
	TableRowSel lipgloss.Style
	Toast       lipgloss.Style
	Muted       lipgloss.Style

	// Spark is the sparkline and top-N bar style: ONE hue, because magnitude
	// is carried by length, not colour (spec §9).
	Spark lipgloss.Style

	// Ok, Warn, and Err are status only. Never use them for a series.
	Ok   lipgloss.Style
	Warn lipgloss.Style
	Err  lipgloss.Style
}

// compile builds a *Styles from p. For the mono palette (every slot empty) it
// skips Foreground/Background entirely and relies on Bold and Reverse alone.
//
// It is deliberately unexported. Exported, it was an API-level hole in the
// colour rule: any package could obtain styled colour with no forbidden text
// in its own source, by handing it a Palette built from a struct literal, a
// zero value, or a mutated copy of a registered one. Two of those three
// contain no "theme.Palette{" text at all, so no textual lint rule can close
// them. Such a palette never passes through the registry, and the contrast
// suite iterates Names() — so it would be structurally invisible to contrast
// validation, and an unreadable pane could ship with no failing test
// (spec §3.2).
//
// StylesFor is the only way in from outside, and it sources its palette from
// the registry. Do not re-export this.
func compile(p Palette) *Styles {
	s := &Styles{Palette: p}

	fg := func(st lipgloss.Style, c Color) lipgloss.Style {
		if c == "" {
			return st
		}

		return st.Foreground(lipgloss.Color(string(c)))
	}

	bg := func(st lipgloss.Style, c Color) lipgloss.Style {
		if c == "" {
			return st
		}

		return st.Background(lipgloss.Color(string(c)))
	}

	base := lipgloss.NewStyle()

	s.Pane = fg(base.Border(lipgloss.RoundedBorder()), p.Border)
	s.PaneFocus = fg(base.Border(lipgloss.RoundedBorder()), p.BorderFocus)
	s.Header = fg(base.Bold(true), p.Accent)
	s.StatusBar = bg(fg(base, p.Text), p.Surface)
	s.KeyLabel = fg(base, p.Muted)
	s.Value = fg(base, p.Text)
	s.TableHeader = fg(base.Bold(true), p.Muted)
	s.TableRowSel = base.Reverse(true) // reverse video works in mono too
	s.Toast = fg(base.Bold(true), p.Warn)
	s.Muted = fg(base, p.Muted)
	s.Spark = fg(base, p.Accent)
	s.Ok = fg(base, p.Ok)
	s.Warn = fg(base, p.Warn)
	s.Err = fg(base, p.Err)

	if p.Name == monoName {
		s.PaneFocus = base.Border(lipgloss.RoundedBorder()).Bold(true)
	}

	return s
}
