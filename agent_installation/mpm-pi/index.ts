/**
 * mpm-pi — Pi extension that wires MPM's cognitive substrate into Pi.
 *
 * Static, hand-maintained bridge to a curated **17-tool subset** of the
 * current MPM Registry (21 Registry entries + the mpm_help discovery
 * closure registered via cmd/mpm-mcp = 22 tools total; 14 unified Domain
 * Tools + 3 standalone tools exposed by this adapter).
 *
 * Until the Phase 1/2 refactor, mpm-mcp exposed 77 granular tools. The
 * current Registry exposes 22 tools total. This Pi adapter registers 17
 * typed tools. The default initial MCP surface that hosts receive at
 * session start is the compact 3-tool surface (mpm_memory, mpm_context,
 * mpm_help); `MPM_EXPOSE_ALL_TOOLS=1` on the MCP env block restores the
 * full 22-tool surface.
 *
 * The 14 Domain Tools are "Fat RPC" — they take `{action, params}` and
 * the backend dispatches. That collapses ~77 distinct tool definitions
 * into 14 near-identical ones, permanently resolving the ~15KB prompt
 * bloat the old surface caused.
 *
 * Tools in the full Registry not registered here (`mpm_work`,
 * `mpm_resolve`, `mpm_blob_read`, `mpm_blob_search`) remain reachable via
 * `mpm call <tool> --payload '<json>'` from a subprocess.
 *
 * Provenance: every spawn inherits
 * `MPM_PROVENANCE_FRAMEWORK=pi` (see ./src/workspace.ts:buildProvenanceEnv)
 * so `artifact_provenance.framework_name="pi"` is stamped on every
 * memory, decision, lesson, handoff, and tool_invocation this extension
 * writes. Framework-scoped directives resolve correctly via
 * `mpm_context.read_directives` because of this attribution.
 *
 * Transport: each tool spawns `mpm call <tool> --payload '<json>'` as a
 * subprocess. mpm emits one zap-style log line to stderr and one JSON
 * envelope to stdout. The parser scans for the last `{…}` line in stdout
 * (see parseLastJsonLine below). All tools fail-open: if `mpm` is missing
 * from PATH, the tool returns a soft error envelope instead of throwing.
 */

import { spawn } from "node:child_process";
import { Type } from "@earendil-works/pi-ai";
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { withProvenance } from "./src/workspace.js";

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

const DEFAULT_TIMEOUT_MS = 30_000;

/**
 * Spawn `mpm call <tool> --payload <json>` and return the parsed envelope.
 * mpm emits one zap-style log line to stderr (e.g. "time=... level=INFO msg=...")
 * and a single JSON object to stdout on success. We find the JSON object
 * on stdout by scanning lines for one that parses cleanly.
 */
function callMpm(
	tool: string,
	payload: Record<string, unknown>,
	opts: { mpmBin?: string; timeoutMs?: number } = {},
): Promise<MpmCallResult> {
	const bin = opts.mpmBin ?? "mpm";
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
				env: withProvenance(),
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

function jsonToText(payload: unknown): string {
	if (payload === null || payload === undefined) return "(no payload)";
	const obj = payload as Record<string, unknown>;
	const text = JSON.stringify(obj, null, 2);
	return text.length > 8000 ? text.slice(0, 8000) + "\n…(truncated)" : text;
}

// --------------------------------------------------------------------------
// Wake-context payload shape (subset of mpm_context/read_wake_context output)
//
// The shared-core wake_context.go readActiveState populates `active_mode`
// from active.json → ResolveActiveMode (which falls back to "default" when
// the requested mode .md file is missing) and `active_persona` from
// ResolveActivePersona (same fallback). However, when active.json has no
// `modes` key at all, `active_mode` is left empty in the JSON payload
// (known shared-core asymmetry — see the canonical agent protocol and the
// integration report). The renderer here applies the same "default"
// fallback the substrate's human-readable formatWakeContext applies, so
// Pi agents always see an explicit mode/persona in wake regardless of
// shared-core state.
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
		lesson_count?: number;
	};
	active_mode?: string;
	active_persona?: string;
}

const DEFAULT_MODE = "default";
const DEFAULT_PERSONA = "default";

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
	// mode/persona: render explicit values when set; default to "default"
	// when absent so the agent always has a concrete identity pair. The
	// hardcoded safe default for both is "default" — see
	// internal/core/active_state.go:resolveActiveComponent defaultName
	// parameter, and the canonical mpm-agent-protocol.md mode/persona
	// contract.
	const mode = wake.active_mode && wake.active_mode.trim() !== "" ? wake.active_mode : DEFAULT_MODE;
	const persona =
		wake.active_persona && wake.active_persona.trim() !== "" ? wake.active_persona : DEFAULT_PERSONA;
	lines.push(`\nActive mode: \`${mode}\``);
	lines.push(`\nActive persona: \`${persona}\``);
	if (wake.epistemic_pressure?.exceeded) {
		const ep = wake.epistemic_pressure;
		const ratioStr = typeof ep.ratio === "number" ? ep.ratio.toFixed(2) : "?";
		const lessonCount = ep.lesson_count;
		const lessonStr = typeof lessonCount === "number" ? `, lessons=${lessonCount}` : "";
		lines.push(
			`\nEpistemic pressure is elevated (ratio=${ratioStr}${lessonStr}). Run \`mpm_system\` with action "compact" to drain outstanding eligible raw memories into lessons (≤50 per LLM batch, atomic per batch, repeats until no eligible rows remain).`,
		);
	}
	lines.push(
		"\nUse the mpm_memory tool to query prior memories and persist new ones, mpm_handoff (action \"write\") to record a handoff at the end of meaningful work, and mpm_scratchpad (actions \"flush\"/\"read\"/\"discard\"/\"promote\") for intra-session working state. The mpm-pi adapter registers 14 Domain Tools + 3 Standalones (17 typed tools total); tools in the full 22-tool MPM Registry (21 Registry entries + the mpm_help discovery closure) not exposed here remain reachable via `mpm call <tool> --payload '<json>'`. On hosts using the default compact 3-tool MCP surface, handoff is reached through `mpm_context` action `write_handoff`; set `MPM_EXPOSE_ALL_TOOLS=1` on the MCP env to restore the full 22-tool surface.",
	);
	return lines.join("\n");
}

// --------------------------------------------------------------------------
// Domain Tool schema + registration helper
//
// All 14 Domain Tools use the same "Fat RPC" shape: {action, params}.
// The action enum and per-action parameters are documented in each tool's
// description; validation happens in the mpm backend, which returns a
// descriptive error envelope on a bad action / missing field.
// --------------------------------------------------------------------------

function domainToolSchema() {
	return Type.Object({
		action: Type.String({
			description: "Which operation to run on this domain. See the tool description for the valid actions.",
		}),
		params: Type.Optional(
			Type.Record(Type.String(), Type.Any(), {
				description: "Free-form parameters for the chosen action. See the tool description for required/optional fields.",
			}),
		),
	});
}

interface DomainToolSpec {
	name: string;
	label: string;
	description: string;
}

/**
 * Register a Fat RPC Domain Tool. `execute` just forwards `action` and
 * `params` to the underlying `mpm call <name>` subprocess.
 */
function registerDomainTool(pi: ExtensionAPI, spec: DomainToolSpec): void {
	pi.registerTool({
		name: spec.name,
		label: spec.label,
		description: spec.description,
		parameters: domainToolSchema(),
		async execute(_id, params, _signal, _onUpdate, _ctx) {
			const { action, params: body } = params as { action: string; params?: Record<string, unknown> };
			const r = await callMpm(spec.name, { action, params: body ?? {} });
			if (!r.success) {
				return { content: [{ type: "text", text: formatFailure(spec.name, r) }], details: { ok: false } };
			}
			return { content: [{ type: "text", text: jsonToText(r.payload) }], details: { ok: true, action } };
		},
	});
}

export default function piMpmExtension(pi: ExtensionAPI) {
	// Cached wake context, populated on session_start, consumed on the first
	// before_agent_start turn of this session. Cleared after delivery so
	// subsequent turns don't re-inject the same banner.
	let cachedWake: WakeContext | null = null;
	let wakeDelivered = false;

	// ----- 2026-08-13 hardening: DB path invariant -----
	// Catch the silent-orphan-db failure mode by refusing to boot
	// against an unexpected db_path. Set MPM_REQUIRED_DB_PATH to a
	// canonical absolute path to activate the gate; when unset the
	// check is skipped so ad-hoc dev environments still work.
	const requiredDbPath = process.env.MPM_REQUIRED_DB_PATH;
	if (requiredDbPath && requiredDbPath.length > 0) {
		const hcPromise = callMpm("mpm_system", {
			action: "health_check",
			params: {},
		});
		hcPromise.then((r) => {
			const live = (r.success && r.payload && typeof r.payload === "object" && typeof (r.payload as Record<string, unknown>).db_path === "string")
				? (r.payload as Record<string, unknown>).db_path as string
				: null;
			if (!live) {
				throw new Error("mpm-pi: db_path invariant — health_check missing db_path (mpm server too old to be gated)");
			}
			if (live !== requiredDbPath) {
				throw new Error(
					`mpm-pi: refusing to boot — DB path invariant violated.\n` +
					`  expected: ${requiredDbPath}\n` +
					`  actual:   ${live}\n` +
					`This usually means two mpm installs on the same host, or a stale scratch db.\n` +
					`Run \`mpm status\` to see which workspace is current.`,
				);
			}
			pi.on("session_start", async () => {
				ctxSafeNotify(pi, `MPM: db_path invariant satisfied (${live})`);
			});
		}).catch((e: Error) => {
			throw e;
		});
	}

	function ctxSafeNotify(_pi: ExtensionAPI, msg: string): void {
		// pi does not have a top-level notifier at register-time; the
		// registered session_start hook above is the safe hook.
		// (no-op here; kept for future extension)
		void msg;
	}

	// ---------- 13 Unified Domain Tools (Fat RPC) -------------------------

	registerDomainTool(pi, {
		name: "mpm_memory",
		label: "MPM Memory",
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

	registerDomainTool(pi, {
		name: "mpm_theories",
		label: "MPM Theories",
		description: `Theory lifecycle. Literal actions:
  propose — Required params.hypothesis. Optional: params.validation_criteria, params.tags, params.dependencies, params.source_ids.
  resolve — Resolve a pending theory. Required params.theory_id, params.conclusion ("confirmed"|"disproven"), params.new_status ("proven"|"disproven"). Optional: params.winner_id.`,
	});

	registerDomainTool(pi, {
		name: "mpm_decisions",
		label: "MPM Decisions",
		description: `Decision recording. Literal actions:
  record — Record an architectural decision. Required params.context, params.choice, params.rationale. Optional: params.outcome, params.tags, params.source_ids.`,
	});

	registerDomainTool(pi, {
		name: "mpm_lessons",
		label: "MPM Lessons",
		description: `Lesson lifecycle. Literal actions:
  save — Required params.fact. Optional: params.type ("warning"|"practice"|"insight"), params.tags, params.source_ids.
  search — Search lessons by FTS5. Required params.query.
  list — List lessons. Optional: params.type.`,
	});

	registerDomainTool(pi, {
		name: "mpm_topics",
		label: "MPM Topics",
		description: `Topic clustering. Literal actions:
  create — Required params.name. Optional: params.description.
  search — Required params.query. Optional: params.limit (default 20).
  link — Link a memory to a topic. Required params.memory_id, params.topic_id. Optional: params.relevance (0-1).`,
	});

	registerDomainTool(pi, {
		name: "mpm_references",
		label: "MPM References",
		description: `Reference document management. Literal actions:
  add — Ingest a reference file. Required params.filepath. Optional: params.title.
  search — Search references by content. Required params.query. Optional: params.limit.
  list — List ingested references. Optional: params.limit (default 50), params.offset (default 0).`,
	});

	registerDomainTool(pi, {
		name: "mpm_evidence",
		label: "MPM Evidence",
		description: `Evidence management. Literal actions:
  add — Attach evidence to an artifact. Required params.artifact_id, params.type ("observation"|"test"|"reproduction"|"challenge"|"decision_outcome"|"external_reference"), params.source_group, params.created_by. Optional: params.artifact_type, params.notes, params.strength (-1..1), params.independence_factor.
  list — List evidence for an artifact. Required params.artifact_id. Optional: params.artifact_type.`,
	});

	registerDomainTool(pi, {
		name: "mpm_confidence",
		label: "MPM Confidence",
		description: `Confidence inspection and audit. Literal actions:
  show — Current confidence. Required params.artifact_id. Optional: params.artifact_type.
  recompute — Force recompute from evidence. Required params.artifact_id. Optional: params.artifact_type.
  explain — Factor breakdown + history trace. Required params.artifact_id. Optional: params.artifact_type.
  history — Confidence timeline. Required params.artifact_id. Optional: params.artifact_type, params.limit (default 50).
  changes — Change events (delta + trigger). Optional: params.artifact_id, params.artifact_type, params.since, params.since_seconds_ago, params.limit.
  trend — Linear fit over window. Required params.artifact_id. Optional: params.artifact_type, params.window_days (default 30).
  quality — Per-source quality stats. No params.`,
	});

	registerDomainTool(pi, {
		name: "mpm_context",
		label: "MPM Context",
		description: `Agent state and routing. Literal actions:
  read_wake_context — Session state, active mode, recent memories. No params.
  read_directives — Active behavioral directives. No params.
  proactive_recall_hint — Surface conversation-relevant memories. Required params.conversation_text. Optional: params.max_hints (default 3), params.min_score.
  query_global_rules — Query shared rules. Optional: params.query, params.limit.
  record_global_rule — Write a shared rule. Required params.fact, params.confirm (true). Optional: params.tags, params.weight, params.provenance.
  promote_to_global — Copy local memory to shared DB. Required params.memory_id, params.confirm (true).
  route — Evaluate mode/persona routing. Required params.prompt.`,
	});

	registerDomainTool(pi, {
		name: "mpm_skills",
		label: "MPM Skills",
		description: `Skill management. Literal actions:
  save — Required params.name, params.version, params.content. Optional: params.author, params.force.
  read — Fetch skill by name. Required params.name. Optional: params.version.
  list — List skills. Optional: params.scope ("local"|"shared"|"all").
  delete — Soft-delete a skill. Required params.skill_id.
  promote_to_global — Share a skill globally. Required params.skill_id, params.confirm (true).`,
	});

	registerDomainTool(pi, {
		name: "mpm_wakes",
		label: "MPM Wakes",
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

	registerDomainTool(pi, {
		name: "mpm_handoff",
		label: "MPM Handoff",
		description: `Inter-session communication: write, read, and audit handoff records. Literal actions:
  write — Write a handoff for the next session. Required params.session_id, params.summary. Optional: params.state ("clean"|"crashed"|"interrupted"|"force_end"), params.commitments, params.open_questions, params.note.
  read — Read a specific handoff. Required params.handoff_id. Optional: params.mark_read.
  list — List handoffs. Optional: params.session_id, params.unread, params.limit (default 10).
  shred — Hard-delete a handoff. Required params.handoff_id. Optional: params.confirm (true).`,
	});

	registerDomainTool(pi, {
		name: "mpm_scratchpad",
		label: "MPM Scratchpad",
		description: `Intra-session volatile working memory for thoughts, partial conclusions, and working context that may not survive to the next session. Literal actions:
  flush — Overwrite the scratchpad for a session. Required params.session_id, params.thesis. Optional: params.supporting.
  read — Read scratchpad for a session. Required params.session_id.
  discard — Delete scratchpad without promoting. Required params.session_id.
  promote — Promote scratchpad to memory, then delete. Required params.session_id.`,
	});

	registerDomainTool(pi, {
		name: "mpm_system",
		label: "MPM System",
		description: `Maintenance, audit, and diagnostics. Literal actions:
  gc_run — Lifecycle decay sweep. Optional: params.dry_run (default true), params.aggressive, params.max_age_hours (default 24). NOT for epistemic pressure — use "compact" when epistemic_pressure.exceeded is true.
  compact — Drain eligible raw memories into lessons in sequential batches of at most 50 (the LLM context safeguard); each batch is independently atomic. force=false (default) RELIEVES pressure — the drain stops as soon as raw_count <= threshold and may leave eligible raw memories remaining. force=true DRAINS everything — the threshold gate is bypassed and the drain continues until the substrate is empty or the per-invocation cap is hit. force does NOT widen the 50-item per-batch limit. Optional params: force (default false), max_batches (default 20, hard cap 100, silently clamped). Response envelope: success (false ONLY on mid-drain failure), batches_processed, raw_processed, lessons_created, raw_remaining, lesson_ids, stop_reason ("no_work" | "completed" | "threshold_reached" | "max_batches_reached" | "failure"), skipped_reason (set on no_work / threshold_reached only), and failed_batch + failure_reason on "failure". Inspect stop_reason (not success) to determine whether the substrate is fully drained.
  health_check — SQLite integrity + domain counts. No params.
  migrate — Import from markdown/JSON. Required params.from_path. Optional: params.format, params.label, params.dry_run, params.commit, params.commit_batch, params.undo_batch.
  query_audit_log — Query anomaly ledger. Optional: params.level, params.component, params.days (default 7), params.limit (default 20).
  list_clusters — List active audit clusters. No params.
  snooze_cluster — Temporarily hide a cluster. Required params.cluster_key, params.snooze_until. Optional: params.reason.
  resolve_cluster — Permanently dismiss a cluster. Required params.cluster_key. Optional: params.reason.
  annotate_cluster — Append forensic annotation. Required params.cluster_key, params.annotation. Optional: params.reason.`,
	});

	// ---------- 3 Standalone Tools ----------------------------------------

	pi.registerTool({
		name: "mpm_retrieval_diagnose",
		label: "MPM Retrieval Diagnose",
		description:
			"Run a standard FTS search and return a per-node diagnostic breakdown: Base FTS Match score, Reuse Count, Last Retrieved timestamp, and Success Count. The retrieval ordering is identical to mpm_memory/query — it layers observability on top without altering ranking. Use when you want to understand WHY a result ranked where it did.",
		parameters: Type.Object({
			query: Type.String({ description: "The FTS query string (same contract as mpm_memory query)." }),
			limit: Type.Optional(Type.Number({ description: "Max results to diagnose (default 10)." })),
			collection: Type.Optional(Type.String({ description: "Optional collection filter (memories, lessons, decisions, theories, skills)." })),
			scope: Type.Optional(Type.Union([Type.Literal("all"), Type.Literal("local"), Type.Literal("shared")], { default: "all" })),
			trace: Type.Optional(Type.Boolean({ description: "When true, returns the 3-stage pipeline diagnostic." })),
		}),
		async execute(_id, params, _signal, _onUpdate, _ctx) {
			const r = await callMpm("mpm_retrieval_diagnose", (params as Record<string, unknown>) ?? {});
			if (!r.success) {
				return { content: [{ type: "text", text: formatFailure("mpm_retrieval_diagnose", r) }], details: { ok: false } };
			}
			return { content: [{ type: "text", text: jsonToText(r.payload) }], details: { ok: true } };
		},
	});

	pi.registerTool({
		name: "mpm_log_to_changelog",
		label: "MPM Log to Changelog",
		description: "Self-report agent work as a changelog entry tied to a git commit SHA.",
		parameters: Type.Object({
			fact: Type.String({ description: "The work performed to record." }),
			commit_hash: Type.String({ description: "Git commit SHA to associate." }),
			tags: Type.Optional(
				Type.Union([Type.String(), Type.Array(Type.String())], {
					description: "Tags as comma-separated string OR a JSON array of strings.",
				}),
			),
		}),
		async execute(_id, params, _signal, _onUpdate, _ctx) {
			const r = await callMpm("mpm_log_to_changelog", (params as Record<string, unknown>) ?? {});
			if (!r.success) {
				return { content: [{ type: "text", text: formatFailure("mpm_log_to_changelog", r) }], details: { ok: false } };
			}
			return { content: [{ type: "text", text: jsonToText(r.payload) }], details: { ok: true } };
		},
	});

	pi.registerTool({
		name: "mpm_request_review",
		label: "MPM Request Review",
		description:
			"Concurrent multi-component review. Fetch artifact bodies from memory ids in 'artifacts' and send the same prompt + artifact to every component in 'components'. Strategy must be 'parallel' (v0.1). Returns rendered Markdown with one section per component. Independent results: one component's failure does not abort the others.",
		parameters: Type.Object({
			components: Type.Array(Type.String(), { description: "Substrate component names to review (e.g. ['memory','critic'])." }),
			prompt: Type.String({ description: "The instruction sent to every component." }),
			artifacts: Type.Optional(Type.Array(Type.String(), { description: "Optional memory ids to pass as pre-resolved text." })),
			strategy: Type.Optional(Type.Union([Type.Literal("parallel")], { default: "parallel" })),
			timeout_secs: Type.Optional(Type.Number({ description: "Optional total timeout in seconds." })),
		}),
		async execute(_id, params, _signal, _onUpdate, _ctx) {
			const r = await callMpm("mpm_request_review", (params as Record<string, unknown>) ?? {});
			if (!r.success) {
				return { content: [{ type: "text", text: formatFailure("mpm_request_review", r) }], details: { ok: false } };
			}
			return { content: [{ type: "text", text: jsonToText(r.payload) }], details: { ok: true } };
		},
	});

	// ---------- Hooks --------------------------------------------------------

	pi.on("session_start", async (_event, ctx) => {
		// Reset the once-per-session delivery flag so /new, /resume, and
		// /fork all get their own wake injection. Without this reset the
		// flag would persist across sessions in the same extension
		// instance, and only the very first session would receive the
		// wake banner.
		wakeDelivered = false;
		try {
			const r = await callMpm("mpm_context", {
				action: "read_wake_context",
				params: {},
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
			const r = await callMpmCli("info", []);
			if (!r.success || !r.raw) {
				ctx.ui?.notify?.(`MPM status: unavailable (${r.spawnError ?? "non-zero exit"})`, "error");
				return;
			}
			ctx.ui?.notify?.(r.raw, "info");
		},
	});
}

/**
 * Run `mpm <subcommand>` directly (e.g. `mpm info`), NOT through the
 * `mpm call` tool surface. `info` / `status` / `doctor` are CLI subcommands,
 * not registry tools, so they can't be reached via callMpm.
 */
function callMpmCli(
	subcommand: string,
	args: string[],
	opts: { mpmBin?: string; timeoutMs?: number } = {},
): Promise<{ success: boolean; raw: string; spawnError: string | null }> {
	const bin = opts.mpmBin ?? "mpm";
	const timeoutMs = opts.timeoutMs ?? DEFAULT_TIMEOUT_MS;

	return new Promise((resolve) => {
		let stdout = "";
		let timedOut = false;
		let settled = false;
		const finish = (r: { success: boolean; raw: string; spawnError: string | null }) => {
			if (settled) return;
			settled = true;
			resolve(r);
		};

		let child: ReturnType<typeof spawn>;
		try {
			child = spawn(bin, [subcommand, ...args], {
				stdio: ["ignore", "pipe", "pipe"],
				env: withProvenance(),
			});
		} catch (err) {
			finish({
				success: false,
				raw: "",
				spawnError: err instanceof Error ? err.message : String(err),
			});
			return;
		}

		child.on("error", (err) => {
			finish({ success: false, raw: stdout, spawnError: err.message });
		});

		child.stdout?.on("data", (chunk: Buffer) => (stdout += chunk.toString()));

		const timer = setTimeout(() => {
			timedOut = true;
			try {
				child.kill("SIGKILL");
			} catch {
				/* ignore */
			}
		}, timeoutMs);
		timer.unref();

		child.on("close", (code) => {
			clearTimeout(timer);
			finish({ success: !timedOut && code === 0, raw: stdout.trim(), spawnError: null });
		});
	});
}
