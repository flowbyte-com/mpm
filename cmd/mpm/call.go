package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"mpm/internal"
	"mpm/internal/config"
)

// ToolHandler is the signature for a call-able tool.
// It receives the parsed payload map and returns (result, error).
type ToolHandler func(payload map[string]interface{}) (interface{}, error)

// testDMOverride lets tests inject a DatabaseManager into call handlers
// without going through the workspace DB. When non-nil, every call handler
// that would otherwise call NewDatabaseManager("") uses this instead.
//
// Production callers (the `mpm call <tool>` CLI path) never set this —
// they go through handleCall → handler() → NewDatabaseManager("") as
// before. Tests that exercise call handlers can override to a fresh temp
// DB and reset at the end with t.Cleanup.
var testDMOverride struct {
	sync.Mutex
	dm *internal.DatabaseManager
}

func setTestDMOverride(dm *internal.DatabaseManager) {
	testDMOverride.Lock()
	defer testDMOverride.Unlock()
	testDMOverride.dm = dm
}

// openCallDM returns the DM to use for a call handler. If a test has set
// an override, that DM is used (without defer-Close — the test owns its
// lifecycle). Otherwise the workspace DM is opened, deferred-closed, and
// returned. Centralizing this avoids the workspace-DB pollution that
// occurs when tests round-trip through call* handlers.
func openCallDM() (*internal.DatabaseManager, func(), error) {
	testDMOverride.Lock()
	override := testDMOverride.dm
	testDMOverride.Unlock()
	if override != nil {
		// Test-owned DM — caller is responsible for closing.
		return override, func() {}, nil
	}
	dm, err := internal.NewDatabaseManager("")
	if err != nil {
		return nil, nil, fmt.Errorf("db: %w", err)
	}
	return dm, func() { dm.Close() }, nil
}

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

	// Evidence / Confidence
	"add_evidence":             callAddEvidence,
	"list_evidence":            callListEvidence,
	"query_confidence_history": callQueryConfidenceHistory,
	"query_confidence_changes": callQueryConfidenceChanges,
	"query_confidence_trend":   callQueryConfidenceTrend,
	"query_memory_quality":     callQueryMemoryQuality,
	"show_confidence":          callShowConfidence,
	"recompute_confidence":     callRecomputeConfidence,

	// Self-audit log
	"query_audit_log":          callQueryAuditLog,
	"explain_confidence":       callExplainConfidence,

	// System
	"read_wake_context":     callReadWakeContext,
	"read_directives":       callReadDirectives,
	"proactive_recall_hint": callProactiveRecallHint,
	"route":                 callRoute,

	// Session handoffs
	"session_end":      callSessionEnd,
	"session_handoff":  callSessionHandoff,
	"list_handoffs":    callListHandoffs,

	// Release
	"log_to_changelog": callLogToChangelog,

	// Memory feedback / mutation (agent loop closure — added 2026-06-26
	// to close the gap where agents had to shell `mpm reinforce <id>` etc.)
	"shred_memory":       callShredMemory,
	"reinforce_memory":   callReinforceMemory,
	"weaken_memory":      callWeakenMemory,
	"snooze_memory":       callSnoozeMemory,
	"set_memory_weight":  callSetMemoryWeight,
	"patch_memory":       callPatchMemory,
	"promote_memory":     callPromoteMemory,

	// Workflow (Tier 2 — added 2026-06-26)
	"review_memories":     callReviewMemories,
	"synthesize_memory":   callSynthesizeMemory,

	// Workflow (Tier 3 — added 2026-06-26)
	"gc_run":              callGCRun,
}

// handleCall is the main entry point for `mpm call <tool>`.
//
// Payload is read from one of three sources (in priority order):
//   1. --payload <json>     inline JSON arg (avoid with apostrophes/quotes)
//   2. --payload-file <p>   JSON file path (no shell escaping)
//   3. stdin                pipe or redirect (`echo ... | mpm call ...` or `< file`)
// If none are present, returns an empty payload (some tools need no input).
func handleCall(args []string) int {
	if len(args) < 1 {
		printError("usage: mpm call <tool_name> [--payload <json> | --payload-file <path>] | (stdin)")
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

// parsePayload extracts JSON from --payload flag, --payload-file flag, or stdin.
// If none are present, returns an empty map (some tools need no input).
//
// Stdin is detected via hasStdinData() (ModeCharDevice check), which correctly
// detects pipes and file redirection. The previous implementation used
// stat.Size() > 0, which silently failed for pipes — Stat returns Size=0 for
// pipes even when data is available, so the input was never read.
func parsePayload(args []string) (map[string]interface{}, error) {
	// Last-flag-wins for --payload and --payload-file, matching standard CLI
	// convention (git, kubectl, etc.). Iterate fully, remember the most
	// recent hit, return after the loop. If both are present, the later one
	// in argv order wins.
	var (
		inlineJSON  string
		inlineSet   bool
		filePath    string
		fileSet     bool
	)
	for i := 0; i < len(args); i++ {
		if args[i] == "--payload" && i+1 < len(args) {
			inlineJSON = args[i+1]
			inlineSet = true
			fileSet = false // --payload overrides any earlier --payload-file
			i++
			continue
		}
		if args[i] == "--payload-file" && i+1 < len(args) {
			filePath = args[i+1]
			fileSet = true
			inlineSet = false // --payload-file overrides any earlier --payload
			i++
			continue
		}
	}
	if inlineSet {
		var p map[string]interface{}
		if err := json.Unmarshal([]byte(inlineJSON), &p); err != nil {
			return nil, fmt.Errorf("--payload: %w", err)
		}
		return p, nil
	}
	if fileSet {
		data, err := os.ReadFile(filePath)
		if err != nil {
			return nil, fmt.Errorf("--payload-file: %w", err)
		}
		// Trim BOM if present (some editors write UTF-8 BOM)
		s := strings.TrimPrefix(string(data), "\xef\xbb\xbf")
		var p map[string]interface{}
		if err := json.Unmarshal([]byte(s), &p); err != nil {
			return nil, fmt.Errorf("--payload-file: %w", err)
		}
		return p, nil
	}
	// No flag; try stdin if data is piped or redirected.
	// hasStdinData uses ModeCharDevice — correct for both pipes (Size=0 but
	// has data) and file redirection (Size>0).
	if hasStdinData() {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return nil, fmt.Errorf("stdin: %w", err)
		}
		// Trim BOM if present (some editors write UTF-8 BOM)
		s := strings.TrimPrefix(string(data), "\xef\xbb\xbf")
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
		// Connection refused just means the web server isn't running —
		// delete stale port file and move on silently.
		if errors.Is(err, syscall.ECONNREFUSED) || strings.Contains(err.Error(), "connection refused") {
			if port == readActivePort() {
				_ = os.Remove(filepath.Join(config.GetMPMDir(), "web.port"))
			}
			return
		}
		// Other errors (timeout, DNS, etc.) are real problems — log to
		// audit ledger so the agent can see them across sessions. The
		// stderr note remains for live debugging.
		if dm, closeDM, dmErr := openCallDM(); dmErr == nil {
			dm.LogAudit(internal.AuditWarn, "relay", "broadcast failed: "+err.Error(), "", internal.AuditContext{
				"event":    eventType,
				"port":     port,
				"endpoint": "/api/internal/broadcast",
			})
			closeDM()
		}
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

	dm, closeDM, err := openCallDM()
	if err != nil {
		return nil, err
	}
	defer closeDM()

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

	dm, closeDM, err := openCallDM()
	if err != nil {
		return nil, err
	}
	defer closeDM()

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

	dm, closeDM, err := openCallDM()
	if err != nil {
		return nil, err
	}
	defer closeDM()

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

	dm, closeDM, err := openCallDM()
	if err != nil {
		return nil, err
	}
	defer closeDM()

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

	dm, closeDM, err := openCallDM()
	if err != nil {
		return nil, err
	}
	defer closeDM()

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

	dm, closeDM, err := openCallDM()
	if err != nil {
		return nil, err
	}
	defer closeDM()

	return dm.RecordDecision(
		internal.ParseStringOr(p["context"], ""),
		choice,
		internal.ParseStringOr(p["rationale"], ""),
		internal.ParseStringOr(p["outcome"], ""),
		tags,
		internal.ActiveContext{Mode: activeMode, Persona: activePersona},
	)
}

// ── Memory feedback / mutation tools ─────────────────────────────────────────
//
// Wire-format (all accept JSON payload via --payload or stdin):
//   shred_memory:        {"memory_id": "<id>"}
//   reinforce_memory:    {"memory_id": "<id>", "delta": 1}
//   weaken_memory:       {"memory_id": "<id>", "delta": 1}
//   snooze_memory:       {"memory_id": "<id>", "days": 1}
//   set_memory_weight:   {"memory_id": "<id>", "weight": 5}
//   patch_memory:        {"memory_id": "<id>", "patch": {"key": "value"}}
//   promote_memory:      {"memory_id": "<id>"}
//
// Added 2026-06-26 to close the agent feedback loop. Without these, agents
// had to shell `mpm reinforce <id>` etc., which forces them to invent CLI
// quoting and parse text output — neither works reliably across
// punctuation-heavy memory ids or non-ASCII content.

func callShredMemory(p map[string]interface{}) (interface{}, error) {
	id, _ := p["memory_id"].(string)
	dm, closeDM, err := openCallDM()
	if err != nil {
		return nil, err
	}
	defer closeDM()
	return dm.ShredMemoryWithCascade(id)
}

func callReinforceMemory(p map[string]interface{}) (interface{}, error) {
	id, _ := p["memory_id"].(string)
	delta := int(internal.ParseFloatOr(p["delta"], 1))
	dm, closeDM, err := openCallDM()
	if err != nil {
		return nil, err
	}
	defer closeDM()
	return dm.ReinforceMemoryTool(id, delta)
}

func callWeakenMemory(p map[string]interface{}) (interface{}, error) {
	id, _ := p["memory_id"].(string)
	delta := int(internal.ParseFloatOr(p["delta"], 1))
	dm, closeDM, err := openCallDM()
	if err != nil {
		return nil, err
	}
	defer closeDM()
	return dm.WeakenMemoryTool(id, delta)
}

func callSnoozeMemory(p map[string]interface{}) (interface{}, error) {
	id, _ := p["memory_id"].(string)
	days := int(internal.ParseFloatOr(p["days"], 1))
	dm, closeDM, err := openCallDM()
	if err != nil {
		return nil, err
	}
	defer closeDM()
	return dm.SnoozeMemory(id, days)
}

func callSetMemoryWeight(p map[string]interface{}) (interface{}, error) {
	id, _ := p["memory_id"].(string)
	weight := int(internal.ParseFloatOr(p["weight"], 0))
	dm, closeDM, err := openCallDM()
	if err != nil {
		return nil, err
	}
	defer closeDM()
	return dm.SetMemoryWeight(id, weight)
}

func callPatchMemory(p map[string]interface{}) (interface{}, error) {
	id, _ := p["memory_id"].(string)
	patch, _ := p["patch"]
	// patch must be a JSON object (map). The DM layer takes a string,
	// so marshal here. A nil/primitive patch is rejected upstream by
	// the DM (UpdateMemoryMetadata validates the prefix).
	patchJSON, err := json.Marshal(patch)
	if err != nil {
		return nil, fmt.Errorf("patch must be JSON-marshalable: %w", err)
	}
	dm, closeDM, err := openCallDM()
	if err != nil {
		return nil, err
	}
	defer closeDM()
	return dm.PatchMemoryMetadata(id, string(patchJSON))
}

func callPromoteMemory(p map[string]interface{}) (interface{}, error) {
	id, _ := p["memory_id"].(string)
	dm, closeDM, err := openCallDM()
	if err != nil {
		return nil, err
	}
	defer closeDM()
	return dm.PromoteMemory(id)
}

// callReviewMemories returns memories due for spaced reinforcement review.
// Wire-format: {"days": 30, "limit": 20} — both optional with sensible defaults.
func callReviewMemories(p map[string]interface{}) (interface{}, error) {
	days := int(internal.ParseFloatOr(p["days"], 30))
	limit := int(internal.ParseFloatOr(p["limit"], 20))
	dm, closeDM, err := openCallDM()
	if err != nil {
		return nil, err
	}
	defer closeDM()
	return dm.ReviewMemories(days, limit)
}

// callSynthesizeMemory runs LLM-driven merge synthesis for one memory.
// Wire-format: {"memory_id": "<id>"}. Lazy SynthClient creation; requires
// MINIMAX_API_KEY or OPENAI_API_KEY in env to actually invoke the LLM.
func callSynthesizeMemory(p map[string]interface{}) (interface{}, error) {
	id, _ := p["memory_id"].(string)
	dm, closeDM, err := openCallDM()
	if err != nil {
		return nil, err
	}
	defer closeDM()
	return dm.SynthesizeMemoryFor(context.Background(), id)
}

// callGCRun wraps dm.RunGC with a typed payload. Safe defaults:
// dry_run=true (no writes), aggressive=false, max_age_hours=24.
// Callers must explicitly set dry_run=false to mutate state. The full
// CLI flag surface (--review, --purge, --shred-negative) stays on
// `mpm gc` because those modes are operationally distinct.
func callGCRun(p map[string]interface{}) (interface{}, error) {
	dryRun := parseBoolDefault(p["dry_run"], true) // safe default
	aggressive := parseBoolDefault(p["aggressive"], false)
	maxAge := int(internal.ParseFloatOr(p["max_age_hours"], 24))

	dm, closeDM, err := openCallDM()
	if err != nil {
		return nil, err
	}
	defer closeDM()

	out, err := dm.RunGC(internal.GCOptions{
		DryRun:      dryRun,
		Aggressive:  aggressive,
		MaxAgeHours: maxAge,
	})
	if err != nil {
		return nil, err
	}

	result := map[string]interface{}{
		"success":         true,
		"dry_run":         dryRun,
		"cooldown_skip":   out.CooldownSkip,
		"ran":             out.Ran,
		"scanned":         out.Scanned,
		"updated":         out.Updated,
		"audit_pruned":    out.AuditPruned,
		"handoff_pruned":  out.HandoffPruned,
		"dead_memory_count": len(out.DeadMemories),
		"dead_memories":   out.DeadMemories,
	}
	if out.LastGCRan != nil {
		result["last_gc_ran"] = out.LastGCRan.Format(time.RFC3339)
	}
	return result, nil
}

// parseBoolDefault extracts a bool from the payload, falling back to def.
// Accepts both native bool (JSON true/false) and the int-shaped values
// some callers emit (0/1, "true"/"false"). Lenient on purpose — payload
// shape across MCP / mpm call / openclaw-plugin isn't strictly uniform.
func parseBoolDefault(v interface{}, def bool) bool {
	switch t := v.(type) {
	case bool:
		return t
	case float64:
		return t != 0
	case int:
		return t != 0
	case string:
		switch t {
		case "true", "True", "TRUE", "1", "yes":
			return true
		case "false", "False", "FALSE", "0", "no", "":
			return false
		}
	}
	return def
}

// callSaveLesson persists a lesson to MPM.
func callSaveLesson(p map[string]interface{}) (interface{}, error) {
	fact, _ := p["fact"].(string)
	if fact == "" {
		return nil, fmt.Errorf("fact is required")
	}
	lessonType := internal.ParseStringOr(p["type"], "insight")
	tags := internal.ParseStringSliceOr(p["tags"])

	dm, closeDM, err := openCallDM()
	if err != nil {
		return nil, err
	}
	defer closeDM()

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

	dm, closeDM, err := openCallDM()
	if err != nil {
		return nil, err
	}
	defer closeDM()

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

	dm, closeDM, err := openCallDM()
	if err != nil {
		return nil, err
	}
	defer closeDM()

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

	dm, closeDM, err := openCallDM()
	if err != nil {
		return nil, err
	}
	defer closeDM()

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

	dm, closeDM, err := openCallDM()
	if err != nil {
		return nil, err
	}
	defer closeDM()

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

	dm, closeDM, err := openCallDM()
	if err != nil {
		return nil, err
	}
	defer closeDM()

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

	dm, closeDM, err := openCallDM()
	if err != nil {
		return nil, err
	}
	defer closeDM()

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

	dm, closeDM, err := openCallDM()
	if err != nil {
		return nil, err
	}
	defer closeDM()

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

	dm, closeDM, err := openCallDM()
	if err != nil {
		return nil, err
	}
	defer closeDM()

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
	dm, closeDM, err := openCallDM()
	if err != nil {
		return nil, err
	}
	defer closeDM()

	data, err := dm.GatherWakeContext()
	if err != nil {
		return nil, fmt.Errorf("gather wake context: %w", err)
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
		"audit_summary":   data.AuditSummary,
		"last_handoff":    data.LastHandoff,
	}, nil
}

// callReadDirectives returns prime directives.
func callReadDirectives(_ map[string]interface{}) (interface{}, error) {
	dm, closeDM, err := openCallDM()
	if err != nil {
		return nil, err
	}
	defer closeDM()

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

	dm, closeDM, err := openCallDM()
	if err != nil {
		return nil, err
	}
	defer closeDM()

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
		return nil, fmt.Errorf("new router: %w", err)
	}

	return router.Evaluate(prompt), nil
}

// ── Evidence / Confidence ───────────────────────────────────────────────────
//
// These handlers expose the v1 confidence/evidence foundation over the
// universal `mpm call` machine interface so OpenClaw agents can add
// evidence, list it, and inspect the confidence timeline. The recompute
// is synchronous (in v1 the SQLite trigger is a no-op due to a documented
// connection-locking issue, so internal.AddEvidence calls RecomputeConfidence
// directly).

// callAddEvidence inserts a new evidence row and returns the resulting
// confidence. Thin shim over dm.AddEvidence.
func callAddEvidence(payload map[string]interface{}) (interface{}, error) {
	artifactType, _ := payload["artifact_type"].(string)
	if artifactType == "" {
		artifactType = "memory"
	}
	var strength float64
	if s, ok := payload["strength"].(float64); ok {
		strength = s
	}
	var independence float64 = 1.0
	if i, ok := payload["independence_factor"].(float64); ok {
		independence = i
	}

	dm, closeDM, err := openCallDM()
	if err != nil {
		return nil, err
	}
	defer closeDM()

	return dm.AddEvidence(internal.EvidenceInput{
		ArtifactID:         getString(payload, "artifact_id"),
		ArtifactType:       artifactType,
		Type:               getString(payload, "type"),
		SourceGroup:        getString(payload, "source_group"),
		Strength:           strength,
		IndependenceFactor: independence,
		CreatedBy:          getString(payload, "created_by"),
		CreatedAt:          time.Now(),
		Notes:              getString(payload, "notes"),
	})
}

// callListEvidence returns all evidence rows for an artifact. Thin shim
// over dm.ListEvidence.
func callListEvidence(payload map[string]interface{}) (interface{}, error) {
	dm, closeDM, err := openCallDM()
	if err != nil {
		return nil, err
	}
	defer closeDM()
	return dm.ListEvidence(getString(payload, "artifact_id"), getString(payload, "artifact_type"))
}

// callQueryConfidenceHistory returns the confidence timeline for an artifact.
func callQueryConfidenceHistory(payload map[string]interface{}) (interface{}, error) {
	limit := 50
	if l, ok := payload["limit"].(float64); ok && l > 0 {
		limit = int(l)
	}
	dm, closeDM, err := openCallDM()
	if err != nil {
		return nil, err
	}
	defer closeDM()
	return dm.QueryConfidenceHistory(getString(payload, "artifact_id"), getString(payload, "artifact_type"), limit)
}

// callQueryConfidenceChanges returns recent confidence-altering events
// with delta and trigger.
func callQueryConfidenceChanges(payload map[string]interface{}) (interface{}, error) {
	var filter internal.ConfidenceChangesFilter
	if secs, ok := payload["since_seconds_ago"].(float64); ok && secs > 0 {
		filter.Since = time.Now().Add(-time.Duration(secs) * time.Second)
	} else if sinceF, ok := payload["since"].(float64); ok && sinceF > 0 {
		filter.Since = time.Unix(int64(sinceF), 0)
	}
	if l, ok := payload["limit"].(float64); ok && l > 0 {
		filter.Limit = int(l)
	}
	if v, ok := payload["artifact_id"].(string); ok {
		filter.ArtifactID = v
	}
	if v, ok := payload["artifact_type"].(string); ok {
		filter.ArtifactType = v
	}

	dm, closeDM, err := openCallDM()
	if err != nil {
		return nil, err
	}
	defer closeDM()
	return dm.QueryConfidenceChanges(filter)
}

// callQueryConfidenceTrend returns the trajectory projection of confidence
// over a time window. Completes the orthogonal set:
//   state  → explain_confidence       (current reasoning trace)
//   cause  → query_confidence_changes (recent events with delta)
//   history → query_confidence_history (full timeline)
//   direction → query_confidence_trend (this: trajectory, velocity)
//
// velocity is the raw signal; trend is the human-readable label.
// Agents reason better from velocity than from labels.
//
// Optional payload: window_days (default 30).
func callQueryConfidenceTrend(payload map[string]interface{}) (interface{}, error) {
	dm, closeDM, err := openCallDM()
	if err != nil {
		return nil, err
	}
	defer closeDM()
	windowDays := 30
	if w, ok := payload["window_days"].(float64); ok && w > 0 {
		windowDays = int(w)
	}
	return dm.QueryConfidenceTrend(getString(payload, "artifact_id"), getString(payload, "artifact_type"), windowDays)
}

// callQueryMemoryQuality returns per-creator memory statistics.
func callQueryMemoryQuality(payload map[string]interface{}) (interface{}, error) {
	dm, closeDM, err := openCallDM()
	if err != nil {
		return nil, err
	}
	defer closeDM()
	return dm.QueryMemoryQuality()
}

// callShowConfidence returns the current confidence and history for an artifact.
func callShowConfidence(payload map[string]interface{}) (interface{}, error) {
	dm, closeDM, err := openCallDM()
	if err != nil {
		return nil, err
	}
	defer closeDM()
	return dm.ShowConfidence(getString(payload,"artifact_id"), getString(payload,"artifact_type"))
}

// callRecomputeConfidence forces a manual recompute and returns the snapshot.
func callRecomputeConfidence(payload map[string]interface{}) (interface{}, error) {
	dm, closeDM, err := openCallDM()
	if err != nil {
		return nil, err
	}
	defer closeDM()
	return dm.RecomputeConfidence(getString(payload,"artifact_id"), getString(payload,"artifact_type"))
}

// callExplainConfidence returns the reasoning trace for an artifact's confidence.
func callExplainConfidence(payload map[string]interface{}) (interface{}, error) {
	dm, closeDM, err := openCallDM()
	if err != nil {
		return nil, err
	}
	defer closeDM()
	return dm.ExplainConfidence(getString(payload,"artifact_id"), getString(payload,"artifact_type"))
}

// ── Release / Changelog ──────────────────────────────────────────────
//
// callLogToChangelog writes a changelog memory tied to a specific
// git commit. Mirrors the MCP log_to_changelog tool exactly — the
// CLI/mcp parity is enforced by routing both through
// DatabaseManager.LogChangelogEntry, which holds the strict
// retrospective contract (full 40-char SHA-1 required). When the
// synthesis engine lands, both the MCP tool and this CLI handler
// will be joined with the git log via the (commit_hash,
// mpm_memory_id) key in changelog.json.
func callLogToChangelog(p map[string]interface{}) (interface{}, error) {
	fact, _ := p["fact"].(string)
	commitHash, _ := p["commit_hash"].(string)
	if fact == "" {
		return nil, fmt.Errorf("fact is required")
	}
	if commitHash == "" {
		return nil, fmt.Errorf("commit_hash is required (strict retrospective contract: every changelog memory must reference an existing commit). Run `git rev-parse HEAD` to get the canonical 40-char SHA-1")
	}

	dm, closeDM, err := openCallDM()
	if err != nil {
		return nil, err
	}
	defer closeDM()

	id, err := dm.LogChangelogEntry(fact, commitHash, internal.ParseStringSliceOr(p["tags"]))
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{
		"success":     true,
		"id":          id,
		"commit_hash": commitHash,
		"collection":  "changelog",
	}, nil
}

// callQueryAuditLog returns recent entries from system_audit_log. The
// agent uses this to investigate what went wrong, especially across
// sessions — the wake context surface only shows a count, the details
// come from this tool.
//
// Args:
//
//	--level      (optional) one of warn|error|fatal; default: any
//	--component (optional) subsystem name (e.g. "relay", "synthesis",
//	             "watcher", "security"); default: any
//	--days       (optional) lookback window in days; default 1
//	--limit      (optional) max rows; default 20, max 500
func callQueryAuditLog(p map[string]interface{}) (interface{}, error) {
	levelStr := getString(p, "level")
	component := getString(p, "component")
	days := 1
	if v, ok := p["days"]; ok {
		switch t := v.(type) {
		case float64:
			days = int(t)
		case int:
			days = t
		}
	}
	limit := 20
	if v, ok := p["limit"]; ok {
		switch t := v.(type) {
		case float64:
			limit = int(t)
		case int:
			limit = t
		}
	}

	dm, closeDM, err := openCallDM()
	if err != nil {
		return nil, err
	}
	defer closeDM()

	items, err := dm.QueryAuditLog(internal.AuditLevel(levelStr), component, days, limit)
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{
		"success": true,
		"count":   len(items),
		"results": items,
	}, nil
}

// callSessionEnd writes a handoff for the just-ended session. The agent
// calls this before exiting so the next session can pick up the thread.
//
// Args:
//
//	--session_id     (required) opaque session identifier (UUID is fine)
//	--summary        (required) 1-3 sentence description of what was done
//	--state          (optional) clean | crashed | interrupted | force_end; default clean
//	--commitments    (optional) JSON array of strings; things this session committed to do
//	--open_questions (optional) JSON array of strings; things still unresolved
func callSessionEnd(p map[string]interface{}) (interface{}, error) {
	sessionID := getString(p, "session_id")
	if sessionID == "" {
		return nil, fmt.Errorf("session_id is required")
	}
	summary := getString(p, "summary")
	if summary == "" {
		return nil, fmt.Errorf("summary is required")
	}
	state := getString(p, "state")
	if state == "" {
		state = internal.HandoffClean
	}

	var commitments []string
	if v, ok := p["commitments"]; ok {
		if arr, ok := v.([]interface{}); ok {
			for _, item := range arr {
				if s, ok := item.(string); ok && s != "" {
					commitments = append(commitments, s)
				}
			}
		}
	}
	var openQuestions []string
	if v, ok := p["open_questions"]; ok {
		if arr, ok := v.([]interface{}); ok {
			for _, item := range arr {
				if s, ok := item.(string); ok && s != "" {
					openQuestions = append(openQuestions, s)
				}
			}
		}
	}

	dm, closeDM, err := openCallDM()
	if err != nil {
		return nil, err
	}
	defer closeDM()

	h, err := dm.EndSession(sessionID, summary, state, commitments, openQuestions)
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{
		"success":        true,
		"handoff":        h,
		"handoff_id":     h.ID,
		"message":        "session ended; handoff written. Next wake will surface it.",
	}, nil
}

// callSessionHandoff returns the most recent handoff. The agent's wake
// context surfaces unread handoffs automatically, but this tool is
// available for explicit re-reads of any handoff (read or unread).
//
// Args:
//
//	--mark_read (optional) "true" to mark the returned handoff as read
//	             after returning; default false. The wake context marks
//	             its own reads — this tool does not by default so the
//	             agent can browse the handoff history without consuming
//	             the wake context's handoff.
//	--unread    (optional) "true" to return only unread handoffs;
//	             default false (returns latest regardless of read state)
func callSessionHandoff(p map[string]interface{}) (interface{}, error) {
	markRead := false
	if v, ok := p["mark_read"]; ok {
		if b, ok := v.(bool); ok {
			markRead = b
		}
	}
	unreadOnly := false
	if v, ok := p["unread"]; ok {
		if b, ok := v.(bool); ok {
			unreadOnly = b
		}
	}

	dm, closeDM, err := openCallDM()
	if err != nil {
		return nil, err
	}
	defer closeDM()

	var h *internal.Handoff
	if unreadOnly {
		h, err = dm.GetLatestUnreadHandoff()
	} else {
		h, err = dm.GetLatestHandoff()
	}
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return map[string]interface{}{
				"success": true,
				"handoff": nil,
				"message": "no handoff found",
			}, nil
		}
		return nil, err
	}
	if markRead {
		// Best-effort mark-read; don't fail the call if marking fails
		// because the caller explicitly opted in. Idempotent.
		_ = dm.MarkHandoffRead(h.ID, "manual-call")
		h.ReadAt = ptrTime(time.Now().UTC())
		h.ReadBy = "manual-call"
	}
	return map[string]interface{}{
		"success": true,
		"handoff": h,
	}, nil
}

// ptrTime is a small helper for the session_handoff tool.
func ptrTime(t time.Time) *time.Time { return &t }

// callListHandoffs returns recent handoffs. Useful for the agent to see
// the history of its own sessions.
//
// Args:
//
//	--limit      (optional) max handoffs; default 10, max 500
//	--unread     (optional) "true" to filter to unread; default false
func callListHandoffs(p map[string]interface{}) (interface{}, error) {
	limit := 10
	if v, ok := p["limit"]; ok {
		switch t := v.(type) {
		case float64:
			limit = int(t)
		case int:
			limit = t
		}
	}
	unreadOnly := false
	if v, ok := p["unread"]; ok {
		if b, ok := v.(bool); ok {
			unreadOnly = b
		}
	}

	dm, closeDM, err := openCallDM()
	if err != nil {
		return nil, err
	}
	defer closeDM()

	items, err := dm.ListHandoffs(limit, unreadOnly)
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{
		"success": true,
		"count":   len(items),
		"results": items,
	}, nil
}
