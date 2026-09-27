# Hermes ↔ MPM Integration

## Integration

| Item | Value |
|---|---|
| Agent / framework | Hermes (minimax-oauth provider, hermes-agent CLI) |
| Integration path | `~/.mpm/agent_installation/mpm-hermes/` |
| Native mechanism | Hermes MCP client (`mcp_servers` in `~/.hermes/config.yaml`) + `.hermes.md` behavioural instruction file (walked up from cwd to git root) |
| MPM interface used | `mpm-mcp` stdio server (compact 3-tool MCP surface via JSON-RPC over stdio — intentionally fixed at `mpm_memory` / `mpm_context` / `mpm_help`; full registered MCP surface = every Registry entry plus the `mpm_help` discovery closure, reachable via `mpm call <tool> --payload '{...}'` or via `MPM_EXPOSE_ALL_TOOLS=1`) |
| Auto-load mechanism | MCP server auto-spawned from `mcp_servers` config; `.hermes.md` walked up from cwd at session start |
| MPM binary actually resolved | `$HOME/.mpm/bin/mpm-mcp` (canonical, absolute path) |
| Database actually used | `$HOME/.mpm/src/db/mpm.db` (the canonical install root; the symlink tree also exposes the same inode via `$HOME/projects/mpm/src/db/mpm.db` and `$HOME/.openclaw/workspace/projects/mpm/src/db/mpm.db`) |

## Architecture

Hermes participates in the MPM substrate via **two host-native surfaces**:

### 1. MCP stdio server (`mpm-mcp`)

Configured in `~/.hermes/config.yaml` under `mcp_servers.mpm`. Hermes
launches `mpm-mcp` as a stdio child process when the MCP client connects
at session start.

**Canonical config block:**

```yaml
mcp_servers:
  mpm:
    command: $HOME/.mpm/bin/mpm-mcp
    args: []
    env:
      MPM_WORKSPACE: $HOME/.mpm
      MPM_PROVENANCE_FRAMEWORK: hermes
    timeout: 60
    connect_timeout: 30
    enabled: true
```

**Binary path resolution:** Absolute — `$HOME/.mpm/bin/mpm-mcp`. No
PATH dependency.

**Workspace resolution:** `MPM_WORKSPACE` is read by `mpm-mcp` at boot
to locate the install root and resolve the database. The value
`$HOME/.mpm` is the canonical install root; the resolved DB lives at
`$HOME/.mpm/src/db/mpm.db` (or its symlink-equivalent path under the
project source tree — see *DB path invariance* below).

**Compact MCP surface** (default — `MPM_EXPOSE_ALL_TOOLS` unset):

| Tool | Purpose |
|---|---|
| `mcp__mpm__mpm_memory` | Persistent memory: `save`, `query`, `show`, `shred`, … |
| `mcp__mpm__mpm_context` | Session state, wake context, directives, mode routing |
| `mcp__mpm__mpm_help` | Capability discovery (full Registry enumeration + CLI fallback) |

**Full substrate surface** (22 MCP registrations = 21 substrate Registry
entries + `mpm_help` discovery closure): reachable via the universal
machine interface
`mpm call <tool> --payload '{"action":"<op>","params":{...}}'` from any
subprocess. Setting `MPM_EXPOSE_ALL_TOOLS=1` on the MCP env block
restores the legacy full-surface exposure to the MCP server's
`tools/list`. See `cmd/mpm-mcp/instructions_primer.txt` for the canonical
Registry. The Registry itself holds exactly 21 entries; `mpm_help` is
the discovery closure registered at server init by `cmd/mpm-mcp` (not
part of the Registry slice).

**DB path invariance:** All three paths resolve to the same inode:

- `$HOME/.mpm/src/db/mpm.db` — canonical install root (preferred)
- `$HOME/.openclaw/workspace/projects/mpm/src/db/mpm.db` — historical OpenClaw project-source path, now a symlink to the install root
- `$HOME/projects/mpm/src/db/mpm.db` — project source tree

The symlink tree is the established install layout. `mpm_required_db_path`
enforces convergence at boot so any of the three paths resolves to the
same DB. Operators should set `MPM_WORKSPACE=$HOME/.mpm` (canonical install
root); the other paths are equivalent by symlink but the install-root
value is the documented contract.

### 2. `.hermes.md` behavioural instruction file

Hermes walks up from cwd to git root looking for `.hermes.md` (or
`HERMES.md`) at session start and concatenates the contents into the
system prompt alongside the user-level persona at `~/.hermes/SOUL.md`
(loaded raw by `load_soul_md`, no managed-block convention).

**Persona is sacred:** the persona stays in `~/.hermes/SOUL.md`. The
MPM behavioural protocol lives in `<project>/.hermes.md`, written by the
installer in `scripts/install_hermes_instructions.py`. The installer
**never** touches `SOUL.md`.

The `.hermes.md` file carries the **static behavioural protocol only** —
not the dynamic wake payload. See *Wake context delivery* below.

## Wake context delivery

Hermes has **no native session-start hook** that injects MPM wake
context into the system prompt (in contrast to ClaudeCode's
`SessionStart`, OpenClaw's `session_start` → `agent_turn_prepare` chain,
OpenCode's `experimental.chat.system.transform`, or Pi's
`session_start` + `before_agent_start` pair).

The intended Hermes flow:

1. At session start, Hermes loads `~/.hermes/SOUL.md` and walks up to
   load `<project>/.hermes.md` into the system prompt. The `.hermes.md`
   content is **protocol guidance only** — no mode, persona, recent
   memories, or last-handoff summary.
2. On the **first turn**, the agent must explicitly call
   `mcp__mpm__mpm_context` action `read_wake_context` to obtain the
   dynamic wake payload (orientation signals: mode, persona, recent
   topics, recent memories, last handoff, available skills, etc.).

Arriving amnesic on a Hermes session (no wake call on turn 1) means the
agent missed the dynamic payload — diagnose the agent's first turn, not
the host integration. Manual refresh is available at any point via the
same `mcp__mpm__mpm_context` action `read_wake_context` call.

**Compact projection:** `params.projection: "compact"` returns a small
id+summary envelope (~1-2 KB) for hosts that only need orientation
signals. Default is the full payload.

## Provenance

Hermes identifies itself to MPM via the `MPM_PROVENANCE_FRAMEWORK` env
var in the MCP env block:

```yaml
env:
  MPM_PROVENANCE_FRAMEWORK: hermes
```

This env var populates `ActiveContext.FrameworkName` (via
`mpmcli.ActiveContextFromEnv`), which:

- Stamps `artifact_provenance.framework_name="hermes"` on every
  memory, decision, lesson, handoff, and other artifact Hermes writes.
- Filters `mpm_context.read_directives` via
  `ReadDirectivesForFramework` so framework-scoped directives reach
  Hermes (and do not leak into other frameworks).

Without `MPM_PROVENANCE_FRAMEWORK=hermes`, every Hermes write is
attributed to an empty / `"mcp"` framework name and Hermes does not
receive framework-scoped directives.

**Canonical vs legacy:** `MPM_PROVENANCE_FRAMEWORK` is canonical
(read first by `mpmcli.ActiveContextFromEnv` and
`provenance.NewFromEnv`). `MPM_FRAMEWORK` is the legacy alias (read
second) — preserved for backwards compatibility with existing installs.
New Hermes installs should set only `MPM_PROVENANCE_FRAMEWORK`.

**What Hermes does NOT set in the static config** (these are populated
per-call / per-invocation where the current architecture expects them,
not statically):

- `MPM_PROVENANCE_MODEL` — the active model is dynamic per turn;
  a static value would be a fabrication.
- `MPM_PROVENANCE_INVOCATION_ID` and
  `MPM_PROVENANCE_PARENT_INVOCATION_ID` — generated per invocation by
  the framework layer, not statically configurable.
- `MPM_PROVENANCE_ACTOR_KIND` — `mpm-mcp`'s audit hook hardcodes
  `actor_kind=agent` for every MCP tool call; setting it in the env
  block would imply a contract the codebase does not implement.

**Provenance contract:** observational, not authoritative. Artifact
correctness must remain independent of provenance availability; a
missing or incorrect provenance row must never block or poison an
artifact transaction.

## Mode and persona defaults

`MPM_ACTIVE_MODE` and `MPM_ACTIVE_PERSONA` are read by
`ActiveContextFromEnv` and stamp `mode` and `persona` onto every
artifact write:

- **If Hermes does not explicitly set `MPM_ACTIVE_MODE` / `MPM_ACTIVE_PERSONA`** (recommended — let MPM default them), writes are stamped with whatever the env values happen to be (empty when unset).
- **Recommended operator pattern:** let the runtime default to `default`/`default` via `mpm route --apply` or by setting `MPM_ACTIVE_MODE=default` and `MPM_ACTIVE_PERSONA=default` on the gateway process, not on the MCP server entry.
- `internal/core/active_state.go:ResolveActiveMode` and `ResolveActivePersona` fall back to `"default"` when a requested mode/persona .md file is missing on disk. The hardcoded safe default is `"default"` for both — the alpha-baseline professional identity.

> **Operator note (current implementation behaviour):** the wake JSON
> payload's `active_mode` field is sourced from `~/.mpm/active.json` via
> `internal/core/wake_context.go:readActiveState`. When `active.json`
> has no `modes` key, `readActiveState` returns an empty string for
> `active_mode` while `active_persona` resolves through
> `ResolveActivePersona`. This asymmetry is a known shared-core
> behaviour, not a Hermes-integration defect. (Reported under
> *Out-of-scope findings* in the integration verification record.)

## Installer

The Python installer
[`scripts/install_hermes_instructions.py`](./scripts/install_hermes_instructions.py)
writes the MPM behavioural section into `<project>/.hermes.md` (or
`HERMES.md`). It uses HTML-comment markers as the managed-section anchor
(Hermes has no built-in managed-block convention).

**Contract** (idempotent):

| Target state | Status | Backup? |
|---|---|---|
| Fresh target | `fresh` | no |
| Existing file, MPM block present, content unchanged | `no-op` | no |
| Existing file, MPM block present, content changed | `replaced` | yes |
| Existing file, no MPM block | `appended` | yes |
| Corrupted target (one marker without the other) | `aborted` (exit 2) | no |
| Uninstall: MPM block removed | `removed` / `empty-unlinked` | yes |

**Usage:**

```bash
# Install into a project
python3 ~/.mpm/agent_installation/mpm-hermes/scripts/install_hermes_instructions.py \
    --target-dir /path/to/project \
    --snippet ~/.mpm/agent_installation/mpm-hermes/templates/hermes.md.snippet

# Uninstall
python3 ~/.mpm/agent_installation/mpm-hermes/scripts/install_hermes_instructions.py \
    --target-dir /path/to/project \
    --uninstall
```

The snippet is generated from the canonical source
`agent_installation/MPM_AGENT_INTEGRATION_SNIPPETS.md` by
`scripts/render_managed_blocks.py`. Drift detection
(`render_managed_blocks.py --check`) verifies byte-for-byte parity.

## Recovery / fallback

If the MCP transport becomes unavailable mid-session, fall back to the
universal machine interface:

```bash
mpm call <tool> --payload '{"action":"<op>","params":{...}}'
```

`mpm call` writes to the same substrate with the same provenance
attribution as the MCP path. Do not abandon persistence when the
preferred transport breaks.

## Verification

| Check | Command | Expected |
|---|---|---|
| MCP surface | `echo '{"jsonrpc":"2.0","id":1,"method":"tools/list"}' \| $HOME/.mpm/bin/mpm-mcp 2>/dev/null \| jq '.result.tools \| length'` | `3` |
| Wake context (first turn) | `mcp__mpm__mpm_context` action `read_wake_context` | success:true, populated payload |
| Handoff write | `mcp__mpm__mpm_context` action `write_handoff` params `{summary}` | handoff_id returned, retrievable via `mpm call mpm_handoff list` |
| DB path convergence | `mpm call mpm_system health_check` → `db_path` | `$HOME/.mpm/src/db/mpm.db` (or symlink-equivalent) |
| Provenance attribution | `mpm add "test"` with `MPM_PROVENANCE_FRAMEWORK=hermes` | `artifact_provenance.framework_name="hermes"` |
| `.hermes.md` managed block | `grep -c 'BEGIN MPM-MANAGED BLOCK:mpm-hermes' <project>/.hermes.md` | `1` |
| Snippet parity | `python3 agent_installation/scripts/render_managed_blocks.py --check` | all 4 adapters in parity |