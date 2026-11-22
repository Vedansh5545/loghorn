# Trace Waterfall View Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: execute with the `orchestrating-plans` skill (one task-runner subagent per task, each dispatching its own fresh reviewer). Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Clicking a span ID or trace ID in the web explorer opens a waterfall of that trace — collapsible span tree, time bars, log lines inside the tree, find, self-time table, serial-run hints and critical path — with a restorable deep link.

**Architecture:** A new Go package `internal/trace` builds the whole tree and its analytics from the store snapshot (spans and logs of one trace, across services); `GET /api/trace` serves it. A new page module `internal/web/static/trace.js` renders it and only collapses, filters, finds and toggles. `app.js` turns ID cells into links, shares its record-detail renderer, and keeps the `trace`/`span` URL parameters.

**Order, and why:** Task 1 ships the endpoint with the tree (testable end to end with curl); Task 2 adds the analytics to the same answer; Tasks 3–5 are the page, each a usable increment over the previous.

**Tech Stack:** Go 1.25, standard library; vanilla JS ES modules under `internal/web/static/` (embedded with `go:embed`). No new modules, no JS libraries.

**Spec:** `docs/superpowers/specs/2026-11-21-trace-waterfall-design.md`. Read it with this plan; decisions D1–D6 there are binding.

## Global Constraints

- Every task ends with: `gofmt -l $(git ls-files '*.go')` prints nothing, `go vet ./...` clean, `go test ./...` passes. Tasks 1 and 2 also run `go test -race ./internal/trace ./internal/web`.
- Commit each task directly on `main`, one commit per task, staging only the task's files by path (`git add <path>…`, never `-A`: the tree has unrelated untracked files). Messages end with the session's attribution line.
- **loghorn is public.** No name of any real app, service, path, cloud project or table in code, tests, fixtures, docs or commits. Fixtures use `api-server`, `pricing`, `example-project`, `SELECT orders`.
- **Never pipe data into a loghorn binary.** Manual checks use the demo on a file argument.
- **Tests and demos never touch the real config.** Go tests that build a server use the existing `newFixture` (it sets `HOME`/`XDG_CONFIG_HOME` to temp dirs). The demo runs as `env -u XDG_CONFIG_HOME HOME="$(mktemp -d)" go run ./internal/web/demo -port 8790 testdata/web/trace.jsonl`.
- Before a browser check, free the port: `lsof -nP -iTCP:8790 -sTCP:LISTEN`, kill that PID. After editing `internal/web/static/*`, restart the demo (the page is embedded).
- Page rules (CLAUDE.md): data reaches the DOM only through `textContent`/attributes, never `innerHTML`; keep `[hidden]{display:none!important}` in `app.css`; every async loader takes a sequence number and drops a superseded answer; a button inside a control that redraws on blur/change does `preventDefault` on `mousedown`.
- Handler tests use `httptest.NewServer` (`newFixture`); numbers in hand-built records are `json.Number`.
- Values, verbatim: endpoint `GET /api/trace?trace=<id>&span=<id>`; limit 10000 spans and 10000 logs; node kinds `span` | `log` | `missing`; placeholder IDs `missing:<span id>`; log node IDs `log:<seq>`; placeholder label `(Missing span <id>)`; serial-run chip `<n> serial queries, <duration>`; colour groups `db`, `http`, `rpc`, `messaging`, `gen_ai`, `span`; toggle labels `Logs: in tree` / `Logs: hidden`; localStorage key `loghorn.traceLogs`; URL `?q=<json>&trace=<id>&span=<id>`; error texts `trace or span is required`, `no spans or logs with that trace id`, `no record with that span id`.
- Out of scope (spec "Deferred and discarded"): flame graph, span links, auto-refresh, sequence diagram, diff, dependency graph, share button, keyboard tree navigation, virtualised rendering, span-event rows.

## Review Focus

1. **Malformed parent links** — a span that is its own parent, a two-span parent cycle, or two records with the same `span_id`: `Build` must end, show every span exactly once, keep the lowest `Seq` of a duplicate. Tests: Task 1 `TestBuildCycle`, `TestBuildDuplicateSpanID`.
2. **Trace ID spellings** — `projects/example-project/traces/<ID>`, upper-case hex, and all-zero IDs: the first two find the trace, all-zero counts as absent. Tests: Task 1 `TestTraceRef`, `TestTraceAPIByCloudTraceForm`.
3. **Degenerate times** — a span with no `end`, an `end` before `start`, a child running past its parent, a trace that is one log line (duration 0): no NaN/Inf in JSON, self time never negative, the page never divides by zero (it uses `Math.max(durationMs, 0.001)`). Tests: Task 1 `TestBuildTimes`, Task 2 `TestSelfTimeChildOverrun`; browser check in Task 3 with the one-log trace `5ce0e4736a3ce929d4bf92f3577b34da`.
4. **Superseded loads** — clicking a second ID, or Back, while a trace is loading: the older answer is dropped (sequence number), the URL and the view agree. Browser check in Task 3.
5. **Huge traces** — more than 10000 spans: the answer is capped, `truncated` is true and the header says so. Tests: Task 1 `TestBuildTruncated` (with a small limit); header text check in Task 3.

---

## File Structure

| Path | Responsibility |
|---|---|
| `internal/record/trace.go` (new) | `NormalizeTraceID`, `NormalizeSpanID`, `TraceRef` |
| `internal/trace/trace.go` (new) | `Trace`, `Node`, `Build`, `Find`: tree, placeholders, log placement, ordering, times, limit |
| `internal/trace/analyze.go` (new) | self time, self-time summary, critical path, serial runs |
| `internal/web/server.go` | `GET /api/trace` |
| `testdata/web/trace.jsonl` (new) | generic fixture: two traces, spans and logs (below) |
| `internal/web/static/trace.js` (new) | the waterfall view |
| `internal/web/static/app.js` | ID links in cells, `fillRecordDetail`, URL params, wiring |
| `internal/web/static/index.html`, `app.css` | `#trace` section and its styles |
| `docs/features.md`, `README.md` | user docs (Task 5) |

### Fixture `testdata/web/trace.jsonl`

Times are milliseconds after `2026-11-17T17:00:00.000Z`. Trace **T1** = `4bf92f3577b34da6a3ce929d0e0e4736`, trace **T2** = `5ce0e4736a3ce929d4bf92f3577b34da`. Span IDs `10000000000000NN` are written `sNN`.

Span lines (shape of `testdata/web/spans.jsonl`; `start`, `end`, `duration_ms`, `service.name`):

| id | parent | name | kind | attributes | service | start–end |
|---|---|---|---|---|---|---|
| s01 | — | `GET /orders` | server | `http.request.method: GET` | api-server | 0–120 |
| s02 | s01 | `auth check` | internal | — | api-server | 2–10 |
| s03…s08 | s01 | `SELECT orders` | client | `db.system.name: postgresql`, `db.query.text: select * from orders where customer_id = $1` | api-server | 12–20, 20–28, 28–36, 36–44, 44–52, 52–60 |
| s09 | s01 | `SELECT items` | client | `db.system.name: postgresql`, `db.query.text: select * from items where order_id = $1` | api-server | 62–74 |
| s10 | s01 | `SELECT items` | client | same as s09 | api-server | 64–80 |
| s11 | s01 | `call pricing` | client | `http.request.method: POST` | api-server | 82–110 |
| s12 | s11 | `POST /price` | server | `http.request.method: POST` | pricing | 84–108 |
| s13 | s12 | `SELECT prices` | client | `db.system.name: postgresql`; `status: error`, `status_message: timeout` | pricing | 90–100 |
| s14 | `ffffffffffffff01` (absent) | `orphan task` | internal | — | api-server | 30–50 |
| `2000000000000001` (T2) | — | `GET /health` | server | `http.request.method: GET` | api-server | 200–201 |

Log lines (each has `"labels":{"schema":"app"}`, `timestamp`, `severity`, `message`):

| tag | trace | span | service | ms | severity | message |
|---|---|---|---|---|---|---|
| L1 | `trace_id` T1 | `span_id` s01 | `service.name` api-server | 1 | INFO | `request received` |
| L2 | `trace_id` T1 | `span_id` s02 | api-server | 5 | INFO | `token ok` |
| L3 | `trace_id` T1 | `span_id` s13 | pricing | 95 | ERROR | `query timed out` |
| L4 | `trace_id` T1 | — | api-server | 115 | INFO | `response sent` |
| L5 | `trace_id` T1 | `span_id` `ffffffffffffff02` | api-server | 40 | WARNING | `late callback` |
| L6 | `logging.googleapis.com/trace`: `projects/example-project/traces/4BF92F3577B34DA6A3CE929D0E0E4736` | `logging.googleapis.com/spanId` s11 | `resource.labels.service_name` api-server | 83 | INFO | `calling pricing` |
| L7 | `trace_id` T2 | `span_id` `2000000000000001` | api-server | 200 | INFO | `health ok` |

Expected T1 pre-order (label, depth): `GET /orders` 0 · `request received` 1 · `auth check` 1 · `token ok` 2 · `SELECT orders` ×6 at 1 · `SELECT items` (62) 1 · `SELECT items` (64) 1 · `call pricing` 1 · `calling pricing` 2 · `POST /price` 2 · `SELECT prices` 3 · `query timed out` 4 · `response sent` 1 · `(missing ffffffffffffff01)` 0 · `orphan task` 1 · `(missing ffffffffffffff02)` 0 · `late callback` 1 — 22 nodes, 14 spans, 6 logs, services `["api-server","pricing"]`, duration 120 ms.

---

### Task 1: Trace tree and `GET /api/trace`

**Review:** standard. **Model:** default.

**Files:**
- Create: `internal/record/trace.go`, `internal/record/trace_test.go`
- Create: `internal/trace/trace.go`, `internal/trace/trace_test.go`
- Create: `testdata/web/trace.jsonl`
- Modify: `internal/web/server.go` (route + handler), `internal/web/server_test.go`

**Interfaces:**
- Produces (Go):
  - `func record.NormalizeTraceID(s string) string` — trim, keep the part after the last `/traces/`, lower-case; `""` for empty or all-zero.
  - `func record.NormalizeSpanID(s string) string` — trim, lower-case; `""` for empty or all-zero.
  - `func record.TraceRef(fields map[string]any) (traceID, spanID string)` — `trace_id` else `trace`; `span_id` else `spanId`; both normalised; non-string values ignored.
  - `const trace.Limit = 10000`
  - `func trace.Find(recs []record.Record, spanID string) string` — the trace ID of the span record with that span ID, else of the first log record with that span ID and a trace ID, else `""`. Input normalised.
  - `func trace.Build(recs []record.Record, traceID string, limit int) (Trace, bool)` — `false` when no record has the trace ID. `limit` applies separately to spans and logs, lowest `Seq` first.
  ```go
  type Trace struct {
      TraceID    string      `json:"traceId"`
      Start      time.Time   `json:"start"`
      DurationMs float64     `json:"durationMs"`
      SpanCount  int         `json:"spanCount"`
      LogCount   int         `json:"logCount"`
      Services   []string    `json:"services"`   // sorted, non-empty strings only
      Truncated  bool        `json:"truncated"`
      Nodes      []Node      `json:"nodes"`      // pre-order
      SerialRuns []SerialRun `json:"serialRuns"` // Task 2; [] until then
      SelfTime   []NameTime  `json:"selfTime"`   // Task 2; [] until then
  }
  type Node struct {
      ID         string    `json:"id"`     // span ID, "missing:<id>", "log:<seq>"
      Parent     string    `json:"parent"` // "" at top level
      Depth      int       `json:"depth"`
      Kind       string    `json:"kind"`   // "span" | "log" | "missing"
      Seq        int       `json:"seq,omitempty"`
      Name       string    `json:"name,omitempty"`    // span name; missing: the absent ID
      Type       string    `json:"type,omitempty"`    // span.type
      Service    string    `json:"service,omitempty"`
      StartMs    float64   `json:"startMs"`           // from Trace.Start
      DurationMs float64   `json:"durationMs"`        // 0 for logs
      SelfMs     float64   `json:"selfMs"`            // Task 2
      Error      bool      `json:"error,omitempty"`   // span status "error"
      Critical   bool      `json:"critical,omitempty"`// Task 2
      Children   int       `json:"children"`          // direct children, spans + logs
      LogTicks   []float64 `json:"logTicks,omitempty"`// StartMs of direct child logs
      Severity   string    `json:"severity,omitempty"`// logs
      Message    string    `json:"message,omitempty"` // logs: message, else msg, else raw; ≤ 500 runes
  }
  type SerialRun struct { Parent, First string; Spans []string; Count int; WallMs float64 } // json: parent, first, spans, count, wallMs
  type NameTime  struct { Name string; Count int; TotalMs, SelfMs float64 }             // json: name, count, totalMs, selfMs
  ```
- Produces (HTTP): `GET /api/trace?trace=<id>&span=<id>` → 200 `Trace` JSON. `trace` wins when both are given; with only `span`, `trace.Find`. 400 `trace or span is required` (neither), 400 `id too long` (> 256 bytes), 404 `no record with that span id` (Find empty), 404 `no spans or logs with that trace id` (Build false). Ignores `service`; reads `s.o.Store.Snapshot()`.

- [ ] **Step 1: Write the fixture** `testdata/web/trace.jsonl` exactly per the File Structure tables (span lines first, then log lines, in table order). Durations are `json` numbers equal to end − start.

- [ ] **Step 2: Write failing tests**
  - `internal/record/trace_test.go` `TestTraceRef` (table): `{trace_id: "4BF9…36"}` → lower-case; `{trace: "projects/example-project/traces/4bf9…36"}` → the bare ID; `{trace_id: "0000…0000"}` → `""`; `{span_id: "ABC"}` and `{spanId: "abc"}` → `"abc"`; `{trace_id: 5}` (json.Number) → `""`; `trace_id` preferred over `trace`.
  - `internal/trace/trace_test.go` loads the fixture with `store.Preload(st, []string{"../../testdata/web/trace.jsonl"}, config.Input{})` and `st.Snapshot()`.
    - `TestBuildFixture`: `Build(recs, T1, Limit)` → node labels and depths equal the expected T1 pre-order (label = `Name` for spans, `Message` for logs, `"(missing " + Name + ")"` for missing); `SpanCount` 14, `LogCount` 6, `Services` `["api-server","pricing"]`, `DurationMs` 120, `Start` 17:00:00.000Z; `Parent` of `orphan task` is `missing:ffffffffffffff01`; s13 `Error` true; `LogTicks` s01 `[1,115]`, s02 `[5]`, s11 `[83]`, s13 `[95]`, `missing:ffffffffffffff02` `[40]`; placeholder `missing:ffffffffffffff01` `StartMs` 30 `DurationMs` 20; `Children` of s01 is 12.
    - `TestBuildOtherTrace`: T2 → 2 nodes (`GET /health`, `health ok` at depth 1), duration 1.
    - `TestBuildUnknown`: `Build(recs, "deadbeef", Limit)` → `false`.
    - `TestFind`: s13 → T1; `ffffffffffffff02` → T1 (log only); `"1000000000000011"` upper-cased → T1; unknown → `""`.
    - `TestBuildTruncated`: `Build(recs, T1, 5)` → `SpanCount` 5, `LogCount` 5, `Truncated` true.
    - Hand-built records (helper `span(seq int, id, parent, name string, startMs, endMs int) record.Record` with `Source: "otel"`, `Time`, `Fields` holding `trace_id`, `span_id`, `parent_span_id`, `name`, `end`, `duration_ms` as `json.Number`):
      - `TestBuildCycle`: a→b, b→a, and c→c → 3 span nodes, each once, all at depth ≤ 1, call returns.
      - `TestBuildDuplicateSpanID`: two records with span ID `a`, Seq 1 named `first`, Seq 2 named `second` → one node, `first`, `SpanCount` 1.
      - `TestBuildTimes`: a span with no `end` and `duration_ms` 7 → `DurationMs` 7; a span with `end` before `start` → `DurationMs` 0; a trace of a single log → `DurationMs` 0 and `json.Marshal` succeeds.
  - `internal/web/server_test.go` (fixture `trace = "../../testdata/web/trace.jsonl"`):
    - `TestTraceAPIByTrace`: `GET /api/trace?trace=<T1>` → 200, 22 nodes, `traceId` T1.
    - `TestTraceAPIBySpan`: `?span=1000000000000013` → T1; `?span=ffffffffffffff02` → T1.
    - `TestTraceAPIByCloudTraceForm`: `?trace=` + `url.QueryEscape("projects/example-project/traces/" + upper(T1))` → T1.
    - `TestTraceAPIErrors`: no params → 400 `trace or span is required`; 300-byte trace → 400; `?trace=deadbeef` → 404 `no spans or logs with that trace id`; `?span=deadbeef` → 404 `no record with that span id`; `?service=pricing&trace=<T1>` still → 22 nodes.

- [ ] **Step 3: Run** `go test ./internal/record ./internal/trace ./internal/web` — Expected: FAIL (undefined `TraceRef`, package `trace` missing, 404 on `/api/trace`).

- [ ] **Step 4: Implement** `record/trace.go`, `trace/trace.go` and the handler. Build: one pass over `recs` collecting span records (`Source == otel`, deduplicated by span ID, lowest Seq) and log records whose `TraceRef` trace ID matches; apply the limit; compute times per spec D4 (start `Record.Time`, end from `end` parsed RFC 3339 nano, else start + `duration_ms`, clamped ≥ start); link children; create placeholders for absent parents and for log span IDs that name no span; place no-span logs under the single root span else top level; sort children (start, spans before logs, Seq); DFS from top-level nodes in the same order, then make each unvisited span top-level (earliest start first) and DFS again, with a visited set. Placeholder start/end = min/max over its descendants' times. Initialise `SerialRuns`/`SelfTime` to empty slices so JSON shows `[]`.

- [ ] **Step 5: Run** `go test -race ./internal/record ./internal/trace ./internal/web` — Expected: PASS. Then `gofmt -l $(git ls-files '*.go')` (empty), `go vet ./...`, `go test ./...`.

- [ ] **Step 6: Manual check** — start the demo (Global Constraints) and `curl -s 'http://127.0.0.1:8790/api/trace?span=1000000000000013' -H 'Host: 127.0.0.1:8790' | head -c 600` shows `"traceId":"4bf92f…"` and the nodes array.

- [ ] **Step 7: Commit** the six files: `feat(trace): build a trace tree and serve it at /api/trace`.

---

### Task 2: Self time, critical path and serial runs

**Review:** standard. **Model:** default.

**Files:**
- Create: `internal/trace/analyze.go`, `internal/trace/analyze_test.go`
- Modify: `internal/trace/trace.go` (call `analyze(&t)` at the end of `Build`)

**Interfaces:**
- Consumes: `Trace`, `Node` from Task 1.
- Produces: `func analyze(t *Trace)` filling `Node.SelfMs`, `Node.Critical`, `Trace.SelfTime` (sorted by `SelfMs` desc, then `Name`), `Trace.SerialRuns` (in pre-order of their first span). Same JSON shape as Task 1; the page (Tasks 3, 5) reads these fields.

Rules (spec D5): self time = duration − |union of child-span intervals clipped to the span|, never negative; logs and placeholders 0. Critical path: from the top-level node with the latest end (ties: lowest index in `Nodes`); for a node with cursor = its end, visit child spans by end descending (ties: start descending, then Seq); a child with end ≤ cursor (+1e-9) is critical, recurse into it, cursor = its start; placeholders are walked like spans but only spans are flagged; the starting node is flagged when it is a span. Serial run: span children of one node in start order; a maximal run of consecutive `Type == "db"` spans each starting ≥ the previous end (−1e-9); a non-db span ends the run, logs are skipped; keep runs with `Count ≥ 3`; `WallMs` = last end − first start.

- [ ] **Step 1: Write failing tests** in `analyze_test.go`, on the fixture via `Build(recs, T1, Limit)`:
  - `TestSelfTimeFixture`: `SelfMs` s01 18, s11 4, s12 14, s13 10, each `SELECT orders` 8, placeholders 0, logs 0.
  - `TestSelfTimeSummary`: `SelfTime` equals, in order: `SELECT orders` {6, 48, 48}, `SELECT items` {2, 28, 28}, `orphan task` {1, 20, 20}, `GET /orders` {1, 120, 18}, `POST /price` {1, 24, 14}, `SELECT prices` {1, 10, 10}, `auth check` {1, 8, 8}, `call pricing` {1, 28, 4}.
  - `TestCriticalPathFixture`: critical set = s01, s02, s03…s08, s10, s11, s12, s13; not s09, s14, no placeholder, no log.
  - `TestSerialRunsFixture`: exactly one run: `Parent` s01, `First` s03, `Spans` s03…s08, `Count` 6, `WallMs` 48.
  - Hand-built (Task 1's `span` helper extended with a `type` argument or a `db` variant):
    - `TestSelfTimeChildOverrun`: parent 0–10, child 5–30 → parent `SelfMs` 5, child 25.
    - `TestSerialRunBrokenByOtherSpan`: db 0–1, db 1–2, internal 2–3, db 3–4, db 4–5 → no run; db 0–1, db 1–2, db 2–3 with a log at 1.5 → one run of 3.
    - `TestCriticalPathSiblingsOverlap`: parent 0–10, children a 0–6 and b 2–9 → b critical, a not (a ends at 6 > cursor 2).
- [ ] **Step 2: Run** `go test ./internal/trace` — Expected: FAIL (SelfMs 0, empty runs).
- [ ] **Step 3: Implement** `analyze` in `analyze.go`; work from an index of children built from `Node.Parent` (Nodes are pre-order, so children follow their parent).
- [ ] **Step 4: Run** `go test -race ./internal/trace ./internal/web`, `gofmt -l $(git ls-files '*.go')`, `go vet ./...`, `go test ./...` — Expected: PASS, no output from gofmt.
- [ ] **Step 5: Commit** `analyze.go`, `analyze_test.go`, `trace.go`: `feat(trace): self time, critical path and serial query runs`.

---

### Task 3: Waterfall page — open, deep link, tree, bars, detail

**Review:** standard. **Model:** opus (sets the page structure Tasks 4–5 build on).

**Files:**
- Create: `internal/web/static/trace.js`
- Modify: `internal/web/static/app.js`, `internal/web/static/index.html`, `internal/web/static/app.css`

**Interfaces:**
- Consumes: `GET /api/trace` (Task 1/2 JSON), `GET /api/record?seq=N` (existing).
- Produces in `app.js`:
  - `export async function fillRecordDetail(container, seq)` — the body of today's `toggleDetail` after the `loading…` placeholder (SQL `h3`+`pre` blocks, then `record`); `toggleDetail` calls it.
  - `TRACE_PATHS = new Set(["trace_id", "trace"])`, `SPAN_PATHS = new Set(["span_id", "spanId", "parent_span_id"])`; `render()` and `renderTable()` wrap a non-empty string cell of those columns in `<a class="trace-link" href="#">` whose click does `preventDefault()`, `stopPropagation()` and `openTrace({trace: v})` or `openTrace({span: v})`.
  - `saveURL()` appends `traceURLParams()` to `?q=…`.
  - `init()` calls `initTrace({api, el, showError, clearError, fillRecordDetail, localTime, formatDuration, severityClass})` before `refresh()`, and `restoreTraceFromURL()` after it.
- Produces in `trace.js` (Tasks 4–5 extend this module):
  - `export function initTrace(deps)`; `export async function openTrace({trace, span}, {push = true} = {})`; `export function closeTrace()`; `export function traceURLParams()` → `""` or `"&trace=<id>"` + `"&span=<id>"` when a span is selected; `export function restoreTraceFromURL()`.
  - Module state `tv = {data, selected, collapsed: Set<nodeId>, logs: "tree", critical: false, find: {q: "", matches: [], i: -1}}` and `let traceSeq = 0` (drop superseded loads).
  - `function visibleNodes()` → nodes whose ancestors are all expanded (and, from Task 4, filtered by `tv.logs`); `function renderTrace()` redraws header, legend, axis, rows; `function traceRow(node, guides)` → one row element with `dataset.id`; `function selectNode(id, {scroll})`; `function typeGroup(type)` → `db|http|rpc|messaging|gen_ai|span`.
  - DOM ids (in `index.html`, inside `.content` after `#bar`… `#footer`): `#trace` (section, `hidden`), `#trace-head`, `#trace-tools` (buttons `#trace-close`, `#trace-reload`, `#trace-collapse`, `#trace-expand`), `#trace-legend`, `#trace-axis`, `#trace-rows`, `#trace-detail`, `#trace-self` (empty until Task 5). Task 4 adds `#trace-logs`, `#trace-find`, `#trace-prev`, `#trace-next`, `#trace-count`; Task 5 adds `#trace-critical`.

Behaviour (spec D6): `openTrace` fetches `/api/trace?trace=…` (or `span=…`), on success hides `#bar`, `#results`, `#footer`, shows `#trace`, pushes `?q=…&trace=<data.traceId>[&span=<id>]` (`push`) or replaces; selects the span (expanding ancestors, `scrollIntoView({block: "center"})`); on error `showError` and stay on (or return to) the table. `popstate` re-reads the URL: a `trace` param opens without pushing, none closes. Close button and Escape (when `#trace-find` is empty, from Task 4) call `closeTrace()`, which goes `history.back()` if this page pushed the entry, else replaces the URL without the params. Rows only for `kind !== "log"` in this task. Grid columns: name `minmax(240px, 35%)`, service `max-content` only when `data.services.length > 1`, bars `1fr`. Connectors: per row, one `.tg` cell per depth level drawing a vertical line when that ancestor has a later visible sibling, plus an elbow (├ / └) at the row's own level. Axis: step = smallest of 1/2/5×10ⁿ ms giving ≤ 10 intervals over `Math.max(durationMs, 0.001)`; labels via `formatDuration(ms, "ms")`; gridlines as a background layer spanning the rows. Bar: `left = startMs/D`, `width = max(durationMs/D, 2px)`; class `bar-<typeGroup>`, `bar-missing` (hatched), `bar-error` (outline `--sev-error`); label after the bar, or before it when `startMs + durationMs > 0.8·D`. Legend lists only the groups present. Header: `localTime(start)` (ISO in `title`), total duration, `N spans · M logs · K services`, and `showing the first 10000 spans and logs` when `truncated`. Selecting a row: class `selected`, `fillRecordDetail(#trace-detail, seq)`; a placeholder shows `This span is not in the data` with its ID and `Children`. Colours: CSS tokens `--trace-db`, `--trace-http`, `--trace-rpc`, `--trace-messaging`, `--trace-gen_ai`, `--trace-span` taken from `--series-1…6`, with dark-mode values alongside the existing dark block.

- [ ] **Step 1: Extract `fillRecordDetail`** from `toggleDetail`; table detail rows still behave the same (browser check below).
- [ ] **Step 2: Add the `#trace` markup and CSS**, keeping `[hidden]{display:none!important}`.
- [ ] **Step 3: Write `trace.js`** per the interfaces above.
- [ ] **Step 4: Wire `app.js`**: ID links in `render()`/`renderTable()`, `saveURL` params, `initTrace`/`restoreTraceFromURL` in `init()`.
- [ ] **Step 5: Run** `go vet ./...` and `go test ./internal/web` — Expected: PASS (the embed still builds).
- [ ] **Step 6: Browser check** (demo per Global Constraints, Spans mode, kind `db`, view Samples, add column `trace_id` if not shown):
  - Click a `span_id` of `SELECT prices` → waterfall opens; URL has `&trace=4bf92f…&span=1000000000000013`; that row is highlighted, scrolled into view, its ancestors expanded; detail pane shows the formatted, coloured SQL then the record.
  - Header shows 120 ms, `14 spans · 6 logs · 2 services`; service column visible; bars coloured db/http/span with a legend; `SELECT prices` has the error outline; the two `(Missing span …)` rows are grey and hatched; connectors and axis labels (0, 20 ms … 120 ms) render; durations label each bar.
  - Collapse all / Expand all and twisties work; Back closes the view and the table query is unchanged; Forward reopens; reloading the deep-link URL reopens the same selection.
  - Open `?trace=5ce0e4736a3ce929d4bf92f3577b34da` → one service, no service column, no NaN in the axis. Open `?trace=deadbeef` → error banner, table shown.
  - Click a trace ID and immediately a different span ID → the second trace wins.
  - In the Logs mode, a log row's table detail row still opens on row click (not on the ID link).
- [ ] **Step 7: Commit** the four static files: `feat(web): trace waterfall view`.

---

### Task 4: Log lines in the tree and find in trace

**Review:** standard. **Model:** default.

**Files:**
- Modify: `internal/web/static/trace.js`, `internal/web/static/index.html`, `internal/web/static/app.css`

**Interfaces:**
- Consumes: Task 3's `tv`, `visibleNodes()`, `renderTrace()`, `traceRow()`, `selectNode()`, DOM ids; `Node.kind == "log"`, `severity`, `message`, `logTicks`.
- Produces: `#trace-logs` (toggle button, text `Logs: in tree` / `Logs: hidden`), `#trace-find` (search input), `#trace-prev`, `#trace-next`, `#trace-count` (`n of m`, or `no matches`); `function findMatches()` → node ids in pre-order.

Behaviour: log rows show a severity badge (`.sev sev-<class>` via `deps.severityClass`, from Task 3) and the first line of the message with ellipsis; a diamond marker at `startMs` in the bar column. Every span or placeholder bar draws a 1 px tick per `logTicks` entry (in both modes). `Logs: hidden` filters log nodes out of `visibleNodes()`; the mode persists in `localStorage["loghorn.traceLogs"]` (`"tree"` | `"hidden"`, read and written in try/catch; default `"tree"`). Hiding logs while a log row is selected clears the selection. Find: case-insensitive substring over span `name` and, when logs are in the tree, log `message`; recomputed on `input`; Enter/▶ next, Shift+Enter/◀ previous, wrapping; going to a match expands ancestors, selects it, scrolls it into view; matched rows get class `match`; the placeholder text is `find spans and logs` or `find spans`. Escape in a non-empty find box clears it; in an empty one closes the view (Task 3's handler). Selecting a log row opens its record in the detail pane.

- [ ] **Step 1: Add the toolbar controls** (`#trace-logs`, find box, ◀ ▶, count) to `#trace-tools`.
- [ ] **Step 2: Render log rows, markers and ticks; implement the toggle with persistence.**
- [ ] **Step 3: Implement find.**
- [ ] **Step 4: Run** `go vet ./...`, `go test ./internal/web` — Expected: PASS.
- [ ] **Step 5: Browser check** on T1: `request received` and `response sent` are children of `GET /orders` in time order; `calling pricing` sits between `call pricing` and `POST /price`; `late callback` sits under `(Missing span ffffffffffffff02)`; `GET /orders` bar shows ticks at 1 ms and 115 ms; `Logs: hidden` removes the six log rows but keeps the ticks, survives a page reload; find `select` → `n of 9`, ▶ walks them with expansion after Collapse all; find `timed out` matches only with logs in the tree; clicking `query timed out` shows its record.
- [ ] **Step 6: Commit** the three files: `feat(web): log lines and find in the trace waterfall`.

---

### Task 5: Serial runs, critical path, self-time table, docs

**Review:** standard. **Model:** default.

**Files:**
- Modify: `internal/web/static/trace.js`, `internal/web/static/index.html`, `internal/web/static/app.css`
- Modify: `docs/features.md` (new section `## Trace waterfall`), `README.md` (one paragraph under the web view section)

**Interfaces:**
- Consumes: `Trace.serialRuns`, `Trace.selfTime`, `Node.critical`, `Node.selfMs`; Task 3's render functions and `#trace-self`.
- Produces: `#trace-critical` toggle button (`aria-pressed`); `function renderSelfTable()`.

Behaviour: in the name cell of each run's `first` span, a chip `6 serial queries, 48 ms` (count + `formatDuration(wallMs, "ms")`), `title` listing that the queries ran one after another; rows of the run's `spans` get class `serial` (faint band). Critical toggle (off by default, not persisted) adds class `critical-on` to `#trace`, which dims bars of nodes without `critical`. Self-time table in `#trace-self`: columns name, count, total, self, self % (of `durationMs`, one decimal); initial order as served; clicking a header sorts by it (second click reverses), numbers right-aligned; a name cell click runs find for that name. Docs: features.md describes opening, deep link, logs toggle, find, serial runs, critical path, self time, and the deferred list by reference to the spec; README gets one paragraph and the deep-link format. Generic examples only.

- [ ] **Step 1: Serial-run chip and band.**
- [ ] **Step 2: Critical path toggle.**
- [ ] **Step 3: Self-time table with sorting.**
- [ ] **Step 4: Docs.**
- [ ] **Step 5: Run** `go vet ./...`, `go test ./...` — Expected: PASS.
- [ ] **Step 6: Browser check** on T1: chip `6 serial queries, 48 ms` on the first `SELECT orders`, band over the six rows, none on `SELECT items`; Critical path on dims `SELECT items` (62 ms) and `orphan task` only among spans; self table starts `SELECT orders 6 · 48 ms · 48 ms · 40.0%` and re-sorts by count, total and name.
- [ ] **Step 7: Commit** the five files: `feat(web): serial runs, critical path and self time in the trace view`.
