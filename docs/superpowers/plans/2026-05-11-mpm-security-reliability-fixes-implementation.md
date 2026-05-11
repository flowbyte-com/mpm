# MPM Security & Reliability Fixes — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement four independent incremental fixes — layered API key regex, deferred VACUUM, remove topic cache, and Smart Fence Telegram chunker.

**Architecture:** Four independent changes across three packages. Each fix is self-contained with its own test.

**Tech Stack:** Go (MPM core + agent), SQLite FTS5, Telegram Bot API

---

## Task Map

| Task | Fix | File | Status |
|------|-----|------|--------|
| 1 | Fix 1: Layered regex | `internal/memory.go` | — |
| 2 | Fix 2: Deferred VACUUM | `cmd/mpm/handlers.go`, `cmd/mpm/maint_cmds.go` | — |
| 3 | Fix 3: Remove topic cache | `cmd/mpm/watch.go` | — |
| 4 | Fix 4: Smart Fence Telegram chunker | `mpm-agent/cmd/telegram/handler.go` | — |

Tasks 1–3 are in the MPM core. Task 4 is in mpm-agent — it is the only task that crosses the daemon boundary.

---

## Task 1: Layered Sensitive Pattern Detection

**File:** `internal/memory.go:435-455`

**Current state:**
```go
var sensitivePatterns = []struct {
    name    string
    pattern *regexp.Regexp
}{
    {"OpenAI API Key", regexp.MustCompile(`sk-[a-zA-Z0-9_-]{20,}`)},
    // ... existing GitHub/AWS/Slack/Stripe/JWT patterns ...
}
```

- [ ] **Step 1: Find the `sensitivePatterns` variable and show surrounding context (lines 430–460)**

Run: `sed -n '430,460p' /home/v/.openclaw/workspace/projects/mpm/internal/memory.go`

- [ ] **Step 2: Edit the OpenAI API Key entry to add layered patterns**

Replace the single OpenAI entry with ordered entries:

```go
var sensitivePatterns = []struct {
    name    string
    pattern *regexp.Regexp
}{
    // Layered: specific prefixes first, general fallback last
    {"OpenAI Project Key", regexp.MustCompile(`sk-proj-[a-zA-Z0-9_-]{20,}`)},
    {"OpenAI Service Key", regexp.MustCompile(`sk-svc-[a-zA-Z0-9_-]{20,}`)},
    {"Anthropic API Key", regexp.MustCompile(`sk-ant-[a-zA-Z0-9_-]{20,}`)},
    {"Generic Secret Key", regexp.MustCompile(`sk-[a-zA-Z0-9_-]{20,}`)},
    {"GitHub Personal Token", regexp.MustCompile(`ghp_[a-zA-Z0-9]{36}`)},
    {"GitHub OAuth Token", regexp.MustCompile(`gho_[a-zA-Z0-9]{36}`)},
    {"GitHub Refresh Token", regexp.MustCompile(`ghr_[a-zA-Z0-9]{72}`)},
    {"AWS Access Key ID", regexp.MustCompile(`AKIA[A-Z0-9]{16}`)},
    {"Slack Token", regexp.MustCompile(`xox[baprs]-[0-9]+-[0-9]+`)},
    {"Stripe API Key", regexp.MustCompile(`sk_live_[0-9a-zA-Z]{24,}`)},
    {"Stripe Test Key", regexp.MustCompile(`sk_test_[0-9a-zA-Z]{24,}`)},
    {"JWT Token", regexp.MustCompile(`eyJ[a-zA-Z0-9_-]*\.eyJ[a-zA-Z0-9_-]*\.[a-zA-Z0-9_-]*`)},
    {"General API Key", regexp.MustCompile(`(?i)(api[_-]?key|apikey)[=:]\s*[^\s]+`)},
    {"Password", regexp.MustCompile(`(?i)(password|passwd|pwd)[=:]\s*[^\s]+`)},
    {"Secret", regexp.MustCompile(`(?i)(secret|token)[=:]\s*[^\s]+`)},
    {"Private Key", regexp.MustCompile(`-----BEGIN\s+(RSA\s+)?PRIVATE\s+KEY-----`)},
    {"SSH Key", regexp.MustCompile(`-----BEGIN\s+OPENSSH\s+KEY-----`)},
    {"Bearer Token", regexp.MustCompile(`(?i)bearer\s+[a-zA-Z0-9_-]{20,}`)},
    {"Database Connection", regexp.MustCompile(`(?i)(mysql|postgres|mongodb|redis)://[^\s]+`)},
}
```

- [ ] **Step 3: Run existing tests to verify no regressions**

Run: `go test -v ./internal/... -run TestSensitive 2>&1 | head -30`
Expected: No FAIL (if no matching tests, pass silently)

- [ ] **Step 4: Commit**

```bash
git add internal/memory.go
git commit -m "fix: layered API key patterns (sk-proj-, sk-svc-, sk-ant-) with ordered matching"
```

---

## Task 2: Deferred VACUUM

**Files:**
- Modify: `cmd/mpm/handlers.go:436-441, 467-472, 509-510, 652`
- Modify: `cmd/mpm/maint_cmds.go` (add VACUUM to `RunSelfMaintenance`)

**Current state of shred handlers:** Each calls `db.Exec("VACUUM")` synchronously after DELETE.

The VACUUM calls to remove:
- `handlers.go:436-441` — `handleShredSessions`
- `handlers.go:467-472` — `handleShredMemories`
- `handlers.go:509-510` — `handleShredTopics`
- `handlers.go:652` — `handleShredDatabase`

- [ ] **Step 1: Find all four VACUUM locations in handlers.go**

Run: `grep -n "VACUUM" /home/v/.openclaw/workspace/projects/mpm/cmd/mpm/handlers.go`

Expected output:
```
436: // Run VACUUM to reclaim space
438: if _, err := db.Exec("VACUUM"); err != nil {
467: // Run VACUUM to reclaim space
469: if _, err := db.Exec("VACUUM"); err != nil {
509: // Run VACUUM to reclaim space
510: if _, err := db.Exec("VACUUM"); err != nil {
652: _, err = db.Exec("VACUUM")
```

- [ ] **Step 2: Remove VACUUM from handleShredSessions (lines 436–441)**

Edit `handlers.go` to remove:
```go
// Run VACUUM to reclaim space
db := store.DB
if _, err := db.Exec("VACUUM"); err != nil {
    sendResponse(conn, "", fmt.Sprintf("Sessions deleted but vacuum failed: %v", err), true, 1)
    return
}
```
Replace with a comment:
```go
// Space reclamation happens during maintenance cycle (deferred VACUUM)
```

- [ ] **Step 3: Remove VACUUM from handleShredMemories (lines 467–472)**

Same pattern — remove the VACUUM block, add comment.

- [ ] **Step 4: Remove VACUUM from handleShredTopics (lines 509–510)**

Remove only `if _, err := db.Exec("VACUUM"); err != nil {` and the following `return` + error line.

- [ ] **Step 5: Remove VACUUM from handleShredDatabase (line 652)**

Remove the line `_, err = db.Exec("VACUUM")`.

- [ ] **Step 6: Add deferred VACUUM to RunSelfMaintenance in maint_cmds.go**

Find where `RunSelfMaintenance` is called (line 309). After the existing maintenance block, add:

```go
// Deferred VACUUM: reclaim space from hard deletes during idle maintenance
// Only run if there are deleted (inactive) records to vacuum
var deletedCount int
dm.DB().QueryRow("SELECT COUNT(*) FROM memories WHERE deleted_at IS NOT NULL").Scan(&deletedCount)
if deletedCount > 0 {
    if _, vacErr := dm.DB().Exec("PRAGMA incremental_vacuum"); vacErr != nil {
        fmt.Fprintf(os.Stderr, "Warning: incremental_vacuum failed: %v\n", vacErr)
    } else {
        fmt.Printf("   Vacuum reclaimed space (%d deleted records)\n", deletedCount)
    }
}
```

Note: The `dm.DB()` access assumes `DatabaseManager` exposes the raw `*sql.DB`. Verify that `NewDatabaseManager` returns a `*DatabaseManager` with a `DB()` accessor method. If not, use `dm.SQLDB()` (which is already used in `watch.go:1408`).

- [ ] **Step 7: Verify the build compiles**

Run: `cd /home/v/.openclaw/workspace/projects/mpm && go build ./cmd/mpm/...`
Expected: No errors

- [ ] **Step 8: Commit**

```bash
git add cmd/mpm/handlers.go cmd/mpm/maint_cmds.go
git commit -m "fix: defer VACUUM to maintenance cycle, prevent Watch daemon lockout"
```

---

## Task 3: Remove Topic Cache

**File:** `cmd/mpm/watch.go`

Remove `topicCache map[string]*topicCluster` from `watcherDaemon` struct and all references in `checkTopicClustering()`.

- [ ] **Step 1: Show the watcherDaemon struct (lines 479–489)**

Run: `sed -n '479,489p' /home/v/.openclaw/workspace/projects/mpm/cmd/mpm/watch.go`

Expected:
```go
type watcherDaemon struct {
    watcher    *fsnotify.Watcher
    dirs       []string
    db         *mpminternal.DatabaseManager
    memory     *mpminternal.MemoryStore
    dryRun     bool
    verbose    bool
    mu         sync.Mutex
    stopCh     chan struct{}
    topicCache map[string]*topicCluster // topic name -> cluster info
}
```

- [ ] **Step 2: Remove `topicCache` from the struct**

Remove the line:
```go
topicCache map[string]*topicCluster // topic name -> cluster info
```

- [ ] **Step 3: Show the newWatcherDaemon function to remove topicCache initialization**

Run: `grep -n "topicCache" /home/v/.openclaw/workspace/projects/mpm/cmd/mpm/watch.go`

Expected locations:
- Line 488: struct field
- Line 526: `topicCache: make(map[string]*topicCluster),` in `newWatcherDaemon`
- Lines 1342–1366: cache reads in `checkTopicClustering`
- Lines 1382–1391: cache writes in `checkTopicClustering`

- [ ] **Step 4: Remove topicCache from newWatcherDaemon (line 526)**

Remove: `topicCache: make(map[string]*topicCluster),`

- [ ] **Step 5: Show checkTopicClustering in full (lines 1309–1393)**

Run: `sed -n '1309,1393p' /home/v/.openclaw/workspace/projects/mpm/cmd/mpm/watch.go`

- [ ] **Step 6: Rewrite checkTopicClustering to remove all topicCache references**

The function currently:
- Reads from `d.topicCache` to check if tag already cached (line 1342)
- Updates `d.topicCache[tag]` in-place for existing clusters (lines 1343–1346)
- Writes new entries to `d.topicCache` after creating topics (lines 1359–1366, 1383–1391)

Replace the entire `checkTopicClustering` function with a version that:
1. Queries `topic_memberships` table directly for existing topic-tag associations
2. Removes all `d.topicCache` reads/writes
3. Still uses `d.mu.Lock()/Unlock()` for the duration of the DB query + topic creation

The rewritten function skeleton:
```go
func (d *watcherDaemon) checkTopicClustering() {
    const clusterThreshold = 3
    d.mu.Lock()
    defer d.mu.Unlock()

    // Query DB directly — no in-memory cache
    ltmMemories, err := d.getLTMMemories()
    if err != nil {
        return
    }

    tagCounts := make(map[string][]string)
    for _, mem := range ltmMemories {
        for _, tag := range mem.Tags {
            tagCounts[tag] = append(tagCounts[tag], mem.ID)
        }
    }

    for tag, memIDs := range tagCounts {
        if len(memIDs) < clusterThreshold {
            continue
        }
        // Check if topic exists in DB (no cache)
        existingTopic, err := d.findTopicByTag(tag)
        if err == nil && existingTopic != nil {
            // Topic exists in DB — no action needed
            continue
        }
        // Create new topic from cluster
        _, err = d.createTopicFromCluster(tag, memIDs)
        if err != nil {
            continue
        }
        fmt.Printf("   🏷️  Topic auto-created: '%s' (tag: %s, %d memories)\n", formatTopicName(tag), tag, len(memIDs))
    }
}
```

- [ ] **Step 7: Verify the build compiles**

Run: `cd /home/v/.openclaw/workspace/projects/mpm && go build ./cmd/mpm/...`
Expected: No errors

- [ ] **Step 8: Run existing tests**

Run: `go test -v ./cmd/mpm/... 2>&1 | head -50`
Expected: No FAIL

- [ ] **Step 9: Commit**

```bash
git add cmd/mpm/watch.go
git commit -m "fix: remove in-memory topic cache, query DB directly for clustering"
```

---

## Task 4: Smart Fence Telegram Chunker

**File:** `mpm-agent/cmd/telegram/handler.go`

The current `sendLongText` at line 1206 splits on `\n\n` paragraph boundaries but has no awareness of markdown code fences. Replace it with a Smart Fence chunker.

- [ ] **Step 1: Show the current sendLongText function (lines 1206–1236)**

Run: `sed -n '1206,1236p' /home/v/.openclaw/workspace/projects/mpm/mpm-agent/cmd/telegram/handler.go`

- [ ] **Step 2: Add a new SmartFenceChunk function above sendLongText**

Add this function before `sendLongText`:

```go
// SmartFenceChunk splits text into Telegram-safe chunks while preserving
// balanced markdown code fences. Code blocks of any size are split with
// properly balanced opening/closing fences across chunk boundaries.
func SmartFenceChunk(text string, maxLen int) []string {
    if len(text) <= maxLen {
        return []string{text}
    }

    const safetyMargin = 196 // keeps us well under 4096 even with fence overhead
    effectiveMax := maxLen - safetyMargin

    var chunks []string
    var buf strings.Builder
    var currentLang string
    inCodeBlock := false

    lines := strings.Split(text, "\n")
    for i, line := range lines {
        isFence := strings.HasPrefix(line, "```")

        if isFence {
            if !inCodeBlock {
                // Opening fence
                inCodeBlock = true
                lang := strings.TrimPrefix(strings.TrimSpace(line), "```")
                if lang != "" {
                    currentLang = lang
                }
            } else {
                // Closing fence
                inCodeBlock = false
                currentLang = ""
            }
        }

        // Check if adding this line would exceed the limit
        candidate := line
        if buf.Len() > 0 && buf.Len()+1+len(candidate) > effectiveMax {
            // Need to emit current chunk
            if inCodeBlock {
                // Close the code block before emitting
                buf.WriteString("\n```")
            }
            chunks = append(chunks, buf.String())
            buf.Reset()

            if inCodeBlock {
                // Reopen the code block in the next chunk
                langPrefix := ""
                if currentLang != "" {
                    langPrefix = currentLang + "\n"
                }
                buf.WriteString("```" + langPrefix + line + "\n")
                continue
            }
        }

        if buf.Len() > 0 {
            buf.WriteString("\n")
        }
        buf.WriteString(line)
    }

    // Emit final chunk
    if buf.Len() > 0 {
        if inCodeBlock {
            buf.WriteString("\n```")
        }
        chunks = append(chunks, buf.String())
    }

    if len(chunks) == 0 {
        return []string{text}
    }
    return chunks
}
```

- [ ] **Step 3: Verify the function compiles (syntax check)**

Run: `cd /home/v/.openclaw/workspace/projects/mpm/mpm-agent && go build ./cmd/telegram/... 2>&1`
Expected: No errors

- [ ] **Step 4: Update sendLongText to use SmartFenceChunk**

Replace the body of `sendLongText` (lines 1206–1236) with:

```go
func (h *Handler) sendLongText(ctx *th.Context, chatID int64, text string) {
    const maxLen = 4096
    chunks := SmartFenceChunk(text, maxLen)
    for i, chunk := range chunks {
        h.sendText(ctx, chatID, chunk)
        if i < len(chunks)-1 {
            time.Sleep(150 * time.Millisecond)
        }
    }
}
```

- [ ] **Step 5: Verify the build compiles**

Run: `cd /home/v/.openclaw/workspace/projects/mpm/mpm-agent && go build ./cmd/telegram/...`
Expected: No errors

- [ ] **Step 6: Commit**

```bash
git add mpm-agent/cmd/telegram/handler.go
git commit -m "fix: Smart Fence chunker for Telegram — balanced markdown fences across message boundaries"
```

---

## Implementation Order

Tasks 1–3 are independent and can be implemented in any order (or in parallel via subagents). Task 4 is in a separate package (`mpm-agent`) and can run concurrently with Tasks 1–3.

Recommended parallel approach:
- Subagent A: Task 1 (Fix 1: layered regex)
- Subagent B: Task 2 (Fix 2: deferred VACUUM)
- Subagent C: Task 3 (Fix 3: remove topic cache)
- Subagent D: Task 4 (Fix 4: Smart Fence Telegram chunker) — runs concurrently with A/B/C

All four must pass build before any are merged.