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
		Description: `Persistent memory for facts, learnings, and context the agent needs to carry across sessions.
Use when: you learn something worth remembering (a fact, a lesson, a decision context); you need to find something you previously stored; or you want to mark something as long-term and suppress it from casual retrieval; you want to commit a milestone against a long-term goal (commit_milestone).
Do not use when: the information is ephemeral working context (use mpm_scratchpad instead); you are making a commitment or tracking work (use mpm_work instead).
For broad queries, projection defaults to 'summary' to keep context bounded. Use projection='full' or mpm_resolve ONLY when reading the complete unabridged content of a specific pointer.
Lifecycle asymmetry: shred is permanent (hard delete — the row is removed with cascade cleanup of dependent topic_memberships, memory_revisions, and confidence_history). There is no restore path. This is deliberately different from mpm_skills.delete, which is soft and recoverable via save with force=true.
Lifecycle asymmetry: delete is soft (the row stays with deleted_at stamped; action=restore brings it back). Reversible via action=restore.
Lifecycle asymmetry: weaken uses an internal floor-protected path (the weight cannot drop below the safety floor of 1). reinforce and weaken accept the same delta shape but their internal mechanics differ; the user-visible contract is symmetric.`,

		Schema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"action": {"type": "string", "enum": ["save","query","show","shred","delete","restore","reinforce","weaken","snooze","set_weight","patch","promote","review","synthesize","challenge","restore_challenge","commit_milestone"]},
				"params": {
					"type": "object",
					"properties": {
						"fact":             {"type": "string"},
						"collection":       {"type": "string"},
						"tags":            {"type": "array", "items": {"type": "string"}},
						"weight":          {"type": "number"},
						"ttl":             {"type": "string"},
						"query":           {"type": "string"},
						"limit":           {"type": "number"},
						"scope":           {"type": "string", "enum": ["all","local","shared"]},
						"projection":      {"type": "string", "enum": ["summary", "full"], "default": "summary", "description": "Output detail level. Defaults to summary."},
						"memory_id":       {"type": "string", "description": "Canonical id field (snake_case). The legacy camelCase alias 'memoryId' is still accepted at runtime for backward compat but is no longer advertised as a valid schema field."},
						"delta":           {"type": "number"},
						"days":            {"type": "number"},
						"patch":           {"type": "object"},
						"evidence":         {"type": "string"},
						"summary":         {"type": "string"},
						"flavor":          {"type": "string", "enum": ["shipped","insight"]},
						"_confidence_band": {"type": "string"},
						"_reasoning_depth": {"type": "string"}
					},
					"additionalProperties": true
				}
			},
			"required": ["action"]
		}`),
		Handler: handleMpmMemory,
	},
	{
		Name: "mpm_theories",
		Description: `Hypothesis management with explicit validation criteria and resolution.
Use when: you form a hypothesis about causality ("I think the FTS tokenizer is producing different results on this OS") and can define a concrete test that would confirm or disprove it. Theories are not guesses — they are testable claims with defined success conditions.
Do not use when: you just want to remember something (mpm_memory); you have a confirmed decision (mpm_decisions).`,
		Schema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"action": {"type": "string", "enum": ["propose","resolve","show","list","query"]},
				"params": {
					"type": "object",
					"oneOf": [
						{
							"properties": {
								"hypothesis":        {"type": "string"},
								"validation_criteria": {"type": "string"},
								"tags":             {"type": "array", "items": {"type": "string"}},
								"dependencies":     {"type": "array", "items": {"type": "string"}},
								"source_ids":       {"type": "array", "items": {"type": "string"}}
							},
							"required": ["hypothesis"]
						},
						{
							"properties": {
								"theoryId":   {"type": "string"},
								"conclusion": {"type": "string"},
								"winnerId":  {"type": "string"},
								"newStatus": {"type": "string", "enum": ["proven","disproven"]}
							},
							"required": ["theoryId", "conclusion"]
						}
					],
					"additionalProperties": true
				}
			},
			"required": ["action"]
		}`),
		Handler: handleMpmTheories,
	},
	{
		Name: "mpm_decisions",
		Description: `Immutable record of architectural choices and the reasoning behind them.
Use when: you make a choice between approaches ("we chose SQLite WAL mode over DELETE journal for concurrent access") and want to preserve the rationale so future-you understands why, even when the alternative is no longer fresh in context.
Do not use when: the choice is trivial or easily reversible; you just want to store a fact (mpm_memory).`,
		Schema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"action": {"type": "string", "enum": ["record","supersede","invalidate","show","list","query"]},
				"params": {
					"type": "object",
					"properties": {
						"choice":    {"type": "string"},
						"context":   {"type": "string"},
						"rationale": {"type": "string"},
						"outcome":   {"type": "string"},
						"tags":     {"type": "array", "items": {"type": "string"}},
						"source_ids": {"type": "array", "items": {"type": "string"}}
					},
					"required": ["choice"],
					"additionalProperties": true
				}
			},
			"required": ["action"]
		}`),
		Handler: handleMpmDecisions,
	},
	{
		Name: "mpm_lessons",
		Description: `Durable lessons from failures, anti-patterns, and hard-won insights.
Use when: something failed and you want to make sure the system never repeats the same mistake; you encounter an unexpected success and want to record why it worked; you want to tag a memory as a "warning" or "practice" so it surfaces in future relevant contexts.
Do not use when: you are documenting a decision (mpm_decisions) or forming a testable hypothesis (mpm_theories).
For broad queries, projection defaults to 'summary' to keep context bounded. Use projection='full' or mpm_resolve ONLY when reading the complete unabridged content of a specific pointer.`,
		Schema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"action": {"type": "string", "enum": ["save","search","list"]},
				"params": {
					"type": "object",
					"properties": {
						"fact":       {"type": "string"},
						"type":      {"type": "string", "enum": ["insight","warning","practice"]},
						"tags":      {"type": "array", "items": {"type": "string"}},
						"query":     {"type": "string"},
						"projection": {"type": "string", "enum": ["summary", "full"], "default": "summary", "description": "Output detail level. Defaults to summary."}
					},
					"additionalProperties": true
				}
			},
			"required": ["action"]
		}`),
		Handler: handleMpmLessons,
	},
	{
		Name: "mpm_topics",
		Description: `Topic labels for clustering related memories and organizing knowledge.
Use when: you want to link multiple memories under a shared theme ("UN-system", "Qatar-Mission", "eCryptfs-boot-order") so future queries can surface the full cluster with a single term.
Do not use when: you just want to store a single fact (mpm_memory save); you need to track work (mpm_work).`,
		Schema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"action": {"type": "string", "enum": ["create","search","link","unlink","list","show"]},
				"params": {
					"type": "object",
					"properties": {
						"name":        {"type": "string"},
						"description": {"type": "string"},
						"query":      {"type": "string"},
						"limit":     {"type": "number"},
						"memory_id": {"type": "string"},
						"topic_id":  {"type": "string"}
					},
					"additionalProperties": true
				}
			},
			"required": ["action"]
		}`),
		Handler: handleMpmTopics,
	},
	{
		Name: "mpm_references",
		Description: `Ingested external documents: PDFs, specs, whitepapers, books.
Use when: you read an external document and want to make its contents searchable via the MPM query surface. References are indexed and queryable; use read to retrieve a single doc by id, search to query chunks, or list to enumerate.
Do not use when: the document is ephemeral or you just want to save a URL to visit later (mpm_memory save).`,
		Schema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"action": {"type": "string", "enum": ["add","read","search","list"]},
				"params": {
					"type": "object",
					"properties": {
						"filepath": {"type": "string"},
						"title":   {"type": "string"},
						"query":   {"type": "string"},
						"id":      {"type": "string", "description": "Reference doc id; required for the read action."},
						"limit":   {"type": "number"},
						"offset":  {"type": "number"}
					},
					"additionalProperties": true
				}
			},
			"required": ["action"]
		}`),
		Handler: handleMpmReferences,
	},
	{
		Name: "mpm_evidence",
		Description: `Attach observations, test results, or external references as evidence to any artifact.
Use when: you want to substantiate a memory, theory, or decision with a concrete observation ("tested on 3 machines, same result"), a test output, a reproduction case, or an external source.
Do not use when: you are storing raw facts without evidentiary context (mpm_memory save). Evidence gives artifacts weight — use it when the truth of something is important enough to require proof.`,
		Schema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"action": {"type": "string", "enum": ["add","list","source_groups"]},
				"params": {
					"type": "object",
					"properties": {
						"artifact_id":         {"type": "string"},
						"artifact_type":        {"type": "string", "enum": ["memory","theory","decision","lesson","skill","work"]},
						"type":               {"type": "string"},
						"source_group":        {"type": "string", "enum": ["filesystem","test","api_response","manual_review","git","ci","external","tool_invocation","api_call","process"],
							"description": "outcome=filesystem,test,api_response,manual_review; audit=git,ci,external; action=tool_invocation,api_call,process. Unknown values are rejected (F6)."},
						"strength":           {"type": "number"},
						"independence_factor": {"type": "number"},
						"created_by":         {"type": "string"},
						"notes":              {"type": "string"}
					},
					"additionalProperties": true
				}
			},
			"required": ["action"]
		}`),
		Handler: handleMpmEvidence,
	},
	{
		Name: "mpm_confidence",
		Description: `Inspect and reason about the system's certainty regarding any stored artifact.
Use when: you need to understand how reliable a piece of information is before acting on it; you want to see the evidence weight behind a memory, theory, or decision; you suspect memory degradation and want to audit the system's confidence signal.
Do not use when: you just want to store a fact (mpm_memory save).`,
		Schema: json.RawMessage(`{"type":"object","properties":{"action":{"type":"string","enum":["show","recompute","explain","history","changes","trend","quality"]},"params":{"type":"object","additionalProperties":true}},"required":["action"]}`),
		Handler: handleMpmConfidence,
	},
	{
		Name:        "mpm_retrieval_diagnose",
		Description: `Diagnostic for retrieval pipeline failures. Run this when your query returns unexpected results, zero results, or results in the wrong order.
Use when: you search for something you know is in memory but it doesn't appear; you want to understand why a particular result ranked where it did; you are debugging FTS5 behavior or retrieval ranking.
Returns per-node diagnostics: BM25 score, reuse count, last-retrieved timestamp, and success count for each node in the result set. Use trace=true for the full 3-stage pipeline breakdown.`,
		Schema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"query":      {"type": "string", "description": "The FTS query string (same contract as mpm_memory query)."},
				"limit":      {"type": "number", "description": "Max results to diagnose (default 10)."},
				"collection": {"type": "string", "description": "Optional collection filter (memories, lessons, decisions, theories, skills)."},
				"scope":      {"type": "string", "enum": ["all", "local", "shared"], "default": "all"},
				"trace":      {"type": "boolean", "default": false, "description": "When true, returns a 3-stage pipeline diagnostic. Cost: one extra FTS5 round-trip per call."}
			},
			"required": ["query"]
		}`),
		Handler: handleMpmRetrievalDiagnose,
	},
	{
		Name: "mpm_context",
		Description: `Agent session state, mode routing, and directive management. The action=read_wake_context is the canonical wake-up hook (call on session start); the wake payload carries a bounded <available_skills> catalogue (a subset of mpm_skills list{scope:"all"}) and supports projection="compact" for a small id+summary envelope (the full WakeContextData is the default).
Use when: you need to wake up at session start; understand the current agent mode/persona; trigger a mode or persona switch; read active behavioral directives governing the current session; query proactive recall for conversation-relevant memories; record / query / retire global house rules that every agent on the workstation should see; promote a local memory to the shared substrate.
Route is especially useful: give it a user prompt and it returns the best-matching mode(s) and persona with scoring.
Lifecycle asymmetry: promote_to_global is one-way / additive. The local memory row stays; the shared copy is created alongside (per Phase 3 of the shared-epistemology design). There is no demote operation. To "remove" a shared rule, use retire_global_rule — it stamps retired_at and the row is filtered from default queries (recoverable via query with include_retired=true).`,
		Schema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"action": {"type": "string", "enum": ["read_wake_context","read_directives","proactive_recall_hint","query_global_rules","record_global_rule","retire_global_rule","promote_to_global","route"]},
				"params": {
					"type": "object",
					"properties": {
						"projection":          {"type": "string", "enum": ["compact"], "description": "Wake-context projection. 'compact' returns a small id+summary envelope; other values fall through to the full WakeContextData branch. Other canonical projection values (summary, full) belong to mpm_memory query, not mpm_context."},
						"format":              {"type": "string", "enum": ["system-prompt"]},
						"conversation_text":   {"type": "string"},
						"max_hints":          {"type": "number"},
						"min_score":          {"type": "number"},
						"query":              {"type": "string"},
						"limit":             {"type": "number"},
						"fact":              {"type": "string"},
						"confirm":           {"type": "boolean"},
						"tags":              {"type": "string"},
						"weight":            {"type": "number"},
						"provenance":        {"type": "string"},
						"memory_id":         {"type": "string"},
						"rule_id":           {"type": "string"},
						"reason":            {"type": "string"},
						"include_retired":   {"type": "boolean"},
						"prompt":            {"type": "string"}
					},
					"additionalProperties": true
				}
			},
			"required": ["action"]
		}`),
		Handler: handleMpmContext,
	},
	{
		Name: "mpm_skills",
		Description: `Reusable procedural knowledge stored as markdown with YAML frontmatter.
Use when: you develop a workflow that works well and want to固化 it as a persistent skill that can be listed, read by name, and reused across sessions without re-inventing the procedure. The save action requires content (the skill markdown body) and name. Skills are versioned and can be shared globally or kept local to this workstation.
Lifecycle asymmetry: delete is soft (the row stays with deleted_at stamped; save with force=true resurrects the tombstoned row). This is deliberately different from mpm_memory.shred, which is permanent and irrecoverable.
Lifecycle asymmetry: promote_to_global is one-way / additive. The local row is preserved (Phase 3 of shared-epistemology design); there is no demote operation. To retire a globally-promoted skill, shred the local copy if you want it gone from the workstation, or rely on the shared substrate's own lifecycle.`,
		Schema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"action": {"type": "string", "enum": ["save","read","list","delete","promote_to_global","workshop"]},
				"params": {
					"type": "object",
					"properties": {
						"name":    {"type": "string"},
						"version": {"type": "string"},
						"content": {"type": "string"},
						"author": {"type": "string"},
						"force":  {"type": "boolean"},
						"scope":  {"type": "string", "enum": ["all","local","shared"]},
						"skill_id": {"type": "string"},
						"confirm":   {"type": "boolean"},
						"mode":             {"type": "string", "enum": ["form","refine"]},
						"intent":           {"type": "string"},
						"change_type":      {"type": "string", "enum": ["correction","extension","restructuring","purpose_change"]},
						"decision_model":   {"type": "object"},
						"proposal":         {"type": "object"},
						"task_context":     {"type": "string"},
						"workflow_description": {"type": "string"},
						"failure_recovery": {"type": "string"},
						"recent_actions":   {"type": "array", "items": {"type": "string"}},
						"evidence":         {"type": "object"},
						"workshop_key":     {"type": "string"}
					},
					"additionalProperties": true
				}
			},
			"required": ["action"]
		}`),
		Handler: handleMpmSkills,
	},
	{
		Name: "mpm_wakes",
		Description: `Deferred work triggers scheduled for future execution.
Use when: you need to schedule a check-in, reminder, or follow-up task to fire automatically at a specific time without the agent running continuously. Wakes survive agent restarts — the scheduler fires them regardless of what session is active.
Tasks (upsert_task) are recurring cron-style triggers; one-shot wakes (schedule) fire once and are marked fired.
Lifecycle asymmetry: delete_task is permanent removal of the task row. For reversibility / preserving history, prefer upsert_task with status='paused' (the row stays, the scheduler skips it, you can flip back to 'active' later without losing state).`,
		Schema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"action": {"type": "string", "enum": ["schedule","check","check_pending_event","list","digest","upsert_task","list_tasks","delete_task"]},
				"params": {
					"type": "object",
					"properties": {
						"reason":         {"type": "string"},
						"target_time":   {"type": "string"},
						"theory_id":     {"type": "string"},
						"recurring_rule": {"type": "string"},
						"metadata":      {"type": "object"},
						"kinds":        {"type": "array", "items": {"type": "string"}},
						"session_id":    {"type": "string"},
						"include_fired":  {"type": "boolean"},
						"overdue_only":   {"type": "boolean"},
						"limit":         {"type": "number"},
						"top_n":         {"type": "number"},
						"id":           {"type": "string"},
						"name":         {"type": "string"},
						"cron_expr":    {"type": "string"},
						"directive_id": {"type": "string"},
						"status":      {"type": "string", "enum": ["active","paused"]}
					},
					"additionalProperties": true
				}
			},
			"required": ["action"]
		}`),
		Handler: handleMpmWakes,
	},

	// ── Handoff primitive (inter-session communication) ──────────────────

	{
		Name: "mpm_handoff",
		Description: `Inter-session communication: write, read, and audit handoff records.
Use when: you are ending a session and need to leave a summary for the next session to pick up. The handoff record is the bridge between two distinct agent shifts — it carries the session summary, not the work itself.
CRITICAL: the write action requires a non-empty summary. Mid-session acknowledgements (ok/thanks/ty/ack) are NOT session-closing events — do not write a handoff in response to a chat ack; only do so at genuine session closure.
Optional commitments and open_questions are persisted and round-tripped: open_questions surface in the next session's wake context. Keep them short — the handoff is a bridge, not a work log (use mpm_work for tasks, mpm_theories for testable questions).`,
		Schema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"action": {"type": "string", "enum": ["write","read","list","shred"]},
				"params": {"type": "object", "description": "Action-specific params envelope. Per-action shape is constrained by the oneOf branches below; the top-level declaration here exists so the schema accurately reflects what handleMpmHandoff reads (via extractParamsOrFail)."}
			},
			"required": ["action"],
			"oneOf": [
				{
					"properties": {
						"action": {"const": "write"},
						"params": {
							"type": "object",
							"properties": {
								"summary":       {"type": "string", "minLength": 1},
								"session_id":    {"type": "string"},
								"state":         {"type": "string", "enum": ["clean","crashed","interrupted","force_end"]},
								"commitments":   {"type": "array", "items": {"type": "string"}},
								"open_questions":{"type": "array", "items": {"type": "string"}}
							},
							"required": ["summary"],
							"additionalProperties": false
						}
					},
					"required": ["params"]
				},
				{
					"properties": {
						"action": {"const": "read"},
						"params": {
							"type": "object",
							"properties": {
								"session_id": {"type": "string"},
								"limit":      {"type": "number"}
							},
							"additionalProperties": true
						}
					}
				},
				{
					"properties": {
						"action": {"const": "list"},
						"params": {
							"type": "object",
							"properties": {
								"unread":     {"type": "boolean"},
								"limit":      {"type": "number"}
							},
							"additionalProperties": true
						}
					}
				},
				{
					"properties": {
						"action": {"const": "shred"},
						"params": {
							"type": "object",
							"properties": {
								"handoff_id": {"type": "string"},
								"confirm":    {"type": "boolean"}
							},
							"required": ["handoff_id"],
							"additionalProperties": true
						}
					},
					"required": ["params"]
				}
			]
		}`),
		Handler: handleMpmHandoff,
	},

	// ── Scratchpad primitive (intra-session volatile memory) ─────────────

	{
		Name: "mpm_scratchpad",
		Description: `Intra-session volatile working memory for thoughts, partial conclusions, and working context that may not survive to the next session.
Use when: you are mid-thought on something complex and need to externalize your working state so you can recover if the session crashes; you want to capture a partial result that is not yet ready to be a proper memory; you need to preserve a chain of reasoning that spans multiple turns.
Promote to memory when the thought is complete and worth preserving. Discard when the thread is abandoned. Scratchpads are not cross-session — they are for within-session resilience only.`,
		Schema: json.RawMessage(`{"type":"object","properties":{"action":{"type":"string","enum":["flush","read","discard","promote"]},"params":{"type":"object","properties":{"session_id":{"type":"string"},"thesis":{"type":"string"},"supporting":{"type":"string"}},"additionalProperties":false}},"required":["action"]}`),
		Handler: handleMpmScratchpad,
	},

	// ── System & maintenance ─────────────────────────────────────────────

	{
		Name: "mpm_system",
		Description: `Maintenance, diagnostics, and housekeeping for the MPM substrate.
Use when: you need to run a lifecycle decay sweep (gc_run); compact raw memories into lessons (compact); check SQLite integrity (health_check); audit the anomaly ledger (query_audit_log); manage or dismiss audit clusters; list active audit clusters (list_clusters); surface critic-emitted findings (critic_findings).
This tool is for system health — not for daily agent work. Prefer specific tools for regular operations.

Per-action semantics:

- gc_run: Lifecycle decay sweep. Optional params: dry_run (default true), aggressive, max_age_hours (default 24). gc_run is NOT for epistemic compaction — it decays stale/expired artifacts by age. Use "compact" instead when epistemic_pressure.exceeded is true.

- compact: Drain eligible raw memories into lessons in sequential batches of at most 50 (the LLM context safeguard); each batch is independently synthesized, validated, and committed. force=false (default) RELIEVES pressure — the drain stops as soon as raw_count <= threshold and may leave eligible raw memories remaining. force=true DRAINS everything — the threshold gate is bypassed and the drain continues until the substrate is empty or the per-invocation cap is hit. force does NOT widen the 50-item per-batch limit. Optional params: force (default false), max_batches (default 20, hard cap 100, silently clamped) — per-invocation cap on LLM calls. Result envelope: success (false ONLY on mid-drain failure), batches_processed, raw_processed, lessons_created, raw_remaining, lesson_ids, stop_reason ("no_work" | "completed" | "threshold_reached" | "max_batches_reached" | "failure"), skipped_reason (set on no_work / threshold_reached only), and failed_batch + failure_reason on "failure". Inspect stop_reason (not success) to determine whether the substrate is fully drained.`,
		Schema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"action": {"type": "string", "enum": ["gc_run","compact","health_check","migrate","query_audit_log","list_clusters","snooze_cluster","unsnooze_cluster","resolve_cluster","annotate_cluster","critic_findings"]},
				"params": {"type": "object", "description": "Action-specific params envelope. Per-action shape is constrained by the oneOf branches below; the top-level declaration here exists so the schema accurately reflects what handleMpmSystem reads (via extractParamsOrFail)."}
			},
			"required": ["action"],
			"oneOf": [
				{
					"properties": {
						"action": {"const": "gc_run"},
						"params": {
							"type": "object",
							"properties": {
								"dry_run":           {"type": "boolean", "default": true,  "description": "Lifecycle decay sweep. Optional. Default true (safe default — no destructive work)."},
								"aggressive":        {"type": "boolean", "default": false, "description": "Enable aggressive pruning beyond the safe default. Optional."},
								"max_age_hours":     {"type": "number",  "default": 24,    "description": "Max age (hours) for stale artifacts. Default 24."},
								"stale_theory_days": {"type": "number",  "default": 30,    "description": "Age threshold (days) past which theories are flagged stale. Default 30; values <0 are clamped to 0."}
							},
							"additionalProperties": false
						}
					},
					"required": ["params"]
				},
				{
					"properties": {
						"action": {"const": "compact"},
						"params": {
							"type": "object",
							"properties": {
								"force":       {"type": "boolean", "default": false, "description": "Bypass the pressure threshold gate so the drain processes every eligible row regardless of raw_count vs threshold. force does NOT widen the 50-item per-batch limit."},
								"max_batches": {"type": "number",  "default": 20,    "description": "Per-invocation safety cap on LLM calls. Default 20, hard cap 100, silently clamped."}
							},
							"additionalProperties": false
						}
					},
					"required": ["params"]
				},
				{
					"properties": {
						"action": {"const": "health_check"},
						"params": {"type": "object", "properties": {}, "additionalProperties": false}
					},
					"required": ["params"]
				},
				{
					"properties": {
						"action": {"const": "migrate"},
						"params": {
							"type": "object",
							"properties": {
								"confirm":      {"type": "boolean", "description": "Required explicit acknowledgement that this bulk database write is intentional. Must be the boolean literal true. Migration is not a read operation; it stages rows in raw_memories, promotes them to memories, or rejects a staged batch via UPDATE. The handler refuses any other value (omitted, false, null, string \"true\", numeric 1) so an agent cannot autonomously trigger persistent state changes."},
								"from_path":    {"type": "string",  "description": "Path to a markdown or json file to stage. Required unless commit_batch or undo_batch is supplied."},
								"format":       {"type": "string",  "description": "File format. 'auto' (default) infers from extension; 'markdown' or 'json' explicit."},
								"label":        {"type": "string",  "description": "Short batch label used in the generated batch_id."},
								"dry_run":      {"type": "boolean", "description": "Do not commit or undo; only stage and report stats."},
								"commit":       {"type": "boolean", "description": "After staging, promote the batch to memories."},
								"commit_batch": {"type": "string",  "description": "Promote a previously-staged batch by id. Mutually exclusive with from_path / undo_batch."},
								"undo_batch":   {"type": "string",  "description": "Tombstone a previously-staged batch by id. Mutually exclusive with from_path / commit_batch."}
							},
							"additionalProperties": false
						}
					},
					"required": ["params", "confirm"]
				},
				{
					"properties": {
						"action": {"const": "query_audit_log"},
						"params": {
							"type": "object",
							"properties": {
								"level":         {"type": "string",  "description": "Audit level filter. Lowercase canonical values: debug/info/warn/error. Other values return 0 hits."},
								"component":     {"type": "string",  "description": "Filter by audit component name."},
								"artifact_id":   {"type": "string",  "description": "Filter by artifact id (memory/decision/theory/lesson/work)."},
								"days":          {"type": "number",  "default": 7, "description": "Lookback window in days. Ignored when 'since' is supplied. Default 7."},
								"since":         {"type": "number",  "description": "Absolute epoch-seconds cutoff. Wins over 'days' when both are present. Future values clamp to 1 day."},
								"limit":         {"type": "number",  "default": 20, "description": "Max rows to return. Default 20."},
								"include_stack": {"type": "boolean", "default": false, "description": "Include the audit stack trace when available. Default false."}
							},
							"additionalProperties": false
						}
					},
					"required": ["params"]
				},
				{
					"properties": {
						"action": {"const": "list_clusters"},
						"params": {"type": "object", "properties": {}, "additionalProperties": false}
					},
					"required": ["params"]
				},
				{
					"properties": {
						"action": {"const": "snooze_cluster"},
						"params": {
							"type": "object",
							"properties": {
								"cluster_key":  {"type": "string", "description": "Primary key from list_clusters. Required."},
								"snooze_until": {"type": "string", "description": "Reactivation cutoff. ISO 8601 absolute ('2026-07-12T12:00:00Z') OR Go duration ('24h', '7d', '1h30m'). Required."},
								"reason":       {"type": "string", "description": "Audit-friendly note. Optional."}
							},
							"required": ["cluster_key", "snooze_until"],
							"additionalProperties": false
						}
					},
					"required": ["params"]
				},
				{
					"properties": {
						"action": {"const": "unsnooze_cluster"},
						"params": {
							"type": "object",
							"properties": {
								"cluster_key": {"type": "string", "description": "Primary key from list_clusters. Required."},
								"reason":      {"type": "string", "description": "Audit-friendly note explaining why the cluster is being reactivated. Optional."}
							},
							"required": ["cluster_key"],
							"additionalProperties": false
						}
					},
					"required": ["params"]
				},
				{
					"properties": {
						"action": {"const": "resolve_cluster"},
						"params": {
							"type": "object",
							"properties": {
								"cluster_key": {"type": "string", "description": "Primary key from list_clusters. Required."},
								"reason":      {"type": "string", "description": "Audit-friendly note explaining root cause. Optional."}
							},
							"required": ["cluster_key"],
							"additionalProperties": false
						}
					},
					"required": ["params"]
				},
				{
					"properties": {
						"action": {"const": "annotate_cluster"},
						"params": {
							"type": "object",
							"properties": {
								"cluster_key": {"type": "string", "description": "Primary key (from list_clusters or remembered historical key for resolved clusters). Required."},
								"annotation":  {"type": "string", "description": "Substantive insight text appended to the audit trail verbatim. Required."},
								"reason":      {"type": "string", "description": "Short label (e.g. 'post-mortem', 'week-later-refinement'). Optional."}
							},
							"required": ["cluster_key", "annotation"],
							"additionalProperties": false
						}
					},
					"required": ["params"]
				},
				{
					"properties": {
						"action": {"const": "critic_findings"},
						"params": {
							"type": "object",
							"properties": {
								"limit": {"type": "number", "default": 50, "description": "Max rows to return. Default 50."}
							},
							"additionalProperties": false
						}
					},
					"required": ["params"]
				}
			]
		}`),
		Handler: handleMpmSystem,
	},
	{
		Name:        "log_to_changelog",
		Description: `Self-report agent work as a structured changelog entry tied to a git commit SHA.
Use when: you have completed a meaningful unit of work (a fix, a feature, a refactor) and want to record it in the project changelog with a reference to the commit that shipped it. The changelog entry is permanent and queryable.

Optional assertion params (epistemic confirmation / contradiction, see docs/epistemic-confirmation.md):
  - confirms_lesson_id / confirms_decision_id / confirms_theory_id (string or array of strings): the named artifacts this commit explicitly validates; each produces an evidence row of type 'reproduction' (+0.85) and a confidence recompute against the asserted artifact.
  - contradicts_lesson_id / contradicts_decision_id / contradicts_theory_id (string or array of strings): the named artifacts this commit explicitly shows to be wrong; each produces an evidence row of type 'challenge' (-0.6) and a confidence recompute. A contradiction strong enough to cross confidence below 0.3 triggers the existing cascade invalidation machinery.

Every assertion is explicit-only — no keyword matching, no semantic inference. An empty or absent param is a no-op; both directions can coexist in one call; all assertions and the changelog memory write share a single transaction (all-or-nothing atomicity).`,
		Schema: json.RawMessage(`{"type":"object","properties":{"fact":{"type":"string","description":"Changelog prose for this commit."},"commit_hash":{"type":"string","description":"Full 40-character SHA-1 of the commit being recorded (strict retrospective contract)."},"tags":{"oneOf":[{"type":"string"},{"type":"array","items":{"type":"string"}}],"description":"Tags as a comma-separated string OR a JSON array of strings."},"confirms_lesson_id":{"oneOf":[{"type":"string"},{"type":"array","items":{"type":"string"}}],"description":"Lesson id(s) this commit validates. Each produces one evidence row of type reproduction (+0.85) and a confidence recompute."},"confirms_decision_id":{"oneOf":[{"type":"string"},{"type":"array","items":{"type":"string"}}],"description":"Decision id(s) this commit validates. Each produces one evidence row of type reproduction (+0.85) and a confidence recompute."},"confirms_theory_id":{"oneOf":[{"type":"string"},{"type":"array","items":{"type":"string"}}],"description":"Theory id(s) this commit validates. Each produces one evidence row of type reproduction (+0.85) and a confidence recompute."},"contradicts_lesson_id":{"oneOf":[{"type":"string"},{"type":"array","items":{"type":"string"}}],"description":"Lesson id(s) this commit shows to be wrong. Each produces one evidence row of type challenge (-0.6) and a confidence recompute; crossing confidence below 0.3 triggers the existing cascade invalidation hook."},"contradicts_decision_id":{"oneOf":[{"type":"string"},{"type":"array","items":{"type":"string"}}],"description":"Decision id(s) this commit shows to be wrong. Same semantics as contradicts_lesson_id."},"contradicts_theory_id":{"oneOf":[{"type":"string"},{"type":"array","items":{"type":"string"}}],"description":"Theory id(s) this commit shows to be wrong. Same semantics as contradicts_lesson_id."}},"required":["fact","commit_hash"]}`),
		Handler:     handleLogToChangelog,
	},
	{
		Name:        "request_review",
		Description: `Concurrent multi-component review: ask multiple independent perspectives to evaluate the same artifact simultaneously.
Use when: you want cross-validation before committing a significant decision; you need a second opinion on a memory, theory, or architecture choice; you want to stress-test a plan against different agent personas (memory, critic, scheduler) at the same time.
Each component responds independently and in parallel. One component's failure does not block the others. No consensus synthesis — you read all responses and decide.`,
		Schema:      json.RawMessage(`{"type":"object","properties":{"components":{"type":"array","items":{"type":"string"},"description":"Substrate component names to review (e.g. ['memory','critic']). Required, at least one."},"prompt":{"type":"string","description":"The instruction sent to every component. Required."},"artifacts":{"type":"array","items":{"type":"string"},"description":"Optional memory ids. Bodies are fetched from the database and passed to every component as pre-resolved text."},"strategy":{"type":"string","enum":["parallel"],"default":"parallel"},"timeout_secs":{"type":"number"}},"required":["components","prompt"]}`),
		Handler:     handleRequestReview,
	},

	// ── Phase 1: Pointer / Blob tools ──────────────────────────────────────

	{
		Name:        "mpm_resolve",
		Description: `Resolve a mpm:// URI to its content. Phase 2 supports mpm://blob/<id>, mpm://work/<id>, mpm://memory/<id>, mpm://lesson/<id>, and mpm://theory/<id>.
Use when: you have a pointer from a previous result and need to dereference it to get the actual content. This is the dereferencing step — you get back the full content of whatever the pointer refers to.
max_bytes applies a soft materialization ceiling for large results.`,
		Schema:      json.RawMessage(`{"type":"object","properties":{"uri":{"type":"string","description":"mpm://blob/|work/|memory/|lesson/|theory/<id>"},"max_bytes":{"type":"integer","description":"Phase 2: caller-requested materialization ceiling in bytes"}},"required":["uri"]}`),
		Handler:     handleMpmResolve,
	},
	// ── Memory primitives ──
	// (challenge / restore_challenge live as actions under mpm_memory —
	//  the standalone mpm_challenge tool was retired on 2026-09-05
	//  because it duplicated mpm_memory.challenge with the same wire
	//  contract; see docs/onboarding-mcp-native-audit-2026-09-05.md
	//  Part C / follow-up commit log.)

	{
		Name:        "mpm_blob_read",
		Description: `Read a raw blob by ID with optional byte offset and server-side size cap.
Use when: you need the raw bytes of a blob (a spilled tool result, a large document, binary data) and want server-side pagination rather than fetching the entire thing.
For pointer dereferencing with content type awareness, prefer mpm_resolve.`,
		Schema:      json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"},"offset":{"type":"integer","default":0},"max_bytes":{"type":"integer","default":51200}},"required":["id"]}`),
		Handler:     handleMpmBlobRead,
	},
	{
		Name:        "mpm_blob_search",
		Description: `Server-side regex search within a blob's content.
Use when: you have a blob ID and need to find a specific pattern inside it without downloading and scanning the entire thing. The search runs server-side and returns matching snippets with byte offsets.
For indexed search across all memories, use mpm_memory query.`,
		Schema:      json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"},"query":{"type":"string","maxLength":256},"regex":{"type":"boolean","default":false},"case_insensitive":{"type":"boolean","default":false},"max_matches":{"type":"integer","default":20},"max_bytes":{"type":"integer","default":51200}},"required":["id","query"]}`),
		Handler:     handleMpmBlobSearch,
	},

	// ── Work primitive ────────────────────────────────────────────────

	{
		Name: "mpm_work",
		Description: `Named work items with an immutable event ledger: create, complete, cancel, or track history.
Use when: you have made a commitment to do something that will span multiple sessions; you need to track a task's progress over time; you want to record a note or completion evidence against a specific piece of work.
The event ledger (history) provides full provenance: who created it, when it was completed, what evidence was attached. A work item is never truly "done" until Git evidence is attached via the complete action.
CRITICAL: the complete action requires work_id. Host session termination does NOT auto-complete a work item — the agent decides when work is done and calls action=complete explicitly.
Do not use when: you just want to store a fact or insight (mpm_memory save).`,
		Schema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"action": {"type": "string", "enum": ["create","list","show","update","complete","cancel","history","note","reopen","resolve_contradiction"]}
			},
			"required": ["action"],
			"oneOf": [
				{
					"properties": {
						"action": {"const": "create"},
						"params": {
							"type": "object",
							"properties": {
								"title":      {"type": "string"},
								"content":    {"type": "string"},
								"session_id": {"type": "string"}
							},
							"required": ["title"],
							"additionalProperties": true
						}
					},
					"required": ["params"]
				},
				{
					"properties": {
						"action": {"const": "list"}
					}
				},
				{
					"properties": {
						"action": {"const": "show"},
						"params": {
							"type": "object",
							"properties": {"work_id": {"type": "string"}},
							"required": ["work_id"],
							"additionalProperties": true
						}
					},
					"required": ["params"]
				},
				{
					"properties": {
						"action": {"const": "update"},
						"params": {
							"type": "object",
							"properties": {
								"work_id":    {"type": "string"},
								"title":       {"type": "string"},
								"content":     {"type": "string"},
								"status":      {"type": "string", "enum": ["open","done","cancelled"]}
							},
							"required": ["work_id"],
							"additionalProperties": true
						}
					},
					"required": ["params"]
				},
				{
					"properties": {
						"action": {"const": "complete"},
						"params": {
							"type": "object",
							"properties": {
								"work_id":    {"type": "string"},
								"title":       {"type": "string"},
								"content":     {"type": "string"},
								"session_id":  {"type": "string"},
								"note":        {"type": "string"}
							},
							"required": ["work_id"],
							"additionalProperties": true
						}
					},
					"required": ["params"]
				},
				{
					"properties": {
						"action": {"const": "cancel"},
						"params": {
							"type": "object",
							"properties": {"work_id": {"type": "string"}, "note": {"type": "string"}},
							"required": ["work_id"],
							"additionalProperties": true
						}
					},
					"required": ["params"]
				},
				{
					"properties": {
						"action": {"const": "history"},
						"params": {
							"type": "object",
							"properties": {"work_id": {"type": "string"}},
							"required": ["work_id"],
							"additionalProperties": true
						}
					},
					"required": ["params"]
				},
				{
					"properties": {
						"action": {"const": "note"},
						"params": {
							"type": "object",
							"properties": {"work_id": {"type": "string"}, "note": {"type": "string"}},
							"required": ["work_id"],
							"additionalProperties": true
						}
					},
					"required": ["params"]
				},
				{
					"properties": {
						"action": {"const": "reopen"},
						"params": {
							"type": "object",
							"properties": {"work_id": {"type": "string"}, "note": {"type": "string"}},
							"required": ["work_id"],
							"additionalProperties": true
						}
					},
					"required": ["params"]
				},
				{
					"properties": {
						"action": {"const": "resolve_contradiction"},
						"params": {
							"type": "object",
							"properties": {
								"work_id":       {"type": "string"},
								"winner_id":      {"type": "string"},
								"winner_outcome": {"type": "string", "enum": ["open","done","cancelled"]},
								"reason":         {"type": "string", "minLength": 1, "description": "Required audit-trail explanation for resolving the contradiction. Enforced by handleResolveContradictionWork at work_handlers.go:343-345."}
							},
							"required": ["work_id", "reason"],
							"additionalProperties": true
						}
					},
					"required": ["params"]
				}
			]
		}`),
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

// MustByName is the panicking form of ByName. Use only at server
// boot when wiring closures, where a missing entry is a programming
// error and the alternative (silent empty handler) is worse.
func MustByName(name string) Tool {
	t, ok := ByName(name)
	if !ok {
		panic("tools.MustByName: registry has no entry for " + name)
	}
	return t
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
