# pi-mpm

Pi extension that wires [MPM](https://flowbyte.com/mpm) into the Pi coding agent.

MPM is a long-term memory / cognitive substrate. Pi is a coding agent harness
with no built-in MCP support. This extension is the smallest correct bridge
between them: it registers Pi tools that shell out to mpm's agent-facing
JSON-RPC interface (`mpm call <tool> --payload '<json>'`).

## What it provides

| Surface        | Name                  | Wraps                              | Purpose                                           |
| -------------- | --------------------- | ---------------------------------- | ------------------------------------------------- |
| Tool (LLM)     | `mpm_recall`          | `query_long_term_memory`           | Free-text recall of durable memories              |
| Tool (LLM)     | `mpm_remember`        | `save_to_memory`                   | Persist a durable observation / decision / fact   |
| Tool (LLM)     | `mpm_session_handoff` | `session_end`                      | Cross-session handoff at end of meaningful work   |
| Tool (LLM)     | `mpm_explain`         | `explain_retrieval`                | Diagnostic: why did recall return this?           |
| Hook           | `session_start`       | `read_wake_context`                | Inject prior handoff + epistemic pressure banner  |
| Command        | `/mpm-status`         | `info`                             | Show install identity (version, DB, counts)       |

Every tool maps 1:1 onto an entry in mpm's `internal/core/tools` registry —
no mpm core changes required.

## Why this exists

From mpm's README:

> "Three surfaces, one substrate. The CLI is the human-facing cognitive
> interface (`mpm remember`, `mpm learn`, ...); `mpm call` and MCP are the
> agent-facing tool surfaces. All map to the same `internal/core/tools`
> registry."

Pi explicitly does not support MCP (`docs/usage.md` §303), so `mpm-mcp` is
not an option. The next-best is `mpm call`, which is a plain
`--payload '<json>'` subprocess — exactly what this extension wraps.

## Install

The canonical source lives at `~/.mpm/agent_plugins/pi-mpm/index.ts`.
To make Pi auto-discover it, add the path to `~/.pi/agent/settings.json`:

```json
{
  "extensions": ["~/.mpm/agent_plugins/pi-mpm"]
}
```

Or copy the file to `~/.pi/agent/extensions/pi-mpm.ts` (or symlink it).
See [Pi's extension docs](https://github.com/earendil-works/pi-coding-agent/blob/main/docs/extensions.md)
for details.

MPM itself must be on `PATH` (verify with `mpm --version`).

## Failure modes (all fail-open)

| Condition                | Behaviour                                                       |
| ------------------------ | --------------------------------------------------------------- |
| `mpm` not on PATH        | Tool returns soft error to LLM; session continues               |
| `mpm` exits non-zero     | `mpm call <tool>` envelope's `error` field rendered to the LLM  |
| Subprocess timeout       | 15s default; child killed; partial stdout discarded             |
| `read_wake_context` fails| `session_start` banner skipped; warning notify only            |

No error throws into the agent turn — every failure path returns a result envelope.

## Assumptions

- mpm is installed at `~/.mpm` and `mpm` is on `PATH`.
- The mpm call subprocess emits one zap-style log line to stderr and a
  single JSON object to stdout on success. The parser scans the last
  JSON-looking line — matches the existing `openclaw-mpm-memory` adapter.
- The Pi runtime is `>=0.84.x` (uses `pi.getSessionEntries` shape — soft, the
  call is gated; uses `pi.on("session_start", ...)` returning `{ message }`
  per docs/extensions.md).

## Verification

```bash
# 1. The extension loads
pi -e ~/.mpm/agent_plugins/pi-mpm --help   # no parse errors expected

# 2. Round-trip a memory
pi --mode json -e ~/.mpm/agent_plugins/pi-mpm \
   -p "Use mpm_remember to store 'pi-mpm smoke test' with tags ['pi-smoke'], then use mpm_recall to find it."

# 3. Wake context surfaces on session_start
pi --mode json -e ~/.mpm/agent_plugins/pi-mpm \
   -p "Read the MPM wake context banner at session start."
```

Manual sanity:

```bash
# Tool envelope shape (raw, for debugging)
mpm call query_long_term_memory --payload '{"query":"foo","limit":3}'
mpm call read_wake_context --payload '{}'
mpm call info --payload '{}'
```

## Repair guide

If `mpm` changes its CLI surface (e.g. renames `query_long_term_memory` →
`recall`):

1. Update the `callMpm(tool, payload, …)` call sites in `index.ts`.
2. The parser (`parseLastJsonLine`) is robust to additional stderr logs
   but assumes stdout is still a single JSON object — if mpm moves to
   NDJSON, replace the "last `{…}` line" heuristic with a JSONL consumer.
3. The `session_start` banner assumes `read_wake_context` returns
   `last_handoff` and `epistemic_pressure`; if either field is renamed,
   update the banner builder.

## Related mpm integrations in this repo

- `agent_plugins/openclaw-mpm-memory/` — the OpenClaw equivalent (memory slot plugin).
- `agent_plugins/mpm-auto-route/`     — per-turn persona auto-routing for OpenClaw.

Both follow the same `mpm call` subprocess pattern; this file is the Pi-shaped
adaptation of the same boundary.

## License

Same as mpm — AGPL-3.0. See `~/.mpm/LICENSE`.
