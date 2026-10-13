# What recall may and may not mutate

**Status:** the automatic mutation described in earlier revisions of this
document was **removed on 2026-10-13**. Semantic retrieval no longer reduces
memory weight, and it no longer writes to the contradiction queue either.
This note records the current contract and why it changed.

## The contract

> **Semantic similarity must never automatically cause `ChallengeMemory` to
> reduce memory weight, and must never cause a read to persist a
> contradiction claim.**

Recall still has **one** independent write path, described below.

## What changed, in two steps

### Step 1 — the weight writes (removed)

`HybridSearch` Phase 4 (internal/core/hybrid_search.go) scanned its top-15
candidates pairwise — C(15,2) = 105 comparisons per query — and, on any pair
scoring cosine ≥ **0.85**, called `ChallengeMemory` on one side. Three
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

### Step 2 — the queue writes (removed, same day)

The same reasoning applies to the queue, and removing only the weight writes
would have left half the problem standing. Cosine cannot establish a
contradiction, so filing one automatically asserts a fact the system does
not have — and it does so on **every read**, from every client, including
diagnostics. The measured cost was **78 proposals across 5 benchmark
queries** under `all-minilm` and **83 across 12** under `nomic-embed-text`.
The single worst query produced **35 proposals**, all of them pairs within
one duplicate cluster, **none of them resolved**.

The three-tier split that replaced it:

| Function | File | Writes? | Reached from |
|---|---|---|---|
| `FindSimilarityCandidates` | `similarity_candidates.go` | **No** — pure, no DB | `EnqueueSimilarityCandidates` only |
| `EnqueueSimilarityCandidates` | `similarity_candidates_store.go` | **Yes** — the sole insert path | `mpm ops resolve-contradictions --propose` only |
| `HybridSearch` | `hybrid_search.go` | **No** | everywhere |

`HybridSearch` Phase 4 is deleted outright, ~244 lines including three
`ChallengeMemory` calls and three `EnqueueContradiction` calls. Discovery did
not die with the writes; it moved to a function that cannot write.

**Ranking and result contents are unchanged.** Phase 4 ran after ranking was
fixed and after `IsChallenged` was populated from pre-existing database
state, so it never fed back into either.

## What recall still writes

| Path | Writes? | Notes |
|---|---|---|
| `EnqueueContradiction` (Phase 4) | **No — removed** | Was the only automatic insert. Gone from the retrieval path. |
| CLI access reinforcement (`recall.go`) | **Yes** | `weight + 0.5`, capped, rate-limited to hourly. Independent of similarity. |
| Memory weight / status | **No** | Only via an explicit, operator-facing call. |

So a retrieval can still write to the database, but only through the
lexical-path reinforcement, which is a deliberate access-tracking mechanism
rather than a consequence of anything the query matched.

> **Operational note.** The `+0.5` reinforcement is applied only to
> `returnedIDs`, which is populated on the **lexical SQL-scan path**. The
> `--semantic` path returns at `recall.go:254` before reaching it, so a
> semantic query reinforces nothing. This was verified with a SQLite trigger
> that logs any weight change, across both paths and both output modes.

## What the queue now contains

Two kinds of row, and the difference is the point:

- **Unverified similarity candidates** — two memories scored high on cosine.
  This shows they are RELATED. It does not show their claims conflict, and
  near-identical duplicates score at the *top* of that range. `detected_by`
  is `similarity-scan` for every row written by the new path.
- **Operator-established conflicts** — a human adjudicated these. Existing
  rows keep whatever `detected_by` they already had and are not reinterpreted,
  migrated or deleted.

A candidate is a "look at this pair" item. Every evidence string begins
`unverified similarity candidate (cosine=…)` and none asserts a contradiction
was found, because none was established.

## Explicit review still works

Discovery and persistence were kept, and are reachable on demand:

```bash
mpm ops resolve-contradictions --propose              # scan + enqueue, then review
mpm ops resolve-contradictions --propose-limit=500   # shallower, cheaper scan
mpm ops resolve-contradictions                       # review only; adds nothing
```

`--propose` is the **only** way a row is ever added, and it writes
independently of `--apply`: `--apply` executes verdicts on rows already in
the queue, `--propose` populates the queue. Omitting both, the command is a
pure read.

`EnqueueContradiction` is idempotent on the memory pair (`INSERT OR IGNORE`
against a uniqueness index on the normalised pair), so re-running a scan over
unchanged data inserts nothing. `EnqueueSimilarityCandidates` returns the
number of rows **actually** inserted — a repeat run reports **0**, not last
run's total re-announced as new work.

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

Queue rows written before this change remain in place with their original
`detected_by`. Nothing reinterprets them.

## Validation guards retained

The `validEmbeddingPair` guard still rejects incomparable vectors — mismatched
dimensions and non-finite values — before any similarity is computed. Two
defects were reachable through the old 0.85 gate:

1. **Truncation.** A 768-dim and a 384-dim vector compared over their
   shared 384 components could score exactly **1.0**, the maximum.
2. **Non-finite values.** A `NaN` in either operand makes every comparison
   false, so `sim < 0.85` is false and the guard *passed*.

These guards are now defence-in-depth. They were moved with the scan into
`FindSimilarityCandidates`, which performs the check explicitly rather than
relying on `cosineSimilarity` returning 0 for mismatched dimensions — a
behaviour a future refactor could change without anyone noticing that a write
path now hung off an unvalidated score.

## Scope notes

- The vector-ranking path (`VectorMatch`, hybrid_search.go) carries its own
  `len(dbEmbedding) != len(queryEmbedding)` guard. Never affected by the
  truncation defect; read-only, no threshold.
- A third `cosineSimilarity` lives in `internal/core/capability`
  (`forge_dedup.go`) with an explicit, documented truncation policy —
  different package, different contract, untouched.
- The 0.85 threshold is **unchanged** and deliberately not re-tuned. It is a
  review-worklist selector, not a contradiction test: it answers "is this pair
  close enough to be worth a human's attention", and makes no claim beyond
  that. Re-tuning it is a product decision about review throughput.
- No LLM classifier was introduced. Classification is the same three
  structural branches as before, unchanged.

## Regression coverage

`internal/core/hybrid_search_nomutate_test.go` pins this contract across
thirteen cases:

| # | Case | Asserts |
|---|---|---|
| 1 | Identical memories | maximal-similarity pair keeps weight |
| 2 | Near-duplicates | single-token difference keeps weight |
| 3 | Genuine contradiction | neither penalised **and not queued**, but still discoverable |
| 4 | All three former branches | retrieval writes nothing; explicit scan still finds each |
| 5 | Repeated retrieval ×6 | queue stays at 0; explicit scan stays at 1 |
| 6 | Diagnostic path | observational |
| 7 | Explicit `ChallengeMemory` | still writes, still records restoration state |
| 8 | Pre-existing challenged state | weight, note and restore info preserved |
| 9 | Ranking stability | order and scores identical across runs |
| 10 | `shared` unattached | retrieval succeeds; the explicit path **reports** failure |
| 11 | 75-row duplicate cluster | 75 weights intact across 3 passes, 0 challenged, 0 queued, all 2775 pairs findable as candidates |
| 12 | Explicit generation | writes only what discovery found; repeat run reports 0; `detected_by` attributable |
| 13 | Zero unintended writes | **every column of every memory and every queue row** byte-identical before/after |

Case 13 is the strong form: it snapshots whole rows rather than checking
columns we happen to know about, so a future regression adding a new column
write fails there even if no assertion was written to catch it.

The net was verified against the old behaviour by temporarily reintroducing
the Phase 4 write behind an environment guard: **five tests fail**
(cases 3, 4, 5, 11, 13) and pass again once removed.

## Related

- Contradiction queue: `mpm ops resolve-contradictions --help`
- Discovery and persistence: `internal/core/similarity_candidates.go`,
  `internal/core/similarity_candidates_store.go`
- Embedding validity rules: `internal/core/db.go` (`validEmbeddingPair`)
- Four-model comparison and migration preconditions:
  [`EMBEDDING-MODEL-COMPARISON.md`](./EMBEDDING-MODEL-COMPARISON.md)