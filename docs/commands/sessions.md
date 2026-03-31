# Session Commands

> 💬 **Conversation history.** Sessions track individual conversations with full metadata and FTS5 search.

## Overview

Sessions represent individual conversation instances. They are stored in SQLite with FTS5 search support and can be shredded for permanent deletion.

### Session Lifecycle

```
┌────────────────────────────────────────────────────────────────────────┐
│                         SESSION LIFECYCLE                               │
├────────────────────────────────────────────────────────────────────────┤
│                                                                        │
│   ┌──────────────┐                                                      │
│   │   START      │─── Create session, record start_time                │
│   └──────┬───────┘                                                      │
│          │                                                               │
│          ▼ Active conversation (messages added)                         │
│   ┌──────────────┐                                                      │
│   │   ACTIVE     │─── May distill to Topic if token limit hit          │
│   └──────┬───────┘                                                      │
│          │                                                               │
│          ▼ User ends or idle timeout                                    │
│   ┌──────────────┐                                                      │
│   │   END        │─── Record end_time, create summary                  │
│   └──────┬───────┘                                                      │
│          │                                                               │
│          ▼ Optional: promote to Topic → Memory                          │
│   ┌──────────────┐                                                      │
│   │   ARCHIVED   │─── Queryable via FTS5, can be shredded              │
│   └──────────────┘                                                      │
│                                                                        │
└────────────────────────────────────────────────────────────────────────┘
```

## CLI Commands

### List Sessions
```bash
mpm session list [--limit <n>] [--offset <n>]
```

Lists all sessions with pagination.

**Example output:**
```
$ mpm session list --limit 2

ID: sess_abc123 | Started: 2026-03-28T10:00:00Z | Messages: 45
Summary: "Discussed Docker deployment strategies"

ID: sess_def456 | Started: 2026-03-27T14:30:00Z | Messages: 23
Summary: "Reviewed GitHub Actions configuration"
```

### Show Session Details
```bash
mpm session show <id>
```

Displays full session content with all metadata.

**Example output:**
```
$ mpm session show sess_abc123

┌─────────────────────────────────────────────────────────────┐
│ Session: sess_abc123                                        │
├─────────────────────────────────────────────────────────────┤
│ Type:         direct                                        │
│ Started:      2026-03-28T10:00:00Z                         │
│ Ended:        2026-03-28T10:45:00Z                        │
│ Messages:     45                                           │
│ Persona:      default                                       │
│ Modes:        [debug]                                       │
│ Summary:      Discussed Docker deployment strategies       │
│ Privacy:      private                                       │
└─────────────────────────────────────────────────────────────┘
```

### Search Sessions
```bash
mpm session search <query> [--limit <n>]
```

Performs FTS5 search with highlighting.

**Example:**
```bash
$ mpm session search "Docker setup"

┌─────────────────────────────────────────────────────────────┐
│ sess_abc123 | Score: -0.892                                  │
│ ...learn about <<Docker>> <<setup>> and containerization... │
└─────────────────────────────────────────────────────────────┘
```

### Delete Session (Soft)
```bash
mpm session delete <id>
```

Soft delete — sets `is_active = false`.

### Shred Session (Hard Delete)
```bash
mpm session shred <id>
```

True irreversible erasure with `DELETE` + `VACUUM`.

> 🔒 **Cannot be undone.** See [Shred Protocol](../advanced/shred-protocol.md).

### Save Current Session (Quick)
```bash
mpm ss
```

Quick shortcut to save the current session state. Equivalent to `mpm session save`.

---

## Session Metadata

### Full Schema

| Field | Type | Description | Example |
|-------|------|-------------|---------|
| `id` | TEXT | Unique identifier | `sess_abc123` |
| `type` | TEXT | Session type | `direct`, `group`, `channel` |
| `content` | TEXT | Full session transcript | Long text |
| `start_time` | DATETIME | Session start (RFC3339) | `2026-03-28T10:00:00Z` |
| `end_time` | DATETIME | Session end (RFC3339) | `2026-03-28T10:45:00Z` |
| `summary` | TEXT | AI-generated summary | `Discussed Docker deployment` |
| `notes` | JSON | Additional notes | `{"rating": 5}` |
| `tags` | JSON | Tag array | `["docker", "devops"]` |
| `persona` | TEXT | Active persona | `default` |
| `modes` | JSON | Active modes array | `["debug"]` |
| `working_dir` | TEXT | Working directory | `/home/user/project` |
| `git_branch` | TEXT | Current git branch | `main` |
| `git_commit` | TEXT | Git commit hash | `a1b2c3d4` |
| `privacy` | TEXT | Privacy level | `private`, `shared` |
| `metadata` | JSON | Additional metadata | `{}` |
| `is_active` | BOOLEAN | Soft delete flag | `true` |
| `created_at` | DATETIME | Creation timestamp | `2026-03-28T10:00:00Z` |

### Metadata Example

```json
{
  "id": "sess_abc123",
  "type": "direct",
  "start_time": "2026-03-28T10:00:00Z",
  "end_time": "2026-03-28T10:45:00Z",
  "summary": "Discussed Docker deployment strategies",
  "notes": {
    "rating": 5,
    "follow_up": true
  },
  "tags": ["docker", "containers", "devops"],
  "persona": "default",
  "modes": ["debug"],
  "working_dir": "/home/user/project",
  "git_branch": "main",
  "git_commit": "a1b2c3d4e5f6",
  "privacy": "private",
  "metadata": {
    "channel": "telegram",
    "provider": "telegram"
  }
}
```

---

## Quick Save Workflow

The `mpm ss` command saves the current session state:

```
User types: ~ss or /ss or mpm ss
    ↓
1. Capture current session state
   - Messages (last N)
   - Active persona
   - Active modes
   - Git info (branch, commit)
   - Timestamp
    ↓
2. Generate summary (if needed)
    ↓
3. Store session in SQLite
    ↓
4. Index in FTS5 for search
    ↓
5. Return confirmation
```

**When to use quick save:**
- Before ending a work session
- After completing a significant task
- Before switching personas/modes
- Periodically during long conversations

---

## Integration

| Component | Integration |
|-----------|------------|
| **Topics** | Sessions can be distilled into topics |
| **Memory** | Topics from sessions can harden into memory |
| **Personas** | Sessions record which persona was active |
| **Modes** | Sessions record which modes were stacked |
| **FTS5** | Full-text search across session content |
| **Shred Protocol** | Hard delete with VACUUM |

---

## Examples

### Save and Search
```bash
# Save current session
mpm ss

# Search past sessions
mpm session search "Docker"

# View session details
mpm session show sess_abc123
```

### Session Stats
```bash
# List recent sessions
mpm session list --limit 10

# Find sessions by date
mpm session list | grep "2026-03-28"
```

### Delete Old Sessions
```bash
# Soft delete (recoverable)
mpm session delete sess_old123

# Hard delete (permanent)
mpm session shred sess_old456
```

---

## Troubleshooting

| Issue | Solution |
|-------|----------|
| `Session not found` | Check ID; may be soft-deleted |
| `No search results` | Try different terms; FTS5 is exact match |
| `Slow listing` | Use `--limit` to reduce results |
| `Quick save fails` | Check database permissions |

---

**Last Updated:** 2026-03-29  
**MPM Version:** 6.0.0
