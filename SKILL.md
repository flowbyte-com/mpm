---
name: mpm
description: FlowByte MPM v3.3. SQLite-based agent memory with FTS search. Manage personas, modes, and persistent knowledge via semantic queries.
---

# MPM (Memory-Persona-Mode) v3.3

## 🧠 Memory Database

**Location:** `memory/memory.db`

### Schema Overview

| Table | Purpose |
|-------|---------|
| `sessions` | Conversation sessions |
| `messages` | Individual messages |
| `memory_items` | Extracted facts, decisions, tasks, insights |
| `entities` | People, projects, organizations, tools |
| `topics` | Auto-extracted conversation themes |
| `relations` | Links between items |
| `memory_fts` | Full-text search index (virtual) |
| `search_log` | Query tracking for optimization |

### Quick Queries

```sql
-- Search memories by content
SELECT * FROM memory_fts WHERE memory_fts MATCH 'search term';

-- Recent decisions
SELECT * FROM memory_items WHERE item_type='decision' ORDER BY created_at DESC LIMIT 10;

-- Get all entities of type 'person'
SELECT * FROM entities WHERE entity_type='person';

-- Memories related to a topic (via relations)
SELECT m.* FROM memory_items m
JOIN relations r ON r.source_type='memory' AND r.source_id=m.id
WHERE r.target_type='topic' AND r.target_id=1;
```

## 🎭 Persona (~p)
- **Selection:** `~p.[name]!act`
- **Purpose:** Identity/Voice override

## 🛠️ Mode (~m)
- **Stacking:** `~m.[name]&[name]!act`
- **Purpose:** Layer task-specific behaviors

## 🚦 System State
Check `active.json` or run `db status` for current state.

## 🔄 Session Management

### Manual Save Session
When sessions aren't auto-saved, use `mpm ss`:  
**Usage:** `~m.mpm!ss` or `./scripts/save-session.sh`

```bash
#!/bin/bash
# Save current session manually
# Extracts memories, entities, topics from context

sqlite3 memory/memory.db <<'SQL'
-- Insert session
INSERT INTO sessions (id, started_at) VALUES (datetime('now'), datetime('now'));

-- Messages, items, entities extracted by agent
-- Then: ./memory/analyze-session.py <session_id>
SQL
```

## Migration
- Old category files moved to `memory/archive/`
- All data migrated to SQLite
- FTS5 enabled for semantic search
