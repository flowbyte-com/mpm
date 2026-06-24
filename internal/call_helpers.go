// call_helpers.go — High-level dm methods that back the mpm call CLI and the MCP server.
// Mirrors wake_context.go: data-gathering logic and wire-format concerns live here so the
// two surfaces stay in lockstep. Each method takes a *DatabaseManager via receiver.
// Active-context injection is read from internal.ActiveContext set by the caller.
package internal

import (
	"context"
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
	// Model is the name of the model that produced this memory. Used for
	// provenance attribution. Empty falls back to the "call" default.
	Model string
	// Agent overrides the default "mpm_call" agent name in provenance.
	Agent string
}

func (ac ActiveContext) provenanceMeta() map[string]interface{} {
	model := ac.Model
	if model == "" {
		model = "call"
	}
	agent := ac.Agent
	if agent == "" {
		agent = "mpm_call"
	}
	return map[string]interface{}{
		"provenance": map[string]interface{}{
			"source":  "agent",
			"model":   model,
			"compute": "relative",
			"agent":   agent,
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

	// Use AddMemoryWithWeight so the caller's weight actually reaches the
	// weight column instead of falling back to the DB default (or Go zero).
	mem, err := store.AddMemoryWithWeight(fact, collection, tags, meta, "", "call", weight)
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
	cfg := DefaultHybridConfig()
	cfg.Limit = limit
	mems, err := HybridSearch(dm, query, collection, cfg)
	if err != nil {
		// Fallback: try FullTextSearch alone (no vector, no quarantine)
		store, storeErr := dm.getSharedStore()
		if storeErr != nil {
			return nil, fmt.Errorf("hybrid search: %w; fallback store: %v", err, storeErr)
		}
		fallback, fallbackErr := store.FullTextSearch(query, collection, limit)
		if fallbackErr != nil {
			return nil, fmt.Errorf("hybrid search: %w; fulltext fallback: %v", err, fallbackErr)
		}
		mems = nil // signal fallback mode
		for _, m := range fallback {
			mems = append(mems, HybridResult{
				ID:         m.ID,
				Content:    m.Content,
				Collection: m.Collection,
				Tags:       strings.Join(m.Tags, ","),
				Weight:     m.Weight,
			})
		}
	}
	items := make([]map[string]interface{}, 0, len(mems))
	for _, m := range mems {
		// Extract banner from content if present (banner is prepended in Phase 5)
		var banner string
		if m.IsConceptDrift {
			banner = conceptDriftWarning
		} else if m.IsChallenged {
			banner = challengeWarning
		}
		items = append(items, map[string]interface{}{
			"id":                  m.ID,
			"content":             m.Content,
			"weight":              m.Weight,
			"tags":                m.Tags,
			"collection":          m.Collection,
			"banner":              banner,
			"is_concept_drift":    m.IsConceptDrift,
			"is_challenged":       m.IsChallenged,
			"challenged_theory_id": m.ChallengedTheoryID,
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
// ac injects provenance + active mode/persona into meta so downstream
// consumers can attribute the decision to the agent's runtime context.
func (dm *DatabaseManager) RecordDecision(contextText, choice, rationale, outcome string, tags []string, ac ActiveContext) (map[string]interface{}, error) {
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
	meta := ac.withActiveContextMeta(nil)
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

// AddReferenceFromFile reads a file from disk, parses it by extension,
// chunks the content, and inserts the resulting document + chunks into
// the unified SQLite store. This is the same pipeline the CLI uses
// (mpm kb reference add) — routing both surfaces through one code path
// fixes the prior bug where the MCP add path wrote to the legacy JSON
// store while search read from the chunked SQLite store, leaving MCP
// additions invisible to MCP searches.
//
// Supports: .pdf, .epub, .html/.xhtml, .txt, .md. Unknown extensions are
// treated as plain text. Parse failures short-circuit with a wrapped error.
//
// tags and reason are optional. chunkSize is clamped to the same 64–2048
// range ChunkByTokens enforces internally; 0 or negative falls back to
// the 512 default.
func (dm *DatabaseManager) AddReferenceFromFile(filepath, title string) (map[string]interface{}, error) {
	return dm.AddReferenceFromFileWith(filepath, title, nil, "", 0)
}

// AddReferenceFromFileWith is the extended entry point. Use this when
// callers want to attach tags, an import_reason, or a non-default chunk
// size. The two-argument AddReferenceFromFile delegates here with zero
// values so existing callers (MCP) stay byte-compatible.
func (dm *DatabaseManager) AddReferenceFromFileWith(filepath, title string, tags []string, reason string, chunkSize int) (map[string]interface{}, error) {
	content, err := parseReferenceFile(filepath)
	if err != nil {
		return nil, err
	}
	if title == "" {
		title = filepathBase(filepath)
	}

	chunks, err := ChunkByTokens(content, chunkSize)
	if err != nil {
		return nil, fmt.Errorf("chunk reference: %w", err)
	}

	// Look up an existing doc by source path so re-ingest reuses the
	// doc id. Without this every re-ingest would create a fresh doc
	// (different id) and the chunk_hash diff would never fire — each
	// ingest would land in a new doc row, the old one orphaned. The
	// content_hash comparison below also lets us short-circuit when
	// the file is byte-identical to the last ingest.
	contentHash := HashContent(content)
	existing, err := FindReferenceBySourcePath(dm.db, filepath)
	if err != nil {
		return nil, fmt.Errorf("find existing reference: %w", err)
	}
	if existing != nil && existing.ContentHash == contentHash {
		// Same source, same bytes — nothing to do. Caller already has
		// the doc id; return success with the existing stats.
		return map[string]interface{}{
			"success":      true,
			"id":           existing.ID,
			"title":        existing.Title,
			"total_chunks": existing.TotalChunks,
			"unchanged":    true,
		}, nil
	}

	now := time.Now().UTC().Format(time.RFC3339)
	docID := GenerateID()
	if existing != nil {
		// Source path seen before but content changed: reuse the id so
		// AddReference's chunk diff can replace chunks in place.
		docID = existing.ID
	}
	doc := &ReferenceDoc{
		ID:           docID,
		Title:        title,
		SourcePath:   filepath,
		SourceType:   DetectSourceType(filepath),
		Tags:         tags,
		ImportReason: reason,
		Content:      content,
		ContentHash:  contentHash,
		TotalChunks:  len(chunks),
		LastIndexed:  now,
		Created:      now,
	}

	refChunks := make([]ReferenceChunk, len(chunks))
	for i, c := range chunks {
		refChunks[i] = ReferenceChunk{
			ID:         ComputeChunkID(docID, c.Index, contentHash), // stable across re-ingest of same content
			DocID:      docID,
			ChunkIndex: c.Index,
			Section:    c.Section,
			Content:    c.Content,
			SourcePath: filepath,
		}
	}

	if err := dm.AddReference(doc, refChunks); err != nil {
		return nil, fmt.Errorf("add reference: %w", err)
	}

	// Embed chunks in a separate phase after the chunk-insert tx has
	// committed. Embedding failures are best-effort: a transient
	// provider outage leaves embedding NULL on the affected rows, and
	// a follow-up embed pass (or re-ingest) fills them in. We do NOT
	// fail the ingest call on embedding errors — the chunk rows are
	// already durable, and the operator can retry embedding later.
	embedded, _, _ := dm.EmbedReferenceChunks(context.Background(), docID)

	return map[string]interface{}{
		"success":      true,
		"id":           doc.ID,
		"title":        doc.Title,
		"total_chunks": doc.TotalChunks,
		"embedded":     embedded,
	}, nil
}

// parseReferenceFile reads a file and returns its extracted text content.
// Dispatches by extension: PDF/EPUB use the dedicated parsers, HTML is
// stripped to text, everything else is read as bytes. Errors from the
// underlying parsers are wrapped so callers see a clear failure path.
//
// Mirrors the dispatch logic in cmd/mpm/simple_cmds.go handleRefAdd;
// both surfaces must agree on what counts as supported content. Keep
// them in sync — if a new extension is added here, add it there too.
func parseReferenceFile(filepath string) (string, error) {
	ext := strings.ToLower(filepathExt(filepath))
	switch ext {
	case ".pdf":
		text, err := ParsePDF(filepath)
		if err != nil {
			return "", fmt.Errorf("parse pdf %q: %w", filepath, err)
		}
		return text, nil
	case ".epub":
		text, err := ParseEPUB(filepath)
		if err != nil {
			return "", fmt.Errorf("parse epub %q: %w", filepath, err)
		}
		return text, nil
	case ".html", ".xhtml":
		data, err := os.ReadFile(filepath)
		if err != nil {
			return "", fmt.Errorf("read %q: %w", filepath, err)
		}
		return StripHTML(string(data)), nil
	case ".txt", ".md":
		data, err := os.ReadFile(filepath)
		if err != nil {
			return "", fmt.Errorf("read %q: %w", filepath, err)
		}
		return string(data), nil
	default:
		// Best-effort: read as bytes. Reject obvious binary noise by
		// surfacing the read error; otherwise accept the raw text.
		data, err := os.ReadFile(filepath)
		if err != nil {
			return "", fmt.Errorf("unsupported file type %q: %w", ext, err)
		}
		return string(data), nil
	}
}

// filepathExt is filepath.Ext without the filepath import — keeps this
// file's imports tight. Trivial wrapper; not worth a separate package.
func filepathExt(p string) string {
	for i := len(p) - 1; i >= 0 && p[i] != '/'; i-- {
		if p[i] == '.' {
			return p[i:]
		}
	}
	return ""
}

// filepathBase is filepath.Base without the filepath import.
func filepathBase(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return p[i+1:]
		}
	}
	return p
}

// ReadDirectives returns all memories that are prime directives.
// A memory is a directive if either it was ingested with
// collection='directives' (the MCP path) or has is_prime_directive=1
// (the legacy column-based path). Both identifiers reach the same set
// once either is set — see the directives section of README.md.
func (dm *DatabaseManager) ReadDirectives() ([]map[string]interface{}, error) {
	rows, err := dm.SQLDB().Query(`
		SELECT id, content, metadata, created_at FROM memories
		WHERE (collection = 'directives' OR is_prime_directive = 1)
		  AND deleted_at IS NULL
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

// AddEvidence inserts an evidence row and returns the resulting confidence
// for the artifact. Validation boundary: required-arg preflight +
// IsValidEvidenceType + strength default. internal.AddEvidence runs the
// deeper checks (sensitive-content scan, type registry, recompute).
//
// Both `mpm call add_evidence` and the `add_evidence` MCP tool route
// through this method, so neither surface can bypass validation.
//
// Returns the same map the previous callAddEvidence returned:
//   {"success": true, "confidence": <float>}
//
// Required fields: artifact_id, type, source_group, created_by.
func (dm *DatabaseManager) AddEvidence(in EvidenceInput) (map[string]interface{}, error) {
	if in.ArtifactID == "" {
		return nil, fmt.Errorf("artifact_id is required")
	}
	if in.Type == "" {
		return nil, fmt.Errorf("type is required")
	}
	if !IsValidEvidenceType(in.Type) {
		return nil, fmt.Errorf("invalid evidence type: %q", in.Type)
	}
	if in.SourceGroup == "" {
		return nil, fmt.Errorf("source_group is required")
	}
	if in.CreatedBy == "" {
		return nil, fmt.Errorf("created_by is required")
	}
	if in.ArtifactType == "" {
		in.ArtifactType = "memory"
	}
	// Fill strength from the registry default if the caller passed 0.
	if in.Strength == 0 {
		if def, ok := DefaultStrength(in.Type); ok {
			in.Strength = def
		}
	}
	// Default independence to 1.0 to match the call handler behavior.
	if in.IndependenceFactor == 0 {
		in.IndependenceFactor = 1.0
	}

	if err := AddEvidence(dm, in); err != nil {
		return nil, err
	}

	var conf float64
	if err := dm.QueryRowTracked(
		fmt.Sprintf(`SELECT confidence FROM %s WHERE id = ?`, ArtifactTable(in.ArtifactType)),
		in.ArtifactID,
	).Scan(&conf); err != nil {
		return nil, fmt.Errorf("read confidence: %w", err)
	}
	return map[string]interface{}{
		"success":    true,
		"confidence": conf,
	}, nil
}

// ListEvidence returns all evidence rows for an artifact, newest first.
// Returns the rows under the "evidence" key — same shape as the
// previous callListEvidence.
//
// Required: artifact_id. Optional: artifact_type (default "memory").
func (dm *DatabaseManager) ListEvidence(artifactID, artifactType string) (map[string]interface{}, error) {
	if artifactID == "" {
		return nil, fmt.Errorf("artifact_id is required")
	}
	if artifactType == "" {
		artifactType = "memory"
	}
	rows, err := dm.QueryTracked(`
		SELECT id, artifact_id, artifact_type, type, source_group, strength,
		       independence_factor, created_by, created_at, expires_at, notes
		FROM evidence
		WHERE artifact_id = ? AND artifact_type = ?
		ORDER BY created_at DESC
	`, artifactID, artifactType)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]interface{}{}
	for rows.Next() {
		var id, aid, atype, t, src, by, notes string
		var strength, ind float64
		var createdAt int64
		var expiresAt *int64
		if err := rows.Scan(&id, &aid, &atype, &t, &src, &strength, &ind, &by, &createdAt, &expiresAt, &notes); err != nil {
			return nil, err
		}
		row := map[string]interface{}{
			"id": id, "artifact_id": aid, "artifact_type": atype,
			"type": t, "source_group": src, "strength": strength,
			"independence_factor": ind, "created_by": by,
			"created_at": createdAt, "notes": notes,
		}
		if expiresAt != nil {
			row["expires_at"] = *expiresAt
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return map[string]interface{}{"evidence": out}, nil
}

// QueryConfidenceHistory returns the confidence timeline for an artifact,
// newest first. limit <= 0 defaults to 50. Thin shim — both
// `mpm call query_confidence_history` and the `query_confidence_history`
// MCP tool route through this method.
//
// Returns the rows under the "history" key — same shape as the
// previous callQueryConfidenceHistory.
//
// Required: artifact_id. Optional: artifact_type (default "memory").
func (dm *DatabaseManager) QueryConfidenceHistory(artifactID, artifactType string, limit int) (map[string]interface{}, error) {
	if artifactID == "" {
		return nil, fmt.Errorf("artifact_id is required")
	}
	if artifactType == "" {
		artifactType = "memory"
	}
	if limit <= 0 {
		limit = 50
	}
	rows, err := dm.QueryTracked(`
		SELECT computed_at, confidence, evidence_count, trigger
		FROM confidence_history
		WHERE artifact_id = ? AND artifact_type = ?
		ORDER BY computed_at DESC
		LIMIT ?
	`, artifactID, artifactType, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]interface{}{}
	for rows.Next() {
		var computedAt int64
		var conf float64
		var evidenceCount int
		var trigger string
		if err := rows.Scan(&computedAt, &conf, &evidenceCount, &trigger); err != nil {
			return nil, err
		}
		out = append(out, map[string]interface{}{
			"computed_at": computedAt, "confidence": conf,
			"evidence_count": evidenceCount, "trigger": trigger,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return map[string]interface{}{"history": out}, nil
}

// QueryConfidenceChanges returns recent confidence-altering events with
// delta and trigger. Wraps internal.QueryConfidenceChanges. Thin shim — both
// `mpm call query_confidence_changes` and the `query_confidence_changes`
// MCP tool route through this method.
//
// Returns: {"changes": [...], "count": N} — same shape as the previous
// callQueryConfidenceChanges.
func (dm *DatabaseManager) QueryConfidenceChanges(filter ConfidenceChangesFilter) (map[string]interface{}, error) {
	changes, err := QueryConfidenceChanges(dm, filter)
	if err != nil {
		return nil, fmt.Errorf("query confidence changes: %w", err)
	}
	return map[string]interface{}{
		"changes": changes,
		"count":   len(changes),
	}, nil
}

// QueryConfidenceTrend returns the trajectory projection of confidence
// over a time window. windowDays <= 0 defaults to 30.
//
// Both `mpm call query_confidence_trend` and the `query_confidence_trend`
// MCP tool route through this method, so neither surface can bypass the
// required-arg check or drift in window-day defaulting.
//
// Returns: {"success": true, "trend": <ConfidenceTrend>}.
//
// Required: artifact_id. Optional: artifact_type (default "memory"),
// window_days (default 30).
func (dm *DatabaseManager) QueryConfidenceTrend(artifactID, artifactType string, windowDays int) (map[string]interface{}, error) {
	if artifactID == "" {
		return nil, fmt.Errorf("artifact_id is required")
	}
	if artifactType == "" {
		artifactType = "memory"
	}
	if windowDays <= 0 {
		windowDays = 30
	}
	trend, err := QueryConfidenceTrend(dm, artifactID, artifactType, windowDays)
	if err != nil {
		return nil, fmt.Errorf("query confidence trend: %w", err)
	}
	return map[string]interface{}{
		"success": true,
		"trend":   trend,
	}, nil
}

// QueryMemoryQuality returns per-creator memory statistics. Surfaces
// which models/agents produce memories that survive.
//
// Both `mpm call query_memory_quality` and the `query_memory_quality`
// MCP tool route through this method, so neither surface can drift in
// the wire format or bypass the underlying QueryMemoryQualityBySource
// pipeline that joins memories → auto_capture evidence → confidence_history.
//
// Returns: {"success": true, "sources": [...], "count": N}.
func (dm *DatabaseManager) QueryMemoryQuality() (map[string]interface{}, error) {
	stats, err := QueryMemoryQualityBySource(dm)
	if err != nil {
		return nil, fmt.Errorf("query memory quality: %w", err)
	}
	return map[string]interface{}{
		"success": true,
		"sources": stats,
		"count":   len(stats),
	}, nil
}

// ShowConfidence returns the current confidence and history for an artifact.
//
// The result shape is {"current": <float>, "history": {"history": [...]}} —
// the nested "history" map is the result of QueryConfidenceHistory, which
// is itself wrapped in a {"history": rows} map. This double-nesting is
// load-bearing: existing CLI callers and tests parse `result.history.history`.
// Do not flatten it.
//
// Both `mpm call show_confidence` and the `show_confidence` MCP tool route
// through this method, so neither surface can drift in the required-arg
// check, the artifact-type default, or the nested shape contract.
//
// Required: artifact_id. Optional: artifact_type (default "memory").
func (dm *DatabaseManager) ShowConfidence(artifactID, artifactType string) (map[string]interface{}, error) {
	if artifactID == "" {
		return nil, fmt.Errorf("artifact_id is required")
	}
	if artifactType == "" {
		artifactType = "memory"
	}
	var conf float64
	if err := dm.QueryRowTracked(
		fmt.Sprintf(`SELECT confidence FROM %s WHERE id = ?`, ArtifactTable(artifactType)),
		artifactID,
	).Scan(&conf); err != nil {
		return nil, fmt.Errorf("read confidence: %w", err)
	}
	hist, err := dm.QueryConfidenceHistory(artifactID, artifactType, 50)
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{
		"current": conf,
		"history": hist,
	}, nil
}

// RecomputeConfidence forces a manual confidence recompute for an
// artifact and returns the new snapshot. Returns the same shape as
// dm.ShowConfidence so callers can read `result.current` and
// `result.history.history` consistently.
//
// Both `mpm call recompute_confidence` and the `recompute_confidence`
// MCP tool route through this method, so neither surface can drift
// in the required-arg check, the artifact-type default, or the
// recompute-then-snapshot ordering.
//
// Required: artifact_id. Optional: artifact_type (default "memory").
func (dm *DatabaseManager) RecomputeConfidence(artifactID, artifactType string) (map[string]interface{}, error) {
	if artifactID == "" {
		return nil, fmt.Errorf("artifact_id is required")
	}
	if artifactType == "" {
		artifactType = "memory"
	}
	if err := RecomputeConfidence(dm, artifactID, artifactType, RecomputeReasonManual); err != nil {
		return nil, err
	}
	return dm.ShowConfidence(artifactID, artifactType)
}

// ExplainConfidence returns the reasoning trace for an artifact's
// confidence: the full component breakdown of f(evidence, decay).
// Distinct from query_confidence_history (audit trail) — this answers
// "why did I get this number?"
//
// Both `mpm call explain_confidence` and the `explain_confidence`
// MCP tool route through this method, so neither surface can drift
// in the required-arg check, the artifact-type default, or the
// wrap shape.
//
// Returns: {"success": true, "explanation": <ConfidenceExplanation>}
// (the explanation is exposed as a generic map matching its JSON shape
// so callers can read `result.explanation.artifact_id` directly).
// Required: artifact_id. Optional: artifact_type (default "memory").
func (dm *DatabaseManager) ExplainConfidence(artifactID, artifactType string) (map[string]interface{}, error) {
	if artifactID == "" {
		return nil, fmt.Errorf("artifact_id is required")
	}
	if artifactType == "" {
		artifactType = "memory"
	}
	exp, err := ExplainConfidence(dm, artifactID, artifactType)
	if err != nil {
		return nil, fmt.Errorf("explain confidence: %w", err)
	}
	// Expose the explanation as a generic map (its JSON shape) so both
	// surfaces can index into it by field name without needing to import
	// the internal type. JSON round-trip is the cheapest, drift-proof way
	// to keep the wire shape authoritative.
	expJSON, err := json.Marshal(exp)
	if err != nil {
		return nil, fmt.Errorf("encode explanation: %w", err)
	}
	var expMap map[string]interface{}
	if err := json.Unmarshal(expJSON, &expMap); err != nil {
		return nil, fmt.Errorf("decode explanation: %w", err)
	}
	return map[string]interface{}{
		"success":     true,
		"explanation": expMap,
	}, nil
}
