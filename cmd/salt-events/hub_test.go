package main

import (
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/TKC-Labs/go-salt-events/internal/filter"
	"github.com/TKC-Labs/go-salt-events/internal/model"
	"github.com/TKC-Labs/go-salt-events/internal/saltipc"
	"github.com/TKC-Labs/go-salt-events/internal/stats"
	"github.com/TKC-Labs/go-salt-events/internal/ui"
)

// start is the fixed instant every clockless test builds its events around.
var start = time.Date(2026, 8, 30, 8, 0, 0, 0, time.UTC)

// testJID is a real-shaped JID: salttag validates them as 20 digits, so a
// shorter one would be parsed as an ordinary tag segment and never correlate.
const testJID = "20260830081402123456"

// newTestHub builds a hub with a stopped clock and the real decoder.
//
// The decoder matters: saltipc.DecodeValue sets DecodeUntypedMap, so it returns
// map[interface{}]interface{} for every map. A test that injected its own
// map[string]any decoder would pass against a hub that cannot read a single
// real event (see the expected-minions tests below).
func newTestHub(t *testing.T, maxMemory int64, maxJobs int) (*hub, *stats.FakeClock) {
	t.Helper()

	clk := stats.NewFakeClock(start)

	return newHub(hubConfig{
		MaxMemory: maxMemory,
		MaxJobs:   maxJobs,
		Clock:     clk,
		Decode:    saltipc.DecodeValue,
	}), clk
}

// feedData pushes one event through the REAL ingest path: msgpack-encoded the
// way Salt encodes it, then run through saltipc.ExtractFields.
func feedData(t *testing.T, f *saltipc.Fake, h *hub, tag string, data map[string]any) {
	t.Helper()

	if err := f.FeedData(h, tag, data); err != nil {
		t.Fatalf("feed %s: %v", tag, err)
	}
}

func TestHubFeedsStatsAndCacheIndependently(t *testing.T) {
	t.Parallel()

	// Invariant 3: stats must survive cache eviction. A tiny cache plus many
	// events proves the rate history is not derived from what the cache kept.
	h, _ := newTestHub(t, 1024, 100)

	for range 500 {
		h.Event(model.Event{
			Arrival: start,
			Tag:     "salt/auth",
			Payload: make([]byte, 100),
		})
	}

	snap := h.Snapshot(filter.Query{}, 100)

	if snap.Cache.Events >= 500 {
		t.Fatalf("premise failed: the cache retained %d of 500 events", snap.Cache.Events)
	}

	if snap.SecSum.Peak < 500 {
		t.Errorf("rate peak = %v, want 500 — stats must not be derived from the cache",
			snap.SecSum.Peak)
	}

	if got := snap.TopCategories; len(got) == 0 || got[0].Count != 500 {
		t.Errorf("top categories = %v, want one key counted 500 times", got)
	}
}

// TestBucketingUsesArrivalNeverStamp is invariant 2 at the seam that owns it.
//
// Every event carries a _stamp an hour in the past — the shape a minion with a
// skewed clock produces. Bucketing on it would put all 500 events outside the
// 120-second window, so the visible peak would collapse to zero.
func TestBucketingUsesArrivalNeverStamp(t *testing.T) {
	t.Parallel()

	h, _ := newTestHub(t, 1<<20, 100)

	skewed := start.Add(-time.Hour)

	for range 500 {
		h.Event(model.Event{
			Arrival: start,
			Stamp:   skewed,
			Tag:     "salt/auth",
		})
	}

	snap := h.Snapshot(filter.Query{}, 100)

	if snap.SecSum.Now != 500 {
		t.Errorf("events/sec now = %v, want 500: a skewed _stamp moved the bucket",
			snap.SecSum.Now)
	}

	if snap.SecSum.Peak != 500 {
		t.Errorf("events/sec peak = %v, want 500: a skewed _stamp moved the bucket",
			snap.SecSum.Peak)
	}
}

// TestSnapshotIsBoundedByLimitNotByCacheSize is invariant 6's measurable half:
// render cost stays O(visible rows), so what the UI is handed each tick must be
// bounded by what it asked for and not by how many events arrived.
func TestSnapshotIsBoundedByLimitNotByCacheSize(t *testing.T) {
	t.Parallel()

	h, _ := newTestHub(t, 1<<30, 100)

	for range 5000 {
		h.Event(model.Event{Arrival: start, Tag: "salt/auth"})
	}

	if got := h.Snapshot(filter.Query{}, 100).Cache.Events; got != 5000 {
		t.Fatalf("premise failed: the cache retained %d of 5000 events", got)
	}

	if got := len(h.Snapshot(filter.Query{}, 50).Events); got != 50 {
		t.Errorf("a snapshot limited to 50 carried %d events", got)
	}
}

func TestHubIsSafeUnderConcurrentIngestAndSnapshot(t *testing.T) {
	t.Parallel()

	h := newHub(hubConfig{MaxMemory: 1 << 20, MaxJobs: 100, Clock: stats.RealClock{}})

	var wg sync.WaitGroup

	wg.Add(2)

	go func() {
		defer wg.Done()

		for range 2000 {
			h.Event(model.Event{Arrival: time.Now(), Tag: "salt/auth"})
		}
	}()

	go func() {
		defer wg.Done()

		for range 2000 {
			_ = h.Snapshot(filter.Query{}, 50)
		}
	}()

	wg.Wait()
}

// TestSnapshotJobsAreNotAliasedIntoTheIndex is the same race one level deeper,
// and it is the one that kills the process rather than merely racing: a Job is
// mutated in place as returns arrive, so a snapshot carrying the live pointer
// lets a pane read a map while the reader goroutine writes it.
//
// Run under -race, this fails without the clones in hub.jobList and
// hub.lookupJob. It also asserts the clone is a real copy, so the race
// detector is not the only thing standing between here and a regression.
func TestSnapshotJobsAreNotAliasedIntoTheIndex(t *testing.T) {
	t.Parallel()

	h := newHub(hubConfig{MaxMemory: 1 << 20, MaxJobs: 100, Clock: stats.RealClock{}, Decode: saltipc.DecodeValue})
	fake := saltipc.NewFake(start)

	feedData(t, fake, h, "salt/job/"+testJID+"/new", map[string]any{
		"jid": testJID, "fun": "state.apply", "minions": []any{"web-1", "web-2"},
	})

	var wg sync.WaitGroup

	wg.Add(2)

	go func() {
		defer wg.Done()

		for i := range 1000 {
			minion := fmt.Sprintf("web-%d", i)
			feedData(t, fake, h, "salt/job/"+testJID+"/ret/"+minion, map[string]any{
				"jid": testJID, "id": minion, "fun": "state.apply", "retcode": 0,
				"success": true, "return": true,
			})
		}
	}()

	go func() {
		defer wg.Done()

		for range 1000 {
			snap := h.Snapshot(filter.Query{}, 50)

			// Everything a pane does with a job, off the ingest goroutine. The
			// list reads rows; the drill-down reads the one job JobLookup
			// clones, which is the path that still hands out maps.
			for _, row := range snap.Jobs {
				_ = row.Failed
				_ = row.Returned
				_ = row.Complete
				_, _ = row.ExpectedCount()
			}

			if snap.JobLookup == nil {
				continue
			}

			if job, lookup := snap.JobLookup(testJID); lookup == stats.LookupFound {
				_ = job.Returns()
				_, _ = job.Missing()
				_ = job.Failed()
			}
		}
	}()

	wg.Wait()

	// The clone must be a copy, not a pointer the index keeps writing to.
	snap := h.Snapshot(filter.Query{}, 50)
	if len(snap.Jobs) != 1 {
		t.Fatalf("the snapshot lists %d jobs, want 1", len(snap.Jobs))
	}

	before := snap.Jobs[0].Returned

	feedData(t, fake, h, "salt/job/"+testJID+"/ret/late-1", map[string]any{
		"jid": testJID, "id": "late-1", "success": true, "return": true,
	})

	if after := snap.Jobs[0].Returned; after != before {
		t.Errorf("a job in an already-taken snapshot grew from %d to %d returns", before, after)
	}
}

// TestHubTakesConnectionStateFromTheReaderAndNowhereElse pins where the status
// bar's connectedness comes from.
//
// It used to be inferred: set by Event, cleared by Gap. Both halves were
// wrong. A quiet master read DISCONNECTED because no event had arrived yet,
// and a SUCCESSFUL reconnect read DISCONNECTED because Reader.Run closes the
// outage window by calling Gap. Each assertion below fails against that
// inference and passes against the socket-derived state.
func TestHubTakesConnectionStateFromTheReaderAndNowhereElse(t *testing.T) {
	t.Parallel()

	h := newHub(hubConfig{MaxMemory: 1 << 20, MaxJobs: 100, Clock: stats.RealClock{}})

	if h.Snapshot(filter.Query{}, 10).Connected {
		t.Error("a hub that has never been attached reports Connected")
	}

	// An event is not evidence of connectedness on its own — but nor may it
	// destroy it. The reader is the only authority.
	h.Event(model.Event{Arrival: time.Now(), Tag: "salt/auth"})

	if h.Snapshot(filter.Query{}, 10).Connected {
		t.Error("Connected = true from event arrival alone; it must come from the socket")
	}

	h.Attached(true)

	if !h.Snapshot(filter.Query{}, 10).Connected {
		t.Error("Connected = false while the reader holds the socket")
	}

	// The call that CLOSES an outage window must not read as an outage.
	now := time.Now()
	h.Gap(now.Add(-time.Second), now)

	if !h.Snapshot(filter.Query{}, 10).Connected {
		t.Error("a Gap report cleared Connected; Run closes an outage by calling Gap, " +
			"so this makes a successful reconnect read as a disconnection")
	}

	h.Attached(false)

	if h.Snapshot(filter.Query{}, 10).Connected {
		t.Error("Connected = true after the reader reported it lost the socket")
	}
}

func TestHubCountsDecodeErrors(t *testing.T) {
	t.Parallel()

	h := newHub(hubConfig{MaxMemory: 1 << 20, MaxJobs: 100, Clock: stats.RealClock{}})

	h.DecodeError(fmt.Errorf("bad frame"))
	h.DecodeError(fmt.Errorf("bad frame"))

	if got := h.Snapshot(filter.Query{}, 10).DecodeErrors; got != 2 {
		t.Errorf("DecodeErrors = %d, want 2", got)
	}
}

// TestExpectedMinionsAreReadFromARealPayload guards the decode shape.
//
// saltipc.DecodeValue returns map[interface{}]interface{} for every map, so a
// reader that asserted map[string]any would find nothing on every real event
// and leave every job's denominator unknown — while passing happily against a
// hand-built map. This test feeds a payload encoded exactly as Salt encodes it.
func TestExpectedMinionsAreReadFromARealPayload(t *testing.T) {
	t.Parallel()

	h, _ := newTestHub(t, 1<<20, 100)
	fake := saltipc.NewFake(start)

	feedData(t, fake, h, "salt/job/"+testJID+"/new", map[string]any{
		"jid":     testJID,
		"fun":     "state.apply",
		"tgt":     "webs",
		"user":    "root",
		"minions": []any{"web-1", "web-2", "web-3"},
	})

	job, lookup := h.Snapshot(filter.Query{}, 10).JobLookup(testJID)
	if lookup != stats.LookupFound {
		t.Fatalf("job lookup = %v, want found", lookup)
	}

	n, state := job.ExpectedCount()
	if state != model.ExpectedKnown || n != 3 {
		t.Errorf("ExpectedCount() = (%d, %v), want (3, known)", n, state)
	}

	if job.Tgt != "webs" || job.User != "root" {
		t.Errorf("job tgt=%q user=%q, want webs/root", job.Tgt, job.User)
	}
}

// TestExpectedMinionsHaveThreeStates is spec §5.3 case B: known, trimmed by the
// master, and never seen are three different facts with three different fixes,
// and the hub is where they are told apart.
func TestExpectedMinionsHaveThreeStates(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		data map[string]any
		want model.ExpectedState
	}{
		{
			name: "known",
			data: map[string]any{"jid": testJID, "minions": []any{"web-1"}},
			want: model.ExpectedKnown,
		},
		{
			// The master gutted the list because the new event exceeded
			// max_event_size. The fix is a master config change, not ours.
			name: "trimmed by the master",
			data: map[string]any{"jid": testJID, "minions": saltipc.TrimmedMarker},
			want: model.ExpectedTrimmed,
		},
		{
			name: "no minions key at all",
			data: map[string]any{"jid": testJID, "fun": "state.apply"},
			want: model.ExpectedUnseen,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h, _ := newTestHub(t, 1<<20, 100)

			feedData(t, saltipc.NewFake(start), h, "salt/job/"+testJID+"/new", tc.data)

			job, lookup := h.Snapshot(filter.Query{}, 10).JobLookup(testJID)
			if lookup != stats.LookupFound {
				t.Fatalf("job lookup = %v, want found", lookup)
			}

			if _, state := job.ExpectedCount(); state != tc.want {
				t.Errorf("ExpectedState = %v, want %v", state, tc.want)
			}
		})
	}
}

// TestAPartialMinionListIsReportedAsUnknown: a denominator that looks
// authoritative and is too small is worse than none. It renders as minions that
// are not missing when they are, which is invariant 10 with extra steps.
func TestAPartialMinionListIsReportedAsUnknown(t *testing.T) {
	t.Parallel()

	h, _ := newTestHub(t, 1<<20, 100)

	feedData(t, saltipc.NewFake(start), h, "salt/job/"+testJID+"/new", map[string]any{
		"jid":     testJID,
		"minions": []any{"web-1", 42, "web-3"},
	})

	job, _ := h.Snapshot(filter.Query{}, 10).JobLookup(testJID)

	if _, state := job.ExpectedCount(); state != model.ExpectedUnseen {
		t.Errorf("ExpectedState = %v, want unseen: a list we could not read in full "+
			"must not become a confident denominator", state)
	}
}

// TestSheddingPayloadsNeverChangesAJobCount is invariant 9 at the seam where
// the cache and the job index meet: a 1,000-target highstate blows the budget,
// every payload is shed, and "which of my minions failed" must still be exactly
// answerable (spec §5.2).
func TestSheddingPayloadsNeverChangesAJobCount(t *testing.T) {
	t.Parallel()

	// Big enough that the job index's own ceiling (10% of this) comfortably
	// holds a 1,000-target job, and small enough that the 812 return payloads
	// below blow straight through the event budget. Shrinking the budget
	// instead would shrink the job ceiling with it and evict the job under
	// test, which would prove nothing about shedding.
	h, _ := newTestHub(t, 4<<20, 100)
	fake := saltipc.NewFake(start)

	expected := make([]any, 0, 1000)
	for i := range 1000 {
		expected = append(expected, fmt.Sprintf("web-%04d", i))
	}

	feedData(t, fake, h, "salt/job/"+testJID+"/new", map[string]any{
		"jid": testJID, "fun": "state.apply", "tgt": "webs", "minions": expected,
	})

	for i := range 812 {
		minion := fmt.Sprintf("web-%04d", i)

		code := 0
		if i < 23 {
			code = 1
		}

		feedData(t, fake, h, "salt/job/"+testJID+"/ret/"+minion, map[string]any{
			"jid": testJID, "id": minion, "fun": "state.apply",
			"retcode": code, "success": code == 0,
			// 812 × 8 KiB is well past the 4 MiB budget.
			"return": strings.Repeat("x", 8192),
		})
	}

	snap := h.Snapshot(filter.Query{}, 100)

	if snap.Cache.Shed == 0 {
		t.Fatal("premise failed: nothing was shed, so this proves nothing")
	}

	job, lookup := snap.JobLookup(testJID)
	if lookup != stats.LookupFound {
		t.Fatalf("job lookup = %v, want found", lookup)
	}

	n, state := job.ExpectedCount()
	if state != model.ExpectedKnown || n != 1000 {
		t.Errorf("ExpectedCount() = (%d, %v), want (1000, known)", n, state)
	}

	if got := job.Returned(); got != 812 {
		t.Errorf("Returned() = %d, want 812", got)
	}

	if got := job.Failed(); got != 23 {
		t.Errorf("Failed() = %d, want 23", got)
	}

	missing, known := job.Missing()
	if !known || len(missing) != 188 {
		t.Errorf("Missing() = (%d, %v), want (188, true)", len(missing), known)
	}

	if len(missing) > 0 && missing[0] != "web-0812" {
		t.Errorf("first missing minion = %q, want web-0812", missing[0])
	}
}

// TestPinnedJobsSurviveEviction is Amendment 2 item 2 end to end: the Jobs pane
// reports what it is drilled into, the root pushes it here every tick, and the
// index must honour it. Without the PinJob call the pinned job evicts exactly
// as if it were never pinned — pinning that looks like it works.
func TestPinnedJobsSurviveEviction(t *testing.T) {
	t.Parallel()

	const maxJobs = 4

	h, _ := newTestHub(t, 1<<20, maxJobs)
	fake := saltipc.NewFake(start)

	// A complete job is the FIRST thing the index evicts, so pinning has to
	// beat the strongest eviction preference to prove anything.
	pinned := "20260830080000000001"

	feedData(t, fake, h, "salt/job/"+pinned+"/new", map[string]any{
		"jid": pinned, "fun": "test.ping", "minions": []any{"web-1"},
	})
	feedData(t, fake, h, "salt/job/"+pinned+"/ret/web-1", map[string]any{
		"jid": pinned, "id": "web-1", "success": true, "return": true,
	})

	h.PinJob(pinned)

	for i := range maxJobs * 3 {
		jid := fmt.Sprintf("2026083008000000%04d", i+100)
		feedData(t, fake, h, "salt/job/"+jid+"/new", map[string]any{
			"jid": jid, "fun": "test.ping", "minions": []any{"web-1"},
		})
		feedData(t, fake, h, "salt/job/"+jid+"/ret/web-1", map[string]any{
			"jid": jid, "id": "web-1", "success": true, "return": true,
		})
	}

	snap := h.Snapshot(filter.Query{}, 100)

	if snap.JobStats.Evicted == 0 {
		t.Fatal("premise failed: nothing was evicted, so the pin was never tested")
	}

	if _, lookup := snap.JobLookup(pinned); lookup != stats.LookupFound {
		t.Errorf("the pinned job was %v; a job being read must never be evicted", lookup)
	}

	// And unpinning releases it.
	h.PinJob("")

	for i := range maxJobs * 2 {
		jid := fmt.Sprintf("2026083008000000%04d", i+500)
		feedData(t, fake, h, "salt/job/"+jid+"/new", map[string]any{
			"jid": jid, "fun": "test.ping", "minions": []any{"web-1"},
		})
		feedData(t, fake, h, "salt/job/"+jid+"/ret/web-1", map[string]any{
			"jid": jid, "id": "web-1", "success": true, "return": true,
		})
	}

	if _, lookup := h.Snapshot(filter.Query{}, 100).JobLookup(pinned); lookup == stats.LookupFound {
		t.Error("the job stayed pinned after the pin was cleared")
	}
}

// TestSnapshotCarriesTheIngestClock: Amendment 2 item 3. The reading comes from
// the same clock the reader stamps arrival with, which is what lets a still
// returning job's duration count up between ticks (spec §7.5).
func TestSnapshotCarriesTheIngestClock(t *testing.T) {
	t.Parallel()

	h, clk := newTestHub(t, 1<<20, 100)

	if got := h.Snapshot(filter.Query{}, 10).Now; !got.Equal(start) {
		t.Errorf("Snapshot.Now = %v, want the clock's %v", got, start)
	}

	clk.Advance(4 * time.Minute)

	if got := h.Snapshot(filter.Query{}, 10).Now; !got.Equal(start.Add(4 * time.Minute)) {
		t.Errorf("Snapshot.Now = %v, want it to follow the clock", got)
	}
}

// update drives one message through the root model.
func update(t *testing.T, m ui.Model, msg tea.Msg) ui.Model {
	t.Helper()

	next, _ := m.Update(msg)

	out, ok := next.(ui.Model)
	if !ok {
		t.Fatal("Update did not return a ui.Model")
	}

	return out
}

// countingSource wraps the hub and records how often the UI pulled from it.
type countingSource struct {
	*hub

	mu    sync.Mutex
	calls int
}

func (c *countingSource) Snapshot(q filter.Query, limit int) ui.Snapshot {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()

	return c.hub.Snapshot(q, limit)
}

func (c *countingSource) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.calls
}

// TestTheUIPullsOncePerTickWhateverTheEventRate is invariant 6: the UI receives
// NO per-event messages. Five thousand events between two ticks must cost the
// UI exactly the two pulls, which is why a storm cannot wedge the console.
func TestTheUIPullsOncePerTickWhateverTheEventRate(t *testing.T) {
	t.Parallel()

	h, _ := newTestHub(t, 1<<30, 100)
	src := &countingSource{hub: h}

	m := ui.NewModel(src, panesFor(), ui.Options{Interval: time.Hour})
	m = update(t, m, tea.WindowSizeMsg{Width: 120, Height: 30})

	for range 5000 {
		h.Event(model.Event{Arrival: start, Tag: "salt/auth"})
	}

	m = update(t, m, ui.TickMsg(start))
	_ = update(t, m, ui.TickMsg(start))

	if got := src.count(); got != 2 {
		t.Errorf("the UI pulled %d snapshots across 2 ticks and 5000 events, want 2", got)
	}
}

// TestPausingFreezesTheViewButNotIngest is invariant 7, which only exists at
// this seam: the pane layer cannot tell whether ingest stopped, and the ingest
// layer cannot tell whether the view is paused.
//
// It asserts BOTH halves. A paused console that also stopped collecting would
// silently lose the storm the operator paused in order to read.
func TestPausingFreezesTheViewButNotIngest(t *testing.T) {
	t.Parallel()

	h, _ := newTestHub(t, 1<<30, 100)

	m := ui.NewModel(h, panesFor(), ui.Options{Interval: time.Hour})
	m = update(t, m, tea.WindowSizeMsg{Width: 120, Height: 30})

	for range 5 {
		h.Event(model.Event{Arrival: start, Tag: "salt/auth"})
	}

	m = update(t, m, ui.TickMsg(start))

	if !strings.Contains(m.View(), "5 events") {
		t.Fatalf("premise failed: the status bar does not report 5 events:\n%s", m.View())
	}

	m = update(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{' '}})

	for range 7 {
		h.Event(model.Event{Arrival: start, Tag: "salt/auth"})
	}

	m = update(t, m, ui.TickMsg(start))

	// Half one: the VIEW is frozen.
	if !strings.Contains(m.View(), "5 events") {
		t.Errorf("the paused view moved: it should still report 5 events\n%s", m.View())
	}

	// Half two: INGEST is not. The hub has all twelve, and unpausing shows them.
	if got := h.Snapshot(filter.Query{}, 100).Cache.Events; got != 12 {
		t.Errorf("the hub holds %d events, want 12: pausing must never stop ingest", got)
	}

	m = update(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{' '}})
	m = update(t, m, ui.TickMsg(start))

	if !strings.Contains(m.View(), "12 events") {
		t.Errorf("unpausing did not reveal the events collected while paused:\n%s", m.View())
	}
}

// TestDrillThroughReachesTheDetailPane is Amendment 2 item 1 end to end: a
// tea.Cmd returned by Live must reach the ROOT, not be handed back to Live.
// Before the root grew a case for it, this drill-through could not be bound at
// all.
func TestDrillThroughReachesTheDetailPane(t *testing.T) {
	t.Parallel()

	h, _ := newTestHub(t, 1<<20, 100)
	fake := saltipc.NewFake(start)

	feedData(t, fake, h, "salt/job/"+testJID+"/ret/web-1", map[string]any{
		"jid": testJID, "id": "web-1", "fun": "state.apply",
		"retcode": 0, "success": true, "return": "the payload the operator wants to read",
	})

	m := ui.NewModel(h, panesFor(), ui.Options{Interval: time.Hour})
	m = update(t, m, tea.WindowSizeMsg{Width: 120, Height: 30})
	m = update(t, m, ui.TickMsg(start))

	// Live is pane 1 and starts focused.
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("enter in Live returned no command")
	}

	m = update(t, next.(ui.Model), cmd())

	view := m.View()

	if !strings.Contains(view, "the payload the operator wants to read") {
		t.Errorf("the drill-through did not reach Detail:\n%s", view)
	}
}

// TestIngestAccumulatesNothingBetweenSnapshots is invariant 7's slow half.
//
// A PAUSED console takes no snapshots while ingest keeps running, so anything
// the ingest path accumulates for the benefit of the NEXT snapshot grows for as
// long as the pause lasts — a leak that only appears when an operator pauses
// during exactly the storm they paused to read, and a first-snapshot-after-the-
// pause that costs more the longer they waited. The hub used to keep a dirty
// set and a clone cache and had to bound them explicitly; it keeps neither now,
// and this pins that rather than the bound: the first snapshot after 5,000
// un-snapshotted jobs must cost what one after 200 costs.
func TestIngestAccumulatesNothingBetweenSnapshots(t *testing.T) {
	t.Parallel()

	feedJobs := func(n int) *hub {
		h, _ := newTestHub(t, 1<<24, 10000)
		fake := saltipc.NewFake(start)

		for i := range n {
			jid := fmt.Sprintf("2026083008%010d", i)
			feedData(t, fake, h, "salt/job/"+jid+"/new", map[string]any{
				"jid": jid, "fun": "test.ping", "minions": []any{"web-1"},
			})
		}

		return h
	}

	brief := firstSnapshotAllocBytes(t, feedJobs(listJobs))
	longPause := firstSnapshotAllocBytes(t, feedJobs(5000))

	if brief == 0 {
		t.Fatal("premise failed: a snapshot allocated nothing at all")
	}

	if longPause > 2*brief {
		t.Errorf("the first snapshot after 5000 un-snapshotted jobs allocates %d bytes "+
			"against %d after %d — ingest is accumulating per-job state for a "+
			"snapshot that a paused console may never take",
			longPause, brief, listJobs)
	}

	// And it still hands out correct rows afterwards.
	h := feedJobs(2000)

	snap := h.Snapshot(filter.Query{}, 10)
	if len(snap.Jobs) != listJobs {
		t.Errorf("the snapshot lists %d jobs, want %d", len(snap.Jobs), listJobs)
	}

	if len(snap.Jobs) > 0 && snap.Jobs[0].Fun != "test.ping" {
		t.Errorf("the newest listed job is %+v, want a test.ping", snap.Jobs[0])
	}
}

// firstSnapshotAllocBytes reports the bytes the very FIRST snapshot a hub takes
// allocates. Unlike snapshotAllocBytes it runs no warm-up, because the state
// being measured is precisely what ingest built up before any snapshot ran.
func firstSnapshotAllocBytes(tb testing.TB, h *hub) uint64 {
	tb.Helper()

	var before, after runtime.MemStats

	runtime.GC()
	runtime.ReadMemStats(&before)

	h.Snapshot(filter.Query{}, 10)

	runtime.ReadMemStats(&after)

	return after.TotalAlloc - before.TotalAlloc
}

// benchHub fills a hub with n events through the real ingest path.
func benchHub(b *testing.B, n int) *hub {
	b.Helper()

	h := newHub(hubConfig{
		MaxMemory: 1 << 30, MaxJobs: 500,
		Clock: stats.NewFakeClock(start), Decode: saltipc.DecodeValue,
	})

	fake := saltipc.NewFake(start)

	for i := range n {
		if err := fake.FeedData(h, fmt.Sprintf("salt/minion/web-%d/start", i%1000),
			map[string]any{"id": fmt.Sprintf("web-%d", i%1000)}); err != nil {
			b.Fatalf("feed: %v", err)
		}
	}

	return h
}

// BenchmarkHubSnapshotHoldsTheIngestLock measures the thing invariant 6 is
// actually about: hub.Snapshot runs entirely under h.mu, so its wall time IS
// the time the reader goroutine is blocked, ten times a second.
//
// The selective case is the one that matters. It used to walk the whole ring
// looking for matches it would never find — 22.1 ms per tick on a full default
// 256 MiB cache, and linear in --max-memory beyond that. Run both sizes: a
// regression shows up as the selective case growing with the cache while the
// unfiltered one does not.
func BenchmarkHubSnapshotHoldsTheIngestLock(b *testing.B) {
	for _, events := range []int{50_000, 500_000} {
		h := benchHub(b, events)

		b.Run(fmt.Sprintf("%d-events/unfiltered", events), func(b *testing.B) {
			for range b.N {
				h.Snapshot(filter.Query{}, 2000)
			}
		})

		q, err := filter.Parse("minion:no-such-minion")
		if err != nil {
			b.Fatalf("Parse: %v", err)
		}

		b.Run(fmt.Sprintf("%d-events/selective", events), func(b *testing.B) {
			for range b.N {
				h.Snapshot(q, 2000)
			}
		})
	}
}

// BenchmarkHubExportHoldsTheIngestLockOnlyForTheCopy separates the export
// path's two costs.
//
// copyAll is the part that runs under h.mu, so its time is the ingest stall.
// AllEvents is copy PLUS the match of every retained event, and the matching
// used to be inside the lock too — which is why the hold was several times the
// copy. A regression that moves it back shows up as the two rows converging.
func BenchmarkHubExportHoldsTheIngestLockOnlyForTheCopy(b *testing.B) {
	for _, events := range []int{50_000, 500_000} {
		h := benchHub(b, events)

		q, err := filter.Parse("minion:no-such-minion")
		if err != nil {
			b.Fatalf("Parse: %v", err)
		}

		b.Run(fmt.Sprintf("%d-events/lock-held", events), func(b *testing.B) {
			for range b.N {
				h.copyAll()
			}
		})

		b.Run(fmt.Sprintf("%d-events/whole-call", events), func(b *testing.B) {
			for range b.N {
				h.AllEvents(q)
			}
		})
	}
}

// jobsHub fills a hub with jobs jobs, each announcing minions targets and
// having every one of them return, through the REAL ingest path.
//
// It is the shape that makes the ingest lock expensive: an orchestration-heavy
// master with many simultaneous fleet-wide jobs. Every job is left dirty,
// because that is the state a busy master is permanently in.
func jobsHub(tb testing.TB, jobs, minions int) *hub {
	tb.Helper()

	h := newHub(hubConfig{
		MaxMemory: 1 << 30, MaxJobs: 5000,
		Clock: stats.NewFakeClock(start), Decode: saltipc.DecodeValue,
	})

	fake := saltipc.NewFake(start)

	targets := make([]any, 0, minions)
	for m := range minions {
		targets = append(targets, fmt.Sprintf("web-%d", m))
	}

	for j := range jobs {
		jid := fmt.Sprintf("2026083008%010d", j)

		if err := fake.FeedData(h, "salt/job/"+jid+"/new", map[string]any{
			"jid": jid, "fun": "state.apply", "tgt": "*", "minions": targets,
		}); err != nil {
			tb.Fatalf("feed job/new: %v", err)
		}

		for m := range minions {
			minion := fmt.Sprintf("web-%d", m)
			if err := fake.FeedData(h, "salt/job/"+jid+"/ret/"+minion, map[string]any{
				"jid": jid, "id": minion, "retcode": 0, "success": true,
			}); err != nil {
				tb.Fatalf("feed job/ret: %v", err)
			}
		}
	}

	return h
}

// touchEveryJob re-delivers one already-seen return for each of the first n
// jobs, which is what puts them back in the ingest layer's dirty set without
// changing any job's size. It is the steady state of a busy master: between two
// 100 ms ticks, every job on screen has moved.
func touchEveryJob(tb testing.TB, h *hub, n int) {
	tb.Helper()

	fake := saltipc.NewFake(start)

	for j := range n {
		jid := fmt.Sprintf("2026083008%010d", j)
		if err := fake.FeedData(h, "salt/job/"+jid+"/ret/web-0", map[string]any{
			"jid": jid, "id": "web-0", "retcode": 0, "success": true,
		}); err != nil {
			tb.Fatalf("feed job/ret: %v", err)
		}
	}
}

// snapshotAllocBytes reports the bytes one Snapshot allocates with every job
// freshly moved.
//
// Bytes rather than wall time on purpose: a timing assertion on a shared CI
// runner is either flaky or so loose it cannot fail. What Snapshot allocates is
// deterministic, and it is the direct proxy for the cost this is about — the
// old path built two maps per job sized by that job's minion set, so its
// allocation and its held-lock time scale together and by the same factor.
//
// touchEveryJob runs OUTSIDE the measured window, twice: once before the
// warm-up snapshot that settles one-off allocations, and once after it, so the
// snapshot being measured is one where every listed job has changed.
func snapshotAllocBytes(tb testing.TB, h *hub, jobs int) uint64 {
	tb.Helper()

	touchEveryJob(tb, h, jobs)
	h.Snapshot(filter.Query{}, 10)
	touchEveryJob(tb, h, jobs)

	var before, after runtime.MemStats

	runtime.GC()
	runtime.ReadMemStats(&before)

	h.Snapshot(filter.Query{}, 10)

	runtime.ReadMemStats(&after)

	return after.TotalAlloc - before.TotalAlloc
}

// TestTheSnapshotJobListDoesNotScaleWithMinionCount is invariant 6 on the job
// path, which is where cache.Snapshot's already-fixed unbounded scan had its
// twin.
//
// hub.jobList runs entirely under h.mu, so its cost IS the reader goroutine's
// stall — measured at 22.55 ms at 200 jobs x 1000 minions against a 100 ms
// tick. The list view renders counts (returned, expected, failed, a duration)
// and never a minion NAME: the per-minion breakdown is the drill-down, which
// goes through JobLookup and clones exactly one job on demand. So a tick must
// not pay for the minion sets of two hundred jobs in order to draw thirty rows
// of numbers.
//
// Both loads are run because they were different bugs. At 200 jobs the clone
// cache is retained and every entry in it is stale, so all 200 are re-cloned.
// At 300 the dirty set exceeds listJobs, the guard DISCARDS the cache, and all
// 200 are re-cloned anyway — the optimisation degrading to full cost under
// exactly the load it exists for.
//
// The assertion is on the SHAPE (flat in minion count), not on a byte budget,
// so it fails on a return to O(job size) without failing on an extra row field.
//
// This test and its subtests must NOT call t.Parallel(), and that is not an
// oversight to be tidied away. runtime.ReadMemStats reports TotalAlloc for the
// whole PROCESS, so anything else allocating during the measured window lands
// in the delta. With the subtests parallel, each one's allocations polluted the
// other's measurement and the test failed roughly four runs in five — while
// still passing often enough to look merely flaky. Running it serially makes
// the measurement deterministic: 5/5 green at -parallel 1.
func TestTheSnapshotJobListDoesNotScaleWithMinionCount(t *testing.T) {
	const (
		lean = 10
		fat  = 1000
	)

	loads := map[string]int{
		"every listed job dirty": listJobs,
		"more dirty than listed": listJobs + 100,
	}

	for name, jobs := range loads {
		t.Run(name, func(t *testing.T) {
			small := snapshotAllocBytes(t, jobsHub(t, jobs, lean), jobs)
			large := snapshotAllocBytes(t, jobsHub(t, jobs, fat), jobs)

			if small == 0 {
				t.Fatal("premise failed: a snapshot allocated nothing at all")
			}

			if large > 2*small {
				t.Errorf("a snapshot of %d jobs allocates %d bytes at %d minions each "+
					"but %d at %d — the tick is paying O(job size) rather than "+
					"O(visible rows), and every byte of it is held ingest lock",
					jobs, large, fat, small, lean)
			}
		})
	}
}

// TestTheJobNewDecodeHappensOutsideTheIngestLock.
//
// factsFor is the ONE msgpack decode at ingest (invariant 4's recorded
// deviation — spec §7.5's denominator has no other source), and it was run
// while h.mu was held: 5.2 ms at 20,000 minions, on a tool that runs as root on
// a production master. It touches no hub state but h.decode, which is set once
// in newHub and never written.
//
// The observable is the LOCK, not the returned facts, so this asserts on the
// lock directly: while a decode is in flight, another goroutine must be able to
// take h.mu. Held, this test times out; hoisted, it returns at once.
func TestTheJobNewDecodeHappensOutsideTheIngestLock(t *testing.T) {
	t.Parallel()

	var once sync.Once

	decoding := make(chan struct{})
	release := make(chan struct{})

	h := newHub(hubConfig{
		MaxMemory: 1 << 20, MaxJobs: 100, Clock: stats.NewFakeClock(start),
		Decode: func(b []byte) (any, error) {
			once.Do(func() { close(decoding) })
			<-release

			return saltipc.DecodeValue(b)
		},
	})

	fed := make(chan struct{})

	go func() {
		defer close(fed)

		fake := saltipc.NewFake(start)
		if err := fake.FeedData(h, "salt/job/"+testJID+"/new", map[string]any{
			"jid": testJID, "fun": "test.ping", "minions": []any{"web-1"},
		}); err != nil {
			t.Errorf("feed job/new: %v", err)
		}
	}()

	<-decoding

	took := make(chan struct{})

	go func() {
		h.Attached(true)
		close(took)
	}()

	select {
	case <-took:
	case <-time.After(2 * time.Second):
		t.Error("the ingest lock was held for the whole job/new decode: nothing " +
			"else can touch the hub while a 20,000-minion payload is unpacked")
	}

	close(release)
	<-fed
	<-took
}

// benchJobListLoads is the ladder the final review measured hub.jobList on.
var benchJobListLoads = []struct{ jobs, minions int }{
	{10, 100}, {200, 100}, {200, 1000}, {300, 1000},
}

// BenchmarkHubSnapshotHoldsTheIngestLockForTheJobList measures the job half of
// the tick's lock hold, the way BenchmarkHubSnapshotHoldsTheIngestLock measures
// the cache half: Snapshot runs entirely under h.mu, so its wall time IS the
// time the reader goroutine is blocked, ten times a second.
//
// Every listed job is put back in the dirty set before each timed iteration,
// with the timer stopped, because a clean dirty set measures nothing — a busy
// master never has one.
func BenchmarkHubSnapshotHoldsTheIngestLockForTheJobList(b *testing.B) {
	for _, load := range benchJobListLoads {
		h := jobsHub(b, load.jobs, load.minions)

		b.Run(fmt.Sprintf("%d-jobs/%d-minions", load.jobs, load.minions), func(b *testing.B) {
			for range b.N {
				b.StopTimer()
				touchEveryJob(b, h, load.jobs)
				b.StartTimer()

				h.Snapshot(filter.Query{}, 2000)
			}
		})
	}
}

// BenchmarkHubIngestLockHeldPerJobNew measures the OTHER thing on this lock,
// and it cannot be measured by timing the call: after the decode is hoisted the
// work still happens inside hub.Event, just not inside h.mu. So this times what
// a competing goroutine — the render tick, in the real program — actually waits
// for when it asks for the lock while job/new events are being ingested.
//
// 20,000 minions is the final review's figure for the payload that cost 5.2 ms
// of held lock.
func BenchmarkHubIngestLockHeldPerJobNew(b *testing.B) {
	const minions = 20_000

	h := newHub(hubConfig{
		MaxMemory: 1 << 30, MaxJobs: 5000,
		Clock: stats.NewFakeClock(start), Decode: saltipc.DecodeValue,
	})

	fake := saltipc.NewFake(start)

	targets := make([]any, 0, minions)
	for m := range minions {
		targets = append(targets, fmt.Sprintf("web-%d", m))
	}

	var (
		waited time.Duration
		worst  time.Duration
		takes  int64
	)

	stop := make(chan struct{})
	done := make(chan struct{})

	go func() {
		defer close(done)

		for {
			select {
			case <-stop:
				return
			default:
			}

			at := time.Now()
			h.Attached(true)

			since := time.Since(at)
			waited += since
			takes++

			if since > worst {
				worst = since
			}

			time.Sleep(50 * time.Microsecond)
		}
	}()

	b.ResetTimer()

	for i := range b.N {
		jid := fmt.Sprintf("2026083008%010d", i)
		if err := fake.FeedData(h, "salt/job/"+jid+"/new", map[string]any{
			"jid": jid, "fun": "state.apply", "minions": targets,
		}); err != nil {
			b.Fatalf("feed job/new: %v", err)
		}
	}

	b.StopTimer()
	close(stop)
	<-done

	if takes > 0 {
		b.ReportMetric(float64(waited.Nanoseconds())/float64(takes)/1e6, "ms/lock-wait")
		b.ReportMetric(float64(worst.Nanoseconds())/1e6, "ms/worst-wait")
	}
}
