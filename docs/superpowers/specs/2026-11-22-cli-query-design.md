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
  (`/?q=…`), which goes through instance discovery.
- `-o FILE` writes to a file, with the format taken from its extension
  (`.csv`, `.tsv`, `.md`; `.sql` with `-to-sql`). `-format csv|tsv|md`
  overrides the extension. Without `-o`, output goes to stdout.
- `-to-sql` prints DuckDB SQL equivalent to the query instead of data.
- Every view the URL can name works: Samples, Table, Graph, Heatmap.
- Every `-web` instance listens on `$TMPDIR/loghorn/<pid>.sock` too;
  `-instances` lists the live ones.

## Non-goals

- No `-exec` or `-run` flag for the command side; `--` already serves.
- No query language on the command line beyond the URL's `q` JSON.
- The trace and aggregate views (`/api/trace`, `/api/aggregate`) are not
  exported; their crumbs in the URL are ignored with a note on stderr.
- No SQL execution, and no SQL input, in this work.
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
  `format` of `csv`, `tsv`, `md` or `sql` and answers with that text instead
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
the query as today and writes text with `Content-Type` `text/csv`,
`text/tab-separated-values`, `text/markdown` or `application/sql`
(`charset=utf-8`). `sql` does not run the query; it validates it and
generates SQL.

Shared rules for data formats:

- Header row uses column paths (stable, match the SQL), not labels.
- Values are raw: numbers as JSON prints them, times RFC 3339 with
  nanoseconds in UTC, booleans `true`/`false`, null as an empty cell, JSON
  values compact.
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
  stream named exactly as the stream (`logs."taa"`, `spans."http.server"`).
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

## Left to the builder

- File layout of the CLI client and formatters.
- Exact wording of messages and `-instances` table.
- How `ToSQL` is factored per view.
