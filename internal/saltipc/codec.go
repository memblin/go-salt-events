// Package saltipc reads the Salt master event bus: the Unix socket, Salt's
// length-prefixed framing, and the msgpack payloads inside it.
//
// It must never import bubbletea, bubbles, or lipgloss (spec §3.1) — that is
// what keeps it testable without a terminal, and what lets the future exporter
// reuse it unchanged.
package saltipc

import (
	"bytes"
	"fmt"
	"io"
	"reflect"
	"strconv"
	"time"

	"github.com/vmihailenco/msgpack/v5"
)

// TrimmedMarker is the literal Salt's dicttrim substitutes for a value that
// pushed an event past max_event_size (spec §2.5). Seeing it means the data
// was destroyed at the master, before we could have kept it.
const TrimmedMarker = "VALUE_TRIMMED"

// stampLayout is _stamp's format: datetime.utcnow().isoformat() — naive UTC,
// no timezone suffix (spec §2.4).
const stampLayout = "2006-01-02T15:04:05.999999"

// extDatetimeLayout is msgpack ext type 78's format: obj.strftime("%Y%m%dT%H:%M:%S.%f").
const extDatetimeLayout = "20060102T15:04:05.999999"

const (
	extDatetime = 78
	extConstant = 79
)

// Fields are the values ingest extracts eagerly. Everything else stays as raw
// msgpack until the Detail pane asks for it (spec §4.2, invariant 4).
type Fields struct {
	ID      string
	JID     string
	Fun     string
	RetCode int

	Success    bool
	HasSuccess bool
	HasRet     bool

	Stamp time.Time
}

// ExtractFields decodes only the top-level keys the indexes need, skipping
// every other value without materialising it. At thousands of events per
// second this is the difference between comfortable and not — and almost all
// of the skipped work would be wasted, since an operator reads a handful of
// payloads per session.
//
// It never returns an error: a payload it cannot read yields zero fields, and
// the event is still cached and counted for its tag. Never panic on bus data.
func ExtractFields(payload []byte) Fields {
	var f Fields

	if len(payload) == 0 {
		return f
	}

	dec := msgpack.NewDecoder(bytes.NewReader(payload))

	n, err := dec.DecodeMapLen()
	if err != nil || n < 0 {
		return f
	}

	for range n {
		key, err := dec.DecodeString()
		if err != nil {
			return f
		}

		if err := extractOne(dec, key, &f); err != nil {
			return f
		}
	}

	return f
}

// extractOne reads or skips the value for one key.
func extractOne(dec *msgpack.Decoder, key string, f *Fields) error {
	switch key {
	case "id":
		f.ID, _ = dec.DecodeString()
	case "jid":
		f.JID = decodeJID(dec)
	case "fun":
		f.Fun, _ = dec.DecodeString()
	case "retcode":
		v, err := dec.DecodeInt64()
		if err != nil {
			return dec.Skip()
		}

		f.RetCode = int(v)
	case "success":
		v, err := dec.DecodeBool()
		if err != nil {
			return dec.Skip()
		}

		f.Success, f.HasSuccess = v, true
	case "return":
		f.HasRet = true

		return dec.Skip()
	case "_stamp":
		s, err := dec.DecodeString()
		if err != nil {
			// Unreadable _stamp is tolerated, not fatal: the field is just left zero.
			return nil //nolint:nilerr // intentional: a bad _stamp doesn't abort the whole extraction
		}

		if t, err := time.Parse(stampLayout, s); err == nil {
			f.Stamp = t.UTC()
		}
	default:
		return dec.Skip()
	}

	return nil
}

// decodeJID normalises a JID that may arrive as a string or an integer.
// salt/payload.py stringifies very long ints, but shorter ones stay numeric.
func decodeJID(dec *msgpack.Decoder) string {
	v, err := dec.DecodeInterface()
	if err != nil {
		return ""
	}

	switch t := v.(type) {
	case string:
		return t
	case int64:
		return strconv.FormatInt(t, 10)
	case uint64:
		return strconv.FormatUint(t, 10)
	default:
		return ""
	}
}

// IsMasterTrimmed reports whether Salt gutted this event at the master.
//
// This is a byte scan rather than a decode on purpose: it runs on every event
// at ingest, and decoding to answer it would defeat the point of ExtractFields.
// A false positive requires an event to legitimately contain the exact string
// VALUE_TRIMMED, which is acceptable — the UI's claim is "the master reports
// trimming here", and that is what the marker means.
func IsMasterTrimmed(payload []byte) bool {
	return bytes.Contains(payload, []byte(TrimmedMarker))
}

// DecodeValue fully decodes a payload for display. This is the ONLY full
// decode in the program, and it runs once per event the operator actually
// opens (spec §4.2).
func DecodeValue(payload []byte) (any, error) {
	dec := msgpack.NewDecoder(bytes.NewReader(payload))
	dec.SetMapDecoder(func(d *msgpack.Decoder) (any, error) {
		return d.DecodeUntypedMap()
	})

	v, err := dec.DecodeInterface()
	if err != nil {
		return nil, fmt.Errorf("decode payload: %w", err)
	}

	return v, nil
}

// init registers decoders for Salt's two msgpack extension types. Ext-decoder
// registration in vmihailenco/msgpack/v5 is process-global (there is no
// per-Decoder variant), so this runs once at package load rather than per
// call to DecodeValue. Without it a payload containing a datetime or a Salt
// constant fails to decode entirely, taking the whole Detail view with it.
func init() {
	msgpack.RegisterExtDecoder(extDatetime, time.Time{},
		func(d *msgpack.Decoder, v reflect.Value, extLen int) error {
			b := make([]byte, extLen)
			if _, err := io.ReadFull(d.Buffered(), b); err != nil {
				return fmt.Errorf("read ext 78: %w", err)
			}

			t, err := time.Parse(extDatetimeLayout, string(b))
			if err != nil {
				return fmt.Errorf("parse ext 78 %q: %w", b, err)
			}

			v.Set(reflect.ValueOf(t.UTC()))

			return nil
		})

	msgpack.RegisterExtDecoder(extConstant, "",
		func(d *msgpack.Decoder, v reflect.Value, extLen int) error {
			b := make([]byte, extLen)
			if _, err := io.ReadFull(d.Buffered(), b); err != nil {
				return fmt.Errorf("read ext 79: %w", err)
			}

			v.SetString(string(b))

			return nil
		})
}
