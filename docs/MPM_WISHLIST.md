# MPM Improvement Wishlist

A running list of things I want to add or change in MPM. Updated as I think of things — not promises, just direction.

---

## In Progress

_(nothing currently in progress — see High-Priority below)_

---

## High-Priority (Do Next)

### Cross-reference linking — DONE (2026-05-15)
Memories, lessons, topics, references — now linked via bounded bidirectional cross-refs. Memory recall shows linked Topics + parent Reference doc. Topic recall shows top-3 memories + count chip. Phase 1 complete.

### Reference chunking control
Fixed chunk size with no user control. Sometimes you want smaller chunks for precision search, larger for context. Make chunk size configurable per-ingest via `--chunk-size` flag (tokens).

---

## Medium-Priority

### Session memory context
On session start, show: "last session you worked on X, had Y open, learned Z." Store `active_mode` and `active_persona` in memory metadata on save. Jump-back context without full recall.

### Memory decay scheduling
TTL is manual. Should be automatic: high-weight memories → slow decay; low-weight + unaccessed → accelerate decay. Eventually suggest deletion instead of infinite storage.

### Session context continuity
File-based mode/persona is correct (human-readable, diff-friendly). The real need: store which mode/persona was active when a memory was saved, and surface "you were in programming mode last session" on session wake-up.

---

## Low-Priority (Nice to Have)

### Bulk import/export
Dump memories to JSON for backup, import from other agents.

### Memory pruning with confirmation
"12 memories haven't been accessed in 90 days — review or delete?"

### Tag autocomplete
When saving with tags, suggest existing tags from similar memories.

### Reference source tracking
Track where ingested documents came from (URL, file path, date) and surface on search results.

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

---

## Deprecated / Not Doing

| # | Reason |
|---|--------|
| ~~Unified blob storage (file-based mode/persona)~~ | File-based is correct — human-readable, easy to edit, diff-friendly. Not debt. |

---

## Cleanup (Technical Debt)

- ~~`synthesize` command~~ — ✅ Removed 2026-05-15 (deleted synthesize.go, unregistered route, removed from help, stripped from watch daemon)
- ~~The `--json` flag pre-scanning hack~~ — ⚠️  Deferred. Build is clean. Standardization is a larger refactor (see router.go helper approach). Would need `ParseArgsWithJSONFlag()` in router + migrate handlers one-by-one.

---

*Last updated: 2026-05-15*