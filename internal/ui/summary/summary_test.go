package summary_test

import (
	"os"
	"regexp"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"

	"github.com/TKC-Labs/go-salt-events/internal/model"
	"github.com/TKC-Labs/go-salt-events/internal/stats"
	"github.com/TKC-Labs/go-salt-events/internal/theme"
	"github.com/TKC-Labs/go-salt-events/internal/ui"
	"github.com/TKC-Labs/go-salt-events/internal/ui/summary"
)

// TestMain pins the colour profile.
//
// Under `go test` stdout is not a terminal, so lipgloss detects the Ascii
// profile and renders every style as plain text — which makes a correct, fully
// themed pane fail the theme guard. If a theme test ever fails, check this pin
// BEFORE touching the assertion.
func TestMain(m *testing.M) {
	lipgloss.SetColorProfile(termenv.TrueColor)
	os.Exit(m.Run())
}

// stylesNamed returns compiled styles for a REGISTERED palette.
//
// theme.StylesFor is the only route to a *theme.Styles from outside
// internal/theme — compile is unexported, so a hand-built palette cannot be
// compiled at all. The pane itself never obtains styles: it receives them as a
// View parameter, which TestOnlyTheRootObtainsStyles enforces across the whole
// of internal/ui.
func stylesNamed(t *testing.T, name string) *theme.Styles {
	t.Helper()

	st, ok := theme.StylesFor(name)
	if !ok {
		t.Fatalf("%s palette missing", name)
	}

	return st
}

func styles(t *testing.T) *theme.Styles {
	t.Helper()

	return stylesNamed(t, "gruvbox-dark")
}

// TestSummaryShowsTheJobIndexHighWaterMark is the brief's first test.
func TestSummaryShowsTheJobIndexHighWaterMark(t *testing.T) {
	t.Parallel()

	// --max-jobs 500 is a starting value, not a sufficient one. The high-water
	// mark is what lets an operator read off the number to configure after a
	// representative session, instead of guessing twice (spec §7.5).
	s := ui.Snapshot{
		JobStats: stats.IndexStats{Tracked: 500, Cap: 500, HighWater: 731, Evicted: 37},
	}

	got := summary.New().View(100, 24, s, styles(t))

	if !strings.Contains(got, "731") {
		t.Errorf("high-water mark missing:\n%s", got)
	}

	if !strings.Contains(got, "--max-jobs") {
		t.Errorf("no guidance naming the knob to turn:\n%s", got)
	}
}

// TestSummaryRendersAllThreeBreakdowns is the brief's second test.
func TestSummaryRendersAllThreeBreakdowns(t *testing.T) {
	t.Parallel()

	s := ui.Snapshot{
		TopCategories: []stats.Entry{{Key: "salt/job/*/ret/*", Pct: 61}},
		TopMinions:    []stats.Entry{{Key: "scache-1", Pct: 38}},
		TopFunctions:  []stats.Entry{{Key: "state.apply", Pct: 55}},
	}

	got := summary.New().View(100, 24, s, styles(t))

	for _, want := range []string{"salt/job/*/ret/*", "scache-1", "state.apply"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q missing:\n%s", want, got)
		}
	}
}

// TestEmptyKeysRenderReadably covers the event class this pane exists to keep
// legible: the master's bare-JID job publish-ack, which carries a deliberately
// EMPTY Category because a bare JID has no category segments. About a fifth of
// real traffic is that shape, so a blank row would be a fifth of the top-N
// table saying nothing.
//
// The substitution is a rendering decision only — nothing upstream stores a
// placeholder, because a stored one would be indistinguishable from a
// minion-sent tag containing the same text (which is exactly how "*" failed).
func TestEmptyKeysRenderReadably(t *testing.T) {
	t.Parallel()

	entry := []stats.Entry{{Key: "", Count: 19, Pct: 19}}

	tests := map[string]struct {
		snap ui.Snapshot
		want string
	}{
		"empty category is the master job publish-ack": {
			snap: ui.Snapshot{TopCategories: entry},
			want: "(master job publish-ack)",
		},
		"empty minion says none": {
			snap: ui.Snapshot{TopMinions: entry},
			want: "(none)",
		},
		"empty function says none": {
			snap: ui.Snapshot{TopFunctions: entry},
			want: "(none)",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got := ansi.Strip(summary.New().View(100, 24, tc.snap, styles(t)))

			if !strings.Contains(got, tc.want) {
				t.Errorf("want %q in:\n%s", tc.want, got)
			}

			if strings.Contains(got, "*") {
				t.Errorf("an empty key must not be shown as a wildcard:\n%s", got)
			}
		})
	}
}

// barGlyphs are the two cells components.Bar draws with.
const barGlyphs = "█░"

// TestNoRankedRowHasABlankLabel is the general form of the rule above: a bar
// and a percentage attached to nothing are unreadable whatever produced the
// empty key.
func TestNoRankedRowHasABlankLabel(t *testing.T) {
	t.Parallel()

	s := ui.Snapshot{
		TopCategories: []stats.Entry{{Key: "", Count: 19, Pct: 19}},
		TopMinions:    []stats.Entry{{Key: "", Count: 3, Pct: 3}},
		TopFunctions:  []stats.Entry{{Key: "", Count: 1, Pct: 1}},
	}

	for _, line := range strings.Split(ansi.Strip(summary.New().View(100, 24, s, styles(t))), "\n") {
		i := strings.IndexAny(line, barGlyphs)
		if i < 0 {
			continue
		}

		if strings.TrimSpace(line[:i]) == "" {
			t.Errorf("ranked row has a blank label: %q", line)
		}
	}
}

// TestLongCategoryIsClippedAtDisplayTime pins the ruling that truncation lives
// HERE and nowhere else. A minion can event.send an arbitrarily long tag, and
// Category derives from it; the stored value stays faithful, and this pane
// shortens it to fit with a visible indicator.
func TestLongCategoryIsClippedAtDisplayTime(t *testing.T) {
	t.Parallel()

	const pathological = 50_000

	for name, key := range map[string]string{
		"ascii":      strings.Repeat("a", pathological),
		"wide runes": strings.Repeat("世", pathological),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			s := ui.Snapshot{TopCategories: []stats.Entry{{Key: key, Count: 1, Pct: 100}}}

			for _, w := range []int{20, 40, 100} {
				got := summary.New().View(w, 24, s, styles(t))

				for _, line := range strings.Split(got, "\n") {
					if lipgloss.Width(line) > w {
						t.Fatalf("w=%d: line is %d cells wide: %q", w, lipgloss.Width(line), line)
					}
				}

				if !strings.Contains(got, "…") {
					t.Errorf("w=%d: clipping left no visible indicator:\n%s", w, ansi.Strip(got))
				}

				// The whole frame is bounded by the box, not by the input: a
				// 50k-character tag must not put 50k characters on the wire to
				// the terminal ten times a second.
				if len(got) > pathological {
					t.Errorf("w=%d: rendered %d bytes for a %d-char key", w, len(got), pathological)
				}
			}
		})
	}
}

// TestControlCharactersNeverReachTheTerminal: this pane lays its own rows out
// rather than routing them through components.RenderTable, so it owns the
// sanitising. A raw ESC from the bus would otherwise drive the terminal of an
// operator running this as root on a production master.
func TestControlCharactersNeverReachTheTerminal(t *testing.T) {
	t.Parallel()

	const attack = "\x1b]0;pwned\x07salt/\nnew\tjob"

	s := ui.Snapshot{
		TopCategories: []stats.Entry{{Key: attack, Count: 1, Pct: 100}},
		TopMinions:    []stats.Entry{{Key: "min\x1b[2Jion", Count: 1, Pct: 100}},
	}

	got := summary.New().View(100, 24, s, styles(t))

	for _, bad := range []string{"\x1b]0;", "\x1b[2J", "\x07", "\t"} {
		if strings.Contains(got, bad) {
			t.Errorf("control sequence %q survived into the output", bad)
		}
	}

	if strings.Count(got, "\n") != strings.Count(summary.New().View(100, 24, ui.Snapshot{}, styles(t)), "\n") {
		t.Error("a newline in bus data changed the pane's line count")
	}
}

// TestRankingsComeOnlyFromTheSnapshot: stats are fed at ingest and never
// derived from the cache (invariant 3). A pane that recomputed a top-N from
// Snapshot.Events would show rows here.
func TestRankingsComeOnlyFromTheSnapshot(t *testing.T) {
	t.Parallel()

	s := ui.Snapshot{
		Events: []model.Event{
			{Tag: "salt/job/20260830/ret/scache-1", Category: "salt/job/*/ret/*", Minion: "scache-1"},
			{Tag: "salt/job/20260830/ret/scache-2", Category: "salt/job/*/ret/*", Minion: "scache-2"},
		},
	}

	got := ansi.Strip(summary.New().View(100, 24, s, styles(t)))

	if strings.Contains(got, "scache-1") || strings.Contains(got, "salt/job/*/ret/*") {
		t.Errorf("rankings were derived from the cached events:\n%s", got)
	}

	if strings.Count(got, "(nothing yet)") != 3 {
		t.Errorf("want all three breakdowns empty:\n%s", got)
	}
}

// TestEvictionIsNeverSilent: a non-zero eviction count is the signal to raise
// --max-jobs, and the knob is named either way so the number can be read off a
// representative session rather than guessed twice (spec §7.5).
func TestEvictionIsNeverSilent(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		stats   stats.IndexStats
		want    []string
		notWant []string
	}{
		"under pressure": {
			stats:   stats.IndexStats{Tracked: 500, Cap: 500, HighWater: 501, Evicted: 37},
			want:    []string{"500/500", "37 evicted", "--max-jobs", "501"},
			notWant: []string{"nothing evicted"},
		},
		"comfortable": {
			stats:   stats.IndexStats{Tracked: 12, Cap: 500, HighWater: 44, Evicted: 0},
			want:    []string{"12/500", "44", "--max-jobs", "nothing evicted"},
			notWant: []string{"evicted —"},
		},
		"before the first tick": {
			stats:   stats.IndexStats{},
			want:    []string{"0/0", "--max-jobs"},
			notWant: []string{"evicted —"},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got := ansi.Strip(summary.New().View(100, 24, ui.Snapshot{JobStats: tc.stats}, styles(t)))

			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("want %q in:\n%s", w, got)
				}
			}

			for _, w := range tc.notWant {
				if strings.Contains(got, w) {
					t.Errorf("did not want %q in:\n%s", w, got)
				}
			}
		})
	}
}

// TestPressureReadoutSurvivesAShortBox: the high-water mark is shown only
// here, so a box too short for everything drops ranked rows before it drops
// the number an operator came for.
func TestPressureReadoutSurvivesAShortBox(t *testing.T) {
	t.Parallel()

	s := ui.Snapshot{
		TopCategories: manyEntries(20),
		TopMinions:    manyEntries(20),
		TopFunctions:  manyEntries(20),
		JobStats:      stats.IndexStats{Tracked: 500, Cap: 500, HighWater: 501, Evicted: 37},
	}

	for _, h := range []int{1, 2, 3, 8, 24} {
		got := ansi.Strip(summary.New().View(100, h, s, styles(t)))

		if !strings.Contains(got, "37 evicted") {
			t.Errorf("h=%d: the pressure readout was dropped:\n%s", h, got)
		}

		if lines := strings.Count(got, "\n") + 1; lines > h {
			t.Errorf("h=%d: rendered %d lines", h, lines)
		}
	}
}

func manyEntries(n int) []stats.Entry {
	out := make([]stats.Entry, 0, n)
	for i := range n {
		out = append(out, stats.Entry{Key: strings.Repeat("k", i+1), Count: uint64(n - i), Pct: float64(i)})
	}

	return out
}

// TestGuidanceSurvivesAnEightyColumnTerminal: the half of the pressure readout
// that would fall off the end of a default-width terminal is the actionable
// half — the knob's name and the number to give it — so the line wraps instead
// of being clipped.
func TestGuidanceSurvivesAnEightyColumnTerminal(t *testing.T) {
	t.Parallel()

	s := ui.Snapshot{JobStats: stats.IndexStats{Tracked: 500, Cap: 500, HighWater: 501, Evicted: 37}}

	for _, w := range []int{40, 60, 78, 80, 100, 200} {
		got := ansi.Strip(summary.New().View(w, 24, s, styles(t)))

		if !strings.Contains(got, "raise --max-jobs above 501") {
			t.Errorf("w=%d: the guidance was clipped:\n%s", w, got)
		}
	}

	// Narrower than the sentence: the prose shortens, the figures do not.
	for _, w := range []int{28, 30, 36} {
		got := ansi.Strip(summary.New().View(w, 24, s, styles(t)))

		if !strings.Contains(got, "--max-jobs >501") {
			t.Errorf("w=%d: the number to configure was lost:\n%s", w, got)
		}
	}
}

// TestFitsItsBox: the root subtracts the border and hands over the CONTENT
// box; a pane that overflowed it would push the frame apart.
func TestFitsItsBox(t *testing.T) {
	t.Parallel()

	s := sample()

	for _, box := range [][2]int{{1, 1}, {2, 3}, {10, 4}, {40, 12}, {100, 24}, {200, 60}} {
		w, h := box[0], box[1]

		got := summary.New().View(w, h, s, styles(t))

		lines := strings.Split(got, "\n")
		if len(lines) > h {
			t.Errorf("%dx%d: rendered %d lines", w, h, len(lines))
		}

		for _, l := range lines {
			if lipgloss.Width(l) > w {
				t.Errorf("%dx%d: line is %d cells wide: %q", w, h, lipgloss.Width(l), ansi.Strip(l))
			}
		}
	}
}

// TestNeverPanics: View is called with a 1x1 content box during a resize and
// with a Snapshot whose slices are all nil before the first tick.
func TestNeverPanics(t *testing.T) {
	t.Parallel()

	snaps := map[string]ui.Snapshot{
		"zero value":  {},
		"populated":   sample(),
		"empty keys":  {TopCategories: []stats.Entry{{}}, TopMinions: []stats.Entry{{}}},
		"nan percent": {TopCategories: []stats.Entry{{Key: "x", Pct: nan()}}},
		"huge key":    {TopCategories: []stats.Entry{{Key: strings.Repeat("z", 50_000), Pct: 1e9}}},
	}

	for name, s := range snaps {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			for _, box := range [][2]int{{-5, -5}, {0, 0}, {1, 1}, {1, 40}, {40, 1}, {3, 2}, {100, 24}} {
				summary.New().View(box[0], box[1], s, styles(t))
			}
		})
	}
}

func nan() float64 {
	zero := 0.0

	return zero / zero
}

// TestKeysBindsNothingDeliberately: nil is the contract's stated answer for a
// pane that owns no key, and this pane owns none — there is nothing to select,
// scroll or toggle.
func TestKeysBindsNothingDeliberately(t *testing.T) {
	t.Parallel()

	if got := summary.New().Keys(); len(got) != 0 {
		t.Errorf("Keys() = %v, want none; a bound key must be advertised here", got)
	}
}

// TestUpdateIsInert: Summary renders aggregates and consumes no input, so it
// must not swallow a key the root or another pane owns.
func TestUpdateIsInert(t *testing.T) {
	t.Parallel()

	p := summary.New()

	next, cmd := p.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'F'}}, sample())

	if cmd != nil {
		t.Error("Summary issued a command for a key it does not own")
	}

	if next != ui.Pane(p) {
		t.Error("Update returned a different pane")
	}
}

// TestTitleIsStable: the root uses it in the tab strip and tests use it as a
// subtest name.
func TestTitleIsStable(t *testing.T) {
	t.Parallel()

	if got := summary.New().Title(); got != "Summary" {
		t.Errorf("Title() = %q, want %q", got, "Summary")
	}
}

// sgrTail matches one SGR sequence at the end of a string.
var sgrTail = regexp.MustCompile("\x1b\\[[0-9;]*m$")

// escapesBefore returns the run of SGR sequences immediately preceding token.
//
// This is what makes the theme assertion specific: it proves the styling
// wrapping THIS token changed, rather than proving that something somewhere in
// the frame changed colour — the weakness that let a root-level theme test
// pass with every pane's styling gutted (Task 15).
func escapesBefore(s, token string) string {
	i := strings.Index(s, token)
	if i < 0 {
		return ""
	}

	head, run := s[:i], ""

	for {
		loc := sgrTail.FindStringIndex(head)
		if loc == nil {
			return run
		}

		run = head[loc[0]:] + run
		head = head[:loc[0]]
	}
}

// TestViewIsStyledByTheThemeItIsGivenAndLaidOutIdentically is the §3.2 guard,
// inside the pane package where View can be compared without root chrome
// around it.
//
// Proven by mutation: replacing this pane's st.X.Render calls with plain text
// makes it FAIL (see the task report's transcript), which is exactly what the
// root-level equivalent cannot do.
func TestViewIsStyledByTheThemeItIsGivenAndLaidOutIdentically(t *testing.T) {
	t.Parallel()

	s := sample()
	gruv := summary.New().View(100, 24, s, stylesNamed(t, "gruvbox-dark"))
	sol := summary.New().View(100, 24, s, stylesNamed(t, "solarized-dark"))
	mono := summary.New().View(100, 24, s, stylesNamed(t, "mono"))

	if ansi.Strip(gruv) != ansi.Strip(sol) || ansi.Strip(gruv) != ansi.Strip(mono) {
		t.Error("the theme changed LAYOUT; ANSI-stripped output must be byte-identical")
	}

	if gruv == sol {
		t.Fatal("the two themes rendered identically; no style reached the output")
	}

	// One token per style this pane applies itself. Each must be wrapped by
	// THIS pane, not merely sit next to something components styled.
	styled := []struct {
		style string
		token string
	}{
		{"Header (block title)", "Top tags"},
		{"Value (ranked label)", "salt/job/*/ret/*"},
		{"Muted (percentage)", " 61.0%"},
		{"Muted (empty block)", "  (nothing yet)"},
		{"KeyLabel (index line)", "jobs "},
		{"Value (occupancy)", "500/500"},
		{"Warn (eviction)", "37 evicted"},
	}

	for _, c := range styled {
		if !strings.Contains(ansi.Strip(gruv), c.token) {
			t.Fatalf("%s: token %q is not in the output at all", c.style, c.token)
		}

		a, b := escapesBefore(gruv, c.token), escapesBefore(sol, c.token)
		if a == b {
			t.Errorf("%s: %q carries no theme-dependent styling (both %q)", c.style, c.token, a)
		}
	}
}

// sample is a snapshot shaped like a real session: a busy return category, the
// bare-JID publish-ack, and a saturated job index.
func sample() ui.Snapshot {
	return ui.Snapshot{
		TopCategories: []stats.Entry{
			{Key: "salt/job/*/ret/*", Count: 1220, Pct: 61},
			{Key: "", Count: 380, Pct: 19},
			{Key: stats.OtherKey, Count: 40, Pct: 2},
		},
		TopMinions:   []stats.Entry{{Key: "scache-1", Count: 760, Pct: 38}},
		TopFunctions: nil,
		JobStats:     stats.IndexStats{Tracked: 500, Cap: 500, HighWater: 501, Evicted: 37},
	}
}
