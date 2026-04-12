# mini-bot Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Rename mpm-agent to mini-bot, add IDENTITY.md core persona system, create mini-bot.db with anchoring/self-improvement, expand tools, and connect to shared MCP server.

**Architecture:** mini-bot is a self-improving agent with stable core identity (IDENTITY.md) separate from MPM personas. It has its own SQLite database (mini-bot.db) for memories, sessions, lessons, anchors, and tool registry. MCP access goes through a shared mini-bot-mcp server that also serves Claude Code. All files live in the binary directory.

**Tech Stack:** Go, SQLite3 with FTS5, telego (Telegram), Unix socket JSON-RPC

---

## File Structure

```
bin/                          (binary directory — ~/mpm/bin/ or similar)
├── mini-bot                  (NEW: CLI entry point, replaces mpm-agent)
├── mini-bot-telegram         (NEW: Telegram bridge, replaces mpm-agent-telegram)
├── mini-bot-mcp             (NEW: MCP server, replaces mpm-agent-mcp)
├── IDENTITY.md               (NEW: core identity — loaded at startup)
├── mini-bot-config.json      (NEW: all config — replaces mpm_config.json)
├── mini-bot.db               (NEW: SQLite with anchoring/self-improve schema)
└── mini-bot.db               (NEW: created on first startup)

mpm-agent/                    (RENAME: mpm-agent → keep for backward compat during transition)
├── cmd/
│   ├── mini-bot/             (NEW: mini-bot CLI commands)
│   │   ├── main.go           (NEW: entry point with PID logging)
│   │   └── repl.go          (NEW: REPL loop)
│   ├── telegram/             (MOVE from ../cmd/telegram → renamed)
│   │   ├── main.go          (RENAME: mpm-agent-telegram → mini-bot-telegram)
│   │   ├── config.go        (UPDATE: loads mini-bot-config.json)
│   │   ├── handler.go       (UPDATE: session manager → mini-bot.db)
│   │   └── session.go       (UPDATE: session persistence → mini-bot.db)
│   └── mcp/
│       └── main.go          (RENAME: mpm-agent-mcp → mini-bot-mcp, token auth, shared)
├── core/
│   ├── agent.go             (UPDATE: PID logging, IDENTITY.md loading, anchor support)
│   ├── db.go                (UPDATE: mini-bot.db path resolution)
│   ├── identity.go          (NEW: IDENTITY.md loading + persona layering)
│   ├── selfimprove.go       (NEW: anchoring, lessons, identity patch, tool registry)
│   ├── config.go            (UPDATE: mini-bot-config.json loading)
│   └── tools.go             (UPDATE: file-aware, task_execute, web_synthesize)
└── docs/superpowers/plans/  (this plan)
```

---

## Task 1: Create mini-bot Core Package Structure

**Files:**
- Create: `mpm-agent/core/identity.go`
- Create: `mpm-agent/core/selfimprove.go`
- Create: `mpm-agent/core/config.go` (update existing)

- [ ] **Step 1: Write test for identity loading**

```go
// mpm-agent/core/identity_test.go
package core

import (
    "os"
    "path/filepath"
    "testing"
)

func TestLoadIdentity(t *testing.T) {
    // Create temp dir with IDENTITY.md
    dir := t.TempDir()
    identity := `# TestBot v1.0
Type: test assistant
Core traits: precise, analytical`
    if err := os.WriteFile(filepath.Join(dir, "IDENTITY.md"), []byte(identity), 0644); err != nil {
        t.Fatal(err)
    }

    // Mock executable path to return temp dir
    oldExec := os.Args[0]
    defer func() { os.Args[0] = oldExec }()

    content := readIdentityFileFrom(dir)
    if content == "" {
        t.Error("expected identity content, got empty")
    }
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./core -run TestLoadIdentity -v`
Expected: FAIL — function doesn't exist

- [ ] **Step 3: Write identity loading implementation**

```go
// mpm-agent/core/identity.go
package core

import (
    "os"
    "path/filepath"
    "strings"
)

// Identity represents the bot's stable core identity.
type Identity struct {
    Name    string
    Version string
    Type    string
    Domain  string
    Traits  string
    Boundaries string
}

// LoadIdentity reads IDENTITY.md from the binary directory.
// Returns empty Identity if file doesn't exist or can't be read.
func LoadIdentity(binaryDir string) (*Identity, error) {
    path := filepath.Join(binaryDir, "IDENTITY.md")
    data, err := os.ReadFile(path)
    if err != nil {
        return nil, err
    }

    content := string(data)
    id := &Identity{}

    lines := strings.Split(content, "\n")
    for _, line := range lines {
        line = strings.TrimSpace(line)
        if line == "" || strings.HasPrefix(line, "#") {
            continue
        }
        if strings.HasPrefix(line, "Type:") {
            id.Type = strings.TrimSpace(strings.TrimPrefix(line, "Type:"))
        } else if strings.HasPrefix(line, "Domain:") {
            id.Domain = strings.TrimSpace(strings.TrimPrefix(line, "Domain:"))
        } else if strings.HasPrefix(line, "Core traits:") {
            id.Traits = strings.TrimSpace(strings.TrimPrefix(line, "Core traits:"))
        } else if strings.HasPrefix(line, "Boundaries:") {
            id.Boundaries = strings.TrimSpace(strings.TrimPrefix(line, "Boundaries:"))
        }
    }

    // Extract name and version from first line if present
    // Format: # Name v1.0
    return id, nil
}

// GetBinaryDir returns the directory containing the running executable.
func GetBinaryDir() string {
    execPath, err := os.Executable()
    if err != nil {
        return ""
    }
    return filepath.Dir(execPath)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./core -run TestLoadIdentity -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add core/identity.go core/identity_test.go
git commit -m "feat: add IDENTITY.md loading system"
```

---

## Task 2: Create mini-bot Database Schema & Anchoring

**Files:**
- Create: `mpm-agent/core/db.go` (mini-bot.db path + schema init)
- Create: `mpm-agent/core/anchors.go`

- [ ] **Step 1: Write test for mini-bot.db schema init**

```go
// mpm-agent/core/db_test.go
package core

import (
    "os"
    "path/filepath"
    "testing"
)

func TestMiniBotDBInit(t *testing.T) {
    dir := t.TempDir()
    dbPath := filepath.Join(dir, "mini-bot.db")

    err := InitMiniBotDB(dbPath)
    if err != nil {
        t.Fatalf("InitMiniBotDB failed: %v", err)
    }

    // Verify tables exist
    db, err := OpenDBForPath(dbPath)
    if err != nil {
        t.Fatal(err)
    }
    defer db.Close()

    tables := []string{"memories", "sessions", "lessons", "anchors", "tools"}
    for _, table := range tables {
        row := db.QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name=?", table)
        var name string
        if err := row.Scan(&name); err != nil {
            t.Errorf("table %s not found: %v", table, err)
        }
    }
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./core -run TestMiniBotDBInit -v`
Expected: FAIL — InitMiniBotDB doesn't exist

- [ ] **Step 3: Write DB initialization with schema**

```go
// mpm-agent/core/db.go
package core

import (
    "database/sql"
    "fmt"
    "os"
    "path/filepath"
    "strings"

    _ "github.com/mattn/go-sqlite3"
)

var MiniBotDBPath string // Set at startup based on binary directory

// ResolveMiniBotDBPath resolves the mini-bot.db path from binary directory.
func ResolveMiniBotDBPath() string {
    if MiniBotDBPath != "" {
        return MiniBotDBPath
    }
    binaryDir := GetBinaryDir()
    if binaryDir == "" {
        return "mini-bot.db"
    }
    return filepath.Join(binaryDir, "mini-bot.db")
}

// InitMiniBotDB creates the mini-bot.db schema if it doesn't exist.
func InitMiniBotDB(dbPath string) error {
    // Ensure parent dir exists
    dir := filepath.Dir(dbPath)
    if dir != "" && dir != "." {
        if err := os.MkdirAll(dir, 0755); err != nil {
            return fmt.Errorf("ensure dir: %w", err)
        }
    }

    db, err := sql.Open("sqlite3", dbPath+"?_busy_timeout=5000")
    if err != nil {
        return err
    }
    defer db.Close()

    schema := `
    CREATE TABLE IF NOT EXISTS memories (
        id TEXT PRIMARY KEY,
        collection TEXT DEFAULT 'general',
        content TEXT NOT NULL,
        session_id TEXT,
        tags TEXT,
        metadata TEXT,
        created_at TEXT,
        deleted_at TEXT
    );

    CREATE VIRTUAL TABLE IF NOT EXISTS memories_fts USING fts5(content, tags, content=memories, content_rowid=rowid);
    CREATE TRIGGER IF NOT EXISTS memories_ai AFTER INSERT ON memories BEGIN INSERT INTO memories_fts(rowid, content, tags) VALUES (new.rowid, new.content, new.tags); END;
    CREATE TRIGGER IF NOT EXISTS memories_ad AFTER DELETE ON memories BEGIN INSERT INTO memories_fts(memories_fts, rowid, content, tags) VALUES (delete, old.rowid, old.content, old.tags); END;
    CREATE TRIGGER IF NOT EXISTS memories_au AFTER UPDATE ON memories BEGIN INSERT INTO memories_fts(memories_fts, rowid, content, tags) VALUES (delete, old.rowid, old.content, old.tags); INSERT INTO memories_fts(rowid, content, tags) VALUES (new.rowid, new.content, new.tags); END;

    CREATE TABLE IF NOT EXISTS sessions (
        id TEXT PRIMARY KEY,
        session_id TEXT,
        content TEXT,
        content_hash TEXT,
        created_at TEXT,
        source_path TEXT,
        metadata TEXT
    );

    CREATE TABLE IF NOT EXISTS lessons (
        id TEXT PRIMARY KEY,
        content TEXT NOT NULL,
        type TEXT DEFAULT 'insight',
        tags TEXT,
        created_at TEXT,
        reinforcement_count INTEGER DEFAULT 1
    );

    CREATE TABLE IF NOT EXISTS anchors (
        id TEXT PRIMARY KEY,
        content TEXT NOT NULL,
        context TEXT,
        weight INTEGER DEFAULT 1,
        session_id TEXT,
        created_at TEXT
    );

    CREATE TABLE IF NOT EXISTS tools (
        id TEXT PRIMARY KEY,
        name TEXT UNIQUE,
        description TEXT,
        definition TEXT,
        source TEXT DEFAULT 'self',
        created_at TEXT
    );
    `

    if _, err := db.Exec(schema); err != nil {
        return fmt.Errorf("create schema: %w", err)
    }
    return nil
}

// OpenDBForPath opens a SQLite db at a specific path.
func OpenDBForPath(dbPath string) (*sql.DB, error) {
    db, err := sql.Open("sqlite3", dbPath+"?_busy_timeout=5000")
    if err != nil {
        return nil, err
    }
    if err := db.Ping(); err != nil {
        db.Close()
        return nil, err
    }
    return db, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./core -run TestMiniBotDBInit -v`
Expected: PASS

- [ ] **Step 5: Write anchor insertion test**

```go
// mpm-agent/core/anchors_test.go
package core

func TestAnchorInsert(t *testing.T) {
    dir := t.TempDir()
    dbPath := filepath.Join(dir, "mini-bot.db")
    InitMiniBotDB(dbPath)

    db, err := OpenDBForPath(dbPath)
    if err != nil {
        t.Fatal(err)
    }
    defer db.Close()

    err = InsertAnchor(db, "test anchor", "session123", 3, "important context")
    if err != nil {
        t.Fatalf("InsertAnchor failed: %v", err)
    }

    anchors, err := GetRecentAnchors(db, 10)
    if err != nil {
        t.Fatal(err)
    }
    if len(anchors) == 0 {
        t.Error("expected at least one anchor")
    }
}
```

- [ ] **Step 6: Run anchor test to verify it fails**

Run: `go test ./core -run TestAnchorInsert -v`
Expected: FAIL — InsertAnchor doesn't exist

- [ ] **Step 7: Write anchor implementation**

```go
// mpm-agent/core/anchors.go
package core

import (
    "database/sql"
    "fmt"
    "strings"
    "time"
)

type Anchor struct {
    ID        string
    Content   string
    Context   string
    Weight    int
    SessionID string
    CreatedAt string
}

// InsertAnchor saves a high-priority anchor to mini-bot.db.
func InsertAnchor(db *sql.DB, content, context, sessionID string, weight int) error {
    id := generateID()
    now := time.Now().Format(time.RFC3339)
    _, err := db.Exec(`
        INSERT INTO anchors (id, content, context, weight, session_id, created_at)
        VALUES (?, ?, ?, ?, ?, ?)
    `, id, content, context, weight, sessionID, now)
    return err
}

// GetRecentAnchors returns the most recent anchors up to limit.
func GetRecentAnchors(db *sql.DB, limit int) ([]Anchor, error) {
    rows, err := db.Query(`
        SELECT id, content, context, weight, session_id, created_at
        FROM anchors ORDER BY created_at DESC LIMIT ?
    `, limit)
    if err != nil {
        return nil, err
    }
    defer rows.Close()

    var anchors []Anchor
    for rows.Next() {
        var a Anchor
        if err := rows.Scan(&a.ID, &a.Content, &a.Context, &a.Weight, &a.SessionID, &a.CreatedAt); err != nil {
            return nil, err
        }
        anchors = append(anchors, a)
    }
    return anchors, nil
}

// ShouldAnchor returns true if the interaction weight exceeds threshold.
func ShouldAnchor(weight int, threshold int) bool {
    return weight >= threshold
}

// FormatAnchorContext formats an anchor for use in system prompt context.
func FormatAnchorContext(a Anchor) string {
    return fmt.Sprintf("[anchor:%d] %s", a.Weight, a.Content)
}
```

- [ ] **Step 8: Run anchor test to verify it passes**

Run: `go test ./core -run TestAnchorInsert -v`
Expected: PASS

- [ ] **Step 9: Commit**

```bash
git add core/db.go core/db_test.go core/anchors.go core/anchors_test.go
git commit -m "feat: add mini-bot.db schema with anchoring support"
```

---

## Task 3: Create mini-bot-config.json & Config Loader

**Files:**
- Create: `mpm-agent/core/config.go` (new config structure)
- Create: `mpm-agent/mini-bot-config.json` (example config)

- [ ] **Step 1: Write test for config loading**

```go
// mpm-agent/core/config_test.go
package core

import (
    "encoding/json"
    "os"
    "path/filepath"
    "testing"
)

func TestLoadMiniBotConfig(t *testing.T) {
    dir := t.TempDir()
    configPath := filepath.Join(dir, "mini-bot-config.json")

    configJSON := `{
      "identity": {"name": "mini-bot", "version": "1.0"},
      "synth": {"model": "MiniMax-M2.7", "api_key": "test-key", "base_url": "https://api.test.com", "max_tokens": 2048, "timeout_seconds": 120},
      "self_improve": {"enabled": true, "anchor_threshold": 3, "lesson_complexity_threshold": 7}
    }`
    if err := os.WriteFile(configPath, []byte(configJSON), 0644); err != nil {
        t.Fatal(err)
    }

    cfg, err := LoadMiniBotConfig(configPath)
    if err != nil {
        t.Fatalf("LoadMiniBotConfig failed: %v", err)
    }
    if cfg.Identity.Name != "mini-bot" {
        t.Errorf("expected name 'mini-bot', got %q", cfg.Identity.Name)
    }
    if cfg.Synth.APIKey != "test-key" {
        t.Errorf("expected api_key 'test-key', got %q", cfg.Synth.APIKey)
    }
    if cfg.SelfImprove.AnchorThreshold != 3 {
        t.Errorf("expected anchor_threshold 3, got %d", cfg.SelfImprove.AnchorThreshold)
    }
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./core -run TestLoadMiniBotConfig -v`
Expected: FAIL — LoadMiniBotConfig doesn't exist

- [ ] **Step 3: Write config structure and loader**

```go
// mpm-agent/core/config.go
package core

import (
    "encoding/json"
    "fmt"
    "os"
    "path/filepath"
)

// MiniBotConfig is the top-level config for mini-bot.
type MiniBotConfig struct {
    Identity   IdentityConfig   `json:"identity"`
    Synth      SynthConfig      `json:"synth"`
    Telegram   TelegramConfig   `json:"telegram"`
    MCP        MCPConfig        `json:"mcp"`
    SelfImprove SelfImproveConfig `json:"self_improve"`
    Paths      PathsConfig       `json:"paths"`
}

type IdentityConfig struct {
    Name    string `json:"name"`
    Version string `json:"version"`
}

type SynthConfig struct {
    Model        string `json:"model"`
    APIKey       string `json:"api_key"`
    BaseURL      string `json:"base_url"`
    MaxTokens    int    `json:"max_tokens"`
    TimeoutSecs  int    `json:"timeout_seconds"`
}

type TelegramConfig struct {
    BotToken     string  `json:"bot_token"`
    Polling      bool    `json:"polling"`
    AllowedUsers []int64 `json:"allowed_users"`
}

type MCPConfig struct {
    Token      string `json:"token"`
    SocketPath string `json:"socket_path"`
}

type SelfImproveConfig struct {
    Enabled                      bool `json:"enabled"`
    AnchorThreshold              int  `json:"anchor_threshold"`
    LessonComplexityThreshold    int  `json:"lesson_complexity_threshold"`
    IdentityPatchApprovalRequired bool `json:"identity_patch_approval_required"`
}

type PathsConfig struct {
    DB         string `json:"db"`
    Identity   string `json:"identity"`
    MCPSocket  string `json:"mcp_socket"`
}

// DefaultMiniBotConfig returns the default config values.
func DefaultMiniBotConfig() *MiniBotConfig {
    return &MiniBotConfig{
        Identity: IdentityConfig{Name: "mini-bot", Version: "1.0"},
        Synth: SynthConfig{
            Model:       "MiniMax-M2.7",
            BaseURL:     "https://api.minimax.io/anthropic",
            MaxTokens:   4096,
            TimeoutSecs: 300,
        },
        SelfImprove: SelfImproveConfig{
            Enabled:                      true,
            AnchorThreshold:             3,
            LessonComplexityThreshold:   7,
            IdentityPatchApprovalRequired: true,
        },
        Paths: PathsConfig{
            DB:        "mini-bot.db",
            Identity:  "IDENTITY.md",
            MCPSocket: "mini-bot-mcp.sock",
        },
    }
}

// LoadMiniBotConfig reads and parses mini-bot-config.json.
func LoadMiniBotConfig(configPath string) (*MiniBotConfig, error) {
    data, err := os.ReadFile(configPath)
    if err != nil {
        return nil, fmt.Errorf("read config at %s: %w", configPath, err)
    }
    var cfg MiniBotConfig
    if err := json.Unmarshal(data, &cfg); err != nil {
        return nil, fmt.Errorf("parse config: %w", err)
    }

    // Apply defaults
    defaults := DefaultMiniBotConfig()
    if cfg.Synth.Model == "" {
        cfg.Synth.Model = defaults.Synth.Model
    }
    if cfg.Synth.BaseURL == "" {
        cfg.Synth.BaseURL = defaults.Synth.BaseURL
    }
    if cfg.Synth.MaxTokens == 0 {
        cfg.Synth.MaxTokens = defaults.Synth.MaxTokens
    }
    if cfg.Synth.TimeoutSecs == 0 {
        cfg.Synth.TimeoutSecs = defaults.Synth.TimeoutSecs
    }
    if cfg.SelfImprove.AnchorThreshold == 0 {
        cfg.SelfImprove.AnchorThreshold = defaults.SelfImprove.AnchorThreshold
    }
    if cfg.SelfImprove.LessonComplexityThreshold == 0 {
        cfg.SelfImprove.LessonComplexityThreshold = defaults.SelfImprove.LessonComplexityThreshold
    }

    // Environment variable overrides
    if cfg.Synth.APIKey == "" {
        if envKey := os.Getenv("MINIMAX_API_KEY"); envKey != "" {
            cfg.Synth.APIKey = envKey
        }
    }
    if cfg.MCP.Token == "" {
        if envToken := os.Getenv("MPM_API_TOKEN"); envToken != "" {
            cfg.MCP.Token = envToken
        }
    }

    return &cfg, nil
}

// GetConfigPath returns the path to mini-bot-config.json.
// Checks binary directory first, then CWD.
func GetConfigPath() string {
    binaryDir := GetBinaryDir()
    if binaryDir != "" {
        path := filepath.Join(binaryDir, "mini-bot-config.json")
        if _, err := os.Stat(path); err == nil {
            return path
        }
    }
    return "mini-bot-config.json"
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./core -run TestLoadMiniBotConfig -v`
Expected: PASS

- [ ] **Step 5: Write example config file**

```json
// mpm-agent/mini-bot-config.json.example
{
  "identity": {
    "name": "mini-bot",
    "version": "1.0"
  },
  "synth": {
    "model": "MiniMax-M2.7",
    "api_key": "",
    "base_url": "https://api.minimax.io/anthropic",
    "max_tokens": 4096,
    "timeout_seconds": 300
  },
  "telegram": {
    "bot_token": "",
    "polling": true,
    "allowed_users": []
  },
  "mcp": {
    "token": "",
    "socket_path": "mini-bot-mcp.sock"
  },
  "self_improve": {
    "enabled": true,
    "anchor_threshold": 3,
    "lesson_complexity_threshold": 7,
    "identity_patch_approval_required": true
  },
  "paths": {
    "db": "mini-bot.db",
    "identity": "IDENTITY.md",
    "mcp_socket": "mini-bot-mcp.sock"
  }
}
```

- [ ] **Step 6: Commit**

```bash
git add core/config.go core/config_test.go mini-bot-config.json.example
git commit -m "feat: add mini-bot-config.json with self-improve settings"
```

---

## Task 4: Self-Improvement System (Lessons, Identity Patching, Tool Registry)

**Files:**
- Create: `mpm-agent/core/selfimprove.go` (new file)
- Create: `mpm-agent/core/identity_fork.go` (identity branching)

### 4a: Core Self-Improvement

- [ ] **Step 1: Write test for lesson extraction**

```go
// mpm-agent/core/selfimprove_test.go
package core

import (
    "os"
    "path/filepath"
    "testing"
)

func TestExtractLesson(t *testing.T) {
    dir := t.TempDir()
    dbPath := filepath.Join(dir, "mini-bot.db")
    InitMiniBotDB(dbPath)

    db, err := OpenDBForPath(dbPath)
    if err != nil {
        t.Fatal(err)
    }
    defer db.Close()

    err = ExtractLesson(db, "Always check file encoding before processing", "insight", "encoding,safety")
    if err != nil {
        t.Fatalf("ExtractLesson failed: %v", err)
    }

    lessons, err := GetRecentLessons(db, 10)
    if err != nil {
        t.Fatal(err)
    }
    if len(lessons) == 0 {
        t.Error("expected at least one lesson")
    }
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./core -run TestExtractLesson -v`
Expected: FAIL — ExtractLesson doesn't exist

- [ ] **Step 3: Write self-improvement implementation**

```go
// mpm-agent/core/selfimprove.go
package core

import (
    "database/sql"
    "fmt"
    "os"
    "path/filepath"
    "time"
)

// Lesson represents a learned lesson stored in mini-bot.db.
type Lesson struct {
    ID                 string
    Content            string
    Type               string
    Tags               string
    CreatedAt          string
    ReinforcementCount int
}

// ExtractLesson saves a new lesson to mini-bot.db.
func ExtractLesson(db *sql.DB, content, lessonType, tags string) error {
    id := generateID()
    now := time.Now().Format(time.RFC3339)
    _, err := db.Exec(`
        INSERT INTO lessons (id, content, type, tags, created_at, reinforcement_count)
        VALUES (?, ?, ?, ?, ?, 1)
    `, id, content, lessonType, tags, now)
    return err
}

// GetRecentLessons retrieves lessons ordered by reinforcement_count.
func GetRecentLessons(db *sql.DB, limit int) ([]Lesson, error) {
    rows, err := db.Query(`
        SELECT id, content, type, tags, created_at, reinforcement_count
        FROM lessons ORDER BY reinforcement_count DESC LIMIT ?
    `, limit)
    if err != nil {
        return nil, err
    }
    defer rows.Close()

    var lessons []Lesson
    for rows.Next() {
        var l Lesson
        if err := rows.Scan(&l.ID, &l.Content, &l.Type, &l.Tags, &l.CreatedAt, &l.ReinforcementCount); err != nil {
            return nil, err
        }
        lessons = append(lessons, l)
    }
    return lessons, nil
}

// GetLessonsByTag retrieves lessons matching a tag.
func GetLessonsByTag(db *sql.DB, tag string, limit int) ([]Lesson, error) {
    rows, err := db.Query(`
        SELECT id, content, type, tags, created_at, reinforcement_count
        FROM lessons WHERE tags LIKE ? ORDER BY reinforcement_count DESC LIMIT ?
    `, "%"+tag+"%", limit)
    if err != nil {
        return nil, err
    }
    defer rows.Close()

    var lessons []Lesson
    for rows.Next() {
        var l Lesson
        if err := rows.Scan(&l.ID, &l.Content, &l.Type, &l.Tags, &l.CreatedAt, &l.ReinforcementCount); err != nil {
            return nil, err
        }
        lessons = append(lessons, l)
    }
    return lessons, nil
}

// ProposeIdentityPatch writes a proposed change to IDENTITY_PATCH.md.
func ProposeIdentityPatch(binaryDir, patchContent string) error {
    path := filepath.Join(binaryDir, "IDENTITY_PATCH.md")
    return os.WriteFile(path, []byte(patchContent), 0644)
}

// ApplyIdentityPatch merges IDENTITY_PATCH.md into IDENTITY.md.
func ApplyIdentityPatch(binaryDir string) error {
    identityPath := filepath.Join(binaryDir, "IDENTITY.md")
    patchPath := filepath.Join(binaryDir, "IDENTITY_PATCH.md")

    identityData, err := os.ReadFile(identityPath)
    if err != nil {
        return fmt.Errorf("read identity: %w", err)
    }

    patchData, err := os.ReadFile(patchPath)
    if err != nil {
        return fmt.Errorf("read patch: %w", err)
    }

    merged := string(identityData) + "\n\n---\n\n" + string(patchData)
    if err := os.WriteFile(identityPath, []byte(merged), 0644); err != nil {
        return fmt.Errorf("write merged identity: %w", err)
    }

    os.Remove(patchPath)
    return nil
}

// RegisterSelfTool adds a self-generated tool to the tools table.
func RegisterSelfTool(db *sql.DB, name, description, definition string) error {
    id := generateID()
    now := time.Now().Format(time.RFC3339)
    _, err := db.Exec(`
        INSERT OR REPLACE INTO tools (id, name, description, definition, source, created_at)
        VALUES (?, ?, ?, ?, 'self', ?)
    `, id, name, description, definition, now)
    return err
}

// GetSelfTools retrieves all self-generated tools.
func GetSelfTools(db *sql.DB) ([]map[string]interface{}, error) {
    rows, err := db.Query(`
        SELECT name, description, definition FROM tools WHERE source = 'self'
    `)
    if err != nil {
        return nil, err
    }
    defer rows.Close()

    var tools []map[string]interface{}
    for rows.Next() {
        var name, description, definition string
        if err := rows.Scan(&name, &description, &definition); err != nil {
            continue
        }
        tools = append(tools, map[string]interface{}{
            "name":        name,
            "description": description,
            "definition":  definition,
        })
    }
    return tools, nil
}
```

- [ ] **Step 4: Run lesson test to verify it passes**

Run: `go test ./core -run TestExtractLesson -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add core/selfimprove.go core/selfimprove_test.go
git commit -m "feat: add self-improvement system (lessons, identity patching, tool registry)"
```

### 4b: Identity Forking

- [ ] **Step 1: Write test for identity branching**

```go
// mpm-agent/core/identity_fork_test.go
package core

import (
    "os"
    "path/filepath"
    "testing"
)

func TestForkIdentity(t *testing.T) {
    dir := t.TempDir()

    // Create main identity
    mainIdentity := `# MainBot v1.0
Type: assistant
Core traits: careful`
    if err := os.WriteFile(filepath.Join(dir, "IDENTITY.md"), []byte(mainIdentity), 0644); err != nil {
        t.Fatal(err)
    }
    os.MkdirAll(filepath.Join(dir, "IDENTITIES"), 0755)

    // Fork the identity
    branchName := "experiment-v1"
    err := ForkIdentity(dir, branchName, "")
    if err != nil {
        t.Fatalf("ForkIdentity failed: %v", err)
    }

    // Verify branch file exists
    branchPath := filepath.Join(dir, "IDENTITIES", "experiment-v1.md")
    if _, err := os.Stat(branchPath); os.IsNotExist(err) {
        t.Errorf("expected branch file at %s", branchPath)
    }

    // Verify branches.json was updated
    metaPath := filepath.Join(dir, "IDENTITIES", "branches.json")
    if _, err := os.Stat(metaPath); os.IsNotExist(err) {
        t.Errorf("expected branches.json at %s", metaPath)
    }
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./core -run TestForkIdentity -v`
Expected: FAIL — ForkIdentity doesn't exist

- [ ] **Step 3: Write identity forking implementation**

```go
// mpm-agent/core/identity_fork.go
package core

import (
    "encoding/json"
    "fmt"
    "os"
    "path/filepath"
    "time"
)

// IdentityBranch represents a branch in the identity version history.
type IdentityBranch struct {
    Name      string `json:"name"`
    Parent    string `json:"parent"`
    CreatedAt string `json:"created_at"`
    Status    string `json:"status"` // active, promoted, deprecated
}

// ForkIdentity creates a new identity branch from current IDENTITY.md.
func ForkIdentity(binaryDir, branchName, parentBranch string) error {
    // Read current main identity
    mainPath := filepath.Join(binaryDir, "IDENTITY.md")
    data, err := os.ReadFile(mainPath)
    if err != nil {
        return fmt.Errorf("read identity: %w", err)
    }

    // Ensure IDENTITIES directory exists
    identitiesDir := filepath.Join(binaryDir, "IDENTITIES")
    if err := os.MkdirAll(identitiesDir, 0755); err != nil {
        return fmt.Errorf("create identities dir: %w", err)
    }

    // Write branch file
    branchPath := filepath.Join(identitiesDir, branchName+".md")
    if err := os.WriteFile(branchPath, data, 0644); err != nil {
        return fmt.Errorf("write branch: %w", err)
    }

    // Update branches.json
    branchesPath := filepath.Join(identitiesDir, "branches.json")
    var branches []IdentityBranch

    if existing, err := os.ReadFile(branchesPath); err == nil {
        json.Unmarshal(existing, &branches)
    }

    parent := "main"
    if parentBranch != "" {
        parent = parentBranch
    }

    branches = append(branches, IdentityBranch{
        Name:      branchName,
        Parent:    parent,
        CreatedAt: time.Now().Format(time.RFC3339),
        Status:    "active",
    })

    data, _ = json.MarshalIndent(branches, "", "  ")
    if err := os.WriteFile(branchesPath, data, 0644); err != nil {
        return fmt.Errorf("write branches: %w", err)
    }

    return nil
}

// ListIdentityBranches returns all identity branches.
func ListIdentityBranches(binaryDir string) ([]IdentityBranch, error) {
    branchesPath := filepath.Join(binaryDir, "IDENTITIES", "branches.json")
    data, err := os.ReadFile(branchesPath)
    if err != nil {
        return nil, err
    }
    var branches []IdentityBranch
    if err := json.Unmarshal(data, &branches); err != nil {
        return nil, err
    }
    return branches, nil
}

// SwitchIdentityBranch switches the active identity to a branch.
func SwitchIdentityBranch(binaryDir, branchName string) error {
    branchPath := filepath.Join(binaryDir, "IDENTITIES", branchName+".md")
    data, err := os.ReadFile(branchPath)
    if err != nil {
        return fmt.Errorf("read branch %s: %w", branchName, err)
    }
    mainPath := filepath.Join(binaryDir, "IDENTITY.md")
    return os.WriteFile(mainPath, data, 0644)
}

// PromoteIdentityBranch merges a branch into main.
func PromoteIdentityBranch(binaryDir, branchName string) error {
    // Read branch content
    branchPath := filepath.Join(binaryDir, "IDENTITIES", branchName+".md")
    data, err := os.ReadFile(branchPath)
    if err != nil {
        return fmt.Errorf("read branch: %w", err)
    }

    // Read current main
    mainPath := filepath.Join(binaryDir, "IDENTITY.md")
    mainData, err := os.ReadFile(mainPath)
    if err != nil {
        return fmt.Errorf("read main: %w", err)
    }

    // Backup current main as main.backup
    if err := os.WriteFile(mainPath+".backup", mainData, 0644); err != nil {
        return fmt.Errorf("backup main: %w", err)
    }

    // Overwrite main with branch content
    if err := os.WriteFile(mainPath, data, 0644); err != nil {
        return fmt.Errorf("promote branch: %w", err)
    }

    // Mark branch as promoted
    branchesPath := filepath.Join(binaryDir, "IDENTITIES", "branches.json")
    var branches []IdentityBranch
    if existing, err := os.ReadFile(branchesPath); err == nil {
        json.Unmarshal(existing, &branches)
    }
    for i := range branches {
        if branches[i].Name == branchName {
            branches[i].Status = "promoted"
        }
    }
    out, _ := json.MarshalIndent(branches, "", "  ")
    os.WriteFile(branchesPath, out, 0644)

    return nil
}

// CompareIdentityBranches returns diff between two branches.
func CompareIdentityBranches(binaryDir, a, b string) (string, error) {
    aPath := filepath.Join(binaryDir, "IDENTITIES", a+".md")
    bPath := filepath.Join(binaryDir, "IDENTITIES", b+".md")

    aData, err := os.ReadFile(aPath)
    if err != nil {
        return "", fmt.Errorf("read branch %s: %w", a, err)
    }
    bData, err := os.ReadFile(bPath)
    if err != nil {
        return "", fmt.Errorf("read branch %s: %w", b, err)
    }

    return fmt.Sprintf("--- %s\n+++ %s\n%s", a, b, diff strings(string(aData), string(bData))), nil
}
```

- [ ] **Step 4: Run identity fork test to verify it passes**

Run: `go test ./core -run TestForkIdentity -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add core/identity_fork.go core/identity_fork_test.go
git commit -m "feat: add identity forking system (git-branch style branching)"
```

---

## Task 5: Tool Expansions (file-aware, task_execute, web_synthesize)

**Files:**
- Modify: `mpm-agent/core/agent.go` (add new tools)
- Add tests for new tools

- [ ] **Step 1: Write test for task_execute tool**

```go
// mpm-agent/core/tools_test.go
package core

func TestTaskExecute(t *testing.T) {
    // Test step execution
    steps := []map[string]interface{}{
        {"tool": "shell", "args": map[string]interface{}{"command": "echo hello"}},
    }
    results, err := ExecuteSteps(steps)
    if err != nil {
        t.Fatalf("ExecuteSteps failed: %v", err)
    }
    if len(results) != 1 {
        t.Errorf("expected 1 result, got %d", len(results))
    }
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./core -run TestTaskExecute -v`
Expected: FAIL — ExecuteSteps doesn't exist

- [ ] **Step 3: Write task_execute implementation**

Add to `mpm-agent/core/tools.go`:

```go
// ExecuteSteps runs a list of steps sequentially and returns results.
// Each step is a map with "tool", "args", and optional "checkpoint".
func ExecuteSteps(steps []map[string]interface{}) ([]map[string]interface{}, error) {
    var results []map[string]interface{}
    for i, step := range steps {
        tool, _ := step["tool"].(string)
        args, _ := step["args"].(map[string]interface{})
        checkpoint, hasCheckpoint := step["checkpoint"].(string)

        if tool == "" {
            results = append(results, map[string]interface{}{
                "step":    i,
                "error":   "no tool specified",
            })
            continue
        }

        result, err := executeTool(tool, args)
        stepResult := map[string]interface{}{
            "step":   i,
            "tool":   tool,
            "result": result,
        }
        if err != nil {
            stepResult["error"] = err.Error()
        }
        if hasCheckpoint {
            stepResult["checkpoint"] = checkpoint
        }
        results = append(results, stepResult)

        // Stop on error unless checkpoint says to continue
        if err != nil && !hasCheckpoint {
            break
        }
    }
    return results, nil
}

// executeTool runs a single tool by name with args.
func executeTool(tool string, args map[string]interface{}) (string, error) {
    switch tool {
    case "shell":
        cmd, _ := args["command"].(string)
        out, err := exec.Command("sh", "-c", cmd).CombinedOutput()
        if err != nil {
            return string(out), err
        }
        return string(out), nil
    case "read_file":
        path, _ := args["path"].(string)
        content, err := os.ReadFile(path)
        if err != nil {
            return "", err
        }
        return string(content), nil
    case "write_file":
        path, _ := args["path"].(string)
        content, _ := args["content"].(string)
        err := os.WriteFile(path, []byte(content), 0644)
        if err != nil {
            return "", err
        }
        return fmt.Sprintf("Written %d bytes", len(content)), nil
    default:
        return "", fmt.Errorf("unknown tool: %s", tool)
    }
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./core -run TestTaskExecute -v`
Expected: PASS

- [ ] **Step 5: Write web_synthesize tool test**

```go
func TestWebSynthesize(t *testing.T) {
    result, err := WebSynthesize("Go language history")
    if err != nil {
        t.Fatalf("WebSynthesize failed: %v", err)
    }
    if result == "" {
        t.Error("expected non-empty result")
    }
}
```

- [ ] **Step 6: Run web synthesize test to verify it fails**

Run: `go test ./core -run TestWebSynthesize -v`
Expected: FAIL — WebSynthesize doesn't exist

- [ ] **Step 7: Write web_synthesize implementation**

```go
// WebSynthesize searches for a query and produces a synthesized answer.
func WebSynthesize(query string) (string, error) {
    // Use existing web_search to get results
    searchResults, err := handleWebSearchTool(query)
    if err != nil {
        return "", fmt.Errorf("web search: %w", err)
    }

    // Parse key facts from search results
    // Return synthesized summary with citations
    lines := strings.Split(strings.TrimSpace(searchResults), "\n")
    var facts []string
    for i, line := range lines {
        if i > 10 {
            break
        }
        if len(line) > 20 {
            facts = append(facts, truncate(line, 200))
        }
    }

    if len(facts) == 0 {
        return "No results found for: " + query, nil
    }

    return fmt.Sprintf("## Synthesis for: %s\n\n%s\n\nSources: web search", query, strings.Join(facts, "\n")), nil
}

// handleWebSearchTool is a helper that calls web search.
func handleWebSearchTool(query string) (string, error) {
    searchURL := "https://duckduckgo.com/html/?q=" + url.QueryEscape(query)
    req, _ := http.NewRequest("GET", searchURL, nil)
    req.Header.Set("User-Agent", "Mozilla/5.0")
    client := &http.Client{Timeout: 10 * time.Second}
    resp, err := client.Do(req)
    if err != nil {
        return "", err
    }
    defer resp.Body.Close()
    body, _ := io.ReadAll(io.LimitReader(resp.Body, 8000))
    return string(body), nil
}
```

- [ ] **Step 8: Run web synthesize test to verify it passes**

Run: `go test ./core -run TestWebSynthesize -v`
Expected: PASS

- [ ] **Step 9: Write file-aware read_file test**

```go
func TestReadFileSemantic(t *testing.T) {
    dir := t.TempDir()
    testFile := filepath.Join(dir, "test.txt")
    os.WriteFile(testFile, []byte("Line 1\nLine 2\nLine 3\nLine 4"), 0644)

    result := ReadFileSemantic(testFile, "summary")
    if result == "" {
        t.Error("expected non-empty result")
    }
}
```

- [ ] **Step 10: Run semantic read test to verify it fails**

Run: `go test ./core -run TestReadFileSemantic -v`
Expected: FAIL

- [ ] **Step 11: Write file-aware read_file implementation**

```go
// ReadFileSemantic reads a file with semantic understanding.
// mode: "full" (default), "summary", "code", "compare"
func ReadFileSemantic(path, mode string) string {
    content, err := os.ReadFile(path)
    if err != nil {
        return fmt.Sprintf("error reading %s: %v", path, err)
    }

    text := string(content)
    switch mode {
    case "summary":
        lines := strings.Split(text, "\n")
        if len(lines) > 10 {
            return fmt.Sprintf("File has %d lines. Key lines:\n%s\n...(%d more lines)",
                len(lines), strings.Join(lines[:5], "\n"), len(lines)-5)
        }
        return text
    case "code":
        var codeLines []string
        for i, line := range strings.Split(text, "\n") {
            if strings.Contains(line, "func ") || strings.Contains(line, "type ") ||
                strings.Contains(line, "const ") || strings.Contains(line, "var ") {
                codeLines = append(codeLines, fmt.Sprintf("%d: %s", i+1, line))
            }
        }
        if len(codeLines) == 0 {
            return "No code structures found"
        }
        return strings.Join(codeLines, "\n")
    default:
        return truncate(text, 2000)
    }
}
```

- [ ] **Step 12: Run semantic read test to verify it passes**

Run: `go test ./core -run TestReadFileSemantic -v`
Expected: PASS

- [ ] **Step 13: Commit**

```bash
git add core/tools.go core/tools_test.go
git commit -m "feat: add task_execute, web_synthesize, and file-aware read tools"
```

---

## Task 6: Update Agent to Use IDENTITY.md + Costume Personas

**Files:**
- Modify: `mpm-agent/core/agent.go`

- [ ] **Step 1: Write test for identity + persona layering**

```go
func TestBuildSystemPromptWithIdentityAndPersona(t *testing.T) {
    binaryDir := t.TempDir()
    os.WriteFile(filepath.Join(binaryDir, "IDENTITY.md"), []byte(`# TestBot
Type: assistant
Core traits: precise`), 0644)

    // Mock GetBinaryDir to return temp dir
    // Build prompt with identity + MPM persona costume
    personaContent := "You are currently wearing the 'analyst' persona."
    prompt := BuildSystemPromptWithIdentity(binaryDir, personaContent, "", nil, nil, nil)

    if !strings.Contains(prompt, "TestBot") {
        t.Error("expected identity name in prompt")
    }
    if !strings.Contains(prompt, "wearing") {
        t.Error("expected persona costume in prompt")
    }
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./core -run TestBuildSystemPromptWithIdentityAndPersona -v`
Expected: FAIL — BuildSystemPromptWithIdentity doesn't exist

- [ ] **Step 3: Write system prompt builder with identity + persona layering**

```go
// BuildSystemPromptWithIdentity builds the full system prompt.
// Priority: IDENTITY.md (core, stable) + persona (costume overlay) + mode (behavior rules)
func BuildSystemPromptWithIdentity(binaryDir, personaContent, modeContent string, memories, directives, references []string) string {
    identity, _ := LoadIdentity(binaryDir)

    var sb strings.Builder

    // Core identity from IDENTITY.md
    if identity != nil {
        sb.WriteString(fmt.Sprintf("You are %s (v%s) — %s. ", identity.Name, identity.Version, identity.Type))
        if identity.Traits != "" {
            sb.WriteString(fmt.Sprintf("Core traits: %s. ", identity.Traits))
        }
        if identity.Boundaries != "" {
            sb.WriteString(fmt.Sprintf("Boundaries: %s. ", identity.Boundaries))
        }
    } else {
        sb.WriteString("You are mini-bot. ")
    }

    // Persona costume overlay (MPM personas)
    if personaContent != "" {
        sb.WriteString(fmt.Sprintf("\n\n[Persona Costume] %s", personaContent))
    }

    // Mode behavior rules
    if modeContent != "" {
        sb.WriteString(fmt.Sprintf("\n\n[Mode] %s", modeContent))
    }

    // Memory context
    if len(memories) > 0 {
        sb.WriteString("\n\n## Relevant Memories\n")
        for _, m := range memories {
            sb.WriteString(fmt.Sprintf("- %s\n", m))
        }
    }

    // Directives
    if len(directives) > 0 {
        sb.WriteString("\n## Prime Directives\n")
        for _, d := range directives {
            sb.WriteString(fmt.Sprintf("- %s\n", d))
        }
    }

    // References
    if len(references) > 0 {
        sb.WriteString("\n## Reference Material\n")
        for _, r := range references {
            sb.WriteString(fmt.Sprintf("- %s\n", r))
        }
    }

    sb.WriteString(fmt.Sprintf("\n## Current Date: %s", time.Now().Format("2006-01-02")))
    return sb.String()
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./core -run TestBuildSystemPromptWithIdentityAndPersona -v`
Expected: PASS

- [ ] **Step 5: Update RunAgent to use new prompt builder and anchoring**

```go
// In RunAgent, replace buildSystemPrompt call:
func RunAgent(query string, history []map[string]interface{}, cfg *Config, onToken func(string), onComplete func(*LLMResponse)) (string, error) {
    // ... existing db open ...
    defer db.Close()

    // Load identity and persona
    binaryDir := GetBinaryDir()
    identity, _ := LoadIdentity(binaryDir)
    personaName, personaContent, _ := getActivePersona(db)
    modeName, modeContent, _ := getActiveMode(db)

    // Get anchors as high-priority context
    anchors, _ := GetRecentAnchors(db, 5)
    var anchorCtx []string
    for _, a := range anchors {
        anchorCtx = append(anchorCtx, FormatAnchorContext(a))
    }

    // Get recent lessons for context
    lessons, _ := GetRecentLessons(db, 3)

    // Build system prompt with identity-first approach
    systemPrompt := BuildSystemPromptWithIdentity(binaryDir, personaContent, modeContent,
        retrieveMemories(db, query, 5),
        retrieveDirectives(db),
        retrieveReferences(db, query, 3))

    // ... rest of existing RunAgent logic ...
}
```

- [ ] **Step 6: Run all core tests to verify nothing is broken**

Run: `go test ./core -v -count=1`
Expected: All PASS

- [ ] **Step 7: Commit**

```bash
git add core/agent.go
git commit -m "feat: integrate IDENTITY.md as core identity with persona costume layering"
```

---

## Task 7: Rename Binaries & Update Process Names

**Files:**
- Rename: `mpm-agent/mpm_agent.go` → `mpm-agent/cmd/mini-bot/main.go`
- Rename: `mpm-agent/cmd/telegram/main.go` → `cmd/telegram/telegram.go`
- Update process names (`os.Args[0] = "mini-bot"`)

- [ ] **Step 1: Rename mpm_agent.go to cmd/mini-bot/main.go**

Copy content to new file with updated process name:

```go
// mpm-agent/cmd/mini-bot/main.go
package main

import (
    "bufio"
    "fmt"
    "io"
    "os"
    "strings"
    "strconv"

    "mpm-agent/core"
)

func main() {
    // Set process name with PID
    pid := os.Getpid()
    os.Args[0] = fmt.Sprintf("mini-bot[%d]", pid)
    log.Printf("mini-bot[%d]: Starting...", pid)

    // Scaffold config if needed
    if err := core.EnsureConfig(); err != nil {
        log.Printf("mini-bot[%d]: warning: could not create default config: %v", pid, err)
    }

    cfg, err := core.LoadConfig()
    if err != nil {
        log.Printf("mini-bot[%d]: config error: %v", pid, err)
        os.Exit(1)
    }

    log.Printf("mini-bot[%d]: Loaded identity from %s", pid, core.GetConfigPath())

    // ... rest same as existing mpm_agent.go ...
}
```

- [ ] **Step 2: Update telegram main.go**

```go
// mpm-agent/cmd/telegram/telegram.go
func main() {
    pid := os.Getpid()
    os.Args[0] = fmt.Sprintf("mini-bot-telegram[%d]", pid)
    log.SetFlags(log.LstdFlags | log.Lshortfile)
    log.Printf("mini-bot-telegram[%d]: Starting...", pid)

    // Load mini-bot-config.json
    configPath := core.GetConfigPath()
    cfg, err := core.LoadMiniBotConfig(configPath)
    if err != nil {
        log.Fatalf("mini-bot-telegram[%d]: Config error: %v", pid, err)
    }
    log.Printf("mini-bot-telegram[%d]: Config loaded from %s", pid, configPath)

    // Load identity
    identity, _ := core.LoadIdentity(core.GetBinaryDir())
    if identity != nil {
        log.Printf("mini-bot-telegram[%d]: Identity: %s v%s", pid, identity.Name, identity.Version)
    }

    // ... rest same as existing ...
}
```

- [ ] **Step 3: Commit**

```bash
git add cmd/mini-bot/main.go cmd/telegram/telegram.go
git commit -m "refactor: rename binaries and add PID to process names"
```

---

## Task 8: Create IDENTITY.md and mini-bot-config.json in bin/ directory

**Files:**
- Create: `bin/IDENTITY.md` (example identity)
- Create: `bin/mini-bot-config.json` (default config)

- [ ] **Step 1: Create IDENTITY.md**

```markdown
# MiniBot v1.0
Type: helpful assistant
Domain: general-purpose
Core traits: patient, thorough, cites sources, asks clarifying questions when unsure
Boundaries: never fabricate facts, always admit uncertainty, no political advice
```

- [ ] **Step 2: Create mini-bot-config.json**

```json
{
  "identity": {
    "name": "mini-bot",
    "version": "1.0"
  },
  "synth": {
    "model": "MiniMax-M2.7",
    "api_key": "",
    "base_url": "https://api.minimax.io/anthropic",
    "max_tokens": 4096,
    "timeout_seconds": 300
  },
  "telegram": {
    "bot_token": "",
    "polling": true,
    "allowed_users": []
  },
  "mcp": {
    "token": "",
    "socket_path": "mini-bot-mcp.sock"
  },
  "self_improve": {
    "enabled": true,
    "anchor_threshold": 3,
    "lesson_complexity_threshold": 7,
    "identity_patch_approval_required": true
  },
  "paths": {
    "db": "mini-bot.db",
    "identity": "IDENTITY.md",
    "mcp_socket": "mini-bot-mcp.sock"
  }
}
```

- [ ] **Step 3: Commit**

```bash
git add bin/IDENTITY.md bin/mini-bot-config.json
git commit -m "feat: add IDENTITY.md and default mini-bot-config.json"
```

---

## Task 9: MCP Server Update (token auth, shared socket)

**Files:**
- Modify: `mpm-agent/cmd/mcp/main.go` (rename to mini-bot-mcp)

- [ ] **Step 1: Update MCP main.go with shared socket and token auth**

```go
// mpm-agent/cmd/mcp/main.go
// Updated to use mini-bot-config.json and shared socket
func main() {
    pid := os.Getpid()
    os.Args[0] = fmt.Sprintf("mini-bot-mcp[%d]", pid)

    // Load mini-bot config for token auth
    configPath := core.GetConfigPath()
    cfg, err := core.LoadMiniBotConfig(configPath)
    if err != nil {
        log.Printf("mini-bot-mcp[%d]: warning: could not load config: %v", pid, err)
    }

    // Token auth from config or env
    expectedToken := ""
    if cfg != nil {
        expectedToken = cfg.MCP.Token
    }
    if expectedToken == "" {
        expectedToken = os.Getenv("MPM_API_TOKEN")
    }

    // Use mini-bot.db for MCP tool access (connects to mpm.db for MPM tools)
    db, err := core.OpenDB()
    if err != nil {
        log.Fatalf("mini-bot-mcp[%d]: open db: %v", pid, err)
    }
    defer db.Close()

    log.Printf("mini-bot-mcp[%d]: Ready on socket (token auth: %v)", pid, expectedToken != "")

    // ... JSON-RPC loop same as existing ...
}
```

- [ ] **Step 2: Commit**

```bash
git add cmd/mcp/main.go
git commit -m "feat: update mpm-agent-mcp to mini-bot-mcp with token auth from config"
```

---

## Task 10: Build and Test the Complete System

**Files:**
- Update: `mpm-agent/Makefile` (add mini-bot build targets)

- [ ] **Step 1: Update Makefile with mini-bot targets**

```makefile
# mini-bot build targets
mini-bot: cmd/mini-bot/main.go core/*.go
	go build -o bin/mini-bot ./cmd/mini-bot

mini-bot-telegram: cmd/telegram/*.go core/*.go
	go build -o bin/mini-bot-telegram ./cmd/telegram

mini-bot-mcp: cmd/mcp/*.go core/*.go
	go build -o bin/mini-bot-mcp ./cmd/mcp

build: mini-bot mini-bot-telegram mini-bot-mcp

test-core:
	go test -v ./core/...

.PHONY: build test-core
```

- [ ] **Step 2: Run build**

Run: `make build`
Expected: All three binaries compile without errors

- [ ] **Step 3: Run tests**

Run: `go test -v ./core/... -count=1`
Expected: All tests pass

- [ ] **Step 4: Commit**

```bash
git add Makefile
git commit -m "feat: add mini-bot build targets to Makefile"
```

---

## Spec Coverage Check

| Spec Section | Task(s) |
|---------------|---------|
| Renames & process identity | Task 7 |
| IDENTITY.md system | Tasks 1, 6 |
| mini-bot.db schema | Task 2 |
| Config (mini-bot-config.json) | Task 3 |
| Shared MCP | Task 9 |
| Memory anchoring (Priority 1) | Task 2 |
| Task handoff (Priority 2) | Task 5 |
| File-aware tools (Priority 3) | Task 5 |
| Web synthesis (Priority 4) | Task 5 |
| Lesson extraction | Task 4 |
| Identity patching | Task 4 |
| Tool registry | Task 4 |
| Agent integration | Task 6 |

All spec sections are covered.

---

**Plan complete and saved to `docs/superpowers/plans/2026-04-12-mini-bot-implementation.md`.**

Two execution options:

**1. Subagent-Driven (recommended)** - I dispatch a fresh subagent per task, review between tasks, fast iteration

**2. Inline Execution** - Execute tasks in this session using executing-plans, batch execution with checkpoints

Which approach?