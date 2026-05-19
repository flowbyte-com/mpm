# TASK: Implement the Proactive Review Hook

We are adding a push-based memory layer to MPM. Instead of waiting for the user to run `mpm recall`, 808 can surface relevant memories — specifically decisions and theories — at the moment they become contextually relevant during a conversation.

The hook fires silently in the background, extracts topic keywords from the recent conversation window, and pushes a low-latency "Recall Hint" when a decision or theory has high semantic overlap with what's being discussed.

---

## 1. Core Concept

**Trigger:** FTS5 semantic overlap detected between conversation keywords and epistemology memories.

**Push content:** When the memory is a `decision` or `theory`, surface its STATUS and RATIONALE directly — not just the content blob.

**Goal:** 808 surfaces "you decided to do X here" or "your hypothesis about Y was resolved" at the moment it becomes relevant, rather than storing it passively.

---

## 2. Context Extraction Layer

### 2a. Keyword Extraction

Add a `ExtractConversationKeywords(text string, maxTokens int) []string` function in `internal/memory.go` (or a new `internal/keywords.go`).

**Implementation:**
- Tokenize the last N turns of conversation text using tiktoken (cl100k_base — same encoder used for reference chunking).
- Extract keywords as content-bearing words: filter out stopwords (the, a, an, is, are, was, were, be, been, being, have, has, had, do, does, did, will, would, should, could, may, might, must, can, this, that, these, those, i, you, he, she, it, we, they, and, or, but, if, then, so, as, for, not, with, from, by, on, at, to, in, of, is, was, are, were, been).
- Return the top keywords by frequency, limited to `maxTokens` (e.g., 50 tokens worth).
- Use simple regex-based stopword removal — no need for an external NLP library.

```go
var englishStopwords = map[string]bool{
    "the": true, "a": true, "an": true, "is": true, "are": true,
    "was": true, "were": true, "be": true, "been": true, "being": true,
    "have": true, "has": true, "had": true, "do": true, "does": true,
    "did": true, "will": true, "would": true, "should": true, "could": true,
    "may": true, "might": true, "must": true, "can": true, "this": true,
    "that": true, "these": true, "those": true, "i": true, "you": true,
    "he": true, "she": true, "it": true, "we": true, "they": true,
    "and": true, "or": true, "but": true, "if": true, "then": true,
    "so": true, "as": true, "for": true, "not": true, "with": true,
    "from": true, "by": true, "on": true, "at": true, "to": true,
    "in": true, "of": true,
}

func ExtractConversationKeywords(text string, maxTokens int) []string {
    enc := getTiktokenEncoder() // reuse existing tiktoken singleton
    tokens := enc.Encode(text, nil, nil)
    if len(tokens) > maxTokens {
        tokens = tokens[len(tokens)-maxTokens:]
    }
    // Decode back to words for stopword filtering
    words := strings.Fields(strings.ToLower(enc.Decode(nil, tokens)))
    var keywords []string
    for _, w := range words {
        w = strings.TrimFunc(w, func(r rune) bool {
            return r == '.' || r == ',' || r == '!' || r == '?' || r == '"' || r == '\'' || r == '(' || r == ')' || r == ':' || r == ';'
        })
        if _, ok := englishStopwords[w]; ok || len(w) < 3 {
            continue
        }
        keywords = append(keywords, w)
    }
    return keywords
}
```

### 2b. FTS5 Overlap Query

Add a `FindEpistemologyOverlaps(dm *DatabaseManager, keywords []string, limit int) ([]map[string]interface{}, error)` function.

**Behavior:**
- Build an FTS5 query from the keywords (same sanitization as `DetectNearMiss`).
- Query `memories` where `collection IN ('theories', 'decisions')` AND `deleted_at IS NULL`.
- Order by bm25 score, limit to top `limit` results (default 3).
- Return the full memory records (id, content, collection, tags, metadata, created_at).

```go
query := strings.Join(keywords, " ")
rows, err := dm.SQLDB().Query(`
    SELECT m.id, m.content, m.collection, m.tags, m.metadata, m.created_at, bm25(memories_fts, 0.0, 0.0, 0.0, 0.0) as score
    FROM memories m
    JOIN memories_fts fts ON m.rowid = fts.rowid
    WHERE memories_fts MATCH ?
      AND m.deleted_at IS NULL
      AND m.collection IN ('theories', 'decisions')
    ORDER BY score
    LIMIT ?
`, query, limit)
```

---

## 3. Recall Hint Push

### 3a. MCP Plugin Tool (OpenClaw Integration)

Add a new tool to the OpenClaw MCP plugin (`openclaw/mpm-plugin/src/index.ts`):

```typescript
RecallHintTool: Tool = {
  name: "proactive_recall_hint",
  description: "Checks recent conversation context for overlap with decisions and theories, returns a Recall Hint if semantic match found",
  inputSchema: {
    type: "object",
    properties: {
      conversation_text: { type: "string", description: "Last few turns of conversation" },
      max_hints: { type: "number", description: "Maximum number of hints to return (default 3)" },
      min_score: { type: "number", description: "Minimum bm25 score threshold (default -10)" }
    },
    required: ["conversation_text"]
  },
  handler: async (args) => {
    const keywords = extractConversationKeywords(args.conversation_text, 50)
    const overlaps = findEpistemologyOverlaps(keywords, args.max_hints || 3)
    if (overlaps.length === 0) return null
    // Format as Recall Hint
    return overlaps.map(m => ({
      id: m.id,
      collection: m.collection,
      content: m.content,
      status: m.metadata?.status,
      conclusion: m.metadata?.conclusion,
      rationale: extractField(m.content, "RATIONALE:"),
      relevance_score: m.score,
    }))
  }
}
```

### 3b. 808 Integration

In the OpenClaw agent context (the `read_wake_context` equivalent for proactive mode):

- After each user message, if the conversation context has shifted noticeably (new keywords detected), call `proactive_recall_hint`.
- If hints are returned, surface them as a low-priority notification — something like:

```
💡 [Recall] You decided: <CHOICE first line>
   STATUS: resolved | 2026-05-10
   RATIONALE: <first line>
```

```
💡 [Recall] Hypothesis: <HYPOTHESIS first line>
   STATUS: proven | 2026-05-12
   CONCLUSION: <conclusion text>
```

**Constraint:** Surface only the most relevant hint (rank 1). Do not flood. One hint per conversation turn maximum.

---

## 4. API for 808 Agent

The hook should be accessible via two paths:

### Path A: MCP Tool (for OpenClaw agent)
```
proactive_recall_hint(conversation_text: string, max_hints?: number) → RecallHint[]
```

### Path B: CLI (for human testing)
```
mpm hint "recent conversation text"
mpm hint "discussed golang channels and mutexes" --max 3
```

Add `hint` as a root-level command in `router.go`:

```go
"hint": {Name: "hint", Description: "Check conversation context for relevant decisions/theories", MinArgs: 1},
```

Handler `handleHint(args []string)` calls `ExtractConversationKeywords` then `FindEpistemologyOverlaps`, prints formatted hints.

---

## 5. Quality Rules

- **One hint per turn max** — even if FTS5 returns 3 overlaps, surface only the top 1
- **No false positives** — if `bm25 score >= -3.0`, skip (too weak a match)
- **No duplicates** — if a theory/decision was already surfaced in the last 10 conversation turns, suppress it (track in a rolling window)
- **Low latency** — the hook should be < 100ms end-to-end; FTS5 query is fast, LLM is NOT called in this path

---

## 6. Files to Modify

- `internal/memory.go` or new `internal/keywords.go` — `ExtractConversationKeywords`
- `internal/memory.go` — add `FindEpistemologyOverlaps`
- `cmd/mpm/handlers.go` — add `handleHint`
- `cmd/mpm/router.go` — register `hint` command
- `openclaw/mpm-plugin/src/index.ts` — add `proactive_recall_hint` tool

---

## 7. Verification

| Test | Expected |
|---|---|
| `mpm hint "golang channel mutex"` | Returns theories/decisions with golang-related content |
| `mpm hint "the weather is nice"` | Returns nothing (no overlap) |
| `mpm hint "token budget decision"` | Returns a decision with CHOICE and RATIONALE |
| `proactive_recall_hint` MCP tool | Returns structured RecallHint[] |
| Duplicate hint within 10 turns | Suppressed, not surfaced |
| bm25 score >= -3.0 | Skipped (too weak) |
| All tests pass | ✅ |