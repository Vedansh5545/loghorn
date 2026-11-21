<img src="images/logo.svg" alt="Alt text" width="300">

# loghorn

Wrap a dev server, record its logs, and surface the failures.

loghorn runs your command and stays out of the way: its output reaches the
terminal unchanged, and every record is also kept in a per-project log file. From
there `-filter` prints only the lines that matter and `-web` serves a query view of
them — so a request that failed is findable in context rather than lost in ten
thousand routine lines.

A line is a **failure** if any of these holds:

- its severity is `ERROR` or above
- its HTTP status is 500 or above
- its message matches `panic`, `fatal`, `exception` or `traceback`, case-insensitively

It reads GCP `LogEntry` JSON, gcloud's YAML and JSON output, and plain text, deciding
per stream which it is.

## Install

Requires Go 1.25 or newer.

```sh
go install github.com/jimbeveridge/loghorn@latest
```

Or from a checkout:

```sh
go build -o loghorn .
```

## Running it

**Wrap the producer:**

```sh
loghorn -- npm run dev
```

loghorn is a transparent wrapper. The command's stdout and stderr reach the terminal
unchanged — through a pty when stdout is a terminal, so colours, progress bars and
prompts work as they do without loghorn — and the keyboard stays the command's.
`Ctrl-C` goes straight to the command; `SIGTERM` and `SIGHUP` sent to loghorn are
forwarded to it, and a command still running `--shutdown-grace` after a `SIGTERM` is
killed. loghorn exits with the command's status (127 when it can't be found).
Meanwhile every record is appended to the project's log file, with escape sequences
removed.

**Record a pipe** — the input is copied to stdout and recorded the same way:

```sh
kubectl logs -f pod | loghorn
gcloud beta logging tail --json | loghorn
```

**Show only the failures** — `-filter` prints just the failing records to stdout,
one per line, instead of everything. From a command or a pipe they are printed as
recorded, with escape sequences removed; from a replay, as they were stored. It works
with every input; with a command the exit status is still the command's:

```sh
loghorn -filter -- go test ./...
loghorn -filter < capture.json
```

**Browse in a web view** — `-web` serves a query view (see below) and prints its URL
to stderr. It combines with a command, a pipe, a replay and `-filter`. On its own it
browses the stored logs:

```sh
loghorn -web -- npm run dev
loghorn -web
```

**Receiving OTEL logs and traces (`-otel`)** — for a command that exports
OpenTelemetry logs or traces, `-otel` starts an OTLP/HTTP receiver on `127.0.0.1`
(a free port) and points the command at it:

```sh
loghorn -otel -- node server.js
```

loghorn sets `OTEL_EXPORTER_OTLP_LOGS_ENDPOINT` (`http://127.0.0.1:<port>/v1/logs`),
`OTEL_EXPORTER_OTLP_LOGS_PROTOCOL=http/protobuf`, `OTEL_LOGS_EXPORTER=otlp`,
`OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` (`http://127.0.0.1:<port>/v1/traces`),
`OTEL_EXPORTER_OTLP_TRACES_PROTOCOL=http/protobuf`, `OTEL_TRACES_EXPORTER=otlp`,
`OTEL_EXPORTER_OTLP_ENDPOINT` (only when you hadn't set it), `OTEL_RESOURCE_ATTRIBUTES`
(yours plus `loghorn.run.id=<id>`) and `LOGHORN_RUN_ID` for the command, and prints the
endpoint and run id to stderr. Received records are appended to `.loghorn/loghorn.log`
as JSON lines (and shown in `-web`, and counted by `-notify`); they are not printed by
`-filter`, which shows the command's own output. Each record's stream is its
`event.name`, else its instrumentation scope name, else `otel`, so `-web` lists it
under that name. The body, attributes, `service.name`, `trace_id`, `span_id` and the
run id are stored as fields, and `loghorn.source` is `otlp`.

Spans the command exports are stored one JSON object per line in
`.loghorn/loghorn.traces.jsonl`, kept and rotated like `loghorn.log` (archived as
`loghorn-<date>.traces.jsonl`); the file appears with the first span.
Each line has `trace_id`, `span_id`, `parent_span_id`, `name`, `kind`, `start`, `end`,
`duration_ms`, `status`, `status_message`, `attributes`, `events`, `links`,
`service.name`, `loghorn.run.id`, `otel.resource` and the instrumentation scope (empty
fields are left out). Spans are not part of `loghorn.log`, `-web`, `-filter` or
`-notify`, and they don't count as OTLP logs below.

Once the first OTLP record arrives, the command's stdout lines that are JSON objects
are no longer stored, since they are most likely the same events logged twice; other
lines are still stored, and everything is still printed. If the command sends no OTLP
logs at all, loghorn says `-otel: no OTLP logs received; stdout was recorded instead`
on exit. `-otel` needs a command after `--`; it combines with `-web` and `-filter`.

**Replays** read a finished log. They never write the log file, so they need `-web`
or `-filter` to show anything:

```sh
loghorn -filter < capture.json      # a file on stdin
loghorn -web -historical            # the stored days, oldest first, then today
loghorn -filter -gh-job 133607019104
loghorn -web -gh-job https://github.com/owner/repo/actions/runs/43954926722/job/133607019104
loghorn -web -gh-run https://github.com/owner/repo/actions/runs/43954926722
```

`-gh-job` takes a job ID (the number after `/job/` in its URL; the run is looked up) or
the whole URL. `-gh-run` takes a run ID or run URL and shows the run's first failed job
(its first job when none failed), listing every job on stderr so you can name another
with `-gh-job`. Both go through the GitHub CLI, so `gh` must be installed and logged
in; a bare ID uses the repository of the current directory. The replay flags refuse a
command or piped stdin, so from cron or CI pass `</dev/null`.

## gcloud

Both shapes gcloud emits are read as-is:

```sh
gcloud beta logging tail --json | loghorn
gcloud logging read --format=yaml | loghorn
```

`--json` produces a JSON array whose first line is a bare `[`, with each entry
pretty-printed across many lines; `--format=yaml` produces `---`-separated documents.
loghorn detects either from the stream's first line and groups each entry into one
record. A line that isn't JSON — a gcloud warning on a merged stderr, say — stays a
plain-text line. A tail stopped with `Ctrl-C` leaves its last entry cut off mid-key;
that partial record is kept and flagged rather than dropped.

gcloud also names `LogEntry` fields in snake_case (`json_payload`, `http_request`)
and encodes severity numerically (`200` for `INFO`, `500` for `ERROR`). Both are
accepted by default, alongside the canonical camelCase-and-string schema.

## Configuration

Settings live in `.config/loghorn/config.toml`. Every key is optional, and every
default is the behaviour loghorn has without a file at all, which is why the file is
worth creating only when a producer needs something unusual.

```toml
[input]
format       = "auto"   # auto | json | yaml | text
field-naming = "auto"   # auto | camel | snake
severity     = "auto"   # auto | string | numeric
```

- **`format`** — how the stream is split into records. `auto` decides from the first
  line. `json` forces JSON-array framing, which is what a tail attached *mid-stream*
  needs, having missed the opening `[`. `yaml` forces `---` documents. `text` turns
  all grouping off, one record per line.
- **`field-naming`** — `auto` accepts either spelling of a `LogEntry` field name;
  `camel` and `snake` restrict it to one.
- **`severity`** — `auto` accepts a string name or a numeric code; `string` and
  `numeric` restrict it to one.
A `display` section, which configured the terminal UI of older versions, is ignored with a warning.

### Where the file is found

loghorn walks up from the directory it starts in, and **the first
`.config/loghorn/config.toml` it finds wins outright** — nothing is merged across
levels, so one file explains all of loghorn's configured behaviour. If the walk
reaches the root without a match, it makes one final stop at
`$XDG_CONFIG_HOME/loghorn/config.toml`, or `~/.config/loghorn/config.toml` when
`XDG_CONFIG_HOME` is unset.

That final stop is what makes personal defaults apply when you run loghorn from
outside your home directory. Note the consequence of "nearest wins" for a working
directory *inside* your home tree: once `~/.config/loghorn/config.toml` exists, the
walk finds that literal path first, so `XDG_CONFIG_HOME` no longer redirects anything.

To see which file is in effect and what it resolves to:

```sh
loghorn -config
```

`.config/loghorn/` is hand-written and belongs in version control. It is not
`.loghorn/`, which holds generated logs and ignores itself.

## The log file

Every record loghorn receives is appended to `.loghorn/loghorn.log` in the directory
it starts in, so each project gets its own logs and its own lock. Each finished day is
kept as `loghorn-YYYY-MM-DD.log`, and days before the last three are deleted. The
directory is created owner-only and ignores itself, because a dev server's logs can
carry tokens and auth headers.

Input read from a file (`loghorn < file`) is not recorded — it is already on disk.
loghorn never creates or writes `.loghorn` inside its own source tree, so recording
live input there (a launched command or a pipe) fails; reading a file, `-historical`
and the GitHub flags work there.

## Web query view

`loghorn -web` also serves a browser view of the run's structured records at
`http://127.0.0.1:<port>/`; the URL is printed to stderr. With a command it
serves until the command exits, with a pipe until end of input, and with a
replay (or alone) until `Ctrl-C`. Pick the port with `-web-port`; the default is a free one.
The server listens on loopback only and refuses requests for any other Host,
and writes that do not come from its own origin.

It lists the streams found in the log (database lines, HTTP lines, and so on),
and for each one a table, newest first, with typed filters, a time range and
text search. Fields get types by inference (int, float, bool, string, timestamp,
duration); a value that does not fit its type is shown raw and flagged, and is
left out of comparisons. Live runs also load the stored days from `.loghorn`;
`-historical`, `-gh-job` and `-gh-run` feed it their replay.

The sidebar holds the whole query: the mode tabs, the stream, the time range,
and the filters, one row per field / operator / value (a bad value is flagged on
its own row and the other filters keep working). Three modes share it:

- **Samples** — the matching records themselves; click a row for the full
  record. Click a column name (in the header or under *Columns*) for its
  menu: visualizer, unit and label (saved to the stream's schema at once),
  Hide, Sort ascending/descending, *Add / Move* (put a field not shown, or
  another column, right of this one; columns hidden in this query come
  first), and Move to start. The trash icon in a header hides the column.
  *A→Z* orders the columns by the last part of their path (`status` for
  `httpRequest.status`); *restore defaults* brings back the stream's default
  columns. Drag a header's edge to set its width, which is saved in the
  schema too; double-click the edge to return to the natural width.
- **Table** — one row per group. *Group by* takes one or more fields (records
  with an empty or bad value group as `(empty)`); *Metrics* always includes
  `count` and adds `sum`, `avg`, `min`, `max`, `p50`, `p95` or `p99` of a
  number field (int, float or duration).
- **Graph** — a line over time per group: the top 10 groups by count, the
  rest folded into `other`. *Plot* picks the metric drawn (`count` or one of
  the metrics); *Granularity* sets the time bucket (`1s` to `1d`), or `auto`,
  which aims for about 100 buckets and shows the size it chose. A range that
  would need more than 2,000 buckets asks for a coarser granularity. Hover
  for each group's value in a bucket.

The URL carries the query, and a favorite saves the mode with it.

Schemas (field types, display and column widths) and favorite queries are saved as TOML in
the repo's `.config/loghorn/`: `schemas/<stream>.toml` for log streams,
`schemas/otel/<stream>.toml` for span streams, and `queries.toml`.
Nothing is written under your home directory.

## Flags

| Flag | Default | |
|---|---|---|
| `--filter` | off | print only the failures to stdout instead of everything |
| `-historical` | off | replay the stored files (needs `-web` or `-filter`) |
| `-gh-job` | | replay a GitHub Actions job's log, by job ID or URL (needs `-web` or `-filter`) |
| `-gh-run` | | replay the first failed job of a GitHub Actions run, by run ID or URL (needs `-web` or `-filter`) |
| `--otel` | off | receive the command's OTLP/HTTP logs and traces on 127.0.0.1 and record them (needs `-- <command>`) |
| `-web` | off | serve the web query view on 127.0.0.1 and print its URL to stderr |
| `-web-port` | `0` | port for `-web` (0 picks a free one) |
| `-config` | off | print the config file in effect, then exit |
| `--notify` | off | desktop notification on fresh errors |
| `--notify-max-age` | `1s` | skip notifications for errors older than this |
| `--notify-reset` | `15s` | quiet gap that ends an error burst |
| `--notify-sound` | on | play the system alert sound with notifications |
| `--exclusive` | off | exit rather than run without the log file if another loghorn holds it |
| `--shutdown-grace` | `5s` | after forwarding SIGTERM to the command, SIGKILL it this much later |

One dash or two makes no difference — `-filter` and `--filter` are the same flag.
`loghorn --help` is authoritative.

## Docs

`docs/ROADMAP.md` tracks what is built and what is planned. Design documents for each
feature live in `docs/superpowers/specs/`.

## License

Apache 2.0 — see [LICENSE](LICENSE).
