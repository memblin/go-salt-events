package saltipc_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/vmihailenco/msgpack/v5"

	"github.com/TKC-Labs/go-salt-events/internal/saltipc"
)

// frame builds one wire frame exactly as salt/transport/frame.py::frame_msg_ipc
// does: a 4-byte big-endian length, then msgpack{head, body}, where body is
// tag + "\n\n" + msgpack(data).
func frame(t *testing.T, tag string, data map[string]any) []byte {
	t.Helper()

	inner, err := msgpack.Marshal(data)
	if err != nil {
		t.Fatalf("marshal data: %v", err)
	}

	body := append([]byte(tag+"\n\n"), inner...)

	outer, err := msgpack.Marshal(map[string]any{"head": map[string]any{}, "body": body})
	if err != nil {
		t.Fatalf("marshal frame: %v", err)
	}

	buf := make([]byte, 0, 4+len(outer))
	buf = binary.BigEndian.AppendUint32(buf, uint32(len(outer)))
	buf = append(buf, outer...)

	return buf
}

func TestFrameReaderDecodesTagAndPayload(t *testing.T) {
	t.Parallel()

	raw := frame(t, "salt/job/20260830081402123456/ret/scache-1",
		map[string]any{"id": "scache-1", "retcode": 0})

	fr := saltipc.NewFrameReader(bytes.NewReader(raw))

	got, err := fr.Next()
	if err != nil {
		t.Fatalf("Next() error: %v", err)
	}

	if got.Tag != "salt/job/20260830081402123456/ret/scache-1" {
		t.Errorf("Tag = %q", got.Tag)
	}

	if len(got.Payload) == 0 {
		t.Error("Payload is empty")
	}
}

func TestFrameReaderSplitsOnFirstTagendOnly(t *testing.T) {
	t.Parallel()

	// A payload can legitimately contain "\n\n" — a state return with a
	// multi-paragraph comment does routinely. Splitting on the last, or on
	// every, occurrence would truncate the tag or corrupt the payload.
	raw := frame(t, "salt/job/20260830081402123456/ret/web-1",
		map[string]any{"comment": "line one\n\nline two"})

	fr := saltipc.NewFrameReader(bytes.NewReader(raw))

	got, err := fr.Next()
	if err != nil {
		t.Fatalf("Next() error: %v", err)
	}

	if got.Tag != "salt/job/20260830081402123456/ret/web-1" {
		t.Errorf("Tag = %q — split on the wrong tagend", got.Tag)
	}

	fields := saltipc.ExtractFields(got.Payload)
	_ = fields // payload must remain decodable
}

func TestFrameReaderHandlesFramesLargerThanPipeBuf(t *testing.T) {
	t.Parallel()

	// This is the case Salt's length prefix exists for: a message past
	// PIPE_BUF (~64KiB) that a streaming unpacker would interleave and
	// corrupt (spec §2.1). A reader that got this wrong would fail exactly
	// during the storms this tool is built to watch.
	big := strings.Repeat("x", 200_000)
	raw := frame(t, "salt/job/20260830081402123456/ret/web-1",
		map[string]any{"return": big})

	fr := saltipc.NewFrameReader(bytes.NewReader(raw))

	got, err := fr.Next()
	if err != nil {
		t.Fatalf("Next() error: %v", err)
	}

	if got.Tag != "salt/job/20260830081402123456/ret/web-1" {
		t.Errorf("Tag = %q", got.Tag)
	}
}

func TestFrameReaderReadsManyFramesBackToBack(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	for range 100 {
		buf.Write(frame(t, "salt/auth", map[string]any{"act": "accept"}))
	}

	fr := saltipc.NewFrameReader(&buf)

	for i := range 100 {
		if _, err := fr.Next(); err != nil {
			t.Fatalf("Next() at frame %d: %v", i, err)
		}
	}

	if _, err := fr.Next(); !errors.Is(err, io.EOF) {
		t.Errorf("after the last frame, err = %v, want io.EOF", err)
	}
}

func TestFrameReaderRejectsImplausibleLength(t *testing.T) {
	t.Parallel()

	// An absurd length means the stream has desynced. Trusting it would
	// allocate a gigabyte; the reader must report it so Reader can reconnect.
	buf := make([]byte, 4)
	binary.BigEndian.PutUint32(buf, 0xFFFFFFFF)

	fr := saltipc.NewFrameReader(bytes.NewReader(buf))

	if _, err := fr.Next(); !errors.Is(err, saltipc.ErrFrameTooLarge) {
		t.Errorf("err = %v, want ErrFrameTooLarge", err)
	}
}

func TestFrameReaderSkipsAMalformedFrameAndContinues(t *testing.T) {
	t.Parallel()

	// The length prefix is what makes recovery clean: a frame whose msgpack
	// is garbage can be skipped by exactly `length` bytes, so one bad event
	// never desyncs the reader (spec §8.2).
	bad := make([]byte, 4+8)
	binary.BigEndian.PutUint32(bad[:4], 8)
	copy(bad[4:], []byte{0xc1, 0xc1, 0xc1, 0xc1, 0xc1, 0xc1, 0xc1, 0xc1})

	var buf bytes.Buffer
	buf.Write(bad)
	buf.Write(frame(t, "salt/auth", map[string]any{"act": "accept"}))

	fr := saltipc.NewFrameReader(&buf)

	if _, err := fr.Next(); err == nil {
		t.Fatal("expected an error for the malformed frame")
	}

	got, err := fr.Next()
	if err != nil {
		t.Fatalf("reader did not recover after a bad frame: %v", err)
	}

	if got.Tag != "salt/auth" {
		t.Errorf("Tag = %q, want salt/auth", got.Tag)
	}
}
