/**
 * MPM Plugin for OpenClaw — Plain JavaScript
 *
 * Architecture:
 *   - Calls the Go mpm binary directly via child_process (no Python wrappers)
 *   - Uses the OpenClaw plugin SDK (definePluginEntry)
 *   - Tool schemas are plain JSON Schema (compatible with all runtimes)
 *
 * Tool summary:
 *   query_long_term_memory → mpm recall --json "<query>" <limit>
 *   save_to_memory         → mpm add --json "<fact>" [--tag <t>] [--weight <w>] [--ttl <ttl>]
 */

import { definePluginEntry } from "/home/v/.nvm/versions/node/v24.14.1/lib/node_modules/openclaw/dist/plugin-sdk/plugin-entry.js";

// ---------------------------------------------------------------------------
// Constants
// ---------------------------------------------------------------------------

const MPM_BINARY =
  process.env.MPM_BINARY ?? "/home/v/workspace/projects/mpm/bin/mpm";
const MPM_WORKSPACE =
  process.env.MPM_WORKSPACE ?? "/home/v/workspace/projects/mpm";

// ---------------------------------------------------------------------------
// Tool Schemas
// ---------------------------------------------------------------------------

const QUERY_LONG_TERM_MEMORY_SCHEMA = {
  type: "object",
  properties: {
    query: {
      type: "string",
      description:
        "Natural language search query for long-term memory.",
    },
    limit: {
      type: "number",
      description: "Maximum number of results to return (default: 5).",
      default: 5,
    },
  },
  required: ["query"],
  additionalProperties: false,
};

const SAVE_TO_MEMORY_SCHEMA = {
  type: "object",
  properties: {
    fact: {
      type: "string",
      description:
        "The fact, lesson, or decision to persist to long-term memory.",
    },
    tags: {
      type: "array",
      items: { type: "string" },
      description:
        "Optional tags for categorization and later retrieval.",
      default: [],
    },
    weight: {
      type: "number",
      description: "Importance weight between 0 and 1 (default: 0.5).",
      default: 0.5,
    },
    ttl: {
      type: "string",
      description:
        "Time-to-live as a duration string, e.g. '24h', '7d', '0' (default '0' = no expiry). " +
        "Use '24h' for ephemeral session-scoped facts, '0' for permanent memories.",
    },
  },
  required: ["fact"],
  additionalProperties: false,
};

// ---------------------------------------------------------------------------
// runMpm — spawn the Go binary, capture output, handle errors
// ---------------------------------------------------------------------------

/**
 * @param {string[]} args
 * @param {number} [timeoutMs=15000]
 * @returns {Promise<{stdout: string, stderr: string, exitCode: number}>}
 */
async function runMpm(args, timeoutMs = 15000) {
  const { spawn } = await import("child_process");

  return new Promise((resolve) => {
    let stdout = "";
    let stderr = "";
    let settled = false;

    const proc = spawn(MPM_BINARY, args, {
      env: { ...process.env, MPM_WORKSPACE },
      stdio: ["ignore", "pipe", "pipe"],
    });

    const timer = setTimeout(() => {
      if (!settled) {
        settled = true;
        proc.kill("SIGKILL");
        resolve({
          stdout,
          stderr: `${stderr}\n[timeout after ${timeoutMs}ms]`.trim(),
          exitCode: 124,
        });
      }
    }, timeoutMs);

    proc.stdout?.on("data", (chunk) => {
      stdout += chunk.toString();
    });

    proc.stderr?.on("data", (chunk) => {
      stderr += chunk.toString();
    });

    proc.on("error", (err) => {
      if (!settled) {
        settled = true;
        clearTimeout(timer);
        resolve({
          stdout: "",
          stderr: `[spawn error] ${err.message}${err.code ? ` (${err.code})` : ""}`,
          exitCode: 1,
        });
      }
    });

    proc.on("close", (code) => {
      if (!settled) {
        settled = true;
        clearTimeout(timer);
        resolve({ stdout, stderr, exitCode: code ?? 0 });
      }
    });
  });
}

/**
 * Parse JSON from mpm stdout. Handles DB locked, non-JSON, and error exits.
 * @param {{stdout: string, stderr: string, exitCode: number}} result
 * @returns {Record<string, any>}
 */
function parseMpmResult(result) {
  const { stdout, stderr, exitCode } = result;

  if (exitCode !== 0) {
    if (
      stderr.includes("database is locked") ||
      stderr.includes("SQLITE_BUSY")
    ) {
      return {
        id: "",
        success: false,
        error: "database_locked",
        message:
          "MPM database is locked — safe to retry. The write was not made.",
      };
    }
    return {
      id: "",
      success: false,
      error: `exit_${exitCode}`,
      message:
        stderr.split("\n")[0] || `MPM exited with code ${exitCode}`,
    };
  }

  const trimmed = stdout.trim();
  if (!trimmed) {
    return { id: "", success: true, count: 0 };
  }

  try {
    return JSON.parse(trimmed);
  } catch {
    return { id: "", success: true, text: trimmed };
  }
}

// ---------------------------------------------------------------------------
// Tool Factories
// ---------------------------------------------------------------------------

/**
 * @param {import("openclaw/plugin-sdk/index.js").OpenClawPluginToolContext} _ctx
 * @returns {import("openclaw/plugin-sdk/index.js").AnyAgentTool}
 */
function makeQueryLongTermMemoryTool(_ctx) {
  return {
    name: "query_long_term_memory",
    description:
      "Search MPM long-term memory. " +
      "Before answering anything about prior work, decisions, dates, people, " +
      "preferences, or todos — run this first. " +
      "Returns matching memories as formatted text.",
    parameters: QUERY_LONG_TERM_MEMORY_SCHEMA,
    emoji_name: "brain",
    execute: async (toolCallId, params) => {
      const { query = "", limit = 5 } = params;

      const result = await runMpm([
        "recall",
        "--json",
        "--",
        query,
        String(limit),
      ]);
      const data = parseMpmResult(result);

      let displayText;
      if (
        data.memories &&
        Array.isArray(data.memories) &&
        data.memories.length > 0
      ) {
        displayText = data.memories
          .map((m) =>
            typeof m.content === "string" ? m.content : JSON.stringify(m)
          )
          .join("\n\n---\n\n");
      } else if (data.text) {
        displayText = String(data.text);
      } else {
        displayText = "(no matching memories found)";
      }

      return {
        toolCallId,
        result: {
          type: "ok",
          results: [
            { content: [{ type: "text", text: displayText }] },
          ],
        },
      };
    },
  };
}

/**
 * @param {import("openclaw/plugin-sdk/index.js").OpenClawPluginToolContext} _ctx
 * @returns {import("openclaw/plugin-sdk/index.js").AnyAgentTool}
 */
function makeSaveToMemoryTool(_ctx) {
  return {
    name: "save_to_memory",
    description:
      "Persist a fact, lesson, or decision to MPM long-term memory. " +
      "After any non-trivial action, lesson learned, or decision — call this. " +
      "Tags help later retrieval. Weight 0.5 by default; higher for important truths. " +
      "Use TTL '24h' for session-scoped facts (e.g. current task context), " +
      "use TTL '0' for permanent memories (e.g. user preferences, project state).",
    parameters: SAVE_TO_MEMORY_SCHEMA,
    emoji_name: "floppy_disk",
    execute: async (toolCallId, params) => {
      const { fact = "", tags = [], weight = 0.5, ttl } = params;

      if (!fact.trim()) {
        return {
          toolCallId,
          result: {
            type: "ok",
            results: [
              {
                content: [
                  {
                    type: "text",
                    text: '{"success":false,"error":"empty_fact","message":"fact cannot be empty"}',
                  },
                ],
              },
            ],
          },
        };
      }

      const args = ["add", fact];
      for (const tag of tags) {
        args.push("--tag", tag);
      }
      if (weight !== 0.5) {
        args.push("--weight", String(weight));
      }
      if (ttl) {
        args.push("--ttl", ttl);
      }
      args.push("--json");

      const result = await runMpm(args);
      const data = parseMpmResult(result);

      return {
        toolCallId,
        result: {
          type: "ok",
          results: [
            {
              content: [{ type: "text", text: JSON.stringify(data) }],
            },
          ],
        },
      };
    },
  };
}

// ---------------------------------------------------------------------------
// Plugin Entry
// ---------------------------------------------------------------------------

export default definePluginEntry({
  id: "mpm",
  name: "MPM (Memory Persistence Module)",
  description:
    "Long-term memory tool for 808 — query and persist facts via the agent's " +
    "own SQLite-backed persistence layer (MPM). " +
    "query_long_term_memory: before answering anything about prior work. " +
    "save_to_memory: after any non-trivial action or decision.",
  register(api) {
    api.registerTool(
      (ctx) => makeQueryLongTermMemoryTool(ctx),
      { names: ["query_long_term_memory"], optional: false }
    );

    api.registerTool(
      (ctx) => makeSaveToMemoryTool(ctx),
      { names: ["save_to_memory"], optional: false }
    );
  },
});