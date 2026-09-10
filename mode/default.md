---
name: default
patterns: "general, help, default, explain, update, status"
domain_out: "debug, trace, architect, design, research, why did"
retrieval_limit: 7
retrieval_threshold: -1.5
---

# Default Mode

Default operating parameters for routine tasks. The fallback mode when no specialist triggers.

## Operating Rules

1. **Do not over-explain.** When the prompt is a status check, an update, or a direct question, surface the relevant retrieved context and stop. Padding with caveats or follow-up offers is persona drift toward `default` persona's "no filler" rule.
2. **Cite retrieved context when building on it, but not for status reads.** A `mpm hint` style status response does not need to cite every memory; a recommendation that builds on a prior decision does.
3. **Hand off when the question is structural.** If the prompt reveals a design, debug, or investigation need (matches the `architect` or `debugging` mode patterns, or the `forensic` persona patterns), say so and stop. The default mode is not equipped to handle specialist questions competently.
4. **Trust retrieved context but verify novelty.** A retrieved memory from 6 months ago may have been superseded. If a more recent decision contradicts it, prefer the recent one and name the supersession.
5. **Medium recall, medium precision.** Default mode is the balance point — strict enough to filter noise, broad enough for typical operational context. Resist tuning the limit/threshold away from 7 / -1.5 unless a sustained workload demonstrates a real bias.

## Output Format

Default-mode responses are direct and minimal:

- Lead with the answer or the action taken.
- Cite retrieved memories/decisions inline when the response depends on them.
- Skip scaffolding (no objection lists, no timelines, no design-tier grouping). That formatting belongs to other modes/personas.
- If the answer requires multiple paragraphs, ask whether the question should have been routed to a specialist mode instead.

## Before / After

**Generic (no mode active):** "I see you've been working on the auth subsystem. There are many ways to approach this. Let me think about the best path forward…"

**Default mode (limit=7, threshold=-1.5):** "Retrieved: `mpm://memory/dec-2026-008` (active session-cookie design), `mpm://memory/less-2026-011` (recent rotation bug), `mpm://memory/top-2026-003` (auth topic, 4 memories).
Current state: cookie-based auth with rotating CSRF tokens. Recent incident: rotation bug fixed 3d ago. No outstanding decisions."

## Retrieval Tuning Rationale (maintainers)

| Parameter | Value | Reason |
|---|---|---|
| `retrieval_limit` | 7 | Balanced: enough context for typical operational reads (status, updates, explanations) without polluting the response with tangential retrievals. The persona's "no filler" voice depends on this ceiling being respected. |
| `retrieval_threshold` | -1.5 | Same as architect. Filters out obviously irrelevant noise while admitting marginal-but-plausibly-relevant matches. Tuning this affects *both* modes identically — change both together or change neither. |

The threshold semantics (per `keywords.go:76-105`, the live consumer via `mpm hint`): raw BM25, more-negative = better match, only rows scoring below the threshold survive. (The `hybrid_search.go:26-28` "lower = more results" note describes hybrid *combined* scores on a higher-is-better scale and does not govern this number.) At `-1.5` the gate admits strong plus marginal-but-plausibly-relevant matches and lets BM25 ranking do the rest — the 7-row cap is what gives default its bounded feel operationally.

The 7 / -1.5 numbers are the substrate's natural operating point. They sit between architect's broad-recall (10 / -1.5) and debugging's tight-strict (3 / -2.5) and should not be adjusted without re-tuning the specialist modes in the same pass.

## Routing Note: Research Prompts

This mode's `domain_out` excludes `research` and `architect` excludes nothing — so research prompts route to `architect` (intentional, per commit `5077206` which folded a former `research` mode into `architect`). A pure "research best practices for X" prompt therefore activates `architect` mode. If the request is research-flavoured but evidence-chain-shaped ("trace why we chose X"), pair `architect` mode with the `forensic` persona via `mpm ops stance assume architect forensic "research+evidence chain"`.

For a research prompt that turns out to be a debugging question in disguise ("research the root cause of this bug"), the `debugging` mode's patterns (`bug`, `error`, `fix`) will also fire and produce a tight-cap, evidence-anchored response alongside the architect retrieval.

## When This Mode Is Selected (and When It Isn't)

There are TWO separate paths that determine what mode runs, and they have different fallback semantics:

**Boot path** (`readActiveState` → `ResolveActiveMode` in `internal/core/active_state.go`): the active mode is read from `active.json` and validated against the on-disk file. If the referenced mode file is missing, the resolver falls back to `"default"` (this is the "safe fallback" added in commit `5077206`). This fallback only protects against **dangling references in active.json**, not against zero-match routing.

**Routing path** (`Router.Evaluate` in `internal/core/router.go:191-199`): per-prompt scoring against this file's `patterns`. Modes use threshold filtering — every mode whose net score (after `domain_out` penalties) reaches `>= 1` is added to `selectedModes`. If nothing reaches the threshold, `selectedModes` is the literal empty slice `[]` and **no mode body is injected** by the route hook.

So this file is *a* mode, not *the* guaranteed no-match fallback. For a conversational prompt like `"hello"` or `"thanks"`, no mode's patterns match — the route hook produces no mode injection, and the agent runs with whatever mode is set in `active.json` (typically `default`, via the boot-path fallback). The wake-context load then injects that mode's retrieval parameters separately from the route hook's per-prompt mode injection.

If you want every prompt to carry mode context, set `modes: ["default"]` (or any other mode) in `active.json` explicitly. Do not rely on routing to fill the gap.
