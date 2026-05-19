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

// Max buffer size to prevent unbounded stdout/stderr accumulation.
// 10 MB is sufficient for any reasonable mpm output while preventing OOM.
const MAX_BUFFER_SIZE = 10 * 1024 * 1024;

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

const LINK_TOPIC_SCHEMA = {
  type: "object",
  properties: {
    memory_id: {
      type: "string",
      description: "ID of the memory to link to a topic.",
    },
    topic_id: {
      type: "string",
      description: "ID of the topic to link the memory to.",
    },
  },
  required: ["memory_id", "topic_id"],
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

const READ_WAKE_CONTEXT_SCHEMA = {
  type: "object",
  properties: {},
  additionalProperties: false,
} as const;

const READ_DIRECTIVES_SCHEMA = {
  type: "object",
  properties: {},
  additionalProperties: false,
} as const;

// ---------------------------------------------------------------------------
// Epistemology Engine — Decision & Theory Tools
// ---------------------------------------------------------------------------


const RECORD_DECISION_SCHEMA = {
  type: "object",
  properties: {
    context: {
      type: "string",
      description:
        "The situation or problem that forced a choice between competing options.",
    },
    choice: {
      type: "string",
      description:
        "What was decided — the specific path, approach, or action taken.",
    },
    rationale: {
      type: "string",
      description:
        "Why this choice won over the alternatives. What evidence or reasoning made it the right call.",
    },
    outcome: {
      type: "string",
      description:
        "Optional: what actually happened when this decision was executed.",
    },
    tags: {
      type: "array",
      items: { type: "string" },
      description: "Optional tags for retrieval.",
      default: [],
    },
    weight: {
      type: "number",
      description: "Importance weight 0–1 (default 0.5). Use 0.8+ for architectural decisions.",
      default: 0.5,
    },
  },
  required: ["context", "choice", "rationale"],
  additionalProperties: false,
} as const;

const PROPOSE_THEORY_SCHEMA = {
  type: "object",
  properties: {
    hypothesis: {
      type: "string",
      description:
        "What you think is true — a causal assumption, a noticed pattern, or a gut feeling about why something is broken.",
    },
    validationCriteria: {
      type: "string",
      description:
        "A specific, executable test or observation that would prove or disprove the hypothesis. " +
        "Be concrete: 'run the benchmark with --json flag before positional arg and compare parse time' " +
        "— not 'test it somehow'.",
    },
    tags: {
      type: "array",
      items: { type: "string" },
      description: "Optional tags.",
      default: [],
    },
  },
  required: ["hypothesis", "validationCriteria"],
  additionalProperties: false,
} as const;


const RESOLVE_THEORY_SCHEMA = {
  type: "object",
  properties: {
    theoryId: {
      type: "string",
      description: "The MPM memory ID of the theory to resolve.",
    },
    conclusion: {
      type: "string",
      description: "What the test or observation actually found. Be specific about the result.",
    },
    newStatus: {
      type: "string",
      enum: ["proven", "disproven"],
      description: "proven if the hypothesis was confirmed; disproven if it was not.",
    },
  },
  required: ["theoryId", "conclusion", "newStatus"],
  additionalProperties: false,
} as const;

const CHALLENGE_MEMORY_SCHEMA = {
  type: "object",
  properties: {
    memoryId: {
      type: "string",
      description: "The MPM memory ID to challenge.",
    },
    evidence: {
      type: "string",
      description: "Evidence that contradicts the memory. Be specific about what changed and why the memory is no longer accurate.",
    },
  },
  required: ["memoryId", "evidence"],
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
  created_at?: string;  // directive timestamp for age calculation
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
      if (stdout.length + chunk.length > MAX_BUFFER_SIZE) {
        settled = true;
        proc.kill("SIGKILL");
        resolve({
          stdout,
          stderr: `${stderr}\n[output exceeded ${MAX_BUFFER_SIZE} bytes]`.trim(),
          exitCode: 125,
        });
        return;
      }
      stdout += chunk.toString();
    });

    proc.stderr?.on("data", (chunk: Buffer) => {
      if (stderr.length + chunk.length > MAX_BUFFER_SIZE) {
        // Only truncate stderr, don't kill — mpm writes warnings there too
        stderr = (stderr + chunk.toString()).slice(-MAX_BUFFER_SIZE);
      } else {
        stderr += chunk.toString();
      }
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
    // Buffer overflow — distinguish from genuinely missing wake context.
    // SIGKILL on overflow produces exitCode 125 with stderr containing
    // "[output exceeded N bytes]" rather than a normal error message.
    if (exitCode === 125 && stderr.includes("[output exceeded")) {
      return {
        id: "",
        success: false,
        error: "wake_context_truncated",
        message:
          "Wake context exceeds buffer limit — session is too large for the wake context tool. " +
          "Proceed with minimal context (no previous session state).",
      };
    }
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
  ctx: OpenClawPluginToolContext
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

      // Detect Telegram or other constrained channels from context.
      // OpenClaw's plugin SDK exposes activeModel and channel in the tool
      // context. If the channel is telegram, use a tighter token budget.
      // Otherwise default to 16000 (generous for most channels).
      let tokenBudget = 16000;
      const ctxAny = ctx as Record<string, unknown>;
      const channel = String(ctxAny.channel || ctxAny.activeModel || "").toLowerCase();
      if (channel.includes("telegram") || process.env.TELEGRAM_TOKEN) {
        tokenBudget = 1000;
      }

      const args: string[] = [
        "recall",
        "--json",
        "--token-budget",
        String(tokenBudget),
        "--",
        query,
        String(limit),
      ];

      const result = await runMpm(args);
      const data = parseMpmResult(result);

      // Format memories for the agent's tool result display
      let displayText: string;
      if (data.memories && Array.isArray(data.memories) && data.memories.length > 0) {
        displayText = data.memories.map((m) => {
          const mem = m as any;
          const xref = mem.cross_references || {};
          const topics = xref.topics || [];
          const refDoc = xref.reference_doc;

          const topicLine = topics.length
            ? `\nTopics: [${topics.map((t: any) => t.name).join("] [")}]`
            : "";
          const refLine = refDoc
            ? `\nRef: ${refDoc.title}`
            : "";

          const content = typeof mem.content === "string" ? mem.content : JSON.stringify(mem);
          return `${content}${topicLine}${refLine}`;
        }).join("\n\n---\n\n");
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

            // If topic has top_memories, append inline preview
            const topMems = (t as any).top_memories || [];
            let memPreview = "";
            if (topMems.length > 0) {
              memPreview = "\n  Top: " + topMems
                .slice(0, 3)
                .map((m: any) => {
                  const content = typeof m.content === "string" ? m.content : "";
                  return content.length > 60 ? content.slice(0, 60) + "…" : content;
                })
                .join(" | ");
              const total = (t as any).memory_count || 0;
              if (total > 3) {
                memPreview += ` [+${total - 3} more]`;
              }
            }

            return `${name}${id}${desc}${memPreview}`;
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

function makeLinkTopicTool(
  _ctx: OpenClawPluginToolContext
): AnyAgentTool {
  return {
    name: "link_topic",
    description:
      "Link an existing memory to an existing topic. " +
      "Use this after saving a memory and seeing topic suggestions. " +
      "The memory and topic must both already exist.",
    parameters: LINK_TOPIC_SCHEMA,
    emoji_name: "link",
    execute: async (toolCallId, params) => {
      const {
        memory_id = "",
        topic_id = "",
      } = params as {
        memory_id: string;
        topic_id: string;
      };

      if (!memory_id.trim() || !topic_id.trim()) {
        return {
          toolCallId,
          result: {
            type: "ok" as const,
            results: [
              {
                content: [
                  {
                    type: "text" as const,
                    text: '{"success":false,"error":"missing_params","message":"memory_id and topic_id are required"}',
                  },
                ],
              },
            ],
          },
        };
      }

      const result = await runMpm([
        "topic", "link",
        topic_id,
        memory_id,
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

// ── Session Wake Context ─────────────────────────────────────────────────────

function makeReadWakeContextTool(
  _ctx: OpenClawPluginToolContext
): AnyAgentTool {
  return {
    name: "read_wake_context",
    description:
      "Read the agent's wake context — session state from the previous session including " +
      "active mode, active persona, recent topics, and recent memories. " +
      "This is the first thing to check on session start to understand where you left off. " +
      "Call this immediately on session start before doing anything else.",
    parameters: READ_WAKE_CONTEXT_SCHEMA,
    emoji_name: "sunrise",
    execute: async (toolCallId, _params) => {
      const result = await runMpm(["wake", "--json"]);
      const data = parseMpmResult(result);

      if (!data) {
        return {
          toolCallId,
          result: {
            type: "ok" as const,
            results: [
              {
                content: [
                  {
                    type: "text" as const,
                    text: "(no wake context available — no previous session found)",
                  },
                ],
              },
            ],
          },
        };
      }

      const ctx = data;
      const lines: string[] = [
        "\xf0\x9f\x8c\x9e **WAKE CONTEXT** \xf0\x9f\x8c\x9e\n",
      ];

      if (ctx.mode || ctx.active_mode) {
        lines.push(`\n  **Mode:** ${ctx.mode || ctx.active_mode}`);
      }
      if (ctx.persona || ctx.active_persona) {
        lines.push(`\n  **Persona:** ${ctx.persona || ctx.active_persona}`);
      }
      if (ctx.recent_topics && ctx.recent_topics.length > 0) {
        lines.push(`\n  **Recent Topics:** ${(ctx.recent_topics as string[]).join(", ")}`);
      }

      if (ctx.recent_memories && Array.isArray(ctx.recent_memories) && ctx.recent_memories.length > 0) {
        lines.push(`\n  **Recent Memories:**`);
        for (const mem of ctx.recent_memories as Array<{ content?: string; created_at?: string }>) {
          const content = (mem.content || "").substring(0, 80);
          const age = mem.created_at
            ? ` (${formatAge(mem.created_at)})`
            : "";
          lines.push(`\n    \u2022 ${content}${age}`);
        }
      }

      const displayText = lines.join("");

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

  // ── helpers ────────────────────────────────────────────────────────────────

  /** Format a directive's created_at timestamp as a human-readable age. */
  function formatAge(createdAt: string): string {
    if (!createdAt) return "unknown age";
    let created: Date;
    try {
      // Try parsing ISO-8601 with fallback for space-separated datetime
      created = new Date(createdAt.replace(" ", "T"));
      if (isNaN(created.getTime())) created = new Date(createdAt);
    } catch {
      return "unknown age";
    }
    if (isNaN(created.getTime())) return "unknown age";

    const diffMs = Date.now() - created.getTime();
    const diffSec = Math.floor(diffMs / 1000);
    if (diffSec < 60) return `${diffSec}s ago`;
    const diffMin = Math.floor(diffSec / 60);
    if (diffMin < 60) return `${diffMin}m ago`;
    const diffHr = Math.floor(diffMin / 60);
    if (diffHr < 24) return `${diffHr}h ago`;
    const diffDays = Math.floor(diffHr / 24);
    if (diffDays < 30) return `${diffDays}d ago`;
    const diffWeeks = Math.floor(diffDays / 7);
    if (diffWeeks < 12) return `${diffWeeks}w ago`;
    const diffMonths = Math.floor(diffDays / 30);
    return `${diffMonths}mo ago`;
  }

  /** Group an array of directive objects by their collection label. */
  function groupByCollection(
    directives: Array<{ collection?: string; content?: string; created_at?: string; id?: string }>
  ): Map<string, Array<{ content: string; created_at: string; id: string }>> {
    const groups = new Map<string, Array<{ content: string; created_at: string; id: string }>>();
    for (const d of directives) {
      const key = (d.collection || "directive").trim();
      if (!groups.has(key)) groups.set(key, []);
      groups.get(key)!.push({
        content: typeof d.content === "string" ? d.content : JSON.stringify(d.content),
        created_at: d.created_at || "",
        id: d.id || "",
      });
    }
    return groups;
  }

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

      if (!data.directives || !Array.isArray(data.directives) || data.directives.length === 0) {
        return {
          toolCallId,
          result: {
            type: "ok" as const,
            results: [
              {
                content: [
                  {
                    type: "text" as const,
                    text: data.message
                      ? String(data.message)
                      : "(no directives defined — run the session that defines them)",
                  },
                ],
              },
            ],
          },
        };
      }

      const groups = groupByCollection(data.directives as Array<{
        collection?: string;
        content?: string;
        created_at?: string;
        id?: string;
      }>);

      const lines: string[] = [
        "\xf0\x9f\x9b\xb8 **808 PRIME DIRECTIVES** \xf0\x9f\x9b\xb8\n",
      ];

      for (const [group, directives] of groups) {
        lines.push(`\n**${group.toUpperCase()}**\n`);
        for (const d of directives) {
          const age = formatAge(d.created_at);
          lines.push(`\n  \u2022 ${d.content}`);
          lines.push(`    \u2192 added ${age}`);
        }
      }

      const displayText = lines.join("\n");

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

// ── Epistemology Engine ─────────────────────────────────────────────────────────


function makeRecordDecisionTool(
  _ctx: OpenClawPluginToolContext
): AnyAgentTool {
  return {
    name: "record_decision",
    description:
      "Record an architectural decision, library choice, or any moment where you chose " +
      "path A over path B. Call this BEFORE or AFTER the decision — not just after. " +
      "The rationale is the most important field: it is what makes past decisions " +
      "reusable when you encounter a similar problem weeks later. " +
      "Trigger: whenever you weigh tradeoffs and choose one, or whenever you notice " +
      "you just picked an approach without recording why.",
    parameters: RECORD_DECISION_SCHEMA,
    emoji_name: "scales",
    execute: async (toolCallId, params) => {
      const {
        context = "",
        choice = "",
        rationale = "",
        outcome = "",
        tags = [],
        weight = 0.5,
      } = params as {
        context: string;
        choice: string;
        rationale: string;
        outcome?: string;
        tags?: string[];
        weight?: number;
      };


      if (!context.trim() || !choice.trim() || !rationale.trim()) {
        return {
          toolCallId,
          result: {
            type: "ok" as const,
            results: [
              {
                content: [
                  {
                    type: "text" as const,
                    text: JSON.stringify({
                      success: false,
                      error: "missing_required_field",
                      message: "context, choice, and rationale are all required.",
                    }),
                  },
                ],
              },
            ],
          },
        };
      }

      const content = [
        `CONTEXT: ${context}`,
        `CHOICE: ${choice}`,
        `RATIONALE: ${rationale}`,
        outcome ? `OUTCOME: ${outcome}` : ``,
      ]
        .filter(line => line.length > `CONTEXT: `.length)
        .join("\n");


      const args = ["add", "--json", "--collection", "decisions", "--", content];
      for (const tag of tags) { args.push("--tag", tag); }
      if (weight !== 0.5) { args.push("--weight", String(weight)); }

      const result = await runMpm(args);
      const data = parseMpmResult(result);

      return {
        toolCallId,
        result: {
          type: "ok" as const,
          results: [{ content: [{ type: "text" as const, text: JSON.stringify(data) }] }],
        },
      };
    },
  };
}

// ── Proactive Recall Hint ─────────────────────────────────────────────────────

const PROACTIVE_RECALL_HINT_SCHEMA = {
  type: "object",
  properties: {
    conversation_text: {
      type: "string",
      description: "Recent conversation context to check for semantically relevant decisions and theories.",
    },
    max_hints: {
      type: "number",
      description: "Maximum number of hint results to return (default: 3).",
      default: 3,
    },
    min_score: {
      type: "number",
      description: "Minimum bm25 score threshold — lower (more negative) = stronger match (default: -3.0).",
      default: -3.0,
    },
  },
  required: ["conversation_text"],
  additionalProperties: false,
} as const;

function makeProactiveRecallHintTool(
  _ctx: OpenClawPluginToolContext
): AnyAgentTool {
  return {
    name: "proactive_recall_hint",
    description:
      "Checks recent conversation context for overlap with decisions and theories. " +
      "Returns a structured Recall Hint if a semantic match is found. " +
      "Call this after each user message — one hint per turn max. " +
      "Surface only the top hint (rank 0) when hints are returned.",
    parameters: PROACTIVE_RECALL_HINT_SCHEMA,
    emoji_name: "bell",
    execute: async (toolCallId, params) => {
      const {
        conversation_text = "",
        max_hints = 3,
        min_score = -3.0,
      } = params as {
        conversation_text: string;
        max_hints?: number;
        min_score?: number;
      };

      if (!conversation_text.trim()) {
        return {
          toolCallId,
          result: {
            type: "ok" as const,
            results: [{ content: [{ type: "text" as const, text: "[]" }] }],
          },
        };
      }

      const result = await runMpm([
        "hint", "--json", "--max", String(max_hints), "--", conversation_text,
      ]);
      const data = JSON.parse(result.stdout || "[]");

      // Format hints as structured RecallHint[] for the agent
      const hints = (Array.isArray(data) ? data : []).map((m: any) => {
        const content = m.content || "";
        const meta = m.metadata || {};
        let status = meta.status || "";
        const conclusion = meta.conclusion || "";

        let choice = "";
        let rationale = "";
        let hypothesis = "";

        // Extract fields from content
        if (m.collection === "decisions") {
          const firstLine = content.split("\n")[0] || "";
          if (firstLine.toUpperCase().startsWith("CHOICE: ")) {
            choice = firstLine.slice(7).trim();
          }
          const rLine = content.split("\n").find((l: string) =>
            l.toUpperCase().startsWith("RATIONALE:")
          );
          if (rLine) {
            rationale = rLine.slice(10).trim();
          }
        } else if (m.collection === "theories") {
          const firstLine = content.split("\n")[0] || "";
          if (firstLine.toUpperCase().startsWith("HYPOTHESIS: ")) {
            hypothesis = firstLine.slice(11).trim();
          }
        }

        if (!status) {
          // Fallback: parse STATUS from content
          const sLine = content.split("\n").find((l: string) =>
            l.toUpperCase().startsWith("STATUS:")
          );
          if (sLine) {
            status = sLine.split(":")[1]?.trim() || "";
          }
        }

        return {
          id: m.id,
          collection: m.collection,
          content: m.content,
          status,
          conclusion,
          rationale,
          choice,
          hypothesis,
          relevance_score: m.score,
          created_at: m.created_at,
        };
      });

      return {
        toolCallId,
        result: {
          type: "ok" as const,
          results: [{ content: [{ type: "text" as const, text: JSON.stringify(hints) }] }],
        },
      };
    },
  };
}

function makeProposeTheoryTool(
  _ctx: OpenClawPluginToolContext
): AnyAgentTool {
  return {
    name: "propose_theory",
    description:
      "Log a hypothesis about causality before writing a fix. " +
      "When you think 'X is probably causing Y' — propose it, define the test, then run the test. " +
      "Writing the validation criteria forces you to confront whether the assumption is actually testable, " +
      "and often collapses a false hypothesis before it wastes an hour of debugging time. " +
      "Trigger: whenever you form a 'I think X is causing Y' assumption during debugging or design work.",
    parameters: PROPOSE_THEORY_SCHEMA,
    emoji_name: "bulb",
    execute: async (toolCallId, params) => {
      const {
        hypothesis = "",
        validationCriteria = "",
        tags = [],
      } = params as {
        hypothesis: string;
        validationCriteria: string;
        tags?: string[];
      };

      if (!hypothesis.trim() || !validationCriteria.trim()) {
        return {
          toolCallId,
          result: {
            type: "ok" as const,
            results: [
              {
                content: [
                  {
                    type: "text" as const,
                    text: JSON.stringify({
                      success: false,
                      error: "missing_required_field",
                      message: "hypothesis and validationCriteria are both required.",
                    }),
                  },
                ],
              },
            ],
          },
        };
      }

      const content = [
        `HYPOTHESIS: ${hypothesis}`,
        `VALIDATION_CRITERIA: ${validationCriteria}`,
        `STATUS: pending`,
      ].join("\n");

      const args = ["add", "--json", "--collection", "theories", "--", content];
      for (const tag of tags) { args.push("--tag", tag); }
      args.push("--tag", "theory");

      const result = await runMpm(args);
      const data = parseMpmResult(result);

      return {
        toolCallId,
        result: {
          type: "ok" as const,
          results: [{ content: [{ type: "text" as const, text: JSON.stringify(data) }] }],
        },
      };
    },
  };
}

function makeResolveTheoryTool(
  _ctx: OpenClawPluginToolContext
): AnyAgentTool {
  return {
    name: "resolve_theory",
    description:
      "Close the loop on a pending theory after running its validation criteria. " +
      "If the hypothesis was confirmed, save the confirmed result as a permanent memory " +
      "(weight=1.0, include the theory_id as a tag for traceability). " +
      "If it was disproven, record what actually caused the problem instead. " +
      "Trigger: immediately after executing the test described in a pending theory's validationCriteria.",
    parameters: RESOLVE_THEORY_SCHEMA,
    emoji_name: "white_check_mark",
    execute: async (toolCallId, params) => {
      const {
        theoryId = "",
        conclusion = "",
        newStatus = "",
      } = params as {
        theoryId: string;
        conclusion: string;
        newStatus: "proven" | "disproven";
      };

      if (!theoryId.trim() || !conclusion.trim() || !newStatus.trim()) {
        return {
          toolCallId,
          result: {
            type: "ok" as const,
            results: [
              {
                content: [
                  {
                    type: "text" as const,
                    text: JSON.stringify({
                      success: false,
                      error: "missing_required_field",
                      message: "theoryId, conclusion, and newStatus are all required.",
                    }),
                  },
                ],
              },
            ],
          },
        };
      }

      if (newStatus !== "proven" && newStatus !== "disproven") {
        return {
          toolCallId,
          result: {
            type: "ok" as const,
            results: [
              {
                content: [
                  {
                    type: "text" as const,
                    text: JSON.stringify({
                      success: false,
                      error: "invalid_status",
                      message: "newStatus must be 'proven' or 'disproven'.",
                    }),
                  },
                ],
              },
            ],
          },
        };
      }

      // Use UpdateMemoryMetadata via mpm patch-memory to update the theory's
      // metadata in-place (no content change → no FTS re-index).
      // Then insert a new resolved record for the conclusion.
      const patchResult = await runMpm([
        "patch-memory", theoryId,
        JSON.stringify({ status: newStatus, conclusion }),
      ]);
      const patchData = parseMpmResult(patchResult);

      // Add resolved record with traceability tag back to original theory.
      const resolvedContent = [
        `RESOLVED_THEORY_ID: ${theoryId}`,
        `STATUS: ${newStatus}`,
        `CONCLUSION: ${conclusion}`,
      ].join("\n");

      const addArgs = [
        "add", "--json", "--collection", "theories",
        "--tag", "resolved",
        "--tag", `theories:${theoryId}`,
        "--weight", "1",
        "--", resolvedContent,
      ];

      const addResult = await runMpm(addArgs);
      const addData = parseMpmResult(addResult);

      return {
        toolCallId,
        result: {
          type: "ok" as const,
          results: [{ content: [{ type: "text" as const, text: JSON.stringify({ patch: patchData, resolved: addData }) }] }],
        },
      };
    },
  };
}

function makeChallengeTool(
  _ctx: OpenClawPluginToolContext
): AnyAgentTool {
  return {
    name: "challenge_memory",
    description:
      "Challenge an existing memory by presenting contradictory evidence. " +
      "This weakens the memory, creates a pending theory, and logs a decision. " +
      "Use when you discover that a stored memory is no longer accurate. " +
      "Trigger: conversation or test results contradict a specific stored memory.",
    parameters: CHALLENGE_MEMORY_SCHEMA,
    emoji_name: "warning",
    execute: async (toolCallId, params) => {
      const {
        memoryId = "",
        evidence = "",
      } = params as {
        memoryId: string;
        evidence: string;
      };

      if (!memoryId.trim() || !evidence.trim()) {
        return {
          toolCallId,
          result: {
            type: "ok" as const,
            results: [
              {
                content: [
                  {
                    type: "text" as const,
                    text: JSON.stringify({
                      success: false,
                      error: "missing_required_field",
                      message: "memoryId and evidence are both required.",
                    }),
                  },
                ],
              },
            ],
          },
        };
      }

      const result = await runMpm([
        "challenge", memoryId, "--",
        evidence,
      ]);
      const data = parseMpmResult(result);

      return {
        toolCallId,
        result: {
          type: "ok" as const,
          results: [{ content: [{ type: "text" as const, text: JSON.stringify(data) }] }],
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

    api.registerTool(
      (ctx: OpenClawPluginToolContext) => makeLinkTopicTool(ctx),
      { names: ["link_topic"], optional: true }
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

    // ── Session Wake Context ──────────────────────────────────────────────
    api.registerTool(
      (ctx: OpenClawPluginToolContext) => makeReadWakeContextTool(ctx),
      { names: ["read_wake_context"], optional: false }
    );

    // ── Directive Tools ──────────────────────────────────────────────────
    api.registerTool(
      (ctx: OpenClawPluginToolContext) => makeReadDirectivesTool(ctx),
      { names: ["read_directives"], optional: false }
    );

    // ── Proactive Recall Hint ───────────────────────────────────────────
    api.registerTool(
      (_ctx: OpenClawPluginToolContext) => makeProactiveRecallHintTool(_ctx),
      { names: ["proactive_recall_hint"], optional: true }
    );

    // ── Epistemology Engine ──────────────────────────────────────────────
    api.registerTool(
      (ctx: OpenClawPluginToolContext) => makeRecordDecisionTool(ctx),
      { names: ["record_decision"], optional: false }
    );

    api.registerTool(
      (ctx: OpenClawPluginToolContext) => makeProposeTheoryTool(ctx),
      { names: ["propose_theory"], optional: false }
    );

    api.registerTool(
      (ctx: OpenClawPluginToolContext) => makeResolveTheoryTool(ctx),
      { names: ["resolve_theory"], optional: false }
    );

    api.registerTool(
      (ctx: OpenClawPluginToolContext) => makeChallengeTool(ctx),
      { names: ["challenge_memory"], optional: false }
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