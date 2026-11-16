# The external Charlie review — verdicts and required corrections

> **What this is.** The verdict on the three pieces of work that the
> twenty-three in-session rounds did not produce: an **independent** review
> carried out outside this repository's review history, by the operator's
> external reviewer. It is recorded here so the decisions that followed it
> survive with the evidence they were taken from.
>
> **Scope.** P7 (#245), A1/A2 (extracted into #246), and the A3
> reviewer-isolation architecture. The earlier twenty-three rounds are
> recorded separately, in `review-round-22.md`, `review-round-23.md` and
> `review-round-24-brief-unused.md`.
>
> **Date:** 2026-11-16. **Status:** the verdicts below were **accepted as
> the basis for the next engineering phase**.

---

## 1. The verdicts

| work | verdict | what it means |
|---|---|---|
| **P7** (#245) | **APPROVED** | No dependency on A1/A2. Independently confirmed. The head and its CI were to be verified, and the merge gate prepared — **but not merged without the operator's explicit approval**. |
| **A1/A2** (the provenance + one-snapshot extraction from #244) | **APPROVED** | Extract into a focused PR, preserving all relevant tests. Confirm the newly assembled head builds and passes CI. The prior approval is **supporting evidence, not automatic approval of a newly assembled commit**. |
| **A3** (the reviewer isolation as implemented in #244) | **HOLD** | **Do not merge the existing reviewer-isolation implementation as a completed security boundary.** |

The HOLD on A3 is not a rejection of the approach. It is a refusal to accept
the implementation **as it stands** — the bubblewrap namespace sandbox in
that branch is proved and committed but **inert**: it is not wired into the
reviewer's actual execution path, so a review still runs outside it. What
the shell protection is today is **best-effort**, and the only **enforced**
layer is the tool catalogue. That is stated in `decision-report.md` §2 and
§3 and in `final-report.md`, and it is why the verdict is HOLD rather than
approve-with-notes.

---

## 2. The five corrections to the bubblewrap architecture

The architecture was **accepted subject to corrections**. These are the
conditions, and none of them was implemented in the branch this record
accompanies — they are the work that the replacement PR must do.

### 2.1 Command option injection

`reviewer_bwrap.py run -- <argv>` forwards a caller-supplied argv into the
sandbox invocation. A command vector whose **first element begins with `-`**
can be interpreted by the wrapper as an **option of the wrapper** rather
than as the command to run. The run path must distinguish the wrapper's own
options from the command, and a command vector that begins with an option
must not be able to reach the wrapper's option parser.

The requirement is a **verified command/options separator** — not a
best-effort parse. A reviewer command is attacker-influenced input to the
wrapper, so the separator is the boundary.

### 2.2 Unnecessary `/etc/ssl` exposure

The namespace binds the runtime read-only, which currently includes
`/etc/ssl` — the **trust store**. A review does not need it, and **every
bound path is a path a reviewer can read**. The binding list must be reduced
to what a review actually needs, and `/etc/ssl` removed unless a specific,
stated need requires it.

The principle: the binding list is an **allow-list**, and a path in it is a
path the reviewer can read. Adding a path for convenience is a capability
grant.

### 2.3 Sensitive sandbox base locations

The script refuses to operate when its base resolves inside an MPM runtime
root or inside the operator's home. That check must also cover **the other
locations where a sandbox base must never sit** — and it must be
**unforgeable from inside the namespace**, not merely checked once at
construction. A check made only at construction time does not constrain what
the namespace can do afterwards.

### 2.4 Fail-open reviewer installation

The install of the reviewer definition is a **deployment step**, and it
currently fails in a way that can leave the machine running with a **weaker
or absent** isolation than the repository defines — which is the exact
hazard the installed copy exists to close.

The install must **fail closed**: if the definition cannot be installed, the
installation must not report success. A deployment step that fails open is
worse than one that fails, because it converts an error into a silent
downgrade of the guarantee.

### 2.5 The alternate-capability check

> **Verify that browser, skill loading, MCP, plugin tools and other
> capabilities cannot provide alternate unconfined execution or filesystem
> access.**

Where an installed host capability **cannot be reliably restricted**, the
instruction is explicit: **disable it for reviewers, or explicitly withhold
the security guarantee**. Silently leaving a capability in place while the
documents describe a boundary it does not have is the failure mode this
whole work exists to prevent.

This is why the browser family and the capability loader appear in
`final-report.md` §"Remaining risks" as **open**, rather than as solved by
the namespace: the namespace confines *code execution*, and those two
capabilities can reach the host's filesystem by another route.

---

## 3. The security architecture — capability removal

The accepted shape, in the operator's terms:

- Register a **dedicated sandboxed-exec tool** through the installed OpenCode
  plugin API.
- **Remove the reviewer's raw host-shell capability.**
- **Remove host-side file tools** that bypass the namespace.
- Execute reviewer commands **only inside the enforced bubblewrap boundary**.
- **Fail closed** if the sandbox cannot be established.
- **Never use an agent-controlled environment variable as security
  authority.**

### Why capability removal and not session-based shell-hook rewriting

The alternative considered and rejected: rewriting the command in the
`create.before` hook, identifying the reviewer's command by correlating a
session identifier.

That approach depends on a correlation the host does not provide. The
hook's event carries `command`, `cwd`, `timeout`, `shell` and `env` — but
**no session identifier**. The environment does carry `OPENCODE_SESSION_ID`,
and the environment is **agent-influenceable**. A security authority that
the agent can influence is not an authority.

Capability removal removes the question entirely: there is **exactly one
execution path**, and it is always sandboxed. The reviewer no longer needs
to be *identified* at execution time, because it has no other way to execute.

The same reasoning applies to the host-side file tools. `read`, `write`,
`edit`, `grep` and `glob` run in the **host's process**, outside any
namespace — so they are not confined by the sandbox at all. Denying them and
reaching files **through the sandbox** is what actually removes the
host-side filesystem access.

### What was explicitly closed

The shell-grammar expansion campaign is **closed and is not to be resumed**.
The earlier shell-matcher research is kept as **historical evidence** and as
**optional defence in depth** — it is not the security boundary, and
extending it further is not the work.

This matches `decision-report.md` §4's finding: the ten documented residual
shell routes become **closed by construction** once the namespace is in
force, because the routes exist only in the unsandboxed path.

---

## 4. The convergence gate

**Blocking** — a demonstrated escape **across the actual isolation
boundary** (a command that runs inside the namespace and reaches the
operator's data; a host-side file tool reaching a runtime root); an
incorrect A1/A2 behaviour; a regression in an existing public contract; a
failed required test or CI check; an untruthful security or compatibility
claim.

**Non-blocking unless a concrete failure is demonstrated** — further matcher
grammar coverage; already-documented host/model divergences; harness
refinements that do not touch the boundary; cosmetic findings.

---

## 5. What the correction list requires as proof

The corrections are only credible with **real execution-boundary tests**
using **disposable sentinels**: a command that runs *inside* the namespace
and does not reach the operator's data. The namespace's own contract is
already pinned by twelve tests; what is missing is a test that the
reviewer's **actual** execution path goes through it — and that the
**operator's** path does not.

A test that exercises the sandbox helper directly proves the helper. It does
not prove the boundary, because the defect being guarded against is precisely
that the reviewer's path **does not go through** the helper.

---

## 6. Where this record is used

- **PR #247** (`feat/reviewer-sandbox-isolation`) — **DRAFT, HOLD** — carries
  the correction list as its required work.
- **PR #246** (`feat/skill-packages-p6-provenance-core`) — A1/A2, approved,
  no reviewer-isolation code.
- **PR #245** (`feat/skill-packages-p7`) — P7, approved, merge gate prepared.
- **PR #244** — stays **open and unmerged**, as the historical record of the
  A3 implementation and of the twenty-three reviews it answered.
