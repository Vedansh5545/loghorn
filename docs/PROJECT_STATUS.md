# Project status: DuckDB storage for loghorn

Last updated: 2026-11-24 · Branch `duckdb-v2` (v2 design), built on
`duckdb-storage`

**Status values:** Planned · In progress · Completed (verified) · Failed ·
Blocked. A task is Completed only when the check named next to it has
run and passed.

## v2 design (branch `duckdb-v2`)

Spec: `docs/superpowers/specs/2026-11-23-duckdb-storage-design.md` (v2,
in review on jimbeveridge/loghorn#2). Plan:
`docs/superpowers/plans/2026-11-24-duckdb-storage-v2.md`. Decisions
D-025 to D-029 are proposed until the author reviews the spec.

### Completed

| Work | Date | Verified by | Ref |
|---|---|---|---|
| v2 spec on the spec PR | 2026-11-24 | Pushed to the fork's `duckdb-storage-spec` | `19b583d` |
| Plan for v2 | 2026-11-24 | Self-review against the spec | `ef02aa6` |
| Tasks 1–4: fixed tables with JSON attributes, raw BLOB, two files, first import, `-db-dir` | 2026-11-24 | New tests failed first; `go test ./...`; all 8055 parity queries (`PARITY_FULL=1`) | `8c36168`, D-025–D-027, D-029 |
| Task 5: closed days to Parquet | 2026-11-24 | Archive tests failed first; `-race` on duckstore, store | `47d97d0`, D-028 |
| Task 6: measurements at 770k records | 2026-11-24 | `LOGHORN_SPEED=1` runs below | `cedf198` |
| Verification | 2026-11-24 | `go vet ./...`; `go test ./...`; `-race` on duckstore, store, web, query, main | `cedf198` |
| Whole-branch review fixes (below) | 2026-11-24 | Each Important finding's test failed first; `go test ./...`; `-race`; all 8055 parity queries | this commit |

### Measurements (about 770k records in one day; MacBook, arm64, 8 CPUs, 16 GB)

`LOGHORN_SPEED=1 LOGHORN_SPEED_DUCKDB_ONLY=1 go test -run SpeedAtScale -v -timeout 30m ./internal/store`.
Medians of 5 warm runs, through the store as the web view calls it.

| Request | In `logs.duckdb` | After archiving to Parquet |
|---|---|---|
| Samples, one stream | 21 ms | 40 ms |
| Samples, filter + sort | 87 ms | 152 ms |
| Search | 85 ms | 130 ms |
| Table, p95 and error rate by status | 310 ms | 338 ms |
| Graph by severity | 90 ms | 160 ms |
| Graph, all kinds | 32 ms | 51 ms |
| Heatmap, db spans | 86 ms | 136 ms |
| Kinds / schema / trace | 3 / 2 / 3 ms | 4 / 2 / 20 ms |

- Every representative request is under the ~500 ms target. Table p95 is
  the slowest: the latency field is text (`"0.123s"`) parsed per record.
- Restart with 770k records stored: 25 ms (no text is reparsed).
- Archiving the day at the next day's open: 1.5 s.
- Writing: about 36k records/s.
- Disk: `logs.duckdb` 192 MB and `traces.duckdb` 61 MB for the day; the
  day's Parquet archives are 24 MB and 8 MB (ZSTD).
- Peak RSS of the benchmark process: 1.7 GB, with DuckDB capped at 1 GB
  (`memoryLimit`); 2.2 GB uncapped. Spike 2 measured about 3 GB for the
  in-memory store.
- With `query.Run`'s in-memory copy of every record in the same process
  (about 3 GB beside DuckDB), the machine was under memory pressure:
  archiving took 25 s and Parquet queries up to 660 ms. Real runs don't
  hold that copy.

### Review findings (2026-11-24, independent review of `duckdb-storage..duckdb-v2`)

No Critical findings. Fixed:
- Important 1: a historical reader broke when the writer later deleted an
  archive it read. Readers now copy archives into memory too
  (`TestReaderSurvivesExpiredArchive`).
- Important 2: a historical run trusted files a deferred writer had
  started, showing only that run's records. It now reads the text unless
  the files are marked imported (`TestReaderReadsMeta`,
  `TestHistoricalNeedsImportedFiles`).
- Important 3: a partial import was marked complete. The mark now needs
  every text file read (`TestPartialImportIsNotMarked`).
- Important 4: the archive's rename wasn't synced before the day's rows
  were deleted. The directory is synced now.
- Important 5: records skipped for a full queue (as during a long
  midnight archive) were lost from DuckDB with one warning. They are
  counted, warned about at Close, and clear the imported mark so the next
  `-web` run imports them from text (`TestSkippedRecordsUnmarkImport`).
- Minor: an unreadable mark no longer looks like files to start again;
  RFC 3339 strings keep nanoseconds (`tsns`, `tsoff` cases); replacing an
  archive and deleting its rows happen under one view lock; stale doc
  comments fixed; unused `FileOptions.Now` removed.

Left as known limits: below.

### Known limits and gaps

- DuckDB files don't shrink: after archiving, `logs.duckdb` keeps its
  size (about the largest day) and reuses the space. Compaction is
  deferred, as the spec allows.
- A crash loses the queued batch from DuckDB (the text keeps it). A run
  that can't open the files leaves its records in text only. Neither is
  brought back automatically (D-027).
- Known value differences: numbers read as text take DuckDB's canonical
  spelling, and arrays read as text keep `<`, `>`, `&` unescaped (D-025).
- DuckDB's file lock is per process, so a second writer is kept out only
  across processes; tests can't check that in-process.
- DuckDB creates a `.wal` with the process umask; loghorn makes it 0600
  after each batch. In a `-db-dir` that others can read, a new WAL is
  readable by them until then. Use an owner-only directory.
- A string that isn't valid UTF-8 is queried with U+FFFD in place of the
  bad bytes (D-025).

### Next actions

1. Wait for the author on the v2 spec (jimbeveridge/loghorn#2), especially
   O-3.
2. Decide with the user whether `duckdb-v2` replaces jimbeveridge/loghorn#3
   or becomes a new PR.
3. Try `-web` on a real project.

## Environment note (user's Mac)

The default SDK (`MacOSX27.0.sdk`) is newer than the Command Line Tools'
linker (26.6), so cgo links of DuckDB fail with "unknown architecture
arm64e.x1". Build and test with
`SDKROOT=/Library/Developer/CommandLineTools/SDKs/MacOSX26.sdk` until
the tools are updated. Anaconda's older `ld` is also ahead of Apple's on
`PATH`.


## First design (branch `duckdb-storage`, jimbeveridge/loghorn#3)

### Completed

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
| Spec PR also carries `docs/DECISIONS.md` | 2026-11-24 | Pushed to the fork's `duckdb-storage-spec` | `f5f7b63` |
| Minor 2: a pair whose stamps disagree on text size is in doubt | 2026-11-24 | `TestStampsDisagreeingOnTextAreInDoubt` failed first | `b0db6c4` |
| Minor 8: SQL trims what Go trims; one parser for rebuilt rows | 2026-11-24 | Tab, newline and Unicode-space cases added to `TestRelationConvertsLikeConvert` failed first | `f375d0f` |
| Minor 9: a rebuilt day's untimed records take times from that day only | 2026-11-24 | `TestRebuiltDayTimesComeFromThatDay` failed first | `556c850` |
| Minors 4 and 5: reads wait out a rollover; late records keep a file | 2026-11-24 | `TestReadsDuringRolloverDontFail`, `TestLateDayRecordStillReachesAFile` failed first | `b7f588d` |
| Minor 7: a request waits for a lagging writer once | 2026-11-24 | `TestReadsWaitOnceWhileWriterIsBehind` failed first | `d59a987` |
| Found while verifying: a batch crossing midnight failed the day file and sent the rest of the run to memory | 2026-11-24 | `TestBatchAcrossMidnightIsWritten` failed first; day tests ×40 under `-race` beside the parity run (failed before) | `93ce5eb` |
| Verification after all fixes | 2026-11-24 | `go vet ./...`, `go test ./...`; `-race` on duckstore, store, web, main; all 8055 parity queries at `d59a987` | — |

### In progress

| Work | Started | State |
|---|---|---|
| Task 6: speed check, docs, whole-branch review | 2026-11-23 | Speed measured (below). Review done: its 4 Important findings, the Critical (`go.sum`) and all 10 Minors fixed; left: try `-web` on a real project |

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

Minor (all fixed 2026-11-24, see Completed):
1. ~~A failed batch isn't one transaction (index rows can be orphaned).~~
   Worse than noted: the batch then went whole to memory, so its rows
   showed twice.
2. ~~`ReadStamp` checks only the logs file's text size.~~
3. ~~The `-web` port check runs after a possibly long rebuild.~~
4. ~~Requests can fail briefly at midnight rollover/expiry.~~ Every store
   read now takes the lock the writer holds across rollover.
5. ~~A midnight race can send one record to memory only.~~
6. ~~`.duckdb.wal` files are not chmod 0600.~~
7. ~~Each request can wait up to 2 × 250 ms when the writer is behind.~~
8. ~~`rebuild` makes a parser per row; DuckDB `trim` differs from Go's.~~
9. ~~Untimed records at the end of a rebuilt day aren't backfilled.~~ Each
   day now takes times from its own text only; a day with no time keeps
   its records untimed.
10. ~~Startup rebuild runs even without `-web`.~~ Fixed by deferring, not
    by a background build (D-024).

### Planned (in order)

1. Try `-web` on a real project.
2. After the author answers the spec PR (#2): a PR for the code, from the
   user's fork.

### Blocked

| Item | Blocks | Needs |
|---|---|---|
| The user's account can't push to `jimbeveridge/loghorn` | Pushing branches there | Nothing now: branches go to the user's fork, PRs from there |

### Failed

None.

### Known limits (not failures)

- "All kinds" views read every stream table, so they slow down as tables
  multiply (128 ms above; D-021).
- Known differences from today: D-011's four, with #1 also covering a field
  whose type differs between services or days read together.
- The author has not reviewed this work (`docs/DECISIONS.md` O-2).

### Next actions

1. Wait for the author on the spec PR (jimbeveridge/loghorn#2).
2. Try `-web` on a real project.
