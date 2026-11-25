# Agent handoff: DuckDB storage for loghorn

Read this first, then `docs/PROJECT_STATUS.md` and `docs/DECISIONS.md`.

## The goal

Make loghorn's web view (`loghorn -web`) faster to filter and query, in
the spirit of Meta's Scuba, by storing OpenTelemetry logs and traces in
DuckDB instead of an in-memory list. Metrics are out of scope. Raw lines
must stay available so structure can be pulled out later.

## How to work

- Superpowers workflow (plugin `obra/superpowers` 7.0.2): brainstorming →
  design doc → user approval → writing-plans → execution with tests first →
  review → verification before any claim of completion.
- Keep `docs/DECISIONS.md`, `docs/PROJECT_STATUS.md` and this file current
  at each decision or milestone. Never rewrite an accepted decision; add a
  new one that supersedes it.
- Design docs go in `docs/superpowers/specs/`, plans in
  `docs/superpowers/plans/`, matching the author's existing files.

## Where things stand

- Branch `duckdb-v2` implements the **v2 design**
  (`docs/superpowers/specs/2026-11-23-duckdb-storage-design.md`, revised
  2026-11-24) with plan `docs/superpowers/plans/2026-11-24-duckdb-storage-v2.md`.
  It is built on `duckdb-storage` (PR jimbeveridge/loghorn#3, the first
  design) and keeps its store façade, `query.RunSQL` and parity test; the
  engine underneath is new. Its choices are D-025 to D-029, all
  *proposed* until the author reviews the v2 spec (O-3).
- `query.Run` and `schema.Infer` stay as the test oracle (D-016). The
  parity test (`internal/query/parity_test.go`) compares 1 in 10 of 8055
  queries by default; `PARITY_FULL=1` runs all (about 2 minutes).
- Speed at about 770k records, restart, archive, disk and memory:
  `LOGHORN_SPEED=1 go test -run SpeedAtScale -v -timeout 30m ./internal/store`.

## The code now

| What | Where |
|---|---|
| Fixed tables, views, attributes JSON | `internal/duckstore/tables.go` |
| Engine, queue, writer goroutine | `internal/duckstore/engine.go`, `writer.go` |
| Reads: `Relation` (JSON → typed per record), records, tallies | `internal/duckstore/read.go`, `sqltype.go` |
| `logs.duckdb` / `traces.duckdb`, meta, readers' copy | `internal/duckstore/files.go` |
| Parquet archive of closed days | `internal/duckstore/archive.go` |
| Store façade and modes; first import from text | `internal/store/store.go`, `internal/store/open.go` |
| Query → SQL with today's semantics | `internal/query/sqlrun.go` (`RunSQL`) |
| Where a live run opens and closes the store; `-db-dir` | `wrapper.go`, `replay.go`, `main.go` |

## Environment notes

- On a normal machine: Go 1.27 with cgo, `go test ./...`. On macOS with
  an SDK newer than the Command Line Tools' linker, set
  `SDKROOT=/Library/Developer/CommandLineTools/SDKs/MacOSX26.sdk`.
- The cloud session that built this branch had no module proxy or push
  access; it used a local Go 1.26.1 build, module clones and DuckDB's
  shared library with `-tags=duckdb_use_lib` (D-013). Patches were handed
  over instead of pushed.
- Repo rules: nothing may `os.Exit` after the child is launched
  (`CLAUDE.md`); never pipe data into a loghorn binary, and tests never
  write into the repo's own `.loghorn/` (Global Constraints in
  `docs/superpowers/plans/2026-11-19-otel-spans.md`).
- Someone else is working on ingestion (`internal/ingest`, `internal/otlp`,
  `internal/recorder`, the `internal/logfile` writer). Avoid changing it.

## Related material outside the repo

From the 2026-11-18 to 2026-11-23 chat, not committed: a two-page
assessment doc, an OpenTelemetry → Parquet practice kit, and a superseded
schema demo (see D-009).
