package components

import (
	"strings"

	"github.com/TKC-Labs/go-salt-events/internal/theme"
)

// NoSelection means no row is highlighted. It is negative so it can never
// collide with a real row index.
const NoSelection = -1

// Column is one table column. Exactly one column should set Flex; it absorbs
// the leftover width. If several do, the last one wins.
type Column struct {
	Title string
	Width int
	Flex  bool
}

// RenderTable renders a header row plus rows, each padded to EXACTLY width
// display cells, and returns len(rows)+1 lines.
//
// The exact padding is load-bearing: without it a selection highlight stops at
// the end of the text rather than spanning the row, which looks like a
// rendering bug rather than a selection.
//
// Cost is O(len(rows)); callers pass only the rows currently on screen
// (invariant 6).
func RenderTable(cols []Column, rows [][]string, sel, width int, st *theme.Styles) []string {
	widths := layout(cols, width)

	out := make([]string, 0, len(rows)+1)

	head := make([]string, len(cols))
	for i, c := range cols {
		head[i] = pad(c.Title, widths[i])
	}

	out = append(out, st.TableHeader.Render(pad(strings.Join(head, " "), width)))

	for r, row := range rows {
		cells := make([]string, len(cols))

		for i := range cols {
			v := ""
			if i < len(row) {
				v = row[i]
			}

			cells[i] = pad(v, widths[i])
		}

		line := pad(strings.Join(cells, " "), width)

		if r == sel {
			out = append(out, st.TableRowSel.Render(line))

			continue
		}

		out = append(out, st.Value.Render(line))
	}

	return out
}

// layout distributes width across columns, giving slack to the flex column and
// shrinking widest-first when the fixed columns alone overflow.
func layout(cols []Column, width int) []int {
	widths := make([]int, len(cols))

	// Guard the shrink loop below, which indexes widths unconditionally: a
	// pane can legitimately be asked to render into no columns at all
	// (a table whose schema has not been chosen yet), and a negative width
	// arrives from a terminal resize before the first real size message.
	if len(cols) == 0 {
		return widths
	}

	fixed, flexIdx := 0, -1

	for i, c := range cols {
		widths[i] = max(0, c.Width)

		if c.Flex {
			flexIdx = i

			continue
		}

		fixed += widths[i]
	}

	avail := width - (len(cols) - 1)

	if flexIdx >= 0 {
		widths[flexIdx] = max(1, avail-fixed)

		return widths
	}

	shrink(widths, &fixed, avail)

	return widths
}

// shrink trims the widest column repeatedly until the fixed columns fit, or
// until every column is down to a single cell. Truncating is the lesser evil:
// dropping a column entirely would hide a field the operator is reading.
func shrink(widths []int, fixed *int, avail int) {
	for *fixed > avail {
		widest := 0
		for i := range widths {
			if widths[i] > widths[widest] {
				widest = i
			}
		}

		if widths[widest] <= 1 {
			return
		}

		widths[widest]--
		*fixed--
	}
}

// pad renders one cell into exactly w display cells.
//
// Cell text is bus-derived, so it is sanitised before it is measured (see
// Sanitise in text.go). The cut carries no Ellipsis marker on purpose: a table
// column boundary is itself the visual cue that a value continues, which a
// free-form ranked row does not have.
func pad(s string, w int) string {
	return PadTo(Sanitise(s), w)
}
