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
| **P2** | `feat(theory): typed, validated, idempotent evidence citations on proposals` — optional `source_type` beside `source_ids`, validated before insert; plus the one-line `artifact_type='skill'` registry correction (A-9) | **B** | No | **No** | Yes (one optional parameter) | 🔄 PR #120 open |
| **P2b** | **gate the `critic_audit` wake kind at the arming AND dispatch boundaries** until the trust boundary is enforced | **A** | No | No | No | ✅ **merged** (#118) |
| **P3** | `feat(critic): a retrospective missed-opportunity hunt` over the existing `tool_invocations` × `artifact_provenance` join | **B** | No | No | No |
| **P4** | `feat(opencode): observe lifecycle events` — subscribe, consume off the hot path, map an explicit allowlist, emit `session.observed` / `session.started` (when actually seen) / `tool.outcome` / `compaction.*` | **C** | Yes (M2, now agreed) | No | No | 🔄 PR #119 open |
| **P5** | The ingest entry point, receipt, `mpm_system action=event_counters`, and the storage decision | **A** | Yes | only if §6.1 later authorises it | Yes (one action) |

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
  **Automated critic scheduling is not enabled** while the duplicate-
  evidence, proposal-idempotency and trust-promotion risks are unresolved.
  The critic's automatic path is **wired but not armed** — the live
  workspace has no `critic_audit` wake and the critic has written no trusted
  lesson — but the wake `kind` is caller-supplied and unvalidated, so it is
  one API call from firing. P2b closes it.
* **P3's proposals may not cite a gap.** An invocation with no corresponding
  artifact is a *candidate opportunity*, never a citation and never proof
  that a lesson exists (contract §3.7, amendment A14).
* **P3 and P4 are complementary, not substitutes.** P3 covers MPM-visible
  activity; P4 extends detection to sessions that never invoke MPM. Neither
  makes the other redundant.
* **The lifecycle-events table is NOT the first milestone.** B's audit shows
  the first end-to-end acceptance test needs no new table: `epistemic_provenance`
  already provides typed, validated and idempotent citations, and the
  retrospective join runs over existing tables. No `SafeMigrations` entry is
  authorised by this adjudication.
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
