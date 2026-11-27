# Continuity 001 — Remove MPM-Managed OS Containment

One focused removal PR. No merge. Subject to independent review.

## 1. Identity

| Field | Value |
|---|---|
| Base | `fedfedf9719a84aebb971594f7a5241cd6f55b0f` (canonical `main`) |
| Head / tree | recorded in the PR body (this file is part of the change) |
| Scope | remove MPM-managed operating-system containment; invoke the agent framework natively |

## 2. What was removed

**Production code**

| Removed | Was |
|---|---|
| `internal/core/opencode/containment.go` | bubblewrap/unshare namespace builder, mount spec, argv |
| `internal/core/opencode/sidecar.go` | inference sidecar (credential holder) |
| `internal/core/opencode/relay.go` | in-namespace inference relay |
| `internal/core/opencode/agentdef.go` + embedded copies | definitions compiled in only to write the sandbox config |
| `opencode/runtime.go` sandbox path | `IsolatedEnv`, `writeSandboxConfig`, `runIsolated`, `Spec` |
| `opencode/review.go` sandbox path | plugin/`sandbox_exec` requirement, isolated review config, default-deny evaluator, session-grant refusal |
| `dispatch/opencode_factory.go` containment | sidecar creation, sidecar/reviewer credentials, MPM relay binary |
| `cmd/mpm/assignment_exec_cmds.go` | sidecar construction and containment credentials |
| `cmd/mpm/main.go` | relay mode dispatch |
| `agent_installation/mpm-opencode/src/sandbox-exec.ts` (+ dist) | the reviewer's `sandbox_exec` tool |
| `agent_installation/mpm-opencode/scripts/reviewer_bwrap.py` | the reviewer's bubblewrap wrapper (1,329 lines) |
| plugin registration / install manifest | sandbox tool and its payload entries |

**Tests and fixtures removed** (their subject no longer exists): the Go
containment argv/execution suites, relay readiness/probe tests, reviewer
permission/auth tests, the factory's containment tests, the plugin's
sandbox tool tests, `test_reviewer_bwrap_sandbox.py`,
`test_reviewer_sandbox_exec_path.py`, `test_reviewer_isolation.py`
(superseded by the installer's own policy check), and the reviewer
permission fixture.

**Net:** 60 files, **+1,189 / −11,050** lines.

## 3. What remains, and why

- **Capability execution domains** (`internal/core/capability`, its
  `BwrapDriver`) are a **different subsystem**: they execute typed
  capability payloads under their own contract, not agents, and are
  untouched. They are not part of the removed agent-execution containment
  and are reported here as retained shared infrastructure.
- **Agent definitions** (`agent_installation/.../templates/agents/`) remain
  as **framework permission definitions**: default-deny, worker =
  file-edit only, reviewer = read tools only. They are not an OS boundary.
- **Orchestration is untouched**: memory/continuity, assignment lifecycle,
  delegation and budgets, R1 ambiguity barrier, R2 structured launch
  diagnostics, immutable attempts, claim/epoch fencing, leases, controlled
  supersession, sealed recovery scope, completion and review evidence,
  operator approval, canonical schema/restore safeguards, and the
  operator-only tool boundary.

## 4. Native execution

`opencode.Runtime` now runs the framework as an ordinary child process:

```
opencode --standalone run --session <id> --agent <a> [--model <m>] \
    --title <t> <prompt>
```

- working directory: the **authorised workspace**;
- environment: the operator's own (`os.Environ()`);
- reads (`session.list`, `message.list`, `debug agents`): the same
  environment, MPM's state directory as cwd;
- agent verification: **existence only** — tool permissions are the
  framework's;
- no MPM binary, no relay, no sidecar socket, no credential held by MPM.

The reviewer path keeps the exact-revision snapshot, host-captured
findings, assistant-authored-only transcription and model verification.

## 5. Configuration migration

The removed subsystem's environment variables are no longer read:
`MPM_OPENCODE_SIDECAR_KEY`, `MPM_OPENCODE_SIDECAR_UPSTREAM`,
`MPM_OPENCODE_SIDECAR_MODEL`, `MPM_OPENCODE_REVIEWER_KEY`,
`MPM_OPENCODE_PLUGIN`.

**Execution refuses while any of them is set**, naming them and explaining
that the boundary was removed. The operator removes them (the containment
they configured no longer exists) or sets
`MPM_CONTAINMENT_REMOVED_ACK=1` for one transitional release. The
acknowledgement changes no behaviour and is intended to be removed after
one upgrade window. The gate is in `dispatch.CheckContainmentRemoval`,
enforced by the dispatcher factory and both explicit execute verbs.

## 6. Revised threat model

MPM operates within a **trusted deployment environment**. An agent with
unrestricted same-user shell access may reach the same files, databases,
credentials and executables as the operator. MPM does **not** protect
against a malicious agent with equivalent operating-system permissions.
Agent-framework permissions and deployment-level isolation (container,
dedicated user, virtual machine) are the operator's responsibility.
Application-level authorisation — operator-only verbs absent from the MCP
surface, claim fencing, budgets, approval — is unchanged and is not
described as OS privilege separation.

## 7. Continuity 001 contract amendment

[`continuity-001-containment-removal-amendment.md`](continuity-001-containment-removal-amendment.md):

- the pilot contract states **trusted native execution**;
- MPM provides **no OS-level containment boundary**;
- **T49/T50 are RETIRED**, not passed: they tested the removed subsystem.
  Their assertions and historical results are preserved in the review
  record; they must not be cited as evidence about native execution;
- **T51** and the same-UID authority limitation remain accurate;
- every recovery/lifecycle/authorisation requirement remains mandatory;
- **credential closeout remains mandatory**;
- live recovery and provider execution still require separate operator
  authorisation; the amendment authorises neither.

## 8. Verification

New/updated tests (synthetic frameworks, disposable databases):

- **native worker lifecycle**: `internal/core/assignment/native_exec_test.go`
  drives the REAL runtime through claim → start → attempt → completion,
  and asserts the ledger;
- **native launch failure**: ambiguity is preserved and the R2 diagnostic
  carries the classification and bounded-output digest, never raw output;
- **fencing**: a stale epoch is refused before the framework runs;
- **native reviewer**: `internal/core/opencode/native_review_test.go` runs
  a review with **no plugin**, asserting the exact-revision snapshot,
  ambient environment, captured findings and written evidence;
- **no prerequisites**: `TestRuntimeNeedsNoContainmentPrerequisites`
  constructs the runtime with only a state directory and a binary;
- **migration gate**: obsolete containment configuration is refused, and
  only `MPM_ACK=1` unblocks it;
- **policy check**: the reviewer installer enforces default-deny + read
  allows + state-changing denies.

Demonstrated without bubblewrap, unprivileged user namespaces, MPM network
isolation or a relay: these tests run on a host whose user-namespace
policy **denies** the removed launcher, and they pass.

Gates: `make build`, `mpm-lint --gate`, `make test` end-to-end,
`make test-scripts`, `make test-testenv`, `make docs-check`, race suites,
PR2 negative controls (14/14), agent installation (488 tests), and CI.
Exact results in the PR body.

## 9. Remaining limitations

- **T49/T50 are retired, not passed.** No MPM-managed boundary exists to
  test.
- **Isolation is the operator's.** MPM neither manages nor certifies an
  external container, dedicated user or VM.
- The capability executor's own bwrap domains are retained and outside
  this change.

## 10. Preservation

Frozen pilot untouched; `/home/v/src/mpm` unmodified; no merge, live
migration, supersession, inference, production installation, dispatcher
enablement, host security-policy change, or Skills P8 work.
