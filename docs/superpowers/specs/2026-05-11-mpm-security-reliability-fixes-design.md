# MPM Security & Reliability Fixes — Design Spec

**Date:** 2026-05-11
**Status:** Approved
**Scope:** Four independent incremental fixes for MPM v1.0

---

## Fix 1: Layered Sensitive Pattern Detection

### Problem
The regex for OpenAI API keys at `internal/memory.go:439`:
```go
{"OpenAI API Key", regexp.MustCompile(`sk-[a-zA-Z0-9_-]{20,}`)}
```
misses modern scoped keys (`sk-proj-`, `sk-svc-`) and logs all `sk-` variants ambiguously.

### Solution
Layered regex array — specific prefixes first, general fallback last. Matched in order; first match wins.

### Patterns (in order)
```go
{"OpenAI Project Key",  regexp.MustCompile(`sk-proj-[a-zA-Z0-9_-]{20,}`)},
{"OpenAI Service Key",  regexp.MustCompile(`sk-svc-[a-zA-Z0-9_-]{20,}`)},
{"Anthropic API Key",   regexp.MustCompile(`sk-ant-[a-zA-Z0-9_-]{20,}`)},
{"Generic Secret Key",  regexp.MustCompile(`sk-[a-zA-Z0-9_-]{20,}`)},
```
Plus existing GitHub/AWS/Slack/Stripe/JWT patterns unchanged.

### Behavior
- `sk-proj-...` → "OpenAI Project Key"
- `sk-svc-...` → "OpenAI Service Key"
- `sk-ant-...` → "Anthropic API Key"
- Any other `sk-{20+}` → "Generic Secret Key"
- Unknown future format (e.g., `sk-ws-...`) → falls through to "Generic Secret Key" catch-all

### Audit Trail
Blocked attempts in `mirror.jsonl` carry the specific pattern name, enabling accurate audit logs.

---

## Fix 2: Deferred VACUUM

### Problem
`handlers.go:438-441` runs `VACUUM` synchronously after hard DELETE in `shred` operations. VACUUM requires an exclusive SQLite lock, which can block the Watch daemon's external DB polling and file ingestion.

### Solution
- Hard DELETE runs synchronously — FTS5 triggers fire immediately, data is unsearchable the moment DELETE commits
- VACUUM deferred to `RunSelfMaintenance()` cycle
- No user-facing blocking, no Watch daemon lock-out

### Changes
- Remove `db.Exec("VACUUM")` from `handleShredSessions()`, `handleShredMemories()`, and all shred handlers
- In `RunSelfMaintenance()` (`memory.go:1333`), add periodic VACUUM call guarded by a check (e.g., only run VACUUM if `deleted_at` records exist and haven't been vacuumed recently, tracked via a lightweight marker)

### Implementation Note
Use `PRAGMA incremental_vacuum` instead of full `VACUUM` for large databases — reclaims space in background chunks without long exclusive lock.

---

## Fix 3: Remove Topic Cache

### Problem
`watch.go:488` has `topicCache map[string]*topicCluster` as an in-memory map in the Watch daemon. The Main daemon has no visibility into it. Running `mpm topic add` via CLI (Main daemon) staleness the Watch daemon's cache, causing missed associations or duplicate inserts.

### Solution
Drop the in-memory `topicCache` entirely.

- `checkTopicClustering()` queries `topic_memberships` table directly on each run
- SQLite on local SSD serves these queries in microseconds — no latency problem being solved

### Changes
- Remove `topicCache map[string]*topicCluster` from `watcherDaemon` struct
- Remove all `d.topicCache` reads/writes in `checkTopicClustering()`
- Replace cache lookups with direct DB query on each clustering run

---

## Fix 4: Smart Fence Telegram Chunker

### Problem
Telegram bridge auto-chunks at 4096 chars. Naive character-based splitting slices markdown code blocks (```), breaking Telegram's renderer and potentially leaking partial context.

### Solution
Stateful line-by-line chunker with fence balancing.

### Algorithm
```
1. Track state: inCodeBlock bool, currentLang string, accumulator []string
2. For each line:
   a. If line starts with ``` (fence):
      - If !inCodeBlock: set inCodeBlock=true, extract language (e.g., "go"), emit accumulator if non-empty, reset accumulator
      - If inCodeBlock: set inCodeBlock=false, add line to accumulator, emit accumulator, reset
   b. Else: add line to accumulator
   c. If accumulator size >= 3900 chars AND not at EOF:
      - If inCodeBlock: append "```" to accumulator (close fence), emit, start new chunk with "```<lang>\n"
      - Else: emit at last safe boundary (newline), continue
3. Emit any remaining accumulator
```

### Properties
- Balanced fences: every chunk sent to Telegram has properly opened/closed fences
- Code blocks of any size: split across sequential chunks with matching fences
- Paragraphs kept intact when possible
- 3900-char safety margin prevents Telegram 400 errors on legitimate 4096-char non-code messages

### Location
`mpm-agent/` Telegram bridge — exact file depends on where `sendMessage`/`chunkMessage` lives.

---

## Files Affected

| File | Changes |
|------|---------|
| `internal/memory.go` | Fix 1: layered regex array |
| `cmd/mpm/handlers.go` | Fix 2: remove VACUUM calls from shred handlers |
| `cmd/mpm/maint_cmds.go` | Fix 2: add VACUUM to `RunSelfMaintenance` |
| `cmd/mpm/watch.go` | Fix 3: remove `topicCache`, simplify `checkTopicClustering` |
| `mpm-agent/` (Telegram bridge) | Fix 4: implement Smart Fence chunker |

## Testing

| Fix | Test |
|-----|------|
| 1 | Add memories containing `sk-proj-...`, `sk-ant-...`, `sk-ws-...` — verify blocked with correct pattern name in `mirror.jsonl` |
| 2 | Run `mpm shred memories -f`, verify response returns instantly; confirm VACUUM happens within next maintenance cycle |
| 3 | Add topic via CLI, verify Watch daemon picks it up on next clustering run |
| 4 | Send LLM output containing 5000-char code block — verify received as 2+ sequential messages with balanced fences |

## Ordering

Fixes 1, 2, and 3 are independent and can be implemented in any order. Fix 4 (Telegram chunker) is independent of the others — it lives in `mpm-agent/` which is a separate package.