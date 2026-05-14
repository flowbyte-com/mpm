# Memory Age + Stale Flag Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Flag memories during recall that haven't been accessed recently, so the LLM knows information might be outdated. Uses `last_accessed_at` and `created_at` already fetched from the previous update.

**Architecture:** Add `--stale-days` flag (default 14) to recall. Compute `is_stale` bool per result. Append ⚠️ STALE chip in human output, include `is_stale` in JSON.

**Tech Stack:** Go (cmd/mpm), SQLite

---

## File Inventory

| File | Role |
|------|------|
| `cmd/mpm/recall.go` | Modified: add --stale-days flag, isMemoryStale func, update memoryEntry struct, update chip renderer |
| `cmd/mpm/recall_test.go` | Modified: add unit tests for isMemoryStale |

**No database schema changes** — `last_accessed_at` and `created_at` already exist on memories table.

---

## Task 1: Add `--stale-days` flag and `isMemoryStale` helper

**Files:**
- Modify: `cmd/mpm/recall.go`

- [ ] **Step 1: Add `isMemoryStale` function**

Add after `formatRationale` (around line 430):

```go
// isMemoryStale returns true if the memory hasn't been accessed within
// staleDays, or if it was never accessed and is older than staleDays from creation.
func isMemoryStale(createdAt, lastAccessed time.Time, staleDays int) bool {
    if staleDays <= 0 {
        return false // feature disabled
    }
    threshold := time.Duration(staleDays) * 24 * time.Hour

    if !lastAccessed.IsZero() {
        return time.Since(lastAccessed) > threshold
    }
    if !createdAt.IsZero() {
        return time.Since(createdAt) > threshold
    }
    return false
}
```

- [ ] **Step 2: Add `--stale-days` flag to handleRecall's FlagSet**

In `handleRecall` (around line 32), add after `limit := fs.Int("limit", 15, ...)`:

```go
staleDays := fs.Int("stale-days", 14, "Days threshold for stale flag (0=disabled)")
```

- [ ] **Step 3: Pre-scan `--stale-days N` alongside `--json`**

In the pre-scan loop (around line 46-53), add handling for `--stale-days`:

```go
for _, arg := range args[1:] {
    if arg == "--json" || arg == "-j" {
        jsonFlagSeen = true
        continue
    }
    if strings.HasPrefix(arg, "--stale-days=") || strings.HasPrefix(arg, "-sd") {
        // handled by flag parser, skip
        continue
    }
    preprocessed = append(preprocessed, arg)
}
```

Note: `fs.Parse` will handle `--stale-days N` automatically since it's a defined flag. The pre-scan just needs to not drop it.

- [ ] **Step 4: Verify build**

Run: `go build ./cmd/mpm/`
Expected: compiles without error

- [ ] **Step 5: Commit**

```bash
git add cmd/mpm/recall.go
git commit -m "feat(recall): add --stale-days flag and isMemoryStale helper

Default 14 days. Feature disabled when stale_days=0.
Uses last_accessed_at (primary) or created_at (fallback).
Phase 1 of wishlist item #4.

Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>
"
```

---

## Task 2: Update `memoryEntry` JSON struct and compute `is_stale`

**Files:**
- Modify: `cmd/mpm/recall.go`

- [ ] **Step 1: Add `is_stale` field to `memoryEntry` struct**

In the JSON output block (around line 147), update the struct:

```go
type memoryEntry struct {
    ID                 string  `json:"id"`
    Content            string  `json:"content"`
    Tags               string  `json:"tags"`
    SessionID          string  `json:"session_id,omitempty"`
    CreatedAt          string  `json:"created_at"`
    ReinforcementCount int     `json:"reinforcement_count"`
    Weight             int     `json:"weight"`
    LastAccessedAt     string  `json:"last_accessed_at,omitempty"`
    Score              float64 `json:"score"`
    Rationale          string  `json:"rationale"`
    IsStale            bool    `json:"is_stale"`
}
```

- [ ] **Step 2: Compute `isStale` per entry in the JSON output block**

In the loop that builds JSON results (around line 179), after computing `score` and `rationale`:

```go
isStale := isMemoryStale(e.createdAt, e.lastAccessedAt, *staleDays)

lastAccessStr := ""
if !e.lastAccessedAt.IsZero() {
    lastAccessStr = e.lastAccessedAt.Format(time.RFC3339)
}
score := computeScore(e.reinforcementCount, e.weight)
rationale := formatRationale(e.reinforcementCount, e.weight, e.lastAccessedAt)

result = append(result, memoryEntry{
    ID:                 shortID(e.id),
    Content:            e.content,
    Tags:               e.tags,
    SessionID:          e.sessionID,
    CreatedAt:          e.createdAt.Format(time.RFC3339),
    ReinforcementCount:  e.reinforcementCount,
    Weight:             e.weight,
    LastAccessedAt:      lastAccessStr,
    Score:               score,
    Rationale:           rationale,
    IsStale:             isStale,
})
```

- [ ] **Step 3: Verify build**

Run: `go build ./cmd/mpm/`
Expected: compiles without error

- [ ] **Step 4: Commit**

```bash
git add cmd/mpm/recall.go
git commit -m "feat(recall): add is_stale field to JSON output

memoryEntry struct gains IsStale bool.
Computed per-entry using isMemoryStale(createdAt, lastAccessedAt, staleDays).
Phase 2 of wishlist item #4.

Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>
"
```

---

## Task 3: Update human-readable output with ⚠️ STALE chip

**Files:**
- Modify: `cmd/mpm/recall.go`

- [ ] **Step 1: Add yellow color variable near the other colors**

In the human output section (around line 171-175), add:

```go
yellow := "\033[33m"
```

- [ ] **Step 2: Compute `isStale` in the human output loop**

In the rendering loop (around line 220), before building chips:

```go
isStale := isMemoryStale(e.createdAt, e.lastAccessedAt, *staleDays)
```

- [ ] **Step 3: Append ⚠️ STALE chip to chips if stale**

In the chip building section, after the weight/LTM chips (around line 214):

```go
if isStale {
    chips = append(chips, fmt.Sprintf("%s⚠️ STALE%s", yellow, reset))
}
```

- [ ] **Step 4: Verify build**

Run: `go build ./cmd/mpm/`
Expected: compiles without error

- [ ] **Step 5: Commit**

```bash
git add cmd/mpm/recall.go
git commit -m "feat(recall): add ⚠️ STALE chip to human-readable output

Yellow warning chip appended when isMemoryStale returns true.
Default threshold 14 days, configurable via --stale-days flag.
Phase 3 of wishlist item #4.

Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>
"
```

---

## Task 4: Add unit tests for `isMemoryStale`

**Files:**
- Modify: `cmd/mpm/recall_test.go`

- [ ] **Step 1: Add test for `isMemoryStale`**

In `cmd/mpm/recall_test.go`, add:

```go
func TestIsMemoryStale(t *testing.T) {
    now := time.Now()
    day := 24 * time.Hour

    tests := []struct {
        name       string
        createdAt  time.Time
        lastAccess time.Time
        staleDays  int
        want       bool
    }{
        {
            name:       "disabled when staleDays=0",
            createdAt:  now.Add(-30 * day),
            lastAccess: time.Time{},
            staleDays:  0,
            want:       false,
        },
        {
            name:       "recent last access — not stale",
            createdAt:  now.Add(-30 * day),
            lastAccess: now.Add(-5 * day),
            staleDays:  14,
            want:       false,
        },
        {
            name:       "last access > threshold — stale",
            createdAt:  now.Add(-30 * day),
            lastAccess: now.Add(-20 * day),
            staleDays:  14,
            want:       true,
        },
        {
            name:       "never accessed, created > threshold — stale",
            createdAt:  now.Add(-20 * day),
            lastAccess: time.Time{},
            staleDays:  14,
            want:       true,
        },
        {
            name:       "never accessed, created < threshold — not stale",
            createdAt:  now.Add(-5 * day),
            lastAccess: time.Time{},
            staleDays:  14,
            want:       false,
        },
        {
            name:       "last access exactly at threshold — not stale (exclusive)",
            createdAt:  now.Add(-30 * day),
            lastAccess: now.Add(-14 * day),
            staleDays:  14,
            want:       false,
        },
        {
            name:       "last access just past threshold — stale",
            createdAt:  now.Add(-30 * day),
            lastAccess: now.Add(-14*day - 1*time.Second),
            staleDays:  14,
            want:       true,
        },
        {
            name:       "zero threshold — disabled",
            createdAt:  now.Add(-100 * day),
            lastAccess: now.Add(-100 * day),
            staleDays:  0,
            want:       false,
        },
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            got := isMemoryStale(tt.createdAt, tt.lastAccess, tt.staleDays)
            if got != tt.want {
                t.Errorf("isMemoryStale(%v, %v, %d) = %v, want %v",
                    tt.createdAt, tt.lastAccess, tt.staleDays, got, tt.want)
            }
        })
    }
}
```

- [ ] **Step 2: Run tests**

Run: `go test -v ./cmd/mpm/ -run TestIsMemoryStale`
Expected: PASS

- [ ] **Step 3: Commit**

```bash
git add cmd/mpm/recall_test.go
git commit -m "test(recall): add TestIsMemoryStale unit tests

Covers: disabled when staleDays=0, recent access, past threshold,
never accessed (created fallback), boundary at exactly threshold.
Phase 4 of wishlist item #4.

Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>
"
```

---

## Self-Review Checklist

1. **Spec coverage:** All three phases covered:
   - Phase 1: `--stale-days` flag + `isMemoryStale` helper
   - Phase 2: `is_stale` field in JSON output
   - Phase 3: ⚠️ STALE chip in human output

2. **Placeholder scan:** No `TODO`, `TBD`, or vague steps. All code is concrete.

3. **Type consistency:**
   - `isMemoryStale(createdAt, lastAccessed time.Time, staleDays int) bool` — signature matches what's called in both JSON and human loops
   - `*staleDays` used correctly as pointer from `fs.Int` return value
   - `time.Since()` and `time.Duration` used consistently

4. **Staleness logic:** Uses `lastAccessed` (primary) if non-zero, else `createdAt` (fallback). Exclusive comparison (`>` not `>=`) means exactly-at-threshold is not stale. Disabled when `staleDays <= 0`.

5. **Chip color:** Yellow (`\033[33m`) used for ⚠️ STALE to visually distinguish from magenta (`\033[35m`) rationale chips.

---

## Execution Options

**Plan complete and saved to `docs/superpowers/plans/2026-05-14-memory-stale-flag-plan.md`. Two execution options:**

**1. Subagent-Driven (recommended)** — I dispatch a fresh subagent per task, review between tasks, fast iteration

**2. Inline Execution** — Execute tasks in this session using executing-plans, batch execution with checkpoints

**Which approach?**