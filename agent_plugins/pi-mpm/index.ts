/**
 * pi-mpm — Pi extension that wraps MPM (mpm call <tool> --payload '<json>').
 *
 * Provides Pi with:
 *   - mpm_recall          : query_long_term_memory  (free-text recall)
 *   - mpm_remember        : save_to_memory          (persist durable info)
 *   - mpm_session_handoff : session_end             (cross-session persistence)
 *   - mpm_explain         : explain_retrieval       (transparency for recall)
 *   - session_start hook  : read_wake_context → cache prior handoff
 *   - before_agent_start  : inject cached handoff into the system prompt once
 *   - /mpm-status command : mpm info                (install identity)
 *
 * mpm is invoked at its canonical install location ($HOME/.mpm/bin/mpm).
 * Override with the MPM_BIN env var if the install is at a non-standard
 * path. If the canonical binary is missing at extension load time, the
 * extension REFUSES TO LOAD (fail-loud) — silent degradation of memory
 * access is a worse failure mode than a clear install error. Runtime
 * subprocess errors (timeout, transient I/O) still fail-soft at the tool
 * level so the LLM can retry or surface a graceful error.
 *
 * Why this exists: mpm's README explicitly designates `mpm call` (and the
 * `mpm-mcp` stdio server) as the agent-facing integration surface. Pi has no
 * built-in MCP support (per docs/usage.md §303), so the JSON-RPC interface
 * is the correct boundary. This file is a thin transport adapter — every
 * tool/hook here maps 1:1 onto an entry in mpm's internal/core/tools registry.
 */

import { spawn } from "node:child_process";
import { accessSync, constants as fsConstants } from "node:fs";
import { join } from "node:path";
import { StringEnum, Type } from "@earendil-works/pi-ai";
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";

// --------------------------------------------------------------------------
// Canonical-binary resolution (alpha install contract).
// Resolve once at module load. If this throws, the extension fails to
// load — the operator sees the install error in Pi's startup output.
// --------------------------------------------------------------------------

function resolveMpmBin(): string {
	const fromEnv = process.env.MPM_BIN?.trim();
	const candidates = [
		fromEnv,
		join(process.env.HOME ?? "", ".mpm", "bin", "mpm"),
	].filter((p): p is string => typeof p === "string" && p.length > 0);

	const tried: string[] = [];
	for (const p of candidates) {
		tried.push(p);
		try {
			accessSync(p, fsConstants.X_OK);
			return p;
		} catch {
			// try next
		}
	}
	throw new Error(
		`pi-mpm: cannot find a usable mpm binary. Tried: ${tried.join(", ")}. ` +
			`Install mpm at the canonical location: \`git clone <repo> ~/.mpm && ` +
			`cd ~/.mpm && make install\` (no sudo required). Or set ` +
			`MPM_BIN=/absolute/path/to/mpm to override the discovery path.`,
	);
}

const MPM_BIN: string = resolveMpmBin();

// --------------------------------------------------------------------------
// Subprocess adapter — `mpm call <tool> --payload '<json>'`
// --------------------------------------------------------------------------

interface MpmCallResult {
	success: boolean;
	payload: unknown;
	raw: string;
	stderr: string;
	timedOut: boolean;
	spawnError: string | null;
}

const DEFAULT_TIMEOUT_MS = 15_000;

/**
 * Spawn `mpm call <tool> --payload <json>` and return the parsed envelope.
 * mpm emits one zap-style log line to stderr (e.g. "time=... level=INFO msg=...")
 * and a single JSON object to stdout on success. We find the JSON object
 * on stdout by scanning lines for one that parses cleanly — same approach
 * as openclaw-mpm-memory's findJsonInOutput.
 */
function callMpm(
	tool: string,
	payload: Record<string, unknown>,
	opts: { mpmBin?: string; timeoutMs?: number } = {},
): Promise<MpmCallResult> {
	const bin = opts.mpmBin ?? MPM_BIN;
	const timeoutMs = opts.timeoutMs ?? DEFAULT_TIMEOUT_MS;
	const json = JSON.stringify(payload);

	return new Promise((resolve) => {
		let stdout = "";
		let stderr = "";
		let timedOut = false;
		let settled = false;
		const finish = (r: MpmCallResult) => {
			if (settled) return;
			settled = true;
			resolve(r);
		};

		let child: ReturnType<typeof spawn>;
		try {
			child = spawn(bin, ["call", tool, "--payload", json], {
				stdio: ["ignore", "pipe", "pipe"],
			});
		} catch (err) {
			finish({
				success: false,
				payload: null,
				raw: "",
				stderr: "",
				timedOut: false,
				spawnError: err instanceof Error ? err.message : String(err),
			});
			return;
		}

		child.on("error", (err) => {
			finish({
				success: false,
				payload: null,
				raw: stdout,
				stderr,
				timedOut: false,
				spawnError: err.message,
			});
		});

		child.stdout?.on("data", (chunk: Buffer) => (stdout += chunk.toString()));
		child.stderr?.on("data", (chunk: Buffer) => (stderr += chunk.toString()));

		const timer = setTimeout(() => {
			timedOut = true;
			try {
				child.kill("SIGKILL");
			} catch {
				/* ignore */
			}
		}, timeoutMs);
		timer.unref();

		child.on("close", () => {
			clearTimeout(timer);
			const parsed = parseLastJsonLine(stdout);
			const success =
				!timedOut && parsed !== null && (parsed as { success?: boolean }).success !== false;
			finish({
				success,
				payload: parsed,
				raw: stdout.trim(),
				stderr: stderr.trim(),
				timedOut,
				spawnError: null,
			});
		});
	});
}

/**
 * Find the last line in `s` that parses as a JSON object. mpm prints
 * zap logs to stderr and one JSON envelope to stdout; we keep this
 * resilient if mpm later adds pre-JSON status output.
 */
function parseLastJsonLine(s: string): unknown {
	if (!s) return null;
	const lines = s.split("\n");
	for (let i = lines.length - 1; i >= 0; i--) {
		const t = lines[i].trim();
		if (t.startsWith("{") && t.endsWith("}")) {
			try {
				return JSON.parse(t);
			} catch {
				// keep scanning
			}
		}
	}
	return null;
}

function formatFailure(tool: string, r: MpmCallResult): string {
	if (r.spawnError) {
		return `mpm not reachable: ${r.spawnError}. Is the mpm binary on PATH?`;
	}
	if (r.timedOut) {
		return `mpm call ${tool} timed out after ${DEFAULT_TIMEOUT_MS}ms.`;
	}
	if (r.payload === null) {
		const tail = (r.stderr || r.raw).slice(-300);
		return `mpm call ${tool} returned no parseable JSON. tail: ${tail}`;
	}
	const payload = r.payload as { error?: string };
	if (payload?.error) {
		return `mpm call ${tool} error: ${payload.error}`;
	}
	return `mpm call ${tool} failed.`;
}

/** Render a JSON payload as compact, LLM-friendly text. */
function jsonToText(payload: unknown): string {
	if (payload === null || payload === undefined) return "(no payload)";
	const obj = payload as Record<string, unknown>;
	const text = JSON.stringify(obj, null, 2);
	// cap output to keep token usage bounded
	return text.length > 8000 ? text.slice(0, 8000) + "\n…(truncated)" : text;
}

// --------------------------------------------------------------------------
// Tool parameter schemas (TypeBox)
// --------------------------------------------------------------------------

const RecallParams = Type.Object({
	query: Type.String({ description: "Free-text query for MPM's FTS5 + reinforcement-weighted recall." }),
	limit: Type.Optional(Type.Integer({ minimum: 1, maximum: 50, description: "Cap on results returned. Default 6." })),
	scope: Type.Optional(
		StringEnum(["all", "local", "shared"] as const, {
			description: "Recall scope. Default 'all' (federated).",
		}),
	),
});

const RememberParams = Type.Object({
	fact: Type.String({ description: "Durable observation, learning, or context to persist." }),
	collection: Type.Optional(
		StringEnum(["memories", "directives", "skills", "lessons"] as const, {
			description: "MPM collection. Default 'memories'. Use 'directives' for prime directives.",
		}),
	),
	tags: Type.Optional(
		Type.Array(Type.String(), { description: "Tags to apply. Defaults to ['pi-session']." }),
	),
	weight: Type.Optional(
		Type.Integer({ minimum: 0, maximum: 100, description: "Initial retrieval weight 0-100. Default 50." }),
	),
});

const SessionHandoffParams = Type.Object({
	summary: Type.String({ description: "One-paragraph summary of what was done and why." }),
	commitments: Type.Optional(
		Type.Array(Type.String(), { description: "Things this session committed to do or follow up on." }),
	),
	open_questions: Type.Optional(
		Type.Array(Type.String(), { description: "Unresolved questions to surface next session." }),
	),
	session_id: Type.Optional(
		Type.String({ description: "Logical session identifier. Defaults to the current Pi session id." }),
	),
});

const ExplainParams = Type.Object({
	query: Type.String({ description: "Query whose retrieval you want explained." }),
	limit: Type.Optional(Type.Integer({ minimum: 1, maximum: 20, description: "Number of hits to explain. Default 5." })),
});

// --------------------------------------------------------------------------
// Wake-context payload shape (subset of read_wake_context output we use)
// --------------------------------------------------------------------------

interface WakeContext {
	last_handoff?: {
		summary?: string;
		commitments?: string[];
		open_questions?: string[];
		created_at?: string;
	};
	epistemic_pressure?: {
		exceeded?: boolean;
		ratio?: number;
		threshold?: number;
	};
	active_mode?: string;
	active_persona?: string;
}

function renderWakeBlock(wake: WakeContext): string {
	const lines: string[] = ["## MPM Wake Context"];
	if (wake.last_handoff) {
		const h = wake.last_handoff;
		lines.push(`\n### Last handoff (${h.created_at ?? "unknown date"})\n${h.summary ?? "(no summary)"}`);
		if (h.commitments && h.commitments.length > 0) {
			lines.push(`\n**Commitments:**\n${h.commitments.map((c) => `- ${c}`).join("\n")}`);
		}
		if (h.open_questions && h.open_questions.length > 0) {
			lines.push(`\n**Open questions:**\n${h.open_questions.map((q) => `- ${q}`).join("\n")}`);
		}
	} else {
		lines.push("\n(no prior handoff)");
	}
	if (wake.active_persona) {
		lines.push(`\nActive persona: \`${wake.active_persona}\``);
	}
	if (wake.epistemic_pressure?.exceeded) {
		const ep = wake.epistemic_pressure;
		const ratioStr = typeof ep.ratio === "number" ? ep.ratio.toFixed(2) : "?";
		const lessonCount = (ep as { lesson_count?: number }).lesson_count;
		const lessonStr = typeof lessonCount === "number" ? `, lessons=${lessonCount}` : "";
		lines.push(
			`\nEpistemic pressure is elevated (ratio=${ratioStr}${lessonStr}). Consider running \`mpm ops stance compact epistemology\` to consolidate.`,
		);
	}
	lines.push(
		"\nUse the mpm_recall tool to query prior memories, mpm_remember to persist new ones, and mpm_session_handoff at the end of meaningful work.",
	);
	return lines.join("\n");
}

// --------------------------------------------------------------------------
// Extension factory
// --------------------------------------------------------------------------

export default function piMpmExtension(pi: ExtensionAPI) {
	// Cached wake context, populated on session_start, consumed on the first
	// before_agent_start turn of this session. Cleared after delivery so
	// subsequent turns don't re-inject the same banner.
	let cachedWake: WakeContext | null = null;
	let wakeDelivered = false;

	// ---------- Tools --------------------------------------------------------

	pi.registerTool({
		name: "mpm_recall",
		label: "MPM Recall",
		description:
			"Free-text recall against MPM's long-term memory substrate (FTS5 + reinforcement-weighted ranking). " +
			"Returns memories with id, content, weight, and provenance. Prefer this over re-reading old files when " +
			"the information may already be persisted. Use mpm_explain to inspect why a query matched.",
		parameters: RecallParams,
		async execute(_id, params, _signal, _onUpdate, _ctx) {
			const r = await callMpm("query_long_term_memory", {
				query: params.query,
				limit: params.limit ?? 6,
				scope: params.scope ?? "all",
			});
			if (!r.success) {
				return {
					content: [{ type: "text", text: formatFailure("query_long_term_memory", r) }],
					details: { ok: false },
				};
			}
			const payload = r.payload as {
				count?: number;
				scope?: string;
				memories?: Array<{ id: string; content: string; weight?: number; tags?: string; created_at?: string }>;
			};
			const mems = payload.memories ?? [];
			const lines: string[] = [];
			lines.push(
				`Found ${payload.count ?? mems.length} memor${mems.length === 1 ? "y" : "ies"} (scope=${payload.scope ?? "all"}).`,
			);
			if (mems.length === 0) {
				lines.push("(no memories matched the query)");
			} else {
				for (const m of mems) {
					const id = m.id ?? "?";
					const w = typeof m.weight === "number" ? ` weight=${m.weight}` : "";
					const tags = m.tags && m.tags !== "" ? ` tags=[${m.tags}]` : "";
					lines.push(`\n— [${id}]${w}${tags}\n${m.content ?? "(no content)"}\n`);
				}
			}
			return {
				content: [{ type: "text", text: lines.join("\n") }],
				details: { ok: true, count: mems.length, scope: payload.scope ?? "all" },
			};
		},
	});

	pi.registerTool({
		name: "mpm_remember",
		label: "MPM Remember",
		description:
			"Persist a durable observation into MPM's memory substrate. Use this when the user tells you to remember " +
			"something, when you learn a project-specific fact that will recur across sessions, or when you want to " +
			"preserve a decision rationale for later recall. Returns the assigned memory id.",
		parameters: RememberParams,
		async execute(_id, params, _signal, _onUpdate, _ctx) {
			const tags = params.tags ?? ["pi-session"];
			const weight = params.weight ?? 50;
			const r = await callMpm("save_to_memory", {
				fact: params.fact,
				collection: params.collection ?? "memories",
				tags,
				weight,
			});
			if (!r.success) {
				return {
					content: [{ type: "text", text: formatFailure("save_to_memory", r) }],
					details: { ok: false },
				};
			}
			const payload = r.payload as { id?: string; success?: boolean; content?: string };
			const newId = payload.id ?? "(no id returned)";
			return {
				content: [
					{
						type: "text",
						text: `Stored memory ${newId} (collection=${params.collection ?? "memories"}, weight=${weight}).`,
					},
				],
				details: { ok: true, id: newId, content: params.fact },
			};
		},
	});

	pi.registerTool({
		name: "mpm_session_handoff",
		label: "MPM Session Handoff",
		description:
			"End-of-session handoff to MPM: persists a one-paragraph summary plus optional commitments and open " +
			"questions. The next time any MPM-backed session calls read_wake_context, this handoff surfaces in the " +
			"wake block. Use near the end of a meaningful unit of work, not on every turn.",
		parameters: SessionHandoffParams,
		async execute(_id, params, _signal, _onUpdate, ctx) {
			const sessionId = params.session_id || ctx.sessionManager.getSessionId();
			const r = await callMpm("session_end", {
				summary: params.summary,
				commitments: params.commitments ?? [],
				open_questions: params.open_questions ?? [],
				session_id: sessionId,
			});
			if (!r.success) {
				return {
					content: [{ type: "text", text: formatFailure("session_end", r) }],
					details: { ok: false },
				};
			}
			const payload = r.payload as { handoff?: { id?: string }; success?: boolean };
			const handoffId = payload.handoff?.id ?? "(no id)";
			return {
				content: [{ type: "text", text: `Handoff ${handoffId} recorded. Future sessions will see this on wake.` }],
				details: { ok: true, id: handoffId },
			};
		},
	});

	pi.registerTool({
		name: "mpm_explain",
		label: "MPM Explain Retrieval",
		description:
			"Diagnostic for a recall query: returns MPM's per-node retrieval trace (BM25 base score, reuse count, " +
			"last retrieved timestamp, success count). Useful when mpm_recall returns something unexpected and the " +
			"agent (or user) wants to understand why.",
		parameters: ExplainParams,
		async execute(_id, params, _signal, _onUpdate, _ctx) {
			const r = await callMpm("explain_retrieval", {
				query: params.query,
				limit: params.limit ?? 5,
			});
			if (!r.success) {
				return {
					content: [{ type: "text", text: formatFailure("explain_retrieval", r) }],
					details: { ok: false },
				};
			}
			return {
				content: [{ type: "text", text: jsonToText(r.payload) }],
				details: { ok: true },
			};
		},
	});

	// ---------- Hooks --------------------------------------------------------

	pi.on("session_start", async (_event, ctx) => {
		// Pull the prior handoff + epistemic-pressure signal. Fail-open: any
		// mpm error logs a warning, session continues without a banner.
		try {
			const r = await callMpm("read_wake_context", {
				session_id: ctx.sessionManager.getSessionId(),
			});
			if (r.success && r.payload && typeof r.payload === "object") {
				cachedWake = r.payload as WakeContext;
			} else {
				ctx.ui?.notify?.("MPM: wake context unavailable.", "warning");
			}
		} catch {
			ctx.ui?.notify?.("MPM: wake context fetch threw.", "warning");
		}
	});

	pi.on("before_agent_start", async (event) => {
		// Inject the cached wake context into the system prompt exactly once
		// per session. Subsequent turns get the unmodified prompt.
		if (wakeDelivered || !cachedWake) return undefined;
		wakeDelivered = true;
		const block = renderWakeBlock(cachedWake);
		return {
			systemPrompt: event.systemPrompt + "\n\n" + block,
		};
	});

	// ---------- Commands -----------------------------------------------------

	pi.registerCommand("mpm-status", {
		description: "Show MPM install identity (version, database, counts). Runs `mpm info`.",
		handler: async (_args, ctx) => {
			const r = await callMpm("info", {});
			if (!r.success || !r.payload) {
				ctx.ui?.notify?.(`MPM status: unavailable (${formatFailure("info", r)})`, "error");
				return;
			}
			ctx.ui?.notify?.(jsonToText(r.payload), "info");
		},
	});
}
