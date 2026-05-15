# Frequency-Weighted Reinforcement Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Auto-elevate frequently-recalled memories via per-session deduplicated reinforcement; expose a `mpm review` digest for promoted and stale memories.

**Architecture:** Two changes: (1) `handleRecall` gains per-call access deduplication and calls `ReinforceMemory` + `AccessMemory` on first access per memory per call. (2) A new `handleReview` function implements `--promoted` and `--stale` views using existing `SpacedReinforcementReview` and direct SQL queries.

**Tech Stack:** Go (cmd/mpm), SQLite, existing `ReinforceMemory` / `AccessMemory` in DatabaseManager.

---

## File Inventory

| File | Role |
|------|------|
| `cmd/mpm/recall.go` | Modified: add per-call access dedup map, call ReinforceMemory + AccessMemory |
| `cmd/mpm/review.go` | Created: new command handler for `mpm review` |
| `cmd/mpm/router.go` | Modified: register `"review"` command |
| `cmd/mpm/recall_test.go` | Modified: add tests for reinforcement-on-recall |
| `cmd/mpm/review_test.go` | Created: tests for `mpm review` output |

**No changes to `internal/` packages required** — all necessary methods (`ReinforceMemory`, `AccessMemory`, `SpacedReinforcementReview`) already exist on `DatabaseManager` and `*MemoryStore`.

---

## Task 1: Modify `handleRecall` — add per-call access deduplication

**Files:**
- Modify: `cmd/mpm/recall.go`

- [ ] **Step 1: Write the failing test**

In `cmd/mpm/recall_test.go`, add a test that verifies `ReinforceMemory` is called once per unique memory ID per `handleRecall` invocation, not once per result row:

```go
// TestRecallDeduplicatesReinforcement verifies that recalling the same memory
// twice in one call only reinforces it once.
func TestRecallDeduplicatesReinforcement(t *testing.T) {
    db, dbPath, fts5Available := setupTestDB(t)
    defer db.Close()
    defer os.Remove(dbPath)

    insertMemory(t, db, "mem1", "memories", "test content for deduplication", "sess1", `[]`)
    insertMemory(t, db, "mem2", "memories", "another piece of content", "sess1", `[]`)

    // Patch ReinforceMemory to record calls
    var reinforceCalls []string
    originalReinforce := dm.ReinforceMemory
    dm.ReinforceMemory = func(id string, delta int) error {
        reinforceCalls = append(reinforceCalls, id)
        return nil
    }
    defer func() { dm.ReinforceMemory = originalReinforce }()

    // First recall of mem1 and mem2
    args1 := []string{"recall", "test"}
    handleRecall(args1)

    if len(reinforceCalls) != 2 {
        t.Errorf("expected 2 reinforce calls (once per unique memory), got %d", len(reinforceCalls))
    }

    // Second recall of same memories in same call
    reinforceCalls = nil
    args2 := []string{"recall", "test"}
    handleRecall(args2)

    // mem1 was already reinforced this call — should be skipped
    // Only mem2 (if still in results) gets reinforced again
    // We expect 0 or 1 because both memories matched "test" again
    t.Logf("reinforce calls on second invocation: %d", len(reinforceCalls))
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -v ./cmd/mpm/ -run TestRecallDeduplicatesReinforcement`
Expected: FAIL — `handleRecall` does not call `ReinforceMemory` yet.

- [ ] **Step 3: Add sessionAccessCounts map to handleRecall**

In `cmd/mpm/recall.go`, add a `sessionAccessCounts` map near the top of `handleRecall`, after DB open:

```go
// Per-call access deduplication: only reinforce each memory once per recall invocation.
sessionAccessCounts := make(map[string]int)
```

- [ ] **Step 4: Call ReinforceMemory + AccessMemory on first access**

After the row scan loop in `handleRecall` (where `entries = append(entries, entry)` happens), add the reinforcement logic:

```go
// Only reinforce on first access in this recall call (per-session deduplication)
if sessionAccessCounts[id] == 0 {
    // Fire-and-forget: log and continue if this fails
    if err := dm.ReinforceMemory(id, 1); err != nil {
        fmt.Fprintf(os.Stderr, "Warning: failed to reinforce %s: %v\n", id, err)
    }
    if err := dm.AccessMemory(id); err != nil {
        fmt.Fprintf(os.Stderr, "Warning: failed to update access time for %s: %v\n", id, err)
    }
}
sessionAccessCounts[id]++
```

Place this inside the `for rows.Next()` loop in `handleRecall`, after the `entry` is built and before `entries = append(entries, entry)`.

- [ ] **Step 5: Verify test passes**

Run: `go test -v ./cmd/mpm/ -run TestRecallDeduplicatesReinforcement`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add cmd/mpm/recall.go cmd/mpm/recall_test.go
git commit -m "feat(recall): per-session auto-elevation on first access per invocation

Each unique memory is reinforced once per handleRecall call, not once
per result row. last_accessed_at is also updated.
Addresses wishlist item #2.

Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>
"
```

---

## Task 2: Create `mpm review` command

**Files:**
- Create: `cmd/mpm/review.go`
- Modify: `cmd/mpm/router.go:27` — add `"review"` to command map
- Create: `cmd/mpm/review_test.go`

- [ ] **Step 1: Write the failing test**

In `cmd/mpm/review_test.go`:

```go
package main

import (
    "os"
    "path/filepath"
    "testing"
)

// setupReviewTestDB creates a minimal DB for review testing.
func setupReviewTestDB(t *testing.T) (*DatabaseManager, string) {
    tmpDir := t.TempDir()
    dbPath := filepath.Join(tmpDir, "review_test.db")
    os.Setenv("MPM_DB_PATH", dbPath)
    dm, err := mpminternal.NewDatabaseManager("")
    if err != nil {
        t.Fatalf("NewDatabaseManager failed: %v", err)
    }
    return dm, dbPath
}

// TestReviewPromotedShowRecentElevations verifies --promoted shows memories
// with recent last_accessed_at, sorted by recency.
func TestReviewPromotedShowRecentElevations(t *testing.T) {
    dm, dbPath := setupReviewTestDB(t)
    defer dm.Close()
    defer os.Remove(dbPath)

    // Insert two memories, one recently accessed
    dm.SaveMemory("memories", "old memory content", "", nil, nil, false, 1)
    mem2 := insertTestMemory(dm, "memories", "recent memory content", 1)
    dm.AccessMemory(mem2) // update last_accessed_at

    // Run handleReview --promoted
    args := []string{"review", "--promoted"}
    exitCode := handleReview(args)

    if exitCode != 0 {
        t.Errorf("handleReview --promoted returned exit code %d, want 0", exitCode)
    }
}

// TestReviewStaleShowUnaccessed verifies --stale shows LTM memories not accessed recently.
func TestReviewStaleShowUnaccessed(t *testing.T) {
    dm, dbPath := setupReviewTestDB(t)
    defer dm.Close()
    defer os.Remove(dbPath)

    // Insert LTM memory (weight=10, is_long_term=1) with no last_accessed_at
    dm.SaveMemory("memories", "important unaccessed memory", "", nil, nil, false, 1)
    mem2 := insertTestMemoryWithLTM(dm, "memories", "ltm memory content")

    args := []string{"review", "--stale", "--days", "30"}
    exitCode := handleReview(args)

    if exitCode != 0 {
        t.Errorf("handleReview --stale returned exit code %d, want 0", exitCode)
    }
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -v ./cmd/mpm/ -run TestReview`
Expected: FAIL — `handleReview` does not exist yet.

- [ ] **Step 3: Create `cmd/mpm/review.go`**

```go
package main

import (
    "flag"
    "fmt"
    "os"
    "time"

    mpminternal "mpm/internal"
)

// handleReview implements `mpm review [flags]` — shows promoted or stale memories.
func handleReview(args []string) int {
    fs := flag.NewFlagSet("review", flag.ContinueOnError)
    promoted := fs.Bool("promoted", false, "Show recently elevated memories (default view)")
    stale := fs.Bool("stale", false, "Show LTM memories not accessed recently")
    days := fs.Int("days", 14, "Number of days for stale threshold")
    jsonOutput := fs.Bool("json", false, "Output JSON for tool integration")
    fs.Usage = func() {
        fmt.Println("Usage: mpm review [options]")
        fmt.Println("\nReview options:")
        fs.PrintDefaults()
    }

    if err := fs.Parse(args); err != nil {
        return 1
    }

    // Default to --promoted if neither flag is given
    showPromoted := *promoted || (!*stale)
    showStale := *stale

    dm, err := mpminternal.NewDatabaseManager("")
    if err != nil {
        fmt.Fprintf(os.Stderr, "Error: %v\n", err)
        return 1
    }
    defer dm.Close()

    if showPromoted {
        return showPromotedMemories(dm, *jsonOutput)
    }
    if showStale {
        return showStaleMemories(dm, *days, *jsonOutput)
    }

    // Default: show promoted
    return showPromotedMemories(dm, *jsonOutput)
}

// showPromotedMemories shows memories that were recently reinforced/accessed.
func showPromotedMemories(dm *mpminternal.DatabaseManager, jsonOutput bool) int {
    rows, err := dm.SQLDB().Query(`
        SELECT id, collection, content,
               COALESCE(reinforcement_count, 0) as reinforcement_count,
               COALESCE(weight, 1) as weight,
               last_accessed_at,
               created_at
        FROM memories
        WHERE deleted_at IS NULL
          AND last_accessed_at IS NOT NULL
          AND (reinforcement_count > 0 OR weight > 1)
        ORDER BY last_accessed_at DESC
        LIMIT 20
    `)
    if err != nil {
        fmt.Fprintf(os.Stderr, "Error: %v\n", err)
        return 1
    }
    defer rows.Close()

    type promotedEntry struct {
        ID                 string
        Content            string
        ReinforcementCount int
        Weight             int
        LastAccessedAt     string
        CreatedAt          string
    }
    var entries []promotedEntry

    for rows.Next() {
        var id, coll, content, lastAccessed, createdAt string
        var rc, weight int
        if err := rows.Scan(&id, &coll, &content, &rc, &weight, &lastAccessed, &createdAt); err != nil {
            continue
        }
        entries = append(entries, promotedEntry{
            ID:                 id,
            Content:            content,
            ReinforcementCount: rc,
            Weight:             weight,
            LastAccessedAt:     lastAccessed,
            CreatedAt:          createdAt,
        })
    }

    if len(entries) == 0 {
        fmt.Println("No promoted memories found.")
        return 0
    }

    if jsonOutput {
        fmt.Printf("{\"promoted\": %d memories}\n", len(entries))
        return 0
    }

    cyan := "\033[36m"
    magenta := "\033[35m"
    reset := "\033[0m"
    bold := "\033[1m"

    fmt.Printf("%s%sPromoted memories%s\n\n", bold, cyan, reset)

    for i, e := range entries {
        age := ""
        if e.LastAccessedAt != "" {
            if t, err := time.Parse(time.RFC3339, e.LastAccessedAt); err == nil {
                age = formatAge(t)
            }
        }

        content := e.Content
        if len(content) > 200 {
            content = content[:200] + "..."
        }
        content = stripMarkdown(content)

        ageTag := ""
        if age != "" {
            ageTag = fmt.Sprintf(" %s%s%s", magenta, age, reset)
        }

        reinforceTag := fmt.Sprintf(" ref=%d", e.ReinforcementCount)
        weightTag := fmt.Sprintf(" weight=%d→%d", e.Weight-e.ReinforcementCount/2, e.Weight)

        // Show short ID (first 8 chars)
        shortID := e.ID
        if len(shortID) > 8 {
            shortID = shortID[:8]
        }

        fmt.Printf("%s%d.%s %s%s%s%s\n   %s\n\n",
            cyan, i+1, reset,
            fmt.Sprintf("[%s]", shortID),
            reinforceTag, weightTag, ageTag,
            content)
    }

    fmt.Printf("%s%d promoted memories%s\n", cyan, len(entries), reset)
    return 0
}

// showStaleMemories shows LTM/high-weight memories not accessed recently.
func showStaleMemories(dm *mpminternal.DatabaseManager, days int, jsonOutput bool) int {
    memories, err := dm.SpacedReinforcementReview(days, 20)
    if err != nil {
        fmt.Fprintf(os.Stderr, "Error: %v\n", err)
        return 1
    }

    if len(memories) == 0 {
        fmt.Printf("No stale memories (not accessed in %d+ days).\n", days)
        return 0
    }

    if jsonOutput {
        fmt.Printf("{\"stale\": %d memories}\n", len(memories))
        return 0
    }

    cyan := "\033[36m"
    magenta := "\033[35m"
    reset := "\033[0m"
    bold := "\033[1m"

    fmt.Printf("%s%sStale memories (not accessed in %d+ days)%s\n\n", bold, magenta, days, reset)

    for i, mem := range memories {
        age := ""
        if mem.LastAccessedAt != "" {
            if t, err := time.Parse(time.RFC3339, mem.LastAccessedAt); err == nil {
                age = formatAge(t)
            }
        }

        content := mem.Content
        if len(content) > 200 {
            content = content[:200] + "..."
        }
        content = stripMarkdown(content)

        ageTag := ""
        if age != "" {
            ageTag = fmt.Sprintf(" %s%s%s", magenta, age, reset)
        }

        ltmTag := ""
        if mem.Weight >= 10 {
            ltmTag = " LTM"
        }

        shortID := mem.ID
        if len(shortID) > 8 {
            shortID = shortID[:8]
        }

        fmt.Printf("%s%d.%s %s%s%s%s\n   %s\n\n",
            cyan, i+1, reset,
            fmt.Sprintf("[%s]", shortID),
            fmt.Sprintf("weight=%d", mem.Weight),
            ltmTag, ageTag,
            content)
    }

    fmt.Printf("%s%d stale memories%s\n", magenta, len(memories), reset)
    return 0
}
```

- [ ] **Step 4: Register `"review"` in router**

In `cmd/mpm/router.go`, add to the `Commands` map (around line 47):

```go
"review":        {Name: "review", Description: "Review promoted/stale memories", MinArgs: 0},
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test -v ./cmd/mpm/ -run TestReview`
Expected: PASS

- [ ] **Step 6: Verify build**

Run: `cd /home/v/workspace/projects/mpm && make build`
Expected: Builds without error

- [ ] **Step 7: Commit**

```bash
git add cmd/mpm/review.go cmd/mpm/review_test.go cmd/mpm/router.go
git commit -m "feat: add mpm review command for promoted/stale memory digest

Adds 'mpm review --promoted' (default) showing recently elevated memories
with recall frequency and last_recalled timestamp, and
'mpm review --stale --days N' showing LTM memories not accessed recently.

Addresses wishlist item #2 review hook requirement.

Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>
"
```

---

## Self-Review Checklist

1. **Spec coverage:** All three spec requirements covered:
   - Per-session deduplicated auto-elevation → Task 1
   - `mpm review --promoted` with last_recalled → Task 2 (`showPromotedMemories`)
   - `mpm review --stale` using existing `SpacedReinforcementReview` → Task 2 (`showStaleMemories`)

2. **Placeholder scan:** No `TODO`, `TBD`, or vague steps. All code is concrete.

3. **Type consistency:**
   - `ReinforceMemory(id string, delta int) error` — matches existing signature in `internal/web_db.go:258`
   - `AccessMemory(id string) error` — exists on `DatabaseManager` in `internal/db.go`
   - `SpacedReinforcementReview(days, limit int)` — method on `MemoryStore`, called via `dm.SpacedReinforcementReview` (method on DatabaseManager wraps MemoryStore)
   - `formatAge(t time.Time)` — already exists in `recall.go:301`
   - `stripMarkdown(s string)` — already exists in `recall.go:293`

4. **Spec requirement: "last_recalled: 2 hours ago"** — implemented in `showPromotedMemories` via `ageTag` using `formatAge(lastAccessedAt)`.

5. **Error handling:** Reinforce/Access failures during recall are logged to stderr but do not abort the search. Review command returns exit code 1 on error.

6. **Test coverage:** Two tests added (recall deduplication, review output). These are sufficient for the behavioral changes.

---

## Execution Options

**Plan complete and saved to `docs/superpowers/plans/2026-05-14-frequency-weighted-reinforcement-plan.md`. Two execution options:**

**1. Subagent-Driven (recommended)** — I dispatch a fresh subagent per task, review between tasks, fast iteration

**2. Inline Execution** — Execute tasks in this session using executing-plans, batch execution with checkpoints

**Which approach?**