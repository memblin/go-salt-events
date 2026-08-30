package saltipc_test

import (
	"bytes"
	"errors"
	"io"
	"os"
	"regexp"
	"testing"

	"github.com/TKC-Labs/go-salt-events/internal/saltipc"
)

// liveFramesPath holds raw bytes captured verbatim off a real Salt 3006.27
// master's master_event_pub.ipc socket — no reformatting, no hand-editing.
//
// Every other test in this package builds its frames with the same
// understanding of the format that frame.go decodes them with, so they can only
// ever prove we are self-consistent. This fixture is the one thing in the suite
// that can prove we are *right*: if Salt's framing, its head/body keys, its
// TAGEND, or its integer widths are not what spec §2.1–§2.4 claims, these
// bytes disagree and these tests fail.
const liveFramesPath = "testdata/live-frames.bin"

// liveFrameCount is the number of frames in the fixture. Pinned so that a
// truncated or partially-rewritten fixture fails loudly rather than silently
// testing fewer frames than it appears to.
const liveFrameCount = 32

// tag shapes observed on a real bus. Note that jobRetTag is NOT simply
// "everything after the jid": the minion id can itself contain dots and dashes.
var (
	// bareJIDTag has no "salt/" prefix and no slashes at all. The master
	// publishes the minion list for a job to the CLI on a tag that is just the
	// jid, so any tag parser assuming a "salt/<category>/..." shape is wrong.
	bareJIDTag = regexp.MustCompile(`^\d{20}$`)

	jobNewTag        = regexp.MustCompile(`^salt/job/\d+/new$`)
	jobRetTag        = regexp.MustCompile(`^salt/job/\d+/ret/.+$`)
	runNewTag        = regexp.MustCompile(`^salt/run/\d+/new$`)
	runRetTag        = regexp.MustCompile(`^salt/run/\d+/ret$`)
	minionRefreshTag = regexp.MustCompile(`^minion/refresh/.+$`)
)

// loadLiveFrames decodes the whole fixture through the real FrameReader.
//
// It skips rather than fails when the fixture is absent so the suite still runs
// for anyone who has not got it — but a fixture that is present and undecodable
// is a hard failure, because that is exactly the regression it exists to catch.
func loadLiveFrames(t *testing.T) []saltipc.Frame {
	t.Helper()

	raw, err := os.ReadFile(liveFramesPath)
	if err != nil {
		t.Skipf("live capture fixture not present at %s (%v); "+
			"regenerate it by capturing master_event_pub.ipc on a real Salt master",
			liveFramesPath, err)
	}

	fr := saltipc.NewFrameReader(bytes.NewReader(raw))

	var frames []saltipc.Frame

	for {
		f, err := fr.Next()
		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			t.Fatalf("frame %d of the live capture failed to decode: %v", len(frames), err)
		}

		frames = append(frames, f)
	}

	return frames
}

// TestLiveCaptureDecodesEveryFrame is the headline claim: real bytes, real
// decoder, nothing left over at the end of the stream.
func TestLiveCaptureDecodesEveryFrame(t *testing.T) {
	t.Parallel()

	frames := loadLiveFrames(t)

	if len(frames) != liveFrameCount {
		t.Fatalf("decoded %d frames, want %d — fixture truncated or replaced",
			len(frames), liveFrameCount)
	}

	for i, f := range frames {
		if f.Tag == "" {
			t.Errorf("frame %d has an empty tag", i)
		}

		// A real event always carries _stamp. If ExtractFields cannot read it,
		// either the map cursor desynced or the layout is wrong — both are the
		// kind of silent corruption this fixture exists to surface.
		if fields := saltipc.ExtractFields(f.Payload); fields.Stamp.IsZero() {
			t.Errorf("frame %d (tag %q): _stamp did not parse", i, f.Tag)
		}
	}
}

// TestLiveCaptureTagShapes pins every tag shape a real master emits, and the
// fields ExtractFields must recover from each. The counts are what a run of
// test.ping/test.version across four minions, a runner, a test.sleep and one
// failing state.apply actually produced.
func TestLiveCaptureTagShapes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		pattern   *regexp.Regexp
		wantCount int
		wantJID   bool
		wantID    bool
		wantFun   bool
		wantRet   bool
	}{{
		// Payload is just {_stamp, minions} — the jid lives only in the tag,
		// so ExtractFields legitimately recovers nothing from the body.
		name:      "bare jid (master to CLI minion list)",
		pattern:   bareJIDTag,
		wantCount: 6,
	}, {
		name:      "job publish",
		pattern:   jobNewTag,
		wantCount: 6,
		wantJID:   true,
		wantFun:   true,
	}, {
		name:      "job return per minion",
		pattern:   jobRetTag,
		wantCount: 17,
		wantJID:   true,
		wantID:    true,
		wantFun:   true,
		wantRet:   true,
	}, {
		name:      "runner publish",
		pattern:   runNewTag,
		wantCount: 1,
		wantJID:   true,
		wantFun:   true,
	}, {
		name:      "runner return",
		pattern:   runRetTag,
		wantCount: 1,
		wantJID:   true,
		wantFun:   true,
		wantRet:   true,
	}, {
		// Payload's only non-_stamp key is the literal string
		// "Minion data cache refresh" — a top-level key containing spaces.
		name:      "minion data cache refresh",
		pattern:   minionRefreshTag,
		wantCount: 1,
	}}

	frames := loadLiveFrames(t)

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var got int

			for _, f := range frames {
				if !tc.pattern.MatchString(f.Tag) {
					continue
				}

				got++

				fields := saltipc.ExtractFields(f.Payload)

				if (fields.JID != "") != tc.wantJID {
					t.Errorf("tag %q: JID = %q, wanted present=%v", f.Tag, fields.JID, tc.wantJID)
				}

				if (fields.ID != "") != tc.wantID {
					t.Errorf("tag %q: ID = %q, wanted present=%v", f.Tag, fields.ID, tc.wantID)
				}

				if (fields.Fun != "") != tc.wantFun {
					t.Errorf("tag %q: Fun = %q, wanted present=%v", f.Tag, fields.Fun, tc.wantFun)
				}

				if fields.HasRet != tc.wantRet {
					t.Errorf("tag %q: HasRet = %v, want %v", f.Tag, fields.HasRet, tc.wantRet)
				}
			}

			if got != tc.wantCount {
				t.Errorf("matched %d frames, want %d", got, tc.wantCount)
			}
		})
	}
}

// TestLiveCaptureEveryTagIsClassified guards the shape table above: if a real
// master emits a tag none of those patterns match, the table is incomplete and
// the reader of this fixture is being told less than it thinks.
func TestLiveCaptureEveryTagIsClassified(t *testing.T) {
	t.Parallel()

	known := []*regexp.Regexp{
		bareJIDTag, jobNewTag, jobRetTag, runNewTag, runRetTag, minionRefreshTag,
	}

	for _, f := range loadLiveFrames(t) {
		matched := false

		for _, re := range known {
			if re.MatchString(f.Tag) {
				matched = true

				break
			}
		}

		if !matched {
			t.Errorf("tag %q matches no known shape", f.Tag)
		}
	}
}

// TestLiveCaptureFailedJobReturn pins the one non-zero retcode in the capture,
// from a state.apply of a state that does not exist.
//
// retcode arrives from Salt as a msgpack fixnum, which DecodeInterface yields
// as int8 — not int64. That is precisely the case decodeIntField's type switch
// exists for, and this is the only test that proves it against real bytes.
func TestLiveCaptureFailedJobReturn(t *testing.T) {
	t.Parallel()

	var failures int

	for _, f := range loadLiveFrames(t) {
		fields := saltipc.ExtractFields(f.Payload)
		if fields.RetCode == 0 {
			continue
		}

		failures++

		if fields.RetCode != 1 {
			t.Errorf("tag %q: RetCode = %d, want 1", f.Tag, fields.RetCode)
		}

		if !fields.HasSuccess || fields.Success {
			t.Errorf("tag %q: Success = %v (has=%v), want false",
				f.Tag, fields.Success, fields.HasSuccess)
		}

		if fields.Fun != "state.apply" {
			t.Errorf("tag %q: Fun = %q, want state.apply", f.Tag, fields.Fun)
		}
	}

	if failures != 1 {
		t.Errorf("found %d non-zero retcodes, want 1", failures)
	}
}

// TestLiveCaptureIsNotMasterTrimmed records that nothing in this capture came
// close to max_event_size. If a regenerated fixture ever does trip Salt's
// dicttrim, that changes what the other assertions here mean.
func TestLiveCaptureIsNotMasterTrimmed(t *testing.T) {
	t.Parallel()

	for _, f := range loadLiveFrames(t) {
		if saltipc.IsMasterTrimmed(f.Payload) {
			t.Errorf("tag %q reports VALUE_TRIMMED; the fixture is no longer an "+
				"untrimmed sample", f.Tag)
		}
	}
}
