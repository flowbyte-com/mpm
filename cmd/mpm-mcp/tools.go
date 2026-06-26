// tools.go — Single source of truth for the MCP tool surface.
//
// One tool spec + one handler per tool, registered via RegisterAllTools
// below. The list of s.AddTool(...) calls is the canonical count — any
// prose claim about "N tools" is decoration and will drift. All handlers
// are thin shims: extract args via type assertion, call a dm method,
// wrap the result. The dm methods live in internal/call_helpers.go and
// back both this MCP server and the `mpm call <tool>` CLI (cmd/mpm/call.go).
//
// Descriptions and arg schemas are copied from
// agent-plugins/opencode-mpm-plugin/src/index.ts (the OpenClaw plugin's tool surface).
// The Python plugin in .claude/mpm-mcp/server.py has a smaller subset
// (read_wake_context, read_directives, propose_theory, resolve_theory,
// record_decision, proactive_recall_hint) which is fully covered here.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"mpm/internal"
)

const emptyWakeContext = "Wake context is empty. Ready for context."

// RegisterAllTools registers every MPM tool on the given MCP server.
// The list of s.AddTool(...) calls inside is the canonical count of
// the MCP tool surface — do not duplicate that count in prose, it
// will drift on the next tool add.
//
// dm must be a long-lived DatabaseManager (the caller owns its Close).
// ac carries the active mode/persona read from MPM_ACTIVE_MODE /
// MPM_ACTIVE_PERSONA env vars; write handlers thread it into provenance
// metadata so every persisted row is attributable to the agent context.
// router provides zero-latency heuristic routing for the route tool.
func RegisterAllTools(s *server.MCPServer, dm *internal.DatabaseManager, ac internal.ActiveContext, router *internal.Router) {
	s.AddTool(toolReadWakeContext(), handleReadWakeContext(dm))
	s.AddTool(toolQueryLongTermMemory(), handleQueryLongTermMemory(dm))
	s.AddTool(toolSaveToMemory(), handleSaveToMemory(dm, ac))
	s.AddTool(toolChallengeMemory(), handleChallengeMemory(dm))
	s.AddTool(toolSaveLesson(), handleSaveLesson(dm))
	s.AddTool(toolSearchLessons(), handleSearchLessons(dm))
	s.AddTool(toolListLessons(), handleListLessons(dm))
	s.AddTool(toolCreateTopic(), handleCreateTopic(dm))
	s.AddTool(toolSearchTopics(), handleSearchTopics(dm))
	s.AddTool(toolLinkTopic(), handleLinkTopic(dm))
	s.AddTool(toolAddReference(), handleAddReference(dm))
	s.AddTool(toolSearchReferences(), handleSearchReferences(dm))
	s.AddTool(toolListReferences(), handleListReferences(dm))
	s.AddTool(toolReadDirectives(), handleReadDirectives(dm))
	s.AddTool(toolProposeTheory(), handleProposeTheory(dm))
	s.AddTool(toolResolveTheory(), handleResolveTheory(dm))
	s.AddTool(toolRecordDecision(), handleRecordDecision(dm, ac))
	s.AddTool(toolProactiveRecallHint(), handleProactiveRecallHint(dm))
	s.AddTool(toolRoute(), handleRoute(router))
	s.AddTool(toolLogToChangelog(), handleLogToChangelog(dm))
	s.AddTool(toolAddEvidence(), handleAddEvidence(dm))
	s.AddTool(toolListEvidence(), handleListEvidence(dm))
	s.AddTool(toolQueryConfidenceHistory(), handleQueryConfidenceHistory(dm))
	s.AddTool(toolQueryConfidenceChanges(), handleQueryConfidenceChanges(dm))
	s.AddTool(toolQueryConfidenceTrend(), handleQueryConfidenceTrend(dm))
	s.AddTool(toolQueryMemoryQuality(), handleQueryMemoryQuality(dm))
	s.AddTool(toolShowConfidence(), handleShowConfidence(dm))
	s.AddTool(toolRecomputeConfidence(), handleRecomputeConfidence(dm))
	s.AddTool(toolExplainConfidence(), handleExplainConfidence(dm))

	// Memory feedback / mutation tools (added 2026-06-26 — close the
	// agent feedback loop so agents don't have to shell out to `mpm
	// reinforce <id>` and parse text output).
	s.AddTool(toolShredMemory(), handleShredMemory(dm))
	s.AddTool(toolReinforceMemory(), handleReinforceMemory(dm))
	s.AddTool(toolWeakenMemory(), handleWeakenMemory(dm))
	s.AddTool(toolSnoozeMemory(), handleSnoozeMemory(dm))
	s.AddTool(toolSetMemoryWeight(), handleSetMemoryWeight(dm))
	s.AddTool(toolPatchMemory(), handlePatchMemory(dm))
	s.AddTool(toolPromoteMemory(), handlePromoteMemory(dm))

	// Workflow tools (Tier 2 — added 2026-06-26)
	s.AddTool(toolReviewMemories(), handleReviewMemories(dm))
	s.AddTool(toolSynthesizeMemory(), handleSynthesizeMemory(dm))

	// Workflow tools (Tier 3 — added 2026-06-26)
	s.AddTool(toolGCRun(), handleGCRun(dm))
}

// jsonResult marshals v to JSON and wraps it in an mcp text result. Errors
// during marshalling fall back to a quoted string so the handler still
// returns a useful response.
func jsonResult(v interface{}) *mcp.CallToolResult {
	b, err := json.Marshal(v)
	if err != nil {
		return mcp.NewToolResultText(fmt.Sprintf("%q", fmt.Sprintf("%v", v)))
	}
	return mcp.NewToolResultText(string(b))
}

// ── Tool specs ─────────────────────────────────────────────────────────────

func toolReadWakeContext() mcp.Tool {
	return mcp.NewTool("read_wake_context",
		mcp.WithDescription(
			"Read the agent's wake context — session state from the previous session: "+
				"active mode, persona, recent topics, and recent memories. "+
				"Call this on session start to understand where you left off."),
	)
}

func toolQueryLongTermMemory() mcp.Tool {
	return mcp.NewTool("query_long_term_memory",
		mcp.WithDescription(
			"Search MPM long-term memory. Before answering anything about prior work, decisions, dates, people, preferences, or todos — run this first. Returns matching memories as JSON."),
		mcp.WithString("query",
			mcp.Required(),
			mcp.Description("Natural language search query for long-term memory."),
		),
		mcp.WithNumber("limit",
			mcp.DefaultNumber(5),
			mcp.Description("Maximum number of results to return."),
		),
		mcp.WithString("collection",
			mcp.Description("Optional collection name to scope the search."),
		),
	)
}

func toolSaveToMemory() mcp.Tool {
	return mcp.NewTool("save_to_memory",
		mcp.WithDescription(
			"Persist a fact, lesson, or decision to MPM long-term memory. After any non-trivial action, lesson learned, or decision — call this. Tags help later retrieval. Weight 0.5 by default; higher for important truths. Use TTL '24h' for session-scoped facts, '0' for permanent."),
		mcp.WithString("fact",
			mcp.Required(),
			mcp.Description("The fact, lesson, or decision to persist."),
		),
		mcp.WithString("tags",
			mcp.Description("Comma-separated tags for categorization."),
		),
		mcp.WithNumber("weight",
			mcp.DefaultNumber(0.5),
			mcp.Description("Importance weight between 0 and 1."),
		),
		mcp.WithString("ttl",
			mcp.Description("TTL: '24h' for session-scoped, '0' for permanent, or Go duration."),
		),
		mcp.WithString("collection",
			mcp.DefaultString("memories"),
			mcp.Description("Collection name."),
		),
	)
}

func toolChallengeMemory() mcp.Tool {
	return mcp.NewTool("challenge_memory",
		mcp.WithDescription(
			"Challenge an existing memory with contradictory evidence. Weakens the memory, creates a pending theory, and logs a decision. Use when conversation or test results contradict a stored memory."),
		mcp.WithString("memoryId",
			mcp.Required(),
			mcp.Description("The MPM memory ID to challenge."),
		),
		mcp.WithString("evidence",
			mcp.Required(),
			mcp.Description("Evidence contradicting the memory — specific details about what changed."),
		),
	)
}

func toolSaveLesson() mcp.Tool {
	return mcp.NewTool("save_lesson",
		mcp.WithDescription(
			"Persist a lesson to MPM — what was learned, observed, or should be remembered. Types: 'warning' (don't do X), 'practice' (do Y), 'insight' (X leads to Y)."),
		mcp.WithString("fact",
			mcp.Required(),
			mcp.Description("The lesson content."),
		),
		mcp.WithString("type",
			mcp.Enum("warning", "practice", "insight"),
			mcp.DefaultString("insight"),
			mcp.Description("Lesson type."),
		),
		mcp.WithString("tags",
			mcp.Description("Comma-separated optional tags for the lesson."),
		),
	)
}

func toolSearchLessons() mcp.Tool {
	return mcp.NewTool("search_lessons",
		mcp.WithDescription(
			"Search MPM lessons for relevant learned knowledge. Use to recall warnings, best practices, and insights before acting."),
		mcp.WithString("query",
			mcp.Required(),
			mcp.Description("Search query for lesson content."),
		),
	)
}

func toolListLessons() mcp.Tool {
	return mcp.NewTool("list_lessons",
		mcp.WithDescription(
			"List all lessons in MPM, optionally filtered by type: 'warning', 'practice', 'insight'."),
		mcp.WithString("type",
			mcp.Enum("warning", "practice", "insight"),
			mcp.Description("Filter by lesson type."),
		),
	)
}

func toolCreateTopic() mcp.Tool {
	return mcp.NewTool("create_topic",
		mcp.WithDescription(
			"Create a topic in MPM to organize related memories and knowledge. Topics group related memories and can be searched."),
		mcp.WithString("name",
			mcp.Required(),
			mcp.Description("Topic name."),
		),
		mcp.WithString("description",
			mcp.Description("Optional topic description."),
		),
	)
}

func toolSearchTopics() mcp.Tool {
	return mcp.NewTool("search_topics",
		mcp.WithDescription(
			"Search MPM topics for relevant knowledge clusters. Topics group related memories and provide context."),
		mcp.WithString("query",
			mcp.Description("Search query for topics."),
		),
		mcp.WithNumber("limit",
			mcp.DefaultNumber(20),
			mcp.Description("Maximum number of results."),
		),
	)
}

func toolLinkTopic() mcp.Tool {
	return mcp.NewTool("link_topic",
		mcp.WithDescription(
			"Link an existing memory to an existing topic. Both must already exist."),
		mcp.WithString("memory_id",
			mcp.Required(),
			mcp.Description("The memory ID to link."),
		),
		mcp.WithString("topic_id",
			mcp.Required(),
			mcp.Description("The topic ID to link to."),
		),
	)
}

func toolAddReference() mcp.Tool {
	return mcp.NewTool("add_reference",
		mcp.WithDescription(
			"Ingest a document as a reference into MPM. Supported: .txt, .md, .html, .epub, .pdf. The document is chunked and indexed for semantic search."),
		mcp.WithString("filepath",
			mcp.Required(),
			mcp.Description("Path to the file to ingest."),
		),
		mcp.WithString("title",
			mcp.Description("Optional title override."),
		),
	)
}

func toolSearchReferences() mcp.Tool {
	return mcp.NewTool("search_references",
		mcp.WithDescription(
			"Search content within ingested reference documents. Returns matching chunks from the reference library."),
		mcp.WithString("query",
			mcp.Required(),
			mcp.Description("Search query for reference content."),
		),
		mcp.WithNumber("limit",
			mcp.DefaultNumber(5),
			mcp.Description("Maximum number of results."),
		),
	)
}

func toolListReferences() mcp.Tool {
	return mcp.NewTool("list_references",
		mcp.WithDescription(
			"List all ingested reference documents in MPM. Shows titles, chunk counts, and tags."),
		mcp.WithNumber("limit",
			mcp.DefaultNumber(50),
			mcp.Description("Maximum number of results."),
		),
		mcp.WithNumber("offset",
			mcp.DefaultNumber(0),
			mcp.Description("Pagination offset."),
		),
	)
}

func toolReadDirectives() mcp.Tool {
	return mcp.NewTool("read_directives",
		mcp.WithDescription(
			"Read the agent's prime directives — behavioral rules and operating principles that define what the agent must and must not do."),
	)
}

func toolProposeTheory() mcp.Tool {
	return mcp.NewTool("propose_theory",
		mcp.WithDescription(
			"Log a hypothesis about causality before writing a fix. When you think 'X is probably causing Y' — propose it, define the test, then run the test. Writing validation criteria forces specificity and often collapses false hypotheses early."),
		mcp.WithString("hypothesis",
			mcp.Required(),
			mcp.Description("The hypothesis or assumption."),
		),
		mcp.WithString("validationCriteria",
			mcp.Required(),
			mcp.Description("A specific, executable test or observation that would prove or disprove the hypothesis."),
		),
		mcp.WithString("tags",
			mcp.Description("Comma-separated optional tags."),
		),
	)
}

func toolResolveTheory() mcp.Tool {
	return mcp.NewTool("resolve_theory",
		mcp.WithDescription(
			"Close the loop on a pending theory after running its validation criteria. If proven, save as permanent memory. If disproven, record what actually caused the problem instead."),
		mcp.WithString("theoryId",
			mcp.Required(),
			mcp.Description("The theory ID to resolve."),
		),
		mcp.WithString("conclusion",
			mcp.Required(),
			mcp.Description("What was concluded after running validation."),
		),
		mcp.WithString("newStatus",
			mcp.Required(),
			mcp.Enum("proven", "disproven"),
			mcp.Description("Whether the hypothesis was proven or disproven."),
		),
	)
}

func toolRecordDecision() mcp.Tool {
	return mcp.NewTool("record_decision",
		mcp.WithDescription(
			"Record an architectural decision, library choice, or any moment where you chose path A over path B. The rationale is the most important field — it makes past decisions reusable weeks later."),
		mcp.WithString("context",
			mcp.Required(),
			mcp.Description("The situation or problem requiring a decision."),
		),
		mcp.WithString("choice",
			mcp.Required(),
			mcp.Description("What was decided."),
		),
		mcp.WithString("rationale",
			mcp.Required(),
			mcp.Description("Why this path was chosen over alternatives."),
		),
		mcp.WithString("outcome",
			mcp.Description("What happened when the decision was executed."),
		),
		mcp.WithString("tags",
			mcp.Description("Comma-separated optional tags."),
		),
		mcp.WithNumber("weight",
			mcp.DefaultNumber(0.5),
			mcp.Description("Decision weight."),
		),
	)
}

// ── route ─────────────────────────────────────────────────────────────────────

func toolRoute() mcp.Tool {
	return mcp.NewTool("route",
		mcp.WithDescription(
			"Evaluate a user prompt and auto-select the best-matching MPM mode(s) "+
				"and persona. Modes use threshold filtering (multiple can activate); "+
				"personas use max-pooling (only the highest scorer wins, if any beats threshold 1). "+
				"Patterns are pre-compiled at server boot. Anti-patterns penalize false positives. "+
				"Returns a full diagnostic report with scores and triggers per component."),
		mcp.WithString("prompt",
			mcp.Required(),
			mcp.Description("The user prompt or message to route."),
		),
	)
}

func handleRoute(router *internal.Router) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		prompt, _ := args["prompt"].(string)
		if prompt == "" {
			return mcp.NewToolResultError("prompt is required"), nil
		}
		report := router.Evaluate(prompt)
		return jsonResult(report), nil
	}
}

// ── proactive_recall_hint ────────────────────────────────────────────────────

func toolProactiveRecallHint() mcp.Tool {
	return mcp.NewTool("proactive_recall_hint",
		mcp.WithDescription(
			"Check recent conversation context for overlap with decisions and theories. Returns structured hints if semantic matches are found. Call this after each user message — surface only the top hint."),
		mcp.WithString("conversation_text",
			mcp.Required(),
			mcp.Description("Recent conversation context to check for relevant decisions and theories."),
		),
		mcp.WithNumber("max_hints",
			mcp.DefaultNumber(3),
			mcp.Description("Maximum hint results."),
		),
		mcp.WithNumber("min_score",
			mcp.DefaultNumber(-3.0),
			mcp.Description("Minimum BM25 score threshold (lower = stronger match)."),
		),
	)
}

// ── Handlers ───────────────────────────────────────────────────────────────

// handleReadWakeContext returns the pre-formatted wake context string from
// internal.ReadWakeContext. Empty wake context (no prior session) returns a
// fixed readiness message so the caller can distinguish "nothing to resume"
// from an error.
func handleReadWakeContext(dm *internal.DatabaseManager) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		s, err := dm.ReadWakeContext()
		if err != nil {
			return mcp.NewToolResultErrorFromErr("read_wake_context failed", err), nil
		}
		if s == "" {
			return mcp.NewToolResultText(emptyWakeContext), nil
		}
		return mcp.NewToolResultText(s), nil
	}
}

func handleQueryLongTermMemory(dm *internal.DatabaseManager) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		query, _ := args["query"].(string)
		if query == "" {
			return mcp.NewToolResultError("query is required"), nil
		}
		limit := int(parseNum(args["limit"], 5))
		if limit <= 0 {
			limit = 5
		}
		collection, _ := args["collection"].(string)

		items, err := dm.HybridSearchMemories(query, collection, limit)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("query_long_term_memory failed", err), nil
		}
		return jsonResult(map[string]interface{}{
			"success":  true,
			"memories": items,
			"count":    len(items),
		}), nil
	}
}

func handleSaveToMemory(dm *internal.DatabaseManager, ac internal.ActiveContext) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		fact, _ := args["fact"].(string)
		if fact == "" {
			return mcp.NewToolResultError("fact is required"), nil
		}
		out, _, err := dm.SaveMemoryWithContext(
			fact,
			defaultString(args["collection"], "memories"),
			parseStringSliceArg(args["tags"]),
			parseNum(args["weight"], 0.5),
			stringArg(args["ttl"]),
			ac,
		)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("save_to_memory failed", err), nil
		}
		return jsonResult(out), nil
	}
}

func handleChallengeMemory(dm *internal.DatabaseManager) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		memoryID, _ := args["memoryId"].(string)
		if memoryID == "" {
			return mcp.NewToolResultError("memoryId is required"), nil
		}
		evidence, _ := args["evidence"].(string)
		out, err := dm.ChallengeMemoryWithTheory(memoryID, evidence)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("challenge_memory failed", err), nil
		}
		return jsonResult(out), nil
	}
}

func handleSaveLesson(dm *internal.DatabaseManager) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		fact, _ := args["fact"].(string)
		if fact == "" {
			return mcp.NewToolResultError("fact is required"), nil
		}
		lessonType := defaultString(args["type"], "insight")
		tags := parseStringSliceArg(args["tags"])

		out, _, err := dm.SaveLesson(fact, lessonType, tags)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("save_lesson failed", err), nil
		}
		return jsonResult(out), nil
	}
}

func handleSearchLessons(dm *internal.DatabaseManager) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		query, _ := args["query"].(string)
		if query == "" {
			return mcp.NewToolResultError("query is required"), nil
		}
		items, err := dm.SearchLessonsLimited(query)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("search_lessons failed", err), nil
		}
		return jsonResult(map[string]interface{}{
			"success": true,
			"results": items,
			"count":   len(items),
		}), nil
	}
}

func handleListLessons(dm *internal.DatabaseManager) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		lessonType, _ := args["type"].(string)
		items, err := dm.ListLessonsFiltered(lessonType)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("list_lessons failed", err), nil
		}
		return jsonResult(map[string]interface{}{
			"success": true,
			"lessons": items,
			"count":   len(items),
		}), nil
	}
}

func handleCreateTopic(dm *internal.DatabaseManager) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		name, _ := args["name"].(string)
		if name == "" {
			return mcp.NewToolResultError("name is required"), nil
		}
		description, _ := args["description"].(string)

		topicID, err := dm.CreateTopicWithDescription(name, description)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("create_topic failed", err), nil
		}
		return jsonResult(map[string]interface{}{
			"success": true,
			"id":      topicID,
			"name":    name,
		}), nil
	}
}

func handleSearchTopics(dm *internal.DatabaseManager) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		query, _ := args["query"].(string)
		limit := int(parseNum(args["limit"], 20))
		if limit <= 0 {
			limit = 20
		}
		items, err := dm.SearchTopicsByQuery(query, limit)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("search_topics failed", err), nil
		}
		return jsonResult(map[string]interface{}{
			"success": true,
			"results": items,
			"count":   len(items),
		}), nil
	}
}

func handleLinkTopic(dm *internal.DatabaseManager) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		memoryID, _ := args["memory_id"].(string)
		if memoryID == "" {
			return mcp.NewToolResultError("memory_id is required"), nil
		}
		topicID, _ := args["topic_id"].(string)
		if topicID == "" {
			return mcp.NewToolResultError("topic_id is required"), nil
		}
		if err := dm.AddMemoryToTopic(memoryID, topicID, "manual"); err != nil {
			return mcp.NewToolResultErrorFromErr("link_topic failed", err), nil
		}
		return jsonResult(map[string]interface{}{
			"success":   true,
			"memory_id": memoryID,
			"topic_id":  topicID,
		}), nil
	}
}

func handleAddReference(dm *internal.DatabaseManager) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		filepath, _ := args["filepath"].(string)
		if filepath == "" {
			return mcp.NewToolResultError("filepath is required"), nil
		}
		title, _ := args["title"].(string)
		out, err := dm.AddReferenceFromFile(filepath, title)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("add_reference failed", err), nil
		}
		return jsonResult(out), nil
	}
}

func handleSearchReferences(dm *internal.DatabaseManager) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		query, _ := args["query"].(string)
		if query == "" {
			return mcp.NewToolResultError("query is required"), nil
		}
		limit := int(parseNum(args["limit"], 5))
		if limit <= 0 {
			limit = 5
		}
		rows, err := dm.SearchReferenceChunks(query, limit)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("search_references failed", err), nil
		}
		items := make([]map[string]interface{}, 0, len(rows))
		for _, r := range rows {
			items = append(items, map[string]interface{}{
				"id":          r["id"],
				"doc_title":   r["doc_title"],
				"chunk_index": r["chunk_index"],
				"content":     r["content"],
			})
		}
		return jsonResult(map[string]interface{}{
			"success": true,
			"results": items,
			"count":   len(items),
		}), nil
	}
}

func handleListReferences(dm *internal.DatabaseManager) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		limit := int(parseNum(args["limit"], 50))
		if limit <= 0 {
			limit = 50
		}
		offset := int(parseNum(args["offset"], 0))
		if offset < 0 {
			offset = 0
		}
		refs, err := dm.ListReferences(limit, offset)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("list_references failed", err), nil
		}
		return jsonResult(map[string]interface{}{
			"success":    true,
			"references": refs,
			"count":      len(refs),
		}), nil
	}
}

func handleReadDirectives(dm *internal.DatabaseManager) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		directives, err := dm.ReadDirectives()
		if err != nil {
			return mcp.NewToolResultErrorFromErr("read_directives failed", err), nil
		}
		if len(directives) == 0 {
			return mcp.NewToolResultText("(no directives defined)"), nil
		}
		// Group by collection (typically "directives") and format like the
		// opencode plugin: **GROUP** header followed by • entries.
		groups := map[string][]map[string]interface{}{}
		order := []string{}
		for _, d := range directives {
			key := "directive"
			if c, ok := d["collection"].(string); ok && c != "" {
				key = c
			} else if d != nil {
				if coll, ok := d["metadata"].(map[string]interface{}); ok {
					if g, ok := coll["group"].(string); ok && g != "" {
						key = g
					}
				}
			}
			if _, ok := groups[key]; !ok {
				order = append(order, key)
			}
			groups[key] = append(groups[key], d)
		}
		var lines []string
		for _, key := range order {
			lines = append(lines, "**"+strings.ToUpper(key)+"**")
			for _, d := range groups[key] {
				content, _ := d["content"].(string)
				lines = append(lines, "  - "+content)
				if created, ok := d["created_at"].(string); ok && len(created) >= 10 {
					lines = append(lines, "    - added "+created[:10])
				}
			}
		}
		return mcp.NewToolResultText(strings.Join(lines, "\n")), nil
	}
}

func handleProposeTheory(dm *internal.DatabaseManager) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		hypothesis, _ := args["hypothesis"].(string)
		if hypothesis == "" {
			return mcp.NewToolResultError("hypothesis is required"), nil
		}
		validationCriteria, _ := args["validationCriteria"].(string)
		tags := parseStringSliceArg(args["tags"])
		if tags == nil {
			tags = []string{}
		}
		out, err := dm.ProposeTheory(hypothesis, validationCriteria, tags)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("propose_theory failed", err), nil
		}
		return jsonResult(out), nil
	}
}

func handleResolveTheory(dm *internal.DatabaseManager) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		theoryID, _ := args["theoryId"].(string)
		if theoryID == "" {
			return mcp.NewToolResultError("theoryId is required"), nil
		}
		conclusion, _ := args["conclusion"].(string)
		if conclusion == "" {
			return mcp.NewToolResultError("conclusion is required"), nil
		}
		newStatus, _ := args["newStatus"].(string)
		if newStatus == "" {
			return mcp.NewToolResultError("newStatus is required"), nil
		}
		out, err := dm.ResolveTheory(theoryID, conclusion, newStatus)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("resolve_theory failed", err), nil
		}
		return jsonResult(out), nil
	}
}

func handleRecordDecision(dm *internal.DatabaseManager, ac internal.ActiveContext) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		choice, _ := args["choice"].(string)
		if choice == "" {
			return mcp.NewToolResultError("choice is required"), nil
		}
		tags := parseStringSliceArg(args["tags"])
		if tags == nil {
			tags = []string{}
		}
		out, err := dm.RecordDecision(
			stringArg(args["context"]),
			choice,
			stringArg(args["rationale"]),
			stringArg(args["outcome"]),
			tags,
			ac,
		)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("record_decision failed", err), nil
		}
		return jsonResult(out), nil
	}
}

func handleProactiveRecallHint(dm *internal.DatabaseManager) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		conversationText, _ := args["conversation_text"].(string)
		if conversationText == "" {
			return mcp.NewToolResultError("conversation_text is required"), nil
		}
		maxHints := int(parseNum(args["max_hints"], 3))
		if maxHints <= 0 {
			maxHints = 3
		}
		minScore := parseNum(args["min_score"], -3.0)
		hints, err := dm.ProactiveRecallHint(conversationText, maxHints, minScore)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("proactive_recall_hint failed", err), nil
		}
		return jsonResult(map[string]interface{}{
			"success": true,
			"hints":   hints,
			"count":   len(hints),
		}), nil
	}
}

// ── Arg helpers ────────────────────────────────────────────────────────────

// parseNum returns a numeric value as float64, falling back to def.
func parseNum(v interface{}, def float64) float64 {
	if v == nil {
		return def
	}
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case int64:
		return float64(n)
	}
	return def
}

// stringArg returns the string value of v (empty string if not a string).
func stringArg(v interface{}) string {
	s, _ := v.(string)
	return s
}

// defaultString returns s (if non-string) or defaultStr.
func defaultString(v interface{}, defaultStr string) string {
	if s, ok := v.(string); ok && s != "" {
		return s
	}
	return defaultStr
}

// parseStringSliceArg accepts a string (comma-separated) or []interface{}
// (each must be a string). Returns nil for any other shape or empty input.
// The opencode plugin emits arrays; many clients flatten to CSV when the
// tool schema is a string — accept both shapes.
func parseStringSliceArg(v interface{}) []string {
	switch t := v.(type) {
	case nil:
		return nil
	case string:
		if t == "" {
			return nil
		}
		parts := strings.Split(t, ",")
		out := make([]string, 0, len(parts))
		for _, p := range parts {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
		return out
	case []interface{}:
		out := make([]string, 0, len(t))
		for _, x := range t {
			if s, ok := x.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	case []string:
		return t
	}
	return nil
}

// log_to_changelog — agent self-reporting primitive.
//
// Call this when an agent finishes a feature arc and wants its
// work captured in the changelog as a first-class memory. The
// synthesis engine (built next) reconciles these memories with the
// git log to produce a human-facing CHANGELOG.md that includes
// both the commit-level facts and the agent's prose narrative.
//
// Strict retrospective contract: commit_hash is REQUIRED. The tool
// will reject a call without one. This is by design — the schema
// join (commit_hash, mpm_memory_id) is one-to-one, and orphan
// entries would silently break the synthesis engine's join. Run
// `git rev-parse HEAD` (or equivalent) to get the canonical 40-char
// SHA-1; short hashes and refs are rejected.
func toolLogToChangelog() mcp.Tool {
	return mcp.NewTool("log_to_changelog",
		mcp.WithDescription(
			"Log a changelog entry tied to a specific git commit. Use after finishing a "+
				"feature arc to record the 'why this matters' prose that the git log "+
				"cannot provide. The fact, commit_hash, and tags are persisted as a "+
				"memory row tagged #changelog and #commit:<hash>. The synthesis engine "+
				"reconciles these memories with the git log to produce a unified "+
				"CHANGELOG.md. Required: commit_hash (full 40-char SHA-1)."),
		mcp.WithString("fact",
			mcp.Required(),
			mcp.Description("The 'why this matters' prose for the changelog entry. Can be multi-line; Markdown is preserved."),
		),
		mcp.WithString("commit_hash",
			mcp.Required(),
			mcp.Description("Full 40-character git SHA-1 this entry corresponds to. Get via `git rev-parse HEAD`."),
		),
		mcp.WithString("tags",
			mcp.Description("Comma-separated extra tags for retrieval. The #changelog and #commit:<hash> tags are auto-injected."),
		),
	)
}

func handleLogToChangelog(dm *internal.DatabaseManager) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()

		fact, _ := args["fact"].(string)
		if strings.TrimSpace(fact) == "" {
			return mcp.NewToolResultError("fact is required"), nil
		}
		commitHash, _ := args["commit_hash"].(string)
		if strings.TrimSpace(commitHash) == "" {
			return mcp.NewToolResultError(
				"commit_hash is required (strict retrospective contract: every changelog memory must reference an existing commit). " +
					"Run `git rev-parse HEAD` to get the canonical 40-char SHA-1."), nil
		}
		extraTags := parseStringSliceArg(args["tags"])

		id, err := dm.LogChangelogEntry(fact, commitHash, extraTags)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("log_to_changelog failed", err), nil
		}
		return jsonResult(map[string]interface{}{
			"success":     true,
			"id":          id,
			"commit_hash": commitHash,
			"tags":        []string{internal.ChangelogTag, internal.CommitTagPrefix + strings.ToLower(commitHash)},
			"collection":  "changelog",
		}), nil
	}
}

// ── add_evidence ──────────────────────────────────────────────────────────

func toolAddEvidence() mcp.Tool {
	return mcp.NewTool("add_evidence",
		mcp.WithDescription(
			"Insert a new evidence row and return the resulting confidence for the artifact. "+
				"Required: artifact_id, type, source_group, created_by. Optional: artifact_type "+
				"(default 'memory'), strength (default from type registry), independence_factor "+
				"(default 1.0), notes."),
		mcp.WithString("artifact_id", mcp.Required(), mcp.Description("Artifact this evidence applies to.")),
		mcp.WithString("artifact_type", mcp.Description("Artifact type: 'memory' or 'lesson'. Default 'memory'.")),
		mcp.WithString("type", mcp.Required(), mcp.Enum("observation", "test", "reproduction", "challenge", "decision_outcome", "external_reference"),
			mcp.Description("Evidence type — must be a known v1 type.")),
		mcp.WithString("source_group", mcp.Required(), mcp.Description("Source group label (e.g. 'user-X', 'test-rig-1').")),
		mcp.WithNumber("strength", mcp.Description("Evidence strength 0–1; default from the type registry.")),
		mcp.WithNumber("independence_factor", mcp.DefaultNumber(1.0), mcp.Description("Independence factor 0–1 (default 1.0).")),
		mcp.WithString("created_by", mcp.Required(), mcp.Description("Who/what created this evidence.")),
		mcp.WithString("notes", mcp.Description("Optional free-text notes.")),
	)
}

func handleAddEvidence(dm *internal.DatabaseManager) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		artifactID, _ := args["artifact_id"].(string)
		if artifactID == "" {
			return mcp.NewToolResultError("artifact_id is required"), nil
		}
		evType, _ := args["type"].(string)
		if evType == "" {
			return mcp.NewToolResultError("type is required"), nil
		}
		source, _ := args["source_group"].(string)
		if source == "" {
			return mcp.NewToolResultError("source_group is required"), nil
		}
		createdBy, _ := args["created_by"].(string)
		if createdBy == "" {
			return mcp.NewToolResultError("created_by is required"), nil
		}
		artifactType, _ := args["artifact_type"].(string)
		var strength float64
		if s, ok := args["strength"].(float64); ok {
			strength = s
		}
		var independence float64 = 1.0
		if i, ok := args["independence_factor"].(float64); ok {
			independence = i
		}
		notes, _ := args["notes"].(string)

		out, err := dm.AddEvidence(internal.EvidenceInput{
			ArtifactID:         artifactID,
			ArtifactType:       artifactType,
			Type:               evType,
			SourceGroup:        source,
			Strength:           strength,
			IndependenceFactor: independence,
			CreatedBy:          createdBy,
			CreatedAt:          time.Now(),
			Notes:              notes,
		})
		if err != nil {
			return mcp.NewToolResultErrorFromErr("add_evidence failed", err), nil
		}
		return jsonResult(out), nil
	}
}

// ── list_evidence ─────────────────────────────────────────────────────────

func toolListEvidence() mcp.Tool {
	return mcp.NewTool("list_evidence",
		mcp.WithDescription("List all evidence rows for an artifact, newest first."),
		mcp.WithString("artifact_id", mcp.Required(), mcp.Description("Artifact ID to list evidence for.")),
		mcp.WithString("artifact_type", mcp.Description("Artifact type: 'memory' or 'lesson'. Default 'memory'.")),
	)
}

func handleListEvidence(dm *internal.DatabaseManager) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		artifactID, _ := args["artifact_id"].(string)
		if artifactID == "" {
			return mcp.NewToolResultError("artifact_id is required"), nil
		}
		artifactType, _ := args["artifact_type"].(string)
		out, err := dm.ListEvidence(artifactID, artifactType)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("list_evidence failed", err), nil
		}
		return jsonResult(out), nil
	}
}

// ── query_confidence_history ──────────────────────────────────────────────

func toolQueryConfidenceHistory() mcp.Tool {
	return mcp.NewTool("query_confidence_history",
		mcp.WithDescription("Return the confidence timeline for an artifact, newest first."),
		mcp.WithString("artifact_id", mcp.Required(), mcp.Description("Artifact ID to query.")),
		mcp.WithString("artifact_type", mcp.Description("Artifact type: 'memory' or 'lesson'. Default 'memory'.")),
		mcp.WithNumber("limit", mcp.DefaultNumber(50), mcp.Description("Max rows to return. Default 50.")),
	)
}

func handleQueryConfidenceHistory(dm *internal.DatabaseManager) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		artifactID, _ := args["artifact_id"].(string)
		if artifactID == "" {
			return mcp.NewToolResultError("artifact_id is required"), nil
		}
		artifactType, _ := args["artifact_type"].(string)
		limit := int(parseNum(args["limit"], 50))
		out, err := dm.QueryConfidenceHistory(artifactID, artifactType, limit)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("query_confidence_history failed", err), nil
		}
		return jsonResult(out), nil
	}
}

// ── query_confidence_changes ──────────────────────────────────────────────

func toolQueryConfidenceChanges() mcp.Tool {
	return mcp.NewTool("query_confidence_changes",
		mcp.WithDescription("Return recent confidence-altering events with delta and trigger. "+
			"Distinct from query_confidence_history (full timeline): this answers 'what moved, by how much, and why, since when?'"),
		mcp.WithNumber("since_seconds_ago", mcp.Description("Look back N seconds. Alternative to `since`.")),
		mcp.WithNumber("since", mcp.Description("Unix timestamp cutoff. Default: last 24h.")),
		mcp.WithNumber("limit", mcp.DefaultNumber(50), mcp.Description("Max rows. Default 50.")),
		mcp.WithString("artifact_id", mcp.Description("Filter to a single artifact.")),
		mcp.WithString("artifact_type", mcp.Description("Filter to a single artifact type.")),
	)
}

func handleQueryConfidenceChanges(dm *internal.DatabaseManager) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		var filter internal.ConfidenceChangesFilter
		if secs, ok := args["since_seconds_ago"].(float64); ok && secs > 0 {
			filter.Since = time.Now().Add(-time.Duration(secs) * time.Second)
		} else if sinceF, ok := args["since"].(float64); ok && sinceF > 0 {
			filter.Since = time.Unix(int64(sinceF), 0)
		}
		if l, ok := args["limit"].(float64); ok && l > 0 {
			filter.Limit = int(l)
		}
		if v, ok := args["artifact_id"].(string); ok {
			filter.ArtifactID = v
		}
		if v, ok := args["artifact_type"].(string); ok {
			filter.ArtifactType = v
		}
		out, err := dm.QueryConfidenceChanges(filter)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("query_confidence_changes failed", err), nil
		}
		return jsonResult(out), nil
	}
}

// ── query_confidence_trend ────────────────────────────────────────────────

func toolQueryConfidenceTrend() mcp.Tool {
	return mcp.NewTool("query_confidence_trend",
		mcp.WithDescription("Return the trajectory projection of confidence over a time window "+
			"(velocity + trend label). Complements query_confidence_history and "+
			"query_confidence_changes."),
		mcp.WithString("artifact_id", mcp.Required(), mcp.Description("Artifact ID to query.")),
		mcp.WithString("artifact_type", mcp.Description("Artifact type: 'memory' or 'lesson'. Default 'memory'.")),
		mcp.WithNumber("window_days", mcp.DefaultNumber(30), mcp.Description("Window size in days. Default 30.")),
	)
}

func handleQueryConfidenceTrend(dm *internal.DatabaseManager) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		artifactID, _ := args["artifact_id"].(string)
		if artifactID == "" {
			return mcp.NewToolResultError("artifact_id is required"), nil
		}
		artifactType, _ := args["artifact_type"].(string)
		windowDays := int(parseNum(args["window_days"], 30))
		out, err := dm.QueryConfidenceTrend(artifactID, artifactType, windowDays)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("query_confidence_trend failed", err), nil
		}
		return jsonResult(out), nil
	}
}

// ── query_memory_quality ─────────────────────────────────────────────────

func toolQueryMemoryQuality() mcp.Tool {
	return mcp.NewTool("query_memory_quality",
		mcp.WithDescription("Return per-creator memory statistics — which models/agents "+
			"produce memories that survive. Driven by the memory_source_evidence_ai trigger "+
			"that auto-attributes each new memory to its writer."),
	)
}

func handleQueryMemoryQuality(dm *internal.DatabaseManager) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		out, err := dm.QueryMemoryQuality()
		if err != nil {
			return mcp.NewToolResultErrorFromErr("query_memory_quality failed", err), nil
		}
		return jsonResult(out), nil
	}
}

// ── show_confidence ───────────────────────────────────────────────────────

func toolShowConfidence() mcp.Tool {
	return mcp.NewTool("show_confidence",
		mcp.WithDescription("Return the current confidence and history for an artifact. "+
			"Result shape: {current: <float>, history: {history: [...]}}."),
		mcp.WithString("artifact_id", mcp.Required(), mcp.Description("Artifact ID to inspect.")),
		mcp.WithString("artifact_type", mcp.Description("Artifact type: 'memory' or 'lesson'. Default 'memory'.")),
	)
}

func handleShowConfidence(dm *internal.DatabaseManager) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		artifactID, _ := args["artifact_id"].(string)
		if artifactID == "" {
			return mcp.NewToolResultError("artifact_id is required"), nil
		}
		artifactType, _ := args["artifact_type"].(string)
		out, err := dm.ShowConfidence(artifactID, artifactType)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("show_confidence failed", err), nil
		}
		return jsonResult(out), nil
	}
}

// ── recompute_confidence ──────────────────────────────────────────────────

func toolRecomputeConfidence() mcp.Tool {
	return mcp.NewTool("recompute_confidence",
		mcp.WithDescription("Force a manual confidence recompute and return the new snapshot. "+
			"Result shape: {current, history: {history: [...]}} (same as show_confidence)."),
		mcp.WithString("artifact_id", mcp.Required(), mcp.Description("Artifact ID to recompute.")),
		mcp.WithString("artifact_type", mcp.Description("Artifact type: 'memory' or 'lesson'. Default 'memory'.")),
	)
}

func handleRecomputeConfidence(dm *internal.DatabaseManager) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		artifactID, _ := args["artifact_id"].(string)
		if artifactID == "" {
			return mcp.NewToolResultError("artifact_id is required"), nil
		}
		artifactType, _ := args["artifact_type"].(string)
		out, err := dm.RecomputeConfidence(artifactID, artifactType)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("recompute_confidence failed", err), nil
		}
		return jsonResult(out), nil
	}
}

// ── explain_confidence ───────────────────────────────────────────────

func toolExplainConfidence() mcp.Tool {
	return mcp.NewTool("explain_confidence",
		mcp.WithDescription("Return the reasoning trace for an artifact's confidence: "+
			"the full component breakdown of f(evidence, decay). Distinct from "+
			"query_confidence_history (audit trail) — this answers 'why did I get this number?'"),
		mcp.WithString("artifact_id", mcp.Required(), mcp.Description("Artifact ID to explain.")),
		mcp.WithString("artifact_type", mcp.Description("Artifact type: 'memory' or 'lesson'. Default 'memory'.")),
	)
}

func handleExplainConfidence(dm *internal.DatabaseManager) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		artifactID, _ := args["artifact_id"].(string)
		if artifactID == "" {
			return mcp.NewToolResultError("artifact_id is required"), nil
		}
		artifactType, _ := args["artifact_type"].(string)
		out, err := dm.ExplainConfidence(artifactID, artifactType)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("explain_confidence failed", err), nil
		}
		return jsonResult(out), nil
	}
}

// ── Memory feedback / mutation tools (added 2026-06-26) ───────────────────────
//
// These mirror the mpm call registry tools and let agents close the memory
// feedback loop via typed JSON arguments instead of shell-quoting `mpm
// reinforce <id>` etc. All schemas require memory_id; the numeric args
// (delta/days/weight) are optional with sensible defaults.

func toolShredMemory() mcp.Tool {
	return mcp.NewTool("shred_memory",
		mcp.WithDescription(
			"Secure-delete a memory and any theory it challenged. Single transaction: "+
				"removes topic_memberships, the memory row, and the linked theory (if any). "+
				"Use after challenge_memory when the memory is confirmed wrong, or to "+
				"remove a stale/duplicate row that you no longer want in recall results."),
		mcp.WithString("memory_id", mcp.Required(), mcp.Description("The MPM memory ID to delete.")),
	)
}

func handleShredMemory(dm *internal.DatabaseManager) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		id, _ := args["memory_id"].(string)
		if id == "" {
			return mcp.NewToolResultError("memory_id is required"), nil
		}
		out, err := dm.ShredMemoryWithCascade(id)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("shred_memory failed", err), nil
		}
		return jsonResult(out), nil
	}
}

func toolReinforceMemory() mcp.Tool {
	return mcp.NewTool("reinforce_memory",
		mcp.WithDescription(
			"Increment a memory's reinforcement_count and bump its weight slightly. "+
				"Call after the user agrees with or re-confirms a memory — stronger "+
				"reinforcement means higher ranking in recall results and slower decay. "+
				"Use delta > 1 for emphatic agreement; default 1 for normal."),
		mcp.WithString("memory_id", mcp.Required(), mcp.Description("The MPM memory ID to reinforce.")),
		mcp.WithNumber("delta", mcp.DefaultNumber(1), mcp.Description("Reinforcement delta. Default 1.")),
	)
}

func handleReinforceMemory(dm *internal.DatabaseManager) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		id, _ := args["memory_id"].(string)
		if id == "" {
			return mcp.NewToolResultError("memory_id is required"), nil
		}
		delta := int(parseNum(args["delta"], 1))
		out, err := dm.ReinforceMemoryTool(id, delta)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("reinforce_memory failed", err), nil
		}
		return jsonResult(out), nil
	}
}

func toolWeakenMemory() mcp.Tool {
	return mcp.NewTool("weaken_memory",
		mcp.WithDescription(
			"Decrement a memory's weight with a hard floor at 1. Use when the user "+
				"corrects or disconfirms a memory — soft punishment that lets natural "+
				"decay finish the job without making the memory permanently "+
				"invisible. For stronger signals (confirmed wrong), use shred_memory "+
				"or challenge_memory instead."),
		mcp.WithString("memory_id", mcp.Required(), mcp.Description("The MPM memory ID to weaken.")),
		mcp.WithNumber("delta", mcp.DefaultNumber(1), mcp.Description("Weaken delta. Default 1.")),
	)
}

func handleWeakenMemory(dm *internal.DatabaseManager) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		id, _ := args["memory_id"].(string)
		if id == "" {
			return mcp.NewToolResultError("memory_id is required"), nil
		}
		delta := int(parseNum(args["delta"], 1))
		out, err := dm.WeakenMemoryTool(id, delta)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("weaken_memory failed", err), nil
		}
		return jsonResult(out), nil
	}
}

func toolSnoozeMemory() mcp.Tool {
	return mcp.NewTool("snooze_memory",
		mcp.WithDescription(
			"Bump a memory's relevance without promoting it to LTM. Caps weight at 9 "+
				"(never reaches the LTM threshold of 10) and refreshes last_accessed_at. "+
				"Use when the agent just re-encountered a memory and wants to keep it "+
				"sticky for a few more days without locking it in permanently."),
		mcp.WithString("memory_id", mcp.Required(), mcp.Description("The MPM memory ID to snooze.")),
		mcp.WithNumber("days", mcp.DefaultNumber(1), mcp.Description("Days to extend relevance. Default 1.")),
	)
}

func handleSnoozeMemory(dm *internal.DatabaseManager) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		id, _ := args["memory_id"].(string)
		if id == "" {
			return mcp.NewToolResultError("memory_id is required"), nil
		}
		days := int(parseNum(args["days"], 1))
		out, err := dm.SnoozeMemory(id, days)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("snooze_memory failed", err), nil
		}
		return jsonResult(out), nil
	}
}

func toolSetMemoryWeight() mcp.Tool {
	return mcp.NewTool("set_memory_weight",
		mcp.WithDescription(
			"Set a memory's weight directly. Use when you know the exact weight "+
				"(e.g. loading a saved ranking, applying a curator decision) and "+
				"don't want to increment/decrement relative to the current value. "+
				"Weight is clamped to [0, 100] — use promote_memory for the standard "+
				"promote-to-LTM (weight=10 + is_long_term=1) flow."),
		mcp.WithString("memory_id", mcp.Required(), mcp.Description("The MPM memory ID to update.")),
		mcp.WithNumber("weight", mcp.Required(), mcp.Description("New weight, 0-100 inclusive.")),
	)
}

func handleSetMemoryWeight(dm *internal.DatabaseManager) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		id, _ := args["memory_id"].(string)
		if id == "" {
			return mcp.NewToolResultError("memory_id is required"), nil
		}
		weight := int(parseNum(args["weight"], 0))
		out, err := dm.SetMemoryWeight(id, weight)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("set_memory_weight failed", err), nil
		}
		return jsonResult(out), nil
	}
}

func toolPatchMemory() mcp.Tool {
	return mcp.NewTool("patch_memory",
		mcp.WithDescription(
			"Merge a JSON object patch into the memory's metadata column. "+
				"Existing keys not in the patch are preserved; keys in the patch "+
				"overwrite existing values. Use for tagging memories post-hoc "+
				"(e.g. set source/verified_by/decay_class) without re-writing the "+
				"whole metadata blob. For raw SQL updates, use save_to_memory instead."),
		mcp.WithString("memory_id", mcp.Required(), mcp.Description("The MPM memory ID to patch.")),
		mcp.WithObject("patch",
			mcp.Required(),
			mcp.Description("JSON object to merge into metadata."),
		),
	)
}

func handlePatchMemory(dm *internal.DatabaseManager) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		id, _ := args["memory_id"].(string)
		if id == "" {
			return mcp.NewToolResultError("memory_id is required"), nil
		}
		patch, ok := args["patch"].(map[string]interface{})
		if !ok {
			return mcp.NewToolResultError("patch must be a JSON object"), nil
		}
		patchJSON, err := json.Marshal(patch)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("patch must be JSON-marshalable", err), nil
		}
		out, err := dm.PatchMemoryMetadata(id, string(patchJSON))
		if err != nil {
			return mcp.NewToolResultErrorFromErr("patch_memory failed", err), nil
		}
		return jsonResult(out), nil
	}
}

func toolPromoteMemory() mcp.Tool {
	return mcp.NewTool("promote_memory",
		mcp.WithDescription(
			"Promote a memory to Long-Term Memory (LTM): clear its TTL, apply a strong "+
				"reinforcement, set weight=10, and mark is_long_term=1. The scoring "+
				"model treats LTM memories as immune to decay — use for facts that "+
				"should survive across sessions (user identity, environment, core "+
				"preferences). Use save_to_memory with weight >= 10 for the same "+
				"effect on creation; this tool promotes an EXISTING memory."),
		mcp.WithString("memory_id", mcp.Required(), mcp.Description("The MPM memory ID to promote.")),
	)
}

func handlePromoteMemory(dm *internal.DatabaseManager) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		id, _ := args["memory_id"].(string)
		if id == "" {
			return mcp.NewToolResultError("memory_id is required"), nil
		}
		out, err := dm.PromoteMemory(id)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("promote_memory failed", err), nil
		}
		return jsonResult(out), nil
	}
}

// ── Workflow tools (Tier 2 — added 2026-06-26) ────────────────────────────────

func toolReviewMemories() mcp.Tool {
	return mcp.NewTool("review_memories",
		mcp.WithDescription(
			"Return memories due for spaced reinforcement review: LTM or high-weight "+
				"memories not accessed in `days`+ days. Use this at session-start or before "+
				"a maintenance pass to surface what's stale. Output is the same shape as "+
				"`query_long_term_memory` — items are ordered by last_accessed_at ascending."),
		mcp.WithNumber("days", mcp.DefaultNumber(30), mcp.Description("Days since last access threshold. Default 30.")),
		mcp.WithNumber("limit", mcp.DefaultNumber(20), mcp.Description("Max items to return. Default 20.")),
	)
}

func handleReviewMemories(dm *internal.DatabaseManager) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		days := int(parseNum(args["days"], 30))
		limit := int(parseNum(args["limit"], 20))
		out, err := dm.ReviewMemories(days, limit)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("review_memories failed", err), nil
		}
		return jsonResult(out), nil
	}
}

func toolSynthesizeMemory() mcp.Tool {
	return mcp.NewTool("synthesize_memory",
		mcp.WithDescription(
			"Run LLM-driven merge synthesis for a single memory against all other "+
				"non-LTM memories. The LLM (MiniMax or OpenAI, picked by env config) decides "+
				"if any near-miss clusters exist and either merges them into a new "+
				"synthesized row (soft-deleting the originals) or no-ops. Use after "+
				"discovering a memory that you suspect has duplicates. Requires "+
				"MINIMAX_API_KEY or OPENAI_API_KEY in env."),
		mcp.WithString("memory_id", mcp.Required(), mcp.Description("The MPM memory ID to scan for near-misses.")),
	)
}

func handleSynthesizeMemory(dm *internal.DatabaseManager) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		id, _ := args["memory_id"].(string)
		if id == "" {
			return mcp.NewToolResultError("memory_id is required"), nil
		}
		out, err := dm.SynthesizeMemoryFor(ctx, id)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("synthesize_memory failed", err), nil
		}
		return jsonResult(out), nil
	}
}

// ── Workflow tools (Tier 3 — added 2026-06-26) ────────────────────────────────

func toolGCRun() mcp.Tool {
	return mcp.NewTool("gc_run",
		mcp.WithDescription(
			"Run one GC maintenance pass: decay sweep + audit/handoff retention. "+
				"Safe defaults: dry_run=true (no writes), aggressive=false, max_age_hours=24. "+
				"Set dry_run=false to actually mutate state. The cooldown cap "+
				"(max_age_hours) prevents overlapping runs — a second invocation "+
				"within the cooldown window returns cooldown_skip=true instead of "+
				"running again. Result includes scanned/updated counts and up to 50 "+
				"dead-memory previews so the agent can decide whether to escalate "+
				"(purge/shred are NOT exposed here; use the `mpm gc` CLI for those)."),
		mcp.WithBoolean("dry_run", mcp.DefaultBool(true), mcp.Description("If true (default), no writes — pure stats.")),
		mcp.WithBoolean("aggressive", mcp.DefaultBool(false), mcp.Description("Double the decay rate.")),
		mcp.WithNumber("max_age_hours", mcp.DefaultNumber(24), mcp.Description("Cooldown between successive GC runs. Default 24.")),
	)
}

func handleGCRun(dm *internal.DatabaseManager) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		dryRun := true
		if v, ok := args["dry_run"].(bool); ok {
			dryRun = v
		}
		aggressive := false
		if v, ok := args["aggressive"].(bool); ok {
			aggressive = v
		}
		maxAge := int(parseNum(args["max_age_hours"], 24))

		out, err := dm.RunGC(internal.GCOptions{
			DryRun:      dryRun,
			Aggressive:  aggressive,
			MaxAgeHours: maxAge,
		})
		if err != nil {
			return mcp.NewToolResultErrorFromErr("gc_run failed", err), nil
		}

		result := map[string]interface{}{
			"success":           true,
			"dry_run":           dryRun,
			"cooldown_skip":     out.CooldownSkip,
			"ran":               out.Ran,
			"scanned":           out.Scanned,
			"updated":           out.Updated,
			"audit_pruned":      out.AuditPruned,
			"handoff_pruned":    out.HandoffPruned,
			"dead_memory_count": len(out.DeadMemories),
			"dead_memories":     out.DeadMemories,
		}
		if out.LastGCRan != nil {
			result["last_gc_ran"] = out.LastGCRan.Format(time.RFC3339)
		}
		return jsonResult(result), nil
	}
}
