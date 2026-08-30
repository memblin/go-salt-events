package export_test

import (
	"bytes"
	"errors"
	"fmt"
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

	// The dangerous case is a failure AFTER real bytes are on disk: the events
	// here are far larger than the write buffer, so the first ones are flushed
	// to the .partial before the failure lands.
	opts := roomyOpts(t)

	calls := 0
	opts.Decode = func(b []byte) (any, error) {
		calls++
		if calls > 3 {
			return nil, errors.New("decode blew up mid-export")
		}

		return map[string]any{"len": len(b)}, nil
	}

	if _, err := export.Write(events(10, 64<<10), opts); err == nil {
		t.Fatal("expected an error")
	}

	assertDirIsEmpty(t, opts.Dir)
}

func TestWriteAbortsMidStreamOnTheHardCap(t *testing.T) {
	t.Parallel()

	// The estimate can be wrong in the safe-looking direction — tiny payloads
	// expand into fat JSON records. The cap has to bound the actual write, not
	// just the guess, and abort the same way ENOSPC does (spec §10.3).
	evs := events(200, 0)

	opts := roomyOpts(t)
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
	// disk on a production master (spec §10.2).
	tests := []struct {
		name string
		evs  []model.Event
		want int64
	}{
		{name: "nothing selected", evs: nil, want: 0},
		{
			name: "tag plus payload, doubled",
			evs:  []model.Event{{Tag: "salt/a", Payload: bytes.Repeat([]byte("x"), 94)}},
			want: 200,
		},
		{
			name: "a shed payload still costs its tag",
			evs:  []model.Event{{Tag: "salt/abcd", Shed: true}},
			want: 18,
		},
		{
			name: "sums across events",
			evs:  []model.Event{{Tag: "ab"}, {Tag: "cd"}, {Tag: "ef"}},
			want: 12,
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
