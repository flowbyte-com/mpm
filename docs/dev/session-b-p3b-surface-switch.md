# Session B — P3b: the critic's trust-boundary transition

> **Author:** Session B (intelligence core). **Branch:** `feat/session-b-p3b-surface-switch`.
> **Base:** `origin/main` = `c7e2dc7` (P3 merged).
> **Authorised by:** `docs/dev/session-a-adjudication.md` §6c.3 and A-new-8
> ("the two lesson-emitting hunts must cite something **resolvable** — the
> artifact each observed, with its own declared type through P2's typed
> path").
>
> **No schema change. No new tool. No registry change. No deployment. No merge.**
>
> **Scope discipline:** the P2b scheduling gate (#118) is untouched — it
> stays shut. D6 (the provenance join), D1/D3/D4-substrate/D5 (duplicate
> confidence, unresolvable legacy citations, cycle-tagged proposals,
> repeated delivery) and the `[]string` silent drop are NOT combined here.

---

## 1. What P3b does, in one paragraph

The critic's last two automatic **trusted-lesson** writes are gone.
`SurvivalAsymmetryHunt` and `WeakTheoryHunt` now emit **`mpm_theories
propose`** — the only existing untrusted proposal surface — with **typed,
resolvable citations** through P2's validated path. Each proposal is
`metadata.status='pending'` until a reviewer resolves it; the critic never
promotes, never resolves, and never writes a lesson. The critic's
transition away from automatic trusted-lesson creation is complete.

The trust boundary, before and after:

| | before P3b | after P3b |
|---|---|---|
| **surface** | `mpm_lessons save` | `mpm_theories propose` |
| **trust** | trusted on arrival — `lessons_base` has no status column | pending — `metadata.status='pending'` |
| **retrievable as trusted knowledge** | yes, through `lessons_fts` | no |
| **evidence** | none (`fact` / `type` / `tags`) | typed, resolvable citations |
| **review step** | none | a reviewer resolves or dismisses the proposal |
| **automatic promotion** | n/a | never |

---

## 2. The one detection correction, and why it was unavoidable

### 2.1 What changed

The survival hunt's cohort query gained one predicate:

```sql
WHERE json_extract(metadata, '$.source') IS NOT NULL
  AND collection IN ('', 'memories')     -- NEW
```

Everything else is preserved byte-for-byte: the same `GROUP BY`, the same
`'call'`/`'direct'` pair, the same sample-size gate, the same
zero-survival guard, the same threshold comparison, the same ordering.

### 2.2 Why it was unavoidable

**The typed citation could never have resolved without it.** P2's
`'memory'` source type resolves against
`collection IN ('', 'memories') AND deleted_at IS NULL`
(`internal/core/citation.go`). An unscoped citation is **REJECTED**, and a
rejection leaves no proposal at all. So an unscoped hunt would have emitted
a finding that could never be applied.

**And the unscoped metric conflated two variables.** Measured on the live
workspace, the rows carrying a provenance `source` label are:

| collection | `source` | rows |
|---|---|---|
| `memories` | call | 33 |
| `agentshell` | call | 10 |
| `project` | call | 6 |
| `changelog` | call | 4 |
| `lessons` | call | 1 |
| `decisions` | cli | 2 |

Every one of those is stamped by the same save layer, so the provenance
label and the artifact KIND are two different variables — and the
unscoped aggregate mixed them. A project note, a changelog entry and a
memory have different lifecycles, so their survival curves are not
comparable. The prey claim is about **memories** ("call-sourced memories
survive at 0.158 while direct-sourced survive at 0.857"), so the cohort is
the memories collection.

### 2.3 The correction is behaviourally observable, and pinned

`TestP3b_SurvivalCohortIsScopedToTheMemoriesCollection` uses the
arithmetic to make the scope observable:

* memories-collection `call` cohort: 100 rows, 5 alive (0.05)
* memories-collection `direct` cohort: 50 rows, 49 alive (0.98)
* 200 healthy `project` rows stamped `call`

| | ratio | fires? |
|---|---|---|
| **scoped** (memories only) | 0.98 / 0.05 = **19.6** | **yes** |
| **unscoped** (+ 200 healthy) | 0.98 / 0.683 = **1.43** | **no** |

So an unscoped cohort **silently swallows the asymmetry** — which is
precisely the defect the scope corrects. The test asserts the hunt fires
*and* that the measured figures are the scoped ones
(`call=0.050 (100 rows)`).

**Non-vacuity:** removing the predicate makes this test fail
(`expected 1 finding — the non-memory rows must not enter the cohort, got
0 findings`), and removing it from the *citation* query makes the
citation test fail (`cited id "project-call" has collection "project" —
it would be REJECTED by P2's 'memory' source type`).

### 2.4 The weak-theory hunt's detection is untouched

Its query, floor, ordering and `LIMIT` are unchanged. Its result set is
now **cited** rather than asserted: the proposal cites the theory it
observed, with `source_type='theory'`.

One wording consequence, handled honestly: the old lesson said *"Pending
theory %s has confidence %.3f"*, but the query does not filter on
`metadata.status`, so a **resolved** theory below the floor would also be
flagged. The proposal therefore says *"the theory cited here"* and does
**not** assert the theory's status — the detection is preserved and the
wording stops making a claim the hunt did not check.

---

## 3. The proposals' shape

### 3.1 `survival_asymmetry`

```jsonc
{
  "hypothesis": "Memories recorded through the \"call\" source path may be subject to a survival disadvantage relative to those recorded \"direct\" (observed direct/call survival ratio band: 2x-4x). The cited memories are the surviving members of the two cohorts the critic measured; the members that did not survive are not citable, so this proposal does not assert the cause of the asymmetry. Whether decay, auto-archive, or a source-priority bias in the write path is responsible is unresolved.",
  "validation_criteria": "A reviewer must recompute the survival rate of the \"call\" and \"direct\" provenance cohorts over the memories collection, establish which mechanism drives the difference (decay, auto-archive, or a source-priority bias in the write path), and cite the specific rows or lifecycle events that establish it as resolvable evidence. Until then the proposal stays pending at the base confidence: an asymmetry between a provenance label and a lifecycle outcome is a correlation, not an established cause.",
  "tags": ["critic", "survival-asymmetry", "decay-investigation"],
  "source_ids": ["<memories.id>", "..."],
  "source_type": "memory"
}
```

**The citations are the cohort's oldest LIVE rows, bounded (5 per cohort).**

* **Live only** — required, not a preference: P2's typed path rejects a
  soft-deleted id, and this hunt fires *because* some of its rows are
  soft-deleted. The proposal says so explicitly: the members that did not
  survive are not citable.
* **Oldest first** — so the set is a stable **prefix**: as the cohort
  grows, the same rows are re-cited and the proposal's evidence does not
  churn. Pinned by
  `TestP3b_SurvivalCitationSetIsStableAsTheCohortGrows`.
* **Both cohorts** — the proposal rests on the *contrast*, and a contrast
  needs both sides.

### 3.2 `weak_theory`

```jsonc
{
  "hypothesis": "The theory cited here (<id>) is held at a confidence below the critic's floor: <excerpt> Whether that reflects a genuinely unsupported claim (it should be resolved as disproven) or merely thin corroboration (it should be supported with further evidence) is unresolved; this proposal asks a reviewer to determine which, and asserts neither outcome.",
  "validation_criteria": "A reviewer must read the cited theory <id> and decide whether it should be resolved as disproven — citing the evidence that contradicts it — or retained pending with further evidence gathered. Until that decision is made with resolvable evidence, the proposal stays pending at the base confidence: a theory's confidence is observation, not verdict.",
  "tags": ["critic", "weak-theory", "theory_<id>"],
  "source_ids": ["<theories.id>"],
  "source_type": "theory"
}
```

The theory's **id** is in the hypothesis and in the tags, so two different
weak theories produce two different proposals. The theory's **confidence**
is in neither — it decays, so it is a volatile value in an identity-bearing
field. It lives in the finding's `Reason`, which is logged and never
persisted. The excerpt is truncated on **runes**, not bytes, because it now
lands in the FTS-indexed content of a proposal.

---

## 4. Idempotency, and the ratio band

### 4.1 The rule

A proposal's identity is `MemoryIdentityHash(contentHash, tags, metadata)`
(`internal/core/db.go`), and the proposal's metadata carries
`status`, `validation_criteria` and `dependencies`. So **three** fields
decide it: the hypothesis (through the content hash), the tags, and the
validation criteria.

Every one of them is a pure function of the observation:

| field | volatile? | rule |
|---|---|---|
| `hypothesis` | the **ratio** was | carries the **band**, never the exact ratio |
| `validation_criteria` | no | fixed text |
| `tags` | no | no cycle marker, no count |
| `source_ids` | no | not identity-bearing; the oldest live prefix |

### 4.2 The ratio band, and why it exists

The ratio is a **live measurement** — rows age out of the cohorts as the
substrate decays — so it drifts from cycle to cycle. An exact ratio in the
hypothesis would therefore make every cycle a **distinct proposal**: the
D4 defect in a new place, and exactly what P3 removed from the tags.

`ratioBand` quantizes the ratio into a power-of-two band (`3.76 → "2x-4x"`,
`19.6 → "16x-32x"`). A band is stable under ordinary drift; only a
crossing changes the proposal's identity — and a crossing **is** a
materially different observation, which deserves its own proposal.

Pinned by `TestP3b_RatioBandIsStableUnderDrift` (a 0.5 % drift leaves the
band alone) and, end to end, by
`TestP3b_SurvivalProposalIsStableUnderObservationDrift` (one row ages out
of the cohort; the hypothesis, the validation criteria and the tags are
**unchanged**).

### 4.3 A changed observation still produces a distinct proposal

Idempotency must not become staleness.
`TestP3b_ChangedObservationCreatesADistinctProposal` proves both halves:

* **weak_theory** — two different weak theories → two different proposals.
* **survival_asymmetry** — the cohort ages from 25 % to 5 % alive, the
  ratio crosses a band, and the proposal becomes a **different** one.

---

## 5. The wake context now says "pending"

Adjudication §6c.2 limitation 2 states that a proposal on the proposal
surface *"would reach the agent's context immediately — labelled pending,
not promoted"*.

**That label did not exist.** The wake context's theory pointer carried an
id, a summary and a pointer URI — and nothing distinguished a pending
proposal from a resolved one. A consumer had to make a second round trip
to learn the difference.

`fetchRecentMemoriesByCollection` now emits the artifact's own
`metadata.status` when it has one:

```jsonc
{"id": "…", "summary": "…", "pointer": "mpm://theory/…", "created_at": "…", "status": "pending"}
```

The key is emitted **only** when the row carries a status, so collections
that have none (decisions use `invalidated` / `superseded` flags instead)
are unchanged on the wire. This is an additive key inside an existing
pointer map — not a tool, not a registry change, not a schema change.

---

## 6. The M5 suite, reconciled

| file | test | disposition |
|---|---|---|
| `internal/critic/m5_trust_contract_test.go` | `TestM5Contract_CriticFindingIsNotTrustedWithoutReview` | **DELETED** — the defect is fixed, and the file is deleted with it (the package had no other contract test) |
| `internal/critic/p3b_trust_boundary_test.go` | `TestP3b_CriticFindingIsNotTrustedWithoutReview` | **PROMOTED** — the same contract, now ordinary passing CI coverage |
| `internal/critic/m5_trust_characterization_test.go` | `TestM5Characterization_CriticEmitsTrustedLessonWithoutReview` | **DELETED** — the behaviour it characterised no longer exists, so keeping an inverted copy would only duplicate the promoted contract |
| `internal/core/m5_trust_characterization_test.go` | `TestM5Characterization_LessonSurfaceHasNoPendingState` | **UNCHANGED, doc updated** — the substrate half of D2 is still open: `lessons_base` still has no pending state. The critic simply no longer writes there. |

### 6.1 The gate guard had to become retirement-aware

`internal/core/m5_trust_gate_test.go` required **every** package that
hosts contract tests to carry a contract *file* under the `m5contract` tag.
Retiring the critic's only contract test therefore broke the guard — the
guard failed a **correct** fix, for the wrong reason.

The guard's own documentation already said the right thing: *"The
existence of at least one contract test file is not asserted … The
retirement procedure deletes a contract test when its defect is fixed or
accepted, so requiring a non-empty population would break the
retirement."* The **code** did not match the documentation. It now does:
the per-package requirement is **conditional on that package actually
declaring** a contract test, and the guard logs when it is skipped.

The guard's core invariant is unchanged — *every contract test is tagged,
no characterization test is tagged* — and the check that no green gate
can enable the `m5contract` tag is unchanged.

### 6.2 The contract-suite count

| | before P3b | after P3b |
|---|---|---|
| core contract failures | 5 | **5** |
| critic contract failures | 1 | **0** (retired) |
| **total** | 6 | **5** |

The remaining five are D1, D3 (×2), D4-substrate and D5 — all core-side,
all still open, all still excluded from every green gate by the guard.

---

## 7. Tests

`internal/critic/p3b_trust_boundary_test.go` — **19 tests** (14 top-level,
5 sub-tests), all green, all against a **real** core `DatabaseManager` in a
disposable workspace with `HOME`, `MPM_WORKSPACE` and `MPM_DB_PATH` pinned.

| test | proves |
|---|---|
| `SurvivalAsymmetry_EmitsPendingProposalNotTrustedLesson` | a pending theory, no lesson, nothing in `lessons_fts` |
| `WeakTheory_EmitsPendingProposalNotTrustedLesson` | the same, and the observed theory is **not** resolved |
| `CriticFindingIsNotTrustedWithoutReview` | the **promoted** M5 contract |
| `SurvivalAsymmetry_CitationsResolveAsMemories` | every cited id resolves as a memory, from both cohorts |
| `WeakTheory_CitationsResolveAsTheories` | the cited id **is** the observed theory, and resolves as a theory |
| `InvalidCitationRejectsTheProposalAtomically` | a bad citation rejects the proposal; **no** theory row, **no** citation row, **no** lesson (both hunts) |
| `RepeatingTheHuntDoesNotCreateAnotherProposal` | two cycles at different cycle numbers → identical payload, same id, one theory row, no duplicate citations (both hunts) |
| `ChangedObservationCreatesADistinctProposal` | a different observation → a different proposal (both hunts) |
| `RatioBandIsStableUnderDrift` | the band's boundaries, and that a 0.5 % drift leaves it alone |
| `SurvivalProposalIsStableUnderObservationDrift` | the identity-bearing fields survive observation drift |
| `SurvivalCitationSetIsStableAsTheCohortGrows` | the citation set is a stable prefix |
| `PendingProposalIsMarkedPendingInWakeContext` | the surfaced proposal carries `status: "pending"` |
| `ExistingDetectionBehaviourIsIntact` | the threshold, the sample-size gate, the missing-cohort gate, the zero-survival gate and the weak-theory floor all still behave exactly as before |
| `SurvivalCohortIsScopedToTheMemoriesCollection` | the one deliberate detection correction |
| `NoHuntTargetsTheTrustedLessonSurface` | the **structural guard** over the whole repertoire |

### 7.1 Non-vacuity — proven, not asserted

Nine deliberate violations, each in a disposable copy of the tree:

| violation | result |
|---|---|
| the cohort scope removed | **FAIL** — `expected 1 finding … got 0 findings` |
| the citation scope removed | **FAIL** — `cited id "project-call" has collection "project" — it would be REJECTED by P2's 'memory' source type` |
| the exact ratio put in the hypothesis | **FAIL** — `expected the hypothesis to carry the ratio band` |
| `weak_theory` reverted to `mpm_lessons save` | **FAIL** — `expected mpm_theories/propose, got mpm_lessons/save` **and** the structural guard fires |
| the weak-theory citation emptied | **FAIL** — `expected exactly 1 cited id, got []` |
| the wake-context `status` removed | **FAIL** — `expected the surfaced proposal to carry an explicit status` |
| P2's atomicity broken (validation moved after the commit) | **FAIL** — `expected NO theory row to survive the rejection, got 1` **and** `expected NO citation row to survive the rejection, got 11` |
| the cycle number put back in the hypothesis | **FAIL** — `survival_asymmetry put a cycle marker in its hypothesis` **and** `expected the two cycles to produce an IDENTICAL proposal` |
| the citation order reversed to newest-first | **FAIL** — `expected the citation set to stay a stable PREFIX of the cohort as it grew` |

The atomicity proof is the sharpest one: it moves P2's citation
validation **out** of the transaction, and the test then observes a
surviving theory row and eleven surviving citation rows. The assertion is
about the transaction, not about the error message.

---

## 8. The audit of every remaining critic output path

Every finding the critic can emit, after P3b:

| path | target | what it writes | trust |
|---|---|---|---|
| `survival_asymmetry` | `mpm_theories propose` | a pending proposal | **pending** ✓ |
| `weak_theory` | `mpm_theories propose` | a pending proposal | **pending** ✓ |
| `poison_pill` | `mpm_theories propose` | a pending proposal | **pending** ✓ |
| `missed_opportunity` (P3) | `mpm_theories propose` | a pending proposal | **pending** ✓ |
| `stale_memory` | `mpm_memory challenge` | a challenge on an existing memory, which creates a **pending** theory (`ChallengeMemoryWithTheory` sets `metadata.status='pending'`) | **pending** ✓ |

**There is no path in `internal/critic` that writes a trusted lesson.**
`TestP3b_NoHuntTargetsTheTrustedLessonSurface` makes that a permanent
property rather than a review outcome: it runs every hunt in the default
repertoire with data that makes each fire, and fails if any of them
targets `mpm_lessons`.

### 8.1 ONE EXCEPTION — the telemetry hunt

`cmd/mpm-critic/main.go`'s `--telemetry-hunt` path
(`runTelemetryHunt`) writes **trusted lessons**:

```go
lessonFn := func(ctx context.Context, payload map[string]any) error {
    cmd := exec.CommandContext(ctx, mpmBin, "call", "mpm_lessons", "--payload", string(b))
```

The payload comes from `telemetry.FindingLessonPayload` in the **mpm**
repository (`internal/telemetry/observe.go`), which renders
`{"action":"save","fact":…,"type":"observation","tags":[…]}` — a lesson
payload with a cycle tag.

**Why it is reported rather than fixed here:**

1. **It is not in the critic's repertoire.** It is opt-in behind
   `--telemetry-hunt` and is not registered in `critic.New`, so the
   structural guard above does not cover it — and it is not part of the
   default cycle.
2. **It is not evidence-bearing in the substrate's sense.** A telemetry
   finding is a token count from the *telemetry store*, not an MPM
   artifact. To become a proposal it would need a **resolvable** typed
   citation, and the natural one — the session's `tool_invocations` rows,
   citable as `tool_invocation` — is a design decision, not a mechanical
   conversion.
3. **It lives partly in another agent's active worktree.** The payload
   builder is in the mpm repository, where Session A is working on
   `feat/p5-lifecycle-event-ingest`. This PR does not touch it.
4. **The lesson payload also carries a cycle tag** (`critic_cycle_<n>`) —
   inert on the lesson surface, fatal on a proposal surface. So the
   conversion, when it happens, must drop it.

**Recommendation for Session A:** either schedule the telemetry hunt's
conversion with the same treatment P3b gave the other two hunts (pending
proposal + typed `tool_invocation` citations + no cycle tag), or record
it as an accepted exception to the trust boundary with the reasons above.
**B does not recommend leaving it unremarked.**

---

## 9. What P3b does NOT do

* **No schema change.** No `SafeMigrations` entry. `lessons_base` still has
  no pending state, and none is added.
* **No new tool. No registry change.** The proposals go through the
  existing `mpm_theories propose`, whose registry already publishes
  `source_type` from P2.
* **The legacy P2 citation path is untouched.** The legacy `source_ids`
  path still auto-resolves the type and does not validate — D3-b, as A
  ruled.
* **The P2b scheduling gate is untouched.** `kind=critic_audit` is still
  refused at the arming and dispatch boundaries (#118). **The gate stays
  shut until A explicitly approves lifting it**, and this PR does not
  request that.
* **D6 is not fixed here.** `artifact_provenance.invocation_id` is still
  populated only by the `mpm_memory save` path.
* **D1, D3, D4-substrate and D5 are not fixed here.** The legacy
  citation path still accepts unresolvable ids and still mislabels their
  type; a caller that explicitly proposes with a cycle-varying tag still
  gets two proposals; duplicate evidence still inflates confidence.
* **The `[]string` silent drop is not fixed here.**
  `internal.ParseStringSliceOr` still returns `nil` for a Go-native
  `[]string`. The P3b tests mirror the `mpm call` JSON round-trip so they
  do not pass for the wrong reason.
* **No deployment, no merge.**

---

## 10. The M5 defects that remain unresolved

| defect | state after P3b |
|---|---|
| **D1** — duplicate evidence inflates confidence | **OPEN** (core) |
| **D2** — the critic writes trusted lessons without review | **CLOSED on the critic side.** The SUBSTRATE half is **OPEN**: `lessons_base` still has no pending state, so any other writer still writes trusted knowledge on arrival. |
| **D3** — unresolvable citations are accepted and mislabelled | **OPEN on the legacy path** (core). The TYPED path is fixed (P2) and the critic uses it. |
| **D4** — a cycle-varying tag defeats proposal identity | **OPEN on the substrate** (core): the tags are still part of the identity hash. The critic's EMISSION side is closed (P3, P3b) — the critic no longer emits a cycle-varying value. |
| **D5** — repeated delivery counts as independent corroboration | **OPEN** (core) |
| **D6** — `artifact_provenance.invocation_id` is populated only by the `mpm_memory save` path | **OPEN** (tracked separately by A) |
| **D7** — `ParseStringSliceOr` silently drops a Go-native `[]string` | **OPEN** (tracked separately by A) |
| **the telemetry hunt's trusted-lesson write** | **OPEN** — see §8.1. Reported for A's decision. |

---

## 11. For Session A

| # | Item | What B needs |
|---|---|---|
| **A-new-8 (fulfilled)** | the two lesson-emitting hunts must cite something **resolvable**, with its own declared type, through P2's typed path | **Action:** review and merge. `survival_asymmetry` cites memories (`source_type='memory'`), `weak_theory` cites the theory it observed (`source_type='theory'`). Both resolve; an invalid citation rejects the proposal atomically. |
| **the P2b gate** | the gate is what protects the automatic path | **NOT requested to be lifted.** It stays shut. The critic's automatic path remains off; only a deliberate `mpm-critic` run exercises the new proposals. |
| **the telemetry hunt** | `cmd/mpm-critic --telemetry-hunt` still writes trusted lessons (§8.1) | **B needs a decision:** schedule the conversion, or record it as an accepted exception. It is not in the critic's default repertoire, and it is not in this PR. |
| **the gate guard change** | `internal/core/m5_trust_gate_test.go` now makes the per-package contract-file requirement conditional on that package declaring a contract test | **Review.** This is the guard's own documented intent made executable; the guard's core invariant and its "no green gate may enable the tag" check are unchanged. |
| **the wake-context `status` key** | `recent_theories` / `recent_decisions` pointers now carry `status` when the row has one | **Review.** Additive, emitted only when non-empty, and it is what makes adjudication §6c.2 limitation 2's "labelled pending" claim true. |
| **the detection correction** | the survival cohort is now scoped to `collection IN ('', 'memories')` | **Review.** It was unavoidable (the typed citation cannot resolve otherwise) and it is pinned by a test whose arithmetic makes the scope observable (§2.3). |
