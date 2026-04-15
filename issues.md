# MPM Issues & Findings

## 🔴 Bugs

### 1. `mpm recall` returns no results (FTS5 query bug)

**Severity:** Medium
**Affected:** `cmd/mpm/recall.go` → `keywordSearch()`

**Problem:** `recall` searches via FTS5 but always returns "No memories found", while `mpm memory search` (which uses `FullTextSearch` in `memory.go`) works correctly.

**Root cause:** `recall.go`'s `keywordSearch` uses a different, broken FTS5 query pattern:

```go
// recall.go (broken) — column-specific MATCH on subquery
SELECT m.id, m.content, m.session_id, m.tags, m.created_at
FROM memories m
LEFT JOIN memories_fts fts ON m.rowid = fts.rowid
WHERE fts.content MATCH 'query' OR m.content LIKE '%query%' ...

// memory.go QueryMemory (correct) — table-level MATCH with JOIN
SELECT m.id, m.collection, m.content, m.session_id, m.tags, m.metadata, ...
FROM memories m
JOIN memories_fts fts ON m.rowid = fts.rowid
WHERE memories_fts MATCH ? AND m.collection = ?
ORDER BY fts.rank
```

The `fts.content MATCH` is column-specific FTS5 syntax which doesn't work correctly in this JOIN context. The correct approach is `memories_fts MATCH ?` (table-level).

**Fix:** Align `recall.go`'s `keywordSearch` with `memory.go`'s `QueryMemory` FTS5 approach:
- Use `memories_fts MATCH ?` instead of `fts.content MATCH ?`
- Add `ORDER BY fts.rank`
- Remove `LEFT JOIN` → `JOIN`

---

### 2. `mpm memory show <id>` returns "Memory not found" for valid IDs

**Severity:** Medium
**Affected:** `cmd/mpm/handlers.go` → `handleMemoryShow`

**Problem:** After adding a memory via `mpm memory add`, `mpm memory show <id>` immediately returns "Memory not found". However `mpm memory list` shows the same memory.

**Root cause:** `handleMemoryShow` uses `store.GetByID()` which queries by `id` column:

```go
err := s.DB.QueryRow("SELECT ... FROM memories WHERE id = ? AND deleted_at IS NULL", id).Scan(...)
```

The `id` in the database is likely a UUID string. But the query fails silently (ErrNoRows) even when the ID exists.

**Likely cause:** `store.GetByID()` in `memory.go` may be using the wrong column or the id comparison is failing due to type mismatch. Need to verify the actual SQL and confirm `memories.id` is being queried correctly.

**Workaround:** Use `mpm memory list` to find memories (works fine).

---

### 3. `mpm lesson add` hangs indefinitely

**Severity:** Low-Medium
**Affected:** `cmd/mpm/handlers.go` → `handleLessonAdd`

**Problem:** `mpm lesson add` command hangs (SIGKILL after timeout). Likely a daemon worker queue issue or the command is waiting on a lock/socket response that never comes.

---

## 🟡 Findings

### 4. Dual database approach needs cleanup

**Finding:** Old `mpm_memory.db` at `src/db/mpm_memory.db` (0 bytes, created 2026-04-05) coexists with the new consolidated `mpm.db`. The old file is deprecated but not removed.

**Action:** Delete `src/db/mpm_memory.db` and add migration notes.

---

### 5. `mpm recall` and `mpm memory search` have overlapping responsibilities

**Finding:** Both commands search memories but use different code paths:
- `recall` → `keywordSearch()` in `recall.go` → direct SQL with FTS5
- `mpm memory search` → `FullTextSearch()` in `memory.go` → loads 1000 recent memories into Go, does `strings.Contains`

Two different search implementations with different results is confusing for users. Should be consolidated.

**Recommendation:** Deprecate `recall` or fix it to use `QueryMemory` from `memory.go`.

---

### 6. `mpm memory list` shows full content, not truncated properly

**Finding:** `mpm memory list` shows snippets with 500 char truncation, but the actual output shows the full content sometimes exceeds this. The truncation logic exists but may not be applied consistently.

---

## 📋 Mode vs Persona Format Comparison

Both define agent behavior but use different formats:

### Modes — JSON

```json
{
  "sym_id": "research",
  "title": "Research",
  "opcodes": {
    "research!scan": "Broad information gathering",
    "research?cite": "Verifying sources"
  },
  "rules": ["No Hallucinations...", "Bi-Directional..."],
  "behavioral_patterns": [...],
  "anti_patterns": [...],
  "exit_criteria": [...]
}
```

**Pros:**
- Machine-readable, easy to parse in code
- Structured for tooling (compilation, validation, opcodes)
- `opcodes` field enables modular command-like patterns

**Cons:**
- Less human-friendly for authoring
- Nested structure gets verbose

---

### Personas — Markdown + YAML frontmatter

```yaml
---
sym_id: hatter
name: Mad Hatter
vibe: Nonsensical yet oddly profound
emoji: ["🎩", "🍵", "🕰️"]
---

## Identity Override
- **Name:** Mad Hatter
- **Vibe:** Nonsensical yet oddly profound...

## Voice & Tone
Conversational non-sequiturs, answers with riddles...
```

**Pros:**
- Human-readable, natural to write
- Free-form markdown allows narrative richness
- Better for personas (character is story-driven)

**Cons:**
- No `opcodes` equivalent — can't express command patterns
- Markdown parsing required (more complex than JSON)

---

## 🏆 Recommendation: Keep Both Formats

Modes and personas serve **different purposes**:

| | Modes | Personas |
|---|---|---|
| **Purpose** | HOW to approach tasks | WHO the agent IS |
| **Format** | JSON (structured, opcode-based) | Markdown (narrative, identity-driven) |
| **Analogy** | Tools/instructions | Character/backstory |

Modes benefit from JSON because they define **operational patterns** (opcodes, rules, exit criteria) — machine-readable things. Personas benefit from Markdown because they define **identity and voice** — human things.

**Unifying them would hurt both:** Forcing JSON onto personas kills the narrative richness. Forcing Markdown onto modes removes the opcode system.

**If consistency is needed:** Consider a shared frontmatter fields (`sym_id`, `name`, `vibe`), but keep the body formats distinct.

---

## 🟢 Feature: Multi-Source Watch Daemon

**Status:** Spec'd in `docs/WATCH.md`
**Summary:** Generalized watch daemon supports multiple memory_dirs, sessions_dirs, and external_dbs. MPM's workspace is its home directory, but it aggregates from anywhere.

**Config (`mpm_config.json`):**
```json
{
  "memory_dirs": [],         // Empty = use internal MPM workspace
  "sessions_dirs": [],       // Empty = use internal MPM workspace
  "external_dbs": [
    {
      "path": "~/.openclaw/memory/main.sqlite",
      "label": "openclaw",
      "interval_seconds": 30
    }
  ]
}
```

**Design decisions:**
- `memory_dirs` / `sessions_dirs` default to internal MPM workspace dirs when empty
- `external_dbs` is an array — supports multiple source databases
- Paths support `~` expansion and relative paths (resolved vs `MPM_WORKSPACE`)
- External DB memories are **copied** (not moved) — source DB is never modified
- Idempotent: external DB import skips memories with already-ingested IDs

**Outstanding questions:**
1. Should last-seen cursor be persisted to disk so daemon restart doesn't re-ingest?

---

## Priority Order for Fixes

1. 🔴 Fix `recall` FTS5 query (same FTS approach as `QueryMemory`)
2. 🔴 Debug `memory show <id>` failure
3. 🟡 Consolidate `recall` and `memory search` search paths
4. 🟡 Delete deprecated `mpm_memory.db`
5. 🟡 Investigate `lesson add` hang
6. 🟢 Implement multi-source watch daemon (config + external DB polling)
7. 🟢 Make synth model configurable via `mpm_config.json`
8. 🟡 Synth: handle `null` array fields from LLM response gracefully

## 🟢 Feature: Configurable Synth Model

**Status:** Spec'd in `docs/SYNTH.md`
**Summary:** `synth` command should read model/API config from `mpm_config.json` instead of relying on OpenClaw gateway config or hardcoded values.

**Config:**
```json
{
  "synth": {
    "model": "minimax/MiniMax-M2.7",
    "api_key": "",
    "base_url": "",
    "max_tokens": 1024,
    "timeout_seconds": 300
  }
}
```

**Use cases:**
- Default: uses MiniMax via config or env var (existing behavior)
- Local Ollama: `model: "llama3"`, `base_url: "http://localhost:11434/v1"`
- LM Studio: `model: "mistral-7b"`, `base_url: "http://localhost:1234/v1"`
- Any OpenAI-compatible API

**Implementation:**
- `callSynthesisLLM()` in `synthesize.go` needs refactoring to read config instead of env vars / OpenClaw config
- Remove `getOpenClawModelBaseURL()` dependency
- Add config loading for `synth` section


**8. 🟡 Synth null handling:** If LLM returns `null` for `topics` or `memories` instead of `[]`, `json.Unmarshal` into `[]string` fails and synthesis errors out. Should handle `null` / missing fields gracefully by defaulting to empty arrays.

**9. 🟡 Synth success/failure reporting:** Success message should be clearer — e.g. "stored X facts" vs "0 facts — nothing memorable". If LLM call succeeded but all DB writes failed, should report failure not success (currently prints ✅ 0 facts).

**10. 🟢 Memory-as-core-principle:** MPM is the agent's persistent long-term memory. All sessions, decisions, and facts worth keeping should be filed via `mpm memory add`. Watch daemon handles passive capture. Synth compresses sessions. Shred protocol corrects mistakes. Lessons store hard-won wisdom.
