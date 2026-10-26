# E — Shared event contract: proposal v0.1 (revised) — for B/C review

> **STATUS: PROPOSAL. NOT AGREED.**
>
> Sessions B and C must both explicitly acknowledge the **minimum shared
> interface** (§7) before any dependent implementation begins. This document
> is the input to that acknowledgement, not the acknowledgement itself.
>
> **Session A does not record B's or C's approval.** §10 is filled in by B and
> C, not by A.
>
> Author: Session A. Draft 1: 2026-10-26. **Revised: 2026-10-26** after the
> contract review. Version string unchanged: `mpm-event/0.1` — no implemented
> interface depends on the field set yet, so a version change would be
> misleading. See the correction table in §0.

---

## 0. What changed from draft 1, and why

Draft 1 was a sound foundation but it made several claims that do not survive
contact with the code. Every correction below is grounded in a specific,
checkable fact about the repository as of `origin/main` = `b5b1840`.

| # | Draft 1 said | Correction | Evidence |
|---|---|---|---|
| A | Project identity included "canonical repository root identity" | Identity is derived from the git **configuration**, never from the filesystem path of a worktree, and the derivation is a documented precedence chain with an explicit "unresolved" outcome | §3.3 |
| B | "Two events with the same hash and the same type are the same observation" | **Removed.** Repeated identical operations are legitimate independent events. Deduplication is *strict* only when the host supplies a stable event key, and *best-effort* otherwise | §3.5 |
| C | `observed_at` in whole unix seconds, ordered by `(observed_at, event_id)` | Millisecond precision; a defined **total** order; observation / ingestion / causal order are three different things and causality is never inferred from timestamps | §3.6 |
| D | "An unmappable event is recorded as `session.completed` with a note" | **Removed.** Unknown events are rejected, ignored or counted — never mapped onto a neighbouring type. Inferred events are distinguishable from observed ones | §3.4, §3.10 |
| E | "The contract routes raw lifecycle events [into] `mpm_evidence`" | **Factually wrong.** The `evidence` table requires an `artifact_id` + `artifact_type`; it cannot hold a standalone observation. The demonstrated storage gap is recorded in §2.6 and no durable capability is implied before it is built | §2.3, §2.6 |
| F | Lifecycle events get `result_status=success` / `outcome_class=ok` | An observation with no operation outcome carries **no** outcome. The existing `internal.ToolOutcomeClass` enum is reused verbatim where an outcome genuinely exists, and no new vocabulary is introduced | §3.7 |
| G | Payload is "redacted by construction" from a proposed field list, then secret-scanned | **Allowlist, not denylist.** Only explicitly permitted fields cross the boundary; nothing is collected and then pattern-matched | §3.8 |
| H | "Events older than 30 days may be pruned" | **Withdrawn.** No pruning is introduced. Retention requires verified storage and scheduler behaviour, and it interacts with strict idempotency | §3.8 |

Two further corrections are structural rather than itemised:

* **§1.1 / §7** now separate the **minimum shared interface** (what B and C
  must agree on) from **implementation choices** (what neither side has
  investigated yet and what A has therefore not decided).
* **§5** separates *what the host declares* from *what must be verified at
  runtime against the installed host*. Draft 1 presented repository-declared
  TypeScript as if it were host capability. It is not.

---

## 1. Scope

### 1.1 The minimum shared interface (MSI) — what this contract fixes

Exactly seven things. B and C must agree on these and nothing more:

1. The **event envelope**: field names, types, bounds, version string (§3.1–§3.2).
2. The **closed event-type vocabulary** and the observed/inferred distinction (§3.4, §4).
3. The **identity rules**: project id derivation, session id rules, event id rules with the strict / best-effort distinction (§3.3, §3.5).
4. The **ordering rules**: precision, the total order, and the prohibition on inferring causality from timestamps (§3.6).
5. The **trust invariants**: observation-only, no automatic promotion, evidence-backed proposals (§3.9).
6. The **ingest contract**: what the core guarantees on receipt, including validation, dedup, receipt and rejection counting (§3.10). *The contract is fixed; the storage that implements it is not yet authorised — see §6.1.*
7. The **outcome semantics**: `operation` / `result_status` / `outcome_class` (§3.7).

### 1.2 Explicitly out of scope

* A generic telemetry framework. MPM already has `internal/telemetry` (cost
  frames over a socket) and `tool_invocations` (the tool-call audit). Neither
  is a host-lifecycle event log and neither is repurposed here.
* A new event table, until the storage gap in §2.6 is accepted and one
  implementation PR is agreed (§6.1).
* Retention and pruning policy (§3.8).
* The transport between adapter and core (§6.2).
* Any change to the existing `tool_invocations` schema or its `result_status`
  CHECK constraint.

---

## 2. The existing substrate — verified facts

Everything in this section was read out of `origin/main` = `b5b1840`. Nothing
here is a plan.

### 2.1 `tool_invocations` — the tool-call audit

`internal/core/schema.go:1327`. Columns that matter:

| Column | Fact |
|---|---|
| `id` | TEXT PRIMARY KEY — **a fresh `uuid.NewString()` per invocation** (`cmd/mpm/audit_hook.go:71`). It is *not* content-derived, so it carries no idempotency. |
| `session_id` | legacy per-process dispatcher grouping; defaults to `"cli-default"` |
| `tool_name`, `action` | `NOT NULL` — the table is schema-constrained to *tool invocations* |
| `invocation_id` | the substrate's correlation id across the call chain |
| `payload_hash` | `sha256:<hex>` over the JSON-encoded request payload, computed by `sha256OfPayload` (`cmd/mpm/audit_hook.go:197`). Go's `json.Marshal` sorts map keys, so it is deterministic. **It is informational; it is not a uniqueness constraint.** |
| `result_status` | `CHECK (result_status IN ('success','error'))` |
| `outcome_class` | nullable; the closed enum of `internal.ToolOutcomeClass` (`internal/core/tool_outcome.go`) |
| `started_at`, `completed_at` | **whole unix seconds** (`startedAt.Unix()`) — the ordering-precision problem in §3.6 is real in this table today |
| `duration_ms` | present, millisecond resolution, per invocation |
| `framework_session_id` | the host's native session id; **never synthesized** |

**The writer is best-effort.** `cmd/mpm/audit_hook.go` inserts the row and, on
failure, logs `slog.Warn("audit insert failed", …)` and returns. The comment at
the top of the file is explicit: *"an audit failure … must NEVER block the
underlying tool call."* The `cmd/mpm-mcp/audit_hook.go` path is the same shape.

**Consequence:** a tool invocation that is *used to deliver* an event is at
most-once and not guaranteed to be recorded at all. It is not a durable
channel for a lifecycle observation.

### 2.2 `system_audit_log` — the substrate's own operational log

`internal/core/schema.go:1044`. Carries the four optional correlation columns
`invocation_id`, `mpm_session_id`, `framework_name`, `framework_session_id`,
with this documented clause already in the schema:

> `framework_session_id` — host-owned native session ID; NULL when host
> provides none (Pi, Hermes without hooks). **NEVER synthesized** from any
> other field.

That clause is inherited by this contract verbatim. It also records
`event_code` — "the bounded machine-readable identifier for the operational
event" — which is the shape a machine-readable event code already takes in
this codebase.

This log is the substrate's *own* operational record (level / component /
message). It is not a host-event log, and its `context` JSON is free-form
operator-facing text.

### 2.3 `evidence` — **not** a general event store

`internal/core/schema.go:267`:

```sql
CREATE TABLE IF NOT EXISTS evidence (
    id           TEXT PRIMARY KEY,
    artifact_id  TEXT NOT NULL,
    artifact_type TEXT NOT NULL CHECK (artifact_type IN
                   ('memory','theory','decision','lesson','work')),
    ...
);
```

Evidence is evidence **for an artifact**. A host lifecycle observation that
has no artifact — a session start, a compaction — cannot be stored here
without inventing a fake artifact id and a fake artifact type.

> **This corrects draft 1's §1.3, which stated that "the contract routes raw
> lifecycle events here". That is not possible without corrupting the
> table's meaning.**

The table is nonetheless the right *destination* for the **promotion** side of
the trust boundary: a critic proposal's supporting observations become
evidence rows attached to the proposed artifact.

### 2.4 `work_events` and `capability_events` — both scoped

* `work_events` (`internal/core/schema.go:886`) — `work_id NOT NULL`, a
  CHECK-constrained `event_type` vocabulary
  (`created, note_appended, completed, cancelled, reopened, title_updated,
  content_updated, claimed_complete, evidence_observed, archived,
  unarchived`), `UNIQUE(work_id, event_index)`. It models the lifecycle of a
  tracked work item, not a host session.
* `capability_events` (`internal/core/schema.go:725`) — `capability_id NOT
  NULL` with `FOREIGN KEY (capability_id) REFERENCES capabilities(id) ON
  DELETE CASCADE`. It is scoped to a registered capability and is deleted with
  it.

Both are correct for what they are. Neither is a host-lifecycle log.

### 2.5 `internal/telemetry` — a different substrate

`internal/telemetry/` implements a socket-served **cost/token frame**
protocol (`Frame`, `ParseFrame`, `InsertFrame`, `QueryInvocation`) with its own
schema version (`v1`) and its own store. It is a measurement transport for
model cost. It is **not** a lifecycle-event substrate and this contract does
not extend it. Reusing it would mean redefining "frame" to mean "event",
which is the generic-telemetry-framework outcome the sprint ruled out.

### 2.6 The demonstrated storage gap

Stated plainly, because the contract must not imply a capability that does not
exist:

> **There is no durable store in MPM for a host lifecycle observation.**
>
> Every candidate table is scoped to something else: `evidence` to an
> artifact, `work_events` to a work item, `capability_events` to a
> capability, `tool_invocations` to a dispatched tool, `system_audit_log` to
> the substrate's own operations. None can hold "a host session started at
> time T" without misrepresenting what that table records.

This gap is now **demonstrated**, which is what §6.1 requires before one
minimal implementation PR is authorised. It is not a licence to build the
table now — see §6.

---

## 3. Part I — Mandatory envelope and trust invariants

Everything in Part I applies to **every** event, without exception.

### 3.1 Version

```json
"schema_version": "mpm-event/0.1"
```

* A consumer that does not recognise the version **ignores** the event and
  increments a counter. There is no partial parsing and no best-effort
  fallback.
* The field set of `mpm-event/0.1` is **frozen on acceptance**. Any change
  to it — including an additive one — is `mpm-event/0.2`.
* Adding an *event type* is a minor bump and is additive.

### 3.2 Envelope

```jsonc
{
  "schema_version": "mpm-event/0.1",
  "event_id":       "ev1_<64 hex>",   // or "ev1b_<32 hex>"  (§3.5)
  "idempotency":    "strict" | "best_effort",
  "event_type":     "session.started",   // closed vocabulary (§4)
  "observation":    "observed",          // or "inferred" (§3.4)
  "observed_at_ms": 1793021872123,       // integer, unix milliseconds
  "ingested_at_ms": 1793021872456,       // set by the core, never by the adapter
  "host_seq":       41,                  // optional; host monotonic counter, if any

  "host":           "opencode",
  "host_version":   "2.1.4",
  "source":         "opencode-plugin",
  "adapter_version": "<adapter build sha>",

  "project_id":         "pj_1a2b…",     // nullable (§3.3)
  "project_id_source":  "git-remote",   // nullable
  "framework_name":     "opencode",
  "framework_session_id": "ses_abc",    // nullable, NEVER synthesized
  "mpm_session_id":      "mpm-…",       // nullable
  "invocation_id":       "…",            // nullable
  "task_id":             "…",            // nullable
  "parent_event_id":     "ev1_…",        // nullable; explicit causality only

  "operation":     "present" | "absent",
  "result_status": "success" | "error" | "unknown",
  "outcome_class": "ok",                 // absent when operation == "absent"
  "outcome_code":  "",                   // absent when outcome_class absent/"ok"

  "trust": "observation",
  "meta":   { … },                       // allowlisted only (§3.8)
  "payload": null                       // reserved; empty in 0.1 (§3.8)
}
```

Required-field rules:

* `schema_version`, `event_id`, `idempotency`, `event_type`, `observation`,
  `observed_at_ms`, `host`, `source`, `adapter_version`, `operation`,
  `result_status`, `trust` are **always** present.
* `ingested_at_ms` is present on a stored event and **absent** on an event as
  emitted by the adapter. An adapter that sets it is malformed.
* `outcome_class` / `outcome_code` are governed by §3.7.

### 3.3 Project identity

**The rule:** project identity is derived from the **git configuration** of the
project the agent is working on, and from **explicit configuration**. It is
never derived from the path of a working directory or a worktree.

Draft 1's failure mode was structural: hashing a "canonical repository root
identity" means that `~/src/mpm`, `~/src/mpm-core` and `~/src/mpm-opencode`
hash three different strings and produce three different ids, which is the
exact opposite of the requirement.

**Resolution chain (first match wins).** The *project directory* is the host's
project directory when the adapter has one, otherwise the process working
directory. It is **not** the MPM workspace.

| # | Source | Result | `project_id_source` |
|---|---|---|---|
| 1 | `MPM_PROJECT_ID` environment variable | the value, normalised | `explicit-env` |
| 2 | `<project_dir>/.mpm-project-id` (a file in the project) | the file's first non-empty trimmed line, normalised | `explicit-file` |
| 3 | git remote identity of `<project_dir>` | derived (§ below) | `git-remote` |
| 4 | none of the above | **`project_id` is absent (null)** | `unresolved` |

Normalisation for sources 1 and 2: trim surrounding whitespace; the value must
match `^[A-Za-z0-9._:-]{1,128}$`. A value that does not is **rejected with a
counted diagnostic** and the chain continues at source 3. A rejected value
never becomes a project id.

**Remote identity (source 3).** Read `git config --get remote.origin.url` with
`-C <project_dir>`. Git resolves this from the repository's **common**
configuration, which linked worktrees share, so all three sprint worktrees
return the same value. Normalise:

1. Trim whitespace.
2. Strip a trailing `.git`, then a trailing `/`.
3. If the value is scp-like (`user@host:path`), drop the user entirely and
   convert to `host/path`.
4. If it is a URL, drop the scheme and any `user[:password]@` credentials.
5. Lower-case the host. Keep the port **unless** it is the default for the
   scheme (22 ssh, 80 http, 443 https).
6. Collapse repeated `/` in the path; strip a leading `/`.

The normalised remote is `host[:port]/path`. Then:

```
project_id = "pj_" + hex(sha256("mpm-project/1\n" + normalised_remote))[:32]
```

**Behaviour that the brief asks to be pinned:**

| Situation | Behaviour |
|---|---|
| Same repo, three linked worktrees | **Identical** `project_id`. Derived from git configuration, not from the worktree path. |
| Same repo, `git@host:org/repo.git` vs `https://host/org/repo` | **Identical** — normalisation collapses both to `host/org/repo`. |
| Fork (different remote) | **Different** `project_id`. This is intentional: a fork's content diverges. An operator who wants them unified sets `MPM_PROJECT_ID` explicitly. |
| Remote moved to a different host or path | **Different** `project_id`. This is a real identity change, overridable with `MPM_PROJECT_ID`. Documented so it is not a surprise. |
| Remote missing, not a git repository, or `git` unavailable | Falls to source 4 → `project_id` is **null**, `project_id_source` is `"unresolved"`. **A project id is never fabricated.** |
| A worktree-local git config override changes `remote.origin.url` | Honoured — git's worktree config takes precedence over the common config. This is an explicit operator action, and it is the only way two worktrees of one repository can diverge. Documented, not prevented. |

**What `project_id` is not.** It is **not** a partitioning key, **not** an
access-control boundary, and **not** a substitute for workspace isolation.
The workspace remains the isolation boundary: `MPM_WORKSPACE` decides which
database is used, exactly as today. A project id is a *label on an
observation*, not a partition. Nothing grants or restricts access because of a
project id.

**Cross-worktree pinning:** `TestProjectID_SameAcrossLinkedWorktrees` (§8)
builds a repository with two linked worktrees and asserts identical ids.

> **Verified empirically on 2026-10-26**, before this contract was put up for
> agreement — the derivation was executed, not only specified:
>
> ```
> $ for d in ~/src/mpm ~/src/mpm-core ~/src/mpm-opencode; do
>     git -C "$d" config --get remote.origin.url
>   done
> https://github.com/flowbyte-com/mpm        (all three)
>
> all three worktrees → project_id = pj_5a6c0d9dbf77282fe197b644291fd9e9
> ```
>
> (A fourth checkout, `~/src/mpm-release`, was verified at the same time and
> produced the same id. It has since been retired — see
> [`coordination.md`](coordination.md) §5.)
>
> A throwaway repository with two linked worktrees was also built to confirm
> the mechanism: all three directories report the **same**
> `--git-common-dir`, all three return the same `remote.origin.url`, and
> `git@github.com:flowbyte-com/mpm.git` and
> `https://github.com/flowbyte-com/mpm` both normalise to
> `github.com/flowbyte-com/mpm` and therefore to the same id.

### 3.4 Session identity, and the observed / inferred distinction

| Identifier | Rule |
|---|---|
| `framework_session_id` | The host's native session id. **Absent when the host provides none** — never synthesized from any other field, never derived from `mpm_session_id` or `session_id`. This inherits the clause already written into `system_audit_log` (§2.2). |
| `mpm_session_id` | MPM's own session id. Nullable; absent when there is no active MPM session. |
| `observation` | `"observed"` or `"inferred"`. **Only `"observed"` may be emitted by the adapter.** |

**Inferred events.** An event that the adapter derived rather than received
from the host is `observation: "inferred"` and **must** carry an `inference`
object naming its `basis` and its `confidence`. Inference is, however, not
part of `mpm-event/0.1`'s emitted vocabulary: **the adapter emits observed
events only**, and inference belongs in Session B's critic, where it can be
labelled as inference and argued with. The candidate `session.resumed` is
therefore **excluded from the 0.1 vocabulary** (§4) rather than being emitted
as a guessed neighbour of a real type.

**Unknown host events.** If the host delivers an event the adapter cannot map
onto the closed vocabulary, the adapter **does not** map it onto a
neighbouring type — draft 1's "record it as `session.completed` with a note"
is withdrawn. The adapter must do exactly one of:

* **reject** it (the event never becomes an envelope), or
* **ignore** it (no envelope), or
* **count** it as unsupported.

In all three cases the outcome is **observable** through the counter surface
in §3.10, and no synthetic event is created to stand in for it.

### 3.5 Event identity and deduplication

**Draft 1's claim that "two events with the same hash and the same type are
the same observation" is withdrawn.** Two identical `mpm_memory save` calls in
the same session are two legitimate, independent events. `payload_hash`
equality means *the payloads were alike*, not *the events were the same*.

The envelope therefore declares its own idempotency mode:

| Mode | When | `event_id` | Storage guarantee |
|---|---|---|---|
| `strict` | the host supplied a **stable event key** | `ev1_` + 64 lowercase hex chars, derived from the key | Inserting the same `event_id` twice yields **one** row. This is a *storage-level* guarantee, not a delivery guarantee. |
| `best_effort` | no stable host key is available | `ev1b_` + 32 lowercase hex chars, from a CSPRNG (a UUIDv4 rendered without dashes) | Every emission is a distinct row. Repeat observations accumulate. |

The prefix makes the mode self-describing: a reader can tell at a glance
whether an id is content-derived.

**Derivation in strict mode.** The host event key is the first non-empty of:
`host_event_id`, the host's `callID`, the host's `messageID` (+ `partID` when
present), or a host-provided monotonic `(session, seq)` pair. The id is
computed over a **length-prefixed** encoding so that no field boundary is
ambiguous:

```
encode(s)   := uvarint(len(s)) ‖ s_bytes
event_input := encode(schema_version) ‖ encode(host) ‖ encode(host_event_key)
event_id    := "ev1_" ‖ hex(sha256(event_input))
```

Length-prefixing removes the ambiguity of delimiter-joined concatenation: the
draft's `sha256(schema_version ‖ host ‖ framework_session_id ‖ host_event_id ‖
event_type)` could in principle be made to collide by moving characters
across a field boundary.

**Stability across retries.** In strict mode the same host event key always
yields the same `event_id` — across processes, across restarts, across
adapter versions — because the input is the key and nothing else. In
best-effort mode it does not, and the contract says so rather than implying
otherwise.

**Exactly-once is not claimed.** The contract guarantees:

* **at most one stored row per `event_id`** (a property of the store's insert),
  and
* **best-effort delivery** of envelopes from adapter to core.

It does **not** claim exactly-once delivery, end-to-end, because the host
does not provide the information required to guarantee it. Where the host
*does* provide a stable key, strict mode gives at-most-one-row storage; where
it does not, the honest mode is best-effort.

`payload_hash` is carried in `meta` using the substrate's existing format
(`sha256:<hex>` over the JSON-encoded payload, `cmd/mpm/audit_hook.go:197`),
so the same string means the same thing in the event envelope and in
`tool_invocations`. It is **not** used for deduplication.

### 3.6 Ordering, timestamps and causality

**Precision.** `observed_at_ms` is an integer of **unix milliseconds**, UTC,
taken by the adapter at the moment of observation. Never derived from a
payload field. Millisecond resolution is the minimum that can order events
produced by a single agent turn; the substrate's existing
`tool_invocations.started_at` is whole seconds, which is why two rapid
invocations are currently indistinguishable there — the envelope does not
inherit that limitation.

**Three different orders.** They are not interchangeable.

| Order | Meaning | Where it comes from |
|---|---|---|
| **Observation order** | the order the adapter saw the events | `observed_at_ms`, refined by `host_seq` when the host provides a monotonic counter. Beyond that it is **not reconstructible** from the envelope. |
| **Ingestion order** | the order the core stored them | `ingested_at_ms`. **Never** an ordering key for "what happened". |
| **Causal order** | which event caused which | **Not derivable from any timestamp.** Must be carried explicitly: `parent_event_id`, `invocation_id`, or a host pairing key such as a `callID` shared by a `before` and an `after` event. |

> **Causality is never inferred from timestamps.** Not from proximity, not
> from ingestion order, and not from a gap in a session's event stream. The
> draft's `session.resumed` heuristic — "an observed session id with a gap
> since its last event" — is exactly the inference this rule forbids, and it
> is withdrawn.

**The ordering key.** Reads order by:

```
(observed_at_ms ASC, host_seq ASC, event_id ASC)
```

with `host_seq` NULL sorting **after** all non-NULL `host_seq` values at the
same `observed_at_ms`, and `event_id` compared as an ASCII string. This is a
**total, deterministic** order: the same set of events always reads in the
same sequence, on any machine, in any process.

It is explicitly **not** a causal order, and does not claim to be.

### 3.7 Outcomes and trust

**Outcome semantics.** Two orthogonal axes, so that an observation is never
dressed up as a completed operation:

| Field | Values | Rule |
|---|---|---|
| `operation` | `present` \| `absent` | `present` only when the observation is the recorded result of an operation MPM itself performed |
| `result_status` | `success` \| `error` \| `unknown` | `unknown` is legal **only** when `operation == "absent"` |
| `outcome_class` | a member of `internal.ToolOutcomeClass` | **Absent** when `operation == "absent"`. When present, it is one of the existing enum's nine values — `ok`, `validation`, `not_found`, `conflict`, `degraded`, `substrate`, `integration`, `timeout`, `internal` — and **no new vocabulary is introduced** |
| `outcome_code` | a bounded ASCII identifier | Absent when `outcome_class` is absent or `ok`; within the class when present — the same rule `internal/core/tool_outcome.go` already states |

**A lifecycle observation with no operation behind it therefore carries no
`outcome_class` at all.** It does not claim `ok`. Draft 1's "for lifecycle
events that are not calls, `result_status` is `success` and `outcome_class` is
`ok`" is withdrawn: it asserted a success that was never observed.

**No change to the existing CHECK constraint.** `tool_invocations.result_status`
remains `CHECK (result_status IN ('success','error'))`. The envelope's
`unknown` is an **envelope-only** value and is never written into that column.
If a lifecycle store is later created (§6.1), its `result_status` column is
nullable and NULL means "no operation outcome" — which is the honest
representation and requires no vocabulary change to the existing table.

**Trust.**

* `trust` is `"observation"` and **only** `"observation"` at the event layer.
  There is no `trusted` value; a trusted artifact is a memory, a lesson or a
  decision, not an event.
* Capturing an event creates **no** row in `memories`, none in `lessons_base`,
  and none in `memories` with `collection = 'decisions'`.
* The promotion path is the existing one, unchanged:

```
event  ──(critic proposal, with the event ids it rests on)──▶  proposal
proposal ──(explicit review: mpm_request_review / challenge)──▶  memory | lesson | decision
```

* A critic proposal **must** carry the `event_id`s it rests on, and each of
  them must resolve to a stored event (or to a `tool_invocations` row). A
  proposal whose evidence cannot be resolved is **rejected** — not silently
  downgraded to an unevidenced proposal.
* Promotion follows the existing review and authority rules. Session A and the
  reviewing agent promote; neither the adapter nor the critic does.

### 3.8 Payloads, limits, versions, retention

**Allowlist, not denylist.** Only explicitly permitted fields cross the
adapter boundary. Nothing is collected and then pattern-matched for secrets —
that ordering is what makes secret-scanning unreliable in the first place.

`meta` is the only field bag in `mpm-event/0.1`. `payload` exists in the
envelope but is **always null** in 0.1.

| `meta` field | Type | Bound | Meaning |
|---|---|---|---|
| `host_event_type` | string | ≤ 64 B | the host's own event-type token, verbatim |
| `host_tool_name` | string | ≤ 128 B | the tool the host invoked |
| `host_action` | string | ≤ 64 B | the host's action verb |
| `host_result` | string | ≤ 64 B | the host's own status token (opaque — **not** MPM's `ToolOutcomeClass`) |
| `host_agent` | string | ≤ 64 B | |
| `host_model_provider` | string | ≤ 64 B | |
| `host_model_id` | string | ≤ 128 B | |
| `host_message_kind` | string | ≤ 64 B | |
| `host_error_code` | string | ≤ 64 B | a **code**, never a message |
| `compaction_kind` | string | ≤ 32 B | |
| `overflow` | boolean | — | |
| `changed_paths_count` | integer | ≥ 0 | a count, never paths |
| `duration_ms` | integer | ≥ 0 | |
| `diff_stat_summary` | string | ≤ 256 B | counts only, no paths |
| `payload_hash` | string | ≤ 80 B | the substrate's `sha256:<hex>` format (§3.5) |

**Never collected, therefore never needing secret-scanning:** prompts, message
or assistant content, file contents, file paths, command lines, command
output, environment variables, headers, credentials, tokens, raw error text,
stack traces, absolute paths outside the workspace.

**Hard limits.** Exceeding any of these **rejects and counts** the event.
The event is **never truncated into a valid-looking event**.

| Limit | Value |
|---|---|
| `meta` fields present | ≤ 16 |
| individual string field | ≤ 512 B UTF-8 (the per-field bounds above are tighter) |
| `meta` total, encoded | ≤ 2048 B |
| `payload` total, encoded | ≤ 4096 B (always 0 in 0.1) |
| **whole envelope, encoded** | **≤ 8192 B** |
| nesting depth | ≤ 3 |
| array length | ≤ 64 |

**Version handling.** An unrecognised `schema_version` is **ignored and
counted** (§3.10). Never partially parsed.

**Retention.** The store owns the data; the **scheduler** owns the execution
of any retention policy; the **adapter** owns none.

> **This contract introduces no automatic pruning.** Draft 1's "events older
> than 30 days may be pruned" is **withdrawn**: it was a proposal, not a
> measurement, and it had not been checked against storage or scheduler
> behaviour.

A retention policy may be proposed later only when both of these are
demonstrated:

1. **Storage behaviour.** What pruning does to the strict-idempotency
   guarantee: pruning a row whose `event_id` a retry would re-insert turns
   at-most-one-row into at-least-once after the window. Whether that is
   acceptable, and whether a tombstone is needed, is unresolved.
2. **Scheduler behaviour.** The scheduler is a systemd user service pinned to
   one workspace (`~/.config/systemd/user/mpm-scheduler.service`). A policy
   that runs in the scheduler must be shown not to disturb the live
   workspace's other maintenance work.

Until both are demonstrated, no pruning is enabled and no event is deleted.

### 3.9 Trust and payload invariants (summary)

Collected here so the mandatory set is in one place:

1. `trust` is `"observation"`, always.
2. Capturing an event creates no memory, lesson or decision row.
3. A proposal must cite resolvable evidence.
4. `framework_session_id` is never synthesized.
5. `project_id` is never fabricated.
6. Unknown host events are never mapped onto a neighbouring type.
7. Only allowlisted metadata crosses the boundary.
8. Over-limit events are rejected, not truncated.
9. An unrecognised version is ignored and counted, not partially parsed.
10. An observation with no operation outcome carries no outcome fields.

### 3.10 The ingest contract, and observable rejection

The **core** provides one ingest entry point with this behaviour. *This is a
contract; the storage that implements it is not yet authorised — §6.1.*

On receipt of an envelope, the core:

1. **Validates** `schema_version`; unrecognised → ignore + count.
2. **Validates** the envelope's shape, required fields and limits; failure →
   reject + count.
3. **Validates** `event_type` against the closed vocabulary; unknown →
   reject + count. *Never* a nearest-neighbour mapping.
4. **Normalises** identity: resolves `project_id` if the adapter did not, and
   never fabricates a missing one.
5. **Deduplicates** on `event_id`: in strict mode an existing id is a
   no-op; in best-effort mode every envelope is new.
6. **Stamps** `ingested_at_ms`.
7. **Stores** the envelope and returns a **receipt** containing the stored
   `event_id`, the `ingested_at_ms`, and whether the insert was new or a
   duplicate.
8. **Counts** every rejection and unsupported event, with a machine-readable
   reason (`limit_exceeded`, `unknown_version`, `unknown_event_type`,
   `invalid_envelope`, `unsupported_host_event`).

The counter surface is **required**, because a silent drop is indistinguishable
from a working system. It must be reachable through an existing
read-only surface (the candidate is `mpm_context`; the exact surface is §6.4).

The core **does not** decide what is worth remembering. The adapter does not
write to the database directly. Neither side promotes anything.

---

## 4. Part II — Optional lifecycle event types

The vocabulary is **closed**. A type is added by a minor version bump.

Every type below is **optional**: each depends on a host capability that may
be absent (§5), and an absent capability yields **no event and no
placeholder** — only a capability-report entry (§5).

| Event type | Required capability | What it asserts | Notes |
|---|---|---|---|
| `session.started` | an observable session-creation signal | a session was created | may legitimately carry a null `framework_session_id` if the host gives none — then it is a **degraded** observation and is flagged, not fabricated |
| `session.observed` | any host event that carries a session id | the adapter saw an event for this session | the neutral alternative to `session.started` when the host exposes no creation signal |
| `context.changed` | `experimental.chat.system.transform` | the system-prompt context was assembled | **already registered** by the plugin |
| `tool.outcome` | a host tool-execution signal | a host tool call completed | distinct from MPM's own `tool_invocations` row: the former is the *host's* observation, the latter is *MPM's* audit of its own dispatch |
| `task.boundary` | the `mpm_work` lifecycle | a tracked work item was created / completed / cancelled | `works` + `work_events` already exist |
| `compaction.started` | a compaction hook | compaction began | highest-risk type; depends entirely on §5 |
| `compaction.completed` | a compaction hook | compaction finished | |
| `handoff.written` | `mpm_handoff write` | a handoff was written | `session_handoffs` already exists |
| `critic.proposal` | the critic's proposal path | the critic raised a proposal | the proposal's own evidence chain is §3.7 |

**Explicitly excluded from `mpm-event/0.1`:** `session.resumed`. It is an
*inference*, and the adapter emits observed events only (§3.4). If a later
version wants it, it must be `observation: "inferred"` with a declared basis,
and it must never be produced by inspecting a gap in a session's event stream.

Adding a type is additive and minor. Removing or redefining a type is major
and needs a migration note.

---

## 5. Part III — Host capabilities requiring verification

**What follows is the honest state of knowledge.** The repository's
`agent_installation/mpm-opencode/src/opencode-sdk.d.ts` is a **hand-written
curated subset** — a mapping of the host's hook surface onto a
`PluginContext` shape (`tool.transform`, `session.hook`, `shell.hook`). It is
**not** evidence about what the installed host exposes.

The host actually installed on this machine is **`opencode v2.1.4`**
(`~/.opencode/bin/opencode`), a compiled binary. Its plugin API is supplied
at runtime by its own loader. The `@opencode/plugin` package present in the
plugin's `node_modules` is a **probe stub** (`"version": "0.0.0-probe"`) whose
only job is to satisfy the import outside the host — it contains no type
information. The `@opencode-ai/plugin@1.19.0` package in the repo's
`node_modules` is the **v1** SDK and is likewise not the v2 host's surface.

So the capabilities below are **declared somewhere**, and each still has to be
**verified against the installed host** by Session C.

| Capability | Where it is declared | Must be verified by C |
|---|---|---|
| host event stream (`event` hook) | `@opencode-ai/plugin@1.19.0` `Hooks.event?: (input: { event: Event })` | whether the **v2.1.4** host exposes an equivalent, and exactly what it delivers |
| `session.compacted` / `session.idle` / `session.error` | `@opencode-ai/sdk` `Event` union (`types.gen.d.ts:414,420,519`) | whether the host's event stream delivers them, and with which payload |
| compaction hooks | `@opencode-ai/plugin@1.19.0`: `experimental.session.compacting` (sessionID), `experimental.compaction.autocontinue` (sessionID, agent, model, provider, message, overflow) | whether the v2 host exposes them; whether they fire in practice |
| `tool.execute.before` / `tool.execute.after` | v1 `Hooks` — both carry `tool`, `sessionID`, **`callID`** | whether v2 exposes them; whether `callID` is stable across the before/after pair (**the leading candidate for a strict-mode host event key**) |
| `chat.message` | v1 `Hooks` — `sessionID`, `agent`, `model`, `messageID` | whether v2 exposes it; whether `messageID` is a stable key |
| `command.execute.before` | v1 `Hooks` — `command`, `sessionID`, `arguments` | whether v2 exposes it; a task-boundary candidate |
| `experimental.chat.system.transform` | v1 `Hooks`; **already used** by the plugin via `ctx.session.hook("context")` | already verified by the plugin working |
| `shell.env` | v1 `Hooks`; **already used** via `ctx.shell.hook("create.before")` — and it carries **no** `sessionID` | already verified; note the absent session id is a real constraint, not an oversight |
| project identity from the host | v1 `PluginInput` carries `directory`, `worktree`, `project: Project`; `WorkspaceInfo` carries `projectID` | whether the v2 host's plugin input exposes a **stable project id**, and whether `worktree` differs per linked worktree — this could supersede the git-remote derivation in §3.3 |

**Session C's first task is to replace this table with measured facts**, and to
classify every type in §4 as *directly observable*, *reliably inferable*, or
*unobservable / unsupported*. Until then, every type in §4 is a **candidate**.

---

## 6. Part IV — Unresolved implementation choices

These are **not** decided by this contract. Each is listed with the decision
that has to be made and by whom.

| # | Question | Who decides | Status |
|---|---|---|---|
| 6.1 | **Storage path for lifecycle events.** (a) A new core-owned table — the recommendation, since the gap in §2.6 is demonstrated; (b) reuse `tool_invocations` through an `mpm_lifecycle` tool — rejected for now, because it makes the dispatch the event and the dispatch is best-effort | A, after the B/C audits, as **one** implementation PR landing in `internal/core/schema.go` with its contract tests in the same PR | **Open.** The contract does not imply a durable capability that does not exist yet. |
| 6.2 | **Transport** from adapter to core. The adapter is a Node plugin and the core is a Go binary, so it is out-of-process. Options: a unix socket owned by the core, a batched file, `mpm call` per event (expensive, and its audit row is best-effort) | A + C, in the implementation PR | **Open.** |
| 6.3 | **Retention policy** | A, after storage and scheduler behaviour are verified (§3.8) | **Deferred.** |
| 6.4 | **The exact surface** of the ingest entry point, the receipt, and the rejection counter | A, in the implementation PR | **Open.** |
| 6.5 | Whether `project_id` / `project_id_source` become columns | A, in the implementation PR | **Open.** |
| 6.6 | Whether any inferred event type is admitted in a later version | B + C | **Open.** Excluded from 0.1. |
| 6.7 | The final `meta` allowlist | C, once the host's real payload shapes are measured | **Proposed above; not final.** |
| 6.8 | The exact byte ceilings | A + C, after measurement | **Proposed; not measured.** |
| 6.9 | Whether the ingest accepts batches, and whether a batch is atomic | A | **Open.** |
| 6.10 | Whether `mpm-event/0.2` is needed for anything | A | **Not needed** — `unknown` is envelope-only and never enters the `tool_invocations` CHECK constraint. |

---

## 7. The agreement surface — what B and C must agree to

Agreement is on the **minimum shared interface**, item by item. Nothing in
§4's optional types, nothing in §6's implementation choices, and nothing
about storage is required for agreement.

- [ ] **M1 — Envelope** (§3.1, §3.2). Field names, types, bounds, the frozen
  `mpm-event/0.1` field set, and the rule that an unrecognised version is
  ignored and counted rather than partially parsed.
- [ ] **M2 — Vocabulary and classification** (§3.4, §4). A closed vocabulary;
  unknown host events are rejected / ignored / counted and never mapped onto
  a neighbouring type; the adapter emits observed events only; inferred
  events are distinguishable and are excluded from 0.1.
- [ ] **M3 — Identity** (§3.3, §3.5). The project-id resolution chain and its
  honest "unresolved" outcome; `framework_session_id` is never synthesized;
  `event_id` in `strict` vs `best_effort` mode, the length-prefixed
  derivation, and the explicit non-claim of exactly-once delivery.
- [ ] **M4 — Ordering** (§3.6). Millisecond precision; the total ordering key;
  observation / ingestion / causal order are distinct; causality is never
  inferred from timestamps.
- [ ] **M5 — Trust** (§3.7). Observation-only; no automatic promotion;
  proposals must cite resolvable evidence; promotion follows the existing
  review and authority rules.
- [ ] **M6 — Ingest contract** (§3.10). Validate → classify → dedup → stamp →
  store → receipt, with a required observable rejection-counter surface. *The
  storage is not part of this agreement.*
- [ ] **M7 — Outcomes** (§3.7). `operation` / `result_status` / `outcome_class`
  with the existing enum reused verbatim, no new vocabulary, and no outcome
  asserted for an observation that has none.

---

## 8. Required contract tests

The contract is only real if it is pinned. Each test belongs in the PR that
introduces the corresponding code — **the tests do not have to exist before
this contract is accepted.**

| Test | Pins |
|---|---|
| `TestProjectID_SameAcrossLinkedWorktrees` | the same repository, from two linked worktrees, resolves to one `project_id` |
| `TestProjectID_SSHAndHTTPSCollapse` | the same repo over `git@host:org/repo.git` and `https://host/org/repo` resolves to one `project_id` |
| `TestProjectID_ExplicitConfigWins` | `MPM_PROJECT_ID` and `.mpm-project-id` take precedence over the derived value |
| `TestProjectID_MissingRemoteIsHonest` | no remote → `project_id_source` is `unresolved`; no id is fabricated |
| `TestProjectID_WorkspaceIsNotProject` | the workspace path never appears in a derived `project_id` |
| `TestEventID_StrictIsStableAcrossRetries` | the same host event key yields the same `event_id` across processes |
| `TestEventID_BestEffortIsDistinctPerEmission` | two emissions of the same observation get distinct ids and two rows |
| `TestEventID_EncodingIsUnambiguous` | a length-prefix collision attempt yields different ids |
| `TestEventDedup_StrictIsIdempotent` | inserting the same strict `event_id` twice yields one row and no error |
| `TestEventOrdering_UsesObservedAtMsNotIngestedAt` | out-of-order arrival is stored, and reads order by the ordering key |
| `TestEventOrdering_TotalOrderIsDeterministic` | equal `observed_at_ms` events have a stable total order |
| `TestEventCausality_NeverInferredFromTimestamps` | a gap in a session's event stream does not produce a causal link |
| `TestEventPayload_AllowlistDropsUnknownFields` | a field outside the allowlist is dropped at the adapter, not stored |
| `TestEventPayload_LimitsRejectNotTruncate` | an over-limit event is rejected and counted, and no truncated event is stored |
| `TestEventTrust_NeverPromotedAutomatically` | capturing an event creates no memory, lesson or decision row |
| `TestEventProposal_EvidenceMustResolve` | a critic proposal citing an unresolvable event id is rejected |
| `TestEventAbsentCapability_ProducesNoEvent` | a capability that did not register yields no event and no placeholder |
| `TestEventUnknownVersion_IsIgnoredAndCounted` | an unrecognised version is ignored and counted, never partially parsed |
| `TestEventOutcome_LifecycleHasNoOutcomeClass` | an observation with `operation=absent` carries no `outcome_class` and does not claim `ok` |
| `TestEventOutcome_UsesExistingToolOutcomeClass` | when `operation=present`, `outcome_class` ∈ `internal.ToolOutcomeClass` — no new vocabulary |
| `TestEventOutcome_UnknownNeverEntersToolInvocations` | the envelope's `unknown` is never written into `tool_invocations.result_status` |
| `TestEventSessionId_NeverSynthesized` | an absent host session id stays NULL in the stored event |
| `TestEventCLIMcpParity` | the CLI and MCP paths produce the same envelope for the same input |

---

## 9. Open questions for B and C

1. **Which host capabilities does the installed OpenCode v2.1.4 actually
   expose?** (§5 — Session C, before the contract is finalised.)
2. Does the host's plugin input carry a **stable project id** that would
   supersede the git-remote derivation in §3.3? If it does, and it is stable
   across linked worktrees, it should replace it. (§5 — Session C.)
3. Is `strict` mode achievable for the types B actually needs, given which
   host keys exist? If the answer is "no" for most types, the honest
   consequence is that most events are best-effort, and B's retrospective
   analysis must tolerate duplicate observations. (§3.5 — Session B.)
4. Does B's existing critic path have a place to carry `event_id`s as
   evidence references, or does that need a change to the proposal shape?
   (§3.7 — Session B.)
5. Does the existing `evidence` table's `artifact_type` vocabulary need a new
   member to carry "this evidence is a lifecycle observation"? Adding one is a
   schema change and therefore a shared-interface change. (§3.7 — Session B
   proposes, A arbitrates.)
6. Are there lifecycle events B needs that are **not** in §4's vocabulary and
   are not inferable from anything the host provides? Those are candidates
   for a `0.2` bump. (§4 — Session B.)
7. Is the 8192-byte envelope ceiling workable for the richest event B expects
   to reason over? (§3.8 — Session B.)
8. Should the counter surface live in `mpm_context` (read-only, already
   exists) or in a dedicated diagnostic action? (§3.10 — A arbitrates.)

---

## 10. Agreement block

> **To be completed by Sessions B and C. Session A's acceptance is not
> sufficient, and Session A does not fill this in on their behalf.**
>
> Agreement is on the **minimum shared interface** (§7). Disagreeing with a
> row of §6 is not a blocking objection; disagreeing with a row of §7 is.

```
Session B — name: ....................  contract version accepted: mpm-event/0.1
  date: ............
  M1 envelope ....................... agree / object (reason) ................
  M2 vocabulary & classification .... agree / object (reason) ................
  M3 identity ....................... agree / object (reason) ................
  M4 ordering ....................... agree / object (reason) ................
  M5 trust ......................... agree / object (reason) ................
  M6 ingest contract ............... agree / object (reason) ................
  M7 outcomes ...................... agree / object (reason) ................
  blocking objections: ............................................

Session C — name: Session C (OpenCode adapter & behavioural evaluation)  contract version accepted: mpm-event/0.1
  date: 2026-10-26   (revised same day — see "superseded block" below)
  M1 envelope ....................... agree — every required field is obtainable: `observed_at_ms`
                                       is the host event's own `created` (unix ms), `host_version` is
                                       ctx.app.version, `adapter_version` is the plugin build.
  M2 vocabulary & classification ... AGREE — objection withdrawn. Measured: the host event stream
                                       IS reachable from a plugin
                                       (ctx.event.subscribe({signal}) -> AsyncIterable), and both
                                       previously-objected rows are directly observable:
                                       `session.started` <- `session.created` (carries sessionID,
                                       projectID, parentID, agent, model) and the pair
                                       <- `session.compaction.started` / `.ended` (both live-captured;
                                       `.delta` and `.failed` also exist). `session.observed` stays
                                       as the unconditional base: the adapter's OWN first session is
                                       created before its iteration loop runs, so that one
                                       `session.created` can be missed. Missing must never be
                                       inferred. Closed vocabulary, no-neighbour-mapping and
                                       observed-only are all implementable and endorsed.
  M3 identity ....................... AGREE — objection downgraded to a non-blocking recommendation.
                                       Every event carries a stable per-event `evt_<hex>` id, i.e.
                                       §3.5's first-choice `host_event_id`, so strict-mode derivation
                                       cannot collide even when nested tool calls share one callID.
                                       RECOMMENDATION (R1, not a blocker): still add `event_type`
                                       (+ host tool name when present) to the derivation input and
                                       add `TestEventID_NestedToolCallsDoNotCollide`, so the guarantee
                                       does not depend on which host key a future adapter picks.
                                       Project identity (improvement): adopt the host's id when a git
                                       remote is independently confirmed — see report §4.3.
  M4 ordering ....................... agree — the event carries `created` in ms; the host gives no
                                       monotonic per-session counter, so `host_seq` is null in
                                       practice and §3.6's NULL ordering already covers it.
                                       Causality stays explicit (parentID / callID / parent_event_id).
  M5 trust ......................... agree — the adapter is structurally incapable of promotion.
                                       NOTE: the event stream widens what is COLLECTED (full tool
                                       input/output, compaction summary text, assistant text), so
                                       §3.8's allowlist is now the load-bearing control.
  M6 ingest contract ............... agree — validate/classify/dedup/stamp/store/receipt plus an
                                       observable rejection counter are implementable. Transport
                                       (§6.2) remains open; measured cost of one `mpm call` spawn is
                                       ~39 ms, and the stream is volatile by contract, so no
                                       subprocess call may run inside the event loop.
  M7 outcomes ...................... agree, one request — §3.7 needs an explicit mapping from the
                                       host's tool-execution status vocabulary to
                                       internal.ToolOutcomeClass's nine values. The host supplies
                                       `session.execution.failed` / `session.tool.failed`, so real
                                       outcomes exist; the mapping must still be pinned by A or B.
  blocking objections: 0  (was 2 — both withdrawn by measurement, not by preference)
  non-blocking recommendations: R1 add event_type to the strict-mode derivation (+ its test);
                                R2 document the first-session startup race beside §4's
                                   `session.started`.
  Evidence: docs/dev/session-c-event-stream-correction.md (supersedes the M2/M3 analysis in
            docs/dev/session-c-host-findings.md §1.1-§1.2 and §4.1.1).
```

> ### Superseded Session C block — recorded verbatim, not deleted
>
> The block below was Session C's first position (`70deac2b`). **Both of its
> blocking objections were withdrawn on 2026-10-26** after the host event
> stream was measured correctly. The text is preserved because the reason it
> was wrong is a reusable lesson: the first-pass probe called
> `ctx.event.subscribe(callback)` and never iterated the returned
> `AsyncIterable`, so "the stream is inert" was an artifact of the call shape,
> not a host limitation. A negative capability claim is only as strong as the
> call that produced it.
>
> ```
> M2 vocabulary & classification ... object (BLOCKING) — §4's `session.started` has no observable
>                                        signal in opencode v2.1.4 (no session-creation hook; the host
>                                        event stream is unreachable from a plugin) ...
> M3 identity ....................... agree, object (BLOCKING) — §3.5's strict-mode derivation must
>                                        also include `event_type` ...
> blocking objections: 2 ...
> ```

**Until this block is filled in, Sessions B and C may audit and investigate,
but may not implement against the contract.** A single unresolved blocking
objection stops dependent implementation; it is not a matter of degree.
