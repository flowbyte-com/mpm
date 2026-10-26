# G — Parallel launch gate: readiness and handoff requirements for Sessions B and C

> **Status: ready to launch.** The gate conditions are enumerated in §2 and
> every one of them is **satisfied** as of 2026-10-26. Session A does **not**
> launch B and C; this document states what they must do when launched.
>
> Authority: Session A alone merges to `main` and deploys to `~/.mpm`
> ([`coordination.md`](coordination.md) §1).

---

## 1. The corrected contract is the input, not the agreement

The revised contract is [`event-contract.md`](event-contract.md). It is a
**proposal**. B and C must acknowledge the **minimum shared interface**
(§7 of that document, items M1–M7) explicitly. A disagreement with an
implementation choice (§6 of that document) is **not** a blocking objection;
a disagreement with an M-item **is**.

Nothing in §4 or §5 of that document needs agreement before investigation
starts. That is deliberate: the sprint's bottleneck is knowing what the host
can actually observe, and only Session C can answer that.

---

## 2. Gate conditions — status

| # | Condition | Status | Evidence |
|---|---|---|---|
| G1 | CI on `main` is trustworthy: the full-suite sweep can fail, and every required check is green on the merged commit | ✅ **met** | #104 merged as `b5b18401974ee559a485cda20abf8f7779f11bf2`; the "Full test suite" job now runs under `shell: bash` (pipefail); run `40332886651` green on head `fa80f48`, run `40337045142` green on `main` = `b5b1840`, and run `40344867185` green on the merge of this document set |
| G2 | The shared event contract exists in a corrected, reviewable form and its agreement surface is explicit | ✅ **met** | [`event-contract.md`](event-contract.md), revised 2026-10-26 |
| G3 | The shared dogfooding workspace exists, is isolated from the live install, and is verified | ✅ **met** | [`dogfooding-runbook.md`](dogfooding-runbook.md) §8; `~/.mpm-dogfood` |
| G4 | The three worktrees sit on one verified base with clean, independent branches | ✅ **met** | `~/src/mpm` = `~/src/mpm-core` = `~/src/mpm-opencode` = the tip of `main`; branches `integration/session-a`, `dev/session-b-core`, `dev/session-c-opencode`; all clean |
| G5 | The correction sequence for the remaining PRs is known and the one known schema conflict is resolved | ✅ **met** | [`integration-matrix.md`](integration-matrix.md) §6; both conflicting index lines are present in `origin/main`'s `internal/core/schema.go` (lines 1007 and 1011) |
| G6 | No session other than A may merge or deploy | ✅ **met** | [`coordination.md`](coordination.md) §1, §7 |
| G7 | B and C have a defined first investigation with a defined deliverable | ✅ **met** | §3 and §4 below |

**One condition is deliberately NOT a gate:** the event contract's
**agreement** is not a condition for *launch*. It is a condition for
*dependent implementation*. B and C may launch, read, audit and measure
immediately.

---

## 3. Session B — first investigation

**Directory:** `~/src/mpm-core` (branch `dev/session-b-core`, base = the tip of `main`).

### 3.1 What to audit

The existing implementations, before proposing anything new:

| Area | Where | The question |
|---|---|---|
| Critic | `internal/critic/` (`audit.go`, `hunts.go`) | What does the critic already detect, and from which evidence? Where could a retrospective "we missed a memory opportunity" check plug in? |
| Scheduler | `internal/scheduler/` (`dispatch.go`, `cascade_drain.go`, `active_uptime.go`, `drill_handler.go`, `cascade_wake_reconcile_handler.go`, `cron_retention_test.go`) | What periodic work already runs, on what cadence, against which workspace? |
| Evidence | `internal/core/evidence*.go` (`evidence.go`, `evidence_store.go`, `evidence_migration.go`, `evidence_reference_url.go`) | What is the real shape of an evidence row, and what would attaching a lifecycle observation require? |
| Ingestion | `cmd/mpm/ingest.go`, `internal/core/migrate_md.go`, `internal/core/raw_memory_shred_linkage.go` | What already promotes an observation into a memory, and by what path? |
| Provenance | `internal/core/provenance*.go` (`provenance.go`, `cascade_provenance.go`) | What does provenance stamp today, and can it carry an event reference? |
| Confidence | `internal/core/confidence*.go` | How are confidences computed and decayed, and where would a proposal enter? |
| Telemetry | `internal/telemetry/` | What does the cost-frame substrate already do, and is any of it reusable for lifecycle observations? (Expect: no — it is a measurement transport.) |
| Trusted-knowledge writers | `internal/core/skill*.go`, `lesson*.go`, `decision*.go` | What are the promotion entry points, and what review do they require? |

### 3.2 The question to answer

> **Which existing facilities already cover "evidence-backed retrospective
> detection of missed memory opportunities", and what is the smallest
> honest gap?**

Concretely:

1. **Does a gap actually exist?** If the critic already receives the
   observations it needs, say so — and the correct outcome is "no new event
   framework". That is a legitimate and welcome result.
2. If a gap exists, **name the existing facility that is closest** and say
   precisely why it is inadequate. A new framework requires that argument.
3. **Do not** introduce a new event or staging framework without
   demonstrating that the existing facilities are inadequate. The contract's
   §2.6 records the storage gap; that is the *storage* question. The
   *detection* question is B's.

### 3.3 Deliverable

A written report, in `~/src/mpm-core`, addressed to Session A:

* The audit table from §3.1, with a verdict per row: **works**,
  **partially works**, **missing**.
* The answer to §3.2, with the evidence.
* Any change to the contract's §6 choices that B's audit implies — stated as
  a proposed change to a specific MSI item (M1–M7), so A can adjudicate it.
* **No implementation.** B's first deliverable is a report, not a PR.

### 3.4 What B must not do

* Merge to `main`, or deploy to `~/.mpm`.
* Edit `~/src/mpm` or `~/src/mpm-opencode`.
* Add a `SafeMigrations` entry to `internal/core/schema.go` before A agrees
  the implementation PR — that file is the sprint's highest-collision file
  and two sessions adding migrations concurrently is the top integration risk.
* Touch the live database at `~/.mpm`.

---

## 4. Session C — first investigation

**Directory:** `~/src/mpm-opencode` (branch `dev/session-c-opencode`, base = the tip of `main`).

### 4.1 What to inspect — the installed host, not the repository's declarations

This is the correction the contract's §5 makes explicit: the repository's
`agent_installation/mpm-opencode/src/opencode-sdk.d.ts` is a **hand-written
curated subset** of the host's API. It is **not** evidence about what the
installed host exposes.

What is actually installed here:

| Thing | Fact |
|---|---|
| Host | `opencode v2.1.4` — `~/.opencode/bin/opencode`, a compiled binary |
| Host plugin loading | `~/.config/opencode/opencode.jsonc` loads `file:///home/v/.mpm/agent_installation/mpm-opencode` |
| `@opencode/plugin` in `node_modules` | a **probe stub**, `"version": "0.0.0-probe"`, containing no type information |
| `@opencode-ai/plugin@1.19.0` in `node_modules` | the **v1** SDK — its `Hooks` interface is informative but is not the v2 host's surface |

So C must establish the v2 host's real surface — by probing the running host
(the `event` hook, the compaction hooks, `tool.execute.before`/`.after`, the
plugin input's `directory`/`worktree`/`project`), and by recording what the
host actually delivers, not what a declaration promises.

### 4.2 The classification C must produce

For every candidate lifecycle event in the contract's §4:

| Classification | Meaning |
|---|---|
| **Directly observable** | the host delivers it; C can read it from a real hook |
| **Reliably inferable** | it can be derived from what the host does deliver, by a rule that is stated and testable |
| **Unobservable / unsupported** | the host provides nothing, in this version or in principle |

C must also report, for each observable event:

* the **host event key** it carries, if any — this decides `strict` versus
  `best_effort` idempotency for that type (contract §3.5);
* whether the payload contains anything the `meta` allowlist (contract §3.8)
  would have to drop;
* whether the host provides a **stable project identity**, which would
  supersede the git-remote derivation in contract §3.3.

### 4.3 The measurement C must produce

The existing behavioural baseline, measured rather than assumed:

| Measurement | How |
|---|---|
| **Discovery** | how a new agent finds the MPM tools — what the tool descriptions, `mpm_help`, and the wake context actually surface |
| **Pointer resolution** | how often an agent can go from a pointer in the wake context to the artifact it names |
| **Token overhead** | what the wake context and the tool surface cost per turn |
| **Tool-selection accuracy** | how often the agent picks the right MPM tool for a task it should use MPM for |
| **Specialised-capability use** | whether the agent uses the capability that fits (for example `mpm_context read_wake_context` rather than an ad-hoc `mpm_memory query`) |

**Retain the current tool surface unless a concrete behavioural failure
justifies a targeted improvement.** A measured failure is the justification;
a preference is not.

### 4.4 Deliverable

A written report, in `~/src/mpm-opencode`, addressed to Session A:

* The host-surface findings — replacing the contract's §5 table with measured
  facts.
* The per-event classification from §4.2.
* The measurements from §4.3.
* Any change to the contract's §6.7 (`meta` allowlist), §6.8 (byte ceilings)
  or §4 (the vocabulary) that C's findings imply — stated as a proposed
  change to a specific MSI item, so A can adjudicate it.
* **No implementation.** C's first deliverable is a report, not a PR.

### 4.5 What C must not do

* Merge to `main`, or deploy to `~/.mpm`.
* Edit `~/src/mpm` or `~/src/mpm-core`.
* Change the tool surface, the tool registry, `package.json` or the lockfile
  before A agrees — the lockfile is pinned by a CI contract test, and the
  tool registry is a shared interface between B, C and the MCP server
  ([`coordination.md`](coordination.md) §2).
* Modify the installed plugin payload at `~/.mpm/agent_installation/`. That is
  a deployment target, changed only by `install.sh` at a milestone.

---

## 5. The launch gate — what happens in the first hour

Each session's **first action** after launch:

1. **Read** `docs/dev/event-contract.md` in full, from the merged commit
   (not from a branch, not from a cached copy).
2. **Acknowledge** the MSI: record, in the shared dogfooding workspace, an
   explicit acknowledgement of items M1–M7 — accepted or objected to, with a
   reason for any objection.
3. **Identify any blocking objection.** A blocking objection is a
   disagreement with an M-item, or a finding that makes an M-item
   unimplementable as written.
4. **Verify the assigned scope**: confirm the worktree, the branch, the base
   commit, and the ownership boundaries in §3.4 / §4.5.

**While a blocking objection is unresolved, no dependent event implementation
may proceed.** Not partially, not "behind a flag", not on a side branch. The
objection is resolved by A adjudicating it against the contract's evidence,
or by B and C agreeing a change to the contract, which requires a version
bump if it changes the frozen 0.1 field set.

Non-blocking disagreements (§6 implementation choices) do not stop work.

### 5.1 The environments B and C must use

```bash
export MPM_WORKSPACE="$HOME/.mpm-dogfood"
export MPM_DB_PATH="$HOME/.mpm-dogfood/src/db/mpm.db"
export MPM_BINARY="$HOME/src/mpm/.build/bin/mpm"
```

* `MPM_WORKSPACE` — the shared workspace. **Not** `~/.mpm`. The plugin pins
  this for every MPM subprocess it spawns (verified: `withWorkspace()` in
  `agent_installation/mpm-opencode/src/workspace.ts`).
* `MPM_DB_PATH` — pinned because `mpm-critic` and `mpm-scheduler` honour it
  (verified). `mpm` and `mpm-mcp` ignore it.
* `MPM_BINARY` — the development build at `b5b1840`. Without it the plugin
  resolves `$HOME/.mpm/bin/mpm`, which predates every one of the fifteen
  corrective PRs.

**Do not** point any experiment at `~/.mpm`. The live install holds real
reference material and is not a test bench.

### 5.2 Scratch work

Every scratch or destructive test gets its own `HOME`, `MPM_WORKSPACE` and
`MPM_DB_PATH` ([`dogfooding-runbook.md`](dogfooding-runbook.md) §5). A
`mktemp -d` scratch area is disposable; `~/.mpm-dogfood` is shared knowledge
and is not scratch.

---

## 6. What A continues to do while B and C investigate

A remains the integration and release authority and keeps the sprint moving
without waiting for B and C:

1. **Watch CI on `main`** and keep the release gate honest.
2. **Refresh and execute the corrective-PR integration sequence**
   ([`integration-matrix.md`](integration-matrix.md)).
3. **Adjudicate** B's and C's proposed contract changes against the evidence.
4. **Prepare the single minimal implementation PR** for whatever the audits
   show is genuinely missing — one PR, with its contract tests, landing in
   `internal/core/schema.go` only if the gap is a storage gap.
5. **Hold the deployment gate.** No deployment to `~/.mpm` until an accepted
   integration milestone has passed the full verification gate
   ([`dogfooding-runbook.md`](dogfooding-runbook.md) §6).

---

## 7. Summary

| Question | Answer |
|---|---|
| Are the conditions to start Sessions B and C satisfied? | **Yes.** G1–G7 are met. |
| Is the event contract agreed? | **No** — it is a proposal. B and C must acknowledge M1–M7. That blocks *dependent implementation*, not *investigation*. |
| May B and C merge? | **No.** Only A merges to `main` and deploys. |
| What do B and C deliver first? | A **report**, not a PR (§3.3, §4.4). |
| Where do B and C work? | `~/src/mpm-core` and `~/src/mpm-opencode`, on `dev/session-b-core` and `dev/session-c-opencode`, at the tip of `main`. |
| Where do their findings go? | The shared dogfooding workspace `~/.mpm-dogfood` — the reasoning channel — and the coordination ledger in [`coordination.md`](coordination.md). |
