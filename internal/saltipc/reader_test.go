package saltipc_test

import (
	"context"
	"errors"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TKC-Labs/go-salt-events/internal/model"
	"github.com/TKC-Labs/go-salt-events/internal/saltipc"
	"github.com/TKC-Labs/go-salt-events/internal/stats"
)

// pubSocket is the only basename this program may ever open (invariant 1).
const pubSocket = "master_event_pub.ipc"

// pullSocket is the socket that could inject events onto the bus. It exists in
// this file exclusively so tests can prove it is never touched.
const pullSocket = "master_event_pull.ipc"

// recordSink collects everything the reader emits and signals once a wanted
// number of events has arrived, so tests can stop the reader deterministically
// instead of sleeping.
// gapWindow is one Gap call. Both ends are kept because one outage now
// produces many calls that share a from, so a test can tell "one outage
// reported repeatedly" from "several outages".
type gapWindow struct {
	from, to time.Time
}

type recordSink struct {
	want int

	mu     sync.Mutex
	events []model.Event
	gaps   []gapWindow
	errs   int
	attach []bool

	reachedOnce sync.Once
	reached     chan struct{}
}

func newRecordSink(want int) *recordSink {
	return &recordSink{want: want, reached: make(chan struct{})}
}

func (s *recordSink) Event(e model.Event) {
	s.mu.Lock()
	s.events = append(s.events, e)
	n := len(s.events)
	s.mu.Unlock()

	if n >= s.want {
		s.reachedOnce.Do(func() { close(s.reached) })
	}
}

func (s *recordSink) Gap(from, to time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.gaps = append(s.gaps, gapWindow{from: from, to: to})
}

func (s *recordSink) DecodeError(error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.errs++
}

// Attached records every level change the reader reports, in order, so a test
// can assert on the SEQUENCE rather than only on the final value — "connected,
// then not, then connected again" is a different story from "still connected",
// and only the sequence tells them apart.
func (s *recordSink) Attached(attached bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.attach = append(s.attach, attached)
}

// attaches returns the recorded Attached sequence.
func (s *recordSink) attaches() []bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]bool, len(s.attach))
	copy(out, s.attach)

	return out
}

func (s *recordSink) snapshot() ([]model.Event, []gapWindow, int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]model.Event, len(s.events))
	copy(out, s.events)

	gaps := make([]gapWindow, len(s.gaps))
	copy(gaps, s.gaps)

	return out, gaps, s.errs
}

// outages counts distinct gap windows by their start, which is how a sink that
// wants "how many times did we lose the bus" gets it now that one outage is
// reported repeatedly while it runs.
func outages(gaps []gapWindow) int {
	seen := make(map[time.Time]struct{}, len(gaps))
	for _, g := range gaps {
		seen[g.from] = struct{}{}
	}

	return len(seen)
}

// busServer stands in for salt-master's IPCMessagePublisher.
//
// It records every byte the client sends it, because invariant 1's read-only
// guarantee is only meaningful if something actually checks that the master
// receives nothing.
type busServer struct {
	dir string
	ln  net.Listener

	handled chan struct{}

	mu         sync.Mutex
	fromClient []byte
	accepted   int
}

// newBusServer listens on <t.TempDir()>/name.
func newBusServer(t *testing.T, name string) *busServer {
	t.Helper()

	dir := t.TempDir()

	ln, err := net.Listen("unix", filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	t.Cleanup(func() { _ = ln.Close() })

	return &busServer{dir: dir, ln: ln, handled: make(chan struct{}, 8)}
}

// serve accepts at most maxConns connections, writing raw to each, then stops
// listening. Closing the listener at the end is what makes reconnect tests
// deterministic: a listener left open accepts into the kernel backlog, so the
// reader would keep "reconnecting" to a socket nobody is serving.
//
// When holdRead is non-zero the connection is held open reading from the client
// first, so the test can assert the client sent nothing.
func (s *busServer) serve(raw []byte, maxConns int, holdRead time.Duration) {
	go func() {
		defer func() { _ = s.ln.Close() }()

		for range maxConns {
			conn, err := s.ln.Accept()
			if err != nil {
				return
			}

			s.mu.Lock()
			s.accepted++
			s.mu.Unlock()

			s.handle(conn, raw, holdRead)
		}
	}()
}

func (s *busServer) handle(conn net.Conn, raw []byte, holdRead time.Duration) {
	defer func() {
		_ = conn.Close()
		s.handled <- struct{}{}
	}()

	_, _ = conn.Write(raw)

	if holdRead == 0 {
		return
	}

	_ = conn.SetReadDeadline(time.Now().Add(holdRead))

	buf := make([]byte, 512)

	n, _ := conn.Read(buf)
	if n > 0 {
		s.mu.Lock()
		s.fromClient = append(s.fromClient, buf[:n]...)
		s.mu.Unlock()
	}
}

func (s *busServer) received() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]byte, len(s.fromClient))
	copy(out, s.fromClient)

	return out
}

func (s *busServer) acceptCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.accepted
}

// runUntil starts the reader, waits for sink to reach its wanted event count,
// then cancels and waits for Run to return. It fails the test rather than
// hanging if the events never arrive.
func runUntil(t *testing.T, r *saltipc.Reader, sink *recordSink) error {
	t.Helper()

	const budget = 5 * time.Second

	ctx, cancel := context.WithTimeout(t.Context(), budget)
	defer cancel()

	done := make(chan error, 1)

	go func() { done <- r.Run(ctx, sink) }()

	select {
	case <-sink.reached:
	case err := <-done:
		return err
	case <-ctx.Done():
		t.Fatalf("timed out waiting for %d events", sink.want)
	}

	cancel()

	return <-done
}

func TestReaderEmitsFullyPopulatedEvents(t *testing.T) {
	t.Parallel()

	raw := frame(t, "salt/job/20260830081402123456/ret/scache-1", map[string]any{
		"id":      "scache-1",
		"jid":     "20260830081402123456",
		"fun":     "state.apply",
		"retcode": 0,
		"success": true,
		"return":  map[string]any{"ok": true},
		"_stamp":  "2026-08-30T08:14:02.123456",
	})

	srv := newBusServer(t, pubSocket)
	srv.serve(raw, 1, 0)

	sink := newRecordSink(1)

	if err := runUntil(t, saltipc.NewReader(srv.dir, time.Now), sink); err != nil {
		t.Fatalf("Run: %v", err)
	}

	events, _, _ := sink.snapshot()
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}

	e := events[0]

	if e.Minion != "scache-1" {
		t.Errorf("Minion = %q", e.Minion)
	}

	if e.Category != "salt/job/*/ret/*" {
		t.Errorf("Category = %q", e.Category)
	}

	if e.Namespace != "job" {
		t.Errorf("Namespace = %q", e.Namespace)
	}

	if e.Kind != model.KindRet {
		t.Errorf("Kind = %v", e.Kind)
	}

	if e.Fun != "state.apply" {
		t.Errorf("Fun = %q", e.Fun)
	}

	if e.JID != "20260830081402123456" {
		t.Errorf("JID = %q", e.JID)
	}

	if !e.HasRet {
		t.Error("HasRet = false; the payload carried a return")
	}

	if e.Arrival.IsZero() {
		t.Error("Arrival is zero; every event must carry an arrival time")
	}

	// Invariant 2: bucketing uses arrival, so Stamp must never overwrite it.
	if e.Arrival.Equal(e.Stamp) {
		t.Error("Arrival equals Stamp; arrival must be taken by the reader, not read off the bus")
	}

	if e.Stamp.IsZero() {
		t.Error("Stamp is zero; _stamp was present and parseable")
	}

	if len(e.Payload) == 0 {
		t.Error("Payload is empty; ingest must retain raw msgpack")
	}
}

func TestReaderPrefersPayloadIDOverTagPositionForMinion(t *testing.T) {
	t.Parallel()

	// Not every minion-bearing tag puts the id in the same position, and the
	// payload's id field is present on essentially every minion-sourced
	// event (spec §4.4).
	raw := frame(t, "salt/minion/wrong-in-tag/start", map[string]any{"id": "right-in-payload"})

	srv := newBusServer(t, pubSocket)
	srv.serve(raw, 1, 0)

	sink := newRecordSink(1)

	if err := runUntil(t, saltipc.NewReader(srv.dir, time.Now), sink); err != nil {
		t.Fatalf("Run: %v", err)
	}

	events, _, _ := sink.snapshot()
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}

	if events[0].Minion != "right-in-payload" {
		t.Errorf("Minion = %q, want right-in-payload", events[0].Minion)
	}
}

func TestReaderFlagsMasterTrimmedEvents(t *testing.T) {
	t.Parallel()

	raw := frame(t, "salt/job/20260830081402123456/ret/web-1",
		map[string]any{"return": saltipc.TrimmedMarker})

	srv := newBusServer(t, pubSocket)
	srv.serve(raw, 1, 0)

	sink := newRecordSink(1)

	if err := runUntil(t, saltipc.NewReader(srv.dir, time.Now), sink); err != nil {
		t.Fatalf("Run: %v", err)
	}

	events, _, _ := sink.snapshot()
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}

	// Spec §5.3 case A: this is data Salt destroyed at the master. The fix is
	// max_event_size, not --max-memory, and the UI can only say so if the
	// reader records which one happened.
	if !events[0].MasterTrimmed {
		t.Error("MasterTrimmed = false, want true")
	}

	if events[0].Shed {
		t.Error("Shed = true; the cache has not touched this event yet")
	}
}

func TestReaderCountsDecodeErrorsAndKeepsGoing(t *testing.T) {
	t.Parallel()

	// A frame whose length prefix is honest but whose body is msgpack's
	// never-used 0xc1 byte: FrameReader consumes exactly the 8 bytes it was
	// promised and then fails to decode them.
	const badBodyLen = 8

	bad := make([]byte, 4+badBodyLen)
	bad[3] = badBodyLen

	for i := 4; i < len(bad); i++ {
		bad[i] = 0xc1
	}

	raw := make([]byte, 0, len(bad))
	raw = append(raw, bad...)
	raw = append(raw, frame(t, "salt/auth", map[string]any{"act": "accept"})...)

	srv := newBusServer(t, pubSocket)
	srv.serve(raw, 1, 0)

	sink := newRecordSink(1)

	if err := runUntil(t, saltipc.NewReader(srv.dir, time.Now), sink); err != nil {
		t.Fatalf("Run: %v", err)
	}

	events, _, errs := sink.snapshot()

	if errs != 1 {
		t.Errorf("DecodeError count = %d, want 1", errs)
	}

	if len(events) != 1 {
		t.Fatalf("got %d events, want 1 — a bad frame must not stop the reader", len(events))
	}

	if events[0].Kind != model.KindAuth {
		t.Errorf("Kind = %v, want auth", events[0].Kind)
	}
}

// TestReaderOnlyEverOpensThePublishSocket is invariant 1. The pull socket is
// the one that could inject events onto the bus; it is listening right next to
// the publish socket here, and must never be accepted on.
func TestReaderOnlyEverOpensThePublishSocket(t *testing.T) {
	t.Parallel()

	srv := newBusServer(t, pubSocket)
	srv.serve(frame(t, "salt/auth", map[string]any{"act": "accept"}), 1, 0)

	pull, err := net.Listen("unix", filepath.Join(srv.dir, pullSocket))
	if err != nil {
		t.Fatalf("listen pull: %v", err)
	}

	t.Cleanup(func() { _ = pull.Close() })

	pullAccepted := make(chan struct{}, 1)

	go func() {
		if _, err := pull.Accept(); err == nil {
			pullAccepted <- struct{}{}
		}
	}()

	sink := newRecordSink(1)

	if err := runUntil(t, saltipc.NewReader(srv.dir, time.Now), sink); err != nil {
		t.Fatalf("Run: %v", err)
	}

	select {
	case <-pullAccepted:
		t.Fatal("the reader connected to master_event_pull.ipc; invariant 1 is broken")
	default:
	}

	if srv.acceptCount() != 1 {
		t.Errorf("publish socket accepted %d connections, want 1", srv.acceptCount())
	}
}

// TestReaderSocketPathIgnoresCallerSuppliedBasenames pins the structural half
// of invariant 1: the caller supplies a directory, never a filename, so no
// input can steer the reader at master_event_pull.ipc.
func TestReaderSocketPathIgnoresCallerSuppliedBasenames(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		sockDir string
	}{
		{name: "plain directory", sockDir: "/var/run/salt/master"},
		{name: "trailing slash", sockDir: "/var/run/salt/master/"},
		{name: "pull socket as a directory", sockDir: "/var/run/salt/master/master_event_pull.ipc"},
		{name: "dot-dot escape attempt", sockDir: "/var/run/salt/master/../master"},
		{name: "empty", sockDir: ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := saltipc.NewReader(tc.sockDir, time.Now).SocketPath()

			if filepath.Base(got) != pubSocket {
				t.Errorf("SocketPath() = %q, basename must always be %q", got, pubSocket)
			}

			if strings.Contains(got, pullSocket+string(filepath.Separator)) {
				return // still fine: the pull name is a directory component, not the socket
			}

			if strings.HasSuffix(got, pullSocket) {
				t.Errorf("SocketPath() = %q resolves to the pull socket", got)
			}
		})
	}
}

// TestReaderNeverWritesToTheBus is the userspace half of invariant 1.
//
// It cannot be enforced by half-closing the write side: shutdown(SHUT_WR) on
// the publish socket makes Salt's tornado IPCMessagePublisher read EOF and drop
// the subscriber instantly, which silently kills ingest. So the guarantee is
// "we never call Write", and this is what checks it.
func TestReaderNeverWritesToTheBus(t *testing.T) {
	t.Parallel()

	const hold = 2 * time.Second

	srv := newBusServer(t, pubSocket)
	srv.serve(frame(t, "salt/auth", map[string]any{"act": "accept"}), 1, hold)

	sink := newRecordSink(1)

	if err := runUntil(t, saltipc.NewReader(srv.dir, time.Now), sink); err != nil {
		t.Fatalf("Run: %v", err)
	}

	select {
	case <-srv.handled:
	case <-time.After(hold + time.Second):
		t.Fatal("server handler never finished")
	}

	if got := srv.received(); len(got) != 0 {
		t.Errorf("the master received %d bytes from us (%q); the tool must never write to the bus",
			len(got), got)
	}
}

// TestReaderAcceptsASymlinkedSockDir pins a finding from a live master:
// /var/run is a symlink to /run, so any guard that resolves the socket path and
// then demands it still starts with /var/run/salt/master rejects the real
// socket. A guard that refuses the genuine article is worse than no guard.
func TestReaderAcceptsASymlinkedSockDir(t *testing.T) {
	t.Parallel()

	srv := newBusServer(t, pubSocket)
	srv.serve(frame(t, "salt/auth", map[string]any{"act": "accept"}), 1, 0)

	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(srv.dir, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	sink := newRecordSink(1)

	if err := runUntil(t, saltipc.NewReader(link, time.Now), sink); err != nil {
		t.Fatalf("Run through a symlinked sock_dir: %v", err)
	}

	if events, _, _ := sink.snapshot(); len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
}

// TestReaderRefusesASocketThatResolvesElsewhere covers the one route left to
// the pull socket once the basename is fixed: a symlink named
// master_event_pub.ipc pointing at it.
func TestReaderRefusesASocketThatResolvesElsewhere(t *testing.T) {
	t.Parallel()

	srv := newBusServer(t, pullSocket)
	srv.serve(frame(t, "salt/auth", map[string]any{"act": "accept"}), 1, 0)

	if err := os.Symlink(filepath.Join(srv.dir, pullSocket),
		filepath.Join(srv.dir, pubSocket)); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()

	err := saltipc.NewReader(srv.dir, time.Now).Run(ctx, newRecordSink(1))
	if err == nil {
		t.Fatal("Run returned nil; a publish socket that resolves to the pull socket must be refused")
	}

	if !strings.Contains(err.Error(), pullSocket) {
		t.Errorf("error %q does not name what it resolved to", err)
	}

	if srv.acceptCount() != 0 {
		t.Error("the pull socket was connected to; invariant 1 is broken")
	}
}

// TestReaderReportsWhetherItHoldsTheSocket is where connectedness comes from.
//
// There was no such signal, so the hub inferred it from event arrival — which
// makes a healthy but quiet master read DISCONNECTED, and makes the Gap that
// CLOSES an outage window read as a fresh one. The sequence matters more than
// the final value: "attached, lost, attached again" is the story of a master
// restart, and only the ordered record can tell it from "still attached".
func TestReaderReportsWhetherItHoldsTheSocket(t *testing.T) {
	t.Parallel()

	srv := newBusServer(t, pubSocket)
	srv.serve(frame(t, "salt/auth", map[string]any{"act": "accept"}), 2, 0)

	sink := newRecordSink(2)

	if err := runUntil(t, saltipc.NewReader(srv.dir, time.Now), sink); err != nil {
		t.Fatalf("Run: %v", err)
	}

	got := sink.attaches()

	if len(got) < 3 {
		t.Fatalf("Attached was reported %v; a connect/drop/reconnect cycle must "+
			"produce at least true, false, true", got)
	}

	if !got[0] {
		t.Errorf("the first Attached report was %v; the socket was open before any "+
			"event arrived, and a quiet master is a healthy master", got[0])
	}

	if !slices.Contains(got, false) {
		t.Errorf("Attached never went false across a disconnect: %v", got)
	}

	// The reader must be reporting attached again by the time the second
	// connection is delivering events; otherwise a successful reconnect still
	// renders as an outage.
	last := slices.Index(got, false)
	if !slices.Contains(got[last:], true) {
		t.Errorf("Attached never went true again after the reconnect: %v", got)
	}
}

// TestReaderReportsAFailedFirstDialAsDetached covers the path Run returns on:
// a first dial that fails is permanent, and a console left reading "connected"
// for a reader that has died is the worst of both.
func TestReaderReportsAFailedFirstDialAsDetached(t *testing.T) {
	t.Parallel()

	sink := newRecordSink(1)

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()

	// A directory with no socket in it at all.
	err := saltipc.NewReader(t.TempDir(), time.Now).Run(ctx, sink)
	if err == nil {
		t.Fatal("Run returned nil for a socket that does not exist")
	}

	got := sink.attaches()
	if len(got) != 1 || got[0] {
		t.Errorf("Attached reports = %v, want exactly one false", got)
	}
}

// TestNotPubSocketIsIdentifiableAndDiagnosable pins the SENTINEL, which is a
// separate contract from the message text the test above checks.
//
// Two things branch on errors.Is(err, ErrNotPubSocket) and neither reads the
// text: Run refuses to retry it (every other dial failure drives the backoff
// loop, and retrying this one forever would be exactly the wrong response to
// "that is not our socket"), and Diagnose turns it into invariant 1's
// explanation rather than a bare errno. An error that stopped wrapping the
// sentinel would leave both silently taking the wrong branch, and the existing
// message assertion would still pass.
func TestNotPubSocketIsIdentifiableAndDiagnosable(t *testing.T) {
	t.Parallel()

	srv := newBusServer(t, pullSocket)
	srv.serve(nil, 1, 0)

	if err := os.Symlink(filepath.Join(srv.dir, pullSocket),
		filepath.Join(srv.dir, pubSocket)); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	reader := saltipc.NewReader(srv.dir, time.Now)

	conn, err := reader.Dial()
	if err == nil {
		_ = conn.Close()

		t.Fatal("Dial opened a publish socket that resolves to the pull socket")
	}

	if !errors.Is(err, saltipc.ErrNotPubSocket) {
		t.Fatalf("Dial returned %v, which does not identify itself as ErrNotPubSocket; "+
			"Run would retry it forever and Diagnose would print a bare errno", err)
	}

	if srv.acceptCount() != 0 {
		t.Error("the pull socket was connected to; invariant 1 is broken")
	}

	got := saltipc.Diagnose(reader.SocketPath(), err)

	for _, want := range []string{
		"refusing to open",
		"structurally incapable of injecting events",
		pubSocket,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("Diagnose(%v) does not mention %q:\n%s", err, want, got)
		}
	}
}

// TestReaderReportsAGapAcrossAReconnect covers spec §8.2: a disconnection that
// draws as a flat line at zero is indistinguishable from a quiet master, which
// is exactly backwards during an incident.
func TestReaderReportsAGapAcrossAReconnect(t *testing.T) {
	t.Parallel()

	srv := newBusServer(t, pubSocket)
	srv.serve(frame(t, "salt/auth", map[string]any{"act": "accept"}), 2, 0)

	sink := newRecordSink(2)

	if err := runUntil(t, saltipc.NewReader(srv.dir, time.Now), sink); err != nil {
		t.Fatalf("Run: %v", err)
	}

	events, gaps, _ := sink.snapshot()

	if len(events) != 2 {
		t.Fatalf("got %d events, want 2 — the reader must reconnect after the master closes", len(events))
	}

	if len(gaps) == 0 {
		t.Fatal("no gap reported for a disconnect/reconnect cycle")
	}

	// One outage is reported many times as it runs — that is the point, the
	// panes must not read a flat zero in the meantime — but it is still one
	// outage: every report carries the same start, so a sink that counts
	// distinct starts counts disconnections rather than retries.
	//
	// The bound is two because this master serves two connections and then
	// stops listening: the outage between them, which the reconnect closed, and
	// the trailing one after the second connection, which was still in progress
	// when the test cancelled. Anything above that would mean retries were
	// being counted as fresh outages.
	if n := outages(gaps); n < 1 || n > 2 {
		t.Errorf("got %d distinct outages from %d gap reports, want 1 or 2 — "+
			"repeated reports of one outage must share a start", n, len(gaps))
	}

	for i, g := range gaps {
		if g.to.Before(g.from) {
			t.Errorf("gap %d = %v, must not be negative", i, g.to.Sub(g.from))
		}
	}
}

// outageSpan is how far the test clock jumps once the master is gone. It only
// has to cross a one-second bucket boundary; three seconds leaves room for the
// gap to be visible in the seconds ring without being near its 120-bucket edge.
const outageSpan = 3 * time.Second

// testClock is a stats.Clock the test drives by hand, shared with the reader so
// arrival stamping, gap reporting and bucket expiry all read the same logical
// time. It is mutex-guarded because the reader goroutine reads it while the
// test advances it.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.t
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.t = c.t.Add(d)
}

// ringSink is the Sink the rate panes actually get: it feeds a real
// stats.Rings, so the assertions are about what the operator would see rather
// than about which methods the reader happened to call.
//
// It serialises ring access the way the hub does — the reader writes under the
// lock, the test reads a snapshot under the same lock.
type ringSink struct {
	mu    sync.Mutex
	rings *stats.Rings

	// gapAfter is the instant a reported gap must reach before it counts as
	// evidence. A gap emitted before the test advanced the clock says nothing
	// about the outage that follows it.
	gapAfter time.Time

	// gapAt is the WALL-CLOCK instant of every report. The logical clock cannot
	// answer this: the reader's refresh rate is a property of its real sleeps,
	// and what it has to be fast enough for is stats.GapValidity, which is also
	// real time. It pins the REFRESH RATE rather than merely the fact that a
	// report happens at all.
	gapAt []time.Time

	gappedOnce sync.Once
	gapped     chan struct{}

	firstOnce sync.Once
	first     chan struct{}
}

func newRingSink(rings *stats.Rings, gapAfter time.Time) *ringSink {
	return &ringSink{
		rings:    rings,
		gapAfter: gapAfter,
		gapped:   make(chan struct{}),
		first:    make(chan struct{}),
	}
}

func (s *ringSink) Event(e model.Event) {
	s.mu.Lock()
	s.rings.Add(e.Arrival)
	s.mu.Unlock()

	s.firstOnce.Do(func() { close(s.first) })
}

func (s *ringSink) Gap(from, to time.Time) {
	s.mu.Lock()
	s.rings.MarkGap(from, to)
	s.gapAt = append(s.gapAt, time.Now())
	s.mu.Unlock()

	if !to.Before(s.gapAfter) {
		s.gappedOnce.Do(func() { close(s.gapped) })
	}
}

// gapTimes is a copy of every report instant so far.
func (s *ringSink) gapTimes() []time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]time.Time(nil), s.gapAt...)
}

func (s *ringSink) DecodeError(error) {}

func (s *ringSink) Attached(bool) {}

func (s *ringSink) summarySeconds() (stats.Summary, stats.Bucket) {
	s.mu.Lock()
	defer s.mu.Unlock()

	secs := s.rings.Seconds()

	return s.rings.SummarySeconds(), secs[len(secs)-1]
}

// TestReaderReportsAGapWhileTheOutageIsStillInProgress is the live half of spec
// §8.2, and the half that matters during an incident.
//
// Reporting the gap only once reconnection succeeds repairs the history
// retroactively but leaves the Rate pane saying "0 events/sec" for the entire
// duration of the outage — the exact inversion §8.2 names, since a master that
// is quiet and a bus we have lost are opposite facts. So the gap is re-reported
// after every failed retry, and here the master never comes back at all: the
// listener closed after one connection, so there is no reconnect to repair
// anything and the rings must already be showing the truth.
func TestReaderReportsAGapWhileTheOutageIsStillInProgress(t *testing.T) {
	t.Parallel()

	srv := newBusServer(t, pubSocket)
	srv.serve(frame(t, "salt/auth", map[string]any{"act": "accept"}), 1, 0)

	start := time.Date(2026, 8, 30, 8, 14, 0, 0, time.UTC)
	clk := &testClock{t: start}
	rings := stats.NewRings(clk)
	sink := newRingSink(rings, start.Add(outageSpan))

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	done := make(chan error, 1)

	go func() { done <- saltipc.NewReader(srv.dir, clk.Now).Run(ctx, sink) }()

	select {
	case <-sink.first:
	case err := <-done:
		t.Fatalf("Run returned %v before delivering the first event", err)
	case <-ctx.Done():
		t.Fatal("timed out waiting for the first event")
	}

	// The master is now gone for good: serve accepted its one connection and
	// closed the listener, which unlinks the socket, so every dial from here on
	// fails. Move the clock past a bucket boundary so the outage is a window
	// the seconds ring can actually show.
	clk.Advance(outageSpan)

	select {
	case <-sink.gapped:
	case err := <-done:
		t.Fatalf("Run returned %v without reporting the outage in progress", err)
	case <-time.After(3 * time.Second):
		t.Fatal("no gap reported while the outage was still in progress: " +
			"the reader waits for a successful reconnect, so the Rate pane " +
			"reads 0 events/sec for the whole outage (spec §8.2)")
	}

	cancel()

	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}

	summary, newest := sink.summarySeconds()

	if !newest.Gap {
		t.Errorf("newest second bucket = %+v, want Gap; a bucket with Count 0 and "+
			"Gap false renders as a flat line at zero, which is a quiet master, "+
			"not a lost bus", newest)
	}

	if !summary.NowIsGap {
		t.Error("SummarySeconds().NowIsGap = false during an in-progress outage; " +
			"the Rate pane would print Now as a genuine 0")
	}

	if summary.Now != 0 {
		t.Errorf("SummarySeconds().Now = %v while gapped, want the zero value", summary.Now)
	}
}

// TestReaderReturnsTheFirstConnectFailure lets main print a diagnostic and exit
// rather than silently backing off forever against a socket that is not there.
func TestReaderReturnsTheFirstConnectFailure(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()

	err := saltipc.NewReader(dir, time.Now).Run(ctx, newRecordSink(1))
	if err == nil {
		t.Fatal("Run returned nil for a socket that does not exist")
	}

	// The error must survive Diagnose's errors.Is checks after wrapping.
	if msg := saltipc.Diagnose(saltipc.NewReader(dir, time.Now).SocketPath(), err); !strings.Contains(msg, "salt-master") {
		t.Errorf("Diagnose of a real dial error = %q, want the salt-master advice", msg)
	}
}

// TestReaderReplaysTheLiveCapture is the end-to-end proof: 32 frames captured
// verbatim off a real Salt 3006.27 master, pushed through a socket into the
// real reader. Nothing here is built by the same code that decodes it.
func TestReaderReplaysTheLiveCapture(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile(liveFramesPath)
	if err != nil {
		t.Skipf("live capture fixture not present at %s (%v)", liveFramesPath, err)
	}

	srv := newBusServer(t, pubSocket)
	srv.serve(raw, 1, 0)

	sink := newRecordSink(liveFrameCount)

	if err := runUntil(t, saltipc.NewReader(srv.dir, time.Now), sink); err != nil {
		t.Fatalf("Run: %v", err)
	}

	events, _, errs := sink.snapshot()

	if len(events) != liveFrameCount {
		t.Fatalf("got %d events, want %d", len(events), liveFrameCount)
	}

	if errs != 0 {
		t.Errorf("DecodeError count = %d, want 0 for a clean live capture", errs)
	}

	var bareJIDs int

	for i, e := range events {
		if e.Arrival.IsZero() {
			t.Errorf("event %d (tag %q) has no arrival time", i, e.Tag)
		}

		if e.Stamp.IsZero() {
			t.Errorf("event %d (tag %q): _stamp did not parse", i, e.Tag)
		}

		if len(e.Payload) == 0 {
			t.Errorf("event %d (tag %q) lost its payload", i, e.Tag)
		}

		// Six of the 32 real tags are bare JIDs with no salt/ prefix. The
		// reader must still recover the JID from the tag.
		if bareJIDTag.MatchString(e.Tag) {
			bareJIDs++

			if e.JID != e.Tag {
				t.Errorf("bare-JID tag %q: JID = %q", e.Tag, e.JID)
			}
		}
	}

	if bareJIDs != 6 {
		t.Errorf("saw %d bare-JID tags, want 6", bareJIDs)
	}
}

func TestDiagnoseNamesTheFixForPermissionDenied(t *testing.T) {
	t.Parallel()

	// A bare "permission denied" makes the operator guess. Naming sudo, the
	// path, and the owner turns it into a one-read fix (spec §8.1).
	msg := saltipc.Diagnose("/var/run/salt/master/master_event_pub.ipc", fs.ErrPermission)

	for _, want := range []string{"sudo", pubSocket} {
		if !strings.Contains(msg, want) {
			t.Errorf("Diagnose() = %q, missing %q", msg, want)
		}
	}
}

func TestDiagnoseNamesSaltMasterForMissingSocket(t *testing.T) {
	t.Parallel()

	msg := saltipc.Diagnose("/var/run/salt/master/master_event_pub.ipc", fs.ErrNotExist)

	for _, want := range []string{"salt-master", "--sock-dir"} {
		if !strings.Contains(msg, want) {
			t.Errorf("Diagnose() = %q, missing %q", msg, want)
		}
	}
}

func TestFakeFeedsEventsWithoutASocket(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 8, 30, 8, 14, 0, 0, time.UTC)

	sink := newRecordSink(2)
	fake := saltipc.NewFake(start)

	fake.Feed(sink, "salt/job/20260830081402123456/ret/scache-1", saltipc.Fields{
		ID:     "scache-1",
		Fun:    "state.apply",
		HasRet: true,
	}, []byte("raw"))

	fake.Feed(sink, "salt/auth", saltipc.Fields{}, nil)

	events, _, _ := sink.snapshot()
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2", len(events))
	}

	if events[0].Minion != "scache-1" {
		t.Errorf("Minion = %q", events[0].Minion)
	}

	if events[0].Category != "salt/job/*/ret/*" {
		t.Errorf("Category = %q", events[0].Category)
	}

	if events[0].JID != "20260830081402123456" {
		t.Errorf("JID = %q, want it recovered from the tag", events[0].JID)
	}

	if events[1].Kind != model.KindAuth {
		t.Errorf("Kind = %v, want auth", events[1].Kind)
	}

	// Arrival must advance so consumers that bucket by arrival see an order.
	if !events[1].Arrival.After(events[0].Arrival) {
		t.Errorf("arrival did not advance: %v then %v", events[0].Arrival, events[1].Arrival)
	}
}

// TestFakeFeedDataRunsTheRealExtractPath lets packages that have no business
// knowing the wire format hand over a plain map and still exercise the same
// field extraction the socket path uses.
func TestFakeFeedDataRunsTheRealExtractPath(t *testing.T) {
	t.Parallel()

	sink := newRecordSink(1)

	fake := saltipc.NewFake(time.Date(2026, 8, 30, 8, 14, 0, 0, time.UTC))

	if err := fake.FeedData(sink, "salt/job/20260830081402123456/ret/scache-1", map[string]any{
		"id":      "scache-1",
		"fun":     "test.ping",
		"retcode": 0,
		"success": true,
		"return":  true,
		"_stamp":  "2026-08-30T08:14:02.123456",
	}); err != nil {
		t.Fatalf("FeedData: %v", err)
	}

	events, _, _ := sink.snapshot()
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}

	e := events[0]

	if e.Fun != "test.ping" || e.Minion != "scache-1" || !e.HasRet {
		t.Errorf("Fun=%q Minion=%q HasRet=%v", e.Fun, e.Minion, e.HasRet)
	}

	if e.Stamp.IsZero() {
		t.Error("Stamp is zero; FeedData must run the real _stamp parse")
	}

	if len(e.Payload) == 0 {
		t.Error("Payload is empty; FeedData must produce real msgpack")
	}
}

// TestTheReaderRepeatsAGapOftenEnoughForTheRings is the reader's half of the
// contract stats.GapValidity states.
//
// stats.Rings extrapolates a reported gap forward for GapValidity, which is
// what stops a bucket rolling over mid-outage from opening as a genuine zero —
// but only on the promise that a reporter repeats itself at least every
// GapReportInterval. Nothing in the type system enforces that: gapRefresh could
// be multiplied by four tomorrow and every unit test on either side would still
// pass while the `now` callout went back to flickering between "no data" and
// "0" on a live master.
//
// So this measures the SPACING of the reports a running reader really delivers,
// and it measures it late enough in the outage to be measuring the right thing.
// Early on, the retry backoff is shorter than the refresh period and every
// round emits a report of its own, which hides the refresh rate entirely; only
// once the backoff has grown past it does gapRefresh become what governs the
// spacing. Hence the settle: by then the reader is in the 1s and 2s rounds on
// its way to the 5s ceiling.
//
// The bound is loose relative to GapReportInterval and tight relative to
// GapValidity, because that is the property that matters — a report is allowed
// to be late, but not so late that the ring stops covering the rollover.
func TestTheReaderRepeatsAGapOftenEnoughForTheRings(t *testing.T) {
	t.Parallel()

	// settle waits out the short backoff rounds; observe is the window the
	// spacing is measured over.
	const (
		settle  = 1200 * time.Millisecond
		observe = 2500 * time.Millisecond
	)

	srv := newBusServer(t, pubSocket)
	srv.serve(frame(t, "salt/auth", map[string]any{"act": "accept"}), 1, 0)

	start := time.Date(2026, 8, 30, 8, 14, 0, 0, time.UTC)
	clk := &testClock{t: start}
	sink := newRingSink(stats.NewRings(clk), start)

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	done := make(chan error, 1)

	go func() { done <- saltipc.NewReader(srv.dir, clk.Now).Run(ctx, sink) }()

	// The master is gone for good once serve has accepted its one connection:
	// the listener closes, which unlinks the socket, so every dial from here on
	// fails and the reader is permanently in waitToRetry.
	select {
	case <-sink.gapped:
	case err := <-done:
		t.Fatalf("Run returned %v without reporting the outage in progress", err)
	case <-ctx.Done():
		t.Fatal("timed out waiting for the first gap report")
	}

	time.Sleep(settle)

	from := time.Now()

	time.Sleep(observe)

	until := time.Now()

	cancel()
	<-done

	// The spacing to measure includes the run up to the window's start and down
	// to its end: a reader silent for the whole window would otherwise show no
	// spacing at all and pass.
	var (
		worst time.Duration
		prev  = from
		seen  int
	)

	for _, at := range sink.gapTimes() {
		if at.Before(from) {
			prev = at

			continue
		}

		if at.After(until) {
			break
		}

		if d := at.Sub(prev); d > worst {
			worst = d
		}

		prev, seen = at, seen+1
	}

	if d := until.Sub(prev); d > worst {
		worst = d
	}

	if seen == 0 {
		t.Fatalf("premise failed: no gap reports at all in %v of outage", observe)
	}

	if limit := 2 * stats.GapReportInterval; worst > limit {
		t.Errorf("the reader left %v between two gap reports (%d reports in %v), "+
			"want at most %v: stats.Rings keeps the head bucket honest for only %v "+
			"after a report, so a bucket rolling over in that silence opens as a "+
			"genuine 0 and the Rate pane prints a quiet master (spec §8.2)",
			worst, seen, observe, limit, stats.GapValidity)
	}
}
