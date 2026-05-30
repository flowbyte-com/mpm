# MPM — Breaking 90/99

Current score: ~71/99. Target: 90+.

This document identifies the gaps and the specific items that close them. Sorted by impact.

---

## The Ceiling Problem

MPM scores71 because it does what it does adequately. To break 90, it needs to do things that **genuinely differentiate it from a notepad with FTS5.** The delta is in capability class, not feature count.

The difference between 71 and 90 is:
- **71:** retrieves facts you stored
- **90:** understands what you know, surfaces what you need before you ask, and maintains a coherent model of reality that gets smarter over time

---

## Items That Move the Needle

### 🔴 Critical (must-have for 85+)

**1. Semantic Search — Embeddings Layer**
- FTS5 keyword search is the single biggest ceiling. BM25 is matching, not understanding.
- Add an embeddings column (local via Go+ SQLite, or Ollama) with cosine similarity scoring.
- Hybrid search: FTS5 for exact matches + embeddings for semantic recall.
- This alone could move the score 8-12 points.
- Scope: `src/embeddings.go`, `internal/embed_manager.go`, new `mpm recall --semantic` flag, existing FTS5 path unchanged for backward compat.

**2. Visual Memory Graph**
- Memory relationships are invisible in CLI. A graph view changes the interaction model entirely.
- Canvas-based (use the canvas skill) or lightweight web UI showing nodes (memories), edges (topic links, cross-refs), and state (challenged = red, LTM = gold, stale = dim).
- This moves it from "tool you query" to "environment you inhabit."
- Scope: `docs/graph_explorer.md` spec, canvas or web renderer, `mpm graph` command.

**3. Watcher + Synthesis Isolation**
- Both run in the same process. A synthesis panic kills the watcher. A watcher deadlock stalls synthesis.
- Move synthesis to a subprocess or goroutine pool with explicit panic recovery.
- Score impact is about reliability perception — a system that never fails feels higher quality.
- Scope: `cmd/mpm/synthesis.go` goroutine isolation, panic handlers, restart-on-failure.

**4. Multi-vendor Synth**
- Currently synth is single-vendor (MiniMax). API failure = silent synth failure.
- Add a provider abstraction: OpenAI → MiniMax → Ollama fallback chain.
- Score impact: 2-3 points, mostly perception of robustness.
- Scope: `internal/synth.go` provider chain, env var config for API keys/urls.

---

### 🟡 High Impact (moves to 87-90)

**5. Proactive Suggestion Engine**
- Not just `mpm hint` when you mention something — periodic "you haven't looked at X in a while" driven by cron.
- Surfaces stale but important memories, undiscussed topics, pending challenge theories.
- Changes MPM from reactive to pre-emptive.
- Scope: `mpm cron suggest` (cron job), `internal/suggestion_engine.go`, MCP tool `proactive_suggest`.

**6. Memory Lifecycle Dashboard**
- `mpm ops status` is a text dump. A real dashboard: decay curves, challenge queue depth, synthesis rate, heaviest topics.
- Makes the epistemology engine visible and actionable.
- Scope: `cmd/mpm/dashboard.go`, terminal UI or canvas render.

**7. External Import Pipeline**
- Readwise, Pocket, Raindrop, Notion — these are where memories go to die.
- A structured ingest pipeline that normalizes bookmarks/articles/notes into MPM memories with source attribution.
- Scope: `cmd/mpm/ingest/` package, `mpm ops ingest` commands per source.

**8. Memory Versioning**
- Currently: overwrite. No history.
- Add a lightweight revision log: `mpm history<id>`, `mpm diff<id> <v1> <v2>`.
- Critical for epistemology — decisions and theories change, and the audit trail should be traceable.
- Scope: `internal/history.go`, `mpm history/diff` commands.

---

### 🟢 Ecosystem (moves to 90+)

**9. Plugin Architecture**
- MPM is a platform, not just a tool. Third-party tools should be able to register commands, hooks, and data sources.
- Minimal plugin API: register command handler, register MCP tool, subscribe to memory events.
- Scope: `internal/plugin.go`, `docs/PLUGIN_API.md`.

**10. Feedback-Driven Weight Adjustment**
- Currently weight is manual or decay-based. A feedback loop: user says "this was useful" or "this is wrong" and weight adjusts.
- `mpm +<id>` / `mpm - <id>` as lightweight feedback, not just `reinforce`/`weaken`.
- The system learns from interaction, not just time.
- Scope: `cmd/mpm/handlers.go` feedback handlers, weight adjustment logic.

**11. Shared Memory Across Agents**
- Sessions currently siloed. A shared memory layer (opt-in) for multi-agent workflows.
- `mpm share<id> --scope<team|project>` — memory visible to other agents working in the same scope.
- Scope: `internal/shared.go`, `mpm share` command, scope-based ACL.

---

## Score Projection

| Item | Points Gained | Running Total |
|------|--------------|---------------|
| Semantic Search | ✅ Done | +10 | 81 |
| Visual Graph | +4 | 85 |
| Watcher/Synth Isolation | +2 | 87 |
| Multi-vendor Synth | +2 | 89 |
| Proactive Suggestion | +1 | 90 |
| Dashboard | +1 | 91 |
| External Import | +1 | 92 |
| Memory Versioning | +1 | 93 |
| Plugin Architecture | +1 | 94 |
| Feedback-Driven Weights | +1 | 95 |
| Shared Memory | +1 | 96 |

---

## Execution Order

1. **Semantic Search** — biggest ceiling, do first
2. **Multi-vendor Synth** — easy win, quick win
3. **Watcher/Synth Isolation** — reliability, before it becomes a problem
4. **Visual Graph** — changes the interaction model
5. **Proactive Suggestion** — makes MPM pre-emptive, not reactive
6. **Dashboard** — makes everything visible
7. **External Import** — ecosystem unlock
8. **Memory Versioning** — epistemology audit trail
9. **Plugin Architecture** — platform scale
10. **Feedback-Driven Weights** — learning loop
11. **Shared Memory** — multi-agent

---

## What NOT to Add

- Mobile UI — scope creep, wrong platform
- Cloud sync — contradicts single-binary zero-dependency philosophy
- AI summarization of memories — adds vendor dependency without solving the retrieval problem
- Collaborative editing — wrong threat model (personal knowledge management, not team wiki)
