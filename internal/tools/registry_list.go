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
		Name:        "save_to_memory",
		Description: "Persist a fact, lesson, or decision to MPM long-term memory.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"fact":{"type":"string"},"tags":{"type":"string"},"weight":{"type":"number"},"ttl":{"type":"string"},"collection":{"type":"string"}},"required":["fact"]}`),
		Handler:     handleSaveToMemory,
	},
	{
		Name:        "query_long_term_memory",
		Description: "Search MPM long-term memory by FTS5 + semantic + reinforcement scoring.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"},"limit":{"type":"number"},"collection":{"type":"string"}},"required":["query"]}`),
		Handler:     handleQueryLongTermMemory,
	},
	{
		Name:        "challenge_memory",
		Description: "Weaken a memory and create a pending theory based on contradictory evidence.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"memoryId":{"type":"string"},"evidence":{"type":"string"}},"required":["memoryId","evidence"]}`),
		Handler:     handleChallengeMemory,
	},
	{
		Name:        "propose_theory",
		Description: "Create a pending theory row in the memories table (collection='theories').",
		Schema:      json.RawMessage(`{"type":"object","properties":{"hypothesis":{"type":"string"},"validation_criteria":{"type":"string"},"tags":{"type":"string"}},"required":["hypothesis"]}`),
		Handler:     handleProposeTheory,
	},
	{
		Name:        "resolve_theory",
		Description: "Resolve a pending theory as confirmed or disproven.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"theoryId":{"type":"string"},"conclusion":{"type":"string","enum":["confirmed","disproven"]},"newStatus":{"type":"string","enum":["proven","disproven"]}},"required":["theoryId","conclusion","newStatus"]}`),
		Handler:     handleResolveTheory,
	},
	{
		Name:        "record_decision",
		Description: "Record an architectural decision with context, choice, and rationale.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"context":{"type":"string"},"choice":{"type":"string"},"rationale":{"type":"string"},"tags":{"type":"string"}},"required":["context","choice","rationale"]}`),
		Handler:     handleRecordDecision,
	},
	{
		Name:        "save_lesson",
		Description: "Persist a lesson learned (warning / practice / insight).",
		Schema:      json.RawMessage(`{"type":"object","properties":{"fact":{"type":"string"},"type":{"type":"string","enum":["warning","practice","insight"]},"tags":{"type":"string"}},"required":["fact"]}`),
		Handler:     handleSaveLesson,
	},
	{
		Name:        "search_lessons",
		Description: "Search MPM lessons by content query.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"},"limit":{"type":"number"}},"required":["query"]}`),
		Handler:     handleSearchLessons,
	},
	{
		Name:        "list_lessons",
		Description: "List lessons, optionally filtered by type.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"type":{"type":"string","enum":["warning","practice","insight"]}}}`),
		Handler:     handleListLessons,
	},
	{
		Name:        "create_topic",
		Description: "Create a topic in MPM.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"},"description":{"type":"string"}},"required":["name"]}`),
		Handler:     handleCreateTopic,
	},
	{
		Name:        "search_topics",
		Description: "Search topics by name or description.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}`),
		Handler:     handleSearchTopics,
	},
	{
		Name:        "link_topic",
		Description: "Link a memory to a topic.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"memory_id":{"type":"string"},"topic_id":{"type":"string"}},"required":["memory_id","topic_id"]}`),
		Handler:     handleLinkTopic,
	},
	{
		Name:        "add_reference",
		Description: "Add an external reference document (file_path, content) to MPM.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"filepath":{"type":"string"},"title":{"type":"string"},"content":{"type":"string"}},"required":["filepath"]}`),
		Handler:     handleAddReference,
	},
	{
		Name:        "search_references",
		Description: "Search reference documents by content.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"},"limit":{"type":"number"}},"required":["query"]}`),
		Handler:     handleSearchReferences,
	},
	{
		Name:        "list_references",
		Description: "List reference documents.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"limit":{"type":"number"}}}`),
		Handler:     handleListReferences,
	},
	{
		Name:        "add_evidence",
		Description: "Attach evidence to an artifact (memory, theory, decision, or lesson) to support or refute it.",
		Schema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"artifact_id":         {"type": "string",  "description": "ID of the artifact (memory/theory/decision/lesson) this evidence attaches to. To attach to a theory, set artifact_type='theory'."},
				"artifact_type":       {"type": "string",  "enum": ["memory","theory","decision","lesson"], "default": "memory", "description": "Type of the artifact. Defaults to 'memory' if omitted."},
				"type":                {"type": "string",  "enum": ["observation","test","reproduction","challenge","decision_outcome","external_reference"], "description": "Evidence type. Determines default strength when strength is not provided (challenge is negative; the rest are positive)."},
				"source_group":        {"type": "string",  "description": "Grouping tag for the source, e.g. 'wc2026 R32' or 'audit-trail-2026-07'."},
				"source_url":          {"type": "string",  "description": "Optional URL of the source."},
				"source_path":         {"type": "string",  "description": "Optional filesystem path of the source."},
				"notes":               {"type": "string",  "description": "Optional narrative note about the evidence."},
				"created_by":          {"type": "string",  "description": "Creator/agent identifier."},
				"strength":            {"type": "number",  "minimum": -1, "maximum": 1, "description": "Optional. Strength in [-1, 1]; defaults to the type's registry value when omitted. Use negative for refuting evidence (challenge type defaults to -0.6)."},
				"independence_factor": {"type": "number",  "default": 1.0,  "description": "Optional independence factor for confidence aggregation; defaults to 1.0."}
			},
			"required": ["artifact_id", "type", "source_group", "created_by"]
		}`),
		Handler: handleAddEvidence,
	},
	{
		Name:        "list_evidence",
		Description: "List evidence rows attached to an artifact (memory, theory, decision, or lesson).",
		Schema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"artifact_id":   {"type": "string", "description": "ID of the artifact to list evidence for."},
				"artifact_type": {"type": "string", "enum": ["memory","theory","decision","lesson"], "default": "memory", "description": "Type of the artifact. Defaults to 'memory' if omitted."}
			},
			"required": ["artifact_id"]
		}`),
		Handler: handleListEvidence,
	},
	{
		Name:        "query_confidence_history",
		Description: "Query the confidence timeline for a memory or theory.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"memory_id":{"type":"string"},"limit":{"type":"number"}}}`),
		Handler:     handleQueryConfidenceHistory,
	},
	{
		Name:        "query_confidence_changes",
		Description: "Query confidence change events.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"memory_id":{"type":"string"},"since":{"type":"string"},"until":{"type":"string"}}}`),
		Handler:     handleQueryConfidenceChanges,
	},
	{
		Name:        "query_confidence_trend",
		Description: "Compute a confidence trend (linear fit) for a memory.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"memory_id":{"type":"string"},"window_days":{"type":"number"}},"required":["memory_id"]}`),
		Handler:     handleQueryConfidenceTrend,
	},
	{
		Name:        "query_memory_quality",
		Description: "Per-source memory quality statistics.",
		Schema:      json.RawMessage(`{"type":"object"}`),
		Handler:     handleQueryMemoryQuality,
	},
	{
		Name:        "show_confidence",
		Description: "Show the current confidence of a memory.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"memory_id":{"type":"string"}},"required":["memory_id"]}`),
		Handler:     handleShowConfidence,
	},
	{
		Name:        "recompute_confidence",
		Description: "Recompute confidence for a memory using current evidence.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"memory_id":{"type":"string"}},"required":["memory_id"]}`),
		Handler:     handleRecomputeConfidence,
	},
	{
		Name:        "explain_confidence",
		Description: "Explain how a memory's confidence was computed (factors + history).",
		Schema:      json.RawMessage(`{"type":"object","properties":{"memory_id":{"type":"string"}},"required":["memory_id"]}`),
		Handler:     handleExplainConfidence,
	},
	{
		Name:        "query_audit_log",
		Description: "Query the runtime anomaly ledger.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"level":{"type":"string"},"component":{"type":"string"},"days":{"type":"number"},"limit":{"type":"number"}}}`),
		Handler:     handleQueryAuditLog,
	},
	{
		Name:        "list_active_clusters",
		Description: "List active audit cluster proposals, partitioned into known vs unknown. Use right before session_end to capture unresolved clusters for the open_questions payload of session_handoff.",
		Schema:      json.RawMessage(`{"type":"object"}`),
		Handler:     handleListActiveClusters,
	},
	{
		Name:        "read_wake_context",
		Description: "Read the agent's wake context — session state, active mode, recent memories.",
		Schema:      json.RawMessage(`{"type":"object"}`),
		Handler:     handleReadWakeContext,
	},
	{
		Name:        "read_directives",
		Description: "Read the active behavioral directives (prime operating principles).",
		Schema:      json.RawMessage(`{"type":"object"}`),
		Handler:     handleReadDirectives,
	},
	{
		Name:        "proactive_recall_hint",
		Description: "Surface conversation-relevant memories as hints before the agent acts.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"conversation":{"type":"string"},"max_hints":{"type":"number"}},"required":["conversation"]}`),
		Handler:     handleProactiveRecallHint,
	},
	{
		Name:        "route",
		Description: "Evaluate the current mode/persona routing for a text snippet.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}`),
		Handler:     handleRoute,
	},
	{
		Name:        "session_end",
		Description: "End the current session: write a handoff for next wake to surface.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"session_id":{"type":"string"},"summary":{"type":"string"},"commitments":{"type":"array"},"open_questions":{"type":"array"}},"required":["session_id","summary"]}`),
		Handler:     handleSessionEnd,
	},
	{
		Name:        "session_handoff",
		Description: "Read the latest session handoff (used by wake context automatically).",
		Schema:      json.RawMessage(`{"type":"object","properties":{"unread":{"type":"boolean"},"mark_read":{"type":"boolean"}}}`),
		Handler:     handleSessionHandoff,
	},
	{
		Name:        "list_handoffs",
		Description: "List session handoff history.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"limit":{"type":"number"},"unread":{"type":"boolean"}}}`),
		Handler:     handleListHandoffs,
	},
	{
		Name:        "log_to_changelog",
		Description: "Self-report agent work as a changelog entry tied to a git commit SHA.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"fact":{"type":"string"},"commit_hash":{"type":"string"},"tags":{"type":"string"}},"required":["fact","commit_hash"]}`),
		Handler:     handleLogToChangelog,
	},
	{
		Name:        "shred_memory",
		Description: "Hard-delete a memory and its mirror file.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"memory_id":{"type":"string"}},"required":["memory_id"]}`),
		Handler:     handleShredMemory,
	},
	{
		Name:        "reinforce_memory",
		Description: "Increment reinforcement count and bump weight.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"memory_id":{"type":"string"},"delta":{"type":"number"}},"required":["memory_id"]}`),
		Handler:     handleReinforceMemory,
	},
	{
		Name:        "weaken_memory",
		Description: "Decrement reinforcement count and reduce weight.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"memory_id":{"type":"string"},"delta":{"type":"number"}},"required":["memory_id"]}`),
		Handler:     handleWeakenMemory,
	},
	{
		Name:        "snooze_memory",
		Description: "Temporarily suppress a memory from retrieval.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"memory_id":{"type":"string"},"hours":{"type":"number"}},"required":["memory_id"]}`),
		Handler:     handleSnoozeMemory,
	},
	{
		Name:        "set_memory_weight",
		Description: "Set an explicit weight on a memory (0-100 scale).",
		Schema:      json.RawMessage(`{"type":"object","properties":{"memory_id":{"type":"string"},"weight":{"type":"number"}},"required":["memory_id","weight"]}`),
		Handler:     handleSetMemoryWeight,
	},
	{
		Name:        "patch_memory",
		Description: "Patch metadata on an existing memory via JSON-Patch ops.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"memory_id":{"type":"string"},"patch":{"type":"object"}},"required":["memory_id","patch"]}`),
		Handler:     handlePatchMemory,
	},
	{
		Name:        "promote_memory",
		Description: "Promote a memory to long-term (LTM) status.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"memory_id":{"type":"string"}},"required":["memory_id"]}`),
		Handler:     handlePromoteMemory,
	},
	{
		Name:        "review_memories",
		Description: "List memories due for spaced-repetition review.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"limit":{"type":"number"},"stale_only":{"type":"boolean"}}}`),
		Handler:     handleReviewMemories,
	},
	{
		Name:        "synthesize_memory",
		Description: "Run LLM synthesis on a memory (find near-miss duplicates, optionally merge).",
		Schema:      json.RawMessage(`{"type":"object","properties":{"memory_id":{"type":"string"}},"required":["memory_id"]}`),
		Handler:     handleSynthesizeMemory,
	},
	{
		Name:        "gc_run",
		Description: "Run a lifecycle decay / archive sweep.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"review":{"type":"boolean"},"purge":{"type":"boolean"}}}`),
		Handler:     handleGCRun,
	},
	{
		Name:        "query_global_rules",
		Description: "Query shared (cross-agent) global rules from the MPM_SHARED_DB. Returns is_global=1 rows from shared.memories. Empty result when no shared DB is attached (local-only mode).",
		Schema:      json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"},"limit":{"type":"number"}}}`),
		Handler:     handleQueryGlobalRules,
	},
	{
		Name:        "record_global_rule",
		Description: "Operator-gated: write a memory to the shared DB with is_global=1. Requires confirm=true. Without confirm the call is rejected. CLI and MCP both gate on this; agents should not write house rules autonomously.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"fact":{"type":"string"},"tags":{"type":"string"},"weight":{"type":"number"},"provenance":{"type":"string"},"confirm":{"type":"boolean"}},"required":["fact","confirm"]}`),
		Handler:     handleRecordGlobalRule,
	},
	{
		Name:        "promote_to_global",
		Description: "Operator-gated: copy a local memory to the shared DB. Original local row stays in the local DB. Shared copy gets is_global=1 and metadata.derived_from_local_id linking back. Requires confirm=true.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"memory_id":{"type":"string"},"confirm":{"type":"boolean"}},"required":["memory_id","confirm"]}`),
		Handler:     handlePromoteToGlobal,
	},
	{
		// Phase 5a: opportunistic scheduler. target_time accepts an
		// absolute unix epoch OR a relative duration string ("24h", "2h",
		// "30m", "7d"). recurring_rule is stored as a hint; the agent
		// itself is responsible for re-scheduling (no daemon parses it).
		Name:        "schedule_wake",
		Description: "Schedule a future wake: writes a row to scheduled_wakes. Any subsequent MPM call after target_time surfaces it as WakesPending in the response. Stateless — no daemon, no cron. Use theory_id to bind the wake to a pending theory.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"reason":{"type":"string"},"target_time":{"type":"string"},"theory_id":{"type":"string"},"recurring_rule":{"type":"string"},"metadata":{"type":"object"}},"required":["reason","target_time"]}`),
		Handler:     handleScheduleWake,
	},
	{
		// Phase 5a: explicit pull variant of the opportunistic check.
		// Folds any due wakes into the response as WakesPending. Useful
		// at session start or after a passive poll.
		Name:        "check_wakes",
		Description: "Pull all due wakes (fired=0 AND target_time<=now), mark them fired, and return them in WakesPending. Idempotent across concurrent callers (transactional mark).",
		Schema:      json.RawMessage(`{"type":"object","properties":{}}`),
		Handler:     handleCheckWakes,
	},
	{
		// Phase 5a: inspect the wake queue. Defaults to pending only;
		// pass include_fired=true for audit, overdue_only=true for the
		// "what did I forget?" inspection case.
		Name:        "list_wakes",
		Description: "List scheduled wakes. Defaults to pending (unfired) only. Pass overdue_only=true to see wakes whose target_time has passed but were never surfaced (likely missed).",
		Schema:      json.RawMessage(`{"type":"object","properties":{"include_fired":{"type":"boolean"},"overdue_only":{"type":"boolean"},"limit":{"type":"number"}}}`),
		Handler:     handleListWakes,
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