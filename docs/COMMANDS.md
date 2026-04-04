# MPM Command Reference

**Conventions:**
- `mpm <cmd>` — standalone, no daemon required
- `(daemon)` — requires `mpm start`
- `(TUI)` — opens interactive terminal UI

---

## Daemon Management

| Command | Description |
|---------|-------------|
| `mpm start` | Start daemon + watch daemon. Handles stale socket cleanup. |
| `mpm stop` | Gracefully stop daemon and watch daemon. |
| `mpm restart` | Stop + start. |
| `mpm shutdown` | Alias for `stop`. |
| `mpm status` | Status table: version, uptime, mode, persona, DB size, memory/session counts, watch PID. |
| `mpm logs` | Tail daemon log output. |
| `mpm doctor [--fix]` | Diagnostics. `--fix` attempts auto-repair. |
| `mpm dashboard` (TUI) | Real-time terminal dashboard. |

---

## Watch Daemon

| Command | Description |
|---------|-------------|
| `mpm watch` | Start watch daemon standalone (no daemon needed). |
| `mpm watch --v` | Verbose mode — prints all file events. |
| `mpm watch --dry-run` | Process files but don't delete them. |
| `mpm watch --once` | One-shot startup sweep, then exit. |
| `mpm watch status` (daemon) | Check if watch daemon is running. |
| `mpm watch start` (daemon) | Start watch daemon via daemon proxy. |
| `mpm watch stop` (daemon) | Stop watch daemon via daemon proxy. |

The watch daemon starts automatically with `mpm start`.

---

## Memory Operations (daemon)

| Command | Description |
|---------|-------------|
| `mpm memory add <content>` | Store a new memory. Scanned for sensitive content first. |
| `mpm memory search <query>` | FTS5 full-text search with BM25 ranking. |
| `mpm memory search-term <term>` | Substring search, 500-char snippets. |
| `mpm memory show <id>` | Show a specific memory by ID. |
| `mpm memory list` | List recent memories (ordered by created_at DESC). |
| `mpm memory shred <id>` | Hard delete one memory (DELETE + VACUUM + FTS5 trigger). |
| `mpm memory wipe -f` | Delete ALL memories. Requires `-f` flag. |

---

## Session Operations (daemon)

Sessions are auto-ingested by the watch daemon. Manual commands:

| Command | Description |
|---------|-------------|
| `mpm session add <content>` | Manually add a session note. |
| `mpm session search <query>` | FTS5 search across sessions. |
| `mpm session show <id>` | Show session by ID. |
| `mpm session list` | List recent sessions. |

---

## Topic Operations (daemon)

Topics form a hierarchical tree. Memories and sessions can belong to topics via `topic_memberships`.

| Command | Description |
|---------|-------------|
| `mpm topic add <name> [description]` | Create a new topic. |
| `mpm topic search <query>` | Search topics by name/description. |
| `mpm topic show <id>` | Show topic details. |
| `mpm topic promote <id>` | Convert topic to a permanent memory. |
| `mpm topic list` | List recent topics. |
| `mpm topic shred <id>` | Shred a specific topic. |
| `mpm topic rm <name>` | Delete topic by name. |

---

## Persona Operations (daemon)

One persona active at a time. Personas are JSON configs in `persona/`.

| Command | Description |
|---------|-------------|
| `mpm persona` (TUI) | Interactive single-select persona picker. |
| `mpm persona list` | List all available personas. |
| `mpm persona active` | Show current active persona. |
| `mpm persona set <name>` | Set active persona by name. |
| `mpm persona clear` | Clear active persona. |

---

## Mode Operations (daemon)

Modes stack — multiple can be active simultaneously. Modes are JSON configs in `mode/`.

| Command | Description |
|---------|-------------|
| `mpm mode` (TUI) | Interactive multi-select mode picker (space to toggle, enter to confirm). |
| `mpm mode list` | List all available modes. |
| `mpm mode active` | Show currently active modes. |
| `mpm mode add <name>` | Add a mode to the active stack. |
| `mpm mode remove <name>` | Remove a mode from the stack. |
| `mpm mode clear` | Clear all active modes. |

**Available modes:** ask, creative, debug, default, design, direct, grow, plan, research, ship

---

## Compile Operations (daemon)

Modes and personas auto-compile on change. These commands force recompilation after manual JSON edits.

| Command | Description |
|---------|-------------|
| `mpm compile mode` | Compile all mode configs. |
| `mpm compile persona` | Compile all persona configs. |
| `mpm compile all` | Compile everything. |

---

## LLM Operations (daemon)

| Command | Description |
|---------|-------------|
| `mpm llm list` | List available LLM providers. |
| `mpm llm status` | Show current LLM configuration. |

---

## Recall & Synthesis (daemon)

| Command | Description |
|---------|-------------|
| `mpm recall <query>` | Search memories with contextual recall (FTS5 + SQLite). |
| `mpm synthesize <uuid>` | Extract structured facts from a session via LLM API. |

---

## Shred Operations (daemon)

Hard delete with VACUUM. Requires `-f` flag for bulk operations.

| Command | Description |
|---------|-------------|
| `mpm shred session <id>` | Shred specific session. |
| `mpm shred topic <id>` | Shred specific topic. |
| `mpm shred sessions -f` | Delete all sessions. |
| `mpm shred memories -f` | Delete all memories. |
| `mpm shred topics -f` | Delete all topics. |
| `mpm shred modes -f` | Delete all compiled modes. |
| `mpm shred personas -f` | Delete all compiled personas. |
| `mpm shred database -f` | Delete entire database and restart. |

---

## Reference Operations (daemon)

PDF and EPUB ingested with pure-Go parsers. Other formats read as plain text. No external tools required.

| Command | Description |
|---------|-------------|
| `mpm reference add <path>` | Ingest a document (PDF, EPUB, markdown, text, and more). |
| `mpm reference list` | List all reference documents. |
| `mpm reference stats` | Show library statistics (document/chunk counts). |
| `mpm reference search <query>` | Search reference documents by content. |
| `mpm reference get <id>` | Show full document content (all chunks). |
| `mpm reference show <id>` | Alias for `get`. |
| `mpm reference shred <id>` | Remove a reference document and its chunks. |

**Supported formats:** PDF (`.pdf`), EPUB (`.epub`), Markdown (`.md`), Plain text (`.txt`), HTML (`.html`), and any other file read as raw text.

---

## Lesson Operations (daemon)

Lessons are distilled knowledge from experience — warnings ("don't do X"), practices ("do Y"), and insights ("X leads to Y"). Unlike memories which store raw experiences, lessons capture learned wisdom.

Lessons are automatically reinforced when the same content is added again, increasing their reinforcement count to surface the most important lessons first.

| Command | Description |
|---------|-------------|
| `mpm lesson add <content> [--type warning\|practice\|insight] [--tags tags]` | Add a new lesson. |
| `mpm lesson list [--type warning\|practice\|insight]` | List all lessons, optionally filtered. |
| `mpm lesson search <query>` | Search lessons by content. |
| `mpm lesson get <id>` | Show a specific lesson. |
| `mpm lesson shred <id>` | Delete a lesson. |
| `mpm lesson stats` | Show lesson statistics. |

**Lesson types:**
- `warning` — "don't do X" (negative lessons, cost was felt)
- `practice` — "do Y" (positive lessons, best practices discovered)
- `insight` — "X leads to Y" (causal knowledge, default)

**Examples:**
```bash
mpm lesson add "Check file extensions before executing rm" --type warning --tags safety,files
mpm lesson add "Use gofmt for Go code formatting" --type practice --tags go,style
mpm lesson add "Deleting .git causes irreversible history loss" --type warning --tags git,safety
```

---

## Other Commands

| Command | Description |
|---------|-------------|
| `mpm help` | Show help menu. |
| `mpm version` | Show version and build info. |
| `mpm fortune` 🦞 | Crustafarian wisdom. |
