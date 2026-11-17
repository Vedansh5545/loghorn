# Web explore view — Samples, Table, Graph — design

Date: 2026-11-17
Status: approved in conversation (user: "Go ahead and implement it")
Builds on: `2026-11-17-web-query-design.md`

## Intent

The web query view (v1) only lists matching rows. To answer Scuba-style
questions — "p95 latency per table", "when did errors start, and for which
route?" — the page needs grouping and aggregation, shown as a table or as a
graph over time. The query controls move into a left sidebar, as in Scuba, so
results get the full width.

## Decided by the user

- Three modes: **Samples** (today's rows), **Table** (one row per group,
  aggregate columns), **Graph** (aggregates over time).
- **Group by** applies to both Table and Graph.
- **Filters move to a left sidebar** as field / operator / value(s) triples,
  ANDed together. A new empty triple appears every time a triple is filled.
- **Graph's x axis is always time.** A granularity dropdown picks the bucket
  size; **auto** picks one that gives a reasonable number of x positions.
- Proposals played back and accepted ("Go ahead and implement it"):
  - The sidebar holds every query control; search stays above the results.
  - Results re-run automatically when a triple completes or any other control
    changes. No Run button (queries over a few MB take milliseconds). An
    `in`/`not in` triple is complete once it has one value.
  - Metrics: count is always on; add avg / sum / min / max / p50 / p95 / p99
    of any number field. Table shows them all as columns, largest count
    first. Graph plots one metric (a picker), one line per group.
  - Graph shows the top 10 groups plus an "other" line. Empty buckets plot 0
    for count, a gap for avg and percentiles.
  - Granularity choices: auto, 1s, 5s, 10s, 30s, 1m, 5m, 15m, 1h, 6h, 1d;
    auto = the smallest that keeps the graph at or under ~100 points.
  - Mode, group-by, metrics and granularity are part of the query, so the URL
    and saved favorites keep them.

## Goals

- Group by zero or more fields of any type; aggregate number fields.
- Table and Graph from the same group/aggregate query.
- Sidebar triples replace the chip strip, the "+ filter" popover and
  double-click editing; a triple is edited in place.
- Everything stays in the single Go binary with the embedded page.

## Non-goals

- OR / nested filters; HAVING-style filters on aggregates.
- Graph x axes other than time; bar/stacked charts; zooming the graph.
- Sorting Table by a chosen column (count, largest first, only).
- Live-updating results (Refresh stays).
- Cross-stream queries.

## Anti-goals

- **Hiding data**: "other" keeps graph totals honest; a record whose group
  value is empty or bad is grouped under `(empty)`, never dropped.
- A chart library from a CDN: the page must work offline from the binary.
- A second toolchain (npm/Vite) for the page.

## Approaches

1. **Aggregate in Go, in the existing engine (chosen).** `query.Run` gains a
   group/aggregate stage between filter and sort/limit, as v1 reserved. Exact
   percentiles by sorting each group's values. A few hundred lines; no
   dependencies.
2. **Embedded DuckDB.** Aggregation in SQL, but cgo, ~40 MB, and copying a
   live, per-stream-shaped store into tables while keeping two type systems
   in sync. Rejected (as in v1) — worth revisiting only for free-form SQL,
   cross-stream joins, or millions of rows.
3. **Aggregate in the browser.** Would mean shipping every matched record to
   the page; the row limit and the server-side typing argue against it.

Graph rendering — Claude's call: **hand-drawn SVG** in `app.js` (axes, ticks,
lines, legend, hover readout). A vendored library (e.g. uPlot) would be
faster to start but adds a third-party file to embed and theme; the chart
needed is small.

## Design

### Query model (additions)

```json
{
  "view": "samples" | "table" | "graph",
  "groupBy": ["databaseContext.metadata.tableName"],
  "aggregates": [{"op": "p95", "field": "databaseContext.metadata.latency"}],
  "plot": {"op": "p95", "field": "databaseContext.metadata.latency"},
  "bucket": "auto"
}
```

- `view` defaults to `samples`. `groupBy`, `aggregates`, `bucket` and `view`
  already exist as reserved fields in `query.Query`; `plot` is new.
- `aggregates` never lists `count`: count is always computed and is always the
  first metric column. Ops: `sum avg min max p50 p95 p99`, each needing a
  field of type int, float or duration. Validation rejects other ops, a
  missing field, or a non-number field.
- `plot` is `{"op":"count"}` (the default) or one of `aggregates`.
- `bucket`: `auto` (default) or one of `1s 5s 10s 30s 1m 5m 15m 1h 6h 1d`.

### Engine (`internal/query`)

Filter, range and search are unchanged. Then, by view:

- **samples**: as today (sort, limit, cells).
- **table**: group matched records by the tuple of group-by values (each
  value rendered as its display string; empty or bad → `(empty)`). Per group:
  count, and each aggregate over the group's non-empty, non-bad values
  (exact; percentile = nearest rank). A group with no values for a metric
  gets an empty cell. Rows sorted by count descending, then by group key;
  `limit` caps groups (`Truncated` set). With no group-by, one row: the
  totals. Result columns: one per group-by field (its schema type and
  visualizer), then `count` (int), then each aggregate. An aggregate's
  column type follows its field (duration stays duration so it renders in
  the field's unit), except `avg` of an int, which is float. Rows carry no
  `seq` (not expandable).
- **graph**: records with no time are left out (the table still counts them).
  Rank groups by count; the top 10 get their own series and every other
  record is aggregated together as one `other` series. Bucket size from
  `bucket`; `auto` picks the smallest size giving at most ~100 buckets over
  the x range. The x range is the query range clipped to the first and last
  matched record times, so a replay is not stretched to "now". Buckets
  align to multiples of the size since the Unix epoch, except 6h and 1d,
  which align to local midnight. Over 2,000 buckets is an error ("pick a
  coarser granularity"). The result carries the bucket start times, the
  chosen bucket size, the plotted metric's column (type/unit for
  formatting) and the series: label, group values, and one value per bucket
  (`null` where a non-count metric has no data; 0 for count).

The HTTP API is unchanged: `POST /api/query` returns the shape for the
query's view. Favorites store the new fields (TOML) like the others.

### Page (`internal/web/static`)

```
 loghorn · live run since 11:02 · 4,770 records                    [Favorites ▾]
┌──────────────────────────────┬──────────────────────────────────────────────────┐
│ [Samples] [Table] [Graph]    │  Search [______________]  [Refresh]               │
│ Stream  [db ▾]               │                                                   │
│ From [this run ▾] To [now ▾] │   Samples: rows, as today                         │
│ Filters                      │   Table: tableName │ count │ p95 latency          │
│  tableName ▾ in ▾ lots,bids ×│   Graph: lines over time, legend below            │
│  latency   ▾ >  ▾ 50ms     × │                                                   │
│  [field ▾] [op ▾] [value]    │                                                   │
│ Group by  [tableName ×][+]   │  (Table, Graph)                                   │
│ Metrics   count · p95 latency│  (Table, Graph)                                   │
│ Plot [p95 latency ▾]         │  (Graph)                                          │
│ Granularity [auto ▾]         │  (Graph)                                          │
│ Columns [edit] [restore]     │  (Samples)                                        │
└──────────────────────────────┴──────────────────────────────────────────────────┘
 312 of 4,012 db records match · …                  [Save as favorite] [Schema…]
```

- The sidebar is a fixed-width column (~300px); under ~800px wide it stacks
  above the results.
- **Filter triples**: field (a searchable picker of the stream's fields),
  operator (valid ops for the field's type), value input by type (text,
  bool select, datetime-local, or the tag input with seen-value suggestions
  for `in`/`not in`; none for `empty`/`not empty`). A triple joins the query
  when complete; the server's validation error for it shows on that triple,
  and it stays out of the query until fixed. There is always one empty
  triple at the end. `×` removes a triple.
- **Group by**: fields as removable tokens plus an add picker.
- **Metrics**: `count` (fixed) then rows of [op ▾][number field ▾] ×, plus
  "+ metric".
- Samples keeps its column features (resizable, trash, restore, wrapping
  headers); the Columns control moves into the sidebar. Table uses the same
  table rendering and visualizers, without row expansion. The footer reads
  "N groups · M records match" in Table.
- **Graph**: SVG, full results width. Local-time x ticks, "nice" y ticks
  formatted by the metric's type (durations via the existing formatter),
  one colour per series from a fixed categorical palette with grey for
  `other`, a legend under the chart, and a hover readout (bucket time and
  each series' value). Colours come from CSS tokens with light and dark
  values.

## Left to the builder

- Exact sidebar styling, palette values, tick algorithm, hover details.
- How the field picker is built (datalist input or filtered list).
- Whether Table/Graph share one request path in `app.js`.

## Testing

- Go unit tests for grouping, each aggregate (including nearest-rank
  percentiles and `(empty)` groups), table truncation, bucket choice and
  alignment, top-10 + other, null vs 0 fill, the 2,000-bucket error, and
  validation of the new fields; a server test that a table and a graph query
  round-trip over HTTP.
- Each page task is checked in a browser (Playwright) against
  `go run ./internal/web/demo testdata/web/*.log testdata/web/gcloud.json`.
  Never pipe input into loghorn to test.
