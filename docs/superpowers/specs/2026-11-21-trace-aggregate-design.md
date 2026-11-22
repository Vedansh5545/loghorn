# Aggregate operation view — design

Date: 2026-11-21
Status: approved (pre-approved by the user; decisions below were made by the
author and are recorded here)

## Intent

The trace waterfall (`docs/superpowers/specs/2026-11-21-trace-waterfall-design.md`)
answers "what happened in this one request". This change answers "is this
request always like this, and where does time go across many requests of one
operation?". The user picks an **operation** — the name of a root span, such
as `GET /orders` — and the page's time range; loghorn merges every matching
trace into one tree keyed by the span-name path (siblings with the same name
merge) and shows, per merged node, how often it runs, how long it takes and
what share of the operation's time it holds.

## What exists today

- Spans are `record.Record`s with `Source: "otel"` held in memory by
  `internal/store`; `Store.Snapshot()` returns every record in `Seq` order.
  There is **no SQL engine**: every query (`internal/query.Run`, the Spans
  mode, the Graph view) is a linear pass in Go over the snapshot. "One query
  for all spans of the matching traces" therefore means one pass over the
  snapshot that groups span records by trace ID.
- `trace.Build(recs, traceID, limit)` (`internal/trace/trace.go`) builds one
  trace: dedupes span IDs, puts spans with an absent parent under
  `missing:<id>` placeholders, breaks cycles, computes `SelfMs`, the critical
  path and `SerialRuns` (≥ 3 consecutive non-overlapping `db` siblings) in
  `analyze`. Build filters its input by trace ID, so it can be handed one
  trace's records.
- `GET /api/trace` serves one trace; the page module
  `internal/web/static/trace.js` renders it (tree rows with connector guides,
  twisties, type-coloured bars `bar-db|http|rpc|messaging|gen_ai|span`, serial
  chips) and manages history (`HISTORY_MARK`, `popstate`, Close = Back).
- The page's range is `state.range = {from, to}` (`from` is `this-run`, `all`,
  a Go duration or RFC 3339; `to` is `now` or RFC 3339), resolved on the server
  by `query.Range.Resolve(now, runStart)`, as `/api/kinds?from=&to=` does.
- The page's service is `state.service` (`""` or `*` = every service).

## Decisions

### A1. Computed in Go, in `internal/trace`

Merging, percentiles, shares and serial statistics are computed server-side
in a new file `internal/trace/aggregate.go`, for the same reason as the
waterfall's D1: they are tested with `go test`; the page has no test harness.

### A2. Endpoint `GET /api/aggregate`

`GET /api/aggregate?name=<root span name>&service=<svc>&from=<from>&to=<to>`.

- `name` is required (400 `name is required`); longer than 1024 bytes → 400
  `name too long`.
- `service` optional; `""` or `*` means every service. It restricts which
  **root spans** match; spans below a matched root come from every service
  (a trace crosses services by nature).
- `from`/`to` use the page's range strings and `query.Range.Resolve`; a bad
  value → 400 with Resolve's error text. A root matches when its start is in
  `[from, to]` (a zero bound is open).
- No matching root → 404 `no root spans with that name in range`.
- Read-only; no config access.

### A3. What counts as an operation root

A span record named exactly `name` (case-sensitive) whose `parent_span_id` is
empty, **or** names a span ID that no span record in the session has (a
remote parent: the caller did not export to loghorn). A span named `name`
whose parent is present is a nested call, not an operation, and is ignored.
When one trace holds several matching roots, the earliest start is the
operation root (lowest `Seq` on a tie); the others count as outside spans
(A6).

### A4. Selection, cap and the single pass

1. **Pass 1** over the snapshot (span records only): collect the set of every
   span ID, and the candidate roots (name, service and range match).
2. Keep the candidates that satisfy A3, one per trace (earliest), sort by root
   start **descending** (most recent first; `Seq` descending on a tie) and keep
   the first `AggCap = 1000`. `matched` = traces before the cap; `capped` =
   `matched > 1000`.
3. **Pass 2** over the snapshot: group the span records of the kept trace IDs
   (map lookup per record). Log records are not read.
4. For each kept trace, `trace.Build(group, traceID, trace.Limit)` — the
   existing builder, so dedupe, cycles, placeholders, self time and serial
   runs are the same as the waterfall's. Find the root node by its span ID and
   merge its subtree (A5).

### A5. Merge rules and per-node metrics

- A merged node is keyed by the path of span names from the operation root.
  Under one merged parent, child spans with the same `name` merge. Only
  `span` nodes are merged; logs, events and placeholders are not.
- Let **N** = traces aggregated and **opTotal** = the sum of the root spans'
  durations over the N traces (ms).
- Per merged node:
  - `traces`: number of traces containing at least one call.
  - `calls`: total spans merged; `callsPerTrace = calls / traces` (average
    over the traces that contain it, so "1.0" means "once whenever present").
  - `p50Ms`, `p95Ms`: per-call duration percentiles, nearest-rank
    (`sorted[ceil(p·n) − 1]`), the same method as `query`'s table.
  - `totalMs` (sum of call durations), `selfMs` (sum of `SelfMs`),
    `totalPct = totalMs / opTotal · 100`, `selfPct = selfMs / opTotal · 100`.
    Children that ran in parallel can sum to more than their parent; the
    page caps the bar at 100 % and shows the real number.
  - `errors`: calls with status error.
  - `type`: the most common `span.type` of its calls (ties: alphabetical);
    `services`: sorted distinct services of its calls.
  - `slowest`: `{traceId, spanId, durationMs}` of its longest call (ties:
    the most recent trace, then lowest `Seq`).
- **Serial pattern** (from each trace's `SerialRuns`): a call that is one of a
  serial run's spans is a serial call. Per merged node: `serialCalls`,
  `serialMs` (their summed durations), `serialPerTrace = serialCalls /
  traces`, `serialPct = serialMs / opTotal · 100`. All zero for nodes that
  never ran in a run (in practice: non-`db` nodes).
- Order: the root first; children by average start offset from their trace's
  root start (ascending), then name. Served in pre-order with `id`
  (`n<index>`), `parent`, `depth`, `children`.
- Merged-node cap: 10 000 (`trace.Limit`). A new path past the cap is dropped
  and `nodesTruncated` is true; the header says so.
- All floats pass through `finite` (no NaN/Inf in JSON); percentages are not
  rounded on the server.

### A6. Honesty about incomplete data

- Header always states `N traces` and, when capped, `N of M traces (capped,
  most recent)`.
- Spans of a kept trace that are not in the operation root's subtree (orphans
  under a missing-parent placeholder, extra roots, cycle members) are
  **outside spans**: they are not merged. The answer carries `outsideSpans`
  (count) and `incompleteTraces` (traces with ≥ 1 outside span); the header
  shows `K traces with M spans outside the root (missing parents)` when
  non-zero.
- A kept trace whose Build hit `trace.Limit` counts in `truncatedTraces`;
  header note when non-zero.
- Spans whose own parent is missing *inside* the subtree cannot happen: Build
  puts them under top-level placeholders, so they are outside spans.

### A7. Answer shape

```json
{
  "name": "GET /orders", "service": "api-server",
  "from": "…", "to": "…",
  "traces": 5, "matched": 5, "capped": false, "cap": 1000,
  "opTotalMs": 600,
  "outsideSpans": 1, "incompleteTraces": 1, "truncatedTraces": 0,
  "nodesTruncated": false,
  "nodes": [
    {"id": "n0", "parent": "", "depth": 0, "children": 3,
     "name": "GET /orders", "type": "http.server", "services": ["api-server"],
     "traces": 5, "calls": 5, "callsPerTrace": 1, "p50Ms": 100, "p95Ms": 200,
     "totalMs": 600, "selfMs": 0, "totalPct": 100, "selfPct": 0,
     "errors": 1, "serialCalls": 0, "serialMs": 0, "serialPerTrace": 0,
     "serialPct": 0,
     "slowest": {"traceId": "…", "spanId": "…", "durationMs": 200}}
  ]
}
```

(`selfMs` above is illustrative.) `from`/`to` are the resolved bounds; a zero
bound is omitted (`omitzero`).

### A8. Performance budget

Target: **≤ 250 ms** server time for 1 000 traces × 50 spans in a store of
500 000 records on a laptop; ≤ 50 ms for the usual local session (tens of
traces). Achieved by two linear passes over the snapshot (map lookups, no
per-trace rescans), `Build` on each trace's own small slice, and no log
records read. A Go benchmark (`BenchmarkAggregate`) records it; it is not a
pass/fail test. No caching on the server; the page keeps the last answer per
`(name, service, from, to)` so Back from a trace does not refetch.

### A9. The page

- **Module** `internal/web/static/agg.js`, section `#agg` in `index.html`,
  placed like `#trace` (replaces the search bar, results and footer; the
  sidebar stays). It imports `typeGroup` and the new exported `treeGuides`
  from `trace.js`; `trace.js` gets `openAggregate` through its deps from
  `app.js` (no import cycle).
- **Reuse:** rows use the waterfall's classes (`.trace-row`, `.tr-name`,
  `.tg`, `.twisty`, `.tw-pad`, `.tbar`, `.bar-<group>`, `.serial-chip`), the
  connector guides come from `treeGuides`, colours and legend are the same.
- **Header:** operation name, service (or `all services`), the range as the
  page shows it, `N traces` / `N of M traces (capped, most recent)`, p50/p95 of
  the root, and the A6 notes. **Toolbar:** Close, Reload, Collapse all,
  Expand all.
- **Columns:** name (guides, twisty, name, serial chip), `traces` (count and %
  of N), `calls/trace`, `p50`, `p95`, `total %`, `self %`, then a share bar:
  a bar of width `min(totalPct, 100)%` in the type colour with an inner darker
  segment of width `selfPct`; an error count badge when `errors > 0`. Columns
  have a title tooltip explaining the metric.
- **Serial chip** on a node with `serialCalls > 0`:
  `<serialPerTrace> serial queries/trace, <serialPct>%` (one decimal).
- **Jump-off:** each row's name has a `slowest <duration>` link; clicking it
  calls `openTrace({trace, span})`, which opens the waterfall with that span
  selected (history push), so Back returns to the aggregate.
- **Entry points:**
  1. Waterfall toolbar button `Aggregate this operation`: the trace's
     operation root = first top-level `span` node, else the first span child of
     a top-level placeholder; uses its name and service and the page's range.
     Hidden when the trace has no span.
  2. Spans mode table (Samples view): `query.Row` gains `spanRoot bool`
     (otel record with no `parent_span_id`); the `name` cell of such a row gets
     a small `∑` link titled `aggregate this operation`, opening with the
     row's name and `state.service`.
  3. Deep link: `?q=<json>&agg=<name>&aggsvc=<service>`; the range and (when
     `aggsvc` is absent) the service come from `q`. Opening pushes a history
     entry marked `{loghornAgg: 1}`; Close goes Back when that mark is
     present, else drops the parameters in place — the waterfall's pattern,
     so Close always returns to where the user came from (table or trace).
- **Loading:** `openAggregate` takes a sequence number and drops superseded
  answers; opening a trace from the view supersedes a pending aggregate load
  and vice versa. A 404 shows the server's message as the error banner and
  leaves the previous view.
- All names reach the DOM through `textContent`/attributes only.

### A10. Flame graph in v1: no

A true flame graph (d3-flame-graph or similar) would show the same merged
tree as icicle rectangles. The indented tree with share bars already shows
"where time goes" with exact numbers and reuses the waterfall's code; a flame
graph needs a vendored library (the page is embedded and offline) and its
own interaction design. Deferred.

## Deferred and discarded

| Item | Decision | Reason |
|---|---|---|
| True flame graph (d3-flame-graph) | deferred | Tree + share bars answer v1's question; needs a vendored library and its own UX. |
| Trace diff vs p50 tree | deferred | Needs alignment of one trace onto the merged tree; separate feature built on this one. |
| Keyboard navigation (↑ ↓ ← →) in waterfall and aggregate | deferred | Mouse covers v1; do both views in one accessibility pass. |
| Span event attribute cap | deferred | User asked to skip for now. |
| Cross-service aggregation nuances (splitting a merged node by service, per-service shares) | deferred | v1 keys by name only and lists the node's services; splitting doubles the tree for little gain on single-service data. |
| Aggregating by a non-root span (sub-operation) | deferred | v1 answers per request; nested operations need a different "per trace" denominator. |
| Entry from the grouped Table/Graph views | deferred | Rows there are groups, not records; the Samples row and the waterfall cover entry. |
| `spanRoot` for remote-parent roots in the table | discarded | Needs a session-wide span-ID lookup per query; the waterfall button reaches those operations. |
| Server-side cache of answers | discarded | Two passes meet the budget; the page caches the last answer for Back. |
| Name patterns / regex operations (e.g. `GET /orders/*`) | deferred | Exact names first; route templating is the instrumentation's job. |
| Histogram/sparkline of durations per node | deferred | p50/p95 answer "always like this?" in v1. |
| Critical-path frequency per node | deferred | Useful, but one more metric to explain; revisit after use. |
| Logs in the aggregate | discarded | Log lines do not merge meaningfully by name path. |
| Choosing a sample other than the most recent N | deferred | Most recent is predictable and matches "is it like this now". |

## Testing

- `internal/trace`: fixture `testdata/web/aggregate.jsonl` (generic services
  `api-server`, `pricing`) — five `GET /orders` traces with serial,
  short-run and parallel `SELECT orders`, a cross-service child, an error
  root, a remote-parent root, an orphan span, plus a `GET /users` trace holding
  a nested `GET /orders`. Exact expected numbers are in the plan. Plus cap,
  range, service, duplicate roots in one trace, no match.
- `internal/query`: `Row.SpanRoot`.
- `internal/web`: `/api/aggregate` success, 400s, 404, via
  `httptest.NewServer`.
- Browser check with the demo over the fixture: deep link, both entry points,
  jump-off and Back, Close, cap/outside notes.

## Open questions

None.
