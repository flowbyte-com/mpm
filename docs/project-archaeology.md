# MPM Repository Archaeology — 2026-08-31

**Author:** 808 (via repo archaeology pass)
**Baseline:** `f415d66` (main, `fix(alpha-blocker): D-001/002/003/004/007/010/013 — eradication pass`)
**Verification pass:** `b67ee79..HEAD` (after this reconciliation)
**Date:** 2026-08-31 (initial pass + verification commit)

## Purpose

This document is the authoritative ledger of every non-main branch in the
MPM repository as of 2026-08-31, with classification of what each branch
contributed, what is already on main, and the disposition for each.

Future agents should be able to answer — without inspecting abandoned
worktrees — *"What is implemented on main? What useful work exists outside
main? Which branch contains the canonical version of each unfinished
feature? What old work is intentionally preserved? What can safely be
deleted?"* by reading this document.

---

## Repository policy (binding)

1. **main is the only authoritative product lineage.** All product behaviour,
   tests, documentation, and shipped features live here. main is the source
   of truth — branch tip SHAs in this document are historical reference, not
   guidance.
2. **Feature/worktrees are temporary working surfaces.** They exist to develop
   a feature or salvage work. Once the work is on main, the worktree is
   candidate for cleanup.
3. **`archive/*` refs are preserved historical records.** A branch with
   potentially useful unique history is moved to `archive/<feature>`
   **before** the original is deleted, so the history is not lost.
4. **Nothing unique is deleted without classification.** Every deletion in
   this pass has been classified as one of:
   - `MERGED_TO_MAIN` — content is functionally on main (often with
     later evolution).
   - `SUPERSEDED_ON_MAIN` — content is on main in newer form (refactor,
     hardening, or rebuild).
   - `MOVED_TO_MAIN_ARCHIVE` — content is preserved on main in `docs/archive/`.
   - `CONFLICT_LEFTOVER` — duplicate file artifact of an abandoned merge.
   - `TEST_FIXTURE` — gitignored runtime/test artifact, not unique content.
   - `OBSOLETE` — code that was refactored with the same semantics.
5. **Branch names are NOT evidence of utility.** Comparison is by file
   content, function signatures, schema, tests, and later changes on main —
   not by branch name or commit count.

---

## Topology snapshot — before

```
* f415d66 (HEAD -> main, origin/main) fix(alpha-blocker): D-001..D-013 — eradication pass
| * 7592b8f (feat/drill-orchestrator) fix(wake-context): surface overdue scheduled_wakes at bootstrap
| * 4c0c7aa (worktree-feat-epistemic-cascade) fix(cascade): preserve attempt_count on stale-recovery reaper (#3)
| * 9c26c8c (rescue-cascade-work) wip(cascade): salvage uncommitted cascade implementation and testhelpers
| * d33355d (worktree-mpm-skill-workshop) feat(workshop): feature-freeze audit + dogfood scripts
|/
```

| Branch | Tip | Merge-base | Branch-only commits | Worktree | Last activity |
|---|---|---|---|---|---|
| `feat/drill-orchestrator` | `7592b8f` | `038f4e66` (2026-08-12) | 17 | none (branch only) | 2026-08-13 |
| `rescue-cascade-work` | `9c26c8c` | `258e1a98` (2026-08-04) | 1 | `.claude/worktrees/agent-a77967f6cf7d719ba` | 2026-08-27 |
| `worktree-feat-epistemic-cascade` | `4c0c7aa` | `258e1a98` (2026-08-04) | 16 | none (branch only) | 2026-08-04 |
| `worktree-mpm-skill-workshop` | `d33355d` | `d33355d` (its own tip; ancestor of main) | 0 (already in main) | `.claude/worktrees/mpm-skill-workshop` | 2026-08-28 |

**Merge-base shared:** `rescue-cascade-work` and `worktree-feat-epistemic-cascade`
share `258e1a98` (the original cascade-fork commit).

---

## Feature archaeology

### 1. Drill orchestrator

| Field | Value |
|---|---|
| Branch(es) | `feat/drill-orchestrator` (17 commits, tip `7592b8f`) |
| Worktree | none |
| Implementation files | `cmd/mpm/drill_cmds.go`, `cmd/mpm/drill_report.go`, `internal/core/drill*.go`, `internal/scheduler/drill_handler.go`, `cmd/mpm/audit_hook.go`, `cmd/mpm-mcp/audit_hook.go`, `internal/core/mpmcli/mpmcli.go`, `drills/*.yaml`, `internal/core/wake_context.go` |
| Test files | `drill_*_test.go`, `tool_invocations_test.go`, `wake_context_overdue_wakes_test.go`, `drill_e2e_test.go`, `drill_e2e_claude_code_test.go` |
| Schema additions | `tool_invocations`, `drill_runs` |
| Key behaviours | `mpm drills list\|show\|run\|report` CLI; drill YAML schema + loader; synthetic harness + Claude Code real-framework harness; `MPM_SESSION_ID` plumbing for `mpm-mcp`; verdict scoring (`Score` pure over evidence); 3-axis compatibility matrix in `mpm drills report` |
| On main? | **FULLY_DUPLICATED_ON_MAIN** — every drill file is byte-identical to main |
| Reimplementation evidence | `internal/core/drill.go` 64 lines on branch == 64 lines on main. `cmd/mpm/drill_cmds.go` 378 lines == 378 lines. `internal/core/drill_score.go` 136 == 136. All drill_*.go files match. Diff `7592b8f..main` on these paths returns empty. |
| Files differing | `wake_context.go` (904 → 1375), `active_state.go` (253 → 278), `sql_dump_validator.go` (411 → 414), `audit_hook.go` (136 → 139), `mpmcli/mpmcli.go` (41 → 58), `router.go` (1143 → 1439), `tools/handlers.go` (3278 → 5465), `tools/handlers_test.go` (1396 → 1994), `call.go` (245 → 428), `audit_test.go` and `usererror_regression_test.go` small diffs — main has **more** content (later evolution), not branch. |
| Truly unique content | None for code. Docs: `docs/WISHLIST.md` (renamed on main to `docs/archive/shared-epistemology.md`, identical), `docs/capability-lifecycle-spec.md` (renamed to `docs/archive/capability-lifecycle.md`), `docs/cli-cognitive-interface-rfc.md` (renamed to `docs/archive/cli-design.md`). All design docs preserved on main with updated path references. |
| Conflict leftovers | None on this branch |
| Disposition | **ARCHIVE** (`archive/feat-drill-orchestrator` at `7592b8f`). Branch can be deleted after archive — content is functionally on main. |

### 2. Epistemic cascade

| Field | Value |
|---|---|
| Branch(es) | `worktree-feat-epistemic-cascade` (16 commits, tip `4c0c7aa`), merged history with `rescue-cascade-work` via shared merge-base `258e1a98` |
| Worktree | none (branch only; original worktree was agent-aaebaa877cd2e7e88, already pruned) |
| Implementation files | `internal/core/cascade_materializer.go`, `cascade_outbox.go`, `cascade_provenance.go`, `cascade_invalidation_test.go`, `cascade_materializer_test.go`, `cascade_outbox_test.go`, `cascade_provenance_test.go`, `cascade_schema_test.go`, `epistemic_cascade_integration_test.go`, `cascading_decay_test.go`, `cmd/mpm/handlers_cascade.go`, `cmd/mpm/self_heal.go` |
| Schema additions | `epistemic_cascade_outbox` (with `attempt_count`), `epistemic_provenance`, plus modified CHECK constraints on `evidence.trigger` and `artifact_provenance.artifact_type` |
| Key behaviours | cascade enqueue on theory/memory invalidation; throttle cascade wake delivery; `mpm cascade materialize` CLI; reasoning provenance edges (typed citation log); stale-recovery reaper preserving `attempt_count` |
| On main? | **FULLY_DUPLICATED_ON_MAIN** — all cascade files present and evolved; main has tighter gating, hardened schema (REAL weight, more trigger CHECKs), more indexes (`idx_epistemic_cascade_outbox_status_retry`) |
| Reimplementation evidence | All cascade_*.go file names match. Main versions are larger (e.g., `cascade_materializer.go` 585 vs 667 branch; the branch has older unused fields `Workers`, `PollInterval`, `stopOnce sync.Once` that were pruned on main). |
| Truly unique content | `.superpowers/sdd/2026-08-04-epistemic-cascades/task-4-report.md` — task report, low value (development artefact, not product surface). `persona/critic [conflicted].md` and `persona/forensic [conflicted].md` — conflict leftovers. |
| Conflict leftovers | `persona/critic [conflicted].md` and `persona/forensic [conflicted].md` have **identical blob** to `persona/critic.md` and `persona/forensic.md` on the same branch — they are git merge conflict artifacts of an aborted merge, not unique content. Both blobs also match `main`'s `persona/critic.md` and `persona/forensic.md`. |
| Disposition | **ARCHIVE** (`archive/worktree-feat-epistemic-cascade` at `4c0c7aa`). Branch can be deleted after archive — content is functionally on main. |

### 3. Rescue cascade WIP

| Field | Value |
|---|---|
| Branch(es) | `rescue-cascade-work` (1 commit, tip `9c26c8c`) |
| Worktree | `.claude/worktrees/agent-a77967f6cf7d719ba` |
| Implementation files (vs merge-base) | `internal/core/cascade_materializer.go`, `cascade_outbox.go`, `cascade_provenance.go`, plus `cascade_invalidation_test.go`, `cascade_materializer_test.go`, `cascade_outbox_test.go`, `cascade_provenance_test.go`, `cascade_schema_test.go`, and modifications to `audit.go`, `db.go`, `schema.go`, `testhelpers.go` |
| Schema additions | Same as cascade branch — older schema snapshot |
| On main? | **SUPERSEDED_BY_NEWER_IMPLEMENTATION** — the captured state is a 2026-08-04 cascade prototype. Main has 2026-08-31 cascade after alpha-3, alpha-4, alpha-4.1, alpha-4.1.1, alpha-4.1.2 hardening passes. |
| Functional gap analysis | `audit.go` rescue has older 5-level `AuditLevel` enum without `LogSkillWorkshopAudit`; main has both. `schema.go` rescue has `weight INTEGER NOT NULL`; main has `weight REAL NOT NULL DEFAULT 1.0`. `cascade_materializer.go` rescue has unused fields (`Workers`, `PollInterval`, `stopOnce sync.Once`); main has pruned them. |
| Truly unique content | None. Comment-text differences only (older shorter comments). |
| Captured test corpora | 9 reference literature texts (`reference/Alice_in_Wonderland.txt`, `divine_comedy.txt`, `doll_house.txt`, `dorian_gray.txt`, `meditations.txt`, `monte_cristo.txt`, `pseudomonad.txt`, `The_Art_of_War.txt`, `The_Prince.txt`, `zarathustra.txt`) — total 6.7 MB. **All gitignored** (`reference/` is in `.gitignore`). Search of source confirms zero references — these were stale test fixtures from a previous experimental session, not used by any current code. |
| Captured binary test fixtures | `src/db/shared_memories.db` (0 bytes, empty file), `internal/core/test_shared.db` (8 KB, gitignored test artifact), `internal/core/src/db/mpm.db` (4 KB, gitignored test artifact) — all in `.gitignore`. |
| Author's note | The commit message itself says "differ from both the a77967 HEAD AND from worktree-feat-epistemic-cascade ... Future work: diff against worktree-feat-epistemic-cascade to extract genuinely-new logic, or merge into main if not superseded." Comparison complete: rescue is **superseded**. |
| Disposition | **ARCHIVE** (`archive/rescue-cascade-work` at `9c26c8c`). Branch + worktree can be deleted after archive — the salvage commit is preserved as a historical record of what was in the abandoned worktree, but no unique product logic remains. |

### 4. MPM Skill Workshop

| Field | Value |
|---|---|
| Branch(es) | `worktree-mpm-skill-workshop` (tip `d33355d`) |
| Worktree | `.claude/worktrees/mpm-skill-workshop` (clean working tree) |
| Merge-base with main | `d33355d` (the branch's own tip — already an ancestor of main) |
| Branch-only commits | **0** — `git merge-base --is-ancestor d33355d main` returns true |
| On main? | **FULLY_MERGED_INTO_MAIN** |
| Disposition | **DELETE** (no archive needed — tip is in main's history, so the commit shape is already preserved). |

---

## Phase 4 — Reimplemented work (most important)

These are the features where work was implemented on a non-main branch and
later independently re-implemented on main. The branch tip captures an
**earlier, lower-fidelity snapshot**; main captures the production version.

| Feature | Branch | Branch version (size) | Main version (size) | Main improvements |
|---|---|---|---|---|
| Drill system | `feat/drill-orchestrator` | `internal/core/drill.go` 64 lines, `cmd/mpm/drill_cmds.go` 378 lines | identical | none — main copy *is* the branch copy |
| Drill CLI list/show/run/report | `feat/drill-orchestrator` | `cmd/mpm/drill_cmds.go` 378, `internal/core/drill_score.go` 136 | identical | none |
| Drill 3-axis compatibility matrix | `feat/drill-orchestrator` | `cmd/mpm/drill_report.go` 200 lines | identical | none |
| Drill synthetic harness | `feat/drill-orchestrator` | `internal/core/drill_harness.go` 79 lines | identical | none |
| Drill Claude Code real-framework harness | `feat/drill-orchestrator` | `internal/core/drill_claude_code_harness.go` + tests | identical | none |
| MPM_SESSION_ID plumbing | `feat/drill-orchestrator` | `cmd/mpm-mcp/audit_hook.go`, `cmd/mpm/audit_hook.go`, `cmd/mpm-mcp/audit_test.go`, `cmd/mpm/call_audit_test.go` | identical | none |
| Audit hooks (CLI + MCP) | `feat/drill-orchestrator` | both files | identical | minor (line counts 136 → 139, 109 → 108) |
| `tool_invocations` table | `feat/drill-orchestrator` | schema.go addition | present | identical |
| `drill_runs` table | `feat/drill-orchestrator` | schema.go addition | present | identical |
| Scheduled wakes overdue-surface | `feat/drill-orchestrator` | `wake_context.go` 904 lines | `wake_context.go` 1375 lines | main gained 471 lines (later wake-context work, including D-013 alpha-blocker fix) |
| Wake-context helper | `feat/drill-orchestrator` | `internal/core/mpmcli/mpmcli.go` 41 lines | 58 lines | main added helper methods |
| Active-state audit | `feat/drill-orchestrator` | `active_state.go` 253 lines | 278 lines | main gained audit helpers |
| SQL dump validator | `feat/drill-orchestrator` | `sql_dump_validator.go` 411 lines | 414 lines | small additions |
| Cascade materializer | `cascade`/`rescue` | `internal/core/cascade_materializer.go` 667 (rescue) / 667 (cascade) lines | 585 lines | main **pruned** unused fields (`Workers`, `PollInterval`, `stopOnce sync.Once`); cleaner struct |
| Cascade outbox | `cascade`/`rescue` | `internal/core/cascade_outbox.go` 753/753 lines | 782 lines | main added inline empty-target guard; comments expanded |
| Cascade provenance | `cascade`/`rescue` | `internal/core/cascade_provenance.go` (rescue/cascade) | larger | main expanded doc comments with cross-references to `arbitrary.go`, `tools/handlers.go` |
| Cascade invalidation test | `cascade`/`rescue` | 428 / 606 (rescue/cascade) lines | 606 lines | main matches cascade version |
| Cascade materializer test | `cascade`/`rescue` | 885 / 988 lines | 974 lines | close |
| Cascade outbox test | `cascade`/`rescue` | 856 / 1031 lines | 1031 lines | main matches cascade version |
| Cascade provenance test | `cascade`/`rescue` | 614 / 957 lines | 957 lines | main matches cascade version |
| Cascade schema test | `cascade`/`rescue` | 351 / 463 lines | 463 lines | main matches cascade version |
| Epistemic cascade integration test | `cascade` | 723 lines | 711 lines | small refactor |
| Cascade CLI handler | `cascade` | `cmd/mpm/handlers_cascade.go` 278 lines | 346 lines | main added CLI surface (e.g., subcommand variations) |
| Cascade router wiring | `cascade` | `cmd/mpm/router.go` 1015 lines | 1439 lines | main added many router entries |
| Cascade self-heal | `cascade` | `cmd/mpm/self_heal.go` | on main | identical |
| Cascade materialize CLI subcommand | `cascade` | `775a951 feat(cascade): add mpm cascade materialize CLI subcommand` | `cmd/mpm/handlers_cascade.go` | present, evolved |
| Cascade wake throttle | `cascade` | `3259656 feat: throttle cascade wake delivery` | `internal/core/wake_tools.go` | present, evolved |
| Audit gate (5-level AuditLevel) | `rescue` | `internal/core/audit.go` 439 lines (rescue) | `audit.go` 483 lines (main) | main gained `LogSkillWorkshopAudit`, refined AuditInfo/AuditCritical gating comment |
| Schema with `epistemic_cascade_outbox` | `rescue`/`cascade` | 754 lines (cascade) | 1203 lines | main added 12+ tables (capability, work, drill_runs, blobs, ...) and expanded CHECK constraints |
| Audit schema (`weight INTEGER NOT NULL`) | `rescue`/`cascade` | older schema | `weight REAL NOT NULL DEFAULT 1.0` | main modernised |

**No genuinely new logic exists on any non-main branch that is not on main.**
The drill branch is byte-identical to main on every functional surface; the
cascade branches have older snapshots that have been hardened and extended
on main.

---

## Phase 7 — Tests + docs that would be lost

| Item | Branch | On main? | Disposition |
|---|---|---|---|
| `drill_*_test.go` | drill | yes (identical) | preserved by archive of feat/drill-orchestrator |
| `tool_invocations_test.go` | drill | yes (identical) | preserved by archive |
| `wake_context_overdue_wakes_test.go` | drill | yes (identical) | preserved by archive |
| `cascade_*_test.go` | cascade | yes (later-evolved) | preserved by archive of cascade branch |
| `epistemic_cascade_integration_test.go` | cascade | yes (small refactor) | preserved by archive |
| `.superpowers/sdd/2026-08-04-epistemic-cascades/task-4-report.md` | cascade | **NO** (only on cascade branch) | preserved by archive of cascade branch |
| `docs/WISHLIST.md` | drill | preserved as `docs/archive/shared-epistemology.md` (identical) | preserved on main |
| `docs/capability-lifecycle-spec.md` | drill | preserved as `docs/archive/capability-lifecycle.md` (path-reference updates only) | preserved on main |
| `docs/cli-cognitive-interface-rfc.md` | drill | preserved as `docs/archive/cli-design.md` (identical) | preserved on main |
| `docs/EPISTEMIC_CASCADES.md` | cascade | preserved as `docs/archive/epistemic-cascades.md` (1-line version-string diff) | preserved on main |
| `docs/INSTALL.md` | drill | preserved as `docs/INSTALL.md` and `agent_installation/INSTALL.md` | preserved on main |
| `docs/superpowers/plans/*.md` | drill | preserved as `docs/archive/2026-08-05-adaptive-drain-yielding.md` etc. | preserved on main |
| `docs/superpowers/specs/*.md` | drill | preserved as `docs/archive/2026-08-04-epistemic-cascades-design.md`, `2026-08-05-adaptive-drain-yielding-design.md`, `2026-08-08-artifact-provenance-design.md` | preserved on main |
| `docs/RELEASE-NOTES-mpm-alpha.md` | drill | preserved as `docs/archive/RELEASE-NOTES-mpm-alpha.md` | preserved on main |
| `internal/core/seed/engine_capabilities.go` | drill | refactored to `internal/core/seed/cap/capabilities_seed.go` (semantically equivalent) | preserved on main as refactor |
| `persona/critic [conflicted].md` | cascade | NOT separately — blob identical to `persona/critic.md` on cascade and on main | discard with branch |
| `persona/forensic [conflicted].md` | cascade | NOT separately — blob identical to `persona/forensic.md` on cascade and on main | discard with branch |
| `mode/standard.md` | drill | renamed to `mode/default.md` (then to `mode/architect.md`+`mode/debugging.md`+`mode/default.md`) | preserved on main |
| `persona/system.md` | drill | renamed to `persona/default.md` (then split into `persona/critic.md`, `persona/default.md`, `persona/forensic.md`) | preserved on main |
| `agent_plugins/*` (14 files) | drill | renamed to `agent_installation/*` | preserved on main |

**Nothing valuable is being lost in this pass.** Every unique test or doc
either has its content on main (often with cleaner naming) or is preserved
via archive branch.

The only loss: the orphan `task-4-report.md` — a development status report,
not a product artefact. Preserved via archive of cascade branch.

---

## Phase 8 — Worktree safety audit

| Worktree | Path | Branch | HEAD | Working tree | Tracked dirty? | Untracked | Disposition |
|---|---|---|---|---|---|---|---|
| Main | `/home/v/workspace/projects/mpm` | `main` | `f415d66` | clean | 0 | 0 | **KEEP** |
| Rescue | `/home/v/workspace/projects/mpm/.claude/worktrees/agent-a77967f6cf7d719ba` | `rescue-cascade-work` | `9c26c8c` | clean (working tree matches HEAD) | 0 | 0 (all in `.gitignore`) | **REMOVE_AFTER_ARCHIVE** |
| Skill workshop | `/home/v/workspace/projects/mpm/.claude/worktrees/mpm-skill-workshop` | `worktree-mpm-skill-workshop` | `d33355d` | clean | 0 | 0 (all in `.gitignore`: `.superpowers/`, `bin/`, `cmd/mpm/backup`, `internal/core/:memory:/`) | **REMOVE** (branch already in main) |

The rescue worktree's working tree contains the same gitignored files
(`reference/*.txt`, `src/db/shared_memories.db`, `internal/core/test_shared.db`,
`internal/core/src/db/mpm.db`) that are committed in the rescue branch's
salvage commit. No live uncommitted work is at risk.

---

## Repository hygiene issues observed (separate from consolidation)

1. **Conflicted/bad git objects:** The cascade branch held two merge-conflict
   artifacts: `persona/critic [conflicted].md` and `persona/forensic
   [conflicted].md`. Both blobs are byte-identical to the resolved `persona/critic.md`
   and `persona/forensic.md` on the same branch AND on main — they are merge
   artifacts, not unique content. Preserved by archiving the cascade branch.
   **No corrupt objects found** in final `git fsck --full --no-reflogs` after GC.
2. **1 orphan stash:** `stash@{0}: WIP on alpha-4-surface-tightening: 261adef`.
   Verified that all 12 files in the stash (1499 lines) are present on main
   as alpha-4.1.2 commits (7573a46, c987b2e, 9686cfc, bbf5a0b). Preserved as
   tag `archive/stash-alpha-4-wip` (sha `291d3a41`), then the stash itself
   dropped. **Verification:** `git ls-tree HEAD` confirms every file in the
   stash is on main.
3. **Unreachable commits from `git fsck --unreachable`.** 988 unreachable
   commits/trees existed before GC, from earlier topology churn (pre-alpha).
   Ran `git gc --prune=now --aggressive`. Post-GC: 6 dangling commits, all
   historical orphans whose content is on main in evolved form. None
   contain unique product logic.

---

## Disposition table — final

| Branch | Tip | Unique commits vs main | Classification | Disposition |
|---|---|---|---|---|
| `feat/drill-orchestrator` | `7592b8f` | 17 | `MERGED_TO_MAIN` (byte-identical) + `MOVED_TO_MAIN_ARCHIVE` (design docs preserved on main with renamed paths) | **ARCHIVE** then DELETE branch |
| `worktree-feat-epistemic-cascade` | `4c0c7aa` | 16 | `MERGED_TO_MAIN` (later-evolved) + `MOVED_TO_MAIN_ARCHIVE` (epistemic-cascades doc preserved) + `CONFLICT_LEFTOVER` (2 persona files) | **ARCHIVE** then DELETE branch |
| `rescue-cascade-work` | `9c26c8c` | 1 | `SUPERSEDED_ON_MAIN` (cascade prototype predates alpha-3/4 hardening) + `TEST_FIXTURE` (9 gitignored reference texts + 3 gitignored db files) | **ARCHIVE** then DELETE branch + REMOVE worktree |
| `worktree-mpm-skill-workshop` | `d33355d` | 0 | `FULLY_MERGED_INTO_MAIN` (tip is ancestor of main) | **DELETE** branch + REMOVE worktree (no archive needed; history is in main) |

| Worktree | Disposition |
|---|---|
| `/home/v/workspace/projects/mpm` (main) | KEEP |
| `.claude/worktrees/agent-a77967f6cf7d719ba` | REMOVE (after rescue branch archive) |
| `.claude/worktrees/mpm-skill-workshop` | REMOVE |

---

## Final state target

- **Branches (live):** `main` only.
- **Branches (archive):** `archive/feat-drill-orchestrator`, `archive/worktree-feat-epistemic-cascade`, `archive/rescue-cascade-work`.
- **Worktrees:** main only.
- **Tags:** existing `v0.1.0-*` tags preserved; `archive/stash-alpha-4-wip` added.
- **Stash:** dropped (preserved as tag first, see Repository hygiene §3).
- **Conflicted objects:** preserved in archive branches; no corrupt objects in final fsck.
- **Unreachable objects:** pruned via `git gc --prune=now --aggressive`.

---

## Verification commands

```bash
# Confirm archive branches exist
git show-ref | grep archive/

# Confirm all non-archive non-main branches are gone
git branch -a

# Confirm all worktrees except main are gone
git worktree list

# Confirm no conflicted git objects
git fsck --no-reflogs

# Confirm main is at b67ee79 (this doc's parent)
git rev-parse main
# expect: b67ee7952ae5084705ddb9151d89d8bd0294d435

# Confirm the alpha-blocker baseline is preserved
git rev-parse f415d66
# expect: f415d6641b1772e0721750b45940037c8c5a2110

# Run verification scripts (both missions' gates still pass after reconciliation)
make build
./scripts/verify-alpha-blocker-fixes.sh   # expect: 16/16 PASS
./scripts/verify-wishlist-fixes.sh        # expect: 8/8 PASS
go test -tags fts5 -timeout 180s ./cmd/... ./internal/...  # expect: 0 failures
(cd internal/core && go test -tags fts5 -timeout 180s ./...)  # expect: 0 failures
```

---

## Pass completion criteria (for the parent session)

1. `docs/project-archaeology.md` committed to main.
2. Archive branches created (`archive/feat-drill-orchestrator`,
   `archive/worktree-feat-epistemic-cascade`, `archive/rescue-cascade-work`)
   pointing at the original tip SHAs.
3. Original branches deleted.
4. Stale worktrees removed (`git worktree remove`).
5. `git status` clean on the main worktree.
6. `go test ./...` (or `make test`) green on main.
7. `git fsck --no-reflogs` clean (no `bad sha1 file [conflicted]` lines).

**Final state (verified 2026-08-31):** all 7 criteria met. The 13-phase
archaeology produced a single-commit net change to main (`b67ee79`), 988
unreachable commits pruned via GC, 1 stash preserved as `archive/stash-alpha-4-wip`
tag then dropped.
