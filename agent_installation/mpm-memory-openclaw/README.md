# @openclaw/mpm-memory

MPM-backed memory slot for OpenClaw. Routes `memory_search` and `memory_get`
through MPM's FTS5 + reinforcement-weighted recall, and injects MPM wake context
into every OpenClaw session via OpenClaw's native hook API.

## What this plugin does

| Surface | Mechanism | Status |
|---------|-----------|--------|
| `memory_search` / `memory_get` | Subprocess → `mpm call mpm_memory` | ✅ Works |
| Wake context injection | `session_start` → `agent_turn_prepare` hooks | ✅ Implemented |
| Provenance env vars | `resolve_exec_env` hook | ✅ Implemented |
| `session_end` → work completion | Not implemented | ✅ Correct — intentional |
| Heartbeat context | `heartbeat_prompt_contribution` hook | ✅ Implemented |
| Memory capability slot | `api.registerMemoryCapability` | ✅ Works |
| Compaction flush | Neutered at sink | ✅ Correct — intentional |

## Install

```bash
# 0. Prerequisites: install MPM core (root scripts/install.sh) and OpenClaw.
#    The adapter installer below verifies both are present and will point
#    you at scripts/install.sh if MPM is missing.

# 1. Run the adapter installer (CWD-independent).
./install.sh
```

`./install.sh` is the canonical entry point and performs the full
OpenClaw-specific setup in the correct order:

```text
1. Locate MPM via canonical paths:
     $HOME/.mpm/bin/mpm            (canonical install root)
     $HOME/.local/bin/mpm          (canonical user symlink)
     command -v mpm                (last-resort PATH lookup)
   Bare `mpm` resolution is intentionally NOT the first check —
   on a freshly-created ~/.local/bin the current login session may
   not have it on PATH yet. The resolved binary is verified by
   absolute path before any plugin config is written.

2. Inspect existing plugin state via `openclaw plugins inspect --json`.
   The installer distinguishes three cases (verified against OpenClaw
   2026.9.4):
     absent              — plugin id not in the registry → fresh install.
     linked-from-here    — plugin id registered and rootDir equals
                           this adapter's directory → skip install step.
     conflicting         — plugin id registered but rootDir points
                           elsewhere → hard error, no overwrite.

3. Install (only when state was "absent"):
     openclaw plugins install <adapter-dir> --link --force --accept-capabilities
   The three flags are the documented 2026.9.4 contract for installing
   a non-ClawHub local source that declares capabilities (this plugin
   declares memory_search, memory_get). See "Trust / capability
   acknowledgement" below. The installer refuses to fall back to a
   non-link install if the link install fails — that would silently
   change the deployment topology and hide the real cause.

4. Enable the plugin entry:
     openclaw plugins enable mpm-memory-openclaw

5. Persist plugin config (always written together, on every rerun):
     config.mpmBin                                 = <absolute path to mpm>
     hooks.allowConversationAccess                 = true
     hooks.allowPromptInjection                    = true
   Both hook flags are required: `agent_turn_prepare` falls in
   OpenClaw's `promptInjectionHookNameSet` (needs
   `allowPromptInjection=true`) AND in `conversationHookNameSet`
   (needs `allowConversationAccess=true` for non-bundled plugins).
   Without both, the plugin runs but wake-context injection is
   silently blocked at register time.

6. Switch plugins.slots.memory to mpm-memory-openclaw.

7. Surface (not auto-apply) the recommended memory-core silence one-liner.

8. Bounded safe gateway lifecycle. Two probes, each bounded TWICE
   (outer `timeout` + the CLI's own --timeout where supported):
     openclaw gateway status --json --timeout <ms>     (RPC probe)
     openclaw gateway restart --safe                   (drain + restart)
   `--safe --wait` is INVALID in 2026.9.4 (the CLI help says
   "--wait ... not compatible with --force or --safe"). `--safe`
   already has bounded-wait semantics; the outer `timeout` is the
   hard cap. If the gateway is absent / unhealthy, the restart is
   skipped silently — config above is already persisted and will
   apply on the next gateway start.

9. Verify (openclaw plugins inspect + plugins list).

### Re-running (idempotency)

Re-running `./install.sh` on a host where the plugin is already
correctly linked from THIS adapter's path is genuinely idempotent for
the install step: no `openclaw plugins install` is reissued, no trust
warning is emitted, no `installedAt` timestamp is bumped. The config
writes (mpmBin + both hook flags + slot) ARE re-applied on every run,
so a fresh OpenClaw config is re-seeded correctly.

If the plugin is already registered but pointing at a different source
(state "conflicting"), the installer refuses with a clear operator
action and does not silently overwrite the unrelated source.

### Memory-core coexistence

`./install.sh` does **not** modify `plugins.entries.memory-core.enabled`.
That is an operator policy decision: some installs keep both the
stock memory-core and this plugin running and rely on
`plugins.slots.memory` to direct which one serves. The installer
prints the recommended one-liner at the end:

```bash
openclaw config set plugins.entries.memory-core.enabled false
```

### Trust / capability acknowledgement

OpenClaw 2026.9.4 distinguishes three orthogonal concerns at plugin
install time, and the installer handles each deliberately:

```text
A. Trust of the local source
   `--force` acknowledges the "non-ClawHub" trust gate. The CLI prints:
     WARNING - Installing plugin from local path: <path>
     This source is outside ClawHub review and trust metadata.
     Only continue if you trust the publisher, package contents, and
     install source.
   The installer passes `--force` automatically because this is a
   first-party adapter from the MPM repository, the operator is the
   publisher, and the source path is resolved from BASH_SOURCE[0] (not
   taken from argv).

B. Capability consent
   `--accept-capabilities` consents to the plugin's declared surface
   (memory_search, memory_get). Without this flag, 2026.9.4 returns
     "Plugin X requires capability consent. The plugin was not
      updated. Re-run the same `openclaw plugins install` or `openclaw
      plugins update` command with --accept-capabilities, keeping its
      source and other options."
   The installer passes `--accept-capabilities` automatically on a
   fresh install. For a re-run on an already-linked plugin, no install
   is issued at all, so this flag is not relevant.

C. Overwrite of an existing plugin
   `--force` ALSO means "overwrite an existing plugin or hook pack".
   The installer never uses that side-effect blindly: it inspects
   `plugins.entries.<id>.rootDir` first and only enters the install
   path on the "absent" branch. The "conflicting" branch fails closed
   with a clear operator action (uninstall or rename) — we never
   silently seize another installation.

If you are reviewing this adapter and want to audit the operator
action of a given install, the installer's full CLI transcript is
appended to `/tmp/mpm-memory-openclaw-install.log`.

## Wake Context Injection

When a session starts, the plugin fetches MPM's wake context via
`mpm call mpm_context --format=system-prompt` and injects it into the agent
prompt via the `agent_turn_prepare` hook (pre-pended as `prependContext`).

The flow:

```
session_start fires
    │
    ▼
async fetch of MPM wake context (non-blocking, 5000 ms ceiling)
    │
    ▼
agent_turn_prepare fires (next agent turn)
    │
    ├── await cached wake context promise (1500 ms race ceiling)
    │
    ▼
prependContext ← wake context injected into agent prompt
```

**First-turn fallback (lifecycle race).** If `agent_turn_prepare` fires
before `session_start` (or in any session where the cache is empty), the
plugin synchronously fetches wake context with the same race ceiling and
injects it. This eliminates the session_start race; subsequent turns
hit the cache.

For heartbeat turns, `heartbeat_prompt_contribution` injects a brief MPM status
note (first 300 chars of wake context) without duplicating the full context.

**This is the only delivery path for wake context.** OpenClaw does not
require any persistent MPM instruction block in `SOUL.md`, `AGENTS.md`,
or any other markdown file. The runtime hook chain — `session_start`
(starting the fetch) followed by `agent_turn_prepare` (awaiting and
returning the result as `prependContext`) — is what surfaces the wake
payload to the model. A regression guard in
`tests/runtime_injection.test.js` pins this contract.

### Per-session cache key

OpenClaw's `PluginAgentTurnPrepareEvent` payload carries
`{ prompt, messages, queuedInjections }` only — `sessionKey` is on the
hook *context* (`ctx.sessionKey`), not on the event. The plugin reads
from `ctx` first and falls back to `event.sessionKey`. This is what
makes `session_start` (which stores under the real sessionKey) and
`agent_turn_prepare` (which looks it up) match.

### OpenClaw automatic-memory-context path

The gateway has a separate automatic BOOTSTRAP.md / USER.md memory-context
pipeline that consults the chosen memory slot's runtime. The plugin's
runtime exposes `classifyWorkspaceMemoryPaths` (returning `[]` for the
MPM case) so the gateway stops excluding the slot for "missing
provenance classification". Without this method, the gateway logs
`excluding automatic memory context: selected memory runtime does not
support provenance classification` and skips the slot — independent of
the typed-hook wake path.

## Provenance

`resolve_exec_env` contributes these env vars to every `exec` tool call:

| Env var | Value | Source |
|---------|-------|--------|
| `MPM_PROVENANCE_FRAMEWORK` | `"openclaw"` | Fixed |
| `MPM_PROVENANCE_SESSION_KEY` | OpenClaw `sessionKey` | Hook context |

`MPM_PROVENANCE_MODEL` and `MPM_PROVENANCE_INVOCATION_ID` are intentionally
**not** set — OpenClaw's hook context does not expose model name or
invocation ID. These fields are left unset rather than fabricated.

## Handoff path on the compact MCP surface

The default 3-tool MCP surface (`mpm_memory`, `mpm_context`, `mpm_help`)
exposed via `mpm-mcp` does **not** include the substrate `mpm_handoff`
tool. To record a handoff from inside an OpenClaw session, call the
compact-surface path:

```text
mpm_context
  action=write_handoff
  params:
    summary      (required, non-empty)
    state        (optional: clean | crashed | interrupted | force_end)
    commitments  (optional: string[])
    open_questions (optional: string[])
    session_id   (optional)
```

The `mpm_context` MCP tool description advertises this action explicitly
("`write_handoff / read_handoff` (session continuity)"); the
`contextAdapter` closure in `cmd/mpm-mcp/tools.go` rewrites the
model-facing name to the substrate action (`write` / `read`) before
forwarding to the underlying `mpm_handoff` handler. **Do not** call
`mcp__mpm__mpm_handoff` — that tool is not on the default compact
surface and the call will be rejected. The substrate `mpm_handoff`
tool remains reachable only via the `mpm call mpm_handoff --payload
'{"action":"write","params":{"summary":"..."}}'` CLI escape hatch,
used when the MCP transport is unavailable or `MPM_EXPOSE_ALL_TOOLS=1`
is set in the MCP env block.

## `session_end` — No Work Completion

**Session end does not emit work completion.** This is intentional and correct.

The semantic contract:

```
Session ended       ≠ work completed
Work completed     ≠ work verified
```

Only an explicit agent action (`mpm call mpm_work --action complete`) emits
`WorkEventTypeClaimedComplete`. `session_end` is a lifecycle observation,
not an epistemic one. The agent may have been interrupted, hit a timeout,
or simply run out of context.

## Compaction Flush (Neutered at the Sink)

This plugin does **not** ingest OpenClaw's compaction flush output. The
`flushPlanResolver` returns a plan whose `relativePath` points at a
throwaway file (`local_flush_trash.md`) under the calling session's
`workspaceDir`. OpenClaw writes the file as part of its flush lifecycle —
satisfying its internal contract — but the file's content is discarded
unread.

**Why neuter the integration.** MPM already has two higher-fidelity
epistemic sources than OpenClaw's auto-generated transcript summary:

- **Scratchpad** — agent-curated Working Context, deliberate, ephemeral.
- **Memories** (`mpm_memory`) — explicit facts with tags, weight, and
  reinforcement, retrievable via FTS5 + hybrid scoring.

Capturing OpenClaw's lossy auto-summary would add a third memory surface
with lower fidelity. The integration boundary is deleted at the sink, not
the source. See decision `40544f5a04a2aac7` (2026-08-13) for full
rationale; this implements the *"Truth once. Views everywhere."* doctrine.

## Configuration

```json5
plugins: {
  entries: {
    "mpm-memory-openclaw": {
      enabled: true,
      config: {
        // ALWAYS use an absolute path here. The OpenClaw gateway runs as a
        // systemd --user service with a stripped PATH; the bare default
        // "mpm" (PATH-resolved) fails at runtime with `spawn mpm ENOENT`.
        // install.sh detects and writes the absolute path automatically
        // on this host — see the systemd gotcha note below.
        mpmBin: "$HOME/.mpm/bin/mpm",
        timeoutMs: 5000,           // subprocess timeout; default 5000
        scope: "all",              // "all" | "local" | "shared"; default "all"
        limitDefault: 6            // default memory_search limit; 1-50
      }
    }
  },
  slots: { memory: "mpm-memory-openclaw" }
}
```

### systemd / PATH-resolved gotcha

The OpenClaw gateway is launched by `systemd --user` and inherits a
deliberately minimal `PATH` (typically `/usr/local/bin:/usr/bin`). Even if
your interactive shell has `mpm` on PATH (e.g. via `~/.local/bin` from a
profile), the gateway's subprocess will not see it. This is by design — the
service-level PATH is independent of the user-shell PATH for predictability
and security.

**Symptom:** plugin logs `health_check failed — mpm mpm_system failed:
spawn mpm ENOENT` at gateway boot, and every `memory_search` /
`memory_get` returns `{disabled:true}`.

**Fix:** set `mpmBin` to the absolute path of the `mpm` binary:

```bash
openclaw config set plugins.entries.mpm-memory-openclaw.config.mpmBin "<absolute path to mpm>"
openclaw gateway restart --safe
```

The bundled `./install.sh` discovers MPM through the canonical install
paths (`$HOME/.mpm/bin/mpm` then `$HOME/.local/bin/mpm` then
`command -v mpm`) and writes the resolved absolute path into
`mpmBin` automatically. Fresh installs are unaffected. This hit was
filed and fixed in 0.1.3 (2026-09-02) after a clean reinstall on a host
where the interactive shell PATH differed from the systemd unit's PATH.

## Failure Modes (All Fail-Open)

| Condition | Behaviour |
|---|---|
| `mpm` not on PATH / binary not found | `memory_search` returns `{disabled:true, error:"...ENOENT..."}`; health-check log surfaces the config-set hint (added 0.1.3) |
| `mpm` exits non-zero | Tool result includes last 500 chars of stderr/stdout as `error` |
| Subprocess timeout | `error: "mpm ... timed out after Nms"` |
| MPM returns zero hits | `results: []`, `total: 0` — normal |
| `memory_get` on non-virtual path | `{notFound:true, supportedPrefix:"mpm://memory/"}` |
| `memory_get` for unknown id | `{notFound:true}` |
| Wake context fetch fails | Agent turn proceeds without wake context (graceful degradation) |

No error throws into the agent turn — every surface has a fail-open path.

## OpenClaw Limitations (Genuine — Not Fixable by this Plugin)

These are framework-level limitations of OpenClaw's plugin API, not bugs in
this plugin:

| Limitation | Impact |
|---|---|
| No `SessionStart`/`SessionEnd` equivalent hooks | ✅ **Resolved** — OpenClaw DOES have `session_start`/`session_end` hooks (discovered 2026-08-25). This plugin uses them. |
| OpenClaw does not expose `model` in hook context | `MPM_PROVENANCE_MODEL` not set |
| OpenClaw does not expose invocation ID in hook context | `MPM_PROVENANCE_INVOCATION_ID` not set |
| `session_end` is a lifecycle event, not epistemic | ✅ **Correct** — plugin does not emit work completion |

## Tool Schemas

Agent-facing tool schemas accurately declare parameters already supported and validated by the underlying handlers. This improves machine-readable discoverability — for code assist, type checking, and agentic tool-routing — without changing tool behaviour or API semantics.

## Result Shape

`memory_search` returns OpenClaw-shaped hits with virtual paths
(`mpm://memory/<id>`):

```json
{
  "results": [
    {
      "path": "mpm://memory/abc123",
      "startLine": 1,
      "endLine": 12,
      "score": 0.87,
      "snippet": "...full recalled content (truncated at 1200 chars)...",
      "source": "mpm",
      "collection": "memories",
      "tags": ["tag1", "tag2"],
      "weight": 87,
      "reinforcementCount": 4,
      "createdAt": "2026-07-30T08:14:05Z",
      "rank": 1
    }
  ],
  "backend": "mpm",
  "query": "...",
  "total": 1,
  "mpmScope": "all"
}
```

## Roadmap

- **mcp fast-path:** Spawn `mpm-mcp` once at plugin register time, route
  calls over MCP `tools/call` instead of per-call `mpm call` subprocess.
- **score fusion:** Combine MPM bm25 + reinforcement into a hybrid score.
- **memory_get by content:** Allow `path: "?query=foo bar"` for ad-hoc reads.

## License

Same as MPM. See `https://flowbyte.com/mpm`.
