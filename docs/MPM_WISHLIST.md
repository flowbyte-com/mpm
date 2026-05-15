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

---

## Deprecated / Not Doing

| # | Reason |
|---|--------|
| ~~Unified blob storage (file-based mode/persona)~~ | File-based is correct — human-readable, easy to edit, diff-friendly. Not debt. |

---

## Cleanup (Technical Debt)

- ~~`synthesize` command~~ — ✅ Removed 2026-05-15 (deleted synthesize.go, unregistered route, removed from help, stripped from watch daemon)
- ~~The `--json` flag pre-scanning hack~~ — ✅ Done (2026-05-15): `ExtractJSONFlag()` centralized in `router.go`, all 16 handlers migrated

---

*Last updated: 2026-05-15*
---

## Future Wishlist (2026-05-15)

| # | Priority | Item | Notes |
|---|----------|------|-------|
| 1 | Medium | Per-collection decay policies | Different halflife/floor per collection (sessions vs decisions vs theories) |
| 2 | Medium | Explicit memory TTL | Hard `expires_at` alongside probabilistic weight decay |
| 3 | Low | Memory deduplication | `content_hash` exists; `mpm dedup` to find/merge near-identical memories |
| 4 | Low | Structured recall filters | `--weight-below`, `--before`, `--collection` combinations for memory archaeology |
| 5 | Low | Interactive memory add | `mpm add -i` stdin editor, multi-line preview before persist |
| 6 | Low | Memory snooze | `mpm snooze <id> [--days N]` bump weight + touch `last_accessed_at` without marking LTM |

*Last updated: 2026-05-15*
