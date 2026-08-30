package ui_test

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/TKC-Labs/go-salt-events/internal/filter"
	"github.com/TKC-Labs/go-salt-events/internal/theme"
	"github.com/TKC-Labs/go-salt-events/internal/ui"
)

// TestMain pins the colour profile. Under `go test` stdout is not a terminal,
// so lipgloss picks the Ascii profile and renders every style as plain text —
// which makes the theme guard fail on CORRECT panes. If that test ever fails,
// check this pin before touching the assertion.
func TestMain(m *testing.M) {
	lipgloss.SetColorProfile(termenv.TrueColor)
	os.Exit(m.Run())
}

// stubSource returns a fixed snapshot and counts how often it is asked for one,
// which is how the pause test distinguishes "the view froze" from "ingest
// stopped".
type stubSource struct {
	snap  ui.Snapshot
	calls *int
}

func (s stubSource) Snapshot(filter.Query, int) ui.Snapshot {
	if s.calls != nil {
		*s.calls++
	}

	return s.snap
}

// stubPane is a minimal Pane for root tests. Its body is bracketed so an
// assertion can tell the CONTENT box apart from the tab strip, which also
// contains every pane title.
type stubPane struct {
	title string

	// st and viewSt record the two separate routes styles reach a pane by, so
	// the theme guard can tell "SetStyles was called" from "View was handed
	// the new set". A real pane can get one and miss the other.
	st     *theme.Styles
	viewSt *theme.Styles
}

func (p *stubPane) Title() string { return p.title }

func (p *stubPane) Update(tea.Msg, ui.Snapshot) (ui.Pane, tea.Cmd) { return p, nil }

func (p *stubPane) View(w, _ int, _ ui.Snapshot, st *theme.Styles) string {
	p.viewSt = st

	body := "[" + p.title + "]"

	return st.Value.Render(body + strings.Repeat("-", max(0, w-len(body))))
}

func (p *stubPane) SetStyles(st *theme.Styles) { p.st = st }

func ready(t *testing.T, m ui.Model, w, h int) ui.Model {
	t.Helper()

	return step(t, m, tea.WindowSizeMsg{Width: w, Height: h})
}

func step(t *testing.T, m ui.Model, msg tea.Msg) ui.Model {
	t.Helper()

	updated, _ := m.Update(msg)

	got, ok := updated.(ui.Model)
	if !ok {
		t.Fatal("Update did not return a ui.Model")
	}

	return got
}

func keys(t *testing.T, m ui.Model, s string) ui.Model {
	t.Helper()

	for _, r := range s {
		m = step(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}

	return m
}

func newModelWith(t *testing.T, src ui.Source) ui.Model {
	t.Helper()

	stubs := stubPanes()

	panes := make([]ui.Pane, 0, len(stubs))
	for _, p := range stubs {
		panes = append(panes, p)
	}

	return ui.NewModel(src, panes, ui.Options{
		Theme: "gruvbox-dark", Interval: 100 * time.Millisecond,
	})
}

// newModelPanes returns the model together with the concrete stubs, for the
// tests that assert on what the root handed each pane.
func newModelPanes(t *testing.T) (ui.Model, []*stubPane) {
	t.Helper()

	stubs := stubPanes()

	panes := make([]ui.Pane, 0, len(stubs))
	for _, p := range stubs {
		panes = append(panes, p)
	}

	return ui.NewModel(stubSource{}, panes, ui.Options{
		Theme: "gruvbox-dark", Interval: 100 * time.Millisecond,
	}), stubs
}

func stubPanes() []*stubPane {
	return []*stubPane{
		{title: "Live"}, {title: "Detail"},
		{title: "Rate"}, {title: "Summary"}, {title: "Jobs"},
	}
}

func newModel(t *testing.T) ui.Model {
	t.Helper()

	return newModelWith(t, stubSource{})
}

func TestNothingRendersBeforeAWindowSizeMsg(t *testing.T) {
	t.Parallel()

	// Without the gate the first frame draws at a fabricated 80x24 and then
	// visibly snaps to the real size.
	if got := newModel(t).View(); got != "" {
		t.Errorf("View() before resize = %q, want empty", got)
	}
}

// TestDegenerateSizesNeverPanic covers the sizes a terminal really sends. A
// resize delivers zero and negative dimensions before the first size message,
// and Task 14 already found the brief's layout() panicking on exactly that.
func TestDegenerateSizesNeverPanic(t *testing.T) {
	t.Parallel()

	sizes := []struct {
		name string
		w, h int
	}{
		{"zero", 0, 0},
		{"negative", -5, -2},
		{"one cell", 1, 1},
		{"no room for chrome", 3, 4},
		{"tall and thin", 1, 200},
	}

	for _, sz := range sizes {
		t.Run(sz.name, func(t *testing.T) {
			t.Parallel()

			m := ready(t, newModel(t), sz.w, sz.h)

			_ = m.View()
			_ = keys(t, m, "3t /").View()
		})
	}
}

// TestTheFrameFillsTheWindowExactly ties chromeRows to the number of rows the
// chrome actually emits. They are two different facts today and nothing else
// notices when they disagree: add a chrome row without touching the constant
// and the bottom line is silently clipped off the terminal — including the
// status bar, which is where DISCONNECTED is displayed.
func TestTheFrameFillsTheWindowExactly(t *testing.T) {
	t.Parallel()

	for _, h := range []int{10, 24, 30, 60} {
		t.Run(fmt.Sprintf("h=%d", h), func(t *testing.T) {
			t.Parallel()

			got := lipgloss.Height(ready(t, newModel(t), 100, h).View())
			if got != h {
				t.Errorf("View() is %d rows in a %d-row window", got, h)
			}
		})
	}
}

func TestDigitKeysJumpPanes(t *testing.T) {
	t.Parallel()

	m := keys(t, ready(t, newModel(t), 100, 30), "3")

	if !strings.Contains(m.View(), "[Rate]") {
		t.Error("pressing 3 did not render the Rate pane's body")
	}
}

func TestDigitPastTheLastPaneIsIgnored(t *testing.T) {
	t.Parallel()

	// "9" on a five-pane build is an ordinary keystroke, not a programming
	// error: it must not index out of range.
	m := keys(t, ready(t, newModel(t), 100, 30), "39")

	if !strings.Contains(m.View(), "[Rate]") {
		t.Error("an out-of-range digit moved focus")
	}
}

func TestTabCyclesPanesAndWraps(t *testing.T) {
	t.Parallel()

	m := ready(t, newModel(t), 100, 30)

	for range 5 {
		m = step(t, m, tea.KeyMsg{Type: tea.KeyTab})
	}

	if !strings.Contains(m.View(), "[Live]") {
		t.Error("tab did not wrap back to the first pane")
	}
}

func TestPauseFreezesTheViewNotIngest(t *testing.T) {
	t.Parallel()

	// Invariant 7: a paused UI that stopped collecting would silently lose the
	// storm the operator paused to read. Pausing must only skip the snapshot
	// refresh — it must never touch the reader, and the root has no handle on
	// the reader to touch. What it can get wrong is re-reading the snapshot,
	// so that is what is asserted.
	calls := 0
	m := ready(t, newModelWith(t, stubSource{calls: &calls}), 100, 30)

	m = step(t, m, tea.KeyMsg{Type: tea.KeySpace})

	if !strings.Contains(m.View(), "PAUSED") {
		t.Fatal("pause state is not visible in the status bar")
	}

	m = step(t, m, ui.TickMsg(time.Now()))

	if calls != 0 {
		t.Errorf("a tick while paused refreshed the snapshot %d times, want 0", calls)
	}

	m = step(t, m, tea.KeyMsg{Type: tea.KeySpace})
	_ = step(t, m, ui.TickMsg(time.Now()))

	if calls != 1 {
		t.Errorf("resuming refreshed the snapshot %d times, want 1", calls)
	}
}

func TestMalformedFilterKeepsThePreviousQuery(t *testing.T) {
	t.Parallel()

	// An empty pane reads as "there are no such events", which is a different
	// and much worse message than "your query is wrong" (spec §6).
	calls := 0
	m := ready(t, newModelWith(t, stubSource{calls: &calls}), 100, 30)

	m = step(t, keys(t, m, "/ok:maybe"), tea.KeyMsg{Type: tea.KeyEnter})

	if !strings.Contains(m.View(), "filter:") {
		t.Error("a malformed query produced no visible error")
	}

	if calls != 0 {
		t.Error("a malformed query refreshed the snapshot, discarding the previous view")
	}
}

func TestValidFilterIsAppliedAndRefreshes(t *testing.T) {
	t.Parallel()

	calls := 0
	m := ready(t, newModelWith(t, stubSource{calls: &calls}), 100, 30)

	m = step(t, keys(t, m, "/minion:web-1"), tea.KeyMsg{Type: tea.KeyEnter})

	if calls != 1 {
		t.Errorf("committing a filter refreshed %d times, want 1", calls)
	}

	if !strings.Contains(m.View(), "minion:web-1") {
		t.Error("the active filter is not shown in the filter bar")
	}
}

func TestUnknownThemeFallsBackToTheDefault(t *testing.T) {
	t.Parallel()

	// A typo in a config file must not stop an incident console from starting.
	m := ui.NewModel(stubSource{}, []ui.Pane{&stubPane{title: "Live"}},
		ui.Options{Theme: "no-such-palette"})

	if got := m.ThemeName(); got != theme.DefaultName {
		t.Errorf("ThemeName() = %q, want %q", got, theme.DefaultName)
	}
}

func TestNoPanesRendersNothingRatherThanPanicking(t *testing.T) {
	t.Parallel()

	m := ready(t, ui.NewModel(stubSource{}, nil, ui.Options{}), 80, 24)

	m = step(t, m, tea.KeyMsg{Type: tea.KeyTab})

	if got := m.View(); got != "" {
		t.Errorf("View() with no panes = %q, want empty", got)
	}
}

func TestPanesAreGivenStylesAtConstruction(t *testing.T) {
	t.Parallel()

	// SetStyles is mandatory because a bubbles component holds styles the View
	// parameter does not reach; a pane that never receives them renders a
	// one-line body rather than nothing, which is easy to miss.
	p := &stubPane{title: "Live"}
	_ = ui.NewModel(stubSource{}, []ui.Pane{p}, ui.Options{Theme: "mono"})

	if p.st == nil {
		t.Error("NewModel did not call SetStyles on its panes")
	}
}
