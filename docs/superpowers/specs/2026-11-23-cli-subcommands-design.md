# CLI subcommands: one store, ways in, ways out

Date: 2026-11-23
Status: design, awaiting approval.

## Intent

loghorn has grown into several tools behind one flag list: a transparent
command wrapper, a pipe recorder, a log replayer (files, stored days, GitHub
Actions), a web explorer, an OTLP receiver, and soon a CLI query client
(`2026-11-22-cli-query-design.md`) and scheduled pulls (architecture notes,
decisions 11–15). `loghorn -h` lists `-notify-sound` next to `-web-port` next
to `-gh-job`, and `mode.check()` exists mostly to reject combinations that
mean nothing.

This spec splits the command line into subcommands grouped by **how long the
process lives and whom it talks to**, the distinction users already make
("does this stay running, or do I run it once?"):

```
 ways in                       the hub                    ways out
 loghorn run -- <cmd>  ──┐                           ┌── web explorer (browser)
 loghorn open <log>    ──┼──▶ loghorn serve  ────────┼── loghorn query <url>
 apps via OTLP         ──┤    (the store)            └── loghorn status
 sources (scheduled)   ──┘
```

It also fixes the properties `serve` needs so a later task can run it as an
OS service (launchd LaunchAgent, systemd `--user` unit, Windows logon task).

## Decisions taken in discussion

- **[user]** Subcommands grouped by lifetime and role, as above.
- **[user]** `serve` is one process that both ingests and serves the web
  page. They share a process because the embedded engine allows one writer
  (architecture notes, decision 4). Ingest and web are *listeners* of
  `serve`, not modes; `serve -h` groups its flags under those headings.
- **[user]** `serve` is always on. **No auto-start** from client commands:
  starting `serve` on demand would leave apps that send OTLP on their own
  (not through `loghorn run`) with no receiver, and OTEL SDKs drop data
  silently when the exporter can't connect. `serve` is started by the OS
  service (a later `loghorn service install`) or by hand in a terminal.
- **[user]** The service requirements below (from the session working on
  `loghorn service`) are part of this design.
- **[user]** Web port default 4380; OTLP keeps 4318 (HTTP) and 4317 (gRPC).
  Every port can be overridden by a `serve` flag, and clients follow the
  override (see Ports and discovery).
- **[user]** When `serve` isn't running, `run` records anyway, writing into
  the data folder itself (see When `serve` isn't running).
- **[user]** `loghorn status` shows the environment variables an OTEL client
  needs to send to `serve`.
- **[user]** Phase 1 of this spec runs before the CLI query plan, whose last
  tasks are amended to use `query` and `status`.
- **[proposed]** Scheduled pulls are configuration that `serve` executes;
  `loghorn source` only edits it. "Cron" is not a mode.

## Goals

- Each command shows and accepts only the flags that apply to it.
- Bare `loghorn` prints a short, grouped command list.
- Today's behaviours all have a home; nothing is lost in phase 1.
- `serve` meets the OS-service requirements when phase 2 builds it.
- The two most-typed forms keep working: `loghorn -- <cmd>` and
  `loghorn <loopback URL>`.

## Non-goals

- `loghorn service install|uninstall|status` (a separate task; this spec only
  makes `serve` ready for it).
- The storage engine, runs, retention, alerts and follow sources themselves
  (architecture notes). This spec fixes only their command-line surface.
- Long (`--flag`) options or a third-party CLI library. Flags stay Go-style
  single dash, parsed with one `flag.FlagSet` per command.

## Commands

| Command | Lifetime | Phase | What it does |
|---|---|---|---|
| `loghorn run [flags] [--] <cmd> [args]` | while the child runs | 1 | Wrap a command: pass output through unchanged, keep its exit status, record its records. |
| `loghorn run [flags] < pipe` | until end of input | 1 | Record piped input, copying it to stdout. |
| `loghorn open [flags] <log>` | 1: until Ctrl-C; 2: one shot | 1 | Open a finished log: a file, `-` (stdin), or a GitHub Actions run or job. |
| `loghorn serve [flags]` | until SIGTERM/SIGINT | 1 (web only), 2 (store) | The store: OTLP receiver, web explorer and API, expiry, scheduled pulls. |
| `loghorn query [flags] <url\|@file\|@->` | one shot | 1 | Print a web view's data (the CLI query design). |
| `loghorn status [-env]` | one shot | 1 | Live instances (phase 1); `serve` health, ports, sources and the OTEL client environment (phase 2). |
| `loghorn config` | one shot | 1 | Print the config file in effect and the settings it resolves to. |
| `loghorn source add\|list\|pause\|resume\|rm` | one shot | 3 | Edit scheduled pulls; `serve` runs them. |
| `loghorn help [command]` | one shot | 1 | Same as `-h`. |

### Why `open` rather than `import`

In phase 1 there is no store, so a replay only *shows* a log. In phase 2 it
imports the log into `serve` as a run (idempotent, decision 13) and then
shows it. "Open" is true in both phases; storing is a side effect the user
doesn't have to think about. `open` also takes a path argument, which reads
better than today's `loghorn -web < file`.

### Top-level help

```
loghorn — capture, store and query the logs and traces of local apps

Usage: loghorn <command> [flags] [args]

Capture
  run -- <cmd>     run a command, passing its output through, and record it
  open <log>       open a finished log: a file, or a GitHub Actions run or job

Store
  serve            run the store: receive OTLP, serve the web explorer

Read
  query <url>      print a web view's data as TSV, CSV, Markdown, JSON, ...
  status           show what is running
  config           show the config file in effect

Shortcuts: 'loghorn -- <cmd>' is 'loghorn run'; 'loghorn <url>' is 'loghorn query'.
Run 'loghorn help <command>' for its flags.
```

`source` appears under Store when phase 3 adds it.

## Dispatch

`main` looks at `os.Args[1]`:

1. none → print top-level help to stderr, exit 2. **Piped stdin is not
   recorded.** Today `cmd | loghorn` records to the project log, which has
   bitten agents probing the binary; recording a pipe now takes an explicit
   `loghorn run`. The help adds one line when stdin is a pipe or file:
   "to record piped input use 'loghorn run'; to view a file use 'loghorn
   open'".
2. `--` → `run` with the rest.
3. a loopback URL (`http://127.0.0.1…`, `http://localhost…`) → `query`.
4. a known command name → that command.
5. `-h`, `-help`, `--help` → top-level help, exit 0.
6. any other word starting with `-` → the **migration error** (below),
   exit 2.
7. any other word → `loghorn: unknown command "npm"; to run a command use
   'loghorn run -- npm …'`, exit 2.

### Flag placement

- `run` stops parsing flags at `--` or at the first non-flag word: everything
  after belongs to the child (`loghorn run -filter npm test -v` passes `-v`
  to npm).
- Every other command accepts flags before and after its positional
  arguments (`loghorn query URL -o out.csv`). Go's `flag` stops at the first
  positional, so a small helper parses, takes one positional, and parses the
  rest again. A `--` ends flag parsing for these too.

### Migration error

Old top-level flags get a message naming the new form, from one table (the
existing `removedFlagError` grows into it):

```
loghorn: -web is now a command: 'loghorn serve' (alone), 'loghorn run -web -- <cmd>' (with a command), 'loghorn open <log>' (a replay)
```

No compatibility shim beyond the two shortcuts: loghorn has few users, and a
precise error is cheaper to keep correct than an alias layer. `-scrollback`
and `-theme` keep their "removed with the terminal UI" message.

## Flag mapping

Every current flag, in phase 1 (subcommands over today's engine) and phase 2
(with the store).

| Today | Phase 1 | Phase 2 |
|---|---|---|
| `loghorn -- cmd` | `loghorn run -- cmd` (shortcut kept) | same; `run` sends records to `serve` over OTLP (decision 9) |
| `cmd \| loghorn` | `cmd \| loghorn run` | same |
| `loghorn -web` (alone) | `loghorn serve` (web over the stored `.loghorn` logs of the start directory) | `loghorn serve` (the store; no start directory) |
| `-web` with a command or pipe | `run -web` | removed; `run` prints the `serve` URL filtered to its run |
| `-web-port N` | `run -web-port`, `serve -web-port` (default 0) | `serve -web-port` (fixed default, see Ports) |
| `-filter` | `run -filter`, `open -filter` | same |
| `-notify`, `-notify-max-age`, `-notify-reset`, `-notify-sound` | `run` | `run` (alerts from `serve` are a separate design, decision 37) |
| `-shutdown-grace` | `run` | `run` |
| `-exclusive` | `run` | removed (no per-directory log file); a spill file, if built, has its own lock |
| `-otel` | `run -otel` | removed; `run` always points the child's `OTEL_*` variables at `serve`, or at its own receiver when `serve` is down |
| `loghorn -web\|-filter < file` | `loghorn open file` or `open - < file` | same; imports the file as a run |
| `-historical` | `open -historical` | removed; the store holds everything (`query`, the web page) |
| `-gh-job ID\|URL` | `open URL` or `open -gh-job ID` | same; imports the job as a run |
| `-gh-run ID\|URL` | `open URL` or `open -gh-run ID` | same |
| `-config` | `loghorn config` | same |
| `-url`, `-o`, `-format`, `-to-sql` (CLI query plan) | `query` flags; `-url X` becomes the positional | same |
| `-instances` (CLI query plan) | `loghorn status` | `loghorn status` (one `serve`; health and sources) |

Notes:

- `open` shows the web view by default. `-filter` prints the important
  records to stdout instead; `-filter -web` does both. This drops today's
  "needs -web or -filter" error: opening a log with no flags now means
  "show it to me".
- `open` takes a GitHub Actions URL as its positional and tells a job URL
  (`…/actions/runs/N/job/M`) from a run URL (`…/actions/runs/N`) by its
  path. Bare IDs are ambiguous, so they use `-gh-job` / `-gh-run`.
- `open -` reads stdin, a file or a pipe, and never records it (open never
  writes). `kubectl logs pod | loghorn open -filter -` filters without
  recording.
- `run` refuses a replay source and `open` refuses a command, so the
  cross-checks in `mode.check()` shrink to: `run` needs a command or a
  pipe; `open` needs exactly one source.

## Phases

1. **Subcommands over today's engine.** Pure command-line restructuring:
   dispatch, per-command FlagSets, help, migration errors, README and usage
   text. No storage or behaviour change, except: piped stdin with no command
   is no longer recorded, and `open` defaults to the web view. `serve` in
   phase 1 is today's `loghorn -web`, so it still depends on the start
   directory; it does not meet the service requirements yet and its help
   says so.
2. **`serve` becomes the store** (architecture spec, not this one). `run`,
   `open`, `query` and `status` become clients of it. The service
   requirements apply from here.
3. **`loghorn source`** with follow sources.

### Interaction with the CLI query plan

`docs/superpowers/plans/2026-11-23-cli-query.md` (not yet executed) adds
top-level `-url`, `-o`, `-format`, `-to-sql` and `-instances`. Two orders
work:

- **A (chosen):** run phase 1 of this spec first, then amend CLI query
  Tasks 6–7 to add a `query` command and `status` instead of top-level
  flags. No flags ship and then move.
- **B:** run the CLI query plan as written; phase 1 then moves its flags
  under `query`. Costs one round of churn in flags, tests and README.

## `serve` as an OS service (phase 2)

Requirements from the `loghorn service` work. Each is a property of
`serve` itself; none needs the service installer.

1. **Independent of the start directory.** launchd starts jobs in `/`. The
   store and `serve`'s own state live in a per-user data folder:
   macOS `~/Library/Application Support/loghorn`; Linux
   `$XDG_STATE_HOME/loghorn` (default `~/.local/state/loghorn`); Windows
   `%LOCALAPPDATA%\loghorn`. A `-data DIR` flag overrides it (tests, and
   anyone who wants it elsewhere). `serve` reads only user-level config
   (`$XDG_CONFIG_HOME/loghorn` or `~/.config/loghorn`, as `svcconf` does)
   and must start with no repo anywhere. Repo `.config/loghorn/` settings
   (input framing, field naming) are applied by the client that parses the
   lines, i.e. `run` and `open`, which do have a start directory.
2. **Fixed, configurable ports** that `run`, `query`, `status` and apps all
   know: OTLP/HTTP 4318, OTLP/gRPC 4317 when the gRPC receiver lands
   (`2026-11-19-otlp-grpc-design.md`), and web 4380. Today both listeners
   use port 0 (`internal/otlp/receiver.go:34`, `web.Listen`). See Ports and
   discovery for the overrides.
3. **Foreground until SIGTERM or SIGINT**, then stop accepting, drain
   in-flight requests, flush the engine (DuckDB checkpoint, pending Parquet
   writes), release the lock and exit 0. `serve` never forks itself into the
   background; the supervisor (or the user's terminal) owns its lifetime.
4. **Fail fast and loud at startup.** Order: load config → take the data
   folder lock → bind every listener → open the store → start sources and
   expiry → serve. Any failure up to "serve" prints one clear
   `loghorn serve: …` line to the log and exits non-zero (2 for a bad flag
   or config, 1 otherwise), so the supervisor's restart and backoff work.
   Nothing that needs undoing happens before the listeners are bound (the
   startup-ordering rule in CLAUDE.md, extended to `serve`).
5. **One instance per data folder**, enforced by a lock file in it. A second
   `serve` exits 1 naming the holder's pid. The lock sits behind a small
   interface with per-OS files (`flock` on Unix, `LockFileEx` on Windows
   through `golang.org/x/sys/windows`); `internal/logfile/lock.go`'s
   `syscall.Flock` moves behind the same interface when it is next touched.
6. **Its own log goes to stderr** by default (launchd and systemd capture
   it). `-log FILE` writes it to a file instead, for a supervisor that
   doesn't capture stderr (Windows). Nothing in `serve` assumes stderr is
   seen by a person.
7. **No GUI session required.** Anything that notifies (later alerts) treats
   a missing display, D-Bus session (`DBUS_SESSION_BUS_ADDRESS` unset under
   systemd) or notification permission as a soft failure: one warning in
   the log, then silence, never an exit.

New `serve` code stays free of Unix-only syscalls so a Windows serve-only
build stays within reach. Today `GOOS=windows go build ./...` fails only on
`syscall.Flock` (`internal/logfile/lock.go`) and `SIGWINCH`
(`internal/wrap/wrap.go`).

### Ports and discovery (phase 2)

Each port is set, highest precedence first, by:

1. a `serve` flag: `-otlp-http-port`, `-otlp-grpc-port`, `-web-port`;
2. the user-level `config.toml`, `[serve]` keys `otlp-http-port`,
   `otlp-grpc-port`, `web-port`;
3. the defaults 4318, 4317, 4380.

A port in use is a startup failure (requirement 4) whose message names the
flag and the config key, e.g. for a user already running an OTel Collector:
`loghorn serve: 127.0.0.1:4318 is in use; pick another OTLP port with -otlp-http-port or [serve] otlp-http-port in <config path>`.

Clients don't guess ports. `serve` listens on a unix socket at a fixed path
in its data folder, `<data>/serve.sock`, and `/api/session` on it reports
the ports actually bound. `run`, `query`, `status` and `open` connect there
first, so a flag override reaches them with no extra setting; they fall back
to the configured web port only where unix sockets are unavailable. The
socket lives in the data folder rather than `$TMPDIR/loghorn` (the CLI query
design's location) because a launchd job may not see the terminal's
`$TMPDIR`, and the two would then look in different places. The CLI query
design's per-instance sockets stay for phase 1's several `-web` instances.

The data folder is found the same way by every command: `serve -data DIR`
for `serve` itself, else `$LOGHORN_DATA_DIR`, else the per-OS default. A
`serve` started with `-data` elsewhere needs `LOGHORN_DATA_DIR` set for
clients to find it; `status` says so when it finds no socket but the config
names a different folder.

### `loghorn status`

Prints, in order:

- whether `serve` is running: pid, start time, data folder, version;
- the ports it bound (web URL, OTLP/HTTP and OTLP/gRPC endpoints);
- sources and their freshness (phase 3, decision 14);
- **the environment an OTEL client needs**, ready to paste:

```
# OpenTelemetry clients: send logs and traces to loghorn
export OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:4318
export OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf
export OTEL_LOGS_EXPORTER=otlp
export OTEL_TRACES_EXPORTER=otlp
export OTEL_METRICS_EXPORTER=none
# and per app: export OTEL_SERVICE_NAME=<your service>
```

The endpoint uses the port `serve` actually bound. `OTEL_METRICS_EXPORTER=none`
is there because `serve` discards metrics today; it goes away when metrics
are stored. `run` sets its child's variables itself (`otlp.ChildEnv`, plus
`LOGHORN_RUN_ID` and `loghorn.run.id`), so these are for apps started
without `run`.

`status -env` prints only the `export` lines, so
`eval "$(loghorn status -env)"` works in a shell or a repo's dev script.
When `serve` isn't running, `status` still prints the environment from the
configured ports (so a repo can be set up before the service is), after the
"isn't running" line, and exits 1; `status -env` prints the lines and exits
0, since the variables don't depend on `serve` being up.

In phase 1, `status` lists the live `-web` instances (the CLI query design's
`-instances` table) and prints no environment: the only OTLP receiver is
`run -otel`'s, on a random port, and `run` already gives its child the
variables.

### When `serve` isn't running (phase 2)

No auto-start; clients say what to do.

- `query`, `status`, `open`: exit 1 with
  `loghorn: loghorn serve isn't running on 127.0.0.1:<port>; start it with 'loghorn serve' or install it with 'loghorn service install'`.
  (`status` prints the same line and exits 1, so scripts can test it.)
- `run`: never blocks or fails the child (decision 9), and records anyway
  as long as it can find the data folder. With no socket answering, it:
  1. writes the child's records to a segment of its own,
     `<data>/incoming/<run id>/`, in the store's segment format, and takes
     only that segment's lock — never the store's lock, so `serve` can start
     (or be restarted by its supervisor) while a long-lived `run`, such as
     a dev server, is still going, and several such `run`s can record at
     once;
  2. receives the child's own OTLP on an ephemeral loopback port and points
     the child's `OTEL_*` variables there, as `-otel` does today, storing
     what arrives in the same segment;
  3. prints one stderr line: `loghorn: serve isn't running; recording this
     run to <data>/incoming/<run id> (apps sending OTLP to 4318 on their
     own aren't recorded until serve starts)`.

  `serve` adopts closed segments into the store at startup and on a timer,
  then deletes them; an open segment (its lock still held) is left for
  later. `run` keeps writing to its segment until the child exits even if
  `serve` comes up meanwhile: switching mid-run would split it. When the
  data folder can't be found or created, `run` warns and passes output
  through without recording.

## Testing

Phase 1:

- Dispatch table: no args (with stdin a terminal, a pipe, a file: none
  records), `--`, loopback URL, each command, `-h`, an old flag, an unknown
  word. Exit codes 0/2 as above.
- Migration error for every row of the flag mapping, including `-scrollback`
  and `-theme`.
- Flag placement: `run` passes a child's `-v` through; `query URL -o f`
  and `open file -filter` parse flags after the positional.
- `open` source detection: path, `-`, job URL, run URL, `-gh-job`,
  `-gh-run`, two sources (error), none (error).
- Existing e2e and main tests move to the new forms; behaviour assertions
  stay the same.
- `go vet`, `go test -race ./...`; the README usage block and `usage()` text
  are checked against `loghorn help` output by a test, so they can't drift.

Phase 2 (in the store's plan): startup failures for a busy port, a held
lock and a bad config each exit non-zero before anything else starts;
SIGTERM flushes and exits 0; `serve` started with cwd `/` and `HOME` set to
a temp dir uses the per-user data folder and no repo config. Port flags
override config, and `status` and `status -env` report the bound ports, with
`serve` up and down. With `serve` down, two concurrent `run`s each write
their own incoming segment, `serve` starts while they hold their locks,
adopts each segment once its run ends, and a busy-port message names the
flag and config key.

## Open questions

1. **Segment format for `run`'s incoming writes.** It must be something
   `serve` can adopt cheaply; the store's own Parquet segment layout
   (architecture notes, storage) is the obvious choice, but that layout is
   specified in the store's design, not here.
2. **Shells other than sh for `status -env`** (fish, PowerShell): add a
   `-shell` choice when someone needs it.
