package ui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

const (
	sep      = " · "
	tabSpace = " "

	// hintGap separates two hints; sep separates the pane's block of hints
	// from the global block, so the two read as two groups rather than one
	// long undifferentiated run.
	hintGap = "  "
)

// tabsView renders the numbered pane strip.
func (m Model) tabsView(w int) string {
	parts := make([]string, 0, len(m.panes))

	for i, p := range m.panes {
		label := fmt.Sprintf(" %d %s ", i+1, p.Title())

		if i == m.focus {
			parts = append(parts, m.styles.TableRowSel.Render(label))

			continue
		}

		parts = append(parts, m.styles.Muted.Render(label))
	}

	return lipgloss.NewStyle().MaxWidth(w).Render(strings.Join(parts, ""))
}

// hintsView keeps the keys permanently on screen. A read-only console is often
// someone's first contact with the tool during an incident, and a key you
// cannot see is a key you do not press.
//
// The focused pane's own keys lead, because they are the ones an operator
// cannot already know: the global block is identical on every pane and is
// learnt once, while a pane-owned binding exists only while that pane is
// focused. Leading also means the pane's keys are the last thing lost when a
// narrow window truncates the line.
func (m Model) hintsView(w int) string {
	parts := make([]string, 0, len(hints))
	for _, h := range hints {
		parts = append(parts, m.hintCell(h[0], h[1]))
	}

	line := strings.Join(parts, hintGap)

	// A pane with no keys of its own contributes nothing at all — no empty
	// group, no dangling separator.
	if pane := m.paneHints(); pane != "" {
		line = pane + m.styles.Muted.Render(sep) + line
	}

	// Truncate before padding: the hint line is the widest chrome row, and a
	// line wider than the window would push every other row's padding out with
	// it rather than simply overflowing on its own.
	return m.styles.StatusBar.Width(w).Render(lipgloss.NewStyle().MaxWidth(w).Render(line))
}

// paneHints renders the focused pane's own keys, or "" when it has none.
//
// Keys is asked here rather than cached because a pane's bindings may depend
// on its state. An empty or nil result is ordinary, and a hint with neither
// key nor label is dropped rather than rendered as a stray separator.
func (m Model) paneHints() string {
	if len(m.panes) == 0 || m.focus < 0 || m.focus >= len(m.panes) {
		return ""
	}

	keys := m.panes[m.focus].Keys()

	parts := make([]string, 0, len(keys))

	for _, k := range keys {
		if k.Key == "" && k.Label == "" {
			continue
		}

		parts = append(parts, m.hintCell(k.Key, k.Label))
	}

	return strings.Join(parts, hintGap)
}

// hintCell renders one key and its label.
func (m Model) hintCell(key, label string) string {
	return m.styles.Header.Render(key) + m.styles.Muted.Render(tabSpace+label)
}

// helpView replaces the pane body while `?` is held open, rather than
// overlaying it. An overlay would have to be composited over content it does
// not own, and at 10Hz any mistake in that composite strobes.
func (m Model) helpView(w, h int) string {
	lines := make([]string, 0, len(hints)+1)
	lines = append(lines, m.styles.Header.Render("keys"))

	for _, hint := range hints {
		lines = append(lines,
			m.styles.KeyLabel.Render(fmt.Sprintf("%8s", hint[0]))+
				m.styles.Value.Render("  "+hint[1]))
	}

	if len(lines) > h {
		lines = lines[:h]
	}

	return lipgloss.NewStyle().MaxWidth(w).Render(strings.Join(lines, "\n"))
}

// filterBarView shows the active query, or the editor, or a parse error.
func (m Model) filterBarView(w int) string {
	style := lipgloss.NewStyle().MaxWidth(w)

	switch {
	case m.filterErr != "":
		return style.Render(m.styles.Err.Render("filter: " + m.filterErr))
	case m.filtering:
		return style.Render(m.styles.Value.Render("/" + m.filterBuf + "▏"))
	case !m.query.IsZero():
		return style.Render(m.styles.Muted.Render("filter: " + m.query.String()))
	default:
		return ""
	}
}

// statusView renders connection, cache pressure, pause state, and theme.
func (m Model) statusView(w int) string {
	conn := "connected"
	if !m.snap.Connected {
		conn = "DISCONNECTED"
	}

	cs := m.snap.Cache

	left := strings.Join([]string{
		conn,
		fmt.Sprintf("%d events", cs.Events),
		// Shed and dropped are shown separately: they are different degrees of
		// loss with the same fix, and a single number would hide that the
		// cache has started discarding events entirely (spec §5.2).
		fmt.Sprintf("cache %s/%s", humanBytes(cs.Used), humanBytes(cs.Budget)),
		fmt.Sprintf("shed %d drop %d", cs.Shed, cs.Dropped),
	}, sep)

	if m.paused {
		left = "PAUSED" + sep + left
	}

	right := m.styles.KeyLabel.Render("theme ") + m.themeName + m.styles.Muted.Render(" [t]")

	return m.styles.StatusBar.Width(w).Render(justify(left, right, w))
}

// justify packs left and right onto exactly w columns, truncating left.
func justify(left, right string, w int) string {
	room := max(0, w-lipgloss.Width(right)-1)

	if lipgloss.Width(left) > room {
		left = lipgloss.NewStyle().MaxWidth(room).Render(left)
	}

	gap := max(1, w-lipgloss.Width(left)-lipgloss.Width(right))

	return left + strings.Repeat(tabSpace, gap) + right
}

// suffixes are the byte-count units above bytes.
const suffixes = "KMGT"

// humanBytes renders a byte count compactly.
//
// exp is clamped to the last suffix rather than trusted to stay in range: an
// int64 reaches exbibytes, which would index past "KMGT" and panic. The status
// bar is drawn ten times a second from numbers that ultimately come off the
// bus, so it does not get to assume they are sane.
func humanBytes(n int64) string {
	const unit = 1024

	if n < unit {
		return fmt.Sprintf("%dB", n)
	}

	div, exp := int64(unit), 0

	for v := n / unit; v >= unit && exp < len(suffixes)-1; v /= unit {
		div *= unit
		exp++
	}

	return fmt.Sprintf("%.0f%c", float64(n)/float64(div), suffixes[exp])
}
