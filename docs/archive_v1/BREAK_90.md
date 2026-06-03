# MPM — Breaking 90/99

**Updated for machine-to-machine infrastructure context.** MPM is not a human-facing product — it is the memory and epistemology layer for a cognitive agent (808). Scores reflect agent utility, not aesthetics.

---

## The Reframe

The original 71/99 ceiling was scored through a human lens: dashboards, serendipity, visual graphs. Through the agent-loop lens, the priorities collapse to:

1. **Deterministic retrieval** — what the agent queries, it gets
2. **Fault tolerance** — synthesis failures must never stall the watcher
3. **Context integrity** — token-budgeted responses that never blow the agent's window
4. **Unshakeable loop stability** — isolated processes that survive vendor API hangs

The score delta (71 → 90) is not about features — it's about the agent never having to think about the memory layer.

---

## Items That Move the Needle

### 🔴 Critical — Agent Loop Infrastructure

**1. Watcher/Synth Isolation (NOW #1)**
- Watcher and synthesis share a concurrency context. A synthesis API hang blocks the watch loop. A watch loop stall deadlocks synthesis.
- **Fix:** Separate SQLite read connection for watcher (WAL readers are thread-safe). Spawn synthesis as an isolated goroutine with deadline propagation and independent panic recovery. Pass events via a typed channel.
- **DLQ (Dead Letter Queue):** Failed synthesis events (hard API outage across all vendors) route to a `synthesis_dlq` SQLite table rather than being dropped or blocking the channel. A periodic retry tick processes the DLQ when vendors recover.
- **Shutdown protocol:** `select` over `{event channel, shutdown signal, dlq-retry tick}`. On shutdown, drain the channel before exiting — no lost events.
- **Score impact:** +3 (reliability perception = loop stability = agent trust)
- **Scope:** `cmd/mpm/watch.go` goroutine split, `internal/synthesis_isolation.go`, `internal/dlq.go`, `internal/synth.go` multi-vendor chain

**2. Multi-vendor Synth Failover**
- Single-vendor synthesis is a single point of failure for 808's cognition. If MiniMax is down, the agent's synthesis pipeline stalls.
- **Fix:** Provider abstraction with ordered fallback chain. Env vars for per-vendor API keys/urls. Silent failover — 808 never knows a vendor切换 occurred.
- **Chain:** MiniMax → OpenAI → Ollama (local). Each provider has independent timeout (10s default). All vendors fail → event routes to DLQ.
- **Score impact:** +2 (hardened against vendor outage)
- **Scope:** `internal/synth.go` provider chain, env vars for `MINIMAX_API_KEY`, `OPENAI_API_KEY`, `OLLAMA_ENDPOINT`

**3. Native Token Budgeting (MCP tool layer)**
- 808 consumes MPM output directly into its context window. Unbounded hybrid search results can blow the window silently.
- **Fix:** MCP tool layer (OpenClaw plugin wrapping MPM) implements strict token budget using tiktoken. Results packed, measured, truncated to defined limit before JSON is handed to the agent.
- MPM core returns the best structured data; transport layer handles projection. Clean separation.
- **Score impact:** +2 (prevents silent context corruption — critical for agent reliability)
- **Scope:** OpenClaw MCP plugin, not MPM core. MPM returns, plugin projects.

---

### 🟡 High Impact — Capability Class

**4. Semantic Search (✅ Done)**
- Hybrid BM25 + cosine similarity. Ollama `nomic-embed-text` (768d). Sigmoid-normalized BM25 unbounded scores.
- **Score impact:** +10 (score: 81/99 post-semantic)
- **Scope:** `internal/embeddings.go`, `internal/hybrid_search.go`, `cmd/mpm/recall.go`

**5. Embedding Backfill Pipeline (✅ Done)**
- `mpm ops backfill-embeddings`: batched, resilient, resume-safe. Auto-embed on `mpm add` and all watcher ingest paths.
- **Score impact:** already folded into semantic search score

**6. Memory Versioning**
- Decisions and theories change. The audit trail must be traceable.
- `mpm history <id>`, `mpm diff <id> <v1> <v2>`
- **Score impact:** +1 (epistemology integrity — 808 can audit its own reasoning)
- **Scope:** `internal/history.go`, `mpm history/diff` commands

**7. Feedback-Driven Weight Adjustment**
- `mpm +<id>` / `mpm -<id>` — lightweight feedback, weight adjusts, system learns from interaction.
- **Score impact:** +1
- **Scope:** `cmd/mpm/handlers.go` feedback handlers, weight adjustment logic

**8. Proactive Suggestion Engine**
- Not reactive. Cron-driven: "you haven't looked at X in a while" — stale but important memories, pending theories.
- **Score impact:** +1 (shifts MPM from tool to proactive partner)
- **Scope:** `mpm cron suggest`, `internal/suggestion_engine.go`, MCP tool `proactive_suggest`

---

### 🟢 Ecosystem — Unlock Scale

**9. External Import Pipeline**
- Readwise, Pocket, Raindrop, Notion — memories go to die in these. Structured ingest normalizes bookmarks/articles/notes with source attribution.
- **Score impact:** +1
- **Scope:** `cmd/mpm/ingest/` package, `mpm ops ingest` commands

**10. Plugin Architecture**
- Third-party tools register command handlers, MCP tools, memory event subscriptions.
- **Score impact:** +1
- **Scope:** `internal/plugin.go`, `docs/PLUGIN_API.md`

**11. Shared Memory Across Agents**
- `mpm share <id> --scope <team|project>` — multi-agent workflows, opt-in shared layer.
- **Score impact:** +1
- **Scope:** `internal/shared.go`, `mpm share` command

---

## Score Projection (Machine-to-Machine)

| Item | Status | Points Gained | Running Total |
|------|--------|--------------|---------------|
| Semantic Search + Backfill | ✅ Done | +10 | 81 |
| Watcher/Synth Isolation + DLQ | 🔴 In Progress | +3 | 84 |
| Multi-vendor Synth Failover | 🔴 Next | +2 | 86 |
| Token Budgeting (MCP layer) | 🟡 Planned | +2 | 88 |
| Memory Versioning | 🟡 Pending | +1 | 89 |
| Feedback-Driven Weights | 🟡 Pending | +1 | 90 |
| Proactive Suggestion | 🟢 Pending | +1 | 91 |
| External Import | 🟢 Pending | +1 | 92 |
| Plugin Architecture | 🟢 Pending | +1 | 93 |
| Shared Memory | 🟢 Pending | +1 | 94 |

**Target: 90/99** — achievable once Isolation, Multi-vendor, and Token Budgeting ship.

---

## Execution Order

1. **Watcher/Synth Isolation + DLQ** — existential for agent loop stability
2. **Multi-vendor Synth Failover** — hardened vendor chain, isolation makes this safe to implement
3. **Token Budgeting** — MCP tool layer, not MPM core
4. **Memory Versioning** — epistemology audit trail
5. **Feedback-Driven Weights** — interaction learning loop
6. **Proactive Suggestion** — pre-emptive not reactive
7. **External Import** — ecosystem unlock
8. **Plugin Architecture** — platform scale
9. **Shared Memory** — multi-agent

---

## What NOT to Add (Revised)

- **Visual Graph** — dropped. A cognitive agent doesn't browse a graph. CLI + structured output is sufficient.
- **Dashboard** — dropped. Text/status output is adequate for machine consumers.
- **Mobile UI** — scope creep, wrong platform
- **Cloud sync** — contradicts single-binary zero-dependency philosophy
- **AI summarization of memories** — adds vendor dependency without solving the retrieval problem

---

## Architecture Notes

### Watcher/Synth Select Pattern

```go
// synthesisWorker runs with isolated context, independent SQLite read connection
func synthesisWorker(events <-chan MemoryEvent, shutdown <-chan struct{}, dlq *DLQ) {
    for {
        select {
        case event := <-events:
            // Independent goroutine per event, deadline propagated
            go func(e MemoryEvent) {
                if err := synthWithFailover(e); err != nil {
                    dlq.Enqueue(e, err) // Route to DLQ, never block
                }
            }(event)

        case <-shutdown:
            // Graceful drain: process remaining events before exit
            drainChan(events)
            return

        case <-time.NewTicker(dlqRetryInterval).C:
            dlq.ProcessRetry() // Background DLQ retry when vendors recover
        }
    }
}

func drainChan(events <-chan MemoryEvent) {
    for {
        select {
        case event := <-events:
            synthWithFailover(event) // Process remaining, drop failures
        default:
            return
        }
    }
}
```

### DLQ Schema

```sql
CREATE TABLE IF NOT EXISTS synthesis_dlq (
    id          TEXT PRIMARY KEY,
    memory_id   TEXT NOT NULL,
    content     TEXT NOT NULL,
    tags        TEXT,           -- JSON array
    attempt     INTEGER DEFAULT 0,
    last_error  TEXT,
    created_at  TEXT DEFAULT (datetime('now')),
    next_retry  TEXT            -- ISO8601, updated after each attempt
);
CREATE INDEX IF NOT EXISTS idx_dlq_next_retry ON synthesis_dlq(next_retry);
```

### Multi-Vendor Fallback Chain

```
MiniMax → OpenAI → Ollama (local) → DLQ
```

Each vendor has independent timeout (10s). All vendors fail → DLQ enqueue, synthesis returns `nil` (no panic, no stall). DLQ processed on retry tick (every 5min).

### Token Budget (MCP Layer)

```
HybridSearchResults → tiktoken count → pack to budget → truncate → JSON → agent
```

Budget is a MCP tool config value (default: 8k tokens). 808's context window projection handled in the OpenClaw plugin, not MPM core.