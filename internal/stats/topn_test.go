package stats_test

import (
	"fmt"
	"testing"

	"github.com/TKC-Labs/go-salt-events/internal/stats"
)

func TestCounterRanksByCount(t *testing.T) {
	t.Parallel()

	c := stats.NewCounter(100)

	for range 10 {
		c.Add("salt/job/*/ret/*")
	}

	for range 3 {
		c.Add("salt/auth")
	}

	c.Add("salt/key")

	got := c.Top(2)
	if len(got) != 2 {
		t.Fatalf("Top(2) returned %d entries", len(got))
	}

	if got[0].Key != "salt/job/*/ret/*" || got[0].Count != 10 {
		t.Errorf("Top()[0] = %+v", got[0])
	}

	if got[1].Key != "salt/auth" || got[1].Count != 3 {
		t.Errorf("Top()[1] = %+v", got[1])
	}
}

func TestCounterPercentagesAreOfTheTotal(t *testing.T) {
	t.Parallel()

	c := stats.NewCounter(100)

	for range 3 {
		c.Add("a")
	}

	c.Add("b")

	got := c.Top(1)
	if len(got) != 1 {
		t.Fatalf("Top(1) returned %d entries", len(got))
	}

	if got[0].Pct < 74 || got[0].Pct > 76 {
		t.Errorf("Pct = %v, want ~75", got[0].Pct)
	}
}

func TestCounterFoldsTheTailIntoOther(t *testing.T) {
	t.Parallel()

	// Cardinality must stay bounded: an unbounded key space on a busy master
	// is a memory leak, and past ~7 classes the display is unreadable anyway.
	c := stats.NewCounter(4)

	for i := range 50 {
		c.Add(fmt.Sprintf("key-%d", i))
	}

	got := c.Top(10)

	if len(got) > 5 {
		t.Fatalf("Top() returned %d entries; cardinality is not bounded", len(got))
	}

	found := false

	for _, e := range got {
		if e.Key == stats.OtherKey {
			found = true
		}
	}

	if !found {
		t.Error("no 'other' entry; the tail must fold rather than be dropped")
	}
}

func TestCounterTotalCountsEverythingIncludingTheTail(t *testing.T) {
	t.Parallel()

	c := stats.NewCounter(2)

	for i := range 20 {
		c.Add(fmt.Sprintf("key-%d", i))
	}

	if got := c.Total(); got != 20 {
		t.Errorf("Total() = %d, want 20 — folding must not lose counts", got)
	}
}

func TestCounterIsStableForEqualCounts(t *testing.T) {
	t.Parallel()

	// Colour follows the entity, not the rank (spec §9). A ranking that
	// reshuffles between renders for equal counts would make rows jitter.
	c := stats.NewCounter(100)
	c.Add("bbb")
	c.Add("aaa")

	first := c.Top(2)
	second := c.Top(2)

	for i := range first {
		if first[i].Key != second[i].Key {
			t.Fatalf("Top() is unstable: %v then %v", first, second)
		}
	}
}
