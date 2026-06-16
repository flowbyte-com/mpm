package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"mpm/internal"
	"mpm/internal/config"
)

// ToolHandler is the signature for a call-able tool.
// It receives the parsed payload map and returns (result, error).
type ToolHandler func(payload map[string]interface{}) (interface{}, error)

// toolRegistry maps OpenClaw plugin tool names to their handlers.
// This is the single universal router — add new tools here.
var toolRegistry = map[string]ToolHandler{
	// Memory
	"save_to_memory":         callSaveToMemory,
	"query_long_term_memory": callQueryLongTermMemory,
	"challenge_memory":       callChallengeMemory,

	// Epistemology
	"propose_theory":  callProposeTheory,
	"resolve_theory":  callResolveTheory,
	"record_decision": callRecordDecision,

	// Lessons
	"save_lesson":    callSaveLesson,
	"search_lessons": callSearchLessons,
	"list_lessons":   callListLessons,

	// Topics
	"create_topic":  callCreateTopic,
	"search_topics": callSearchTopics,
	"link_topic":    callLinkTopic,

	// References
	"add_reference":     callAddReference,
	"search_references": callSearchReferences,
	"list_references":   callListReferences,

	// System
	"read_wake_context":     callReadWakeContext,
	"read_directives":       callReadDirectives,
	"proactive_recall_hint": callProactiveRecallHint,
	"route":                 callRoute,
}

// handleCall is the main entry point for `mpm call <tool> [--payload <json>]`.
func handleCall(args []string) int {
	if len(args) < 1 {
		printError("usage: mpm call <tool_name> [--payload <json>]")
		return 1
	}

	toolName := args[0]
	handler, ok := toolRegistry[toolName]
	if !ok {
		printError("unknown tool: %s. available: %s", toolName, strings.Join(availableToolNames(), ", "))
		return 1
	}

	payload, err := parsePayload(args[1:])
	if err != nil {
		printError("failed to parse payload: %v", err)
		return 1
	}

	result, err := handler(payload)
	if err != nil {
		// Errors are JSON to stderr so the agent can parse them
		fmt.Fprintf(os.Stderr, "%s\n", must(json.Marshal(map[string]interface{}{
			"success": false,
			"error":   err.Error(),
		})))
		return 1
	}

	// Extract __sse_broadcast before any mutation.
	var sseRaw interface{}
	if resultMap, ok := result.(map[string]interface{}); ok {
		sseRaw = resultMap["__sse_broadcast"]
		delete(resultMap, "__sse_broadcast")
	}
	fmt.Println(string(must(json.Marshal(result))))
	// Relay tool_exec to the web server's SSE broker.
	relayBroadcast("tool_exec", map[string]interface{}{
		"tool":   toolName,
		"result": result,
	})
	// Relay any extra SSE event bundled in the result (e.g. immune_slash from challenge_memory).
	if sse, ok := sseRaw.(map[string]interface{}); ok {
		if et, ok := sse["eventType"].(string); ok {
			relayBroadcast(et, sse["payload"])
		}
	}
	return 0
}

// parsePayload extracts JSON from --payload flag or stdin.
// If neither is present, returns an empty map (some tools need no input).
func parsePayload(args []string) (map[string]interface{}, error) {
	for i := 0; i < len(args); i++ {
		if args[i] == "--payload" && i+1 < len(args) {
			var p map[string]interface{}
			if err := json.Unmarshal([]byte(args[i+1]), &p); err != nil {
				return nil, fmt.Errorf("--payload: %w", err)
			}
			return p, nil
		}
	}
	// No --payload flag; try stdin if it has data.
	if stat, _ := os.Stdin.Stat(); stat.Size() > 0 {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return nil, fmt.Errorf("stdin: %w", err)
		}
		s := string(data)
		// Trim BOM if present (some editors write UTF-8 BOM)
		s = strings.TrimPrefix(s, "\xef\xbb\xbf")
		if len(s) > 0 {
			var p map[string]interface{}
			if err := json.Unmarshal([]byte(s), &p); err != nil {
				return nil, fmt.Errorf("stdin: %w", err)
			}
			return p, nil
		}
	}
	return map[string]interface{}{}, nil
}

func availableToolNames() []string {
	names := make([]string, 0, len(toolRegistry))
	for k := range toolRegistry {
		names = append(names, k)
	}
	return names
}

func must(data []byte, _ error) []byte { return data }

// relayBroadcast sends an event to the web server's internal SSE relay endpoint.
// If the web server is not running, the event is silently dropped — this is
// intentional: SSE is best-effort telemetry, not a critical path.
func relayBroadcast(eventType string, payload interface{}) {
	port := os.Getenv("MPM_PORT")
	if port == "" {
		port = readActivePort()
	}
	if port == "" {
		port = "18792"
	}
	body, _ := json.Marshal(map[string]interface{}{
		"eventType": eventType,
		"payload":   payload,
	})
	resp, err := http.Post(
		"http://localhost:"+port+"/api/internal/broadcast",
		"application/json",
		bytes.NewReader(body),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[relay] post error: %v\n", err)
		return
	}
	resp.Body.Close()
}

// readActivePort reads the port number written by the web server to its PID file.
// The PID file path is sourced from config.GetMPMDir() so it lives in ~/.mpm/.
func readActivePort() string {
	mpmDir := config.GetMPMDir()
	portFile := mpmDir + "/web.port"
	data, err := os.ReadFile(portFile)
	if err != nil || len(data) == 0 {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// ── Tool Dispatchers ────────────────────────────────────────────────────────
//
// Each callXxx is now a thin dispatcher into the corresponding dm method in
// internal/call_helpers.go. The dm methods are the single source of truth
// for the tool surface — they also back the Go MCP server (cmd/mpm-mcp).
// The active-mode/persona globals are still set by the existing
// injectActiveContext() helper in handlers.go and passed via ActiveContext.

// callSaveToMemory persists a fact, lesson, or decision to MPM long-term memory.
func callSaveToMemory(p map[string]interface{}) (interface{}, error) {
	fact, _ := p["fact"].(string)
	if fact == "" {
		return nil, fmt.Errorf("fact is required")
	}
	injectActiveContext()
	defer clearActiveContext()

	dm, err := internal.NewDatabaseManager("")
	if err != nil {
		return nil, fmt.Errorf("db: %w", err)
	}
	defer dm.Close()

	out, mem, err := dm.SaveMemoryWithContext(
		fact,
		internal.ParseStringOr(p["collection"], "memories"),
		internal.ParseStringSliceOr(p["tags"]),
		internal.ParseFloatOr(p["weight"], 0.5),
		internal.ParseStringOr(p["ttl"], ""),
		internal.ActiveContext{Mode: activeMode, Persona: activePersona},
	)
	if err != nil {
		return nil, err
	}
	// SaveMemoryWithContext does not bundle __sse_broadcast; the CLI SSE
	// relay expects the legacy "memory_saved" event with this shape.
	out["__sse_broadcast"] = map[string]interface{}{
		"eventType": "memory_saved",
		"payload": map[string]interface{}{
			"id":         mem.ID,
			"content":    mem.Content,
			"weight":     mem.Weight,
			"collection": mem.Collection,
			"tags":       mem.Tags,
			"provenance": map[string]interface{}{"agent": "mpm_call"},
		},
	}
	return out, nil
}

// callQueryLongTermMemory searches memory for context.
func callQueryLongTermMemory(p map[string]interface{}) (interface{}, error) {
	query, _ := p["query"].(string)
	if query == "" {
		return nil, fmt.Errorf("query is required")
	}
	limit := int(internal.ParseFloatOr(p["limit"], 5))
	if limit <= 0 {
		limit = 5
	}
	collection, _ := p["collection"].(string)

	dm, err := internal.NewDatabaseManager("")
	if err != nil {
		return nil, fmt.Errorf("db: %w", err)
	}
	defer dm.Close()

	items, err := dm.HybridSearchMemories(query, collection, limit)
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{
		"success":  true,
		"memories": items,
		"count":    len(items),
	}, nil
}

// callChallengeMemory weakens a memory and creates a pending theory.
func callChallengeMemory(p map[string]interface{}) (interface{}, error) {
	memoryID, _ := p["memoryId"].(string)
	if memoryID == "" {
		return nil, fmt.Errorf("memoryId is required")
	}
	evidence, _ := p["evidence"].(string)

	dm, err := internal.NewDatabaseManager("")
	if err != nil {
		return nil, fmt.Errorf("db: %w", err)
	}
	defer dm.Close()

	return dm.ChallengeMemoryWithTheory(memoryID, evidence)
}

// callProposeTheory logs a hypothesis with validation criteria.
func callProposeTheory(p map[string]interface{}) (interface{}, error) {
	hypothesis, _ := p["hypothesis"].(string)
	if hypothesis == "" {
		return nil, fmt.Errorf("hypothesis is required")
	}
	validationCriteria, _ := p["validation_criteria"].(string)
	tags := internal.ParseStringSliceOr(p["tags"])
	if tags == nil {
		tags = []string{}
	}

	dm, err := internal.NewDatabaseManager("")
	if err != nil {
		return nil, fmt.Errorf("db: %w", err)
	}
	defer dm.Close()

	return dm.ProposeTheory(hypothesis, validationCriteria, tags)
}

// callResolveTheory marks a theory as proven or disproven.
func callResolveTheory(p map[string]interface{}) (interface{}, error) {
	theoryID, _ := p["theoryId"].(string)
	if theoryID == "" {
		return nil, fmt.Errorf("theoryId is required")
	}
	conclusion, _ := p["conclusion"].(string)
	if conclusion == "" {
		return nil, fmt.Errorf("conclusion is required")
	}
	newStatus, _ := p["newStatus"].(string)
	if newStatus != "proven" && newStatus != "disproven" {
		return nil, fmt.Errorf("newStatus must be 'proven' or 'disproven'")
	}

	dm, err := internal.NewDatabaseManager("")
	if err != nil {
		return nil, fmt.Errorf("db: %w", err)
	}
	defer dm.Close()

	return dm.ResolveTheory(theoryID, conclusion, newStatus)
}

// callRecordDecision logs a decision with context, choice, and rationale.
func callRecordDecision(p map[string]interface{}) (interface{}, error) {
	choice, _ := p["choice"].(string)
	if choice == "" {
		return nil, fmt.Errorf("choice is required")
	}
	tags := internal.ParseStringSliceOr(p["tags"])
	if tags == nil {
		tags = []string{}
	}
	injectActiveContext()
	defer clearActiveContext()

	dm, err := internal.NewDatabaseManager("")
	if err != nil {
		return nil, fmt.Errorf("db: %w", err)
	}
	defer dm.Close()

	return dm.RecordDecision(
		internal.ParseStringOr(p["context"], ""),
		choice,
		internal.ParseStringOr(p["rationale"], ""),
		internal.ParseStringOr(p["outcome"], ""),
		tags,
		internal.ActiveContext{Mode: activeMode, Persona: activePersona},
	)
}

// callSaveLesson persists a lesson to MPM.
func callSaveLesson(p map[string]interface{}) (interface{}, error) {
	fact, _ := p["fact"].(string)
	if fact == "" {
		return nil, fmt.Errorf("fact is required")
	}
	lessonType := internal.ParseStringOr(p["type"], "insight")
	tags := internal.ParseStringSliceOr(p["tags"])

	dm, err := internal.NewDatabaseManager("")
	if err != nil {
		return nil, fmt.Errorf("db: %w", err)
	}
	defer dm.Close()

	out, lesson, err := dm.SaveLesson(fact, lessonType, tags)
	if err != nil {
		return nil, err
	}
	// SaveLesson does not bundle __sse_broadcast; add the legacy lesson_saved
	// event so the SSE broker can relay it.
	out["__sse_broadcast"] = map[string]interface{}{
		"eventType": "lesson_saved",
		"payload": map[string]interface{}{
			"id":   lesson.ID,
			"type": string(lesson.Type),
			"fact": fact,
		},
	}
	return out, nil
}

// callSearchLessons searches lesson content.
func callSearchLessons(p map[string]interface{}) (interface{}, error) {
	query, _ := p["query"].(string)
	if query == "" {
		return nil, fmt.Errorf("query is required")
	}

	dm, err := internal.NewDatabaseManager("")
	if err != nil {
		return nil, fmt.Errorf("db: %w", err)
	}
	defer dm.Close()

	items, err := dm.SearchLessonsLimited(query)
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{
		"success": true,
		"results": items,
		"count":   len(items),
	}, nil
}

// callListLessons lists all lessons, optionally filtered by type.
func callListLessons(p map[string]interface{}) (interface{}, error) {
	lessonType := internal.ParseStringOr(p["type"], "")

	dm, err := internal.NewDatabaseManager("")
	if err != nil {
		return nil, fmt.Errorf("db: %w", err)
	}
	defer dm.Close()

	items, err := dm.ListLessonsFiltered(lessonType)
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{
		"success": true,
		"lessons": items,
		"count":   len(items),
	}, nil
}

// callCreateTopic creates a new topic.
func callCreateTopic(p map[string]interface{}) (interface{}, error) {
	name, _ := p["name"].(string)
	if name == "" {
		return nil, fmt.Errorf("name is required")
	}
	description := internal.ParseStringOr(p["description"], "")

	dm, err := internal.NewDatabaseManager("")
	if err != nil {
		return nil, fmt.Errorf("db: %w", err)
	}
	defer dm.Close()

	topicID, err := dm.CreateTopicWithDescription(name, description)
	if err != nil {
		return nil, fmt.Errorf("create topic: %w", err)
	}
	return map[string]interface{}{
		"success": true,
		"id":      topicID,
		"name":    name,
	}, nil
}

// callSearchTopics searches topics by name/description.
func callSearchTopics(p map[string]interface{}) (interface{}, error) {
	query, _ := p["query"].(string)
	limit := int(internal.ParseFloatOr(p["limit"], 20))
	if limit <= 0 {
		limit = 20
	}

	dm, err := internal.NewDatabaseManager("")
	if err != nil {
		return nil, fmt.Errorf("db: %w", err)
	}
	defer dm.Close()

	items, err := dm.SearchTopicsByQuery(query, limit)
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{
		"success": true,
		"results": items,
		"count":   len(items),
	}, nil
}

// callLinkTopic links a memory to a topic.
func callLinkTopic(p map[string]interface{}) (interface{}, error) {
	memoryID, _ := p["memory_id"].(string)
	if memoryID == "" {
		return nil, fmt.Errorf("memory_id is required")
	}
	topicID, _ := p["topic_id"].(string)
	if topicID == "" {
		return nil, fmt.Errorf("topic_id is required")
	}

	dm, err := internal.NewDatabaseManager("")
	if err != nil {
		return nil, fmt.Errorf("db: %w", err)
	}
	defer dm.Close()

	if err := dm.AddMemoryToTopic(memoryID, topicID, "manual"); err != nil {
		return nil, fmt.Errorf("link topic: %w", err)
	}
	return map[string]interface{}{
		"success":   true,
		"memory_id": memoryID,
		"topic_id":  topicID,
	}, nil
}

// callAddReference ingests a document as a reference.
func callAddReference(p map[string]interface{}) (interface{}, error) {
	filepath, _ := p["filepath"].(string)
	if filepath == "" {
		return nil, fmt.Errorf("filepath is required")
	}
	title := internal.ParseStringOr(p["title"], "")

	dm, err := internal.NewDatabaseManager("")
	if err != nil {
		return nil, fmt.Errorf("db: %w", err)
	}
	defer dm.Close()

	return dm.AddReferenceFromFile(filepath, title)
}

// callSearchReferences searches reference content.
func callSearchReferences(p map[string]interface{}) (interface{}, error) {
	query, _ := p["query"].(string)
	if query == "" {
		return nil, fmt.Errorf("query is required")
	}
	limit := int(internal.ParseFloatOr(p["limit"], 5))
	if limit <= 0 {
		limit = 5
	}

	dm, err := internal.NewDatabaseManager("")
	if err != nil {
		return nil, fmt.Errorf("db: %w", err)
	}
	defer dm.Close()

	results, err := dm.SearchReferenceChunks(query, limit)
	if err != nil {
		return nil, fmt.Errorf("search references: %w", err)
	}
	items := make([]map[string]interface{}, 0, len(results))
	for _, r := range results {
		items = append(items, map[string]interface{}{
			"id":          r["id"],
			"doc_title":   r["doc_title"],
			"chunk_index": r["chunk_index"],
			"content":     r["content"],
		})
	}
	return map[string]interface{}{
		"success": true,
		"results": items,
		"count":   len(items),
	}, nil
}

// callListReferences lists all ingested reference documents.
func callListReferences(p map[string]interface{}) (interface{}, error) {
	limit := int(internal.ParseFloatOr(p["limit"], 50))
	if limit <= 0 {
		limit = 50
	}
	offset := int(internal.ParseFloatOr(p["offset"], 0))
	if offset < 0 {
		offset = 0
	}

	dm, err := internal.NewDatabaseManager("")
	if err != nil {
		return nil, fmt.Errorf("db: %w", err)
	}
	defer dm.Close()

	refs, err := dm.ListReferences(limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list references: %w", err)
	}
	return map[string]interface{}{
		"success":    true,
		"references": refs,
		"count":      len(refs),
	}, nil
}

// callReadWakeContext returns the last session's context. Data gathering is
// delegated to internal.ReadWakeContext (single source of truth shared with
// the Go MCP server).
func callReadWakeContext(_ map[string]interface{}) (interface{}, error) {
	dm, err := internal.NewDatabaseManager("")
	if err != nil {
		return nil, fmt.Errorf("db: %w", err)
	}
	defer dm.Close()

	data, err := dm.GatherWakeContext()
	if err != nil {
		return nil, fmt.Errorf("gather wake context: %w", err)
	}

	if data.SessionID == "" {
		return map[string]interface{}{
			"success":         true,
			"session_id":      "",
			"active_mode":     "",
			"active_persona":  "",
			"recent_topics":   []string{},
			"recent_memories": []map[string]interface{}{},
		}, nil
	}

	memRefs := make([]map[string]interface{}, 0, len(data.RecentMemories))
	for _, m := range data.RecentMemories {
		memRefs = append(memRefs, map[string]interface{}{
			"id":         m.ID,
			"content":    m.Content,
			"created_at": m.CreatedAt,
		})
	}

	return map[string]interface{}{
		"success":         true,
		"session_id":      data.SessionID,
		"active_mode":     data.ActiveMode,
		"active_persona":  data.ActivePersona,
		"recent_topics":   data.RecentTopics,
		"recent_memories": memRefs,
	}, nil
}

// callReadDirectives returns prime directives.
func callReadDirectives(_ map[string]interface{}) (interface{}, error) {
	dm, err := internal.NewDatabaseManager("")
	if err != nil {
		return nil, fmt.Errorf("db: %w", err)
	}
	defer dm.Close()

	directives, err := dm.ReadDirectives()
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{
		"success":    true,
		"directives": directives,
		"count":      len(directives),
	}, nil
}

// callProactiveRecallHint checks conversation context for relevant decisions/theories.
func callProactiveRecallHint(p map[string]interface{}) (interface{}, error) {
	conversationText, _ := p["conversation_text"].(string)
	if conversationText == "" {
		return nil, fmt.Errorf("conversation_text is required")
	}
	maxHints := int(internal.ParseFloatOr(p["max_hints"], 3))
	if maxHints <= 0 {
		maxHints = 3
	}

	dm, err := internal.NewDatabaseManager("")
	if err != nil {
		return nil, fmt.Errorf("db: %w", err)
	}
	defer dm.Close()

	overlaps, err := dm.ProactiveRecallHint(conversationText, maxHints, internal.ParseFloatOr(p["min_score"], -3.0))
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{
		"success": true,
		"hints":   overlaps,
		"count":   len(overlaps),
	}, nil
}

// callRoute evaluates a prompt against the workspace's mode+persona
// configuration and returns a RoutingReport. This is the JSON-RPC path
// for OpenClaw and Hermes — pure JSON, no text rendering.
//
// Unlike mpm route (text), this handler returns errors instead of silently
// producing empty output. Callers are machines and can handle failures.
func callRoute(p map[string]interface{}) (interface{}, error) {
	prompt, _ := p["prompt"].(string)
	if prompt == "" {
		return nil, fmt.Errorf("prompt is required")
	}

	workspace := resolveRouteWorkspace()
	router, err := internal.NewRouter(workspace)
	if err != nil {
		return nil, fmt.Errorf("router init: %w", err)
	}

	report := router.Evaluate(prompt)
	return map[string]interface{}{
		"selected_modes":   report.SelectedModes,
		"selected_persona": report.SelectedPersona,
		"scores":           report.Scores,
	}, nil
}
