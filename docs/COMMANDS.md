# MPM Command Reference

Conventions: `mpm <cmd>` = standalone (no daemon). `*` = requires `mpm start`.

## Core Memory (standalone)

```
mpm add <content> [--collection <name>] [--tag <a,b>] [--weight <1-100>] [--ttl <7d>] [--session <id>]
mpm ls [--collection <name>] [--tag <t>] [--since YYYY-MM-DD] [--until YYYY-MM-DD] [--limit <n>]
mpm show <id>
mpm rm <id>
mpm recall <query> [--since YYYY-MM-DD] [--until YYYY-MM-DD] [--limit <n>]
mpm shred <id>            # DELETE (hard delete, FTS5 triggers sync); VACUUM deferred to `mpm maintain`
```

## Memory Importance (standalone)

```
mpm promote <id>          # weight=10, is_long_term=true, clear TTL
mpm reinforce <id> [delta] # default +1
mpm weaken <id> [delta]    # default -1
mpm set-weight <id> <0-100>
```

## Stats & Maintenance (standalone)

```
mpm stats                  # Total/active/LTM counts, tag distribution, reinforcement
mpm prune [--older-than 90d] [--never-accessed]
mpm export [--format json|csv] [--collection <name>] [--since YYYY-MM-DD] [--until YYYY-MM-DD] [--output <file>]
mpm maintain [--review] [--days <n>]  # weight decay, consolidation, spaced reinforcement
```

## Mode / Persona (require daemon)

```
mpm mode                   # Interactive TUI (multi-select)
mpm mode list|active|add <name>|remove <name>|clear
mpm persona                # Interactive TUI (single-select)
mpm persona list|active|set <name>|clear
mpm prime-directives
```

## Session / Lesson (require daemon)

```
mpm session list|show <id>|search <query>|add <content>|shred <id>
mpm lesson add <content> [--type warning|practice|insight] [--tags <a,b>]
mpm lesson list|search <query>|get <id>|shred <id>|stats
```

## Topic Management (standalone)

```
mpm topic create [--today|--yesterday|--from D --to D] [--desc <text>]
mpm topic add <memory-id> <topic-name>
mpm topic remove <memory-id> <topic-name>
mpm topic list|show <name>|rm <name>
```

## Reference Library (standalone)

```
mpm reference add <file>   # PDF, EPUB, .md, .txt
mpm reference list|search <query>|get <id>|shred <id>|scan
```

## Synthesis (require daemon)

```
mpm synthesize <session-uuid>
```

Reads session JSONL → LLM extracts facts, topics, summary → stores as memories. Config in `mpm_config.json` `[synth]` section.

Supported providers: MiniMax, OpenAI, Ollama, LM Studio (any OpenAI-compatible API).

Env var fallbacks: `MINIMAX_API_KEY`, `MINIMAX_BASE_URL`.

## Ingest (standalone)

```
mpm ingest <path> [--dry-run] [--batch-size <n>]
mpm ingest list-schemas <path>
mpm ingest status|review|cleanup|history|undo <batch-id>
```

Imports from external SQLite (OpenClaw chunks table). Stages to `raw_memories` with dedup, security filtering, and LLM review pipeline.

## Watch Lifecycle (detached)

```
mpm watch start      # Spawn detached background watcher (returns immediately)
mpm watch stop       # Signal the detached watcher to shut down gracefully
mpm watch status     # Check if watcher is running (PID file based)
mpm watch restart    # Not supported while detached watcher is running (stop + start instead)
mpm watch add-path <path> --type memory|sessions
mpm watch remove-path <path> --type memory|sessions
mpm watch list-paths
```

The detached watcher writes its PID to `{MPM_DIR}/watch.pid` and blocks until signaled. It handles `SIGTERM` / `os.Interrupt` gracefully (drains pool, cleans PID file, exits).

## System

```
mpm doctor      # Run diagnostics (6 categories)
mpm menu        # Mode/persona interactive picker
mpm web         # Serves at :18792
```

SPA with memories CRUD, topics CRUD, lessons CRUD, unified search, prime directives view. Token auth via `web_token` in config. Vanilla JS + CSS (no build step).

## Relevance Scoring

```
score = (reinforcement_count * 2) + (weight * 1.5) + recency_bonus
```

- 1: Normal memory
- 5-9: Important
- 10+: Long-term memory (LTM)
