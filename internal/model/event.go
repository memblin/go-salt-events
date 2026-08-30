// Package model is the shared vocabulary between the ingest side and the UI
// side. It imports neither, which is what lets each be tested without the
// other (spec §3.1).
package model

import "time"

// Kind classifies an event by what it represents, independent of its tag
// text. Panes switch on this rather than re-parsing tags.
type Kind uint8

// Kind values, one per event class the panes switch on.
const (
	KindOther Kind = iota
	KindNew
	KindRet
	KindProg
	KindStart
	KindAuth
	KindKey
	KindPresence
)

func (k Kind) String() string {
	switch k {
	case KindNew:
		return "new"
	case KindRet:
		return "ret"
	case KindProg:
		return "prog"
	case KindStart:
		return "start"
	case KindAuth:
		return "auth"
	case KindKey:
		return "key"
	case KindPresence:
		return "presence"
	case KindOther:
		return "other"
	default:
		return "other"
	}
}

// Event is one message off the bus.
//
// Payload deliberately stays as raw msgpack: ingest decodes only the fields
// above it, and full decoding happens once, lazily, when the Detail pane asks
// (spec §4.2, invariant 4).
type Event struct {
	// Arrival is when the reader received the event. All bucketing and
	// windowing uses this, never Stamp — Stamp is set by whichever process
	// fired the event, so a skewed minion clock would corrupt the graphs
	// (spec §4.3, invariant 2).
	Arrival time.Time

	// Stamp is Salt's _stamp field. Zero when absent or unparseable.
	Stamp time.Time

	Tag       string
	Namespace string
	Category  string
	JID       string
	Minion    string
	Fun       string
	Kind      Kind

	RetCode int
	Success bool
	HasRet  bool

	// Payload is raw msgpack, or nil once the cache has shed it.
	Payload []byte

	// Shed reports that OUR cache dropped the payload to stay under budget.
	// Distinct from MasterTrimmed: different cause, different fix (spec §5.3).
	Shed bool

	// MasterTrimmed reports that Salt's trim_dict gutted the event at the
	// master because it exceeded max_event_size, before we ever saw it.
	MasterTrimmed bool
}

// Size is the event's cost against the cache memory budget (spec §5.1).
func (e Event) Size() int64 {
	const perEventOverhead = 256

	return int64(len(e.Tag) + len(e.Payload) + perEventOverhead)
}
