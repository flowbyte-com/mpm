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
Do not use when: the information is ephemeral working context (use mpm_scratchpad instead); you are making a commitment or tracking work (use mpm_work instead).`,
		Schema: json.RawMessage(`{"type":"object","properties":{"action":{"type":"string","enum":["save","query","shred","reinforce","weaken","snooze","set_weight","patch","promote","review","synthesize","challenge","commit_milestone"]},"params":{"type":"object","properties":{"projection":{"type":"boolean","description":"Phase 2B: return summary+pointer+retrieval_metadata instead of full content"}},"additionalProperties":true}},"required":["action"]}`),
		Handler: handleMpmMemory,
	},
	{
		Name: "mpm_theories",
		Description: `Hypothesis management with explicit validation criteria and resolution.
Use when: you form a hypothesis about causality ("I think the FTS tokenizer is producing different results on this OS") and can define a concrete test that would confirm or disprove it. Theories are not guesses — they are testable claims with defined success conditions.
Do not use when: you just want to remember something (mpm_memory); you have a confirmed decision (mpm_decisions).`,
		Schema: json.RawMessage(`{"type":"object","properties":{"action":{"type":"string","enum":["propose","resolve"]},"params":{"type":"object","additionalProperties":true}},"required":["action"]}`),
		Handler: handleMpmTheories,
	},
	{
		Name: "mpm_decisions",
		Description: `Immutable record of architectural choices and the reasoning behind them.
Use when: you make a choice between approaches ("we chose SQLite WAL mode over DELETE journal for concurrent access") and want to preserve the rationale so future-you understands why, even when the alternative is no longer fresh in context.
Do not use when: the choice is trivial or easily reversible; you just want to store a fact (mpm_memory).`,
		Schema: json.RawMessage(`{"type":"object","properties":{"action":{"type":"string","enum":["record"]},"params":{"type":"object","additionalProperties":true}},"required":["action"]}`),
		Handler: handleMpmDecisions,
	},
	{
		Name: "mpm_lessons",
		Description: `Durable lessons from failures, anti-patterns, and hard-won insights.
Use when: something failed and you want to make sure the system never repeats the same mistake; you encounter an unexpected success and want to record why it worked; you want to tag a memory as a "warning" or "practice" so it surfaces in future relevant contexts.
Do not use when: you are documenting a decision (mpm_decisions) or forming a testable hypothesis (mpm_theories).`,
		Schema: json.RawMessage(`{"type":"object","properties":{"action":{"type":"string","enum":["save","search","list"]},"params":{"type":"object","properties":{"projection":{"type":"boolean","description":"Phase 2D: return summary+pointer+retrieval_metadata instead of full content"}},"additionalProperties":true}},"required":["action"]}`),
		Handler: handleMpmLessons,
	},
	{
		Name: "mpm_topics",
		Description: `Topic labels for clustering related memories and organizing knowledge.
Use when: you want to link multiple memories under a shared theme ("UN-system", "Qatar-Mission", "eCryptfs-boot-order") so future queries can surface the full cluster with a single term.
Do not use when: you just want to store a single fact (mpm_memory save); you need to track work (mpm_work).`,
		Schema: json.RawMessage(`{"type":"object","properties":{"action":{"type":"string","enum":["create","search","link"]},"params":{"type":"object","additionalProperties":true}},"required":["action"]}`),
		Handler: handleMpmTopics,
	},
	{
		Name: "mpm_references",
		Description: `Ingested external documents: PDFs, specs, whitepapers, books.
Use when: you read an external document and want to make its contents searchable via the MPM query surface. References are indexed and queryable but not automatically retrieved — you search them explicitly.
Do not use when: the document is ephemeral or you just want to save a URL to visit later (mpm_memory save).`,
		Schema: json.RawMessage(`{"type":"object","properties":{"action":{"type":"string","enum":["add","search","list"]},"params":{"type":"object","additionalProperties":true}},"required":["action"]}`),
		Handler: handleMpmReferences,
	},
	{
		Name: "mpm_evidence",
		Description: `Attach observations, test results, or external references as evidence to any artifact.
Use when: you want to substantiate a memory, theory, or decision with a concrete observation ("tested on 3 machines, same result"), a test output, a reproduction case, or an external source.
Do not use when: you are storing raw facts without evidentiary context (mpm_memory save). Evidence gives artifacts weight — use it when the truth of something is important enough to require proof.`,
		Schema: json.RawMessage(`{"type":"object","properties":{"action":{"type":"string","enum":["add","list"]},"params":{"type":"object","additionalProperties":true}},"required":["action"]}`),
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
		Description: `Agent session state, mode routing, and directive management.
Use when: you need to understand the current agent mode/persona; you want to trigger a mode or persona switch based on task context; you need to read active behavioral directives governing the current session; you want to query the proactive recall hint for conversation-relevant memories.
Route is especially useful: give it a user prompt and it returns the best-matching mode(s) and persona with scoring.`,
		Schema: json.RawMessage(`{"type":"object","properties":{"action":{"type":"string","enum":["read_wake_context","read_directives","proactive_recall_hint","query_global_rules","record_global_rule","promote_to_global","route"]},"params":{"type":"object","additionalProperties":true}},"required":["action"]}`),
		Handler: handleMpmContext,
	},
	{
		Name: "mpm_skills",
		Description: `Reusable procedural knowledge stored as markdown with YAML frontmatter.
Use when: you develop a workflow that works well and want to固化 it as a persistent skill that can be listed, read by name, and reused across sessions without re-inventing the procedure. The save action requires content (the skill markdown body) and name. Skills are versioned and can be shared globally or kept local to this workstation.`,
		Schema: json.RawMessage(`{"type":"object","properties":{"action":{"type":"string","enum":["save","read","list","delete","promote_to_global"]},"params":{"type":"object","additionalProperties":true}},"required":["action"]}`),
		Handler: handleMpmSkills,
	},
	{
		Name: "mpm_wakes",
		Description: `Deferred work triggers scheduled for future execution.
Use when: you need to schedule a check-in, reminder, or follow-up task to fire automatically at a specific time without the agent running continuously. Wakes survive agent restarts — the scheduler fires them regardless of what session is active.
Tasks (upsert_task) are recurring cron-style triggers; one-shot wakes (schedule) fire once and are marked fired.`,
		Schema: json.RawMessage(`{"type":"object","properties":{"action":{"type":"string","enum":["schedule","check","check_pending_event","list","digest","upsert_task","list_tasks","delete_task"]},"params":{"type":"object","additionalProperties":true}},"required":["action"]}`),
		Handler: handleMpmWakes,
	},

	// ── Handoff primitive (inter-session communication) ──────────────────

	{
		Name: "mpm_handoff",
		Description: `Inter-session communication: write, read, and audit handoff records.
Use when: you are ending a session and need to leave a summary for the next session to pick up. The handoff record is the bridge between two distinct agent shifts — it carries the session summary, not the work itself.
Note: intentions must be expressed as Work items (mpm_work create) and open questions as Theories (mpm_theories propose). The handoff carries only a prose summary. Hard schema rejection enforces this boundary — if you try to pass commitments or open_questions here, the call fails with a validation error.`,
		Schema: json.RawMessage(`{"type":"object","properties":{"action":{"type":"string","enum":["write","read","list","shred"]},"params":{"type":"object","properties":{"session_id":{"type":"string"},"summary":{"type":"string"},"state":{"type":"string","enum":["clean","crashed","interrupted","force_end"]},"note":{"type":"string"},"unread":{"type":"boolean"},"mark_read":{"type":"boolean"},"limit":{"type":"number"},"handoff_id":{"type":"string"},"confirm":{"type":"boolean"}},"additionalProperties":false}},"required":["action"]}`),
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
Use when: you need to run a lifecycle decay sweep (gc_run); compact raw memories into lessons (compact); check SQLite integrity (health_check); audit the anomaly ledger (query_audit_log); manage or dismiss audit clusters; list active audit clusters (list_clusters).
This tool is for system health — not for daily agent work. Prefer specific tools for regular operations.`,
		Schema: json.RawMessage(`{"type":"object","properties":{"action":{"type":"string","enum":["gc_run","compact","health_check","migrate","query_audit_log","list_clusters","snooze_cluster","resolve_cluster","annotate_cluster"]},"params":{"type":"object","additionalProperties":true}},"required":["action"]}`),
		Handler: handleMpmSystem,
	},
	{
		Name:        "log_to_changelog",
		Description: `Self-report agent work as a structured changelog entry tied to a git commit SHA.
Use when: you have completed a meaningful unit of work (a fix, a feature, a refactor) and want to record it in the project changelog with a reference to the commit that shipped it. The changelog entry is permanent and queryable.`,
		Schema:      json.RawMessage(`{"type":"object","properties":{"fact":{"type":"string"},"commit_hash":{"type":"string"},"tags":{"oneOf":[{"type":"string"},{"type":"array","items":{"type":"string"}}],"description":"Tags as a comma-separated string OR a JSON array of strings."}},"required":["fact","commit_hash"]}`),
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
Do not use when: you just want to store a fact or insight (mpm_memory save).`,
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
