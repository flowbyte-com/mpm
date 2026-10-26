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
| **Event contract** (new, `event-contract.md`) | Session A drafts; **B and C must both acknowledge a version** | C emits, B ingests | §4 below |
| **Schema migrations** — `internal/core/schema.go` `SafeMigrations` / `CommonIndexes` | Session A | B and C may both add columns | Every migration entry lands in one PR, reviewed together. Two sessions adding migrations concurrently is the highest-risk collision in the sprint. |
| **Workspace / path resolution** — `internal/core/config/` | Session A | everything | Changing it changes where the live DB lives. Session A only. |
| **OpenCode package manifest** — `package.json` + lockfile | Session C | CI, installer | The lockfile is pinned by `scripts/tests/test_release_gate_bootstrap_wiring.py`; a dependency change must keep that contract test green. |
| **`docs/SPEC.md`** | Session A arbitrates | all | Five PRs touch it this round. Additive edits only; a **semantic** change needs A's review. |

## 3. Branch and worktree ownership

| Worktree | Branch | Owner | Rule |
|---|---|---|---|
| `~/src/mpm` | `integration/session-a` | **A** | integration base; A merges here first |
| `~/src/mpm-core` | `dev/session-b-core` | **B** | B's feature branches branch from here |
| `~/src/mpm-opencode` | `dev/session-c-opencode` | **C** | C's feature branches branch from here |

Per-session feature branches:

```
Session A:  integration/session-a, docs/sprint-foundation
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

| Branch | PR | HEAD | Session | Status |
|---|---|---|---|---|
| `fix/hal9001-r3-install-next-steps` | #99 | `0f67fcbe` | A | ✅ CI green |
| `fix/hal9001-r2-save-evidence` | #96 | `49c8e6f3` | A | ✅ CI green (gofmt fix pushed) |
| `fix/hal9001-mpmcli-env-isolation` | #102 | `af8a699e` | A | ✅ green |
| `fix/hal9001-signal-test-determinism` | #103 | `5c687d00` | A | re-running |
| `fix/hal9001-ci-gate-integrity` | #104 | `4c0bfb0e` | A | stacked on #103 |
| `fix/hal9001-shred-agent-auth` | #85 | `7ae59580` | A | open |
| `fix/hal9001-confidence-provenance` | #86 | `68a8b22c` | A | open |
| `fix/hal9001-embedding-migration-diagnostics` | #87 | `da25efeb` | A | open |
| `fix/hal9001-raw-memory-shred-linkage` | #88 | `e0a2af94` | A | open |
| `fix/hal9001-provenance-test-isolation` | #89 | `ed9ad9de` | A | open |
| `fix/hal9001-gc-reporting-honesty` | #90 | `0a6b218c` | A | open |
| `fix/hal9001-ingest-undo-honesty` | #91 | `b6bffa8e` | A | open |
| `fix/hal9001-r2-confidence-decay` | #95 | `ac523931` | A | open |
| `fix/hal9001-r3-directive-retention` | #97 | `583d325f` | A | open |
| `fix/hal9001-r3-schema-init-concurrency` | #98 | `4fe840d6b` | A | open |
| `integration/session-a` | — | `414b276` | A | Session A integration base |
| `dev/session-b-core` | — | `414b276` | B | Session B base |
| `dev/session-c-opencode` | — | `414b276` | C | Session C base |
| `docs/sprint-foundation` | — | — | A | this documentation set |
| `hal9001/r3-integration` | — | `cbe6af7` | A | pre-existing combined tree |
| `integration/pre-release-12` | — | `a8c3b60` | A | **validated checkpoint — not a baseline** |

### Cross-session dependencies

| From | To | Dependency | Status |
|---|---|---|---|
| C | B | C emits lifecycle events; B ingests and validates them | ⛔ **blocked on an agreed event contract** |
| C | A | C's adapter changes must keep `test_makefile_agent_adapter_wiring.py` and `test_release_gate_bootstrap_wiring.py` green | open |
| B | A | B's schema migrations land in `internal/core/schema.go` — one PR, reviewed together | open |
| B | C | B may consume the adapter's raw event stream; the stream is *evidence*, not trusted knowledge | open |
| A | B, C | A merges; B and C submit PRs only | standing |

### Integration status

| Item | Verified SHA | Verified by | Gate |
|---|---|---|---|
| 12-way corrective integration | `a8c3b60` (`integration/pre-release-12`) | `make build` + `make test-race` (24/24 packages, 0 data races) | **must be rebuilt from merged heads** |
| pre-existing combined tree (#85–#98) | `cbe6af7` (`hal9001/r3-integration`) | pre-existing; **not re-verified this round** | superseded by the rebuild |
| `origin/main` | `414b276` | CI green | — |

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
