# `~/.mpm/agent_plugins/` — Agent Integration Surfaces

This directory holds the canonical agent-facing integration code for MPM.
Each agent (framework / CLI / harness) gets one subdirectory here.

> **Invariant:** Any new integration code, scripts, manifests, skills,
> extensions, plugins, adapters, or onboarding helpers for an MPM-aware
> agent lives under `~/.mpm/agent_plugins/<agent>/`. MPM core (`internal/`,
> `cmd/`, `src/`) is intentionally untouched by these adapters.
>
> The MPM machine-facing interface (`mpm call <tool> --payload '{"action":"…","params":{…}}'`
> + the `mpm-mcp` stdio server) is the contract. Adapter directories consume
> that contract — they do not import internal MPM packages.

## Index

| Agent | Path | Mechanism | Surface | Status |
|---|---|---|---|---|
| **OpenClaw** | [`openclaw-mpm-memory/`](./openclaw-mpm-memory/) | (a) OpenClaw plugin (`memory_search`/`memory_get` slot, kind:memory) + (b) MCP stdio server | read+write | alpha-validated 2026-08-19 |
| OpenClaw | [`mpm-auto-route/`](./mpm-auto-route/) | OpenClaw plugin (turn-key mode/persona switch via `mpm route --apply`) | bootstrap inject | live |
| OpenCode | [`opencode-mpm/`](./opencode-mpm/) | OpenCode extension (TypeScript) | full | live |
| Pi | [`pi-mpm/`](./pi-mpm/) | Pi extension (TypeScript) | full | live |
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

## Validation record

| Date | Agent | Verdict | Notes |
|---|---|---|---|
| 2026-08-19 | OpenClaw | **READY** | Tests A–E live-verified; error paths tested; PATH/DB invariants exercised. See [`openclaw-mpm-memory/VALIDATION-2026-08-19.md`](./openclaw-mpm-memory/VALIDATION-2026-08-19.md). |
