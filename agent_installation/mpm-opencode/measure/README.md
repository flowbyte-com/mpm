# Session C — measurement harness

Measurement instruments for the OpenCode adapter and the MPM tool surface.
Everything here is **read-only with respect to MPM and the live host**: no
script in this directory writes to `~/.mpm`, to `~/.mpm-dogfood`, to the live
OpenCode configuration, to the live session store, or to the installed plugin
payload.

The measurements these scripts produced, and the conclusions drawn from them,
are in [`docs/dev/session-c-host-findings.md`](../../../docs/dev/session-c-host-findings.md).

## Layout

| Path | What it is |
|---|---|
| `measure_tool_surface.py` | Starts `mpm-mcp`, performs the MCP handshake, issues `tools/list`, and reports description/schema bytes and a token estimate for the compact (3-tool) and full (23-tool) surfaces. Calls no tool. |
| `fixture_run.sh` | Runs one prompt against an **isolated** OpenCode instance whose MPM plugin is pointed at a **disposable** MPM workspace, then reports the MPM tool calls the agent actually made, read from the substrate's own `tool_invocations` audit. |
| `probe/host_surface_probe.js` | An OpenCode v2 plugin that records the real `ctx` surface and the payloads of every hook the host fires. |
| `probe/project_identity_probe.js` | A minimal plugin that records `ctx.location` only — used to test whether the host's project id is stable across linked worktrees and across remote spellings. |
| `evidence/` | The raw captures the report cites. |

## Running the probes

Both probes are **directory plugins** for the OpenCode v2 loader: the loader
resolves a directory plugin by a root-level `server.js` (or `index.js`) and
never consults `package.json`. Copy a probe into its own directory with a
`server.js` that re-exports it:

```js
export { default } from "./host_surface_probe.js"
```

then point an isolated config at it:

```json
{ "plugins": ["file:///absolute/path/to/the/probe/dir"] }
```

and run OpenCode with **all four XDG roots redirected**, so the live
configuration, session store and cache are never touched:

```bash
export XDG_CONFIG_HOME=/tmp/probe/config
export XDG_DATA_HOME=/tmp/probe/data
export XDG_STATE_HOME=/tmp/probe/state
export XDG_CACHE_HOME=/tmp/probe/cache
export OPENCODE_CONFIG_DIR=/tmp/probe/config/opencode
export OPENCODE_DISABLE_AUTOUPDATE=1
opencode debug paths    # verify isolation before doing anything else
```

> **Never register a bogus name with `ctx.tool.hook`.** Its dispatcher
> implements only `execute.before` and `execute.after`, and the lookup
> happens when the hook *fires* — so a typo registers silently and then
> throws a `TypeError` the first time any tool executes in that process.
> `ctx.session.hook` is safe (it accepts any name and simply never fires it).

## Running the tool-surface measurement

```bash
export MPM_WORKSPACE="$HOME/.mpm-dogfood"
export MPM_DB_PATH="$HOME/.mpm-dogfood/src/db/mpm.db"
python3 agent_installation/mpm-opencode/measure/measure_tool_surface.py \
  --mcp "$HOME/src/mpm/.build/bin/mpm-mcp" \
  --json /tmp/tool_surface.json
```

Expected output (dev build `0.1.0-dev+g70deac2b`):

```
compact :   3 tools | desc   1142 B | surface   2244 B | ~  561 tok
full    :  23 tools | desc  18755 B | surface  43947 B | ~10987 tok
delta   :  20 tools |  41703 B | ~10426 tok | ratio 19.58x
```

The token figures use a `bytes/4` approximation, not a tokenizer. They are
comparative, not absolute.

## Running a behavioural fixture

```bash
agent_installation/mpm-opencode/measure/fixture_run.sh <label> "<prompt>"
```

The runner pins `MPM_WORKSPACE` and `MPM_DB_PATH` to a **disposable**
workspace under `/tmp/opencode/probe-c/mpm-scratch` and loads the
**installed** plugin payload read-only. It prints the agent's stdout and the
resulting `tool_invocations` rows.

Seed the scratch workspace first, for example:

```bash
export MPM_WORKSPACE=/tmp/opencode/probe-c/mpm-scratch
mpm call mpm_memory --payload '{"action":"save","params":{"fact":"…","collection":"decisions"}}'
mpm call mpm_lessons --payload '{"action":"save","params":{"fact":"…","type":"warning"}}'
mpm call mpm_work   --payload '{"action":"create","params":{"title":"…","description":"…"}}'
```

## Evidence files

| File | Contents |
|---|---|
| `evidence/host_surface_probe_run.jsonl` | The complete probe capture: `ctx` shape, every namespace, every fired hook with its payload, and the tool-execution context. |
| `evidence/project_identity_remotes.jsonl` | `ctx.location.project.id` for seven repositories with different `remote.origin.url` spellings — the evidence for "the host's project id is the git remote, normalised". |
| `evidence/project_identity_noremote.jsonl` | The same probe for a repository with no remote and for two plain directories — the evidence for the one place the host's id diverges from the contract's §3.3. |
| `evidence/tool_surface_measurement.json` | The raw output of `measure_tool_surface.py`, including every tool's description and byte count. |
