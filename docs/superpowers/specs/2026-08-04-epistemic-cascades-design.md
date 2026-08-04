# Epistemic Cascades Design

**Date:** 2026-08-04  
**Status:** Approved design; implementation pending  
**Scope:** Decisions and theories only

## Problem

When a foundational artifact is explicitly invalidated, downstream decisions and theories that relied on it remain structurally intact. Existing explicit theory dependencies can schedule `stale_foundation` wakes, but they do not create atomic, per-artifact re-evaluation theories, and retrieval provenance is not used to discover implicit reasoning dependencies.

## Goals and non-goals

### Goals

- Detect downstream decisions and theories affected by an explicit foundation invalidation.
- Generate exactly one pending cascade theory per downstream reasoning artifact.
- Preserve causal evidence, including `trigger_evidence_id` when present.
- Commit durable cascade intent atomically with the invalidating mutation.
- Materialize theories asynchronously with retry, restart recovery, and idempotency.
- Bound wake delivery to three cascade theories per `check_wakes` call by default.
- Bound recursive propagation with a default maximum `cascade_depth` of 3.
- Make permanent materialization failures visible through a durable dead-letter state and `CRITICAL` audit event.

### Non-goals

- Automatically challenge lessons: lessons are immutable historical observations.
- Automatically challenge global rules: global rules require explicit operator oversight.
- Trigger cascades on ordinary confidence decreases.
- Group multiple downstream artifacts into one theory.
- Change the meaning of existing explicit dependency edges.

## Trigger policy

Cascades occur only on explicit invalidation events:

- theory resolution as `disproven`;
- explicit memory shredding;
- a defined hard confidence invalidation transition below the configured threshold.

A normal confidence decrease is not sufficient. Each invalidation receives a stable `invalidation_event_id`, which is used for deduplication and causal tracing.

## Architecture

The feature adds a durable SQLite cascade outbox. Each row represents one invalidation event affecting one downstream reasoning artifact. The invalidating transaction applies the root transition, captures the evidence snapshot, discovers downstream decisions and theories, and inserts deduplicated intents. It does not create theory rows, update FTS, or schedule delivery wakes.

A bounded background materializer claims pending intents and creates one pending theory per intent. It then records `materialized_theory_id`. Successful theories enter the wake-delivery path. Materializer throughput is independent from wake-delivery throughput.

### Outbox fields

- `id`
- `invalidation_event_id`
- `dead_artifact_id`
- `dead_artifact_type`
- `downstream_artifact_id`
- `downstream_artifact_type`
- `trigger_evidence_id` (nullable)
- `cascade_depth`
- `reason`
- `status`: `pending`, `processing`, `materialized`, or `failed`
- `materialized_theory_id` (nullable)
- retry count, next retry time, terminal error, and created/updated timestamps

The uniqueness key is `(dead_artifact_id, downstream_artifact_id, invalidation_event_id)`.

## Dependency discovery

Two edge sources are combined:

1. `memories.dependencies` supplies explicit forward dependencies, especially theory-to-foundation relationships.
2. Retrieval observability provenance supplies citations represented by `source_ids`, allowing decisions and theories to be challenged when they used a foundation in context even without an explicit dependency declaration.

Only downstream artifacts of type `decision` or `theory` are eligible. Lessons and global rules are excluded. Discovery must preserve typed IDs where available while remaining compatible with existing untyped citations.

## Materialized theory contract

Each generated theory is one-to-one with a downstream artifact and contains:

### Metadata

- `cascade: true`
- `cascade_version: 1`
- `dead_artifact_id`
- `dead_artifact_type`
- `downstream_artifact_id`
- `downstream_artifact_type`
- `trigger_evidence_id` when available
- `cascade_depth`
- `generated_at`

The dead artifact is also included in the generated theory's `dependencies` list. Metadata explains why the theory exists; dependencies identify what it relies on; validation criteria define how it is closed.

### Content and validation

The hypothesis is a concise human-readable statement that the downstream artifact requires re-evaluation because its cited foundation collapsed. Validation criteria require independent review of whether the downstream artifact remains valid without that foundation, followed by binary resolution as proven or disproven.

## Delivery and recursion

`check_wakes` applies a cascade-specific delivery cap of **3 theories per check** by default. Undelivered wakes remain pending for later checks.

Depth semantics:

- depth 0: original invalidation;
- depth 1: direct downstream cascade theory;
- depth 2: cascade caused by later invalidation of a depth-1 theory;
- depth 3: final permitted recursive cascade.

Further cascades are suppressed and emit a `CRITICAL` audit event. Deduplication prevents repeated observations of the same invalidation from bypassing the guard. Configuration may change the limit, but 3 is the default.

## Failure handling

Outbox insertion failure aborts the invalidating transaction so root state and cascade intent cannot diverge. Materializer failures use bounded retries and backoff. After the retry limit, the intent becomes a durable dead-letter row and emits a `CRITICAL` audit entry containing the intent ID, source/downstream IDs, retry count, and terminal error. Restart recovery resumes pending or reclaimable processing rows. Theory creation and outbox updates are idempotent.

## Verification matrix

Tests must cover:

- one foundation affecting multiple decisions and theories;
- exclusion of lessons and global rules;
- explicit dependency and provenance-derived edges;
- duplicate invalidations and idempotent retries;
- restart/recovery behavior;
- dead-letter audit records;
- depth-3 cutoff and suppression audit;
- `trigger_evidence_id` propagation;
- wake pagination independent of materializer volume;
- ordinary confidence decreases not triggering cascades;
- transaction rollback when outbox insertion fails.

## Open implementation constraints

Use the existing single `DatabaseManager` connection and SQLite transaction patterns. Keep the invalidation transaction lightweight. Reuse existing theory creation, scanner, FTS, evidence snapshot, wake, and audit APIs rather than introducing alternate write paths.
