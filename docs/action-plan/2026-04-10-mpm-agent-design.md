# mpm-agent Design

## What It Is

A companion binary (`mpm-agent`) — a Go-native AI agent that uses MPM as its brain. Reads and writes directly to `mpm.db` via SQLite. Built as a single self-contained Go file.

## Relationship to MPM

| Aspect | Relationship |
|--------|-------------|
| Binary | Separate (`mpm-agent`), not part of `mpm` |
| Database | Reads/writes `mpm.db` directly via SQLite |
| Config | Reuses `mpm_config.json` `synth` section for LLM |
| Tools | MPM-aware tools write back to MPM |
| Source | Lives in `flowbyte/mpm-agent/` (MPM's sibling) |

## CLI Interface

```bash
mpm-agent                    # REPL mode
mpm-agent "query"           # Single-shot mode (streaming)
mpm-agent --batch "query"   # Single-shot, batch (no streaming)
mpm-agent --help
```

## Toolset

### Filesystem
| Tool | Input | Output |
|------|-------|--------|
| `read_file` | `{"path": "/foo/bar.txt"}` | File contents |
| `write_file` | `{"path": "/foo/bar.txt", "content": "..."}` | Success/error |

### Shell
| Tool | Input | Output |
|------|-------|--------|
| `shell` | `{"command": "ls -la"}` | stdout + stderr |

### Web
| Tool | Input | Output |
|------|-------|--------|
| `web_search` | `{"query": "Go error handling"}` | Top results with snippets |
| `web_fetch` | `{"url": "https://..."}` | Page content |

### MPM (read)
| Tool | Input | Output |
|------|-------|--------|
| `mpm_memory_search` | `{"query": "sqlite vacuum"}` | FTS5 results from `memories` table |
| `mpm_lesson_search` | `{"query": "debugging"}` | Matching lessons |
| `mpm_mode_list` | `{}` | Available modes |
| `mpm_persona_list` | `{}` | Available personas |

### MPM (write)
| Tool | Input | Output |
|------|-------|--------|
| `mpm_lesson_add` | `{"content": "Don't do X", "type": "warning", "tags": "safety"}` | Lesson ID |
| `mpm_mode_set` | `{"mode": "research"}` | Success/error |
| `mpm_persona_set` | `{"persona": "oracle"}` | Success/error |
| `mpm_synthesize` | `{"session_uuid": "..."}` | Synthesis result |

## Agent Loop

```
for {
    1. Build system prompt + retrieve relevant memories (FTS5)
    2. Call LLM with tools spec + context
    3. If response has no tool_calls → stream tokens to terminal, break
    4. If response has tool_calls → execute in parallel, collect results
    5. Append tool results to conversation
    6. Loop to step 1
}
```

## Context Building

On each loop iteration:
1. Retrieve top N memories via FTS5 `MATCH` (N=5, configurable)
2. Format as markdown section: `## Relevant Memories`
3. Prepend to system prompt
4. Conversation history (last M turns) appended after memories

## Output Truncation

| Output length | Behavior |
|--------------|----------|
| ≤1000 chars | Raw output |
| >1000 chars | First 500 + `...N chars truncated...` + last 500 |

## LLM Configuration

Reuses `mpm_config.json` `synth` section:
```json
{
  "synth": {
    "model": "MiniMax-M2.7",
    "api_key": "",
    "base_url": "",
    "max_tokens": 1024,
    "timeout_seconds": 300
  }
}
```

Fallback priority:
1. `mpm_config.json` synth section
2. Environment: `MINIMAX_API_KEY`, `MINIMAX_BASE_URL`

## Directory Structure

```
flowbyte/
├── mpm/                         # MPM (this repo)
│   ├── bin/mpm                  # MPM binary
│   ├── src/db/mpm.db           # SQLite database
│   └── ...
└── mpm-agent/                  # Companion agent (separate repo or sibling)
    ├── mpm_agent.go             # Single Go file (~800-1200 lines)
    ├── go.mod                   # Minimal: mpm/db, sqlite3, net/http
    └── Makefile                 # build, install, clean
```

## go.mod Dependencies (minimal)

```
module mpm-agent

go 1.18

require (
    github.com/mattn/go-sqlite3  # SQLite
)
```

No MPM import — agent reads MPM.db directly via raw SQL. No Go agent SDK — hand-rolled loop is ~50 lines.

## System Prompt

Base prompt instructs the agent:
- Who it is (MPM's agent companion)
- Available tools and their JSON schemas
- How to format tool calls
- Memory retrieval context

## Error Handling

| Situation | Behavior |
|-----------|---------|
| LLM API error | Print error to stderr, exit 1 |
| Tool execution error | Return error as tool result, continue loop |
| No memories found | Continue without memory context |
| MPM DB not found | Exit with helpful error ("run mpm start first") |
| Streaming interrupted (Ctrl+C) | Clean exit |

## Streaming

Use SSE/streamed response from OpenAI-compatible API:
- Tokens written directly to stdout as received
- No buffering until complete
- Interruptible with Ctrl+C

## v1 Scope

Ship this first:
- Single `mpm_agent.go` file
- REPL + single-shot + batch flag
- All tools above
- Streaming output
- MPM config reuse
- Smart truncation

## Out of Scope for v1

- Tool chaining / dependencies
- Async parallel tool execution
- Custom system prompt files
- Multiple memory strategies (BM25, vector, etc.)
- Persistent conversation history across runs
