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

- Brainstorming is in progress: approach and layout are chosen (D-001 to
  D-008), one question is open (O-1, scope split).
- No design doc, plan or code yet.

## What to read in the code (as of `331ddc3`)

| What | Where |
|---|---|
| The shared record every view uses | `internal/record/record.go` (`Record`) |
| How spans become records and get a stream | `internal/record/span.go` (`SpanStream`, `NormalizeSpan`) |
| The in-memory store DuckDB replaces | `internal/store/store.go` (`Add`, `Snapshot`, `Kinds`) |
| Query engine (Samples, Table, Graph, heatmap) | `internal/query/` (`Run`, aggregate, graph, heatmap) |
| Trace waterfall and aggregate | `internal/trace/` (`Build`, `Aggregate`) |
| Text files loghorn writes | `internal/logfile/` (`loghorn.log`, `loghorn.traces.jsonl`) |
| Author's architecture decisions and spike results | `docs/superpowers/specs/2026-11-19-architecture-working-notes.md` |
| Author's SQL interface design (Query ↔ SQL) | `docs/superpowers/specs/2026-11-18-sql-query-design.md` |

## Environment notes

- The Claude cloud session that started this work has Go 1.24.7, no
  DuckDB, no package installs and no push access. Build and test on a
  machine with Go 1.26.1 and cgo.
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
