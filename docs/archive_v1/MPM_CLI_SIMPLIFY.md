# TASK: Implement the CLI Simplification — Invisible CLI + ops Junk Drawer

We are restructuring the MPM CLI to have a tiny, intuitive daily surface area while keeping full power accessible through `ops`. After this change, daily use should feel like using a notepad, not a database.

---

## 1. Stdin Pipe Detection

**Rule:** If stdin has data and no subcommand is given, treat it as an add operation.

- In `main.go`, before routing: check `os.Stdin.Stat()` for incoming data.
- If stdin is readable AND `os.Args` has only 1 token (just `mpm` itself):
  - Read stdin content.
  - Show preview: `"Will save: <first 120 chars>..."`
  - Prompt `[y/N]` — if `yes` → proceed with `AddMemory` pipeline (auto-synthesis fires).
  - If `N` or error → exit cleanly.
- If no piped data → proceed normally.
- If more than 1 arg → ignore stdin (user is calling a specific command).
- Empty stdin → skip silently, route normally.

---

## 2. Root Default-to-Recall

**Rule:** A bare string at position 1 (not a flag) routes to recall.

- In `main.go`, after stdin check but before routing:
  - If `len(os.Args) == 2` (command name + one bare positional string):
    - Check it does NOT look like a flag (`os.Args[1][0] != '-'`).
    - If true: rewrite `os.Args` to `["mpm", "recall", "<string>"]` before routing.
- Example: `mpm token budget` → `mpm recall token budget`

**Constraint:** `--token-budget`, `-i`, `--json` and other flags bypass this rewrite. Only a bare single string triggers it.

---

## 3. `ops` Junk Drawer — Group Engine Room Under One Command

**Goal:** Daily `--help` shows only what you need. Everything powerful but rare is under `ops`.

### Root commands after simplification (7 total):
```
mpm <query>       recall — search memories (default-to-recall above)
mpm add <text>    add a memory
mpm add -i        interactive add via $EDITOR
mpm snooze <id>   bump a memory's relevance
mpm ls             list memories
mpm show <id>      show one memory
mpm rm <id>        delete a memory
mpm ops            engine room (see below)
mpm help           full help
```

### `mpm ops` subcommands:
```
mpm ops doctor [--explain]       diagnostics
mpm ops maintain                self-maintenance (decay, consolidate, prune)
mpm ops synthesize [--dry-run]    LLM synthesis on all memories
mpm ops gc [--dry-run/--review/--purge]  memory decay sweep
mpm ops watch                   start/stop/status watcher daemon
mpm ops web                     start web UI server
mpm ops review                  spaced reinforcement review
mpm ops stats                   memory statistics
mpm ops prune                   prune expired memories
mpm ops export                  export memories to JSON
mpm ops backup [path]           database backup
mpm ops restore-db <path>        restore from .sql dump
mpm ops ingest                  import from external SQLite
mpm ops switch                  interactive persona/mode switcher
mpm ops directives              show behavioral directives
mpm ops mode                    mode operations
mpm ops persona                persona operations
mpm ops topic                   topic operations
mpm ops lesson                   lesson operations
mpm ops session                  session operations
mpm ops memory                  memory operations
mpm ops reference               reference library
mpm ops wake                    show last session context
mpm ops gateway                 gateway control
mpm ops help                    ops subcommand help
```

### Implementation:

**Router changes (`cmd/mpm/router.go`):**
- Add `ops` case in root switch — it is a parent that calls a sub-router.
- The sub-router reads `os.Args[2]` (first arg after `ops`) and dispatches to the appropriate handler.
- `mpm ops help` prints a list of all ops subcommands.
- **Backwards compatibility:** Keep all existing root command aliases (`doctor`, `maintain`, `synthesize`, `gc`, `watch`, `web`, `review`, `stats`, `prune`, `export`, `backup`, `restore-db`, `ingest`, `switch`, `directives`, `mode`, `persona`, `topic`, `lesson`, `session`, `memory`, `reference`, `wake`, `gateway`) working at the root. Old scripts must not break.
- Help text for each moved command: "Available via `mpm ops <command>`" note.

**New `ops` router (`cmd/mpm/ops.go` or inline in router.go):**
```go
opsSubcommands := map[string]func([]string) int{
    "doctor":      runDoctorCommand,
    "maintain":    runMaintainCommand,
    "synthesize":  handleSynthesize,
    "gc":          handleGC,
    "watch":       handleWatch,
    "web":         handleWeb,
    "review":      handleReview,
    "stats":       handleStats,
    "prune":       handlePrune,
    "export":      handleExport,
    "backup":      handleBackup,
    "restore-db":  handleRestoreDB,
    "ingest":     handleIngest,
    "switch":     handleSwitch,
    "directives": handleDirectives,
    "mode":       handleMode,
    "persona":    handlePersona,
    "topic":      handleTopic,
    "lesson":     handleLesson,
    "session":    handleSession,
    "memory":     handleMemory,
    "reference":  handleReference,
    "wake":       handleWake,
    "gateway":   handleGateway,
    "help":       runOpsHelp,
}
```

---

## 4. Updated Root Help Text

After restructuring, `mpm help` should show only:

```
mpm · Memory Persistence Module

Usage: mpm <query|text> [flags]

Daily Commands:
  mpm <query>       Search memories (default when called with a bare string)
  mpm add <text>    Add a new memory
  mpm add -i        Interactive add — opens $EDITOR
  mpm snooze <id>   Bump a memory's relevance (no LTM promotion)
  mpm ls            List memories
  mpm show <id>     Show memory details
  mpm rm <id>       Delete a memory
  mpm help          Show this help

Engine Room (ops):
  mpm ops           Run maintenance, diagnostics, synthesis, and more
  mpm ops help       List all ops subcommands

Also available:
  mpm ops mode | persona | topic | lesson | session | reference | wake
```

---

## Files to Modify

- `cmd/mpm/main.go` — stdin detection + default-to-recall rewrite
- `cmd/mpm/router.go` — add `ops` parent + sub-router + update root help template
- `cmd/mpm/help.go` (or wherever help text is defined) — new compact help template

---

## Codebase Rigidity Rules

- All existing root commands must remain functional at root level (backwards compatibility).
- `mpm doctor`, `mpm maintain`, `mpm synthesize` etc. must all still work.
- Stdin pipe detection must not interfere with `mpm recall --json` (which uses a pipe internally).
- No new database writes — only routing and help text changes.
- All tests must pass.

---

## Verification

- `mpm help` — shows 7 daily commands + ops reference only
- `mpm token budget` → routes to `mpm recall token budget`
- `cat idea.md | mpm` → shows preview, saves on `y`
- `mpm ops help` → lists all ops subcommands
- `mpm ops doctor --explain` → runs EXPLAIN QUERY PLAN
- `mpm doctor` still works (backwards compatible)
- All tests pass