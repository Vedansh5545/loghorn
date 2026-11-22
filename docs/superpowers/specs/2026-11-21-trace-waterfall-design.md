# Trace waterfall view — design

Date: 2026-11-21
Status: approved (pre-approved by the user; decisions below were made by the
author and are recorded here)

## Intent

The web explorer shows spans and log lines as flat rows. A span's place in
its request, what ran in series, and which log lines it wrote are invisible.
This change adds a **trace waterfall** (internally "HV", hierarchical view):
click a span ID or trace ID in the table and the page shows that trace as a
collapsible span tree on the left with time bars on the right, like Google
Cloud Trace or Jaeger's timeline. Log lines that carry the trace ID appear
inside the tree, under the span that wrote them.

## What exists today

- `-otel` stores spans one JSON object per line (`internal/otlp/spans.go`):
  `trace_id`, `span_id`, `parent_span_id?`, `name`, `kind?`, `start`, `end`,
  `duration_ms`, `status?`, `status_message?`, `attributes{}`, `events[]`,
  `links[]`, `service.name`, `otel.resource{}`, …
- `record.Normalize` turns a span line into a `record.Record` with
  `Source: "otel"`, `Stream` = its kind, `Time` = `start`, and `Fields` holding
  `trace_id`, `span_id`, `parent_span_id`, `name`, `kind`, `start`, `end`,
  `duration_ms` (`json.Number`), `status`, `span.type` (one of `gen_ai`,
  `messaging`, `rpc`, `db`, `http.server`, `http.client`, `span`) and the
  attributes lifted to the root (`db.query.text`, …).
- Log records carry trace context under different names by origin:
  OTLP logs (`internal/otlp/mapping.go`) write `trace_id` and `span_id` (hex);
  Cloud Logging entries carry `trace` (`projects/<p>/traces/<hex>`) and
  `spanId` (promoted from `logging.googleapis.com/trace` / `…/spanId`); local
  JSON loggers often write `trace_id`/`span_id` or `spanId`.
- The store (`internal/store`) keeps every record in memory; `Snapshot()`
  returns them in `Seq` order. Queries are evaluated in Go
  (`internal/query.Run`) over the snapshot — there is no SQL engine.
- `GET /api/record?seq=N` returns a record's raw JSON plus SQL fields
  formatted by `sqlfmt.Format` and coloured by `sqlcolor.Tokens` (GoSQLX). The
  page shows this in an inline detail row (`toggleDetail` in `app.js`).
- The page (`internal/web/static/app.js`, ~2800 lines) has no JS test
  harness; browser checks use `go run ./internal/web/demo <file>`.

## Decisions

### D1. Tree analytics are computed in Go, not in the page

The tree, missing-parent placeholders, log placement, self time, critical path
and serial runs are all computed server-side in a new package
`internal/trace`, and the page only renders, collapses, finds and toggles.
Reason: Go has `go test`; the page has no test harness, and these are exactly
the computations where an off-by-one or a cycle would silently mislead.

### D2. One endpoint: `GET /api/trace`

`GET /api/trace?trace=<id>` or `GET /api/trace?span=<id>` (both may be given;
`trace` wins). With only `span`, the server finds the trace ID from the span
record with that `span_id`, else from a log record with that span ID and a
trace ID. The answer lists every span and every log record of the trace
across **all services and both sources**; `?service=` is ignored, because a
trace crosses services by nature.

Errors: neither parameter → 400 `trace or span is required`; a value longer
than 256 bytes → 400; nothing found → 404 `no spans or logs with that trace
id` / `no record with that span id`.

### D3. ID normalisation

`record.NormalizeTraceID(s)`: trim spaces, take the part after the last
`/traces/` if present, lowercase. `record.NormalizeSpanID(s)`: trim,
lowercase. An all-zero ID counts as absent. Decimal Cloud Trace v1 span IDs
are **not** converted (rare today; noted as deferred).

`record.TraceRef(fields) (traceID, spanID string)` reads, in order,
`trace_id` then `trace` for the trace, and `span_id` then `spanId` for the
span, both normalised. For span records it returns the span's own IDs.

### D4. Tree rules

- A span's parent is the span whose `span_id` equals its `parent_span_id`.
  No `parent_span_id` → a root.
- **Missing parent:** spans whose `parent_span_id` names no span in the data
  are grouped under one placeholder node per missing ID, shown as
  `(Missing span <id>)`. The placeholder is a top-level node; its start is its
  earliest child's start and its end its latest child's end.
- **Logs:** a log whose span ID names a span goes under that span. A log whose
  span ID names no span goes under that ID's missing placeholder (created if
  needed — the log is evidence the span existed). A log with no span ID goes
  under the trace's root span when there is exactly one root span, else at
  the top level.
- **Ordering:** children (and top-level nodes) sort by start time, then spans
  before logs at the same instant, then `Seq`.
- **Duplicates:** two span records with the same `span_id` (a file loaded
  twice, a re-sent export) — the lowest `Seq` wins; the rest are dropped.
- **Cycles:** a span not reached from any root or placeholder (a parent
  cycle, or a span that is its own parent) is made a top-level node, earliest
  start first, so every span appears exactly once and building always ends.
- **Times:** start = `Record.Time`; end = `end` field, else start +
  `duration_ms`; an end before the start is clamped to the start. All
  offsets are milliseconds (float) from the trace start, which is the earliest
  span start or log time; the trace end is the latest span end or log time.
- **Limit:** at most 10 000 spans and 10 000 logs (lowest `Seq` first); over
  that, `truncated: true` and the page says so.

### D5. Analytics

- **Self time** of a span = its duration minus the union of its child
  *spans'* intervals clipped to the span's own interval. Logs have no
  duration. Placeholders have self time 0.
- **Self-time summary:** spans grouped by `name` → count, total duration,
  self time; served sorted by self time descending, sortable in the page.
- **Critical path** (Jaeger's algorithm): start from the top-level node that
  ends last; walk its children from the latest-ending back, taking each child
  that ends at or before a cursor (initially the parent's end), recursing into
  it, and moving the cursor to that child's start. Whole spans are flagged
  `critical`; partial segments are not drawn in v1.
- **Serial runs:** among the span children of one node, in start order, a run
  of ≥ 3 consecutive `db` spans (`span.type` `db`) where each starts at or
  after the previous one ends. Any non-`db` span sibling ends a run; log rows
  do not. Each run reports its first span, count and wall time (last end −
  first start), shown as `6 serial queries, 48 ms`.

### D6. The page

- **Opening.** In the Samples and Table views, a cell whose column path is
  `trace_id` or `trace` renders its value as a link; so does `span_id`,
  `spanId` or `parent_span_id`. Clicking it (without toggling the row's
  detail) opens the waterfall: a span ID selects that span, expands its
  ancestors and scrolls it into view; a trace ID selects nothing.
- **Deep link.** The page URL becomes `?q=<query>&trace=<id>&span=<id>`
  (`span` optional). Opening pushes a history entry, so Back closes the view;
  selecting a row replaces the entry. On page load a `trace` parameter opens
  the view after the normal startup. Close (button or Escape when the find box
  is empty) returns to the table with its query intact.
- **Placement.** The view replaces the search bar, results and footer inside
  `.content`; the sidebar stays as it is.
- **Header:** trace start (local time, ISO in the tooltip), total duration,
  span count, log count, event count (when there are events), service count, and a truncation note. A **Reload**
  button refetches (live runs); there is no auto-refresh.
- **Toolbar:** Close, Reload, Collapse all, Expand all, `Logs & events: in
  tree / hidden`, `Critical path` toggle, the find box with ◀ ▶ and `n of m`.
- **Grid:** one row per visible node — name column (tree connectors, a ▸/▾
  twisty on nodes with children, name with ellipsis and full name in the
  tooltip), a service column **only when the trace has more than one
  service**, and the bar column. A time axis above the rows shows 5–10
  gridline labels at 1/2/5 × 10ⁿ ms steps, formatted with `formatDuration`;
  the gridlines run down through the rows.
- **Bars** are positioned by `startMs/durationMs` of the trace, at least 2 px
  wide, coloured by type: `db`, `http` (`http.server` and `http.client`),
  `rpc`, `messaging`, `gen_ai`, `span` (everything else); a legend shows the
  types present. An error span gets the error colour outline. Each bar is
  labelled with its duration, after the bar, or before it when the bar ends in
  the right fifth of the axis. Placeholder bars are grey and hatched.
- **Logs in the tree:** log rows show a severity badge and the message (first
  line, ellipsis), a small diamond at their time on the axis, and a tick on
  the parent span's bar at the log's time. `Logs: hidden` removes the log rows
  but **keeps the ticks**, which is the "markers only" mode — there is no
  separate third mode. The choice is remembered in `localStorage`
  (`loghorn.traceLogs`, read in try/catch).
- **Span events** (the span line's `events` array) are child nodes of their
  span (kind `event`, ID `event:<span id>:<index>`, capped at 10 000 like
  spans and logs), ordered by time with the span's logs. A row shows a ◆ and
  the event name; an `exception` event is drawn in the error colour and shows
  `exception.type: exception.message`. The span's bar gets a tick at each
  event's time (red for an exception). An event outside its span's range keeps
  its real time and stretches the trace; one without a valid time sits at the
  span's start. Events are points: they add no self time and are not on the
  critical path. The `Logs & events` toggle hides event rows as it does log
  rows (ticks stay), and find matches event names and exception messages.
  Selecting an event shows its name, time, parent span (a link that selects
  it) and attributes. The table does not expand events into rows; each span
  record carries a derived `span.event_count` field, and the full array stays
  in the record JSON.
- **Selection** highlights the row and shows its record in a detail pane below
  the grid, using the same rendering as the table's detail row (SQL fields
  formatted and coloured, then the record JSON). A placeholder shows
  `This span is not in the data` with its ID and child count.
- **Find** matches span names and, when logs and events are in the tree, log
  messages, event names and exception messages (case-insensitive substring). Enter / ▶ goes to the next match, Shift+Enter /
  ◀ the previous; going to a match expands its ancestors, selects it and
  scrolls it into view. Matching rows are marked.
- **Serial runs** show as a chip `6 serial queries, 48 ms` in the name cell of
  the run's first span, with a faint band behind the run's rows.
- **Critical path** toggle (off by default) dims bars not on the path.
- **Self-time table** below the detail pane: name, count, total, self, self %
  of the trace duration; click a header to sort.

## Deferred and discarded

| Item | Decision | Reason |
|---|---|---|
| Aggregate flame graph across many traces | deferred | A different question (where time goes overall) and a separate view; candidate libraries d3-flame-graph, flame-chart-js. |
| Span links / cross-trace jumps (messaging) | deferred | Links are stored but rare in today's data; needs its own navigation design. |
| Live auto-refresh of an in-progress trace | deferred | A manual Reload button covers v1 without polling or scroll-jump handling. |
| Sequence diagram between services | deferred | Usage is single-service today; little to draw. |
| Trace diff / compare | deferred | Needs two-trace alignment; separate feature. |
| Service dependency graph view | discarded for now | Nothing to show for a single service. |
| Share button | discarded | The deep link in the URL is the share. |
| Log display "markers only" as a third mode | folded in | `Logs: hidden` keeps the bar ticks, which is markers-only. |
| Critical-path segments within a span | deferred | Whole-span flags answer "which spans matter" in v1. |
| Keyboard tree navigation (↑ ↓ ← →) | deferred | Mouse and find cover v1; add with accessibility pass. |
| Virtualised rendering for very large traces | deferred | v1 renders every row and caps at 10 000 spans + 10 000 logs. |
| Decimal (Cloud Trace v1) span IDs | deferred | Not seen in current data. |
| Resizable name column, zoom/brush on the axis | deferred | Fixed proportions first; revisit with real traces. |
| Index of records by trace ID | deferred | One linear scan of the in-memory snapshot per open is fast enough for a local tool. |

## Testing

- `internal/record`: `TraceRef` and the normalisers (prefix, case, all-zero,
  log vs span spellings).
- `internal/trace`: a fixture trace (`testdata/web/trace.jsonl`, generic
  services `api-server` and `pricing`) with nesting, a 6-query serial run, two
  overlapping db spans, a cross-service child, an error span, a span with an
  absent parent, logs under spans, a log with only a trace ID, a log with an
  unknown span ID, a Cloud-Logging-style log, and a second unrelated trace.
  Exact expected node order, self times, critical set and serial run are in
  the plan. Plus cycles, duplicates, missing/negative ends, truncation.
- `internal/web`: `/api/trace` by trace, by span (span record and log-only
  span), errors (400/404), cross-service, via `httptest.NewServer`.
- Browser check with the demo over the fixture for every page item above.

## Open questions

None.
