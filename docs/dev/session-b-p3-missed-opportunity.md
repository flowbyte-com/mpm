# Session B — P3: the retrospective missed-opportunity hunt

> **Author:** Session B (intelligence core). **Branch:** `feat/session-b-p3-missed-opportunity`.
> **Base:** `origin/main` = `9337f04` (P2 merged).
> **Authorised by:** `docs/dev/session-a-adjudication.md` §6c.2 limitation 3
> ("P3 — hunts that cite resolvable sources and stop using cycle tags") and
> the coordination ledger's P3 row.
>
> **No schema change. No new tool. No deployment. No merge.**
>
> **Scope discipline:** D1, D2 and D5 remain separately tracked and untouched.
> The critic is **not** rewritten, and the lesson-emitting hunts are **not**
> moved to a proposal surface — that is A's §6c.3 switch, and it needs the
> operator's product decision (§6c.2 limitation 4).

---

## 1. What P3 does

**Two things, and the second one is the more important.**

1. **A new hunt** — `MissedOpportunityHunt` — that finds agent sessions
   which did MPM-mediated work and recorded nothing, and emits **one pending
   proposal per session**, citing the session's tool invocations as typed
   evidence.
2. **The cycle tags are gone from every hunt.** A `cycle_<n>` tag is inert on
   the lesson surface and **fatal** on the proposal surface, so removing them
   now is what makes A's next step — moving the hunts to `mpm_theories
   propose` — safe.

The hunt is the only critic hunt that looks at **absent** knowledge. The
other three read `memories` and detect degradation of what is recorded.

---

## 2. The finding that shaped the design

### 2.1 The audit's measurement was wrong

`docs/dev/session-b-audit.md` §4.1 measured the
`tool_invocations` × `artifact_provenance` join on the live workspace and
reported:

```
ses_e8dea1636ffeF4t1WRV5QiOaSv | 12 invocations | 0 artifacts   <-- a real observed gap
```

**That was not a gap.** That session had made **8 successful
`mpm_decisions record` calls** and 3 handoff writes. It recorded plenty.

The join reported zero artifacts because:

> **`artifact_provenance.invocation_id` is populated ONLY by the `mpm_memory`
> save path.**

The per-call provenance bridge — the one place that overlays the
`ActiveContext`'s invocation id onto the effective provenance — exists only
in `internal/core/memory_tools.go:184`. Every other write path resolves its
provenance with an **empty** invocation id
(`dm.getEffectiveProvenance()` → `Resolve("", "", "", "")`), so its
`artifact_provenance` row cannot be joined back to the call that produced it.

Measured on the live workspace:

| `artifact_type` | provenance rows | rows that join to a `tool_invocations` row |
|---|---|---|
| `memory` | 53 | **49** |
| `work` | 3 | **3** |
| `lesson` | 3 | 1 |
| **`decision`** | **26** | **0** |

24 of the 26 decision rows carry an **empty** `invocation_id`; the other two
carry a *session* id. **A hunt built on the join alone reports a session
that recorded eight decisions as one that recorded nothing.**

This is pinned as a characterization of current behaviour in
`internal/critic/p3_missed_opportunity_test.go`
(`TestP3ArtifactProvenance_InvocationIDIsPopulatedOnlyByTheMemorySavePath`),
so the day someone fixes it, the test fails and the hunt's second signal can
be simplified.

### 2.2 The hunt therefore computes TWO signals, and requires BOTH to be absent

| | Signal | What it proves |
|---|---|---|
| **1** | the **join**: no `artifact_provenance` row is attributable to any of the session's invocations | the artifact exists and belongs to that exact call |
| **2** | the **tool log**: the session made no **successful call to a recording action** | the call persisted durable state |

Both are disqualifying when met. Signal 2 is what closes the hole signal 1
leaves.

**The recording vocabulary is declared, not inferred.** It lives in one map
(`recordingActions` in `internal/critic/missed_opportunity.go`), the hunt's
SQL predicate is **generated from it**, and a test pins it exactly. The
failure mode of an incomplete vocabulary is a **false gap** — a pending
proposal claiming a missed opportunity that did not happen — so the map must
be extended in the same change that adds a recording action. That asymmetry
is stated in the code.

Against the live workspace, the rule correctly finds **no candidates**: every
session with more than one invocation made at least one recording call.

---

## 3. The gap rule, and what the proposal actually asserts

`docs/dev/session-a-adjudication.md` §6b.2: *"Invocation gaps are candidate
opportunities, not proof."* A gap may be **reported**; it may never be
**cited**.

So:

* **The citations are the session's `tool_invocations` rows**, which exist
  and resolve as type `tool_invocation` (P2's typed path validates each one
  and rejects the whole proposal if one does not resolve). The gap is the
  **trigger**, never a citation.
* **Where the only thing available is a gap, there is no proposal at all.**
  `missedOpportunityProposal` returns `ok=false` and the hunt logs it as a
  finding for a human. The branch is unreachable from the current detection
  path — the hunt's candidates are derived from the very rows it would cite —
  so it is exercised directly by
  `TestP3MissedOpportunity_GapOnlyCandidateProducesNoProposal`.
* **The hypothesis asserts only what the cited invocations show.** It says
  the session did work whose invocations are cited here, that none of the
  cited calls recorded anything durable, and that no artifact is
  attributable to any of them. It explicitly says it **does not assert that
  any particular lesson exists** and is never promoted automatically.

The proposal is **pending**, at the base confidence, and the hunt never
promotes anything.

---

## 4. Idempotency — and a correction to the audit

The proposal must be a **pure function of its evidence**. Every
identity-bearing field is checked:

| Field | Identity-bearing? | Rule |
|---|---|---|
| `hypothesis` | **yes** (the content hash) | names the session; never a count |
| `tags` | **yes** (the identity hash folds `json(tags)`) | no cycle marker |
| `validation_criteria` | **yes** (it is in the metadata, and the identity hash folds the metadata) | no cycle marker |
| `source_ids` | **no** (citations are rows, not identity) | the session's oldest invocations, bounded |

### 4.1 The correction

`docs/dev/session-b-audit.md` §4.2 said:

> *"The cycle number, if wanted, belongs in `validation_criteria`, which is
> not part of the identity hash."*

**That was wrong.** `validation_criteria` IS part of the proposal's
metadata, and `MemoryIdentityHash` folds the metadata into the identity. It
is identity-bearing **twice over**, because `proposeTheory` also folds it
into the content.

Pinned by `internal/core/p3_identity_test.go`:

| Test | Pins |
|---|---|
| `TestP3Identity_ValidationCriteriaParticipatesInProposalIdentity` | two proposals differing only in `validation_criteria` are **two different proposals** |
| `TestP3Identity_CycleVaryingTagCreatesASecondProposal` | the D4 defect at the substrate level: a cycle-varying tag **does** create a second proposal |
| `TestP3Identity_IdenticalProposalIsIdempotent` | an identical proposal returns the **same id** on every submission |

### 4.2 What that means for D4

P3 fixes the **emission side** of D4: the critic no longer emits a
cycle-varying tag, so the same finding re-emitted in a later cycle lands on
the same proposal.

**The substrate half of D4 is untouched.** A caller that explicitly proposes
with `cycle_5` and `cycle_6` tags still gets two proposals, because the tags
are part of the identity hash. That is still pinned by
`internal/core/m5_trust_characterization_test.go`
(`TestM5Characterization_CycleTagBreaksProposalIdempotency`), and its
contract twin `TestM5Contract_CycleVaryingTagDoesNotCreateSecondProposal`
**still fails by design** behind the `m5contract` tag. Closing it would mean
changing the identity hash itself, which touches shared dedup semantics and
needs A's arbitration.

**The M5 suite's disposition for this PR:**

| File | Test | Disposition |
|---|---|---|
| `internal/critic/m5_trust_characterization_test.go` | `TestM5Characterization_CycleMarkerDefeatsProposalIdentity` | **inverted** — it pinned the critic's emission, which P3 changed |
| `internal/core/m5_trust_characterization_test.go` | `TestM5Characterization_CycleTagBreaksProposalIdempotency` | **unchanged** — it pins the substrate property, which P3 does not change |
| `internal/core/m5_trust_contract_test.go` | `TestM5Contract_CycleVaryingTagDoesNotCreateSecondProposal` | **unchanged** — still failing by design; the substrate property is still open |

The M5 contract suite still fails with **6 expected failures** (5 core + 1
critic) — the same count as before P3 — and the green gate guard
(`TestM5TrustGate_ContractTestsAreExcludedFromEveryGreenGate`) still passes.

---

## 5. The cycle tags, and why removing them is preparation, not cosmetics

A `cycle_<n>` tag is:

* **inert** on the lesson surface — `AddLesson` deduplicates on the content
  hash, so a differing tag is simply ignored on a repeat; and
* **fatal** on the proposal surface — the tags are part of the identity
  hash, so the tag makes every cycle a **distinct pending proposal**. That
  is D4, and it is what floods the substrate with indistinguishable pending
  theories and inflates the very evidence rows the substrate counts as
  corroboration.

So the tag is a trap that only bites when a hunt is moved to a proposal
surface — and **moving the hunts there is exactly what A's §6c.3 rules for
next**.

P3 therefore removes the cycle tag from **all three** hunts that carried one
(`survival_asymmetry`, `weak_theory`, `poison_pill`), and moves the cycle
number into the finding's **Reason**, which is logged and never persisted.
`Audit.Run` now logs the Reason on successful emission, so the cycle stays
observable in the audit trail.

**A structural guard prevents the regression:**
`TestP3NoHuntEmitsACycleTag` runs every finding-emitting hunt with data that
makes it fire, and asserts that no emitted finding carries a `cycle_` tag or
a cycle marker in its hypothesis.

### 5.1 The poison pill's second volatile value

The poison pill's hypothesis also embedded the target memory's
**confidence**. The confidence of a memory **decays between cycles**, so the
hypothesis text changed and produced a second proposal for the same target —
the same defect class, in the same payload. It is now in the Reason.

### 5.2 What was deliberately left alone

`StaleMemoryHunt` puts the cycle number in a **challenge's evidence
string**, not in a tag and not in a proposal. A challenge patches an
existing memory's metadata; it creates no new artifact, and the cycle
number there is genuine forensics — it records *when* the flag was raised.
Left unchanged, deliberately.

---

## 6. The proposal's shape

```jsonc
{
  "tool":   "mpm_theories",
  "action": "propose",
  "payload": {
    "hypothesis":          "Session <id> performed MPM-mediated work whose tool invocations are cited here. None of the cited calls recorded anything durable, and MPM attributed no artifact to any of them, so a knowledge-capture opportunity may exist for this session. This proposal asserts only what the cited invocations show: it does not assert that any particular lesson exists, it is not evidence that one does, and it is never promoted automatically.",
    "validation_criteria": "Before this proposal is promoted, a reviewer must determine what knowledge session <id> did not capture, and must cite that knowledge as resolvable evidence. Until then it stays pending at the base confidence: the absence of a record is a candidate opportunity, never evidence that a lesson exists.",
    "tags":                ["critic", "missed_opportunity", "session:<id>"],
    "source_ids":          ["<tool_invocations.id>", "..."],
    "source_type":         "tool_invocation"
  }
}
```

The `source_ids` are `tool_invocations.id` values — the column P2's
`tool_invocation` source type resolves against.

### 6.1 The citation set converges

The citations are the session's **oldest** invocations, bounded by
`MaxCitations` (default 200). So:

* the proposal's **identity never changes** as the session keeps working —
  the hypothesis, the tags and the validation criteria are untouched;
* the citations **accumulate** as evidence while the session works, then
  **converge** on a stable prefix;
* the newest activity is never cited, because a moving citation set would
  change the proposal's meaning on every cycle.

---

## 7. Tests

### 7.1 The hunt — `internal/critic/p3_missed_opportunity_test.go`

| Test | Covers |
|---|---|
| `SessionWithOnlyReadsIsACandidate` | the positive path; the citations are typed `tool_invocation` |
| `SessionThatRecordedAMemoryIsNotACandidate` | the ordinary guard (audit criterion B-3) |
| **`SessionThatRecordedADecisionIsNotACandidate`** | **the false-gap guard** — the join cannot see decisions, so the recording-call signal must |
| `SessionWithAnAttributedArtifactIsNotACandidate` | the join signal alone disqualifies |
| `FailedRecordingCallDoesNotDisqualify` | only a **successful** recording call counts |
| `SessionWithoutFrameworkSessionIDIsSkipped` | honest degradation — skipped at **detection**, not merely producing no finding |
| `SessionsSharingAnMPMSessionIDAreNotMerged` | two host sessions sharing one `mpm_session_id` are two candidates (§2.9) |
| `CitationsResolveAsToolInvocations` | every cited id resolves |
| `GapOnlyCandidateProducesNoProposal` | the gap rule's explicit case |
| `ProposalCarriesNoCycleMarker` | no cycle marker in the hypothesis, the criteria or the tags; the hypothesis does not assert a lesson exists |
| `ProposalIsStableAcrossRuns` | two runs produce byte-identical proposals |
| `CitationSetConvergesOnTheOldestPrefix` | the identity is unchanged by new activity; the citations accumulate |
| `CitationsAreBoundedAndConverge` | the bound holds; the set converges |
| `HuntWritesNothing` | the hunt is read-only |
| `RecordingActionVocabulary_IsDeclaredAndComplete` | the vocabulary is pinned |
| `RecordingActionPredicate_IsGeneratedFromTheVocabulary` | the SQL predicate cannot drift from the map |
| `IsRegisteredInTheDefaultCritic` | `critic.New` registers the hunt — an unimplemented hunt is a capability nobody has |
| `EndToEnd_CreatesOnePendingProposalAndNothingElse` | **audit criterion B-1**, through the production dispatch: exactly one pending proposal, no lesson/decision/memory created, not promoted, base confidence, typed citations |
| `EndToEnd_IsIdempotent` | **audit criteria B-2 and B-6**: the second run produces zero new proposals and zero new rows |
| `EndToEnd_RejectsAnUnresolvableCitation` | the atomic rejection: a vanished cited invocation rejects the proposal and leaves nothing |
| `EndToEnd_SessionThatRecordedIsNotReported` | the false-gap guard on the real schema |
| `ArtifactProvenance_InvocationIDIsPopulatedOnlyByTheMemorySavePath` | the substrate defect the hunt works around |
| `NoHuntEmitsACycleTag` | the structural guard |

### 7.2 The identity mechanism — `internal/core/p3_identity_test.go`

| Test | Covers |
|---|---|
| `ValidationCriteriaParticipatesInProposalIdentity` | `validation_criteria` is identity-bearing |
| `CycleVaryingTagCreatesASecondProposal` | D4 at the substrate level |
| `IdenticalProposalIsIdempotent` | the positive case the hunt relies on |

### 7.3 Non-vacuity — proven, not asserted

Six deliberate violations, each in a disposable copy of the tree:

| Violation | Result |
|---|---|
| the recording-call signal is disabled | **FAIL** — `expected 0 findings — the session recorded 8 decisions, got 1` |
| the join signal is disabled | **FAIL** — `expected 0 findings — an artifact is attributable to the session's invocations` |
| the poison pill's cycle tag is re-introduced | **FAIL** — `expected NO cycle_* tag at cycle 5` and the inverted M5 characterization fails too |
| the proposal's content is made volatile (the count in the hypothesis) | **FAIL** — `expected the hypothesis to be unchanged by new activity` |
| the hunt falls back to `mpm_session_id` instead of skipping | **FAIL** — `expected NO candidate session without a framework_session_id` |
| a cycle tag is re-introduced in the survival-asymmetry hunt | **FAIL** — `survival_asymmetry emitted the cycle tag "cycle_0"` |

The fourth proof found a real weakness in the first version of the test
suite: the degradation test originally asserted only that **no finding** was
produced, which a fallback that produced a candidate and then dropped it for
having nothing to cite would also satisfy. The test now asserts that the
session is rejected **at detection**.

### 7.4 Isolation

Every test pins `HOME`, `MPM_WORKSPACE` and `MPM_DB_PATH` to disposable
locations. The end-to-end tests open a **real** core `DatabaseManager` at
the canonical `<workspace>/src/db/mpm.db` — the path `MPM_DB_PATH` names — so
the production path resolution is exercised rather than bypassed. The
critic's own unit tests use the package's hermetic temp-file fixture.

---

## 8. Verification

| Gate | Result |
|---|---|
| `internal/critic` full suite | **ok** (1.2 s) |
| `internal/core` full suite | **ok** (205.6 s) |
| `internal/core/tools` full suite | **ok** (41.8 s) |
| Root module `go test ./...` (all packages) | **ok** — `cmd/mpm` 414.1 s, `cmd/mpm-mcp`, `cmd/mpm-critic`, `cmd/mpm-scheduler`, `cmd/mpm-telemetry`, `internal/scheduler`, `internal/telemetry`, `internal/testenv`, … |
| M5 contract suite (`-tags 'fts5 m5contract'`) | **FAIL — 6 expected failures**, the same count as before P3 |
| M5 gate guard `TestM5TrustGate_ContractTestsAreExcludedFromEveryGreenGate` | **ok** |
| `go vet -tags fts5` (root, core) | **clean** |
| `mpm-lint --gate` (the fatal-class gate) | **PASS** — mutex, sql, fd and imports thresholds all ok |
| `make test-scripts` | **ok** — 540 tests |
| `make test-agent-installation` | **ok** — 494 tests |
| `gofmt` on every file this PR touches | **clean** |
| Live database knowledge tables | **byte-identical** to the runbook baseline (27/116/85/2/61/5) |

---

## 9. What this PR does not do

* **No schema change.** No `SafeMigrations` entry.
* **No new tool.** The hunt emits through the existing `mpm_theories`.
* **The lesson-emitting hunts are NOT moved to a proposal surface.** That is
  A's §6c.3 switch, and §6c.2 limitation 4 makes it a **product decision**
  ("a proposal is not a lesson … its output becomes a queue item rather than
  a durable lesson"). It needs the operator.
* **The hunts do not cite resolvable sources yet** — except the poison pill,
  which now cites its target memory as `source_type='memory'`. The other two
  emit `mpm_lessons save`, which has **no `source_ids` parameter at all**;
  giving them citations requires the surface switch.
* **D1, D2 and D5 remain separately tracked.** D2 (the critic writing trusted
  lessons with no review) is unchanged and still pinned by the M5 contract
  suite. The P2b gate (#118) keeps the critic's automatic path off.
* **The substrate half of D4 is unchanged** — see §4.2.
* **The provenance defect in §2.1 is NOT fixed here.** It is recorded as
  **D6** for A (below), because fixing it changes provenance behaviour on
  every write path and is not required by P3.
* **No deployment, no merge.** Only Session A merges to `main` and deploys.

---

## 10. For Session A

| # | Item | What B needs |
|---|---|---|
| **A-4 / P3 row (approved)** | the missed-opportunity hunt | **Action:** review and merge. One new hunt in `internal/critic/`, no schema change, no new tool, no registry change. |
| **A-new-4** | **the hunt is registered in `critic.New` by DEFAULT**, so `mpm-critic` runs it. The critic's *automatic* path stays off because of P2b (#118), so it only runs when an operator or a reviewing agent runs `mpm-critic` deliberately. **B's recommendation:** keep it on by default — the hunt is pending-only, idempotent and non-promoting, and the P2b gate is the thing that protects the automatic path. If A prefers it opt-in behind a flag (like `--telemetry-hunt`), that is a one-line change. |
| **A-new-5** | **the recording-action vocabulary is a declared map** and must be extended whenever MPM gains a recording action. Its failure mode is a **false gap** — a pending proposal claiming a missed opportunity that did not happen. **B asks A to hold this invariant when adding write surfaces.** |
| **A-new-6** | **D6 — `artifact_provenance.invocation_id` is populated only by the `mpm_memory save` path.** Decisions (26/26 on the live workspace), theories and lessons record an artifact that **cannot be joined back to the call that produced it**. This is why the join alone reports false gaps, and why P3 needs a second signal. **B recommends tracking it as a separate defect** and fixing it in its own PR: it changes provenance behaviour on every write path and is not required by P3. When it is fixed, `TestP3ArtifactProvenance_InvocationIDIsPopulatedOnlyByTheMemorySavePath` fails and the hunt's second signal can be simplified. |
| **A-new-7** | **a latent, non-production silent drop:** `internal.ParseStringSliceOr` returns `nil` for a Go-native `[]string`, so an in-process caller passing `source_ids` as `[]string` gets **no citations at all** — silently. Production is unaffected (every payload crosses a JSON boundary first), but the P3 end-to-end tests had to mirror the `mpm call` JSON round-trip to avoid passing for the wrong reason. **B recommends tracking it separately** rather than changing a shared parse helper used by ~20 handlers in this PR. |
| **A-new-8** | **the next step, and it needs the operator:** move `survival_asymmetry` and `weak_theory` from `mpm_lessons save` to `mpm_theories propose` (§6c.3), so they can carry typed citations. §6c.2 limitation 4 makes that a **product decision**: the critic's output becomes a queue item rather than a durable lesson. P3 has made it **safe** — the cycle tags are gone, so a hunt on the proposal surface can no longer produce a distinct proposal per cycle. |
