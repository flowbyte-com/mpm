# Issue #92 — confidence decay: audit, defect, closure analysis

> **Scope.** An audit of MPM's confidence model and the bounded
> confidence-decay maintenance pass, a defect found in that pass's trust
> boundary, the fix, and the closure analysis for issue #92.
>
> **Nothing was changed in the live installation.** No service was
> restarted, no configuration touched, no MPM tool call was made against
> the live workspace, no deployment or merge was performed. The live
> database was inspected through an explicitly read-only connection.
>
> **Author:** Session B. **Date:** 2026-10-29.
> **Base revision:** `origin/main`. The branch was written against `e493dc07`
> and rebased onto `2ae3339a` (which merged PR #169, the issue-#164 write-path
> fix) after #169 landed during the work; the rebase was clean and the diff
> against main is unchanged in substance.

---

## 1. Verdict in one paragraph

Issue #92 is **implemented but not yet closed**. The bounded maintenance
pass that #92 asked for was merged in PR #95 (`f2c1ff4d`), which
delivered the scheduler trigger, the operator command, the per-artifact
transaction, the watermark, and the documented participant set. That work
was correct as far as it went, and the gap #92 describes — a stored
confidence that never moves because nothing recomputes it — is closed.

But the audit found a **material defect in the pass**: it silently
re-elevated a challenge-restored memory from the F7.1 floor of 0.5 to
0.783519310948, and it would have re-elevated a superseded decision from
0.5 to 0.5996 — in both cases with **no evidence added**, no operator
action, and no cascade. The same hole was in the operator's manual
recompute (`mpm ops confidence recompute`), which raised a *challenged*
memory from 0.5 to 0.800000000000. A maintenance pass that reverses a
recorded challenge is worse than no maintenance pass at all, so the
defect is fixed here before the feature is scheduled.

Two operator decisions are **not** made in this document, because they
are policy rather than mechanism: the default cadence and batch size
that #95 selected silently (§8), and whether the recurring operation
should be enabled at all in the live installation (§9).

---

## 2. The audit (requirement 1)

### 2.1 The formula

`internal/core/confidence.go` is pure Go with no DB access and an
explicit clock, so it is deterministic by construction:

```
confidence = sigmoid( log(initial/(1-initial)) + Σ strength×independence×e^(-μ·ageDays) − λ·tDays )
```

| Artifact type | Initial (`InitialConfidence`) | Decay λ per day | μ |
|---|---|---|---|
| memory | 0.8 | 0.01 | 0.005 |
| theory | 0.5 | 0.02 | 0.005 |
| decision | 0.6 | 0.001 | 0.005 |
| lesson | 0.7 | 0.003 | 0.005 |

`tDays` is measured from the most recent **positive** evidence, falling
back to the artifact's creation time when there is none. The spec's two
properties — *more positive evidence never decreases confidence; decay
never increases it* — hold because the evidence term is non-negative for
positive evidence and the decay term only subtracts.

### 2.2 The stored value and the ledger

`memories.confidence` (and `lessons.confidence`) is a **cache**. The
source of truth is the `evidence` ledger, and `confidence_history` is the
audit trail of every recomputation: `(artifact_id, artifact_type,
confidence, computed_at, evidence_count, trigger)`. The `trigger`
vocabulary is a CHECK-constrained enum; the maintenance pass writes
`decay_tick`.

Provenance-only evidence (`source_group = 'auto_capture'`, written by the
memory-insert trigger) is excluded from every confidence computation:
saving a claim does not make it more likely to be correct.

### 2.3 The recompute paths

`recomputeConfidenceAt` in `internal/core/evidence_store.go` is the
single body every recomputation goes through. Its callers were, before
this change:

| Caller | Reason | Transactional? |
|---|---|---|
| `AddEvidence` | `evidence_added` | yes (`WithTx`) |
| `mpm ops confidence recompute` | `manual_recompute` | **no** |
| `mpm_confidence` action=`recompute` | `manual_recompute` | **no** |
| the decay pass (PR #95) | `decay_tick` | yes (`WithTx`) |

Two consequences of the "no transaction" fact were load-bearing for this
work: the **upward** cascade hook only fires for a transactional
recompute (`node.Tx() != nil`), so the operator's manual recompute never
emits a `foundation_proven` event at all; and the **downward** cascade
does fire from the maintenance pass, which is the inherited behaviour.

### 2.4 What was missing

Before PR #95, nothing recomputed on a schedule. `mpm ops gc` decays
**weight**, not confidence. `idle_dream` is referenced in comments but
has no implementation in this tree. So a stored confidence was a snapshot
from the last evidence event, and it diverged from the formula
indefinitely — the divergence #92 reports.

---

## 3. Eligibility (requirement 2)

| Population | In the pass? | Why |
|---|---|---|
| `memories` (collection `memories`, `''`, legacy) | **yes** | artifact type `memory` |
| `theories` | **yes** | artifact type `theory`; `status='proven'` is excluded (see below) |
| `decisions` | **yes** | artifact type `decision` |
| `lessons_base` | **no** | the watermark lives on `memories`; `lessons_base` has no `metadata` column either, so a lesson cannot carry the trust-boundary state the guard reads. Their cached confidence is not consulted by any gate. Adding a second watermark surface is a follow-up, not a silent omission — the CLI and the scheduler both report exactly which kinds they cover. |
| directives (`is_prime_directive`) | no | standing policy, not an artifact |
| retired, soft-deleted rows | no | terminal states |
| `status='challenged'` | no | a challenge pins the confidence to the floor and expires the evidence (F7.1) |
| `status='proven'` | no | an operator-asserted terminal state; decaying it would walk the cached value down to the invalidation threshold and enqueue a `confidence_floor` cascade that invalidates every downstream artifact citing a theory the operator declared sound |

The participant query is one definition read by both the backlog count
and the page query, so the reported backlog cannot drift from the rows
the pass actually scans.

---

## 4. The pass (requirements 3, 5, 8, 9)

`internal/core/confidence_maintenance.go` · `RunConfidenceDecay`.

* **Bounded.** At most `Limit` artifacts per invocation (default 100,
  hard cap 1000). `Due` and `Deferred` are reported, so an operator sees
  the backlog instead of a partial pass silently looking complete.
* **Restartable.** Each artifact is recomputed and its
  `confidence_checked_at` watermark advanced in **one** transaction. A
  failure rolls that artifact back, leaves the watermark untouched, and
  does not prevent the rest of the batch from making progress. The failed
  artifact is therefore still due and is retried on the next pass.
* **Idempotent.** The watermark, not the clock, is what stops an
  artifact being re-selected: a second pass at the same instant finds
  nothing due. An unchanged result advances the watermark but appends
  **no** `confidence_history` row, so the ledger grows in proportion to
  the artifacts that actually moved.
* **Deterministic.** `Now` is injectable, and every computation takes the
  clock explicitly. Two passes at the same `Now` produce the same values.
* **Cancellable.** `ConfidenceDecayOptions.Ctx` is observed at the
  **artifact boundary**: the artifact in flight completes, the ones not
  reached keep their old watermark and stay due. A cancelled pass does
  **not** advance the cadence marker — gating an unfinished backlog
  behind a six-hour interval would hide it for no reason.

### 4.1 Scan cost

The participant query's plan is:

```
SEARCH memories USING INDEX idx_memories_deleted (deleted_at=?)
```

It is O(live rows) per pass, not O(due rows): the `CASE` expression over
`confidence_checked_at`/`created_at` is not indexable, so
`idx_memories_confidence_checked` cannot be used for the selection. That
is acceptable because the pass is cadence-gated (§7) — it runs at most
once per interval, not once per scheduler tick — and the work it then
does is bounded by `Limit`.

The measured cost of a 100-artifact pass on an isolated database is
**24–55 ms**, dominated by the per-artifact transaction rather than by
the scan. The live participant set is 91 rows.

### 4.2 Write amplification

One changed artifact costs exactly: one `memories.confidence` update,
one `memories.confidence_checked_at` update, and one
`confidence_history` row — all in one transaction. One unchanged artifact
costs one `confidence_checked_at` update and nothing else. The history
ledger is the unbounded table, and it grows only in proportion to the
artifacts whose value actually moved.

---

## 5. Dry run (requirement 4)

`mpm ops confidence decay --dry-run` computes the same thing the pass
would: the same evidence loader, the same formula, the same clock, and
**the same trust-boundary guard**. It writes nothing — no confidence
update, no watermark, no history row.

The guard has to be in the preview, or the preview would report
"changed" for an artifact the pass cannot move, and the operator would
be looking at a wrong backlog.

---

## 6. The trust-boundary defect (requirement 7, 10)

### 6.1 What was wrong

F7.1 is explicit about the challenge cycle: a challenged memory's
confidence is **pinned** to `ChallengedMemoryConfidenceFloor` (0.5) and
its evidence rows are **expired**, so that fresh evidence is the only
thing that can re-elevate it. Restoration resets to the floor rather
than re-promoting the pre-challenge value.

The decay pass already refused to touch a memory whose metadata status
is `challenged`. But **the restore clears that status** — deliberately,
so the memory can be worked with again — and leaves the floor in place.
A restored memory is therefore a **participant** of the pass, and the
pass recomputes it from its empty (all-expired) evidence set. The formula
then derives the *initial* confidence, which is higher than the floor.

Measured, on a memory restored ten days after its creation:

```
restored memory confidence = 0.783519310948 after the decay pass,
want it unchanged at the F7.1 floor 0.500000000000
```

with no evidence added, no operator action, and no cascade. A memory
that was explicitly challenged and explicitly restored was silently
promoted to near-maximal confidence by a background maintenance pass.

The same hole was in the **operator's manual recompute**, which is worse
because it is the tool an operator reaches for:

```
challenged memory confidence = 0.800000000000 after a manual recompute,
want it unchanged at the floor 0.500000000000
```

That silently reversed a recorded challenge — and the value it wrote is
the *initial* confidence, derived from the memory's now-empty ledger,
which is exactly the value F7.1 exists to withhold.

(The measured digits depend on one subtlety worth recording: the
challenge neutralises evidence by setting `expires_at = now` at
one-second granularity, and the loader treats a row as expired only once
`expires_at < now`. A recompute in the *same second* as the challenge
therefore still sees the neutralised evidence and derives the
pre-challenge value — 0.879350543202 in the first measurement — instead
of the initial one. The regression test pins the expiry into the past so
the number it reports is the same on every run.)

A second, different demotion had the same shape. F5-2 drops a superseded
or invalidated decision to `DecisionInitialConfidence` (0.5) with the
documented contract *"the artifact is at 'no evidence' until fresh
evidence arrives"*. The live installation contains exactly such a row:

| | |
|---|---|
| id | `bdd6e161a89c1dcf` |
| collection | `decisions` |
| stored confidence | 0.5 |
| derived confidence | 0.5996 |
| live evidence rows | 0 |
| `confidence_history` | one row, `trigger=supersede`, `confidence=0.5`, `evidence_count=0` |

A first deploy of the pass would have raised it to 0.5996 — undoing a
recorded supersede — inside one cadence window.

### 6.2 The fix

One guard, at the layer that owns the invariant: `recomputeConfidenceAt`
in `internal/core/evidence_store.go`. Because it lives there, it covers
the decay pass, the manual recompute, and any future recompute path at
once.

```
A recompute may not RAISE a confidence value unless the raise is justified.

1. No recompute except an evidence event may UNDO A TRUST DEMOTION.
   Trust-demoted = metadata.status='challenged', or
                   metadata.restored_from_challenge, or
                   metadata.superseded, or
                   metadata.invalidated.
2. The maintenance pass may not promote an artifact that has NO LIVE
   EVIDENCE. The formula's value for such an artifact is the
   absence-of-evidence baseline (the per-type initial confidence, decayed
   from creation), not a statement about support.
3. The maintenance pass never triggers the upward "foundation proven"
   crossing. "This foundation is now solid" is a statement about
   evidence, and the pass creates none.
```

Three deliberate asymmetries:

* **A raise is clamped; a decay is still applied.** Lower confidence is
  the conservative direction, and the formula's whole purpose is to move
  it down. Freezing the value instead would reintroduce exactly the
  stored/derived divergence this pass exists to remove.
* **An evidence event is exempt.** Fresh evidence is precisely what
  re-elevates a demoted artifact (F7.1, F5-2). Without the exemption the
  challenge cycle would be permanent.
* **The operator's manual recompute is not bound by rule 2.** It is a
  repair tool: an artifact whose cached value disagrees with what the
  formula derives is exactly what the operator is trying to fix. It *is*
  bound by rule 1 — a recompute is a re-derivation from evidence, not an
  un-challenge.

The fast path costs nothing: a raise is only possible when the derived
value exceeds the stored one, and rule 2 is decided from the evidence set
the recompute has already loaded. The metadata read happens only for the
rare artifact that survives both fast paths.

### 6.3 Verification (requirement 10)

Every clause is pinned by a test, and each clause was shown to be
load-bearing by disabling it and observing the failure:

| Mutation | Test that fails |
|---|---|
| whole guard disabled | `RestoredMemoryKeepsItsFloor` (0.783519310948), `DryRunAgreesWithThePassOnAPinnedArtifact`, `ManualRecomputeDoesNotRaiseAChallengedMemory` (0.800000000000), `DecayPassDoesNotPromoteASupersededDecision` (0.599519904141), `ManualRecomputeDoesNotRaiseASupersededDecision` (0.600000000000), `PassDoesNotPromoteAnArtifactWithNoLiveEvidence` |
| rule 1 disabled | `ManualRecomputeDoesNotRaiseAChallengedMemory`, `ManualRecomputeDoesNotRaiseASupersededDecision`, `ManualRecomputeDoesNotRaiseAnInvalidatedDecision`, `DecayPassDoesNotPromoteAnInvalidatedDecisionWithLiveEvidence` |
| rule 2 disabled | `PassDoesNotPromoteAnArtifactWithNoLiveEvidence` |
| rule 3 disabled | `PassNeverDeclaresAFoundationProven` |
| evidence-event exemption disabled | `RestoredMemoryIsNotDeclaredProven`, `FreshEvidenceReElevatesAChallengedMemory` |
| the dry-run preview stops applying the guard | `DryRunAgreesWithThePassOnAPinnedArtifact` |

The whole table was re-run independently on the merged tree (main at
`f38a36ea`) by Session A on 2026-10-29, and every row reproduced
exactly: each mutation broke exactly the named tests and nothing else.
The last row — the dry-run preview — was added by that re-run, because
the dry run applies the guard on a separate code path
(`confidenceDecayWouldChange`) from the pass itself
(`recomputeConfidenceAt`). A preview that reported "changed" for an
artifact the real pass would leave alone would misreport the backlog.

### 6.3.1 The fourth demotion was untested, and the shape that exposed it

The guard names **four** trust demotions — challenge,
restore-from-challenge, supersede and invalidate — and this table
originally exercised only three. The invalidate demotion had no test at
all, which left a quarter of the "four demotions" claim unproven.

Adding the missing test then exposed a subtlety worth recording,
because it is the difference between a demotion being protected by its
MARKER and being protected by an accident:

* A **challenge** neutralises the memory's evidence rows (`expires_at =
  now`). A challenged or restored memory therefore has **no live
  evidence**, and the *absence-of-evidence* rule (rule 2) already
  refuses to promote it.
* An **invalidation** does **not** touch the evidence table. The ledger
  rows stay **live**.

So an invalidated decision is the one demoted artifact where rule 2
cannot protect it: the derived value is justified by real evidence, so
only the demotion marker itself can hold the value down. Two tests now
cover it — one with an empty ledger (protected by rule 2) and one with
live evidence (protected by rule 1 alone, and failing when rule 1 is
disabled).

The generalisation: **for the pass path, the challenge-family demotions
are protected twice over, and the invalidate demotion only once.** A
future demotion that leaves its evidence live would need rule 1 to be
load-bearing, and rule 1 is exactly the clause that had no test.

The two cascade assertions carry their own **negative controls**: a
fixture that registers a downstream `assumes_false` citation and then
shows that a *non-maintenance* recompute of the same shape **does** emit
the event. Without that, an assertion of "no intent was created" would
hold for the wrong reason — no downstream target — and would prove
nothing.

Requirement 10 asks that maintenance not promote uncertain findings into
trusted lessons. The guard answers it in three parts:

* the pass never writes a lesson, and never raises a trust-demoted
  artifact;
* the pass never triggers the upward `foundation_proven` crossing, so it
  cannot declare a pending theory proven;
* the pass creates no evidence rows, invents no citations, and approves
  nothing. Its only outputs are a confidence value, a watermark, and a
  history row.

---

## 7. Scheduler invocation, cadence and limits (requirement 8)

`internal/scheduler/confidence_decay.go` registers one tick handler,
`confidence_decay`, wired in `cmd/mpm-scheduler/main.go`.

| Key | Default | Meaning |
|---|---|---|
| `confidence.decay.enabled` | 1 | on/off |
| `confidence.decay.interval_hours` | 6 | minimum time between passes |
| `confidence.decay.limit` | 100 | artifacts per pass |
| `confidence.decay.min_age_hours` | 24 | artifacts younger than this (or checked more recently) are skipped |

The scheduler ticks every 60 s by default; the cadence gate makes the
pass run at most once per `interval_hours`. The last-run marker advances
even on a partial pass (the failed artifacts keep their watermark and
are retried at the next scheduled run, not on every tick) — **except**
when the pass was cancelled, which leaves the marker alone so the next
tick can resume the backlog.

The handler's defaults are pinned against the core's documented defaults
by a test, because they live in two packages and can drift.

---

## 8. The operator policy question — ANSWERED

**PR #95 selected these defaults silently.** `enabled=1`,
`interval_hours=6`, `limit=100`, `min_age_hours=24` were written into the
handler without an operator decision recorded anywhere. That is a policy
choice presented as a default, and it deserves an explicit owner.

**The operator answered (2026-10-29).** The approved policy is:

| key | value |
|---|---|
| `confidence.decay.enabled` | **0** — off by default, explicit opt-in only |
| `confidence.decay.interval_hours` | **6** |
| `confidence.decay.limit` | **100** |
| `confidence.decay.min_age_hours` | **24** |

The reasoning behind each of the three questions that were open:

1. **Enabled by default?** **No.** A substrate that decays confidence
   unattended changes the value a retrieval ranking sees, without anyone
   asking. The `enabled=0` default with an explicit opt-in is the
   conservative choice, and it is now the shipped behaviour.
2. **Interval?** **Six hours.** A stored confidence can be up to six hours
   stale; a shorter interval costs proportionally more passes for a
   smaller staleness window.
3. **`min_age_hours`?** **24 hours.** Freshly written artifacts stay out
   of the pass, on the grounds that evidence may legitimately arrive
   minutes after the save. A shorter window starts decaying artifacts
   before their evidence has settled.

**What this changed in the code.** The `enabled` fallback flipped from
`1` to `0`; the other three numbers were already the approved values.
The four now live in `internal/core` as
`DefaultConfidenceDecayEnabled` / `DefaultConfidenceDecayInterval` /
`DefaultConfidenceDecayLimit` / `DefaultConfidenceDecayMinAge`, and the
scheduler reads them from there — so the code, the SPEC and the tests
cannot drift apart.

**What an upgrade changes, precisely.** Nothing is written to
`system_config` by the upgrade, so nothing can be clobbered:

- an install with **no** `confidence.decay.enabled` row never chose a
  value, and therefore starts **off** after the upgrade. That is the
  intended consequence — the old default was selected silently, so there
  is no operator intent to preserve;
- an install with an **explicit** `1` keeps running the pass;
- an install with an **explicit** `0` keeps not running it.

The live installation falls in the first case: its `system_config` has
**no `confidence.*` keys** (§9). So the upgrade leaves it exactly as it
is — off, and with no decay handler in the live binary at all.

The operation stays **off** in the live installation — which it already
is (§9), and which the new default makes the *shipped* behaviour rather
than an accident of an old binary predating the wiring.

---

## 9. Live state (read-only)

| Fact | Value |
|---|---|
| Live binaries | `~/.mpm/bin/*` built **2026-10-24 15:14** — they **predate PR #95** (merged 2026-10-26 12:20) |
| Live scheduler binary, checked directly | `strings ~/.mpm/bin/mpm-scheduler \| grep -c confidence_decay` → **0**. The live scheduler has **no** decay handler, so the recurring operation is **not** running |
| Live scheduler | `mpm-scheduler.service`, `--interval 60s`, started 2026-10-28 16:50, `NRestarts=0` |
| Live `confidence_decay` handler | **absent** — the live scheduler binary predates the wiring, so the recurring operation is **not** running |
| Live `system_config` | 1 row total, **no `confidence.*` keys** |
| Live `system_audit_log` / `scheduled_wakes` / `scheduled_tasks` | 11 / 6 / 1 rows — unchanged by this work |
| Live memories | 119 rows, 91 in the decay participant set |
| Live memories **due** on a first deploy (min_age 24 h) | **72** |

What a first deploy of the pass would do to the live substrate, computed
from the live ledger with the guard in place:

| | |
|---|---|
| would move **down** | **71** |
| would move **up** | **0** |
| **raise blocked by the guard** | **1** — `bdd6e161a89c1dcf`, the superseded decision |
| artifacts with live evidence | 0 |
| invalidation crossings (down past 0.3) | 0 |
| proven crossings (up past 0.8) | 0 |

So the live blast radius of enabling the pass is: **71 cached confidence
values decay, nothing is promoted, and no cascade fires.** Without the
guard, one decision would have been promoted.

---

## 10. Does this close #92?

**The implementation gap is closed, and the scheduling decision has now
been made (2026-10-29). Both conditions for closure are therefore met.**

*Why the implementation gap is closed.* The stored confidence now moves
on a schedule, through the authoritative recompute path, in a bounded,
restartable, idempotent, cancellable and deterministic pass, with a
dry-run that matches it exactly. A stored value and the value the
formula derives agree again — except where a trust demotion deliberately
set a different value, which is the point of a demotion.

*Why the issue should stay open.* #92 is titled *"time-based decay is
modelled but no pass ever recomputes it"*. The pass exists, but in the
live installation it **does not run**, and the reason it does not run is
that #95's defaults were never an operator decision. Closing an issue
about absent scheduled maintenance while the scheduling is still an
undeclared default would record a resolution the live system does not
have.

*Recommended closure.* Close #92 when, and only when:

1. the operator has chosen the cadence, the batch size, the minimum age
   and the enabled/disabled default (§8), and
2. the choice is enabled in the live installation **or** the operator
   has decided the substrate will not run it unattended.

The recommendation accompanying this work is **enabled=0 by default**,
with the pass available as an explicit operator action
(`mpm ops confidence decay`) and an opt-in for the scheduler handler. A
substrate whose cached confidence decays unattended changes what
retrieval sees, and that is a product decision rather than a bug fix.

### 10.1 The conditions are now met

**Condition 1 — the operator has chosen.** The policy is decided and
recorded in §8 and in SPEC §6.2: `enabled=0`, `interval_hours=6`,
`limit=100`, `min_age_hours=24`. The `enabled` fallback was flipped
from `1` to `0`, the four numbers moved into `internal/core` so the
code, the documentation and the tests cannot drift, and the scheduler
now reads them from there.

**Condition 2 — the substrate will not run it unattended.** The
operator's decision is `enabled=0`, so the substrate will not run it
unattended. The live installation is unchanged and stays off: its
`system_config` has no `confidence.*` keys, and its scheduler binary
predates the wiring entirely (§9).

*What the upgrade does to a live install.* Nothing is written to
`system_config`, so nothing can be clobbered. An install with no
`confidence.decay.enabled` row starts off; an install with an explicit
`1` keeps running; an install with an explicit `0` keeps not running.
The live installation is in the first case, so the upgrade changes
nothing there.

*Installed-runtime verification is a separate activity.* The live
binaries still predate both the pass and this correction. Verifying the
corrected behaviour against the installed runtime is a release activity
with its own operator checklist, and it is **not** part of closing
#92: the issue asks whether the mechanism exists and whether the
scheduling is decided, and both now have answers in code.

---

## 11. Release risk

| Risk | Severity | Assessment |
|---|---|---|
| The pass promotes an uncertain finding | **fixed** | the guard, with per-clause mutation evidence (§6.3) |
| The pass declares a foundation proven / couples to critic arming | **fixed** | rule 3; the downward invalidation cascade is inherited unchanged |
| The pass reverses a recorded challenge or supersede | **fixed** | rule 1; measured live candidate `bdd6e161a89c1dcf` |
| A first deploy changes 71 live confidence values | **low** | all moves are downward; 0 promotions; 0 cascade crossings; the values were stale snapshots the formula had already moved past |
| The pass is expensive on a large substrate | **low** | O(live rows) scan, cadence-gated; 24–55 ms per 100 artifacts; 91 live rows |
| The history ledger grows without bound | **none** | unchanged artifacts write no history row; one row per changed artifact |
| The operator's manual recompute becomes less capable | **low, deliberate** | it can no longer undo a recorded demotion; it can still repair a stale cache, including one with no live evidence |
| The operator's manual recompute loses the cascade hooks | **pre-existing** | it was never transactional, so it never fired them; unchanged by this work |
| The `enabled` default flips to 0, so an install that was running the pass silently stops | **intended** | the old default was selected silently, with no operator decision recorded; an install that *chose* a value keeps it (§8) |
| An install loses a setting during the upgrade | **none** | the upgrade writes nothing to `system_config`; it reads a key, it does not seed it |

**Live risk of this change: none.** The change is inert until a binary
containing it is deployed, and — with the approved policy — deploying it
does not enable the recurring operation at all. The pass is off by
default; the operator opts in explicitly.

---

## 12. Where the facts live

| Fact | Document |
|---|---|
| the decay-maintained contract, the trust boundary, the cancellable pass | `docs/SPEC.md` §6.2 |
| the F7.1 floor and its ledger | `docs/SPEC.md` §4.5 |
| the F5-2 supersession demotion | `docs/SPEC.md` §4.3 |
| the pass's contract, in code | `internal/core/confidence_maintenance.go` |
| the guard, in code | `internal/core/evidence_store.go` |
| the scheduler trigger | `internal/scheduler/confidence_decay.go` |
