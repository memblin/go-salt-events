package saltipc_test

import (
	"context"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TKC-Labs/go-salt-events/internal/model"
	"github.com/TKC-Labs/go-salt-events/internal/saltipc"
)

// pubSocket is the only basename this program may ever open (invariant 1).
const pubSocket = "master_event_pub.ipc"

// pullSocket is the socket that could inject events onto the bus. It exists in
// this file exclusively so tests can prove it is never touched.
const pullSocket = "master_event_pull.ipc"

// recordSink collects everything the reader emits and signals once a wanted
// number of events has arrived, so tests can stop the reader deterministically
// instead of sleeping.
type recordSink struct {
	want int

	mu     sync.Mutex
	events []model.Event
	gaps   []time.Duration
	errs   int

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

	s.gaps = append(s.gaps, to.Sub(from))
}

func (s *recordSink) DecodeError(error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.errs++
}

func (s *recordSink) snapshot() ([]model.Event, []time.Duration, int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]model.Event, len(s.events))
	copy(out, s.events)

	gaps := make([]time.Duration, len(s.gaps))
	copy(gaps, s.gaps)

	return out, gaps, s.errs
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

	if len(gaps) != 1 {
		t.Fatalf("got %d gaps, want exactly 1 for one disconnect/reconnect cycle", len(gaps))
	}

	if gaps[0] < 0 {
		t.Errorf("gap duration = %v, must not be negative", gaps[0])
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
