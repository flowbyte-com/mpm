# MPM — Agent-onboarding MCP-native channels audit (2026-09-05)

> **Status:** audit-only. No migration proposed in this pass. Findings
> + ranked recommendation only, per the brief.
>
> Scope: 4-part investigation of whether MPM's current `agent_installation/
> MPM_AGENT_INTEGRATION_SNIPPETS.md` (one universal managed block,
> replicated into per-host files by `render_managed_blocks.py`) could
> be replaced — partially or fully — by MCP-native mechanisms:
> `initialize.instructions`, per-tool `description`, and a tool
> naming/grouping audit. OpenClaw is out of scope for Part A (it
> does runtime injection, not file-based onboarding).

## Part A — does `initialize.instructions` reach the model?

### What the MCP spec actually says

Verified against the lifecycle spec (`modelcontextprotocol.io/specification/2025-06-18/basic/lifecycle`):

- The `InitializeResult` schema has an `instructions` field with comment "Optional instructions for the client".
- The spec uses the wording "for the **client**", not "for the **model**" or "for the user".
- The spec does **not** mandate that the client surface the field to the model automatically.

The "for the client" wording means: client implementations decide what to do with it. There is no portability guarantee across hosts.

### Per-host empirical + source-grep findings

| Host | Support | Evidence |
|---|---|---|
| **Hermes** | **NO** | Local source: `/home/v/.hermes/hermes-agent/tools/mcp_tool.py:2495` stores `self.initialize_result: Optional[Any] = None` after `await session.initialize()`, but every reference to `initialize_result` in the codebase reads only `.capabilities` (line 7092, 2524). Zero hits for `.instructions` in client-side code. The instructions field is stored but never extracted into the system prompt, log, or any model-visible context. Confirmation: `grep -rn '\.instructions' /home/v/.hermes/hermes-agent/ --include='*.py' \| grep -v _test.py \| grep -v protein_shellcode` returned nothing relevant. |
| **Claude Code** | **YES — auto-injected into the system prompt as an `<mcp_instructions>` block at session start, in addition to per-server `--mcp-config` server-list metadata.** | **Interactive probe transcript (2026-09-05).** A stdio MCP server whose `instructions` field was a unique marker `MPM_PROBE_E279C249` (written to `/tmp/instructions_probe/marker.txt` at server startup, cross-checked against the model's response) was registered with `claude --mcp-config ... --strict-mcp-config --allowedTools "mcp__mpm_probe__echo"`. The model quoted the marker verbatim from both the tool-return and the system-prompt surfacing, with no instruction to do so in the user prompt. Earlier public-source analysis was inconclusive (Anthropic docs do not mention the field; TypeScript MCP client is not in the public repo tree) — the public docs are silent about a behaviour the implementation does provide. Tool descriptions and schemas are also forwarded to the model on `tools/list`, but that is a separate MCP-spec behaviour from `instructions` and is covered in Part B. |
| **OpenCode** | **YES — auto-injected as an `<mcp_instructions>` system-prompt block at session start.** | **Interactive probe transcript (2026-09-05).** Same probe server (marker `MPM_PROBE_F77BB74C`) was wired through `opencode.jsonc` `mcp.mpm_probe` and run via `opencode run --print-logs --pure` from a clean cwd. The model quoted the instructions field verbatim alongside the tool's own return value, and explicitly attributed the source: *"Note: this last quoted block is reconstructed from the system prompt's `<mcp_instructions>` block, not returned by `mpm_probe_echo` itself — the echo tool only returns the `echoed` and `probe_marker` fields shown above."* This attribution is the unambiguous signal — the model received the field via the host's system-prompt injection, not via any tool call. Same caveat as Claude Code re. docs: OpenCode's public docs (`opencode.ai/docs/mcp-servers/`) do not describe this behaviour, but the implementation does provide it. |
| **Pi (pi-mono)** | **REACHABLE ONLY VIA EXPLICIT PROXY CALL — `instructions` is NOT auto-injected; the upstream `pi-mono` CLI does not natively ship MCP at all, so this verdict is conditional on the third-party `pi-mcp-adapter` (v2.32.1, published 2026-09-01, maintained by `nicobailon` outside the `badlogic` org, listed on `pi.dev` and seeing 761K downloads/mo).** | **Interactive probe transcript (2026-09-05), three steps.** Probe 1: `mcp({instructions:"mpm_probe"})` returns *"No instructions cached for 'mpm_probe'. Use `mcp({ connect: 'mpm_probe' })` to connect and refresh."* — confirming (a) the adapter does **not** auto-connect at startup, and (b) the `instructions` shortcut is only populated from a prior cached connection. Probe 2: `mcp({connect:"mpm_probe"})` returned the marker `MPM_PROBE_2A31810F` embedded in the instructions field as part of the connect handshake response — but a follow-up `mcp({instructions:"mpm_probe"})` in the same turn again reported *"No instructions cached"*. This is a state-isolation quirk in the adapter: the `connect` response embeds the instructions inline, but the cache the `instructions` shortcut queries is not populated by the ad-hoc connect. Probe 3 (the natural cold-start test): asked the model to *"Use the MCP server named mpm_probe. Call its echo tool with the message 'hello from probe'. Then tell me what the instructions field for that server contains."* — the model autonomously invoked the proxy, ran the tool, and reported the instructions field verbatim. **So the field is reachable in Pi via the proxy, but it is discoverability, not auto-onboarding: the model has to actively call the proxy to see the field. The cold-start `instructions` reach that Claude Code and OpenCode provide is NOT replicated.** This matches the brief's prediction that Pi's lack of native MCP plus the adapter's invocation-by-design would make this a discoverability improvement rather than a replacement for automatic delivery — and that the managed block remains necessary for Pi's onboarding. (The earlier `agent_installation/pi-mpm/README.md` claim that "Pi explicitly does not support MCP" is still true at the `pi-mono` level; it is now misleading because it ignores the third-party bridge. Follow-up: that README should be amended to acknowledge the adapter and clarify what it does and does not replace.) |
| **OpenClaw** | out of scope (Part A) | OpenClaw was scoped out per the brief; runtime injection does not depend on file-based or `initialize`-based onboarding. |

### What this means empirically

In the four hosts I could verify:
- For **Hermes**, the **client-side source** demonstrates that `instructions` is read into `initialize_result.capabilities` only. If MPM ships an `instructions` field, Hermes currently ignores it.
- For **Claude Code** and **OpenCode**, the `instructions` field IS surfaced to the model at session start (auto-injected into the system prompt as a per-server block). The public docs are silent about this behaviour, but interactive probes confirm it.
- For **Pi (via the third-party `pi-mcp-adapter`)**, the field is reachable via the proxy but only on explicit call, and the upstream `pi-mono` has no MCP at all — so the question is whether the *practical* Pi experience is "instructions-aware at cold start," and the answer is **no**.

There is no universal "host surfaces instructions" guarantee. Any migration that relies on this field reaching the model is not portable. Concretely, an `instructions`-only migration would leave Hermes and Pi without onboarding; per-tool descriptions (Part B) and the managed block itself remain necessary for those hosts.

### Size limits

Where the field IS surfaced (none confirmed in this audit), the spec does not impose a length limit. Host implementations may apply their own. Without empirical probe of a host that actually surfaces it, no reliable limit is stated.

## Part B — do per-tool descriptions carry the operational constraints currently in the managed block?

### Size of the managed block (canonical)

The single rendered block — universal across hosts, only the tool name prefix changes per host — is **~3.0 KB / ~5 sections of text**:

1. **Wake on session start** — `mpm_context.read_wake_context` is the only wake entry point; mention of `<available_skills>` catalogue and `params.projection: "compact"` option; orient signals listed (mode, persona, recent topics, recent memories, recent milestones, last handoff, open work, overdue scheduled wakes).
2. **Persist during work, not only at the end** — list of actions on five different tools (`mpm_memory.save`, `mpm_decisions.record`, `mpm_lessons.save`, `mpm_topics.create`, `mpm_references.add`); heuristic for what counts as durable.
3. **Skill discovery before reinventing** — `mpm_context.proactive_recall_hint` filter; `mpm_skills.read` and `mpm_skills.list{scope:all}` fallback; "<available_skills>" catalogue already in wake.
4. **Handoff before genuine session closure** — `mpm_handoff.write` with `{summary: required, session_id, state ∈ {clean, crashed, interrupted, force_end}, commitments[], open_questions[]}`; mid-session acks are NOT session-closing; `mpm_scratchpad` lifecycle (flush/read/discard/promote) for intra-session volatile state.
5. **Session closure is not work completion** — `mpm_work` lifecycle (`create/update/note/complete`); explicit statement: host session termination does NOT auto-complete a work item.
6. **Source of truth** — generic principle, no specific tool.
7. **Recovery / fallback** — `mpm call <tool> --payload ...` when MCP transport is unavailable; provenance attribution.

Note that rules 6 and 7 are **not tool-attached** — they're cross-cutting behavioural principles. They never need to move into a tool description.

### Tool-description audit — gaps where the constraint lives only in the managed block

| # | Tool | Operational constraint currently block-only | In description? | Gap? |
|---|---|---|---|---|
| 1 | `mpm_work` | `complete` requires `work_id` | Schema: `work_id` is an unrequired string in the generic params object. Description does NOT say it's required for `complete`. | **GAP** — same-shape params for all actions, no per-action requirement |
| 2 | `mpm_work` | "host session termination does NOT auto-complete a work item" | Description says "A work item is never truly 'done' until Git evidence is attached via the complete action." — partial cover of the verification half, not the no-auto-complete half. | **GAP (partial)** |
| 3 | `mpm_handoff` | `summary` is the only required field for `write` | Schema: `"required":["action"]` only — `summary` is NOT marked required at the schema layer. Description does NOT say it's the only required field. | **GAP** |
| 4 | `mpm_handoff` | mid-session acks are NOT session-closing | Description: "you are ending a session and need to leave a summary" — could be read either way (suggests session-closing at any handoff call). | **GAP** |
| 5 | `mpm_memory` | `projection` defaults to `summary`, not `full` | Description: "For broad queries, projection defaults to 'summary' to keep context bounded. Use projection='full' or mpm_resolve ONLY when reading the complete unabridged content of a specific pointer." | **COVERED** |
| 6 | `mpm_lessons` | `projection` defaults to `summary` | Description: same sentence as `mpm_memory`. | **COVERED** |
| 7 | `mpm_context.read_wake_context` | `<available_skills>` catalogue + `projection: "compact"` option | Description: "you need to read active behavioral directives governing the current session" — does NOT mention `<available_skills>` or `projection: compact`. The tool only has 1 action so the action is implied; the catalogue/compact aren't. | **GAP** |
| 8 | `mpm_skills` | discovery: read by name + list with `{scope:"all"}` | Description: comprehensive but does not mention `scope: "all"` fallback for catalog browsing. | **GAP (minor)** |
| 9 | `mpm_scratchpad` | four actions: `flush/read/discard/promote` with `{session_id, thesis, supporting}` | Schema has the params; description does not enumerate the four actions. | **GAP (minor)** — actions enumerable from schema `enum` |

### Implication for moving managed-block content into descriptions

Rules 5 and 6 (memory/lessons projection default) are already in descriptions — that's the precedent that "this kind of constraint can live in a description and reach the model reliably."

The seven gaps above are small enough to close: rule 2 alone is ~150 bytes; rule 7 is ~80 bytes; total estimated delta is well under 1 KB across the impacted tools. None of these changes touch the cross-cutting principles (rules 6, 7 of the managed block). The drift-detection analogue for descriptions is straightforward: a small test that asserts each impacted description contains the relevant phrase, in the same style as `render_managed_blocks.py --check`.

### Description size limits

The MCP spec does not impose a hard length cap on tool descriptions. Common practice in the model SDKs we use (mark3labs/mcp-go) is to forward descriptions verbatim into the tool list. LLM context windows render the full tool list at startup; if 22 tool descriptions aggregated exceeded 32 KB, some clients would truncate, but in this audit the aggregate is well under that and the gaps would add < 1 KB total.

I did **not** empirically probe any host's description-truncation behaviour in this audit — that would require the same kind of interactive probe I could not run for Part A.

## Part C — tool naming and grouping audit

### Tool name + action enum coverage (full registry)

| Tool | Description (first 100 chars) | Actions |
|---|---|---|
| `mpm_memory` | Persistent memory for facts, learnings, and context... | save, query, show, shred, reinforce, weaken, snooze, set_weight, patch, promote, review, synthesize, challenge, restore_challenge, commit_milestone |
| `mpm_theories` | Hypothesis management with explicit validation criteria... | propose, resolve, show, list, query |
| `mpm_decisions` | Immutable record of architectural choices... | record, supersede, invalidate, show, list, query |
| `mpm_lessons` | Durable lessons from failures, anti-patterns... | save, search, list |
| `mpm_topics` | Topic labels for clustering related memories... | create, search, link, list, show |
| `mpm_references` | Ingested external documents: PDFs, specs, whitepapers... | add, read, search, list |
| `mpm_evidence` | Attach observations, test results... | add, list, source_groups |
| `mpm_confidence` | Inspect and reason about the system's certainty... | show, recompute, changes, trend |
| `mpm_retrieval_diagnose` | Diagnostic for retrieval pipeline failures... | (1-action tool) |
| `mpm_context` | Agent session state, mode routing, directive management... | read_wake_context, read_directives, proactive_recall_hint, query_global_rules, record_global_rule, promote_to_global, route |
| `mpm_skills` | Reusable procedural knowledge... | save, read, list, delete, promote_to_global, workshop |
| `mpm_wakes` | Deferred work triggers scheduled for future execution... | schedule, check, check_pending_event, list, digest, upsert_task, list_tasks, delete_task |
| `mpm_handoff` | Inter-session communication... | write, read, list, shred |
| `mpm_scratchpad` | Intra-session volatile working memory... | flush, read, discard, promote |
| `mpm_system` | Maintenance, diagnostics, and housekeeping... | gc_run, compact, health_check, migrate, query_audit_log, list_clusters, snooze_cluster, resolve_cluster, annotate_cluster, critic_findings |
| `log_to_changelog` | Self-report agent work as structured changelog entry... | (1-action tool) |
| `request_review` | Concurrent multi-component review... | (1-action tool) |
| `mpm_resolve` | Resolve a mpm:// URI to its content... | (1-action tool) |
| `mpm_challenge` | Weaken a memory and create a pending theory... | challenge / restore |
| `mpm_blob_read` | Read a raw blob by ID... | (1-action tool) |
| `mpm_blob_search` | Server-side regex search within a blob... | (1-action tool) |
| `mpm_work` | Named work items with an immutable event ledger... | create, list, show, update, complete, cancel, history, note, reopen, resolve_contradiction |

### Per-tool naming assessment

Names that read cleanly from the name alone: `mpm_memory`, `mpm_lessons`, `mpm_topics`, `mpm_references`, `mpm_evidence`, `mpm_wakes`, `mpm_handoff`, `mpm_scratchpad`, `mpm_blob_read`, `mpm_blob_search`, `mpm_decisions`, `mpm_theories`, `mpm_confidence`, `mpm_skills`, `log_to_changelog`, `request_review` — name + a single noun implies the purpose.

Names that lean on the description to disambiguate:

- **`mpm_resolve`** — Description: "Resolve a mpm:// URI to its content." This is fine once read, but the bare name could read like conflict resolution or DNS-style resolution. **LOW likelihood** of confusion, but worth a one-word clarification in the description (which it already has).
- **`mpm_challenge`** — Description: "Weaken a memory and create a pending theory contesting it." Strong disambiguation. **LOW** on its own.
- **`mpm_context`** — Description: "Agent session state, mode routing, and directive management." Generic name; relies entirely on the description to convey the seven very different actions. **MEDIUM** likelihood of confusion for an agent unfamiliar with the tool — picking between `read_wake_context`, `route`, `proactive_recall_hint`, `record_global_rule` requires reading the full description.
- **`mpm_work`** vs **`mpm_scratchpad`** vs **`mpm_handoff`** — three "session lifecycle" tools with partially overlapping semantics (work items, scratchpad, handoff). **LOW** for skilled agents; **MEDIUM** for new agents who haven't internalized the distinction.

### Bundling many ops behind one `action` parameter — evidence?

I searched the codebase for any locally-run tool-selection eval data and found none. I also searched published MCP guidance — neither the MCP spec nor mark3labs/mcp-go gives strong direction either way on action-enum vs separate-tool. The general MCP-authored guidance emphasizes "tools and their descriptions are how models decide what to call" but does not specifically address the question.

For MPM specifically, this audit observed:

- `mpm_memory` has 15 actions, all sharing similar nested `params` shape. That's a lot of branches under one name; LLMs reading the description need to scan the action enum to know what's available. But the description is dense and helpful, and the model surface is single-handed.
- `mpm_system` has 10 actions covering `gc_run`, `compact`, `health_check`, etc. — the actions are heterogeneous (operational vs diagnostic).
- `mpm_wakes` has 8 actions, several of which (`upsert_task`, `check_pending_event`, `digest`) are useful but not inferable from the tool name alone.

If the framework default is "fewer ops per tool, more tools," `mpm_memory` would split into `mpm_memory_save`, `mpm_memory_query`, etc. That increases tool-list size (already 22) and may or may not improve picking accuracy. **Inconclusive from available evidence; should be tested, not decided by intuition.**

### Near-duplicate-sounding concerns — ranked by likely wrong-tool-call risk

#### HIGH — `mpm_challenge` vs `mpm_memory.challenge`

Both call `dm.ChallengeMemoryWithTheory(memoryID, evidence)`. Both succeed; both return `{memory_id, theory_id, ...}`. The handler comment at `internal/core/tools/handlers_mpm_challenge.go:14-16` says: *"It wraps dm.ChallengeMemoryWithTheory directly — same behavior, same response shape (memory_id, theory_id, theory_status, action), so the existing callers see no change."* And the comment at line 41 says the legacy `mpm_memory.challenge` action is "preserved for parity."

A model might pick either surface. They produce identical on-disk state. **The duplication is harmless for state divergence** but creates parallel surfaces for the same operation; an agent trying to be "consistent" with prior session choices could end up calling both, doubling the work. Or one might be deprecated on the model side without the model knowing.

**Recommendation: deprecate one of the two.** Either (a) hide `mpm_challenge` and keep `mpm_memory.challenge`, or (b) hide `mpm_memory.challenge` and point `mpm_challenge` at the rest. Either way, the deprecated surface should be removed from the tool list rather than left as a confusing parallel.

#### MEDIUM — `record` (decisions) vs `save` (memory/lessons)

`mpm_decisions.record`, `mpm_memory.save`, `mpm_lessons.save`, `mpm_topics.create`, `mpm_references.add`. Five different verbs for five different operations that all do "persist a row." An agent writing a decision might instinctively reach for `mpm_memory.save` out of muscle memory. Each verb was probably chosen for a reason specific to that tool's content shape (a decision has a *choice + context + rationale*, a memory has a *fact*, etc.), but from the model's perspective the verbs are not inferable from the names alone.

**Likelihood:** medium. The five descriptions are clear about which goes where, but a hurried agent — or an agent that has internalized mpm as "save fact / save lesson" — might guess `mpm_memory.save` for a decision-context fact.

**Recommendation:** No rename. The descriptions are explicit enough that a model reading them should pick the right tool. If this becomes a real failure mode (testable via the same eval setup as Part C's open question), revisit.

#### MEDIUM — `restore_challenge` (action) is a buried reverse path

`mpm_memory` action=`restore_challenge` and `mpm_challenge` action=`restore` — both call `dm.RestoreMemoryFromChallenge(memoryID)`. The action lives as the 14th of 15 enum values in `mpm_memory`'s schema, which the model sees in the JSON schema. Likely accessibly, but not inferable from any name.

**Recommendation:** same as the `mpm_challenge` duplication — deprecate one of the two surfaces.

#### LOW — `mpm_resolve` could read like conflict resolution

Description clarifies (`"Phase 2 supports mpm://blob/<id>, mpm://work/<id>, mpm://memory/<id>, mpm://lesson/<id>, and mpm://theory/<id>"`). Bare name is ambiguous; description is unambiguous. Keep name, rely on description.

#### LOW — `mpm_context` action=`route` vs `mpm_context` itself

The bundling is intentional (mode routing lives alongside wake context), but a model could conceivably try to *route* by calling `mpm_context` thinking it's the routing mechanism. Description covers this.

#### LOW — three tool names for session-lifecycle concepts

`mpm_work`, `mpm_scratchpad`, `mpm_handoff` are distinct enough — work items are durable tasks with verification, scratchpad is intra-session volatile, handoff is inter-session persistent. Description-aside, the names mostly carry their weight. A one-sentence cross-reference in each description ("for inter-session persistent, not volatile — see mpm_scratchpad / mpm_handoff") would help.

## Part D — ranked recommendation

### Option ranking

#### Option 1 — **KEEP current managed-block architecture as-is, with no migration in this pass**

**Best fit if:** the priority is portability across all hosts including ones whose MCP-client behaviour is unverified.

- Pros: the managed block already works on every host, with explicit per-host renderers and a working drift-detection mechanism (`render_managed_blocks.py --check`).
- Cons: the operational knowledge in the block is invisible to anyone or anything that doesn't happen to read the file at install time, and small drift incidents (operators editing the wrong file) recur (per the prior remediation pass).

#### Option 2 — **PARTIAL migration: close the tool-description gaps from Part B; add `instructions` field to mpm-mcp initialize as a defense-in-depth signal; keep the managed block for cross-cutting rules 6 and 7**

**Best fit if:** the priority is "make the constraints that *are* tool-attached also available to MCP-native tooling without removing the file-based fallback."

**What moves where (concretely):**
- Move the per-tool operational constraints (rules 1, 2, 4, 5 from the block — the wake-context details, work-completion rule, handoff-required-fields rule, projection defaults already done) into the corresponding tool **descriptions and schemas**:
  - `mpm_work` description: add "complete action requires `work_id`; host session termination does NOT auto-complete a work item — call action=`complete` with the work_id to mark it done." Add per-action `oneOf`-required-fields to the schema so the constraint is enforced.
  - `mpm_handoff` description: add "action=`write` requires `summary`; mid-session acks (ok/thanks/ty/ack) are NOT session-closing — don't write a handoff for them." Add `summary` to the required-array on the write branch.
  - `mpm_context` description (or `read_wake_context` action enum): add `projection: "compact"` option with description and example payload, and explicitly call out the `<available_skills>` catalogue.
  - `mpm_skills` description: add the discovery fallback — `list{scope:"all"}` for catalog browsing.
  - `mpm_scratchpad` description: enumerate `flush/read/discard/promote` so an agent who picks the tool name knows the lifecycle.
- Add `instructions` to mpm-mcp's `InitializeResult` (one-line SDK call: `NewInitializeResult(..., "...short protocol summary...")`). This is free; it's defense-in-depth. **It does NOT migrate the block off the files** — the files are still the canonical delivery.
- Keep the managed block for the cross-cutting (non-tool-attached) rules: rule 6 (source of truth), rule 7 (CLI fallback path), and the per-host install mechanics (file paths, hooks).

**What stays necessary regardless:**

- Host-specific file paths and install mechanics (e.g. `~/.claude/CLAUDE.md`, `~/.openclaw/workspace/flowbyte/mpm/agent_installation/CLAUDE_CODE_INTEGRATION.md`) — these are transport facts, not behavioural ones.
- Per-action enum values and tool-discovery (Part C finding: high `mpm_challenge`/`mpm_memory.challenge` duplication) — needs a separate decision and is independent of the managed-block question.
- Whatever Part A leaves uncertain (instructions field delivery): until we have empirical evidence that some host surfaces it, the file-based delivery is the only reliable channel.

**Migration cost and risk:**

- Cost is low in code (one-line SDK call to add instructions, plus description-text updates on five tools; plus per-action `oneOf`-required-fields which is a schema refactor).
- Risk: the `instructions` field change is **completely inert** on hosts that don't surface it (Hermes is one confirmed), and **additive** on hosts that do. No downside.
- Risk on description changes: descriptions are already part of the tool surface; if the model picks the right tool today, adding a clarifying sentence can't change the model away from the right answer.
- Drift detection: replace with per-tool description-content tests (e.g. `internal/core/tools/registry_drift_test.go` asserting each impacted description contains the relevant operational phrase). Cross-reference against the same escalation taxonomy. **The render-script's drift-detection mechanism is genuinely excellent and we should not lose it** — it covers copy/paste example bytes and adapter-rendered output. The new mechanism covers per-tool description content. Both are needed for a complete posture.

#### Option 3 — **FULL migration off the managed block, rely on `instructions` + descriptions**

**Best fit if:** there is strong empirical evidence (in Part A or follow-up probes) that the four supported hosts **all** surface the `instructions` field reliably.

**Current evidence does NOT support this option.** Hermes source shows it does not. Claude Code, OpenCode, Pi cannot be verified from public source. The spec does not require it. Without empirical proof across all four hosts, full migration would silently drop behavioural guidance on at least one of them (Hermes, with high confidence).

**Do not recommend in this pass.**

### Decision recommendations, ranked by impact per cost

1. **Close the seven tool-description gaps from Part B.** Highest value-to-cost: zero new infrastructure, immediate effect wherever the model reads tool descriptions (every host that supports MCP, regardless of `instructions` support). Drift-prevention requires a small new test (analogous to `render_managed_blocks.py --check` but on description content rather than file content). This is the move with the lowest risk and clearest benefit.

2. **Add `instructions` field to mpm-mcp's InitializeResult.** Free, additive, gives the system a graceful path if any future host supports it. Defense-in-depth, not a load-bearing change.

3. **Resolve the `mpm_challenge` / `mpm_memory.challenge` duplication in Part C.** This is independent of the managed-block question but came up. Either surface is valid; pick one and remove the other from the registry. Prevents confusion for both the model and future contributors.

4. **Investigate Part A gaps for Claude Code, OpenCode, Pi empirically.** Stand up the probe server at `/tmp/instructions_probe/probe_server.py` (already written in this audit) and drive each host's CLI in interactive mode with a marker string in the initialize response. Ask the model directly. Record pass/fail. If any host surfaces instructions, that's a free upgrade; if none do, the recommendation to not fully migrate strengthens.

### What I am explicitly NOT recommending in this pass

- Renaming tools (out of scope per Part C instruction "do NOT rename anything").
- Fully migrating off the managed block (evidence doesn't support it).
- Adding side-channel delivery mechanisms beyond what's already in place.
- Re-architecting the action-enum vs separate-tool pattern (no public eval evidence; would need a study, not a refactor).

---

*End of audit. Report-only deliverable. Awaiting project-lead
decision on which ranked recommendations to act on.*

### Follow-up (2026-09-05)

The HIGH-risk `mpm_challenge` / `mpm_memory.challenge` duplication
flagged in Part C was resolved the same day by retiring the
standalone `mpm_challenge` tool. The canonical challenge surface
is now `mpm_memory` action=`challenge` (and the matching
`restore_challenge` action for the restore side).

Substantive findings of this audit are unchanged. The "22 tools" /
"22 tools" counts in Part B and Part C reflect pre-retirement state;
post-retirement the registry holds 21 tools total. The two
recommendations in Part D that the project lead explicitly actioned
on (1: close the seven tool-description gaps, and 2: add `instructions`
to `InitializeResult`) are addressed in the next two commits.
