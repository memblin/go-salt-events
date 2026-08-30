package components

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// This file is the ONE implementation of the text discipline every pane needs:
// control-character sanitisation, display-cell truncation, and the wording for
// a ranked key that is empty.
//
// It is shared rather than copied by ruling. Four panes were written
// concurrently and three of them independently grew their own clip/fit/sanitise
// helper. The copies then diverged — one stopped sanitising, one stopped
// marking its truncation, one lost its input bound — and that divergence is the
// documented root cause of the wave review's Critical finding and four of its
// Important ones: two panes rendering the SAME ranked slice produced two
// different strings for the same key, one of which was silently wrong.
//
// A pane that needs different fitting parameterises these, it does not fork
// them.

// Ellipsis marks a clipped label. It is one cell wide and cannot be confused
// with a wildcard or with tag syntax.
const Ellipsis = "…"

// controlGlyph stands in for a control character.
const controlGlyph = '·'

// maxCellBytes bounds the work Clip does before measuring. A display cell can
// never be produced by more than four UTF-8 bytes of *width-bearing* text, so
// cutting the input to maxCellBytes×width bytes keeps a pathological
// 50,000-character category from costing a full-length scan on every one of the
// ten frames drawn each second (invariant 6). Without it a single hostile tag
// costs 4 ms a frame instead of 100 µs.
const maxCellBytes = 4

// Sanitise replaces control characters with a visible placeholder.
//
// Tags, minion IDs, function names and payload previews all arrive from the bus
// and are minion-supplied: a minion that can event.send can put anything it
// likes in them. A newline would split one row across two lines and
// desynchronise the frame — and, because lipgloss Height is a MINIMUM, grow the
// root's frame and push the status bar off the bottom of the terminal. A raw
// ESC would let event data drive the terminal of an operator running this as
// root on a production master: move the cursor, set the window title, repaint
// the screen. A tab is included because its width depends on the terminal's
// stops, so it cannot be measured and would break width-exactness.
func Sanitise(s string) string {
	if !strings.ContainsFunc(s, isControl) {
		return s
	}

	return strings.Map(func(r rune) rune {
		if isControl(r) {
			return controlGlyph
		}

		return r
	}, s)
}

// isControl reports whether r is a C0 control, DEL, or a C1 control.
func isControl(r rune) bool {
	const (
		space = 0x20
		del   = 0x7f
		c1Lo  = 0x80
		c1Hi  = 0x9f
	)

	return r < space || r == del || (r >= c1Lo && r <= c1Hi)
}

// Fit truncates s to at most w DISPLAY cells and leaves no marker.
//
// Display cells, not bytes or runes: these strings carry ANSI styling, and
// cutting one by index would slice an escape sequence in half — and would make
// the truncation point depend on the theme, which the layout guard forbids.
//
// Fit deliberately does NOT sanitise, because it is also the last width pass a
// pane runs over its own ALREADY-STYLED lines; replacing the ESC introducing an
// SGR sequence there would print the escape codes as literal garbage. Raw
// bus-derived text must go through Clip, or through Sanitise before reaching
// here.
func Fit(s string, w int) string {
	if w <= 0 {
		return ""
	}

	if lipgloss.Width(s) <= w {
		return s
	}

	return lipgloss.NewStyle().MaxWidth(w).Render(s)
}

// Clip fits bus-derived text into at most w display cells, marking any loss
// with Ellipsis.
//
// This is the only bound on a tag's or a category's length in the whole tool,
// by ruling: the stored value stays faithful to what the minion sent and is
// shortened only for display. Three things therefore happen here, in order —
// a BYTE bound so the work is O(w) rather than O(len(s)) (invariant 6);
// sanitisation, because this text is not routed through RenderTable and arrives
// from the bus; and the width fit itself, marked so a truncated tag cannot be
// mistaken for a real tag that genuinely ends there.
func Clip(s string, w int) string {
	if w <= 0 {
		return ""
	}

	cut := false

	if maxBytes := maxCellBytes * w; len(s) > maxBytes {
		// ToValidUTF8 removes the partial rune the byte cut may have left, and
		// any invalid sequence the bus supplied. Cutting by bytes can only ever
		// discard text that was already past the column.
		s = strings.ToValidUTF8(s[:maxBytes], "")
		cut = true
	}

	s = Sanitise(s)

	if !cut && lipgloss.Width(s) <= w {
		return s
	}

	if w == 1 {
		return Ellipsis
	}

	return Fit(s, w-1) + Ellipsis
}

// PadTo renders s into exactly w display cells, truncating or right-padding.
//
// The exact padding is load-bearing wherever a row can be highlighted: without
// it a selection stops at the end of the text rather than spanning the row,
// which reads as a rendering bug rather than a selection.
func PadTo(s string, w int) string {
	if w <= 0 {
		return ""
	}

	s = Fit(s, w)

	if gap := w - lipgloss.Width(s); gap > 0 {
		return s + strings.Repeat(" ", gap)
	}

	return s
}

// The two substitutes for a ranked key that is empty.
//
// They live here, next to the fitting, because the Rate and Summary panes
// render the SAME stats.Entry slices and an operator compares the two screens.
// Two spellings of one key would read as two different keys.
const (
	// PublishAck names the empty Category. Roughly a fifth of real traffic
	// (measured: 6 of 32 frames off a live Salt 3006.27 master) is the master's
	// bare-JID job publish-ack, which carries a deliberately empty Category
	// because a bare JID has no category segments. Rendering a blank label for
	// it would leave a bar and a percentage attached to nothing an operator
	// could act on.
	PublishAck = "(master job publish-ack)"

	// NoKey is the substitute for an empty minion or function. Neither should
	// occur — both are extracted from tag segments that exist or do not — but a
	// blank row is never an acceptable render.
	NoKey = "(none)"
)

// RankedLabel renders one ranked top-N key into exactly w display cells.
//
// empty is the wording for a key that is the empty string; it is passed in
// rather than hardcoded because the correct wording depends on which breakdown
// is being drawn — an empty Category is a specific, identifiable event class,
// an empty minion is merely absent.
//
// The substitution happens HERE, at render time, and nowhere else: the counter
// and the snapshot keep the empty string, because a placeholder stored upstream
// would be indistinguishable from a minion-sent tag containing the same text.
// An earlier attempt stored "*" and was rejected for exactly that collision.
func RankedLabel(key, empty string, w int) string {
	if key == "" {
		key = empty
	}

	return PadTo(Clip(key, w), w)
}
