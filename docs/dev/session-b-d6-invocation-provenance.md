# Session B — D6: invocation-to-artifact provenance integrity

**Branch:** `feat/session-b-d6-invocation-provenance` (from `origin/main` = `3584190`)
**Status:** D6 **CLOSED** for every recording path the substrate can attribute today, by a
correction that needs **no schema change, no migration, no tool-registry change and no
public tool-contract change**.
**Scope:** the provenance bridge only. D3, D4, D7, the telemetry hunt, the P2b scheduler
gate, the evidence identity mechanism from PR #130 and the lifecycle event contracts are
untouched; interactions are reported in §12.

---

## 1. Summary

A tool invocation and the artifact it produced had no durable link. `artifact_provenance`
rows existed for decisions, lessons, theories and work items — but the
`invocation_id` column was **empty** on all but one write path, so the artifact could not
be joined back to the call that made it.

Measured on the live workspace before the correction (`~/.mpm/src/db/mpm.db`, **read-only**;
the workspace is live and still growing, so the row counts are a snapshot — the *rates* are the
finding, not the totals):

| `artifact_type` | rows | with `invocation_id` | join to `tool_invocations` |
|---|---|---|---|
| `memory` | 54 | 54 | 50 |
| **`decision`** | **29** | **0** | **0** |
| `lesson` | 6 | 1 | 1 |
| `work` | 3 | 3 | 3 |

Twenty-nine decisions were recorded. **None of them joined to its producing invocation.**
The provenance looked complete — a row existed, with a framework, a model, a session — and
was unattributable. That is the worst of both readings: a reader cannot tell "no
invocation is known" from "an invocation is known and was not recorded".

The cause was a **missing bridge**, not a missing column. The dispatcher minted a
per-call invocation id and carried it in the `ActiveContext` all the way to the tool
handler. Exactly one write path read it. The rest resolved provenance from the
process-wide resolver base — which, by construction, **cannot** carry an invocation id
(`CreationProvenance` has no such field; `Resolve("", "", "", "")` returns `""`).

The correction threads the identity through every recording path and removes the two
places that **manufactured** an invocation id when none was known. A missing invocation id
now stays missing.

---

## 2. Root cause

### 2.1 The trace, end to end

```
OpenCode plugin                     agent_installation/mpm-opencode/src/index.ts:266
  env.MPM_PROVENANCE_INVOCATION_ID = `inv_${crypto.randomUUID()}`

  ├─ programmatic calls             callMpmWithProvenance → buildProvenanceEnv → per call
  └─ shell calls                    shellEnv → buildProvenanceEnv → PER SHELL COMMAND

CLI transport                       cmd/mpm/call.go:185-210, 249
  invocationID := os.Getenv("MPM_PROVENANCE_INVOCATION_ID")
  if invocationID == "" { invocationID = uuid.NewString() }
  ac := ActiveContext{ …, InvocationID: invocationID }
  → tool.Handler(dm, ac, payload)                       cmd/mpm/call.go:249
  → recordToolInvocation(dm, ac, …)                     cmd/mpm/audit_hook.go:67
      (the audit row and the handler SHARE one id)

MCP transport                       cmd/mpm-mcp/tools.go:589-617
  callAC := ac
  if callAC.InvocationID == "" { callAC.InvocationID = uuid.NewString() }
  → handler(dm, callAC, payload)
  → recordToolInvocation(dm, callAC, …)                 cmd/mpm-mcp/audit_hook.go

core handler                       internal/core/tools/handlers.go
  → dm.RecordDecision(…, ac)                           ← the ac arrives here
  → dm.SaveLesson(…, ac)                                ← …and was dropped here
  → dm.ProposeTheory(…, ac)
  → dm.EndSession(…, ac)
  → dm.LogChangelogEntry(…, ac)

core write path
  → saveMemoryRow(…, ac)              internal/core/db.go:4186
      prov := dm.getEffectiveProvenance()   ← resolver base: InvocationID == ""
      → RecordArtifactProvenance(tx, id, artifactType, prov, false)
```

**The break is the last step.** `getEffectiveProvenance()` asks the resolver for
`Resolve("", "", "", "")` — no session, no invocation — so the resulting
`EffectiveProvenance.InvocationID` is always `""`. The `ac` the handler had been given
was never consulted.

### 2.2 The one path that worked, and why it was not enough

`internal/core/memory_tools.go` (pre-fix) bridged the context the only way it could:

```go
if ac.FrameworkName != "" || ac.Model != "" || ac.SessionID != "" || ac.InvocationID != "" {
    dm.perCallProvenanceOverride = dm.provenanceFromContext(ac)
    defer func() { dm.perCallProvenanceOverride = nil }()
}
```

`dm.perCallProvenanceOverride` is **shared mutable state on the `DatabaseManager`**. It
is process-wide, not call-scoped. A long-lived `mpm-mcp` server dispatches calls
concurrently: two overlapping saves would overwrite each other's override, and the loser
would stamp its artifact with the **winner's** invocation id. The one path that carried
identity was also the one path that could **conflate** identity.

So the defect had two faces, and both had to be fixed:

* **incompleteness** — the other recording paths lost the identity entirely;
* **unsoundness** — the path that kept it kept it in a way that could mix two calls up.

### 2.3 The two fabrications

A missing invocation id was never left missing. Two places minted one:

```go
// internal/core/db.go — provenanceFromContext (pre-fix; now db.go:6797)
if ac.InvocationID != "" {
    base.InvocationID = ac.InvocationID
} else if base.InvocationID == "" {
    base.InvocationID = GenerateID()        // ← a fabricated id
}
```

```go
// internal/core/db.go — AppendWorkEvent (pre-fix)
if invocationID == "" {
    if base := dm.getEffectiveProvenance(); base != nil && base.InvocationID != "" {
        invocationID = base.InvocationID
    } else {
        invocationID = GenerateID()        // ← a fabricated id
    }
}
```

A minted id joins to **no row** in `tool_invocations`. The provenance row therefore
looked attributed and attributed nothing — a strictly worse failure than an empty column,
because an empty column is visibly empty while a fabricated one is silently wrong. The
work path was affected: a work created by a background caller carried an invocation id
that no invocation row had ever claimed.

---

## 3. The audit: every recording path

Every path below was traced from the dispatcher to the artifact write. "Genuine id" means
the caller holds a real per-call invocation id that also exists in `tool_invocations`.

| Path | Tool | Invocation id available? | Pre-fix `artifact_provenance` | Pre-fix join |
|---|---|---|---|---|
| Memory | `mpm_memory.save` | **yes** | row written (via the shared override) | 50 / 54 |
| Decision | `mpm_decisions.record` | **yes** — `RecordDecision` already took `ac` | row written, `invocation_id` empty | **0 / 29** |
| Decision supersede | `mpm_decisions.supersede` | **yes** | row written (new decision row), empty | 0 |
| Theory proposal | `mpm_theories.propose` | **yes** | row written, empty | 0 |
| Lesson | `mpm_lessons.save` | **yes** | row written, empty | 0 / 6 |
| Changelog | `mpm_log_to_changelog` | **yes** | row written (as `memory`), empty | 0 |
| Promoted scratchpad | `mpm_scratchpad.promote` | **yes** | row written, empty | 0 |
| Skill | `mpm_skills.save` | **yes** | row written (as `memory`), empty | 0 |
| Work | `mpm_work.create/note` | **yes** | row written, id present | 3 / 3 |
| **Handoff** | `mpm_handoff.write`, `mpm_context.write_handoff` | **yes** | **NO ROW AT ALL** | 0 / 18 |
| Reference | `mpm_references.add/import` | yes | **no row** — no vocabulary member (§8.1) | 0 / 27 |
| Topic | `mpm_topics.create` | yes | **no row** — no vocabulary member (§8.1) | 0 / 1 |
| Evidence | `mpm_evidence.add` | yes | **no row** — no vocabulary member (§8.1) | 0 / 62 |
| Lifecycle ingest | `mpm_system.event_ingest` | yes | **no row — and must stay none** (§8.3) | n/a |

Two findings deserve their own line:

* **The handoff wrote no provenance row at all.** `handoff` has been a member of the
  canonical `artifact_type` vocabulary since the vocabulary was defined
  (`internal/core/schema.go:23`) — the path simply never used it. The continuity-writing
  action was invisible to the invocation join.
* **`mpm_decisions.supersede` is not in the hunt's recording vocabulary**, but it is a
  registered recording action: it writes a new decision row. Before D6 it was invisible
  to **both** of the hunt's signals. §9.4 uses it as the isolation case.

### 3.1 What was recoverable, and what was absent

The distinction the brief asks for:

* **Absent but recoverable** — decisions, theories, lessons, changelog entries, promoted
  scratchpads, skills. The identity existed in the `ActiveContext` the handler already
  held; it was simply not consulted. Recovering it required no new data and no new
  surface.
* **Absent and unrecoverable** — the reference, topic and evidence paths. Not because the
  identity was lost, but because `artifact_provenance.artifact_type` has no member for
  those artifacts, so there is nowhere to record it. §8.1.
* **Never present** — a direct `mpm` CLI command (`mpm skill add`) that does not go
  through the dispatcher writes no `tool_invocations` row at all. There is no invocation
  to point at. Those artifacts now record `invocation_id` NULL, which is the truth.

---

## 4. The identity model

Three rules, and nothing else:

1. **An invocation id is carried, never minted.** MPM records the identity it was given.
   It never manufactures one to complete a row. A missing invocation id is a fact about
   the world, not a gap in the record.
2. **One invocation may legitimately produce several artifacts.** A decision and a
   lesson written by the same call share that call's id — that is one turn of one agent,
   recorded honestly. The defect was the opposite: an id belonging to nobody.
3. **An artifact rewritten in place is attributed to its latest writer.** The handoff is
   UPSERTed on `session_id` and keeps its id; a skill version's id is deterministic and
   its content is replaced by a force-overwrite. In both cases a second write lands on
   the same `artifact_id`, so the provenance row must be **refreshed**. A plain INSERT
   would fail the `UNIQUE (artifact_id, artifact_type)` constraint and freeze the row on
   the *first* writer while the content belongs to the *latest* one — false attribution in
   the opposite direction.

### 4.1 The bridge

```go
// internal/core/db.go:457
func (dm *DatabaseManager) provenanceForWrite(ac ActiveContext) *EffectiveProvenance {
    if dm.perCallProvenanceOverride != nil {   // 1. explicit override (cascade)
        return dm.perCallProvenanceOverride
    }
    if ac.hasProvenanceIdentity() {            // 2. the caller's identity
        return dm.provenanceFromContext(ac)
    }
    return dm.getEffectiveProvenance()         // 3. the environment
}
```

Step 3 always yields `invocation_id` NULL — the resolver base has no invocation field to
carry. The bridge takes the identity as an **argument**, so two concurrent dispatches
cannot conflate: there is no shared state to race over.

---

## 5. What changed

| Change | Where | Why |
|---|---|---|
| `provenanceForWrite(ac)` — the single bridge | `internal/core/db.go:457` | one place decides where a write's provenance comes from |
| `hasProvenanceIdentity()` / `firstActiveContext()` | `internal/core/active_state.go:666,682` | "does this context know anything?" and the optional-argument idiom |
| `SaveMemoryNodeWithInvocation(node, …, ac, …)` | `internal/core/db.go:4163` | the tx-aware **and** identity-carrying memory write |
| `provenanceForWrite` used by `saveMemoryRow` and the lesson insert | `internal/core/db.go` | decisions, theories, changelog entries and promoted scratchpads write through `saveMemoryRow`; lessons through `AddLesson` |
| Variadic `ac ...ActiveContext` on `AddLesson`, `SaveLesson`, `ProposeTheory*`, `LogChangelogEntry*`, `EndSession`/`EndSessionV2` | `internal/core/*.go` | the identity is an **optional trailing argument**: zero churn at existing call sites, and the repo's own idiom (`expiresAt ...time.Time`) |
| `SaveSkillWithInvocation` | `internal/core/skill_db.go` | `SaveSkill` keeps its exact signature — `internal/core/seed` declares an interface with it and the seed package cannot import `internal/core` (import cycle) |
| `RecordArtifactProvenanceUpsert` | `internal/core/provenance_db.go:87` | refresh-in-place for artifacts whose identity is stable across rewrites |
| `refreshArtifactProvenance` | `internal/core/db.go:485` | the shared refresh used by the handoff and the skill force-overwrite |
| **Fabrication removed** in `provenanceFromContext` | `internal/core/db.go:6797` | a missing invocation id stays missing |
| **Fabrication removed** in `AppendWorkEvent` | `internal/core/db.go` | the same rule on the sibling table; `work_events.invocation_id` is nullable |
| The shared overlay **deleted** from the memory save | `internal/core/memory_tools.go` | it was shared mutable state — the conflation hazard |
| `contextAdapter` forwards the **per-call** context | `cmd/mpm-mcp/tools.go:543` | it used the captured process-level context, dropping the invocation id for every `mpm_context` action, `write_handoff` included |
| `ResolveFrameworkModelForInvocations` publishes the work-level fallback under the empty invocation id | `internal/core/db.go` | with the fabrication gone, a work event with no invocation id must still reach its work's own provenance row — the F16 contract, honestly |

**Not changed:** the database schema, the `artifact_provenance` DDL, the
`artifact_type` vocabulary, the tool registry, any tool's parameters or response shape,
the `tool_invocations` contract, the lifecycle event contract, the evidence identity
mechanism from PR #130, the confidence formula, the P3 hunt's recording vocabulary.

---

## 6. Behaviour changes the correction forces

These are real and intended.

1. **`artifact_provenance.invocation_id` is now populated on the recording paths.** Rows
   written after this change carry the id of the call that produced them. Rows written
   before it are untouched (§7).
2. **A work event with no invocation now carries `invocation_id` NULL** instead of a
   fabricated uuid. `work_events.invocation_id` is nullable for exactly this.
3. **The work's `artifact_provenance` row can carry `invocation_id` NULL.** A background
   caller has no invocation; the row now says so.
4. **A handoff now has an `artifact_provenance` row** (`artifact_type = 'handoff'`), and a
   rewritten handoff's row names the **latest** writer.
5. **`ResolveFrameworkModelForInvocations` may return an entry under the empty
   invocation id.** Its only production consumer looks up by the event's own invocation
   id, so an event without one now finds the work-level fallback. This is what keeps the
   F16 contract ("history surfaces framework/model") true after the fabrication is gone.
6. **Two `tool_invocations` rows can share one `invocation_id`** when one host shell
   command runs several `mpm call`s. This is a property of the *host integration*, not of
   the correction — §8.2.

---

## 7. Historical-data compatibility

* **No migration.** No schema change, so no table rebuild, no backfill.
* **No backfill of `invocation_id`.** A historical row with an empty `invocation_id`
  stays empty. The id of the call that produced it was never recorded anywhere, so
  inventing one would attribute a historical artifact to a call that never happened.
* **Historical rows remain readable.** `GetMemory`, the decision/lesson/theory readers
  and the work history surfaces are unchanged; D6 only adds a value to a column that was
  empty.
* **The hunt's behaviour on historical data is unchanged** for sessions recorded before
  the correction: their artifacts still do not join. That is correct — the information
  was never captured. What changes is that the hunt no longer *needs* the second signal
  to protect a session that recorded knowledge.

---

## 8. What is still not attributable

### 8.1 Three recording paths have nowhere to record their identity — **needs Session A**

`artifact_provenance.artifact_type` is a closed vocabulary, generated into the DDL's
`CHECK` constraint from `ProvenanceArtifactTypes`
(`internal/core/schema.go:23-33`):

```
memory, theory, lesson, decision, handoff, directive, work
```

There is no member for a **reference document**, a **topic** or an **evidence row**. All
three are recording actions in the P3 hunt's vocabulary (`mpm_references.add/import`,
`mpm_topics.create`, `mpm_evidence.add`), and all three write durable records:

| Artifact | Live rows | Vocabulary member |
|---|---|---|
| `reference_docs` | 27 | **none** |
| `topics` | 1 | **none** |
| `evidence` | 62 | **none** |

So their artifacts have no provenance row, and the join cannot reach them. **This is a
schema change** — the `CHECK` vocabulary and the table's canonical form — and therefore
**Session A's domain**, not this branch's.

> **Proposal to Session A (authorisation requested).**
>
> Widen `ProvenanceArtifactTypes` with `'reference'`, `'topic'` and `'evidence'`, and
> write a provenance row on the `mpm_references.add/import`, `mpm_topics.create` and
> `mpm_evidence.add` paths.
>
> *Mechanism (already built and tested):* the vocabulary is generated into the DDL from
> the Go slice, and `migrateArtifactProvenanceWorkType`
> (`internal/core/provenance_migration.go:280`) already rebuilds the table when the live
> `CHECK` vocabulary differs from the canonical set — the exact path the `'work'` widening
> took. The migration is fail-closed on non-canonical rows.
>
> *Cost:* one table rebuild per existing database (the `'work'` widening did the same),
> plus the three write-path bridges. No new column, no new index, no backfill.
>
> *Decision needed:* whether `evidence` belongs in `artifact_provenance` at all, or
> whether the evidence table should carry the invocation id directly. The second is
> arguably cleaner — an evidence row already has its own identity — and it is a different
> table from the one this defect is about.

**A skill and a changelog entry** are stored in the `memories` table, so their provenance
rows carry `artifact_type = 'memory'`. That is pre-existing and unchanged by D6; their
**invocation ids are now correct**, which is what D6 was about. A separate vocabulary
widening for `'skill'` / `'changelog'` is a cosmetic improvement, not a defect.

### 8.2 One host shell command, one invocation id — **needs Session A**

Measured end to end with the built binary against a disposable workspace:

```
$ mpm call mpm_lessons  …   # and, in the SAME shell command,
$ mpm call mpm_handoff …

decision|inv_7aad…|1|mpm_decisions
lesson  |inv_7bd5…|2|mpm_lessons,mpm_handoff      ← two tool_invocations rows, one id
handoff |inv_7bd5…|2|mpm_lessons,mpm_handoff
```

The OpenCode plugin's `shellEnv` hook calls `buildProvenanceEnv`, which mints
`MPM_PROVENANCE_INVOCATION_ID = inv_<uuid>`
(`agent_installation/mpm-opencode/src/index.ts:266`) **once per shell command** and
exports it into the shell. Every `mpm call` inside that command inherits it. Separate
shell commands get separate ids (verified).

The consequence: two distinct MPM tool invocations inside one host shell command share
one invocation id, so the join is one-to-many across that boundary. The **core** is
correct — it records exactly the identity it was given — but the identity is coarser
than the invocation.

> **Proposal to Session A (decision needed).**
>
> The documented semantics are `MPM_PROVENANCE_INVOCATION_ID` = "this invocation's unique
> ID" (`docs/SPEC.md:289`, `agent_installation/mpm-opencode/README.md:171`). A shell
> command is the *host's* invocation; the `mpm call` inside it is a child.
>
> *Option A (host-side, smallest):* the OpenCode plugin's `shellEnv` stops exporting
> `MPM_PROVENANCE_INVOCATION_ID` and exports `MPM_PROVENANCE_PARENT_INVOCATION_ID`
> instead. Each `mpm call` then mints its own id (`cmd/mpm/call.go:186-189` already
> falls back to `uuid.NewString()`) and records the shell command as its parent. The
> plugin's programmatic path is unaffected — it keeps `MPM_PROVENANCE_INVOCATION_ID`,
> which is correct there.
>
> *Option B (core-side):* `cmd/mpm/call.go` always mints its own invocation id and
> treats an inherited `MPM_PROVENANCE_INVOCATION_ID` as the parent. This changes the
> meaning of an env var used by **all five** agent installations and by the Claude Code
> integration, where the env id *is* the current invocation. It is a shared-dispatcher
> change and needs the compatibility matrix re-checked.
>
> **Option A is the recommendation**: it is confined to one host integration and leaves
> the env var's documented meaning intact. It is not implemented in this branch because
> the provenance-env contract is shared across the agent installations and the
> compatibility matrix is A's.

### 8.3 `mpm_system event_ingest` — correctly NOT a recording action

The lifecycle ingest records an **observation**. It creates no memory, no lesson, no
decision, no theory and — after this change — no `artifact_provenance` row. It is not in
the hunt's recording-call vocabulary, and it is not in the vocabulary this branch adds
anything to. §9.5 and §9.6 pin both halves: an ingest-only session is still reported by
the hunt, and the ingest writes no provenance row.

The distinctions the brief asks for are preserved:

| | produces an artifact? | in the hunt's recording vocabulary? |
|---|---|---|
| A tool invocation (`mpm_decisions.record`) | yes | yes |
| A lifecycle observation (`mpm_system.event_ingest`) | **no** | **no** |
| A knowledge-producing action (decision, lesson, theory) | yes | yes |
| A continuity-writing action (handoff) | yes | yes |
| A read-only action (`mpm_memory.query`) | no | no |

---

## 9. Test results

### 9.1 The regression suite

| # | Requirement | Test |
|---|---|---|
| 1 | Memory saves retain accurate invocation provenance | `TestD6_MemorySaveRetainsInvocationProvenance` |
| 2 | Decision writes retain their producing invocation id | `TestD6_DecisionWriteRetainsProducingInvocationID` |
| 3 | Other recording actions preserve available provenance | `TestD6_OtherRecordingActionsPreserveAvailableProvenance` (theory, lesson, handoff, skill, changelog) |
| 4 | Two artifacts from one invocation share that id | `TestD6_TwoArtifactsFromOneInvocationShareTheInvocationID` |
| 5 | Distinct invocations are never conflated | `TestD6_DistinctInvocationsAreNeverConflated` |
| 6 | Missing provenance stays missing, not synthesized | `TestD6_MissingProvenanceStaysMissing`, `TestD6_NoInvocationIDIsFabricatedAnywhere` |
| 7 | Retries and replays cause no false attribution | `TestD6_RetriesAndReplaysDoNotCreateFalseAttribution` |
| 8 | The P3 hunt does not flag a session that recorded knowledge | `TestD6_MissedOpportunity_SessionThatRecordedIsNotFlagged` (+ the "recorded nothing" control) |
| 9 | Lifecycle ingestion is not knowledge preservation | `TestD6_LifecycleIngestionIsNotKnowledgePreservation`, `TestD6_MissedOpportunity_LifecycleIngestionIsNotARecording` |
| 10 | Historical records remain accessible without backfill | `TestD6_HistoricalRecordsRemainAccessibleWithoutBackfill` |

Plus the transport-level test
`TestD6_ContextAdapterForwardsThePerCallInvocation` (`cmd/mpm-mcp`), and the tool-surface
test `TestD6_ToolHandlersThreadTheInvocationContext` (`internal/core/tools`).

Every test uses a **disposable file-backed database** and an **isolated workspace root**
(`HOME`, `MPM_WORKSPACE`, `MPM_DB_PATH` all redirected into `t.TempDir()`).

### 9.2 The hunt's own characterisation test was inverted

`internal/critic/p3_missed_opportunity_test.go` carried
`TestP3ArtifactProvenance_InvocationIDIsPopulatedOnlyByTheMemorySavePath`, which
**asserted the defect**: it required the decision's `invocation_id` to be empty, and
documented that it would fail once the defect was fixed. It is now
`TestP3ArtifactProvenance_InvocationIDIsPopulatedOnEveryRecordingPath`, and it asserts
the corrected behaviour — the decision, the lesson, the theory and the handoff all carry
the invocation id and all join to the `tool_invocations` row.

### 9.3 Non-vacuity — proven, not asserted

Each probe reinstates one mechanism of the defect and shows the tests fail. The tree was
restored byte-identically after each probe (`diff` against a backup).

**Probe 1 — the bridge disabled** (`provenanceForWrite` ignores the `ActiveContext`):

```
--- FAIL: TestD6_MemorySaveRetainsInvocationProvenance
      memory provenance invocation_id = "", want "inv_mem_1"
--- FAIL: TestD6_DecisionWriteRetainsProducingInvocationID
      decision provenance invocation_id = "", want "inv_dec_1"
--- FAIL: TestD6_OtherRecordingActionsPreserveAvailableProvenance
      theory|lesson|handoff|skill|changelog provenance invocation_id = "", want "inv_all_1"
--- FAIL: TestD6_TwoArtifactsFromOneInvocationShareTheInvocationID
--- FAIL: TestD6_DistinctInvocationsAreNeverConflated
--- FAIL: TestD6_RetriesAndReplaysDoNotCreateFalseAttribution
--- FAIL: TestD6_HistoricalRecordsRemainAccessibleWithoutBackfill
--- FAIL: TestD6_ToolHandlersThreadTheInvocationContext        (internal/core/tools)
--- FAIL: TestD6_MissedOpportunity_SessionThatRecordedIsNotFlagged  (internal/critic)
```

**Probe 2 — the fabrication reinstated** (the bridge intact):

```
--- FAIL: TestD6_MissingProvenanceStaysMissing
      memory provenance invocation_id = "a98cc49189629095", want EMPTY
      — a caller that knows the framework does not thereby know the invocation
--- FAIL: TestD6_NoInvocationIDIsFabricatedAnywhere
      work provenance invocation_id = "f4a3cde4ccd329e0", want EMPTY
      work event invocation_id = "f4a3cde4ccd329e0", want EMPTY
      1 provenance rows carry an invocation id that joins to no tool_invocations row
```

**Probe 3 — the MCP adapter drops the per-call context again**:

```
--- FAIL: TestD6_ContextAdapterForwardsThePerCallInvocation
      handoff provenance invocation_id = "", want "inv_mcp_1"
```

### 9.4 The hunt's second signal now works on its own

`mpm_decisions.supersede` is a **registered recording action that the hunt's
recording-call vocabulary does not name** (the vocabulary has `mpm_decisions.record`).
It writes a new decision row. Before D6 it was invisible to **both** of the hunt's
signals — the recording-call signal missed it, and the join missed it because the
decision's provenance carried no invocation id. The session was reported as a gap.

`TestD6_MissedOpportunity_SessionThatRecordedIsNotFlagged` drives that exact shape: six
read-only invocations, one `mpm_decisions.supersede` invocation, and a decision written
through the production path with that invocation's identity. The session is **not**
reported.

The hunt's vocabulary is **not** weakened. Two controls pin that:

* `TestD6_MissedOpportunity_SessionThatRecordedNothingIsStillFlagged` — a session that
  did six reads and recorded nothing is still reported.
* `TestD6_MissedOpportunity_LifecycleIngestionIsNotARecording` — a session that only
  ingested lifecycle observations is still reported.

### 9.5 End-to-end, through the real binary

Against a disposable workspace, using the built `mpm` binary:

```
$ mpm call mpm_decisions --payload '{"action":"record","params":{…}}'
$ mpm call mpm_lessons   --payload '{"action":"save","params":{…}}'
$ mpm call mpm_handoff   --payload '{"action":"write","params":{…}}'

artifact_type  invocation_id                    joins
decision       inv_7aad0d20-…-8f38-e567b9a824a5   1  (mpm_decisions.record)
lesson         inv_7bd5e555-…-579d55422838       1  (mpm_lessons.save)
handoff        inv_7bd5e555-…-579d55422838       1  (mpm_handoff.write)
```

Every artifact joins to the invocation that produced it. (The lesson and the handoff
share an id because they were run in **one** shell command — §8.2.)

### 9.6 Gates

| Gate | Result |
|---|---|
| `internal/core` (`go test -tags fts5`) | **ok** — 223.5 s |
| `internal/core/tools` (`go test -tags fts5`) | **ok** — 40.8 s |
| root module (`go test -tags fts5 ./...`) | **ok** — all 17 packages |
| `go vet ./...` | clean |
| `gofmt` | clean on every file this branch touches (the pre-existing gofmt drift in other files is untouched) |
| `mpm-lint --gate` | **PASS** (all classes; the one `no-check` site this branch introduced was restructured to satisfy the scans rule) |
| `make test-scripts` | **OK** — 540 tests, 1 skipped |
| `make test-agent-installation` | **OK** — 494 Python + 89 TS tests |
| `make test-race` | see §11 |

---

## 10. The P3 hunt: both signals, one of them repaired

The hunt asks "which sessions did MPM-mediated work without recording anything durable?"
and answers it with two independent signals:

1. **the recording-call signal** — the tool/action vocabulary
   (`internal/critic/missed_opportunity.go:122-137`);
2. **the provenance join** — `LEFT JOIN artifact_provenance ap ON ap.invocation_id =
   ti.invocation_id` (`internal/critic/missed_opportunity.go:308`).

Before D6, signal 2 was structurally blind for every path except the memory save, which
is why signal 1 existed. D6 **repairs signal 2**; it does **not** remove signal 1. Signal
1 still covers the recording actions that write no provenance row at all (§8.1) and the
ones whose tool.action the vocabulary does not name (§9.4).

The hunt's `HAVING` clause is unchanged: a session is reported only when it has **no**
recording call **and** **no** attributable artifact. D6 makes the second condition
meaningful for the paths it covers.

---

## 11. CI

Appended after the CI run: see §13.

---

## 12. Interactions with the other M5 defects

| Defect | State after this branch |
|---|---|
| **D6** incomplete invocation→artifact provenance | **CLOSED** for every recording path the substrate can attribute; two residual gaps are escalated in §8 with proposals |
| D1 duplicate evidence inflates confidence | CLOSED by PR #130 — untouched here |
| D5 repeated delivery counted as corroboration | CLOSED by PR #130 — untouched here |
| D2 trusted-lesson surface has no pending state | OPEN (not touched) |
| D3 unresolvable / mislabelled citations | OPEN (not touched) |
| D4 cycle-specific tags defeat proposal idempotency | OPEN (not touched) |
| D7 silent `[]string` parsing | OPEN (not touched) |
| telemetry hunt creates trusted lessons | OPEN (A's decision pending; not touched) |

**The P2b scheduler gate stays as A left it** — not enabled, not disabled, not touched.

**The evidence identity mechanism from PR #130 is not touched.** `evidence_identity.go`
and `evidence_store.go` are unchanged; the only interaction is that an evidence row is
still not an `artifact_provenance` row (§8.1).

---

## 13. What was NOT done

* **No schema change.** The `artifact_provenance` DDL, its `CHECK` vocabulary and its
  indexes are untouched. The three paths that need a vocabulary member are escalated in
  §8.1, not implemented.
* **No migration, no backfill.** Historical rows keep their empty `invocation_id`.
* **No tool-registry change**, no new tool parameter, no CLI flag, no change to any
  response shape.
* **No change to the lifecycle event contracts**, the event store, the ingest handler, or
  the `mpm_system` tool's vocabulary.
* **No change to the P3 hunt's recording vocabulary** — signal 1 is untouched and signal 2
  is repaired.
* **No change to the OpenCode plugin** (§8.2 is a proposal, not an implementation).
* **No change to the live installation, the production database or any other agent's
  worktree.** Every measurement of the live database was **read-only**; every test
  writes to a disposable file-backed database under `t.TempDir()`.
* **D3, D4, D7 and the telemetry-hunt trust corrections were not combined with this
  work.**

---

## 14. Commit and PR

* Branch: `feat/session-b-d6-invocation-provenance`, from `origin/main` = `3584190`.
* The correction and its tests: **`552c86d`** —
  *"fix(core): carry the invocation identity into every recording path (D6)"*.
* This report: the commit that follows it.
* PR: `https://github.com/flowbyte-com/mpm/pull/132`, base `main`, head `158de35`, MERGEABLE.
* Merge authority is Session A's.
