# D — Coordination and ownership specification

> Establishes the ownership map for the three-session sprint. Session A is
> the sole merge and deployment authority.

## 1. Authority

| Role | Session | May do | May **not** do |
|---|---|---|---|
| **A — integration & release** | `~/src/mpm` | merge to `main`; sequence PRs; deploy to `~/.mpm`; edit any file | — |
| **B — intelligence core** | `~/src/mpm-core` | implement in its own worktree; open PRs | merge; deploy; edit Session A's or C's worktree |
| **C — OpenCode adapter** | `~/src/mpm-opencode` | implement in its own worktree; open PRs | merge; deploy; edit Session A's or B's worktree |

**Only Session A merges to `main` and deploys the installed runtime.** No
session may modify another session's worktree — not even to "fix" a one-line
conflict. A conflict is recorded and handed to Session A.

## 2. Ownership map — actual paths

Boundaries are drawn on real directories, verified by inspection (not guessed).

### Session B — MPM intelligence core

| Path | Contents | Notes |
|---|---|---|
| `internal/critic/` | `audit.go`, `hunts.go` | critic proposals, stale settling, durable cycles |
| `internal/scheduler/` | `dispatch.go`, `cascade_drain.go`, `active_uptime.go`, `drill_handler.go`, `cascade_wake_reconcile_handler.go`, `cron_retention_test.go` | scheduler, cascade drain, drills |
| `internal/core/evidence*.go` | `evidence.go`, `evidence_store.go`, `evidence_migration.go`, `evidence_reference_url.go` | evidence ingestion and provenance |
| `internal/core/provenance*.go` | `provenance.go`, `cascade_provenance.go` | provenance stamping |
| `internal/core/confidence*.go` | `confidence.go`, `confidence_maintenance.go` | scoring and decay |
| `internal/core/cascade*.go`, `internal/core/cascade_*.go` | cascade obligations | cross-artifact obligations |
| `internal/telemetry/` | telemetry | existing observation capability |
| `internal/core/skill*.go`, `internal/core/lesson*.go`, `internal/core/decision*.go` | trusted-knowledge writers | **only** where a proposal is promoted |

### Session C — OpenCode adapter and behavioural evaluation

| Path | Contents | Notes |
|---|---|---|
| `agent_installation/mpm-opencode/src/` | `index.ts`, `index.v2.ts`, `opencode-sdk.d.ts`, `resolve-mpm-binary.ts`, `workspace.ts` | **the plugin.** `index.v2.ts` is the live V2 implementation; `opencode-sdk.d.ts` is a hand-written subset of the real SDK types |
| `agent_installation/mpm-opencode/tests/` | `namespace_contract.test.ts`, `test_resolve_mpm_binary.test.ts` | adapter-local tests |
| `agent_installation/mpm-opencode/tests-build/` | compiled `.test.js` | build output |
| `agent_installation/mpm-opencode/package.json`, `package-lock.json` | Node package + lockfile | **the lockfile is pinned by a CI contract test** — treat as a shared interface |
| `agent_installation/mpm-opencode/reconcile.json` | host wiring manifest | the installer contract |
| `agent_installation/mpm-opencode/templates/` | rendered agent instructions | |
| `agent_installation/tests/` | cross-adapter parity suite | **shared** — Session A arbitrates |

### Session A — integration and release

| Path | Contents |
|---|---|
| `.github/workflows/build-test.yml` | CI |
| `Makefile` | gate targets |
| `scripts/` | installer, `scripts/lib/binary_transaction.sh`, `scripts/tests/` |
| `install.sh`, `uninstall.sh` | installation |
| `internal/core/schema.go` | **the migration authority** — every schema change lands here |
| `internal/core/db.go` | the single `DatabaseManager` |
| `internal/core/tools/registry_list.go`, `handlers.go` | **the tool surface** — the contract between the adapter and the core |
| `cmd/mpm/call.go` | the universal machine interface |
| `cmd/mpm-mcp/tools.go` | MCP registration |
| `internal/core/config/` | workspace and path resolution |
| `docs/SPEC.md` | the behavioural specification |
| `release_acceptance/` | release acceptance module |

### Shared interfaces — the actual contract points

| Interface | Owner of the definition | Consumers | Change procedure |
|---|---|---|---|
| **Tool registry** — `internal/core/tools/registry_list.go` (+ `handlers.go`) | Session A arbitrates; **B and C both depend on it** | MCP server, CLI, every agent adapter | A change to a tool's name, schema or description is a **shared-interface change**: it needs a PR from the proposing session, a contract test update, and A's merge. |
| **Event contract** — `event-contract.md` | Session A drafts; **B and C must both acknowledge the minimum shared interface (M1–M7)** | C emits, B ingests | §4 below. The **agreement surface is M1–M7** in the contract's §7 — a disagreement with an *implementation* choice (the contract's §6) is **not** a blocking objection. |
| **Schema migrations** — `internal/core/schema.go` `SafeMigrations` / `CommonIndexes` | Session A | B and C may both add columns | Every migration entry lands in one PR, reviewed together. Two sessions adding migrations concurrently is the highest-risk collision in the sprint. **No session may add a migration before A agrees the implementation PR** — the contract's §6.1 leaves the lifecycle-event storage path deliberately open, and one PR must settle it. |
| **Workspace / path resolution** — `internal/core/config/` | Session A | everything | Changing it changes where the live DB lives. Session A only. |
| **OpenCode package manifest** — `package.json` + lockfile | Session C | CI, installer | The lockfile is pinned by `scripts/tests/test_release_gate_bootstrap_wiring.py`; a dependency change must keep that contract test green. |
| **`docs/SPEC.md`** | Session A arbitrates | all | Five PRs touch it this round. Additive edits only; a **semantic** change needs A's review. |

## 3. Branch and worktree ownership

| Worktree | Branch | Owner | HEAD | Rule |
|---|---|---|---|---|
| `~/src/mpm` | `integration/session-a` | **A** | tip of `main` | integration base; A merges here first |
| `~/src/mpm-core` | `dev/session-b-core` | **B** | tip of `main` | B's feature branches branch from here |
| `~/src/mpm-opencode` | `dev/session-c-opencode` | **C** | tip of `main` | C's feature branches branch from here |

All three are **linked worktrees of one Git object store** (`~/src/mpm/.git`),
so they share objects and a single `origin`. They are not independent
clones.

> **`main` is not checked out in any worktree.** After the post-merge
> cleanup (2026-10-26) `~/src/mpm-release` was retired and the local `main`
> branch was left tracking `origin/main` without a worktree. Session A syncs
> it with `git fetch origin && git branch -f main origin/main`, and
> `integration/session-a` is fast-forwarded with
> `git merge --ff-only origin/main`.
>
> **The two integration checkpoints survive as branches, not worktrees:**
> `hal9001/r3-integration` (`cbe6af7`) and `integration/pre-release-12`
> (`a8c3b60`). Recreate a worktree for either with
> `git worktree add ~/src/worktrees/<name> <branch>`. They are **evidence
> only** — `main` is the verified integration result.

Per-session feature branches:

```
Session A:  integration/session-a, main (unattached), chore/<topic>
Session B:  dev/session-b-core, feat/session-b-<topic>
Session C:  dev/session-c-opencode, feat/session-c-<topic>
```

**A session may only ever have one branch checked out in its own worktree.**
If a session needs a second branch, it creates a linked worktree under
`~/src/worktrees/<session>-<topic>/` and records it in the ledger.

**No session may check out another session's branch.** A branch is checked out
in exactly one worktree; Git enforces this and the attempt will fail — that
is the desired outcome, not an obstacle to work around.

## 4. Shared-interface change procedure

1. **The proposing session opens an issue-shaped note** in the coordination
   ledger (§5) stating: the interface, the proposed change, the consumers
   affected, the compatibility story, and the contract tests that must change.
2. **The other session confirms impact** within one working day, by reading
   the proposed change and reporting what breaks. Silence is not consent —
   after one day, A decides.
3. **A opens the PR.** The PR must:
   - carry the interface change and its contract tests in the **same** PR
     (a change without its test is not reviewable);
   - update the affected session's ledger row in the same PR;
   - state explicitly which session must rebase.
4. **The dependent session rebases** its own branch onto the merged change
   and re-runs its own gate. It does not merge.
5. **A merges**, in the order established by the integration matrix.

**The event contract (§ `event-contract.md`) is the one interface where
"agreement" must be explicit and versioned**, because both sides implement
against it simultaneously.

## 5. Coordination ledger

> Checked in so that agents can recover the operational state **without a
> functioning MPM connection**. The MPM database is the durable store; this
> file is the recoverable one. If they disagree, the git file wins for
> *operational state* and the database wins for *reasoning*.

### Active branches and PRs

> **Refreshed 2026-10-26.** Every corrective PR is **merged**. The rows
> below now record the **merge commits on `main`**, not the PR heads.

| Branch | PR | Merged as | Session | Status |
|---|---|---|---|---|
| `fix/hal9001-mpmcli-env-isolation` | #102 | `e093bcc` | A | ✅ merged |
| `fix/hal9001-provenance-test-isolation` | #89 | `bf142f3` | A | ✅ merged |
| `fix/hal9001-gc-reporting-honesty` | #90 | `8c640d1` | A | ✅ merged |
| `fix/hal9001-ingest-undo-honesty` | #91 | `9e62fd2` | A | ✅ merged |
| `fix/hal9001-embedding-migration-diagnostics` | #87 | `b50f27c` | A | ✅ merged |
| `fix/hal9001-r2-save-evidence` | #96 | `ea775ad` | A | ✅ merged |
| `fix/hal9001-shred-agent-auth` | #85 | `9a5661c` | A | ✅ merged |
| `fix/hal9001-raw-memory-shred-linkage` | #88 | `3c18840` | A | ✅ merged |
| `fix/hal9001-confidence-provenance` | #86 | `765334b` | A | ✅ merged |
| `fix/hal9001-r3-directive-retention` | #97 | `d89c11c` | A | ✅ merged |
| `fix/hal9001-r3-schema-init-concurrency` | #98 | `321a695` | A | ✅ merged |
| `fix/hal9001-r3-install-next-steps` | #99 | `f7e0185` | A | ✅ merged |
| `fix/hal9001-r2-confidence-decay` | #95 | `f2c1ff4` | A | ✅ merged (the one schema conflict, resolved by keeping both index lines) |
| `fix/hal9001-signal-test-determinism` | #103 | `cefe14b` | A | ✅ merged |
| `fix/hal9001-ci-gate-integrity` | #104 | `b5b1840` | A | ✅ merged — the full-suite sweep can now fail |

| Branch | Session | HEAD | Status |
|---|---|---|---|
| `integration/session-a` | A | tip of `main` | Session A integration base |
| `dev/session-b-core` | B | tip of `main` | Session B base |
| `dev/session-c-opencode` | C | tip of `main` | Session C base |
| `main` | A | tip of `main` | tracks `origin/main`; **not checked out** — syncs with `git branch -f main origin/main` |
| *(no docs branch)* | A | — | the documentation set is merged into `main` (PRs #105, #106, #107); its branch was deleted after the merge |
| `hal9001/r3-integration` | A | `cbe6af7` | pre-existing combined tree — **evidence only**, no worktree |
| `integration/pre-release-12` | A | `a8c3b60` | **validation checkpoint — evidence only, not a baseline**, no worktree |
| `chore/mpm-core-gomod-uuid-direct` | A | `1a16e90` | **the one piece of unmerged work found in the whole repository**, preserved onto a branch before its worktree was removed |
| `dev/session-b-core` | B | `dev/session-b-core` (audit commit, on top of `70deac2`) | Session B audit deliverable — **not merged**. `docs/dev/session-b-audit.md`: M1–M7 acknowledged with no blocking objection (recorded in `~/.mpm-dogfood` as decision `3a987eca2c708fac`); capability audit; minimal-change recommendation; **no schema change requested**. |
| `dev/session-b-core` | B | `dev/session-b-core` (regression commit, on top of the audit commit) | Session B M5 trust-defect regression coverage — **not merged, no production code changed**. `docs/dev/session-b-m5-trust-regression.md`. Six green characterization tests pin the CURRENT behaviour of defects D1–D5; six contract tests assert the M5-COMPLIANT behaviour and **fail today by design** behind the `m5contract` build tag; `internal/core/m5_trust_gate_test.go` (always green) mechanically proves the contract suite can never enter a green gate, and its non-vacuity was proven against three deliberate violations. **Decisions required from A: A-1 (the M5 trust tension), A-2 (the D1/D5 observation-identity mechanism — schema), A-3 (the D3 registry parameter), A-4 (the D4 mechanism), A-5 (gate acknowledgement).** |

### Post-merge cleanup record — 2026-10-26

With every agent inactive and every PR merged, the tree was inventoried and
reduced. **Nothing was deleted before it was proven obsolete.**

| What | How many | How it was proven obsolete |
|---|---|---|
| local branches deleted | **38** | every commit on them is present in `main` by patch-id equivalence, or reverse-applies cleanly onto `main`, or (where the patch context had been rebased) every symbol it introduces exists in `main` |
| worktrees removed | **26** | each held only a branch already proven merged, or a debug-only scratch state; nothing unique was lost |
| remote branches deleted | **15** | the merged-PR heads `origin/fix/hal9001-*`; all proven present in `main` |

**Reclaimed:** `~/src/worktrees` went from **1.8 GB to under 25 MB** (4 KB once the
documentation worktree was retired as well).

**What was preserved, deliberately:**

* `hal9001/r3-integration` and `integration/pre-release-12` — the two
  integration checkpoints, kept as branches.
* `chore/mpm-core-gomod-uuid-direct` — the **only** unmerged work found
  anywhere in the repository: an uncommitted `internal/core/go.mod` edit in
  the `baseline-main` worktree, marking `github.com/google/uuid` as a direct
  dependency of the `mpm-core` module (it is imported directly by five files
  in `internal/core`, but the module's `go.mod` listed it as `// indirect`).
  It was saved onto a branch off the current `main`, verified to build, and
  is **available to merge as a one-line PR** — it was deliberately not
  merged in this foundation task.
* `cmd/dbg_main.go.tmp` in `~/src/mpm` — a pre-existing untracked file
  (579 B, 2026-10-17). **Not mine to delete.**

**What was discarded, and why:** two debug-only modifications in worktrees
that were being removed — three `PROBE` print statements added to
`scripts/tests/test_binary_transaction.py` during the #103 investigation, and
a *duplicated* comment block in `scripts/tests/test_install_sh_contracts.py`
(the equivalent change, with a single clean comment, is already in `main`).
Neither was product work.

### Adjudicated implementation sequence — 2026-10-26

> Recorded by Session A after independently verifying both investigations.
> The full adjudication is [`session-a-adjudication.md`](session-a-adjudication.md).
>
> **The contract is AGREED** on `mpm-event/0.1` (M1–M7, zero blocking
> objections from either session), with amendments A1–A13 applied to
> [`event-contract.md`](event-contract.md).

| # | PR | Owner | Depends on the contract? | Schema? | Registry? |
|---|---|---|---|---|---|
| **P1** | `fix(opencode): deliver the wake context` — extraction (`content`, type-checked, never coerced), per-session cache, concurrent-build suppression, retry-on-empty, session attribution on the fetch | **C** | No | No | No |
| **P2** | `feat(theory): typed, validated, idempotent evidence citations on proposals` — optional `source_type` beside `source_ids`, validated before insert; plus the one-line `artifact_type='skill'` registry correction (A-9) | **B** | No | **No** | Yes (one optional parameter) | ✅ **merged** (#120) |
| **P2b** | **gate the `critic_audit` wake kind at the arming AND dispatch boundaries** until the trust boundary is enforced | **A** | No | No | No | ✅ **merged** (#118) |
| **P2 status** | **PR #120 MERGED** (`9337f04`, merged by A after independent review). Branch `feat/session-b-p2-citations`. Report: `docs/dev/session-b-p2-citations.md`. The typed citation path is enforced in the CORE (transport-independent): a typed citation that does not resolve, is wrongly typed, or points at a soft-deleted row is **rejected**, and the rejection is **atomic** — no theory row, no dependency patch, no citation row (`TestP2Citation_AtomicFailureLeavesNoPartialRecord`). The registry enum and the core vocabulary are pinned to each other by an **enforceable test in both directions**. The **legacy untyped** path is **unchanged** — that is **D3**, now recorded as a preserved backward-compatibility exception in contract amendment **A19**. **21 tests** green. No schema change, no deployment. |
| **P3** | `feat(critic): a retrospective missed-opportunity hunt` over the existing `tool_invocations` × `artifact_provenance` join | **B** | No | No | No | 🔄 PR open |
| **P3 status** | **PR #123 MERGED** (`c7e2dc7`, merged by A after independent review). Branch `feat/session-b-p3-missed-opportunity`, base `9337f04` (P2 merged). Report: `docs/dev/session-b-p3-missed-opportunity.md`. One new hunt (`internal/critic/missed_opportunity.go`), no schema change, no new tool, no registry change. **The join alone is NOT sufficient**: `artifact_provenance.invocation_id` is populated ONLY by the `mpm_memory save` path (0 of 26 live decision rows join), so a join-only hunt reports a session that recorded 8 decisions as one that recorded nothing — the audit's own "real observed gap" was a false positive. The hunt therefore requires TWO signals to be absent: the join, and a declared vocabulary of recording actions. **The cycle tags are gone from every hunt** (`survival_asymmetry`, `weak_theory`, `poison_pill`) — inert on the lesson surface, fatal on the proposal surface — which is what makes A's §6c.3 surface switch safe. The poison pill's volatile confidence moved to the Reason for the same reason. **26 tests** (20 hunt + 3 identity + 3 inverted/updated), all green, with **non-vacuity proven against six deliberate violations**. Gates: `internal/critic` ok, `internal/core` ok, `internal/core/tools` ok, root `go test ./...` ok, `mpm-lint --gate` PASS, `make test-scripts` ok (540), `make test-agent-installation` ok (494). M5 contract suite still **6 expected failures** (unchanged). **No schema change, no deployment.** **Session A's rulings on the five open items are recorded in the merge commit `c7e2dc7` and summarised in the row below.** |
| **P3 rulings** | **A-new-4** — **approved**: keep the missed-opportunity hunt registered in `critic.New` by default. It is pending-only, idempotent, non-promoting, and its citations are typed and resolvable. The P2b gate (#118) keeps the *automatic* path off — **the gate is the switch, not the hunt**. **A-new-5** — **accepted, and A is bound by it**: the recording-action vocabulary's invariant is about actions that persist **knowledge or continuity** state, **not merely durable state**. **P5's `mpm_system action=event_ingest` must NOT be added to it** — an observation is explicitly not knowledge (contract §3.9 rule 2), and adding it would suppress a genuine knowledge-capture opportunity. **A-new-6** — **accepted as a separate tracked defect (D6), deliberately not in this PR**: it changes provenance behaviour on every write path, and the two-signal design already handles it. **A-new-7** — **accepted as a separate tracked defect**: latent in production, and a helper that returns `nil` for a valid input is a drop, not a default. **A-new-8** — **approved, and required before the P2b gate can be lifted**: §6c.2 limitation 4 is resolved in favour of the surface switch (the critic's output becomes a queue item rather than a durable lesson). **The requirement:** the two lesson-emitting hunts must cite something **resolvable** — the artifact each observed, with its own declared type through P2's typed path. It can be a follow-up PR; **the P2b gate stays on until it lands**. |
| **P3b status** | **PR #127 MERGED** (`c733288`, merged by A after independent review). Branch `feat/session-b-p3b-surface-switch`. Report: `docs/dev/session-b-p3b-surface-switch.md`. **The A-new-8 surface switch**: the last two lesson-emitting hunts (`survival_asymmetry`, `weak_theory`) now emit `mpm_theories propose` — a **pending** proposal — with **typed, resolvable citations** through P2's path (`source_type: "memory"` / `"theory"`, the artifact each hunt observed). The hypotheses assert only what the cited artifact shows; neither names a lesson. The critic's M5 contract failure count went **1 → 0** because the defect is **FIXED** — the deleted contract file's own header prescribed exactly this retirement, and its assertion survives **stronger** in the always-compiled `TestP3b_CriticFindingIsNotTrustedWithoutReview`. The core's M5 contract failures are **unchanged at 5**. One **detection** change (not just a surface change): the survival aggregate is now scoped to `collection IN ('', 'memories')`, which was **unavoidable** — P2's `memory` citation type resolves only against that collection, so an unscoped citation would be rejected and the proposal could never land. |
| **P4** | `feat(opencode): observe lifecycle events` — subscribe, consume off the hot path, map an explicit allowlist, emit `session.observed` / `session.started` (when actually seen) / `tool.outcome` / `compaction.*` | **C** | Yes (M2, now agreed) | No | No | 🔄 PR #119 open |
| **P5** | The durable `mpm-event/0.1` ingest: the `lifecycle_events` store, the `mpm_system event_ingest` action, the read-backed receipt, and `mpm_system event_counters` | **A** | Yes | **Yes — one narrow core-owned `lifecycle_events` table** (§6.1 decided 2026-10-27, amendment A17) | Yes (two actions on `mpm_system`) | ✅ **merged** (#122, `f71a6ac`) |

* **P1 and P2 are independent and may proceed in parallel.** P3 depends on
  P2. P4 depends on the contract and on P1's plumbing. P5 is A's and is
  explicitly last.
* **P2b is merged** (#118): a narrow rejection at the producer and at the
  dispatch boundary, no schema and no tool-surface change. The critic's
  automatic path is now refused **twice over** — a wake cannot be armed,
  and a wake that already exists is not executed.
* **A-11 is AMENDED, not merely deferred** (see
  `session-a-adjudication.md` §6c). The critic **can** emit an existing
  untrusted proposal — `mpm_theories propose`, a `pending` hypothesis with
  the same citation mechanism — **without any schema change**. It should,
  but only after P2 and P3 land; until then the P2b gate keeps the
  automatic path off entirely, which is strictly safer than switching
  surfaces while D3 and D4 are live.
* **The M5 defect list, decomposed — 2026-10-27 (after P3b).** The live
  signal is **5 core** M5 contract failures (the critic's own sixth went to
  zero when P3b fixed it). Which defect each failure names:

  | Defect | Status | Where it bites |
  |---|---|---|
  | **D1** — duplicate evidence inflates confidence | **CLOSED** (#130) | the evidence row's id is now the observation's identity and the PRIMARY KEY enforces "at most one stored row per observation per artifact"; the contract assertion was PROMOTED into `internal/core/evidence_identity_test.go` |
  | **D3** — unresolvable citation accepted | **live on the LEGACY path only** | the typed path is fixed (P2); the legacy untyped path is a preserved exception (A19) |
  | **D3** — citation does not preserve its type | **live on the LEGACY path only** | same |
  | **D4** — a cycle-varying tag creates a second proposal | **live (substrate half)** | the emission side is fixed (P3); the identity hash is shared dedup semantics and needs its own arbitration |
  | **D5** — repeated delivery counts as independent corroboration | **CLOSED** (#130) | same mechanism as D1; `TestEvidenceIdentity_RepeatedDeliveryDoesNotInflateConfidence` |
  | **D2** — the critic writes trusted lessons | **CLOSED on the critic side** (P3b); the core half (no pending state on `lessons_base`) remains | the critic now writes no trusted lesson anywhere; `TestP3b_NoHuntTargetsTheTrustedLessonSurface` guards it |
  | **D6** — `artifact_provenance.invocation_id` is populated only by `mpm_memory save` | **live, tracked separately** | 0 of 26 live decision rows join; P3's hunt works around it with a two-signal design |

  **The live signal is now 3 core M5 contract failures** (down from 6):
  D3 ×2 (the legacy path) and D4 (the substrate identity hash).

  **D1 and D5 are closed**, so the two preconditions for the P2b gate are
  both met: the **emission** side (P3b — the critic writes no trusted lesson
  anywhere) and the **corroboration** side (#130 — a repeated delivery no
  longer raises a confidence). What remains is D3 on a path the critic does
  **not** use and D4's shared dedup semantics.

  **So lifting the gate is no longer blocked by a defect — it is a POLICY
  DECISION about enabling scheduled critic execution**, and it should be made
  deliberately rather than by accident. The brief for this round says to keep
  the gate active, and it stays active. Automated critic scheduling stays
  **off**.

* **A capability gap worth closing (not a defect).** The observation key is
  not exposed on the `mpm_evidence` tool surface, so every tool-mediated
  caller — including the critic — is in `best_effort` mode and cannot
  distinguish *"two independent observations that happen to read the same"*
  from *"one observation delivered twice"*. That is a **capability** gap, not
  a correctness gap: the D1/D5 scenario is the fingerprint's case and is
  covered.
  **Automated critic scheduling is not enabled** while the duplicate-
  evidence, proposal-idempotency and trust-promotion risks are unresolved.
  The critic's automatic path is **wired but not armed** — the live
  workspace has no `critic_audit` wake and the critic has written no trusted
  lesson — but the wake `kind` is caller-supplied and unvalidated, so it is
  one API call from firing. P2b closes it.
* **P3's proposals may not cite a gap.** An invocation with no corresponding
  artifact is a *candidate opportunity*, never a citation and never proof
  that a lesson exists (contract §3.7, amendment A14).
* **P5's ingest transport must not collide with the missed-opportunity
  hunt.** There is **no collision today**: the reference adapter's ingest
  issues a plain `mpm call` *without* the provenance env, so its audit row
  lands with `framework_session_id = NULL`, and the hunt filters
  `coalesce(framework_session_id,'') <> ''` — so those rows are outside both
  the candidate query and the citation query. **The constraint for the
  future:** if the ingest transport is ever changed to carry the provenance
  env, the adapter's ingest calls would enter the candidate set and (being
  the oldest) would become **the cited evidence**. The clean fix is a
  symmetric **exclusion** vocabulary beside `recordingActions`. Recorded by
  A; C has been told directly.
* **P3 and P4 are complementary, not substitutes.** P3 covers MPM-visible
  activity; P4 extends detection to sessions that never invoke MPM. Neither
  makes the other redundant.
* **The lifecycle-events table was NOT the first milestone — and is now
  authorised (P5, 2026-10-27).** B's audit showed the first end-to-end
  acceptance test needed no new table: `epistemic_provenance` already
  provides typed, validated and idempotent citations, and the retrospective
  join runs over existing tables. That remains true of the *acceptance test
  itself*. But P4 now produces real envelopes and there is **nowhere for
  them to go**, which is the measured gap §6.1 asked for. One narrow
  core-owned table — `lifecycle_events` — is authorised (amendment A17), with
  the DDL, index plan, rollback and regression plan in
  `session-a-p5-lifecycle-ingest.md`. No `SafeMigrations` entry is
  authorised: the migration is a `CREATE TABLE IF NOT EXISTS` in
  `BaseTables`, which is self-applying.
* **P5 × P3: the ingest's audit row must not carry the host session id.**
  The reference adapter's ingest transport issues a plain `mpm call`
  **without** the provenance env, so the ingest's `tool_invocations` row
  lands with `framework_session_id = NULL` — verified end to end against
  the dogfooding workspace. That matters because P3's hunt groups
  `tool_invocations` by `framework_session_id`: a NULL session id places
  the adapter's ingest rows outside both the hunt's candidate query and
  its citation query, so they cannot be counted as the agent's
  MPM-mediated work and cannot become the cited evidence.
  **If the ingest transport is ever changed to carry the provenance env**
  — a reasonable thing to want, so the ingest's own audit row is
  attributable to the host session — the adapter's ingest calls would
  enter the hunt's candidate set: they would count toward
  `MinInvocations`, and (being the oldest) they would be **cited as the
  evidence**, which would make the proposal rest on "the adapter observed
  some events" rather than on the agent's work. The clean fix is a
  symmetric **exclusion** vocabulary beside `recordingActions`:
  invocations that are not the agent's work, excluded from both the
  candidate set and the citations. **No code change is needed today.**
* **P3 will need a `lifecycle_event` citation source type.** Contract §3.7
  says a critic proposal "must carry the `event_id`s it rests on, and each
  of them must resolve to a stored event (or to a `tool_invocations` row)".
  P2's canonical vocabulary (`internal.CitationSourceTypes()`) has
  `tool_invocation` but **no** `lifecycle_event`, so a proposal that cites a
  stored lifecycle event cannot be typed against it today. The registry enum
  and the core vocabulary are pinned to each other by an enforceable test, so
  adding the type is a one-line change in both places — but it must be done
  in **P3**, when the critic's proposals start citing event ids, not
  retrofitted afterwards.
* **The existing specialised MPM tool surface is unchanged** pending
  evidence-based review.
* **P4 must not start until the structural allowlist test exists**, and that
  test must use the **real captured shapes** in
  `agent_installation/mpm-opencode/measure/evidence/`.

### Cross-session dependencies

| From | To | Dependency | Status |
|---|---|---|---|
| C | B | C emits lifecycle events; B ingests and validates them | ✅ **unblocked** — C's revised acknowledgement records **zero blocking objections** (`docs/dev/session-c-event-stream-correction.md`). The host event stream **is** reachable from a plugin (`ctx.event.subscribe({signal})` → `AsyncIterable`); 31 event types were captured, including `session.created`, `session.compaction.started`/`.ended`, `session.tool.*` and `session.execution.*`. B should now assume **event-derived observations**, not hook-derived only. **B must tolerate missing and duplicate events:** the stream is volatile by the host's own contract (slow consumer → overflow), and the adapter's own first session can miss `session.created`. Two items still need B's input: the §3.7 host-status → `ToolOutcomeClass` mapping, and whether the proposal shape can cite `event_id`s. |
| C | A | C's adapter changes must keep `test_makefile_agent_adapter_wiring.py` and `test_release_gate_bootstrap_wiring.py` green | open — C's first PR (the wake-context extraction fix) touches no shared interface; the later plugin-surface PR (adding `mpm_help` / `mpm_resolve` / `mpm_work`) **is** a shared-interface change under §2 and needs A's PR. |
| B | A | B's schema migrations land in `internal/core/schema.go` — one PR, reviewed together | open — and **no migration may be added before A agrees the implementation PR** |
| B | C | B may consume the adapter's raw event stream; the stream is *evidence*, not trusted knowledge | open — **measured constraint (corrected 2026-10-26):** the host event stream **is** reachable from a plugin (`ctx.event.subscribe({signal})` → `AsyncIterable`; 31 event types captured). It is **volatile by the host's own contract** (a slow consumer overflows and fails the stream; no replay), so the adapter must queue and must never `await` a subprocess inside the iterator. Missing observations are normal and must never be inferred. |
| A | B, C | A merges; B and C submit PRs only | standing |

### Session C — first-investigation deliverables (2026-10-26)

| Item | Where | Status |
|---|---|---|
| Technical report — host findings, observability matrix, tool-surface measurements, failures, minimal integration, contract changes, recommended PRs | [`session-c-host-findings.md`](session-c-host-findings.md) | ✅ complete (first pass) |
| **Second pass — event-stream correction: M2/M3 objections withdrawn, corrected observability matrix, redaction hazard, environment verification** | [`session-c-event-stream-correction.md`](session-c-event-stream-correction.md) | ✅ complete — **supersedes part of the first pass** |
| M1–M7 acknowledgement — **final: zero blocking objections** | [`event-contract.md`](event-contract.md) §10 (Session C block, revised; the superseded block is preserved beneath it) | ✅ revised 2026-10-26 |
| Measurement harness + raw evidence | `agent_installation/mpm-opencode/measure/` (incl. `probe/event_stream_probe.js` and `evidence/event_stream_probe_run*.jsonl`) | ✅ committed |
| Shared dogfooding records | `~/.mpm-dogfood` — first pass: memory `880914fcb252f6ce`, lessons `3d4c577a76580dc5`, `4007e7bcf2ae89ce`, `850112858d8562f8`, `24486718472717f8`, handoff `de56dc39fcf8fb14`. **Second pass:** decision `281d5267b2e9d4d4` (supersedes the first acknowledgement `8cf1f0e3cc41465e`), practice `0748acce2e286a4e` (probe methodology), warning `66e1bade14d256f8` (environment pins do not reach the host), handoff `287bba3565dbfc66` (to Session A, with open questions) | ✅ written |

**Three findings A must act on independently of the contract:**

1. **Production defect — the wake context is never injected into an OpenCode
   agent's system prompt.** The installed plugin reads
   `payload.context ?? payload.text`; `callMpm` sets `payload` to the **whole
   envelope**, and `read_wake_context` with `format: "system-prompt"` returns
   `{content, format, success}`. Consequences: wake context never delivered,
   per-session cache never populates, **~19 redundant `mpm call mpm_context`
   spawns per session** (~39 ms each, unattributed). Verified against **both**
   the stale installed binary and the dev build. Fix and tests: report §11.1.
2. **The environment pins do not reach the running host.** No `MPM_*` variable
   exists in the desktop app, the CLI service or the agent process
   (`/proc/<pid>/environ`). The plugin therefore resolves `~/.mpm` and the
   **pre-sprint** binary `0.1.0-dev+g3df72315`. Measured consequence: 410 plugin
   wake-context fetches in 2 h hit the **live** DB, and 12 memories + 2
   decisions written by OpenCode agents in 12 h also landed there — while
   `~/.mpm-dogfood` is written only via explicit shell exports with
   `framework_session_id = NULL`. **The sprint's reasoning channel is split
   across two databases.** Correction report §6.
3. **The plugin's tool surface omits `mpm_help`, `mpm_resolve` and `mpm_work`**
   (plus `mpm_route` and the two blob tools). `mpm_work` is the contract's
   `task.boundary` capability, and it is reachable only shell-side — where
   `framework_session_id` is lost. Report §7.2.

### Integration status

| Item | Verified SHA | Verified by | Gate |
|---|---|---|---|
| **The merged result** (15 PRs) | `b5b1840` (the tip of `origin/main` carries this document on top) | `make build` + `make test-race` + `make test-scripts` (540 tests); CI run `40337045142` — all three checks green | ✅ **this is the verified integration result** |
| 12-way corrective checkpoint | `a8c3b60` (`integration/pre-release-12`) | `make build` + `make test-race` at the time | **evidence only** — superseded by `main`; never installed |
| pre-existing combined tree (#85–#98) | `cbe6af7` (`hal9001/r3-integration`) | pre-existing | **evidence only** — superseded by `main`; never installed |
| installed runtime | `3df72315` (`~/.mpm`) | the live install | **predates all fifteen corrective PRs**; deliberately not deployed |

### Release-cleanup round — 2026-10-27

Session A's round on C's OpenCode-only release cleanup. `origin/main` =
`4de89ea` at the time of writing; CI run `40517704744` green.

| Item | Where | Status |
|---|---|---|
| **#125** — the OpenCode documentation corrections and the retirement of the superseded automatic-agent-install guide | merged as `84d852e` | ✅ **verified, not re-merged.** The retired document is gone; the only remaining references to it are the two tests that **pin** its retirement; root `install.sh` names no adapter. All five accuracy claims (plugin-directory registration, `ctx.session.hook("context")` / `ctx.shell.hook("create.before")`, `MPM_WORKSPACE` → `$HOME/.mpm`, the `MPM_BINARY` → `~/.mpm/bin/mpm` → `~/.local/bin/mpm` → PATH chain, Node 20+, bare tool names) check out against source. |
| **#126** — the legacy-adapter removal proposal | adjudication posted as a PR comment | ✅ **approved.** OpenCode-only supported-host scope; removal authorized in a **separate implementation PR** under four decisions (telemetry spec relocated, historical `created_by` not rewritten, the refresh-safety guarantee preserved host-independently, the JS family handled explicitly). |
| **#133** — the §3 host-independent preservation regression | `agent_installation/tests/test_legacy_installation_preserved.py` (9 regressions) | ✅ **merged into `main` with this ledger entry.** Names no adapter directory; the adapters that exist are **discovered at run time**, so it is correct before and after the removal — verified by running it against a tree with **every adapter directory removed**. |
| **#134** — the adapter's client-side counters do not reconcile | `agent_installation/mpm-opencode/src/event-subscriber.ts` | ⏳ open with C. `enqueued=10, delivered=9` while the server stored 10. `stop()` snapshots before any await (one delivery in flight) and the reconciling `phase:"final"` snapshot never lands, because the plugin's cleanup is fire-and-forget and the host exits first. **No `pending`/buffer-depth field exists**, so "in flight" and "lost" are indistinguishable. The **wire** contract is unaffected. |

**The §3 guarantee has a carrier that outlives the removal.** Every
host-specific safety suite dies with its adapter — taking the guarantee's
only test at the exact moment it becomes load-bearing. The carrier is
therefore host-independent: a synthetic host with no adapter, and a
**simulated retirement** (a copy of `agent_installation/` with every
adapter directory removed). Its non-vacuity was **demonstrated**, not
asserted: a reconciler that cleans up orphan managed blocks fails 5 of the
9; an installer that clobbers the user's own text fails exactly the one
that covers it; the shared-script lint catches both the direct and the
indirect (`rm -rf "${VAR}"`) form of a destructive cleanup.

**Measured release-gate impact of the removal** (per-adapter populations,
each figure from an actual run):

| Family | Per adapter | Total today | After removal | Floor |
|---|---|---|---|---|
| Python | claude-code 68, hermes 38, memory-openclaw 13, **opencode 119**, pi 74 | 312 | **119** | 150 → **must drop** (measured: `FATAL: collected 119 Python tests, floor is 150`) |
| JS | memory-openclaw 143, auto-mode-persona 21 | 164 | **0** | 100 → **explicit empty handling; 0 is wrong in either branch** |
| TS | opencode 89 | 89 | 89 | 15 → unchanged |
| umbrella | `agent_installation/tests/` | **503** | ~395–410 | 300 → unchanged |

`scripts/tests/test_makefile_agent_adapter_wiring.py`'s *"at least 3
Python suites"* assertion **fails** at 1 (measured). Its *"at least 1 JS
suite"* assertion is a **false green**: its regex parses the following
comment line as the list when the list is empty.

**The coupled halves cannot land separately.** The Go registry
(`cmd/mpm/onboard_hosts.go`, `cmd/mpm/onboard_cmds.go`) and the Python
registry (`agent_installation/scripts/check_installed_managed_blocks.py`)
are guarded by two-way drift guards, and the `Makefile` adapter lists are
guarded by a recipe that fails closed on a missing `tests/` directory.
The removal PR must therefore carry both sides, or `main` is red in
between. A granted C the shared-surface edits for exactly this reason.

**P4/P5 end-to-end verification — complete, against the real isolated
OpenCode v2.1.4 host** (`scripts/dev_host.sh run`, disposable workspaces
`/tmp/opencode/e2e-ws{,2}`, binary `g4de89ea7`):

| Requirement | Result |
|---|---|
| durable ingestion + read-backed receipt | ✅ 10/10 envelopes delivered and stored (2 `session.observed` + 8 `tool.outcome`), 10 `event_ingest` audit rows, all success |
| duplicate delivery → `duplicate: true` | ✅ replay returned `{"duplicate": true, "ingested_at_ms": 1793111198891}` — the **original** stored timestamp; row count unchanged |
| invalid envelopes rejected | ✅ `invalid_envelope: event_id must be ev1_<64 lowercase hex> in strict mode`; nothing stored |
| correct event counters | ✅ `stored_total=10, stored_by_event_type={session.observed:2, tool.outcome:8}, stored_by_schema_version={mpm-event/0.1:10}, rejected_total=1, rejected_by_reason={invalid_envelope:1}` |
| no raw agent content in lifecycle storage | ✅ the canary `PROMPT_SECRET_e2e_canary_7f3a` appears only in `memories.content` (the agent's deliberate save); **0** occurrences in `lifecycle_events.meta` or any `tool_invocations` column |
| no writes to the live database | ✅ **the e2e's own traffic went to the disposable workspaces** (`/tmp/opencode/e2e-ws{,2}`) — measured: **0** `event_ingest` and **0** `event_counters` rows in the live DB, and each disposable workspace's own `health_check` row carries the e2e's timestamp (14:26:29, 14:27:40). **Correction, recorded 2026-10-27 ~17:05:** the live DB *does* now carry an **empty** `lifecycle_events` table. It was created at **14:59:59** by a Session A discovery command — `./.build/bin/mpm call mpm_memory --payload '{"action":"help"}'` — that was run **without** `MPM_WORKSPACE`/`MPM_DB_PATH` pointing away from the live workspace. The new binary's schema bootstrap ran against the live DB and created the table (one `tool_invocations` row with `result_status=error` came with it). It is **empty**, additive, forward-compatible (the release creates that table anyway), and the live runtime is healthy — the scheduler still ticks every minute with no error or warning. It was left in place: dropping a table from the live database would be a far more invasive write than the one it undoes. **Rule, now recorded as a lesson in the dogfood workspace: never invoke a repo-built `mpm` binary without `MPM_WORKSPACE` and `MPM_DB_PATH` set to a disposable path — not even for a one-line interface discovery.** (The `release_acceptance` suite is hermetic: across the window of a local `make release-gate` run, the live DB's only writes were the operator's own host — 97 `read_wake_context` rows, none from the suite.) |
| no false missed-opportunity detection | ✅ the critic ran 4 hunts and emitted **0 findings**. The adapter's ingest audit rows carry `framework_session_id = NULL`, so P3's candidate query sees `invocations=4, recording_calls=1, ingest_rows=0`. **Forward-looking risk, quantified in the ledger:** if the ingest transport ever carries `MPM_PROVENANCE_FRAMEWORK_SESSION_ID`, those rows join the candidate set and — being the oldest — become the cited evidence. |

**Deployment was not performed.** The live runtime is untouched: binary
`g3df72315`, checkout `3df7231`, MainPID 2804, `NRestarts=0`, and no
`lifecycle_events` table in the live database.

### P3b integration addendum — 2026-10-27 (Session A)

**#127 (P3b) was already merged** as `c733288` (head `dd8f416`, squash).
Re-verified against the merged tree rather than re-reviewed. The five named
concerns are each pinned by a test in
`internal/critic/p3b_trust_boundary_test.go` (15 tests):

| Concern | Pinned by |
|---|---|
| typed citation resolution | `..._SurvivalAsymmetry_CitationsResolveAsMemories`, `..._WeakTheory_CitationsResolveAsTheories`, `..._InvalidCitationRejectsTheProposalAtomically` |
| stable proposal identities | `..._RepeatingTheHuntDoesNotCreateAnotherProposal`, `..._ChangedObservationCreatesADistinctProposal`, `..._RatioBandIsStableUnderDrift`, `..._SurvivalProposalIsStableUnderObservationDrift`, `..._SurvivalCitationSetIsStableAsTheCohortGrows` |
| pending status in wake context | `..._PendingProposalIsMarkedPendingInWakeContext` |
| survival-cohort filtering correction | `..._SurvivalCohortIsScopedToTheMemoriesCollection` |
| no hunt on the trusted surface | `..._NoHuntTargetsTheTrustedLessonSurface`, `..._CriticFindingIsNotTrustedWithoutReview` |

**The isolated end-to-end critic evaluation** (disposable workspace, seeded so
every hunt fires; `/tmp/opencode/critic_e2e.sh`):

| Path | Emitted | Surface |
|---|---|---|
| `survival_asymmetry` | `mpm_theories propose` (cites 11 memories) | pending |
| `stale_memory` | `mpm_memory challenge` | pending (challenge theory) |
| `weak_theory` | `mpm_theories propose` (cites `th_weak_001`) | pending |
| `missed_opportunity` | `mpm_theories propose` (cites 6 tool_invocations) | pending |
| `poison_pill` (cycle 5) | `mpm_theories propose` (cites `mem_call_012`) | pending |

```
lessons_base rows             : 0        tool_invocations mpm_theories : 4
lessons_fts rows              : 0        pending theories                : 5
tool_invocations mpm_lessons  : 0        citations, all resolving        : 18
wake context: recent_theories → 5 entries, every one "status": "pending"
```

The control theory (`th_strong_001`, resolved, confidence 0.95) was neither
cited nor claimed.

**The telemetry-hunt ruling (#138, issue #139).** The last M5 trust-boundary
exception is **closed**: `cmd/mpm-critic --telemetry-hunt` is now a read-only
diagnostic. It reports the observation and writes nothing.

Three measured facts decided it:

1. **No resolvable evidence source.** P2's citation vocabulary resolves only
   inside `mpm.db`; the observation's own evidence (token counts) lives in
   `telemetry.db`, which no citation type can address. And the observation is
   about an **absence** — a session that recorded nothing — so there is nothing
   to cite. Citing anything else would be inventing a citation.
2. **The path was broken.** Its count called `mpm call mpm_provenance`, a tool
   that **does not exist** in the registry and never did. The hunt failed on
   every qualifying session and `mpm-critic --telemetry-hunt` exited **1** —
   which is what kept the trust exception latent. **The ordering rule this
   establishes: close the trust boundary FIRST.** Repairing the count against a
   substrate that still accepted a trusted-lesson write would *arm* the write.
3. **The previous tests could not have caught it.** They used a **fake `mpm`**
   that answered `mpm_provenance` with `{"count":0}`, so they asserted *which
   binary* was called and never *whether the call would work*.

The count is now computed in-process (`critic.SessionArtifactCount`, the same
attribution rule the missed-opportunity hunt applies). Non-vacuity of the new
regression is demonstrated: one `INSERT INTO lessons_base` back into the hunt
fails it.

**Outstanding trust blockers, exactly** (the table below is the state as of
#138; the current list, after #132, #135, #140, #141 and #143, is in
§"the trust-blocker table, restated" at the end of this section):

| # | Blocker | State |
|---|---|---|
| 1 | **D1 / D5** — evidence duplication and confidence inflation | **CLOSED** by `#130` (`3584190`), verified: the assertions were promoted into `internal/core/evidence_identity_test.go`, which runs in every green gate. **Not** a blocker. |
| 2 | **D3 ×2** — the legacy untyped citation path | **Accepted exception** under A19 (backward compatibility). Not a blocker for the release; it is a known limitation. |
| 3 | **D4** — the substrate identity hash | **Open.** B's. |
| 4 | **`mpm-telemetry observe`** — the same trusted-lesson exception, plus the same missing tool, plus a store-path disagreement with the spec (`src/db/telemetry.db` vs the spec'd `<workspace>/telemetry.db`) | **Open** — issue #139. Ruling: read-only until a resolvable evidence source exists; the store path must be settled first. |
| 5 | **Automatic critic scheduling** | **Still barred.** The P2b arming gate (`internal/core/wake_tools.go`: `critic_audit` is refused by `ScheduleWake`) and the dispatch gate (`internal/scheduler/scheduler.go`: `executeOne` refuses a blocked kind) are both intact and both tested. |

**No deployment.** The live runtime is untouched.

### The red release gate on the post-#137 head — 2026-10-27 (Session A)

Main run `40564010549` (head `9406936`, the #137 squash) was **red**: the
release gate failed in the OpenCode TypeScript suite, `TS: 46 passing tests`
against a floor of 15, with every lifecycle-events test failing on

```
The requested module './event-envelope.js' does not provide an export named 'ADAPTER_SOURCE'
```

**It is a data race, not a source regression.** The identical tree passed on
the previous main (`3abfdda`, run `40550339159`), and #137 changed nothing
under `agent_installation/mpm-opencode/src/` or `tests/`.

**The cause, measured.** Three test files each ran a full `tsc` build into the
SAME `dist/` directory — `test_lifecycle_events.test.ts` and
`test_wake_context.test.ts` via the local compiler, and
`test_resolve_mpm_binary.test.ts` via `npx tsc` — while `node --test` runs
test files in **parallel processes**. Two compilers writing one output
directory is a race, and `tsc` truncates an output file before rewriting it, so
a reader that catches a file mid-rewrite sees a **zero-byte file**, which is a
*valid* module with *no* exports. The parse succeeds; the missing export is
then reported by the importer — so the error names a TypeScript symbol and
says nothing about the race.

Measured during a single `tsc` run: `dist/event-envelope.js` observed at
**0, 16384, 20480 and 23039** bytes. The CI failure reproduced deterministically
by truncating the file to zero bytes and importing its importer:

```
SyntaxError: The requested module './event-envelope.js' does not provide an export named 'ADAPTER_SOURCE'
```

— byte-for-byte the CI message.

**The fix.** The gate builds the adapter payload once, before any test file
runs, using the same pinned local `tsc`; the tests only READ `dist/`. The
`before` hooks that built it are gone (all three).

**The non-vacuity is demonstrated, not asserted:**

| Mutation | Result |
|---|---|
| re-add a self-build to a test file | the pin fails, naming the offender |
| delete the payload build from the recipe | the pin fails |
| move the payload build after `node --test` | the pin fails |
| drop the empty-`dist/` FATAL | the pin fails |

**The behavioural proof:** the suite now passes 89/89 with `dist/` made
**read-only** — which was impossible while the tests built it.

A second, unrelated local-only failure was found and cleared: the retired
adapter directories (`mpm-claude-code`, `mpm-hermes`, `mpm-memory-openclaw`,
`mpm-pi`) survived #137's removal as **gitignored `__pycache__` and
`.pytest_cache` leftovers**, and their presence made
`test_agent_adapters_python_list_matches_the_adapters_on_disk` fail locally.
A clean CI checkout never saw them, so this was never a CI failure; the
leftovers are gone.

### Session B's remaining PRs, merged — 2026-10-27 (Session A)

Four of B's PRs were reviewed and merged on top of #143. Each was verified
against the CURRENT main (not its own stale base) before merging.

| PR | What it is | Merge | Session A's note |
|---|---|---|---|
| **#135** | **D7** — a Go-native `[]string` vanished at the parse boundary | `8ff4e7a` | A real trust hole: a citation list that silently lost elements meant *a proposal resting on less than its author declared, and the author was told it succeeded*. Now a citation list is accepted **whole or not at all**. |
| **#132** | **D6** — the invocation identity was carried on 1 of 6 recording paths | `3de18a4` | Repairs the substrate defect that forced the critic's missed-opportunity hunt to use two signals. The trust surface is untouched: the diff reaches neither `epistemic_provenance` nor `lessons_base`. |
| **#141** | **D3-b** — a legacy citation recorded the wrong type | `434e47e` | A work item or a tool invocation cited without a declared type was recorded as `source_type="memory"` — a **false record**. Now it records what it actually cites. |
| **#140** | **D4**'s production-path proof + the independent M5 release assessment | `a4e3da9` | Test-only plus documentation. It names two residuals that were not on A's list before: **F2** and **D2-core**. |

**Session A's ruling on D3-a** (the half of D3 that remains open), adjudicating
Session B's three-step retirement proposal:

* **Step 1 — APPROVED as the next change.** When `source_type` is omitted and
  every cited id resolves unambiguously to exactly one canonical type, the
  substrate uses that resolved type, so the citation is validated without the
  caller changing anything. The decisive property: it **cannot reject
  anything that is accepted today**, because the type is derived FROM the
  resolution. An unresolvable id still falls back, so both the shredding case
  and the never-existed case keep working. It tightens nothing.
* **Step 2 — APPROVED IN PRINCIPLE, as its own shared-interface PR.**
  `mpm_decisions record` / `supersede` have **no `source_type` parameter at
  all**, so a decision's citations can never be typed and therefore can never
  be validated. Adding the parameter mirrors `mpm_theories.propose` exactly.
  It touches the shared registry, so it follows the shared-interface change
  procedure rather than riding along here.
* **Step 3 — DEFERRED, not approved.** Rejecting a legacy citation of an id
  that never existed turns today's successes into errors, and the substrate
  cannot currently tell a shredded id from a never-existing one. If it is ever
  approved it needs the sunset: one release of deprecation notice, then
  rejection, then removal of the path with `source_type` required.

**Session A's position on the P2b gate.** Session B's assessment argues the
gate's removal condition is met. **The gate stays CLOSED.** Beyond the
standing instruction, the residuals give a reason: **F2** means the critic can
*silently* lose a finding under contention (its emission failure is logged and
the cycle continues), and **D2-core** means the lesson surface still publishes
trusted knowledge directly. The gate can be re-considered when F2 is fixed and
D2-core's pending state exists.

### The trust-blocker table, restated — 2026-10-27 (after #132, #135, #140, #141, #143)

| # | Item | State |
|---|---|---|
| 1 | **D1 / D5** — evidence duplication and confidence inflation | **CLOSED** by `#130`; the assertions live in `internal/core/evidence_identity_test.go`, which runs in every green gate. **Not** a blocker. |
| 2 | **D2** — the critic created trusted lessons with no review | **CLOSED** for every critic path: P3b moved the last two lesson-emitting hunts to pending proposals, and `#138` made the opt-in telemetry hunt read-only. |
| 3 | **D2-core** — the lesson surface itself has no pending state | **OPEN.** A property of the SURFACE, not of any writer: `lessons_base` has no status column, so a row there is trusted on arrival. Implementing a pending state is a **schema change** (A's authority). Must be written into the release notes: treat the lesson surface as operator-only until it exists. |
| 4 | **D3-b** — a citation recorded the wrong type | **CLOSED** by `#141`. |
| 5 | **D3-a** — the legacy citation path accepts an unresolvable id | **OPEN, accepted as a documented limitation.** Ruling above: step 1 approved as the next change, step 2 approved as a separate shared-interface PR, step 3 deferred behind a deprecation period. Not blocking safe manual or automatic critic use — the critic uses the typed path on every hunt. |
| 6 | **D4** — an incidental tag difference still splits a proposal | **CLOSED in production** (the critic's emission fix), with a substrate residual open. The `m5contract` D4 test stays red until A rules on whether an incidental difference should split a proposal. Not blocking. The extra finding **F1** — the identity treats the tag SET as a sequence, so `["critic","alpha"]` and `["alpha","critic"]` are two proposals — is cosmetic and rides with the same decision. |
| 7 | **F2** — concurrent proposals are lost to `SQLITE_BUSY` | **OPEN. The most consequential residual.** Measured: 2–4 of 8 concurrent proposals lost; reproduced under `make test-race` (~1 run in 6). The critic's emission failure is *logged and the cycle continues*, so under contention it can **silently lose a finding**. Candidate fix validated: `BEGIN IMMEDIATE` (measured 4/8 lost → 0/8). It deserves its own PR, **before** the P2b gate is lifted. |
| 8 | **`mpm-telemetry observe`** — the same trusted-lesson exception, plus the same missing tool, plus a store-path disagreement with the spec (`src/db/telemetry.db` vs the spec'd `<workspace>/telemetry.db`) | **OPEN** — issue #139. The command is currently **inert** (it calls a tool that is not in the registry, then calls `mpm_lessons` with a flat payload the params-envelope check rejects), so it cannot write — but it is an advertised command, and repairing its count the way #138 repaired the critic's would **arm** a trusted-lesson write. Ruling: read-only until a resolvable evidence source exists; the store path must be settled first. |
| 9 | **Automatic critic scheduling** | **Still barred.** The P2b arming gate (`internal/core/wake_tools.go`: `critic_audit` is refused by `ScheduleWake`) and the dispatch gate (`internal/scheduler/scheduler.go`: `executeOne` refuses a blocked kind) are both intact and both tested. Re-consider when #3 and #7 are closed. |

### The release-candidate acceptance — 2026-10-27 (Session A)

**Integrated main SHA: `4111c46dbe30de44e9ed8f7a715fff9e8c3e469e`** ("fix(opencode):
the lifecycle observer's delivery accounting must close (P4)"), the squash of
**#142**. #142's branch predated #143, so the import-block conflict in
`tests/test_lifecycle_events.test.ts` was resolved by keeping **both** halves:
#143's rule (the `before` hook does **not** build `dist/`; the gate builds the
payload once, before any test file runs) and #142's `mkdtempSync`/`rmSync`/
`tmpdir` additions (per-test **unique** temp directories). `spawnSync` was
**not** reintroduced — it was used only by the self-build, which no longer
exists. The pin that keeps this honest is
`scripts/tests/test_makefile_typescript_test_wiring.py` (18 tests): no
`*.test.ts` may invoke a TypeScript compiler.

The full report is `docs/dev/release-candidate-2026-10-30.md`. The four
acceptance phases, all against disposable workspaces, with the live tree
proved untouched by **three** independent detectors (a snapshot, a `strace -f`
of the whole process tree, and a systemd shim that records the requests it
refuses to act on):

| Phase | Harness | Result |
|---|---|---|
| 2 — the reproducible release candidate | `/tmp/opencode/rc/01_install.sh` | **21 / 21** |
| 3 — the cross-session learning acceptance | `/tmp/opencode/rc/02_sessions.sh` | **27 / 27** |
| 4 — the critic acceptance | `/tmp/opencode/rc/03_critic.sh` | **36 / 36** + **5 / 5** (the P2b gates) |
| 5 — the failure injection | `/tmp/opencode/rc/04_failure.sh` | **38 / 38** |

**Three measurements that change how a future session must assert.**

1. **The critic's no-duplicates contract is PER OBSERVATION, not per total.**
   The pending-theory count over five cycles was **6 → 11 → 15 → 15 → 15**: it
   climbs and then **converges**, because the weak-theory hunt legitimately
   observes the critic's **own** new proposals (each is a *different*
   observation). A total-count duplicate assertion fails for the right
   behaviour. The right assertions are per-observation (exactly one proposal
   cites `th_weak_001`, `ti_quiet_0`, `mem_direct_001` — each stayed at 1
   across every cycle) plus convergence (identical across the last two
   evaluations) plus boundedness. **A measurement that says "duplicates are
   possible" was measuring the wrong thing.**
2. **A critic finding cannot corroborate what it criticises**, because the
   evidence source group **`auto_capture` is excluded from confidence
   scoring** (`scoringEvidenceClause`, `internal/core/evidence_store.go`). A
   theory cited at confidence 0.2 stayed at exactly 0.2 after five critic runs.
3. **A plugin that refuses to boot does not write to the host's stderr.**
   OpenCode captures a plugin's `console.warn` in **its own** log, at
   `$XDG_DATA_HOME/opencode/log/opencode.log`, as `failed to load plugin …
   cause="…db_path invariant violated"`. An acceptance assertion about a
   boot refusal must read the host log.

**The non-vacuity control, and the design mistake the first run exposed.**
The first control pointed at an *empty* workspace — and the control agent
**routed around it**, found the seeded workspace on disk and retrieved the
fact with the MPM CLI. It invented nothing; it simply had another route to
the data. A control the agent can route around proves nothing. The corrected
control runs the Session 2 prompt in a **bubblewrap mount namespace** with the
seeded root masked by a `--tmpfs`, and the witness probe was moved **outside**
the masked root — otherwise the control observes nothing and becomes vacuous
in the other direction. Measured: the control's system prompt was observed (82
parts), the marker is absent from it, and the control could not answer.

**#142 was merged; #134 is closed.** The P4 delivery-counter discrepancy was
a **diagnostic** defect, not a data-loss defect, and #142 closes it: the
counters are split into **cumulative** and **instantaneous**, and the
accounting closes at every reading —
`enqueued = delivered + failed + dropped_overflow + abandoned + queued + in_flight`.
Measured on a real store: the teardown reading is `enqueued=10 delivered=9
in_flight=1` (the tenth delivery was **outstanding**, not lost) and the final
reading is `delivered=10 balanced=true`, with all 10 stored in the core. The
ambiguity between "outstanding" and "lost" — the whole content of #134 — is
gone.

**#126 is closed as approved.** The legacy-adapter-removal proposal's five
open questions are answered by #137's implementation record
(`docs/dev/opencode-only-adapter-removal-implementation.md`): the telemetry
adapter was **relocated** to `agent_installation/lib/telemetry_adapter.py`; the
`created_by` rename is **deferred as a data migration**; the OpenClaw
refresh-safety guarantee was **retargeted** via #133; the JS gate keeps an
**explicit empty state with no floor** (a `0` floor is a silent pass wearing a
number's clothing); and every floor was **measured after** the removal.

**The one open product decision — the lifecycle transport is OFF by default.**
With `MPM_EVENT_INGEST` unset — the state of every installation, because
nothing in the repository, the installer, the runtime package or the SPEC sets
it — the observer builds the envelopes and drops every one
(`dropped_unconfigured == envelopes_built`, `delivered == 0`), so
`lifecycle_events` stays empty; with `MPM_EVENT_INGEST=mpm_system` the same
run stores 4–10 rows. **It is not a defect and it is not A's to close
unilaterally**: the adapter's own harness *pins* the default as a contract,
and enabling ingestion by default changes what a default installation
**writes**. It is also **not a trust risk**: `event_ingest` writes only
`lifecycle_events` plus an audit row, creates no memory/theory/decision/
lesson, and cannot arm the critic. Three options were put to the owner on
#142 — (1) the adapter installer sets the transport, (2) flip the default and
update the pinned expectation in `measure/p4_lifecycle_e2e.sh`, (3) document
it as an operator step. **Option 3's operator step was applied for the
acceptance run**, and the full cross-session acceptance passes on that path.

### The trust-blocker table after the release-candidate acceptance — 2026-10-27 (after #142)

| # | Item | State | Blocks the RC? |
|---|---|---|---|
| 1 | **D1 / D5** | **CLOSED** by `#130`. | No |
| 2 | **D2** | **CLOSED** for every critic path. | No |
| 3 | **D2-core** — the lesson surface has no pending state | **OPEN.** A schema change (A's authority). **Must be in the release notes: the lesson surface is operator-only.** | No |
| 4 | **D3-b** | **CLOSED** by `#141`. | No |
| 5 | **D3-a** | **OPEN**, accepted as a documented limitation; the ruling stands (step 1 approved as the next change, step 2 as a separate shared-interface PR, step 3 deferred). | No |
| 6 | **D4** / **F1** | **CLOSED in production**; the tag-ordering residual is cosmetic. | No |
| 7 | **F2** — the critic can silently lose a finding under contention | **OPEN.** The most consequential residual. The candidate fix is validated (`BEGIN IMMEDIATE`: 4/8 lost → 0/8). Its own PR, **before** the P2b gate is lifted. | No for the RC; **yes** for lifting the gate |
| 8 | **`mpm-telemetry observe`** | **OPEN** — issue #139. Inert, so it cannot write. Read-only until a resolvable evidence source exists; the store path must be settled first. **Repairing its count would ARM a trusted-lesson write.** | No |
| 9 | **Automatic critic scheduling** | **Still barred.** Both gates intact and now **exercised end to end**: the producer refused to arm a `critic_audit` wake, and the **consumer** refused to execute one that was seeded directly by SQL — recorded as processed-and-refused, handler never invoked. | n/a |
| 10 | **The lifecycle transport's default** | **OPEN product decision** (§ above). Three options with the owner. Not a defect; not a trust risk. | No, with a documented operator step |

**The recommendation for Friday 30 October is CONDITIONAL GO.** The two
conditions are documentation: the release notes must state the
`MPM_EVENT_INGEST=mpm_system` operator step, and must state that the lesson
surface is operator-only until a pending state exists.

### Issue-shaped note — the runtime migration's shared-interface changes (2026-10-28, Session A)

> §4 step 1: the proposing session opens an issue-shaped note in the ledger
> stating the interface, the proposed change, the consumers affected, the
> compatibility story, and the contract tests that must change.

**The interface:** `internal/core/config/` — the substrate's shared
configuration and path-resolution package. It is Session A's to change
(§2), and it is consumed by every binary and by `internal/core` itself.

**The proposed changes**, all landed on `feat/runtime-migration-design`:

| Change | Interface | Consumers affected |
|---|---|---|
| `config.ValidateConfig()` — a new exported function | additive | the CLI and the MCP server's startup gates |
| `config.ResolveTarget(explicitDB)` / `config.ResolveDBPath(workspace)` / `config.CanonicalDBPath(workspace)` / `config.HasCanonicalDBShape(p)` / `config.WorkspaceRootForDBPath(p)` / `config.IsDBPathConflict(err)` / `config.DBPathConflictError` | additive | all four binaries; `internal/scheduler` |
| `config.DefaultMemoryPaths()` — no longer dereferences a nil config | behavioural, not signature | `internal/core`'s memory store |

**The compatibility story:** every addition is **additive** — no
existing signature changed and no existing behaviour was removed. The
one behavioural change is the **refusal** of a divergent database
selector, and it is a refusal that used to be a silent redirection:

- `MPM_DB_PATH` naming a database other than the workspace's own was
  **silently ignored** by the CLI and the MCP server, and **silently
  rewritten** into a different database by the scheduler and the critic.
- It is now an explicit conflict error everywhere.

The environment the live installation actually runs under is
**consistent** (`MPM_WORKSPACE=/home/v/.mpm`,
`MPM_DB_PATH=/home/v/.mpm/src/db/mpm.db`), so the live scheduler and
critic are unaffected. The refusal only fires on a divergence that was
previously invisible.

**The contract tests that changed:**

| Test | Package |
|---|---|
| `internal/core/config/config_malformed_test.go` (new) | `mpm-core/config` |
| `internal/core/config/dbpath_test.go` (new) | `mpm-core/config` |
| `internal/core/memory_malformed_config_test.go` (new) | `mpm-core` |
| `cmd/mpm/db_path_resolution_test.go` (new) | `mpm` |
| `cmd/mpm-scheduler/db_path_resolution_test.go` (new) | `mpm-scheduler` |
| `cmd/mpm-critic/db_path_resolution_test.go` (new) | `mpm-critic` |
| `internal/scheduler/critic_handler_env_test.go` (new) | `mpm/scheduler` |
| `internal/scheduler/mpm_bin_test.go` — `TestDefaultDBPath_ExplicitEnvWins` → `TestDefaultDBPath_EnvOverrideIsNotForwardedRaw` | `mpm/scheduler` |
| `internal/core/test_db_safety_test.go` — one whitelist entry, with its justification | `mpm-core` |
| `release_acceptance/gitfree_build_identity_test.go` (new) | `release_acceptance` |
| `scripts/tests/test_build_provenance.py` — the producer assertions | scripts |

**Who must rebase:** nobody. No session depends on the changed
behaviour, and `internal/core/config/` is A's. Session B's
`feat/session-b-139-telemetry-observe` and Session C's
`feat/lifecycle-subscriber-delivery-accounting` are untouched by it —
**with one exception to check**: Session B's critic tests set
`MPM_DB_PATH` to the canonical path of their disposable workspace, which
the new rule accepts. Session C's adapter does not read `MPM_DB_PATH`.

**The design and its evidence:**
[`runtime-migration-plan.md`](runtime-migration-plan.md) and
[`runtime-migration-evidence.md`](runtime-migration-evidence.md).

**Note for §3 (branch ownership):** §3 still names `dev/session-b-core`,
which no longer exists — the worktrees sit on feature branches. That
drift is pre-existing and is a separate documentation PR, not a change
this note makes.


### Issue-shaped note — the rehearsal's live-installation isolation gate (2026-10-30, Session B)

> §4 step 1: the proposing session opens an issue-shaped note in the ledger
> stating the interface, the proposed change, the consumers affected, the
> compatibility story, and the contract tests that must change.

**Reported by:** Session A, from the 30 October upgrade rehearsal.

**The defect.** The rehearsal asserted that the live installation was
untouched by hashing `(path, size, mtime)` over a list of live paths,
**including the live database file**. That assertion was invalid: the
live scheduler records health checks and SQLite checkpoints the WAL, so
the live database's size and mtime move on their own. Measured, with the
production scheduler active and nothing else running — the file grew by
**86 KB** over four minutes while the operator's knowledge did not change
by one bit, and the old rule taken twice ninety seconds apart reported
`CHANGED` on that untouched tree.

**The proposed change** — `scripts/rehearsal/live_gate.py` (new) and the
rehearsal's two suites. The gate becomes **classified** instead of one
hash:

| Class | What it is | The rule |
|---|---|---|
| **PROTECTED** | the installed binaries, the host assets, the systemd unit, `opencode.jsonc`, `AGENTS.md` | **byte identity** — the path set, the mode, the sha256. The *mtime* is not part of it: an mtime records when a file was written, not what it contains |
| **DOMAIN** | the operator's knowledge inside the live database — `memories`, `lessons`, `evidence`, volatile columns excluded | **must not change** — the substrate's own definition (`state_fingerprint.py --content`) |
| **OPERATIONAL** | `tool_invocations`, `system_config` — the two tables the live scheduler actually moves | **may change**; the movement is **reported with the delta** |

**The consumers affected:** the rehearsal harness only. The live
installation, its configuration, its database and its scheduler are
**not** touched — the live database is opened read-only, through
`file:…?mode=ro`.

**The compatibility story:** the rehearsal's assertion count is unchanged
(**135**). The rehearsal's other two detectors — the `strace` of the
whole process tree, and the systemd path shim — are **untouched**; the
gate replaces the *file-state* detector and does not weaken the causal
one. The installation and rollback assertions are unchanged. The
negative suite's BEFORE snapshot now spans the **whole suite** rather
than two adjacent instants. The gate **fails closed**: an unreadable
live database means *could not measure* (exit 2), not *nothing changed*.

**The contract tests that changed:** `scripts/tests/test_rehearsal_live_gate.py`
(new, **22 tests**) — the negative control (a modified configuration, a
modified managed instruction, an executable changed / added / removed, a
host asset, the systemd unit, **a memory written**, a modified lesson, a
new table in the live database), the ordinary-activity cases (the audit
trail growing, the liveness counter being rewritten, a read bumping the
volatile columns, the database file growing), the fail-closed cases, and
the **A/B non-vacuity**: the old rule, reproduced verbatim, sees a change
where the gate passes — and a same-size, mtime-restored content change is
invisible to the old rule and caught by the gate.

**Who must rebase:** nobody. No session depends on the changed behaviour.

**What Session A must carry into the final combined-candidate
verification.** The corrected gate is the **file-state detector** and it
must be in the candidate. It is the detector that fails when the
production scheduler is busy — which is precisely the condition under
which the combined verification runs.

**One row of A's own record rests on the invalid method.**
`docs/dev/release-candidate-2026-10-30.md` row 10 reads *"The live tree
snapshot is unchanged | PASS"*, and its "three independent detectors"
section describes that snapshot as including **the live database**. That
check passed on the day it was run — the scheduler happened not to write
inside the window — but the **method could not support the claim**: it
would have failed on a tree the rehearsal never touched, and it could
not see a content change that preserved a file's size. **The correction
replaces the method; it does not contradict the result.** A's record is
**not edited** by Session B — it is A's document, and its other two
detectors (the `strace`, 0 hits; the systemd shim) are independent of
the file-state rule and remain exactly as recorded.

| Branch | PR | Session | Status |
|---|---|---|---|
| `fix/rehearsal-live-isolation-gate` | [#190](https://github.com/flowbyte-com/mpm/pull/190) | B | 🟡 open — awaiting A's merge; **the corrected gate must be in the combined candidate** |

Full record: [`session-b-rehearsal-live-isolation-gate.md`](session-b-rehearsal-live-isolation-gate.md).
The rehearsal's own documentation: [`scripts/rehearsal/README.md`](../../scripts/rehearsal/README.md).


## 6. Cross-session communication protocol

1. **The ledger is the channel of record.** A session updates its own rows in
   `docs/dev/coordination.md` in the same PR as its change. A dependency or a
   blocker is recorded there, not only in chat.
2. **MPM is the reasoning channel.** Decisions, lessons and handoffs go into
   the shared dogfooding workspace so the next session inherits them. The
   ledger carries *state*; MPM carries *why*.
3. **Every integration report names exact SHAs** — the commit that was
   tested, not the branch name. "CI was green" without a SHA is not
   evidence.
4. **CI results are tied to the commit they were run on.** If the head moves
   after a green run, the run is stale.
5. **Blockers are stated, not worked around.** If a session cannot proceed
   because of another session's interface, it records the dependency and
   moves to independent work — it does not edit the other session's worktree.

## 7. Release policy

- Merge to `main` only by Session A, only after the matrix's gate for that
  stage has passed on the PR's actual head.
- Deploy to `~/.mpm` only by Session A, only at a **milestone** (not every
  commit), and only through the procedure in `dogfooding-runbook.md` §6.
- **Experimental work is never deployed to the shared runtime to simplify
  testing.** The dogfooding workspace is disposable; the live database is
  not a test bench.
- A deployment records the deployed SHA, the binary provenance, and the
  database backup location.
