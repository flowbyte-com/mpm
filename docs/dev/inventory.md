# A — Repository inventory and cleanup record

> Session A foundation assignment. All state below was **re-measured** on
> 2026-10-26; the previous handover's figures are treated as claims to verify,
> not as facts.

## 1. Repository identity

There are **two independent clones** of the same upstream repository. This
distinction governs every deployment and isolation decision in this sprint.

| | Development repo | Installed runtime |
|---|---|---|
| Path | `~/src/mpm` | `~/.mpm` |
| Git identity | `github.com/flowbyte-com/mpm` (`origin`) | same upstream, **separate object store** |
| Object store | `~/src/mpm/.git/objects` (inode 36315323) | `~/.mpm/.git/objects` (inode 36964284) |
| Branch / HEAD | `integration/session-a` @ `414b276` | `main` @ `3df7231` |
| Working tree | clean except one untracked scratch file | clean |
| Role | where development happens | where the **live knowledge database** and the **installed binaries** live |

**Finding:** `~/.mpm` is not a bare runtime directory — it is a full git
checkout of the repository with its own `.git`, `Makefile`, `install.sh` and
`.mcp.json`. `install.sh` builds the installed binaries **from that checkout**,
which is why the installed binary reports `0.1.0-dev+g3df72315.t1792851233`
— exactly the runtime checkout's HEAD. Deployment therefore means: advance
`~/.mpm` to a verified commit, then run that checkout's `install.sh`.

**Canonical local repository: `~/src/mpm`.** It holds the object store for all
linked worktrees and is the repository under active development. `~/.mpm` is a
*deployment target*, not a development repository, and must not be used as a
worktree.

## 2. Layout before this assignment

```
~/src/mpm                      primary checkout, on feature/version-build-provenance (c4350f2)
~/src/mpm-release              release checkout, main @ 3df7231
~/src/worktrees/hal9001-integration-r3   hal9001/r3-integration (cbe6af7)
~/src/worktrees/hal9001-r2     fix/hal9001-r2-save-evidence
~/src/worktrees/hal9001-r3     fix/hal9001-r3-directive-retention
~/src/worktrees/hal9001-r3-nextsteps    fix/hal9001-r3-install-next-steps
~/src/worktrees/hal9001-r3-schema-lock  fix/hal9001-r3-schema-init-concurrency
~/src/worktrees/hal9001-shred-auth      fix/hal9001-provenance-test-isolation
~/src/worktrees/mergesim       integration/pre-release-12 (validation tree)
~/src/worktrees/pr99-check     detached @ 18dd9f1 (holding the #99 fix)
```

12 worktrees, 19 local branches, 16 remote branches.

## 3. Layout now

### 3.1 The three permanent development worktrees

```
~/src/mpm                 Session A  integration/session-a      414b276
~/src/mpm-core            Session B  dev/session-b-core         414b276
~/src/mpm-opencode        Session C  dev/session-c-opencode     414b276
```

All three are linked worktrees of `~/src/mpm/.git` — one object store, one
`git fetch`, one set of refs. Each has an **independent branch and index**;
each was verified to build all five binaries (`make build`).

### 3.2 Everything else, and why it is still there

| Path | Branch | HEAD | Why retained |
|---|---|---|---|
| `~/src/mpm-release` | `main` | `3df7231` | holds `main`; clean; 12 commits behind. **Release snapshot / `main` holder.** |
| `worktrees/hal9001-integration-r3` | `hal9001/r3-integration` | `cbe6af7` | the pre-existing combined integration tree (PRs #85–#98) |
| `worktrees/mergesim` | `integration/pre-release-12` | `a8c3b60` | **the validated 12-way integration checkpoint** |
| `worktrees/pr99-check` | detached | `18dd9f1` | held the #99 fix; the fix now lives on the #99 branch — **retire candidate** |
| `worktrees/hal9001-r2` | `fix/hal9001-r2-save-evidence` | `49c8e6f` | open PR #96 |
| `worktrees/hal9001-r3` | `fix/hal9001-r3-directive-retention` | `583d325` | open PR #97 |
| `worktrees/hal9001-r3-nextsteps` | `fix/hal9001-r3-install-next-steps` | `0f67fcb` | open PR #99 |
| `worktrees/hal9001-r3-schema-lock` | `fix/hal9001-r3-schema-init-concurrency` | `4fe840d` | open PR #98 |
| `worktrees/hal9001-shred-auth` | `fix/hal9001-provenance-test-isolation` | `ed9ad9d` | open PR #89 |
| `worktrees/fix-mpmcli-env` | `fix/hal9001-mpmcli-env-isolation` | `af8a699` | new PR #102 |
| `worktrees/fix-signal-test` | `fix/hal9001-signal-test-determinism` | `5c687d0` | new PR #103 |
| `worktrees/fix-ci-gate` | `fix/hal9001-ci-gate-integrity` | `4c0bfb0` | new PR #104 |

## 4. Branch inventory

**16 local branches**

```
integration/session-a                       414b276   (new — Session A integration base)
dev/session-b-core                          414b276   (new — Session B)
dev/session-c-opencode                      414b276   (new — Session C)
main                                        3df7231   (held by ~/src/mpm-release)
hal9001/r3-integration                      cbe6af7   (unmerged combined tree, no PR)
integration/pre-release-12                  a8c3b60   (validated checkpoint, no PR)
feature/version-build-provenance            c4350f2   (merged — see §5)
fix/hal9001-shred-agent-auth                7ae5958   PR #85
fix/hal9001-confidence-provenance           68a8b22   PR #86
fix/hal9001-embedding-migration-diagnostics da25efe   PR #87
fix/hal9001-raw-memory-shred-linkage       e0a2af9   PR #88
fix/hal9001-provenance-test-isolation       ed9ad9d   PR #89
fix/hal9001-gc-reporting-honesty            0a6b218   PR #90
fix/hal9001-ingest-undo-honesty             b6bffa8   PR #91
fix/hal9001-r2-confidence-decay             ac52393   PR #95
fix/hal9001-r2-save-evidence                49c8e6f   PR #96
fix/hal9001-r3-directive-retention          583d325   PR #97
fix/hal9001-r3-schema-init-concurrency      4fe840d   PR #98
fix/hal9001-r3-install-next-steps           0f67fcb   PR #99
fix/hal9001-mpmcli-env-isolation            af8a699   PR #102
fix/hal9001-signal-test-determinism         5c687d0   PR #103
fix/hal9001-ci-gate-integrity               4c0bfb0   PR #104
```

**13 remote branches:** the 12 open-PR heads + `origin/main`. Every remote
head's SHA was verified equal to its PR's `headRefOid`.

## 5. Unique and unmerged work identified

| Item | Unique? | Disposition |
|---|---|---|
| `hal9001/r3-integration` (`cbe6af7`) | **Yes** — 24 commits ahead of `origin/main`; a combined tree of PRs #85–#98 with merge commits | **preserved** |
| `integration/pre-release-12` (`a8c3b60`) | **Yes** — 13 commits; a validated 12-way integration of all twelve corrective PRs plus the #99 contract fix | **preserved** — the user designated it a checkpoint, explicitly *not* the new baseline |
| `feature/version-build-provenance` (`c4350f2`) | **No** — `git diff 9842744b feature/version-build-provenance` is empty, and `9842744b` (PR #46's squash commit) is an ancestor of `origin/main` | **preserved** (branch only; no longer checked out) |
| `~/src/mpm/cmd/dbg_main.go.tmp` | 579 B, dated 2026-10-17 — a throwaway `go run` SQLite probe | **preserved**, untracked, carried across the branch switch intact |
| `~/.mpm/src/db/mpm.db` | **the live knowledge store** | **untouched** — 27 reference docs / 116 chunks / 69 memories at last read |

**Nothing was deleted in this assignment round.** Every directory, branch and
file listed above still exists.

## 6. The four MPM records created during housekeeping — for decision

The user asked for these to be reported before any decision. **They have not
been shredded.** They are the only writes this sprint has made to the live
database.

### Record 1 — memory `468712d48e80d33c` — *the #88/#95 conflict and its resolution*

- **Type:** `memories` collection (project-scoped fact), confidence 0.8, weight 5.
- **Contents:** both PRs insert a `CommonIndexes` line at the *same* anchor in
  `internal/core/schema.go`; the conflict is symmetric and unavoidable by
  reordering; the resolution is to keep **both** index lines; the two
  `SafeMigrations` entries sit at different anchors and never conflict. Also
  lists every other shared-file overlap and its clean merge result.
- **Provenance:** written by the Session-A housekeeping agent during the
  earlier branch-cleanup round, using `mpm_memory save`.
- **Usefulness:** **high, but now superseded by a committed, reviewable
  artifact.** The same content is in PR #103's and the integration matrix's
  body, and in `docs/dev/integration-matrix.md`. Its durable value is the
  *conflict-resolution fact*, which a future agent will otherwise rediscover.

### Record 2 — memory `9867a79cd5392cee` — *the mpmcli ambient-environment defect*

- **Type:** `memories`, confidence 0.8, weight 5.
- **Contents:** the exact symptom, the root cause (`ActiveContextFromEnv`
  reads the canonical `MPM_PROVENANCE_FRAMEWORK` first, while the test only
  cleared the legacy `MPM_FRAMEWORK`), the proof that it reproduces on
  pristine `origin/main`, and the observation that PR #89's scope did not
  reach `internal/core/mpmcli`.
- **Provenance:** same agent, same round.
- **Usefulness:** **now fully superseded by PR #102**, which is the fix and
  carries the diagnosis in its commit message and PR body. The record is
  strictly redundant.

### Record 3 — decision `b4451481f2863a9e` — *the merge order for the twelve corrective PRs*

- **Type:** `decisions` collection.
- **Contents:** the recommended order
  `#89 → #90 → #91 → #87 → #96 → #85 → #88 → #86 → #95 → #97 → #98 → #99`,
  with the per-step rationale and the full conflict matrix.
- **Provenance:** same agent, same round.
- **Usefulness:** **medium.** The order is still the recommended one, but it
  now lives in a reviewable, git-versioned document
  (`docs/dev/integration-matrix.md`) and in the PR bodies. The MPM decision
  record adds reachability from other agents' wake context — but only if the
  agents are pointed at the same workspace.

### Record 4 — handoff `f87482811ad1a142` — *the housekeeping completion notice*

- **Type:** `handoffs` collection, `ended_state: clean`.
- **Contents:** the deletion list with SHAs, the final inventory, the merge
  order and the #99 blocker.
- **Provenance:** same agent, same round.
- **Usefulness:** **low, and actively misleading now.** It describes a state
  that has since changed materially (#99 is green, three new PRs exist, the
  layout is different). A handoff is a snapshot; a stale snapshot read as
  current fact is exactly the failure mode the sprint's first principle
  warns about.

### Recommendation

Shred **record 4** (stale snapshot), keep **records 1 and 3** (durable
facts not otherwise in the wake path), and shred or rewrite **record 2**
(fully superseded by PR #102). Alternatively shred all four — the git
artifacts are the system of record, and the user's standing instruction for
housekeeping and automated-test tasks is to avoid unrequested writes to live
MPM.

**No further MPM writes will be made pending this decision.**

## 7. Cleanup candidates and reclaimable space

Nothing below has been removed. Each entry states the exact action and what
must be true first.

| Candidate | Action | Reclaims | Precondition |
|---|---|---|---|
| `worktrees/pr99-check` | `git worktree remove` | 52 MB | its contents are now on the #99 branch — **satisfied** |
| `worktrees/fix-mpmcli-env`, `fix-signal-test`, `fix-ci-gate` | `git worktree remove` | 128 MB | the three PRs are merged |
| `worktrees/hal9001-r2`, `-r3`, `-r3-nextsteps`, `-r3-schema-lock`, `-shred-auth` | `git worktree remove` | 648 MB | the five PRs are merged |
| `worktrees/mergesim` | `git worktree remove` | 129 MB | the pre-release checkpoint is superseded by a rebuilt integration |
| `worktrees/hal9001-integration-r3` | `git worktree remove` | 127 MB | superseded by the rebuilt integration |
| `~/src/mpm-release` | retire after `main` is moved into `~/src/mpm` | 124 MB | `main` fast-forwarded; release snapshot no longer needed |
| `feature/version-build-provenance` | `git branch -d` | negligible | content proven merged (already proven) |
| `~/src/mpm/.build` | `make clean` | ~30 MB per tree | reproducible build output |

**Total reclaimable: ~1.2 GB** of the 1.9 GB under `~/src`. Nothing is
reclaimable *today* except `pr99-check` and the build outputs, and even
those are left in place pending the user's approval — the checkpoint
worktrees in particular are explicitly designated as evidence.

## 8. Blockers requiring approval

1. Retirement of `~/src/mpm-release` (and therefore where `main` lives).
2. Retirement of the pre-release checkpoint worktrees.
3. Disposition of the four MPM records (§6).

## 9. What was **not** done, and why

- **No PR was merged.** Session A owns merging; this round produced the plan.
- **Nothing was deployed.** `~/.mpm` remains at `3df7231`; the installed
  binaries and the scheduler were not touched.
- **The live database was not modified** except by the four records in §6,
  which were written during the earlier round and are reported here for
  decision.
- **No worktree was removed** in this round: every candidate in §7 is either
  still needed by an open PR, is designated evidence, or is the release
  snapshot.
