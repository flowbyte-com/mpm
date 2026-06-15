# Claude Code ↔ MPM Workspace Plugin — Design

**Date:** 2026-06-15
**Status:** Revised draft (mpm path resolution in install.sh, MAX_TIMEOUT_MS cap)
**Author:** Brainstorming session with user

## Summary

A workspace-level Claude Code plugin that exposes MPM's reasoning primitives to Claude Code via the Model Context Protocol. The plugin source lives in a sibling `claudecode-mpm-plugin/` folder; `install.sh` symlinks the files into `.claude/` so Claude Code auto-loads the MCP server on session start.

Mirrors the 18-tool surface of `opencode-mpm-plugin/src/index.ts`, translated from TypeScript to Python (using the official `mcp` Python SDK). Adds a `skills/mpm/SKILL.md` that teaches the agent the MPM workflow (read wake context on start, query memory before answering, propose theories before fixes, etc.).

## Goals

- Give Claude Code native tool access to MPM (memories, lessons, topics, references, decisions, theories, wake context, directives, proactive recall hints).
- Drop-in parity with the existing `opencode-mpm-plugin` and `hermes-mpm-plugin` so all three agents see the same tool surface.
- Source-of-truth in a version-controllable sibling folder, with a one-command install (symlink by default) into `.claude/`.
- Future-installable into other workspaces by pointing `MPM_WORKSPACE` at a different repo.

## Non-Goals

- Editing the `mpm/` Go source. This plugin is a *consumer* of `mpm call`; the Go binary is unchanged.
- Marketplace / distribution manifests (no `plugin.json`, no `marketplace.json`).
- Slash commands (no `/mpm wake`).
- Hooks (no auto-save on tool use, no auto-recall on user message).
- Settings.json modifications.
- Telemetry / analytics.
- TLS or auth — `mpm` is local; the MCP server is stdio-only.

## Architecture

```
┌──────────────────────────────────────────────────────────────┐
│  Claude Code session                                         │
│  ┌────────────────────────────────────────────┐              │
│  │  Agent: "What did we decide about X?"      │              │
│  │       │                                    │              │
│  │       ▼                                    │              │
│  │  Tool call: mpm_query_long_term_memory     │              │
│  └────────┬───────────────────────────────────┘              │
│           │ stdio                                              │
│           ▼                                                    │
│  ┌─────────────────────────────────────────────┐              │
│  │  python3 .claude/mpm-mcp/server.py          │ ◀── symlink │
│  │  (claudecode-mpm-plugin/server.py)          │              │
│  │                                              │              │
│  │  • run_mpm(args, timeout, signal)           │              │
│  │  • parse_mpm_result(result)                 │              │
│  │  • 18 tool handlers                         │              │
│  └────────┬────────────────────────────────────┘              │
│           │ subprocess.Popen                                   │
│           ▼                                                    │
│  ┌─────────────────────────────────────────────┐              │
│  │  mpm call <tool> --payload <json>           │              │
│  │  (resolves MPM_WORKSPACE via mpm's own      │              │
│  │   fallback chain)                           │              │
│  └────────┬────────────────────────────────────┘              │
│           ▼                                                    │
│  ┌─────────────────────────────────────────────┐              │
│  │  mpm binary                                  │              │
│  │  → JSON response on stdout                  │              │
│  └─────────────────────────────────────────────┘              │
└──────────────────────────────────────────────────────────────┘
```

**Key invariants:**
- The MCP server is a thin translator. It does no caching, no state, no DB access — every call shells out to `mpm`.
- One `asyncio.create_subprocess_exec` per call (no persistent process). Matches opencode's design; keeps error handling simple.
- 15s default timeout, 10 MiB stdout cap. SIGKILL on overrun.
- All 18 tools share the same `run_mpm` + `parse_mpm_result` pipeline; tool-specific code is purely schema + output formatting.

## File Layout

```
claudecode-mpm-plugin/
├── .mcp.json                 # MCP config template (${MPM_BINARY} placeholder; rendered at install)
├── server.py                 # MCP server: 18 tools → mpm call
├── test_server.py            # pytest: unit + optional smoke
├── requirements.txt          # mcp>=1.0, pytest>=8.0
├── install.sh                # Symlink (default) or copy; resolves mpm path; renders .mcp.json
├── README.md                 # Setup, config, troubleshooting
├── .gitignore                # Excludes .venv/
└── skills/
    └── mpm/
        └── SKILL.md          # Agent workflow guidance
```

After `./install.sh --symlink`:
```
.claude/
├── mcp.json                  (rendered from .mcp.json template, with $MPM_BINARY replaced)
├── mpm-mcp/
│   └── server.py             → ../../claudecode-mpm-plugin/server.py
└── skills/
    └── mpm/
        └── SKILL.md          → ../../../claudecode-mpm-plugin/skills/mpm/SKILL.md
```

`test_server.py` is **not** symlinked — it lives in the source folder, run with `pytest` from there. This keeps the `.claude/` tree minimal.

## Tool Set (18 tools — full parity with `opencode-mpm-plugin`)

| Tool | Args (per-tool `timeout_override_ms` listed where useful) | Maps to `mpm call` |
|------|-----------------------------------------------------------|---------------------|
| **Memory** | | |
| `query_long_term_memory` | `query: string, limit?: number, timeout_override_ms?: int` | `query_long_term_memory` |
| `save_to_memory` | `fact: string, tags?: string[], weight?: number, ttl?: string, collection?: string, timeout_override_ms?: int` | `save_to_memory` |
| `challenge_memory` | `memoryId: string, evidence: string, timeout_override_ms?: int` | `challenge_memory` |
| **Lessons** | | |
| `save_lesson` | `fact: string, type?: enum, tags?: string[], timeout_override_ms?: int` | `save_lesson` |
| `search_lessons` | `query: string, timeout_override_ms?: int` | `search_lessons` |
| `list_lessons` | `type?: enum, timeout_override_ms?: int` | `list_lessons` |
| **Topics** | | |
| `create_topic` | `name: string, description?: string, timeout_override_ms?: int` | `create_topic` |
| `search_topics` | `query: string, limit?: number, timeout_override_ms?: int` | `search_topics` |
| `link_topic` | `memory_id: string, topic_id: string, timeout_override_ms?: int` | `link_topic` |
| **References** | | |
| `add_reference` | `filepath: string, title?: string, timeout_override_ms?: int` (suggests 120000) | `add_reference` |
| `search_references` | `query: string, limit?: number, timeout_override_ms?: int` | `search_references` |
| `list_references` | `limit?: number, offset?: number, timeout_override_ms?: int` | `list_references` |
| **Session** | | |
| `read_wake_context` | `timeout_override_ms?: int` | `read_wake_context` |
| `read_directives` | `timeout_override_ms?: int` | `read_directives` |
| **Epistemology** | | |
| `propose_theory` | `hypothesis: string, validationCriteria: string, tags?: string[], timeout_override_ms?: int` | `propose_theory` |
| `resolve_theory` | `theoryId: string, conclusion: string, newStatus: enum, timeout_override_ms?: int` | `resolve_theory` |
| `record_decision` | `context: string, choice: string, rationale: string, outcome?: string, tags?: string[], weight?: number, timeout_override_ms?: int` | `record_decision` |
| **Proactive** | | |
| `proactive_recall_hint` | `conversation_text: string, max_hints?: number, min_score?: number, timeout_override_ms?: int` | `proactive_recall_hint` |

**`timeout_override_ms`** is an optional integer (milliseconds). When omitted, the 15s default applies. Tools that frequently exceed 15s (synthesis workers, ingest, PDF/EPUB parsing, theory resolution after LLM eval) should be invoked with a higher value by the agent. Tools that are always fast (read_wake_context, search_lessons) can stay on the default.

Tool names, args, and descriptions are translated verbatim from `opencode-mpm-plugin/src/index.ts` (the opencode plugin is the source of truth for the schema). Output formatting also matches: query/search/list tools return human-readable text blocks (not raw JSON), save/create tools return JSON.

## `server.py` Design

### Async + non-blocking subprocess

The MCP server uses `mcp.server.fastmcp.FastMCP`. Tool handlers are declared `async def` and shell out via `asyncio.create_subprocess_exec` + `await proc.communicate(timeout=...)` so the asyncio event loop is never blocked while waiting for `mpm`. (Plain `subprocess.Popen` inside `async def` would still block the loop on `communicate()`; the asyncio variant is required to handle multiple MCP client connections concurrently and to integrate with the SDK's `ctx.abort` signal.)

**Buffer safety:** the classic subprocess deadlock is "child fills stderr buffer while parent waits on stdout" — `proc.communicate()` (and its async equivalent) reads stdout and stderr *concurrently* into in-memory buffers, sidestepping the deadlock. Combined with the 10 MiB cap, total in-memory use is bounded to ~20 MiB per call, which is safe.

### Pydantic schemas (strict typing)

Every tool's input is defined as a Pydantic `BaseModel`, not a raw `dict`. Pydantic generates a precise JSON schema for Claude Code's tool-use planner; loose types (e.g. `memoryId: str = None`) lead the planner to invent defaults that fail the `mpm` argument validation. Use `Optional[T] = None` for optional fields, `Field(...)` for required fields, and explicit `Literal` enums for the small enums (`propose_theory.newStatus`, `save_lesson.type`).

Example:
```python
from pydantic import BaseModel, Field
from typing import Optional, Literal

class ReadWakeContextInput(BaseModel):
    timeout_override_ms: Optional[int] = Field(
        None, description="Per-call timeout in ms; defaults to 15000."
    )

class ProposeTheoryInput(BaseModel):
    hypothesis: str = Field(..., min_length=1, description="The hypothesis or assumption.")
    validation_criteria: str = Field(..., min_length=1, description="Executable test that would prove or disprove the hypothesis.")
    tags: list[str] = Field(default_factory=list, description="Optional tags.")
    timeout_override_ms: Optional[int] = Field(None, description="Per-call timeout in ms; defaults to 15000.")

class ResolveTheoryInput(BaseModel):
    theory_id: str = Field(..., min_length=1)
    conclusion: str = Field(..., min_length=1)
    new_status: Literal["proven", "disproven"]
    timeout_override_ms: Optional[int] = Field(None, description="Per-call timeout in ms; defaults to 15000.")
```

The FastMCP decorator accepts the Pydantic model directly: `@mcp.tool(name="propose_theory")(async def propose_theory(args: ProposeTheoryInput) -> str: ...)`.

### Core functions

```python
async def run_mpm(args: list[str], timeout_ms: int = 15000) -> MpmRunResult:
    """Spawn `mpm` with args via asyncio.create_subprocess_exec.
    Use await proc.communicate(timeout=...) to drain stdout and stderr
    concurrently (avoids the stderr-buffer deadlock).
    Returns {stdout, stderr, exit_code}. Kills with SIGKILL on overrun.
    Raises asyncio.TimeoutError on timeout."""

def parse_mpm_result(result: MpmRunResult) -> dict:
    """Map non-zero exit codes to structured error dicts.
    Recognize `database is locked` / `SQLITE_BUSY` and `[output exceeded]`.
    On success, JSON.parse(stdout) with text fallback."""

def make_call_mpm_call(run_mpm):
    """Returns async (tool_name, payload, timeout_override_ms=None) -> dict.
    Wraps `mpm call <tool> --payload <json>`.
    Passes timeout_override_ms through to run_mpm; falls back to 15s default."""

def format_age(created_at: str) -> str:
    """Human-readable age for wake_context and directives output."""

def debug_log(line: str) -> None:
    """Append a debug line to $MPM_DEBUG_LOG (default .claude/debug.log) when DEBUG=1.
    Includes ISO timestamp + line. Best-effort; never raises."""
```

Every `@mcp.tool()` registration is `async def`, takes a Pydantic input model, and calls `run_mpm` via `await`.

### Limits

- `MAX_BUFFER = 10 * 1024 * 1024` (10 MiB, matches opencode)
- `DEFAULT_TIMEOUT_MS = 15000` (matches opencode)
- `MAX_TIMEOUT_MS = 5 * 60 * 1000` (5 minutes, hard cap). `run_mpm` clamps the effective timeout to `min(MAX_TIMEOUT_MS, timeout_override_ms or DEFAULT_TIMEOUT_MS)`. This prevents a runaway LLM synthesis from hanging the MCP session if the upstream provider stalls. Silent cap (the agent doesn't see "clamped" errors — the cap simply wins).
- SIGKILL on timeout or stdout overflow
- `stderr` truncated to last `MAX_BUFFER` bytes on overflow (informational only)
- Per-call `timeout_override_ms` from the tool schema overrides the default (e.g. `add_reference` and `propose_theory` typically need 60000-120000ms; `read_wake_context` is fine on 15s)

### Env

- `MPM_BINARY` (default `"mpm"`) — path to the `mpm` binary
- `MPM_WORKSPACE` — **not** set by `.mcp.json`; relies on the `mpm` binary's own resolution chain (`$MPM_WORKSPACE` → `~/.openclaw/workspace/projects/mpm` → CWD). The shipped `.mcp.json` leaves this unset so the plugin is portable across workspaces.
- `DEBUG` — if `"1"`, `server.py` appends a line to `MPM_DEBUG_LOG` (default `.claude/debug.log`) for every tool invocation: timestamp, tool name, full `mpm` argv, exit code, byte counts. Lets the user verify exactly what the agent is sending without tailing the full session. The log file is created lazily; the parent directory must already exist (the symlink install guarantees this).

### Error mapping (from `parse_mpm_result`)

| Condition | Mapped error | Retryable? |
|-----------|--------------|------------|
| `exitCode == 125` + `[output exceeded]` | `wake_context_truncated` | No |
| stderr contains `database is locked` or `SQLITE_BUSY` | `database_locked` | Yes |
| Other non-zero exit | `exit_<N>` with first stderr line as message | Depends |
| Empty stdout on success | `{id: "", success: true, count: 0}` | N/A |
| Invalid JSON on success | `{id: "", success: true, text: <raw>}` | N/A |

## `.mcp.json`

The shipped template (relative paths, since `.mcp.json` lives at `claudecode-mpm-plugin/.mcp.json` and resolves from there). `${MPM_BINARY}` is a placeholder; `install.sh` substitutes the resolved absolute path of the `mpm` binary at install time.

```json
{
  "mcpServers": {
    "mpm": {
      "type": "stdio",
      "command": "${PWD}/.venv/bin/python3",
      "args": ["${PWD}/server.py"],
      "env": {
        "MPM_BINARY": "${MPM_BINARY}"
      }
    }
  }
}
```

After install, the rendered `.claude/mcp.json` looks like (with `$MPM_PATH` resolved):
```json
{
  "mcpServers": {
    "mpm": {
      "type": "stdio",
      "command": "/home/v/workspace/projects/mpm/claudecode-mpm-plugin/.venv/bin/python3",
      "args": ["/home/v/workspace/projects/mpm/claudecode-mpm-plugin/server.py"],
      "env": {
        "MPM_BINARY": "/home/v/workspace/projects/mpm/bin/mpm"
      }
    }
  }
}
```

`MPM_WORKSPACE` intentionally unset — the `mpm` binary resolves it via its own fallback chain.

## `install.sh` Design

The install creates a project-local venv at `claudecode-mpm-plugin/.venv`, installs the SDK into it, resolves the `mpm` binary path, and symlinks the source files (plus the venv's python) into `.claude/`. The shipped `.mcp.json` invokes the venv's python interpreter explicitly, so the plugin is self-contained — no `--user` install, no system-Python pollution, no venv activation required from the user.

**`mpm` binary resolution ("ghost environment" guard):** Claude Code may spawn the MCP server with a different `$PATH` than the user's terminal. To avoid `mpm: command not found` when `mpm` lives outside `/usr/local/bin` (e.g. a fresh checkout where `mpm` is at `bin/mpm` or via the `./mpm` symlink in the repo root), `install.sh` resolves the binary once at install time using this fallback chain and writes the absolute path into the installed `.claude/mcp.json`:

1. `$MPM_BINARY` env var (if set and exists)
2. `command -v mpm` (system PATH)
3. `$SRC/../bin/mpm` (the repo's `bin/mpm` build output, if the plugin is checked in next to the source)
4. `$SRC/../mpm` (the symlink at the repo root)
5. Hard fail: print an error and exit 1 if none of the above resolve to an executable.

The template `.mcp.json` in `claudecode-mpm-plugin/` uses `"${MPM_BINARY}"` as a placeholder that install.sh replaces with the resolved absolute path.

```bash
#!/usr/bin/env bash
# install.sh — symlink (default) or copy claudecode-mpm-plugin/ into ../.claude/
# Always creates a project-local venv at $SRC/.venv and points the installed
# server at $SRC/.venv/bin/python3 so runtime deps are isolated.
# Resolves the mpm binary at install time and rewrites .mcp.json with the
# absolute path so the plugin works regardless of $PATH in the MCP child env.
#
# Flags:
#   --symlink   (default) Symlink files; edits in source propagate.
#   --copy             Copy files; one-shot install, no link drift.
#   --uninstall        Remove .claude/mcp.json, .claude/mpm-mcp/, .claude/skills/mpm/.

set -euo pipefail

SRC="$(cd "$(dirname "$0")" && pwd)"
DST="$(cd "$SRC/.." && pwd)/.claude"
VENV="$SRC/.venv"
VENV_PY="$VENV/bin/python3"

# Sanity checks
[ -f "$SRC/.mcp.json" ] || { echo "error: $SRC/.mcp.json missing"; exit 1; }
[ -f "$SRC/server.py" ] || { echo "error: $SRC/server.py missing"; exit 1; }
command -v python3 >/dev/null || { echo "error: python3 not in PATH"; exit 1; }

# Resolve the mpm binary (ghost-environment guard)
resolve_mpm() {
    if [ -n "${MPM_BINARY:-}" ] && [ -x "$MPM_BINARY" ]; then
        echo "$MPM_BINARY"; return
    fi
    if command -v mpm >/dev/null 2>&1; then
        command -v mpm; return
    fi
    if [ -x "$SRC/../bin/mpm" ]; then
        echo "$SRC/../bin/mpm"; return
    fi
    if [ -x "$SRC/../mpm" ]; then
        echo "$SRC/../mpm"; return
    fi
    echo "error: could not locate 'mpm' binary (set \$MPM_BINARY or run 'make install')" >&2
    exit 1
}
MPM_PATH="$(resolve_mpm)"

# Create venv if missing
if [ ! -x "$VENV_PY" ]; then
    echo "Creating venv at $VENV ..."
    python3 -m venv "$VENV"
fi

# Install deps into venv
"$VENV_PY" -c 'import mcp' 2>/dev/null || {
    echo "Installing mcp SDK into venv..."
    "$VENV_PY" -m pip install --quiet -r "$SRC/requirements.txt"
}

# Create symlinks
mkdir -p "$DST/mpm-mcp" "$DST/skills/mpm"
ln -sf "$SRC/server.py"      "$DST/mpm-mcp/server.py"
ln -sf "$SRC/skills/mpm/SKILL.md" "$DST/skills/mpm/SKILL.md"

# Render .mcp.json with the resolved mpm path
sed "s|\${MPM_BINARY}|$MPM_PATH|g" "$SRC/.mcp.json" > "$DST/mcp.json"

echo "✓ Installed (symlink). Restart Claude Code to pick up MCP server."
echo "  mpm binary: $MPM_PATH"
```

`--copy` and `--uninstall` branches:
- `--copy`: replace each `ln -sf` with `cp -f`. Idempotent; safe to re-run. The venv is created the same way. The `.mcp.json` render step is identical.
- `--uninstall`: `rm -f "$DST/mcp.json"`, then `rm -rf "$DST/mpm-mcp" "$DST/skills/mpm"`. Safe to run when nothing is installed. The `.venv` directory in the source folder is *not* touched.

`.venv/` is added to `.gitignore` (and the project root's `.gitignore` if needed) so the venv isn't committed.

## `README.md` Outline

The README is the user-facing entry point. Sections:

1. **What this is** — one paragraph: workspace-level Claude Code plugin that exposes MPM's reasoning primitives as MCP tools.
2. **Install** — `./install.sh --symlink` (default) or `--copy`. Auto-creates `.venv/`, installs deps, creates symlinks.
3. **Uninstall** — `./install.sh --uninstall`.
4. **Update** — re-run `./install.sh`. For `--symlink`, just edit the source files; for `--copy`, re-run with `--copy`.
5. **Tools** — table of the 18 tools, group by category, with one-line descriptions.
6. **SKILL.md** — short paragraph pointing to `skills/mpm/SKILL.md` and noting that the agent follows its workflow automatically.
7. **Configuration** — env vars (`MPM_BINARY`, `MPM_WORKSPACE`, `DEBUG`, `MPM_DEBUG_LOG`).
8. **Troubleshooting** — common issues: `mpm` not in PATH, venv not created, debug log not appearing, MCP server not picked up.
9. **Development** — how to add a new tool, how to run tests, how to enable DEBUG=1.
10. **License / attribution** — short.

## `SKILL.md` Design

Front-matter + workflow guidance (no automation). Canonical directives (verbatim, copy these into `SKILL.md`):

1. **Wake context is your anchor.** When you need to know what the user was doing before you arrived, call `mpm_read_wake_context` immediately. If it's empty, tell the user you are ready for context — do not invent prior state. This is the single most important instruction in this skill; it prevents hallucinated context on the first turn.
2. **Read directives first.** Call `mpm_read_directives` on session start. They define what you must and must not do.
3. **Recall before answering.** Before answering any question about prior work, decisions, dates, people, preferences, or todos, call `mpm_query_long_term_memory` with a natural-language query.
4. **Persist after action.** After any non-trivial action, lesson learned, or decision, call `mpm_save_to_memory` / `mpm_save_lesson` / `mpm_record_decision`. Do not wait for the user to ask.
5. **Hypothesize before fixing.** Before non-obvious fixes, call `mpm_propose_theory` with explicit, executable validation criteria. Do not skip this step.
6. **Resolve after validation.** After running the validation, call `mpm_resolve_theory` with the conclusion (`proven` or `disproven`).
7. **Surface proactively (optional).** On each user message, optionally call `mpm_proactive_recall_hint` with the last user message to surface relevant prior context. Surface only the top hint.

## Error Handling & Edge Cases

- **`mpm` binary not in PATH (or different PATH under Claude Code):** `run_mpm` returns `{exitCode: 1, stderr: "[spawn error] ..."}`. `parse_mpm_result` returns `exit_1` with the spawn error message. The agent sees a clean error and can tell the user `mpm` is not installed. *Prevention:* `install.sh` resolves the binary at install time and writes the absolute path into the rendered `.mcp.json` — the runtime never has to do its own PATH lookup.
- **Workspace doesn't exist (no `mpm.db`):** `mpm` exits non-zero with a "no database" error. Mapped to `exit_<N>` with the stderr line. Agent can recover by running `mpm init` or pointing `MPM_WORKSPACE` elsewhere.
- **Venv missing or broken:** `install.sh` creates `.venv/` if absent and re-installs deps if `import mcp` fails. The user can re-run `./install.sh` to repair. The shipped `.mcp.json` points at `.venv/bin/python3` explicitly, so a broken system Python doesn't affect the plugin.
- **Long-running call (LLM synthesis > 15s):** the 15s default times out and the tool returns `exit_124`. Tools that frequently exceed 15s (`add_reference`, `propose_theory`, etc.) should be invoked with `timeout_override_ms` set to 60000-120000.
- **Runaway timeout request:** `run_mpm` clamps to `MAX_TIMEOUT_MS = 5 * 60 * 1000`. The agent doesn't see an error — the cap silently wins. Documented for the SKILL.md author so this isn't surprising.
- **Symlink target deleted:** the MCP server's `args[0]` path becomes invalid; stdio spawn fails with ENOENT. Install.sh does not guard against this; users who delete `claudecode-mpm-plugin/` should also remove the symlinks.
- **Stderr backpressure deadlock:** averted by `await proc.communicate()`, which drains both streams concurrently into in-memory buffers capped at `MAX_BUFFER` bytes each.

## Testing

`test_server.py` (pytest):

**Unit tests** (no `mpm` binary required):
- `parse_mpm_result` happy path → returns parsed JSON
- `parse_mpm_result` empty stdout → returns `{id: "", success: true, count: 0}`
- `parse_mpm_result` invalid JSON stdout → returns `{id: "", success: true, text: <raw>}`
- `parse_mpm_result` exit 125 + "output exceeded" → `wake_context_truncated`
- `parse_mpm_result` exit N + "database is locked" → `database_locked`
- `parse_mpm_result` exit N + "SQLITE_BUSY" → `database_locked`
- `parse_mpm_result` exit 1 + random stderr → `exit_1` with first stderr line
- `run_mpm` injects `MPM_BINARY` into env (mocked `asyncio.create_subprocess_exec`)
- `run_mpm` raises `asyncio.TimeoutError` on timeout (mocked)
- `run_mpm` honours `timeout_override_ms` per call
- `format_age` for various timestamps

**Smoke tests** (auto-skip if `mpm` not on PATH or `MPM_WORKSPACE` doesn't have a valid db):
- Spawn the server via `mcp` SDK test client
- Call `mpm_read_wake_context`, assert non-error response shape
- Call `mpm_query_long_term_memory` with a known query, assert string response

Run with:
```bash
cd claudecode-mpm-plugin
# If you haven't run install.sh yet:
python3 -m venv .venv
.venv/bin/pip install -r requirements.txt
.venv/bin/pytest test_server.py -v
```

## Dependencies

`requirements.txt`:
```
mcp>=1.0
pytest>=8.0
```

No system-level deps beyond Python 3.10+ (for `match` statements in error mapping) and a working `mpm` binary in PATH at runtime.

## Open Questions

None at design time. Risks:
- The 18-tool set is large; the `mcp.Tool` registrations will be the bulk of the file. Mitigation: phased implementation (2 → 18), each tool is 20-40 lines of schema + output formatting, mostly mechanical translation from opencode plugin.
- The 15s default timeout is too short for some `mpm` operations (notably `synthesize`, LLM-backed `propose_theory`, and PDF/EPUB `add_reference`). Mitigation: `timeout_override_ms` is exposed in every tool schema; tools that frequently exceed 15s should be invoked with 60000-120000ms.
- A runaway LLM call could request a very long `timeout_override_ms` and hang the MCP session. Mitigation: `MAX_TIMEOUT_MS = 5 * 60 * 1000` is enforced as a silent cap inside `run_mpm`.
- Claude Code may spawn the MCP server with a different `$PATH` than the user's terminal, breaking `mpm: command not found`. Mitigation: `install.sh` resolves the `mpm` binary at install time using a 5-step fallback chain and writes the absolute path into the rendered `.mcp.json`.
- The MCP server has no authentication. Acceptable because it's stdio-only and Claude Code runs the process; the only way to attack it is to compromise the local user account.

## Out of Scope (Explicit)

- No edits to `mpm/` Go source.
- No `plugin.json` / `marketplace.json` / distribution.
- No slash commands (`/mpm ...`).
- No hooks (`PreToolUse` / `PostToolUse`).
- No settings.json modifications.
- No telemetry / analytics.
- No TLS or auth on the MCP server (stdio is sufficient for local).
- No Windows support (symlinks + bash script; could be added later).

## Implementation Phasing

End state is 18 tools; we do not implement them all at once. Three phases:

**Phase 1 — Plumbing (target: 2 tools, 1 test, installable)**
- Scaffold: `claudecode-mpm-plugin/` with `.mcp.json`, `server.py`, `requirements.txt`, `install.sh`, `README.md`, `skills/mpm/SKILL.md`, `test_server.py`.
- Implement only 2 tools: `read_wake_context` and `query_long_term_memory`. These exercise the full plumbing (subprocess spawn, JSON parse, output formatting) without needing schema variety.
- Implement `parse_mpm_result` with full error mapping.
- Implement unit tests for `parse_mpm_result` (no `mpm` needed) and a smoke test that auto-skips if `mpm` is absent.
- `./install.sh --symlink` works; `.claude/mcp.json` is valid.

**Phase 2 — Manual verification (proves the plumbing)**
- Run `python3 claudecode-mpm-plugin/server.py` directly and feed it a JSON-RPC `tools/list` request via stdin to confirm the MCP handshake works outside Claude Code.
- Restart Claude Code; verify the 2 tools appear in the tool palette and that `mpm_read_wake_context` returns the same data as `mpm wake` from the terminal.
- Set `DEBUG=1` and confirm `.claude/debug.log` is being written.

**Phase 3 — Scale to 18 tools**
- Add the remaining 16 tools in 2-3 commits: (a) memory writes + lessons (5 tools), (b) topics + references (6 tools), (c) epistemology + directives + proactive (5 tools).
- Each batch: implement, run unit tests, restart Claude Code, smoke-test the new tools.
- Final acceptance: all 18 tools work end-to-end from a Claude Code session.

Rationale: debugging one tool's MCP schema + spawn pipeline is much faster than debugging 18 at once. The first two tools (read_wake_context, query_long_term_memory) exercise every code path that all 18 will share.

## Acceptance Criteria

Phase 1 (plumbing):
- [ ] `claudecode-mpm-plugin/` exists with `.mcp.json` (template), `server.py`, `test_server.py`, `requirements.txt`, `install.sh`, `README.md`, `skills/mpm/SKILL.md`, `.gitignore`.
- [ ] `server.py` is `async def`-based; uses `asyncio.create_subprocess_exec` + `await proc.communicate()`.
- [ ] Tool inputs are Pydantic `BaseModel`s with strict types.
- [ ] `run_mpm` clamps timeout to `MAX_TIMEOUT_MS = 5 * 60 * 1000`.
- [ ] 2 tools implemented: `read_wake_context`, `query_long_term_memory`.
- [ ] `parse_mpm_result` covers all 4 error paths (timeout, output exceeded, db locked, exit_N).
- [ ] `python3 -m pytest test_server.py -v` passes (unit tests; smoke tests skip if `mpm` absent).
- [ ] `DEBUG=1` env var writes a log line per tool call to `.claude/debug.log`.
- [ ] `install.sh` resolves the `mpm` binary (5-step fallback chain) and renders `.claude/mcp.json` with the absolute path. `install.sh --symlink`, `--copy`, and `--uninstall` all work.

Phase 2 (manual verification):
- [ ] `echo '{"jsonrpc":"2.0","id":1,"method":"tools/list"}' | python3 claudecode-mpm-plugin/server.py` returns the 2 tools.
- [ ] After restart, Claude Code shows the 2 `mpm_*` tools in the tool palette.
- [ ] Calling `mpm_read_wake_context` from a Claude Code session returns the same data as `mpm wake` from the terminal.

Phase 3 (full parity):
- [ ] All 18 tools implemented and unit-tested.
- [ ] All 18 tools visible in Claude Code tool palette.
- [ ] End-to-end smoke test exercises one tool from each category (memory, lesson, topic, reference, session, epistemology, proactive).
- [ ] README documents install, config, troubleshooting, and uninstall.
