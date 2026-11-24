# Continuity 001, PR 3 — the OpenCode adapter and the reviewer integration

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
| the sidecar | the worker's provider credential holder; the reviewer has its own isolated process credential | `internal/core/opencode/sidecar.go` |

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

* **A permission declaration is not a boundary.** The namespace is.
* The adapter **refuses a session whose `permissions` array is non-empty**,
  even when the runtime reports success.
* The worker's agent definition is a *narrowing*, not the guarantee.

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

## 5. Runtime containment

The worker's whole process tree runs inside a bubblewrap mount namespace.

### The composition, and why the order is the security property

```
unshare --user --map-root-user --net -- /bin/sh -c '
    ip link set lo up
    <relay> &
    exec <bwrap args> -- <runtime>'
```

The outer `unshare` is **not** incidental. `bwrap --unshare-net` produces a
network namespace owned by the **parent** user namespace, so `ip link set lo up`
inside it fails with `EPERM` — and without loopback there is no address for the
relay, and without the relay there is no inference. Making the network
namespace a child of the **new** user namespace is what lets its owner bring
loopback up.

Measured result of this composition:

| | observed |
|---|---|
| `lo` | up (`127.0.0.1/8`) |
| egress to `1.1.1.1:443` | **BLOCKED** |
| the host's live OpenCode service on `127.0.0.1:49374` | **UNREACHABLE** |
| `/home`, `/home/v`, the production MPM database, the operator's checkout | **ABSENT** |
| `/etc/shadow`, `/etc/sudoers`, `/root` | **ABSENT** |
| the authorised worktree | reachable, read-write |

### Two credential leaks found and closed

Both were reproduced before they were fixed, and both are regression-tested.

1. **`--clearenv` does not clear bubblewrap's own init.** `/proc/1/environ`
   inside the namespace exposed the **caller's entire environment** — every
   provider key and `SSH_AUTH_SOCK`. Fix: the containment is launched with an
   explicit, short environment (`exec.Cmd.Env`), never the adapter's.

2. **`--setenv NAME VALUE` puts the value in argv**, readable through
   `/proc/<pid>/cmdline`. Fix: no credential is ever passed to the sandbox.
   The runtime authenticates to a **socket**, not to a provider.

### The worker is file-edit-only

`shell` is **denied**. The worker edits files and nothing else.

This is deliberate and provisional: with loopback up (the relay needs it), a
shell would let the model originate connections. A confined command capability
is a worthwhile later addition, but it needs the namespace and network
boundary verified independently first. **This is not a complete autonomous
coding environment, and the documentation does not claim it is.**

---

## 6. The inference sidecar

The worker's provider-credential holder, and it lives **outside** the
worker's namespace. The reviewer does not use this sidecar: its dedicated
credential is read only by the isolated reviewer process, and the
`sandbox_exec` helper scrubs it.

```
worker runtime ──HTTP──► relay (in namespace, no credential)
                              │ unix socket (bound in)
                              ▼
                        sidecar (outside, holds the credential)
                              │ HTTPS, one allow-listed host
                              ▼
                          the provider
```

A unix socket is a filesystem object, so it is the one channel that survives
network-namespace separation. That is why the relay exists.

### What the sidecar refuses

* any path but the one chat-completions endpoint;
* any method but `POST`;
* a model outside the allow-list;
* a body over the request ceiling;
* a completion length over the token ceiling;
* a response over the response ceiling;
* an exchange longer than the timeout.

It never forwards a caller-supplied `base_url`, `baseURL`, `api_base` or
`endpoint`: the upstream is **fixed at construction**, so the worker cannot
use it as a general proxy. The caller never supplies the credential; the
sidecar attaches it.

### Audit

One bounded line per request: outcome, reason, model, request bytes, response
bytes, duration. **Never** prompt text, completion text, headers or
credentials — asserted by a test that sends a marker string and reads the log
back.

---

## 7. The reviewer integration

Unchanged in spirit and **not weakened**:

* `mpm-reviewer` stays default-deny, with `sandbox_exec` as its only execution
  path;
* as of PR #260 it also denies `mpm_assignment`; verified in the isolated
  installation at rule index 23 of 47;
* `skill` is host-refused — `{"type":"permission.rejected","message":"Permission denied: skill"}`
  — verified by an actual attempted invocation rather than by reading the
  definition;
* a session-level grant on a reviewer session is the negative control: the
  host honours it, and the adapter refuses to **accept the result**. The
  check runs after the reviewer process, so the grant was effective during
  that run; the namespace, not the permission declaration, is the boundary.

The reviewer's provider credential is `MPM_OPENCODE_REVIEWER_KEY`, separate
from the worker's `MPM_OPENCODE_SIDECAR_KEY`. The adapter maps the measured
`minimax-coding-plan/MiniMax-M3.1-Flash` model to OpenCode's
`MINIMAX_API_KEY` **in the isolated reviewer process only**. It has no
fallback to an ambient provider key, the worker key or the operator's auth
store, and a model other than the measured
`minimax-coding-plan/MiniMax-M3.1-Flash` is refused rather than tried with
another provider. The CN endpoint and other same-family models were not
measured and are refused too.

Both the CONFIGURED model and the SESSION's own reported model are checked.
`Review` compares `session.model` with the configured model before returning,
and `ExecuteReview` checks the result against the model the dispatcher or the
explicit verb authorised. A runtime that silently fell back to another
provider does not produce an acceptable verdict.

The plugin's `sandboxProcessEnv` removes provider credentials before it
spawns its helper; `reviewer_bwrap.py` removes them from the environment it
passes to bubblewrap; bubblewrap's `--clearenv` then clears the command's
environment. The wrapper's own `/proc/<pid>/environ` can retain the
pre-scrub bytes until the process exits — `os.environ.pop` updates the
mapping, it does not erase the kernel's exec-time record — but that process
is on the HOST, outside the sandbox's PID namespace, so the command cannot
reach it. The scrub is an environment-passing boundary, not a process-memory
eraser.

The verifier reads the **ordered** resolved list and applies the host's
last-match-wins rule to both actions and resources. It is a conservative
coverage proof, not a full host-matcher implementation: it refuses a shape it
cannot prove. A host-prepended allow that a later agent deny overrides —
OpenCode 2.3.2's `read *.env.example` allow, overridden by `read * deny` — is
not treated as effective. An allow that survives is refused, including
`shell`, `execute`, network, subagent, skill and MPM actions; `sandbox_exec`
must itself survive, not merely appear before an overridden rule. Resource
patterns are matched, not collapsed into action-wide permissions, and a broad
allow is not accepted because a narrow later deny happened to match one
example. An effective `ask` is requestable, not denied, so it is unsafe unless
a later allow or deny fully covers it; the host's original `.env` asks are
covered by the later `read * deny`. Unknown permission effects fail closed,
and escapes, `?`, character classes, `**`, empty patterns, multi-star action
patterns, and two-star resource allows are refused because their host
semantics were not measured. The measured two-star `*middle*` resource form is
accepted for an ask or a deny; the host's `.env.*` asks pass, and a two-star
deny can cover a narrower one-star allow.

**There is no fallback to a less restricted reviewer.** If the sandbox or the
reviewer configuration is unavailable, the review refuses to start.

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
transcript is not empty". A tool part renders as `[tool sandbox_exec]`, which
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
| a confined command capability for the worker | after the namespace and network boundary are verified independently |
| project administration | **not PR 3** — an explicitly seeded fixture is used in the disposable integration database, and a separate operator-only project-creation operation is a prerequisite to PR 6. **Manual SQL insertion is not the supported production workflow.** |

---

## 9. Known limitations, stated plainly

1. **Exactly-once execution is not claimed.** OpenCode has no idempotent
   creation primitive. The adapter narrows the window to "intent persisted,
   launch not observed" and records that as `ambiguous`.
2. **The sidecar is a credential *isolator*, not a credential *manager*.** It
   reads one key from one environment variable and forwards to one host. It is
   not a secrets store and does not try to be.
3. **The worker cannot execute commands.** That is a scope decision, not a
   missing feature.
4. **The `lo`-up composition depends on `unshare --map-root-user`** being
   permitted in the host's user-namespace policy. On a host that forbids it,
   the containment refuses to start rather than degrading — which is the
   correct failure, but it means PR 3's live gate requires that policy.
5. **A credential exposed in an earlier probe requires operator rotation.**
   The value is not repeated here.
