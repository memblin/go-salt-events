package main

import (
	"fmt"
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

			// Everything a pane does with a job, off the ingest goroutine.
			for _, job := range snap.Jobs {
				_ = job.Failed()
				_ = job.Returns()
				_, _ = job.Missing()
			}

			if snap.JobLookup == nil {
				continue
			}

			if job, lookup := snap.JobLookup(testJID); lookup == stats.LookupFound {
				_ = job.Returns()
			}
		}
	}()

	wg.Wait()

	// The clone must be a copy, not a pointer the index keeps writing to.
	snap := h.Snapshot(filter.Query{}, 50)
	if len(snap.Jobs) != 1 {
		t.Fatalf("the snapshot lists %d jobs, want 1", len(snap.Jobs))
	}

	before := snap.Jobs[0].Returned()

	feedData(t, fake, h, "salt/job/"+testJID+"/ret/late-1", map[string]any{
		"jid": testJID, "id": "late-1", "success": true, "return": true,
	})

	if after := snap.Jobs[0].Returned(); after != before {
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

// TestTheJobCopyCacheStaysBoundedWhileNothingSnapshots: the dirty set is
// emptied by a snapshot, and a PAUSED console takes none — invariant 7 keeps
// ingest running the whole time. Unbounded, that is a slow leak that only
// appears when an operator pauses during exactly the storm they paused to read.
func TestTheJobCopyCacheStaysBoundedWhileNothingSnapshots(t *testing.T) {
	t.Parallel()

	h, _ := newTestHub(t, 1<<20, 5000)
	fake := saltipc.NewFake(start)

	for i := range 2000 {
		jid := fmt.Sprintf("2026083008%010d", i)
		feedData(t, fake, h, "salt/job/"+jid+"/new", map[string]any{
			"jid": jid, "fun": "test.ping", "minions": []any{"web-1"},
		})
	}

	if got := len(h.jobDirty); got > listJobs {
		t.Errorf("the dirty set holds %d jobs after 2000 arrived with no snapshot, want at most %d",
			got, listJobs)
	}

	// And it still hands out correct, un-aliased copies afterwards.
	snap := h.Snapshot(filter.Query{}, 10)
	if len(snap.Jobs) != listJobs {
		t.Errorf("the snapshot lists %d jobs, want %d", len(snap.Jobs), listJobs)
	}

	if len(snap.Jobs) > 0 && snap.Jobs[0].Fun != "test.ping" {
		t.Errorf("the newest listed job is %+v, want a test.ping", snap.Jobs[0])
	}
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
