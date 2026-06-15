# claudecode-mpm-plugin

A workspace-level Claude Code plugin that exposes MPM (Memory Persistence
Module) reasoning primitives to Claude Code via the Model Context Protocol.

Mirrors the 18-tool surface of `opencode-mpm-plugin`. Ships with 2 tools in
this version: `read_wake_context` and `query_long_term_memory`. The remaining
16 are added in a follow-up plan.

## Install

```bash
./install.sh --symlink   # default; edits in source propagate
# or
./install.sh --copy      # one-shot copy, no link drift
```

The installer:
1. Resolves the `mpm` binary via a 4-step fallback chain (`$MPM_BINARY` → `which mpm` → `../bin/mpm` → `../mpm` symlink → fail).
2. Creates a project-local venv at `claudecode-mpm-plugin/.venv` (skipped if present).
3. Installs `mcp`, `pydantic`, `pytest`, `pytest-asyncio` into the venv.
4. Symlinks (or copies) `server.py` and `skills/mpm/SKILL.md` into `.claude/`.
5. Renders `.mcp.json` at the project root from the template, with the absolute mpm path and the venv's python interpreter. (Claude Code reads MCP server configs from `.mcp.json` at the project root, not from `.claude/mcp.json`.)

Restart Claude Code to pick up the MCP server.

## Uninstall

```bash
./install.sh --uninstall
```

Removes `.mcp.json`, `.claude/mpm-mcp/`, `.claude/skills/mpm/`. The
project-local venv is **not** removed.

## Tools (Phase 1 — 2 of 18)

| Tool | Purpose |
|------|---------|
| `read_wake_context` | Read the agent's wake context (active mode, persona, recent topics/memories). |
| `query_long_term_memory` | Search long-term memory with a natural-language query. |

The remaining 16 tools (lessons, topics, references, decisions, theories,
proactive recall, etc.) ship in Phase 2.

## SKILL.md

`skills/mpm/SKILL.md` is loaded by Claude Code at session start and teaches
the agent the MPM workflow: read wake context first, query memory before
answering, propose theories before fixes, etc. Treat it as a versioned
prompt — update it in the repo and the agent learns the new workflow on the
next session restart.

## Configuration

| Env var | Default | Purpose |
|---------|---------|---------|
| `MPM_BINARY` | `mpm` (resolved at install) | Path to the `mpm` binary. |
| `MPM_WORKSPACE` | (unset; mpm resolves) | MPM workspace dir. If unset, `mpm` uses its own fallback chain. |
| `DEBUG` | unset | Set to `1` to enable debug logging. |
| `MPM_DEBUG_LOG` | `.claude/debug.log` | Path to the debug log file (created lazily). |

## Troubleshooting

**`mpm: command not found`**
The renderer couldn't locate the `mpm` binary. Set `$MPM_BINARY` to the
absolute path, or run `make install` in the parent `mpm/` project, then
re-run `./install.sh`.

**`ModuleNotFoundError: No module named 'mcp'`**
The venv is missing or broken. Delete `claudecode-mpm-plugin/.venv/` and
re-run `./install.sh --symlink`.

**MCP server not visible in Claude Code**
- Confirm `.mcp.json` exists at the project root (not `.claude/mcp.json` — that location is never read) and is valid JSON.
- Restart Claude Code (the MCP registry loads at startup).
- Check the Claude Code logs for stdio errors from the server.

**Tool call times out (exit_124)**
Default timeout is 15s. For slow operations (`add_reference`, `propose_theory`)
the agent should pass `timeout_override_ms` (e.g. 60000). The server clamps
any override to 5 minutes.

## Development

```bash
# Run tests
cd claudecode-mpm-plugin
.venv/bin/pytest test_tools.py test_server.py -v

# Run server directly for manual smoke testing (MCP requires the full handshake)
printf '%s\n%s\n%s\n' \
  '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"smoke","version":"0"}}}' \
  '{"jsonrpc":"2.0","method":"notifications/initialized"}' \
  '{"jsonrpc":"2.0","id":2,"method":"tools/list"}' \
  | ./.venv/bin/python3 server.py

# Add DEBUG logging
DEBUG=1 ./.venv/bin/python3 server.py
```

## License

Same as the parent `mpm` project.
