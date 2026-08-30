// Package components holds the shared render primitives: width-exact tables
// and the sparkline. Panes build from these rather than hand-rolling layout,
// so selection highlights span full rows and columns shrink predictably.
//
// Every function here takes a *theme.Styles and reads colour from nothing
// else. This package never builds a palette and never runs the theme
// package's compile step, by any route — not a struct literal, not a
// zero value, not a mutated copy of a registered one. A palette built outside
// the registry would never reach the contrast suite, which iterates
// theme.Names(), so an unreadable pane could ship with no failing test
// (spec §3.2). The forbidigo rule only bans the colour constructors
// textually; this comment is the reason the API-level routes are avoided too.
//
// Rendering is O(cells) in the width it is given and O(rows) in the rows it is
// handed. Nothing here walks an unbounded collection, because the UI renders
// from a snapshot at ~10Hz and must stay independent of event rate
// (spec §4.1, invariant 6).
package components

import (
	"math"
	"strings"

	"github.com/TKC-Labs/go-salt-events/internal/stats"
	"github.com/TKC-Labs/go-salt-events/internal/theme"
)

// blocks are the eight sparkline levels, ascending.
var blocks = [...]rune{'▁', '▂', '▃', '▄', '▅', '▆', '▇', '█'}

// gapGlyph marks a bucket during which we were not connected. It must be
// visually distinct from the lowest block: a disconnection that renders as a
// flat line at zero is indistinguishable from a quiet master, which is exactly
// backwards during an incident (spec §8.2).
const gapGlyph = '·'

// barFull and barEmpty are the top-N bar glyphs. Length carries magnitude;
// the empty remainder is drawn so rows share one baseline instead of ending
// ragged (spec §9).
const (
	barFull  = "█"
	barEmpty = "░"
)

// Sparkline renders buckets into exactly width display cells.
//
// fixedMax pins the vertical scale when non-zero. With autoscaling a 5/sec
// blip and a 5000/sec storm render identically, which is why the numeric
// callout beside a sparkline is mandatory and why the fixed-scale toggle
// exists (spec §9).
//
// Gap buckets render as gapGlyph in the Warn style rather than as a
// zero-height bar. Warn is legitimate here and is not a series colour:
// spec §9 reserves Ok/Warn/Err for status including *connection state*, and a
// gap is precisely connection state. In the mono theme Warn is empty, so the
// distinction still survives on the glyph alone.
func Sparkline(buckets []stats.Bucket, width int, fixedMax uint64, st *theme.Styles) string {
	if width <= 0 {
		return ""
	}

	sampled := resample(buckets, width)
	scale := autoscale(sampled, fixedMax)

	var sb strings.Builder

	// Contiguous runs of the same kind share one Render call, so a fully
	// connected window costs exactly one styling pass.
	for i := 0; i < len(sampled); {
		gap := sampled[i].Gap

		var run strings.Builder

		j := i
		for ; j < len(sampled) && sampled[j].Gap == gap; j++ {
			if gap {
				run.WriteRune(gapGlyph)

				continue
			}

			run.WriteRune(level(sampled[j].Count, scale))
		}

		if gap {
			sb.WriteString(st.Warn.Render(run.String()))
		} else {
			sb.WriteString(st.Spark.Render(run.String()))
		}

		i = j
	}

	return sb.String()
}

// autoscale returns fixedMax when it is set, otherwise the tallest live
// bucket. Gap buckets are excluded: a disconnection carries no count, so
// letting one influence the scale would be inventing data.
func autoscale(sampled []stats.Bucket, fixedMax uint64) uint64 {
	if fixedMax != 0 {
		return fixedMax
	}

	var scale uint64

	for _, b := range sampled {
		if !b.Gap && b.Count > scale {
			scale = b.Count
		}
	}

	return scale
}

// level maps a count onto a block glyph.
//
// The ratio is computed in float64 rather than as count*len(blocks)/scale:
// counts are uint64 straight off a counter, and the integer form overflows
// and wraps for large counts, which would render a storm as a flat line —
// silently, and exactly when the operator most needs to see it.
func level(count, scale uint64) rune {
	if scale == 0 || count == 0 {
		return blocks[0]
	}

	ratio := float64(count) / float64(scale)
	if ratio > 1 {
		ratio = 1
	}

	return blocks[int(ratio*float64(len(blocks)-1))]
}

// resample squeezes or stretches buckets to exactly n cells, taking the max of
// each group so a spike is never averaged away — a burst that vanishes because
// the window was wider than the terminal is the one thing this view must not
// do.
//
// A group is a gap only when every bucket in it is a gap: one live bucket
// inside a disconnection is real traffic and must still be drawn. With no
// buckets at all the whole line is a gap, not a row of zeros — "we have no
// data" is not "the master was quiet" (spec §8.2).
func resample(buckets []stats.Bucket, n int) []stats.Bucket {
	out := make([]stats.Bucket, n)

	if len(buckets) == 0 {
		for i := range out {
			out[i] = stats.Bucket{Gap: true}
		}

		return out
	}

	for i := range n {
		lo := i * len(buckets) / n

		hi := (i + 1) * len(buckets) / n
		if hi <= lo {
			hi = lo + 1
		}

		if hi > len(buckets) {
			hi = len(buckets)
		}

		out[i] = merge(buckets[lo:hi])
	}

	return out
}

// merge folds one group of buckets into the single cell that represents it.
func merge(group []stats.Bucket) stats.Bucket {
	agg := stats.Bucket{Gap: true}

	for _, b := range group {
		if b.Gap {
			continue
		}

		agg.Gap = false

		if b.Count > agg.Count {
			agg.Count = b.Count
		}
	}

	return agg
}

// Bar renders a horizontal magnitude bar in a single hue, exactly width cells
// wide.
//
// One hue on purpose: length carries magnitude and the row's text label
// carries identity, so this degrades perfectly to the mono theme and avoids
// colour-by-rank repainting rows as a ranking reshuffles (spec §9).
//
// pct is clamped rather than trusted. Callers derive it from counters that can
// be zero, so NaN and out-of-range values reach here as ordinary data.
func Bar(pct float64, width int, st *theme.Styles) string {
	if width <= 0 {
		return ""
	}

	filled := fill(pct, width)

	var sb strings.Builder

	if filled > 0 {
		sb.WriteString(st.Spark.Render(strings.Repeat(barFull, filled)))
	}

	if rest := width - filled; rest > 0 {
		sb.WriteString(st.Muted.Render(strings.Repeat(barEmpty, rest)))
	}

	return sb.String()
}

// fill converts a percentage into a cell count in [0, width].
//
// NaN is tested explicitly because it compares false against everything, so an
// ordering-based clamp alone would let it through into a conversion whose
// result is platform-defined.
func fill(pct float64, width int) int {
	switch {
	case math.IsNaN(pct), pct <= 0:
		return 0
	case pct >= 100:
		return width
	default:
		return int(pct / 100 * float64(width))
	}
}
