# 2026-09-07 — Stale cron row cleanup

**Status:** Step 2 of 4 from the `openclaw doctor` investigation
(`wakes_overdue=9407`, root cause: SQL type-coercion bug in
`ProcessScheduledTasks`).

## What was removed

Three non-canonical rows from `scheduled_tasks`, all dated test-seed
leftovers from 2026-09-02 13:33–13:38 (acceptance-test runs that left
artifacts in the live DB):

| id | cron_expr | directive_id | status | reason |
|---|---|---|---|---|
| `acceptance-test-task` | `*/5 * * * *` | `mpm-seed-daemon-health` | active | non-canonical, test seed |
| `acceptance-test-task-2` | `0 4 * * *` | `mpm-seed-daemon-health` | active | non-canonical, test seed |
| `doc-check-task` | `*/10 * * * *` | `mpm-seed-daemon-health` | paused | non-canonical, test seed |

None appear in the canonical registry at
`internal/core/seed/scheduled_tasks.go`, whose only entry is
`epistemic-compaction` (`0 3 * * *`, directive
`mpm-seed-epistemic-compaction-policy`).

## What was preserved

`epistemic-compaction` — the single canonical task — was untouched:
`status=active`, `cron_expr="0 3 * * *"`, `directive_id="mpm-seed-epistemic-compaction-policy"`.

## Verification

Post-delete, the new scheduler (PID 50032, built from commit 4ed04b4)
was observed for one full tick (10:52:12 → 10:53:12):

- No "cron injected wakes" log lines (no tasks due; next legitimate
  fire for `epistemic-compaction` is 2026-09-08 03:00 UTC).
- No rows regenerated.
- Watchdog alive.
- `epistemic-compaction` row unchanged.

## Why this was safe to do

The Step 1 fix (`4ed04b4`) ensures `ProcessScheduledTasks` now only
matches rows where `next_run_at <= now` (was matching every active row
on every tick due to the SQL type-coercion bug). Even if the canonical
seed registry were somehow re-applied and re-created the test rows,
the new binary would not over-fire them. Step 2 is therefore
defensive cleanup, not a behaviour change.

## Cross-references

- Step 1 commit: `4ed04b4` — `fix(cron): pass int64 Unix epoch to
  scheduled_tasks due-query — stops 60s over-fire`
- Investigation report: see handoff preceding the 4-step cleanup prompt
- MPM lesson: `7de0fa2df7898109` (silent time.Time → TEXT coercion)