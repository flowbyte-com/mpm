# TASK: Implement the Agent Context Bundle (Wishlist #3 & #8)

We are upgrading the agent's cognitive pipeline with context-aware deduplication and session-specific token budgeting.

---

## 1. Context-Aware Deduplication (Wishlist #3)

We are replacing dumb hash-based dedup with LLM-driven synthesis. When the watcher detects overlapping memories, it should invoke the MiniMax-M2.7 backend to merge them intelligently.

### 1a. FTS5 Pre-Filter — Detecting Near-Miss Candidates

SQLite FTS5's `bm25()` rank provides a fast, zero-cost relevance score without vector embeddings. Use it as a cheap pre-filter before any API call is made.

**Algorithm:**

```
On new memory ingestion (after AddMemory succeeds):
  1. Extract core content words via FTS5 tokenization of the new memory's content
  2. Run: SELECT id, content, bm25(memories_fts) as score FROM memories_fts WHERE memories_fts MATCH '<core words>' ORDER BY score LIMIT 10
  3. For each result where score < -10 (configurable threshold — SQLite bm25 returns negative values where lower is better, so more negative = stronger match):
       → Compute content_hash of candidate and compare to new memory's hash
       → If different hash but score is high: flag as near-miss candidate
  4. If 1+ near-miss candidates:
       → Queue for synthesis (skip if same pair already queued this session)
  5. If no candidates: write memory normally, no synthesis call
```

**Key constraints:**
- Only query top 10 results by bm25 — no full table scan
- `content_hash` comparison already prevents exact duplicates; bm25 handles semantic near-misses only
- Track `content_hash` of candidates to skip self-match
- Deduplicate at session level: don't re-trigger synthesis for the same pair within one ingestion session

### 1b. LLM Synthesis Prompt

When a near-miss is detected, invoke the synth endpoint with the following. **System prompt is self-contained — no user wrapper text.**

```
You are a precise memory consolidation tool. Your task is to merge two or more overlapping memory fragments into a single, coherent memory — without adding, inferring, or hallucinating any facts not present in the input.

INPUT:
You will receive N memory fragments separated by "---MEMORY---". Each fragment may share overlapping content with the others. Your job is to produce a SINGLE consolidated memory.

RULES:
1. PRESERVE all factual claims from every fragment. Do not drop information.
2. REMOVE only pure redundancy — if two fragments say the same thing in different words, rephrase once in the output. Do not decide one version is "better" and discard it.
3. NEVER add new facts, conclusions, or interpretations not present in the input.
4. PRESERVE all tags from the input. Merge duplicates. Do not invent new tags.
5. OUTPUT a JSON object with two fields:
   - "content": the synthesized memory text (max ~500 words)
   - "tags": the merged and deduplicated tag array
6. The output must be valid JSON. No markdown, no explanation, no preamble.

EXAMPLE:
Input fragment 1: "v prefers short messages. Prefers direct communication."
Input fragment 2: "User v — short and direct. Doesn't like fluff."
Output: {"content": "v prefers short, direct communication. No fluff.", "tags": ["preference", "v", "communication"]}
```

**API call:**
- `model`: from `mpm_config.json.synth.model`
- `messages`: `[{"role": "system", "content": "<prompt above>"}, {"role": "user", "content": "---MEMORY---\n<mem1>\n---MEMORY---\n<mem2>..."}]`
- `max_tokens`: from `mpm_config.json.synth.max_tokens` (1024)
- `timeout_seconds`: from `mpm_config.json.synth.timeout_seconds` (300)
- Call the `base_url` from config

### 1c. Synthesis Resolution

On LLM success:
- Write the merged memory to `memories` table as a new entry with `collection: "memories"` (or current collection), `is_long_term: 1` by default after synthesis
- Soft-delete the old fragments: `UPDATE memories SET deleted_at = CURRENT_TIMESTAMP WHERE id IN (<old_ids>)`
- Log to `watchdog.jsonl`: `{op: "synthesize", new_id: "...", old_ids: [...], timestamp: "..."}`

On LLM failure:
- Write the original new memory without merging
- Log error to `watchdog.jsonl`: `{op: "synthesize_failed", content: "<truncated>", error: "<reason>", timestamp: "..."}`
- Do not block ingestion — write the original and move on

---

## 2. Session-Aware Token Budgeting (Wishlist #8)

We need to constrain heavy recalls in Telegram (1,000-token limit) without throttling the agent globally.

### 2a. Go Handler: `mpm recall --token-budget`

Refactor `cmd/mpm/handlers.go` `handleRecall()` to accept a `--token-budget <n>` flag:

```
mpm recall <query> [--limit N] [--token-budget N]
```

**Implementation:**
- After fetching results (memories + topics), use `tiktoken` to count cumulative token load as results are assembled
- Once cumulative tokens >= token_budget, stop appending results
- Append a truncation notice: `"[Truncated: token budget of <N> reached — showing X of Y results]"`
- Default: no budget ceiling (current behavior unchanged)
- Budget of 0 or negative: treated as unbounded

**Key constraint:** `query_long_term_memory` is platform-agnostic — it only reads the `--token-budget` value from whatever invokes it. The budget constraint lives in the caller, not in the tool logic.

### 2b. OpenClaw Plugin: Pass Token Budget to `mpm recall`

In `openclaw/mpm-plugin/src/index.ts`, update the `query_long_term_memory` tool's `runMpm` call:

```typescript
const result = await runMpm([
  "recall",
  "--json",
  "--token-budget",
  String(tokenBudget ?? 16000), // default wide-open unless channel constraint is set
  "--",
  query,
  String(limit),
]);
```

**Token budget detection (OpenClaw):**
- The plugin has access to `OpenClawPluginToolContext` — read `ctx.activeModel?.model` or similar metadata to detect Telegram vs other channels
- Alternatively: pass `--token-budget 1000` hardcoded for the Telegram plugin entry point; CLI/MCP server omit the flag
- The budget detection lives at the plugin entry point, not inside the tool — the tool itself remains generic

### 2c. Fallback Behavior

- If `--token-budget` not passed: recall behaves exactly as before (no truncation, all results up to `--limit`)
- If budget is reached: return partial results with a specific truncation notice: `"[Truncated: token budget of <N> reached — showing X of Y results]"`
- Never throw an error on budget hit — always return structured JSON with whatever was collected

---

## Codebase Rigidity Rules

- All synthesis API calls use the `synth` config block — no hardcoded URLs or API keys
- API timeout: use `context.WithTimeout` at `synth.timeout_seconds * time.Second`
- On API timeout: log to `watchdog.jsonl`, write original without merging, do not block ingestion
- Do not introduce vector embedding dependencies
- The `query_long_term_memory` OpenClaw tool remains completely agnostic to which platform invokes it
- All database writes route through `DatabaseManager` (WAL mode, no lock contention)

## Files to Modify

- `cmd/mpm/handlers.go` — add `--token-budget` flag to `handleRecall()`
- `cmd/mpm/simple_cmds.go` or a new file — synthesis handler with API call and resolution logic
- `cmd/mpm/router.go` — register synthesis/recall commands
- `internal/memory.go` — FTS5 near-miss detection in `AddMemory` or a new `DetectNearMiss()` method
- `openclaw/mpm-plugin/src/index.ts` — pass `--token-budget` from plugin context
- `mpm_config.json.example` — ensure `synth` block is documented

## Verification

- `go test ./...` all pass
- `mpm recall test --token-budget 100` returns at most N results, with truncation notice when budget hit
- Synthesis: ingest two near-miss memories, verify merged LTM written and old fragments soft-deleted
- Synthesis failure: verify original memory written, error logged to `watchdog.jsonl`