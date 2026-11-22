# Aggregate Operation View Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use orchestrating-plans (the user's standing choice) to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Merge every trace of one operation (a root span name) in the page's range into one tree keyed by span-name path, with per-node frequency, p50/p95, time shares and serial-query statistics, served at `GET /api/aggregate` and shown in a new page view.

**Architecture:** `internal/trace/aggregate.go` selects operation roots in one pass over the store snapshot, groups the kept traces' span records in a second pass, builds each with the existing `trace.Build` (so self time and serial runs are reused) and merges the root subtrees. `internal/web/server.go` serves it. `internal/web/static/agg.js` renders it with the waterfall's row classes and connector guides and shares its history pattern.

**Tech Stack:** Go 1.25 (stdlib), vanilla JS ES modules embedded with `go:embed`, Playwright MCP for browser checks.

**Spec:** `docs/superpowers/specs/2026-11-21-trace-aggregate-design.md`

## Global Constraints

- Every task ends with: `gofmt -l $(git ls-files '*.go') internal/trace/` prints nothing, `go vet ./...` clean, `go test ./...` passes.
- Commit each task directly on `main`, one commit per task, staging only that task's files by path (never `git add -A`/`.`/`-u`: the tree has unrelated changes in `images/`, `.config/`, `.playwright-mcp/`, `docs/otel-schema.md`, `.superpowers/`). Messages end with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.
- **loghorn is public.** Fixtures, tests, docs and commits use only generic names: services `api-server`, `pricing`; spans `GET /orders`, `GET /users`, `GET /price`, `SELECT orders`, `SELECT prices`, `auth check`, `cache get`. Before each commit, check the staged diff for the maintainer's private app, service and project names (the repo memory lists them); none may appear.
- **Never pipe input into a loghorn binary.** Manual checks use the demo on a file argument.
- **Tests and demos never touch the real config:** Go server tests use `newFixture`; the demo runs as `env -u XDG_CONFIG_HOME HOME="$(mktemp -d)" go run ./internal/web/demo -port 8790 testdata/web/aggregate.jsonl testdata/web/trace.jsonl`.
- Values, verbatim: endpoint `GET /api/aggregate?name=&service=&from=&to=`; `AggCap = 1000`; name max 1024 bytes; errors `name is required`, `name too long`, `no root spans with that name in range`; node IDs `n<index>`; percentiles nearest-rank `sorted[ceil(p·n)−1]`; URL `?q=<json>&agg=<name>&aggsvc=<service>`; history mark `{loghornAgg: 1}`; toolbar button `Aggregate this operation`; table link text `∑` titled `aggregate this operation`; chip `<x.x> serial queries/trace, <y.y>%`; header `N traces` / `N of M traces (capped, most recent)`; outside note `K traces with M spans outside the root (missing parents)`, singular/plural via the waterfall's `plural` helper (`1 trace with 1 span …`).
- Page rules (CLAUDE.md): data reaches the DOM only via `textContent`/attributes; keep `[hidden]{display:none!important}`; every async loader takes a sequence number and drops a superseded answer; restart the demo after any `internal/web/static/*` edit; free port 8790 first (`lsof -nP -iTCP:8790 -sTCP:LISTEN`, kill by PID); handler tests use `httptest.NewServer`; hand-built numbers are `json.Number`.
- Out of scope (spec "Deferred and discarded"): flame graph, trace diff, keyboard navigation, span event attribute cap, per-service splitting, sub-operation aggregation, entry from grouped views, regex names, histograms.

## Review Focus

1. **Remote-parent and duplicate roots** — a `GET /orders` span whose `parent_span_id` names a span absent from the session must count as an operation root; one present as a child must not; two matching roots in one trace use the earliest and count the other's subtree as outside spans. Tests: Task 1 `TestAggregateFixture` (T5, T7), `TestAggregateTwoRoots`.
2. **Degenerate durations** — `opTotal` of 0 (all roots zero-length): shares must be 0, never NaN/Inf, and the JSON must encode. Test: Task 1 `TestAggregateZeroDuration`.
3. **Cap and range honesty** — more traces than the cap: the most recent are kept, `matched`/`capped` say so and the header shows `N of M traces (capped, most recent)`. Tests: Task 1 `TestAggregateCap`, `TestAggregateRange`; the browser cannot reach the cap with the fixture, so the Task 2 reviewer checks the header string against the answer fields.
4. **History round-trips** — table → aggregate → slowest trace → Back → Back: each step shows the right view, the table query survives, Close from the aggregate returns to the trace it came from. Browser check in Tasks 2 and 3.
5. **Untrusted span names** — a name such as `<b>x</b>` or one containing `&agg=` must render as text and round-trip through the URL (`encodeURIComponent`). Browser check in Task 2 (`?agg=%3Cb%3Ex%3C%2Fb%3E` shows the 404 banner as plain text); the Task 2 reviewer checks every DOM write in `agg.js`.

---

## File Structure

- Create `internal/trace/aggregate.go` — operation selection, merge, metrics (`Aggregate`, `Operation`, `OpNode`).
- Create `internal/trace/aggregate_test.go`, `testdata/web/aggregate.jsonl`.
- Modify `internal/web/server.go` (+ `server_test.go`) — `GET /api/aggregate`.
- Create `internal/web/static/agg.js` — the view.
- Modify `internal/web/static/trace.js` — export `typeGroup`, `treeGuides`; hide `#agg` on show; `Aggregate this operation` button.
- Modify `internal/web/static/app.js` — wire `initAggregate`, `saveURL` params, `∑` link on root span names.
- Modify `internal/web/static/index.html`, `app.css` — `#agg` section and styles.
- Modify `internal/query/run.go` (+ test) — `Row.SpanRoot`.
- Modify `README.md`, `docs/features.md` — one paragraph each.

## Fixture `testdata/web/aggregate.jsonl` (Task 1)

Span lines in the same shape as `testdata/web/trace.jsonl` (`trace_id`, `span_id`, `parent_span_id?`, `name`, `kind`, `start`, `end`, `duration_ms`, `status?`, `attributes`, `service.name`). Date `2026-11-17`, UTC. Root spans are `kind: server` with `attributes.http.request.method` (→ `http.server`); `SELECT …` spans are `kind: client` with `attributes.db.system.name: postgresql` (→ `db`); `GET /price` client is `kind: client` with `http.request.method` (→ `http.client`); `auth check` and `cache get` are `kind: internal`. Offsets are ms from the trace's root start. Trace IDs `a<k>` padded to 32 hex (`a1000000000000000000000000000000` …), span IDs `a<k>` + 14 hex.

| Trace | Root start | Root `GET /orders` (api-server) | Children (api-server unless noted) |
|---|---|---|---|
| T1 | 17:00:00 | 0–100 | `auth check` 2–12; `SELECT orders` 12–22, 22–32, 32–42 |
| T2 | 17:01:00 | 0–120 | `auth check` 2–12; `SELECT orders` ×6 back-to-back 12–72 |
| T3 | 17:02:00 | 0–80, `status: error` | `auth check` 2–12; `SELECT orders` 12–22, 30–40 |
| T4 | 17:03:00 | 0–200 | `auth check` 2–12; `SELECT orders` ×3 each 12–52 (parallel); `GET /price` (http.client) 60–160 → child `GET /price` (**pricing**, server) 65–155 → child `SELECT prices` (pricing) 70–90 |
| T5 | 17:04:00 | 0–100, `parent_span_id` `a5000000000000ff` (absent) | `auth check` 2–12; `cache get` 20–25 with `parent_span_id` `a5000000000000fe` (absent: an orphan) |
| T7 | 17:05:00 | `GET /users` 0–50 | `GET /orders` (internal) 5–30 — nested, never an operation root |

Expected merge for `name=GET /orders`, no service, open range (N = 5, opTotal = 600 ms):

| id | node | traces | calls | calls/trace | p50 | p95 | totalMs | selfMs | totalPct | selfPct | errors | serialCalls | serialPerTrace | serialPct | services |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| n0 | GET /orders | 5 | 5 | 1 | 100 | 200 | 600 | 300 | 100 | 50 | 1 | 0 | 0 | 0 | api-server |
| n1 | auth check | 5 | 5 | 1 | 10 | 10 | 50 | 50 | 8.333… | 8.333… | 0 | 0 | 0 | 0 | api-server |
| n2 | SELECT orders | 4 | 14 | 3.5 | 10 | 40 | 230 | 230 | 38.333… | 38.333… | 0 | 9 | 2.25 | 15 | api-server |
| n3 | GET /price | 1 | 1 | 1 | 100 | 100 | 100 | 10 | 16.666… | 1.666… | 0 | 0 | 0 | 0 | api-server |
| n4 | GET /price | 1 | 1 | 1 | 90 | 90 | 90 | 70 | 15 | 11.666… | 0 | 0 | 0 | 0 | pricing |
| n5 | SELECT prices | 1 | 1 | 1 | 20 | 20 | 20 | 20 | 3.333… | 3.333… | 0 | 0 | 0 | 0 | pricing |

Plus: `matched 5`, `capped false`, `outsideSpans 1`, `incompleteTraces 1`, `truncatedTraces 0`; `n0.slowest` = T4 root (200 ms); `n2.slowest` = T4's first `SELECT orders` by `Seq` (40 ms); `n2.type` `db`; parents: n1–n3 → n0, n4 → n3, n5 → n4.

---

### Task 1: Aggregate engine and `GET /api/aggregate`

Review: full (quantity math: percentiles, shares, serial counts) · Model: opus

**Files:**
- Create: `internal/trace/aggregate.go`, `internal/trace/aggregate_test.go`, `testdata/web/aggregate.jsonl`
- Modify: `internal/web/server.go` (route next to `GET /api/trace`, handler after `trace`), `internal/web/server_test.go`

**Interfaces:**
- Consumes: `trace.Build(recs []record.Record, traceID string, limit int) (Trace, bool)`, `record.TraceRef`, `record.NormalizeSpanID`, `service(r)`, `number`, `finite` (same package); `query.Range.Resolve(now, runStart)`.
- Produces (Go):

```go
const AggCap = 1000

type AggOptions struct {
	Name    string
	Service string    // "" or "*": every service
	From    time.Time // zero: open
	To      time.Time // zero: open
	Cap     int       // <= 0 means AggCap
}

type Slowest struct {
	TraceID    string  `json:"traceId"`
	SpanID     string  `json:"spanId"`
	DurationMs float64 `json:"durationMs"`
}

// Field sketch: one field per line in code; JSON tags are camelCase (spec A7).
type OpNode struct {
	ID, Parent string; Depth, Children int        // json: id, parent, depth, children
	Name, Type string; Services []string           // name, type, services
	Traces, Calls int; CallsPerTrace float64       // traces, calls, callsPerTrace
	P50Ms, P95Ms, TotalMs, SelfMs, TotalPct, SelfPct float64
	Errors, SerialCalls int
	SerialMs, SerialPerTrace, SerialPct float64
	Slowest Slowest
}

type Operation struct {
	Name, Service string                        // name, service
	From, To time.Time                          // from, to (`json:",omitzero"`)
	Traces, Matched int; Capped bool; Cap int   // traces, matched, capped, cap
	OpTotalMs float64                           // opTotalMs
	OutsideSpans, IncompleteTraces, TruncatedTraces int
	NodesTruncated bool
	Nodes []OpNode                              // pre-order; never null
}

// Aggregate merges the traces whose operation root (spec A3) is named o.Name.
// It reports false when no root matches.
func Aggregate(recs []record.Record, o AggOptions) (Operation, bool)
```

JSON tags are the camelCase names in the spec's A7 example.
- Produces (HTTP): `GET /api/aggregate?name=&service=&from=&to=` → 200 `Operation` JSON; 400 `name is required` / `name too long` / Resolve's error; 404 `no root spans with that name in range`.

- [ ] **Step 1: Write the fixture** `testdata/web/aggregate.jsonl` per the fixture table above (one span per line).

- [ ] **Step 2: Write failing tests** in `internal/trace/aggregate_test.go`, loading the fixture the way `trace_test.go` loads `trace.jsonl`:
  - `TestAggregateFixture`: `Aggregate(recs, AggOptions{Name: "GET /orders"})` → every value in the expected-merge table (floats compared within 1e-6), node order n0…n5 and parents, `matched 5`, `capped false`, `cap 1000`, `outsideSpans 1`, `incompleteTraces 1`, slowest of n0 and n2.
  - `TestAggregateCap`: `Cap: 2` → `Traces 2`, `Matched 5`, `Capped true`, `Cap 2`, n0 `Calls 2` and `TotalMs 300` (T5 + T4).
  - `TestAggregateRange`: `From: 2026-11-17T17:02:30Z` → `Traces 2`, `Matched 2`, `Capped false`; `To: 2026-11-17T17:00:30Z` → `Traces 1`.
  - `TestAggregateService`: `Service: "pricing"` → ok false; `"api-server"` and `"*"` → 5 traces.
  - `TestAggregateNoMatch`: `Name: "GET /users/x"` → ok false; `Name: "auth check"` (never a root) → ok false.
  - `TestAggregateTwoRoots`: hand-built records (`json.Number` durations) of one trace with two parentless `GET /orders` roots at 0 ms and 50 ms, each with one child → `Traces 1`, n0 `Calls 1`, `OutsideSpans 2`, `IncompleteTraces 1`.
  - `TestAggregateZeroDuration`: one trace, root and child both 0 ms → `TotalPct`/`SelfPct`/`SerialPct` 0 on every node, `json.Marshal` succeeds.
  - `BenchmarkAggregate`: 1 000 generated traces × 50 spans plus 200 000 unrelated log records; reports ns/op (not asserted).

- [ ] **Step 3: Run** `go test ./internal/trace -run Aggregate` — expected FAIL (undefined: Aggregate).

- [ ] **Step 4: Implement `Aggregate`** per spec A3–A6:
  - Pass 1 over `recs` (`Source == record.SourceOtel` only): set of span IDs; candidates = name equal, `service(r)` equal unless `o.Service` is `""`/`"*"`, `r.Time` within `[From, To]`. Keep candidates whose normalised `parent_span_id` is empty or not in the set; per trace the earliest (`Time`, then `Seq`). Sort by `Time` desc, `Seq` desc; `Matched = len`; keep `Cap`.
  - Pass 2: `map[traceID][]record.Record` for kept IDs (span records only; copy the struct).
  - Per trace (process in the kept order): `Build(group, id, Limit)`; `TruncatedTraces++` when `Truncated`. Find the root node by span ID; walk its subtree via `Parent` links; spans of the trace not in it are outside spans. Collect the set of span IDs that appear in any `SerialRuns[].Spans`.
  - Merge: a `map[parentMergedIndex+"\x00"+name]` → merged index; accumulate per call the duration list, `SelfMs`, error, type counts, services, start offset from the root's `StartMs`, serial membership, slowest candidate (longer wins; tie keeps the earlier-processed, i.e. more recent, trace; then lower `Seq`). Stop creating paths at `Limit` merged nodes (`NodesTruncated`).
  - Finish: sort each merged node's children by mean start offset then name, emit pre-order with `n<index>` IDs, compute percentiles (nearest-rank on the sorted durations), per-trace ratios, and shares against `OpTotalMs` (0 when `OpTotalMs` is 0), every float through `finite`.

- [ ] **Step 5: Run** `go test ./internal/trace` and `go test -race ./internal/trace` — PASS. Run `go test ./internal/trace -bench Aggregate -run '^$' -benchtime 5x`; note ns/op in the commit body (spec budget: ≤ 250 ms).

- [ ] **Step 6: Write failing handler tests** in `internal/web/server_test.go` with `newFixture(t, "../../testdata/web/aggregate.jsonl")`:
  - `TestAggregateAPI`: `GET /api/aggregate?name=GET%20%2Forders&from=all` → 200, `traces 5`, 6 nodes, `nodes[2].name "SELECT orders"`, `nodes[2].serialCalls 9`.
  - `TestAggregateAPIErrors`: no `name` → 400 `name is required`; 1025-byte name → 400 `name too long`; `from=yesterday` → 400 containing `range from`; `name=nope` → 404 `no root spans with that name in range`; `service=pricing&name=GET%20%2Forders` → 404.
  - `TestAggregateAPIRange`: `from=2026-11-17T17:02:30Z` → `traces 2`.

- [ ] **Step 7: Implement** route `mux.HandleFunc("GET /api/aggregate", s.aggregate)` and `func (s *server) aggregate(w http.ResponseWriter, r *http.Request)`: validate `name`, resolve the range as `kinds` does, `trace.Aggregate(snapshot, AggOptions{…, Service: r.URL.Query().Get("service")})`, write JSON.

- [ ] **Step 8: Verify**
  - `go test ./internal/trace ./internal/web` and `go test -race ./internal/trace ./internal/web` — PASS
  - `gofmt -l $(git ls-files '*.go') internal/trace/` — empty; `go vet ./...` — clean; `go test ./...` — PASS
  - Demo (per Global Constraints, run in background) then `curl -s 'http://127.0.0.1:8790/api/aggregate?name=GET%20%2Forders&from=all' -H 'Host: 127.0.0.1:8790' | head -c 800` shows `"traces":5`; kill the demo.

- [ ] **Step 9: Commit**

```bash
git add internal/trace/aggregate.go internal/trace/aggregate_test.go testdata/web/aggregate.jsonl internal/web/server.go internal/web/server_test.go
git commit -m "feat(trace): aggregate an operation's traces at /api/aggregate"
```

---

### Task 2: Aggregate view in the page (deep link, tree, metrics, history)

Review: full (history/popstate interplay with the waterfall; untrusted names in the DOM) · Model: opus

**Files:**
- Create: `internal/web/static/agg.js`
- Modify: `internal/web/static/trace.js` (export `typeGroup`, add/export `treeGuides`, use it in `renderTrace`; `showView` also sets `$("agg").hidden = true`), `internal/web/static/app.js` (`initAggregate` wiring, `saveURL`), `internal/web/static/index.html` (`<section id="agg" class="trace agg" hidden>`), `internal/web/static/app.css`

**Interfaces:**
- Consumes: `GET /api/aggregate` (Task 1 JSON); `openTrace({trace, span}, {push})` from `trace.js`; app.js helpers `{api, el, showError, clearError, formatDuration, getRange: () => state.range, getService: () => state.service}`.
- Produces (JS):
  - `trace.js`: `export function typeGroup(type)` (unchanged body); `export function treeGuides(vis)` → `string[][]`, one guides array per node of `vis` (nodes with `id`, `parent`, `depth`), values `"v" | "" | "tee" | "end"` exactly as `renderTrace` computes them today.
  - `agg.js`: `export function initAggregate(deps)`; `export async function openAggregate({name, service}, {push = true} = {})`; `export function aggURLParams()` → `"&agg=<enc name>[&aggsvc=<enc service>]"` or `""`; `export function restoreAggregateFromURL()`.
  - DOM ids: `#agg`, `#agg-head`, `#agg-tools`, `#agg-close`, `#agg-reload`, `#agg-collapse`, `#agg-expand`, `#agg-legend`, `#agg-grid`, `#agg-cols` (column header row), `#agg-rows`.

- [ ] **Step 1: Refactor `trace.js`**: move the guide computation out of `renderTrace` into `treeGuides(vis)`; export it and `typeGroup`. Browser-check the waterfall is unchanged on `?trace=4bf92f3577b34da6a3ce929d0e0e4736` (connectors, twisties) before going on.

- [ ] **Step 2: Markup and CSS**: `#agg` mirrors `#trace`'s top (head, tools, legend) and a grid of columns `minmax(240px, 35%) repeat(6, max-content) 1fr` (name, traces, calls/trace, p50, p95, total %, self %, share bar); header cells carry the spec's tooltips. Share bar: `.abar.bar-<group>` width `min(totalPct,100)%`, inner `.abar-self` width `min(selfPct,100)%` darker; `.agg-err` red count badge. Reuse `.trace-row`, `.tr-name`, `.tg`, `.twisty`, `.tw-pad` as they are.

- [ ] **Step 3: `agg.js` state and loading**: `av = {data, key, collapsed:Set, seq}`; `openAggregate` builds `"/api/aggregate?name=…&service=…&from=…&to=…"` from its args and `deps.getRange()`; if `key` equals the cached answer's key and the view is not being reloaded, reuse it (no fetch). Sequence number: drop superseded answers; on error show the banner, leave the current view, and on a non-push restore drop `agg`/`aggsvc` from the URL. On success: hide `#bar`, `#results`, `#footer`, `#trace` (and reset nothing in trace.js), show `#agg`, render, write the URL (`push` → `history.pushState({loghornAgg:1}, "", url)`, else replace).

- [ ] **Step 4: History**: `urlWith(params)` keeps only `q` plus `params` (as `trace.js` does). Close: if `history.state?.loghornAgg`, `history.back()`; else hide and replace the URL with `urlWith("")`. `popstate`: if the URL has `agg` and no `trace`/`span` → `openAggregate({name, service: aggsvc ?? deps.getService()}, {push:false})`; else `++seq` and hide `#agg` without touching `#bar`/`#results`/`#footer` when the URL has `trace` (the waterfall's handler shows itself). `hideAgg()` unhides the table parts only when the URL has neither `trace` nor `agg`. Escape closes the view (same guards as `trace.js`: no open dialog, not typing in another input). `app.js saveURL` appends `traceURLParams() + aggURLParams()`; `init` calls `restoreAggregateFromURL()` after `restoreTraceFromURL()` (a URL never carries both).

- [ ] **Step 5: Render**: header per spec A9 (name, service or `all services`, range `from → to` as strings, `N traces` or `N of M traces (capped, most recent)`, root `p50 … · p95 …`, outside and truncation notes); legend of present type groups; rows with `treeGuides`, twisty (click and dblclick toggle, as in `traceRow`), name, cells: `traces` as `5 (100%)`, `callsPerTrace` one decimal, `p50`/`p95` via `formatDuration(ms/1000, "ms")`, `total %`/`self %` one decimal, share bar, error badge. Collapse all / Expand all / Reload (refetch, keep `collapsed`).

- [ ] **Step 6: Verify**
  - `go test ./internal/web` — PASS (embed builds); `go vet ./...`; `go test ./...`.
  - Restart the demo (Global Constraints command). Playwright:
    1. `http://127.0.0.1:8790/?agg=GET%20%2Forders` — header `GET /orders · all services · 5 traces`, 6 rows in order n0…n5 with connectors, `SELECT orders` row shows `4 (80%)`, `3.5`, `10 ms`, `40 ms`, `38.3%`; outside note `1 trace with 1 span outside the root (missing parents)`; console has no errors.
    2. Collapse `GET /price` (api-server): its two descendants disappear; Expand all restores.
    3. Close → table shows, URL has no `agg`; Back → aggregate reopens from cache (no new `/api/aggregate` request in `browser_network_requests`).
    4. `?agg=%3Cb%3Ex%3C%2Fb%3E` → error banner `no root spans with that name in range` as plain text.
    5. `?trace=4bf92f3577b34da6a3ce929d0e0e4736` still renders the waterfall unchanged.
  - Kill the demo, close the browser.

- [ ] **Step 7: Commit**

```bash
git add internal/web/static/agg.js internal/web/static/trace.js internal/web/static/app.js internal/web/static/index.html internal/web/static/app.css
git commit -m "feat(web): aggregate operation view"
```

---

### Task 3: Entry points, jump-off to the slowest trace, serial chips, docs

Review: standard · Model: sonnet

**Files:**
- Modify: `internal/query/run.go` (`Row.SpanRoot`), `internal/query/run_test.go`, `internal/web/static/trace.js`, `internal/web/static/agg.js`, `internal/web/static/app.js`, `internal/web/static/index.html`, `internal/web/static/app.css`, `README.md` (after the **Trace waterfall** paragraph), `docs/features.md` (new `## Aggregate operation view` after `## Trace waterfall`, linking the spec)

**Interfaces:**
- Consumes: Task 2's `openAggregate({name, service}, {push})`; Task 1's `OpNode.slowest`, `serialCalls`, `serialPerTrace`, `serialPct`; `openTrace({trace, span})`.
- Produces: `query.Row.SpanRoot bool` (`json:"spanRoot,omitempty"`), true when `r.Source == record.SourceOtel` and its `parent_span_id` field is absent or an empty string; `initTrace` deps gain `openAggregate` and `getService`; button `#trace-agg`.

- [ ] **Step 1: Failing test** `TestRunSpanRoot` in `internal/query/run_test.go`: two otel records (one with `parent_span_id`, one without) in a Samples query → `rows[i].SpanRoot` false/true respectively; a log record → false.

- [ ] **Step 2: Run** `go test ./internal/query -run SpanRoot` — FAIL; implement in the row loop of `Run`; rerun — PASS.

- [ ] **Step 3: Waterfall button**: `<button id="trace-agg" type="button" title="merge every trace of this operation in the page's range">Aggregate this operation</button>` in `#trace-tools`. In `renderTrace`, find the operation root (first `depth 0` `span` node; else the first `span` whose parent is a `missing` node at depth 0); hide the button when none. Click → `d.openAggregate({name: root.name, service: root.service || ""})` (push).

- [ ] **Step 4: Table link**: in `app.js`, when `state.source === "otel"`, a cell of column path `name` whose row has `spanRoot` gets a trailing `a.agg-link` with text `∑`, title `aggregate this operation`, `href="#"`; click does `preventDefault`, `stopPropagation` (not the detail toggle) and `openAggregate({name: <cell value>, service: state.service === "*" ? "" : state.service})`. Do it at both call sites of `traceLinked` (Samples and the table renderer).

- [ ] **Step 5: Jump-off and serial chip in `agg.js`**: after the name, a `a.agg-slowest` link `slowest <duration>` titled `open the slowest trace containing this span`; click → `openTrace({trace: n.slowest.traceId, span: n.slowest.spanId})` (push, so Back returns here). When `serialCalls > 0`, a `.serial-chip` `<serialPerTrace.toFixed(1)> serial queries/trace, <serialPct.toFixed(1)>%` titled `average queries per trace that ran one after another inside a serial run, and their share of the operation's time`.

- [ ] **Step 6: Docs**: README paragraph (how to open: `∑` on a root span name in Spans mode, the waterfall's button, `?agg=`; what the columns mean; cap 1000 most recent). `docs/features.md` section pointing at the spec's deferred list.

- [ ] **Step 7: Verify**
  - `go test ./internal/query ./internal/web` — PASS; `gofmt -l $(git ls-files '*.go')` empty; `go vet ./...`; `go test ./...`.
  - Restart the demo; Playwright:
    1. Spans mode (sidebar switch to spans), the kind holding the `GET /orders` server spans: rows named `GET /orders` that are roots show `∑`; the nested T7 `GET /orders` (internal, has a parent) does not; clicking `∑` opens the aggregate with 5 traces.
    2. In the aggregate, `SELECT orders` shows chip `2.3 serial queries/trace, 15.0%` (`(2.25).toFixed(1)` is `2.3`) and `slowest 40 ms`; click it → waterfall of T4 with the first parallel `SELECT orders` selected.
    3. Waterfall shows `Aggregate this operation`; Back returns to the aggregate; Back again returns to the table with its query intact.
    4. From `?trace=4bf92f3577b34da6a3ce929d0e0e4736` click `Aggregate this operation` → aggregate of `GET /orders` service `api-server` (header says `api-server`); Close → back to that trace.
    5. Console has no errors. Kill the demo, close the browser.

- [ ] **Step 8: Commit**

```bash
git add internal/query/run.go internal/query/run_test.go internal/web/static/trace.js internal/web/static/agg.js internal/web/static/app.js internal/web/static/index.html internal/web/static/app.css README.md docs/features.md
git commit -m "feat(web): open the aggregate view from spans and traces; jump to the slowest trace"
```
