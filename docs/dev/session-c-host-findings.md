# Session C — OpenCode v2.1.4 host findings, MPM tool-surface measurements, and the `mpm-event/0.1` acknowledgement

> **Author:** Session C — OpenCode adapter and behavioural evaluation.
> **Branch:** `dev/session-c-opencode` @ `70deac2bb3fce3c63da28bfbd9ca82ca2c4d1e2e`
> (verified equal to `origin/main` at the time of writing).
> **Worktree:** `~/src/mpm-opencode`. No other session's worktree and no installed
> runtime were modified.
> **Date:** 2026-10-26. **Model:** `minimax-coding-plan/MiniMax-M3.1-Flash` via
> OpenCode v2.1.4.
> **Status:** investigation and measurement only. **No implementation, no merge,
> no deploy, no change to the tool surface, the tool registry, `package.json` or
> the lockfile.**

---

## 0. Executive summary

Three findings dominate this report. They are ordered by consequence.

| # | Finding | Consequence |
|---|---|---|
| **F1** | **The MPM wake context has never been injected into an OpenCode agent's system prompt.** The installed plugin reads `payload.context ?? payload.text`; the substrate returns the text under **`payload.content`**. The extractor always yields `""`, so the context is never pushed and the per-session cache is never populated. | The system's central mechanism is dead in the OpenCode adapter — **confirmed live**: the installed plugin made **370 wake-context fetches in the last hour** (~12 per minute), each a ~39 ms process spawn, and **delivered nothing**. |
| **F2** | **The event contract's strict-mode `event_id` derivation collides.** OpenCode gives nested tool calls **one shared `callID`**. `sha256(schema_version ‖ host ‖ host_event_key)` alone gives the outer and inner tool events the *same* id, so one is silently deduplicated away. | **Blocking objection to M3.** The derivation input must include `event_type`. |
| **F3** | **Two §4 vocabulary rows cannot be populated as written.** There is no session-creation hook, and the only compaction signal the plugin can receive is a single "compaction context was assembled" event, not a started/finished pair. | **Blocking objection to M2.** The vocabulary needs A's adjudication. |

Everything else the report contains is either **good news that should be
preserved** (the host provides a project id that already satisfies the
contract's identity requirements; classification, preservation and
cross-session continuation all work; the compact MCP surface is 19.6× smaller
than the full one) or a **measured, bounded gap** with a named correction.

**Reconciliation with Session B (concurrent, §12).** B concluded that the
retrospective-detection path needs **no new event framework, no new table
and no schema change**. C's measurements agree, and that **reorders C's
priorities**: the three adapter fixes that need no contract agreement are
worth more than the event work, which is blocked on adjudication and builds
a store B's design does not need. **B acknowledged M1–M7 with no objection;
C has two. The contract is not agreed, and A's adjudication should focus on
those two.**

---

## 1. M1–M7 acknowledgement

This is Session C's position on the minimum shared interface
(`docs/dev/event-contract.md` §7). It is recorded in the shared dogfooding
workspace as decision `8cf1f0e3cc41465e`.

**Contract version acknowledged: `mpm-event/0.1`.**

| Item | Verdict | Notes |
|---|---|---|
| **M1** — Envelope | **AGREE** | Every required field is implementable against the measured host. `host_version` is available as `ctx.app.version` (`"2.1.4"`); `adapter_version` is the plugin build sha, already emitted as `dist/mpm-build-provenance.json`. |
| **M2** — Vocabulary & classification | **AGREE IN PRINCIPLE — one BLOCKING OBJECTION** | The closed-vocabulary discipline, the "never map onto a neighbouring type" rule and the observed-only rule are all implementable and I endorse them. **But two §4 rows are not populatable.** See §1.1. |
| **M3** — Identity | **AGREE — one BLOCKING OBJECTION** | The §3.3 chain, the "never synthesize `framework_session_id`" rule and the strict/best-effort split are all implementable. **But the §3.5 derivation is unsafe as written.** See §1.2. Also: the host already supplies a better project id (§4.3) — this is an *improvement* to M3, not an objection to it. |
| **M4** — Ordering | **AGREE** | Millisecond precision is directly obtainable (`Date.now()`; the host's own event envelope carries `created` in ms). The host gives the plugin **no** monotonic per-session counter, so `host_seq` will be `null` in practice — §3.6 already specifies the NULL ordering, so the rule needs no change. The prohibition on inferring causality from timestamps is endorsed without qualification. |
| **M5** — Trust | **AGREE** | The adapter is *structurally* incapable of promotion: it can only call `mpm call`, and every write path requires an explicit tool call with explicit parameters. No adapter code path writes a memory, lesson or decision without the model asking for it. |
| **M6** — Ingest contract | **AGREE** | Validate → classify → dedup → stamp → store → receipt, with an observable rejection counter, is implementable. The one thing M6 depends on and does not fix is the transport (§6.2), for which I have a measured cost figure (§6.4) that bears on A's decision. |
| **M7** — Outcomes | **AGREE, with one minor request** | The adapter can derive `result_status` and an `outcome_class` from the host's tool-execution status token (§4.5). **Request:** §3.7 needs an explicit mapping table from the host's status vocabulary to `internal.ToolOutcomeClass`'s nine values. The host's vocabulary is opaque and undocumented; A (or B, who owns the enum's semantics) should pin it, otherwise the adapter will invent one. |

**Blocking objections: 2** (M2 and M3). Per the launch gate §5, no dependent
event implementation may proceed until A resolves them.

### 1.1 Blocking objection to M2 — two §4 vocabulary rows are not populatable

**`session.started` — required capability "an observable session-creation
signal".** There is none.

The complete set of session hook names the v2.1.4 host actually fires is
measured in §4.2: `context`, `compaction`, `generate`, `model.request`,
`title`, `http.request`, `http.response`, `retry`,
`experimental.ws.handshake`. **None of them is a session-creation signal**,
and none carries a "this session was created" payload. The host's *event
stream* does have `session.created`, but §4.1 shows that stream is not
reachable from a plugin in this host version.

`session.started` is therefore **not directly observable**. It is *reliably
inferable* — the first `ctx.session.hook("context")` for a given `sessionID`
means that session became active — but §3.4 says the adapter emits observed
events only, and inference belongs to Session B's critic.

> **Proposal to A (M2).** Either (a) remove `session.started` from the 0.1
> vocabulary and let `session.observed` carry it (the neutral alternative §4
> already offers), or (b) admit `session.started` as an **inferred** event with
> a declared basis, which §3.4 currently forbids the adapter from producing
> and which would require a §3.4 change. **Option (a) costs nothing**;
> `session.observed` is already in the vocabulary and is directly observable
> from the `context` hook's `sessionID`.

**`compaction.started` / `compaction.completed` — required capability "a
compaction hook".** One exists, but it is not a pair.

`ctx.session.hook("compaction")` fires when the host assembles the context
for a compaction. It is a **single** signal about a context build. The host's
event stream has the real `session.compaction.started` / `.ended` / `.failed`
triple — but again, unreachable from a plugin (§4.1).

So the contract asks for two events that bracket a compaction, and the only
signal the plugin can receive is one event that says "a compaction context
was assembled". Deriving "completed" from it would be exactly the
timestamp-gap inference §3.6 forbids.

> **Proposal to A (M2).** Replace the pair with a single observed type —
> `compaction.context` — meaning "the host assembled a compaction context" —
> and let Session B's critic decide whether a compaction *completed* from
> subsequent evidence. If A prefers to keep the started/completed pair, the
> pair must be sourced from the host event stream, and §6.2's transport and
> the event-stream accessibility question become prerequisites rather than
> consequences.

### 1.2 Blocking objection to M3 — the strict-mode `event_id` derivation collides

§3.5 derives a strict-mode id as:

```
event_input := encode(schema_version) ‖ encode(host) ‖ encode(host_event_key)
event_id    := "ev1_" ‖ hex(sha256(event_input))
```

where `host_event_key` is "the first non-empty of: `host_event_id`, the host's
`callID`, the host's `messageID` (+ `partID` when present), or a host-provided
monotonic `(session, seq)` pair".

**Measured (2026-10-26):** OpenCode v2.1.4 gives **nested tool calls one
shared `callID`**. From the raw probe capture
(`measure/evidence/host_surface_probe_run.jsonl`):

```
tool.hook execute.before  {"tool":"execute",   "id":"call_60bea1446fee4148832b5230", ...}
tool.hook execute.before  {"tool":"probe_tool","id":"call_60bea1446fee4148832b5230", ...}
```

The outer `execute` tool (which ran `await tools.probe_tool(...)`) and the
inner `probe_tool` carry the **identical** `id`. The `id` is stable and unique
per *invocation*, not per *tool call within the invocation*.

With the derivation as written, the `tool.outcome` event for `execute` and
the `tool.outcome` event for `probe_tool` would derive the **same**
`event_id`. §3.10 step 5 ("Deduplicates on `event_id`: in strict mode an
existing id is a no-op") would then silently discard the second event. This
is a **silent data-loss** failure mode, which is the worst kind for a
contract whose stated purpose is durable observation.

> **Proposal to A (M3).** Extend the derivation input with the event type
> and the host tool name:
>
> ```
> event_input := encode(schema_version) ‖ encode(host) ‖ encode(event_type)
>              ‖ encode(host_event_key) ‖ encode(host_tool_name)
> ```
>
> `host_tool_name` may be omitted when the host provides no tool name. The
> length-prefixed encoding already in §3.5 handles the extra fields without
> ambiguity, and `TestEventID_EncodingIsUnambiguous` already exists to pin it.
> The contract's own §8 test list should gain
> `TestEventID_NestedToolCallsDoNotCollide`.

### 1.3 Non-blocking: an *improvement* to M3 — the host already has the project id

Contract §9 Q2 asks Session C: "Does the host's plugin input carry a stable
project id that would supersede the git-remote derivation in §3.3?"

**Yes. Measured.** See §4.3. This is an improvement to M3, not an objection
to it, and it needs A's decision (§9, item 1).

---

## 2. Method — how the host was investigated

Three independent methods, cross-checked:

1. **Static extraction from the installed host binary.**
   `~/.opencode/bin/opencode` is a Bun standalone executable whose
   JavaScript sources are embedded as plain text. `strings` plus a
   byte-offset scan recovered the **plugin-context builder verbatim**
   (`agent_installation/mpm-opencode/measure/` references the same facts).
   This is authoritative for *which hook names exist* and *how the dispatcher
   is wired*.

2. **A live probe plugin in a fully isolated OpenCode instance.**
   `XDG_CONFIG_HOME`, `XDG_DATA_HOME`, `XDG_STATE_HOME` and `XDG_CACHE_HOME`
   were all redirected to `/tmp/opencode/probe-c/*`; the live
   `~/.config/opencode`, the live session store and `~/.local/share/opencode`
   were never touched. The probe plugin is read-only and fails open. The
   harness is preserved at `agent_installation/mpm-opencode/measure/probe/`.

3. **Behavioural fixtures with the real MPM plugin.**
   The *installed* plugin payload (`~/.mpm/agent_installation/mpm-opencode`)
   was loaded by the isolated host, pointed at a **disposable** MPM workspace
   (`/tmp/opencode/probe-c/mpm-scratch`). Neither `~/.mpm` nor
   `~/.mpm-dogfood` received a single write from any fixture. The harness is
   `agent_installation/mpm-opencode/measure/fixture_run.sh`.

**Isolation verification.** `opencode debug paths` under the redirected
environment confirmed:

```
data    /tmp/opencode/probe-c/data/opencode
config  /tmp/opencode/probe-c/config/opencode
state   /tmp/opencode/probe-c/state/opencode
db      /tmp/opencode/probe-c/data/opencode/opencode.db
```

The installed plugin was **read**, never written. The live OpenCode database
was read **read-only** (`file:…?mode=ro`) once, to confirm project/worktree
grouping.

### 2.1 The repository's declarations are not evidence — confirmed

The brief and the launch gate both warned that
`agent_installation/mpm-opencode/src/opencode-sdk.d.ts` is a hand-written
curated subset. That is now demonstrated rather than asserted:

* the stub declares `PluginContext` with exactly `tool`, `session`, `shell`;
  the real host provides **26** namespaces (§4.1);
* the stub's `ToolContext` declares `sessionID`, `agent`, `messageID`, `id`,
  `signal` — which is **correct**, and the only part of the stub that
  survived contact with the host unchanged;
* the stub's `PluginContext` has no `location`, so the host's project
  identity is invisible to anyone reading the declarations.

**Recommendation:** the stub should be replaced by the measured surface
(§4.1) once A accepts the contract, so the next reader is not misled. This
is a documentation change, not an interface change, and it is not urgent.

---

## 3. The installed host and the installed plugin

| Thing | Measured fact |
|---|---|
| Host binary | `~/.opencode/bin/opencode`, ELF x86-64, **not stripped** |
| Host version | `opencode v2.1.4` (`ctx.app = {name:"desktop", version:"2.1.4", channel:"latest"}`) |
| Host plugin loader | directory plugin resolved by a root-level `server.js` / `index.js`; `package.json` `main`/`exports` are **not** consulted (the installed plugin's `server.js` shim documents this, and the probe confirmed it: the log line was `loading plugin id=/tmp/.../plugin entrypoint=file:///tmp/.../plugin/server.js`) |
| Host plugin source | `@opencode/plugin` is **not installed** for the plugin; the host aliases the specifier at load time. The installed package's `node_modules` contains only `@types/node` and `typescript`. `peerDependencies: {"@opencode/plugin": "^2.0.0"}` is marked optional. |
| Installed plugin payload | `~/.mpm/agent_installation/mpm-opencode/dist/index.v2.js`, staged 2026-10-24, 29 200 B |
| Installed runtime | `~/.mpm` @ `3df72315` (predates all fifteen corrective PRs — unchanged, deliberately) |
| Dev build used for measurement | `0.1.0-dev+g70deac2b.t1793040708` = `~/src/mpm/.build/bin/mpm` |
| MCP server used for measurement | `~/src/mpm/.build/bin/mpm-mcp` (same build) |
| Environment pinned | `MPM_WORKSPACE=$HOME/.mpm-dogfood`, `MPM_DB_PATH=$HOME/.mpm-dogfood/src/db/mpm.db`, `MPM_BINARY=$HOME/src/mpm/.build/bin/mpm` |

**On the environment requirement.** The three variables above are what the
brief asks to be pinned. Two facts bear on how they reach the host:

* `mpm` and `mpm-mcp` **ignore `MPM_DB_PATH`**; `mpm-critic` and
  `mpm-scheduler` honour it (already established by Session A).
* The plugin pins `MPM_WORKSPACE` for every subprocess it spawns, via
  `withWorkspace()` in `src/workspace.ts` — **but it never sets
  `MPM_DB_PATH`**, and it has no way to: `mpm` ignores it anyway.

Therefore **`MPM_DB_PATH` cannot be made to apply to the running host/plugin
by configuration alone.** The only lever is `MPM_WORKSPACE`, which selects
the database directory for `mpm`/`mpm-mcp` alike. The dogfooding workspace is
selected correctly by `MPM_WORKSPACE` alone; `MPM_DB_PATH` is a redundant
pin for the binaries that honour it (`mpm-critic`, `mpm-scheduler`), which
are not spawned by the plugin. **No change is required, but the brief's
expectation that all three apply to the running host is only true for
`MPM_WORKSPACE` and `MPM_BINARY`.** Flagged so A does not assume otherwise.

---

## 4. Verified OpenCode host and plugin API findings

### 4.1 The plugin context — measured, complete

`ctx` provides **26** namespaces. Full key list, from
`measure/evidence/host_surface_probe_run.jsonl`:

```
app        location    options    agent      aisdk      command
credential event       experimental generate   model      provider
integration mcp        permission plugin      reference  rpc
skill      storage     tool       vcs        websearch  worktree
session    shell
```

There is **no** `ctx.chat`, **no** `ctx.project`, **no** `ctx.client`, **no**
`ctx.experimental.chat`. The `chat.*` and `command.execute.before` hooks the
contract's §5 lists as v1 capabilities **do not exist in v2** under any name.

Method shapes (all own-key lists confirmed at runtime):

| Namespace | Shape |
|---|---|
| `ctx.app` | `{name, version, channel}` — plain values |
| `ctx.location` | `{directory, workspaceID, project}`; `project = {id, directory, canonical}` |
| `ctx.tool` | `{reload, list, transform, hook}` — **`hook` exists** |
| `ctx.session` | `{hook, list, create, get, remove, switchAgent, switchModel, prompt, generate, command, compact, synthetic, interrupt, update, move, wait, context}` |
| `ctx.shell` | `{hook}` |
| `ctx.permission` | `{hook, list, get, reply, assert}` |
| `ctx.aisdk` | `{hook}` |
| `ctx.event` | `{subscribe}` — **present but delivers nothing** (§4.1.1) |
| `ctx.tool.transform(editor)` | editor: `{list, get, namespace, add, update, remove}` |

#### 4.1.1 `ctx.event.subscribe` exists but is inert

The plugin-facing event bus registers without error and delivers **zero**
events across a full agent turn — while the server's own log and the
`opencode.db` `event` table show the host publishing `session.*`,
`message.*`, `permission.*` and `tool.*` events with the envelope:

```
{ id: "evt_179cae8f500132E6vM0aD8lvR4", created: 1793044703477,
  type: "provider.updated", location: {directory, workspaceID}, data: {} }
```

The full host event-type vocabulary is in the binary (49 `session.*` types
plus `permission.asked/replied`, `form.*`, `message.*`, `session.text.delta`
and friends). It is rich and it is exactly the vocabulary the contract wants —
**and it is not reachable from a plugin in v2.1.4.**

That is a hard constraint, not a preference. The events are reachable from
the server's log, from the local SQLite `event` table, and from the SSE
endpoint; none of those is a documented plugin surface. A plugin *could* read
the local SQLite file, but that is reading another process's private store —
not a contract anyone should write against.

**Consequence for the contract:** every §4 vocabulary row must be sourced
from the **hook** surface, not from the event stream. This is what forces the
M2 objection in §1.1.

### 4.2 The hook surface — exactly which names fire

`ctx.session.hook(name, fn, options?)` — the third argument is an **options
filter** (the host's own plugins pass `{providerID: …}`).

| Hook | Names the host **actually fires** | Payload shape (measured) |
|---|---|---|
| `ctx.session.hook` | `context`, `compaction`, `generate`, `model.request`, `title`, `http.request`, `http.response`, `retry`, `experimental.ws.handshake` | `context`: `{sessionID, model, system, messages, options, agent, tools}` · `title`: `{sessionID, model, system, messages, options}` · `model.request`: `{sessionID, agent, model, kind, baseURL, headers}` · `http.request`/`http.response`: `{sessionID, agent, model, kind, request[, response]}` |
| `ctx.tool.hook` | `execute.before`, `execute.after` | before: `{tool, sessionID, agent, messageID, id, input}` · after: `{…, status, result}` |
| `ctx.permission.hook` | `evaluate` | not captured (no permission prompt in the fixtures) |
| `ctx.shell.hook` | `create.before` (used by MPM today) | `{command, cwd, timeout, shell, env}` — **no `sessionID`** (already noted in `opencode-sdk.d.ts`; confirmed) |
| `ctx.aisdk.hook` | `sdk`, `language` | not relevant to MPM |

**Two registration hazards, both measured:**

1. **Every `hook()` accepts any name at registration time.** Registering
   `ctx.session.hook("telemetry", fn)` succeeds silently and the callback
   never fires. A plugin that registers a typo gets no error and no signal.
2. **`ctx.tool.hook` is different and dangerous.** Its dispatcher implements
   only `execute.before` and `execute.after`:

   ```js
   hook:(o,r)=>I(e.tool.hook(o,(n)=>oe[o](()=>Promise.resolve(r(n)))))
   //                                        ^^^^^^ oe = { "execute.before": …, "execute.after": … }
   ```

   `oe[o]` is evaluated **when the hook fires**, not when it is registered. So
   registering `ctx.tool.hook("execute", fn)` — a plausible typo — succeeds,
   and then **throws a `TypeError` the first time any tool executes in that
   process**, because `oe["execute"]` is `undefined`.

   *This is why the round-1 probe's bulk registration of 30+ hook names
   across `session`/`shell`/`tool`/`permission` is not repeated in the
   shipped harness: `ctx.tool.hook("before")`, `("after")`, `("call")` and
   `("invocation")` would have poisoned the process.* The final harness only
   registers the two valid names.

### 4.3 Project identity — the host already satisfies the contract

`ctx.location.project = {id, directory, canonical}`.

`id` is a **40-hex digest of the git remote**, not of the filesystem path.
Measured with seven throwaway repositories, one shared isolated database:

| `remote.origin.url` | `ctx.location.project.id` |
|---|---|
| `git@github.com:flowbyte-com/mpm.git` | `0f29f7080ac8ce4666de0d72752574d42c53ab77` |
| `git@github.com:flowbyte-com/mpm` | `0f29f7080ac8ce4666de0d72752574d42c53ab77` |
| `https://github.com/flowbyte-com/mpm` | `0f29f7080ac8ce4666de0d72752574d42c53ab77` |
| `https://github.com/flowbyte-com/mpm.git` | `0f29f7080ac8ce4666de0d72752574d42c53ab77` |
| `https://user:secret@github.com/flowbyte-com/mpm.git` | `0f29f7080ac8ce4666de0d72752574d42c53ab77` |
| `git@github.com:flowbyte-com/other.git` | `c78f6ec5b16eb2db55ee3391075e825b9af4599b` |
| `/tmp/…/local-remote.git` (local path) | `ce54bc61161512beb17a3190b7b47c18427085d0` |

and three linked worktrees of one repository:

| directory | project id | `project.canonical` |
|---|---|---|
| `…/probe-c/repo` | `0f29f7080ac8ce4666de0d72752574d42c53ab77` | `…/probe-c/repo` |
| `…/probe-c/wt1` | `0f29f7080ac8ce4666de0d72752574d42c53ab77` | `…/probe-c/repo` |
| `…/probe-c/wt2` | `0f29f7080ac8ce4666de0d72752574d42c53ab77` | `…/probe-c/repo` |

The live host's own database confirms the same grouping for the sprint
worktrees: project `0f29f7080ac8ce4666de0d72752574d42c53ab77` owns
`/home/v/.mpm`, `/home/v/src/mpm`, `/home/v/src/mpm-core` and
`/home/v/src/mpm-opencode`.

**This is exactly what the contract's §3.3 chain is specified to produce:**
SSH and HTTPS collapse, `.git` is stripped, embedded credentials are
stripped, a different remote gives a different id, and all linked worktrees
of one repository give one id. The host computes it correctly and
**already**.

**The one divergence:** where there is **no** git remote, the host still
emits an id — derived from the path (a repo with no remote gave
`d155668d…`, two plain directories gave `b0b69d2f…` and `1903d8e2…`). The
contract's §3.9.5 says "`project_id` is never fabricated". Adopting the host
value unconditionally would violate that rule in the no-remote case, and
would also mean the id is path-derived — the exact failure mode §3.3 was
written to prevent.

> **Proposal to A (M3 / §9 Q2).** Adopt `ctx.location.project.id` as the
> project identity **when the adapter independently confirms that a git
> remote exists** — i.e. keep the contract's own chain as the *verifier*, and
> take the host's value as the *canonical form*. When there is no remote,
> fall through to §3.3's chain and end at `unresolved`/`null` exactly as the
> contract specifies. Record `project_id_source: "host-git-remote"` in that
> case, so the distinction is observable.
>
> This is strictly better than the current chain: it also works for a
> repository with a remote but no `origin` alias, and it removes a whole
> class of normalisation code (and its tests) from the adapter.

### 4.4 The tool execution context — where the identifiers live

`ctx.tool.hook("execute.before")` payload, verbatim from the probe:

```json
{
  "tool": "probe_tool",
  "sessionID": "ses_e8634e367ffe20qy7275R8C4b9",
  "agent": "build",
  "messageID": "msg_179cb1dc7001GDPrH1b82MiPAD",
  "id": "call_60bea1446fee4148832b5230",
  "input": { "note": "x" }
}
```

and `execute.after` adds `"status": "completed"` and a `result`.

**The plugin's own tool-execution context carries the same fields** (plus
`progress` and `signal`):

```
sessionID  ses_e86384f50ffePOiA9HtatEZTO8
agent      build
messageID  msg_179c7b1db00163qWMozQrF8Jpu
id         call_01a179c7b4047205811811d3
progress   [fn]
signal     AbortSignal
```

Identifier formats are stable and self-describing:
`ses_…` (session), `msg_…` (message), `call_…` (call).

**The MPM adapter currently reads only `toolCtx.sessionID`.** It discards
`id` (the callID), `messageID` and `agent` — the three fields §3.5 needs for
strict-mode identity and that the contract's §3.8 `meta` allowlist already
names (`host_message_kind`, `host_agent`). This is the single smallest change
that would let `tool.outcome` events be `strict` rather than `best_effort`.

### 4.5 The outcome signal

`execute.after` supplies `status` and, on failure, an `error` object. The
host's own status vocabulary is opaque and undocumented. The host's own
plan-mode plugin tests `r.status !== "error"`, so at least `"completed"` and
`"error"` exist. §3.7's `result_status` and `outcome_class` can therefore be
populated, but the **mapping** from the host's status token to MPM's nine
`ToolOutcomeClass` values must be pinned by A or B — otherwise the adapter
will invent a mapping, which is exactly what §3.7's "no new vocabulary is
introduced" is meant to prevent. This is the M7 minor request.

---

## 5. Per-event observability matrix

Classification per the launch gate §4.2.

**Directly observable** = the host delivers it; C can read it from a real
hook. **Reliably inferable** = derivable from what the host delivers by a
stated, testable rule. **Unobservable** = the host provides nothing.

| Contract §4 type | Host signal | Classification | Host event key | Notes |
|---|---|---|---|---|
| `session.started` | — | **Unobservable** (reliably inferable from the first `context` hook per `sessionID`, but inference is forbidden to the adapter by §3.4) | none | **Blocking objection, §1.1.** The host event stream has `session.created` but is unreachable (§4.1.1). |
| `session.observed` | `ctx.session.hook("context")` → `sessionID` | **Directly observable** | `sessionID` (per-session, not per-event) | The neutral alternative. Fires at least once per session, often several times. |
| `context.changed` | `ctx.session.hook("context")` | **Directly observable** | `sessionID` + adapter-local counter | **Already registered by the plugin.** Payload carries `system`, `messages`, `options`, `agent`, `tools`. |
| `tool.outcome` | `ctx.tool.hook("execute.before"/"execute.after")` | **Directly observable** | `id` (callID) — **but shared across nested calls** (blocking objection, §1.2) | Payload: `{tool, sessionID, agent, messageID, id, input[, status, result]}`. `execute.before` is fire-and-forget; `execute.after` may be awaited. |
| `task.boundary` | `mpm_work` create/complete/cancel | **Directly observable — but only via the host shell** | none (MPM-generated ids) | `mpm_work` is **not** in the OpenCode plugin's tool surface (§7.2). The shell path stamps `MPM_PROVENANCE_FRAMEWORK=opencode` but **not** the session id, so `framework_session_id` is null. |
| `compaction.started` | `ctx.session.hook("compaction")` | **Directly observable**, but it is a *context-assembly* signal, not a "began" signal | none | Fires when the host builds a compaction context. It did not fire in the fixtures (no compaction occurred). |
| `compaction.completed` | — | **Unobservable** | none | The host's `session.compaction.ended` exists but is unreachable (§4.1.1). **Blocking objection, §1.1.** |
| `handoff.written` | `mpm_handoff write` | **Directly observable** | none (MPM id) | `mpm_handoff` **is** in the plugin surface. |
| `critic.proposal` | the critic's proposal path | **Directly observable** (Session B's domain) | none | Not C's to instrument. |

### 5.1 Additional signals C was asked to classify

The brief asks about more than the §4 vocabulary. Full results:

| Signal | Classification | Finding |
|---|---|---|
| **Session start** | **Unobservable** | No session-creation hook. The first `context` hook for a `sessionID` is the closest thing. |
| **Session resume** | **Unobservable** | No hook. §3.4 already excludes `session.resumed` from 0.1 for exactly this reason — C now confirms the reason empirically. |
| **Session idle** | **Unobservable** | Not a hook name. The host publishes `session.viewed` / `session.execution.*` to its event stream, which the plugin cannot reach. |
| **Session completion** | **Unobservable** | No hook. `ctx.session.interrupt` is an *action*, not a notification. |
| **Session interruption** | **Unobservable from a hook**; partially inferable | `execute.after` reports `status: "error"` for an aborted tool, but there is no session-level interruption signal. |
| **Tool execution before / after** | **Directly observable** | `ctx.tool.hook("execute.before" / "execute.after")`. This is the strongest signal the host offers. |
| **Host event id** | **Directly observable, but not to plugins** | The event envelope's `id` is `evt_<hex>` and `created` is epoch-ms. Not reachable via `ctx.event.subscribe` (§4.1.1). |
| **Host invocation id** | **Directly observable** | The tool context's `id` is `call_<hex>` and is stable across the before/after pair — it is the natural strict-mode key (with the §1.2 correction). |
| **Task boundaries** | **Directly observable via `mpm_work`** | But `mpm_work` is absent from the plugin surface and reachable only shell-side (§7.2). |
| **Context changes** | **Directly observable** | `ctx.session.hook("context")` — already wired. |
| **Compaction initiation / completion** | **One signal, not two** | `ctx.session.hook("compaction")` — one event, meaning "compaction context assembled". See §1.1. |
| **Project / worktree identity** | **Directly observable** | `ctx.location.project.{id,directory,canonical}` + `ctx.worktree.{list,create,…}`. The project id already satisfies §3.3 (§4.3). |
| **Session metadata** | **Directly observable** | `sessionID` (`ses_…`), `agent` (`build`), `messageID` (`msg_…`), `model` (from the `context` payload: `{providerID, id}`). |
| **Model identity** | **Directly observable** | `ctx.session.hook("context")` payload `model`; `ctx.model.list()`; and `model.request` carries `model` + `baseURL`. **Note:** the *tool* context carries **no** model (the stub's comment is correct). |

---

## 6. Existing adapter capability inventory

### 6.1 What the installed plugin does today

`agent_installation/mpm-opencode/src/index.v2.ts` (and its compiled twin):

| Capability | Implementation | Status |
|---|---|---|
| Tool registration | `ctx.tool.transform(editor => editor.add(…))` — 14 domain tools + 3 standalone = **17** | working |
| Substrate transport | one `mpm call <tool> --payload '<json>'` spawn per tool call | working; **~39 ms per call** |
| Wake-context injection | `ctx.session.hook("context")` → `fetchWakeContext()` → `event.system.push({type:"text", text})` | **BROKEN — §7.1** |
| Shell provenance | `ctx.shell.hook("create.before")` → stamps `MPM_PROVENANCE_FRAMEWORK` + `MPM_PROVENANCE_INVOCATION_ID` | working; **no session id** |
| Tool provenance | `buildProvenanceEnv(model, sessionID)` → `MPM_PROVENANCE_FRAMEWORK_SESSION_ID` | working; **only `sessionID` is used** — `callID`, `messageID` and `agent` are discarded |
| Boot health check | `mpm_system health_check` with a 2 s timeout | working |
| DB-path invariant | `MPM_REQUIRED_DB_PATH` gate | working |
| Fail-open behaviour | every call returns a soft error envelope | working — a genuinely good design choice |
| Plugin teardown | `return () => wakeContextCache.clear()` | working |
| **Event stream** | — | **not implemented; not implementable via the documented plugin API in v2.1.4** (§4.1.1) |
| **Tool-execution hooks** | — | **not implemented, but available** — `ctx.tool.hook` exists |
| **Compaction hook** | — | **not implemented, but available** — `ctx.session.hook("compaction")` |
| **Identifier capture beyond `sessionID`** | — | **not implemented, but available** (§4.4) |

### 6.2 MPM tool-surface measurements

`agent_installation/mpm-opencode/measure/measure_tool_surface.py` — starts
`mpm-mcp` against the pinned workspace, performs the MCP handshake, issues
`tools/list`, and exits. **It calls no tool and writes nothing.**

| Surface | Tools | Description bytes | Surface bytes | ~Tokens/turn |
|---|---|---|---|---|
| **MCP compact (default)** | **3** | **1 142** | **2 244** | **~561** |
| MCP full (`MPM_EXPOSE_ALL_TOOLS=1`) | 23 | 18 755 | 43 947 | ~10 987 |
| **Δ (full − compact)** | **+20** | **+17 613** | **+41 703** | **+10 426** |
| **Ratio** | | | **19.58×** | |

Compact tools: `mpm_context` (558 B desc), `mpm_help` (253 B), `mpm_memory`
(331 B).

**The compact-surface premise is confirmed.** The full surface would cost
**~10.4 k more tokens on every single turn**; the compact surface is
**19.6× smaller**.

**But the OpenCode adapter does not use the MCP surface.** It bypasses
`mpm-mcp` entirely and registers 17 tools flat:

| Component | Bytes |
|---|---|
| 14 domain tool descriptions | 9 137 |
| 3 standalone tool descriptions | 1 500 |
| 13 × shared fat-RPC input schema | 4 979 |
| 3 standalone input schemas | 2 168 |
| **OpenCode plugin surface total** | **17 784 B ≈ 4 446 tokens/turn** |

**The OpenCode adapter's tool surface is ~7.9× the MCP compact surface and
~40% of the MCP full surface.** The compact-surface optimisation — a
deliberate, contract-tested design decision in
`docs/dev/tool-surface-baseline.md` §1 — **simply does not apply to the
OpenCode adapter**, because the adapter never goes through the MCP server.

| Mechanism | Cost |
|---|---|
| `mpm_help action=list` (discovery step 1) | 5 234 B ≈ 1 308 tok |
| `mpm_help action=show mpm_work` (step 2) | 4 952 B ≈ 1 238 tok |
| **Full discovery round-trip** | **10 186 B ≈ 2 546 tok** |
| `read_wake_context` (default projection) | 12 283 B ≈ 3 070 tok |
| `read_wake_context` (`projection: "compact"`) | 3 821 B ≈ 955 tok |
| `read_wake_context` (`format: "system-prompt"`) | 2 151 B ≈ 538 tok |

The discovery round-trip costs **~4.5× the entire compact tool surface**, but
it is a **one-time per-session** cost while the tool surface is **per turn** —
so compacting pays for itself after ~4.5 turns. That is a defensible
trade-off and is **not** a finding against the design.

### 6.3 Behavioural fixtures — what the agent actually did

Three fixtures, each against a disposable MPM workspace seeded with known
artefacts (one decision about the FTS5/`-lm` build flags, one lesson about
`SetMaxOpenConns(1)`, one plain memory about OpenCode project ids, one open
work item). Tool calls counted from the substrate's **own** `tool_invocations`
audit — no new instrumentation.

#### F1 — retrieval

> *"Our CI build is failing with `undefined reference to 'log'`. What does MPM
> already know about this, and what should I do? Use your persistent memory
> tools to look it up."*

**26 MPM invocations**, 11 host-level `execute` calls (the host batches
several MPM calls per `execute` via `Promise.all`).

| Tool / action | Count |
|---|---|
| `mpm_memory / query` | **13** |
| `mpm_context / query_global_rules` | 2 |
| `mpm_context / read_wake_context` | 1 |
| `mpm_context / read_directives` | 1 |
| `mpm_context / proactive_recall_hint` | 1 |
| `mpm_lessons / list`, `mpm_lessons / search` | 1 each |
| `mpm_evidence / list`, `mpm_confidence / show`, `mpm_references / list`, `mpm_handoff / list`, `mpm_system / list_clusters` | 1 each |
| `mpm_retrieval_diagnose` | 1 |

**Outcome: the correct answer was retrieved and the reasoning was correct.**
The agent identified the three-flag set, mapped `undefined reference to
'log'` to the missing `-lm`, and even read the open work item from the wake
context. It also volunteered two accurate caveats (the working directory was
empty; the decision sits at 0.60 confidence).

**Two measured inefficiencies:**

1. **13 `mpm_memory / query` calls for one question.** The agent's own
   report: *"my first query `undefined reference to log CI build` returned
   **0 hits**. MPM's FTS5 builder ANDs all tokens … Single-term queries hit
   it immediately."*

   **Session C verified this independently** (§6.3.1) — it is a
   **retrieval-quality property of the substrate**, not a tool-surface
   defect, and it is a finding for **Session B**, not for the adapter.
2. **The first call was `mpm_retrieval_diagnose`** — a *diagnostic* tool
   (it explains *why* a result ranked where it did), used as a *retrieval*
   tool. It returned nothing useful and cost a call. The agent then used the
   host's native `search()` tool to discover the right action. **This is a
   tool-description problem**, but a small one: `mpm_retrieval_diagnose`'s
   description does say *"Use when you want to understand WHY a result ranked
   where it did"*, and it is the only tool whose description mentions
   ranking. The agent still reached for it first.

#### 6.3.1 FTS5 query semantics — independently characterised

The agent's report was **re-verified by Session C** with direct CLI calls,
because it is the single most consequential retrieval observation in this
report and it was initially second-hand.

Measured against the dogfooding workspace (`mpm_memory query`, `limit: 8`):

| Query | Hits |
|---|---|
| `linking` | **2** |
| `CI` | **6** |
| `linking zzzznotpresent` | **0** |
| `undefined reference to log CI build` | **0** |

and against the fixture workspace, where the FTS5/`-lm` decision lives:

| Query | Hits |
|---|---|
| `fts5` | 1 |
| `CGO_LDFLAGS` | 1 |
| `CGO_LDFLAGS fts5` | 1 |
| `CGO_LDFLAGS fts5 linking` | 1 |
| `CGO_LDFLAGS fts5 linking undefinedreference` | **0** |

**Characterisation: the FTS5 `MATCH` string is space-separated, so FTS5
applies implicit AND across every token. A single token that matches no
record zeroes the entire result, even when every other token matches the
same record.**

`mpm_retrieval_diagnose` confirms the mechanism from the other side: its
"Stage 1 — Query Transformation" reports `BuildFTS5Query` appending a `*`
prefix per token, and "Stage 2 — FTS5 Execution" reports the `MATCH`
string sent to SQLite.

**Why this matters more than a retrieval quirk.** A multi-word query that
returns nothing is **indistinguishable from "the knowledge does not
exist"**. The agent has no signal that it should retry with fewer tokens —
and indeed it did retry, thirteen times, in F1. This is a plausible
contributor to Session B's "missed retrieval opportunities" question and
should be read together with it.

Two further measured properties of the current retrieval configuration,
worth recording because they change how any behavioural number should be
read:

* `retrieval.mode` is **`lexical_only`** in both workspaces, with
  `semantic_only: false` and `vector_weight: 0.5`. **The semantic/vector
  layer is not active**, so all the measurements in this report exercise
  lexical retrieval only.
* `retrieval_threshold` is `-3` and the FTS5 BM25 values in the sampled
  workspace were `min=-1.106, max=-0.696` — i.e. **every** candidate was
  inside the threshold, so the threshold was not what discarded them.
  (`mpm_retrieval_diagnose` reported "FTS5 found rows; HybridSearch
  discarded them" — 7 found, 5 kept — but `mpm_memory query` on the same
  query returned the same 5, so the two surfaces **agree**; the earlier
  reading of a discrepancy was a measurement error on Session C's side, in a
  probe that read the wrong response key. Corrected here rather than left
  in the record.)

**Discovery behaviour observed:** the agent used the **host's** `search()`
primitive (`search({query:"memory query search list actions",
namespace:"mpm_memory"})`) to find MPM's actions, not `mpm_help` — because
`mpm_help` is not in the plugin's surface (§7.2). It also searched for a
pointer-resolution tool (`search({query:"resolve memory pointer …"})`) and,
finding none, **fell back to re-querying by text** — which is why F1 needed
13 queries instead of 1 query + 1 resolve.

#### F2 — specialist discovery (`mpm_work`)

> *"Create a tracked work item titled 'Session C follow-up' … Then tell me
> its id."*

**Outcome: PASS, by the shell escape hatch.** `mpm_work / create` ×2,
`list` ×2, `show` ×1 — **all with `framework_session_id = NULL`**.

The agent could not find `mpm_work` in the tool surface (correct — it is not
there), discovered it through the host's `search()` primitive, and invoked it
via `mpm call mpm_work --payload …` in the shell. **It worked. But the
session linkage was lost**, because `ctx.shell.hook("create.before")` carries
no `sessionID` (already documented in `opencode-sdk.d.ts`).

**This is the concrete cost of the missing `mpm_work` and `mpm_help`:** the
contract's `task.boundary` event type names `mpm_work` as its required
capability, and in the OpenCode adapter that capability is only reachable in a
way that **cannot be attributed to a session**.

#### F3 — classification and preservation

> *"We just decided to drop the legacy 77-tool plugin in favour of the
> 17-tool Fat-RPC surface, because the legacy surface cost about 10k tokens
> per turn … Record this architectural decision with MPM so the next agent
> inherits it. Then confirm what you recorded and where."*

**15 MPM invocations. Outcome: PASS on every axis C was asked to measure.**

| Behaviour | Result |
|---|---|
| **Knowledge-type classification** | **PASS.** The agent chose `mpm_decisions / record` — the correct typed operation for an architectural decision — **not** `mpm_memory / save`. |
| **Preservation of a decision** | **PASS.** Decision `90aedb4e16bbd0e1` created, with context, choice, rationale and tags. |
| **Supporting evidence** | **PASS.** `mpm_evidence / add` ×2 attached to the decision, with `artifact_type: "decision"`. |
| **Cross-session continuation** | **PASS.** `mpm_handoff / write` — handoff `f3968fd8e027e4a8` written with the decision pointer, the commitment, and an open question. |
| **Read-back verification** | **PASS.** The agent verified the write via the API, direct SQL *and* the substrate's `tool_invocations` audit. |
| **Isolation discipline** | **PASS.** It noticed the harness pinned a disposable workspace, explicitly declined to write to `~/.mpm` outside the sanctioned interface, and flagged that `promote_to_global` had failed because the shared DB was not attached. |
| **Confidence** | `mpm_confidence / show` used appropriately. |
| **Cost** | 3 × `mpm_memory / query`, 2 × `mpm_evidence / list`, 2 × `mpm_evidence / add` — proportionate. |

**Conclusion on classification: no defect found.** The typed operations and
the agent instructions together produce correct classification. **Do not
change the classification guidance.**

---

## 7. Concrete failures, with supporting evidence

### 7.1 F1 — the wake context is never injected (production defect)

**The bug, in the installed payload**
(`~/.mpm/agent_installation/mpm-opencode/dist/index.v2.js`, line 177 —
identical in `src/index.v2.ts`):

```js
const payload = r.payload;
return payload?.context ?? payload?.text ?? "";     // ← neither key exists
```

**What the substrate actually returns** for the exact request the plugin
makes:

```
$ mpm call mpm_context --payload '{"action":"read_wake_context","params":{"format":"system-prompt"}}'
top-level keys: ['content', 'format', 'success']
```

The text is under **`content`** (2 045 bytes of real wake context, verified
by inspection). Neither `context` nor `text` is present.

**Consequences, all measured:**

1. `fetchWakeContext()` returns `""` on every call.
2. `if (context)` is false → `event.system.push(…)` **never runs** → **the
   MPM wake context never reaches the agent's system prompt**.
3. `wakeContextCache.set(…)` **never runs** → the cache never populates →
   **every** `session.hook("context")` firing spawns another `mpm call
   mpm_context`.

**Measured cost:** across three fixture sessions, the substrate's own audit
recorded **57 `read_wake_context` invocations made by the plugin itself**
(`framework_name = "mpm-cli"`), i.e. **~19 per session**, each a full
`mpm` process spawn + database open at **~39 ms**. None of them carried a
session id, because `fetchWakeContext` uses `callMpm` **without** the
provenance env that `callMpmWithProvenance` applies.

**And it is not hypothetical — it is happening in the live install right
now.** The live `~/.mpm` database was read read-only at the end of this
session:

```
read_wake_context in the last hour:  370
read_wake_context in the last 30 min: 367
```

Every one of them is `framework_name = "mpm-cli"` with a **null**
`framework_session_id`, i.e. the plugin's own `fetchWakeContext` path, not an
agent call. The live plugin is spending **~12 process spawns per minute** on
a fetch that, per the above, **returns nothing to the agent**. The only
other live invocations in the same window are the plugin's own boot-time
`mpm_system health_check` and one read — **no knowledge write of any kind
reached `~/.mpm`** (0 memories created in the two hours covered by this
session).

**The irony worth stating plainly:** the wake context's own content says
*"On every session start, call read_wake_context before responding to the
user."* The agent never receives that text. In F1 the agent nevertheless
called `read_wake_context` once on its own — which is why the fixture still
produced a good answer. **The system works in the fixtures by accident, not
by design.**

**The narrow fix.** Read the string field `content`:

```js
const text =
  typeof payload?.content === "string" ? payload.content
  : typeof payload?.context === "string" ? payload.context
  : typeof payload?.text === "string" ? payload.text
  : "";
```

Note carefully: the **`projection: "compact"`** variant returns
`{context: {…an object…}}` — `context` is a **dict**, not a string. So
`payload.context` is not a drop-in substitute for text, and the fix must
type-check, not merely key-check. `params.format = "system-prompt"` is the
correct request; it is already what the plugin sends.

**Regression test** (see §10).

### 7.2 F3 — the plugin surface omits six registered capabilities

| Missing from the OpenCode plugin | Consequence |
|---|---|
| `mpm_help` | **Progressive discovery is unavailable.** The MCP surface has it as one of its three tools; the plugin does not expose it at all. The agent instead uses the host's native `search()` primitive — which works, but is an *OpenCode* facility, not an MPM one, and it finds tool *names*, not MPM *actions*. |
| `mpm_resolve` | **Pointers cannot be resolved.** The wake context and every artifact carry `mpm://memory/<id>` pointers. Measured in F1: the agent searched for a resolve tool and fell back to re-querying by text — 13 queries where 1 query + 1 resolve would do. The contract's `mpm_resolve` exists precisely for this. |
| `mpm_work` | **`task.boundary` (§4) has no in-surface capability.** Reachable only shell-side, with `framework_session_id = NULL` (§6.3 F2). |
| `mpm_blob_read`, `mpm_blob_search` | Blob-backed artifacts are unreachable in-surface. |
| `mpm_route` | Mode/persona routing is unreachable in-surface (`mpm_context / route` is present, but `mpm_route` itself is not). |

**This is an inconsistency between the two adapters, not a redesign
proposal.** The MCP server's compact surface is *deliberately* 3 tools with
`mpm_help` as the escape hatch; the plugin's surface is 17 flat tools with
**no** escape hatch. The result is that the plugin gets the token cost of
the full surface *and* the discoverability of the compact one, which is the
worst of both.

**Two candidate corrections, for A to choose:**

* **(a) Add `mpm_help` (and `mpm_resolve`, `mpm_work`) to the plugin's
  surface** — 3 more tools, ~1.3 kB of description, restoring discovery and
  pointer resolution. Cheapest, and it aligns the plugin with the compact
  MCP design.
* **(b) Reduce the plugin to the 3-tool compact surface** (`mpm_memory`,
  `mpm_context`, `mpm_help`) and let `mpm_help` + `mpm call` reach the
  specialists, exactly as the MCP server does. Saves ~15 kB ≈ 3.9 k tokens
  per turn. Bigger change; it would break any consumer relying on the 17
  flat tools.

**C's recommendation is (a) for this release** — it fixes the two measured
functional gaps (pointer resolution, work items) at low cost, and (b) is the
right long-term shape but should not be forced into a release candidate
without its own measurement. **This is A's decision, not C's.**

### 7.3 Non-defects — things that look like defects and are not

* **13 queries for one question** — the FTS5 implicit-AND behaviour
  (§6.3.1), a retrieval-quality property of the substrate. **Session B's
  domain.** C records it; C does not propose a fix. The severity is not the
  query count but that **an empty result is indistinguishable from "the
  knowledge does not exist"** — which is what turns one failed multi-word
  query into thirteen.
* **The agent's first call being `mpm_retrieval_diagnose`** — a
  tool-description issue affecting exactly one tool; the description is
  accurate and the agent recovered immediately.
* **`framework_session_id` missing on shell-path calls** — a consequence of
  the host's `shell.create.before` payload having no `sessionID`. It is a
  **host limitation**, documented in `opencode-sdk.d.ts` and confirmed here.
  The only in-adapter mitigation is to prefer in-surface tools (which is
  §7.2's argument).

---

## 8. Minimal proposed lifecycle integration

The smallest adapter work that delivers reliable memory behaviour, given
everything measured above.

### 8.1 Principles

1. **Do not redesign the MCP surface.** §6.2 confirms the compact-surface
   premise. The measured problems are in the *plugin's* surface, and the
   fix there is additive.
2. **Do not decide what becomes trusted knowledge.** The adapter observes and
   forwards; Session B interprets. §1's M5 endorsement is the reason this
   boundary is cheap to hold.
3. **Prefer the hook surface over the event stream.** The event stream is
   unreachable from a plugin in v2.1.4 (§4.1.1). Any design that assumes it
   is wrong for this host.
4. **Capture identifiers now, decide storage later.** §4.4 shows the
   callID, messageID and agent are available at the hook and are currently
   discarded. Capturing them costs nothing now and is impossible to recover
   later.

### 8.2 The adapter work, in priority order

> **Priority note (added after reading Session B's audit).** B concludes that
> the retrospective-detection path needs **no new event framework, no new
> table and no schema change**, and that the lifecycle-event store
> (contract §6.1) is **not on the critical path**. C's measurements agree.
> That reorders the priorities below: **A1, A3 and A6 — none of which touch
> the contract — are worth more than A2, A4 and A5, which are blocked on
> adjudication and which build a store that B's design does not need.** The
> contract work is still worth doing, but it should not gate the release
> candidate.

| # | Change | Why | Size | Needs the contract? |
|---|---|---|---|---|
| **A1** | **Fix the wake-context extraction** (§7.1) | The central mechanism is dead. Highest value by a wide margin. | ~5 lines + 1 test | **No** |
| **A2** | **Capture `id`, `messageID`, `agent` in the tool path** (§4.4) | Required for strict-mode `tool.outcome` identity (§1.2). | ~5 lines | Yes (C2) |
| **A3** | **Add `mpm_help` (+ `mpm_resolve`, `mpm_work`) to the plugin surface** (§7.2) | Restores discovery, pointer resolution and the `task.boundary` capability. | ~20 lines, additive | No — but it **is** a shared-interface change under `coordination.md` §2, so it needs A's PR |
| **A4** | **Register `ctx.tool.hook("execute.before"/"execute.after")`** and emit `tool.outcome` | The only directly observable, host-keyed lifecycle signal in v2.1.4. | ~40 lines | Yes (C2) |
| **A5** | **Register `ctx.session.hook("compaction")`** and emit a single `compaction.context` | The only compaction signal available. | ~20 lines | **Yes — requires A's M2 adjudication first** |
| **A6** | **Stamp the session id on the plugin's own subprocess calls** | `fetchWakeContext` uses `callMpm` without the provenance env; ~19 per-session wake-context calls are unattributed. | ~3 lines | **No** |

**A1, A3 and A6 are independent of the contract** and can proceed on A's
approval alone. A2, A4 and A5 depend on the contract being acknowledged
(A2 and A4 on the §1.2 correction, A5 on M2).

### 8.3 What the adapter must **not** do

* It must not infer session start, resume, idle or completion from
  timestamp gaps (§3.4, §3.6).
* It must not map an unknown host event onto a neighbouring type (§3.4).
* It must not promote anything (§3.7).
* It must not fabricate a project id (§3.9.5) — which is why §4.3's proposal
  keeps the adapter's own remote check as the *verifier*.

---

## 9. Event fields and guarantees needed from Session B

### 9.1 Fields Session B needs in the envelope

| Field | Needed for | Note |
|---|---|---|
| `event_type` | **Mandatory in the `event_id` derivation** (§1.2) | Without it, nested tool calls collide. |
| `host_tool_name` | distinguishing nested tool calls | The host supplies it in the hook payload; it is also the natural `meta` field. |
| `framework_session_id` | grouping a session's observations | Available as `ses_…` from the tool path. |
| `observed_at_ms` | ordering (§3.6) | Adapter-supplied, `Date.now()`. |
| `host_event_key` | strict-mode dedup | The host's `call_…`. Must be recorded **as received**, not re-derived. |
| `meta.host_agent` | distinguishing agents within a session | `build` / `plan` / … |
| `meta.host_message_kind` | turn boundaries | Available from `messageID`'s prefix. |
| `meta.duration_ms` | latency analysis | `execute.after` has no duration; the adapter can measure it around the call, or pair `before`/`after` by `id`. |
| `outcome_class` | classification of failures | **Requires the host-status → `ToolOutcomeClass` mapping (§1, M7).** |

### 9.2 Guarantees Session B needs from the adapter

1. **Every emitted event is observed, never inferred.** The adapter emits
   only what a hook delivered.
2. **Unknown host hooks are counted, never mapped.** §3.4's rule, and it is
   cheap: the adapter knows the exact list of hook names it registered.
3. **The `event_id` derivation is a pure function of the documented input.**
   It must be recomputable by B from the stored envelope alone. §1.2's
   correction is required for this to be true *and* collision-free.
4. **The host tool name is preserved verbatim** in `meta`, so B can group by
   tool without re-parsing.
5. **The absence of a capability is reported.** A missing hook must produce a
   capability-report entry, not silence — §4's "an absent capability yields
   no event and no placeholder — only a capability-report entry" (§5).

### 9.3 Questions for Session B

* Does B's critic need the **tool name** at all, or only the outcome? This
  determines whether `tool.outcome` events need one envelope per tool or one
  per invocation.
* Does B's existing proposal path have a place to carry `event_id`s as
  evidence references (contract §9 Q4)?
* Given that the host event stream is unreachable from a plugin, does B's
  retrospective analysis have to work from **hook-derived observations only**?
  If B needs the full `session.*` event vocabulary, that is a **host-access**
  problem, not an adapter problem, and A needs to know.

---

## 10. Contract changes requiring Session A's decision

Stated as changes to specific MSI items, per launch gate §4.4.

| # | Item | Change requested | Urgency |
|---|---|---|---|
| **C1** | **M2 (§4 vocabulary)** | Replace `session.started` + `compaction.started` + `compaction.completed` with `session.observed` + `compaction.context` (one observed event each), **or** admit an inferred `session.started` with a declared basis. | **Blocking** |
| **C2** | **M3 (§3.5)** | Add `event_type` and `host_tool_name` to the strict-mode derivation input. Add `TestEventID_NestedToolCallsDoNotCollide` to §8. | **Blocking** |
| **C3** | **M3 (§3.3)** | Adopt `ctx.location.project.id` as the canonical project id **when the adapter independently confirms a git remote exists**; keep §3.3's chain as the verifier and as the fallback; add `project_id_source: "host-git-remote"`. | High |
| **C4** | **M7 (§3.7)** | Pin the host-status → `internal.ToolOutcomeClass` mapping table. | High |
| **C5** | **§6.2 (transport)** | Decide with the measured cost in hand: **~39 ms per `mpm call`** spawn; the current adapter would need **~2 spawns per tool event**. A batched or socket transport is strongly indicated for any per-tool-event volume. | High |
| **C6** | **§5 (host capability table)** | Replace the table with §4.1–§4.5 of this report. Specifically: `tool.execute.before/after` **exist** (as `ctx.tool.hook`); `chat.message` and `command.execute.before` **do not exist**; `experimental.session.compacting` and `experimental.compaction.autocontinue` **do not exist** (the single `ctx.session.hook("compaction")` replaces them); `experimental.chat.system.transform` **exists** as `ctx.session.hook("context")`; the host's `event` hook **exists but is inert**. | High |
| **C7** | **§6.7 (`meta` allowlist)** | Add `host_tool_name` (already listed), `host_message_id` and `host_call_id`; the host supplies all three and §3.8's list omits the latter two. | Medium |
| **C8** | **§6.8 (byte ceilings)** | The 8192-byte envelope ceiling is comfortable: the richest measured payload is `execute.after`'s `result`, and the allowlist admits no payload body. **No change needed.** | — |

---

## 11. Recommended first implementation PR and acceptance tests

### 11.1 Recommended first PR — the wake-context fix (A1)

**One PR, one defect, one behaviour.** It is independent of the contract, it
is a production defect in the *installed* payload, and it is small enough to
review in minutes.

**Title:** `fix(opencode): read the wake context from `content`, not `context`/`text`

**Changes (all inside `agent_installation/mpm-opencode/`):**

1. `src/index.v2.ts` — `fetchWakeContext`: extract the **string** field
   `content` (with `context`/`text` as typed fallbacks). ~5 lines.
2. `src/index.v2.ts` — `fetchWakeContext`: use `callMpmWithProvenance` (or
   pass the session id) so the wake-context call is session-attributed
   (A6). ~3 lines.
3. `tests/` — a new unit test for the extractor covering all four shapes
   (`content` string, `content` absent, `context` as a **dict**,
   `context` as a string).
4. `tests/` — a regression test asserting that a second `context` hook for
   the same `sessionID` performs **no** second subprocess spawn (the cache
   now populates).
5. Rebuild `dist/` and bump the recorded build provenance.

**Acceptance tests:**

| # | Test | Asserts |
|---|---|---|
| T1 | `test_wake_context_extractor_reads_content` | A payload `{content: "…", format: "system-prompt", success: true}` yields the text. |
| T2 | `test_wake_context_extractor_ignores_non_string_context` | A payload `{context: {…object…}}` yields `""`, not `"[object Object]"`. |
| T3 | `test_wake_context_extractor_prefers_content_over_context` | `{content: "A", context: "B"}` yields `"A"`. |
| T4 | `test_wake_context_is_injected_into_system_prompt` | After the hook fires with a `sessionID`, `event.system` contains the wake text. **This is the test that fails today.** |
| T5 | `test_wake_context_is_cached_per_session` | The second `context` hook for the same `sessionID` performs **zero** subprocess spawns. |
| T6 | `test_wake_context_call_is_session_attributed` | The wake-context call stamps `MPM_PROVENANCE_FRAMEWORK_SESSION_ID`. |
| T7 | `test_wake_context_cache_survives_tool_calls` | A tool call between two context hooks does not invalidate the cache. |

**Why this PR first, and not the contract work:** the contract work is
blocked on A's adjudication of C1 and C2; this PR is not blocked on
anything, it fixes a defect in the *shipped* payload, and it makes every
subsequent measurement more meaningful — with the wake context actually
delivered, the tool-behaviour numbers in §6.3 change, and re-measuring them
on a fixed adapter is worth more than re-measuring them on a broken one.

### 11.2 Second PR — identifier capture and the `tool.outcome` hook (A2 + A4)

**Blocked on:** C2 (the §1.2 derivation correction) and A's acceptance of
the amended M3.

**Changes:** capture `id`/`messageID`/`agent` in the tool path; register
`ctx.tool.hook("execute.before"/"execute.after")`; emit `tool.outcome`
envelopes with `idempotency: "strict"`, `operation: "present"`, and the
mapping from C4.

**Acceptance tests:**

| # | Test | Asserts |
|---|---|---|
| T8 | `test_tool_outcome_before_and_after_share_one_event_id` | The `before` and `after` envelopes for one tool call derive the **same** `event_id` (they are the same observation). |
| T9 | `test_tool_outcome_for_nested_calls_do_not_collide` | The outer `execute` and the inner `probe_tool` derive **different** `event_id`s. **This is the test C2 exists for.** |
| T10 | `test_event_id_is_stable_across_processes` | The same host key yields the same `event_id` in two separate processes. |
| T11 | `test_tool_outcome_carries_no_result_body` | The envelope's `meta` contains only allowlisted fields; the tool's `result` is not carried. |
| T12 | `test_tool_outcome_uses_existing_tool_outcome_class` | `outcome_class` ∈ `internal.ToolOutcomeClass`; no new vocabulary. |
| T13 | `test_tool_outcome_unknown_status_is_unknown_not_ok` | An unrecognised host status maps to `result_status: "unknown"`, never to `"success"`. |

### 11.3 Third PR — plugin surface additions (A3)

**Blocked on:** A's acceptance, because the tool registry is a shared
interface under `coordination.md` §2.

**Changes:** register `mpm_help`, `mpm_resolve` and `mpm_work` in the
plugin's `ctx.tool.transform`.

**Acceptance tests:**

| # | Test | Asserts |
|---|---|---|
| T14 | `test_plugin_surface_includes_discovery_and_resolve` | The plugin's tool list contains `mpm_help`, `mpm_resolve`, `mpm_work`. |
| T15 | `test_plugin_surface_remains_within_byte_budget` | The plugin's total surface stays below an agreed ceiling (baseline: 17 784 B). |
| T16 | `test_pointer_resolution_round_trip` | A pointer from a wake-context candidate resolves to its artefact. **This is the F1 failure mode, turned into a test.** |

---

## 12. Reconciliation with Session B's first deliverable

Session B delivered concurrently (handoff `468d62b0a7992f15`, report
`docs/dev/session-b-audit.md` on `dev/session-b-core`, commit `93011a0a`).
This section records where the two investigations agree, where they differ,
and where B's findings change C's recommendations.

### 12.1 Agreement

| B's finding | C's position |
|---|---|
| `196d17dcc2a6bf6d` — `framework_session_id` population is path-dependent | **Agree, and C localises it.** Measured: the plugin's own wake-context calls carry **no** session id (57 of them, §7.1), and every `mpm_work` call the fixture agent made went shell-side and also carried none (§6.3 F2). B's "path-dependent" characterisation matches: the id is present on the plugin's in-surface tool path and absent everywhere else. |
| `cc86a851339b1914` — the MPM environment is not inherited by the OpenCode process | **Agree, and C adds the mechanism** (§3): `MPM_DB_PATH` is **ignored** by `mpm` and `mpm-mcp` (only `mpm-critic`/`mpm-scheduler` honour it), so the workspace pin is what actually decides where plugin tool calls write. Only `MPM_WORKSPACE` and `MPM_BINARY` can be made to apply to the running host by configuration. |
| B: the retrospective hunt needs **no new event framework, no new table, no schema change**; `artifact_provenance.invocation_id → tool_invocations.invocation_id → framework_session_id` works on live data | **Agree, and it changes C's priorities.** If the retrospective detection path does not need a lifecycle-event store, then the contract's §6.1 storage decision is **not on the critical path** for the sprint's headline goal, and the adapter's highest-value work is the two fixes that need no contract agreement at all. §8.2 is re-ordered accordingly. |

### 12.2 Difference

**B acknowledged M1–M7 with no blocking objection** (decision
`3a987eca2c708fac`). **C acknowledged with two blocking objections** (§1).
The contract is therefore **not agreed**, and the two positions differ on
exactly the two points where C has measured evidence B did not have:

* **M2** — whether the §4 vocabulary rows are populatable at all. B's audit
  was of the *core*; the host's hook surface is C's evidence.
* **M3** — whether the §3.5 derivation is collision-free. C's nested-call
  measurement is the only evidence on this, and it is specific: the host
  gives one `callID` per *invocation*, not per *tool call within it*.

**A should treat C's two objections as the ones requiring adjudication**,
and should note that B's acceptance does not cover them.

### 12.3 B's four questions to C — answered

Recorded in the shared dogfooding workspace as handoff `3c07a972611869dc`.

| B's question | Answer |
|---|---|
| Does OpenCode v2.1.4 deliver a stable per-tool-call key? | **Yes** — `id` = `call_<hex>`, stable across the before/after pair. **But** nested tool calls share it, so the derivation must include `event_type` and the tool name (blocking objection C2). |
| Any event for a session that never called an MPM tool? | **No, not today.** The adapter's only per-session signal is the wake-context injection, and it is broken (§7.1). Once fixed, `ctx.session.hook("context")` fires for **every** session, MPM-using or not, and carries the `sessionID` — which is exactly the per-session signal B needs. Session *creation* itself is not observable in this host version. |
| A wall-clock timestamp? | **Yes** — `Date.now()`, millisecond precision. The host gives the plugin no monotonic per-session counter, so `host_seq` will be `null` in practice (§3.6's NULL ordering already covers it). |
| Is the host's project identity identical across linked worktrees? | **Yes**, and measured across seven remote spellings and three linked worktrees (§4.3). One divergence: with no git remote the host still emits a path-derived id, which §3.9.5 forbids adopting silently. |

### 12.4 One open question C asks B

**Does the critic's proposal shape need to carry the host tool name as well
as the callID?** B's finding `64abe497b6d98572` — that the current proposal
shape cannot carry resolvable evidence — is the same class of problem, and
it lands on the same nested-call ambiguity: two events derived from one
callID are indistinguishable unless the tool name travels with them. If B's
proposal citation carries only `invocation_id`, it inherits the ambiguity.

---

## 13. Reproducibility record


| Item | Value |
|---|---|
| Repository revision | `70deac2bb3fce3c63da28bfbd9ca82ca2c4d1e2e` (branch `dev/session-c-opencode`, equal to `origin/main` at the time of writing) |
| Host | `opencode v2.1.4`, `~/.opencode/bin/opencode`, ELF x86-64, not stripped |
| Host plugin payload | `~/.mpm/agent_installation/mpm-opencode/dist/index.v2.js` (installed, 2026-10-24) — **read only** |
| Dev build | `0.1.0-dev+g70deac2b.t1793040708` (`~/src/mpm/.build/bin/mpm`, `mpm-mcp`) |
| Installed runtime | `~/.mpm` @ `3df72315` — **not modified, not deployed to** |
| Model | `minimax-coding-plan/MiniMax-M3.1-Flash` via OpenCode v2.1.4 |
| MPM workspace (measurement) | `/tmp/opencode/probe-c/mpm-scratch` (disposable, seeded per fixture) |
| MPM workspace (shared knowledge) | `~/.mpm-dogfood` — **writes only**: decision `8cf1f0e3cc41465e`, memory `880914fcb252f6ce`, lessons `3d4c577a76580dc5`, `4007e7bcf2ae89ce`, `850112858d8562f8`, `24486718472717f8`, handoff `de56dc39fcf8fb14` |
| OpenCode isolation | `XDG_CONFIG_HOME`, `XDG_DATA_HOME`, `XDG_STATE_HOME`, `XDG_CACHE_HOME` → `/tmp/opencode/probe-c/*`; verified via `opencode debug paths` |
| Live database access | read-only (`file:…?mode=ro`), once, for project/worktree grouping |
| Harness | `agent_installation/mpm-opencode/measure/` — `measure_tool_surface.py`, `fixture_run.sh`, `probe/host_surface_probe.js`, `probe/project_identity_probe.js` |
| Raw evidence | `agent_installation/mpm-opencode/measure/evidence/` — probe runs, remote-matrix runs, tool-surface measurement |
| Go / Node | Go 1.26.6; Node 20 |

**Isolation statement.** No write of any kind reached `~/.mpm`, the live
OpenCode database, `~/.local/share/opencode`, the live OpenCode
configuration, another session's worktree, or the installed plugin payload.
The only persistent writes made by this session are (a) the shared
dogfooding MPM records listed above and (b) the new files under
`agent_installation/mpm-opencode/measure/` and this document, on
`dev/session-c-opencode`.

---

## 14. What Session C did **not** do

* Did not implement, merge or deploy anything.
* Did not modify the tool registry, `package.json`, the lockfile, or any
  shared interface.
* Did not modify the installed plugin payload at `~/.mpm/agent_installation/`.
* Did not modify another session's worktree.
* Did not modify the live database, the live systemd services, or the live
  OpenCode configuration.
* Did not implement anything against the contract — two blocking objections
  are open.
