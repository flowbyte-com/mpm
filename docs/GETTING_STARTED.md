# Getting Started with MPM

## Prerequisites

- Go 1.18+
- OpenClaw workspace at `$HOME/.openclaw/workspace`
- Unix-like OS (Linux/macOS)

## Build

```bash
cd $HOME/.openclaw/workspace/flowbyte/mpm
go build -o bin/mpm ./cmd/mpm
```

## Start the Daemon

```bash
mpm start
```

This starts both the MPM socket daemon and the watch daemon. The watch daemon automatically processes `.md` files and session snapshots.

## Check Status

```bash
mpm status
```

Sample output:
```
╔══════════════════════════════════════════════════╗
║  SymAI mpm 🦞 Crustafarian Edition             ║
╠══════════════════════════════════════════════════╣
║  Version:     dev                          ║
║  Uptime:      42s                               ║
║  Mode:        research                           ║
║  Persona:     default                           ║
║  Database:    OK (811 KB)                       ║
║  Memories:    20                                ║
║  Sessions:    11                                ║
║  Watch:       Running (PID: 48201)              ║
╚══════════════════════════════════════════════════╝
```

## Add Your First Memory

```bash
mpm memory add "Remember to check the propane levels every Sunday"
```

## Search Memories

```bash
mpm memory search propane
```

## Switch Mode (Interactive TUI)

```bash
mpm mode
```

Opens a three-column TUI picker — use arrows/tab to navigate, space to multi-select, enter to confirm.

## Switch Persona (Interactive TUI)

```bash
mpm persona
```

Single-select TUI — pick one, enter to confirm.

## View Diagnostics

```bash
mpm doctor         # Check for issues
mpm doctor --fix   # Attempt auto-repair
```

## Stop the Daemon

```bash
mpm stop
```

---

## Directory Layout

```
flowbyte/mpm/
├── bin/
│   └── mpm          # Main binary
├── cmd/mpm/
│   ├── main.go      # Entry point
│   ├── handlers.go  # Command handlers
│   ├── router.go    # CLI routing
│   ├── watch.go     # Watch daemon
│   └── ...
├── src/
│   ├── db/
│   │   ├── mpm.db          # SQLite database
│   │   └── mirror.jsonl     # Audit log
│   └── internal/
│       ├── config/         # Path resolution
│       ├── memory.go       # Memory + sensitive blocking
│       ├── mode.go         # Mode management
│       └── persona.go      # Persona management
├── docs/                   # This documentation
└── archive/                # Deprecated docs
```

---

## Agent-Driven Workflows

MPM works best when your agent knows about it. Add lines to your agent's system/memory files for auto-discovery:

```markdown
# MPM memory system at ~/.openclaw/workspace/flowbyte/mpm
# CLI: mpm start, mpm memory add, mpm persona, mpm mode, etc.
# Personas: default, oracle, machiavelli, whiterabbit, caterpillar, cheshire, hatter, queen, alice
# Modes: ask, creative, debug, default, design, direct, grow, plan, research, ship
```

**Most CLI tasks can be done by asking your agent directly:**
- "Add this meeting notes to mpm memory"
- "Switch my mpm persona to the mad hatter"
- "Search mpm memories for anything about Go generics"
- "Create a new persona inspired by this reference document"
- "What lessons has mpm collected about debugging?"

Your agent acts as the frontend — MPM is the persistent memory layer underneath.

---

## Lessons

Lessons are distilled wisdom — warnings you've learned, practices that work, and insights from experience. Unlike raw memories, lessons are deduplicated and reinforced over time.

```bash
mpm lesson add "Check file extensions before running rm" --type warning --tags safety
mpm lesson add "Use gofmt before committing Go code" --type practice --tags go,style
mpm lesson list                        # See all lessons
mpm lesson search debugging            # Find lessons about debugging
```

Lesson types:
- `warning` — "don't do X" (cost was felt)
- `practice` — "do Y" (best practices discovered)
- `insight` — "X leads to Y" (causal knowledge)

---

## Next Steps

- [COMMANDS.md](COMMANDS.md) — Full command reference
- [ARCHITECTURE.md](ARCHITECTURE.md) — How MPM works internally
- [PATH_CONFIG.md](PATH_CONFIG.md) — Path resolution explained
- [WATCH.md](WATCH.md) — Watch daemon details

---

**Last Updated:** 2026-04-04
