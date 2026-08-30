package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/vmihailenco/msgpack/v5"

	"github.com/TKC-Labs/go-salt-events/internal/config"
	"github.com/TKC-Labs/go-salt-events/internal/filter"
	"github.com/TKC-Labs/go-salt-events/internal/saltipc"
	"github.com/TKC-Labs/go-salt-events/internal/ui"
)

// frame builds one wire frame exactly as salt/transport/frame.py::frame_msg_ipc
// does: a 4-byte big-endian length, then msgpack{head, body}, where body is
// tag + "\n\n" + msgpack(data) (spec §2.1).
//
// It is a copy of internal/saltipc's own test helper, and deliberately so: this
// test exists to prove the WIRING decodes what the socket really carries, so it
// must build its bytes independently of the package under test rather than
// borrow a helper that could drift with it.
func frame(t *testing.T, tag string, data map[string]any) []byte {
	t.Helper()

	inner, err := msgpack.Marshal(data)
	if err != nil {
		t.Fatalf("marshal data: %v", err)
	}

	body := append([]byte(tag+"\n\n"), inner...)

	outer, err := msgpack.Marshal(map[string]any{"head": map[string]any{}, "body": body})
	if err != nil {
		t.Fatalf("marshal frame: %v", err)
	}

	buf := binary.BigEndian.AppendUint32(make([]byte, 0, frameHeader+len(outer)), uint32(len(outer)))

	return append(buf, outer...)
}

// serveFrames listens on a socket named exactly as Salt's publish socket is,
// inside a fresh directory, and writes payload to every client that connects.
//
// The name matters: the reader re-checks the resolved BASENAME before every
// dial, so a socket called anything else is refused outright (invariant 1).
func serveFrames(t *testing.T, payload []byte) string {
	t.Helper()

	dir := t.TempDir()

	ln, err := net.Listen("unix", filepath.Join(dir, config.PubSocketName))
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}

			_, _ = conn.Write(payload)

			// Held open: closing here would look like a master restart and put
			// the reader into its reconnect backoff mid-test.
			t.Cleanup(func() { _ = conn.Close() })
		}
	}()

	return dir
}

// TestTheWholeWiringCarriesAnEventFromTheSocketToTheScreen is the one test that
// exercises every seam this task owns at once: socket → reader → hub → snapshot
// → root → pane. Each layer is tested in isolation elsewhere; nothing else
// notices when they are connected wrongly.
func TestTheWholeWiringCarriesAnEventFromTheSocketToTheScreen(t *testing.T) {
	t.Parallel()

	wire := frame(t, "salt/job/"+testJID+"/ret/web-1", map[string]any{
		"jid": testJID, "id": "web-1", "fun": "state.apply",
		"retcode": 0, "success": true, "return": "it worked",
	})

	dir := serveFrames(t, append(append([]byte{}, wire...), frame(t, "salt/auth", map[string]any{
		"id": "web-1", "act": "accept",
	})...))

	h, _ := newTestHub(t, 1<<20, 100)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The reader is given the DIRECTORY: the filename is not the caller's to
	// give, which is how invariant 1 is enforced by construction.
	reader := saltipc.NewReader(dir, time.Now)

	if got := reader.SocketPath(); filepath.Base(got) != config.PubSocketName {
		t.Fatalf("the reader resolved %q, which is not the publish socket", got)
	}

	done := make(chan error, 1)
	go func() { done <- reader.Run(ctx, h) }()

	waitFor(t, func() bool { return h.Snapshot(filter.Query{}, 10).Cache.Events == 2 })

	m := ui.NewModel(h, panesFor(), ui.Options{Interval: time.Hour})
	m = update(t, m, tea.WindowSizeMsg{Width: 140, Height: 30})
	m = update(t, m, ui.TickMsg(start))

	view := m.View()

	if !strings.Contains(view, "salt/auth") {
		t.Errorf("the newest event never reached the screen:\n%s", view)
	}

	if !strings.Contains(view, "connected") {
		t.Errorf("the status bar does not report the connection:\n%s", view)
	}

	// The job correlated on the way through, from the tag and the payload's
	// indexed fields alone.
	if job, _ := h.Snapshot(filter.Query{}, 10).JobLookup(testJID); job == nil || job.Returned() != 1 {
		t.Errorf("the ret event did not correlate into the job index: %+v", job)
	}

	cancel()

	if err := <-done; err != nil {
		t.Errorf("reader.Run returned %v, want nil on cancellation", err)
	}
}

// serveQuietly listens on a socket named exactly as Salt's publish socket is,
// accepts, and then says nothing at all — a healthy master with nothing to
// report. It reports each accept on the returned channel so a test can wait
// for the connection rather than for an event that is never coming.
//
// closeAfterFirst makes it drop the first connection, which is what a master
// restart looks like from here.
func serveQuietly(t *testing.T, closeAfterFirst bool) (string, <-chan struct{}) {
	t.Helper()

	dir := t.TempDir()

	ln, err := net.Listen("unix", filepath.Join(dir, config.PubSocketName))
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	t.Cleanup(func() { _ = ln.Close() })

	accepted := make(chan struct{}, 8)

	go func() {
		for i := 0; ; i++ {
			conn, err := ln.Accept()
			if err != nil {
				return
			}

			accepted <- struct{}{}

			if i == 0 && closeAfterFirst {
				_ = conn.Close()

				continue
			}

			t.Cleanup(func() { _ = conn.Close() })
		}
	}()

	return dir, accepted
}

// TestAQuietMasterStillReadsConnected pins where connectedness comes from.
//
// It was derived from event ARRIVAL — set by hub.Event, cleared by hub.Gap —
// so a socket that is open and healthy read DISCONNECTED in capitals until the
// first event landed, which on a quiet master is minutes. Spec §8.2's concern
// is that an outage must not render as a quiet master; this is the exact
// inverse, and it is worse during an incident because the operator is told the
// tool is broken when it is working.
func TestAQuietMasterStillReadsConnected(t *testing.T) {
	t.Parallel()

	dir, accepted := serveQuietly(t, false)

	h, _ := newTestHub(t, 1<<20, 100)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- saltipc.NewReader(dir, time.Now).Run(ctx, h) }()

	select {
	case <-accepted:
	case <-time.After(5 * time.Second):
		t.Fatal("the reader never connected")
	}

	waitFor(t, func() bool { return h.Snapshot(filter.Query{}, 10).Connected })

	if got := h.Snapshot(filter.Query{}, 10).Events; len(got) != 0 {
		t.Fatalf("the master sent %d events; this test only means something when it is silent",
			len(got))
	}

	cancel()
	<-done
}

// TestASuccessfulReconnectReadsConnected pins the inverted half.
//
// Reader.Run closes an outage window by calling sink.Gap(lostAt, now) on the
// reconnect path, so the one call that means "we are back" was the call that
// set connected = false. Immediately after a SUCCESSFUL reconnect the console
// therefore reported DISCONNECTED.
func TestASuccessfulReconnectReadsConnected(t *testing.T) {
	t.Parallel()

	dir, accepted := serveQuietly(t, true)

	h, _ := newTestHub(t, 1<<20, 100)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- saltipc.NewReader(dir, time.Now).Run(ctx, h) }()

	for i := range 2 {
		select {
		case <-accepted:
		case <-time.After(5 * time.Second):
			t.Fatalf("the reader never made connection %d", i+1)
		}
	}

	// The second connection is live and the outage is over. A gap was reported
	// while it ran, and must not still be reading as the current state.
	waitFor(t, func() bool { return h.Snapshot(filter.Query{}, 10).Connected })

	snap := h.Snapshot(filter.Query{}, 10)
	if len(snap.Events) != 0 {
		t.Fatalf("the master sent %d events; this test only means something when it is silent",
			len(snap.Events))
	}

	cancel()
	<-done
}

// waitFor polls until cond holds or the test gives up. A socket test cannot be
// driven by an injected clock — the kernel decides when the bytes land — so
// this is the one place anything here waits, and it fails loudly rather than
// hanging.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()

	const (
		limit = 5 * time.Second
		step  = time.Millisecond
	)

	deadline := time.Now().Add(limit)

	for time.Now().Before(deadline) {
		if cond() {
			return
		}

		time.Sleep(step)
	}

	t.Fatalf("condition still false after %s", limit)
}

// pullSocketName is the socket that could inject events onto the bus. It is
// named here exclusively so tests can prove it is never opened.
const pullSocketName = "master_event_pull.ipc"

func TestCaptureRecordsFramesVerbatim(t *testing.T) {
	t.Parallel()

	// Two frames, one of them larger than PIPE_BUF, because the length prefix
	// exists precisely so those do not interleave into garbage (spec §2.1).
	want := append(
		frame(t, "salt/auth", map[string]any{"id": "web-1"}),
		frame(t, "salt/job/"+testJID+"/ret/web-1", map[string]any{
			"jid": testJID, "id": "web-1", "return": strings.Repeat("x", 70000),
		})...)

	dir := serveFrames(t, want)
	out := filepath.Join(t.TempDir(), "frames.bin")

	if err := captureFrames(dir, out, 2); err != nil {
		t.Fatalf("captureFrames: %v", err)
	}

	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read capture: %v", err)
	}

	if !bytes.Equal(got, want) {
		t.Errorf("the capture is not byte-identical to the stream: got %d bytes, want %d",
			len(got), len(want))
	}
}

// TestCaptureRefusesASocketThatResolvesElsewhere is invariant 1 on the
// --capture path.
//
// Layer 1 (the basename is derived, not supplied) has always held here.
// Layer 2 — the re-check of the RESOLVED basename before every dial — did not:
// captureFrames dialled directly, so the one route layer 2 exists to close, a
// symlink named master_event_pub.ipc pointing at the pull socket, was open on
// this path. The tool is read-only either way, but invariant 1 is stated
// STRUCTURALLY: master_event_pull.ipc is never opened, and here it was.
//
// The pull socket really listens, so a bypass is observable rather than
// inferred.
func TestCaptureRefusesASocketThatResolvesElsewhere(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	pull, err := net.Listen("unix", filepath.Join(dir, pullSocketName))
	if err != nil {
		t.Fatalf("listen pull: %v", err)
	}

	t.Cleanup(func() { _ = pull.Close() })

	accepted := make(chan struct{}, 1)

	go func() {
		conn, err := pull.Accept()
		if err != nil {
			return
		}

		accepted <- struct{}{}

		// Closed at once so a capture that got through fails on EOF rather
		// than parking this test on a read that never returns.
		_ = conn.Close()
	}()

	if err := os.Symlink(filepath.Join(dir, pullSocketName),
		filepath.Join(dir, config.PubSocketName)); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	out := filepath.Join(t.TempDir(), "frames.bin")

	err = captureFrames(dir, out, 1)
	if err == nil {
		t.Fatal("captureFrames accepted a publish socket that resolves to the pull socket")
	}

	select {
	case <-accepted:
		t.Fatal("--capture connected to master_event_pull.ipc; invariant 1 is broken")
	default:
	}

	if !strings.Contains(err.Error(), pullSocketName) {
		t.Errorf("the refusal %q does not name what the socket resolved to", err)
	}

	if _, statErr := os.Stat(out); statErr == nil {
		t.Error("a refused capture still created its output file")
	}
}

func TestSplitCaptureArgs(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		args    []string
		frames  int
		out     string
		rest    []string
		wantErr bool
	}{
		{
			name: "no capture flags",
			args: []string{"--theme", "mono", "--max-jobs=5"},
			rest: []string{"--theme", "mono", "--max-jobs=5"},
		},
		{
			// The form `just capture` writes.
			name:   "equals form",
			args:   []string{"--capture=200", "--capture-out=frames.bin"},
			frames: 200,
			out:    "frames.bin",
		},
		{
			name:   "single dash and separate values",
			args:   []string{"-capture", "3", "-capture-out", "f.bin"},
			frames: 3,
			out:    "f.bin",
		},
		{
			name:   "capture flags are removed from what config sees",
			args:   []string{"--sock-dir", "/tmp/x", "--capture=1", "--capture-out=f.bin", "--no-color"},
			frames: 1,
			out:    "f.bin",
			rest:   []string{"--sock-dir", "/tmp/x", "--no-color"},
		},
		{
			// Recording to nowhere is a mistake worth naming: the alternative
			// is a program that connects, reads 200 frames and drops them.
			name:    "capture without a destination",
			args:    []string{"--capture=10"},
			wantErr: true,
		},
		{
			name:    "non-numeric count",
			args:    []string{"--capture=many", "--capture-out=f.bin"},
			wantErr: true,
		},
		{
			name:    "missing value at the end",
			args:    []string{"--capture"},
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			opts, rest, err := splitCaptureArgs(tc.args)

			if tc.wantErr {
				if err == nil {
					t.Fatalf("splitCaptureArgs(%v) = %+v, want an error", tc.args, opts)
				}

				if !errors.Is(err, errCapture) {
					t.Errorf("error %v does not identify itself as a capture error", err)
				}

				return
			}

			if err != nil {
				t.Fatalf("splitCaptureArgs(%v): %v", tc.args, err)
			}

			if opts.frames != tc.frames || opts.out != tc.out {
				t.Errorf("options = %+v, want frames=%d out=%q", opts, tc.frames, tc.out)
			}

			if !reflect.DeepEqual(rest, tc.rest) {
				t.Errorf("remaining args = %v, want %v", rest, tc.rest)
			}
		})
	}
}

// fakeSender records what serve would have handed the running TUI.
type fakeSender struct {
	msgs []tea.Msg
}

func (f *fakeSender) Send(msg tea.Msg) { f.msgs = append(f.msgs, msg) }

// TestADeadReaderReachesTheTUIAsAnInstruction is the wiring half of spec §8.1.
//
// serve read the reader's error only AFTER program.Run() returned, and then
// wrapped it as a bare errno rather than passing it through saltipc.Diagnose.
// So the console ran indefinitely showing nothing but DISCONNECTED, and the
// single most common first-run failure — forgetting sudo — printed
// "connect: permission denied" once the operator gave up, when Diagnose
// already produces the exact instruction they needed.
func TestADeadReaderReachesTheTUIAsAnInstruction(t *testing.T) {
	t.Parallel()

	const sockPath = "/run/salt/master/master_event_pub.ipc"

	cases := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "forgot sudo",
			err:  fmt.Errorf("connect to %s: %w", sockPath, fs.ErrPermission),
			want: "sudo salt-events",
		},
		{
			name: "master is not running",
			err:  fmt.Errorf("connect to %s: %w", sockPath, fs.ErrNotExist),
			want: "systemctl status salt-master",
		},
		{
			name: "not the publish socket",
			err:  fmt.Errorf("%w: resolves elsewhere", saltipc.ErrNotPubSocket),
			want: "structurally incapable of injecting events",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var sender fakeSender

			reportReaderFailure(&sender, sockPath, tc.err)

			if len(sender.msgs) != 1 {
				t.Fatalf("the TUI was sent %d messages, want exactly 1", len(sender.msgs))
			}

			got, ok := sender.msgs[0].(ui.ReaderErrorMsg)
			if !ok {
				t.Fatalf("the TUI was sent %T, want ui.ReaderErrorMsg", sender.msgs[0])
			}

			if !strings.Contains(string(got), tc.want) {
				t.Errorf("the diagnosis does not carry %q:\n%s", tc.want, got)
			}

			// And it must survive the trip through the real root model onto a
			// real frame — the point is that the operator SEES it.
			m := ui.NewModel(nil, panesFor(), ui.Options{Interval: time.Hour})

			sized, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
			shown, _ := sized.(ui.Model).Update(got)

			view, viewOK := shown.(ui.Model)
			if !viewOK {
				t.Fatal("Update did not return a ui.Model")
			}

			if !strings.Contains(ansi.Strip(view.View()), tc.want) {
				t.Errorf("the running console does not show %q:\n%s", tc.want, view.View())
			}
		})
	}
}

// TestACleanReaderShutdownSaysNothing: quitting is not a failure, and a
// diagnosis printed over an ordinary exit teaches the operator to ignore them.
func TestACleanReaderShutdownSaysNothing(t *testing.T) {
	t.Parallel()

	var sender fakeSender

	reportReaderFailure(&sender, "/run/salt/master/master_event_pub.ipc", nil)

	if len(sender.msgs) != 0 {
		t.Errorf("a clean shutdown sent %v to the TUI", sender.msgs)
	}
}

// TestExportWritesTheFilteredSet checks the seam `w` runs across: the hub's
// copy of the filtered events, and internal/export's refusal-first contract.
func TestExportWritesTheFilteredSet(t *testing.T) {
	t.Parallel()

	h, _ := newTestHub(t, 1<<20, 100)
	fake := saltipc.NewFake(start)

	feedData(t, fake, h, "salt/auth", map[string]any{"id": "web-1"})
	feedData(t, fake, h, "salt/job/"+testJID+"/ret/web-1", map[string]any{
		"jid": testJID, "id": "web-1", "return": "ok",
	})

	dir := t.TempDir()

	cfg := config.Config{ExportDir: dir, ExportMax: 1 << 30}

	note, err := exporter(cfg, h)(mustParse(t, "salt/auth"))
	if err != nil {
		t.Fatalf("export: %v", err)
	}

	if !strings.Contains(note, "wrote 1 events") {
		t.Errorf("export reported %q, want the filtered count of 1", note)
	}

	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("export directory holds %d files (%v)", len(entries), err)
	}

	if strings.HasSuffix(entries[0].Name(), ".partial") {
		t.Errorf("the export left a partial file: %s", entries[0].Name())
	}

	// The payload must round-trip. saltipc.DecodeValue returns
	// map[interface{}]interface{}, which encoding/json refuses outright, so an
	// exporter handed the raw decoder writes nothing at all — and a count-only
	// assertion would not notice a payload that came out null.
	line, err := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	if err != nil {
		t.Fatalf("read export: %v", err)
	}

	var record struct {
		Tag     string         `json:"tag"`
		Payload map[string]any `json:"payload"`
	}

	if err := json.Unmarshal(bytes.TrimSpace(line), &record); err != nil {
		t.Fatalf("the export is not valid NDJSON: %v\n%s", err, line)
	}

	if record.Tag != "salt/auth" || record.Payload["id"] != "web-1" {
		t.Errorf("exported record = %+v, want the salt/auth event with its payload", record)
	}
}

// TestExportSurvivesARealPayloadJSONRefuses drives the whole `w` seam with the
// two shapes that have each taken an export down: a nested
// map[interface{}]interface{} (what DecodeValue returns for every real event)
// and a non-finite float (which msgpack carries and encoding/json refuses).
//
// The wiring hands internal/export the RAW decoder now; the JSON-safety pass
// belongs to the package that requires it, so this test is what proves the
// seam still holds after the conversion moved down a layer.
func TestExportSurvivesARealPayloadJSONRefuses(t *testing.T) {
	t.Parallel()

	h, _ := newTestHub(t, 1<<20, 100)
	fake := saltipc.NewFake(start)

	feedData(t, fake, h, "salt/job/"+testJID+"/ret/web-1", map[string]any{
		"jid": testJID, "id": "web-1",
		"return": map[string]any{
			"pkg_|-install_|-nginx_|-installed": map[string]any{
				"result":  true,
				"changes": map[string]any{"nginx": "1.24"},
			},
			"load": math.NaN(),
		},
	})

	dir := t.TempDir()

	if _, err := exporter(config.Config{ExportDir: dir, ExportMax: 1 << 30}, h)(filter.Query{}); err != nil {
		t.Fatalf("export of a real payload: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("export directory holds %d files (%v)", len(entries), err)
	}

	line, err := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	if err != nil {
		t.Fatalf("read export: %v", err)
	}

	var record struct {
		Payload map[string]any `json:"payload"`
	}

	if err := json.Unmarshal(bytes.TrimSpace(line), &record); err != nil {
		t.Fatalf("the export is not valid NDJSON: %v\n%s", err, line)
	}

	ret, ok := record.Payload["return"].(map[string]any)
	if !ok {
		t.Fatalf("the nested return did not round-trip: %#v", record.Payload)
	}

	if ret["load"] != "NaN" {
		t.Errorf("payload return.load = %#v, want the string \"NaN\"", ret["load"])
	}

	state, ok := ret["pkg_|-install_|-nginx_|-installed"].(map[string]any)
	if !ok {
		t.Fatalf("the non-identifier state key did not round-trip: %#v", ret)
	}

	changes, ok := state["changes"].(map[string]any)
	if !ok || changes["nginx"] != "1.24" {
		t.Errorf("the doubly-nested map did not round-trip: %#v", state)
	}
}

// TestExportRefusesOverTheCap: invariant 8's cap is the backstop that bounds
// the write even when the estimate is wrong, and a refusal must be reported to
// the operator rather than swallowed.
func TestExportRefusesOverTheCap(t *testing.T) {
	t.Parallel()

	h, _ := newTestHub(t, 1<<20, 100)
	fake := saltipc.NewFake(start)

	feedData(t, fake, h, "salt/auth", map[string]any{"id": "web-1"})

	dir := t.TempDir()

	_, err := exporter(config.Config{ExportDir: dir, ExportMax: 1}, h)(filter.Query{})
	if err == nil {
		t.Fatal("export accepted a write over --export-max")
	}

	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Errorf("a refused export still wrote %d files", len(entries))
	}
}

// mustParse compiles a filter query for a test.
func mustParse(t *testing.T, s string) filter.Query {
	t.Helper()

	q, err := filter.Parse(s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}

	return q
}
