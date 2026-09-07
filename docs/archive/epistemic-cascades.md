# Epistemic Cascades — Operator Guide

**Feature added:** 2026-08-04
**Positive-direction cascade added:** 2026-09-07
**MPM version:** post-2026-07-16 (security audit baseline)

## Conceptual model

When the epistemic status of a foundational artifact crosses a hard boundary, MPM preserves downstream artifacts and emits explicit re-evaluation intents rather than silently rewriting downstream conclusions.

Cascades are a **safety mechanism**, not a propagation mechanism. They never overwrite downstream content; they generate a pending re-evaluation theory that a human or an agent must resolve.

Cascade operates in two directions. The two directions share the outbox table, the materializer, the wake machinery, and the atomicity guarantees. They differ in their trigger surfaces, their polarity contract, and the hypothesis text the materializer emits.

| | negative cascade | positive cascade |
|---|---|---|
| foundation event | becomes invalid / disproven | becomes proven / crosses confidence ceiling |
| trigger reasons | `theory_disproven`, `memory_shredded`, `confidence_floor` | `foundation_proven`, `confidence_ceiling` |
| discovery path | `dependencies` JSON **OR** `epistemic_provenance` rows (any polarity, including NULL) | `epistemic_provenance` rows **only**, filtered to `polarity='assumes_false'` |
| downstream eligibility | decisions and theories | decisions and theories that have explicitly opted in via `polarity='assumes_false'` |
| hypothesis framing | "foundation invalidated; review whether downstream conclusion still holds" | "foundation proven; review whether downstream conclusion still holds given the foundation is now established" |

The trigger surfaces are **not identical**. Negative cascades fire from the full discovery path. Positive cascades fire only from the explicit provenance path with `assumes_false` polarity. Bare `dependencies` JSON entries cannot trigger positive cascades because they carry no polarity field and are structurally excluded from the positive discovery path (not merely NULL-defaulted).

## What cascades produce

Every cascade event (negative or positive direction) creates one **cascade intent** per downstream decision/theory. The materializer (running inside `cascade_drain` on `mpm-scheduler`, or directly via `mpm cascade materialize`) converts each intent into a **pending re-evaluation theory** with metadata identifying the foundation, the affected downstream, and the trigger reason.

The generated theory is written via `ProposeTheoryWithExtras` (the standard theory write path — scanner + FTS), not a parallel write surface.

## Operational model

### Normal operation: scheduler-driven drain

```
NORMAL OPERATION
mpm-scheduler
   -> cascade_drain handler (registered tick handler)
   -> claim pending intents (atomic claim in BEGIN IMMEDIATE)
   -> materialize each intent into a re-evaluation theory
   -> schedule cascade wakes for the agent
```

`cascade_drain` is registered as a tick handler in `cmd/mpm-scheduler/main.go`. There is no auto-starting background goroutine on the DatabaseManager. There is no second scheduling mechanism; the doc's previous reference to a cron entry is stale.

### Foreground escape hatch: `mpm cascade materialize`

`mpm cascade materialize` runs the same materializer with no time budget — the operator has chosen to wait. Use it when:

- You don't want to run `mpm-scheduler` for a one-shot drain.
- You need to drain a backlog outside the scheduler's tick cycle (post-incident catch-up, manual cleanup, CI smoke test).

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
```

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

## Generated cascade-theory provenance

Every materialised cascade intent becomes a pending theory with:

### Subject of review

The downstream artifact. The generated theory's hypothesis and validation criteria are written about the downstream (`intent.DownstreamArtifactID`, `intent.DownstreamArtifactType`). The foundation is mentioned as the cited dependency, not as the subject.

### How the foundation is represented

The foundation (`intent.DeadArtifactID`) is included in **two** distinct places:

1. **Theory `dependencies` column** — JSON array; contains `[intent.DeadArtifactID]`. This is the discoverable citation for subsequent cascade discovery.
2. **Theory `metadata.cascade.*` fields** — top-level cascade metadata blob:
   - `cascade: true`
   - `cascade_version: 1`
   - `dead_artifact_id: <intent.DeadArtifactID>`
   - `dead_artifact_type: <intent.DeadArtifactType>`
   - `downstream_artifact_id: <intent.DownstreamArtifactID>`
   - `downstream_artifact_type: <intent.DownstreamArtifactType>`
   - `cascade_depth: <intent.CascadeDepth>`
   - `generated_at: <RFC3339>`
   - `trigger_evidence_id: <optional, only when `intent.TriggerEvidenceID.Valid`>`

The theory also carries a provenance row in `artifact_provenance` via `WithProvenanceOverride(provenanceWithParent(dead_artifact_id))`. That row records `parent_artifact_id = intent.DeadArtifactID` for forensic walking of the causal chain.

### Citation behaviour

The generated theory does **not** cite the downstream artifact. It is generated FOR the downstream, but the dependency link runs in the opposite direction (foundation → theory). The `epistemic_provenance` row written for the cascade theory has `parent_artifact_id = dead_artifact_id`; it does not add a polarity-bearing citation of the downstream.

### Polarity

The generated cascade theory does **NOT** carry a polarity-bearing provenance row. `WithProvenanceOverride` writes the `artifact_provenance` row (parent/causality lineage), but the polarity-aware table is `epistemic_provenance`, which is not written by the materializer. Effectively:

> Generated cascade theories have NULL polarity on the path that the positive-cascade discovery queries. They are **invisible** to the positive-direction discovery query (`SELECT ... FROM epistemic_provenance WHERE polarity='assumes_false'`).

This is a deliberate safety property: even if a future code change introduced a positive cascade trigger that defaulted to `assumes_true`, a generated cascade theory would not be discovered through the polarity path because the row simply does not exist.

The polarity of a generated cascade theory in the storage sense is **NULL**, and the discovery paths respect that. It is not "inferred to assume_true" or "inferred to assume_false" — it is absent.

### Recursive cascade participation

A generated cascade theory **can** participate in subsequent cascades via the negative-direction discovery path:

- The theory's `dependencies` JSON contains the foundation ID.
- The negative discovery path queries `dependencies` JSON (`WHERE json_each.value = ?`).
- Therefore, if the foundation is **further** invalidated, the generated cascade theory will be discovered as a downstream and a fresh intent will be created for it (subject to `cascade_depth` limit).

A generated cascade theory **cannot** participate in subsequent positive cascades through the polarity path because no `epistemic_provenance` row exists for it.

The `cascade_depth` field on each intent bounds recursion: depth 0 is the root event, depth 1 is a direct cascade, depth `MaxCascadeDepth` is the final permitted cascade, depth `> MaxCascadeDepth` is suppressed with a `CRITICAL` audit event.

### Outbox row fields used by the materializer

The materializer reads from the outbox row:

| Field | Used for |
|-------|----------|
| `invalidation_event_id` | Stable causal trace id; also written into the theory's tags as `"cascade:<event_id>"`. |
| `dead_artifact_id` / `dead_artifact_type` | Identifies the foundation. |
| `downstream_artifact_id` / `downstream_artifact_type` | Identifies the subject of the re-evaluation. |
| `trigger_evidence_id` | Optional; only set when the cascade was triggered by an evidence-driven `RecomputeConfidence`. Forwarded into `metadata.cascade.trigger_evidence_id`. |
| `cascade_depth` | Bounded by `MaxCascadeDepth`. |
| `reason` | Branches the hypothesis text (positive vs negative wording). |

## Cascade depth limit

Maximum recursion depth: **`MaxCascadeDepth = 3`** (configurable via `CascadeMaterializerOptions.MaxCascadeDepth`).

| Depth | Meaning |
|-------|---------|
| 0 | Root invalidation event |
| 1 | Direct downstream cascade theory |
| 2 | Cascade of a depth-1 cascade theory |
| 3 | Final permitted recursive cascade |
| 4+ | Suppressed; CRITICAL audit event emitted; intent enters dead-letter state |

The depth is embedded in each outbox row (`cascade_depth`) and in the materialised theory's metadata (`cascade_depth`).

## Polarity — explicit-only opt-in for positive cascades

Polarity is a **safety invariant**, not merely an implementation detail. The `epistemic_provenance` table has a `polarity` column with a CHECK constraint restricting values to NULL, `'assumes_true'`, or `'assumes_false'`:

| Polarity value | Behaviour for positive cascade |
|---|---|
| **NULL** | Inert for positive cascades. Pre-existing citations and every call without an explicit polarity land here. |
| **`'assumes_false'`** | Explicit opt-in: "this downstream artifact assumes the source is false". When the source is proven true, the dependent surfaces for re-evaluation. |
| **`'assumes_true'`** | Stored (storage contract) but currently unused by any trigger surface. Reserved for forward compatibility. |

**Safety invariant:**

> Polarity is **never inferred** from citation content (no keyword matching), from natural-language wording (no "negation" detection), from semantic similarity, or from dependency structure. The design is explicit-only — same stance as confirmation/contradiction (see `docs/archive/epistemic-confirmation.md`).

A downstream that explicitly negates its foundation in plain English text but does not pass `polarity='assumes_false'` to `RecordProvenance` will not fire a positive cascade when the foundation is proven. That is the intended behavior, not a bug.

## Confidence threshold-crossing detectors

The cascade triggers on the **crossing event**, not on arbitrary changes in confidence. Mere decreases or increases do not trigger; the transition itself does.

### Negative threshold-crossing detector

```
old >= HardConfidenceInvalidationThreshold (0.3)
new <  HardConfidenceInvalidationThreshold (0.3)
```

The trigger fires only when both conditions hold: the artifact was at-or-above the threshold on the previous recompute, AND the new recompute puts it below. Once below the threshold, subsequent recomputes that continue to be below it do **not** re-trigger the cascade. The detector is implemented at `internal/core/evidence_store.go:344-345`:

```go
crossed := (!hasOldConf || oldConf >= HardConfidenceInvalidationThreshold) &&
    conf < HardConfidenceInvalidationThreshold
```

The `!hasOldConf` clause treats a brand-new confidence record as if its prior value were at-or-above the threshold (the safe side for first-time invalidation).

### Positive threshold-crossing detector

```
old <  HardConfidenceProvenThreshold (0.8)
new >= HardConfidenceProvenThreshold (0.8)
```

Mirror image: the artifact was below the threshold on the previous recompute AND the new recompute climbs to at-or-above. Confidence increases that stay below the proven threshold do **not** trigger. Implemented at `internal/core/evidence_store.go:365-366`:

```go
ceilingCrossed := (!hasOldConf || oldConf < HardConfidenceProvenThreshold) &&
    conf >= HardConfidenceProvenThreshold
```

### Constants

| Constant | Default |
|---|---|
| `HardConfidenceInvalidationThreshold` | `0.3` |
| `HardConfidenceProvenThreshold` | `0.8` |

## Wake delivery cap

`CheckPendingWakes` applies a per-call cap of **`MaxCascadeWakePerCheck = 3`** cascade wakes. The cap limits **delivery** per `check_wakes` call, not cascade-intent creation.

> The wake cap throttles delivery/notification. It does not discard, suppress, or silently coalesce pending cascade intents. Cascade intents live in `epistemic_cascade_outbox`; the cap only affects how many cascade-flavoured wake rows are surfaced to the agent in a single `check_wakes` call.

Notification and cron wakes are unaffected. Cascade wakes beyond the cap remain pending for the next call.

```sql
-- Verify the cap constant
SELECT * FROM scheduled_wakes
WHERE metadata LIKE '%"kind":"cascade"%'
ORDER BY target_time ASC;
```

**Pagination example:** 9 cascade wakes pending → three calls to `check_wakes` with `kinds=["cascade"]` returns 3, 3, 3.

## Scheduler drain budget semantics

The `cascade_drain` handler runs on every scheduler tick under a per-tick wall-clock budget (default 30s). The handler yields on **four** distinct reasons and logs each with the `yield_reason` field:

| `yield_reason` | Operational meaning |
|---|---|
| `queue_empty` | Outbox drained, normal completion. No more pending intents. Handler exits cleanly until next tick. |
| `budget_exhausted` | Per-tick budget consumed before the queue drained. More pending intents remain. Expected under load — the next tick will resume. |
| `context_cancelled` | Scheduler shutdown mid-tick (e.g. `mpm-scheduler` stopped). Expected on daemon shutdown; the next start picks up where it left off. |
| `error` | Actual handler or DB failure during a batch. Investigate logs. The handler logs an `AuditWarn` and a structured `Error` line. |

`budget_exhausted` is **not** a failure. It is a normal scheduling-yield condition: the handler does what it can within its budget and yields to the next tick. An operator who sees `budget_exhausted` consistently should consider:

- Increasing `CascadeDrainOptions.Budget` (the per-tick ceiling).
- Investigating the outbox for unusually large backlogs (`SELECT COUNT(*) FROM epistemic_cascade_outbox WHERE status='pending'`).

The scheduler's 60s tick has 30s of headroom for the cascade_drain budget by default.

## Retry and backoff

| Attempt | Backoff |
|---------|---------|
| 1 | 2 seconds |
| 2 | 4 seconds |
| 3 | 8 seconds |

After `MaxRetries = 3` attempts, the intent enters **dead-letter state** (`status='failed'`) and a `CRITICAL` audit event is recorded with the intent ID, source/downstream IDs, attempt count, and terminal error.

## Dependency discovery

Two edge sources are combined when discovering downstream targets for **negative** cascades:

1. **Explicit `dependencies` JSON** — theories that list the dead artifact ID in their `dependencies` column.
2. **Typed provenance citations** — `epistemic_provenance` rows linking source → downstream (created when `source_ids` are passed to `RecordDecision` or `ProposeTheory`).

Only downstream artifacts of type **`decision`** or **`theory`** are eligible. Lessons and global rules are explicitly excluded (`isEligibleCascadeType` filter).

**Positive cascades can only originate from path 2.** Bare `dependencies` JSON entries have no polarity field and are structurally excluded from the positive-direction discovery path — not merely NULL-defaulted. `discoverPositiveCascadeTargets` queries only `epistemic_provenance` rows where `polarity='assumes_false'`; the JSON path is not consulted at all.

> This is stronger than relying on the NULL default to keep pre-existing JSON entries inert — there is no field at all to carry the opt-in, so opt-in via JSON is impossible by construction.

## Failure handling and atomicity guarantees

| Failure | Behaviour |
|---------|-----------|
| Outbox INSERT fails | Root mutation rolls back (atomicity: no divergence between invalidation and outbox). |
| Theory creation fails | Exponential backoff retry; after `MaxRetries = 3` attempts → dead-letter + `CRITICAL` audit event. |
| Depth exceeds limit | Intent suppressed at materialisation; dead-letter + `CRITICAL` audit event. |
| Materializer crashes mid-batch | Restart recovery: `status='processing'` rows with `updated_at > 5 minutes ago` (the staleness window) are reset to `status='pending'` on the next `claimCascadeIntents`. `attempt_count` is preserved across recoveries (a poison pill that gets SIGKILL'd mid-process must accumulate retries across crashes; otherwise the dead-letter cap never trips). |
| Wake insert fails after materialisation | The `wake_scheduled` flag stays `0` on the outbox row; the reconcile pass (`ReconcileUnscheduledCascadeWakes`) re-schedules the wake on the next sweep. The theory is durable; only the wake-booking is recoverable. |

These guarantees are the post-M3 audit H-2/H-3 fixes (2026-08-31) and are regression-pinned by `internal/core/cascade_materializer_durability_test.go`.

### Idempotent dedup

The outbox carries `UNIQUE(dead_artifact_id, downstream_artifact_id, invalidation_event_id)`. The same root invalidation cannot produce duplicate intents for the same downstream within one event. `markMaterialized` is also idempotent: re-running on an already-materialised row produces the same theory (theory identity = `invalidation_event_id + downstream_artifact_id`).

## Federation (shared DB)

When `MPM_SHARED_DB` is attached, cascade intents are written to both local and shared `epistemic_cascade_outbox` tables atomically. The materializer processes the local outbox; the shared outbox is available to cross-agent shared-materializer instances. The `epistemic_provenance` federated read path surfaces citations from both DBs.

## Configuration knobs

| Knob | Default | Notes |
|------|---------|-------|
| `MaxCascadeDepth` | `3` | Storage-level ceiling on `cascade_depth`. |
| `MaxCascadeWakePerCheck` | `3` | Wake delivery cap per `check_wakes` call. |
| `MaxRetries` | `3` | Retries before dead-letter. |
| `CascadeMaterializerOptions.WakeDelay` | `1s` | Delay before scheduling cascade wake after materialisation. Set to `0` to disable. |
| `CascadeMaterializerOptions.BatchSize` | `10` | Intents claimed per `MaterializeBatch` call. |
| `CascadeDrainOptions.Budget` | `30s` | Per-tick wall-clock budget for `cascade_drain`. |
| `CascadeDrainOptions.BatchSize` | `10` | Claim size per inner-loop iteration. |
| `HardConfidenceInvalidationThreshold` | `0.3` | Confidence floor for negative-cascade trigger. |
| `HardConfidenceProvenThreshold` | `0.8` | Confidence ceiling for positive-cascade trigger. |
| `PolarityAssumesTrue` | `"assumes_true"` | Storage contract; not currently fired by any trigger surface. |
| `PolarityAssumesFalse` | `"assumes_false"` | Opt-in polarity for `foundation_proven` / `confidence_ceiling` cascades. |

All are constants in `internal/core/`; the materializer options are also settable via `NewCascadeMaterializer(dm, opts)`.

## Confirm draining is happening

`mpm-scheduler` logs a `cascade drain yielded` line on every tick where the handler runs, with one of the four `yield_reason` values above.

### "I shredded a root directive but the cascade hasn't materialised"

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

## Terminology

| Term | Meaning |
|---|---|
| foundation | The artifact whose epistemic state changed. Named `dead_artifact_id` in the outbox row even when the change is positive (foundation becomes proven). The naming reflects the historical negative-direction origin. |
| foundation invalidation | Negative direction event: a foundation becomes invalid / disproven. |
| foundation proven | Positive direction event: a foundation crosses the proven threshold. |
| foundation_proven reason | Positive-direction reason label; emitted by `EnqueueCascadeFoundationProven`. |
| downstream artifact | The artifact that cited (or depended on) the foundation and is now subject to re-evaluation. Named `downstream_artifact_id` in the outbox row. |
| dependent | Used interchangeably with "downstream artifact" in the negative-direction surface. |
| cascade intent | One row in `epistemic_cascade_outbox`. Pending → processing → materialised/failed. |
| cascade theory | The pending theory materialised from a cascade intent. Identified by `cascade=true` in metadata and a unique id. |
| re-evaluation theory | Same as "cascade theory". The naming reflects that the downstream requires conscious re-assessment, not automatic rewriting. |
| cascade wake | A wake row with `metadata.kind='cascade'`; surfaces the re-evaluation theory to the agent. |
| invalidation event | The causal event id shared by all intents produced from one trigger surface invocation. Stable across the cascade chain. |
