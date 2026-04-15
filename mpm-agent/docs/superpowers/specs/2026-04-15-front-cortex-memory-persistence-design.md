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

### 2. Session summary row

Add a separate `session_summaries` table keyed by `session_id`:

```sql
CREATE TABLE session_summaries (
    session_id TEXT PRIMARY KEY,
    summary TEXT,           -- "Discussed persistence, tested OpenRouter"
    last_topic TEXT,
    created_at TEXT,
    updated_at TEXT
);

CREATE TABLE identity_knowledge (
    id TEXT PRIMARY KEY,
    key TEXT UNIQUE,        -- "user_name", "current_project", "active_work"
    value TEXT,
    updated_at TEXT
);
```

After each session (or after significant exchanges), generate a one-line summary:

- "User tested tool streaming with OpenRouter, confirmed it works"
- "Discussed memory persistence limitations, scheduled fix for 2026-04-15"

### 3. Front cortex context loader

On every `RunAgent` call, build a `frontCortex` struct containing:

```go
type frontCortex struct {
    userName     string    // from identity_knowledge
    currentProject string   // from identity_knowledge
    recentTopics  []string // last 5 session summaries
    activeWork    string    // last_topic from most recent session
    anchors       []Anchor // last 10 anchors (not 5)
    recentLessons []Lesson // last 3 lessons
}
```

Injected into system prompt before anchors section:

```
## Who I'm talking to
- User: [userName]
- Current project: [currentProject]
- Active work: [activeWork]

## Recent context
- [topic 1]
- [topic 2]
- ...

## Anchored Memories
...
```

### 4. Identity knowledge injection

`IDENTITY.md` currently only describes the bot. Add a companion `IDENTITY_USER.md` or store in `identity_knowledge` table:

- `user_name`: "V"
- `current_project`: "MPM agent development"
- `preferences`: "likes concise responses, OpenRouter over MiniMax"
- `active_work`: "implementing front cortex memory persistence"

The agent loads this on every call. If empty, it learns from conversation over time (via anchors).

### 5. Lessons retrieval

Add `GetRecentLessons(db, 3)` call in `RunAgent` and inject into system prompt under "Learned patterns":

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
    summary TEXT NOT NULL,
    last_topic TEXT,
    message_count INTEGER DEFAULT 0,
    created_at TEXT,
    updated_at TEXT
);

CREATE TABLE identity_knowledge (
    id TEXT PRIMARY KEY,
    key TEXT UNIQUE NOT NULL,
    value TEXT NOT NULL,
    source TEXT DEFAULT 'learned',  -- 'explicit' if set by user
    updated_at TEXT
);

-- Update anchors to include all sessions (not just last 5)
-- Query: ORDER BY created_at DESC LIMIT 10 instead of 5

-- New function in session.go:
func (sm *SessionManager) UpdateSessionSummary(chatID int64, summary string, topic string) error
```

---

## File Map

| File | Changes |
|------|---------|
| `core/agent.go` | Add `loadFrontCortex()` function, call it in `RunAgent`, inject into system prompt |
| `cmd/telegram/session.go` | Fix `Save()` to merge, add `UpdateSessionSummary()`, add `GetSessionSummaries()` |
| `cmd/telegram/handler.go` | After agent completes, call `UpdateSessionSummary()` with generated summary |
| `core/selfimprove.go` | Add `GetRecentLessons()` call site in agent.go context builder |
| `core/anchors.go` | Increase anchor retrieval limit from 5 to 10 |
| `core/db.go` | Add `InitFrontCortexTables()` for new tables |

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
6. **Front cortex context builder** — wires everything together

---

## Notes

- The session summary generation can be simple: last user message + last assistant message topic, truncated to 80 chars
- Identity knowledge can be seeded from anchors: if user says "I'm V working on MPM", that's a weight-2 anchor that gets promoted to identity_knowledge
- No need to store full history from old sessions — the summary + anchors + lessons provide sufficient context for the agent to reason about continuity