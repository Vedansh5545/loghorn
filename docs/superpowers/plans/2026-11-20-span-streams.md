# Span Streams in the Web View Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: execute with the `orchestrating-plans` skill (one task-runner subagent per task, each dispatching its own fresh reviewer). Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `-web` (live and replay) reads the stored span files and live spans, sorts each span into a stream by standard OTEL attributes, and shows those streams next to the log streams, kept apart by a `source` (`log` | `otel`).

**Architecture:** `record.Normalize` gains a span branch decided by line shape, plus an exported `record.NormalizeSpan` for the live hook; `Record` gains `Source`. Stream identity becomes (source, name) through store, schema inference, saved schemas (`schemas/otel/<name>.toml`), queries and the HTTP API. `main` preloads span files after log files (live and replay) and the `spanSink` hook adds live spans to the web store. The page carries `source` in its state, URL, schema calls and stream picker.

**Order, and why:** Task 1 is pure library (records only; nothing consumes spans yet, and every span still gets a stream that no API can select apart from logs until Task 2). Task 2 threads the source through every layer the server uses. Task 3 wires the files and the live hook in `main`. Task 4 is the page, which needs Task 2's API.

**Tech Stack:** Go 1.25 (standard library, BurntSushi/toml already in use); vanilla JS page under `internal/web/static/` (embedded with `go:embed`). No new modules.

**Spec:** `docs/superpowers/specs/2026-11-20-span-streams-design.md` (commit 33ff083). Read it with this plan.

## Global Constraints

- Every task ends with: `gofmt -l $(git ls-files '*.go')` prints nothing, `go vet ./...` clean, `go test ./...` and `go test -race ./...` pass.
- Commit each task directly to `main`, one commit per task (no branch, no merge commit). Commit messages end with the session's attribution lines.
- **loghorn is public.** Never write the name of any real app under test, its paths, its app-specific attribute prefix, vendor names taken from its spans, or run ids into code, tests, fixtures, docs or commit messages. Fixtures use the generic made-up spans in Task 1 (`example-model`, `example-queue`, `example.com`).
- **Ingestion is off-limits** except the `spanSink` hook in `wrapper.go`: no edits to `internal/ingest`, `internal/otlp`, `internal/recorder`, or `internal/logfile/writer.go`. Adding a read-only lister to `internal/logfile/history.go` is allowed. `-otel` writes and frames exactly as today.
- **Never pipe data into a loghorn binary** (`… | loghorn`): piped stdin is recorded to the project log. Manual checks run from a scratch dir (`mktemp -d`), with `< file` or a command after `--`. Tests never touch the repo's own `.loghorn`.
- Source values, verbatim: `log` (default; also what `""` means everywhere) and `otel`. Any other value is a 400 from the API.
- Span stream names, verbatim: `gen_ai`, `messaging`, `rpc`, `db`, `http.server`, `http.client`, `span`. Rule order exactly as the spec's table; first match wins.
- Log lines keep today's rule (`labels.schema`, else `http-request`, else dropped). A span is never dropped.
- Saved schemas: log streams at `<config>/schemas/<name>.toml` (unchanged), span streams at `<config>/schemas/otel/<name>.toml`. `schema.ValidStream` applies to the name in both. `FindConfigDir` unchanged.
- CLAUDE.md web notes apply: handler tests use `httptest.NewServer` (the existing `newFixture`); number values in hand-built test records are `json.Number`; rebuild (or re-run the demo) after editing `internal/web/static/*`; before a browser check, make sure the port is free (`lsof -nP -iTCP:<port> -sTCP:LISTEN`, kill by PID).
- Startup ordering (CLAUDE.md): span preload happens inside `startWeb`, which runs before the OTLP receiver is bound and before the child launches. Nothing new may `os.Exit` after the child is launched.
- Out of scope: deleting old stored runs (manual, outside this plan), trace trees, span events as records, `-filter`, `-historical` without `-web`, the CLI, merging the files, user-configurable rules.

## Review Focus

1. **A trace-correlated log line is not a span.** OTLP log records in `loghorn.log` carry `trace_id`/`span_id` but no `start`; they must keep the log rule (stream from `labels.schema`, else `http-request`, else dropped), and a span-shaped line that also carries `labels.schema` (either `labels` or `logging.googleapis.com/labels`) is a log line. Test: Task 1 `TestNormalizeLogLineWithSpanIDStaysLog`, `TestNormalizeSpanShapeWithLabelsSchemaIsLog`.
2. **Same name, two sources.** `db` from `labels.schema` and `db` spans must never mix: counts, inferred schema, query rows, saved files and the schema cache are all per (source, name); a URL or favorite without `source` means `log`. Tests: Task 2 `TestStreamsBySource` (store), `TestSpanAndLogStreamsApart` (web), `TestSaveLoadOtelSchemaPath`.
3. **A log stream literally named `otel`.** It saves to `schemas/otel.toml`, a file, while span schemas live in the directory `schemas/otel/`; neither shadows the other. Test: Task 2 `TestLogStreamNamedOtelSavesBesideOtelDir`.
4. **Span preload vs live hook.** A span is in the store exactly once: preloaded from the file at `startWeb` time, or added by `spanSink` afterwards; the receiver must still be bound after `startLiveWeb`. A span whose file write fails, or a run with no log file (lock held elsewhere), still reaches the store. Tests: Task 3 `TestSpanSinkStoreOnly`, `TestSpanSinkAddsWhenFileFails`; concurrency via `go test -race`.
5. **Attribute key collisions.** An attribute named like a top-level span key (`name`, `service.name`) or a synthesized one (`timestamp`, `severity`) is kept as `attributes.<key>` and never overwrites the span's value. Test: Task 1 `TestNormalizeSpanAttributeCollision`.

---

## File Structure

| Path | Responsibility |
|---|---|
| `internal/record/record.go` | `Record.Source`, `StreamKey`, `Key()`, `ParseSource`; span branch in `Normalize`; log records get `Source: SourceLog` |
| `internal/record/span.go` (new) | `SpanStream` rule table, `NormalizeSpan`, span shape test, field lifting |
| `internal/record/span_test.go` (new) | classifier table + record-shape tests |
| `testdata/web/spans.jsonl` (new) | generic span fixture, one line per rule plus overlaps |
| `internal/store/store.go` | counts per `StreamKey`; `StreamCount.Source` |
| `internal/schema/{infer,saved,types}.go` | `Schema.Source`; `Infer(source, stream, …)`; `LoadSaved(dir, source, stream)`; source-aware `SaveSchema` path |
| `internal/query/{query,run}.go` | `Query.Source`; validated; `Run` filters by (source, stream) |
| `internal/web/server.go` | `/api/streams` `{name, source, count}`; `?source=` on schema GET/PUT; cache by `StreamKey` |
| `internal/logfile/history.go` | `HistoricalSpanFiles` |
| `wrapper.go`, `replay.go` | span preload (live + replay); `spanSink` adds to the store |
| `internal/web/static/app.js` (+ `index.html` only if needed) | `state.source`, picker labels, URL, schema calls |
| `README.md` | span streams in `-web`; `schemas/otel/` path |

---

### Task 1: Span records and the stream classifier

**Model:** sonnet · **Review:** standard

**Files:**
- Create: `internal/record/span.go`, `internal/record/span_test.go`, `testdata/web/spans.jsonl`
- Modify: `internal/record/record.go`
- Test: `internal/record/span_test.go` (existing `internal/record/*_test.go` must still pass)

**Interfaces:**
- Consumes: nothing new.
- Produces (package `github.com/jimbeveridge/loghorn/internal/record`):
  - `const SourceLog = "log"`, `const SourceOtel = "otel"`.
  - `Record.Source string` (new field, after `Stream`). `Normalize` sets it to `SourceLog` for log records and `SourceOtel` for spans.
  - `type StreamKey struct { Source, Name string }` (comparable, used as map key).
  - `func (r Record) Key() StreamKey` — `Source` `""` maps to `SourceLog` (hand-built test records keep working).
  - `func ParseSource(s string) (string, bool)` — `""`→`(SourceLog, true)`, `log`/`otel`→itself,true; anything else →`("", false)`.
  - Stream name constants: `StreamGenAI = "gen_ai"`, `StreamMessaging = "messaging"`, `StreamRPC = "rpc"`, `StreamDB = "db"`, `StreamHTTPServer = "http.server"`, `StreamHTTPClient = "http.client"`, `StreamSpan = "span"`.
  - `func SpanStream(kind string, attrs map[string]any) string` — the spec's rule table; an attribute "is present" when its key exists with a non-nil value.
  - `func NormalizeSpan(line []byte) (Record, bool)` — false unless the line is a span (below); copies `line` into `Raw`.
  - `Normalize(e, in)` — in the `{`-JSON branch only, when the decoded object is span-shaped, returns the same record `NormalizeSpan` would. YAML documents are never spans.

**Decided:**
- Span shape: a JSON object with string `span_id`, string `start`, and **no** `schema` key in a `labels` object, a `logging.googleapis.com/labels` object, or a flat `labels.schema` key.
- Fields: every top-level key except `attributes` goes through the existing `flatten` (so `otel.resource` → `otel.resource.<k>`; `events`/`links` stay whole arrays). Then `timestamp` = `start` (the string) and synthetic `severity` = `"ERROR"` when `status == "error"`, else `"INFO"`. Then each attribute in sorted key order is written under its own key verbatim (`flatten(fields, key, v)`), unless that key is already in `fields`, in which case it goes under `"attributes."+key`. A non-object `attributes` is kept as the field `attributes`.
- `Time` = `start` parsed with `time.RFC3339Nano`; on parse failure `Time` stays zero (the store fills it). Numbers are `json.Number` (decode with `UseNumber`).
- No `message` field is synthesized.

- [ ] **Step 1: Create `testdata/web/spans.jsonl`** with exactly these nine lines (generic; trace id shared):

```
{"trace_id":"0af7651916cd43dd8448eb211c80319c","span_id":"a000000000000001","name":"chat example-model","kind":"client","start":"2026-11-17T17:01:01.000Z","end":"2026-11-17T17:01:02.500Z","duration_ms":1500,"attributes":{"gen_ai.operation.name":"chat","gen_ai.request.model":"example-model","http.request.method":"POST"},"service.name":"demo-svc","otel.resource":{"host.name":"host-1"},"otel.scope.name":"demo"}
{"trace_id":"0af7651916cd43dd8448eb211c80319c","span_id":"a000000000000002","name":"send task","kind":"producer","start":"2026-11-17T17:01:03.000Z","end":"2026-11-17T17:01:03.040Z","duration_ms":40,"attributes":{"messaging.system.name":"example-queue","rpc.system.name":"grpc"},"service.name":"demo-svc"}
{"trace_id":"0af7651916cd43dd8448eb211c80319c","span_id":"a000000000000003","name":"Lookup","kind":"client","start":"2026-11-17T17:01:04.000Z","end":"2026-11-17T17:01:04.010Z","duration_ms":10,"attributes":{"rpc.system":"grpc"},"service.name":"demo-svc"}
{"trace_id":"0af7651916cd43dd8448eb211c80319c","span_id":"a000000000000004","name":"SELECT items","kind":"client","start":"2026-11-17T17:01:05.000Z","end":"2026-11-17T17:01:05.008Z","duration_ms":8,"attributes":{"db.system.name":"sqlite","db.query.text":"select id from items where id = ?","db.response.returned_rows":3},"service.name":"demo-svc"}
{"trace_id":"0af7651916cd43dd8448eb211c80319c","span_id":"a000000000000005","name":"db open","kind":"internal","start":"2026-11-17T17:01:00.000Z","end":"2026-11-17T17:01:00.020Z","duration_ms":20,"attributes":{"db.system.name":"sqlite"},"service.name":"demo-svc"}
{"trace_id":"0af7651916cd43dd8448eb211c80319c","span_id":"a000000000000006","name":"GET /items/{id}","kind":"server","start":"2026-11-17T17:01:06.000Z","end":"2026-11-17T17:01:06.030Z","duration_ms":30,"attributes":{"http.request.method":"GET","http.route":"/items/{id}","http.response.status_code":200},"service.name":"demo-svc"}
{"trace_id":"0af7651916cd43dd8448eb211c80319c","span_id":"a000000000000007","name":"GET","kind":"client","start":"2026-11-17T17:01:07.000Z","end":"2026-11-17T17:01:07.120Z","duration_ms":120,"attributes":{"http.method":"GET","url.full":"https://example.com/"},"service.name":"demo-svc"}
{"trace_id":"0af7651916cd43dd8448eb211c80319c","span_id":"a000000000000008","name":"job.run","kind":"internal","start":"2026-11-17T17:01:08.000Z","end":"2026-11-17T17:01:08.200Z","duration_ms":200,"status":"error","status_message":"boom","attributes":{"name":"collide"},"events":[{"time":"2026-11-17T17:01:08.100Z","name":"retry"}],"service.name":"demo-svc"}
{"trace_id":"0af7651916cd43dd8448eb211c80319c","span_id":"a000000000000009","name":"SELECT orders","kind":"client","start":"2026-11-17T17:01:09.000Z","end":"2026-11-17T17:01:09.004Z","duration_ms":4.5,"attributes":{"db.system":"postgresql","db.query.text":"select * from orders"},"service.name":"demo-svc"}
```

Expected streams: `gen_ai`, `messaging`, `rpc`, `db`, `span`, `http.server`, `http.client`, `span`, `db` → `db` 2, `span` 2, the rest 1.

- [ ] **Step 2: Write the failing tests** in `internal/record/span_test.go`:
  - `TestSpanStream` — table over `(kind, attrs) → stream`: each rule once with current names; legacy `messaging.system`, `rpc.system`, `db.system`, `http.method`; overlaps: gen_ai+`http.request.method` (client) → `gen_ai`; `messaging.system.name`+`rpc.system.name` → `messaging`; `internal`+`db.system.name` → `span`; `server`+`db.system.name` → `span`; `client`+`http.request.method` only → `http.client`; `internal`+`http.request.method` → `span`; nil attrs → `span`; `db.system.name: nil` with kind client → `span`.
  - `TestNormalizeSpanFixture` — every line of `../../testdata/web/spans.jsonl` gives `ok`, `Source == SourceOtel`, and the stream sequence from Step 1.
  - `TestNormalizeSpanFields` — on fixture line 4: `Fields["db.query.text"] == "select id from items where id = ?"`, no key `attributes.db.query.text`, `Fields["db.response.returned_rows"] == json.Number("3")`, `Fields["duration_ms"] == json.Number("8")`, `Fields["timestamp"] == "2026-11-17T17:01:05.000Z"`, `Fields["severity"] == "INFO"`, `Time.Equal(2026-11-17T17:01:05Z)`, `string(Raw)` equals the line. On line 1: `Fields["otel.resource.host.name"] == "host-1"`, `Fields["service.name"] == "demo-svc"`. On line 8: `Fields["severity"] == "ERROR"`, `Fields["events"]` is a `[]any` of length 1.
  - `TestNormalizeSpanAttributeCollision` — line 8: `Fields["name"] == "job.run"`, `Fields["attributes.name"] == "collide"`; a hand-built span with attributes `{"service.name":"x","timestamp":"t","severity":"s"}` and top-level `service.name` `"svc"` keeps `service.name=="svc"`, `timestamp`==start, `severity=="INFO"`, and has `attributes.service.name=="x"`, `attributes.timestamp=="t"`, `attributes.severity=="s"`.
  - `TestNormalizeRoutesSpans` — `Normalize(adapter.New(config.Default().Input).ParseLine(line), config.Default().Input)` on fixture line 4 returns the same `Stream`, `Source` and `Fields` as `NormalizeSpan(line)`.
  - `TestNormalizeLogLineWithSpanIDStaysLog` — `{"time":"2026-11-17T17:00:00Z","trace_id":"t","span_id":"s","labels":{"schema":"api"},"message":"m"}` → stream `api`, `Source == SourceLog`; the same without `labels` → not ok (dropped, as today).
  - `TestNormalizeSpanShapeWithLabelsSchemaIsLog` — a span-shaped line plus `"logging.googleapis.com/labels":{"schema":"db"}` → `Source == SourceLog`, stream `db`; `NormalizeSpan` on it → false. `NormalizeSpan` on `not json`, `[]`, and `{"span_id":1,"start":"x"}` → false.
  - `TestRecordKeyAndParseSource` — `Record{Stream:"db"}.Key() == StreamKey{SourceLog,"db"}`; `ParseSource` cases from the Interfaces block.

- [ ] **Step 3: Run to see them fail**

Run: `go test ./internal/record/`
Expected: FAIL (undefined: `SpanStream`, `NormalizeSpan`, …).

- [ ] **Step 4: Implement** the Interfaces block in `internal/record/span.go` and `record.go`. `SpanStream` is a slice of `{stream string; match func(kind string, attrs map[string]any) bool}` checked in order, ending in `StreamSpan`. Log records from `Normalize` get `Source: SourceLog`.

- [ ] **Step 5: Run to see them pass**

Run: `go test ./internal/record/ ./internal/store/ ./internal/query/ ./internal/web/ ./internal/otlp/ && go vet ./...`
Expected: PASS (other packages unchanged in behaviour).

- [ ] **Step 6: Commit**

```bash
git add internal/record testdata/web/spans.jsonl
git commit -m "feat(record): classify spans into streams by OTEL attributes"
```

---

### Task 2: Stream identity is (source, name) through store, schema, query and API

**Model:** sonnet · **Review:** full (saved-schema file paths change; schema cache keying; every query path filters by source)

**Files:**
- Modify: `internal/store/store.go`, `internal/schema/infer.go`, `internal/schema/saved.go`, `internal/schema/types.go`, `internal/query/query.go`, `internal/query/run.go`, `internal/web/server.go`, `README.md` (the `schemas/<stream>.toml` line ~256: add `schemas/otel/<stream>.toml` for span streams)
- Test: `internal/store/store_test.go`, `internal/schema/saved_test.go`, `internal/schema/infer_test.go`, `internal/query/run_test.go`, `internal/web/server_test.go`

**Interfaces:**
- Consumes (Task 1): `record.StreamKey`, `Record.Key()`, `record.ParseSource`, `record.SourceLog`, `record.SourceOtel`.
- Produces:
  - `store.StreamCount{Name string; Source string; Count int}` (field order exactly this); `Store.Streams()` sorted by `Name`, then `Source`; counts keyed by `r.Key()`.
  - `schema.Schema.Source string` with tags `toml:"source,omitempty" json:"source"` (placed after `Stream`). Log schemas therefore save with no `source` line, as today.
  - `func schema.Infer(source, stream string, recs []record.Record) Schema` — filters by `r.Key() == StreamKey{source', stream}` where `source'` is `ParseSource(source)`'s value; sets `Schema.Source` to that value.
  - `func schema.LoadSaved(dir, source, stream string) (*Schema, error)` — path `schemas/<stream>.toml` for `log`, `schemas/otel/<stream>.toml` for `otel`; returns `nil, nil` for an unknown source, invalid name, or absent file; sets `Source` on the result.
  - `schema.SaveSchema(dir string, s Schema)` — unchanged signature; path chosen by `s.Source` (`""`/`log` → `schemas/`, `otel` → `schemas/otel/`, created with `MkdirAll`); unknown source → `ErrUnsavableStream`.
  - `schema.Effective` copies `inferred.Source` to the result.
  - `query.Query.Source string` with tags `json:"source,omitempty" toml:"source,omitempty"` (after `Stream`). `Validate` rejects a source `ParseSource` refuses: error text `unknown source "<s>"`. `Run` restricts to `r.Key() == StreamKey{src, stream}` where `src` is `q.Source`, else `s.Source`, else `log`.
  - HTTP:
    - `GET /api/streams` → `[{"name":…, "source":"log"|"otel", "count":…}]`.
    - `GET /api/schema?stream=&source=` and `PUT /api/schema?stream=&source=` — `source` optional (default `log`); unknown → 400 `unknown source`. PUT: a non-empty body `source` that differs from the parameter → 400 `body source does not match ?source=`; the saved schema gets the parameter's source.
    - `POST /api/query` body takes `source` (via `query.Query`).
    - `GET /api/record` uses `rec.Key()` for its schema.
  - `server.cache` becomes `map[record.StreamKey]cachedSchema`; `effective(k record.StreamKey)`.

- [ ] **Step 1: Update existing tests to the new signatures**, then write the new failing tests:
  - store: `TestStreamsBySource` — add `{Stream:"db"}`, `{Stream:"db",Source:"otel"}` ×2, `{Stream:"api",Source:"log"}` → `Streams()` = `[{api log 1} {db log 1} {db otel 2}]`. Existing literals become keyed (`{Name:"api",Source:"log",Count:1}`).
  - schema: `TestSaveLoadOtelSchemaPath` — `SaveSchema(dir, Schema{Stream:"db",Source:"otel",…})` returns `<dir>/schemas/otel/db.toml`, `<dir>/schemas/db.toml` does not exist, `LoadSaved(dir,"otel","db")` round-trips with `Source=="otel"`, `LoadSaved(dir,"log","db")` is nil. `TestLogStreamNamedOtelSavesBesideOtelDir` — save log `otel` and otel `db`; both files exist (`schemas/otel.toml`, `schemas/otel/db.toml`) and each loads back its own fields. `SaveSchema` with `Source:"bogus"` → `ErrUnsavableStream`. A saved log schema file has no `source` key (read the file, assert no `source =`).
  - schema: `TestInferBySource` — records `{db, log}` with field `a` and `{db, otel}` with field `b`: `Infer("otel","db",recs)` has only `b`, `Source=="otel"`; `Infer("","db",recs)` has only `a`, `Source=="log"`.
  - query: `TestRunFiltersBySource` — same mixed records; `Run` with `Query{Stream:"db",Source:"otel"}` has `StreamTotal==1`; `Query{Stream:"db"}` has `StreamTotal==1` over the log record; `Validate` with `Source:"x"` fails.
  - web: `TestSpanAndLogStreamsApart` — `newFixture(t, pino, spans)` (const `spans = "../../testdata/web/spans.jsonl"`); `/api/streams` contains `{db log 3}`, `{db otel 2}`, `{gen_ai otel 1}`, `{span otel 2}`; `POST /api/query` `{"stream":"db","source":"otel","range":{"from":"all"},"columns":["db.query.text","duration_ms"]}` → `matched==2`, `streamTotal==2`, column paths include `db.query.text`; the same without `source` → `streamTotal==3`; `GET /api/schema?stream=db&source=otel` has field `db.query.text` and `schema.source=="otel"`; `?source=bogus` → 400 via `wantError`.
  - web: `TestSchemaPutOtelWritesOtelDir` — `PUT /api/schema?stream=db&source=otel` with a body built from the GET → 200, response `path` ends with `schemas/otel/db.toml`, `schemas/db.toml` absent; a following `GET ?stream=db&source=otel` shows `saved:true` fields, `GET ?stream=db` (log) shows none saved; PUT with body `"source":"log"` and `?source=otel` → 400.

- [ ] **Step 2: Run to see them fail**

Run: `go test ./internal/store/ ./internal/schema/ ./internal/query/ ./internal/web/`
Expected: FAIL (compile errors on new fields/signatures, then the new assertions).

- [ ] **Step 3: Implement** the Interfaces block. `schemaPath(dir, source, stream string) string`. In `server.go`, parse `source` once per handler with `record.ParseSource`. Update every `Infer`/`LoadSaved` caller (`grep -rn 'Infer(\|LoadSaved(' internal`).

- [ ] **Step 4: Run to see them pass**

Run: `go test -race ./internal/... && go vet ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/store internal/schema internal/query internal/web/server.go internal/web/server_test.go README.md
git commit -m "feat(web): streams are identified by source and name"
```

---

### Task 3: Read span files and live spans into the web store

**Model:** sonnet · **Review:** full (live hook adds to the store from receiver goroutines; preload must precede the receiver)

**Files:**
- Modify: `internal/logfile/history.go`, `wrapper.go`, `replay.go`, `README.md` (the `-otel` section line ~106: spans now appear in `-web` as span streams; still not in `loghorn.log` or `-filter`)
- Test: `internal/logfile/history_test.go`, `main_test.go`

**Interfaces:**
- Consumes: `record.NormalizeSpan(line []byte) (record.Record, bool)` (Task 1); `store.Preload(s, paths, in)` (unchanged — it now routes span lines through `Normalize`'s span branch); `store.StreamCount{Name, Source, Count}` (Task 2).
- Produces:
  - `func logfile.HistoricalSpanFiles(dir string) ([]string, error)` — same contract as `HistoricalFiles` with `SpansBase`/`SpansSuffix`: `loghorn-YYYY-MM-DD.traces.jsonl` oldest first, then `loghorn.traces.jsonl`; missing dir → `nil, nil`. Both functions share one unexported `historicalFiles(dir, base, suffix string)`; `HistoricalFiles` behaviour unchanged.
  - `func preloadStored(st *store.Store, dir string, in config.Input)` (package `main`, `wrapper.go`) — log files then span files; errors print `loghorn: -web: preload: <err>` to stderr and do not exit. Replaces the inline preload in `startWeb` (live mode only, as today).
  - `func preloadSpans(st *store.Store, dir string, in config.Input)` — span files only; used by `preloadStored` and by `runReplay`.
  - `func spanSink(w *logfile.Writer, st *store.Store, onFail func(error)) func(line []byte)` — nil only when both `w` and `st` are nil. Per line: write to `w` if non-nil (first failure → `onFail` once, as today); then, independently of the write's result, if `st` is non-nil and `record.NormalizeSpan(line)` is ok, `st.Add(r, time.Now())`.

**Decided:**
- `runCommand` passes `ws.store()` to `spanSink`; its order is unchanged (`startLiveWeb` → … → `otlp.Listen` → `NewReceiver`), which is what keeps preload and the hook from overlapping. Add a one-line comment at the `spanSink` call saying so.
- `runReplay`: after the source loop, `if ws != nil && kind == kindHistorical`, preload span files from `logDir()` into `ws.st`. `-historical` with no log files still exits as today (span-only dirs are out of scope).
- `internal/web/demo` needs no change: `go run ./internal/web/demo pino.log spans.jsonl` already preloads both through `store.Preload`.

- [ ] **Step 1: Write the failing tests**
  - `internal/logfile/history_test.go`: `TestHistoricalSpanFiles` — temp dir with `loghorn.log`, `loghorn-2026-11-18.log`, `loghorn.traces.jsonl`, `loghorn-2026-11-19.traces.jsonl`, `loghorn-2026-11-18.traces.jsonl`, `loghorn.lock`, a dir `loghorn-2026-11-17.traces.jsonl` → exactly `[…-11-18.traces.jsonl, …-11-19.traces.jsonl, loghorn.traces.jsonl]`; `HistoricalFiles` on the same dir is unchanged (`[loghorn-2026-11-18.log, loghorn.log]`); missing dir → `nil, nil`.
  - `main_test.go`: update `TestSpanSinkWithNoWriterIsNil` to `spanSink(nil, nil, …)`; `TestSpanSinkStoreOnly` — `spanSink(nil, st, …)` non-nil; feeding fixture line 4 of `testdata/web/spans.jsonl` gives `st.Streams() == [{db otel 1}]`; a non-span line adds nothing. `TestSpanSinkAddsWhenFileFails` — extend the existing failure setup in `TestSpanSinkWritesCompanionAndFailsOnce` (directory squatting on the archive name) with a store: after the failing write, the span is still in the store and `onFail` ran once.
  - `main_test.go`: `TestPreloadStoredReadsLogsAndSpans` — temp dir with `loghorn-2026-11-17.log` (copy of `testdata/web/pino.log`), `loghorn-2026-11-17.traces.jsonl` (first 4 fixture lines), `loghorn.traces.jsonl` (remaining 5) → `Streams()` includes `{db log 3}`, `{db otel 2}`, `{span otel 2}`, and the last record's `Fields["span_id"] == "a000000000000009"` (archive read before current).
- [ ] **Step 2: Run to see them fail**

Run: `go test ./internal/logfile/ . -run 'Historical|SpanSink|PreloadStored'`
Expected: FAIL (undefined `HistoricalSpanFiles`, `preloadStored`; `spanSink` arity).

- [ ] **Step 3: Implement** the Interfaces block in `history.go`, `wrapper.go` (`startWeb`, `spanSink`, the call in `runCommand`) and `replay.go`; update README.

- [ ] **Step 4: Run to see them pass**

Run: `go test -race ./... && go vet ./...`
Expected: PASS.

- [ ] **Step 5: Manual check against the binary** (scratch dir, no pipe into loghorn):

```bash
go build -o /tmp/lh-span ./ && D=$(mktemp -d) && mkdir -p $D/.loghorn && \
cp testdata/web/pino.log $D/.loghorn/loghorn.log && cp testdata/web/spans.jsonl $D/.loghorn/loghorn.traces.jsonl && \
(cd $D && /tmp/lh-span -web -web-port 8791 < /dev/null &) ; sleep 2; \
curl -s -H 'Host: 127.0.0.1:8791' http://127.0.0.1:8791/api/streams; \
kill $(lsof -nP -tiTCP:8791 -sTCP:LISTEN)
```
Expected: the JSON lists log `db`/`api`/… and otel `db` (2), `gen_ai`, `messaging`, `rpc`, `http.server`, `http.client`, `span` (2).

- [ ] **Step 6: Commit**

```bash
git add internal/logfile/history.go internal/logfile/history_test.go wrapper.go replay.go main_test.go README.md
git commit -m "feat(web): preload span files and add live spans to the store"
```

---

### Task 4: The page selects span streams; repo-wide verification

**Model:** sonnet · **Review:** standard

**Files:**
- Modify: `internal/web/static/app.js` (and `index.html`/`app.css` only if a change is truly needed)
- Test: browser check (no JS test harness exists); `go test ./internal/web/` keeps `TestIndexServed`/`TestStaticAssetsServed` green

**Interfaces:**
- Consumes (Task 2): `/api/streams` items `{name, source, count}`; `?source=` on `GET`/`PUT /api/schema`; `source` in the `POST /api/query` body; `schema.source` in the schema response.
- Produces: page state `state.source` (`"log"` | `"otel"`), sent in every query body (the body is `state`); favorites saved from the page carry `source`.

**Decided:**
- `streamKey()` returns `state.source + ":" + state.stream`; `lastResultStream` stores that key, and every `lastResultStream === state.stream` comparison compares keys instead.
- Picker option `value` = `source + ":" + name` (split on the first `:` when read; sources never contain `:`). Label: `name (count)` when the name exists under one source; `name (source, count)` when it exists under both — e.g. `db (log, 3)` and `db (otel, 2)`.
- `applyQuery`: `state.source = q.source === "otel" ? "otel" : "log"`. `saveURL` omits `source` when it is `log` and always writes it when `otel` (old URLs and favorites still mean `log`).
- `loadSession` keeps the current stream only if a stream with the same name **and** source exists; the fallback (largest count) sets both.
- Schema calls go through one helper `schemaURL(stream, source)` → `"/api/schema?stream=" + enc(stream) + "&source=" + enc(source)`. Every "is the loaded schema the current stream" check (`defaultColumns`, `saveFieldFormat`, the schema editor save) also compares `schema.source` to the source captured with the stream (`saveFieldFormat` captures `source` beside `stream` and re-checks both after each await).
- The one-row probe query (`api("POST", "/api/query", { stream: … , limit: 1 })`) also sends `source`.
- `loadFavorite` captures and restores `streamKey()` instead of `state.stream`. A favorite's menu title shows `name (otel) · ` for otel queries.
- Schema dialog title: `Schema · name`, plus ` (otel)` for otel streams.

- [ ] **Step 1: Implement** the decisions above in `app.js` (`grep -n 'state.stream\|lastResultStream\|/api/schema' internal/web/static/app.js` lists every site).

- [ ] **Step 2: Go checks**

Run: `go test ./internal/web/ && node --check internal/web/static/app.js`
Expected: PASS, no syntax error.

- [ ] **Step 3: Browser check** (port free first: `lsof -nP -iTCP:8790 -sTCP:LISTEN`, kill any PID shown). From a scratch dir so saved schemas don't land in the repo:

```bash
R=$PWD; D=$(mktemp -d) && cd $D && git init -q && go run -C $R ./internal/web/demo -port 8790 $R/testdata/web/pino.log $R/testdata/web/spans.jsonl
```
(If `go run -C` with a relative package doesn't resolve, build first: `go build -C $R -o $D/demo ./internal/web/demo` and run `./demo …`.) Open `http://127.0.0.1:8790/` and confirm:
  1. The picker shows `db (log, 3)` and `db (otel, 2)`, and plain `gen_ai (1)`, `span (2)`, `http.server (1)` …
  2. Choosing `db (otel, 2)`: Samples shows 2 rows; the column menu offers `db.query.text`, `duration_ms`; the URL contains `"source":"otel"`. Reload the page: still on otel `db`.
  3. Choosing `db (log, 3)`: 3 rows, URL has no `source`.
  4. Graph view on otel `db` with an aggregate over `duration_ms` draws without error.
  5. Change a field's format on otel `db` (column menu) → `$D/.config/loghorn/schemas/otel/db.toml` exists and `$D/.config/loghorn/schemas/db.toml` does not.
  6. Save a favorite on otel `db`, switch to log `db`, load the favorite → back on otel `db`.
  7. No console errors.
Stop the demo (Ctrl-C) afterwards.

- [ ] **Step 4: Repo-wide verification**

Run: `gofmt -l $(git ls-files '*.go'); go vet ./... && go test ./... && go test -race ./... && CGO_ENABLED=0 go build ./...`
Expected: no gofmt output; all PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/web/static
git commit -m "feat(web): stream picker and URL carry the span source"
```
