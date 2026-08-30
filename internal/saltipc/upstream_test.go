package saltipc_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// saltRoot is the onedir install this pin is written against.
const saltRoot = "/opt/saltstack/salt/lib/python3.11/site-packages/salt"

// TestSaltStillUsesLengthPrefixedIPCFraming pins the upstream behaviour our
// FrameReader is built on (spec §13: "pin upstream behaviour you work around").
//
// If Salt ever reverts to a bare streaming msgpack stream, or changes the
// prefix width, this test fails and names the file to look at — rather than
// leaving us with a reader that silently decodes nothing.
func TestSaltStillUsesLengthPrefixedIPCFraming(t *testing.T) {
	t.Parallel()

	src, err := os.ReadFile(filepath.Join(saltRoot, "transport", "frame.py"))
	if err != nil {
		t.Skipf("salt source not present at %s: %v", saltRoot, err)
	}

	if !strings.Contains(string(src), `struct.pack(">I", len(payload))`) {
		t.Fatalf("salt/transport/frame.py no longer prefixes IPC frames with a "+
			"4-byte big-endian length; internal/saltipc/frame.go must be revisited "+
			"(looked in %s)", saltRoot)
	}
}

// TestSaltMaxEventSizeIsStillOneMiB pins the ceiling the cache budget relies on.
func TestSaltMaxEventSizeIsStillOneMiB(t *testing.T) {
	t.Parallel()

	src, err := os.ReadFile(filepath.Join(saltRoot, "config", "__init__.py"))
	if err != nil {
		t.Skipf("salt source not present: %v", err)
	}

	if !strings.Contains(string(src), `"max_event_size": 1048576`) {
		t.Fatal("salt's default max_event_size is no longer 1048576; " +
			"revisit the cache budget arithmetic in spec §5.1")
	}
}

// TestSaltTagendIsStillDoubleNewline pins the tag delimiter.
func TestSaltTagendIsStillDoubleNewline(t *testing.T) {
	t.Parallel()

	src, err := os.ReadFile(filepath.Join(saltRoot, "utils", "event.py"))
	if err != nil {
		t.Skipf("salt source not present: %v", err)
	}

	if !strings.Contains(string(src), `TAGEND = "\n\n"`) {
		t.Fatal("salt's TAGEND is no longer \\n\\n; revisit decodeFrame")
	}
}
