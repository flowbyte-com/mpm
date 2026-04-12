# Claude Code + MPM Integration Scope

## Goal

Connect Claude Code (this agent) to MPM as its persistent memory and state layer — giving it long-term memory, session awareness, mode/persona switching, and a reference knowledge base across all sessions.

## What MPM Provides

| Capability | MPM Storage | What It Means for Claude Code |
|-----------|-------------|-------------------------------|
| Long-term memory | `memories` table (FTS5/LIKE) | Recall facts from weeks ago, searched by query |
| Session history | `sessions` table | Know what we did last session, last week |
| Topics | `topics`, `topic_memberships` | Organize memory by project/topic |
| Modes | `modes` table | Behavioral modes (e.g. "debug", "creative", "review") |
| Personas | `personas` table | Response style profiles injected as system prompt |
| Reference docs | `references` table | A searchable knowledge library |
| Lessons | `lessons` table | Warnings, practices, insights from past mistakes |

## Architecture

```
Claude Code (this process)
└── MCP Server (mpm-agent-mcp)
    └── SQLite database (mpm.db)
        ├── memories, sessions, references, lessons
        ├── modes, personas
        └── state/active_mode, state/active_persona
```

Claude Code connects via the MCP (Model Context Protocol) server, which provides all MPM tools as callable functions.

## Current Status

### Implemented

- [x] MCP server with all MPM tools exposed
- [x] Token authentication for MCP access
- [x] Memory search/add/delete
- [x] Session search/update
- [x] Reference and lesson search/add
- [x] Mode and persona list/set
- [x] Directive list/add
- [x] General tools (read_file, write_file, shell, web_search, web_fetch)

### Configuration

Claude Code settings (`~/.claude/settings.json`):
```json
"mcpServers": {
  "mpm": {
    "command": "/home/v/.openclaw/workspace/flowbyte/mpm/mpm-agent/bin/mpm-agent-mcp",
    "env": {
      "MPM_WORKSPACE": "/home/v/.openclaw/workspace/flowbyte/mpm",
      "MPM_API_TOKEN": "<token>"
    }
  }
}
```

See [MPM_AGENT_MCP.md](MPM_AGENT_MCP.md) for full MCP documentation.

## Session Continuity

At the start of each session, call `mpm_session_search` with `session_id="claude-code-workspace"` to load prior conversation context. End each session with `mpm_session_update` to persist the conversation for next time.

## Out of Scope

- Changes to the MPM daemon or watch system
- Changes to how other mpm-agent clients (Telegram) work
- Migrating MPM to a different database
- The mpm CLI tool itself

## Success Criteria

- [x] Claude Code can search and retrieve memories from prior sessions
- [x] Claude Code can store new memories that persist after exit
- [ ] Mode/persona switching changes Claude Code's behavioral context
- [x] Session continuity: Claude Code knows what project/issue was last discussed
