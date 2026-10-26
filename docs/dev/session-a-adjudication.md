# Session A — adjudication of the Session B and Session C investigations

> **Author:** Session A — integration and release authority.
> **Date:** 2026-10-26. **Base:** `origin/main` = `70deac2`.
> **Scope:** adjudicate two Session C investigations and one Session B audit,
> record the contract state, resolve the environment split, authorise the
> wake-context repair, and define the smallest coordinated first PRs.
>
> **Status:** this document records decisions **A** has taken. It does not
> merge, deploy, or modify the installed runtime, the live database, the
> live scheduler, or the user's running desktop processes.

---

## 0. What A verified independently, before deciding

Nothing below is taken on trust. Every load-bearing claim was re-checked
against the code, the host binary, or a live run.

| Claim | Source | A's verification | Verdict |
|---|---|---|---|
| The host event stream is unreachable from a plugin | C pass 1 | **Wrong.** The host binary contains `event:{subscribe:(o)=>z(e.event.subscribe().pipe(Ga((r)=>T(r)),Ir((r)=>r)),o)}` at offset `163945713`, and `z` is `Plugin.Event.makeStreams`, whose only public member is `[Symbol.asyncIterator]`. The first-pass probe passed a *callback* and never iterated. | **Withdrawn by C; confirmed** |
| `ctx.event.subscribe({signal})` returns an `AsyncIterable` | C pass 2 | The corrected probe's own capture records `formA.result → symbols:["Symbol(Symbol.asyncIterator)"], isAsyncIterable:true`, `keys:[]` | **Confirmed** |
| 31 distinct event types, incl. `session.created` and the compaction pair | C pass 2 | A's own count of the committed captures: **32 distinct types over 119 records**; per run **22 / 31 / 26** — exactly C's table. `session.created` and `session.compaction.started`/`.ended` all present | **Confirmed** |
| Every event carries a stable `evt_…` id and a ms `created` | C pass 2 | All **119/119** ids are `evt_`-prefixed and **unique**; all `created` values are integers in ms | **Confirmed** |
| The stream is a redaction hazard | C pass 2 | The captures' `dataKeys` confirm every content-bearing field C names: `session.tool.success.content`, `session.tool.input.ended.text`, `session.tool.called.input`, `session.compaction.delta/ended.text`, `session.text.delta.delta`, `session.created/renamed.title`, `session.instructions.updated.delta` | **Confirmed** |
| The host's stream is volatile by contract | C pass 2 | The binary's own API text: *"Volatile by contract: a slow consumer overflows and fails the stream, and events during disconnection are missed."* | **Confirmed** |
| `epistemic_provenance` already provides typed + validated + idempotent citations | B audit | `internal/core/schema.go:442` — `source_type TEXT NOT NULL`, `event_id TEXT NOT NULL`, `UNIQUE (source_id, downstream_id, event_id)` | **Confirmed — no schema change needed** |
| A cited `tool_invocations` id lands as `source_type='memory'` | B audit | `internal/core/epistemology_tools.go:693` passes `""` for `sourceType`; `internal/core/cascade_provenance.go:169-181` resolves it against `memories` and **falls back to `"memory"`** | **Confirmed** |
| `mpm_provenance` (called by the telemetry hunt) does not exist | B audit | No such tool is registered; only `internal/telemetry/observe.go:10` mentions it, in a comment | **Confirmed** |
| The registry advertises `artifact_type='skill'`, which the CHECK rejects | B audit (A-9) | `internal/core/tools/registry_list.go:249` lists `"skill"`; `internal/core/schema.go:270` CHECK is `IN ('memory','theory','decision','lesson','work')` | **Confirmed — real defect** |
| `framework_session_id` is path-dependent | B audit | Live DB: `opencode` **101/101 set**; `mpm-cli` **0/5830**; `mcp` **0/26** | **Confirmed** |
| The scheduler-status line is process-based | **A's own runbook** | `cmd/mpm/handlers_info.go:418` runs `systemctl --user is-active mpm-scheduler` and `systemctl --user show … MainPID`. It is **systemd-unit-based** | **B is right; A's runbook is corrected below** |
| The wake context is never injected | C (both passes) | `read_wake_context` returns `{content, format, success}` at top level; `projection:compact` returns `{context: <dict>, projection, success}`. The adapter read `payload?.context ?? payload?.text` | **Confirmed** |
| The environment pins do not reach the running host | C pass 2 | A read every running OpenCode process's `/proc/<pid>/environ`: the desktop app, its renderer and `opencode-cli serve --service` have **no** `MPM_*` variable | **Confirmed** |

---

## 1. The contract state

### 1.1 The two blocking objections

**M2 (vocabulary) — WITHDRAWN, accepted.** `session.started` is directly
observable as `session.created`; `compaction.started`/`compaction.completed`
as `session.compaction.started`/`.ended`, both live-captured. The premise
("only the hook surface is available") was wrong, and both objections rested
on it.

**M3 (identity) — WITHDRAWN as a blocker, but its substance is ADOPTED.**
Every event carries a stable `evt_…` id, so strict-mode derivation on the
host event id cannot collide. **However, the objection identified a real
defect in the contract text**, and A amends it:

> §3.5's host-event-key chain listed the host's `callID` as a candidate
> unique key. **It is not one.** The measurements show **nested tool calls
> share a single `callID`** (`call_60bea144…` for both an outer `execute`
> and the nested `probe_tool`). A `callID` is an **operation correlation id**,
> not an event identity. The chain must therefore be amended (below).

This is exactly the reconciliation the brief asked for: the `evt_…`
identifier is the **event identity**; the `callID` is a **correlation
field** and must never be used as a guaranteed-unique event id.

### 1.2 Contract amendments A makes

| # | Amendment | Reason |
|---|---|---|
| **A1** | **§3.5** — remove the host's `callID` from the unique host-event-key chain. It stays a **correlation** field (`meta.host_call_id`), and is explicitly *not* an identity. | Nested tool calls share one `callID`; using it as an identity would silently deduplicate a real event away. |
| **A2** | **§3.5** — add `event_type` (and `host_tool_name` where present) to the strict-mode derivation input. | C's **R1**. Defence in depth: the guarantee then does not depend on which host key an adapter happens to pick. Costs nothing. |
| **A3** | **§3.5 / §7 M3** — record the **measured** host key: `evt_<hex>`, unique per published event, 119/119 in the captures. | Replaces the speculative "first non-empty of …" list with a measured fact. |
| **A4** | **§4** — beside `session.started`, record the **first-session startup race**: the host creates the plugin's own session *before* the adapter's iterator starts, so that one `session.created` may never arrive. Its absence must not be read as "no session started", and it must **not** be inferred. | C's **R2**. |
| **A5** | **§3.8** — record the stream's **volatility** and the rule that **no subprocess call may run inside the event iterator** (the host's own contract: a slow consumer overflows and the stream fails). | C's **C9**. |
| **A6** | **§5** — replace the capability table with the **measured** surface: the event stream exists and works; `chat.message` and `command.execute.before` genuinely do not; `experimental.session.compacting`/`…autocontinue` do not, but `session.compaction.*` **events** do. | C's **C6**. |
| **A7** | **§6.7** — the `meta` allowlist gains `host_event_type`, `host_call_id`, `host_message_id`. Everything content-bearing stays out. | C's **C7**. |
| **A8** | **§6.2** — the transport constraint: one `mpm call` spawn measured at **~39 ms**; the stream is volatile, so envelopes must be queued and handled out of band. | C's **C5**. |
| **A9** | **§6.4** — **decided**: the ingest entry point, receipt and rejection counter live on **`mpm_system`**, not `mpm_context`. The receipt's new-or-duplicate determination must come from a **read-back** of the stored row, not from the `INSERT` result. | B's **A-3**, which A accepts. `mpm_context` is the session-start surface and already carries `contextual_focus`; ingestion counters would add tokens to every wake for information no agent needs at session start. |
| **A10** | **§6.1** — **decided: deferred, and explicitly NOT the first milestone.** No new table is authorised. See §4. | B's **A-2**. The audits show the first end-to-end acceptance test needs no lifecycle store. |
| **A11** | **§3.7 / M5** — the M5 tension is **ruled**: see §2. | B's **A-1**. |
| **A12** | **§7 M7** — the host-status → `internal.ToolOutcomeClass` mapping is **pinned by A**: see §2. | C's **C4**. |
| **A13** | **§3.3 / M3** — project identity: adopt the host's project id **only when a git remote is independently confirmed**; `project_id_source` becomes `host-git-remote`. A host id alone is not a project identity. | C's **C3**, accepted with the "independently confirmed" qualifier, which C itself proposed. |

### 1.3 The agreement

| Item | Session B | Session C | A's ruling |
|---|---|---|---|
| M1 envelope | AGREE (2 notes) | AGREE | **agreed**; B's note (a) — the always-null `payload` field — is **sustained**: `payload` stays reserved and explicitly outside the frozen set's byte accounting. B's note (b) — a host timestamp is a **new** `meta` field in a 0.2 bump, never a substitute for `observed_at_ms` — is **sustained**, and is now concrete (`host_created_ms`, A7). |
| M2 vocabulary | AGREE | AGREE (objection withdrawn) | **agreed** |
| M3 identity | AGREE (1 clarification) | AGREE (objection downgraded) | **agreed, with A1–A3, A13** |
| M4 ordering | AGREE | AGREE | **agreed** |
| M5 trust | AGREE (1 tension referred) | AGREE | **agreed, with A11** |
| M6 ingest | AGREE (1 requirement) | AGREE | **agreed, with A9** (B's read-back requirement is adopted verbatim) |
| M7 outcomes | AGREE (1 clarification) | AGREE (1 request) | **agreed, with A12** and B's clarification: `result_status`/`outcome_class` are **MPM-scoped, not host-scoped**; the host's status belongs in the opaque `meta.host_result` and `result_status` reads `unknown` when `operation` is absent. |

**Both sessions accept `mpm-event/0.1` with zero blocking objections.**
The evidence is in the shared dogfooding workspace: B's decision
`3a987eca2c708fac`, and C's decision `281d5267b2e9d4d4` (which supersedes
its withdrawn first-pass acknowledgement `8cf1f0e3cc41465e`).

> **The contract is recorded as AGREED on `mpm-event/0.1`, with amendments
> A1–A13.**
>
> **Agreement is not implementation.** Nothing in the substrate yet receives,
> stores or counts a lifecycle event. §4 below defines the smallest
> implementation that the audits show is actually needed.

---

## 2. Rulings on the items referred to A

| # | Item | A's ruling |
|---|---|---|
| **A-1** | **The M5 tension**: does the critic's existing automatic lesson-writing count as "automatic promotion" M5 forbids? | **Ruled: B's second option — record the behaviour as known and out of scope, and state explicitly that the event contract does not endorse it.** Amending M5 to require a *pending* surface would need a status column on `lessons_base` — a schema change this sprint should not take for a pre-existing behaviour. The event contract governs **events**; it neither creates nor blesses the critic's auto-lesson path. B's PR-1 (typed, validated evidence citations) **strengthens** that path rather than changing its shape. |
| **A-2** | **§6.1 — the storage path for lifecycle observations** | **Deferred. No new table is authorised now.** See §4: B's audit shows the first end-to-end acceptance test does not need one. Revisit only against a **measured** gap, and then as one PR with its contract tests. |
| **A-3** | **§6.4 — the ingest entry point, receipt and counter surface** | **`mpm_system`**, per B. A new action `mpm_system action=event_counters` beside `critic_findings` and `query_audit_log`. The receipt is backed by a **read-back**, not the `INSERT` result (Substrate Defense Triad rule 3). |
| **A-4** | **Registry change** — typed proposal citation | **Approved** as one PR with its core change, per [`coordination.md`](coordination.md) §4.3. **Session B owns it.** |
| **A-5** | **Registry change** — the read surface for the new proposal type | **Approved**, same PR discipline. **Session B owns it.** |
| **A-6** | **Environment wiring** | **Decided — see §3.** The mechanism is a **separately launched, pinned, isolated development host**. |
| **A-7** | **Runbook correction — the scheduler-status mechanism** | **Accepted. B is right and A's runbook was wrong.** The check is **systemd-unit-based** (`systemctl --user is-active mpm-scheduler` + `MainPID`), not process-based. A's **conclusion** stands and is better supported: the unit is pinned to `MPM_WORKSPACE=/home/v/.mpm`, so the line is evidence about the **live** workspace only. The runbook is corrected. |
| **A-8** | **Two pre-existing defects** — the telemetry hunt calls the non-existent `mpm_provenance` and opens the wrong telemetry DB | **Triaged, not fixed.** Agreed with B: **do not** resurrect `mpm_provenance`. If the hunt is ever wanted it must be rewritten on `tool_invocations`. Out of this sprint's scope. |
| **A-9** | **Registry advertises `artifact_type='skill'`, which the CHECK rejects** | **Confirmed and approved.** One-line registry fix. **Session B may include it in its PR-1** (same PR, same registry surface). It is a live defect: a caller following the registry gets a CHECK violation. |
| **C-3** | Host project id | **Accepted with the qualifier**: adopt the host's id only when a git remote is independently confirms it; `project_id_source: "host-git-remote"`. |
| **C-4** | **Host-status → `ToolOutcomeClass` mapping** | **Pinned by A** (see below). |
| **C-5** | Transport | **Recorded as a constraint, not decided** (§6.2 stays open). |
| **C-6** | Capability table | **Replaced with the measured surface** (A6). |
| **C-7** | `meta` allowlist additions | **Accepted** (A7). |
| **C-8** | Byte ceilings | **No change needed** — agreed. The richest *allowlisted* envelope is tiny; the rich fields are the ones that must not be collected. |
| **C-9** | Volatility + no-subprocess-in-iterator | **Accepted** (A5). |

### 2.1 A-1's companion — the pinned host-status → `ToolOutcomeClass` mapping (A-12)

B and C both asked A to pin this so the adapter does not invent a vocabulary.
A pins it as follows, in `meta` (opaque, host-scoped) and in the MPM-scoped
fields (never a new vocabulary):

| Host signal | `meta.host_result` (opaque) | `operation` | `result_status` | `outcome_class` |
|---|---|---|---|---|
| `session.execution.succeeded` | `"succeeded"` | `absent` | `unknown` | *absent* |
| `session.execution.failed` | `"failed"` | `absent` | `unknown` | *absent* |
| `session.execution.interrupted` | `"interrupted"` | `absent` | `unknown` | *absent* |
| `session.tool.success` | `"success"` | `present` | `success` | `ok` |
| `session.tool.failed` | `"failed"` | `present` | `error` | `internal` **only** when the host gives no finer signal; otherwise the adapter maps the *host's* finer code through the existing enum and must document the mapping |
| `session.compaction.started`/`.ended` | `"started"`/`"ended"` | `absent` | `unknown` | *absent* |
| `session.compaction.failed` | `"failed"` | `absent` | `unknown` | *absent* |

The rule that makes this safe: **the host's vocabulary is opaque and lives
in `meta`; the MPM vocabulary is only used when an MPM operation actually
produced the observation.** No host status is ever coerced into
`ToolOutcomeClass` without an operation behind it, and no new enum value is
introduced.

---

## 3. The environment split — resolved

### 3.1 What is broken, measured

| Fact | Evidence |
|---|---|
| No `MPM_*` variable reaches any running OpenCode process | The desktop app (`/opt/OpenCode/ai.opencode`), its renderer and `opencode-cli serve --service` were each read from `/proc/<pid>/environ`: **no `MPM_WORKSPACE`, no `MPM_DB_PATH`, no `MPM_BINARY`.** |
| The plugin therefore resolves the **live** workspace and the **stale** binary | `resolveWorkspace()` falls back to `$HOME/.mpm`; `resolveMpmBinary()` falls back to `$HOME/.mpm/bin/mpm` → `0.1.0-dev+g3df72315`, which predates all fifteen corrective PRs. |
| Plugin traffic lands in the **live** database | Live `~/.mpm`: **329** `read_wake_context` rows in the last hour (≈5.5/min), and **12** agent-written `artifact_provenance` rows in the last 12 h (`framework_name=opencode`, `actor_kind=agent`), including sprint findings. |
| Shell-side calls land in the **dogfood** database | `~/.mpm-dogfood`: 2 `read_wake_context` rows in 12 h, 22 memories / 12 decisions — all written by agents that exported `MPM_WORKSPACE` explicitly. |
| The two channels are **unattributed** on both sides | `framework_session_id` is NULL on **5748 of 5754** `read_wake_context` rows, and on **100/100** dogfood rows. Only the plugin's *tool* path is attributed (101/101 `opencode` rows). |

### 3.2 The mechanism A investigated, and what is NOT available

A tested the candidate mechanisms against an isolated host rather than
assuming them:

| Mechanism | Result |
|---|---|
| **Plugin options** (`"plugins": [["file://…", {opts}]]`) | **NOT AVAILABLE in v2.1.4.** The host's config normaliser rejects the tuple form: `configuration normalization diagnostic … path=$.plugins.0 kind=invalid action="skipped malformed recognized value"`. A probe confirmed `effect(input, options)` is called with `options = null`. **Session C's proposed "plugin-level configuration override" is not implementable as a host feature in this version.** |
| **A separately launched, pinned host** | **WORKS.** A probe plugin, started by an isolated `opencode run --standalone` with the pins exported, observed `MPM_WORKSPACE`, `MPM_DB_PATH` and `MPM_BINARY` **in its own process environment**, along with the isolated `XDG_*` roots. The **real** MPM plugin, run the same way, wrote its `health_check` and `read_wake_context` rows into the **scratch** database and **nothing** into `~/.mpm` or `~/.mpm-dogfood`. |

### 3.3 A's decision

> **The mechanism is a separately launched development host**: a dedicated
> launch wrapper that
>
> 1. **requires** `MPM_WORKSPACE`, `MPM_DB_PATH` and `MPM_BINARY` to be set
>    and **refuses to start** if any is missing or resolves to `~/.mpm` —
>    so it **cannot silently fall back to the live workspace**;
> 2. redirects `XDG_{CONFIG,DATA,STATE,CACHE}_HOME` and
>    `OPENCODE_CONFIG_DIR` to a disposable tree, so the host's own state,
>    session store and database are never touched;
> 3. registers the **repository's** plugin (`file://…/agent_installation/mpm-opencode`),
>    not the installed payload;
> 4. uses `opencode run --standalone` (or `opencode serve` on a free port
>    with `OPENCODE_PASSWORD` read from its own log), so no shared managed
>    service is contacted;
> 5. points the plugin's `resolveMpmBinary` at the **development** build.
>
> **The desktop application is not touched.** It is a separate, user-facing
> process; changing how it is launched requires the operator's approval
> (see §7).

The workspace resolution is verified **from inside the plugin process** —
the plugin writes its own resolved `MPM_WORKSPACE` / `MPM_BINARY` to a
probe log — never inferred from the launching shell's variables. A's
verification harness is at `measure/probe/` and its verdict is measured from
the host's own system-prompt array.

### 3.4 What A is NOT doing, and why

* **Not** modifying the installed plugin payload, the live database, the
  live scheduler, the live systemd unit, or `~/.mpm/.mcp.json`.
* **Not** restarting, reloading or signalling any running desktop process.
* **Not** changing user-wide configuration (`~/.config/opencode`) without
  the operator's approval.

**The desktop app will keep writing to the live database until the operator
restarts it with the pins set, or adopts a plugin-level default.** A records
this as an open operational item (§7), not as a solved problem.

---

## 4. Implementation coordination — the smallest coordinated first PRs

### 4.1 The governing finding

> **Session B's audit shows the lifecycle-event store is NOT on the critical
> path.** The first end-to-end acceptance test — *evidence-backed
> retrospective detection of missed memory opportunities* — needs only what
> already exists: `tool_invocations` × `artifact_provenance` (a join B
> measured working on live data) and a **typed, validated, idempotent
> citation** on the proposal. `epistemic_provenance` already has all three
> properties, so **no schema change is needed**.

**Therefore: the lifecycle-events table is NOT the first implementation
milestone.** It is authorised only against a measured gap, and then as one PR
with its contract tests (A2).

### 4.2 The coordinated first PRs

| # | PR | Owner | Needs the contract? | Schema? | Registry? |
|---|---|---|---|---|---|
| **P1** | **`fix(opencode): deliver the wake context`** — extraction (`content`, type-checked, never coerced), per-session cache, concurrent-build suppression, retry-on-empty, session attribution on the fetch | **C** | No | No | No |
| **P2** | **`feat(critic): typed, validated, idempotent evidence citations on proposals`** — optional `source_type` beside `source_ids` on the propose path, validated before insert, passed through to `recordProvenanceNode` | **B** | No | **No** | **Yes** (one optional parameter) |
| **P3** | **`feat(critic): a retrospective missed-opportunity hunt`** over the existing `tool_invocations` × `artifact_provenance` join | **B** | No | No | No |
| **P4** | **`feat(opencode): observe lifecycle events`** — subscribe to `ctx.event.subscribe({signal})`, consume off the hot path, map an explicit allowlist, emit `session.observed` / `session.started` (when actually seen) / `tool.outcome` / `compaction.*` | **C** | **Yes** (M2, now agreed) | No | No |
| **P5** | The ingest entry point + receipt + `mpm_system action=event_counters` + the storage decision (A2/A9) | **A** | Yes | **only if A2 later authorises it** | Yes (one action) |

**P1 and P2 are independent and can proceed in parallel.** P3 depends on P2.
P4 depends on the contract (now agreed) and on P1's plumbing. P5 is A's
and is explicitly last.

### 4.3 What stays unchanged

* **The existing specialised MPM tool surface is unchanged** pending
  evidence-based review. No tool is added, renamed or removed by this
  adjudication.
* **The schema is unchanged** in P1–P4. A keeps the migration authority.
* **No `SafeMigrations` entry** is authorised by this adjudication.

---

## 5. The wake-context repair — authorised, and verified

### 5.1 The defect, precisely

`read_wake_context` with `format: "system-prompt"` returns

```json
{"content": "<prose>", "format": "system-prompt", "success": true}
```

at the **top level** of the `mpm call` envelope. The adapter read
`payload?.context ?? payload?.text` — **neither key exists**, so the
extractor returned `""` on every call. Two consequences followed:

1. `if (context)` was false, so **`event.system.push(...)` never ran** and
   **`wakeContextCache.set(...)` never ran**. The per-session cache was
   never populated, so **every** context-hook invocation re-spawned `mpm`.
2. `projection: "compact"` returns `context` as a **dict**. A key-only fix
   would still yield `""`; a fix that took `payload.context` without a type
   check would push `"[object Object]"` into the system prompt.

### 5.2 The authorisation

> **Session C owns P1.** The approved change is exactly:
>
> * `src/wake-context.ts` (new) owns extraction and injection policy:
>   `extractWakeContextText()` returns the first **non-empty string** among
>   `content`, `context`, `text` — **never a coercion** — and
>   `createWakeContextInjector()` carries the per-session cache plus
>   concurrent-build suppression and retry-on-empty.
> * `src/index.v2.ts` delegates its `context` hook to the injector (three
>   lines) and its fetch to the extractor.
> * **No** tool, registry, transport, `package.json`, lockfile or
>   shared-interface change. **No** schema change.
>
> Required regression coverage: the real `content` shape; the legacy
> `context`/`text` shapes and their precedence; empty and malformed
> envelopes (null, array, number, the compact dict) with **no** coercion;
> per-session caching; **failed-retrieval retry** without poisoning the
> session; duplicate suppression, sequential **and concurrent**; and a
> non-empty part actually reaching `event.system`.

### 5.3 A's independent verification

| Check | Result |
|---|---|
| TypeScript build (`tsc`, `tsc -p tsconfig.tests.json`) | ✅ clean |
| TS suite | ✅ **45 passing, 0 failing** (28 new; floor 15) |
| **Negative control** — the same harness pointed at a copy of the plugin with the pre-fix extractor | ✅ **VERDICT: FAIL** — the envelope still carried the marker, but the marker did **not** reach the hook. The harness detects the defect rather than merely passing. |
| **Genuine end-to-end injection** — an isolated OpenCode v2.1.4 host, this repository's plugin, a witness probe, a disposable workspace holding a unique marker memory | ✅ **VERDICT: PASS** — the marker appeared in the **actual system-prompt array the agent received**, at both the hook and at teardown. A's own independent run reproduced this. |

**The acceptance criterion the brief set — "the agent must actually receive
the expected context" — is met, and the harness is shown to be
non-vacuous.** A successful tool invocation alone would not have been
enough, and was not relied on.

### 5.4 The polling issue — measured separately, not bundled

The brief requires this to be a **separate, measurable** issue. It is
measured, and the measurement is decisive:

| | Before the fix | After the fix |
|---|---|---|
| Plugin fetches for **3 prompts in 1 session** (one plugin process) | **3** | **1** |
| Live rate, `~/.mpm` | **329 rows/hour** (≈5.5/min), all un-attributed | — |

**The amplification was caused by the extraction bug**: the cache guard sat
*downstream* of the truthiness check, so a broken extraction disabled the
cache entirely. Fixing extraction fixes the amplification.

**Two residual issues remain and are NOT part of P1**, because the brief
requires them to be kept separate and because they are different concerns:

1. **No negative-result handling.** A fetch that fails (DB locked, 5 s
   timeout) is not cached, so *every* subsequent hook invocation retries —
   an unbounded retry with no backoff and no rate limit. P1 improves this
   (retry is allowed, concurrent duplicates are suppressed) but does not
   bound the retry rate.
2. **The fetch is still unattributed.** `fetchWakeContext` uses `callMpm`,
   not `callMpmWithProvenance`, so the plugin's wake-fetch rows keep
   `framework_session_id = NULL` — **5748 of 5754** live rows. A's
   measurement: the plugin's *tool* calls are attributed **101/101**
   (`framework_name=opencode`), so the fix is to route the fetch through the
   existing provenance helper. That helper already exists
   (`callMpmWithProvenance` in `src/index.v2.ts`); **no new infrastructure
   is needed.**

**A authorises item 2 as part of P1** — it is a one-line change to an
existing helper and it is what makes the wake traffic attributable. Item 1
is **deferred** and recorded, not bundled.

---

## 6. Data safety — findings on Session C's committed captures

A scanned every committed evidence file for credentials, keys, tokens,
passwords, absolute paths outside disposable scratch, and user content.

| File | Finding |
|---|---|
| `event_stream_probe_run{1,2,3}*.jsonl` | ✅ **Publishable.** Record **event shapes** (type, `id`, `created`, top-level key names, `dataKeys`) — **not values**. No prompt text, no tool output, no file contents, no credentials. |
| `event_vocabulary_session_static.txt` | ✅ Publishable. A static token extract. |
| `project_identity_*.jsonl` | ✅ Publishable. Disposable scratch paths and synthetic remotes. |
| `tool_surface_measurement*.json` | ✅ Publishable. Tool descriptions and byte counts; the only absolute paths are the operator's own workspace and dev-build paths. |
| `host_surface_probe_run.jsonl` | ⚠ **Publishable, but only by luck of that run.** It records **whole hook payloads**, which include the host's `system` array and the user-message array. In this run those contained only OpenCode's own harness boilerplate (945 chars) and the probe's `"[depth]"` markers — **no** user AGENTS.md content, **no** credentials, **no** real prompts. But the *method* would capture real content if run in a real session. |

**A's ruling on the method:**

> The `host_surface_probe.js` probe must be changed to record **shapes, not
> payloads** — exactly as `event_stream_probe.js` already does — **before it
> is run again**. This is a requirement on the probe, not a rejection of the
> committed capture.

### 6.1 Structural metadata allowlisting — required before any storage

A's ruling, reinforced by Session C's own V5 finding:

> **No lifecycle event may reach MPM storage until the adapter maps it
> through a structural allowlist.** Specifically:
>
> 1. The adapter **maps explicitly** — for each event type it enumerates
>    the exact allowlisted fields it copies. **No spread, no `data`
>    forwarding.**
> 2. The envelope's `payload` **stays `null`** in `mpm-event/0.1`.
> 3. The allowlist gains `host_event_type`, `host_call_id`,
>    `host_message_id` (A7). Everything content-bearing stays out:
>    `session.text.*.text/delta`, `session.tool.input.ended.text`,
>    `session.tool.called.input`, `session.tool.success.content`,
>    `session.compaction.delta/ended.text`, `session.created/renamed.title`,
>    `session.instructions.updated.delta`.
> 4. A regression test must feed a **real captured shape** — the fixtures
>    in the committed captures — and assert none of those fields survive
>    into the envelope. A hand-written shape is not sufficient evidence.

**The adapter must never forward raw tool inputs, outputs, prompts or
compaction summaries as a shortcut.** That is now a hard requirement on
P4, and P4 does not start until the allowlist test exists.

---

## 7. Open items requiring the operator

| # | Item | Why it is the operator's |
|---|---|---|
| 1 | **Whether the desktop OpenCode app is restarted with the three pins set.** Until it is, the plugin path keeps writing to the **live** database and the **stale** binary. | It changes a running user-facing process. A will not restart or reconfigure it without approval. |
| 2 | **Whether `~/.config/opencode/opencode.jsonc` gains a development-specific plugin registration** (the repository's plugin, not the installed payload). | User-wide configuration. |
| 3 | **Whether a dedicated dogfooding scheduler unit is created.** The mechanism exists (`--db`, `--lock`, its own `MPM_WORKSPACE`); a unit is a deployment act. | Deployment authority is A's, but this is a new service. |
| 4 | **The four housekeeping MPM memory records** — keep, refine, or shred. | Still outstanding from the previous round. |
| 5 | **Whether PR #108** (the one-line `internal/core/go.mod` uuid-direct correction) is merged. It is green and ready; A did not merge it because the authorisation for that round covered #104 and #105 only. | A will merge on instruction. |

---

## 8. One unexplained filesystem event — recorded, not hidden

During this round, `~/src/mpm/docs/dev/` was **deleted from Session A's
working tree** (the other two worktrees were unaffected). The git objects
were intact and the directory was restored cleanly with
`git checkout -- docs/dev`; **no tracked data was lost**.

A's investigation did not establish a cause. What was ruled out:

* No file in the repository removes `docs/` — `grep` over `scripts/`,
  `cmd/`, `internal/`, `Makefile` and every `agent_installation/*/install.sh`
  found no `rm -rf docs`, no `rmtree(docs…)` and no `os.RemoveAll` that
  could target it.
* `git clean` cannot remove **tracked** files.
* The other two linked worktrees, running the same scripts, were unaffected —
  which argues against a repo script and for an absolute path aimed at
  Session A's directory specifically.
* The deletion was not reproduced during a 30-second watch, and `docs/dev`
  has remained present since the restore.

A records it as an **unexplained event with no data loss**, notes the
mitigation (the content is committed, so it is always recoverable), and
notes that **Session C was demonstrably active throughout** — its worktree
files were modified at 21:15:11, seconds before A noticed. A has not
attributed blame. A's practice change: **verify the working tree is clean
of unexpected deletions before every commit.**

---

## 9. Reproducibility record

| Item | Value |
|---|---|
| Repository revision | `70deac2bb3fce3c63da28bfbd9ca82ca2c4d1e2e` (`origin/main` at adjudication) |
| Host | `opencode v2.1.4` — `~/.opencode/bin/opencode` (240 416 944 B); desktop app `/opt/OpenCode/ai.opencode` |
| Host binary evidence | dispatcher at offset `163945713`; volatility text at `122451326` and `160453401` |
| Plugin-option probe | isolated host; `$.plugins.0 kind=invalid action="skipped malformed recognized value"`; `effect(input, options)` received `options=null` |
| Isolation harness | `XDG_{CONFIG,DATA,STATE,CACHE}_HOME` + `OPENCODE_CONFIG_DIR` under a disposable root; verified with `opencode debug paths` |
| Acceptance harness | `measure/wake_context_e2e.sh` — isolated host, disposable workspace, marker memory, witness probe reading the real system-prompt array |
| Polling measurement | one `opencode serve` process, one session, three prompts; plugin fetches counted in the scratch DB by `framework_name='mpm-cli'` |
| Live DB access | **read-only** (`mode=ro`) throughout |
| Live plugin payload, live DB, live scheduler, running desktop | **unmodified, not restarted** |
