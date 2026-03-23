# MPM Quick Reference

## Session Commands

| Command | Description | Example |
|---------|-------------|---------|
| `~m.mpm!ss <context>` | Save current session | `~m.mpm!ss "refactoring database schema"` |
| `~m.mpm!analyze <id> [ctx]` | Analyze session content | `~m.mpm!analyze 1 "we moved to SQLite"` |
| `~m.mpm!import <file>` | Import memories from JSON | `~m.mpm!import export_202403.json` |

## Shell Scripts

```bash
# Save manual session
./scripts/save-session.sh "what we did" "channel" 4

# Analyze with full context
./scripts/analyze-session.py 1 "Full conversation context here"

# Search memories
./scripts/search-memories.sh "database schema"

# List recent sessions
sqlite3 memory/memory.db "SELECT id, session_key, summary FROM sessions ORDER BY id DESC LIMIT 10;"

# View session details
sqlite3 memory/memory.db "SELECT * FROM sessions WHERE id = 1;"
```

## SQL Queries

```sql
-- Recent memory items
SELECT item_type, summary, created_at FROM memory_items ORDER BY created_at DESC LIMIT 5;

-- Most mentioned entities
SELECT name, entity_type, mention_count FROM entities ORDER BY mention_count DESC;

-- Active topics
SELECT topic_name, mention_count FROM topics ORDER BY last_active DESC;

-- Memory with related entities
SELECT m.summary, e.name, e.entity_type
FROM memory_items m
JOIN relations r ON r.source_type = 'memory' AND r.source_id = m.id
JOIN entities e ON r.target_type = 'entity' AND r.target_id = e.id
WHERE m.item_type = 'decision';
```

## Migration Note

```bash
# Old v1 files (.md) → New v3.3 (.db)
# Already completed for existing memories
# New sessions auto-captured in SQLite

# Verify migration
sqlite3 memory/memory.db <<SQL
SELECT 
    (SELECT COUNT(*) FROM sessions) AS sessions,
    (SELECT COUNT(*) FROM memory_items) AS memories,
    (SELECT COUNT(*) FROM entities) AS entities,
    (SELECT COUNT(*) FROM topics) AS topics;
SQL
```
