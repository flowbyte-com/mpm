# `~/.mpm/agent_installation/` — Agent Integration Surfaces

This directory holds the canonical agent-facing integration code for MPM.
Each agent (framework / CLI / harness) gets one subdirectory here.

> **Invariant:** Any new integration code, scripts, manifests, skills,
> extensions, plugins, adapters, or onboarding helpers for an MPM-aware
> agent lives under `~/.mpm/agent_installation/<agent>/`. MPM core (`internal/`,
> `cmd/`, `src/`) is intentionally untouched by these adapters.
>
> The MPM machine-facing interface (`mpm call <tool> --payload '{"action":"…","params":{…}}'`
> + the `mpm-mcp` stdio server) is the contract. Adapter directories consume
> that contract — they do not import internal MPM packages.

## Index

| Agent | Path | Mechanism | Surface | Status |
|---|---|---|---|---|
| **OpenClaw** | [`openclaw-mpm-memory/`](./openclaw-mpm-memory/) | (a) OpenClaw plugin (`memory_search`/`memory_get` slot, kind:memory) + (b) MCP stdio server | read+write | **alpha-validated 2026-08-19** |
| OpenClaw | [`mpm-auto-route/`](./mpm-auto-route/) | OpenClaw plugin (turn-key mode/persona switch via `mpm route --apply`) | bootstrap inject | live |
| **OpenCode** | [`opencode-mpm/`](./opencode-mpm/) | OpenCode plugin (TypeScript, 16 tools via `mpm call`) | full | **alpha-validated 2026-08-19** |
| **Claude Code** | [`claude-code-mpm/`](./claude-code-mpm/) | Claude Code MCP server (`~/.claude/.mcp.json`, 16 tools via `mpm-mcp`) | full | **alpha-validated 2026-08-19** |
| **Hermes** | [`hermes-mpm/`](./hermes-mpm/) | Hermes MCP client (`~/.hermes/config.yaml`, 16 tools via `mpm-mcp`) | full | **READY 2026-08-21** |
| Pi | [`pi-mpm/`](./pi-mpm/) | Pi extension (TypeScript, 16 tools via `mpm call`) | full | live |
| (auto-route lives at the install root) | [`mpm-auto-route/`](./mpm-auto-route/) | OpenClaw plugin (per-turn router) | bootstrap inject | live |

## OpenClaw — the canonical integration story

OpenClaw participates in the MPM cognitive substrate via **two complementary surfaces**, plus an optional third:

### 1. MCP stdio bundle — `mpm__*` native tools (full read+write)

**What it is.** `~/.mpm/bin/mpm-mcp` is the canonical machine-facing MCP
server. It speaks the JSON-RPC over stdio contract that OpenClaw's
gateway launches. Once wired, the agent has direct access to the **entire**
MPM surface as first-class native tools (`mpm__mpm_memory`, `mpm__mpm_session`,
`mpm__explain_retrieval`, `mpm__mpm_wakes`, `mpm__mpm_topics`, …).

**Wiring.** Lives in OpenClaw's runtime config (NOT in the MPM repo):

```bash
openclaw config set mcp.servers.mpm.command '/home/v/.mpm/bin/mpm-mcp'
openclaw config set mcp.servers.mpm.env.MPM_WORKSPACE '/home/v/.mpm'
openclaw config set mcp.servers.mpm.env.MPM_ACTIVE_MODE 'programming'
openclaw config set mcp.servers.mpm.env.MPM_ACTIVE_PERSONA 'correspondent'
openclaw gateway restart
```

A canonical `.mcp.json` snapshot lives at
[`openclaw-mpm-memory/.mcp.json`](./openclaw-mpm-memory/.mcp.json) for
reproducibility.

**Strengths.** Deterministic absolute binary path (PATH-independent).
Fast (~5ms median per call vs ~50ms subprocess). Full surface.
**Weaknesses.** Requires gateway restart to change config; bundle
disposes on mid-session gateway restart (recovery: fall back to
`mpm call` CLI per SOUL.md "MCP Runtime Recovery").

### 2. Memory slot plugin — `memory_search` / `memory_get` (read-only)

**What it is.** [`openclaw-mpm-memory/`](./openclaw-mpm-memory/) is an
OpenClaw plugin of `kind:"memory"`. It fills `plugins.slots.memory`
(the slot OpenClaw's `core/doctor/memory-search` check refuses to clear
when left on the default `memory-core`). Routes OpenClaw's
`memory_search` and `memory_get` tool calls through
`mpm call mpm_memory --payload '{"action":"query","params":{…}}'`.

**Wiring.**

```bash
openclaw plugins install ./openclaw-mpm-memory --link
./install.sh                          # idempotent bootstrap
openclaw config set plugins.entries.openclaw-mpm-memory.enabled true
openclaw config set plugins.slots.memory openclaw-mpm-memory
openclaw gateway restart
openclaw doctor --lint --only core/doctor/memory-search --json  # expect ok:true
```

**Strengths.** Satisfies the doctor check; integrates with OpenClaw's
existing `memory_search`/`memory_get` tool surface so any agent that
already speaks that contract transparently benefits. Fail-open on MPM
unavailability (`{disabled:true, error:…}` instead of crashing the turn).
Boot-time health check + DB path invariant gate (`MPM_REQUIRED_DB_PATH`).
**Weaknesses.** **Read-only** — no `memory_save` tool. Per-call subprocess
overhead (~50ms cold) vs MCP aggregator (~5ms). Compaction flush
**neutered at the sink** by architectural decision `40544f5a04a2aac7`
(MPM already has higher-fidelity epistemic sources — scratchpad +
explicit memories).

### 3. Mode/persona auto-route — `mpm-auto-route/` (bootstrap inject)

**What it is.** [`mpm-auto-route/`](./mpm-auto-route/) is an OpenClaw
plugin that, on every inbound prompt, shells out to `mpm route --apply`
and appends MPM's returned `<system-reminder>` block to the bootstrap
prompt. Lets MPM own mode/persona switching without touching OpenClaw
core.

### Sharing one MPM substrate

Both surfaces converge on the same MPM installation and database. The
canonical invariant is the `db_path` exposed by `mpm__mpm_system` /
`mpm call mpm_system --payload '{"action":"health_check","params":{}}'`:

```json
{ "db_path": "/home/v/workspace/projects/mpm/src/db/mpm.db",
  "db_path_raw": "/home/v/.mpm/src/db/mpm.db", "ok": true, ... }
```

To enforce "this agent MUST run against this specific DB", set
`MPM_REQUIRED_DB_PATH` in the OpenClaw gateway environment. The
plugin's boot-time health check will refuse to register if the live
`db_path` doesn't match.

## MPM Agent Protocol — host-independent behavioral layer

> "MPM tools provide capability. Host instructions provide behavioral
> adoption." — see [`mpm-agent-protocol.md`](./mpm-agent-protocol.md) for
> the canonical host-independent protocol, which is the **single source of
> truth** that all host adapters reference.

**Capability alone does not make an agent use MPM consistently.** An agent
needs persistent instructions telling it:

1. To call `read_wake_context` at session start (or have the host hook
   auto-inject wake context)
2. To persist meaningful state during work, not only at the end
3. To write a handoff before genuine session closure
4. To use MPM as the source of truth for cross-session continuity
5. To fall back to `mpm call <tool> --payload ...` when the host's
   preferred integration breaks

OpenClaw inherits these rules from `~/.openclaw/workspace/SOUL.md` and
`AGENTS.md`, which already encode the protocols above (predating this
canonical document). The host-independent protocol extracts the
behavioral layer so other agents — Claude Code, OpenCode, Hermes, Pi,
and any future host — can adopt the same operating contract without
re-deriving it from scattered documentation.

### Host adapters

| Host | Adapter file | Installer |
|---|---|---|
| OpenClaw | `~/.openclaw/workspace/SOUL.md` + `AGENTS.md` (existing) | OpenClaw runtime reads these directly |
| Claude Code | `~/.claude/CLAUDE.md` | run `claude-code-mpm/install.sh` |
| OpenCode | `<project>/AGENTS.md` or `~/.config/opencode/AGENTS.md` | run `opencode-mpm/scripts/install_agents_instructions.py` |
| Hermes | `<project>/.hermes.md` (or `HERMES.md`) | run `hermes-mpm/scripts/install_hermes_instructions.py` |
| Pi | `~/.pi/agent/AGENTS.md` (global) or `<project>/AGENTS.md` | run `pi-mpm/scripts/install_agents_instructions.py` |
| Other hosts | copy snippet from one of the above; adapt `--target` | manual |

The canonical protocol must remain coherent across hosts. Edit it
additively, and update every host adapter that consumes it. The
markers and snippets are *not* duplicated text — they **reference**
[`mpm-agent-protocol.md`](./mpm-agent-protocol.md).

### What changes vs. what stays put

- **Canonical (`mpm-agent-protocol.md`)** — host-independent principles,
  wake/persist/handoff/recovery vocabulary, MCP-vs-CLI fallback shape.
- **OpenClaw** — `SOUL.md` already encodes this; do not duplicate. Add
  a one-line reference in the existing MPM section linking to
  `mpm-agent-protocol.md`. Host-specific recovery details (gateway
  restart, bundle-mcp disposal) remain where they are.
- **Claude Code** — `~/.claude/CLAUDE.md` is the host's persistent
  instruction surface. The installer's managed block uses
  `<!-- BEGIN/END MPM-MANAGED SECTION:claude-code-instructions -->` so
  user-written content above and below the markers is preserved across
  reinstalls.
- **OpenCode** — `AGENTS.md` is the host's persistent instruction
  surface. Same managed-block convention as Claude Code. The
  `opencode-mpm` plugin already injects wake context automatically via
  the `experimental.chat.system.transform` hook; AGENTS.md adds the
  behavioral layer (handoff discipline, persist-during-work) the hook
  cannot reasonably inject.
- **Hermes** — `.hermes.md` (or `HERMES.md`) at the project root is
  walked from cwd up to git root and concatenated into the system
  prompt alongside the user-level `~/.hermes/SOUL.md` persona (per
  `hermes-agent/agent/prompt_builder.py:load_soul_md`). The persona
  stays in SOUL.md (loaded raw, no managed-block convention); the
  behavioral protocol goes in `.hermes.md` via the installer, which
  uses leading HTML-comment markers since Hermes has no built-in
  managed-block convention.
- **Pi** — Pi searches for `AGENTS.md` (or `CLAUDE.md`) at session
  start in three locations per pi docs: `~/.pi/agent/AGENTS.md`
  (global), `AGENTS.md` in any parent of cwd, `AGENTS.md` in cwd.
  Same managed-block convention as Claude Code and OpenCode. The
  `pi-mpm` extension at `~/.mpm/agent_installation/pi-mpm/` registers all
  16 MPM tools with full coverage; AGENTS.md adds the behavioral
  layer (wake, persist, handoff, recovery) the typed tools do not
  enforce — capability does not equal adoption. Disable AGENTS.md
  loading with `--no-context-files` / `-nc` for sessions that should
  bypass the MPM behavioral contract.

## Validation record

| Date | Agent | Verdict | Notes |
|---|---|---|---|
| 2026-08-19 | OpenClaw | **READY** | Tests A–E live-verified; error paths tested; PATH/DB invariants exercised. See [`openclaw-mpm-memory/VALIDATION-2026-08-19.md`](./openclaw-mpm-memory/VALIDATION-2026-08-19.md). |
| 2026-08-19 | OpenCode | **READY** | Tests A–E live-verified via fresh `opencode run` sessions + plugin-level tests. Plugin required repair (missing `id` export) before it could load. See [`opencode-mpm/VALIDATION-2026-08-19.md`](./opencode-mpm/VALIDATION-2026-08-19.md). |
| 2026-08-19 | Claude Code | **READY** | 9/10 tests PASS, 1/10 INSPECTED via direct JSON-RPC against `mpm-mcp`. Integration uses canonical `mpm-mcp` server. See [`claude-code-mpm/VALIDATION-2026-08-19.md`](./claude-code-cpm/VALIDATION-2026-08-19.md). |
| 2026-08-21 | Hermes | **READY** | Tests A–E live-verified via MCP. MCP wiring was already active and functional. Phantom FTS5 corruption bug fixed (WAL timing race in HealthCheck). Legacy dead code excised. See [`hermes-mpm/SKILL.md`](./hermes-mpm/SKILL.md). |
