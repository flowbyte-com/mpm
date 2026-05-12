# MPM Todo — Prioritized

## 🔴 Must Fix (broken functionality)

_(None — all items below were already fixed in commit de27094)_

---

## 🟡 Should Fix (correctness / UX)

_(None — all items below were already verified working)_

---

## 🟢 Nice to Have

_(None at this time)_

---

## Completed (historical — for reference)

### Fixed in commit de27094 ("fix: three gremlins from daemon-to-unified refactor")

- ✅ `mpm recall` FTS5 query — uses correct `memories_fts MATCH ?` pattern with `JOIN ... ORDER BY fts.rank`
- ✅ `mpm memory show <id>` — `store.GetByID()` works correctly
- ✅ `mpm lesson add` — completes successfully (no hang)
- ✅ `mpm recall` vs `mpm memory search` — both use aligned FTS5 approach
- ✅ `mpm memory list` truncation — 500-char limit with `...` suffix works correctly
- ✅ Synth null handling — correctly skips unmarshal when LLM returns `null`
- ✅ Synth success/failure reporting — correctly differentiates "no facts" vs "all saves failed"
- ✅ Synth model configurable via `mpm_config.json` — reads `Model`, `APIKey`, `BaseURL`, `MaxTokens`, `TimeoutSecs`

### Prior completed work

- ✅ Multi-source watch daemon (memory_dirs, sessions_dirs, external_dbs)
- ✅ Detached watcher with PID file lifecycle
- ✅ Graceful SIGTERM/Interrupt shutdown
- ✅ PID timing fix (writeWatchPID after pool start)
- ✅ Systemd unit for reboot persistence
- ✅ FTS5 triggers use `CREATE TRIGGER IF NOT EXISTS`
- ✅ Common indexes use `CREATE INDEX IF NOT EXISTS`
- ✅ Removed dual DB (`mpm_memory.db` deleted)
- ✅ Unified single-process architecture (no daemon subprocess)
