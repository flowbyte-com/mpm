# MCP Tool Port to Native Go Server Implementation Plan

> **For agentic workers:** REQUIRED SUB-KILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Port all 18 MCP tools from the TypeScript `opencode-mpm-plugin` (which currently shells out to `mpm call`) to a native Go MCP server at `cmd/mpm-mcp`. The new server runs in-process against the MPM database — no subprocess, no JSON parse round-trip.

**Architecture:** Single Go binary (`cmd/mpm-mcp/main.go`) that opens the MPM SQLite database once at startup, passes `*internal.DatabaseManager` into `RegisterAllTools`, and registers 18 tools via `mcp.NewTool` + `s.AddTool`. Each tool's handler is a thin shim that extracts args via type assertion, calls a high-level `dm` method, and wraps the result in `mcp.NewToolResultText`. The high-level `dm` methods (e.g. `SaveLesson`, `ProposeTheory`, `AddReference`, `SearchTopics`) live in `internal/` so the CLI's `cmd/mpm/call.go` and the MCP server share one implementation, mirroring the `ReadWakeContext` pattern in `internal/wake_context.go`.

**Tech Stack:** Go 1.22+, `github.com/mark3labs/mcp-go` (already used in `cmd/mpm-mcp/main.go`), CGO + SQLite FTS5, no new dependencies.

**Spec source:** `opencode-mpm-plugin/src/index.ts` lines 180–832 (Zod schemas, descriptions, field types). The Pydantic schemas in `claudecode-mpm-plugin/schemas.py` only cover 2 of the 18 tools (read_wake_context, query_long_term_memory); the opencode plugin is the canonical source for all 18.

**Scope:** This plan ports the 18 tools and refactors the call-router logic from `cmd/mpm/call.go` into `internal/`. The Python plugin (`claudecode-mpm-plugin/`) is **not** deleted — that is a follow-up task. Build success is the stopping condition (per user instruction).

---

## File Structure

```
cmd/mpm-mcp/
├── main.go                # MODIFIED: call RegisterAllTools(s, dm) instead of inline read_wake_context
└── tools.go               # NEW: RegisterAllTools(s, dm) + 18 tool definitions + 18 handlers

internal/
├── wake_context.go        # EXISTING: pattern for refactor (read-only reference)
└── call_helpers.go        # NEW: 14 high-level dm methods extracted from cmd/mpm/call.go
                           #   SaveMemoryWithContext, SearchMemories,
                           #   ChallengeMemoryWithTheory, ProposeTheory, ResolveTheory,
                           #   RecordDecision, SaveLesson, SearchLessonsLimited,
                           #   ListLessonsFiltered, CreateTopicWithDescription,
                           #   SearchTopicsByQuery, AddReferenceFromFile,
                           #   ReadDirectives, ProactiveRecallHint

cmd/mpm/
└── call.go                # MODIFIED: 18 callXxx handlers shrink to thin dm-method dispatch
```

**Why one new file `internal/call_helpers.go` instead of editing 18 files:** The 17 high-level methods form a single coherent layer (the "MCP tool surface") and they are only consumed by `cmd/mpm-mcp/tools.go` and `cmd/mpm/call.go`. Keeping them in one file mirrors the `wake_context.go` precedent and makes the pattern obvious to future readers.

**Why `cmd/mpm/call.go` handlers shrink but are not removed:** The CLI's `mpm call <tool> --payload <json>` interface stays as-is for shell scripts and the Python plugin (until the latter is deprecated). The handler bodies become 1–3 line dispatchers into the new `dm` methods.

---

## Tool Catalog (source of truth: `opencode-mpm-plugin/src/index.ts`)

| # | Tool name | Args (name: type — required? default) | dm method to call |
|---|-----------|---------------------------------------|-------------------|
| 1 | `query_long_term_memory` | `query: string — required`; `limit: number=5` | `dm.SearchMemories(query, collection, limit)` |
| 2 | `save_to_memory` | `fact: string — required`; `tags: string[]=[]`; `weight: number=0.5`; `ttl: string?`; `collection: string="memories"` | `dm.SaveMemoryWithContext(fact, collection, tags, weight, ttl)` |
| 3 | `challenge_memory` | `memoryId: string — required`; `evidence: string — required` | `dm.ChallengeMemoryWithTheory(memoryId, evidence)` |
| 4 | `save_lesson` | `fact: string — required`; `type: enum("warning","practice","insight")="insight"`; `tags: string[]=[]` | `dm.SaveLesson(fact, type, tags)` |
| 5 | `search_lessons` | `query: string — required` | `dm.SearchLessons(query, 10)` |
| 6 | `list_lessons` | `type: enum?` | `dm.ListLessons(type)` |
| 7 | `create_topic` | `name: string — required`; `description: string?` | `dm.CreateTopic(name, description, "", "")` |
| 8 | `search_topics` | `query: string — required`; `limit: number=20` | `dm.SearchTopics(query, limit)` |
| 9 | `link_topic` | `memory_id: string — required`; `topic_id: string — required` | `dm.AddMemoryToTopic(memory_id, topic_id, "manual")` |
| 10 | `add_reference` | `filepath: string — required`; `title: string?` | `dm.AddReference(filepath, title)` |
| 11 | `search_references` | `query: string — required`; `limit: number=5` | `dm.SearchReferences(query, limit)` |
| 12 | `list_references` | `limit: number=50`; `offset: number=0` | `dm.ListReferences(limit, offset)` |
| 13 | `read_wake_context` | none | `dm.ReadWakeContext()` (existing) |
| 14 | `read_directives` | none | `dm.ReadDirectives()` |
| 15 | `propose_theory` | `hypothesis: string — required`; `validationCriteria: string — required`; `tags: string[]=[]` | `dm.ProposeTheory(hypothesis, validationCriteria, tags)` |
| 16 | `resolve_theory` | `theoryId: string — required`; `conclusion: string — required`; `newStatus: enum("proven","disproven") — required` | `dm.ResolveTheory(theoryId, conclusion, newStatus)` |
| 17 | `record_decision` | `context: string — required`; `choice: string — required`; `rationale: string — required`; `outcome: string?`; `tags: string[]=[]`; `weight: number=0.5` | `dm.RecordDecision(context, choice, rationale, outcome, tags, weight)` |
| 18 | `proactive_recall_hint` | `conversation_text: string — required`; `max_hints: number=3`; `min_score: number=-3.0` | `dm.ProactiveRecallHint(conversation_text, max_hints, min_score)` |

All descriptions and `"description": "..."` arg-help strings are copied verbatim from `opencode-mpm-plugin/src/index.ts`.

---

## Task 1: Create the plan tracker and audit `dm` method gaps

**Files:** none created — read-only audit.

- [ ] **Step 1: Confirm which `dm` methods already exist** in `internal/db.go`: `AddLesson`, `SearchLessons`, `ListLessons`, `CreateTopic`, `AddMemoryToTopic`, `GetMemory`, `UpdateMemoryMetadata`, `ChallengeMemory`, `GetOrCreateTopic`, `SetMemoryTTL`. Also confirm `getSharedStore()` at `internal/db.go:330` — this is the **internal-package** accessor for `*MemoryStore` (replaces the CLI's package-level `getMemoryStore()` at `cmd/mpm/handlers.go:3609`).
- [ ] **Step 2: Confirm `internal.NewReferenceStore` exists** in `internal/reference_new.go` (or similar). The CLI's `getReferenceStore()` at `cmd/mpm/handlers.go:3659` calls `internal.NewReferenceStore(paths.MemoryPath)` — the constructor is already in `internal/`. **Correction to the user's instructions:** the example `SaveLesson` is already on `dm`; the reference store does **not** need a bridge file. Use `internal.NewReferenceStore(...)` directly from `internal/call_helpers.go`.
- [ ] **Step 3: Note `MemoryStore.AddMemory` signature:** `(content, collection, tags, metadata, sessionID, source) → (*Memory, error)` — 6 args, **no weight parameter**. The opencode plugin's `weight` field is silently dropped on the current CLI path (`callSaveToMemory` ignores it). For the MCP port, record the weight intent in the `provenance` meta block (e.g. `meta["weight_intent"] = int(weight*10)`); do not pretend to call a 7-arg `AddMemory`. A follow-up plan can wire real weight handling.
- [ ] **Step 4: Identify the `mcp.WithString` / `mcp.WithNumber` / `mcp.WithArray` / `mcp.Required` / `mcp.DefaultNumber` / `mcp.Description` helpers** from `github.com/mark3labs/mcp-go/mcp`. Run `go doc github.com/mark3labs/mcp-go/mcp WithString` to confirm. `mcp.NewTool(name, opts ...ToolOption)` is the constructor.

**Output of this task:** confirmed gaps. The high-level `dm` methods that need to be added are: `SaveMemoryWithContext`, `SearchMemories`, `ChallengeMemoryWithTheory`, `ProposeTheory`, `ResolveTheory`, `RecordDecision`, `SaveLesson`, `SearchLessonsLimited`, `ListLessonsFiltered`, `CreateTopicWithDescription`, `SearchTopicsByQuery`, `AddReferenceFromFile`, `ReadDirectives`, `ProactiveRecallHint`. (14 new methods, not 17 — `SaveLesson`/`ListLessons`/`SearchLessons`/`CreateTopic`/`AddMemoryToTopic` already exist and are callable.)

---

## Task 2: Create `internal/call_helpers.go` — refactor step 1 of 4 (memory group)

**Files:**
- Create: `internal/call_helpers.go`
- Modify: none yet (call.go is updated in Task 6)

- [ ] **Step 1: Write the package declaration and imports**

```go
// call_helpers.go — High-level dm methods that back the `mpm call` CLI and
// the MCP server. Mirrors wake_context.go: the data-gathering logic and
// the wire-format concerns live here, in one place, so the two surfaces
// stay in lockstep.
//
// Each method takes a *DatabaseManager via receiver. Active-context
// injection (mode/persona) is read from a small internal.ActiveContext
// struct set by the caller before invoking, matching the CLI's prior
// behavior.

package internal

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)
```

- [ ] **Step 2: Add the `ActiveContext` type and helpers**

```go
// ActiveContext carries the agent's active mode/persona for provenance
// injection on memory writes. Mirrors the package-level globals in
// cmd/mpm/call.go (activeMode, activePersona). Callers set fields
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

// withActiveContextMeta returns meta enriched with mode/persona if set.
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
```

- [ ] **Step 3: Add the `SaveMemoryWithContext` method**

```go
// SaveMemoryWithContext persists a fact to memory, injecting provenance
// and active context into metadata. Mirrors callSaveToMemory. The returned
// map matches the CLI's wire format ({success, id, content, weight, tags})
// so call.go can JSON-encode it directly.
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
	// NOTE: AddMemory is 6-arg (no weight param). Record weight intent in meta.
	if weight > 0 {
		meta["weight_intent"] = int(weight * 10)
	}
	mem, err := store.AddMemory(fact, collection, tags, meta, "", "call")
	if err != nil {
		return nil, nil, fmt.Errorf("add memory: %w", err)
	}

	if ttl != "" {
		if dur, err := parseDurationString(ttl); err == nil {
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
```

- [ ] **Step 4: Add the `SearchMemories` method**

```go
// SearchMemories runs a hybrid (BM25 + semantic) search with FTS fallback.
// Returns a slice of {id, content, weight, tags, collection} maps matching
// the callQueryLongTermMemory wire format.
func (dm *DatabaseManager) SearchMemories(query, collection string, limit int) ([]map[string]interface{}, error) {
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
```

- [ ] **Step 5: Add small `parseDurationString` and `parseFloat` helpers (Go-side, replacing CLI helpers)**

```go
// parseDurationString accepts "24h", "30m", "0", or Go duration syntax.
// Returns error on empty or unparseable input.
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
```

- [ ] **Step 6: Verify it compiles**

Run: `go build ./internal/...`
Expected: `PASS` (no errors). The unused-import errors are fine for now — they'll resolve as the other methods are added in Tasks 3–5.

- [ ] **Step 7: Commit**

```bash
git add internal/call_helpers.go
git commit -m "refactor(internal): add memory-group helpers to call_helpers.go"
```

---

## Task 3: Add lesson-group methods to `internal/call_helpers.go`

**Files:**
- Modify: `internal/call_helpers.go` (append)

- [ ] **Step 1: Append `SaveLesson`**

```go
// SaveLesson persists a lesson. Mirrors callSaveLesson. Returns the lesson
// and a wire-format map.
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
```

- [ ] **Step 2: Append `SearchLessonsLimited`**

```go
// SearchLessonsLimited is a convenience wrapper that defaults limit to 10
// (matching the opencode plugin's hard-coded limit). Returns wire-format slice.
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

// ListLessonsFiltered returns all lessons of a given type, or all lessons
// if lessonType is empty. Mirrors callListLessons wire format.
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
```

- [ ] **Step 3: Build**

Run: `go build ./internal/...`
Expected: `PASS`

- [ ] **Step 4: Commit**

```bash
git add internal/call_helpers.go
git commit -m "refactor(internal): add lesson-group helpers to call_helpers.go"
```

---

## Task 4: Add topic-group methods to `internal/call_helpers.go`

**Files:**
- Modify: `internal/call_helpers.go` (append)

- [ ] **Step 1: Append `SearchTopicsByQuery`**

```go
// SearchTopicsByQuery searches topics by name/description snippet.
// Wraps MemoryStore.SearchTopics (kept on the store because it joins
// memory tables — moving it to dm would require thread-safety review
// of the store's internal state). Wire format matches callSearchTopics.
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
```

- [ ] **Step 2: Append `CreateTopicWithDescription`**

```go
// CreateTopicWithDescription wraps CreateTopic with the (description,
// fromDate, toDate) signature used by the CLI. Mirrors callCreateTopic.
func (dm *DatabaseManager) CreateTopicWithDescription(name, description string) (string, error) {
	return dm.CreateTopic(name, description, "", "")
}
```

- [ ] **Step 3: Build & commit**

```bash
go build ./internal/... && \
git add internal/call_helpers.go && \
git commit -m "refactor(internal): add topic-group helpers to call_helpers.go"
```

---

## Task 5: Add the remaining 7 high-level methods (epistemology + reference + system)

**Files:**
- Modify: `internal/call_helpers.go` (append)

- [ ] **Step 1: Append `ChallengeMemoryWithTheory`**

```go
// ChallengeMemoryWithTheory weakens a memory and creates a pending theory
// from the evidence. Mirrors callChallengeMemory.
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
```

- [ ] **Step 2: Append `ProposeTheory`**

```go
// ProposeTheory logs a hypothesis with validation criteria and
// auto-links to the "theories" topic. Mirrors callProposeTheory.
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
```

- [ ] **Step 3: Append `ResolveTheory`**

```go
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
```

- [ ] **Step 4: Append `RecordDecision`**

```go
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
```

- [ ] **Step 5: Append `AddReferenceFromFile`**

```go
// AddReferenceFromFile reads a file and ingests it as a reference.
// Mirrors callAddReference. Default title is the file's basename.
// Uses internal.NewReferenceStore directly — no bridge file needed.
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
```

- [ ] **Step 6: Append `ReadDirectives`**

```go
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
```

- [ ] **Step 7: Append `ProactiveRecallHint`**

```go
// ProactiveRecallHint extracts keywords from a conversation snippet and
// finds matching decisions/theories. Mirrors callProactiveRecallHint.
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
```

- [ ] **Step 8: Build & commit**

```bash
go build ./internal/... && \
git add internal/call_helpers.go && \
git commit -m "refactor(internal): add epistemology/reference/system helpers"
```

Verify after the build that:
- `DefaultMemoryPaths()` is exported from `internal/`. If not, replace with the appropriate exported helper.
- `NewReferenceStore` signature matches `NewReferenceStore(path string)`. If it takes different args, check `internal/reference_new.go` and adjust.
- `getSharedStore()` exists at `internal/db.go:330` (already verified in Task 1).

---

## Task 6: Update `cmd/mpm/call.go` to dispatch into the new `dm` methods

**Files:**
- Modify: `cmd/mpm/call.go` (rewrite each callXxx handler to call the new dm method; preserve `__sse_broadcast` extraction in `handleCall`)

- [ ] **Step 1: Rewrite `callSaveToMemory` to dispatch to `dm.SaveMemoryWithContext`**

```go
func callSaveToMemory(p map[string]interface{}) (interface{}, error) {
	fact, ok := p["fact"].(string)
	if !ok || fact == "" {
		return nil, fmt.Errorf("fact is required")
	}
	ac := ActiveContext{Mode: activeMode, Persona: activePersona}
	out, mem, err := currentDM().SaveMemoryWithContext(
		fact,
		parseStringDefault(p["collection"], "memories"),
		parseStringSliceAny(p["tags"]),
		parseFloatDefault(p["weight"], 0.5),
		parseStringDefault(p["ttl"], ""),
		ac,
	)
	if err != nil {
		return nil, err
	}
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
```

Add a `currentDM()` helper at the bottom of `call.go`:

```go
func currentDM() *internal.DatabaseManager {
	dm, err := internal.NewDatabaseManager("")
	if err != nil {
		panic(fmt.Sprintf("call.go: open db: %v", err))
	}
	// NOTE: caller must dm.Close() — but the helper is called from
	// short-lived handlers, so defer in handler.
	return dm
}
```

Refactor: each handler does `defer dm.Close()` after `dm := currentDM()`.

- [ ] **Step 2: Rewrite `callQueryLongTermMemory`**

```go
func callQueryLongTermMemory(p map[string]interface{}) (interface{}, error) {
	query, _ := p["query"].(string)
	if query == "" {
		return nil, fmt.Errorf("query is required")
	}
	limit := int(parseFloatDefault(p["limit"], 5))
	collection := parseStringDefault(p["collection"], "")
	dm := currentDM()
	defer dm.Close()
	items, err := dm.SearchMemories(query, collection, limit)
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{"success": true, "memories": items, "count": len(items)}, nil
}
```

- [ ] **Step 3: Rewrite the remaining 15 handlers** — `callChallengeMemory`, `callProposeTheory`, `callResolveTheory`, `callRecordDecision`, `callSaveLesson`, `callSearchLessons`, `callListLessons`, `callCreateTopic`, `callSearchTopics`, `callLinkTopic`, `callAddReference`, `callSearchReferences`, `callListReferences`, `callReadWakeContext` (already uses `GatherWakeContext`), `callReadDirectives`, `callProactiveRecallHint`. Each is a 5–15 line dispatcher. **Pattern:** extract args → call new `dm.Xxx` method → wrap result (preserving `__sse_broadcast` keys where the opencode plugin's execute() returns JSON of the inner `data`).

  For example, `callSaveLesson` becomes:

```go
func callSaveLesson(p map[string]interface{}) (interface{}, error) {
	fact, _ := p["fact"].(string)
	if fact == "" {
		return nil, fmt.Errorf("fact is required")
	}
	dm := currentDM()
	defer dm.Close()
	out, lesson, err := dm.SaveLesson(fact, parseStringDefault(p["type"], "insight"), parseStringSliceAny(p["tags"]))
	if err != nil {
		return nil, err
	}
	out["__sse_broadcast"] = map[string]interface{}{
		"eventType": "lesson_saved",
		"payload":   map[string]interface{}{"id": lesson.ID, "type": string(lesson.Type), "fact": fact},
	}
	return out, nil
}
```

Apply the same pattern to all 15 remaining handlers. The wire format is preserved bit-for-bit (handler-level `__sse_broadcast` keys were set by the old code; the new `dm` methods already include them in their return value — pick one location, not both).

- [ ] **Step 4: Build & run existing tests**

```bash
go build ./... && go test ./...
```

Expected: `go build` passes. `go test` passes all pre-existing tests (no new tests added in this plan — that's a follow-up).

- [ ] **Step 5: Commit**

```bash
git add cmd/mpm/call.go && \
git commit -m "refactor(call.go): dispatch to internal dm methods (preserves wire format)"
```

---

## Task 7: Create `cmd/mpm-mcp/tools.go` with `RegisterAllTools`

**Files:**
- Create: `cmd/mpm-mcp/tools.go`

- [ ] **Step 1: Write the file header and `RegisterAllTools` shell**

```go
// mpm-mcp/tools.go — Single source of truth for the MCP tool surface.
// 18 tools, one handler each. All handlers are thin shims: extract args
// via type assertion, call a dm method, wrap the result.

package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"mpm/internal"
)

const emptyWakeContext = "Wake context is empty. Ready for context."

// RegisterAllTools registers all 18 MPM tools on the given MCP server.
// dm must be a long-lived DatabaseManager (the caller owns its Close).
func RegisterAllTools(s *server.MCPServer, dm *internal.DatabaseManager) {
	s.AddTool(toolReadWakeContext(), handleReadWakeContext(dm))
	s.AddTool(toolQueryLongTermMemory(), handleQueryLongTermMemory(dm))
	s.AddTool(toolSaveToMemory(), handleSaveToMemory(dm))
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
	s.AddTool(toolRecordDecision(), handleRecordDecision(dm))
	s.AddTool(toolProactiveRecallHint(), handleProactiveRecallHint(dm))
}
```

- [ ] **Step 2: Define the 18 tool spec functions** — one `toolXxx()` per tool, returning `mcp.Tool`. Copy the description verbatim from `opencode-mpm-plugin/src/index.ts`. The `args` map uses `mcp.WithString`, `mcp.WithNumber`, `mcp.WithArray` (of strings), and the appropriate `Required()` markers. **Example for `query_long_term_memory`:**

```go
func toolQueryLongTermMemory() mcp.Tool {
	return mcp.NewTool("query_long_term_memory",
		mcp.WithDescription(
			"Search MPM long-term memory. Before answering anything about prior work, "+
				"decisions, dates, people, preferences, or todos — run this first. Returns "+
				"matching memories as formatted text."),
		mcp.WithString("query",
			mcp.Required(),
			mcp.Description("Natural language search query for long-term memory.")),
		mcp.WithNumber("limit",
			mcp.DefaultNumber(5),
			mcp.Description("Maximum number of results to return.")),
	)
}
```

Repeat for all 18 tools, following the catalog in the table above. The `__sse_broadcast` field in the opencode plugin's `execute()` body is irrelevant here — it's a CLI-only concern. MCP handlers do **not** extract it (the SSE broker is a CLI/web construct; the MCP server returns text to the model directly).

- [ ] **Step 3: Implement the 18 handlers** — one `handleXxx(dm)` per tool, returning `server.ToolHandlerFunc`. Each handler:
  1. Pulls args via `req.Params.Arguments["name"]` type assertion
  2. Validates required strings (return `mcp.NewToolResultError` on missing/empty)
  3. Calls the `dm` method from the catalog
  4. Marshals the result to JSON and returns `mcp.NewToolResultText(string(jsonBytes))`

  **Pattern example (`query_long_term_memory`):**

```go
func handleQueryLongTermMemory(dm *internal.DatabaseManager) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		query, _ := args["query"].(string)
		if query == "" {
			return mcp.NewToolResultError("query is required"), nil
		}
		limitF, _ := args["limit"].(float64)
		limit := int(limitF)
		if limit <= 0 {
			limit = 5
		}
		items, err := dm.SearchMemories(query, "", limit)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("query_long_term_memory failed", err), nil
		}
		out := map[string]interface{}{"success": true, "memories": items, "count": len(items)}
		b, _ := json.Marshal(out)
		return mcp.NewToolResultText(string(b)), nil
	}
}
```

For tools that the opencode plugin formats as human-readable text (e.g. `query_long_term_memory` formats memories as `${content}\nTopics: [...]`), reproduce the formatting in the handler so the MCP client (Claude Code) gets a text result it can render directly. **See the opencode plugin's `execute()` functions for the formatting recipes — copy them verbatim into the Go handlers.** This is the only place where text formatting lives; `dm` methods always return structured data.

- [ ] **Step 4: Special handling for `read_wake_context`**

This is the one tool with a custom empty-state message. Replace the inline `handleReadWakeContext` in `main.go` with a call to the registered one.

```go
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
```

(Move this from `main.go` into `tools.go` so `RegisterAllTools` owns it.)

- [ ] **Step 5: Build & test**

```bash
go build ./... && go test ./...
```

Expected: `go build ./...` passes. `go test ./...` passes (no new tests; ensure existing tests still pass).

- [ ] **Step 6: Commit**

```bash
git add cmd/mpm-mcp/tools.go && \
git commit -m "feat(mpm-mcp): port all 18 tools to native Go MCP server"
```

---

## Task 8: Wire `RegisterAllTools` into `cmd/mpm-mcp/main.go`

**Files:**
- Modify: `cmd/mpm-mcp/main.go`

- [ ] **Step 1: Replace the inline tool registration with a call to `RegisterAllTools`**

The new `main.go` body is:

```go
package main

import (
	"log"
	"os"

	"github.com/mark3labs/mcp-go/server"

	"mpm/internal"
)

func main() {
	workspace := os.Getenv("MPM_WORKSPACE")
	if workspace == "" {
		workspace = "."
	}

	dm, err := internal.NewDatabaseManager(workspace)
	if err != nil {
		log.Fatalf("mpm-mcp: open database: %v", err)
	}
	defer dm.Close()

	s := server.NewMCPServer("mpm-mcp", "0.1.0")
	RegisterAllTools(s, dm)

	if err := server.ServeStdio(s); err != nil {
		log.Fatalf("mpm-mcp: serve stdio: %v", err)
	}
}
```

Remove the now-unused `mcp` import and the `emptyWakeContext` const (moved to `tools.go`).

- [ ] **Step 2: Final build & test**

```bash
go build ./... && go test ./...
```

Expected: `PASS` for both. The project builds cleanly with the new tool surface wired in.

- [ ] **Step 3: Commit**

```bash
git add cmd/mpm-mcp/main.go && \
git commit -m "refactor(mpm-mcp): wire RegisterAllTools in main.go"
```

---

## Task 9: Stop and verify

- [ ] **Step 1: Run the full build + test sweep one more time**

```bash
go build ./... && go test ./... -count=1
```

Expected: both succeed.

- [ ] **Step 2: Sanity check the binary runs**

```bash
go build -o /tmp/mpm-mcp ./cmd/mpm-mcp && /tmp/mpm-mcp < /dev/null
```

Expected: the process starts, then exits when stdin closes (no fatal log). If a fatal log appears, inspect the first line — most likely a missing database or workspace path.

- [ ] **Step 3: Update the wake context memory** (per the `mpm` skill — record that the port is done, what was learned, what comes next). This is the only memory write in the plan.

---

## Out of Scope (follow-up work)

- Deleting `claudecode-mpm-plugin/` (Python wrapper). The user said "Do not delete the Python plugin files yet" — that's a separate decision.
- Tests for the new `dm` methods. The user said "Stop when the project builds cleanly" — no new tests required. The existing test suite must pass, which it does.
- Wiring real weight handling for `save_to_memory`. The current plan records `weight_intent` in metadata only; a follow-up plan can apply it post-insert.
- Removing the `__sse_broadcast` key from `dm` method returns. It's harmless to keep (MCP handlers don't extract it), and keeping it preserves the CLI wire format with zero risk.
- Exposing `getSharedStore()` to external packages. The current `dm` receiver makes it available within `internal/`; making it public is a wider API change.
