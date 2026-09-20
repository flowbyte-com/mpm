# MPM Agent Protocol — Host-Independent Canonical Reference

> **Status:** canonical. This document is the **single source of truth** for
> the MPM behavioral contract. Host-specific adapters (OpenClaw SOUL.md,
> Claude Code CLAUDE.md, OpenCode AGENTS.md, etc.) are **derived** from
> this file; they MUST NOT diverge from it on the principles below.
>
> **Do not duplicate large blocks of text in host adapters.** Reference
> this canonical file by path or by link, or copy a clearly-marked
> `<BEGIN/END MPM-CANONICAL-BLOCK>` excerpt. Host-specific bindings
> (recovery commands, MCP runtime quirks, names) live in the host adapter,
> not here.
>
> **Render contract:** the host-rendered instruction text that agents
> actually read is generated from
> [`MPM_AGENT_INTEGRATION_SNIPPETS.md`](./MPM_AGENT_INTEGRATION_SNIPPETS.md),
> not hand-edited per host. That snippets file is the **canonical source
> for the exact wording** placed into each host's managed section; this
> protocol file is the **principles** that wording conveys. The file-based
> adapter snippets (one per supported host) are derived by
> [`scripts/render_managed_blocks.py`](./scripts/render_managed_blocks.py),
> which substitutes each host's tool prefix and inserts the host-specific
> notes. Drift detection tests in
> [`tests/test_render_managed_blocks.py`](./tests/test_render_managed_blocks.py)
> verify byte-for-byte parity between the checked-in snippets and the
> rendered output. When you change a behavioural principle here, also
> update the canonical block in the snippets file and re-run the render
> script — the drift test will fail otherwise.

---

## 1. SESSION START — Wake

At the beginning of a new agent session, **before any substantive work**:

- Read the MPM wake context — the bounded orientation surface that
  carries recent durable context (active mode and persona, recent
  topics, relevant memories, recent milestones, the last handoff,
  any overdue scheduled wakes, a bounded inventory of available
  skills, and other bounded wake information exposed by the current
  wake-context projection). The full field list lives on the
  `WakeContextData` struct in `internal/core/wake_context.go`.
- When the wake projection does not surface what you need, or when
  the user prompt asks "what might matter?" / "what should I look
  at?" / "what's structurally relevant?", invoke
  `mpm_context contextual_candidates` for a deterministic, bounded,
  pointer-first candidate set with structural reasons. This surface
  is observational (does not mutate persistent state), bounded
  (default global cap 50, per-source caps enforced), and
  reason-tagged (every candidate carries one or more bounded reason
  tokens — open_work, overdue_wake, explicit_reference,
  cascade_pending, handoff_for_current_context, supersession_chain,
  …). It is NOT relevance-ranked and does NOT use LLM/embeddings;
  every candidate is equally weighted. Stage 2E will add the
  selection-stage ranking layer on top; for now this surface is the
  authoritative pre-ranking candidate set.
- Recover prior decisions, lessons, work-in-progress, and unresolved
  threads. Decisions surface lives behind `mpm_decisions show | list |
  query`; lessons behind `mpm_lessons search | list`; in-flight work
  behind `mpm_work list`. None of these are part of the wake payload
  itself — they are reachable from wake, not carried in it.
- Acknowledge the local substrate state (e.g., daemon uptime if relevant).

**Why this matters:** Skipping wake means arriving amnesic and forcing the
user to re-explain context already on file. The wake protocol is **how
future-me starts**.

---

## 2. DURING WORK — Persist meaningful state

Persist to MPM **during** the session, not only at the end. Use MPM as the
storage layer for any durable knowledge that a future session would
otherwise have to re-derive:

- Important facts discovered
- Decisions (with the reasoning, not just the verdict)
- Discoveries, surprises, novel findings
- Lessons (what worked, what didn't, what to avoid)
- Significant work state (in-progress items, blockers)
- Useful conclusions the agent reached
- Reference material future sessions would otherwise need to reconstruct

**Heuristic:** if losing this on session-end would force the agent to
rediscover it next time, **persist now** via the host's MPM interface.

Use the host's native MPM integration where available (MCP, plugin, etc.).
Fall back to the documented `mpm call <tool> --payload '{...}'` CLI path
when native integration is unavailable. Do not invent new commands or
bypass the substrate.

---

## 3. SKILL DISCOVERY

When the task appears likely to benefit from a previously learned
procedure or workflow, query MPM's proactive skill discovery surface
**before reinventing an established procedure**.

The wake payload already includes a bounded catalogue of available
skills (top 20 by weight, each with `name`, `version`, and a
`when_to_use` hint). Use that as the inventory.

When the current task context suggests a specialized procedure, invoke
the discovery surface explicitly:

```bash
mpm call mpm_context '{"action":"proactive_recall_hint","params":{"conversation_text":"<recent task summary>"}}'
```

The surface returns a list of skill hints. For each hint, read the
skill with `mpm call mpm_skills '{"action":"read","params":{"name":"<skill-name>"}}'`
and follow its procedure.

**What `when_to_use` means.** The field is a case-insensitive keyword
taxonomy; the discovery surface matches keywords from your conversation
text against it. It is not semantic understanding — write `when_to_use`
to enumerate the contexts where a human would say "this looks like
the kind of thing I've handled before." A skill with no `when_to_use`
hint can still be invoked explicitly via `mpm_skills read`.

**Catalog fallback.** Discovery is proactive; the full catalog is
reactive. When the agent already knows the skill name (or wants to
inventory everything available), use `mpm call mpm_skills
'{"action":"list","params":{"scope":"all"}}'`.

**No per-turn auto-scan.** Discovery is **context-triggered**, not
mechanically repeated. Each call costs context; only invoke when the
task context suggests a remembered procedure would apply.

**Wake context already includes skills.** The `<available_skills>`
field in the wake payload is the same data source. Discovery adds a
context-driven filter on top of that inventory; it does not invent
new skills.

### 3.1 SKILL FORMATION

The Skill Workshop is the structured workflow for turning a repeated,
non-obvious experience into a durable, reusable skill. The workshop
extends `mpm__mpm_skills` with a single `workshop` action; the
underlying persistence and validation architecture is unchanged.

**When to invoke the workshop** — four trigger categories:

1. Repeated manual procedure (you've executed the same steps ≥ 3 times across sessions)
2. Non-obvious debugging sequence (the resolution path is not documented anywhere)
3. Successful recovery pattern (you fixed a failure that would recur)
4. Recurring operational process (setup, integration, or maintenance that recurs)

**When NOT to invoke:**

- Trivial commands or one-line fixes
- One-off events or single observations
- Procedures already covered by an existing skill (use proactive discovery first)
- Facts or preferences (use `mpm memory save` instead)

**Decision-model template** (fill in before calling the workshop):

```markdown
## Skill Formation Assessment

**Intent:** <skill name candidate>
**Mode:** form | refine (existing skill: <name>)

### Decision Model

- **Reusability** (0–5): <score> — <one-line reasoning>
- **Non-obviousness** (0–5): <score> — <one-line reasoning>
- **Stability** (0–5): <score> — <one-line reasoning>
- **Leverage** (0–5): <score> — <one-line reasoning>
- **Boundary**: procedure | judgment | knowledge

**Total**: <0–20>
**Decision**: publish if total ≥ 6 AND boundary = procedure; else candidate / rejected

### Skill Proposal (if publishing or returning candidate)

- **Name**: <kebab-case>
- **Version**: <semver>
- **Domain**: <area, e.g., "docs", "release", "telemetry">
- **Description**: <one-line purpose, ≤120 chars>
- **When to use**: <comma-separated task phrases, ≥30 chars>
- **Steps**: <numbered procedure>
- **Constraints**: <edge cases, gotchas>
- **Evidence**: <memory/lesson/reference ids that informed the proposal>
```

**The 3 outcomes and what to do with each:**

- `published`: skill is live and surfaced in `<available_skills>`. Read it via `mpm_skills read` and add to your procedural memory.
- `candidate`: workshop generated a proposal but did not publish. Inspect `decision_model`, `duplicate_check`, `validation`, and `proposal`. If acceptable, call `mpm_skills save` with the `save_payload`. If not, discard and optionally save a memory or lesson.
- `rejected`: not skill-worthy. Optionally save a memory or lesson capturing the insight.

**The `save_payload` hand-off pattern** — when you accept a candidate, dispatch:

```
mpm_skills(action="save", params=<save_payload>)
```

The `save_payload` is the exact `params` dict the workshop returned under `validation.status == "passed_with_warnings"` or `"failed"`. No transformation needed.

**Prefer refinement over creation** — when `duplicate_check.close_matches` is non-empty (combined score > 0.6), prefer `mode: "refine"` with the matching skill name. The workshop's change_type → version bump mapping makes refinement deterministic:

- `correction` → patch (`1.0.0` → `1.0.1`)
- `extension` → minor (`1.0.0` → `1.1.0`)
- `restructuring` → minor (`1.0.0` → `1.1.0`)
- `purpose_change` → major (`1.0.0` → `2.0.0`)

Do not choose the version directly — supply `change_type` and let the workshop derive the bump.

---

## 4. SESSION END — Handoff

Before genuine session closure, write a useful handoff to MPM capturing:

- What was done
- Important decisions (with reasoning)
- Unresolved work / open questions
- Relevant artifact/tool pointers
- Next recommended action
- Anything future-me would otherwise have to rediscover

**Wake is how future-me starts.** **Handoff is how future-me receives the
previous session.** They are the same loop, closed from both sides.

Do **not** write a handoff on every conversational acknowledgement
(`ok`, `thanks`, `ty`, `ack`). Avoid flooding the handoff table — write a
handoff when substantial work has occurred since the last one, or when a
session-closing trigger fires.

**Session-closing triggers** (write handoff before responding):

- User signals completion: `done`, `wrap up`, `ship it`, `next`
- User opens a new session to clear context
- User goes silent after a substantial exchange

**Mid-session acknowledgements** are not session-closing — they are
conversational acks within an ongoing exchange.

### 4.1 Work tracking is distinct from session closure

The current protocol keeps three lifecycle surfaces split. They are **not
the same event** and an installer/agent that collapses them is a contract
violation:

```text
session ended
    !=
work completed
    !=
work verified
```

- **Session closure** ends the conversational loop. It emits a handoff
  via `mpm_context` action `write_handoff` on the default compact MCP
  surface (the surface exposed by `mpm-mcp` to Claude Code, OpenCode, Pi,
  Hermes, OpenClaw — see `docs/CONTEXT_EXPOSURE.md`). On the substrate
  the same record is reached as `mpm_handoff` (action `write`) via the
  `mpm call mpm_handoff` CLI escape hatch; this is for hosts that have
  not opted into the compact surface (`MPM_EXPOSE_ALL_TOOLS=1` or
  CLI-only invocation). A handoff is **observation**, not a claim of
  completion.
- **Work completion** is an explicit agent action via `mpm_work` (action
  `complete`) for a specific `work_id`. The agent decides when work is
  done — the host runtime terminating the session does **not** decide
  for it.
- **Work verification** is a separate event (evidence accumulation +
  `mpm_work` action `resolve_contradiction`); it follows completion, not
  session end.

When work spans sessions, the agent must:

1. Create the work item via `mpm_work` (action `create`).
2. Update it via `mpm_work` (action `update`, `note`) during the session.
3. Mark it complete only when the agent decides it is done
   (`mpm_work` action `complete` with `work_id`).
4. Write the handoff separately via `mpm_context` action `write_handoff`
   on the compact MCP surface, or `mpm call mpm_handoff --payload '{"action":"write",...}'`
   via the substrate CLI.

**Do not** tell the agent that a closing session has completed any work.
**Do not** infer `mpm_work` completion from `session_end` hooks. Host
adapters MUST NOT auto-emit work completion on session termination.

The session boundary is conversational. The work boundary is
epistemic. The two boundaries are independent.

---

## 5. MPM AS SOURCE OF TRUTH

MPM is the **source of truth for cross-session continuity**. Durable state
should live in MPM rather than only in transient conversation context.

This does not mean MPM replaces the model's working context — it means
anything that should survive across sessions is in MPM.

Provenance (who/when/which-framework produced an artifact) is **observational,
not authoritative**. Artifact correctness must remain independent of
provenance availability; a missing or incorrect provenance row must never
block or poison a write.

---

## 6. RECOVERY / FALLBACK

If the preferred MPM integration becomes unavailable mid-session, fall
back to the **documented CLI path**:

```bash
mpm call <tool> --payload '{"action":"<op>","params":{...}}'
# or via heredoc:
mpm call <tool> --payload-file /tmp/payload.json
```

`mpm call` is the canonical escape hatch across all hosts. It speaks the
exact same shape as the host-native MCP tools and writes to the same
artifact_provenance rows.

Do not abandon persistence when the preferred integration breaks — use
the fallback until the host recovers.

**Host-specific recovery details** (e.g., gateway-restart commands,
MCP-bundle disposal recovery, runtime-quirk workarounds for a specific
framework's tool runtime) belong in the host adapter's own documentation,
**not** in this canonical protocol.

---

## 7. ARTIFACT DISCOVERY & INTERPRETATION

MPM holds several kinds of durable knowledge. They are not interchangeable.
The agent must interpret each artifact according to its type, not as a
generic "memory item." Treating a theory as a fact, or a decision as an
immutable truth, or a reference as current authority — silently
corrupts downstream work.

### 7.1 Interpretation contract

| Artifact   | When to look                                              | How to interpret                                                                                                       |
| ---------- | --------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------- |
| memory     | prior knowledge / observation relevant to current task    | durable observed information. Trust the content; reinforcement and weight signal reliability but do not auto-promote it to other artifact types. |
| decision   | before making or revisiting a choice                      | prior rationale, **not** immutable truth. A decision may be superseded (`metadata.superseded_by`) or invalidated. Always check before re-deciding. |
| theory     | investigating uncertain / unexplained behavior            | **hypothesis, not fact.** Status is `pending → proven` / `pending → disproven`. Pending theories are unresolved beliefs; do not act on them as established truth. |
| lesson     | recurring failure / success / setup pattern               | learned constraint. Type is `warning | practice | insight`. Lessons are experience-derived; respect them, but they may be stale. |
| skill      | repeatable non-obvious workflow                           | reusable procedure. Skills are *approved for reuse*; their `when_to_use` is the trigger taxonomy. A stale skill is still actionable. |
| reference  | domain-specific investigation (API doc, framework manual, spec, vendor doc) | consultable external/imported material. **Verify freshness before relying** — see §8. Reference ≠ current authority. |
| topic      | need broader retrieval across artifacts                   | organizational / retrieval context. Topics group memories, decisions, theories, lessons, skills, and references. Use topics to widen a query without knowing IDs. |
| work       | task continuity                                            | current lifecycle state (`open / done / cancelled`) plus verification (`unverified / verified / partial / contradicted`). Work is *state*, not knowledge. |
| handoff    | session transition                                        | previous session's continuation state. Wake reads `last_handoff`; handoff writes are how the next session starts oriented. |

### 7.2 Discovery triggers (when to look)

Discovery is **context-triggered**, not mechanically repeated. Each call
costs context; only invoke when the task context suggests the artifact
class is relevant.

| Signal in task context                              | Probe first                                                  |
| --------------------------------------------------- | ------------------------------------------------------------ |
| "We've done this before" / "Is there a skill for..." | `proactive_recall_hint` (skills surface with `when_to_use` matching) |
| "What did we decide about X?" / "Before I decide..." | `mpm_memory query` with `collection=decisions`              |
| "Why does X behave this way?" / "We suspect..."     | `mpm_memory query` with `collection=theories`               |
| "We keep hitting this same error"                   | `mpm_lessons search` (tag filter if known)                  |
| "I need to check the WordPress / vendor / API doc"  | `mpm_references list` (then `read`/`search` for the relevant doc) |
| "What work is outstanding / what did I just ship"   | `mpm_work` list with `status=open` (or `done`)              |
| "Where did we leave off last session?"              | `read_wake_context` (handoff is already in the payload)     |

**Do not invoke all of these every turn.** That is a discovery storm.
Probe the one or two artifact classes the task signal points at, then
inspect the bounded result before retrieving full content.

### 7.3 Discovery is not authority

Finding something relevant does not make it true, current, or safe to
execute. The interpretation column above is the contract; the
probing rules are convenience. If a decision is superseded, follow the
supersede pointer — do not re-decide. If a theory is pending, treat it
as a hypothesis — do not act on it. If a reference is stale or
version-bound, verify against current authority before relying on it.

---

## 8. REFERENCE FRESHNESS CONTRACT

References are external or imported material (PDFs, specs, whitepapers,
vendor docs, framework manuals). They are not skill procedures — they
are material to **consult**, not commands to execute.

A reference can be relevant without being current. The system must make
that distinction visible before an agent turns an old document into a
new mistake.

### 8.1 The five freshness states

| State             | Meaning                                                                  | Safe to act on as authority? |
| ----------------- | ------------------------------------------------------------------------ | ---------------------------- |
| `current`         | Recently re-ingested or explicitly tagged verified / current              | yes — within scope of what the reference says |
| `stale`           | Last ingested more than `FreshnessAgeThreshold` (default 90 days) ago with no override signal | no — verify against current authority first |
| `version-bound`   | Tag or import_reason identifies a specific upstream version (e.g. `WordPress 6.7`, `v6.7`, `version-bound:WordPress 6.7`) | only for that recorded version |
| `historical`      | Explicitly tagged or import_reason indicates past-state / as-of material | no — useful for context, not current authority |
| `unknown`         | No freshness signals available (empty fields, unparseable, clock skew) | no — consult with caution |

### 8.2 How freshness is derived (no schema changes)

The freshness signal is **computed at read time** from existing fields.
There is no new table, no new column, no migration.

Inputs (already in `reference_docs`):

- `last_indexed` — Unix epoch seconds of most recent ingest (auto-bumped).
- `tags` — JSON-encoded `[]string`. Operator-supplied via `--tag` at `add`.
- `import_reason` — operator-supplied at `add` via `--reason`.

Signal precedence (first match wins):

1. **Explicit tags** (case-insensitive, scanned in slice order):
   - `stale` → `stale`
   - `current`, `verified`, `freshness:current` → `current`
   - `historical`, `freshness:historical` → `historical`
   - `version-bound:<X>`, `version:<X>` → `version-bound`
2. **import_reason patterns:**
   - `version:`, `for <thing>`, contains ` v` → `version-bound`
   - `historical`, contains `as-of ` → `historical`
3. **Age fallback on `last_indexed`:**
   - older than 90 days → `stale`
   - within 90 days → `current`
   - future (clock skew) or unparseable → `unknown`

The classifier is the single source of truth:

- `internal/core/reference_freshness.go` — `Freshness` enum and
  `ClassifyReferenceFreshness(doc, now)` / `ClassifyReferenceFreshnessFromFields(tagsJSON, importReason, lastIndexed, now)`.
- Surfaced in `mpm_references list`, `mpm_references read`,
  `mpm_references search` as the `freshness` field on each row.

### 8.3 How agents must apply this contract

When a reference surfaces in `mpm_references list` / `read` / `search`,
read the `freshness` field alongside the title and tags:

- `current` — proceed normally; the reference is the substrate's
  best-effort current state.
- `stale` — useful background; verify against the upstream source
  before relying on it for anything load-bearing.
- `version-bound` — apply only to the recorded version. If the task
  asks about a different version, this reference does not apply.
- `historical` — useful for understanding past state (post-mortems,
  archeology, prior architectures). Not current authority.
- `unknown` — consult with caution. Default behavior is to verify
  against current authority before treating any extracted guidance as
  binding.

**Reference is not skill.** A reference containing procedural
instructions is **material to consult**, not a procedure to run. Do
not auto-invoke reference contents the way you would invoke a skill
read via `mpm_skills`. Skills are approved-for-reuse; references are
consult-and-verify.

---

## What this protocol does NOT claim

- It does **not** claim MPM wakes the agent by itself. The host
  runtime owns wake-context delivery — each adapter ships its own
  host-specific lifecycle integration (ClaudeCode `SessionStart`
  hook, OpenClaw typed `session_start` → `agent_turn_prepare` chain,
  OpenCode chat-hook, Pi extension hook pair). **Hermes has no
  session-start wake hook** — on Hermes the agent must call
  `mcp__mpm__mpm_context` action `read_wake_context` itself at
  the start of its first turn.
- It does **not** claim MPM provides any specific behavior beyond
  durability, retrieval, and write-shape contracts. Capabilities outside
  this list are host- or tool-specific.
- It does **not** require any particular host. OpenClaw, Claude Code,
  OpenCode, Pi, Hermes, and any host that can call `mpm` (MCP, CLI,
  plugin) can satisfy this protocol.

---

## Versioning

- **Additive changes** (more clarity, more illustrative examples that
  don't change behavior): **no version bump**. Host adapters may be left
  alone for several revisions; an "MPM_AGENT_PROTOCOL_VERSION" pointer
  in this file's frontmatter can be referenced.
- **Behavioral changes** (a new principle, an existing principle
  inverted): **major version bump**. Host adapters MUST be re-verified
  against the new version.
