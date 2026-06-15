// call_helpers.go — High-level dm methods that back the mpm call CLI and the MCP server.
// Mirrors wake_context.go: data-gathering logic and wire-format concerns live here so the
// two surfaces stay in lockstep. Each method takes a *DatabaseManager via receiver.
// Active-context injection is read from internal.ActiveContext set by the caller.
package internal

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// ActiveContext carries the agent's active mode/persona for provenance
// injection on memory writes. Mirrors the package-level globals in
// cmd/mpm/handlers.go (activeMode, activePersona). Callers set fields
// before invoking write methods; reads are safe with zero value.
type ActiveContext struct {
	Mode    string
	Persona string
}

func (ac ActiveContext) provenanceMeta() map[string]interface{} {
	return map[string]interface{}{
		"provenance": map[string]interface{}{
			"source":  "agent",
			"model":   "call",
			"compute": "relative",
			"agent":   "mpm_call",
		},
	}
}

func (ac ActiveContext) withActiveContextMeta(meta map[string]interface{}) map[string]interface{} {
	if meta == nil {
		meta = map[string]interface{}{}
	}
	for k, v := range ac.provenanceMeta() {
		meta[k] = v
	}
	if ac.Mode != "" {
		meta["active_mode"] = ac.Mode
	}
	if ac.Persona != "" {
		meta["active_persona"] = ac.Persona
	}
	return meta
}

// SaveMemoryWithContext persists a fact to memory, injecting provenance
// and active context into metadata. weight (0–1 float) is recorded in
// meta.weight_intent since MemoryStore.AddMemory has no weight param.
func (dm *DatabaseManager) SaveMemoryWithContext(
	fact, collection string,
	tags []string,
	weight float64,
	ttl string,
	ac ActiveContext,
) (map[string]interface{}, *Memory, error) {
	if collection == "" {
		collection = "memories"
	}
	if weight <= 0 {
		weight = 0.5
	}

	meta := ac.withActiveContextMeta(nil)

	store, err := dm.getSharedStore()
	if err != nil {
		return nil, nil, fmt.Errorf("get memory store: %w", err)
	}

	if weight > 0 {
		meta["weight_intent"] = int(weight * 10)
	}

	mem, err := store.AddMemory(fact, collection, tags, meta, "", "call")
	if err != nil {
		return nil, nil, fmt.Errorf("add memory: %w", err)
	}

	if ttl != "" {
		if dur, err := parseDurationString(ttl); err == nil && dur > 0 {
			dm.SetMemoryTTL(mem.ID, time.Now().Add(dur))
		}
	}

	return map[string]interface{}{
		"success": true,
		"id":      mem.ID,
		"content": mem.Content,
		"weight":  mem.Weight,
		"tags":    mem.Tags,
	}, mem, nil
}

// HybridSearchMemories runs hybrid (BM25 + semantic) search with FTS fallback.
// Named to avoid collision with dm.SearchMemories in web_db.go.
func (dm *DatabaseManager) HybridSearchMemories(query, collection string, limit int) ([]map[string]interface{}, error) {
	if limit <= 0 {
		limit = 5
	}
	store, err := dm.getSharedStore()
	if err != nil {
		return nil, fmt.Errorf("get memory store: %w", err)
	}
	mems, err := store.HybridSearch(query, collection, limit)
	if err != nil {
		mems, err = store.FullTextSearch(query, collection, limit)
		if err != nil {
			return nil, fmt.Errorf("search: %w", err)
		}
	}
	items := make([]map[string]interface{}, 0, len(mems))
	for _, m := range mems {
		items = append(items, map[string]interface{}{
			"id":         m.ID,
			"content":    m.Content,
			"weight":     m.Weight,
			"tags":       m.Tags,
			"collection": m.Collection,
		})
	}
	return items, nil
}

// SaveLesson persists a lesson. Mirrors callSaveLesson.
func (dm *DatabaseManager) SaveLesson(fact, lessonType string, tags []string) (map[string]interface{}, *Lesson, error) {
	if lessonType == "" {
		lessonType = "insight"
	}
	lesson, err := dm.AddLesson(fact, LessonType(lessonType), tags, "")
	if err != nil {
		return nil, nil, fmt.Errorf("add lesson: %w", err)
	}
	return map[string]interface{}{
		"success":       true,
		"id":            lesson.ID,
		"type":          string(lesson.Type),
		"reinforcement": lesson.ReinforcementCount,
	}, lesson, nil
}

// SearchLessonsLimited is a convenience wrapper that defaults limit to 10.
func (dm *DatabaseManager) SearchLessonsLimited(query string) ([]map[string]interface{}, error) {
	lessons, err := dm.SearchLessons(query, 10)
	if err != nil {
		return nil, fmt.Errorf("search lessons: %w", err)
	}
	items := make([]map[string]interface{}, 0, len(lessons))
	for _, l := range lessons {
		items = append(items, map[string]interface{}{
			"id":      l.ID,
			"type":    string(l.Type),
			"content": l.Content,
			"tags":    l.Tags,
		})
	}
	return items, nil
}

// ListLessonsFiltered returns lessons of a given type, or all lessons if empty.
func (dm *DatabaseManager) ListLessonsFiltered(lessonType string) ([]map[string]interface{}, error) {
	lessons, err := dm.ListLessons(lessonType)
	if err != nil {
		return nil, fmt.Errorf("list lessons: %w", err)
	}
	items := make([]map[string]interface{}, 0, len(lessons))
	for _, l := range lessons {
		items = append(items, map[string]interface{}{
			"id":         l.ID,
			"type":       string(l.Type),
			"content":    l.Content,
			"tags":       l.Tags,
			"created_at": l.Created,
		})
	}
	return items, nil
}

// SearchTopicsByQuery wraps MemoryStore.SearchTopics. Wire format matches
// callSearchTopics.
func (dm *DatabaseManager) SearchTopicsByQuery(query string, limit int) ([]map[string]interface{}, error) {
	if limit <= 0 {
		limit = 20
	}
	store, err := dm.getSharedStore()
	if err != nil {
		return nil, fmt.Errorf("get memory store: %w", err)
	}
	results, err := store.SearchTopics(query, limit)
	if err != nil {
		return nil, fmt.Errorf("search topics: %w", err)
	}
	items := make([]map[string]interface{}, 0, len(results))
	for _, r := range results {
		items = append(items, map[string]interface{}{
			"id":          r.ID,
			"name":        r.Title,
			"description": r.Snippet,
			"created_at":  r.Created,
		})
	}
	return items, nil
}

// CreateTopicWithDescription wraps CreateTopic with the (description, "", "")
// signature used by the CLI. Mirrors callCreateTopic.
func (dm *DatabaseManager) CreateTopicWithDescription(name, description string) (string, error) {
	return dm.CreateTopic(name, description, "", "")
}

// ChallengeMemoryWithTheory weakens a memory and creates a pending theory.
// Mirrors callChallengeMemory.
func (dm *DatabaseManager) ChallengeMemoryWithTheory(memoryID, evidence string) (map[string]interface{}, error) {
	mem, err := dm.GetMemory(memoryID)
	if err != nil {
		return nil, fmt.Errorf("memory not found: %w", err)
	}
	if err := dm.ChallengeMemory(memoryID, -2, evidence); err != nil {
		return nil, fmt.Errorf("weaken memory: %w", err)
	}
	theoryContent := fmt.Sprintf("CHALLENGED_MEMORY_ID: %s\nEVIDENCE: %s\nORIGINAL_CONTENT: %s",
		memoryID, evidence, mem["content"])
	theoryMeta := map[string]interface{}{
		"status":               "pending",
		"challenged_memory_id": memoryID,
		"evidence":             evidence,
	}
	store, err := dm.getSharedStore()
	if err != nil {
		return nil, fmt.Errorf("get memory store: %w", err)
	}
	theory, err := store.AddMemory(theoryContent, "theories", []string{"challenge"}, theoryMeta, "", "call")
	if err != nil {
		return nil, fmt.Errorf("create theory: %w", err)
	}
	return map[string]interface{}{
		"success":             true,
		"memory_id":           memoryID,
		"action":              "weakened",
		"theory_id":           theory.ID,
		"theory_status":       "pending",
		"__sse_broadcast": map[string]interface{}{
			"eventType": "immune_slash",
			"payload": map[string]interface{}{
				"memory_id":     memoryID,
				"theory_id":     theory.ID,
				"action":        "weakened",
				"theory_status": "pending",
			},
		},
	}, nil
}

// ProposeTheory logs a hypothesis with validation criteria and auto-links
// to the "theories" topic. Mirrors callProposeTheory.
func (dm *DatabaseManager) ProposeTheory(hypothesis, validationCriteria string, tags []string) (map[string]interface{}, error) {
	if tags == nil {
		tags = []string{}
	}
	content := hypothesis
	if validationCriteria != "" {
		content += "\n\nVALIDATION_CRITERIA: " + validationCriteria
	}
	meta := map[string]interface{}{
		"status":              "pending",
		"validation_criteria": validationCriteria,
	}
	store, err := dm.getSharedStore()
	if err != nil {
		return nil, fmt.Errorf("get memory store: %w", err)
	}
	mem, err := store.AddMemory(content, "theories", tags, meta, "", "call")
	if err != nil {
		return nil, fmt.Errorf("propose theory: %w", err)
	}
	topicID, _ := dm.GetOrCreateTopic("theories")
	dm.AddMemoryToTopic(mem.ID, topicID, "primary")
	return map[string]interface{}{
		"success":    true,
		"id":         mem.ID,
		"status":     "pending",
		"hypothesis": hypothesis,
		"__sse_broadcast": map[string]interface{}{
			"eventType": "theory_proposed",
			"payload": map[string]interface{}{
				"id":         mem.ID,
				"hypothesis": hypothesis,
				"status":     "pending",
			},
		},
	}, nil
}

// ResolveTheory marks a theory as proven or disproven. Mirrors callResolveTheory.
func (dm *DatabaseManager) ResolveTheory(theoryID, conclusion, newStatus string) (map[string]interface{}, error) {
	if newStatus != "proven" && newStatus != "disproven" {
		return nil, fmt.Errorf("newStatus must be 'proven' or 'disproven'")
	}
	mem, err := dm.GetMemory(theoryID)
	if err != nil {
		return nil, fmt.Errorf("theory not found: %w", err)
	}
	if coll, _ := mem["collection"].(string); coll != "theories" {
		return nil, fmt.Errorf("memory %s is not a theory (collection: %s)", theoryID, coll)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	patch := map[string]interface{}{
		"status":      newStatus,
		"conclusion":  conclusion,
		"resolved_at": now,
	}
	patchJSON, _ := json.Marshal(patch)
	if err := dm.UpdateMemoryMetadata(theoryID, string(patchJSON)); err != nil {
		return nil, fmt.Errorf("resolve theory: %w", err)
	}
	dm.ReinforceMemory(theoryID, 1)
	return map[string]interface{}{
		"success":     true,
		"id":          theoryID,
		"status":      newStatus,
		"conclusion":  conclusion,
		"resolved_at": now,
		"__sse_broadcast": map[string]interface{}{
			"eventType": "theory_resolved",
			"payload": map[string]interface{}{
				"id":          theoryID,
				"status":      newStatus,
				"conclusion":  conclusion,
				"resolved_at": now,
			},
		},
	}, nil
}

// RecordDecision logs an architectural decision. Mirrors callRecordDecision.
func (dm *DatabaseManager) RecordDecision(contextText, choice, rationale, outcome string, tags []string) (map[string]interface{}, error) {
	if tags == nil {
		tags = []string{}
	}
	content := "CHOICE: " + choice
	if contextText != "" {
		content += "\nCONTEXT: " + contextText
	}
	if rationale != "" {
		content += "\nRATIONALE: " + rationale
	}
	if outcome != "" {
		content += "\nOUTCOME: " + outcome
	}
	meta := map[string]interface{}{}
	if contextText != "" {
		meta["context"] = contextText
	}
	if rationale != "" {
		meta["rationale"] = rationale
	}
	store, err := dm.getSharedStore()
	if err != nil {
		return nil, fmt.Errorf("get memory store: %w", err)
	}
	mem, err := store.AddMemory(content, "decisions", tags, meta, "", "call")
	if err != nil {
		return nil, fmt.Errorf("record decision: %w", err)
	}
	return map[string]interface{}{
		"success": true,
		"id":      mem.ID,
		"choice":  choice,
	}, nil
}

// AddReferenceFromFile reads a file and ingests it as a reference.
func (dm *DatabaseManager) AddReferenceFromFile(filepath, title string) (map[string]interface{}, error) {
	data, err := os.ReadFile(filepath)
	if err != nil {
		return nil, fmt.Errorf("read file: %w", err)
	}
	content := string(data)
	if title == "" {
		parts := strings.Split(filepath, "/")
		title = parts[len(parts)-1]
	}
	store := NewReferenceStore(DefaultMemoryPaths().MemoryPath)
	ref, err := store.Add(title, filepath, nil, content)
	if err != nil {
		return nil, fmt.Errorf("add reference: %w", err)
	}
	return map[string]interface{}{
		"success": true,
		"id":      ref.ID,
		"title":   ref.Title,
	}, nil
}

// ReadDirectives returns all memories with collection='directives'.
// Mirrors callReadDirectives.
func (dm *DatabaseManager) ReadDirectives() ([]map[string]interface{}, error) {
	rows, err := dm.SQLDB().Query(`
		SELECT id, content, metadata, created_at FROM memories
		WHERE collection = 'directives' AND deleted_at IS NULL
		ORDER BY created_at ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("query directives: %w", err)
	}
	defer rows.Close()
	var directives []map[string]interface{}
	for rows.Next() {
		var id, content, createdAt string
		var metaJSON *string
		if err := rows.Scan(&id, &content, &metaJSON, &createdAt); err != nil {
			continue
		}
		var meta map[string]interface{}
		if metaJSON != nil && *metaJSON != "" {
			json.Unmarshal([]byte(*metaJSON), &meta)
		}
		directives = append(directives, map[string]interface{}{
			"id":         id,
			"content":    content,
			"metadata":   meta,
			"created_at": createdAt,
		})
	}
	return directives, nil
}

// ProactiveRecallHint extracts keywords from a conversation snippet and
// finds matching decisions/theories.
func (dm *DatabaseManager) ProactiveRecallHint(conversationText string, maxHints int, minScore float64) ([]map[string]interface{}, error) {
	if maxHints <= 0 {
		maxHints = 3
	}
	keywords := ExtractConversationKeywords(conversationText, 50)
	overlaps, err := FindEpistemologyOverlaps(dm, keywords, maxHints, minScore)
	if err != nil {
		return nil, fmt.Errorf("find overlaps: %w", err)
	}
	return overlaps, nil
}

// parseDurationString accepts "24h", "30m", "0", or Go duration syntax.
// "0" returns (0, nil) meaning "no expiry". Errors on empty.
func parseDurationString(s string) (time.Duration, error) {
	if s == "" {
		return 0, fmt.Errorf("empty duration")
	}
	if s == "0" {
		return 0, nil
	}
	return time.ParseDuration(s)
}

func parseFloatDefault(v interface{}, def float64) float64 {
	if v == nil {
		return def
	}
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case string:
		if f, err := strconv.ParseFloat(n, 64); err == nil {
			return f
		}
	}
	return def
}

func parseStringDefault(v interface{}, def string) string {
	if v == nil {
		return def
	}
	if s, ok := v.(string); ok {
		return s
	}
	return def
}

func parseStringSliceAny(v interface{}) []string {
	if v == nil {
		return nil
	}
	if arr, ok := v.([]interface{}); ok {
		out := make([]string, 0, len(arr))
		for _, x := range arr {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	if s, ok := v.(string); ok && s != "" {
		return strings.Split(s, ",")
	}
	return nil
}

// ── Exported parse helpers (used by cmd/mpm/call.go and cmd/mpm-mcp/tools.go) ──
//
// The lowercase versions above are the canonical implementations. These
// exported wrappers exist only to bridge to the cmd/* packages, which can't
// see unexported identifiers. Keep behavior identical.

func ParseStringOr(v interface{}, def string) string {
	return parseStringDefault(v, def)
}

func ParseStringSliceOr(v interface{}) []string {
	return parseStringSliceAny(v)
}

func ParseFloatOr(v interface{}, def float64) float64 {
	return parseFloatDefault(v, def)
}
