package saltipc_test

import (
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
