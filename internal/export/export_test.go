package export_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TKC-Labs/go-salt-events/internal/export"
	"github.com/TKC-Labs/go-salt-events/internal/model"
)

// fakeSpace reports whatever the test wants.
type fakeSpace struct {
	avail int64
	total int64
	err   error
}

func (f fakeSpace) Available(string) (int64, int64, error) {
	return f.avail, f.total, f.err
}

func events(n, payload int) []model.Event {
	out := make([]model.Event, 0, n)

	for i := range n {
		out = append(out, model.Event{
			Arrival: time.Date(2026, 8, 30, 8, 14, 2, 0, time.UTC),
			Tag:     fmt.Sprintf("salt/job/2026083008140212345%d/ret/web-1", i%10),
			Minion:  "web-1",
			Payload: bytes.Repeat([]byte("x"), payload),
		})
	}

	return out
}

func baseOpts(t *testing.T, space export.SpaceChecker) export.Options {
	t.Helper()

	return export.Options{
		Dir:   t.TempDir(),
		Max:   1 << 30,
		Now:   func() time.Time { return time.Date(2026, 8, 30, 8, 14, 2, 0, time.UTC) },
		Space: space,
		Chown: func(string) error { return nil },
		Decode: func(b []byte) (any, error) {
			return map[string]any{"len": len(b)}, nil
		},
	}
}

// roomyOpts is the common case: plenty of space, so the test is about
// something other than the space check.
func roomyOpts(t *testing.T) export.Options {
	t.Helper()

	return baseOpts(t, fakeSpace{avail: 100 << 30, total: 200 << 30, err: nil})
}

// assertDirIsEmpty is the second half of invariant 8: a refused or failed
// export leaves the destination exactly as it found it.
func assertDirIsEmpty(t *testing.T, dir string) {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}

	for _, e := range entries {
		t.Errorf("a failed export left %s behind", e.Name())
	}
}

// partialSize reports the size of the in-progress export, so a mid-write test
// can prove bytes really reached the platter instead of assuming it.
func partialSize(t *testing.T, dir string) int64 {
	t.Helper()

	matches, err := filepath.Glob(filepath.Join(dir, "*.partial"))
	if err != nil || len(matches) != 1 {
		t.Errorf("want exactly one .partial in %s, got %v (%v)", dir, matches, err)

		return 0
	}

	info, err := os.Stat(matches[0])
	if err != nil {
		t.Errorf("Stat %s: %v", matches[0], err)

		return 0
	}

	return info.Size()
}

func TestWriteProducesATimestampedFile(t *testing.T) {
	t.Parallel()

	opts := baseOpts(t, fakeSpace{avail: 100 << 30, total: 200 << 30, err: nil})

	got, err := export.Write(events(10, 100), opts)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	if !strings.Contains(filepath.Base(got.Path), "20260830T081402Z") {
		t.Errorf("filename is not timestamped: %s", got.Path)
	}

	if !strings.HasSuffix(got.Path, ".ndjson") {
		t.Errorf("wrong extension: %s", got.Path)
	}

	if got.Events != 10 {
		t.Errorf("Events = %d, want 10", got.Events)
	}
}

func TestWriteNeverOverwritesAnExistingFile(t *testing.T) {
	t.Parallel()

	opts := baseOpts(t, fakeSpace{avail: 100 << 30, total: 200 << 30, err: nil})

	first, err := export.Write(events(2, 10), opts)
	if err != nil {
		t.Fatalf("first Write: %v", err)
	}

	second, err := export.Write(events(2, 10), opts)
	if err != nil {
		t.Fatalf("second Write: %v", err)
	}

	if first.Path == second.Path {
		t.Errorf("second export clobbered the first at %s", first.Path)
	}
}

func TestWriteRefusesWhenSpaceIsTight(t *testing.T) {
	t.Parallel()

	// The whole point. This runs as root on a production master; the refusal
	// is the feature (invariant 8).
	opts := baseOpts(t, fakeSpace{avail: 1 << 20, total: 100 << 30, err: nil})

	_, err := export.Write(events(1000, 10_000), opts)
	if !errors.Is(err, export.ErrInsufficientSpace) {
		t.Fatalf("err = %v, want ErrInsufficientSpace", err)
	}

	assertDirIsEmpty(t, opts.Dir)
}

func TestWriteLeavesHeadroomEvenWhenTheEstimateFits(t *testing.T) {
	t.Parallel()

	// Fitting is not enough: the write must leave max(1 GiB, 10%) free, so a
	// successful export cannot be what tips the master over.
	need := export.Estimate(events(100, 1000))

	opts := baseOpts(t, fakeSpace{avail: need + (1 << 20), total: 100 << 30, err: nil})

	if _, err := export.Write(events(100, 1000), opts); !errors.Is(err, export.ErrInsufficientSpace) {
		t.Errorf("err = %v, want ErrInsufficientSpace — headroom was not enforced", err)
	}

	assertDirIsEmpty(t, opts.Dir)
}

func TestWriteRefusesOverTheHardCap(t *testing.T) {
	t.Parallel()

	opts := baseOpts(t, fakeSpace{avail: 100 << 30, total: 200 << 30, err: nil})
	opts.Max = 1000

	if _, err := export.Write(events(1000, 1000), opts); !errors.Is(err, export.ErrOverMax) {
		t.Errorf("err = %v, want ErrOverMax", err)
	}

	assertDirIsEmpty(t, opts.Dir)
}

func TestWriteLeavesNoPartialFileOnWriteFailure(t *testing.T) {
	t.Parallel()

	// Another process can win the race despite the pre-flight. A truncated
	// .ndjson that looks complete is worse than no file at all.
	opts := baseOpts(t, fakeSpace{avail: 100 << 30, total: 200 << 30, err: nil})
	opts.Decode = func([]byte) (any, error) { return nil, errors.New("boom") }

	_, err := export.Write(events(5, 10), opts)
	if err == nil {
		t.Fatal("expected an error")
	}

	entries, rErr := os.ReadDir(opts.Dir)
	if rErr != nil {
		t.Fatalf("ReadDir: %v", rErr)
	}

	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".partial") {
			t.Errorf("a .partial file was left behind: %s", e.Name())
		}

		if strings.HasSuffix(e.Name(), ".ndjson") {
			t.Errorf("a complete-looking file was left after a failure: %s", e.Name())
		}
	}
}

func TestWriteRecordsBothTruncationCausesSeparately(t *testing.T) {
	t.Parallel()

	// The export is the last place these can be told apart — the original bus
	// data is gone. Flattening them loses the distinction permanently
	// (spec §10.4).
	opts := baseOpts(t, fakeSpace{avail: 100 << 30, total: 200 << 30, err: nil})

	evs := []model.Event{
		{Arrival: time.Now(), Tag: "salt/a", Shed: true},
		{Arrival: time.Now(), Tag: "salt/b", MasterTrimmed: true, Payload: []byte("x")},
	}

	got, err := export.Write(evs, opts)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	data, err := os.ReadFile(got.Path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	s := string(data)

	if !strings.Contains(s, `"payload_truncated":true`) {
		t.Errorf("shed flag missing:\n%s", s)
	}

	if !strings.Contains(s, `"master_trimmed":true`) {
		t.Errorf("master_trimmed flag missing:\n%s", s)
	}
}

func TestWriteExportsShedEventsRatherThanOmittingThem(t *testing.T) {
	t.Parallel()

	// A shed event's tag and timing are still evidence.
	opts := baseOpts(t, fakeSpace{avail: 100 << 30, total: 200 << 30, err: nil})

	got, err := export.Write([]model.Event{
		{Arrival: time.Now(), Tag: "salt/shed", Shed: true},
	}, opts)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	data, err := os.ReadFile(got.Path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	if !strings.Contains(string(data), "salt/shed") {
		t.Error("a shed event was omitted from the export")
	}

	if !strings.Contains(string(data), `"payload":null`) {
		t.Errorf("a shed payload should export as null, got:\n%s", data)
	}
}

func TestResolveDirPrefersTheInvokingUsersHomeOverTmp(t *testing.T) {
	t.Parallel()

	// /tmp is frequently tmpfs — writing an export there spends RAM on the
	// machine already running the master, so it is the last resort and /var/tmp
	// is preferred over it (spec §10.1).
	env := func(k string) string {
		switch k {
		case "SUDO_USER":
			return "tkcadmin"
		case "HOME":
			return "/root"
		default:
			return ""
		}
	}

	homeFor := func(u string) (string, error) {
		if u == "tkcadmin" {
			return "/home/tkcadmin", nil
		}

		return "", errors.New("no such user")
	}

	if got := export.ResolveDir("", env, homeFor); got != "/home/tkcadmin" {
		t.Errorf("ResolveDir = %q, want /home/tkcadmin", got)
	}

	noSudo := func(string) string { return "" }
	if got := export.ResolveDir("", noSudo, homeFor); got != "/var/tmp" {
		t.Errorf("ResolveDir fallback = %q, want /var/tmp", got)
	}
}

// --- Invariant 8, first half: no write without a pre-flight check ----------

func TestWriteRefusesRatherThanSkippingThePreFlightCheck(t *testing.T) {
	t.Parallel()

	// A caller that forgets to inject a SpaceChecker must not silently get an
	// unchecked write: that is exactly invariant 8's failure mode, and a nil
	// deref here would take the whole TUI down with it.
	opts := roomyOpts(t)
	opts.Space = nil

	if _, err := export.Write(events(3, 10), opts); err == nil {
		t.Error("Write with no SpaceChecker succeeded; the pre-flight check was skipped")
	}

	assertDirIsEmpty(t, opts.Dir)
}

func TestWriteRefusesWhenTheSpaceCheckItselfFails(t *testing.T) {
	t.Parallel()

	// An unreadable statfs means we do not know how much room there is, and
	// "do not know" must never be treated as "enough".
	opts := baseOpts(t, fakeSpace{avail: 0, total: 0, err: errors.New("statfs: EACCES")})

	if _, err := export.Write(events(3, 10), opts); err == nil {
		t.Error("Write succeeded despite an unusable space check")
	}

	assertDirIsEmpty(t, opts.Dir)
}

func TestWriteRejectsAnUnusableConfiguration(t *testing.T) {
	t.Parallel()

	// internal/config already rejects a non-positive export-max, but Write is
	// reachable from tests and from any future caller, and "0" must never be
	// read as "unlimited" on a production master.
	tests := []struct {
		name   string
		mutate func(o *export.Options)
	}{
		{name: "zero max", mutate: func(o *export.Options) { o.Max = 0 }},
		{name: "negative max", mutate: func(o *export.Options) { o.Max = -1 }},
		{name: "no destination", mutate: func(o *export.Options) { o.Dir = "" }},
		{name: "no decoder", mutate: func(o *export.Options) { o.Decode = nil }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			opts := roomyOpts(t)
			dir := opts.Dir

			tt.mutate(&opts)

			if _, err := export.Write(events(3, 10), opts); err == nil {
				t.Error("expected an error")
			}

			assertDirIsEmpty(t, dir)
		})
	}
}

func TestWriteDefaultsAMissingClock(t *testing.T) {
	t.Parallel()

	// A nil Now is a programming slip, not bus data; falling back to the real
	// clock is better than panicking inside the render loop's write goroutine.
	opts := roomyOpts(t)
	opts.Now = nil
	opts.Chown = nil

	got, err := export.Write(events(2, 10), opts)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	if !strings.HasSuffix(got.Path, ".ndjson") {
		t.Errorf("Path = %q, want a .ndjson file", got.Path)
	}
}

// --- Invariant 8, second half: never a partial file -----------------------

func TestWriteLeavesNothingBehindAfterBytesHaveReachedDisk(t *testing.T) {
	t.Parallel()

	// The dangerous case is a failure AFTER real bytes are on disk. Decode here
	// returns the payload itself rather than its length, so each record encodes
	// to ~64 KiB and the first three are long past the 4 KiB bufio buffer by the
	// time the fourth fails. onDisk is captured at the moment of the failure so
	// this test proves that rather than merely asserting it.
	opts := roomyOpts(t)

	var onDisk int64

	calls := 0
	opts.Decode = func(b []byte) (any, error) {
		calls++
		if calls > 3 {
			onDisk = partialSize(t, opts.Dir)

			return nil, errors.New("decode blew up mid-export")
		}

		return string(b), nil
	}

	if _, err := export.Write(events(10, 64<<10), opts); err == nil {
		t.Fatal("expected an error")
	}

	if onDisk <= 0 {
		t.Errorf("no bytes had reached the .partial, so this is not the mid-write case")
	}

	assertDirIsEmpty(t, opts.Dir)
}

func TestWriteAbortsMidStreamOnTheHardCap(t *testing.T) {
	t.Parallel()

	// The estimate can still be wrong in the safe-looking direction: a payload
	// of control bytes expands six-for-one when encoded (each NUL becomes a
	// six-character escape in the JSON string), an expansion no per-record
	// floor can bound. The cap therefore has to bound the actual write, not
	// just the guess, and abort the same way ENOSPC does (spec §10.3).
	evs := events(20, 0)
	for i := range evs {
		evs[i].Payload = make([]byte, 1024) // all NUL
	}

	opts := roomyOpts(t)
	opts.Decode = func(b []byte) (any, error) { return string(b), nil }
	opts.Max = export.Estimate(evs) + 1 // passes the pre-flight, fails mid-write

	_, err := export.Write(evs, opts)
	if !errors.Is(err, export.ErrOverMax) {
		t.Fatalf("err = %v, want ErrOverMax", err)
	}

	assertDirIsEmpty(t, opts.Dir)
}

func TestWriteFailsCleanlyWhenTheDestinationIsUnusable(t *testing.T) {
	t.Parallel()

	// §10.1 resolves the directory but cannot prove it is writable; the proof
	// is the open, which must fail without creating anything.
	parent := t.TempDir()

	opts := roomyOpts(t)
	opts.Dir = filepath.Join(parent, "does-not-exist")

	if _, err := export.Write(events(2, 10), opts); err == nil {
		t.Error("expected an error writing into a missing directory")
	}

	assertDirIsEmpty(t, parent)
}

func TestWriteDoesNotDisturbAnObstructedPartialPath(t *testing.T) {
	t.Parallel()

	// If something already occupies the .partial path, the open fails. It must
	// fail without unlinking whatever is there — that file is not ours.
	opts := roomyOpts(t)

	obstruction := filepath.Join(
		opts.Dir, "salt-events-20260830T081402Z.ndjson.partial")
	if err := os.WriteFile(obstruction, []byte("someone else's data"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, err := export.Write(events(2, 10), opts); err == nil {
		t.Error("expected an error when the .partial path is occupied")
	}

	if _, err := os.Stat(obstruction); err != nil {
		t.Errorf("the pre-existing file was disturbed: %v", err)
	}
}

func TestWriteLeavesNoPartialBehindOnSuccess(t *testing.T) {
	t.Parallel()

	opts := roomyOpts(t)

	got, err := export.Write(events(20, 4096), opts)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	entries, err := os.ReadDir(opts.Dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}

	if len(entries) != 1 {
		t.Errorf("want exactly the finished export, got %d entries", len(entries))
	}

	info, err := os.Stat(got.Path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}

	// An export of a production event bus is not world-readable (spec §10.1).
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", info.Mode().Perm())
	}

	if got.Bytes != info.Size() {
		t.Errorf("Bytes = %d, but the file is %d bytes", got.Bytes, info.Size())
	}
}

func TestWriteReportsAChownFailureWithoutLosingTheExport(t *testing.T) {
	t.Parallel()

	// The data is already safely on disk by then. Deleting a good export
	// because the ownership handoff failed would be the worse outcome, so the
	// Result still carries the path the operator needs.
	opts := roomyOpts(t)
	opts.Chown = func(string) error { return errors.New("chown: EPERM") }

	got, err := export.Write(events(3, 10), opts)
	if err == nil {
		t.Fatal("expected the chown failure to be reported")
	}

	if got.Path == "" {
		t.Fatal("Result.Path was empty, so the operator cannot find the export")
	}

	if _, sErr := os.Stat(got.Path); sErr != nil {
		t.Errorf("the complete export was discarded: %v", sErr)
	}
}

// --- Record shape ---------------------------------------------------------

func TestWriteDistinguishesRetcodeZeroFromNoReturnAtAll(t *testing.T) {
	t.Parallel()

	// retcode 0 is "succeeded", absent is "we never saw a return". With a
	// shed payload these extracted fields are the only surviving evidence, so
	// flattening them into the same JSON output loses a real distinction.
	opts := roomyOpts(t)

	got, err := export.Write([]model.Event{
		{Arrival: time.Now(), Tag: "salt/ok", HasRet: true, RetCode: 0, Success: true, Shed: true},
		{Arrival: time.Now(), Tag: "salt/none", HasRet: false},
	}, opts)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	data, err := os.ReadFile(got.Path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %d:\n%s", len(lines), data)
	}

	if !strings.Contains(lines[0], `"retcode":0`) {
		t.Errorf("a successful return lost its retcode:\n%s", lines[0])
	}

	if strings.Contains(lines[1], `"retcode"`) {
		t.Errorf("an event with no return invented a retcode:\n%s", lines[1])
	}
}

func TestWriteRecordsTheFieldsSpec104Names(t *testing.T) {
	t.Parallel()

	opts := roomyOpts(t)
	opts.Decode = func([]byte) (any, error) { return map[string]any{"fun": "state.apply"}, nil }

	got, err := export.Write([]model.Event{{
		Arrival:   time.Date(2026, 8, 30, 8, 14, 2, 123456000, time.UTC),
		Stamp:     time.Date(2026, 8, 30, 8, 14, 2, 120000000, time.UTC),
		Tag:       "salt/job/20260830081402123456/ret/scache-1",
		Namespace: "job",
		Category:  "salt/job/*/ret/*",
		JID:       "20260830081402123456",
		Minion:    "scache-1",
		Fun:       "state.apply",
		Payload:   []byte("m"),
	}}, opts)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	data, err := os.ReadFile(got.Path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	line := string(data)

	for _, want := range []string{
		`"arrival":"2026-08-30T08:14:02.123456Z"`,
		`"stamp":"2026-08-30T08:14:02.12Z"`,
		`"tag":"salt/job/20260830081402123456/ret/scache-1"`,
		`"namespace":"job"`,
		`"category":"salt/job/*/ret/*"`,
		`"jid":"20260830081402123456"`,
		`"minion":"scache-1"`,
		`"fun":"state.apply"`,
		`"payload":{"fun":"state.apply"}`,
		`"payload_truncated":false`,
		`"master_trimmed":false`,
	} {
		if !strings.Contains(line, want) {
			t.Errorf("missing %s in:\n%s", want, line)
		}
	}

	if strings.Count(line, "\n") != 1 {
		t.Errorf("one event must produce exactly one NDJSON line:\n%s", line)
	}
}

func TestWriteOmitsAnAbsentStamp(t *testing.T) {
	t.Parallel()

	// _stamp may be absent or unparseable (spec §2.4). A zero time rendered as
	// year 1 would read as a real timestamp.
	opts := roomyOpts(t)

	got, err := export.Write([]model.Event{
		{Arrival: time.Date(2026, 8, 30, 8, 14, 2, 0, time.UTC), Tag: "salt/a"},
	}, opts)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	data, err := os.ReadFile(got.Path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	if strings.Contains(string(data), `"stamp"`) {
		t.Errorf("an absent _stamp was fabricated:\n%s", data)
	}
}

func TestWriteReportsEveryEventAndByteItWrote(t *testing.T) {
	t.Parallel()

	opts := roomyOpts(t)

	got, err := export.Write(events(37, 20), opts)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	data, err := os.ReadFile(got.Path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	if n := strings.Count(string(data), "\n"); n != 37 {
		t.Errorf("wrote %d lines, want 37", n)
	}

	if got.Events != 37 {
		t.Errorf("Events = %d, want 37", got.Events)
	}
}

func TestWriteAcceptsAnEmptySelection(t *testing.T) {
	t.Parallel()

	// An over-narrow filter is the operator's problem to see, not a crash.
	opts := roomyOpts(t)

	got, err := export.Write(nil, opts)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	if got.Events != 0 || got.Bytes != 0 {
		t.Errorf("got %+v, want an empty export", got)
	}
}

// --- Estimate -------------------------------------------------------------

func TestEstimateIsDeliberatelyPessimistic(t *testing.T) {
	t.Parallel()

	// Over-estimating costs a declined export; under-estimating costs a full
	// disk on a production master (spec §10.2). Each event carries a 160-byte
	// envelope floor on top of §10.2's tag+payload sum before the 2.0 factor,
	// because the timestamp, the JSON keys and the two truncation flags are
	// real bytes that §10.2's formula does not count.
	tests := []struct {
		name string
		evs  []model.Event
		want int64
	}{
		{name: "nothing selected", evs: nil, want: 0},
		{
			name: "tag plus payload plus the envelope, doubled",
			evs:  []model.Event{{Tag: "salt/a", Payload: bytes.Repeat([]byte("x"), 94)}},
			want: (6 + 94 + 160) * 2,
		},
		{
			name: "a shed payload still costs its tag and its envelope",
			evs:  []model.Event{{Tag: "salt/abcd", Shed: true}},
			want: (9 + 160) * 2,
		},
		{
			name: "sums across events",
			evs:  []model.Event{{Tag: "ab"}, {Tag: "cd"}, {Tag: "ef"}},
			want: 3 * (2 + 160) * 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := export.Estimate(tt.evs); got != tt.want {
				t.Errorf("Estimate = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestEstimateCoversTheBytesActuallyWritten(t *testing.T) {
	t.Parallel()

	// A pre-flight is only worth having if the number it checks bounds the real
	// write. Spec §10.2's bare tag+payload sum under-shot these two shapes by
	// 2.1x and 10.2x measured, because it counts none of the record envelope.
	// Pinned as ">= what was actually written", not as a ratio: the margin is
	// allowed to move, the direction is not.
	shed := make([]model.Event, 0, 1000)

	for range 1000 {
		shed = append(shed, model.Event{
			Arrival: time.Date(2026, 8, 30, 8, 14, 2, 0, time.UTC),
			Tag:     "salt/a",
			Shed:    true,
		})
	}

	tests := []struct {
		name string
		evs  []model.Event
	}{
		{name: "many small events with no payload", evs: events(200, 0)},
		{name: "shed events, where only the tag survives", evs: shed},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			opts := roomyOpts(t)

			est := export.Estimate(tt.evs)

			got, err := export.Write(tt.evs, opts)
			if err != nil {
				t.Fatalf("Write: %v", err)
			}

			info, err := os.Stat(got.Path)
			if err != nil {
				t.Fatalf("Stat: %v", err)
			}

			t.Logf("estimate %d, wrote %d (%.2fx)", est, info.Size(),
				float64(est)/float64(info.Size()))

			if est < info.Size() {
				t.Errorf(
					"Estimate = %d but the export is %d bytes on disk: the pre-flight "+
						"would let this write start and then run out of room",
					est, info.Size())
			}

			if est < got.Bytes {
				t.Errorf("Estimate = %d, but Write reported %d bytes", est, got.Bytes)
			}
		})
	}
}

// --- Destination resolution ------------------------------------------------

func TestResolveDirFollowsSpec101(t *testing.T) {
	t.Parallel()

	homeFor := func(u string) (string, error) {
		if u == "tkcadmin" {
			return "/home/tkcadmin", nil
		}

		return "", errors.New("no such user")
	}

	envFrom := func(m map[string]string) func(string) string {
		return func(k string) string { return m[k] }
	}

	tests := []struct {
		name     string
		explicit string
		env      map[string]string
		want     string
	}{
		{
			name:     "an explicit directory wins over everything",
			explicit: "/srv/exports",
			env:      map[string]string{"SUDO_USER": "tkcadmin", "HOME": "/root"},
			want:     "/srv/exports",
		},
		{
			name: "SUDO_USER's home beats root's HOME",
			env:  map[string]string{"SUDO_USER": "tkcadmin", "HOME": "/root"},
			want: "/home/tkcadmin",
		},
		{
			name: "an unknown SUDO_USER falls through to HOME",
			env:  map[string]string{"SUDO_USER": "ghost", "HOME": "/home/chris"},
			want: "/home/chris",
		},
		{
			name: "HOME is used when there is no sudo",
			env:  map[string]string{"HOME": "/home/chris"},
			want: "/home/chris",
		},
		{
			// /var/tmp, never /tmp: /tmp is frequently tmpfs, so an export
			// there spends RAM on the machine running the master.
			name: "nothing set falls back to /var/tmp",
			env:  map[string]string{},
			want: "/var/tmp",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := export.ResolveDir(tt.explicit, envFrom(tt.env), homeFor)
			if got != tt.want {
				t.Errorf("ResolveDir = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestChownToSudoUserIsANoOpWithoutSudo(t *testing.T) {
	t.Parallel()

	chown := export.ChownToSudoUser(func(string) string { return "" })

	if err := chown("/nonexistent/path"); err != nil {
		t.Errorf("chown without SUDO_USER = %v, want nil", err)
	}
}

func TestChownToSudoUserReportsAnUnknownUser(t *testing.T) {
	t.Parallel()

	chown := export.ChownToSudoUser(func(k string) string {
		if k == "SUDO_USER" {
			return "no-such-user-fbaa1c7e"
		}

		return ""
	})

	if err := chown("/nonexistent/path"); err == nil {
		t.Error("expected an error for an unknown SUDO_USER")
	}
}

// --- The production space checker -----------------------------------------

func TestStatfsCheckerReadsARealFilesystem(t *testing.T) {
	t.Parallel()

	avail, total, err := export.NewStatfsChecker().Available(t.TempDir())
	if err != nil {
		t.Fatalf("Available: %v", err)
	}

	if total <= 0 {
		t.Errorf("total = %d, want a positive filesystem size", total)
	}

	if avail < 0 || avail > total {
		t.Errorf("avail = %d, want 0 <= avail <= total (%d)", avail, total)
	}
}

func TestStatfsCheckerReportsAMissingDirectory(t *testing.T) {
	t.Parallel()

	// "cannot tell" must never be reported as "plenty of room".
	_, _, err := export.NewStatfsChecker().Available(filepath.Join(t.TempDir(), "absent"))
	if err == nil {
		t.Error("expected an error statfs-ing a missing directory")
	}
}

// decodedPayload reads back the payload of the single record in the export
// written by opts, so a test can assert on what encoding/json actually
// produced rather than on the fact that Write returned nil.
func decodedPayload(t *testing.T, path string) map[string]any {
	t.Helper()

	raw, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatalf("read export: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 1 {
		t.Fatalf("want exactly one NDJSON line, got %d", len(lines))
	}

	var rec struct {
		Payload map[string]any `json:"payload"`
	}

	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		t.Fatalf("the export is not valid NDJSON: %v\nline: %s", err, lines[0])
	}

	return rec.Payload
}

// TestWriteRewritesTheDecodersOutputIntoJSONSafeValues is the guarantee this
// package owes every caller: it requires JSON-safe input, so it is the package
// that must produce it.
//
// saltipc.DecodeValue sets DecodeUntypedMap, so EVERY map off the real bus —
// at every nesting level — is a map[interface{}]interface{}, which
// encoding/json refuses outright. A caller that hands over the raw decoder
// must still get a valid export; requiring each caller to remember to wrap it
// is what re-arms this bug on the next one.
func TestWriteRewritesTheDecodersOutputIntoJSONSafeValues(t *testing.T) {
	t.Parallel()

	opts := roomyOpts(t)
	opts.Decode = func([]byte) (any, error) {
		return map[interface{}]interface{}{
			// A non-identifier top-level key, as captured off a live master.
			"pkg_|-install_|-nginx_|-installed": map[interface{}]interface{}{
				"result":  true,
				"changes": map[interface{}]interface{}{"nginx": "1.24"},
			},
			// A non-string key: msgpack permits any type as a map key.
			7: "seven",
			"list": []interface{}{
				"scalar",
				map[interface{}]interface{}{"nested": "deeper"},
			},
		}, nil
	}

	res, err := export.Write(events(1, 10), opts)
	if err != nil {
		t.Fatalf("Write with a real-shaped decoder: %v", err)
	}

	payload := decodedPayload(t, res.Path)

	state, ok := payload["pkg_|-install_|-nginx_|-installed"].(map[string]any)
	if !ok {
		t.Fatalf("the non-identifier top-level key did not round-trip: %#v", payload)
	}

	changes, ok := state["changes"].(map[string]any)
	if !ok || changes["nginx"] != "1.24" {
		t.Errorf("the nested interface-keyed map did not round-trip: %#v", state)
	}

	if payload["7"] != "seven" {
		t.Errorf("a non-string map key did not round-trip as text: %#v", payload)
	}

	list, ok := payload["list"].([]any)
	if !ok || len(list) != 2 {
		t.Fatalf("the list did not round-trip: %#v", payload["list"])
	}

	item, ok := list[1].(map[string]any)
	if !ok || item["nested"] != "deeper" {
		t.Errorf("a map inside a list did not round-trip: %#v", list[1])
	}
}

// TestWriteSurvivesANonFiniteFloat pins the failure that took a whole export
// down: one NaN anywhere in any retained payload made encoding/json refuse the
// record, stream() return on the first error, and writeFile unlink the
// .partial — so the operator got nothing at all, not the other events.
func TestWriteSurvivesANonFiniteFloat(t *testing.T) {
	t.Parallel()

	opts := roomyOpts(t)
	opts.Decode = func([]byte) (any, error) {
		return map[interface{}]interface{}{
			"nan":      math.NaN(),
			"pos_inf":  math.Inf(1),
			"neg_inf":  math.Inf(-1),
			"finite":   1.5,
			"nan32":    float32(math.NaN()),
			"nested":   map[interface{}]interface{}{"also": math.NaN()},
			"in_list":  []interface{}{math.Inf(1)},
			"ordinary": "text",
		}, nil
	}

	res, err := export.Write(events(1, 10), opts)
	if err != nil {
		t.Fatalf("Write with a non-finite float in the payload: %v", err)
	}

	payload := decodedPayload(t, res.Path)

	for key, want := range map[string]any{
		"nan":      "NaN",
		"pos_inf":  "+Inf",
		"neg_inf":  "-Inf",
		"nan32":    "NaN",
		"finite":   1.5,
		"ordinary": "text",
	} {
		if got := payload[key]; got != want {
			t.Errorf("payload[%q] = %#v, want %#v", key, got, want)
		}
	}

	nested, ok := payload["nested"].(map[string]any)
	if !ok || nested["also"] != "NaN" {
		t.Errorf("a non-finite float nested in a map was not neutralised: %#v", payload["nested"])
	}

	list, ok := payload["in_list"].([]any)
	if !ok || len(list) != 1 || list[0] != "+Inf" {
		t.Errorf("a non-finite float inside a list was not neutralised: %#v", payload["in_list"])
	}
}

// TestWriteBoundsHowDeeplyAPayloadMayNest keeps a hostile payload from
// overflowing the goroutine stack. A payload is minion-supplied and can
// legally be a megabyte of nothing but nested one-element maps.
func TestWriteBoundsHowDeeplyAPayloadMayNest(t *testing.T) {
	t.Parallel()

	const depth = 5000

	var deep any = "bottom"
	for range depth {
		deep = map[interface{}]interface{}{"d": deep}
	}

	opts := roomyOpts(t)
	opts.Decode = func([]byte) (any, error) { return deep, nil }

	res, err := export.Write(events(1, 10), opts)
	if err != nil {
		t.Fatalf("Write with a %d-deep payload: %v", depth, err)
	}

	raw, err := os.ReadFile(filepath.Clean(res.Path))
	if err != nil {
		t.Fatalf("read export: %v", err)
	}

	if !strings.Contains(string(raw), "nested too deeply to export") {
		t.Error("a payload past the depth bound must be rendered as a sentinel, not silently truncated")
	}
}
