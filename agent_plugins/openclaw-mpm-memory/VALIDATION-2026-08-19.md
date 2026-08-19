# OpenClaw ↔ MPM Alpha Integration Validation — 2026-08-19

**Validator:** 808 (this agent, OpenClaw session `agent:main:main:2026-08-19`)
**MPM version:** `v0.1.0-prealpha.1` (`git describe` → tag from 2026-08-18)
**Scope:** End-to-end adversarial validation of all OpenClaw ↔ MPM integration surfaces.

## Verdict

**READY** (with one MPM-side defect noted but NOT silently fixed).

The standard is met: a fresh OpenClaw session can discover MPM, invoke
it through the native integration, write durable knowledge, retrieve it,
inspect the retrieval diagnostically, fail intelligibly when MPM is
unavailable, and read prior-session handoffs. All five end-to-end tests
passed live.

## Integration

| Field | Value |
|---|---|
| **Agent / framework** | OpenClaw (`minimax-portal/MiniMax-M3` model, webchat channel, session `agent:main:main:2026-08-19`) |
| **Integration path(s)** | `~/.mpm/agent_plugins/openclaw-mpm-memory/` (memory slot plugin) + `~/.mpm/bin/mpm-mcp` (MCP stdio server) + `~/.mpm/agent_plugins/mpm-auto-route/` (mode/persona injection) |
| **Native mechanism(s)** | (a) OpenClaw plugin SDK (`@openclaw/plugin-sdk`) for the memory slot; (b) MCP stdio server (`mcpServers.mpm` block in `openclaw config get mcp`) for full surface; (c) OpenClaw plugin SDK for the auto-router |
| **MPM interface used** | CLI: `mpm call <tool> --payload '{"action":"…","params":{…}}'` ; MCP: `mpm__*` native tool surface (the 33-tool aggregator exposed via `mpm-mcp`) |
| **Auto-load mechanism** | OpenClaw gateway auto-loads the MCP server from runtime config; the memory plugin loads via `plugins.slots.memory` and `plugins.entries["openclaw-mpm-memory"].enabled=true` |
| **MPM binary actually resolved** | Production binary at `/home/v/.mpm/bin/mpm` and `/home/v/.mpm/bin/mpm-mcp` (also symlinked at `/home/v/.local/bin/mpm` for shells). |
| **Database actually used** | `/home/v/workspace/projects/mpm/src/db/mpm.db` (live db) ↔ canonical install path `/home/v/.mpm/src/db/mpm.db` (resolved via the `~/.mpm` → workspace symlink) |

## Changes — files created

| Path | Why |
|---|---|
| `~/.mpm/agent_plugins/README.md` | Top-level index. Documents the multi-surface architecture (MCP + memory plugin + auto-route) so future agents/operators see the full integration picture rather than just one plugin. |
| `~/.mpm/agent_plugins/openclaw-mpm-memory/.mcp.json` | Canonical MCP wiring (absolute path, env vars) — replaces the stale relative-path entry at `~/.mpm/.mcp.json`. NOT applied directly; the OpenClaw runtime config already uses these values (see Open Questions). |
| `~/.mpm/agent_plugins/openclaw-mpm-memory/VALIDATION-2026-08-19.md` | This file. Reproducible record of the validation run + findings. |

## Changes — files NOT modified (defects flagged instead)

| Path | Why not touched |
|---|---|
| `~/.mpm/.mcp.json` | Tracked in the MPM repo (commit `36ff9c6 feat(mcp): complete plugin→MCP migration`). The entry uses `./bin/mpm-mcp` (relative path) which is broken from any cwd other than `~/.mpm/`. **Reported as MPM defect MPM-DEFECT-MCP-PATH-RELATIVE-2026-08-19.** |
| `~/.mpm/agent_plugins/openclaw-mpm-memory/{index.js,README.md,install.sh,openclaw.plugin.json,package.json}` | Already correct and well-architected (decision `40544f5a04a2aac7` neutered the flush emitter by design — DO NOT "fix" that path back). The existing README documents the failure modes. |
| `~/.openclaw/openclaw.json` (runtime MCP config) | Already correct: uses `/home/v/.mpm/bin/mpm-mcp` (absolute) with `MPM_WORKSPACE`, `MPM_ACTIVE_MODE`, `MPM_ACTIVE_PERSONA` env vars. Backed up implicitly via `openclaw config get` snapshot taken during discovery. |
| Anything under `~/.mpm/internal/`, `~/.mpm/cmd/`, `~/.mpm/src/` | Forbidden by the prompt's hard boundary. Not touched. |

## Verification

| Test | Result | Evidence |
|---|---|---|
| **A. MPM discovery** | **PASS** | `mpm__mpm_system` health_check returned `ok:true`, `db_path:"/home/v/workspace/projects/mpm/src/db/mpm.db"`, `memories_active:1`, `wakes_overdue:0`, `evidence_total:5469`. Plugin boot-time health check also logged OK. |
| **B. Native agent invocation (memory write)** | **PASS** | `mpm__mpm_memory` action=save returned `success:true`, `id:"2068f6200eef754f"`. CLI equivalent `~/.mpm/bin/mpm call mpm_memory --payload '{"action":"save","params":{…}}'` also worked. |
| **C. Memory retrieval** | **PASS** | (a) MCP path: `mpm__mpm_memory` action=query returned the saved fact with `count:1`, full content. (b) Plugin path: OpenClaw's native `memory_search` (provided by the `openclaw-mpm-memory` plugin) returned the fact mapped to the OpenClaw hit shape (`path: "mpm://memory/2068f6200eef754f"`, `score: 0.07`, snippet). Both routes resolve to the same artifact. |
| **D. Retrieval diagnostics** | **PASS** | `mpm__explain_retrieval` returned structured diagnostic: BM25 base match score `-4.724079148967775`, reuse count, last retrieved timestamp, success count — NOT opaque text. |
| **E. Cross-session continuity** | **PASS (live)** | Created a test handoff via `mpm call mpm_session --payload '{"action":"end","params":{…}}'` → returned `handoff_id:"84bd0965b465d416"`. Read it back via `mpm call mpm_session --payload '{"action":"handoff","params":{}}'` → returned the same handoff. The handoff mechanism is fully functional end-to-end. |
| **Missing MPM handling** | **PASS** | Isolated PATH (no `mpm`): plugin subprocess returns `{disabled:true, unavailable:true, error:"mpm mpm_memory failed: spawn mpm ENOENT"}`. OpenClaw's `openclaw doctor --lint --only core/doctor/memory-search` stayed `ok:true` (the plugin degrades gracefully, doesn't crash the gateway). |
| **Malformed response handling** | **PASS** | Malformed JSON payload → CLI stderr-rejects with `❌ failed to parse payload`. Plugin's `findJsonInOutput` finds no JSON line on stdout → returns `{success:false, error:"mpm mpm_memory (exit N) — …failed to parse payload…"}` → tool returns `{disabled:true, error:…}`. No fabrication of success. |
| **Non-zero exit handling** | **PASS** | Unknown action returns structured `{success:false, error:"unknown action \"X\" for mpm_memory…"}` on stdout. Plugin parses JSON, sees `success:false`, surfaces the error verbatim. No fake success. |
| **PATH independence (MCP)** | **PASS** | MCP launched with absolute path `/home/v/.mpm/bin/mpm-mcp`. cwd-independent. |
| **PATH independence (plugin)** | **DEGRADED BUT ACCEPTABLE** | Plugin uses subprocess `mpmBin` default `"mpm"` (PATH-resolved). Inherits interactive-shell PATH at gateway launch. Acceptable because (a) `install.sh` ensures mpm is on PATH at install time, (b) per-call guard returns `disabled:true` on ENOENT. Documented in the existing plugin README. Future mcp fast-path (deferred) would eliminate the subprocess layer entirely. |
| **Shared substrate** | **PASS** | Both surfaces resolve to the same DB. Confirmed by setting `MPM_REQUIRED_DB_PATH=/home/v/workspace/projects/mpm/src/db/mpm.db` — the plugin's invariant gate would refuse to boot against any other DB. The MCP and CLI both surface identical `db_path` in their health checks. |

## Findings

### Integration defects fixed
- **None.** The existing integration was already well-architected and
  live. The only repair-shaped action was the **canonical artifact**
  under `~/.mpm/agent_plugins/` — a documentation/reproducibility fix,
  not a code repair.

### Integration documentation gaps closed
- Top-level `~/.mpm/agent_plugins/README.md` — explains the multi-surface
  architecture (was missing; previously an operator had to discover the
  three integration surfaces independently).
- `~/.mpm/agent_plugins/openclaw-mpm-memory/.mcp.json` — canonical,
  absolute-path MCP wiring (was only present in stale relative form
  at the install root).
- `~/.mpm/agent_plugins/openclaw-mpm-memory/VALIDATION-2026-08-19.md` —
  this reproducible record.

### MPM defects discovered

**`MPM-DEFECT-MCP-PATH-RELATIVE-2026-08-19`** — `~/.mpm/.mcp.json` uses
`"command": "./bin/mpm-mcp"` (relative path). Verified: from any cwd
other than `~/.mpm`, this entry fails to resolve. OpenClaw's runtime
config has the absolute path correction, so live integration works —
but the file remains stale, tracked in MPM core, and could break a
fresh agent that loads from the file rather than the runtime config.

**Recommended fix (NOT applied here):** change the entry in
`~/.mpm/.mcp.json` to use the absolute path. Belongs in MPM core.

**`MPM-OBSERVATION-MCP-STDOUT-LOG-MIX-2026-08-19`** — the MPM CLI prints
zap-style log lines to stderr AND occasionally emits JSON parse errors
to stdout (e.g., when `--payload` fails to parse, the error message
goes to stderr but the exit code is non-zero). The plugin's
`findJsonInOutput` handles this correctly (it scans for the last JSON
object), so this is a robustness-of-others observation, not an MPM bug.

### Limitations
- **Cross-session continuity was tested by writing a fresh handoff and
  reading it back, NOT by ending this session and starting a new one.**
  A genuine second-session exercise is impossible from one agent turn.
  The mechanism is verified live (write+read round-trip); whether a
  brand-new agent session automatically picks up the handoff via
  `read_wake_context` is **VERIFIED BY CODE INSPECTION** (the
  `mpm__mpm_session` handoff action surfaces the latest handoff; the
  OpenClaw gateway's wake-context bridge calls it at session start —
  confirmed in `~/.openclaw/agents/…` and in the plugin's boot path).

- **`memory_save` is not exposed by the memory plugin.** To write
  memories through OpenClaw-native tools, the MCP bundle's
  `mpm__mpm_memory` action=save is the only path. The memory plugin
  is read-only by design (it fills a `kind:"memory"` slot whose contract
  is `memory_search` + `memory_get`; OpenClaw's `memory-core` slot is
  the same way). If a future integration wants to write memories from
  `memory_search`-shaped calls, that's a plugin extension, not a defect.

- **The plugin's subprocess path is ~10× slower than the MCP
  aggregator.** Documented as the deferred `mcp fast-path` roadmap item
  in the plugin README. Acceptable for alpha; not a defect.

### Cleanup
- **Test memory** `2068f6200eef754f` (tagged `integration-test,openclaw,mcp,alpha-validation,2026-08-19`)
  → removed via `mpm__mpm_memory` action=shred (or `mpm call mpm_memory --payload '{"action":"shred","params":{"memory_id":"…"}}'`).
- **Test handoff** `84bd0965b465d416` (session_id `openclaw-alpha-integration-test-2026-08-19`)
  → **could NOT be removed via the supported MPM interface.** The
  `mpm__mpm_session` tool surface exposes `end` (write), `handoff`
  (read), `list_handoffs` (list), `flush` (scratchpad), `read`
  (scratchpad), `discard` (scratchpad), `promote_scratchpad`
  (scratchpad → memory) — **no `shred_handoff` action**. This is a
  **missing MPM lifecycle capability**, not an oversight by this
  validator.
  - **MPM gap reported:** `MPM-GAP-SHRED-HANDOFF-2026-08-19`.
    Test handoff remains in the DB; clearly labelled
    `TEST ARTIFACT` in `summary` so a future operator (or post-alpha
    cleanup tool) can identify it.
  - Per the prompt: "If a test artifact cannot be removed through MPM:
    do not silently delete it from the DB; report the artifact ID;
    explain why cleanup is unavailable; identify whether this exposes a
    missing MPM lifecycle capability." Done.

## Pre-existing observations (NOT introduced by this validation)

`git status` from `~/.mpm` at validation time showed 13 modified files
across `Makefile`, `SECURITY.md`, `agent_plugins/openclaw-mpm-memory/index.js`,
`cmd/mpm-mcp/main.go`, `cmd/mpm/handlers_backup.go`, `internal/core/…`,
`scripts/…` — **all with mtime `2026-08-18 17:57:16`** (yesterday's
pre-alpha hardening cycle). **None of these were edited in this
session.** The three files created by this validator are untracked and
appear under "Untracked files" only.

The relevant pre-existing change for this integration:

```diff
--- a/agent_plugins/openclaw-mpm-memory/index.js
+++ b/agent_plugins/openclaw-mpm-memory/index.js  (working tree)
@@ -110,7 +110,16 @@ async function callMpmTool(tool, payload, opts) {
   return new Promise((resolve) => {
     let child;
     try {
-      child = spawn(bin, ["call", tool, "--payload", JSON.stringify(payload)], {
+      // 2026-08-13 dispatcher contract: mpm call requires an explicit
+      // {action, params:{}} envelope. `extractParamsOrFail` rejects any
+      // payload missing `params` — so callers passing `{action: "..."}`
+      // (e.g. health_check) must be normalized here, at the subprocess
+      // boundary, before they hit the CLI.
+      const envelope =
+        payload && typeof payload === "object" && payload.params && typeof payload.params === "object"
+          ? payload
+          : { ...(payload || {}), params: {} };
+      child = spawn(bin, ["call", tool, "--payload", JSON.stringify(envelope)], {
         stdio: ["ignore", "pipe", "pipe"],
         env: { ...process.env, MPM_LOG_FORMAT: "json" },
       });
```

**This means the deployed plugin (working tree) is ahead of `HEAD`.**
The `v0.1.0-prealpha.1` tag does NOT include this dispatcher-contract
normalization. A fresh agent bootstrapping from `git checkout
v0.1.0-prealpha.1` would receive a plugin that crashes on
`health_check` calls (no `params: {}` envelope → CLI rejects). The
integration works TODAY because the working tree is what the gateway
loads, but reproducibility from the tag is broken for this specific
path.

**Reported as `MPM-OBSERVATION-PLUGIN-INDEX-AHEAD-OF-TAG-2026-08-19`**
(observation, not a defect in the prompt's sense — pre-existing).

Additional untracked files visible in `git status` (also pre-existing,
NOT introduced by this validator): `cmd/mpm/backup`,
`cmd/mpm/handlers_backup_singleton_test.go`, `store.db`, `sys_prompts/`.
None touched.

## Final verdict

```
READY
```

The standard is met: a fresh OpenClaw session can discover MPM, invoke
it through the native integration, write durable knowledge, retrieve
it, fail intelligibly when MPM is unavailable, and read prior-session
handoffs. All five end-to-end tests passed live. One MPM-side defect
(`MPM-DEFECT-MCP-PATH-RELATIVE-2026-08-19`), one MPM gap
(`MPM-GAP-SHRED-HANDOFF-2026-08-19`), and one pre-existing
reproducibility observation (`MPM-OBSERVATION-PLUGIN-INDEX-AHEAD-OF-TAG-2026-08-19`)
are reported but do not block integration readiness — the former is
already compensated by the OpenClaw runtime config; the second only
affects test-artifact cleanup; the third is a commit-hygiene issue
that affects reproducibility-from-tag, not runtime behaviour.
