# mini-bot-telegram Token Reduction Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Reduce input tokens for "hi mini-bot" from ~7709 to ≤1500 by trimming session history (50→10), context retrieval limits, and tool schema verbosity.

**Architecture:** Five targeted changes: (1) session history cap, (2) context retrieval caps (anchors/lessons/memories/references/directives), (3) duplicate tool removal, (4) input_schema trim, (5) directive retrieval limit. No architectural changes — purely parameter and schema trimming.

**Tech Stack:** Go (mpm-agent), SQLite/FTS5

---

## File Map

| File | Change type |
|------|-------------|
| `cmd/telegram/session.go:15` | Modify `maxHistoryMessages` constant |
| `core/agent.go:24-26` | Modify `maxHistoryMessages` constant |
| `core/agent.go:270-276` | Modify limits in `RunAgent` |
| `core/agent.go:150` | Modify `LIMIT` in `retrieveDirectives` |
| `core/agent.go:467-468` | Add skip for `execute_mpm_command` in `buildToolListWithLoaded` |
| `core/tools.go:1192-1441` | Trim `input_schema` descriptions in `init()` |
| `core/agent_test.go:53-57` | Update test to expect ≤15 (not ≤100) |

---

## Task 1: Trim Session History (50 → 10)

**Files:**
- Modify: `cmd/telegram/session.go:15`
- Modify: `core/agent.go:24-26`
- Modify: `core/agent_test.go:53-57`

- [ ] **Step 1: Update `session.go` constant**

```go
// cmd/telegram/session.go:15
// Before
const maxHistoryMessages = 50

// After
const maxHistoryMessages = 10
```

- [ ] **Step 2: Update `core/agent.go` constant**

```go
// core/agent.go:24
// Before
maxHistoryMessages = 50

// After
maxHistoryMessages = 10
```

- [ ] **Step 3: Update test expectations**

The test at `core/agent_test.go:53-57` has two checks:
- `maxHistoryMessages <= 0` — error if not positive (keep this)
- `maxHistoryMessages > 100` — warn if too large (change to `> 20`)

```go
// core/agent_test.go:56
// Before
if maxHistoryMessages > 100 {

// After
if maxHistoryMessages > 20 {
```

- [ ] **Step 4: Verify**

Run: `grep -n "maxHistoryMessages" cmd/telegram/session.go core/agent.go core/agent_test.go`
Expected: Both constants show `10`, test threshold shows `20`

- [ ] **Step 5: Commit**

```bash
git add cmd/telegram/session.go core/agent.go core/agent_test.go
git commit -m "feat: trim session history from 50 to 10 messages"
```

---

## Task 2: Trim Context Retrieval Limits

**Files:**
- Modify: `core/agent.go:270-276`

- [ ] **Step 1: Update `RunAgent` retrieval limits**

```go
// core/agent.go:270-276 — in RunAgent()

// Before
anchors, _ := GetRecentAnchors(db, 10)
lessons, _ := GetRecentLessons(db, 3)
memories := retrieveMemories(db, query, 5)
references := retrieveReferences(db, query, 3)

// After
anchors, _ := GetRecentAnchors(db, 5)
lessons, _ := GetRecentLessons(db, 2)
memories := retrieveMemories(db, query, 3)
references := retrieveReferences(db, query, 2)
```

- [ ] **Step 2: Verify**

Run: `grep -n "GetRecentAnchors\|GetRecentLessons\|retrieveMemories\|retrieveReferences" core/agent.go | grep -v "func\|//"`
Expected: Lines show limits (5, 2, 3, 2)

- [ ] **Step 3: Commit**

```bash
git add core/agent.go
git commit -m "feat: trim context retrieval limits (anchors→5, lessons→2, memories→3, refs→2)"
```

---

## Task 3: Trim Directive Retrieval Limit (20 → 5)

**Files:**
- Modify: `core/agent.go:150`

- [ ] **Step 1: Update `retrieveDirectives` LIMIT**

```go
// core/agent.go:150
// Before
ORDER BY created_at DESC LIMIT 20

// After
ORDER BY created_at DESC LIMIT 5
```

- [ ] **Step 2: Verify**

Run: `grep -n "LIMIT" core/agent.go`
Expected: Line shows `LIMIT 5` for directives

- [ ] **Step 3: Commit**

```bash
git add core/agent.go
git commit -m "feat: trim directive retrieval from 20 to 5"
```

---

## Task 4: Remove Duplicate `execute_mpm_command`

**Files:**
- Modify: `core/agent.go:465-475`

- [ ] **Step 1: Add skip in `buildToolListWithLoaded` for duplicate**

Locate the loop that adds `baseTools` (around line 464-475). Add a check to skip `execute_mpm_command`:

```go
// core/agent.go:464-475
// Before
for _, name := range baseTools {
    def, ok := GetTool(name)
    if !ok || name == "execute_mpm_command" {
        continue
    }
    tools = append(tools, map[string]interface{}{
        "name":        def.Name,
        "description": def.Description,
        "input_schema": def.InputSchema,
    })
}

// After
for _, name := range baseTools {
    def, ok := GetTool(name)
    if !ok {
        continue
    }
    // execute_mpm_command is already hardcoded as a framework tool above — skip
    if name == "execute_mpm_command" {
        continue
    }
    tools = append(tools, map[string]interface{}{
        "name":        def.Name,
        "description": def.Description,
        "input_schema": def.InputSchema,
    })
}
```

- [ ] **Step 2: Verify**

Run: `grep -n "execute_mpm_command" core/agent.go`
Expected: Only one occurrence at line ~449 (hardcoded), none in the baseTools loop

- [ ] **Step 3: Commit**

```bash
git add core/agent.go
git commit -m "fix: remove duplicate execute_mpm_command in buildToolListWithLoaded"
```

---

## Task 5: Trim Tool `input_schema` Verbosity

**Files:**
- Modify: `core/tools.go:1192-1441`

- [ ] **Step 1: Trim all tool schemas to minimal form**

For each tool in `init()`, shorten descriptions to 1-line. Key targets:

```go
// core/tools.go — in init(), replace each ToolDefinition:

// read_file (around line 1193)
RegisterTool("read_file", ToolDefinition{
    Name:        "read_file",
    Description: "Read a file.",
    InputSchema: map[string]interface{}{
        "type": "object",
        "properties": map[string]interface{}{
            "path": map[string]interface{}{"type": "string"},
        },
        "required": []string{"path"},
    },
})

// write_file (around line 1207)
RegisterTool("write_file", ToolDefinition{
    Name:        "write_file",
    Description: "Write to a file.",
    InputSchema: map[string]interface{}{
        "type": "object",
        "properties": map[string]interface{}{
            "path":    map[string]interface{}{"type": "string"},
            "content": map[string]interface{}{"type": "string"},
        },
        "required": []string{"path", "content"},
    },
})

// ReadFileSemantic (around line 1225)
RegisterTool("ReadFileSemantic", ToolDefinition{
    Name:        "ReadFileSemantic",
    Description: "Read file with semantic modes: summary, code, compare.",
    InputSchema: map[string]interface{}{
        "type": "object",
        "properties": map[string]interface{}{
            "path": map[string]interface{}{"type": "string"},
            "mode": map[string]interface{}{"type": "string"},
        },
        "required": []string{"path"},
    },
})

// ReadFileCompare (around line 1243)
RegisterTool("ReadFileCompare", ToolDefinition{
    Name:        "ReadFileCompare",
    Description: "Compare two files.",
    InputSchema: map[string]interface{}{
        "type": "object",
        "properties": map[string]interface{}{
            "pathA": map[string]interface{}{"type": "string"},
            "pathB": map[string]interface{}{"type": "string"},
        },
        "required": []string{"pathA", "pathB"},
    },
})

// WebSynthesize (around line 1261)
RegisterTool("WebSynthesize", ToolDefinition{
    Name:        "WebSynthesize",
    Description: "Web search with synthesis.",
    InputSchema: map[string]interface{}{
        "type": "object",
        "properties": map[string]interface{}{
            "query": map[string]interface{}{"type": "string"},
        },
        "required": []string{"query"},
    },
})

// jq (around line 1275)
RegisterTool("jq", ToolDefinition{
    Name:        "jq",
    Description: "Filter JSON with jq.",
    InputSchema: map[string]interface{}{
        "type": "object",
        "properties": map[string]interface{}{
            "filter": map[string]interface{}{"type": "string"},
            "file":   map[string]interface{}{"type": "string"},
        },
        "required": []string{"filter", "file"},
    },
})

// update_identity_knowledge (around line 1293)
RegisterTool("update_identity_knowledge", ToolDefinition{
    Name:        "update_identity_knowledge",
    Description: "Record user identity facts.",
    InputSchema: map[string]interface{}{
        "type": "object",
        "properties": map[string]interface{}{
            "key":   map[string]interface{}{"type": "string"},
            "value": map[string]interface{}{"type": "string"},
        },
        "required": []string{"key", "value"},
    },
})

// list_toolkits (around line 1315)
RegisterTool("list_toolkits", ToolDefinition{
    Name:        "list_toolkits",
    Description: "List available toolkits.",
    InputSchema: map[string]interface{}{
        "type": "object",
        "properties": map[string]interface{}{},
    },
})

// load_toolkit (around line 1323)
RegisterTool("load_toolkit", ToolDefinition{
    Name:        "load_toolkit",
    Description: "Load a toolkit.",
    InputSchema: map[string]interface{}{
        "type": "object",
        "properties": map[string]interface{}{
            "name": map[string]interface{}{"type": "string"},
        },
        "required": []string{"name"},
    },
})

// unload_toolkit (around line 1337)
RegisterTool("unload_toolkit", ToolDefinition{
    Name:        "unload_toolkit",
    Description: "Unload a toolkit.",
    InputSchema: map[string]interface{}{
        "type": "object",
        "properties": map[string]interface{}{
            "name": map[string]interface{}{"type": "string"},
        },
        "required": []string{"name"},
    },
})

// execute_mpm_command (around line 1449) — description already short, no change needed
```

For coding tools (`rg`, `sg`, `repomap`, `git_status`, `git_commit`, `git_diff`) — trim to minimal:

```go
// rg (around line 1354)
RegisterTool("rg", ToolDefinition{
    Name:        "rg",
    Description: "Search with ripgrep.",
    InputSchema: map[string]interface{}{
        "type": "object",
        "properties": map[string]interface{}{
            "query": map[string]interface{}{"type": "string"},
            "path":  map[string]interface{}{"type": "string"},
        },
        "required": []string{"query"},
    },
})

// sg (around line 1368)
RegisterTool("sg", ToolDefinition{
    Name:        "sg",
    Description: "Code analysis with ast-grep.",
    InputSchema: map[string]interface{}{
        "type": "object",
        "properties": map[string]interface{}{
            "path":  map[string]interface{}{"type": "string"},
            "rule":  map[string]interface{}{"type": "string"},
            "query": map[string]interface{}{"type": "string"},
        },
        "required": []string{"path"},
    },
})

// repomap (around line 1382)
RegisterTool("repomap", ToolDefinition{
    Name:        "repomap",
    Description: "Generate repo symbol map.",
    InputSchema: map[string]interface{}{
        "type": "object",
        "properties": map[string]interface{}{
            "path":  map[string]interface{}{"type": "string"},
            "depth": map[string]interface{}{"type": "integer"},
        },
    },
})

// git_status (around line 1394)
RegisterTool("git_status", ToolDefinition{
    Name:        "git_status",
    Description: "Show git status.",
    InputSchema: map[string]interface{}{
        "type": "object",
        "properties": map[string]interface{}{
            "repo": map[string]interface{}{"type": "string"},
        },
    },
})

// git_commit (around line 1405)
RegisterTool("git_commit", ToolDefinition{
    Name:        "git_commit",
    Description: "Create a commit.",
    InputSchema: map[string]interface{}{
        "type": "object",
        "properties": map[string]interface{}{
            "message": map[string]interface{}{"type": "string"},
        },
        "required": []string{"message"},
    },
})

// git_diff (around line 1417)
RegisterTool("git_diff", ToolDefinition{
    Name:        "git_diff",
    Description: "Show uncommitted changes.",
    InputSchema: map[string]interface{}{
        "type": "object",
        "properties": map[string]interface{}{
            "file": map[string]interface{}{"type": "string"},
        },
    },
})

// execute_shell (around line 1428)
RegisterTool("execute_shell", ToolDefinition{
    Name:        "execute_shell",
    Description: "Run a shell command.",
    InputSchema: map[string]interface{}{
        "type": "object",
        "properties": map[string]interface{}{
            "command": map[string]interface{}{"type": "string"},
            "cwd":     map[string]interface{}{"type": "string"},
        },
        "required": []string{"command"},
    },
})
```

- [ ] **Step 2: Verify build compiles**

Run: `cd /home/v/.openclaw/workspace/projects/mpm/mpm-agent && CGO_CFLAGS="-DSQLITE_ENABLE_FTS5" CGO_LDFLAGS="-lm" go build ./...`
Expected: No errors

- [ ] **Step 3: Commit**

```bash
git add core/tools.go
git commit -m "feat: trim tool input_schema to minimal form for openrouter/free"
```

---

## Verification

After all tasks complete:

1. **Build:** `cd .../mpm-agent && make build`
2. **Run single test:** `CGO_CFLAGS="-DSQLITE_ENABLE_FTS5" CGO_LDFLAGS="-lm" go test ./core/... -run TestMaxHistory -v`
3. **Manual smoke test:** Send "hi mini-bot" via Telegram → confirm input tokens ≤ 1500 (log line: `[agent] API usage: input=X`)

**Build verification only — no new unit tests needed** for parameter changes. The behavioral contract (max X messages enforced) is tested by existing `core/agent_test.go` checks.