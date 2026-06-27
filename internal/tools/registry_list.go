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
		Schema:      json.RawMessage(`{"type":"object","properties":{"statement":{"type":"string"},"rationale":{"type":"string"},"tags":{"type":"string"},"memory_id":{"type":"string"}}}`),
		Handler:     handleProposeTheory,
	},
	{
		Name:        "resolve_theory",
		Description: "Resolve a pending theory as confirmed or disproven.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"theory_id":{"type":"string"},"conclusion":{"type":"string","enum":["confirmed","disproven"]},"evidence":{"type":"string"}},"required":["theory_id","conclusion"]}`),
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
		Description: "Add a piece of evidence supporting or refuting a theory.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"theory_id":{"type":"string"},"notes":{"type":"string"},"source_group":{"type":"string"},"source_url":{"type":"string"},"source_path":{"type":"string"},"created_by":{"type":"string"}},"required":["theory_id"]}`),
		Handler:     handleAddEvidence,
	},
	{
		Name:        "list_evidence",
		Description: "List evidence entries, optionally filtered.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"theory_id":{"type":"string"},"limit":{"type":"number"}}}`),
		Handler:     handleListEvidence,
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