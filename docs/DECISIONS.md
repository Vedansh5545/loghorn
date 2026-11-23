# Decisions

Technical decisions for the DuckDB storage work, newest last. Earlier
architecture decisions live in
`docs/superpowers/specs/2026-11-19-architecture-working-notes.md` (cited
below as "notes #N") and are not repeated here.

**Rules for this file:** never edit an entry's decision after it is
accepted. To change one, add a new entry and mark the old one
`Superseded by D-NNN`.

**Status values:** Proposed · Accepted (provisional, until the design doc
is approved) · Accepted · Superseded · Rejected.

**Decided by:** *user*, the person directing this work; *author*, the
loghorn maintainer, via the notes; *Claude*, a proposal the user accepted
or has not yet answered.

---

## D-001 Store logs and traces in DuckDB, in two files

- **Date:** 2026-11-19, confirmed 2026-11-23 · **Status:** Accepted (provisional) · **Decided by:** user
- **Decision:** Replace the web view's in-memory record list with DuckDB:
  one file for logs, one for traces. Metrics stay discarded.
- **Why:** Faster filters and queries in the web view. The author already
  chose DuckDB as the only engine (notes #5, spikes 2 and 3).
- **Alternatives:** Keep the in-memory store (slow, about 3 GB RAM per run,
  spike 2); one DuckDB file for both signals; SQLite or ClickHouse
  (rejected by the author's spikes).
- **Consequences:** cgo builds. DuckDB lets one process write a file, so
  one loghorn process owns both files.

## D-002 Layout B: a schema per service, a table per stream, typed columns

- **Date:** 2026-11-23 · **Status:** Accepted (provisional) · **Decided by:** user (picked from A/B/C)
- **Decision:** Inside each file, a schema per service key and a table per
  stream, with one typed column per field, named by its dotted path.
- **Why:** It matches notes #16 ("database = service") and is the layout
  spike 2 measured at 1–17 ms on about 1 GB.
- **Alternatives:** A: one table per signal, attributes in one JSON column
  (fixed shape, but JSON parsing on every filter; speed not measured).
  C: A plus promoted typed columns (most moving parts).
- **Consequences:** Tables and columns are created as data arrives.
  Cross-stream reads need generated views (D-004). Type changes need rules
  (D-005).

## D-003 Span tables by span type; the span name is a column

- **Date:** 2026-11-23 · **Status:** Accepted (provisional) · **Decided by:** Claude
- **Decision:** In the traces file, one table per span type (`db`,
  `http.server`, …, and catch-all `span`). The displayed kind (the span
  name, for unclassified spans) is a `stream` column.
- **Why:** Unclassified spans are listed under their own name today
  (`internal/record/span.go`), so one table per name could mean thousands of
  tables.
- **Alternatives:** A table per displayed kind; one spans table (the
  2026-11-19 proposal, see D-009).
- **Consequences:** The web view's kind list reads the `stream` column.

## D-004 The shared format is a set of generated views

- **Date:** 2026-11-23 · **Status:** Accepted (provisional) · **Decided by:** Claude
- **Decision:** loghorn generates `all_records` (the columns every
  `record.Record` has) and one view per stream over both files. The web
  view reads only views.
- **Why:** `record.Record` is already the shared format every view uses
  (`internal/record/record.go`). Views keep that shape, so web code changes
  stay small.
- **Alternatives:** A third DuckDB file holding a merged copy (duplicates
  the data); merging in Go after separate queries.
- **Consequences:** Views must be rebuilt when a table or column appears.

## D-005 Schema evolution and bad values

- **Date:** 2026-11-23 · **Status:** Accepted (provisional) · **Decided by:** author (notes #19, #21)
- **Decision:** A new field adds a column. A changed type widens the column
  (integer → decimal → text). A value that doesn't fit is empty (NULL) in
  that column and stays visible in `_raw`.
- **Why:** Logs are never dropped or hidden.
- **Consequences:** Every table keeps `_raw`. Long text columns are
  compressed with zstd (spike 2).

## D-006 Per-repo, per-day files in `.loghorn/`

- **Date:** 2026-11-23 · **Status:** Accepted (provisional) · **Decided by:** Claude
- **Decision:** `.loghorn/logs-YYYY-MM-DD.duckdb` and
  `.loghorn/traces-YYYY-MM-DD.duckdb`, written by the run that holds
  `loghorn.lock`, rotated at midnight, kept 3 days like the text files.
- **Why:** It fits today's code and lock. DuckDB files don't shrink after
  DELETE, so expiry drops whole days. The author also chose a file per day
  (notes #39).
- **Alternatives:** A central store owned by a new `loghorn serve` (notes
  #4, #20): the long-term direction, but a separate project.
- **Consequences:** `serve` can take the same layout over later. Queries
  spanning days read several files.

## D-007 Text files stay the source of truth; no ingestion changes

- **Date:** 2026-11-23 · **Status:** Accepted (provisional) · **Decided by:** Claude
- **Decision:** `loghorn.log` and `loghorn.traces.jsonl` keep being
  written. Records enter DuckDB where they enter the in-memory store today
  (`store.Add`), not in the OTLP receiver. DuckDB can be rebuilt from the
  text files.
- **Why:** Someone else is working on ingestion (anti-goal in
  `docs/superpowers/specs/2026-11-20-span-streams-design.md`). It also gives
  a safe fallback.
- **Consequences:** Disk holds both forms for now (notes open question 6).

## D-008 Batched background writes

- **Date:** 2026-11-23 · **Status:** Accepted (provisional) · **Decided by:** Claude
- **Decision:** Rows are appended in batches in the background, never one
  per request.
- **Why:** Spike 2 saw DuckDB flushes stall for up to 0.5 s.
- **Consequences:** Newly arrived records appear in queries after a short
  delay. The exact batch size and delay are left to the design.

## D-009 Single spans table (2026-11-19 proposal)

- **Date:** 2026-11-19 · **Status:** Superseded by D-002 and D-003 · **Decided by:** Claude
- **Decision:** One `spans` table per service, shared columns with logs,
  an `all_records` view.
- **Why superseded:** The web view now sorts spans into streams by span
  type (commits `808cd5b`, `1dd0d54`), so spans follow the per-stream
  layout.
- **Note:** The proposal was a demo SQL file shared in chat and was never
  added to the repo.

---

## Open questions

- **O-1 Scope split** (asked 2026-11-23, waiting on user). Option one:
  this design covers storage plus views, and a second design moves the web
  view's queries to SQL. Option two: both in one design. Claude recommends
  the split. The speed gain needs the SQL part (notes #35).
- **O-2 Author review.** D-001 to D-008 haven't been reviewed by the loghorn
  author. They follow his notes except D-001's two-files-by-signal split.
