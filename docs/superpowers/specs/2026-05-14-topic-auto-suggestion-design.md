# Topic Auto-Suggestion on Save — Design

> **For agentic workers:** After writing this design, use superpowers:writing-plans to create the implementation plan. Do NOT implement until the design is approved.

**Goal:** When `mpm add` saves a memory, silently run a topic relevance check and append high-confidence topic matches to the JSON response. The LLM sees the suggestion and can decide to call `link_topic` — no blocking, no stdin.

---

## Context

The Go CLI is invoked headlessly by a TypeScript plugin via `child_process.spawn`. The process must:
1. Save the memory
2. Run topic relevance check (non-blocking)
3. Exit with JSON that includes `suggested_topics`
4. Never block on stdin

The TypeScript plugin reads the JSON and renders a "💡 System Hint" for the LLM.

---

## Architecture

### Phase 1: Go Backend — Topic Suggestion

**Files:**
- `cmd/mpm/handlers.go` — modify `handleMemoryAdd`
- `cmd/mpm/topic.go` — add `sanitizeContentForFTS(content string) string`

**Flow in `handleMemoryAdd`:**
```
1. store.AddMemory(...) → get new memory ID
2. Build sanitized query from memory content
3. Run FTS5 topic search
4. Score matches, filter by confidence >= 0.3
5. Append suggested_topics to response
```

**`sanitizeContentForFTS(content string) string`:**
- Strip punctuation and markdown
- Split on whitespace
- Filter out stop-words (common: the, is, at, to, a, in, on, for, of, and, or, but, with, as, by, from)
- Filter out short words (< 4 chars)
- Join remaining keywords with ` OR `
- Return empty string if < 2 keywords remain

**FTS5 topic search:**
- Query `topics` table using `topics_fts MATCH '<sanitized_keywords>'`
- Also search `description` column
- Return top 3 matches with confidence ≥ 0.3

**Confidence score:**
```
score = matched_keywords / total_keywords_in_topic_name
```
If topic has "Go Architecture" (2 keywords) and 2 match → confidence 1.0. If 1 matches → 0.5.

**JSON response (--json mode):**
```json
{
  "success": true,
  "id": "abc123",
  "content": "saved memory content...",
  "suggested_topics": [
    {"name": "Go Architecture", "id": "topic-xyz", "confidence": 0.71}
  ]
}
```

**Human-readable output:**
```
Memory added: abc123
💡 Consider linking to: Go Architecture (0.71), Concurrency Patterns (0.45)
```

**Error handling:** If topic search fails, log to stderr and continue — the save itself is not failed. Only fail if save failed.

---

### Phase 2: TypeScript Plugin — `link_topic` Tool

**File:** `openclaw/mpm-plugin/src/index.ts`

**New tool:**
```typescript
const LINK_TOPIC_SCHEMA = {
  type: "object",
  properties: {
    memory_id: {
      type: "string",
      description: "ID of the memory to link to a topic.",
    },
    topic_name: {
      type: "string",
      description: "Name of the topic to link the memory to.",
    },
  },
  required: ["memory_id", "topic_name"],
  additionalProperties: false,
} as const;
```

**`makeLinkTopicTool`:**
- Command: `["topic", "add", memory_id, topic_name, "--json"]`
- Returns JSON result of the link operation

**Registered as:** `link_topic` tool

---

## Component Details

### `sanitizeContentForFTS`

```go
func sanitizeContentForFTS(content string) string {
    // Strip markdown
    content = stripMarkdown(content)

    // Split on whitespace
    words := strings.Fields(content)

    // Stop-words (common English)
    stopWords := map[string]bool{
        "the": true, "is": true, "at": true, "to": true, "a": true,
        "in": true, "on": true, "for": true, "of": true, "and": true,
        "or": true, "but": true, "with": true, "as": true, "by": true,
        "from": true, "it": true, "this": true, "that": true, "be": true,
        "have": true, "has": true, "had": true, "were": true, "was": true,
        "are": true, "been": true, "being": true, "have": true, "has": true,
    }

    var keywords []string
    for _, w := range words {
        w = strings.ToLower(w)
        w = strings.Trim(w, ".,!?;:\"'()[]{}")
        if len(w) >= 4 && !stopWords[w] {
            keywords = append(keywords, w)
        }
    }

    if len(keywords) < 2 {
        return ""
    }

    return strings.Join(keywords, " OR ")
}
```

### Confidence scoring

```go
func computeTopicConfidence(memoryKeywords []string, topicName string) float64 {
    topicWords := strings.Fields(topicName)
    matched := 0
    topicLower := make(map[string]bool)
    for _, w := range topicWords {
        topicLower[strings.ToLower(w)] = true
    }
    for _, kw := range memoryKeywords {
        if topicLower[kw] {
            matched++
        }
    }
    if len(topicWords) == 0 {
        return 0
    }
    return float64(matched) / float64(len(topicWords))
}
```

---

## Response Format

### JSON mode (`mpm add --json`)
```json
{
  "success": true,
  "id": "abc12345",
  "content": "Use connection pooling for external APIs",
  "collection": "memories",
  "suggested_topics": [
    {"name": "API Design", "id": "topic-xyz", "confidence": 0.8}
  ]
}
```

### Human mode
```
✅ Memory added: abc12345

💡 Consider linking to: API Design (0.80)
```

---

## Files to Change

| File | Change |
|------|--------|
| `cmd/mpm/handlers.go` | Modify `handleMemoryAdd` to call topic suggestion after save |
| `cmd/mpm/topic.go` | Add `sanitizeContentForFTS` and `suggestTopicsForMemory` functions |
| `openclaw/mpm-plugin/src/index.ts` | Add `LINK_TOPIC_SCHEMA`, `makeLinkTopicTool`, register it |

---

## No new database tables

The `topics` table and `topic_memberships` table already exist. No schema changes.

---

## Scope Notes

- **Lessons auto-suggestion** is separate (the wishlist mentions it alongside topic suggestion). Implement topic first, lessons second — same pattern.
- **Threshold 0.3** is chosen to suggest topics with at least 1 keyword overlap in short names. Can be tuned later.
- **Max 3 suggestions** — prevents overwhelming the LLM with too many options.

---

## Testability

- `sanitizeContentForFTS` is a pure function — unit testable without DB
- `suggestTopicsForMemory` requires DB but can use temp DB
- `handleMemoryAdd` with `--json` can be tested end-to-end by capturing stdout