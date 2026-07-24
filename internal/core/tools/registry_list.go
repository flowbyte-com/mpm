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
		Name:        "migrate",
		Description: "Import memories from a non-SQLite source (markdown/JSON). Stage mode: parses source and inserts into raw_memories (pending). Commit mode: promotes a previously-staged batch to memories. Undo mode: rejects all rows for a batch. Format auto-detected from extension; pass format='markdown' or 'json' to override.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"from_path":{"type":"string","description":"Source file path"},"format":{"type":"string","enum":["markdown","json","auto"],"default":"auto"},"label":{"type":"string","description":"Batch label (used in batch_id)"},"dry_run":{"type":"boolean","default":false},"commit":{"type":"boolean","default":false,"description":"After staging, immediately promote the batch to memories"},"commit_batch":{"type":"string","description":"Promote a previously-staged batch (alternative to from_path)"},"undo_batch":{"type":"string","description":"Rollback a batch (alternative to from_path)"}}}`),
		Handler:     handleMigrate,
	},
	{
		Name:        "save_to_memory",
		Description: "Persist a fact, lesson, or decision to MPM long-term memory. Weight accepts both the legacy 0.0-1.0 float scale (multiplied by 10) AND the 0-100 integer scale (used directly) — values > 1.0 are auto-detected as integer-scale and stored verbatim. Default weight is 5 (mid-low confidence) when omitted.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"fact":{"type":"string"},"tags":{"oneOf":[{"type":"string"},{"type":"array","items":{"type":"string"}}],"description":"Tags as a comma-separated string OR a JSON array of strings."},"weight":{"type":"number"},"ttl":{"type":"string"},"collection":{"type":"string"}},"required":["fact"]}`),
		Handler:     handleSaveToMemory,
	},
	{
		Name:        "query_long_term_memory",
		Description: "Searches local + shared memories by FTS5 lexical matching + semantic similarity + reinforcement scoring. Hyphenated words are split into separate tokens (e.g., 'lazy-start' becomes tokens 'lazy' AND 'start'), so natural-language hyphenated queries work correctly. Use broad keywords rather than literal phrases — the FTS5 contract is implicit-AND across all tokens, with prefix wildcards applied per token. Special FTS5 characters (\", (, ), *, +, :, -) are stripped from query input; pass natural-language strings, not raw FTS5 syntax. Federated across DBs: scope='all' (default) merges local + shared with Shared Premium scoring (1.20x shared, 1.35x shared+rules, capped at 1.0); 'local' restricts to local tables; 'shared' restricts to shared.memories via FTS5. Use collection to narrow to a specific collection (theories, decisions, lessons, memories, etc.). Pass empty query to return no results (not all memories).",
		Schema:      json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"},"limit":{"type":"number"},"collection":{"type":"string"},"scope":{"type":"string","enum":["all","local","shared"],"description":"Recall scope. all (default) merges local + shared with Shared Premium (1.20x shared, 1.35x shared+rules, cap 1.0); local restricts to local tables; shared restricts to shared.memories via FTS5.","default":"all"}},"required":["query"]}`),
		Handler:     handleQueryLongTermMemory,
	},
	{
		Name:        "challenge_memory",
		Description: "Weaken a memory and create a pending theory based on contradictory evidence.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"memoryId":{"type":"string"},"evidence":{"type":"string"}},"required":["memoryId","evidence"]}`),
		Handler:     handleChallengeMemory,
	},
	{
		Name:        "commit_milestone",
		Description: "Commit a deliberate narrative milestone. Thin wrapper over save_to_memory with a type:milestone-* tag for wake-context surfacing. summary must be ≥50 chars and defensible from the summary alone.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"summary":{"type":"string","minLength":50,"description":"The milestone claim. Must be ≥50 chars and stand alone — another agent should be able to defend it from the summary text without surrounding context."},"flavor":{"type":"string","enum":["shipped","insight"],"default":"shipped","description":"Type of milestone. 'shipped' = work done (e.g. shipped a feature, fixed a class of bug, closed a debt arc). 'insight' = durable learning (e.g. architectural rule discovered, anti-pattern identified, principle earned)."},"tags":{"oneOf":[{"type":"string"},{"type":"array","items":{"type":"string"}}],"description":"Tags as a comma-separated string OR a JSON array of strings. The handler always injects type:milestone-<flavor>; do not double-prefix."}},"required":["summary"]}`),
		Handler:     handleCommitMilestone,
	},
	{
		Name:        "propose_theory",
		Description: "Create a pending theory row in the memories table (collection='theories'). Pass dependencies as a JSON array of artifact IDs (memories, lessons, theories) that this theory's reasoning depends on; deletion of any dependency fires a 'stale foundation' wake so the agent can re-evaluate.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"hypothesis":{"type":"string"},"validation_criteria":{"type":"string"},"tags":{"oneOf":[{"type":"string"},{"type":"array","items":{"type":"string"}}],"description":"Tags as a comma-separated string OR a JSON array of strings."},"dependencies":{"type":"array","items":{"type":"string"},"description":"Artifact IDs (memories/lessons/theories) this theory depends on. Deletion fires a stale-foundation wake."}},"required":["hypothesis"]}`),
		Handler:     handleProposeTheory,
	},
	{
		Name:        "resolve_theory",
		Description: "Resolve a pending theory. NOTE: this explicit tool exists but is now the FALLBACK path. The PREFERRED path is the implicit auto-resolution hook: save a memory carrying tags `theory:<id>` AND `outcome:proven` (or `outcome:disproven`), and the theory row flips status in the same transaction as the memory insert. Use this explicit resolve_theory call only when you don't have a memory to anchor the resolution to. Required: theoryId, conclusion (enum: 'confirmed' or 'disproven'). Optional: newStatus (defaults to match conclusion — 'proven' for confirmed, 'disproven' for rejected). Forensics: the implicit hook sets resolved_by='save_to_memory:theory_resolve_hook'; this explicit tool sets resolved_by='mcp:resolve_theory'.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"theoryId":{"type":"string"},"conclusion":{"type":"string","enum":["confirmed","disproven"]},"newStatus":{"type":"string","enum":["proven","disproven"]},"winnerId":{"type":"string","description":"Arc 1 closure: when present, treats the theory as an arbitration theory from resolve-contradictions and routes to the auto-slash path. Absent = legacy path (mark resolved, no slash)."}},"required":["theoryId","conclusion","newStatus"]}`),
		Handler:     handleResolveTheory,
	},
	{
		Name:        "record_decision",
		Description: "Record an architectural decision. STRICTLY REQUIRED fields: context (the situation that triggered the decision), choice (what was decided), rationale (why this choice over alternatives — the substrate rejects empty rationale as a malformed record). The rationale field is non-bypassable: an agent must always articulate why the choice was made, even if briefly. Optional but recommended: outcome (post-hoc learning about the decision's eventual outcome, surfaced via search_references for future-me), tags (semantic territory for proactive_recall_hint), dependencies (JSON array of memory/theory/decision IDs this decision builds on).",
		Schema:      json.RawMessage(`{"type":"object","properties":{"context":{"type":"string"},"choice":{"type":"string"},"rationale":{"type":"string"},"outcome":{"type":"string","description":"Optional. Out-of-band learning captured about the decision's eventual outcome; visible to future-me via search_references."},"tags":{"oneOf":[{"type":"string"},{"type":"array","items":{"type":"string"}}],"description":"Tags as a comma-separated string OR a JSON array of strings."}},"required":["context","choice","rationale"]}`),
		Handler:     handleRecordDecision,
	},
	{
		Name:        "save_lesson",
		Description: "Persist a lesson learned (warning / practice / insight).",
		Schema:      json.RawMessage(`{"type":"object","properties":{"fact":{"type":"string"},"type":{"type":"string","enum":["warning","practice","insight"]},"tags":{"oneOf":[{"type":"string"},{"type":"array","items":{"type":"string"}}],"description":"Tags as a comma-separated string OR a JSON array of strings."}},"required":["fact"]}`),
		Handler:     handleSaveLesson,
	},
	{
		Name:        "search_lessons",
		Description: "Searches lessons by FTS5 lexical matching. Hyphenated words are split into separate tokens (e.g., 'lazy-start' becomes tokens 'lazy' AND 'start'), so natural-language hyphenated queries work correctly. Use broad keywords rather than literal phrases — the FTS5 contract is implicit-AND across all tokens, with prefix wildcards applied per token. Special FTS5 characters (\", (, ), *, +, :, -) are stripped from query input; pass natural-language strings, not raw FTS5 syntax. Examples: query 'lazy-start' matches lessons containing 'lazy' AND 'start'; query 'go test' matches lessons containing 'go' AND 'test'. Pass empty query to return no results (not all lessons).",
		Schema:      json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}`),
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
		Description: "Create a topic in MPM — a named semantic cluster for grouping related memories. Required: name (short slug; e.g., '2026-world-cup', 'openclaw-mcp'). Optional: description (longer prose describing the theme). Returns the topic ID; use link_topic to attach memories. Topics organize multi-memory themes independently of collection (lessons, decisions, memories) — the same memory can belong to multiple topics; the same topic can hold memories across collections.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"},"description":{"type":"string"}},"required":["name"]}`),
		Handler:     handleCreateTopic,
	},
	{
		Name:        "search_topics",
		Description: "Search topics by name or description.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"},"limit":{"type":"number","default":20,"description":"Optional. Max results; default 20."}},"required":["query"]}`),
		Handler:     handleSearchTopics,
	},
	{
		Name:        "link_topic",
		Description: "Link a memory to a topic. Required: memoryId, topicId. Optional: relevance (float 0.0-1.0; default 1.0 — how central the memory is to the topic; surfaces in retrieval ordering). One memory can belong to many topics; one topic can hold many memories. When a memory linked to a topic is recalled, the topic surfaces adjacent linked memories too — topics are how the agent builds multi-memory context windows. Idempotent: re-linking with the same relevance is a no-op.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"memory_id":{"type":"string"},"topic_id":{"type":"string"}},"required":["memory_id","topic_id"]}`),
		Handler:     handleLinkTopic,
	},
	{
		Name:        "add_reference",
		Description: "Add an external reference document by reading its file path into MPM.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"filepath":{"type":"string"},"title":{"type":"string"}},"required":["filepath"]}`),
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
		Description: "List reference documents (long-form source material ingested via add_reference). Optional: limit (default 50), offset (default 0; for pagination beyond the first page). Returns rows with id, title, filepath, indexed_at, and chunk_count. Reference docs live in a separate 'shelf' from memories — they don't decay and aren't auto-promoted to long-term, but they ARE searchable via search_references. Use for the 'what source material have we ingested?' inspection case.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"limit":{"type":"number","default":50},"offset":{"type":"number","default":0,"description":"Optional. Pagination offset; default 0."}}}`),
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
				"source_group":        {"type": "string",  "description": "Grouping tag for the source, e.g. 'wc2026 R32' or 'audit-trail-2026-07'. Use 'notes' for narrative context, citations, or per-evidence rationale."},
				"notes":               {"type": "string",  "description": "Optional narrative note about the evidence — use this for citations, URLs, file paths, or any context the source_group tag can't carry."},
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
		Description: "Query the confidence timeline for an artifact (memory, theory, decision, or lesson).",
		Schema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"artifact_id":   {"type": "string", "description": "ID of the artifact to query."},
				"artifact_type": {"type": "string", "enum": ["memory","theory","decision","lesson"], "default": "memory", "description": "Type of the artifact."},
				"limit":         {"type": "number", "default": 50, "description": "Max rows to return. Defaults to 50."}
			},
			"required": ["artifact_id"]
		}`),
		Handler: handleQueryConfidenceHistory,
	},
	{
		Name:        "query_confidence_changes",
		Description: "Query confidence change events (delta + trigger) for an artifact, or global recent events.",
		Schema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"artifact_id":        {"type": "string", "enum": ["memory","theory","decision","lesson"], "default": "memory", "description": "Optional. ID of the artifact to filter on. Pair with artifact_type when ambiguous."},
				"artifact_type":      {"type": "string", "enum": ["memory","theory","decision","lesson"], "default": "memory", "description": "Optional. Type of the artifact. Defaults to 'memory'."},
				"since":              {"type": "string", "description": "Optional. ISO-8601 timestamp or unix epoch seconds — return changes on or after this time."},
				"since_seconds_ago":  {"type": "number", "description": "Optional alternative to 'since': duration in seconds from now."},
				"limit":              {"type": "number", "description": "Optional. Max rows to return; default 50."}
			}
		}`),
		Handler: handleQueryConfidenceChanges,
	},
	{
		Name:        "query_confidence_trend",
		Description: "Compute a confidence trend (linear fit) for an artifact's history.",
		Schema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"artifact_id":   {"type": "string", "description": "ID of the artifact."},
				"artifact_type": {"type": "string", "enum": ["memory","theory","decision","lesson"], "default": "memory", "description": "Type of the artifact."},
				"window_days":   {"type": "number", "default": 30,  "description": "Rolling window for the linear fit; defaults to 30 days."}
			},
			"required": ["artifact_id"]
		}`),
		Handler: handleQueryConfidenceTrend,
	},
	{
		Name:        "query_memory_quality",
		Description: "Per-source memory quality statistics. No parameters required. Returns aggregate stats grouped by source (call, migrate, seed, web, etc.): counts, average weight, average confidence, confidence distribution histogram, weight distribution histogram. Use to spot a source that's degrading (e.g., a migrate import that brought in low-quality rows) or to audit LTM proportions across sources. Results are point-in-time — re-run periodically to track drift between sessions.",
		Schema:      json.RawMessage(`{"type":"object"}`),
		Handler:     handleQueryMemoryQuality,
	},
	{
		Name:        "show_confidence",
		Description: "Show the current confidence of an artifact (memory, theory, decision, or lesson).",
		Schema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"artifact_id":   {"type": "string"},
				"artifact_type": {"type": "string", "enum": ["memory","theory","decision","lesson"], "default": "memory"}
			},
			"required": ["artifact_id"]
		}`),
		Handler: handleShowConfidence,
	},
	{
		Name:        "recompute_confidence",
		Description: "Force a manual recompute of an artifact's confidence from current evidence.",
		Schema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"artifact_id":   {"type": "string"},
				"artifact_type": {"type": "string", "enum": ["memory","theory","decision","lesson"], "default": "memory"}
			},
			"required": ["artifact_id"]
		}`),
		Handler: handleRecomputeConfidence,
	},
	{
		Name:        "explain_confidence",
		Description: "Explain how an artifact's confidence was computed (factors + history trace).",
		Schema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"artifact_id":   {"type": "string"},
				"artifact_type": {"type": "string", "enum": ["memory","theory","decision","lesson"], "default": "memory"}
			},
			"required": ["artifact_id"]
		}`),
		Handler: handleExplainConfidence,
	},
	{
		Name:        "query_audit_log",
		Description: "Query the runtime anomaly ledger. Filters (all optional, combined with AND): level (enum: 'warn' | 'error' | 'fatal'), component (enum: 'relay' | 'synthesis' | 'watcher' | 'security' | 'cluster'), days (integer; default 1 — last N days). Optional: limit (default 50). Returns rows newest-first. Standard shapes: `{'level':'error','days':7}` for all errors in the last week; `{'component':'cluster'}` for cluster-related entries; `{'days':30,'limit':200}` for the deep-scan shape. Use filter examples as a starting point; the underlying query is plain SQL.",
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
		Name:        "snooze_cluster",
		Description: "Temporarily hide an active audit cluster proposal. Auto-reactivates when snooze_until passes. Use when the cluster is a known noisy signal that should not dominate the wake context right now.",
		Schema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"cluster_key":  {"type": "string", "description": "The cluster_key from list_active_clusters (component:hash format)."},
				"snooze_until": {"type": "string", "description": "ISO 8601 absolute (e.g. 2026-07-12T12:00:00Z) OR Go-relative duration (e.g. 24h, 7d, 1h30m). Required."},
				"reason":       {"type": "string", "description": "Audit-friendly note — recorded in audit context for the watchdog stream."}
			},
			"required": ["cluster_key", "snooze_until"]
		}`),
		Handler: handleSnoozeCluster,
	},
	{
		Name:        "resolve_cluster",
		Description: "Permanently dismiss an active audit cluster proposal. Sets status=resolved; the cluster row stays in the table for forensics but is filtered out of list_active_clusters forever. Use when the cluster's root cause is known and it should no longer surface as an active signal.",
		Schema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"cluster_key": {"type": "string", "description": "The cluster_key from list_active_clusters (component:hash format)."},
				"reason":      {"type": "string", "description": "Audit-friendly note explaining the resolution root cause."}
			},
			"required": ["cluster_key"]
		}`),
		Handler: handleResolveCluster,
	},
	{
		Name:        "annotate_cluster",
		Description: "Append a forensic annotation to an existing audit-cluster proposal's audit trail. Does NOT change cluster status, snooze_until, count, or any state field. Use when late-arriving context or root-cause refinement should be captured without re-opening the cluster. Annotations accumulate in the audit log and are queryable via query_audit_log(component='cluster').",
		Schema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"cluster_key": {"type": "string", "description": "The cluster_key from list_active_clusters OR a remembered historical key for resolved/snoozed clusters."},
				"annotation":  {"type": "string", "description": "Substantive insight, root-cause refinement, or post-mortem context. Appended verbatim to the audit trail. Required."},
				"reason":      {"type": "string", "description": "Optional short label (e.g. 'post-mortem', 'week-later-refinement') to categorize the annotation."}
			},
			"required": ["cluster_key", "annotation"]
		}`),
		Handler: handleAnnotateCluster,
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
		Schema:      json.RawMessage(`{"type":"object","properties":{"conversation_text":{"type":"string","description":"Required. The conversation snippet to anchor recall on."},"max_hints":{"type":"number","default":3,"description":"Optional. Max hints to surface; default 3."},"min_score":{"type":"number","default":-3,"description":"Optional. Minimum relevance score; default -3.0 (matches default boost score). Negative values allow weak matches."}},"required":["conversation_text"]}`),
		Handler:     handleProactiveRecallHint,
	},
	{
		Name:        "route",
		Description: "Evaluate the current mode/persona routing for a prompt.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"prompt":{"type":"string","description":"Required. The prompt to route through mode/persona selection."}},"required":["prompt"]}`),
		Handler:     handleRoute,
	},
	{
		Name:        "session_end",
		Description: "End the current session: write a handoff for next wake to surface.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"session_id":{"type":"string"},"summary":{"type":"string"},"state":{"type":"string","description":"Optional. Session state field (e.g. 'clean', 'crashed', 'force_end'); defaults to 'clean' if omitted."},"commitments":{"type":"array"},"open_questions":{"type":"array"}},"required":["session_id","summary"]}`),
		Handler:     handleSessionEnd,
	},

	// --- Ephemeral Scratchpad (volatile thesis storage) ---
	//
	// Single-row-per-session working memory for hypotheses that aren't ready
	// for save_to_memory. Orphans (unpromoted scratchpads from previous
	// sessions) surface on next wake context with age tagging so the agent
	// can promote, amend, or discard.
	//
	// Wire-format invariant: session_id is REQUIRED on all four tools.
	// Defaulting to the current session would let the agent make lazy
	// context-blind queries; explicit session_id keeps the contract uniform
	// across the surface and matches the AST guard rail's expectation.
	{
		Name:        "flush_scratchpad",
		Description: "Save or update a volatile working thesis for a session. Use this to explicitly checkpoint reasoning that isn't ready for permanent memory. Idempotent per session_id.",
		Schema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"session_id": {"type": "string", "description": "The explicit session ID to attach this scratchpad to."},
				"thesis":     {"type": "string", "description": "The core hypothesis, prediction, or working thought."},
				"supporting": {"type": ["string", "object"], "description": "Optional supporting evidence, variables, or context (JSON object or pre-stringified JSON)."}
			},
			"required": ["session_id", "thesis"]
		}`),
		Handler: handleFlushScratchpad,
	},
	{
		Name:        "read_scratchpad",
		Description: "Read the current ephemeral scratchpad for a specific session.",
		Schema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"session_id": {"type": "string", "description": "The explicit session ID of the scratchpad to retrieve."}
			},
			"required": ["session_id"]
		}`),
		Handler: handleReadScratchpad,
	},
	{
		Name:        "discard_scratchpad",
		Description: "Permanently delete an ephemeral scratchpad without promoting it to memory.",
		Schema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"session_id": {"type": "string", "description": "The explicit session ID of the scratchpad to delete."}
			},
			"required": ["session_id"]
		}`),
		Handler: handleDiscardScratchpad,
	},
	{
		Name:        "promote_scratchpad",
		Description: "Atomically promote an ephemeral scratchpad into a permanent memory with a `from-scratchpad:<session_id>` lineage tag, then delete the scratchpad. If the security scanner rejects the synthesized memory, the entire operation rolls back and the scratchpad is preserved for retry.",
		Schema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"session_id": {"type": "string", "description": "The explicit session ID of the scratchpad to promote."}
			},
			"required": ["session_id"]
		}`),
		Handler: handlePromoteScratchpad,
	},
	{
		Name:        "session_handoff",
		Description: "Read the latest session handoff (used by wake context automatically).",
		Schema:      json.RawMessage(`{"type":"object","properties":{"unread":{"type":"boolean"},"mark_read":{"type":"boolean"}}}`),
		Handler:     handleSessionHandoff,
	},
	{
		Name:        "list_handoffs",
		Description: "List session handoff history. Optional: limit (default 10), unread (boolean; default false — set true to surface only handoffs not yet consumed by the wake_context system). Returns rows newest-first with session_id, summary, ended_at, state, and the unread flag. Use when the wake context summary is too short to diagnose a thread: 'what did the previous session hand forward, and is it still unread?'",
		Schema:      json.RawMessage(`{"type":"object","properties":{"limit":{"type":"number"},"unread":{"type":"boolean"}}}`),
		Handler:     handleListHandoffs,
	},
	{
		Name:        "log_to_changelog",
		Description: "Self-report agent work as a changelog entry tied to a git commit SHA.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"fact":{"type":"string"},"commit_hash":{"type":"string"},"tags":{"oneOf":[{"type":"string"},{"type":"array","items":{"type":"string"}}],"description":"Tags as a comma-separated string OR a JSON array of strings."}},"required":["fact","commit_hash"]}`),
		Handler:     handleLogToChangelog,
	},
	{
		Name:        "shred_memory",
		Description: "Hard-delete a memory or lesson by ID. Lesson-aware: passing a lesson ID (from list_lessons / search_lessons / save_lesson) cascades through lessons_base AND lessons_fts atomically in a single transaction; passing a memory ID soft-deletes from memories (sets deleted_at) and cascades to topic_memberships + challenged_theories. The result map reports `lesson_id` vs `memory_id` to disambiguate which path fired. Success:true means the row is actually gone — for lessons this includes the FTS5 index row; for memories this is a soft-delete with audit trail. Idempotent: shredding a non-existent ID returns success:true (no-op). Required field: memory_id (accepts either memory OR lesson IDs).",
		Schema:      json.RawMessage(`{"type":"object","properties":{"memory_id":{"type":"string"}},"required":["memory_id"]}`),
		Handler:     handleShredMemory,
	},
	{
		Name:        "reinforce_memory",
		Description: "Increment reinforcement count and bump weight. Required: memoryId. Optional: delta (integer; default 1 — how much to increment reinforcement_count and add to the weight column). Use when a memory has been useful and should rise in retrieval ranking. Successive calls accumulate: calling twice with delta=1 produces a total delta of 2. The weight change cascades through hybrid-search scoring; reinforcement_count feeds proactive_recall_hint.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"memory_id":{"type":"string"},"delta":{"type":"number"}},"required":["memory_id"]}`),
		Handler:     handleReinforceMemory,
	},
	{
		Name:        "weaken_memory",
		Description: "Decrement reinforcement count and reduce weight. Required: memoryId. Optional: delta (integer; default 1 — how much to decrement reinforcement_count and subtract from the weight column). Use when a memory has been misleading or contradicted. Once weight drops below 0 the row is eligible for `gc_run --shred-negative` (only if a proven theory exists; the theory provides the evidence chain — negative weight alone is never sufficient). Successive calls accumulate.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"memory_id":{"type":"string"},"delta":{"type":"number"}},"required":["memory_id"]}`),
		Handler:     handleWeakenMemory,
	},
	{
		Name:        "snooze_memory",
		Description: "Temporarily suppress a memory from retrieval. Required: memoryId. Optional: days (integer; default 1 — how many days to suppress before the row reappears in query results). The row stays in the DB; only retrieval is filtered. Use when a memory is technically true but actively distracting in current context. Successive calls RESET the snooze window — calling twice with days=1 produces a total snooze of 1 day, not 2 (use days=N if you need a longer window).",
		Schema:      json.RawMessage(`{"type":"object","properties":{"memory_id":{"type":"string"},"days":{"type":"number","default":1,"description":"Optional. Snooze duration in days; default 1."}},"required":["memory_id"]}`),
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
		Description: "Patch metadata on an existing memory via JSON-Patch (RFC 6902) ops. Required: memoryId. Required: patch (array of JSON-Patch ops). Supported ops: add, remove, replace, move, copy, test. Path uses JSON Pointer syntax (e.g., '/metadata/key', '/tags/0'). Example payload: `{'memoryId':'abc123','patch':[{'op':'replace','path':'/tags','value':['new-tag']}]}`. Use for surgical metadata updates; for weight changes use set_memory_weight; for soft-delete use shred_memory.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"memory_id":{"type":"string"},"patch":{"type":"object"}},"required":["memory_id","patch"]}`),
		Handler:     handlePatchMemory,
	},
	{
		Name:        "promote_memory",
		Description: "Promote a memory to long-term (LTM) status by setting `is_long_term=1`. Required: memoryId. Use when a memory has proven durable value — referenced multiple times across sessions, contains an architectural decision, or has survived validation. The flag exempts the row from standard decay sweeps in `gc_run`. Promotion does NOT bypass eviction logic for lessons (lessons have their own LTM gate). Idempotent: re-promoting an LTM row is a no-op, returns success:true. The promotion is recorded in metadata.promoted_at for forensic tracing.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"memory_id":{"type":"string"}},"required":["memory_id"]}`),
		Handler:     handlePromoteMemory,
	},
	{
		Name:        "review_memories",
		Description: "List memories due for spaced-repetition review.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"limit":{"type":"number","default":20},"days":{"type":"number","default":30,"description":"Optional. Stale window in days; default 30."}}}`),
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
		Description: "Run a lifecycle decay / archive sweep. dry_run=true is the safe default (no writes). The full CLI flag surface (--review, --purge, --shred-negative) stays on `mpm gc` because those modes are operationally distinct.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"dry_run":{"type":"boolean","default":true,"description":"Safe default true. Set false to actually mutate state."},"aggressive":{"type":"boolean","default":false,"description":"Optional. Aggressive sweep."},"max_age_hours":{"type":"number","default":24,"description":"Optional. Max age (hours) for decay eligibility; default 24."}}}`),
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
		Schema:      json.RawMessage(`{"type":"object","properties":{"fact":{"type":"string"},"tags":{"oneOf":[{"type":"string"},{"type":"array","items":{"type":"string"}}],"description":"Tags as a comma-separated string OR a JSON array of strings."},"weight":{"type":"number"},"provenance":{"type":"string"},"confirm":{"type":"boolean"}},"required":["fact","confirm"]}`),
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
		Description: "Schedule a future wake: writes a row to scheduled_wakes. Any subsequent MPM call after target_time surfaces it as WakesPending in the response. Stateless — no daemon, no cron. target_time accepts a unix epoch (seconds), a relative duration ('24h', '30m', '7d', '1d'), or an ISO-8601 timestamp ('2026-07-12T12:00:00Z'). Use theory_id to bind the wake to a pending theory.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"reason":{"type":"string"},"target_time":{"type":"string"},"theory_id":{"type":"string"},"recurring_rule":{"type":"string"},"metadata":{"type":"object"}},"required":["reason","target_time"]}`),
		Handler:     handleScheduleWake,
	},
	{
		// Phase 5a: explicit pull variant of the opportunistic check.
		// Folds any due wakes into the response as WakesPending. Useful
		// at session start or after a passive poll.
		Name:        "check_wakes",
		Description: "Pull due wakes (fired=0 AND target_time<=now), mark them fired, return in WakesPending. Idempotent across concurrent callers. Pass `kinds` to filter by metadata.kind; default is notification-only (backward compatible). Use `kinds: [\"*\"]` to surface every pending wake regardless of kind (cron-injected, system, etc.); use `kinds: [\"notification\", \"cron\"]` for a specific subset.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"kinds":{"type":"array","items":{"type":"string"},"description":"Wake kinds to surface. Default (omitted): notification-only. Pass [\"*\"] for all kinds, or specific kinds like [\"notification\", \"cron\"]. The literal string \"*\" short-circuits to no kind filter."}}}`),
		Handler:     handleCheckWakes,
	},
	{
		// Arc 2: pull incoming epistemic event wakes for the calling
		// session. Mirrors check_wakes but reads from shared.event_wakes
		// instead of scheduled_wakes. The target_session is taken from
		// the ActiveContext (injected by the dispatcher); explicit
		// session_id is allowed for unit tests.
		Name:        "check_pending_event_wakes",
		Description: "Pull all pending event wakes (fired=0) targeting this session, mark them fired, and return them in EventWakesPending. Idempotent across concurrent callers. Use to surface incoming epistemic events (rules, resolutions, arbitrations) without going through the opportunistic fold.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"session_id":{"type":"string","description":"Override target session ID (default: ActiveContext.SessionID)"}}}`),
		Handler:     handleCheckPendingEventWakes,
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
	{
		// Phase 5a: compact wake summary. For the "agent wakes up after
		// a long idle" case — listing every overdue wake individually
		// would blow out context. Digest returns age buckets + top-N
		// most overdue + total counts in ~500 bytes regardless of how
		// many wakes piled up.
		Name:        "digest_wakes",
		Description: "Compact summary of overdue + pending wakes. Returns age-bucket counts, top-N most overdue, and totals. Use at session start after long idle instead of list_wakes to avoid context blowout.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"top_n":{"type":"number","description":"How many top-overdue rows to surface (default 5)"}}}`),
		Handler:     handleDigestWakes,
	},
	{
		// Phase 5b: self-diagnosis surface. PRAGMA integrity +
		// page/freelist stats + domain counts (memories_active,
		// theories_pending, wakes_overdue, evidence_total) +
		// lifetime SQLITE_BUSY retry counter. Use when the agent
		// notices latency, timeouts, or wants to confirm the
		// substrate is healthy before long-running operations.
		Name:        "health_check",
		Description: "Self-diagnosis: SQLite integrity + page stats + domain counts + lifetime SQLITE_BUSY retry counter. Returns one compact payload, no parameters.",
		Schema:      json.RawMessage(`{"type":"object","properties":{}}`),
		Handler:     handleHealthCheck,
	},

	{
		// Phase 2 of the epistemic compaction pipeline. Reflex to the
		// epistemic_pressure trigger on every wake_context: when the
		// agent sees exceeded=true, it calls this tool to drain a
		// bounded batch of raw memories into a durable lesson.
		//
		// Wire-format contract:
		//   - Returns {compacted, lessons_created, raw_marked, lesson_id}
		//     on commit; {skipped_reason} on no-op (no raw / below
		//     threshold).
		//   - Atomicity: lesson insert + raw mark share one transaction.
		//     LLM failure → zero DB writes (no partial state).
		//   - Schema violation (model returns invalid JSON) surfaces
		//     as model_schema_violation error; data plane untouched.
		//
		// force=true bypasses the pressure threshold (rare; mostly for
		// tests). Default is false — the agent respects the gauge.
		Name:        "compact_epistemology",
		Description: "Compact a batch of raw memories into a durable lesson. Reads its own state from the epistemic_pressure view; bails cheaply when there is nothing to compact. Atomic transaction ensures no partial state. force=true bypasses the pressure threshold.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"force":{"type":"boolean","default":false,"description":"Bypass the pressure threshold (rare; mostly for tests)."}}}`),
		Handler:     handleCompactEpistemology,
	},

	// ── Scheduled Tasks (Agentic Cron) ──────────────────────────────
	// Three split tools following the existing wake / lesson pattern
	// (schedule_wake / list_wakes / check_wakes are all separate
	// tools, not a multiplexed CRUD). The agent reads each tool's
	// schema to learn the required fields; no guessing.

	{
		Name:        "upsert_scheduled_task",
		Description: "Create or update a recurring Agentic Cron task. The mpm-scheduler daemon polls scheduled_tasks on a 60s tick loop, injects a standard scheduled_wakes row at each fire, and rolls over next_run_at automatically — you do not need to manually reschedule. Required: id (semantic slug, re-using updates), name (human label), cron_expr (standard 5-field cron, parsed by robfig/cron/v3, e.g. '0 3 * * *' = daily 03:00 UTC), directive_id (the directive the agent reads when it wakes; the handler runs a fail-fast lookup to confirm the directive exists before accepting the upsert — better to catch a typo at 2 PM than have the daemon silently drop the wake at 3 AM), status ('active' or 'paused'). To stop a recurring task without deleting it, call upsert again with status='paused'.",
		Schema:      json.RawMessage(`{"type":"object","required":["id","name","cron_expr","directive_id","status"],"properties":{"id":{"type":"string","description":"Semantic slug (e.g., 'epistemic-compaction'). Re-using an ID updates the existing row."},"name":{"type":"string"},"cron_expr":{"type":"string","description":"Standard 5-field cron expression."},"directive_id":{"type":"string","description":"ID of an existing directive in the 'directives' collection; the handler validates it exists."},"status":{"type":"string","enum":["active","paused"]}}}`),
		Handler:     handleUpsertScheduledTask,
	},
	{
		Name:        "list_scheduled_tasks",
		Description: "List all scheduled tasks ordered by next_run_at ASC. Returns id, name, cron_expr, directive_id, status, last_run_at, next_run_at, created_at, updated_at for each task. Use to inspect what's queued and what fired last.",
		Schema:      json.RawMessage(`{"type":"object","properties":{}}`),
		Handler:     handleListScheduledTasks,
	},
	{
		Name:        "delete_scheduled_task",
		Description: "Hard-delete a scheduled task by id. Most operators should set status='paused' via upsert_scheduled_task instead — paused rows are kept for forensics and re-enableable. Use delete only when you want permanent removal.",
		Schema:      json.RawMessage(`{"type":"object","required":["id"],"properties":{"id":{"type":"string","description":"Semantic slug of the task to delete."}}}`),
		Handler:     handleDeleteScheduledTask,
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