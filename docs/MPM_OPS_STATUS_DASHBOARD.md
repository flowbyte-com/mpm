# TASK: Implement `mpm ops status` — System Dashboard

We are adding a real-time system status dashboard to `mpm ops`, giving 808 and operators a single command to see the full health of the MPM layer at a glance.

---

## 1. Overview

Add `mpm ops status` as a subcommand under `ops`. It shows:
- Memory counts by collection
- Theory/decision counts with resolution status
- Watcher daemon status (PID liveness)
- Synthesis activity stats (total synthesized, last synthesis timestamp)
- LTM count (weight >= 10)
- Recent watchdog events (last 3 synthesis skips/failures)

Fast `COUNT(*)` queries only — dashboard renders in <50ms.

---

## 2. Implementation

### `ops` sub-router entry

In `cmd/mpm/router.go`, add to `opsSubcommands`:
```go
"status": handleStatus,
```

### `handleStatus` function

In `cmd/mpm/handlers.go`, add:

```go
func handleStatus(args []string) int {
    dm := getDM() // reuse existing DatabaseManager singleton
    printStatusDashboard(dm)
    return 0
}
```

### `printStatusDashboard(dm *DatabaseManager)`

Query and display:

```go
func printStatusDashboard(dm *DatabaseManager) {
    // Memory counts (fast COUNT queries)
    totalMemories, _ := countMemories(dm, "")
    ltmCount, _ := countMemories(dm, "weight >= 10")
    theoriesCount, _ := countMemories(dm, "collection = 'theories'")
    decisionsCount, _ := countMemories(dm, "collection = 'decisions'")
    activeTheories, _ := countTheoriesByStatus(dm, "pending")
    resolvedTheories, _ := countTheoriesByStatus(dm, "resolved")

    // Watcher daemon
    daemonStatus := getDaemonStatus() // reuse PID check from watch handlers

    // Synthesis stats (from watchdog log or a metadata query)
    synthCount, lastSynth := getSynthesisStats(dm)

    // Recent watchdog events (last 3)
    recentEvents := getRecentWatchdogEvents(dm, 3)

    // Output
    fmt.Println("⚡ MPM · System Status")
    fmt.Println("─────────────────────────────")
    fmt.Printf("Memories:  %d total | %d LTM\n", totalMemories, ltmCount)
    fmt.Printf("Theories:  %d total | %d pending | %d resolved\n", theoriesCount, activeTheories, resolvedTheories)
    fmt.Printf("Decisions: %d total\n", decisionsCount)
    fmt.Printf("Watcher:   %s\n", daemonStatus)
    fmt.Printf("Synthesis: %d merged | last: %s\n", synthCount, lastSynth)
    if len(recentEvents) > 0 {
        fmt.Println("─────────────────────────────")
        fmt.Println("Recent events:")
        for _, e := range recentEvents {
            fmt.Printf("  %s %s\n", e.op, e.detail)
        }
    }
    fmt.Println("─────────────────────────────")
    fmt.Println("Run `mpm help` for daily commands.")
    fmt.Println("Run `mpm ops help` for engine room.")
}
```

### Helper functions

```go
func countMemories(dm *DatabaseManager, where string) (int, error) {
    var query string
    var args []interface{}
    if where == "" {
        query = "SELECT COUNT(*) FROM memories WHERE deleted_at IS NULL"
    } else {
        query = "SELECT COUNT(*) FROM memories WHERE deleted_at IS NULL AND " + where
    }
    var count int
    err := dm.SQLDB().QueryRow(query, args...).Scan(&count)
    return count, err
}

func countTheoriesByStatus(dm *DatabaseManager, status string) (int, error) {
    var count int
    query := `SELECT COUNT(*) FROM memories
              WHERE collection = 'theories' AND deleted_at IS NULL
              AND json_extract(metadata, '$.status') = ?`
    err := dm.SQLDB().QueryRow(query, status).Scan(&count)
    return count, err
}

func getDaemonStatus() string {
    // Re-use existing PID check from watch handlers
    pidFile := filepath.Join(getMPMDir(), "watch.pid")
    data, err := os.ReadFile(pidFile)
    if err != nil {
        return "not running"
    }
    pid := strings.TrimSpace(string(data))
    if pid == "" {
        return "not running"
    }
    // Check process liveness
    process, err := os.FindProcess(pidToInt(pid))
    if err != nil || process == nil {
        return "not running"
    }
    // Sending signal 0 checks if process exists without killing it
    err = process.Signal(syscall.Signal(0))
    if err != nil {
        return "not running (PID " + pid + ")"
    }
    return "running (PID " + pid + ")"
}

func pidToInt(s string) int {
    v, _ := strconv.Atoi(s)
    return v
}

func getSynthesisStats(dm *DatabaseManager) (int, string) {
    var count int
    var lastTime string
    dm.SQLDB().QueryRow("SELECT COUNT(*), MAX(synthesized_at) FROM memories WHERE deleted_at IS NULL AND json_extract(metadata, '$.synthesized') = true").Scan(&count, &lastTime)
    if lastTime == "" {
        lastTime = "never"
    }
    return count, lastTime
}

type watchdogEvent struct {
    op     string
    detail string
}

func getRecentWatchdogEvents(dm *DatabaseManager, limit int) []watchdogEvent {
    // Read last N lines from watchdog.jsonl
    path := filepath.Join(getMPMDir(), "watchdog.jsonl")
    data, err := os.ReadFile(path)
    if err != nil {
        return nil
    }
    lines := strings.Split(string(data), "\n")
    if len(lines) > limit {
        lines = lines[len(lines)-limit:]
    }
    var events []watchdogEvent
    for _, line := range lines {
        if line == "" {
            continue
        }
        var m map[string]interface{}
        if json.Unmarshal([]byte(line), &m) != nil {
            continue
        }
        op := ""
        if v, ok := m["op"].(string); ok {
            op = v
        }
        detail := ""
        if v, ok := m["reason"].(string); ok {
            detail = v
        } else if v, ok := m["error"].(string); ok {
            detail = v
        }
        if op != "" {
            events = append(events, watchdogEvent{op: op, detail: detail})
        }
    }
    return events
}
```

---

## 3. Output Format

```
⚡ MPM · System Status
────────────────────────────────────
Memories:  142 total | 23 LTM
Theories:  4 total | 1 pending | 3 resolved
Decisions: 2 total
Watcher:   running (PID 12345)
Synthesis: 12 merged | last: 2026-05-19T14:30:00Z
────────────────────────────────────
Recent events:
  synthesize_skip insufficient candidates
  synthesize_skip insufficient candidates
────────────────────────────────────
Run `mpm help` for daily commands.
Run `mpm ops help` for engine room.
```

**Empty/sparse state:**
```
⚡ MPM · System Status
────────────────────────────────────
Memories:  0 total | 0 LTM
Theories:  0 total | 0 pending | 0 resolved
Decisions: 0 total
Watcher:   not running
Synthesis: 0 merged | last: never
────────────────────────────────────
Run `mpm help` for daily commands.
Run `mpm ops help` for engine room.
```

---

## 4. Files to Modify

- `cmd/mpm/router.go` — add `"status": handleStatus` to opsSubcommands
- `cmd/mpm/handlers.go` — add `handleStatus`, `printStatusDashboard`, helper functions
- All tests must pass

---

## 5. Verification

| Test | Expected |
|---|---|
| `mpm ops status` | Shows formatted dashboard with counts, daemon status, synthesis stats |
| Empty database | Shows all zeros, "not running" for watcher |
| Daemon running | Shows "running (PID N)" |
| `mpm help` | Unchanged — dashboard is NOT the default fallback |
| All tests pass | ✅ |

---

## 6. Constraint

This is `mpm ops status` — a specific subcommand, not the default fallback for `mpm`. The root `mpm` with no args continues to show `mpm help`. No DB queries on a bare `mpm` invocation.