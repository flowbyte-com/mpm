# Memory Commands

> 💾 **Unified memory store.** MPM provides persistent semantic memory with FTS5 search and vector search.

## Overview

MPM's memory system stores all agent knowledge in a single SQLite database:

- **Memories** — Distilled, hardened knowledge
- **Sessions** — Temporary conversation history
- **Topics** — Curated knowledge groupings
- **References** — Ingested documents

All tiers support full-text search via FTS5 and the shred protocol for hard delete.

## CLI Commands

### List Memories
```bash
mpm memory list [--limit <n>] [--offset <n>]
```

Lists all memories with pagination.

**Example output:**
```
$ mpm memory list --limit 2

ID: mem_abc123 | Collection: memories | Created: 2026-03-28
Content: "Docker containers provide isolation..."

ID: mem_def456 | Collection: memories | Created: 2026-03-27
Content: "GitHub Actions workflow..."
```

### Add Memory
```bash
mpm memory add <content> [--tags <tags>]
```

Adds a new memory. Content is scanned for sensitive data before storage.

**Options:**
| Option | Description |
|--------|-------------|
| `<content>` | Memory text content |
| `--tags` | Comma-separated tags (e.g., `--tags "docker,devops"`) |

**Example:**
```bash
mpm memory add "Docker is a containerization platform" --tags "docker,containers"
```

### Search Memories
```bash
mpm memory search <query> [--limit <n>]
```

Performs FTS5 search with highlighting. Uses BM25 ranking and `snippet()` for context.

**Example:**
```bash
$ mpm memory search "Docker containers"

┌─────────────────────────────────────────────────────────────┐
│ Query: "Docker containers"                                  │
├─────────────────────────────────────────────────────────────┤
│ mem_abc123 | Score: -1.234                                  │
│ ...learn about <<Docker>> <<containers>> for orchestration │
└─────────────────────────────────────────────────────────────┘
```

### Delete Memory (Soft)
```bash
mpm memory delete <id>
```

Soft delete — sets `is_active = false`. Memory remains in database but hidden from queries.

### Shred Memory (Hard Delete)
```bash
mpm memory shred <id>
```

**True irreversible erasure:**
1. `DELETE FROM memories WHERE id = ?`
2. `DELETE FROM memories_fts WHERE rowid = ?`
3. `VACUUM` — rewrites database file
4. Verification query confirms deletion

> 🔒 **This cannot be undone.** See [Shred Protocol](../advanced/shred-protocol.md) for details.

### Statistics
```bash
mpm memory stats
```

Shows system statistics.

**Example output:**
```
$ mpm memory stats

┌─────────────────────────────────────────────────────────────┐
│ Memory Statistics                                           │
├─────────────────────────────────────────────────────────────┤
│ Total Memories:     42                                     │
│ Active Memories:    38                                     │
│ Soft Deleted:        4                                     │
│ FTS5 Status:       OK                                      │
│ Firewall Hits:      12 (blocked attempts)                  │
└─────────────────────────────────────────────────────────────┘
```

---

## Memory Schema

### Core Columns

| Column | Type | Description |
|--------|------|-------------|
| `id` | TEXT | Unique identifier (UUID) |
| `collection` | TEXT | Source: `memories`, `sessions`, `topics` |
| `content` | TEXT | The actual memory content |
| `session_id` | TEXT | Parent session ID (if applicable) |
| `tags` | JSON | Array of tag strings |
| `metadata` | JSON | Additional metadata |
| `embedding` | JSON | Vector embedding (32-dim float64) |
| `created_at` | DATETIME | Creation timestamp (RFC3339) |
| `is_active` | BOOLEAN | Soft delete flag (default: true) |

### FTS5 Virtual Table

| Column | Description |
|--------|-------------|
| `content` | Copy of memory content for full-text search |

---

## FTS5 Search

All memory tiers support full-text search:

| Feature | Description |
|---------|-------------|
| Highlighting | `snippet()` with `<<` and `>>` tags |
| Ranking | BM25 relevance scoring |
| Fallback | Substring search if FTS5 unavailable |
| Speed | ~1-5ms for typical queries |

> 📖 See [FTS5 Search Guide](../advanced/ft5-search.md) for implementation details.

---

## Shred Protocol

True hard delete versus soft delete:

| Operation | Command | Effect | Reversible? |
|----------|---------|--------|-------------|
| Soft Delete | `mpm memory delete <id>` | Sets `is_active = false` | ✅ (can restore) |
| Hard Delete | `mpm memory shred <id>` | `DELETE` + `VACUUM` | ❌ (gone forever) |

> 🔒 See [Shred Protocol](../advanced/shred-protocol.md) for GDPR/CCPA compliance details.

---

## Examples

### Add a Memory
```bash
mpm memory add "PostgreSQL uses MVCC for concurrency control" \
  --tags "database,postgres,sql"
```

### Search and View
```bash
# Search for memories about Docker
mpm memory search "docker"

# View specific memory
mpm memory show mem_abc123
```

### Bulk Operations
```bash
# List first 50 memories
mpm memory list --limit 50

# Search with pagination
mpm memory search "kubernetes" --limit 20 --offset 0   # First 20
mpm memory search "kubernetes" --limit 20 --offset 20  # Next 20
```

### Delete Old Memories
```bash
# Soft delete (recoverable)
mpm memory delete mem_old123

# Hard delete (permanent)
mpm memory shred mem_old456
```

---

## Troubleshooting

| Issue | Solution |
|-------|----------|
| `Sensitive content blocked` | Content matched a secret pattern; remove secrets before storing |
| `Memory not found` | Check ID; may have been soft-deleted (`mpm memory list --all`) |
| `Search returns no results` | Try different keywords; FTS5 requires exact term matching |
| `Slow search` | Run `mpm repair fts` to optimize FTS5 indexes |

---

**Last Updated:** 2026-03-29  
**MPM Version:** 6.0.0
