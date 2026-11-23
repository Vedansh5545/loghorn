# DuckDB storage and SQL queries — implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Store logs and spans in per-day DuckDB files (a schema per service, a table per stream, typed columns) and answer every `/api/*` request of `-web` with SQL, with the same JSON as today.

**Architecture:** A new package `internal/duckstore` owns DuckDB: day catalogs, the background writer, column evolution, the `_loghorn` metadata tables, the generated views and a "relation" builder that projects requested fields converted to their effective type. `internal/store` stays the façade every caller already uses (`Add`, `Info`, `Streams`, `Kinds`, `Preload`) and gains the read methods the web needs. The query semantics stay in `internal/query`: a new `RunSQL` turns a validated `Query` into SQL over a relation and reuses today's helpers (`cell`, `xAxis`, `bands`, ranking) so answers match `query.Run`, which stays as the test oracle.

**Tech Stack:** Go 1.26.1, cgo, `github.com/duckdb/duckdb-go/v2 v2.20001.0` (DuckDB 2.0.1).

**Spec:** `docs/superpowers/specs/2026-11-23-duckdb-storage-design.md` (approved, D-011). Decisions D-014..D-020 below refine it and are recorded in `docs/DECISIONS.md`.

## Global Constraints

- Files: `.loghorn/logs-YYYY-MM-DD.duckdb`, `.loghorn/traces-YYYY-MM-DD.duckdb`, mode `0o600`, written only by the `loghorn.lock` holder; `keepDays = 3`.
- `_id` = local day number since 1970-01-01 × 2³² + per-day sequence (logs and spans share it); replay modes use day 0, so ids are 1..n as today. JSON field stays `seq`.
- Shared columns, in order: `_id BIGINT, _time TIMESTAMP_NS, _time_inherited BOOLEAN, _source VARCHAR, _service VARCHAR, _stream VARCHAR, _trace_id VARCHAR, _span_id VARCHAR, _parent_span_id VARCHAR, _name VARCHAR`, then field columns, then `_bad VARCHAR[], _raw VARCHAR`.
- Type map: int→BIGINT, float→DOUBLE, bool→BOOLEAN, string→VARCHAR, timestamp→TIMESTAMP_NS (naive UTC), duration→DOUBLE seconds (D-014), json→VARCHAR (D-017).
- Column type = `schema.Infer`'s rule over the table's tallies; `timestamp`/`receiveTimestamp` forced timestamp. Stored value = `schema.Convert(v, column field)`; rejected → NULL plus path in `_bad`.
- Names differing only in case get `~2`, `~3`… suffixes; metadata lives in schema `_loghorn`.
- Percentiles nearest-rank: 1-based index `clamp(ceil(p/100*n), 1, n)` over sorted values; `error_rate` = upper(trim(severity as string)) ∈ {ERROR, CRITICAL, ALERT, EMERGENCY, FATAL}.
- Writes never block the child or the OTLP receiver; the queue holds at most 200 000 records; overflow drops for DuckDB only, marks the day dirty, warns once.
- Query times use `time.Local`.

## Review Focus

1. A field whose type differs between services or days (int in one, string in another) queried with service `*` — must convert, not error.
2. Field names that differ only in case (`userId`/`userID`) and dotted names — both must stay separate and queryable.
3. A request arriving right after records were added (tests, live page refresh) — must see them (D-015).
4. Records with no time (replay of untimed lines) — must sort first ascending/last descending and match a zero `from` only.
5. Graph buckets of 6h/1d across a DST change — starts come from Go's `xAxis`, SQL only assigns rows to them.

---

### Task 1: Driver and schema tallies

**Files:** Modify `go.mod`, `internal/schema/infer.go`; Test `internal/schema/tally_test.go`.

**Interfaces — Produces:**
- `type schema.KindSet uint16`; `func schema.Classify(v any) KindSet` (one bit); `type schema.Tally struct{ Count int; Kinds KindSet }`
- `func schema.TallyType(path string, k KindSet) Type` (Infer's `typ()` plus forced timestamp)
- `func schema.FromTallies(source, stream string, t map[string]Tally) Schema` (Fields sorted, Visualizer, Columns = DefaultColumns)
- `Infer` reimplemented on these.

- [ ] Test `TestFromTalliesMatchesInfer`: for every fixture in `testdata/web`, `FromTallies` over tallies built with `Classify` equals `Infer`.
- [ ] Implement; `go test ./internal/schema` passes; add `require github.com/duckdb/duckdb-go/v2 v2.20001.0`.
- [ ] Commit.

### Task 2: duckstore engine (memory catalogs)

**Files:** Create `internal/duckstore/{engine.go,names.go,sqltype.go,writer.go,meta.go,views.go,relation.go}`; Test `internal/duckstore/*_test.go`.

**Interfaces — Produces:**
- `func OpenMemory(in config.Input) (*Engine, error)`; `(*Engine).Add(r record.Record)` (r.Seq is the id); `Sync(timeout)`, `Close()`, `Version() uint64`.
- `Relation(source, service, stream string, cols []schema.Field) (sql string, args []any)` — rows `_id,_time,_time_inherited,_service,_source,_stream,_raw, v0,x0,…` where `vi` is field i converted to `cols[i].Type` and `xi` is "present but bad".
- `Records(ctx, where string, args ...any) ([]record.Record, error)` — rebuilt from `_raw` over `all_records`, ordered by `_id`.
- `Tallies(source, service, kind string) (map[string]schema.Tally, error)`; `QueryContext`; `DB()`.

- [ ] Tests: new stream/field create table/column; int→float→string widening rewrites from `_raw` (`"1.50"` kept); bad timestamp → NULL + `_bad`; `userId`/`userID` separate; dotted names; views union two catalogs with widest types; `Relation` conversion table checked against `schema.Convert` for int/float/bool/string/duration/timestamp/json pairs.
- [ ] Implement; tests pass; commit.

### Task 3: SQL query execution with parity

**Files:** Create `internal/query/sqlrun.go`; Modify `aggregate.go`, `graph.go`, `heatmap.go`, `run.go` (extract `rankBy`, `axis`, `samplesRows`); Test `internal/query/parity_test.go` (package `query_test`).

**Interfaces — Consumes:** Task 2's `Relation`, `QueryContext`, `Records`. **Produces:** `type query.Backend interface{ Relation(...); QueryContext(...); Records(...) }`; `func RunSQL(ctx, b Backend, s schema.Schema, q Query, now, runStart time.Time) (Result, error)`.

- [ ] Parity test over `testdata/web/*` plus a generated set (services, streams, mixed types, inherited/zero times, durations, spans): every view × filter op × type, search, `*` stream/service, Offset, Order/Top/other, every bucket incl. auto, heatmap with/without cell, empty results; sum/avg rel. tol. 1e-9, everything else `reflect.DeepEqual` on JSON.
- [ ] Implement; parity passes; commit.

### Task 4: store façade and web wiring

**Files:** Rewrite `internal/store/store.go`; Modify `internal/web/server.go`, tests using `Snapshot`, `internal/trace` tests, `internal/web/demo/main.go`.

**Interfaces — Produces:** `store.Store` methods `Add, SetFallbackService, Flush, Sync, Close, Info, Streams, Kinds, Schema(svc, src, stream), Query(ctx, sch, q, now), Record(seq), TraceRecords(traceID, spanID), AggregateRecords(name)`; `Snapshot` removed (`Records()` kept for tests).

- [ ] Existing `internal/web`, `internal/trace`, `internal/recorder`, root tests pass unchanged in behaviour.
- [ ] Commit.

### Task 5: day files, modes, rebuild, rollover, failure

**Files:** Create `internal/duckstore/days.go`; Modify `wrapper.go`, `replay.go`, `main.go`; Test `internal/duckstore/days_test.go`, root e2e.

**Interfaces — Produces:** `duckstore.OpenDir(DirOptions{Dir, Write bool, Now, Loc, Input, Warn})`; `store.Open(store.Options{…})`.

- [ ] Tests: writer creates pair 0600 and `_day` clean on close; reopen resumes ids; crash (no clean close) → rebuilt from text; text size changed → rebuilt; non-holder reads earlier days read-only and today from text; `-web` alone falls back to text when a day is locked; midnight rollover; expiry; write failure → warning once, dirty, records continue in memory.
- [ ] Wire: lock holder runs the store with or without `-web`; `closeLog` before store close; replay calls `Flush` at end of input.
- [ ] `go test ./...` passes; commit.

### Task 6: speed, docs, review

- [ ] Benchmark ~770k records (spike-2 shape): Samples, Table, Graph, Heatmap, Kinds; record results in `PROJECT_STATUS.md`.
- [ ] Update `DECISIONS.md`, `PROJECT_STATUS.md`, `AGENT_HANDOFF.md`; whole-branch review (superpowers:requesting-code-review); fix findings; commit; export patches.
