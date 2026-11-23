# Project status: DuckDB storage for loghorn

Last updated: 2026-11-23 · Branch `duckdb-storage`, based on `331ddc3`

**Status values:** Planned · In progress · Completed (verified) · Failed ·
Blocked. A task is Completed only when the check named next to it has
run and passed.

## Completed

| Work | Date | Verified by | Ref |
|---|---|---|---|
| Repo review and improvement plan | 2026-11-18 | Code reading plus three reproductions; repo not built | chat only |
| Storage approach chosen (layout B) and first decisions recorded | 2026-11-23 | User answers in chat | `docs/DECISIONS.md` D-001…D-008 |

## In progress

| Work | Started | State |
|---|---|---|
| Design for DuckDB storage (Superpowers brainstorming) | 2026-11-23 | Waiting on user: O-1 scope split |

## Planned (in order)

1. Write the design doc at
   `docs/superpowers/specs/2026-11-23-duckdb-storage-design.md`.
2. Builder check of the design, then one round of the user's questions.
3. User approves the design. Nothing is built before this.
4. Implementation plan at `docs/superpowers/plans/2026-11-23-duckdb-storage.md`.
5. Execute the plan with tests first, review each task, verify.
6. If O-1 is the split: a second design for running web view queries as SQL.

## Blocked

| Item | Blocks | Needs |
|---|---|---|
| DuckDB can't be installed in the cloud session (package installs off) | Running or testing any DuckDB code there | Package installs allowed, or work on a local machine |
| The cloud session can't push to `jimbeveridge/loghorn` | Sharing commits through GitHub | Push access, or the user applies the patch |
| The cloud session has Go 1.24.7; `go.mod` needs 1.26.1 | Building and testing loghorn there | Go 1.26 toolchain download or a local machine |

## Failed

None.

## Next actions

1. User answers O-1 (scope split).
2. Claude writes the design doc and runs the builder check.
