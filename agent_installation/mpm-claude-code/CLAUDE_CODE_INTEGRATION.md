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
| `MPM_PROVENANCE_PARENT_INVOCATION_ID` | Causal invocation lineage | _(empty)_ |
| `MPM_PROVENANCE_FRAMEWORK_SESSION_ID` | Native host session identity (`$CLAUDE_SESSION_ID`) | _(empty when absent)_ |

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

The installer (`./install.sh`) materializes the canonical SessionStart hook
at `~/.claude/hooks/mpm-session-start` and wires it into
`~/.claude/settings.json` automatically — no manual script creation needed.

If you want to install the hook by hand, copy
`agent_installation/mpm-claude-code/scripts/mpm-session-start` to
`~/.claude/hooks/mpm-session-start` and `chmod +x` it. The hook:

1. Calls `mpm call mpm_context --payload '{"action":"read_wake_context","params":{"format":"system-prompt"}}'`.
2. Extracts the JSON envelope's `content` field (the system-prompt formatted wake text).
3. Emits `{"hookSpecificOutput":{"hookEventName":"SessionStart","additionalContext":"<escaped wake>"}}` on stdout.

**The hook must emit a JSON envelope.** Plain prose on stdout is silently
dropped by ClaudeCode — verified empirically from the working superpowers
plugin at `~/.claude/plugins/cache/claude-plugins-official/superpowers/6.3.0/hooks/session-start`,
whose `printf '{ "hookSpecificOutput": ... }'` is what makes its
`<EXTREMELY-IMPORTANT>` content reach the model. The MPM hook uses the
exact same envelope shape.

The hook also exports `MPM_PROVENANCE_FRAMEWORK=claude-code` and (where
Claude Code provides them) `MPM_PROVENANCE_MODEL`,
`MPM_PROVENANCE_INVOCATION_ID`, and `MPM_PROVENANCE_FRAMEWORK_SESSION_ID`
from `$CLAUDE_MODEL` / `$CLAUDE_INVOCATION_ID` / `$CLAUDE_SESSION_ID`.
These populate MPM's audit trail with the calling framework's
identity. `$CLAUDE_SESSION_ID` maps to the framework-session slot
(NOT to parent invocation — Claude Code does not expose a causal
parent invocation identifier today, so the
`MPM_PROVENANCE_PARENT_INVOCATION_ID` slot stays empty).

### 3.2 Settings Configuration

The installer merges the SessionStart hook into `~/.claude/settings.json`:

```json
{
  "hooks": {
    "SessionStart": [
      {
        "matcher": "startup",
        "hooks": [
          {
            "type": "command",
            "command": "$HOME/.claude/hooks/mpm-session-start",
            "timeout": 10
          }
        ]
      }
    ]
  }
}
```

If you use a project-level settings file (`.claude/settings.json` at the repo
root), this hook runs only for that project. Unrelated `UserPromptSubmit`
hooks (e.g. `mpm route --apply`) are preserved across install/uninstall.

### 3.3 Semantic Contract: Session End ≠ Claimed Complete

> **A Claude Code session ending does not automatically emit `claimed_complete`.**
> Only emit it when the agent explicitly closes work via `mpm call mpm_work` with `{"action":"complete","params":{"work_id":"..."}}`.

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
Claude explicitly calls: mpm call mpm_work --payload '{"action":"complete","params":{"work_id":"xyz"}}'
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
    ├─▶ (optionally) mpm call mpm_work --payload '{"action":"complete","params":{"work_id":"..."}}'
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
# View all Claude Code tool invocations (framework=claude-code).
# tool_invocations has no MCP aggregator action; read the workspace DB
# directly (read-only) or use the audit-log surface for anomalies:
sqlite3 -readonly "$MPM_WORKSPACE/src/db/mpm.db" \
  "SELECT invocation_id, tool_name, result_status FROM tool_invocations WHERE framework_name = 'claude-code' LIMIT 10"

# Anomaly ledger (audit events), via the supported mpm_system action:
mpm call mpm_system --payload '{"action":"query_audit_log","params":{"days":7,"limit":20}}'
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
