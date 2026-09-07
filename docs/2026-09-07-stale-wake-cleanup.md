# 2026-09-07 — Stale wake cleanup

**Status:** Step 3 of 4 from the `openclaw doctor` investigation
(`wakes_overdue=9407`, root cause: SQL type-coercion bug in
`ProcessScheduledTasks`, fixed in commit `4ed04b4`).

## Cron wakes cleared

**9456 cron wakes deleted** (count was 9419 at the start of the
investigation; grew by 37 between the start of the read-only
investigation and the Step 1 binary restart at 10:47 BST).

All `created_by = 'mpm-scheduler'` and
`metadata.kind = 'cron'`. Source queries:
`internal/core/scheduled_tasks.go:181` (before Step 1 fix).
None were ever marked `fired=1` — by design, the scheduler does
not consume cron wakes; `mpm-mcp`'s opportunistic fold was the
intended consumer and never ran against this DB.

## Orphan cascade wakes — inspected and deleted

**2 cascade wakes deleted** after on-inspection confirmation of test
pollution origin.

| id | target_time | reason |
|---|---|---|
| `wk-800e7484e54bf48ba6cb20c3f8242f71` | 2026-09-05 20:22:27 | cascade-reconcile: theory 11f1e2baa09ed7c0 requires review — downstream of invalidation e1658e2a3a5d31e0 (recovered from unscheduled state) |
| `wk-0b2c3558dd34db0500535de1a71a0bb7` | 2026-09-05 20:22:28 | cascade: theory 11f1e2baa09ed7c0 requires review — downstream of invalidation e1658e2a3a5d31e0 |

**Inspection trail:**

- The cascade invalidation event (`e1658e2a3a5d31e0`) targeted lesson
  `8b782a303905ebe5` with reason `confidence_floor`.
- Three identical `challenge` evidence rows seeded the lesson's
  confidence cross, all with `created_by =
  "log_to_changelog:deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"` and
  `source_group = "git"` and `strength = -0.6`.
- The `deadbeefdeadbeefdeadbeefdeadbeefdeadbeef` SHA is an unambiguous
  test placeholder — the only evidence trail here is from the Sep 5
  19:50/20:12 live-probe commits (`83c0b3a`, `d8dbd92`) which the
  operator had earlier flagged as "explicitly did probe the real DB".
- The downstream re-evaluation theory (`11f1e2baa09ed7c0`) and the
  original downstream theory (`effd6ad369b86c7b`) both still exist
  with `status = 'pending'`. They are out of scope for this pass per
  the operator's explicit instruction that theory triage requires a
  human to read content and judge real-vs-stale.

**Drain path attempt:**

- Ran `mpm cascade materialize --once`. Returned
  `materialized=0 failed=0 pending_after=0` — the cascade outbox
  intent is already `materialized`; no further outbox work to do.
- The orphan wakes have no pipeline path forward because their
  underlying outbox intent is already materialized; they were just
  notification triggers that never fired.

Per the operator's "only delete them if drain fails or if you
determine on inspection they're pure test artifacts with nothing
meaningful downstream" — both criteria met.

## Post-cleanup state

| metric | before | after |
|---|---:|---:|
| `mpm doctor` Scheduler line | "9407 wake(s) overdue" | "1 wake(s) overdue" |
| Cron wakes in `scheduled_wakes` | 9456 | 0 |
| Cascade wakes in `scheduled_wakes` | 2 | 0 |
| `scheduled_tasks` rows | 4 | 1 (`epistemic-compaction`) |

The remaining 1 overdue wake is a `cascade_summary` tick-summary
that the scheduler re-injects every minute; it is expected transient
behaviour, not a backlog.

## Cross-references

- Step 1 commit: `4ed04b4` — `fix(cron): pass int64 Unix epoch to
  scheduled_tasks due-query — stops 60s over-fire`
- Step 2 commit: `aa2ddb6` — `docs(ops): record removal of three
  non-canonical scheduled_tasks rows`
- MPM lesson: `7de0fa2df7898109` (silent time.Time → TEXT coercion)