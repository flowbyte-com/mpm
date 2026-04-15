# MPM Command Reference

> **Everything is a memory.** Sessions, topics, lessons are just memories with different collections. This makes the CLI simple.

**Conventions:**
- `mpm <cmd>` — standalone, no daemon required
- `(daemon)` — requires `mpm start`
- `(TUI)` — opens interactive terminal UI

---

## Core Memory Commands

*No daemon required — fast, works anywhere*

### Add Memory

```bash
mpm add <content> [options]
```

| Option | Description |
|--------|-------------|
| `--collection <name>` | Collection name (default: memories) |
| `--tag <tag1,tag2>` | Tags (comma-separated) |
| `--session <id>` | Session ID to associate |
| `--weight <1-100>` | Initial importance (default: 1) |
| `--ttl <7d,24h>` | Time to live (e.g., 7d, 24h, 2h) |

**Examples:**
```bash
mpm add "Remember to call mom"
mpm add "Use sqlite3 Vacuum after bulk deletes" --tag go,sqlite --weight 10
mpm add "Temporary note" --ttl 7d
mpm add "Project meeting notes" --collection session --tag meeting
```

### List Memories

```bash
mpm ls [options]
```

| Option | Description |
|--------|-------------|
| `--collection <name>` | Filter by collection |
| `--tag <tag>` | Filter by tag |
| `--since <date>` | Since date (YYYY-MM-DD) |
| `--until <date>` | Until date (YYYY-MM-DD) |
| `--limit <n>` | Max results (default: 20) |

**Examples:**
```bash
mpm ls                                    # List recent memories
mpm ls --collection session              # List sessions
mpm ls --tag important --limit 50        # Important memories
mpm ls --since 2024-01-01               # Memories since date
```

### Show Memory

```bash
mpm show <id>
```

Show full details of a memory by ID.

### Recall / Search

```bash
mpm recall <query> [options]
mpm s <query>                           # Shorthand alias
```

| Option | Description |
|--------|-------------|
| `--since <date>` | Since date (YYYY-MM-DD) |
| `--until <date>` | Until date (YYYY-MM-DD) |
| `--limit <n>` | Max results (default: 15) |

**Examples:**
```bash
mpm recall golang
mpm recall "project management" --since 7d
mpm s database --limit 10
```

### Delete Memory

```bash
mpm rm <id>                              # Soft delete
mpm shred <id>                           # Secure delete (DELETE + VACUUM)
```

---

## Memory Importance

*No daemon required*

### Promote to LTM

```bash
mpm promote <id>
```

Makes memory permanent (weight=10, is_long_term=true), clears any TTL.

### Reinforce / Weaken

```bash
mpm reinforce <id> [delta]              # Increment reinforcement (default: +1)
mpm weaken <id> [delta]                 # Decrement reinforcement (default: -1)
```

Reinforcement increases `weight` slightly and tracks usefulness.

**Examples:**
```bash
mpm reinforce abc123                     # +1 reinforcement
mpm reinforce abc123 5                   # +5 reinforcement
mpm weaken def456                        # -1 reinforcement
```

### Set Weight Directly

```bash
mpm set-weight <id> <0-100>
```

**Examples:**
```bash
mpm set-weight abc123 50                 # Set to medium importance
mpm set-weight abc123 100                # Maximum importance
```

---

## Statistics & Maintenance

*No daemon required*

### Memory Stats

```bash
mpm stats
```

Shows:
- Total/active/deleted/expired counts
- LTM count (weight >= 10)
- Never-accessed memories (candidates for pruning)
- Distribution by collection
- Top tags
- Reinforcement distribution

### Prune

```bash
mpm prune [options]
```

| Option | Description |
|--------|-------------|
| `--older-than <duration>` | Prune memories older than (e.g., 90d, 30d, 24h) |
| `--never-accessed` | Prune memories never accessed |

**Examples:**
```bash
mpm prune                                # Prune expired TTL memories
mpm prune --older-than 90d              # Prune memories older than 90 days
mpm prune --never-accessed              # Prune forgotten memories
```

### Export

```bash
mpm export [options]
```

| Option | Description |
|--------|-------------|
| `--format json|csv` | Output format (default: json) |
| `--collection <name>` | Filter by collection |
| `--since <date>` | Since date (YYYY-MM-DD) |
| `--until <date>` | Until date (YYYY-MM-DD) |
| `--output <file>` | Output file (default: stdout) |

**Examples:**
```bash
mpm export --format json > memories.json
mpm export --collection session --since 2024-01-01 --format csv > sessions.csv
```

### Self-Maintenance

```bash
mpm maintain [options]
```

Run self-improvement processes (decay, consolidate, prune).

| Option | Description |
|--------|-------------|
| `--review` | Show LTM memories not accessed recently (for spaced reinforcement) |
| `--days <n>` | Days since access for review (default: 14) |

**Examples:**
```bash
mpm maintain                             # Run full self-maintenance
mpm maintain --review                    # Show memories needing reinforcement
mpm maintain --review --days 30         # Review memories not accessed in 30 days
```

---

## Daemon-Required Commands

*These require `mpm start` — they affect runtime agent configuration*

### Mode Operations

Modes are stackable behavioral contexts.

```bash
mpm mode [name]                         # Switch mode (TUI if no name)
mpm mode list                            # List available modes
mpm mode active                          # Show active modes
mpm mode add <name>                      # Add mode to stack
mpm mode remove <name>                   # Remove from stack
mpm mode clear                           # Clear all modes
```

**Available modes:** ask, creative, debug, default, design, direct, grow, plan, research, ship

### Persona Operations

One persona active at a time.

```bash
mpm persona [name]                       # Switch persona (TUI if no name)
mpm persona list                         # List available personas
mpm persona active                        # Show current persona
mpm persona set <name>                   # Set by name
mpm persona clear                        # Clear persona
```

### Prime Directives

```bash
mpm prime-directives
```

Show the 808 agent directives.

---

## Watch Daemon

Auto-ingestion from filesystem.

```bash
mpm watch [options]                     # Start watch daemon (standalone)
mpm watch --v                            # Verbose mode
mpm watch --dry-run                      # Process files but don't delete
mpm watch --once                         # One-shot sweep, then exit
```

**Started automatically with `mpm start`**

---

## Other Commands

| Command | Description |
|---------|-------------|
| `mpm start` | Start daemon + watch daemon |
| `mpm stop` | Stop daemon gracefully |
| `mpm restart` | Restart daemon |
| `mpm status` | Status dashboard |
| `mpm logs` | Tail daemon logs |
| `mpm doctor [--fix]` | Diagnostics, `--fix` attempts auto-repair |
| `mpm dashboard` (TUI) | Real-time terminal dashboard |
| `mpm web` | Start web UI server |
| `mpm ingest <path>` | Import memories from external SQLite |
| `mpm help` | Show help |
| `mpm version` | Show version |

---

## Legacy Commands

*These still work but route through daemon — kept for compatibility*

```bash
mpm session ...                          # Session operations
mpm topic ...                             # Topic operations
mpm lesson ...                            # Lesson operations
mpm reference ...                         # Reference library
mpm memory ...                            # Memory operations
```

---

## Relevance Scoring

Memories are scored by:
```
score = (reinforcement_count * 2) + (weight * 1.5) + recency_bonus
```

Higher scores surface first in relevance-ordered queries.

**Weight meanings:**
- 1: Normal memory
- 5-9: Important
- 10+: Long-term memory (LTM)
