# MPM Documentation

> **MPM (Memory-Persona-Mode Manager)** — A 100% SQLite-native agent state management system for OpenClaw agents.

## 📋 Table of Contents

### Getting Started
| Document | Description |
|----------|-------------|
| **[Introduction](getting-started/introduction.md)** | Overview of MPM, purpose, key features, and architecture |
| **[Quick Start](getting-started/quick-start.md)** | Step-by-step installation and verification |

### Command Reference
| Document | Description |
|----------|-------------|
| **[Memory](commands/memory.md)** | Memory operations: list, search, add, shred |
| **[Sessions](commands/sessions.md)** | Session management: list, search, quick save |
| **[Topics](commands/topics.md)** | Topic system: create, search, members, lifecycle |
| **[Personas](commands/personas.md)** | Persona management: create, set, switch personas |
| **[Modes](commands/modes.md)** | Mode stacking: add, clear, behavioral toggles |
| **[References](commands/references.md)** | Document ingestion: PDF, EPUB, markdown |
| **[Watch](commands/watch.md)** | File watcher daemon for auto-ingestion |

### Advanced Features
| Document | Description |
|----------|-------------|
| **[FTS5 Search](advanced/ft5-search.md)** | Full-text search with SQLite `snippet()` highlighting |
| **[Shred Protocol](advanced/shred-protocol.md)** | True hard delete with `DELETE` + `VACUUM` |
| **[Native Ingestion](advanced/native-ingestion.md)** | Pure Go PDF/EPUB parsing (no Python) |
| **[Topic System](advanced/topic-system.md)** | Memory lifecycle: distillation → hardening → retrieval |
| **[Path Resolution](advanced/path-resolution.md)** | Portable installation and cascading paths |
| **[Daemon Architecture](advanced/daemon.md)** | Persistent socket daemon, structured logging |
| **[Webhook System](advanced/webhook.md)** | Real-time external notifications |

### Security
| Document | Description |
|----------|-------------|
| **[Security Model](security/security.md)** | Threat model, 17 regex patterns, GDPR compliance |

---

## Quick Navigation

| Need | Go To |
|------|-------|
| I just installed MPM | [Quick Start](getting-started/quick-start.md) |
| How do I search memories? | [Memory Commands](commands/memory.md) |
| How does FTS5 work? | [FTS5 Search](advanced/ft5-search.md) |
| I need to delete data permanently | [Shred Protocol](advanced/shred-protocol.md) |
| How do I create a persona? | [Persona Commands](commands/personas.md) |
| What are modes and how do they stack? | [Mode Commands](commands/modes.md) |
| How does the topic system work? | [Topic System](advanced/topic-system.md) |
| How does PDF parsing work? | [Native Ingestion](advanced/native-ingestion.md) |
| How do I set up portable MPM? | [Path Resolution](advanced/path-resolution.md) |
| What secrets are blocked? | [Security Model](security/security.md) |
| How do I run as a daemon? | [Daemon Architecture](advanced/daemon.md) |
| How do I get Slack/Discord alerts? | [Webhook System](advanced/webhook.md) |
| How do I auto-ingest files? | [Watch Command](commands/watch.md) |

---

## Documentation Structure

```
docs/
├── README.md                          # This file
├── MIGRATION.md                        # Upgrading from older versions
│
├── getting-started/
│   ├── introduction.md               # Overview, features, architecture
│   └── quick-start.md                 # Installation and verification
│
├── commands/
│   ├── memory.md                      # Memory operations
│   ├── sessions.md                    # Session management
│   ├── topics.md                      # Topic commands
│   ├── personas.md                    # Persona management
│   ├── modes.md                      # Mode stacking
│   └── references.md                  # Document ingestion
│
├── advanced/
│   ├── ft5-search.md                  # FTS5 implementation
│   ├── shred-protocol.md              # Hard delete protocol
│   ├── native-ingestion.md            # PDF/EPUB parsing
│   ├── topic-system.md                # Topic architecture
│   ├── path-resolution.md             # Portable paths
│   ├── daemon.md                      # Persistent socket daemon
│   └── webhook.md                     # External notifications
│
└── security/
    └── security.md                    # Threat model, blocking patterns
```

---

## Key Features at a Glance

| Feature | Benefit |
|---------|---------|
| **100% SQLite** | No external dependencies, single file database |
| **FTS5 Search** | ~1-5ms full-text queries with highlighting |
| **Shred Protocol** | Mathematically unrecoverable deletes |
| **Pure Go Parsing** | PDF/EPUB without Python or Calibre |
| **17 Pattern Blocking** | API keys and secrets never touch disk |
| **JSONL Audit Log** | Compliance-ready audit trail |
| **Topic Lifecycle** | Sessions → Topics → Memories |

---

## Maintenance

**Last Updated:** 2026-03-29  
**MPM Version:** 6.0.0

### Recent Changes (v6.0.0)
- ✅ Consolidated all documentation to `docs/` folder
- ✅ Created Getting Started guide
- ✅ Created Commands reference
- ✅ Created Advanced features documentation
- ✅ Created Security whitepaper
- ✅ Archived legacy docs in `docs/archive/`
- ✅ **Daemon Architecture** — persistent Unix socket daemon with process isolation
- ✅ **Structured JSON Logging** — thread-safe 10MB rotating log file
- ✅ **Webhook System** — async notifications to Slack/Discord/custom APIs
- ✅ **Rate Limiting** — batching when >50 events/second
- ✅ **Per-User Socket Paths** — XDG_RUNTIME_DIR → ~/.mpm → /tmp fallback

### Contribution Guidelines
When updating docs:
1. Keep headings descriptive (H2 minimum for sections)
2. Include code blocks with language tags
3. Link to related documentation
4. Update "Last Updated" date
5. Test all links point to existing files

---

**Status:** ✅ Production Ready
