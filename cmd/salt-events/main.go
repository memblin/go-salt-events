// Command salt-events is a read-only console for the Salt master event bus.
//
// It subscribes to master_event_pub.ipc — and ONLY that socket. The pull
// socket, which could inject events onto the bus, is never opened, and that is
// enforced by construction rather than by convention: the reader is handed a
// DIRECTORY and derives the filename itself (invariant 1, see saltipc.Reader).
// Nothing here writes to the socket either — not even shutdown(SHUT_WR), which
// Salt's tornado publisher reads as EOF and answers by dropping the subscriber.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"os/user"
	"syscall"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/TKC-Labs/go-salt-events/internal/config"
	"github.com/TKC-Labs/go-salt-events/internal/export"
	"github.com/TKC-Labs/go-salt-events/internal/filter"
	"github.com/TKC-Labs/go-salt-events/internal/saltipc"
	"github.com/TKC-Labs/go-salt-events/internal/stats"
	"github.com/TKC-Labs/go-salt-events/internal/theme"
	"github.com/TKC-Labs/go-salt-events/internal/ui"
	"github.com/TKC-Labs/go-salt-events/internal/ui/detail"
	"github.com/TKC-Labs/go-salt-events/internal/ui/jobs"
	"github.com/TKC-Labs/go-salt-events/internal/ui/live"
	"github.com/TKC-Labs/go-salt-events/internal/ui/rate"
	"github.com/TKC-Labs/go-salt-events/internal/ui/summary"
)

// clock is the ONE clock this process reads. The reader stamps each event's
// arrival with it and the hub stamps each snapshot with it, so a duration
// measured between the two — a job still returning, an event's age — is
// meaningful. Salt's _stamp is never used for any of it (spec §4.3,
// invariant 2).
var clock stats.Clock = stats.RealClock{}

func main() {
	if err := run(); err != nil {
		if errors.Is(err, config.ErrHelp) {
			// -h/--help: the FlagSet has already written the usage block, so
			// printing err here would put an error line under it and exit 1,
			// which makes a successful `salt-events -h` look broken.
			os.Exit(0)
		}

		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	capture, args, err := splitCaptureArgs(os.Args[1:])
	if err != nil {
		return err
	}

	cfg, err := config.Load(args, os.Getenv)
	if err != nil {
		return err
	}

	// The reader derives this same path from the same directory; it is computed
	// here for the startup diagnostic and the help overlay.
	sockPath := config.SocketPath(cfg.SockDir)

	if capture.frames > 0 {
		return captureFrames(sockPath, capture.out, capture.frames)
	}

	query, err := filter.Parse(cfg.Filter)
	if err != nil {
		return fmt.Errorf("--filter: %w", err)
	}

	// Fail loudly and specifically BEFORE starting the TUI: a diagnostic
	// printed behind an alternate screen buffer is a diagnostic nobody reads
	// (spec §8.1).
	if _, statErr := os.Stat(sockPath); statErr != nil {
		return errors.New(saltipc.Diagnose(sockPath, statErr))
	}

	return serve(cfg, query, sockPath)
}

// serve runs the reader and the TUI until one of them stops.
func serve(cfg config.Config, query filter.Query, sockPath string) error {
	h := newHub(hubConfig{
		MaxMemory: cfg.MaxMemory,
		MaxJobs:   cfg.MaxJobs,
		Clock:     clock,
		Decode:    saltipc.DecodeValue,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// The reader is given the sock DIRECTORY, never a file path: the basename
	// is not the caller's to give (invariant 1).
	reader := saltipc.NewReader(cfg.SockDir, clock.Now)

	readErr := make(chan error, 1)

	go func() { readErr <- reader.Run(ctx, h) }()

	program := tea.NewProgram(
		ui.NewModel(h, panesFor(), ui.Options{
			Theme:      themeName(cfg),
			Interval:   config.RenderInterval,
			Filter:     query,
			SockPath:   sockPath,
			ConfigPath: cfg.ConfigPath,
			Export:     exporter(cfg, h),
		}),
		tea.WithAltScreen(),
		tea.WithContext(ctx),
	)

	if _, err := program.Run(); err != nil && !killedBySignal(ctx, err) {
		return fmt.Errorf("run TUI: %w", err)
	}

	// Stop the reader and report what it hit, but only if it has already
	// finished: a socket read blocked in the kernel unblocks on the ctx's
	// close, and waiting on it here would hang the exit behind that.
	stop()

	select {
	case err := <-readErr:
		if err != nil {
			return fmt.Errorf("event reader: %w", err)
		}
	default:
	}

	return nil
}

// killedBySignal reports whether the program ended because the process was
// signalled rather than because anything went wrong.
//
// A SIGTERM (or a SIGINT that arrives from outside the terminal, since raw mode
// delivers ctrl+c as a key rather than a signal) cancels the context, and
// bubbletea reports that as ErrProgramKilled. Printing it as an error and
// exiting 1 would make an ordinary `systemctl stop`-shaped shutdown look like a
// crash.
func killedBySignal(ctx context.Context, err error) bool {
	return errors.Is(err, tea.ErrProgramKilled) && ctx.Err() != nil
}

// panesFor builds the five panes, in tab-strip order (spec §7).
func panesFor() []ui.Pane {
	return []ui.Pane{
		live.New(),
		// The decoder is INJECTED so internal/ui never imports internal/saltipc
		// — the layer rule (spec §3.1) forbids the UI knowing the wire format,
		// and depguard fails the build on it.
		detail.New(saltipc.DecodeValue),
		rate.New(),
		summary.New(),
		jobs.New(),
	}
}

// themeName resolves the palette, honouring --no-color.
//
// Under --no-color the mono palette is selected rather than colour being
// stripped downstream: mono is a registered, contrast-validated palette in
// which bar length and text labels carry the entire encoding (spec §9), so
// everything stays readable instead of merely losing its colour.
func themeName(cfg config.Config) string {
	if cfg.NoColor {
		return theme.MonoName
	}

	return cfg.Theme
}

// exporter wires `w` to internal/export (spec §10).
//
// The refusal paths are the feature, so the error text is passed through
// verbatim: it names the estimate, the space available and the headroom rule,
// which is what tells the operator to narrow the filter rather than to go
// hunting (spec §10.2, invariant 8).
func exporter(cfg config.Config, h *hub) ui.ExportFunc {
	return func(q filter.Query) (string, error) {
		res, err := export.Write(h.AllEvents(q), export.Options{
			Dir:   export.ResolveDir(cfg.ExportDir, os.Getenv, homeForUser),
			Max:   cfg.ExportMax,
			Now:   clock.Now,
			Space: export.NewStatfsChecker(),
			// Under sudo the file would otherwise be root-owned and unreadable
			// by the operator who asked for it (spec §10.1).
			Chown:  export.ChownToSudoUser(os.Getenv),
			Decode: jsonSafeDecode,
		})

		// A failed chown is the one case that returns both: the export IS on
		// disk and complete, and deleting a good export over an ownership
		// handoff would be the worse outcome. Say both things.
		if err != nil && res.Path != "" {
			return fmt.Sprintf("wrote %d events to %s, but could not hand it to you: %v",
				res.Events, res.Path, err), nil
		}

		if err != nil {
			return "", err
		}

		return fmt.Sprintf("wrote %d events (%d bytes) to %s", res.Events, res.Bytes, res.Path), nil
	}
}

// maxJSONDepth bounds how deep jsonSafe recurses.
//
// A payload is minion-supplied and can legally be a megabyte of nothing but
// nested one-element maps, which is hundreds of thousands of levels — enough to
// overflow the goroutine stack. "Never panic on bus data" makes this a bound,
// not a comment. It matches internal/ui/detail's identical bound, for the same
// reason.
const maxJSONDepth = 32

// jsonSafeDecode is the decoder handed to the EXPORTER, as distinct from the
// one handed to the Detail pane.
//
// They need different shapes and that is the wiring layer's problem to solve,
// which is precisely why both take an injected decoder. saltipc.DecodeValue
// sets DecodeUntypedMap, so every map it returns is a
// map[interface{}]interface{} — the shape the Detail pane's renderer wants, and
// the one encoding/json refuses outright ("unsupported type"). Handing the raw
// decoder to the exporter makes `w` fail on every event carrying a map, which
// is every real event off the bus.
func jsonSafeDecode(payload []byte) (any, error) {
	v, err := saltipc.DecodeValue(payload)
	if err != nil {
		return nil, err
	}

	return jsonSafe(v, 0), nil
}

// jsonSafe rewrites a decoded payload into something encoding/json can marshal.
//
// Keys are stringified rather than asserted to string: msgpack permits any type
// as a map key, and one event captured off a live master carried a top-level
// key with spaces in it, so nothing here may assume an identifier shape.
func jsonSafe(v any, depth int) any {
	if depth > maxJSONDepth {
		return "…(nested too deeply to export)"
	}

	switch t := v.(type) {
	case map[interface{}]interface{}:
		out := make(map[string]any, len(t))
		for key, val := range t {
			out[fmt.Sprint(key)] = jsonSafe(val, depth+1)
		}

		return out

	case map[string]interface{}:
		out := make(map[string]any, len(t))
		for key, val := range t {
			out[key] = jsonSafe(val, depth+1)
		}

		return out

	case []interface{}:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = jsonSafe(item, depth+1)
		}

		return out

	default:
		return v
	}
}

// homeForUser looks up a home directory in the passwd database, for
// export.ResolveDir's $SUDO_USER step.
func homeForUser(name string) (string, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return "", fmt.Errorf("lookup user %q: %w", name, err)
	}

	return u.HomeDir, nil
}
