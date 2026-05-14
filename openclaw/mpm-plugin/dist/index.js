// src/index.ts
import { definePluginEntry } from "/home/v/.nvm/versions/node/v24.14.1/lib/node_modules/openclaw/dist/plugin-sdk/plugin-entry.js";
var MPM_BINARY = process.env.MPM_BINARY ?? "/home/v/workspace/projects/mpm/bin/mpm";
var MPM_WORKSPACE = process.env.MPM_WORKSPACE ?? "/home/v/workspace/projects/mpm";
var QUERY_LONG_TERM_MEMORY_SCHEMA = {
  type: "object",
  properties: {
    query: {
      type: "string",
      description: "Natural language search query for long-term memory."
    },
    limit: {
      type: "number",
      description: "Maximum number of results to return (default: 5).",
      default: 5
    }
  },
  required: ["query"],
  additionalProperties: false
};
var SAVE_TO_MEMORY_SCHEMA = {
  type: "object",
  properties: {
    fact: {
      type: "string",
      description: "The fact, lesson, or decision to persist to long-term memory."
    },
    tags: {
      type: "array",
      items: { type: "string" },
      description: "Optional tags for categorization and later retrieval.",
      default: []
    },
    weight: {
      type: "number",
      description: "Importance weight between 0 and 1 (default: 0.5).",
      default: 0.5
    },
    ttl: {
      type: "string",
      description: "Time-to-live as a duration string, e.g. '24h', '7d', '0' (default '0' = no expiry). Use '24h' for ephemeral session-scoped facts, '0' for permanent memories."
    }
  },
  required: ["fact"],
  additionalProperties: false
};
var SAVE_LESSON_SCHEMA = {
  type: "object",
  properties: {
    fact: {
      type: "string",
      description: "The lesson content \u2014 what was learned or observed."
    },
    type: {
      type: "string",
      enum: ["warning", "practice", "insight"],
      description: "Lesson type.",
      default: "insight"
    },
    tags: {
      type: "array",
      items: { type: "string" },
      description: "Optional tags for retrieval.",
      default: []
    }
  },
  required: ["fact"],
  additionalProperties: false
};
var SEARCH_LESSONS_SCHEMA = {
  type: "object",
  properties: {
    query: {
      type: "string",
      description: "Search query for lessons."
    }
  },
  required: ["query"],
  additionalProperties: false
};
var LIST_LESSONS_SCHEMA = {
  type: "object",
  properties: {
    type: {
      type: "string",
      enum: ["warning", "practice", "insight"],
      description: "Filter by lesson type (optional)."
    }
  },
  additionalProperties: false
};
var CREATE_TOPIC_SCHEMA = {
  type: "object",
  properties: {
    name: {
      type: "string",
      description: "Topic name."
    },
    description: {
      type: "string",
      description: "Optional description."
    }
  },
  required: ["name"],
  additionalProperties: false
};
var SEARCH_TOPICS_SCHEMA = {
  type: "object",
  properties: {
    query: {
      type: "string",
      description: "Search query for topics."
    }
  },
  required: ["query"],
  additionalProperties: false
};
var ADD_REFERENCE_SCHEMA = {
  type: "object",
  properties: {
    filepath: {
      type: "string",
      description: "Absolute path to the document to ingest."
    },
    title: {
      type: "string",
      description: "Optional title for the document."
    }
  },
  required: ["filepath"],
  additionalProperties: false
};
var SEARCH_REFERENCES_SCHEMA = {
  type: "object",
  properties: {
    query: {
      type: "string",
      description: "Search string for reference content."
    },
    limit: {
      type: "number",
      description: "Max results (default: 5).",
      default: 5
    }
  },
  required: ["query"],
  additionalProperties: false
};
var LIST_REFERENCES_SCHEMA = {
  type: "object",
  properties: {},
  additionalProperties: false
};
var READ_DIRECTIVES_SCHEMA = {
  type: "object",
  properties: {},
  additionalProperties: false
};
async function runMpm(args, timeoutMs = 15e3) {
  const { spawn } = await import("child_process");
  return new Promise((resolve) => {
    let stdout = "";
    let stderr = "";
    let settled = false;
    const proc = spawn(MPM_BINARY, args, {
      // Pass MPM_WORKSPACE so the binary knows where the DB is
      env: { ...process.env, MPM_WORKSPACE },
      stdio: ["ignore", "pipe", "pipe"]
    });
    const timer = setTimeout(() => {
      if (!settled) {
        settled = true;
        proc.kill("SIGKILL");
        resolve({
          stdout,
          stderr: `${stderr}
[timeout after ${timeoutMs}ms]`.trim(),
          exitCode: 124
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
          exitCode: 1
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
function parseMpmResult(result) {
  const { stdout, stderr, exitCode } = result;
  if (exitCode !== 0) {
    if (stderr.includes("database is locked") || stderr.includes("SQLITE_BUSY") || stderr.includes("database is locked")) {
      return {
        id: "",
        success: false,
        error: "database_locked",
        message: "MPM database is locked \u2014 this is safe to retry. The write was not made."
      };
    }
    return {
      id: "",
      success: false,
      error: `exit_${exitCode}`,
      message: stderr.split("\n")[0] || `MPM exited with code ${exitCode}`
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
function makeQueryLongTermMemoryTool(_ctx) {
  return {
    name: "query_long_term_memory",
    description: "Search MPM long-term memory. Before answering anything about prior work, decisions, dates, people, preferences, or todos \u2014 run this first. Returns matching memories as formatted text.",
    parameters: QUERY_LONG_TERM_MEMORY_SCHEMA,
    emoji_name: "brain",
    execute: async (toolCallId, params) => {
      const { query = "", limit = 5 } = params;
      const result = await runMpm([
        "recall",
        "--json",
        "--",
        query,
        String(limit)
      ]);
      const data = parseMpmResult(result);
      let displayText;
      if (data.memories && Array.isArray(data.memories) && data.memories.length > 0) {
        displayText = data.memories.map((m) => typeof m.content === "string" ? m.content : JSON.stringify(m)).join("\n\n---\n\n");
      } else if (data.text) {
        displayText = String(data.text);
      } else {
        displayText = "(no matching memories found)";
      }
      return {
        toolCallId,
        result: {
          type: "ok",
          results: [{ content: [{ type: "text", text: displayText }] }]
        }
      };
    }
  };
}
function makeSaveToMemoryTool(_ctx) {
  return {
    name: "save_to_memory",
    description: "Persist a fact, lesson, or decision to MPM long-term memory. After any non-trivial action, lesson learned, or decision \u2014 call this. Tags help later retrieval. Weight 0.5 by default; higher for important truths. Use TTL '24h' for session-scoped facts (e.g. current task context), use TTL '0' for permanent memories (e.g. user preferences, project state).",
    parameters: SAVE_TO_MEMORY_SCHEMA,
    emoji_name: "floppy_disk",
    execute: async (toolCallId, params) => {
      const {
        fact = "",
        tags = [],
        weight = 0.5,
        ttl
      } = params;
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
                    text: '{"success":false,"error":"empty_fact","message":"fact cannot be empty"}'
                  }
                ]
              }
            ]
          }
        };
      }
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
          type: "ok",
          results: [
            {
              content: [{ type: "text", text: JSON.stringify(data) }]
            }
          ]
        }
      };
    }
  };
}
function makeSaveLessonTool(_ctx) {
  return {
    name: "save_lesson",
    description: "Persist a lesson to MPM \u2014 what was learned, observed, or should be remembered. Three types: 'warning' (don't do X), 'practice' (do Y), 'insight' (X leads to Y). Lessons accumulate as the agent's learned experience.",
    parameters: SAVE_LESSON_SCHEMA,
    emoji_name: "bookmark",
    execute: async (toolCallId, params) => {
      const {
        fact = "",
        type = "insight",
        tags = []
      } = params;
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
                    text: '{"success":false,"error":"empty_fact","message":"fact cannot be empty"}'
                  }
                ]
              }
            ]
          }
        };
      }
      const args = [
        "lesson",
        "add",
        fact,
        "--type",
        type,
        "--tags",
        tags.join(","),
        "--json"
      ];
      const result = await runMpm(args);
      const data = parseMpmResult(result);
      return {
        toolCallId,
        result: {
          type: "ok",
          results: [
            {
              content: [{ type: "text", text: JSON.stringify(data) }]
            }
          ]
        }
      };
    }
  };
}
function makeSearchLessonsTool(_ctx) {
  return {
    name: "search_lessons",
    description: "Search MPM lessons for relevant learned knowledge. Use this to recall warnings, best practices, and insights before acting.",
    parameters: SEARCH_LESSONS_SCHEMA,
    emoji_name: "magnifying_glass",
    execute: async (toolCallId, params) => {
      const { query = "" } = params;
      const result = await runMpm(["lesson", "search", query, "--json"]);
      const data = parseMpmResult(result);
      let displayText;
      if (data.results && Array.isArray(data.results) && data.results.length > 0) {
        displayText = data.results.map((l) => {
          const type = l.type || "insight";
          const content = typeof l.content === "string" ? l.content : JSON.stringify(l.content);
          return `[${type}] ${content}`;
        }).join("\n\n---\n\n");
      } else if (data.message) {
        displayText = String(data.message);
      } else {
        displayText = "(no matching lessons found)";
      }
      return {
        toolCallId,
        result: {
          type: "ok",
          results: [{ content: [{ type: "text", text: displayText }] }]
        }
      };
    }
  };
}
function makeListLessonsTool(_ctx) {
  return {
    name: "list_lessons",
    description: "List all lessons in MPM, optionally filtered by type. Types: 'warning' (don't do X), 'practice' (do Y), 'insight' (X leads to Y).",
    parameters: LIST_LESSONS_SCHEMA,
    emoji_name: "books",
    execute: async (toolCallId, params) => {
      const { type } = params;
      const typeArg = type ? `--type=${type}` : "";
      const args = ["lesson", "list", typeArg, "--json"].filter(Boolean);
      const result = await runMpm(args);
      const data = parseMpmResult(result);
      let displayText;
      if (data.lessons && Array.isArray(data.lessons) && data.lessons.length > 0) {
        displayText = data.lessons.map((l) => {
          const lessonType = l.type || "insight";
          const content = typeof l.content === "string" ? l.content : JSON.stringify(l.content);
          const created = l.created_at ? ` (${String(l.created_at).slice(0, 10)})` : "";
          return `[${lessonType}]${created} ${content}`;
        }).join("\n");
      } else if (data.message) {
        displayText = String(data.message);
      } else {
        displayText = "(no lessons stored)";
      }
      return {
        toolCallId,
        result: {
          type: "ok",
          results: [{ content: [{ type: "text", text: displayText }] }]
        }
      };
    }
  };
}
function makeCreateTopicTool(_ctx) {
  return {
    name: "create_topic",
    description: "Create a topic in MPM to organize related memories and knowledge. Topics can be searched and serve as memory clusters.",
    parameters: CREATE_TOPIC_SCHEMA,
    emoji_name: "label",
    execute: async (toolCallId, params) => {
      const {
        name = "",
        description = ""
      } = params;
      if (!name.trim()) {
        return {
          toolCallId,
          result: {
            type: "ok",
            results: [
              {
                content: [
                  {
                    type: "text",
                    text: '{"success":false,"error":"empty_name","message":"name cannot be empty"}'
                  }
                ]
              }
            ]
          }
        };
      }
      const result = await runMpm([
        "topic",
        "add",
        name,
        description,
        "--json"
      ]);
      const data = parseMpmResult(result);
      return {
        toolCallId,
        result: {
          type: "ok",
          results: [
            {
              content: [{ type: "text", text: JSON.stringify(data) }]
            }
          ]
        }
      };
    }
  };
}
function makeSearchTopicsTool(_ctx) {
  return {
    name: "search_topics",
    description: "Search MPM topics for relevant knowledge clusters. Topics group related memories and provide context for the agent.",
    parameters: SEARCH_TOPICS_SCHEMA,
    emoji_name: "magnifying_glass",
    execute: async (toolCallId, params) => {
      const { query = "" } = params;
      const result = await runMpm(["topic", "search", query, "--json"]);
      const data = parseMpmResult(result);
      let displayText;
      if (data.results && Array.isArray(data.results) && data.results.length > 0) {
        displayText = data.results.map((t) => {
          const name = t.name || t.description || "(unnamed)";
          const id = t.id ? ` [${t.id}]` : "";
          const desc = t.description ? `: ${t.description}` : "";
          return `${name}${id}${desc}`;
        }).join("\n");
      } else if (data.message) {
        displayText = String(data.message);
      } else {
        displayText = "(no matching topics found)";
      }
      return {
        toolCallId,
        result: {
          type: "ok",
          results: [{ content: [{ type: "text", text: displayText }] }]
        }
      };
    }
  };
}
function makeAddReferenceTool(_ctx) {
  return {
    name: "add_reference",
    description: "Ingest a document as a reference into MPM. Supported formats: .txt, .md, .html, .epub, .pdf. The document is chunked and indexed for semantic search.",
    parameters: ADD_REFERENCE_SCHEMA,
    emoji_name: "paperclip",
    execute: async (toolCallId, params) => {
      const {
        filepath = "",
        title
      } = params;
      if (!filepath.trim()) {
        return {
          toolCallId,
          result: {
            type: "ok",
            results: [
              {
                content: [
                  {
                    type: "text",
                    text: '{"success":false,"error":"empty_filepath","message":"filepath cannot be empty"}'
                  }
                ]
              }
            ]
          }
        };
      }
      const titleArg = title ? ["--title", title] : [];
      const args = ["reference", "add", filepath, ...titleArg, "--json"].filter(Boolean);
      const result = await runMpm(args);
      const data = parseMpmResult(result);
      return {
        toolCallId,
        result: {
          type: "ok",
          results: [
            {
              content: [{ type: "text", text: JSON.stringify(data) }]
            }
          ]
        }
      };
    }
  };
}
function makeSearchReferencesTool(_ctx) {
  return {
    name: "search_references",
    description: "Search content within ingested reference documents. Returns matching chunks from the reference library.",
    parameters: SEARCH_REFERENCES_SCHEMA,
    emoji_name: "books",
    execute: async (toolCallId, params) => {
      const {
        query = "",
        limit = 5
      } = params;
      const result = await runMpm([
        "reference",
        "search",
        query,
        String(limit),
        "--json"
      ]);
      const data = parseMpmResult(result);
      let displayText;
      if (data.results && Array.isArray(data.results) && data.results.length > 0) {
        displayText = data.results.map((r) => {
          const docTitle = r.doc_title || "document";
          const chunkIdx = r.chunk_index ?? 0;
          const content = typeof r.content === "string" ? r.content : JSON.stringify(r.content);
          return `[${docTitle} chunk ${chunkIdx}]
${content}`;
        }).join("\n\n---\n\n");
      } else if (data.message) {
        displayText = String(data.message);
      } else {
        displayText = "(no matching reference content found)";
      }
      return {
        toolCallId,
        result: {
          type: "ok",
          results: [{ content: [{ type: "text", text: displayText }] }]
        }
      };
    }
  };
}
function makeListReferencesTool(_ctx) {
  return {
    name: "list_references",
    description: "List all ingested reference documents in MPM. Shows document titles, chunk counts, and tags.",
    parameters: LIST_REFERENCES_SCHEMA,
    emoji_name: "books",
    execute: async (toolCallId, _params) => {
      const result = await runMpm(["reference", "ls", "--json"]);
      const data = parseMpmResult(result);
      let displayText;
      if (data.references && Array.isArray(data.references) && data.references.length > 0) {
        displayText = data.references.map((r) => {
          const title = r.title || "untitled";
          const chunks = r.total_chunks ?? 0;
          const tags = r.tags ? ` [${r.tags}]` : "";
          const created = r.created_at ? ` \u2014 ${String(r.created_at).slice(0, 10)}` : "";
          return `${title} (${chunks} chunks)${tags}${created}`;
        }).join("\n");
      } else if (data.message) {
        displayText = String(data.message);
      } else {
        displayText = "(no references stored)";
      }
      return {
        toolCallId,
        result: {
          type: "ok",
          results: [{ content: [{ type: "text", text: displayText }] }]
        }
      };
    }
  };
}
function makeReadDirectivesTool(_ctx) {
  return {
    name: "read_directives",
    description: "Read the agent's prime directives \u2014 behavioral rules and operating principles. These define what the agent must and must not do.",
    parameters: READ_DIRECTIVES_SCHEMA,
    emoji_name: "scroll",
    execute: async (toolCallId, _params) => {
      const result = await runMpm(["directives", "--json"]);
      const data = parseMpmResult(result);
      let displayText;
      if (data.directives && Array.isArray(data.directives) && data.directives.length > 0) {
        displayText = data.directives.map((d) => {
          const collection = d.collection || "directive";
          const content = typeof d.content === "string" ? d.content : JSON.stringify(d.content);
          return `[${collection}]
${content}`;
        }).join("\n\n---\n\n");
      } else if (data.message) {
        displayText = String(data.message);
      } else {
        displayText = "(no directives defined \u2014 run the session that defines them)";
      }
      return {
        toolCallId,
        result: {
          type: "ok",
          results: [{ content: [{ type: "text", text: displayText }] }]
        }
      };
    }
  };
}
var index_default = definePluginEntry({
  id: "mpm",
  name: "MPM (Memory Persistence Module)",
  description: "Long-term memory tool for 808 \u2014 query and persist facts via the agent's own SQLite-backed persistence layer (MPM). Use query_long_term_memory before answering questions about prior work. Use save_to_memory after any non-trivial action or decision.",
  register(api) {
    api.registerTool(
      (ctx) => makeQueryLongTermMemoryTool(ctx),
      { names: ["query_long_term_memory"], optional: false }
    );
    api.registerTool(
      (ctx) => makeSaveToMemoryTool(ctx),
      { names: ["save_to_memory"], optional: false }
    );
    api.registerTool(
      (ctx) => makeSaveLessonTool(ctx),
      { names: ["save_lesson"], optional: false }
    );
    api.registerTool(
      (ctx) => makeSearchLessonsTool(ctx),
      { names: ["search_lessons"], optional: false }
    );
    api.registerTool(
      (ctx) => makeListLessonsTool(ctx),
      { names: ["list_lessons"], optional: false }
    );
    api.registerTool(
      (ctx) => makeCreateTopicTool(ctx),
      { names: ["create_topic"], optional: false }
    );
    api.registerTool(
      (ctx) => makeSearchTopicsTool(ctx),
      { names: ["search_topics"], optional: false }
    );
    api.registerTool(
      (ctx) => makeAddReferenceTool(ctx),
      { names: ["add_reference"], optional: false }
    );
    api.registerTool(
      (ctx) => makeSearchReferencesTool(ctx),
      { names: ["search_references"], optional: false }
    );
    api.registerTool(
      (ctx) => makeListReferencesTool(ctx),
      { names: ["list_references"], optional: false }
    );
    api.registerTool(
      (ctx) => makeReadDirectivesTool(ctx),
      { names: ["read_directives"], optional: false }
    );
  }
});
export {
  index_default as default
};
