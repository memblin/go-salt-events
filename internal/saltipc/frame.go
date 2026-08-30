package saltipc

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/vmihailenco/msgpack/v5"
)

// MaxFrameSize bounds a single frame. Salt's own max_event_size is 1 MiB, so
// anything approaching this means the stream has desynced rather than that an
// event is genuinely enormous.
const MaxFrameSize = 64 << 20

// ErrFrameTooLarge signals a length prefix that cannot be real. The caller
// reconnects rather than trying to resynchronise mid-stream.
var ErrFrameTooLarge = errors.New("frame length implausible; stream desynced")

// tagend is Salt's TAGEND (spec §2.2).
var tagend = []byte("\n\n")

// Frame is one decoded event off the wire.
type Frame struct {
	Tag string

	// Payload is the raw msgpack event data, still undecoded (invariant 4).
	Payload []byte
}

// FrameReader decodes Salt's IPC framing from a stream.
//
// The format is [uint32 big-endian length][msgpack{head, body}]. Salt moved to
// this from a streaming msgpack Unpacker specifically because concurrent
// writes past PIPE_BUF interleaved on the Unix socket and corrupted the stream
// (spec §2.1) — so a streaming decoder here would fail on exactly the
// high-throughput bursts this tool exists to observe.
type FrameReader struct {
	br  *bufio.Reader
	hdr [4]byte
}

// NewFrameReader wraps r.
func NewFrameReader(r io.Reader) *FrameReader {
	const bufSize = 1 << 16

	return &FrameReader{br: bufio.NewReaderSize(r, bufSize)}
}

// Next reads one frame.
//
// On a decode failure it has already consumed exactly `length` bytes, so the
// caller may simply call Next again: one malformed event does not desync the
// reader. The two errors that are NOT recoverable are io.EOF and
// ErrFrameTooLarge.
func (f *FrameReader) Next() (Frame, error) {
	if _, err := io.ReadFull(f.br, f.hdr[:]); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return Frame{}, io.EOF
		}

		return Frame{}, fmt.Errorf("read length prefix: %w", err)
	}

	length := binary.BigEndian.Uint32(f.hdr[:])
	if length == 0 || length > MaxFrameSize {
		return Frame{}, fmt.Errorf("%w: %d bytes", ErrFrameTooLarge, length)
	}

	buf := make([]byte, length)
	if _, err := io.ReadFull(f.br, buf); err != nil {
		return Frame{}, fmt.Errorf("read frame body: %w", err)
	}

	return decodeFrame(buf)
}

// framed mirrors salt.transport.frame.frame_msg_ipc's structure.
type framed struct {
	Body []byte `msgpack:"body"`
}

// decodeFrame turns one frame's msgpack into a Frame.
func decodeFrame(buf []byte) (Frame, error) {
	var fr framed
	if err := msgpack.Unmarshal(buf, &fr); err != nil {
		return Frame{}, fmt.Errorf("decode frame: %w", err)
	}

	// Split on the FIRST tagend only. Payloads legitimately contain "\n\n" —
	// a state return with a multi-paragraph comment does routinely — and
	// splitting anywhere else truncates the tag or corrupts the payload.
	idx := bytes.Index(fr.Body, tagend)
	if idx < 0 {
		return Frame{}, errors.New("frame body has no tag delimiter")
	}

	return Frame{
		Tag:     string(fr.Body[:idx]),
		Payload: fr.Body[idx+len(tagend):],
	}, nil
}
