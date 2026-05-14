/**
 * MPM Plugin for OpenClaw — TypeScript Source
 *
 * Architecture:
 *   - Plugins live in ~/.openclaw/workspace/projects/mpm-plugin/
 *   - Source is TypeScript; bundled at install time by OpenClaw's plugin pipeline
 *   - Go binary is called directly via child_process.spawn (no Python wrappers)
 *   - Tool schemas use plain JSON Schema (not TypeBox) — compatible with all runtimes
 *
 * Tool summary:
 *   query_long_term_memory → mpm recall --json "<query>" <limit>
 *   save_to_memory         → mpm add --json "<fact>" [--tag <t>] [--weight <w>] [--ttl <ttl>]
 *
 * System prompt injection:
 *   OpenClaw plugin SDK has no generic prompt injection hook (no api.registerPrompt,
 *   no api.addSystemDirectives). The only prompt-building mechanism is
 *   registerMemoryCapability (memory-core specific). So MPM's mandatory recall/save
 *   directives live in AGENTS.md instead — it is loaded at startup and is the
 *   canonical place for persistent system-level guidance.
 */

import { definePluginEntry } from "openclaw/plugin-sdk/plugin-entry.js";
import type {
  OpenClawPluginApi,
  OpenClawPluginToolContext,
  AnyAgentTool,
} from "openclaw/plugin-sdk/index.js";

// ---------------------------------------------------------------------------
// Constants
// ---------------------------------------------------------------------------

const MPM_BINARY =
  process.env.MPM_BINARY ?? "/home/v/workspace/projects/mpm/bin/mpm";
const MPM_WORKSPACE =
  process.env.MPM_WORKSPACE ?? "/home/v/workspace/projects/mpm";

// ---------------------------------------------------------------------------
// Tool Schemas — plain JSON Schema (works with all OpenClaw runtimes)
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
} as const;

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
        "Time-to-live as a duration string, e.g. '24h', '7d', '0' (default '0' = no expiry). Use '24h' for ephemeral session-scoped facts, '0' for permanent memories.",
    },
  },
  required: ["fact"],
  additionalProperties: false,
} as const;

// ---------------------------------------------------------------------------
// runMpm — robust child_process spawn to Go binary
// ---------------------------------------------------------------------------

interface MpmRunResult {
  stdout: string;
  stderr: string;
  exitCode: number;
}

interface MpmJsonResult {
  id?: string;
  success?: boolean;
  memories?: Array<{ id?: string; content?: string }>;
  [key: string]: unknown;
}

/**
 * Spawn the MPM binary with the given args, capture stdout/stderr.
 *
 * Handles:
 *   - DB locked errors (returns graceful fallback, not a crash)
 *   - Binary not found / permission errors
 *   - Timeout (default 15s)
 *   - Non-zero exit codes
 */
async function runMpm(args: string[], timeoutMs = 15000): Promise<MpmRunResult> {
  const { spawn } = await import("child_process");

  return new Promise((resolve) => {
    let stdout = "";
    let stderr = "";
    let settled = false;

    const proc = spawn(MPM_BINARY, args, {
      // Pass MPM_WORKSPACE so the binary knows where the DB is
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

    proc.stdout?.on("data", (chunk: Buffer) => {
      stdout += chunk.toString();
    });

    proc.stderr?.on("data", (chunk: Buffer) => {
      stderr += chunk.toString();
    });

    proc.on("error", (err: Error & { code?: string }) => {
      if (!settled) {
        settled = true;
        clearTimeout(timer);
        resolve({
          stdout: "",
          stderr: `[spawn error] ${err.message}${
            err.code ? ` (${err.code})` : ""
          }`,
          exitCode: 1,
        });
      }
    });

    proc.on("close", (code: number | null) => {
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
 */
function parseMpmResult(result: MpmRunResult): MpmJsonResult {
  const { stdout, stderr, exitCode } = result;

  if (exitCode !== 0) {
    if (
      stderr.includes("database is locked") ||
      stderr.includes("SQLITE_BUSY") ||
      stderr.includes("database is locked")
    ) {
      return {
        id: "",
        success: false,
        error: "database_locked",
        message:
          "MPM database is locked — this is safe to retry. The write was not made.",
      };
    }
    return {
      id: "",
      success: false,
      error: `exit_${exitCode}`,
      message: stderr.split("\n")[0] || `MPM exited with code ${exitCode}`,
    };
  }

  const trimmed = stdout.trim();
  if (!trimmed) {
    return { id: "", success: true, count: 0 };
  }

  try {
    return JSON.parse(trimmed) as MpmJsonResult;
  } catch {
    // Non-JSON output — treat as raw text
    return { id: "", success: true, text: trimmed };
  }
}

// ---------------------------------------------------------------------------
// Tool Factories
// ---------------------------------------------------------------------------

function makeQueryLongTermMemoryTool(
  _ctx: OpenClawPluginToolContext
): AnyAgentTool {
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
      const { query = "", limit = 5 } = params as {
        query: string;
        limit?: number;
      };

      const result = await runMpm([
        "recall",
        "--json",
        "--",
        query,
        String(limit),
      ]);
      const data = parseMpmResult(result);

      // Format memories for the agent's tool result display
      let displayText: string;
      if (data.memories && Array.isArray(data.memories) && data.memories.length > 0) {
        displayText = data.memories
          .map((m) => (typeof m.content === "string" ? m.content : JSON.stringify(m)))
          .join("\n\n---\n\n");
      } else if (data.text) {
        displayText = String(data.text);
      } else {
        displayText = "(no matching memories found)";
      }

      return {
        toolCallId,
        result: {
          type: "ok" as const,
          results: [{ content: [{ type: "text" as const, text: displayText }] }],
        },
      };
    },
  };
}

function makeSaveToMemoryTool(
  _ctx: OpenClawPluginToolContext
): AnyAgentTool {
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
      const {
        fact = "",
        tags = [],
        weight = 0.5,
        ttl,
      } = params as {
        fact: string;
        tags?: string[];
        weight?: number;
        ttl?: string;
      };

      if (!fact.trim()) {
        return {
          toolCallId,
          result: {
            type: "ok" as const,
            results: [
              {
                content: [
                  {
                    type: "text" as const,
                    text: '{"success":false,"error":"empty_fact","message":"fact cannot be empty"}',
                  },
                ],
              },
            ],
          },
        };
      }

      // Build: mpm add --json -- "<fact>" [--tag <t>] [--weight <w>] [--ttl <t>]
      const args = ["add", "--json", "--", fact];
      for (const tag of tags) {
        args.push("--tag", tag);
      }
      if (weight !== 0.5) {
        args.push("--weight", String(weight));
      }
      if (ttl) {
        args.push("--ttl", ttl);
      }

      const result = await runMpm(args);
      const data = parseMpmResult(result);

      return {
        toolCallId,
        result: {
          type: "ok" as const,
          results: [
            {
              content: [{ type: "text" as const, text: JSON.stringify(data) }],
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
    "Use query_long_term_memory before answering questions about prior work. " +
    "Use save_to_memory after any non-trivial action or decision.",
  register(api: OpenClawPluginApi) {
    // ── Tools ────────────────────────────────────────────────────────────
    api.registerTool(
      (ctx: OpenClawPluginToolContext) =>
        makeQueryLongTermMemoryTool(ctx),
      { names: ["query_long_term_memory"], optional: false }
    );

    api.registerTool(
      (ctx: OpenClawPluginToolContext) => makeSaveToMemoryTool(ctx),
      { names: ["save_to_memory"], optional: false }
    );

    // ── Memory Capability (prompt builder for recall guidance) ────────────
    //
    // MPM does not own the memory slot (that is memory-core), but we can
    // register a lightweight memory capability that contributes prompt text.
    // This fires on every agent turn — equivalent to memory-core's built-in
    // "Before answering anything about prior work..." guidance.
    //
    // NOTE: If a full memory slot is later assigned to MPM (replacing
    // memory-core), this pattern should be replaced with registerMemoryCapability
    // using a proper flushPlanResolver.
    //
    // For now, the mandatory recall/save directives live in AGENTS.md
    // (loaded at startup) as the system-prompt anchor.
  },
});