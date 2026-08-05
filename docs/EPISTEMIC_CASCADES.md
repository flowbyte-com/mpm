# Epistemic Cascades — Operator Guide

**Feature added:** 2026-08-04  
**MPM version:** post-audit-2026-07-16

## Overview

When a foundational artifact (memory, decision, or theory) is explicitly invalidated, downstream decisions and theories that relied on it remain structurally intact. Epistemic cascades automatically generate re-evaluation theories for every downstream reasoning artifact so the agent can consciously re-assess rather than operating on a now-invalid foundation.

Cascades fire only on explicit invalidation events:

- **Theory disproval** — `ResolveTheory` with `status="disproven"`  
- **Memory shred** — `ShredMemory` or `ShredMemoryWithCascade`  
- **Hard confidence crossing** — `RecomputeConfidence` drops confidence below `HardConfidenceInvalidationThreshold` (default: `0.3`)

Ordinary confidence decreases that stay above the threshold do **not** trigger cascades.

## What cascades produce

Each invalidation event creates one **cascade intent** per downstream decision/theory. The materializer (running as `mpm cascade materialize`) converts each intent into a **pending re-evaluation theory** with metadata identifying the dead foundation, the affected downstream, and the trigger reason.

The generated theory's hypothesis states that the downstream artifact requires re-evaluation. Its validation criteria require independent review followed by binary resolution as proven or disproven.

## Outbox table schema

```sql
CREATE TABLE epistemic_cascade_outbox (
    id                          TEXT PRIMARY KEY,
    invalidation_event_id       TEXT NOT NULL,          -- stable event ID for causal tracing
    dead_artifact_id            TEXT NOT NULL,
    dead_artifact_type          TEXT NOT NULL,          -- 'memory' | 'decision' | 'theory'
    downstream_artifact_id     TEXT NOT NULL,
    downstream_artifact_type    TEXT NOT NULL,          -- 'decision' | 'theory' only (lessons/global rules excluded)
    trigger_evidence_id        TEXT,
    cascade_depth              INTEGER NOT NULL DEFAULT 0,
    reason                     TEXT NOT NULL,           -- e.g. 'memory_shredded', 'theory_disproven'
    status                     TEXT NOT NULL,           -- 'pending' | 'processing' | 'materialized' | 'failed'
    materialized_theory_id     TEXT,                    -- set after successful materialization
    attempt_count              INTEGER NOT NULL DEFAULT 0,
    next_retry_at              INTEGER,                  -- unix epoch seconds; NULL = eligible now
    terminal_error            TEXT,
    created_at                 INTEGER NOT NULL,
    updated_at                 INTEGER NOT NULL
);

-- Unique key for idempotent dedup within one invalidation event
UNIQUE(dead_artifact_id, downstream_artifact_id, invalidation_event_id)
```

**Indexes:**
```sql
idx_epistemic_cascade_outbox_event   ON epistemic_cascade_outbox(invalidation_event_id);
idx_epistemic_cascade_outbox_dead   ON epistemic_cascade_outbox(dead_artifact_id);
idx_epistemic_cascade_outbox_status_retry ON epistemic_cascade_outbox(status, next_retry_at);
```

### Query examples

```sql
-- Pending intents waiting to be materialized
SELECT id, dead_artifact_id, downstream_artifact_id, cascade_depth, reason
FROM epistemic_cascade_outbox
WHERE status = 'pending'
ORDER BY created_at ASC;

-- Failed (dead-letter) intents requiring operator review
SELECT id, dead_artifact_id, downstream_artifact_id, cascade_depth,
       terminal_error, attempt_count, created_at
FROM epistemic_cascade_outbox
WHERE status = 'failed'
ORDER BY updated_at DESC;

-- Outbox summary
SELECT status, COUNT(*) FROM epistemic_cascade_outbox GROUP BY status;

-- All cascade theories (materialized)
SELECT id, metadata
FROM memories
WHERE collection = 'theories'
  AND json_extract(metadata, '$.cascade') = 1;
```

## CLI usage

### `mpm cascade materialize`

Drains the outbox by repeatedly claiming pending intents, materializing each into a re-evaluation theory, and scheduling a cascade wake for the agent.

```bash
mpm cascade materialize [flags]
```

**Flags:**

| Flag | Default | Description |
|------|---------|-------------|
| `--once` | `false` | Run one batch and exit |
| `--max-iterations N` | `0` (unbounded) | Bound the number of batches; exits code `2` on timeout |
| `--poll-interval T` | `5s` | Sleep between empty-queue polls (minimum `1s`) |

**Exit codes:**

| Code | Meaning |
|------|---------|
| `0` | Queue drained successfully |
| `1` | Runtime error |
| `2` | `--max-iterations` exceeded |

```bash
# Drain once (single batch)
mpm cascade materialize --once

# Run with operator oversight: stop after 10 iterations
mpm cascade materialize --max-iterations 10

# Continuous drain (use in systemd timer or cron)
mpm cascade materialize
```

**Cron scheduling example** — run every 5 minutes to drain the queue:

```cron
*/5 * * * * /usr/local/bin/mpm cascade materialize --poll-interval 30s
```

### `mpm cascade list-dead-letters`

Inspects failed (dead-letter) intents that exhausted retries or were suppressed at depth limit.

```bash
mpm cascade list-dead-letters
```

Exit code `0` always (prints summary even when no dead-letters exist).

Sample output:
```
CASCADE DEAD-LETTER INTENTS (status=failed)
------------------------------------------------------------------------------------------------------------------------
ID                                  EVENT_ID  DEAD_TYPE  DOWN_TYPE   DOWN_ID     DEPTH  ATTEMPTS TERMINAL_ERROR
------------------------------------------------------------------------------------------------------------------------
intent-abc123                      evt-xyz    memory     decision    abc123456   4      3        depth exceeded MaxCascadeDepth

Outbox summary — pending=2 processing=0 materialized=15 failed=1
```

## Cascade depth limit

Maximum recursion depth: **`MaxCascadeDepth = 3`** (configurable via `CascadeMaterializerOptions.MaxCascadeDepth`).

| Depth | Meaning |
|-------|---------|
| 0 | Root invalidation event |
| 1 | Direct downstream cascade theory |
| 2 | Cascade of a depth-1 cascade theory |
| 3 | Final permitted recursive cascade |
| 4+ | Suppressed; CRITICAL audit event emitted; intent enters dead-letter state |

The depth is embedded in each outbox row (`cascade_depth`) and in the materialized theory's metadata (`cascade_depth`).

## Wake delivery cap

`CheckPendingWakes` applies a per-call cap of **`MaxCascadeWakePerCheck = 3`** cascade wakes. Notification and cron wakes are unaffected. Rows beyond the cap remain pending for the next call.

```sql
-- Verify the cap constant
SELECT * FROM scheduled_wakes
WHERE metadata LIKE '%"kind":"cascade"%'
ORDER BY target_time ASC;
```

**Pagination example:**
- 9 cascade wakes pending → three calls to `check_wakes` with `kinds=["cascade"]` returns 3, 3, 3.

## Retry and backoff

| Attempt | Backoff |
|---------|---------|
| 1 | 2 seconds |
| 2 | 4 seconds |
| 3 | 8 seconds |

After `MaxRetries = 3` attempts, the intent enters **dead-letter state** (`status='failed'`) and a `CRITICAL` audit event is recorded with the intent ID, source/downstream IDs, attempt count, and terminal error.

## Dependency discovery

Two edge sources are combined when discovering downstream targets:

1. **Explicit `dependencies` JSON** — theories that list the dead artifact ID in their `dependencies` column
2. **Typed provenance citations** — `epistemic_provenance` rows linking source → downstream (created when `source_ids` are passed to `RecordDecision` or `ProposeTheory`)

Only downstream artifacts of type **`decision`** or **`theory`** are eligible. Lessons and global rules are explicitly excluded.

## Failure handling

| Failure | Behavior |
|---------|----------|
| Outbox INSERT fails | Root mutation rolls back (atomicity: no divergence) |
| Theory creation fails | Exponential backoff retry; after 3 attempts → dead-letter + CRITICAL audit |
| Depth exceeds limit | Intent suppressed; dead-letter + CRITICAL audit |
| Materializer crashes | Restart recovery: `status='processing'` rows with `updated_at > 5 minutes ago` reset to `status='pending'` |

## Hard confidence threshold

`HardConfidenceInvalidationThreshold = 0.3`

A cascade fires only when confidence **crosses** the threshold (not just decreases toward it). Once below the threshold, further recomputes do not re-trigger cascades.

## Federation (shared DB)

When `MPM_SHARED_DB` is attached, cascade intents are written to both local and shared `epistemic_cascade_outbox` tables atomically. The materializer processes the local outbox; the shared outbox is available to cross-agent shared-materializer instances. The `epistemic_provenance` federated read path surfaces citations from both DBs.

## Configuration knobs

| Knob | Default | Notes |
|------|---------|-------|
| `MaxCascadeDepth` | `3` | Storage-level ceiling on `cascade_depth` |
| `MaxCascadeWakePerCheck` | `3` | Wake delivery cap per `check_wakes` call |
| `MaxRetries` | `3` | Retries before dead-letter |
| `CascadeMaterializerOptions.WakeDelay` | `1s` | Delay before scheduling cascade wake after materialization |
| `CascadeMaterializerOptions.BatchSize` | `10` | Intents claimed per `MaterializeBatch` call |
| `HardConfidenceInvalidationThreshold` | `0.3` | Confidence floor for cascade trigger |

All are constants in `internal/core/`; the materializer options are also settable via `NewCascadeMaterializer(dm, opts)`.

## Operational notes — scheduler-driven cascade drain

Cascade intent draining is now driven exclusively by the `cascade_drain`
handler in `mpm-scheduler`. There is no longer an auto-starting
background goroutine on the DatabaseManager.

### Confirm draining is happening

`mpm-scheduler` logs a `cascade drain yielded` line on every tick where
the handler runs, with one of four `yield_reason` values:

| reason              | meaning                                              |
|---------------------|------------------------------------------------------|
| `queue_empty`       | outbox drained, normal exit                          |
| `budget_exhausted`  | budget (default 30s) ran out, more pending           |
| `context_cancelled` | scheduler shutdown mid-tick, expected on `mpm stop`  |
| `error`             | DB-level error during a batch, investigate logs      |

### "I shredded a root directive but the cascade hasn't materialized"

1. Is `mpm-scheduler` running? Look for `cascade drain yielded` log lines.
2. Is the outbox non-empty?
   ```sql
   sqlite3 src/db/mpm.db \
     "SELECT COUNT(*) FROM epistemic_cascade_outbox WHERE status='pending';"
   ```
3. Is the handler yielding `budget_exhausted` consistently? Check
   `intents_materialized` per tick; raise `CascadeDrainOptions.Budget` if
   you need faster drain after large blasts.
4. Are intents in `status='failed'`? Inspect with `mpm cascade list-dead-letters`.

### Foreground escape hatch

If you don't want to run `mpm-scheduler`, use `mpm cascade materialize`.
No time budget — the operator chose to wait.

### Tuning the budget

`CascadeDrainOptions.Budget` defaults to 30s. The handler yields when
the budget runs out; the scheduler's 60s tick has 30s of headroom.
