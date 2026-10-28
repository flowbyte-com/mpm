# Session-end detection and continuity preservation in OpenCode v2.1.4

**Session C — 2026-10-28.**
Scope: what MPM can *reliably observe* when an OpenCode session finishes,
becomes idle, is interrupted, is archived, or the host process exits.

This is an **investigation report and a design proposal**. It contains no
code that changes behaviour. The one behaviour change the investigation
produced — recording the host's session-lifecycle events that the adapter
was swallowing — is a **separate, focused PR**:
[`feat/session-lifecycle-observations`](https://github.com/flowbyte-com/mpm/pull/148).

**Nothing here was deployed.** Every measurement ran against a disposable
workspace, a disposable database and the repository's own build, through
the isolated launcher `scripts/dev_host.sh`. The live installation was
never touched.

---

## 1. Method

### 1.1 The host was the real one

Every scenario ran the **installed** `opencode v2.1.4` through
`scripts/dev_host.sh`, which pins `MPM_WORKSPACE`, `MPM_DB_PATH` and
`MPM_BINARY` to disposable values, redirects all four XDG roots, and
registers **this repository's** plugin plus a witness probe. The live
config, the live session store and the live database are never touched.

### 1.2 The witness probe

`agent_installation/mpm-opencode/measure/probe-session-end/` records, for
every event the plugin's own `ctx.event.subscribe({signal})` stream
delivers:

* the event **type**, a sequence number, the wall clock and the host's
  `id`/`created`;
* the **key names** of `data` — the shape, never the content;
* four small scalars that carry a boundary's *meaning*: `sessionID`,
  `reason`, `outcome`, `archived`.

It records **no prompt text, no assistant message, no tool input, no tool
output and no compaction summary**. The P4 structural allowlist governs
what MPM *persists*; the probe holds itself to the same discipline so the
investigation does not become the leak it is investigating.

It also records the `ctx` surface (key names only) and the exact moment
the host calls the plugin's **synchronous** cleanup callback.

### 1.3 The scenarios

`agent_installation/mpm-opencode/measure/session_end_scenarios.py` drives
nine scenarios, one per signal, so that a signal which merely happens to
appear alongside another is not mistaken for one:

| scenario | what it does |
|---|---|
| `normal` | one prompt in a fresh session, then a graceful host stop |
| `multi-turn` | **two prompts in ONE session** |
| `interrupted` | interrupt an in-flight execution (`POST /api/session/{id}/interrupt`) |
| `shutdown-mid-execution` | SIGTERM the host **during** an execution |
| `session-switch` | a second session in the same host |
| `archived` | archive the session, then **restore** it |
| `deleted` | delete the session |
| `compaction` | compact the session |
| `abrupt-kill` | SIGKILL the host during an execution |

### 1.4 How the host's own model was read

Two sources, both from the installed binary and its live server:

* the **OpenAPI document** the server serves (`GET /openapi.json`), which
  names the session endpoints and the session schema; and
* the host's **own event-handler table**, read out of the installed
  binary. It is the authoritative list of the events the host publishes,
  and it names the ones a host user would never guess.

---

## 2. What the host actually publishes

### 2.1 There is no `session.idle` **event**

This is the single most consequential finding, and it is a *negative* one.

The host models "the agent loop became idle" as a **session MESSAGE**, not
as an event. Its own reducer does this:

```js
case "session.execution.succeeded":
case "session.execution.failed":
case "session.execution.interrupted":
  … insert({ id, type: "idle", outcome: "succeeded"|"failed"|"interrupted", … })
```

So the host *synthesises* an `idle` message from an execution event. The
`Session.Message.Idle` schema confirms it: `{id, time, type:"idle",
outcome, error?}`. `Session.Info.time` carries the matching
`{created, updated, idle, viewed, archived}` timestamps.

**Consequence:** a plugin watching the event stream never sees
`session.idle`. There is nothing to map. The nearest observable facts are
the execution events it is derived from, and `Session.Info.time.idle` —
which a plugin could *poll* through `ctx.session.get`, but polling is not
an event and is not proposed here.

### 2.2 The session-lifecycle events that DO exist

From the host's own event-handler table:

`session.created` · `session.viewed` · `session.archived` ·
`session.unarchived` · `session.metadata.updated` · `session.permissions` ·
`session.deleted` · `session.forked` · `session.moved` ·
`session.renamed` · `session.execution.started` ·
`session.execution.succeeded` · `session.execution.failed` ·
`session.execution.interrupted` · `session.inbox.*` ·
`session.compaction.*` · `session.step.*` · `session.text.*` ·
`session.tool.*` · `session.reasoning.*` · `session.retry.scheduled` ·
`session.shell.*` · `session.instructions.updated` · `session.synthetic` ·
`session.skill.activated` · `session.revert.*` · `session.usage.recorded` ·
`session.agent.selected` · `session.model.selected` ·
`session.message.content.updated`

Plus `location.shutdown` and a family of `*.updated` events
(`agent`, `command`, `integration`, `model`, `plugin`, `provider`,
`reference`, `skill`, `websearch`).

### 2.3 The traces

**`normal` — one prompt, then a graceful stop:**

```
session.inbox.enqueued → session.execution.started
  → …steps, text, reasoning, usage…
  → session.execution.succeeded
  → [SIGTERM]
  → location.shutdown
  → [cleanup.callback]        ← the plugin's synchronous cleanup
  → [stream.ended]
  → [process.exit code=130]
```

**`location.shutdown` is published AFTER the SIGTERM and BEFORE the
plugin's cleanup callback.** The host announces the shutdown, then calls
its plugins.

**`multi-turn` — two prompts in ONE session:**

```
session.execution.started → session.execution.succeeded
  → session.inbox.enqueued → session.execution.started
  → session.execution.succeeded
```

**No session-end event appears between the two turns.** A completed
execution is a **turn** boundary, not a session boundary.

**`interrupted` — `POST /api/session/{id}/interrupt` during an execution:**

```
session.execution.started → session.tool.failed → session.step.failed
  → session.execution.interrupted   { reason: "user" }
```

**`shutdown-mid-execution` — SIGTERM during an execution:**

```
[SIGTERM] → location.shutdown → [cleanup.callback] → [process.exit]
```

**No `session.execution.interrupted`.** The host's reducer has a branch
for `reason === "shutdown"`, but that event was **not published** on this
path. See the matrix — this is an *ambiguous* row, not a supported one.

**`session-switch` — a second session in the same host:**

```
session.created (session B) → session.execution.started → session.execution.succeeded
```

**Session A received no terminal event.** Switching away does not end a
session.

**`archived` — archive, then restore:**

```
session.archived → session.unarchived
```

**`deleted`:** `session.deleted`.

**`compaction`:** `session.compaction.started { reason: "manual" }` — and
**no session-end event**.

**`abrupt-kill` — SIGKILL during an execution:**

```
(nothing)
```

No `location.shutdown`, no `cleanup.callback`, no
`session.execution.interrupted`. **The host published nothing at all.**

---

## 3. Capability matrix

**Supported** — published by the host, observed on the plugin's stream,
repeatable, and recorded by MPM.

**Unsupported** — does not exist as a host event; nothing to observe.

**Ambiguous** — the host declares it, but it was not observed in the
measured runs; the adapter can map it but its payload shape is unverified.

| signal | verdict | published? | observed on the plugin stream? | repeatable? | a session TERMINAL boundary? | recorded by MPM? |
|---|---|---|---|---|---|---|
| `session.execution.succeeded` | **supported** | yes | yes | yes — **once per turn** | **NO** | yes — `session.observed`, `host_result=succeeded` |
| `session.execution.failed` | **ambiguous** | yes (host's table) | **not observed** in 9 runs | yes | **NO** | yes — mapped; never yet exercised against the real host |
| `session.execution.interrupted` | **supported** | yes | yes — `{reason:"user"}` | yes | **NO** | yes — `session.observed`, `host_result=interrupted` |
| `session.idle` | **UNSUPPORTED** | **no — it does not exist as an event** | n/a | n/a | n/a | n/a — the host synthesises an idle *message* from the execution events |
| `session.archived` | **supported** | yes | yes | yes — **reversible** (`session.unarchived`) | **NO** — the closest thing, but an archive is a user action and can be undone | **yes — after the fix** (PR #148); before, it was **dropped** |
| `session.unarchived` | **supported** | yes | yes | yes | **NO** | **yes — after the fix** |
| `session.deleted` | **supported** | yes | yes | yes | **terminal**, but **destructive** — the session's data is gone | **yes — after the fix**; before, it was **dropped** |
| `session.viewed` | **ambiguous** | yes (host's table) | not observed | yes | **NO** — a viewer action | no |
| `location.shutdown` | **supported** | yes | yes — **graceful stop only** | once per host process | **NO** — host-level, and carries **no session id** | **no** — counted as `ignored`; there is no conformant `mpm-event/0.1` type for it |
| the plugin's cleanup callback | **supported** | n/a — the host invokes it | yes | once per plugin load | **NO** — host-level | **no** — same reason |
| an abrupt kill (SIGKILL) | **UNSUPPORTED** | **nothing at all** | n/a | n/a | n/a | **no** — the adapter wrote nothing, not even its own counters |

### 3.1 The answer to "does any signal establish a session terminal boundary?"

**No. Not one of them.**

* `session.execution.*` is a **turn** boundary — proven by `multi-turn`:
  two successful executions, one session, nothing in between.
* `session.archived` is the closest, and it is **not** a boundary: it is
  reversible, and the host's own `Session.Info.time.archived` is a
  timestamp, not a tombstone.
* `session.deleted` **is** terminal, but it is destructive and it is a
  user action, not an agent conclusion.
* `location.shutdown` and the cleanup callback are **host-process**
  boundaries, not session boundaries, and neither names a session.
* On an abrupt kill there is **no signal at all**.

**The host has no session-terminal event, and none can be manufactured.**

---

## 4. What the adapter recorded — the gap the fix closes

### 4.1 Before

With the real transport and a disposable MPM store:

```
se-archived : 27 host events observed → 2 stored → the archive stored NOTHING
se-deleted  : 27 host events observed → 2 stored → the delete stored NOTHING
```

**Mechanism.** `session.archived` and `session.deleted` carry a session
id, so they fell into the adapter's **activity fallback** — which emits
**at most one** `session.observed` per session. By the time the host
archives or deletes a session, that single observation has long since
been emitted, so the event was *seen and then dropped*.

### 4.2 After (PR #148)

```
se-archived : 28 host events observed → 4 envelopes built → 4 delivered
              stored: inbox.enqueued, execution.succeeded, archived, unarchived
se-deleted  : 28 host events observed → 3 envelopes built → 3 delivered
              stored: inbox.enqueued, execution.succeeded, deleted
```

(The post-fix `archived` scenario also **restores** the session, which the
pre-fix run did not — hence one extra envelope and one extra event.)

The correction maps the three session-lifecycle events explicitly, to
`session.observed` — the contract's own **neutral** base type: *"any host
event that carries a session id | the adapter saw an event for this
session"* (§4). This is not a neighbour-mapping: §3.4 forbids mapping an
event onto a type that **asserts** something else, and `session.observed`
asserts only that the adapter saw the event.

**What the envelope does not assert:** that the session ended. No envelope
infers an end from the absence of later events — §3.4 forbids it, and §4
explicitly excludes `session.resumed` for exactly that reason.

---

## 5. Continuity and recovery: what MPM's own records support

### 5.1 MPM already defines the session boundary — by the handoff

This is the most important thing to know about continuity, and it is
already implemented:

* `session_handoffs` carries `ended_at`, `ended_state`,
  `summary`, `commitments`, `open_questions`;
* `ended_state` is already the closed vocabulary
  `{clean, crashed, interrupted, force_end}`;
* the wake context's **`session_previous_id` is the most recent handoff
  row**, and `session_previous_ended_at` is *that same row's* `ended_at` —
  the code is explicit that it "is never inferred from another session's
  start".

So **MPM does not need a host session-terminal event, because MPM already
defines the boundary itself**: a session ends when a handoff is written.
That is the right design given §4's finding, and it is already shipped.

### 5.2 Durable positive observations vs missing information

**What MPM can state positively, today:**

| fact | source | durable? |
|---|---|---|
| an execution succeeded / failed / was interrupted | the host's execution event, recorded as `session.observed` | yes — once ingested |
| the session was archived / restored / deleted | the host's lifecycle event, recorded after PR #148 | yes — once ingested |
| a compaction started / completed | the host's compaction pair | yes — once ingested |
| which MPM tools were dispatched, when, and with what outcome | `tool_invocations` — **MPM's own dispatch audit** | **yes — written by the core at dispatch time** |
| the session ended, and how | `session_handoffs.ended_state` | yes |
| what was learned | `memories` / `lessons` / `decisions` | yes |

**What MPM cannot state, and must not infer:**

| missing fact | why it cannot be stated |
|---|---|
| "session X ended" | no host event says so; inferring it from a gap in the event stream is forbidden (§3.4, §4's `session.resumed` ruling) |
| "session X was cut off" | on SIGKILL the host publishes **nothing** |
| "the host died" | same |
| anything about a session the adapter never saw | the host stream is volatile and delivery is best-effort |

### 5.3 Recognising an interrupted or unfinished session

**Interrupted: recognisable, positively.** `session.execution.interrupted`
is a measured, published, durable fact, and MPM records it with
`host_result: "interrupted"`. It says *this execution was interrupted* —
not *the session ended*, and not *the work is lost*.

**Unfinished: NOT recognisable as a fact, but recognisable as an
absence inside MPM's own store — and that is a different and safer
thing.** The honest statement is:

> An MPM session that has rows in `tool_invocations` but **no row in
> `session_handoffs`** was not closed by a handoff.

That absence is in **MPM's own authoritative store** — the dispatch audit
is written by the core, durably, at dispatch time. It is not a gap in a
best-effort stream, so reading it is not the forbidden inference. It
says *no handoff was written*; it does **not** say *the session ended*,
and it does not say *the work was lost*.

This is the only defensible basis for a recovery signal, and it needs no
host event at all.

### 5.4 The missed-capture opportunity

`tool_invocations` also makes a **positive** comparison possible inside
MPM's own store:

> this MPM session dispatched *N* tool calls, of which *k* were
> `mpm_memory save` / `mpm_lessons save` / `mpm_decisions record`, and
> *k* is zero.

Both sides of that comparison are authoritative rows in MPM's own
database, so it is not an inference about a volatile stream. It says
**no capture was made** — not *what should have been captured*. Nothing
in this design invents the content of a capture that was not made.

---

## 6. Design proposal — for Session A and the operator

> **This section is a proposal. Nothing in it is implemented.**
> Items D2 and D3 need a ruling; D1, D4 and D5 are the recommended
> defaults.

### D1. Keep MPM's session-end model as it is — the explicit handoff. **(recommended)**

MPM already defines the session boundary by the act of writing a
handoff, and the wake context already reads the previous session from
that row. Introducing a host-derived "session ended" event would either
duplicate that model or — worse — invite the gap-inference §3.4 forbids.

**Do not build a host session-terminal event.** There is none.

### D2. Optionally, a `mpm-event/0.2` type for host lifecycle. **(defer)**

`location.shutdown` is currently **counted and dropped** — there is no
conformant `mpm-event/0.1` type for an event that carries no session id.
A `host.shutdown` type would make it durable.

**Assessment: low value, low reliability, non-zero cost.** It is absent
on SIGKILL — the case that matters most — so it would record the benign
case and miss the harmful one. It costs a schema version (0.1 → 0.2),
which is a coordinated change across the core, the parity guard and the
contract. **Recommendation: defer.**

### D3. An automatic handoff on an observed interruption. **(needs a ruling)**

**Proposal.** When the adapter observes `session.execution.interrupted` —
a positive, measured fact — it may write an `mpm_handoff` with
`ended_state: "interrupted"`.

**The constraints it must satisfy, and why they are hard:**

1. **The summary must not be invented.** The handoff handler rejects an
   empty `summary`. The summary must be derived **only** from MPM's own
   durable records for that session (the dispatch audit), and it must say
   only what they say. If there are no records, there is nothing honest
   to write, and **no handoff is written**.
2. **It must not run on the hot path.** The contract forbids subprocess
   work inside the event iterator. The write must be out of band, in the
   observer's existing bounded drain.
3. **It must be bounded and best-effort.** A handoff write is an
   `mpm call` spawn. If the host exits first, the write is lost. This is
   **not** a guaranteed write, and the release notes must say so.
4. **It must not create a trusted artifact.** A handoff is a record with
   `trust` governed by its own surface; it creates no lesson, no theory,
   no decision, and it marks no work complete.
5. **It must not use the cleanup callback.** The cleanup contract is a
   **synchronous** `() => void`: the host cannot await it, so a write
   there would be unreliable by construction. The trigger must be the
   *event*, not the teardown.

**Open question for Session A:** should an interrupted *turn* end the
*session*? The evidence says a turn is not a session (multi-turn), and
the host itself keeps the session alive after an interruption. Writing a
session-ending handoff on a turn-level interruption would **invent** a
session end — the thing this whole investigation was commissioned to
avoid. The defensible variant is a handoff that records
`ended_state: "interrupted"` **for that execution**, not a claim that the
session ended.

### D4. A read-only "missed capture" diagnostic. **(recommended, no ruling needed)**

Report, per MPM session: the number of dispatched tool calls, the number
that were captures, and the difference — using `tool_invocations` only.

It is **read-only**, it never writes, and it never proposes *what* should
have been captured. It answers "did this session capture anything?",
which is a question about MPM's own authoritative rows.

### D5. What must NOT be built. **(binding constraints)**

* No automatic creation of **trusted lessons** at session end.
* No automatic **"work completed"** marking at session end.
* No inference of a session end from the **absence** of events.
* No **automatic handoff summary** assembled from absent evidence.
* No persistence of raw prompts, assistant messages, tool inputs or tool
  outputs. The P4 structural allowlist stands.
* No dependence on an **async cleanup** completing after the host exits.

---

## 7. Unavoidable limitations

1. **The host has no session-terminal event.** No adapter can observe
   one, and none can be manufactured from the events that exist.
2. **An abruptly killed host publishes nothing.** No shutdown event, no
   cleanup callback, no interrupted execution — and the adapter wrote not
   even its own diagnostic counters. Anything in flight is lost, and no
   adapter can record what it never received.
3. **The plugin's cleanup contract is synchronous** (`() => void`). The
   host cannot await it, so no cleanup-time write can be reliable.
   Anything that must be written must be written **earlier**, from an
   event.
4. **The host stream is volatile and delivery is best-effort.** The tail
   of the stream is lost at host exit. Measured: in
   `shutdown-mid-execution`, 48 host events were observed and **one**
   envelope reached the store.
5. **An archive is reversible and a delete is destructive.** Neither is a
   semantic "the work is finished".
6. **`session.idle` is a message, not an event.** A plugin cannot observe
   it on the event stream. (`ctx.session.get` could poll
   `Session.Info.time.idle`, but polling is not an event, is not
   reliable, and is not proposed.)
7. **Landing the lifecycle transport remains an operator decision.**
   Every measurement in this report was taken with
   `MPM_EVENT_INGEST=mpm_system`. With it unset — the state of every
   installation today — the adapter records **nothing**, so none of the
   above reaches the store. This is the same operator step the release
   candidate already documents.

---

## 8. Should this be in Friday's release candidate?

**The narrow fix: YES — include it if it merges before the cut.**

| | |
|---|---|
| What it is | three host events mapped to the existing `session.observed` type |
| New event type | **no** |
| New schema version | **no** |
| New persistent artifact | **no** |
| Automatic behaviour | **no** |
| New trust path | **no** — the envelopes are `trust: "observation"` and can create nothing |
| Core storage / provenance | **unchanged** |
| Blast radius | the adapter's envelope count for a session that is archived or deleted; no reader of `lifecycle_events` depends on that count |
| Evidence | 9 scenarios against the real host, 7 new regression tests, parity guard green, adapter suites green, isolated-host e2e green |

It is additive, it is measured, and it repairs a case where MPM
**silently dropped** a fact it had already observed.

**The design proposal: NO — defer past the release candidate.**

D3 in particular would write to `session_handoffs` **automatically** — a
durable, agent-visible surface — and it needs Session A's ruling on
whether a turn-level interruption may end a session. D2 costs a schema
version. D4 and D1 are recommendations, not changes.

**A note on the operator step.** The fix only has an effect when
`MPM_EVENT_INGEST=mpm_system` is set — the same operator step the
release-candidate report already documents. It therefore improves the
enabled path and changes nothing on the default path.

---

## 9. Artefacts

| artefact | where |
|---|---|
| the witness probe | `agent_installation/mpm-opencode/measure/probe-session-end/` |
| the scenario driver | `agent_installation/mpm-opencode/measure/session_end_scenarios.py` |
| the fix + tests | PR #148, `feat/session-lifecycle-observations` |
| the raw traces | the scenario driver's `--out` directory |

The driver is committed **with** the fix: it is the evidence for both the
gap and the correction, exactly as P4's `measure/` directory is the
evidence for P4.
