# Mini-Bot Memory Cortex

The memory system operates as a **4-layer memory hierarchy**, progressing from transient to hardened. Each layer serves a distinct purpose in the token-efficiency vs. information-density tradeoff.

```
┌─────────────────────────────────────────────────────────────┐
│  LAYER 1: TRANSIENT                                         │
│  Sliding message history (3 messages)                       │
│  Purpose: Conversational continuity only                   │
│  Token cost: High per-message, no persistence              │
└─────────────────────────────────────────────────────────────┘
                           ↓
┌─────────────────────────────────────────────────────────────┐
│  LAYER 2: STRUCTURED                                        │
│  Anchors - extracted facts with 30-day TTL                  │
│  Purpose: Long-term context without history overhead        │
│  Created: After each response for messages >100 chars      │
└─────────────────────────────────────────────────────────────┘
                           ↓
┌─────────────────────────────────────────────────────────────┐
│  LAYER 3: REFINED                                           │
│  Anchors with high reference_count get TTL refresh          │
│  Purpose: Frequently-used facts stay in context             │
│  Trigger: Value overlap in model response                   │
└─────────────────────────────────────────────────────────────┘
                           ↓
┌─────────────────────────────────────────────────────────────┐
│  LAYER 4: HARDENED                                          │
│  Condensed Anchors - forged from 5+ related anchors        │
│  Purpose: High-density "pillar" memories                    │
│  Trigger: Topic density ≥5, async LLM synthesis            │
└─────────────────────────────────────────────────────────────┘
```

## Layer 1: Transient Memory

**Storage:** SQLite `sessions.metadata` (JSON array)

**Behavior:**
- Sliding window of 3 messages (user + assistant pairs)
- Persisted per chat ID (`telegram:{chat_id}`)
- Cleared on `/new` or `/clear`

**Purpose:** Bridge the immediate conversation gap. Everything else should not depend on this for context.

## Layer 2: Structured Anchors

**Storage:** SQLite `anchors` table

**Schema:**
```sql
id              TEXT PRIMARY KEY
facts           TEXT        -- JSON array: ["namespace:key=value", ...]
summary         TEXT        -- One-line human-readable summary
tags            TEXT        -- JSON array: ["work", "tech", ...]
context         TEXT        -- Original context/metadata
weight          INTEGER     -- Importance (1-10), length-based initially
session_id      TEXT        -- Source session
reference_count INTEGER     -- Times anchor was "used" in response
expires_at      TEXT        -- RFC3339 TTL, default 30 days
created_at      TEXT        -- RFC3339 creation time
is_condensed    INTEGER     -- 0=active, 1=ancestor of condensed
ancestor_ids    TEXT        -- JSON array for condensed anchors
```

**Creation flow** (`selfImprove` goroutine):
1. User message > 100 chars triggers anchor creation
2. `ExtractFactsFromText()` parses key facts:
   - `occupation=<role>` — from "I work as a...", "I'm a..."
   - `has=<text>` — from "I have...", "I own..."
   - `likes=<text>` — from "I like...", "I enjoy..."
   - `dislikes=<text>` — from "I hate...", "I can't..."
   - `problem=<text>` — from "issue", "hurt", "pain"
   - Falls back to `fact=<text>`
3. `ExtractTagsFromText()` assigns topic tags:
   - `work`, `health`, `tech`, `personal`, `hobby`, `finance`, `travel`, `learning`
4. Weight assigned by message length:
   - 100-200 chars → weight 1
   - 200+ chars → weight 2
5. TTL set to 30 days from creation

**Retrieval:** `GetRecentAnchors(db, 3)` — returns 3 most recent non-expired anchors

## Layer 3: Refined Anchors (Use it or Lose it)

**Mechanism:** Reference counting with TTL refresh

**Trigger:** After each response, `IncrementUsedAnchors()` checks if model output contains anchor values using word-boundary regex with plural tolerance:
```go
func IncrementUsedAnchors(db *sql.DB, responseText string, anchors []Anchor) {
	for _, anchor := range anchors {
		for _, fact := range anchor.Facts {
			val := fact
			if idx := strings.Index(fact, "="); idx >= 0 {
				val = fact[idx+1:]
			}
			if val == "" {
				continue
			}
			// Match whole word, case-insensitive, allow optional plural 's'
			pattern := `(?i)\b` + regexp.QuoteMeta(val) + `s?\b`
			if matched, _ := regexp.MatchString(pattern, responseText); matched {
				IncrementAnchorReference(db, anchor.ID)
				break
			}
		}
	}
}
```

**Effect:**
- Frequently referenced anchors get TTL reset to +30 days → they persist
- One-off conversation anchors expire after 30 days without reference
- High `reference_count` anchors sort to top in `GetAnchorsByTags` queries

## Layer 4: Hardened (Condensation)

**Trigger:** Topic density check after anchor insertion

```go
density, _ := GetTopicAnchorDensity(db)  // Returns tag → uncondensed count
for tag, count := range density {
    if count >= 5 {
        go func(t string) { CondenseAnchors(db, t, synthCfg) }(tag)
    }
}
```

**Condensation pipeline** (`CondenseAnchors`):
1. Fetch all uncondensed anchors for tag (ordered by created_at ASC)
2. Build synthesis prompt with conflict resolution rule
3. Call LLM with synthesis prompt
4. Atomic transaction:
   - Insert new condensed anchor (inherits summed ref_counts, fresh TTL, ancestor_ids)
   - Mark all source anchors as `is_condensed = 1`

**LLM Synthesis Prompt:**
```
### ROLE: COGNITIVE ARCHITECT (LONG-TERM MEMORY DISTILLER)
You are the internal maintenance process for your identity. Your task is to perform "Memory Hardening." You will take multiple scattered memory anchors and forge them into a single, high-density "Hardened Pillar."

### TOPIC: {tag}

### SOURCE ANCHORS (Chronological Order):
- ID: ... | Created: ...
  Facts: [...]
  Summary: ...
  Context: ...

### CORE DIRECTIVES:
1. **CONFLICT RESOLUTION (CRITICAL):** If facts from different anchors disagree, the one with the LATEST timestamp is the current truth. Overwrite the old state.
2. **REDUNDANCY REMOVAL:** Do not repeat the same fact. Merge overlapping observations into a single, comprehensive entry.
3. **NOISE FILTERING:** Strip away conversational filler, "just checking in" comments, and transient frustrations. Keep the signal (technical fixes, philosophical stances, personal preferences).
4. **SYNTHESIS:** Don't just list the facts; synthesize them. If 5 anchors discuss CSS bugs, create a distilled technical record of the "Lessons Learned."

### OUTPUT FORMAT (Strict JSON):
{
  "facts": ["namespace:key=value", "namespace:key=value"],
  "summary": "One-sentence high-level truth that encapsulates all sources.",
  "distilled_context": "A 2-3 paragraph 'Golden Record'. It should provide enough technical/philosophical detail that the original raw anchors are no longer needed."
}
```

**Inheritance:**
- `reference_count` = SUM of all ancestor ref_counts (becomes "Elder Anchor")
- `weight` = MAX of ancestor weights
- `ancestor_ids` = JSON array of all source anchor IDs (for provenance tracing)
- TTL reset to 30 days

## Query Paths

| Function | Purpose | Order |
|----------|---------|-------|
| `GetRecentAnchors(db, 3)` | Recency-based context | `created_at DESC` |
| `GetAnchorsByTags(db, query, 3)` | Topic-matched retrieval | `weight DESC, reference_count DESC` |
| `GetTopicAnchorDensity(db)` | Condensation trigger check | `COUNT(*) GROUP BY tags` |

## System Prompt Injection

Anchors are injected into the system prompt via `BuildSystemPromptWithIdentity()`:

```go
if len(anchors) > 0 {
    sb.WriteString("\n\n## Anchored Memories\n")
    for _, a := range anchors {
        sb.WriteString(fmt.Sprintf("- [weight:%d ref:%d] %s\n",
            a.Weight, a.ReferenceCount, FormatAnchorContext(a)))
    }
}
```

`FormatAnchorContext` renders as:
```
Summary of anchor
Facts: fact1 | fact2 | fact3
Tags: tag1, tag2
```

## Configuration (mini-bot-config.json)

```json
{
  "synth": {
    "max_tokens": 1024,        // Reduced from 4096 for Telegram replies
    ...
  },
  "self_improve": {
    "enabled": true,
    "anchor_threshold": 3,      // Weight threshold for anchoring
    ...
  }
}
```

**Key tunables:**
- `maxHistoryMessages` (session.go): 3 — keeps history lean
- `MaxTokens`: 1024 — sufficient for Telegram, reduces cost
- Anchor threshold: 100 chars — only substantial messages get anchored
- Condensation threshold: 5 anchors per tag — enough material for quality synthesis

## Memory Lifecycle Summary

| Stage | What happens | Token overhead |
|-------|--------------|----------------|
| Transient | Message stored in sliding history | High (per-message) |
| Structured | Facts extracted, anchor created with 30d TTL | Low (once) |
| Refined | Reference count increments, TTL refreshes | Minimal |
| Hardened | 5+ anchors forged into condensed pillar | One-time LLM cost |

The system is designed for **pragmatic RAG**: no token overhead for model coordination, no reliance on "honest" citation, works identically across MiniMax, OpenRouter, or local Ollama.