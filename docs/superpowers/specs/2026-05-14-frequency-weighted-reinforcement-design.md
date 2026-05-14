# MPM Frequency-Weighted Reinforcement — Design

**Date:** 2026-05-14
**Status:** Approved
**Wishlist item:** #2 — Frequency-weighted reinforcement

---

## Goal

Memories that are frequently recalled auto-elevate in importance without requiring manual reinforcement. The agent stays informed of what changed via a review digest.

---

## What Already Exists

| Component | Location | Note |
|-----------|----------|------|
| `AccessMemory()` | `internal/memory.go:2641` | Updates `last_accessed_at` — currently never called during recall |
| `ReinforceMemory()` | `internal/memory.go:2599` / `internal/web_db.go:258` | Increments `reinforcement_count` + `weight` — called manually via `mpm reinforce` |
| `SpacedReinforcementReview()` | `internal/memory.go:1195` | Returns LTM/high-weight memories not accessed recently |
| `DecayWeights()` | `internal/memory.go:965` | Decays weight of never-reinforced memories over time |
| `RunSelfMaintenance()` | `internal/memory.go:1373` | Runs decay + prune + consolidation on a schedule |

---

## What's Missing

1. **Recall never calls `AccessMemory`** — `last_accessed_at` is always NULL for recalled memories
2. **No auto-elevation on recall** — `ReinforceMemory` is only called manually
3. **No per-session access tracking** — can't dedupe multiple recalls of the same memory in one session
4. **No review digest** — `SpacedReinforcementReview()` output is not exposed via CLI

---

## Design

### 1. Per-Session Access Deduplication

In `cmd/mpm/recall.go`, add an in-memory `map[string]int` keyed by memory ID, tracking how many times each memory has been accessed in the current CLI invocation (not persistent across sessions — this is per-call, not per-day).

```go
// At the top of handleRecall (or in a closure)
sessionAccessCounts := make(map[string]int)
```

On each memory returned by the search, before adding it to the results:

```go
// Only reinforce on first access in this recall call
if sessionAccessCounts[mem.ID] == 0 {
    dm.ReinforceMemory(mem.ID, 1)
    dm.AccessMemory(mem.ID)
}
sessionAccessCounts[mem.ID]++
```

**Why this approach:**
- 10 recalls of the same memory in one session → 1 elevation (not 10)
- Elevation is tied to the recall event, so "recalled 7× in past 3 days" is interpretable signal
- No persistent "access count" column needed — `reinforcement_count` already captures the signal over time
- Lightweight: `map[string]int` is garbage-collected when `handleRecall` returns

### 2. Review Digest Command

New command: `mpm review [flags]`

**`--promoted` (default)** — Shows memories that received a `reinforcement_count` or `weight` increase since `last_accessed_at` was updated, ordered by most recent elevation:

```text
$ mpm review --promoted

Promoted memories (since last review):

1. a1b2c3d4  recalled 7× in 3 days   weight 1→4   last_recalled: 2 hours ago
   "...session context from your work on the auth refactor..."

2. e5f6g7h8  recalled 3× in 1 day    weight 2→3   last_recalled: yesterday
   "...postgres connection pooling config..."

3. i9j1k2l3  recalled 5× in 7 days   weight 1→3   last_recalled: 3 days ago
   "...mpm watch --bg daemon lifecycle..."
```

Fields per row: `id (short)`, `recall frequency`, `weight delta`, `last_recalled (relative)`, content snippet.

**`--stale`** — Shows LTM/high-weight memories that haven't been accessed in N days (default 14):

```text
$ mpm review --stale --days 30

Stale memories (not accessed in 30+ days):

1. m1n2o3p4  LTM weight=10  last_recalled: 45 days ago
   "...deprecated API endpoint removal procedure..."

2. q5r6s7t8  LTM weight=8   last_recalled: 38 days ago
   "...incident response runbook for DB failover..."
```

**Query logic for `--promoted`:**

```sql
SELECT id, collection, content,
       reinforcement_count,
       weight,
       last_accessed_at,
       created_at
FROM memories
WHERE deleted_at IS NULL
  AND last_accessed_at IS NOT NULL
  AND (reinforcement_count > 0 OR weight > 1)
ORDER BY last_accessed_at DESC
LIMIT 20
```

**Query logic for `--stale`** (using existing `SpacedReinforcementReview`):

```go
memories, err := store.SpacedReinforcementReview(days, 20)
```

---

## Data Flow

```
handleRecall(query)
  ├─→ keywordSearchWithTime()     // FTS5 search
  ├─→ for each memory:
  │     ├─→ sessionAccessCounts[id] == 0 ? ReinforceMemory(id, 1) : skip
  │     └─→ AccessMemory(id)
  └─→ display results

mpm review --promoted
  └─→ Query: memories with last_accessed_at NOT NULL, ordered by last_accessed_at DESC

mpm review --stale
  └─→ SpacedReinforcementReview(days, limit)
```

---

## Error Handling

- If `ReinforceMemory` or `AccessMemory` fails during recall, log to stderr but **do not fail the search** — recall results take priority
- If the database is unavailable for the review command, return a clear error

---

## Testing Considerations

- Unit test `handleRecall` with a mock `DatabaseManager` to verify `ReinforceMemory` is called once per unique memory ID per invocation, not once per recall
- Integration test: insert memories, call recall, verify `reinforcement_count` incremented and `last_accessed_at` updated
- Review command: verify `--promoted` shows memories with recent `last_accessed_at`, `--stale` shows memories past the day threshold

---

## Scope Constraint

This design covers:
- Auto-elevation on recall (per-session deduplicated)
- `mpm review --promoted` digest
- `mpm review --stale` digest

It does **not** cover:
- Scheduled decay/maintenance (already exists in `RunSelfMaintenance`)
- Cross-reference linking (#7)
- Topic auto-suggest on save (#3)
- Any of the deprecation items