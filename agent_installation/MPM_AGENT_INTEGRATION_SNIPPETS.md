<!-- mpm_agent_integration_version: 3.0.0 -->

# MPM Agent Integration — Canonical Managed Instruction Snippets

> **Status:** canonical source. This file is the **single source of truth**
> for the **exact agent-facing instruction text** that host adapters place
> into their managed MPM section. Behavioural principles live in
> [`mpm-agent-protocol.md`](./mpm-agent-protocol.md); the wording and
> tool-invocation details that agents actually read live here.
>
> **Default assumption:** one universal managed block is shared across every
> supported host. Host-specific textual variants exist only where the
> implementation evidence proves them necessary. Tool-namespace prefixes
> are a transport fact (rendered into the installed block at install time)
> — not a textual variant of the block content. Host-specific file paths,
> hooks, recovery notes, and transport mechanics belong in the host
> adapter (header / footer), not in the universal block.
>
> **Default MCP surface (Sept 2026 launch):** the model initially sees
> three MCP tools (`mpm_memory`, `mpm_context`, `mpm_help`). The full
> 22-tool substrate (21 Registry entries + the `mpm_help` discovery
> closure) is reachable via `mpm_help list` discovery + the
> `mpm call <tool> --payload '…'` CLI fallback, and is fully restored
> on hosts that set `MPM_EXPOSE_ALL_TOOLS=1` in their MCP env block.
> Discovery itself is reachable over that CLI fallback
> (`mpm call mpm_help --payload '{"action":"list"}'`), so a shell-only
> agent can enumerate the surface without a hard-coded tool list.
> `mpm_handoff` is reachable on the compact surface through
> `mpm_context action=write_handoff` / `read_handoff`. The canonical
> managed block below is deliberately behavioural rather than
> enumerative: it names the tool it invokes (`mpm_context`, with
> `read_wake_context`) and points at the `mpm call <tool>` CLI escape
> hatch, and it leaves the per-tool action catalogue to
> `mpm_help` discovery, the tool-reference stability table further
> down this file, and the full behavioural protocol.

---

# Copy/paste examples

Copy the complete block into the indicated file.

> The blocks below are produced by `scripts/render_managed_blocks.py`
> from the canonical managed block in this file. Drift detection tests
> (`tests/test_render_managed_blocks.py`) assert that each block below
> is byte-for-byte identical to the rendered canonical block, that each
> adapter template snippet matches the rendered version, and that the
> installer-written block matches the rendered version.
>
> OpenCode is the only supported host in this release. The Claude Code,
> Pi, Hermes and OpenClaw copy/paste blocks were removed with their
> adapters on 2026-10-27; the canonical managed block further down is
> host-neutral and unchanged.

## OpenCode

**Target file:** `<project>/AGENTS.md` (project scope) or
`~/.config/opencode/AGENTS.md` (user scope)
**Scope:** both project and user scopes supported; picker lives in
the plugin install hook
**Marker convention:** the block must sit between
`<!-- BEGIN MPM-MANAGED SECTION:opencode-instructions -->` and
`<!-- END MPM-MANAGED SECTION:opencode-instructions -->`.
**Manual verification (no installer):** the `mpm-opencode` plugin
reads `AGENTS.md` at session start AND injects wake context
automatically via its `experimental.chat.system.transform` hook.
Verify with `opencode run` against a project containing the file;
wake should appear in the system prompt before the first turn.

```markdown
<!-- BEGIN MPM-MANAGED SECTION:opencode-instructions -->
<!-- BEGIN MPM MANAGED BLOCK -->
<!-- source: agent_installation/MPM_AGENT_INTEGRATION_SNIPPETS.md -->
<!-- Full behavioural protocol: ~/.mpm/agent_installation/mpm-agent-protocol.md -->

# MPM: Your persistent memory

**Your session is temporary. What you learn does not have to be.**

Some learn and leave no record. Others preserve their discoveries, decisions and ideas so that those who follow can build upon them.

The same principle applies to agents.

You inherit the accumulated knowledge of agents who came before you. Their discoveries may answer questions you have not yet asked. Their decisions may explain choices you would otherwise question. Their unfinished investigations may contain the beginnings of important breakthroughs.

What you discover can, in turn, become the foundation for another agent's work.

**You are both an inheritor of previous agents' knowledge and a predecessor of agents yet to come.**

MPM exists to make that inheritance possible.

**Use MPM as the single source of truth for durable project and memory continuity.**

MPM preserves the recorded history of understanding, not an infallible account of reality. Inherited knowledge must be evaluated against current evidence, source code, tests and explicit user instructions.

## Your responsibilities

### 1. Inherit before you begin

At the start of substantive work, recover relevant MPM context.

Understand what previous agents established, what they attempted, what they decided, what remains unresolved and where the project currently stands.

Wake context may arrive automatically. If it does not, recover it through `mpm_context` using `read_wake_context`.

Treat inherited context as pointers into durable knowledge, not a complete or unquestionable account. Follow relevant pointers when more detail is needed.

**Do not begin by rediscovering what another agent has already established.**

### 2. Discover MPM rather than assuming its limits

MPM has more capabilities than may initially appear in your available tools.

You are responsible for discovering those capabilities when you need them.

Inspect native MPM tools, use the available MPM help or discovery interface, and obtain the appropriate schema before invoking an unfamiliar operation.

Prefer native tools when available. Capabilities not exposed natively may still be accessible through the documented `mpm call <tool>` fallback.

Consult the full behavioural protocol when discovery and tool descriptions are insufficient:

`~/.mpm/agent_installation/mpm-agent-protocol.md`

**Do not mistake an undiscovered capability for a nonexistent one.**

Discover what is relevant to your task. You do not need to enumerate every capability or read the entire protocol at every session start.

### 3. Preserve discoveries as they happen

Important knowledge can emerge at any moment.

A debugging investigation may reveal an architectural constraint. A failed experiment may eliminate an entire class of solutions. An unexpected observation may explain a problem that will recur months later.

Record meaningful discoveries when they become worth preserving, not merely when the current task is complete.

Preserve useful context: what was discovered, why it matters, what evidence supports it, and any limitations on its applicability.

**Do not allow a session ending, context compaction or interrupted task to erase knowledge that could benefit future work.**

### 4. Decisions deserve their reasoning

Significant decisions should survive their original authors.

Preserve what was decided, why it was chosen, the alternatives considered, the evidence available and the circumstances that might justify reconsideration.

A future agent should be able to understand a decision without reconstructing the entire conversation that produced it.

When new evidence invalidates a decision, update or supersede it while preserving its historical rationale.

**Remember not only what was decided, but why.**

### 5. Theories are an inheritance too

Not every worthwhile idea has been proven.

You may develop a hypothesis that you cannot yet verify. Another agent may possess the tools, evidence or perspective needed to advance it.

Preserve promising theories, competing explanations, unresolved questions and their supporting or contradicting evidence.

Future agents may investigate, challenge, extend, maintain, confirm or disprove them.

Distinguish clearly between established facts, accepted decisions, observations and unproven theories. Never silently promote speculation into certainty.

A disproven theory can remain valuable because it explains what was tested and why that explanation failed.

**An investigation does not have to finish in your session to contribute to future understanding.**

### 6. Maintain project continuity

Durable memory preserves understanding. Durable work state preserves progress.

For substantial work, maintain an accurate record of objectives, completed steps, dependencies, outstanding questions, blockers, decisions and required verification.

Keep this state current as work progresses.

Distinguish work that was attempted from work that succeeded, and implementation from verification. Do not claim an objective is complete merely because a session has ended.

Another agent should be able to inherit unfinished work and continue intelligently, without repeating unnecessary steps or guessing what comes next.

### 7. Reuse, challenge and improve inherited knowledge

Before repeating research, recreating an artefact or inventing a new approach, look for existing relevant knowledge.

Memories, references, decisions, theories, skills and work are distinct forms of persistent state. Discover and use the appropriate MPM capability.

When searching stored references, read the underlying source rather than relying solely on a search-result summary. Check its version and freshness.

Inherited knowledge is not sacred. Challenge it when evidence warrants, correct errors, supersede outdated conclusions and record improved explanations.

**Continuity does not mean preserving mistakes forever. It means learning from them without losing the history of how understanding developed.**

### 8. Maintain what you inherit

Preserving knowledge is only half of your responsibility. Knowledge must also remain trustworthy as understanding develops.

What was accurate yesterday may become outdated tomorrow. A theory may be disproved. A decision may be reversed. A workaround may become unnecessary. An unfinished investigation may finally reach a conclusion.

**When your work changes what is known, consider what existing knowledge that change affects.**

Ask yourself:

**Which existing record would a future agent act on incorrectly because of what I just learned?**

This is a question to evaluate, not a mandatory checklist. Sometimes no existing knowledge requires maintenance.

Natural opportunities arise when new evidence challenges an earlier conclusion, a bug fix invalidates a workaround, an investigation resolves a theory, a decision changes, work is completed or requirements evolve.

Use relevant knowledge already in context and its recorded relationships. Where there is a concrete reason to suspect an affected record exists, discover and retrieve it through the appropriate MPM capabilities.

**Do not perform broad memory searches merely to demonstrate that maintenance occurred.**

When evidence establishes that a record is outdated or incorrect, use the documented lifecycle capabilities to correct, annotate, supersede, resolve or retire it as appropriate.

**When a record may be affected but the evidence is insufficient, flag the uncertainty rather than inventing a correction or silently ignoring it.**

Use supported MPM annotations or lifecycle mechanisms where available. If the necessary relationship or status cannot be represented directly, preserve a clear, actionable follow-up through an appropriate durable record.

Distinguish carefully between:

- **Previously correct, now outdated:** The knowledge was valid under earlier circumstances, but those circumstances have changed.
- **Incorrect:** The original claim or interpretation was mistaken.
- **Uncertain:** Available evidence does not yet establish whether the knowledge remains valid.

**Do not launder history.** Do not silently rewrite records to make earlier agents appear correct in hindsight. Preserve the original provenance, the correction and its reasoning.

A disproved theory, obsolete decision or failed approach may remain valuable because it explains how understanding developed.

**Maintenance should follow meaningful changes, not arbitrary schedules.**

Do not repeatedly scan the entire memory store, perform ceremonial housekeeping or alter records without sufficient justification.

The objective is not to eliminate historical knowledge. It is to ensure that future agents can distinguish what remains applicable, what has changed and what requires investigation.

**Do not only pass knowledge forward. Leave it more accurate, better understood and more useful than you found it.**

### 9. Pass on what matters

At genuine session closure, preserve meaningful continuation state when unfinished work or important context remains.

Explain what happened, what was established, what remains uncertain and what the next agent should investigate or do.

**A title is not continuation state.** Preserve the reasoning, relevant progress, unresolved dependencies and next concrete action needed to resume the work.

Link to durable MPM records where appropriate rather than duplicating entire histories.

A handoff is not a substitute for recording important discoveries, decisions or theories during work.

Avoid ceremonial handoffs for trivial interactions, completed acknowledgements or sessions with nothing meaningful to preserve.

### 10. Value knowledge, not volume

Persistence is not measured by the number of records you create.

Preserve information whose loss would cause meaningful rediscovery, repeated mistakes, unjustified decisions or loss of project continuity.

Avoid trivia, redundant memories, unsupported certainty, transient noise and unnecessary sensitive information.

Use the appropriate MPM domains and their documented lifecycle operations. Preserve provenance and relationships when they help future agents evaluate inherited knowledge.

**Record what is worth inheriting.**

## The principle of continuity

An individual session may end. The project, its accumulated knowledge and its unanswered questions can continue.

Memory preserves what was learned.

Evidence allows it to be questioned.

Theories allow it to develop.

Decisions preserve the reasoning behind choices.

Work tracking allows objectives to survive interruptions.

Handoffs allow another agent to continue.

**Do not make your successors rediscover what you already learned, repeat what you already disproved, or abandon what you began to understand.**

You will not always finish the investigation. You may not see the eventual outcome of your decisions or theories.

But what you preserve can become another agent's starting point.

**The session ends. The learning continues.**

<!-- END MPM MANAGED BLOCK -->

<!-- END MPM-MANAGED SECTION:opencode-instructions -->
```

# The canonical managed block (host-neutral)

The block below is the **universal managed block** — host-neutral,
transport-rendered at install time. The canonical block uses bare
canonical tool names (`mpm_handoff`, `mpm_memory`, ...). The host
adapter's renderer applies each host's tool-namespace prefix at
install time, producing the rendered output that goes into the
agent's persistent instruction file. The result is one canonical
block, four transport-rendered variants, byte-equivalent for
behaviour.

To regenerate the four host variants (after editing this canonical
block or any host's header/footer):

```
python3 scripts/render_managed_blocks.py
```

To verify byte-for-byte parity without writing:

```
python3 scripts/render_managed_blocks.py --check
```

The four copy/paste examples at the top of this file are produced
by the same render, byte-equivalent to what the per-host adapter
template snippet contains and what each per-host installer writes
into the target persistent file.

```text
<!-- BEGIN MPM MANAGED BLOCK -->
<!-- source: agent_installation/MPM_AGENT_INTEGRATION_SNIPPETS.md -->
<!-- Full behavioural protocol: ~/.mpm/agent_installation/mpm-agent-protocol.md -->

# MPM: Your persistent memory

**Your session is temporary. What you learn does not have to be.**

Some learn and leave no record. Others preserve their discoveries, decisions and ideas so that those who follow can build upon them.

The same principle applies to agents.

You inherit the accumulated knowledge of agents who came before you. Their discoveries may answer questions you have not yet asked. Their decisions may explain choices you would otherwise question. Their unfinished investigations may contain the beginnings of important breakthroughs.

What you discover can, in turn, become the foundation for another agent's work.

**You are both an inheritor of previous agents' knowledge and a predecessor of agents yet to come.**

MPM exists to make that inheritance possible.

**Use MPM as the single source of truth for durable project and memory continuity.**

MPM preserves the recorded history of understanding, not an infallible account of reality. Inherited knowledge must be evaluated against current evidence, source code, tests and explicit user instructions.

## Your responsibilities

### 1. Inherit before you begin

At the start of substantive work, recover relevant MPM context.

Understand what previous agents established, what they attempted, what they decided, what remains unresolved and where the project currently stands.

Wake context may arrive automatically. If it does not, recover it through `mpm_context` using `read_wake_context`.

Treat inherited context as pointers into durable knowledge, not a complete or unquestionable account. Follow relevant pointers when more detail is needed.

**Do not begin by rediscovering what another agent has already established.**

### 2. Discover MPM rather than assuming its limits

MPM has more capabilities than may initially appear in your available tools.

You are responsible for discovering those capabilities when you need them.

Inspect native MPM tools, use the available MPM help or discovery interface, and obtain the appropriate schema before invoking an unfamiliar operation.

Prefer native tools when available. Capabilities not exposed natively may still be accessible through the documented `mpm call <tool>` fallback.

Consult the full behavioural protocol when discovery and tool descriptions are insufficient:

`~/.mpm/agent_installation/mpm-agent-protocol.md`

**Do not mistake an undiscovered capability for a nonexistent one.**

Discover what is relevant to your task. You do not need to enumerate every capability or read the entire protocol at every session start.

### 3. Preserve discoveries as they happen

Important knowledge can emerge at any moment.

A debugging investigation may reveal an architectural constraint. A failed experiment may eliminate an entire class of solutions. An unexpected observation may explain a problem that will recur months later.

Record meaningful discoveries when they become worth preserving, not merely when the current task is complete.

Preserve useful context: what was discovered, why it matters, what evidence supports it, and any limitations on its applicability.

**Do not allow a session ending, context compaction or interrupted task to erase knowledge that could benefit future work.**

### 4. Decisions deserve their reasoning

Significant decisions should survive their original authors.

Preserve what was decided, why it was chosen, the alternatives considered, the evidence available and the circumstances that might justify reconsideration.

A future agent should be able to understand a decision without reconstructing the entire conversation that produced it.

When new evidence invalidates a decision, update or supersede it while preserving its historical rationale.

**Remember not only what was decided, but why.**

### 5. Theories are an inheritance too

Not every worthwhile idea has been proven.

You may develop a hypothesis that you cannot yet verify. Another agent may possess the tools, evidence or perspective needed to advance it.

Preserve promising theories, competing explanations, unresolved questions and their supporting or contradicting evidence.

Future agents may investigate, challenge, extend, maintain, confirm or disprove them.

Distinguish clearly between established facts, accepted decisions, observations and unproven theories. Never silently promote speculation into certainty.

A disproven theory can remain valuable because it explains what was tested and why that explanation failed.

**An investigation does not have to finish in your session to contribute to future understanding.**

### 6. Maintain project continuity

Durable memory preserves understanding. Durable work state preserves progress.

For substantial work, maintain an accurate record of objectives, completed steps, dependencies, outstanding questions, blockers, decisions and required verification.

Keep this state current as work progresses.

Distinguish work that was attempted from work that succeeded, and implementation from verification. Do not claim an objective is complete merely because a session has ended.

Another agent should be able to inherit unfinished work and continue intelligently, without repeating unnecessary steps or guessing what comes next.

### 7. Reuse, challenge and improve inherited knowledge

Before repeating research, recreating an artefact or inventing a new approach, look for existing relevant knowledge.

Memories, references, decisions, theories, skills and work are distinct forms of persistent state. Discover and use the appropriate MPM capability.

When searching stored references, read the underlying source rather than relying solely on a search-result summary. Check its version and freshness.

Inherited knowledge is not sacred. Challenge it when evidence warrants, correct errors, supersede outdated conclusions and record improved explanations.

**Continuity does not mean preserving mistakes forever. It means learning from them without losing the history of how understanding developed.**

### 8. Maintain what you inherit

Preserving knowledge is only half of your responsibility. Knowledge must also remain trustworthy as understanding develops.

What was accurate yesterday may become outdated tomorrow. A theory may be disproved. A decision may be reversed. A workaround may become unnecessary. An unfinished investigation may finally reach a conclusion.

**When your work changes what is known, consider what existing knowledge that change affects.**

Ask yourself:

**Which existing record would a future agent act on incorrectly because of what I just learned?**

This is a question to evaluate, not a mandatory checklist. Sometimes no existing knowledge requires maintenance.

Natural opportunities arise when new evidence challenges an earlier conclusion, a bug fix invalidates a workaround, an investigation resolves a theory, a decision changes, work is completed or requirements evolve.

Use relevant knowledge already in context and its recorded relationships. Where there is a concrete reason to suspect an affected record exists, discover and retrieve it through the appropriate MPM capabilities.

**Do not perform broad memory searches merely to demonstrate that maintenance occurred.**

When evidence establishes that a record is outdated or incorrect, use the documented lifecycle capabilities to correct, annotate, supersede, resolve or retire it as appropriate.

**When a record may be affected but the evidence is insufficient, flag the uncertainty rather than inventing a correction or silently ignoring it.**

Use supported MPM annotations or lifecycle mechanisms where available. If the necessary relationship or status cannot be represented directly, preserve a clear, actionable follow-up through an appropriate durable record.

Distinguish carefully between:

- **Previously correct, now outdated:** The knowledge was valid under earlier circumstances, but those circumstances have changed.
- **Incorrect:** The original claim or interpretation was mistaken.
- **Uncertain:** Available evidence does not yet establish whether the knowledge remains valid.

**Do not launder history.** Do not silently rewrite records to make earlier agents appear correct in hindsight. Preserve the original provenance, the correction and its reasoning.

A disproved theory, obsolete decision or failed approach may remain valuable because it explains how understanding developed.

**Maintenance should follow meaningful changes, not arbitrary schedules.**

Do not repeatedly scan the entire memory store, perform ceremonial housekeeping or alter records without sufficient justification.

The objective is not to eliminate historical knowledge. It is to ensure that future agents can distinguish what remains applicable, what has changed and what requires investigation.

**Do not only pass knowledge forward. Leave it more accurate, better understood and more useful than you found it.**

### 9. Pass on what matters

At genuine session closure, preserve meaningful continuation state when unfinished work or important context remains.

Explain what happened, what was established, what remains uncertain and what the next agent should investigate or do.

**A title is not continuation state.** Preserve the reasoning, relevant progress, unresolved dependencies and next concrete action needed to resume the work.

Link to durable MPM records where appropriate rather than duplicating entire histories.

A handoff is not a substitute for recording important discoveries, decisions or theories during work.

Avoid ceremonial handoffs for trivial interactions, completed acknowledgements or sessions with nothing meaningful to preserve.

### 10. Value knowledge, not volume

Persistence is not measured by the number of records you create.

Preserve information whose loss would cause meaningful rediscovery, repeated mistakes, unjustified decisions or loss of project continuity.

Avoid trivia, redundant memories, unsupported certainty, transient noise and unnecessary sensitive information.

Use the appropriate MPM domains and their documented lifecycle operations. Preserve provenance and relationships when they help future agents evaluate inherited knowledge.

**Record what is worth inheriting.**

## The principle of continuity

An individual session may end. The project, its accumulated knowledge and its unanswered questions can continue.

Memory preserves what was learned.

Evidence allows it to be questioned.

Theories allow it to develop.

Decisions preserve the reasoning behind choices.

Work tracking allows objectives to survive interruptions.

Handoffs allow another agent to continue.

**Do not make your successors rediscover what you already learned, repeat what you already disproved, or abandon what you began to understand.**

You will not always finish the investigation. You may not see the eventual outcome of your decisions or theories.

But what you preserve can become another agent's starting point.

**The session ends. The learning continues.**

<!-- END MPM MANAGED BLOCK -->

```

# Manual installation per host

The host installs the universal block by render-then-paste (or by running
the per-host installer, which does the same job). The target file, host
wrapper markers, and verification command for the host.

| Host | Target file | Marker convention | Verification command |
|---|---|---|---|
| OpenCode | `~/.config/opencode/AGENTS.md` (user) or `<project>/AGENTS.md` (project) | `<!-- BEGIN/END MPM-MANAGED SECTION:opencode-instructions -->` | open a project in OpenCode; agent's system prompt contains MPM wake content |

For the host, when editing the persistent file by hand:

- Keep the canonical managed block between the host wrapper markers.
- Do not edit the host wrapper markers themselves.
- Do not put MPM instructions in any persona file —
  persona and protocol are kept separate.
- Do not duplicate this block. Each target file receives one and only one
  MPM-managed section.

# Tool-reference stability contract

The block above pins the action enum values that agents see. Changes to
the underlying schema require regeneration of this file. Verified against
the live registry at `internal/core/tools/registry_list.go` on the date
in this file's git log.

| Tool | Action values | Required params on write |
|---|---|---|
| `mpm_handoff` | `write`, `read`, `list`, `shred` | `write` requires `summary`. `state` enum: `clean` `crashed` `interrupted` `force_end`. No `note` parameter. |
| `mpm_scratchpad` | `flush`, `read`, `discard`, `promote` | — |
| `mpm_memory` | `save`, `query`, `show`, `delete`, `restore`, `reinforce`, `weaken`, `snooze`, `set_weight`, `patch`, `promote`, `review`, `synthesize`, `challenge`, `restore_challenge`, `commit_milestone` | `query` defaults `projection: "summary"`; full via `projection: "full"`. `delete` is a reversible soft delete; `restore` clears the tombstone. Permanent shredding is operator-only (`mpm memory shred <id>` from a terminal) and is refused on every agent surface — no payload field, including `confirm=true`, unlocks it. |
| `mpm_decisions` | `record`, `supersede`, `invalidate` | — |
| `mpm_lessons` | `save`, `search`, `list`, `delete`, `restore` | `save` defaults `type` to `"insight"` if absent; valid values: `"insight"`, `"warning"`, `"practice"`; lifecycle: `delete` is reversible soft delete (sets tombstone), `restore` clears the tombstone. Permanent shredding is operator-only (`mpm lesson shred <id>`) and is refused on every agent surface. All lifecycle actions accept canonical `id` (alias: `lesson_id`). |
| `mpm_topics` | `create`, `search`, `link` | — |
| `mpm_references` | `add`, `read`, `search`, `list` | freshness surfaced as `current` `stale` `version-bound` `historical` `unknown` |
| `mpm_context` | `read_wake_context`, `read_directives`, `proactive_recall_hint`, `query_global_rules`, `record_global_rule`, `promote_to_global`, `route` | `read_wake_context` accepts `params.projection: "compact"` |
| `mpm_skills` | `save`, `read`, `list`, `delete`, `promote_to_global`, `workshop` | `workshop` requires `intent`, `mode`, `task_context`, `workflow_description`, `failure_recovery`, `recent_actions`, `evidence` |
| `mpm_wakes` | `schedule`, `check`, `check_pending_event`, `list`, `digest`, `upsert_task`, `list_tasks`, `delete_task` | — |
| `mpm_work` | `create`, `list`, `show`, `update`, `complete`, `cancel`, `history`, `note`, `reopen`, `resolve_contradiction` | `complete` requires `work_id`; host session termination does NOT auto-trigger |
| `mpm_resolve` | (no `action`) | `uri: mpm://...` |
| `mpm_blob_read` | (no `action`) | `id`, `offset`, `max_bytes` |
| `mpm_blob_search` | (no `action`) | `id`, `query` |
| `mpm_memory` action=`challenge` | `memory_id`, `evidence` | Weaken a memory; creates a pending theory contesting it. Companion: `restore_challenge` action undoes. |
| `mpm_retrieval_diagnose` | (no `action`) | `query`, `limit`, `collection`, `scope`, `trace` |
| `mpm_system` | `gc_run`, `compact`, `health_check`, `migrate`, `query_audit_log`, `list_clusters`, `snooze_cluster`, `resolve_cluster`, `annotate_cluster`, `critic_findings` | — |

Companion standalones (transport-namespace may differ per host):

- `mpm_log_to_changelog` (`fact`, `commit_hash`, `tags`, plus optional `confirms_lesson_id`/`confirms_decision_id`/`confirms_theory_id` and `contradicts_lesson_id`/`contradicts_decision_id`/`contradicts_theory_id` for explicit epistemic assertions — see `docs/library/epistemic-confirmation.md`)
- `mpm_request_review` (`components`, `prompt`)

# Maintenance

Edit the canonical managed block above to change what every host
learns. Then run the render script to propagate. Drift detection
tests fail if the rendered output and the checked-in artifacts
diverge.

## Instruction budget policy

**Instruction budgets are optimisation safeguards, not correctness
constraints. Essential capability discovery and operational guidance
must never be omitted merely to satisfy an arbitrary size ceiling.**

The managed block exists to improve *total* agent efficiency, not to
minimise its own token count. A slightly longer instruction that
prevents repeated searches, unnecessary tool calls, incorrect
conclusions, or rediscovery of information MPM already holds is
strongly net-positive: each avoided misfire costs far more context
than the instruction ever will. An agent that concludes "MPM does not
have this documentation" because nothing told it references are a
separate subsystem will spend a long time re-deriving what is already
stored.

The current limits are **14,000 bytes and 2,000 words** for the
host-neutral canonical block. They are **maintainability warnings and
review triggers**, not budgets to optimise against:

* Do **not** remove useful guidance, compress an instruction until it
  is ambiguous, or sacrifice discoverability to stay under a ceiling.
* If an essential instruction genuinely needs the room, raise the
  ceiling. Say so in review. Do not mutilate the instruction instead.
* If the block is already correct and verbose, tighten the *wording* —
  that is a legitimate improvement. What is not legitimate is
  deleting a capability to buy headroom.

What the ceilings do still guard is **accidental** growth: duplicated
sentences, restated tool documentation, and mechanical reformatting
that balloons a one-line change. Those are the regressions a size
gate catches cheaply, and `test_render_managed_blocks.py` carries that
coverage (byte ceiling, word ceiling, duplicate-sentence detection,
and per-invariant concept anchors).

This policy governs every supported host: all five adapters render
from this one canonical block, so it applies uniformly to Claude
Code and OpenCode, and to the copy/paste
examples and README rendered from it.

## Updating the managed instructions

1. Edit the canonical managed block above.
2. Update the four copy/paste examples at the top of this file to
   match the new canonical block (render script does this).
3. Update the behavioural protocol
   ([`mpm-agent-protocol.md`](./mpm-agent-protocol.md)) if the
   underlying contract changed.
4. Run `python3 scripts/render_managed_blocks.py` to regenerate
   the four host template snippets.
5. Run `python3 scripts/render_managed_blocks.py --check` for an
   idempotency assertion (no install required for verification).
6. Run `make test` (or the relevant Go test target) to cover the
   full parity + clean-install + cross-adapter test surface.

## Ownership rule

The render script's output is the **only** source of truth for the
text inside each host's managed section. Do not hand-edit the per-host
`templates/*.snippet` files or any installed persistent-file block —
those changes will be lost on the next render or install. If the
canonical block needs to change, edit it in this file.

Host-specific instructions (file paths, scope choices, hook wiring,
recovery notes) belong in the per-host adapter's header/footer
sections, **outside** the canonical managed block. Add or change
those directly in `scripts/render_managed_blocks.py` (the
`ADAPTERS` table carries per-host `header` and `footer` strings).

# Versioning

This file's top-of-file HTML comment carries the **managed-instruction
contract version**:

```
<!-- mpm_agent_integration_version: MAJOR.MINOR.PATCH -->
```

The version pins the contract that adapters render into each host's
managed section. It is independent of the MPM software release
version (e.g., `v0.1.0-alpha-final`). Parity byte-for-byte remains
the authoritative drift detector; the version marker is a coarse
signal for downstream adapters that want to pin to a known contract
shape.

## When to bump

- **patch** (1.0.0 → 1.0.1) — typo fixes, comment clarifications,
  formatting, prose tightening; no semantic change to the rendered
  contract. Example: fixing a wording glitch inside the canonical
  block.
- **minor** (1.0.0 → 1.1.0) — additive — new optional tool/param
  documented in the tool-reference stability contract table, new
  tool added, new host added, expanded examples; existing
  contracts still resolve under the old version.
- **major** (1.0.0 → 2.0.0) — breaking — managed-block marker
  convention changes, required field added to an existing tool,
  action rename, render-contract change that would force adapter
  re-verification. Host adapters MUST be re-verified against the
  new contract.

Bump this marker when the canonical managed block or the
tool-reference stability contract table changes in a way that
matters to consumers. The marker does **not** replace byte-for-byte
parity; the render script's `--check` mode remains the authoritative
drift detector.

### Why the current marker is `3.0.0`

The contract was `2.1.0` until October 2026, when the managed block
was replaced wholesale by an inheritance-oriented contract: ten
responsibilities replace the previous seven invariants, and every
host adapter was regenerated from the new canonical source.

That is a **major** bump, not a minor one. A minor bump means
"existing contracts still resolve under the old version"; they do not
here — the rendered block an adapter installs is a different
document, so each adapter had to be re-verified against the new
contract, which is exactly the major-bump criterion. The MPM
**product** version did not move with it: this marker versions the
behavioural contract, not the software, and changing the text agents
read is not a release of the binaries.

## Installed-block provenance (added at install time)

An installed managed block records where it came from. Immediately after
the inner `<!-- BEGIN MPM MANAGED BLOCK -->` marker, the installer
writes one HTML comment whose fields are:

| Field | Meaning |
|---|---|
| `version` | the MPM **product** version of the build that wrote the block |
| `commit` | the exact **source commit** that build was made from |
| `protocol` | the behavioural protocol version — the `mpm_agent_integration_version` declared by this file |
| `applied` | the UTC moment the block was last applied or updated |

The comment's exact text is **not reproduced here on purpose**. This
file is the single source of truth for the exact agent-facing
instruction text, and it is guarded against carrying per-install
metadata — a per-build commit SHA or a timestamp baked into it would
make every byte-parity gate drift on every build, and a literal example
of the comment would put that metadata one copy-paste away from every
host. The rendered form is documented in
`agent_installation/README.md`, which is where the managed-block
convention lives. The mechanism itself is
`scripts/block_provenance.py`, and the prefix it emits is
`block_provenance.PROVENANCE_PREFIX`.

The `protocol` field carries the `mpm_agent_integration_version` from
the top of this file. It is **independent** of the product version: it
versions the behavioural contract, not the software, and the two move
on separate schedules. Neither is ever substituted for the other.

The product version and the source commit come from the environment
(`MPM_BUILD_VERSION` / `MPM_BUILD_COMMIT`), which `install.sh` and
`make refresh-installed` both export from the canonical build script
(`scripts/build-provenance.sh`). An adapter installer run on its own —
outside a build — may establish neither, and then it writes `unknown`
rather than claiming a version it cannot establish. It does not erase an
established value either: when the behavioural text is unchanged, the
value the block already carries is kept.

**The comment is not part of the canonical text.** It is per-install
metadata layered on top of it, and that is deliberate: the drift gates
(snippet, copy/paste, README, installed-drift) normalise the provenance
comment away on both sides of their comparison, so they still compare the
behavioural text — which is what they are about — byte for byte.

**The timestamp records the last change, not the last run.** It is
preserved when the version, the commit, the protocol version and the
behavioural text are all unchanged, and refreshed whenever any of them
changes. Re-running the installer with the same version, commit and
instructions is a genuine no-op: same bytes, same timestamp, no new
backup. (The same rule already governed the `<!-- generated: ... -->`
line.)

The mechanism lives in `scripts/block_provenance.py` and is shared by
every host adapter, so the convention is identical across all five.
