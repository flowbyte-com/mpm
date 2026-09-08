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

## I just want the snippets

**→ [`MPM_AGENT_INTEGRATION_SNIPPETS.md`](./MPM_AGENT_INTEGRATION_SNIPPETS.md)**

The top of that file carries four copy/paste blocks — one per
persistent-file host (Claude Code, OpenCode, Pi, Hermes) — with the
exact wording to paste into your host's persistent instruction file
(CLAUDE.md / AGENTS.md / .hermes.md). The same content is what the
per-host installers write; the installers are a convenience, not a
requirement.

If you are using OpenClaw, you don't need a snippets file at all —
the runtime injects the same seven invariants into the system prompt
automatically. See the OpenClaw row in [INSTALL.md](./INSTALL.md).

On Hermes, the managed block is delivered via `.hermes.md` (protocol
guidance only — not the dynamic wake payload). The agent must call
`mcp__mpm__mpm_context` action `read_wake_context` itself at the
start of its first turn to obtain the wake payload; Hermes has no
session-start hook for automatic wake injection.

For installation, the rest of this README explains the architecture;
the actual procedural manual is [INSTALL.md](./INSTALL.md).

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
host-independent contract. It defines the **principles** an MPM-aware
agent must honour:

1. **Wake** at session start (read MPM context before substantive work)
2. **Persist** during work, not only at the end
3. **Handoff** before genuine session closure
4. **MPM is the source of truth** for cross-session continuity
5. **Recovery / fallback** to `mpm call <tool> --payload ...` when the
   preferred integration breaks

All host adapters reference this file. They do not duplicate the
principles; they translate them into host-native instruction surfaces
and MCP-bundle wiring.

## Canonical managed-instruction snippets

[`MPM_AGENT_INTEGRATION_SNIPPETS.md`](./MPM_AGENT_INTEGRATION_SNIPPETS.md)
is the canonical source for the **exact wording** that adapters render
into each host's managed section. It contains the canonical managed
block (between `<!-- BEGIN MPM MANAGED BLOCK -->` and
`<!-- END MPM MANAGED BLOCK -->` markers) with `{TOOL_PREFIX}`
placeholders for each host's transport namespace.

The four file-based adapter snippets
(`claude-code-mpm/templates/CLAUDE.md.snippet`,
`opencode-mpm/templates/AGENTS.md.snippet`,
`pi-mpm/templates/AGENTS.md.snippet`,
`hermes-mpm/templates/hermes.md.snippet`) are **generated**, not
hand-edited. The render script
[`scripts/render_managed_blocks.py`](./scripts/render_managed_blocks.py):

1. Reads the canonical source.
2. Extracts the canonical managed block.
3. Substitutes each adapter's `{TOOL_PREFIX}` (e.g., `mpm__` for
   Claude Code, `mcp__mpm__` for Hermes, empty for OpenCode and Pi).
4. Composes each adapter's full snippet (header + rendered block +
   host-specific notes).
5. Writes the snippet to the adapter's `templates/` directory.

**Drift detection:** the test suite
[`tests/test_render_managed_blocks.py`](./tests/test_render_managed_blocks.py)
verifies byte-for-byte parity between the checked-in snippets and the
rendered output. Run `python3 scripts/render_managed_blocks.py --check`
to verify; re-run without `--check` to regenerate after editing the
canonical source.

## Supported hosts

| Host | Adapter directory | Mechanism | Persistent-instruction surface |
|---|---|---|---|
| **OpenClaw** | [`openclaw-mpm-memory/`](./openclaw-mpm-memory/) + [`openclaw-mpm-auto-mode-persona/`](./openclaw-mpm-auto-mode-persona/) | OpenClaw plugin (`memory_search`/`memory_get` slot, kind:memory) + auto-mode/persona plugin (turn-key mode/persona injection) | `~/.openclaw/workspace/SOUL.md` + `AGENTS.md` (loaded by OpenClaw runtime) |
| **Claude Code** | [`claude-code-mpm/`](./claude-code-mpm/) | MCP server (`~/.claude/.mcp.json`, default 3-tool initial surface via `mpm-mcp` — `MPM_EXPOSE_ALL_TOOLS=1` restores the full 22-tool registry) + CLAUDE.md managed block | `~/.claude/CLAUDE.md` (managed-block convention) |
| **OpenCode** | [`opencode-mpm/`](./opencode-mpm/) | OpenCode plugin (TypeScript, 17 typed tools + `mpm call` CLI fallback to the full 22-tool substrate registry) + AGENTS.md managed block | `<project>/AGENTS.md` or `~/.config/opencode/AGENTS.md` (managed-block convention) |
| **Hermes** | [`hermes-mpm/`](./hermes-mpm/) | Hermes MCP client (`~/.hermes/config.yaml`, default 3-tool initial surface via `mpm-mcp` — `MPM_EXPOSE_ALL_TOOLS=1` restores the full 22-tool registry) + .hermes.md behavioral section | `<project>/.hermes.md` (or `HERMES.md`, walked from cwd to git root) — persona stays in `~/.hermes/SOUL.md` |
| **Pi** | [`pi-mpm/`](./pi-mpm/) | Pi extension (TypeScript, 17 typed tools + `mpm call` CLI fallback to the full 22-tool substrate registry) + AGENTS.md managed block | `~/.pi/agent/AGENTS.md` (global) or `<project>/AGENTS.md` (per-pi-docs search order) |

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
| [`mpm-agent-protocol.md`](./mpm-agent-protocol.md) | Canonical behavioral protocol — principles an MPM-aware agent honours |
| [`MPM_AGENT_INTEGRATION_SNIPPETS.md`](./MPM_AGENT_INTEGRATION_SNIPPETS.md) | Canonical managed-instruction snippets — exact wording rendered into each host's managed section |
| [`scripts/render_managed_blocks.py`](./scripts/render_managed_blocks.py) | Renders per-adapter snippets from the canonical source |
| [`tests/test_render_managed_blocks.py`](./tests/test_render_managed_blocks.py) | Byte-for-byte drift detection for adapter snippets |
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

## Maintenance

### Refreshing installed managed blocks

After editing `MPM_AGENT_INTEGRATION_SNIPPETS.md` (the canonical source
for the universal managed block), the per-host rendered snippets and
the locally installed managed blocks must be refreshed in lock-step:

```bash
make refresh-installed
```

This target runs:

1. **`render_managed_blocks.py`** — regenerates each adapter's
   `<adapter>/templates/<file>.snippet` from the canonical source.
2. **Each persistent-file host's installer** — refreshes the user's
   installed managed block in `~/.claude/CLAUDE.md`,
   `~/.config/opencode/AGENTS.md`, `~/.pi/agent/AGENTS.md`. Installers
   are content-aware: if the installed block already matches the
   snippet, the install is a no-op (idempotent).
3. **`render_managed_blocks.py --check`** — verifies byte-for-byte
   parity between the canonical source and the refreshed artifacts.

`make refresh-installed` is the safe re-run path when the canonical
source has drifted from a host's installed file. It is also the
post-edit verification step for any change to
`MPM_AGENT_INTEGRATION_SNIPPETS.md`.

Hermes is skipped here (no installed managed block in this
environment; the `hermes-mpm/SKILL.md` documents the manual flow).
OpenClaw uses runtime injection rather than a persistent managed file,
so its install path is via `openclaw-mpm-memory/install.sh` — a
separate concern (plugin wiring, not instruction-file refresh).

The structural smoke test
[`tests/test_refresh_installed.py`](./tests/test_refresh_installed.py)
verifies the target is registered in `.PHONY`, that every referenced
installer script and template snippet exists, and that the post-refresh
`--check` is wired up correctly.
