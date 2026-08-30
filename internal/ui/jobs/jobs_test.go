package jobs_test

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"

	"github.com/TKC-Labs/go-salt-events/internal/model"
	"github.com/TKC-Labs/go-salt-events/internal/stats"
	"github.com/TKC-Labs/go-salt-events/internal/theme"
	"github.com/TKC-Labs/go-salt-events/internal/ui"
	"github.com/TKC-Labs/go-salt-events/internal/ui/jobs"
)

func TestMain(m *testing.M) {
	lipgloss.SetColorProfile(termenv.TrueColor)
	os.Exit(m.Run())
}

// styles compiles a registered palette.
//
// theme.StylesFor rather than theme.Compile: Compile lives in the theme
// package's own export_test.go, which is compiled only when testing THAT
// package, so it is not reachable from here. StylesFor is the only route in
// from outside, and TestOnlyTheRootObtainsStyles skips _test.go files, so
// calling it in a test does not breach the pane contract — jobs.go itself
// never mentions it.
func styles(t *testing.T) *theme.Styles {
	t.Helper()

	st, ok := theme.StylesFor("gruvbox-dark")
	if !ok {
		t.Fatal("gruvbox-dark is not registered")
	}

	return st
}

// bigJob builds a 1000-target job with 812 returns and 23 failures.
func bigJob(state model.ExpectedState) *model.Job {
	j := model.NewJob("20260830081402123456")
	j.Fun = "state.apply"
	j.Tgt = "webs"
	j.Start = time.Date(2026, 8, 30, 8, 14, 2, 0, time.UTC)
	j.ExpectedState = state

	if state == model.ExpectedKnown {
		for i := range 1000 {
			j.AddExpected(fmt.Sprintf("web-%04d", i))
		}
	}

	for i := range 812 {
		code := 0
		if i < 23 {
			code = 1
		}

		j.AddReturn(model.RetInfo{
			Minion:  fmt.Sprintf("web-%04d", i),
			RetCode: code,
			Success: code == 0,
			Arrival: j.Start.Add(time.Duration(i) * time.Millisecond),
		})
	}

	return j
}

func snapWith(j *model.Job) ui.Snapshot {
	return ui.Snapshot{
		Jobs:     []model.JobRow{j.Row()},
		JobStats: stats.IndexStats{Tracked: 1, Cap: 500},
		JobLookup: func(string) (*model.Job, stats.Lookup) {
			return j, stats.LookupFound
		},
	}
}

// drill enters the drill-down for the first job.
func drill(t *testing.T, p *jobs.Pane, s ui.Snapshot) ui.Pane {
	t.Helper()

	out, _ := p.Update(tea.KeyMsg{Type: tea.KeyEnter}, s)

	return out
}

// press sends a rune key.
func press(p ui.Pane, s ui.Snapshot, r rune) ui.Pane {
	out, _ := p.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}}, s)

	return out
}

func TestJobsListShowsReturnedOverExpected(t *testing.T) {
	t.Parallel()

	got := jobs.New().View(120, 24, snapWith(bigJob(model.ExpectedKnown)), styles(t))

	if !strings.Contains(got, "812/1000") {
		t.Errorf("returned/expected missing:\n%s", got)
	}
}

func TestJobsRendersTheThreeExpectedStatesDistinctly(t *testing.T) {
	t.Parallel()

	// Spec §5.3 case B. The bug this guards is two states collapsing into one,
	// so these MUST be three different renderings — asserting that is the
	// whole point of the test.
	st := styles(t)

	known := jobs.New().View(120, 24, snapWith(bigJob(model.ExpectedKnown)), st)
	trimmed := jobs.New().View(120, 24, snapWith(bigJob(model.ExpectedTrimmed)), st)
	unseen := jobs.New().View(120, 24, snapWith(bigJob(model.ExpectedUnseen)), st)

	if known == trimmed || known == unseen || trimmed == unseen {
		t.Error("the three expected-count states do not render distinctly")
	}

	if !strings.Contains(known, "812/1000") {
		t.Error("known state must show the real denominator")
	}

	if !strings.Contains(trimmed, "⚠") {
		t.Errorf("trimmed state must be marked:\n%s", trimmed)
	}

	if !strings.Contains(unseen, "?") {
		t.Errorf("unseen state must be marked:\n%s", unseen)
	}
}

func TestJobsNeverRendersZeroMissingWhenExpectedIsUnknown(t *testing.T) {
	t.Parallel()

	// Invariant 10. "0 missing" reads as "everything returned" and would send
	// an operator away from broken machines. Both unknown states are checked:
	// the fabricated zero is equally wrong for either cause.
	for _, tc := range []struct {
		name  string
		state model.ExpectedState
	}{
		{"trimmed by the master", model.ExpectedTrimmed},
		{"never seen", model.ExpectedUnseen},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			s := snapWith(bigJob(tc.state))
			got := ansi.Strip(drill(t, jobs.New(), s).View(120, 30, s, styles(t)))

			if strings.Contains(got, "0 missing") {
				t.Errorf("rendered a fabricated missing count:\n%s", got)
			}

			// The literal above only catches one spelling, and the tally reads
			// "missing     0". ANY digit after the word is a fabricated count,
			// however it is spaced, so that is what is asserted.
			idx := strings.Index(got, "missing")
			if idx < 0 {
				t.Fatalf("the drill-down does not mention missing at all:\n%s", got)
			}

			if after := strings.TrimLeft(got[idx+len("missing"):], " "); after != "" &&
				after[0] >= '0' && after[0] <= '9' {
				t.Errorf("a number follows \"missing\" on an unknown expected set:\n%s", got)
			}

			if !strings.Contains(strings.ToLower(got), "unknown") {
				t.Errorf("must say the expected set is unknown:\n%s", got)
			}
		})
	}
}

func TestJobsDrillDownShowsFailedAndMissingFirst(t *testing.T) {
	t.Parallel()

	// At 1000 targets the operator cannot read every row, so the actionable
	// ones must be at the top regardless of job size (spec §7.5).
	s := snapWith(bigJob(model.ExpectedKnown))
	got := drill(t, jobs.New(), s).View(120, 30, s, styles(t))

	if !strings.Contains(got, "188") {
		t.Errorf("missing count (1000-812) not shown:\n%s", got)
	}

	if !strings.Contains(got, "23") {
		t.Errorf("failed count not shown:\n%s", got)
	}

	// A failed minion must appear before an ok one.
	failedAt := strings.Index(got, "web-0000")
	okAt := strings.Index(got, "web-0100")

	if failedAt < 0 {
		t.Fatalf("no failed minion rendered:\n%s", got)
	}

	if okAt >= 0 && okAt < failedAt {
		t.Error("an ok minion sorted above a failed one")
	}
}

func TestJobsDrillDownNamesMissingMinions(t *testing.T) {
	t.Parallel()

	// "which ones" is the actual question; a count alone is not actionable.
	//
	// `f` cycles needs-attention → failed only → missing only → all
	// (spec §7.5), so reaching the missing-only view takes two presses. The
	// brief's version of this test pressed once and asserted a missing minion;
	// that is the failed-only view, where naming one would be a bug. Spec
	// beats brief.
	s := snapWith(bigJob(model.ExpectedKnown))

	failedOnly := press(drill(t, jobs.New(), s), s, 'f')
	if got := failedOnly.View(120, 30, s, styles(t)); strings.Contains(got, "web-0812") {
		t.Errorf("the failed-only view named a missing minion:\n%s", got)
	}

	got := press(failedOnly, s, 'f').View(120, 30, s, styles(t))

	if !strings.Contains(got, "web-0812") {
		t.Errorf("a missing minion is not named:\n%s", got)
	}
}

func TestJobsViewCycleReturnsToTheDefault(t *testing.T) {
	t.Parallel()

	s := snapWith(bigJob(model.ExpectedKnown))
	st := styles(t)

	p := drill(t, jobs.New(), s)
	want := p.View(120, 30, s, st)

	seen := map[string]bool{want: true}

	for range 3 {
		p = press(p, s, 'f')
		seen[p.View(120, 30, s, st)] = true
	}

	if len(seen) != 4 {
		t.Errorf("the four `f` views are not four distinct renderings (%d distinct)", len(seen))
	}

	if got := press(p, s, 'f').View(120, 30, s, st); got != want {
		t.Error("`f` did not wrap back to the default view")
	}
}

func TestJobsPinsTheDrilledJob(t *testing.T) {
	t.Parallel()

	// A job vanishing out from under the cursor while being read is the worst
	// possible moment to lose it (spec §7.5).
	s := snapWith(bigJob(model.ExpectedKnown))

	p := jobs.New()
	if p.PinnedJID() != "" {
		t.Errorf("PinnedJID = %q before any drill-down, want empty", p.PinnedJID())
	}

	drilled, ok := drill(t, p, s).(*jobs.Pane)
	if !ok {
		t.Fatal("not a *jobs.Pane")
	}

	if drilled.PinnedJID() != "20260830081402123456" {
		t.Errorf("PinnedJID = %q, want the drilled job", drilled.PinnedJID())
	}

	back, _ := drilled.Update(tea.KeyMsg{Type: tea.KeyEsc}, s)

	if jp, isPane := back.(*jobs.Pane); !isPane || jp.PinnedJID() != "" {
		t.Error("esc must release the pin")
	}
}

func TestJobsHeaderReportsEvictionPressure(t *testing.T) {
	t.Parallel()

	s := snapWith(bigJob(model.ExpectedKnown))
	s.JobStats = stats.IndexStats{Tracked: 500, Cap: 500, HighWater: 731, Evicted: 37}

	got := jobs.New().View(120, 24, s, styles(t))

	if !strings.Contains(got, "37") || !strings.Contains(got, "--max-jobs") {
		t.Errorf("eviction must never be silent:\n%s", got)
	}
}

func TestJobsDistinguishesUnseenFromEvicted(t *testing.T) {
	t.Parallel()

	// stats.Lookup keeps these apart because the fixes differ: evicted is
	// answered by --max-jobs, unseen by attaching sooner. Inventing a job for
	// either would fabricate every count on the drill-down screen
	// (invariant 10).
	base := snapWith(bigJob(model.ExpectedKnown))
	st := styles(t)

	lookups := map[stats.Lookup]string{
		stats.LookupEvicted: "",
		stats.LookupUnseen:  "",
	}

	for kind := range lookups {
		s := base
		s.JobLookup = func(string) (*model.Job, stats.Lookup) { return nil, kind }

		lookups[kind] = ansi.Strip(drill(t, jobs.New(), base).View(120, 20, s, st))
	}

	evicted, unseen := lookups[stats.LookupEvicted], lookups[stats.LookupUnseen]

	if evicted == unseen {
		t.Errorf("evicted and unseen render identically:\n%s", evicted)
	}

	if !strings.Contains(evicted, "--max-jobs") {
		t.Errorf("evicted must name the knob that fixes it:\n%s", evicted)
	}

	if strings.Contains(unseen, "--max-jobs") {
		t.Errorf("unseen must not blame the job index:\n%s", unseen)
	}

	for kind, got := range lookups {
		// A job we do not hold has no counts at all; printing one would be
		// the fabrication invariant 10 forbids.
		if strings.Contains(got, "missing") || strings.Contains(got, "812") {
			t.Errorf("%v invented job detail:\n%s", kind, got)
		}
	}
}

func TestJobsSurvivesNilAndTinySnapshots(t *testing.T) {
	t.Parallel()

	// ui.Snapshot's slices are all nil before the first tick and JobLookup is
	// a nil func field; the content box can be 1x1 mid-resize. Neither may
	// panic.
	st := styles(t)
	populated := snapWith(bigJob(model.ExpectedKnown))

	cases := []struct {
		name    string
		drilled bool
		snap    ui.Snapshot
	}{
		{"zero snapshot, list", false, ui.Snapshot{}},
		{"zero snapshot, drilled", true, ui.Snapshot{}},
		{"nil JobLookup with jobs present", true, ui.Snapshot{Jobs: populated.Jobs}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var p ui.Pane = jobs.New()
			if tc.drilled {
				p = drill(t, jobs.New(), populated)
			}

			for _, size := range [][2]int{{1, 1}, {0, 0}, {-1, -1}, {3, 2}, {200, 60}} {
				if got := p.View(size[0], size[1], tc.snap, st); size[1] > 0 &&
					strings.Count(got, "\n") >= size[1] {
					t.Errorf("%v: rendered more than %d lines", size, size[1])
				}
			}
		})
	}
}

// TestJobsFitsItsBox is the width assertion this package did not have.
//
// Its absence is why the defect shipped: the nearest equivalent,
// TestJobsSurvivesNilAndTinySnapshots, checked HEIGHT only and ran every size
// against an empty or nil-JobLookup snapshot, so it never rendered a POPULATED
// list or a drill-down into a small box. Two blind spots intersecting exactly
// on the bug.
//
// Overflow here is not a clipped line. The root's frame is
// PaneFocus.Width(contentW).Height(contentH) and lipgloss Height is a MINIMUM,
// so an over-long line word-wraps, the frame grows a row per wrap, and the
// status bar scrolls off the bottom of the terminal. 60 and 72 are in the list
// because that is where it bites — a split tmux pane, not an 80-column
// terminal, which is presumably why nobody saw it.
func TestJobsFitsItsBox(t *testing.T) {
	t.Parallel()

	st := styles(t)

	// A non-zero eviction count is what makes the index header long: the quiet
	// form is a dozen cells and would never have caught this.
	populated := snapWith(bigJob(model.ExpectedKnown))
	populated.JobStats = stats.IndexStats{Tracked: 500, Cap: 500, Evicted: 37, HighWater: 501}

	empty := ui.Snapshot{JobStats: populated.JobStats}

	renders := map[string]func(s ui.Snapshot, w, h int) string{
		"list": func(s ui.Snapshot, w, h int) string {
			return jobs.New().View(w, h, s, st)
		},
		"drill-down": func(s ui.Snapshot, w, h int) string {
			return drill(t, jobs.New(), populated).View(w, h, s, st)
		},
		"drill-down, all rows": func(s ui.Snapshot, w, h int) string {
			p := drill(t, jobs.New(), populated)

			return press(press(press(p, populated, 'f'), populated, 'f'), populated, 'f').
				View(w, h, s, st)
		},
	}

	snaps := map[string]ui.Snapshot{"populated": populated, "empty": empty}

	for rname, render := range renders {
		for sname, s := range snaps {
			t.Run(rname+", "+sname, func(t *testing.T) {
				t.Parallel()

				for _, box := range [][2]int{
					{1, 1}, {8, 4}, {16, 6}, {20, 10}, {40, 12},
					{60, 20}, {72, 20}, {80, 24}, {120, 30},
				} {
					w, h := box[0], box[1]
					got := render(s, w, h)

					lines := strings.Split(got, "\n")
					if len(lines) > h {
						t.Errorf("%dx%d: rendered %d lines into a %d-line box",
							w, h, len(lines), h)
					}

					for i, l := range lines {
						if lw := lipgloss.Width(l); lw > w {
							t.Errorf("%dx%d: line %d is %d cells wide: %q",
								w, h, i, lw, ansi.Strip(l))
						}
					}
				}
			})
		}
	}
}

func TestJobsKeysReportWhatIsBoundNow(t *testing.T) {
	t.Parallel()

	// A key not listed here ships undiscoverable; that is the whole reason
	// ui.Pane makes Keys mandatory.
	s := snapWith(bigJob(model.ExpectedKnown))

	list := keyStrings(jobs.New().Keys())
	if !list["enter"] || !list["↑/↓"] {
		t.Errorf("the list view must advertise enter and ↑/↓, got %v", list)
	}

	drilled := keyStrings(drill(t, jobs.New(), s).Keys())
	for _, k := range []string{"f", "esc", "↑/↓", "enter"} {
		if !drilled[k] {
			t.Errorf("the drill-down must advertise %q, got %v", k, drilled)
		}
	}
}

// TestJobsDrillDownEnterOpensTheSelectedMinionsReturn is the §7.5 drill-through
// from this side. The pane holds no events — a job carries only the eagerly
// extracted fields (invariant 9) — so all it can do is name the pair and let
// the root resolve it against the cache.
//
// The cursor is moved first: an assertion on row 0 alone passes just as well
// for a pane that always opens the first row.
func TestJobsDrillDownEnterOpensTheSelectedMinionsReturn(t *testing.T) {
	t.Parallel()

	s := snapWith(bigJob(model.ExpectedKnown))

	// The default view is needs-attention, which puts the 23 failed minions
	// first, sorted: web-0000, web-0001, …
	p := drill(t, jobs.New(), s)
	p, _ = p.Update(tea.KeyMsg{Type: tea.KeyDown}, s)

	_, cmd := p.Update(tea.KeyMsg{Type: tea.KeyEnter}, s)
	if cmd == nil {
		t.Fatal("enter on a minion row returned no command: the drill-through is unbound")
	}

	open, ok := cmd().(ui.OpenJobReturnMsg)
	if !ok {
		t.Fatalf("enter emitted %T, want ui.OpenJobReturnMsg", cmd())
	}

	if open.Minion != "web-0001" {
		t.Errorf("enter opened %q, want the selected row web-0001", open.Minion)
	}

	if open.JID != "20260830081402123456" {
		t.Errorf("enter named job %q, want the drilled JID", open.JID)
	}
}

// TestJobsDrillDownEnterOnAMissingMinionDeclinesWithAReason: a missing minion
// never returned, so there is nothing to open. Emitting the ordinary open
// message would make the root report "it may have aged out of the cache",
// sending the operator to look for an event that never existed.
func TestJobsDrillDownEnterOnAMissingMinionDeclinesWithAReason(t *testing.T) {
	t.Parallel()

	s := snapWith(bigJob(model.ExpectedKnown))

	// attention → failed → missing.
	p := press(press(drill(t, jobs.New(), s), s, 'f'), s, 'f')

	_, cmd := p.Update(tea.KeyMsg{Type: tea.KeyEnter}, s)
	if cmd == nil {
		t.Fatal("enter on a missing row returned no command, so the key silently does nothing")
	}

	msg := cmd()

	if _, wrong := msg.(ui.OpenJobReturnMsg); wrong {
		t.Fatal("enter on a missing minion asked the root to open a return that never existed")
	}

	notice, ok := msg.(ui.NoticeMsg)
	if !ok {
		t.Fatalf("enter emitted %T, want ui.NoticeMsg", msg)
	}

	// web-0812 is the first minion with no return: 1000 expected, 812 returned.
	if !strings.Contains(string(notice), "web-0812") {
		t.Errorf("the notice does not name the minion: %q", notice)
	}
}

// TestJobsDurationCountsUpFromTheSnapshotClock is spec §7.5's live `dur`, and
// invariant 2's edge of it: the reading comes from the snapshot's arrival-side
// clock, never from anything a minion sent.
func TestJobsDurationCountsUpFromTheSnapshotClock(t *testing.T) {
	t.Parallel()

	job := bigJob(model.ExpectedKnown) // 812 of 1000 returned: still running
	s := snapWith(job)

	// LastRet is Start + 811ms, so a frozen duration reads well under a second.
	frozen := jobs.New().View(120, 24, s, styles(t))
	if !strings.Contains(frozen, "811ms") {
		t.Fatalf("premise failed: a clockless snapshot should freeze dur at the last return:\n%s",
			firstTableRow(frozen))
	}

	s.Now = job.Start.Add(4*time.Minute + 12*time.Second)

	live := jobs.New().View(120, 24, s, styles(t))
	if !strings.Contains(live, "4m12s") {
		t.Errorf("dur did not count up to the snapshot clock:\n%s", firstTableRow(live))
	}
}

// TestJobsDurationOfAFinishedJobDoesNotCountUp: a completed job's duration is a
// fact about the job, not about how long the operator has been looking at it.
func TestJobsDurationOfAFinishedJobDoesNotCountUp(t *testing.T) {
	t.Parallel()

	job := model.NewJob("20260830081402123456")
	job.Fun = "test.ping"
	job.ExpectedState = model.ExpectedKnown
	job.Start = time.Date(2026, 8, 30, 8, 14, 2, 0, time.UTC)
	job.AddExpected("web-1")
	job.AddReturn(model.RetInfo{Minion: "web-1", Success: true, Arrival: job.Start.Add(1500 * time.Millisecond)})

	s := snapWith(job)
	s.Now = job.Start.Add(time.Hour)

	got := jobs.New().View(120, 24, s, styles(t))
	if !strings.Contains(got, "1.5s") {
		t.Errorf("a complete job's dur moved with the clock:\n%s", firstTableRow(got))
	}
}

// firstTableRow is the job row, for a readable failure message.
func firstTableRow(view string) string {
	lines := strings.Split(view, "\n")
	if len(lines) < 3 {
		return view
	}

	return strings.Join(lines[:3], "\n")
}

// firstLine is the pane's own header row, without the table below it.
func firstLine(view string) string {
	line, _, _ := strings.Cut(view, "\n")

	return line
}

func keyStrings(hints []ui.KeyHint) map[string]bool {
	out := make(map[string]bool, len(hints))
	for _, h := range hints {
		out[h.Key] = true
	}

	return out
}

func TestJobsSanitisesMinionSuppliedText(t *testing.T) {
	t.Parallel()

	// Minion IDs and function names come off the bus. A newline would
	// desynchronise the frame; a raw ESC would let event data drive the
	// operator's terminal, and this tool is expected to run as root on a
	// production master.
	//
	// The assertion is against the RAW output, per injected sequence. An
	// earlier version tested strings.Contains(ansi.Strip(got), "\x1b"), which
	// cannot fail for the escape half of its claim: ansi.Strip REMOVES
	// well-formed CSI sequences, including the injected "\x1b[2J", so it was
	// asserting only that a MALFORMED escape had survived stripping — the one
	// case an attacker would never send. Mutation-proved: deleting sanitise
	// from jobs.fit left that version, and the whole Jobs suite, green.
	//
	// None of the tokens below can be produced by this pane's own styling: SGR
	// sequences begin "\x1b[" and terminate in "m", never in "J", and neither
	// OSC (ESC ]) nor BEL nor a tab appears anywhere in a theme's output.
	j := model.NewJob("20260830081402123456")
	j.Fun = "state.\x1b[2Japply"
	j.Tgt = "webs\x1b]0;pwned\x07"
	j.ExpectedState = model.ExpectedKnown
	j.AddExpected("evil\nminion")
	j.AddExpected("tab\thost\x1b]0;pwned\x07")
	j.AddExpected("web-01.dc1.example.com")
	j.AddReturn(model.RetInfo{Minion: "web-01.dc1.example.com", RetCode: 0, Success: true})
	j.AddReturn(model.RetInfo{Minion: "bad\x1b[2Jminion", RetCode: 1, Success: false})

	s := snapWith(j)
	st := styles(t)

	views := map[string]string{
		"list":          jobs.New().View(120, 24, s, st),
		"drill-down":    drill(t, jobs.New(), s).View(120, 24, s, st),
		"missing only":  press(press(drill(t, jobs.New(), s), s, 'f'), s, 'f').View(120, 24, s, st),
		"all rows":      press(press(press(drill(t, jobs.New(), s), s, 'f'), s, 'f'), s, 'f').View(120, 24, s, st),
		"narrow drill":  drill(t, jobs.New(), s).View(60, 24, s, st),
		"narrow list":   jobs.New().View(60, 24, s, st),
		"tiny drill":    drill(t, jobs.New(), s).View(12, 8, s, st),
		"one-cell list": jobs.New().View(1, 1, s, st),
	}

	for name, got := range views {
		for _, bad := range []string{"\x1b[2J", "\x1b]0;", "\x07", "\t", "evil\nminion"} {
			if strings.Contains(got, bad) {
				t.Errorf("%s: bus-supplied %q reached the terminal unsanitised:\n%q",
					name, bad, got)
			}
		}
	}
}

// TestJobsThemeChangesColourButNotLayout is the §3.2 guard with teeth.
//
// The root-level version of this assertion passes even with every pane's
// styling gutted, because the root's own chrome changes colour (see
// internal/ui/theme_switch_test.go). Here there is no chrome: the compared
// strings are this pane's View output and nothing else, so replacing any
// st.X.Render call with a plain string makes the raw outputs converge and
// this test fail. Verified by mutation, not assumed — see the task report.
func TestJobsThemeChangesColourButNotLayout(t *testing.T) {
	t.Parallel()

	one, ok := theme.StylesFor("gruvbox-dark")
	if !ok {
		t.Fatal("gruvbox-dark is not registered")
	}

	two, ok := theme.StylesFor("solarized-dark")
	if !ok {
		t.Fatal("solarized-dark is not registered")
	}

	s := snapWith(bigJob(model.ExpectedKnown))
	s.JobStats = stats.IndexStats{Tracked: 500, Cap: 500, HighWater: 731, Evicted: 37}

	unknown := snapWith(bigJob(model.ExpectedTrimmed))

	views := map[string]func(st *theme.Styles) string{
		"list": func(st *theme.Styles) string {
			return jobs.New().View(120, 24, s, st)
		},
		// The list body is coloured by components.RenderTable, so the whole
		// view above stays styled even if this pane's own chrome is gutted.
		// The index header is the part jobs.go styles itself, and it is the
		// eviction warning — the one line that must not go quiet.
		"list index header": func(st *theme.Styles) string {
			return firstLine(jobs.New().View(120, 24, s, st))
		},
		"drill-down": func(st *theme.Styles) string {
			return drill(t, jobs.New(), s).View(120, 30, s, st)
		},
		"drill-down, all rows": func(st *theme.Styles) string {
			p := drill(t, jobs.New(), s)

			return press(press(press(p, s, 'f'), s, 'f'), s, 'f').View(120, 30, s, st)
		},
		"unknown expected set": func(st *theme.Styles) string {
			return drill(t, jobs.New(), unknown).View(120, 30, unknown, st)
		},
	}

	for name, render := range views {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			a, b := render(one), render(two)

			if ansi.Strip(a) == a {
				t.Fatalf("no styling reached the output at all:\n%s", a)
			}

			if a == b {
				t.Errorf("the palette did not reach this pane's output:\n%s", a)
			}

			if ansi.Strip(a) != ansi.Strip(b) {
				t.Errorf("LAYOUT varies with the theme:\n%s\n---\n%s",
					ansi.Strip(a), ansi.Strip(b))
			}
		})
	}
}
