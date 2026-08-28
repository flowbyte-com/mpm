# pi-mpm

Pi extension that wires the full [MPM](https://flowbyte.com/mpm) cognitive
substrate into the Pi coding agent. Every `mpm call <tool>` reachable from
the CLI is reachable from Pi as a typed tool.

## Coverage

**16 Pi tools registered**, in two layers:

| Layer | Count | Source |
|---|---|---|
| Unified Domain Tools ("Fat RPC") | 13 | `index.ts` (hand-written) |
| Standalone tools | 3 | `index.ts` (hand-written) |

The 13 Domain Tools cover the full cognitive surface from mpm's registry,
each dispatching on an `action` enum with free-form `params`:

| Domain | Actions |
|---|---|
| `mpm_memory` | save, query, shred, reinforce, weaken, snooze, set_weight, patch, promote, review, synthesize, challenge, commit_milestone |
| `mpm_session` | end, handoff, list_handoffs, flush, read, discard, promote_scratchpad |
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

- `explain_retrieval` — per-node retrieval diagnostic (same ordering as query)
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

Pi explicitly does not support MCP (`docs/usage.md` §303), so `mpm-mcp` is
not an option. The next-best is `mpm call`, which is a plain
`--payload '<json>'` subprocess. This extension is the smallest correct
bridge: 13 Fat-RPC domain adapters (one per domain) plus 3 standalone
adapters.

## Why 16 tools (and not 77)

Until the Phase 1/2 registry refactor of mpm, `mpm-mcp` exposed 77
granular tools (one per registry entry). The agent-facing tool definition
prompt — every tool's name, description, and parameter schema — grew to
~15KB of context on every turn. mpm now exposes **16** tools: the 13
Domain Tools are "Fat RPC" — each takes `{action: string, params: object}`
and the mpm backend validates and dispatches. That collapses ~77 distinct
tool definitions into 13 near-identical ones.

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
- `agent_installation/mpm-auto-route/`     — OpenClaw per-turn persona auto-routing.

Both follow the same `mpm call` subprocess pattern; pi-mpm is the Pi-shaped
adaptation with **full coverage** instead of the narrow OpenClaw slot.

## License

Same as mpm — AGPL-3.0. See `~/.mpm/LICENSE`.
