# MPM — Memory-Persona-Mode Manager

SQLite-native agent state management for AI agents. Single binary, zero external deps.

## Quick Start

```bash
make build                          # Build bin/mpm
./bin/mpm add "Remember this fact"  # Add memory
./bin/mpm ls                        # List memories
./bin/mpm recall sqlite             # Search via FTS5
```

## CLI

All commands execute in-process — no daemon needed:

| Command | Description |
|---------|-------------|
| `mpm add <content>` | Add memory (`--collection`, `--tag`, `--weight`, `--ttl`) |
| `mpm ls` | List memories (`--collection`, `--tag`, `--since`, `--until`, `--limit`) |
| `mpm show <id>` | Show memory details |
| `mpm rm <id>` | Soft delete |
| `mpm recall <query>` | Search with FTS5 + LIKE fallback (`--since`, `--until`) |
| `mpm shred <id>` | Secure delete (DELETE + VACUUM) |
| `mpm promote <id>` | Promote to LTM (weight=10) |
| `mpm reinforce <id> [n]` | Increment reinforcement |
| `mpm weaken <id> [n]` | Decrement reinforcement |
| `mpm set-weight <id> <0-100>` | Set weight directly |
| `mpm stats` | Memory statistics dashboard |
| `mpm prune` | Prune (`--older-than 90d`, `--never-accessed`) |
| `mpm export` | Export to JSON/CSV (`--format`, `--collection`, `--since`) |
| `mpm maintain` | Self-maintenance (decay, consolidate, prune) |
| `mpm reference add/list/search/get/shred` | Reference library |
| `mpm topic create/add/remove/list/show/rm` | Topic management |
| `mpm ingest <path>` | Import from external SQLite |
| `mpm doctor` | Diagnostics |
| `mpm web` | Start web UI server |
| `mpm mode [list\|active\|add\|remove\|clear]` | Multi-select behavioral modes |
| `mpm persona [list\|active\|set\|clear]` | Single-select identity |
| `mpm prime-directives` | Show 808 directives |
| `mpm session [add\|search\|show\|list]` | Session operations |
| `mpm lesson [add\|list\|search\|get\|shred\|stats]` | Lesson operations |
| `mpm memory [add\|search\|show\|shred\|list]` | Legacy memory ops |
| `mpm synthesize <uuid>` | LLM session synthesis |
| `mpm dashboard` | Real-time TUI dashboard |
| `mpm menu` | Interactive mode/persona picker |
| `mpm watch [start\|stop\|restart\|status]` | File watcher lifecycle |

## Architecture

```
CLI ──> Router ──> Handler (in-process) ──> SQLite (mpm.db)
                         │
                         └──> WorkerPool (goroutine pool)
                               ├── fsnotify watcher ──> .md → LTM memory
                               ├── .lock removed → session facts
                               └── external DB polling
```

Unified single process — all commands execute in-process with no daemon subprocess or Unix socket IPC. The file watcher runs as a background goroutine, submitting events to a `WorkerPool` via a buffered channel.

## Memory Model

Everything is a **memory**. Collections distinguish types via the `collection` field:

| Collection | Purpose |
|------------|---------|
| `memories` | General facts with reinforcement tracking |
| `session` | Session-derived facts |
| `lessons` | Learned wisdom (warning/practice/insight) |

**Metadata fields**: `weight` (1-100), `reinforcement_count`, `is_long_term` (weight >= 10), `expires_at` (TTL), `last_accessed_at` (recency).

**Relevance scoring**: `(reinforcement_count * 2) + (weight * 1.5) + recency_bonus`

## Modes & Personas

Modes define *how* to work (multi-select). Personas define *who* the agent is (single-select). Both are JSON files in `mode/` and `persona/` directories, tracked via `active.json`.

## Database

**Location**: `src/db/mpm.db` (SQLite with FTS5)
**Key tables**: `memories`, `sessions`, `topics`, `topic_memberships`, `modes`, `personas`, `reference_docs`, `reference_chunks`, `lessons`, `system_config`, `raw_memories`, `external_db_cursors`
**FTS5 indexes**: `memories_fts`, `sessions_fts`, `topics_fts`, `references_fts`, `lessons_fts` — auto-synced via INSERT/UPDATE/DELETE triggers.

**Query strategy**: FTS5 MATCH → LIKE fallback → recent rows.

## Security

All content is scanned against **17 regex patterns** before any write. Blocked patterns include API keys (OpenAI, GitHub, AWS, Stripe, Slack), JWTs, private keys, SSH keys, database URLs, `password=`/`secret=` patterns, bearer tokens. Blocked content is logged to `mirror.jsonl` but never stored.

Toxic phrase detection (prompt injection) via `toxicphrases.txt` — case-insensitive substring match, cached after first load.

## Path Resolution

No hardcoded paths. Cascade: `MPM_WORKSPACE` env var → executable-relative → CWD.

| Path | Default |
|------|---------|
| Database | `$MPM_WORKSPACE/src/db/mpm.db` |
| Mirror log | `$MPM_WORKSPACE/src/db/mirror.jsonl` |
| Modes | `$MPM_WORKSPACE/mode/` |
| Personas | `$MPM_WORKSPACE/persona/` |

## Build

```bash
make build    # bin/mpm (requires CGO for SQLite FTS5)
make test     # go test -tags fts5 ./...
make install  # sudo install to /usr/local/bin/mpm
```

Requires Go 1.18+ and CGO. Dependencies: `mattn/go-sqlite3`, `fsnotify`, `bubbletea`, `lipgloss`, `pdf`, `golang.org/x/net`.

## Configuration (`mpm_config.json`)

```json
{
  "memory_dirs": ["/path/to/watch"],
  "sessions_dirs": ["/path/to/sessions"],
  "external_dbs": [{"path": "...", "label": "openclaw", "interval_seconds": 30}],
  "synth": {"model": "MiniMax-M2.7", "api_key": "", "base_url": ""}
}
```

## Web UI

`mpm web` serves SPA at port 18792 with token auth (`web_token` in config).

## File Watcher

Auto-started via `mpm watch start`. Monitors `.md` files (ingested as LTM memories), session `.lock` removal (triggers fact extraction), and external SQLite databases (polls for new rows on configurable intervals). Runs as a background goroutine — no separate subprocess.

## License

GNU AGPL v3. Part of the OpenClaw agent ecosystem.

Created by v (human) + Great_808 (AI).