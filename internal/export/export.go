// Package export writes the filtered event set to NDJSON.
//
// This program runs as root on a production salt-master, so the export refuses
// before it can fill the filesystem rather than discovering the problem
// halfway through a write (invariant 8). The two halves of that invariant are
// both here: Write performs the pre-flight space check before it opens
// anything, and every byte goes to a .partial that is either renamed into
// place whole or unlinked.
package export

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"time"

	"github.com/TKC-Labs/go-salt-events/internal/model"
)

// Errors callers branch on.
var (
	ErrInsufficientSpace = errors.New("not enough free space to export safely")
	ErrOverMax           = errors.New("export would exceed the configured maximum")
)

// errUnusableOptions reports a caller mistake rather than a condition of the
// machine. It is deliberately not ErrOverMax or ErrInsufficientSpace: the
// modal for those tells the operator to narrow the filter, which is useless
// advice when the real problem is a missing space checker.
var errUnusableOptions = errors.New("export options are unusable")

// jsonExpansion is how much larger NDJSON is expected to be than msgpack.
//
// Real ratios for this shape of data run 1.3-1.8x. 2.0 is deliberately
// pessimistic: over-estimating costs a declined export, under-estimating costs
// a full disk on a production master.
const jsonExpansion = 2.0

// recordOverhead is the per-event floor added to the raw byte count before
// jsonExpansion is applied.
//
// Spec §10.2's formula is Σ(len(tag)+len(payload)), which omits the fixed cost
// of the record envelope entirely: the RFC3339Nano arrival timestamp, the JSON
// keys, and the two truncation flags come to roughly 120-165 bytes for a real
// event even when the payload is empty. Without this floor a large selection of
// small events under-estimates by ~2.1x and a selection of shed events (whose
// payload is gone, so §10.2's sum is just the tag) by ~10x — which is exactly
// the ENOSPC on a production master that the pre-flight exists to prevent.
//
// 160 is a deliberate over-estimate, and it stays spec-compatible because it
// can only ever RAISE the number: it can never permit a write §10.2's formula
// would have refused, only refuse one §10.2's formula would have permitted.
// Do not "clean this up" into the tag/payload sum — an over-estimate costs a
// declined export, an under-estimate costs the master.
const recordOverhead = 160

// minHeadroom is the floor on space that must remain after the write.
const minHeadroom = 1 << 30 // 1 GiB

// headroomFraction is the proportional floor, whichever is larger.
const headroomFraction = 0.10

// partialSuffix marks a file that is not yet a complete export. It is appended
// after .ndjson rather than replacing it so a leftover is obviously ours and
// obviously incomplete.
const partialSuffix = ".partial"

// filePerm keeps an export of a production event bus off other users' eyes
// (spec §10.1).
const filePerm = 0o600

// Options configures a write. Every external dependency is injected so the
// refusal paths are testable.
type Options struct {
	// Dir is the destination directory, already resolved (spec §10.1).
	Dir string

	// Max is the hard cap in bytes. It bounds the actual write, not just the
	// estimate, and must be positive: a zero cap is a misconfiguration, never
	// "unlimited". internal/config already rejects a non-positive value, and
	// Write re-checks because it is reachable from other callers.
	Max int64

	// Now supplies the timestamp in the filename. Defaults to time.Now.
	Now func() time.Time

	// Space is the pre-flight check. Required: without it there is no
	// invariant 8, so Write refuses rather than proceeding blind.
	Space SpaceChecker

	// Chown hands the finished file to the invoking user. Optional.
	Chown func(path string) error

	// Decode turns a raw payload into a Go value. Injected so this package
	// never imports a msgpack library (spec §3.1).
	//
	// It does NOT have to return something encoding/json can marshal: whatever
	// it returns is passed through jsonSafe before it is encoded, so a caller
	// may hand over the same raw decoder every other consumer gets. That is
	// deliberate — requiring each caller to wrap its decoder is what re-arms
	// the "unsupported type" bug on the next one.
	Decode func([]byte) (any, error)
}

// Result describes a completed export.
type Result struct {
	Path   string
	Events int
	Bytes  int64
}

// record is one NDJSON line (spec §10.4).
type record struct {
	Arrival   string `json:"arrival"`
	Stamp     string `json:"stamp,omitempty"`
	Tag       string `json:"tag"`
	Namespace string `json:"namespace,omitempty"`
	Category  string `json:"category,omitempty"`
	JID       string `json:"jid,omitempty"`
	Minion    string `json:"minion,omitempty"`
	Fun       string `json:"fun,omitempty"`

	// Pointers, not plain values: retcode 0 means "succeeded" and absent
	// means "never returned". omitempty on an int would render those two
	// identically, and for a shed event these fields are the only surviving
	// evidence of the return (invariant 9's inputs, spec §10.4).
	RetCode *int  `json:"retcode,omitempty"`
	Success *bool `json:"success,omitempty"`

	Payload any `json:"payload"`

	// The two truncation causes stay separate. This file is the LAST place
	// they can be told apart — the bus data is gone (spec §5.3, §10.4).
	PayloadTruncated bool `json:"payload_truncated"`
	MasterTrimmed    bool `json:"master_trimmed"`
}

// Estimate returns the pessimistic encoded size of events.
//
// This is spec §10.2's sum plus recordOverhead per event; see that constant for
// why the bare §10.2 formula under-shoots the encoded size by 2-10x.
func Estimate(events []model.Event) int64 {
	var raw int64

	for _, e := range events {
		raw += int64(len(e.Tag)+len(e.Payload)) + recordOverhead
	}

	return int64(float64(raw) * jsonExpansion)
}

// Write exports events, refusing rather than risking the filesystem.
//
// On any failure the destination directory is left exactly as it was found:
// either the file is complete or it does not exist. The one case that returns
// both a Result and an error is a failed chown — the export is on disk and
// readable by root, and deleting a good export over an ownership handoff would
// be the worse outcome.
func Write(events []model.Event, opts Options) (Result, error) {
	opts, err := opts.normalised()
	if err != nil {
		return Result{}, err
	}

	if err := preflight(events, opts); err != nil {
		return Result{}, err
	}

	return writeFile(events, opts)
}

// normalised validates the caller's options and fills in the defaults that
// have a safe one. Anything whose absence would weaken invariant 8 is an
// error instead.
func (o Options) normalised() (Options, error) {
	if o.Dir == "" {
		return o, fmt.Errorf("%w: no destination directory", errUnusableOptions)
	}

	if o.Max <= 0 {
		return o, fmt.Errorf(
			"%w: max must be positive, got %d (0 is not unlimited)", errUnusableOptions, o.Max)
	}

	if o.Space == nil {
		return o, fmt.Errorf("%w: no space checker, so the write cannot be pre-flighted",
			errUnusableOptions)
	}

	if o.Decode == nil {
		return o, fmt.Errorf("%w: no payload decoder", errUnusableOptions)
	}

	o.Dir = filepath.Clean(o.Dir)

	if o.Now == nil {
		o.Now = time.Now
	}

	return o, nil
}

// preflight is invariant 8's first half: it runs before anything is opened,
// and its refusals are the feature (spec §10.2).
func preflight(events []model.Event, opts Options) error {
	est := Estimate(events)

	if est > opts.Max {
		return fmt.Errorf("%w: estimated %d bytes, limit %d", ErrOverMax, est, opts.Max)
	}

	avail, total, err := opts.Space.Available(opts.Dir)
	if err != nil {
		// Not knowing how much room there is is not permission to write.
		return fmt.Errorf("check free space: %w", err)
	}

	headroom := int64(float64(total) * headroomFraction)
	if headroom < minHeadroom {
		headroom = minHeadroom
	}

	if avail-est < headroom {
		return fmt.Errorf(
			"%w: need ~%d bytes, %d available, and %d must remain free",
			ErrInsufficientSpace, est, avail, headroom)
	}

	return nil
}

// writeFile streams the export to a .partial and renames it into place.
func writeFile(events []model.Event, opts Options) (Result, error) {
	final, err := uniquePath(opts.Dir, opts.Now())
	if err != nil {
		return Result{}, err
	}

	partial := final + partialSuffix

	// O_EXCL: if something already sits at the partial path it is not ours,
	// so this fails rather than truncating a stranger's file.
	f, err := os.OpenFile(filepath.Clean(partial), os.O_CREATE|os.O_EXCL|os.O_WRONLY, filePerm)
	if err != nil {
		return Result{}, fmt.Errorf("create %s: %w", partial, err)
	}

	written, err := writeAll(f, events, opts)
	if err != nil {
		// Never leave a truncated file that looks complete. ENOSPC lands here
		// despite the pre-flight, because another process can win the race.
		if rmErr := os.Remove(partial); rmErr != nil {
			return Result{}, errors.Join(err, fmt.Errorf("remove %s: %w", partial, rmErr))
		}

		return Result{}, err
	}

	// rename(2) within a directory is atomic, so a .ndjson is always whole.
	if err := os.Rename(partial, final); err != nil {
		_ = os.Remove(partial)

		return Result{}, fmt.Errorf("rename into place: %w", err)
	}

	res := Result{Path: final, Events: len(events), Bytes: written}

	if opts.Chown != nil {
		if err := opts.Chown(final); err != nil {
			// Not fatal: the data is written, and the Result still carries the
			// path. The operator just needs sudo to read it, which is worth a
			// warning rather than losing the export.
			return res, fmt.Errorf("chown %s: %w", final, err)
		}
	}

	return res, nil
}

// writeAll streams, flushes to the platter, and closes, keeping whichever
// error came first. A close error after a clean stream still fails the export:
// the bytes may never have reached the file.
func writeAll(f *os.File, events []model.Event, opts Options) (int64, error) {
	written, err := stream(f, events, opts)

	if err == nil {
		// rename(2) is atomic with respect to the directory entry only. Without
		// this an unlucky crash could leave a present-but-empty .ndjson, which
		// is exactly the "looks complete, is not" outcome invariant 8 forbids.
		if syncErr := f.Sync(); syncErr != nil {
			err = fmt.Errorf("sync export: %w", syncErr)
		}
	}

	if closeErr := f.Close(); err == nil && closeErr != nil {
		err = fmt.Errorf("close export: %w", closeErr)
	}

	return written, err
}

// stream writes NDJSON one event at a time.
//
// Never buffered whole: the cache may hold hundreds of megabytes and a JSON
// copy in memory would double peak RSS at exactly the wrong moment (spec §10.3).
func stream(w io.Writer, events []model.Event, opts Options) (int64, error) {
	bw := bufio.NewWriter(w)

	var written int64

	for _, e := range events {
		line, err := encode(e, opts.Decode)
		if err != nil {
			return written, err
		}

		n, err := bw.Write(line)
		written += int64(n)

		if err != nil {
			return written, fmt.Errorf("write export: %w", err)
		}

		// The cap bounds the real write, not just the estimate, and aborts the
		// same way ENOSPC does (spec §10.3).
		if written > opts.Max {
			return written, fmt.Errorf(
				"%w: wrote %d bytes, limit %d", ErrOverMax, written, opts.Max)
		}
	}

	if err := bw.Flush(); err != nil {
		return written, fmt.Errorf("flush export: %w", err)
	}

	return written, nil
}

// encode renders one event as an NDJSON line, newline included.
func encode(e model.Event, decode func([]byte) (any, error)) ([]byte, error) {
	rec := record{
		Arrival:          e.Arrival.UTC().Format(time.RFC3339Nano),
		Stamp:            "",
		Tag:              e.Tag,
		Namespace:        e.Namespace,
		Category:         e.Category,
		JID:              e.JID,
		Minion:           e.Minion,
		Fun:              e.Fun,
		RetCode:          nil,
		Success:          nil,
		Payload:          nil,
		PayloadTruncated: e.Shed,
		MasterTrimmed:    e.MasterTrimmed,
	}

	// Stamp is Salt's, and may be absent or unparseable (spec §2.4). A zero
	// time formatted anyway would export as year 1 and read as real.
	if !e.Stamp.IsZero() {
		rec.Stamp = e.Stamp.UTC().Format(time.RFC3339Nano)
	}

	if e.HasRet {
		retCode, success := e.RetCode, e.Success
		rec.RetCode, rec.Success = &retCode, &success
	}

	// A shed event exports with a null payload rather than being omitted: its
	// tag and timing are still evidence (spec §10.4).
	if len(e.Payload) > 0 {
		v, err := decode(e.Payload)
		if err != nil {
			return nil, fmt.Errorf("decode payload for %s: %w", e.Tag, err)
		}

		// The injected decoder returns whatever the wire format decodes to,
		// which encoding/json refuses for most real payloads. Neutralising it
		// HERE is what makes the guarantee a property of this package rather
		// than a step each caller must remember — see jsonSafe.
		rec.Payload = jsonSafe(v, 0)
	}

	line, err := json.Marshal(rec)
	if err != nil {
		return nil, fmt.Errorf("marshal record for %s: %w", e.Tag, err)
	}

	return append(line, '\n'), nil
}

// uniquePath builds a timestamped filename that does not already exist
// (spec §10.1). A collision appends -2, -3 … rather than clobbering.
func uniquePath(dir string, now time.Time) (string, error) {
	const maxAttempts = 100

	stamp := now.UTC().Format("20060102T150405Z")
	base := filepath.Join(dir, "salt-events-"+stamp)

	for i := range maxAttempts {
		p := base + ".ndjson"
		if i > 0 {
			p = base + "-" + strconv.Itoa(i+1) + ".ndjson"
		}

		if _, err := os.Stat(p); errors.Is(err, os.ErrNotExist) {
			return p, nil
		}
	}

	return "", fmt.Errorf("could not find an unused filename under %s", dir)
}

// ResolveDir picks the export destination (spec §10.1).
//
// env and homeFor are injected so this is testable without a real sudo
// environment or a real passwd database.
func ResolveDir(
	explicit string,
	env func(string) string,
	homeFor func(string) (string, error),
) string {
	if explicit != "" {
		return explicit
	}

	// Under sudo $HOME is root's, so the operator's own home would never be
	// found. SUDO_USER's home therefore wins.
	if u := env("SUDO_USER"); u != "" {
		if home, err := homeFor(u); err == nil && home != "" {
			return home
		}
	}

	if home := env("HOME"); home != "" {
		return home
	}

	// /var/tmp, never /tmp: /tmp is frequently tmpfs, so writing an export
	// there spends RAM on the machine already running the master.
	return "/var/tmp"
}

// ChownToSudoUser returns a Chown func that hands the file back to the
// invoking user. Under sudo the file would otherwise be root-owned and
// unreadable without sudo — a papercut that would recur on every export.
func ChownToSudoUser(env func(string) string) func(string) error {
	name := env("SUDO_USER")
	if name == "" {
		return func(string) error { return nil }
	}

	return func(path string) error {
		u, err := user.Lookup(name)
		if err != nil {
			return fmt.Errorf("lookup %s: %w", name, err)
		}

		uid, err := strconv.Atoi(u.Uid)
		if err != nil {
			return fmt.Errorf("parse uid %q: %w", u.Uid, err)
		}

		gid, err := strconv.Atoi(u.Gid)
		if err != nil {
			return fmt.Errorf("parse gid %q: %w", u.Gid, err)
		}

		if err := os.Chown(path, uid, gid); err != nil {
			return fmt.Errorf("chown %s: %w", path, err)
		}

		return nil
	}
}
