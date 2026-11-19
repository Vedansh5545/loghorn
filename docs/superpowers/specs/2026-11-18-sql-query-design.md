# SQL queries — design

Date: 2026-11-18
Status: draft, partly superseded by `2026-11-19-architecture-working-notes.md`
(read that first). Superseded here: the data source, cache and run-start
sections, the SQLite engine/dialect and type mapping (now DuckDB), and
"the browser keeps the native engine" (it now runs through DuckDB).

## Intent

An LLM investigating a distributed-systems incident should be able to start
from the query a person is looking at in the web view and keep going on its
own: drill down, pivot, join streams on a trace id, compare before and after.
SQL is the query language LLMs know best, so loghorn speaks it.

The usual start: a person copies the web view's URL and pastes it into the
LLM's terminal. The LLM runs it with the loghorn CLI, asks for its SQL, and
from then on writes its own SQL, which can be far richer than anything the
web view's controls express. A person can also paste SQL (or a `?sql=` URL)
into the browser; if the web view's controls can show it, they do.

## Goals

- **Run SQL from the CLI** and print the result to stdout: samples (rows) or
  tables (aggregates), as an aligned text table or `-format json|tsv|csv`.
- **Accept the browser's URL directly.** `loghorn -url '<pasted URL>'` runs
  the query in it, whether the URL holds today's `?q=<json>` or `?sql=`.
- **Show a query's SQL.** `loghorn -to-sql '<URL or SQL>'` prints the
  SQL for a URL's query without running it, so the LLM can start editing.
- **Schema discovery** an LLM can act on: `SHOW DATABASES`, `SHOW TABLES`,
  `DESCRIBE <table>`, `SHOW CREATE TABLE <table>`,
  `SHOW CREATE DATABASE <service>`, with types, units, null rates, distinct
  counts and common values.
- **Browser accepts `?sql=`.** The server converts the SQL to the web view's
  `Query`; if it converts, the controls show it and the native engine runs it
  as today. If it doesn't, the page shows an error naming what blocked it.
- **Stable time during an investigation.** Relative ranges (`1h`, `this-run`)
  work at the start; `-now <RFC 3339>` pins "now" so follow-up queries see the
  same window.
- **Output that is cheap in tokens and honest about what it left out**: row
  and cell limits with explicit markers, a footer with counts and time span,
  and errors that suggest the fix.

## Non-goals (v1)

- Running arbitrary SQL in the browser. The browser only converts SQL to its
  own `Query`; the SQL engine is CLI-only.
- A graph in the CLI output. A graph-shaped query prints its time series as a
  table, which an LLM reads more accurately than a picture.
- The central OTEL directory, per-service saved schemas and explicit schema
  evolution. This design names things so they carry over (see "Databases and
  tables"), but builds none of it.
- Writing through SQL. The engine is read-only.
- The browser writing `?sql=` URLs. It keeps writing `?q=`.

## Anti-goals

- **Hiding data.** A value that doesn't parse as its column's type is NULL
  in that column (so `status >= 500` never matches text), is counted as a
  type violation by `DESCRIBE`, and stays visible in `_raw`.
- **Slowing the browser.** The web view keeps the native engine
  (`query.Run`). SQL never sits on its path.
- **A silent default time window.** A query with no time condition covers all
  stored data, as SQL would; the footer says so.
- **Reading stdin.** loghorn records piped stdin to the project log, so
  `-sql` never reads its query from stdin. Long queries use `-sql-file`.

## Constraints

- Go, single binary, no cgo (unchanged).
- The engine adds no more than ~6 MB to the binary.
- Raises the module's `go` directive to 1.26 (required by the engine).
- Data volume: `.loghorn/` keeps 4 days; the spike used 50k and 500k records.

## Approaches considered

Measured by a benchmark spike (worktree branch
`worktree-agent-a566f5b8576d37f38`, package `internal/sqlspike/`, commit
b9d73a8; Apple M5 Pro). The bar for the browser was ≤ 1.1× native latency.

| Query, 500k records | native | SQLite | GMS, records read in place | GMS, copied into its tables |
|---|---|---|---|---|
| samples | 26 ms | 21 ms (0.8×) | 127 ms (4.8×) | 26 ms (1.0×) |
| table | 93 ms | 187 ms (2.0×) | 122 ms (1.3×) | 91 ms (1.0×) |
| graph | 103 ms | 117 ms (1.14×) | 271 ms (2.6×) | 240 ms (2.3×) |

1. **Hybrid: native engine for the browser, SQLite (`ncruces/go-sqlite3`,
   pure Go) for the CLI** — chosen. Every engine returned the same results
   as native on the three UI queries. SQLite ran all the investigation
   queries (join, before/after, `COUNT(DISTINCT)`, window functions, JSON,
   REGEXP) in 15–400 ms at 500k records. It adds 5.2 MB. Loading 500k
   records takes 0.9 s, hence the cache below.
2. **go-mysql-server for everything** — rejected. It adds 57 MB and 165
   modules. Copying 500k records into its own tables took 289 s. A REGEXP
   query took 248 s. It has no percentile function (the workaround is 21×
   slower than native). One way of bucketing time silently returned the
   wrong number of groups.
3. **SQL engine for everything, browser included** — rejected. No engine met
   1.1× on all three UI query shapes.

The dialect is therefore SQLite's, and loghorn adds the MySQL-style `SHOW`
and `DESCRIBE` statements itself.

## Design

### Where the CLI's data comes from

The CLI reads the `.loghorn/` store under the current directory, the same
files the web view preloads in a live run (archives oldest first, then the
current file), through the same ingest and normalisation, so a record has
the same canonical fields, effective time and stream in both.

It does not contact a running `-web` server; the host and port in a pasted
URL are ignored. The LLM must run the CLI from the directory loghorn ran in.
When `.loghorn/` is missing, the error says so and names the directory
searched.

### The cache

Loading every record into SQLite on each CLI call would cost ~0.9 s at 500k
records, and an investigation runs dozens of queries. The CLI keeps a SQLite
file, `.loghorn/sql-cache.db` (derived data, safe to delete), and brings it
up to date before each query:

- It records, per source file, the identity and the byte offset loaded so
  far. New bytes at the end of a file are appended; a new file is loaded.
- A source file that disappeared, shrank or changed identity, a changed
  schema (column set or types), or a cache written by another loghorn
  version causes a full rebuild.
- Updates run in one transaction under SQLite's own locking, so two CLI
  calls at once are safe.
- With `-now`, records later than the pinned now are still loaded; the
  pinned now only changes how relative times resolve.

Indexes: the spike found that indexes helped some queries (samples 4×, join
30×) and hurt others (graph 2×) as the planner chose them for GROUP BY. v1
creates an index on `_time` only; anything more is a builder call backed by
the spike's benchmarks.

### Databases and tables

The naming is chosen to carry over to the central OTEL directory:

- **Database = service.** A record's database is its `service.name` field
  (OTEL resource attribute) when present. Records without one are in
  `main`, SQLite's default database, so unqualified table names work today.
  Each other service is attached as its own database, so
  `checkout.http_request` works.
- **Table = stream** (`labels.schema`), named verbatim. Streams may contain
  `-` (e.g. `http-request`); the CLI prints such names double-quoted
  everywhere it shows SQL.
- **`all_records`**, a view across every stream of every database with the
  columns every record has (`_time`, `_seq`, `_db`, `_stream`, `_raw`) plus
  `severity`, `message`, `trace_id` and `span_id` where a record has them.
  It answers "what happened around 11:41?" and "everything for this trace".
  In the OTEL directory it becomes the table of OTEL's standard fields.

### Columns and types

Every table has loghorn's columns, prefixed `_` so they never collide with a
field:

| column | meaning |
|---|---|
| `_time` | effective record time (as in the web view) |
| `_time_inherited` | 1 when `_time` came from a neighbouring record |
| `_seq` | record sequence number |
| `_raw` | the raw line, as searched in the web view |

Field columns use the canonical dotted path verbatim, double-quoted:
`"httpRequest.status"`. Types map as follows:

| loghorn type | SQLite column | stored as |
|---|---|---|
| int | `INTEGER` | integer |
| float | `REAL` | real |
| bool | `INTEGER` | 0 / 1 |
| string | `TEXT` | text |
| timestamp | `TEXT` | ISO 8601 UTC, fixed width: `2026-11-18T09:54:00.000000Z` |
| duration | `REAL` | seconds |
| json | `TEXT` | JSON text (use `json_extract`, `json_each`) |

Fixed-width UTC timestamps sort and compare correctly as text, and are what
an LLM expects to read. The spike stored integer microseconds; that was
faster to bucket but an LLM would misuse it.

A field's type is its saved type when the stream has a saved schema, else
its inferred type. A field with mixed types infers to string, as today.

### Functions loghorn registers

| function | meaning |
|---|---|
| `lh_now()` | the pinned now (`-now`), else the time the CLI started |
| `ago(d)` | `lh_now()` minus a Go duration (`'1h'`, `'90m'`) |
| `this_run_start()` | start of the most recent live loghorn run (below) |
| `bucket(t, size)` | start of `t`'s bucket; sizes as in the web view (`1s` … `1d`) |
| `search(text, s)` | the web view's search: case-insensitive substring |
| `percentile(x, p)` | aggregate; matches the web view's p50/p95/p99 exactly |
| `p50(x)`, `p95(x)`, `p99(x)` | shorthands for `percentile` |
| `duration(s)` | Go duration text (`'250ms'`) to seconds, for comparisons |
| `regexp(re, s)` | backs SQLite's `REGEXP` operator |

All return timestamps in the fixed-width text form above. SQLite's own
`datetime('now')` is not redirected, so `-to-sql` and `-sql-help` steer
the LLM to `lh_now()` and `ago()`.

**`this-run` across processes.** In the web view, "this run" is the live
process's start time, which nothing records. The log writer gains a run-start
marker written when a live run opens `.loghorn/`. `this_run_start()` returns
the latest marker at or before `lh_now()`. With no marker, a query using it
fails with "no run start recorded; use a duration such as `ago('1h')`".

### The engine is read-only

User SQL runs on a connection with `PRAGMA query_only` and an authorizer that
denies writes, `ATTACH`/`DETACH`, `PRAGMA` and extension loading. Only one
statement runs per call.

### Query → SQL

One package (e.g. `internal/sqlq`) converts in both directions. Each web view
query shape has one canonical SQL form.

- **Range:** `from` `1h` → `_time >= ago('1h')`; `this-run` →
  `_time >= this_run_start()`; RFC 3339 → a timestamp literal; `all` → no
  condition. `to` `now` → no upper bound; RFC 3339 → `_time <= '…'`.
- **Filters** → `AND`ed conditions using the web view's operators:
  `= != < <= > >=`, `IN`, `NOT IN`, `CONTAINS` → `instr(col, 'v') > 0`,
  `EMPTY` → `(col IS NULL OR col = '')`, and so on. Durations compare via
  `duration('50ms')`.
- **Search** → `search(_raw, '…')`.
- **Samples:** `SELECT <columns> FROM <stream> WHERE … ORDER BY … LIMIT n`.
- **Table:** `GROUP BY` the group-by fields, aggregates as `count(*)`,
  `sum`, `avg`, `min`, `max`, `p50`/`p95`/`p99`, `ORDER BY` the order
  metric, `LIMIT` = Top. SQL has no "other" row; the CLI's table omits it.
- **Graph:** `bucket(_time, '<size>')` plus the group-by fields, restricted to
  the top groups by a fixed CTE shape:

  ```sql
  WITH base AS (SELECT * FROM "http-request" WHERE _time >= ago('1h')),
       top AS (SELECT service FROM base GROUP BY service
               ORDER BY count(*) DESC LIMIT 10)
  SELECT bucket(_time, '1m') AS t, service, count(*) AS count
  FROM base WHERE service IN (SELECT service FROM top)
  GROUP BY t, service ORDER BY t
  ```

  A bucket of `auto` is emitted as the concrete size the native engine would
  choose for the range.
- **Settings SQL can't carry** (view, chart, column order when it differs
  from the select list, `bucket=auto`) go in one trailing comment:
  `-- loghorn: view=graph chart=stacked bucket=auto`. Converting back
  restores them; without the comment they take their defaults, and the view
  follows the SQL's shape.

### SQL → Query (for the browser)

A small hand-written parser for exactly the subset above: one `SELECT` (or
the graph's fixed `WITH` shape), one stream, `AND`ed conditions in the forms
the converter emits, `GROUP BY`, `ORDER BY`, `LIMIT`, and the trailing
`-- loghorn:` comment. It tolerates what an edit naturally changes: keyword
case, whitespace, condition order, alias names, quoting style (`"x"` or
`` `x` ``), and an added or removed condition.

Anything else is rejected with a message naming the first construct that
blocked it: "the web view can't show a JOIN; run it with `loghorn -sql`".
The browser shows that message as an error and leaves its current query in
place.

Round-trip guarantee: for every `Query` the web view can produce,
Query → SQL → Query returns the same `Query`, and SQLite running the SQL
returns the same rows as `query.Run` (excluding the table's "other" row).
Both are tested.

### HTTP API (browser)

- The page reads `?sql=` (alongside `?q=`, which wins if both are present),
  sends it to `POST /api/sql-to-query`, and applies the returned `Query` as
  if it came from `?q=`. On error it shows the message. After applying, it
  rewrites the URL as `?q=` as it does today.
- The endpoint uses the same Host guard and loopback-only listener as the
  rest of the API.

### CLI

New flags; each is mutually exclusive with live input, `-- <cmd>`, `-web`,
`-historical`, `-gh-job` and `-gh-run`, checked before anything starts (see
CLAUDE.md, "Startup ordering"):

| flag | does |
|---|---|
| `-sql '<SQL>'` | runs SQL (or a `SHOW`/`DESCRIBE` statement) |
| `-sql-file <path>` | the same, reading the SQL from a file |
| `-url '<URL>'` | runs the query in a web view URL (`?q=` or `?sql=`) |
| `-to-sql '<URL>'` | prints the SQL for a URL's query; runs nothing |
| `-now <RFC 3339>` | pins `lh_now()` |
| `-format text\|json\|tsv\|csv` | output format; default `text` |
| `-max-rows N` | row cap when the SQL has no `LIMIT`; default 200 |
| `-max-cell N` | cell width cap in `text`; default 200 characters |
| `-sql-help` | prints the LLM guide (below) |

**Output.** `text` is an aligned table. A cut cell ends with
`…[+N chars]`. The footer:

```
200 of 12,340 rows (raise -max-rows or add LIMIT)
_time 2026-11-18T08:54:00Z .. 2026-11-18T09:53:58Z
now 2026-11-18T09:54:00Z (pin with -now 2026-11-18T09:54:00Z)
```

The time-span line covers the rows returned when the result has a `_time`
or bucket column. When the SQL references none of `_time`, `ago(`,
`this_run_start(`, the footer adds "no time condition: covers all stored
data, 2026-11-15T08:02Z .. 2026-11-18T09:57Z". `json` puts the same facts in
a `meta` object; `tsv`/`csv` print them to stderr.

The total row count comes from re-running the query as
`SELECT count(*) FROM (<sql>)` only when the cap was hit.

**Errors suggest the fix.** "no such column: http.status" becomes
`unknown column "http.status" in "http-request"; did you mean
"httpRequest.status"?` (closest names by edit distance), and likewise for
tables and functions. Exit status is non-zero on any error.

### Discovery statements

loghorn handles these itself; they never reach SQLite.

- `SHOW DATABASES` — each database (service) with its streams and rows.
- `SHOW TABLES [FROM db]` — each table with row count and `_time` span.
- `DESCRIBE [db.]table` — one row per column: name, loghorn type, SQLite
  type, unit, declared or inferred, null rate, distinct count, up to five
  common values with their share (`ERROR 12%, WARN 30%`), and the type
  violation count. Distinct counts and common values may be computed over a
  sample at large row counts; the output says when.
- `SHOW CREATE TABLE [db.]table` — the `CREATE TABLE` with a comment per
  column carrying type, unit and declared/inferred.
- `SHOW CREATE DATABASE db` — every table's `CREATE TABLE` in that database.

### The LLM guide (`-sql-help`)

One page an LLM reads once: the database/table/column conventions, the `_`
columns, timestamp and duration representation, loghorn's functions, the
time-pinning workflow (start relative, then pass `-now`), the output footer,
and three worked examples (follow a trace across streams, error rate before
vs after a time, slowest paths by p95). README links it.

## Decided by the user

- SQL is the LLM's interface; the CLI is its main surface. People paste the
  browser URL into the LLM's terminal, so the CLI takes URLs directly.
- The LLM's job is to investigate an incident from the query, not to hand
  back a URL; it can ask for a query's SQL.
- The browser only converts SQL to its own query, and shows an error when it
  can't. No SQL engine in the browser.
- The browser must stay within ~1.1× of today's latency; 2× is too slow.
- Relative times at the start of an investigation; held fixed after.
- A query with no time condition covers all data.
- Database = `service.name`; saved schemas move to per-service later.
- Schema changes need explicit evolution (later, with the OTEL directory).
- A benchmark spike before choosing an engine; it chose SQLite.

## Claude's calls (correct any of these)

- SQLite's dialect, with loghorn's own `SHOW`/`DESCRIBE`.
- Timestamps as fixed-width ISO 8601 UTC text; durations as seconds.
- A persistent, incrementally updated cache at `.loghorn/sql-cache.db`.
- The CLI reads the current directory's `.loghorn/`, not the server in the
  URL.
- Records without `service.name` live in `main`.
- Stream names used verbatim as table names, quoted when needed.
- An `all_records` view across streams.
- `_`-prefixed loghorn columns.
- Values that don't parse as the column's type are NULL, counted, and kept
  in `_raw`.
- A run-start marker in `.loghorn/` so `this-run` works from the CLI.
- A hand-written parser for the browser subset instead of a SQL parser
  dependency.
- `-to-sql` emits a concrete bucket size, keeping `auto` in the comment.
- SQL has no "other" row.
- Default caps: 200 rows, 200 characters per cell.
- `-sql-file` instead of reading SQL from stdin.
- `?q=` wins over `?sql=` when a URL has both.

## Left to the builder

- Package layout and the cache's bookkeeping tables.
- Indexes beyond `_time`, justified by the spike's benchmarks.
- How `DESCRIBE` samples at large row counts, within ~2 s at 500k rows.
- Where `trace_id`, `span_id`, `severity` and `message` come from for
  `all_records` in today's (non-OTEL) logs.
- The exact `-sql-help` text.
- Performance targets: a warm CLI query within the spike's SQLite numbers; a
  cold cache build ≤ 2 s at 500k records; an incremental update of a few
  hundred records < 100 ms.
