# MPM Data Quality — TODO

**Last updated:** 2026-04-02
**Status:** 🟡 In Progress

---

## Database Schema — Fix Missing Columns ✅ DONE

All schema columns now present in production DB:
- ✅ `content_hash TEXT` in memories (added by sub-agent, 2026-04-02)
- ✅ `deleted_at TEXT` in memories
- ✅ `reference_id TEXT` in memories
- ✅ `memory_id TEXT` in topic_memberships

## Data Backfill ✅ DONE

- ✅ **TODO-BF-002 (sessions table):** 4 sessions backfilled from .jsonl files — UUID, model, duration, message count, cwd, metadata
- ✅ **TODO-BF-003 (content_hash):** column added; backfill still needed for existing old memories
- ✅ **Pipeline rewrite:** `processSessionFile()` in watch.go now produces 1 session record + 1 summary memory per .jsonl. Does NOT delete .jsonl (kept as archive).

## Data Utility — Remaining

### DONE: Stale-lock timeout ✅
Added `staleSessionSweep()` goroutine — runs every 5 minutes, scans for `.jsonl`+`.lock` pairs idle longer than `--stale-timeout` (default 30m) and force-processes them. Prevents data loss from crashed/force-killed sessions.

### TODO-UTIL-003: Run dedup
**Problem:** Old memories (461 noise entries) have been purged. 4 session summaries now have `content_hash` set.
**How:** Run `mpm dedup` to check for any remaining exact duplicates.

### TODO-UTIL-004: Tag quality report
**Problem:** Old memories have sparse/no tags, new ones from auto-tagging (TODO-005) are richer.
**How:** Run a one-time report, consider `UPDATE memories SET tags = '["openclaw"]' WHERE tags = '{"session-fact":true}'` to clean up the old noise tag.

### TODO-UTIL-001: Session timeline view command
**Problem:** `sessions` table now has 4 records but `mpm session list` still shows empty.
**How:** Extend `handleSessionList()` to read from `sessions` table, show session timeline with model, duration, message count.

### TODO-BF-001: Backfill session_id for old memories
**Problem:** 0/461 old memories have session_id — still orphaned from sessions.
**How:** Match by timestamp window: for each old memory, find the session whose first_event_time ≤ memory.created_at ≤ last_event_time.

### TODO-BF-003: Backfill content_hash for old memories
**Problem:** content_hash column added but old memories have NULL hash.
**How:** `UPDATE memories SET content_hash = lower(hex(sha256(content))) WHERE content_hash IS NULL`

### TODO-UTIL-002: Daily/weekly memory digest
**How:** Add `mpm memory digest [days]` command.

---

## Ideas for Later (Backlog)

- [ ] Topic extraction from untagged memories (LLM-based clustering)
- [ ] `mpm brain` interactive explorer (browse by topic hierarchy)
- [ ] Session comparison ("how was today vs yesterday?")
- [ ] Memory age衰减 — older memories gradually reduce in search rank
- [ ] Export memories as markdown by topic/date range

---

## Completed

_(none yet — 2026-04-02)_
