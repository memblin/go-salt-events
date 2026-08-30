package export

import (
	"fmt"
	"math"
	"strconv"
)

// maxJSONDepth bounds how deep jsonSafe recurses.
//
// A payload is minion-supplied and can legally be a megabyte of nothing but
// nested one-element maps, which is hundreds of thousands of levels — enough to
// overflow the goroutine stack. "Never panic on bus data" makes this a bound,
// not a comment. It matches internal/ui/detail's identical bound, for the same
// reason.
const maxJSONDepth = 32

// jsonSafe rewrites a decoded payload into something encoding/json can marshal.
//
// # Why this lives HERE and not at the call site
//
// This package requires JSON-safe input — that is what "writes NDJSON" means —
// so the guarantee belongs in the package that requires it, where it is correct
// for every caller and impossible to forget. It was originally applied by the
// wiring layer around the injected decoder, which worked and was load-bearing,
// but left the next caller of Write free to hand over a raw decoder and re-arm
// the bug.
//
// It is applied to the RESULT of the injected Options.Decode rather than by
// decoding here: this package must never learn the wire format (spec §3.1,
// enforced by depguard), so it takes the decoder and neutralises what comes
// back.
//
// # What it neutralises, and why each one is not hypothetical
//
//   - map[interface{}]interface{}. saltipc.DecodeValue sets DecodeUntypedMap,
//     so EVERY map off the bus — at every nesting level — has interface keys,
//     and encoding/json refuses that type outright. This is the same bug class
//     that has now been found three times in this project.
//   - Non-finite floats. msgpack carries NaN and ±Inf happily; encoding/json
//     refuses them, and stream() abandons the whole export on the first
//     refusal, so ONE arbitrary float anywhere in any retained payload used to
//     cost the operator every other event too.
//   - Depth. See maxJSONDepth.
//
// Keys are stringified rather than asserted to string: msgpack permits any type
// as a map key, and one event captured off a live master carried a top-level
// key with spaces in it, so nothing here may assume an identifier shape.
func jsonSafe(v any, depth int) any {
	if depth > maxJSONDepth {
		return "…(nested too deeply to export)"
	}

	switch t := v.(type) {
	case map[interface{}]interface{}:
		out := make(map[string]any, len(t))
		for key, val := range t {
			out[fmt.Sprint(key)] = jsonSafe(val, depth+1)
		}

		return out

	case map[string]interface{}:
		out := make(map[string]any, len(t))
		for key, val := range t {
			out[key] = jsonSafe(val, depth+1)
		}

		return out

	case []interface{}:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = jsonSafe(item, depth+1)
		}

		return out

	case float64:
		return finite(t, 64)

	case float32:
		return finite(float64(t), 32)

	default:
		return v
	}
}

// finite returns f unchanged, or its string form when it is NaN or ±Inf.
//
// A string is the honest rendering: the value really was NaN, and the
// alternatives — null, zero, or refusing the record — either invent a number
// that was never on the bus or throw away the export. It follows the same
// convention as the depth bound above: when a value cannot be represented,
// say so in the file rather than failing the write.
//
// bitSize is passed through so a float32 does not acquire the spurious
// precision of a float64 conversion; it only affects finite values.
func finite(f float64, bitSize int) any {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		// "NaN", "+Inf", "-Inf" — the same spellings Go itself prints.
		return strconv.FormatFloat(f, 'g', -1, bitSize)
	}

	if bitSize == 32 {
		// Round-trips through the shortest decimal that reads back as the same
		// float32, so 0.1 exports as 0.1 rather than as 0.10000000149011612.
		v, err := strconv.ParseFloat(strconv.FormatFloat(f, 'g', -1, 32), 64)
		if err != nil {
			return f
		}

		return v
	}

	return f
}
