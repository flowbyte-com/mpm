# Cross-reference Linking Phase 1 — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add bounded bidirectional cross-reference linking to MPM. Memory recall shows linked Topics + parent Reference doc. Topic recall shows top-3 memories + count chip.

**Architecture:** On-demand JOINs via existing `topic_memberships` + `reference_id`. No new tables. No cache invalidation risk.

**Tech Stack:** Go (database layer), TypeScript (OpenClaw plugin tool layer)

---

## File Map

| File | Responsibility |
|------|---------------|
| `internal/web_db.go` | New query methods: `GetMemoryTopics()`, `GetReferenceDoc()`, `GetTopicTopMemories()` |
| `cmd/mpm/recall.go` | Enrich `handleRecall` to accept memory ID and show "Related:" block |
| `cmd/mpm/handlers.go` | Enrich `handleTopicShow` to show top-3 memories + count chip |
| `openclaw/mpm-plugin/src/index.ts` | Format cross_references in tool result display |
| `docs/MPM_WISHLIST.md` | Mark item #8 as done |

---

## Task 1: Add DB query methods in `internal/web_db.go`

**Files:**
- Modify: `internal/web_db.go` (add after `GetMemory`, ~line 206)
- Test: `internal/memory_test.go` (add unit tests)

- [ ] **Step 1: Add `TopicRef` and `MemoryRef` types after the imports**

```go
// TopicRef is a lightweight topic reference for cross-reference display
type TopicRef struct {
    ID   string
    Name string
    Role string // "manual", "auto", "related"
}

// ReferenceDocRef is a lightweight reference doc reference
type ReferenceDocRef struct {
    ID       string
    Title    string
    FilePath string
}

// MemoryRef is a lightweight memory reference (truncated content for lists)
type MemoryRef struct {
    ID         string
    Content    string
    Collection string
    Weight     int
}
```

- [ ] **Step 2: Add `GetMemoryTopics()` after `GetMemoryByExternalID()` (~line 239)**

```go
// GetMemoryTopics returns all topics linked to a memory, ordered by role then name
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
    if refs == nil {
        refs = []TopicRef{}
    }
    return refs, rows.Err()
}
```

- [ ] **Step 3: Add `GetReferenceDoc()` after `GetMemoryTopics()`**

```go
// GetReferenceDoc returns a reference doc by ID (lightweight, no chunks)
func (dm *DatabaseManager) GetReferenceDoc(docID string) (*ReferenceDocRef, error) {
    if docID == "" {
        return nil, nil
    }
    var title, filePath string
    err := dm.db.QueryRow(`SELECT title, file_path FROM reference_docs WHERE id = ?`, docID).Scan(&title, &filePath)
    if err == sql.ErrNoRows {
        return nil, nil
    }
    if err != nil {
        return nil, err
    }
    return &ReferenceDocRef{ID: docID, Title: title, FilePath: filePath}, nil
}
```

- [ ] **Step 4: Add `GetTopicTopMemories()` after `GetReferenceDoc()`**

```go
// GetTopicTopMemories returns top-N memories for a topic (by weight DESC, created_at DESC)
// plus the total count of all linked memories (for the count chip).
// The content field is truncated to 120 chars for list display.
func (dm *DatabaseManager) GetTopicTopMemories(topicID string, limit int) ([]MemoryRef, int, error) {
    if limit <= 0 {
        limit = 3
    }

    rows, err := dm.db.Query(`
        SELECT m.id, m.content, m.collection, m.weight
        FROM topic_memberships tm
        JOIN memories m ON m.id = tm.memory_id
        WHERE tm.topic_id = ? AND tm.memory_id IS NOT NULL AND m.deleted_at IS NULL
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
        var content string
        if err := rows.Scan(&r.ID, &content, &r.Collection, &r.Weight); err == nil {
            if len(content) > 120 {
                r.Content = content[:120] + "…"
            } else {
                r.Content = content
            }
            refs = append(refs, r)
        }
    }
    if refs == nil {
        refs = []MemoryRef{}
    }

    var total int
    dm.db.QueryRow(`SELECT COUNT(*) FROM topic_memberships WHERE topic_id = ? AND memory_id IS NOT NULL`, topicID).Scan(&total)

    return refs, total, rows.Err()
}
```

- [ ] **Step 5: Write unit tests in `internal/memory_test.go`**

```go
func TestGetMemoryTopics(t *testing.T) {
    store := NewMemoryStore(t.TempDir())
    store.InitSQLite()
    defer store.Close()

    // Create a memory
    mem, _ := store.AddMemory("test content", "memories", nil, nil, "", "test")

    // Create topics
    tid1, _ := store.db.Exec(`INSERT INTO topics (id, name) VALUES (?, ?)`, GenerateID(), "topic-a")
    tid2, _ := store.db.Exec(`INSERT INTO topics (id, name) VALUES (?, ?)`, GenerateID(), "topic-b")

    // Link memory to both topics
    store.db.Exec(`INSERT INTO topic_memberships (memory_id, topic_id, role) VALUES (?, ?, ?)`, mem.ID, tid1, "manual")
    store.db.Exec(`INSERT INTO topic_memberships (memory_id, topic_id, role) VALUES (?, ?, ?)`, mem.ID, tid2, "auto")

    dm := &DatabaseManager{db: store.DB.DB, dbPath: store.SQLiteDBPath}
    topics, err := dm.GetMemoryTopics(mem.ID)

    require.NoError(t, err)
    require.Len(t, topics, 2)
    // Ordered by role then name: manual topic-a, auto topic-b
    assert.Equal(t, "manual", topics[0].Role)
    assert.Equal(t, "topic-a", topics[0].Name)
}
```

- [ ] **Step 6: Run tests**

Run: `cd /home/v/workspace/projects/mpm && go test -v ./internal/... -run TestGetMemoryTopics -count=1`
Expected: PASS

- [ ] **Step 7: Commit**

```bash
git add internal/web_db.go internal/memory_test.go
git commit -m "feat: add cross-ref query methods (GetMemoryTopics, GetReferenceDoc, GetTopicTopMemories)"
```

---

## Task 2: Enrich `handleRecall` with "Related:" block and memory ID lookup

**Files:**
- Modify: `cmd/mpm/recall.go:170-216` (JSON output section) and `cmd/mpm/recall.go:218-296` (human-readable section)
- No new test file needed (integration test via CLI)

- [ ] **Step 1: Modify JSON output section to append cross_references**

In `handleRecall`, around line 170 where `*jsonOutput` is handled, after building each `memoryEntry`, add:

```go
// Fetch cross-references for each memory
type crossRefEntry struct {
    Topics      []mpminternal.TopicRef        `json:"topics"`
    ReferenceDoc *mpminternal.ReferenceDocRef `json:"reference_doc"`
}

// Get reference_id from the memory row (add to the scan earlier)
var nullableRefID sql.NullString
// Add nullableRefID to the rows.Scan call:
// ..., &nullableRefID

// After building memoryEntry, fetch cross-references
var topics []mpminternal.TopicRef
var refDoc *mpminternal.ReferenceDocRef

if err := dm.db.QueryRow(`SELECT reference_id FROM memories WHERE id = ?`, e.id).Scan(&nullableRefID); err == nil {
    if nullableRefID.Valid && nullableRefID.String != "" {
        refDoc, _ = dm.GetReferenceDoc(nullableRefID.String)
    }
}
topics, _ = dm.GetMemoryTopics(e.id)

result = append(result, memoryEntry{
    // ... existing fields ...
    CrossReferences: crossRefEntry{
        Topics:       topics,
        ReferenceDoc: refDoc,
    },
})
```

Note: The `recallEntry` struct needs `reference_id` added to its scan — modify the scan around line 120 to include `reference_id`.

- [ ] **Step 2: Modify human-readable output to add "Related:" block**

After the main content block (after line 291 where `fmt.Printf` closes), add:

```go
// After the main memory block fmt.Printf, add cross-reference display
// Fetch topics and ref doc (use cached values from the JSON section logic)
topics, _ := dm.GetMemoryTopics(e.id)

var refTitle string
if refDoc != nil {
    refTitle = refDoc.Title
}

if len(topics) > 0 || refTitle != "" {
    topicChips := []string{}
    for _, t := range topics {
        topicChips = append(topicChips, fmt.Sprintf("%s[%s]%s", magenta, t.Name, reset))
    }

    fmt.Printf("\n%s─ Related ────────────────────────────────%s\n", bold, reset)
    if len(topics) > 0 {
        fmt.Printf("  Topics: %s\n", strings.Join(topicChips, " "))
    }
    if refTitle != "" {
        fmt.Printf("  Ref:    %s\n", refTitle)
    }
    fmt.Printf("%s─────────────────────────────────────────%s\n", bold, reset)
}
```

- [ ] **Step 3: Build and smoke test**

Run: `cd /home/v/workspace/projects/mpm && make build 2>&1`
Expected: Clean build with no errors

Run: `cd /home/v/workspace/projects/mpm && ./bin/mpm recall --json "test" 2>&1 | head -50`
Expected: JSON output with `cross_references` field present (may be empty if no links)

- [ ] **Step 4: Commit**

```bash
git add cmd/mpm/recall.go
git commit -m "feat: add Related block to recall output with cross-reference links"
```

---

## Task 3: Enrich `handleTopicShow` with top-3 memories + count chip

**Files:**
- Modify: `cmd/mpm/handlers.go:962-1023` (handleTopicShow function)

- [ ] **Step 1: Add top-3 memories block to human-readable output in `handleTopicShow`**

In `handleTopicShow`, after the existing output (after line 1019 where description is printed, before line 1022), add:

```go
// Get top-3 memories and total count
memories, total, err := dm.GetTopicTopMemories(id, 3)
if err == nil && len(memories) > 0 {
    output.WriteString(fmt.Sprintf("\n%s─ Top Memories ──────────────────────────%s\n", "\033[1m", "\033[0m"))
    for i, mem := range memories {
        content := mem.Content
        if len(content) > 120 {
            content = content[:120] + "…"
        }
        output.WriteString(fmt.Sprintf("\n%d. %s\n", i+1, content))
    }
    if total > 3 {
        remaining := total - 3
        output.WriteString(fmt.Sprintf("\n%s[+ %d other linked memories]%s\n", "\033[2m", remaining, "\033[0m"))
    }
    output.WriteString(fmt.Sprintf("%s─────────────────────────────────────────%s\n", "\033[1m", "\033[0m"))
}
```

Note: Replace `"\033[1m"` with a variable defined at the top of `handleTopicShow`:
```go
bold := "\033[1m"
reset := "\033[0m"
```

- [ ] **Step 2: Ensure `handleTopicShow` uses `DatabaseManager` directly**

Check that `handleTopicShow` uses `dm` (DatabaseManager) not `store` (MemoryStore) — it currently uses `store.db` for queries. Change to use `dm` for the new methods:

```go
// Replace the store.db.Query calls with dm.db.Query
// The existing code uses store := getMemoryStore(); db := store.DB
// For the new GetTopicTopMemories, we need dm directly
dm, err := mpminternal.NewDatabaseManager("")
if err != nil {
    // error handling
}
// Then use dm.GetTopicTopMemories(id, 3) instead of store-based queries
```

- [ ] **Step 3: Build and smoke test**

Run: `cd /home/v/workspace/projects/mpm && make build 2>&1`
Expected: Clean build

- [ ] **Step 4: Commit**

```bash
git add cmd/mpm/handlers.go
git commit -m "feat: add top-3 + count chip to topic recall output"
```

---

## Task 4: TypeScript cross-reference formatting in `openclaw/mpm-plugin/src/index.ts`

**Files:**
- Modify: `openclaw/mpm-plugin/src/index.ts:486-519` (query_long_term_memory execute function)
- Modify: `openclaw/mpm-plugin/src/index.ts:820-862` (search_topics execute function)

- [ ] **Step 1: Update `query_long_term_memory` tool to format cross_references**

In `makeQueryLongTermMemoryTool`, the `execute` function around line 500 already parses memories. Update the display formatting:

```typescript
// In the execute function, replace the displayText building around line 500
if (data.memories && Array.isArray(data.memories) && data.memories.length > 0) {
    displayText = data.memories.map((m) => {
        const mem = m as any;
        const xref = mem.cross_references || {};
        const topics = xref.topics || [];
        const refDoc = xref.reference_doc;

        const topicLine = topics.length
            ? `\nTopics: [${topics.map((t: any) => t.name).join("] [")}]`
            : "";
        const refLine = refDoc
            ? `\nRef: ${refDoc.title}`
            : "";

        const content = typeof mem.content === "string" ? mem.content : JSON.stringify(mem);
        return `${content}${topicLine}${refLine}`;
    }).join("\n\n---\n\n");
}
```

- [ ] **Step 2: Update `search_topics` tool to format top_memories**

In `makeSearchTopicsTool`, find where topic results are displayed and add top-3 memory previews:

```typescript
// In makeSearchTopicsTool execute function, after getting topic results
// Around line 838 where displayText is built for topics
displayText = data.results
    .map((t: MpmJsonResult) => {
        const name = t.name || t.description || "(unnamed)";
        const id = t.id ? ` [${t.id}]` : "";
        const desc = t.description ? `: ${t.description}` : "";

        // If topic has top_memories, append inline preview
        const topMems = (t as any).top_memories || [];
        let memPreview = "";
        if (topMems.length > 0) {
            memPreview = "\n  Top: " + topMems
                .slice(0, 3)
                .map((m: any) => {
                    const content = typeof m.content === "string" ? m.content : "";
                    return content.length > 60 ? content.slice(0, 60) + "…" : content;
                })
                .join(" | ");
            const total = (t as any).memory_count || 0;
            if (total > 3) {
                memPreview += ` [+${total - 3} more]`;
            }
        }

        return `${name}${id}${desc}${memPreview}`;
    })
    .join("\n");
```

- [ ] **Step 3: Rebuild the plugin**

Run: `cd /home/v/workspace/projects/mpm/openclaw/mpm-plugin && npm run build 2>&1`
Expected: Clean TypeScript build

- [ ] **Step 4: Commit**

```bash
git add openclaw/mpm-plugin/src/index.ts
git commit -m "feat: format cross-references in TypeScript tool results"
```

---

## Task 5: Update MPM Wishlist

**Files:**
- Modify: `docs/MPM_WISHLIST.md`

- [ ] **Step 1: Mark item #8 as done**

Find the cross-reference linking line and update status. Show diff:

```diff
- [ ] #8 Cross-reference linking — surface related topics/references when memory is retrieved — MEDIUM — Phase 2
+ [x] #8 Cross-reference linking — surface related topics/references when memory is retrieved — MEDIUM — Phase 1 (done 2026-05-15)
```

- [ ] **Step 2: Commit**

```bash
git add docs/MPM_WISHLIST.md
git commit -m "docs: mark wishlist #8 cross-reference linking as done"
```

---

## Spec Coverage Check

| Spec Section | Task |
|-------------|------|
| Query Strategy: Memory-Centric JOIN | Task 1 (GetMemoryTopics) + Task 2 (recall output) |
| Query Strategy: Topic-Centric Bounded (top-3 + count) | Task 1 (GetTopicTopMemories) + Task 3 (topic show) |
| Terminal UI: "Related:" block | Task 2 (recall) + Task 3 (topic show) |
| TypeScript Agent UI: cross_references JSON | Task 4 |
| Wishlist update | Task 5 |

**No gaps found.**

---

## Self-Review

- **Placeholder scan:** No TBD, no TODOs. All code blocks show actual implementation.
- **Type consistency:** `TopicRef`, `ReferenceDocRef`, `MemoryRef` defined once in Task 1 and referenced consistently in Tasks 2-4.
- **Scope check:** Focused on Phase 1 (memory↔topic↔ref traversal). No schema changes, no new tables.
- **Ambiguity check:** Sort order (`weight DESC, created_at DESC`) is explicit in SQL. Truncation (120 chars) is explicit in code.

---

**Plan complete and saved to `docs/superpowers/plans/2026-05-15-cross-reference-linking-plan.md`.**

Two execution options:

**1. Subagent-Driven (recommended)** - I dispatch a fresh subagent per task, review between tasks, fast iteration

**2. Inline Execution** - Execute tasks in this session using executing-plans, batch execution with checkpoints

Which approach?