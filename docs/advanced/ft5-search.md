# FTS5 Search Guide

> Learn how MPM's full-text search works, including highlighting, ranking, and fallback behavior.

## Overview

MPM implements full-text search using SQLite's **FTS5** (Full-Text Search version 5) module. FTS5 provides:

- **~1-5ms query times** (vs 5-10ms substring fallback)
- **BM25 ranking** — relevance scoring based on term frequency
- **`snippet()` highlighting** — context around matches
- **Trigger-based sync** — FTS5 tables stay in sync automatically

### Why FTS5?

| Approach | Pros | Cons |
|----------|------|------|
| FTS5 (MPM) | Built into SQLite, fast, no external service | Limited to SQLite |
| External search (Elasticsearch) | Scalable, feature-rich | Extra infrastructure, network dependency |
| Substring `LIKE` | No setup | Slow (~5-10ms), poor ranking |

For an embedded AI agent memory system, FTS5 provides the right balance of speed, simplicity, and functionality.

---

## Features

### Highlighting

Search results include highlighted context using the `snippet()` function:

| Setting | Value |
|---------|-------|
| Open tag | `<<` |
| Close tag | `>>` |
| Max tokens | 16 |
| Ellipsis | `...` |

**Example output:**
```
$ mpm memory search "docker containers"

...learn about <<docker>> <<containers>> for orchestration...
```

### Ranking

Results are ordered by **BM25** (Best Matching 25), SQLite's built-in relevance algorithm. BM25 considers:

- Term frequency (how often the term appears)
- Document frequency (how rare the term is)
- Document length (longer documents score lower for same term frequency)

### Fallback

If FTS5 virtual tables are unavailable, MPM falls back to **substring search**:

```sql
SELECT * FROM memories WHERE content LIKE '%query%';
```

> ⚠️ **Limitation:** Fallback search does not support highlighting or BM25 ranking.

---

## Search Commands

### Search Sessions
```bash
mpm session search "docker containers"
```

### Search Topics
```bash
mpm topic search "artificial intelligence"
```

### Search Memories
```bash
mpm memory search "kubernetes deployment"
```

### Pagination

Use `--limit` and `--offset` for pagination:

```bash
mpm memory search "query" --limit 10 --offset 0   # First 10
mpm memory search "query" --limit 10 --offset 10  # Next 10
```

---

## SQL Implementation

### FTS5 Virtual Tables

```sql
-- Memories FTS5
CREATE VIRTUAL TABLE IF NOT EXISTS memories_fts USING fts5(
    content,
    content_rowid=id,
    content_table='memories'
);

-- Sessions FTS5
CREATE VIRTUAL TABLE IF NOT EXISTS sessions_fts USING fts5(
    content,
    content_rowid=id,
    content_table='sessions'
);

-- Topics FTS5
CREATE VIRTUAL TABLE IF NOT EXISTS topics_fts USING fts5(
    name,
    description,
    content_rowid=id,
    content_table='topics'
);
```

### Tokenizer

MPM uses the default `unicode61` tokenizer, which:
- Splits text on Unicode boundaries
- Handles Unicode-aware case folding
- No stemming (exact term matching)

> 💡 **Customization:** For stemming, you can recreate FTS5 tables with the `porter` tokenizer:
> ```sql
> CREATE VIRTUAL TABLE memories_fts USING fts5(content, tokenize='porter');
> ```

### Triggers for Automatic Sync

```sql
-- Trigger to keep FTS5 in sync with memories table
CREATE TRIGGER IF NOT EXISTS memories_ai AFTER INSERT ON memories
BEGIN
    INSERT INTO memories_fts(rowid, content) VALUES (NEW.id, NEW.content);
END;

CREATE TRIGGER IF NOT EXISTS memories_ad AFTER DELETE ON memories
BEGIN
    DELETE FROM memories_fts WHERE rowid = OLD.id;
END;

CREATE TRIGGER IF NOT EXISTS memories_au AFTER UPDATE ON memories
BEGIN
    UPDATE memories_fts SET content = NEW.content WHERE rowid = NEW.id;
END;
```

> ⚠️ **Out-of-Sync Index:** If triggers were not created (older versions), run:
> ```bash
> mpm repair fts   # Rebuild FTS5 indexes
> ```

### Search Query with Snippet

```sql
SELECT 
    id, 
    content, 
    snippet(memories, content, '<<', '>>', '...', 16) AS highlighted,
    created_at
FROM memories
WHERE content MATCH ?
ORDER BY rank
LIMIT ? OFFSET ?
```

---

## Performance

| Operation | Time | Notes |
|-----------|------|-------|
| FTS5 Search | ~1-5ms | BM25 ranking included |
| Substring Fallback | ~5-10ms | No ranking |
| FTS5 Index Size | ~2-3x data | Depends on content |
| VACUUM (post-delete) | ~100-500ms | Shred operation |

### Optimization Tips

1. **Rebuild indexes periodically:**
   ```bash
   sqlite3 mpm_memory.db "INSERT INTO memories_fts(memories) VALUES('optimize');"
   ```

2. **Use appropriate chunk sizes:** Large documents are chunked. Smaller chunks = more precise search.

3. **Index frequently searched fields:** Only `content` is indexed by default. Metadata fields (`tags`, `collection`) use substring search.

---

## Security and Privacy

### Sensitive Content in Search

The sensitive content blocking (`isSensitiveContent()`) runs **before** content reaches FTS5 tables. Search results will never contain blocked patterns because they never entered the system.

### Audit Log

Blocked attempts are logged to `mirror.jsonl`:

```json
{
  "timestamp": "2026-03-29T06:30:00Z",
  "reason": "OpenAI API Key",
  "action": "blocked",
  "type": "sensitive_attempt"
}
```

---

## Troubleshooting

| Issue | Cause | Solution |
|-------|-------|----------|
| No search results | FTS5 table missing | `mpm repair fts` to rebuild |
| Slow queries | Large dataset, no indexes | Run `INSERT INTO ... VALUES('optimize')` |
| Highlighting missing | Using fallback | Check `mpm status` for FTS5 health |

---

**Last Updated:** 2026-03-29  
**MPM Version:** 6.0.0
