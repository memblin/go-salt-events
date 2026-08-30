package stats

import "sort"

// OtherKey is the fold-in bucket for keys past the cardinality cap. Cycling or
// generating new classes past a handful is both a memory leak on a busy master
// and unreadable on screen, so the tail folds rather than growing (spec §9).
const OtherKey = "other"

// Entry is one ranked key.
type Entry struct {
	Key   string
	Count uint64
	Pct   float64
}

// Counter is a top-N counter with bounded cardinality.
//
// Counter is NOT safe for concurrent use — concurrent map access on counts
// would crash outright, not just race. The architecture serialises access
// externally: a reader goroutine feeds it under the hub's mutex (Task 21),
// and the UI reads a snapshot assembled under that same lock. Do not add a
// mutex here — that would nest a second lock inside one already held on
// every ingest.
type Counter struct {
	maxKeys int
	counts  map[string]uint64
	other   uint64
	total   uint64
}

// NewCounter returns a Counter tracking at most maxKeys distinct keys.
func NewCounter(maxKeys int) *Counter {
	return &Counter{maxKeys: maxKeys, counts: make(map[string]uint64)}
}

// Add counts one occurrence of key.
func (c *Counter) Add(key string) {
	c.total++

	if _, known := c.counts[key]; known {
		c.counts[key]++

		return
	}

	if len(c.counts) >= c.maxKeys {
		c.other++

		return
	}

	c.counts[key] = 1
}

// Total is every counted occurrence, including folded ones. Folding must never
// lose counts, or the percentages stop summing to 100 and the display lies.
func (c *Counter) Total() uint64 { return c.total }

// Top returns the n highest-counted keys, plus the folded tail when non-empty.
//
// Ties break by key name so the ordering is stable between renders: rows that
// reshuffle under the cursor for equal counts are hostile to read.
func (c *Counter) Top(n int) []Entry {
	out := make([]Entry, 0, len(c.counts)+1)

	for k, v := range c.counts {
		out = append(out, Entry{Key: k, Count: v})
	}

	sort.Slice(out, func(a, b int) bool {
		if out[a].Count != out[b].Count {
			return out[a].Count > out[b].Count
		}

		return out[a].Key < out[b].Key
	})

	if len(out) > n {
		out = out[:n]
	}

	if c.other > 0 {
		out = append(out, Entry{Key: OtherKey, Count: c.other})
	}

	for i := range out {
		if c.total > 0 {
			out[i].Pct = float64(out[i].Count) / float64(c.total) * 100
		}
	}

	return out
}
