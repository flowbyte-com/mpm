# 2026-09-07 — Malformed-metadata wake cleanup

**Status:** Step 4 of 4 from the `openclaw doctor` investigation
(`wakes_overdue=9407`, root cause: SQL type-coercion bug in
`ProcessScheduledTasks`, fixed in commit `4ed04b4`).

## Rows deleted

4 rows from `scheduled_wakes` where `json_valid(metadata) = 0`:

| id | fired_at | reason |
|---|---|---|
| `wk-858c2d2baae096a22ade9c2731e0da8d` | 2026-09-03 12:41:32 | "Verify scheduler daemon tick and delivery — 808 verification" |
| `wk-766130cea9df77432ffb71472669ac0b` | 2026-09-03 12:43:51 | "808 verification — round 2, tight fuse" |
| `wk-074c7db76c86d0d36203f5ebaba70aef` | 2026-09-03 12:43:56 | "808 verification — round 3, opportunistic trigger test" |
| `wk-84ee294182d3bfb722424b254c526c6b` | 2026-09-03 14:29:34 | "A" (truncated input — 1-character reason) |

All 4 already had `fired = 1`; removal does not change the overdue-wake
count. The three "808 verification" rows are from operator probe runs
on 2026-09-03; the "A"-reason row is the result of malformed input
(cron-expr or wake-reason was truncated to one character somewhere in
the pipeline).

## Behavioural impact

These rows were the source of the recurring
`level=WARN msg="cascade drain: dedupe lookup failed; inserting wake anyway" err="query last cascade summary: malformed JSON"`
log line on every scheduler tick. The cascade-summary wake selection
query (`SELECT last cascade summary`) iterates these rows and the JSON
parse fails on each one. The handler logs the WARN and falls through
to inject a fresh `cascade_summary` wake anyway, so the warning was
spurious and self-perpetuating.

After deletion, the dedupe lookup returns clean JSON and the WARN
stops. Verified by observing 75 s of post-cleanup scheduler logs.

## Pre/post state

| metric | before | after |
|---|---:|---:|
| `scheduled_wakes` rows with malformed `metadata` | 4 | 0 |
| WARNs in scheduler log per tick (cascade_summary path) | 1 | 0 |
| Overdue-wake count (unchanged — these rows were fired=1) | 1 | 1 |

## Cross-references

- Step 1 commit: `4ed04b4` — `fix(cron): pass int64 Unix epoch to
  scheduled_tasks due-query — stops 60s over-fire`
- Step 2 commit: `aa2ddb6` — `docs(ops): record removal of three
  non-canonical scheduled_tasks rows`
- Step 3 commit: `36092a4` — `docs(ops): record stale cron + cascade
  wake cleanup (9456 + 2 deleted)`
- MPM lesson: `7de0fa2df7898109` (silent time.Time → TEXT coercion)