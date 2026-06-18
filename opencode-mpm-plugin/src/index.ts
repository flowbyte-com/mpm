import { spawn } from "node:child_process";
import { tool } from "@opencode-ai/plugin";
import type { Plugin, PluginModule, PluginInput } from "@opencode-ai/plugin";

const MAX_BUFFER = 10 * 1024 * 1024;

interface MpmRunResult {
  stdout: string;
  stderr: string;
  exitCode: number;
}

function makeRunMpm(settings: { binary: string; workspace: string }) {
  return async function runMpm(
    args: string[],
    timeoutMs = 15000,
    signal?: AbortSignal,
  ): Promise<MpmRunResult> {
    return new Promise((resolve) => {
      let stdout = "";
      let stderr = "";
      let settled = false;

      const env: Record<string, string | undefined> = { ...process.env };
      env.MPM_WORKSPACE = settings.workspace;

      const proc = spawn(settings.binary, args, {
        env,
        stdio: ["ignore", "pipe", "pipe"],
        signal,
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
        if (stdout.length + chunk.length > MAX_BUFFER) {
          settled = true;
          clearTimeout(timer);
          proc.kill("SIGKILL");
          resolve({
            stdout,
            stderr: `${stderr}\n[output exceeded ${MAX_BUFFER} bytes]`.trim(),
            exitCode: 125,
          });
          return;
        }
        stdout += chunk.toString();
      });

      proc.stderr?.on("data", (chunk: Buffer) => {
        if (stderr.length + chunk.length > MAX_BUFFER) {
          stderr = (stderr + chunk.toString()).slice(-MAX_BUFFER);
        } else {
          stderr += chunk.toString();
        }
      });

      proc.on("error", (err: NodeJS.ErrnoException) => {
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

      proc.on("close", (code: number | null) => {
        if (!settled) {
          settled = true;
          clearTimeout(timer);
          resolve({ stdout, stderr, exitCode: code ?? 0 });
        }
      });
    });
  };
}

function parseMpmResult(result: MpmRunResult): Record<string, unknown> {
  const { stdout, stderr, exitCode } = result;

  if (exitCode !== 0) {
    if (exitCode === 125 && stderr.includes("[output exceeded")) {
      return {
        id: "",
        success: false,
        error: "wake_context_truncated",
        message:
          "Wake context exceeds buffer limit — session too large for the wake context tool.",
      };
    }
    if (stderr.includes("database is locked") || stderr.includes("SQLITE_BUSY")) {
      return {
        id: "",
        success: false,
        error: "database_locked",
        message: "MPM database is locked — safe to retry.",
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
  if (!trimmed) return { id: "", success: true, count: 0 };

  try {
    return JSON.parse(trimmed) as Record<string, unknown>;
  } catch {
    return { id: "", success: true, text: trimmed };
  }
}

function makeCallMpmCall(runMpm: ReturnType<typeof makeRunMpm>) {
  return async function callMpmCall(
    toolName: string,
    payload: Record<string, unknown>,
    timeoutMs = 15000,
    signal?: AbortSignal,
  ): Promise<Record<string, unknown>> {
    const result = await runMpm(
      ["call", toolName, "--payload", JSON.stringify(payload)],
      timeoutMs,
      signal,
    );
    return parseMpmResult(result);
  };
}

function formatAge(createdAt: string): string {
  if (!createdAt) return "unknown age";
  let created: Date;
  try {
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

const MpmPlugin: Plugin = async (ctx: PluginInput) => {
  const binary = process.env.MPM_BINARY ?? "mpm";
  const workspace = process.env.MPM_WORKSPACE ?? ctx.worktree ?? ctx.directory ?? "";
  const runMpm = makeRunMpm({ binary, workspace });
  const callMpmCall = makeCallMpmCall(runMpm);

  return {
    tool: {
      // ── Core Memory ─────────────────────────────────────────────────
      query_long_term_memory: tool({
        description:
          "Search MPM long-term memory. Before answering anything about prior work, decisions, dates, people, preferences, or todos — run this first. Returns matching memories as formatted text.",
        args: {
          query: tool.schema
            .string()
            .describe("Natural language search query for long-term memory."),
          limit: tool.schema
            .number()
            .default(5)
            .describe("Maximum number of results to return."),
        },
        async execute(args, ctx) {
          const data = await callMpmCall("query_long_term_memory", args, 15000, ctx.abort);
          const memories = data.memories;
          if (Array.isArray(memories) && memories.length > 0) {
            return memories
              .map((m: Record<string, unknown>) => {
                const content =
                  typeof m.content === "string"
                    ? m.content
                    : JSON.stringify(m.content);
                const xref = (m.cross_references as Record<string, unknown>) || {};
                const topics = (xref.topics as Array<{ name: string }>) || [];
                const refDoc = xref.reference_doc as { title?: string } | undefined;
                const topicLine = topics.length
                  ? `\nTopics: [${topics.map((t) => t.name).join("] [")}]`
                  : "";
                const refLine = refDoc?.title ? `\nRef: ${refDoc.title}` : "";
                return `${content}${topicLine}${refLine}`;
              })
              .join("\n\n---\n\n");
          }
          if (data.text) return String(data.text);
          return "(no matching memories found)";
        },
      }),

      save_to_memory: tool({
        description:
          "Persist a fact, lesson, or decision to MPM long-term memory. After any non-trivial action, lesson learned, or decision — call this. Tags help later retrieval. Weight 0.5 by default; higher for important truths. Use TTL '24h' for session-scoped facts, '0' for permanent.",
        args: {
          fact: tool.schema.string().describe("The fact, lesson, or decision to persist."),
          tags: tool.schema
            .array(tool.schema.string())
            .default([])
            .describe("Tags for categorization."),
          weight: tool.schema
            .number()
            .default(0.5)
            .describe("Importance weight between 0 and 1."),
          ttl: tool.schema
            .string()
            .optional()
            .describe("TTL: '24h' for session-scoped, '0' for permanent, or Go duration."),
          collection: tool.schema
            .string()
            .default("memories")
            .describe("Collection name."),
        },
        async execute(args, ctx) {
          if (!args.fact.trim()) {
            return JSON.stringify({
              success: false,
              error: "empty_fact",
              message: "fact cannot be empty",
            });
          }
          const payload: Record<string, unknown> = {
            fact: args.fact,
            tags: args.tags,
            weight: args.weight,
          };
          if (args.ttl) payload.ttl = args.ttl;
          if (args.collection) payload.collection = args.collection;
          const data = await callMpmCall("save_to_memory", payload, 15000, ctx.abort);
          return JSON.stringify(data);
        },
      }),

      challenge_memory: tool({
        description:
          "Challenge an existing memory with contradictory evidence. Weakens the memory, creates a pending theory, and logs a decision. Use when conversation or test results contradict a stored memory.",
        args: {
          memoryId: tool.schema.string().describe("The MPM memory ID to challenge."),
          evidence: tool.schema
            .string()
            .describe(
              "Evidence contradicting the memory — specific details about what changed.",
            ),
        },
        async execute(args, ctx) {
          if (!args.memoryId.trim() || !args.evidence.trim()) {
            return JSON.stringify({
              success: false,
              error: "missing_required_field",
              message: "memoryId and evidence are required",
            });
          }
          const data = await callMpmCall(
            "challenge_memory",
            { memoryId: args.memoryId, evidence: args.evidence },
            15000,
            ctx.abort,
          );
          return JSON.stringify(data);
        },
      }),

      // ── Lessons ────────────────────────────────────────────────────
      save_lesson: tool({
        description:
          "Persist a lesson to MPM — what was learned, observed, or should be remembered. Types: 'warning' (don't do X), 'practice' (do Y), 'insight' (X leads to Y).",
        args: {
          fact: tool.schema.string().describe("The lesson content."),
          type: tool.schema
            .enum(["warning", "practice", "insight"])
            .default("insight")
            .describe("Lesson type."),
          tags: tool.schema
            .array(tool.schema.string())
            .default([])
            .describe("Optional tags for the lesson."),
        },
        async execute(args, ctx) {
          if (!args.fact.trim()) {
            return JSON.stringify({
              success: false,
              error: "empty_fact",
              message: "fact cannot be empty",
            });
          }
          const data = await callMpmCall(
            "save_lesson",
            { fact: args.fact, type: args.type, tags: args.tags },
            15000,
            ctx.abort,
          );
          return JSON.stringify(data);
        },
      }),

      search_lessons: tool({
        description:
          "Search MPM lessons for relevant learned knowledge. Use to recall warnings, best practices, and insights before acting.",
        args: {
          query: tool.schema.string().describe("Search query for lesson content."),
        },
        async execute(args, ctx) {
          const data = await callMpmCall(
            "search_lessons",
            { query: args.query, limit: 10 },
            15000,
            ctx.abort,
          );
          const results = data.results;
          if (Array.isArray(results) && results.length > 0) {
            return results
              .map((l: Record<string, unknown>) => {
                const lessonType = l.type || "insight";
                const content =
                  typeof l.content === "string"
                    ? l.content
                    : JSON.stringify(l.content);
                return `[${lessonType}] ${content}`;
              })
              .join("\n\n---\n\n");
          }
          if (data.message) return String(data.message);
          return "(no matching lessons found)";
        },
      }),

      list_lessons: tool({
        description:
          "List all lessons in MPM, optionally filtered by type: 'warning', 'practice', 'insight'.",
        args: {
          type: tool.schema
            .enum(["warning", "practice", "insight"])
            .optional()
            .describe("Filter by lesson type."),
        },
        async execute(args, ctx) {
          const payload: Record<string, unknown> = {};
          if (args.type) payload.type = args.type;
          const data = await callMpmCall("list_lessons", payload, 15000, ctx.abort);
          const lessons = data.lessons;
          if (Array.isArray(lessons) && lessons.length > 0) {
            return lessons
              .map((l: Record<string, unknown>) => {
                const lessonType = l.type || "insight";
                const content =
                  typeof l.content === "string"
                    ? l.content
                    : JSON.stringify(l.content);
                const created = l.created_at
                  ? ` (${String(l.created_at).slice(0, 10)})`
                  : "";
                return `[${lessonType}]${created} ${content}`;
              })
              .join("\n");
          }
          if (data.message) return String(data.message);
          return "(no lessons stored)";
        },
      }),

      // ── Topics ─────────────────────────────────────────────────────
      create_topic: tool({
        description:
          "Create a topic in MPM to organize related memories and knowledge. Topics group related memories and can be searched.",
        args: {
          name: tool.schema.string().describe("Topic name."),
          description: tool.schema
            .string()
            .optional()
            .describe("Optional topic description."),
        },
        async execute(args, ctx) {
          if (!args.name.trim()) {
            return JSON.stringify({
              success: false,
              error: "empty_name",
              message: "name cannot be empty",
            });
          }
          const data = await callMpmCall(
            "create_topic",
            { name: args.name, description: args.description ?? "" },
            15000,
            ctx.abort,
          );
          return JSON.stringify(data);
        },
      }),

      search_topics: tool({
        description:
          "Search MPM topics for relevant knowledge clusters. Topics group related memories and provide context.",
        args: {
          query: tool.schema.string().describe("Search query for topics."),
          limit: tool.schema
            .number()
            .default(20)
            .describe("Maximum number of results."),
        },
        async execute(args, ctx) {
          const data = await callMpmCall(
            "search_topics",
            { query: args.query, limit: args.limit },
            15000,
            ctx.abort,
          );
          const results = data.results;
          if (Array.isArray(results) && results.length > 0) {
            return results
              .map((t: Record<string, unknown>) => {
                const name = t.name || t.description || "(unnamed)";
                const id = t.id ? ` [${t.id}]` : "";
                const desc = t.description ? `: ${t.description}` : "";
                const topMems = (t as Record<string, unknown>).top_memories as
                  | Array<{ content?: string }>
                  | undefined;
                let memPreview = "";
                if (topMems && topMems.length > 0) {
                  memPreview =
                    "\n  Top: " +
                    topMems
                      .slice(0, 3)
                      .map((m) => {
                        const c = m.content || "";
                        return c.length > 60 ? c.slice(0, 60) + "\u2026" : c;
                      })
                      .join(" | ");
                  const total = (t as Record<string, unknown>).memory_count as
                    | number
                    | undefined;
                  if (total && total > 3) memPreview += ` [+${total - 3} more]`;
                }
                return `${name}${id}${desc}${memPreview}`;
              })
              .join("\n");
          }
          if (data.message) return String(data.message);
          return "(no matching topics found)";
        },
      }),

      link_topic: tool({
        description:
          "Link an existing memory to an existing topic. Both must already exist.",
        args: {
          memory_id: tool.schema.string().describe("The memory ID to link."),
          topic_id: tool.schema.string().describe("The topic ID to link to."),
        },
        async execute(args, ctx) {
          if (!args.memory_id.trim() || !args.topic_id.trim()) {
            return JSON.stringify({
              success: false,
              error: "missing_params",
              message: "memory_id and topic_id are required",
            });
          }
          const data = await callMpmCall(
            "link_topic",
            { memory_id: args.memory_id, topic_id: args.topic_id },
            15000,
            ctx.abort,
          );
          return JSON.stringify(data);
        },
      }),

      // ── References ────────────────────────────────────────────────
      add_reference: tool({
        description:
          "Ingest a document as a reference into MPM. Supported: .txt, .md, .html, .epub, .pdf. The document is chunked and indexed for semantic search.",
        args: {
          filepath: tool.schema.string().describe("Path to the file to ingest."),
          title: tool.schema
            .string()
            .optional()
            .describe("Optional title override."),
        },
        async execute(args, ctx) {
          if (!args.filepath.trim()) {
            return JSON.stringify({
              success: false,
              error: "empty_filepath",
              message: "filepath cannot be empty",
            });
          }
          const payload: Record<string, unknown> = { filepath: args.filepath };
          if (args.title) payload.title = args.title;
          const data = await callMpmCall("add_reference", payload, 15000, ctx.abort);
          return JSON.stringify(data);
        },
      }),

      search_references: tool({
        description:
          "Search content within ingested reference documents. Returns matching chunks from the reference library.",
        args: {
          query: tool.schema.string().describe("Search query for reference content."),
          limit: tool.schema
            .number()
            .default(5)
            .describe("Maximum number of results."),
        },
        async execute(args, ctx) {
          const data = await callMpmCall(
            "search_references",
            { query: args.query, limit: args.limit },
            15000,
            ctx.abort,
          );
          const results = data.results;
          if (Array.isArray(results) && results.length > 0) {
            return results
              .map((r: Record<string, unknown>) => {
                const docTitle = r.doc_title || "document";
                const chunkIdx = r.chunk_index ?? 0;
                const content =
                  typeof r.content === "string"
                    ? r.content
                    : JSON.stringify(r.content);
                return `[${docTitle} chunk ${chunkIdx}]\n${content}`;
              })
              .join("\n\n---\n\n");
          }
          if (data.message) return String(data.message);
          return "(no matching reference content found)";
        },
      }),

      list_references: tool({
        description:
          "List all ingested reference documents in MPM. Shows titles, chunk counts, and tags.",
        args: {
          limit: tool.schema
            .number()
            .default(50)
            .describe("Maximum number of results."),
          offset: tool.schema
            .number()
            .default(0)
            .describe("Pagination offset."),
        },
        async execute(args, ctx) {
          const data = await callMpmCall(
            "list_references",
            { limit: args.limit, offset: args.offset },
            15000,
            ctx.abort,
          );
          const refs = data.references;
          if (Array.isArray(refs) && refs.length > 0) {
            return refs
              .map((r: Record<string, unknown>) => {
                const title = r.title || "untitled";
                const chunks = r.total_chunks ?? 0;
                const tags = r.tags ? ` [${r.tags}]` : "";
                const created = r.created_at
                  ? ` \u2014 ${String(r.created_at).slice(0, 10)}`
                  : "";
                return `${title} (${chunks} chunks)${tags}${created}`;
              })
              .join("\n");
          }
          if (data.message) return String(data.message);
          return "(no references stored)";
        },
      }),

      // ── System ─────────────────────────────────────────────────────
      read_wake_context: tool({
        description:
          "Read the agent's wake context — session state from the previous session: active mode, persona, recent topics, and recent memories. Call this on session start to understand where you left off.",
        args: {},
        async execute(_args, ctx) {
          const data = await callMpmCall("read_wake_context", {}, 15000, ctx.abort);
          if (!data || !data.success) {
            return "(no wake context available — no previous session found)";
          }

          const lines: string[] = [];
          if (data.mode || data.active_mode) {
            lines.push(`**Mode:** ${data.mode || data.active_mode}`);
          }
          if (data.persona || data.active_persona) {
            lines.push(`**Persona:** ${data.persona || data.active_persona}`);
          }
          if (Array.isArray(data.recent_topics) && data.recent_topics.length > 0) {
            lines.push(
              `**Recent Topics:** ${(data.recent_topics as string[]).join(", ")}`,
            );
          }
          if (
            data.recent_memories &&
            Array.isArray(data.recent_memories) &&
            data.recent_memories.length > 0
          ) {
            lines.push("**Recent Memories:**");
            for (const mem of data.recent_memories as Array<{
              content?: string;
              created_at?: string;
            }>) {
              const content = (mem.content || "").substring(0, 80);
              const age = mem.created_at ? ` (${formatAge(mem.created_at)})` : "";
              lines.push(`  \u2022 ${content}${age}`);
            }
          }

          return lines.length > 0 ? lines.join("\n") : "(wake context is empty)";
        },
      }),

      read_directives: tool({
        description:
          "Read the agent's prime directives — behavioral rules and operating principles that define what the agent must and must not do.",
        args: {},
        async execute(_args, ctx) {
          const data = await callMpmCall("read_directives", {}, 15000, ctx.abort);
          const directives = data.directives;
          if (!Array.isArray(directives) || directives.length === 0) {
            return data.message
              ? String(data.message)
              : "(no directives defined)";
          }

          const groups = new Map<
            string,
            Array<{ content: string; created_at: string }>
          >();
          for (const d of directives as Array<{
            collection?: string;
            content?: string;
            created_at?: string;
          }>) {
            const key = (d.collection || "directive").trim();
            if (!groups.has(key)) groups.set(key, []);
            groups.get(key)!.push({
              content:
                typeof d.content === "string"
                  ? d.content
                  : JSON.stringify(d.content),
              created_at: d.created_at || "",
            });
          }

          const lines: string[] = [];
          for (const [group, dirs] of groups) {
            lines.push(`**${group.toUpperCase()}**`);
            for (const d of dirs) {
              lines.push(`  \u2022 ${d.content}`);
              lines.push(`    \u2192 added ${formatAge(d.created_at)}`);
            }
          }
          return lines.join("\n");
        },
      }),

      // ── Epistemology ──────────────────────────────────────────────
      propose_theory: tool({
        description:
          "Log a hypothesis about causality before writing a fix. When you think 'X is probably causing Y' — propose it, define the test, then run the test. Writing validation criteria forces specificity and often collapses false hypotheses early.",
        args: {
          hypothesis: tool.schema.string().describe("The hypothesis or assumption."),
          validationCriteria: tool.schema
            .string()
            .describe(
              "A specific, executable test or observation that would prove or disprove the hypothesis.",
            ),
          tags: tool.schema
            .array(tool.schema.string())
            .default([])
            .describe("Optional tags."),
        },
        async execute(args, ctx) {
          if (!args.hypothesis.trim() || !args.validationCriteria.trim()) {
            return JSON.stringify({
              success: false,
              error: "missing_required_field",
              message: "hypothesis and validationCriteria are required",
            });
          }
          const data = await callMpmCall(
            "propose_theory",
            {
              hypothesis: args.hypothesis,
              validation_criteria: args.validationCriteria,
              tags: args.tags,
            },
            15000,
            ctx.abort,
          );
          return JSON.stringify(data);
        },
      }),

      resolve_theory: tool({
        description:
          "Close the loop on a pending theory after running its validation criteria. If proven, save as permanent memory. If disproven, record what actually caused the problem instead.",
        args: {
          theoryId: tool.schema.string().describe("The theory ID to resolve."),
          conclusion: tool.schema
            .string()
            .describe("What was concluded after running validation."),
          newStatus: tool.schema
            .enum(["proven", "disproven"])
            .describe("Whether the hypothesis was proven or disproven."),
        },
        async execute(args, ctx) {
          if (
            !args.theoryId.trim() ||
            !args.conclusion.trim() ||
            !args.newStatus.trim()
          ) {
            return JSON.stringify({
              success: false,
              error: "missing_required_field",
              message: "theoryId, conclusion, and newStatus are required",
            });
          }
          const data = await callMpmCall(
            "resolve_theory",
            {
              theoryId: args.theoryId,
              conclusion: args.conclusion,
              newStatus: args.newStatus,
            },
            15000,
            ctx.abort,
          );
          return JSON.stringify(data);
        },
      }),

      record_decision: tool({
        description:
          "Record an architectural decision, library choice, or any moment where you chose path A over path B. The rationale is the most important field — it makes past decisions reusable weeks later.",
        args: {
          context: tool.schema.string().describe("The situation or problem requiring a decision."),
          choice: tool.schema.string().describe("What was decided."),
          rationale: tool.schema
            .string()
            .describe("Why this path was chosen over alternatives."),
          outcome: tool.schema
            .string()
            .optional()
            .describe("What happened when the decision was executed."),
          tags: tool.schema
            .array(tool.schema.string())
            .default([])
            .describe("Optional tags."),
          weight: tool.schema
            .number()
            .default(0.5)
            .describe("Decision weight."),
        },
        async execute(args, ctx) {
          if (!args.context.trim() || !args.choice.trim() || !args.rationale.trim()) {
            return JSON.stringify({
              success: false,
              error: "missing_required_field",
              message: "context, choice, and rationale are required",
            });
          }
          const payload: Record<string, unknown> = {
            context: args.context,
            choice: args.choice,
            rationale: args.rationale,
            tags: args.tags,
            weight: args.weight,
          };
          if (args.outcome) payload.outcome = args.outcome;
          const data = await callMpmCall("record_decision", payload, 15000, ctx.abort);
          return JSON.stringify(data);
        },
      }),

      proactive_recall_hint: tool({
        description:
          "Check recent conversation context for overlap with decisions and theories. Returns structured hints if semantic matches are found. Call this after each user message — surface only the top hint.",
        args: {
          conversation_text: tool.schema
            .string()
            .describe("Recent conversation context to check for relevant decisions and theories."),
          max_hints: tool.schema
            .number()
            .default(3)
            .describe("Maximum hint results."),
          min_score: tool.schema
            .number()
            .default(-3.0)
            .describe("Minimum BM25 score threshold (lower = stronger match)."),
        },
        async execute(args, ctx) {
          if (!args.conversation_text.trim()) return "[]";
          const data = await callMpmCall(
            "proactive_recall_hint",
            {
              conversation_text: args.conversation_text,
              max_hints: args.max_hints,
              min_score: args.min_score,
            },
            15000,
            ctx.abort,
          );
          return JSON.stringify(data.hints ?? []);
        },
      }),
    },
  };
};

export default {
  server: MpmPlugin,
} satisfies PluginModule;
