# Session B — M5 release assessment for Session A

**Branch:** `feat/session-b-d4-proposal-identity` (from `origin/main` = `3abfdda`)
**Target:** the OpenCode-only release candidate, Friday 30 October 2026.
**Audience:** Session A (integration and release acceptance).
**Status of this document:** an **independent** classification of the remaining M5 risks and
blockers, produced without touching A's integration sprint, the live workspace, the
scheduler configuration or the installed runtime.

---

## 1. The one-paragraph answer

**Nothing found blocks the OpenCode-only release.** The three defects that would have
blocked it — D1, D5 and the critic's automatic trust behaviour — are closed, and the
**P2b gate that keeps the automatic scheduled critic OFF can now be lifted** because its
stated removal condition is met. Four residuals remain, none of which corrupts an
artifact: **D3-a** (the legacy citation path still accepts an unresolvable id),
**D4** (an incidental tag difference still splits a proposal), **F2** (concurrent proposals
are lost to `SQLITE_BUSY`), and **D2-core** (the lesson surface has no pending state, so any
writer to it publishes trusted knowledge directly). Each is classified in §5.

**The one finding that changes the release's shape rather than its safety:** the opt-in
`mpm-critic --telemetry-hunt` path is **completely inert** — every lesson save it attempts
is rejected, twice over, and the reason is never surfaced. It therefore does **not**
reproduce D2, but it also does not work at all. It should be fixed or removed before anyone
enables it (§4).

---

## 2. The state of the M5 defects

| Defect | State | Evidence | Residual? |
|---|---|---|---|
| **D1** — a repeated delivery counted as independent corroboration | **CLOSED** | PR #130; `internal/core/evidence_identity_test.go` (16 tests, ordinary CI) | no |
| **D5** — two distinct observations scored the same as one delivered twice | **CLOSED** | PR #130, same mechanism | no |
| **D2** — the critic wrote trusted lessons with no review | **CLOSED on the critic's default path** | the three hunts and the poison pill now emit `mpm_theories propose` with typed, validated, resolvable citations; `TestP3b_NoHuntTargetsTheTrustedLessonSurface`, `TestP3b_CriticFindingIsNotTrustedWithoutReview` | the **lesson surface itself** still has no pending state (§3.2); the opt-in telemetry hunt still *targets* it (§4) |
| **D3-b** — a citation recorded the wrong type | **CLOSED** | this sprint's D3 PR; `TestM5Contract_CitationPreservesItsType` now passes | no |
| **D3-a** — the legacy citation path accepts an unresolvable id | **OPEN** | `TestM5Contract_UnresolvableCitationIsRejected` — still red | yes, §5.1 |
| **D4** — a cycle-varying tag creates a second proposal | **CLOSED in production**; the substrate residual is open | the critic's emission fix landed; `TestD4_PoisonPillRepeatedCyclesProduceOnePendingProposal` proves it end to end | yes, §5.2 |
| **F2** (new) — concurrent proposals are lost to `SQLITE_BUSY` | **OPEN** | measured: 2–4 of 8 concurrent proposals lost; mechanism isolated | yes, §5.3 |

### 2.1 The remaining contract failures

The `m5contract` suite is deliberately red. After the D3 PR, exactly **two** failures
remain, and **no contract test has been weakened, inverted or deleted** by this sprint:

```
--- FAIL: TestM5Contract_UnresolvableCitationIsRejected              ← D3-a
--- FAIL: TestM5Contract_CycleVaryingTagDoesNotCreateSecondProposal   ← D4
```

The green characterization suite (`TestM5Characterization*`) still passes in full — the
defective behaviours remain pinned as characterizations, so nothing has been quietly
changed.

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

**One caveat A must weigh before lifting it:** the gate's condition says "the critic", and
the critic has an **opt-in second path** — `--telemetry-hunt` — that still targets the
trusted lesson surface. §4.

### 3.3 D2-core: the lesson surface still has no pending state

The critic no longer writes to it, but the surface itself is unchanged: `mpm_lessons save`
publishes **trusted** knowledge directly. There is no pending state, no review step and no
"proposed lesson" concept on that surface.

So D2 is closed **as a behaviour of the critic** and open **as a property of the surface**.
Any future writer to `mpm_lessons` inherits the same unbounded trust. The M5 specification
asks for a pending lesson state; implementing it is a schema change, which is A's
authority. **Recommendation: record it as the accepted residual for the initial release**
(§5.4) — the surface has exactly one writer today, and that writer is the operator.

---

## 4. The opt-in telemetry hunt — inert, and quietly so

### 4.1 What it does

`mpm-critic --telemetry-hunt` additionally runs `HighTokenNoArtifactHunt` from the
telemetry store and emits each finding as a **lesson**:

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

### 4.2 It cannot write anything — two independent rejections

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

### 4.3 The failure is invisible

`cmd/mpm-critic/main.go` captures the child's **stdout only** (`cmd.Output()`), so the
tool's error message on stderr is discarded, and the hunt logs:

```
telemetry lesson save failed  session=…  err=mpm_lessons: : exit status 1
```

— an error message with the reason removed. The hunt then **continues to the next
finding**, and the process exits **0**.

⇒ An operator who enables `--telemetry-hunt` sees "telemetry hunt complete" and a
successful exit code, and **nothing was recorded**.

### 4.4 What this means for M5

* **D2 does NOT reproduce on this path.** The trusted lesson surface is never written, so
  the "critic writes trusted knowledge without review" failure does not occur here — not
  because the path was fixed, but because it is inert.
* **But the path is not safe to enable.** If the type were "fixed" to `insight`, the hunt
  would write trusted observations with **no review step and no pending state**, and the
  content it writes is a **live measurement**: `f.Reason` embeds the token count, which
  grows between cycles. `AddLesson` deduplicates on the content hash, so a growing count
  means a **new lesson every cycle** rather than a reinforcement — unbounded accumulation
  of trusted observations about the same session.
* **Recommendation: fix the payload shape and the type before the flag is documented, or
  remove the flag.** It is currently advertised (`mpm-critic --help`) but cannot work.

---

## 5. The classification

Four questions, four answers per residual.

### 5.1 D3-a — the legacy citation path accepts an unresolvable id

| Question | Answer |
|---|---|
| Blocking safe **manual** critic use? | **NO.** The critic uses the **typed** path on every hunt; its citations are validated and resolvable. The legacy path is reachable only when a caller omits `source_type`. |
| Blocking **automatic scheduled** critic use? | **NO.** Same reason. |
| Blocking the **OpenCode-only release**? | **NO.** |
| Acceptable as a **documented limitation**? | **YES**, with one caveat: the caveat is that `mpm_decisions record`/`supersede` have **no `source_type` parameter at all**, so a decision's citations can never be validated. That is a registry gap, and it is A's shared interface — see the D3 PR's §6.3 (step 2). |

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

### 5.4 D2-core — the lesson surface has no pending state

| Question | Answer |
|---|---|
| Blocking safe **manual** critic use? | **NO.** The critic does not write to the surface. |
| Blocking **automatic scheduled** critic use? | **NO**, provided `--telemetry-hunt` is not enabled (§4). |
| Blocking the **OpenCode-only release**? | **NO.** |
| Acceptable as a **documented limitation**? | **YES — and this is the one limitation that should be written into the release notes.** The surface publishes trusted knowledge directly, so it must be treated as an operator-only surface until a pending state exists. The M5 specification asks for that pending state; implementing it is a schema change (A's authority). |

---

## 6. What A has to decide, in one list

| # | Decision | A's authority because | Session B's recommendation |
|---|---|---|---|
| **R1** | Lift the **P2b gate**? | it is a release risk decision | **Yes** — its stated removal condition is met (§3.2). But fix F2 first if the cadence is short. |
| **R2** | `mpm-critic --telemetry-hunt`: **fix or remove**? | it is an advertised but inert flag | **Fix the payload shape and the type** — or remove the flag until it can be done properly. Do not enable it as it stands (§4). |
| **R3** | Should an **incidental tag difference** split a proposal? | it is a shared identity-semantics decision | The brief's D4 criteria are already met; accept the current model, retire the synthetic contract test, and file F1 as a cosmetic limitation — **or** mandate a scoped identity change (§5.2 of the D4 report). |
| **R4** | Does the legacy citation path get **tightened**? | it changes a public tool's behaviour for every caller | **Not for the initial release.** The typed path is the answer; the migration steps are in the D3 report §6.3. |
| **R5** | Add `source_type` to `mpm_decisions record`/`supersede`? | it is a tool-registry change | **Yes** — it is the concrete gap that makes a decision's citations unvalidatable, and it mirrors `mpm_theories.propose` exactly. |
| **R6** | A **pending lesson state** on the lesson surface? | it is a schema change | **Defer**, and record the surface as operator-only in the release notes (§5.4). |

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
| concurrent proposals are lost | measured in the D4 report §6.1, mechanism isolated in §6.3 |
| the telemetry hunt's payload is rejected twice over | measured with the real binary in §4.2 |
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
  change, the `source_type` registry addition or the pending lesson state. All four are
  decisions listed in §6.
* No M5 regression test weakened, inverted or deleted.
