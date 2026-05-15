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
- `weight` — float64, starts at 1.0, LTM is 10.0+
- `last_accessed_at` — timestamp of last `recall` or `show` call (updated by handler)
- `created_at` — immutable creation timestamp
- `is_long_term` — boolean, LTM flag bypasses normal decay

**Note:** `weight` column is stored as INTEGER in the schema but all decay math is done in float64. The database receives a truncated int value on write. This avoids accumulation of floating-point error while keeping decay precise.

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
- `weight` floors at `-10.0` (don't go infinitely negative)
- `is_long_term = true` memories never decay below 1.0 (LTM protected)

**All decay computation uses float64 throughout.** Only the final value written to DB is truncated to int. This ensures a 3-day-old low-weight memory decays by `3 × 0.1 = 0.3`, not `0` (integer truncation bug).

### 1.3 Access Tracking

`last_accessed_at` is updated in `handleRecall` and `handleShow` before the handler returns:

```go
// In handleRecall — update accessed timestamp after FTS search
dm.db.Exec(`UPDATE memories SET last_accessed_at = CURRENT_TIMESTAMP WHERE id = ?`, memID)
```

For `handleShow`, same pattern. This ensures decay math is based on actual usage, not just creation date.

### 1.4 Implicit Reinforcement on Recall

Decay without bounce leads to net erosion — all memories slowly approach weight 0 with only manual `mpm reinforce` as counterbalance. To correct this, recall implicitly reinforces memory weight by +0.5 per recall event (capped at +1 per session per memory to prevent abuse).

**Implementation:** After `handleRecall` returns results, a batch UPDATE increments weight for each returned memory:

```sql
UPDATE memories SET weight = MIN(weight + 0.5, weight + 1.0, 100.0)
WHERE id IN (?, ?, ...) AND weight < 100.0
```

This is a single batch update per recall call, not per result. The cap (`MIN(..., weight + 1.0)`) ensures a memory can gain at most 1.0 weight per session even if recalled multiple times.

**Important:** `RecallMemories` in `web_db.go` currently only updates `last_accessed_at`. This behavior must be added to `handleRecall` as part of Phase 1 implementation. `mpm reinforce` continues to work as-is (net +2 per call).

---

## 2. The Active Sweep — `mpm gc` Command

### 2.1 Command Interface

```
mpm gc [--dry-run] [--json] [--aggressive] [--review] [--purge] [--max-age <hours>]
```

- `--dry-run`: compute decay and report dead memories, but don't update DB
- `--aggressive`: decay at 2× rate (for testing or cleanup)
- `--json`: structured output for tool integration
- `--review`: soft-delete all dead memories (set `deleted_at = NOW()` for weight <= 0)
- `--purge`: hard delete memories where `deleted_at IS NOT NULL AND deleted_at < now() - 30 days`
- `--max-age <hours>`: skip scan if last gc ran within this window (default: 24)

### 2.2 Implementation: `handleGC` in `cmd/mpm/handlers.go`

```go
func handleGC(args []string) int {
    jsonOutput, _ := ExtractJSONFlag(args)
    dryRun := false
    aggressive := false
    review := false
    purge := false
    maxAgeHours := 24

    for _, arg := range args {
        if arg == "--dry-run" { dryRun = true }
        if arg == "--aggressive" { aggressive = true }
        if arg == "--review" { review = true }
        if arg == "--purge" { purge = true }
        if strings.HasPrefix(arg, "--max-age=") {
            fmt.Sscanf(arg, "--max-age=%d", &maxAgeHours)
        }
    }

    dm, err := mpminternal.NewDatabaseManager("")
    if err != nil {
        fmt.Fprintf(os.Stderr, "Error: %v\n", err)
        return 1
    }
    defer dm.Close()

    // Check frequency cap — skip if last gc ran within max-age window
    if lastGC, err := dm.GetSystemConfig("last_gc_at"); err == nil {
        if t, ok := lastGC["updated_at"].(string); ok {
            last, _ := time.Parse(time.RFC3339, t)
            if time.Since(last).Hours() < float64(maxAgeHours) {
                fmt.Println("Skipped: last gc was", last.Format("2006-01-02 15:04"))
                return 0
            }
        }
    }

    // Purge mode: hard delete old reviewed memories and exit
    if purge {
        result, _ := dm.db.Exec(`
            DELETE FROM memories
            WHERE deleted_at IS NOT NULL
            AND deleted_at < datetime('now', '-30 days')
        `)
        purged, _ := result.RowsAffected()
        fmt.Printf("Purged %d old deleted memories\n", purged)
        return 0
    }

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
    var updated, scanned int

    for rows.Next() {
        scanned++
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

        // Compute decay amount (float64 throughout)
        decay := computeDecay(float64(weight), daysSinceAccess, isLongTerm, createdAt, aggressive)
        newWeight := float64(weight) - decay

        // Floor
        if newWeight < -10.0 {
            newWeight = -10.0
        }
        // LTM protection: is_long_term never decays below 1.0
        if isLongTerm && newWeight < 1.0 {
            newWeight = 1.0
        }

        // Dead if <= 0
        if newWeight <= 0.0 && float64(weight) > 0.0 {
            deadMemories = append(deadMemories, map[string]interface{}{
                "id":      id,
                "content": content,
                "weight":  weight,
                "decay":   decay,
            })
        }

        if !dryRun && newWeight != float64(weight) {
            dm.db.Exec(`UPDATE memories SET weight = ? WHERE id = ?`, int(newWeight), id)
            updated++
        }
    }

    // --review mode: soft-delete all dead memories
    if review && len(deadMemories) > 0 && !dryRun {
        for _, m := range deadMemories {
            dm.db.Exec(`UPDATE memories SET deleted_at = CURRENT_TIMESTAMP WHERE id = ?`, m["id"])
        }
        fmt.Printf("Soft-deleted %d dead memories\n", len(deadMemories))
    }

    // Update last_gc_at timestamp
    if !dryRun {
        dm.SaveSystemConfig("last_gc_at", `{"timestamp":"`+time.Now().Format(time.RFC3339)+`"}`, "", "")
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

func computeDecay(weight float64, daysSinceAccess float64, isLongTerm bool, createdAt *time.Time, aggressive bool) float64 {
    multiplier := 1.0
    if aggressive {
        multiplier = 2.0
    }

    if isLongTerm {
        return daysSinceAccess * 0.01 * multiplier
    }
    if weight >= 10 {
        return daysSinceAccess * 0.02 * multiplier
    }
    if weight >= 5 {
        return daysSinceAccess * 0.05 * multiplier
    }

    // Low-weight: decay scales with age (newer = faster decay)
    ageFactor := 1.0
    if createdAt != nil {
        daysSinceCreated := time.Now().Sub(*createdAt).Hours() / 24
        ageFactor = math.Min(daysSinceCreated/30.0, 1.0)
    }
    baseDecay := 0.1 + 0.2*ageFactor
    return daysSinceAccess * baseDecay * multiplier
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

In `handleRecall` and any read path, dead memories are excluded by filtering both `deleted_at IS NULL` **and** `weight > 0`:

```sql
WHERE deleted_at IS NULL AND weight > 0
```

**This is a critical implementation requirement.** The existing `deleted_at IS NULL` filter alone is insufficient — a memory with `weight <= 0` and `deleted_at = NULL` would still appear in results. Every SELECT against `memories` that is meant to find alive memories must include `AND weight > 0`.

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

### 4.1 Startup Sequence with Frequency Cap

At agent initialization, AGENTS.md directive forces the LLM to call:

```
mpm gc --dry-run --json [--max-age <hours>]
```

**Frequency cap:** `--max-age <hours>` (default: 24) skips the scan if `mpm gc` was last run within the window. This prevents a full scan on every session start.

The LLM is instructed to call this once per startup at most. A `last_gc_at` timestamp is tracked in `system_config`:

```sql
-- Check: skip if last gc ran within max-age window
SELECT raw_json FROM system_config WHERE key = 'last_gc_at'
-- If returned timestamp is within max-age, output "Skipped: last gc was <time>"
-- and exit without scanning
```

### 4.2 Output Format

The JSON output is parsed and surfaced to the agent as a tool result. The agent sees:

```
Memory sweep complete. Scanned: 47 | Updated: 12 | Dead: 3

Dead memories:
  [a1b2c3d4] "old debugging note from last week that is no longer relevant"
  [e5f6g7h8] "temporary test content"
  [i9j0k1l2] "resolved error message that is no longer actionable"

Suggestion: Run `mpm gc` to soft-delete these, then `mpm gc --purge` after review.
```

If no dead memories: `Memory sweep clean. No action needed.`

### 4.2 Two-Step Confirmation

The agent is not auto-deleted. The flow is:

1. `mpm gc --dry-run --json` → agent reviews dead list
2. Agent calls `mpm gc --review --json` → soft-delete (set `deleted_at = NOW()` for weight <= 0)
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

1. **Add `computeDecay(weight float64, ...)`** — pure function, testable in isolation. Returns `float64`. All internal math float64.
2. **Add `handleGC`** — with `--dry-run`, `--json`, `--aggressive`, `--review`, `--purge`, `--max-age` flags
3. **Register `gc`** command in `router.go`
4. **Add weight filter to all recall queries** — `WHERE deleted_at IS NULL AND weight > 0`. This is a REQUIRED change — dead memories must be excluded from all read paths.
5. **Update `handleRecall`** — add implicit reinforcement (+0.5, capped +1/session) via batch UPDATE after results returned. Also update `last_accessed_at`.
6. **Add `handleRestore`** — clears `deleted_at` and sets `weight = 1` for a given memory ID
7. **Write tests** — `TestComputeDecay` with table-driven cases covering all tiers, age scaling, LTM protection
8. **Add `mpm gc --dry-run --json --max-age 24` to AGENTS.md** startup hook

---

## 8. Open Questions — Resolved

| Question | Resolution |
|----------|------------|
| Hard delete or soft delete? | Soft delete (`deleted_at` column). Hard purge only after 30-day review window. |
| Decay formula complexity? | Pragmatic: 3 tiers based on weight, age factor for low-weight memories. No ML, no exponential curves — just linear + age scaling. |
| How does agent see dead memories? | `mpm gc --dry-run --json` surfaced via AGENTS.md directive at startup. |
| What triggers decay? | `mpm gc` run manually or via cron. Could also be called by `mpm maintain`. Not automatic on every write. |
| LTM protection? | `is_long_term = true` → decay rate 0.01/day, never goes below 1.0. |
| Integer truncation bug? | All decay math uses float64 throughout. Only the final value written to DB is truncated to int. A 3-day-old low-weight memory decays by 0.3 (not 0). |
| Dead memories in FTS5? | Recall queries must include `AND weight > 0` in addition to `deleted_at IS NULL`. This is a required implementation change. |
| Implicit reinforcement? | `handleRecall` applies +0.5 weight (capped +1/session per memory) after returning results. Prevents net erosion without manual reinforce calls. |
| Startup scan cost? | `--max-age <hours>` flag skips scan if `last_gc_at` is within the window. Default: 24 hours. Prevents full scan on every session boot. |

---

## Self-Review

- **Placeholder scan:** No TBD, all concrete code
- **Scope check:** Focused on Phase 1 — decay math, gc command, soft-delete lifecycle. No auto-scheduling yet (that would be cron or background goroutine — separate task)
- **No schema migration:** All columns exist
- **Testability:** `computeDecay` is a pure function — easy to unit test with table-driven cases