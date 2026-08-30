package ui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/TKC-Labs/go-salt-events/internal/ui/components"
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
//
// It carries three things beyond the global keys, each because it is otherwise
// undiscoverable:
//
//   - the FOCUSED pane's own Keys(), which are the ones an operator cannot
//     already know — the global block is the same on every pane and is learnt
//     once;
//   - the filter language, including the one piece of it that reliably
//     surprises people: `minion:*` matches events with NO minion as well, and
//     `minion:?*` is the "has a minion" spelling;
//   - the resolved socket and config paths, so a config file that is not being
//     read is diagnosable without strace (spec §11).
func (m Model) helpView(w, h int) string {
	lines := m.helpKeyLines(w)

	lines = append(lines, "", m.helpHeader("filter language  (/ to edit, esc to cancel)", w))

	for _, row := range filterHelp {
		lines = append(lines, m.helpRow(row[0], row[1], w))
	}

	lines = append(lines, "", m.helpHeader("this session", w))
	lines = append(lines,
		m.helpRow("socket", pathOrNone(m.sockPath), w),
		m.helpRow("config", pathOrNone(m.configPath), w))

	if len(lines) > h {
		lines = lines[:h]
	}

	return strings.Join(lines, "\n")
}

// helpKeyLines renders the global block followed by the focused pane's own.
//
// The pane's keys are here because the `?` overlay is where an operator looks
// for a complete list, and the hint line under the frame truncates first.
func (m Model) helpKeyLines(w int) []string {
	lines := make([]string, 0, len(hints)+len(filterHelp)+helpExtraLines)
	lines = append(lines, m.helpHeader("keys", w))

	for _, hint := range hints {
		lines = append(lines, m.helpRow(hint[0], hint[1], w))
	}

	if len(m.panes) == 0 || m.focus < 0 || m.focus >= len(m.panes) {
		return lines
	}

	pane := m.panes[m.focus]

	keys := pane.Keys()
	if len(keys) == 0 {
		return lines
	}

	lines = append(lines, "", m.helpHeader(pane.Title()+" pane", w))
	for _, k := range keys {
		lines = append(lines, m.helpRow(k.Key, k.Label, w))
	}

	return lines
}

// helpHeader renders one section heading, fitted like every other line.
func (m Model) helpHeader(text string, w int) string {
	return m.styles.Header.Render(components.Fit(components.Sanitise(text), w))
}

// helpKeyColumn is the width the key column is padded to. The label column then
// starts in the same place on every row, which is what makes the block
// scannable rather than a list of sentences.
//
// It must be at least as wide as the longest key printed in it — the filter
// examples, not the keystrokes — because PadTo TRUNCATES an over-long value.
// A narrower column silently renders `minion:?*` as `minion:?`, which is a
// different and valid query, so the overlay would be teaching the wrong thing.
const helpKeyColumn = 14

// helpExtraLines is the headroom helpKeyLines leaves for the section headers,
// blank separators and path rows appended after the key block. It only sizes
// the initial allocation.
const helpExtraLines = 12

// helpRow renders one key/label pair, fitted so a narrow window truncates
// rather than wrapping — lipgloss Height is a MINIMUM, so a wrapped line grows
// the root's frame and pushes the status bar off the bottom of the terminal.
func (m Model) helpRow(key, label string, w int) string {
	col := min(helpKeyColumn, max(0, w))

	return m.styles.KeyLabel.Render(components.PadTo(components.Sanitise(key), col)) +
		m.styles.Value.Render(components.Fit("  "+components.Sanitise(label), w-col))
}

// filterHelp is the query language, in the order an operator meets it
// (spec §6).
var filterHelp = [...][2]string{
	{"salt/job/*", "a bare term is a TAG GLOB, fnmatch — Salt's own semantics"},
	{"minion:web-1", "field term; terms are space-separated and AND together"},
	{"fields", "minion: jid: fun: ok: ns: kind:"},
	{"minion:*", "matches events with NO minion too — * matches the empty field"},
	{"minion:?*", "the \"has a minion\" spelling: one character, then anything"},
	{"ok:false", "only failed returns"},
}

// pathOrNone renders a resolved path, or says it was never resolved. An empty
// line here would read as a path of "", which is a real and different thing.
func pathOrNone(p string) string {
	if p == "" {
		return "(none)"
	}

	return components.Sanitise(p)
}

// filterBarView shows the active query, or the editor, or a parse error.
func (m Model) filterBarView(w int) string {
	style := lipgloss.NewStyle().MaxWidth(w)

	switch {
	case m.filterErr != "":
		return style.Render(m.styles.Err.Render("filter: " + m.filterErr))
	case m.filtering:
		return style.Render(m.styles.Value.Render("/" + m.filterBuf + "▏"))
	case m.notice != "":
		// Sanitised because a notice can quote a minion ID or a JID straight
		// off the bus, and this tool runs as root: a raw ESC in a tag would
		// otherwise reach the operator's terminal (components.Sanitise, by
		// ruling — there is one implementation of this).
		return style.Render(m.styles.Warn.Render(components.Sanitise(m.notice)))
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
