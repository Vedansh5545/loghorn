# Span streams in the web view — design

Date: 2026-11-20
Status: awaiting approval

## Intent

The app under test used to write structured log lines that each carried a
`labels.schema` value (`db`, `api`, `external`, …), and loghorn's web view
showed each value as a *stream*. The app now records that work as
OpenTelemetry spans instead, which `-otel` stores in
`.loghorn/loghorn.traces.jsonl`. Today the web view never reads that file, so
the old streams have gone quiet.

This change makes the web view read the span file too and sort each span
into a stream by its **standard OpenTelemetry semantic-convention
attributes**, so the explorer (Samples / Table / Graph, column menus, saved
schemas) works on spans the way it worked on the old schema'd log lines.

## Goals

- `-web` (live and replay) shows span streams next to the existing log
  streams.
- A span's stream is decided only by standard OTEL attributes and span kind.
  loghorn is a public, generic tool; nothing app-specific is built in.
- Every span lands in some stream; none is dropped.
- When the log and span files are later merged into one file, the
  classifier keeps working without a rewrite.

## Non-goals

- Rules over app-specific attributes (a `<app>.*` namespace). Spans that only
  such attributes could identify go to the catch-all stream for now. Rules
  configurable from the repo's `.config/loghorn/` are a later change.
- Trace views: parent/child trees, waterfall, joining a log line to its span.
- Span events as records of their own. They stay a JSON field on the span.
- `-filter`, `-historical` without `-web`, and the CLI: unchanged.
- Merging the two files (planned separately).

## Anti-goals

- **Keep ingestion changes minimal.** Another person is working on it. The
  only ingestion edit allowed is the small hook that reports live spans to
  the store (see "Live spans"). No change to `internal/ingest`,
  `internal/otlp`, `internal/recorder` or the `internal/logfile` writer, and
  no change to what `-otel` writes or how it frames lines.
- A span stream must never mix with a log stream, even when they share a name
  (see "Stream identity").
- No re-reading of files per query. The store is updated live, and a query
  reads the store.

## Constraints

- Log lines keep today's stream rule (`labels.schema`, else `http-request`).
- Stream names must pass `schema.ValidStream` (`^[a-z0-9][a-z0-9._-]*$`),
  because saved schemas are stored as `schemas/<stream>.toml`.
- Stored historical runs that hold the old log formats will be deleted (the
  user's decision). The deletion is a manual step done in the app's own
  `.loghorn/`, after showing the user the exact file list. loghorn's code
  does not do it.

## Stream identity

A stream is identified by **source plus name**. The source is `log` (from
`labels.schema` / `http-request`, as today) or `otel` (a span). One name can
exist under both sources, for example a logging app with `labels.schema=db`
that also sends DB spans. These stay two separate streams.

- **Records:** `record.Record` gains a `Source` field. The store counts
  streams by (source, name).
- **API:** `/api/streams` returns `{name, source, count}`. Every endpoint that
  takes `stream` also takes `source`, defaulting to `log` so existing URLs
  keep working. This includes the schema GET and PUT, and queries.
- **Page:** the stream selection box shows only the name when that name exists
  under one source. When it exists under both, both entries show their
  source in parens: `db (log)` and `db (otel)`. The page URL always carries
  the source when it is `otel`, so a link stays unambiguous if a clash
  appears later.
- **Saved schemas:** span streams save to `schemas/otel/<name>.toml`, and log
  streams to `schemas/<name>.toml` as today. `ValidStream` still applies to
  the name. `FindConfigDir`'s walk is unchanged.
- **Schema cache:** the server's per-stream schema cache is keyed by (source,
  name).

## The span line (as written today)

One compact JSON object per line (`internal/otlp/spans.go`, `spanLine`):

`trace_id`, `span_id`, `parent_span_id?`, `name`, `kind?`
(`internal|server|client|producer|consumer`), `start`, `end` (RFC 3339 nano,
UTC), `duration_ms` (number), `status?` (`ok|error`), `status_message?`,
`attributes{}` (flat, dotted OTEL keys), `events[]`, `links[]`,
`service.name`, `loghorn.run.id`, `otel.resource{}`, `otel.scope.name`,
`otel.scope.version`.

Files: `loghorn.traces.jsonl` (today) and `loghorn-YYYY-MM-DD.traces.jsonl`
(archives, same rotation and retention as the log).

## Stream rules

A line is a **span** if it is a JSON object with a string `span_id` and a
string `start`, and no `labels.schema`. This decides by the line's shape, not
by its file, so the rule survives the planned merge.

Rules are checked in order and the first match wins. Order matters, because
one outbound client span can carry several namespaces at once. For example,
an LLM call has `gen_ai.*` plus `http.*`, and a cloud-queue `createTask` has
both `rpc.*` and `messaging.*`.

| # | Stream | Rule |
|---|---|---|
| 1 | `gen_ai` | any attribute key starting `gen_ai.` |
| 2 | `messaging` | `messaging.system.name` or legacy `messaging.system` |
| 3 | `rpc` | `rpc.system.name` or legacy `rpc.system` |
| 4 | `db` | kind `client` and `db.system.name` (or legacy `db.system`) |
| 5 | `http.server` | kind `server` and `http.request.method` (or legacy `http.method`) |
| 6 | `http.client` | kind `client` and `http.request.method` (or legacy `http.method`) |
| 7 | `span` | everything else |

Notes:
- Rule 4 requires `client` because a database-open step (an `internal` span)
  also carries `db.system.name`.
- Messaging comes before rpc: a queue send over gRPC is a message send.
- Email delivery: the outbound send carries `messaging.system.name`, and the
  app is moving its delivery job to a `consumer` span with
  `messaging.system.name`. Both land in `messaging` with no email-specific
  rule.
- Background jobs, timers, startup steps and similar `internal` spans have no
  semantic convention and go to `span`.
- The rules are a Go table in one function, easy to extend, but not
  user-configurable in this change.

## Span records

`record.Record` for a span:

- **Stream**: from the rules above.
- **Time**: `start`. The store's time filter, ordering and charts use it.
- **Fields**: the span's top-level keys, with `attributes` lifted to the root
  under their own OTEL keys, so a column is `db.query.text`, not
  `attributes.db.query.text`. Attribute keys are taken verbatim and never
  split, so a flat key like `http.url` cannot collide with a nested object.
  - If an attribute key equals a top-level span key (e.g. an attribute called
    `name`), the span key wins and the attribute is kept as
    `attributes.<key>`.
  - `otel.resource` flattens to `otel.resource.<key>` as log records do
    today.
  - `events` and `links` stay whole as JSON arrays.
  - `timestamp` is set to `start`, so the existing default columns, time
    handling and `ORDER BY timestamp` keep working.
  - `duration_ms` stays a number, so it can be charted and aggregated.
- **Raw**: the line as stored.

Values follow the same typing as log records: numbers are `json.Number`.

## Approaches considered

1. **Classify spans inside the existing record path; read both files
   (chosen).** Add a span branch in front of `record.Normalize`, and load the
   span files wherever the web view loads log files. It reuses the store,
   schema inference, queries and the whole page, and it carries over
   unchanged when the files merge.
2. **A separate span store and API.** Rejected: it duplicates the explorer
   for no gain, and the merge would undo it.
3. **Read only the span file.** Rejected: some logs have no span. Auth
   events, log lines outside any span, and WARN/ERROR lines stay in
   `loghorn.log`. Reading only the span file would lose them.

## Where it reads the span file

- **Live `-web` startup** (`startWeb`): after `store.Preload` of the log files,
  preload the span files: archives oldest first, then
  `loghorn.traces.jsonl`.
- **Replay `-web`** (`runReplay`, `-historical` and `-web` alone): after the
  log sources, preload the span files into the same store. Replay from a file
  on stdin or from GitHub has no span file, so nothing changes there.
- **Live spans during a run**: in `wrapper.go`, `spanSink` gets the web store,
  if there is one, and after writing each line it also adds the normalised
  span to the store. Log lines already reach the store this way, through the
  recorder. A span whose file write failed is still added to the store. A
  new query then sees spans that arrived mid-run, without re-reading any
  file.

The startup preload and the live hook cannot overlap. The preload runs in
`startWeb` before the receiver starts, so a span is either in the file at
preload time or arrives through the hook afterwards. The plan must keep that
ordering.

A new `logfile.HistoricalSpanFiles(dir)` (or a base/suffix-parameterised
`HistoricalFiles`) lists the span files. `parseArchiveDay` already takes base
and suffix. This only reads the directory, so it is not a writer change.

Span lines are framed one object per line. `ingest.Stored` is reused
unchanged.

## Testing

- Classifier: a table test with one span per rule, plus the overlaps
  (gen_ai+http, messaging+rpc, `internal`+db.system.name, `client` with
  http but no other namespace), and legacy attribute names.
- Record: field lifting, an attribute that collides with a top-level span
  key, time from `start`, and `json.Number` durations.
- Preload: a temp `.loghorn/` with log and span archives plus current files
  gives the expected streams and counts.
- Web: `/api/streams` lists span streams, and a query on `db` returns rows
  with `db.query.text` (with `httptest.NewServer`; see CLAUDE.md).
- A manual check against a real span file from the app under test.

## Left to the builder

- Package placement of the span normaliser (`record.NormalizeSpan` vs a
  branch in `Normalize`).
- Whether span records get a synthetic `severity` (e.g. `ERROR` when
  `status=error`) so the default column set looks right. Recommended, but
  not required.

## Open questions

None.
