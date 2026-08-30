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
	Gap(from, to time.Time)

	// DecodeError reports a frame we could not read. Counted and surfaced,
	// never fatal.
	DecodeError(error)
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
			// A path that is not the publish socket is never retried.
			if !connected || errors.Is(err, ErrNotPubSocket) {
				return err
			}

			if !sleep(ctx, backoff) {
				return nil
			}

			backoff = min(backoff*2, maxBackoff)

			continue
		}

		if connected {
			sink.Gap(lostAt, r.now())
		}

		connected = true
		backoff = minBackoff

		lostAt = r.consume(ctx, conn, sink)
	}
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
