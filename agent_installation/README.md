# `~/.mpm/agent_installation/` — Agent Integration Surfaces

This directory holds the canonical agent-facing integration code for MPM.
Each supported agent (framework / CLI / harness / IDE) gets one subdirectory
here.

> **Invariant:** Any new integration code, scripts, manifests, skills,
> extensions, plugins, adapters, or onboarding helpers for an MPM-aware
> agent lives under `~/.mpm/agent_installation/<agent>/`. MPM core
> (`internal/`, `cmd/`, `src/`) is intentionally untouched by these
> adapters.
>
> The MPM machine-facing interface (`mpm call <tool> --payload '{"action":"…","params":{…}}'`
> + the `mpm-mcp` stdio server) is the contract. Adapter directories
> consume that contract — they do not import internal MPM packages.

## What this directory is

A bundle of per-agent installation assets:

| Kind | Examples | Purpose |
|---|---|---|
| **Plugins / extensions** | `opencode-mpm/`, `openclaw-mpm-memory/`, `openclaw-mpm-auto-mode-persona/`, `pi-mpm/` | Native host extension code (TypeScript, JS) |
| **MCP bundles** | `claude-code-mpm/`, `hermes-mpm/` | `.mcp.json` configs + install scripts for hosts that wire MCP via a side-channel config file |
| **Behavioral-instruction installers** | `claude-code-mpm/`, `opencode-mpm/`, `hermes-mpm/`, `pi-mpm/` | Snippet + Python installer that writes the MPM behavioral protocol into the host's persistent instruction file (CLAUDE.md / AGENTS.md / .hermes.md) |
| **Canonical protocol doc** | `mpm-agent-protocol.md` | Host-independent behavioral contract — the single source of truth that host adapters reference |

The name `agent_installation/` (rather than `agent_plugins/`) reflects
this mix: only a subset of the directories are plugins in the strict
sense; the rest is install scripts, MCP bundles, and protocol
documentation.

## Design principle

> **MPM provides capability. Host instructions make that capability
> part of normal agent behavior.**

An MCP bundle wires `mpm__*` tools into the agent (capability).
A behavioral-instruction installer writes the MPM protocol into the
host's persistent instruction file (CLAUDE.md / AGENTS.md / .hermes.md /
SOUL.md), so the agent actually invokes wake-context, persists during
work, writes handoffs at session end, and falls back to `mpm call` when
the preferred integration breaks.

Without the second layer, capability may exist but adoption is
inconsistent — agents may skip wake, persist nothing, and leave no
handoff, forcing the next session to rediscover everything.

## Canonical behavioral protocol

[`mpm-agent-protocol.md`](./mpm-agent-protocol.md) is the canonical
host-independent contract. It defines five invariants:

1. **Wake** at session start (read MPM context before substantive work)
2. **Persist** during work, not only at the end
3. **Handoff** before genuine session closure
4. **MPM is the source of truth** for cross-session continuity
5. **Recovery / fallback** to `mpm call <tool> --payload ...` when the
   preferred integration breaks

All host adapters reference this file. They do not duplicate the
principles; they translate them into host-native instruction surfaces
and MCP-bundle wiring.

## Supported hosts

| Host | Adapter directory | Mechanism | Persistent-instruction surface |
|---|---|---|---|
| **OpenClaw** | [`openclaw-mpm-memory/`](./openclaw-mpm-memory/) + [`openclaw-mpm-auto-mode-persona/`](./openclaw-mpm-auto-mode-persona/) | OpenClaw plugin (`memory_search`/`memory_get` slot, kind:memory) + auto-mode/persona plugin (turn-key mode/persona injection) | `~/.openclaw/workspace/SOUL.md` + `AGENTS.md` (loaded by OpenClaw runtime) |
| **Claude Code** | [`claude-code-mpm/`](./claude-code-mpm/) | MCP server (`~/.claude/.mcp.json`, full 22-tool registry via `mpm-mcp`) + CLAUDE.md managed block | `~/.claude/CLAUDE.md` (managed-block convention) |
| **OpenCode** | [`opencode-mpm/`](./opencode-mpm/) | OpenCode plugin (TypeScript, 17 typed tools + `mpm call` CLI fallback to the full 22-tool registry) + AGENTS.md managed block | `<project>/AGENTS.md` or `~/.config/opencode/AGENTS.md` (managed-block convention) |
| **Hermes** | [`hermes-mpm/`](./hermes-mpm/) | Hermes MCP client (`~/.hermes/config.yaml`, 22 tools via `mpm-mcp`) + .hermes.md behavioral section | `<project>/.hermes.md` (or `HERMES.md`, walked from cwd to git root) — persona stays in `~/.hermes/SOUL.md` |
| **Pi** | [`pi-mpm/`](./pi-mpm/) | Pi extension (TypeScript, 17 typed tools + `mpm call` CLI fallback to the full 22-tool registry) + AGENTS.md managed block | `~/.pi/agent/AGENTS.md` (global) or `<project>/AGENTS.md` (per-pi-docs search order) |

## Directory map

| Directory | What it contains |
|---|---|
| `claude-code-mpm/` | MCP wiring (`.mcp.json.template` + `install.sh`) + CLAUDE.md behavioral-protocol installer (snippet + Python installer + 27 tests) |
| `opencode-mpm/` | TypeScript plugin (`src/index.ts`, 17 typed tools + `mpm call` fallback to full registry) + AGENTS.md behavioral-protocol installer (snippet + Python installer) |
| `openclaw-mpm-memory/` | OpenClaw plugin (`index.js`): `memory_search`/`memory_get` slot + wake-context injection hooks + provenance env var hooks |
| `openclaw-mpm-auto-mode-persona/` | OpenClaw plugin (`index.js`): per-turn `mpm route --apply` invocation + bootstrap injection |
| `hermes-mpm/` | MCP wiring (config reference in SKILL.md) + .hermes.md behavioral-protocol installer (snippet + Python installer + 19 tests) |
| `pi-mpm/` | Pi extension (`index.ts`, 17 typed tools + `mpm call` fallback to full registry) + AGENTS.md behavioral-protocol installer (snippet + Python installer + 19 tests) |

Each adapter directory's own README (or SKILL.md for Hermes) is the
host-specific deep dive. See [INSTALL.md](./INSTALL.md) for
installation procedures.

## Installation

**See [INSTALL.md](./INSTALL.md)** for the full installation manual —
prerequisites, exact commands per host, scope choices, verification,
uninstall, troubleshooting, and provenance configuration.

One-line entry points (full procedure in INSTALL.md):

| Host | Install |
|---|---|
| OpenClaw | `cd openclaw-mpm-memory && ./install.sh` |
| Claude Code | `cd claude-code-mpm && ./install.sh` |
| OpenCode | `ln -s "$PWD/opencode-mpm" ~/.config/opencode/plugin/opencode-mpm` |
| Hermes | (config already in `~/.hermes/config.yaml`; run `hermes-mpm/scripts/install_hermes_instructions.py` for the behavioral section) |
| Pi | add `~/.mpm/agent_installation/pi-mpm` to `~/.pi/agent/settings.json` `extensions` |

## Documentation map

| Document | What it covers |
|---|---|
| [`README.md`](./README.md) | This file — orientation, supported hosts, design principle |
| [`INSTALL.md`](./INSTALL.md) | Procedural installation manual — prerequisites, commands per host, verification, uninstall, troubleshooting |
| [`mpm-agent-protocol.md`](./mpm-agent-protocol.md) | Canonical behavioral protocol — wake, persist, handoff, continuity, recovery |
| `<host>/README.md` or `<host>/SKILL.md` | Host-specific deep dive (validation evidence, host quirks, recovery details) |

## Validation record

| Date | Host | Verdict | Evidence |
|---|---|---|---|
| 2026-08-19 | OpenClaw | **READY** | Tests A–E live-verified; error paths tested; PATH/DB invariants exercised. See [`openclaw-mpm-memory/VALIDATION-2026-08-19.md`](./openclaw-mpm-memory/VALIDATION-2026-08-19.md). |
| 2026-08-19 | OpenCode | **READY** | Tests A–E live-verified via fresh `opencode run` sessions + plugin-level tests. Plugin required repair (missing `id` export) before it could load. See [`opencode-mpm/VALIDATION-2026-08-19.md`](./opencode-mpm/VALIDATION-2026-08-19.md). |
| 2026-08-19 | Claude Code | **READY** | 9/10 tests PASS, 1/10 INSPECTED via direct JSON-RPC against `mpm-mcp`. Integration uses canonical `mpm-mcp` server. See [`claude-code-mpm/VALIDATION-2026-08-19.md`](./claude-code-mpm/VALIDATION-2026-08-19.md). |
| 2026-08-21 | Hermes | **READY** | Tests A–E live-verified via MCP. MCP wiring was already active and functional. Phantom FTS5 corruption bug fixed (WAL timing race in HealthCheck). Legacy dead code excised. See [`hermes-mpm/SKILL.md`](./hermes-mpm/SKILL.md). |
| 2026-08-28 | Pi | **READY (code-inspected)** | `pi-mpm` extension registered 16 typed tools; AGENTS.md behavioral installer tests green (19/19). Live agent-session validation not run in this environment — see INSTALL.md verification section for the post-install checks. |

## Skill formation

The MPM Skill Workshop (canonical protocol §3.1 "SKILL FORMATION")
provides a guided workflow for authoring skills. Invoke it via
`mpm call mpm_skills '{"action":"workshop","params":{...}}'`.
