import { definePluginEntry } from "openclaw/plugin-sdk/plugin-entry.js";
const MPM_BINARY = process.env.MPM_BINARY ?? "/home/v/workspace/projects/mpm/bin/mpm";
const MPM_WORKSPACE = process.env.MPM_WORKSPACE ?? "/home/v/workspace/projects/mpm";
const MAX_BUFFER_SIZE = 10 * 1024 * 1024;
const QUERY_LONG_TERM_MEMORY_SCHEMA = {
    type: "object",
    properties: {
        query: {
            type: "string",
            description: "Natural language search query for long-term memory.",
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
            description: "The fact, lesson, or decision to persist to long-term memory.",
        },
        tags: {
            type: "array",
            items: { type: "string" },
            description: "Optional tags for categorization and later retrieval.",
            default: [],
        },
        weight: {
            type: "number",
            description: "Importance weight between 0 and 1 (default: 0.5).",
            default: 0.5,
        },
        ttl: {
            type: "string",
            description: "Optional TTL string: '24h' for session-scoped, '0' for permanent, or a Go duration like '72h'.",
        },
        collection: {
            type: "string",
            description: "Optional collection name (default: 'memories').",
            default: "memories",
        },
    },
    required: ["fact"],
    additionalProperties: false,
};
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
};
const SAVE_LESSON_SCHEMA = {
    type: "object",
    properties: {
        fact: {
            type: "string",
            description: "The lesson content — what was learned, observed, or should be remembered.",
        },
        type: {
            type: "string",
            enum: ["warning", "practice", "insight"],
            description: "Lesson type: 'warning' (don't do X), 'practice' (do Y), 'insight' (X leads to Y).",
            default: "insight",
        },
        tags: {
            type: "array",
            items: { type: "string" },
            description: "Optional tags for the lesson.",
            default: [],
        },
    },
    required: ["fact"],
    additionalProperties: false,
};
const SEARCH_LESSONS_SCHEMA = {
    type: "object",
    properties: {
        query: {
            type: "string",
            description: "Search query for lesson content.",
        },
    },
    required: ["query"],
    additionalProperties: false,
};
const LIST_LESSONS_SCHEMA = {
    type: "object",
    properties: {
        type: {
            type: "string",
            enum: ["warning", "practice", "insight"],
            description: "Filter lessons by type.",
        },
    },
    additionalProperties: false,
};
const CREATE_TOPIC_SCHEMA = {
    type: "object",
    properties: {
        name: {
            type: "string",
            description: "Topic name.",
        },
        description: {
            type: "string",
            description: "Optional topic description.",
        },
    },
    required: ["name"],
    additionalProperties: false,
};
const SEARCH_TOPICS_SCHEMA = {
    type: "object",
    properties: {
        query: {
            type: "string",
            description: "Search query for topics.",
        },
        limit: {
            type: "number",
            description: "Maximum number of results (default: 20).",
            default: 20,
        },
    },
    additionalProperties: false,
};
const LINK_TOPIC_SCHEMA = {
    type: "object",
    properties: {
        memory_id: {
            type: "string",
            description: "The memory ID to link.",
        },
        topic_id: {
            type: "string",
            description: "The topic ID to link to.",
        },
    },
    required: ["memory_id", "topic_id"],
    additionalProperties: false,
};
const ADD_REFERENCE_SCHEMA = {
    type: "object",
    properties: {
        filepath: {
            type: "string",
            description: "Path to the file to ingest (.txt, .md, .html, .epub, .pdf).",
        },
        title: {
            type: "string",
            description: "Optional title override.",
        },
    },
    required: ["filepath"],
    additionalProperties: false,
};
const SEARCH_REFERENCES_SCHEMA = {
    type: "object",
    properties: {
        query: {
            type: "string",
            description: "Search query for reference content.",
        },
        limit: {
            type: "number",
            description: "Maximum number of results (default: 5).",
            default: 5,
        },
    },
    required: ["query"],
    additionalProperties: false,
};
const LIST_REFERENCES_SCHEMA = {
    type: "object",
    properties: {
        limit: {
            type: "number",
            description: "Maximum number of results (default: 50).",
            default: 50,
        },
        offset: {
            type: "number",
            description: "Pagination offset (default: 0).",
            default: 0,
        },
    },
    additionalProperties: false,
};
const READ_WAKE_CONTEXT_SCHEMA = {
    type: "object",
    properties: {},
    additionalProperties: false,
};
const READ_DIRECTIVES_SCHEMA = {
    type: "object",
    properties: {},
    additionalProperties: false,
};
const RECORD_DECISION_SCHEMA = {
    type: "object",
    properties: {
        context: {
            type: "string",
            description: "The situation or problem that required a decision.",
        },
        choice: {
            type: "string",
            description: "What was decided.",
        },
        rationale: {
            type: "string",
            description: "Why this path was chosen over alternatives.",
        },
        outcome: {
            type: "string",
            description: "Optional: what actually happened when this decision was executed.",
        },
        tags: {
            type: "array",
            items: { type: "string" },
            description: "Optional tags.",
            default: [],
        },
        weight: {
            type: "number",
            description: "Decision weight (default: 0.5).",
            default: 0.5,
        },
    },
    required: ["context", "choice", "rationale"],
    additionalProperties: false,
};
const PROPOSE_THEORY_SCHEMA = {
    type: "object",
    properties: {
        hypothesis: {
            type: "string",
            description: "The hypothesis or assumption.",
        },
        validationCriteria: {
            type: "string",
            description: "A specific, executable test or observation that would prove or disprove the hypothesis.",
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
};
const RESOLVE_THEORY_SCHEMA = {
    type: "object",
    properties: {
        theoryId: {
            type: "string",
            description: "The theory ID to resolve.",
        },
        conclusion: {
            type: "string",
            description: "What was concluded after running the validation criteria.",
        },
        newStatus: {
            type: "string",
            enum: ["proven", "disproven"],
            description: "Whether the hypothesis was proven or disproven.",
        },
    },
    required: ["theoryId", "conclusion", "newStatus"],
    additionalProperties: false,
};
const ADD_EVIDENCE_SCHEMA = {
    type: "object",
    properties: {
        artifact_id: {
            type: "string",
            description: "The ID of the target artifact (memory, theory, decision, or lesson).",
        },
        artifact_type: {
            type: "string",
            enum: ["memory", "theory", "decision", "lesson"],
            description: "The category of the target artifact.",
        },
        type: {
            type: "string",
            enum: ["observation", "test", "reproduction", "challenge", "decision_outcome", "external_reference"],
            description: "The specific classification of the evidence. Critical: Use 'challenge' for any evidence that contradicts the artifact.",
        },
        source_group: {
            type: "string",
            description: "A short descriptor of where this evidence originated (e.g., 'unit_test_suite', 'terminal_stderr', 'user_prompt', 'linter_output').",
        },
        strength: {
            type: "number",
            description: "Optional. A value between -1.0 and 1.0. Omit this to use the engine's calibrated default strength for the evidence type.",
        },
        created_by: {
            type: "string",
            description: "The identity of the agent logging the evidence (e.g., 'openclaw-agent').",
        },
        notes: {
            type: "string",
            description: "Optional. A brief, human-readable summary of what the evidence actually is (e.g., 'Test coverage dropped by 4% after implementing the new caching layer').",
        },
    },
    required: ["artifact_id", "artifact_type", "type", "source_group", "created_by"],
    additionalProperties: false,
};
const LIST_EVIDENCE_SCHEMA = {
    type: "object",
    properties: {
        artifact_id: {
            type: "string",
            description: "The ID of the target artifact.",
        },
        artifact_type: {
            type: "string",
            enum: ["memory", "theory", "decision", "lesson"],
            description: "The category of the target artifact.",
        },
    },
    required: ["artifact_id", "artifact_type"],
    additionalProperties: false,
};
const QUERY_CONFIDENCE_HISTORY_SCHEMA = {
    type: "object",
    properties: {
        artifact_id: {
            type: "string",
            description: "The ID of the target artifact.",
        },
        artifact_type: {
            type: "string",
            enum: ["memory", "theory", "decision", "lesson"],
            description: "The category of the target artifact.",
        },
        limit: {
            type: "number",
            description: "Optional. The maximum number of history records to return. Defaults to 10.",
            default: 10,
        },
    },
    required: ["artifact_id", "artifact_type"],
    additionalProperties: false,
};
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
        proc.stderr?.on("data", (chunk) => {
            if (stderr.length + chunk.length > MAX_BUFFER_SIZE) {
                stderr = (stderr + chunk.toString()).slice(-MAX_BUFFER_SIZE);
            }
            else {
                stderr += chunk.toString();
            }
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
async function callMpmCall(toolName, payload, timeoutMs = 15000) {
    const result = await runMpm([
        "call",
        toolName,
        "--payload",
        JSON.stringify(payload),
    ], timeoutMs);
    return parseMpmResult(result);
}
function parseMpmResult(result) {
    const { stdout, stderr, exitCode } = result;
    if (exitCode !== 0) {
        if (exitCode === 125 && stderr.includes("[output exceeded")) {
            return {
                id: "",
                success: false,
                error: "wake_context_truncated",
                message: "Wake context exceeds buffer limit — session is too large for the wake context tool. " +
                    "Proceed with minimal context (no previous session state).",
            };
        }
        if (stderr.includes("database is locked") ||
            stderr.includes("SQLITE_BUSY")) {
            return {
                id: "",
                success: false,
                error: "database_locked",
                message: "MPM database is locked — this is safe to retry. The write was not made.",
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
        return JSON.parse(trimmed);
    }
    catch {
        return { id: "", success: true, text: trimmed };
    }
}
function formatAge(createdAt) {
    if (!createdAt)
        return "unknown age";
    let created;
    try {
        created = new Date(createdAt.replace(" ", "T"));
        if (isNaN(created.getTime()))
            created = new Date(createdAt);
    }
    catch {
        return "unknown age";
    }
    if (isNaN(created.getTime()))
        return "unknown age";
    const diffMs = Date.now() - created.getTime();
    const diffSec = Math.floor(diffMs / 1000);
    if (diffSec < 60)
        return `${diffSec}s ago`;
    const diffMin = Math.floor(diffSec / 60);
    if (diffMin < 60)
        return `${diffMin}m ago`;
    const diffHr = Math.floor(diffMin / 60);
    if (diffHr < 24)
        return `${diffHr}h ago`;
    const diffDays = Math.floor(diffHr / 24);
    if (diffDays < 30)
        return `${diffDays}d ago`;
    const diffWeeks = Math.floor(diffDays / 7);
    if (diffWeeks < 12)
        return `${diffWeeks}w ago`;
    const diffMonths = Math.floor(diffDays / 30);
    return `${diffMonths}mo ago`;
}
function makeQueryLongTermMemoryTool(_ctx) {
    return {
        name: "query_long_term_memory",
        description: "Search MPM long-term memory. " +
            "Before answering anything about prior work, decisions, dates, people, " +
            "preferences, or todos — run this first. " +
            "Returns matching memories as formatted text.",
        parameters: QUERY_LONG_TERM_MEMORY_SCHEMA,
        emoji_name: "brain",
        execute: async (toolCallId, params) => {
            const { query = "", limit = 5 } = params;
            const data = await callMpmCall("query_long_term_memory", { query, limit });
            let displayText;
            if (data.memories && Array.isArray(data.memories) && data.memories.length > 0) {
                displayText = data.memories.map((m) => {
                    const mem = m;
                    const xref = mem.cross_references || {};
                    const topics = xref.topics || [];
                    const refDoc = xref.reference_doc;
                    const topicLine = topics.length
                        ? `\nTopics: [${topics.map((t) => t.name).join("] [")}]`
                        : "";
                    const refLine = refDoc ? `\nRef: ${refDoc.title}` : "";
                    const content = typeof mem.content === "string" ? mem.content : JSON.stringify(mem);
                    return `${content}${topicLine}${refLine}`;
                }).join("\n\n---\n\n");
            }
            else if (data.text) {
                displayText = String(data.text);
            }
            else {
                displayText = "(no matching memories found)";
            }
            return {
                toolCallId,
                result: {
                    type: "ok",
                    results: [{ content: [{ type: "text", text: displayText }] }],
                },
            };
        },
    };
}
function makeSaveToMemoryTool(_ctx) {
    return {
        name: "save_to_memory",
        description: "Persist a fact, lesson, or decision to MPM long-term memory. " +
            "After any non-trivial action, lesson learned, or decision — call this. " +
            "Tags help later retrieval. Weight 0.5 by default; higher for important truths. " +
            "Use TTL '24h' for session-scoped facts (e.g. current task context), " +
            "use TTL '0' for permanent memories (e.g. user preferences, project state).",
        parameters: SAVE_TO_MEMORY_SCHEMA,
        emoji_name: "floppy_disk",
        execute: async (toolCallId, params) => {
            const { fact = "", tags = [], weight = 0.5, ttl, collection, } = params;
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
            const payload = { fact, tags, weight };
            if (ttl)
                payload.ttl = ttl;
            if (collection)
                payload.collection = collection;
            const data = await callMpmCall("save_to_memory", payload);
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
function makeSaveLessonTool(_ctx) {
    return {
        name: "save_lesson",
        description: "Persist a lesson to MPM — what was learned, observed, or should be remembered. " +
            "Three types: 'warning' (don't do X), 'practice' (do Y), 'insight' (X leads to Y). " +
            "Lessons accumulate as the agent's learned experience.",
        parameters: SAVE_LESSON_SCHEMA,
        emoji_name: "bookmark",
        execute: async (toolCallId, params) => {
            const { fact = "", type = "insight", tags = [], } = params;
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
            const data = await callMpmCall("save_lesson", { fact, type, tags });
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
function makeSearchLessonsTool(_ctx) {
    return {
        name: "search_lessons",
        description: "Search MPM lessons for relevant learned knowledge. " +
            "Use this to recall warnings, best practices, and insights before acting.",
        parameters: SEARCH_LESSONS_SCHEMA,
        emoji_name: "magnifying_glass",
        execute: async (toolCallId, params) => {
            const { query = "" } = params;
            const data = await callMpmCall("search_lessons", { query, limit: 10 });
            let displayText;
            if (data.results && Array.isArray(data.results) && data.results.length > 0) {
                displayText = data.results
                    .map((l) => {
                    const lessonType = l.type || "insight";
                    const content = typeof l.content === "string" ? l.content : JSON.stringify(l.content);
                    return `[${lessonType}] ${content}`;
                })
                    .join("\n\n---\n\n");
            }
            else if (data.message) {
                displayText = String(data.message);
            }
            else {
                displayText = "(no matching lessons found)";
            }
            return {
                toolCallId,
                result: {
                    type: "ok",
                    results: [{ content: [{ type: "text", text: displayText }] }],
                },
            };
        },
    };
}
function makeListLessonsTool(_ctx) {
    return {
        name: "list_lessons",
        description: "List all lessons in MPM, optionally filtered by type. " +
            "Types: 'warning' (don't do X), 'practice' (do Y), 'insight' (X leads to Y).",
        parameters: LIST_LESSONS_SCHEMA,
        emoji_name: "books",
        execute: async (toolCallId, params) => {
            const { type } = params;
            const payload = {};
            if (type)
                payload.type = type;
            const data = await callMpmCall("list_lessons", payload);
            let displayText;
            if (data.lessons && Array.isArray(data.lessons) && data.lessons.length > 0) {
                displayText = data.lessons
                    .map((l) => {
                    const lessonType = l.type || "insight";
                    const content = typeof l.content === "string" ? l.content : JSON.stringify(l.content);
                    const created = l.created_at ? ` (${String(l.created_at).slice(0, 10)})` : "";
                    return `[${lessonType}]${created} ${content}`;
                })
                    .join("\n");
            }
            else if (data.message) {
                displayText = String(data.message);
            }
            else {
                displayText = "(no lessons stored)";
            }
            return {
                toolCallId,
                result: {
                    type: "ok",
                    results: [{ content: [{ type: "text", text: displayText }] }],
                },
            };
        },
    };
}
function makeCreateTopicTool(_ctx) {
    return {
        name: "create_topic",
        description: "Create a topic in MPM to organize related memories and knowledge. " +
            "Topics can be searched and serve as memory clusters.",
        parameters: CREATE_TOPIC_SCHEMA,
        emoji_name: "label",
        execute: async (toolCallId, params) => {
            const { name = "", description = "", } = params;
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
                                        text: '{"success":false,"error":"empty_name","message":"name cannot be empty"}',
                                    },
                                ],
                            },
                        ],
                    },
                };
            }
            const data = await callMpmCall("create_topic", { name, description });
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
function makeSearchTopicsTool(_ctx) {
    return {
        name: "search_topics",
        description: "Search MPM topics for relevant knowledge clusters. " +
            "Topics group related memories and provide context for the agent.",
        parameters: SEARCH_TOPICS_SCHEMA,
        emoji_name: "magnifying_glass",
        execute: async (toolCallId, params) => {
            const { query = "", limit = 20 } = params;
            const data = await callMpmCall("search_topics", { query, limit });
            let displayText;
            if (data.results && Array.isArray(data.results) && data.results.length > 0) {
                displayText = data.results
                    .map((t) => {
                    const name = t.name || t.description || "(unnamed)";
                    const id = t.id ? ` [${t.id}]` : "";
                    const desc = t.description ? `: ${t.description}` : "";
                    const topMems = t.top_memories || [];
                    let memPreview = "";
                    if (topMems.length > 0) {
                        memPreview = "\n  Top: " + topMems
                            .slice(0, 3)
                            .map((m) => {
                            const content = typeof m.content === "string" ? m.content : "";
                            return content.length > 60 ? content.slice(0, 60) + "…" : content;
                        })
                            .join(" | ");
                        const total = t.memory_count || 0;
                        if (total > 3) {
                            memPreview += ` [+${total - 3} more]`;
                        }
                    }
                    return `${name}${id}${desc}${memPreview}`;
                })
                    .join("\n");
            }
            else if (data.message) {
                displayText = String(data.message);
            }
            else {
                displayText = "(no matching topics found)";
            }
            return {
                toolCallId,
                result: {
                    type: "ok",
                    results: [{ content: [{ type: "text", text: displayText }] }],
                },
            };
        },
    };
}
function makeLinkTopicTool(_ctx) {
    return {
        name: "link_topic",
        description: "Link an existing memory to an existing topic. " +
            "Use this after saving a memory and seeing topic suggestions. " +
            "The memory and topic must both already exist.",
        parameters: LINK_TOPIC_SCHEMA,
        emoji_name: "link",
        execute: async (toolCallId, params) => {
            const { memory_id = "", topic_id = "", } = params;
            if (!memory_id.trim() || !topic_id.trim()) {
                return {
                    toolCallId,
                    result: {
                        type: "ok",
                        results: [
                            {
                                content: [
                                    {
                                        type: "text",
                                        text: '{"success":false,"error":"missing_params","message":"memory_id and topic_id are required"}',
                                    },
                                ],
                            },
                        ],
                    },
                };
            }
            const data = await callMpmCall("link_topic", { memory_id, topic_id });
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
function makeAddReferenceTool(_ctx) {
    return {
        name: "add_reference",
        description: "Ingest a document as a reference into MPM. " +
            "Supported formats: .txt, .md, .html, .epub, .pdf. " +
            "The document is chunked and indexed for semantic search.",
        parameters: ADD_REFERENCE_SCHEMA,
        emoji_name: "paperclip",
        execute: async (toolCallId, params) => {
            const { filepath = "", title, } = params;
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
                                        text: '{"success":false,"error":"empty_filepath","message":"filepath cannot be empty"}',
                                    },
                                ],
                            },
                        ],
                    },
                };
            }
            const payload = { filepath };
            if (title)
                payload.title = title;
            const data = await callMpmCall("add_reference", payload);
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
function makeSearchReferencesTool(_ctx) {
    return {
        name: "search_references",
        description: "Search content within ingested reference documents. " +
            "Returns matching chunks from the reference library.",
        parameters: SEARCH_REFERENCES_SCHEMA,
        emoji_name: "books",
        execute: async (toolCallId, params) => {
            const { query = "", limit = 5, } = params;
            const data = await callMpmCall("search_references", { query, limit });
            let displayText;
            if (data.results && Array.isArray(data.results) && data.results.length > 0) {
                displayText = data.results
                    .map((r) => {
                    const docTitle = r.doc_title || "document";
                    const chunkIdx = r.chunk_index ?? 0;
                    const content = typeof r.content === "string" ? r.content : JSON.stringify(r.content);
                    return `[${docTitle} chunk ${chunkIdx}]\n${content}`;
                })
                    .join("\n\n---\n\n");
            }
            else if (data.message) {
                displayText = String(data.message);
            }
            else {
                displayText = "(no matching reference content found)";
            }
            return {
                toolCallId,
                result: {
                    type: "ok",
                    results: [{ content: [{ type: "text", text: displayText }] }],
                },
            };
        },
    };
}
function makeListReferencesTool(_ctx) {
    return {
        name: "list_references",
        description: "List all ingested reference documents in MPM. " +
            "Shows document titles, chunk counts, and tags.",
        parameters: LIST_REFERENCES_SCHEMA,
        emoji_name: "books",
        execute: async (toolCallId, params) => {
            const { limit = 50, offset = 0 } = params;
            const data = await callMpmCall("list_references", { limit, offset });
            let displayText;
            if (data.references && Array.isArray(data.references) && data.references.length > 0) {
                displayText = data.references
                    .map((r) => {
                    const title = r.title || "untitled";
                    const chunks = r.total_chunks ?? 0;
                    const tags = r.tags ? ` [${r.tags}]` : "";
                    const created = r.created_at ? ` — ${String(r.created_at).slice(0, 10)}` : "";
                    return `${title} (${chunks} chunks)${tags}${created}`;
                })
                    .join("\n");
            }
            else if (data.message) {
                displayText = String(data.message);
            }
            else {
                displayText = "(no references stored)";
            }
            return {
                toolCallId,
                result: {
                    type: "ok",
                    results: [{ content: [{ type: "text", text: displayText }] }],
                },
            };
        },
    };
}
function makeReadWakeContextTool(_ctx) {
    return {
        name: "read_wake_context",
        description: "Read the agent's wake context — session state from the previous session including " +
            "active mode, active persona, recent topics, and recent memories. " +
            "This is the first thing to check on session start to understand where you left off. " +
            "Call this immediately on session start before doing anything else.",
        parameters: READ_WAKE_CONTEXT_SCHEMA,
        emoji_name: "sunrise",
        execute: async (toolCallId, _params) => {
            const data = await callMpmCall("read_wake_context", {});
            if (!data || !data.success) {
                return {
                    toolCallId,
                    result: {
                        type: "ok",
                        results: [
                            {
                                content: [
                                    {
                                        type: "text",
                                        text: "(no wake context available — no previous session found)",
                                    },
                                ],
                            },
                        ],
                    },
                };
            }
            const ctx = data;
            const lines = [
                "\xf0\x9f\x8c\x9e **WAKE CONTEXT** \xf0\x9f\x8c\x9e\n",
            ];
            if (ctx.mode || ctx.active_mode) {
                lines.push(`\n  **Mode:** ${ctx.mode || ctx.active_mode}`);
            }
            if (ctx.persona || ctx.active_persona) {
                lines.push(`\n  **Persona:** ${ctx.persona || ctx.active_persona}`);
            }
            if (Array.isArray(ctx.recent_topics) && ctx.recent_topics.length > 0) {
                lines.push(`\n  **Recent Topics:** ${ctx.recent_topics.join(", ")}`);
            }
            if (ctx.recent_memories && Array.isArray(ctx.recent_memories) && ctx.recent_memories.length > 0) {
                lines.push(`\n  **Recent Memories:**`);
                for (const mem of ctx.recent_memories) {
                    const content = (mem.content || "").substring(0, 80);
                    const age = mem.created_at ? ` (${formatAge(mem.created_at)})` : "";
                    lines.push(`\n    \u2022 ${content}${age}`);
                }
            }
            const displayText = lines.join("");
            return {
                toolCallId,
                result: {
                    type: "ok",
                    results: [{ content: [{ type: "text", text: displayText }] }],
                },
            };
        },
    };
}
function makeReadDirectivesTool(_ctx) {
    function groupByCollection(directives) {
        const groups = new Map();
        for (const d of directives) {
            const key = (d.collection || "directive").trim();
            if (!groups.has(key))
                groups.set(key, []);
            groups.get(key).push({
                content: typeof d.content === "string" ? d.content : JSON.stringify(d.content),
                created_at: d.created_at || "",
                id: d.id || "",
            });
        }
        return groups;
    }
    return {
        name: "read_directives",
        description: "Read the agent's prime directives — behavioral rules and operating principles. " +
            "These define what the agent must and must not do.",
        parameters: READ_DIRECTIVES_SCHEMA,
        emoji_name: "scroll",
        execute: async (toolCallId, _params) => {
            const data = await callMpmCall("read_directives", {});
            if (!data.directives || !Array.isArray(data.directives) || data.directives.length === 0) {
                return {
                    toolCallId,
                    result: {
                        type: "ok",
                        results: [
                            {
                                content: [
                                    {
                                        type: "text",
                                        text: data.message ? String(data.message) : "(no directives defined)",
                                    },
                                ],
                            },
                        ],
                    },
                };
            }
            const groups = groupByCollection(data.directives);
            const lines = [
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
                    type: "ok",
                    results: [{ content: [{ type: "text", text: displayText }] }],
                },
            };
        },
    };
}
function makeRecordDecisionTool(_ctx) {
    return {
        name: "record_decision",
        description: "Record an architectural decision, library choice, or any moment where you chose " +
            "path A over path B. Call this BEFORE or AFTER the decision — not just after. " +
            "The rationale is the most important field: it is what makes past decisions " +
            "reusable when you encounter a similar problem weeks later. " +
            "Trigger: whenever you weigh tradeoffs and choose one, or whenever you notice " +
            "you just picked an approach without recording why.",
        parameters: RECORD_DECISION_SCHEMA,
        emoji_name: "scales",
        execute: async (toolCallId, params) => {
            const { context = "", choice = "", rationale = "", outcome = "", tags = [], weight = 0.5, } = params;
            if (!context.trim() || !choice.trim() || !rationale.trim()) {
                return {
                    toolCallId,
                    result: {
                        type: "ok",
                        results: [
                            {
                                content: [
                                    {
                                        type: "text",
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
            const payload = { context, choice, rationale, tags, weight };
            if (outcome)
                payload.outcome = outcome;
            const data = await callMpmCall("record_decision", payload);
            return {
                toolCallId,
                result: {
                    type: "ok",
                    results: [{ content: [{ type: "text", text: JSON.stringify(data) }] }],
                },
            };
        },
    };
}
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
};
function makeProactiveRecallHintTool(_ctx) {
    return {
        name: "proactive_recall_hint",
        description: "Checks recent conversation context for overlap with decisions and theories. " +
            "Returns a structured Recall Hint if a semantic match is found. " +
            "Call this after each user message — one hint per turn max. " +
            "Surface only the top hint (rank 0) when hints are returned.",
        parameters: PROACTIVE_RECALL_HINT_SCHEMA,
        emoji_name: "bell",
        execute: async (toolCallId, params) => {
            const { conversation_text = "", max_hints = 3, min_score = -3.0, } = params;
            if (!conversation_text.trim()) {
                return {
                    toolCallId,
                    result: {
                        type: "ok",
                        results: [{ content: [{ type: "text", text: "[]" }] }],
                    },
                };
            }
            const data = await callMpmCall("proactive_recall_hint", { conversation_text, max_hints, min_score });
            const hints = (Array.isArray(data.hints) ? data.hints : []).map((m) => {
                const content = m.content || "";
                const meta = m.metadata || {};
                let status = meta.status || "";
                const conclusion = meta.conclusion || "";
                let choice = "";
                let rationale = "";
                let hypothesis = "";
                if (m.collection === "decisions") {
                    const firstLine = content.split("\n")[0] || "";
                    if (firstLine.toUpperCase().startsWith("CHOICE: ")) {
                        choice = firstLine.slice(7).trim();
                    }
                    const rLine = content.split("\n").find((l) => l.toUpperCase().startsWith("RATIONALE:"));
                    if (rLine)
                        rationale = rLine.slice(10).trim();
                }
                else if (m.collection === "theories") {
                    const firstLine = content.split("\n")[0] || "";
                    if (firstLine.toUpperCase().startsWith("HYPOTHESIS: ")) {
                        hypothesis = firstLine.slice(11).trim();
                    }
                }
                if (!status) {
                    const sLine = content.split("\n").find((l) => l.toUpperCase().startsWith("STATUS:"));
                    if (sLine)
                        status = sLine.split(":")[1]?.trim() || "";
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
                    type: "ok",
                    results: [{ content: [{ type: "text", text: JSON.stringify(hints) }] }],
                },
            };
        },
    };
}
function makeProposeTheoryTool(_ctx) {
    return {
        name: "propose_theory",
        description: "Log a hypothesis about causality before writing a fix. " +
            "When you think 'X is probably causing Y' — propose it, define the test, then run the test. " +
            "Writing the validation criteria forces you to confront whether the assumption is actually testable, " +
            "and often collapses a false hypothesis before it wastes an hour of debugging time. " +
            "Trigger: whenever you form a 'I think X is causing Y' assumption during debugging or design work.",
        parameters: PROPOSE_THEORY_SCHEMA,
        emoji_name: "bulb",
        execute: async (toolCallId, params) => {
            const { hypothesis = "", validationCriteria = "", tags = [], } = params;
            if (!hypothesis.trim() || !validationCriteria.trim()) {
                return {
                    toolCallId,
                    result: {
                        type: "ok",
                        results: [
                            {
                                content: [
                                    {
                                        type: "text",
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
            const payload = { hypothesis, validation_criteria: validationCriteria, tags };
            const data = await callMpmCall("propose_theory", payload);
            return {
                toolCallId,
                result: {
                    type: "ok",
                    results: [{ content: [{ type: "text", text: JSON.stringify(data) }] }],
                },
            };
        },
    };
}
function makeResolveTheoryTool(_ctx) {
    return {
        name: "resolve_theory",
        description: "Close the loop on a pending theory after running its validation criteria. " +
            "If the hypothesis was confirmed, save the confirmed result as a permanent memory " +
            "(weight=1.0, include the theory_id as a tag for traceability). " +
            "If it was disproven, record what actually caused the problem instead. " +
            "Trigger: immediately after executing the test described in a pending theory's validationCriteria.",
        parameters: RESOLVE_THEORY_SCHEMA,
        emoji_name: "white_check_mark",
        execute: async (toolCallId, params) => {
            const { theoryId = "", conclusion = "", newStatus = "", } = params;
            if (!theoryId.trim() || !conclusion.trim() || !newStatus.trim()) {
                return {
                    toolCallId,
                    result: {
                        type: "ok",
                        results: [
                            {
                                content: [
                                    {
                                        type: "text",
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
                        type: "ok",
                        results: [
                            {
                                content: [
                                    {
                                        type: "text",
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
            const data = await callMpmCall("resolve_theory", { theoryId, conclusion, newStatus });
            return {
                toolCallId,
                result: {
                    type: "ok",
                    results: [{ content: [{ type: "text", text: JSON.stringify(data) }] }],
                },
            };
        },
    };
}
function makeChallengeTool(_ctx) {
    return {
        name: "challenge_memory",
        description: "Challenge an existing memory by presenting contradictory evidence. " +
            "This weakens the memory, creates a pending theory, and logs a decision. " +
            "Use when you discover that a stored memory is no longer accurate. " +
            "Trigger: conversation or test results contradict a specific stored memory.",
        parameters: CHALLENGE_MEMORY_SCHEMA,
        emoji_name: "warning",
        execute: async (toolCallId, params) => {
            const { memoryId = "", evidence = "", } = params;
            if (!memoryId.trim() || !evidence.trim()) {
                return {
                    toolCallId,
                    result: {
                        type: "ok",
                        results: [
                            {
                                content: [
                                    {
                                        type: "text",
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
            const data = await callMpmCall("challenge_memory", { memoryId, evidence });
            return {
                toolCallId,
                result: {
                    type: "ok",
                    results: [{ content: [{ type: "text", text: JSON.stringify(data) }] }],
                },
            };
        },
    };
}
function makeAddEvidenceTool(_ctx) {
    return {
        name: "add_evidence",
        description: "Records new evidence supporting or challenging an existing memory, theory, decision, or lesson. " +
            "Use this when you observe system behavior, run unit tests, or receive user feedback that " +
            "validates or contradicts existing knowledge. The system will automatically recompute its " +
            "confidence in the artifact based on this evidence.",
        parameters: ADD_EVIDENCE_SCHEMA,
        emoji_name: "test_tube",
        execute: async (toolCallId, params) => {
            const { artifact_id = "", artifact_type = "memory", type = "", source_group = "", strength, created_by = "", notes = "", } = params;
            if (!artifact_id.trim() || !type.trim() || !source_group.trim() || !created_by.trim()) {
                return {
                    toolCallId,
                    result: {
                        type: "ok",
                        results: [
                            {
                                content: [
                                    {
                                        type: "text",
                                        text: JSON.stringify({
                                            success: false,
                                            error: "missing_required_field",
                                            message: "artifact_id, type, source_group, and created_by are all required.",
                                        }),
                                    },
                                ],
                            },
                        ],
                    },
                };
            }
            const payload = {
                artifact_id,
                artifact_type,
                type,
                source_group,
                created_by,
            };
            if (typeof strength === "number")
                payload.strength = strength;
            if (notes)
                payload.notes = notes;
            const data = await callMpmCall("add_evidence", payload);
            return {
                toolCallId,
                result: {
                    type: "ok",
                    results: [{ content: [{ type: "text", text: JSON.stringify(data) }] }],
                },
            };
        },
    };
}
function makeListEvidenceTool(_ctx) {
    return {
        name: "list_evidence",
        description: "Retrieves the complete chronological ledger of evidence supporting or challenging a specific " +
            "artifact. Use this to understand the specific observations and tests that form the foundation " +
            "of the system's current confidence level.",
        parameters: LIST_EVIDENCE_SCHEMA,
        emoji_name: "clipboard",
        execute: async (toolCallId, params) => {
            const { artifact_id = "", artifact_type = "memory", } = params;
            if (!artifact_id.trim()) {
                return {
                    toolCallId,
                    result: {
                        type: "ok",
                        results: [
                            {
                                content: [
                                    {
                                        type: "text",
                                        text: JSON.stringify({
                                            success: false,
                                            error: "missing_required_field",
                                            message: "artifact_id is required.",
                                        }),
                                    },
                                ],
                            },
                        ],
                    },
                };
            }
            const data = await callMpmCall("list_evidence", { artifact_id, artifact_type });
            return {
                toolCallId,
                result: {
                    type: "ok",
                    results: [{ content: [{ type: "text", text: JSON.stringify(data) }] }],
                },
            };
        },
    };
}
function makeQueryConfidenceHistoryTool(_ctx) {
    return {
        name: "query_confidence_history",
        description: "Retrieves the historical timeline of confidence score changes for a specific artifact. " +
            "Use this to see the trajectory of an artifact's reliability — whether it is trending up " +
            "(proven over time), trending down (recently challenged), or slowly decaying due to age.",
        parameters: QUERY_CONFIDENCE_HISTORY_SCHEMA,
        emoji_name: "chart_with_upwards_trend",
        execute: async (toolCallId, params) => {
            const { artifact_id = "", artifact_type = "memory", limit = 10, } = params;
            if (!artifact_id.trim()) {
                return {
                    toolCallId,
                    result: {
                        type: "ok",
                        results: [
                            {
                                content: [
                                    {
                                        type: "text",
                                        text: JSON.stringify({
                                            success: false,
                                            error: "missing_required_field",
                                            message: "artifact_id is required.",
                                        }),
                                    },
                                ],
                            },
                        ],
                    },
                };
            }
            const data = await callMpmCall("query_confidence_history", {
                artifact_id,
                artifact_type,
                limit,
            });
            return {
                toolCallId,
                result: {
                    type: "ok",
                    results: [{ content: [{ type: "text", text: JSON.stringify(data) }] }],
                },
            };
        },
    };
}
export default definePluginEntry({
    id: "mpm",
    name: "MPM (Memory Persistence Module)",
    description: "Long-term memory tool for 808 — query and persist facts via the agent's " +
        "own SQLite-backed persistence layer (MPM). " +
        "Use query_long_term_memory before answering questions about prior work. " +
        "Use save_to_memory after any non-trivial action or decision.",
    register(api) {
        api.registerTool((ctx) => makeQueryLongTermMemoryTool(ctx), { names: ["query_long_term_memory"], optional: false });
        api.registerTool((ctx) => makeSaveToMemoryTool(ctx), { names: ["save_to_memory"], optional: false });
        api.registerTool((ctx) => makeSaveLessonTool(ctx), { names: ["save_lesson"], optional: false });
        api.registerTool((ctx) => makeSearchLessonsTool(ctx), { names: ["search_lessons"], optional: false });
        api.registerTool((ctx) => makeListLessonsTool(ctx), { names: ["list_lessons"], optional: false });
        api.registerTool((ctx) => makeCreateTopicTool(ctx), { names: ["create_topic"], optional: false });
        api.registerTool((ctx) => makeSearchTopicsTool(ctx), { names: ["search_topics"], optional: false });
        api.registerTool((ctx) => makeLinkTopicTool(ctx), { names: ["link_topic"], optional: false });
        api.registerTool((ctx) => makeAddReferenceTool(ctx), { names: ["add_reference"], optional: false });
        api.registerTool((ctx) => makeSearchReferencesTool(ctx), { names: ["search_references"], optional: false });
        api.registerTool((ctx) => makeListReferencesTool(ctx), { names: ["list_references"], optional: false });
        api.registerTool((ctx) => makeReadWakeContextTool(ctx), { names: ["read_wake_context"], optional: false });
        api.registerTool((ctx) => makeReadDirectivesTool(ctx), { names: ["read_directives"], optional: false });
        api.registerTool((ctx) => makeRecordDecisionTool(ctx), { names: ["record_decision"], optional: false });
        api.registerTool((ctx) => makeProactiveRecallHintTool(ctx), { names: ["proactive_recall_hint"], optional: false });
        api.registerTool((ctx) => makeProposeTheoryTool(ctx), { names: ["propose_theory"], optional: false });
        api.registerTool((ctx) => makeResolveTheoryTool(ctx), { names: ["resolve_theory"], optional: false });
        api.registerTool((ctx) => makeChallengeTool(ctx), { names: ["challenge_memory"], optional: false });
        api.registerTool((ctx) => makeAddEvidenceTool(ctx), { names: ["add_evidence"], optional: false });
        api.registerTool((ctx) => makeListEvidenceTool(ctx), { names: ["list_evidence"], optional: false });
        api.registerTool((ctx) => makeQueryConfidenceHistoryTool(ctx), { names: ["query_confidence_history"], optional: false });
    },
});
