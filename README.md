# salt-events

A read-only terminal console for the Salt master event bus. It runs locally on
a salt-master, caches the session's events in memory, and makes them
searchable, filterable and summarisable — with a live view of what the master
is actually doing right now.

```
 1 Live  2 Detail  3 Rate  4 Summary  5 Jobs
╭──────────────────────────────────────────────────────────────────────────╮
│Events/sec  last 120s              now 42  peak 311  mean 58              │
│ ▁▁▂▁▃▇█▆▃▂▁▁▁▂▅██▇▄▂▁▁▁▁▂▁▁▃▆██▅▂▁▁▁▂▁▁▁                                 │
│                                                                          │
│Top tags                          Top minions                             │
│salt/job/*/ret/* ██████░░░░  61%  scache-1  ████░░░░░░  38%                │
╰──────────────────────────────────────────────────────────────────────────╯
F pin the scale · 1-5 pane  tab next  t theme  / filter  space pause  w export  ? help  q quit
connected · 41203 events · cache 118M/256M · shed 0 drop 0    theme gruvbox-dark [t]
```

## Requirements

- Linux. The event bus is a Unix domain socket; there is no other transport.
- A Salt master. Upstream behaviour is pinned against **3006.27** (framing,
  `max_event_size`, `sock_dir` relocation), and the decoder is tested against
  frames captured verbatim off a real 3006.27 master.
- **root.** `master_event_pub.ipc` is `srw------- root root`, so:

```bash
sudo salt-events
```

## Install

Packages are attached to each [release](https://git.tkclabs.io/TKC-Labs/go-salt-events/releases),
for `linux/amd64`. The binaries are statically linked, so they carry no glibc
dependency and a package built on one host installs on another.

Only `amd64` is published. The build cross-compiles cleanly to `arm64`, but no
runner here can execute an `arm64` binary, and shipping an artefact that has
never been run on its target architecture is a guess rather than a release.
Build it yourself with `just build-release linux arm64` if you need one.

```bash
# RPM (AlmaLinux, Rocky, RHEL, Fedora)
sudo dnf install ./salt-events-<version>.x86_64.rpm

# DEB (Debian, Ubuntu)
sudo apt install ./salt-events_<version>_amd64.deb

# Or just the binary
tar xzf salt-events_<version>_linux_amd64.tar.gz
sudo install -m 0755 salt-events /usr/local/bin/
```

Verify what you downloaded against `sha256sums.txt` from the same release:

```bash
sha256sum -c sha256sums.txt
```

The packages deliberately do **not** depend on `salt-master`. The tool runs on a
master, but declaring that dependency would make it uninstallable on a jump host
where you only want to read an exported NDJSON file. They also install no
systemd unit — this is an interactive console, not a service — and set no setuid
bit: it needs root because the publish socket is `0600 root:root`, and the
answer to that is `sudo`.

To build from source instead:

```bash
just build          # ./salt-events, version derived from git describe
just package        # every release artefact into dist/
```

Check which build you are holding with:

```bash
salt-events --version
```

Running without it is not a mystery: the console starts, the reader fails on
its first connect, and the pane is replaced by the reason and the fix rather
than by a bare `permission denied`.

## Install

```bash
just build      # produces ./salt-events
```

## How it stays out of the way

These are the promises that make the tool safe to run on a production master.
Each one is enforced structurally and covered by tests, not left to care.

- **Only `master_event_pub.ipc` is ever opened, read-only.** The pull socket —
  the one that could *inject* events onto the bus — is never opened, and that
  is enforced by construction: the reader is handed a *directory* and derives
  the filename itself, then re-checks the resolved basename before every dial,
  so no path you can supply reaches the other socket. Nothing in the program
  writes to the connection.
- **Payloads are not decoded at ingest.** Only the handful of fields needed for
  indexing are read; the rest stays as raw msgpack until you open an event in
  the Detail pane. Filtering never forces a decode either.
- **The cache is bounded by a memory budget, not an event count.** Over budget
  it sheds *payloads*, oldest first, before it drops whole events — so tag,
  timing, job and rate history survive a storm that the payloads cannot. The
  status bar shows both counters separately (`shed 0 drop 0`), because they are
  different degrees of loss.
- **Shedding never changes a job count or an aggregate.** Job state and every
  statistic derive from eagerly-extracted fields, never from cached payloads, so
  a 1,000-target highstate stays readable after the budget has bitten.
- **Rate graphs key off arrival time, never Salt's `_stamp`.** A minion with a
  skewed clock cannot bend the graph, and durations never run backwards.
- **A gap is drawn differently from a zero.** While the reader is disconnected
  the rate sparklines render `·` in the warning colour, not a flat line at
  zero — a disconnection that looks like a quiet master is exactly backwards
  during an incident.
- **An unknown missing-minion count is never shown as a number.** If a job's
  expected minion set was trimmed by the master or was never seen, the Jobs
  pane says `expected set unknown` and why, rather than `0 missing` — which
  would read as "everything returned" and is the most dangerous possible wrong
  answer.
- **Master-side trimming and our own shedding are shown distinctly.** Salt
  replacing an oversize value with `VALUE_TRIMMED` and our cache dropping a
  payload have different causes and different fixes, so they are never
  collapsed into one marker.
- **Export refuses rather than filling your disk.** See below.
- **Pausing freezes the view, never ingest.** The storm you paused to read is
  still being collected while you read it.

## Panes

| # | Pane | What it is for |
|---|---|---|
| 1 | Live | Streaming tail: age, tag, minion, one-line summary. `enter` opens an event in Detail. |
| 2 | Detail | The full decoded payload of one event, pretty-printed and scrollable. The only place a payload is fully decoded. |
| 3 | Rate | Events/sec over 120s and events/min over 60m, plus inline top tags and top minions. `F` pins the scale. |
| 4 | Summary | Top tags, minions and functions over a wider window, plus the job index's occupancy and peak concurrent count. |
| 5 | Jobs | `salt/job/<jid>/new` correlated against `salt/job/<jid>/ret/<minion>`. `enter` drills into a job for the per-minion breakdown. |

## Keys

```
1-5  pane        tab / shift+tab  next / previous pane
t    theme       /  filter        space  pause the view
w    export      ?  help          q or ctrl+c  quit
esc  dismiss the filter editor, a job drill-down, or a reader error
```

The focused pane's own keys are shown on the hint line under the frame and in
full in the `?` overlay — which is also where the **resolved socket and config
paths** for the session are shown, so a config file that is not being read is
diagnosable without `strace`.

## Filtering

Press `/`, type, `enter` to apply, `esc` to cancel.

```
/ salt/job/*/ret/*  minion:scache-1  ok:false
```

- A **bare term** is a glob matched against the tag with fnmatch semantics —
  Salt's own, so reactor and beacon muscle memory transfers directly.
- A **prefixed term** narrows one field: `minion:`, `jid:`, `fun:`, `ok:`,
  `ns:`, `kind:`.
- Terms are space-separated and **AND** together.
- A malformed query is reported inline and leaves the **previous** filter
  active — an empty pane reads as "there are no such events", which is a very
  different message from "your query is wrong".

Field terms match only the eagerly-extracted fields, so filtering can never
force a payload decode.

### The Live filter searches a bounded recent window

This is real, user-visible behaviour and worth understanding before you draw
conclusions from an empty pane.

Each frame, the cache walks back from the newest event looking for matches and
**stops after a fixed budget** (16,000 events) even if it has not filled the
view. That bound is what keeps rendering `O(visible rows)` and off the ingest
lock at 5,000 events/sec — but it is a real loss of *reach*: a match older than
the budget is still retained, and still exported, it is simply not drawn.

So the filter bar says so whenever the scan stopped short:

```
filter: minion:web-041 · looked back 4231 of 190210 retained events; w exports the whole set
```

**`w` is not bounded.** Export covers the whole retained set for the query. If
you are hunting for something old and selective, export and grep rather than
concluding it is not there.

## Export

`w` writes the currently filtered events as NDJSON, one JSON object per line.

Before it opens anything it estimates the encoded size, `statfs`es the
destination, and **refuses unless the write would leave at least
`max(1 GiB, 10% of the filesystem)` free.** This runs as root on a master, and
filling `/` would be a self-inflicted outage. A refusal names the estimate, the
space available and the headroom rule, so the answer is "narrow the filter",
not "go hunting". `--export-max` (default 1 GiB) is a second, hard cap that
bounds the write itself even if the estimate was wrong.

The write is streamed — never assembled in memory — and atomic: every byte goes
to a `.partial` that is either renamed into place whole or unlinked, so a
mid-write `ENOSPC` leaves no truncated file behind.

Destination: `--export-dir` if given, else **`$SUDO_USER`'s** home directory,
else `$HOME`, else `/var/tmp` (never `/tmp`, which is frequently tmpfs — an
export there spends RAM on the machine already running the master). The file is
mode `0600` and is `chown`ed back to you, so a `sudo` session does not leave you
a root-owned file you cannot read.

## Configuration

Precedence is **flags > environment > config file > defaults**. Environment
keys are `SALTEV_<KEY>`.

| Flag | Env | Default | Purpose |
|---|---|---|---|
| `--sock-dir` | `SALTEV_SOCK_DIR` | `/var/run/salt/master` | Salt's `sock_dir` |
| `--max-memory` | `SALTEV_MAX_MEMORY` | 268435456 (256 MiB) | event cache budget, in bytes |
| `--max-jobs` | `SALTEV_MAX_JOBS` | 500 | jobs retained for correlation |
| `--export-dir` | `SALTEV_EXPORT_DIR` | see above | NDJSON destination |
| `--export-max` | `SALTEV_EXPORT_MAX` | 1073741824 (1 GiB) | hard cap on one export |
| `--filter` | `SALTEV_FILTER` | none | initial filter query |
| `--theme` | `SALTEV_THEME` | `gruvbox-dark` | colour theme |
| `--no-color` | `SALTEV_NO_COLOR` | off | select the `mono` palette |
| `--config` | — | see below | path to `config.toml` |

Config lives at `~/.config/salt-events/config.toml`, resolved from
**`$SUDO_USER`'s** home rather than root's — so a config written as yourself is
actually read under `sudo`. Full resolution order and the TOML key names are in
[docs/running.md](docs/running.md).

`salt-events -h` prints the **compiled-in defaults**, not the effective
configuration. To see what this session actually resolved, press `?` in the
running console.

### Tuning `--max-jobs`

500 is a safe starting value, not a sufficient one. The tool is built to make a
wrong value *visible* rather than to guess a right one:

- The Jobs pane header shows occupancy and lifetime evictions, and names
  `--max-jobs` the first time it evicts.
- The Summary pane shows the peak concurrent tracked job count — run a
  representative session and set `--max-jobs` above that number.
- A job requested by JID that was evicted says **"evicted from the job index —
  raise `--max-jobs`"**, which is a different message from "never seen".

Per-job cost is dominated by the target list, roughly one short string per
minion, so raising this into the thousands is cheap. A memory ceiling (10% of
`--max-memory`) backstops it so the knob cannot be turned into an OOM.

## Themes

`gruvbox-dark` (default), `solarized-dark`, `solarized-light`, `mono`. `t`
cycles them live. Every palette is validated for contrast *after* 256-colour
quantisation, and `mono` carries the entire encoding in bar length and text
labels, so nothing is lost over a colourless terminal.

## Scope

Read-only, by design and by construction. No firing events, no reactor or
beacon management, no salt-api, no job control, and no on-disk persistence
across sessions — the cache is memory only, and `w` is the way data leaves.

## Development

```bash
just check              # fmt-check, vet, lint, test
just test-race          # go test -race
just build              # build ./salt-events
just test-integration   # needs a live master; skips cleanly without one
```

See `just --list` for everything, and [docs/running.md](docs/running.md) for
operating it.

## Licence

Apache-2.0. See [LICENSE](LICENSE).
