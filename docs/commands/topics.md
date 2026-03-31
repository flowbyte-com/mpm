# Topic Commands

> 📚 **Semantic groupings.** Topics organize related memories and sessions for curated knowledge management.

## Overview

Topics organize related memories and sessions into semantic groupings. Topics can be created from any memory source and promoted to memory store after verification.

### The Topic Lifecycle

```
┌────────────────────────────────────────────────────────────────────────┐
│                      TOPIC SYSTEM LIFECYCLE                            │
├────────────────────────────────────────────────────────────────────────┤
│                                                                        │
│   1. DISTILLATION (Session → Topic)                                    │
│      Session hits token limit or concept shift                         │
│      → AI summarizes key points                                        │
│      → New Topic created with provenance                              │
│                                                                        │
│   2. HARDENING (Topic → Memory)                                        │
│      Topic verified 3+ times in queries                                │
│      → Topic promoted to permanent Memory                              │
│      → Provenance preserved (topic → memory)                          │
│                                                                        │
│   3. RETRIEVAL (Query → Multi-stage Search)                            │
│      Search Sessions → Topics → Memory → References                    │
│      → Best matches returned with source attribution                  │
│                                                                        │
└────────────────────────────────────────────────────────────────────────┘
```

## CLI Commands

### List Topics
```bash
mpm topic list [--limit <n>]
```

Lists all topics with member counts.

**Example output:**
```
$ mpm topic list --limit 3

┌─────────────────────────────────────────────────────────────┐
│ Topics                                                       │
├─────────────────────────────────────────────────────────────┤
│ ID: top_abc123 | Docker Best Practices | 5 members        │
│ ID: top_def456 | GitHub Actions Workflows | 3 members      │
│ ID: top_ghi789 | API Design Patterns | 8 members           │
└─────────────────────────────────────────────────────────────┘
```

### Show Topic Details
```bash
mpm topic show <id>
```

Shows topic details with all members.

**Example output:**
```
$ mpm topic show top_abc123

┌─────────────────────────────────────────────────────────────┐
│ Topic: Docker Best Practices                                 │
├─────────────────────────────────────────────────────────────┤
│ ID:           top_abc123                                   │
│ Description:  Container best practices and patterns       │
│ Source:       session (sess_xyz789)                        │
│ Created:      2026-03-28T10:00:00Z                        │
│ Updated:      2026-03-29T14:30:00Z                        │
├─────────────────────────────────────────────────────────────┤
│ Members (5):                                               │
│   • mem_001 | Container naming conventions                │
│   • mem_002 | Docker networking best practices             │
│   • mem_003 | Multi-stage builds                          │
│   • mem_004 | Volume mounting strategies                 │
│   • mem_005 | Resource limits and quotas                  │
└─────────────────────────────────────────────────────────────┘
```

### Create Topic
```bash
mpm topic add
```

Interactive creation. Prompts for:

| Prompt | Description |
|--------|-------------|
| Source type | `memory`, `session`, or `reference` |
| Source ID | The original record ID |
| Title | Topic title |
| Description | Brief description |

### Delete Topic (Soft)
```bash
mpm topic delete <id>
```

Prompts for confirmation. Uses **soft delete** (sets `is_active = false`).

### Shred Topic (Hard Delete)
```bash
mpm topic shred <id>
```

True irreversible erasure:
1. Deletes all `topic_memberships` entries
2. Deletes the topic
3. Runs `VACUUM`
4. Verification confirms deletion

> 🔒 **Cascading:** Shredding a topic also removes all membership links. See [Shred Protocol](../advanced/shred-protocol.md).

### List Topic Members
```bash
mpm topic members <id>
```

Lists all memories/sessions linked to a topic.

### Search Topics
```bash
mpm topic search <query> [--limit <n>]
```

Performs FTS5 search across topic titles and descriptions.

---

## Topic Schema

### Topics Table

| Column | Type | Description |
|--------|------|-------------|
| `id` | TEXT | Unique identifier |
| `topic` | TEXT | Topic title |
| `description` | TEXT | Topic description |
| `source_type` | TEXT | Source: `memory`, `session`, `reference` |
| `source_id` | TEXT | Original source database ID |
| `tags` | JSON | Array of tag strings |
| `created_at` | DATETIME | Creation timestamp |
| `updated_at` | DATETIME | Last update timestamp |
| `is_active` | BOOLEAN | Soft delete flag |

### Topic Memberships Table

| Column | Type | Description |
|--------|------|-------------|
| `topic_id` | TEXT | Foreign key to topics |
| `memory_id` | TEXT | Foreign key to memories |
| `added_at` | DATETIME | When memory was added |

---

## Provenance Tracking

Every topic retains its origin:

```json
{
  "id": "top_abc123",
  "topic": "Docker Best Practices",
  "source_type": "session",
  "source_id": "sess_xyz789",
  "created_at": "2026-03-28T10:00:00Z"
}
```

**Benefits of provenance:**
- Trace topic back to original session
- Understand context of knowledge
- Verify information sources

---

## Integration

| Component | Integration |
|-----------|------------|
| **Sessions** | Topics distilled from sessions |
| **Memory** | Topics can harden into memory |
| **References** | Topics can be created from documents |
| **FTS5** | Topics have searchable FTS5 table |
| **Shred Protocol** | Hard delete cascades to memberships |

---

## Examples

### Create a Topic
```bash
# Interactive creation
mpm topic add

# Or create from a session
mpm topic add
# Prompts:
#   Source type: session
#   Source ID: sess_abc123
#   Title: Docker Best Practices
#   Description: Container best practices and patterns
```

### Manage Topic Members
```bash
# List members
mpm topic members top_abc123

# View specific topic
mpm topic show top_abc123
```

### Search Topics
```bash
# Search by keyword
mpm topic search "docker"

# Search and limit results
mpm topic search "api design" --limit 5
```

### Delete Topic
```bash
# Soft delete (recoverable)
mpm topic delete top_abc123

# Hard delete (permanent)
mpm topic shred top_abc123
```

---

## Troubleshooting

| Issue | Solution |
|-------|----------|
| `Topic not found` | Check ID with `mpm topic list` |
| `Topic has no members` | Add memories: `mpm memory add --topic <id>` |
| `Can't shred topic` | Check for foreign key locks; try again |
| `Provenance lost` | Check `source_type` and `source_id` columns |

---

**Last Updated:** 2026-03-29  
**MPM Version:** 6.0.0
