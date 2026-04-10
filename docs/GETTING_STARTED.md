# Getting Started with MPM

MPM (Memory-Persona-Mode Manager) — SQLite-native agent state management.

## Prerequisites

- Go 1.18+
- Unix-like OS (Linux/macOS)
- ~50MB disk space

## Build

```bash
cd ~/.openclaw/workspace/flowbyte/mpm
make build
```

This creates `bin/mpm`. Run it directly with `./bin/mpm` or add it to your PATH.

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
║  Version:     dev                               ║
║  Uptime:      42s                               ║
║  Mode:        research                          ║
║  Persona:     default                           ║
║  Database:    OK (811 KB)                       ║
║  Memories:    20                               ║
║  Sessions:    11                               ║
║  Watch:       Running (PID: 48201)              ║
╚══════════════════════════════════════════════════╝
```

## Add Your First Memory

```bash
mpm memory add "Remember to check the propane levels every Sunday"
```

## Search Memories

```bash
mpm recall propane
```

## Switch Mode

```bash
mpm mode
```

Opens a three-column TUI picker — use arrows/tab to navigate, space to multi-select, enter to confirm.

## Switch Persona

```bash
mpm persona
```

Single-select TUI — pick one, enter to confirm.

## Add a Lesson

Lessons capture hard-won wisdom:

```bash
mpm lesson add "Check file extensions before running rm" --type warning --tags safety
mpm lesson add "Use gofmt before committing Go code" --type practice --tags go,style
```

Lesson types:
- `warning` — "don't do X" (negative lessons, cost was felt)
- `practice` — "do Y" (positive lessons, best practices discovered)
- `insight` — "X leads to Y" (causal knowledge, default)

List and search lessons:
```bash
mpm lesson list
mpm lesson search debugging
```

## View Diagnostics

```bash
mpm doctor         # Check for issues
mpm doctor --fix  # Attempt auto-repair
```

## Stop the Daemon

```bash
mpm stop
```

## Directory Layout

```
~/mpm/
├── bin/mpm                    # Compiled binary
├── src/db/
│   ├── mpm.db                # SQLite database
│   └── mirror.jsonl          # Audit log
├── mode/                     # Mode configurations (JSON)
├── persona/                  # Persona configurations (Markdown)
├── docs/                     # This documentation
├── internal/                 # Source code
└── mpm_config.json          # Configuration
```

## Configuration

Edit `mpm_config.json` to configure watch directories and external databases:

```json
{
  "memory_dirs": [],
  "sessions_dirs": [],
  "external_dbs": [
    {
      "path": "$HOME/.openclaw/memory/main.sqlite",
      "label": "openclaw",
      "interval_seconds": 30
    }
  ],
  "synth": {
    "model": "MiniMax-M2.7",
    "api_key": "",
    "base_url": ""
  }
}
```

See [WATCH.md](WATCH.md) for details on auto-ingestion and [SYNTH.md](SYNTH.md) for synthesis configuration.

## Next Steps

- [COMMANDS.md](COMMANDS.md) — Full command reference
- [ARCHITECTURE.md](ARCHITECTURE.md) — How MPM works internally
- [PATH_CONFIG.md](PATH_CONFIG.md) — Path resolution explained
- [WATCH.md](WATCH.md) — Watch daemon details
- [INSTALL.md](../INSTALL.md) — System-wide installation

---

**Last Updated:** 2026-04-10
