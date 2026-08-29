# Claude Code Auto-Routing via UserPromptSubmit Hook — Design

**Date:** 2026-06-16
**Status:** Approved (brainstorming session)
**Author:** Brainstorming session with user
**Supersedes:** `2026-06-15-claude-mpm-plugin-design.md` (the deleted Python MCP-server plugin; git status `D` entries for `claudecode-mpm-plugin/`)

## Summary

Wire Claude Code into MPM's `internal.Router` (the same heuristic engine the `mpm-mcp` server already exposes) so that *every user prompt* is auto-routed to the appropriate mode + persona *before* the LLM sees it. Implementation is a single `UserPromptSubmit` hook in `~/.claude/settings.json` whose command is `mpm route` — a new top-level CLI command that renders a `<system-reminder>` block. `mpm` stays stateless; no daemon, no socket, no MCP round-trip from the hook.

The companion `mpm call route` JSON-RPC entry (also new) serves OpenClaw and Hermes, who are explicitly forbidden from sharing the text-rendering path.

## Goals

- **Zero-latency pre-hook.** Routing runs synchronously before the LLM is invoked, in microseconds (compiled Go regex), so the user perceives no delay.
- **No behavior change for users.** No alias, no wrapper script, no muscle-memory shift. Drop one JSON snippet, install complete.
- **Stateless execution.** Hook calls the `mpm` binary on PATH; no daemon, no socket, no IPC. Mirrors MPM's "the binary is the database" philosophy.
- **Surfaces env breakage, never masks it.** If `mpm` is missing from PATH, Claude Code shows a non-blocking hook error in the transcript. Silent ghost-bypasses are a bug.
- **Two consumers, one engine.** `mpm route` (text render) and `mpm call route` (JSON) both call `internal.Router.Evaluate()`. The MCP server's `route` tool (in `mpm-mcp`) is a third caller of the same engine.
- **Hard limit respected.** Rendered output stays under Claude Code's 10,000-char hook stdout cap (we cap at 9,500 internally for headroom).

## Non-Goals

- Editing `mpm-mcp` (the native Go MCP server already exposes `route` correctly; no change there).
- Building an `mpm install-claude-hook` helper. README-only install for v1. YAGNI.
- Marketing / distribution / marketplace manifests.
- TLS, auth, telemetry. The hook runs the local `mpm` binary, period.
- A shell wrapper around `mpm route` for "PATH safety." PATH failures should surface, not be silently absorbed.

## Context

MPM has a `route` tool that scores user prompts against the `mode/` and `persona/` directories using pre-compiled regex with anti-pattern penalties, hot-reloaded on file change (`internal/router.go`, `cmd/mpm-mcp/tools.go:342`).

**What exists already (no work needed):**
- `internal.Router` — the scoring engine
- `cmd/mpm-mcp/tools.go:toolRoute` + `handleRoute` — exposes `route` as an MCP tool (3rd consumer)
- `opencode-mpm-plugin/src/index.ts` — the OpenClaw/OpenCode integration (calls into MPM via TypeScript)
- `hermes-mpm-plugin/base.py` — the Hermes integration (calls into MPM via Python)
- Native Go MCP server binary `mpm-mcp` with 19 tools

**What is new in this spec:**
- `mpm call route` — JSON-RPC entry in the main `mpm` binary's `call` registry (tool #19, the existing list)
- `mpm route` — top-level CLI command that renders text for hook consumption
- A `UserPromptSubmit` hook entry for `~/.claude/settings.json` (documented in README, no install helper)
- README section + tool-table row for `mpm call route`

**What is being replaced:** The old `claudecode-mpm-plugin/` (a Python MCP server exposing 18 tools to Claude Code via stdio) is gone. The new architecture inverts the relationship: instead of Claude Code calling MPM tools *during* a turn, the hook runs MPM's router *before* each turn, and the LLM inherits the mode/persona as system context.

## Architecture

```
User submits prompt to Claude Code
   │
   ▼
UserPromptSubmit hook fires (sync, before LLM)
   │
   │  hook command: "mpm route"
   │  stdin: {"prompt": "<user text>", ...}
   │  env: user's PATH, MPM_ROUTE (optional), MPM_WORKSPACE (optional)
   │
   ▼
mpm route (Go binary)
   │
   ├── read prompt (positional arg OR stdin-JSON OR stdin-literal)
   ├── check /noroute prefix → exit 0, no output
   ├── check MPM_ROUTE=off env → exit 0, no output
   ├── resolve MPM workspace (env → compile-default → CWD fallback)
   ├── load internal.Router, hot-reload if needed
   ├── Router.Evaluate(prompt) → RoutingReport{modes, persona, scores}
   ├── if no mode and no persona selected → exit 0, no output
   ├── read mode JSON + persona MD files from disk
   ├── render <system-reminder> block
   ├── if rendered > 9500 chars → truncate persona first, then mode
   └── print to stdout, exit 0
   │
   ▼
stdout (rendered text) injected into Claude's context by Claude Code
   │
   ▼
Claude sees mode + persona instructions before responding
```

**Failure mode philosophy:** `mpm route` is designed to *never block the user*. Any failure — missing workspace, broken DB, malformed mode JSON, missing persona file — results in exit 0 with empty stdout. The hook then injects nothing and Claude Code proceeds normally. The only non-zero exit is reserved for programmer errors (bad flag syntax) that humans running `mpm route` interactively need to see.

**PATH failure is the exception:** If `mpm` is not on PATH, the *hook command* fails (Claude Code shows `<UserPromptSubmit hook error>` non-blocking). This is intentional — silent masking would be a worse bug than a visible error.

## Component 1: `mpm call route` (JSON-RPC endpoint)

**Purpose:** Machine interface for OpenClaw and Hermes. Pure JSON, no text rendering.

**Wire location:** `cmd/mpm/call.go` `toolRegistry` map.

**Signature:**
```
mpm call route --payload '{"prompt": "<user text>"}'
```

**Returns:** Standard `mpm call` JSON envelope wrapping a `RoutingReport`:
```json
{
  "success": true,
  "result": {
    "selected_modes": ["code-review"],
    "selected_persona": "security-focused",
    "scores": { ... },
    "triggers": { ... }
  }
}
```

**Error handling:** This is a machine interface; non-zero exits and JSON error envelopes are returned for real failures (missing workspace, malformed payload). Unlike `mpm route`, it does *not* swallow errors silently.

**Relationship to the MCP server's `route` tool:** The MCP server (`mpm-mcp`) already exposes its own `route` tool — that is an MCP-protocol surface, not a CLI surface. `mpm call route` is the *separate* JSON-RPC path for OpenClaw/Hermes, who call into the Go binary directly via `child_process.execSync` / `subprocess.run` rather than going through MCP. Both paths use the same `internal.Router` underneath.

## Component 2: `mpm route` (top-level CLI command)

**Purpose:** Human/hook interface. Renders a `<system-reminder>` text block to stdout for Claude Code to inject as context.

**Wire location:** `cmd/mpm/router.go` (the `CommandRouter` registry). Add to `r.Commands` map with `case "route"` in the `Execute` switch.

**Input precedence:**
1. Positional arg: `mpm route "<prompt>"` — preferred for shells/hooks
2. Stdin if no positional arg:
   - If stdin starts with `{`, parse JSON, extract `prompt` field
   - Else treat stdin literally as the prompt

**Opt-out mechanisms (checked in this order):**
- `/noroute` prefix (anywhere in prompt) → exit 0, no output
- `MPM_ROUTE=off` env var → exit 0, no output
- Empty/whitespace prompt → exit 0, no output

**Output format:**
```
<system-reminder>
MPM auto-route active: mode=<name> persona=<name>

<verbatim content of mode/<name>.md>

<verbatim content of persona/<name>.md>
</system-reminder>
```

Both mode and persona files are Markdown with YAML frontmatter (`mode/<name>.md`, `persona/<name>.md` — verified against the working `mode/architect.md` and `persona/default.md`). The renderer dumps the file contents verbatim; the LLM parses the frontmatter.

**Truncation rules** (length cap = 9,500 chars to stay under Claude Code's 10K hook stdout limit):
| Combined length | Action |
|---|---|
| ≤ 9,500 | Render fully |
| 9,500–10,000 (with persona present) | Truncate persona block, append `[...truncated, see mode/<name>.md]` |
| > 10,000 even with persona removed | Truncate mode block at ~9K, append same marker |

Truncation priority: **mode (operational rules) > persona (voice/tone)**. Correctness over sound.

**Edge cases:**

| Case | Behavior |
|---|---|
| No MPM workspace resolvable | exit 0, empty stdout |
| DB missing or corrupt | exit 0, empty stdout |
| Mode JSON malformed | exit 0, empty stdout (refuse to inject broken content; `mpm call route` surfaces the real error) |
| Persona file missing for selected persona | Render mode only, append `[persona X not found on disk]` marker |
| Mode file missing for selected mode | exit 0, empty stdout (operational rules are load-bearing; can't inject partial) |
| Run interactively (TTY) | Silent on success, silent on no-match, **stderr only on programmer error** |
| Run from hook (non-TTY) | Stdout only, no stderr noise |

**Exit codes:** Always 0 except for programmer errors (bad flag syntax etc.).

## Component 3: Claude Code hook configuration

**Hook spec source:** `code.claude.com/docs/en/hooks` (UserPromptSubmit event).

**`~/.claude/settings.json` snippet:**
```json
{
  "hooks": {
    "UserPromptSubmit": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "mpm route",
            "timeout": 1,
            "statusMessage": "MPM routing…"
          }
        ]
      }
    ]
  }
}
```

**Why 1s timeout:** The premise of this architecture is that compiled Go regex evaluates in microseconds and a cold binary boot takes single-digit ms. A 1s timeout is three orders of magnitude over the expected runtime. If we hit it, something is genuinely wrong (I/O lock, FS hang) and we want to fail fast, surface the error in the transcript, and let the user continue typing.

**No wrapper script:** Bare `mpm route`. PATH failures should surface as visible errors, not be silently absorbed by a shell wrapper that "saves" the user from a broken environment.

**`mpm` PATH requirement:** The user's shell environment must have `mpm` on `PATH`. The README documents this. There is no fallback or auto-install.

**`MPM_WORKSPACE` env var (optional):** Standard MPM path resolution applies. If unset, MPM falls back to the compile-time default (`$HOME/.openclaw/workspace/projects/mpm`) then CWD. The README documents this.

## Edge case matrix (consolidated)

### `mpm route` (text)

| Case | Behavior |
|---|---|
| Empty/whitespace prompt | exit 0, empty stdout |
| `/noroute` prefix | exit 0, empty stdout |
| `MPM_ROUTE=off` env | exit 0, empty stdout |
| MPM workspace unresolvable | exit 0, empty stdout |
| DB missing or corrupt | exit 0, empty stdout |
| Mode JSON malformed | exit 0, empty stdout |
| Persona file missing | Inject mode only, append marker |
| Mode file missing | exit 0, empty stdout (load-bearing) |
| Rendered 9.5K–10K | Truncate persona, append marker |
| Rendered > 10K even persona-trimmed | Truncate mode at ~9K, append marker |
| Stdin not valid JSON | Treat as literal prompt |
| Interactive (TTY) | Silent on success, stderr only on programmer error |
| From hook (non-TTY) | Stdout only, no stderr |

### `mpm call route` (JSON)

Same workspace/DB/Mode/Persona handling as `mpm route`, **but with real error returns** (non-zero exit, JSON error envelope). One-axis difference: silent-vs-loud failure.

### Hook (Claude Code's responsibility)

| Case | Behavior |
|---|---|
| `mpm` not on PATH | Claude Code logs non-blocking hook error in transcript; prompt proceeds |
| `mpm route` hangs past 1s | Same non-blocking error |
| `mpm route` writes to stderr | Ignored by Claude Code; only stdout is injected |

## Testing strategy

| Layer | Type | Covers |
|---|---|---|
| `internal/router.go` (existing) | unit | Mode scoring, persona max-pool, hot-reload |
| `mpm route` rendering | unit + table | Truncation rules, JSON-stdin parsing, persona-not-found, length cap |
| `mpm route` integration | shell script | `echo "review code for security" \| mpm route` produces non-empty `<system-reminder>`; `echo "hi" \| mpm route` produces nothing; `echo "/noroute foo" \| mpm route` produces nothing |
| `mpm call route` handler | unit + table | Payload parsing, error envelopes, JSON-shape stability for OpenClaw/Hermes |
| Hook end-to-end | manual (documented) | Install snippet, send a prompt, verify Claude's response shape reflects the mode |

No automated end-to-end hook test — Claude Code doesn't expose a hook harness, and stubbing one would be more code than the hook itself. The manual checklist in the README is the durable verification.

## README update scope

- **Tool count:** 19 (confirmed: `read_wake_context`, `query_long_term_memory`, `save_to_memory`, `challenge_memory`, `save_lesson`, `search_lessons`, `list_lessons`, `create_topic`, `search_topics`, `link_topic`, `add_reference`, `search_references`, `list_references`, `read_directives`, `propose_theory`, `resolve_theory`, `record_decision`, `route`, `proactive_recall_hint`). `route` is the 19th entry; the count does not change.
- **New section:** `## Claude Code integration` with the JSON snippet, PATH requirement, opt-out mechanisms, truncation rule, and the "verify it works" one-liner.
- **Tool table:** Add `mpm call route` row in the existing call-tool table.
- **JSON-RPC examples block:** Add `route` example showing the payload + return shape.

## File-by-file change list (for implementation plan)

| File | Change |
|---|---|
| `cmd/mpm/call.go` | Add `route` to `toolRegistry`; new `callRoute` handler; ~30 LOC |
| `cmd/mpm/router.go` | Add `route` to `CommandRouter.Commands`; `case "route"` in `Execute` switch; `handleRoute` handler that calls the new `mpm route` text renderer; ~50 LOC |
| `cmd/mpm/route_render.go` (new) | `mpm route` text rendering: input parsing (arg/stdin JSON/literal), opt-out checks, workspace resolution, router invocation, length cap, truncation; ~200 LOC |
| `cmd/mpm/route_render_test.go` (new) | Table-driven tests for rendering, truncation, JSON-stdin parsing, opt-outs |
| `cmd/mpm/call_test.go` (existing or new) | Tests for `callRoute` JSON handler |
| `README.md` | New `## Claude Code integration` section; `route` row in tool table; `route` example in JSON-RPC block |
| No new files in `internal/` | The renderer is a CLI concern, not an engine concern. `internal.Router` is unchanged. |
| No new files in `cmd/mpm-mcp/` | The MCP server's `route` tool is unchanged. |
| No new files in `opencode-mpm-plugin/` or `hermes-mpm-plugin/` | Those plugins already work; they call `mpm call route` (or its existing `mpm-mcp` equivalent). |

## Open considerations (deferred)

- **`mpm install-claude-hook` helper** — deferred until/unless README install becomes a real friction point.
- **Wrapper script** — intentionally not built. PATH failures should be visible.
- **Live-reload of `mpm route` between hooks** — `internal.Router` already hot-reloads on mode/persona file mtime change (`internal/router.go:127`), so this is automatic.
- **Telemetry on hit rate / mode distribution** — not in scope; can be added later via the existing SSE broker if useful.
