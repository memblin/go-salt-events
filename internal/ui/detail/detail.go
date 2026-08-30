// Package detail renders one event's full payload (spec §7.2).
//
// It receives the decoder as a function so this package never imports
// internal/saltipc — the layer rule forbids the UI knowing the wire format
// (spec §3.1, enforced by depguard). cmd/salt-events injects
// saltipc.DecodeValue at construction.
//
// This is the ONLY place in the program a payload is fully decoded (spec §4.2,
// invariant 4), and it decodes once per selected event rather than once per
// frame: at the 10Hz render tick, decoding in View would re-parse the same
// megabyte ten times a second.
package detail

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/TKC-Labs/go-salt-events/internal/model"
	"github.com/TKC-Labs/go-salt-events/internal/theme"
	"github.com/TKC-Labs/go-salt-events/internal/ui"
	"github.com/TKC-Labs/go-salt-events/internal/ui/components"
)

// DecodeFunc turns a raw payload into a displayable value.
//
// The shape it returns is not free-form. saltipc.DecodeValue sets
// DecodeUntypedMap, so every map in the result is a map[interface{}]interface{}
// — see pairs, which is where that fact is handled and why.
type DecodeFunc func([]byte) (any, error)

// Pane shows one event.
type Pane struct {
	decode DecodeFunc

	ev  model.Event
	has bool

	// body is the rendered event, built once per selection. Lines are held
	// UNSTYLED so a theme switch needs no rebuild and so only the lines
	// actually on screen ever get styled.
	body  []line
	built bool

	offset int
}

// New returns a Detail pane using decode for payloads.
func New(decode DecodeFunc) *Pane {
	return &Pane{decode: decode, ev: model.Event{}, has: false, body: nil, built: false, offset: 0}
}

// Title implements ui.Pane.
func (p *Pane) Title() string { return "Detail" }

// SetStyles implements ui.Pane. Nothing here caches styles: colours are applied
// at render time from the *theme.Styles View is handed.
func (p *Pane) SetStyles(*theme.Styles) {}

// Keys implements ui.Pane.
func (p *Pane) Keys() []ui.KeyHint {
	return []ui.KeyHint{
		{Key: "↑/↓", Label: "scroll"},
		{Key: "g", Label: "top of payload"},
		{Key: "G", Label: "end of payload"},
	}
}

// SetEvent selects the event to display, discarding any previous render.
func (p *Pane) SetEvent(e model.Event) {
	p.ev, p.has = e, true
	p.body, p.built = nil, false
	p.offset = 0
}

// Update handles scrolling. The offset is clamped in View, which is the only
// place that knows how many lines the payload came to.
func (p *Pane) Update(msg tea.Msg, _ ui.Snapshot) (ui.Pane, tea.Cmd) {
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return p, nil
	}

	switch key.String() {
	case "down", "j":
		p.offset++
	case "up", "k":
		p.offset = max(0, p.offset-1)
	case "g", "home":
		p.offset = 0
	case "G", "end":
		p.offset = len(p.lines())
	}

	return p, nil
}

// View renders the event into the w×h content box.
func (p *Pane) View(w, h int, _ ui.Snapshot, st *theme.Styles) string {
	if w < 1 || h < 1 {
		return ""
	}

	if !p.has {
		return st.Muted.Render(components.Fit("no event selected — pick one in Live and press enter", w))
	}

	lines := p.lines()
	if len(lines) == 0 {
		return ""
	}

	// The stored offset is clamped HERE, not in Update, and written back: only
	// the render knows the payload's height. Without the write-back a held-down
	// key banks thousands of invisible presses, and the first few hundred
	// presses of `up` afterwards would do nothing at all.
	p.offset = min(max(0, p.offset), len(lines)-1)

	visible := lines[p.offset:]
	if len(visible) > h {
		visible = visible[:h]
	}

	out := make([]string, 0, len(visible))
	for _, l := range visible {
		out = append(out, l.render(w, st))
	}

	return strings.Join(out, "\n")
}

// lines returns the rendered event, decoding at most once per selection.
func (p *Pane) lines() []line {
	if p.built {
		return p.body
	}

	p.body = append(p.headLines(), p.payloadLines()...)
	p.built = true

	return p.body
}

// clockLayout is the wall-clock format for the two timestamps in the header.
const clockLayout = "15:04:05.000"

// headLines renders the eagerly-extracted fields: they are available whether or
// not the body survived, which is the whole point of the eager/lazy split
// (spec §4.2).
func (p *Pane) headLines() []line {
	out := []line{
		{label: "", text: sanitise(p.ev.Tag), slot: slotHeader},
		{label: "arrival ", text: p.ev.Arrival.Format(clockLayout), slot: slotValue},
	}

	// _stamp is set by whichever process fired the event and may be absent or
	// unparseable, so it is shown only when it parsed — and never used for
	// anything but display (spec §2.4, invariant 2).
	if !p.ev.Stamp.IsZero() {
		out = append(out, line{label: "_stamp  ", text: p.ev.Stamp.Format(clockLayout), slot: slotValue})
	}

	return append(out, line{label: "", text: "", slot: slotValue})
}

// payloadLines renders the payload, or the reason it is unavailable.
//
// The two unavailable cases carry DIFFERENT messages naming DIFFERENT fixes
// (spec §5.3 case A, invariant 5). A generic "payload unavailable" would send
// the operator to raise --max-memory for data Salt destroyed at the master,
// where no amount of local memory would have helped.
//
// The master-trimmed banner is emitted BEFORE the payload is decoded, not
// merged into the success path: a trimmed event whose remains also fail to
// decode must still say the master trimmed it. Attaching the banner to the
// decoded output loses that fact in exactly the case the operator most needs
// it.
func (p *Pane) payloadLines() []line {
	if p.ev.Shed {
		return shedLines()
	}

	var out []line

	if p.ev.MasterTrimmed {
		out = append(out, masterTrimmedLines()...)
	}

	if len(p.ev.Payload) == 0 {
		return append(out, line{label: "", text: "no payload", slot: slotMuted})
	}

	if p.decode == nil {
		return append(out, line{label: "", text: "no decoder was wired into this pane", slot: slotErr})
	}

	v, err := p.decode(p.ev.Payload)
	if err != nil {
		return append(out, line{label: "", text: "could not decode payload: " + sanitise(err.Error()), slot: slotErr})
	}

	return append(out, render(v)...)
}

// shedLines explains OUR truncation. It never mentions the master's knob.
func shedLines() []line {
	return []line{
		{label: "", text: "payload dropped by this tool's cache to stay inside its memory budget.", slot: slotMuted},
		{label: "", text: "The event's indexed fields above are intact; only the body is gone.", slot: slotMuted},
		{label: "", text: "Raise --max-memory to keep payloads for longer.", slot: slotMuted},
	}
}

// masterTrimmedLines explains the MASTER's truncation. It never mentions ours:
// the data was destroyed before this tool saw the event, so --max-memory is
// irrelevant and naming it would be actively misleading.
func masterTrimmedLines() []line {
	return []line{
		{label: "", text: "Salt trimmed this event at the master: it exceeded max_event_size,", slot: slotWarn},
		{label: "", text: "so values below may read " + valueTrimmed + ". This tool never saw the", slot: slotWarn},
		{label: "", text: "originals — raise max_event_size in /etc/salt/master to keep them.", slot: slotWarn},
		{label: "", text: "", slot: slotValue},
	}
}

// line is one output line, held unstyled.
//
// label is the key part and always renders in the KeyLabel style; text is the
// value and renders in slot's style. Splitting them is what lets a payload key
// and its value carry different weight without the pane composing styled
// strings it would then have to measure.
type line struct {
	label string
	text  string
	slot  slot
}

// render styles and truncates l to w cells.
func (l line) render(w int, st *theme.Styles) string {
	label := components.Fit(l.label, w)

	text := components.Fit(l.text, w-lipgloss.Width(label))
	if label == "" {
		return styleFor(l.slot, st).Render(text)
	}

	return st.KeyLabel.Render(label) + styleFor(l.slot, st).Render(text)
}

// slot names the style a line's value renders in. It is an index rather than a
// lipgloss.Style so a line can be built without a palette, which is what lets
// the payload be rendered once and styled per frame under whatever theme is
// current.
type slot uint8

const (
	slotValue slot = iota
	slotHeader
	slotMuted
	slotWarn
	slotErr
)

func styleFor(s slot, st *theme.Styles) lipgloss.Style {
	switch s {
	case slotHeader:
		return st.Header
	case slotMuted:
		return st.Muted
	case slotWarn:
		return st.Warn
	case slotErr:
		return st.Err
	case slotValue:
		return st.Value
	default:
		return st.Value
	}
}

// valueTrimmed is the literal Salt substitutes for a value it dropped at the
// master (spec §5.3). Seeing it means max_event_size, never --max-memory.
const valueTrimmed = "VALUE_TRIMMED"

// maxDepth bounds how deep the renderer recurses.
//
// A payload is minion-supplied and can legally be a megabyte of nothing but
// nested one-element maps, which is hundreds of thousands of levels — enough to
// overflow the goroutine stack. "Never panic on bus data" makes this a bound,
// not a comment.
const maxDepth = 32

// maxLines bounds how many lines one payload renders to. At the 1 MiB
// max_event_size ceiling an adversarial payload is worth ~500k lines; holding
// those as strings costs tens of MB on a machine already running a master.
// Nothing is lost that the operator could have read anyway.
const maxLines = 20000

// indentStep is one nesting level.
const indentStep = 2

// render turns a decoded payload into lines.
func render(v any) []line {
	r := &renderer{out: nil}
	r.node(0, "", false, v, 0)

	if len(r.out) >= maxLines {
		r.out = append(r.out, line{
			label: "",
			text:  "… display stopped after " + strconv.Itoa(maxLines) + " lines; the rest of this payload is not shown.",
			slot:  slotMuted,
		})
	}

	return r.out
}

type renderer struct{ out []line }

func (r *renderer) emit(l line) {
	if len(r.out) >= maxLines {
		return
	}

	r.out = append(r.out, l)
}

// node renders v. key names it when it is a map entry; item marks it as an
// element of a list.
func (r *renderer) node(indent int, key string, item bool, v any, depth int) {
	if len(r.out) >= maxLines {
		return
	}

	if depth > maxDepth {
		r.emit(line{label: head(indent, key, item, false), text: "…(nested too deeply to display)", slot: slotMuted})

		return
	}

	if kv, ok := pairs(v); ok {
		r.mapping(indent, key, item, kv, depth)

		return
	}

	if items, ok := v.([]interface{}); ok {
		r.sequence(indent, key, item, items, depth)

		return
	}

	text := scalar(v)

	r.emit(line{label: head(indent, key, item, false), text: text, slot: scalarSlot(text)})
}

func (r *renderer) mapping(indent int, key string, item bool, kv []pair, depth int) {
	if len(kv) == 0 {
		r.emit(line{label: head(indent, key, item, false), text: "{}", slot: slotValue})

		return
	}

	child := indent

	if key != "" || item {
		r.emit(line{label: head(indent, key, item, true), text: "", slot: slotValue})
		child += indentStep
	}

	for _, p := range kv {
		r.node(child, p.key, false, p.val, depth+1)
	}
}

func (r *renderer) sequence(indent int, key string, item bool, items []interface{}, depth int) {
	if len(items) == 0 {
		r.emit(line{label: head(indent, key, item, false), text: "[]", slot: slotValue})

		return
	}

	child := indent

	if key != "" || item {
		r.emit(line{label: head(indent, key, item, true), text: "", slot: slotValue})
		child += indentStep
	}

	for _, v := range items {
		r.node(child, "", true, v, depth+1)
	}
}

// head builds the label: the indent, plus the key or list bullet, plus the
// separator. A container's separator has no trailing space because its value is
// on the following lines.
func head(indent int, key string, item, container bool) string {
	pad := strings.Repeat(" ", indent)

	switch {
	case item && container:
		return pad + "-"
	case item:
		return pad + "- "
	case key == "":
		return pad
	case container:
		return pad + key + ":"
	default:
		return pad + key + ": "
	}
}

// pair is one map entry with its key already rendered as text.
type pair struct {
	key string
	val any
}

// pairs flattens a decoded map into key-sorted entries, reporting false for
// anything that is not a map.
//
// The map[interface{}]interface{} case is the one that matters and is listed
// first for that reason. saltipc.DecodeValue sets DecodeUntypedMap, so it
// returns map[interface{}]interface{} for EVERY map, at every nesting level,
// and never map[string]any — a renderer that type-asserted map[string]any would
// fall through to the scalar branch and print every real event in the program
// as one unreadable Go-syntax blob. That is not a hypothetical: it is what the
// first draft of this file did, against 32 frames captured off a live 3006.27
// master. map[string]any is still accepted because injected test decoders and
// any future decoder may produce it.
//
// Keys are rendered as text rather than asserted to string: one captured event
// carried a top-level key containing spaces ("Minion data cache refresh"), and
// msgpack permits non-string keys outright, so nothing here may assume an
// identifier shape.
//
// Sorting is by key text, which makes the render deterministic — Go map
// iteration is not, and an operator watching a payload reshuffle every frame
// would reasonably conclude the data was changing.
func pairs(v any) ([]pair, bool) {
	var out []pair

	switch t := v.(type) {
	case map[interface{}]interface{}:
		out = make([]pair, 0, len(t))
		for k, val := range t {
			out = append(out, pair{key: scalar(k), val: val})
		}
	case map[string]interface{}:
		out = make([]pair, 0, len(t))
		for k, val := range t {
			out = append(out, pair{key: sanitise(k), val: val})
		}
	default:
		return nil, false
	}

	sort.Slice(out, func(i, j int) bool { return out[i].key < out[j].key })

	return out, true
}

// scalar renders a leaf value.
//
// Every branch here is a shape observed in the live capture: retcode arrives as
// an int8, `return` is polymorphic (bool, string, or list), and ext-78
// datetimes decode to time.Time. Binary is summarised rather than printed —
// dumping raw bytes into a terminal is how a payload takes over the screen.
func scalar(v any) string {
	switch t := v.(type) {
	case nil:
		return "null"
	case string:
		return sanitise(t)
	case []byte:
		return "<" + strconv.Itoa(len(t)) + " bytes of binary>"
	case time.Time:
		return t.UTC().Format(time.RFC3339Nano)
	default:
		return sanitise(fmt.Sprint(t))
	}
}

// scalarSlot picks the style for a leaf. A value Salt replaced at the master
// must not read as a minion's actual answer (spec §5.3, invariant 5).
func scalarSlot(text string) slot {
	if text == valueTrimmed {
		return slotWarn
	}

	return slotValue
}

// sanitise replaces control characters with a visible placeholder.
//
// It is components.Sanitise under a local name because it is called from a
// dozen leaf cases below and the shorter name keeps them readable. The
// IMPLEMENTATION is shared by ruling: three panes each grew their own copy of
// this, the copies diverged, and that divergence was the root cause of the wave
// review's Critical finding. components' table does this for its own cells;
// this pane renders payload text OUTSIDE that table, so it must call it here.
// Payload content is minion-supplied and this tool runs as root on a production
// master: a raw ESC would let event data move the operator's cursor, set the
// window title, or repaint the screen, and a newline would add a row the height
// clamp never accounted for.
func sanitise(s string) string { return components.Sanitise(s) }
