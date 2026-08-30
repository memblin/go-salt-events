package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/TKC-Labs/go-salt-events/internal/saltipc"
)

// captureOptions is what --capture and --capture-out asked for.
type captureOptions struct {
	frames int
	out    string
}

// The capture flags. They are handled before config.Load rather than inside it
// because config.Config is the RUNTIME configuration — a fixture recorder is
// not a setting, it is a different program that happens to share a binary, and
// adding it to Config would put it in the precedence table, the TOML schema and
// the environment namespace where it means nothing.
const (
	flagCapture    = "capture"
	flagCaptureOut = "capture-out"
)

// errCapture reports a malformed capture flag.
var errCapture = errors.New("capture")

// splitCaptureArgs pulls the capture flags out of args and returns the rest.
//
// It is a hand-rolled split rather than a second flag.FlagSet because
// config.Load owns the FlagSet and parses with ContinueOnError: an unknown
// --capture would make it print usage and fail, and there is no way to add a
// flag to another package's private FlagSet. Both spellings of each form are
// accepted (-flag, --flag, =value and a following argument), because that is
// what the flag package itself accepts and `just capture` writes the = form.
func splitCaptureArgs(args []string) (captureOptions, []string, error) {
	var (
		opts captureOptions
		rest []string
	)

	for i := 0; i < len(args); i++ {
		name, value, hasValue := splitFlag(args[i])

		if name != flagCapture && name != flagCaptureOut {
			rest = append(rest, args[i])

			continue
		}

		if !hasValue {
			if i+1 >= len(args) {
				return opts, nil, fmt.Errorf("%w: --%s needs a value", errCapture, name)
			}

			i++
			value = args[i]
		}

		if err := opts.set(name, value); err != nil {
			return opts, nil, err
		}
	}

	if opts.frames > 0 && opts.out == "" {
		return opts, nil, fmt.Errorf("%w: --%s requires --%s", errCapture, flagCapture, flagCaptureOut)
	}

	return opts, rest, nil
}

// set applies one capture flag.
func (o *captureOptions) set(name, value string) error {
	if name == flagCaptureOut {
		o.out = value

		return nil
	}

	n, err := strconv.Atoi(value)
	if err != nil {
		return fmt.Errorf("%w: --%s=%q: %w", errCapture, flagCapture, value, err)
	}

	if n < 0 {
		return fmt.Errorf("%w: --%s must not be negative, got %d", errCapture, flagCapture, n)
	}

	o.frames = n

	return nil
}

// splitFlag decomposes one argument into a flag name and, when the =form was
// used, its value. A non-flag argument yields an empty name.
func splitFlag(arg string) (name, value string, hasValue bool) {
	if !strings.HasPrefix(arg, "-") {
		return "", "", false
	}

	trimmed := strings.TrimLeft(arg, "-")

	name, value, hasValue = strings.Cut(trimmed, "=")

	return name, value, hasValue
}

// frameHeader is Salt's 4-byte big-endian length prefix (spec §2.1).
const frameHeader = 4

// maxCaptureFrame is the implausible-length bound, well above Salt's 1 MiB
// max_event_size. A prefix larger than this means the stream is desynced, not
// that a 4 GiB event exists, and allocating it would take the machine out
// (spec §8.2 — saltipc's reader applies the same bound).
const maxCaptureFrame = 64 << 20

// captureFrames records n raw frames from the live socket into out.
//
// Fixtures MUST come from the real bus. A hand-written fixture encodes our
// assumptions about the wire format and would pass even when those assumptions
// are wrong — which is exactly the failure this project is most exposed to
// (spec §13).
//
// The frames are written back byte for byte, prefix included, so the file is a
// verbatim recording of the stream rather than a re-encoding of our reading of
// it. Nothing here decodes anything, and nothing here writes to the socket.
func captureFrames(sockPath, out string, n int) error {
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		return errors.New(saltipc.Diagnose(sockPath, err))
	}

	defer func() { _ = conn.Close() }()

	// Cleaned because it comes from a command-line flag, not from a constant.
	f, err := os.Create(filepath.Clean(out))
	if err != nil {
		return fmt.Errorf("create %s: %w", out, err)
	}

	defer func() { _ = f.Close() }()

	if err := copyFrames(conn, f, n); err != nil {
		return err
	}

	// Closed explicitly as well as deferred: a capture that reported success
	// and then failed to flush would leave a truncated fixture that looks
	// authoritative.
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", out, err)
	}

	fmt.Fprintf(os.Stderr, "captured %d frames to %s\n", n, out)

	return nil
}

// copyFrames reads exactly n length-prefixed frames from src into dst.
func copyFrames(src io.Reader, dst io.Writer, n int) error {
	hdr := make([]byte, frameHeader)

	for i := range n {
		if _, err := io.ReadFull(src, hdr); err != nil {
			return fmt.Errorf("read frame %d length: %w", i, err)
		}

		length := binary.BigEndian.Uint32(hdr)
		if length > maxCaptureFrame {
			return fmt.Errorf("%w: frame %d claims %d bytes, which means the stream is desynced",
				errCapture, i, length)
		}

		body := make([]byte, length)
		if _, err := io.ReadFull(src, body); err != nil {
			return fmt.Errorf("read frame %d body: %w", i, err)
		}

		if _, err := dst.Write(hdr); err != nil {
			return fmt.Errorf("write frame %d length: %w", i, err)
		}

		if _, err := dst.Write(body); err != nil {
			return fmt.Errorf("write frame %d body: %w", i, err)
		}
	}

	return nil
}
