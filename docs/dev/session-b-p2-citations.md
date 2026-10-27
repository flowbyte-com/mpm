# Session B — P2: typed, validated, idempotent evidence citations on `mpm_theories propose`

> **Author:** Session B (intelligence core). **Branch:** `feat/session-b-p2-citations`.
> **Base:** `origin/main` = `57bf9327d57b8b6598abfee758c615aa9ac9e1bc`.
> **Authorised by:** `docs/dev/session-a-adjudication.md` §2 (A-4) and §4.2 (P2 row).
>
> **No schema change. No deployment. No merge.** The registry change is the one
> shared-interface edit, and it is exactly the one A approved (A-4) and
> described (A-9 is included in the same PR, as the ledger's P2 row directs).
>
> **Scope discipline:** D1, D2, D4 and D5 remain separately tracked defects and
> are **not** touched here. The critic is **not** rewritten.

---

## 1. What P2 does

`mpm_theories propose` gains **one optional parameter**, `source_type`, beside
the existing `source_ids`. When it is supplied, every cited id is **typed** and
**validated**; a citation that does not resolve as its declared type
**rejects the whole proposal** and leaves **no partial proposal record**.

It reuses `epistemic_provenance`, which already provides all three required
properties — **no schema change**:

| Property | Where it comes from | Verified |
|---|---|---|
| typed | `epistemic_provenance.source_type TEXT NOT NULL` (`internal/core/schema.go:442`) | A's own verification, `session-a-adjudication.md` §0 |
| identifiable | `epistemic_provenance.event_id TEXT NOT NULL` | A's own verification |
| idempotent | `UNIQUE (source_id, downstream_id, event_id)` + `ON CONFLICT … DO NOTHING` (`internal/core/cascade_provenance.go:181-187`) | §4 below, by measurement |

---

## 2. The design

### 2.1 Two paths, one write path

A citation carries an optional type. The type decides which path a citation
takes.

| | **LEGACY** — `Type == ""` | **TYPED** — `Type != ""` |
|---|---|---|
| Who uses it | every existing caller | a caller that supplies `source_type` |
| The cited id is validated? | **no** (historical behaviour) | **yes** — it must resolve *as that type* |
| The recorded `source_type` | auto-resolved; falls back to `"memory"` for a non-artifact id | the **declared** type, verbatim |
| The recorded `event_id` | the **downstream** id (the proposal) | the **cited** id |
| A bad citation | accepted (historical) | **rejects the proposal** |

The legacy path is **byte-for-byte unchanged**. That is the whole point of
"preserve backward compatibility for callers not supplying the new optional
citation fields": the cascade materializer, `mpm_self_heal`, the CLI and every
existing tool call keep working exactly as before.

Both paths go through **one** function — `recordCitationsNode` — so the two
cannot drift apart. `recordSourceCitationsNode` (the historical helper, still
used by `RecordDecision`) is now a thin wrapper over it.

### 2.2 Why the typed path writes the cited id as the event id

The legacy path overloads `event_id` with the downstream id, which means the
cited observation is **not identifiable** from the citation row — one of the
findings in `docs/dev/session-b-audit.md` §3.3.

The typed path writes the **cited id** as the event id. Two consequences, both
desirable:

1. The citation row is **self-describing**: `source_id` and `event_id` both
   name the observation the proposal rests on, and `source_type` says what it
   is.
2. The unique key `(source_id, downstream_id, event_id)` gains
   **per-citation granularity**, so re-submitting the same proposal with the
   same citations is a no-op rather than a second row — and two *different*
   citations are two different rows rather than one.

Both are pinned by tests (§4).

### 2.3 The source-type vocabulary

Closed, and deliberately small:

| Type | Resolves against |
|---|---|
| `memory` | `memories` where `collection IN ('', 'memories')` and not soft-deleted |
| `theory` | `memories` where `collection = 'theories'` and not soft-deleted |
| `decision` | `memories` where `collection = 'decisions'` and not soft-deleted |
| `lesson` | `lessons_base`, not soft-deleted |
| `work` | `works` |
| `tool_invocation` | `tool_invocations` |

The five artifact types mirror the `evidence` table's CHECK-constrained
`artifact_type` vocabulary. `tool_invocation` is added for a citation that
points at the substrate's tool-call audit — the citation kind
`mpm-event/0.1` §3.7 explicitly contemplates (*"each of them must resolve to
a stored event (or to a `tool_invocations` row)"*).

**A citation must point at something that is still there**: a soft-deleted
artifact does not resolve. `tool_invocations` has no `deleted_at` — the audit
table is append-only and a shredded row is removed rather than tombstoned.

### 2.4 Atomicity

The proposal, its dependency-column patch and its citations are written in
**one transaction** (`dm.WithTx`). The resolution check runs on the same
`DBNode`, so it shares the transaction's snapshot: a citation that cannot be
resolved cannot be written, and the rejection rolls the proposal back with it.

The consequence, pinned by a test: a batch whose **first** citation is valid
and whose **second** is not leaves **nothing** behind — not the theory row,
not the dependency patch, and **not the valid citation either**. A partial
write would mean a proposal that exists without the evidence it claims to
rest on, which is exactly the state M5 forbids.

---

## 3. The shared-interface change (A-4)

**One optional parameter, on one action:**

```jsonc
"source_type": {"type": "string",
                "enum": ["memory","theory","decision","lesson","work","tool_invocation"],
                "description": "Optional. Declares the type of EVERY id in source_ids, so each citation is typed and validated: a cited id that does not resolve as that type rejects the whole proposal and leaves no partial record. Omit it and the citations are recorded in the historical untyped way (type auto-resolved, not validated)."}
```

The registry schema **is** the MCP server's published input schema
(`cmd/mpm-mcp/tools.go:412` → `mcp.NewToolWithRawSchema(tool.Name,
tool.Description, tool.Schema)`), so declaring the parameter here is what
makes the MCP path accept it. A test reads the registry schema and asserts
the enum is **exactly** the canonical vocabulary — no drift in either
direction.

### 3.1 The one limitation, stated rather than hidden

`source_type` declares the type of **every** id in the call. A proposal that
needs to cite **two different kinds** of source in one call cannot express it
with this parameter.

I implemented A's approved shape rather than a per-citation `citations`
array, because A approved and described this one, and because it is the
smallest change that satisfies the requirement. If mixed-type citations turn
out to be needed, the natural extension is a per-citation array on the same
core primitive — **not** a new mechanism — and it would be a second
one-parameter registry addition, which A should approve at that point rather
than now.

For the P3 use case (a hunt citing a session's tool invocations) one type is
sufficient.

### 3.2 A-9 — the registry correction, in the same PR

`mpm_evidence`'s `artifact_type` enum advertised `"skill"`, which the
`evidence` table's CHECK constraint rejects:

```sql
CHECK (artifact_type IN ('memory','theory','decision','lesson','work'))
```

A caller following the registry got a CHECK violation. The enum is now
exactly the database's vocabulary, and a test reads the CHECK constraint back
**out of the live schema** and asserts the registry does not exceed it — so
the two cannot drift again.

---

## 4. Tests

### 4.1 Core — `internal/core/p2_citation_test.go` (11 tests, always in the gate)

| Test | Covers |
|---|---|
| `TypedCitationRecordsDeclaredTypeAndCitedEventID` | **correct types** — the declared type is recorded verbatim, not the `"memory"` fallback; the event id is the cited id |
| `TypedCitationOfEachArtifactTypeResolves` | **correct types** — each vocabulary entry resolves against the right table |
| `UnresolvableSourceIsRejected` | **invalid sources** — rejected, and **no theory row and no citation row** survive |
| `WrongDeclaredTypeIsRejected` | **correct types** — a memory id declared as a tool invocation / lesson / decision / work is rejected (4 sub-cases) |
| `UnknownSourceTypeIsRejected` | **invalid sources** — the vocabulary is closed; `lifecycle_event` is rejected by name |
| `SoftDeletedArtifactIsRejected` | **correct types** — a citation must point at something still there |
| `AtomicFailureLeavesNoPartialRecord` | **atomic failure** — a valid citation followed by an invalid one leaves **nothing**, including the valid citation |
| `RepeatedSubmissionIsIdempotent` | **repeated submissions** — the same proposal with the same citation twice ⇒ one citation row |
| `RepeatedSubmissionOfDifferentCitationsAccumulates` | **repeated submissions** — two *different* cited ids are two different rows, so a proposal resting on two observations loses neither |
| `LegacyPathIsUnchanged` | **backward compatibility** — an unresolvable id is still accepted, the type is still auto-resolved to `"memory"`, the event id is still the downstream id |
| `LegacyAndTypedPathsAreDistinct` | **backward compatibility** — the two paths' event ids differ, so a migration from one to the other cannot silently collide |

### 4.2 Tool surface — `internal/core/tools/p2_citation_tool_test.go` (6 tests)

| Test | Covers |
|---|---|
| `RegistryDeclaresSourceType` | the registry declares the parameter, typed `string`, with the canonical enum |
| `HandleProposeWithSourceTypeIsTyped` | the handler routes to the **typed** path when the parameter is present |
| `HandleProposeWithoutSourceTypeIsLegacy` | the handler routes to the **legacy** path when it is absent |
| `HandleProposeRejectsUnresolvableTypedCitation` | rejection at the tool boundary, with no proposal surviving |
| `HandleProposeRejectsUnknownSourceType` | an unknown type is rejected, not ignored |
| `EvidenceArtifactTypeEnumMatchesDatabase` | the A-9 correction, checked against the live CHECK constraint |

### 4.3 The D3 split

P2 fixes the **typed** path. The **legacy** path is deliberately unchanged, so
the defect that the M5 contract suite pins does not go away:

| | Status after P2 |
|---|---|
| **D3 — typed citations** | **fixed.** A typed citation that does not resolve rejects the proposal; the declared type is preserved. |
| **D3-b — legacy `source_ids` path** | **still open, separately tracked.** An unresolvable id on the legacy path is still accepted and still mislabelled `"memory"`. Tightening it changes behaviour for existing callers (the cascade materializer, `mpm_self_heal`), so it needs **its own ruling**. The M5 contract suite's `TestM5Contract_UnresolvableCitationIsRejected` and `TestM5Contract_CitationPreservesItsType` continue to pin it — they are in the `m5contract` suite on PR #117 and neither is touched by this PR. |

D1, D2, D4 and D5 are **untouched** and remain separately tracked.

---

## 5. Verification

| Gate | Result |
|---|---|
| `internal/core` full suite | **ok** (243.7 s) |
| `internal/core/tools` full suite | **ok** (35.9 s) |
| Root module `go test ./...` (all packages) | **ok** — `cmd/mpm` 420.9 s, `cmd/mpm-mcp`, `cmd/mpm-critic`, `cmd/mpm-scheduler`, `cmd/mpm-telemetry`, `internal/critic`, `internal/scheduler`, `internal/telemetry`, `internal/testenv`, … |
| CI "Core tests" step regex (`-short -tags fts5`) | **ok** |
| `go vet -tags fts5` (root, core, tools) | **clean** |
| `mpm-lint --gate` (the fatal-class gate) | **PASS** — mutex, sql, fd and imports thresholds all ok |
| `make test-scripts` | **ok** — 540 tests |
| `make test-agent-installation` | **ok** — 494 tests + 46 TS adapter tests |
| P2 core tests | **11/11 ok** |
| P2 tool-surface tests | **6/6 ok** |
| `gofmt` on every file this PR touches | **clean** (the two pre-existing `gofmt` complaints in `epistemology_tools.go` are at `origin/main` and are **not** reformattted here — reformatting them would bury the diff) |
| Live database knowledge tables | **byte-identical** to the runbook baseline (27/116/85/2/61/5) |

Every P2 test pins an isolated `HOME`, `MPM_WORKSPACE` and `MPM_DB_PATH` and
opens a **disposable file-backed database at the canonical
`<workspace>/src/db/mpm.db`**, so the production path resolution is exercised
rather than bypassed. The repo-wide DB-safety AST guard flags only an **empty
string literal** as the first argument to `NewDatabaseManager`, so these
helpers need **no whitelist entry** and the guard's own suite passes unchanged.

---

## 6. A harness constraint worth knowing — found the hard way

`scripts/tests/test_build_install_separation.py::_copy_tracked_tree` stages the
repository with **`git ls-files`**, so it copies only **tracked** content.
A **new, untracked** source file is therefore invisible to that test's build,
while the **modified tracked** files that reference it are copied — producing
a build failure that looks like a code error:

```
internal/core/epistemology_tools.go:144:129: undefined: Citation
```

It is not a code error. `go build` in the working tree succeeds; only the
git-staged copy fails. **A new source file must be `git add`-ed before
`make test-scripts` can pass.** Recorded so the next session does not lose
time to it.

---

## 7. What this PR does not do

* **No schema change.** No `SafeMigrations` entry. `epistemic_provenance`
  already had every property required.
* **No change to the legacy `source_ids` path** — that is D3-b and needs its
  own ruling.
* **No change to the critic.** D2 stays a tracked defect; A's A-1 ruling
  (record it as known and out of scope) stands.
* **No change to D1, D4 or D5.** They remain separately tracked.
* **No lifecycle-event store.** That is A's A2 decision, still deferred.
* **No deployment, no merge.** Only Session A merges to `main` and deploys.
* **No edit to `~/src/mpm` or `~/src/mpm-opencode`.**

---

## 8. For Session A

| # | Item | What B needs |
|---|---|---|
| **A-4 (approved)** | the registry change | **Action:** merge PR-1 after review. One optional parameter on `mpm_theories` propose, plus the A-9 `artifact_type='skill'` correction in the same registry surface, as the ledger's P2 row directs. |
| **A-3 (approved)** | no new tool, no schema | **Action:** confirm the implementation matches — no `mpm_provenance` resurrection, no table, no new tool. It does. |
| **A-new-1** | the `source_type` limitation | **Decision:** accept the single-type limitation for this sprint (one type per call), or ask for a per-citation `citations` array now. **B's recommendation:** accept it — the P3 use case needs one type, and the core primitive already supports a mixed list, so a later extension is one parameter, not a new mechanism. |
| **A-new-2** | **D3-b** — the legacy `source_ids` path | **Decision:** whether the legacy path should also validate and reject. It changes behaviour for the cascade materializer and `mpm_self_heal`, so it needs its own ruling. **B's recommendation:** leave it unchanged this sprint and record it as a known gap — tightening a legacy path in the same PR that adds the typed one doubles the blast radius of both. The M5 contract suite already pins it, so it cannot be forgotten. |
| **A-new-3** | the `git ls-files` staging constraint | **Awareness:** a new source file must be `git add`-ed before `make test-scripts` passes. Not a defect — but it will bite the next session that adds a file. |
