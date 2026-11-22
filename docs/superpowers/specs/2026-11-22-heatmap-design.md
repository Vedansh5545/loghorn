# Latency heatmap view — design

## Intent

Add a fourth explore view, **Heatmap**, beside Samples, Table and Graph, modelled
on Cloud Trace's latency heatmap. It answers "when were calls slow, and which
ones?": time on x, span duration on a log y-axis, colour = number of spans in
the cell. Clicking a cell narrows a span list below the chart to that time
slice and duration band, with a **Grouped** tab that summarises the same spans
by name and service. It is not a kind of graph: it plots counts only and has no
metrics, order or top.

By default it covers **every span kind**, not one stream.

## Goals

- Heatmap of span count by (time bucket × log-duration band) in Spans mode.
- A span list below the chart, behind a draggable splitter, with two tabs:
  - **Spans**: the matching spans (id, name, service, start, duration, status),
    sorted by duration descending; a span opens the trace view.
  - **Grouped**: one row per (span name, service): count, p50, p90, p95 of
    duration, error rate; sorted by p90 descending; a row opens the aggregate
    view for that name.
- Clicking a cell: outline it, dim every other cell, narrow both tabs to that
  cell, and show a banner above the tabs — "Cell selected: <time>, Spans: N,
  Duration: 1.6ms – 2.5ms" — with **Clear selection**. Clicking the selected
  cell again, Clear selection or Esc restores the full range.
- Hovering a cell shows its time slice, duration band and count.
- Legend under the axis: "Number of spans 0 [scale] max".
- Existing range, filters and search apply to the heatmap and to both tabs.
- **All kinds** becomes a real stream (`*`) in Spans mode, usable in Samples,
  Table and Graph too.

## Non-goals

- Heatmap for logs (no duration). The Heatmap tab shows only in Spans mode.
- A y-axis on any field other than duration; a selectable colour metric.
- Selection persisted in the URL or in favorites (view, range, filters and
  bucket are saved as for the other views; the selected cell is not).
- Multi-cell or brush selection.

## Anti-goals

- Binning in the browser from raw spans (does not scale).
- A second, parallel filter/sort/percentile implementation for the heatmap's
  list — it must reuse the samples and table query paths.

## Approaches

1. **All kinds as a stream (`*`), heatmap as `view: "heatmap"`** — *chosen.*
   The query engine and schema inference accept stream `*` meaning "every
   stream of this source". The heatmap is one more view of `query.Run`. The
   Spans tab is an ordinary samples query with the cell's time slice as the
   range and duration bounds as filters; Grouped is an ordinary table query.
   Bonus: All kinds works in every view. Cost: a merged schema across kinds,
   and new p90 / error-rate metrics.
2. Separate `/api/heatmap` endpoint returning grid, cell spans and grouped rows.
   Self-contained but duplicates filtering, sorting and percentiles, and the
   list would miss column, copy and filter features. Rejected.
3. Browser-side binning from raw durations. Does not scale. Rejected.

## Design

### Server: stream `*`

- `query.Run` (`internal/query/run.go`): stream `*` matches every record whose
  source matches; `StreamTotal` counts them.
- `schema.Infer`: stream `*` infers over every stream of the source. Fields
  whose types differ between kinds follow the existing mixed-type rule.
  The server's schema cache keys on it like any other stream. Saved field
  settings are per source already, so they apply unchanged.
- `/api/kinds` is unchanged; the page adds the All kinds row itself (count =
  sum of the kinds) at the top of the Spans kind list.
- Pins: `*` cannot be pinned (it is always first). Favorites may store
  `stream = "*"`.
- Clash: an unclassified span's stream is its own name (`record/span.go`), so
  a span literally named `*` would collide. Classification sends such a span to
  the generic `span` stream instead. *(builder's call)*

### Server: heatmap view

`Query.View` accepts `"heatmap"`. `Bucket` drives the x-axis exactly as in the
graph (`chooseBucket`, `bucketStart`, range clipping from `runGraph` — factor
the shared x-axis code out rather than copy it). Validation rejects the view
for the log source.

Y-axis: `duration_ms` on a log scale with **5 bands per decade**, edges
`10^(k/5)` ms (… 1, 1.58, 2.51, 3.98, 6.31, 10 …), matching the reference
("1.6ms – 2.5ms"). The band range spans from the decade below the smallest
duration in range to the decade above the largest (at least one decade).
Durations ≤ 0 fall in the lowest band; spans with no `duration_ms` and records
with no time are left out and counted in `skipped`.

Binning reads the raw numeric `duration_ms` value of each record (as
`metricValue` does), not the schema type, so an `int`-inferred or retyped
field still bins.

Response (in `Result`, like `graph`):

```json
"heatmap": {
  "bucket": "1m", "bucketSeconds": 60,
  "starts": ["…"],                       // as Graph.Starts
  "bands": [{"lo": 1, "hi": 1.585}, …], // ms, ascending; lo inclusive, hi exclusive
  "counts": [[0, 3, …], …],              // counts[timeIndex][bandIndex]
  "max": 52, "total": 812, "skipped": 4
}
```

### Server: new metrics

- `p90` joins `AggregateOps` (same percentile code as p50/p95/p99).
- `error_rate`: fraction (0–1) of records in the group whose `severity` is
  ERROR or higher. For spans, `record/span.go` derives `severity = ERROR` from
  `status == "error"` (which `internal/otlp/spans.go` writes from the OTLP
  status code), so one rule covers spans and logs. It takes no field, like
  `count`; the page shows it as a percentage. *(builder's call: severity
  rather than status, so it also means something for logs)*
- Both are offered in the page's Metrics, Plot and Order controls for Table
  and Graph, not only in Grouped. The metric UI, `plotKey`, `syncAggregates`,
  `validateExplore` and `rankGroups` learn that `error_rate` has no field.
  *(builder's call)*

### Server: cell selection

`Query` gains `Cell *Cell` (JSON `cell`, not saved: `toml:"-"`), with
`From`, `To` (times) and `Lo`, `Hi` (ms; `Lo` omitted for the lowest band).
`query.Run` applies it **after** the user's range, filters and search, as
`From <= time < To` (half-open, unlike `Range.To`) and `Lo <= duration_ms <
Hi` on the raw number. The page clips `From` to the user's range start, since
the first bucket can begin before it. This keeps the lists consistent with the
cell's count and avoids filter validation against an `int`-typed field.

### Page

- A fourth view tab **Heatmap** (line-drawing icon matching the others), shown
  only in Spans mode; switching to Logs while on Heatmap falls back to Samples.
- Entering Heatmap selects the **All kinds** stream; picking a single kind in
  the kind list narrows it. Leaving Heatmap keeps whatever stream is selected.
  A deep link or favorite restores the view and stream exactly as saved.
  *(builder's call: simplest predictable rule)*
- Sidebar in Heatmap: range, filters, search and Bucket (`data-views` includes
  `heatmap`); group by, metrics, order, top, plot and chart are hidden.
- Layout: chart on top, splitter (move `initSplit`/`moveSplit` out of
  `trace.js` into a shared module parameterised by element and storage key,
  and use it from both), banner,
  Spans | Grouped tabs, list. Default split ~55% chart.
- Chart drawn in a new module `heatmap.js` (like `graph.js`), SVG, x-axis time
  labels in local time, y-axis decade labels (100us, 1ms, 10ms, 100ms, 1s, 10s)
  on the right as in the reference, sequential single-hue colour scale (light
  → dark) with zero cells empty; follow the `dataviz` skill for the palette in
  light and dark themes.
- Selection: outline the cell, dim others (lower opacity), banner, and re-run
  the list query with the `cell` described above.
- The lower pane *is* the view's single result area (`#head`, `#body`,
  footer, Load next), so the existing renderers and paging are reused rather
  than refactored to take targets. Only the active tab's query runs; switching
  tab re-runs it. *(builder's call)*
- Spans tab: a samples query, sorted `duration_ms` desc by default, paged with
  the existing Rows / Load next controls, rendered with the existing samples
  table (columns, copy, record detail, trace link). It shares `state.columns`
  and `state.sort` with the Samples view, so a column change carries over and
  a heatmap favorite saves them as Samples does. *(builder's call)*
- Grouped tab: a table query, groupBy `[name, <service field>]`, aggregates
  `p50, p90, p95 of duration_ms, error_rate`, order p90 desc; rendered with the
  existing table renderer. Its column menu hides "group by" and "filter by"
  (the grouping is fixed). Clicking a row's name selects the Spans tab with a
  `name = <that name>` filter added (and the service filter, if not already
  narrowed), keeping the cell selection. *(decided with the user)*
- Every new loader follows the sequence-number rule in `CLAUDE.md`.

## Constraints

- Follow `CLAUDE.md` web-view notes: rebuild after static edits, `[hidden]`
  rule, `.sb-field` rows with `data-views`, httptest servers, `json.Number` in
  test records, temp `HOME` for tests and demos, loader sequencing, deep-link
  loads after the table query.
- Work happens in the `heatmap` worktree (`.claude/worktrees/heatmap`).
- loghorn is public: no private app names in code, tests or docs.

## Left to the builder

- Exact name of the service field in span records and the column set the Spans
  tab shows by default under `*`.
- Colour step count and exact palette.
- Tooltip styling; banner wording beyond the content above.
- How the shared x-axis code is factored between graph and heatmap.
