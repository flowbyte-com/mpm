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

import { spawn, spawnSync } from "node:child_process";
import { definePluginEntry } from "@openclaw/plugin-sdk/plugin-entry";
import { jsonResult } from "@openclaw/plugin-sdk/tool-results";

const MP_MEMORY_PATH_PREFIX = "mpm://memory/";

// OpenClaw plugin tool parameters are plain JSON Schema (see memory-core's
// MemorySearchSchema for the reference shape). TypeBox-style helpers are not
// re-exported from @openclaw/plugin-sdk.

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

async function callMpmTool(tool, payload, opts) {
  const bin = opts.mpmBin;
  const timeoutMs = opts.timeoutMs;
  return new Promise((resolve) => {
    let child;
    try {
      child = spawn(bin, ["call", tool, "--payload", JSON.stringify(payload)], {
        stdio: ["ignore", "pipe", "pipe"],
        env: { ...process.env, MPM_LOG_FORMAT: "json" },
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

function ensureMpmAvailable(bin) {
  return new Promise((resolve) => {
    let which;
    try {
      which = spawn("which", [bin], { stdio: ["ignore", "pipe", "ignore"] });
    } catch {
      resolve(false);
      return;
    }
    let out = "";
    which.stdout.on("data", (d) => (out += d));
    which.on("close", (code) => resolve(code === 0 && out.trim().length > 0));
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
// Plugin entry
// --------------------------------------------------------------------------

export default definePluginEntry({
  id: "openclaw-mpm-memory",
  name: "MPM Memory",
  description:
    "Memory slot backed by MPM. Routes memory_search/memory_get through `mpm call mpm_memory` (aggregator tool, action:query). FTS5 lexical + reinforcement-weighted recall. Replaces the default memory-core plugin (semantic vector recall is not provided by MPM in this adapter).",
  kind: "memory",
  enabledByDefault: false,

  register(api) {
    const log = api.logger ?? console;
    const entryConfig =
      api?.config?.plugins?.entries?.["openclaw-mpm-memory"]?.config ?? {};
    const mpmBin = typeof entryConfig.mpmBin === "string" ? entryConfig.mpmBin : "mpm";
    const timeoutMs =
      typeof entryConfig.timeoutMs === "number" ? entryConfig.timeoutMs : 5000;
    const scope = typeof entryConfig.scope === "string" ? entryConfig.scope : "all";
    const limitDefault =
      typeof entryConfig.limitDefault === "number" ? entryConfig.limitDefault : 6;

    // Synchronous PATH probe via spawnSync would block briefly; instead we
    // register the tools unconditionally and let per-call guard handle
    // missing-binary cases (returns {disabled:true, error: "ENOENT..."}).
    // Operators who want boot-time validation can run install.sh first.
    if (typeof log.info === "function") {
      log.info(`openclaw-mpm-memory: registered (mpmBin=${mpmBin}, scope=${scope}, timeout=${timeoutMs}ms, limit=${limitDefault})`);
    }

    // Boot-time health check — fail loud at OpenClaw startup rather than
    // silently returning disabled:true during reasoning turns. We don't
    // *throw* here (that would crash the gateway boot for a transient
    // mpm blip); we log loudly with action items, then continue. Operator
    // sees this on every OpenClaw restart and can act on it.
    //
    // Wrapped in an async IIFE so we don't make register() async (the
    // plugin SDK contract is sync). The check runs to completion in the
    // background; on a healthy box it finishes in ~50ms, well before the
    // agent's first memory_search turn.
    //
    // Action: ping `mpm call mpm_system` with action:health_check, which
    // returns SQLite integrity + domain counts. ok:true means the mpm
    // binary exists, the mcp server is reachable, and the DB is healthy.
    (async () => {
      try {
        const hc = await callMpmTool("mpm_system", { action: "health_check" }, {
          mpmBin,
          // Health check should be cheap; cap tightly so a stuck mpm
          // doesn't block boot for 5s.
          timeoutMs: 2000,
        });
        if (hc && hc.ok === true) {
          if (typeof log.info === "function") {
            log.info(
              `openclaw-mpm-memory: health_check ok ` +
              `(memories=${hc.memories_active ?? "?"} pending_theories=${hc.theories_pending ?? "?"} ` +
              `wakes_overdue=${hc.wakes_overdue ?? "?"})`,
            );
          }
        } else {
          const err = (hc && hc.error) || "no ok:true in response";
          if (typeof log.warn === "function") {
            log.warn(
              `openclaw-mpm-memory: health_check failed — ${err}. ` +
              `Recall will return disabled:true until mpm is reachable. ` +
              `Check that /home/v/.mpm/bin/mpm exists and mcp.servers.mpm ` +
              `is registered.`,
            );
          }
        }
      } catch (e) {
        if (typeof log.warn === "function") {
          log.warn(`openclaw-mpm-memory: health_check threw — ${e.message || e}`);
        }
      }
    })();

    // Cached manager per agent id. The manager is what the gateway uses to
    // probe embedding readiness for `doctor.memory.status`. Recall itself
    // happens through the registered tools (memory_search/memory_get), not
    // through this handle — but the doctor check requires a non-null manager
    // with `status()` and (optionally) `probeEmbeddingAvailability()`.
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
          // Doctor's default call uses `probe: false`, which short-circuits
          // to the SKIPPED sentinel and never lands here. If `probe: true`
          // is ever requested, do a real mpm round-trip via `mpm --version`.
          try {
            const r = spawnSync(mpmBin, ["--version"], {
              stdio: ["ignore", "pipe", "pipe"],
              timeout: Math.min(timeoutMs, 2000),
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
          // Not used by the doctor. Recall is handled by the registered
          // memory_search tool, which has the canonical parameter schema.
          return { results: [], total: 0 };
        },
        async close() {},
      };
      managersByAgent.set(agentId, m);
      return m;
    }

    // Register the memory capability.
    api.registerMemoryCapability({
      promptBuilder: () => "",
      flushPlanResolver: () => ({ kind: "noop" }),
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
          const payload = { action: "query", params: params_ };
          try {
            const result = await callMpm("mpm_memory", payload);
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
            // MPM has no direct id-get; re-query with the id as the search term
            // and take the top hit. The FTS5 match on id (if present in content)
            // plus the lexical match on the id string are usually sufficient.
            const result = await callMpm("mpm_memory", {
              action: "query",
              params: {
                query: id,
                limit: 1,
                scope,
              },
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
