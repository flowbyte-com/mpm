# Development sprint foundation — index and readiness report

> **Scope:** Session A foundation assignment (integration + release engineering).
> **Prepared:** 2026-10-26. **Target:** OpenCode-only release candidate, Friday 30 October 2026.

This directory holds the eight foundation deliverables. Every claim below is
backed by command output captured on 2026-10-26; nothing here is a plan
presented as a completed action.

| # | Deliverable | Document | Status |
|---|---|---|---|
| A | Repository inventory and cleanup record | [`inventory.md`](inventory.md) | Complete |
| B | Corrective PR integration matrix | [`integration-matrix.md`](integration-matrix.md) | Complete |
| C | CI optimization report | [`ci-optimization.md`](ci-optimization.md) | Baseline complete; two quick wins merged as PRs, larger restructuring deferred |
| D | Coordination and ownership specification | [`coordination.md`](coordination.md) | Complete |
| E | Shared event-contract proposal | [`event-contract.md`](event-contract.md) | **Draft for B/C review — not agreed** |
| F | Dogfooding and isolation runbook | [`dogfooding-runbook.md`](dogfooding-runbook.md) | Complete |
| G | Tool-surface evaluation baseline | [`tool-surface-baseline.md`](tool-surface-baseline.md) | Baseline complete; comparative work deferred to Session C |
| H | Foundation readiness report | this file | Complete |

---

## 1. What was accomplished

### 1.1 Three permanent development worktrees exist and build

| Path | Branch | HEAD | Purpose | Builds |
|---|---|---|---|---|
| `~/src/mpm` | `integration/session-a` | `414b276` | Session A — integration and release | ✅ 5 binaries |
| `~/src/mpm-core` | `dev/session-b-core` | `414b276` | Session B — intelligence core | ✅ 5 binaries |
| `~/src/mpm-opencode` | `dev/session-c-opencode` | `414b276` | Session C — OpenCode adapter | ✅ 5 binaries |

All three are **linked worktrees of one Git object store** (`~/src/mpm/.git`),
verified by `git worktree list`; they are not independent clones. All three
sit on the same verified base, `origin/main` = `414b27651ee66ec5dcad9ba75cd43b6c4b2fac9c`.

### 1.2 Corrective PR work advanced

| PR | Change | Head | CI |
|---|---|---|---|
| **#99** | contract test corrected to the supported `mpm_context` payload form | `0f67fcb` | ✅ **all three checks pass** |
| **#102** | `mpmcli` environment isolation (new) | `af8a699` | ✅ green |
| **#103** | signal-interruption test determinism (new) | `5c687d0` | re-running after a CI-caught fix |
| **#104** | CI gate integrity (new, stacked on #103) | `4c0bfb0` | queued |
| **#96** | gofmt correction pushed to its owning branch | `49c8e6f` | re-running |

### 1.3 CI false-green defect found and fixed

The "Full test suite" job **could not fail**: its step piped `go test` into
`tee` under GitHub's default shell, which has no `pipefail`. PR #104 restores
the gate and adds superseded-run cancellation. See
[`ci-optimization.md`](ci-optimization.md).

### 1.4 Two pre-existing test defects found, diagnosed and fixed

| Defect | Evidence | PR |
|---|---|---|
| `mpm-core/mpmcli` inherits ambient `MPM_PROVENANCE_FRAMEWORK` | reproduces identically on pristine `origin/main` | #102 |
| signal-interruption test is timing-dependent | **4 of 4** full-suite runs failed on pristine `origin/main` | #103 |

### 1.5 The four MPM records created during housekeeping were **not** shredded

They are reported in [`inventory.md`](inventory.md) §6 for the user's decision.
No further MPM writes were made to the live database during this assignment.

---

## 2. Exact relevant commit SHAs

```
origin/main                                  414b27651ee66ec5dcad9ba75cd43b6c4b2fac9c
LICENSE blob (unchanged)                     be628ce05bea64f91149c26557612de16bc212c6

#99  fix/hal9001-r3-install-next-steps       0f67fcbed9f5733593e93b5e1a0233891d4537f4
#96  fix/hal9001-r2-save-evidence            49c8e6f354baff68378dd432c00f63e4e1cfa5b0
#102 fix/hal9001-mpmcli-env-isolation         af8a699e8f91e5a89e95bd9d99520970d0aa8219
#103 fix/hal9001-signal-test-determinism      5c687d0017892c5317835ba32be638c57c36df1d
#104 fix/hal9001-ci-gate-integrity            4c0bfb0e8461f968490fc752cdd4eec40ada3e62

integration/session-a                        414b27651ee66ec5dcad9ba75cd43b6c4b2fac9c
dev/session-b-core                           414b27651ee66ec5dcad9ba75cd43b6c4b2fac9c
dev/session-c-opencode                       414b27651ee66ec5dcad9ba75cd43b6c4b2fac9c
hal9001/r3-integration                       cbe6af7c27e172ef0c8e87057238fb830924d88f
integration/pre-release-12                   a8c3b6037e8d3404978259aa545ced496a97cf5c

installed runtime (~/.mpm)                   3df723156d7649517508212453de72d347759f43
installed binary                             0.1.0-dev+g3df72315.t1792851233
```

---

## 3. Uncommitted changes

| Location | State |
|---|---|
| `~/src/mpm` | clean except the pre-existing untracked `cmd/dbg_main.go.tmp` (579 B, 2026-10-17) — **preserved, not mine to delete** |
| `~/src/mpm-core`, `~/src/mpm-opencode` | clean |
| `~/src/mpm-release` | clean, `main` @ `3df7231` |
| all 12 other worktrees | clean |

The only work-in-progress lives on pushed branches:
`fix/hal9001-mpmcli-env-isolation`, `fix/hal9001-signal-test-determinism`,
`fix/hal9001-ci-gate-integrity`, and this documentation branch
`docs/sprint-foundation` (unpushed at the time of writing).

---

## 4. Safety checks performed

- Every branch deletion and worktree removal in the preceding housekeeping
  round re-verified the SHA **immediately before** the destructive command.
- The 12-way integration merge was performed in a **disposable worktree**
  with the single conflict resolved by keeping both index lines; the result
  passed `make build` and `make test-race` (24/24 Go packages, 0 data races).
- The live database was read **read-only** throughout (`file:…?mode=ro`).
- `~/.mpm`, its databases, installed binaries and running services were not
  modified, migrated or restarted.
- Every automated test used an isolated `HOME` / `MPM_WORKSPACE`, or a
  `t.TempDir()`-scoped fixture.
- The `LICENSE` blob is byte-identical to `origin/main`'s.

---

## 5. Remaining blockers

| # | Blocker | Who | Blocking what |
|---|---|---|---|
| 1 | **Event contract not yet agreed** | Sessions B + C | Substantial lifecycle implementation. **Mandatory gate.** |
| 2 | `#103` must merge before `#104` is green | Session A | #104's CI |
| 3 | `integration/pre-release-12` is a *checkpoint*, not a baseline | Session A | Must be rebuilt from reviewed PR heads before the final integration |
| 4 | `~/src/mpm-release` holds `main` and is 12 commits behind | Session A | Promoting `main` into `~/src/mpm` (one command, deferred) |
| 5 | Dogfooding workspace **not yet initialised** | Session A | Shared-knowledge verification (procedure is fully specified and verified in [`dogfooding-runbook.md`](dogfooding-runbook.md)) |
| 6 | `feature/version-build-provenance` shows `[gone]` upstream | Session A | Cosmetic; branch content is proven merged |

---

## 6. Decisions requiring approval

1. **The four MPM memory records** — keep, refine, or shred. Details and my
   recommendation in [`inventory.md`](inventory.md) §6.
2. **Whether `~/src/mpm-release` should be retired** so that `main` can live
   in `~/src/mpm`. It is clean and 12 commits stale; retiring it reclaims
   124 MB and removes a second checkout of the same repo.
3. **Whether to keep the 12 open-PR worktrees** under `~/src/worktrees/`
   until their PRs merge, or retire the six whose branches are already
   represented in `hal9001/r3-integration`. Reclaimable now: ~750 MB.
4. **CI restructuring beyond the two merged quick wins** — the full-suite
   job's redundancy is documented but not changed, because removing it would
   reduce verification.
5. **Event contract v0.1 acceptance** by Sessions B and C.

---

## 7. Recommended next actions

**Session A (now)**

1. Merge `#103` (signal-test determinism) — it unblocks `#104` and removes a
   flake that makes *any* PR's "Build + core tests" job intermittently red.
2. Merge `#104` (CI gate integrity), then `#102` (mpmcli isolation).
3. Merge the 12 corrective PRs in the order in
   [`integration-matrix.md`](integration-matrix.md), resolving the single
   `internal/core/schema.go` conflict by keeping **both** index lines.
4. Rebuild the integration result from the *merged* heads, then re-run the
   full gate (`make test-race`) on the rebuilt tree.
5. Present the event contract (document E) to Sessions B and C for explicit
   acknowledgement of a versioned contract.
6. Only then: milestone promotion to `~/.mpm` per
   [`dogfooding-runbook.md`](dogfooding-runbook.md) §6.

**Session B** — may begin *investigation and auditing* of the existing critic,
scheduler, evidence and telemetry implementations immediately (read-only,
in `~/src/mpm-core`). Substantial new lifecycle ingestion waits on the
agreed contract.

**Session C** — may begin *read-only investigation* of the installed OpenCode
plugin API in `~/src/mpm-opencode` immediately. See
[`event-contract.md`](event-contract.md) §4 for what is actually known today
about the available hooks.

**Neither session may merge to `main` or deploy to `~/.mpm`.**

---

## 8. Readiness gate

| Requirement | Status |
|---|---|
| Three isolated development worktrees verified | ✅ built, clean, independent branches, one object store |
| Existing unique and unmerged work preserved | ✅ 16 local branches, 13 remote, 15 worktrees; nothing deleted this round |
| Branch and file ownership boundaries documented | ✅ [`coordination.md`](coordination.md) |
| Shared event-contract draft reviewed and agreed | ⛔ **drafted, not agreed — blocks substantial B/C work** |
| Core/adapter ownership split unambiguous | ✅ [`coordination.md`](coordination.md) §2 |
| Shared dogfooding workspace configured or setup fully verified | ⚠️ **procedure verified, not yet initialised** |
| Destructive tests isolated from live and shared knowledge | ✅ see [`dogfooding-runbook.md`](dogfooding-runbook.md) §5 |
| PR dependencies and major integration conflicts understood | ✅ [`integration-matrix.md`](integration-matrix.md) |
| CI release-verification requirements known | ✅ [`ci-optimization.md`](ci-optimization.md) |
| Session A remains sole merge and deployment authority | ✅ stated in [`coordination.md`](coordination.md) §1 |

**Verdict: the foundation is safe and Sessions B and C may launch for
read-only investigation and audit immediately. Substantial lifecycle
implementation waits on the event contract being agreed.**
