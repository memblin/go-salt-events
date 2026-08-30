package rate_test

import (
	"os"
	"regexp"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"

	"github.com/TKC-Labs/go-salt-events/internal/stats"
	"github.com/TKC-Labs/go-salt-events/internal/theme"
	"github.com/TKC-Labs/go-salt-events/internal/ui"
	"github.com/TKC-Labs/go-salt-events/internal/ui/rate"
)

func TestMain(m *testing.M) {
	lipgloss.SetColorProfile(termenv.TrueColor)
	os.Exit(m.Run())
}

func styles(t *testing.T) *theme.Styles {
	t.Helper()

	return stylesNamed(t, "gruvbox-dark")
}

func stylesNamed(t *testing.T, name string) *theme.Styles {
	t.Helper()

	// theme.StylesFor, not theme.Get + theme.Compile: compile is unexported
	// and its test-only Compile shim lives in the theme package's own test
	// binary, so it is not reachable from here. StylesFor sources its palette
	// from the registry, which is the only route a palette reaches the
	// contrast suite (spec §3.2). Calling it from a _test.go file is outside
	// TestOnlyTheRootObtainsStyles' scan, which skips test files.
	st, ok := theme.StylesFor(name)
	if !ok {
		t.Fatalf("theme %q is not registered", name)
	}

	return st
}

// sample is a busy, fully connected window.
//
// HasData is true and NowIsGap false because that is what stats.summarise
// returns for a window with live buckets in it. A Summary carrying counts but
// HasData=false cannot come out of the stats package, and building one here
// would make the callout tests assert against a state that never occurs.
func sample() ui.Snapshot {
	secs := make([]stats.Bucket, stats.SecondBuckets)
	for i := range secs {
		secs[i] = stats.Bucket{Count: uint64(i % 50)}
	}

	mins := make([]stats.Bucket, stats.MinuteBuckets)
	for i := range mins {
		mins[i] = stats.Bucket{Count: uint64(i * 100)}
	}

	return ui.Snapshot{
		Seconds: secs,
		Minutes: mins,
		SecSum:  stats.Summary{Now: 42, Peak: 311, Mean: 58, HasData: true},
		MinSum:  stats.Summary{Now: 2500, Peak: 9100, Mean: 3000, HasData: true},
		TopCategories: []stats.Entry{
			{Key: "salt/job/*/ret/*", Count: 610, Pct: 61},
			{Key: "salt/minion/*", Count: 220, Pct: 22},
		},
		TopMinions: []stats.Entry{
			{Key: "scache-1", Count: 380, Pct: 38},
		},
	}
}

// buckets builds a window of n buckets, all gaps or all live zeros.
func buckets(n int, gap bool) []stats.Bucket {
	out := make([]stats.Bucket, n)
	for i := range out {
		out[i] = stats.Bucket{Gap: gap}
	}

	return out
}

// quiet is a connected master that saw no events: real zeros.
func quiet() ui.Snapshot {
	return ui.Snapshot{
		Seconds: buckets(stats.SecondBuckets, false),
		Minutes: buckets(stats.MinuteBuckets, false),
		SecSum:  stats.Summary{HasData: true},
		MinSum:  stats.Summary{HasData: true},
	}
}

// blind is a window in which we were not connected at all: no data.
func blind() ui.Snapshot {
	return ui.Snapshot{
		Seconds: buckets(stats.SecondBuckets, true),
		Minutes: buckets(stats.MinuteBuckets, true),
		SecSum:  stats.Summary{NowIsGap: true},
		MinSum:  stats.Summary{NowIsGap: true},
	}
}

func TestRateAlwaysRendersTheNumericCallouts(t *testing.T) {
	t.Parallel()

	// MANDATORY, not decorative. An autoscaled sparkline renders a 5/sec blip
	// and a 5000/sec storm identically, so now/peak/mean are the only things
	// carrying scale (spec §9). If these ever become conditional, the graph
	// silently stops meaning anything.
	got := rate.New().View(100, 24, sample(), styles(t))

	for _, want := range []string{"42", "311", "58", "2.5k", "9.1k"} {
		if !strings.Contains(got, want) {
			t.Errorf("callout %q missing from:\n%s", want, got)
		}
	}
}

func TestRateRendersTwoSeparateSparklines(t *testing.T) {
	t.Parallel()

	// Never a dual-axis chart. Events/sec and events/min have different
	// scales; sharing one plot would invent a relationship that is not in the
	// data (spec §9).
	got := rate.New().View(100, 24, sample(), styles(t))

	if !strings.Contains(got, "Events/sec") || !strings.Contains(got, "Events/min") {
		t.Errorf("both series must be separately labelled:\n%s", got)
	}
}

func TestRateFixedScaleToggle(t *testing.T) {
	t.Parallel()

	p := rate.New()
	st := styles(t)
	s := sample()

	auto := p.View(100, 24, s, st)

	next, _ := p.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'F'}}, s)

	fixed := next.View(100, 24, s, st)

	if auto == fixed {
		t.Error("the fixed-scale toggle changed nothing")
	}

	// The toggle must move the GRAPH, not just print a note: pinning the
	// y-axis to the window peak is the whole point of comparing two periods.
	if sparkOf(t, auto) == sparkOf(t, fixed) {
		t.Error("the fixed-scale toggle left the sparkline unchanged")
	}

	back, _ := next.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'F'}}, s)
	if back.View(100, 24, s, st) != auto {
		t.Error("toggling twice did not return to autoscale")
	}
}

// sparkOf returns the events/sec sparkline row, ANSI stripped.
func sparkOf(t *testing.T, view string) string {
	t.Helper()

	lines := strings.Split(ansi.Strip(view), "\n")
	if len(lines) < 2 {
		t.Fatalf("no sparkline row in:\n%s", view)
	}

	return lines[1]
}

func TestRateTopNRendersLabelsNotColourAlone(t *testing.T) {
	t.Parallel()

	// Identity is carried by the text label and magnitude by bar length, one
	// hue throughout — which is what makes this readable in the mono theme
	// (spec §9).
	got := rate.New().View(100, 24, sample(), styles(t))

	for _, want := range []string{"salt/job/*/ret/*", "scache-1"} {
		if !strings.Contains(got, want) {
			t.Errorf("label %q missing:\n%s", want, got)
		}
	}
}

func TestRateFitsItsBox(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		w, h int
		snap ui.Snapshot
	}{
		"wide":            {100, 24, sample()},
		"narrow":          {60, 12, sample()},
		"narrow no data":  {60, 12, blind()},
		"one column":      {1, 1, sample()},
		"three rows":      {40, 3, sample()},
		"empty snapshot":  {80, 20, ui.Snapshot{}},
		"blind wide":      {100, 24, blind()},
		"quiet very thin": {12, 8, quiet()},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got := rate.New().View(tc.w, tc.h, tc.snap, styles(t))

			lines := strings.Split(got, "\n")
			if len(lines) > tc.h {
				t.Errorf("rendered %d lines into a %d-line box", len(lines), tc.h)
			}

			for i, l := range lines {
				if w := lipgloss.Width(l); w > tc.w {
					t.Errorf("line %d width = %d, exceeds %d", i, w, tc.w)
				}
			}
		})
	}
}

// TestRateSanitisesBusSuppliedKeys asserts against the RAW output, never
// against ansi.Strip(got).
//
// Stripping first is the mistake that made the sibling Jobs test toothless:
// ansi.Strip removes well-formed CSI sequences, so an assertion applied to a
// stripped view is checking that a MALFORMED escape survived — precisely the
// case an attacker would not send. The top-N keys here are stats.Entry keys
// from Snapshot.TopCategories / TopMinions, which is to say a tag or minion ID
// a minion can set to anything at all via event.send, and this tool is expected
// to run as root on a production master.
func TestRateSanitisesBusSuppliedKeys(t *testing.T) {
	t.Parallel()

	s := ui.Snapshot{
		TopCategories: []stats.Entry{
			{Key: "salt/\x1b]0;pwned\x07job", Count: 10, Pct: 50},
			{Key: "tab\there", Count: 5, Pct: 25},
		},
		TopMinions: []stats.Entry{{Key: "web\x1b[2Jok", Count: 3, Pct: 15}},
	}

	got := rate.New().View(100, 30, s, styles(t))

	// "\x1b[2J" cannot be confused with this pane's own styling: SGR sequences
	// terminate in "m", never in "J".
	for _, bad := range []string{"\x1b]0;", "\x1b[2J", "\x07", "\t"} {
		if strings.Contains(got, bad) {
			t.Errorf("control sequence %q reached the terminal:\n%q", bad, got)
		}
	}
}

// TestRateHeightClampSurvivesANewlineInATag is the second half of the same
// defect and is a frame bug rather than a security one.
//
// View clamps its line COUNT to h and then fits each line to w, but a key
// containing newlines is a single element of that slice which renders as
// several terminal rows — lipgloss MaxWidth applies per line rather than
// collapsing them. Since the root's frame is Height(contentH) and lipgloss
// Height is a MINIMUM, the overflow grows the frame and pushes the filter bar,
// hint line and status bar off the bottom of the terminal.
func TestRateHeightClampSurvivesANewlineInATag(t *testing.T) {
	t.Parallel()

	s := ui.Snapshot{
		TopMinions: []stats.Entry{{Key: "a\nb\nc\nd\ne\nf\ng\nh", Count: 1, Pct: 100}},
	}

	st := styles(t)

	for _, h := range []int{3, 6, 10, 24} {
		got := rate.New().View(100, h, s, st)

		if lines := strings.Count(got, "\n") + 1; lines > h {
			t.Errorf("h=%d: emitted %d lines, which grows the root's frame:\n%q", h, lines, got)
		}
	}
}

// TestRateNamesTheEmptyKeyTheSameWayTheSummaryPaneDoes.
//
// Rate and Summary render the SAME Snapshot.TopCategories / TopMinions slices,
// and an operator compares the two screens. An empty Category is the master's
// bare-JID publish-ack and is roughly a fifth of real traffic, so a blank label
// here is not an edge case — it is the single most frequent class in the
// breakdown, rendered as a bar and a percentage attached to nothing.
func TestRateNamesTheEmptyKeyTheSameWayTheSummaryPaneDoes(t *testing.T) {
	t.Parallel()

	s := ui.Snapshot{
		TopCategories: []stats.Entry{{Key: "", Count: 19, Pct: 19}},
		TopMinions:    []stats.Entry{{Key: "", Count: 3, Pct: 3}},
	}

	got := ansi.Strip(rate.New().View(100, 24, s, styles(t)))

	for _, want := range []string{"(master job publish-ack)", "(none)"} {
		if !strings.Contains(got, want) {
			t.Errorf("want %q in:\n%s", want, got)
		}
	}

	if strings.Contains(got, "*") {
		t.Errorf("an empty key must not be shown as a wildcard:\n%s", got)
	}

	// The general form of the rule: whatever produced the empty key, a bar and
	// a percentage attached to nothing carry no identity at all (spec §9).
	for _, line := range strings.Split(got, "\n") {
		i := strings.IndexAny(line, "█░")
		if i < 0 {
			continue
		}

		if strings.TrimSpace(line[:i]) == "" {
			t.Errorf("ranked row has a blank label: %q", line)
		}
	}
}

// TestRateMarksATruncatedTag: a tag cut with no marker reads as a real tag that
// genuinely ends there. Summary marks the identical key from the identical
// slice with "…", so an unmarked cut here is also two panes disagreeing about
// one string.
func TestRateMarksATruncatedTag(t *testing.T) {
	t.Parallel()

	const long = "salt/job/20260830081402123456/ret/web-server-01.datacentre1.example.com"

	s := ui.Snapshot{TopCategories: []stats.Entry{{Key: long, Count: 1, Pct: 100}}}

	got := ansi.Strip(rate.New().View(100, 24, s, styles(t)))

	if strings.Contains(got, long) {
		t.Fatalf("the tag was not truncated at all, so there is nothing to mark:\n%s", got)
	}

	if !strings.Contains(got, "…") {
		t.Errorf("a truncated tag carries no visible marker:\n%s", got)
	}
}

// TestRateNeverPanics covers the two states the contract calls out: a content
// box as small as 1x1 during a resize, and a Snapshot whose slices are all nil
// before the first tick.
func TestRateNeverPanics(t *testing.T) {
	t.Parallel()

	sizes := [][2]int{{1, 1}, {0, 0}, {-3, -3}, {2, 40}, {200, 1}}
	snaps := map[string]ui.Snapshot{"nil": {}, "sample": sample(), "blind": blind()}

	for name, s := range snaps {
		for _, sz := range sizes {
			t.Run(name, func(t *testing.T) {
				t.Parallel()

				p := rate.New()
				p.SetStyles(styles(t))
				_ = p.View(sz[0], sz[1], s, styles(t))
			})
		}
	}
}

// TestGapRendersDifferentlyFromZero is the point of this pane.
//
// A gap bucket always carries Count == 0, so reading Count naively prints
// "0 events/sec" directly beneath a sparkline correctly drawing a break —
// telling the operator "nothing happened" when the truth is "nobody was
// recording" (spec §8.2). stats.Summary carries NowIsGap and HasData for
// exactly this, and this test asserts the two states are DIFFERENT, not merely
// that each renders.
func TestGapRendersDifferentlyFromZero(t *testing.T) {
	t.Parallel()

	st := styles(t)

	q := ansi.Strip(rate.New().View(100, 24, quiet(), st))
	g := ansi.Strip(rate.New().View(100, 24, blind(), st))

	if q == g {
		t.Fatalf("a total outage renders identically to a quiet master:\n%s", q)
	}

	if !strings.Contains(q, "now 0") {
		t.Errorf("a quiet master must report a real zero rate:\n%s", q)
	}

	if strings.Contains(g, "now 0") {
		t.Errorf("an outage reported a zero rate; nobody was recording:\n%s", g)
	}

	for _, want := range []string{"no data"} {
		if !strings.Contains(g, want) {
			t.Errorf("an outage must say %q:\n%s", want, g)
		}
	}

	// The sparkline must draw the break too: the callout and the graph have to
	// agree, or one of them is lying.
	if !strings.Contains(sparkOf(t, ansi.Strip(rate.New().View(100, 24, blind(), st))), "·") {
		t.Errorf("the outage sparkline drew no break:\n%s", g)
	}
}

// TestPartialGapKeepsPeakAndMean checks the half-gapped case: the newest
// bucket is a gap, so `now` is unknown, but the window still has live buckets,
// so peak and mean remain real measurements and must still print.
func TestPartialGapKeepsPeakAndMean(t *testing.T) {
	t.Parallel()

	s := sample()
	s.SecSum = stats.Summary{Peak: 311, Mean: 58, NowIsGap: true, HasData: true}

	got := ansi.Strip(rate.New().View(100, 24, s, styles(t)))

	if strings.Contains(got, "now 0") {
		t.Errorf("a gapped newest bucket printed as a zero rate:\n%s", got)
	}

	for _, want := range []string{"now no data", "peak 311", "mean 58"} {
		if !strings.Contains(got, want) {
			t.Errorf("want %q in:\n%s", want, got)
		}
	}
}

// TestFixedScaleIgnoresAGappedWindow: with no live bucket at all, Peak is not
// a measurement, so pinning to it would pin the axis to a fabricated zero.
func TestFixedScaleIgnoresAGappedWindow(t *testing.T) {
	t.Parallel()

	p := rate.New()

	next, _ := p.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'F'}}, blind())

	got := ansi.Strip(next.View(100, 24, blind(), styles(t)))

	if strings.Contains(got, "peak 0") || strings.Contains(got, "mean 0") {
		t.Errorf("a fully gapped window reported fabricated aggregates:\n%s", got)
	}
}

// TestKeysAdvertisesTheFixedScaleToggle: the root renders whatever comes back,
// so a binding missing from Keys ships undiscoverable (spec §9, pane.go).
func TestKeysAdvertisesTheFixedScaleToggle(t *testing.T) {
	t.Parallel()

	p := rate.New()

	find := func(hints []ui.KeyHint) bool {
		for _, h := range hints {
			if h.Key == "F" && h.Label != "" {
				return true
			}
		}

		return false
	}

	if !find(p.Keys()) {
		t.Errorf("the F toggle is not advertised: %+v", p.Keys())
	}

	// Captured BEFORE the toggle: Update may legitimately mutate the receiver
	// and return it, so p and next can be the same pane.
	before := labelOf(p.Keys())

	next, _ := p.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'F'}}, sample())

	if !find(next.Keys()) {
		t.Errorf("the F toggle vanished once pressed: %+v", next.Keys())
	}

	// Keys is called every frame so a stateful binding can report what is true
	// NOW; the label must therefore track the state.
	if before == labelOf(next.Keys()) {
		t.Error("the toggle's hint does not say what pressing it will now do")
	}
}

func labelOf(hints []ui.KeyHint) string {
	for _, h := range hints {
		if h.Key == "F" {
			return h.Label
		}
	}

	return ""
}

func TestTitleIsStable(t *testing.T) {
	t.Parallel()

	if got := rate.New().Title(); got != "Rate" {
		t.Errorf("Title() = %q, want %q", got, "Rate")
	}
}

// sgrTail matches one SGR escape sequence anchored at the end of a string.
var sgrTail = regexp.MustCompile(`\x1b\[[0-9;:]*m$`)

// escapesBefore returns the contiguous run of ANSI escape sequences
// immediately preceding the first occurrence of token.
//
// This is what gives the theme test teeth. Asserting only that two rendered
// frames differ is nearly vacuous here: this pane embeds components.Sparkline
// and components.Bar, which are styled by the components package, so the frames
// would still differ across themes with every style call in THIS package
// removed — the same weakness Task 15 found at the root. Pinning the escape
// run that sits directly against a specific token asserts that THIS pane
// styled THAT text: strip the style and the run collapses to the previous
// segment's theme-independent reset (or to nothing at the start of the frame),
// making it identical under both themes and failing the test.
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
func TestViewIsStyledByTheThemeItIsGivenAndLaidOutIdentically(t *testing.T) {
	t.Parallel()

	s := sample()
	gruv := rate.New().View(100, 24, s, stylesNamed(t, "gruvbox-dark"))
	sol := rate.New().View(100, 24, s, stylesNamed(t, "solarized-dark"))
	mono := rate.New().View(100, 24, s, stylesNamed(t, "mono"))

	if ansi.Strip(gruv) != ansi.Strip(sol) || ansi.Strip(gruv) != ansi.Strip(mono) {
		t.Error("the theme changed LAYOUT; ANSI-stripped output must be byte-identical")
	}

	if gruv == sol {
		t.Fatal("the two themes rendered identically; no style reached the output")
	}

	// One token per style this pane applies itself. Each must be wrapped by
	// THIS pane, not merely sit next to something the components package
	// styled.
	styled := []struct {
		style string
		token string
	}{
		{"Header", "Events/sec"},
		{"KeyLabel", "now "},
		{"Value", "42"},
		{"Muted", "  61%"},
		{"Header (top-N title)", "Top tags"},
		{"Value (top-N label)", "salt/job/*/ret/*"},
	}

	for _, c := range styled {
		if !strings.Contains(ansi.Strip(gruv), strings.TrimSpace(c.token)) {
			t.Fatalf("%s: token %q is not in the output at all", c.style, c.token)
		}

		a, b := escapesBefore(gruv, c.token), escapesBefore(sol, c.token)
		if a == b {
			t.Errorf("%s: %q carries no theme-dependent styling (both %q)", c.style, c.token, a)
		}
	}
}

// TestWarnMarksTheOutage: gap-vs-zero must survive as more than text — the
// callout's "no data" is styled as connection state, which spec §9 explicitly
// permits Warn to carry.
func TestWarnMarksTheOutage(t *testing.T) {
	t.Parallel()

	got := rate.New().View(100, 24, blind(), stylesNamed(t, "gruvbox-dark"))

	if escapesBefore(got, "no data") == "" {
		t.Errorf("the outage callout is unstyled:\n%q", got)
	}
}
