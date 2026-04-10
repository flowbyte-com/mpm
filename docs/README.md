# MPM Documentation

> **MPM (Memory-Persona-Mode Manager)** — A 100% SQLite-native agent state management system for OpenClaw agents.

## Table of Contents

| Document | Description |
|----------|-------------|
| **[ARCHITECTURE.md](ARCHITECTURE.md)** | System architecture, daemon design, database schema |
| **[COMMANDS.md](COMMANDS.md)** | Full CLI command reference |
| **[GETTING_STARTED.md](GETTING_STARTED.md)** | Installation and quick start |
| **[PATH_CONFIG.md](PATH_CONFIG.md)** | Path resolution and configuration |
| **[SYNTH.md](SYNTH.md)** | Session synthesis pipeline |
| **[WATCH.md](WATCH.md)** | Watch daemon for auto-ingestion |
| **[security/security.md](security/security.md)** | Threat model and 17 regex blocking patterns |

## Quick Navigation

| Need | Go To |
|------|-------|
| How does MPM work? | [ARCHITECTURE.md](ARCHITECTURE.md) |
| How do I search memories? | [COMMANDS.md](COMMANDS.md) |
| I just installed MPM | [GETTING_STARTED.md](GETTING_STARTED.md) |
| How do I set up watch directories? | [WATCH.md](WATCH.md) |
| How does session synthesis work? | [SYNTH.md](SYNTH.md) |
| What secrets are blocked? | [security/security.md](security/security.md) |

---

## Key Features at a Glance

| Feature | Benefit |
|---------|---------|
| **100% SQLite** | No external dependencies, single file database |
| **FTS5 Search** | Full-text search with SQLite `snippet()` highlighting |
| **Shred Protocol** | True hard delete with `DELETE` + `VACUUM` |
| **Pure Go Parsing** | PDF/EPUB without Python or Calibre |
| **17 Pattern Blocking** | API keys and secrets never touch disk |
| **Watch Daemon** | fsnotify-based auto-ingestion from file system |
| **Two-Daemon Architecture** | Main daemon + independent watch daemon |

---

**Status:** Production Ready
