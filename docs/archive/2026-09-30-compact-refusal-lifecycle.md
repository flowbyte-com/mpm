# Compact Refusal Lifecycle — Design Specification

> **Status:** **Implemented** (2026-10-01). All 16 claims in §10 are
> pinned by `internal/core/compact_design_matrix_test.go`. Two
> deliberate deviations from the text below are recorded in §11.
> **Date:** 2026-09-30 (design), 2026-10-01 (implemented)
> **Scope:** the epistemic-compaction refusal path — what a sanctioned
> refusal from the model *means*, how a refused batch stops blocking the
> queue, and what the pressure signal is allowed to demand.
>
> **This document is a specification, not a report.** It states
> normative decisions. Every claim about current behaviour is cited to
> the line that establishes it, so a later reader can check rather than
> re-derive.
>
> **Where the implementation and this text disagree, this text is the
> bug** — with the two exceptions named in §11, which were
> implementation-time decisions taken under the design's own
> operator-only intent and are now the contract.

**Repository:** `/home/v/workspace/projects/mpm`
**Authoritative product document:** `docs/SPEC.md` (must be updated
alongside implementation — §8).
**Contributor rules:** `CLAUDE.md` (validation gate, SQLite invariants,
test isolation, scope discipline).

---

## 0. Why this document exists

Compaction of epistemic pressure has never completed. An isolated
reproduction established the following, and each point is load-bearing
for a different section below:

| # | Established fact | Evidence |
|---|---|---|
| F1 | The prompt **sanctions** a refusal as a successful terminal outcome | `internal/core/synth/compact.go:52-66` |
| F2 | The refusal sentinel is exactly `{"title":"", "body":"", "tags":[]}` | `compact.go:54-56` |
| F3 | A refusal reaches persistence as an ordinary empty lesson candidate | `SynthesizeCompactLessonWithPlan` returns raw text (`compact.go:112`); the orchestrator unmarshals it into a zero-valued `CompactLesson` |
| F4 | Validation converts that sanctioned refusal into an error | `CompactLesson.Validate()` → `title required` (`internal/core/compact.go:89-100`), wrapped as `lesson_validation_failed` (`compact.go:248`) |
| F5 | Batch selection is oldest-first, stateless, and unmarked on failure | `ORDER BY created_at ASC LIMIT ?` with no bookmark or offset (`compact.go:473-479`) |
| F6 | Therefore a refused batch is reselected verbatim, forever | F5 combined with F4: the rows are never marked, so the predicate still matches them |
| F7 | `compacted_into` is written only inside the lesson-insert transaction | `commitLessonAndMark` (`compact.go:512-575`) — the `json_insert` marking and the `INSERT INTO lessons` share one `tx` |
| F8 | A drain invocation is bounded to 8 batches | `compactDrainMaxBatchesDefault = 8`, `compactDrainMaxBatchesHardCap = 8` (`compact.go:130,136`) |

F1+F2 against F4 is the core contradiction this document resolves: the
prompt tells the model that refusing is correct, and the code then
treats the refusal as a malfunction. The failure is reported, but the
rows are left exactly as they were, so the next run makes the identical
call on the identical 50 rows (F6). Nothing ever drains.

**Invariant that must survive every option below (F7):**
`compacted_into` is never written except in the same transaction that
inserts the lesson it points at. A refused batch therefore *cannot* use
that field to record its own outcome — there is no lesson. Any design
that needs a marker for "handled but not compacted" must introduce a
*different* field. This is why §2 adds a new metadata key rather than
reusing the existing one.

---

## 1. Vocabulary

The word "quarantined" was considered and rejected. A refusal means
**this batch does not constitute a synthesizable lesson** — it says
nothing about whether the individual memories are valid, useful, or
retrievable. A perfectly legitimate memory may simply have no sibling
worth consolidating with. Quarantine implies the data is suspect, and
nothing here establishes that.

| Term | Meaning | Still retrievable? | Requeueable? |
|---|---|---|---|
| **pending** | never yet offered to the model | yes | n/a |
| **compacted** | a lesson was created from it; `compacted_into` is set (F7) | yes | no (terminal, correct) |
| **deferred** | offered, and the model sanctioned a refusal; deferral keys are set | **yes, unchanged** | **yes** |
| **failed** | offered, and something actually broke (schema, transport, budget) | yes | n/a — loud, see §5 |

`deferred` is deliberately a *scheduling* state, not a judgment about
the data. The row is not modified in any way an operator would describe
as "we rejected your memory."

---

## 2. C1 — a typed refusal outcome

### 2.1 The problem

`CompactLesson.Validate()` is currently asked to do two incompatible
jobs: enforce that a well-formed lesson has a title, a body, and tags;
**and** decide whether an all-empty object is a deliberate refusal or a
malformed model output. It cannot, because after `json.Unmarshal` the
two are the same value. It resolves the ambiguity by calling the
sanctioned case an error (F4), which is how a correct model behaviour
becomes a red failure.

### 2.2 The decision

Introduce an explicit three-way outcome, decided **before**
`Validate()` is consulted:

```go
type CompactOutcome string

const (
    OutcomeLesson  CompactOutcome = "lesson"   // a real lesson was produced
    OutcomeRefused CompactOutcome = "refused"  // the sanctioned sentinel
    OutcomeError   CompactOutcome = "error"    // genuinely malformed
)
```

Classification is by exact shape, and the sentinel is the *only*
refusal:

| Parsed shape | Outcome | Rationale |
|---|---|---|
| all three fields empty (`title=="" && body=="" && len(tags)==0`) | `refused` | exactly F2's sentinel, and nothing else produces it |
| `title==""` but `body` or `tags` non-empty | `error` | not the sentinel; a truncated or hallucinated response |
| all fields present | `Validate()` decides `lesson` vs `error` | unchanged semantics |

**`Validate()` keeps its current body and its current responsibility.**
It is still the sole authority on whether a *populated* lesson is
well-formed. It is simply never asked to adjudicate the sentinel,
because classification runs first. This is the whole point: the
malformed-output check and the refusal check stop sharing one code
path.

The classifier belongs at the boundary where the raw model text enters
the domain — immediately after `json.Unmarshal`, before the struct is
treated as a candidate lesson. It is a pure function of the parsed
value, so it is exhaustively testable without a database.

### 2.3 Consequence

`refused` is **not** an error. It returns `nil` error. The batch is
deferred per §3. The drain continues to the next batch. The refusal
never reaches the operator as a failure, because it is not one.

---

## 3. C2 — the deferred state

### 3.1 Representation

Deferred rows are marked in the existing `metadata` JSON, alongside the
field F7 already uses:

```json
{
  "compaction_deferred_at":    "2026-09-30T21:14:02Z",
  "compaction_deferred_reason": "refusal_sentinel",
  "compaction_deferred_batch":  "les-none",
  "compaction_deferred_sample": "…truncated raw model response…"
}
```

A `metadata` key rather than new table columns, because that is the
mechanism `compacted_into` already uses and `CLAUDE.md` §6 says to avoid
migrations unless the behaviour genuinely requires durable schema
evolution. It does not: this is a per-row annotation on an existing
JSON column with an existing extraction predicate that already reads
`json_extract(metadata, '$.compacted_into')`.

A separate key is required regardless of storage choice, for the reason
in F7: `compacted_into` may only be written when a lesson exists.

### 3.2 Properties

A deferred row:

1. **content is not modified.** `content`, `weight`, `collection`,
   `created_at`, and every other substantive column are byte-identical
   before and after. The `metadata` column *does* change — it must, since
   that is where the deferral keys live — so "unmodified" here means
   "unmodified in every respect a retrieval or an operator would
   describe", never "no byte anywhere changed". Stating it otherwise would
   be false and would invite an implementer to add a separate column
   precisely to make the sentence true.
2. **stays fully retrievable** — no read path consults the deferral
   keys. Search, `memory get`, wake context, and export are untouched
   by design, and this is a testable assertion, not an aspiration;
3. **leaves the batch pool** — the extraction predicate gains
   `json_extract(metadata, '$.compaction_deferred_at') IS NULL`, so the
   row is no longer selected;
4. **is never deleted and never forged as compacted**;
5. **is requeueable** — clearing the four keys returns it to `pending`,
   where `ORDER BY created_at ASC` places it back among the oldest. The
   requeue contract is specified in §3.3.

Property 3 is the mechanism that ends the loop. Because a deferred row
no longer matches the extraction predicate, the *next*
`extractRawBatch` call returns the **next** un-attempted rows rather
than the same 50. F6 is broken by making the predicate honest about
what has already been offered.

### 3.2.1 Atomicity of a deferral

Deferring a batch is a **transactional all-or-nothing operation**. Either
every row in the batch receives the deferral annotation, or none does.
There is no partial state in which some rows of one batch are deferred
and others are not.

This is not an aspirational nicety — a partial write would be actively
harmful. `compaction_deferred_batch` is what makes a deferral groupable
and inspectable, and it is what lets an operator reason "these 50 were
refused together for this reason". If a crash mid-write left 30 of 50
marked, the remaining 20 would be reselected as a fresh batch, offered
to the model again, and very likely refused again — re-creating exactly
the loop this design removes, but with a corrupt annotation to diagnose
on top of it.

All rows in one deferral therefore carry **identical** values for:

| key | value |
|---|---|
| `compaction_deferred_at` | one timestamp, computed once before the write |
| `compaction_deferred_reason` | one reason string (`refusal_sentinel`) |
| `compaction_deferred_batch` | one stable batch id |
| `compaction_deferred_sample` | one truncated model response, the same for every row in the group |

The timestamp is computed **once, in the caller**, and passed to the
transaction. Computing it per-row inside the loop would produce
microsecond-different values, which are equal in practice but not
identical in the database — enough to make "group these rows by batch"
a query that nearly-but-not-quite matches.

The write follows the repository's existing transaction discipline
(CLAUDE.md §4): a single `tx` carrying both the metadata updates and the
caller's audit write, with `*sql.Tx` threaded through the helper. It must
not be a loop of independent bare-`db` writes.

### 3.3 Requeue

Requeue returns a deferred row to the selectable pool. It is specified
here as `mpm compact requeue-deferred` — an **operator CLI** command with
no MCP counterpart (§11.1 records that decision and why). Its semantics
are fully specified here because a partially-specified requeue is the
most likely way this design does real damage.

**R1 — explicit operator action only.** `requeue_deferred` is never
implied, defaulted, or triggered. No scheduler tick sets it, no wake
does, no retry logic does, and a bare `compact_epistemology` call must not
set it. A refusal was a considered judgment by the model; silently
re-offering the same rows on a timer would reintroduce the infinite loop
this design removes, just slower and harder to notice. The parameter
defaults to off, and the drain's refusal path must not be able to reach
it.

**R2 — bounded target.** A single requeue call affects at most `limit`
rows (default: the same `compactBatchSize` of 50; explicit `limit` is
capped by the existing hard cap), selected `ORDER BY created_at ASC` —
the same ordering the drain uses, so the operator requeues the oldest
deferrals first and the behaviour is predictable. It is not
"requeue everything", and there is no unbounded mode. Unbounded requeue
of a large backlog is a single call that can offer hundreds of rows back
to the model, which is exactly the model-call pressure the stage budget
exists to bound.

**R3 — clears only deferral metadata.** The requeue removes exactly the
four `compaction_deferred_*` keys:

- `compaction_deferred_at`
- `compaction_deferred_reason`
- `compaction_deferred_batch`
- `compaction_deferred_sample`

and writes nothing else. Content, weight, collection, `created_at`, and
every other substantive column are untouched. This is the same
all-but-the-metadata claim §3.2 property 1 makes for deferral, read in
reverse: the two operations are exact inverses with respect to the row's
identity and its retrieval behaviour, which is what makes requeue
auditable — a requeued row is byte-identical to a row that had never
been deferred.

The one exception is `updated_at`, which both operations stamp, and the
exception is deliberate: the row *was* modified, and leaving the
timestamp at the deferral time would make a requeued row look untouched
to anything that reads recency. `updated_at` is a bookkeeping column,
not part of the row's identity or its retrievability, which is why §3.2
property 1 says "substantive column" rather than "column". Every
substantive column — including `created_at` — survives requeue
byte-identical, which is the property the audit trail depends on.

**R4 — never clears `compacted_into`.** Requeue deletes no key it did
not create, and specifically does not touch `compacted_into`. This is
the asymmetry that matters most. `compacted_into` is written only when a
lesson exists (F7) and means the row's content *has* been synthesised
into durable knowledge. A row that carries both keys — compacted at
some earlier point, and later re-deferred for some reason — must stay
compacted. Clearing `compacted_into` would make the row eligible for
re-synthesis of content that already has a lesson, duplicating a lesson
and silently un-doing real work. Requeue is a deferral-lifecycle
operation; it is not, and must never become, a compaction-lifecycle
operation.

**R5 — auditable.** Every requeue writes an audit row recording that it
happened and naming the bounded target actually cleared (row ids, or a
stable batch id and a count). The row is written **in the same
transaction as the mutation**, so the guarantee cannot be broken by a
failed insert: an audit failure rolls the requeue back rather than
leaving an override with no record of it. The rationale is
asymmetric: a deferral is machine-initiated and needs no per-row
attribution because `compaction_deferred_reason` is already stored on the
row; a requeue is a *human overriding a machine decision*, and the
record of who overrode what, when, and how much is the only way to
reconstruct why a refused batch was offered again. It also makes
repeated requeue visible: a row requeued three times has three audit
rows and, if it defers again, a `compaction_deferred_batch` that
differs from its predecessor's.

Two properties follow from the above and are worth stating because they
are what make requeue *inspectable* before it is performed: before
running it, an operator can list the deferred rows and their reasons,
because the reason and a truncated sample are stored on the row.

Deferred rows are not re-batched with newly-arrived material. They
return to the pool on their own `created_at`, so a requeued row may
join a batch with whatever is pending at that time. Requeue therefore
also consumes stage budget on the next drain like any other row — it
returns the row to the pool, it does not schedule a synthesis.

---

## 4. C3 — pressure semantics

### 4.1 The problem

`epistemic_pressure_v` exposes a single `raw_count`
(`internal/core/schema.go:154-161`) counting every un-compacted memory.
If deferred rows stay in that count, the wake trigger keeps demanding
work that has already been terminally declined — the pressure signal
would never fall, and the agent would be permanently nagged about a
backlog it is not permitted to work on. That is the "endless demand"
failure and it is a *new* failure this design could introduce, so it
must be designed against rather than discovered later.

### 4.2 The decision

The view gains a third count and the wake reads a different one:

| Field | Meaning | Change |
|---|---|---|
| `raw_count` | total un-compacted, **including** deferred | **unchanged** — same name, same meaning |
| `actionable_pending` | `pending` only; rows an agent may act on right now | **new** |
| `deferred_count` | rows offered and refused | **new** |

**`raw_count` is not redefined.** It keeps its current semantics
exactly, and this is the compatibility requirement: existing readers
selecting `raw_count, lesson_count` continue to work, and any operator
who reads "raw" as "everything not yet compacted" still gets that. A
redesign that quietly narrowed `raw_count` would have been a silent
contract break for every consumer, including the CLI and MCP surfaces.

The **wake trigger** moves to `actionable_pending`. Concretely: the
epistemic-pressure wake fires when `actionable_pending` crosses the
threshold, not `raw_count`. Until then it is silent. A deferred backlog
alone can no longer hold a wake open.

`deferred_count` is reported so the pressure block can say *why* the
backlog is not shrinking, rather than leaving the operator to infer it.
A block whose raw_count is 120, actionable_pending is 0, and
deferred_count is 120 is a completely different situation from one
where all 120 are actionable — and the agent needs to be able to tell
them apart to avoid re-issuing a call that cannot succeed.

### 4.3 The equations, stated against the real predicates

The view's current `raw_count` predicate (`internal/core/schema.go:155-160`)
is:

```sql
collection = 'memories'
  AND deleted_at IS NULL
  AND (metadata IS NULL OR metadata = ''
       OR json_extract(metadata, '$.compacted_into') IS NULL)
```

and `extractRawBatch`'s selection predicate
(`internal/core/compact.go:474-479`) is **the same predicate, character
for character**. That is the load-bearing fact for this design: the
drain and the pressure view agree today, and they must keep agreeing,
or the pressure signal will describe a different population than the one
the drain works on.

So define, over the set `U` of rows matching that predicate verbatim:

```text
U    = { m : collection='memories' AND deleted_at IS NULL
             AND (metadata IS NULL OR metadata = ''
                  OR json_extract(metadata,'$.compacted_into') IS NULL) }

D    = { m ∈ U : json_extract(metadata,'$.compaction_deferred_at') IS NOT NULL }
A    = { m ∈ U : json_extract(metadata,'$.compaction_deferred_at') IS NULL }
```

and the three reported fields are exactly:

```text
raw_count         = |U|          = |A| + |D|      -- UNCHANGED, same name, same predicate
deferred_count    = |D|                            -- NEW
actionable_pending = |A|                          -- NEW
```

Equivalently, and this is the identity that must be asserted in tests:

```text
raw_count = actionable_pending + deferred_count
```

with the single predicate change that both `epistemic_pressure_v` and
`extractRawBatch` apply — adding
`AND json_extract(metadata, '$.compaction_deferred_at') IS NULL` to
define `A`. The user's suggested form — *deferred_count = uncompacted +
deferred* — is **not** what is implemented here, and the difference is
not cosmetic:

- Under the design as written, `deferred_count` counts only rows that
  are *simultaneously* un-compacted and deferred. A row that was
  compacted (carrying `compacted_into`) and later annotated deferred is
  in neither `A` nor `D`, and is not reported anywhere. That is
  correct, because a compacted row is finished work and its presence in
  a pressure count is noise.
- Were `deferred_count` to be "everything ever deferred, compacted or
  not", the identity would be `raw_count = actionable_pending +
  deferred_count − already_compacted_deferred`, and the view would need
  a third subquery over rows that have left the working set. That is a
  strictly larger query for no operational gain: the question an
  operator asks is "how much of my *pending* backlog is stuck", and the
  answer is `|D|`.

The empty-`metadata` arm deserves explicit attention because it is easy
to break. The predicate's third arm,
`metadata IS NULL OR metadata = ''`, exists so that rows with no
metadata are still selectable. `json_extract` returns NULL for both of
those, so the new `deferred_at IS NULL` arm is satisfied and such rows
land in `A`, not `D`. No special-casing of the empty case is needed, and
none may be added — an implementation that writes the new arm as
`metadata = '' OR ...` rather than relying on `json_extract`'s NULL
would be relying on incidental behaviour of a function whose NULL
semantics are the entire point.

One consequence of `U` being unchanged is worth stating because it is a
property, not a bug: `raw_count` does **not** fall when rows are
deferred. It is a measure of backlog size, and deferring does not shrink
the backlog — it changes what the backlog consists of. That is exactly
why the *wake trigger* must move to `actionable_pending` while the
*field* must not move at all. A design that shrank `raw_count` would
make the backlog look solved; a design that left the wake on `raw_count`
would make it unresolvable. Keeping the field and moving the trigger is
the only combination that is both honest and live.

---

## 5. C4 — subdivision against the synthesis budget

### 5.1 The budgets this must respect

These are existing, enforced values — not targets this design may
trade away:

| Constant | Value | Source |
|---|---|---|
| `compactBatchSize` | 50 | `compact.go:121` |
| `compactDrainMaxBatchesDefault` / `HardCap` | 8 / 8 | `compact.go:130,136` |
| `MaxSemanticStagesPerInvocation` | 8 | `synth/safeguard.go:133` |
| `absoluteCallCeilingPerInvocation` | 16 | `synth/safeguard.go:140` |

One fact sharpens the arithmetic considerably: **a refusal is cheap.** It
is a well-formed JSON response, so it consumes **one** wire call and
triggers neither the transient retry nor the malformed-repair slot. The
3-calls-per-stage worst case applies to genuine *errors*, not to the
refusal path this document is about. The table below is priced on the
refusal path, with the error-path multiplier noted.

### 5.2 Options, priced

Worst case: a 50-row batch whose model call refuses, and in which
**every** sub-batch also refuses. Stages are the unit
`MaxSemanticStagesPerInvocation` counts.

| | Option | Stages for one 50-row all-refusing batch | Wire calls (refusal path) | Fits 8-stage / 16-call budget? | Rows salvaged |
|---|---|---|---|---|---|
| **A** | Defer the whole batch on first refusal | 1 | 1 | yes, 8× headroom | 0 |
| **B** | One bounded deterministic split, then defer | 3 (parent + 2 halves) | 3 | yes — 3 of 8 stages, but only *after* the drain's accounting is corrected to count stages (§5.3) | 0 if both halves refuse |
| **C** | Fixed smaller initial batch (50→10) | 5 | 5 | yes, but only 1 batch per invocation | depends on refusal rate |
| **D** | Deterministic non-LLM grouping | 0 extra | 0 | yes | n/a — abandons synthesis |
| **E** | Recursive subdivision, strict global call budget | see below | see below | **no**, see §5.3 | highest |

**Option E's cost, stated plainly.** Binary subdivision of *n* items has
2*n*−1 nodes in its recursion tree, and each node is one LLM call. For
*n* = 50 fully subdivided to singletons that is **99 calls** — against a
16-call per-invocation ceiling, a **6× overrun**, and against an
8-stage ceiling, a **12× overrun**. Three levels of subdivision
(50 → 8 groups) already costs 15 stages. This is the explosion the
option is famous for, and it is why "salvages more rows" is not a
sufficient reason to choose it. A strict global budget does not make it
cheap; it makes it *unpredictable* — the same input costs 1 call or 15
depending on where the refusals fall, which is precisely the property a
cost guard exists to eliminate.

### 5.3 The decision: **B**, with a fixed stage cost

Split once, deterministically, in half; attempt both halves; if both
refuse, defer all 50.

Why B over the alternatives:

- **over A** — A gives up the entire batch on the first refusal. A
  contradictory *pair* inside an otherwise-coherent 50 is a realistic
  case, and B recovers it. The cost is bounded and known in advance:
  exactly 3 stages, never 15, never 99.
- **over C** — C pays 5 stages on *every* invocation to defend against a
  failure that, on the current evidence, is not universal. B pays 3
  stages only on the batches that actually refuse. If the refusal rate
  is low, C is strictly more expensive for a benefit that never
  materialises.
- **over D** — D removes the model from the decision, so it can never
  refuse, and that is exactly why it cannot synthesise. It would need a
  different downstream path, changing the semantics of what a "lesson"
  is. Out of scope for a defect fix.
- **over E** — rejected on the numbers above. E's only advantage is
  salvaging more rows in a case (deeply nested refusal) where the
  remaining rows are individually the *least* synthesizable material in
  the batch.

**Deterministic, not model-chosen.** The split is by position in the
already-ordered selection — first 25, last 25 — with no model call and
no randomness. Two runs on the same backlog split identically, which is
what makes the cost predictable and the behaviour testable.

**Odd and undersized batches.** The split must be total and defined for
every batch size, not just the nominal 50:

```
half = len(rows) / 2          # integer division, floor
left  = rows[:half]
right = rows[half:]
```

| `len(rows)` | split | notes |
|---|---|---|
| 50 (nominal) | 25 / 25 | the normal case |
| 49 (odd) | 24 / 25 | the larger child goes **right**; floor on the left |
| 2 | 1 / 1 | minimum viable split |
| 1 | — | **no split is attempted**; defer immediately |

The `len < 2` case is the important one, because it is where a naive
"always split" implementation crashes or silently loops. A single memory
has no sibling to synthesise with, and the model has already said it
cannot produce a lesson from it — splitting it would produce two empty
halves and a second, pointless model call to learn nothing. The rule is
therefore: **`len(rows) < 2` defers on the parent's refusal without
splitting**, costing 1 stage rather than 3.

Note the interaction with the existing early-exit: the drain loop
already exits when `batch.Compacted < compactBatchSize`
(`internal/core/compact.go:415-418`), so undersized batches do occur and
are already treated as terminal for that iteration. Splitting does not
change that; it only applies to a full batch that refused.

**Why floor-left, and not ceiling-left.** Either choice is
deterministic, so this is a tie-break on which is easier to re-derive from
the rows alone. Floor-left means `right` is always the larger child, so
"the larger half" is a stable description — and it matches Go's slice
idiom (`rows[:n/2]`, `rows[n/2:]`), which is what an implementer will
write by default. Choosing the other convention would require an
explicit adjustment and a comment explaining why.

**The stage budget holds — with one correction to the arithmetic.**
Worst case per invocation under B would be 8 batches × 3 stages = 24
semantic stages, which **exceeds**
`MaxSemanticStagesPerInvocation = 8`. The batch cap and the stage cap
are different budgets, and satisfying one does not satisfy the other, so
subdivision cannot rely on the batch cap alone.

**However, the drain already enforces the stage ceiling today**
(`internal/core/compact.go:357-359`): `maxBatches` is clamped to
`MaxSemanticStagesPerInvocation` after the default and hard-cap clamps,
and the comment there calls itself "the operative truth: 8 stages per
invocation." So the total cannot reach 24 stages today.

What subdivision changes is the *meaning* of that clamp. Today one
batch consumes one stage; under B a batch may consume three (parent plus
two halves). The existing clamp counts batches, so it stops admitting
new batches without accounting for the extra stages a single batch can
now spend. Implementation must therefore make the drain count **stages**
rather than batches, keeping the same ceiling of 8.

### 5.4 The exact stage budget for one drain invocation

The hard ceiling is **`MaxSemanticStagesPerInvocation` = 8 semantic
stages per invocation** (`internal/core/synth/safeguard.go:133`). This
is a per-invocation ceiling on model attempts, and it is treated as
fixed — not a target this design may trade away.

A **stage** is one model synthesis attempt. One drain invocation spends
stages in this exact order:

| step | action | stages consumed | cumulative |
|---|---|---|---|
| 1 | initial attempt on batch 1 (50 rows) | 1 | 1 |
| 2 | it refuses → split deterministically into two children | 0 (no model call) | 1 |
| 3 | attempt child A (25 rows) | 1 | 2 |
| 4 | attempt child B (25 rows) | 1 | 3 |

So **one batch costs 1 stage when it succeeds, 3 when it refuses**, and
splitting itself is free because it is positional, not model-driven.

The invocation continues admitting fresh batches until the budget is
gone. Note that a batch's cost is **not** knowable before it is
attempted — the whole/partial split is only taken if the parent actually
refuses — so the check happens at two distinct points, and both are
*before* spending:

```
stagesUsed = 0
for each selected batch:
    # admission: every batch costs at least 1
    if stagesUsed + 1 > 8:
        stop with stop_reason = "max_batches_reached"

    attempt the whole batch                    # spend 1
    stagesUsed += 1
    if it succeeded or errored:
        continue                               # next batch

    # it refused
    if len(batch) < 2:
        defer the batch; continue               # no split, nothing to split
    if stagesUsed + 2 > 8:
        defer the batch; continue               # cannot afford both children
    split deterministically
    attempt child A; attempt child B            # spend 2
    stagesUsed += 2
    defer whichever children refused
```

Consequences worth stating plainly:

- **Worst case per invocation is exactly 8 stages, never 9.** Both
  checks precede the spend, so the invariant is structural rather than
  arithmetic — there is no path on which the counter exceeds 8.
- **A batch is never abandoned half-processed.** If the budget cannot
  afford the split, the whole batch is deferred as-is rather than
  having its first child synthesised and its second abandoned. An
  abandoned child is precisely the F6 failure mode in miniature: the
  rows stay pending, are reselected, and produce the same refusal again
  having already been billed one attempt. Splitting is all-or-nothing
  for the same reason deferral is (§3.2.1).
- **A fully-refusing backlog retires 4 batches (200 rows) per
  invocation**: two at the full 3-stage cost (6 stages), then two more
  whose splits are refused for budget and which are therefore deferred
  whole at 1 stage each. So the 110 uncompacted rows of the F6 scenario
  — batches of 50, 50, and 10 — reach `actionable_pending == 0` in a
  **single** invocation, not forever. The bound is a consequence of the
  budget arithmetic, and it is what makes deferral a bounded settlement
  rather than an open-ended parking lot.

This is a change to the drain's *accounting*, not to the safeguard's
constants. The safeguard's limits are treated as fixed.

### 5.5 On renaming "batches" to "semantic stages"

Subdivision makes the old word wrong: a "batch" was 1:1 with a model
call, and now it is 1:1 with 1 *or* 3. Leaving the parameter named
`max_batches` while it governs stages would be a lie of exactly the kind
already present in the advertised 20/100 (§5.3).

**Decision: do not rename the public parameter; rename the internal
accounting, and make the relationship explicit in the docs.**

- `max_batches` stays. It is a public parameter in the registry schema
  and the tool description; renaming it is a breaking wire change for
  every agent that passes it, for a defect fix that does not require it.
- Internally the drain loop's counter becomes `stagesUsed`, and the
  clamp at `compact.go:357` is re-expressed against
  `MaxSemanticStagesPerInvocation` directly rather than through
  `maxBatches`.
- The registry description and schema get one added sentence: the value
  bounds **model calls per invocation** (8), and with refusal
  subdivision a single batch may consume up to 3 of them.

The naming asymmetry is worth a note in the docs, because "batch" will
remain a real concept (a set of rows selected together) while "stage"
becomes the unit of cost. They are different things and conflating them
is what produced the 20/100 drift in the first place.

**Documentation defect found while verifying this section.** The
`max_batches` parameter is advertised as "default 20, hard cap 100" at
**five** sites, all of which disagree with the constants 8 and 8
(`compact.go:130,136`):

| site | text |
|---|---|
| `compact.go:20-22` | header doc comment: `max_batches — per-invocation cap on LLM calls (default 20, hard cap 100). 20 batches × 50 raw = 1000 raw memories per call.` |
| `compact.go:117-120` | `compactBatchSize` doc comment: `while still draining a backlog of 1000+ raw in ~20 calls` |
| `registry_list.go:505` | registry tool description: `max_batches (default 20, hard cap 100, silently clamped)` |
| `registry_list.go:537` | JSON schema: `"default": 20, "description": "Default 20, hard cap 100..."` |
| `handlers.go:4850` | handler doc comment: `max_batches (int, default 20, hard cap 100)` |

Sites 1 and 2 are separate comment blocks, which is why the second one
is easy to miss: the "1000+ raw in ~20 calls" claim documents
`compactBatchSize`, not `max_batches`, but it is a capacity claim about
the same drain and is wrong by the same 12×.

The *effective* behaviour is correct because the value is clamped
(`compact.go:350-359`), so this is not a live defect — an agent passing
`max_batches: 100` silently gets 8. But the advertised contract is wrong
on a safety-relevant knob, and an agent reasoning about cost from the
description will mispredict by 12×. Two of the five are
machine-read: `registry_list.go:537` is the JSON schema an agent parses,
and `registry_list.go:505` is the description it reads, so this is not
merely stale prose — it is a wrong machine contract. Correcting all five
sites is in scope for the implementing change.

The stale numbers also encode a now-false cost model, which is why this
is a §C4 concern rather than a typo: "20 batches × 50 raw = 1000 raw
memories per call" is the advertised *unit economics of a drain
invocation*, and it is off by 12×. Any capacity planning an agent or
operator performs from it is wrong.

After B's split, if both halves refuse, all 50 are deferred and the
block is gone from the pool. Progress is therefore guaranteed even on
the total-failure path, which is the property A also has and which is
why A remains a legitimate degraded mode if the stage cap proves too
tight in practice.

---

## 6. C5 — the progress invariant

> **One sanctioned semantic refusal can never prevent later pending
> memories from being considered forever.**

**Mechanism.** Every batch terminates in exactly one of three states,
and in all three the batch leaves the extraction predicate:

| Terminal state | Rows leave the pool because | Pool shrinks by |
|---|---|---|
| lesson committed | `compacted_into` set (F7) | 50 |
| refusal → deferred | `compaction_deferred_at` set (§3) | 50 |
| genuine error | drain stops and reports (§7) | 0 — **loud** |

Because a deferred batch is indistinguishable from a compacted one to
the selection query — both fail the predicate — the "reselect the same
50 forever" behaviour (F6) cannot recur. Each drain invocation either
advances the pool by at least one batch or reports why it did not.

**Why this is an invariant and not a hope.** The predicate is the only
thing that decides what is selected, and every terminating outcome
writes to it. There is no path in which a batch completes without
changing the predicate, so there is no path in which the same rows are
offered twice.

**The residual this does *not* cover, stated plainly.** The invariant
is about *sanctioned refusal*. A batch that fails with a genuine error
(§7) leaves its rows `pending`, and those rows will be reselected on the
next invocation. This design does not hide that: a permanently erroring
first batch can still block later rows across invocations. Fixing that
would mean deferring on error, which would convert a loud operational
failure into a quiet one — a worse trade. It is recorded here as a
known residual with a named follow-up (defer after *N* consecutive
error outcomes for the same rows, with the count surfaced), explicitly
**not** designed in this document.

---

## 7. Errors stay loud

Only refusals become quiet. A genuine error — schema violation, exhausted
retry budget, transport failure — keeps today's behaviour: the drain
stops, the failure is reported with `failed_batch` and
`failure_reason`, and the rows stay `pending` for inspection.

Rationale: an operator debugging a broken model configuration needs to
see it. Deferring on error would convert a fixable misconfiguration
into an invisible backlog that quietly grows, and it would make the
pressure signal in §4 lie.

---

## 8. C6 — out of scope

The 75 historical fixture rows currently sitting un-compacted in the
live substrate are **explicitly out of scope** for this design.

- No backfill. Nothing in this design retroactively marks, defers, or
  rewrites any existing row.
- No migration, no data edit, no one-shot repair script.
- Those rows are `pending` today and will remain `pending` until the
  implemented behaviour first offers them to the model. Whether each is
  compacted or deferred is then decided by the normal path, with the
  normal audit trail.

They are called out because a design that adds a new state to a
selection predicate is exactly the kind of change that tempts a
"while we're here" backfill, and because their presence in the live
substrate makes the pressure numbers in §4 immediately non-zero in a way
that would otherwise look like a regression on first run.

---

## 9. Compatibility summary

| Surface | Effect |
|---|---|
| `epistemic_pressure_v` | two columns added; `raw_count` and `lesson_count` unchanged in name and meaning |
| wake / `epistemic_pressure` block | new fields added; trigger switches to `actionable_pending` |
| `CompactLesson.Validate()` | body unchanged; no longer consulted for the sentinel |
| `compact_epistemology` result envelope | `stop_reason` gains a refusal-derived value; existing values unchanged |
| `compact_epistemology` parameters | unchanged — requeue is **CLI-only**, not a parameter (§11.1) |
| `max_batches` advertised default/cap | **corrected** 20/100 → 8/8 at all sites (§5.5 enumerated five; implementation found seven — §11.2); the parameter *name* is unchanged, only the advertised value |
| drain loop accounting | counts stages instead of batches; ceiling stays 8 |
| `compacted_into` semantics | unchanged (F7) |
| `docs/SPEC.md` | must be updated alongside implementation |

No existing column is redefined and no existing field is repurposed.
Every change is additive, which is the compatibility analysis §4 was
required to perform.

---

## 10. What implementation must prove

Not a task list — the claims this design makes, each of which must be
pinned by a test or the design is wrong.

1. The exact sentinel classifies as `refused`; a partially-populated
   object with an empty title classifies as `error`. (Pure unit test, no
   DB.)
2. `Validate()`'s existing assertions are unchanged for populated
   lessons. (Guards against §2.2 quietly weakening validation.)
3. A refused batch writes the four deferral keys and does **not** write
   `compacted_into`.
4. A deferred row is returned unchanged by every read path that matters
   — search, get, and export.
5. Two consecutive drains over the same fixture select **different**
   rows. (Directly falsifies F6.)
6. The stage cap counts **stages**, not batches, and the worst-case
   refusal path stays within `MaxSemanticStagesPerInvocation` (= 8)
   even when every batch spends three stages. A 3-stage batch must
   leave fewer than 8 batch slots for the remainder of the invocation.
7. `raw_count` counts deferred rows; `actionable_pending` does not; and
   the identity `raw_count = actionable_pending + deferred_count` holds
   for every fixture population, including the empty-`metadata` and
   `NULL`-metadata arms.
8. A fully-refusing backlog reaches a steady state with
   `actionable_pending == 0` in bounded invocations.
9. Error outcomes still stop the drain and still report
   `failed_batch` / `failure_reason`.
10. A deferral is atomic: a fault injected midway through the write
    leaves **zero** rows annotated, never a partial group. And every
    row in one deferral carries a byte-identical
    `compaction_deferred_at` and `compaction_deferred_batch`.
11. Requeue returns a deferred row to the pool at its original
    `created_at` position, clears exactly the four
    `compaction_deferred_*` keys, and leaves every other substantive
    column byte-identical (`updated_at` is stamped by design — R3).
12. Requeue does **not** clear `compacted_into` on a row that carries
    both. (Directly falsifies the R4 hazard.)
13. Requeue is bounded: a call requeues at most `limit` rows and
    returns which ones. There is no unbounded mode.
14. Requeue is explicit: a bare `compact_epistemology` call, a
    scheduler tick, and a wake context do not set `requeue_deferred`.
15. Requeue writes an audit row identifying the action and the rows
    cleared.
16. The advertised `max_batches` default and hard cap equal the
    constants (8 and 8) at all five sites listed in §5.3 — including the
    JSON schema's `"default": 20` and the registry description — and the
    "1000+ raw in ~20 calls" cost claim is corrected to match.

All implementation tests use temporary state only. The live substrate's
existing rows are fixtures for nothing and are not written to.

All 16 claims are discharged by `internal/core/compact_design_matrix_test.go`
(24 cases; §11.4). Claim 16's "five sites" is corrected to seven in
§11.2 — the matrix asserts the value at every site rather than counting
them, so it is insensitive to that correction.

---

## 11. Implementation record

Implemented across five commits (`7a5f5a5`, `5e59652`, `1b1c884`,
`bab0705`, `9864eae`) plus the Phase 10 matrix. Two decisions below
depart from the text above. Both were taken at implementation time under
the design's own stated intent, and both are now the contract rather than
a divergence from it.

### 11.1 Requeue is CLI-only, not a `compact_epistemology` parameter

§3.3 opens with "It is proposed as `compact_epistemology` gaining a
`requeue_deferred` parameter." **Not implemented as specified.** R1 says
"explicit operator action only" and R5 gives the reason: a requeue is a
*human overriding a machine decision*. An agent that could requeue could
undo a refusal on its own judgement — and a refusal is a considered
judgement, made because the model looked at the rows and declined.
Undoing it automatically puts the loop back, just slower and harder to
notice.

So requeue is `mpm compact requeue-deferred`, following the existing
`mpm capability grant-operator` precedent for an operator-only mutation.
The compact action's params schema is `additionalProperties: false`, so
the refusal is structural rather than a check somebody can forget to
write. `cmd/mpm/compact_cmds_test.go::TestRequeue_IsNotReachableFromTheMCPAction`
reads the **live** registry and fails if any alias of the parameter
appears in the compact branch's schema.

R2, R3, R4, and R5 are implemented as written on this surface.

**R5's audit row is not a supplement to the dispatch hook — it is the
only record.** This section originally claimed the `tool_invocations`
row "comes free from the existing dispatch hook." That was wrong, and
the CLI-only decision in §11.1 is what made it wrong. The dispatch hook
(`cmd/mpm/audit_hook.go`) fires only for calls arriving via `mpm call` or
the MCP server. Requeue is deliberately reachable from neither: the CLI
router (`cmd/mpm/router.go` → `handleCompact`) invokes it directly,
never passing through `recordToolInvocation`. On the one path requeue
actually takes, no `tool_invocations` row exists at all.

So `RequeueDeferred` writes the `system_audit_log` row itself, naming the
row ids and the count requeued — which is the part R5's rationale is
actually about. (It does not record `remaining_deferred`: that figure is
read after the commit, so including it would put a value in the trail
that no one could verify at the moment it was written.) An earlier
implementation wrote the row *after* the commit and treated a failure as
non-fatal. That is a best-effort trail, and it is not what R5 promises:
under an injected audit failure it reported success for a mutation whose
only forensic evidence was missing. The row now moves inside the
transaction, matching `PurgeWork` (`internal/core/work_purge.go`), which
already holds MPM to the same guarantee for the more destructive
administrative mutation. Pinned by
`TestRequeue_AuditFailureRollsBackTheMutation`.

### 11.2 Seven stale `max_batches` sites, not five

§5.5 enumerates five. Implementing the correction found **seven**. The
two extra are doc comments in `compact.go` that describe the drain's
capacity without naming `max_batches` on the same line, so the original
line-referenced table missed them. Both are corrected; no limit was
raised, per the design's own instruction.

### 11.3 What Phase 5 required

§6's progress invariant needed no new code — it is structural, once a
refused group is deferred and the selection predicate excludes deferred
rows. It is pinned by the matrix rather than implemented by a mechanism,
which is the outcome the design asked for.

### 11.4 The 24-case matrix

§10's 16 claims are discharged by `internal/core/compact_design_matrix_test.go`,
which asserts each claim directly rather than delegating to the
per-phase tests written alongside the code. That duplication is
intentional: the phase tests are where a regression will be debugged;
the matrix is where a reader can walk the design's claims top to bottom
and see each one discharged, and it would prove nothing on its own if it
only pointed at other test names.

Claim 16 is a source-text check by necessity — a test cannot observe a
doc comment. It is scoped to the `max_batches` neighbourhoods rather than
whole files, because `registry_list.go` and `handlers.go` legitimately
document other parameters that default to 20, and a file-wide grep would
report those as failures and teach a later reader to ignore the check.
