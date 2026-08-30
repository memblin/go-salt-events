package ui

import "time"

// TickMsg drives the render loop. It is the ONLY message that moves data into
// the UI: the reader goroutine never sends per-event messages, so the bubbletea
// queue cannot grow with event rate (spec §4.1, invariant 6).
type TickMsg time.Time

// snapshotLimit bounds how many events a snapshot carries. The Live pane can
// only show a screenful; copying the whole cache every tick would make render
// cost scale with cache size rather than viewport size.
const snapshotLimit = 2000

// defaultInterval is used when Options.Interval is zero or negative.
// tea.Tick with a non-positive duration fires in a tight loop, which would
// pin a core and starve the reader goroutine — the exact failure mode the
// snapshot architecture exists to avoid.
const defaultInterval = 100 * time.Millisecond
