# What recall may and may not mutate

**Status:** the automatic mutation described in earlier revisions of this
document was **removed on 2026-10-13**. Semantic retrieval no longer reduces
memory weight. This note records the current contract and why it changed.

## The contract

> **Semantic similarity must never automatically cause `ChallengeMemory` to
> reduce memory weight.**

That is the whole guarantee, and it is narrower than "recall is read-only".
Recall still has two independent write paths, both described below.

## What changed

`HybridSearch` Phase 4 (internal/core/hybrid_search.go) scans its top-15
candidates pairwise — C(15,2) = 105 comparisons per query — and, on any pair
scoring cosine ≥ **0.85**, used to call `ChallengeMemory` on one side. Three
branches did this: state-collision, provenance-tier, and equal-tier.

The problem was that cosine similarity establishes that two memories are
**related**, not that they **contradict** each other. Nothing in any branch
inspected what the memories actually claimed, so:

- The **equal-tier branch fired on `scanSet[i]` by rank position alone** —
  `j > i` by loop construction. Which memory got penalised depended on
  where it landed in a ranked list.
- **Identical and near-identical statements tripped every branch**, because
  similarity is maximal exactly where duplication is maximal. The gate was
  guaranteed to fire on exactly the case it should ignore.
- On the live store this had challenged **40 distinct members of a single
  75-row near-identical cluster that all asserted the same claim** (30 of
  them pinned at the weight floor of 1.0). Their notes read
  `"unresolved state collision (cosine=0.92) … pending manual review"` —
  the system recording that it had not resolved anything.
- Under the `all-minilm` configuration, 5 of 30 benchmark queries mutated
  the store, including three *irrelevant* ones; `"chicken alfredo pasta
  recipe with mushrooms"` cost −12 weight across 4 challenges.

All three `ChallengeMemory` calls are now gone. The paired
`EnqueueContradiction` calls remain, so the detection signal survives as a
reviewable proposal.

## What recall still writes

Being precise about this, because the distinction matters:

| Path | Writes? | Notes |
|---|---|---|
| `EnqueueContradiction` (Phase 4) | **Yes** | Inserts into `shared.contradiction_log`. Fails soft when `shared` is not attached — the proposal is dropped rather than enforced. |
| CLI access reinforcement (`recall.go`) | **Yes** | `weight + 0.5`, capped, rate-limited to hourly. Independent of similarity. |
| Memory weight / status | **No** | Only via an explicit, operator-facing call. |

So a retrieval can still write to the database. What it can no longer do is
**reduce a memory's weight**. Losing a review item when `shared` is
unattached is the intended failure direction; silently penalising a memory
is not.

## Explicit challenges still work

The sanctioned path is unchanged and audited:

- `mpm challenge-memory` / `handleChallengeMemory`
- `epistemology_tools.go` (`dm.ChallengeMemory`, slash 2)

`ChallengeMemory` (internal/core/db.go) opens a real transaction and writes
`status` → `"challenged"`, a reduced `weight`, `challenged_theory_id` /
`challenged_at`, the pre-challenge `weight` and `confidence` into a restore
snapshot, and an audit record. Retrieval cannot reach it.

## Pre-existing state is untouched

Memories challenged before this change remain challenged, with their
`challenged_prior_weight` / `challenged_prior_confidence` intact and
restorable. Retrieval does not re-slash them, and does not clear them.
Restoring them is a separate, deliberate action via the restore path.

## Validation guards retained

The `validEmbeddingPair` guard (hybrid_search.go) still rejects incomparable
vectors — mismatched dimensions and non-finite values — before any
similarity is computed. Two defects were reachable through the old 0.85
gate:

1. **Truncation.** A 768-dim and a 384-dim vector compared over their
   shared 384 components could score exactly **1.0**, the maximum.
2. **Non-finite values.** A `NaN` in either operand makes every comparison
   false, so `sim < 0.85` is false and the guard *passed*.

These guards are now defence-in-depth rather than the only thing standing
between a malformed vector and a weight reduction: the write is gone, but
an inflated cosine still produces a spurious proposal, so the guard remains
worth having. The same reasoning applies to the `embedding_model`
compatibility filter in `vector_index.go`.

## Scope notes

- The vector-ranking path (`VectorMatch`, hybrid_search.go:989) carries its
  own `len(dbEmbedding) != len(queryEmbedding)` guard. Never affected by
  the truncation defect; read-only, no threshold.
- A third `cosineSimilarity` lives in `internal/core/capability`
  (`forge_dedup.go`) with an explicit, documented truncation policy —
  different package, different contract, untouched.
- Phase 4's 0.85 gate still selects *which pairs are proposed*. It is a
  relevance filter for review, not a contradiction test, and nothing
  depends on it being contradiction-aware.

## Regression coverage

`internal/core/hybrid_search_nomutate_test.go` pins this contract across ten
cases: identical memories, near-duplicates, genuine contradictions,
all three former branches, queue idempotency, the diagnostic path, the
explicit challenge path, preservation of pre-existing challenged state,
ranking stability, and safe failure when `shared` is unattached.

## Related

- Contradiction queue: `mpm ops resolve-contradictions`
- Embedding validity rules: `internal/core/db.go` (`validEmbeddingPair`)
- Four-model comparison and migration preconditions:
  [`EMBEDDING-MODEL-COMPARISON.md`](./EMBEDDING-MODEL-COMPARISON.md)