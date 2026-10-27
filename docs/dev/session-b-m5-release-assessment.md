# Session B — M5 release assessment for Session A

**Branch:** authored on `feat/session-b-d4-proposal-identity` (from `origin/main` = `3abfdda`);
this final update rides on `feat/session-b-release-assessment-merge-state` (from `origin/main`
= `a4e3da9`).
**Target:** the OpenCode-only release candidate, Friday 30 October 2026.
**Audience:** Session A (integration and release acceptance).
**Status of this document:** an **independent** classification of the remaining M5 risks and
blockers, produced without touching A's integration sprint, the live workspace, the
scheduler configuration or the installed runtime.

> **Final state.** Session A merged both of this sprint's PRs: **#141** (the D3
> correction, `434e47e`) and **#140** (the D4 integrity matrix and production-path proof,
> `a4e3da9`). `main` also carries PR #137 (the legacy host adapter retirement), PR #138
> (the telemetry hunt read-only), PR #132 (D6), PR #135 (D7) and PR #143.
>
> **Session A then ruled on every decision in §6** — PR #144 (`ef7a535`), the restated
> trust-blocker ledger. D3-a is accepted as a documented limitation with the retirement
> proposal ruled step by step; the P2b gate **stays closed**; F2 and D2-core are both named
> in the ledger; and the `mpm-telemetry observe` sibling is tracked as **#139**. Each ruling
> is recorded against its row in §6 below.
>
> The §4 findings are updated for #138, and §4.5 records the one sibling it left behind.
> **§2.2 records the verification of the merged tree** — `origin/main` + PR #140, merged
> in a scratch worktree and swept — which is the state A released.

---

## 1. The one-paragraph answer

**Nothing found blocks the OpenCode-only release.** The three defects that would have
blocked it — D1, D5 and the critic's automatic trust behaviour — are closed, and the
**P2b gate that keeps the automatic scheduled critic OFF** has been **left closed by
Session A's ruling** (PR #144) — this assessment had argued its stated removal condition
is met, and A chose to keep it shut on the strength of F2 and D2-core. Four residuals
remain, none of which corrupts an artifact: **D3-a** (the legacy citation path still
accepts an unresolvable id — accepted as a documented limitation, with the retirement
proposal ruled), **D4** (an incidental tag difference still splits a proposal), **F2**
(concurrent proposals are lost to `SQLITE_BUSY`), and **D2-core** (the lesson surface has
no pending state, so any writer to it publishes trusted knowledge directly). Each is
classified in §5, and each carries A's ruling where one has been made.

**The trust-boundary exception in the telemetry hunt is RESOLVED** — by Session A's own
adjudication, PR #138, which made the hunt **read-only** while this assessment was being
written. This document's §4 records the independent measurement that motivated that
adjudication; the finding it produced is now closed on `main`.

**One sibling of that defect remains, and it is the one thing this assessment asks A to
look at.** The **same** broken count and the **same** broken lesson payload still live in
`cmd/mpm-telemetry/observe.go` — the standalone `mpm-telemetry observe` command, which PR
#138 did not touch. Measured against the real binary, it calls `mpm call mpm_provenance`
(a tool that is **not in the registry and never was**) and then calls `mpm call mpm_lessons`
with a flat payload that the params-envelope check rejects. It therefore cannot write
either — it is inert, exactly as the critic's copy was — but it is a **live, advertised
command** on the release's `mpm-telemetry` binary, and if anyone repairs its count the way
#138 repaired the critic's, they would **arm a trusted-lesson write**. §4.2.

---

## 2. The state of the M5 defects

| Defect | State | Evidence | Residual? |
|---|---|---|---|
| **D1** — a repeated delivery counted as independent corroboration | **CLOSED** | PR #130; `internal/core/evidence_identity_test.go` (16 tests, ordinary CI) | no |
| **D5** — two distinct observations scored the same as one delivered twice | **CLOSED** | PR #130, same mechanism | no |
| **D2** — the critic wrote trusted lessons with no review | **CLOSED** | the three hunts and the poison pill now emit `mpm_theories propose` with typed, validated, resolvable citations; `TestP3b_NoHuntTargetsTheTrustedLessonSurface`, `TestP3b_CriticFindingIsNotTrustedWithoutReview`; and **PR #138** made the opt-in telemetry hunt read-only | the **lesson surface itself** still has no pending state (§3.2); the standalone `mpm-telemetry observe` command still carries the same exception (§4.2) |
| **D3-b** — a citation recorded the wrong type | **CLOSED** | this sprint's D3 PR; `TestM5Contract_CitationPreservesItsType` now passes | no |
| **D3-a** — the legacy citation path accepts an unresolvable id | **OPEN, accepted as a documented limitation** | `TestM5Contract_UnresolvableCitationIsRejected` — still red, deliberately. **Session A has ruled** on the retirement proposal (D3 report §6.3): step 1 approved, step 2 approved in principle as its own shared-interface PR, step 3 deferred behind a deprecation period. | yes, §5.1 |
| **D4** — a cycle-varying tag creates a second proposal | **CLOSED in production**; the substrate residual is open | the critic's emission fix landed; `TestD4_PoisonPillRepeatedCyclesProduceOnePendingProposal` proves it end to end | yes, §5.2 |
| **F2** (new) — concurrent proposals are lost to `SQLITE_BUSY` | **OPEN** | measured: 2–4 of 8 concurrent proposals lost; mechanism isolated | yes, §5.3 |

### 2.1 The remaining contract failures

The `m5contract` suite is deliberately red. **Exactly two** failures remain on `main`, and
**no contract test has been weakened, inverted or deleted** by this sprint:

```
--- FAIL: TestM5Contract_UnresolvableCitationIsRejected              ← D3-a
--- FAIL: TestM5Contract_CycleVaryingTagDoesNotCreateSecondProposal   ← D4
```

The green characterization suite (`TestM5Characterization*`) still passes in full — the
defective behaviours remain pinned as characterizations, so nothing has been quietly
changed.

### 2.2 The merged-tree verification — what A released

Because `main` advanced six times while this sprint ran (PRs #137, #138, #132, #135, #143
and #141), the PR was verified against the **merged** result rather than against its own
base: `origin/main` + PR #140, merged in a scratch worktree, then swept.

| Check | Result |
|---|---|
| the merge | **automatic, with no conflicts** |
| the 13 new D3 + D4 tests on the merged tree | **all pass** |
| the M5 characterization suite on the merged tree | **all 3 pass** |
| the M5 contract suite on the merged tree | **exactly 2 failures** — D3-a and D4; `CitationPreservesItsType` **passes** |
| `internal/core` (+ 10 sub-packages) on the merged tree | **ok** — 210.4s, zero failures |
| `internal/core/tools` on the merged tree | **ok** — 37.0s, zero failures |
| root module (17 packages) on the merged tree | **ok** — 411.6s for `cmd/mpm`, zero failures |
| `internal/critic` on the merged tree | **ok** — 4.6s, zero failures |

**So the state A released is: two contract failures, both OPEN by design, both with a
decision attached.**

> **A note on how the merged-tree sweep was run, because the first attempt looked like a
> failure and was not.** The sweep was first run as raw `go test` in a scratch worktree
> with no build, and it reported failures — the root-module CLI tests
> (*"developer build artifact missing: … Run `make build` first"*) and the build-config
> invariant (*"… must exist (run `make build` first)"*). Both are **build-ordering
> artefacts of the scratch worktree**, not defects: `make test` depends on `build`, and
> the raw `go test` sweep skips it. After `make build`, every one of those tests passes
> and the whole tree is green. Recorded because a scratch-tree sweep that skips the build
> is a trap, and the next agent to run one will meet it too.

---

## 3. The P2b gate — the automatic critic is OFF, and it can now be turned on

### 3.1 What is in force today

The `critic_audit` wake kind is **blocked at both boundaries**:

| Boundary | Where | Effect |
|---|---|---|
| **Arming** (producer) | `internal/core/wake_tools.go:298` — `ScheduleWake` refuses the kind | the `critic_audit` wake cannot be armed at all, by any producer |
| **Dispatch** (consumer) | `internal/scheduler/scheduler.go:546` — `executeOne` refuses the kind | a wake that already exists (created before the gate, by an older binary, or by direct SQL) is refused with an observable error and marked fired with the reason in `last_error` |

⇒ **The automatic scheduled critic does not run today.** Not "runs unsafely" — it does not
run. A manual `mpm-critic` invocation still works and is the supported path.

### 3.2 The gate's removal condition is now met

The gate's own documentation states the condition (`internal/core/wake_tools.go`):

> *It is removed when the trust boundary is actually enforced — the critic emits a proposal
> on an existing untrusted surface with typed, validated, resolvable citations, and no
> longer writes trusted lessons directly.*

| Condition | Met? | Evidence |
|---|---|---|
| the critic emits a proposal on an **untrusted** surface | **yes** | `mpm_theories propose` — a pending proposal, not a durable lesson |
| with **typed** citations | **yes** | every hunt emits `source_type`; `TestP3b_CitationsResolveAsMemories`, `TestP3b_CitationsResolveAsTheories`, `TestP3MissedOpportunity_CitationsResolveAsToolInvocations` |
| **validated** and **resolvable** | **yes** | `TestP3b_InvalidCitationRejectsTheProposalAtomically`, `TestP3MissedOpportunity_EndToEnd_RejectsAnUnresolvableCitation` |
| the critic **no longer writes trusted lessons** | **yes on the default path** | `TestP3b_NoHuntTargetsTheTrustedLessonSurface` — a structural guard over the hunt registry |
| the duplicate-evidence inflation is closed | **yes** | D1/D5 (PR #130) |

**⇒ Recommendation to A: the P2b gate's stated preconditions are satisfied, and lifting it
is A's call.** It is a release decision, not a defect.

**The caveat that applied before Session A's #138 no longer applies.** The gate's condition
says "the critic", and the critic had an **opt-in second path** — `--telemetry-hunt` — that
still targeted the trusted lesson surface. **PR #138 made that path read-only**, so the
critic now has no path to the lesson surface at all, opt-in or not. §4.

**One residual of that caveat remains, outside the critic:** the standalone
`mpm-telemetry observe` command carries the same exception and was not touched by #138.
It is manual-only and inert, so it does not bear on the gate — but it is the thing to fix
next. §4.5, decision **R7**.

### 3.3 D2-core: the lesson surface still has no pending state

The critic no longer writes to it — and since #138, not even the opt-in path does. But the
surface itself is unchanged: `mpm_lessons save` publishes **trusted** knowledge directly.
There is no pending state, no review step and no "proposed lesson" concept on that surface.

So D2 is closed **as a behaviour of the critic** and open **as a property of the surface**.
Any future writer to `mpm_lessons` inherits the same unbounded trust. The M5 specification
asks for a pending lesson state; implementing it is a schema change, which is A's
authority. **Recommendation: record it as the accepted residual for the initial release**
(§5.4) — the surface has exactly one writer today, and that writer is the operator.

---

## 4. The telemetry hunt — RESOLVED by Session A's #138, with one sibling left

> **STATUS: this section was written against `3abfdda` and updated after Session A's
> adjudication landed on `main` as PR #138 (`7d18237`).** The finding below is what
> motivated that adjudication; the adjudication is now in `main`.

### 4.1 What the path did

`mpm-critic --telemetry-hunt` additionally ran `HighTokenNoArtifactHunt` from the
telemetry store and emitted each finding as a **lesson**:

```go
// internal/telemetry/observe.go
func FindingLessonPayload(f Finding, cycleTag string) map[string]any {
	return map[string]any{
		"action": "save",
		"fact":   f.Reason,
		"type":   "observation",
		"tags":   []string{"telemetry", "high-token-no-artifact", "auto", cycleTag},
	}
}
```

### 4.2 It could not write anything — two independent rejections

Measured with the real binary against a disposable workspace:

| # | The payload | The rejection |
|---|---|---|
| 1 | the **flat** shape `{"action":"save","fact":…,"type":…,"tags":…}` | `mpm_lessons: payload has top-level field(s) [fact type tags] outside the params envelope — expected shape {"action":"<op>","params":{…}}` |
| 2 | the correct `{"action":"save","params":{…}}` envelope, with the hunt's own `"type":"observation"` | `invalid lesson type "observation": must be one of warning, practice, or insight` |

The lesson surface's type allow-list is `{warning, practice, insight}`
(`internal/core/db.go`, `ValidLessonTypes`). The hunt emits `"observation"`, which the
M5 specification wants — *a finding is observation; human arbitration decides* — but which
the surface does not accept. The two disagree, and the disagreement is the whole story.

**The same payload with `"type":"insight"` succeeds** — so the trusted surface would
accept it if the type matched.

### 4.3 The failure was invisible

`cmd/mpm-critic/main.go` captured the child's **stdout only** (`cmd.Output()`), so the
tool's error message on stderr was discarded, and the hunt logged:

```
telemetry lesson save failed  session=…  err=mpm_lessons: : exit status 1
```

— an error message with the reason removed. The hunt then **continued to the next
finding**, and the process exited **0**.

⇒ An operator who enabled `--telemetry-hunt` saw "telemetry hunt complete" and a
successful exit code, and **nothing was recorded**.

**Session A's #138 found a third breakage in the same path, independently of this
measurement:** the artifact count shelled out to `mpm call mpm_provenance`, a tool that is
**not in the registry and never was**. So the hunt failed on every qualifying session and
exited 1. (Both exit codes are consistent with the measurement above: with no qualifying
session the count is never called and the hunt exits 0 having written nothing; with one, it
exits 1 before any write.)

### 4.4 The adjudication, and what it established

**PR #138 (`7d18237`, on `main`):** the telemetry hunt is now a **read-only diagnostic**. It
computes the observation and **reports** it; it writes nothing — not a lesson, not a
theory, not an artifact, not a wake. The count is computed **in-process** from the
substrate's own attribution (`internal/critic/artifact_count.go`,
`critic.SessionArtifactCount`), so no subprocess is involved and the path has no way to
write. The regression is `cmd/mpm-critic/telemetry_hunt_readonly_test.go`, with its
non-vacuity demonstrated by putting one `INSERT INTO lessons_base` back into the hunt.

The reasoning A recorded is worth keeping, because it is the general rule:

> *The ordering rule this establishes: close the trust boundary FIRST. Repairing the count
> against a substrate that still accepted a trusted-lesson write would ARM the write, not
> fix it.*

**⇒ This assessment's §4 finding is CLOSED.** The telemetry hunt does not reproduce D2, and
it now cannot write at all.

### 4.5 ⚠️ The sibling: `mpm-telemetry observe` carries the SAME exception

**PR #138 fixed `cmd/mpm-critic/main.go`. It did not touch `cmd/mpm-telemetry/observe.go`** —
the standalone `mpm-telemetry observe` command, which the `mpm-telemetry` binary advertises
in its help output.

That file still contains **both** breakages, verbatim:

| In `cmd/mpm-telemetry/observe.go` | The breakage | Verified against the real binary |
|---|---|---|
| `defaultArtifactCountFn` | `mpm call mpm_provenance` with `{"action":"count_by_session","session_id":…}` — **the tool does not exist** | `{"error":"unknown tool: mpm_provenance. available: mpm_memory, mpm_theories, …"}` |
| `defaultLessonSaveFn` | `mpm call mpm_lessons` with the **flat** `FindingLessonPayload` | `{"error":"mpm_lessons: payload has top-level field(s) [fact type tags] outside the params envelope …"}` |

The count failure is fatal to the command: `telemetry.Hunt` returns the error, `observe`
returns it, and the process exits non-zero. So `mpm-telemetry observe` **cannot write a
lesson** — it is inert, exactly as the critic's copy was.

**But it is a live, advertised command on the release's `mpm-telemetry` binary, and the
inertness is an accident of two independent breakages rather than a designed boundary.**
The hazard is concrete: someone who "fixes" the count the way #138 fixed the critic's
count — by computing it in-process — would **ARM** the lesson write, because the payload
is still wrong. The payload is wrong in a way that is *easy* to "fix" too (move the fields
into `params`, change `"observation"` to `"insight"`), and then the path would write
**trusted** observations with no review step, no pending state, and a **live measurement**
in the content (`f.Reason` embeds the token count, which grows between cycles — so
`AddLesson`'s content-hash dedup would produce a **new lesson every cycle** rather than a
reinforcement).

**Reachability.** `mpm-telemetry observe` is **manual only**: no systemd unit runs it, and
`scripts/tests/test_runtime_asset_provisioning.py` asserts that *no scheduler handler
shells out to `mpm-telemetry observe`*. The scheduled telemetry component is
`mpm-telemetry serve` (the collector), which does not write to the substrate at all.

**Recommendation for A (decision R7):** extend the #138 adjudication to
`mpm-telemetry observe` — make it read-only, exactly as #138 made the critic's copy
read-only, and reuse `critic.SessionArtifactCount` for the count. **This is A's call and
A's surface**, so it is reported here rather than implemented: the same reasoning #138
recorded applies, and it is the same class of defect in the same trust boundary.

**Nothing about this blocks the release.** The path is inert, it is manual, and it is not
scheduled. It is reported because it is a **live trap** for whoever next repairs it, and
because the repair is trivially able to make things worse.

---

## 5. The classification

Four questions, four answers per residual.

### 5.1 D3-a — the legacy citation path accepts an unresolvable id

> **RULED.** Session A's ledger (PR #144, `ef7a535`) records D3-a as **OPEN, accepted as
> a documented limitation**, with the ruling on this assessment's retirement proposal:
> **step 1 approved** (the typed path becomes the default for `mpm_theories propose` — it
> cannot reject anything accepted today), **step 2 approved in principle** as its own
> shared-interface PR, **step 3 deferred** behind a deprecation period. The contract test
> therefore stays red, deliberately, until step 3 lands.

| Question | Answer |
|---|---|
| Blocking safe **manual** critic use? | **NO.** The critic uses the **typed** path on every hunt; its citations are validated and resolvable. The legacy path is reachable only when a caller omits `source_type`. |
| Blocking **automatic scheduled** critic use? | **NO.** Same reason. |
| Blocking the **OpenCode-only release**? | **NO** — and A has ruled: accepted as a documented limitation. |
| Acceptable as a **documented limitation**? | **YES — RULED**, with one caveat recorded in the ledger: `mpm_decisions record`/`supersede` have **no `source_type` parameter at all**, so a decision's citations can never be validated. That is the registry gap, and it is A's shared interface — the D3 report's §6.3 step 2, approved in principle. |

**Why it is not a data-integrity risk:** a legacy citation of an id that resolves to
nothing is recorded with `source_type = "memory"` — a mislabel, not a false claim of
evidence, and the proposal still says what it claims. The typed path (which the critic
uses) rejects the same input outright.

### 5.2 D4 — an incidental tag difference still splits a proposal

| Question | Answer |
|---|---|
| Blocking safe **manual** critic use? | **NO.** Every production emitter's identity-bearing fields are a pure function of its evidence. The residual needs an emitter that varies an incidental field — no production emitter does. |
| Blocking **automatic scheduled** critic use? | **NO.** Same reason, and now proven by `TestD4_PoisonPillRepeatedCyclesProduceOnePendingProposal`. |
| Blocking the **OpenCode-only release**? | **NO.** |
| Acceptable as a **documented limitation**? | **YES**, with the decision in the D4 report §5.4 attached: *should an incidental difference (a tag) split a proposal?* Until A rules, the `m5contract` D4 test stays red — retiring it would assert a decision that has not been made. |

**The extra finding (F1):** the identity treats the tag SET as a sequence, so
`["critic","alpha"]` and `["alpha","critic"]` are two proposals. Cosmetic, reachable by an
ordinary agent, and the same decision would fix it.

### 5.3 F2 — concurrent proposals are lost to `SQLITE_BUSY`

| Question | Answer |
|---|---|
| Blocking safe **manual** critic use? | **NO.** The loss is **loud** for an agent: the call returns `database is locked`, and the caller can retry. Nothing is corrupted and nothing is silently dropped. |
| Blocking **automatic scheduled** critic use? | **NO — but it is the most consequential residual.** The critic's emission failure is **logged and the cycle continues** (`internal/critic/audit.go`: *"Failures are logged but do not stop the cycle"*), so under contention the critic can **silently lose a finding**. With one writer (the critic alone) it cannot happen; with the critic and an agent writing at the same moment, it can. |
| Blocking the **OpenCode-only release**? | **NO**, on the evidence: the single-user OpenCode release has one agent, and the critic is either off (P2b gate) or running on a daily cadence (§3.1). The measured loss rate is under **concurrent-writer** conditions only — 2–4 of 8 writers in a burst. |
| Acceptable as a **documented limitation**? | **YES for the initial release**, **but it should be fixed before the scheduled critic is enabled at a short cadence**, and it deserves its own PR (it is not a D4 change). The mechanism is isolated and the candidate fix is validated: a deferred transaction that reads and then writes cannot upgrade to a write in WAL when another writer has committed since its snapshot — `BEGIN IMMEDIATE` eliminates it (measured: 4/8 lost → 0/8). |

**Recommendation:** schedule it as an independent PR, before the P2b gate is lifted.

**Session A's ruling (PR #144):** F2 is named in the ledger as **NEW, and the most
consequential residual** — *"concurrent proposals are lost to SQLITE_BUSY (2-4 of 8
measured; reproduced under make test-race), and the critic's emission failure is logged and
the cycle continues, so a finding can be lost SILENTLY. Candidate fix validated:
BEGIN IMMEDIATE (4/8 lost -> 0/8)."* It is one of the two reasons the P2b gate stays
closed.

### 5.3.1 F2 reproduced itself, under the race detector

F2 is not a theoretical risk, and the strongest evidence for it is not the probe but the
**D4 PR's own test**. The M5 integrity matrix's concurrent-identical-proposals test was
first written to assert that all eight concurrent callers succeed. Under
`make test-race` it failed — **roughly one run in six** — with:

```
concurrent ProposeTheory: propose theory: database is locked
```

That is F2, reproduced by the matrix written to detect it. The test was rewritten to pin
the dedup invariant and to treat a lost race as the loud failure it must be (D4 report
§6.5).

Two things follow for this assessment:

1. **The defect is real enough to break a test that asserted its absence.** It is not a
   corner case that only a synthetic probe can reach.
2. **The ordinary suite did not catch it — the race gate did.** A reviewer who runs only
   `go test` would have seen a green matrix and a defect hiding inside it. The canonical
   gate is `make test-race`, and the numbers in D4 report §8 are from that gate.

### 5.4 D2-core — the lesson surface has no pending state

| Question | Answer |
|---|---|
| Blocking safe **manual** critic use? | **NO.** The critic does not write to the surface — and since #138, neither does the opt-in telemetry path. |
| Blocking **automatic scheduled** critic use? | **NO.** The critic has no path to the surface. |
| Blocking the **OpenCode-only release**? | **NO.** |
| Acceptable as a **documented limitation**? | **YES — and this is the one limitation that should be written into the release notes.** The surface publishes trusted knowledge directly, so it must be treated as an operator-only surface until a pending state exists. The M5 specification asks for that pending state; implementing it is a schema change (A's authority). The two paths that tried to reach it are now inert or read-only (§4), so the residual is a property of the SURFACE, not of any writer. |

---

## 6. What A has to decide, in one list

| # | Decision | A's authority because | Session B's recommendation |
|---|---|---|---|
| **R1** | Lift the **P2b gate**? | it is a release risk decision | **Session B recommended yes** — the stated removal condition is met (§3.2), after F2 if the cadence is short. **A's ruling (PR #144): the gate STAYS CLOSED.** *"Session B's assessment argues the removal condition is met; the gate stays closed, and F2 and D2-core give the reason beyond the standing instruction."* A's call, taken, and the reasoning is A's own. |
| **R2** | `mpm-critic --telemetry-hunt`: **fix or remove**? | it is an advertised but inert flag | **DECIDED — by A, in PR #138**: the hunt is now a **read-only diagnostic**. Closed (§4.4). |
| **R3** | Should an **incidental tag difference** split a proposal? | it is a shared identity-semantics decision | The brief's D4 criteria are already met; accept the current model, retire the synthetic contract test, and file F1 as a cosmetic limitation — **or** mandate a scoped identity change (§5.2 of the D4 report). **A's ledger (PR #144): D4 is CLOSED in production, substrate residual OPEN, and "the extra finding F1 rides with the same decision."** |
| **R4** | Does the legacy citation path get **tightened**? | it changes a public tool's behaviour for every caller | **RULED (PR #144):** D3-a is **accepted as a documented limitation**; step 1 of the retirement proposal is **approved**, step 2 approved in principle as its own shared-interface PR, step 3 deferred behind a deprecation period (D3 report §6.3). |
| **R5** | Add `source_type` to `mpm_decisions record`/`supersede`? | it is a tool-registry change | **Yes** — it is the concrete gap that makes a decision's citations unvalidatable, and it mirrors `mpm_theories.propose` exactly. **Approved in principle by A (PR #144), to be its own shared-interface PR.** |
| **R6** | A **pending lesson state** on the lesson surface? | it is a schema change | **Defer**, and record the surface as operator-only in the release notes (§5.4). **A's ledger (PR #144): D2-core is NEW, "must be in the release notes."** |
| **R7** | **NEW — extend #138 to `mpm-telemetry observe`?** | it is A's release surface, and the same trust boundary | **Yes** — make it read-only, exactly as #138 made the critic's copy read-only, and reuse `critic.SessionArtifactCount` for the count. **A's ledger (PR #144) records it as OPEN under #139**: *"Currently INERT, so it cannot write, but repairing its count would ARM a trusted-lesson write."* |

---

## 7. The evidence this assessment rests on

Every claim above is pinned by a test that runs in an ordinary CI gate, or by a
measurement reproduced in this document.

| Claim | Where |
|---|---|
| the P2b gate is in force at both boundaries | `internal/core/wake_tools.go:298`, `internal/scheduler/scheduler.go:546` |
| the critic's default hunts never target the lesson surface | `TestP3b_NoHuntTargetsTheTrustedLessonSurface` |
| the critic's citations are typed and validated | `TestP3b_CitationsResolveAsMemories`, `TestP3b_CitationsResolveAsTheories`, `TestP3MissedOpportunity_CitationsResolveAsToolInvocations`, `TestP3b_InvalidCitationRejectsTheProposalAtomically` |
| repeated critic cycles produce one pending proposal | **`TestD4_PoisonPillRepeatedCyclesProduceOnePendingProposal`** (new) |
| the same, across a process restart | **`TestD4_CriticCyclesAcrossAProcessRestartProduceOnePendingProposal`** (new) |
| a pending proposal is never promoted | **`TestD4_PendingProposalIsNeverAutomaticallyPromoted`** (new) |
| concurrent proposals are lost | measured in the D4 report §6.1, mechanism isolated in §6.3, **reproduced under `-race` by the D4 matrix's own test** (§5.3.1) |
| the telemetry hunt's payload is rejected twice over | measured with the real binary in §4.2; **and PR #138 made the path read-only** (§4.4) |
| **`mpm-telemetry observe` carries the same exception** | `cmd/mpm-telemetry/observe.go` — `defaultArtifactCountFn` calls `mpm_provenance` (**not in the registry**) and `defaultLessonSaveFn` sends the flat payload; both verified against the real binary (§4.5) |
| the remaining contract failures are exactly two | §2.1 |

Every test used a disposable file-backed database in an isolated `HOME` /
`MPM_WORKSPACE` / `MPM_DB_PATH` root, or the real binary against a disposable workspace.
**No live workspace, no installed runtime, no scheduler configuration and no A's branches
were touched.**

---

## 8. What was NOT done

* No live installation or database modification. No deployment. No merge.
* No change to A's release infrastructure or to C's adapter cleanup.
* No change to the **P2b gates** — neither removed nor enabled.
* No change to the scheduler's configuration.
* No implementation of F2, and no implementation of the D3-a tightening, the D4 identity
  change, the `source_type` registry addition, the pending lesson state, or the
  `mpm-telemetry observe` read-only change. All five are decisions listed in §6.
* **No change to `cmd/mpm-critic` or `cmd/mpm-telemetry`** — Session A's #138 owns the
  telemetry hunt, and the sibling finding in §4.5 is reported, not implemented.
* No M5 regression test weakened, inverted or deleted.
