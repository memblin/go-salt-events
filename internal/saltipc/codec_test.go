package saltipc_test

import (
	"bytes"
	"testing"
	"time"

	"github.com/vmihailenco/msgpack/v5"

	"github.com/TKC-Labs/go-salt-events/internal/saltipc"
)

// pack is a test helper that builds a payload the way Salt does.
func pack(t *testing.T, m map[string]any) []byte {
	t.Helper()

	b, err := msgpack.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	return b
}

func TestExtractFieldsReadsIndexedFieldsOnly(t *testing.T) {
	t.Parallel()

	payload := pack(t, map[string]any{
		"id":      "scache-1",
		"jid":     "20260830081402123456",
		"fun":     "state.apply",
		"retcode": 1,
		"success": false,
		"return":  map[string]any{"big": "value"},
		"_stamp":  "2026-08-30T08:14:02.123456",
	})

	got := saltipc.ExtractFields(payload)

	if got.ID != "scache-1" {
		t.Errorf("ID = %q, want scache-1", got.ID)
	}

	if got.JID != "20260830081402123456" {
		t.Errorf("JID = %q, want 20260830081402123456", got.JID)
	}

	if got.Fun != "state.apply" {
		t.Errorf("Fun = %q, want state.apply", got.Fun)
	}

	if got.RetCode != 1 {
		t.Errorf("RetCode = %d, want 1", got.RetCode)
	}

	if got.Success || !got.HasSuccess {
		t.Errorf("Success = %v HasSuccess = %v, want false/true", got.Success, got.HasSuccess)
	}

	if !got.HasRet {
		t.Error("HasRet = false, want true")
	}

	want := time.Date(2026, 8, 30, 8, 14, 2, 123456000, time.UTC)
	if !got.Stamp.Equal(want) {
		t.Errorf("Stamp = %v, want %v", got.Stamp, want)
	}
}

func TestExtractFieldsHandlesJIDAsIntegerOrString(t *testing.T) {
	t.Parallel()

	// salt/payload.py::ext_type_encoder stringifies very long ints, but a JID
	// that fits in an int64 arrives as an integer. Both must normalise.
	for _, tc := range []struct {
		name string
		jid  any
	}{
		{"string", "20260830081402123456"},
		{"integer", int64(20260830081402)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := saltipc.ExtractFields(pack(t, map[string]any{"jid": tc.jid}))
			if got.JID == "" {
				t.Error("JID is empty; want a normalised string")
			}
		})
	}
}

func TestExtractFieldsToleratesGarbage(t *testing.T) {
	t.Parallel()

	// Never panic on bus data. A payload we cannot read yields zero fields,
	// and the event is still cached and counted for its tag.
	for _, b := range [][]byte{nil, {}, {0xc1}, {0x81, 0xa2, 'i', 'd'}} {
		_ = saltipc.ExtractFields(b)
	}
}

func TestDecodeValueHandlesSaltExtTypes(t *testing.T) {
	t.Parallel()

	// Ext 78 is a datetime encoded as %Y%m%dT%H:%M:%S.%f (spec §2.3).
	ext78, err := msgpack.Marshal(map[string]any{
		"when": msgpack.RawMessage(append(
			[]byte{0xc7, 22, 78}, []byte("20260830T08:14:02.123456")[:22]...,
		)),
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	if _, err := saltipc.DecodeValue(ext78); err != nil {
		t.Errorf("DecodeValue on ext 78 returned error: %v", err)
	}
}

// extBytes wraps a raw ext body in msgpack's ext8 header (0xc7, len, type).
// ext8 accepts any length 0-255, including ones a fixext size would also
// cover, so it is a safe, uniform choice for hand-built test payloads.
func extBytes(typ byte, body []byte) []byte {
	return append([]byte{0xc7, byte(len(body)), typ}, body...)
}

// TestDecodeValueHandlesExt79SaltConstant covers ext type 79, whose body is
// itself msgpack: a (name, value) 2-tuple built by Salt's own
// salt.utils.msgpack.dumps((obj.name, obj.value), use_bin_type=True) (see
// salt/payload.py). The two well-formed byte sequences below were produced
// by Salt 3006.27's real encoder, not by anything on our side of the wire —
// the whole point of this test is that our previous assumption (the ext
// body is raw UTF-8 text) was wrong.
func TestDecodeValueHandlesExt79SaltConstant(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		body []byte
		want string
	}{
		{
			// _Constant("MISSING", None): fixarray(2), fixstr(7) "MISSING", nil.
			name: "nil value renders as bare name",
			body: []byte{0x92, 0xa7, 'M', 'I', 'S', 'S', 'I', 'N', 'G', 0xc0},
			want: "MISSING",
		},
		{
			// _Constant("NOT_SET", 42): fixarray(2), fixstr(7) "NOT_SET", fixint 42.
			name: "integer value renders as name=value",
			body: []byte{0x92, 0xa7, 'N', 'O', 'T', '_', 'S', 'E', 'T', 0x2a},
			want: "NOT_SET=42",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			payload := pack(t, map[string]any{
				"const": msgpack.RawMessage(extBytes(79, tc.body)),
			})

			got, err := saltipc.DecodeValue(payload)
			if err != nil {
				t.Fatalf("DecodeValue: %v", err)
			}

			m, ok := got.(map[any]any)
			if !ok {
				t.Fatalf("DecodeValue returned %T, want map[any]any", got)
			}

			if m["const"] != tc.want {
				t.Errorf("const = %v, want %q", m["const"], tc.want)
			}
		})
	}
}

// TestDecodeValueToleratesMalformedExt79 covers ext-79 bodies that aren't a
// well-formed (name, value) tuple. Never panic on bus data: a malformed
// field must degrade to a readable placeholder and must not fail the whole
// decode.
func TestDecodeValueToleratesMalformedExt79(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		body []byte
	}{
		{"not an array", []byte{0x2a}},           // fixint 42
		{"wrong arity", []byte{0x91, 0xa1, 'x'}}, // fixarray(1), "x"
		{"undecodable", []byte{0xc1}},            // reserved/never-used code
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			payload := pack(t, map[string]any{
				"const": msgpack.RawMessage(extBytes(79, tc.body)),
			})

			got, err := saltipc.DecodeValue(payload)
			if err != nil {
				t.Fatalf("DecodeValue returned error, want graceful degradation: %v", err)
			}

			m, ok := got.(map[any]any)
			if !ok {
				t.Fatalf("DecodeValue returned %T, want map[any]any", got)
			}

			s, ok := m["const"].(string)
			if !ok || s == "" {
				t.Errorf("const = %#v, want a non-empty placeholder string", m["const"])
			}
		})
	}
}

func TestExtractFieldsSurvivesTypeMismatchOnEarlierField(t *testing.T) {
	t.Parallel()

	// retcode arrives as a string (a type mismatch), immediately followed by
	// a well-formed fun. A typed-decode-then-Skip fallback for retcode (e.g.
	// dec.DecodeInt64() failing, then dec.Skip()) would consume retcode's
	// type-code byte before discovering the mismatch, then Skip() from one
	// byte into the value — desyncing the map cursor so the perfectly
	// well-formed fun that follows is silently lost too.
	//
	// Key order is pinned explicitly via the Encoder, rather than built from
	// a Go map (whose iteration order — and therefore wire order, since
	// msgpack.Marshal does not sort map keys by default — is randomised), so
	// the trigger is deterministic regardless of run.
	var buf bytes.Buffer

	enc := msgpack.NewEncoder(&buf)
	if err := enc.EncodeMapLen(2); err != nil {
		t.Fatalf("EncodeMapLen: %v", err)
	}

	for _, kv := range []string{"retcode", "hello", "fun", "state.apply"} {
		if err := enc.EncodeString(kv); err != nil {
			t.Fatalf("EncodeString(%q): %v", kv, err)
		}
	}

	got := saltipc.ExtractFields(buf.Bytes())

	if got.Fun != "state.apply" {
		t.Errorf("Fun = %q, want state.apply (a type-mismatched retcode must not corrupt fields decoded after it)",
			got.Fun)
	}
}

func TestIsMasterTrimmed(t *testing.T) {
	t.Parallel()

	trimmed := pack(t, map[string]any{"return": "VALUE_TRIMMED"})
	if !saltipc.IsMasterTrimmed(trimmed) {
		t.Error("IsMasterTrimmed = false for a trimmed payload, want true")
	}

	clean := pack(t, map[string]any{"return": "ok"})
	if saltipc.IsMasterTrimmed(clean) {
		t.Error("IsMasterTrimmed = true for a clean payload, want false")
	}
}
