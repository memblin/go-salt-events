package cache_test

import (
	"bytes"
	"fmt"
	"testing"
	"time"

	"github.com/TKC-Labs/go-salt-events/internal/cache"
	"github.com/TKC-Labs/go-salt-events/internal/model"
)

// matchAll accepts every event.
type matchAll struct{}

func (matchAll) Match(model.Event) bool { return true }

// matcherFunc adapts a function to cache.Matcher.
type matcherFunc func(model.Event) bool

func (f matcherFunc) Match(e model.Event) bool { return f(e) }

func event(tag string, payloadBytes int) model.Event {
	return model.Event{
		Arrival: time.Date(2026, 8, 30, 8, 14, 2, 0, time.UTC),
		Tag:     tag,
		Payload: bytes.Repeat([]byte("x"), payloadBytes),
	}
}

func TestCacheRetainsEventsUnderBudget(t *testing.T) {
	t.Parallel()

	c := cache.New(1 << 20)

	for i := range 10 {
		c.Add(event(fmt.Sprintf("salt/auth/%d", i), 100))
	}

	got := c.Stats()
	if got.Events != 10 {
		t.Errorf("Events = %d, want 10", got.Events)
	}

	if got.Shed != 0 || got.Dropped != 0 {
		t.Errorf("Shed = %d Dropped = %d, want 0/0 under budget", got.Shed, got.Dropped)
	}
}

func TestCacheShedsPayloadsBeforeDroppingEvents(t *testing.T) {
	t.Parallel()

	// Spec §5.2: tag and rate history answer most questions and are tiny, so
	// payloads go first. Dropping whole events first would throw away the
	// cheap, useful part to keep the expensive, rarely-read part.
	c := cache.New(20_000)

	for i := range 50 {
		c.Add(event(fmt.Sprintf("salt/job/2026083008140212345%d/ret/web-1", i), 2_000))
	}

	got := c.Stats()

	if got.Shed == 0 {
		t.Error("Shed = 0; payloads must be shed before events are dropped")
	}

	if got.Events == 0 {
		t.Fatal("Events = 0; every event was dropped instead of shedding payloads")
	}

	if got.Used > got.Budget {
		t.Errorf("Used = %d exceeds Budget = %d", got.Used, got.Budget)
	}
}

func TestCacheShedMarksTheEventDistinctlyFromMasterTrimming(t *testing.T) {
	t.Parallel()

	// Spec §5.3 case A: same symptom, opposite fixes. If the cache set
	// MasterTrimmed, the UI would tell the operator to change master config
	// for data WE dropped.
	c := cache.New(2_000)

	for i := range 20 {
		c.Add(event(fmt.Sprintf("salt/auth/%d", i), 500))
	}

	for _, e := range c.All() {
		if e.Shed && e.MasterTrimmed {
			t.Error("a shed event was also flagged MasterTrimmed; the causes must stay distinct")
		}

		if e.Shed && e.Payload != nil {
			t.Error("a shed event still carries its payload")
		}
	}
}

func TestCacheDropsOldestFirst(t *testing.T) {
	t.Parallel()

	c := cache.New(3_000)

	for i := range 100 {
		c.Add(event(fmt.Sprintf("salt/auth/%03d", i), 100))
	}

	all := c.All()
	if len(all) == 0 {
		t.Fatal("cache is empty")
	}

	// The newest event must always survive.
	if all[len(all)-1].Tag != "salt/auth/099" {
		t.Errorf("newest retained event = %q, want salt/auth/099", all[len(all)-1].Tag)
	}
}

func TestCacheNeverExceedsItsBudget(t *testing.T) {
	t.Parallel()

	// The whole point: this runs as root on a production master.
	c := cache.New(50_000)

	for i := range 5_000 {
		c.Add(event(fmt.Sprintf("salt/job/2026083008140212%04d/ret/web-1", i), 1_000))

		if got := c.Stats(); got.Used > got.Budget {
			t.Fatalf("Used = %d exceeded Budget = %d after %d events",
				got.Used, got.Budget, i)
		}
	}
}

func TestCacheSnapshotAppliesTheMatcher(t *testing.T) {
	t.Parallel()

	c := cache.New(1 << 20)
	c.Add(event("salt/auth", 10))
	c.Add(event("salt/job/20260830081402123456/ret/web-1", 10))

	only := matcherFunc(func(e model.Event) bool { return e.Tag == "salt/auth" })

	got := c.Snapshot(only, 100)
	if len(got) != 1 || got[0].Tag != "salt/auth" {
		t.Errorf("Snapshot = %v", got)
	}
}

// TestCacheShedPreservesAnAlreadyMasterTrimmedEvent is the other half of
// invariant 5. The previous test proves the cache never *sets* MasterTrimmed;
// this one proves it never *clears* it either.
//
// An event can legitimately be both: the master gutted an oversize value
// before we saw it, and then our budget shed what was left. Those are two
// separate facts with two separate fixes (raise max_event_size / raise
// --max-memory), so they must stay separately representable rather than
// collapsing into one "data missing" flag (spec §5.3 case A).
func TestCacheShedPreservesAnAlreadyMasterTrimmedEvent(t *testing.T) {
	t.Parallel()

	c := cache.New(1_200)

	for i := range 10 {
		e := event(fmt.Sprintf("salt/auth/%d", i), 500)
		e.MasterTrimmed = true

		c.Add(e)
	}

	all := c.All()
	if len(all) == 0 {
		t.Fatal("cache is empty")
	}

	sawBoth := false

	for _, e := range all {
		if !e.MasterTrimmed {
			t.Errorf("event %q lost its MasterTrimmed flag; the cache must never clear Salt's fact", e.Tag)
		}

		if e.Shed {
			sawBoth = true
		}
	}

	if !sawBoth {
		t.Fatal("premise failed: nothing was shed, so the both-causes case was never exercised")
	}
}

// TestCacheSheddingNeverAltersAnIndexedField is invariant 9, expressed at the
// only layer this package can express it at.
//
// Job correlation and every aggregate read exclusively the eagerly-extracted
// fields (spec §4.2). If shedding could disturb one of them, the Jobs pane
// would keep rendering and would quietly render wrong numbers — the worst
// possible failure for this tool (spec §5.2).
//
// MUTATE THIS TEST before trusting it: in cache.degrade's shedding loop, add
// `e.Minion = ""` next to `e.Payload = nil` and confirm this fails.
func TestCacheSheddingNeverAltersAnIndexedField(t *testing.T) {
	t.Parallel()

	const jid = "20260830081402123456"

	arrival := time.Date(2026, 8, 30, 8, 14, 2, 0, time.UTC)

	// A deliberately tiny budget: every payload will be shed.
	c := cache.New(8 << 10)

	const total = 200

	for i := range total {
		c.Add(model.Event{
			Arrival: arrival.Add(time.Duration(i) * time.Millisecond),
			Stamp:   arrival.Add(time.Duration(i) * time.Millisecond),
			Kind:    model.KindRet,
			JID:     jid,
			Minion:  fmt.Sprintf("web-%04d", i),
			Fun:     "state.apply",
			RetCode: i % 2,
			Success: i%2 == 0,
			HasRet:  true,
			Tag:     fmt.Sprintf("salt/job/%s/ret/web-%04d", jid, i),
			Payload: bytes.Repeat([]byte("y"), 4096),
		})
	}

	if got := c.Stats(); got.Shed == 0 {
		t.Fatalf("premise failed: the cache shed nothing (used=%d budget=%d)", got.Used, got.Budget)
	}

	all := c.All()
	if len(all) == 0 {
		t.Fatal("cache is empty")
	}

	shedSeen := 0

	for _, e := range all {
		if e.Shed {
			shedSeen++
		}

		// The tag carries the index this event was created with, so every
		// eagerly-extracted field can be checked against it independently of
		// how many events survived.
		var i int
		if _, err := fmt.Sscanf(e.Tag, "salt/job/"+jid+"/ret/web-%04d", &i); err != nil {
			t.Fatalf("unparseable tag %q: %v", e.Tag, err)
		}

		want := model.Event{
			Arrival: arrival.Add(time.Duration(i) * time.Millisecond),
			Stamp:   arrival.Add(time.Duration(i) * time.Millisecond),
			Kind:    model.KindRet,
			JID:     jid,
			Minion:  fmt.Sprintf("web-%04d", i),
			Fun:     "state.apply",
			RetCode: i % 2,
			Success: i%2 == 0,
			HasRet:  true,
		}

		switch {
		case !e.Arrival.Equal(want.Arrival):
			t.Errorf("event %d: Arrival = %v, want %v", i, e.Arrival, want.Arrival)
		case !e.Stamp.Equal(want.Stamp):
			t.Errorf("event %d: Stamp = %v, want %v", i, e.Stamp, want.Stamp)
		case e.Kind != want.Kind:
			t.Errorf("event %d: Kind = %v, want %v", i, e.Kind, want.Kind)
		case e.JID != want.JID:
			t.Errorf("event %d: JID = %q, want %q", i, e.JID, want.JID)
		case e.Minion != want.Minion:
			t.Errorf("event %d: Minion = %q, want %q", i, e.Minion, want.Minion)
		case e.Fun != want.Fun:
			t.Errorf("event %d: Fun = %q, want %q", i, e.Fun, want.Fun)
		case e.RetCode != want.RetCode:
			t.Errorf("event %d: RetCode = %d, want %d", i, e.RetCode, want.RetCode)
		case e.Success != want.Success:
			t.Errorf("event %d: Success = %v, want %v", i, e.Success, want.Success)
		case e.HasRet != want.HasRet:
			t.Errorf("event %d: HasRet = %v, want %v", i, e.HasRet, want.HasRet)
		}
	}

	if shedSeen == 0 {
		t.Error("no retained event was marked Shed, so nothing was actually checked post-shedding")
	}
}

func TestCacheSnapshotLimit(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		matcher  cache.Matcher
		limit    int
		wantTags []string
	}{
		{
			name:     "limit smaller than the match count keeps the newest",
			matcher:  matchAll{},
			limit:    2,
			wantTags: []string{"salt/job/c", "salt/job/d"},
		},
		{
			name:     "limit larger than the match count returns everything, oldest first",
			matcher:  matchAll{},
			limit:    100,
			wantTags: []string{"salt/auth/a", "salt/auth/b", "salt/job/c", "salt/job/d"},
		},
		{
			name:     "a nil matcher matches everything",
			matcher:  nil,
			limit:    100,
			wantTags: []string{"salt/auth/a", "salt/auth/b", "salt/job/c", "salt/job/d"},
		},
		{
			name:     "a selective matcher is applied before the limit",
			matcher:  matcherFunc(func(e model.Event) bool { return e.Kind == model.KindRet }),
			limit:    1,
			wantTags: []string{"salt/job/d"},
		},
		{
			// The UI derives the limit from the visible row count, which is
			// zero on a terminal too short to draw a table.
			name:     "a zero limit returns nothing",
			matcher:  matchAll{},
			limit:    0,
			wantTags: nil,
		},
		{
			name:     "a negative limit returns nothing rather than panicking",
			matcher:  matchAll{},
			limit:    -1,
			wantTags: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			c := cache.New(1 << 20)

			for _, tag := range []string{"salt/auth/a", "salt/auth/b"} {
				c.Add(event(tag, 10))
			}

			for _, tag := range []string{"salt/job/c", "salt/job/d"} {
				e := event(tag, 10)
				e.Kind = model.KindRet

				c.Add(e)
			}

			got := c.Snapshot(tt.matcher, tt.limit)
			if len(got) != len(tt.wantTags) {
				t.Fatalf("Snapshot returned %d events, want %d", len(got), len(tt.wantTags))
			}

			for i, want := range tt.wantTags {
				if got[i].Tag != want {
					t.Errorf("Snapshot()[%d].Tag = %q, want %q", i, got[i].Tag, want)
				}
			}
		})
	}
}

func TestCacheAllReturnsAnIndependentCopy(t *testing.T) {
	t.Parallel()

	// Export (Task 14) writes from All() while ingest keeps running. If All
	// aliased the ring, a later Add could reallocate underneath the writer.
	c := cache.New(1 << 20)
	c.Add(event("salt/auth/0", 10))

	all := c.All()
	all[0].Tag = "clobbered"

	if got := c.All(); got[0].Tag != "salt/auth/0" {
		t.Errorf("mutating the returned slice changed the cache: Tag = %q", got[0].Tag)
	}
}

func TestCacheStatsCountsDrops(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		budget      int64
		events      int
		payload     int
		wantEvents  int
		wantShed    bool
		wantDropped bool
	}{
		{
			name:       "under budget nothing degrades",
			budget:     1 << 20,
			events:     5,
			payload:    100,
			wantEvents: 5,
		},
		{
			name:     "shedding alone can be enough",
			budget:   4_000,
			events:   5,
			payload:  1_000,
			wantShed: true,
		},
		{
			name:        "past shedding, whole events go",
			budget:      1_000,
			events:      50,
			payload:     1_000,
			wantShed:    true,
			wantDropped: true,
		},
		{
			// Nothing in this package may panic on data shapes it did not
			// choose, and a zero budget is a reachable misconfiguration.
			name:        "a zero budget drops everything without panicking",
			budget:      0,
			events:      10,
			payload:     10,
			wantEvents:  0,
			wantShed:    true,
			wantDropped: true,
		},
		{
			name:        "a negative budget drops everything without panicking",
			budget:      -1,
			events:      10,
			payload:     10,
			wantEvents:  0,
			wantShed:    true,
			wantDropped: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			c := cache.New(tt.budget)

			for i := range tt.events {
				c.Add(event(fmt.Sprintf("salt/auth/%d", i), tt.payload))
			}

			got := c.Stats()

			if got.Budget != tt.budget {
				t.Errorf("Budget = %d, want %d", got.Budget, tt.budget)
			}

			if got.Used > got.Budget && got.Events > 0 {
				t.Errorf("Used = %d exceeds Budget = %d", got.Used, got.Budget)
			}

			if got.Events != len(c.All()) {
				t.Errorf("Events = %d but All() returned %d", got.Events, len(c.All()))
			}

			if tt.wantEvents != 0 && got.Events != tt.wantEvents {
				t.Errorf("Events = %d, want %d", got.Events, tt.wantEvents)
			}

			if (got.Shed > 0) != tt.wantShed {
				t.Errorf("Shed = %d, want any = %v", got.Shed, tt.wantShed)
			}

			if (got.Dropped > 0) != tt.wantDropped {
				t.Errorf("Dropped = %d, want any = %v", got.Dropped, tt.wantDropped)
			}
		})
	}
}

func TestCacheDegradationNeverLosesTheAccounting(t *testing.T) {
	t.Parallel()

	// Used must stay exactly the sum of what is retained. A drift here shows
	// up as a status bar that reads "203 MiB / 256 MiB" on an empty cache.
	c := cache.New(6_000)

	for i := range 200 {
		c.Add(event(fmt.Sprintf("salt/auth/%03d", i), 300))
	}

	var want int64
	for _, e := range c.All() {
		want += e.Size()
	}

	if got := c.Stats(); got.Used != want {
		t.Errorf("Used = %d, want %d (the sum of retained event sizes)", got.Used, want)
	}
}
