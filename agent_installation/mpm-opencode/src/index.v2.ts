/**
 * mpm-opencode — OpenCode 2.x plugin wiring MPM's cognitive
 * substrate in as typed tools over MPM's registry transport.
 *
 * This is the compiled-payload implementation staged under the plugin
 * package (see server.js at the package root: OpenCode resolves a
 * directory plugin by a root-level `server.*` module and never consults
 * package.json main/exports).
 *
 * History:
 *   src/index.ts     — the OpenCode V1 implementation (`@opencode-ai/plugin`).
 *                      NOT loadable by OpenCode 2 regardless of registration;
 *                      kept as the historical reference for the tool surface.
 *   src/index.v2.ts  — this module: the 2026-10-16 port to the OpenCode 2
 *                      plugin API (`@opencode/plugin`), runtime-verified
 *                      against opencode v2.0.30.
 *
 * Architectural mapping (V1 -> V2):
 *   - async plugin fn returning hooks  ->  Plugin.define({ id, setup(ctx) })
 *   - `tool` hook map (tool() helper)  ->  ctx.tool.transform(editor => editor.add(...))
 *   - experimental.chat.system.transform -> ctx.session.hook("context") and
 *                                        event.system.push({type:"text",...})
 *   - shell.env                        ->  ctx.shell.hook("create.before")
 *   - execute(args, ctx) with ctx.model/ctx.sessionID
 *                                      ->  execute(input, context) with
 *                                          context.sessionID / context.signal
 *                                          (V2 does not surface `model` on the
 *                                          tool context; provenance therefore
 *                                          stamps framework + invocation +
 *                                          framework session id, no model id.)
 *
 * Transport surface unchanged from V1: the 13 Fat-RPC Domain Tools
 * ({action, params}) plus the 3 Standalone Tools. Each execute() spawns
 * `mpm call <tool> --payload '<json>'` and returns text content (fail-open
 * error envelope on transport failure).
 *
 * Boot-time health check: on plugin load we ping
 * `mpm call mpm_system --payload '{"action":"health_check","params":{}}'`
 * with a 2s timeout. On degraded result we console.warn the discovery audit
 * so the operator sees it on every restart. Per-call failures fail-open.
 */
import { spawn } from "node:child_process";
import { Plugin } from "@opencode/plugin";
import { withWorkspace } from "./workspace.js";
import { resolveMpmBinary, DiscoveryAttempt } from "./resolve-mpm-binary.js";

/**
 * JSON decoded from mpm's stdout. Deliberately `any`-shaped: mpm's payload
 * envelope is the substrate's own contract, validated downstream by the
 * caller's field checks (and the whole point of the fail-open envelope).
 */
type MpmPayload = Record<string, any>;

interface CallResult {
  success: boolean;
  payload: MpmPayload | null;
  raw: string;
  stderr: string;
  timedOut: boolean;
  spawnError: string | null;
}

interface CallOptions {
  timeoutMs?: number;
  env?: Record<string, string>;
}

// --------------------------------------------------------------------------
// MPM binary resolution
// --------------------------------------------------------------------------
const _discoveryAttempts: DiscoveryAttempt[] = [];
const RESOLVED_MPM_BINARY = resolveMpmBinary(process.env, undefined, _discoveryAttempts);

// --------------------------------------------------------------------------
// Subprocess adapter (unchanged from V1)
// --------------------------------------------------------------------------
const DEFAULT_TIMEOUT_MS = 30_000;
const HEALTH_CHECK_TIMEOUT_MS = 2_000;
// Session wake context cache — only inject once per session
const wakeContextCache = new Map<string, string>();

function callMpm(
  bin: string,
  toolName: string,
  payload: unknown,
  opts: CallOptions = {},
): Promise<CallResult> {
  const timeoutMs = opts.timeoutMs ?? DEFAULT_TIMEOUT_MS;
  const json = JSON.stringify(payload);
  return new Promise((resolve) => {
    let stdout = "";
    let stderr = "";
    let timedOut = false;
    let settled = false;
    const finish = (r: CallResult) => {
      if (settled) return;
      settled = true;
      resolve(r);
    };
    let child;
    try {
      child = spawn(bin, ["call", toolName, "--payload", json], {
        stdio: ["ignore", "pipe", "pipe"],
        env: withWorkspace(opts.env ?? {}),
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
    child.stdout?.on("data", (chunk) => (stdout += chunk.toString()));
    child.stderr?.on("data", (chunk) => (stderr += chunk.toString()));
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
      const success = !timedOut && parsed !== null && parsed.success !== false;
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

function parseLastJsonLine(s: string): MpmPayload | null {
  if (!s) return null;
  const lines = s.split("\n");
  for (let i = lines.length - 1; i >= 0; i--) {
    const t = lines[i].trim();
    if (t.startsWith("{") && t.endsWith("}")) {
      try {
        return JSON.parse(t) as MpmPayload;
      } catch {
        // keep scanning
      }
    }
  }
  return null;
}

function formatFailure(toolName: string, r: CallResult): string {
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
  const payload = r.payload;
  if (payload?.error) {
    return `${toolName} error: ${payload.error}`;
  }
  return `${toolName}: mpm call failed.`;
}

function jsonToText(payload: MpmPayload | null): string {
  if (payload === null || payload === undefined) return "(no payload)";
  const obj = payload;
  const text = JSON.stringify(obj, null, 2);
  return text.length > 8000 ? text.slice(0, 8000) + "\n…(truncated)" : text;
}

// --------------------------------------------------------------------------
// Wake context injection (OpenCode V2 equivalent of Claude Code SessionStart)
// --------------------------------------------------------------------------
async function fetchWakeContext(bin: string): Promise<string> {
  const r = await callMpm(
    bin,
    "mpm_context",
    { action: "read_wake_context", params: { format: "system-prompt" } },
    { timeoutMs: 5_000 },
  );
  if (!r.success || r.payload === null) {
    return "";
  }
  const payload = r.payload;
  return payload?.context ?? payload?.text ?? "";
}

// --------------------------------------------------------------------------
// Provenance env
// --------------------------------------------------------------------------
function buildProvenanceEnv(
  model: { id?: string } | undefined,
  sessionID: string | undefined,
): Record<string, string> {
  const env: Record<string, string> = {
    MPM_PROVENANCE_FRAMEWORK: "opencode",
  };
  if (model?.id) {
    env.MPM_PROVENANCE_MODEL = model.id;
  }
  if (sessionID) {
    // OpenCode's sessionID is the host's continuing conversation/session
    // identity, NOT an invocation-lineage ancestor. Stamp it as
    // MPM_PROVENANCE_FRAMEWORK_SESSION_ID.
    env.MPM_PROVENANCE_FRAMEWORK_SESSION_ID = sessionID;
  }
  env.MPM_PROVENANCE_INVOCATION_ID = `inv_${crypto.randomUUID()}`;
  return env;
}

function callMpmWithProvenance(
  bin: string,
  toolName: string,
  payload: unknown,
  opts: { model?: { id?: string }; sessionID?: string; timeoutMs?: number } = {},
): Promise<CallResult> {
  const { model, sessionID, timeoutMs } = opts;
  const provenanceEnv = buildProvenanceEnv(model, sessionID);
  const env: Record<string, string> = {};
  for (const [key, value] of Object.entries(process.env)) {
    if (value !== undefined) env[key] = value;
  }
  Object.assign(env, provenanceEnv);
  return callMpm(bin, toolName, payload, {
    timeoutMs,
    env: withWorkspace(env),
  });
}

async function pingHealth(
  bin: string,
): Promise<{ ok: boolean; reason: string }> {
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
  const payload = r.payload;
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

function emitBootWarning(bin: string, reason: string): void {
  const lines = [
    ``,
    `⚠ mpm-opencode BOOT WARNING: mpm could not be resolved at OpenCode startup`,
    `  reason: ${reason}`,
    `  resolved mpm: ${bin}`,
    `  discovery audit:`,
    ..._discoveryAttempts.map(
      (a) => `    - [${a.reason}] ${a.candidate}` +
        ` (exists=${a.exists}, executable=${a.executable}` +
        (a.resolved ? `, resolved=${a.resolved}` : "") +
        ")",
    ),
    `  action: install/repair MPM at one of the paths above, then restart OpenCode.`,
    `  note: tools fail-open on each call (soft error envelope) until mpm is reachable.`,
    `  note: this process will not auto-rediscover mpm if it is installed later —`,
    `        the resolved binary is cached at module load.`,
    ``,
  ];
  for (const line of lines) console.warn(line);
}

// --------------------------------------------------------------------------
// Domain Tool specs (13 unified Fat-RPC tools) + standalone tools
// Descriptions copied verbatim from the V1 plugin (src/index.ts) so the
// agent-visible contract is identical. Ported from the runtime-verified
// dist artifact; the compiled output of this file must stay behaviourally
// identical to that artifact.
// --------------------------------------------------------------------------
type DomainToolSpec = { name: string; description: string };
type StandaloneToolSpec = { name: string; description: string; input: MpmPayload };
type DomainToolInput = { action?: string; params?: MpmPayload };

const DOMAIN_TOOLS: DomainToolSpec[] = [
    {
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
    },
    {
        name: "mpm_theories",
        description: `Theory lifecycle. Literal actions:
  propose — Required params.hypothesis. Optional: params.validation_criteria, params.tags, params.dependencies, params.source_ids.
  resolve — Resolve a pending theory. Required params.theory_id, params.conclusion ("confirmed"|"disproven"), params.new_status ("proven"|"disproven"). Optional: params.winner_id.`,
    },
    {
        name: "mpm_decisions",
        description: `Decision recording. Literal actions:
  record — Record an architectural decision. Required params.context, params.choice, params.rationale. Optional: params.outcome, params.tags, params.source_ids.`,
    },
    {
        name: "mpm_lessons",
        description: `Lesson lifecycle. Literal actions:
  save — Required params.fact. Optional: params.type ("warning"|"practice"|"insight"), params.tags, params.source_ids.
  search — Search lessons by FTS5. Required params.query.
  list — List lessons. Optional: params.type.`,
    },
    {
        name: "mpm_topics",
        description: `Topic clustering. Literal actions:
  create — Required params.name. Optional: params.description.
  search — Required params.query. Optional: params.limit (default 20).
  link — Link a memory to a topic. Required params.memory_id, params.topic_id. Optional: params.relevance (0-1).`,
    },
    {
        name: "mpm_references",
        description: `Reference document management. Literal actions:
  add — Ingest a reference file. Required params.filepath. Optional: params.title.
  search — Search references by content. Required params.query. Optional: params.limit.
  list — List ingested references. Optional: params.limit (default 50), params.offset (default 0).`,
    },
    {
        name: "mpm_evidence",
        description: `Evidence management. Literal actions:
  add — Attach evidence to an artifact. Required params.artifact_id, params.type ("observation"|"test"|"reproduction"|"challenge"|"decision_outcome"|"external_reference"), params.source_group, params.created_by. Optional: params.artifact_type, params.notes, params.strength (-1..1), params.independence_factor.
  list — List evidence for an artifact. Required params.artifact_id. Optional: params.artifact_type.`,
    },
    {
        name: "mpm_confidence",
        description: `Confidence inspection and audit. Literal actions:
  show — Current confidence. Required params.artifact_id. Optional: params.artifact_type.
  recompute — Force recompute from evidence. Required params.artifact_id. Optional: params.artifact_type.
  explain — Factor breakdown + history trace. Required params.artifact_id. Optional: params.artifact_type.
  history — Confidence timeline. Required params.artifact_id. Optional: params.artifact_type, params.limit (default 50).
  changes — Change events (delta + trigger). Optional: params.artifact_id, params.artifact_type, params.since, params.since_seconds_ago, params.limit.
  trend — Linear fit over window. Required params.artifact_id. Optional: params.artifact_type, params.window_days (default 30).
  quality — Per-source quality stats. No params.`,
    },
    {
        name: "mpm_context",
        description: `Agent state and routing. Literal actions:
  read_wake_context — Session state, active mode, recent memories. No params.
  read_directives — Active behavioral directives. No params.
  proactive_recall_hint — Surface conversation-relevant memories. Required params.conversation_text. Optional: params.max_hints (default 3), params.min_score.
  query_global_rules — Query shared rules. Optional: params.query, params.limit.
  record_global_rule — Write a shared rule. Required params.fact, params.confirm (true). Optional: params.tags, params.weight, params.provenance.
  promote_to_global — Copy local memory to shared DB. Required params.memory_id, params.confirm (true).
  route — Evaluate mode/persona routing. Required params.prompt.`,
    },
    {
        name: "mpm_skills",
        description: `Skill management. Literal actions:
  save — Required params.name, params.version, params.content. Optional: params.author, params.force.
  read — Fetch skill by name. Required params.name. Optional: params.version.
  list — List skills. Optional: params.scope ("local"|"shared"|"all").
  delete — Soft-delete a skill. Required params.skill_id.
  promote_to_global — Share a skill globally. Required params.skill_id, params.confirm (true).`,
    },
    {
        name: "mpm_wakes",
        description: `Wake scheduling and inspection. Literal actions:
  schedule — Required params.reason, params.target_time (epoch/duration/ISO-8601). Optional: params.theory_id, params.metadata. For recurring workflows use upsert_task (cron).
  check — Pull due wakes. Optional: params.kinds (array; default notification-only; ["*"] for all).
  check_pending_event — Pull event wakes for this session. Optional: params.session_id.
  list — List scheduled wakes. Optional: params.include_fired, params.overdue_only, params.limit.
  digest — Compact overdue wake summary. Optional: params.top_n (default 5).
  upsert_task — Create/update a cron task. Required params.id, params.name, params.cron_expr, params.directive_id, params.status.
  list_tasks — List all scheduled tasks. No params.
  delete_task — Hard-delete a task. Required params.id.`,
    },
    {
        name: "mpm_handoff",
        description: `Inter-session communication: write, read, and audit handoff records. Literal actions:
  write — Write a handoff for the next session. Required params.session_id, params.summary. Optional: params.state ("clean"|"crashed"|"interrupted"|"force_end"), params.commitments, params.open_questions, params.note.
  read — Read a specific handoff. Required params.handoff_id. Optional: params.mark_read.
  list — List handoffs. Optional: params.session_id, params.unread, params.limit (default 10).
  shred — Hard-delete a handoff. Required params.handoff_id. Optional: params.confirm (true).`,
    },
    {
        name: "mpm_scratchpad",
        description: `Intra-session volatile working memory for thoughts, partial conclusions, and working context that may not survive to the next session. Literal actions:
  flush — Overwrite the scratchpad for a session. Required params.session_id, params.thesis. Optional: params.supporting.
  read — Read scratchpad for a session. Required params.session_id.
  discard — Delete scratchpad without promoting. Required params.session_id.
  promote — Promote scratchpad to memory, then delete. Required params.session_id.`,
    },
    {
        name: "mpm_system",
        description: `Maintenance, audit, and diagnostics. Literal actions:
  gc_run — Lifecycle decay sweep. Optional: params.dry_run (default true), params.aggressive, params.max_age_hours (default 24).
  compact — Drain eligible raw memories into lessons in sequential batches of at most 50 (the LLM context safeguard); each batch is independently atomic. force=false (default) RELIEVES pressure — the drain stops as soon as raw_count <= threshold and may leave eligible raw memories remaining. force=true DRAINS everything — the threshold gate is bypassed and the drain continues until the substrate is empty or the per-invocation cap is hit. force does NOT widen the 50-item per-batch limit. Optional params: force (default false), max_batches (default 20, hard cap 100, silently clamped). Response envelope: success (false ONLY on mid-drain failure), batches_processed, raw_processed, lessons_created, raw_remaining, lesson_ids, stop_reason ("no_work" | "completed" | "threshold_reached" | "max_batches_reached" | "failure"), skipped_reason (set on no_work / threshold_reached only), and failed_batch + failure_reason on "failure". Inspect stop_reason (not success) to determine whether the substrate is fully drained.
  health_check — SQLite integrity + domain counts. No params.
  migrate — Import from markdown/JSON. Required params.from_path. Optional: params.format, params.label, params.dry_run, params.commit, params.commit_batch, params.undo_batch.
  query_audit_log — Query anomaly ledger. Optional: params.level, params.component, params.days (default 7), params.limit (default 20).
  list_clusters — List active audit clusters. No params.
  snooze_cluster — Temporarily hide a cluster. Required params.cluster_key, params.snooze_until. Optional: params.reason.
  resolve_cluster — Permanently dismiss a cluster. Required params.cluster_key. Optional: params.reason.
  annotate_cluster — Append forensic annotation. Required params.cluster_key, params.annotation. Optional: params.reason.`,
    },
];

const DOMAIN_INPUT_SCHEMA: MpmPayload = {
    type: "object",
    properties: {
        action: {
            type: "string",
            description: "Which operation to run on this domain. See the tool description for the valid actions.",
        },
        params: {
            type: "object",
            additionalProperties: true,
            description: "Free-form parameters for the chosen action. See the tool description for required/optional fields.",
        },
    },
    required: ["action"],
    additionalProperties: false,
};

const STANDALONE_TOOLS: StandaloneToolSpec[] = [
    {
        name: "mpm_retrieval_diagnose",
        description: "Run a standard FTS search and return a per-node diagnostic breakdown: Base FTS Match score, Reuse Count, Last Retrieved timestamp, and Success Count. The retrieval ordering is identical to mpm_memory/query — it layers observability on top without altering ranking. Use when you want to understand WHY a result ranked where it did.",
        input: {
            type: "object",
            properties: {
                query: { type: "string", description: "The FTS query string (same contract as mpm_memory query)." },
                limit: { type: "number", description: "Max results to diagnose (default 10)." },
                collection: { type: "string", description: "Optional collection filter (memories, lessons, decisions, theories, skills)." },
                scope: { type: "string", enum: ["all", "local", "shared"], description: "Optional scope filter." },
                trace: { type: "boolean", description: "When true, returns the 3-stage pipeline diagnostic." },
            },
            required: ["query"],
            additionalProperties: false,
        },
    },
    {
        name: "mpm_log_to_changelog",
        description: "Self-report agent work as a changelog entry tied to a git commit SHA.",
        input: {
            type: "object",
            properties: {
                fact: { type: "string", description: "The work performed to record." },
                commit_hash: { type: "string", description: "Git commit SHA to associate." },
                tags: {
                    anyOf: [
                        { type: "string", description: "Comma-separated tags" },
                        { type: "array", items: { type: "string" } },
                    ],
                    description: "Tags as comma-separated string OR a JSON array of strings.",
                },
            },
            required: ["fact", "commit_hash"],
            additionalProperties: false,
        },
    },
    {
        name: "mpm_request_review",
        description: "Concurrent multi-component review. Fetch artifact bodies from memory ids in 'artifacts' and send the same prompt + artifact to every component in 'components'. Strategy must be 'parallel' (v0.1). Returns rendered Markdown with one section per component. Independent results: one component's failure does not abort the others.",
        input: {
            type: "object",
            properties: {
                components: { type: "array", items: { type: "string" }, description: "Substrate component names to review (e.g. ['memory','critic'])." },
                prompt: { type: "string", description: "The instruction sent to every component." },
                artifacts: { type: "array", items: { type: "string" }, description: "Optional memory ids to pass as pre-resolved text." },
                strategy: { type: "string", enum: ["parallel"], description: "Must be 'parallel' (v0.1)." },
                timeout_secs: { type: "number", description: "Optional total timeout in seconds." },
            },
            required: ["components", "prompt"],
            additionalProperties: false,
        },
    },
];

// --------------------------------------------------------------------------
// Plugin entry (OpenCode 2 API)
// --------------------------------------------------------------------------
export default Plugin.define({
    id: "mpm-opencode",
    async setup(ctx) {
        const bin = RESOLVED_MPM_BINARY;
        console.warn(`mpm-opencode (V2 port) loading; mpm binary: ${bin}`);

        // Boot-time health check (same semantics as V1).
        const health = await pingHealth(bin);
        if (!health.ok) {
            emitBootWarning(bin, health.reason);
        }

        // DB path invariant (2026-08-13 hardening, ported from V1).
        const requiredDbPath = process.env.MPM_REQUIRED_DB_PATH;
        if (requiredDbPath && requiredDbPath.length > 0) {
            const r = await callMpm(bin, "mpm_system", { action: "health_check", params: {} }, { timeoutMs: HEALTH_CHECK_TIMEOUT_MS });
            const payload = r.payload ?? {};
            const live = payload.db_path;
            if (!live) {
                emitBootWarning(bin, "MPM_REQUIRED_DB_PATH is set but health_check did not surface db_path. The mpm server is too old to be gated.");
                throw new Error("mpm-opencode: db_path invariant — health_check missing db_path");
            }
            if (live !== requiredDbPath) {
                emitBootWarning(bin, `refusing to boot — DB path invariant violated.\n` +
                    `  expected: ${requiredDbPath}\n` +
                    `  actual:   ${live}\n` +
                    `This usually means two mpm installs on the same host, or a stale scratch db.\n` +
                    `Run \`mpm status\` to see which workspace is current.`);
                throw new Error("mpm-opencode: db_path invariant violated");
            }
            console.warn(`mpm-opencode: db_path invariant satisfied (${live})`);
        }

        // ----- Tools: 13 Fat-RPC domain tools + 3 standalones ----------------
        await ctx.tool.transform((editor) => {
            for (const spec of DOMAIN_TOOLS) {
                editor.add({
                    name: spec.name,
                    description: spec.description,
                    input: DOMAIN_INPUT_SCHEMA,
                    async execute(input, toolCtx) {
                        const args = (input ?? {}) as DomainToolInput;
                        const { action, params: body } = args;
                        const r = await callMpmWithProvenance(bin, spec.name, { action, params: body ?? {} }, { sessionID: toolCtx?.sessionID });
                        if (!r.success) {
                            return { content: formatFailure(spec.name, r) };
                        }
                        return { content: jsonToText(r.payload) };
                    },
                });
            }
            for (const spec of STANDALONE_TOOLS) {
                editor.add({
                    name: spec.name,
                    description: spec.description,
                    input: spec.input,
                    async execute(input, toolCtx) {
                        const args = (input ?? {}) as MpmPayload;
                        const r = await callMpmWithProvenance(bin, spec.name, args, { sessionID: toolCtx?.sessionID, timeoutMs: spec.name === "mpm_request_review" ? 120_000 : undefined });
                        if (!r.success) {
                            return { content: formatFailure(spec.name, r) };
                        }
                        return { content: jsonToText(r.payload) };
                    },
                });
            }
        });

        // ----- Hook: wake context injection ---------------------------------
        // V1: experimental.chat.system.transform. Injects MPM wake context
        // into the system prompt once per session (cached by sessionID).
        await ctx.session.hook("context", async (event) => {
            if (!event.sessionID)
                return;
            if (wakeContextCache.has(event.sessionID))
                return;
            const context = await fetchWakeContext(bin);
            if (context) {
                wakeContextCache.set(event.sessionID, context);
                event.system.push({ type: "text", text: context });
            }
        });

        // ----- Hook: shell provenance ---------------------------------------
        // V1: shell.env. The V2 ShellCreateBefore event carries no sessionID,
        // so we stamp the framework + invocation + MPM_WORKSPACE pin; the
        // session-id stamping happens on programmatic tool calls.
        await ctx.shell.hook("create.before", (event) => {
            const provenance = buildProvenanceEnv(undefined, undefined);
            for (const [key, value] of Object.entries(provenance)) {
                event.env[key] = value;
            }
        });

        return () => {
            // cleanup: drop per-session wake cache (session store is process-local)
            wakeContextCache.clear();
        };
    },
});
