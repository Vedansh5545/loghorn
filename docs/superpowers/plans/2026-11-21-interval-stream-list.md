# Interval-Aware Stream List and Per-Service Config Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: execute with the `orchestrating-plans` skill (one task-runner subagent per task, each dispatching its own fresh reviewer). Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** The web view lists the kinds (span classification, else span name; log stream for logs) present in the selected interval, per service, with pins, a Logs/Spans switch and a service selector; pins, favorites and field settings move to `~/.config/loghorn/services/<key>/`.

**Architecture:** A span's `Record.Stream` becomes its kind, so the one-source-one-stream query path is unchanged. `Record` gains `Service`; the store fills a fallback (`default`, or the child's `OTEL_SERVICE_NAME` under `-- cmd`). `store.Kinds` scans the store once per call for interval counts. A new `internal/svcconf` package owns the per-service directory, key escaping, pins and atomic writes; `schema` replaces per-stream saved schemas with per-source field files merged by path. The server reaches config through `Options.ConfigBase`, never by walking the tree. The page replaces the stream `<select>` with an interval list.

**Order, and why:** Tasks 1–2 are library (records, store, query); Task 3 is config on disk; Task 4 threads both through the HTTP API and `main`; Tasks 5–6 are the page, which needs Task 4's API. Each task ends green.

**Tech Stack:** Go 1.25, standard library, BurntSushi/toml (already used); vanilla JS under `internal/web/static/` (embedded). No new modules.

**Spec:** `docs/superpowers/specs/2026-11-21-interval-stream-list-design.md` (commit 401dfa3). Builds on `docs/superpowers/specs/2026-11-20-span-streams-design.md`. Read the spec with this plan.

## Global Constraints

- Every task ends with: `gofmt -l $(git ls-files '*.go')` prints nothing, `go vet ./...` clean, `go test ./...` passes. Tasks 2, 4 and 6 also run `go test -race ./...`.
- Commit each task on the `interval-streams` branch in the worktree `/Users/jim/code/loghorn/.claude/worktrees/interval-streams` (not `main`), one commit per task. Commit messages end with the session's attribution lines.
- **loghorn is public.** No name of any real app, its attribute prefix, vendors or run ids in code, tests, fixtures, docs or commits. Fixtures are generic and made up (`shop-api`, `mail`, `sweep orders`, `example-model`, `example.com`).
- **Ingestion is off-limits** except reading `OTEL_SERVICE_NAME` in `wrapper.go`: no edits to `internal/ingest`, `internal/otlp`, `internal/recorder`, `internal/logfile`. (The fallback service is applied in `store.Add`, not the recorder.)
- **Never pipe data into a loghorn binary.** Manual checks use the demo (`go run ./internal/web/demo FILE`) from a scratch dir.
- **Tests never touch the real config.** Any test that resolves or writes config sets `t.Setenv("HOME", t.TempDir())` and `t.Setenv("XDG_CONFIG_HOME", t.TempDir())` (or passes an explicit temp base). Browser checks run the demo with `XDG_CONFIG_HOME=$(mktemp -d)`.
- Values, verbatim: sources `log` | `otel`; span types `gen_ai`, `messaging`, `rpc`, `db`, `http.server`, `http.client`, `span`; the new span field `span.type`; fallback service `default`; all-services value `*`; config files `queries.toml`, `pins.toml`, `fields-log.toml`, `fields-otel.toml`; directory `<base>/services/<escaped key>/` with `<base>` = `$XDG_CONFIG_HOME/loghorn` when set, else `~/.config/loghorn`; the read-only tooltip `Choose a service to save`; top-N = 10.
- Kinds budget: `store.Kinds` ≤ 50 ms over 200k records, enforced by a test (Task 2).
- CLAUDE.md web notes apply: handler tests use `httptest.NewServer` (the existing `newFixture`); numbers in hand-built records are `json.Number`; rebuild or re-run the demo after editing `internal/web/static/*`; keep `[hidden]{display:none!important}` in `app.css`; new sidebar controls are `.sb-field` rows (`.sb-l` + `.sb-body`); a button inside a control that redraws on blur/change does `preventDefault` on `mousedown`; any metrics edit reconciles `state.plot`/`state.order` (`syncAggregates`); optional int TOML fields carry `omitzero`; queued schema saves re-apply their patch when they run; before a browser check free the port (`lsof -nP -iTCP:<port> -sTCP:LISTEN`, kill by PID).
- Startup ordering (CLAUDE.md): the migration notice and anything that can `os.Exit` run before the child is launched.
- Out of scope: listing values within a kind, choosing the grouping, cross-kind queries, a new query engine, automatic migration, writing the service into stored lines.

## Review Focus

1. **A service key that escapes the config tree.** Keys `..`, `.`, `a/b`, `../x`, `%`, empty, very long, and `*` must never resolve outside `<base>/services/`; escaping round-trips. Leading `.` is escaped as `%2E` (a deliberate tightening of the spec's rule so `.`/`..` and hidden dirs can't occur). Test: Task 3 `TestServiceDirStaysUnderBase`, `TestEscapeKeyRoundTrip`.
2. **Two field saves for the same source at once** (e.g. two kinds edited in quick succession, or two tabs) must both land; a save never writes inferred types of untouched fields; an unreadable existing `fields-*.toml` is an error, never overwritten. Tests: Task 3 `TestMergeFieldsKeepsOthers`, `TestMergeFieldsBadFileNotOverwritten`; Task 4 `TestSchemaPutConcurrentMerge`.
3. **Counting while live records arrive.** `Kinds` runs under the store's read lock while `Add` appends spans and log lines from other goroutines. Test: Task 2 `TestKindsConcurrentAdd` under `-race`.
4. **Span field collisions.** An attribute literally named `span.type`, or a span with an empty `name`, must not break the kind: the attribute moves to `attributes.span.type`; an unclassified span with no name has kind `span`. Test: Task 1 `TestSpanTypeAttributeCollision`, `TestKindEmptyName`.
5. **A selection with nothing in range.** A selected service or kind absent from the interval stays selected: `/api/kinds` returns empty lists without error, `/api/query` returns 0 rows, pinned kinds show greyed `0`; an old URL naming the retired `span` stream falls back to the top kind without an error banner. Tests: Task 4 `TestKindsUnknownServiceEmpty`; browser checks in Tasks 5–6.

---

## File Structure

| Path | Responsibility |
|---|---|
| `internal/record/record.go`, `span.go` | `Record.Service`, `ServiceKey`, `DefaultService`, `span.type`, kind as `Stream` |
| `internal/store/store.go` (+ `race_on_test.go`/`race_off_test.go`) | fallback service in `Add`, `SetFallbackService`, `Kinds`, `Count` |
| `internal/query/query.go`, `run.go` | `Query.Service`, exported `Range.Resolve`, service filter, interval default columns |
| `internal/schema/infer.go` | `Infer(service, source, stream, recs)`, exported `DefaultColumns(source, fields)` |
| `internal/schema/saved.go` | `LoadFields`, `MergeFields`, new `Effective`; per-stream `LoadSaved`/`SaveSchema`/`ValidStream` deleted; `FindConfigDir` kept (migration notice only) |
| `internal/svcconf/svcconf.go` (new) | `Base`, `EscapeKey`, `UnescapeKey`, `Dir`, `WriteAtomic` |
| `internal/svcconf/pins.go` (new) | `Pin`, `LoadPins`, `SavePins` |
| `internal/web/server.go` | `Options.ConfigBase`; `/api/kinds`, `/api/pins`; `service` on query/schema/favorites; cache per (service, source, kind) |
| `wrapper.go`, `replay.go`, `main.go`, `migrate.go` (new), `internal/web/demo/main.go` | `ConfigBase` wiring, `OTEL_SERVICE_NAME` fallback, one-time migration notice |
| `testdata/web/services.jsonl` (new) | generic multi-service fixture (logs + spans, > 10 kinds, spread over an hour) |
| `internal/web/static/{app.js,index.html,app.css}` | mode switch, service selector, interval list, pins, All read-only, per-field saves |
| `README.md` | per-service config location and the manual copy from a repo `.config/loghorn/` |

---

### Task 1: Kinds and services on records

**Review:** standard. **Model:** default.

**Files:**
- Modify: `internal/record/record.go`, `internal/record/span.go`, `internal/store/store.go`, `wrapper.go`
- Test: `internal/record/span_test.go`, `internal/record/record_test.go`, `internal/store/store_test.go`, `main_test.go` (update the `span` expectation at line ~206), `wrapper_test.go` or `main_test.go` for `childService`

**Interfaces:**
- Produces:
  - `record.Record.Service string` — the service key; `""` from `Normalize`/`NormalizeSpan` when the record names none (the store fills it).
  - `const record.DefaultService = "default"`
  - `func record.ServiceKey(fields map[string]any) string` — first non-empty string of `fields["otel.resource.service.namespace"]`, `fields["service.namespace"]`, `fields["service.name"]`, `fields["resource.labels.service_name"]`; else `""`.
  - Span records: `Fields["span.type"]` = `SpanStream(kind, attrs)`; `Record.Stream` = that type unless it is `span`, then the span's `name` (string), and `span` when the name is empty.
  - `func (s *Store) SetFallbackService(key string)` — nil-receiver safe; `Add` sets `r.Service` to the fallback (initially `record.DefaultService`) when `r.Service == ""`.
  - `func childService(env []string) string` in `wrapper.go` — the value of the last `OTEL_SERVICE_NAME=` entry, `""` if none.

- [ ] **Step 1: Write the failing tests**
  - `TestSpanKind` (table over spans built as JSON strings): `{"kind":"client","attributes":{"db.system.name":"x"},"name":"SELECT t"}` → `Stream == "db"`, `Fields["span.type"] == "db"`; `{"kind":"internal","name":"sweep orders"}` → `Stream == "sweep orders"`, `Fields["span.type"] == "span"`.
  - `TestKindEmptyName`: unclassified span with `"name":""` → `Stream == "span"`.
  - `TestSpanTypeAttributeCollision`: attribute `"span.type":"mine"` → `Fields["span.type"] == "span"`, `Fields["attributes.span.type"] == "mine"`.
  - `TestServiceKey` (table): span with `service.name` only → that name; span with `otel.resource.service.namespace:"mail"` and `service.name:"mail-worker"` → `"mail"`; OTLP log line `{"service.name":"api","labels":{"schema":"db"},…}` via `Normalize` → `"api"`; GCP LogEntry with `resource.labels.service_name:"web"` → `"web"`; pino line → `""`.
  - `TestStoreFallbackService`: `Add` with `Service:""` → stored `"default"`; after `SetFallbackService("shop-api")` → `"shop-api"`; a record with `Service:"x"` stays `"x"`; `(*Store)(nil).SetFallbackService("a")` doesn't panic.
  - `TestChildService`: `[]string{"PATH=/bin","OTEL_SERVICE_NAME=shop-api"}` → `"shop-api"`; none → `""`.
- [ ] **Step 2: Run** `go test ./internal/record/ ./internal/store/ . -run 'SpanKind|KindEmptyName|SpanTypeAttribute|ServiceKey|FallbackService|ChildService'` — Expected: FAIL (undefined / wrong stream).
- [ ] **Step 3: Implement.** In `spanRecord` set `fields["span.type"]` *before* the attributes loop so a colliding attribute is moved by the existing `spanKeyTaken` logic. Set `Service: ServiceKey(fields)` in both `spanRecord` and `Normalize`. In `store.Store` add field `fallback string` initialised in `New` to `record.DefaultService`; `Streams()` counts by the new `Stream` unchanged. In `runCommand` (`wrapper.go`) right after `ws := startLiveWeb(...)` (the preload is done by then, so stored lines stay `default`): `if svc := childService(os.Environ()); svc != "" { ws.store().SetFallbackService(svc) }`. `runPipe` and replay set nothing.
- [ ] **Step 4: Update existing tests** that expect the stream `span` for named unclassified spans (`internal/record/span_test.go`, `main_test.go:206`, any in `internal/web/server_test.go`) to expect the span name.
- [ ] **Step 5: Run** `go test ./...` — Expected: PASS. `gofmt -l $(git ls-files '*.go')` empty; `go vet ./...` clean.
- [ ] **Step 6: Commit** `feat(record): span kind as stream, span.type field and record service key`

---

### Task 2: Interval counts and service-aware queries

**Review:** full (store locking, the budget). **Model:** default.

**Files:**
- Modify: `internal/store/store.go`, `internal/query/query.go`, `internal/query/run.go`, `internal/schema/infer.go`
- Create: `internal/store/race_on_test.go`, `internal/store/race_off_test.go` (copy the `internal/query` pattern: `const raceEnabled = true/false` under `//go:build race` / `!race`)
- Test: `internal/store/store_test.go`, `internal/query/run_test.go`, `internal/schema/infer_test.go`

**Interfaces:**
- Consumes: `Record.Service`, `Record.Stream` as kind (Task 1).
- Produces:
  - `type store.Count struct { Name string; Count int }`
  - `func (s *Store) Kinds(source, service string, from, to time.Time) (kinds, services []Count)` — one pass under `RLock`, no snapshot copy. A record counts when `Source` matches (`""` = log) and its time is in `[from, to)`; zero `from`/`to` are unbounded; a zero-time record counts only when `from` is zero. `services`: per service, for that source, ignoring `service`. `kinds`: per `Stream`, restricted to `service` unless it is `*`. Both sorted by count descending, then name ascending; never nil (empty slices).
  - `func (r query.Range) Resolve(now, runStart time.Time) (from, to time.Time, err error)` — the existing `resolve`, exported (callers updated).
  - `query.Query.Service string \`json:"service,omitempty" toml:"-"\`` — `Run` skips records whose `Service` differs unless `Service` is `""` or `*`. `toml:"-"` keeps it out of `queries.toml`.
  - `func schema.Infer(service, source, stream string, recs []record.Record) schema.Schema` — `service` `""` or `*` means every service.
  - `func schema.DefaultColumns(source string, fields []Field) []string` — the existing `defaultColumns`, exported, with leading paths `timestamp, severity, message` for log and `timestamp, name, duration_ms` for otel (each only if present), then the rest by `Count` desc, capped at `maxDefaultColumns`; skips `Hidden` fields, `labels.*` and `json` types as today. `Infer` uses it.
  - In `query.Run`, samples view with `len(q.Columns) == 0`: columns = `schema.DefaultColumns(src, fs)` where `fs` are the schema's fields with `Count` replaced by the number of *matched* records holding the path and zero-count fields dropped (interval inference; types still from `s`).

- [ ] **Step 1: Write the failing tests**
  - `TestKindsRangeEdges`: records at 10:00, 10:05, 10:10 (otel, service `a`, kinds `db`,`db`,`x`); `Kinds("otel","a", 10:00, 10:10)` → kinds `[{db 2}]`; `from` zero, `to` zero → `[{db 2} {x 1}]`.
  - `TestKindsServiceAll`: services `a` (3 recs) and `b` (1) → `Kinds("otel","*",…)` kinds include both services' kinds; `services == [{a 3} {b 1}]`; `Kinds("otel","b",…)` kinds only `b`'s, services still both.
  - `TestKindsEmpty`: empty store and unknown service → `kinds` and `services` are non-nil, length 0.
  - `TestKindsBudget`: skip when `raceEnabled`; build 200k records (3 services × 2 sources × 30 kinds, times 1 ms apart, `Fields` a small map) with `Add`; run `Kinds("otel","*",t0+10%,t0+90%)` 5 times; `min` elapsed must be `<= 50*time.Millisecond`, else `t.Fatalf` with the time.
  - `TestKindsConcurrentAdd`: one goroutine `Add`s 10k records while another calls `Kinds` in a loop; no race under `-race`.
  - `TestRunServiceFilter` (query): records of services `a`,`b` in the same stream; `Service:"a"` → only `a`'s; `"*"` and `""` → both.
  - `TestRunIntervalColumns`: field `foo` only on records before the range, `bar` only inside → result columns contain `bar`, not `foo`; with `q.Columns = ["foo"]` the columns are `["foo"]`.
  - `TestDefaultColumnsSpanLead`: otel fields `timestamp,name,duration_ms,severity,trace_id` → starts `timestamp,name,duration_ms`.
  - `TestInferService`: `Infer("a","otel","db",recs)` counts only service `a`.
- [ ] **Step 2: Run** `go test ./internal/store/ ./internal/query/ ./internal/schema/` — Expected: FAIL (undefined).
- [ ] **Step 3: Implement** the signatures above. `Kinds` uses two `map[string]int` and sorts. If `TestKindsBudget` misses 50 ms, add the spec's fallback (per-(service, source, kind) time-sorted index maintained in `Add`, two binary searches per kind) without changing the signature; record which one shipped in the commit message. Update the `Infer` callers (`internal/web/server.go` passes `"*"` for now; Task 4 threads the service) and `internal/query/run_test.go`.
- [ ] **Step 4: Run** `go test ./...` and `go test -race ./...` — Expected: PASS. `go test ./internal/store/ -run KindsBudget -v` prints PASS (not SKIP) without `-race`.
- [ ] **Step 5: Commit** `feat(store): interval kind and service counts; service-aware queries`

---

### Task 3: Per-service config on disk

**Review:** full (config writes, merge by path, path escaping). **Model:** default.

**Files:**
- Create: `internal/svcconf/svcconf.go`, `internal/svcconf/pins.go`, `internal/svcconf/svcconf_test.go`
- Modify: `internal/schema/saved.go`, `internal/schema/types.go`
- Test: `internal/schema/saved_test.go` (delete tests of removed functions; keep `FindConfigDir` tests)

**Interfaces:**
- Produces:
  - `func svcconf.Base(xdg, home string) (string, error)` — `xdg/loghorn` when `xdg != ""`, else `home/.config/loghorn`; error when both empty. Never walks, never creates.
  - `func svcconf.BaseFromEnv() (string, error)` — `Base(os.Getenv("XDG_CONFIG_HOME"), os.UserHomeDir())`.
  - `func svcconf.EscapeKey(key string) string` — `""` → `default`; bytes `[A-Za-z0-9._-]` kept except a `.` at index 0, which becomes `%2E`; every other byte `%XX` (upper-case hex).
  - `func svcconf.UnescapeKey(s string) (string, error)`
  - `func svcconf.Dir(base, key string) string` — `filepath.Join(base, "services", EscapeKey(key))`.
  - `func svcconf.WriteAtomic(path string, encode func(io.Writer) error) error` — `MkdirAll(dir, 0o755)`, temp file in the same dir, encode, chmod 0644, sync, close, rename (the pattern in `favorites.write`).
  - `type svcconf.Pin struct { Source string \`toml:"source" json:"source"\`; Kind string \`toml:"kind" json:"kind"\` }`
  - `func svcconf.LoadPins(dir string) ([]Pin, error)` — `dir/pins.toml` (`[[pin]]`), absent → `[]Pin{}`.
  - `func svcconf.SavePins(dir string, pins []Pin) error` — each `Source` through `record.ParseSource` (unknown → `svcconf.ErrBadPin`), `Kind` non-empty, duplicates dropped keeping first order, written with `WriteAtomic`.
  - `func schema.LoadFields(dir, source string) ([]Field, error)` — `dir/fields-<log|otel>.toml` (`[[field]]`), absent → nil; validates each with `validateField` (empty visualizer defaulted as `LoadSaved` did); sorted by path; `Saved = true`.
  - `func schema.MergeFields(dir, source string, fields []Field) (path string, err error)` — validate the input fields, `LoadFields` (an error aborts with nothing written), replace or add each by `Path`, sort, `svcconf.WriteAtomic`. Unknown source → `schema.ErrBadSource`. Callers serialise.
  - `func schema.Effective(inferred Schema, saved []Field) Schema` — applies `Type, Unit, Visualizer, Label, Hidden, Width, Wrap` of saved fields whose path is in `inferred` (sets `Saved`); saved paths absent from `inferred` are ignored (the file is source-wide); columns = `inferred.Columns` minus hidden.
  - Removed: `LoadSaved`, `SaveSchema`, `ValidStream`, `ErrUnsavableStream`, `schemaDir`, `schemaPath`, `Field.New` (and its `json:"new"`). `Schema.Columns` stays (inferred defaults) but is no longer saved. `FindConfigDir` stays (Task 4's migration notice).

- [ ] **Step 1: Write the failing tests** (every test sets temp `HOME` and `XDG_CONFIG_HOME`)
  - `TestBase`: `Base("/x","/h") == "/x/loghorn"`; `Base("","/h") == "/h/.config/loghorn"`; `Base("","")` errors. `TestBaseFromEnv` with `t.Setenv` both ways.
  - `TestEscapeKeyRoundTrip`: for `shop-api`, `a.b_c`, `ns/svc`, `..`, `.hidden`, `%`, `日本`, `*` → `UnescapeKey(EscapeKey(k)) == k`; `EscapeKey("") == "default"`; `EscapeKey("ns/svc") == "ns%2Fsvc"`; `EscapeKey("..") == "%2E."`.
  - `TestServiceDirStaysUnderBase`: for the keys above plus `../x`, `a/../../b`, a 300-byte key, `filepath.Rel(base+"/services", Dir(base,k))` has no `..` element and no separator.
  - `TestPinsRoundTrip`: save `[{otel db} {otel sweep orders} {otel db}]` → load `[{otel db} {otel sweep orders}]`; file contains `[[pin]]`; absent file → empty non-nil; source `bogus` → `ErrBadPin`.
  - `TestMergeFieldsKeepsOthers`: merge `[{path:"status", type:"int", visualizer:"plain"}]`, then `[{path:"latency", type:"duration", visualizer:"duration", width:120}]` → `LoadFields` returns both; re-merge `status` with `label:"code"` replaces only `status`; `fields-otel.toml` and `fields-log.toml` are separate files; no `width = 0` line in the file (omitzero).
  - `TestMergeFieldsBadFileNotOverwritten`: write garbage to `fields-log.toml`; `MergeFields` errors; file bytes unchanged.
  - `TestMergeFieldsRejectsInvalid`: unknown visualizer → error, nothing written.
  - `TestEffectiveSourceWide`: saved has `status` (type int) and `other` (absent from inferred) → result has `status` with type int and `Saved`; no `other`; hidden saved field dropped from columns.
- [ ] **Step 2: Run** `go test ./internal/svcconf/ ./internal/schema/` — Expected: FAIL.
- [ ] **Step 3: Implement** per Interfaces. `schema` imports `svcconf` for `WriteAtomic`; `svcconf` must not import `schema`. Fix compile errors in `internal/web/server.go` minimally (temporarily call `LoadFields(dir, source)` where `LoadSaved` was; `putSchema` calls `MergeFields(dir, src, sch.Fields)`; drop `ValidStream`) — Task 4 replaces the directory source.
- [ ] **Step 4: Run** `go test ./...` — Expected: PASS; gofmt/vet clean.
- [ ] **Step 5: Commit** `feat(config): per-service config dir, pins, and per-source field settings merged by path`

---

### Task 4: API, wiring and migration notice

**Review:** full (config writes through HTTP, the migration notice, startup order). **Model:** default.

**Files:**
- Modify: `internal/web/server.go`, `internal/web/demo/main.go`, `wrapper.go`, `replay.go`, `main.go`, `README.md`
- Create: `migrate.go` (package main), `testdata/web/services.jsonl`
- Test: `internal/web/server_test.go`, `migrate_test.go`

**Interfaces:**
- Consumes: `store.Kinds`, `query.Range.Resolve`, `Query.Service`, `schema.Infer(service,…)` (Task 2); `svcconf.*`, `schema.LoadFields/MergeFields/Effective` (Task 3).
- Produces (HTTP; the page relies on these exactly):
  - `web.Options{ Store *store.Store; ConfigBase string; Now func() time.Time }` — `StartDir` removed. Service dir = `svcconf.Dir(ConfigBase, service)`.
  - `service` parameter: `?service=` on GET/PUT `/api/schema`, GET `/api/favorites`, PUT/DELETE `/api/favorites/{name}`, GET/PUT `/api/pins`, GET `/api/kinds`; the `service` body field on POST `/api/query`. Missing → `default`. `*` = all services: allowed on reads of `/api/kinds`, `/api/schema` (inferred over all services, no saved fields applied) and `/api/query`; any write with `*`, and GET `/api/pins`/`/api/favorites` with `*`, → 400 `{"error":"Choose a service to save"}` (writes) / `{"error":"choose a service"}` (reads; the page loops per service).
  - `GET /api/kinds?source=&service=&from=&to=` → `{"kinds":[{"name":string,"count":int}],"services":[{"key":string,"count":int}]}`; `from`/`to` take the query's range values (`this-run`, `15m`, `all`, RFC 3339, `now`, empty) resolved with `query.Range{From,To}.Resolve(now, runStart)`; bad value or source → 400.
  - `GET /api/pins?service=k` → `{"pins":[{"source","kind"}]}`; `PUT /api/pins?service=k` body `{"pins":[…]}` → the saved list in the same shape; `ErrBadPin` → 400.
  - `PUT /api/schema?source=&service=` body `{"fields":[Field…]}` → `{"path":string}` (merge by path; `stream` param no longer needed and ignored). `GET /api/schema?stream=&source=&service=` unchanged shape (`{"schema","ops"}`), fields no longer carry `new`.
  - `GET /api/session` drops `configDir`/`configFound`, adds `"configBase": string`.
  - `/api/record` uses the record's own `Service` for its schema. `/api/streams` stays.
  - Schema cache key: `struct{ Service, Source, Name string }`.
  - `func repoConfigNotice(start, base string) string` (`migrate.go`) — `schema.FindConfigDir(start)`; if found and it holds `schemas/` or `queries.toml`, and the absolute repo config path is not a line of `<base>/notified-repos`, append it there (`os.MkdirAll` base) and return the line `loghorn: <repo dir> is no longer read; pins, favorites and field settings now live in <base>/services/<service>/ (see README)`; otherwise `""`. Errors are swallowed (returns `""`).
- Fixture `testdata/web/services.jsonl` (span lines in the existing `spans.jsonl` shape plus log lines; write it with a throwaway script, commit only the file): service `shop-api` (top-level `service.name`) with 12 unclassified internal spans named `sweep orders`, `token refresh`, `boot step`, `send email`, `cache warm`, `timer tick`, `reindex`, `export csv`, `import feed`, `prune sessions`, `rotate keys`, `compact logs` where the i-th name (1-based) appears 13−i times, plus 6 `db` spans (`kind:"client"`, `db.system.name:"exampledb"`) and 4 `http.server` spans; service `mail-worker` with `otel.resource.service.namespace:"mail"` (key `mail`) with 3 `send email` and 2 `messaging` spans; GCP LogEntry lines with `resource.labels.service_name:"shop-api"` (`labels.schema` `db` ×3, `api` ×2) and 3 pino lines (`logging.googleapis.com/labels.schema` `db`, service `default`). Start times spread evenly from `2026-11-17T16:00:00Z` to `16:59:00Z` with the `db` spans all after `16:30:00Z`.

- [ ] **Step 1: Write the failing tests** (`newFixture` passes `ConfigBase: t.TempDir()`; also `t.Setenv` temp `HOME`/`XDG_CONFIG_HOME`)
  - `TestKindsFollowRange`: over `services.jsonl`, `/api/kinds?source=otel&service=shop-api&from=all` has `sweep orders` count 12 and `db` 6; with `from=2026-11-17T16:00:00Z&to=2026-11-17T16:30:00Z` has no `db`; `services` = `[{shop-api …} {mail …}]` (by count).
  - `TestKindsUnknownServiceEmpty`: `service=nobody` → 200, `kinds: []`, services still listed.
  - `TestKindsBadRange400`: `from=yesterday` → 400.
  - `TestPinsRoundTrip`: PUT `{"pins":[{"source":"otel","kind":"db"}]}` for `shop-api` → GET returns it; file at `<base>/services/shop-api/pins.toml`; PUT with `service=*` → 400 `Choose a service to save`; bad source → 400.
  - `TestQueryServiceFilter`: POST `/api/query` `{"stream":"send email","source":"otel","service":"mail","range":{"from":"all"}}` matches 3; `service:"*"` matches 12 (the 9 `shop-api` + 3); missing service on a `default`-only kind (pino `db`, source log) matches the 3 pino lines.
  - `TestSchemaPutMergesBySource`: PUT `{"fields":[{"path":"duration_ms","type":"int","visualizer":"duration","unit":"ms"}]}` for otel/shop-api, then GET schema of two different kinds → both show it `saved`; `fields-otel.toml` holds exactly one `[[field]]`.
  - `TestSchemaPutConcurrentMerge`: 20 goroutines each PUT one distinct field path for the same service/source → file holds 20 fields.
  - `TestFavoritesPerService`: PUT favorite `x` for `shop-api`, GET for `mail` → `[]`; GET for `shop-api` → `[x]`; saved `queries.toml` has no `service` key.
  - `TestRepoConfigNotice` (`migrate_test.go`): temp repo with `.git` and `.config/loghorn/queries.toml` → first call returns the line containing the base's `services` path; second call `""`; repo with only `.config/loghorn/config.toml` → `""`; no `.config` → `""`. Base is a temp dir; `HOME` set to a temp dir.
  - Update existing tests: `StartDir` → `ConfigBase`; schema PUT tests to the fields body; drop `TestSchemaPutBadStream400` and `new`-flag assertions.
- [ ] **Step 2: Run** `go test ./internal/web/ . -run 'Kinds|Pins|QueryService|SchemaPut|FavoritesPerService|RepoConfigNotice'` — Expected: FAIL.
- [ ] **Step 3: Implement** the handlers and wiring. `writeMu` serialises pins, fields and favorites writes. `startWeb` and the demo resolve `svcconf.BaseFromEnv()` (error → print and exit, before any child) and pass `ConfigBase`; `startWeb` prints `repoConfigNotice(cwd, base)` to stderr when non-empty, before `web.Listen`. `replay.go` follows the same path through `startWeb`; check it for any other `StartDir` use. Update the demo's doc comment (config now under `$XDG_CONFIG_HOME/loghorn/services/`; run it with a scratch `XDG_CONFIG_HOME`).
- [ ] **Step 4: README.** Replace the `.config/loghorn/` schemas/favorites paragraph (README ~line 256) with: the per-service location and file names, the key rule (namespace, else name, else `default`; `OTEL_SERVICE_NAME` for `-- cmd` stdout), and a manual copy example: `mkdir -p ~/.config/loghorn/services/<key> && cp .config/loghorn/queries.toml ~/.config/loghorn/services/<key>/` with a note that old `schemas/*.toml` must be re-saved field by field (their `[[field]]` entries can be pasted into `fields-log.toml` / `fields-otel.toml`). `config.toml` stays where it is.
- [ ] **Step 5: Run** `go test ./...`, `go test -race ./...` — Expected: PASS; gofmt/vet clean. Smoke: `cd $(mktemp -d) && XDG_CONFIG_HOME=$(mktemp -d) go run /Users/jim/code/loghorn/.claude/worktrees/interval-streams/internal/web/demo -port 8791 /Users/jim/code/loghorn/.claude/worktrees/interval-streams/testdata/web/services.jsonl &` then `curl -s 'http://127.0.0.1:8791/api/kinds?source=otel&service=shop-api&from=all'` shows `sweep orders` with 12; kill the demo by PID.
- [ ] **Step 6: Commit** `feat(web): /api/kinds, per-service pins, favorites and field settings; repo config notice`

---

### Task 5: Page — mode switch, service selector and interval list

**Review:** standard. **Model:** opus (list layout and selection rules need judgement).

**Files:**
- Modify: `internal/web/static/app.js`, `internal/web/static/index.html`, `internal/web/static/app.css`

**Interfaces:**
- Consumes: Task 4's HTTP contract exactly.
- Produces (Task 6 relies on these names):
  - `state.service` (string; `*` = All) — in the query JSON, so it goes to `/api/query` and the URL `?q=`; `state.source` is the mode (`log` = Logs, `otel` = Spans); `state.stream` is the kind.
  - `let kinds = []` / `let services = []` — last `/api/kinds` answer; `async function loadKinds()` fetches `/api/kinds?source=&service=&from=&to=` from `state.range` and calls `renderKindList()`.
  - `function renderKindList()` — draws rows into `#kind-list`; each row is a `.kind-row` element with `data-kind`, a name span and a count span; Task 6 adds the pin toggle inside it via `kindRowExtras(row, name)` (a no-op hook defined here) and pinned rows via `pinnedKinds()` (returns `[]` here).
  - `function selectKind(name)` — the old `#stream` change handler body (reset columns/filters/explore, `loadSchema`, `rebuildExplore`, `runQuery`).
  - `function svcParam()` → `"&service=" + encodeURIComponent(state.service)`; every `/api/schema`, `/api/favorites` call carries it (`schemaURL(stream, source)` appends it).
  - localStorage key `loghorn.service`, read and written inside try/catch.

- [ ] **Step 1: Markup.** In the sidebar `.sb-form`, before `#sb-stream`, add `.sb-field` rows `#sb-mode` (label `Mode`, body: two `.tab`-style buttons `Logs` / `Spans` with `data-source="log|otel"`) and `#sb-service` (label `Service`, body `<select id="service">`). Replace `<select id="stream">` in `#sb-stream`'s `.sb-body` with `<div id="kind-list" role="listbox" aria-label="kinds">`, a `More (N) ▸` button `#kind-more`, and a hidden `<input id="kind-filter" type="search" placeholder="filter kinds">`. Look of rows and More is the builder's choice within existing sidebar styles; the list scrolls when long.
- [ ] **Step 2: List.** `renderKindList`: pinned rows first (from `pinnedKinds()`, count from `kinds` or `0`, greyed class `kind-zero` when 0), then unpinned kinds in server order, top 10 shown; the rest behind More, which expands in place and shows `#kind-filter` (case-insensitive substring over the hidden rest). The selected kind is always shown, greyed `0` when absent from `kinds`. Counts use `toLocaleString()`. Remove the `(log)`/`(otel)` clash labels and `renderStreams`.
- [ ] **Step 3: Service selector.** Options: `All` (`*`), then `services` by count (`name (count)`); a selected service not in `services` is still listed as `name (none in range)`. Initial choice in `loadSession`: URL `state.service` if present; else localStorage `loghorn.service`; else the service with the most records; with exactly one present, that one. The page never switches service on a range change. Changing it saves to localStorage, reloads kinds, keeps the kind if present else selects the first row with a count > 0.
- [ ] **Step 4: Mode switch.** Clicking the other mode sets `state.source`, reloads kinds, and selects the first row with count > 0 (else the first row, else none).
- [ ] **Step 5: Interval.** `rangeChanged`/`customChanged` and `refresh()` call `loadKinds()` alongside `runQuery()`. `loadSession` stops calling `/api/streams`; the empty-store message keys off `session.count === 0`. Old URLs: a `?q=` with `stream`/`source` and no `service` opens that kind under the initial service; a kind not in the list (e.g. the retired `span`) falls back to the top kind with no error banner.
- [ ] **Step 6: URL and columns.** `saveURL` writes `state.columns` as is (empty means "inferred over the interval"; stop writing `defaultColumns()`), plus `service`. `streamKey()` includes the service.
- [ ] **Step 7: Verify.** `go vet ./... && go test ./...` pass. Browser: free port 8791 (`lsof -nP -iTCP:8791 -sTCP:LISTEN`, kill PID), then from a scratch dir `XDG_CONFIG_HOME=$(mktemp -d) go run <worktree>/internal/web/demo -port 8791 <worktree>/testdata/web/services.jsonl`; open `http://127.0.0.1:8791/`, From = all. Check: Spans mode, service selector lists `shop-api` and `mail` and `All`; the list shows 10 rows ranked `sweep orders 12` … with `More (N)` revealing the rest and the filter narrowing it; a custom From of 16:30 local-equivalent changes counts (`sweep orders` drops; `db` keeps 6); switching to Logs lists `db`, `api` for `shop-api`; `All` shows both services' kinds; reload keeps mode/service/kind from the URL; a hand-edited URL with `"stream":"span","source":"otel"` opens the top kind with no banner; no console errors.
- [ ] **Step 8: Commit** `feat(web): logs/spans switch, service selector and interval kind list`

---

### Task 6: Page — pins, All read-only, per-field saves; final checks

**Review:** standard. **Model:** default.

**Files:**
- Modify: `internal/web/static/app.js`, `internal/web/static/app.css`, `internal/web/static/index.html` (only if needed)

**Interfaces:**
- Consumes: Task 5's `kindRowExtras`, `pinnedKinds`, `renderKindList`, `selectKind`, `svcParam`, `services`; Task 4's `/api/pins`, `/api/favorites`, `PUT /api/schema` body `{"fields":[…]}`.
- Produces: `let pins = []` (`[{source, kind, service}]`), `async function loadPins()`, `const readOnly = () => state.service === "*"`.

- [ ] **Step 1: Pins.** `loadPins` on service change and session load: one service → `GET /api/pins?service=`; All → one GET per key in `services` (present in the interval), each pin tagged with its service. `pinnedKinds()` returns this mode's pins (labelled `kind · service` under All). `kindRowExtras` adds a pin toggle button shown on row hover (always shown when pinned); its `mousedown` calls `preventDefault()`; click toggles and `PUT /api/pins?service=` the full list of this service's pins, then redraws. Under All the toggle is disabled with title `Choose a service to save`.
- [ ] **Step 2: Favorites.** `loadFavorites` passes `service`; under All it loads each present service's favorites and labels entries `name · service`. Save and delete are disabled under All (`#fav-save`, `#fav-save-menu`, delete ×) with title `Choose a service to save`. On `selectKind`, if this service's favorites hold one whose `query.stream`/`query.source` match and whose `query.columns` is non-empty, set `state.columns` to a copy of the first such (file order); otherwise `state.columns = []` (interval inference). Keep favorites of the current service cached so selection doesn't refetch.
- [ ] **Step 3: Field saves.** `saveFieldFormat` keeps its queue and re-applies its patch when it runs (CLAUDE.md) but sends `PUT /api/schema?source=&service=` with body `{"fields":[schemaFieldBody(f)]}` — only the edited field — then reloads the schema. The schema dialog's `saveSchema` sends only fields whose `schemaFieldBody` differs from the loaded schema's (no `columns`); nothing changed → status `no changes`. Remove the `new` badge. Under All: `saveFieldFormat` returns without saving (width drags stay temporary), the column menu's format controls and `#schema-save` are disabled with title `Choose a service to save`.
- [ ] **Step 4: Verify.** `go vet ./... && go test ./...` pass. Browser (same demo command as Task 5, fresh `XDG_CONFIG_HOME`): pin `db` under `shop-api` → it moves to the top and `<XDG>/loghorn/services/shop-api/pins.toml` holds it; set From/To to 16:00–16:20 → `db` stays pinned, greyed `0`; pin toggling with the filter box focused still works (mousedown); switch to All → pins labelled `db · shop-api`, pin toggle and favorite save disabled with the tooltip; under `shop-api` change `duration_ms`'s visualizer from the column menu → `fields-otel.toml` holds only that field, and another kind shows it; save a favorite with columns `name, duration_ms` on `sweep orders`, re-select the kind → those columns; select a kind with no favorite → columns follow the interval (change the range so a field disappears from all rows and confirm its column drops). No console errors.
- [ ] **Step 5: Final repo-wide checks.** `gofmt -l $(git ls-files '*.go')` (empty), `go vet ./...`, `go test ./...`, `go test -race ./...` — all clean. `git grep -n 'StartDir\|ValidStream\|LoadSaved\|SaveSchema\|/api/streams' -- internal/web/static` prints nothing.
- [ ] **Step 6: Commit** `feat(web): pins, read-only All, per-field schema saves`
