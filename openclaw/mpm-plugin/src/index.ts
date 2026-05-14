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
 *   Core memory:
 *     query_long_term_memory → mpm recall --json "<query>" <limit>
 *     save_to_memory         → mpm add --json "<fact>" [--tag <t>] [--weight <w>] [--ttl <ttl>]
 *   Lessons:
 *     save_lesson       → mpm lesson add <content> --type <type> --tags <tags> --json
 *     search_lessons    → mpm lesson search <query> --json
 *     list_lessons      → mpm lesson list [--type=<type>] --json
 *   Topics:
 *     create_topic      → mpm topic add <name> [description] --json
 *     search_topics     → mpm topic search <query> --json
 *   References:
 *     add_reference     → mpm reference add <file> [--title <title>] --json
 *     search_references → mpm reference search <query> [--limit <n>] --json
 *     list_references   → mpm reference ls --json
 *   Directives:
 *     read_directives   → mpm directives --json
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

// ── Core Memory ───────────────────────────────────────────────────────────────

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

// ── Lessons ─────────────────────────────────────────────────────────────────

const SAVE_LESSON_SCHEMA = {
  type: "object",
  properties: {
    fact: {
      type: "string",
      description: "The lesson content — what was learned or observed.",
    },
    type: {
      type: "string",
      enum: ["warning", "practice", "insight"],
      description: "Lesson type.",
      default: "insight",
    },
    tags: {
      type: "array",
      items: { type: "string" },
      description: "Optional tags for retrieval.",
      default: [],
    },
  },
  required: ["fact"],
  additionalProperties: false,
} as const;

const SEARCH_LESSONS_SCHEMA = {
  type: "object",
  properties: {
    query: {
      type: "string",
      description: "Search query for lessons.",
    },
  },
  required: ["query"],
  additionalProperties: false,
} as const;

const LIST_LESSONS_SCHEMA = {
  type: "object",
  properties: {
    type: {
      type: "string",
      enum: ["warning", "practice", "insight"],
      description: "Filter by lesson type (optional).",
    },
  },
  additionalProperties: false,
} as const;

// ── Topics ───────────────────────────────────────────────────────────────────

const CREATE_TOPIC_SCHEMA = {
  type: "object",
  properties: {
    name: {
      type: "string",
      description: "Topic name.",
    },
    description: {
      type: "string",
      description: "Optional description.",
    },
  },
  required: ["name"],
  additionalProperties: false,
} as const;

const SEARCH_TOPICS_SCHEMA = {
  type: "object",
  properties: {
    query: {
      type: "string",
      description: "Search query for topics.",
    },
  },
  required: ["query"],
  additionalProperties: false,
} as const;

// ── References ───────────────────────────────────────────────────────────────

const ADD_REFERENCE_SCHEMA = {
  type: "object",
  properties: {
    filepath: {
      type: "string",
      description: "Absolute path to the document to ingest.",
    },
    title: {
      type: "string",
      description: "Optional title for the document.",
    },
  },
  required: ["filepath"],
  additionalProperties: false,
} as const;

const SEARCH_REFERENCES_SCHEMA = {
  type: "object",
  properties: {
    query: {
      type: "string",
      description: "Search string for reference content.",
    },
    limit: {
      type: "number",
      description: "Max results (default: 5).",
      default: 5,
    },
  },
  required: ["query"],
  additionalProperties: false,
} as const;

const LIST_REFERENCES_SCHEMA = {
  type: "object",
  properties: {},
  additionalProperties: false,
} as const;

// ── Directives ───────────────────────────────────────────────────────────────

const READ_DIRECTIVES_SCHEMA = {
  type: "object",
  properties: {},
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

// ── Lessons ─────────────────────────────────────────────────────────────────────

function makeSaveLessonTool(
  _ctx: OpenClawPluginToolContext
): AnyAgentTool {
  return {
    name: "save_lesson",
    description:
      "Persist a lesson to MPM — what was learned, observed, or should be remembered. " +
      "Three types: 'warning' (don't do X), 'practice' (do Y), 'insight' (X leads to Y). " +
      "Lessons accumulate as the agent's learned experience.",
    parameters: SAVE_LESSON_SCHEMA,
    emoji_name: "bookmark",
    execute: async (toolCallId, params) => {
      const {
        fact = "",
        type = "insight",
        tags = [],
      } = params as {
        fact: string;
        type?: "warning" | "practice" | "insight";
        tags?: string[];
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

      const args = [
        "lesson", "add",
        fact,
        "--type", type,
        "--tags", tags.join(","),
        "--json",
      ];

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

function makeSearchLessonsTool(
  _ctx: OpenClawPluginToolContext
): AnyAgentTool {
  return {
    name: "search_lessons",
    description:
      "Search MPM lessons for relevant learned knowledge. " +
      "Use this to recall warnings, best practices, and insights before acting.",
    parameters: SEARCH_LESSONS_SCHEMA,
    emoji_name: "magnifying_glass",
    execute: async (toolCallId, params) => {
      const { query = "" } = params as { query: string };

      const result = await runMpm(["lesson", "search", query, "--json"]);
      const data = parseMpmResult(result);

      // Format lessons for display
      let displayText: string;
      if (data.results && Array.isArray(data.results) && data.results.length > 0) {
        displayText = data.results
          .map((l: MpmJsonResult) => {
            const type = l.type || "insight";
            const content = typeof l.content === "string" ? l.content : JSON.stringify(l.content);
            return `[${type}] ${content}`;
          })
          .join("\n\n---\n\n");
      } else if (data.message) {
        displayText = String(data.message);
      } else {
        displayText = "(no matching lessons found)";
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

function makeListLessonsTool(
  _ctx: OpenClawPluginToolContext
): AnyAgentTool {
  return {
    name: "list_lessons",
    description:
      "List all lessons in MPM, optionally filtered by type. " +
      "Types: 'warning' (don't do X), 'practice' (do Y), 'insight' (X leads to Y).",
    parameters: LIST_LESSONS_SCHEMA,
    emoji_name: "books",
    execute: async (toolCallId, params) => {
      const { type } = params as { type?: "warning" | "practice" | "insight" };

      const typeArg = type ? `--type=${type}` : "";
      const args = ["lesson", "list", typeArg, "--json"].filter(Boolean);

      const result = await runMpm(args);
      const data = parseMpmResult(result);

      // Format lessons for display
      let displayText: string;
      if (data.lessons && Array.isArray(data.lessons) && data.lessons.length > 0) {
        displayText = data.lessons
          .map((l: MpmJsonResult) => {
            const lessonType = l.type || "insight";
            const content = typeof l.content === "string" ? l.content : JSON.stringify(l.content);
            const created = l.created_at ? ` (${String(l.created_at).slice(0, 10)})` : "";
            return `[${lessonType}]${created} ${content}`;
          })
          .join("\n");
      } else if (data.message) {
        displayText = String(data.message);
      } else {
        displayText = "(no lessons stored)";
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

// ── Topics ───────────────────────────────────────────────────────────────────

function makeCreateTopicTool(
  _ctx: OpenClawPluginToolContext
): AnyAgentTool {
  return {
    name: "create_topic",
    description:
      "Create a topic in MPM to organize related memories and knowledge. " +
      "Topics can be searched and serve as memory clusters.",
    parameters: CREATE_TOPIC_SCHEMA,
    emoji_name: "label",
    execute: async (toolCallId, params) => {
      const {
        name = "",
        description = "",
      } = params as {
        name: string;
        description?: string;
      };

      if (!name.trim()) {
        return {
          toolCallId,
          result: {
            type: "ok" as const,
            results: [
              {
                content: [
                  {
                    type: "text" as const,
                    text: '{"success":false,"error":"empty_name","message":"name cannot be empty"}',
                  },
                ],
              },
            ],
          },
        };
      }

      const result = await runMpm([
        "topic", "add",
        name,
        description,
        "--json",
      ]);
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

function makeSearchTopicsTool(
  _ctx: OpenClawPluginToolContext
): AnyAgentTool {
  return {
    name: "search_topics",
    description:
      "Search MPM topics for relevant knowledge clusters. " +
      "Topics group related memories and provide context for the agent.",
    parameters: SEARCH_TOPICS_SCHEMA,
    emoji_name: "magnifying_glass",
    execute: async (toolCallId, params) => {
      const { query = "" } = params as { query: string };

      const result = await runMpm(["topic", "search", query, "--json"]);
      const data = parseMpmResult(result);

      // Format topics for display
      let displayText: string;
      if (data.results && Array.isArray(data.results) && data.results.length > 0) {
        displayText = data.results
          .map((t: MpmJsonResult) => {
            const name = t.name || t.description || "(unnamed)";
            const id = t.id ? ` [${t.id}]` : "";
            const desc = t.description ? `: ${t.description}` : "";
            return `${name}${id}${desc}`;
          })
          .join("\n");
      } else if (data.message) {
        displayText = String(data.message);
      } else {
        displayText = "(no matching topics found)";
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

// ── References ───────────────────────────────────────────────────────────────

function makeAddReferenceTool(
  _ctx: OpenClawPluginToolContext
): AnyAgentTool {
  return {
    name: "add_reference",
    description:
      "Ingest a document as a reference into MPM. " +
      "Supported formats: .txt, .md, .html, .epub, .pdf. " +
      "The document is chunked and indexed for semantic search.",
    parameters: ADD_REFERENCE_SCHEMA,
    emoji_name: "paperclip",
    execute: async (toolCallId, params) => {
      const {
        filepath = "",
        title,
      } = params as {
        filepath: string;
        title?: string;
      };

      if (!filepath.trim()) {
        return {
          toolCallId,
          result: {
            type: "ok" as const,
            results: [
              {
                content: [
                  {
                    type: "text" as const,
                    text: '{"success":false,"error":"empty_filepath","message":"filepath cannot be empty"}',
                  },
                ],
              },
            ],
          },
        };
      }

      const titleArg = title ? ["--title", title] : [];
      const args = ["reference", "add", filepath, ...titleArg, "--json"].filter(Boolean);

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

function makeSearchReferencesTool(
  _ctx: OpenClawPluginToolContext
): AnyAgentTool {
  return {
    name: "search_references",
    description:
      "Search content within ingested reference documents. " +
      "Returns matching chunks from the reference library.",
    parameters: SEARCH_REFERENCES_SCHEMA,
    emoji_name: "books",
    execute: async (toolCallId, params) => {
      const {
        query = "",
        limit = 5,
      } = params as {
        query: string;
        limit?: number;
      };

      const result = await runMpm([
        "reference", "search",
        query,
        String(limit),
        "--json",
      ]);
      const data = parseMpmResult(result);

      // Format reference chunks for display
      let displayText: string;
      if (data.results && Array.isArray(data.results) && data.results.length > 0) {
        displayText = data.results
          .map((r: MpmJsonResult) => {
            const docTitle = r.doc_title || "document";
            const chunkIdx = r.chunk_index ?? 0;
            const content = typeof r.content === "string" ? r.content : JSON.stringify(r.content);
            return `[${docTitle} chunk ${chunkIdx}]\n${content}`;
          })
          .join("\n\n---\n\n");
      } else if (data.message) {
        displayText = String(data.message);
      } else {
        displayText = "(no matching reference content found)";
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

function makeListReferencesTool(
  _ctx: OpenClawPluginToolContext
): AnyAgentTool {
  return {
    name: "list_references",
    description:
      "List all ingested reference documents in MPM. " +
      "Shows document titles, chunk counts, and tags.",
    parameters: LIST_REFERENCES_SCHEMA,
    emoji_name: "books",
    execute: async (toolCallId, _params) => {
      const result = await runMpm(["reference", "ls", "--json"]);
      const data = parseMpmResult(result);

      // Format references for display
      let displayText: string;
      if (data.references && Array.isArray(data.references) && data.references.length > 0) {
        displayText = data.references
          .map((r: MpmJsonResult) => {
            const title = r.title || "untitled";
            const chunks = r.total_chunks ?? 0;
            const tags = r.tags ? ` [${r.tags}]` : "";
            const created = r.created_at ? ` — ${String(r.created_at).slice(0, 10)}` : "";
            return `${title} (${chunks} chunks)${tags}${created}`;
          })
          .join("\n");
      } else if (data.message) {
        displayText = String(data.message);
      } else {
        displayText = "(no references stored)";
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

// ── Directives ───────────────────────────────────────────────────────────────

function makeReadDirectivesTool(
  _ctx: OpenClawPluginToolContext
): AnyAgentTool {
  return {
    name: "read_directives",
    description:
      "Read the agent's prime directives — behavioral rules and operating principles. " +
      "These define what the agent must and must not do.",
    parameters: READ_DIRECTIVES_SCHEMA,
    emoji_name: "scroll",
    execute: async (toolCallId, _params) => {
      const result = await runMpm(["directives", "--json"]);
      const data = parseMpmResult(result);

      // Format directives for display
      let displayText: string;
      if (data.directives && Array.isArray(data.directives) && data.directives.length > 0) {
        displayText = data.directives
          .map((d: MpmJsonResult) => {
            const collection = d.collection || "directive";
            const content = typeof d.content === "string" ? d.content : JSON.stringify(d.content);
            return `[${collection}]\n${content}`;
          })
          .join("\n\n---\n\n");
      } else if (data.message) {
        displayText = String(data.message);
      } else {
        displayText = "(no directives defined — run the session that defines them)";
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
    // ── Core Memory Tools ─────────────────────────────────────────────────
    api.registerTool(
      (ctx: OpenClawPluginToolContext) =>
        makeQueryLongTermMemoryTool(ctx),
      { names: ["query_long_term_memory"], optional: false }
    );

    api.registerTool(
      (ctx: OpenClawPluginToolContext) => makeSaveToMemoryTool(ctx),
      { names: ["save_to_memory"], optional: false }
    );

    // ── Lesson Tools ──────────────────────────────────────────────────────
    api.registerTool(
      (ctx: OpenClawPluginToolContext) => makeSaveLessonTool(ctx),
      { names: ["save_lesson"], optional: false }
    );

    api.registerTool(
      (ctx: OpenClawPluginToolContext) => makeSearchLessonsTool(ctx),
      { names: ["search_lessons"], optional: false }
    );

    api.registerTool(
      (ctx: OpenClawPluginToolContext) => makeListLessonsTool(ctx),
      { names: ["list_lessons"], optional: false }
    );

    // ── Topic Tools ──────────────────────────────────────────────────────
    api.registerTool(
      (ctx: OpenClawPluginToolContext) => makeCreateTopicTool(ctx),
      { names: ["create_topic"], optional: false }
    );

    api.registerTool(
      (ctx: OpenClawPluginToolContext) => makeSearchTopicsTool(ctx),
      { names: ["search_topics"], optional: false }
    );

    // ── Reference Tools ──────────────────────────────────────────────────
    api.registerTool(
      (ctx: OpenClawPluginToolContext) => makeAddReferenceTool(ctx),
      { names: ["add_reference"], optional: false }
    );

    api.registerTool(
      (ctx: OpenClawPluginToolContext) => makeSearchReferencesTool(ctx),
      { names: ["search_references"], optional: false }
    );

    api.registerTool(
      (ctx: OpenClawPluginToolContext) => makeListReferencesTool(ctx),
      { names: ["list_references"], optional: false }
    );

    // ── Directive Tools ──────────────────────────────────────────────────
    api.registerTool(
      (ctx: OpenClawPluginToolContext) => makeReadDirectivesTool(ctx),
      { names: ["read_directives"], optional: false }
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