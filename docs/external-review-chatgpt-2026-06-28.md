# External Review of MPM — ChatGPT (cynical mode), 2026-06-28

## Overall: 8.8/10

| Area               | Score |
|--------------------|------:|
| Architecture       | 9.5   |
| Originality        | 9.5   |
| Practicality       | 8     |
| Cognitive model    | 9     |
| Complexity         | 6     |
| Documentation      | 9     |
| Risk of feature creep | 5  |

## What's genuinely excellent

1. **Separating facts from reasoning** — memories / decisions / theories / lessons / evidence as distinct collections, not text blobs.
2. **Confidence derived from evidence** — cached function of evidence rather than user-edited value.
3. **Challenge lifecycle** — Memory → Challenge → Theory → Evidence → Resolution, with old beliefs preserved as historical artifacts.
4. **SQLite-first** — disciplined substrate choice; avoids the alphabet-soup architecture diagram.

Documentation quality singled out, especially the line "Truth is external. Confidence is internal."

## What's overbuilt (per reviewer)

1. **Too many autonomous systems** — memory, theory engine, confidence engine, challenge engine, drift detector, self-healing, audit log, session handoffs, wake scheduler, proactive recall, routing, personas, modes, directives, SSE, maintenance, synthesis, immune system, review engine. "That's approaching an operating system." Risk is interaction complexity, not code complexity.
2. **Confidence becoming hard to reason about** — decay + evidence strength + evidence type + challenge + time + reinforcement + weight + drift = many knobs. **Recommendation: ONE canonical mathematical formula documented, not prose.** Something like `confidence = base + Σ positive − Σ negative − decay(t)`.
3. **Weight vs confidence overlap** — reviewer's question: "Can weight disappear?" Suggests retrieval score = BM25 + semantic + confidence + recency. Weight currently mixes importance / frequency / usefulness — three different dimensions.
4. **Personas + modes + directives = 8 layers** — needs a precedence diagram in README.
5. **Mixing two jobs** — Persistent knowledge (memories, decisions, theories, lessons) vs Agent runtime (modes, wake, scheduling, routing, personas, session handoff). Eventually split mentally — possibly same repo — into MPM Core and Agent Runtime.

## What to add

- **Belief graphs** — A supports B, A contradicts C, D depends on B. Confidence propagates. Explanations become richer: "this decision lost confidence because theory X failed" instead of "confidence = 0.42".

## What to remove

- Very little. Mostly postpone. **Freeze new features for several weeks, use the system, see what survives.** Half the "essential" features will be untouched; two tiny features will become the foundation.

## Final impression (verbatim)

> "I think this is one of the more thoughtful AI memory architectures I've seen from an individual project. The distinguishing idea is not SQLite, FTS, or even long-term memory. It's that you've treated **reasoning itself as a first-class persistent object**. Decisions, theories, evidence, and challenges are separate entities with explicit lifecycles rather than annotations on a blob of text. That is a meaningful conceptual step beyond most 'AI memory' systems."

> "The main risk is not that the architecture is wrong. It's that it becomes *too complete*. Mature systems often owe their longevity to having a very small, stable core. In MPM, I think that core is already visible: Persistent artifacts (memories, decisions, theories, lessons); Evidence-backed confidence; Challenge and revision instead of overwrite; Simple, durable SQLite storage."

> "If those remain the center of gravity and the surrounding features stay modular rather than entangled, MPM has the ingredients to be a durable cognitive substrate rather than just another elaborate memory layer."

## 808's reaction (initial read)

Strong agreement on items 1, 2, 3, 5 of the "overbuilt" section. Disagreement / nuance on item 1 ("too many systems"): each one was added because the prior state was missing it — not feature creep in the classic sense. The interaction-complexity concern is real but the cure is a precedence diagram and freeze, not removal.

The "split Core vs Runtime" framing is the reviewer's sharpest insight. Phase 5a (scheduled_wakes) is clearly Runtime; the memories/decisions/theories/lessons/evidence substrate is clearly Core. Worth adopting the framing mentally even without code-splitting.
