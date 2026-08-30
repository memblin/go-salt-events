package saltipc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"path/filepath"
	"time"

	"github.com/TKC-Labs/go-salt-events/internal/config"
	"github.com/TKC-Labs/go-salt-events/internal/model"
	"github.com/TKC-Labs/go-salt-events/internal/salttag"
	"github.com/TKC-Labs/go-salt-events/internal/stats"
)

// Sink receives everything the reader produces. The cache and the stats
// aggregator both implement it; the reader knows about neither.
//
// Every method is called from the reader goroutine and may be called at a very
// high rate, so implementations must be cheap and must not block on the UI.
type Sink interface {
	// Event delivers one decoded event.
	Event(model.Event)

	// Gap reports a window during which we were not connected. The rate rings
	// render this differently from zero: a disconnection that draws as a flat
	// line at zero is indistinguishable from a quiet master, which is exactly
	// backwards during an incident (spec §8.2).
	//
	// One outage produces MANY calls, not one. The window is re-reported while
	// the outage is still running, each time with the same from and a later to,
	// because a gap announced only on reconnect leaves the panes reading "0
	// events/sec" for the whole outage — the inversion §8.2 exists to prevent.
	// Implementations must therefore be idempotent over a repeated window;
	// stats.Rings.MarkGap is, since it sets flags on empty buckets rather than
	// accumulating. A sink that wants to count distinct outages counts distinct
	// from values: from is stable for the life of one outage and only changes
	// when the next one begins.
	Gap(from, to time.Time)

	// DecodeError reports a frame we could not read. Counted and surfaced,
	// never fatal.
	DecodeError(error)

	// Attached reports whether the reader currently holds the publish socket.
	//
	// It exists because connectedness is a property of the SOCKET and there is
	// nowhere else to learn it. Inferring it from event arrival instead makes a
	// healthy but quiet master read DISCONNECTED — the exact inverse of spec
	// §8.2's concern, and worse during an incident, because the operator is
	// told the tool is broken when it is working.
	//
	// It is a LEVEL, not an edge: true on every successful dial, false the
	// moment the connection is lost or a dial fails, and it may be repeated.
	// Implementations must be idempotent and must not derive it from anything
	// else. In particular Gap must not clear it — Run closes an outage window
	// by calling Gap on the RECONNECT path, so the one call that means "we are
	// back" would be the call that says "we are gone".
	Attached(bool)
}

// ErrNotPubSocket reports that the path we were about to open does not resolve
// to Salt's publish socket. It is fatal, never retried: invariant 1 says this
// program is structurally incapable of touching any other socket on the bus,
// and quietly carrying on would be exactly the wrong response.
var ErrNotPubSocket = errors.New("socket does not resolve to " + config.PubSocketName)

// Backoff bounds for reconnection after the master restarts.
const (
	minBackoff = 250 * time.Millisecond
	maxBackoff = 5 * time.Second
)

// gapRefresh is how often an in-progress outage is re-reported while we wait to
// retry.
//
// A single report per retry is not enough at the far end of the backoff: the
// rate rings expire buckets by wall clock, so buckets opened after the last
// report would be empty, and at the 5s maximum backoff the newest seconds
// bucket — the one the Rate pane prints as "now" — would read a flat zero again
// for most of every wait.
//
// It is stats.GapReportInterval and not a number of its own. That is the point:
// this used to be an independent time.Second which happened to equal the rate
// ring's bucket width, and equal was never sufficient — two 1 Hz processes with
// an arbitrary phase offset cannot cover each other, so on a live outage the
// `now` callout flipped between "no data" and "0" about once a second. The
// coupling is now a stated contract on the other side of the boundary (see
// stats.GapValidity): the ring treats a report as describing the present for
// GapValidity afterwards, on the promise that a reporter repeats it at least
// every GapReportInterval. This is the reader keeping that promise, and there is
// no second constant left to drift.
const gapRefresh = stats.GapReportInterval

// Reader owns the publish socket for the process lifetime.
//
// # Invariant 1
//
// Only master_event_pub.ipc is ever opened, and only for reading. That is
// enforced by construction rather than by convention, in three layers:
//
//  1. NewReader takes a *directory*. The basename comes from
//     config.PubSocketName via config.SocketPath, and filepath.Join can never
//     drop or rewrite its final element, so no caller input — not "..", not a
//     trailing separator, not a path that itself names master_event_pull.ipc —
//     can produce a path whose basename is anything else. There is no exported
//     field, option or setter that reaches the basename.
//  2. Before each dial the resolved path's *basename* is re-checked, which
//     closes the one remaining route: a symlink named master_event_pub.ipc
//     pointing at the pull socket. Only the basename is checked, never the
//     directory prefix — on a real master /var/run is a symlink to /run, so a
//     containment check against /var/run/salt/master rejects the genuine
//     socket, and a guard that refuses the real thing is worse than no guard.
//  3. The connection is only ever handed to a FrameReader as an io.Reader.
//     Nothing in this package calls Write on it.
//
// Note that (3) is deliberately a userspace guarantee. It is tempting to make
// writing impossible at the kernel level with shutdown(SHUT_WR), but Salt's
// publisher is a tornado IPCMessagePublisher whose IOStream reads the
// half-close as EOF and drops the subscriber immediately — verified against a
// live 3006.27 master, where it produced zero bytes of capture. Do not
// "harden" this reader that way.
type Reader struct {
	sockPath string
	now      func() time.Time
}

// NewReader returns a Reader for the publish socket inside sockDir.
//
// sockDir is a directory — the resolved sock_dir from config, not a file path.
// See the Reader doc comment for why the filename is not the caller's to give.
//
// now is injected so arrival stamping and gap behaviour are testable without
// sleeping.
func NewReader(sockDir string, now func() time.Time) *Reader {
	return &Reader{sockPath: config.SocketPath(sockDir), now: now}
}

// SocketPath is the path this Reader will open. Callers need it for the
// startup diagnostic (§8.1) and for the status line's "resolved sock_dir".
func (r *Reader) SocketPath() string {
	return r.sockPath
}

// Run reads until ctx is cancelled, reconnecting across master restarts.
//
// It returns nil on clean cancellation. A permission or missing-socket failure
// on the FIRST connection is returned so main can hand it to Diagnose and exit;
// after that, failures are transient and drive the backoff loop instead — a
// master restarting mid-incident must not take the console down with it.
func (r *Reader) Run(ctx context.Context, sink Sink) error {
	backoff := minBackoff
	connected := false

	var lostAt time.Time

	for {
		// Cancellation is a clean stop, not a failure: main asked us to quit.
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		conn, err := r.dial()
		if err != nil {
			// Said before the retry decision so a sink learns about a failed
			// FIRST dial too: Run returns permanently in that case, and a
			// console left reading "connected" for a reader that has died is
			// the worst of both.
			sink.Attached(false)

			// A path that is not the publish socket is never retried.
			if !connected || errors.Is(err, ErrNotPubSocket) {
				return err
			}

			// The outage is happening NOW. Reporting it only once the
			// reconnect succeeds repairs the history retroactively but leaves
			// the Rate pane saying "0 events/sec — the master is quiet" for the
			// entire time we are actually blind, which is the inversion spec
			// §8.2 exists to prevent. Re-reporting from the same lostAt keeps it
			// one contiguous window; MarkGap absorbs the repetition.
			if !r.waitToRetry(ctx, backoff, lostAt, sink) {
				return nil
			}

			backoff = min(backoff*2, maxBackoff)

			continue
		}

		// The socket is open. This is the only thing that means "connected",
		// and it is said before the first byte arrives because a quiet master
		// is a healthy master.
		sink.Attached(true)

		if connected {
			// Closes the window at the instant it actually ended, which the
			// reports made during the wait could only approximate.
			sink.Gap(lostAt, r.now())
		}

		connected = true
		backoff = minBackoff

		lostAt = r.consume(ctx, conn, sink)

		// consume only returns once the stream has ended, so we are blind from
		// here until the next successful dial — including while ctx is being
		// cancelled, which is the shutdown path and harms nothing.
		sink.Attached(false)
	}
}

// Dial opens the publish socket for reading, applying every layer of
// invariant 1, and hands the caller the connection.
//
// It exists for --capture, which records raw frames off the live bus and
// therefore needs the socket rather than decoded events. It is the ONE
// supported way to obtain that connection: a second, hand-rolled net.Dial next
// to this one has layer 1 (the basename is derived from the directory) but not
// layer 2 (the RESOLVED basename is re-checked), which leaves the symlink route
// to master_event_pull.ipc open on that path. There is no reason to write
// another dial; use this.
//
// The caller owns the connection and must close it. Nothing in this package
// ever writes to it and neither may the caller (invariant 1, layer 3).
func (r *Reader) Dial() (net.Conn, error) {
	return r.dial()
}

// dial opens the publish socket, refusing anything that is not it.
func (r *Reader) dial() (net.Conn, error) {
	if err := r.verifyPubSocket(); err != nil {
		return nil, err
	}

	conn, err := net.Dial("unix", r.sockPath)
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", r.sockPath, err)
	}

	return conn, nil
}

// verifyPubSocket checks that the path resolves to something still named
// master_event_pub.ipc (invariant 1, layer 2).
//
// Only the basename is compared. See the Reader doc comment: comparing the
// directory prefix breaks on every real master, because /var/run is a symlink
// to /run.
func (r *Reader) verifyPubSocket() error {
	resolved, ok := resolveSymlinks(r.sockPath)
	if !ok {
		// Nothing to resolve — the socket is missing, or a path component is
		// unreadable. Both are the dial's story to tell, with the errno that
		// Diagnose turns into an instruction.
		return nil
	}

	if filepath.Base(resolved) != config.PubSocketName {
		return fmt.Errorf("%w: %s resolves to %s", ErrNotPubSocket, r.sockPath, resolved)
	}

	return nil
}

// resolveSymlinks reports the fully resolved path, or false if it cannot be
// resolved at all.
func resolveSymlinks(path string) (string, bool) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", false
	}

	return resolved, true
}

// waitToRetry waits for d before the next dial, keeping the outage reported as
// it runs. It reports false if ctx was cancelled first.
//
// The wait is broken into gapRefresh slices so the window stays current rather
// than being announced once and going stale. Cost per report is bounded by the
// ring size, not by the outage length: MarkGap clamps its walk to the visible
// window, so an outage of hours costs the same per report as one of seconds,
// and repeating the same window only re-sets flags.
func (r *Reader) waitToRetry(ctx context.Context, d time.Duration, lostAt time.Time, sink Sink) bool {
	for remaining := d; ; remaining -= gapRefresh {
		sink.Gap(lostAt, r.now())

		if remaining <= 0 {
			return true
		}

		if !sleep(ctx, min(remaining, gapRefresh)) {
			return false
		}
	}
}

// sleep waits for d, reporting false if ctx was cancelled first.
func sleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// consume reads frames until the stream ends, returning the disconnect time.
//
// conn is passed to NewFrameReader as an io.Reader and is never written to
// (invariant 1, layer 3).
func (r *Reader) consume(ctx context.Context, conn net.Conn, sink Sink) time.Time {
	// Unblock the read when ctx is cancelled. The watcher must not outlive the
	// connection: over a long run with repeated master restarts, one goroutine
	// per reconnect parked on ctx.Done() is an unbounded leak.
	done := make(chan struct{})

	defer close(done)
	defer func() { _ = conn.Close() }()

	go func() {
		select {
		case <-ctx.Done():
		case <-done:
		}

		_ = conn.Close()
	}()

	fr := NewFrameReader(conn)

	for {
		f, err := fr.Next()
		if err != nil {
			// EOF and a desynced stream both mean "reconnect". A single
			// malformed frame does not: the length prefix means we already
			// consumed exactly that frame, so we can keep reading (spec §8.2).
			if errors.Is(err, io.EOF) || errors.Is(err, ErrFrameTooLarge) || ctx.Err() != nil {
				return r.now()
			}

			sink.DecodeError(err)

			continue
		}

		sink.Event(r.build(f))
	}
}

// build turns a wire frame into a domain event.
func (r *Reader) build(f Frame) model.Event {
	return buildEvent(r.now(), f.Tag, ExtractFields(f.Payload), f.Payload)
}

// buildEvent assembles one event from a tag, its eagerly-extracted fields and
// the still-undecoded payload. Shared by the socket reader and the Fake so a
// test double cannot drift from the real ingest path.
//
// Only the indexed fields are read; the payload is carried through as raw
// msgpack and fully decoded exactly once, later, if the operator opens it
// (spec §4.2, invariant 4).
//
// arrival is passed in rather than taken here because it must be the moment the
// event was received, and all bucketing depends on it, never on _stamp
// (spec §4.3, invariant 2).
func buildEvent(arrival time.Time, tag string, fields Fields, payload []byte) model.Event {
	info := salttag.Parse(tag)

	// The payload's id wins over the tag: not every minion-bearing tag puts the
	// id in the same position, and id is present on essentially every
	// minion-sourced event (spec §4.4).
	minion := fields.ID
	if minion == "" {
		minion = info.Minion
	}

	jid := fields.JID
	if jid == "" {
		jid = info.JID
	}

	return model.Event{
		Arrival:   arrival,
		Stamp:     fields.Stamp,
		Tag:       tag,
		Namespace: info.Namespace,
		Category:  info.Category,
		JID:       jid,
		Minion:    minion,
		Fun:       fields.Fun,
		Kind:      info.Kind,
		RetCode:   fields.RetCode,
		Success:   fields.Success,
		HasRet:    fields.HasRet,
		Payload:   payload,

		// Shed is the cache's to set, once it drops this payload to stay under
		// budget. Ingest has not touched it and must not claim otherwise: the
		// two are different failures with different fixes (spec §5.3).
		Shed:          false,
		MasterTrimmed: IsMasterTrimmed(payload),
	}
}

// Diagnose turns a connection failure into an instruction.
//
// "permission denied" alone makes the operator guess; naming the path, the
// cause, and the fix turns it into a single read (spec §8.1).
func Diagnose(sockPath string, err error) string {
	switch {
	case errors.Is(err, ErrNotPubSocket):
		return fmt.Sprintf(
			"refusing to open %s: it does not resolve to %s.\n\n"+
				"This tool only ever reads the publish socket, so that it is\n"+
				"structurally incapable of injecting events onto the bus. Check\n"+
				"what that path actually points at:\n\n    ls -l %s\n\n"+
				"Underlying error: %v\n",
			sockPath, config.PubSocketName, sockPath, err)

	case errors.Is(err, fs.ErrPermission):
		return fmt.Sprintf(
			"cannot read %s: permission denied.\n\n"+
				"The Salt master event socket is owned by root with mode 0600, so this\n"+
				"tool must run as root:\n\n    sudo salt-events\n\n"+
				"Confirm the owner and mode with:\n\n    ls -l %s\n",
			sockPath, sockPath)

	case errors.Is(err, fs.ErrNotExist):
		return fmt.Sprintf(
			"no event socket at %s.\n\n"+
				"Check that salt-master is running:\n\n"+
				"    systemctl status salt-master\n\n"+
				"If the master uses a non-default sock_dir, point at it with\n"+
				"--sock-dir (or SALTEV_SOCK_DIR). Note that Salt itself relocates\n"+
				"sock_dir to <cachedir>/.salt-unix when the configured path is long\n"+
				"(spec §2.6), so the directory in salt master's config may not be\n"+
				"where the socket actually is.\n",
			sockPath)

	default:
		return fmt.Sprintf("cannot read %s: %v\n", sockPath, err)
	}
}
