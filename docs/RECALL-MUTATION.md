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
| CLI access reinforcement (`recall.go`) | **Yes — lexical only** | `weight + 0.5`, capped, rate-limited to hourly. Independent of similarity. `--semantic` does not run this path. |
| MCP `mpm_memory` query (`RecordRetrieval`) | **Yes — observability** | One `retrieval_metadata` row per returned id (handlers.go:585-592). Fire-and-forget; not a memory mutation. |
| MCP `mpm_read_wake_context` (`RecordRetrieval`) | **Yes — observability** | One `retrieval_metadata` row per surfaced memory or milestone (handlers.go:2486, 2497). Same observability contract. |
| Memory weight / status | **No** | Only via an explicit, operator-facing call. |

So a retrieval can still write to the database, but only through the
lexical-path reinforcement, which is a deliberate access-tracking
mechanism rather than a consequence of anything the query matched —
and only through the observability writes the MCP `mpm_memory` query
and `mpm_read_wake_context` surfaces make via `RecordRetrieval`. The
MCP-side writes are scoped to `retrieval_metadata`; they do not change
the underlying memory row. The CLI's `--semantic` recall applies
neither: it does not call `RecordRetrieval` and does not call the
lexical reinforcement path.

> **Operational note.** The `+0.5` reinforcement is applied only to
> `returnedIDs`, which is populated on the **lexical SQL-scan path**.
> The `--semantic` path returns at `recall.go:199` from `handleRecall`
> (the early return inside the `if *semantic` block) into
> `renderHybridResults` (`recall.go:981`), which never touches
> `returnedIDs` (declared at `recall.go:174`) and has no `Exec` or
> `UPDATE` call. A semantic query therefore reinforces nothing. This was
> verified with a SQLite trigger that logs any weight change, across
> both paths and both output modes.

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

Idempotency of `EnqueueContradiction` for the sequential case comes
from a **Go-level pre-check** on the unresolved pair, not from a
database-level guarantee. `EnqueueContradictionReport`
(`internal/core/contradiction_log.go:199`, the live writer; see its
`# Why the explicit pre-check` comment at `:171-181`) runs
`SELECT COUNT(*) ... WHERE memory_id_a = ? AND memory_id_b = ?
AND resolved_at IS NULL` (`:235-238`) before the `INSERT OR IGNORE`;
a pending match returns `(false, nil)` and never reaches SQL.

Three things the pre-check is **not**:

- The `UNIQUE(memory_id_a, memory_id_b, detected_at)` index
  (`internal/core/contradiction_schema_patch.go:69`) is not what
  suppresses the repeat. `detected_at` defaults to
  `CURRENT_TIMESTAMP` with one-second resolution, so `INSERT OR IGNORE`
  alone only deduplicates a re-detection inside the same wall-clock
  second — faster than an operator can type the command twice. The
  pre-check was added in `9e75074` after this exact failure mode was
  measured.
- The check is `SELECT` then `INSERT`, not atomic. Two concurrent
  enqueues of the same fresh pair can both insert. That race is
  accepted in `contradiction_log.go:193-198`: the queue is a review
  worklist, a duplicate costs an operator one glance, and the
  alternative — a new `UNIQUE` index on the live shared schema — is
  a larger change than the defect warrants. The callers that matter
  are sequential operator invocations.
- The pre-check is scoped to `resolved_at IS NULL` to match the
  queue's worklist semantics (`contradiction_schema_patch.go:21-24`,
  the partial index on `resolved_at IS NULL`). Once a pair is
  resolved it is immediately eligible to be queued again,
  deliberately.

The sequential case is pinned by
`TestEnqueueContradictionReport_RepeatAcrossSecondsIsNotANewWrite`
(`internal/core/contradiction_queue_idempotency_test.go:45`).
`EnqueueSimilarityCandidates` returns the number of rows **actually**
inserted — a repeat run reports **0**, not last run's total
re-announced as new work — and the end-to-end shape is pinned by
`TestEnqueueSimilarityCandidates_SecondRunOverUnchangedDataInsertsNothing`
(`internal/core/contradiction_queue_idempotency_test.go:156`).

## Applying a candidate requires adjudication

`--apply` no longer treats a queue row as its own justification. A row may
penalise a memory only when `detected_by` carries an explicit operator
adjudication. Everything else is unverified and fails closed: a similarity
candidate, a row routed to arbitration, a legacy row carrying a session id, a
row with no `detected_by` at all, and any value written by something this
codebase has never heard of.

```bash
mpm ops resolve-contradictions --json                      # find queue row ids
mpm ops resolve-contradictions --adjudicate=42:"mem-a claims X, mem-b claims not-X"
mpm ops resolve-contradictions --apply                     # now those rows may penalise
```

`--adjudicate` is per row and repeatable, and the reason is mandatory — it is
the audit trail that distinguishes a considered decision from a reflex. It is
deliberately **not** a force flag: a force flag answers "apply this anyway"
and leaves the question of whether the pair is a real contradiction
unanswered and unrecoverable. There is no batch form, because adjudicating
without reading each pair is the mistake the gate exists to prevent.

The segment is appended, never rewritten, so the detection provenance that
brought a row to the queue stays legible next to the judgement made about it:

```
similarity-scan|adjudicated:read both, claims conflict
```

**No schema change.** This reuses the compound `detected_by` idiom already
established for `theory:` (`applyArbitration`), so there is no migration, no
new column and no backfill.

Refused rows are **not discarded**. Each is reported as `NEEDS ADJUDICATION`
with the `detected_by` that failed the check, why it was not applied, and the
exact command that would change that; `--json` carries the same rows in
`needs_adjudication_rows` alongside a `needs_adjudication` count. They stay in
the queue, waiting. The gate applies to `--apply`, not to visibility — a
dry-run still lists every row, because that is how an operator finds out what
is in the queue in the first place.


## Explicit challenges still work

The sanctioned path is unchanged and audited:

- `mpm challenge <id> "<evidence>"` / `mpm challenge restore <id>`
  (`docs/SPEC.md:560`; the underlying handlers are `handleChallenge`
  at `cmd/mpm/handlers_challenge.go:12` and `handleChallengeRestore`
  at `:194`)
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

"Restorable" here means the **memory half** — the pre-challenge weight goes
back. It never meant belief was reinstated. A restore puts back ranking, not
confidence: `confidence` stays at the challenged floor, the supporting
`evidence` rows stay expired, and the runtime decay accumulators stay reset.
Restoring a flagged memory therefore leaves it findable again but still
distrusted, and fresh evidence is what re-earns confidence. This is F7.1,
not a limitation of the recovery contract, and it is documented in
`docs/SPEC.md` §4.5.

The same holds for the adjudication paths. Memories the two Arc 1 writers
slashed *before* the recovery contract existed (`applyResolution`,
`applyArbitrationResolution`) had their `weight` and `retrieval_priority`
reduced with no record of the prior values, so they are **not** restorable
through MPM — `mpm challenge restore` refuses them with an explicit
"cannot be reversed through MPM" error rather than the misleading
`is not challenged`, and never reconstructs a value it never stored. Nothing
migrates or backfills those rows; recovering them is an out-of-band decision.
Challenges written by the current code are fully restorable on both paths.

Queue rows written before this change remain in place with their original
`detected_by`. Nothing reinterprets them — and because historical rows carry no
`adjudicated:` segment, they are read as *unverified*. Existing rows are
therefore review items, not findings: an operator who wants one applied must
adjudicate it explicitly first. That is the asymmetry made enforceable.

## Operator workflow for the residue (issue #20)

The 41 memories left in `status='challenged'` by the retired
auto-challenge defect (writes removed 2026-10-13) are **preserved
until individually adjudicated**. No blanket restore; no
backfill; no migration. The count is an observation, not an
invariant — it grows if a future operator-initiated
`resolve-contradictions --apply` on an adjudicated row produces a
new challenge via the live `ChallengeMemory` path. Operators are
expected to enumerate and decide per row.

**Discovery surfaces** (read-only, both surfaces cover the same
population — local `memories` plus shared `memories` when attached):

- `mpm challenge list [--json] [--limit N]` — per-row enumeration
  with the columns an operator needs to make a per-row judgement:
  id, store, collection, current weight, the prior weight the
  restore would reinstate, which writer produced the challenge,
  whether the row is restorable through MPM at all, and the
  `challenged_at` timestamp. `--json` is the canonical envelope
  (success / count / limit / challenged[]) for scripts; the
  default is a human-readable table.
- `mpm doctor` — a single-row entry under "Challenged memories"
  reports the population count. Status `PASS` when the count is
  zero, `WARN` when non-zero. The message includes the
  floor/at-floor split so the operator can see at a glance how
  much of the population is a flag-only restore.

**Per-row semantics of `mpm challenge restore <id>`** — what
actually happens when an operator runs the recovery path:

1. The `status` flag is cleared. The row becomes retrievable
   again.
2. `weight` is restored to `challenged_prior_weight` when that
   baseline is present. For 27 of the 41 live rows, the prior
   weight was 1.0, so this is a **no-op on weight** — only the
   flag clears, and the row continues to rank at the floor.
   For 4 rows the prior weight was above the floor; restore
   moves them back to that prior value. The `weight→prior`
   column in `mpm challenge list` makes the no-op case visible
   in advance.
3. `confidence` is reset to `ChallengedMemoryConfidenceFloor`
   (0.5) when the challenge dropped it. **The pre-challenge
   confidence is not re-promoted.** A restore is the memory
   half (ranking, status flag), not the belief half (confidence,
   evidence, decay accumulators). Fresh evidence is what
   re-earns confidence, the same as for any other memory. This
   is the F7.1 contract from `docs/SPEC.md` §4.5; this section
   restates it because the operator running the workflow is the
   one who needs to know what they are not getting back.
4. The linked theory, when present and matchable, is marked
   disproven. The 41 live rows carry free-text values in
   `challenged_theory_id` (the retired defect wrote
   human-readable reasons into that field rather than a theory
   id), so the theory half reports `not_found` for them — the
   memory half still lands. The `theory_reference_shape`
   column in `mpm challenge list` output is the operator's
   signal that the theory half will be a no-op for that row.

**Arc 1 writer records (separate population).** The Arc 1
challenge writers, predating the recovery contract, recorded
no reversible baselines: a row in that population has no
`challenged_prior_weight` (and typically no
`challenged_prior_retrieval_priority` either). For such rows,
`mpm challenge restore` refuses the operation with the
existing "cannot be reversed through MPM" error, and they
surface in `mpm challenge list` with `Restoreable=false` and
a `[!] no prior` weight marker. The audit-time 41 rows are
NOT Arc 1 writer rows — the existing 41-row audit reports
prior weights recorded for all 41. Arc 1 records are a
separate, smaller population that grows only if a future
operator-initiated action writes through the legacy path.
The exact count of Arc 1 records is not asserted here; check
`mpm doctor` and `mpm challenge list` for the live state.

**Contradiction-apply interaction.** The auto-challenge gate is
gone, but `mpm ops resolve-contradictions --apply` can still
reach `ChallengeMemory` from an unverified similarity candidate.
Any new challenge from that path adds to this population. The
discovery surfaces above make that growth visible — the
`mpm doctor` count tracks it; `mpm challenge list` enumerates
it; the per-row `challenged_via` column tells the operator which
writer produced each new entry.

**Out of scope.** No DB-level backfill or mass update; no
re-derivation of pre-challenge values for Arc 1 writer
records; no changes to retrieval, embedding, or
contradiction-apply algorithms. The `mpm challenge restore`
calls on the 41 live rows are a separate operator action and
a separate decision; this section provides the discovery
surface and the policy, not
the restore itself.

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
| 4 | All three former branches | direct retrieval writes nothing to memory or queue; explicit scan still finds each |
| 5 | Repeated retrieval ×6 | queue stays at 0; explicit scan stays at 1 |
| 6 | Diagnostic path | observational |
| 7 | Explicit `ChallengeMemory` | still writes, still records restoration state |
| 8 | Pre-existing challenged state | weight, note and restore info preserved |
| 9 | Ranking stability | order and scores identical across runs |
| 10 | `shared` unattached | retrieval succeeds; the explicit path **reports** failure |
| 11 | 75-row duplicate cluster | 75 weights intact across 3 passes, 0 challenged, 0 queued, all 2775 pairs findable as candidates |
| 12 | Explicit generation | writes only what discovery found; repeat run reports 0; `detected_by` attributable |
| 13 | Zero unintended writes (direct path) | `TestDirectSearch_WritesNothingToMemoryQueueOrRetrievalMetadata` — every column of every memory, every queue row, and every `retrieval_metadata` row byte-identical before/after a direct `HybridSearch` / `HybridSearchMemories` call. |

Case 13 is the strong form on the direct path: it snapshots whole rows
of three stores (memory, queue, retrieval_metadata) rather than
checking columns we happen to know about, so a future regression
adding a new column write to any of the three stores fails there even
if no assertion was written to catch it. The contract is **scoped to
the direct search path** — the MCP `mpm_memory` query and
`mpm_read_wake_context` surfaces are pinned *separately* in
`internal/core/tools/retrieval_metadata_writes_test.go`, where they
*are* expected to write `retrieval_metadata` rows (observability) and
must not mutate memory or the queue. The pair of tests is the
complete contract: direct search writes nothing to any of the three
stores; MCP surfaces write only `retrieval_metadata`.

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