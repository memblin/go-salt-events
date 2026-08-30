# go-salt-events — handoff to a larger machine

Written 2026-08-30 after the controlling session died twice mid-run on a
2 vCPU / 2 GB host. Everything below is recovered from git and the ledger, not
from conversation memory.

## Read these first, in this order

1. `.superpowers/sdd/2026-08-30-salt-events/progress.md` — **the ledger.** The
   authority on progress. Contains the pre-flight conflict scan, every ruling
   made so far with its cost-if-wrong, the wave plan, and standing decisions.
2. `docs-extra/superpowers/2026-08-30-salt-events-design.md` — the spec. The
   binding authority; the plan argues from it.
3. `docs-extra/superpowers/plans/2026-08-30-salt-events.md` — the 22-task plan.
4. `git log --oneline` — what actually landed.

## ⚠️ These do NOT travel with a git push

`docs-extra/superpowers/` and `.superpowers/sdd/` are both git-ignored. The
spec, the plan, all 22 pre-generated task briefs, every implementer/reviewer
report, and the ledger itself exist **only on the original host**. A clone of
the pushed branch cannot continue this work.

Copy both directories to the new machine before resuming, e.g.:

```
rsync -av --include='docs-extra/***' --include='.superpowers/***' \
      tkcadmin@<old-host>:~/repos/gitea/TKC-Labs/go-salt-events/ \
      <dest>/go-salt-events/
```

Do not regenerate the briefs — they are already extracted and the ledger's
rulings reference them by number.

## Status: 8 of 22 tasks done

Committed, reviewed, approved: Tasks 1, 2, 4, 5, 13.
Committed, review outstanding: Task 6 (`internal/config`) — it was dispatched
but the session died before the reviewer returned. **Re-dispatch that review.**
Committed after this handoff: Tasks 8 + 9 (`internal/stats`) plus their fix
round.

Not started (14): Task 3 (`salttag`), 7 (reader), 10 (jobs index), 11 (cache),
12 (filter), 14–19 (UI: components, root, live/detail, rate, summary, jobs),
20 (export), 21 (wiring), 22 (integration + docs).

## Remaining wave plan

```
W2 remainder: T3
W3:  T10 | T11 | T20      (T11 before T12 — see ledger ruling)
W4:  T7  | T12 | T14
W5:  T15                  (solo; owns the pane contract)
W6:  T16 | T17 | T18
W7:  T19 then T21
W8:  T22
```

## Live-fire work still owed (Task 22)

The original host has a running Salt 3006.27 master and passwordless sudo. The
integration test and `just capture` read `/var/run/salt/master/master_event_pub.ipc`
read-only and were authorised to run unattended. **If the new machine has no
salt-master, Task 22's integration test will skip rather than fail** — which
means the wire-format decoding never gets proven against a real bus. Either run
Task 22 back on the original host, or accept that gap explicitly and say so.

## Standing user decisions

- Concurrency cap **3**. The user set this explicitly after watching the host
  bog down; it is an instruction, not a ruling. Re-evaluate only if the user
  says so — do not raise it because the new machine looks roomier.
- Implementers never run `git add`/`git commit`; the controller commits each
  task. Concurrent agents in one checkout race on `.git/index.lock`.
- Merge to `main` locally once the final whole-branch review is clean.
- As of this handoff: push to `origin`
  (`ssh://git@git.tkclabs.io:2222/TKC-Labs/go-salt-event.git`) is authorised.
  Note the remote repo name is singular (`go-salt-event`) while the module is
  plural (`go-salt-events`) — confirm that is intended.

## Two rulings that constrain upcoming tasks

**T13 parked Important — the colour rule has an API-level escape hatch.**
Any package can obtain a styled colour without writing `lipgloss.Color(`, via
`theme.Compile(theme.Palette{...})`, or a zero-value `Palette` with field
assignment, or by mutating a copy from `theme.Get()`. Two of those three routes
contain no `theme.Palette{` text, so no forbidigo pattern closes them all.
Worse than a theme-switching bug: a hand-built palette never passes through the
registry, and the contrast test iterates `theme.Names()`, so such a palette is
structurally invisible to contrast validation — an unreadable pane is possible
with no test that would catch it. Unfixable inside T13 (unexporting `Palette`'s
fields breaks the brief's own external `theme_test.go`).

The user has chosen to **enforce it properly in T15**. T15's dispatch must
require: the root model is the sole caller of `theme.Compile`; a forbidigo
pattern on `theme.Palette{` added as a partial tripwire and documented as
partial, not airtight; and a test asserting panes receive styles only from the
root. T14's dispatch must forbid `components` from constructing a `Palette` or
calling `Compile` — it takes `*theme.Styles` as a parameter.

**T8 concurrency contract — documented, not mutexed.** `Rings` and `Counter`
are explicitly not safe for concurrent use. The lock lives at Task 21's hub:
the reader folds events into cache and stats behind one mutex, and the UI reads
a snapshot assembled under that same lock. **Task 21 must serialise access** —
concurrent map access on `Counter.counts` crashes outright rather than merely
racing.

**Carry into T17 (Rate pane):** `Summary` now has `NowIsGap` and `HasData`. The
pane must check them before rendering a number and must never print `0` for a
window it did not observe. Zero means "we watched and nothing happened"; a gap
means "we weren't looking".

## Process notes learned the hard way

- Reviewer replies arrive **truncated** if long. Instruct every reviewer to
  write its full report to
  `.superpowers/sdd/2026-08-30-salt-events/review-task-<N>-findings.md` and
  reply with only verdicts plus one-line headlines.
- For a scoped re-review, `FIX_BASE` must be the commit immediately **before**
  the fix commit — not "the head the previous review saw", which by then has
  other tasks' commits between it and the fix.
- `/usr/local/go/bin` is not on PATH by default on the original host.
