# TASK: Implement Context-Aware Synthesis-Driven Deduplication

We are upgrading `mpm synthesize` from a passive maintenance tagger into an active compression engine. When the system detects multiple related memory fragments, it synthesizes them into a single higher-density LTM record — collapsing redundancy, not just tagging it.

---

## 1. Overview

The synthesis pipeline already exists in `internal/synthesize.go` (`AutoSynthesize`, `DetectNearMiss`, `SynthClient`). This task fixes three correctness gaps and adds a quality gate. The flow:

```
New memory saved → DetectNearMiss (FTS5 bm25) → Quality gate (≥2 candidates)
  → LLM synthesis → New LTM record → Transfer topic links
  → Soft-delete originals (including triggering memory) → Done
```

---

## 2. Critical Fixes (Required)

### Fix A: Delete the triggering memory after synthesis

**Current behavior:** The incoming memory (newID) that triggered `AutoSynthesize` stays in the database after synthesis. Result: the synthesized LTM + the original = duplicate.

**Correct behavior:** After the new LTM is successfully saved, soft-delete the original triggering memory:

```go
dm.SQLDB().Exec("UPDATE memories SET deleted_at = CURRENT_TIMESTAMP WHERE id = ?", newID)
```

This runs after step 5 (SaveMemory succeeds). Without it, synthesis doubles rather than compresses.

### Fix B: Preserve the oldest `created_at`

**Current behavior:** The synthesized record gets a fresh `CURRENT_TIMESTAMP`.

**Correct behavior:** Inherit the `created_at` of the oldest fragment. The synthesized record carries the full lineage.

```go
// Before calling SaveMemory for the new LTM:
rows := dm.SQLDB().QueryRow(`
    SELECT MIN(created_at) FROM memories
    WHERE id = ? OR id IN (?, ?, ...)<!-- candidates -->
`, newID, sourceIDs...)
var oldestCreatedAt string
row.Scan(&oldestCreatedAt)

// In SaveMemory call, pass created_at
```

If `SaveMemory` doesn't support a `createdAt` parameter, use a direct INSERT after synthesis instead of `SaveMemory`:

```go
_, err := dm.SQLDB().Exec(`
    INSERT INTO memories (id, content, collection, tags, metadata, embedding, weight, created_at, updated_at)
    VALUES (?, ?, 'memories', ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
`, newSynthID, result.Content, tagsJSON, metadataJSON, embedding, 10, oldestCreatedAt)
```

### Fix C: Transfer topic_memberships to the new LTM

**Current behavior:** Candidates are soft-deleted and their topic links are orphaned.

**Correct behavior:** Before soft-deleting candidates, transfer their topic links to the new synthesized ID:

```go
_, err := dm.SQLDB().Exec(`
    INSERT OR IGNORE INTO topic_memberships (memory_id, topic_id, role, created_at)
    SELECT ?, topic_id, role, created_at
    FROM topic_memberships
    WHERE memory_id IN (?, ?, ...)
`, newSynthID, newID, sourceIDs...)
```

Use `INSERT OR IGNORE` — safe to run even if links already exist.

### Fix D: Exclude epistemology collections from auto-synthesis

**Current behavior:** `theories` and `decisions` memories are included in FTS5 near-miss detection.

**Correct behavior:** Exclude them. Epistemology memories have their own lifecycle rules (pending → resolved) and should never be auto-synthesized with general memories.

In `DetectNearMiss` (`internal/synthesize.go`), add to the SQL WHERE clause:

```sql
AND m.collection NOT IN ('theories', 'decisions')
```

### Fix E: Quality gate — minimum pair count

**Current behavior:** Synthesis triggers on any FTS5 result with bm25 score < -10.

**Correct behavior:** Only synthesize when there are at least 2 candidates (the triggering memory + at least 1 candidate). A single near-miss is not enough.

In `AutoSynthesize`, after building `toMerge`:

```go
if len(toMerge) < 1 {
    // Need at least 1 candidate (new memory + 1 candidate = 2 total fragments)
    logWatchdogOp(dm, "synthesize_skip", map[string]interface{}{
        "reason": "insufficient candidates",
        "count":  len(toMerge),
    })
    return
}
```

Log as `synthesize_skip` with reason `insufficient candidates` — not an error, just no-op.

---

## 3. LLM Prompt Refinement

Update the system prompt in `SynthClient.Synthesize` (or wherever the prompt is defined):

```
You are a senior archivist. Your goal is to collapse redundant memory fragments
into a single, concise Master Memory. Maintain all unique facts, decisions,
and context from the fragments. Do not lose nuance. Preserve the identity of
each fragment's core insight. Output only the synthesized Master Memory content.
```

The existing `handleSynthesize` command (manual `mpm synthesize`) should also use this prompt.

---

## 4. Files to Modify

- `internal/synthesize.go` — Fixes A (delete triggering memory), B (preserve created_at), C (transfer topic links), D (exclude epistemology collections), E (quality gate)
- All existing tests must pass

---

## 5. Verification

| Test | Expected |
|---|---|
| Two fragments on same topic, synthesize | Both soft-deleted, one new LTM, topic link transferred |
| `SELECT created_at FROM memories WHERE id = <newSynthID>` | Matches oldest fragment's `created_at` |
| `SELECT topic_memberships WHERE memory_id = <newSynthID>` | Links from all candidates present |
| Epistemology memory saved | Does NOT trigger synthesis (excluded from FTS5 query) |
| Single near-miss candidate | `synthesize_skip: insufficient candidates` logged, no synthesis |
| Triggering memory deleted after synthesis | `SELECT deleted_at FROM memories WHERE id = <newID>` returns timestamp |
| `make test` | All tests pass |

---

## 6. Optional: Make it Observable

Add a line to `mpm stats` output or create `mpm ops stats` to show synthesis activity:
- Total syntheses performed
- Total memories compressed (soft-deleted + merged)
- Last synthesis timestamp

This is optional — fix the correctness gaps first.