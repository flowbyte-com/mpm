# MPM Introduction

**Version:** 6.0.0  
**Date:** 2026-03-28  
**Status:** Production Ready

## What is MPM?

MPM (Memory-Persona-Mode Manager) is a **100% SQLite-native** agent state management system for OpenClaw agents. It provides persistent memory, behavioral templates, and lightweight toggles for AI assistants — all without external databases.

**Target Audience:**
- **OpenClaw developers** building AI agent systems
- **AI agent builders** who need persistent, searchable memory
- **Privacy-conscious users** requiring local-only data storage
- **Compliance teams** needing GDPR/CCPA-ready data erasure

**Typical Use Cases:**
- Long-term memory for AI assistants (learns from past sessions)
- Behavioral personas (switch between "developer", "support", "researcher")
- Topic-based knowledge curation (distill sessions → topics → memories)
- Document ingestion and search (PDF/EPUB without external tools)
- Secure secret blocking (prevents API keys from being stored)

## Key Features

| Feature | Status | Description |
|---------|--------|-------------|
| SQLite-only | ✅ | No ChromaDB, no external vector database |
| FTS5 Search | ✅ | Full-text search with `snippet()` highlighting |
| Shred Protocol | ✅ | Hard delete with `DELETE` + `VACUUM` |
| Native PDF/EPUB | ✅ | Pure Go parsing, no Python dependencies |
| Sensitive Blocking | ✅ | 17 regex patterns block secrets/tokens |
| Audit Logging | ✅ | JSONL mirror with RFC3339 timestamps |
| Vector Search | ✅ | SQLite L2 distance fallback + FTS5 |

### Why These Matter

**SQLite-only:** Simplicity and portability. One file, zero external dependencies, runs anywhere Go compiles.

**FTS5 Search:** SQLite's built-in full-text search provides ~1-5ms query times with BM25 ranking and highlighted snippets — no separate search service needed.

**Shred Protocol:** Standard "deletes" in databases often leave data recoverable. MPM's `DELETE` + `VACUUM` ensures data is mathematically unrecoverable — critical for GDPR compliance.

**Native Parsing:** Using pure Go (`github.com/ledongthuc/pdf`) means no Python runtime, no Calibre dependency, and faster parsing (~1.5s for 10MB PDF).

**Sensitive Blocking:** AI agents often receive API keys and tokens. MPM blocks 17 patterns (OpenAI keys, GitHub PATs, AWS credentials, etc.) before they can touch disk.

## Architecture Overview

```
┌─────────────────────────────────────────────────────────────┐
│ MPM CLI (mpm)                                               │
│ • status, dashboard, search, topic, session, persona, mode │
└───────────────────┬─────────────────────────────────────────┘
                    │
                    ▼
┌─────────────────────────────────────────────────────────────┐
│ SQLite Database (mpm.db)                             │
│ • FTS5 virtual tables (memories_fts, sessions_fts)         │
│ • Vector search (L2 distance fallback)                      │
│ • All schemas consolidated into single DB                   │
└───────────────────┬─────────────────────────────────────────┘
                    │
                    ▼
┌─────────────────────────────────────────────────────────────┐
│ OpenClaw Gateway                                            │
│ • State persistence                                         │
│ • Provider routing                                          │
└─────────────────────────────────────────────────────────────┘
```

> 📊 For a detailed architecture breakdown, see [**Overview**](advanced/overview.md)

## Database Schema

### Core Tables

| Table | Purpose | Key Columns |
|-------|---------|-------------|
| `memories` | Semantic knowledge | id, collection, content, embedding, tags |
| `sessions` | Temporary history | id, content, created_at, completed_at |
| `topics` | Curated knowledge | id, title, description, source_type |
| `topic_members` | Many-to-many mapping | topic_id, memory_id |

### FTS5 Virtual Tables

| Table | Purpose |
|-------|---------|
| `memories_fts` | Full-text search on memories |
| `sessions_fts` | Full-text search on sessions |
| `topics_fts` | Full-text search on topics |

> 📖 For FTS5 implementation details, see [**FTS5 Search Guide**](advanced/ft5-search.md)

## CLI Command Categories

### Core Commands
```bash
mpm status          # Full dashboard
mpm dashboard       # Interactive terminal UI
mpm help            # List all commands
```

### Memory Management
```bash
mpm memory list     # List all memories
mpm memory search   # FTS5 search with highlighting
mpm memory add      # Add new memory
mpm memory shred    # Hard delete with VACUUM
```

### Session Management
```bash
mpm session list    # List sessions
mpm session search  # FTS5 search
mpm session add     # Add session
mpm session shred   # Hard delete
mpm ss              # Quick save current session
```

### Persona Management
```bash
mpm persona list    # List all personas
mpm persona set     # Set active persona
mpm persona create  # Create new persona
mpm persona delete  # Remove persona
```

### Mode Management
```bash
mpm mode list       # List all modes
mpm mode add        # Add mode to stack
mpm mode active     # Show active modes
mpm mode clear      # Remove all modes
```

### Topic Management
```bash
mpm topic list      # List topics
mpm topic add       # Create topic
mpm topic search    # FTS5 search
mpm topic members   # List topic members
mpm topic shred     # Hard delete with cascade
```

### Reference Library
```bash
mpm reference list  # List documents
mpm reference add   # Add document (PDF/EPUB/TXT/MD)
mpm reference search # FTS5 search
mpm reference get   # Retrieve full document
mpm reference shred # Hard delete
```

## Quick Start

> 🚀 New to MPM? Start here! For detailed installation, see [**Quick Start Guide**](getting-started/quick-start.md)

### 1. Verify Installation
```bash
mpm config show     # Check paths are correct
mpm status          # Full dashboard
```

### 2. Select Active Persona
```bash
mpm persona set default
```

### 3. Add Modes
```bash
mpm mode add strategy
mpm mode add concise
```

### 4. Test Memory Search
```bash
mpm memory search "test"
```

## Configuration

### Config File Location
```
$HOME/.openclaw/workspace/flowbyte/mpm/mpm_config.json
```

### Default Paths
- **Database:** `~/.openclaw/workspace/memory/mpm.db`
- **Memory:** `~/.openclaw/workspace/memory/`
- **Sessions:** `~/.openclaw/agents/main/sessions/`

### Environment Variables
```bash
export MPM_WORKSPACE="$HOME/.openclaw/workspace"
export MPM_MEMORY_DIR="/custom/memory/path"
export MPM_SESSIONS_DIR="/custom/sessions/path"
```

> 🔧 For portable installation strategies, see [**Path Resolution Guide**](advanced/path-resolution.md)

## Version Information

### Current Version: 6.0.0 (2026-03-27)

**Major Changes:**
- ✅ Single SQLite database (mpm.db)
- ✅ FTS5 search with `snippet()` highlighting
- ✅ Shred protocol for hard deletes
- ✅ Native PDF/EPUB parsing
- ✅ Complete topic system
- ✅ Sensitive content blocking (17 patterns)
- ✅ Audit logging (JSONL mirror)

## Security Model

### Sensitive Content Blocking

MPM blocks **17 regex patterns** before any content touches storage:

| Category | Patterns Blocked |
|----------|------------------|
| API Keys | OpenAI, GitHub PAT, GitHub OAuth, GitHub Refresh, AWS, Stripe |
| Credentials | Passwords, Secrets, Database connection strings |
| Tokens | JWT, Bearer tokens |
| Keys | Private keys (RSA), SSH keys |

> 🔒 For full security documentation, see [**Security Model**](security/security.md)

### File Permissions
- **Directories:** 0700 (owner only)
- **Files:** 0600 (owner only)

### Audit Trail
- **JSONL mirror:** `~/.openclaw/workspace/memory/mirror.jsonl`
- **All blocked attempts** logged with timestamp and reason

## Next Steps

1. **[Quick Start](getting-started/quick-start.md)** — Installation and verification (start here!)
2. **[CLI Commands](commands/)** — Full command reference
3. **[FTS5 Search](advanced/ft5-search.md)** — Full-text search details
4. **[Shred Protocol](advanced/shred-protocol.md)** — Hard delete for compliance
5. **[Topic System](advanced/topic-system.md)** — Knowledge curation lifecycle
6. **[Security](security/security.md)** — Sensitive content blocking

## Support

- **Main Documentation:** `~/.openclaw/workspace/flowbyte/mpm/docs/`
- **Source Code:** `~/.openclaw/workspace/flowbyte/mpm/`
- **Database:** `~/.openclaw/workspace/flowbyte/mpm/src/db/`

---

**Last Updated:** 2026-03-29  
**Status:** Production Ready 🚀
