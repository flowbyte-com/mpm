# MPM Documentation

> **MPM (Memory-Persona-Mode Manager)** — A 100% SQLite-native agent state management system. Everything is a memory — the CLI is intentionally simple.

## Simplified CLI Philosophy

**Everything is a memory.** Sessions, topics, lessons are just memories with different collections/metadata. This means:
- ~10 simple commands instead of 30+ nested subcommands
- No need for `mpm session list` — just `mpm ls --collection session`
- Memory importance via reinforcement learning, not separate "lesson" systems

## Table of Contents

| Document | Description |
|----------|-------------|
| **[INSTALL.md](../INSTALL.md)** | Installation, build, shell setup, OpenClaw integration |
| **[GETTING_STARTED.md](GETTING_STARTED.md)** | First-time setup, daemon management |
| **[ARCHITECTURE.md](ARCHITECTURE.md)** | System architecture, daemon design, database schema |
| **[COMMANDS.md](COMMANDS.md)** | Full CLI command reference |
| **[PATH_CONFIG.md](PATH_CONFIG.md)** | Path resolution and MPM_WORKSPACE |
| **[SYNTH.md](SYNTH.md)** | Session synthesis pipeline |
| **[WATCH.md](WATCH.md)** | Watch daemon for auto-ingestion |
| **[security/security.md](security/security.md)** | Threat model and 17 regex blocking patterns |
| **[mode/README.md](../mode/README.md)** | Creating and using modes |
| **[persona/README.md](../persona/README.md)** | Creating and using personas |

## Quick Navigation

| Need | Go To |
|------|-------|
| How do I install MPM? | [INSTALL.md](../INSTALL.md) |
| How do I get started? | [GETTING_STARTED.md](GETTING_STARTED.md) |
| How does MPM work? | [ARCHITECTURE.md](ARCHITECTURE.md) |
| How do I search memories? | [COMMANDS.md](COMMANDS.md) |
| How do I set up watch directories? | [WATCH.md](WATCH.md) |
| How does session synthesis work? | [SYNTH.md](SYNTH.md) |
| What secrets are blocked? | [security/security.md](security/security.md) |

## Key Features at a Glance

| Feature | Benefit |
|---------|---------|
| **100% SQLite** | No external dependencies, single file database |
| **Simplified CLI** | ~10 commands instead of 30+ nested subcommands |
| **Everything is Memory** | Sessions, topics, lessons unified under one model |
| **Reinforcement Learning** | Memories improve with use via reinforce/weaken |
| **FTS5 Search** | Full-text search with SQLite `snippet()` highlighting |
| **Shred Protocol** | True hard delete with `DELETE` + `VACUUM` |
| **Pure Go Parsing** | PDF/EPUB without external tools |
| **17 Pattern Blocking** | API keys and secrets never touch disk |
| **Watch Daemon** | fsnotify-based auto-ingestion from filesystem |
| **Two-Daemon Architecture** | Main daemon + independent watch daemon |
| **External DB Polling** | Sync memories from OpenClaw or other SQLite sources |

## Status

**Production Ready** — MPM is actively used as the memory layer for OpenClaw agents.
