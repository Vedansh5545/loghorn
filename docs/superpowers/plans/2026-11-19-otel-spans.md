# `-otel` Spans Implementation Plan

> **Amendment (user decision, during Task 1):** span files are named
> `loghorn.traces.jsonl` (today) and `loghorn-YYYY-MM-DD.traces.jsonl`
> (archives), not `loghorn-spans.log` / `loghorn-spans-<date>.log`. The
> writer takes a base name and a suffix; each writer's archive matching
> requires its exact suffix. Where this plan still says `SpansBase` /
> `loghorn-spans`, use the names Task 1 records under "As built" in the
> workspace context.md.

> **For agentic workers:** REQUIRED SUB-SKILL: execute with the `orchestrating-plans` skill (one task-runner subagent per task, each dispatching its own fresh reviewer). Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** under `loghorn -otel -- <cmd>`, every span the child exports is stored as one JSON line in `.loghorn/loghorn.traces.jsonl`, rotated and pruned like `loghorn.log`; the child is also pointed at the traces endpoint. Metrics stay discarded; the web view does not change.

**Architecture:** `logfile.Writer` is parameterised by a base name and a suffix (`loghorn` + `.log`; `loghorn` + `.traces.jsonl`). The span writer is a *companion* of the log writer: created from it, sharing its directory, clock, location and its one `loghorn.lock`, closed by the log writer's `Close` before the lock is released, and opening its file only on the first span. `internal/otlp` gains `MapSpans` (spec "Spans" table); the receiver decodes `/v1/traces` fully and delivers span lines to a second callback under the same drain guard as logs. `wrapper.go` wires the companion writer to that callback; `ChildEnv` adds the three traces variables.

**Order, and why:** Task 1 (logfile) is a library change with no new caller, so it cannot break a run. Task 2 builds the mapping, receiver change, environment and wiring on top of its `Companion` interface and proves the whole path end to end.

**Tech Stack:** Go 1.25; existing `go.opentelemetry.io/proto/otlp` (`collector/trace/v1`, `trace/v1`, `common/v1`, `resource/v1`) and `google.golang.org/protobuf`. **No new modules** — the end-to-end child posts a hand-built protobuf `ExportTraceServiceRequest` with `net/http` rather than pulling in the OTEL trace SDK/exporter (**Decided**: `otlptracehttp` is not in `go.mod` and is not needed to exercise the receiver).

**Spec:** `docs/superpowers/specs/2026-11-19-otel-receiver-design.md` — sections "Spans", "Child environment", "Receiver", "Testing" (the spans changes are commit a5ec5e8). Earlier plan for the rest of the spec: `docs/superpowers/plans/2026-11-19-otel-and-tui-removal.md`.

## Global Constraints

- Every task ends with: `gofmt -l .` prints nothing (ignore anything under `.claude/`), `go vet ./...` clean, `CGO_ENABLED=0 go build ./...` succeeds, `go test ./...` and `go test -race ./...` pass.
- Commit each task directly to `main` (no branch, no merge commit). Commit messages end with the session's attribution lines.
- **Never pipe data into a loghorn binary** (`… | loghorn`): piped stdin is recorded to the project log. Manual checks run the binary from a scratch directory with a command after `--`, or `< file`.
- **Tests never write into the repo's own `.loghorn`.** Unit tests use `t.TempDir()` for every `logfile.Open`; binary tests go through `runLoghorn(t, dir, …)` with `dir := t.TempDir()` (it already sets `cmd.Dir`, `HOME`, `XDG_CONFIG_HOME`).
- **Pruning never deletes the other writer's files**, in either direction: the log writer never removes `loghorn.traces.jsonl` or `loghorn-<date>.traces.jsonl`; the span writer never removes `loghorn.log` or `loghorn-<date>.log`. Both directions are tested (Task 1).
- File names, verbatim: `loghorn.log`, `loghorn-YYYY-MM-DD.log`, `loghorn.traces.jsonl`, `loghorn-YYYY-MM-DD.traces.jsonl`, `loghorn.lock`. Retention unchanged: `keepDays = 3`. Files `0o600`, as today.
- Stored span keys, exactly: `trace_id`, `span_id`, `parent_span_id`, `name`, `kind`, `start`, `end`, `duration_ms`, `status`, `status_message`, `attributes`, `events` (items `{time, name, attributes}`), `links` (items `{trace_id, span_id, attributes}`), `service.name`, `loghorn.run.id`, `otel.resource`, `otel.scope.name`, `otel.scope.version`.
- Child environment under `-otel` adds, always set (replacing any existing value): `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT=<endpoint>/v1/traces`, `OTEL_EXPORTER_OTLP_TRACES_PROTOCOL=http/protobuf`, `OTEL_TRACES_EXPORTER=otlp`. Existing variables unchanged.
- The web view (`internal/web`), `-historical`, `-filter` and the recorder do not change behaviour; spans never enter `loghorn.log` or the live store, and spans do not count as "OTLP logs" for the stdout dedupe switch or the `no OTLP logs received` warning (**Decided**).
- Startup ordering (CLAUDE.md): nothing new may `os.Exit` after the child is launched.
- Builder decisions this plan makes where the spec leaves room are marked **Decided:** in the task that owns them.

## Review Focus

1. **A span batch flushed as the child exits.** The SDK's final export can arrive after the child is gone; it must be written, and nothing may be written after the writers close. Spans are delivered under the same `dmu` read lock as logs, and `Drain` still runs before `closeLog`. Tests: Task 2 `TestDrainWaitsForTraffic` extended to traces, `TestNoSpanDeliveryAfterDrain`.
2. **Pruning across writers.** Five-day-old `loghorn-*.traces.jsonl` next to five-day-old `loghorn-*.log`: each writer prunes only its own; `-historical` never lists span files. Tests: Task 1 `TestLogPruneSparesSpanFiles`, `TestSpanPruneSparesLogFiles`, `TestHistoricalFilesIgnoresSpanFiles`.
3. **A late write after `Close`** (a handler racing shutdown) must not create `loghorn.traces.jsonl` outside the lock. Test: Task 1 `TestCompanionWriteAfterCloseCreatesNothing`.
4. **OTLP/JSON span, parent and link ids are hex.** Without the rewrite `protojson` decodes them as base64 and stores wrong ids. Test: Task 2 `TestJSONHexSpanIDs`.
5. **Midnight before the first span / leftover from an earlier day.** A `loghorn.traces.jsonl` left from yesterday is archived as `loghorn-<its mtime day>.traces.jsonl` when the companion is created, and a run that crosses midnight before its first span still opens a fresh file for the new day. Tests: Task 1 `TestCompanionArchivesLeftoverAtCreate`, `TestCompanionFirstWriteAfterMidnight`.

---

## File Structure

| Path | Responsibility |
|---|---|
| `internal/logfile/writer.go` | `Writer` parameterised by base name; `Companion`; lazy open; companions closed before the lock |
| `internal/logfile/history.go` | `HistoricalFiles` passes `LogBase` to the now-parameterised name parser |
| `internal/otlp/spans.go` (new) | `MapSpans`: OTLP `Span` → stored JSON line |
| `internal/otlp/receiver.go` | `/v1/traces` decoded and delivered; `NewReceiver` takes a span callback; hex-id rewrite for spans |
| `internal/otlp/env.go` | three traces variables |
| `wrapper.go` | creates the span companion under `-otel`, passes its sink to the receiver |
| `main.go` | usage and `-otel` flag text |
| `README.md` | `-otel` section: spans file and traces variables |
| `otel_e2e_test.go` | child posts a span; span file checked; no file without spans |

---

### Task 1: `logfile.Writer` by base name, and the span companion

**Model:** sonnet · **Review:** full (deletes files: naming and pruning; locking: companion shares and must close inside the log writer's lock)

**Files:**
- Modify: `internal/logfile/writer.go`, `internal/logfile/history.go`
- Test: `internal/logfile/writer_test.go`, `internal/logfile/rollover_test.go`, `internal/logfile/history_test.go`, new `internal/logfile/companion_test.go`

**Interfaces:**
- Consumes: nothing new.
- Produces (package `github.com/jimbeveridge/loghorn/internal/logfile`):
  - `const LogBase = "loghorn"`, `const SpansBase = "loghorn-spans"`.
  - `func Open(dir string, clock func() time.Time, loc *time.Location) (*Writer, error)` — **unchanged signature**; the writer it returns has base `LogBase`.
  - `func (w *Writer) Companion(base string) (*Writer, error)` — returns a writer for `base` in the same dir with the same clock and location, holding no lock of its own. Before returning it archives a leftover `<base>.log` from an earlier day (mtime rule, as `start`) and prunes `<base>-<date>.log` archives, but **does not create** `<base>.log`; the file is opened (with the full leftover-archive/prune/open sequence) on the first `Write`. Errors: base equal to `w`'s own base, or `w` already closed → non-nil error, no writer. Call once per base.
  - `(*Writer).Write(rec []byte) error` and `(*Writer).Close() error` — same contracts as today, for both kinds. A companion's `Close` closes only its file. The log writer's `Close` closes every companion it created **before** releasing the lock. After `Close` (either one), a companion's `Write` returns nil and creates nothing.
  - Unexported, parameterised: `archiveName(base, name string) (string, bool)`, `parseArchiveDay(base, name string, loc *time.Location) (time.Time, bool)`; the writer derives its current name `base+".log"` and archive names `base+"-"+date+".log"`. `currentName`/`archivePrefix` constants go away (`HistoricalFiles` uses `LogBase+".log"` and `parseArchiveDay(LogBase, …)`).

**Decided:**
- Companion over a separate `Open`-like constructor: the spec puts the span writer under the log writer's lock, and tying its lifetime to the log writer's `Close` makes "file closed before lock released" structural rather than a caller ordering rule.
- The "stopped or closed" state becomes an explicit `closed bool` field (today it is `f == nil`), because a companion has `f == nil` before its first span too. `Write` opens lazily only when `!closed && f == nil`. A write failure sets `closed` as today's code sets `f = nil`.
- Lock ordering: the log writer's `Close` holds its own `mu` and then takes each companion's `mu`; a companion never takes its parent's `mu`. The parent's companion list is guarded by the parent's `mu`.
- No base-name validation beyond "not the parent's own" (callers pass the two constants).
- Package doc comment updated to say the package writes `loghorn.log` and, as a companion, `loghorn.traces.jsonl`, with the same rotation.

- [ ] **Step 1: Write the failing tests** (all in `t.TempDir()`, clock `at(…)` in `la`, reusing `openAt`, `putFile`, `exists`, `readFile`):
  - `TestLogPruneSparesSpanFiles` (writer_test.go): put `loghorn-2026-09-10.log`, `loghorn-2026-09-10.traces.jsonl`, `loghorn.traces.jsonl` (mtime today), `loghorn-spans-notadate.log`; `openAt(… at(2026,9,15,12,0))` → `loghorn-2026-09-10.log` gone; the three span files still exist, `loghorn.traces.jsonl` content unchanged.
  - `TestSpanPruneSparesLogFiles` (companion_test.go): `openAt(… at(2026,9,15,12,0))` on an empty dir, **then** put `loghorn-2026-09-09.log` and `loghorn-2026-09-10.traces.jsonl`, `loghorn-2026-09-14.traces.jsonl` (so only the companion's pruning can act), then `Companion(SpansBase)` → `loghorn-2026-09-10.traces.jsonl` gone, `loghorn-2026-09-14.traces.jsonl` kept, `loghorn-2026-09-09.log` and `loghorn.log` still exist.
  - `TestCompanionCreatesFileOnFirstWrite`: after `Companion(SpansBase)`, `loghorn.traces.jsonl` does not exist; `Write([]byte("a"))` → file is `"a\n"`, mode `0o600`; `loghorn.log` unaffected.
  - `TestCompanionArchivesLeftoverAtCreate`: `loghorn.traces.jsonl` with mtime `at(2026,9,14,22,0)`, open at `at(2026,9,15,10,0)`, `Companion` (no write) → `loghorn-2026-09-14.traces.jsonl` holds the old content, `loghorn.traces.jsonl` absent.
  - `TestCompanionFirstWriteAfterMidnight`: leftover `loghorn.traces.jsonl` mtime `at(2026,9,15,9,0)`; open and `Companion` at `at(2026,9,15,10,0)` (leftover kept); advance clock to `at(2026,9,16,1,0)`; `Write("b")` → `loghorn-2026-09-15.traces.jsonl` holds the leftover, `loghorn.traces.jsonl` is `"b\n"`.
  - `TestCompanionRollsOver`: write `"a"` on the 15th, `"b"` after midnight → `loghorn-2026-09-15.traces.jsonl == "a\n"`, `loghorn.traces.jsonl == "b\n"`, and `loghorn.log` / `loghorn-2026-09-15.log` untouched by the span rollover.
  - `TestCompanionWriteAfterCloseCreatesNothing`: two cases — closing the companion, and closing only the log writer — then `Write` returns nil and `loghorn.traces.jsonl` does not exist.
  - `TestCloseClosesCompanionBeforeLock`: companion written once; `w.Close()`; then a second `Open` on the same dir succeeds (lock released) and `Write` on the old companion returns nil without appending.
  - `TestCompanionErrors`: `Companion(LogBase)` → error; `Companion` after `w.Close()` → error.
  - `TestCompanionWriteFailureLeavesLogWriter` (rollover_test.go style, reusing however `TestWriteFailureStopsTheWriter` forces the failure): the companion's failure is returned once, later companion writes return nil, and the log writer still writes.
  - `TestHistoricalFilesIgnoresSpanFiles` (history_test.go): dir with `loghorn-2026-09-14.log`, `loghorn.log`, `loghorn-2026-09-14.traces.jsonl`, `loghorn.traces.jsonl` → exactly `[loghorn-2026-09-14.log, loghorn.log]`.

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/logfile/`
Expected: FAIL to compile (`Companion`, `SpansBase`, `LogBase` undefined).

- [ ] **Step 3: Implement** in `internal/logfile/writer.go` and `history.go`: add `base string`, `closed bool`, `companions []*Writer` to `Writer`; split `start(today)` into `settle(today)` (leftover archive + prune) and `open(today)`; `Companion` runs `settle(w.midnight(clock()))` under the parent's `mu`; the companion's `write` calls `start(w.midnight(now))` when `f == nil`, otherwise the existing rollover check; parameterise the name helpers by `w.base`.

- [ ] **Step 4: Verify**

Run: `go test -race -count=1 ./internal/logfile/ && gofmt -l . | grep -v '^.claude/' ; go vet ./... && CGO_ENABLED=0 go build ./... && go test ./... && go test -race ./...`
Expected: all PASS, gofmt prints nothing.

- [ ] **Step 5: Commit**

```bash
git add internal/logfile/
git commit -m "feat(logfile): writer by base name; span companion under the log lock"
```

---

### Task 2: Store spans from `/v1/traces`

**Model:** sonnet · **Review:** full (receiver concurrency and drain ordering — see the `Drain` doc comment in `internal/otlp/receiver.go`; untrusted request bodies now fully decoded; child environment)

**Files:**
- Create: `internal/otlp/spans.go`, `internal/otlp/spans_test.go`
- Modify: `internal/otlp/receiver.go`, `internal/otlp/receiver_test.go`, `internal/otlp/env.go`, `internal/otlp/env_test.go`, `wrapper.go`, `main.go` (usage text, `-otel` flag help), `README.md` (`-otel` section), `otel_e2e_test.go`

**Interfaces:**
- Consumes (Task 1): `logfile.SpansBase`, `(*logfile.Writer).Companion(base string) (*logfile.Writer, error)`, `(*logfile.Writer).Write`.
- Produces (package `otlp`; imports `coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"`, `tracepb "go.opentelemetry.io/proto/otlp/trace/v1"`):
  - `func MapSpans(req *coltracepb.ExportTraceServiceRequest) [][]byte` — one compact JSON object (no trailing newline) per `Span`, in request order.
  - `func NewReceiver(logs, spans func(line []byte), now func() time.Time) *Receiver` — **signature change**: `spans` is the new second argument; `spans == nil` means traces are decoded and discarded. Every existing caller (`wrapper.go`, `receiver_test.go`'s `newServer` and `startReal`) is updated.
  - `ChildEnv(environ []string, endpoint, runID string) []string` — unchanged signature, three more variables.
  - In `package main` (`wrapper.go`): `func openSpanLog(logs *logfile.Writer) *logfile.Writer` (nil in → nil out; a `Companion` error is printed as `loghorn: span file: <err>` and returns nil, the run goes on) and `func spanSink(w *logfile.Writer, onFail func(error)) func(line []byte)` (nil writer → nil func, so the receiver discards; onFail runs at most once, printing `loghorn: span file stopped: <err>`).

**Mapping** (spec "Spans" table, plus **Decided:** points):
- `trace_id`, `span_id`: lowercase hex, always present (even if empty or zero — a span without them is malformed but still stored).
- `parent_span_id`: hex; omitted when empty or all zero.
- `name`: verbatim, always present.
- `kind`: `SPAN_KIND_INTERNAL`→`internal`, `SERVER`→`server`, `CLIENT`→`client`, `PRODUCER`→`producer`, `CONSUMER`→`consumer`; unspecified or unknown → omitted.
- `start`, `end`: `time.Unix(0, n).UTC().Format(time.RFC3339Nano)`; each omitted when its nanos are 0 (**Decided**).
- `duration_ms`: `float64(end−start)/1e6`, present only when both are non-zero and `end >= start` (**Decided**).
- `status`: `STATUS_CODE_OK`→`ok`, `STATUS_CODE_ERROR`→`error`, unset → omitted. `status_message`: present whenever non-empty, regardless of code.
- `attributes`: object, keys verbatim, values via the existing `anyValue`; omitted when there are none. No reserved-key renaming (span attributes live inside their own object).
- `events`: array of `{"time", "name", "attributes"}` — `time` as RFC 3339 nano UTC (omitted if 0), `attributes` omitted if none; all attributes kept whole (exception events included, no truncation). Omitted when no events.
- `links`: array of `{"trace_id", "span_id", "attributes"}` (hex; `attributes` omitted if none). Omitted when no links.
- Resource: `service.name` and `loghorn.run.id` promoted; every other resource attribute in `otel.resource` (omitted when empty) — same rule as logs. `otel.scope.name`/`otel.scope.version` omitted when empty.
- Not stored (**Decided**): `trace_state`, `flags`, `dropped_*_count`, schema URLs.
- Encoding: a struct with the fields in the spec table's order and `omitempty` (`duration_ms` as `*float64`), so lines read in a stable, spec-like order.

**Receiver (Decided):**
- `/v1/traces`: `unmarshal(body, isJSON, &coltracepb.ExportTraceServiceRequest{}, fixSpanHexIDs)` → 400 `malformed request` on failure (as logs; JSON that is valid but not a trace request is now a 400, not a 200). Lines from `MapSpans` are delivered to `spans` inside the same `r.dmu.RLock()` / `closed` check as log lines, before the reply. Reply: empty `ExportTraceServiceResponse` in the request's encoding.
- `/v1/metrics`: unchanged (`validate`, discard).
- `fixSpanHexIDs(doc map[string]any)`: walks `resourceSpans[].scopeSpans[].spans[]` rewriting `traceId` (32 hex), `spanId` and `parentSpanId` (16 hex), and each `links[]` `traceId`/`spanId`, from hex to base64. Factor the per-key rewrite out of `fixHexIDs` into a shared helper.
- Update the `Receiver` type comment (logs and traces kept, metrics discarded) and the `Drain` comment: deliveries to **both** callbacks stop before Drain returns; call Drain before closing the log writer (which closes the span writer).
- Spans do not go through the recorder, so `SawOTLP` and the dedupe switch are unaffected.

**Wiring (`wrapper.go`, `runCommand`):** under `o.otel`, after `openRecordingLog`, `spans := openSpanLog(logs)`; `rcv = otlp.NewReceiver(rec.OTLP, spanSink(spans, …), nil)`. No new close call: `closeLog(logs)` (already after `rcv.Drain`) closes the companion. Without a log file (lock held elsewhere) `spans` is nil and traces are accepted and discarded.

**Copy:** usage paragraph for `-otel` becomes "… the log records it receives are stored with the command's output, and spans in .loghorn/loghorn.traces.jsonl (kept and rotated like loghorn.log)." Flag help: `"receive the command's OTLP/HTTP logs and traces on 127.0.0.1 and record them (needs -- <command>)"`. README `-otel` section: list the three traces variables and the spans file.

- [ ] **Step 1: Write the failing tests**
  - `spans_test.go` (decode with `json.Decoder.UseNumber`):
    - `TestMapSpanFull`: resource `{service.name: "api", loghorn.run.id: "R", host.name: "h"}`, scope `{name: "lib", version: "1.2"}`, span with trace id `0102…10` (16 bytes), span id `a1…a8`, parent `b1…b8`, name `"GET /x"`, kind `SERVER`, start `1763584812000000000`, end `1763584812001500000`, status `ERROR` message `"boom"`, attribute `http.response.status_code` int 500 → `trace_id == "0102030405060708090a0b0c0d0e0f10"`, `span_id`, `parent_span_id` hex, `kind == "server"`, `start == "2025-11-19T20:40:12Z"`, `end == "2025-11-19T20:40:12.0015Z"`, `duration_ms == 1.5`, `status == "error"`, `status_message == "boom"`, `attributes == {"http.response.status_code": 500}`, `service.name == "api"`, `loghorn.run.id == "R"`, `otel.resource == {"host.name": "h"}`, `otel.scope.name == "lib"`, `otel.scope.version == "1.2"`.
    - `TestMapSpanRootAndUnset`: zero parent id, kind unspecified, status unset with empty message, no attributes/events/links, resource with only `service.name` → keys `parent_span_id`, `kind`, `status`, `status_message`, `attributes`, `events`, `links`, `otel.resource` all absent.
    - `TestMapSpanKinds`: table of the five kinds → the five strings.
    - `TestMapSpanStatusOK`: `STATUS_CODE_OK` → `"ok"`; unset code with message `"m"` → no `status`, `status_message == "m"`.
    - `TestMapSpanTimes`: start 0 → no `start`, no `duration_ms`; end < start → both times present, no `duration_ms`.
    - `TestMapSpanEventsAndLinks`: event `exception` at a set time with `exception.type`, `exception.message`, and a 10 000-byte `exception.stacktrace` → `events[0] == {"time": …, "name": "exception", "attributes": {…}}` with the stacktrace equal to the input; link → `{"trace_id": hex, "span_id": hex, "attributes": {"k": "v"}}`; an event with no attributes has no `attributes` key.
    - `TestMapSpansOrder`: two resources × two scopes × two spans → 8 lines in request order (names `s0`…`s7`).
  - `receiver_test.go` (update `newServer`/`startReal` to the new `NewReceiver(s.collect, spans.collect, fixedNow)` with a second `sink`):
    - Replace `TestTracesAndMetricsAccepted` with `TestTracesStored` (protobuf, JSON and gzip-protobuf bodies each carrying one span → 200, the body decodes as `ExportTraceServiceResponse` in the request's encoding, the span sink got one line per request with the right `name`, the log sink got nothing) and `TestMetricsDiscarded` (200, both sinks empty).
    - `TestJSONHexSpanIDs`: OTLP/JSON with hex `traceId`, `spanId`, `parentSpanId` and a link's `traceId`/`spanId` → stored hex equals the input hex.
    - `TestTracesNilSinkDiscards`: `NewReceiver(logs, nil, …)` → traces 200, no panic.
    - `TestBadBodies`: the traces cases now include JSON `{"resourceSpans": 5}` → 400.
    - `TestTracesTooLargeIs413`: a >16 MB traces body → 413, nothing delivered.
    - `TestDrainWaitsForTraffic`: add a traces post during the drain window and assert its span is delivered.
    - `TestNoSpanDeliveryAfterDrain`: after `Drain` returns, a traces post (if the server still accepts it) delivers nothing to the span sink.
  - `env_test.go`: `ChildEnv` sets `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT=http://127.0.0.1:9/v1/traces`, `OTEL_EXPORTER_OTLP_TRACES_PROTOCOL=http/protobuf`, `OTEL_TRACES_EXPORTER=otlp`; each replaces a pre-existing value in place (e.g. `OTEL_TRACES_EXPORTER=none` → `otlp`) and no key appears twice.
  - `otel_e2e_test.go`:
    - In `otelChild`, after the log record, POST one protobuf `ExportTraceServiceRequest` to `os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT")` (`Content-Type: application/x-protobuf`): resource attributes `service.name = "e2e"` and `loghorn.run.id = os.Getenv("LOGHORN_RUN_ID")`, one span `name "e2e.span"`, kind `SERVER`, set ids and times. Exit 97 if the POST fails or is not 200.
    - `TestOtelEndToEnd`: `.loghorn/loghorn.traces.jsonl` has exactly one line; it has `name == "e2e.span"`, `kind == "server"`, `service.name == "e2e"`, `loghorn.run.id == id`; and `loghorn.log` contains no line with `"e2e.span"`.
    - `TestOtelNoLogsWarns`: `.loghorn/loghorn.traces.jsonl` does not exist (created on first span).
    - `TestOtelChildEnvironment`: extend the echoed string with `$OTEL_EXPORTER_OTLP_TRACES_ENDPOINT|$OTEL_EXPORTER_OTLP_TRACES_PROTOCOL|$OTEL_TRACES_EXPORTER` and the regex with `http://127\.0\.0\.1:\d+/v1/traces\|http/protobuf\|otlp`.

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/otlp/ && go test -run 'TestOtel' .`
Expected: FAIL to compile (`MapSpans` undefined, `NewReceiver` argument count).

- [ ] **Step 3: Implement** `MapSpans` in `internal/otlp/spans.go`; the receiver changes, `fixSpanHexIDs` and doc comments in `receiver.go`; the three variables in `ChildEnv`'s `set` map and its append-order list (after the logs variables) and its doc comment; `openSpanLog`/`spanSink` and the `NewReceiver` call in `wrapper.go`; usage, flag help and README copy above.

- [ ] **Step 4: Verify**

Run: `go test -race -count=1 ./internal/otlp/ && go test -race -count=1 -run 'TestOtel' . && gofmt -l . | grep -v '^.claude/' ; go vet ./... && CGO_ENABLED=0 go build ./... && go test ./... && go test -race ./...`
Expected: all PASS, gofmt prints nothing.

- [ ] **Step 5: Commit**

```bash
git add internal/otlp/ wrapper.go main.go README.md otel_e2e_test.go
git commit -m "feat: -otel stores spans in .loghorn/loghorn.traces.jsonl"
```
