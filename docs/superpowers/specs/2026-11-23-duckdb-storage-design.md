# DuckDB storage and SQL queries for the web view — design

Date: 2026-11-23
Status: approved 2026-11-23 (D-011); refined by D-014..D-020
Decisions: `docs/DECISIONS.md` D-001 to D-010 (this document is where they
become final)

## Intent

The web view (`loghorn -web`) keeps every record of the session in an
in-memory list (`internal/store`) and answers each request by scanning it in
Go (`internal/query`, `internal/trace`). Every run also re-parses up to three
days of `.loghorn/` text files into that list. The author measured the cost
at the app's scale: a Table query takes 716 ms and a Graph query 520 ms,
against about 5 ms for DuckDB, and each run holds about 3 GB of RAM
(architecture notes, spike 2).

This change stores logs and traces in DuckDB and runs the web view's
requests as SQL, so filtering and querying feel instant, Scuba style. Raw
lines stay available so structure can be pulled out later.

It follows the author's decisions in
`2026-11-19-architecture-working-notes.md`: DuckDB is the only engine (#5),
the browser's queries run through DuckDB and `query.Run` is retired (#35),
database = service and table = stream (#16), and bad values are never hidden
(#21).

## Goals

- `-web` answers `/api/query` (Samples, Table, Graph, Heatmap), `/api/kinds`,
  `/api/streams`, `/api/schema`, `/api/record`, `/api/trace` and
  `/api/aggregate` from DuckDB, with the **same JSON responses as today**.
- Logs are stored in one DuckDB file per day, spans in another.
- Inside each file: a schema per service, a table per stream, a typed column
  per field.
- One shared set of views (`all_records` plus a view per stream) that every
  request reads.
- A live run with `-web` starts without re-parsing the stored days.
- Browser views stay under ~50 ms at the app's scale, as the notes set (#33).
  Full-text search may stay at ~100–300 ms (#36a).

## Non-goals

- `loghorn serve`, a central store, or several processes sharing one store.
  Files stay per repo (D-006).
- Parquet archives (notes #39). The per-day files leave room for them.
- The SQL CLI (`-sql`, `-url`, `-to-sql`, `SHOW`/`DESCRIBE`). Its converter
  is reused later, but no CLI flags are added here.
- Metrics. They stay accepted and discarded.
- Changing which records reach the store. Log lines with neither
  `labels.schema` nor `httpRequest` are still dropped (notes #36b leaves
  this open).
- Any change to ingestion: `internal/ingest`, `internal/otlp`,
  `internal/recorder` and the `internal/logfile` writer.
- Changing the web page (`internal/web/static`). It must not notice.

## Anti-goals

- **Different answers.** A query must return the same rows, groups, counts,
  percentiles and buckets as `query.Run` returns today, apart from the
  listed "Known differences". Faster but different is a failure.
- **Hiding data.** A value that doesn't fit its column is NULL there, still
  counted, and still visible in `_raw` (notes #21).
- **Slowing the wrapped command.** Writing to DuckDB never blocks the child's
  output or the OTLP receiver.
- **Losing data when DuckDB fails.** The text files are still written and
  remain the source of truth (D-007). A DuckDB failure is reported once, and
  the run goes on.

## Constraints

- Go, cgo allowed (notes #2). `go.mod` is at Go 1.26.1.
- DuckDB lets one process write a file. Others may open it read-only only
  while no process has it open for writing.
- Spike 2's gotchas apply: compress long text columns with zstd, keep
  appender flushes off the request path, and quote dotted column names.
- Spike 3's gotcha applies: under `UNION ALL BY NAME`, a column whose type
  differs between files becomes VARCHAR and breaks aggregates.
- Retention is today's `keepDays = 3`.
- Query times use `time.Local`, as `query.Run` does today (graph buckets and
  heatmap columns align to local time).

## Approaches

**Table layout** (user picked B, D-002):

| | Layout | Gain | Cost |
|---|---|---|---|
| A | One table per signal, attributes in one JSON column | Fixed shape; "All kinds" and trace lookups are a plain WHERE | JSON parsed on every filter; speed not measured |
| **B** | A table per stream, a typed column per field | Fastest filters; measured by the author at 1–17 ms on ~1 GB | Tables and columns appear as data arrives; views must be generated; type changes need rules |
| C | A plus promoted typed columns per stream | Middle ground | Most moving parts |

**Scope** (user picked one design, D-010): storage and SQL queries together,
so the speed arrives in one step. The alternative was storage first, then
queries in a second design: each half testable sooner, but no speed gain
until the second.

**How queries reach DuckDB.** There is one sensible way: a Go package turns
the web view's `query.Query` into SQL, as the author's SQL spec
(`2026-11-18-sql-query-design.md`, "Query → SQL") already designs. Loading
DuckDB rows back into Go and keeping `query.Run` would gain no speed.
Running SQL written in the browser is a non-goal (notes #32).

## Design

### Files and who writes them

- `.loghorn/logs-YYYY-MM-DD.duckdb` and `.loghorn/traces-YYYY-MM-DD.duckdb`,
  one pair per local day, mode `0o600` like the text files.
- **The writer** is the process that holds `loghorn.lock`, with or without
  `-web`, so a later `-web` finds the days already stored. A process without
  the lock writes no DuckDB file, as it writes no text file today.
- A record goes into the pair for the day it **arrived**, matching the text
  log's rotation. Its `_time` may be earlier. Queries read every retained
  day, so placement doesn't change answers.
- **At midnight** the writer closes yesterday's pair and opens today's.
- **Expiry** deletes whole pairs older than `keepDays`, at startup and at
  each rollover. DuckDB files don't shrink after DELETE, so rows are never
  deleted (notes #39).
- **Upgrade.** A retained day with text files but no DuckDB pair is built
  from its text files by the writer at startup, in the background. Until it
  is done, that day is read from text into memory (below).

### What each mode reads

| Mode | Reads |
|---|---|
| Live with `-web`, holding the lock | Its own open files for today, plus earlier retained days attached read-only |
| Live with `-web`, lock held elsewhere | Earlier retained days attached read-only. Today is loaded from today's text files into an in-memory database, plus this run's own records |
| `-web` alone (stored days) | Each retained day attached read-only. A day that can't be opened (held by a writer, or not built yet) is loaded from its text files into memory |
| `< file`, `-gh-job`, `-gh-run` | An in-memory database filled from the input; no files |

All modes use the same table, column and view code. Only where the tables
live differs. In-memory loading reuses today's parse path
(`store.Preload` → `record.Normalize`/`NormalizeSpan`).

### Writing records

- Records enter where `store.Add` is called today: `recorder.go:90`,
  `wrapper.go:184` (spans) and `store.Preload`. `Store` keeps its method
  names (`Add`, `Snapshot` goes, see below), so callers change little.
- `Add` does today's work (service fallback, inherited time, backfilling
  leading records with no time) and puts the record on a queue. A background
  goroutine appends queued records in batches with DuckDB's appender. Nothing
  waits on it.
- **Record id.** Today's `Seq` is per session. It becomes `_id BIGINT` =
  (local day number since 1970-01-01) × 2³² + sequence within that day. Logs
  and spans of a day share one sequence. It is unique across retained days,
  stable across restarts, and below 2⁵³, so the page can keep it in a
  JavaScript number. The JSON field stays `seq`.
- **New stream** → `CREATE TABLE` in that service's schema. **New field** →
  `ALTER TABLE ADD COLUMN`. Both happen in the writer, before the batch that
  needs them.
- **Column type = what `schema.Infer` would say** over every value the table
  has seen for that path. The writer keeps Infer's per-path tallies (below),
  so the rule is exactly Infer's: one kind gives that type, int with float
  gives float, any other mix gives string, and `timestamp` and
  `receiveTimestamp` are always timestamp. Once string, a column stays string.
- **Type change.** When a batch changes a column's type, the writer changes
  the column and rewrites that column's existing values by running each
  row's `_raw` back through the normaliser. So a widened column holds the
  values as received (`1.50`, not `1.5`), not DuckDB's casts. This is rare:
  at most a few times per field.
- **Stored value = `schema.Convert(value, field with the column's type)`.**
  A value Convert rejects (e.g. a number in a forced timestamp column that
  isn't an epoch) is NULL in the column, and its path is added to the row's
  `_bad` list. So `empty` and `not-empty` filters can still tell a bad value
  from a missing one, as today. The value stays in `_raw`.
- **Record ordering within a time.** Records wait in the queue while their
  time is still unknown, then go out with the backfilled time, so a stored
  `_time` never changes later.
- **The queue is bounded.** If it fills, records are skipped for DuckDB only
  (never for the text files), the day is marked dirty (below), and loghorn
  prints one warning to stderr.

### Field tallies and table metadata

Each day file has two small tables the writer maintains:

- `_fields`: one row per (service, source, displayed kind, path) with
  Infer's tally (the count of records holding the path, nulls included, and
  the set of kinds seen) and the column it lives in. `/api/schema` merges
  these tallies across the attached days and services and runs Infer's
  `typ()` on them, so it returns exactly today's schema, per displayed kind,
  including kinds that share the catch-all `span` table.
- `_day`: the day, whether the file was closed cleanly, whether any write
  was skipped or failed, and the time of the last commit.

**Rebuild on doubt.** At startup the writer rebuilds a retained day from its
text files when the day has text files but no pair, wasn't closed cleanly
(a crash), had a skipped or failed write, or has a text file modified after
the pair's last commit (e.g. an older loghorn wrote text only). The rebuild
writes a new pair under a temporary name, then renames it over the old one.
Until it finishes, that day is read from text into memory.

### Tables

Names: schema = service key (`record.ServiceKey`); table = stream name. Span
tables are named by span type (`record.SpanStream`: `db`, `http.server`, …,
catch-all `span`); the displayed kind (the span name for unclassified spans)
is the `_stream` column (D-003).

**Names that collide.** DuckDB compares identifiers without regard to case,
even when quoted. So when a service, stream or field name differs from an
existing one only in case (`userId` and `userID`, `API` and `api`), the newer
one gets a suffix (`userID~2`). The mapping from name to table or column is
kept in `_fields` and is the only way code finds a column. Data is never
merged across names that differ in case.

Every table, logs and spans, starts with the shared columns. All are
`_`-prefixed so no field (such as a Docker-style `stream`) can collide:

| column | type | from `record.Record` |
|---|---|---|
| `_id` | BIGINT | `Seq` (above) |
| `_time` | TIMESTAMP_NS (UTC) | `Time`, keeping today's nanoseconds |
| `_time_inherited` | BOOLEAN | `TimeInherited` |
| `_source` | VARCHAR | `Source` (`log` or `otel`) |
| `_service` | VARCHAR | `Service` |
| `_stream` | VARCHAR | `Stream` |

then one column per field path in `Fields`, then:

| column | type | from |
|---|---|---|
| `_bad` | VARCHAR[] | paths whose value didn't fit their column (above) |
| `_raw` | VARCHAR, zstd | `Raw` |

Field types map from loghorn's schema types:

| loghorn type | DuckDB column |
|---|---|
| int | BIGINT |
| float | DOUBLE |
| bool | BOOLEAN |
| string | VARCHAR (zstd for long text) |
| timestamp | TIMESTAMP_NS (UTC) |
| duration | VARCHAR holding the value as received, as text (`250ms`, or a number such as `0.25`) |
| json | JSON |

Durations stay text because `schema.Convert` parses them at query time with
the field's unit, and the saved unit can change. Fields already present on
spans (`trace_id`, `span_id`, `parent_span_id`, `duration_ms`, `status`,
`events`, `links`, …) are ordinary field columns; nothing is renamed.

### Views: the shared format

loghorn generates views in the session's own in-memory catalog, over every
attached day:

- **One view per stream**, named `"<source>/<service>/<stream>"`: `UNION ALL`
  of that table across days, with an **explicit column list**, each column
  cast to its widest type across days (never `BY NAME` inference, per spike
  3). A column missing from a day is `NULL` there.
- **`all_records`**: `UNION ALL` of every stream view, with only the shared
  columns plus `trace_id`, `span_id`, `parent_span_id`, `severity` and
  `message` where a table has them (NULL otherwise).
- Views are rebuilt when a table or column appears, or a day is attached or
  detached. A request uses the views as they were when it started.

The web view reads only these views. `Snapshot()` is removed.

**What SQL decides, and what Go shows.** SQL decides which rows match, their
order, groups and metrics, using the typed columns. Everything shown to the
user is built from the original record: for each returned row, Go fetches
`_id`, `_time`, `_time_inherited`, `_service` and `_raw` and runs `_raw`
through `record.Normalize`/`NormalizeSpan`, which gives exactly the record
stored today. Samples cells (`cell()`), `/api/record`, `/api/trace` and
`/api/aggregate` use these rebuilt records, so what users read is identical
to today's. Samples return at most `MaxLimit` rows, so the cost is bounded.

### Queries

A new package (suggested name `internal/sqlq`) turns a validated
`query.Query` plus its effective `schema.Schema` into one SQL statement and
turns the rows back into `query.Result`. It follows the author's SQL spec,
"Query → SQL", where that spec covers a view, and extends it as follows.

| Request | SQL | Kept in Go |
|---|---|---|
| Range | `_time >= from AND _time <= to`, inclusive like `query.Run`. A zero `from` keeps rows with no time | `Range.Resolve` |
| Stream, source, service | Choose the stream view, or `all_records` filtered by `_source` for `*` (All kinds), plus `_service = ?` unless `*` | — |
| Filters | One condition per filter, using the effective type (below) | `Validate` |
| Search | Case-insensitive substring of `_raw` | — |
| Heatmap cell | The cell's time and duration bounds, half-open as `HeatCell.contains` | — |
| Samples | `ORDER BY` sort field then `_id`, `LIMIT`/`OFFSET`; default columns = fields with a value in the matching rows | Building `Cell`s with today's `cell()` from the rebuilt records, SQL tokens |
| Table | `GROUP BY`, metrics, `Order`, `Top` | The "other" row and ranking ties, if SQL can't express them exactly |
| Graph | Buckets from `chooseBucket`/`xAxis`, top groups by `Order`, at most `MaxGraphTop` | Bucket choice, filling empty buckets |
| Heatmap | Counts by time column and decade band from `bands()` | Band and column edges |
| Kinds | Counts by `_stream` and by `_service` over `all_records`, half-open range as `Store.Kinds` | — |

- **Effective types.** A field's effective type comes from the saved field
  settings (`schema.Effective`), and may differ from the stored column type.
  When they match, SQL uses the column directly. When they differ, SQL uses
  a conversion expression that gives the same value as `schema.Convert`,
  including units and epoch timestamps. A value that fails to convert never
  matches a filter and is skipped by aggregates, as today.
- **Metrics** must equal today's: `p50`/`p90`/`p95`/`p99` are
  **nearest-rank** (`query.percentile`), and `error_rate` uses
  `isErrorSeverity`.
- **Schema inference** (`/api/schema`, default columns) merges the
  `_fields` tallies instead of scanning records, and returns the same
  `schema.Schema` that `schema.Infer` returns today. It is cached per view
  version, as now.
- **A bad value** (path in `_bad`) is not empty: it matches `not-empty` and
  no other filter, as `Convert`'s `Bad` does today.
- **Trace and aggregate.** `/api/trace` fetches every row of `all_records`
  with the trace id (`span_id` first, for `trace.Find`), rebuilds the
  records, and calls `trace.Build` unchanged. `/api/aggregate` fetches the
  root spans with that name in range, then every row of their traces, and
  calls `trace.Aggregate` unchanged.
- **`query.Run` stays as a test oracle** until the parity tests pass, then
  is deleted with the Go-only parts of `internal/query` that nothing else
  uses (notes #35).

### Testing

- **Parity.** One fixture set (`testdata/web` plus a generated set with
  several services, streams, types, inherited times, durations with units,
  spans with parents and events) is loaded both into today's store and into
  DuckDB. Every `/api/*` response is compared. Cases: each view, each filter
  op on each type, saved type overrides, search, All kinds, every service vs
  one, Offset paging, Order/Top/"other", every bucket size including auto,
  heatmap with and without a cell, and empty results.
- **Storage.** New stream, new field, widening int → float → string across
  batches and across days, bad values NULL with `_raw` kept, quoted dotted
  names, inherited-time backfill, midnight rollover, expiry, the upgrade
  build from text.
- **Modes.** Each row of "What each mode reads", including a second live run
  in the same repo and a stored day held open by another process.
- **Failure.** A DuckDB write failure is reported once; text files and the
  child carry on; the day is marked for rebuild; the web view keeps
  answering, with records from then on held in an in-memory database (the
  same code as "lock held elsewhere").
- **Rebuild on doubt.** Each trigger in "Field tallies and table metadata"
  causes a rebuild, and a rebuilt day answers like a day loaded from text.
- **Comparing numbers.** `sum` and `avg` are compared with a relative
  tolerance of 1e-9, because DuckDB adds in parallel and `query.Run` adds in
  record order. Everything else is compared exactly.
- **Speed.** A benchmark over the spike 2 dataset shape (≈770k records, one
  day; and 3 days), checked against the ~50 ms target.

## Decided by the user

- DuckDB for logs and traces, in two files; metrics ignored (D-001).
- Layout B: a schema per service, a table per stream, typed columns (D-002).
- One design for storage and SQL queries (D-010).
- Accepted in conversation (provisional until this doc is approved): span
  tables by span type with the name as a column (D-003); generated views as
  the shared format (D-004); per-repo, per-day files written by the lock
  holder (D-006); text files stay the source of truth (D-007); batched
  background writes (D-008).

From the author's notes: DuckDB only (#5), browser via DuckDB with
`query.Run` retired (#35), database = service (#16), schema evolution and
bad values (#19, #21, D-005).

## Claude's calls (correct any of these)

- The lock holder writes DuckDB even without `-web`.
- Records go into the day they arrived, not the day of `_time`.
- A run that can't open a day's file reads that day from its text files into
  memory.
- `_id` = day number × 2³² + sequence, shared by logs and spans.
- Durations stored as text, converted at query time.
- Column types follow `schema.Infer` exactly; a type change rewrites the
  column from `_raw`.
- Bad values are NULL plus a `_bad` entry, so they stay distinguishable from
  empty ones.
- `_time` and timestamp columns are nanosecond `TIMESTAMP_NS`, as today's
  records are.
- Every shared column is `_`-prefixed (`_stream`, not `stream`).
- Names that differ only in case get a `~2` suffix, never merged.
- A `_fields` tally table per day gives exact `/api/schema` answers.
- Days are rebuilt from text whenever their DuckDB pair is in doubt.
- The writer's queue is bounded; overflow skips DuckDB only and marks the
  day for rebuild.
- After a DuckDB failure, the session keeps answering with an in-memory tail.
- Views live in the session's in-memory catalog, not in the day files.
- What users read is built in Go from `_raw`; SQL only selects, orders and
  aggregates.
- `sum` and `avg` compared with a 1e-9 relative tolerance.
- `query.Run` kept as the test oracle until parity passes.

## Left to the builder

- Package layout, and whether `internal/store` is rewritten in place or
  replaced.
- The Go DuckDB driver and appender use, batch size and flush interval
  (must keep requests off the flush path).
- How the writer and request goroutines share the DuckDB instance.
- Exact SQL for each view shape, conversion expressions, and the
  nearest-rank percentile.
- How identifiers are quoted and escaped.
- Whether zstd applies per column or database-wide.
- How the "other" row and graph tie-breaking are computed, provided the
  parity tests pass.
- Error messages.

## Known differences from today (awaiting the user's answer)

What users read is identical to today's. These are the cases where *which*
rows match, or a group's value, can still differ from `query.Run`:

1. **A saved type override that needs the original text.** If a user changes
   a number field to string in the schema editor, filters compare DuckDB's
   text for the number (`1.5`), where today they compare the text as
   received (`1.50`). The same applies to timestamp → string, and to
   float → int, where today `2.0` is bad but DuckDB reads it as 2.
2. **A field present but null.** Today the Samples default columns count a
   field that is present with a null value. In DuckDB, null and absent look
   the same per row. The whole-stream schema (`_fields`) still counts it
   exactly; only the per-interval default column list can differ.
3. **Days rebuilt from text.** A live run gives records with no service the
   child's `OTEL_SERVICE_NAME`, and records with no time their arrival time.
   A day rebuilt from text gets `default` and the previous record's time
   instead, as `-web` reloads do today. So a day can look slightly different
   after a crash or upgrade than it did live.
4. **The last digit of `sum` and `avg`**, from adding in a different order.

The alternative to 1 and 2 is storing a text copy of every field next to its
typed column. That costs roughly double the storage and slower writes.

## Open risks

- **Reopening after a crash.** A day file left with a pending write-ahead log
  may not open read-only. The fallback to text covers it, but the writer must
  recover it on its next start.
- **DST.** Local-time buckets across a daylight-saving change must match
  `query.Run`. A parity case should cross one.
- **Many tables.** Span types are a fixed list, but log streams are as many
  as the app emits. View rebuild time with hundreds of tables is unmeasured.
- **Author review.** The author hasn't seen this design (`docs/DECISIONS.md`
  O-2). Splitting the files by signal is the one point not in his notes.
