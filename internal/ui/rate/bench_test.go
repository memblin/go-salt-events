package rate_test

import (
	"strconv"
	"strings"
	"testing"

	"github.com/TKC-Labs/go-salt-events/internal/stats"
	"github.com/TKC-Labs/go-salt-events/internal/theme"
	"github.com/TKC-Labs/go-salt-events/internal/ui"
	"github.com/TKC-Labs/go-salt-events/internal/ui/rate"
)

// BenchmarkRateViewLongKey is invariant 6 measured rather than asserted.
//
// Render cost must be independent of payload size: a minion can event.send a
// tag of any length, Category derives from it, and this pane redraws ten times
// a second. Handing the whole key to lipgloss costs a full-length scan per key
// per frame — measured at 4 125 µs against 103 µs for the bounded version on a
// 50 000-character key, which with two breakdown columns of five rows each is
// enough to miss the frame budget outright.
//
// It is a benchmark rather than a unit test on purpose: the defect is a
// complexity class, and a wall-clock threshold inside the suite would be flaky
// on shared CI. Compare /50000 against /50 — the RATIO is the finding.
func BenchmarkRateViewLongKey(b *testing.B) {
	st, ok := theme.StylesFor("gruvbox-dark")
	if !ok {
		b.Fatal("gruvbox-dark is not registered")
	}

	for _, n := range []int{50, 50_000} {
		b.Run(strconv.Itoa(n), func(b *testing.B) {
			key := strings.Repeat("a", n)
			s := ui.Snapshot{
				TopCategories: []stats.Entry{{Key: key, Count: 1, Pct: 100}},
				TopMinions:    []stats.Entry{{Key: key, Count: 1, Pct: 100}},
			}

			p := rate.New()

			b.ReportAllocs()
			b.ResetTimer()

			for range b.N {
				_ = p.View(100, 24, s, st)
			}
		})
	}
}
