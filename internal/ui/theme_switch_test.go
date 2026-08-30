package ui_test

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/TKC-Labs/go-salt-events/internal/theme"
)

// TestThemeSwitchChangesColourButNotLayout is the §3.2 layout guard.
//
// Know exactly what it does and does not catch, because it is weaker at the
// root than the same assertion is inside a pane package, and reading it as
// stronger than it is would be worse than not having it.
//
// It catches LAYOUT drift: ANSI-stripped output must be byte-identical across
// two themes, so a pane whose geometry varies with the palette fails here.
//
// It does NOT catch an unthemed pane. This was verified by mutation, not
// assumed: replacing the whole stub pane body with lipgloss.NewStyle().Render
// leaves this test PASSING, because the root's own chrome — tabs, borders,
// hints, status bar — is themed and its colours still change, so the frames
// still differ. At the root, "the output changed" says nothing about which
// part changed. TestSwitchingThemesReachesEveryPane is the assertion with
// teeth here; the equivalent colour-difference check belongs in each pane's
// own package (Tasks 16-19), where View can be compared without chrome around
// it.
func TestThemeSwitchChangesColourButNotLayout(t *testing.T) {
	t.Parallel()

	m := ready(t, newModel(t), 100, 30)

	before := m.View()

	after := keys(t, m, "t")
	afterView := after.View()

	if before == afterView {
		t.Error("switching themes changed nothing at all; the styles never reached the frame")
	}

	if layoutOf(before) != layoutOf(afterView) {
		t.Error("switching themes changed LAYOUT; a pane varies geometry with the theme")
	}

	if after.ThemeName() == m.ThemeName() {
		t.Error("the theme name did not advance")
	}
}

// layoutOf strips ANSI and drops the status row.
//
// The status row is excluded because it PRINTS the palette name, which is the
// one piece of text on screen that is legitimately different lengths under
// different themes ("mono" vs "solarized-light"). Comparing it would make the
// guard fail on correct code, and padding the name to a fixed width to keep
// the guard happy would be tailoring the product to the test. Everything the
// guard actually exists to protect — the tab strip, the pane frame, and the
// pane body — is above that row and is still compared byte for byte.
func layoutOf(view string) string {
	lines := strings.Split(ansi.Strip(view), "\n")
	if len(lines) == 0 {
		return ""
	}

	return strings.Join(lines[:len(lines)-1], "\n")
}

// TestSwitchingThemesReachesEveryPane is the guard with teeth at this level.
//
// The root owns the single active *theme.Styles, so what can actually go wrong
// here is distribution: a switch that restyles the chrome but leaves a pane on
// the old palette. On screen that reads as a half-broken theme rather than as
// a bug in one file, and the whole-frame colour comparison above cannot see it
// — so this asserts the plumbing directly, on BOTH routes styles travel:
// SetStyles (for components that cache their own) and the View parameter (for
// everything else).
func TestSwitchingThemesReachesEveryPane(t *testing.T) {
	t.Parallel()

	m, panes := newModelPanes(t)
	m = ready(t, m, 100, 30)
	_ = m.View()

	m = keys(t, m, "t")
	_ = m.View()

	want := m.ThemeName()

	for _, p := range panes {
		if p.st == nil || p.st.Palette.Name != want {
			t.Errorf("pane %q was not given the %q styles via SetStyles", p.title, want)
		}
	}

	// Only the focused pane is rendered, so only it can be checked this way.
	if focused := panes[0]; focused.viewSt == nil || focused.viewSt.Palette.Name != want {
		t.Errorf("pane %q was rendered with stale styles", focused.title)
	}
}

// TestThemeCycleVisitsEveryRegisteredPaletteWithoutPanicking walks the whole
// registry, including mono — whose every slot is empty, and which is therefore
// the one palette where a style that renders nothing is CORRECT rather than a
// bug.
func TestThemeCycleVisitsEveryRegisteredPaletteWithoutPanicking(t *testing.T) {
	t.Parallel()

	m := ready(t, newModel(t), 100, 30)
	seen := map[string]bool{}

	for range len(theme.Names()) {
		m = keys(t, m, "t")
		seen[m.ThemeName()] = true

		if m.View() == "" {
			t.Fatalf("theme %q rendered an empty frame", m.ThemeName())
		}
	}

	if len(seen) != len(theme.Names()) {
		t.Errorf("cycling visited %d palettes, want %d", len(seen), len(theme.Names()))
	}
}
