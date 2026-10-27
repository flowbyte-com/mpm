# Session B — D4: proposal identity, and what a proposal's identity *is*

**Branch:** `feat/session-b-d4-proposal-identity` (from `origin/main` = `3abfdda`)
**Status:** D4 is **closed in its production form** — a periodic reviewer re-finding the same
thing does not accumulate a pending proposal every cycle — and that is now **proven against
the real production path**, including across a process restart. **Two substrate-level
residuals remain and need Session A's ruling** (§5, §6). Neither is corrected here.
**Scope:** the proposal identity model, the production emitters, and the adversarial
regression matrix. No schema change, no migration, no registry change, no new tool, no
change to the shared identity algorithm, and **no M5 regression test weakened, inverted or
deleted**. D1, D2, D3, D5, D6, D7 and the P2b scheduler gate are untouched; interactions
are reported in §9.

---

## 1. Summary

The D4 defect is stated as: *"Repeated critic cycles generate duplicate theories because of
cycle-specific tags."*

**That production defect is closed.** The correction was the emission-side fix Session A
ruled for (decision A-4): the critic's hunts moved every volatile value out of every
identity-bearing field and into the finding's `Reason`, which is logged and never persisted.
That correction landed with Session B's P3 surface switch.

What was missing was the **proof** on the production path. The M5 contract test for D4
asserted a *synthetic* property — that a hand-written `cycle_5` tag must not split a
proposal — which no production emitter produces any more. This PR adds the proof that
D4 actually means:

> **A periodic reviewer re-finding the same thing must not accumulate a new pending
> proposal every cycle.**

proved end to end — the real hunt, at two different cycle numbers, through the real
`mpm_theories propose` dispatch — and proved again across a **process restart**.

The investigation also produced **two findings the contract suite did not name**, both
reported in §5 and §6 rather than fixed:

| # | Finding | Severity |
|---|---|---|
| **F1** | The proposal's identity treats the tag SET as a **sequence**. `["critic","alpha"]` and `["alpha","critic"]` are two different proposals. | low — cosmetic duplication |
| **F2** | **Concurrent proposals are lost.** Eight concurrent distinct proposals lose 2–4 of them to `SQLITE_BUSY`. | **medium** — a proposal the caller asked for is not created |

F2 is the more consequential of the two, and §6 carries its measured mechanism, the
experiment that isolates it, and the validated candidate fix.

---

## 2. The identity model, as the substrate states it

A proposal's identity is the SHA-256 of its content hash, its tags and its stable metadata
(`internal/core/db.go`, `MemoryIdentityHash`):

```go
h := sha256.Sum256([]byte(contentHashHex + "\x00" + string(tj) + "\x00" + string(mj)))
```

For a proposal (`internal/core/epistemology_tools.go`, `proposeTheory`):

| Input | Reaches the hash as | Identity-bearing? |
|---|---|---|
| `hypothesis` | the content, and hence the content hash | **yes** |
| `validation_criteria` | the content **and** the metadata — **twice** | **yes** |
| `dependencies` | the metadata | **yes** |
| `status` | the metadata (`"pending"`) | **yes** (frozen at insert) |
| `tags` | `json(tags)` | **yes** |
| the critic's cycle number | — | **no** (it lives in `Reason`) |
| the critic's measured ratios | — | **no** (quantised into a band, or in `Reason`) |

The substrate says this out loud, twice:

> *the hypothesis, the tags and the validation criteria ARE the proposal's identity (a
> cycle-varying value in any of them is a new proposal every cycle)* —
> `internal/critic/audit.go`

> *the content hash, tags, and STABLE metadata* — `internal/core/db.go`

So the model is **deliberate and documented**: four fields decide whether two proposals are
the same proposal.

### 2.1 Which fields are substantive, and which are incidental

The brief asks for this explicitly, and warns: *"Do not assume every metadata difference is
insignificant."*

| Field | Substantive? | Why |
|---|---|---|
| `hypothesis` | **yes** | the claim. Two different claims are two different proposals — full stop. |
| `validation_criteria` | **yes** | *how the claim must be checked*. A proposal that asks for a decay event and a proposal that asks for an archive event are different requests, and the substrate's own hunts depend on this being true (`internal/core/p3_identity_test.go` pins it). |
| `dependencies` | **yes** | *what the claim stands on*. A dependency edge is a claim about the proposal's foundation. |
| `status` | **incidental** in practice | it is frozen at insert (`"pending"`), so it never distinguishes two proposals in practice — but it IS hashed. |
| **`tags`** | **incidental** | tags are **labels** for retrieval and filtering. They are not part of the claim and not part of its evaluation. |

**The tags are the incidental field.** That is the whole of D4: an incidental field is
identity-bearing, so an incidental difference creates a second proposal.

The counter-argument deserves its weight: for a **memory**, a tag difference *is* a
substantive difference ("urgent" vs "archived" are different artifacts), and the F19
comment says so — *"two saves that differ in provenance (human vs model) are legitimately
distinct artifacts"*. For a **proposal**, though, the claim is carried entirely by the
hypothesis and its criteria; the tags only say how to find it later.

**This is a judgement call, and it is Session A's to make.** §5 sets out the options with
their exact consequences.

### 2.2 The mechanism that made it bite

The critic's hunts used to put volatile values into identity-bearing fields:

| Hunt | What it used to put there | Where it is now |
|---|---|---|
| `PoisonPillHunt` | `cycle_<n>` in the **tags**; the target's **confidence** in the hypothesis | the `Reason` |
| `WeakTheoryHunt` | the theory's **confidence** in the hypothesis | the `Reason` |
| `SurvivalAsymmetryHunt` | the exact **survival ratio** in the hypothesis | the quantised **band**; the exact figures in the `Reason` |
| `MissedOpportunityHunt` | a **count** of invocations in the hypothesis | the count is in the `Reason`; the hypothesis names the session |

Every one of those is now a pure function of the evidence. The `Reason` is logged and never
persisted, which is what makes the cycle observable without making it identity-bearing.

---

## 3. The audit: every production emitter of a proposal

| Emitter | Path | Identity-bearing fields stable across cycles? | Evidence |
|---|---|---|---|
| `SurvivalAsymmetryHunt` | `mpm_theories.propose` (typed) | **yes** — the band is quantised; the citation set is the oldest stable prefix | `TestP3b_RepeatingTheHuntDoesNotCreateAnotherProposal`, `TestP3b_RatioBandIsStableUnderDrift`, `TestP3b_SurvivalProposalIsStableUnderObservationDrift`, `TestP3b_SurvivalCitationSetIsStableAsTheCohortGrows` |
| `WeakTheoryHunt` | `mpm_theories.propose` (typed) | **yes** — the confidence is in the `Reason` | `TestP3b_RepeatingTheHuntDoesNotCreateAnotherProposal` |
| `MissedOpportunityHunt` | `mpm_theories.propose` (typed) | **yes** — the hypothesis names the session, never a count | `TestP3MissedOpportunity_ProposalCarriesNoCycleMarker`, `TestP3MissedOpportunity_ProposalIsStableAcrossRuns`, `TestP3MissedOpportunity_EndToEnd_IsIdempotent` |
| `PoisonPillHunt` | `mpm_theories.propose` (typed) | **yes** — the cycle number is in the `Reason` | `TestPoisonPillHunt_HasNoCycleTag`, `TestM5Characterization_CycleMarkerDefeatsProposalIdentity`, **and the new `TestD4_PoisonPillRepeatedCyclesProduceOnePendingProposal`** |
| any hunt, structurally | — | **no hunt may emit a `cycle_<n>` tag** | `TestP3NoHuntEmitsACycleTag` |
| a human or an agent | `mpm_theories.propose` | **not guaranteed** — the caller chooses the tags | §5 |

**Every production emitter is now stable.** The residual exposure is the last row: the
substrate does not defend itself against a caller that varies an incidental field.

---

## 4. What this PR adds

### 4.1 The production-path D4 contract

`internal/critic/d4_proposal_idempotency_test.go`:

| Test | What it proves |
|---|---|
| `TestD4_PoisonPillRepeatedCyclesProduceOnePendingProposal` | the **PoisonPillHunt** — the hunt D4 is *about*, and the one the P3b suite does not cover — at cycles 5 and 10 produces **identical identity-bearing fields** and **one** pending proposal through the real `mpm_theories propose` dispatch |
| `TestD4_CriticCyclesAcrossAProcessRestartProduceOnePendingProposal` | the same, with the database **closed and reopened** between the cycles: the proposal's identity must be recomputable from what is stored, because the critic is a one-shot process per audit and nothing but the file survives |

### 4.2 The adversarial matrix — the substrate side

`internal/core/d4_proposal_identity_test.go`:

| Matrix question | Test | Pins |
|---|---|---|
| 1. Repeated identical proposals | `TestD4_RepeatedIdenticalProposalIsOneProposal` | the same claim three times is **one** proposal |
| 2. Materially different hypotheses | `TestD4_MateriallyDifferentHypothesesRemainDistinct` | two different claims stay **two** proposals |
| 3. Metadata-only changes | `TestD4_MetadataOnlyChangeCreatesASecondProposal` | a tag-only difference is currently a **second** proposal — the residual, pinned as a characterization |
| 3b. Tag **order** | `TestD4_TagOrderIsSignificantUnderTheCurrentIdentity` | the same label set in a different order is currently a **second** proposal — **finding F1** |
| 4. Different validation criteria | `TestD4_DifferentValidationCriteriaRemainDistinct` | different checks stay **two** proposals |
| 4b. Different dependencies | `TestD4_DifferentDependenciesRemainDistinct` | different foundations stay **two** proposals |
| 5. Repeated cycles after a restart | `TestD4_ProposalIdentitySurvivesAProcessRestart` | the identity is recomputable from what is stored |
| 9. Previously resolved proposals | `TestD4_ResolvedProposalIsNotReopenedByAnIdenticalReproposal` | an adjudicated proposal is **not** reopened by a repeat call |
| 11. Concurrent submissions | `TestD4_ConcurrentIdenticalProposalsProduceOneProposal` | N concurrent identical proposals → **one** proposal, and every success names the row that was actually recorded |
| 11b. Concurrent distinct submissions | `TestD4_ConcurrentDistinctProposalsAreNeverSilentlyLost` | a success always corresponds to a recorded row, and distinct proposals never share an id — **finding F2** |
| 12. No automatic promotion | `TestD4_PendingProposalIsNeverAutomaticallyPromoted` | a pending proposal stays pending, writes nothing to the lesson surface, and keeps the base confidence |

The citation half of the matrix (questions 6, 7, 8, 10 — invalid ids, typed citations,
incorrect type declarations, replayed evidence) is answered by
`internal/core/d3_legacy_citation_test.go` (D3), `internal/core/p2_citation_test.go` (the
typed path) and `internal/core/evidence_identity_test.go` (replay).

### 4.3 The matrix's shape, and why it is shaped that way

Two rules:

1. **A characterization pins what the substrate does; a contract pins what it must do.**
   The current D4 contract test asserted a *mechanism* ("tags must not split a proposal")
   that no production emitter exercises any more. The new tests assert the *behaviour* the
   M5 requirement names, plus characterizations of the current semantics so that any
   change to them is visible and deliberate.
2. **Nothing was weakened, inverted or deleted.** The `m5contract` suite still fails on D4
   exactly as it did before this PR — see §7.3.

---

## 5. Finding F1 — and the decision Session A has to make

### 5.1 What was measured

`MemoryIdentityHash` folds `json(tags)`. `json` of a **slice** is a **sequence**, so the
tag list's order is part of the identity:

```
["critic","alpha"]  ≠  ["alpha","critic"]     →  two proposals
```

Tags are a set. Two proposals that carry exactly the same labels, in a different order, are
one proposal in every sense that matters — and the substrate makes them two.

This is reachable without any cycle at all: an agent that proposes the same claim twice and
lists its labels in a different order gets two pending proposals.

### 5.2 The options, with their exact consequences

| Option | Change | Blast radius | Historical identities |
|---|---|---|---|
| **A. Canonicalise the tag order** | sort the tags before hashing | **global** — every artifact, every collection | historical rows keep their stored hash, so a re-save of an unsorted-tagged row would no longer match unless a legacy-match fallback is added to the dedup matcher |
| **B. Exclude the tags from the proposal's identity** | a proposal's identity is its claim, not its labels | the `theories` collection only | the same legacy-match problem, scoped to proposals |
| **C. Leave it** | nothing | none | none |

**All three touch the shared identity algorithm or the shared dedup matcher.** The brief
says:

> *If fixing D4 requires changing `MemoryIdentityHash` globally, historical identities or
> existing deduplication guarantees, stop at an evidence-backed design and obtain Session
> A's approval.*

⇒ **This PR does not implement any of them.** The design above is the evidence-backed
design the brief asks for.

### 5.3 What the brief asks for, and what the answer is

> *Determine whether two genuinely different hypotheses remain distinguishable and whether
> two equivalent hypotheses can remain one pending proposal across repeated critic cycles.*

| Question | Answer | Proven by |
|---|---|---|
| Do two genuinely different hypotheses remain distinguishable? | **YES** | `TestD4_MateriallyDifferentHypothesesRemainDistinct` |
| Can two equivalent hypotheses remain one pending proposal across repeated critic cycles? | **YES** | `TestD4_PoisonPillRepeatedCyclesProduceOnePendingProposal`, `TestD4_CriticCyclesAcrossAProcessRestartProduceOnePendingProposal` |

**The brief's success criteria for D4 are met today.** The residual is narrower: the
substrate does not defend itself against an emitter that varies an *incidental* field. The
production emitter does not vary one.

### 5.4 The one decision A has to make

> **Should an incidental difference (a tag) split a proposal?**

* **If yes** (the current model): D4 is closed, the `m5contract` assertion about tags
  should be retired as a decision A has made, and F1 should be filed as a cosmetic
  limitation.
* **If no**: the identity for proposals must change, and §5.2's options need choosing —
  with the historical-compat work each option implies.

A's answer determines whether the `m5contract` D4 test is retired (yes) or stays red
(no). **Until then the test stays red**, because retiring it would be asserting a decision
that has not been made.

---

## 6. Finding F2 — concurrent proposals are lost

### 6.1 What was measured

Matrix question 11. Eight concurrent callers, each proposing a **distinct** claim:

```
journal_mode=wal busy_timeout=5000
  proposal 2: err=propose theory: database is locked
  proposal 5: err=propose theory: database is locked
PROPOSALS: 2/8 failed          (a second run: 4/8)
MEMORIES:  0/8 failed          (the comparison)
```

**Two things make this a proposal-path defect and not a substrate-wide one:**

1. The **memory** save path under the same contention loses **nothing** (0/8).
2. The difference is the transaction *shape*.

### 6.2 The mechanism

| Path | Shape | Result |
|---|---|---|
| memory save | the duplicate probe runs **outside** the transaction (`dm.FindLiveDuplicateMemory` → `dm.db`), then the INSERT | no read-then-write upgrade → no stale snapshot → **no loss** |
| proposal | the duplicate probe runs **INSIDE** the transaction (`saveMemoryRow` → `findLiveDuplicateNode(node, …)`), then the INSERT | read-then-write upgrade → **stale snapshot** → loss |

`WithTx` opens a **deferred** transaction (`dm.db.Begin()`). In WAL mode, a deferred
transaction that has taken a read snapshot and then tries to upgrade to a write gets
**SQLITE_BUSY** when another connection has committed since that snapshot was taken — and
that error is **immediate**: `busy_timeout` does not apply to it, because the problem is
not a lock wait but a stale snapshot. The transaction cannot be upgraded, and the retry
inside `ExecTracked` cannot help: the snapshot is still stale.

### 6.3 The isolating experiment

A raw transaction-shape experiment against the same database, 8 concurrent writers each
doing a read-then-write:

| Shape | Failed |
|---|---|
| **A**: `BEGIN` (deferred), read, write | **4/8** |
| **B**: `BEGIN IMMEDIATE`, read, write | **0/8** |

`BEGIN IMMEDIATE` takes the write lock at the start, so the transaction's snapshot is
current and the writers serialise on the write lock with `busy_timeout` doing its job.

### 6.4 Why it is not fixed here

The fix touches the substrate's **transaction discipline** — the area the repository's own
guidance flags as trap-laden ("Do not call `db.SetMaxOpenConns(1)`… MPM relies on WAL +
`busy_timeout=5000` + tx discipline"). Two viable shapes:

* **BEGIN IMMEDIATE for the proposal transaction** — needs a new transaction helper (a
  pinned `*sql.Conn`, as `internal/core/similarity_candidates_store.go` already does) and
  changes when the write lock is taken for every proposal.
* **A callback-level retry around `dm.WithTx`** — `WithTx`'s own comment sanctions the
  pattern (*"callers that need callback-level retry wrap `WithTx` in their own retry
  loop"*), and the proposal's callback is pure transactional work, so a rolled-back
  attempt leaves nothing behind.

Neither is a D4 change. The brief says: *"Do not implement unrelated fixes merely because
the investigation discovers them."* ⇒ **reported, measured, mechanism-proven and
fix-designed — not implemented.** It is the first item of the release assessment
(`docs/dev/session-b-m5-release-assessment.md`).

### 6.5 The finding is reproducible, and it bit this PR's own test

F2 is not a theoretical risk. It reproduced **roughly one run in six** under the race
detector, in the test this PR added.

`TestD4_ConcurrentIdenticalProposalsProduceOneProposal` was first written to assert that
**all eight** concurrent callers succeed and return the same id. Under `make test-race` it
failed on exactly that assertion:

```
--- FAIL: TestD4_ConcurrentIdenticalProposalsProduceOneProposal (3.20s)
    concurrent ProposeTheory: propose theory: database is locked
    concurrent proposal 1 returned no id
```

That is F2, reproduced by the matrix that was written to detect it. The test was
**rewritten** to pin the DEDUP invariant — every success names the same proposal, exactly
one row exists, and that row is the one every success named — while treating a lost race
as what it is: **a loud failure**, never a success with nothing recorded. Asserting that
all N callers succeed would be asserting that F2 does not exist.

The rewritten test was then re-proven non-vacuous: with the dedup defeated (the
pre-insert duplicate probe short-circuited), it fails on exactly the assertion it exists to
make —

```
    concurrent proposal 3 returned "fffe2a83669c9206", want "7aad3d4747834c85" — identical concurrent proposals must resolve to ONE proposal
    theory rows = 4 ([7aad3d4747834c85 97c643d17173e110 a5011b61e9607a8e fffe2a83669c9206]), want exactly 1
```

— and it was run **ten times** under `-race` with no failure.

**This is worth A's attention for one reason beyond F2 itself:** a concurrency test that
asserts the absence of a real defect is not a regression test, it is a landmine. It fails
on a slow machine, in CI, in a fortnight, and the failure looks like flakiness in a new
test rather than the discovery of an old defect. The matrix is only useful if each of its
assertions is one the substrate actually guarantees.

### 6.6 What it costs

* An **agent** that proposes concurrently with another writer loses the proposal and gets
  an **error** — loud, retryable, no corruption.
* The **critic** emits through `mpm call`; an emission failure is **logged and the cycle
  continues** (`internal/critic/audit.go`: *"Failures are logged but do not stop the
  cycle"*). So under contention the critic can **silently lose a finding**.

That asymmetry is what makes F2 a scheduled-critic concern rather than a manual one.

---

## 7. Regression coverage and non-vacuity

### 7.1 The new tests

| File | Tests |
|---|---|
| `internal/core/d4_proposal_identity_test.go` | 11 |
| `internal/critic/d4_proposal_idempotency_test.go` | 2 |

All disposable file-backed databases in isolated `HOME` / `MPM_WORKSPACE` /
`MPM_DB_PATH` roots. No live workspace is touched.

### 7.2 Non-vacuity

The D4 production-path test was run against a **reinstated** defect in an isolated git
worktree: the `PoisonPillHunt` was given back its `cycle_<n>` tag. The test then fails on
exactly the assertion it exists to make — the two cycles carry different identity-bearing
fields, and the second cycle writes a second proposal. The worktree was restored and
removed.

### 7.3 The contract suite — unchanged

```
--- FAIL: TestM5Contract_UnresolvableCitationIsRejected              ← D3-a, open
--- FAIL: TestM5Contract_CycleVaryingTagDoesNotCreateSecondProposal   ← D4, open
```

**Both D4 and D3-a contract failures remain.** No contract test was weakened, inverted or
deleted by this PR. §5.4 states exactly what A's ruling would retire.

---

## 8. Gates

Every gate below was run against the branch head. The `make test-race` result is the
canonical pre-merge gate.

| Gate | Result |
|---|---|
| `internal/core` (+ 10 sub-packages) | **ok** — zero failures |
| `internal/core/tools` | **ok** — zero failures |
| root module (17 packages) | **ok** — zero failures |
| `go vet ./...` | **clean** |
| `gofmt -l` on the touched files | **clean** |
| `mpm-lint --gate` | **PASS** |
| **`make test-race`** | **PASS** — `EXIT=0`, 24 Go packages ok under `-race`, **zero `FAIL` and zero `DATA RACE`**; `make test-scripts` OK (540 tests, floor 50), `make test-bench` OK (217 tests, floor 50), `make test-agent-installation` OK (503 tests, floor 300), `make test-agent-adapters` OK (Python 312 / floor 150, JS 164 / floor 100, TS 89 / floor 15) |
| the M5 contract suite | **2 failures, both OPEN by design** — D4 and D3-a. See §7.3. |
| the M5 characterization suite | **ok** — all three pass |
| the new D4 tests under `-race` | **10 consecutive runs, zero failures** — after the rewrite in §6.5 |
| CI (PR #140) | see §11.1 |

The race-gate package timings (the ones that matter for a reviewer's patience):

| Package | Time under `-race` |
|---|---|
| `cmd/mpm` | 406.6s |
| `mpm-core` (`internal/core`) | 270.0s |
| `internal/core/tools` | 41.1s |
| `internal/scheduler` | 22.8s |
| `cmd/mpm-telemetry` | 15.6s |
| `internal/critic` | 4.4s |
| `internal/testenv` | 9.4s |
| `cmd/mpm-mcp` | 9.5s |

**The `internal/core` figure includes the 13 new D4 tests**, and it is the number that
would have exposed the §6.5 flakiness: the first draft of the concurrent-proposals test
failed under `-race` and did not fail in the ordinary suite. That gap — between the two
gates — is exactly why the race gate is the canonical one, and why this report records
both.

---

## 9. Interactions with the other M5 defects

* **D1 / D5 (evidence identity, PR #130)** — untouched. The evidence row's identity is a
  different mechanism from the proposal's.
* **D2 (no pending lesson state)** — untouched, and matrix question 12 now *proves* the
  pending proposal is never promoted to the lesson surface.
* **D3 (legacy citations)** — a separate PR. This PR's matrix covers the citation
  questions through the existing D3/P2 suites.
* **D6 / D7** — untouched, on their own branches.
* **P2 (the typed citation path)** — untouched.
* **The P2b scheduler gates** — untouched. Nothing in this PR enables or removes a gate.

---

## 10. What was NOT done

* **No change to `MemoryIdentityHash`, to the dedup matcher, or to the unique index.**
  Every option for the residual needs Session A's ruling (§5.2).
* **No change to the transaction discipline.** F2 is reported with a proven mechanism and a
  validated candidate fix, and left alone (§6.4).
* **No M5 regression test weakened, inverted or deleted.**
* No schema change, no migration, no registry change, no new tool.
* No live-workspace or installed-runtime modification.
* No merge, no deployment. The D6 and D7 branches were not modified.
* No unrelated repairs.

---

## 11. Commit, PR and results

| | |
|---|---|
| Branch | `feat/session-b-d4-proposal-identity` |
| Base | `origin/main` = `3abfdda` |
| PR | **#140** |
| Commits | `75ae307` — *test(m5): the M5 integrity matrix, and D4's production-path proof*<br>`2c73a58` — *test(m5): a concurrency test must not assert the absence of a real defect* |

Files changed:

| File | Change |
|---|---|
| `internal/core/d4_proposal_identity_test.go` | **new** — 11 matrix tests |
| `internal/critic/d4_proposal_idempotency_test.go` | **new** — 2 production-path tests |
| `docs/dev/session-b-d4-proposal-identity.md` | **new** — this report |
| `docs/dev/session-b-m5-release-assessment.md` | **new** — the release-blocker classification |

### 11.1 CI

| Job | Result |
|---|---|
| Build + core tests | *pending* |
| Full test suite | *pending* |
| Release gate (cross-agent continuity + public-CLI subprocess suite) | *pending* |

### 11.2 The two things a reviewer should read first

1. **§6.5** — the race detector reproduced F2 in this PR's own test, and the test was
   rewritten. It is the clearest evidence in the sprint that F2 is real, and it is the
   reason the matrix's concurrency assertions are shaped the way they are.
2. **§5.4** — the decision. "Should an incidental difference (a tag) split a proposal?"
   Until A rules, the `m5contract` D4 test stays red, and that is deliberate: retiring it
   would assert a decision that has not been made.
