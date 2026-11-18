# Web query view — design

Date: 2026-11-17
Status: draft, awaiting approval

## Intent

A Scuba-like way to explore the logs loghorn is reading. Today the only way to
answer "which `db` lines touched `lots` and took over 50 ms?" is to scroll the
TUI or hand-write a Cloud Logging / SQL query. The web view replaces that with
a query builder: pick a stream, pick a field, pick an operator, set a value,
see the rows.

The first version only shows matching rows in a table. Later versions add
aggregation (group-by, count, avg, percentiles) shown as tables or graphs, so
the query model and engine are designed for that now even though the UI does
not expose it yet.

The data source is the auction backend's structured logs. Its log lines carry
a `labels.schema` stream label (`api`, `auth`, `db`, `email`, `external`,
`server`, `worker` — see auction `backend/src/http/logger.ts`), and each stream
has its own payload shape. loghorn must stay generic: nothing about auction's
streams or fields is hard-coded.

## Goals

- `loghorn -web` serves a page from the running loghorn process, alongside the
  TUI, for whatever that process is reading: a live run (command or pipe),
  `-historical`, `-gh-job`, or `-gh-run`.
- Pick a **stream**: the distinct values of `labels.schema`. Every query
  names exactly one stream. Lines without that label (npm/tsx output, GitHub
  Actions' own lines, any plain text) are not in the web view at all; they
  stay in the TUI.
- Build a query from **filters**, ANDed together. Each filter is
  field + operator + value(s), with operators chosen by the field's type:
  - number (int, float, duration): `=`, `!=`, `<`, `<=`, `>`, `>=`
  - string: `=`, `!=`, `IN`, `NOT IN`, `CONTAINS`, `EMPTY`, `NOT EMPTY`
  - bool: `= true`, `= false`
  - timestamp: compared like a number, entered/shown as local time
  - every type: `EMPTY` / `NOT EMPTY` (missing or null counts as empty)
- A **time range**: start defaults to the start of this loghorn run, end
  defaults to "Now". Both are editable. For replay modes (`-historical`,
  `-gh-*`) the default start is the earliest record.
- A free-text **search** box: case-insensitive substring over the raw line,
  ANDed with the filters.
- **Refresh** re-runs the current query against the latest data. There are no
  live updates.
- Results as a **table**: newest first, chosen columns, a row expands to the
  full pretty-printed record.
- **Inferred schemas**: for each stream, loghorn discovers its fields and
  their types from the data.
- **Saved schemas**: the inferred schema can be saved to the repo and edited
  there, to override a field's type and pick its **visualizer** (for example,
  an int64 epoch-millis field shown as local time).
- **Favorite queries**: save, list, load and delete named queries.

## Non-goals (v1)

- Aggregation, group-by and graphs (designed for, not built).
- Live-updating results.
- OR / nested boolean filters.
- Querying across several loghorn processes or worktrees at once.
- An "All streams" choice and cross-stream questions ("what happened around
  11:41?"). Valid and wanted later; deferred.
- A standalone `loghorn -web` server with no TUI (later).
- Auth, TLS, remote access.

## Anti-goals

- **Hiding data.** A saved schema must never make a field or row vanish. A
  field that is in the data but not in the saved schema still shows, marked
  "new". A value that doesn't parse as the schema's type shows raw, marked,
  and is excluded from numeric comparisons rather than coerced to 0.
- **Coupling loghorn to auction.** No auction stream or field names in Go code.
- **Exposing logs to the network.** The logs contain user data.
- **Changing what the TUI does.** `-web` is purely additive.

## Constraints

- Go, single binary, no cgo. The UI is static HTML/CSS/JS embedded with
  `go:embed`; no Node toolchain or frontend build step in this repo.
- Listen on `127.0.0.1` only.
- Data volumes are small: auction writes a few MB per day and `.loghorn/`
  keeps 4 days. A linear scan per query is acceptable; target < 200 ms for
  50k records.
- `.loghorn/` stays gitignored and holds only log data. Schemas and favorites
  are committed in the repo.

## Approaches considered

1. **Go query engine in loghorn + embedded static page** — chosen. One
   binary, reuses the existing adapters that already read both pino-local and
   Cloud Logging shapes; a scan over a few MB is milliseconds. Cost:
   aggregation is ours to write later.
2. **Go + embedded DuckDB**, translating the builder to SQL. Aggregation would
   come nearly free, but it needs cgo, adds ~40 MB, and splits type inference
   between DuckDB and our saved schema — heavy for a few MB of data.
3. **Go JSON API + separate React/Vite app.** Richer widgets, but a second
   toolchain and build inside a Go CLI repo; the UI isn't big enough to
   justify it.

## Design

### Where the data comes from

The web server queries a **session store**: every record this loghorn process
has ingested, kept in memory independently of the TUI's `-scrollback` ring
(5000 by default, too small for queries). Each record keeps its arrival
time (`Received`) and the canonical record (below).

- **Live run:** the store also loads the existing `.loghorn/` files
  (archives oldest first, then the current file) at startup, so a time range
  earlier than this run still finds data. Records from those files are marked
  as before-this-run; the "this run" start is the process start time.
- **`-historical`:** the files it reads are the store.
- **`-gh-job` / `-gh-run`:** the app's labelled lines in the fetched job log
  are the store; GitHub's own lines are not. GitHub Actions job logs normally
  prefix every line with an RFC 3339 timestamp
  (`2026-11-17T18:06:33.1234567Z {"severity":…}`), and nothing in loghorn
  strips it today, so the app's JSON would not be recognised as JSON. The web
  view's ingest strips that prefix (and uses it as the effective time when
  the line has no timestamp of its own). Verified 2026-11-17 on auction run
  44131823601: every line carries the prefix (7 fractional digits, e.g.
  `2026-11-17T17:00:30.3832868Z {"severity":"INFO",…}`), the `integration`
  and `concurrency` jobs carry 200–290 labelled lines each (`db`, `auth`,
  `server`, `external`, `worker`), and the `web` job carries none. Whether the
  TUI should also strip the prefix is a separate change.

Bounding memory: the store holds at most what `.loghorn/` retains (4 days)
plus this run; there is no separate cap in v1.

**Record time.** Only labelled records are in the store, and the time range
and default sort use one *effective time* per record: the log's own
`timestamp`; failing that, the GitHub Actions line prefix; failing that, the
nearest earlier record's effective time (or `Received` in a live run, as
`Received` is not stored in `.loghorn/` files). The UI marks inherited
times. In `-gh-*` and `-historical` modes, "this run" means the whole loaded
log.

### Canonical record

The same log line looks different locally and in Cloud Logging. Locally (pino)
it is flat: `time`, `severity`, `"logging.googleapis.com/labels": {schema}`,
payload fields at the top. In Cloud Logging the agent promotes some of those
into LogEntry fields (`timestamp`, `labels`, `trace`, `httpRequest`, …), adds
others (`receiveTimestamp`, `insertId`, `resource`, `logName`), and moves the
rest under `jsonPayload` (or `json_payload` from gcloud). Schemas and
favorites must work on both, so every record is normalised to one canonical
field set before inference or querying:

- **Promoted fields**, always top level, read from either spelling:
  - `timestamp` ← `timestamp` | `time` (the adapter's existing
    `logEntryTimestamp`)
  - `severity`
  - `labels.*` ← `labels` | `"logging.googleapis.com/labels"`
  - `trace`, `spanId` ← LogEntry fields | `logging.googleapis.com/trace` /
    `logging.googleapis.com/spanId`
  - `httpRequest.*`, `resource.*`, `receiveTimestamp`, `insertId`, `logName`
    when present
- **Payload fields** keep their own path with no `jsonPayload.` prefix:
  `databaseContext.metadata.rowsAffected` is the same field in both sources.
  `message` is a payload field.
- snake_case LogEntry keys from gcloud are mapped to camelCase, as the
  adapter already does (`field()` / `snakeCase`). Application payload keys are
  never renamed.
- Nested objects flatten to dotted paths. Arrays are not flattened: an array
  is a single field of type `json`, shown as JSON and searchable as text.
- The stream is `labels.schema`. A LogEntry with an `httpRequest` object but
  no `labels.schema` (a Cloud Run request log, `run.googleapis.com/requests`,
  which has no payload) goes to the `http-request` stream.

Normalisation is a **separate step** that runs on an `entry.Entry` after the
existing adapters, used only by the web view in v1. The adapters and the TUI
are not changed. It covers JSON LogEntry and gcloud YAML records alike (both
already decode to the same map), and honours the `[input]` settings exactly
as the adapters do: `field-naming = camel` reads only camelCase LogEntry keys,
`auto` reads either. Other records without a stream label are skipped.

### Inferred schema

For each stream, a pass over the store records each field path's observed
value kinds and picks a type:

- `int`, `float`, `bool`, `string`, `json` from the JSON value kinds (an int
  field that ever has a fraction becomes `float`).
- `duration` when every non-empty string value matches a Go-style duration
  like `0.003s` or `12ms` (auction's `latency`). Durations compare as
  seconds.
- `timestamp` when every non-empty value is an RFC 3339 string. `timestamp`
  and `receiveTimestamp` are always `timestamp`.
- Mixed kinds become `string`.
- Numbers are **never** guessed to be timestamps; that takes a saved schema.

Inference also counts how often each field appears, which decides the default
columns: `timestamp`, `severity`, `message`, then the most frequent payload
fields, up to about 8.

### Saved schemas and visualizers

Saved schemas are committed in the repo under `.config/loghorn/schemas/`, one
TOML file per stream (`db.toml`, `api.toml`). Stream names come from data, so
a file name is used only if the stream matches `^[a-z0-9_-]+$`; other streams
can be queried but not saved.
The repo's `.config/loghorn/` **directory** is found by walking up from the
working directory to the git root (inclusive); it counts as found whether or
not a `config.toml` is in it. Unlike the config loader, this walk never falls
back to the user-level `~/.config/loghorn/`, because schemas belong to the
repo. If none is found, saving creates `.config/loghorn/` at the git root (or
the working directory outside git) and says where.

A saved schema lists fields, each with:

- `type` — overrides the inferred type (e.g. `timestamp` for an int64).
- `unit` — for numeric timestamps: `s`, `ms`, `us`, `ns`; for durations the
  display unit.
- `visualizer` — how the value is shown:
  - `raw` (default for strings/json)
  - `number` (thousands separators)
  - `local-time` (timestamps, shown in the browser's local zone, ms precision)
  - `duration` (e.g. `3 ms`, `1.2 s`)
  - `severity` (coloured pill)
  - `sql` (monospace, formatted with the existing `sqlfmt` in the expanded row)
  - `json` (pretty-printed, collapsed in the table)
- `label` — optional display name.
- `hidden` — leaves it out of the default columns (still selectable, still
  filterable; never removed).

The schema also stores the stream's default column list.

"Save schema" in the UI writes the current inferred schema merged with any
existing saved file: saved settings win, newly seen fields are added,
fields no longer seen are kept. The UI lets you change a field's type and
visualizer before saving. Saving overwrites only the stream's file.

The inferred schema is cached per stream and recomputed only when the store
has grown since the last inference.

Types and visualizers are resolved on the server: the API returns typed
values and a visualizer name per column; the page only renders.

### Favorite queries

Committed as `.config/loghorn/queries.toml`, a list of named queries. A query
holds: name, stream, filters, search text, columns, sort, and a time range.
The time range is saved as relative (`this-run`, `last 15m/1h/24h`, `all`) or
absolute when the user chose absolute times. Names are unique; saving an
existing name asks before overwriting.

### Query model

One JSON shape, used by the API, the URL, and saved favorites:

```json
{
  "stream": "db",
  "range": {"from": "this-run", "to": "now"},
  "filters": [
    {"field": "databaseContext.metadata.tableName", "op": "in", "values": ["lots", "bids"]},
    {"field": "databaseContext.metadata.latency", "op": ">", "values": ["50ms"]}
  ],
  "search": "drizzle",
  "columns": ["timestamp", "databaseContext.metadata.operation", "message"],
  "sort": {"field": "timestamp", "desc": true},
  "limit": 1000
}
```

Reserved for later, ignored in v1: `"groupBy": [...]`,
`"aggregates": [{"op": "count"}, {"op": "p95", "field": "..."}]`,
`"bucket": "1m"`, `"view": "table" | "line" | "bar"`. The engine is
structured as filter → (group/aggregate) → sort → limit so these slot in.

The current query is mirrored into the page URL (query string), so a
browser reload or a copied link reproduces it within the same server.

### HTTP API

All JSON, all under `/api/`, all read-only except saving:

- `GET /api/session` — mode (live / historical / gh-job), run start time,
  earliest and latest record time, record count.
- `GET /api/streams` — streams with record counts.
- `GET /api/schema?stream=db` — the effective schema (inferred merged with
  saved) with per-field type, visualizer, presence count, and new/saved flags.
- `POST /api/query` — body is the query model; returns columns (with types
  and visualizers), rows, total matched, and whether the limit truncated.
- `PUT /api/schema?stream=db` — save the schema.
- `GET /api/favorites`, `PUT /api/favorites/{name}`,
  `DELETE /api/favorites/{name}`.
- `GET /` — the embedded page.

Writes are refused unless the request's `Origin` (or `Host`) is the server's
own loopback address, so a malicious page in another tab can't write files.

### The page

One page, top to bottom:

```
 loghorn · live run since 11:02:14 · 4,770 records        [Favorites ▾]
 Stream [db ▾]   From [this run ▾]  To [now ▾]   Search [________]  [Refresh]
 Filters:  tableName IN (lots, bids) ×   latency > 50ms ×   [+ filter]
 Columns:  timestamp · operation · tableName · latency · message   [edit]
 ───────────────────────────────────────────────────────────────────────
 11:41:07.204  SELECT  lots   61 ms  Database: SELECT lots returned 12 rows
 11:41:06.990  UPDATE  bids   73 ms  Database: UPDATE bids affected 1 row
   ▸ (click a row: full record, pretty JSON, SQL formatted)
 ───────────────────────────────────────────────────────────────────────
 312 of 4,012 db records match · showing 312 · [Save as favorite] [Schema…]
```

- "+ filter": field picker (searchable list of the stream's fields with
  their types) → operator list for that type → value input (for `IN`, a
  multi-value input that suggests values seen in the data).
- "Schema…" opens a panel listing every field with its inferred type,
  overrideable type and visualizer, and a Save button.
- Nothing re-queries automatically except when the stream, filters, range or
  search change; Refresh re-runs against new data.

### CLI and TUI

- `-web` turns the server on. `-web-port N` picks the port; the default is
  an OS-assigned free port (several loghorns in several worktrees must not
  collide). The URL is shown in the TUI status bar and printed on exit.
- `-web` works with live input, `-historical`, `-gh-job`, `-gh-run`. It is
  rejected with `-filter` (headless) in v1.
- Help text and README document it.

## Decided by the user

- Scuba-like query builder over stream fields, typed operators; first version
  shows rows; aggregation/graphs later.
- No live updates; a Refresh button.
- Time range defaulting to start of this loghorn run → now; a search box.
- Inferred schemas that can be saved, with a per-field visualizer (e.g.
  int64 → local time).
- Favorite queries.
- Handle GCP's LogEntry reorganisation; promoted fields such as timestamp are
  needed.
- Schemas are attached to the repo; `.loghorn/` holds log data and stays
  gitignored.
- Approach 1 (Go engine + embedded page). Works with `-gh-run` / `-gh-job`
  sources too.
- One stream per query; "All streams" and cross-stream questions deferred.
- Lines without a stream label (GitHub's own lines, plain text) are not in
  the web view; they stay in the TUI.

## Claude's calls (correct any of these)

- Favorites are committed in the repo alongside schemas.
- Server runs inside the live loghorn process; loopback only; random port by
  default.
- Filters AND together; no OR in v1.
- A separate unbounded-by-count session store, preloaded from `.loghorn/` in
  live mode.
- Canonical field names drop the `jsonPayload.` prefix and promote LogEntry
  fields to the top level.
- Arrays are not flattened.
- Numbers are never auto-inferred as timestamps.
- Favorites store relative time ranges by default.
- TOML for saved files, matching the existing config.
- Effective record time = log timestamp, else inherited from the nearest
  earlier record (rules under "Record time").
- Schema/favorites directory walk stops at the git root and never uses the
  user-level config directory.
- Normalisation is a separate web-only step; TUI and adapters are unchanged.
- Normalisation honours `[input]` field-naming/severity like the adapters.
- Inferred schemas are cached until the store grows.
- Strip GitHub Actions' per-line timestamp prefix in the web view's ingest.
- Schema files only for streams named `[a-z0-9_-]+`.

## Left to the builder

- Package layout (e.g. `internal/web`, `internal/query`, `internal/schema`).
- The exact flattening/inference code and how it is shared with the TUI.
- Page styling and the JS structure (vanilla JS or a small vendored library
  embedded in the binary; no CDN at runtime).
- Default result limit and the "truncated" UX.
- Exact TOML layout of schema and favorites files.
