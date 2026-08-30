# Running salt-events

Operating notes for `salt-events` on a real master. For what the tool is and
why it is safe to point at production, see the [README](../README.md).

---

## 1. Requirements

| | |
|---|---|
| OS | Linux. The event bus is a Unix domain socket; there is no other transport. |
| Salt | A running `salt-master`. Upstream behaviour is pinned against **3006.27**. |
| Privileges | **root.** `master_event_pub.ipc` is mode `0600`, owned `root:root`. |
| Terminal | Anything with a TTY. Colour is optional — see [Themes](#7-themes). |

Nothing else. There is no daemon, no configuration required to start, no state
written anywhere unless you press `w`.

## 2. Running it

```bash
sudo salt-events
```

`sudo` is not optional. The event socket's mode is Salt's, not ours:

```console
$ ls -l /var/run/salt/master/master_event_pub.ipc
srw------- 1 root root 0 Aug 30 08:14 /var/run/salt/master/master_event_pub.ipc
```

If you want your own environment (`SALTEV_*`) and config to carry through
`sudo`, use `sudo -E`. The config file is found under `$SUDO_USER`'s home
either way (§4), so a plain `sudo` still reads the config you wrote as
yourself.

Quit with `q` or `ctrl+c`. Nothing is left behind: the cache is memory only.

### Verifying it is only reading

If you would rather check than take the README's word for it, the honest check
is the source, because the guarantee is structural rather than behavioural:

```bash
grep -rn master_event_pull --include='*.go' .
```

Every hit is either a comment explaining why that socket is never opened, or a
*test asserting that it is not*. There is no code path that names it. The
reader is handed a **directory** and derives the filename itself from a
constant, then re-checks the resolved basename before every connect — so no
`--sock-dir` you can supply, and no symlink, steers it at the pull socket. The
connection is only ever used as an `io.Reader`; nothing writes to it, not even
`shutdown(SHUT_WR)`, which Salt's publisher reads as EOF and answers by
dropping the subscriber.

At runtime, `sudo ls -l /proc/$(pgrep -x salt-events)/fd` shows the process
holding exactly one socket. (Linux reports it as `socket:[inode]` rather than a
path, so the fd listing cannot name it — that is why the source check above is
the one worth running.)

## 3. Command-line flags

Go's flag package accepts one or two leading dashes and both `--flag value` and
`--flag=value`.

| Flag | Default | Notes |
|---|---|---|
| `--sock-dir` | `/var/run/salt/master` | Salt's `sock_dir`. See §8.1 for when this is wrong. |
| `--max-memory` | `268435456` (256 MiB) | Event cache budget, **in bytes**. No `256M` suffix form. |
| `--max-jobs` | `500` | Jobs kept in the correlation index. See §9. |
| `--export-dir` | resolved (§6) | Destination for `w`. |
| `--export-max` | `1073741824` (1 GiB) | Hard cap on one export, in bytes. |
| `--filter` | none | Initial filter query, same syntax as `/`. |
| `--theme` | `gruvbox-dark` | One of the four palettes in §7. |
| `--no-color` | off | Selects the `mono` palette. |
| `--config` | resolved (§4) | Path to `config.toml`. |

`salt-events -h` prints this list with the **compiled-in defaults** — it is not
a report of the effective configuration. For that, press `?` in the running
console, which shows the socket path and the config path this session actually
resolved.

### `--version`

```console
$ salt-events --version
salt-events v0.1.0 (a1b2c3d, built 2026-08-30T21:00:00Z, go1.26.6)
```

Prints to stdout and exits 0, so it pipes cleanly. It is answered before the
configuration is loaded, so it works on a host with no socket, no config file
and no Salt at all — which is exactly the situation in which someone is asking
which binary they have.

A build from a release tag reports that tag. A local `just build` reports
`git describe`, with `-dirty` appended when the working tree has uncommitted
changes. A `go install` build has no linker stamps at all and falls back to the
VCS revision Go embeds automatically, so it still identifies itself.

### The capture flags

```bash
sudo salt-events --capture=200 --capture-out=internal/saltipc/testdata/live-frames.bin
# or: just capture 200
```

`--capture N --capture-out PATH` records `N` raw frames off the live bus,
verbatim including the length prefixes, and exits without starting the console.
It exists to regenerate the test fixtures. Both flags are required together;
`--capture` without `--capture-out` is an error.

They are listed in `-h` under "Fixture recording", but deliberately kept out of
the config file, the environment namespace and the precedence table: a fixture
recorder is a different program that happens to share a binary, not a runtime
setting.

## 4. Configuration

Precedence, highest first:

```
command-line flags  >  SALTEV_* environment  >  config.toml  >  built-in defaults
```

Every tier fails **loudly**. A malformed environment value, an unparseable
config file, or an out-of-range budget is reported with the offending key and
value and the program exits — a setting that is silently ignored is
indistinguishable from one that was never written.

### 4.1 Environment

| Key | Flag it mirrors |
|---|---|
| `SALTEV_SOCK_DIR` | `--sock-dir` |
| `SALTEV_MAX_MEMORY` | `--max-memory` |
| `SALTEV_MAX_JOBS` | `--max-jobs` |
| `SALTEV_EXPORT_DIR` | `--export-dir` |
| `SALTEV_EXPORT_MAX` | `--export-max` |
| `SALTEV_FILTER` | `--filter` |
| `SALTEV_THEME` | `--theme` |
| `SALTEV_NO_COLOR` | `--no-color` |

There is deliberately no environment key for `--config`: the config path is the
one thing that must be unambiguous when you are diagnosing a config that is not
being read.

`SALTEV_NO_COLOR` accepts exactly what Go's `strconv.ParseBool` accepts —
`1/0`, `true/false`, `t/f`, `TRUE/FALSE`. Anything else (`yes`, `on`) is an
error rather than a guess.

### 4.2 The config file

Resolution order, first hit wins:

1. `--config <path>` if given
2. `$SUDO_USER`'s home + `/.config/salt-events/config.toml`
3. `$HOME` + `/.config/salt-events/config.toml`
4. `/etc/salt-events/config.toml`

`$SUDO_USER` beats `$HOME` because under `sudo` `$HOME` is `/root`, and a
config written at your own `~/.config/salt-events/config.toml` would otherwise
be silently ignored — close to undiagnosable without `strace`. The resolved
path is shown in the `?` overlay whether or not a file exists there.

An absent file is normal and is not an error. A file that **exists but does not
parse** is an error, and the message names the path.

```toml
# ~/.config/salt-events/config.toml — every key is optional
sock_dir   = "/var/run/salt/master"
theme      = "solarized-dark"
export_dir = "/var/tmp"
filter     = "salt/job/*/ret/*"
max_memory = 536870912    # bytes
export_max = 2147483648   # bytes
max_jobs   = 2000
no_color   = false
```

## 5. Keys

### Global — available on every pane

| Key | Action |
|---|---|
| `1`–`5` | Jump to a pane by number |
| `tab` / `shift+tab` | Next / previous pane |
| `/` | Edit the filter (`enter` applies, `esc` cancels) |
| `space` | Pause the **view**. Ingest keeps running. |
| `w` | Export the filtered set to NDJSON (§6) |
| `t` | Cycle theme |
| `?` | Help overlay: all keys, the filter language, and this session's resolved socket and config paths |
| `esc` | Dismiss a reader error; leave a job drill-down |
| `q`, `ctrl+c` | Quit |

### Per pane

| Pane | Keys |
|---|---|
| Live | `↑`/`↓` move cursor · `g` jump to oldest · `G` resume following the tail · `enter` open in Detail |
| Detail | `↑`/`↓` scroll · `g` top of payload · `G` end of payload |
| Rate | `F` pin the y-axis scale (toggles back to autoscale) |
| Summary | none |
| Jobs (list) | `↑`/`↓` select job · `enter` drill in |
| Jobs (drilled in) | `↑`/`↓` select minion · `enter` open that minion's return in Detail · `f` cycle view (needs-attention / failed / missing / all) · `esc` back to the list |

Scrolling in Live releases follow-mode so the incoming stream does not fight
your cursor; `G` takes it back.

## 6. Filtering

Press `/`, type, `enter` to apply. `esc` cancels and leaves the previous filter
in place. A query that does not parse is reported inline and **also** leaves the
previous filter active — an empty pane reads as "there are no such events",
which is a very different message from "your query is wrong".

```
salt/job/*/ret/*  minion:scache-1  ok:false
     │                 │               └─ only failed returns
     │                 └─ field term
     └─ tag glob (fnmatch — Salt's own semantics)
```

- **Bare term** — a glob matched against the whole tag, fnmatch semantics
  (`*`, `?`, `[abc]`). `/` is *not* a separator, so `salt/job/*` works the way
  it does in a reactor config.
- **Prefixed term** — narrows one field. Terms are space-separated and AND
  together.

| Prefix | Matches | Value |
|---|---|---|
| `minion:` | the minion id | glob |
| `jid:` | the job id | glob |
| `fun:` | the function | glob |
| `ns:` | the namespace (`job`, `auth`, `key`, `minion`, `run`, `wheel`, `presence`, `syndic`, `cloud`, `fileserver`, `queue`, `custom`) | glob |
| `kind:` | the event class | exactly one of `new`, `ret`, `prog`, `start`, `auth`, `key`, `presence`, `other` |
| `ok:` | return success | exactly `true` or `false` |

Two things reliably surprise people:

- **`minion:*` also matches events with no minion at all**, because `*` matches
  the empty string. The "has a minion" spelling is `minion:?*`.
- **`ok:` only matches events that carry a return.** `ok:false` is "a return
  that failed", not "anything that is not a success", so job `new` events and
  auth events match neither `ok:true` nor `ok:false`.

A typo in a field name or in a `kind:` value is a query error, not a term that
silently matches nothing. Queries are capped at 64 terms and 512 characters per
glob — far above anything typed, and there so a pasted query cannot freeze the
render loop.

### The Live filter searches a bounded recent window

Each frame the cache walks back from the newest event and **stops after 16,000
events** even if it has not filled the view. That bound is what keeps rendering
independent of event rate. It costs reach: an older match is still retained and
still exported, it is simply not drawn.

The filter bar says so whenever the scan stopped short of the oldest retained
event:

```
filter: minion:web-041 · looked back 4231 of 190210 retained events; w exports the whole set
```

If that line is absent, the view is complete for the query. **Export is not
bounded** — `w` covers the whole retained set — so when hunting something old
and selective, export and grep rather than concluding it is not there.

## 7. Export (`w`)

`w` writes the **currently filtered** events as NDJSON, one object per line,
off the render loop. Ingest continues throughout.

### Where it goes

First writable of:

1. `--export-dir` / `SALTEV_EXPORT_DIR`
2. `$SUDO_USER`'s home directory
3. `$HOME`
4. `/var/tmp`

`/var/tmp`, never `/tmp`: `/tmp` is frequently tmpfs, so an export there spends
RAM on the machine already running the master.

The filename is `salt-events-YYYYMMDDThhmmssZ.ndjson` (UTC). If that name
somehow exists, `-2`, `-3` … is appended — an export never overwrites. The file
is mode `0600` and is `chown`ed to `$SUDO_USER`, so a `sudo` session does not
leave you a root-owned file you cannot read.

### The pre-flight refusal

Before anything is opened:

1. The encoded size is **estimated** — the raw tag and payload bytes plus a
   per-record envelope floor, times a deliberately pessimistic 2.0 JSON
   expansion factor.
2. The destination filesystem is `statfs`ed.
3. The export is **refused** unless the write would leave at least
   `max(1 GiB, 10% of the filesystem)` free.

A refusal appears on the notice line under the pane and names the estimate, the
space available and the headroom rule. The action is to narrow the filter and
try again. Nothing has been written at that point.

`--export-max` (default 1 GiB) is a second, independent cap that bounds the
actual write, so a wrong estimate cannot become a full disk either.

### While it writes

Every byte goes to `<name>.ndjson.partial` in the destination directory, which
is `rename(2)`d into place only on success. A rename within a directory is
atomic, so a file that looks complete is complete. If the disk fills anyway —
another process can win the race — the partial is unlinked and the notice says
how many events had been written. A truncated `.ndjson` is never left behind.

The stream is written event by event; the export is never assembled in memory,
so exporting a 256 MiB cache does not double the process's RSS.

An event whose payload was shed by the budget still exports, with
`"payload": null` and `"payload_truncated": true`. Its tag and timing are still
evidence. Salt's own trimming is a separate flag, `"master_trimmed"` — the two
are never collapsed, because once exported the original bus data is gone.

## 8. Themes

`gruvbox-dark` (default), `mono`, `solarized-dark`, `solarized-light`. `t`
cycles them in that (alphabetical) order, live, without losing scroll position.

`--no-color` selects `mono` by name rather than stripping colour downstream:
`mono` is a real palette in which bar length and text labels carry the whole
encoding, so the console stays readable over a pipe or on a terminal with no
colour at all.

Every palette is validated for contrast **after** 256-colour quantisation, so
what passes in truecolor also passes in a 256-colour terminal. An unknown theme
name falls back to the default rather than refusing to start — a typo in a
config file must not stop an incident console.

## 9. Sizing the caches

Two independent bounds, deliberately not shared: a payload storm cannot evict
job state, and job state cannot crowd out events.

**`--max-memory` (256 MiB)** bounds the event cache. Over budget it sheds
payloads oldest-first, and only drops whole events if shedding is not enough.
The status bar shows `cache 118M/256M · shed 0 drop 0`. A non-zero `shed` means
old payloads are gone but all tags, timings, jobs and rates are intact; a
non-zero `drop` means whole events are being lost and the budget is genuinely
too small for this master's rate.

**`--max-jobs` (500)** bounds the job correlation index. 500 is a safe starting
value, not a sufficient one, and the tool is built to make a wrong value
visible:

- The Jobs pane header shows occupancy and lifetime evictions, and names
  `--max-jobs` the first time it evicts.
- The Summary pane shows the **peak concurrent** tracked job count. Run a
  representative session and set `--max-jobs` above that number.
- A JID that was evicted reports "evicted from the job index — raise
  `--max-jobs`", which is a different message from "never seen" — different
  causes, different fixes.

Eviction is oldest-completed-first. A job still receiving returns is never
evicted, and neither is the job you currently have open. Per-job cost is
roughly one short string per target, so thousands of jobs is cheap; a ceiling
of 10% of `--max-memory` backstops the knob so it cannot be turned into an OOM.

## 10. Troubleshooting

### 10.1 `no event socket at /var/run/salt/master/master_event_pub.ipc`

The program refuses to start, before the TUI, and prints the reason to stderr —
a diagnostic behind an alternate screen buffer is a diagnostic nobody reads.

Check the master is up:

```bash
systemctl status salt-master
```

If it is up, the socket is somewhere else. **Salt relocates its own
`sock_dir`**: when the configured path is longer than `cachedir + 10` characters
it silently moves the sockets to `<cachedir>/.salt-unix`, because `sun_path` is
limited to 107 bytes. So the directory in the master config may not be where the
socket is. Find it and point at it:

```bash
sudo find /var/run/salt /var/cache/salt -name 'master_event_pub.ipc' 2>/dev/null
sudo salt-events --sock-dir /var/cache/salt/master/.salt-unix
```

`salt-events` replicates that relocation rule itself, but **only when you have
not overridden `sock_dir`** at any tier — an operator who named a directory did
so precisely because auto-resolution pointed at the wrong place, and second-
guessing them would silently defeat the override.

### 10.2 `permission denied` — you forgot `sudo`

The socket exists and is stat-able by anyone who can traverse the directory, so
the console *starts*. The reader then fails on its first connect, and the pane
is replaced by the §8.1 diagnostic:

```
THE EVENT READER STOPPED — nothing new is arriving; what follows is why, and
what is on screen is what was collected before it

cannot read /var/run/salt/master/master_event_pub.ipc: permission denied.

The Salt master event socket is owned by root with mode 0600, so this
tool must run as root:

    sudo salt-events

Confirm the owner and mode with:

    ls -l /var/run/salt/master/master_event_pub.ipc

esc  dismiss (the reader does not restart; quit and fix the cause)
```

`esc` dismisses the block so you can read what was collected, but the reader
does **not** come back. Quit and re-run under `sudo`.

The same treatment applies to the other fatal case: a `--sock-dir` whose
`master_event_pub.ipc` resolves, through symlinks, to something else. That is
refused outright and never retried — the one structural promise this tool makes
is that it cannot touch the socket that could write to the bus.

### 10.3 The master restarts under you

This is not fatal and needs no action. The reader reconnects with exponential
backoff (250 ms doubling to a 5 s ceiling) and keeps the console up.

While it is disconnected:

- the status bar reads **`DISCONNECTED`**, and it says so from the moment the
  socket is lost rather than waiting for events to stop arriving;
- the rate sparklines render the affected buckets as `·` in the warning colour,
  **not** as zeros. A disconnection drawn as a flat line at zero is
  indistinguishable from a quiet master, which is exactly backwards during an
  incident.

The gap is reported continuously while the outage runs, not retroactively on
reconnect, and the head of the graph stays honest across the second boundaries
the outage crosses.

That second part needs saying because it was wrong until recently, and the way
it is wrong is instructive. The rate ring opens a new one-second bucket as the
clock passes into it, ten times a second, whether or not anything is arriving —
and a bucket with a count of zero and no gap flag *is* a genuine zero. The
reader re-reported the outage once a second, which sounds sufficient and is not:
two things happening once a second with an arbitrary offset between them cannot
cover each other, so for part of every second the `now` callout read **`0`**
rather than **`no data`**, flipping between the two for the length of the
outage. During an incident those are opposite facts.

It is now a stated contract in both directions rather than two numbers that
happened to match: a gap report describes the present for a defined window
afterwards (`stats.GapValidity`, half a bucket), and the reader promises to
repeat itself more often than that (`stats.GapReportInterval`, a quarter of a
bucket). A bucket that opens inside the window opens as a gap. When the reports
stop — the master is back — the window lapses inside one bucket, so a
reconnected but quiet master goes back to reading as quiet rather than as an
outage.

Events fired while you were disconnected are gone — the bus has no replay. The
gap markers are how you know a flat stretch is missing data rather than quiet.

### 10.4 The bus is idle and nothing appears

An empty Live pane on a `connected` status bar means the master genuinely has
nothing to say. Connection state is read from the **socket**, never inferred
from event arrival, precisely so a healthy quiet master is not reported as
broken.

To confirm end to end, generate traffic from another terminal:

```bash
sudo salt '*' test.ping
```

If events appear for that and not for what you were looking for, the filter is
the next suspect — check the filter bar for a `looked back N of M` note (§6),
and remember `minion:*` matches events with no minion too.

### 10.5 Decode errors

A malformed frame is counted and skipped, never fatal: the length prefix means
exactly that frame is consumed, so one bad event cannot desync the stream. An
implausible length (> 64 MiB) is treated as a desync — the connection is closed
and re-established. A steady stream of decode errors against a supported master
means the wire format has drifted and is worth reporting.

## 11. Running the test suite on a master

The ordinary suite needs nothing:

```bash
just check        # fmt-check, vet, lint, test
just test-race
```

The integration suite touches the real bus and needs root:

```bash
sudo -E env "PATH=$PATH" just test-integration
```

It **auto-skips** when the socket is absent or unreadable, so it stays runnable
off a master and without root. A skip verifies nothing, though, so the skip is
made checkable — set `SALT_EVENTS_REQUIRE_BUS=1` and every one of those skips
becomes a failure:

```bash
sudo -E env "PATH=$PATH" SALT_EVENTS_REQUIRE_BUS=1 just test-integration
```

Use that when you believe you are on a master and want the run to *prove* it.
**Do not set it in CI** — no CI runner can host a salt-master, so it would make
CI permanently red for a reason that carries no information.

A run on a live but silent master connects, verifies the socket, and logs
`WARNING: NOTHING VERIFIED BEYOND CONNECT`. Generate traffic in another
terminal (`sudo salt '*' test.ping`) for the decode assertions to mean anything.
