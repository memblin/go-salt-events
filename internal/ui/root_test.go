package ui_test

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"

	"github.com/TKC-Labs/go-salt-events/internal/filter"
	"github.com/TKC-Labs/go-salt-events/internal/model"
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

	// hints is what Keys reports. Most stubs leave it nil, which is the
	// ordinary case of a pane that binds nothing and must render no hint group
	// at all.
	hints []ui.KeyHint

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

func (p *stubPane) Keys() []ui.KeyHint { return p.hints }

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

// stubPanes mirrors the real build: most panes bind nothing of their own, and
// Rate owns the `F` toggle from spec §9 — the binding that motivated Keys.
func stubPanes() []*stubPane {
	return []*stubPane{
		{title: "Live"}, {title: "Detail"},
		{title: "Rate", hints: []ui.KeyHint{{Key: "F", Label: "fit"}}},
		{title: "Summary"}, {title: "Jobs"},
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

// hintRow returns the rendered hint line, ANSI stripped. It is the chrome row
// directly above the status bar (tabs, frame, filter bar, HINTS, status), so
// asserting on it proves the hints reached the frame in the place the operator
// reads, not merely that the string appears somewhere on screen.
func hintRow(t *testing.T, view string) string {
	t.Helper()

	lines := strings.Split(ansi.Strip(view), "\n")
	if len(lines) < 2 {
		t.Fatalf("view has %d line(s); there is no hint row", len(lines))
	}

	return lines[len(lines)-2]
}

// TestTheFocusedPanesKeysReachTheHintLine is the reason Keys exists. A method
// nothing renders leaves a binding exactly as undiscoverable as no method at
// all, so this asserts the rendered row rather than the method's return value.
//
// The separator is the marker for "a pane contributed a group": it appears on
// this row only when there are pane hints, so its absence is how the empty
// cases prove nothing dangling was drawn.
func TestTheFocusedPanesKeysReachTheHintLine(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		hints   []ui.KeyHint
		want    []string
		notWant []string
	}{
		{
			name:  "a pane-owned key is advertised alongside the global keys",
			hints: []ui.KeyHint{{Key: "F", Label: "fit"}},
			want:  []string{"F fit", "q quit", "·"},
		},
		{
			name: "several keys keep the pane's own order",
			hints: []ui.KeyHint{
				{Key: "F", Label: "fit"},
				{Key: "enter", Label: "open"},
			},
			want: []string{"F fit  enter open"},
		},
		{
			name:    "a pane with no keys contributes no group",
			hints:   nil,
			want:    []string{"q quit"},
			notWant: []string{"·"},
		},
		{
			name:    "an empty hint is dropped rather than drawn",
			hints:   []ui.KeyHint{{}},
			want:    []string{"q quit"},
			notWant: []string{"·"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			m := ui.NewModel(stubSource{},
				[]ui.Pane{&stubPane{title: "Rate", hints: c.hints}},
				ui.Options{Theme: "gruvbox-dark"})

			row := hintRow(t, ready(t, m, 120, 24).View())

			for _, want := range c.want {
				if !strings.Contains(row, want) {
					t.Errorf("hint row %q does not contain %q", row, want)
				}
			}

			for _, notWant := range c.notWant {
				if strings.Contains(row, notWant) {
					t.Errorf("hint row %q contains %q, want it absent", row, notWant)
				}
			}
		})
	}
}

// TestSwitchingPanesSwitchesTheHints covers the state the hint line exists for:
// a pane-owned binding is only live while that pane is focused, so a stale
// hint would advertise a key that does nothing.
func TestSwitchingPanesSwitchesTheHints(t *testing.T) {
	t.Parallel()

	m := ready(t, newModel(t), 120, 30)

	if row := hintRow(t, keys(t, m, "3").View()); !strings.Contains(row, "F fit") {
		t.Errorf("focusing Rate did not show its own key: hint row %q", row)
	}

	if row := hintRow(t, keys(t, m, "1").View()); strings.Contains(row, "F fit") {
		t.Errorf("focusing a keyless pane left Rate's key on screen: hint row %q", row)
	}
}

// pinningPane reports a pinned JID, the way the Jobs pane does while drilled in.
type pinningPane struct {
	stubPane

	jid string
}

func (p *pinningPane) PinnedJID() string { return p.jid }

// viewerPane records what the root asked it to display, the way Detail does.
type viewerPane struct {
	stubPane

	got model.Event
}

func (p *viewerPane) SetEvent(e model.Event) { p.got = e }

// pinningSource records the pins the root pushed at it.
type pinningSource struct {
	stubSource

	pins []string
}

func (s *pinningSource) PinJob(jid string) { s.pins = append(s.pins, jid) }

// TestTheRootPinsWhateverAPaneIsHolding is the carry-forward the Jobs pane
// could not do itself: it exposes PinnedJID, and without this call the pinned
// job scrolls and evicts exactly as if it were never pinned — pinning that
// looks like it works.
func TestTheRootPinsWhateverAPaneIsHolding(t *testing.T) {
	t.Parallel()

	holder := &pinningPane{stubPane: stubPane{title: "Jobs"}}
	src := &pinningSource{}

	m := ui.NewModel(src, []ui.Pane{&stubPane{title: "Live"}, holder}, ui.Options{
		Theme: "gruvbox-dark", Interval: time.Hour,
	})
	m = ready(t, m, 100, 30)

	m = step(t, m, ui.TickMsg(time.Now()))

	holder.jid = "20260830081402123456"
	m = step(t, m, ui.TickMsg(time.Now()))

	holder.jid = ""
	_ = step(t, m, ui.TickMsg(time.Now()))

	want := []string{"", "20260830081402123456", ""}
	if !slices.Equal(src.pins, want) {
		t.Errorf("pins pushed = %v, want %v", src.pins, want)
	}
}

// TestPinningSurvivesAPause: the pin is what stops the job the operator is
// READING from being evicted, and pausing is exactly when they are reading it.
func TestPinningSurvivesAPause(t *testing.T) {
	t.Parallel()

	holder := &pinningPane{stubPane: stubPane{title: "Jobs"}, jid: "20260830081402123456"}
	src := &pinningSource{}

	m := ui.NewModel(src, []ui.Pane{holder}, ui.Options{Theme: "gruvbox-dark", Interval: time.Hour})
	m = ready(t, m, 100, 30)
	m = keys(t, m, " ") // pause
	_ = step(t, m, ui.TickMsg(time.Now()))

	if len(src.pins) == 0 || src.pins[len(src.pins)-1] != "20260830081402123456" {
		t.Errorf("pins pushed while paused = %v, want the held job", src.pins)
	}
}

// TestAPaneCommandReachesTheRoot is the routing fix itself. Model.Update's
// default arm forwards to the FOCUSED pane, so before the root grew a case for
// it, a message a pane emitted came straight back to that pane and the
// drill-through of spec §7.2 could not be bound at all.
func TestAPaneCommandReachesTheRoot(t *testing.T) {
	t.Parallel()

	viewer := &viewerPane{stubPane: stubPane{title: "Detail"}}

	m := ui.NewModel(stubSource{}, []ui.Pane{&stubPane{title: "Live"}, viewer}, ui.Options{
		Theme: "gruvbox-dark", Interval: time.Hour,
	})
	m = ready(t, m, 100, 30)

	m = step(t, m, ui.OpenDetailMsg{Event: model.Event{Tag: "salt/job/x/ret/web-1"}})

	if viewer.got.Tag != "salt/job/x/ret/web-1" {
		t.Errorf("the Detail pane was handed %q, want the opened event", viewer.got.Tag)
	}

	// And the root focuses it, because an event opened on a pane nobody is
	// looking at is not opened.
	if !strings.Contains(m.View(), "[Detail]") {
		t.Errorf("opening an event did not focus the Detail pane:\n%s", m.View())
	}
}

// TestOpeningAJobReturnThatIsNotCachedSaysSo: the return may have been shed,
// dropped, or hidden by the filter. All three are ordinary, and a key that
// silently does nothing is indistinguishable from a broken one.
func TestOpeningAJobReturnThatIsNotCachedSaysSo(t *testing.T) {
	t.Parallel()

	viewer := &viewerPane{stubPane: stubPane{title: "Detail"}}

	m := ui.NewModel(stubSource{}, []ui.Pane{&stubPane{title: "Live"}, viewer}, ui.Options{
		Theme: "gruvbox-dark", Interval: time.Hour,
	})
	m = ready(t, m, 100, 30)

	m = step(t, m, ui.OpenJobReturnMsg{JID: "20260830081402123456", Minion: "web-041"})

	view := ansi.Strip(m.View())

	if !strings.Contains(view, "web-041") {
		t.Errorf("the miss was not reported to the operator:\n%s", view)
	}

	if viewer.got.Tag != "" {
		t.Errorf("Detail was handed a fabricated event: %+v", viewer.got)
	}
}

// TestOpeningAJobReturnFindsItInTheSnapshot is the other half: when the return
// IS cached, the pair resolves to the event and Detail gets it.
func TestOpeningAJobReturnFindsItInTheSnapshot(t *testing.T) {
	t.Parallel()

	viewer := &viewerPane{stubPane: stubPane{title: "Detail"}}

	want := model.Event{
		Tag: "salt/job/20260830081402123456/ret/web-041", Kind: model.KindRet,
		JID: "20260830081402123456", Minion: "web-041",
	}

	// A second, NEWER return for the same job from a different minion. Without
	// it a root that matched on the JID alone would still find the right event
	// by luck, and this test could not fail.
	src := stubSource{snap: ui.Snapshot{Events: []model.Event{
		{Tag: "salt/auth"},
		want,
		{
			Tag: "salt/job/20260830081402123456/ret/web-999", Kind: model.KindRet,
			JID: "20260830081402123456", Minion: "web-999",
		},
	}}}

	m := ui.NewModel(src, []ui.Pane{&stubPane{title: "Live"}, viewer}, ui.Options{
		Theme: "gruvbox-dark", Interval: time.Hour,
	})
	m = ready(t, m, 100, 30)
	m = step(t, m, ui.TickMsg(time.Now()))

	_ = step(t, m, ui.OpenJobReturnMsg{JID: want.JID, Minion: want.Minion})

	if viewer.got.Tag != want.Tag {
		t.Errorf("Detail was handed %q, want %q", viewer.got.Tag, want.Tag)
	}
}

// TestExportRunsOffTheRenderLoopAndReportsBack: `w` is on the permanent hint
// strip, so it must be bound; and the write must happen in a tea.Cmd rather
// than inside Update, because a several-hundred-megabyte NDJSON write on the
// render goroutine would freeze the frame (spec §10.3).
func TestExportRunsOffTheRenderLoopAndReportsBack(t *testing.T) {
	t.Parallel()

	var gotQuery string

	m := ui.NewModel(stubSource{}, []ui.Pane{&stubPane{title: "Live"}}, ui.Options{
		Theme:    "gruvbox-dark",
		Interval: time.Hour,
		Filter:   mustQuery(t, "salt/auth"),
		Export: func(q filter.Query) (string, error) {
			gotQuery = q.String()

			return "wrote 3 events to /var/tmp/salt-events.ndjson", nil
		},
	})
	m = ready(t, m, 120, 30)

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'w'}})
	if cmd == nil {
		t.Fatal("w returned no command: the advertised export key does nothing")
	}

	if gotQuery != "" {
		t.Error("the export ran inside Update rather than in the command")
	}

	m = step(t, next.(ui.Model), cmd())

	if gotQuery != "salt/auth" {
		t.Errorf("export received query %q, want the active filter", gotQuery)
	}

	if !strings.Contains(ansi.Strip(m.View()), "wrote 3 events") {
		t.Errorf("the export result never reached the operator:\n%s", ansi.Strip(m.View()))
	}
}

// TestARefusedExportIsReported: the refusals are the feature. One that vanished
// would leave the operator believing a file exists (invariant 8).
func TestARefusedExportIsReported(t *testing.T) {
	t.Parallel()

	m := ui.NewModel(stubSource{}, []ui.Pane{&stubPane{title: "Live"}}, ui.Options{
		Theme:    "gruvbox-dark",
		Interval: time.Hour,
		Export: func(filter.Query) (string, error) {
			return "", errors.New("not enough free space to export safely")
		},
	})
	m = ready(t, m, 120, 30)

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'w'}})
	m = step(t, next.(ui.Model), cmd())

	if !strings.Contains(ansi.Strip(m.View()), "not enough free space") {
		t.Errorf("the refusal never reached the operator:\n%s", ansi.Strip(m.View()))
	}
}

// TestANoticeIsClearedByTheNextKeystroke: it reports what just happened, and a
// stale one read as current is worse than none.
func TestANoticeIsClearedByTheNextKeystroke(t *testing.T) {
	t.Parallel()

	m := ready(t, newModel(t), 120, 30)
	m = step(t, m, ui.NoticeMsg("something happened"))

	if !strings.Contains(ansi.Strip(m.View()), "something happened") {
		t.Fatal("premise failed: the notice never rendered")
	}

	m = keys(t, m, "1")

	if strings.Contains(ansi.Strip(m.View()), "something happened") {
		t.Error("the notice survived a keystroke")
	}
}

// TestHelpOverlayCarriesWhatIsOtherwiseUndiscoverable. Three things live here
// because they exist nowhere else: the focused pane's own keys, the filter
// language, and the resolved config path — spec §11 wants the last of these
// visible so a config file that is not being read is diagnosable without
// strace.
func TestHelpOverlayCarriesWhatIsOtherwiseUndiscoverable(t *testing.T) {
	t.Parallel()

	panes := []ui.Pane{
		&stubPane{title: "Live"},
		&stubPane{title: "Rate", hints: []ui.KeyHint{{Key: "F", Label: "fix the scale"}}},
	}

	m := ui.NewModel(stubSource{}, panes, ui.Options{
		Theme:      "gruvbox-dark",
		Interval:   time.Hour,
		SockPath:   "/var/run/salt/master/master_event_pub.ipc",
		ConfigPath: "/home/operator/.config/salt-events/config.toml",
	})
	m = ready(t, m, 120, 40)

	// Focus Rate, then open help: the overlay must report the FOCUSED pane's
	// keys, not a fixed list.
	m = keys(t, m, "2")

	closed := ansi.Strip(m.View())
	open := ansi.Strip(keys(t, m, "?").View())

	// The pane's keys are ALSO on the hint line under the frame, so presence
	// alone would pass against an overlay that lists nothing. What the overlay
	// must do is add an occurrence.
	if strings.Count(open, "fix the scale") <= strings.Count(closed, "fix the scale") {
		t.Errorf("the overlay does not list the focused pane's keys:\n%s", open)
	}

	for _, want := range []string{
		"minion:?*",
		"/var/run/salt/master/master_event_pub.ipc",
		"/home/operator/.config/salt-events/config.toml",
	} {
		if !strings.Contains(open, want) {
			t.Errorf("the help overlay does not mention %q:\n%s", want, open)
		}

		// And it comes from the overlay, not from chrome that is always drawn.
		if strings.Contains(closed, want) {
			t.Errorf("%q is on screen with the overlay closed, so this proves nothing", want)
		}
	}
}

// mustQuery compiles a filter query for a test.
func mustQuery(t *testing.T, s string) filter.Query {
	t.Helper()

	q, err := filter.Parse(s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}

	return q
}
