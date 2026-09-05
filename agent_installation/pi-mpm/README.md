# pi-mpm

Pi extension that wires the full [MPM](https://flowbyte.com/mpm) cognitive
substrate into the Pi coding agent. Every `mpm call <tool>` reachable from
the CLI is reachable from Pi as a typed tool.

## Coverage

**17 Pi tools registered**, in two layers:

| Layer | Count | Source |
|---|---|---|
| Unified Domain Tools ("Fat RPC") | 14 | `index.ts` (hand-written) |
| Standalone tools | 3 | `index.ts` (hand-written) |

The 14 Domain Tools cover the **core cognitive surface** exposed by this
adapter. The current MPM registry exposes **21 tools total** (14 domain
tools via Fat RPC + 4 pointer/dedicated tools — `mpm_work`,
`mpm_resolve`, `mpm_blob_read`, `mpm_blob_search` — +
3 standalones), so the Pi adapter is a hand-curated 17-tool subset.
Tools not registered here (`mpm_work`, `mpm_resolve`,
`mpm_blob_read`, `mpm_blob_search`) remain reachable via the canonical
`mpm call <tool> --payload '<json>'` CLI fallback.

Each Domain Tool dispatches on an `action` enum with free-form
`params`:

| Domain | Actions |
|---|---|
| `mpm_memory` | save, query, shred, reinforce, weaken, snooze, set_weight, patch, promote, review, synthesize, challenge, commit_milestone |
| `mpm_handoff` | write, read, list, shred |
| `mpm_scratchpad` | flush, read, discard, promote |
| `mpm_wakes` | schedule, check, check_pending_event, list, digest, upsert_task, list_tasks, delete_task |
| `mpm_theories` | propose, resolve |
| `mpm_lessons` | save, search, list |
| `mpm_decisions` | record |
| `mpm_topics` | create, search, link |
| `mpm_references` | add, search, list |
| `mpm_evidence` | add, list |
| `mpm_confidence` | show, recompute, explain, history, changes, trend, quality |
| `mpm_skills` | save, read, list, delete, promote_to_global |
| `mpm_context` | read_wake_context, read_directives, proactive_recall_hint, query_global_rules, record_global_rule, promote_to_global, route |
| `mpm_system` | gc_run, compact, health_check, migrate, query_audit_log, list_clusters, snooze_cluster, resolve_cluster, annotate_cluster |

Plus 3 standalone tools with their own parameter schemas:

- `mpm_retrieval_diagnose` — per-node retrieval diagnostic (same ordering as query). Subprocess invocation: `mpm call mpm_retrieval_diagnose`.
- `log_to_changelog` — self-report work against a git commit SHA
- `request_review` — concurrent multi-component review

Plus the same hooks and command as before:

- `session_start` → calls `mpm_context` (action `read_wake_context`), caches the prior handoff
- `before_agent_start` → injects the cached wake context into the system prompt once
- `/mpm-status` slash command → runs `mpm info`

## Why this exists

From mpm's README:

> "Three surfaces, one substrate. The CLI is the human-facing cognitive
> interface; `mpm call` and MCP are the agent-facing tool surfaces. All
> map to the same `internal/core/tools` registry."

Pi's upstream (`badlogic/pi-mono`) does not ship a native MCP client, so
`mpm-mcp` is not directly addressable from a stock Pi install. There is a
widely-used third-party bridge, **`pi-mcp-adapter`** (currently v2.32.1,
published 2026-09-01, maintained by `nicobailon` outside the `badlogic`
org, listed on `pi.dev` and seeing ~761K downloads/mo — install with
`pi install npm:pi-mcp-adapter`), which exposes MCP servers to Pi through
a single proxy tool. It is *not* what this extension uses: MPM's bundled
Pi integration talks to the substrate through Pi's native extension API
(`index.ts` registered tools + `before_agent_start` hook for wake-context
injection) and the `AGENTS.md` managed block for the behavioral contract,
not through MCP. A Pi user who prefers the MCP route can install
`pi-mcp-adapter` and register `mpm-mcp` as a configured server, but the
adapter surfaces `initialize.instructions` only on explicit proxy call —
verified by interactive probe 2026-09-05 (`docs/onboarding-mcp-native-audit-2026-09-05.md`
Part A): the model has to actively invoke `mcp({connect:"name"})` before
`mcp({instructions:"name"})` returns the field, and there is a same-turn
state-isolation quirk where the `instructions` shortcut reports "no
instructions cached" even after a successful `connect` in the same turn.
The `AGENTS.md` managed block installed by this extension remains the
reliable onboarding path for Pi users, since auto-injection of MCP
`instructions` into Pi's system prompt is not how the adapter is designed
to work — and was not observed in probing.

## Why 21 tools (and not 77)

Until the Phase 1/2 registry refactor of mpm, `mpm-mcp` exposed 77
granular tools (one per registry entry). The agent-facing tool definition
prompt — every tool's name, description, and parameter schema — grew to
~15KB of context on every turn. mpm now exposes **21 tools in the full
registry**: the unified Domain Tools are "Fat RPC" — each takes `{action:
string, params: object}` and the mpm backend validates and dispatches.
That collapses ~77 distinct tool definitions into a small set of
near-identical ones.

This Pi adapter registers a **17-tool subset** of the full 21-tool
registry (14 Domain Tools + 3 Standalones). The remaining registry tools
(`mpm_work`, `mpm_resolve`, `mpm_blob_read`, `mpm_blob_search`) are
reachable through the `mpm call <tool> --payload '<json>'` CLI fallback
when needed. Adding them as typed Pi tools is straightforward — the
schema is identical — but each new tool increases the prompt payload,
so the subset is curated rather than exhaustive.

Because every domain tool shares the same trivial parameter schema
(`action` + free-form `params`), the tool-definition prompt is now compact
instead of bloated. Per-action validation still happens in the mpm backend,
which returns a descriptive error envelope (`"Valid actions include …"`)
that the agent can self-correct from.

This file is **hand-written and static** by design: the registry is stable
enough that code generation would be an anti-pattern.

## Install

The canonical source lives at `~/.mpm/agent_installation/pi-mpm/index.ts`.
Auto-loading is enabled by adding the path to `~/.pi/agent/settings.json`:

```json
{
  "extensions": ["~/.mpm/agent_installation/pi-mpm"]
}
```

Or copy/symlink `index.ts` to `~/.pi/agent/extensions/`. See
[Pi's extension docs](https://github.com/earendil-works/pi-coding-agent/blob/main/docs/extensions.md)
for details.

MPM itself must be on `PATH` (`mpm --version` to verify).

**Loading precedence (per Pi docs):** Pi searches for `AGENTS.md` (or
`CLAUDE.md`) at session start in this order:

1. `~/.pi/agent/AGENTS.md` (global, all projects)
2. `AGENTS.md` in any parent directory of cwd (walking up)
3. `AGENTS.md` in cwd (current project)

The installer in this directory defaults to writing the global
location (`~/.pi/agent/AGENTS.md`). Per-project scope is `--scope
project`. To target a specific file name (e.g. `CLAUDE.md` instead of
`AGENTS.md`), pass `--filename CLAUDE.md`. To bypass the entire
context-file chain (and therefore skip the MPM behavioral contract
for a single session), launch Pi with `--no-context-files` / `-nc`.

## Behavioral-protocol install (per-project, optional)

The Pi extension handles wake-context injection automatically via the
`before_agent_start` hook. The behavioral protocol is delivered via
`AGENTS.md`, not via the typed tools. To install the AGENTS.md managed
section:

```bash
# Global scope (writes ~/.pi/agent/AGENTS.md):
python3 ~/.mpm/agent_installation/pi-mpm/scripts/install_agents_instructions.py \
    --scope user \
    --snippet ~/.mpm/agent_installation/pi-mpm/templates/AGENTS.md.snippet

# Per-project scope (writes $cwd/AGENTS.md):
python3 ~/.mpm/agent_installation/pi-mpm/scripts/install_agents_instructions.py \
    --scope project \
    --snippet ~/.mpm/agent_installation/pi-mpm/templates/AGENTS.md.snippet
```

The installer accepts `--target <file>`, `--target-dir <dir>`,
`--filename <name>`, and `--uninstall` overrides.

## Uninstall

```bash
# 1. Remove the extension entry from ~/.pi/agent/settings.json.
#    (The Python installer does not touch settings.json — that is a
#    manual step.)

# 2. Strip the managed section from the global AGENTS.md:
python3 ~/.mpm/agent_installation/pi-mpm/scripts/install_agents_instructions.py \
    --scope user --uninstall

# 3. (Or for project scope, repeat with --scope project.)
```

The uninstall path strips the managed block; if the file would be
empty afterwards, the file is unlinked. A backup is written before
any mutation (`<target>.bak.YYYYMMDDTHHMMSSZ`).

## Trade-offs

**Parameter schema permissiveness.** Each domain tool's parameter schema is
`{action: string, params: object}` — i.e., free-form. This is a deliberate
consequence of the Fat RPC pattern: the per-action fields are documented in
each tool's description, and validation is delegated to mpm itself.

The mpm backend returns a structured error envelope when required fields
are missing or an action is unknown (`{"error":"... Valid actions include
...","success":false}`), and the wrapper surfaces that as a soft error to
the LLM, which can self-correct. The trade-off is that the Pi tool picker
doesn't get per-action typed-schema hints — the LLM relies on the tool
description instead.

## Failure modes (all fail-open)

| Condition                | Behaviour                                                       |
| ------------------------ | --------------------------------------------------------------- |
| `mpm` not on PATH        | Tool returns soft error to LLM; session continues               |
| `mpm` exits non-zero     | `mpm call <tool>` envelope's `error` field rendered to the LLM  |
| Subprocess timeout       | 30s default; child killed; partial stdout discarded             |
| `read_wake_context` fails| `session_start` banner skipped; warning notify only            |

No error throws into the agent turn — every failure path returns a result envelope.

## Repair guide

If mpm changes its CLI surface:

1. **Tool renamed or domain reshuffled**: mpm's registry is the source of
   truth — update the corresponding tool name, description, and action list
   in `index.ts`. The change is confined to a few lines per domain.
2. **Domain action added/removed**: update that domain tool's description.
   The parameter schema never needs to change (it's free-form).
3. **Stdout format change** (e.g., NDJSON instead of single-object):
   update `parseLastJsonLine` in `index.ts` to consume NDJSON rather than
   scan for the last `{…}` line.

## Related mpm integrations in this repo

- `agent_installation/openclaw-mpm-memory/` — OpenClaw memory slot plugin (2 tools).
- `agent_installation/openclaw-mpm-auto-mode-persona/` — OpenClaw per-turn mode/persona auto-routing.

Both follow the same `mpm call` subprocess pattern; pi-mpm is the Pi-shaped
adaptation with **full coverage** instead of the narrow OpenClaw slot.

## License

Same as mpm — AGPL-3.0. See `~/.mpm/LICENSE`.
