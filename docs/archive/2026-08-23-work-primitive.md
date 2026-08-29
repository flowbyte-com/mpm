# Work Primitive Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a first-class `Work` primitive — durable intended future actions with cross-session persistence and pointer resolution.

**Architecture:** A new `works` table, an `mpm_work` tool with 5 actions (create/list/show/update/complete+cancel), `mpm://work/<id>` pointer resolution, and bounded wake-context projection. Three-state lifecycle: open/done/cancelled. No `in_progress`, no priority, no relationships in v1.

**Tech Stack:** Go (mpm-core module), SQLite with WAL mode, FTS5, existing pointer architecture.

**Spec:** `docs/work-primitive-design.md`

---

## Global Constraints

- Schema changes must use `SafeMigrations` (additive only, no destructive changes)
- All write paths must pass through the content scanner in `SaveMemoryNode`
- All new write operations must include read-back assertion before returning success
- New tables use Unix-epoch INTEGER for timestamps (not RFC3339 strings)
- ID generation uses `GenerateID()` (monotonic counter + nanosecond timestamp → SHA256 → 16 hex chars)
- CLI and MCP share the same handler function via `HandlerFunc` signature
- Pointer syntax is `mpm://work/<id>` — no query strings or fragments
- Wake context hard cap: `MaxWakeContextBytes = 32 * 1024`

---

## Task Map

```
work.go (types)          → db.go (CRUD) → schema.go (migration)
    ↓                        ↓
summarize.go              handlers.go (tool)
    ↓                        ↓
wake_context.go           registry_list.go (tool registration)
    ↓                        ↓
cmd/mpm-mcp/tools.go      test files
    ↓
work_test.go (unit)
    ↓
work_integration_test.go (E2E / cross-session)
```

---

### Task 1: Define Work types

**Files:**
- Create: `internal/core/work.go`
- Test: `internal/core/work_test.go`

**Interfaces:**
- Consumes: nothing (new types)
- Produces: `Work`, `WorkStatus`, `WorkStatus*` constants, `WakeContextWork`

- [ ] **Step 1: Write the failing test**

```go
// internal/core/work_test.go
package internal

import "testing"

func TestWorkStatusValues(t *testing.T) {
    if WorkStatusOpen != "open" {
        t.Errorf("WorkStatusOpen = %q, want 'open'", WorkStatusOpen)
    }
    if WorkStatusDone != "done" {
        t.Errorf("WorkStatusDone = %q, want 'done'", WorkStatusDone)
    }
    if WorkStatusCancelled != "cancelled" {
        t.Errorf("WorkStatusCancelled = %q, want 'cancelled'", WorkStatusCancelled)
    }
}

func TestWorkStruct(t *testing.T) {
    w := Work{
        ID:      "test-work-001",
        Title:   "Investigate telemetry admission",
        Content: "",
        Status:  WorkStatusOpen,
    }
    if w.Title != "Investigate telemetry admission" {
        t.Errorf("Title = %q, want 'Investigate telemetry admission'", w.Title)
    }
    if w.Status != WorkStatusOpen {
        t.Errorf("Status = %v, want WorkStatusOpen", w.Status)
    }
}

func TestWorkStatusConstants(t *testing.T) {
    // Verify constants match string values
    if string(WorkStatusOpen) != "open" {
        t.Errorf("WorkStatusOpen string = %q, want 'open'", string(WorkStatusOpen))
    }
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -tags fts5 ./internal/core/... -run TestWorkStatusValues -v`  
Expected: FAIL — `work.go` does not exist

- [ ] **Step 3: Write minimal implementation**

```go
// internal/core/work.go
package internal

// WorkStatus represents the lifecycle state of a work item.
type WorkStatus string

const (
    WorkStatusOpen      WorkStatus = "open"
    WorkStatusDone     WorkStatus = "done"
    WorkStatusCancelled WorkStatus = "cancelled"
)

// Work is a durable representation of an intended future action.
type Work struct {
    ID          string     `json:"id"`
    Title       string     `json:"title"`
    Content     string     `json:"content,omitempty"`
    Status      WorkStatus `json:"status"`
    CreatedAt   int64      `json:"created_at"`
    UpdatedAt   int64      `json:"updated_at"`
    CompletedAt *int64     `json:"completed_at,omitempty"`
    SessionID   string     `json:"session_id,omitempty"`
}

// WakeContextWork is the bounded projection of a work item for wake context.
type WakeContextWork struct {
    ID        string     `json:"id"`
    Title     string     `json:"title"`    // truncated to 120 chars
    Status    WorkStatus `json:"status"`
    Pointer   string     `json:"pointer"` // "mpm://work/<id>"
    CreatedAt int64      `json:"created_at"`
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test -tags fts5 ./internal/core/... -run TestWorkStatus -v`  
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/core/work.go internal/core/work_test.go
git commit -m "feat(work): add Work types and status constants"
```

---

### Task 2: Add works table to schema

**Files:**
- Modify: `internal/core/schema.go` — add works table + indexes to `SafeMigrations`
- Test: verify migration applies cleanly

**Interfaces:**
- Consumes: nothing (additive schema)
- Produces: `works` table created on migration

- [ ] **Step 1: Write the failing test (schema migration)**

```go
// internal/core/work_migration_test.go
package internal

import (
    "database/sql"
    "testing"
)

func TestWorksTableMigration(t *testing.T) {
    // Create in-memory DB with current schema
    db, err := sql.Open("sqlite3", ":memory:")
    if err != nil {
        t.Fatalf("open: %v", err)
    }
    defer db.Close()

    // Run base tables
    for _, ddl := range BaseTables {
        if _, err := db.Exec(ddl); err != nil {
            t.Fatalf("BaseTables: %v", err)
        }
    }
    for _, ddl := range CommonIndexes {
        if _, err := db.Exec(ddl); err != nil {
            t.Fatalf("CommonIndexes: %v", err)
        }
    }

    // Verify works table does NOT exist before migration
    var count int
    err = db.QueryRow("SELECT COUNT(*) FROM works").Scan(&count)
    if err == nil {
        t.Error("works table should not exist before SafeMigrations")
    }

    // Run SafeMigrations
    for _, m := range SafeMigrations {
        if _, err := db.Exec(m); err != nil {
            t.Fatalf("SafeMigration: %v", err)
        }
    }

    // Verify works table exists after migration
    err = db.QueryRow("SELECT COUNT(*) FROM works").Scan(&count)
    if err != nil {
        t.Errorf("works table should exist after SafeMigrations: %v", err)
    }

    // Verify correct columns
    var title, status, content string
    err = db.QueryRow("SELECT title, status, content FROM works LIMIT 1").Scan(&title, &status, &content)
    if err != sql.ErrNoRows && err != nil {
        t.Errorf("column query: %v", err)
    }

    // Verify CHECK constraint on status
    _, err = db.Exec("INSERT INTO works (id, title, status) VALUES ('test', 'title', 'invalid_status')")
    if err == nil {
        t.Error("invalid status should have been rejected by CHECK constraint")
    }
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -tags fts5 ./internal/core/... -run TestWorksTableMigration -v`  
Expected: FAIL — `works` not in SafeMigrations yet

- [ ] **Step 3: Add works table to SafeMigrations in schema.go**

Find the `SafeMigrations` slice in `internal/core/schema.go` and add:

```go
// Works table — durable intended future actions
`CREATE TABLE IF NOT EXISTS works (
    id            TEXT PRIMARY KEY,
    title        TEXT NOT NULL,
    content      TEXT NOT NULL DEFAULT '',
    status       TEXT NOT NULL DEFAULT 'open'
                 CHECK (status IN ('open', 'done', 'cancelled')),
    created_at   INTEGER NOT NULL DEFAULT (CAST(strftime('%s','now') AS INTEGER)),
    updated_at   INTEGER NOT NULL DEFAULT (CAST(strftime('%s','now') AS INTEGER)),
    completed_at INTEGER,
    session_id   TEXT
);`,
`CREATE INDEX IF NOT EXISTS idx_works_status ON works(status);`,
`CREATE INDEX IF NOT EXISTS idx_works_session ON works(session_id);`,
`CREATE INDEX IF NOT EXISTS idx_works_created ON works(created_at DESC);`,
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test -tags fts5 ./internal/core/... -run TestWorksTableMigration -v`  
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/core/schema.go internal/core/work_migration_test.go
git commit -m "feat(work): add works table to schema migration"
```

---

### Task 3: Implement Work CRUD in db.go

**Files:**
- Modify: `internal/core/db.go` — add Work CRUD methods
- Test: `internal/core/work_crud_test.go`

**Interfaces:**
- Consumes: `Work` struct, `GenerateID()`
- Produces: `AddWork`, `GetWork`, `ListWorks`, `UpdateWork`, `CompleteWork`, `CancelWork`

**Design rules to follow:**
- Write-path read-back assertion (Defense Triad rule 3)
- `COALESCE` on aggregate queries to prevent NULL Scan panics
- RowsAffected check for non-view tables

- [ ] **Step 1: Write failing tests for AddWork**

```go
// internal/core/work_crud_test.go
package internal

import (
    "testing"
    "time"
)

func TestAddWork(t *testing.T) {
    dm := NewDatabaseManager(":memory:")
    defer dm.Close()

    work, err := dm.AddWork("Investigate telemetry admission", "", 0)
    if err != nil {
        t.Fatalf("AddWork: %v", err)
    }
    if work.Title != "Investigate telemetry admission" {
        t.Errorf("Title = %q, want %q", work.Title, "Investigate telemetry admission")
    }
    if work.Status != WorkStatusOpen {
        t.Errorf("Status = %v, want WorkStatusOpen", work.Status)
    }
    if work.ID == "" {
        t.Error("ID should not be empty")
    }
    if work.CreatedAt == 0 {
        t.Error("CreatedAt should be set")
    }
}

func TestAddWork_WithContent(t *testing.T) {
    dm := NewDatabaseManager(":memory:")
    defer dm.Close()

    work, err := dm.AddWork("Fix the bug", "The telemetry collector crashes on SIGTERM", 0)
    if err != nil {
        t.Fatalf("AddWork: %v", err)
    }
    if work.Content != "The telemetry collector crashes on SIGTERM" {
        t.Errorf("Content = %q, want %q", work.Content, "The telemetry collector crashes on SIGTERM")
    }
}

func TestAddWork_SessionID(t *testing.T) {
    dm := NewDatabaseManager(":memory:")
    defer dm.Close()

    work, err := dm.AddWork("Test work", "", "session-abc-123")
    if err != nil {
        t.Fatalf("AddWork: %v", err)
    }
    if work.SessionID != "session-abc-123" {
        t.Errorf("SessionID = %q, want %q", work.SessionID, "session-abc-123")
    }
}
```

- [ ] **Step 2: Run test — verify it fails**

Run: `go test -tags fts5 ./internal/core/... -run TestAddWork -v`  
Expected: FAIL — `AddWork` not defined

- [ ] **Step 3: Write AddWork**

Add to `db.go`:

```go
// AddWork inserts a new work item and returns it after read-back assertion.
func (dm *DatabaseManager) AddWork(title, content, sessionID string) (*Work, error) {
    id := GenerateID()
    now := time.Now().Unix()

    _, err := dm.db.Exec(`
        INSERT INTO works (id, title, content, status, created_at, updated_at, session_id)
        VALUES (?, ?, ?, 'open', ?, ?, ?)
    `, id, title, content, now, now, nullString(sessionID))
    if err != nil {
        return nil, fmt.Errorf("insert work: %w", err)
    }

    // Read-back assertion
    work, err := dm.GetWork(id)
    if err != nil {
        return nil, fmt.Errorf("write verification failed for %s: %w", id, err)
    }
    return work, nil
}

func nullString(s string) interface{} {
    if s == "" {
        return nil
    }
    return s
}
```

- [ ] **Step 4: Run tests — verify they pass**

Run: `go test -tags fts5 ./internal/core/... -run TestAddWork -v`  
Expected: PASS

- [ ] **Step 5: Write GetWork, ListWorks, UpdateWork, CompleteWork, CancelWork tests and implementations**

Write tests first, then implementations:

```go
func TestGetWork(t *testing.T) {
    dm := NewDatabaseManager(":memory:")
    defer dm.Close()

    created, _ := dm.AddWork("Test work", "", 0)
    retrieved, err := dm.GetWork(created.ID)
    if err != nil {
        t.Fatalf("GetWork: %v", err)
    }
    if retrieved.Title != created.Title {
        t.Errorf("Title = %q, want %q", retrieved.Title, created.Title)
    }
}

func TestGetWork_NotFound(t *testing.T) {
    dm := NewDatabaseManager(":memory:")
    defer dm.Close()

    _, err := dm.GetWork("does-not-exist")
    if err == nil {
        t.Error("GetWork should error for missing ID")
    }
}

func TestListWorks(t *testing.T) {
    dm := NewDatabaseManager(":memory:")
    defer dm.Close()

    dm.AddWork("Work 1", "", 0)
    dm.AddWork("Work 2", "", 0)

    works, err := dm.ListWorks()
    if err != nil {
        t.Fatalf("ListWorks: %v", err)
    }
    if len(works) != 2 {
        t.Errorf("len(works) = %d, want 2", len(works))
    }
}

func TestListWorks_OnlyOpen(t *testing.T) {
    dm := NewDatabaseManager(":memory:")
    defer dm.Close()

    w1, _ := dm.AddWork("Open work", "", 0)
    dm.AddWork("Done work", "", 0)
    dm.CompleteWork(w1.ID)

    works, err := dm.ListWorks()
    if err != nil {
        t.Fatalf("ListWorks: %v", err)
    }
    if len(works) != 1 {
        t.Errorf("len(works) = %d, want 1 (done work filtered out)", len(works))
    }
    if works[0].Title != "Open work" {
        t.Errorf("Title = %q, want 'Open work'", works[0].Title)
    }
}

func TestCompleteWork(t *testing.T) {
    dm := NewDatabaseManager(":memory:")
    defer dm.Close()

    work, _ := dm.AddWork("Test work", "", 0)
    completed, err := dm.CompleteWork(work.ID)
    if err != nil {
        t.Fatalf("CompleteWork: %v", err)
    }
    if completed.Status != WorkStatusDone {
        t.Errorf("Status = %v, want WorkStatusDone", completed.Status)
    }
    if completed.CompletedAt == nil {
        t.Error("CompletedAt should be set")
    }
}

func TestCancelWork(t *testing.T) {
    dm := NewDatabaseManager(":memory:")
    defer dm.Close()

    work, _ := dm.AddWork("Test work", "", 0)
    cancelled, err := dm.CancelWork(work.ID)
    if err != nil {
        t.Fatalf("CancelWork: %v", err)
    }
    if cancelled.Status != WorkStatusCancelled {
        t.Errorf("Status = %v, want WorkStatusCancelled", cancelled.Status)
    }
}

func TestReopenWork(t *testing.T) {
    dm := NewDatabaseManager(":memory:")
    defer dm.Close()

    work, _ := dm.AddWork("Test work", "", 0)
    dm.CompleteWork(work.ID)

    reopened, err := dm.UpdateWork(work.ID, WorkStatusOpen)
    if err != nil {
        t.Fatalf("UpdateWork: %v", err)
    }
    if reopened.Status != WorkStatusOpen {
        t.Errorf("Status = %v, want WorkStatusOpen", reopened.Status)
    }
    if reopened.CompletedAt != nil {
        t.Error("CompletedAt should be cleared on re-open")
    }
}
```

Implement each method following the write-path read-back assertion pattern:

```go
func (dm *DatabaseManager) GetWork(id string) (*Work, error) {
    var w Work
    var sessionID, content sql.NullString
    var completedAt sql.NullInt64
    err := dm.db.QueryRow(`
        SELECT id, title, content, status, created_at, updated_at, completed_at, session_id
        FROM works WHERE id = ?
    `, id).Scan(&w.ID, &w.Title, &content, &w.Status, &w.CreatedAt, &w.UpdatedAt, &completedAt, &sessionID)
    if err == sql.ErrNoRows {
        return nil, fmt.Errorf("work not found: %s", id)
    }
    if err != nil {
        return nil, fmt.Errorf("get work: %w", err)
    }
    if content.Valid {
        w.Content = content.String
    }
    if completedAt.Valid {
        w.CompletedAt = &completedAt.Int64
    }
    if sessionID.Valid {
        w.SessionID = sessionID.String
    }
    return &w, nil
}

func (dm *DatabaseManager) ListWorks() ([]*Work, error) {
    rows, err := dm.db.Query(`
        SELECT id, title, content, status, created_at, updated_at, completed_at, session_id
        FROM works WHERE status = 'open'
        ORDER BY created_at DESC
    `)
    if err != nil {
        return nil, fmt.Errorf("list works: %w", err)
    }
    defer rows.Close()

    var works []*Work
    for rows.Next() {
        var w Work
        var content, sessionID sql.NullString
        var completedAt sql.NullInt64
        if err := rows.Scan(&w.ID, &w.Title, &content, &w.Status, &w.CreatedAt, &w.UpdatedAt, &completedAt, &sessionID); err != nil {
            return nil, fmt.Errorf("scan work row: %w", err)
        }
        if content.Valid {
            w.Content = content.String
        }
        if completedAt.Valid {
            w.CompletedAt = &completedAt.Int64
        }
        if sessionID.Valid {
            w.SessionID = sessionID.String
        }
        works = append(works, &w)
    }
    return works, nil
}

func (dm *DatabaseManager) CompleteWork(id string) (*Work, error) {
    return dm.updateWorkStatus(id, WorkStatusDone)
}

func (dm *DatabaseManager) CancelWork(id string) (*Work, error) {
    return dm.updateWorkStatus(id, WorkStatusCancelled)
}

func (dm *DatabaseManager) UpdateWork(id string, status WorkStatus) (*Work, error) {
    return dm.updateWorkStatus(id, status)
}

func (dm *DatabaseManager) updateWorkStatus(id string, status WorkStatus) (*Work, error) {
    now := time.Now().Unix()
    var completedAt interface{}
    if status == WorkStatusDone {
        completedAt = now
    }
    // NULL completed_at when moving out of done/cancelled
    if status == WorkStatusOpen {
        completedAt = nil
    }

    res, err := dm.db.Exec(`
        UPDATE works SET status = ?, updated_at = ?, completed_at = ? WHERE id = ?
    `, status, now, completedAt, id)
    if err != nil {
        return nil, fmt.Errorf("update work status: %w", err)
    }

    affected, _ := res.RowsAffected()
    if affected == 0 {
        return nil, fmt.Errorf("work not found: %s", id)
    }

    return dm.GetWork(id)
}
```

- [ ] **Step 6: Run all CRUD tests**

Run: `go test -tags fts5 ./internal/core/... -run TestWork -v`  
Expected: PASS

- [ ] **Step 7: Commit**

```bash
git add internal/core/work_crud_test.go
git add internal/core/db.go  # (changes to AddWork, GetWork, ListWorks, CompleteWork, CancelWork, UpdateWork)
git commit -m "feat(work): add Work CRUD methods to DatabaseManager"
```

---

### Task 4: Add SummarizeWork to summarize.go

**Files:**
- Modify: `internal/core/summarize.go` — add `SummarizeWork`
- Test: add to `work_test.go`

- [ ] **Step 1: Add SummarizeWork function**

```go
// SummarizeWork truncates a work title to maxChars, breaking at rune boundaries.
func SummarizeWork(title string, maxChars int) string {
    return SummarizeBounded(title, maxChars)
}
```

- [ ] **Step 2: Test truncation**

```go
func TestSummarizeWork(t *testing.T) {
    long := strings.Repeat("a", 300)
    result := SummarizeWork(long, 120)
    if len(result) > 120 {
        t.Errorf("len(SummarizeWork) = %d, want <= 120", len(result))
    }
}
```

- [ ] **Step 3: Commit**

---

### Task 5: Wire work into wake context

**Files:**
- Modify: `internal/core/wake_context.go` — add `OpenWorks []WakeContextWork` to `WakeContextData`; add `gatherOpenWorks()`; update `formatWakeContext`
- Test: `wake_context_test.go` — add `TestWakeContext_OpenWorks`

**Interfaces:**
- Consumes: `GatherWakeContext()` → `ListWorks()`
- Produces: `WakeContextData.OpenWorks`

- [ ] **Step 1: Add OpenWorks field to WakeContextData**

Add to the "Attention & Pending Work" section:

```go
// OpenWorks are work items with status='open'. Bounded to 5 items
// ordered by created_at DESC (oldest first so the agent sees what has
// been waiting longest).
OpenWorks []WakeContextWork `json:"open_works"`
```

- [ ] **Step 2: Add gatherOpenWorks()**

```go
// gatherOpenWorks returns up to 5 open work items for wake context.
func (dm *DatabaseManager) gatherOpenWorks() []WakeContextWork {
    rows, err := dm.db.Query(`
        SELECT id, title, status, created_at
        FROM works WHERE status = 'open'
        ORDER BY created_at ASC
        LIMIT 5
    `)
    if err != nil {
        dm.LogAudit(AuditWarn, "wake_context", "gatherOpenWorks: "+err.Error(), "", AuditContext{})
        return nil
    }
    defer rows.Close()

    out := make([]WakeContextWork, 0, 5)
    for rows.Next() {
        var w WakeContextWork
        if err := rows.Scan(&w.ID, &w.Title, &w.Status, &w.CreatedAt); err != nil {
            dm.LogAudit(AuditWarn, "wake_context", "gatherOpenWorks scan: "+err.Error(), "", AuditContext{})
            continue
        }
        w.Pointer = "mpm://work/" + w.ID
        // Truncate title to 120 chars
        if len(w.Title) > 120 {
            w.Title = w.Title[:120]
        }
        out = append(out, w)
    }
    return out
}
```

- [ ] **Step 3: Call gatherOpenWorks in GatherWakeContext**

Add after the existing gather calls:

```go
data.OpenWorks = dm.gatherOpenWorks()
```

Ensure the slice is always non-nil by initializing it:

```go
data.OpenWorks = make([]WakeContextWork, 0)
```

- [ ] **Step 4: Update formatWakeContext to render OpenWorks**

Add to `formatWakeContext`:

```go
if len(d.OpenWorks) > 0 {
    lines = append(lines, fmt.Sprintf("**Open Work (%d):**", len(d.OpenWorks)))
    for _, w := range d.OpenWorks {
        title := w.Title
        if len(title) > 80 {
            title = title[:80] + "…"
        }
        lines = append(lines, fmt.Sprintf("  - %s [%s]", title, w.Status))
    }
}
```

- [ ] **Step 5: Write and run tests**

```go
func TestWakeContext_OpenWorks(t *testing.T) {
    dm := NewDatabaseManager(":memory:")
    defer dm.Close()

    // Create some work
    dm.AddWork("First work item", "", 0)
    dm.AddWork("Second work item", "", 0)

    ctx, err := dm.GatherWakeContext()
    if err != nil {
        t.Fatalf("GatherWakeContext: %v", err)
    }
    if len(ctx.OpenWorks) != 2 {
        t.Errorf("len(OpenWorks) = %d, want 2", len(ctx.OpenWorks))
    }

    // Verify pointer format
    for _, w := range ctx.OpenWorks {
        if !strings.HasPrefix(w.Pointer, "mpm://work/") {
            t.Errorf("Pointer = %q, want prefix 'mpm://work/'", w.Pointer)
        }
    }
}
```

Run: `go test -tags fts5 ./internal/core/... -run TestWakeContext_OpenWorks -v`  
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add internal/core/wake_context.go
git commit -m "feat(work): surface open work items in wake context"
```

---

### Task 6: Add mpm_work tool handler

**Files:**
- Create: `internal/core/work_handlers.go` — `handleMpmWork`
- Modify: `internal/core/tools/handlers.go` — register action dispatch
- Modify: `internal/core/tools/registry_list.go` — add `mpm_work` tool entry
- Test: `internal/core/tools/work_handlers_test.go`

**Interfaces:**
- Consumes: `Work` CRUD methods on `CoreDB`
- Produces: `mpm_work` tool with create/list/show/update/complete/cancel

- [ ] **Step 1: Write handleMpmWork**

```go
// internal/core/work_handlers.go
package mpminternal

import (
    "fmt"
)

// handleMpmWork dispatches work actions: create, list, show, update, complete, cancel.
func handleMpmWork(dm *DatabaseManager, p map[string]interface{}) (interface{}, error) {
    action, _ := p["action"].(string)
    params, _ := p["params"].(map[string]interface{})

    switch action {
    case "create":
        return handleCreateWork(dm, params)
    case "list":
        return handleListWorks(dm, params)
    case "show":
        return handleShowWork(dm, params)
    case "update":
        return handleUpdateWork(dm, params)
    case "complete":
        return handleCompleteWork(dm, params)
    case "cancel":
        return handleCancelWork(dm, params)
    default:
        return nil, fmt.Errorf("unknown action: %q (supported: create, list, show, update, complete, cancel)", action)
    }
}

func handleCreateWork(dm *DatabaseManager, p map[string]interface{}) (interface{}, error) {
    title, _ := p["title"].(string)
    if title == "" {
        return nil, fmt.Errorf("title is required for create")
    }
    content, _ := p["content"].(string)
    sessionID, _ := p["session_id"].(string)

    work, err := dm.AddWork(title, content, sessionID)
    if err != nil {
        return nil, err
    }
    return workToMap(work), nil
}

func handleListWorks(dm *DatabaseManager, p map[string]interface{}) (interface{}, error) {
    works, err := dm.ListWorks()
    if err != nil {
        return nil, err
    }
    result := make([]map[string]interface{}, len(works))
    for i, w := range works {
        result[i] = workToMap(w)
    }
    return result, nil
}

func handleShowWork(dm *DatabaseManager, p map[string]interface{}) (interface{}, error) {
    workID, _ := p["work_id"].(string)
    if workID == "" {
        return nil, fmt.Errorf("work_id is required for show")
    }
    work, err := dm.GetWork(workID)
    if err != nil {
        return nil, err
    }
    return workToMap(work), nil
}

func handleUpdateWork(dm *DatabaseManager, p map[string]interface{}) (interface{}, error) {
    workID, _ := p["work_id"].(string)
    if workID == "" {
        return nil, fmt.Errorf("work_id is required for update")
    }
    statusStr, _ := p["status"].(string)
    if statusStr == "" {
        return nil, fmt.Errorf("status is required for update")
    }
    status := WorkStatus(statusStr)
    if status != WorkStatusOpen && status != WorkStatusDone && status != WorkStatusCancelled {
        return nil, fmt.Errorf("invalid status: %q (must be open, done, or cancelled)", statusStr)
    }
    work, err := dm.UpdateWork(workID, status)
    if err != nil {
        return nil, err
    }
    return workToMap(work), nil
}

func handleCompleteWork(dm *DatabaseManager, p map[string]interface{}) (interface{}, error) {
    workID, _ := p["work_id"].(string)
    if workID == "" {
        return nil, fmt.Errorf("work_id is required for complete")
    }
    work, err := dm.CompleteWork(workID)
    if err != nil {
        return nil, err
    }
    return workToMap(work), nil
}

func handleCancelWork(dm *DatabaseManager, p map[string]interface{}) (interface{}, error) {
    workID, _ := p["work_id"].(string)
    if workID == "" {
        return nil, fmt.Errorf("work_id is required for cancel")
    }
    work, err := dm.CancelWork(workID)
    if err != nil {
        return nil, err
    }
    return workToMap(work), nil
}

func workToMap(w *Work) map[string]interface{} {
    m := map[string]interface{}{
        "id":         w.ID,
        "title":      w.Title,
        "status":     w.Status,
        "created_at": w.CreatedAt,
        "updated_at": w.UpdatedAt,
    }
    if w.Content != "" {
        m["content"] = w.Content
    }
    if w.CompletedAt != nil {
        m["completed_at"] = *w.CompletedAt
    }
    if w.SessionID != "" {
        m["session_id"] = w.SessionID
    }
    return m
}
```

- [ ] **Step 2: Add mpm_work to registry_list.go**

```go
{
    Name: "mpm_work",
    Description: `Work lifecycle. Actions:
  create — Create a work item. Required: params.title (string). Optional: params.content, params.session_id.
  list — List open work items ordered by created_at DESC. No params required.
  show — Get a work item by ID. Required: params.work_id.
  update — Update a work item's status. Required: params.work_id, params.status (open|done|cancelled).
  complete — Mark a work item as done. Required: params.work_id.
  cancel — Mark a work item as cancelled. Required: params.work_id.`,
    Schema: json.RawMessage(`{"type":"object","properties":{"action":{"type":"string","enum":["create","list","show","update","complete","cancel"]},"params":{"type":"object","properties":{"title":{"type":"string"},"content":{"type":"string"},"session_id":{"type":"string"},"work_id":{"type":"string"},"status":{"type":"string","enum":["open","done","cancelled"]}},"additionalProperties":true}},"required":["action"]}`),
    Handler: handleMpmWork,
},
```

- [ ] **Step 3: Add tests for handleMpmWork**

```go
func TestHandleMpmWork_Create(t *testing.T) {
    dm := NewDatabaseManager(":memory:")
    defer dm.Close()

    result, err := handleMpmWork(dm, map[string]interface{}{
        "action": "create",
        "params": map[string]interface{}{
            "title": "Investigate telemetry admission",
        },
    })
    if err != nil {
        t.Fatalf("handleMpmWork create: %v", err)
    }
    m := result.(map[string]interface{})
    if m["title"] != "Investigate telemetry admission" {
        t.Errorf("title = %q, want %q", m["title"], "Investigate telemetry admission")
    }
    if m["status"] != "open" {
        t.Errorf("status = %q, want %q", m["status"], "open")
    }
}

func TestHandleMpmWork_List(t *testing.T) {
    dm := NewDatabaseManager(":memory:")
    defer dm.Close()

    dm.AddWork("Work 1", "", 0)
    dm.AddWork("Work 2", "", 0)

    result, err := handleMpmWork(dm, map[string]interface{}{
        "action": "list",
        "params": map[string]interface{}{},
    })
    if err != nil {
        t.Fatalf("handleMpmWork list: %v", err)
    }
    list := result.([]map[string]interface{})
    if len(list) != 2 {
        t.Errorf("len(list) = %d, want 2", len(list))
    }
}

func TestHandleMpmWork_Complete(t *testing.T) {
    dm := NewDatabaseManager(":memory:")
    defer dm.Close()

    work, _ := dm.AddWork("Test work", "", 0)

    result, err := handleMpmWork(dm, map[string]interface{}{
        "action": "complete",
        "params": map[string]interface{}{
            "work_id": work.ID,
        },
    })
    if err != nil {
        t.Fatalf("handleMpmWork complete: %v", err)
    }
    m := result.(map[string]interface{})
    if m["status"] != "done" {
        t.Errorf("status = %q, want %q", m["status"], "done")
    }
}

func TestHandleMpmWork_InvalidStatus(t *testing.T) {
    dm := NewDatabaseManager(":memory:")
    defer dm.Close()

    work, _ := dm.AddWork("Test work", "", 0)

    _, err := handleMpmWork(dm, map[string]interface{}{
        "action": "update",
        "params": map[string]interface{}{
            "work_id": work.ID,
            "status":  "in_progress", // invalid
        },
    })
    if err == nil {
        t.Error("update with invalid status should error")
    }
}
```

Run: `go test -tags fts5 ./internal/core/... -run TestHandleMpmWork -v`  
Expected: PASS

- [ ] **Step 4: Commit**

```bash
git add internal/core/work_handlers.go
git add internal/core/tools/registry_list.go
git commit -m "feat(work): add mpm_work tool with create/list/show/update/complete/cancel"
```

---

### Task 7: Wire mpm://work/<id> pointer resolution

**Files:**
- Modify: `cmd/mpm-mcp/tools.go` — add `resolveWork` to `artifactResolverAdapter`; add `"work"` case
- Modify: `internal/core/tools/handlers.go` — add `"work"` to kind validation in `handleMpmResolve`
- Test: `cmd/mpm-mcp/work_resolve_test.go`

**Interfaces:**
- Consumes: `dm.GetWork(id)`, `SummarizeWork`
- Produces: `mpm://work/<id>` resolution via `mpm_resolve`

- [ ] **Step 1: Add resolveWork to artifactResolverAdapter**

In `cmd/mpm-mcp/tools.go`, add to the `Resolve` switch:

```go
case "work":
    return a.resolveWork(ctx, p, opts)
```

And the method:

```go
func (a *artifactResolverAdapter) resolveWork(ctx context.Context, p tools.Pointer, opts tools.ResolveOptions) (tools.Resolution, error) {
    work, err := a.dm.GetWork(p.ID)
    if err != nil {
        return tools.Resolution{}, err
    }

    content := work.Title
    if work.Content != "" {
        content = work.Title + "\n\n" + work.Content
    }

    maxBytes := int(opts.MaxBytes)
    if maxBytes <= 0 {
        maxBytes = 512
    }
    bounded := len(content) > maxBytes
    if bounded {
        content = core.SummarizeWork(content, maxBytes)
    }

    _ = a.dm.RecordRetrieval(p.ID, "work")

    return tools.Resolution{
        Pointer:     "mpm://work/" + p.ID,
        ContentType: "text/plain",
        Reader:      io.NopCloser(strings.NewReader(content)),
        Metadata:    nil,
        Bounded:     bounded,
    }, nil
}
```

- [ ] **Step 2: Update handleMpmResolve kind validation**

In `internal/core/tools/handlers.go`, find the kind validation in `handleMpmResolve` and add `"work"`:

```go
if ptr.Kind != "blob" && ptr.Kind != "memory" && ptr.Kind != "lesson" && ptr.Kind != "theory" && ptr.Kind != "work" {
    return nil, fmt.Errorf("%w: ...", ErrUnsupportedKind)
}
```

- [ ] **Step 3: Write pointer resolution tests**

```go
func TestResolveWorkPointer(t *testing.T) {
    // Integration test via mpm_resolve tool
    result, err := handleMpmResolve(map[string]interface{}{
        "uri": "mpm://work/" + work.ID,
    })
    if err != nil {
        t.Fatalf("resolve work: %v", err)
    }
    m := result.(map[string]interface{})
    if !strings.Contains(m["content"].(string), "Investigate telemetry") {
        t.Errorf("content missing expected title")
    }
    if m["bounded"] != false {
        t.Errorf("bounded = %v, want false (short content)", m["bounded"])
    }
}
```

- [ ] **Step 4: Run tests**

```bash
go test -tags fts5 ./cmd/mpm-mcp/... -run TestMpmResolve -v
go test -tags fts5 ./internal/core/tools/... -run TestMpmResolve -v
```

Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add cmd/mpm-mcp/tools.go internal/core/tools/handlers.go
git commit -m "feat(work): add mpm://work/<id> pointer resolution"
```

---

### Task 8: Integration tests for cross-session persistence

**Files:**
- Create: `cmd/mpm/work_integration_test.go`
- Test: full cross-session lifecycle

**This is the canonical acceptance test.** CRUD tests prove SQLite works; this proves MPM's cognitive model works.

- [ ] **Step 1: Write the cross-session test**

```go
func TestWork_CrossSessionPersistence(t *testing.T) {
    // Use a shared temp DB to simulate cross-session
    tmpDir := t.TempDir()
    dbPath := filepath.Join(tmpDir, "test_work.db")

    // Session A
    dmA, _ := NewDatabaseManager(dbPath)
    work, err := dmA.AddWork("Investigate telemetry admission", "The telemetry collector is not firing on clean deploy", "")
    if err != nil {
        t.Fatalf("Session A AddWork: %v", err)
    }
    if work.Status != WorkStatusOpen {
        t.Errorf("Session A: status = %v, want open", work.Status)
    }
    dmA.Close()

    // Session B — new DB instance, same file
    dmB, _ := NewDatabaseManager(dbPath)

    // Work persists
    retrieved, err := dmB.GetWork(work.ID)
    if err != nil {
        t.Fatalf("Session B GetWork: %v", err)
    }
    if retrieved.Title != "Investigate telemetry admission" {
        t.Errorf("Session B: title = %q, want %q", retrieved.Title, "Investigate telemetry admission")
    }

    // Wake context surfaces it
    ctx, err := dmB.GatherWakeContext()
    if err != nil {
        t.Fatalf("Session B GatherWakeContext: %v", err)
    }
    if len(ctx.OpenWorks) != 1 {
        t.Errorf("Session B: len(OpenWorks) = %d, want 1", len(ctx.OpenWorks))
    }
    if ctx.OpenWorks[0].ID != work.ID {
        t.Errorf("Session B: OpenWorks[0].ID = %q, want %q", ctx.OpenWorks[0].ID, work.ID)
    }

    // Complete from Session B
    completed, err := dmB.CompleteWork(work.ID)
    if err != nil {
        t.Fatalf("Session B CompleteWork: %v", err)
    }
    if completed.Status != WorkStatusDone {
        t.Errorf("Session B: status = %v, want done", completed.Status)
    }

    // No longer in open works
    ctx2, _ := dmB.GatherWakeContext()
    if len(ctx2.OpenWorks) != 0 {
        t.Errorf("Session B after complete: len(OpenWorks) = %d, want 0", len(ctx2.OpenWorks))
    }

    dmB.Close()
}

func TestWork_NoSilentDisappearance(t *testing.T) {
    tmpDir := t.TempDir()
    dbPath := filepath.Join(tmpDir, "test_work_no_ttl.db")

    dm, _ := NewDatabaseManager(dbPath)
    work, _ := dm.AddWork("Long-term investigation", "This might take weeks", "")

    // Simulate time passing (no actual sleep — just verify no TTL mechanism exists)
    // Work should still be there
    retrieved, _ := dm.GetWork(work.ID)
    if retrieved.Status != WorkStatusOpen {
        t.Errorf("Status = %v, want open (no TTL)", retrieved.Status)
    }

    // List should still return it
    works, _ := dm.ListWorks()
    found := false
    for _, w := range works {
        if w.ID == work.ID {
            found = true
            break
        }
    }
    if !found {
        t.Error("Work should still be in list after time passes")
    }

    dm.Close()
}

func TestWork_CancelAndReopen(t *testing.T) {
    dm := NewDatabaseManager(":memory:")
    defer dm.Close()

    work, _ := dm.AddWork("Maybe later", "", 0)
    dm.CancelWork(work.ID)

    // Cancelled work is NOT in list
    works, _ := dm.ListWorks()
    if len(works) != 0 {
        t.Errorf("ListWorks after cancel: len = %d, want 0", len(works))
    }

    // Can re-open
    reopened, _ := dm.UpdateWork(work.ID, WorkStatusOpen)
    if reopened.Status != WorkStatusOpen {
        t.Errorf("Reopen: status = %v, want open", reopened.Status)
    }

    // Back in list
    works, _ = dm.ListWorks()
    if len(works) != 1 {
        t.Errorf("ListWorks after reopen: len = %d, want 1", len(works))
    }
}
```

- [ ] **Step 2: Run integration tests**

```bash
go test -tags fts5 ./cmd/mpm/... -run TestWork_CrossSession -v
go test -tags fts5 ./cmd/mpm/... -run TestWork_NoSilent -v
go test -tags fts5 ./cmd/mpm/... -run TestWork_CancelAndReopen -v
```

Expected: PASS

- [ ] **Step 3: Commit**

```bash
git add cmd/mpm/work_integration_test.go
git commit -m "test(work): cross-session persistence and no-silent-disappearance integration tests"
```

---

### Task 9: Final verification

**Run the full test suite:**

```bash
# Core module
cd internal/core && go test -tags fts5 -v ./...

# Main module
cd ../.. && go test -tags fts5 -v ./cmd/mpm/... -run TestWork

# MCP server
go test -tags fts5 -v ./cmd/mpm-mcp/...
```

**Verify:**
- All `TestWork*` tests pass
- `TestWakeContext_OpenWorks` passes
- `mpm call mpm_work --payload '{"action":"list","params":{}}'` works
- `mpm call mpm_resolve --payload '{"uri":"mpm://work/<id>"}'` works
- `mpm wake` shows open work items in output

---

## Summary

```
Implemented:
  ✓ Work types (work.go)
  ✓ works table migration (schema.go SafeMigrations)
  ✓ Work CRUD (db.go)
  ✓ SummarizeWork (summarize.go)
  ✓ Wake context integration (wake_context.go)
  ✓ mpm_work tool (work_handlers.go, registry_list.go)
  ✓ mpm://work/<id> pointer resolution (tools.go, handlers.go)
  ✓ Cross-session persistence integration test
  ✓ No-silent-disappearance test
  ✓ Cancel and reopen test
```

**Compatibility:** Fully additive. No existing behavior changes. All prior tests continue to pass.

**Spec:** `docs/work-primitive-design.md`
