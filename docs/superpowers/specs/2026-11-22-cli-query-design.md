# CLI query: export a web view from the command line

## Intent

A loghorn page URL already names a complete query. The user wants to hand that
URL to the `loghorn` binary and get the view's data as CSV, TSV or Markdown,
with no setup, so the data can go to a shell pipeline, a file or an LLM. The
same command with `-to-sql` prints the query as SQL an LLM can read and edit.
The SQL is meant for the planned DuckDB engine; until that exists nothing runs
it, but its shape and table model are fixed now.

A second, smaller goal is finding a running instance without knowing its port:
every `-web` instance also listens on a per-user unix socket, and the CLI can
use it when no URL is given.

## Goals

- `loghorn http://127.0.0.1:60000/?q=…` prints the view's data (TSV by
  default) and exits 0. The positional form is recognised when `args[0]`
  starts with `http://127.0.0.1` or `http://localhost` (with a port or not).
- `-url URL` does the same explicitly, and also accepts a URL with no host
  (`/?q=…`), which goes through instance discovery. `-url @file` reads the
  URL from a file and `-url @-` from stdin (surrounding whitespace trimmed),
  for long `?q=` URLs that are awkward to quote.
- `-o FILE` writes to a file, with the format taken from its extension
  (`.csv`, `.tsv`, `.md`, `.json`, `.jsonl`, `.yaml`/`.yml`, `.toon`;
  `.sql` with `-to-sql`). `-format` overrides the extension. Without `-o`, output goes to stdout.
- `-to-sql` prints DuckDB SQL equivalent to the query instead of data.
- Every view the URL can name works: Samples, Table, Graph, Heatmap.
- The page's copy bars become one "Copy" button with a format droplist, and
  the main views copy exactly what the CLI prints.
- Computed numbers carry no binary-float noise (`23.84`, not
  `23.839999999999996`).
- Every `-web` instance listens on `$TMPDIR/loghorn/<pid>.sock` too;
  `-instances` lists the live ones.

## Non-goals

- No `-exec` or `-run` flag for the command side; `--` already serves.
- No query language on the command line beyond the URL's `q` JSON.
- The trace and aggregate views (`/api/trace`, `/api/aggregate`) are not
  exported; their crumbs in the URL are ignored with a note on stderr.
- No SQL execution and no SQL input (`-sql`) in this work; the agreed
  syntax is recorded under Future.
- The CLI never reads `.loghorn/` itself: spans received through `-otel` are
  in the instance's memory before they are on disk.

## Anti-goals

- Exporting must not change the instance: no writes to pins, favorites or
  schema, and no store mutation.
- The socket must not widen access: only the owning user may connect.
- A query must never launch a child process. A URL-looking `args[0]` is never
  treated as a command, and a query flag combined with `--` command args is a
  usage error (exit 2) before anything starts (CLAUDE.md "Startup ordering").

## Approaches

- **A. Server-side formatting (chosen).** `POST /api/query` accepts a
  `format` (`csv`, `tsv`, `md`, `json`, `jsonl`, `yaml`, `toon` or `sql`)
  and answers with that text instead
  of JSON. The CLI is a thin client that streams the body. The SQL needs the
  effective schema (inferred types plus saved settings), which only the
  server holds; curl and a later page "Download" button get the same output.
  Cost: the server grows a little.
- **B. Client-side formatting.** The CLI fetches JSON and `/api/schema` and
  formats locally. Server only gains the socket. Two requests, and the SQL
  generator would see the saved schema rather than the effective one.
  Rejected for that reason.
- **C. CLI reads the stored files directly.** Rejected: misses live
  in-memory data, and the user wants the running instance.

## Design

### Command line

| Invocation | Meaning |
|---|---|
| `loghorn http://127.0.0.1:P/?q=…` | query that instance over TCP |
| `loghorn -url http://localhost:P/?q=…` | same |
| `loghorn -url '/?q=…'` | query through the socket of the single live instance |
| `loghorn -instances` | list live instances (pid, port, cwd, start time) |
| add `-o out.csv` / `-format md` | output file / format |
| add `-to-sql` | print SQL instead of data |

- A URL host other than `127.0.0.1`/`localhost` is refused (loghorn only
  serves loopback).
- `q` is decoded exactly as the page decodes it (`app.js` reads the same
  parameter). Unknown fields are ignored, as the page does.
- Query mode conflicts with `-web`, `-otel`, `-historical`, `-gh-job`,
  `-gh-run` and a command: exit 2 with a message. Query mode never reads
  stdin and ignores it, whatever it is: agent shells, CI, `xargs` and
  `while read` loops run with stdin as a pipe or file, and the piped-input
  refusal exists only because piped input gets recorded, which a query
  never does. (Builder's-check call.)
- Errors from the server (4xx/5xx JSON `{"error":…}`) print as
  `loghorn: <message>` on stderr, exit 1. An unreachable instance says so and
  suggests `-instances`.
- Discovery with zero live sockets: error. With several: error that lists
  them (same table as `-instances`) and says to pass the full URL.

### Server: `format` on `/api/query`

`query.Query` gains `Format string` (`json:"format,omitempty" toml:"-"`; not
saved with a favorite). Empty means JSON as today. Otherwise the handler runs
the query as today and writes text with the format's `Content-Type`
(`text/csv`, `text/tab-separated-values`, `text/markdown`,
`application/json`, `application/jsonl`, `application/yaml`, `text/plain`
for TOON, `application/sql`; all `charset=utf-8`). `sql` also runs the
query (see SQL below).

The formats fall in two families:

- **Flat:** `csv`, `tsv`, `md`, `jsonl`. One row (one JSON object per line
  for `jsonl`) per record, group or bucket, in the shapes below.
- **Document:** `json`, `yaml`, `toon`. A `columns` section, then the data,
  nested where the view groups it (see Document shapes).

Shared rules:

- Column names are the field paths (stable, match the SQL), not labels.
- Values are raw: numbers as JSON prints them, times RFC 3339 with
  nanoseconds in UTC, booleans `true`/`false`. A field of type JSON is real
  nested structure in `jsonl` and the document formats, compact JSON text in
  `csv`/`tsv`/`md`.
- Null is an empty cell in `csv`/`tsv`/`md`, `null` elsewhere (TOON: `null`).
- `NaN` and `Infinity` are DOUBLE values (DuckDB holds them). Where the format
  has no such number (JSON, JSONL, TOON) they are written as the strings
  `"NaN"`, `"Infinity"`, `"-Infinity"`; YAML uses `.nan`, `.inf`, `-.inf`.
- A value that does not fit its column's type (the Go side's "bad" value)
  is exported as null, as the SQL table model loads it. The original text
  stays in the record's `raw`.
- CSV: RFC 4180 via `encoding/csv`.
- TSV: a tab, newline, carriage return or backslash in a value is written as
  `\t`, `\n`, `\r`, `\\`.
- Markdown: a GitHub table; `|` escaped as `\|`, newlines as `<br>`.
- When the result is truncated (`Truncated`, or Samples hit the limit),
  stderr — not the data — gets a note with matched vs shown counts. The CLI
  prints it; the server sends it in an `X-Loghorn-Note` header.

Shapes per view:

- **Samples:** one row per record, the result's columns.
- **Table:** one row per group: group-by columns, then `count`, then each
  aggregate as `op(field)` (`p95(duration_ms)`, `error_rate`).
- **Graph:** wide. One row per time bucket; first column `time` (bucket
  start), then one column per series named by its label (`GET /a · 200`,
  `other`). With no group-by the single column is the plot metric's name
  (`count`, `p95(duration_ms)`). A bucket with no data is an empty cell.
  The series order is the result's order.
- **Heatmap:** wide. One row per time bucket; `time`, then one column per
  duration band named `lo-hi ms` (lowest band `<hi ms`). Cells are counts.

### Column descriptions (document formats)

`columns` lists every column of the data, in order, each with:

- `name`: the field path, or the metric name (`count`, `p95(duration_ms)`).
- `type`: the DuckDB type of the SQL table model (`VARCHAR`, `BIGINT`,
  `DOUBLE`, `BOOLEAN`, `TIMESTAMPTZ`, `JSON`), so what the export declares is
  what an LLM meets in SQL. A metric's type is its SQL result type (`count`
  is `BIGINT`, `avg` and pNN of a `BIGINT` are `DOUBLE`, `error_rate` is
  `DOUBLE`).
- `unit`: when known. A duration is `DOUBLE` with `unit: ms` (the SQL model
  stores durations in ms); other units come from the field's schema unit.
  Omitted when unknown.
- `nullable`: true when this result has a null in the column.
- `mismatched`: the number of values exported as null because they did not
  fit the type; omitted when 0, so a replacement is never silent.
- `otel_name`: the original OpenTelemetry attribute key, when the field
  came from OTLP and loghorn's path differs from it (an attribute moved
  under `attributes.` because it collided with a span field, or a key
  rewritten in snake_case). Omitted otherwise. (Builder's-check call on the
  meaning; the user did not define it.)

No Python-style union types: a column declared `DOUBLE | VARCHAR` would
describe a column that does not exist in SQL. A semantic or display kind is
left out for now.

### Document shapes

Every document has `view`, `columns`, the counts (`matched`,
`streamTotal`, `truncated`) and the resolved `from`/`to`, then the data:

- **Samples:** `rows`, one object per record keyed by column name. In TOON
  this is a tabular array.
- **Table:** `groups`, nested by group-by level: an array per level, each
  element holding that level's key and either the next level's `groups` or,
  at the innermost level, `count` and the metrics. Arrays, not maps keyed by
  value: JSON keys lose types (`200` becomes `"200"`), and an empty group
  value has no good key. Parent levels carry no numbers; the server computes
  no subtotals, and a parent's p95 cannot be derived from its children.
  Parents appear in order of their first child in the result.
- **Graph:** `bucket`, `starts` (bucket start times, once), and `series`,
  nested by group-by level as the Table. The innermost level holds `count`
  and `values`, an array aligned to `starts` with null for no data. The
  `other` series is a top-level element with `other: true`. With no
  group-by, `series` has one element with only `count` and `values`.
- **Heatmap:** `bucket`, `bands` (each `{band, lo, hi}` in ms, `band` the
  name used below, `lo` omitted on the lowest band), `max`, `total`,
  `skipped`, and `buckets`: one element per time bucket with at least one
  span, `{time, cells}`, where `cells` lists only non-empty bands as
  `{band, count}`. Sparse because heatmaps are mostly zeros, and a band's
  name reads better for an LLM than an index into a matrix. In TOON each
  `cells` list is a tabular array.

`jsonl` uses the flat shapes; a Heatmap in `jsonl` is one line per
non-empty cell: `{time, band, lo, hi, count}`.

TOON is written with `github.com/toon-format/toon-go` (v1.0.1, the TOON
project's own Go implementation), YAML with `gopkg.in/yaml.v3` (already a
dependency). loghorn builds the document as ordered Go values and hands it to
the library; it does not encode TOON itself.

### Computed numbers

Raw values are exported as the exact text the log held. Computed values
(`sum`, `avg`, `error_rate`, graph and table metrics, ms converted from ns)
are rounded to 15 significant digits where `internal/query` computes them,
then printed in the shortest form that round-trips
(`strconv.FormatFloat(v, 'g', -1, 64)` after rounding). A float64 holds 15
decimal digits reliably, so only arithmetic noise goes:
`23.839999999999996` → `23.84`, `0.30000000000000004` → `0.3`. Rounding at
the source keeps the page, the copy buttons and every export identical. The
DuckDB equivalence test compares DOUBLEs with a relative tolerance, since
DuckDB's own arithmetic carries the same noise.

### Copy buttons in the page

The three copy bars (`copyTableBar` in `copy.js`: the main table bar for
Samples/Table/Heatmap, the aggregate view, the trace view) each have
"Copy as Markdown" and "Copy as CSV" today, built in the browser from the
displayed (formatted) table.

- Each bar becomes one "Copy" button with a ▾ droplist of formats. The
  button copies in the format chosen last, remembered in `localStorage`
  (wrapped in try/catch; default Markdown when unset).
- **Main views** (Samples, Table, Graph, Heatmap): the copy POSTs the current
  query to `/api/query` with the chosen `format` and copies the reply, so the
  clipboard holds exactly what `loghorn <url>` prints, `columns` section
  included. Formats: all seven data formats. The bar shows for Graph too,
  which has no copy today. The query sent is the one behind the rows shown
  (same offset and limit for Samples).
- Values copy raw (`1234.5`, not `1.2 s`). This is a visible change from
  today's formatted copy, and the point of it.
- **Trace and aggregate views** keep building the text in the page; the
  server has no export for them. Their droplist offers Markdown, CSV and TSV.
- The clipboard write must stay inside the click's user gesture while the
  fetch is pending (Safari): use
  `navigator.clipboard.write([new ClipboardItem({"text/plain": promise})])`
  where supported, else fetch then `writeText`.
- Success and failure show in the bar's existing status, as today.
- Per CLAUDE.md, a droplist button inside a control that redraws on blur
  must `preventDefault` on `mousedown`.

Row limit: the URL's `limit` and `top` apply as in the page (`limit` 0 is
the Samples default of 100). `-limit N` on the CLI overrides `limit` for
Samples and `top` for Table and Graph (within their existing caps), and is
refused for Heatmap, which has no row limit. The truncation note covers
Samples hitting the limit, Table groups cut by `top` (`Groups` > rows), and
Graph series folded into `other`.

### SQL (`format: sql`)

`query.ToSQL(q Query, s schema.Schema, r *Result) (string, error)`
in a new file in `internal/query`. The handler runs the query first and
passes the `Result`, because three things the SQL needs exist only after a
run: the bucket `auto` resolved to, the heatmap band edges, and the Samples
column list when the URL names none (`intervalColumns`). These are frozen
into the SQL as resolved. The range is translated as the URL states it, with
no rewriting: a relative range stays relative, an absolute one stays
absolute. The page will offer copying the URL either way, so the user picks
there. Re-running a relative query later may pick different rows than the
frozen buckets and bands were sized for; it stays a valid query. Output is
DuckDB SQL, formatted with `internal/sqlfmt`, preceded by a comment block
that states the table model, so an LLM editing it knows what it targets.

Table model (the contract the DuckDB work must implement):

- Schema `logs` for log records and `spans` for span records; one table per
  stream named exactly as the stream (`logs."app"`, `spans."http.server"`).
  `spans."*"` is every span kind (the Spans `*` stream).
- Columns: `seq BIGINT` (store order; every sort breaks ties on it, and the
  default Samples sort is `time DESC, seq DESC`), `time TIMESTAMPTZ` (the
  effective time, including inherited times, which is what the range and
  sorts use), `service VARCHAR`, `raw VARCHAR` (the line search matches), and one column per field path, quoted with its dotted
  name (`"http.status"`), typed from the schema type (string → VARCHAR,
  int → BIGINT, float → DOUBLE, duration → DOUBLE ms, bool → BOOLEAN,
  time → TIMESTAMPTZ, json → JSON). A value that does not fit its field's
  type (the Go side's "bad" value) loads as NULL. In Go a bad value matches
  `not-empty`; in SQL it does not. The SQL header comment says so, and the
  DuckDB equivalence test uses records without bad values. (Builder's-check
  call: keeping raw text beside every column doubles the model for a rare
  case.)

Translation:

- Service other than `*` → `service = '…'`.
- Range: a relative `from` becomes `time >= now() - INTERVAL '…'`; RFC 3339
  becomes a literal. `to` likewise; `now`/empty adds nothing.
- `this-run` is relative, like `now()`: it stays symbolic in the SQL rather
  than becoming the current run's literal start and ID. (An absolute export
  is the page's job: its "copy absolute URL" button replaces `this-run` with
  timestamps before the CLI ever sees it.) It becomes
  `time >= loghorn.this_run_start() AND ("loghorn.run.id" =
  loghorn.this_run_id() OR "loghorn.run.id" IS NULL)`. The two macros are
  part of the table-model contract: the DuckDB layer defines them for the
  run that is current when the SQL executes. A comment explains that the
  command's own output lines carry no run ID, so once later runs are stored
  the query also matches their untagged lines. Wrapped commands are a
  developer-testing convenience, rarely used in production, so captured
  lines are not stamped with the run ID; stored records are unchanged.
- Filters: every operator in `hasOp` maps to SQL with the same semantics
  (case-insensitive `contains` → `ILIKE` with `%`/`_` escaped; `in` →
  `IN (…)`; `empty` → `IS NULL OR = ''` for strings). Literals are quoted
  per type; identifiers always double-quoted with `"` doubled.
- Search → `raw ILIKE '%…%'` (escaped).
- Samples: `SELECT <columns or *> … ORDER BY <sort> LIMIT n OFFSET m`.
- Table: `GROUP BY` the group-by fields; `count(*)`, `sum`, `avg`, `min`,
  `max`; pNN as exact nearest rank to match Go's `percentile()`
  (`ceil(p·n)-1`), e.g. `list_sort(list(f))[ceil(0.95 * count(f))]` (DuckDB
  lists are 1-based), with a comment saying why it is not
  `quantile_cont`; error rate as the fraction with
  severity at or above ERROR (same set as `isErrorSeverity`); order and top
  as the server applies them.
- Graph: `time_bucket(INTERVAL '<resolved bucket>', time)` with the bucket
  the server resolved for `auto`; top-N series by the order metric, the rest
  folded into `other`, written with CTEs. The output is long (bucket,
  series, value); a comment says the CLI's wide form is a pivot of it.
- Heatmap: the band edges the server computed, as a `CASE` on
  `duration_ms`, grouped by bucket and band.
- Anything that cannot be expressed exactly is an error, never silently
  approximate SQL. The bad-value rule above is a property of the table
  model, not of a query, and is the one stated exception.

### Socket and discovery

- Directory: `$TMPDIR/loghorn` (macOS gives a per-user `$TMPDIR`), else
  `$XDG_RUNTIME_DIR/loghorn`, else `/tmp/loghorn-<uid>`. Created 0700; if it
  exists and is not owned by the user or is group/world accessible, the
  socket is skipped with a warning (the TCP listener still serves).
- Every `-web` start (live, replay, standalone) listens on `<pid>.sock` in
  that directory, served by the same handler as TCP, and removes it on
  normal shutdown. A failure to create the socket warns and continues; it is
  never fatal.
- Guard: a request that arrived on the unix listener skips the Host and
  Origin checks (browsers cannot reach a unix socket; file permissions are
  the access control). Every other request is guarded as today.
- `/api/session` gains `pid`, `cwd`, `port` and `started` so `-instances` and
  the discovery error can describe each instance.
- Discovery connects to every `*.sock` in the directory; one that refuses a
  connection is stale and is removed.

### Verification

- `internal/query` unit tests for each format and view shape, escaping, and
  ToSQL golden tests per operator and view.
- If a `duckdb` binary is on PATH, a test loads the test records into DuckDB
  in the table model and checks that the generated SQL returns the same rows
  as `query.Run` (with a fixed `now`, and for Graph after pivoting the long
  SQL output); it skips otherwise.
- Handler tests over `httptest.NewServer` (TCP) and a real unix listener.
- An end-to-end test runs the built binary against a demo instance with the
  positional URL, `-url`, `-o`, `-to-sql` and `-instances`.
- Tests point `HOME` and `TMPDIR` at temp dirs.

## Future: SQL input (agreed syntax, not built now)

Recorded so later work keeps the shape. `-sql` is not built in this round;
`-url @file`/`@-` and the stdin rules below are.

- `-sql 'SELECT …'` runs SQL against the DuckDB table model above, once the
  DuckDB engine exists. `-sql @query.sql` reads it from a file, `-sql @-`
  from stdin (curl's convention; no SQL statement starts with `@`), the same
  forms `-url` already takes.
- Stdin must never be taken as log input in query mode. Any query flag
  (`-url`, `-sql`, `-to-sql`, `-instances`, or a positional loopback URL)
  makes the run a query before `detectInput` is consulted, so a piped stdin
  is never `kindPipe` there. Stdin is read only when a flag names `@-`, and
  at most one flag may: `-url @- -sql @-` is a usage error (exit 2). Text
  read through `@-` is never recorded to `.loghorn/`.

## Left to the builder

- File layout of the CLI client and formatters.
- Exact wording of messages and `-instances` table.
- How `ToSQL` is factored per view.
