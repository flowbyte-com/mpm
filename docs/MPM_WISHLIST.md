# MPM Improvement Wishlist

A running list of things I want to add or change in MPM. Updated as I think of things — not promises, just direction.

---

## In Progress

_(empty — all queues clear)_

---

## Medium-Priority

_(all medium-priority items complete — see Completed below)_

---

## Low-Priority (Nice to Have)

_(all low-priority items complete — see Completed below)_

---

## Completed ✅

| # | Item | Status |
|---|------|--------|
| 1 | Recall explanation — score + rationale chips per result | ✅ Done (2026-05-14) |
| 2 | Frequency-weighted reinforcement — auto-elevation + `mpm review` digest | ✅ Done (2026-05-14) |
| 3 | Topic auto-suggestion — link to related topics on save | ✅ Done (2026-05-14) |
| 4 | Stale memory flag — inline flagging in recall output | ✅ Done (2026-05-14) |
| 5 | Better directives UI — grouped + age display via TS layer | ✅ Done (2026-05-15) |
| 6 | Epistemology Engine — Decision Ledger + Theory Tracker | ✅ Done (2026-05-15) |
| 7 | `UpdateMemoryMetadata()` + `mpm patch-memory` command | ✅ Done (2026-05-15) |
| 8 | `resolve_theory` in-place metadata patch (no FTS re-index) | ✅ Done (2026-05-15) |
| 9 | Cross-reference linking — bounded bidirectional Memory↔Topic↔Reference | ✅ Done (2026-05-15) |
| 10 | Reference chunking control — `--chunk-size` flag, tiktoken batch encode/decode | ✅ Done (2026-05-15) |
| 11 | Session memory context — `mpm wake` + active_mode/persona injection | ✅ Done (2026-05-15) |
| 12 | Memory decay scheduling — `mpm gc` (--dry-run/--review/--purge), computeDecay (float64), implicit reinforcement on recall (+0.5, capped +1/hr), weight > 0 filter in all recall queries | ✅ Done (2026-05-15) |
| 13 | Tag autocomplete — `mpm _suggest_tags <prefix>` (hidden command) + `scripts/mpm-completion.sh` bash/zsh TAB wiring | ✅ Done (2026-05-15) |
| 14 | Reference source tracking — `--source` flag override, `FilePath` field surfaced as `[Source: …]` in list/search/get output | ✅ Done (2026-05-15) |
| 15 | Proactive Deadlock Observability | ✅ Done (2026-05-19): DatabaseManager watchdog with `watchdog.jsonl` (separate from mirror.jsonl), exponential backoff, 100ms slow-query threshold. `ExecTracked`, `QueryTracked`, `QueryRowTracked` methods. |
| 16 | Fsnotify Reconciliation Sweep | ✅ Done (2026-05-19): 30s startup delay + 10-min periodic sweep via `time.NewTicker`; 25 file/sweep cap; `source_path` metadata check to detect already-ingested files; `EventReconciliationSweep` worker pool event. |
| 17 | Context-Aware Synthesis Deduplication | ✅ Done (2026-05-19): Fixed synthesis to delete triggering memory after LTM save, preserve oldest `created_at`, transfer topic_memberships, exclude epistemology collections, quality gate ≥2 candidates. |
| 18 | Proactive Review Hook | ✅ Done (2026-05-19): FTS5-triggered recall hints via `mpm hint` and `proactive_recall_hint` MCP tool; `ExtractConversationKeywords` (tiktoken, stopword filter); `FindEpistemologyOverlaps` with bm25; quality rules (one hint/turn, score >= -3.0, 10-turn suppression window); bugfix: bm25() zero-weight arg → no-arg form in both synthesize.go and keywords.go |
| 19 | Context Switcher (`mpm ops switch`) | ✅ Done (2026-05-19): Interactive TUI for persona + mode switching; `ActiveState` struct with `loadActiveJSON`/`saveActiveJSON`; `GetSystemPrompt()` reads active frontmatter; multi-select modes (comma-separated); graceful fallback; wired to both `ops switch` and root `switch` |
| 20 | Autonomous Epistemological Pruning | ✅ Done (2026-05-19): `mpm challenge <id> "<evidence>"` — weakens by 3, creates pending theory + decision; `mpm ops gc --shred-negative` shreds only weight<0 AND proven theory exists; immune system workflow deployed; MCP tool `challenge_memory` registered in OpenClaw plugin |

---

## Deprecated / Not Doing

| # | Reason |
|---|--------|
| ~~Unified blob storage (file-based mode/persona)~~ | File-based is correct — human-readable, easy to edit, diff-friendly. Not debt. |

---

## Cleanup (Technical Debt)

- ~~`synthesize` command~~ — ✅ Removed 2026-05-15 (deleted synthesize.go, unregistered route, removed from help, stripped from watch daemon)
- ~~The `--json` flag pre-scanning hack~~ — ✅ Done (2026-05-15): `ExtractJSONFlag()` centralized in `router.go`, all 16 handlers migrated
- ~~`TestChunkByTokens_EdgeCases`~~ — ✅ Fixed 2026-05-19: (1) `strings.TrimSpace(content)==""` guard moved BEFORE `getTiktokenEncoder()` call — previously tiktoken init failure would cause error on empty content before reaching the guard; (2) `return []Chunk{}, nil` → `return nil, nil`. Empty slice vs nil distinction in Go caught both issues.

---
## Future Wishlist (2026-05-30)

### v1.2 — Automatic Theory Resolution

Closed loop for the epistemological pruning system:

| # | Item | Status |
|---|------|--------|
| 21 | **Bidirectional links** — store `challenged_theory_id` in memory metadata and `memory_id` in theory metadata at challenge time | Pending |
| 22 | **Transactional challenge** — atomic transaction: metadata patch + theory creation with rollback on failure | Pending |
| 23 | **Auto-resolve on restore** — `mpm challenge restore <id>` resolves linked theory (status → disproven) and clears memory metadata in one tx | Pending |
| 24 | **Cascade theory delete on shred** — `mpm shred <id>` deletes linked theory alongside memory in one tx | Pending |
| 25 | **Warning injector** — LLM-visible `[Note: This memory is challenged — treat as unverified]` prepended to content at recall time | Pending |
| 26 | **[CHALLENGED] chip** — human-visible flag in `mpm ls` and `mpm show` output | Pending |

Spec: `docs/PRUNING_AUTOMATION.md`


### v1.3 — Optional

| # | Item | Status |
|---|------|--------|
| 27 | `mpm resolve <theory_id> --proven\|--disproven` — manual adjudication for edge cases where human override is needed | Pending |

### Item #3 Rewrite: Context-Aware Deduplication

~~**Old scope:** `content_hash` exists; `mpm dedup` to find/merge near-identical memories by dropping one.~~
**Done:** Context-aware synthesis deduplication via LLM — compresses redundant fragments into richer LTM records. ✅

### Proactive Review Hook

~~**Proactive Review Hook** — FTS5-triggered recall hints during conversation with STATUS and RATIONALE.~~ **Done.** ✅

### Context Switcher (`mpm ops switch`)

~~**Context Switcher** — unified interactive TUI for persona + mode switching.~~ **Done.** ✅