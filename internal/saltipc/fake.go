package saltipc

import (
	"fmt"
	"time"

	"github.com/vmihailenco/msgpack/v5"
)

// Fake drives a Sink without a socket, for tests in packages that consume
// events but have no business knowing the wire format.
//
// It shares buildEvent with the real reader on purpose: a fake that assembled
// events its own way would let the cache, the stats aggregator and the panes be
// green against a shape the bus never produces.
type Fake struct {
	// Now supplies each event's arrival time. NewFake wires it to a clock that
	// advances one millisecond per call, so events are strictly ordered without
	// any test having to sleep.
	Now func() time.Time
}

// fakeTick is how far NewFake's clock advances per event.
const fakeTick = time.Millisecond

// NewFake returns a Fake with a fixed, strictly increasing starting clock.
func NewFake(start time.Time) *Fake {
	now := start

	return &Fake{Now: func() time.Time {
		now = now.Add(fakeTick)

		return now
	}}
}

// Feed builds an event from a tag and already-extracted fields and delivers it.
//
// Use this when a test wants to state the indexed fields directly. payload may
// be nil or arbitrary bytes; nothing decodes it unless the test asks a Detail
// pane to.
func (f *Fake) Feed(sink Sink, tag string, fields Fields, payload []byte) {
	sink.Event(buildEvent(f.Now(), tag, fields, payload))
}

// FeedData encodes data as Salt would, then runs it through the real
// ExtractFields path before delivering it.
//
// This is the entry point for packages that should not know the wire format:
// they hand over a plain map and still get an event whose fields were derived
// exactly as the socket reader derives them, payload included. Feed is the
// faster, lower-fidelity alternative for tests that only care about the
// indexed fields.
func (f *Fake) FeedData(sink Sink, tag string, data map[string]any) error {
	payload, err := msgpack.Marshal(data)
	if err != nil {
		return fmt.Errorf("encode fake payload for %s: %w", tag, err)
	}

	sink.Event(buildEvent(f.Now(), tag, ExtractFields(payload), payload))

	return nil
}
