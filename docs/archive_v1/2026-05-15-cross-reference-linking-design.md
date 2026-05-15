# MPM Cross-reference Linking — Phase 1 Technical Design

**Date:** 2026-05-15
**Author:** 808 (∓808)
**Status:** Draft for review

---

## Decision Log

| Decision | Choice | Rationale |
|----------|--------|-----------|
| Query strategy | **A — On-demand JOINs** | Uses existing `topic_memberships` + `reference_id`; no schema changes; no cache invalidation risk |
| Surfacing policy | **Always surface** | Connections are first-class; agent can follow neural paths without explicit flag |
| Traversal direction | **Bounded Bidirectional** | Memory→(Topics, Ref) is cheap; Topic→Memory is bounded to top-3 + count chip |
| Reference model | **1:1** | `reference_id` on memories = single parent doc |

---

## Overview

Cross-reference linking makes Memories, Topics, and References feel like a brain instead of a filing cabinet. When a memory is retrieved, it surfaces its linked topics and parent reference doc. When a topic is retrieved, it surfaces the top-3 most relevant memories plus a count chip for the rest.

No new tables. No new columns. Uses existing `topic_memberships` and `reference_id` via on-demand JOINs.

---

## 1. Query Strategy

### Memory-Centric: `mpm recall <id>`

Fires 2 queries at recall time:

**Topics JOIN:**
```sql
SELECT t.id, t.name, t.description
FROM topic_memberships tm
JOIN topics t ON t.id = tm.topic_id
WHERE tm.memory_id = ?
ORDER BY tm.role, t.name
LIMIT 10
```

**Reference doc lookup (single row, no JOIN needed):**
```sql
SELECT id, title, file_path
FROM reference_docs
WHERE id = (SELECT reference_id FROM memories WHERE id = ?)
```

The `reference_id` column on `memories` already holds the FK. Single-row lookup is O(1).

### Topic-Centric: `mpm topic recall <id>` (or `mpm recall --topic <id>`)

Bounded to prevent explosion:

**Top-3 memories by relevance:**
```sql
SELECT m.id, m.content, m.collection, m.weight, m.created_at,
       tm.role
FROM topic_memberships tm
JOIN memories m ON m.id = tm.memory_id
WHERE tm.topic_id = ?
ORDER BY m.weight DESC, m.created_at DESC
LIMIT 3
```

**Count chip (single aggregate):**
```sql
SELECT COUNT(*) as total
FROM topic_memberships
WHERE topic_id = ? AND memory_id IS NOT NULL
```

Total: 2 queries, both O(1) with existing indexes on `topic_memberships(topic_id)`.

---

## 2. Terminal UI: The "Related:" Block

The human-readable output of `mpm recall` gets a footer section separated by a horizontal rule. The rule uses box-drawing characters for terminal compatibility.

**When memory has topics AND a reference doc:**
```
─────────────────────────────────────────
ID:        abc123def456  Collection: memories
Weight:    3             Reinforcement: 2
Age:       7d ago
─────────────────────────────────────────
[memory content spanning
multiple lines]

─ Related ────────────────────────────────
Topics:  [architecture] [wp-plugin]
Ref:    Epistemology Engine spec (§Phase 1)
─────────────────────────────────────────
```

**When memory has topics but no reference:**
```
Topics:  [architecture] [wp-plugin]
─────────────────────────────────────────
```

**When memory has neither (no output, no separator):**
```
─────────────────────────────────────────
ID:        abc123def456  Collection: memories
Weight:    3             Reinforcement: 2
Age:       7d ago
─────────────────────────────────────────
[memory content]
─────────────────────────────────────────
```

**When topic recall returns results:**
```
Topic: architecture

[topic description]

─ Top Memories ────────────────────────────
[memory 1 content — truncated to 2 lines]
[memory 2 content — truncated to 2 lines]
[memory 3 content — truncated to 2 lines]
─────────────────────────────────────────
[+ 197 other linked memories]
─────────────────────────────────────────
```

**Truncation rule:** When displaying related memories in topic output, truncate each to a single line (first 120 chars + "…"). Full memory is available via `mpm recall <id>`.

**Sort order for topic's top-3:** `weight DESC, created_at DESC` (highest weight first, recency as tiebreaker). This gives the most "important" memories prominence.

---

## 3. TypeScript Tool Response Format (Agent UI)

Separate from terminal display. The `query_long_term_memory` tool returns a structured JSON for the LLM context window:

```typescript
interface MemoryWithCrossRefs {
  id: string;
  collection: string;
  content: string;
  tags: string[];
  weight: number;
  reinforcement_count: number;
  created_at: string;
  cross_references: {
    topics: Array<{
      id: string;
      name: string;
      role: "manual" | "auto" | "related";
    }>;
    reference_doc: {
      id: string;
      title: string;
      file_path: string;
    } | null;
  };
  // ... other fields
}
```

**For topic recall (`search_topics`):**
```typescript
interface TopicWithTopMemories {
  id: string;
  name: string;
  description: string;
  memory_count: number;
  top_memories: Array<{
    id: string;
    content: string; // truncated to first line
    collection: string;
    weight: number;
  }>;
}
```

The TypeScript tool layer formats the Go CLI `--json` output into these structures client-side before returning to the LLM. No new Go endpoints needed — the existing `--json` recall output is enriched with `cross_references` field.

---

## 4. Go Changes

### 4.1 Recall handler enrichment (`cmd/mpm/simple_cmds.go` or `cmd/mpm/router.go`)

The `recall` command handler calls `GetMemoryByID()` (or equivalent). After fetching the memory record, two additional queries fire:

```go
// Get linked topics
topics, _ := dm.GetMemoryTopics(memoryID)

// Get reference doc (if reference_id is set)
var refDoc *ReferenceDoc = nil
if memory["reference_id"] != nil && memory["reference_id"] != "" {
    refDoc, _ = dm.GetReferenceDoc(memory["reference_id"].(string))
}

// Append cross_references to the memory map before JSON serialization
memory["cross_references"] = map[string]interface{}{
    "topics":        topics,
    "reference_doc": refDoc,
}
```

### 4.2 New methods in `internal/db.go`

**`GetMemoryTopics(memoryID string) ([]TopicRef, error)`**
```go
type TopicRef struct {
    ID   string
    Name string
    Role string
}

func (dm *DatabaseManager) GetMemoryTopics(memoryID string) ([]TopicRef, error) {
    rows, err := dm.db.Query(`
        SELECT t.id, t.name, tm.role
        FROM topic_memberships tm
        JOIN topics t ON t.id = tm.topic_id
        WHERE tm.memory_id = ?
        ORDER BY tm.role, t.name
    `, memoryID)
    if err != nil {
        return nil, err
    }
    defer rows.Close()

    var refs []TopicRef
    for rows.Next() {
        var r TopicRef
        if err := rows.Scan(&r.ID, &r.Name, &r.Role); err == nil {
            refs = append(refs, r)
        }
    }
    return refs, rows.Err()
}
```

**`GetReferenceDoc(docID string) (*ReferenceDoc, error)`**
```go
type ReferenceDoc struct {
    ID       string
    Title    string
    FilePath string
}

func (dm *DatabaseManager) GetReferenceDoc(docID string) (*ReferenceDoc, error) {
    var title, filePath string
    err := dm.db.QueryRow(`SELECT title, file_path FROM reference_docs WHERE id = ?`, docID).Scan(&title, &filePath)
    if err == sql.ErrNoRows {
        return nil, nil
    }
    if err != nil {
        return nil, err
    }
    return &ReferenceDoc{ID: docID, Title: title, FilePath: filePath}, nil
}
```

**`GetTopicTopMemories(topicID string, limit int) ([]MemoryRef, int, error)`**
```go
type MemoryRef struct {
    ID        string
    Content   string
    Collection string
    Weight    int
}

func (dm *DatabaseManager) GetTopicTopMemories(topicID string, limit int) ([]MemoryRef, int, error) {
    if limit <= 0 {
        limit = 3
    }

    // Top N by weight/recency
    rows, err := dm.db.Query(`
        SELECT m.id, m.content, m.collection, m.weight
        FROM topic_memberships tm
        JOIN memories m ON m.id = tm.memory_id
        WHERE tm.topic_id = ? AND tm.memory_id IS NOT NULL
        ORDER BY m.weight DESC, m.created_at DESC
        LIMIT ?
    `, topicID, limit)
    if err != nil {
        return nil, 0, err
    }
    defer rows.Close()

    var refs []MemoryRef
    for rows.Next() {
        var r MemoryRef
        if err := rows.Scan(&r.ID, &r.Content, &r.Collection, &r.Weight); err == nil {
            // Truncate content to first line for list display
            if len(r.Content) > 120 {
                r.Content = r.Content[:120] + "…"
            }
            refs = append(refs, r)
        }
    }
    if refs == nil {
        refs = []MemoryRef{}
    }

    // Total count for chip
    var total int
    dm.db.QueryRow(`SELECT COUNT(*) FROM topic_memberships WHERE topic_id = ? AND memory_id IS NOT NULL`, topicID).Scan(&total)

    return refs, total, rows.Err()
}
```

### 4.3 CLI changes

**`mpm recall <id>`** — gets cross_references appended (topics + ref doc).

**`mpm topic recall <id>`** — gets top-3 + count chip.

**`mpm recall --json` output** — the JSON field `cross_references` is always present (empty arrays if no links).

---

## 5. TypeScript Changes (`openclaw/mpm-plugin/src/index.ts`)

The `query_long_term_memory` tool's `execute()` function formats the Go JSON output into the `MemoryWithCrossRefs` structure.

```typescript
execute: async (toolCallId, params) => {
  const { query = "", limit = 5 } = params as { query: string; limit?: number };
  const result = await runMpm(["recall", "--json", "--", query, String(limit)]);
  const data = parseMpmResult(result);

  let displayText: string;
  if (data.memories && Array.isArray(data.memories) && data.memories.length > 0) {
    displayText = data.memories.map((m) => {
      const xref = (m as any).cross_references;
      const topicChips = xref?.topics?.length
        ? `\nTopics: [${xref.topics.map((t: any) => t.name).join("] [")}]`
        : "";
      const refLine = xref?.reference_doc
        ? `\nRef: ${xref.reference_doc.title}`
        : "";
      const content = typeof m.content === "string" ? m.content : JSON.stringify(m);
      return `${content}${topicChips}${refLine}`;
    }).join("\n\n---\n\n");
  } else if (data.text) {
    displayText = String(data.text);
  } else {
    displayText = "(no matching memories found)";
  }

  return {
    toolCallId,
    result: {
      type: "ok" as const,
      results: [{ content: [{ type: "text" as const, text: displayText }] }],
    },
  };
}
```

The `search_topics` tool is similarly enriched to show top-3 memories inline.

---

## 6. Files to Modify

| File | Change |
|------|--------|
| `internal/db.go` | Add `GetMemoryTopics()`, `GetReferenceDoc()`, `GetTopicTopMemories()` |
| `cmd/mpm/simple_cmds.go` | Enrich recall output with cross_references block; add topic recall with top-3 + chip |
| `openclaw/mpm-plugin/src/index.ts` | Format cross_references in tool result display |
| `docs/MPM_WISHLIST.md` | Mark #8 (cross-reference linking) as done |

---

## 7. Open Questions — Resolved

| Question | Resolution |
|----------|------------|
| Query strategy? | Approach A — on-demand JOINs via existing `topic_memberships` + `reference_id` |
| Always surface or on flag? | Always surface — connections are first-class citizens |
| Topic recall explosion? | Bounded to top-3 + `[+ N other]` chip |
| Reference model? | 1:1 via existing `reference_id` column |

---

## Self-Review

- **Placeholder scan:** No TBD, no TODOs. All sections have concrete specs.
- **Internal consistency:** Memory-centric uses `topic_memberships` JOIN; topic-centric uses same table + count. Both use existing indexes. No contradiction.
- **Scope check:** Focused on Phase 1 (memory↔topic↔ref traversal). Recursive/bidirectional depth deferred.
- **Ambiguity check:** "weight DESC, created_at DESC" is explicit for sort order. Truncation at 120 chars is explicit.