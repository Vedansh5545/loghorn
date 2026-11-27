# DuckDB storage v2 Implementation Plan

> **Superseded in part (2026-11-27 note).** This plan was built as written on 2026-11-24, then changed by D-030 (2026-11-25) and the review fixes of 2026-11-27. Where it differs from the code, the code and `docs/DECISIONS.md` are right:
>
> - **Two files and two tables** (`logs.duckdb` with `logs`, `traces.duckdb` with `spans`) became **one `records` table in one file, `loghorn.duckdb`**; `_source` tells logs from spans (D-030).
> - **Parquet per signal** (`archive/<logs|traces>-YYYY-MM-DD.parquet`) became **one file per closed day for all records**, `duckdb-archive/records-YYYY-MM-DD.parquet` (D-028, D-030).
> - **`-db-dir` and `store.Options.DBDir`** are gone. The file lives in serve's data folder.
> - **The `loghorn.lock` holder as owner, read-only and historical readers** became **`loghorn serve` as the only process that opens the file** (D-030). `store.Open` opens only the writer; `FileOptions.Write`, `DeferImport` and the readers' copy were removed (commit `15b6542`).
> - **`wrapper.go`, `replay.go` and `-web`** were removed upstream. serve's `openStore` (`internal/serve/serve.go`) opens the store, before it binds its socket and ports (commit `ef6583b`).
> - **The `imported` row in a `_meta` table** (`Meta`/`SetMeta`) became **the file `loghorn.duckdb.imported`** beside the database: removed when serve opens the files, written again only by a clean Close of complete files (D-027, commit `cc0df89`). `_meta` is no longer used.
>
> Stale passages below are marked *(superseded: …)*.

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the per-stream, per-day DuckDB layout with the v2 design: one fixed table per signal in `logs.duckdb` and `traces.duckdb` *(superseded: one `records` table in `loghorn.duckdb`, D-030)*, JSON attributes, raw records kept, and closed days archived to Parquet.

**Architecture:** Built on the `duckdb-storage` branch, keeping its seams: `query.RunSQL` reads through `query.Backend` (`Relation`, `QueryContext`, `Rebuild`), and the web view reads through the `store.Store` façade. Only `internal/duckstore` is rewritten underneath, together with the store's open path. Custom fields are extracted from JSON and converted at query time with the existing `conv` SQL, so new attributes and type settings need no table changes.

**Tech Stack:** Go 1.27, duckdb-go v2 (cgo), DuckDB JSON and Parquet (both built in).

**Spec:** `docs/superpowers/specs/2026-11-23-duckdb-storage-design.md` (v2, revised 2026-11-24).

## Global Constraints

- Build and test on macOS with `SDKROOT=/Library/Developer/CommandLineTools/SDKs/MacOSX26.sdk`.
- Keep the web UI and REST contracts; `query.Run` stays the reference, and the parity test (`PARITY_FULL=1`) must pass or each difference must be documented.
- Fixed fields in both tables *(superseded: in the one `records` table)*: `_id` BIGINT, `_time` TIMESTAMP_NS (UTC), `_time_inherited` BOOLEAN, `_source`, `_service`, `_stream`, `trace_id`, `span_id`, `parent_span_id`, `severity`, `message` VARCHAR, `duration_ms` DOUBLE, `attributes` JSON, `_raw` (lossless). `_id` keeps the existing encoding `day<<32 | seq` (day = `store.DayNumber`), so `_id >> 32` is a record's archive period and no extra column is needed.
- No tables per stream, no columns added at run time, no type-widening rewrites.
- Do not delete text logs or change the text writer, `internal/ingest`, `internal/otlp` or `internal/recorder`.
- Nothing may `os.Exit` after the child process starts. Tests never write to the repo's `.loghorn/`.
- Storage failures are visible (a warning) and the session keeps running in memory.
- Commits end with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.

## Review Focus

- A raw record that is not valid UTF-8 (binary bytes in a log line) must round-trip exactly; `_raw` is BLOB, tested in Task 1.
- A field that is a number in some records and a string in others must convert per record as `schema.Convert` does; tested in Task 2.
- A crash during the first import from text must not leave half-imported files that are then trusted; tested in Task 4.
- A failed or partial Parquet export must leave the hot records in place and publish no archive file; tested in Task 5.
- A record with no time at all must still be stored, queried and archived with its insertion day; tested in Tasks 1 and 5.

---

### Task 1: Fixed tables, writer and record reads (memory engine)

**Files:**
- Create: `internal/duckstore/tables.go` (DDL, `all_records` view, attribute JSON)
- Rewrite: `internal/duckstore/engine.go`, `internal/duckstore/writer.go`, `internal/duckstore/read.go` (from `views.go`)
- Delete: `internal/duckstore/catalog.go`, `views.go`, `days.go`, `names.go`, `days_test.go`
- Test: `internal/duckstore/engine_test.go`

**Interfaces:**
- Produces: `OpenMemory(in config.Input, warn func(string)) (*Engine, error)`; `(*Engine) Add(r record.Record)`, `AddWait`, `Sync(time.Duration) bool`, `Close() error`, `ReadLock() func()`, `DB() *sql.DB`, `QueryContext`, `Version() uint64`, `SetTime`, `SetMaxQueue`; `Records(ctx, where string, args ...any) ([]record.Record, error)` with `where` over `all_records` columns; `Rebuild(id int64, t time.Time, inherited bool, service, source, stream, raw string) record.Record`.
- Tables: `logs` and `spans`, each with the fixed fields. The view `memory.main.all_records` is `logs UNION ALL spans` (plus archives, Task 5). *(superseded: one table, `records`, D-030.)*
- `attributes` is `record.Fields` encoded as one flat JSON object (dotted path keys), using `json.Encoder` with `SetEscapeHTML(false)`; `json.Number` stays a JSON number.
- Common fields: `trace_id`/`span_id` from `record.TraceRef`; `parent_span_id` (normalized) and `message` = span `name` for spans; for logs `severity` = `Fields["severity"]`, `message` = `Fields["message"]`; `duration_ms` = span `duration_ms` field when numeric.

- [ ] **Step 1: Write the failing tests**
  - `TestRecordsRoundTrip`: add a log and a span; `Records(ctx, "true")` returns both, in `_id` order, with equal `Seq`, `Time`, `Service`, `Stream`, `Source`, `Fields` and `Raw`.
  - `TestRawIsLossless`: raw `"{\"labels\":{\"schema\":\"s\"},\"m\":\"\xff\x00\"}"` (invalid UTF-8) is stored and read back byte for byte from `_raw`.
  - `TestUntimedRecordIsStored`: a record with zero `Time` is stored with NULL `_time`, keeps its day in `_id`, and is returned by `Records`.
  - `TestCommonColumns`: a span's `trace_id`, `span_id`, `parent_span_id`, `message`, `duration_ms` and a log's `severity`, `message` are set; others NULL.
- [ ] **Step 2: Run them to see them fail**

  Run: `go test ./internal/duckstore -run 'RoundTrip|Lossless|Untimed|CommonColumns'` — Expected: FAIL (columns missing).
- [ ] **Step 3: Implement** the fixed DDL, the writer appending each batch to `logs` or `spans` in one transaction (Appender), and `Records`/`Rebuild` reading `all_records`. Drop catalogs, views per stream, retyping and `schemaMu`.
- [ ] **Step 4: Run the package tests** — Expected: PASS (tests about day files and retyping are removed with their code).
- [ ] **Step 5: Commit** `feat(duckstore): one fixed table per signal with JSON attributes and raw bytes`

### Task 2: Relation and tallies over JSON

**Files:**
- Modify: `internal/duckstore/read.go`, `internal/duckstore/writer.go`, `internal/duckstore/sqltype.go`
- Test: `internal/duckstore/engine_test.go`, `internal/query/parity_test.go` (run only)

**Interfaces:**
- Produces: `Relation(source, service, stream string, cols []schema.Field) (string, []any)` with the same columns as before (`_id, _time, _time_inherited, _service, _source, _stream, _raw`, then `vi`, `xi` per field); `Tallies(source, service, kind string) (map[string]schema.Tally, error)`.
- Value of field `p`: `attributes -> '<JSON pointer of p>'`, converted by `json_type`: VARCHAR via `conv(text, TString, f)`, numbers via `conv(.., TFloat or TInt, f)`, BOOLEAN via `conv(.., TBool, f)`, ARRAY via `conv(.., TJSON, f)`. `xi` is true when the value is present and not empty but converts to NULL.
- Tallies: table `_fields(day INTEGER, service VARCHAR, source VARCHAR, kind VARCHAR, path VARCHAR, n BIGINT, kinds INTEGER, PRIMARY KEY(day, service, source, kind, path))`, updated per batch with `ON CONFLICT DO UPDATE SET n = n + excluded.n, kinds = kinds | excluded.kinds`; `kind` is the old `tableKey` (stream, or `span.type`).

- [ ] **Step 1: Write the failing tests**
  - `TestRelationConvertsLikeConvert` (existing cases, rewritten for JSON): for each value and type, `vi` equals `schema.Convert` and `xi` equals `Bad`.
  - `TestMixedKindsConvertPerRecord`: path `n` is `json.Number("5")` in one record and `"5"` in another; as `int` both are 5; as `bool` the number is bad and `"true"` is true.
  - `TestTalliesCountPerKind`: tallies of two batches add up per path and kind set.
- [ ] **Step 2: Run them to see them fail.**
- [ ] **Step 3: Implement** `Relation` over `logs`/`spans` *(superseded: over `records`)* filtered by `_source`, `_service` and `_stream`, and the tally upsert.
- [ ] **Step 4: Run** `go test ./internal/duckstore ./internal/store ./internal/query` and `PARITY_FULL=1 go test ./internal/query -run Parity` — Expected: PASS, or each remaining difference written down in `docs/DECISIONS.md` (as a proposed entry) with its test case.
- [ ] **Step 5: Commit** `feat(duckstore): query custom fields from JSON attributes`

### Task 3: Two database files, persistence and failure

*(superseded: one file, `loghorn.duckdb`, opened only for writing by serve; no read-only open, no `_meta` table. See the note at the top.)*

**Files:**
- Create: `internal/duckstore/files.go`
- Test: `internal/duckstore/files_test.go`

**Interfaces:**
- Produces: `type FileOptions struct { Dir string; Write bool; KeepDays int; Now func() time.Time }`; `OpenFiles(in config.Input, warn func(string), o FileOptions) (*Engine, error)` attaching `Dir/logs.duckdb` and `Dir/traces.duckdb` (created when `Write`, chmod 0600 with their WAL; read-only otherwise; an error if a read-only file is missing or locked); `(*Engine) MaxSeq(day int) int` (highest stored seq of a day, hot or archived); `(*Engine) Meta(key string) (string, bool)` and `SetMeta(key, value string) error` (table `_meta` in `logs.duckdb`). *(superseded: `FileOptions{Dir, KeepDays}`; one file, `loghorn.duckdb`, always written; `Meta`/`SetMeta` and `_meta` replaced by the mark file `loghorn.duckdb.imported`.)*
- On the first failed file write: warn once, and send the rest of the session to the memory tables (the `all_records` view covers both).

- [ ] **Step 1: Write the failing tests**
  - `TestFilesPersistAcrossReopen`: write two records, close, reopen; `Records` returns them with the same ids, and `MaxSeq(day)` is 2.
  - `TestReadOnlyOpenNeedsFiles`: `OpenFiles` with `Write: false` on an empty dir fails; after a writer closes, it opens and reads. *(superseded: no read-only open.)*
  - `TestWriteFailureFallsBackToMemory`: after the files are made unwritable mid-session (detach under the writer), later records are still returned and exactly one warning was given.
  - `TestFilesAreOwnerOnly`: both files have mode 0600.
- [ ] **Step 2: Run them to see them fail.**
- [ ] **Step 3: Implement** `files.go`; tallies and meta tables live in `logs.duckdb`. *(superseded: tallies live in `loghorn.duckdb`; the mark is the file `loghorn.duckdb.imported`.)*
- [ ] **Step 4: Run** `go test ./internal/duckstore` — Expected: PASS.
- [ ] **Step 5: Commit** `feat(duckstore): logs.duckdb and traces.duckdb that persist across runs` *(as built then; see the note at the top)*

### Task 4: Store open, first import and the storage directory

*(superseded: `wrapper.go`, `replay.go`, `-web` and `-db-dir` are gone; serve opens the store in its data folder, as its only writer. There is no live reader from text and no historical read of the files. See the note at the top.)*

**Files:**
- Rewrite: `internal/store/days.go` → `internal/store/open.go`
- Modify: `wrapper.go`, `replay.go`, `main.go` (flag `-db-dir`) *(superseded: removed)*
- Test: `internal/store/open_test.go` (replaces `days_test.go`), `main_test.go`

**Interfaces:**
- Consumes: `OpenFiles`, `MaxSeq`, `Meta`/`SetMeta`.
- Produces: `store.Options{Mode, RunStart, Input, Dir, DBDir, Write, Now, Warn}` (`DeferBuild` removed; `DBDir == ""` means `Dir`); `store.Open(Options) (*Store, error)`. *(superseded: `store.Options{Mode, RunStart, Input, Dir, Now, Warn}`; `Open` always opens the writer.)*
  - `Write`: open the files for writing. If meta `imported` is not `"1"`, the files are new or a first import was interrupted: delete them, recreate, import the retained days' text (`KeepDays` = 3 and today) with their own days, then `SetMeta("imported", "1")`. Ids continue per day from `MaxSeq(day)`. *(superseded: the mark is the file `loghorn.duckdb.imported`, removed before the files open and written by a clean Close of complete files.)*
  - Not `Write`, live: memory engine with the retained days read from text (as on `main`). *(superseded: removed; only serve's fallback, when the files can't open, reads text into memory.)*
  - Historical: open the files read-only; if that fails (missing or locked by a live writer), read the text into memory and warn. *(superseded: removed.)*
- Flag *(superseded: removed)*: `-db-dir DIR`, "directory for loghorn's DuckDB and Parquet files (default: the .loghorn directory)".

- [ ] **Step 1: Write the failing tests**
  - `TestFirstOpenImportsTextOnce`: text for yesterday and today; first `Open` imports all records; a second `Open` imports nothing more (count unchanged) and keeps ids.
  - `TestInterruptedImportIsRedone`: files exist without `imported`; `Open` rebuilds them and the count matches the text.
  - `TestRecordsPersistWithoutText`: records added in one session are present after reopening even when the text files are deleted.
  - `TestHistoricalFallsBackToText`: with the files held open by a writer, a historical `Open` reads the text and warns. *(superseded: removed.)*
  - `TestDBDirSeparateFromTextDir`: with `DBDir` set, the `.duckdb` files are created there and none in `Dir`. *(superseded: removed with `-db-dir`.)*
- [ ] **Step 2: Run them to see them fail.**
- [ ] **Step 3: Implement** `open.go` and the wrapper, replay and flag changes.
- [ ] **Step 4: Run** `go test ./...` — Expected: PASS.
- [ ] **Step 5: Commit** `feat(store): open the two DuckDB files, import text once, -db-dir` *(as built then; see the note at the top)*

### Task 5: Parquet archive of closed days

**Files:**
- Create: `internal/duckstore/archive.go`
- Modify: `internal/duckstore/tables.go` (view over archives), `internal/duckstore/writer.go` (archive at midnight), `internal/store/open.go` (archive at open)
- Test: `internal/duckstore/archive_test.go`, `internal/store/open_test.go`

**Interfaces:**
- Produces: `(*Engine) Archive(today int) error` (writer only): for each signal and each day (`_id >> 32`) before `today` present in the hot table, export to `Dir/archive/<logs|traces>-YYYY-MM-DD.parquet.tmp` *(superseded: one file per day for all records, `Dir/duckdb-archive/records-YYYY-MM-DD.parquet.tmp`)* (ZSTD), validate, rename to `.parquet`, then delete that day's hot rows; finally delete archives and `_fields` rows with day `< today - KeepDays`. The view adds `read_parquet([...], union_by_name = true)` per signal *(superseded: one list for all records)* when archives exist.
  - Validation: row count, `min/max/sum(_id)`, and `md5(_raw)` aggregate equal between the hot day and the file.
  - Failure (export, validation or rename): remove the `.tmp`, keep the hot rows, warn once; the next `Archive` tries again.
  - An archive file for a day that still has hot rows is skipped by the view until the hot rows are gone, so nothing is read twice.
- Midnight: the writer calls `Archive` when a batch carries a later day than the last one it wrote.

- [ ] **Step 1: Write the failing tests**
  - `TestArchiveMovesClosedDays`: records on days d−1 and d; `Archive(d)` writes `logs-<d−1>.parquet` and `traces-<d−1>.parquet` *(superseded: `records-<d−1>.parquet`)*; hot tables keep only day d; `Records` returns every record once with its id and raw.
  - `TestArchivedRawReprocesses`: a record read back from Parquet rebuilds (`Rebuild`) to the same `Fields` as before.
  - `TestFailedArchiveKeepsHotRows`: with validation forced to fail (a test hook), no `.parquet` exists, hot rows remain, one warning.
  - `TestArchiveExpiresOldDays`: archives and tallies older than `today − KeepDays` are removed.
  - `TestRelationReadsArchives`: a Table query over d−1 and d counts both days.
  - `TestUntimedRecordArchived`: a record with no time on day d−1 is archived and still returned.
- [ ] **Step 2: Run them to see them fail.**
- [ ] **Step 3: Implement** `archive.go` and the view and writer changes.
- [ ] **Step 4: Run** `go test ./...` and `go test -race ./internal/duckstore ./internal/store` — Expected: PASS.
- [ ] **Step 5: Commit** `feat(duckstore): archive closed days to Parquet`

### Task 6: Measure, verify and document

**Files:**
- Modify: `internal/store/bench_test.go` (startup, memory and disk measurements), `docs/PROJECT_STATUS.md`, `docs/DECISIONS.md`, `docs/AGENT_HANDOFF.md`

- [ ] **Step 1:** Extend the benchmark to the ~770k-record dataset: end-to-end latency of Samples, Table, Graph, Heatmap and Search (warm and cold), startup time with persisted data, peak RSS, and disk size of the `.duckdb` and `.parquet` files. Record dataset, hardware and conditions.
- [ ] **Step 2:** Investigate any representative query above 500 ms.
- [ ] **Step 3:** Run `go vet ./...`, `go test ./...`, `go test -race` on duckstore, store, web, query and main, and `PARITY_FULL=1 go test ./internal/query -run Parity` — Expected: all PASS.
- [ ] **Step 4:** Add v2 decisions to `docs/DECISIONS.md` marked "Proposed (v2), pending review", each naming the earlier decision it would replace; update `PROJECT_STATUS.md` (measurements, open questions from the spec, known gaps such as an unflushed batch lost from DuckDB on a crash) and `AGENT_HANDOFF.md`.
- [ ] **Step 5: Commit** `docs: v2 storage status, measurements and proposed decisions`
