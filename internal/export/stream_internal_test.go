package export

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/TKC-Labs/go-salt-events/internal/model"
)

// errAfter fails once it has accepted n bytes, which is how ENOSPC arrives
// mid-write: the pre-flight passed, and another process won the race.
type errAfter struct {
	n     int
	taken int
}

var errNoSpace = errors.New("no space left on device")

func (w *errAfter) Write(p []byte) (int, error) {
	if w.taken >= w.n {
		return 0, errNoSpace
	}

	w.taken += len(p)

	return len(p), nil
}

func streamOpts() Options {
	return Options{
		Dir:   "",
		Max:   1 << 30,
		Now:   time.Now,
		Space: nil,
		Chown: nil,
		Decode: func(b []byte) (any, error) {
			return map[string]any{"len": len(b)}, nil
		},
	}
}

func manyEvents(n int) []model.Event {
	out := make([]model.Event, 0, n)

	for range n {
		out = append(out, model.Event{
			Arrival: time.Date(2026, 8, 30, 8, 14, 2, 0, time.UTC),
			Tag:     "salt/job/20260830081402123456/ret/web-1",
			Payload: []byte("payload"),
		})
	}

	return out
}

func TestStreamSurfacesAWriteFailure(t *testing.T) {
	t.Parallel()

	// The write cannot be assumed to succeed just because the pre-flight did.
	// If this error is swallowed, writeFile renames a truncated file into
	// place and the operator ships an export that silently ends early.
	w := &errAfter{n: 1 << 12, taken: 0}

	written, err := stream(w, manyEvents(5000), streamOpts())
	if !errors.Is(err, errNoSpace) {
		t.Fatalf("err = %v, want the underlying write error", err)
	}

	if !strings.Contains(err.Error(), "write export") {
		t.Errorf("error is not attributed to the export write: %v", err)
	}

	if written <= 0 {
		t.Errorf("written = %d, want the byte count reached before the failure", written)
	}
}

func TestStreamStopsAtTheFirstFailedEvent(t *testing.T) {
	t.Parallel()

	// Abort, do not skip: an export that silently dropped the events it could
	// not encode would look complete while missing the interesting ones.
	opts := streamOpts()

	seen := 0
	opts.Decode = func([]byte) (any, error) {
		seen++

		return nil, errNoSpace
	}

	var sink strings.Builder

	if _, err := stream(&sink, manyEvents(10), opts); err == nil {
		t.Fatal("expected an error")
	}

	if seen != 1 {
		t.Errorf("decoded %d events after the first failure, want 1", seen)
	}
}
