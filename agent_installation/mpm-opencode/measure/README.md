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
| `wake_context_e2e.sh` | End-to-end proof for the wake-context fix. Runs an isolated host with this repository's plugin plus `probe/wake_context_probe.js`, against a disposable MPM workspace seeded with a unique marker, and reports whether the marker reached a system-prompt part. Pass a deliberately defective plugin directory as the second argument for a negative control. |
| `p4_lifecycle_e2e.sh` | End-to-end check for the P4 lifecycle observer. Runs `scripts/dev_host.sh` twice — unconfigured, then with `MPM_EVENT_INGEST=mpm_system` — and asserts the envelopes validate, contain no tool/prompt content, that the ingest endpoint's receipts are valid, and that the delivery accounting closes at every reading. Writes `*.envelopes.jsonl` under its run root. |
| `p4_delivery_accounting.sh` | The delivery-accounting invariant against a **real** disposable MPM event store. Drives the real transport (`mpm call mpm_system` → `event_ingest`) with `p4_accounting_harness.mjs` and compares the client's `delivered` count with the core's own stored count. Two runs: drain-all, and stop-in-flight — the latter reproduces the reported case (10 stored, 9 reported) and shows the tenth event accounted for as in flight. Requires `MPM_BINARY`; refuses to run against the live workspace. |
| `fixture_run.sh` | Runs one prompt against an **isolated** OpenCode instance whose MPM plugin is pointed at a **disposable** MPM workspace, then reports the MPM tool calls the agent actually made, read from the substrate's own `tool_invocations` audit. |
| `probe/host_surface_probe.js` | An OpenCode v2 plugin that records the real `ctx` surface and the payloads of every hook the host fires. **Note:** its `ctx.event.subscribe(callback)` call is *wrong* and produced the first pass's false "inert stream" conclusion — kept as-is because the error is instructive. Use `event_stream_probe.js` instead. |
| `probe/event_stream_probe.js` | **Corrected** event-stream probe: registers the callback form (to show it yields nothing) *and* consumes the real `AsyncIterable`, recording every delivered event type, its `data` keys and a bounded payload. Also exercises `ctx.session.compact` so both halves of the compaction pair are captured. |
| `probe/wake_context_probe.js` | Witness plugin for `wake_context_e2e.sh`. Records the system-prompt parts visible to a `context` hook, both in registration order and by array reference at teardown. |
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

> **A probe must export `{ id, setup }`** (or use `Plugin.define`). A
> function-shaped default export is rejected by the v2 loader:
> `Plugin must export a default definition with an id and an effect or setup
> function`. Always grep the host log for `failed to load plugin` before
> trusting a capture that contains no events — "the probe saw nothing" and
> "the probe never loaded" look identical in an empty log file.

> **`ctx.event.subscribe` returns an `AsyncIterable`, not a callback
> registration.** Consume it with `for await (const ev of
> ctx.event.subscribe({ signal }))`. Passing a function as the first argument
> silently returns a stream that is never read.

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
