/**
 * opencode-mpm — OpenCode plugin that wires MPM's cognitive substrate in
 * as 16 typed tools.
 *
 * Architecture: lightweight adapter. No domain logic, no LLM prompts, no
 * caching. Each tool is a thin transport shim that stringifies its
 * payload and spawns `mpm call <tool> --payload '<json>'` as a subprocess.
 * The 13 Domain Tools use the "Fat RPC" `{action, params}` shape; the 3
 * Standalones use their own narrow schemas. The backend (mpm) handles
 * dispatch, validation, schema, and persistence.
 *
 * This is a deliberate replacement for the legacy 77-tool plugin that
 * shipped before the 13-aggregator schema collapse (2026-08-11). The
 * legacy plugin generated one tool per (action, artifact-type) pair —
 * ~15KB of prompt bloat per agent. The 16-tool adapter collapses to
 * one Zod schema shape with free-form `params`, validated by the mpm
 * backend.
 *
 * Boot-time health check: on plugin initialization we ping
 * `mpm call mpm_system action:health_check` with a 2s timeout. If the
 * ping fails or the response reports degraded scheduler/DB state,
 * we emit a loud warning to the OpenCode boot log so the operator
 * sees it on every restart. Per-call failures still fail-open (we
 * return a soft error envelope instead of throwing) so a transient
 * mpm blip doesn't kill the turn.
 *
 * Transport: `mpm call <tool> --payload '<json>'` via spawn. mpm
 * emits one zap-style log line to stderr and a single JSON envelope
 * to stdout. The parser scans for the last `{...}` line. See
 * parseLastJsonLine below.
 */

import { spawn } from "node:child_process";
import { tool } from "@opencode-ai/plugin";
import type { Plugin, PluginInput, PluginModule } from "@opencode-ai/plugin";

// --------------------------------------------------------------------------
// Subprocess adapter
// --------------------------------------------------------------------------

const DEFAULT_TIMEOUT_MS = 30_000;
const HEALTH_CHECK_TIMEOUT_MS = 2_000;
const MP_MEMORY_PATH_PREFIX = "mpm://"; // reserved for any future virtual-path use

interface MpmCallResult {
	success: boolean;
	payload: unknown;
	raw: string;
	stderr: string;
	timedOut: boolean;
	spawnError: string | null;
}

/**
 * Spawn `mpm call <tool> --payload <json>` and return the parsed envelope.
 * mpm emits one zap-style log line to stderr (e.g. "time=... level=INFO msg=...")
 * and a single JSON object to stdout on success. We find the JSON object
 * on stdout by scanning lines for one that parses cleanly.
 */
function callMpm(
	bin: string,
	toolName: string,
	payload: Record<string, unknown>,
	opts: { timeoutMs?: number } = {},
): Promise<MpmCallResult> {
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
			child = spawn(bin, ["call", toolName, "--payload", json], {
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

function formatFailure(toolName: string, r: MpmCallResult): string {
	if (r.spawnError) {
		return `${toolName}: mpm not reachable: ${r.spawnError}. Is the mpm binary on PATH?`;
	}
	if (r.timedOut) {
		return `${toolName}: mpm call timed out.`;
	}
	if (r.payload === null) {
		const tail = (r.stderr || r.raw).slice(-300);
		return `${toolName}: mpm returned no parseable JSON. tail: ${tail}`;
	}
	const payload = r.payload as { error?: string };
	if (payload?.error) {
		return `${toolName} error: ${payload.error}`;
	}
	return `${toolName}: mpm call failed.`;
}

function jsonToText(payload: unknown): string {
	if (payload === null || payload === undefined) return "(no payload)";
	const obj = payload as Record<string, unknown>;
	const text = JSON.stringify(obj, null, 2);
	return text.length > 8000 ? text.slice(0, 8000) + "\n…(truncated)" : text;
}

// --------------------------------------------------------------------------
// Boot-time health check
// --------------------------------------------------------------------------

interface HealthCheckResult {
	ok: boolean;
	reason: string;
}

/**
 * Async ping `mpm call mpm_system action:health_check` with a 2s timeout.
 * Returns {ok, reason}. We DO NOT throw on failure — the caller decides
 * whether to log a warning or hard-fail the plugin load.
 *
 * Triggers DEGRADED on:
 *   - spawn / timeout / parse failure
 *   - payload.ok === false
 *   - payload.scheduler.state && payload.scheduler.state !== "ok"
 *   - payload.scheduler.last_status === "error"
 */
async function pingHealth(bin: string): Promise<HealthCheckResult> {
	const r = await callMpm(
		bin,
		"mpm_system",
		{ action: "health_check", params: {} },
		{ timeoutMs: HEALTH_CHECK_TIMEOUT_MS },
	);
	if (r.timedOut) {
		return { ok: false, reason: `health check timed out after ${HEALTH_CHECK_TIMEOUT_MS}ms` };
	}
	if (r.spawnError) {
		return { ok: false, reason: `spawn failed: ${r.spawnError}` };
	}
	if (r.payload === null) {
		return { ok: false, reason: `no parseable JSON in mpm stdout. tail: ${(r.stderr || r.raw).slice(-200)}` };
	}
	const payload = r.payload as {
		ok?: boolean;
		scheduler?: { state?: string; last_status?: string; last_error?: string };
	};
	if (payload.ok === false) {
		return { ok: false, reason: `mpm reported ok=false` };
	}
	const sched = payload.scheduler ?? {};
	if (sched.state && sched.state !== "ok") {
		return { ok: false, reason: `scheduler.state=${sched.state} (last_error=${sched.last_error || ""})` };
	}
	if (sched.last_status === "error") {
		return { ok: false, reason: `scheduler.last_status=error (last_error=${sched.last_error || ""})` };
	}
	return { ok: true, reason: "" };
}

/**
 * Emit a loud warning to the OpenCode boot log. The Plugin contract
 * returns Promise<Hooks>; we don't have a logger reference, so we
 * use console.warn which surfaces in the standard OpenCode log.
 */
function emitBootWarning(bin: string, reason: string): void {
	const lines = [
		``,
		`⚠ opencode-mpm BOOT WARNING: mpm health check failed`,
		`  reason: ${reason}`,
		`  check that ${bin} exists and is healthy.`,
		`  tools will fail-open on each call until mpm is reachable.`,
		``,
	];
	for (const line of lines) console.warn(line);
}

// --------------------------------------------------------------------------
// Domain Tool schema + registration helper
//
// All 13 Domain Tools use the same "Fat RPC" shape: {action, params}.
// The action enum and per-action parameters are documented in each
// tool's description; validation happens in the mpm backend, which
// returns a descriptive error envelope on a bad action / missing field.
// --------------------------------------------------------------------------

function domainToolSchema() {
	return {
		action: tool.schema
			.string()
			.describe("Which operation to run on this domain. See the tool description for the valid actions."),
		params: tool.schema
			.object({})
			.optional()
			.describe("Free-form parameters for the chosen action. See the tool description for required/optional fields."),
	};
}

interface DomainToolSpec {
	name: string;
	description: string;
}

function registerDomainTool(
	bin: string,
	tools: Record<string, ReturnType<typeof tool>>,
	spec: DomainToolSpec,
): void {
	tools[spec.name] = tool({
		description: spec.description,
		args: domainToolSchema(),
		async execute(args, _ctx) {
			const { action, params: body } = args as { action: string; params?: Record<string, unknown> };
			const r = await callMpm(bin, spec.name, { action, params: body ?? {} });
			if (!r.success) {
				return formatFailure(spec.name, r);
			}
			return jsonToText(r.payload);
		},
	});
}

// --------------------------------------------------------------------------
// Plugin entry
// --------------------------------------------------------------------------

const OpenCodeMpmPlugin: Plugin = async (_ctx: PluginInput) => {
	const bin = process.env.MPM_BINARY ?? "mpm";

	// Boot-time health check. Logs to console.warn on failure so the
	// operator sees it on every OpenCode restart. Per-call failures
	// still fail-open (each tool returns a soft error envelope), so a
	// transient mpm blip doesn't kill the turn.
	const health = await pingHealth(bin);
	if (!health.ok) {
		emitBootWarning(bin, health.reason);
	}

	// ----- 2026-08-13 hardening: DB path invariant -----
	// Catch the silent-orphan-db failure mode by refusing to boot
	// against an unexpected db_path. Set MPM_REQUIRED_DB_PATH to a
	// canonical absolute path to activate the gate; when unset the
	// check is skipped so ad-hoc dev environments still work.
	const requiredDbPath = process.env.MPM_REQUIRED_DB_PATH;
	if (requiredDbPath && requiredDbPath.length > 0) {
		const r = await callMpm(bin, "mpm_system", { action: "health_check", params: {} }, {
			timeoutMs: HEALTH_CHECK_TIMEOUT_MS,
		});
		const payload = (r.payload ?? {}) as { db_path?: string; ok?: boolean };
		const live = payload.db_path;
		if (!live) {
			emitBootWarning(bin, "MPM_REQUIRED_DB_PATH is set but health_check did not surface db_path. The mpm server is too old to be gated.");
			throw new Error("opencode-mpm: db_path invariant — health_check missing db_path");
		}
		if (live !== requiredDbPath) {
			emitBootWarning(
				bin,
				`refusing to boot — DB path invariant violated.\n` +
				`  expected: ${requiredDbPath}\n` +
				`  actual:   ${live}\n` +
				`This usually means two mpm installs on the same host, or a stale scratch db.\n` +
				`Run \`mpm status\` to see which workspace is current.`,
			);
			throw new Error("opencode-mpm: db_path invariant violated");
		}
		console.warn(`opencode-mpm: db_path invariant satisfied (${live})`);
	}

	const tools: Record<string, ReturnType<typeof tool>> = {};

	// ---------- 13 Unified Domain Tools (Fat RPC) -------------------------

	registerDomainTool(bin, tools, {
		name: "mpm_memory",
		description: `Memory CRUD and lifecycle. Literal actions:
  save — Required params.fact. Optional: params.collection, params.tags, params.weight, params.ttl.
  query — Search memories (FTS5 + semantic). Required params.query. Optional: params.limit, params.collection, params.scope ("all"|"local"|"shared").
  shred — Soft-delete a memory / hard-delete a lesson. Required params.memory_id.
  reinforce — Bump weight. Required params.memory_id. Optional: params.delta (default 1).
  weaken — Reduce weight. Required params.memory_id. Optional: params.delta (default 1).
  snooze — Suppress from retrieval for N days. Required params.memory_id. Optional: params.days (default 1).
  set_weight — Set explicit weight 0-100. Required params.memory_id, params.weight.
  patch — JSON-Patch metadata (RFC 6902). Required params.memory_id, params.patch.
  promote — Mark as long-term. Required params.memory_id.
  review — List memories due for spaced repetition. Optional: params.days (default 30), params.limit (default 20).
  synthesize — LLM dedup/merge. Required params.memory_id.
  challenge — Weaken + create theory from contradiction. Required params.memory_id, params.evidence.
  commit_milestone — Narrative milestone. Required params.summary (>=50 chars). Optional: params.flavor, params.tags.`,
	});

	registerDomainTool(bin, tools, {
		name: "mpm_theories",
		description: `Theory lifecycle. Literal actions:
  propose — Required params.hypothesis. Optional: params.validation_criteria, params.tags, params.dependencies, params.source_ids.
  resolve — Resolve a pending theory. Required params.theory_id, params.conclusion ("confirmed"|"disproven"), params.new_status ("proven"|"disproven"). Optional: params.winner_id.`,
	});

	registerDomainTool(bin, tools, {
		name: "mpm_decisions",
		description: `Decision recording. Literal actions:
  record — Record an architectural decision. Required params.context, params.choice, params.rationale. Optional: params.outcome, params.tags, params.source_ids.`,
	});

	registerDomainTool(bin, tools, {
		name: "mpm_lessons",
		description: `Lesson lifecycle. Literal actions:
  save — Required params.fact. Optional: params.type ("warning"|"practice"|"insight"), params.tags, params.source_ids.
  search — Search lessons by FTS5. Required params.query.
  list — List lessons. Optional: params.type.`,
	});

	registerDomainTool(bin, tools, {
		name: "mpm_topics",
		description: `Topic clustering. Literal actions:
  create — Required params.name. Optional: params.description.
  search — Required params.query. Optional: params.limit (default 20).
  link — Link a memory to a topic. Required params.memory_id, params.topic_id. Optional: params.relevance (0-1).`,
	});

	registerDomainTool(bin, tools, {
		name: "mpm_references",
		description: `Reference document management. Literal actions:
  add — Ingest a reference file. Required params.filepath. Optional: params.title.
  search — Search references by content. Required params.query. Optional: params.limit.
  list — List ingested references. Optional: params.limit (default 50), params.offset (default 0).`,
	});

	registerDomainTool(bin, tools, {
		name: "mpm_evidence",
		description: `Evidence management. Literal actions:
  add — Attach evidence to an artifact. Required params.artifact_id, params.type ("observation"|"test"|"reproduction"|"challenge"|"decision_outcome"|"external_reference"), params.source_group, params.created_by. Optional: params.artifact_type, params.notes, params.strength (-1..1), params.independence_factor.
  list — List evidence for an artifact. Required params.artifact_id. Optional: params.artifact_type.`,
	});

	registerDomainTool(bin, tools, {
		name: "mpm_confidence",
		description: `Confidence inspection and audit. Literal actions:
  show — Current confidence. Required params.artifact_id. Optional: params.artifact_type.
  recompute — Force recompute from evidence. Required params.artifact_id. Optional: params.artifact_type.
  explain — Factor breakdown + history trace. Required params.artifact_id. Optional: params.artifact_type.
  history — Confidence timeline. Required params.artifact_id. Optional: params.artifact_type, params.limit (default 50).
  changes — Change events (delta + trigger). Optional: params.artifact_id, params.artifact_type, params.since, params.since_seconds_ago, params.limit.
  trend — Linear fit over window. Required params.artifact_id. Optional: params.artifact_type, params.window_days (default 30).
  quality — Per-source quality stats. No params.`,
	});

	registerDomainTool(bin, tools, {
		name: "mpm_context",
		description: `Agent state and routing. Literal actions:
  read_wake_context — Session state, active mode, recent memories. No params.
  read_directives — Active behavioral directives. No params.
  proactive_recall_hint — Surface conversation-relevant memories. Required params.conversation_text. Optional: params.max_hints (default 3), params.min_score.
  query_global_rules — Query shared rules. Optional: params.query, params.limit.
  record_global_rule — Write a shared rule. Required params.fact, params.confirm (true). Optional: params.tags, params.weight, params.provenance.
  promote_to_global — Copy local memory to shared DB. Required params.memory_id, params.confirm (true).
  route — Evaluate mode/persona routing. Required params.prompt.`,
	});

	registerDomainTool(bin, tools, {
		name: "mpm_skills",
		description: `Skill management. Literal actions:
  save — Required params.name, params.version, params.content. Optional: params.author, params.force.
  read — Fetch skill by name. Required params.name. Optional: params.version.
  list — List skills. Optional: params.scope ("local"|"shared"|"all").
  delete — Soft-delete a skill. Required params.skill_id.
  promote_to_global — Share a skill globally. Required params.skill_id, params.confirm (true).`,
	});

	registerDomainTool(bin, tools, {
		name: "mpm_wakes",
		description: `Wake scheduling and inspection. Literal actions:
  schedule — Required params.reason, params.target_time (epoch/duration/ISO-8601). Optional: params.theory_id, params.recurring_rule, params.metadata.
  check — Pull due wakes. Optional: params.kinds (array; default notification-only; ["*"] for all).
  check_pending_event — Pull event wakes for this session. Optional: params.session_id.
  list — List scheduled wakes. Optional: params.include_fired, params.overdue_only, params.limit.
  digest — Compact overdue wake summary. Optional: params.top_n (default 5).
  upsert_task — Create/update a cron task. Required params.id, params.name, params.cron_expr, params.directive_id, params.status.
  list_tasks — List all scheduled tasks. No params.
  delete_task — Hard-delete a task. Required params.id.`,
	});

	registerDomainTool(bin, tools, {
		name: "mpm_session",
		description: `Session lifecycle and scratchpad. Literal actions:
  end — Write a handoff for next wake. Required params.session_id, params.summary. Optional: params.state, params.commitments, params.open_questions.
  handoff — Read latest handoff. Optional: params.unread, params.mark_read.
  list_handoffs — List handoff history. Optional: params.limit (default 10), params.unread.
  flush — Overwrite the ephemeral scratchpad. Required params.session_id, params.thesis. Optional: params.supporting.
  read — Read scratchpad for a session. Required params.session_id.
  discard — Delete scratchpad without promoting. Required params.session_id.
  promote_scratchpad — Promote scratchpad to memory, then delete. Required params.session_id.`,
	});

	registerDomainTool(bin, tools, {
		name: "mpm_system",
		description: `Maintenance, audit, and diagnostics. Literal actions:
  gc_run — Lifecycle decay sweep. Optional: params.dry_run (default true), params.aggressive, params.max_age_hours (default 24).
  compact — Compact raw memories into a lesson. Optional: params.force.
  health_check — SQLite integrity + domain counts. No params.
  migrate — Import from markdown/JSON. Required params.from_path. Optional: params.format, params.label, params.dry_run, params.commit, params.commit_batch, params.undo_batch.
  query_audit_log — Query anomaly ledger. Optional: params.level, params.component, params.days (default 7), params.limit (default 20).
  list_clusters — List active audit clusters. No params.
  snooze_cluster — Temporarily hide a cluster. Required params.cluster_key, params.snooze_until. Optional: params.reason.
  resolve_cluster — Permanently dismiss a cluster. Required params.cluster_key. Optional: params.reason.
  annotate_cluster — Append forensic annotation. Required params.cluster_key, params.annotation. Optional: params.reason.`,
	});

	// ---------- 3 Standalone Tools ----------------------------------------

	tools.explain_retrieval = tool({
		description:
			"Run a standard FTS search and return a per-node diagnostic breakdown: Base FTS Match score, Reuse Count, Last Retrieved timestamp, and Success Count. The retrieval ordering is identical to mpm_memory/query — it layers observability on top without altering ranking. Use when you want to understand WHY a result ranked where it did.",
		args: {
			query: tool.schema.string().describe("The FTS query string (same contract as mpm_memory query)."),
			limit: tool.schema.number().optional().describe("Max results to diagnose (default 10)."),
			collection: tool.schema.string().optional().describe("Optional collection filter (memories, lessons, decisions, theories, skills)."),
			scope: tool.schema.enum(["all", "local", "shared"]).optional().describe("Optional scope filter."),
			trace: tool.schema.boolean().optional().describe("When true, returns the 3-stage pipeline diagnostic."),
		},
		async execute(args, _ctx) {
			const r = await callMpm(bin, "explain_retrieval", (args as Record<string, unknown>) ?? {});
			if (!r.success) return formatFailure("explain_retrieval", r);
			return jsonToText(r.payload);
		},
	});

	tools.log_to_changelog = tool({
		description: "Self-report agent work as a changelog entry tied to a git commit SHA.",
		args: {
			fact: tool.schema.string().describe("The work performed to record."),
			commit_hash: tool.schema.string().describe("Git commit SHA to associate."),
			tags: tool.schema.union([tool.schema.string(), tool.schema.array(tool.schema.string())]).optional().describe("Tags as comma-separated string OR a JSON array of strings."),
		},
		async execute(args, _ctx) {
			const r = await callMpm(bin, "log_to_changelog", (args as Record<string, unknown>) ?? {});
			if (!r.success) return formatFailure("log_to_changelog", r);
			return jsonToText(r.payload);
		},
	});

	tools.request_review = tool({
		description:
			"Concurrent multi-component review. Fetch artifact bodies from memory ids in 'artifacts' and send the same prompt + artifact to every component in 'components'. Strategy must be 'parallel' (v0.1). Returns rendered Markdown with one section per component. Independent results: one component's failure does not abort the others.",
		args: {
			components: tool.schema.array(tool.schema.string()).describe("Substrate component names to review (e.g. ['memory','critic'])."),
			prompt: tool.schema.string().describe("The instruction sent to every component."),
			artifacts: tool.schema.array(tool.schema.string()).optional().describe("Optional memory ids to pass as pre-resolved text."),
			strategy: tool.schema.literal("parallel").optional().describe("Must be 'parallel' (v0.1)."),
			timeout_secs: tool.schema.number().optional().describe("Optional total timeout in seconds."),
		},
		async execute(args, _ctx) {
			const r = await callMpm(bin, "request_review", (args as Record<string, unknown>) ?? {});
			if (!r.success) return formatFailure("request_review", r);
			return jsonToText(r.payload);
		},
	});

	return { tool: tools };
};

export default {
	server: OpenCodeMpmPlugin,
} satisfies PluginModule;
