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

> **A host-supplied project id is adopted only when a git remote
> independently confirms it.** The measured host supplies one
> (`session.created.data.projectID`, and `ctx.location.project.id`), and it
> agrees with the git-derived identity. When both are present and agree, the
> derived value is used and `project_id_source` becomes **`host-git-remote`**,
> which records that two independent sources confirmed it. A host id on its
> own is **not** a project identity — it is another party's label.

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
`host_event_id`, the host's `messageID` (+ `partID` when present), or a
host-provided monotonic `(session, seq)` pair. The id is computed over a
**length-prefixed** encoding so that no field boundary is ambiguous:

```
encode(s)   := uvarint(len(s)) ‖ s_bytes
event_input := encode(schema_version) ‖ encode(host) ‖ encode(event_type) ‖ encode(host_event_key)
event_id    := "ev1_" ‖ hex(sha256(event_input))
```

**`event_type` is part of the derivation input.** It costs nothing and it
removes the guarantee's dependence on *which* host key an adapter happens
to select.

> **A host's per-call correlation id is NOT an event identity.**
>
> The measured host behaviour is that **nested tool calls share a single
> correlation id** — one `call_…` value covers both an outer call and the
> call nested inside it. Such a value is an **operation correlation
> identifier**, not an event identifier: deriving an `event_id` from it
> would give the outer and the inner event the same id, and the second
> would be silently deduplicated away.
>
> A host correlation id therefore travels as `meta.host_call_id` — a
> **correlation** field, usable for pairing a "before" with an "after" —
> and is **never** used as a unique event key.
>
> The **measured** host event identity for OpenCode v2.1.4 is a per-event
> `evt_<hex>` id: unique per published event (119 of 119 distinct in the
> committed captures), present on **every** event, and accompanied by a
> millisecond `created` stamp on the event itself. That is the contract's
> first-choice `host_event_id`, and it is what makes strict-mode identity
> trivially available without any derivation risk.

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

**The host-status → MPM-outcome mapping (pinned by A, 2026-10-26).** Both
sessions asked A to pin this so the adapter would not invent a vocabulary.
The rule is that the **host's vocabulary is opaque** and lives in
`meta.host_result`; the **MPM vocabulary is used only when an MPM operation
actually produced the observation**:

| Host signal | `meta.host_result` | `operation` | `result_status` | `outcome_class` |
|---|---|---|---|---|
| `session.execution.succeeded` | `"succeeded"` | `absent` | `unknown` | *absent* |
| `session.execution.failed` | `"failed"` | `absent` | `unknown` | *absent* |
| `session.execution.interrupted` | `"interrupted"` | `absent` | `unknown` | *absent* |
| `session.tool.success` | `"success"` | `present` | `success` | `ok` |
| `session.tool.failed` | `"failed"` | `present` | `error` | `internal` **only** when the host gives no finer signal; otherwise the adapter maps the host's finer code through the **existing** enum and documents the mapping |
| `session.compaction.started` / `.ended` | `"started"` / `"ended"` | `absent` | `unknown` | *absent* |
| `session.compaction.failed` | `"failed"` | `absent` | `unknown` | *absent* |

**No host status is coerced into `ToolOutcomeClass` without an operation
behind it, and no new enum value is introduced.**

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

> **The stream is volatile by contract, and that constrains the adapter.**
> The host's own API description of the same stream states: *"Subscribe to
> events … Volatile by contract: a slow consumer overflows and fails the
> stream, and events during disconnection are missed."* Two consequences are
> binding on any implementation:
>
> 1. **No subprocess call may run inside the event iterator.** The adapter
>    must enqueue and handle envelopes out of band. (Measured: one `mpm call`
>    spawn costs ~39 ms; a synchronous call inside the loop is a slow
>    consumer, and a slow consumer fails the stream.)
> 2. **Best-effort delivery is not a fallback — it is the contract.** The
>    stream does not replay, and a missing event is not evidence that
>    nothing happened. §3.5 already refuses to claim exactly-once; this is
>    why.

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
| `host_event_type` | string | ≤ 64 B | the host's own event-type token, verbatim |
| `host_call_id` | string | ≤ 128 B | the host's per-call **correlation** id — **never** an event identity (§3.5) |
| `host_message_id` | string | ≤ 128 B | the host's message id, when present |

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
| `session.started` | an observable session-creation signal | a session was created | may legitimately carry a null `framework_session_id` if the host gives none — then it is a **degraded** observation and is flagged, not fabricated. **Startup race (measured):** the host creates the adapter's *own* first session *before* the adapter's iterator starts consuming, so that one creation event may never arrive. Its absence must **not** be read as "no session started", and it must **not** be inferred — `session.observed` is the unconditional base type. |
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

## 5. Part III — Host capabilities — **measured** (replaces the declared table)

> **This section was rewritten on 2026-10-26.** The previous version listed
> capabilities *declared* in a type file and asked Session C to verify them.
> Session C has now measured the installed host, and A has independently
> confirmed the central facts. **The table below is measured, not declared.**

**The measurement.** An isolated OpenCode v2.1.4 host, three runs, a probe
that consumed the stream correctly. Committed captures:
`agent_installation/mpm-opencode/measure/evidence/event_stream_probe_run*.jsonl`.
**119 event records, 32 distinct types, per run 22 / 31 / 26.**

**The host's own dispatcher** (extracted from `~/.opencode/bin/opencode` at
byte offset `163945713`):

```js
event:{subscribe:(o)=>z(e.event.subscribe().pipe(Ga((r)=>T(r)),Ir((r)=>r)),o)}
```

`z` is `Plugin.Event.makeStreams`, whose only public member is
`[Symbol.asyncIterator]`. **The signature is `subscribe(options?) -> AsyncIterable`.**
The v1 SDK's `Hooks.event?: (input: {event})` and the repository's
hand-written `opencode-sdk.d.ts` describe a **different** API and are not
evidence about this host.

### 5.1 The measured surface

| Capability | Status on opencode v2.1.4 | Evidence |
|---|---|---|
| **host event stream** | ✅ **WORKS** — `ctx.event.subscribe({signal})` returns an `AsyncIterable` | 119 records / 32 types captured; `formA.result → isAsyncIterable:true` |
| **per-event identity** | ✅ every event carries a unique `evt_<hex>` id | 119/119 distinct, all `evt_`-prefixed |
| **per-event timestamp** | ✅ `created`, unix **milliseconds**, host-stamped | all 119 values integer, ms magnitude |
| **session creation** | ✅ `session.created` — `sessionID, projectID, parentID, slug, title, agent, model, version, location, subpath` | live-captured |
| **compaction pair** | ✅ `session.compaction.started` (`sessionID, reason, inputID`) and `.ended` (`sessionID, reason, model, text, cost, tokens`); `.delta` and `.failed` also in the host vocabulary | live-captured |
| **execution lifecycle** | ✅ `session.execution.started` / `.succeeded` (`sessionID`); `.failed` / `.interrupted` named in the host, **not yet exercised** | live-captured / expected |
| **tool lifecycle** | ✅ `session.tool.input.started/.ended`, `.called`, `.progress`, `.success` (`.failed` named, not yet exercised) — all carry the tool `id` (`call_…`) | live-captured |
| **session identity** | ✅ `ctx.session.hook("context")` carries `sessionID` | already used by the plugin |
| **shell provenance** | ✅ `ctx.shell.hook("create.before")` — **carries no `sessionID`** | already used by the plugin |
| **compaction *hooks*** | ❌ `experimental.session.compacting` / `experimental.compaction.autocontinue` **do not exist** on this host — but `session.compaction.*` **events** do | measured absence |
| **`chat.message` hook** | ❌ does not exist on this host | measured absence |
| **`command.execute.before` hook** | ❌ does not exist on this host | measured absence |
| **plugin options** | ❌ **the v2.1.4 config normaliser rejects the tuple form** (`$.plugins.0 kind=invalid action="skipped malformed recognized value"`); `effect(input, options)` receives `options = null` | measured — **there is no plugin-options configuration channel in this host version** |
| **host project identity** | ⚠ `session.created.data.projectID` and `ctx.location.project.id` both supply an id. A host id is adopted **only when a git remote independently confirms it** (§3.3) | measured; qualifier added |

### 5.2 The redaction hazard — the allowlist is now load-bearing

The stream delivers **full tool input, full tool output content, the entire
compaction summary text, assistant text, and user-authored titles.** The
measured `data` keys that carry forbidden content:

| Field | Contains |
|---|---|
| `session.text.delta` / `.ended`.`text` | assistant output |
| `session.tool.input.ended`.`text` | raw tool input |
| `session.tool.called`.`input` | the parsed input object |
| `session.tool.success`.`content` | raw tool output |
| `session.compaction.delta` / `.ended`.`text` | the full compaction summary |
| `session.created`.`title`, `session.renamed`.`title` | user-authored text |
| `session.instructions.updated`.`delta` | instruction-content hashes |

**Consequences, binding on any implementation:**

1. The adapter **maps explicitly** per event type. **No spread, no `data`
   forwarding.**
2. The envelope's `payload` **stays `null`** in `mpm-event/0.1`.
3. The regression test must run against a **real captured shape** — the
   committed captures are the fixtures. A hand-written shape is not
   sufficient evidence.

### 5.3 The first-session startup race

The host creates the adapter's **own** first session before the adapter's
iterator starts consuming, so that one `session.created` may never be
delivered. Every child/subagent session created afterwards **is** delivered.

* `session.created` is directly observable **for sessions created while the
  adapter is consuming**.
* For the adapter's own first session it may be missed — so the adapter must
  not treat its absence as "no session started", and must **not** infer it.
* `session.observed` remains the unconditional base type.

### 5.4 What the first pass got wrong, and why it is kept

Session C's first pass concluded the stream was unreachable. That was a
**measurement error**: the probe called `ctx.event.subscribe(callback)` and
never iterated the returned `AsyncIterable`. Its own capture recorded
`{"dispKeys":[]}`, which it read as "the host publishes nothing".

The lesson, kept in the record because it is the sprint's recurring failure
mode: **a negative capability claim is only as strong as the call that
produced it.** The first-pass report is preserved unedited.

## 6. Part IV — Unresolved implementation choices

These are **not** decided by this contract. Each is listed with the decision
that has to be made and by whom.

| # | Question | Who decides | Status |
|---|---|---|---|
| 6.1 | **Storage path for lifecycle events.** (a) A new core-owned table; (b) reuse `tool_invocations` through an `mpm_lifecycle` tool — rejected, because it makes the dispatch the event and the dispatch is best-effort | A | **DECIDED — deferred. No new table is authorised now, and it is explicitly NOT the first implementation milestone.** The B/C audits show the first end-to-end acceptance test needs no lifecycle store: `epistemic_provenance` already provides typed, validated and idempotent citations, and the retrospective join runs over the existing `tool_invocations` × `artifact_provenance`. Revisit only against a **measured** gap, and then as one PR in `internal/core/schema.go` with its contract tests. |
| 6.2 | **Transport** from adapter to core. The adapter is a Node plugin and the core is a Go binary, so it is out-of-process. Options: a unix socket owned by the core, a batched file, `mpm call` per event (expensive, and its audit row is best-effort) | A + C, in the implementation PR | **Open**, with one constraint now fixed (§3.8): **no subprocess call may run inside the event iterator** — measured ~39 ms per spawn, and the stream is volatile by contract. Envelopes must be queued and handled out of band. |
| 6.3 | **Retention policy** | A, after storage and scheduler behaviour are verified (§3.8) | **Deferred.** |
| 6.4 | **The exact surface** of the ingest entry point, the receipt, and the rejection counter | A | **DECIDED — `mpm_system`**, not `mpm_context`. A new action `mpm_system action=event_counters` beside `critic_findings` and `query_audit_log`. `mpm_context` is the session-start surface and already carries `contextual_focus`; ingestion counters would add tokens to every wake for information no agent needs at session start. **The receipt's new-or-duplicate determination must come from a read-back of the stored row, not from the `INSERT` result** (Substrate Defense Triad rule 3). |
| 6.5 | Whether `project_id` / `project_id_source` become columns | A, in the implementation PR | **Open.** |
| 6.6 | Whether any inferred event type is admitted in a later version | B + C | **Open.** Excluded from 0.1. |
| 6.7 | The final `meta` allowlist | C, once the host's real payload shapes are measured | **CLOSED.** The measured shapes are in §5.2; the allowlist gains `host_event_type`, `host_call_id` and `host_message_id` (§3.8). Everything content-bearing stays out, and a regression test must use the **real captured shapes**. |
| 6.8 | The exact byte ceilings | A + C, after measurement | **CLOSED — no change.** The richest *allowlisted* envelope is tiny; the rich fields are exactly the ones that must not be collected (§5.2). |
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

> **RECORDED AS AGREED — 2026-10-26, by Session A.**
>
> Both Sessions B and C accepted `mpm-event/0.1` on the **minimum shared
> interface (M1–M7)** with **zero blocking objections**. A has applied
> amendments **A1–A13** (see
> [`session-a-adjudication.md`](session-a-adjudication.md) §1.2).
>
> **Agreement is not implementation.** Nothing in the substrate yet receives,
> stores or counts a lifecycle event. §6.1 is explicitly deferred and the
> lifecycle-events table is **not** the first implementation milestone.
>
> The agreement evidence, in the shared dogfooding workspace:
> Session B's decision `3a987eca2c708fac`; Session C's decision
> `281d5267b2e9d4d4`, which supersedes its withdrawn first-pass
> acknowledgement `8cf1f0e3cc41465e`.

```
Session B — name: Session B (intelligence core)   contract version accepted: mpm-event/0.1
  date: 2026-10-26
  M1 envelope ....................... agree  (2 notes: the always-null `payload` field is
                                       sustained as reserved and outside the frozen set's
                                       byte accounting; a host timestamp is a NEW `meta`
                                       field in a 0.2 bump, never a substitute for
                                       observed_at_ms)
  M2 vocabulary & classification .... agree  (dependency recorded, not a disagreement:
                                       M2/M6 depend on 6.1 and 6.4 being decided)
  M3 identity ....................... agree  (clarification: `mpm_session_id` is the MPM
                                       interaction-lifecycle identity, not a per-conversation
                                       one)
  M4 ordering ....................... agree
  M5 trust ......................... agree  (one clause-internal tension referred to A —
                                       ruled in session-a-adjudication.md §2, A-1)
  M6 ingest contract ............... agree  (one requirement added: the receipt's
                                       new-or-duplicate determination comes from a READ-BACK
                                       of the stored row, not the INSERT result)
  M7 outcomes ...................... agree  (clarification: `result_status`/`outcome_class`
                                       are MPM-scoped, not host-scoped; the host's status
                                       belongs in `meta.host_result`)
  blocking objections: 0
  referred to A: A-1 (M5 tension), A-2 (6.1 storage), A-3 (6.4 surface), A-4/A-5 (registry),
                  A-6 (environment), A-7 (runbook), A-8 (triage), A-9 (registry/evidence)

Session C — name: Session C (OpenCode adapter)   contract version accepted: mpm-event/0.1
  date: 2026-10-26  (second pass; supersedes the withdrawn first pass of the same day)
  M1 envelope ....................... agree
  M2 vocabulary & classification ... AGREE — objection WITHDRAWN. The host event stream IS
                                       reachable (`ctx.event.subscribe({signal})` ->
                                       AsyncIterable); `session.created` and the compaction
                                       pair are live-captured. `session.observed` stays the
                                       unconditional base. Missing must never be inferred.
  M3 identity ....................... AGREE — objection downgraded. Every event carries a
                                       stable `evt_<hex>` id, so strict derivation cannot
                                       collide. R1 accepted as amendment A2 (`event_type` in
                                       the derivation input) and A1 (the host's `callID` is a
                                       CORRELATION id — nested calls share one — and is
                                       removed from the unique-key chain).
  M4 ordering ....................... agree
  M5 trust ......................... agree — the adapter is structurally incapable of
                                       promotion. NOTE: the event stream widens what is
                                       COLLECTED, so §3.8's allowlist is load-bearing.
  M6 ingest contract ............... agree — transport (6.2) stays open; ~39 ms per
                                       `mpm call`; no subprocess call inside the event loop.
  M7 outcomes ...................... agree, one request — SATISFIED: A pins the
                                       host-status -> ToolOutcomeClass mapping in §3.7.
  blocking objections: 0  (was 2 — both withdrawn by measurement, not by preference)
  non-blocking recommendations: R1 -> A2 (adopted); R2 -> A4 (adopted)
  referred to A: C-3 (project id -> A13), C-4 (outcomes -> A12), C-5 (transport -> A8),
                  C-6 (capability table -> A6), C-7 (allowlist -> A7), C-8 (ceilings -> closed),
                  C-9 (volatility -> A5)
```

**Amendments A1–A13, applied by Session A on 2026-10-26:**

| # | Amendment |
|---|---|
| A1 | §3.5 — the host's `callID` is removed from the unique host-event-key chain; it is a **correlation** id (`meta.host_call_id`), never an identity. Nested tool calls share one. |
| A2 | §3.5 — `event_type` is added to the strict-mode derivation input. |
| A3 | §3.5 — the measured host event identity is recorded: `evt_<hex>`, unique per published event. |
| A4 | §4 — the first-session startup race is recorded beside `session.started`. |
| A5 | §3.8 / §6.2 — the stream's volatility and the "no subprocess inside the iterator" rule are binding. |
| A6 | §5 — the capability table is **replaced** with the measured surface. |
| A7 | §3.8 / §6.7 — the `meta` allowlist gains `host_event_type`, `host_call_id`, `host_message_id`. |
| A8 | §6.2 — the transport constraint is fixed; the choice stays open. |
| A9 | §6.4 — **decided**: `mpm_system`, with a read-backed receipt. |
| A10 | §6.1 — **decided: deferred.** No new table; not the first milestone. |
| A11 | §3.7 / M5 — the M5 tension is ruled: the critic's existing automatic lesson-writing is a **pre-existing substrate behaviour** that this contract neither creates nor endorses. Amending it would require a schema change on `lessons_base`, which this sprint does not take. |
| A12 | §3.7 / M7 — the host-status → `ToolOutcomeClass` mapping is **pinned**. |
| A13 | §3.3 / M3 — a host project id is adopted only when a git remote independently confirms it (`host-git-remote`). |
