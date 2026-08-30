package ui

import (
	"time"

	"github.com/TKC-Labs/go-salt-events/internal/model"
)

// TickMsg drives the render loop. It is the ONLY message that moves data into
// the UI: the reader goroutine never sends per-event messages, so the bubbletea
// queue cannot grow with event rate (spec §4.1, invariant 6).
type TickMsg time.Time

// OpenDetailMsg asks the root to show one event in the Detail pane and focus
// it. A pane returns it as a tea.Cmd; the ROOT handles it.
//
// It exists because a pane cannot reach another pane. Model.Update's default
// arm forwards to the FOCUSED pane, so before this message a tea.Cmd returned
// by Live was delivered straight back to Live and could never reach the root —
// which is why the drill-through of spec §7.2 and §7.5 shipped unbound rather
// than shipping as a key that silently did nothing.
//
// It carries the event by value. The Detail pane keeps the selection until it
// is replaced, so a reference into the snapshot would pin that snapshot's
// backing array for as long as the operator left the pane open.
type OpenDetailMsg struct {
	Event model.Event
}

// OpenJobReturnMsg asks the root to open one minion's return for a job in the
// Detail pane (spec §7.5, "enter on a minion row opens that return").
//
// The Jobs pane cannot resolve this itself: a job holds only the eagerly
// extracted fields, never the events (invariant 9), so the return's payload
// lives in the cache and nowhere else. The root resolves the pair against the
// snapshot it is already holding, and says so when it cannot — a shed, dropped
// or filtered-out return is an ordinary outcome, not a bug, and silently doing
// nothing would read as a broken key.
type OpenJobReturnMsg struct {
	JID    string
	Minion string
}

// NoticeMsg carries one line of transient text to the root's chrome, which
// clears it on the next keystroke.
//
// It exists so a pane can decline an action and say WHY. The failure this
// guards is the one that left the drill-through unbound in the first place: a
// key that appears to do nothing is indistinguishable from a broken one, and
// "this minion never returned, so there is no return to open" is a fact only
// the pane holds.
//
// It is not a general logging channel. Anything that must survive a keystroke
// belongs in a pane's own body.
type NoticeMsg string

// ReaderErrorMsg carries the reason the event reader stopped, already turned
// into an instruction by the wiring layer (spec §8.1).
//
// It is separate from NoticeMsg and deliberately so. A notice is transient and
// is cleared by the next keystroke; this is not — the reader does not come
// back, and until it is dismissed the console is showing history rather than a
// live bus, which the operator must be told. It is also multi-line, because
// the whole value of §8.1's diagnostics is the remedy underneath the cause:
// "permission denied" alone makes the operator guess, and `sudo salt-events`
// under it turns the screen into a single read.
//
// Without it the two most common first-run failures — forgetting sudo, and a
// master mid-restart — produced nothing on screen but DISCONNECTED, and the
// reason was printed only after the operator gave up and quit.
//
// The wiring layer builds the text with saltipc.Diagnose; internal/ui must not
// import the ingest layer (spec §3.1) and so is handed the finished string.
type ReaderErrorMsg string

// snapshotLimit bounds how many events a snapshot carries. The Live pane can
// only show a screenful; copying the whole cache every tick would make render
// cost scale with cache size rather than viewport size.
const snapshotLimit = 2000

// defaultInterval is used when Options.Interval is zero or negative.
// tea.Tick with a non-positive duration fires in a tight loop, which would
// pin a core and starve the reader goroutine — the exact failure mode the
// snapshot architecture exists to avoid.
const defaultInterval = 100 * time.Millisecond
