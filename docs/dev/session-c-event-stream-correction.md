# Session C — second pass: the host event stream is reachable, and the M2/M3 objections are withdrawn

> **Author:** Session C — OpenCode adapter and behavioural evaluation.
> **Branch:** `dev/session-c-opencode`, based on `70deac2b` (the reported
> integration baseline; equal to `origin/main` at the time of writing).
> **Worktree:** `~/src/mpm-opencode`.
> **Date:** 2026-10-26. **Host:** OpenCode v2.1.4. **Model:**
> `minimax-coding-plan/MiniMax-M3.1-Flash`.
> **Status:** investigation, measurement and correction only. **No
> implementation, no merge, no deploy, no change to the tool surface, the tool
> registry, `package.json` or the lockfile.**
>
> **This document supersedes three findings in
> [`session-c-host-findings.md`](session-c-host-findings.md).** It does not
> replace that report: the original is left in place, unedited, because the
> error it contains is itself worth inheriting (§1). Where the two disagree,
> **this document wins**, and the reason is stated in each case.

---

## 0. Executive summary

The first-pass report concluded that the OpenCode v2.1.4 **host event stream is
not reachable from a plugin**. That conclusion is wrong. It was produced by a
probe that called the API with the wrong shape and therefore never received an
event.

| # | Correction | Consequence |
|---|---|---|
| **V1** | **The host event stream is reachable from a plugin.** `ctx.event.subscribe({signal})` returns an **`AsyncIterable`**; the first-pass probe passed a *callback* and never iterated the result, so it observed zero events. Measured: **31 distinct event types** across three isolated runs, including `session.created`, `session.compaction.started`/`.ended`, `session.tool.*` and `session.execution.*`. | The §5 capability table, the §4.1.1 "inert stream" finding and the Session B dependency note ("B should assume hook-derived observations only") are all wrong and must be corrected. |
| **V2** | **`session.started` is directly observable** as `session.created`, carrying `sessionID`, a host `projectID`, `parentID`, `agent` and `model`. | **Blocking objection M2 is withdrawn.** |
| **V3** | **The compaction pair is directly observable**: `session.compaction.started` *and* `session.compaction.ended` were both captured live, plus `.delta` and `.failed` in the host vocabulary. | **Blocking objection M2 is withdrawn.** The proposal to replace the pair with one `compaction.context` type is unnecessary. |
| **V4** | **The host supplies a stable per-event id** (`evt_…`) and a millisecond `created` timestamp on every event, plus `sessionID`/`assistantMessageID`/`callID` on tool events. | **Blocking objection M3 is downgraded to non-blocking**: keying strict ids on the host event id cannot collide. The `event_type` addition remains a recommended hardening. |
| **V5** | **The event stream is a redaction hazard.** It delivers full tool *input*, tool *output content*, and the entire compaction *summary text*. | §3.8's allowlist is now the load-bearing control, not a formality. `payload` must never become "forward `event.data`". |
| **V6** | **The environment pins do not reach the running host.** No `MPM_*` variable exists in the desktop app, the CLI service, or the agent process; the plugin resolves `~/.mpm` and the **stale** binary `0.1.0-dev+g3df72315`. | The "shared dogfooding workspace" is only used when an agent explicitly exports `MPM_WORKSPACE` in a shell. Sprint knowledge is landing in the **live** database. This is the brief's environment requirement, and the honest answer is **not met**. |

**The contract position after this pass: M1–M7 are agreed with _zero_ blocking
objections** (was: two). Two non-blocking recommendations remain. §5 restates
the whole acknowledgement so that nothing has to be reassembled from two
documents.

---

## 1. How the first-pass error happened — and why it is worth keeping

The first-pass probe did this
(`measure/probe/host_surface_probe.js`, still in the tree):

```js
const disp = ctx.event.subscribe((...args) => { emit("host.event", {...}) })
```

The host builds the plugin's `event` namespace like this — extracted verbatim
from `/home/v/.opencode/bin/opencode` at byte offset `163945354`:

```js
_,{ …, event:{subscribe:(o)=>z(e.event.subscribe().pipe(Ga((r)=>T(r)),Ir((r)=>r)),o)}, … }
```

where `z` is `Plugin.Event.makeStreams`, whose return value is:

```js
(l,E)=>({ [Symbol.asyncIterator](){ … return {next, return} } })
```

So the signature is `subscribe(options?) -> AsyncIterable`, with `options.signal`
supported. The probe passed a **function** as `options` and **never iterated**
the returned object. `Object.keys(disp)` on that object is `[]`, which the
report read as evidence that nothing was there. It recorded
`{"dispType":"object","dispKeys":[]}` — and the absence of `host.event` lines
was then attributed to the host.

This is the failure mode worth inheriting: **a negative capability claim is only
as strong as the call that produced it.** The first-pass report treated "my
registration produced no events" as "the host publishes no events to plugins",
without checking the call shape against the host's own dispatcher. The
corrected probe records the returned value's shape *and* consumes it
(`measure/probe/event_stream_probe.js`), so a future reader can tell the two
failures apart from the capture alone.

**The first-pass report also contained the disconfirming datum.** Its own §4.2
records that `ctx.session.hook` accepts any name and never fires — i.e. that
"registered" and "delivers" are different facts in this host. The same
scepticism was not applied to `event.subscribe`.

---

## 2. Method

Three isolated runs, each with its own history:

| Run | Purpose | Events | Distinct types |
|---|---|---|---|
| `run1` | baseline trivial turn | 22 | 22 |
| `run2` | subagent spawned (child session created mid-turn) + manual compaction | 58 | 31 |
| `run3` | compaction triggered during a live turn, to capture the `.ended` half | — | — |

Isolation, verified before every run with `opencode debug paths`:

```
data    /tmp/opencode/probe-d/data/opencode
config  /tmp/opencode/probe-d/config/opencode
state   /tmp/opencode/probe-d/state/opencode
db      /tmp/opencode/probe-d/data/opencode/opencode.db
```

The live `~/.config/opencode`, the live session store, the live OpenCode
database and the installed MPM plugin payload were **read only, never
written**. No MPM workspace was touched by any run: the probe plugin imports no
MPM code and calls no MPM tool. The probe registers **no hook names at all** —
it cannot poison the tool dispatcher (see the first-pass report's §4.2 warning
about `ctx.tool.hook` typos).

Raw capture: `measure/evidence/event_stream_probe_run*.jsonl`. Probe source:
`measure/probe/event_stream_probe.js`. Static vocabulary extract:
`measure/evidence/event_vocabulary_session_static.txt`.

### 2.1 A note on the plugin entrypoint

The first attempt at this probe failed to load. The host's v2 loader rejects a
function-shaped default export:

```
Plugin must export a default definition with an id and an effect or setup function.
(cause: SchemaError(Expected object at ["default"]))
```

The installed MPM plugin satisfies this via `Plugin.define({id, setup})`. A
probe must export `{ id, setup }` (or use `Plugin.define`). This is recorded
because it is the difference between "probe ran and saw nothing" and "probe
never loaded", and the host logs the latter clearly — the first-pass harness
should assert on that log line before trusting a capture.

---

## 3. The measured event surface

### 3.1 Envelope

Every delivered event has exactly these top-level keys:

```json
{
  "id":       "evt_179f10f85001LYlIygAAyW5UCz",
  "created":  1793047203717,
  "type":     "session.execution.started",
  "location": { "directory": "/tmp/opencode/probe-d/work", "workspaceID": null },
  "data":     { "sessionID": "ses_e860ef18cffe6tCMsswTpjK2Th" }
}
```

* `id` — `evt_<hex>`, unique per published event. **This is the contract's
  first-choice `host_event_id`** (§3.5) and it makes strict-mode identity
  trivially available.
* `created` — unix **milliseconds**, host-stamped. Matches §3.6's precision
  requirement without adapter measurement.
* `location.directory` — the project directory the event belongs to.
* `data` — event-specific, and the only field that varies in shape.

### 3.2 Delivery guarantee — the host says it is volatile

The host's own API description for the same stream
(`opencode`, offset `160453401`) states:

> "Subscribe to events … **Volatile by contract: a slow consumer overflows and
> fails the stream, and events during disconnection are missed.**"

This matters for the contract in two ways:

1. **It justifies `best_effort` delivery** (§3.5 already refuses to claim
   exactly-once). The stream does not change that.
2. **The adapter must never block inside the iterator.** Any `await` on an MPM
   subprocess inside the `for await` body risks overflow and a failed stream.
   Envelopes must be queued and handled out-of-band.

### 3.3 Observed event types and payload shapes

31 distinct types were observed. The payload column is the exact `data` key
list, measured — not inferred.

| Event type | `data` keys | Notes |
|---|---|---|
| `session.created` | `sessionID, projectID, location, subpath, parentID, slug, title, agent, model, version` | **`session.started`** source. `parentID` gives the subagent/child boundary. `projectID` is the host's project identity. |
| `session.renamed` | `sessionID, title` | user-authored text |
| `session.instructions.updated` | `sessionID, delta` | instruction-content hashes |
| `session.inbox.enqueued` / `.delivered` | `sessionID, inboxID[, item]` | turn boundaries |
| `session.execution.started` / `.succeeded` | `sessionID` | **session activity / completion** |
| `session.step.started` | `sessionID, assistantMessageID, agent, model, started` | model identity without a hook |
| `session.step.streamed` / `.ended` | `+ finish, rawFinish, cost, tokens` | cost data |
| `session.text.started` / `.delta` / `.ended` | `sessionID, assistantMessageID, ordinal, [delta\|text]` | **assistant content — must not be forwarded** |
| `session.tool.input.started` | `sessionID, assistantMessageID, id, name` | `id` = `call_…` |
| `session.tool.input.ended` | `…, id, text` | **full tool input — must not be forwarded** |
| `session.tool.called` | `…, id, input, executed` | **full parsed input** |
| `session.tool.progress` | `…, id, metadata` | |
| `session.tool.success` / `.failed` | `…, id, content, metadata, executed` | **full tool output — must not be forwarded** |
| `session.compaction.started` | `sessionID, reason, inputID` | **`compaction.started`** |
| `session.compaction.delta` | `sessionID, text` | **compaction summary text** |
| `session.compaction.ended` | `sessionID, reason, model, text, cost, tokens` | **`compaction.completed`**; full summary text |
| `session.usage.updated` | `sessionID, cost, tokens` | |
| `*.updated` (`agent`, `model`, `provider`, `command`, `plugin`, `skill`, `reference`, `websearch`, `integration`) | `[]` | catalogue invalidation; not lifecycle |
| `location.shutdown` | `[]` | host teardown |

Vocabulary that exists in the host but was **not** observed in these runs (it
is present in the host's event projection helper, which special-cases
`session.compaction.ended`, and in the static token extract):
`session.idle`, `session.error`, `session.execution.failed`,
`session.execution.interrupted`, `session.tool.failed`,
`session.compaction.failed`, `session.archived`, `session.deleted`,
`session.forked`, `session.moved`, `session.child.*`,
`session.message.content.updated`.

**Honest status of the unobserved ones:** named in the host binary and handled
by its event projection code, therefore *expected* to be deliverable, but not
live-captured here. They are classified **directly observable (expected), not
yet exercised** — not "inferable", and not claimed as verified.

### 3.4 One measured limitation: the adapter's own first session can be missed

In `run1` and `run2` the **parent** session's `session.created` was **not**
delivered, while the **child** session created mid-turn **was**. The parent
session is created during host startup, before the plugin's iteration loop
begins consuming; the stream is a live feed with no replay.

Consequences, stated precisely:

* `session.created` is **directly observable for sessions created while the
  adapter is consuming** — every child/subagent session, every session created
  after the host is up.
* For the adapter's *own first* session the event may be missed, so the
  adapter must not treat its absence as "no session started".
* This is why `session.observed` should remain the base event: it is
  unconditional. `session.started` is emitted **when observed**, which §4
  already permits (every type is optional, and a missing capability yields no
  event).
* It is **not** evidence for inference: the adapter must not synthesise
  `session.started` from "first event for this session". §3.4's
  observed-only rule still binds.

---

## 4. The redaction hazard (new, and the most important design constraint)

The event stream makes the *easy* implementation catastrophically wrong:

```js
// DO NOT DO THIS — it captures prompts, file contents, command output and
// compaction summaries, all of which §3.8 says must never be collected.
for await (const ev of ctx.event.subscribe({})) forward(ev.data)
```

Measured fields that carry content the contract forbids collecting:

| Field | Contains |
|---|---|
| `session.text.delta` / `.ended`.`text` | **assistant output** |
| `session.tool.input.ended`.`text` | **raw tool input** (shell commands, file paths, patch bodies) |
| `session.tool.called`.`input` | **the parsed input object** |
| `session.tool.success`.`content` | **raw tool output** (file contents, command output) |
| `session.compaction.delta` / `.ended`.`text` | **the full compaction summary**, which restates the conversation |
| `session.created`.`title`, `session.renamed`.`title` | **user-authored text** |
| `session.instructions.updated`.`delta` | instruction-content hashes |

§3.8's allowlist is therefore not a stylistic preference — it is the only thing
standing between this stream and a prompt/content logger. Three concrete
requirements follow, and they belong in the contract's implementation notes:

1. **The adapter maps explicitly.** For each event type it must enumerate the
   exact allowlisted fields it copies. No spread, no `data` forwarding.
2. **The envelope's `payload` stays `null`.** The stream makes it tempting;
   §3.2's "reserved; always null in 0.1" must hold.
3. **A regression test must feed a synthetic event containing content-bearing
   fields and assert none of them survive into the envelope.** §8's
   `TestEventPayload_AllowlistDropsUnknownFields` covers the class, but it must
   be run against a *real captured shape*, not a hand-written one — the shapes
   above are the fixtures to use.

---

## 5. Corrected position on M1–M7

**Contract version: `mpm-event/0.1`. Blocking objections: 0 (was 2).**

This supersedes the Session C block in `event-contract.md` §10 and the
first-pass report §1.

| Item | Verdict | Basis |
|---|---|---|
| **M1** — Envelope | **AGREE** | Every required field is obtainable. `observed_at_ms` is the event's own `created`; `host_version` is `ctx.app.version`; `host_seq` remains null (no monotonic counter); `adapter_version` is the plugin build. |
| **M2** — Vocabulary & classification | **AGREE — objection withdrawn** | `session.started` ← `session.created`; `compaction.started`/`compaction.completed` ← `session.compaction.started`/`.ended`, both **live-captured** (§3.3). The closed-vocabulary, no-neighbour-mapping and observed-only rules are implementable and endorsed. |
| **M3** — Identity | **AGREE — objection downgraded to non-blocking** | Every event carries a stable `evt_…` id, so strict-mode derivation on `host_event_id` cannot collide even for nested tool calls. *Recommendation retained:* include `event_type` (and `host_tool_name` where present) in the derivation anyway, because it costs nothing and removes the dependency on which host key happens to be chosen. |
| **M4** — Ordering | **AGREE** | The host stamps `created` in milliseconds on the event itself; `host_seq` is null and §3.6's NULL ordering already covers it. Causality remains explicit (`parentID`, `callID`, `parent_event_id`), never timestamp-inferred. |
| **M5** — Trust | **AGREE** | Unchanged: the adapter can only call `mpm call`; it cannot promote. The event stream does not widen that — it widens what is *collected*, which is why §4 above matters. |
| **M6** — Ingest contract | **AGREE** | Validate/classify/dedup/stamp/store/receipt plus an observable rejection counter are implementable. Transport (§6.2) stays open; measured cost of one `mpm call` spawn is ~39 ms, and the stream's volatility (§3.2) rules out doing it inline. |
| **M7** — Outcomes | **AGREE, one request** | `session.execution.failed` / `session.tool.failed` give real outcomes. **Request unchanged:** §3.7 needs the explicit host-status → `internal.ToolOutcomeClass` mapping pinned by A or B, or the adapter will invent one. |

### 5.1 Why this is a correction and not a change of mind

The earlier position was not a judgement call; it rested on a measurement that
was wrong. Both objections depended on the same false premise — "only the hook
surface is available":

* M2 needed a session-creation signal and a compaction pair. Both exist in the
  event stream and were then captured live.
* M3's collision assumed the adapter must derive ids from `callID`, because
  `callID` was the only host key the hook surface offered. The stream supplies
  a per-event id, so the premise lapses.

The *reasoning* in both objections was sound given the premise. The premise is
what changed, and it changed because it was tested properly.

### 5.2 Non-blocking recommendations (not objections)

| # | Recommendation | Why |
|---|---|---|
| **R1** | Add `event_type` (and `host_tool_name` when present) to the §3.5 strict-mode derivation input, and add `TestEventID_NestedToolCallsDoNotCollide`. | Defence in depth. `evt_…` makes it unnecessary today; it stops being unnecessary the moment any adapter falls back to `callID` for a hook-sourced event, which the nested-call measurement shows is ambiguous. |
| **R2** | Document the first-session startup race (§3.4) in §4 next to `session.started`. | So a future implementer does not "fix" a missing `session.created` by inferring it — which §3.4 forbids. |

---

## 6. Environment verification — the pins do not reach the running host

The brief requires:

```bash
MPM_WORKSPACE=$HOME/.mpm-dogfood
MPM_DB_PATH=$HOME/.mpm-dogfood/src/db/mpm.db
MPM_BINARY=$HOME/src/mpm/.build/bin/mpm
```

and requires that they apply **to the running host/plugin, not merely an
interactive child shell**. Measured: **none of the three is set in any running
host process.**

```
$ for p in 517496 539959 1568327; do tr '\0' '\n' < /proc/$p/environ | grep '^MPM_'; done
(no output)
```

(pids: the desktop app, the `opencode-cli serve --service` process, and the
agent process running this session.)

Consequences, each verified rather than argued:

| Effect | Evidence |
|---|---|
| The plugin resolves the **live** workspace | `resolveWorkspace()` falls back to `$HOME/.mpm` (`src/workspace.ts:21`). The live DB recorded **410 plugin wake-context fetches in two hours**; the dogfood workspace recorded **0**. |
| The plugin resolves the **stale** binary | `resolveMpmBinary()` order is `MPM_BINARY` → `$HOME/.mpm/bin/mpm` → `~/.local/bin/mpm` → PATH. `~/.mpm/bin/mpm` exists and reports **`0.1.0-dev+g3df72315`** — the pre-sprint install, which predates all fifteen corrective PRs. |
| Agent knowledge writes land in the **live** DB | `artifact_provenance` in `~/.mpm`: **12 memories and 2 decisions** created in the last 12 h, all `framework_name=opencode`, `actor_kind=agent`. Several are sprint findings (e.g. the PR #95 blocker, the "recurring MPM defect class" note). |
| The dogfooding workspace is used **only** shell-side | `~/.mpm-dogfood` has 21 memories / 11 decisions / 6 handoffs in the same window, all with `framework_session_id = NULL` — i.e. written by an agent that explicitly exported `MPM_WORKSPACE` for a `mpm call`. |

**Why this is the most operationally important finding in this document.**
The sprint's coordination protocol says MPM is the reasoning channel and
`~/.mpm-dogfood` is the shared workspace. In practice the *plugin* path — the
path an agent takes whenever it calls an MPM tool in the normal way — writes to
the **live** database, while the *shell* path writes to dogfood. So:

* the two channels are **split across two databases**, and a handoff written
  shell-side is invisible to an agent whose plugin points at `~/.mpm`;
* knowledge committed to the shared workspace during this sprint is at risk of
  being read by nobody;
* the live database — which the brief says must not be a test bench — is
  receiving agent-written knowledge;
* every behavioural measurement taken *through the plugin* is measured against
  the **old** binary, not the dev build.

This is also a **correction to the first-pass report §3**, which concluded
"MPM_DB_PATH cannot be made to apply … **No change is required**". That is true
of `MPM_DB_PATH` specifically (`mpm` and `mpm-mcp` ignore it) and false of the
requirement as a whole: `MPM_WORKSPACE` and `MPM_BINARY` *can* apply, and
currently **neither does**.

**No change was made by this session** — redirecting a running host is an
operator action, and the live configuration and installed runtime are outside
my authority. Options for Session A, in cost order:

1. **Operator sets the three variables before launching OpenCode** (desktop
   app and/or service). Cheapest, no code change, and it makes the dogfooding
   workspace the real default. Requires a restart of the user's host, which is
   the operator's call — explicitly out of scope for a probe.
2. **Add a plugin-level, first-class workspace override** (for example a config
   field honoured by `resolveWorkspace()`), so the sprint does not depend on
   how the host was launched. This is an adapter change and therefore my work
   under A's approval.
3. **Accept the split and make it explicit**: require every cross-session write
   to go shell-side with an explicit `MPM_WORKSPACE`. This is what is happening
   today; it is fragile and loses session attribution.

I recommend (1) for the sprint and (2) as the durable fix.

---

## 7. What re-verified clean

Not everything in the first pass was wrong. These were re-tested and hold:

| Claim | Re-verification |
|---|---|
| **The wake context is never injected** | Confirmed against **both** binaries. `mpm call mpm_context --payload '{"action":"read_wake_context","params":{"format":"system-prompt"}}'` returns the envelope `{content, format, success}` at top level — no `payload`, no `context`, no `text`. The plugin's `callMpm` sets `payload = parsed envelope`, then reads `payload?.context ?? payload?.text` → `""`. The proposed fix (`typeof payload?.content === "string"`) is correct. |
| **`projection: "compact"` returns an object, not a string** | Confirmed: `{context: {...}, projection:"compact", success:true}` with `context` a **dict** at envelope top level. A key-only fix would push `[object Object]`; the type check is required. |
| **Nested tool calls share one `callID`** | Confirmed from the first-pass capture (`call_60bea144…` for both `execute` and the nested `probe_tool`). Still true, and still relevant to R1. |
| **Tool-surface byte counts** | Re-ran `measure_tool_surface.py` against the dev `mpm-mcp`: compact **3 tools / 2 244 B / ~561 tok**; full **23 / 43 947 B / ~10 987 tok**; ratio **19.58×** — identical to the reported figures. |
| **Plugin surface is 17 tools and omits discovery/resolve/work** | Confirmed by enumerating the plugin's registered names: 17, and no `mpm_help`, `mpm_resolve`, `mpm_work`, `mpm_route`, `mpm_blob_*`. |
| **`task.boundary` is reachable only shell-side** | Confirmed: `mpm_work` is absent from the plugin surface, so the shell path is the only route, and `ctx.shell.hook` carries no `sessionID`. |

**Correction to the first-pass §6.3 F1 attribution.** The 13 `mpm_memory query`
calls were attributed to FTS5 implicit-AND. That remains a real substrate
property, and it remains Session B's finding. But an additional, adapter-side
cause is now visible: `mpm_resolve` is absent from the plugin surface, and the
wake context carries `mpm://…` pointers the agent cannot resolve in-surface. A
pointer-first agent would have needed one query plus one resolve. Both causes
should be recorded; the adapter half is mine.

---

## 8. Minimal proposed lifecycle integration — revised

The first-pass plan was ordered on the premise that lifecycle events were
expensive to obtain. With the stream available, the ordering changes again, but
**the top priority does not**: the wake-context fix is still the only change
that revives a mechanism that is already shipped and already broken.

| # | Change | Why | Needs the contract? |
|---|---|---|---|
| **A1** | Fix wake-context extraction (`content`, type-checked) + session attribution on the fetch | The central mechanism is dead and is spawning ~3.4 subprocesses/minute to deliver nothing. | **No** |
| **A2** | Subscribe to `ctx.event.subscribe({signal})`, consume off the hot path, map an allowlist, emit `session.observed` + `session.started` (when seen) | Cheapest path to per-session observation for **every** session, MPM-using or not. | Yes (M2 now agreed) |
| **A3** | Add `mpm_help` + `mpm_resolve` + `mpm_work` to the plugin surface | Restores discovery, pointer resolution and the `task.boundary` capability in-surface with a session id. | No — but shared interface, A's PR under `coordination.md` §2 |
| **A4** | Emit `tool.outcome` and `compaction.started`/`.completed` from `session.tool.*` / `session.compaction.*` | Both now directly observable with a host event id and a ms timestamp. | Yes (now unblocked) |
| **A5** | Resolve the workspace/binary pin (§6) | Until it is resolved, every agent-side MPM call reads and writes the live DB with the stale binary. | No — operator/agent decision |
| **A6** | Keep the hook surface for `shell.create.before` provenance only | It is the one hook MPM actually needs, and it has no session id. | No |

**A1 and A3 remain the release-critical changes**, and A5 is now arguably
joint-first with them because it determines whether the sprint's own knowledge
lands where the sprint says it lands.

**What the adapter still must not do:** forward `event.data`; infer session
start/resume/idle/completion from gaps; map unknown events onto neighbours;
promote anything; fabricate a project id (§4.3's proposal — adopt the host id
only when a git remote is independently confirmed — still stands, and
`session.created` now supplies `projectID` too, so the two sources agree).

---

## 9. What Session B can now be told (supersedes report §9 / §12.3)

| Question | Corrected answer |
|---|---|
| Is the host event stream reachable? | **Yes.** `ctx.event.subscribe({signal})` → `AsyncIterable`. 31 types captured. |
| Is there a per-session signal for a session that never called an MPM tool? | **Yes** — `session.created` (when the adapter is already consuming), `session.execution.started`, `session.step.started`. Not conditional on MPM usage. |
| Is there a stable per-tool-call key? | **Yes, two**: the host event `id` (`evt_…`, unique per event) and the tool call `id` (`call_…`, shared by nested calls). Prefer the former. |
| Is there a wall-clock timestamp? | **Yes**, on the event itself: `created`, unix ms. |
| Are session outcomes observable? | **Yes** — `session.execution.succeeded/failed/interrupted`, `session.error`, `session.idle` (the last four named in the host, not yet exercised). |
| Does B need a lifecycle store? | Unchanged from B's audit — that is B's call. The stream makes *capture* cheap; it does not make a new table necessary. |
| What must B tolerate? | **Duplicate and missing observations.** The stream is volatile by contract; an overflow drops events. Best-effort delivery, strict per-event ids. |

**The one thing B must not do:** treat a missing event as a fact. The stream
has no replay, and the startup race (§3.4) means the first session's
`session.created` may simply never arrive.

---

## 10. Contract changes requiring Session A's decision — revised

| # | Item | Change requested | Status |
|---|---|---|---|
| **C1** | M2 (§4) | **Withdrawn.** No vocabulary change is needed: `session.created` and the compaction pair are directly observable. Keep `session.observed` as the unconditional base. | resolved by measurement |
| **C2** | M3 (§3.5) | **Downgraded to R1** (non-blocking): add `event_type` to the derivation and add the nested-collision test as hardening, not as a blocker. | recommendation |
| **C3** | M3 (§3.3) | Adopt the host's project id when a git remote is independently confirmed; `project_id_source: "host-git-remote"`. Now doubly attested: `ctx.location.project.id` **and** `session.created.data.projectID`. | high |
| **C4** | M7 (§3.7) | Pin the host-status → `ToolOutcomeClass` mapping. | high |
| **C5** | §6.2 (transport) | Measured ~39 ms per `mpm call`; the stream is volatile, so **no subprocess call may run inside the event loop**. Batching or a socket is indicated. | high |
| **C6** | §5 (capability table) | **Replace wholesale** with §3 of this document. Specifically: the event stream **exists and works**; `chat.message` and `command.execute.before` genuinely do not exist; `experimental.session.compacting` / `…autocontinue` do not exist, but `session.compaction.*` events do; `experimental.chat.system.transform` exists as `ctx.session.hook("context")`. | high |
| **C7** | §6.7 (`meta` allowlist) | Add `host_event_type`, `host_call_id`, `host_message_id`; keep everything content-bearing **out**. §4 of this document lists exactly which captured fields must never cross. | medium |
| **C8** | §6.8 (byte ceilings) | No change needed. The richest allowlisted envelope is tiny; the rich fields are the ones that must not be collected. | none |
| **C9** | §3.8 (new note) | Record the stream's **volatility** and the "no subprocess inside the iterator" rule as implementation constraints. | medium |

---

## 11. Revised first implementation PR

**Unchanged from the first-pass recommendation, and now with a corrected
diagnosis on the record.**

**PR 1 — `fix(opencode): deliver the wake context (read `content`, attribute the call)`**

1. `src/index.v2.ts` — `fetchWakeContext`: read the string `content` from the
   envelope (which is what `callMpm` calls `payload`), with a type check; keep
   `context`/`text` only as typed fallbacks.
2. Use the provenance-carrying call helper so the fetch is session-attributed.
3. Tests T1–T7 from the first-pass report §11.1, unchanged.

**PR 2 — `feat(opencode): observe lifecycle events`** (now unblocked)

4. Subscribe with `ctx.event.subscribe({ signal })`, consume in a detached
   async loop, queue envelopes, never `await` a subprocess inside the iterator.
5. Map an explicit allowlist per event type. **No `data` forwarding.**
6. Emit `session.observed`, `session.started` (when a `session.created` is
   actually seen), `tool.outcome`, `compaction.started`, `compaction.completed`.

**Acceptance tests:**

| # | Test | Asserts |
|---|---|---|
| T1–T7 | wake-context extractor + cache + attribution | as the first-pass report (§11.1) |
| T8 | `test_event_subscribe_is_consumed_as_async_iterable` | The adapter treats the return value as `AsyncIterable`, not a callback registration. **This is the test that would have caught the first-pass error.** |
| T9 | `test_event_allowlist_drops_content_fields` | A captured `session.tool.success` shape yields an envelope with **no** `content`; same for `session.tool.input.ended.text`, `session.compaction.ended.text`, `session.text.delta.delta`. Fixtures taken from the captures in §3.3. |
| T10 | `test_session_started_not_inferred_when_created_is_missed` | A first event without a preceding `session.created` produces `session.observed` and **no** `session.started`. |
| T11 | `test_compaction_pair_from_events` | `session.compaction.started` → `compaction.started`; `.ended` → `compaction.completed`; `.failed` → completed-with-error, never silently dropped. |
| T12 | `test_strict_event_id_uses_host_event_id` | Same `evt_…` → same `event_id`; nested tool calls under one `callID` do not collide. |
| T13 | `test_no_subprocess_call_inside_event_iterator` | A structural test: the loop body enqueues and returns. |
| T14 | `test_plugin_surface_includes_discovery_and_resolve` | `mpm_help`, `mpm_resolve`, `mpm_work` present (PR 3). |

---

## 12. Reproducibility record

| Item | Value |
|---|---|
| Repository revision | `a52b396` on `dev/session-c-opencode`; `origin/main` = `70deac2b` |
| Host | `opencode v2.1.4`, `~/.opencode/bin/opencode` (240 416 944 B) |
| Probe entrypoint shape | `export default { id, setup }` (a function default is rejected by the loader) |
| Isolation | `XDG_{CONFIG,DATA,STATE,CACHE}_HOME` → `/tmp/opencode/probe-d/*`; verified with `opencode debug paths` |
| Runs | `run1` 22 events / 22 types; `run2` 58 events / 31 types; `run3` compaction pair |
| Evidence | `agent_installation/mpm-opencode/measure/evidence/event_stream_probe_run{1,2,3}*.jsonl`, `event_vocabulary_session_static.txt` |
| Probe source | `agent_installation/mpm-opencode/measure/probe/event_stream_probe.js` |
| Tool surface | re-measured with `measure_tool_surface.py` → identical to the first pass |
| Live DB access | read-only (`mode=ro`) for `tool_invocations`, `memories`, `artifact_provenance` |
| Dogfood DB access | read-only for record verification |
| Nodes / servers left running | none (isolated service stopped and verified) |

**Isolation statement.** No write reached `~/.mpm`, the live OpenCode database,
`~/.local/share/opencode`, the live configuration, the installed plugin
payload, `~/.mpm-dogfood` (by this document's probes), or another session's
worktree. The probe directory is disposable scratch under `/tmp/opencode`.

---

## 13. What this session did not do

* Did not implement, merge or deploy anything.
* Did not change the tool registry, `package.json`, the lockfile or any shared
  interface.
* Did not modify the installed plugin payload, the live OpenCode configuration,
  or the user's running sessions.
* Did not redirect the running host's workspace (§6) — that is an operator
  action, and I did not take it.
* Did not re-run the behavioural fixtures against a fixed adapter: the fix does
  not exist yet, so there is nothing fixed to measure. That is PR 1's
  follow-up.
