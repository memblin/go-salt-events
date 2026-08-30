//go:build integration

// This file holds the gate every integration test in this package goes
// through. It is compiled only under -tags=integration.
//
// The gate exists because of a tension the rest of the suite does not have:
// these tests need a live Salt master, most hosts do not have one, and a test
// that cannot run must not fail CI. The answer everywhere is to skip — but a
// skip verifies nothing, and a suite that has silently skipped for six months
// is indistinguishable from one that passes. So the skip is made CHECKABLE:
// set SALT_EVENTS_REQUIRE_BUS=1 and every skip in here becomes a failure.
//
// Do NOT set that variable in CI. No CI runner can host a salt-master, so it
// would make CI permanently and uninformatively red. It is for a human running
// the suite ON a master, deliberately, to prove the run meant something.
package saltipc_test

import (
	"errors"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/TKC-Labs/go-salt-events/internal/config"
	"github.com/TKC-Labs/go-salt-events/internal/saltipc"
)

// requireBusEnv turns every skip in this file into a failure.
//
// It is spelt SALT_EVENTS_* rather than SALTEV_* on purpose: SALTEV_ is the
// runtime configuration namespace (spec §11), and this is not a setting of the
// program — it is an assertion about the machine the test is running on.
const requireBusEnv = "SALT_EVENTS_REQUIRE_BUS"

// sockDirEnv overrides where the integration tests look for the socket, for a
// master whose sock_dir Salt relocated (spec §2.6). It is the same key the
// program itself reads, so `sudo -E just test-integration` sees the operator's
// own setting without a second thing to remember.
const sockDirEnv = "SALTEV_SOCK_DIR"

// busRequired reports whether the operator asserted that a live bus is present.
//
// A malformed value is a hard failure rather than a shrug. "I exported
// SALT_EVENTS_REQUIRE_BUS=yes and the suite still skipped" is precisely the
// silent failure this variable exists to remove, so it is not allowed to
// recur one level up. Accepted spellings are strconv.ParseBool's, matching
// internal/config's envBool.
func busRequired(t *testing.T) bool {
	t.Helper()

	v := os.Getenv(requireBusEnv)
	if v == "" {
		return false
	}

	required, err := strconv.ParseBool(v)
	if err != nil {
		t.Fatalf("%s=%q: want a boolean (1/0, true/false); "+
			"an unreadable value here would silently leave the skips in place, "+
			"which is the exact thing this variable exists to prevent: %v",
			requireBusEnv, v, err)
	}

	return required
}

// skipWithoutBus reports that there is no usable event bus here.
//
// It skips by default and FAILS when SALT_EVENTS_REQUIRE_BUS is set, and both
// messages carry the same reason: the point is not to hide the reason behind
// the outcome, it is to let the operator choose which outcome that reason
// should produce.
func skipWithoutBus(t *testing.T, reason string) {
	t.Helper()

	if busRequired(t) {
		t.Fatalf("%s is set, so a missing event bus is a FAILURE, not a skip.\n\n%s",
			requireBusEnv, reason)
	}

	t.Skipf("SKIPPED, NOTHING VERIFIED — no live Salt master here.\n\n%s\n\n"+
		"Set %s=1 to make this a failure instead, when you believe you are on a master.",
		reason, requireBusEnv)
}

// requireLiveBus returns a Reader for the live publish socket, or skips.
//
// It proves readability by DIALLING, not by stat-ing and not by opening the
// path as a file. Both of the cheaper probes are wrong here:
//
//   - os.Stat succeeds for any user who can traverse the directory, and the
//     socket is mode 0600 root:root (spec §2.7). A stat-only gate therefore
//     lets a non-root run past the door and fail later on connect, which is a
//     red suite for the one reason that was supposed to skip.
//   - os.Open on a socket inode returns ENXIO ("no such device or address")
//     even for root, because open(2) has no meaning for AF_UNIX. A gate built
//     on it skips ALWAYS, including on a healthy master under sudo — verified
//     on this host against a real listening socket.
//
// connect(2) is the operation the program actually performs, so it is the one
// that answers the question being asked. The probe connection is closed
// immediately; Salt's publisher handles a subscriber disconnect routinely.
//
// The dial goes through saltipc's own guarded Dial so the probe cannot reach a
// socket the program itself would refuse (invariant 1, layer 2).
func requireLiveBus(t *testing.T) *saltipc.Reader {
	t.Helper()

	sockDir := os.Getenv(sockDirEnv)
	if sockDir == "" {
		sockDir = config.DefaultSockDir
	}

	reader := saltipc.NewReader(sockDir, time.Now)
	sockPath := reader.SocketPath()

	conn, err := reader.Dial()
	if err == nil {
		_ = conn.Close()

		return reader
	}

	// A path that does not resolve to master_event_pub.ipc is never a "no bus
	// here" condition. It means the one structural guarantee this program
	// makes — that it cannot touch the socket which could inject events
	// (invariant 1) — was aimed at something else, and skipping past that
	// would be the worst possible reading of the failure.
	if errors.Is(err, saltipc.ErrNotPubSocket) {
		t.Fatalf("refusing to run against %s: %v\n\n%s",
			sockPath, err, saltipc.Diagnose(sockPath, err))
	}

	skipWithoutBus(t, saltipc.Diagnose(sockPath, err))

	return nil
}
