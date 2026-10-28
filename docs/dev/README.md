# Development sprint foundation — index and readiness report

> **Scope:** Session A — integration and release engineering.
> **Prepared:** 2026-10-26. **Target:** OpenCode-only release candidate,
> Friday 30 October 2026.
>
> **This is the second revision.** The first revision was the foundation
> audit. This one records what Session A did **after** it: the CI
> integration, the contract correction, the dogfooding verification and the
> launch gate. Every claim below is backed by command output or a commit SHA
> captured on 2026-10-26.

| # | Deliverable | Document | Status |
|---|---|---|---|
| A | Repository inventory and cleanup record | [`inventory.md`](inventory.md) | Complete (round 1) |
| B | Corrective PR integration — **post-merge verification** | [`integration-matrix.md`](integration-matrix.md) | **Complete — all 15 PRs merged; the merged result is verified** |
| C | CI optimization report | [`ci-optimization.md`](ci-optimization.md) | **Complete — the false-green defect is fixed and merged** |
| D | Coordination and ownership specification | [`coordination.md`](coordination.md) | Complete, ledger refreshed |
| E | Shared event-contract proposal | [`event-contract.md`](event-contract.md) | **Revised proposal `mpm-event/0.1` — awaiting B/C acknowledgement** |
| F | Dogfooding and isolation runbook | [`dogfooding-runbook.md`](dogfooding-runbook.md) | **Complete — `~/.mpm-dogfood` initialised and verified** |
| G | Tool-surface evaluation baseline | [`tool-surface-baseline.md`](tool-surface-baseline.md) | Baseline complete; measurement assigned to Session C |
| H | Foundation readiness report | this file | Complete |
| I | **Parallel launch gate and B/C handoff** | [`launch-gate.md`](launch-gate.md) | **Complete — launch conditions met** |
| J | **Runtime migration — the plan** | [`runtime-migration-plan.md`](runtime-migration-plan.md) | **Complete — design accepted (D1–D7); the live migration is a separate approval gate** |
| K | **Runtime migration — the evidence** | [`runtime-migration-evidence.md`](runtime-migration-evidence.md) | **Complete — 135 rehearsal assertions, 0 failures, reproducible with `make rehearsal`** |
| L | **Compact managed instruction block (contract 4.0.0)** | [`session-c-compact-managed-block.md`](session-c-compact-managed-block.md) | **Reviewed — MERGE WITH CORRECTIONS; corrections applied, not merged, not deployed** |

**Row L is a Session C proposal, not a Session A deliverable.** Session A
reviewed it on 2026-10-28 and recommended merging with one non-blocking
annotation — that `mpm_work` and `mpm_help` are reached with `mpm call`, not
as native OpenCode tools — which is applied at both points of use in the
block. It replaces the always-on managed instruction block (11,225 bytes /
2,009 tokens) with a 2,688-byte / 632-token behaviour matrix, and moves the
twelve behavioural commitments into a new section of the canonical protocol.
Its inherited handover is
[`session-c-handoff-2026-10-29.md`](session-c-handoff-2026-10-29.md); that
document carries a status note at its head recording which items were actioned
and three corrections to the handover itself.

---

## 1. What changed since the foundation audit

### 1.1 CI is trustworthy on `main`

| PR | Change | Merge SHA | CI |
|---|---|---|---|
| **#102** | `mpmcli` environment isolation | `e093bcc` | ✅ green before merge |
| **#103** | signal-interruption test determinism | `cefe14b` | ✅ green before merge |
| **#104** | CI gate integrity | `b5b1840` | ✅ green before merge, ✅ green on `main` after |

**#103** removed a flake that made the "Build + core tests" job red for
*any* PR — 4 of 4 full-suite runs failed on pristine `origin/main` before
it.

**#104** fixed a **false-green defect**: the "Full test suite" job ran
`go test … | tee test-output.log` under GitHub's default shell
(`bash -e {0}`, errexit only, **no pipefail**), so a failing `go test`
reported `tee`'s exit status and **could not fail the job**. The job read
`success` while its own log showed `FAIL`. The fix declares `shell: bash`
on the piped steps and adds a per-ref `concurrency` group with
`cancel-in-progress: true`. It **adds** a gate; it removes none.

Verified, not assumed:

```
$ bash -e -c 'false | tee /dev/null'; echo $?
0                                        # the default shell: pipefail absent
$ bash --noprofile --norc -eo pipefail -c 'false | tee /dev/null'; echo $?
1                                        # shell: bash: the failure propagates
```

And the new regression test is **not vacuous**: removing the real
`shell: bash` line from the "Full test sweep" step makes **2 of its 10
tests fail**.

### 1.2 #104's branch did contain #103's commits — and was corrected

`fix/hal9001-ci-gate-integrity` was branched from `5c687d0`, which is
#103's **second** commit. #103 was then **squash-merged** as `cefe14b`,
which also carries #103's third commit `bd51e78` — a commit the #104 branch
did not have. Merging #104 as it stood therefore produced a **content
conflict** in `scripts/tests/test_binary_transaction.py`.

Resolved by `git rebase --onto origin/main 5c687d0
fix/hal9001-ci-gate-integrity`, which replays **only** #104's own commit
(`4c0bfb0`) onto the new `main` and drops #103's already-merged commits.
The resulting head `fa80f48` is a single commit on top of `cefe14b`; no
duplicate or stale change was integrated.

### 1.3 The event contract was corrected, not merely re-drafted

The first draft was a useful foundation. It was **wrong** in eight
substantive respects, and two of its claims did not survive contact with
the code:

* it claimed raw lifecycle events could be routed into the `evidence`
  table — **they cannot**: that table requires an `artifact_id` and a
  CHECK-constrained `artifact_type`;
* it claimed "two events with the same hash and the same type are the same
  observation" — **repeated identical operations are legitimate
  independent events**.

The full correction table is in [`event-contract.md`](event-contract.md) §0.
The version string stays `mpm-event/0.1`, because no implemented interface
depends on the field set yet.

### 1.4 The dogfooding workspace exists and is verified

`~/.mpm-dogfood` was created and seeded earlier on 2026-10-26 (it was
already populated with the sprint's shared decisions, lessons and one
handoff — the foundation report's "not yet initialised" blocker was stale).
It has now been **verified**:

* its database is a **distinct inode** (`36706413`) from the live one
  (`36984047`);
* `mpm` and `mpm-mcp` **ignore `MPM_DB_PATH`** (proven with a sentinel path
  that was never created) — but **`mpm-critic` and `mpm-scheduler` honour
  it**, so it must be pinned;
* the OpenCode plugin's `resolveWorkspace()` / `withWorkspace()` and
  `resolveMpmBinary()` were run and their behaviour confirmed;
* scratch isolation, 8 concurrent writers + 8 concurrent readers, and
  backup/restore against disposable data all pass;
* the live database's **knowledge tables are unchanged**.

### 1.5 The scheduler question has an honest answer

The dogfooding workspace has **no scheduler**, and the `mpm info` line
`mpm-scheduler : running (pid 3050)` is **not** evidence that it does —
the check is process-based, not workspace-scoped. The live scheduler's own
environment and open file descriptors were read to prove it maintains only
`~/.mpm`. The live systemd service was **not** modified.

---

## 2. Exact relevant commit SHAs

```
  #102 mpmcli environment isolation          e093bcc3d90a4d7fb3eb2086568fa79f0b6aa06e
  #89  provenance test isolation              bf142f3
  #90  gc reporting honesty                  8c640d1
  #91  ingest undo honesty                   9e62fd2
  #87  embedding-migration doctor            b50f27c
  #96  reject evidence on save               ea775ad
  #85  operator-only shred                   9a5661c
  #88  staged-row shred linkage              3c18840
  #86  provenance-only evidence              765334b
  #97  directive retention                   d89c11c
  #98  schema-init serialisation             321a695
  #99  installer next steps                  f7e0185
  #95  confidence decay                      f2c1ff4
  #103 signal-test determinism               cefe14b
  #104 CI gate integrity                     b5b1840

integration/session-a                        the tip of main
dev/session-b-core                           the tip of main
dev/session-c-opencode                       the tip of main
main (checked out at ~/src/mpm-release)      the tip of main

installed runtime (~/.mpm)                   3df723156d7649517508212453de72d347759f43
installed binary                             0.1.0-dev+g3df72315.t1792851233
development build used for dogfooding        0.1.0-dev+gb5b18401.t1793023964

CI run on the PR head fa80f48                40332886651   ✅ all three green
CI run on main = b5b1840                     40337045142   ✅ all three green
CI run on main = this document's merge       40344867185   ✅ all three green
CI run on main = the final merge             40351939110   ✅ all three green
```

**Post-merge cleanup (2026-10-26):** 38 local branches, 26 worktrees and
15 remote branches removed — each proven obsolete first. `~/src/worktrees`
went from **1.8 GB to under 25 MB**. One piece of unmerged work was found and
**preserved** onto `chore/mpm-core-gomod-uuid-direct`; the two integration
checkpoints survive as branches. The record is in
[`coordination.md`](coordination.md) §5.

**The verified integration result for this round** is the tip of `main`, with
CI run **`40370094369`** green on all three required checks — that run
covers the fifteen corrective PRs and this documentation set.

> **How to read "the tip of main" here.** The fifteen corrective PRs are
> merged as `b5b1840`, and this document set is the commit that follows it
> on `main`. A document cannot quote its own merge SHA, so the SHAs it
> quotes are the **code** SHAs it describes; the tip is whatever `main`
> points at. To reproduce the state: check out the tip of `main` and read
> `docs/dev/`.

---

## 3. The three worktrees

| Path | Branch | HEAD | Purpose |
|---|---|---|---|
| `~/src/mpm` | `integration/session-a` | tip of `main` | Session A — integration and release |
| `~/src/mpm-core` | `dev/session-b-core` | tip of `main` | Session B — intelligence core |
| `~/src/mpm-opencode` | `dev/session-c-opencode` | tip of `main` | Session C — OpenCode adapter |

All three are **linked worktrees of one Git object store** (`~/src/mpm/.git`),
all three sit on the same verified base, and all three were fast-forwarded
from `414b276` through `b5b1840` to the tip of `main` in this round. `~/src/mpm` carries one
pre-existing untracked file, `cmd/dbg_main.go.tmp` (579 B, 2026-10-17) —
**preserved, not mine to delete**.

---

## 4. Safety checks performed

- The live database was read **read-only** throughout (`file:…?mode=ro`).
  Its knowledge tables are byte-for-byte unchanged; only
  `tool_invocations` grows, from the live OpenCode plugin's own
  `read_wake_context` polling.
- `~/.mpm`, its databases, its installed binaries and its running services
  were **not** modified, migrated or restarted. The systemd unit was not
  edited, reloaded or stopped.
- Every destructive or experimental operation ran in a `mktemp -d` scratch
  area with its own `HOME`, `MPM_WORKSPACE` and `MPM_DB_PATH`.
- Backup and restore were exercised **only** against disposable data.
- The CI negative test (removing the fix to prove the regression test
  catches it) was run in a scratch copy of the workflow, not against the
  repository.

---

## 5. Remaining blockers

| # | Blocker | Who | Blocking what |
|---|---|---|---|
| 1 | **The event contract is not yet acknowledged** by B and C | B + C | Dependent lifecycle implementation. **Not** investigation, and **not** the launch. |
| 2 | **The storage gap for lifecycle observations is real but not yet closed** — one minimal implementation PR after the audits | A | The durable storage of lifecycle events |
| 3 | **The release candidate is not assembled** — the B/C work is not started | B + C | The 30 October target |
| 4 | **Nothing has been deployed** to `~/.mpm` (still at `3df72315`, which predates all fifteen corrective PRs) | A | The release candidate — deliberately, per the deployment gate |
| 5 | `idx_memories_confidence_checked` does not accelerate the decay query it was added for | A | Nothing — a performance claim defect, recorded, not fixed this round |
| 6 | **The runtime migration's remaining implementation** — the `install.sh` bundle phase, the pointer swap, the transactional promotion retargeted at `$BUNDLE/bin`, and the runtime-only uninstall | A | **Closed by this PR** (#156) — the implementation is landed. The **live migration** remains a separate approval gate (J) |
| 7 | **The `lifecycle_events` name collision** — a database already carrying an unrelated table of that name silently does not receive the substrate's schema for it | A | **Closed by this PR** (#156) — the installer's preflight compares the live table's shape against the substrate's own DDL and **refuses** a collision, before the build and before any promotion. SQLite's behaviour is unchanged; what changed is that the collision can no longer reach a binary |

### 5.1 The runtime migration — where it stands (2026-10-28)

The design is **accepted** (decisions D1–D7 in
[`runtime-migration-plan.md`](runtime-migration-plan.md) §0). The safety
prerequisites that must land **before** any migration are landed:

| Prerequisite | State |
|---|---|
| A malformed configuration is a controlled validation error, not a panic (F3) | ✅ landed |
| One resolver for the database path; a divergent selector fails explicitly (D4) | ✅ landed |
| Every installed executable reports a trustworthy embedded identity without `.git` (D5) | ✅ landed |
| The rehearsal is reproducible source, not a local one-off | ✅ landed — `make rehearsal`, 135 assertions, 0 failures |
| The design and its evidence are in version-controlled documentation | ✅ landed — this directory |

**The live migration has NOT been performed**, and no pull request
performs it. It is a separate approval gate with its own operator
checklist (J §9).


---

## 6. Decisions requiring the operator's approval

1. **The four MPM memory records** created during housekeeping — keep,
   refine, or shred. Details and a recommendation in
   [`inventory.md`](inventory.md) §6.
2. ~~**Whether `~/src/mpm-release` should be retired**~~ — **done
   (2026-10-26).** The worktree was removed; the local `main` branch tracks
   `origin/main` without a worktree, and `~/src/mpm` remains Session A's
   `integration/session-a`.
3. **Whether to keep the 12 open-PR worktrees** under `~/src/worktrees/`.
   Their PRs are all merged now; they are reclaimable.
4. **Whether to give the dogfooding workspace its own scheduler unit.** The
   mechanism exists (`--db`, `--lock`, `MPM_WORKSPACE`), but creating a unit
   is a deployment act and the brief for this round forbade touching the
   live systemd service.
5. **Event contract v0.1 acceptance** by Sessions B and C — the MSI items
   M1–M7 in [`event-contract.md`](event-contract.md) §7.

---

## 7. What Session A does next, while B and C investigate

1. **Hold the CI gate** on `main` and keep the release checks honest.
2. **Adjudicate** B's and C's proposed contract changes against the
   evidence in the contract.
3. **Prepare the single minimal implementation PR** for whatever the audits
   show is genuinely missing — one PR, with its contract tests.
4. **Keep the deployment gate closed** until an accepted milestone has
   passed the full verification gate.
5. **Refresh the integration record** if B and C's PRs land.

---

## 8. Readiness gate

| Requirement | Status |
|---|---|
| CI on `main` is trustworthy | ✅ #104 merged; the full-suite sweep can now fail; all three checks green on the merged commit |
| Three isolated development worktrees on one verified base | ✅ all at the tip of `main`, clean, independent branches |
| Existing unique and unmerged work preserved | ✅ **7 local branches, 3 worktrees** after the post-merge cleanup; the one piece of unmerged work found was preserved onto `chore/mpm-core-gomod-uuid-direct` |
| Branch and file ownership boundaries documented | ✅ [`coordination.md`](coordination.md) |
| Shared event contract reviewed and presented | ✅ revised `mpm-event/0.1` — **awaiting B/C acknowledgement of M1–M7** |
| Core/adapter ownership split unambiguous | ✅ [`coordination.md`](coordination.md) §2 |
| Shared dogfooding workspace configured **and verified** | ✅ `~/.mpm-dogfood`; isolation, concurrency, backup/restore all verified |
| Destructive tests isolated from live and shared knowledge | ✅ [`dogfooding-runbook.md`](dogfooding-runbook.md) §8.3 |
| PR dependencies and integration conflicts understood and resolved | ✅ both conflicting schema indexes preserved and re-reviewed |
| CI release-verification requirements known and preserved | ✅ [`ci-optimization.md`](ci-optimization.md) §6 |
| Session A remains sole merge and deployment authority | ✅ [`coordination.md`](coordination.md) §1 |

**Verdict: the gate conditions for launching Sessions B and C are met.**
They may launch, read, audit and measure immediately. Substantive lifecycle
implementation waits on the contract being acknowledged — that is a gate on
*implementation*, not on *starting*.

> **Session A does not launch B and C.** The launch is the operator's call.
> The requirements each session must satisfy on its first hour are in
> [`launch-gate.md`](launch-gate.md).
