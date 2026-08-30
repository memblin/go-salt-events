//go:build integration

// This file is compiled only under -tags=integration and needs a live Salt
// master. On any host without one it SKIPS, and a skip verifies nothing —
// see the skip messages, which say so in as many words, and see
// integration_test.go for how to turn those skips into failures.
package saltipc_test

import (
	"context"
	"testing"
	"time"

	"github.com/TKC-Labs/go-salt-events/internal/saltipc"
)

// integrationWindow is how long we sit on the real bus. Long enough that a
// busy master will produce something, short enough to stay a test.
const integrationWindow = 10 * time.Second

// TestIntegrationReaderReadsTheRealPublishSocket is the only test in this
// package that touches a production Salt master.
//
// It asserts what only the real thing can prove: that the socket opens, that
// real frames decode without error, and that the reader stays connected. It
// deliberately does NOT require events to arrive — a quiet master is a normal
// master — but it reports loudly when none did, because a run that decoded
// nothing has not exercised the decoder.
//
// Run it on the master with:
//
//	sudo -E go test -tags=integration -count=1 -run Integration ./internal/saltipc/
//
// Add SALT_EVENTS_REQUIRE_BUS=1 to assert that the bus really is there, which
// turns "no master, skipped" into a failure.
func TestIntegrationReaderReadsTheRealPublishSocket(t *testing.T) {
	// Skips (or, under SALT_EVENTS_REQUIRE_BUS, fails) when the socket is
	// absent or unreadable. Both are ordinary — no master here, or no sudo —
	// and neither is this test's news to break.
	reader := requireLiveBus(t)

	sink := newRecordSink(1)

	ctx, cancel := context.WithTimeout(t.Context(), integrationWindow)
	defer cancel()

	if err := reader.Run(ctx, sink); err != nil {
		t.Fatalf("Run against the real master: %v\n%s",
			err, saltipc.Diagnose(reader.SocketPath(), err))
	}

	events, gaps, errs := sink.snapshot()

	t.Logf("real bus: %d events, %d gaps, %d decode errors in %v",
		len(events), len(gaps), errs, integrationWindow)

	if errs != 0 {
		t.Errorf("%d frames off the real bus failed to decode; framing or msgpack "+
			"handling has drifted from this master's Salt version", errs)
	}

	if len(gaps) != 0 {
		t.Errorf("%d gaps in %v; the reader lost the publish socket while the "+
			"master was up", len(gaps), integrationWindow)
	}

	if len(events) == 0 {
		t.Logf("WARNING: NOTHING VERIFIED BEYOND CONNECT: the master was silent for %v, "+
			"so no real frame was decoded. Re-run while generating traffic "+
			"(e.g. salt '*' test.ping) for this test to mean anything.",
			integrationWindow)

		return
	}

	for i, e := range events {
		if e.Arrival.IsZero() {
			t.Errorf("event %d (tag %q) has no arrival time", i, e.Tag)
		}

		if e.Tag == "" {
			t.Errorf("event %d arrived with an empty tag", i)
		}

		if len(e.Payload) == 0 {
			t.Errorf("event %d (tag %q) lost its payload", i, e.Tag)
		}
	}
}
