# Continuity 001 — Containment Removal Amendment

**Status:** proposed, subject to independent review. This amendment does
**not** authorise a live pilot, a deployment, or provider execution.

## 1. What changed

MPM-managed operating-system containment is **removed**. The following
are no longer part of MPM:

- bubblewrap mount namespaces and the namespace launcher;
- unprivileged user-namespace creation and the unshared network namespace;
- containment-specific mounts, environment scrubbing and credential
  isolation;
- the inference sidecar and its relay;
- the reviewer's `sandbox_exec` execution path and `reviewer_bwrap.py`;
- containment prerequisites in the installer and the adapter
  configuration;
- AppArmor compatibility requirements.

MPM now invokes the configured agent framework through its normal
supported interface, as an ordinary child process with the operator's
environment. Operators may still run MPM and its agents inside an
externally managed container, a dedicated user account or a virtual
machine; MPM neither manages nor certifies that isolation.

## 2. Why

MPM is an agent orchestration, memory and continuity system. Its
application-level controls — operator authority, claim fencing, budgets,
approval, recovery scope, evidence — are accountability mechanisms, not an
operating-system privilege boundary. An agent with the operator's
operating-system permissions can reach what the operator can reach.
Presenting a process sandbox as MPM's security model conflated the two and
made native operation depend on host policy MPM does not own.

## 3. Responsibility split (threat model)

| Layer | Owner |
|---|---|
| Memory, continuity, assignments, delegation, budgets, execution state, recovery, approvals, audit | MPM |
| Tool permissions and agent execution policy | the agent framework |
| Filesystem, network, credential and privilege isolation | the deployment environment |

MPM does **not** protect against a malicious same-user agent. Native
execution is intended for trusted deployments.

## 4. Test consequences — T49/T50 retired, not passed

- **T49** (contained worker cannot reach operator CLI/host FS) and
  **T50** (contained reviewer cannot reach DB/home) tested the REMOVED
  subsystem. They are **retired**, not passed and not waived: there is no
  MPM-managed containment for them to exercise.
- Their assertions and historical results are preserved in the review
  record and in `docs/dev/reviewer-isolation-evidence-2026-11-16/`. They
  must not be cited as evidence about native execution.
- **T51** (same-UID authority limitation) remains the accurate statement
  of native execution's boundary and stays documented.
- All other requirements remain mandatory under native execution:
  R1 ambiguity barrier, R2 structured launch diagnostics, immutable
  attempts, tree-scoped authorisation, claim/epoch fencing, leases,
  budgets, supersession lineage, sealed recovery scope, the operator-only
  recovery service boundary, completion and review evidence, C1/C2
  temporal validation, schema and restore safeguards, approval and audit.

## 5. Execution contract for the proposed second pilot

- The pilot uses **trusted native execution**.
- MPM provides **no OS-level containment boundary**; the agent framework
  and deployment environment own security policy.
- The exact-revision review snapshot, host-captured findings, model
  verification, claim fencing and every recovery safeguard are unchanged.
- **Credential closeout remains mandatory.**
- **Live recovery and provider execution still require separate operator
  authorisation.** Nothing here authorises either.
- The pre-pilot gate that previously required T49/T50 is replaced by: a
  documented deployment-isolation decision by the operator, and a
  verification run of the native worker and reviewer paths against the
  pilot-local runtime.

## 6. Configuration migration

Environments configured for the removed containment still export its
variables. MPM **refuses to execute** while any of
`MPM_OPENCODE_SIDECAR_KEY`, `MPM_OPENCODE_SIDECAR_UPSTREAM`,
`MPM_OPENCODE_SIDECAR_MODEL`, `MPM_OPENCODE_REVIEWER_KEY` or
`MPM_OPENCODE_PLUGIN` is set, and reports exactly which ones. Remove them —
the containment they configured no longer exists — or set
`MPM_CONTAINMENT_REMOVED_ACK=1` to acknowledge the removal for this
transitional release. The acknowledgement changes no behaviour and is
intended to be removed after one upgrade window.
