# Continuity 001, PR 6 — the pilot prerequisites

Status: **historical, amended**. MPM-managed operating-system containment
has been removed: execution is native, the agent framework and the
deployment environment own isolation, and T49/T50 are retired with the
removed subsystem. The current contract is
[`continuity-001-containment-removal-amendment.md`](continuity-001-containment-removal-amendment.md).
Credential closeout, operator-authorised recovery and explicit execution
authorisation remain mandatory.

This note records the two safe prerequisites the operator authorised before
any live inference: the operator-only project-creation operation and the
host-created Git worktree. It also records the evidence rehearsal and the
Gate 1 blockers that remain. It does **not** claim a pilot ran.

Baseline: `origin/main` = `b72d408d8e01a140706d248861b31b66b70e9441`
(the accepted PR 5 merge), tree
`33a33c6a43feb3c07d0663d6faaea589e5299edc`.

---

## 1. Project creation (Decision A)

`mpm assignment create-project --id cpj-<16 lowercase hex>`.

The `projects` row is the identity every assignment names. The
schema existed; the supported way to create the row did not, and the only
path was a raw `INSERT` in tests. The new operation:

- takes an `OperatorAuthority` and is absent from the tool registry, so no
  `mpm_assignment` action, no `mpm call` action and no MCP schema reaches
  it;
- validates the project model's identity shape (`cpj-` and 16 lowercase
  hexadecimal characters);
- is **idempotent** for a live identity: a repeat returns the existing row
  with `created: false`, and no field is rewritten;
- refuses a **withdrawn** identity (`deleted_at` set) — an identity is not
  resurrected;
- runs in one transaction; a concurrent creator that loses the primary-key
  race reads the winner back and reports `created: false`;
- writes the `projects` row and **nothing else** — not `project_names`,
  not a thread, not a checkpoint;
- records `created_by` as the writer's attribution. The actor is a label,
  never a credential, and creation does not branch on it.

Scope: `internal/core/assignment/create_project.go`, the
`create-project` verb in `cmd/mpm/assignment_cmds.go`, and tests. No
authentication claim and no broader project-management API.

---

## 2. The host-created Git worktree (Decision B)

PR 4 created an empty directory per assignment. That is enough to run a
file-edit-only worker against nothing and not enough to do engineering
work. The dispatcher now owns a genuine Git worktree:

- `workspace_root` is the operator-configured root; `repository_path` is
  the source repository. The two must be absolute and must not be the same
  or contain one another. Both are resolved, so a symlinked root that
  resolves equal to or inside the repository is refused too. The
  An existing root entry must be a directory: a regular file or a
  dangling symlink is refused. The repository and the resolved root are
  validated when the engine is built, before any claim.
- The host runs `git worktree add -b mpm/assignment/<id> <root>/<id>
  <base_sha>` for an implementation assignment. The base is the
  assignment's exact `BaseSHA`; a missing or malformed base, a base that
  is not in the repository, or a leftover branch without its worktree is
  refused.
- Recovery **reuses** the worktree only after verifying: the path is a
  real directory, not a symlink, and a direct child of the resolved root;
  `.git` is a regular file whose target resolves inside the repository's
  `.git/worktrees/`; the metadata's `gitdir` backlink points at this
  workspace; the worktree's common Git directory is the configured
  repository's; the branch is the assignment's; and HEAD is the base or a
  descendant of it (the host may have committed).
- A bare directory, a full clone, a symlinked path, another repository's
  worktree, another branch, a moved worktree (even with the same
  assignment id), metadata outside `.git/worktrees/`, a tampered pointer
  or a base that is not an ancestor is **refused**, never adopted.
- The worktree's `.git` is a file whose pointer names the host's Git
  metadata. The worker can read the checked-out files; it does not run
  `git` (its agent definition denies the shell), and the host stages and
  commits. MPM does not sandbox the worker, so the pointer target is
  reachable by an agent with shell access; the agent definition is the
  permission boundary, and the deployment environment is the isolation
  boundary.
- On the **dispatch** path, the claim is taken before provisioning; a
  live lease or a stale epoch refuses before any workspace exists.
- On the **recovery** path, the worktree is never created: recovery
  verifies the recorded worktree and refuses when it is missing. An
  outstanding attempt implies a launch, and a missing worktree is a fact
  for a human; creating one while a lease or an older session may still be
  live would be a second writer's workspace.

Scope: `internal/core/dispatch/worktree.go`, the `repository_path`
configuration field, and the dispatcher wiring. Tests use real temporary
Git repositories and fake runtimes.

---

## 3. File-edit-only execution is unchanged (Decision C)

The pinned `mpm-worker` definition is byte-identical to the template. It
still denies `shell`, `execute`, `browser`, `webfetch`, `websearch`,
`skill`, `subagent`, `mpm_*`, host-side file tools and every filesystem
root but the worktree. Tests and commits are host-controlled. A failing
test can reach the worker only as a recorded, bounded note — no general
shell was added.

---

## 4. The required negative controls

Each one is a test that takes the adversarial shape and requires the safe
answer. The negative controls are the adversarial tests; where a test is
feature regression coverage rather than an attack, this note says so. The
mutation-style controls remain PR 2's Python harness.

| # | control | test |
|---|---|---|
| 1 | model-facing tools cannot create projects | `TestAssignment_NoRegistryActionCreatesAProject`, `TestAssignment_OperatorActionsAreNotAdvertised`, `TestAssignment_NoPayloadFieldUnlocksTheOperatorActions` |
| 2 | duplicate creation is idempotent; conflicting creation refused | `TestCreateProjectIsIdempotent`, `TestCreateProjectRefusesAWithdrawnIdentity`, `TestConcurrentProjectCreationCreatesOneRow` (the CLI test is feature coverage, not a control) |
| 3 | an unexpected existing workspace is not adopted | `TestTheProvisionerRefusesAnUnexpectedDirectory`, `TestTheProvisionerRefusesAnUnexpectedFile`, `TestTheProvisionerRefusesAFullClone` |
| 4 | a worktree cannot escape its configured root | `TestTheProvisionerRefusesASymlinkedWorkspace`, `TestTheProvisionerRefusesAnAssignmentIDThatIsNotAPathShape`, `TestTheProvisionerRefusesAWorkspaceRootInsideTheRepository` (and the refusal leaves no directory behind), `TestTheProvisionerRefusesARootEqualToTheRepository`, `TestANewEngineRefusesAResolvedRootInsideTheRepository`, `TestANewEngineRefusesAResolvedRootEqualToTheRepository`, `TestANewEngineRefusesAWorkspaceRootThatIsAFile`, `TestANewEngineRefusesADanglingSymlinkRoot` |
| 5 | a stale/second claim cannot take another assignment's workspace | `TestALiveLeaseDoesNotProvisionAWorkspace`, `TestTheProvisionerRefusesAnotherRepository`, `TestTheProvisionerRefusesTheWrongBranch` |
| 6 | a worker cannot reach or modify the real Git metadata | `TestTheProvisionerCreatesAWorktreeAtTheBase` (the pointer target is outside the workspace), `TestTheProvisionerRefusesATamperedPointer`, `TestTheProvisionerRefusesAPointerToTheMainGitDirectory`, `TestTheProvisionerRefusesAMovedWorktree`, `TestTheProvisionerRefusesMetadataOutsideTheWorktreeDirectory`, `TestTheProvisionerRefusesASameIDMove`, `TestTheProvisionerIgnoresAnAmbientGitDir`, `TestTheConfiguredPathsMustNotContainOneAnother`, `TestANewEngineRefusesAResolvedRootInsideTheRepository`. Residual: the pointer's text names the host path; MPM does not sandbox the worker, so an agent with shell access could follow it. |
| 7 | a worker cannot execute a shell or host-side test command | `TestWorkerDefinitionDeniesExecution` (the pinned definition), `TestWorkerDefinitionIsPinnedToTheTemplate` |
| 8 | restart recovery preserves the worktree identity | `TestARecreatedDispatcherFindsTheSameWorktree`, `TestTheProvisionerReusesTheSameWorktree` |
| 9 | a running previous session still blocks unsafe reuse | `TestACrashDuringTheExecutionIsRefusedAndVisible`, `TestARunningOlderEpochSessionBlocksTheWorkspace`, `TestAnUnreachableRuntimeBlocksTheOlderEpochAttempt`, `TestTheRecoveryPassRefusesAMissingWorktree` |
| 10 | execution refuses an environment still configured for the removed containment | `TestContainmentRemovalGate` (every removed variable, the relay socket alone, the acknowledgement, and non-`1` acknowledgements) |

The PR 2 harness (`make test-continuity-controls`) remains at **14/14
load-bearing**. PR 5's evidence suites are unchanged; PR 4's dispatcher
suites were updated to give their fixtures a real source repository and a
base revision, and new worktree tests were added.

---

## 5. Evidence rehearsal (Decision D)

`mpm engineering publish --dry-run --fixture continuity-001 --repo
flowbyte-com/mpm --branch evidence` builds a valid plan (snapshot
`sha256:acfce173ab9e…`, files listed, `dry_run: true`) and writes nothing.
The publisher and `mpm check` suites are green at the baseline. No
`~/.mpm/evidence.json` was created, no GitHub write was made, and the
intended production target (`flowbyte-com/mpm` @ `evidence`) is recorded
here only as an intention. Publication remains an operator authorisation.

---

## 6. What remains blocked

- **Credential (Gate 1.1).** The operator confirms rotation and supplies
  the provider credential through the agent framework's own configuration.
  The removed containment's variables (`MPM_OPENCODE_SIDECAR_KEY`,
  `MPM_OPENCODE_SIDECAR_UPSTREAM`, `MPM_OPENCODE_SIDECAR_MODEL`,
  `MPM_OPENCODE_REVIEWER_KEY`, `MPM_OPENCODE_PLUGIN`) are no longer read;
  an environment that still sets them is refused with a migration
  diagnostic until they are removed (or `MPM_CONTAINMENT_REMOVED_ACK=1` is
  set for one transitional release). No live inference was attempted.
- **Gate 1 re-run.** After this prerequisite merges, Gate 1 must be re-run
  against the new `main`; the other blockers (project creation, worktree,
  evidence target) are addressed by this change or remain operator
  decisions.
- **The worker does not run tests by default (Gate 1.4).** Its shipped
  definition denies the shell; whether an agent may execute commands is
  the agent framework's and the deployment environment's decision. The
  pilot's tests run on the host under operator/CI control, and the pilot
  assessment must not describe them as commands Alpha executed.
