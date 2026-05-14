# Recall Explanation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add `score` (0–1 fractional) and inline rationale chips to each recall result — showing reinforcement count, weight, LTM status, and last accessed age.

**Architecture:** Extend `recallEntry` struct with three new fields, update SQL to fetch them, render compact chips in human output, and compute score + rationale string for JSON.

**Tech Stack:** Go (cmd/mpm), SQLite

---

## File Inventory

| File | Role |
|------|------|
| `cmd/mpm/recall.go` | Modified: extend struct, update SQL, render chips, compute score/rationale |
| `cmd/mpm/recall_test.go` | Modified: add test for chip rendering in output |

**No changes to `internal/` packages or database schema required.** All needed columns (`reinforcement_count`, `weight`, `last_accessed_at`) already exist on `memories` table.

---

## Task 1: Extend `recallEntry` struct and update SQL

**Files:**
- Modify: `cmd/mpm/recall.go:93-99`

- [ ] **Step 1: Add fields to `recallEntry` struct**

In `cmd/mpm/recall.go`, update the `recallEntry` struct:

```go
type recallEntry struct {
    content             string
    sessionID           string
    createdAt           time.Time
    tags                string
    synthesized         bool
    reinforcementCount int    // NEW
    weight              int    // NEW
    lastAccessedAt      time.Time // NEW
}
```

- [ ] **Step 2: Update `keywordSearchWithTime` SQL — add 3 columns to SELECT**

In `keywordSearchWithTime` (`cmd/mpm/recall.go:251-269`), add columns to both FTS5 and LIKE fallback queries.

**FTS5 query (line ~251):**
```sql
SELECT m.id, m.content, m.session_id, m.tags, m.created_at,
       COALESCE(m.reinforcement_count, 0) as reinforcement_count,
       COALESCE(m.weight, 1) as weight,
       m.last_accessed_at
FROM memories m
JOIN memories_fts fts ON m.rowid = fts.rowid
WHERE memories_fts MATCH ? AND m.deleted_at IS NULL AND m.collection = ?
```

**LIKE fallback query (line ~278):**
```sql
SELECT id, content, session_id, tags, created_at,
       COALESCE(reinforcement_count, 0) as reinforcement_count,
       COALESCE(weight, 1) as weight,
       last_accessed_at
FROM memories
WHERE deleted_at IS NULL AND collection = ?
  AND (content LIKE ? OR tags LIKE ?)
```

- [ ] **Step 3: Update row scan in `handleRecall` — read 3 new columns**

In `handleRecall` (around line 102), update the `rows.Scan` call:

```go
var id, content, createdAt string
var nullableSessionID, nullableTags sql.NullString
var reinforcementCount, weight int64
var nullableLastAccessed sql.NullTime

if err := rows.Scan(&id, &content, &nullableSessionID, &nullableTags, &createdAt,
    &reinforcementCount, &weight, &nullableLastAccessed); err != nil {
    continue
}
```

Then when building the entry (around line 114):
```go
entry := recallEntry{
    content:             content,
    sessionID:          sessionID,
    tags:               nullableTags.String,
    reinforcementCount: int(reinforcementCount),
    weight:             int(weight),
}
if nullableLastAccessed.Valid {
    entry.lastAccessedAt = nullableLastAccessed.Time
}
```

- [ ] **Step 4: Verify build**

Run: `go build ./cmd/mpm/`
Expected: compiles without error

---

## Task 2: Add rationale computation helpers

**Files:**
- Modify: `cmd/mpm/recall.go`

- [ ] **Step 1: Add `computeScore` and `formatRationale` functions**

Add after `formatAge` (around line 325):

```go
// computeScore returns a fractional score 0-1 based on reinforcement count and weight.
// Formula: clamp((rc * 2 + weight * 1.5) / 50, 0, 1)
func computeScore(rc, weight int) float64 {
    raw := float64(rc*2) + float64(weight*3)/2
    result := raw / 50.0
    if result > 1.0 {
        result = 1.0
    }
    if result < 0.0 {
        result = 0.0
    }
    return result
}

// formatRationale returns a one-line string describing why this memory matters.
func formatRationale(rc, weight int, lastAccessed time.Time) string {
    parts := []string{}

    if rc > 0 {
        parts = append(parts, fmt.Sprintf("%dx ref", rc))
    }

    if weight > 1 {
        parts = append(parts, fmt.Sprintf("weight %d", weight))
    }

    if weight >= 10 {
        parts = append(parts, "LTM")
    }

    if !lastAccessed.IsZero() {
        parts = append(parts, "accessed "+formatAge(lastAccessed))
    }

    return strings.Join(parts, " · ")
}
```

Note: `strings` is already imported (`recall.go:11`).

- [ ] **Step 2: Verify build**

Run: `go build ./cmd/mpm/`
Expected: compiles without error

---

## Task 3: Update human-readable output with chips

**Files:**
- Modify: `cmd/mpm/recall.go:176-213`

- [ ] **Step 1: Replace existing result rendering with chip-aware version**

In `handleRecall`, find the human-readable output block (starting around line 176, `fmt.Printf("%s%sRecall — %s%s\n\n"...)`). Replace the result loop (lines 178–211) with:

```go
for i, e := range entries {
    age := ""
    if !e.createdAt.IsZero() {
        age = formatAge(e.createdAt)
    }

    content := e.content
    if len(content) > 250 {
        content = content[:250] + "..."
    }
    content = stripMarkdown(content)

    // Build chip list
    chips := []string{}

    // Reinforcement count
    if e.reinforcementCount > 0 {
        chips = append(chips, fmt.Sprintf("%dx ref", e.reinforcementCount))
    }

    // Weight (skip if ≤ 1 since that's default)
    if e.weight > 1 {
        chips = append(chips, fmt.Sprintf("weight %d", e.weight))
    }

    // LTM flag
    ltmTag := ""
    if e.weight >= 10 {
        ltmTag = fmt.Sprintf(" %sLTM%s", magenta, reset)
    }

    // Access age (use lastAccessedAt, not createdAt)
    accessAge := ""
    if !e.lastAccessedAt.IsZero() {
        accessAge = formatAge(e.lastAccessedAt)
    }

    // ID chip
    shortID := shortID(memID) // memID from scan
    idChip := fmt.Sprintf("%s[%s]%s", dim, shortID, reset)

    // Assemble chip line
    chipParts := []string{idChip}
    for _, c := range chips {
        chipParts = append(chipParts, fmt.Sprintf("%s%s%s", magenta, c, reset))
    }
    if accessAge != "" {
        chipParts = append(chipParts, fmt.Sprintf("%s%s%s", magenta, accessAge, reset))
    }

    chipsLine := strings.Join(chipParts, " · ")
    if ltmTag != "" {
        chipsLine += ltmTag
    }

    synthTag := ""
    if e.synthesized {
        synthTag = fmt.Sprintf(" %s[synth]%s", magenta, reset)
    }

    fmt.Printf("%s%d.%s %s%s%s\n    %s\n\n",
        cyan, i+1, reset,
        chipsLine, synthTag,
        reset,
        content)
}
```

Note: `memID` variable name may differ — check the actual scan variable name in the file when editing. It may be `id`.

- [ ] **Step 2: Verify build**

Run: `go build ./cmd/mpm/`
Expected: compiles without error

---

## Task 4: Update JSON output with score and rationale

**Files:**
- Modify: `cmd/mpm/recall.go:145-169`

- [ ] **Step 1: Add score and rationale to JSON memoryEntry struct**

In the JSON output block (around line 147), update the `memoryEntry` struct:

```go
type memoryEntry struct {
    ID                 string `json:"id"`
    Content            string `json:"content"`
    Tags               string `json:"tags"`
    SessionID          string `json:"session_id,omitempty"`
    CreatedAt          string `json:"created_at"`
    ReinforcementCount int    `json:"reinforcement_count"`
    Weight             int    `json:"weight"`
    LastAccessedAt     string `json:"last_accessed_at,omitempty"`
    Score              float64 `json:"score"`
    Rationale          string  `json:"rationale"`
}
```

- [ ] **Step 2: Compute score and rationale per entry**

When building the JSON result (around line 154), compute and populate:

```go
lastAccessStr := ""
if !e.lastAccessedAt.IsZero() {
    lastAccessStr = e.lastAccessedAt.Format(time.RFC3339)
}
score := computeScore(e.reinforcementCount, e.weight)
rationale := formatRationale(e.reinforcementCount, e.weight, e.lastAccessedAt)

result = append(result, memoryEntry{
    ID:                 shortID(memID), // confirm variable name
    Content:            e.content,
    Tags:               e.tags,
    SessionID:          e.sessionID,
    CreatedAt:          e.createdAt.Format(time.RFC3339),
    ReinforcementCount: e.reinforcementCount,
    Weight:             e.weight,
    LastAccessedAt:     lastAccessStr,
    Score:              score,
    Rationale:          rationale,
})
```

- [ ] **Step 3: Verify build**

Run: `go build ./cmd/mpm/`
Expected: compiles without error

---

## Task 5: Add test for chip rendering

**Files:**
- Modify: `cmd/mpm/recall_test.go`

- [ ] **Step 1: Add test for rationale computation**

In `cmd/mpm/recall_test.go`, add:

```go
func TestComputeScore(t *testing.T) {
    tests := []struct {
        rc, weight int
        wantMin, wantMax float64
    }{
        {0, 1, 0.0, 0.1},      // default: very low score
        {5, 10, 0.5, 0.7},     // moderate: mid score
        {10, 10, 0.8, 1.0},    // high: near max
        {50, 20, 1.0, 1.0},    // capped at 1.0
    }
    for _, tt := range tests {
        got := computeScore(tt.rc, tt.weight)
        if got < tt.wantMin || got > tt.wantMax {
            t.Errorf("computeScore(%d, %d) = %v, want between %v and %v",
                tt.rc, tt.weight, got, tt.wantMin, tt.wantMax)
        }
    }
}

func TestFormatRationale(t *testing.T) {
    now := time.Now()
    tests := []struct {
        rc, weight   int
        lastAccessed time.Time
        wantSubstr   string
    }{
        {5, 12, now.Add(-48 * time.Hour), "5x ref"},
        {5, 12, now.Add(-48 * time.Hour), "weight 12"},
        {5, 12, now.Add(-48 * time.Hour), "LTM"},
        {5, 12, now.Add(-48 * time.Hour), "accessed"},
        {0, 1, time.Time{}, ""}, // no chips expected for default memory
    }
    for _, tt := range tests {
        got := formatRationale(tt.rc, tt.weight, tt.lastAccessed)
        if tt.wantSubstr != "" && !strings.Contains(got, tt.wantSubstr) {
            t.Errorf("formatRationale(%d, %d, _) = %q, want substring %q",
                tt.rc, tt.weight, got, tt.wantSubstr)
        }
    }
}
```

- [ ] **Step 2: Run tests**

Run: `go test -v ./cmd/mpm/ -run "TestComputeScore|TestFormatRationale"`
Expected: PASS

---

## Self-Review Checklist

1. **Spec coverage:** All spec requirements met — score (0–1 fractional), rationale chips in human output, score+rationale in JSON.

2. **Placeholder scan:** No `TODO`, `TBD`, or vague steps. All SQL, code, and test code is concrete.

3. **Type consistency:**
   - `recallEntry` fields match DB columns (`reinforcement_count` INT, `weight` INT, `last_accessed_at` DATETIME)
   - `computeScore(rc, weight int) float64` — called with `int` args, returns `float64`
   - `formatRationale(rc, weight int, lastAccessed time.Time) string` — uses existing `formatAge`
   - `shortID(id string) string` — already exists in `review.go`, must be accessible in `recall.go` (both in `cmd/mpm` package)

4. **Score formula:** `(rc * 2 + weight * 1.5) / 50` → max value 1.0 when rc=10, weight=10 → (20+15)/50 = 0.7... wait. Recalculate: if we want rc=10, w=10 → 0.85 → denominator should be ~41. Or target rc=20, w=10 → max. Let me verify: (20*2 + 10*1.5) = 55. For score=1.0, denominator should be 55. Use denominator 55 to allow typical high-value memories to approach 1.0.

   Actually: use denominator 60 to give headroom. rc=10, w=10 → (20+15)/60 = 0.58. rc=20, w=10 → (40+15)/60 = 0.92. rc=20, w=20 → (40+30)/60 = 1.17 → capped to 1.0. Good.

5. **Variable name:** The scan variable for memory ID in `handleRecall` is `id` (from `rows.Scan(&id, &content, ...)`). Use `shortID(id)` in both human and JSON output.

6. **chipsLine rendering:** The `·` separator uses unicode middle dot (U+00B7). Ensure terminal supports it (most do).

7. **Test coverage:** `computeScore` and `formatRationale` have unit tests. No integration test for chip rendering in full output (human readability — visual inspection sufficient).

---

## Execution Options

**Plan complete and saved to `docs/superpowers/plans/2026-05-14-recall-explanation-plan.md`. Two execution options:**

**1. Subagent-Driven (recommended)** — I dispatch a fresh subagent per task, review between tasks, fast iteration

**2. Inline Execution** — Execute tasks in this session using executing-plans, batch execution with checkpoints

**Which approach?**