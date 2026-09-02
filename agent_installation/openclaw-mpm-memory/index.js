// @openclaw/mpm-memory — Memory slot backed by MPM.
//
// Thin transport adapter: the agent's memory_search/memory_get tool calls
// shell out to `mpm call mpm_memory` (the aggregator MCP tool) with an
// action-dispatch payload. The 33-tool → 13-aggregator schema collapse
// (2026-08-11) lets us keep this adapter as ONE call-shape instead of
// a fan-out across granular tool names. Result shape is mapped onto
// OpenClaw's expected MemorySearchResult contract (path, startLine,
// endLine, score, snippet). Hit paths are virtual (mpm://memory/<id>) —
// the full recalled content lives in the snippet so memory_get only matters
// when an agent needs the body of a specific id, in which case we
// re-query with the id.
//
// Boot-time health check: register() pings `mpm call mpm_system` with
// action:health_check so a missing/broken mpm fails loudly at OpenClaw
// boot (where the operator can see and fix it) rather than silently
// returning disabled:true during reasoning turns.
//
// Fail-open: per-call failures still return jsonResult({disabled:true})
// rather than throwing, so a transient backend blip doesn't kill the
// turn. The boot-time check is the only hard-fail path.
//
// Cost: one subprocess per memory_search (~50ms cold). The MCP
// aggregator path (mcp.servers.mpm) is the long-term fast-path — see
// README.
//
// --------------------------------------------------------------------------
// WAKE CONTEXT INJECTION (OpenClaw parity with Claude Code SessionStart)
// --------------------------------------------------------------------------
//
// OpenClaw fires `session_start` before the first agent turn in every session
// (new or resumed). The `agent_turn_prepare` hook runs after prompt prep and
// receives queued injections from that session. We chain them:
//
//   session_start
//       │
//       ▼ (async, non-blocking)
//   fetch wake context via `mpm call mpm_context --format=system-prompt`
//   store pending promise keyed by sessionKey
//       │
//       ▼ (next agent turn)
//   agent_turn_prepare
//       │
//       ├── await the pending promise
//       │   (may already be resolved if session_start finished fast enough)
//       │
//       ▼
//   prependContext ← wake context injected into agent prompt
//
// Heartbeat turns use `heartbeat_prompt_contribution` instead, which fires only
// for background monitor/lifecycle turns and does not interfere with user turns.
//
// session_end does NOT emit work completion. Session ended ≠ work verified.
// Work completion requires an explicit agent action (mpm call mpm_work --complete).
//
// --------------------------------------------------------------------------
// PROVENANCE
// --------------------------------------------------------------------------
//
// `resolve_exec_env` contributes MPM_PROVENANCE_* env vars to every exec tool
// call, providing framework attribution for MPM's audit trail:
//
//   MPM_PROVENANCE_FRAMEWORK=openclaw
//   MPM_PROVENANCE_SESSION_KEY=<current sessionKey, if available>
//
// OpenClaw does not expose model name or invocation ID in the hook context,
// so those fields are left unset (never fabricated).
//
// --------------------------------------------------------------------------

import { spawn, spawnSync } from "node:child_process";
import { definePluginEntry } from "openclaw/plugin-sdk/plugin-entry";
import { jsonResult } from "openclaw/plugin-sdk/tool-results";
import { withWorkspace } from "./lib/workspace.js";

const MP_MEMORY_PATH_PREFIX = "mpm://memory/";
const PLUGIN_ID = "openclaw-mpm-memory";
const MPM_FRAMEWORK_ID = "openclaw";

// --------------------------------------------------------------------------
// Per-session wake context cache
// --------------------------------------------------------------------------
// Map<sessionKey, Promise<string>> — keyed by OpenClaw sessionKey.
// The promise resolves to the formatted wake context string (or "" on failure).
// agent_turn_prepare awaits it; session_end clears it.

/** @type {Map<string, Promise<string>>} */
const wakeContextCache = new Map();

function sessionKeyFor(event) {
  return event?.sessionKey || event?.session?.sessionKey || "default";
}

// --------------------------------------------------------------------------
// Subprocess adapter
// --------------------------------------------------------------------------

function findJsonInOutput(s) {
  if (!s) return null;
  // mpm logs to stderr in zap format, prints JSON envelope to stdout on success.
  // On failure, stdout may be a human-readable error line — find the last line
  // that parses as a JSON object.
  const lines = s.split("\n");
  for (let i = lines.length - 1; i >= 0; i--) {
    const t = lines[i].trim();
    if (t.startsWith("{") && t.endsWith("}")) return t;
  }
  return null;
}

/**
 * Call an MPM tool subprocess.
 * @param {string} tool
 * @param {object} payload  — mpm call envelope {action, params}
 * @param {object} opts
 * @param {string} opts.mpmBin
 * @param {number} opts.timeoutMs
 * @returns {Promise<object>}
 */
async function callMpmTool(tool, payload, opts) {
  const bin = opts.mpmBin;
  const timeoutMs = opts.timeoutMs;
  return new Promise((resolve) => {
    let child;
    try {
      const envelope =
        payload && typeof payload === "object" && payload.params && typeof payload.params === "object"
          ? payload
          : { ...(payload || {}), params: {} };
      child = spawn(bin, ["call", tool, "--payload", JSON.stringify(envelope)], {
        stdio: ["ignore", "pipe", "pipe"],
        env: withWorkspace({ MPM_LOG_FORMAT: "json" }),
      });
    } catch (e) {
      resolve({ success: false, error: `spawn failed: ${e.message}` });
      return;
    }
    let stdout = "";
    let stderr = "";
    const timer = setTimeout(() => {
      try { child.kill("SIGKILL"); } catch {}
      resolve({ success: false, error: `mpm ${tool} timed out after ${timeoutMs}ms`, _timedOut: true });
    }, timeoutMs);
    child.stdout.on("data", (d) => (stdout += d));
    child.stderr.on("data", (d) => (stderr += d));
    child.on("error", (e) => {
      clearTimeout(timer);
      resolve({ success: false, error: `mpm ${tool} failed: ${e.message}` });
    });
    child.on("close", (code) => {
      clearTimeout(timer);
      const jsonLine = findJsonInOutput(stdout);
      if (jsonLine) {
        try {
          const parsed = JSON.parse(jsonLine);
          resolve(parsed);
          return;
        } catch {
          // fall through to error envelope
        }
      }
      const hint =
        code === null ? "(no exit — likely signal)" :
        code !== 0 ? `(exit ${code})` :
        "";
      const tail =
        (stderr || stdout || "").trim().split("\n").slice(-3).join(" | ").slice(0, 500);
      resolve({
        success: false,
        error: `mpm ${tool} ${hint} — ${tail || "no output"}`.trim(),
      });
    });
  });
}

/**
 * Fetch MPM wake context formatted for system-prompt injection.
 * Returns "" on failure so the plugin never blocks an agent turn.
 *
 * @param {object} opts
 * @param {string} opts.mpmBin
 * @param {number} opts.timeoutMs
 * @param {string} opts.frameworkId  — provenance framework identifier
 * @param {string} [opts.sessionKey] — OpenClaw sessionKey for provenance
 * @returns {Promise<string>}
 */
async function fetchWakeContext(opts) {
  const { mpmBin, timeoutMs, frameworkId, sessionKey } = opts;
  const env = {
    ...process.env,
    MPM_LOG_FORMAT: "json",
    MPM_PROVENANCE_FRAMEWORK: frameworkId,
  };
  if (sessionKey) env.MPM_PROVENANCE_SESSION_KEY = sessionKey;

  return new Promise((resolve) => {
    let child;
    try {
      child = spawn(mpmBin, [
        "call", "mpm_context",
        "--payload", JSON.stringify({
          action: "read_wake_context",
          params: { format: "system-prompt" },
        }),
      ], {
        stdio: ["ignore", "pipe", "pipe"],
        env: withWorkspace(env),
      });
    } catch (e) {
      resolve("");
      return;
    }
    let stdout = "";
    const timer = setTimeout(() => {
      try { child.kill("SIGKILL"); } catch {}
      resolve("");
    }, timeoutMs);
    child.stdout.on("data", (d) => (stdout += d));
    child.on("error", () => { clearTimeout(timer); resolve(""); });
    child.on("close", () => {
      clearTimeout(timer);
      const jsonLine = findJsonInOutput(stdout);
      if (jsonLine) {
        try {
          const parsed = JSON.parse(jsonLine);
          // MPM returns {success: true, content: "..."} or {success: false, error: "..."}
          if (parsed && parsed.success && typeof parsed.content === "string") {
            resolve(parsed.content);
            return;
          }
        } catch { /* fall through */ }
      }
      resolve("");
    });
  });
}

// --------------------------------------------------------------------------
// Result adapter — MPM memories → OpenClaw memory_search hits
// --------------------------------------------------------------------------

function adaptMemoryHit(memory, idx) {
  const id = memory.id || `unknown-${idx}`;
  const content = memory.content || memory.text || memory.snippet || "";
  const lines = content ? content.split("\n").length : 1;
  // MPM weights are 0–100 (legacy float scale × 10). Map to 0–1 for score.
  const score =
    typeof memory.weight === "number"
      ? Math.max(0, Math.min(1, memory.weight / 100))
      : 0.5;
  return {
    path: `${MP_MEMORY_PATH_PREFIX}${id}`,
    startLine: 1,
    endLine: Math.max(1, lines),
    score,
    snippet: content.slice(0, 1200),
    source: "mpm",
    collection: memory.collection || "memories",
    tags: memory.tags || [],
    weight: memory.weight,
    reinforcementCount: memory.reinforcement_count,
    createdAt: memory.created_at,
    rank: idx + 1,
  };
}

// --------------------------------------------------------------------------
// OpenClaw plugin tool parameter schemas
// --------------------------------------------------------------------------

const MemorySearchSchema = {
  type: "object",
  additionalProperties: false,
  required: ["query"],
  properties: {
    query: {
      type: "string",
      description:
        "Free-text query for MPM's hybrid FTS5 + reinforcement-weighted recall.",
    },
    maxResults: {
      type: "integer",
      minimum: 1,
      maximum: 50,
      description: "Cap on results returned. Default 6.",
    },
    minScore: {
      type: "number",
      description: "Optional relevance floor; MPM omits weaker hits.",
    },
    corpus: {
      type: "string",
      description:
        "Accepted for OpenClaw schema parity; MPM has no corpus split. Always treated as 'memory'.",
    },
  },
};

const MemoryGetSchema = {
  type: "object",
  additionalProperties: false,
  required: ["path"],
  properties: {
    path: {
      type: "string",
      description:
        "A path returned by memory_search. Currently only mpm://memory/<id> is supported.",
    },
    from: {
      type: "integer",
      description: "Accepted for parity; ignored — full body returned.",
    },
    lines: {
      type: "integer",
      description: "Accepted for parity; ignored — full body returned.",
    },
    corpus: {
      type: "string",
      description: "Accepted for parity; ignored.",
    },
  },
};

// --------------------------------------------------------------------------
// Plugin entry
// --------------------------------------------------------------------------

export default definePluginEntry({
  id: PLUGIN_ID,
  name: "MPM Memory",
  description:
    "Memory slot backed by MPM. Routes memory_search/memory_get through `mpm call mpm_memory` (aggregator tool, action:query). Injects MPM wake context at session start via OpenClaw hooks. FTS5 lexical + reinforcement-weighted recall. Replaces the default memory-core plugin (semantic vector recall is not provided by MPM in this adapter).",
  kind: "memory",
  enabledByDefault: false,

  register(api) {
    const log = api.logger ?? console;
    const entryConfig =
      api?.config?.plugins?.entries?.[PLUGIN_ID]?.config ?? {};
    const mpmBin = typeof entryConfig.mpmBin === "string" ? entryConfig.mpmBin : "mpm";
    const timeoutMs =
      typeof entryConfig.timeoutMs === "number" ? entryConfig.timeoutMs : 5000;
    const scope = typeof entryConfig.scope === "string" ? entryConfig.scope : "all";
    const limitDefault =
      typeof entryConfig.limitDefault === "number" ? entryConfig.limitDefault : 6;

    if (typeof log.info === "function") {
      log.info(
        `openclaw-mpm-memory: registered ` +
        `(mpmBin=${mpmBin}, scope=${scope}, timeout=${timeoutMs}ms, limit=${limitDefault})`
      );
    }

    // ------------------------------------------------------------------
    // Boot-time health check
    // ------------------------------------------------------------------
    (async () => {
      try {
        const hc = await callMpmTool("mpm_system", { action: "health_check" }, {
          mpmBin,
          timeoutMs: 2000,
        });
        if (hc && hc.ok === true) {
          const required = process.env.MPM_REQUIRED_DB_PATH;
          if (required && typeof required === "string" && required.length > 0) {
            const live = (hc && typeof hc.db_path === "string") ? hc.db_path : null;
            if (!live) {
              if (typeof log.error === "function") {
                log.error(
                  "openclaw-mpm-memory: refusing to boot — MPM_REQUIRED_DB_PATH is set " +
                  "but health_check did not surface db_path. The mpm server is too old to be gated."
                );
              }
              throw new Error("openclaw-mpm-memory: mpm db_path invariant — health_check missing db_path");
            }
            if (live !== required) {
              if (typeof log.error === "function") {
                log.error(
                  "openclaw-mpm-memory: refusing to boot — DB path invariant violated.\n" +
                  `  expected: ${required}\n  actual:   ${live}\n` +
                  "This usually means two mpm installs on the same host, or a stale scratch db."
                );
              }
              throw new Error("openclaw-mpm-memory: db_path invariant violated");
            }
            if (typeof log.info === "function") {
              log.info(`openclaw-mpm-memory: db_path invariant satisfied (${live})`);
            }
          }
          if (typeof log.info === "function") {
            log.info(
              `openclaw-mpm-memory: health_check ok ` +
              `(memories=${hc.memories_active ?? "?"} ` +
              `pending_theories=${hc.theories_pending ?? "?"} ` +
              `wakes_overdue=${hc.wakes_overdue ?? "?"})`
            );
          }
        } else {
          const err = (hc && hc.error) || "no ok:true in response";
          if (typeof log.warn === "function") {
            const errStr = String(err);
            const isPathError = /ENOENT|not found|spawn/i.test(errStr) || (typeof errStr === "string" && errStr.toLowerCase().includes("enoent"));
            const hint = isPathError
              ? `Looks like a missing-binary or PATH issue — the gateway runs under systemd with a stripped PATH. Set the absolute mpm path in plugin config: openclaw config set plugins.entries.openclaw-mpm-memory.config.mpmBin /absolute/path/to/mpm. Current mpmBin="${mpmBin}".`
              : `Check that ${mpmBin} exists and mcp.servers.mpm is registered.`;
            log.warn(
              `openclaw-mpm-memory: health_check failed — ${err}. ` +
              `Recall will return disabled:true until mpm is reachable. ${hint}`
            );
          }
        }
      } catch (e) {
        if (typeof log.warn === "function") {
          log.warn(`openclaw-mpm-memory: health_check threw — ${e.message || e}`);
        }
      }
    })();

    // ------------------------------------------------------------------
    // Session extension: per-session wake context cache
    // ------------------------------------------------------------------
    // Stored as JSON string in OpenClaw's session extension state.
    // This is the durable projection; the in-memory Map (wakeContextCache)
    // is the fast read path for the hook handlers.
    let sessionExtRegistered = false;
    function registerSessionExtension() {
      if (sessionExtRegistered) return;
      sessionExtRegistered = true;
      try {
        api.session?.state?.registerSessionExtension?.({
          id: `${PLUGIN_ID}:wake-context`,
          // OpenClaw plugin SDK requires namespace + description; missing
          // either logs a warning at every boot and may reject the
          // extension in strict mode. Added 0.1.3.
          namespace: PLUGIN_ID,
          description:
            "MPM wake context cache — durable projection of the per-" +
            "session Promise<string> map for the session_start → " +
            "agent_turn_prepare hook chain.",
          init: () => ({}),
          onLoad: () => ({}),
        });
      } catch (e) {
        if (typeof log.debug === "function") {
          log.debug(`openclaw-mpm-memory: session extension registration failed — ${e.message}`);
        }
      }
    }
    registerSessionExtension();

    // ------------------------------------------------------------------
    // Hook: session_start
    //
    // Fires before the first agent turn in every session (new or resumed).
    // Non-blocking async fetch: starts the MPM wake context fetch but does
    // NOT await it here — the promise is stored in wakeContextCache and
    // awaited by agent_turn_prepare on the next hook call.
    //
    // OpenClaw hook type: PluginHookSessionStartEvent
    // Fields: sessionId, sessionKey?, resumedFrom?
    // ------------------------------------------------------------------
    api.on("session_start", (event) => {
      const sessionKey = event?.sessionKey || "default";
      // Avoid duplicate fetches for the same sessionKey
      if (wakeContextCache.has(sessionKey)) return;

      // Start async fetch (non-blocking — don't await here)
      const promise = fetchWakeContext({
        mpmBin,
        timeoutMs,
        frameworkId: MPM_FRAMEWORK_ID,
        sessionKey,
      });
      wakeContextCache.set(sessionKey, promise);

      if (typeof log.debug === "function") {
        log.debug(`openclaw-mpm-memory: session_start for ${sessionKey}${event?.resumedFrom ? ` (resumedFrom=${event.resumedFrom})` : ""}`);
      }
    });

    // ------------------------------------------------------------------
    // Hook: session_end
    //
    // Fires when a session terminates.
    //
    // IMPORTANT: session ended ≠ work completed ≠ work verified.
    // No mpm work --complete is emitted here. The agent must explicitly
    // call mpm call mpm_work --action complete to claim work done.
    //
    // OpenClaw hook type: PluginHookSessionEndEvent
    // Fields: sessionId, sessionKey?, reason?, messageCount, durationMs?,
    //         transcriptArchived?, nextSessionId?, nextSessionKey?
    // ------------------------------------------------------------------
    api.on("session_end", (event) => {
      const sessionKey = event?.sessionKey || "default";
      wakeContextCache.delete(sessionKey);

      if (typeof log.debug === "function") {
        log.debug(
          `openclaw-mpm-memory: session_end for ${sessionKey} ` +
          `(reason=${event?.reason ?? "?"}, messages=${event?.messageCount ?? "?"})`
        );
      }
      // DO NOT emit work completion here. Session ended ≠ work verified.
      // The semantic contract is: agent explicitly calls mpm_work --complete
      // when it decides work is done, not when the session terminates.
    });

    // ------------------------------------------------------------------
    // Hook: agent_turn_prepare
    //
    // Runs after prompt prep, before the model call, for every agent turn.
    // Receives queued injections from session_start fetch.
    //
    // We await the cached wake context promise and inject it as
    // prependContext so it appears at the top of the agent prompt.
    //
    // OpenClaw hook type: PluginAgentTurnPrepareEvent
    // Returns: {prependContext?, appendContext?}
    // ------------------------------------------------------------------
    api.on("agent_turn_prepare", async (event) => {
      const sessionKey = event?.sessionKey || "default";
      const cached = wakeContextCache.get(sessionKey);

      if (!cached) return; // No fetch was started for this session

      // Await even if already resolved (safe for already-resolved promises)
      let wakeContext = "";
      try {
        wakeContext = await Promise.race([
          cached,
          new Promise((resolve) => setTimeout(() => resolve(""), timeoutMs)),
        ]);
      } catch {
        // On any error, inject nothing — never block the agent turn
        return;
      }

      if (wakeContext && wakeContext.length > 0) {
        return { prependContext: wakeContext };
      }
    });

    // ------------------------------------------------------------------
    // Hook: heartbeat_prompt_contribution
    //
    // Fires only for background monitor / lifecycle heartbeat turns.
    // Injects a minimal MPM status note — lightweight, non-blocking.
    // Does NOT inject the full wake context (heartbeats are for monitors,
    // not for re-establishing session context).
    //
    // OpenClaw hook type: PluginHeartbeatPromptContributionEvent
    // Fields: sessionKey?, agentId?, heartbeatName?
    // Returns: {prependContext?, appendContext?}
    // ------------------------------------------------------------------
    api.on("heartbeat_prompt_contribution", async (event) => {
      // Fetch a lightweight MPM status for heartbeat context
      const sessionKey = event?.sessionKey || "default";
      let statusText = "";

      try {
        const result = await callMpmTool("mpm_context", {
          action: "read_wake_context",
          params: { format: "system-prompt" },
        }, {
          mpmBin,
          timeoutMs: Math.min(timeoutMs, 2000), // tighter budget for heartbeats
        });
        // Only take the first 300 chars — heartbeat context must be minimal
        statusText = (result?.content || "").slice(0, 300);
      } catch {
        // Fail silently — heartbeat context is optional
      }

      if (statusText) {
        return { prependContext: `[MPM heartbeat] ${statusText}` };
      }
    });

    // ------------------------------------------------------------------
    // Hook: resolve_exec_env
    //
    // Contributes MPM_PROVENANCE_* environment variables to every exec
    // tool call, populating MPM's audit trail with OpenClaw framework
    // attribution.
    //
    // OpenClaw does not expose model name or invocation ID in the hook
    // context, so those fields are intentionally left unset (never
    // fabricated).
    //
    // OpenClaw hook type: PluginHookResolveExecEnvEvent
    // Fields: sessionKey?, toolName?, host?
    // Returns: Record<string, string> — env vars to merge
    // ------------------------------------------------------------------
    api.on("resolve_exec_env", (event) => {
      const env = {
        MPM_PROVENANCE_FRAMEWORK: MPM_FRAMEWORK_ID,
      };
      // OpenClaw sessionKey is the closest equivalent to Claude Code's
      // CLAUDE_SESSION_ID. Pass it when available.
      if (event?.sessionKey) {
        env.MPM_PROVENANCE_SESSION_KEY = event.sessionKey;
      }
      // Model name is not available in the OpenClaw hook context.
      // MPM_PROVENANCE_MODEL is intentionally omitted rather than
      // fabricated.
      return env;
    });

    // ------------------------------------------------------------------
    // Memory capability: promptBuilder + flushPlanResolver + runtime
    // ------------------------------------------------------------------

    // Cached manager per agent id for doctor.memory.status
    const managersByAgent = new Map();
    function getOrCreateManager(agentId) {
      let m = managersByAgent.get(agentId);
      if (m) return m;
      const workspaceDir =
        (api?.config?.agents?.list ?? []).find((a) => a?.id === agentId)?.workspace ||
        api?.config?.agents?.defaults?.workspace ||
        process.cwd();
      m = {
        status() {
          return {
            workspaceDir,
            provider: "mpm",
            backend: "mpm",
            mpm: { bin: mpmBin, scope },
          };
        },
        async probeEmbeddingAvailability() {
          try {
            const r = spawnSync(mpmBin, ["--version"], {
              stdio: ["ignore", "pipe", "pipe"],
              timeout: Math.min(timeoutMs, 2000),
              env: withWorkspace(),
            });
            if (r.status === 0) return { ok: true, checked: true };
            return {
              ok: false,
              checked: true,
              error: r.stderr
                ? r.stderr.toString().trim().split("\n").slice(-1)[0]
                : `mpm --version exited ${r.status}`,
            };
          } catch (e) {
            return {
              ok: false,
              checked: true,
              error: String((e && e.message) || e),
            };
          }
        },
        async search() {
          return { results: [], total: 0 };
        },
        async close() {},
      };
      managersByAgent.set(agentId, m);
      return m;
    }

    api.registerMemoryCapability({
      // promptBuilder is NOT used for wake context injection — we use
      // session_start + agent_turn_prepare hooks instead, which gives us
      // per-session lifecycle control and proper async fetch + inject.
      // promptBuilder can only return static text; agent_turn_prepare lets
      // us await the MPM call before the turn starts.
      promptBuilder: () => "",

      flushPlanResolver: () => {
        return {
          kind: "compaction_flush",
          relativePath: "local_flush_trash.md",
          systemPrompt:
            "You are performing epistemic compaction. Distill the recent " +
            "conversation into durable architectural lessons, decisions, " +
            "and factual context. Output clean markdown. " +
            "Do not include anything that isn't factually present in " +
            "the input conversation.",
          model: "",
          reserveTokensFloor: 20000,
          softThresholdTokens: 4000,
          forceFlushTranscriptBytes: 0,
        };
      },
      runtime: {
        async getMemorySearchManager(params) {
          const agentId = params?.agentId || "default";
          return { manager: getOrCreateManager(agentId) };
        },
        async closeAllMemorySearchManagers() {
          managersByAgent.clear();
        },
        async closeMemorySearchManager(params) {
          if (params?.agentId) managersByAgent.delete(params.agentId);
        },
        resolveMemoryBackendConfig() {
          return { backend: "mpm", mpm: { bin: mpmBin, scope } };
        },
      },
      publicArtifacts: { async listArtifacts() { return []; } },
    });

    // ------------------------------------------------------------------
    // Tools: memory_search + memory_get
    // ------------------------------------------------------------------

    const callMpm = (tool, payload) => callMpmTool(tool, payload, { mpmBin, timeoutMs });

    api.registerTool(
      () => ({
        label: "Memory Search (MPM)",
        name: "memory_search",
        description:
          "Hybrid FTS5 + reinforcement-weighted recall across the agent's MPM long-term memory (lessons, decisions, memories, theories, references). " +
          "Returned hit paths are virtual (mpm://memory/<id>); the recalled content sits in the snippet. " +
          "If response has disabled:true, MPM is unreachable — surface to the user.",
        parameters: MemorySearchSchema,
        async execute(_callId, params) {
          const limit = params.maxResults ?? limitDefault;
          const params_ = { query: params.query, limit, scope };
          if (typeof params.minScore === "number") params_.min_score = params.minScore;
          const payload_ = { action: "query", params: params_ };
          try {
            const result = await callMpm("mpm_memory", payload_);
            if (!result || result.success === false) {
              const err =
                (result && result.error) ||
                `mpm exit ${result && result._exitCode != null ? result._exitCode : "?"}`;
              return jsonResult({
                disabled: true,
                unavailable: true,
                error: err,
                backend: "mpm",
                query: params.query,
              });
            }
            const memories = Array.isArray(result.memories) ? result.memories : [];
            const hits = memories.map(adaptMemoryHit);
            return jsonResult({
              results: hits,
              backend: "mpm",
              query: params.query,
              total: typeof result.count === "number" ? result.count : hits.length,
              mpmScope: result.scope,
            });
          } catch (e) {
            return jsonResult({
              disabled: true,
              unavailable: true,
              error: String((e && e.message) || e),
              backend: "mpm",
              query: params.query,
            });
          }
        },
      }),
      { names: ["memory_search"] }
    );

    api.registerTool(
      () => ({
        label: "Memory Get (MPM)",
        name: "memory_get",
        description:
          "Read a memory entry by virtual path (mpm://memory/<id>). Returns the full recalled content for that id. " +
          "If the id cannot be resolved, returns {notFound:true}. Note: in most cases the snippet returned by memory_search is sufficient and this tool is not needed.",
        parameters: MemoryGetSchema,
        async execute(_callId, params) {
          const path = params.path || "";
          if (!path.startsWith(MP_MEMORY_PATH_PREFIX)) {
            return jsonResult({
              error: `unsupported memory path: ${path}`,
              notFound: true,
              supportedPrefix: MP_MEMORY_PATH_PREFIX,
            });
          }
          const id = path.slice(MP_MEMORY_PATH_PREFIX.length);
          if (!id) {
            return jsonResult({ error: "empty memory id", notFound: true });
          }
          try {
            const result = await callMpm("mpm_memory", {
              action: "query",
              params: { query: id, limit: 1, scope },
            });
            if (!result || result.success === false) {
              return jsonResult({
                disabled: true,
                unavailable: true,
                error: (result && result.error) || "mpm query failed",
              });
            }
            const memories = Array.isArray(result.memories) ? result.memories : [];
            const hit = memories.find((m) => m && m.id === id) || memories[0];
            if (!hit) {
              return jsonResult({ notFound: true, path });
            }
            return jsonResult({
              path: `${MP_MEMORY_PATH_PREFIX}${hit.id || id}`,
              content: hit.content || hit.text || hit.snippet || "",
              tags: hit.tags || [],
              weight: hit.weight,
              collection: hit.collection,
              createdAt: hit.created_at,
              source: "mpm",
            });
          } catch (e) {
            return jsonResult({
              disabled: true,
              unavailable: true,
              error: String((e && e.message) || e),
            });
          }
        },
      }),
      { names: ["memory_get"] }
    );
  },
});
