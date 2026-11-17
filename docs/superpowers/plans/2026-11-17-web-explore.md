# Web Explore (Samples / Table / Graph) Implementation Plan

> **For agentic workers:** Execute with the `orchestrating-plans` skill (one task-runner subagent per task, each dispatching its own fresh reviewer). Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add Table (grouped aggregates) and Graph (aggregates over time) modes beside today's Samples, and move every query control into a Scuba-style left sidebar with inline filter triples.

**Architecture:** `internal/query.Run` gains a group/aggregate stage after filter/range/search; the result shape depends on `Query.View`. The embedded page (`internal/web/static`) gets a sidebar, a Table renderer reusing the samples table code, and a hand-drawn SVG graph in a new `graph.js` module.

**Tech Stack:** Go 1.x standard library; vanilla ES modules, no build step, no third-party JS.

**Spec:** `docs/superpowers/specs/2026-11-17-web-explore-design.md` (read it first; also `2026-11-17-web-query-design.md` for v1).

## Global Constraints

- No new Go modules, no cgo, no npm/Vite; no CDN or third-party JS — the page must work offline from the binary.
- All log data reaches the DOM via `textContent`/attributes, never `innerHTML` (header comment of `app.js`).
- Keep `[hidden]{display:none!important}` in `app.css`.
- After editing anything in `internal/web/static/`, rebuild / re-run the demo before checking in a browser (files are `go:embed`ed).
- Never pipe input into loghorn to test. Browser checks use the demo server: `go run ./internal/web/demo -port 8797 testdata/web/gcloud.json testdata/web/pino.log` (run in background; stop it afterwards).
- Handler tests use `httptest.NewServer` (the Host guard needs a local address).
- Aggregate ops: `sum avg min max p50 p95 p99`; count is implicit and always first. Number field types: `int`, `float`, `duration`.
- Buckets: `auto 1s 5s 10s 30s 1m 5m 15m 1h 6h 1d`; auto target ≤ 100 buckets; > 2000 buckets is an error with the message `too many time buckets; pick a coarser granularity`.
- Graph: top 10 groups by count + one `other` series. Empty bucket: 0 for count, `null` otherwise.
- Empty/bad group value → group label `(empty)`; never dropped.
- Commit messages end with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.
- Full checks before each commit: `gofmt -l . && go vet ./... && go test ./... && node --check internal/web/static/*.js`.

## Review Focus

1. **NaN/Inf in JSON** — a metric over a group whose values are all empty/bad must produce an empty cell / `null`, never NaN (which makes `json.Marshal` fail and the whole query 500). Test: `TestTableMetricAllEmptyIsNull` (Task 1), `TestGraphResultMarshals` (Task 2).
2. **Invalid triple value** (e.g. `abc` for an int field) must show its error on that triple and leave the other triples' results showing. Browser check in Task 3.
3. **Stream change** with group-by, metrics or triples naming fields the new stream lacks — they are cleared, as filters are today. Browser check in Task 4.
4. **No plottable data** (zero matches, or every match lacks a time) — Graph shows a "nothing to plot" message, not an empty/NaN SVG. Test: `TestGraphNoTimedRecords` (Task 2); browser check in Task 5.
5. **Old URLs and favorites** without `view`/`plot` load as Samples; a table/graph favorite round-trips through the TOML store. Test: `TestFavoriteRoundTripsExploreFields` (Task 1).

---

### Task 1: Engine — query fields, validation, Table aggregation

**Review: full** (aggregation math). **Model: sonnet.**

**Files:**
- Modify: `internal/query/query.go` (Query, Validate)
- Create: `internal/query/aggregate.go`
- Modify: `internal/query/run.go` (Run dispatches by view; Column, Result)
- Test: `internal/query/aggregate_test.go`; favorites round-trip test in the favorites package's existing test file (`internal/favorites/*_test.go`)

**Interfaces:**
- Produces (Go, used by Task 2):
  - `Query.Plot *Aggregate` (`json:"plot,omitempty" toml:"plot,omitempty"`). `View`, `GroupBy`, `Aggregates`, `Bucket` already exist — update their comments (no longer "ignored").
  - `var AggregateOps = []string{"sum","avg","min","max","p50","p95","p99"}`, `var Buckets = []string{"auto","1s","5s","10s","30s","1m","5m","15m","1h","6h","1d"}`.
  - `func (q Query) view() string` → `"samples"` when empty.
  - `Column` gains `Group bool \`json:"group,omitempty"\`` (a group-by column in Table).
  - `Result` gains `View string \`json:"view"\`` and `Groups int \`json:"groups,omitempty"\`` (Table: total groups before the limit).
  - `func groupKey(r *record.Record, fields []schema.Field) (key string, labels []string)` — labels are display strings, `(empty)` for empty/bad; key joins labels with `"\x00"`.
  - `func metricColumn(a Aggregate, s schema.Schema) Column` — count: `{Path:"count", Label:"count", Type:int, Visualizer:"number"}`; else Path `op + "(" + field + ")"`, Label `op + " " + (field label or path)`, Type = field type except `avg` of int → float; Unit and Visualizer from the field (int/float metric with a non-number visualizer → `"number"`).
  - `func metricValue(recs []*record.Record, a Aggregate, f schema.Field) (float64, bool)` — false when no non-empty, non-bad values.
  - `func percentile(sorted []float64, p float64) float64` — nearest rank: `sorted[ceil(p/100*n)-1]`.
- Produces (JSON, used by Tasks 3–5): Table result = `{view:"table", columns:[group cols (group:true)…, count, metrics…], rows:[{seq:0, cells}], matched, groups, truncated, streamTotal, from, to}`.

- [ ] **Step 1: Write failing tests** in `aggregate_test.go` using the existing `rec`/`sch`/`run` helpers:
  - `TestValidateExploreFields`: errors for `view:"pie"`, aggregate op `"median"`, aggregate with no field, aggregate on a string field, `bucket:"7m"`, `plot` not `count` and not in `aggregates`; OK for a full valid table query.
  - `TestTableGroupsByCountDesc`: 3 recs `route=a`, 1 `route=b` → rows `[a,3]`, `[b,1]`; `Groups==2`; columns `route`(group) then `count`.
  - `TestTableNoGroupByIsTotals`: no groupBy → one row, count = matched.
  - `TestTableEmptyGroupValue`: a rec missing `route` → its row's group cell is empty (`V==nil`) and it is counted, not dropped.
  - `TestTablePercentileNearestRank`: latency (float) 1..10 → p50=5, p95=10, p99=10; avg=5.5; sum=55; min=1; max=10.
  - `TestTableDurationMetricKeepsType`: p95 of a `duration` field → column Type `duration`, Unit copied from the field.
  - `TestTableAvgOfIntIsFloat`.
  - `TestTableMetricAllEmptyIsNull`: group whose latency values are all missing → metric cell `V==nil`; `json.Marshal(res)` succeeds.
  - `TestTableLimitTruncatesGroups`: 5 groups, `limit:2` → 2 rows, `Groups==5`, `Truncated`.
  - `TestSamplesUnchangedWhenViewEmpty`: existing samples behaviour with `View:""` sets `res.View=="samples"`.
  - In the favorites tests, `TestFavoriteRoundTripsExploreFields`: save/load a query with view `table`, groupBy, aggregates, plot, bucket → equal.

- [ ] **Step 2: Run** `go test ./internal/query/ ./internal/favorites/` — expect FAIL (undefined fields/functions).

- [ ] **Step 3: Implement.** Validate checks: view ∈ {"",samples,table,graph}; each aggregate op ∈ AggregateOps with a field whose type is int/float/duration; bucket ∈ Buckets or ""; plot nil, `{op:"count"}`, or equal to one of `aggregates`; groupBy entries non-empty. `Run` keeps filter/range/search as is, sets `res.Matched`, then: samples → existing path; table → `runTable(matched, s, q, &res)` in `aggregate.go` (group, sort by count desc then key asc, limit groups, build columns/cells; group cells via the existing `cell(raw, f)` of the group's first record, or `Cell{}` for `(empty)`; metric cells: int-typed metric → int64 cell (exact path as `cell`), others float; never NaN/Inf); graph → returns `res` with `View:"graph"` only (Task 2 fills it).

- [ ] **Step 4: Run** `go test ./internal/query/ ./internal/favorites/ ./internal/web/` — PASS.

- [ ] **Step 5: Full checks, commit** `feat(query): group-by and aggregates for the table view`.

---

### Task 2: Engine — Graph time series

**Review: full** (bucketing/time math). **Model: sonnet.**

**Files:**
- Create: `internal/query/graph.go`
- Modify: `internal/query/run.go` (graph dispatch; Result.Graph)
- Test: `internal/query/graph_test.go`; `internal/web/server_test.go` (HTTP round trip for table + graph)

**Interfaces:**
- Consumes: Task 1's `groupKey`, `metricColumn`, `metricValue`, `Buckets`, `Query.Plot`.
- Produces:
  ```go
  type Graph struct {
      Bucket        string      `json:"bucket"`        // chosen size, e.g. "1m" (never "auto")
      BucketSeconds float64     `json:"bucketSeconds"`
      Starts        []time.Time `json:"starts"`        // bucket start times, ascending
      Metric        Column      `json:"metric"`
      Series        []Series    `json:"series"`        // by count desc; "other" last
  }
  type Series struct {
      Label  string     `json:"label"`  // group labels joined " · "; "all" with no group-by; "other"
      Group  []string   `json:"group"`
      Other  bool       `json:"other,omitempty"`
      Count  int        `json:"count"`
      Values []*float64 `json:"values"` // len == len(Starts); nil = no data
  }
  ```
  `Result.Graph *Graph \`json:"graph,omitempty"\``.
  - `func chooseBucket(name string, span time.Duration) (time.Duration, string, error)` — auto: smallest size with `ceil(span/size) <= 100` (span 0 → 1s; none fits → 1d); explicit: that size, error if `> 2000` buckets.
  - `func bucketStart(t time.Time, size time.Duration, loc *time.Location) time.Time` — sizes ≤ 1h: `t.Truncate(size)`; 6h and 1d: local midnight of `t` in `loc` plus whole multiples of size.
  - `func runGraph(matched []*record.Record, s schema.Schema, q Query, from, to time.Time, loc *time.Location) (*Graph, error)`; `Run` calls it with `time.Local`.

- [ ] **Step 1: Write failing tests** in `graph_test.go` (use `time.FixedZone("X", -8*3600)` for loc):
  - `TestChooseBucketAuto`: span 10m → `10s` (60 buckets; `5s` would be 120); 2h → `5m` (24; `1m` would be 120); 90s → `1s`; 0 → `1s`; 30 days → `1d`.
  - `TestChooseBucketTooMany`: `"1s"` over 1h → error containing `pick a coarser granularity`.
  - `TestBucketStartAlign`: 1m truncates seconds; 1d in the −8h zone aligns to local midnight, not UTC.
  - `TestGraphCountFillsZero`: recs at t0 and t0+3m, bucket `1m`, no group-by → one series `all`, Values `[1,0,0,1]`, `len(Starts)==4`, `Starts[0]==t0`.
  - `TestGraphAvgGapIsNull`: same with plot `avg(latency)` → middle values nil.
  - `TestGraphTopTenPlusOther`: 12 groups (group i has i+1 records) → 11 series, first is the largest, last has `Other:true`, Label `other`, Count = records of the 2 smallest groups.
  - `TestGraphRangeClippedToData`: query range `all` with to=now far after the data → last Start is the last record's bucket.
  - `TestGraphNoTimedRecords`: all matched recs have zero Time → `Graph.Series` empty, no error.
  - `TestGraphResultMarshals`: result with nil values and a metric whose group has no values → `json.Marshal` OK.
  - In `server_test.go`, `TestQueryTableAndGraphOverHTTP`: POST a table query and a graph query via `httptest.NewServer`; 200, `view` echoed, graph has `series`.

- [ ] **Step 2: Run** `go test ./internal/query/ ./internal/web/` — FAIL.

- [ ] **Step 3: Implement** `graph.go`. x range = `[max(from, first timed match), min(to-or-now, last timed match)]`. Rank groups by count over the whole range; top 10 own series, the rest pooled into `other` and aggregated as one group (not summed from series, so percentiles stay true). Per series per bucket: count, or `metricValue` over that bucket's records (nil when false). Records with zero Time are skipped.

- [ ] **Step 4: Run** `go test ./internal/query/ ./internal/web/` — PASS.

- [ ] **Step 5: Full checks, commit** `feat(query): graph time series with bucketing and top-10 groups`.

---

### Task 3: Page — sidebar layout, mode tabs, filter triples

**Review: standard. Model: sonnet.**

**Files:**
- Modify: `internal/web/static/index.html`, `app.css`, `app.js`

**Interfaces:**
- Consumes: v1 page code (`state`, `runQuery`, `render`, `applyQuery`, `schemaFields`, `distinctValues`, the tag input, `localInputValue`, the column editor).
- Produces (JS, used by Tasks 4–5):
  - `state.view` (`"samples"|"table"|"graph"`, default `"samples"`), `state.groupBy` (`[]`), `state.aggregates` (`[]`), `state.plot` (`{op:"count"}`), `state.bucket` (`"auto"`), all handled by `applyQuery` defaults.
  - `function setView(v)` — switches tabs, toggles `[data-views]` sidebar sections (attribute lists the views a section shows in, e.g. `data-views="table graph"`), runs the query.
  - `function renderResult(result)` — dispatches on `result.view`: `"samples"` → existing `render`; `"table"` / `"graph"` → stubs Tasks 4/5 replace (`renderTable`, `renderGraph`).
  - `<aside id="sidebar">` sections with ids `sb-view`, `sb-stream`, `sb-range`, `sb-filters`, `sb-group` (`data-views="table graph"`), `sb-metrics` (`data-views="table graph"`), `sb-plot` (`data-views="graph"`), `sb-bucket` (`data-views="graph"`), `sb-columns` (`data-views="samples"`); results area `#results` contains `#table` and an empty `<div id="graph" hidden>`.

- [ ] **Step 1: Layout.** Two-column grid: sidebar ~300px, results fill the rest; below 800px the sidebar stacks above. Move Stream, From/To and the Columns control (edit + restore defaults + column panel) into the sidebar; Search and Refresh stay above the results. Mode tabs (Samples / Table / Graph) at the top of the sidebar.
- [ ] **Step 2: Filter triples.** Replace the chip strip, `#filter-add` popover and double-click editing with a list of triple rows in `#sb-filters`: field picker (input + `datalist` of the stream's field paths, or a filtered list — builder's call), operator `select` from `schemaData.ops[type]`, value input by type (reuse the tag input for `in`/`not-in`, none for `empty`/`not-empty`), and `×`. A complete triple (field known, op chosen, value(s) present) is written into `state.filters` and the query re-runs; there is always exactly one empty triple at the end. A triple the server rejects shows the server message on that row and is left out of `state.filters` (validate by posting the query with it, as `addFilter` does today); other triples keep working. Editing a complete triple in place updates its filter.
- [ ] **Step 3: Browser check** with the demo server (Playwright): tabs switch and hide the right sections; add `severity in ERROR` → rows filter, a new empty triple appears; edit it to add `INFO`; `×` removes it; an int field with value `abc` shows the error on that triple only; the narrow (700px) layout stacks; resize/trash/restore still work in Samples. Screenshot both widths.
- [ ] **Step 4: Full checks, commit** `feat(web): sidebar with mode tabs and inline filter triples`.

---

### Task 4: Page — group-by, metrics and the Table view

**Review: standard. Model: sonnet.**

**Files:**
- Modify: `internal/web/static/index.html`, `app.css`, `app.js`

**Interfaces:**
- Consumes: Task 3's sidebar sections and `state` fields; Task 1's table JSON.
- Produces: `function renderTable(result)`; `function metricLabel(a)` (`"count"` or `op + " " + (label||path)`), used by Task 5's plot picker.

- [ ] **Step 1: Group by** (`#sb-group`): removable field tokens + add picker (same field picker as triples). **Metrics** (`#sb-metrics`): fixed `count`, then rows `[op ▾][number field ▾] ×` and `+ metric`; only int/float/duration fields offered. Any change re-runs. Switching stream clears groupBy, aggregates, plot (`{op:"count"}`), as filters are cleared today.
- [ ] **Step 2: `renderTable`**: reuse the samples table head/body code (visualizers, wrapping headers, resize) but no row expansion, no trash buttons; null group cells show a muted `(empty)`; footer `N groups · M records match` (+ ` · limit reached`).
- [ ] **Step 3: Browser check**: Table with no group-by shows one totals row; group by `severity` with `p95` of a duration/number field shows rows sorted by count; duration column renders in its unit; switching stream clears group-by/metrics; URL reload restores the table view.
- [ ] **Step 4: Full checks, commit** `feat(web): group-by, metrics and the table view`.

---

### Task 5: Page — Graph view

**Review: standard. Model: sonnet.** Load the `dataviz` skill before choosing colours.

**Files:**
- Create: `internal/web/static/graph.js`
- Modify: `internal/web/static/app.js`, `app.css`, `index.html`
- Modify: `README.md` (web view section: modes, sidebar, group-by, metrics, granularity)

**Interfaces:**
- Consumes: Task 2's `Graph` JSON; Task 4's `metricLabel`; `formatDuration`, `localTime` from `app.js` (export them, or pass a `format(v)` function in).
- Produces: `export function drawGraph(container, graph, format)` in `graph.js`; `renderGraph(result)` in `app.js` calls it.

- [ ] **Step 1: Controls.** `#sb-plot`: select of `count` + each aggregate (`metricLabel`), writes `state.plot`. `#sb-bucket`: select of the bucket list; when `auto`, show the chosen size next to it (`auto (1m)`) from `graph.bucket`.
- [ ] **Step 2: `drawGraph`**: SVG sized to the container width (redraw on resize), local-time x ticks, "nice" y ticks formatted by the metric's type (durations via `formatDuration` with the column unit), one polyline per series breaking at `null`s, 10 categorical colours from CSS tokens (light + dark) with grey for `other`, legend under the chart (label + count), hover readout: a vertical rule at the nearest bucket and a box listing bucket time and each series' value. Zero series or zero starts → a "nothing to plot in this range" message instead of an SVG.
- [ ] **Step 3: Browser check**: Graph with no group-by (one line), grouped by severity (several lines + legend), plot p95 of a number field, granularity auto vs `1s`; a `1s` choice over a long range shows the server's "pick a coarser granularity" error; a filter that matches nothing shows the empty message; dark mode (`emulateMedia colorScheme: dark`) is legible. Screenshots.
- [ ] **Step 4: README** web-view section updated.
- [ ] **Step 5: Full checks, commit** `feat(web): graph view over time`.
