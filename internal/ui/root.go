package ui

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/TKC-Labs/go-salt-events/internal/filter"
	"github.com/TKC-Labs/go-salt-events/internal/model"
	"github.com/TKC-Labs/go-salt-events/internal/theme"
)

// ExportFunc writes the currently filtered event set and reports, in one line,
// what it did or why it refused.
//
// It is injected rather than imported because internal/export resolves a
// destination, checks free space and chowns to $SUDO_USER — all of which is the
// wiring layer's business, not the UI's. The root only knows that `w` runs it
// OFF the render loop (spec §10.3) and that the answer is one line of text.
type ExportFunc func(q filter.Query) (string, error)

// Options configures the root model.
type Options struct {
	Theme    string
	Interval time.Duration
	Filter   filter.Query
	SockPath string

	// ConfigPath is the config file the session resolved, shown in the help
	// overlay so a config that is not being read is diagnosable without strace
	// (spec §11). It is the path that WOULD be read, whether or not it exists.
	ConfigPath string

	// Export is what `w` runs. A nil Export leaves the key bound to a
	// diagnostic rather than to nothing: `w` is on the permanent hint strip, so
	// a build without an exporter must say so rather than appear broken.
	Export ExportFunc
}

// Model is the bubbletea root. It owns focus, the tick, and the single active
// *theme.Styles — which it is the only place in internal/ui to obtain.
type Model struct {
	src   Source
	panes []Pane
	focus int

	styles    *theme.Styles
	themeName string

	query     filter.Query
	filtering bool
	filterBuf string
	filterErr string

	snap Snapshot

	// notice is a one-line transient message — an export result, or the reason
	// a drill-through found nothing. It is cleared by the next keystroke, so it
	// cannot linger and be read as current.
	notice string

	interval   time.Duration
	sockPath   string
	configPath string
	export     ExportFunc

	width, height int

	// ready gates View. Nothing renders until a real WindowSizeMsg arrives:
	// without the gate the first frame draws at a fabricated 80x24 and then
	// visibly snaps.
	ready bool

	paused   bool
	showHelp bool
}

// NewModel builds the root over the panes it is given.
//
// An unknown or empty theme name falls back to the default rather than
// failing: a typo in a config file must not stop an incident console from
// starting. theme.StylesFor is the only route to a *Styles, and it sources its
// palette from the registry, so the styles here are always ones the contrast
// suite has validated (spec §3.2).
func NewModel(src Source, panes []Pane, opts Options) Model {
	st, ok := theme.StylesFor(opts.Theme)

	name := opts.Theme
	if !ok {
		name = theme.DefaultName
		st, _ = theme.StylesFor(name)
	}

	for _, pane := range panes {
		pane.SetStyles(st)
	}

	interval := opts.Interval
	if interval <= 0 {
		interval = defaultInterval
	}

	return Model{
		src:        src,
		panes:      panes,
		styles:     st,
		themeName:  name,
		query:      opts.Filter,
		interval:   interval,
		sockPath:   opts.SockPath,
		configPath: opts.ConfigPath,
		export:     opts.Export,
	}
}

// Init starts the render tick.
func (m Model) Init() tea.Cmd { return m.tick() }

func (m Model) tick() tea.Cmd {
	return tea.Tick(m.interval, func(t time.Time) tea.Msg { return TickMsg(t) })
}

// ThemeName returns the active palette name, for the status bar and for tests.
func (m Model) ThemeName() string { return m.themeName }

// SockPath returns the socket the session was started against, for the help
// overlay and startup diagnostics (spec §8.1).
func (m Model) SockPath() string { return m.sockPath }

// Update routes messages.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		// A resize can deliver zero or negative dimensions, and does so before
		// the first real size message on some terminals. Treating those as
		// "not ready yet" keeps every downstream width computation positive
		// instead of pushing the clamping into each pane.
		m.width, m.height = msg.Width, msg.Height
		m.ready = msg.Width > 0 && msg.Height > 0

		return m, nil

	case TickMsg:
		// The pin is synced even while paused. Pausing freezes what the
		// operator is LOOKING at, and the job they are looking at is precisely
		// the one that must not be evicted out from under them (spec §7.5).
		m.syncPin()

		// Pausing freezes the VIEW, never ingest — a paused UI that stopped
		// collecting would silently lose the storm the operator paused to
		// read (invariant 7). The reader goroutine is untouched here; all this
		// skips is the snapshot refresh.
		if !m.paused {
			m.snap = m.refresh()
		}

		return m, m.tick()

	case tea.KeyMsg:
		return m.handleKey(msg)

	// The three cases below are why Update has a message switch at all beyond
	// keys and ticks: without them a tea.Cmd returned by a pane is delivered
	// straight back to the FOCUSED pane by the default arm and can never reach
	// the root, which is what left the drill-through of spec §7.2 unbindable.
	case OpenDetailMsg:
		return m.openDetail(msg.Event), nil

	case OpenJobReturnMsg:
		return m.openJobReturn(msg), nil

	case NoticeMsg:
		m.notice = string(msg)

		return m, nil
	}

	return m.routeToPane(msg)
}

// syncPin tells the source which job a pane is holding open, every tick.
//
// It is a level rather than an edge — see JobPinner — and it is re-asserted on
// every tick because the alternative is a pin that survives the pane that set
// it. A Source that cannot pin is the ordinary case in tests and costs nothing.
func (m Model) syncPin() {
	pinner, ok := m.src.(Pinner)
	if !ok {
		return
	}

	pinner.PinJob(m.pinnedJID())
}

// pinnedJID is the first job any pane is holding open, or "".
func (m Model) pinnedJID() string {
	for _, p := range m.panes {
		holder, ok := p.(JobPinner)
		if !ok {
			continue
		}

		if jid := holder.PinnedJID(); jid != "" {
			return jid
		}
	}

	return ""
}

// openDetail hands an event to the Detail pane and focuses it.
func (m Model) openDetail(e model.Event) Model {
	for i, p := range m.panes {
		viewer, ok := p.(EventViewer)
		if !ok {
			continue
		}

		viewer.SetEvent(e)
		m.focus, m.showHelp = i, false

		return m
	}

	m.notice = "this build has no Detail pane"

	return m
}

// openJobReturn resolves a job/minion pair against the snapshot in hand.
//
// The scan is newest-first because a minion may return twice for one JID and
// the later return is the current answer. It is O(snapshot), not O(cache), and
// runs once per keypress rather than per frame.
//
// A miss is reported rather than ignored: the return may have been shed by the
// budget, dropped entirely, or simply hidden by the active filter, and all
// three are ordinary. A key that silently does nothing is indistinguishable
// from a broken one.
func (m Model) openJobReturn(msg OpenJobReturnMsg) Model {
	for i := len(m.snap.Events) - 1; i >= 0; i-- {
		e := m.snap.Events[i]
		if e.Kind == model.KindRet && e.JID == msg.JID && e.Minion == msg.Minion {
			return m.openDetail(e)
		}
	}

	m.notice = "no cached return from " + msg.Minion + " for job " + msg.JID +
		" — it may have aged out of the cache, or the active filter hides it"

	return m
}

// refresh pulls a snapshot, tolerating a nil Source so a model can be built
// and rendered before the ingest hub exists.
func (m Model) refresh() Snapshot {
	if m.src == nil {
		return m.snap
	}

	return m.src.Snapshot(m.query, snapshotLimit)
}

// handleKey applies global keys, then the filter editor, then the pane.
func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.filtering {
		return m.handleFilterKey(msg)
	}

	// Any keystroke clears the notice: it reports what just happened, and a
	// stale one read as current would be worse than none.
	m.notice = ""

	key := msg.String()

	if i, ok := paneIndex(key, len(m.panes)); ok {
		m.focus = i

		return m, nil
	}

	switch key {
	case keyQuit, keyInterrupt:
		return m, tea.Quit
	case keyNextPane, keyPrevPane:
		m.focus = m.step(key)

		return m, nil
	case keyTheme:
		return m.cycleTheme(), nil
	case keyPause:
		m.paused = !m.paused

		return m, nil
	case keyFilter:
		m.filtering, m.filterBuf, m.filterErr = true, m.query.String(), ""

		return m, nil
	case keyHelp:
		m.showHelp = !m.showHelp

		return m, nil
	case keyExport:
		return m, m.exportCmd()
	}

	return m.routeToPane(msg)
}

// step returns the next focus index, wrapping in either direction. It is a
// no-op with no panes: len(m.panes) is a divisor, and a build wired with an
// empty pane list must degrade to an empty frame, not a panic.
func (m Model) step(key string) int {
	n := len(m.panes)
	if n == 0 {
		return 0
	}

	if key == keyPrevPane {
		return (m.focus + n - 1) % n
	}

	return (m.focus + 1) % n
}

// handleFilterKey edits the filter query.
func (m Model) handleFilterKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case keyEscape:
		m.filtering, m.filterErr = false, ""

		return m, nil
	case keyEnter:
		return m.commitFilter()
	case keyBackspace:
		if m.filterBuf != "" {
			m.filterBuf = m.filterBuf[:len(m.filterBuf)-1]
		}

		return m, nil
	}

	if r := msg.Runes; len(r) > 0 {
		m.filterBuf += string(r)
	} else if msg.String() == keyPause {
		m.filterBuf += keyPause
	}

	return m, nil
}

// commitFilter parses the edited query and applies it.
//
// A malformed query leaves the PREVIOUS filter active rather than clearing the
// view: an empty pane reads as "there are no such events", which is a
// different and much worse message than "your query is wrong" (spec §6).
func (m Model) commitFilter() (tea.Model, tea.Cmd) {
	q, err := filter.Parse(m.filterBuf)
	if err != nil {
		m.filterErr = err.Error()

		return m, nil
	}

	m.query, m.filtering, m.filterErr = q, false, ""
	m.snap = m.refresh()

	return m, nil
}

// exportCmd runs the export as a tea.Cmd — i.e. on bubbletea's own goroutine,
// OFF the render loop, so a multi-hundred-megabyte NDJSON write cannot stall
// the frame (spec §10.3). Ingest is untouched throughout: the exporter reads a
// copy taken under the ingest lock, it does not hold that lock while writing.
//
// The whole answer, including a refusal, comes back as one notice line. The
// modal of spec §10.2 is not built here; the refusal text carries the estimate,
// the space available and the headroom rule, which is the part that tells the
// operator what to do.
func (m Model) exportCmd() tea.Cmd {
	if m.export == nil {
		return func() tea.Msg { return NoticeMsg("export is not wired into this build") }
	}

	// Captured by value: Model is copied on every Update, and the closure must
	// not read fields off a Model that has since moved on.
	export, q := m.export, m.query

	return func() tea.Msg {
		note, err := export(q)
		if err != nil {
			return NoticeMsg("export refused: " + err.Error())
		}

		return NoticeMsg(note)
	}
}

// cycleTheme switches palettes live.
//
// A failed lookup leaves the current styles in place; it cannot happen while
// Next only returns registered names, but returning a nil *Styles here would
// nil-panic in every pane's View on the next frame.
func (m Model) cycleTheme() Model {
	name := theme.Next(m.themeName)

	st, ok := theme.StylesFor(name)
	if !ok {
		return m
	}

	m.themeName, m.styles = name, st

	for _, pane := range m.panes {
		pane.SetStyles(st)
	}

	return m
}

// routeToPane forwards a message to the focused pane.
func (m Model) routeToPane(msg tea.Msg) (tea.Model, tea.Cmd) {
	if len(m.panes) == 0 {
		return m, nil
	}

	p, cmd := m.panes[m.focus].Update(msg, m.snap)
	m.panes[m.focus] = p

	return m, cmd
}

// chromeRows is the number of full-width rows the root draws around the pane:
// tabs, filter bar, hints, status.
const chromeRows = 4

// borderCells is the two columns and two rows the pane frame occupies.
const borderCells = 2

// View renders the whole frame.
//
// The root owns the pane border and passes each pane the CONTENT box with the
// border already subtracted. Panes must not draw their own — the border is the
// most theme-visible element on screen, so a pane that draws its own makes
// theme switching look like it only applies to some panes.
func (m Model) View() string {
	if !m.ready || len(m.panes) == 0 {
		return ""
	}

	contentW := max(1, m.width-borderCells)
	contentH := max(1, m.height-chromeRows-borderCells)

	// The focused pane always owns the frame: only one pane is on screen at a
	// time, so an unfocused border would never be drawn.
	frame := m.styles.PaneFocus.Width(contentW).Height(contentH)

	body := m.helpView(contentW, contentH)
	if !m.showHelp {
		body = m.panes[m.focus].View(contentW, contentH, m.snap, m.styles)
	}

	return lipgloss.JoinVertical(lipgloss.Left,
		m.tabsView(m.width),
		frame.Render(body),
		m.filterBarView(m.width),
		m.hintsView(m.width),
		m.statusView(m.width),
	)
}
