# Project status: DuckDB storage for loghorn

Last updated: 2026-11-24 · Branch `duckdb-storage`, based on `331ddc3`

**Status values:** Planned · In progress · Completed (verified) · Failed ·
Blocked. A task is Completed only when the check named next to it has
run and passed.

## Completed

| Work | Date | Verified by | Ref |
|---|---|---|---|
| Repo review and improvement plan | 2026-11-18 | Code reading plus three reproductions; repo not built | chat only |
| Storage approach chosen (layout B) and first decisions recorded | 2026-11-23 | User answers in chat | `docs/DECISIONS.md` D-001…D-008 |
| Scope chosen: one design for storage and SQL queries | 2026-11-23 | User answer in chat | D-010 |
| Design approved | 2026-11-23 | User approval in chat | `docs/superpowers/specs/2026-11-23-duckdb-storage-design.md`, D-011 |
| Local build environment (Go 1.26.1 + DuckDB 2.0.1) | 2026-11-23 | `go build/vet/test ./...` pass on upstream code | D-013 |
| Implementation plan | 2026-11-23 | Self-review against the spec | `docs/superpowers/plans/2026-11-23-duckdb-storage.md`, `f398a84` |
| Task 1: schema tallies (Infer rebuilt on them) | 2026-11-23 | `go test ./internal/schema` | `8e3ff66` |
| Task 2: `internal/duckstore` engine | 2026-11-23 | `go test ./internal/duckstore` (round trip, widening from `_raw`, bad values, case clashes, conversions vs `schema.Convert`) | `03ebaf3` |
| Task 3: `query.RunSQL` with parity | 2026-11-23 | All 8055 parity queries match `query.Run` (`PARITY_FULL=1`) | `5233a0a`, D-021, D-022 |
| Task 4: web view served from DuckDB | 2026-11-23 | `go test ./...` (25 packages); `-race` on store, duckstore, web, recorder | `c2e38af`, D-023 |
| Task 5: day files, modes, rebuild, rollover, failure | 2026-11-23 | `go test ./...`; smoke run of the built binary (two runs, then `-web` alone over the files) | `25b971e` |
| `go.mod`/`go.sum` completed (`go mod tidy`) | 2026-11-24 | `go build/vet/test ./...` on the user's Mac (macOS 26.6, Go 1.27.4, cgo) | `2633b81` |
| Verification on a normal machine | 2026-11-24 | `go test ./...` (26 packages); all 8055 parity queries (`PARITY_FULL=1`, 2 m 8 s); `-race` on store, duckstore, web, recorder, query, main | `2633b81` |
| Spec sent for the author's review | 2026-11-24 | Spec-only PR open | jimbeveridge/loghorn#2 (branch `duckdb-storage-spec` on the user's fork) |
| Minor 6: `.duckdb.wal` owner-only | 2026-11-24 | `TestDayWALIsOwnerOnly` failed first | `b4fb42b` |
| Minor 1: a day-file batch is one transaction | 2026-11-24 | `TestFailedBatchLeavesNoPartialRows` failed first (records showed twice); `TestDayColumnWidensToStringInTransaction` | `7fc48f0` |
| Minors 10 and 3: plain runs defer rebuilds; `-web` binds before the store | 2026-11-24 | `TestOpenDeferredWriterBuildsNothing`, `TestLiveStoreOptionsDeferBuildWithoutWeb`, `TestWebPortCheckedBeforeRebuild` failed first | `9acef4e`, D-024 |

## In progress

| Work | Started | State |
|---|---|---|
| Task 6: speed check, docs, whole-branch review | 2026-11-23 | Speed measured (below). Review done: its 4 Important findings fixed test-first; Critical = `go.sum` (needs a networked machine); 10 Minor deferred (below) |

### Speed at about 770k records (one day, 2-vCPU cloud sandbox)

Median of 5, DuckDB vs today's `query.Run` on the same records (the old
path's snapshot copy and schema inference are not included, so it was
slower in real use, as the author's 716 ms / 520 ms show).

| Request | DuckDB | `query.Run` |
|---|---|---|
| Samples, one stream | 20 ms | 71 ms |
| Samples, filter + sort | 23 ms | 149 ms |
| Search | 68 ms | 112 ms |
| Table, p95 and error rate by status | 26 ms | 300 ms |
| Graph by severity | 49 ms | 154 ms |
| Graph, all kinds | 128 ms | 165 ms |
| Heatmap, db spans | 32 ms | 60 ms |
| Kinds / schema / trace | 9 / 5 / 19 ms | — |

Writing: about 25k records/s in this sandbox. A burst beyond the
200k-record queue is skipped for DuckDB, marks the day dirty, and is
rebuilt from text at the next start. The logs file for the day is 121 MB.

### Review findings (2026-11-24)

Fixed, each with a test that failed first:
- A saved epoch unit (s/us/ns) on a numeric timestamp was ignored —
  `TestRunSQLTimestampUnitOverride`.
- Queries failed while a column's type changed — `TestQueriesDuringRetype`
  (queries now take a read lock the writer holds across a type change).
- A stored-days `-web` reader held today's file, so the next writer
  couldn't open DuckDB — `TestOpenStoredLeavesTodayToTheWriter`,
  `TestOpenWriterKeepsGoingWhenTodayWontOpen` (readers read today from
  text; a writer that can't open today keeps it in memory).
- Replays and text loads skipped records when the queue filled —
  `TestReplayNeverSkips` (they now wait for room).

Deferred (Minor); 1, 3, 6 and 10 fixed 2026-11-24 (see Completed):
1. ~~A failed batch isn't one transaction (index rows can be orphaned).~~
   Worse than noted: the batch then went whole to memory, so its rows
   showed twice. Fixed.
2. `ReadStamp` checks only the logs file's text size.
3. ~~The `-web` port check runs after a possibly long rebuild.~~ Fixed.
4. Requests can fail briefly at midnight rollover/expiry.
5. A midnight race can send one record to memory only.
6. ~~`.duckdb.wal` files are not chmod 0600.~~ Fixed.
7. Each request can wait up to 2 × 250 ms when the writer is behind.
8. `rebuild` makes a parser per row; DuckDB `trim` differs from Go's for tabs/newlines.
9. Untimed records at the end of a rebuilt day aren't backfilled.
10. ~~Startup rebuild runs even without `-web` (about 44 s for 600k lines).~~
    Fixed by deferring, not by a background build (D-024).

## Planned (in order)

1. Remaining minors 2, 4, 5, 7, 8, 9, if wanted.
2. Try `-web` on a real project.
3. After the author answers the spec PR (#2): a PR for the code, from the
   user's fork.

## Blocked

| Item | Blocks | Needs |
|---|---|---|
| The user's account can't push to `jimbeveridge/loghorn` | Pushing branches there | Nothing now: branches go to the user's fork, PRs from there |

## Failed

None.

## Known limits (not failures)

- "All kinds" views read every stream table, so they slow down as tables
  multiply (128 ms above; D-021).
- Known differences from today: D-011's four, with #1 also covering a field
  whose type differs between services or days read together.
- The author has not reviewed this work (`docs/DECISIONS.md` O-2).

## Environment note (user's Mac)

The default SDK (`MacOSX27.0.sdk`) is newer than the Command Line Tools'
linker (26.6), so cgo links of DuckDB fail with "unknown architecture
arm64e.x1". Build and test with
`SDKROOT=/Library/Developer/CommandLineTools/SDKs/MacOSX26.sdk` until
the tools are updated. Anaconda's older `ld` is also ahead of Apple's on
`PATH`.

## Next actions

1. Wait for the author on the spec PR (jimbeveridge/loghorn#2).
2. Try `-web` on a real project; then the remaining minors.
