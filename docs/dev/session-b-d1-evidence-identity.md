# Session B — D1/D5: the evidence observation identity

**Branch:** `feat/session-b-d1-evidence-identity` (from `origin/main` = `444ec86`)
**Status:** D1 **CLOSED**, D5 **CLOSED**, both by a correction that needs no schema
change, no migration and no tool-registry change.
**Scope:** the evidence→confidence path only. D3, D4, D6, D7, the telemetry hunt,
the OpenCode lifecycle ingestion, the legacy adapter and the P2b scheduler gate
are untouched; interactions are reported in §12.

---

## 1. Summary

An evidence row records an **observation** about an artifact. A **delivery** of
that observation is a separate act. The substrate could not tell the two apart:
every delivery minted a fresh row id, the evidence table carried no uniqueness
guarantee beyond its primary key, and the confidence recompute summed every
non-expired row in full. Two deliveries of one observation therefore raised the
artifact's confidence twice — repetition mistaken for corroboration, which is
the exact inverse of M5's trust boundary.

The correction gives every evidence row a durable **observation identity** and
lets the substrate enforce it at the storage boundary:

* the row's **id is derived** from the observation's identity instead of minted
  at random, and
* the evidence table's **PRIMARY KEY** is the uniqueness guarantee — "at most
  one stored row per observation per artifact".

That is the same storage-level guarantee `docs/dev/event-contract.md` §3.5 gives
a strict-mode event id, expressed with what the substrate already has: an
existing identifier and an existing uniqueness guarantee. **No schema change,
no migration, no new index, no backfill.** Legacy rows keep their random ids
and cannot collide with derived ones.

Two identity modes, mirroring the event contract's own vocabulary:

| Mode | When | Identity |
|---|---|---|
| `strict` | the caller supplies a stable observation key | the key alone |
| `best_effort` | no key supplied | the submission's semantic fingerprint |

---

## 2. Root cause

### 2.1 The code path

```
mpm_evidence add  (tool handler / CLI / Go API)
  → AddEvidence(dm, in)                      internal/core/evidence_store.go
      id := GenerateID()                      ← a fresh id per DELIVERY
      INSERT INTO evidence (...)              ← no uniqueness beyond the PK
      → RecomputeConfidence(...)
          → loadEvidenceForRecompute(...)      ← every non-expired scoring row
          → computeConfidence(...)             ← Σ strength × independence × recency
```

Three facts make the defect:

1. **`internal/core/evidence_store.go` minted a fresh id per insert.** The id
   was a *row* identity, never an *observation* identity, so the ledger could
   not express "this is the same observation again".
2. **The `evidence` table had no uniqueness guarantee beyond `id TEXT PRIMARY
   KEY`** (`internal/core/schema.go:267-287`) and no observation-identity
   column. `independence_factor` exists and is the intended dependence signal,
   but it is caller-asserted and nothing enforces it against anything.
3. **`loadEvidenceForRecompute` (`internal/core/evidence_store.go:511-554`)
   selected every non-expired, non-`auto_capture` row**, and
   `computeConfidence` (`internal/core/confidence.go:119-138`) added each row's
   `strength × independence × recencyWeight` to the log-odds. Every duplicate
   counted in full.

### 2.2 The measurement (before the correction)

```
confidence: 0.5 → 0.6224593183800414 → 0.7310585651678245
confidence_history: (0.622459…, evidence_count=1, evidence_added)
                   (0.731058…, evidence_count=2, evidence_added)
```

Two deliveries of ONE observation, and two deliveries of TWO observations,
produced **the same** confidence — the substrate could not tell them apart.

### 2.3 What made it reachable in practice

* The `mpm_evidence` tool handler stamped `CreatedAt: time.Now()` on every
  call, so even a caller that wanted to say "this is the same observation"
  could not.
* `mpm_log_to_changelog` wrote one evidence row per confirmation/contradiction
  with a fresh timestamp each time, so a retried changelog entry inflated the
  confidence of the artifacts it touched.
* The critic re-evaluates the same pending proposal on every cycle. It does
  not write evidence rows itself (`internal/critic/` emits `mpm_theories
  propose` and `mpm_lessons save` findings), but an agent acting on a finding
  re-submits the same observation, and each submission used to count again.

---

## 3. Identity carriers that were inspected and rejected

The brief asked whether an existing identifier, provenance relationship or
uniqueness guarantee could carry the observation's identity. Measured answers:

| Candidate | Verdict | Why |
|---|---|---|
| `epistemic_provenance.event_id` | **rejected** | The table is an *artifact-relation* table: `UNIQUE (source_id, downstream_id, event_id)` with `polarity ∈ {assumes_true, assumes_false}` (`internal/core/schema.go:442-452`). An evidence row has no second artifact, so using it would mean inventing a fake source or downstream id — the exact anti-pattern `docs/dev/event-contract.md` §2.3 names for the `evidence` table ("cannot be stored here without inventing a fake artifact id"). It is also not wired to the evidence/confidence path at all today. |
| `evidence.reference_url` | **rejected** | It is pinned not to affect strength, confidence or verification (`internal/core/evidence_reference_url_roundtrip_test.go`: `TestEvidenceReferenceURL_DoesNotAffectConfidence`, `…_NotReadByConfidenceLoaders`), and it identifies a *source*, not an observation — two observations legitimately share a URL. Using it as an identity carrier would have broken a pinned invariant. |
| `independence_factor` | **rejected** | It is a *weight*, not an identity. Two distinct observations legitimately share a weight, and the same observation may be re-weighted. Conflating the two would make the dependence signal meaningless. |
| `created_at` | **rejected as the delivery time** | The substrate's own timestamp says when the submission ARRIVED; a replay arrives later by definition. (A caller-stated observation time is treated differently — see §5.3.) |
| `evidence.id` | **ADOPTED** | It was already the row's primary key — an existing identifier with an existing uniqueness guarantee. Making it *derived* rather than *minted* is the whole correction. |
| Host `callID` | **rejected** | `docs/dev/event-contract.md` §3.5 says it explicitly: nested calls share it, so it is not an event identity. It is not used anywhere in this correction. |
| `payload_hash` | **rejected** | §3.5: it means "the payloads were alike", not "the events were the same". Using it as a dedup key would repeat the withdrawn draft-1 rule B. |

**Conclusion: no existing column can carry the observation's identity, but no
new column is needed either** — the row's own id can.

---

## 4. The model

```
observation identity  — WHAT was observed, and therefore which deliveries
                        are the same claim about the world.
delivery             — WHEN and BY WHOM it was submitted.
```

The correction makes the identity durable and lets the substrate enforce it.

### 4.1 Storage identity

`evidence.id` becomes a pure function of
`(artifact_type, artifact_id, mode, identity material)`:

```
id = "obv1_" + hex(sha256(length-prefixed material))[:32]
```

* The prefix makes the derived population **auditable**: an operator can tell
  an identity-backed row from a legacy one by looking at the id.
* Legacy ids are 16 random hex characters (`GenerateID()`); a derived id is
  prefixed and longer, so the two populations can never collide. **No legacy
  row is rewritten and no backfill is needed.**
* The **primary key** is the uniqueness guarantee. A second delivery of the
  same observation lands on the same key, so the insert is a no-op — enforced
  by SQLite, not by an application-level check that a race could interleave.

### 4.2 The two modes

**`strict`** — the caller supplies `EvidenceInput.ObservationKey`, a stable
identity for the observation. The key alone determines the identity: the same
key is the same observation, and two different keys are two different
observations **even when their text is identical**. This is how a caller
distinguishes "two independent observations that happen to read the same" from
"one observation delivered twice".

The key is used **verbatim** (no trimming, no case folding): two keys that
differ in any byte are two different observations. The key is never persisted —
only its SHA-256 digest reaches the stored identity — and it is never echoed
into an error message.

**`best_effort`** — no key supplied. The identity is derived from the
submission's **semantic fingerprint**: `type`, `source_group`, `strength`,
`independence_factor`, `created_by`, `notes`. Two submissions that agree in
every semantic dimension are therefore ONE observation.

This is the conservative direction, and the direction matters: a delivery that
carries no distinguishing information cannot be *proven* to be a second
observation, and **under-counting corroboration is safe while over-counting it
is not**. The alternative — content-hash dedup as the primary rule — is the
rule `docs/dev/event-contract.md` §3.5 withdrew for events, and it is not
re-introduced here.

The fingerprint is length-prefixed, not delimiter-joined, so a delimiter inside
a note cannot make two different submissions collide.

---

## 5. The rules, case by case

| # | Case | Rule | Result |
|---|---|---|---|
| 1 | The same observation delivered twice with the same stable identity | `strict`: the key determines the id | **one row**, no recompute, no confidence change, no ledger row. The delivery is reported as a replay. |
| 2 | The same observation replayed after a retry or restart | the id is derived, so it survives the process and the file | **one row**, unchanged confidence. Proven across a real restart (the test closes the database and opens a fresh manager on the same workspace). |
| 3 | Two independent observations with identical textual content | `strict` with two different keys | **two rows**, two contributions. `best_effort` (no keys) collapses them — the conservative reading, and the reason the `observation_key` surface is requested in §10. |
| 4 | Different observations supporting the same conclusion | different content ⇒ different fingerprint ⇒ different id | **two rows**, two contributions. |
| 5 | Two conflicting observations | different content (strength, notes) ⇒ different id | **two rows**; the negative one keeps pulling the confidence down. |
| 6 | Multiple citations referring to the same original observation | citations resolve to artifacts and `tool_invocations`, never to evidence rows (`internal/core/citation.go:115-126`), and the confidence sum reads evidence rows only | **no inflation**: a citation repeated is still one citation, and the observations it rests on are counted once each. |

### 5.1 Retries and replays

A replay is **not an error** — the observation *is* recorded, which is what the
caller asked for. It is also **not a write**:

* no second evidence row;
* no confidence recompute ⇒ no `confidence_history` row and no change to the
  artifact's confidence;
* no work-verification re-derivation and no `evidence_observed` event, because
  the evidence set did not change.

The single exception: a replay that carries a `reference_url` the stored row
does not have records it on the stored row (filling a `NULL`). Dropping
caller-supplied provenance would lose data, and a reference URL is provably
confidence-neutral.

### 5.2 Conflicting evidence under a claimed identity

In `strict` mode, the same key delivered with **different content** is a
conflict, not a replay: the substrate refuses it (an error, zero partial
state) rather than silently keeping one of the two claims. The comparison is
made against the **stored row's own content**, so the check is a fact about
the ledger rather than an assumption about the caller.

### 5.3 The one exception to "the delivery time is not the identity"

When the **caller states** when the observation was made, that statement *is*
part of the identity. The substrate's own delivery timestamp never is.

That distinction is what lets a caller record several observations of the same
thing at different times — three checkpoints, two challenges raised by two
reviewers — without needing an observation key. It is also why the tool
handler no longer stamps `CreatedAt` (see §6).

### 5.4 A replay never resurrects a neutralized observation

The challenge path (F7.1, `internal/core/db.go:6048`) and the contradiction
resolver (`internal/core/db.go:7380`) deliberately **expire** the rows they
neutralize. A replay of such an observation does not bring it back to life:
the stored row stays authoritative, and the observation stays neutralized.

---

## 6. What changed

| File | Change |
|---|---|
| `internal/core/evidence_identity.go` | **new** — the observation identity: derivation, validation, the two modes, the storage-id function. |
| `internal/core/evidence_store.go` | `EvidenceInput.ObservationKey`; `AddEvidenceWithOutcome` (the outcome-reporting variant); the insert is now `ON CONFLICT(id) DO NOTHING` + a reconcile step; the delivery timestamp is filled in *after* the identity is derived. |
| `internal/core/evidence_tools.go` | the DM method reports the outcome internally; the **response shape is unchanged**. |
| `internal/core/changelog_mcp.go` | the confirmation/contradiction paths carry an **explicit observation key** (`changelog-<kind>:<commit>:<type>:<artifact>:<occurrence>`), so a retried changelog entry replays instead of re-counting. |
| `internal/core/tools/handlers.go` | the `mpm_evidence` handler no longer stamps `CreatedAt` — the transport does not get to state the observation's time. |
| `internal/core/evidence_identity_test.go` | **new** — the promoted regression suite (§9). |
| `internal/core/m5_trust_contract_test.go` | the D1 and D5 contract tests are **retired** (promoted, §9). |
| `internal/core/m5_trust_characterization_test.go` | the D1 and D5 characterizations are **retired** (the behaviour they pinned is now the corrected behaviour). |
| `internal/core/tools/work_complete_verification_regression_test.go`, `cmd/mpm/call_evidence_test.go` | two fixtures revised (§8). |

### 6.1 Why the changelog path needs an explicit key

`mpm_log_to_changelog` is re-submittable: the same commit, logged twice, must
be one observation. The key names the commit, the artifact, and **which
occurrence of that artifact the entry is** — so three contradictions of the
*same* artifact in one call are three observations (the caller asked for
three), while a retry of that same call replays all three. Counting the
occurrence *per artifact* rather than per position means a reordered retry
still maps each artifact to its own identities.

---

## 7. Confidence-calculation impact

* The **formula is unchanged**: `sigmoid(log(initialOdds) + Σ effective_evidence − decay)`.
* The **evidence set** is different: it now holds one row per observation
  rather than one row per delivery. `confidence_history.evidence_count`
  therefore counts *observations*, which is what the column's name says.
* An artifact's confidence **falls** if its ledger contained duplicated
  deliveries and the duplicates are removed. Existing stored values are **not**
  recalculated — see §9.
* The decay anchor (`lastPositiveAt`) is unaffected: it is the most recent
  *positive* evidence row, and a replay adds no row.
* `DeriveWorkVerification` (the work path) reads the same evidence set, so it
  sees the same de-duplication. Corroboration there now requires
  **distinguishable** observations — see §8.

---

## 8. Behaviour changes the correction forces

Repetition used to be corroboration in three places. Under M5 it is not, so
the fixtures that expressed "N observations" by repeating one payload had to
say what made the observations distinct. **No assertion was weakened**; three
fixtures now state their distinctness:

1. `internal/core/tools/work_complete_verification_regression_test.go` —
   two challenge rows for the F8.1 "contradiction surfaces on cancelled work"
   case now carry distinct notes ("challenge raised by reviewer 1/2"). The
   corroborated contradiction still surfaces.
2. `cmd/mpm/call_evidence_test.go` — the two evidence submissions behind
   `TestCallQueryConfidenceChanges_ReturnsDeltas` now carry distinct notes.
   Two history rows with deltas are still produced.
3. The work-verification corroboration rule (BLOCKER 3: aggregated strength
   ≥ 1.0 ⇒ verified) is unchanged; it now requires observations the substrate
   can tell apart. The existing tests that already stated a distinct
   observation time (`internal/core/blocker_2_3_test.go`,
   `internal/core/t20_1_contradiction_test.go`,
   `internal/core/cascade_invalidation_test.go`,
   `internal/core/epistemic_contradiction_test.go`,
   `internal/core/f71_f81_cancellation_challenge_test.go`) needed no change.

**This is the honest cost of the correction, and it is the safe direction:** a
work whose evidence cannot be told apart is no longer *verified* by repetition.

---

## 9. Historical-data compatibility

* **No migration.** Legacy rows keep their random ids and their stored values.
* **No backfill.** A derived id is prefixed and longer, so it can never
  collide with a legacy id.
* **Legacy rows keep counting exactly as they did.** A database that already
  holds duplicated evidence keeps its existing confidence values: no automatic
  recalculation is performed, because *lowering* a stored confidence is a
  policy decision for a human, not a side effect of a bug fix. This limitation
  is pinned by `TestEvidenceIdentity_LegacyDuplicatesKeepTheirHistoricalWeight`.
* **Legacy rows remain fully accessible**: listed by `mpm_evidence list`,
  `mpm why`, the confidence explanation, and the work-verification surfaces,
  with their ids, notes and references unchanged.
* **New rows sit beside old ones.** A legacy row and a new observation on the
  same artifact coexist and both count — which is correct: the legacy row is
  evidence that was recorded, and the correction does not rewrite history.

---

## 10. The request to Session A: one small shared surface

The correction closes D1/D5 **without** any shared change. What it cannot do
without one is let an agent name an observation, so that two *independent*
observations with identical text are not collapsed by the fingerprint.

**Measured reason the existing fields are insufficient:** no stored field can
express "these two deliveries are the same observation" *and* "these two are
different observations" at the same time. `reference_url` is pinned
confidence-neutral and identifies a source, not an observation;
`independence_factor` is a weight; `created_at` is the delivery time; the
`evidence` table has no observation column; `epistemic_provenance` is an
artifact-relation table with no second artifact to hang an observation on.
The row id *can* carry the identity (it now does) — but the identity has to
come from somewhere, and today it can only come from the content.

**Minimal proposed change (NOT implemented here — it is A's to approve):**

1. `mpm_evidence` tool schema (`internal/core/tools/registry_list.go`): one
   optional `params.observation_key` string, described as "stable identity of
   the observation this evidence records; the same key is the same
   observation, delivered again or not".
2. `internal/core/tools/handlers.go`: read it into `EvidenceInput.ObservationKey`.
3. `cmd/mpm/evidence_cmds.go`: one optional `--observation-key` flag.
4. A parity test beside `internal/core/tools/evidence_reference_url_parity_test.go`
   (the repo already has that pattern for `reference_url`).
5. Optionally — and only if the identity should be *readable* in the ledger
   rather than only re-derivable — an `evidence.observation_key TEXT` column
   with a partial unique index
   `CREATE UNIQUE INDEX ... ON evidence(artifact_id, artifact_type, observation_key)
   WHERE observation_key IS NOT NULL`. This is a schema change to
   `internal/core/schema.go`, the sprint's highest-collision file, and it is
   **not needed for enforcement**: the derived id already enforces uniqueness.
   Its only benefit is auditability ("which observation was this?").

**Compatibility:** purely additive. Existing callers omit the parameter and
keep today's behaviour. The parameter's validation already exists in the core
(`validateObservationKey`), so the tool surface inherits it.

**Smallest testable plan:** add the parameter to the registry + handler +
CLI, add the parity test, and promote
`TestEvidenceIdentity_IdenticalTextFromIdentifiedObservationsIsNotCollapsed`
to run through the tool surface as well as the Go API. No schema change, no
migration, no confidence formula change.

---

## 11. Test results

### 11.1 The promoted regression suite

`internal/core/evidence_identity_test.go` — ordinary tests, in every green
gate. It covers the nine required behaviours:

| # | Requirement | Test |
|---|---|---|
| 1 | Repeated delivery does not inflate confidence | `TestEvidenceIdentity_RepeatedDeliveryDoesNotInflateConfidence` |
| 2 | Retries preserve idempotency | `TestEvidenceIdentity_RetryPreservesIdempotency` (across a real restart) |
| 3 | Distinct independent evidence still raises confidence | `TestEvidenceIdentity_DistinctIndependentEvidenceStillRaisesConfidence` |
| 4 | Identical text from independently identified observations is not collapsed | `TestEvidenceIdentity_IdenticalTextFromIdentifiedObservationsIsNotCollapsed` |
| 5 | Conflicting evidence retains its intended influence | `TestEvidenceIdentity_ConflictingEvidenceRetainsItsInfluence` |
| 6 | Repeated critic cycles do not manufacture corroboration | `TestEvidenceIdentity_RepeatedCriticCyclesDoNotManufactureCorroboration` (5 cycles → 1 row, 1 ledger row) |
| 7 | Invalid or ambiguous identity fails safely | `TestEvidenceIdentity_InvalidIdentityFailsSafely`, `TestEvidenceIdentity_ContradictingIdentityIsRefused` |
| 8 | Existing legitimate evidence remains accessible | `TestEvidenceIdentity_ExistingEvidenceRemainsAccessible`, `…_LegacyDuplicatesKeepTheirHistoricalWeight` |
| 9 | Confidence recalculation is stable across repeated execution | `TestEvidenceIdentity_ConfidenceRecalculationIsStable` |

Plus: `…_DerivedRowIDIsStableAndPrefixed`, `…_ReplayRecordsAMissingReferenceURL`,
`…_ReplayDoesNotResurrectANeutralizedObservation`,
`…_WorkspacePathIsTheCanonicalOne`.

**Isolation:** every test uses the shared M5 fixtures
(`internal/core/m5_trust_helpers_test.go`), which pin `HOME`, `MPM_WORKSPACE`
and `MPM_DB_PATH` to disposable locations and open a disposable **file-backed**
database at the canonical `<workspace>/src/db/mpm.db` path. No test can reach
an operator's live database.

### 11.2 The M5 contract suite

The D1 and D5 contract tests were **promoted** into the ordinary suite (they
now pass in every green gate) and **removed** from the `m5contract`-tagged
file, whose remaining contracts (D3 ×2, D4) still fail — deliberately, for
defects that are not addressed here:

```
--- FAIL: TestM5Contract_UnresolvableCitationIsRejected      (D3)
--- FAIL: TestM5Contract_CitationPreservesItsType             (D3)
--- FAIL: TestM5Contract_CycleVaryingTagDoesNotCreateSecondProposal (D4)
```

The gate guard (`internal/core/m5_trust_gate_test.go`) still passes: every
`TestM5Contract*` function is still behind the tag, no characterization file
is tagged, and no Makefile or CI workflow mentions the tag.

### 11.3 Non-vacuity — proven, not asserted

The defect was **reinstated in an isolated scratch worktree** (`git worktree
add --detach /tmp/opencode/d1-nonvacuity 66f44a8`; the identity derivation
replaced by the old per-delivery `GenerateID()`) and the promoted suite was
run there. It fails, exactly and only, on the behaviours the correction
introduced. The scratch tree was removed afterwards.

### 11.4 Gates

| Gate | Result |
|---|---|
| `internal/core` (`go test -tags fts5`) | **PASS** |
| `internal/core/tools` | **PASS** |
| root module (`go test -tags fts5 ./...`) | **PASS** |
| M5 contract suite (`-tags 'fts5 m5contract'`) | 3 expected failures (D3, D3, D4) — the unaddressed defects |
| `go vet` | clean |
| `mpm-lint --gate` | **PASS** |
| `make test-scripts` | **PASS** |
| `make test-agent-installation` | **PASS** |
| `gofmt` | clean on every changed file |

### 11.5 Non-vacuity measurement

Scratch worktree at `/tmp/opencode/d1-nonvacuity`, defect reinstated (a fresh
`GenerateID()` per delivery, no identity). The promoted suite, run there:

```
--- FAIL: TestEvidenceIdentity_RepeatedDeliveryDoesNotInflateConfidence
--- FAIL: TestEvidenceIdentity_RetryPreservesIdempotency
--- FAIL: TestEvidenceIdentity_IdenticalTextFromIdentifiedObservationsIsNotCollapsed
--- FAIL: TestEvidenceIdentity_RepeatedCriticCyclesDoNotManufactureCorroboration
--- FAIL: TestEvidenceIdentity_ContradictingIdentityIsRefused
--- FAIL: TestEvidenceIdentity_DerivedRowIDIsStableAndPrefixed
--- FAIL: TestEvidenceIdentity_ReplayRecordsAMissingReferenceURL
--- FAIL: TestEvidenceIdentity_ReplayDoesNotResurrectANeutralizedObservation
```

with the defect's own numbers back:

```
expected exactly 1 evidence row after two identical deliveries, got 2
expected the repeated delivery not to change confidence: 0.622459303 -> 0.731058552
expected a retry to append no confidence_history row: 1 -> 2
cycle 5 changed confidence: 0.622459280 -> 0.924141798
expected 1 evidence row after 5 critic cycles, got 5
```

The tests that pin *preserved* behaviour — distinct independent evidence,
conflicting evidence, an invalid identity, existing legacy evidence, the
stability of the recomputation — still pass with the defect reinstated, which
is what makes them ordinary regression tests rather than a restatement of the
correction. The same suite passes on the branch. The tests are not vacuous.

---

## 12. Interactions with the other M5 defects (reported, not touched)

* **D2 / D4 (the critic).** The critic does not write evidence rows; it emits
  findings. The correction makes a *re-submitted* observation idempotent, so a
  critic that re-evaluates the same proposal can no longer inflate the
  proposal's confidence by re-submitting the same observation. This is a
  mitigation of D5's blast radius, not a fix of D4 (duplicate proposals).
* **D3 (citation validation).** Untouched. Note the split: citations resolve
  to artifacts and `tool_invocations`, never to evidence rows, so the D1/D5
  correction does not weaken D3's requirement that a proposal carry
  *resolvable* citations.
* **D6 (provenance).** Untouched. The evidence rows this correction writes
  still carry no `invocation_id` provenance; the observation identity is not a
  provenance record and does not pretend to be one.
* **D7 (`[]string` parsing).** Untouched.
* **The telemetry hunt's trusted-lesson creation.** Untouched (A's decision
  pending).
* **The event contract.** §3.5's rules are *applied*, not re-litigated: strict
  identity requires a stable key, exactly-once is not claimed (only "at most
  one stored row per observation"), and `callID`/`payload_hash` are not used as
  identities.
* **mpm-event/0.1's event store.** Untouched. Evidence rows are not events
  (§2.3), and this correction does not route lifecycle events into them.

---

## 13. Remaining M5 defects

| Defect | State after this branch |
|---|---|
| **D1** duplicate evidence inflates confidence | **CLOSED** — one row per observation; the confidence cannot move on a replay. |
| **D5** repeated delivery counted as independent corroboration | **CLOSED** — a replay is not a contribution; distinct observations still are. |
| D2 trusted-lesson surface has no pending state | OPEN (critic side closed in P3; the substrate half is open) |
| D3 unresolvable / mislabelled citations | OPEN (the typed-citation path is fixed; the legacy path is not) |
| D4 cycle-specific tags defeat proposal idempotency | OPEN |
| D6 missing invocation→artifact provenance | OPEN |
| D7 silent `[]string` parsing | OPEN |
| telemetry hunt creates trusted lessons | OPEN (A's decision pending) |

---

## 14. What was NOT done

* No schema change, no migration, no new index, no backfill.
* No tool-registry change, no new tool parameter, no CLI flag, no change to
  any response shape — the `observation_key` surface is **requested** (§10),
  not implemented.
* No change to the confidence formula, the decay term, the work-verification
  rule, or the reference-URL contract.
* No change to `epistemic_provenance`, `tool_invocations`, the event store or
  the citation vocabulary.
* No live database was modified; no deployment or installation change; no
  automatic critic scheduling was activated.
* No worktree of Session A's or Session C's was touched.

---

## 15. Commit and PR

* Branch: `feat/session-b-d1-evidence-identity`, from `origin/main` = `444ec86`.
* The correction and its tests: **`66f44a8`** —
  *"fix(core): an observation identity for evidence, so a repeated delivery is
  not corroboration (D1, D5)"*.
* This report is the second commit on the branch; the PR head is the branch
  head (see the MPM handoff for the link).
