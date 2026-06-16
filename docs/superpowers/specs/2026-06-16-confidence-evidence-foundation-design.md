# Confidence and Evidence Foundation — Design

**Date:** 2026-06-16
**Status:** Draft (pending user review)
**Author:** Design discussion with user
**Supersedes:** None — first spec for this architecture

## Summary

Introduce an explicit confidence layer to MPM that is independent of artifact content. Decouple the current `weight` field into separate `retrieval_priority` and `importance` axes, add a derived `confidence` field that estimates truth based on a typed evidence table, and preserve historical states so that confidence updates never rewrite the past. This is the foundation for calibration tracking, challenge refactor, hindsight annotations, and the explicit forgetting log — all of which depend on the schema changes in this spec.

## Goals

- Make confidence an independently-modifiable property of an artifact, not a side-effect of weight
- Make `weight = truth` impossible to encode by retiring the field and exposing a compatibility view during migration
- Make evidence a typed, time-bounded, source-tracked first-class entity
- Make confidence structurally impossible to set manually (generated column)
- Preserve historical state so confidence updates never erase the past
- Enable future work: calibration tracking, challenge refactor, hindsight annotations, forgetting log

## Non-Goals

- Calibration tracking (separate spec, depends on this one)
- Challenge object + graph edges refactor (separate spec)
- Theory DAG with `refines / contradicts / supersedes` (separate spec)
- Hindsight annotations (separate spec)
- Rejection log (separate spec)
- Data migration of existing memories/theories/decisions (this spec defines the new schema; a follow-up defines how existing rows get initial values)
- ISR / Epistemic Debt telemetry (uses confidence but doesn't define it)

## Context

MPM's current `weight` field secretly encodes four concepts: importance, truth, retrieval priority, and decay resistance. The system has no concept of evidence; beliefs are inferred from `reinforcement_count` and `mpm +/-` operations. The result is a schema that cannot distinguish "this memory is true" from "this memory is often retrieved" — the four-concepts-in-one-field problem.

The four-concepts problem is a structural limitation. Every "wouldn't it be nice if..." feature (calibration, evidence weighting, source independence, hindsight annotations) is blocked on it. This spec unblocks them by replacing the conflated schema with explicit, orthogonal axes.

## Design Principle: Knowledge and Confidence Are Independent

The architecture treats knowledge (the content of a memory, theory, decision) and confidence (the system's current belief in that knowledge) as orthogonal axes:

- **Knowledge and confidence are independent.**
- **Confidence may change without modifying knowledge.** Adding contradicting evidence changes confidence but leaves the artifact content untouched.
- **Knowledge may change without modifying confidence.** A hindsight annotation changes what the artifact says but should not retroactively alter the confidence that the *original* statement was believed.
- **Historical states are preserved.** A confidence update is a new snapshot, not an in-place edit. The artifact's past confidence values are queryable.

Every schema decision, every evidence type, every decay function in this spec is derived from these four points.

> **Confidence is the system's *current* estimate of truth derived from *current* evidence. The artifact is historical fact. These are independent: confidence is a snapshot, the artifact is a record.**

## Core Decisions

### 1. Retire `weight` as a first-class field

Current:

```
weight  -- secretly encodes importance + truth + retrieval + decay-resistance
```

Future:

```
retrieval_priority   -- how often this should surface in retrieval
importance           -- how much does this matter to the project/domain
confidence           -- derived from current evidence (GENERATED column)
```

Migration: `legacy_weight = max(0.01, (retrieval_priority + importance) / 2)` exposed as a compatibility view during v2. The function is monotonic and conservative (never returns 0) so old commands behave predictably. In v3, the compatibility view is removed entirely.

**Why retire, not deprecate:** deprecating leaves the field alive. If `weight` remains a real column, agents and humans will keep writing to it, and the conflation returns. The schema should make the wrong thing difficult.

### 2. Exponential confidence decay with per-collection λ

```
confidence(t) = base_confidence × e^(-λ × t_since_last_positive_evidence)
```

where λ varies by collection. Decay operates on the time since the last *positive* evidence (i.e., the last time new evidence arrived that supported the artifact, not the last time it was retrieved).

Initial λ values (provisional, to be tuned against real data):

| Collection | λ | Half-life | Notes |
|---|---|---|---|
| `decisions` | 0.001 | ~693 days | Architectural decisions decay slowly |
| `lessons` | 0.003 | ~231 days | Best practices, warnings, patterns |
| `memories` (general) | 0.01 | ~69 days | Default for unclassified memories |
| `theories` | 0.02 | ~35 days | Hypotheses decay faster — encourage resolution |
| `current_events` | 0.05 | ~14 days | Time-sensitive facts (the model may not have this collection yet) |

**Why exponential, not linear:** linear decay assumes uniform uncertainty accumulation, which doesn't match how knowledge actually ages. Confidence stays high for a while, then drops faster as time passes without reinforcement. Exponential models this.

**Hard cutoff for forgetting:** pure exponential decay never reaches zero, so memories accumulate an infinite tail. The forgetting log (future spec) uses a hard cutoff of `confidence < 0.05` as its entry condition. The decay function is therefore exponential up to the cutoff, then explicit forgetting takes over. The cutoff threshold is a parameter, not a constant — the spec ships with 0.05, the implementation can tune it.

### 3. Evidence is a typed first-class entity

Evidence is no longer inferred from reinforcement counts. It's a real table, with types, source groups, strengths, and time bounds. The six v1 types are listed below.

## Schema

### Existing tables (memories / theories / decisions / lessons)

For v1 of this foundation, the artifact table is a logical view over the existing four tables. Each existing table gains three new columns: `retrieval_priority`, `importance`, and `confidence` (the last as a generated column). The unified `artifacts` view is a `UNION ALL` of the four tables with the new columns.

This avoids a destructive schema migration. A future v2 may unify the four tables into a single `artifacts` table with a `type` discriminator — but that's a separate, larger migration with its own trade-offs.

```sql
ALTER TABLE memories  ADD COLUMN retrieval_priority REAL NOT NULL DEFAULT 0.5;
ALTER TABLE memories  ADD COLUMN importance         REAL NOT NULL DEFAULT 0.5;
ALTER TABLE memories  ADD COLUMN confidence         REAL GENERATED ALWAYS AS (
    -- computed from current evidence; see Confidence Calculation below
    confidence_from_evidence(memories.id, 'memory')
) STORED;

-- (same ALTER statements for theories, decisions, lessons)
```

The generated column expression calls a function (`confidence_from_evidence`) that reads the current evidence set for the artifact and returns the computed confidence. SQLite recomputes the column on every evidence insert / update / expire.

### `evidence` table (new)

```sql
CREATE TABLE evidence (
    id                  TEXT PRIMARY KEY,        -- ULID
    artifact_id         TEXT NOT NULL,           -- FK to (memories|theories|decisions|lessons).id
    artifact_type       TEXT NOT NULL,           -- 'memory' | 'theory' | 'decision' | 'lesson'
    type                TEXT NOT NULL,           -- see Evidence Types below
    source_group        TEXT NOT NULL,           -- groups correlated evidence (e.g., "log-server-01")
    strength            REAL NOT NULL,           -- raw strength in [-1, 1] (negative for challenge)
    independence_factor REAL NOT NULL DEFAULT 1.0,
    created_at          INTEGER NOT NULL,        -- unix seconds
    expires_at          INTEGER,                 -- NULL = no expiry
    notes               TEXT                     -- human-readable context
);

CREATE INDEX idx_evidence_artifact ON evidence(artifact_id, artifact_type);
CREATE INDEX idx_evidence_type     ON evidence(type);
CREATE INDEX idx_evidence_source   ON evidence(source_group);
CREATE INDEX idx_evidence_expires  ON evidence(expires_at);
```

### `confidence_history` table (new)

```sql
CREATE TABLE confidence_history (
    id              TEXT PRIMARY KEY,        -- ULID
    artifact_id     TEXT NOT NULL,
    artifact_type   TEXT NOT NULL,
    confidence      REAL NOT NULL,
    computed_at     INTEGER NOT NULL,        -- unix seconds
    evidence_count  INTEGER NOT NULL,        -- evidence rows used in this calculation
    trigger         TEXT NOT NULL            -- 'evidence_added' | 'evidence_expired' | 'decay_tick' | 'manual_recompute'
);

CREATE INDEX idx_conf_history_artifact ON confidence_history(artifact_id, artifact_type, computed_at);
```

This is the historical state preservation from the design principle. The `confidence` field on the artifact is the *current* value; the `confidence_history` table preserves every prior value. A confidence update inserts a new row here; it never overwrites.

### `artifacts` view (new)

```sql
CREATE VIEW artifacts AS
  SELECT 'memory'   AS type, id, retrieval_priority, importance, confidence, created_at FROM memories
  UNION ALL
  SELECT 'theory'   AS type, id, retrieval_priority, importance, confidence, created_at FROM theories
  UNION ALL
  SELECT 'decision' AS type, id, retrieval_priority, importance, confidence, created_at FROM decisions
  UNION ALL
  SELECT 'lesson'   AS type, id, retrieval_priority, importance, confidence, created_at FROM lessons;
```

Future queries that need to reason about "all artifacts uniformly" use this view.

## The Six Evidence Types (v1)

Start embarrassingly simple. Add more types only when the existing ones prove insufficient.

| Type | Default strength | Independence semantics |
|---|---|---|
| `observation` | 0.4 | Weak-to-medium. "I saw this happen." |
| `test` | 0.7 | Controlled evidence. Unit test passed. |
| `reproduction` | 0.85 | Very strong. Bug reproduced N times. |
| `challenge` | -0.6 | Negative evidence. Contradictory. |
| `decision_outcome` | 0.95 | Reality grading your work. Decision succeeded or failed. |
| `external_reference` | 0.6 | Reference-backed. RFC, official docs, paper. |

Strengths are starting points; the implementation allows per-evidence override. The signs are critical: `challenge` has negative strength, directly modeling "evidence against this artifact."

A type registry (in `internal/evidence.go`) is the single source of truth for default strengths, validation, and any future type-specific logic. Adding a seventh type means adding a row to the registry, not editing multiple files.

## Independence Factor

`source_group` clusters evidence by origin. The scoring function multiplies strength by an independence factor:

| Relationship | Factor | Example |
|---|---|---|
| Correlated | 0.3 | 50 logs from the same machine |
| Partially-independent | 0.5 | Logs from 5 machines in the same cluster |
| Independent | 1.0 | Logs from unrelated environments |
| Orthogonal | 1.5 | Independent test, independent codebase, independent reviewer |

Effective evidence contribution: `effective = strength × independence_factor × recency_weight`.

`source_group` is the only knob needed for this — the implementation can infer or suggest an `independence_factor` from how many distinct groups have contributed, but the field is explicit so humans can override.

## Confidence Calculation

Confidence is a **generated column** in SQLite, not a user-editable field. This is the structural enforcement of "confidence is not stored truth" — there is no SQL path to set it directly.

```
confidence(artifact) = sigmoid( log_odds(artifact) )

log_odds(artifact) = log( base_odds(artifact) )
                   + Σ effective_evidence(artifact)
                   - decay(artifact)

effective_evidence(e) = strength(e) × independence_factor(e) × recency_weight(e)

recency_weight(e) = e^(-μ × age_in_days(e))   -- μ is global, small (~0.005)

decay(artifact) = λ(collection) × t_since_last_positive_evidence(artifact)
```

The base odds and sigmoid bound confidence to (0, 1). Negative `challenge` evidence subtracts from log-odds, naturally reducing confidence. Per-collection λ enforces the decay rates from the table above. Recency weight gives recent evidence more weight than stale evidence (independent of the collection-level decay).

**Implementation:** the generated column expression calls a registered SQL function (`confidence_from_evidence`) that reads the current evidence set and returns the computed confidence. SQLite recomputes the column on every evidence insert / update / expire. The historical state is recorded in `confidence_history` via a trigger:

```sql
CREATE TRIGGER trg_evidence_added
AFTER INSERT ON evidence
BEGIN
    INSERT INTO confidence_history (id, artifact_id, artifact_type, confidence, computed_at, evidence_count, trigger)
    VALUES (
        gen_ulid(),
        NEW.artifact_id,
        NEW.artifact_type,
        confidence_from_evidence(NEW.artifact_id, NEW.artifact_type),
        unixepoch(),
        (SELECT count(*) FROM evidence WHERE artifact_id = NEW.artifact_id AND artifact_type = NEW.artifact_type),
        'evidence_added'
    );
END;
```

(Similar triggers for `evidence_expired`, `decay_tick` scheduled from `idle_dream`, and `manual_recompute` for diagnostic use.)

**Why generated, not application-level:** application-level derivation can be bypassed by direct SQL writes. Generated columns cannot. This is the same principle as "schema should make the wrong thing difficult."

## Historical State Preservation

Every confidence change produces a row in `confidence_history`. The current `confidence` is the *latest* row for the artifact; prior rows are immutable. This is how "historical states are preserved" is enforced.

Queries:

```sql
-- Current confidence (matches the generated column)
SELECT confidence FROM artifacts WHERE id = ?;

-- Confidence as of a specific time
SELECT confidence
FROM confidence_history
WHERE artifact_id = ? AND artifact_type = ? AND computed_at <= ?
ORDER BY computed_at DESC
LIMIT 1;

-- Full confidence timeline for an artifact
SELECT computed_at, confidence, evidence_count, trigger
FROM confidence_history
WHERE artifact_id = ? AND artifact_type = ?
ORDER BY computed_at;
```

## Migration Strategy

### v2: Add the new fields and table, deprecate `weight`

1. Add `retrieval_priority`, `importance` columns to `memories` / `theories` / `decisions` / `lessons` (default 0.5)
2. Add `confidence` as a generated column to the same four tables, calling `confidence_from_evidence`
3. Create the `evidence` table
4. Create the `confidence_history` table
5. Create the `artifacts` view (`UNION ALL` over the four tables)
6. Add a `legacy_weight` view that computes the old value: `legacy_weight = max(0.01, (retrieval_priority + importance) / 2)`
7. All existing commands continue to read from `weight`; new commands read from the new fields
8. Mark `weight` as deprecated in code comments and the config doc

### v3: Remove `weight`

1. After v2 has been live long enough that all consumers have migrated
2. Drop the `weight` column from the four tables
3. Drop the `legacy_weight` view
4. Update all commands to use the new fields

The "long enough" criterion is tracked separately; the v3 cutover is its own decision. The spec does not pin down the v2 → v3 timeline.

## File-by-File Change List (for implementation plan)

| File | Change |
|---|---|
| `internal/schema.go` | Add new columns to `BaseTables`; add `evidence` and `confidence_history` table definitions |
| `internal/evidence.go` (new) | Evidence type registry, default strengths, validation |
| `internal/confidence.go` (new) | Confidence calculation: log-odds, decay, recency weight, generated column expression function |
| `internal/db.go` | Add the new tables to init; add the `confidence_from_evidence` SQL function registration; add the history triggers |
| `internal/memory.go` | Update `Memory` struct with new fields; update `AddMemory` to set initial `retrieval_priority` and `importance` (default 0.5) |
| `internal/theory.go` (or wherever theories live) | Update `Theory` struct with new fields |
| `internal/decision.go` (or wherever) | Update `Decision` struct with new fields |
| `internal/lesson.go` (or wherever) | Update `Lesson` struct with new fields |
| `cmd/mpm/handlers*.go` | Update commands that read `weight` to read `retrieval_priority` + `importance` |
| `cmd/mpm/handlers*.go` | New commands: `mpm evidence add/list`, `mpm ops confidence` |
| `cmd/mpm/call.go` | Add `add_evidence` and `query_confidence_history` to `toolRegistry` |
| Tests | New test files for evidence, confidence calculation, historical state queries, decay function, independence factor scoring |

## Trade-offs and Open Questions

### Trade-offs accepted

- **Application-level confidence derivation is now schema-enforced.** This is the right call for an epistemology engine; the alternative (convention-based derivation) always erodes.
- **Historical state requires a write on every confidence change.** A 10x increase in writes to the artifact table. This is acceptable for v1; if it becomes a hot path, batch the history writes in the `idle_dream` worker.
- **The `evidence` table will grow large over time.** It's not pruned by default. The forgetting log (future spec) prunes it alongside the artifact.
- **Generated columns in SQLite cannot reference user-defined functions in all versions.** The implementation must verify that the deployed SQLite supports this; if not, the function is registered as a built-in extension.

### Open questions for v1

- **Initial `confidence` for new artifacts:** what value? A default of 0.5 (the neutral point) is reasonable. New artifacts start uncertain until evidence arrives.
- **Evidence type for `inference`:** the model often infers things from other knowledge without direct observation. Is `observation` (with low strength) the right home, or do we need a seventh type? Defer to v2.
- **Recency weight μ:** the global `μ` for the per-evidence recency decay isn't pinned down. Defer to a v1.1 tuning pass.
- **Confidence interval:** the spec gives a point estimate. Do we also need a confidence *interval* (variance)? Calibration needs it. Defer to the calibration spec.
- **Per-collection confidence override:** can an artifact have a `confidence_override` field for human-judgment cases (e.g., "I know this is true regardless of evidence")? Defer; current spec treats all confidence as derived.

## What's Missing From This Spec

- Calibration tracking — separate spec
- Challenge refactor (object + edges) — separate spec
- Theory DAG relations — separate spec
- Hindsight annotations — separate spec
- Forgetting log (uses the `confidence < 0.05` cutoff defined here) — separate spec
- Per-bucket calibration reporting — separate spec
- Rejection log — separate spec

Each of these is enabled by this spec's schema but is its own design problem.
