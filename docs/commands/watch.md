# Watch Command

> **File watcher and ingestion daemon.** Automatically processes memory files and session snapshots.

## Overview

The `watch` command starts a persistent daemon that monitors memory and session directories, automatically ingesting new files into MPM's SQLite database.

## Architecture

```
┌─────────────────────────────────────────────────────────────────┐
│                    Watch Daemon                                  │
│                                                                 │
│  ┌─────────────────────────────────────────────────────────────┐│
│  │  Startup Sweep                                              ││
│  │  • Process all existing .md files as LTM memories           ││
│  │  • Process .jsonl files (only if no .lock exists)          ││
│  │  • Delete processed files after successful ingestion       ││
│  └─────────────────────────────────────────────────────────────┘│
│                            │                                    │
│                            ▼                                    │
│  ┌─────────────────────────────────────────────────────────────┐│
│  │  fsnotify Event Loop                                        ││
│  │                                                             ││
│  │  Route A (.md files):                                       ││
│  │    → Create/Write event                                     ││
│  │    → Read → Sanitize → Ingest as LTM → Delete file         ││
│  │                                                             ││
│  │  Route B (.lock files):                                     ││
│  │    → Remove event (lock deleted = session complete)         ││
│  │    → Process matching .jsonl → Extract facts → Delete        ││
│  └─────────────────────────────────────────────────────────────┘│
│                            │                                    │
│                            ▼                                    │
│  ┌─────────────────────────────────────────────────────────────┐│
│  │  Data Promotion Pipeline                                    ││
│  │  • Keyword extraction (markdown tags + TF-IDF)             ││
│  │  • Hash-based embeddings (SHA256)                           ││
│  │  • LTM storage with weight=10                               ││
│  │  • JSONL mirror update                                      ││
│  └─────────────────────────────────────────────────────────────┘│
└─────────────────────────────────────────────────────────────────┘
```

## CLI Command

```bash
mpm watch [options]
```

## Options

| Option | Description | Default |
|--------|-------------|---------|
| `--dir <path>` | Watch directory for memory files | `memory_dir` from config |
| `--sessions <path>` | Watch directory for session files | `sessions_dir` from config |
| `--dry-run` | Process files but don't delete them | `false` |
| `--once` | Run startup sweep only, then exit | `false` |
| `--v` | Verbose output | `false` |

## Processing Routes

### Route A: Markdown Memory Files (.md)

Triggered by `Create` or `Write` fsnotify events on `.md` files.

**Pipeline:**
1. Read file content
2. Sanitize with regex (remove sensitive patterns)
3. Check against `toxicphrases.txt` firewall
4. Extract keywords/tags (markdown `#tag` + TF-IDF)
5. Generate SHA256 hash-based embedding
6. Ingest as LTM memory with `weight=10`
7. Update JSONL mirror
8. Delete original `.md` file

### Route B: Session JSONL Files (.jsonl)

Triggered by `Remove` fsnotify events on `.lock` files (lock deletion = session complete).

**Prerequisite:** Corresponding `.lock` file must not exist.

**Pipeline:**
1. Derive `.jsonl` path from `.lock` path
2. Parse JSONL session file
3. Extract facts (structured memories from session)
4. Check against firewall
5. Tag with `session-fact`
6. Ingest as LTM memory with `weight=10`
7. Update JSONL mirror
8. Delete original `.jsonl` file

## Keywords Extraction

The watch daemon extracts keywords using two methods:

1. **Markdown tags** — `#tag` patterns in content
2. **TF-IDF scoring** — Top 5 meaningful terms (filtered stopwords)

Tags are stored as `["tag1", "tag2", ...]` in the `tags` JSON column.

## File Naming Conventions

| Pattern | Behavior |
|---------|----------|
| `*.md` | Process as LTM memory |
| `*.jsonl` | Process as session facts (only if no `.lock` exists) |
| `*conflicted*` | Skip (ignore conflict markers) |
| `*.lock` | Trigger for matching `.jsonl` processing |

## Examples

### Start Watch Daemon
```bash
mpm watch
```

### Verbose Mode
```bash
mpm watch --v
```

### Dry Run (Process but Don't Delete)
```bash
mpm watch --dry-run --v
```

### Watch Custom Directories
```bash
mpm watch --dir /path/to/memory --sessions /path/to/sessions
```

### One-Shot Sweep
```bash
mpm watch --once --v
```

### Combined with MPM Daemon

```bash
# Terminal 1: Start MPM daemon
mpm daemon

# Terminal 2: Start watch daemon
mpm watch --v
```

## Interaction with MPM Daemon

The watch daemon operates independently of the MPM socket daemon:

| Daemon | Purpose | Lives in |
|--------|---------|---------|
| `mpm` (socket) | CLI commands, state management | Memory |
| `mpm watch` | File ingestion, auto-processing | Files |

They share the same SQLite database but communicate through different channels:
- **Socket daemon**: Unix socket `/run/user/uid/mpm.sock`
- **Watch daemon**: Filesystem events + direct DB writes

## Exit Codes

| Code | Meaning |
|------|---------|
| 0 | Success |
| 1 | Configuration error or initialization failure |

## Troubleshooting

### "No directories to watch"
```bash
# Check config
mpm config show

# Ensure memory_dir and sessions_dir are set
cat /home/v/.openclaw/workspace/projects/mpm/mpm_config.json
```

### Files not being processed
1. Check verbose mode: `mpm watch --v`
2. Verify file permissions (must be writable)
3. Check for `.lock` files blocking `.jsonl` processing
4. Review JSONL mirror: `tail -20 /home/v/.openclaw/workspace/projects/mpm/src/db/mirror.jsonl`

### "Sensitive content blocked"
- Content matched a blocked pattern (API key, password, etc.)
- Sanitize content before it reaches the watch directory
- See [Security Model](../security/security.md) for blocked patterns

---

**Last Updated:** 2026-03-30  
**MPM Version:** 6.0.1
