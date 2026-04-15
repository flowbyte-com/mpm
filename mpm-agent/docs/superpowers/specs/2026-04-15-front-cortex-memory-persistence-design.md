# Mini-Bot Front Cortex — Memory Persistence Design

**Date:** 2026-04-15
**Goal:** Make mini-bot persist conversation context, user identity, and recent work across sessions — so on restart or new conversation it knows who you are and what you were doing.

---

## Problem Statement

Current mini-bot session flow:

1. **Session history** — `Save()` does `ON CONFLICT DO UPDATE` which **replaces** existing history, not appends. Only the last exchange survives. Prior sessions are permanently lost.

2. **Anchors** — correctly written on substantial messages and injected into system prompt, but limited to last 5 anchors. Days-old context is invisible.

3. **Lessons** — 72 rows written by `selfImprove` but `RunAgent` never reads them. Knowledge gained is never reused.

4. **IDENTITY.md** — only describes the bot, contains nothing about the user, current project, or ongoing work.

5. **No cross-session summary** — when you message after a restart, the agent has no idea what was discussed previously.

**Result:** "morning mini-bot" starts a completely fresh conversation. The bot doesn't know who you are, what you were working on, or anything from prior sessions.

---

## Architecture: Three Memory Layers

```
Layer 1: SESSION HISTORY (short-term, per-chat)
    └── sm.Get() / sm.Save() — last 50 messages, merged (not replaced)
    └── Loaded on every message — provides conversation continuity

Layer 2: FRONT CORTEX (persistent working memory)
    └── Session summary: what was discussed in recent sessions
    └── Identity info: who is the user, current project, preferences
    └── Recent anchors: last N anchors across all sessions (not just last 5)
    └── Injected into system prompt on every call — always available

Layer 3: LONG-TERM MEMORIES (query-activated)
    └── Lessons: GetRecentLessons() — learned patterns, already stored
    └── Memories: FTS5 search matching current query
    └── Directives: prime directives, no filter
    └── References: relevant docs, query-activated
```

---

## Key Design Decisions

### 1. Session save — merge not replace

**Current:** `ON CONFLICT(id) DO UPDATE SET metadata=...` — replaces all history with latest exchange

**Fixed:** Load existing history → merge with new messages → save merged result. Keep last 50 messages.

```go
func (sm *SessionManager) Save(chatID int64, messages []map[string]interface{}) error {
    existing, _ := sm.Get(chatID)
    merged := append(existing, messages...)
    // Keep sliding window of last 50
    if len(merged) > 50 {
        merged = merged[len(merged)-50:]
    }
    // ... serialize and save
}
```

### 2. Session summary — idle-timer goroutine with lightweight summarization

**Why not simple truncation:** If a session has 40 messages of deep debugging and the final exchange is "Great, thanks, I'll commit that now", truncating to last message gives a useless summary. We need the whole session distilled.

**Pattern:** Same as `selfImprove` — goroutine fires after handler returns, non-blocking.

1. After `RunAgent` completes, spawn `go summarizeSession(chatID, messages)` if session has 10+ messages.
2. Start a 5-minute idle timer per chat.
3. If user messages before timer fires → reset timer.
4. On idle timeout → use lightweight model to summarize the full session buffer into one line.
5. On `/new` or `/clear` → cancel any pending summary goroutine for that chat.

**Summary model:** Use a fast cheap model (e.g., `gemma4:31b` or `qwen3-coder-next:cloud`) via the same OpenRouter API used by the main agent. Cost is minimal — one call per idle session.

### 3. Front cortex context loader with hard caps

On every `RunAgent` call, build a `frontCortex` struct:

```go
type frontCortex struct {
    userName     string    // from identity_knowledge
    currentProject string  // from identity_knowledge
    recentTopics  []string // last 3 session summaries (max)
    activeWork    string    // last_topic from most recent session
    anchors       []Anchor // last 10 anchors (max)
    recentLessons []Lesson // last 3 lessons
}
```

**Hard caps to prevent token bloat** (total front cortex target: ≤600 chars):

| Section | Max items | Max chars |
|---------|-----------|-----------|
| Identity info | 4 keys | 200 |
| Session summaries | 3 | 200 |
| Anchors | 10 | 150 |
| Lessons | 3 | 100 |
| **Total** | — | **~650** |

If adding an item would exceed cap, drop oldest/lowest-weight first. The agent can still access older content via FTS5 memory search.

Injected into system prompt before anchors section:

```
## Who I'm talking to
- User: [userName] | Project: [currentProject]
- Active work: [activeWork]

## Recent context
- [topic 1]
- [topic 2]

## Anchored Memories
...

## Learned from past sessions
...
```

### 4. Identity knowledge — explicit tool, not automatic promotion

`IDENTITY.md` only describes the bot. Identity knowledge about the user lives in `identity_knowledge` table.

**How identity facts get stored:** A dedicated tool `update_identity_knowledge` that the agent calls explicitly when the user provides identity information. Example triggers:

- User says "I'm V" → agent calls `update_identity_knowledge("user_name", "V")`
- User says "working on MPM" → agent calls `update_identity_knowledge("current_project", "MPM")`
- User says "I prefer OpenRouter" → agent calls `update_identity_knowledge("preferences", "OpenRouter over MiniMax")`

**No automatic promotion from anchors.** Anchors are context; identity_knowledge is deliberate. This prevents the system from incorrectly promoting a random conversation topic to identity.

**Tool signature:**
```go
// update_identity_knowledge stores or updates an identity fact about the user.
update_identity_knowledge(key: string, value: string, source: string)
// key: "user_name" | "current_project" | "active_work" | "preferences" | "constraints"
// value: the fact
// source: "explicit" (user stated it) | "inferred" (agent deduced it, requires confirmation)
```

### 5. Lessons retrieval

Add `GetRecentLessons(db, 3)` call in `RunAgent` and inject into system prompt under "Learned from past sessions":

```
## Learned from past sessions
- [lesson 1]
- [lesson 2]
```

---

## Database Schema Changes

```sql
-- New tables:

CREATE TABLE session_summaries (
    session_id TEXT PRIMARY KEY,
    summary TEXT NOT NULL,           -- "Fixed OpenRouter endpoint, confirmed streaming works"
    last_topic TEXT,
    message_count INTEGER DEFAULT 0,
    created_at TEXT,
    updated_at TEXT
);

CREATE TABLE identity_knowledge (
    id TEXT PRIMARY KEY,
    key TEXT UNIQUE NOT NULL,       -- "user_name", "current_project", "active_work", "preferences"
    value TEXT NOT NULL,
    source TEXT DEFAULT 'inferred', -- 'explicit' if user stated it, 'inferred' if agent deduced
    updated_at TEXT
);

-- New function in session.go:
func (sm *SessionManager) UpdateSessionSummary(chatID int64, summary string, topic string) error
func (sm *SessionManager) GetSessionSummaries(chatID int64, limit int) ([]SessionSummary, error)
func (sm *SessionManager) SetIdentityKnowledge(key, value, source string) error
func (sm *SessionManager) GetIdentityKnowledge(key string) (string, error)
func (sm *SessionManager) GetAllIdentityKnowledge() (map[string]string, error)
```

---

## File Map

| File | Changes |
|------|---------|
| `core/agent.go` | Add `loadFrontCortex()` function, call it in `RunAgent`, inject into system prompt; add `update_identity_knowledge` tool |
| `cmd/telegram/session.go` | Fix `Save()` to merge, add session summary functions, identity knowledge functions |
| `cmd/telegram/handler.go` | Spawn idle-timer summarization goroutine after agent completes; cancel on `/new`/`/clear` |
| `core/selfimprove.go` | Add `GetRecentLessons()` call site in front cortex builder |
| `core/anchors.go` | Increase anchor retrieval limit from 5 to 10 |
| `core/db.go` | Add `InitFrontCortexTables()` for new tables |
| `core/tools.go` | Add `update_identity_knowledge` to tool list |

---

## What the user experiences

**Before (broken):**
```
User: morning mini-bot
Bot: [completely fresh, no idea who you are or what was discussed]
```

**After (fixed):**
```
User: morning mini-bot
Bot: [loads front cortex]
   "Good morning V! Ready to continue with the front cortex implementation?
    Last session we confirmed OpenRouter streaming works. Still on for that?"
```

**On restart:**
```
Bot knows: user is V, prefers OpenRouter, was implementing tool streaming,
previous session confirmed it works, 72 lessons learned, anchors from 2 days ago still present.
```

---

## Implementation Priority

1. **Fix `Save()` merge** — critical, stops data loss right now
2. **Add `session_summaries` table + `UpdateSessionSummary()`** — enables "what were we discussing"
3. **Increase anchor limit to 10** — easy, immediate improvement
4. **Add lessons to system prompt** — uses existing `GetRecentLessons()`
5. **Add `identity_knowledge` table** — "who am I talking to"
6. **Add `update_identity_knowledge` tool** — agent explicitly records identity facts
7. **Front cortex context builder** — wires everything together with hard caps
8. **Idle-timer summarization goroutine** — summarize full session after 5 min idle

---

## Notes

- Summary generation fires after 5 min idle — non-blocking goroutine, same pattern as `selfImprove`
- Identity knowledge promotion is **explicit only** — no automatic anchor-to-identity promotion
- Hard caps (≤600 chars front cortex) prevent context window bloat while keeping recent context available
- Session summaries provide cross-session continuity without storing full history
- Cancel pending summary on `/new` or `/clear` — stale summaries from a cleared session are useless