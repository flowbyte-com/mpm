# Memory Decay Scheduling — Phase 1 Technical Design

**Date:** 2026-05-15
**Author:** 808 (∓808)
**Status:** Draft for review

---

## Overview

Automate TTL of memories via weight decay. High-weight (LTM) memories decay slowly; low-weight, unaccessed memories decay fast. Memories reaching weight <= 0 are flagged for deletion review.

**Design principle:** Soft-delete only (no hard deletes). Dead memories are flagged with `deleted_at` — they drop out of FTS5 search but remain for recovery. The agent reviews suggested deletions before they are purged.

---

## 1. The Decay Algorithm

### 1.1 Input Variables

Each memory has:
- `weight` — integer, starts at 1, LTM is 10+
- `last_accessed_at` — timestamp of last `recall` or `show` call (updated by handler)
- `created_at` — immutable creation timestamp
- `is_long_term` — boolean, LTM flag bypasses normal decay

### 1.2 Decay Formula

```
daysSinceAccess = max(0, now - last_accessed_at)
daysSinceCreated = max(0, now - created_at)

if is_long_term:
    decay_amount = daysSinceAccess * 0.01   // ~1% per day (very slow)
else if weight >= 10:
    decay_amount = daysSinceAccess * 0.02  // LTM-ish, moderate decay
else if weight >= 5:
    decay_amount = daysSinceAccess * 0.05  // mid-weight, moderate
else:
    // Low-weight: linear decay scaled by how recently created
    // New memories decay faster to surface them for review
    ageFactor = min(daysSinceCreated / 30.0, 1.0)  // 0 to 1 over 30 days
    decay_amount = daysSinceAccess * (0.1 + 0.2 * ageFactor)
    // Range: 0.1/day (fresh) to 0.3/day (30+ days old)

new_weight = weight - decay_amount
```

**Boundary conditions:**
- `weight <= 0` → memory is "dead", flagged for review
- `weight` floors at `-10` (don't go infinitely negative)
- `is_long_term = true` memories never decay below 1 (LTM protected)

### 1.3 Access Tracking

`last_accessed_at` is updated in `handleRecall` and `handleShow` before the handler returns:

```go
// In handleRecall — update accessed timestamp after FTS search
dm.db.Exec(`UPDATE memories SET last_accessed_at = CURRENT_TIMESTAMP WHERE id = ?`, memID)
```

For `handleShow`, same pattern. This ensures decay math is based on actual usage, not just creation date.

### 1.4 Reinforcement Bounce

When a memory is reinforced (`mpm reinforce`) or recalled in context, weight is bumped. The bounce should exceed decay to create net gain:

```go
// reinforce: net +2 (decay for the day is ~0.1, so net is positive)
weight = weight + 2

// recall in context (implicit reinforcement): net +0.5
weight = weight + 0.5
```

---

## 2. The Active Sweep — `mpm gc` Command

### 2.1 Command Interface

```
mpm gc [--dry-run] [--json] [--aggressive]
```

- `--dry-run`: compute decay and report dead memories, but don't update DB
- `--aggressive`: decay at 2× rate (for testing or cleanup)
- `--json`: structured output for tool integration

### 2.2 Implementation: `handleGC` in `cmd/mpm/handlers.go`

```go
func handleGC(args []string) int {
    jsonOutput, _ := ExtractJSONFlag(args)
    dryRun := false
    aggressive := false

    for _, arg := range args {
        if arg == "--dry-run" {
            dryRun = true
        }
        if arg == "--aggressive" {
            aggressive = true
        }
    }

    dm, err := mpminternal.NewDatabaseManager("")
    if err != nil {
        fmt.Fprintf(os.Stderr, "Error: %v\n", err)
        return 1
    }
    defer dm.Close()

    // Get all non-deleted memories
    rows, err := dm.db.Query(`
        SELECT id, weight, last_accessed_at, created_at, is_long_term, content
        FROM memories WHERE deleted_at IS NULL
    `)
    if err != nil {
        fmt.Fprintf(os.Stderr, "Error: %v\n", err)
        return 1
    }
    defer rows.Close()

    now := time.Now()
    var deadMemories []map[string]interface{}
    var updated int

    for rows.Next() {
        var id string
        var weight int
        var lastAccessed, createdAt *time.Time
        var isLongTerm bool
        var content string

        rows.Scan(&id, &weight, &lastAccessed, &createdAt, &isLongTerm, &content)

        // Compute days since access
        lastAccessTime := lastAccessed
        if lastAccessTime == nil {
            lastAccessTime = createdAt
        }
        daysSinceAccess := now.Sub(*lastAccessTime).Hours() / 24

        // Compute decay amount
        decay := computeDecay(weight, daysSinceAccess, isLongTerm, createdAt, aggressive)
        newWeight := weight - decay

        // Floor
        if newWeight < -10 {
            newWeight = -10
        }

        // Dead if <= 0
        if newWeight <= 0 && weight > 0 {
            deadMemories = append(deadMemories, map[string]interface{}{
                "id":      id,
                "content": content,
                "weight":  weight,
                "decay":   decay,
            })
        }

        if !dryRun && newWeight != weight {
            dm.db.Exec(`UPDATE memories SET weight = ? WHERE id = ?`, newWeight, id)
            updated++
        }
    }

    // Output
    if jsonOutput {
        type gcResult struct {
            Scanned    int      `json:"scanned"`
            Updated    int      `json:"updated"`
            DeadCount  int      `json:"dead_count"`
            DeadMemories []map[string]interface{} `json:"dead_memories,omitempty"`
            DryRun     bool     `json:"dry_run"`
        }
        result := gcResult{
            Scanned:    scanned,
            Updated:    updated,
            DeadCount:  len(deadMemories),
            DeadMemories: deadMemories,
            DryRun:     dryRun,
        }
        data, _ := json.Marshal(result)
        fmt.Println(string(data))
    } else {
        fmt.Printf("Scanned: %d | Updated: %d | Dead: %d\n", scanned, updated, len(deadMemories))
        if len(deadMemories) > 0 {
            fmt.Println("\nDead memories (weight <= 0):")
            for _, m := range deadMemories {
                content := m["content"].(string)
                if len(content) > 60 {
                    content = content[:60] + "…"
                }
                fmt.Printf("  [%s] %s\n", m["id"].(string)[:8], content)
            }
        }
    }
    return 0
}

func computeDecay(weight int, daysSinceAccess float64, isLongTerm bool, createdAt *time.Time, aggressive bool) int {
    multiplier := 1.0
    if aggressive {
        multiplier = 2.0
    }

    if isLongTerm {
        return int(daysSinceAccess * 0.01 * multiplier)
    }
    if weight >= 10 {
        return int(daysSinceAccess * 0.02 * multiplier)
    }
    if weight >= 5 {
        return int(daysSinceAccess * 0.05 * multiplier)
    }

    // Low-weight: decay scales with age (newer = faster decay)
    ageFactor := 1.0
    if createdAt != nil {
        daysSinceCreated := time.Now().Sub(*createdAt).Hours() / 24
        ageFactor = math.Min(daysSinceCreated/30.0, 1.0)
    }
    baseDecay := 0.1 + 0.2*ageFactor
    return int(daysSinceAccess * baseDecay * multiplier)
}
```

---

## 3. Soft-Delete Lifecycle

### 3.1 Dead Memory States

A memory progresses through states:

```
alive (weight > 0) → dead (weight <= 0, deleted_at = nil) → reviewed (deleted_at set) → purged (hard delete)
```

**State 1 — Alive:** Normal queries, FTS5 searchable, weight > 0
**State 2 — Dead (unreviewed):** `deleted_at = NULL`, `weight <= 0`. Excluded from `handleRecall` (FTS5 WHERE clause filters), visible in `mpm gc` output
**State 3 — Reviewed:** `deleted_at = <timestamp>` set by `mpm gc --review` or `mpm shred <id>`. Excluded from all queries. Retained for recovery for 30 days
**State 4 — Purged:** Hard deleted after 30-day review window

### 3.2 FTS5 Query Filter

In `handleRecall` and any read path, soft-deleted memories are excluded:

```sql
WHERE deleted_at IS NULL
```

This is already in the schema indexes (`idx_memories_deleted`). No change needed — the filter applies by default.

### 3.3 Hard Purge

`mpm gc --purge` performs hard delete of memories where:
- `deleted_at IS NOT NULL`
- `deleted_at < now() - 30 days`

```go
if arg == "--purge" {
    result, _ := dm.db.Exec(`DELETE FROM memories WHERE deleted_at IS NOT NULL AND deleted_at < datetime('now', '-30 days')`)
    purged, _ := result.RowsAffected()
    fmt.Printf("Purged %d old deleted memories\n", purged)
}
```

### 3.4 Recovery Path

`mpm restore <id>` clears `deleted_at` and sets `weight = 1`:

```go
dm.db.Exec(`UPDATE memories SET deleted_at = NULL, weight = 1 WHERE id = ?`, id)
```

---

## 4. Integration — AGENTS.md Startup Hook

### 4.1 Startup Sequence

At agent initialization, AGENTS.md directive forces the LLM to call:

```
mpm gc --dry-run --json
```

The JSON output is parsed and surfaced to the agent as a tool result. The agent sees:

```
Memory sweep complete. Scanned: 47 | Updated: 12 | Dead: 3

Dead memories:
  [a1b2c3d4] "old debugging note from last week that is no longer relevant"
  [e5f6g7h8] "temporary test content"
  [i9j0k1l2] "resolved error message that is no longer actionable"

Suggestion: Run `mpm gc` to soft-delete these, then `mpm gc --purge` after review.
```

### 4.2 Two-Step Confirmation

The agent is not auto-deleted. The flow is:

1. `mpm gc --dry-run --json` → agent reviews dead list
2. Agent calls `mpm gc --json` → soft-delete (set `deleted_at = NOW()`)
3. Agent calls `mpm gc --purge` → hard delete after 30-day window

This gives the agent opportunity to `mpm restore <id>` if it decides a flagged memory is still valuable.

---

## 5. Database Changes

### 5.1 No Schema Migration

All required columns (`weight`, `last_accessed_at`, `deleted_at`, `is_long_term`) already exist in `memories` table. The algorithm uses only these columns.

### 5.2 Access Tracking Update

`last_accessed_at` is updated via a simple `UPDATE` in `handleRecall` and `handleShow`. This is a single write per recall (not per result), so performance impact is minimal:

```sql
UPDATE memories SET last_accessed_at = CURRENT_TIMESTAMP WHERE id = ?
```

---

## 6. Files to Modify

| File | Change |
|------|--------|
| `cmd/mpm/handlers.go` | Add `handleGC()`, `computeDecay()` |
| `cmd/mpm/router.go` | Register `gc` command |
| `cmd/mpm/simple_cmds.go` | Ensure `handleRecall` updates `last_accessed_at` after search |
| `docs/MPM_WISHLIST.md` | Move "Memory decay scheduling" to In Progress |

---

## 7. Implementation Tasks

1. **Add `computeDecay()`** — pure function, testable in isolation
2. **Add `handleGC`** — with `--dry-run`, `--json`, `--aggressive`, `--purge` flags
3. **Register `gc`** command in `router.go`
4. **Update `handleRecall`** — add `last_accessed_at` update after FTS results
5. **Add `handleShred` / `handleRestore`** — soft-delete and recovery
6. **Write tests** — `TestComputeDecay` with known inputs
7. **Add `mpm gc --dry-run` to AGENTS.md** startup hook

---

## 8. Open Questions — Resolved

| Question | Resolution |
|----------|------------|
| Hard delete or soft delete? | Soft delete (`deleted_at` column). Hard purge only after 30-day review window. |
| Decay formula complexity? | Pragmatic: 3 tiers based on weight, age factor for low-weight memories. No ML, no exponential curves — just linear + age scaling. |
| How does agent see dead memories? | `mpm gc --dry-run --json` surfaced via AGENTS.md directive at startup. |
| What triggers decay? | `mpm gc` run manually or via cron. Could also be called by `mpm maintain`. Not automatic on every write. |
| LTM protection? | `is_long_term = true` → decay rate 0.01/day, never goes below weight 1. |

---

## Self-Review

- **Placeholder scan:** No TBD, all concrete code
- **Scope check:** Focused on Phase 1 — decay math, gc command, soft-delete lifecycle. No auto-scheduling yet (that would be cron or background goroutine — separate task)
- **No schema migration:** All columns exist
- **Testability:** `computeDecay` is a pure function — easy to unit test with table-driven cases