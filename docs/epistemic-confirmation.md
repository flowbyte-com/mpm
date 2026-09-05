# Epistemic Confirmation — Operator Guide

**Feature added:** 2026-09-05
**MPM version:** post-`1ccc33c` (the canonical behavioral-contract commit)

## Overview

Epistemic cascades fire on **invalidation**: a foundational artifact is disproven, shredded, or crosses the hard-confidence threshold, and downstream dependents get re-evaluation theories. There is no mirror for **confirmation**: when reality validates a lesson's prediction, no signal flows back. The lesson's confidence decays under the same rule as a lesson nobody's thought about in months that may no longer be true.

This doc specifies the **confirmation hook** — a minimal addition that gives `log_to_changelog` an optional structured assertion that, when present, fires `mpm_evidence.add` against the asserted artifact as part of the same call. It is the **other half of epistemic cascades** — upstream reinforcement rather than downstream invalidation fan-out.

## Why a hook, not a new mechanism

The existing infrastructure already supports confirmation:

- **5 of 6 evidence types are positive-strength** (`observation` +0.4, `test` +0.7, `reproduction` +0.85, `decision_outcome` +0.95, `external_reference` +0.6; only `challenge` is negative at −0.6) — `internal/core/evidence.go:20-27`.
- **`mpm_evidence.add` accepts `artifact_type="lesson"`** — the schema enum, CHECK constraint, recompute path, and confidence math all handle it. No new vocabulary needed.
- **`RecomputeConfidence` is universal** — `confidence.go:119-138` is artifact-type-driven via `InitialConfidence` and `decayLambda` only; lessons get `0.7` initial and `0.003`/day decay (slowest of the four canonical artifact types).
- **The cascade invalidation hook will not cross-fire** — `evidence_store.go:328-340` only enqueues cascade intents when confidence crosses below `HardConfidenceInvalidationThreshold (0.3)` on the recompute. Positive-strength evidence on a 0.7-default lesson can never drop it below 0.3, so confirmation writes do not trigger any downstream re-evaluation cascade.

The gap is **workflow wiring**, not confidence-engine work. A new paradigm would re-implement machinery the existing math already does correctly.

## What confirmation produces

A confirmation event creates **one evidence row** with:

- `artifact_id` = the asserted lesson / decision / theory id
- `artifact_type` = `lesson` / `decision` / `theory`
- `type` = `reproduction` (default — fits "this prediction came true again"; the design-doc-asserted narrowness below explains why we don't expose the type as caller choice)
- `source_group` = `git` (the confirmation is a git-commit-anchored event)
- `strength` = `0.85` (the registry default for `reproduction`)
- `independence_factor` = `1.0` (default)
- `created_by` = `"log_to_changelog:<commit_hash>"`
- `notes` = `"confirmed by changelog entry <commit_hash>"`

`RecomputeConfidence` then runs synchronously: it loads the artifact's evidence set, recomputes confidence via `computeConfidence`, writes the new confidence column, appends a `confidence_history` row with `trigger='evidence_added'`. The artifact's confidence rises (or stays high if already reinforced). Nothing else fires.

## Trigger conditions

A confirmation hook fires **only** when all of the following are explicit:

1. The caller invokes `log_to_changelog` (the canonical commit-anchored write path).
2. The caller passes one or more `confirms_*_id` parameters naming the artifact(s) they assert are validated by this commit.
3. Each named artifact exists, is of the asserted type, and is **not** in a terminal state (no `is_challenged`, not soft-deleted).

The mechanism is **opt-in and explicit-only**:

- No keyword matching between commit messages and lesson content.
- No semantic inference from "this commit fixes bug X" → "the FTS5 lesson applies."
- The standard is the same one MPM already holds for `source_ids` on `mpm_decisions.record` and `mpm_theories.propose` — whoever records the artifact asserts the connection explicitly, with a known-id parameter.

## CLI surface

### `mpm call log_to_changelog`

Existing payload gains three optional parameters:

```json
{
  "action": "log_to_changelog",
  "params": {
    "fact": "...",
    "commit_hash": "abc...40chars",
    "confirms_lesson_id": ["<lesson_id>", ...],
    "confirms_decision_id": ["<decision_id>", ...],
    "confirms_theory_id": ["<theory_id>", ...]
  }
}
```

| Param | Type | Required | Description |
|-------|------|----------|-------------|
| `confirms_lesson_id` | string or array of strings | no | Lesson ids whose predictions this commit validates |
| `confirms_decision_id` | string or array of strings | no | Decision ids whose rationale this commit validates |
| `confirms_theory_id` | string or array of strings | no | Theory ids whose hypothesis this commit validates |

When present, each named id triggers one `AddEvidence` call (synchronously, inside the same `WithTx` wrapper that holds the changelog memory write). On confirmation error, the entire `log_to_changelog` call rolls back — atomicity guarantees the changelog memory and the evidence rows either all land or none do.

## Schema changes

**None.** This design reuses the existing `evidence` table, the existing `confidence_history` CHECK constraint (already widened to 9 values in `6514a42`), and the existing `RecomputeConfidence` machinery. No new trigger types. No new tables. No new columns.

## Configuration knobs

**None.** Confirmation uses the registry default strength (`reproduction` = +0.85) and the registry default independence (`1.0`). There is no operator knob because the mechanism has one job — record an explicit assertion — and exposes no tunable behavior.

If a future need arises for caller-strength override or for finer-grained trigger taxonomy, those are separate design arcs and would land in their own docs.

## Failure handling

| Failure | Behavior |
|---------|----------|
| Named artifact does not exist | `log_to_changelog` returns an error naming the unknown id; the entire transaction rolls back; no changelog memory or evidence rows land |
| Named artifact is of the wrong type (e.g. `confirms_lesson_id` names a decision id) | error before any write; transaction rolls back |
| Named artifact is in a terminal state (challenged, soft-deleted) | error before any write; transaction rolls back |
| `AddEvidence` fails partway through (e.g. scanner rejects `notes`) | transaction rolls back; no partial state |
| Empty `confirms_*_id` array | no-op for that array (the other arrays still process); not an error |
| Mixed array of valid + invalid ids | error on first invalid; transaction rolls back |

## Operational notes — distinguish this from keyword inference

Confirmation is **explicit-only**. Operators reviewing substrate state should be able to trust that an evidence row with `created_by="log_to_changelog:<commit_hash>"` reflects an explicit assertion by whoever wrote that changelog entry. If you find one that doesn't, that's a documentation/UX gap (caller didn't know the option existed) — fix the caller, not the substrate.

Things this mechanism will **not** do, by design:

- Match `[commit: <hash>]` text in a lesson body to `commit:<hash>` tags on changelog memories and auto-reinforce. The synthesis engine's existing text-pattern matching handles that surface — confirmation is a different signal.
- Boost confidence based on semantic similarity between a commit message and lesson content.
- Reach across hosts or workspaces — confirmation is local to the substrate that holds the assertion.

## Cross-reference

- **Cascade doc**: `docs/archive/epistemic-cascades.md` — read together with this doc to understand the full bidirectional epistemic lifecycle.
- **Behavioral contract**: `docs/tool-behavioral-contract.md` §4 — confirmation does not change the idempotency class of `log_to_changelog` (the changelog memory write is still not idempotent across distinct commits; each confirmation event is a fresh evidence row).
- **Hard confidence threshold**: 0.3 (cascade doc + `evidence_store.go:330`) — confirmation cannot cross this floor, so the cascade enqueue is unreachable from this path.
- **Evidence type registry**: `internal/core/evidence.go:20-27` — single source of truth for v1 types and strengths.