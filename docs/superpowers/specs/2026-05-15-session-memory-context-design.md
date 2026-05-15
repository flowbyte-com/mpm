# Session Memory Context — Phase 1 Technical Design

**Date:** 2026-05-15
**Author:** 808 (∓808)
**Status:** Draft for review

---

## Overview

Adding session wake-up context so the agent knows immediately upon session start: "last session you worked on X, had Y open, learned Z." This is stored passively — no active recording during the session, just a clean read on wake-up.

**Design principle:** Passive write-on-save. `active_mode` and `active_persona` are injected into memory metadata when any memory is saved (not on every keystroke). The wake-up command reads the most recent session's memories to surface context.

---

## 1. The Write Path — State Tracking

### 1.1 Where State Comes From

`active_mode` and `active_persona` are derived from CLI invocation context, not from agent introspection:

- **Mode:** Set by `--mode` flag on `mpm add`, `mpm recall`, etc. Persisted in `config/current_mode` file by `mpm mode set <name>`.
- **Persona:** Set by `--persona` flag or by loading a persona file via `mpm persona load <name>`. Persisted in `config/current_persona` file.

The CLI handlers (`cmd/mpm/handlers.go`, `cmd/mpm/simple_cmds.go`) read these values at invocation time and pass them into `AddMemory` via the metadata map.

### 1.2 How `AddMemory` Injects State

In `internal/memory.go:302-311`, the `fullMetadata` map is built before insert. We extend it to include `active_mode` and `active_persona` when they are available in the calling context:

```go
// Add metadata fields for filtering
fullMetadata := map[string]interface{}{
    "source":    source,
    "created":   mem.Created,
    "tags":      strings.Join(tags, ","),
    "timestamp": time.Now().Unix(),
}
// Caller-supplied mode/persona (from CLI invocation context)
if mode := os.Getenv("MPM_ACTIVE_MODE"); mode != "" {
    fullMetadata["active_mode"] = mode
}
if persona := os.Getenv("MPM_ACTIVE_PERSONA"); persona != "" {
    fullMetadata["active_persona"] = persona
}
// ... merge caller's metadata on top
for k, v := range metadata {
    fullMetadata[k] = v
}
```

The environment variables are set by `handleModeSet` / `handlePersonaLoad` after they write the config files. Alternatively, they can be passed directly through the handler chain via a context struct or package-level variables set before `AddMemory` calls.

### 1.3 Package-Level State (Simpler Approach)

Since MPM runs in a single process and handlers call `AddMemory` synchronously within the same goroutine, we can use package-level variables set at the start of each CLI invocation:

```go
// cmd/mpm/handlers.go (package level)
var activeMode string
var activePersona string

// At the start of each handler, before calling AddMemory:
func detectActiveContext() (mode, persona string) {
    // Read from config files
    modePath := filepath.Join(config.GetMPMDir(), "config", "current_mode")
    if data, err := os.ReadFile(modePath); err == nil {
        mode = strings.TrimSpace(string(data))
    }
    personaPath := filepath.Join(config.GetMPMDir(), "config", "current_persona")
    if data, err := os.ReadFile(personaPath); err == nil {
        persona = strings.TrimSpace(string(data))
    }
    return
}

// In AddMemory, merge these into fullMetadata
if activeMode != "" {
    fullMetadata["active_mode"] = activeMode
}
if activePersona != "" {
    fullMetadata["active_persona"] = activePersona
}
```

This avoids the goroutine-safety concern — single-process, handler executes, then state is cleared or remains for the next invocation. No concurrent access because each CLI invocation is sequential.

### 1.4 Session Boundary

Sessions are tracked via `session_id` in `memories` (the `sessions` table stores session metadata). When a session ends (process exits), the `active_mode`/`active_persona` for that session are implicitly preserved in every memory that was saved during it.

---

## 2. The Read Path — Wake-Up Command

### 2.1 New Command: `mpm wake`

```
mpm wake
```

Returns human-readable context about the previous session:

```
╭─ Last Session ──────────────────────────────────────────────╮
│ Mode:      programming                                       │
│ Persona:   ∑808                                              │
│ Topics:    golang-patterns, sqlite-optimization              │
│                                                           │
│ Recent memories:                                            │
│   • "Reviewing PR #42 — the FTS5 JOIN is 3x faster"        │
│   • "Switched to tiktoken batch encoding"                  │
│   • "Fixed memory leak in watch daemon"                    │
╰───────────────────────────────────────────────────────────╯
```

### 2.2 Implementation: `handleWake` in `cmd/mpm/handlers.go`

1. Find the most recent `session` record (by `created_at` DESC, limit 1)
2. Query memories from that session (ORDER BY `created_at` DESC, limit 5)
3. Extract `active_mode` and `active_persona` from the most recent memory's metadata
4. Query topics linked to those memories
5. Format output

```go
func handleWake(args []string) int {
    dm, err := mpminternal.NewDatabaseManager("")
    if err != nil {
        fmt.Fprintf(os.Stderr, "Error: %v\n", err)
        return 1
    }
    defer dm.Close()

    // Get most recent session
    session, err := dm.GetLastSession()
    if err != nil || session == nil {
        fmt.Println("No previous session found.")
        return 0
    }

    // Get recent memories from this session
    memories, err := dm.GetSessionMemories(session.ID, 5)
    if err != nil {
        fmt.Fprintf(os.Stderr, "Error: %v\n", err)
        return 1
    }

    // Extract active_mode/persona from most recent memory
    var activeMode, activePersona string
    if len(memories) > 0 {
        if m, ok := memories[0].Metadata["active_mode"]; ok {
            activeMode = fmt.Sprintf("%v", m)
        }
        if p, ok := memories[0].Metadata["active_persona"]; ok {
            activePersona = fmt.Sprintf("%v", p)
        }
    }

    // Get topics linked to these memories
    var topicNames []string
    for _, mem := range memories {
        topics, _ := dm.GetMemoryTopics(mem.ID)
        for _, t := range topics {
            topicNames = append(topicNames, t.Name)
        }
    }
    // Deduplicate
    seen := map[string]bool{}
    uniqueTopics := []string{}
    for _, n := range topicNames {
        if !seen[n] {
            seen[n] = true
            uniqueTopics = append(uniqueTopics, n)
        }
    }

    // Format output
    fmt.Printf("Mode: %s\n", activeMode)
    fmt.Printf("Persona: %s\n", activePersona)
    fmt.Printf("Topics: %s\n", strings.Join(uniqueTopics[:3], ", "))
    fmt.Println("\nRecent memories:")
    for _, mem := range memories {
        content := mem.Content
        if len(content) > 80 {
            content = content[:80] + "…"
        }
        fmt.Printf("  • %s\n", content)
    }
    return 0
}
```

### 2.3 JSON Output Support

`mpm wake --json` returns structured JSON for tool integration:

```json
{
  "session_id": "abc123",
  "active_mode": "programming",
  "active_persona": "∑808",
  "recent_topics": ["golang-patterns", "sqlite-optimization"],
  "recent_memories": [
    {"id": "...", "content": "...", "created_at": "..."},
    {"id": "...", "content": "...", "created_at": "..."}
  ]
}
```

---

## 3. The Agent Hook — TypeScript Plugin Integration

### 3.1 OpenClaw Plugin Initialization

The OpenClaw TypeScript plugin (`openclaw/mpm-plugin/src/index.ts`) calls `mpm wake --json` at initialization to inject context into the agent's system prompt or working context.

The call happens in the plugin's `onAgentStart` or equivalent initialization hook:

```typescript
// In openclaw/mpm-plugin/src/index.ts — plugin initialization
async function onAgentStart(ctx: OpenClawPluginToolContext) {
  // Fetch last session context
  const wakeResult = await runMpmCommand(['wake', '--json']);
  const context = JSON.parse(wakeResult.stdout);

  // Surface context to agent — either via a tool result or direct prompt injection
  ctx.agent.setContext('lastSession', context);
}

// Utility wrapper
async function runMpmCommand(args: string[]): Promise<{ stdout: string; stderr: string }> {
  return new Promise((resolve) => {
    const proc = spawn(MPM_BINARY, args, {
      cwd: MPM_WORKSPACE,
      env: { ...process.env, MPM_WORKSPACE },
    });
    let stdout = '', stderr = '';
    proc.stdout.on('data', (d) => stdout += d);
    proc.stderr.on('data', (d) => stderr += d);
    proc.on('close', () => resolve({ stdout, stderr }));
  });
}
```

### 3.2 How 808 Receives the Context

The plugin surfaces the `wake` output through the `read_directives` tool or by injecting it as a tool call result that 808 sees immediately on startup. The exact mechanism depends on OpenClaw's prompt injection capabilities:

**Option A:** Call `mpm wake` as a forced first tool invocation (like `read_directives`)

**Option B:** Parse the JSON into a context block and inject it as part of the system prompt building (same path used by identity anchoring)

The implementation will use Option A — call `mpm wake --json` as a tool call at session start — since the plugin already has this pattern for `read_directives`.

### 3.3 Session ID Tracking

Sessions are created by the `handleSessionStart` command (or equivalent). The session ID is stored in the `sessions` table and linked from `memories.session_id`. On wake-up, we find the most recent session by `created_at` and list its memories.

---

## 4. Database Changes

### 4.1 Session Table Query

```sql
-- Get most recent session
SELECT id, session_id, created_at FROM sessions ORDER BY created_at DESC LIMIT 1;

-- Get recent memories from a session
SELECT id, content, metadata, created_at FROM memories
WHERE session_id = ? ORDER BY created_at DESC LIMIT 5;
```

### 4.2 Metadata Fields

No schema migration needed. `active_mode` and `active_persona` are stored in the existing `metadata` JSON column of `memories`. Query uses `json_extract`:

```sql
SELECT id, content, created_at,
       json_extract(metadata, '$.active_mode') as active_mode,
       json_extract(metadata, '$.active_persona') as active_persona
FROM memories
WHERE session_id = ? ORDER BY created_at DESC LIMIT 5;
```

---

## 5. Files to Modify

| File | Change |
|------|--------|
| `cmd/mpm/handlers.go` | Add `handleWake()` command, add `detectActiveContext()` helper, set `activeMode`/`activePersona` before calling `AddMemory` |
| `cmd/mpm/router.go` | Register `wake` command |
| `internal/db.go` | Add `GetLastSession()` and `GetSessionMemories()` to `DatabaseManager` |
| `openclaw/mpm-plugin/src/index.ts` | Call `mpm wake --json` at agent initialization |
| `docs/MPM_WISHLIST.md` | Move "Session memory context" to In Progress |

---

## 6. Implementation Tasks

1. **Add `GetLastSession()` and `GetSessionMemories()`** to `internal/db.go`
2. **Add `handleWake`** command in `cmd/mpm/handlers.go` with human-readable and JSON output
3. **Register `wake`** command in `cmd/mpm/router.go`
4. **Add `detectActiveContext()`** helper in `cmd/mpm/handlers.go` and wire `activeMode`/`activePersona` into `AddMemory` calls
5. **Update OpenClaw plugin** (`openclaw/mpm-plugin/src/index.ts`) to call `mpm wake --json` at initialization
6. **Add tests** for `GetLastSession`, `GetSessionMemories`, and wake output format
7. **Update MPM_WISHLIST.md** — move "Session memory context" to In Progress

---

## 7. Open Questions — Resolved

| Question | Resolution |
|----------|------------|
| Active mode/persona source? | Read from `config/current_mode` and `config/current_persona` files at handler invocation time |
| Schema migration needed? | No — `active_mode`/`active_persona` stored in existing `metadata` JSON column |
| Session tracking? | Uses existing `sessions` table + `memories.session_id` FK |
| Goroutine safety? | Single-process MPM — each CLI invocation is sequential, no concurrent `AddMemory` calls |
| How does TS plugin call it? | `spawn(mpmBinary, ['wake', '--json'])` at agent initialization |

---

## Self-Review

- **Placeholder scan:** No TBD, all concrete code
- **Scope check:** Focused on Phase 1 — write context on save, read on wake. No active session monitoring.
- **No new tables:** Uses existing `sessions` and `memories` tables with `metadata` JSON column
- **Compatibility:** Existing `AddMemory` signature unchanged; metadata injected transparently