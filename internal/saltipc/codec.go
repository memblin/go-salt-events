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
	"math"
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
//
// Every indexed scalar field is read via DecodeInterface plus a type
// assertion/switch, never via a type-specific Decode* call (DecodeString,
// DecodeInt64, DecodeBool) with a Skip() or bare discard as the mismatch
// fallback. Those typed calls read the value's type-code byte first and only
// then discover it doesn't match, so by the time a fallback runs, the code
// byte is already consumed and Skip() (or simply moving on) resumes one byte
// into the middle of the value rather than at a value boundary. That desyncs
// the map cursor for the rest of the payload, and because Go map iteration
// order is randomised, an unrelated, perfectly well-formed field encountered
// afterwards is silently dropped too. DecodeInterface decodes whatever value
// is actually present and always consumes it in full, so a field encoded as
// an unexpected type costs exactly that one field and leaves the cursor at
// the next value's boundary. Do not reinstate the typed-decode-then-Skip
// shape here.
func extractOne(dec *msgpack.Decoder, key string, f *Fields) error {
	switch key {
	case "id":
		f.ID = decodeStringField(dec)
	case "jid":
		f.JID = decodeJID(dec)
	case "fun":
		f.Fun = decodeStringField(dec)
	case "retcode":
		if v, ok := decodeIntField(dec); ok {
			f.RetCode = v
		}
	case "success":
		if v, ok := decodeBoolField(dec); ok {
			f.Success, f.HasSuccess = v, true
		}
	case "return":
		f.HasRet = true

		return dec.Skip()
	case "_stamp":
		if s := decodeStringField(dec); s != "" {
			if t, err := time.Parse(stampLayout, s); err == nil {
				f.Stamp = t.UTC()
			}
		}
	default:
		return dec.Skip()
	}

	return nil
}

// decodeStringField reads exactly one value and returns it as a string, or ""
// if it decoded to something else entirely (including a decode error).
func decodeStringField(dec *msgpack.Decoder) string {
	v, err := dec.DecodeInterface()
	if err != nil {
		return ""
	}

	s, _ := v.(string)

	return s
}

// decodeBoolField reads exactly one value and reports whether it was a bool.
func decodeBoolField(dec *msgpack.Decoder) (bool, bool) {
	v, err := dec.DecodeInterface()
	if err != nil {
		return false, false
	}

	b, ok := v.(bool)

	return b, ok
}

// decodeIntField reads exactly one value and reports whether it was some
// integer type. DecodeInterface does not normalise integers to a single Go
// type (spec: fixnum decodes to int8, larger values to the narrowest of
// int16/int32/int64/uint8/uint16/uint32/uint64 that fits the wire encoding),
// so every integer kind it can produce must be handled explicitly.
func decodeIntField(dec *msgpack.Decoder) (int, bool) {
	v, err := dec.DecodeInterface()
	if err != nil {
		return 0, false
	}

	switch t := v.(type) {
	case int8:
		return int(t), true
	case int16:
		return int(t), true
	case int32:
		return int(t), true
	case int64:
		return int(t), true
	case uint8:
		return int(t), true
	case uint16:
		return int(t), true
	case uint32:
		return int(t), true
	case uint64:
		// Bounds-checked: a uint64 above math.MaxInt64 would silently wrap
		// when narrowed to int (gosec G115). retcode never legitimately
		// reaches that range; treat it as an unreadable field instead of
		// risking a corrupted value.
		if t > math.MaxInt64 {
			return 0, false
		}

		return int(t), true
	default:
		return 0, false
	}
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

			v.SetString(decodeConstant(b))

			return nil
		})
}

// decodeConstant renders ext type 79's body for display. Per salt/payload.py,
// a Salt constant is encoded as
//
//	salt.utils.msgpack.dumps((obj.name, obj.value), use_bin_type=True)
//
// i.e. the ext body is itself msgpack: a 2-element array of (name, value),
// not raw text. It renders as "name" when value is nil (the common case, e.g.
// _Constant("MISSING", None)) and "name=value" otherwise, collapsing the pair
// into the single readable string DecodeValue's callers already expect.
//
// This runs while decoding an operator-requested payload (spec §4.2), so a
// malformed body must degrade to a readable placeholder, never panic and
// never fail the whole decode: an unparseable ext-79 field should cost that
// one field, not the rest of the event.
func decodeConstant(b []byte) string {
	dec := msgpack.NewDecoder(bytes.NewReader(b))

	v, err := dec.DecodeInterface()
	if err != nil {
		return fmt.Sprintf("<salt-constant: undecodable: %v>", err)
	}

	tuple, ok := v.([]interface{})
	if !ok || len(tuple) != 2 {
		return fmt.Sprintf("<salt-constant: unexpected shape %v>", v)
	}

	name, ok := tuple[0].(string)
	if !ok {
		return fmt.Sprintf("<salt-constant: non-string name %v>", tuple[0])
	}

	if tuple[1] == nil {
		return name
	}

	return name + "=" + formatConstantValue(tuple[1])
}

// formatConstantValue stringifies a constant's value field. Bin-typed values
// (Python bytes) decode to []byte, which %v would render as an unreadable
// slice of integers, so those are converted to string like any other scalar.
func formatConstantValue(v any) string {
	if b, ok := v.([]byte); ok {
		return string(b)
	}

	return fmt.Sprintf("%v", v)
}
