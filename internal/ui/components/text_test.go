package components_test

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"

	"github.com/TKC-Labs/go-salt-events/internal/ui/components"
)

// These helpers are shared by five panes, so a defect here is a defect in all
// five at once. That is the trade the consolidation ruling accepted: the copies
// diverging was the worse failure, but the price is that this file has to be
// the strongest test in the package rather than the thinnest.

func TestSanitiseReplacesEveryControlCharacter(t *testing.T) {
	t.Parallel()

	// The exact sequences a hostile event.send would carry. This tool is
	// expected to run as root on a production master.
	const attack = "salt/\x1b]0;pwned\x07new\tjob\nline\x1b[2J\x7f\u0085"

	got := components.Sanitise(attack)

	for _, bad := range []string{"\x1b", "\x07", "\t", "\n", "\x7f", "\u0085"} {
		if strings.Contains(got, bad) {
			t.Errorf("control character %q survived: %q", bad, got)
		}
	}

	// Everything printable is untouched, so a sanitised tag is still readable.
	if !strings.Contains(got, "salt/") || !strings.Contains(got, "new") {
		t.Errorf("printable text was damaged: %q", got)
	}

	if clean := "salt/job/*/ret/*"; components.Sanitise(clean) != clean {
		t.Errorf("clean text was rewritten: %q", components.Sanitise(clean))
	}
}

// TestFitLeavesStyledTextIntact is why Fit does not sanitise.
//
// Fit is the last width pass a pane runs over its OWN already-styled lines. If
// it replaced the ESC introducing each SGR sequence, every pane would print its
// escape codes as literal garbage instead of colour.
func TestFitLeavesStyledTextIntact(t *testing.T) {
	t.Parallel()

	styled := lipgloss.NewStyle().Bold(true).Render("abcdef")

	if got := components.Fit(styled, 10); got != styled {
		t.Errorf("Fit rewrote a short styled line:\n%q\n%q", got, styled)
	}

	got := components.Fit(styled, 3)
	if lipgloss.Width(got) != 3 {
		t.Errorf("Fit(styled, 3) is %d cells wide: %q", lipgloss.Width(got), got)
	}

	if !strings.Contains(got, "\x1b[") {
		t.Errorf("Fit destroyed the styling it was handed: %q", got)
	}
}

func TestFitNeverExceedsItsWidth(t *testing.T) {
	t.Parallel()

	for _, s := range []string{"", "a", "abcdef", "世界世界世界", strings.Repeat("x", 500)} {
		for _, w := range []int{-1, 0, 1, 2, 5, 40} {
			if got := components.Fit(s, w); lipgloss.Width(got) > max(0, w) {
				t.Errorf("Fit(%q, %d) is %d cells wide", s, w, lipgloss.Width(got))
			}
		}
	}
}

// TestClipMarksEveryCutAndOnlyACut: an unmarked truncation reads as a value
// that genuinely ends there, which for a Salt tag is a different tag.
func TestClipMarksEveryCutAndOnlyACut(t *testing.T) {
	t.Parallel()

	if got := components.Clip("short", 20); got != "short" {
		t.Errorf("Clip marked a value it did not cut: %q", got)
	}

	for _, w := range []int{1, 2, 5, 12, 40} {
		got := components.Clip(strings.Repeat("a", 200), w)

		if lipgloss.Width(got) > w {
			t.Errorf("w=%d: Clip returned %d cells: %q", w, lipgloss.Width(got), got)
		}

		if !strings.Contains(got, components.Ellipsis) {
			t.Errorf("w=%d: a truncated value carries no marker: %q", w, got)
		}
	}

	if got := components.Clip("anything", 0); got != "" {
		t.Errorf("Clip(_, 0) = %q, want empty", got)
	}
}

// TestClipSanitisesBeforeMeasuring: Clip is the door bus-derived text comes
// through, so a raw escape must never survive it — and a newline must never
// turn one row into several, which is how a pane's height clamp gets defeated
// and the root's frame grows.
func TestClipSanitisesBeforeMeasuring(t *testing.T) {
	t.Parallel()

	got := components.Clip("a\x1b]0;pwned\x07b\nc\td", 40)

	for _, bad := range []string{"\x1b", "\x07", "\t", "\n", "\x7f", "\u0085"} {
		if strings.Contains(got, bad) {
			t.Errorf("control sequence %q survived Clip: %q", bad, got)
		}
	}

	if strings.Contains(got, "\n") {
		t.Errorf("Clip emitted more than one line: %q", got)
	}
}

// TestClipBoundsItsInputByBytes is invariant 6.
//
// A minion can event.send a 50,000-character tag and the pane redraws ten times
// a second, so the work must be proportional to the COLUMN, not to the input.
//
// The zero-width case is the one with teeth, and it is why the bound is a BYTE
// bound taken FIRST rather than a width check. Fifty thousand U+200B zero-width
// spaces are 150,000 bytes and measure ZERO display cells, so every width-based
// guard downstream waves them straight through: delete the leading byte cut and
// this call returns all 150,000 bytes, which a pane then puts on the wire to
// the terminal ten times a second. With the cut it returns 33.
func TestClipBoundsItsInputByBytes(t *testing.T) {
	t.Parallel()

	const pathological = 50_000

	for name, key := range map[string]string{
		"ascii":       strings.Repeat("a", pathological),
		"wide runes":  strings.Repeat("世", pathological),
		"control run": strings.Repeat("\x1b", pathological),
		"zero width":  strings.Repeat("\u200b", pathological),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			for _, w := range []int{1, 8, 32, 100} {
				got := components.Clip(key, w)

				if lipgloss.Width(got) > w {
					t.Errorf("w=%d: %d cells wide", w, lipgloss.Width(got))
				}

				// Four bytes per cell is the bound, plus the marker; anything
				// near the input length means the bound is gone.
				if len(got) > 4*w+len(components.Ellipsis) {
					t.Errorf("w=%d: returned %d bytes for a %d-char input",
						w, len(got), pathological)
				}
			}
		})
	}
}

// TestClipNeverSplitsARune: the byte bound cuts mid-rune by design, and the
// partial rune has to go rather than reach the terminal as U+FFFD noise.
func TestClipNeverSplitsARune(t *testing.T) {
	t.Parallel()

	for _, w := range []int{1, 2, 3, 4, 5, 6, 7, 8} {
		got := components.Clip(strings.Repeat("世", 100), w)

		if !isValidUTF8(got) {
			t.Errorf("w=%d: Clip produced invalid UTF-8: %q", w, got)
		}

		if lipgloss.Width(got) > w {
			t.Errorf("w=%d: %d cells wide: %q", w, lipgloss.Width(got), got)
		}
	}
}

func isValidUTF8(s string) bool {
	for _, r := range s {
		if r == '�' {
			return false
		}
	}

	return true
}

func TestPadToRendersExactlyItsWidth(t *testing.T) {
	t.Parallel()

	for _, s := range []string{"", "ab", "abcdefghij", "世界"} {
		for _, w := range []int{1, 4, 12} {
			if got := components.PadTo(s, w); lipgloss.Width(got) != w {
				t.Errorf("PadTo(%q, %d) is %d cells wide: %q", s, w, lipgloss.Width(got), got)
			}
		}
	}

	if got := components.PadTo("ab", 0); got != "" {
		t.Errorf("PadTo(_, 0) = %q, want empty", got)
	}
}

// TestRankedLabelNamesTheEmptyKey pins the wording the Rate and Summary panes
// share. They render the SAME stats.Entry slices, and an operator compares the
// two screens: two spellings of one key read as two different keys, and a blank
// label leaves a bar and a percentage attached to nothing.
func TestRankedLabelNamesTheEmptyKey(t *testing.T) {
	t.Parallel()

	got := components.RankedLabel("", components.PublishAck, 40)
	if !strings.Contains(got, components.PublishAck) {
		t.Errorf("an empty category is unlabelled: %q", got)
	}

	if strings.TrimSpace(got) == "" {
		t.Error("an empty key produced a blank label")
	}

	if got := components.RankedLabel("", components.NoKey, 40); !strings.Contains(got, "(none)") {
		t.Errorf("an empty minion is unlabelled: %q", got)
	}

	// A wildcard was the rejected earlier attempt: it collides with a
	// minion-sendable literal "*" tag.
	if strings.Contains(components.PublishAck+components.NoKey, "*") {
		t.Error("an empty key must never be spelled as a wildcard")
	}

	// A real key is passed through, sanitised, bounded and marked.
	if got := components.RankedLabel("salt/job/*/ret/*", components.PublishAck, 40); !strings.Contains(
		got, "salt/job/*/ret/*") {
		t.Errorf("a real key was rewritten: %q", got)
	}

	if got := components.RankedLabel("web\x1b[2Jok", components.NoKey, 40); strings.Contains(got, "\x1b") {
		t.Errorf("a hostile key survived RankedLabel: %q", got)
	}

	for _, w := range []int{1, 5, 24, 40} {
		if got := components.RankedLabel("", components.PublishAck, w); lipgloss.Width(got) != w {
			t.Errorf("w=%d: RankedLabel is %d cells wide: %q", w, lipgloss.Width(got), got)
		}
	}
}
