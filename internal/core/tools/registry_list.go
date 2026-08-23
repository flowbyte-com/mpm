package tools

import "encoding/json"

// Registry is the single authoritative list of every MPM tool.
// Both `mpm call <name>` (CLI) and the MCP server iterate this slice.
//
// Adding a new tool:
//  1. Write the handler in handlers.go (signature: HandlerFunc).
//  2. Append a Tool entry below with name, description, schema, handler.
//  3. Done. Both surfaces pick it up automatically.
//
// Schema field is a raw JSON-Schema snippet. Both the CLI (for
// future `--help` introspection) and the MCP server (via
// NewToolWithRawSchema) consume it. Hand-writing JSON-Schema is
// verbose but explicit and survives the absence of a schema-builder
// dependency. See schema_test.go for a coverage check that every
// Registry entry has a non-empty schema.
//
// The Registry is intentionally a slice, not a map. Linear scan is
// fine for ~50 tools; the explicit ordering makes this file readable
// as a directory of capabilities.
var Registry = []Tool{
	{
		Name: "mpm_memory",
		Description: `Memory CRUD and lifecycle. Actions:
  save — Persist a fact. Required: params.fact (string). Optional: params.collection, params.tags, params.weight, params.ttl, params._confidence_band, params._reasoning_depth.
  query — Search memories (FTS5 + semantic). Required: params.query (string). Optional: params.limit, params.collection, params.scope ("all"|"local"|"shared").
  shred — Soft-delete a memory or hard-delete a lesson. Required: params.memory_id.
  reinforce — Bump weight + reinforcement_count. Required: params.memory_id. Optional: params.delta (int, default 1).
  weaken — Reduce weight + reinforcement_count. Required: params.memory_id. Optional: params.delta (int, default 1).
  snooze — Suppress from retrieval for N days. Required: params.memory_id. Optional: params.days (int, default 1).
  set_weight — Set explicit weight (0-100). Required: params.memory_id, params.weight.
  patch — JSON-Patch metadata (RFC 6902). Required: params.memory_id, params.patch (array of ops).
  promote — Mark as long-term. Required: params.memory_id.
  review — List memories due for spaced-repetition. Optional: params.days (default 30), params.limit (default 20).
  synthesize — LLM dedup/merge. Required: params.memory_id.
  challenge — Weaken + create theory from contradiction. Required: params.memory_id, params.evidence.
  commit_milestone — Narrative milestone (save wrapper). Required: params.summary (≥50 chars). Optional: params.flavor ("shipped"|"insight"), params.tags.`,
		Schema: json.RawMessage(`{"type":"object","properties":{"action":{"type":"string","enum":["save","query","shred","reinforce","weaken","snooze","set_weight","patch","promote","review","synthesize","challenge","commit_milestone"]},"params":{"type":"object","additionalProperties":true}},"required":["action"]}`),
		Handler: handleMpmMemory,
	},
	{
		Name: "mpm_theories",
		Description: `Theory lifecycle. Actions:
  propose — Create a pending theory. Required: params.hypothesis (string). Optional: params.validation_criteria, params.tags, params.dependencies (array of artifact IDs), params.source_ids (array of citation IDs).
  resolve — Resolve a pending theory. Required: params.theory_id, params.conclusion ("confirmed"|"disproven"), params.new_status ("proven"|"disproven"). Optional: params.winner_id (arbitration path).`,
		Schema: json.RawMessage(`{"type":"object","properties":{"action":{"type":"string","enum":["propose","resolve"]},"params":{"type":"object","additionalProperties":true}},"required":["action"]}`),
		Handler: handleMpmTheories,
	},
	{
		Name: "mpm_decisions",
		Description: `Decision recording. Actions:
  record — Record an architectural decision. Required: params.context, params.choice, params.rationale. Optional: params.outcome, params.tags, params.source_ids.`,
		Schema: json.RawMessage(`{"type":"object","properties":{"action":{"type":"string","enum":["record"]},"params":{"type":"object","additionalProperties":true}},"required":["action"]}`),
		Handler: handleMpmDecisions,
	},
	{
		Name: "mpm_lessons",
		Description: `Lesson lifecycle. Actions:
  save — Persist a lesson learned. Required: params.fact (string). Optional: params.type ("warning"|"practice"|"insight"), params.tags, params.source_ids.
  search — Search lessons by FTS5. Required: params.query (string).
  list — List lessons. Optional: params.type ("warning"|"practice"|"insight").`,
		Schema: json.RawMessage(`{"type":"object","properties":{"action":{"type":"string","enum":["save","search","list"]},"params":{"type":"object","additionalProperties":true}},"required":["action"]}`),
		Handler: handleMpmLessons,
	},
	{
		Name: "mpm_topics",
		Description: `Topic clustering. Actions:
  create — Create a topic. Required: params.name (string). Optional: params.description.
  search — Search topics. Required: params.query (string). Optional: params.limit (default 20).
  link — Link a memory to a topic. Required: params.memory_id, params.topic_id. Optional: params.relevance (float 0-1).`,
		Schema: json.RawMessage(`{"type":"object","properties":{"action":{"type":"string","enum":["create","search","link"]},"params":{"type":"object","additionalProperties":true}},"required":["action"]}`),
		Handler: handleMpmTopics,
	},
	{
		Name: "mpm_references",
		Description: `Reference document management. Actions:
  add — Ingest a reference file. Required: params.filepath (string). Optional: params.title.
  search — Search references by content. Required: params.query (string). Optional: params.limit.
  list — List ingested references. Optional: params.limit (default 50), params.offset (default 0).`,
		Schema: json.RawMessage(`{"type":"object","properties":{"action":{"type":"string","enum":["add","search","list"]},"params":{"type":"object","additionalProperties":true}},"required":["action"]}`),
		Handler: handleMpmReferences,
	},
	{
		Name: "mpm_evidence",
		Description: `Evidence management. Actions:
  add — Attach evidence to an artifact. Required: params.artifact_id, params.type ("observation"|"test"|"reproduction"|"challenge"|"decision_outcome"|"external_reference"), params.source_group, params.created_by. Optional: params.artifact_type ("memory"|"theory"|"decision"|"lesson"), params.notes, params.strength (-1 to 1), params.independence_factor.
  list — List evidence for an artifact. Required: params.artifact_id. Optional: params.artifact_type.`,
		Schema: json.RawMessage(`{"type":"object","properties":{"action":{"type":"string","enum":["add","list"]},"params":{"type":"object","additionalProperties":true}},"required":["action"]}`),
		Handler: handleMpmEvidence,
	},
	{
		Name: "mpm_confidence",
		Description: `Confidence inspection and audit. Actions:
  show — Current confidence of an artifact. Required: params.artifact_id. Optional: params.artifact_type.
  recompute — Force recompute from evidence. Required: params.artifact_id. Optional: params.artifact_type.
  explain — Factor breakdown + history trace. Required: params.artifact_id. Optional: params.artifact_type.
  history — Confidence timeline. Required: params.artifact_id. Optional: params.artifact_type, params.limit (default 50).
  changes — Change events (delta + trigger). Optional: params.artifact_id, params.artifact_type, params.since, params.since_seconds_ago, params.limit.
  trend — Linear fit over window. Required: params.artifact_id. Optional: params.artifact_type, params.window_days (default 30).
  quality — Per-source quality stats. No params required.`,
		Schema: json.RawMessage(`{"type":"object","properties":{"action":{"type":"string","enum":["show","recompute","explain","history","changes","trend","quality"]},"params":{"type":"object","additionalProperties":true}},"required":["action"]}`),
		Handler: handleMpmConfidence,
	},
	{
		Name:        "explain_retrieval",
		Description: "Run a standard FTS search and return a per-node diagnostic breakdown: Base FTS Match score, Reuse Count (how often the node has been surfaced into agent working memory), Last Retrieved timestamp, and Success Count. The retrieval ordering is identical to query_long_term_memory — the FTS bm25() rank is preserved bit-for-bit. This tool layers observability on top, it does NOT alter ranking. Use this when you want to understand WHY a result ranked where it did, or how often it has been consumed before.",
		Schema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"query":      {"type": "string", "description": "The FTS query string (same contract as query_long_term_memory)."},
				"limit":      {"type": "number", "description": "Max results to diagnose (default 10)."},
				"collection": {"type": "string", "description": "Optional collection filter (memories, lessons, decisions, theories, skills)."},
				"scope":      {"type": "string", "enum": ["all", "local", "shared"], "default": "all"},
				"trace":      {"type": "boolean", "default": false, "description": "When true, returns a 3-stage pipeline diagnostic (raw query → BuildFTS5Query → FTS5 row count + BM25 distribution → HybridSearch input/output + discarded-row analysis). Permanent observability surface for investigating retrieval failures — zero-result queries, short-token drops, hyphen crashes. Cost: one extra FTS5 round-trip per call."}
			},
			"required": ["query"]
		}`),
		Handler: handleExplainRetrieval,
	},
	{
		Name: "mpm_context",
		Description: `Agent state and routing. Actions:
  read_wake_context — Read session state, active mode, recent memories. No params.
  read_directives — Read active behavioral directives. No params.
  proactive_recall_hint — Surface conversation-relevant memories. Required: params.conversation_text (string). Optional: params.max_hints (default 3), params.min_score (default -3).
  query_global_rules — Query shared rules. Optional: params.query, params.limit.
  record_global_rule — Write a shared rule. Required: params.fact, params.confirm (true). Optional: params.tags, params.weight, params.provenance.
  promote_to_global — Copy local memory to shared DB. Required: params.memory_id, params.confirm (true).
  route — Evaluate mode/persona routing. Required: params.prompt.`,
		Schema: json.RawMessage(`{"type":"object","properties":{"action":{"type":"string","enum":["read_wake_context","read_directives","proactive_recall_hint","query_global_rules","record_global_rule","promote_to_global","route"]},"params":{"type":"object","additionalProperties":true}},"required":["action"]}`),
		Handler: handleMpmContext,
	},
	{
		Name: "mpm_skills",
		Description: `Skill management. Actions:
  save — Persist a skill (markdown + YAML frontmatter). Required: params.name, params.version, params.content. Optional: params.author, params.force.
  read — Fetch skill by name. Required: params.name. Optional: params.version.
  list — List available skills. Optional: params.scope ("local"|"shared"|"all").
  delete — Soft-delete a skill. Required: params.skill_id.
  promote_to_global — Share a skill globally. Required: params.skill_id, params.confirm (true).`,
		Schema: json.RawMessage(`{"type":"object","properties":{"action":{"type":"string","enum":["save","read","list","delete","promote_to_global"]},"params":{"type":"object","additionalProperties":true}},"required":["action"]}`),
		Handler: handleMpmSkills,
	},
	{
		Name: "mpm_wakes",
		Description: `Wake scheduling and inspection. Actions:
  schedule — Schedule a future wake. Required: params.reason, params.target_time (epoch/duration/ISO-8601). Optional: params.theory_id, params.recurring_rule, params.metadata.
  check — Pull due wakes and mark fired. Optional: params.kinds (array; default notification-only; ["*"] for all).
  check_pending_event — Pull event wakes for this session. Optional: params.session_id.
  list — List scheduled wakes. Optional: params.include_fired, params.overdue_only, params.limit.
  digest — Compact overdue wake summary. Optional: params.top_n (default 5).
  upsert_task — Create/update a cron task. Required: params.id, params.name, params.cron_expr, params.directive_id, params.status ("active"|"paused").
  list_tasks — List all scheduled tasks. No params.
  delete_task — Hard-delete a task. Required: params.id.`,
		Schema: json.RawMessage(`{"type":"object","properties":{"action":{"type":"string","enum":["schedule","check","check_pending_event","list","digest","upsert_task","list_tasks","delete_task"]},"params":{"type":"object","additionalProperties":true}},"required":["action"]}`),
		Handler: handleMpmWakes,
	},
	{
		Name: "mpm_session",
		Description: `Session lifecycle and scratchpad. Actions:
  end — Write a handoff for next wake. Required: params.session_id, params.summary. Optional: params.state, params.commitments, params.open_questions.
  handoff — Read latest handoff. Optional: params.unread, params.mark_read.
  list_handoffs — List handoff history. Optional: params.limit (default 10), params.unread.
  flush — Overwrite the ephemeral scratchpad. Required: params.session_id, params.thesis. Optional: params.supporting.
  read — Read scratchpad for a session. Required: params.session_id.
  discard — Delete scratchpad without promoting. Required: params.session_id.
  promote_scratchpad — Promote scratchpad to memory, then delete. Required: params.session_id.`,
		Schema: json.RawMessage(`{"type":"object","properties":{"action":{"type":"string","enum":["end","handoff","list_handoffs","flush","read","discard","promote_scratchpad"]},"params":{"type":"object","additionalProperties":true}},"required":["action"]}`),
		Handler: handleMpmSession,
	},
	{
		Name: "mpm_system",
		Description: `Maintenance, audit, and diagnostics. Actions:
  gc_run — Lifecycle decay sweep. Optional: params.dry_run (default true), params.aggressive, params.max_age_hours (default 24).
  compact — Compact raw memories into a lesson. Optional: params.force.
  health_check — SQLite integrity + domain counts. No params.
  migrate — Import from markdown/JSON. Required: params.from_path (for stage). Optional: params.format, params.label, params.dry_run, params.commit, params.commit_batch, params.undo_batch.
  query_audit_log — Query anomaly ledger. Optional: params.level, params.component, params.days (default 7), params.limit (default 20).
  list_clusters — List active audit clusters. No params.
  snooze_cluster — Temporarily hide a cluster. Required: params.cluster_key, params.snooze_until. Optional: params.reason.
  resolve_cluster — Permanently dismiss a cluster. Required: params.cluster_key. Optional: params.reason.
  annotate_cluster — Append forensic annotation. Required: params.cluster_key, params.annotation. Optional: params.reason.`,
		Schema: json.RawMessage(`{"type":"object","properties":{"action":{"type":"string","enum":["gc_run","compact","health_check","migrate","query_audit_log","list_clusters","snooze_cluster","resolve_cluster","annotate_cluster"]},"params":{"type":"object","additionalProperties":true}},"required":["action"]}`),
		Handler: handleMpmSystem,
	},
	{
		Name:        "log_to_changelog",
		Description: "Self-report agent work as a changelog entry tied to a git commit SHA.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"fact":{"type":"string"},"commit_hash":{"type":"string"},"tags":{"oneOf":[{"type":"string"},{"type":"array","items":{"type":"string"}}],"description":"Tags as a comma-separated string OR a JSON array of strings."}},"required":["fact","commit_hash"]}`),
		Handler:     handleLogToChangelog,
	},
	{
		Name:        "request_review",
		Description: "Concurrent multi-component review. Fetch artifact bodies from memory ids in 'artifacts' (optional list of ids; resolved text is passed to each component). Send the same prompt + artifact to every component named in 'components' (e.g. ['memory','critic','scheduler']). Each component resolves to a profile via the execution-profile abstraction; per-component concurrent fan-out, per-request optional timeout. Strategy must be 'parallel' (v0.1). Returns rendered Markdown with one section per component — full text response per component, or an error block. Independent results: one component's failure does not abort the others. Non-goals (v0.1): no consensus synthesis, no review persistence, no retry, no streaming.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"components":{"type":"array","items":{"type":"string"},"description":"Substrate component names to review (e.g. ['memory','critic']). Required, at least one."},"prompt":{"type":"string","description":"The instruction sent to every component. Required."},"artifacts":{"type":"array","items":{"type":"string"},"description":"Optional memory ids. Bodies are fetched from the database and passed to every component as pre-resolved text (NOT ids — the coordinator never sees ids)."},"strategy":{"type":"string","enum":["parallel"],"default":"parallel","description":"Dispatch strategy. v0.1 only supports 'parallel'."},"timeout_secs":{"type":"number","description":"Optional total timeout in seconds for the orchestration. If zero or omitted, the caller's context governs."}},"required":["components","prompt"]}`),
		Handler:     handleRequestReview,
	},

	// ── Phase 1: Pointer / Blob tools ──────────────────────────────────────

	{
		Name:        "mpm_resolve",
		Description: "Resolve a mpm:// URI to its content. Phase 1 supports mpm://blob/<id> only.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"uri":{"type":"string"},"max_bytes":{"type":"integer","default":0}},"required":["uri"]}`),
		Handler:     handleMpmResolve,
	},
	{
		Name:        "mpm_blob_read",
		Description: "Read a blob with byte offset and server-side max_bytes cap.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"},"offset":{"type":"integer","default":0},"max_bytes":{"type":"integer","default":51200}},"required":["id"]}`),
		Handler:     handleMpmBlobRead,
	},
	{
		Name:        "mpm_blob_search",
		Description: "Server-side regex search within a blob.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"},"query":{"type":"string","maxLength":256},"regex":{"type":"boolean","default":false},"case_insensitive":{"type":"boolean","default":false},"max_matches":{"type":"integer","default":20},"max_bytes":{"type":"integer","default":51200}},"required":["id","query"]}`),
		Handler:     handleMpmBlobSearch,
	},

	// ── Work primitive ────────────────────────────────────────────────

	{
		Name: "mpm_work",
		Description: `Work lifecycle. Actions:
  create — Create a work item. Required: params.title (string). Optional: params.content, params.session_id.
  list — List open work items ordered by created_at DESC. No params required.
  show — Get a work item by ID. Required: params.work_id.
  update — Update a work item's status (deprecated; use title/content params instead). Required: params.work_id, params.status (open|done|cancelled). Prefer: params.title or params.content for explicit events.
  complete — Mark a work item as done. Required: params.work_id. Optional: params.note.
  cancel — Mark a work item as cancelled. Required: params.work_id. Optional: params.note.
  history — Get full event history for a work item. Required: params.work_id.
  note — Append a contextual note to a work item's history. Required: params.work_id, params.note.
  reopen — Re-open a completed or cancelled work item. Required: params.work_id.`,
		Schema: json.RawMessage(`{"type":"object","properties":{"action":{"type":"string","enum":["create","list","show","update","complete","cancel","history","note","reopen"]},"params":{"type":"object","properties":{"title":{"type":"string"},"content":{"type":"string"},"session_id":{"type":"string"},"work_id":{"type":"string"},"status":{"type":"string","enum":["open","done","cancelled"]},"note":{"type":"string"}},"additionalProperties":true}},"required":["action"]}`),
		Handler: handleMpmWork,
	},
}

// ByName returns the tool with the given name, or false.
// O(n) scan is fine for ~50 tools; the alternative is a name→index
// map built at init() if the registry grows past a few hundred.
func ByName(name string) (Tool, bool) {
	for _, t := range Registry {
		if t.Name == name {
			return t, true
		}
	}
	return Tool{}, false
}

// Names returns every registered tool name in registry order.
// Used by `mpm call --help` and the MCP ListTools handler.
func Names() []string {
	out := make([]string, 0, len(Registry))
	for _, t := range Registry {
		out = append(out, t.Name)
	}
	return out
}