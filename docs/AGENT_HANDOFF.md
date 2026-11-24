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

- Design approved (`docs/superpowers/specs/2026-11-23-duckdb-storage-design.md`,
  D-011), refined by D-014 to D-023.
- Plan `docs/superpowers/plans/2026-11-23-duckdb-storage.md`: tasks 1–6
  done; the whole-branch review's Important findings are fixed. What is
  left is in `docs/PROJECT_STATUS.md` (`go.sum`, deferred minors, PR).
  Execution ledger (git-ignored, shipped beside the bundle as
  `progress.md`): `.superpowers/sdd/2026-11-23-duckdb-storage/progress.md`,
  with every ruling made while building.
- `query.Run` and `schema.Infer` stay as the test oracle (D-016). The
  parity test (`internal/query/parity_test.go`) compares 1 in 10 of 8055
  queries by default; `PARITY_FULL=1` runs all (about 4 minutes).
- Speed at about 770k records: `LOGHORN_SPEED=1 go test -run SpeedAtScale -v ./internal/store`.

## The code now

| What | Where |
|---|---|
| DuckDB engine: catalogs, writer, column evolution, views, relation | `internal/duckstore/` (`engine.go`, `writer.go`, `views.go`, `sqltype.go`) |
| Day files: attach, stamp, build, rollover, expiry, failure | `internal/duckstore/days.go` |
| Store façade and modes (writer, reader, stored days, replay) | `internal/store/store.go`, `internal/store/days.go` |
| Query → SQL with today's semantics | `internal/query/sqlrun.go` (`RunSQL`) |
| Where a live run opens and closes the store | `wrapper.go` (`openLiveStore`, `closeStore`), `replay.go` |

## Environment notes

- On a normal machine: Go 1.26.1 with cgo, then `go mod tidy` once (the
  branch's `go.sum` lacks the DuckDB driver's entries, D-013) and
  `go test ./...`.
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
