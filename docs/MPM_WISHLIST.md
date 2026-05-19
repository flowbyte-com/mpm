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
## Future Wishlist (2026-05-19)

_(All items complete — see Completed above.)_

### Item #3 Rewrite: Context-Aware Deduplication

~~**Old scope (Low priority):** `content_hash` exists; `mpm dedup` to find/merge near-identical memories by dropping one.~~
**New scope (Medium priority — DONE):** Instead of hash-based dedup, route near-misses through MiniMax-M2.7 backend to allow 808 to actively synthesize two redundant memories into a single, richer Long-Term Memory (LTM). The synth step becomes a first-class operation rather than a blind dedup. ✅

### Proactive Review Hook

~~**Proactive Review Hook** — FTS5-triggered recall hints during conversation with STATUS and RATIONALE.~~ **DONE.**