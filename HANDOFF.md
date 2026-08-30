# go-salt-events — handoff

**Temporary file. Delete once the work is resumed elsewhere.**

Written 2026-08-30. The controlling session ran on a 2 vCPU / 2 GB host and
died twice mid-run; work is moving to a larger machine. Nothing was lost.

Source host for anything referenced below: `tkcadmin@salt-1.tkclabs.io`
Repo path there: `/home/tkcadmin/repos/gitea/TKC-Labs/go-salt-events`

---

## 1. Disposition

**8 of 22 tasks complete.** All work is on `feat/initial-implementation`,
11 commits ahead of `main`, pushed to
`ssh://git@git.tkclabs.io:2222/TKC-Labs/go-salt-event.git`.
(The remote repo name is singular, `go-salt-event`; the Go module is plural,
`github.com/TKC-Labs/go-salt-events`. Harmless, but confirm it was intended.)

`just check` is **green** across every package: fmt, vet, lint, and all tests.

| Task | Package | State |
|---|---|---|
| 1 | scaffold, `just` gate, depguard/forbidigo rules | done, reviewed, approved |
| 2 | `internal/model` | done, reviewed, approved |
| 4 + 5 | `internal/saltipc` | done, reviewed; 1 Important found, fixed, re-review confirmed |
| 13 | `internal/theme` | done, reviewed; 1 Important **parked** → §6 |
| 6 | `internal/config` | done, committed, **review never ran** → §5 |
| 8 + 9 | `internal/stats` | done, committed, fixed; **re-review never ran** → §5 |

**Not started — 14 tasks:** 3 (`salttag`), 7 (socket reader), 10 (job index),
11 (cache), 12 (filter), 14 (UI components), 15 (root model + pane contract),
16 (live/detail panes), 17 (rate pane), 18 (summary pane), 19 (jobs pane),
20 (export), 21 (wiring + ingest hub), 22 (integration test + docs).

### Commits

```
9134a47  docs: add temporary handoff notes for machine migration
817b436  fix(stats): distinguish gaps from zero in Summary and bound ring walks
dfa2433  feat(config): resolve sock_dir, SUDO_USER config paths, and precedence
c3a6b5d  fix(saltipc): consume whole values in ExtractFields on type mismatch
72d1dd9  feat(stats): add bounded-cardinality top-N counters
b661927  feat(stats): add EPS/EPM ring buffers with gap marking and a fake clock
6773fb6  feat(theme): add palettes, compiled styles, and contrast validation
9ed6951  feat(saltipc): decode Salt's length-prefixed IPC framing
5dbc773  feat(saltipc): add shallow field extraction and full msgpack decoding
d0dc292  feat(model): add Event and Job vocabulary with three-state expected count
61025bf  chore: scaffold module, just gate, and depguard/forbidigo layer rules
```

---

## 2. ⚠️ Files you must copy — cloning the repo is not enough

`docs-extra/superpowers/` and `.superpowers/sdd/` are git-ignored by project
policy. They hold the **spec, the plan, all 22 pre-generated task briefs, the
ledger, and every implementer and reviewer report**. Without them you have the
code but none of the requirements and none of the decisions behind it.

The destination has SSH access to this user. After cloning, from your repo root:

```bash
# 1 — spec and 22-task plan (the binding requirements)
scp -r tkcadmin@salt-1.tkclabs.io:/home/tkcadmin/repos/gitea/TKC-Labs/go-salt-events/docs-extra \
      ./docs-extra

# 2 — ledger, all 22 task briefs, all reports and review packages
mkdir -p .superpowers
scp -r tkcadmin@salt-1.tkclabs.io:/home/tkcadmin/repos/gitea/TKC-Labs/go-salt-events/.superpowers/sdd \
      ./.superpowers/sdd
```

Or in one pass:

```bash
rsync -av \
  tkcadmin@salt-1.tkclabs.io:/home/tkcadmin/repos/gitea/TKC-Labs/go-salt-events/docs-extra \
  tkcadmin@salt-1.tkclabs.io:/home/tkcadmin/repos/gitea/TKC-Labs/go-salt-events/.superpowers \
  ./
```

### What is in there, and why each matters

| Path (relative to repo root) | Why you need it |
|---|---|
| `docs-extra/superpowers/2026-08-30-salt-events-design.md` | The **spec**. Binding authority; the plan argues from it. Conflicts resolve against this. |
| `docs-extra/superpowers/plans/2026-08-30-salt-events.md` | The 22-task plan, ~9,800 lines, with complete code for most tasks. |
| `.superpowers/sdd/2026-08-30-salt-events/progress.md` | **The ledger.** Pre-flight conflict scan, every ruling with its cost-if-wrong, wave plan, standing decisions. Read before touching code. |
| `.superpowers/sdd/2026-08-30-salt-events/task-N-brief.md` | All 22 briefs, already extracted. **Do not regenerate** — the ledger's rulings reference them by number. |
| `.superpowers/sdd/2026-08-30-salt-events/global-constraints.md` | The constraints block pasted into every reviewer dispatch. |
| `.superpowers/sdd/2026-08-30-salt-events/*-report.md` | Implementer reports incl. TDD RED/GREEN evidence. |
| `.superpowers/sdd/2026-08-30-salt-events/review-*.diff` | Generated review packages. |
| `.superpowers/sdd/2026-08-30-salt-events/review-task-8-9-findings.md` | The full Tasks 8+9 review write-up. |

Read order on arrival: this file → the ledger → the spec → the plan.

### Environment on the source host, for reference

- Go 1.26.6 at `/usr/local/go`; **`/usr/local/go/bin` is not on PATH by
  default** — prefix commands with `export PATH=$PATH:/usr/local/go/bin`.
- `just` at `/usr/bin/just`.
- Salt 3006.27 onedir at `/opt/saltstack/salt`, **live master running**,
  event socket `/var/run/salt/master/master_event_pub.ipc`
  (`srw------- root root`, needs root). Passwordless sudo available.
- `tea` CLI is **not** installed, and both Gitea MCP servers failed to connect
  — which is why this handoff is a committed file rather than Gitea issues.

---

## 3. Resume here

Immediate, in order:

1. Copy the two directories above.
2. Re-dispatch the **Task 6 review** (§5).
3. Re-dispatch the **Tasks 8+9 scoped re-review** (§5).
4. Continue the wave plan (§4).

The process is `superpowers:subagent-driven-development`: one implementer per
task, a task review after each, fix loop, controller commits, then a
whole-branch review at the end.

---

## 4. Remaining wave plan

Dependency-ordered. Tasks on the same line are parallel-safe (disjoint
packages).

```
W2 remainder: T3
W3:  T10 | T11 | T20      (T11 must precede T12 — see below)
W4:  T7  | T12 | T14
W5:  T15                  (solo; owns the pane contract)
W6:  T16 | T17 | T18
W7:  T19, then T21
W8:  T22
```

Ordering constraints found in the pre-flight scan and still in force:

- **T11 (cache) before T12 (filter).** The plan has T12's step 6 reach back
  into T11's package to wire the real matcher.
- **Same-package pairs go to one agent, not two.** T4+T5 and T8+T9 were each
  handled this way because their tests reference each other's symbols;
  splitting them leaves the package uncompilable mid-flight. T10 is also
  `internal/stats`, so it must not run concurrently with anything else there.
- **T16–T19 are parallel-safe** — the panes live in separate sub-packages
  (`ui/live`, `ui/rate`, …), not in `ui` itself.

---

## 5. Outstanding reviews — do these first

### 5a. Task 6 (`internal/config`) — review never ran

Implemented, verified, committed as `dfa2433`. The review was dispatched and
the session died before it returned. **This is the only committed task on the
branch with no review of any kind.**

Review package already generated:
`.superpowers/sdd/2026-08-30-salt-events/review-c3a6b5d..dfa2433.diff`
(range `c3a6b5d..dfa2433`). Implementer report: `task-6-report.md`.

Unverified implementer claims: 8/8 tests pass, vet and lint clean;
`DefaultSockDir = "/var/run/salt/master"` checked against spec §2.6, the
brief's Step 3 code, and the live install; `SocketPath` structurally cannot
return the pull socket.

Emphases for the reviewer:

1. **Invariant 1 is Critical here.** Only `master_event_pub.ipc` is ever
   opened, never `master_event_pull.ipc`. The program's read-only guarantee —
   its structural inability to inject events onto a production Salt bus —
   rests entirely on `SocketPath`. Any route to the pull socket, however
   indirect, is Critical. (A controller spot-check found the pull socket named
   only in a doc comment at `paths.go:14` explaining its deliberate absence —
   not a substitute for review.)
2. **The `SUDO_USER` seam.** This tool runs as root via `sudo` on a production
   master. Naively resolving `$HOME` under sudo yields `/root`, so the
   operator's own config silently fails to load and they get defaults with no
   error at all. `ResolveConfigPath` takes injected `env`/`homeFor` functions
   for exactly this reason — any direct `os.Getenv` or `os.UserHomeDir`
   reintroduces the trap and makes it untestable.
3. **Precedence.** A test that sets only one source proves nothing. Check
   there are tests where two or more sources disagree and the winner is
   asserted.
4. **Error handling.** Does `Load` distinguish "config absent" (fine, use
   defaults) from "present but unparseable" (must surface)? Silently
   swallowing a malformed config is the same class of failure as the sudo trap.
5. Judge explicitly whether the first RED phase counts as TDD evidence: it
   produced `no non-test Go files in .../internal/config` rather than the
   brief's predicted `undefined: config.ResolveSockDir`, an artifact of a
   from-scratch package. The second RED matched the brief exactly.

### 5b. Tasks 8+9 (`internal/stats`) — scoped re-review never ran

The task review returned **Needs fixes** with 3 Important findings. The fix
round completed and is committed as `817b436`; `just check` is green. What is
missing is verification that the fixes addressed the findings.

Run a scoped re-review over **`72d1dd9..817b436`** — *not* from the head the
original review saw, since other tasks' commits interleave. Verdict each
finding ADDRESSED / NOT ADDRESSED and check the fix diff for new breakage.

Full write-up: `.superpowers/sdd/2026-08-30-salt-events/review-task-8-9-findings.md`

**Finding 1 — `Summary.Now` collapsed a gap into zero.** It read the newest
bucket's `Count` without checking its `Gap` flag. Gap buckets always carry
`Count == 0`, so the Rate pane's numeric callout would read `0 events/sec`
directly beneath a sparkline correctly drawing a break — telling an operator
"nothing happened" when the truth is "nobody was recording". `Peak`/`Mean`
shared the cause only in a fully-gapped window.
*Ruling:* fix in `Summary`, not the render layer — `Bucket.Gap` is
first-class precisely because gap-vs-zero is a fact about the data, and the
planned Prometheus exporter (spec §14) would otherwise re-derive it from raw
buckets. *Applied:* `Summary` gained `NowIsGap` and `HasData`, plus two tests.

**Finding 2 — `advance()` walked O(elapsed time), not O(ring size).** The
unbounded loop was a property of `advance()` itself, not just `MarkGap`, so it
also fired on an ordinary `Add()` after a suspended host or forward clock jump.
*Ruling:* clamp in `advance()`, not at the Task 7 call site — `Rings` owns the
"N buckets" invariant, and a caller should not need to know the ring's internal
sizing to call `MarkGap` with an arbitrary `(from, to)`. *Applied:* both
clamped; `MarkGap`'s duplicated seconds/minutes blocks extracted into
`markGapRing`, which also cleared a gocognit finding on production code.

**Finding 3 — no concurrency contract on `Rings`/`Counter`.**
*Ruling — this departed from the reviewer's advice.* The reviewer offered
"document **or** add a mutex"; the controller ruled **document only**, because
the plan puts the lock at Task 21's hub — the reader folds events into cache
and stats behind one mutex, and the UI reads a snapshot under that same lock.
An internal mutex would nest a second lock inside one already held on every
ingest: overhead on the hot path plus a second lock ordering to get wrong.
*Applied:* doc comments on both types. **Task 21 must serialise access** —
concurrent map access on `Counter.counts` crashes outright, not merely races.

---

## 6. Parked finding: the colour rule has an API-level escape hatch

Task 13 passed review with one Important finding **parked, not fixed**. It is a
defect in the plan's API design, not the implementation.

Spec §3.2 confines colour literals to `internal/theme`, enforced by a forbidigo
rule banning `lipgloss.Color(` elsewhere. That rule is a textual scan, and the
API offers three ways around it — any package can obtain a styled colour with
no forbidden call in its own source:

```go
s := theme.Compile(theme.Palette{Accent: theme.Color("#ff00ff")})  // struct literal
var p theme.Palette; p.Accent = theme.Color("#ff00ff")             // zero value + assign
p, _ := theme.Get("gruvbox-dark"); p.Accent = theme.Color("#ff00ff") // mutate a copy
```

Two of the three contain no `theme.Palette{` text, so **no forbidigo pattern
closes all of them.**

**Why it is worse than a theme-switching bug:** a hand-built `Palette` never
passes through the registry, and the contrast test iterates `theme.Names()` —
so such a palette is *structurally invisible to contrast validation*. Not "a
pane that won't switch themes", but **a pane never contrast-checked at all**,
with no test that would catch it. Given the project validates contrast *after*
256-colour quantisation precisely because a palette can pass at 24-bit and fail
in a real terminal, that is the exact failure the package exists to prevent.

Currently **dormant** — no consumer exists yet. It goes live at Task 14.

Not fixable inside Task 13: unexporting `Palette`'s fields closes all three
routes, but `theme_test.go` is an external `package theme_test` reading
`p.Base`/`p.Text` directly, so unexporting breaks the brief's own mandated test.

**Decision (the user's, not the controller's): enforce properly in Task 15.**
T15's dispatch must require:

- the root model is the **sole caller** of `theme.Compile`;
- a forbidigo pattern on `theme.Palette{` as a partial tripwire, **documented
  as partial, not airtight** — a rule that looks complete but isn't is worse
  than one honestly labelled;
- a test asserting panes receive styles only from the root.

**T14's dispatch must forbid** `components` from constructing a `Palette` or
calling `Compile`; it takes `*theme.Styles` as a parameter and nothing more.

---

## 7. Task 22: wire-format decoding is unverified against a real bus

Every decoding test so far runs against frames **this project constructs
itself**. That proves the decoder matches our understanding of Salt's format;
it does not prove our understanding is right.

`salt-1.tkclabs.io` has the live Salt 3006.27 master. **The new machine
probably does not.** Two silent coverage reductions follow:

- Task 22's integration test **auto-skips** when the socket is absent or
  unreadable. Correct behaviour — it keeps the suite runnable off a master —
  but on a machine with no master it reports *pass* having verified nothing.
- Task 5's three upstream-pinning tests (asserting `salt/transport/frame.py`
  still contains `struct.pack(">I", len(payload))`, and that `max_event_size`
  is still 1 MiB) **passed rather than skipped** on `salt-1`, meaning they
  really read Salt's source. They will skip without Salt at
  `/opt/saltstack/salt/lib/python3.11/site-packages/salt`.

A green suite that quietly stopped checking the only real-world thing is the
failure mode to guard against.

**Options:** run Task 22 back on `salt-1`; or run `just capture` on `salt-1`
and commit the recorded frames to `internal/saltipc/testdata/` so real bytes
travel with the repo (weaker than a live test — it cannot catch a *future*
Salt change — but far stronger than self-constructed frames); or accept the
gap and record it explicitly. **Not yet done at time of writing.** Note
`just capture` runs under `sudo`, so the file lands root-owned — `chown` it
back or neither the test run nor the commit can read it.

---

## 8. Standing decisions and process notes

- **Concurrency cap 3.** A user instruction, not a heuristic — set after
  watching the host bog down. Do not raise it because the new machine looks
  roomier; ask first.
- **Implementers never run `git add`/`git commit`.** The controller commits per
  task. Concurrent agents in one checkout race on `.git/index.lock` and produce
  commits containing other tasks' files.
- **Merge to `main` locally** once the final whole-branch review is clean.
- **No `//nolint`, and don't weaken `.golangci.yml` to pass.** Ruled twice.
  This project enforces structurally (depguard, forbidigo) rather than by
  comment. One deliberate exception exists: `gocognit` is excluded for
  `_test.go` only, because the plan mandates table-driven tests with subtests
  and that style is what trips the linter.
- **Long reviewer replies arrive truncated.** Instruct every reviewer to write
  its full report to
  `.superpowers/sdd/2026-08-30-salt-events/review-task-<N>-findings.md` and
  reply with verdicts plus one-line headlines only. This cost several round
  trips before it was adopted.
- **Scoped re-review `FIX_BASE`** must be the commit immediately *before* the
  fix commit — not "the head the previous review saw", which by then has other
  tasks' commits between it and the fix.

### Deferred minors for the final whole-branch review to triage

- `model.ExpectedCount()` has no direct test (the brief's verbatim test file
  omits one). It is the only invariant-10-adjacent logic without direct
  coverage, and invariant 10 — never fabricate a missing-minion count — is the
  highest-stakes guarantee in the codebase.
- `TestDecodeValueHandlesSaltExtTypes` asserts only that no error occurs, never
  that the decoded value is the correct `time.Time`; it would not catch a
  decoder that runs clean but yields a zero time. Also truncates the ext-78
  test string to 22 of 24 chars, harmless only because Go's `.999999` layout is
  variable-precision.
- No test covers a `Gap` bucket being cleared after the ring wraps past it and
  reuses the slot (correct by inspection, untested).
- No documented behaviour for `NewCounter(0)`, negative `maxKeys`, or
  `Top(n <= 0)`.
- Task 1's mutation check: the brief's *literal* Step 6 repro (a blank lipgloss
  import) exits non-zero, but golangci-lint's `uniq-by-line` dedup lets revive's
  finding mask depguard's, so it does not print `ingest-must-not-import-tui`.
  Not a misconfiguration — a named-import variant proves the rule fires — but a
  future reader following the brief verbatim may conclude depguard is broken.
- The second depguard rule, `ui-must-not-import-ingest`, has **never been
  mutation-tested**, because `internal/ui` did not exist. Prove it fires at
  Task 15 before Tasks 16–19 build on it.
