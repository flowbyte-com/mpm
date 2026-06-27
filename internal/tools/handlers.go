package tools

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	mpminternal "mpm/internal"
	"mpm/internal"
)

// handlers.go contains the unified tool handlers that power both
// `mpm call <tool>` (CLI) and the MCP server. Each handler takes a
// DatabaseManager + ActiveContext + payload, calls the appropriate
// internal/ method, and returns a JSON-marshallable result.
//
// Conventions:
//   - Read payload args with the `internal.Parse*Or` helpers (they
//     default-when-missing / type-coerce consistently).
//   - Return (result, nil) on success; (nil, err) on failure.
//   - Use `ac` for write provenance; do NOT read global mode/persona vars.
//   - Don't open/close the DB — the dispatcher owns that lifetime.

func handleSaveToMemory(dm *mpminternal.DatabaseManager, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	fact, _ := p["fact"].(string)
	if fact == "" {
		return nil, fmt.Errorf("fact is required")
	}


	out, mem, err := dm.SaveMemoryWithContext(
		fact,
		internal.ParseStringOr(p["collection"], "memories"),
		internal.ParseStringSliceOr(p["tags"]),
		internal.ParseFloatOr(p["weight"], 0.5),
		internal.ParseStringOr(p["ttl"], ""),
		ac,
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
func handleQueryLongTermMemory(dm *mpminternal.DatabaseManager, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	query, _ := p["query"].(string)
	if query == "" {
		return nil, fmt.Errorf("query is required")
	}
	limit := int(internal.ParseFloatOr(p["limit"], 5))
	if limit <= 0 {
		limit = 5
	}
	collection, _ := p["collection"].(string)


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
func handleChallengeMemory(dm *mpminternal.DatabaseManager, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	memoryID, _ := p["memoryId"].(string)
	if memoryID == "" {
		return nil, fmt.Errorf("memoryId is required")
	}
	evidence, _ := p["evidence"].(string)


	return dm.ChallengeMemoryWithTheory(memoryID, evidence)
}

// callProposeTheory logs a hypothesis with validation criteria.
func handleProposeTheory(dm *mpminternal.DatabaseManager, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	hypothesis, _ := p["hypothesis"].(string)
	if hypothesis == "" {
		return nil, fmt.Errorf("hypothesis is required")
	}
	validationCriteria, _ := p["validation_criteria"].(string)
	tags := internal.ParseStringSliceOr(p["tags"])
	if tags == nil {
		tags = []string{}
	}


	return dm.ProposeTheory(hypothesis, validationCriteria, tags)
}

// callResolveTheory marks a theory as proven or disproven.
func handleResolveTheory(dm *mpminternal.DatabaseManager, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
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


	return dm.ResolveTheory(theoryID, conclusion, newStatus)
}

// callRecordDecision logs a decision with context, choice, and rationale.
func handleRecordDecision(dm *mpminternal.DatabaseManager, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	choice, _ := p["choice"].(string)
	if choice == "" {
		return nil, fmt.Errorf("choice is required")
	}
	tags := internal.ParseStringSliceOr(p["tags"])
	if tags == nil {
		tags = []string{}
	}


	return dm.RecordDecision(
		internal.ParseStringOr(p["context"], ""),
		choice,
		internal.ParseStringOr(p["rationale"], ""),
		internal.ParseStringOr(p["outcome"], ""),
		tags,
		ac,
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

func handleShredMemory(dm *mpminternal.DatabaseManager, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	id, _ := p["memory_id"].(string)
	return dm.ShredMemoryWithCascade(id)
}

func handleReinforceMemory(dm *mpminternal.DatabaseManager, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	id, _ := p["memory_id"].(string)
	delta := int(internal.ParseFloatOr(p["delta"], 1))
	return dm.ReinforceMemoryTool(id, delta)
}

func handleWeakenMemory(dm *mpminternal.DatabaseManager, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	id, _ := p["memory_id"].(string)
	delta := int(internal.ParseFloatOr(p["delta"], 1))
	return dm.WeakenMemoryTool(id, delta)
}

func handleSnoozeMemory(dm *mpminternal.DatabaseManager, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	id, _ := p["memory_id"].(string)
	days := int(internal.ParseFloatOr(p["days"], 1))
	return dm.SnoozeMemory(id, days)
}

func handleSetMemoryWeight(dm *mpminternal.DatabaseManager, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	id, _ := p["memory_id"].(string)
	weight := int(internal.ParseFloatOr(p["weight"], 0))
	return dm.SetMemoryWeight(id, weight)
}

func handlePatchMemory(dm *mpminternal.DatabaseManager, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	id, _ := p["memory_id"].(string)
	patch, _ := p["patch"]
	// patch must be a JSON object (map). The DM layer takes a string,
	// so marshal here. A nil/primitive patch is rejected upstream by
	// the DM (UpdateMemoryMetadata validates the prefix).
	patchJSON, err := json.Marshal(patch)
	if err != nil {
		return nil, fmt.Errorf("patch must be JSON-marshalable: %w", err)
	}
	return dm.PatchMemoryMetadata(id, string(patchJSON))
}

func handlePromoteMemory(dm *mpminternal.DatabaseManager, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	id, _ := p["memory_id"].(string)
	return dm.PromoteMemory(id)
}

// callReviewMemories returns memories due for spaced reinforcement review.
// Wire-format: {"days": 30, "limit": 20} — both optional with sensible defaults.
func handleReviewMemories(dm *mpminternal.DatabaseManager, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	days := int(internal.ParseFloatOr(p["days"], 30))
	limit := int(internal.ParseFloatOr(p["limit"], 20))
	return dm.ReviewMemories(days, limit)
}

// callSynthesizeMemory runs LLM-driven merge synthesis for one memory.
// Wire-format: {"memory_id": "<id>"}. Lazy SynthClient creation; requires
// MINIMAX_API_KEY or OPENAI_API_KEY in env to actually invoke the LLM.
func handleSynthesizeMemory(dm *mpminternal.DatabaseManager, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	id, _ := p["memory_id"].(string)
	return dm.SynthesizeMemoryFor(context.Background(), id)
}

// callGCRun wraps dm.RunGC with a typed payload. Safe defaults:
// dry_run=true (no writes), aggressive=false, max_age_hours=24.
// Callers must explicitly set dry_run=false to mutate state. The full
// CLI flag surface (--review, --purge, --shred-negative) stays on
// `mpm gc` because those modes are operationally distinct.
func handleGCRun(dm *mpminternal.DatabaseManager, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	dryRun := parseBoolDefault(p["dry_run"], true) // safe default
	aggressive := parseBoolDefault(p["aggressive"], false)
	maxAge := int(internal.ParseFloatOr(p["max_age_hours"], 24))


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
func handleSaveLesson(dm *mpminternal.DatabaseManager, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	fact, _ := p["fact"].(string)
	if fact == "" {
		return nil, fmt.Errorf("fact is required")
	}
	lessonType := internal.ParseStringOr(p["type"], "insight")
	tags := internal.ParseStringSliceOr(p["tags"])


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
func handleSearchLessons(dm *mpminternal.DatabaseManager, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	query, _ := p["query"].(string)
	if query == "" {
		return nil, fmt.Errorf("query is required")
	}


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
func handleListLessons(dm *mpminternal.DatabaseManager, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	lessonType := internal.ParseStringOr(p["type"], "")


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
func handleCreateTopic(dm *mpminternal.DatabaseManager, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	name, _ := p["name"].(string)
	if name == "" {
		return nil, fmt.Errorf("name is required")
	}
	description := internal.ParseStringOr(p["description"], "")


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
func handleSearchTopics(dm *mpminternal.DatabaseManager, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	query, _ := p["query"].(string)
	limit := int(internal.ParseFloatOr(p["limit"], 20))
	if limit <= 0 {
		limit = 20
	}


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
func handleLinkTopic(dm *mpminternal.DatabaseManager, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	memoryID, _ := p["memory_id"].(string)
	if memoryID == "" {
		return nil, fmt.Errorf("memory_id is required")
	}
	topicID, _ := p["topic_id"].(string)
	if topicID == "" {
		return nil, fmt.Errorf("topic_id is required")
	}


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
func handleAddReference(dm *mpminternal.DatabaseManager, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	filepath, _ := p["filepath"].(string)
	if filepath == "" {
		return nil, fmt.Errorf("filepath is required")
	}
	title := internal.ParseStringOr(p["title"], "")


	return dm.AddReferenceFromFile(filepath, title)
}

// callSearchReferences searches reference content.
func handleSearchReferences(dm *mpminternal.DatabaseManager, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	query, _ := p["query"].(string)
	if query == "" {
		return nil, fmt.Errorf("query is required")
	}
	limit := int(internal.ParseFloatOr(p["limit"], 5))
	if limit <= 0 {
		limit = 5
	}


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
func handleListReferences(dm *mpminternal.DatabaseManager, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	limit := int(internal.ParseFloatOr(p["limit"], 50))
	if limit <= 0 {
		limit = 50
	}
	offset := int(internal.ParseFloatOr(p["offset"], 0))
	if offset < 0 {
		offset = 0
	}


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
func handleReadWakeContext(dm *mpminternal.DatabaseManager, ac mpminternal.ActiveContext, _ map[string]interface{}) (interface{}, error) {

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
func handleReadDirectives(dm *mpminternal.DatabaseManager, ac mpminternal.ActiveContext, _ map[string]interface{}) (interface{}, error) {

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
func handleProactiveRecallHint(dm *mpminternal.DatabaseManager, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	conversationText, _ := p["conversation_text"].(string)
	if conversationText == "" {
		return nil, fmt.Errorf("conversation_text is required")
	}
	maxHints := int(internal.ParseFloatOr(p["max_hints"], 3))
	if maxHints <= 0 {
		maxHints = 3
	}


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
func handleRoute(dm *mpminternal.DatabaseManager, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
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
func handleAddEvidence(dm *mpminternal.DatabaseManager, ac mpminternal.ActiveContext, payload map[string]interface{}) (interface{}, error) {
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
func handleListEvidence(dm *mpminternal.DatabaseManager, ac mpminternal.ActiveContext, payload map[string]interface{}) (interface{}, error) {
	return dm.ListEvidence(getString(payload, "artifact_id"), getString(payload, "artifact_type"))
}

// callQueryConfidenceHistory returns the confidence timeline for an artifact.
func handleQueryConfidenceHistory(dm *mpminternal.DatabaseManager, ac mpminternal.ActiveContext, payload map[string]interface{}) (interface{}, error) {
	limit := 50
	if l, ok := payload["limit"].(float64); ok && l > 0 {
		limit = int(l)
	}
	return dm.QueryConfidenceHistory(getString(payload, "artifact_id"), getString(payload, "artifact_type"), limit)
}

// callQueryConfidenceChanges returns recent confidence-altering events
// with delta and trigger.
func handleQueryConfidenceChanges(dm *mpminternal.DatabaseManager, ac mpminternal.ActiveContext, payload map[string]interface{}) (interface{}, error) {
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
func handleQueryConfidenceTrend(dm *mpminternal.DatabaseManager, ac mpminternal.ActiveContext, payload map[string]interface{}) (interface{}, error) {
	windowDays := 30
	if w, ok := payload["window_days"].(float64); ok && w > 0 {
		windowDays = int(w)
	}
	return dm.QueryConfidenceTrend(getString(payload, "artifact_id"), getString(payload, "artifact_type"), windowDays)
}

// callQueryMemoryQuality returns per-creator memory statistics.
func handleQueryMemoryQuality(dm *mpminternal.DatabaseManager, ac mpminternal.ActiveContext, payload map[string]interface{}) (interface{}, error) {
	return dm.QueryMemoryQuality()
}

// callShowConfidence returns the current confidence and history for an artifact.
func handleShowConfidence(dm *mpminternal.DatabaseManager, ac mpminternal.ActiveContext, payload map[string]interface{}) (interface{}, error) {
	return dm.ShowConfidence(getString(payload,"artifact_id"), getString(payload,"artifact_type"))
}

// callRecomputeConfidence forces a manual recompute and returns the snapshot.
func handleRecomputeConfidence(dm *mpminternal.DatabaseManager, ac mpminternal.ActiveContext, payload map[string]interface{}) (interface{}, error) {
	return dm.RecomputeConfidence(getString(payload,"artifact_id"), getString(payload,"artifact_type"))
}

// callExplainConfidence returns the reasoning trace for an artifact's confidence.
func handleExplainConfidence(dm *mpminternal.DatabaseManager, ac mpminternal.ActiveContext, payload map[string]interface{}) (interface{}, error) {
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
func handleLogToChangelog(dm *mpminternal.DatabaseManager, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	fact, _ := p["fact"].(string)
	commitHash, _ := p["commit_hash"].(string)
	if fact == "" {
		return nil, fmt.Errorf("fact is required")
	}
	if commitHash == "" {
		return nil, fmt.Errorf("commit_hash is required (strict retrospective contract: every changelog memory must reference an existing commit). Run `git rev-parse HEAD` to get the canonical 40-char SHA-1")
	}


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
func handleQueryAuditLog(dm *mpminternal.DatabaseManager, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
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
func handleSessionEnd(dm *mpminternal.DatabaseManager, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
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
func handleSessionHandoff(dm *mpminternal.DatabaseManager, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
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


	var h *internal.Handoff
	var err error
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
func handleListHandoffs(dm *mpminternal.DatabaseManager, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
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
