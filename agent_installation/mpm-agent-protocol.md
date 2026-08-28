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

---

## 1. SESSION START — Wake

At the beginning of a new agent session, **before any substantive work**:

- Read the MPM wake context (mode, persona, topics, recent memories,
  open work, last handoff, key decisions, active lessons, key pointers).
- Recover prior decisions, lessons, work-in-progress, and unresolved threads.
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

- **Reusability** (0–2): <score> — <one-line reasoning>
- **Non-obviousness** (0–2): <score> — <one-line reasoning>
- **Stability** (0–2): <score> — <one-line reasoning>
- **Leverage** (0–2): <score> — <one-line reasoning>
- **Boundary**: procedure | fact | preference | one_off

**Total**: <0–8>
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

## What this protocol does NOT claim

- It does **not** claim MPM wakes the agent by itself. The host runtime
  must wire wake-context delivery (SessionStart hook, system-prompt
  injection, plugin startup handler, etc.).
- It does **not** claim MPM provides any specific behavior beyond
  durability, retrieval, and write-shape contracts. Capabilities outside
  this list are host- or tool-specific.
- It does **not** require any particular host. OpenClaw, Claude Code,
  OpenCode, and any host that can call `mpm` (MCP, CLI, plugin) can
  satisfy this protocol.

---

## Versioning

- **Additive changes** (more clarity, more illustrative examples that
  don't change behavior): **no version bump**. Host adapters may be left
  alone for several revisions; an "MPM_AGENT_PROTOCOL_VERSION" pointer
  in this file's frontmatter can be referenced.
- **Behavioral changes** (a new principle, an existing principle
  inverted): **major version bump**. Host adapters MUST be re-verified
  against the new version.
