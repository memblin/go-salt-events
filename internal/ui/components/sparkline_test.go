package components_test

import (
	"math"
	"os"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/TKC-Labs/go-salt-events/internal/stats"
	"github.com/TKC-Labs/go-salt-events/internal/theme"
	"github.com/TKC-Labs/go-salt-events/internal/ui/components"
)

// TestMain pins the colour profile.
//
// Under `go test` stdout is not a terminal, so lipgloss detects the Ascii
// profile and renders every style as plain text — which makes correct,
// fully-themed components fail the theme guard. If a theme test ever fails,
// check this pin BEFORE touching the assertion.
func TestMain(m *testing.M) {
	lipgloss.SetColorProfile(termenv.TrueColor)
	os.Exit(m.Run())
}

// styles returns compiled styles for a REGISTERED palette.
//
// This is the ONLY place in the package that compiles a palette, and it is
// fed only from theme.Get, so the palette under test is one the contrast
// suite already validates by iterating theme.Names(). A hand-built palette —
// struct literal, zero value, or mutated copy — would be structurally
// invisible to that validation and must never appear here.
//
// The compile call cannot currently be avoided: theme exposes no
// name-to-Styles constructor, and a zero-value Styles renders every row
// identically, which would make TestTableSelectionIsVisible untestable.
// A theme.StylesFor(name) (*Styles, bool) helper would let this line go away
// and let the ban be tightened to cover tests as well.
//
// The components themselves never compile a palette and never name the
// Palette type: they receive *theme.Styles as a parameter and nothing more.
func styles(t *testing.T) *theme.Styles {
	t.Helper()

	p, ok := theme.Get("gruvbox-dark")
	if !ok {
		t.Fatal("gruvbox-dark palette missing")
	}

	return theme.Compile(p)
}

func TestSparklineRendersExactlyWidthCells(t *testing.T) {
	t.Parallel()

	buckets := make([]stats.Bucket, 120)
	for i := range buckets {
		buckets[i] = stats.Bucket{Count: uint64(i)}
	}

	got := components.Sparkline(buckets, 40, 0, styles(t))

	if w := lipgloss.Width(got); w != 40 {
		t.Errorf("width = %d, want 40", w)
	}
}

func TestSparklineDistinguishesGapFromZero(t *testing.T) {
	t.Parallel()

	// A disconnection rendering identically to a quiet master is exactly
	// backwards during an incident (spec §8.2).
	zeros := make([]stats.Bucket, 10)

	gaps := make([]stats.Bucket, 10)
	for i := range gaps {
		gaps[i] = stats.Bucket{Gap: true}
	}

	st := styles(t)

	zeroOut := components.Sparkline(zeros, 10, 0, st)
	gapOut := components.Sparkline(gaps, 10, 0, st)

	if zeroOut == gapOut {
		t.Error("a gap renders identically to zero; a master restart looks like calm")
	}
}

func TestSparklineFixedMaxPinsTheScale(t *testing.T) {
	t.Parallel()

	// With autoscaling, a 5/sec blip and a 5000/sec storm render identically.
	// The fixed-scale toggle is what lets two periods be compared (spec §9).
	small := []stats.Bucket{{Count: 1}, {Count: 5}}
	large := []stats.Bucket{{Count: 1000}, {Count: 5000}}

	st := styles(t)

	if components.Sparkline(small, 2, 0, st) != components.Sparkline(large, 2, 0, st) {
		t.Skip("autoscaled output already differs; nothing to prove here")
	}

	if components.Sparkline(small, 2, 5000, st) == components.Sparkline(large, 2, 5000, st) {
		t.Error("with a fixed max, a small series must not render like a large one")
	}
}

func TestSparklineHandlesDegenerateInput(t *testing.T) {
	t.Parallel()

	st := styles(t)

	for _, w := range []int{0, -1, 1} {
		_ = components.Sparkline(nil, w, 0, st)
	}

	_ = components.Sparkline([]stats.Bucket{{Count: 0}}, 10, 0, st)
}

func TestBarLengthCarriesMagnitude(t *testing.T) {
	t.Parallel()

	// Top-N is one hue; length is the encoding (spec §9). If length did not
	// vary, the pane would be conveying nothing.
	st := styles(t)

	short := components.Bar(10, 20, st)
	long := components.Bar(90, 20, st)

	if strings.Count(short, "█") >= strings.Count(long, "█") {
		t.Error("bar length does not increase with magnitude")
	}
}

// --- additional coverage beyond the brief ------------------------------------

// TestSparklineWidthIsExactAcrossShapes sweeps the resample boundary in both
// directions. Downsampling (many buckets into few cells) and upsampling (few
// buckets into many cells) are different code paths, and an off-by-one in
// either silently shifts every pane laid out beside the sparkline.
func TestSparklineWidthIsExactAcrossShapes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		buckets int
		width   int
	}{
		{"far more buckets than cells", 120, 7},
		{"slightly more buckets than cells", 41, 40},
		{"exactly as many buckets as cells", 40, 40},
		{"slightly fewer buckets than cells", 39, 40},
		{"far fewer buckets than cells", 3, 80},
		{"single bucket into many cells", 1, 33},
		{"many buckets into one cell", 120, 1},
		{"no buckets at all", 0, 20},
	}

	st := styles(t)

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			buckets := make([]stats.Bucket, tc.buckets)
			for i := range buckets {
				buckets[i] = stats.Bucket{Count: uint64(i % 9)}
			}

			got := components.Sparkline(buckets, tc.width, 0, st)

			if w := lipgloss.Width(got); w != tc.width {
				t.Errorf("width = %d, want %d", w, tc.width)
			}
		})
	}
}

// TestSparklineNonPositiveWidthRendersNothing guards the classic failure:
// slice indexing under a zero or negative width.
func TestSparklineNonPositiveWidthRendersNothing(t *testing.T) {
	t.Parallel()

	st := styles(t)
	buckets := []stats.Bucket{{Count: 1}, {Gap: true}, {Count: 9}}

	for _, w := range []int{0, -1, -100} {
		if got := components.Sparkline(buckets, w, 0, st); got != "" {
			t.Errorf("Sparkline(width=%d) = %q, want empty", w, got)
		}
	}
}

// TestSparklineGapIsNotDrawnInTheSeriesHue locks in the second half of the
// gap signal. The glyph alone already distinguishes a gap (and is the whole
// encoding under the mono theme), so a mutation that drops the Warn styling
// survives every other test here. Spec §9 reserves Warn for status
// *including connection state*, which is exactly what a gap is.
func TestSparklineGapIsNotDrawnInTheSeriesHue(t *testing.T) {
	t.Parallel()

	st := styles(t)

	gaps := []stats.Bucket{{Gap: true}}

	if got := components.Sparkline(gaps, 1, 0, st); got == st.Spark.Render("·") {
		t.Error("a gap is drawn in the series hue; it is connection status, not a data point")
	}
}

// TestSparklineKeepsAGapSurroundedByData is the mutation guard for the
// gap-vs-zero distinction. Testing all-gap against all-zero (above) still
// passes if gaps are only special-cased when the whole window is a gap; a gap
// in the MIDDLE of live traffic is the case that actually occurs during a
// master restart.
func TestSparklineKeepsAGapSurroundedByData(t *testing.T) {
	t.Parallel()

	st := styles(t)

	withGap := []stats.Bucket{{Count: 5}, {Gap: true}, {Count: 5}}
	withZero := []stats.Bucket{{Count: 5}, {Count: 0}, {Count: 5}}

	if components.Sparkline(withGap, 3, 0, st) == components.Sparkline(withZero, 3, 0, st) {
		t.Error("a gap between two live buckets renders as a zero-height bar")
	}
}

// TestSparklineGapDoesNotSuppressNeighbouringData checks a gap cell does not
// swallow the buckets either side of it when the series is downsampled.
func TestSparklineGapDoesNotSuppressNeighbouringData(t *testing.T) {
	t.Parallel()

	st := styles(t)

	allGap := make([]stats.Bucket, 10)
	for i := range allGap {
		allGap[i] = stats.Bucket{Gap: true}
	}

	oneLive := make([]stats.Bucket, 10)
	copy(oneLive, allGap)
	oneLive[4] = stats.Bucket{Count: 100}

	if components.Sparkline(allGap, 5, 0, st) == components.Sparkline(oneLive, 5, 0, st) {
		t.Error("a live bucket inside a gap window vanished")
	}
}

// TestSparklinePreservesSpikesWhenDownsampled is why resampling takes the max
// of each group rather than the mean: a burst that disappears because the
// window was wider than the terminal is the one thing this view must not do.
func TestSparklinePreservesSpikesWhenDownsampled(t *testing.T) {
	t.Parallel()

	st := styles(t)

	flat := make([]stats.Bucket, 120)
	for i := range flat {
		flat[i] = stats.Bucket{Count: 1}
	}

	spike := make([]stats.Bucket, 120)
	copy(spike, flat)
	spike[77] = stats.Bucket{Count: 5000}

	if components.Sparkline(flat, 20, 5000, st) == components.Sparkline(spike, 20, 5000, st) {
		t.Error("a single-bucket spike was averaged away by downsampling")
	}
}

// TestSparklineHandlesUniformAndExtremeCounts covers all-equal, all-zero and
// enormous values — arithmetic that overflows or divides by zero here would
// take down the whole TUI on bus data.
func TestSparklineHandlesUniformAndExtremeCounts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		count    uint64
		fixedMax uint64
	}{
		{"all zero autoscaled", 0, 0},
		{"all zero fixed", 0, 100},
		{"all equal autoscaled", 42, 0},
		{"all equal fixed at same value", 42, 42},
		{"count far above fixed max", 1_000_000, 1},
		{"max uint64 autoscaled", math.MaxUint64, 0},
		{"max uint64 with max fixed", math.MaxUint64, math.MaxUint64},
		{"max uint64 with tiny fixed", math.MaxUint64, 1},
	}

	st := styles(t)

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			buckets := make([]stats.Bucket, 12)
			for i := range buckets {
				buckets[i] = stats.Bucket{Count: tc.count}
			}

			got := components.Sparkline(buckets, 12, tc.fixedMax, st)

			if w := lipgloss.Width(got); w != 12 {
				t.Errorf("width = %d, want 12", w)
			}
		})
	}
}

// TestSparklineDoesNotWrapOnHugeCounts is the guard for the scaling
// arithmetic. Computing the level as count*len(blocks)/scale overflows uint64
// for large counts and wraps to the LOWEST block, so a 10^19 event/sec
// counter would render as a flat calm line — silently, and exactly when the
// operator most needs to see it.
func TestSparklineDoesNotWrapOnHugeCounts(t *testing.T) {
	t.Parallel()

	st := styles(t)

	counts := []uint64{
		1,
		1 << 32,
		math.MaxUint64 / 8,
		math.MaxUint64 / 7,
		math.MaxUint64 - 1,
		math.MaxUint64,
	}

	for _, c := range counts {
		// A bucket at exactly the ceiling is a full-height bar, whatever
		// the magnitude.
		got := components.Sparkline([]stats.Bucket{{Count: c}}, 1, c, st)

		if !strings.ContainsRune(got, '█') {
			t.Errorf("count=%d at fixedMax=%d did not render a full block", c, c)
		}

		// The same holds when the scale is derived rather than pinned.
		if got := components.Sparkline([]stats.Bucket{{Count: c}}, 1, 0, st); !strings.ContainsRune(got, '█') {
			t.Errorf("count=%d autoscaled did not render a full block", c)
		}
	}
}

// TestSparklineEmptyWindowIsAGapNotCalm: with no buckets at all we have no
// information. Drawing a flat zero line would assert the master was quiet,
// which is the §8.2 error in its purest form.
func TestSparklineEmptyWindowIsAGapNotCalm(t *testing.T) {
	t.Parallel()

	st := styles(t)

	empty := components.Sparkline(nil, 8, 0, st)
	zeros := components.Sparkline(make([]stats.Bucket, 8), 8, 0, st)

	if empty == zeros {
		t.Error("an empty window renders as a quiet master")
	}

	if !strings.ContainsRune(empty, '·') {
		t.Errorf("empty window = %q, want gap glyphs", empty)
	}
}

// TestBarIsExactlyWidthCells keeps top-N rows aligned. A bar that renders
// short leaves ragged right edges down the pane.
func TestBarIsExactlyWidthCells(t *testing.T) {
	t.Parallel()

	st := styles(t)

	for _, pct := range []float64{0, 0.4, 1, 50, 99.6, 100} {
		for _, w := range []int{1, 2, 7, 40} {
			if got := lipgloss.Width(components.Bar(pct, w, st)); got != w {
				t.Errorf("Bar(%v, %d) width = %d, want %d", pct, w, got, w)
			}
		}
	}
}

// TestBarClampsHostileInput: percentages are computed by callers from
// counters that can be zero, so NaN and out-of-range values reach here.
func TestBarClampsHostileInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		pct   float64
		width int
	}{
		{"negative percent", -50, 10},
		{"far over one hundred", 100000, 10},
		{"not a number", math.NaN(), 10},
		{"positive infinity", math.Inf(1), 10},
		{"negative infinity", math.Inf(-1), 10},
		{"zero width", 50, 0},
		{"negative width", 50, -3},
	}

	st := styles(t)

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := components.Bar(tc.pct, tc.width, st)

			want := tc.width
			if want < 0 {
				want = 0
			}

			if w := lipgloss.Width(got); w != want {
				t.Errorf("width = %d, want %d", w, want)
			}
		})
	}
}

// TestBarDegradesToLengthAlone: spec §9 forbids a categorical palette, so a
// top-N row must still read correctly with every escape sequence removed.
// Counting glyphs rather than comparing rendered strings is deliberate — ANSI
// codes never contain a block glyph, so this measures the mono encoding
// without mutating the global colour profile other parallel tests depend on.
func TestBarDegradesToLengthAlone(t *testing.T) {
	t.Parallel()

	st := styles(t)

	prev := -1

	for _, pct := range []float64{0, 25, 50, 75, 100} {
		got := strings.Count(components.Bar(pct, 40, st), "█")
		if got <= prev {
			t.Errorf("Bar(%v) filled %d cells, want more than %d", pct, got, prev)
		}

		prev = got
	}
}
