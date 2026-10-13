# Read-only recall can mutate memory state

**Status:** documented behaviour, not a design proposal. Nothing here is
changed by the `cosineSimilarity` dimension-safety fix that prompted this
note; per that fix's scope the behaviour is recorded and left intact.

## The short answer

Yes. A recall that the caller experiences as read-only — `mpm_context
read_wake_context`, `mpm_memory query`, the session-start wake hook —
can persistently mutate the memories it returns.

## The path

`HybridSearch` (internal/core/hybrid_search.go) runs a pairwise O(n²)
contradiction scan over its candidate set after the vector and BM25
retrieval stages have assembled it. The scan is read-only until a pair
scores at or above **0.85**, at which point one side is passed to
`DatabaseManager.ChallengeMemory`.

Three distinct outcomes, all writes:

| Condition | Write |
|---|---|
| One side `isChallenged`, other is not | `ChallengeMemory(unchallenged, 1, …)` — slash 1 |
| Provenance tiers differ | `ChallengeMemory(loser, 3 or 5, …)` — slash 3 for `ephemeral`, else 5 |
| Equal tier, neither challenged | `ChallengeMemory(first, 1, …)` — flags for manual review |

Each is paired with `EnqueueContradiction`, which inserts a row into the
shared contradiction queue that an operator later resolves via
`mpm ops resolve-contradictions`.

`ChallengeMemory` (internal/core/db.go:5482) is not advisory. It opens a
real `tx`, verifies the row is not hard-deleted, and writes:

- `status` → `"challenged"`, plus `challenged_theory_id` and `challenged_at`
- a reduced `weight`
- the pre-challenge `weight` and `confidence` into a restore snapshot
- an audit record

So the mutation is durable, attributable, and reversible only through the
restore path — not by simply re-running the query.

## Why the 0.85 threshold matters

The threshold is the sole gate. A score that is *meaningless but large*
is indistinguishable from a score that is *meaningful and large*, so a
defective similarity function silently converts recall into an
epistemics operation.

This is why the dimension-safety fix in the same commit places an
explicit `validEmbeddingPair` guard at hybrid_search.go:448 rather than
relying on `cosineSimilarity` returning 0 for incomparable inputs. The
returned 0 would suppress the write correctly, but only as a side effect
of a function whose contract is about angles, not about whether two
vectors may be compared at all.

Two defects were reachable through this gate:

1. **Truncation.** A 768-dim vector and a 384-dim vector were compared
   over their shared 384 components. Two embeddings that agree
   perfectly on that prefix scored exactly **1.0** — the maximum — and
   a 384/384 pair from a different model generation could therefore
   challenge and down-weight a healthy memory.

2. **Non-finite values.** A `NaN` in either operand makes every
   comparison false, so `sim < 0.85` is false and the guard *passes*.
   Non-finite input could reach `ChallengeMemory` and enqueue a
   contradiction whose recorded `Similarity` is not a number.

## Scope notes

- The vector-ranking path at hybrid_search.go:989 already carried its own
  inline `len(dbEmbedding) != len(queryEmbedding)` guard. It was never
  affected by the truncation defect; it is read-only and has no threshold.
- A third `cosineSimilarity` lives in `internal/core/capability`
  (`forge_dedup.go:179`) with an explicit, documented truncation policy —
  it deliberately underestimates similarity to avoid false-positive
  fork detection. Different package, different contract, untouched here.
- Whether read-only recall *should* mutate is a separate design question.
  It is not answered here.

## Related

- Contradiction queue: `mpm ops resolve-contradictions`
- Embedding validity rules: `internal/core/db.go` (`validEmbeddingPair`)