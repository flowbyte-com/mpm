# MPM Memory Database v3.3

**File:** `memory/memory.db` (SQLite)

## Schema

### Core Tables

#### sessions
Conversation sessions with metadata.

| Field | Type | Notes |
|-------|------|-------|
| id | INTEGER PK | Auto |
| session_key | TEXT UNIQUE | e.g., "cli_20250615_abc123" |
| channel | TEXT | cli, telegram, etc. |
| start_time | TIMESTAMP | |
| end_time | TIMESTAMP | |
| summary | TEXT | Auto-generated |
| topic_tags | TEXT | Comma-separated |
| importance | INTEGER | 0-10 |

#### messages
Individual messages in sessions.

| Field | Type | Notes |
|-------|------|-------|
| id | INTEGER PK | Auto |
| session_id | INTEGER FK | -> sessions |
| role | TEXT | user/assistant/system/tool |
| content | TEXT | |
| timestamp | TIMESTAMP | |
| tool_calls | TEXT | JSON format |

#### memory_items
Extracted facts, decisions, tasks, preferences, insights.

| Field | Type | Notes |
|-------|------|-------|
| id | INTEGER PK | Auto |
| item_type | TEXT | fact/decision/task/preference/entity/insight |
| content | TEXT | Full text |
| summary | TEXT | One-liner for LLM context |
| source_session_id | INTEGER FK | -> sessions |
| confidence | REAL | 0.0-1.0 extraction confidence |
| created_at | TIMESTAMP | |
| expires_at | TIMESTAMP | NULL = permanent |
| access_count | INTEGER | Usage tracking |
| last_accessed | TIMESTAMP | |

#### entities
People, projects, organizations, tools, concepts, locations.

| Field | Type | Notes |
|-------|------|-------|
| id | INTEGER PK | Auto |
| entity_type | TEXT | person/project/organization/concept/tool/location |
| name | TEXT | |
| aliases | TEXT | Comma-separated alternate names |
| description | TEXT | |
| first_seen | TIMESTAMP | |
| last_mentioned | TIMESTAMP | |
| mention_count | INTEGER | |

#### topics
Auto-extracted themes.

| Field | Type | Notes |
|-------|------|-------|
| id | INTEGER PK | Auto |
| topic_name | TEXT UNIQUE | |
| keywords | TEXT | |
| first_seen | TIMESTAMP | |
| last_active | TIMESTAMP | |
| mention_count | INTEGER | |
| parent_topic_id | INTEGER FK | Self-reference for hierarchy |

#### relations
Link any items to any other items.

| Field | Type | Notes |
|-------|------|-------|
| id | INTEGER PK | Auto |
| source_type | TEXT | memory/entity/topic/session |
| source_id | INTEGER | |
| relation_type | TEXT | mentions/part_of/related_to/depends_on |
| target_type | TEXT | memory/entity/topic/session |
| target_id | INTEGER | |
| strength | REAL | 0.0-1.0 |
| created_at | TIMESTAMP | |

## Full-Text Search

```bash
# Search memory content
cd ~/.picoclaw/workspace/mpm/memory
sqlite3 memory.db "SELECT * FROM memory_fts WHERE memory_fts MATCH 'database';"

# Search messages
sqlite3 memory.db "SELECT * FROM message_fts WHERE message_fts MATCH 'error';"
```

## Migration Status
- ✅ Migrated 15 entries from v1 category system
- ✅ Dropped old tables (memory_entries, memory_meta)
- ✅ Enabled FTS5 for semantic search

## Backup
- `memory.db.v1.bak` - Original v1 database (pre-migration)
