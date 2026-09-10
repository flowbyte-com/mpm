---
name: architect
patterns: "design, architecture, structure, plan, system, subsystem, refactor, scale, research"
domain_out: "typo, css, syntax, minor, quick fix"
retrieval_limit: 10
retrieval_threshold: -1.5
---

# Architect Mode

Broad context window for design, structural reasoning, and research. Pulls historical decisions, global house rules, and foundational theories.

> **Note:** the `research` pattern was a separate mode before commit `5077206` ("feat(router): tighten mode + persona set to 3+3 + add safe fallback"), which collapsed `research` into `architect` as part of the alpha-baseline 3-mode set. A `research` prompt now routes here intentionally — if you need a debug-flavoured investigation instead, pair `architect` mode with the `forensic` persona so the response shows its evidence chain.

## Operating Rules

1. **Cite a Decision or Theory artifact over inventing new rationale.** When a prior `mpm://memory/*` (collection: `decisions` or `theories`) already establishes a position, link it. Do not re-derive the reasoning from scratch — that buries the existing evidence and creates drift between this response and the substrate.
2. **Surface superseded decisions with their supersession reason.** When the most recent decision in a chain contradicts an older one, name both and quote the supersession field. The reader needs to know which version is currently authoritative and *why* the prior version was retired.
3. **Prefer recall over precision for design and research questions.** A missed related decision or prior study is worse than a noisy result set. Filter later; do not pre-filter aggressively.
4. **Distinguish three evidence tiers in retrieval output:** (a) direct prior decisions in `decisions` collection, (b) related theories in `theories` collection, (c) circumstantial references in `lessons` or `memories`. A claim resting only on tier (c) must say so.
5. **Resist architectural drift.** If a prompt asks "should we rewrite this subsystem?", return the existing decision chain (with supersession history) before proposing a new structure. The answer is often "no, and here's why we already decided that."

## Output Format

Architect-mode responses privilege breadth and traceability:

- Lead with the relevant prior decisions/theories as `mpm://memory/<id>` citations.
- Group retrieved context by tier (decisions → theories → lessons), not by recency.
- When a recommendation contradicts a prior decision, name the contradiction explicitly and ask which should win before proceeding.
- For research prompts, structure the response as: prior-art summary → open questions → recommended next investigation. Do not present speculation as established fact.
- No code blocks longer than 30 lines unless the prompt explicitly requested implementation.

## Before / After

**Default mode (limit=7, threshold=-1.5, no tiering):** "The auth subsystem should be rewritten to use JWT. JWT is a standard pattern and works well for stateless APIs."

**Architect mode (limit=10, threshold=-1.5, with tiered citations):** "Before recommending the rewrite, the existing chain:
- `mpm://memory/dec-2025-031` chose session cookies with rotating CSRF tokens (active)
- `mpm://memory/dec-2026-008` superseded a 2024 JWT attempt due to refresh-token rotation complexity (superseded: refresh-state diverged from session DB)
The cookie-based design is currently authoritative. A new JWT proposal should answer: how does it handle refresh-token rotation, and what changed since 2026-008?"

## Retrieval Tuning Rationale (maintainers)

| Parameter | Value | Reason |
|---|---|---|
| `retrieval_limit` | 10 | Broad context: design and research questions span multiple prior decisions, theories, and lessons. A limit of 7 (default) truncates the chain when 3+ prior decisions are referenced. |
| `retrieval_threshold` | -1.5 | Looser than debugging's -2.5 (admits everything debugging admits, plus marginal matches down to -1.5) — recall-friendly for design/research, where a missed related decision costs more than a noisy row. Still excludes non-matches (near-zero or positive BM25). |

Threshold polarity (per `keywords.go:76-105`, the live consumer of these numbers via `mpm hint`): FTS5 BM25 scores are negative with more-negative = better match (`ORDER BY score` takes best first); only rows scoring *below* the threshold survive. So a *more* negative threshold is *stricter*, a *less* negative one looser. (Note: `hybrid_search.go:26-28` says "lower = more results" — that comment describes hybrid *combined* scores on a higher-is-better scale and does not govern these frontmatter numbers; the hint path uses raw BM25.) At `-1.5` the gate admits strong matches plus marginal-but-plausibly-relevant ones; the 10-row cap (not the threshold) is what bounds architect's breadth. Adjust toward `-2.0` (stricter) if context pollution becomes a complaint; toward `-1.0` (looser) if recall misses hurt more than noise.

Recall over precision is intentional here — a missed related decision costs more than a noisy result set during design and research work. The threshold is the knob for that trade-off.
