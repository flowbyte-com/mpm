// tools.go — Single source of truth for the MCP tool surface.
//
// 18 tools, one spec + one handler each. All handlers are thin shims:
// extract args via type assertion, call a dm method, wrap the result.
// The dm methods live in internal/call_helpers.go and back both this MCP
// server and the `mpm call <tool>` CLI (cmd/mpm/call.go).
//
// Descriptions and arg schemas are copied from
// opencode-mpm-plugin/src/index.ts (the OpenClaw plugin's tool surface).
// The Python plugin in .claude/mpm-mcp/server.py has a smaller subset
// (read_wake_context, read_directives, propose_theory, resolve_theory,
// record_decision, proactive_recall_hint) which is fully covered here.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"mpm/internal"
)

const emptyWakeContext = "Wake context is empty. Ready for context."

// RegisterAllTools registers all 19 MPM tools on the given MCP server.
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
