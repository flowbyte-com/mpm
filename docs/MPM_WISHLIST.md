# MPM Improvement Wishlist

A running list of things I want to add or change in MPM. Updated as I think of things — not promises, just direction.

---

## High-Value Improvements

### 1. Recall should explain itself ✅ DONE (2026-05-14)
`query_long_term_memory` returns matches but no ranking rationale. The agent sees results without knowing why they matched.
- Add `score` (0-1) and `rationale` (one-line) to each result
- Makes the agent contextualize rather than just dump
**Implemented:**
- `mpm recall` output now shows chip-based rationale: `[id] · Nx ref · weight N · LTM · accessed Nd ago`
- JSON output includes `score` (0-1 fractional, `(rc*2 + weight*1.5) / 55`) and `rationale` string
- `computeScore` and `formatRationale` helper functions added to `cmd/mpm/recall.go`

### 2. Frequency-weighted reinforcement ✅ DONE (2026-05-14)
~~Weight is static — set at save time and never updated.~~
- ~~Memories recalled every session should auto-elevate~~
- ~~Memories never recalled should surface for "did you forget this?" review~~
- ~~Spaced repetition logic, basically~~
**Implemented:**
- `handleRecall` now calls `ReinforceMemory(id, 1)` on first access per invocation (per-call deduplication)
- `mpm review --promoted` shows recently elevated memories with `last_recalled` timestamp
- `mpm review --stale --days N` shows LTM/high-weight memories not accessed in N+ days
- Per-session deduplication: same memory recalled twice in one call = reinforced once

### 3. Topic auto-suggestion on save ✅ DONE (2026-05-14)
When saving a memory, if content semantically matches an existing topic:
- Prompt: "this relates to topic X — link it?"
- Same for lessons: "this looks like a warning, save as lesson?"
- System nudges structure instead of making agent do it manually
**Implemented:**
- `mpm add` now runs FTS5 topic search after save, appends `suggested_topics` to JSON output
- `sanitizeContentForFTS` strips punctuation/markdown, filters stop-words (< 4 chars), OR-joins for safe FTS5 MATCH
- `computeTopicConfidence` scores matches by keyword overlap
- New `mpm topic link <topic-id> <memory-id>` command for linking
- TypeScript `link_topic` tool added to plugin

### 4. Memory age + stale flag ✅ DONE (2026-05-14)
No concept of "you stored this 3 weeks ago and haven't touched it."
- Add `last_recalled` timestamp
- Surface stale memories for review
- "Unused memory" alerts
**Implemented:**
- `mpm recall --stale-days N` flags memories not accessed within N days (default 14, disabled with 0)
- `is_stale` bool in JSON output per result
- ⚠️ STALE chip in human-readable output (yellow, appended when memory exceeds threshold)
- `isMemoryStale` helper: lastAccessed primary, createdAt fallback, exclusive comparison
- Unit tests cover 7 boundary cases

### 5. Session context continuity
Mode/persona files are correctly file-based (human-readable, easy to edit, diff-friendly — no need to move to SQLite).

The real question is **cross-session continuity**: knowing which mode/persona was active when a memory was saved, and surfacing "you were in programming mode last session, working on X."
- Store `active_mode` and `active_persona` in memory metadata on save
- On session start, show: "last session: programming mode, personified as hatter, discussed Y"
- This makes the file-based approach a strength (easy to read mode state) rather than a limitation

### 6. Reference chunking control
Fixed chunk size with no user control.
- Sometimes want smaller chunks for precision, larger for context
- Make chunk size configurable per-ingest

---

## Medium-Value Improvements

### 7. Cross-reference linking
Memories, lessons, topics, references — currently siloed.
- When a memory is retrieved, show linked topics/references
- When a topic is shown, surface related memories

### 8. Memory decay scheduling
TTL is manual. Should be automatic:
- High-weight memories → slow decay
- Low-weight + unaccessed → accelerate decay
- Eventually suggest deletion instead of infinite storage

### 9. Better directives UI
`read_directives` returns raw JSON.
- Format it with collection/category grouping
- Show directive age ("defined 2 weeks ago")
- Allow directive versioning

### 10. Session memory context
On session start, show: "last session you worked on X, had Y open, learned Z."
- Summary of previous session without full recall
- Jump-back context

---

## Low-Value (Nice to Have)

### 11. Bulk import/export
Dump memories to JSON for backup, import from other agents.

### 12. Memory pruning with confirmation
"12 memories haven't been accessed in 90 days — review or delete?"

### 13. Tag autocomplete
When saving with tags, suggest existing tags from similar memories.

### 14. Reference source tracking
Track where ingested documents came from (URL, file path, date) and surface on search results.

---

## Things to Remove / Deprecate

- `synthesize` command → replaced by better recall, rarely used — **pending**
- The `--json` flag pre-scanning hack → standardize arg parsing across all handlers — **pending**

*Last updated: 2026-05-14*