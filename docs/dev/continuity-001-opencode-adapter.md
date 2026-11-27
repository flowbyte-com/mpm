# Continuity 001, PR 3 — the OpenCode adapter and the reviewer integration

> **Superseded in part.** MPM-managed operating-system containment
> (bubblewrap, user/network namespaces, the inference sidecar and the
> reviewer `sandbox_exec` path) has been **removed**. Sections 5 and 6
> below record what was withdrawn; the amendment is
> [`continuity-001-containment-removal-amendment.md`](continuity-001-containment-removal-amendment.md).
> Execution is now native: the agent framework and the deployment
> environment own isolation.


**Status:** design of record for PR 3 of six, issue #258.
**Prerequisite:** PR #260, merged as `ccf5895c`; its accepted head `9bd04b7c` is
an ancestor of `main`.

---

## 1. What PR 3 is, and what it is not

PR 1 was storage. PR 2 was the coordination interface. **PR 3 connects the two
to a real runtime**: an assignment that the operator explicitly selected is
executed in a native OpenCode session, recovered after an interruption, and
reviewed independently.

Three sentences bound it:

* **MPM coordinates. OpenCode executes. HAL supervises.**
* MPM does **not** implement inference, model selection, tool execution or an
  independent conversation loop.
* The adapter is a **translator** between MPM assignment state and OpenCode
  runtime state. When it has to choose, it chooses to refuse.

Not in this PR: the dispatcher, polling, automatic root creation, aggregate
budgets, the GitHub evidence publisher, deployment, or any operator merge.

### The four pieces

| | what it does | where |
|---|---|---|
| the mapping | one row per execution attempt, written **before** the launch | `internal/core/migration_agent_task_sessions.go` |
| the adapter | validates, reconciles, launches, records | `internal/core/assignment/execute.go` |
| the runtime | the only code that talks to OpenCode | `internal/core/opencode/` |
| ~~the sidecar~~ | **removed** with the containment; the framework holds its own credentials | — |

---

## 2. The measured host behaviour this design is built on

Every claim below was measured on OpenCode **v2.3.2** on the operator's host,
in an isolated configuration, rather than taken from documentation.

### 2.1 `session.create` is not idempotent; `run --session` is

`session.create` mints its own id on every call, so an interruption between
creating a session and persisting its identity is genuinely ambiguous.

`opencode run --session <id>` creates the session **with the caller's id** and
continues it if it already exists. Measured: a second `run --session` with the
same id continued the same session (`SECOND` replied in-context), and the
session count stayed at one.

So the adapter **mints the id itself, persists it, and only then launches**.
That is the whole recovery story.

### 2.2 A session's outcome is observable; its existence proves nothing

`session.list` reports `outcome` (`succeeded` / `failed`, absent while
running), `agent`, `model`, `location.directory`, `parentID` and the session's
own `permissions`. `session.message.list` reports the message stream,
including a terminal `{"type":"idle","outcome":…,"error":…}` envelope.

The adapter records success only from `outcome == "succeeded"`. An absent
outcome is recorded as **`ambiguous`**, never folded into a boolean.

### 2.3 The workspace comes from the process CWD

`session.create`'s `directory` parameter is **ignored** — measured: a create
with `directory: …/subdir` still reported `location.directory` as the server's
CWD. Running the CLI from a different directory *does* move the session.

So the adapter launches from the authorised worktree and **verifies the
reported `location.directory`** against it before believing any result.

### 2.4 An unknown agent is a RUNTIME failure, not a fallback

**This corrects an earlier MPM memory**, which stated that the host resolves an
unknown agent name by falling back to a default agent that inherits the
parent's tool surface. That is not what v2.3.2 does.

Measured: `session.create` accepted `agent: "no-such-agent-xyz"` verbatim, and
the failure appeared only when the run started —
`Session.AgentNotFoundError`, `outcome: failed`, and an `idle` envelope
carrying `{"type":"unknown","message":"Agent not found: …"}`.

Consequences, and they are load-bearing:

* an unknown agent name is **refused before a session exists**, by an explicit
  allow-list, because relying on the host would create a session and burn a
  run before learning the name was wrong;
* the fallback behaviour an operator might assume is **not** a safety net, so
  nothing here depends on it.

> Recorded here, in repository documentation, as the operator directed. The
> production substrate was not modified.

### 2.5 A session-level permission grant overrides an agent-level deny

`session.create` **and** `session.update` accept a `permissions` array. A
grant of `[{"action":"shell","resource":"*","effect":"allow"}]` on an
`mpm-reviewer` session produced a session where `shell` was present and
**executed on the host filesystem** — `uid=1000`, groups including `sudo`,
host root visible, `/sandbox` absent (i.e. outside the namespace). The
control, `permissions: null`, saw only `sandbox_exec` and `skill` and **no
`shell`**.

The mechanism is the host's **last-match-wins** resolution: the agent's own
wildcard deny beats the host's prepended global allow, but a session-level
grant lands *after* the agent rules and therefore wins over them.

Consequences:

* **A permission declaration is not an operating-system boundary.** In the
  withdrawn design the namespace was; the namespace is gone, so the
  framework's permission configuration plus the deployment environment are
  what constrains a session.
* **The adapter neither refuses nor grants on a session whose
  `permissions` array is non-empty.** The grant is the agent framework's
  own policy and is recorded as reported; MPM verifies identity, workspace
  and model, not tool permissions.
* The worker's agent definition remains a *permission narrowing* owned by
  the framework.

---

## 3. The execution mapping

One additive, local-only table. PR 1's hash-pinned DDL is untouched, and no
approval or provenance column is repurposed as session storage.

```
agent_task_sessions(id, task_id, attempt, claim_epoch, runtime,
                    intended_agent, native_session_id, correlation_ref,
                    workspace_id, workspace_path, launch_state,
                    runtime_outcome, last_error, created_at, updated_at)
```

The properties recovery depends on are enforced by the **database**, not by
convention:

| rule | why |
|---|---|
| `native_session_id` is `NOT NULL` and **write-once** | it is what reconciliation matches on; a rewrite would point the record at a session that never ran |
| an attempt's `claim_epoch` must be the assignment's **current** claim | a reclaimed worker cannot even record an intent |
| attempts are **consecutive** per task | a retry is visibly a retry, not a silent second execution |
| `(runtime, native_session_id)` is **unique** | two assignments cannot claim one runtime object |
| at most one **non-terminal** attempt per `(task_id, claim_epoch)` | two concurrent callers must not both obtain permission to launch; a service-level check cannot close that window, because its read and its write are not one critical section |
| `launch_state` moves **forward only** | a failed attempt cannot be relabelled a success |
| the row is **not deletable** | an executed assignment cannot be made to look unexecuted |

`agent_task_events` still carries the durable history. **No new event type was
added** — the ledger's vocabulary is PR 1's closed set.

The single-flight index is scoped to the **claim epoch** on purpose. A worker
whose lease was reclaimed leaves a stale intent behind, and an index on
`(task_id)` alone would then block every future attempt of that assignment
forever.

### The reviewer cannot be launched twice

The evidence check that refuses a completed review lives **inside the
transaction that persists the launch intent**, not in a preliminary read
before it. A preliminary read is in a different transaction, so a concurrent
caller can pass it, wait for another review to complete, and then persist its
intent anyway — the partial index does not help, because it covers only
non-terminal attempts and the winner's attempt goes terminal within
milliseconds.

So the authoritative row is read in the same transaction as the write: the
refusal and the intent are one critical section, and the refusal happens
**before** a second OpenCode session can be launched. A review that *failed*
recorded no evidence and stays retryable.

### The launch-state graph

```
intent ──► launched ──► running ──► completed
   │            │           │    └─► failed
   │            │           │    └─► ambiguous
   └────────────┴───────────┴────► cancelled
   └────────► completed / failed / ambiguous        (a blocking launch
                                                     may reach a terminal
                                                     state directly)
```

### An ambiguous attempt is a hard barrier

`ambiguous` is terminal for the ATTEMPT and blocking for the TASK. The
adapter refuses a new launch for any non-record-only `Execute` while an
ambiguous attempt exists anywhere in the task's history — not merely the
most recent one, and regardless of an expired lease, a newer claim epoch
or an absent native session. The durable attempt is never relabelled
`failed`, `cancelled` or `completed`; only an explicit operator
reconciliation outside the adapter may clear it. The dispatcher checks the
same history before it claims and reports `recovery_required`.

### Launch failure diagnostics

A launch without trustworthy native session state remains **ambiguous**.
`Runtime.Launch` returns a `LaunchError` carrying a safe classification:
command start vs exit, the exit code, the session-inspection class, whether
a state was observed, whether inference remains unknown, and the bounded
output's byte count and SHA-256. It never carries raw stdout or stderr;
those may contain prompt text or a secret. The adapter records that
structured reason instead of replacing it with `the runtime produced no
session state`, and raw output is never written to the assignment
database, dispatcher report, prompt or public diagnostic.

`intent → completed` is legal because the adapter's launch is **blocking**: it
cannot observe the intermediate states, and pretending otherwise would be a
record of events that were never seen.

---

## 4. The recovery contract

The adapter's order is fixed, and step 3 is what stops a retry from silently
duplicating work:

1. validate the assignment (role, epoch, lease, state, the tree's permitted
   operations);
2. read the task's attempts;
3. decide **from evidence**;
4. only then mint an id, persist the intent, and launch.

| the record says | the runtime says | the adapter |
|---|---|---|
| no attempts | — | start a new attempt |
| `intent` | session not found | **resume** — nothing was created, so reusing the id cannot duplicate |
| `intent` | session exists | **resume** — the same `run --session` continues it |
| `intent` | runtime unreachable | **ambiguous**, and stop |
| `launched` / `running` | no outcome yet | **refuse** — a running attempt is not a licence for a second |
| `launched` / `running` | session lost | **ambiguous**, and stop |
| terminal | — | a **new** attempt, with its own number |

### What reuse does not prove

A reused session id does **not** prove that repeating a prompt has no effects.
So the adapter never re-prompts a session it finds running or terminal; it
either records what the runtime said or refuses. Where OpenCode cannot offer a
fully idempotent creation operation, the adapter **records the ambiguity and
fails closed** rather than claiming exactly-once execution.

### The fence

Every attempt write presents the attempt's epoch **and** requires that epoch
still to be the assignment's current claim, in the same statement:

```sql
WHERE id = ? AND claim_epoch = ?
  AND EXISTS (SELECT 1 FROM agent_tasks
               WHERE id = agent_task_sessions.task_id AND claim_epoch = ?)
```

A stale worker matches no row. Its attempt record stays truthful; the
assignment simply does not move.

---

## 5. Runtime containment — REMOVED

The containment this section described has been removed: the bubblewrap
mount namespace, the unshared network namespace, the containment mounts,
the environment scrubbing and the credential isolation are gone, along
with the `reviewer_bwrap.py` helper and the reviewer's `sandbox_exec`
tool.

MPM now launches the agent framework as an ordinary child process in the
authorised workspace, with the operator's environment. Tool permissions
come from the agent framework's configuration; filesystem, network and
credential isolation are the deployment environment's responsibility. The
shipped agent templates remain as FRAMEWORK permission definitions
(`agent_installation/mpm-opencode/templates/agents/`), not as an
operating-system boundary.

## 6. The inference sidecar — REMOVED

The sidecar and its relay were how a network-isolated worker reached a
provider. Without that isolation they have no purpose: the framework
reads its own provider configuration and credentials from the operator's
environment. `MPM_OPENCODE_SIDECAR_KEY`, `MPM_OPENCODE_SIDECAR_UPSTREAM`,
`MPM_OPENCODE_SIDECAR_MODEL`, `MPM_OPENCODE_REVIEWER_KEY` and
`MPM_OPENCODE_PLUGIN` are no longer read; an environment that still sets
them is refused with a migration diagnostic until the operator removes
them (or sets `MPM_CONTAINMENT_REMOVED_ACK=1` for one transitional
release).

The reviewer still has its OWN model, and the session's reported model is
still verified against it. That is orchestration, not isolation.

## 7. The reviewer integration

The reviewer path is **orchestration**, and it is unchanged:

* `mpm-reviewer` is still the pinned reviewer definition, and the shipped
  template is still default-deny — now with the READ tools (`read`,
  `grep`, `glob`, `list`) as its only allowed actions. That is a FRAMEWORK
  permission definition, not an operating-system boundary;
* the reviewer still has its own model, and the session's reported model
  is still verified against it;
* the review still runs in a disposable checkout of the EXACT revision,
  and the reviewed revision is read from that snapshot by the HOST;
* the findings are still captured from the native message stream, and only
  assistant-authored text can be transcribed as a verdict;
* the reviewer still never runs in the worker's workspace or under the
  worker's agent definition.

What is gone is the claim that the reviewer's tool permissions are an OS
boundary. An agent with the operator's operating-system permissions can
reach what the operator can reach; the deployment environment is where
that is controlled.

### Evidence capture

`reviewed_sha` and `findings_sha256` exist in PR 1's schema and nothing in PR 1
or PR 2 ever wrote them after creation. PR 3 adds **one narrowly scoped, fenced
write**:

* it requires a **reviewer**, `in_progress`, at the presented epoch;
* it writes **only** the reviewed revision, the findings digest and the
  evidence pointer;
* it is **absent from the tool registry** — reachable only through
  `mpm assignment capture-review-evidence`, which the adapter drives.

It records evidence, and the host then transcribes the reviewer's own
terminal verdict from the assistant-authored text into the same fenced
transaction. The transaction covers the EVIDENCE COLUMNS and the verdict;
the on-disk transcript file is written first and retained for diagnosis even
when no verdict is recorded, so it is an artifact, not a second atomic fact. The verdict is not an independent host decision: the text must
be the reviewer's final line, exactly `VERDICT: APPROVED` or
`VERDICT: CHANGES REQUESTED`; prompts, tool output and diagnostics are not
sources. The subject moves only through the existing reviewer transition,
and a completed review is still not operator approval.

### What counts as findings

The captured stream is a **faithful record**: the submitted prompt, the tool
activity and the runtime's diagnostics all appear, because a review that
refused to run is not a review that found nothing.

The *authorship* gate is a separate question, and it is stricter than "the
transcript is not empty". A tool part renders as `[tool bash]`, which
is not whitespace — so a session where the reviewer only **ran tools** would
otherwise pass as a review. A review counts only when the **assistant wrote
text**: a prompt-only session, an empty assistant turn, tool activity alone
and a diagnostics-only session are all refused, and a message carrying both
tool activity and substantive prose satisfies the gate.

The refusal happens where the stream is read, so it is part of the completion
contract rather than a later inspection: a session that produced no
assistant-authored text is not a completed review, however it exited.

---

## 8. What PR 3 does not do

| | deferred to |
|---|---|
| the dispatcher, polling, recovery daemon, aggregate budgets | PR 4 |
| the GitHub evidence contract, read-only `check` discovery | PR 5 |
| the first real assignment (#253) | PR 6 |
| a command capability for the worker | an agent-framework and deployment decision (the shipped definition denies the shell; MPM does not sandbox) |
| project administration | **not PR 3** — an explicitly seeded fixture is used in the disposable integration database, and a separate operator-only project-creation operation is a prerequisite to PR 6. **Manual SQL insertion is not the supported production workflow.** |

---

## 9. Known limitations, stated plainly

1. **Exactly-once execution is not claimed.** OpenCode has no idempotent
   creation primitive. The adapter narrows the window to "intent persisted,
   launch not observed" and records that as `ambiguous`.
2. **MPM holds no provider credential.** The framework reads its own
   configuration from the operator's environment; MPM neither stores nor
   isolates credentials.
3. **The shipped worker definition does not execute commands.** That is a
   framework permission decision (the operator can change it), not an MPM
   boundary; whether commands may run is the agent framework's and the
   deployment environment's policy.
4. **Isolation is the deployment environment's.** A container, a dedicated
   user account or a virtual machine is where filesystem, network,
   credential and privilege isolation belongs. MPM neither manages nor
   certifies it.
5. **A credential exposed in an earlier probe requires operator rotation.**
   The value is not repeated here.
