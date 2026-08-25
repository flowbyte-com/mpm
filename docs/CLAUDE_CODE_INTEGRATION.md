# Claude Code Integration Recipe

This document describes how to connect Claude Code sessions to MPM's memory and
work-tracking substrate. It is the **tested** integration contract — syntax is
verified against the current Claude Code settings schema, not inferred.

> **Status:** `mpm-telemetry` has a pre-existing FTS5 linker failure
> (`undefined reference to 'log'` in the sqlite3 binding). This is unrelated to
> the integration path. The client path (`cmd/mpm` + `internal/core`) passes all
> regression tests. See [Pre-existing Issues](#pre-existing-issues) at the end.

---

## Layer 1: What MPM Provides

### 1.1 `read_wake_context --format=system-prompt`

Returns a human-readable projection of the current wake context:

```
**Mode:** coding
**Persona:** default

**Pending Work (2):**
  - Update README [mpm://work/abc]
  - Fix auth bug [partial] [mpm://work/xyz]

**Recent Memories:**
  - 4的记忆: use pnpm not npm (weight=9)
  - 5の教訓: always validate DB inputs (weight=8)
```

```bash
mpm call mpm_context --payload '{"action":"read_wake_context","params":{"format":"system-prompt"}}'
```

### 1.2 Provenance Environment Variables

When Claude Code sets these before invoking `mpm call`, MPM records the calling
framework, model, and invocation chain in its audit trail:

| Env var | Purpose | Default |
|---------|---------|---------|
| `MPM_PROVENANCE_FRAMEWORK` | Caller identity | `mpm-cli` |
| `MPM_PROVENANCE_MODEL` | Model name | _(empty)_ |
| `MPM_PROVENANCE_INVOCATION_ID` | This invocation's ID | _(generated)_ |
| `MPM_PROVENANCE_PARENT_INVOCATION_ID` | Parent invocation ID | _(empty)_ |

These populate `tool_invocations.framework_name`,
`tool_invocations.invocation_id`, and `artifact_provenance.model_name`.

---

## Layer 2: What Claude Code Provides

### 2.1 Settings File

Claude Code reads `~/.claude/settings.json` (project-scoped:
`.claude/settings.json`). Settings are JSON only — `.yml` is not supported.

### 2.2 Hook Events

Claude Code fires `SessionStart` once when a session begins. The hook runs a
command or script and captures stdout to inject context.

| Hook | When | Timeout |
|------|------|--------|
| `SessionStart` | Session begins or resumes | 600 s (default) |
| `SessionEnd` | Session terminates | **1.5 s shared budget** |

### 2.3 Runtime Environment Variables

These are set by Claude Code in the subprocess environment when hooks execute:

| Variable | Description |
|----------|-------------|
| `CLAUDE_MODEL` | Current model (e.g. `sonnet-5-20250514`) |
| `CLAUDE_SESSION_ID` | Current session ID |
| `CLAUDE_INVOCATION_ID` | Current invocation ID |
| `CLAUDE_PROJECT_DIR` | Project root directory |

> **Note:** `CLAUDE_MODEL`, `CLAUDE_SESSION_ID`, and `CLAUDE_INVOCATION_ID` are
> runtime variables set by Claude Code. Their presence and exact naming may
> change between releases — verify against the current
> [Claude Code environment variables docs](https://code.claude.com/docs/en/env-vars.md)
> before relying on them in production.

---

## Layer 3: The Glue

### 3.1 Hook Script

Create `~/.claude/scripts/mpm-wake.sh` (or any path you prefer):

```bash
#!/bin/bash
# mpm-wake.sh — inject MPM wake context into Claude Code at session start
# MPM_WORKSPACE is inherited from the Claude Code subprocess environment.

set -euo pipefail

# Export MPM provenance from Claude Code runtime variables.
# Use existing CLAUDE_* vars where available; fall back to safe defaults.
export MPM_PROVENANCE_FRAMEWORK=claude-code
export MPM_PROVENANCE_MODEL="${CLAUDE_MODEL:-unknown}"
export MPM_PROVENANCE_INVOCATION_ID="${CLAUDE_INVOCATION_ID:-}"
export MPM_PROVENANCE_PARENT_INVOCATION_ID="${CLAUDE_SESSION_ID:-}"

# Retrieve and print the wake context. Claude Code captures stdout and injects
# it into the session prompt. JSON success envelope is printed to stderr (or
# suppressed); the human-readable context is on stdout.
exec mpm call mpm_context \
  --payload '{"action":"read_wake_context","params":{"format":"system-prompt"}}' \
  2>/dev/null
```

Make it executable:

```bash
chmod +x ~/.claude/scripts/mpm-wake.sh
```

### 3.2 Settings Configuration

Add to `~/.claude/settings.json`:

```json
{
  "hooks": {
    "SessionStart": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "/home/user/.claude/scripts/mpm-wake.sh",
            "timeout": 30,
            "shell": "bash"
          }
        ]
      }
    ]
  }
}
```

If you use a project-level settings file (`.claude/settings.json` at the repo
root), this hook runs only for that project.

### 3.3 Semantic Contract: Session End ≠ Claimed Complete

> **A Claude Code session ending does not automatically emit `claimed_complete`.**
> Only emit it when the agent explicitly closes work via `mpm call mpm_work --action complete`.

Claude Code's `SessionEnd` hook has a 1.5-second shared budget and is not a
reliable place to record completion. More importantly, a session ending is a
lifecycle event, not an epistemic one — the agent may have been interrupted,
hit a timeout, or simply run out of context. Emitting `claimed_complete` on
session end would silently undo the verification model:

```
Session ends
    ≠
Work is complete and verified
```

The correct pattern:

```
Claude Code starts
    ↓
MPM wake context injected into session
    ↓
Claude works; may record evidence via mpm call
    ↓
Claude explicitly calls mpm call mpm_work --action complete --work-id=xyz
    ↓
MPM emits WorkEventTypeClaimedComplete
    ↓
Evidence accumulates in background
    ↓
MPM DeriveWorkVerification derives verification status
```

---

## Layer 4: End-to-End Contract

```
Session starts (Claude Code)
    │
    ├─▶ SessionStart hook fires
    │       │
    │       ▼
    │   mpm-wake.sh runs
    │       │
    │       ├── export MPM_PROVENANCE_* from CLAUDE_* env vars
    │       │
    │       ▼
    │   mpm call mpm_context --action read_wake_context --format=system-prompt
    │       │
    │       ▼
    │   stdout ──▶ injected into Claude session prompt
    │
    ├─▶ Claude Code runs normally
    │
    ├─▶ (optionally) mpm call mpm_work --action complete --work-id=...
    │       │
    │       ├── emits WorkEventTypeClaimedComplete
    │       ├── derives verification from evidence
    │       └── records in audit trail (framework=claude-code)
    │
    └─▶ Session ends
            │
            ▼
        SessionEnd hook fires (1.5s budget — lightweight only)
```

---

## Usage Examples

### Create and complete a work item from Claude Code

```bash
# Create work
mpm call mpm_work --payload '{
  "action": "create",
  "params": {"title": "Refactor auth module"}
}'

# ... do the work ...

# Claim completion (emits WorkEventTypeClaimedComplete)
mpm call mpm_work --payload '{
  "action": "complete",
  "params": {
    "work_id": "<id-from-above>",
    "note": "Refactored JWT validation, added PKCE support"
  }
}'
```

### Query audit trail from another session

```bash
# View all Claude Code tool invocations (framework=claude-code)
mpm call mpm_system --payload '{
  "action": "query",
  "params": {"sql": "SELECT invocation_id, tool_name, result_status FROM tool_invocations WHERE framework_name = '\''claude-code'\'' LIMIT 10"}
}'
```

---

## Pre-existing Issues

| Issue | Status | Notes |
|-------|--------|-------|
| `mpm-telemetry` FTS5 linker failure (`undefined reference to 'log'`) | **Known issue, not introduced by this integration** | Affects `cmd/mpm-telemetry/serve.go`. All `cmd/mpm` and `internal/core` tests pass. |

This integration only uses `cmd/mpm` (`mpm call`) and the core library — the telemetry package is a separate binary and is not part of the integration path.

---

## Changelog

- **2026-08-25** — Initial integration recipe. Hook schema verified against
  `code.claude.com/docs/en/hooks.md` and `settings-reference.md`. Environment
  variables verified against `code.claude.com/docs/en/env-vars.md`.
