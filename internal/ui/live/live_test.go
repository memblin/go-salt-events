package live_test

import (
	"os"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"

	"github.com/TKC-Labs/go-salt-events/internal/model"
	"github.com/TKC-Labs/go-salt-events/internal/theme"
	"github.com/TKC-Labs/go-salt-events/internal/ui"
	"github.com/TKC-Labs/go-salt-events/internal/ui/live"
)

// TestMain forces a colour profile: `go test` runs without a TTY, where
// lipgloss would otherwise strip every escape sequence and the theme guard
// below would compare two identical uncoloured strings and pass vacuously.
func TestMain(m *testing.M) {
	lipgloss.SetColorProfile(termenv.TrueColor)
	os.Exit(m.Run())
}

// styles fetches a registered palette's styles.
//
// The task brief called theme.Compile here. That does not compile from this
// package: Compile lives in internal/theme/export_test.go, so it exists only
// inside the theme package's own test binary. theme.StylesFor is the route in
// from outside, and it sources its palette from the registry — which is the
// point (spec §3.2).
func styles(t *testing.T, name string) *theme.Styles {
	t.Helper()

	st, ok := theme.StylesFor(name)
	if !ok {
		t.Fatalf("no such palette %q", name)
	}

	return st
}

func snap(events ...model.Event) ui.Snapshot { return ui.Snapshot{Events: events} }

func TestLiveRendersTagMinionAndAge(t *testing.T) {
	t.Parallel()

	s := snap(model.Event{
		Arrival: time.Now(),
		Tag:     "salt/job/20260830081402123456/ret/scache-1",
		Minion:  "scache-1",
		Kind:    model.KindRet,
	})

	got := live.New().View(100, 10, s, styles(t, "gruvbox-dark"))

	for _, want := range []string{"scache-1", "ret", "0s"} {
		if !strings.Contains(got, want) {
			t.Errorf("view is missing %q:\n%s", want, got)
		}
	}
}

func TestLiveMarksShedAndMasterTrimmedDistinctly(t *testing.T) {
	t.Parallel()

	// Spec §5.3 case A: same symptom, opposite fixes. If the pane rendered one
	// marker for both, the operator would turn the wrong knob.
	s := snap(
		model.Event{Arrival: time.Now(), Tag: "salt/a", Shed: true},
		model.Event{Arrival: time.Now(), Tag: "salt/b", MasterTrimmed: true},
	)

	got := live.New().View(100, 10, s, styles(t, "gruvbox-dark"))

	var shedLine, trimLine string

	for _, l := range strings.Split(got, "\n") {
		if strings.Contains(l, "salt/a") {
			shedLine = l
		}

		if strings.Contains(l, "salt/b") {
			trimLine = l
		}
	}

	if shedLine == "" || trimLine == "" {
		t.Fatalf("both events must render:\n%s", got)
	}

	if lipgloss.Width(shedLine) == 0 || shedLine == trimLine {
		t.Error("shed and master-trimmed events render identically")
	}

	// Stronger than "not identical": each marker must name a DIFFERENT thing,
	// and neither may name the other's knob. Two markers that differed only by
	// a colour would pass the check above and still be invisible under mono.
	if !strings.Contains(ansi.Strip(shedLine), "[shed]") {
		t.Errorf("the shed row does not say so in text:\n%s", shedLine)
	}

	if !strings.Contains(ansi.Strip(trimLine), "[trimmed@master]") {
		t.Errorf("the master-trimmed row does not say so in text:\n%s", trimLine)
	}

	if strings.Contains(ansi.Strip(shedLine), "master") {
		t.Error("the shed marker blames the master; that is the wrong cause and the wrong fix")
	}
}

func TestLiveSelectionMoves(t *testing.T) {
	t.Parallel()

	s := snap(
		model.Event{Arrival: time.Now(), Tag: "salt/a"},
		model.Event{Arrival: time.Now(), Tag: "salt/b"},
	)

	p := live.New()

	before := p.View(100, 10, s, styles(t, "gruvbox-dark"))

	next, _ := p.Update(tea.KeyMsg{Type: tea.KeyUp}, s)

	after := next.View(100, 10, s, styles(t, "gruvbox-dark"))

	if before == after {
		t.Error("moving the cursor changed nothing visible")
	}
}

func TestLiveFollowsTheTailUntilTheCursorMoves(t *testing.T) {
	t.Parallel()

	s := snap(
		model.Event{Arrival: time.Now(), Tag: "salt/a"},
		model.Event{Arrival: time.Now(), Tag: "salt/b"},
	)

	p := live.New()

	if e, ok := p.Selected(s); !ok || e.Tag != "salt/b" {
		t.Errorf("a new pane must follow the tail, selected %+v ok=%v", e, ok)
	}

	if _, cmd := p.Update(tea.KeyMsg{Type: tea.KeyUp}, s); cmd != nil {
		t.Errorf("cursor movement issued a command: %v", cmd)
	}

	if e, _ := p.Selected(s); e.Tag != "salt/a" {
		t.Errorf("scrolling back selected %q, want salt/a", e.Tag)
	}

	// A newly arrived event must NOT drag the cursor along while scrolled back
	// — that is the whole reason follow is released on movement.
	grown := snap(s.Events[0], s.Events[1], model.Event{Arrival: time.Now(), Tag: "salt/c"})
	if e, _ := p.Selected(grown); e.Tag != "salt/a" {
		t.Errorf("a new event moved the released cursor to %q", e.Tag)
	}

	p.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("G")}, grown)

	if e, _ := p.Selected(grown); e.Tag != "salt/c" {
		t.Errorf("G selected %q, want the tail salt/c", e.Tag)
	}
}

func TestLiveRendersWithinItsBoxAndDrawsNoBorder(t *testing.T) {
	t.Parallel()

	// The root owns the frame. A pane that draws its own makes theme
	// switching look like it only applies to some panes.
	s := snap(model.Event{Arrival: time.Now(), Tag: "salt/a"})

	got := live.New().View(40, 5, s, styles(t, "gruvbox-dark"))

	for _, glyph := range []string{"╭", "╰", "│", "─"} {
		if strings.Contains(got, glyph) {
			t.Errorf("pane drew a border glyph %q; the root owns the frame", glyph)
		}
	}

	for i, line := range strings.Split(got, "\n") {
		if w := lipgloss.Width(line); w > 40 {
			t.Errorf("line %d width = %d, exceeds the 40-cell content box", i, w)
		}
	}
}

// TestLiveNeverExceedsItsBoxAndNeverPanics covers the two states the contract
// singles out: a content box as small as 1x1 during a resize, and a Snapshot
// whose slices are all nil before the first tick.
func TestLiveNeverExceedsItsBoxAndNeverPanics(t *testing.T) {
	t.Parallel()

	many := make([]model.Event, 200)
	for i := range many {
		many[i] = model.Event{Arrival: time.Now(), Tag: "salt/job/x/ret/minion", Minion: "m", Kind: model.KindRet}
	}

	cases := []struct {
		name string
		w, h int
		s    ui.Snapshot
	}{
		{"nil snapshot", 80, 24, ui.Snapshot{}},
		{"one by one", 1, 1, snap(many...)},
		{"one by one empty", 1, 1, ui.Snapshot{}},
		{"zero box", 0, 0, snap(many...)},
		{"negative box", -3, -3, snap(many...)},
		{"taller than the data", 80, 100, snap(many[:2]...)},
		{"shorter than the data", 80, 5, snap(many...)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := live.New().View(tc.w, tc.h, tc.s, styles(t, "gruvbox-dark"))

			lines := strings.Split(got, "\n")
			if got == "" {
				lines = nil
			}

			if tc.h >= 0 && len(lines) > tc.h && len(tc.s.Events) > 0 {
				t.Errorf("rendered %d lines into a box %d tall", len(lines), tc.h)
			}

			for i, l := range lines {
				if w := lipgloss.Width(l); tc.w >= 0 && w > tc.w {
					t.Errorf("line %d width = %d, exceeds the %d-cell box", i, w, tc.w)
				}
			}
		})
	}
}

// TestLiveKeysAdvertiseWhatItBinds guards the reason ui.Pane.Keys exists: a
// bound key that is not listed ships undiscoverable.
func TestLiveKeysAdvertiseWhatItBinds(t *testing.T) {
	t.Parallel()

	p := live.New()

	listed := map[string]bool{}
	for _, k := range p.Keys() {
		listed[k.Key] = true

		if k.Label == "" {
			t.Errorf("key %q has no label", k.Key)
		}
	}

	for _, want := range []string{"↑/↓", "g", "G"} {
		if !listed[want] {
			t.Errorf("Keys() does not advertise %q", want)
		}
	}

	// It reports what is true NOW, not a fixed list (ui.Pane.Keys).
	following := p.Keys()

	p.Update(tea.KeyMsg{Type: tea.KeyUp}, snap(model.Event{Tag: "salt/a"}))

	if scrolledBack := p.Keys(); scrolledBack[len(scrolledBack)-1] == following[len(following)-1] {
		t.Error("the follow hint reads the same whether or not the pane is following")
	}
}

// TestLiveStylesItsOwnOutput is the pane-level theme guard.
//
// The root-level equivalent (ui.TestThemeSwitchChangesColourButNotLayout)
// passes even with every pane's styling gutted, because the root's own chrome
// changes colour and the frames still differ. Here there is no chrome: if this
// pane stops passing the *theme.Styles through to what it renders, the two
// frames become byte-identical and this fails.
//
// Verified by mutation: replacing the components.RenderTable call in View with
// an unstyled strings.Join over the same cells makes this test FAIL and leaves
// every other test in the package passing.
func TestLiveStylesItsOwnOutput(t *testing.T) {
	t.Parallel()

	// An arrival well in the past keeps the age cell stable ("1m") across the
	// two renders; "0s" could tick over to "1s" between them and fail the
	// layout comparison for a reason that has nothing to do with theming.
	s := snap(
		model.Event{Arrival: time.Now().Add(-90 * time.Second), Tag: "salt/a", Minion: "scache-1", Kind: model.KindRet},
		model.Event{Arrival: time.Now().Add(-90 * time.Second), Tag: "salt/b", Minion: "scache-2", Kind: model.KindNew},
	)

	gruvbox := live.New().View(60, 6, s, styles(t, "gruvbox-dark"))
	solarized := live.New().View(60, 6, s, styles(t, "solarized-dark"))

	if gruvbox == solarized {
		t.Errorf("two palettes rendered identical output; the pane is not styling anything:\n%s", gruvbox)
	}

	if ansi.Strip(gruvbox) != ansi.Strip(solarized) {
		t.Errorf("the theme changed LAYOUT:\n%s\n---\n%s", ansi.Strip(gruvbox), ansi.Strip(solarized))
	}

	// mono has every palette slot empty on purpose, so it must still render
	// the same text — that is where the [shed] / [trimmed@master] wording,
	// rather than colour, carries the whole distinction (spec §9).
	if mono := live.New().View(60, 6, s, styles(t, "mono")); ansi.Strip(mono) != ansi.Strip(gruvbox) {
		t.Errorf("mono changed layout:\n%s", ansi.Strip(mono))
	}
}

// TestLiveSanitisesHostileTagText: tags come off the bus and are therefore
// minion-supplied. A raw ESC reaching a root operator's terminal is a real
// escalation, not a cosmetic bug.
func TestLiveSanitisesHostileTagText(t *testing.T) {
	t.Parallel()

	s := snap(model.Event{
		Arrival: time.Now(),
		Tag:     "salt/\x1b]0;pwned\x07evil\nsecond-line",
		Minion:  "m\x1b[31m",
	})

	got := live.New().View(80, 6, s, styles(t, "mono"))

	if strings.Contains(got, "\x1b]0;") || strings.Contains(got, "\x07") {
		t.Errorf("an escape sequence from the bus reached the output: %q", got)
	}

	if n := len(strings.Split(got, "\n")); n > 6 {
		t.Errorf("a newline in a tag split the row: %d lines", n)
	}
}
