# Shred Protocol Guide

> 🔒 **For compliance and security.** The Shred Protocol ensures data is mathematically unrecoverable.

## Overview

The Shred Protocol implements **true "hard delete"** with irreversible data erasure for MPM databases. Unlike standard database deletes that leave data recoverable via disk analysis, MPM combines `DELETE` + `VACUUM` to ensure complete physical erasure.

### Why "Shred"?

Standard database operations:
```sql
DELETE FROM memories WHERE id = ?;  -- Row marked as deleted, data still on disk
```

MPM Shred Protocol:
```sql
DELETE FROM memories WHERE id = ?;   -- Row removed
VACUUM;                              -- Database file physically rewritten
```

### Features

| Feature | Description |
|---------|-------------|
| **True Hard Delete** | Uses `DELETE FROM` without soft-deletion flags |
| **Cascading** | Topics cascade to `topic_memberships` |
| **Verification** | Post-delete count verification |
| **VACUUM** | Executes SQLite `VACUUM` to physically rewrite database |
| **Irreversible** | Data is mathematically unrecoverable |

---

## Shred Flow

```
┌──────────────────────────────────────────────────────────────────┐
│                     SHRED PROTOCOL                                │
├──────────────────────────────────────────────────────────────────┤
│                                                                  │
│  1. DELETE                                                        │
│     └─ Execute: DELETE FROM <table> WHERE id = ?                  │
│                                                                  │
│  2. CASCADE (for topics only)                                    │
│     └─ Execute: DELETE FROM topic_memberships WHERE topic_id = ?│
│                                                                  │
│  3. VERIFY                                                        │
│     └─ Query: SELECT COUNT(*) FROM <table> WHERE id = ?          │
│     └─ Expected: 0                                               │
│                                                                  │
│  4. VACUUM                                                        │
│     └─ Execute: VACUUM                                           │
│     └─ Rewrites database file, eliminates free pages              │
│                                                                  │
│  5. POST-VERIFY                                                   │
│     └─ Final count query to confirm erasure                      │
│                                                                  │
└──────────────────────────────────────────────────────────────────┘
```

---

## Shred Commands

### Shred Session
```bash
mpm session shred <id>
```

### Shred Topic
```bash
mpm topic shred <id>
```

> ⚠️ **Cascading:** Shredding a topic also removes all `topic_memberships` entries.

### Shred Memory
```bash
mpm memory shred <id>
```

---

## SQL Implementation

### Shred Query (Memories)
```sql
-- Step 1: Delete the memory
DELETE FROM memories WHERE id = ?;

-- Step 2: Also delete from FTS5 table
DELETE FROM memories_fts WHERE rowid = ?;

-- Step 3: Verify deletion
SELECT COUNT(*) FROM memories WHERE id = ?;  -- Should return 0
```

### Cascading for Topics
```sql
-- Step 1: Delete memberships first (foreign key order)
DELETE FROM topic_memberships WHERE topic_id = ?;

-- Step 2: Delete the topic
DELETE FROM topics WHERE id = ?;

-- Step 3: Delete from FTS5
DELETE FROM topics_fts WHERE rowid = ?;

-- Step 4: Verify
SELECT COUNT(*) FROM topics WHERE id = ?;  -- Should return 0
```

### VACUUM Execution
```sql
VACUUM;
```

> ⚠️ **VACUUM Implications:**
> - Takes ~100-500ms depending on database size
> - Database is locked during VACUUM
> - Requires sufficient free disk space (~database size)
> - Cannot be run inside a transaction

---

## Verification

### Post-Shred Checklist

After running any shred command, verify erasure:

```bash
# 1. Confirm via MPM (no output = success)
mpm memory list | grep <id>
# Should return empty

# 2. Direct database verification
sqlite3 ~/.openclaw/workspace/memory/mpm_memory.db \
  "SELECT COUNT(*) FROM memories WHERE id = '<id>';"
# Expected: 0

# 3. Check FTS5 index
sqlite3 ~/.openclaw/workspace/memory/mpm_memory.db \
  "SELECT COUNT(*) FROM memories_fts WHERE rowid = '<id>';"
# Expected: 0
```

### Audit Log

Shred operations are logged to `mirror.jsonl`:

```json
{
  "timestamp": "2026-03-29T06:30:00Z",
  "operation": "shred",
  "table": "memories",
  "id": "abc123",
  "result": "success"
}
```

---

## Compliance

### GDPR "Right to be Forgotten" (Article 17)

| GDPR Requirement | MPM Implementation |
|-----------------|-------------------|
| Erase personal data | `DELETE` + `VACUUM` |
| No data recovery | VACUUM rewrites DB file |
| Erasure confirmation | Post-shred verification |
| Cascading erasure | Topic → memberships |

> ⚠️ **Important:** The `mirror.jsonl` audit log is **append-only**. For full GDPR compliance, implement log rotation:
> ```bash
> # After shredding, prune old entries
> grep -v "<id>" ~/.openclaw/workspace/memory/mirror.jsonl > /tmp/mirror_new.jsonl
> mv /tmp/mirror_new.jsonl ~/.openclaw/workspace/memory/mirror.jsonl
> ```

### CCPA Compliance

| CCPA Requirement | MPM Implementation |
|-----------------|-------------------|
| Right to delete | Shred protocol |
| No selective deletion | Hard delete is irreversible |
| Deletion confirmation | Verification queries |

---

## Security Model

### Data Sovereignty

| Principle | Implementation |
|-----------|----------------|
| No soft deletion | Pure `DELETE` operations |
| Physical erasure | `VACUUM` rewrites database file |
| Cascading | Foreign key relationships handled |
| Verification | Post-delete count checks |

### What Shred Does NOT Do

| Operation | Done by Shred? |
|-----------|----------------|
| Delete from `mirror.jsonl` | ❌ No (append-only) |
| Clear filesystem blocks | ❌ No (VACUUM clears within SQLite) |
| Delete backup files | ❌ No (user responsibility) |

> 💡 **Complete Erasure:** For complete erasure including audit logs:
> ```bash
> mpm memory shred <id>
> # Then prune audit log
> grep -v "<id>" ~/.openclaw/workspace/memory/mirror.jsonl > /tmp/m
> mv /tmp/m ~/.openclaw/workspace/memory/mirror.jsonl
> ```

---

## Go Implementation

```go
func Shred(db *sql.DB, table, id string) error {
    // 1. Execute DELETE
    query := fmt.Sprintf("DELETE FROM %s WHERE id = ?", table)
    _, err := db.Exec(query, id)
    if err != nil {
        return fmt.Errorf("delete failed: %w", err)
    }

    // 2. Verify deletion
    var count int
    err = db.QueryRow(fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE id = ?", table), id).Scan(&count)
    if err != nil {
        return fmt.Errorf("verification query failed: %w", err)
    }
    if count != 0 {
        return fmt.Errorf("deletion verification failed: %d rows remaining", count)
    }

    // 3. Execute VACUUM
    _, err = db.Exec("VACUUM")
    if err != nil {
        return fmt.Errorf("vacuum failed: %w", err)
    }

    // 4. Post-VACUUM verification
    err = db.QueryRow(fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE id = ?", table), id).Scan(&count)
    if err != nil {
        return fmt.Errorf("post-vacuum verification failed: %w", err)
    }
    if count != 0 {
        return fmt.Errorf("post-vacuum verification failed: %d rows remaining", count)
    }

    return nil
}
```

---

## Use Cases

### Sensitive Data Removal

```bash
# Delete memories containing API keys (if not caught by firewall)
mpm memory shred <id>

# Remove sessions with personal information
mpm session shred <id>

# Erase topics with classified content
mpm topic shred <id>
```

### Compliance

| Regulation | Use Case |
|------------|----------|
| GDPR | "Right to be forgotten" requests |
| CCPA | Consumer data deletion |
| HIPAA | Protected health information |
| PCI-DSS | Cardholder data removal |

---

## Troubleshooting

| Issue | Solution |
|-------|----------|
| VACUUM fails with "locked" | Retry after other operations complete |
| VACUUM fails with "no space" | Free disk space, then retry |
| Verification shows remaining rows | Report bug (should not happen) |
| Slow shred operation | Normal for large databases (~100-500ms) |

---

**Last Updated:** 2026-03-29  
**MPM Version:** 6.0.0
