# Epistemic Confirmation & Contradiction — Operator Guide

**Feature added:** 2026-09-05 (confirmation); 2026-09-05 (contradiction)
**MPM version:** post-`1ccc33c` (the canonical behavioral-contract commit)

## Overview

Epistemic cascades fire on **invalidation**: a foundational artifact is disproven, shredded, or crosses the hard-confidence threshold, and downstream dependents get re-evaluation theories. There was no mirror for **confirmation** (reality validates a lesson's prediction) or **contradiction** (reality shows the lesson is wrong). Both directions existed in the evidence-type registry but nothing gave them the same easy, explicit, structured call site.

This doc specifies the **explicit-assertion hook** — a minimal addition that gives `log_to_changelog` three optional structured assertion params per direction (lesson / decision / theory), each routing through the appropriate evidence type. Together, confirmation + contradiction are the **other half of epistemic cascades** — upstream reinforcement and explicit invalidation, rather than downstream automatic invalidation fan-out.

The two directions are deliberately NOT symmetric at the cascade-invalidation hook:

- **Confirmation** (positive evidence, `reproduction` strength +0.85) cannot drop confidence below the 0.3 cascade floor from any 0.7+ baseline. The cascade hook is unreachable from this path.
- **Contradiction** (negative evidence, `challenge` strength −0.6) lowers confidence toward the floor; a strong contradiction (multiple challenge rows, or an override-strength challenge) can cross the floor and legitimately enqueue cascade intents for downstream dependents. The cascade hook IS reachable from this path by design.

## Why a hook, not a new mechanism

The existing infrastructure already supports both directions:

- **5 of 6 evidence types are positive-strength** (`observation` +0.4, `test` +0.7, `reproduction` +0.85, `decision_outcome` +0.95, `external_reference` +0.6) and **1 is negative-strength** (`challenge` −0.6) — `internal/core/evidence.go:20-27`. The `challenge` type's help text: *"negative evidence — flags the artifact as contradicted; moves verified → contradicted"* — which fits both "this turned out to be wrong" and "this is being challenged/disputed."
- **`mpm_evidence.add` accepts `artifact_type="lesson"`** — the schema enum, CHECK constraint, recompute path, and confidence math all handle it. No new vocabulary needed.
- **`RecomputeConfidence` is universal** — `confidence.go:119-138` is artifact-type-driven via `InitialConfidence` and `decayLambda` only; lessons get `0.7` initial and `0.003`/day decay (slowest of the four canonical artifact types).
- **The cascade invalidation hook handles both directions correctly** — `evidence_store.go:328-340` enqueues cascade intents only on threshold-crossing transitions (`oldConf >= HardConfidenceInvalidationThreshold AND newConf < HardConfidenceInvalidationThreshold`). For confirmation, this is unreachable from any 0.7+ baseline. For contradiction, this is reachable by design — a strong contradiction (multiple challenge rows or override-strength) can cross the floor and legitimately enqueue cascade intents for downstream dependents.

The gap was **workflow wiring**, not confidence-engine work. A new paradigm would re-implement machinery the existing math already does correctly.

## What each direction produces

A **confirmation** event creates one evidence row with:

- `artifact_id` = the asserted lesson / decision / theory id
- `artifact_type` = `lesson` / `decision` / `theory`
- `type` = `reproduction` (default strength +0.85; fits "this prediction came true again")
- `source_group` = `git` (the confirmation is a git-commit-anchored event)
- `independence_factor` = `1.0` (default)
- `created_by` = `"log_to_changelog:<commit_hash>"`
- `notes` = `"confirmed by changelog entry <commit_hash>"`

A **contradiction** event creates one evidence row with:

- `artifact_id` / `artifact_type` = same as confirmation
- `type` = `challenge` (default strength −0.6; fits "this artifact is contradicted")
- `source_group` = `git`
- `independence_factor` = `1.0`
- `created_by` = `"log_to_changelog:<commit_hash>"`
- `notes` = `"contradicted by changelog entry <commit_hash>"`

`RecomputeConfidence` then runs synchronously: it loads the artifact's evidence set, recomputes confidence via `computeConfidence`, writes the new confidence column, appends a `confidence_history` row with `trigger='evidence_added'`. For confirmation, the confidence rises (or stays high if already reinforced). For contradiction, the confidence falls — and if it crosses below `HardConfidenceInvalidationThreshold (0.3)`, the cascade invalidation hook at `evidence_store.go:328-340` enqueues one cascade intent per downstream dependent (decision or theory citing the artifact via `dependencies` JSON or `epistemic_provenance` rows).

## Cascade cross-fire asymmetry

The two directions behave differently at the cascade hook:

| Direction | Default strength | Effect on confidence | Can cross cascade floor? | Cascade fires? |
|-----------|------------------|----------------------|--------------------------|----------------|
| Confirmation | +0.85 | Rises | **No** — from any 0.7+ baseline, +0.85 cannot drop confidence below 0.3 | **No** (by construction) |
| Contradiction | −0.6 | Falls | **Yes** — 3 default-strength challenges cumulatively drop log-odds by −1.8, crossing the floor | **Yes** (when threshold crossed AND downstream dependents exist) |

This asymmetry is **deliberate, not a bug**. Confirmation is upstream reinforcement: nothing downstream needs to react. Contradiction is explicit invalidation: the user is asserting "this artifact is wrong," and downstream dependents SHOULD react — that's what the cascade machinery is for.

We do NOT suppress the contradiction path's cascade enqueue. Doing so would silently hide the user's intent when they assert "this artifact is wrong," which would be a far worse failure mode than an occasional false-positive cascade (which the existing dead-letter handling at `docs/archive/epistemic-cascades.md` already addresses).

## Trigger conditions

An assertion hook fires **only** when all of the following are explicit:

1. The caller invokes `log_to_changelog` (the canonical commit-anchored write path).
2. The caller passes one or more `confirms_*_id` parameters (positive direction), or one or more `contradicts_*_id` parameters (negative direction), or both.
3. Each named artifact exists, is of the asserted type, and is **not** in a terminal state (no `is_challenged`, not soft-deleted).

The mechanism is **opt-in and explicit-only**:

- No keyword matching between commit messages and lesson content.
- No semantic inference from "this commit fixes bug X" → "the FTS5 lesson applies."
- The standard is the same one MPM already holds for `source_ids` on `mpm_decisions.record` and `mpm_theories.propose` — whoever records the artifact asserts the connection explicitly, with a known-id parameter.

## CLI surface

### `mpm call log_to_changelog`

Existing payload gains six optional parameters (three per direction):

```json
{
  "action": "log_to_changelog",
  "fact": "...",
  "commit_hash": "abc...40chars",
  "confirms_lesson_id":   ["<lesson_id>",   ...],
  "confirms_decision_id": ["<decision_id>", ...],
  "confirms_theory_id":   ["<theory_id>",   ...],
  "contradicts_lesson_id":   ["<lesson_id>",   ...],
  "contradicts_decision_id": ["<decision_id>", ...],
  "contradicts_theory_id":   ["<theory_id>",   ...]
}
```

(Note: `log_to_changelog` is a top-level tool, not an action-based tool — `fact` and `commit_hash` live at the top level of the payload, not under a `params` envelope. This matches the existing schema at `internal/core/tools/registry_list.go:482`.)

| Param | Type | Required | Description |
|-------|------|----------|-------------|
| `confirms_lesson_id` | string or array | no | Lesson ids whose predictions this commit validates |
| `confirms_decision_id` | string or array | no | Decision ids whose rationale this commit validates |
| `confirms_theory_id` | string or array | no | Theory ids whose hypothesis this commit validates |
| `contradicts_lesson_id` | string or array | no | Lesson ids this commit shows to be wrong |
| `contradicts_decision_id` | string or array | no | Decision ids this commit shows to be invalidated |
| `contradicts_theory_id` | string or array | no | Theory ids this commit shows to be disproven |

When any are present, each named id triggers one `AddEvidence` call (synchronously, inside the same `WithTx` wrapper that holds the changelog memory write). Both directions can coexist in one call. On any validation error, the entire `log_to_changelog` call rolls back — atomicity guarantees the changelog memory and the evidence rows either all land or none do.

## Schema changes

**None.** This design reuses the existing `evidence` table, the existing `confidence_history` CHECK constraint (already widened to 9 values in `6514a42`), and the existing `RecomputeConfidence` machinery. No new trigger types. No new tables. No new columns.

## Configuration knobs

**None.** Both directions use the registry defaults (`reproduction` = +0.85 for confirmation, `challenge` = −0.6 for contradiction) and the registry default independence (`1.0`). There is no operator knob because the mechanism has one job — record an explicit assertion — and exposes no tunable behavior.

If a future need arises for caller-strength override or for finer-grained trigger taxonomy, those are separate design arcs and would land in their own docs.

## Failure handling

| Failure | Behavior |
|---------|----------|
| Named artifact does not exist | `log_to_changelog` returns an error naming the unknown id; the entire transaction rolls back; no changelog memory or evidence rows land |
| Named artifact is of the wrong type (e.g. `confirms_lesson_id` names a decision id) | error before any write; transaction rolls back |
| Named artifact is in a terminal state (challenged, soft-deleted) | error before any write; transaction rolls back |
| `AddEvidence` fails partway through (e.g. scanner rejects `notes`) | transaction rolls back; no partial state |
| Empty `confirms_*_id` / `contradicts_*_id` array | no-op for that array (the other arrays still process); not an error |
| Mixed array of valid + invalid ids | error on first invalid; transaction rolls back |
| Mixed-direction batch (some confirms + some contradicts) | both directions process in a single transaction; same atomicity rules apply |

## Operational notes — distinguish this from keyword inference

Confirmation and contradiction are both **explicit-only**. Operators reviewing substrate state should be able to trust that an evidence row with `created_by="log_to_changelog:<commit_hash>"` reflects an explicit assertion by whoever wrote that changelog entry. If you find one that doesn't, that's a documentation/UX gap (caller didn't know the option existed) — fix the caller, not the substrate.

Things this mechanism will **not** do, by design:

- Match `[commit: <hash>]` text in a lesson body to `commit:<hash>` tags on changelog memories and auto-reinforce. The synthesis engine's existing text-pattern matching handles that surface — confirmation/contradiction is a different signal.
- Boost or lower confidence based on semantic similarity between a commit message and lesson content.
- Reach across hosts or workspaces — assertions are local to the substrate that holds them.
- Suppress cascade firing on contradiction. The cascade is the legitimate downstream reaction to an explicit invalidation assertion; suppressing it would silently hide the user's intent.

## Cross-reference

- **Cascade doc**: `docs/archive/epistemic-cascades.md` — read together with this doc to understand the full bidirectional epistemic lifecycle.
- **Behavioral contract**: `docs/tool-behavioral-contract.md` §4 — confirmation and contradiction do not change the idempotency class of `log_to_changelog` (the changelog memory write is still not idempotent across distinct commits; each assertion event is a fresh evidence row).
- **Hard confidence threshold**: 0.3 (cascade doc + `evidence_store.go:330`) — confirmation cannot cross this floor; contradiction can, and the cascade machinery then enqueues downstream dependents.
- **Evidence type registry**: `internal/core/evidence.go:20-27` — single source of truth for v1 types and strengths.