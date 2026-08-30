package components_test

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"

	"github.com/TKC-Labs/go-salt-events/internal/ui/components"
)

func TestTableRowsArePaddedToExactlyTheGivenWidth(t *testing.T) {
	t.Parallel()

	// Without exact padding a selection highlight stops at the end of the
	// text instead of spanning the row, which reads as a rendering bug.
	cols := []components.Column{
		{Title: "A", Width: 5},
		{Title: "B", Flex: true},
	}

	rows := [][]string{{"x", "y"}, {"longer", "value"}}

	got := components.RenderTable(cols, rows, 0, 40, styles(t))

	for i, line := range got {
		if w := lipgloss.Width(line); w != 40 {
			t.Errorf("line %d width = %d, want 40", i, w)
		}
	}
}

func TestTableSelectionIsVisible(t *testing.T) {
	t.Parallel()

	// Mutation lesson from go-valkey-tui: replacing the cursor index with
	// NoSelection passed the entire suite — every row still rendered, paging
	// still worked, you simply could not see what was selected.
	cols := []components.Column{{Title: "A", Flex: true}}
	rows := [][]string{{"one"}, {"two"}}

	st := styles(t)

	withSel := components.RenderTable(cols, rows, 1, 20, st)
	without := components.RenderTable(cols, rows, components.NoSelection, 20, st)

	if withSel[2] == without[2] {
		t.Error("the selected row renders identically to an unselected one")
	}
}

func TestTableTruncatesRatherThanOverflowing(t *testing.T) {
	t.Parallel()

	cols := []components.Column{{Title: "A", Width: 30}, {Title: "B", Width: 30}}
	rows := [][]string{{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}}

	got := components.RenderTable(cols, rows, components.NoSelection, 20, styles(t))

	for i, line := range got {
		if w := lipgloss.Width(line); w != 20 {
			t.Errorf("line %d width = %d, want 20 — over-wide columns must shrink", i, w)
		}
	}
}

// --- additional coverage beyond the brief ------------------------------------

// TestTableSelectionMovesWithTheIndex guards the other half of the mutation
// above: a highlight that is visible but always lands on the same row is just
// as broken as no highlight at all.
func TestTableSelectionMovesWithTheIndex(t *testing.T) {
	t.Parallel()

	cols := []components.Column{{Title: "A", Flex: true}}
	rows := [][]string{{"one"}, {"two"}, {"three"}}

	st := styles(t)

	first := components.RenderTable(cols, rows, 0, 20, st)
	second := components.RenderTable(cols, rows, 1, 20, st)

	if first[1] == second[1] {
		t.Error("row 0 renders the same whether or not it is selected")
	}

	if first[2] == second[2] {
		t.Error("row 1 renders the same whether or not it is selected")
	}
}

// TestTableOutOfRangeSelectionHighlightsNothing: the cursor is clamped by the
// pane, but an off-by-one there must not panic or highlight a phantom row.
func TestTableOutOfRangeSelectionHighlightsNothing(t *testing.T) {
	t.Parallel()

	cols := []components.Column{{Title: "A", Flex: true}}
	rows := [][]string{{"one"}, {"two"}}

	st := styles(t)

	none := components.RenderTable(cols, rows, components.NoSelection, 20, st)

	for _, sel := range []int{-7, 2, 99} {
		got := components.RenderTable(cols, rows, sel, 20, st)

		if len(got) != len(none) {
			t.Fatalf("sel=%d produced %d lines, want %d", sel, len(got), len(none))
		}

		for i := range got {
			if got[i] != none[i] {
				t.Errorf("sel=%d changed line %d; an out-of-range cursor must highlight nothing", sel, i)
			}
		}
	}
}

// TestTableAlwaysRendersAHeaderAndEveryRow: a pane that silently drops rows
// under a narrow terminal hides events, which is the opposite of the job.
func TestTableAlwaysRendersAHeaderAndEveryRow(t *testing.T) {
	t.Parallel()

	cols := []components.Column{{Title: "A", Width: 4}, {Title: "B", Flex: true}}
	rows := [][]string{{"1", "a"}, {"2", "b"}, {"3", "c"}}

	for _, w := range []int{0, 1, 3, 10, 200} {
		got := components.RenderTable(cols, rows, components.NoSelection, w, styles(t))

		if len(got) != len(rows)+1 {
			t.Errorf("width %d: got %d lines, want %d (header + %d rows)",
				w, len(got), len(rows)+1, len(rows))
		}
	}
}

// TestTableHandlesDegenerateInput is the never-panic guard. Widths, heights
// and slices all come from callers; ragged rows come from the bus.
func TestTableHandlesDegenerateInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		cols  []components.Column
		rows  [][]string
		sel   int
		width int
	}{
		{"nil everything", nil, nil, components.NoSelection, 40},
		{"no columns but rows present", nil, [][]string{{"x"}}, 0, 40},
		{"no rows", []components.Column{{Title: "A", Flex: true}}, nil, 0, 40},
		{"zero width", []components.Column{{Title: "A", Width: 3}}, [][]string{{"x"}}, 0, 0},
		{"negative width", []components.Column{{Title: "A", Width: 3}}, [][]string{{"x"}}, 0, -5},
		{"negative width, no columns", nil, [][]string{{"x"}}, 0, -5},
		{"negative width, flex column", []components.Column{{Title: "A", Flex: true}}, [][]string{{"x"}}, 0, -5},
		{"row shorter than columns", []components.Column{{Title: "A", Width: 3}, {Title: "B", Width: 3}}, [][]string{{"x"}}, 0, 20},
		{"row longer than columns", []components.Column{{Title: "A", Width: 3}}, [][]string{{"x", "y", "z"}}, 0, 20},
		{"empty row", []components.Column{{Title: "A", Width: 3}}, [][]string{{}}, 0, 20},
		{"zero-width column", []components.Column{{Title: "A"}, {Title: "B", Width: 5}}, [][]string{{"x", "y"}}, 0, 20},
		{"negative column width", []components.Column{{Title: "A", Width: -9}, {Title: "B", Width: 5}}, [][]string{{"x", "y"}}, 0, 20},
		{"every column flex", []components.Column{{Title: "A", Flex: true}, {Title: "B", Flex: true}}, [][]string{{"x", "y"}}, 0, 20},
		{"more columns than width", []components.Column{{Title: "A", Width: 1}, {Title: "B", Width: 1}, {Title: "C", Width: 1}}, [][]string{{"a", "b", "c"}}, 0, 2},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := components.RenderTable(tc.cols, tc.rows, tc.sel, tc.width, styles(t))

			want := tc.width
			if want < 0 {
				want = 0
			}

			for i, line := range got {
				if w := lipgloss.Width(line); w != want {
					t.Errorf("line %d width = %d, want %d", i, w, want)
				}
			}
		})
	}
}

// TestTableHandlesHostileCellContent: tags and payload previews come straight
// off the bus. CJK and emoji are two display cells wide, and a naive
// len()-based pad ragged-edges every row that contains one.
func TestTableHandlesHostileCellContent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cell string
	}{
		{"cjk wide runes", "salt/минион/日本語のタグ"},
		{"emoji", "salt/job/2026🔥/ret"},
		{"combining marks", "éééééé"},
		{"control characters", "salt/\x00\x07\x1b/ret"},
		{"a bare newline", "salt/job\nret"},
		{"a tab", "salt\tjob\tret"},
		{"an ansi escape from the bus", "\x1b[31mred\x1b[0m"},
		{"empty", ""},
	}

	cols := []components.Column{{Title: "Tag", Flex: true}, {Title: "N", Width: 4}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := components.RenderTable(cols, [][]string{{tc.cell, "1"}},
				components.NoSelection, 30, styles(t))

			for i, line := range got {
				if w := lipgloss.Width(line); w != 30 {
					t.Errorf("line %d width = %d, want 30", i, w)
				}

				if strings.ContainsAny(line, "\n") {
					t.Errorf("line %d contains a newline; one row must be one line", i)
				}
			}
		})
	}
}

// TestTableFlexColumnAbsorbsSlack: the flex column is what lets the tag column
// grow on a wide terminal instead of leaving dead space on the right.
func TestTableFlexColumnAbsorbsSlack(t *testing.T) {
	t.Parallel()

	cols := []components.Column{
		{Title: "T", Width: 8},
		{Title: "Tag", Flex: true},
	}

	long := strings.Repeat("x", 200)
	rows := [][]string{{"12:00:00", long}}

	st := styles(t)

	narrow := components.RenderTable(cols, rows, components.NoSelection, 30, st)
	wide := components.RenderTable(cols, rows, components.NoSelection, 90, st)

	if strings.Count(narrow[1], "x") >= strings.Count(wide[1], "x") {
		t.Error("the flex column did not absorb the extra width")
	}
}

// TestTableHeaderShowsColumnTitles: a table whose header is blank gives the
// operator no way to know what a column means.
func TestTableHeaderShowsColumnTitles(t *testing.T) {
	t.Parallel()

	cols := []components.Column{
		{Title: "TIME", Width: 8},
		{Title: "TAG", Flex: true},
	}

	got := components.RenderTable(cols, [][]string{{"12:00:00", "salt/x"}},
		components.NoSelection, 60, styles(t))

	if len(got) == 0 {
		t.Fatal("no lines rendered")
	}

	for _, title := range []string{"TIME", "TAG"} {
		if !strings.Contains(got[0], title) {
			t.Errorf("header %q is missing column title %q", got[0], title)
		}
	}
}

func TestNoSelectionIsNotAValidRowIndex(t *testing.T) {
	t.Parallel()

	if components.NoSelection >= 0 {
		t.Errorf("NoSelection = %d; it must never collide with a real row index",
			components.NoSelection)
	}
}
