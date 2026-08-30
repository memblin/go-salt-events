package detail_test

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"

	"github.com/TKC-Labs/go-salt-events/internal/model"
	"github.com/TKC-Labs/go-salt-events/internal/theme"
	"github.com/TKC-Labs/go-salt-events/internal/ui"
	"github.com/TKC-Labs/go-salt-events/internal/ui/detail"
)

// TestMain forces a colour profile: `go test` runs without a TTY, where
// lipgloss strips every escape sequence, and the theme guard below would then
// compare two identical uncoloured strings and pass vacuously.
func TestMain(m *testing.M) {
	lipgloss.SetColorProfile(termenv.TrueColor)
	os.Exit(m.Run())
}

// styles fetches a registered palette's styles.
//
// The task brief called theme.Compile here; that is only reachable from inside
// the theme package's own test binary (internal/theme/export_test.go).
// theme.StylesFor is the route in from outside and it sources its palette from
// the registry, which is the point (spec §3.2).
func styles(t *testing.T, name string) *theme.Styles {
	t.Helper()

	st, ok := theme.StylesFor(name)
	if !ok {
		t.Fatalf("no such palette %q", name)
	}

	return st
}

// somePayload is a non-empty stand-in. Nothing in this package parses it — the
// decoder is injected — but bodyLines must see a payload to decode at all.
var somePayload = []byte{0x80}

// untyped mirrors what saltipc.DecodeValue actually returns.
//
// DecodeValue sets DecodeUntypedMap, so every map it hands back is a
// map[interface{}]interface{} and never a map[string]any. Tests that decode to
// map[string]any pass against a pane that cannot render a single real event,
// which is exactly the trap this package exists downstream of.
func untyped() any {
	return map[interface{}]interface{}{
		"fun":     "state.apply",
		"retcode": int8(0), // int8 is what a real retcode arrives as
		"success": true,
		"return":  []interface{}{true, "changed", nil}, // polymorphic
		// A real event off a 3006.27 master carried a top-level key with
		// spaces in it. Nothing may assume identifier-shaped keys.
		"Minion data cache refresh": true,
		"nested": map[interface{}]interface{}{
			"cmd_|-run_|-echo_|-run": map[interface{}]interface{}{"result": false},
		},
	}
}

func decodesTo(v any) detail.DecodeFunc {
	return func([]byte) (any, error) { return v, nil }
}

// TestDetailRendersTheDecoderTheProjectActuallyHas is the load-bearing test of
// this package: the pane must render the type saltipc.DecodeValue really
// returns, not the type a hand-written test fixture makes convenient.
func TestDetailRendersTheDecoderTheProjectActuallyHas(t *testing.T) {
	t.Parallel()

	p := detail.New(decodesTo(untyped()))
	p.SetEvent(model.Event{Tag: "salt/job/1/ret/m", Payload: somePayload})

	got := ansi.Strip(p.View(120, 60, ui.Snapshot{}, styles(t, "gruvbox-dark")))

	for _, want := range []string{
		"fun", "state.apply",
		"retcode", "0",
		"return", "changed",
		"Minion data cache refresh",
		"cmd_|-run_|-echo_|-run",
		"result", "false",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered payload is missing %q:\n%s", want, got)
		}
	}

	// The failure mode this guards against is not "missing a field" but
	// "rendered the Go value as one unreadable blob, or gave up entirely".
	for _, never := range []string{"unsupported type", "map[interface", "could not render"} {
		if strings.Contains(got, never) {
			t.Errorf("output contains %q; the renderer does not handle the real decode type:\n%s", never, got)
		}
	}
}

func TestDetailDistinguishesShedFromMasterTrimmed(t *testing.T) {
	t.Parallel()

	st := styles(t, "gruvbox-dark")

	shed := detail.New(decodesTo(map[string]any{"k": "v"}))
	shed.SetEvent(model.Event{Tag: "salt/a", Shed: true})
	shedOut := ansi.Strip(shed.View(100, 40, ui.Snapshot{}, st))

	trimmed := detail.New(decodesTo(untyped()))
	trimmed.SetEvent(model.Event{Tag: "salt/b", MasterTrimmed: true, Payload: somePayload})
	trimOut := ansi.Strip(trimmed.View(100, 40, ui.Snapshot{}, st))

	if !strings.Contains(shedOut, "--max-memory") {
		t.Errorf("shed message does not name the fix:\n%s", shedOut)
	}

	if !strings.Contains(trimOut, "max_event_size") {
		t.Errorf("trimmed message does not name the fix:\n%s", trimOut)
	}

	if strings.Contains(shedOut, "max_event_size") {
		t.Error("the shed message names the MASTER's knob; that is the wrong fix")
	}

	if strings.Contains(trimOut, "--max-memory") {
		t.Error("the master-trimmed message names OUR knob; no amount of local memory would have kept that data")
	}

	// The two must also be distinguishable about WHO dropped it, not just
	// about which flag to turn (spec §5.3 case A).
	if !strings.Contains(trimOut, "master") {
		t.Errorf("the master-trimmed message does not say the master dropped it:\n%s", trimOut)
	}
}

// TestDetailMarksAValueTrimmedByTheMasterInsideThePayload covers the other half
// of §5.3 case A: Salt substitutes the literal VALUE_TRIMMED for an oversize
// value, and that substitution is the master's, not ours.
func TestDetailMarksAValueTrimmedByTheMasterInsideThePayload(t *testing.T) {
	t.Parallel()

	st := styles(t, "gruvbox-dark")

	// Both renders differ ONLY in the value text, so blanking that text out
	// leaves nothing but the styling. Comparing whole lines instead would pass
	// on a pane that styles nothing at all, because "VALUE_TRIMMED" and
	// "returned-normally" are different strings whatever colour they are.
	render := func(value string) string {
		p := detail.New(decodesTo(map[interface{}]interface{}{"minions": value}))
		p.SetEvent(model.Event{Tag: "salt/job/1/new", Payload: somePayload})

		out := p.View(100, 40, ui.Snapshot{}, st)

		if !strings.Contains(ansi.Strip(out), value) {
			t.Fatalf("value %q did not render at all:\n%s", value, ansi.Strip(out))
		}

		return strings.ReplaceAll(out, value, "<value>")
	}

	// An operator scanning a large payload has to see that a value was
	// destroyed at the master, not read VALUE_TRIMMED as a minion's answer.
	if trimmed, ordinary := render("VALUE_TRIMMED"), render("returned-normally"); trimmed == ordinary {
		t.Errorf("a master-trimmed value is styled exactly like an ordinary one:\n%q", ansi.Strip(trimmed))
	}
}

func TestDetailScrolls(t *testing.T) {
	t.Parallel()

	st := styles(t, "gruvbox-dark")

	p := detail.New(decodesTo(untyped()))
	p.SetEvent(model.Event{Tag: "salt/a", Payload: somePayload})

	before := p.View(80, 4, ui.Snapshot{}, st)

	p.Update(tea.KeyMsg{Type: tea.KeyDown}, ui.Snapshot{})

	after := p.View(80, 4, ui.Snapshot{}, st)

	if before == after {
		t.Errorf("scrolling down changed nothing:\n%s", ansi.Strip(before))
	}

	p.Update(tea.KeyMsg{Type: tea.KeyUp}, ui.Snapshot{})

	if back := p.View(80, 4, ui.Snapshot{}, st); back != before {
		t.Errorf("scrolling back up did not restore the view:\n%s\n---\n%s", ansi.Strip(before), ansi.Strip(back))
	}

	// Holding a key down must not bank thousands of invisible presses: after
	// running off the bottom, ONE press of up has to move the view.
	for range 200 {
		p.Update(tea.KeyMsg{Type: tea.KeyDown}, ui.Snapshot{})
	}

	bottom := p.View(80, 4, ui.Snapshot{}, st)

	p.Update(tea.KeyMsg{Type: tea.KeyUp}, ui.Snapshot{})

	if up := p.View(80, 4, ui.Snapshot{}, st); up == bottom {
		t.Error("one press of up after scrolling past the end did nothing; the offset was never clamped")
	}

	// A new event resets the scroll: keeping the old offset would open the
	// next payload part-way down for no reason the operator can see.
	p.SetEvent(model.Event{Tag: "salt/b", Payload: somePayload})

	if reset := p.View(80, 4, ui.Snapshot{}, st); !strings.Contains(ansi.Strip(reset), "salt/b") {
		t.Errorf("selecting a new event kept the old scroll offset:\n%s", ansi.Strip(reset))
	}
}

// TestDetailNeverPanicsOnHostileInput: payload content is minion-supplied, and
// this pane renders it OUTSIDE components' table, so it sanitises its own text.
func TestDetailNeverPanicsOnHostileInput(t *testing.T) {
	t.Parallel()

	deep := func() any {
		var v any = "bottom"
		for range 20000 {
			v = map[interface{}]interface{}{"k": v}
		}

		return v
	}

	cases := []struct {
		name   string
		decode detail.DecodeFunc
		ev     model.Event
		w, h   int
	}{
		{"escape sequence in a value", decodesTo(map[interface{}]interface{}{
			"out": "\x1b]0;pwned\x07\x1b[2Jgone",
		}), model.Event{Tag: "salt/a", Payload: somePayload}, 80, 20},
		{"escape sequence in a key", decodesTo(map[interface{}]interface{}{
			"\x1b[31mkey": "v",
		}), model.Event{Tag: "salt/a", Payload: somePayload}, 80, 20},
		{"escape sequence in the tag", decodesTo(untyped()),
			model.Event{Tag: "salt/\x1b]0;pwned\x07", Payload: somePayload}, 80, 20},
		{"pathologically deep nesting", decodesTo(deep()),
			model.Event{Tag: "salt/a", Payload: somePayload}, 80, 20},
		{"non-string map keys", decodesTo(map[interface{}]interface{}{int8(1): "a", true: "b"}),
			model.Event{Tag: "salt/a", Payload: somePayload}, 80, 20},
		{"binary value", decodesTo(map[interface{}]interface{}{"b": []byte{0x00, 0xff}}),
			model.Event{Tag: "salt/a", Payload: somePayload}, 80, 20},
		{"decode failure", func([]byte) (any, error) { return nil, errors.New("bad msgpack") },
			model.Event{Tag: "salt/a", Payload: somePayload}, 80, 20},
		{"one by one", decodesTo(untyped()), model.Event{Tag: "salt/a", Payload: somePayload}, 1, 1},
		{"zero box", decodesTo(untyped()), model.Event{Tag: "salt/a", Payload: somePayload}, 0, 0},
		{"negative box", decodesTo(untyped()), model.Event{Tag: "salt/a", Payload: somePayload}, -4, -4},
		{"no payload", decodesTo(untyped()), model.Event{Tag: "salt/a"}, 80, 20},
		{"stamped", decodesTo(untyped()), model.Event{
			Tag: "salt/a", Payload: somePayload, Stamp: time.Now(), Arrival: time.Now(),
		}, 80, 20},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			p := detail.New(tc.decode)
			p.SetEvent(tc.ev)

			got := p.View(tc.w, tc.h, ui.Snapshot{}, styles(t, "gruvbox-dark"))

			if strings.Contains(ansi.Strip(got), "\x1b") || strings.Contains(got, "\x07") {
				t.Errorf("an escape sequence from the bus survived into the output: %q", got)
			}

			lines := strings.Split(got, "\n")
			if got == "" {
				lines = nil
			}

			if tc.h >= 0 && len(lines) > tc.h {
				t.Errorf("rendered %d lines into a box %d tall", len(lines), tc.h)
			}

			for i, l := range lines {
				if width := lipgloss.Width(l); tc.w >= 0 && width > tc.w {
					t.Errorf("line %d width = %d, exceeds the %d-cell box", i, width, tc.w)
				}
			}
		})
	}
}

func TestDetailWithNoEventSaysSoAndDrawsNoBorder(t *testing.T) {
	t.Parallel()

	got := detail.New(decodesTo(untyped())).View(60, 10, ui.Snapshot{}, styles(t, "gruvbox-dark"))

	if !strings.Contains(ansi.Strip(got), "no event selected") {
		t.Errorf("an empty Detail pane does not say why it is empty:\n%s", got)
	}

	for _, glyph := range []string{"╭", "╰", "│", "─"} {
		if strings.Contains(got, glyph) {
			t.Errorf("pane drew a border glyph %q; the root owns the frame", glyph)
		}
	}
}

func TestDetailKeysAdvertiseWhatItBinds(t *testing.T) {
	t.Parallel()

	listed := map[string]bool{}
	for _, k := range detail.New(decodesTo(untyped())).Keys() {
		listed[k.Key] = true

		if k.Label == "" {
			t.Errorf("key %q has no label", k.Key)
		}
	}

	for _, want := range []string{"↑/↓", "g", "G"} {
		if !listed[want] {
			t.Errorf("Keys() does not advertise %q", want)
		}
	}
}

// TestDetailStylesItsOwnOutput is the pane-level theme guard.
//
// The root-level equivalent passes even with every pane's styling gutted,
// because the root's own chrome changes colour and the frames still differ.
// Here there is no chrome: if this pane stops using the *theme.Styles it is
// handed, the two frames become byte-identical and this fails.
//
// Verified by mutation: replacing every st.X.Render(...) in View and its
// helpers with the bare string makes this test FAIL while the rest of the
// package still passes.
func TestDetailStylesItsOwnOutput(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		ev   model.Event
	}{
		{"payload", model.Event{Tag: "salt/a", Payload: somePayload}},
		{"shed", model.Event{Tag: "salt/a", Shed: true}},
		{"master trimmed", model.Event{Tag: "salt/a", MasterTrimmed: true, Payload: somePayload}},
		{"no event at all", model.Event{}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			render := func(palette string) string {
				p := detail.New(decodesTo(untyped()))
				if tc.ev.Tag != "" {
					p.SetEvent(tc.ev)
				}

				return p.View(80, 30, ui.Snapshot{}, styles(t, palette))
			}

			gruvbox, solarized := render("gruvbox-dark"), render("solarized-dark")

			if gruvbox == solarized {
				t.Errorf("two palettes rendered identical output; the pane styles nothing:\n%s", gruvbox)
			}

			if ansi.Strip(gruvbox) != ansi.Strip(solarized) {
				t.Errorf("the theme changed LAYOUT:\n%s\n---\n%s", ansi.Strip(gruvbox), ansi.Strip(solarized))
			}

			if mono := render("mono"); ansi.Strip(mono) != ansi.Strip(gruvbox) {
				t.Errorf("mono changed layout:\n%s\n---\n%s", ansi.Strip(mono), ansi.Strip(gruvbox))
			}
		})
	}
}
