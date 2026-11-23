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

It also gives `serve` the properties it needs so a later task can run it as
an OS service (launchd LaunchAgent, systemd `--user` unit, Windows logon
task).

loghorn targets a developer's own single-user machine. Nothing here defends
against other local users.

## Decisions taken in discussion

- **[user]** Subcommands grouped by lifetime and role, as above.
- **[user]** `serve` is one process that both ingests and serves the web
  page. They share a process because the store has one writer (architecture
  notes, decision 4). Ingest and web are *listeners* of `serve`, not modes;
  `serve -h` groups its flags under those headings.
- **[user]** `-otel` and `-web` are dropped. `serve` is the only OTLP
  receiver and the only web server for live data; `run` sends to it.
- **[user]** `serve` is always on. **No auto-start** from client commands:
  starting `serve` on demand would leave apps that send OTLP on their own
  (not through `loghorn run`) with no receiver, and OTEL SDKs drop data
  silently when the exporter can't connect. `serve` is started by the OS
  service (a later `loghorn service install`) or by hand in a terminal.
- **[user]** The OS-service requirements below are part of this design.
- **[user]** Ports: web 4380, OTLP/HTTP 4318, OTLP/gRPC 4317. Each has a
  `serve` flag to override it.
- **[user]** Clients find `serve` through a unix socket at
  `/tmp/loghorn.sock`, which needs no data folder to locate. `serve -socket`
  moves it, for a second instance.
- **[user]** When `serve` is down, what `run` does is the user's choice
  (`serve-down` setting), so `run` never quietly takes over for a failed
  `serve`.
- **[user]** `loghorn ui` opens `serve`'s web page in the browser, using
  the URL the socket reports.
- **[user]** `loghorn status` shows the environment variables an OTEL client
  needs to send to `serve`.
- **[user]** Phase 1 of this spec runs before the CLI query plan, whose last
  tasks are amended to use `query` and `status`.
- **[proposed]** Scheduled pulls are configuration that `serve` executes;
  `loghorn source` only edits it. "Cron" is not a mode.

## Goals

- Each command shows and accepts only the flags that apply to it.
- Bare `loghorn` prints a short, grouped command list.
- One always-on `serve` receives everything live and serves the web page,
  meeting the OS-service requirements from phase 1.
- The two most-typed forms keep working: `loghorn -- <cmd>` and
  `loghorn <loopback URL>`.

## Non-goals

- `loghorn service install|uninstall|status` (a separate task; this spec only
  makes `serve` ready for it).
- The DuckDB store, runs as a query concept, retention by run, alerts and
  follow sources (architecture notes). Phase 1 keeps today's storage: the
  day-rotated `loghorn.log` and `loghorn.traces.jsonl`, moved to the data
  folder.
- Moving existing per-repo `.loghorn/` logs into the data folder. They stay
  where they are; `open` can read one as a file.
- Long (`--flag`) options or a third-party CLI library. Flags stay Go-style
  single dash, parsed with one `flag.FlagSet` per command.

## Commands

| Command | Lifetime | Phase | What it does |
|---|---|---|---|
| `loghorn serve [flags]` | until SIGTERM/SIGINT | 1 | The store: OTLP receiver, web explorer and API, storage in the data folder; later expiry and scheduled pulls. |
| `loghorn run [flags] [--] <cmd> [args]` | while the child runs | 1 | Wrap a command: pass output through unchanged, keep its exit status, send its records to `serve`. |
| `loghorn run [flags] < pipe` | until end of input | 1 | Record piped input, copying it to stdout. |
| `loghorn open [flags] <log>` | 1: until Ctrl-C; 2: one shot | 1 | Open a finished log: a file, `-` (stdin), or a GitHub Actions run or job. |
| `loghorn query [flags] <url\|@file\|@->` | one shot | 1 (CLI query plan) | Print a web view's data. |
| `loghorn ui [-print]` | one shot | 1 | Open `serve`'s web page in the default browser. |
| `loghorn status [-env]` | one shot | 1 | `serve` health, ports, data folder, incoming runs, and the OTEL client environment. |
| `loghorn config` | one shot | 1 | Print the config file in effect and the settings it resolves to. |
| `loghorn source add\|list\|pause\|resume\|rm` | one shot | 3 | Edit scheduled pulls; `serve` runs them. |
| `loghorn help [command]` | one shot | 1 | Same as `-h`. |

### `open`

In phase 1, `open` shows a finished log in a temporary web view of its own
(today's replay code: it prints the URL and serves until Ctrl-C), on a free
port, apart from `serve`. `-filter` prints the important records to stdout
instead; `-filter -view` does both. A replay is a finished, separate log,
and `serve`'s phase 1 store holds one live stream of records, so mixing a
week-old GitHub job into it would put it into the live view.

In phase 2, `open` imports the log into `serve` as a run (idempotent,
decision 13), prints `serve`'s URL filtered to that run, and exits; the
temporary view goes away. "Open" is true in both phases, which is why the
command isn't called `import`.

- The positional is a path, `-` (stdin: a file or a pipe, never recorded),
  or a GitHub Actions URL. A job URL (`…/actions/runs/N/job/M`) and a run
  URL (`…/actions/runs/N`) are told apart by their path. Bare IDs are
  ambiguous, so they use `-gh-job ID` / `-gh-run ID`.
- `kubectl logs pod | loghorn open -filter -` filters without recording.
- Exactly one source; none or two is a usage error.

### Top-level help

```
loghorn — capture, store and query the logs and traces of local apps

Usage: loghorn <command> [flags] [args]

Store
  serve            run the store: receive OTLP, serve the web explorer

Capture
  run -- <cmd>     run a command, passing its output through, and record it
  open <log>       open a finished log: a file, or a GitHub Actions run or job

Read
  ui               open the web explorer in your browser
  query <url>      print a web view's data as TSV, CSV, Markdown, JSON, ...
  status           show serve's state and the OTEL environment for apps
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
existing `removedFlagError` grows into it), for example:

```
loghorn: -web was removed: run 'loghorn serve' for the web view; 'loghorn run' sends to it, and 'loghorn open <log>' shows a finished log
loghorn: -otel was removed: 'loghorn run' always points the command's OpenTelemetry SDK at 'loghorn serve'
```

No compatibility shim beyond the two shortcuts: loghorn has few users, and a
precise error is cheaper to keep correct than an alias layer. `-scrollback`
and `-theme` keep their "removed with the terminal UI" message.

## Flag mapping

| Today | New |
|---|---|
| `loghorn -- cmd` | `loghorn run -- cmd` (shortcut kept); records go to `serve` |
| `cmd \| loghorn` | `cmd \| loghorn run` |
| `loghorn -web` (alone) | `loghorn serve` |
| `-web` with a command or pipe | removed; `run` prints `serve`'s URL |
| `-web-port N` | `serve -web-port`; `open -web-port` for its temporary view |
| `-otel` | removed; `run` always points the child's `OTEL_*` variables at `serve` |
| `-filter` | `run -filter`, `open -filter` |
| `-notify`, `-notify-max-age`, `-notify-reset`, `-notify-sound` | `run` (alerts from `serve` are a separate design, decision 37) |
| `-shutdown-grace` | `run` |
| `-exclusive` | removed: `run` no longer writes a per-directory log file |
| `loghorn -web\|-filter < file` | `loghorn open file`, or `open - < file` |
| `-historical` | removed: `serve` loads the stored days at startup; `open .loghorn/loghorn-2026-11-20.log` reads an old per-repo day |
| `-gh-job ID\|URL` | `open URL` or `open -gh-job ID` |
| `-gh-run ID\|URL` | `open URL` or `open -gh-run ID` |
| `-config` | `loghorn config` |
| `-url`, `-o`, `-format`, `-to-sql` (CLI query plan) | `query` flags; `-url X` becomes the positional |
| `-instances` (CLI query plan) | `loghorn status` |

`run` refuses a replay source and `open` refuses a command, so the
cross-checks in `mode.check()` shrink to: `run` needs a command or a pipe;
`open` needs exactly one source.

## `serve`

### What it does in phase 1

- Binds the OTLP/HTTP receiver (4318) and the web server (4380) on
  127.0.0.1, and the unix socket.
- Stores what arrives — OTLP logs and spans from apps, and the records `run`
  sends — with today's writers, in the data folder: `loghorn.log` (records)
  and `loghorn.traces.jsonl` (spans), rotated and expired as today.
- Serves today's web page over one live store, preloaded from the stored
  days at startup (today's `-web` live mode). Records from several `run`s
  and apps arrive concurrently; each carries its `service.name` and
  `loghorn.run.id`, which the page's service selection already uses.
- Adopts incoming runs (see When `serve` is down).

`serve` reads only user-level config: `config.toml` from
`$XDG_CONFIG_HOME/loghorn` or `~/.config/loghorn`, and the page's saved
settings from the `svcconf` base, as the web server does today. Repo
`.config/loghorn/` settings (input framing, field naming) are applied by the
client that parses the lines, i.e. `run` and `open`, which have a start
directory.

### `run` as a client

`run` keeps everything it does locally today (pass-through, pty, signals,
exit status, `-filter`, `-notify*`) and parses each line with the existing
adapters. Instead of appending to `.loghorn/loghorn.log`, it sends the
records to `serve` as OTLP logs, batched, over OTLP/HTTP to the port
`serve` reports. Each log record's body is the framed line exactly as
`run` would have written it to the log file; the resource carries
`service.name`, `loghorn.run.id` and `loghorn.input` (the run's
`[input]` settings as JSON), and the scope is named `loghorn.run`. `serve`
recognises that scope and records the body through a recorder with the
run's input settings and service, so a run's stored records are the same
as today's. Any other OTLP endpoint sees ordinary log records with the
line as body. (This narrows decision 9, which also proposed parsed fields
as attributes: `serve` would only parse the body again, and the run's
repo settings must decide that parse.) The child's `OTEL_*` variables point at `serve`'s
OTLP port (`otlp.ChildEnv`, unchanged). `run` prints `serve`'s web URL to
stderr once, at start.

`service.name` for the wrapped output: `-service NAME`, else the child's
`OTEL_SERVICE_NAME`, else the repo directory name plus the command's base
name (decision 9 lists the choices; this is the phase 1 pick).

### Data folder

Per user: macOS `~/Library/Application Support/loghorn`; Linux
`$XDG_STATE_HOME/loghorn` (default `~/.local/state/loghorn`); Windows
`%LOCALAPPDATA%\loghorn`. `serve -data DIR` overrides it. `run` needs it
only when recording with `serve` down (see below), and finds it the same
way, with `$LOGHORN_DATA_DIR` in place of the flag.

### Ports and the socket

Each port is set, highest precedence first, by:

1. a `serve` flag: `-otlp-http-port`, `-otlp-grpc-port`, `-web-port`;
2. the user-level `config.toml`, `[serve]` keys `otlp-http-port`,
   `otlp-grpc-port`, `web-port`;
3. the defaults 4318, 4317, 4380.

(4317 is bound only once the gRPC receiver lands,
`2026-11-19-otlp-grpc-design.md`.)

`serve` listens on `/tmp/loghorn.sock` (`serve -socket PATH` overrides; on
Windows the default is `%LOCALAPPDATA%\loghorn\loghorn.sock`). `/api/session`
on the socket reports the ports actually bound, the data folder, pid, start
time and version. Clients connect to the socket first, so a port flag on
`serve` reaches them with no extra setting; `$LOGHORN_SOCKET` points them at
another instance's socket. The socket replaces the CLI query design's
per-instance `$TMPDIR/loghorn/<pid>.sock`: there is one `serve`, and a
launchd job may not see the terminal's `$TMPDIR`. On startup `serve`
removes a socket file that refuses connections (left by a crash); one that
answers belongs to a live `serve`, so starting fails (requirement 5). Linux
`systemd-tmpfiles` may age out files in `/tmp`; `serve` checks every few
minutes that its socket path still exists and re-creates it if not.

A second instance takes its own socket, data folder and ports:
`loghorn serve -socket /tmp/loghorn-b.sock -data ~/lh-b -web-port 4390 -otlp-http-port 4328`.

### OS-service requirements

From the `loghorn service` work. Each is a property of `serve` itself; none
needs the installer. All apply from phase 1.

1. **Independent of the start directory.** launchd starts jobs in `/`.
   `serve` never reads or writes relative to its working directory and
   starts with no repo anywhere.
2. **Fixed, configurable ports**, as above. Today both listeners use port 0
   (`internal/otlp/receiver.go:34`, `web.Listen`).
3. **Foreground until SIGTERM or SIGINT**, then stop accepting, drain
   in-flight requests, flush and close the files (phase 2: checkpoint the
   engine, finish Parquet writes), remove the socket, release the lock and
   exit 0. `serve` never forks itself into the background; the supervisor
   (or the user's terminal) owns its lifetime.
4. **Fail fast and loud at startup.** Order: load config → take the data
   folder lock → bind the socket and every listener → open storage →
   adopt incoming runs → serve. Any failure before "serve" prints one
   `loghorn serve: …` line and exits non-zero (2 for a bad flag or config,
   1 otherwise), so the supervisor's restart and backoff work. A busy port
   names its flag and config key:
   `loghorn serve: 127.0.0.1:4318 is in use (another OTLP receiver?); choose another port with -otlp-http-port or [serve] otlp-http-port in <config path>`.
   Nothing that needs undoing happens before the listeners are bound (the
   startup-ordering rule in CLAUDE.md, extended to `serve`).
5. **One instance per data folder**, enforced by a lock file in it. A second
   `serve` on the same folder exits 1 naming the holder's pid. The lock sits
   behind a small interface with per-OS files (`flock` on Unix, `LockFileEx`
   on Windows through `golang.org/x/sys/windows`); `internal/logfile`'s
   `syscall.Flock` moves behind it.
6. **Its own log goes to stderr** by default (launchd and systemd capture
   it). `-log FILE` writes it to a file instead, for a supervisor that
   doesn't capture stderr (Windows). Nothing in `serve` assumes stderr is
   seen by a person.
7. **No GUI session required.** Anything that notifies (later alerts)
   treats a missing display, D-Bus session (`DBUS_SESSION_BUS_ADDRESS` unset
   under systemd) or notification permission as a soft failure: one warning
   in the log, then silence, never an exit.

New `serve` code stays free of Unix-only syscalls so a Windows serve-only
build stays within reach. Today `GOOS=windows go build ./...` fails only on
`syscall.Flock` (`internal/logfile/lock.go`) and `SIGWINCH`
(`internal/wrap/wrap.go`).

## When `serve` is down

No auto-start. `query`, `status` and (phase 2) `open` exit 1 with:

```
loghorn: loghorn serve isn't running (no answer on /tmp/loghorn.sock); start it with 'loghorn serve' or install it with 'loghorn service install'
```

`run` never blocks or fails the child (decision 9). What it does about
recording is the user's choice, so it never quietly stands in for a failed
`serve`. The setting is `[run] serve-down` in the user-level `config.toml`,
overridden per invocation by `run -serve-down`:

| Value | What `run` does |
|---|---|
| `record` (default) | Records the run into the data folder (below). |
| `skip` | Passes output through and records nothing. |
| `fail` | Exits 1 before starting the child, with the message above. For CI and scripts that must capture every run. |

`record` is the default because the commonest case is an agent starting a
test run, and losing its logs is the worse outcome. The failure stays
visible three ways:

- `run` prints a warning to stderr every time: `loghorn: serve isn't
  running; recording this run to <data>/incoming/<run id> (apps sending
  OTLP on their own aren't recorded until serve starts)`.
- `loghorn status` reports it to the user even when the warning only
  reached an agent: `serve isn't running; 3 runs recorded to incoming/
  since 08:12`.
- `serve` logs each incoming run it adopts when it comes back.

How `record` works:

1. `run` writes its records to `<data>/incoming/<run id>/` with today's
   writers (`loghorn.log`, `loghorn.traces.jsonl`) and locks only that
   folder, never the data folder's lock. `serve` can start, or be restarted
   by its supervisor, while a long-lived `run` (a dev server) is going, and
   several `run`s can record at once.
2. It receives the child's own OTLP on an ephemeral loopback port and
   points the child's `OTEL_*` variables there (today's `-otel` path),
   storing what arrives in the same folder.
3. It keeps writing there until the child exits, even if `serve` comes up
   meanwhile: switching mid-run would split it.

`serve` adopts an incoming folder whose lock is free, at startup and every
minute: it loads the records into its live store, appends them to its own
files, then deletes the folder. A folder still locked belongs to a running
`run` and is left for later. If the data folder can't be found or created,
`record` degrades to `skip` with a warning.

## One-shot `run -serve`

**[user]** `run -serve` starts `serve` inside `run` when no serve answers on the
socket: the same data folder, socket and user-level ports as `loghorn serve`, so
it records straight into the store, receives OTLP from any app on 4318 while it
runs, and keeps the web view up after the command exits until SIGINT or SIGTERM.
The exit status stays the command's. With a serve already running, `-serve`
adds nothing. If the in-process serve can't start, `run` prints why ("can't
start serve: …") and follows `serve-down`. This is not auto-start: it happens
only when asked for, on that run. While it holds the data folder's lock, a
supervised serve fails its start and is retried until the one-shot run ends.

**[user]** It waits for Ctrl-C only when stdin is a terminal; otherwise it
closes serve when the run ends, saying the run is stored. **[user]** AI coding
agents must not start serve: `loghorn serve` exits 2 and `run -serve` is
ignored (falling back to serve-down) when `CLAUDECODE`, `AI_AGENT`,
`CODEX_SANDBOX` or `CODEX_SANDBOX_NETWORK_DISABLED` is set, unless
`LOGHORN_ALLOW_AGENT` is. An agent's own runs still record.

## `loghorn ui`

Asks the socket for `serve`'s web URL (`/api/session`) and opens it in the
default browser: `open` on macOS, `xdg-open` on Linux,
`rundll32 url.dll,FileProtocolHandler` on Windows. It prints the URL to
stderr as well, and exits 0 once the opener has been started; it doesn't
wait for the browser.

- `-print` prints the URL to stdout and opens nothing, for a remote shell
  or a script.
- If the opener is missing or fails (a headless box, no `xdg-open`), `ui`
  prints the URL and a one-line note, and exits 1.
- With `serve` down it prints the "isn't running" message and exits 1;
  it never starts `serve`.
- It honours `$LOGHORN_SOCKET`, so it opens a second instance's page when
  pointed at that instance.

The opener lives in a small package of its own, so phase 2's `open` (which
prints `serve`'s URL filtered to the imported run) can offer the same.

## `loghorn status`

Prints, in order:

- whether `serve` is running: pid, start time, version, data folder,
  socket;
- the ports it bound (web URL, OTLP/HTTP, and OTLP/gRPC once it exists);
- incoming runs waiting to be adopted, if any;
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
are stored. `run` sets its child's variables itself, so these are for apps
started without `run`.

`status -env` prints only the `export` lines, so
`eval "$(loghorn status -env)"` works in a shell or a repo's dev script.
When `serve` isn't running, `status` prints the "isn't running" line, then
the environment from the configured ports (so a repo can be set up before
the service is), and exits 1; `status -env` prints the lines and exits 0,
since the variables don't depend on `serve` being up.

## Phases

1. **Subcommands and an always-on `serve`** (this spec): dispatch,
   per-command flag sets, help and migration errors; `serve` with fixed
   ports, the socket, the data folder and the service requirements, over
   today's file storage; `run` as an OTLP client with `serve-down`;
   `status`; `open` with its temporary view.
2. **The DuckDB store behind `serve`** (architecture spec, not this one).
   The command line doesn't change; `open` imports into `serve` instead of
   showing a temporary view.
3. **`loghorn source`** with follow sources.

### Interaction with the CLI query plan

`docs/superpowers/plans/2026-11-23-cli-query.md` was executed before this
spec's plan. It added top-level `-url`, `-o`, `-format`, `-limit`,
`-to-sql` and `-instances`, the positional URL, and per-instance sockets
under `$TMPDIR/loghorn` (`web.SocketDir`, `internal/instances`). Phase 1
moves them: the flags go under `query` (`-url X` becomes the positional),
`-instances` is dropped in favour of `status`, and the per-instance
sockets and discovery are replaced by `serve`'s one socket. `open`'s
temporary view gets no socket; `query` reaches it by its full URL.

## Testing

- Dispatch: no args (with stdin a terminal, a pipe, a file: none records),
  `--`, loopback URL, each command, `-h`, an old flag, an unknown word.
  Exit codes 0/2 as above.
- Migration error for every removed or moved flag, including `-scrollback`
  and `-theme`.
- Flag placement: `run` passes a child's `-v` through; `query URL -o f` and
  `open file -filter` parse flags after the positional.
- `open` source detection: path, `-`, job URL, run URL, `-gh-job`,
  `-gh-run`, two sources and none (errors).
- `serve` startup, each with `HOME` and the data folder in a temp dir,
  cwd `/`, and a temp socket path: a busy port (message names the flag and
  key), a held data-folder lock, a live socket, a stale socket (removed,
  start succeeds), a bad config; each exits non-zero before binding or
  writing anything else. SIGTERM flushes, removes the socket and exits 0.
  Port flags override config, and `/api/session` reports the bound ports.
- `run` against a test `serve`: records arrive with `service.name` and the
  run id, output and exit status pass through unchanged, the child's OTLP
  reaches `serve`.
- `serve-down`: `skip` records nothing; `fail` exits 1 without starting
  the child; `record` writes an incoming folder, two concurrent `run`s
  write two, `serve` starts while they hold their locks, adopts each once
  its run ends, and `status` reports them in between.
- `status` and `status -env` with `serve` up and down.
- `ui` with the opener replaced by a test double: the URL from
  `/api/session` reaches it; `-print` prints it and starts nothing; a
  failing opener prints the URL and exits 1; `serve` down exits 1.
- Existing e2e and main tests move to the new forms.
- `go vet`, `go test -race ./...`, `GOOS=windows go vet ./internal/...` on
  the packages `serve` uses; the README usage block is checked against
  `loghorn help` output by a test, so they can't drift.

## Open questions

1. **Shells other than sh for `status -env`** (fish, PowerShell): add a
   `-shell` choice when someone needs it.
