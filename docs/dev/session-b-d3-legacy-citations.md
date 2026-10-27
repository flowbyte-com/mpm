# Session B — D3: the legacy citation path must record what it actually cites

**Branch:** `feat/session-b-d3-legacy-citations` (from `origin/main` = `3abfdda`)
**Status:** D3 **split**. **D3-b is CLOSED** by a backward-compatible correction: a legacy
citation now records the type of the artifact it actually cites. **D3-a remains OPEN** and
needs Session A's compatibility ruling — the evidence and the retirement proposal are in §6.
**Scope:** the legacy citation path's type resolution. No schema change, no migration, no
registry change, no new tool, no rejection of anything that was accepted before. D1, D4,
D5, D6, D7 and the P2b scheduler gate are untouched; interactions are reported in §8.

---

## 1. Summary

`mpm_theories propose` and `mpm_decisions record` accept a list of cited source ids. A
citation may carry a **declared type**, and when it does, the substrate validates it: the
type must be in the canonical vocabulary, and the cited id must resolve **as that type**.
A citation that fails either check is **rejected**, and the rejection leaves no partial
proposal behind. That is the typed path, and it is correct.

When the caller omits the type, the citation takes the **legacy** path: the id is written
unvalidated, and its type is *inferred* at write time. The inference was broken for two
of the six canonical types.

The resolver the legacy path used — `ResolveArtifactType` — knows two things: the
`memories` table (by its `collection` discriminator) and the lessons table. The canonical
**citation** vocabulary knows six:

| Canonical citation type | Where the row lives | Legacy resolver finds it? |
|---|---|---|
| `memory` | `memories` (`collection` ∈ `''`, `memories`) | **yes** |
| `theory` | `memories` (`collection = 'theories'`) | **yes** |
| `decision` | `memories` (`collection = 'decisions'`) | **yes** |
| `lesson` | `lessons_base` | **yes** |
| `work` | `works` | **NO** |
| `tool_invocation` | `tool_invocations` | **NO** |

A legacy citation of a tool invocation or of a work item therefore fell through the
resolver and landed on the `"memory"` fallback.

**That is not a label of convenience. It is a false record.** The id names a tool
invocation; the row says it is a memory. A reader could not tell a citation of a tool
invocation from a citation of a memory — and the typed path, which asks the *very same*
questions, resolves the same id as `tool_invocation`.

The correction makes the legacy path ask the canonical vocabulary the same questions the
typed path asks. It **rejects nothing**, **changes no interface**, and **rewrites no
historical row**.

---

## 2. Root cause

### 2.1 The two paths, side by side

`internal/core/epistemology_tools.go:765-797` — `recordCitationsNode` — is the single
citation writer. It branches on the citation's declared type:

```go
if c.Type == "" {
    // LEGACY: exactly the historical behaviour.
    if err := dm.recordProvenanceNode(node, c.ID, "", downstreamID, downstreamType, downstreamID, ""); err != nil {
        return err
    }
    continue
}
// TYPED: the declared type must be known, and the cited id must resolve AS THAT TYPE.
if !ValidCitationSourceType(c.Type) { … }
resolved, err := citationSourceExists(node, c.ID, c.Type)
if !resolved {
    return fmt.Errorf("citation source %q does not resolve as type %q", c.ID, c.Type)
}
```

Both paths end at `recordProvenanceNode`, and both write the same row. The difference is
in how `source_type` is decided:

* **TYPED** — the declared type, written **verbatim**, after the id was proven to resolve
  as that type.
* **LEGACY** — `sourceType == ""` at the write, so `recordProvenanceNode` resolves it:

```go
// pre-fix
if sourceType == "" {
    resolved, err := dm.ResolveArtifactType(sourceID)
    if err != nil {
        sourceType = "memory"   // the fallback
    } else {
        sourceType = resolved
    }
}
```

### 2.2 The resolver's blind spot

`ResolveArtifactType` (`internal/core/cascade_provenance.go`) resolves in this order:

1. `SELECT collection FROM memories WHERE id = ? AND deleted_at IS NULL` →
   `collectionToArtifactType(collection)`.
2. `SELECT id FROM lessons_base WHERE id = ?` → `lesson`.
3. (shared mode) `shared.memories` → the same collection mapping.
4. otherwise → the error that makes the caller fall back to `"memory"`.

It has no lookup for `works` and none for `tool_invocations`, because it was written for
the **cascade materializer**, whose dispatch vocabulary is the artifact kinds it
invalidates. The citation vocabulary is wider, and the two were never reconciled.

The fallback is not a bug. It is documented, deliberate, and load-bearing:

> *Fall back to "memory" — better than failing the whole write when a legacy caller cites
> an ID that has since been shredded.*

So the resolver's failure mode was always "record it as a memory", and for two of the six
canonical types that fallback is simply **wrong**.

### 2.3 Why the shared resolver was not widened

`ResolveArtifactType` has exactly two callers:

| Caller | What it does with the answer |
|---|---|
| `recordProvenanceNode` (the legacy citation path) | the `source_type` of a citation row |
| `cmd/mpm/evidence_cmds.go:153` (`mpm evidence add`) | the `artifact_type` of an evidence row |

The second one is a trap. The `evidence` table's constraint is:

```sql
artifact_type TEXT NOT NULL CHECK (artifact_type IN ('memory','theory','decision','lesson','work'))
```

`tool_invocation` is **not** in that list. Widening the shared resolver would have made
`mpm evidence add --artifact-id <a tool invocation id>` resolve successfully and then
**fail the CHECK constraint** — turning a clear, actionable error

> `--artifact-type is required when --artifact <id> cannot be resolved to a known kind`

into a raw SQLite constraint failure. That is why the correction adds a
**citation-scoped** resolver instead of widening the shared one.

---

## 3. The audit: every production caller of the legacy path

The legacy path is reachable from exactly three surfaces. The enumeration is exhaustive:
`recordCitationsNode` is called from `recordSourceCitationsNode` (legacy) and from
`recordCitationsNode` (typed), and `recordSourceCitationsNode` has exactly one caller.

| # | Surface | How the legacy path is reached | Depends on permissiveness? |
|---|---|---|---|
| 1 | `mpm_theories.propose` **without** `source_type` | `handlers.go` → `dm.ProposeTheory` → `ProposeTheoryWithExtras` → `proposeTheory(legacyCitations(sourceIDs))` | **YES** — a caller that cites an id that does not resolve today gets a successful proposal. Rejecting it is D3-a. |
| 2 | `mpm_decisions.record` **without** `source_type` | `handlers.go` → `dm.RecordDecision` → `recordSourceCitationsNode` → legacy | **YES** — same shape. |
| 3 | `mpm_decisions.supersede` | `handlers.go` → `dm.SupersedeDecision` → `RecordDecision` with `allSourceIDs = [originalID] + sourceIDs` → legacy | The **injected** citation (the superseded decision) resolves by construction; the caller's own `source_ids` do not. |

Everything else is already on the validated path:

| Surface | Path | Validated? |
|---|---|---|
| `mpm_theories.propose` **with** `source_type` | `dm.ProposeTheoryWithCitations` → typed | **yes** |
| The critic's **four hunts** (survival-asymmetry, stale-memory, weak-theory, poison-pill, missed-opportunity) | they emit `source_ids` **and** `source_type` → typed | **yes** |
| The cascade materializer | `ProposeTheoryWithExtras(…, nil, …)` — **no** source ids at all | n/a — it cites its foundation through the `dependencies` column, not through `epistemic_provenance` |
| `cmd/mpm/self_heal.go:224` | `dm.ProposeTheory(hypothesis, criteria, nil, nil, tags)` — **no** source ids | n/a |
| `cmd/mpm/handlers_cognitive_verbs.go:350`, `cmd/mpm/handlers_epistemology.go:428` | `dm.SupersedeDecision(…, nil, …)` | the injected citation only |

**The conclusion the brief asks for:** the *automatic*, untrusted emitter — the critic —
already uses the validated typed path on every hunt. The permissive path is reachable
**only** by an explicit caller choice (omitting `source_type`), and the only two
production callers that could migrate without a caller change are the decision surfaces,
whose registry has no `source_type` parameter at all.

### 3.1 The registry gap

`internal/core/tools/registry_list.go` declares:

| Tool | `source_ids` | `source_type` |
|---|---|---|
| `mpm_theories` → `propose` | ✅ `{"type":"array","items":{"type":"string"}}` | ✅ `{"type":"string","enum":["memory","theory","decision","lesson","work","tool_invocation"]}` |
| `mpm_decisions` → `record` / `supersede` | ✅ | ❌ **absent** |

So a **decision's citations can never be typed**, and therefore can never be validated.
That is a concrete, identified gap — and it is a **tool-registry change**, which is
Session A's shared interface. It is D3-c in §6.

---

## 4. The correction

`internal/core/cascade_provenance.go` — one new resolver, used by the legacy branch only:

```go
func (dm *DatabaseManager) resolveLegacySourceType(sourceID string) string {
	if resolved, err := dm.ResolveArtifactType(sourceID); err == nil {
		return resolved
	}
	for _, t := range legacyCitationExtraTypes {   // work, tool_invocation
		query, ok := citationSourceQueries[t]
		if !ok { continue }
		var one int
		if err := dm.db.QueryRow(query, sourceID).Scan(&one); err == nil {
			return t
		}
	}
	return legacyCitationTypeFallback            // "memory" — unchanged
}
```

Three properties, each chosen deliberately:

* **It asks the canonical vocabulary the same questions the typed path asks**
  (`citationSourceQueries`). The legacy auto-resolution and the typed validation can
  therefore never disagree about what an id *is* — a property the test suite pins
  directly.
* **The common case costs what it always cost.** `ResolveArtifactType` is asked first and
  answers for every memory, theory, decision and lesson id, so those citations pay the
  same two queries they paid before. The two extra lookups happen only for an id the
  historical resolver could not place — which is exactly the case being corrected.
* **Nothing is rejected.** An id that resolves to nothing still lands as `"memory"`, and
  the write still succeeds. See §6.

---

## 5. Behaviour changes

| Cited id | Pre-D3 | Post-D3 |
|---|---|---|
| a memory | `source_type = "memory"` | **unchanged** |
| a theory | `source_type = "theory"` | **unchanged** |
| a decision | `source_type = "decision"` | **unchanged** |
| a lesson | `source_type = "lesson"` | **unchanged** |
| **a tool invocation** | `source_type = "memory"` | **`source_type = "tool_invocation"`** |
| **a work item** | `source_type = "memory"` | **`source_type = "work"`** |
| an id that resolves to nothing | `source_type = "memory"`, **accepted** | **unchanged** |
| a typed citation (any type) | declared type, verbatim | **unchanged** |
| a typed citation that does not resolve | **rejected**, no partial row | **unchanged** |

Nothing is rejected that was accepted before. No interface changed. No historical row is
rewritten — a historical citation of a tool invocation keeps its (wrong) `"memory"` type,
because rewriting it would need a migration, and a migration needs A's approval.

### 5.1 Who reads the recorded `source_type`

The change is only safe if nothing depends on the *wrong* value. Every reader was checked:

| Reader | Uses `source_type` how? | Affected? |
|---|---|---|
| `cascade_outbox.go:611-628` — `discoverCascadeTargets` | filters on **downstream** `_type`, not on `source_type` | **no** |
| `cascade_outbox.go:857-872` — `discoverPositiveCascadeTargets` | filters on `polarity` and reads `downstream_id`/`downstream_type` | **no** |
| `contextual_candidates_sources.go:372` | selects `source_type`; the candidate's **kind** comes from `downstream_type` | **no** |
| `work_purge.go:313-331` | renders `"cites this work (source_type=…)"` into a human-readable string | **no** — the string becomes accurate |
| `cascade_provenance.go:261-320` — `listDownstreamCitationsOn` | returns `source_type` as data; the cascade's target discovery uses `downstream_type` | **no** |

**Nothing branches on the value.** The column is descriptive, and the correction makes the
description true.

### 5.2 The compatibility guarantee this correction amends — **Session A must see this**

Two P2 tests pin the legacy path's auto-resolved value **for a tool invocation**:

| Test | Pinned value |
|---|---|
| `internal/core/tools/p2_citation_tool_test.go` — `TestP2Tool_HandleProposeWithoutSourceTypeIsLegacy` | `source_type = "memory"` |
| `cmd/mpm-mcp/p2_citation_mcp_test.go` — `TestP2MCP_LegacyCitationIsUnchangedOverMCP` | `source_type = "memory"` |

Both say, in their own words, that *callers that do not supply `source_type` must be
unaffected*. **The correction changes that recorded value**, and the two tests are
amended in this PR.

**The conflict is direct and unavoidable.** The M5 contract suite says the citation must
preserve `tool_invocation`; the P2 suite says the same citation must record `memory`.
Both cannot hold. The M5 contract test is the red suite that *defines the defect*; the P2
test pins the *current* behaviour, which is the behaviour the defect produced.

**What the amendment preserves — and strengthens.** Each amended test now asserts the
legacy path's actual fingerprint, which is the **`event_id`**: on the legacy path it is
the **downstream** id, on the typed path it is the **cited** id. So:

* the test still proves the **legacy** path ran (a handler that routed the call to the
  typed path would record a different `event_id` and fail);
* it still proves the call **succeeded** and the citation was **recorded** — the
  caller's experience is unchanged;
* it no longer asserts a value that is **wrong**; and
* the typed-path test's non-vacuity is preserved through the same `event_id` assertion —
  its own comment, which said the `source_type` value was what discriminated the two
  paths, is corrected, because after D3 it is the `event_id` that does.

**Nothing is rejected, no interface changed, and no historical row is rewritten.** What
changed is one recorded *value*, from a mislabel to the truth, for two of the six canonical
types.

**If Session A prefers the P2 guarantee over the M5 contract on this point**, the
correction must be dropped and D3-b re-classified as an accepted residual. That is a
one-line revert of `internal/core/cascade_provenance.go`'s legacy branch plus the two
test amendments; §6.3's step 1 (make the typed path the default) would then be the only
way to reach the correct type, and it needs the same ruling.

---

## 6. D3-a: the open half, and the retirement proposal

### 6.1 What is still wrong

A legacy citation of an id that resolves to **nothing at all** is still accepted, and the
proposal still succeeds:

```
mpm_theories propose {"hypothesis":"…","source_ids":["no-such-artifact-…"]}
→ {"success": true, "id":"…"}
```

The citation row is written with `source_type = "memory"` — the historical fallback.

### 6.2 Why it is not corrected here

The fallback is **load-bearing**, and its documented reason is explicit:

> *Fall back to "memory" — better than failing the whole write when a legacy caller cites
> an ID that has since been shredded.*

Rejecting an unresolvable id would reject **both** of these cases, and they are not the
same thing:

| Case | Today | Under a rejection rule |
|---|---|---|
| the id **never existed** (a typo, a hallucinated id) | accepted, recorded as a memory | rejected — **correct** |
| the id **has since been shredded** (a real artifact, now gone) | accepted, recorded as a memory | rejected — **breaks the documented fallback** |

The substrate cannot currently tell them apart: `ResolveArtifactType` filters on
`deleted_at IS NULL`, so a soft-deleted row and a never-existing row look identical to it.
Distinguishing them would need a separate "does the row exist at all?" read — which is
possible, and is the shape of the retirement below.

**And the brief is explicit:** *"Do not globally tighten legacy behaviour without an
explicit compatibility decision."* Rejecting in the legacy path tightens it for every
caller of `mpm_theories propose` and `mpm_decisions record` at once — a public-tool
behaviour change that turns today's successes into errors. That is Session A's call.

### 6.3 The retirement proposal — for Session A's ruling

Three steps, in order of increasing commitment. **Step 1 is recommended.**

**Step 1 — make the typed path the DEFAULT for `mpm_theories propose` (no registry
change).**

The tool already accepts `source_type`; the schema already declares it. A caller that
omits it today gets the permissive path. The proposal is: when `source_type` is omitted
**and** every cited id resolves unambiguously to exactly one canonical type, the substrate
uses that resolved type — so the citation is **validated** without the caller having to
change anything.

* An id that resolves → the citation is typed, and therefore validated. **An unresolvable
  id still falls back** — so the shredding case keeps working, and the "never existed"
  case keeps working too. This is **not** a tightening.
* The recorded `source_type` becomes explicit instead of inferred-and-possibly-wrong.
* The **only** behaviour that changes is the *validation* of an id that resolves: today a
  caller can declare `source_type: "memory"` for an id that is really a tool invocation
  and it is rejected — with the default, that caller's citation is simply typed
  correctly.

**Step 2 — add `source_type` to `mpm_decisions record` and `supersede` (registry change —
A's shared interface).**

This is the concrete gap in §3.1: a decision's citations can never be typed today, so they
can never be validated. Adding the parameter mirrors `mpm_theories.propose` exactly — the
same enum, the same optionality, the same typed write path.

**Step 3 — reject a legacy citation of an id that never existed (the compatibility
decision).**

Only after steps 1 and 2, and only with A's explicit approval, the legacy path could
reject an id that resolves to nothing **and** has no row at all (soft- or hard-deleted).
This is the change that turns today's successes into errors, so it needs the sunset below.

**The sunset, if step 3 is ever approved:**

1. A deprecation notice on the legacy path — a warning in the tool's response envelope,
   naming the typed parameter — for one release.
2. The legacy path rejects an id that resolves to nothing.
3. The legacy path is removed; `source_type` becomes required.

### 6.4 What the correction in this PR already buys

Even without any of the three steps, D3-b closes a **contract failure**:
`TestM5Contract_CitationPreservesItsType` now passes, because the legacy path records
`tool_invocation` for a tool invocation instead of `memory`.

---

## 7. Regression coverage

`internal/core/d3_legacy_citation_test.go` — 4 tests, disposable file-backed databases in
isolated `HOME` / `MPM_WORKSPACE` / `MPM_DB_PATH` roots.

| Test | What it pins |
|---|---|
| `TestD3_LegacyCitationRecordsTheTypeItCites` | **the correction** — a legacy citation of a tool invocation records `tool_invocation`, of a work item records `work`, of a memory records `memory` |
| `TestD3_LegacyCitationOfAnUnresolvableIdIsStillAccepted` | **the compatibility guarantee that is NOT changed** — an unresolvable legacy citation is still accepted and still recorded as `"memory"`. This is the guard that keeps D3-a visible: tighten the legacy path and this test fails, forcing the decision to be explicit. |
| `TestD3_TypedPathIsUnchanged` | the typed path still records the declared type verbatim and still rejects an unresolvable citation with no partial proposal |
| `TestD3_LegacyAndTypedPathsAgreeOnTheType` | **the invariant that makes the correction safe** — the legacy auto-resolution and the typed validation reach the same answer for the same id |

### 7.1 Non-vacuity — proven, not asserted

In an isolated git worktree (`/tmp/opencode/d3-nonvacuity`, detached at the D3 head,
restored and removed afterwards), the correction was reverted — the canonical vocabulary
is no longer consulted:

```
--- FAIL: TestD3_LegacyCitationRecordsTheTypeItCites
    tool invocation: citation source_type = "memory", want "tool_invocation" — a citation must record the type of what it actually cites
    work item: citation source_type = "memory", want "work" — a citation must record the type of what it actually cites
--- FAIL: TestD3_LegacyAndTypedPathsAgreeOnTheType
    tool invocation: the legacy path recorded "memory" and the typed path recorded "tool_invocation" — the two paths must agree about what an id IS
```

And the M5 contract test goes red with the defect reinstated, which is the point of the
contract suite:

```
--- FAIL: TestM5Contract_CitationPreservesItsType
    expected the citation to preserve source_type 'tool_invocation' (defect D3), got "memory"
```

With the correction in place, that contract test **passes**.

### 7.2 The contract suite after this PR

```
--- FAIL: TestM5Contract_UnresolvableCitationIsRejected        ← D3-a, OPEN (needs A)
--- FAIL: TestM5Contract_CycleVaryingTagDoesNotCreateSecondProposal  ← D4, OPEN (needs A)
```

`TestM5Contract_CitationPreservesItsType` — **D3-b — now passes.** The contract suite has
gone from three failures to two, and no test was weakened, inverted or deleted.

---

## 8. Interactions with the other M5 defects

* **D1 / D5 (evidence identity, PR #130)** — untouched. The evidence row's identity is a
  different mechanism from the citation's type.
* **D2 (no pending lesson state)** — untouched. The lesson surface is not written by this
  path.
* **D4 (proposal identity)** — untouched, and it is a separate PR.
* **D6 / D7** — untouched and on their own branches.
* **P2 (the typed citation path)** — **untouched by construction**: the correction is in
  the legacy branch only, and `TestD3_TypedPathIsUnchanged` pins it.

---

## 9. What was NOT done

* No schema change, no migration, no backfill. A historical citation of a tool invocation
  keeps its (wrong) `"memory"` type; rewriting it would need a migration, and a migration
  needs A's approval.
* No registry change. `mpm_decisions`' missing `source_type` is reported as D3-c, not
  changed here.
* No change to `ResolveArtifactType`, so `mpm evidence add`'s behaviour is untouched — and
  the `evidence` table's `artifact_type` CHECK stays valid.
* **No rejection of anything that was accepted before.** D3-a is a compatibility decision,
  and §6.3 is the proposal for it.
* No change to the typed path.
* No live-workspace or installed-runtime modification. Every test used a disposable
  file-backed database in an isolated `t.TempDir()` root.
* No merge, no deployment.

---

## 10. Commit, PR and results

| | |
|---|---|
| Branch | `feat/session-b-d3-legacy-citations` |
| Base | `origin/main` = `3abfdda` |
| PR | **#141** — **MERGED** into `main` as `434e47e` |
| Commits | `0643b22` — *fix(core): a legacy citation must record the type of what it cites (D3)*<br>`2495ed8` — *docs(dev): the D3 report*<br>`e826290` — *test(m5): the legacy path's fingerprint is its event_id, not its mislabel* |

Files changed:

| File | Change |
|---|---|
| `internal/core/cascade_provenance.go` | the legacy branch resolves through `resolveLegacySourceType`; the new resolver, the extra-type list and the fallback constant |
| `internal/core/d3_legacy_citation_test.go` | **new** — 4 regression tests |
| `internal/core/tools/p2_citation_tool_test.go` | **amended** — the legacy-path test now asserts the legacy `event_id` fingerprint and the corrected `source_type` (§5.2) |
| `cmd/mpm-mcp/p2_citation_mcp_test.go` | **amended** — the same, over MCP; plus a corrected comment on the typed-path test |
| `docs/dev/session-b-d3-legacy-citations.md` | **new** — this report |

### 10.1 Test results

Every suite below was run against the branch head `e826290`, on a disposable file-backed
database in an isolated `HOME` / `MPM_WORKSPACE` / `MPM_DB_PATH` root.

| Suite | Command | Result |
|---|---|---|
| `internal/core` (+ 10 sub-packages) | `go test -tags fts5 -count=1 ./...` | **ok** — `mpm-core` 222.0s, `capability` 7.2s, `orchestration` 0.2s, `seed` 1.8s, `synth` 8.1s, and 5 more; **zero failures** |
| `internal/core/tools` | `go test -tags fts5 -count=1 ./...` | **ok** 33.9s — zero failures |
| root module (17 packages) | `go test -tags fts5 -count=1 ./...` | **ok** — `cmd/mpm` 393.2s, `cmd/mpm-mcp` 7.2s, `cmd/mpm-telemetry` 13.6s, `internal/critic` 4.3s, `internal/scheduler` 22.6s, `internal/testenv` 5.0s, and 11 more; **zero failures** |
| the M5 contract suite | `go test -tags 'fts5 m5contract' -count=1 -run 'TestM5Contract'` | **2 failures, both OPEN by design** — `UnresolvableCitationIsRejected` (D3-a) and `CycleVaryingTagDoesNotCreateSecondProposal` (D4, PR #140). `CitationPreservesItsType` — D3-b — **passes**. |
| the M5 characterization suite | `go test -tags fts5 -count=1 -run 'TestM5Characterization'` | **ok** — all three pass; the defective behaviours remain pinned as characterizations |
| `go vet ./...` | — | clean |
| `gofmt -l` on the touched files | — | clean |
| `mpm-lint --gate` | — | **PASS** |
| `make test-race` | — | see §10.2 |
| CI (PR #141) | — | see §10.2 |

### 10.2 The race-detector gate and CI

| Gate | Result |
|---|---|
| `make test-race` | **PASS** — `EXIT=0`, **24 Go packages ok under `-race`, zero `FAIL` and zero `DATA RACE`**; `make test-scripts` OK (540 tests, floor 50), `make test-bench` OK (217 tests, floor 50), `make test-agent-installation` OK (503 tests, floor 300), `make test-agent-adapters` OK (Python 312 / floor 150, JS 164 / floor 100, TS 89 / floor 15) |
| CI — Build + core tests | **SUCCESS** — 19:25:28Z → 19:39:48Z (14m20s) |
| CI — Full test suite | **SUCCESS** — 19:25:29Z → 19:36:55Z (11m26s) |
| CI — Release gate (cross-agent continuity + public-CLI subprocess suite) | **SUCCESS** — 19:25:28Z → 19:47:43Z (22m15s) |

**CI run `40568962538`, at head `9b00dc9` — all three jobs green.** The commit that follows
this one is **documentation only** — it records these results and changes no code, no test
and no configuration.

The race-gate package timings:

| Package | Time under `-race` |
|---|---|
| `cmd/mpm` | 403.1s |
| `mpm-core` (`internal/core`) | 243.0s |
| `internal/core/tools` | 40.9s |
| `internal/scheduler` | 22.7s |
| `cmd/mpm-mcp` | 8.2s |
| `internal/critic` | 4.1s |
| `internal/testenv` | 8.0s |

**Note for the reviewer.** The D3 branch's race gate is green *because* of the two P2
amendments. The first CI run on the branch failed on all three jobs — and the failure was
the unamended `TestP2MCP_LegacyCitationIsUnchangedOverMCP`, which asserted the old
`"memory"` value. That is the compatibility conflict of §5.2 showing up in CI rather than
in a report: the two suites cannot both hold, and the amendment is what makes the branch
buildable.
