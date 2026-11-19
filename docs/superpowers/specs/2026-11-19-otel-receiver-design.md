# `-otel` receiver and removing the TUI — design

Date: 2026-11-19
Status: draft, awaiting approval

An interim step towards `loghorn serve` (see
`2026-11-19-architecture-working-notes.md`, decisions 8–9 and 41). It builds
the OTLP receiver and the OTLP → record mapping that `serve` will need, on
top of today's subprocess path and `.loghorn/` storage, and removes the TUI
(decision 1: "a bit of a failed experiment"), turning `loghorn -- <cmd>`
into a transparent wrapper.

## Intent

auction is adding OTEL logging now. To test it locally, a developer (or an
LLM) runs

```
loghorn -otel -- npm run dev
```

and loghorn acts as the OTLP endpoint for that command: it receives the
app's OTEL log records and stores them in `.loghorn/` alongside the
command's other output, so `-web` and `-historical` see them with no other
changes. The command's output reaches the terminal (or the LLM) unchanged,
as if loghorn weren't there.

## Goals

- `-otel` with `-- <cmd>`: loghorn binds an OTLP/HTTP receiver on loopback,
  points the child at it through the standard OTEL environment variables,
  and runs the child.
- The TUI is gone. `loghorn -- <cmd>` passes the child's output through
  unchanged, keeps its exit code, and records it, with or without `-otel`.
- OTEL log records are stored as JSON lines in `.loghorn/` that the existing
  normaliser reads, so every existing view works.
- Every record from one invocation carries the same run id.
- The child's JSON stdout is not stored twice once its OTLP logs arrive.

## Non-goals

- `loghorn serve`, DuckDB, Parquet — later.
- Storing traces or metrics. They are accepted and discarded.
- OTLP/gRPC. HTTP only (protobuf and JSON).
- `-otel` without a command (a standalone receiver) — that is `serve`.
- Moving alerts into the web view (decision 37) — later.
- Importing GitHub Actions logs into `.loghorn/` — later, with `serve`.

## Design

### Flag and startup

- `-otel` requires `-- <cmd>`. It is rejected with piped input, a file,
  `-historical`, `-gh-job` and `-gh-run`. It works with `-web` and `-filter`.
- The receiver binds `127.0.0.1:0` **before** the child is launched, like
  `-web` (CLAUDE.md, "Startup ordering"): a bind failure exits with no child
  running.
- The chosen endpoint is printed with the other startup information.

### The child's environment

loghorn adds to the child's environment (grandchildren inherit it):

| variable | value | why |
|---|---|---|
| `OTEL_EXPORTER_OTLP_LOGS_ENDPOINT` | `http://127.0.0.1:<port>/v1/logs` | always set; overrides the app's own logs endpoint |
| `OTEL_EXPORTER_OTLP_LOGS_PROTOCOL` | `http/protobuf` | always set |
| `OTEL_LOGS_EXPORTER` | `otlp` | always set |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | `http://127.0.0.1:<port>` | only if unset, so traces/metrics don't fail against the default `localhost:4318` |
| `OTEL_RESOURCE_ATTRIBUTES` | existing value + `loghorn.run.id=<id>` | run identity |
| `LOGHORN_RUN_ID` | `<id>` | for scripts and test harnesses |

The run id is `<UTC start time, compact>-<4 random hex>`, e.g.
`20261119T204012Z-3fa2`.

### The receiver

- `POST /v1/logs`: `application/x-protobuf` or `application/json` (OTLP's
  JSON encoding), optionally `Content-Encoding: gzip`. Replies with an empty
  `ExportLogsServiceResponse` in the request's encoding. Bodies over 16 MB
  get 413.
- `POST /v1/traces`, `POST /v1/metrics`: decoded enough to validate, then
  discarded; reply success so SDKs don't log export errors.
- Anything else: 404. Listens on loopback only.
- Uses the OTLP protobuf types (`go.opentelemetry.io/proto/otlp`) and
  `protojson`; no collector dependency.
- After the child exits, the receiver stays up until 500 ms pass with no
  request, at most 3 s, so the SDK's final batch is stored; then it closes.

### OTLP log record → stored line

Each `LogRecord`, with its `Resource` and `InstrumentationScope`, becomes
one JSON object written through the same path as a stdout line (into
`.loghorn/` and, with `-web`, the live session store):

| stored key | from |
|---|---|
| `timestamp` | `time_unix_nano`, else `observed_time_unix_nano`, RFC 3339 nano UTC |
| `severity` | `severity_text` if set, else from `severity_number`: 1–8 `DEBUG`, 9–12 `INFO`, 13–16 `WARNING`, 17–20 `ERROR`, 21–24 `CRITICAL` |
| `message` | `body` when it is a string |
| (top-level fields) | `body` when it is a map: its keys merged as fields; `msg`/`message` in it becomes `message` |
| (top-level fields) | record `attributes`, keys verbatim (`http.response.status_code`) |
| `trace_id`, `span_id` | hex, when non-zero |
| `service.name` | resource attribute, promoted |
| `loghorn.run.id` | resource attribute, promoted |
| `otel.resource` | the other resource attributes, as an object |
| `otel.scope.name`, `otel.scope.version` | the instrumentation scope |
| `labels.schema` | the stream (below) |
| `loghorn.source` | `"otlp"` |

Attribute values map to JSON naturally: strings, bools, ints (as numbers),
doubles, arrays, nested maps; bytes as base64. A record attribute that
collides with a promoted key is kept under `otel.attributes.<key>`.

**Stream.** OTEL has no `labels.schema`. Following OTEL conventions, the
stream is, in order: the record's `event_name` field or `event.name`
attribute (OTEL's name for a type of event with a defined structure); else
the instrumentation scope name; else `otel`. auction should set
`event.name` per payload shape (e.g. `auction.db`).

Saved schemas currently accept only stream names matching `[a-z0-9_-]+`.
Event names contain dots, so the rule widens to `[a-z0-9][a-z0-9._-]*`.

### The child's stdout

The child's output is passed through as described in "Removing the TUI".
For storage:

- Until the first OTLP log record arrives, every line is stored as today.
- After it, stdout lines that are JSON objects are **not** stored (the app's
  OTLP records carry the same events); every other line — npm output, stack
  traces, crash messages — still is.
- If the run ends with no OTLP log record received, loghorn prints
  "-otel: no OTLP logs received; stdout was recorded instead" on exit.

So a misconfigured app loses nothing, and a working one is stored once,
apart from any JSON lines written before its first export.

### Ordering

OTLP records arrive in batches (the SDK's default delay is up to a few
seconds), so file order is arrival order, not event order. Their own
timestamps are kept; the web view sorts by time. Effective-time inheritance
does not apply to OTLP records (they always have a time).

## Removing the TUI

### `loghorn -- <cmd>` becomes a transparent wrapper

- **Output.** When loghorn's stdout is a terminal, the child's stdout and
  stderr are a pty (as today, so tools keep colour and line buffering), and
  everything read from it is copied to loghorn's stdout. Otherwise (run by an
  LLM, a script, CI) they are two pipes, copied to loghorn's stdout and
  stderr respectively, so the child sees no terminal and nothing is merged.
  Bytes are copied unchanged; only the recorded copy is framed into lines
  and has ANSI escape sequences stripped.
- **Input and signals.** The child stays in the terminal's foreground
  process group (no `Setpgid`) and reads loghorn's stdin directly, so
  Ctrl-C, Ctrl-Z and keyboard input reach it exactly as without loghorn.
  loghorn ignores SIGINT and SIGQUIT while the child runs (as `time` does)
  and forwards SIGTERM and SIGHUP to it; after a forwarded SIGTERM,
  `-shutdown-grace` later it sends SIGKILL. The pty's window size follows
  loghorn's terminal (SIGWINCH).
- **Exit.** loghorn exits with the child's exit status (128 + signal number
  if it was killed by a signal), after the recorder and the `-otel` receiver
  drain. A loghorn-internal failure after launch (e.g. the log file
  stopped) is reported on stderr but never changes the exit status or stops
  the child.
- **`-web`** serves while the child runs and stops when loghorn exits. Its
  URL is printed to stderr at startup.

### Other inputs

| input | before | after |
|---|---|---|
| piped stdin (`cmd \| loghorn`) | TUI, recorded | passed through to stdout, recorded |
| `loghorn < file` | TUI replay | requires `-web` or `-filter` |
| `-historical` | TUI over stored files | requires `-web` or `-filter` |
| `loghorn -web` with no other input | (TUI live mode) | serves the stored `.loghorn/` files until Ctrl-C, like `-historical -web` |
| `-gh-job` | TUI replay | requires `-web` or `-filter` |
| `-gh-run` | TUI job picker | picks the first failed job (else the first job), prints the run's jobs and their ids to stderr; requires `-web` or `-filter` |

- **`-filter`** keeps its meaning — print only important lines — and now
  works with every input, including `-- <cmd>`, where it replaces the full
  pass-through on stdout. Useful for an LLM that only wants errors.
- **`-web` with `-filter`** is allowed (today it is rejected because the web
  view needed the TUI's run).
- **`-notify`** and its flags keep working: the alert coalescer and desktop
  notifier sit in the producer loop, not the TUI.

### What is deleted

- `internal/tui` (including the `-gh-run` picker), `internal/clipboard`, and
  any other package only the TUI imports (the builder confirms with
  `go build ./...` and `go vet ./...` after deleting).
- Flags `-scrollback` and `-theme`, and the terminal-background query.
  Passing them is an error naming them as removed.
- The `[display]` config section only configured the TUI's Detail pane. It
  is still accepted so existing config files load, with a one-line warning
  that it is ignored. (`internal/sqlfmt` stays: the web view uses it.)
- README: the "Keys" section and TUI descriptions go; "Running it" describes
  the wrapper, `-filter`, `-web` and `-otel`.

## Testing

- Mapping unit tests: string and map bodies, every attribute value type,
  severity text vs number, zero vs set trace ids, key collisions, each stream
  fallback, `event_name` field vs attribute.
- Receiver tests through `httptest.NewServer`: protobuf, JSON, gzip, 413,
  traces/metrics accepted, unknown path 404.
- An end-to-end test runs a small child (a Go test helper using the OTEL Go
  SDK's OTLP/HTTP log exporter) under `-otel` and checks the `.loghorn/`
  lines, the run id, the stdout dedupe switch-over, and the no-OTLP warning.
- Environment tests: existing `OTEL_RESOURCE_ATTRIBUTES` kept and extended;
  `OTEL_EXPORTER_OTLP_ENDPOINT` left alone when already set.
- Wrapper tests: output passes through byte-for-byte (pipes and pty); stdout
  and stderr stay separate with pipes; exit status and 128 + signal
  propagate; SIGTERM is forwarded and escalates to SIGKILL after the grace;
  the recording matches today's for the same output; `-filter` with
  `-- <cmd>`.
- Flag tests: removed flags error; each input row in the table above,
  including the "requires `-web` or `-filter`" errors; `[display]` warns.

## Decided by the user

- `-otel` on the normal subprocess path: loghorn acts as the collector, sets
  the OTEL environment, sets up the pty, and runs the command.
- Store in `.loghorn/` for this first pass.
- With `-otel`, store OTLP records plus non-JSON stdout lines, not JSON
  stdout lines (option (a)).
- Claude chooses the stream mapping following OTEL practice.
- Remove the TUI in this spec.

## Claude's calls (correct any of these)

- Stream = `event.name` → scope name → `otel`; saved-schema stream names may
  contain dots.
- Logs-specific endpoint variables always set; the generic endpoint only when
  unset.
- Run id format and `LOGHORN_RUN_ID`.
- JSON stdout is stored until the first OTLP record arrives, and a warning
  is printed if none ever does.
- Traces and metrics accepted and discarded.
- HTTP only; 16 MB body limit; 500 ms idle / 3 s max drain after exit.
- Stored key names (`trace_id`, `span_id`, `otel.resource`, `otel.scope.*`,
  `loghorn.source`).
- The wrapper: pty when stdout is a terminal, else separate pipes; the
  child stays in the terminal's process group and reads stdin directly;
  loghorn ignores SIGINT/SIGQUIT and forwards SIGTERM/SIGHUP; exit status
  passes through.
- Replays (`< file`, `-historical`, `-gh-*`) require `-web` or `-filter`;
  `loghorn -web` alone serves the stored files.
- `-gh-run` picks the first failed job instead of a picker.
- `-filter` works with every input; `-web` with `-filter` is allowed.
- `-notify` stays in the wrapper until alerts move to the web view.
- Removed flags error rather than being silently ignored; `[display]` is
  accepted with a warning.

## Left to the builder

- The single point where stdout lines and OTLP records both enter the
  writer and the live store.
- Exact JSON encoding helpers and where the mapping lives (e.g.
  `internal/otlp`).
- README and `-help` text.
- How `main.go` is restructured once the Bubble Tea program is gone.
