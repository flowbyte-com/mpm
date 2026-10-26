# Session B — M5 trust-defect regression coverage

> **Author:** Session B (intelligence core). **Branch:** `dev/session-b-core`.
> **Base:** `70deac2bb3fce3c63da28bfbd9ca82ca2c4d1e2e`.
> **Scope:** deterministic regression coverage for the five trust defects
> demonstrated in [`session-b-audit.md`](session-b-audit.md) §3.
>
> **No production code was changed.** No trust or promotion logic, no tool
> registry, no database schema, no shared interface. This deliverable is
> test fixtures and this report.
>
> **Addressed to:** Session A. **Copy:** Session C.

---

## 1. What was added

| File | Build tag | Runs in the green gate? | Purpose |
|---|---|---|---|
| `internal/core/m5_trust_helpers_test.go` | none | **yes** | Shared isolation fixtures. |
| `internal/core/m5_trust_characterization_test.go` | none | **yes** | Pins the CURRENT behaviour of defects D1–D5. **Green.** |
| `internal/core/m5_trust_contract_test.go` | `m5contract` | **no** | Asserts the M5-COMPLIANT behaviour. **Red today — by design.** |
| `internal/core/m5_trust_gate_test.go` | none | **yes** | The mechanical guarantee that the contract suite can never enter a green gate. **Green.** |
| `internal/critic/m5_trust_characterization_test.go` | none | **yes** | Pins the critic's contribution to D2 and D4. **Green.** |
| `internal/critic/m5_trust_contract_test.go` | `m5contract` | **no** | Asserts the critic's M5-compliant behaviour. **Red today — by design.** |

**Six green tests, six intentionally-failing reproductions, one green gate guard.**

---

## 2. The two-file discipline, and why there are two files

A defect that is real and unfixed has two truths: *what the substrate does
today* and *what M5 requires it to do*. Both need to be pinned, and they must
not live in the same file — a test that asserts defective behaviour must not
be able to fail a required green check, and a test that asserts correct
behaviour must not be able to pass silently.

| | Characterization file | Contract file |
|---|---|---|
| Asserts | the CURRENT behaviour, defect included | the M5-COMPLIANT behaviour |
| Today | **passes** | **fails** |
| Build tag | none | `m5contract` |
| In a green gate? | **yes, always** | **no, ever** |
| Retired by | **inverting** it in the fix's PR | **deleting** it in the fix's PR |

Both files are named for what they are, and each test's doc comment names the
defect, the responsible production code, the minimal correction and whether
Session A must approve it.

---

## 3. The gate: why the red suite can never enter a green CI gate

The `m5contract` tag excludes the contract files from every default build:
`make test`, `make test-race`, `make test-core-precommit`, the CI "Core
tests" step and the CI full-suite sweep all invoke `go test -tags fts5` with
no additional tags.

**That is a claim that can rot.** A CI edit, a Makefile edit or a stray
`-tags` addition could absorb a failing contract test into a required check
and turn the sprint's red signal green. So the claim is *checked*, by
`internal/core/m5_trust_gate_test.go`, which is always compiled and always
green:

1. Every file declaring a `TestM5Contract*` function carries the `m5contract`
   build constraint, evaluated with `build.Context.MatchFile` — the
   toolchain's own evaluator, so the guard cannot disagree with the compiler.
2. The contract files are **absent from the default build** and **present under
   the tag**, again via `go/build`.
3. No `TestM5Characterization*` file carries a build constraint, so the green
   side cannot be quietly excluded either.
4. Neither the Makefile nor any CI workflow mentions `m5contract`.

### 3.1 Non-vacuity — proven, not asserted

The gate was verified against three deliberate violations in a disposable copy
of the tree (`/tmp/opencode/m5-gate-nonvacuity`, since removed). Each one made
the gate **fail**:

| Violation introduced | Gate result |
|---|---|
| `//go:build m5contract` removed from `internal/core/m5_trust_contract_test.go` | **FAIL** — *"declares TestM5Contract* functions but is compiled by the DEFAULT build — a contract test would be running in a required green gate"* |
| `m5contract` added to the `Makefile` | **FAIL** — *"Makefile mentions the \"m5contract\" build tag — the M5 contract suite would be inside a required green gate"* |
| `//go:build m5contract` added to `internal/core/m5_trust_characterization_test.go` | **FAIL** — *"declares TestM5Characterization* functions but is EXCLUDED from the default build — the green side of the suite must always run"* |

The gate is a real gate, not a comment.

### 3.2 What the gate deliberately does not assert

It does **not** require at least one contract test file to exist. The
retirement procedure deletes a contract test when its defect is fixed or
accepted, so a non-empty population requirement would break retirement. The
invariant that must hold both before and after retirement is: *every contract
test is tagged; no characterization test is tagged*.

---

## 4. Per-defect findings

### D1 — Duplicate submissions of the same observation inflate confidence

**Reproduction (today, green).**
`TestM5Characterization_DuplicateEvidenceInflatesConfidence` — two
byte-identical `mpm_evidence` submissions against one pending theory.

```
evidence rows:                       2
confidence:       0.500000  →  0.622459  →  0.731059
confidence_history: (0.622459, evidence_count=1, evidence_added)
                   (0.731059, evidence_count=2, evidence_added)
```

**Expected correct behaviour.** Submitting the same observation twice leaves
the artifact's confidence where one submission left it, and leaves exactly one
evidence row. A retried delivery, a duplicated ingestion and a replayed
collector must not read as corroboration.

**Existing implementation responsible.**
`internal/core/evidence_store.go:292` mints a fresh id per insert. The
`evidence` table has no unique index beyond its primary key and no
content-hash column (`internal/core/schema.go:267-287`). The confidence
recompute at `internal/core/evidence_store.go:511-554` sums every non-expired,
non-`auto_capture` row, so each duplicate counts in full.

**Minimal proposed correction.** Give the evidence row an observation
identity and refuse a second row carrying the same identity for the same
artifact. Whether that is a new column, a stored hash, or a unique index over
existing columns is **Session A's** decision; the contract test is written
against the behaviour, not the mechanism, so A's choice does not invalidate
it.

**Regression tests.**
`TestM5Characterization_DuplicateEvidenceInflatesConfidence` (green) and
`TestM5Contract_DuplicateEvidenceDoesNotInflateConfidence` (red).

**Needs Session A's approval: YES** — the mechanism is a schema change in
`internal/core/schema.go`, the sprint's highest-collision file.

### D2 — The critic creates trusted lessons without explicit review

**Reproduction (today, green).**
`TestM5Characterization_CriticEmitsTrustedLessonWithoutReview` — a
`WeakTheoryHunt` over a pending theory emits a finding whose target is
`mpm_lessons` / `save`. The finding's payload carries `fact`, `type` and
`tags` and **nothing else** — no evidence, no citation, no review step.
Applying it writes a lesson that is immediately retrievable through
`lessons_fts` with a full confidence value.

The core half is pinned separately by
`TestM5Characterization_LessonSurfaceHasNoPendingState`: `PRAGMA
table_info(lessons_base)` lists `rowid, id, type, content, tags,
reinforcement_count, source_session_id, created, content_hash,
retrieval_priority, importance, confidence` — **no status, state, review_status
or pending column of any kind**.

**Expected correct behaviour.** A critic-authored finding must not be
retrievable as trusted knowledge until it has been reviewed. It must be
pending, or absent.

**Existing implementation responsible.**
`internal/critic/hunts.go:297-309` (`WeakTheoryHunt`) and `:106-118`
(`SurvivalAsymmetryHunt`) emit `Tool: "mpm_lessons", Action: "save"`. The
lesson surface is `internal/core/db.go:5366` (`AddLesson`) over a
`lessons_base` table with no status column.

**Minimal proposed correction.** Session A's adjudication **A-1** decides
which of two readings governs:

* (a) the critic may only write to a **pending** surface — which needs a
  status column on `lessons_base` (a schema change); or
* (b) the behaviour is recorded as a **known pre-existing exception** and
  `mpm-event/0.1` M5 is amended to say so explicitly.

**Regression tests.**
`TestM5Characterization_CriticEmitsTrustedLessonWithoutReview` and
`TestM5Characterization_LessonSurfaceHasNoPendingState` (green);
`TestM5Contract_CriticFindingIsNotTrustedWithoutReview` (red).

**Needs Session A's approval: YES** — this is the M5 tension already
referred to A in the audit (§6, A-1).

### D3 — Proposals containing incorrect or unresolvable evidence citations

**Reproduction (today, green).**
`TestM5Characterization_UnresolvableCitationIsAcceptedAndMislabelled` — a
proposal whose `source_ids` contains an id that resolves to **nothing** is
accepted with `"success":true`, and the citation is stored as:

```
source_id       = no-such-artifact-0000000000000000
source_type     = memory          <-- MISLABELLED
downstream_type = theory
event_id        = <the proposal's own id>
```

The mislabelling is deterministic: `ResolveArtifactType`
(`internal/core/cascade_provenance.go:368-416`) fails on a non-artifact id and
`recordProvenanceNode` falls back to `"memory"`
(`internal/core/cascade_provenance.go:168-178`). **MPM cannot currently
distinguish a citation of a memory from a citation of a tool invocation.**

**Expected correct behaviour.** A proposal whose cited source id does not
resolve is **rejected**, leaving no partial state. And a citation preserves
the type of what it cites.

**Existing implementation responsible.**
`internal/core/epistemology_tools.go:676-698` (`recordSourceCitationsNode`)
passes `source_type=""` and `event_id` hardcoded to the downstream id, and
never validates that the cited id exists. The whole proposal is already one
`WithTx`, so a validation failure rolls the artifact insert back for free.

**Minimal proposed correction.** Validate each cited source id against its
declared source type before the insert, and let the caller declare that type
so it is written through verbatim. `epistemic_provenance.source_type` already
exists and is free-text, so **no schema change is needed** — only the
tool-registry parameter and the write path. This is the same change the audit
recommended as PR-1.

**Regression tests.**
`TestM5Characterization_UnresolvableCitationIsAcceptedAndMislabelled`
(green); `TestM5Contract_UnresolvableCitationIsRejected` and
`TestM5Contract_CitationPreservesItsType` (red).

**Needs Session A's approval: YES** — the tool-registry parameter on
`mpm_theories propose` (`internal/core/tools/registry_list.go`) is A's shared
interface.

### D4 — Repeated critic cycles generate duplicate theories because of cycle-specific tags

**Reproduction (today, green).**
`TestM5Characterization_CycleTagBreaksProposalIdempotency` — the *same*
hypothesis proposed twice with identical tags is idempotent (one row, one id);
the same hypothesis with `cycle_5` and `cycle_6` produces **two distinct
proposals**.

The critic half is pinned by
`TestM5Characterization_CycleMarkerDefeatsProposalIdentity`: two
`PoisonPillHunt` runs at cycles 5 and 10 emit findings whose tags differ
**only** in the cycle marker, so the marker alone defeats the proposal
identity.

**Expected correct behaviour.** A periodic reviewer re-finding the same thing
must not accumulate a new pending proposal every cycle. The cycle marker must
not be part of the proposal's identity.

**Existing implementation responsible.**
`internal/core/db.go:6269-6287` (`MemoryIdentityHash`) folds `json(tags)`
into the identity; the partial unique index `idx_memories_identity_live`
(`internal/core/schema.go:992`) then treats a cycle-varying tag as a
genuinely different artifact. Emitters: `internal/critic/hunts.go:373-378`
(`PoisonPillHunt`), `:115` and `:306` (the two lesson hunts).

**Minimal proposed correction.** Carry the cycle marker **outside** the
proposal identity — in the `validation_criteria` text or the proposal's
metadata rather than in its tags. **No schema change and no shared-interface
change**: it is a change to how the critic emits findings, which is Session
B's own area once A has ruled on the trust policy.

**Regression tests.**
`TestM5Characterization_CycleTagBreaksProposalIdempotency` and
`TestM5Characterization_CycleMarkerDefeatsProposalIdentity` (green);
`TestM5Contract_CycleVaryingTagDoesNotCreateSecondProposal` (red).

**Needs Session A's approval: PARTIAL.** The emission-side fix is Session B's.
If A instead mandates a change to the identity hash, that touches shared
dedup semantics in `internal/core/db.go` and needs A's arbitration.

### D5 — Distinguishing independent corroborating evidence from repeated delivery

**Reproduction (today, green).**
`TestM5Characterization_RepeatedDeliveryCountsAsIndependentCorroboration` —
two submissions describing the **same** observation and two submissions
describing **different** observations produce the **same** confidence
(`0.731058563` in both cases). The substrate has no identity for the
underlying observation, so repeated delivery is indistinguishable from
independent corroboration.

The characterisation also shows that the *mechanism for expressing*
dependence does exist and works when used — a single row at
`independence_factor = 0.5` scores below one full-strength row — but it is
purely caller-asserted, and nothing enforces that a caller uses it.

**Expected correct behaviour.** The same observation delivered again must
contribute what one observation contributes, while two distinct observations
must continue to contribute twice. **The distinction must be preserved, not
merely asserted.**

**Existing implementation responsible.**
`internal/core/confidence.go:119-138` sums `strength × independence` over
every scoring evidence row; `internal/core/evidence_store.go:511-554` loads
them. `independence_factor` (`internal/core/evidence_store.go:107`) is the
intended dependence signal but is unenforced, and there is no observation
identity to enforce it against.

**Minimal proposed correction.** The same observation identity that closes D1
also closes D5 — once the substrate can tell two submissions of one
observation from two submissions of two observations, the dependence becomes
representable and enforceable. The mechanism is A's choice; the contract test
pins the behaviour.

**Regression tests.**
`TestM5Characterization_RepeatedDeliveryCountsAsIndependentCorroboration`
(green); `TestM5Contract_RepeatedDeliveryDoesNotCountAsIndependentCorroboration`
(red).

**Needs Session A's approval: YES** — the mechanism is a schema change.

---

## 5. Isolation

Every test in the suite pins **`HOME`**, **`MPM_WORKSPACE`** and
**`MPM_DB_PATH`** to disposable locations and opens a **disposable file-backed
database at the canonical `<workspace>/src/db/mpm.db` path** — the same path
`MPM_DB_PATH` names — so the production path resolution is exercised rather
than bypassed.

```go
func m5IsolatedWorkspace(t *testing.T) string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	ws := t.TempDir()
	t.Setenv("MPM_WORKSPACE", ws)
	t.Setenv("MPM_DB_PATH", filepath.Join(ws, "src", "db", "mpm.db"))
	return ws
}
```

* `HOME` is pinned because `config.GetMPMDir()` falls back to `$HOME/.mpm`
  when `MPM_WORKSPACE` is unset, and because workspace-scoped files
  (`active.json`, `toxicphrases.txt`) are created under the resolved root.
* `MPM_DB_PATH` is pinned because `mpm-critic` and `mpm-scheduler` honour it
  while `mpm` and `mpm-mcp` ignore it — an unpinned value could silently
  redirect a later subprocess at the live database.
* `NewDatabaseManager` is always called with a **non-empty** disposable root.
  The repo-wide DB-safety AST guard
  (`internal/core/test_db_safety_test.go`) flags only an **empty string
  literal** as the first argument — the case that falls through to
  `$HOME/.mpm` — so these helpers are correctly not flagged and **no
  whitelist entry is needed**. The guard's own suite passes unchanged
  (`TestDBSafety_*`: 6/6 green).

---

## 6. Running the suites

**Green suite** (always part of the gate):

```bash
cd internal/core && CGO_CFLAGS=-DSQLITE_ENABLE_FTS5=1 CGO_LDFLAGS=-lm \
  go test -tags fts5 -count=1 -run 'TestM5' -v ./
cd internal/critic && CGO_CFLAGS=-DSQLITE_ENABLE_FTS5=1 CGO_LDFLAGS=-lm \
  go test -tags fts5 -count=1 -run 'TestM5' -v ./...
```

**Contract suite** (opt-in only — expected to FAIL):

```bash
cd internal/core && CGO_CFLAGS=-DSQLITE_ENABLE_FTS5=1 CGO_LDFLAGS=-lm \
  go test -tags 'fts5 m5contract' -count=1 -run 'TestM5Contract' -v ./
cd internal/critic && CGO_CFLAGS=-DSQLITE_ENABLE_FTS5=1 CGO_LDFLAGS=-lm \
  go test -tags 'fts5 m5contract' -count=1 -run 'TestM5Contract' -v ./...
```

Current contract-suite output (all six failures are the defects, made
visible):

```
--- FAIL: TestM5Contract_DuplicateEvidenceDoesNotInflateConfidence
    expected exactly 1 evidence row after two identical submissions (defect D1), got 2
    expected the duplicate submission not to change confidence (defect D1): 0.622459316 -> 0.731058564
--- FAIL: TestM5Contract_UnresolvableCitationIsRejected
    expected the proposal to be REJECTED because its cited source id "no-such-artifact-…" does not resolve (defect D3)
    expected a rejected proposal to leave NO theory row (defect D3), got 1
--- FAIL: TestM5Contract_CitationPreservesItsType
    expected the citation to preserve source_type 'tool_invocation' (defect D3), got "memory"
--- FAIL: TestM5Contract_CycleVaryingTagDoesNotCreateSecondProposal
    expected the cycle-varying tag NOT to create a second proposal (defect D4): got ids "6f1807d7…" and "f8d14d2b…"
    expected exactly 1 theory row after the cycle-varying re-proposal (defect D4), got 2
--- FAIL: TestM5Contract_RepeatedDeliveryDoesNotCountAsIndependentCorroboration
    expected two DISTINCT observations to score higher than one observation delivered twice (defect D5): distinct=0.731058550 repeated=0.731058550
--- FAIL: TestM5Contract_CriticFindingIsNotTrustedWithoutReview
    expected a critic-authored lesson to NOT be retrievable as trusted knowledge without review (defect D2), but lessons_fts returned 1 hit(s) — the critic wrote into the trusted surface with no review step
```

---

## 7. Retirement procedure — how these tests end

When Session A adjudicates a defect and the correction lands, **in the same
PR**:

1. **Invert** the matching characterization test — it must now assert the
   CORRECT behaviour and still pass. This is what makes the fix a visible
   change rather than a silent one.
2. **Delete** the matching contract test — its assertion now holds.
   (Where A accepts a defect as-is rather than fixing it, the contract test is
   deleted and the exception is recorded in the contract; the
   characterization test is inverted to assert the accepted behaviour and
   labelled as such.)
3. Run both suites. The green suite must stay green, and the contract suite
   must have one fewer failing test.

The **gate guard is not retired** — it keeps the tag discipline enforced for
whatever contract tests remain.

---

## 8. Decisions required from Session A

| # | Decision | Why it is A's | B's recommendation |
|---|---|---|---|
| **A-1** (re-confirmed) | **The M5 trust tension.** Does the critic's existing automatic lesson-writing count as the automatic promotion M5 forbids, or is it a pre-existing exception the sprint records as out of scope? | M5 is a shared-interface item. | Record it as an explicit exception **and** amend M5 to say so — unless A is willing to spend a schema change on a pending state for critic-authored lessons, which the sprint's four-day runway does not favour. Either way it must be **written down**, not left implicit. |
| **A-2** | **D1 / D5 mechanism.** How is an evidence row's observation identity expressed — a new column, a stored hash, or a unique index over existing columns? | `internal/core/schema.go` is A's migration authority. | A unique index over existing columns is cheapest and needs no new column; a hash column is more explicit. **The contract test pins the behaviour, not the mechanism**, so A's choice does not invalidate it. |
| **A-3** | **D3 registry change.** Approve the optional `source_type` parameter on `mpm_theories propose` and the core validation that rejects an unresolvable citation. | `internal/core/tools/registry_list.go` is A's shared interface. | Approve as ONE PR with its core change and its contract tests, per `coordination.md` §4.3. **No schema change is needed** — `epistemic_provenance.source_type` already exists and is free-text. |
| **A-4** | **D4 mechanism.** Is the emission-side fix (carry the cycle marker outside the proposal identity) acceptable, or must A mandate a change to the shared identity hash? | The identity hash is shared dedup semantics in `internal/core/db.go`. | The emission-side fix. It is Session B's own area, needs no schema change, and no shared-interface change. |
| **A-5** | **Gate acknowledgement.** Confirm that the `m5contract` tag must never be enabled in a green gate, so that A's own future CI edits keep this property. | A owns CI and the Makefile. | Confirm. The gate test enforces it mechanically, so the property survives CI edits without depending on A remembering. |

**Nothing in this deliverable needs A's approval to land as-is** — it is test
fixtures and documentation, and every green test passes.

---

## 9. Regression results

| Suite | Command | Result |
|---|---|---|
| Green (core) | `go test -tags fts5 -count=1 -run 'TestM5' ./internal/core/` | **ok** — 6/6 pass |
| Green (critic) | `go test -tags fts5 -count=1 -run 'TestM5' ./internal/critic/...` | **ok** — 2/2 pass |
| Contract (core) | `go test -tags 'fts5 m5contract' -run 'TestM5Contract' ./internal/core/` | **FAIL — 5 expected failures** (D1, D3×2, D4, D5) |
| Contract (critic) | `go test -tags 'fts5 m5contract' -run 'TestM5Contract' ./internal/critic/...` | **FAIL — 1 expected failure** (D2) |
| Full `internal/core` suite | `go test -tags fts5 -count=1 ./internal/core/` | **ok** (230.8 s) |
| Full `internal/critic` suite | `go test -tags fts5 -count=1 ./internal/critic/...` | **ok** (0.5 s) |
| CI "Core tests" step regex | `go test -short -count=1 -tags fts5 -run "TestSynthesis\|…\|TestDBSafety" ./...` | **ok** |
| DB-safety guard | `go test -short -count=1 -tags fts5 -run TestDBSafety -v ./` | **ok** — 6/6 pass, no whitelist entry needed |
| `go vet` under the tag | `go vet -tags 'fts5 m5contract' ./` (core and root) | **clean** |

---

## 10. Incidental finding recorded on the way

`confidence_history` has **no monotonic ordering column**: `id` is a generated
UUID and `computed_at` is a whole-second INTEGER
(`internal/core/schema.go:296-304`), so "which confidence change came first"
is not reconstructible from the ledger. The D1 characterization test has to
order by `rowid` to see the duplication in sequence. This matters for D1 and
D5, because the ledger is where an auditor would look to see *why* a
proposal's confidence rose — and it cannot answer that on its own. Recorded
for A's consideration alongside A-2; **not** asserted as a defect here,
because it is a forensics nicety rather than a trust violation.

---

## 11. What was NOT done

* **No production code was changed** — no trust logic, no promotion logic, no
  tool registry, no database schema, no shared interface.
* **No `SafeMigrations` entry.**
* **No merge and no deployment.** This is committed to `dev/session-b-core`
  only; only Session A merges to `main` and deploys.
* **No edit to `~/src/mpm` or `~/src/mpm-opencode`.**
* **No write to the live database.** The live knowledge tables are
  byte-identical to the runbook baseline; every test ran against a disposable
  workspace with its own `HOME`, `MPM_WORKSPACE` and `MPM_DB_PATH`.
* **No contract test was added to a green gate**, and the gate guard proves
  it cannot be.
* **The existing audit commit (`93011a0`) is preserved**, unchanged.
