# AGENTS.md

Headless SQLite substrate for long-lived autonomous systems. Single-process, single-database, no socket IPC, no HTTP server in `mpm` proper.

## 1. Commands & Build Flags

```bash
make build                          # bin/{mpm, mpm-mcp, mpm-scheduler, mpm-critic, mpm-telemetry}
make test                           # all Go tests (FTS5 build flags, no race detector)
make test-race                      # all Go tests WITH the race detector (canonical CI gate)
./install.sh                        # canonical user-space install (build + systemd + PATH symlinks)
./uninstall.sh                      # canonical runtime-only uninstall (data preserved)
./uninstall.sh --purge              # remove runtime + persistent state
./uninstall.sh --shred              # remove + best-effort secure overwrite
./scripts/deploy.sh                 # maintainer: rebuild + restart gateway + mpm-scheduler + mpm-telemetry
```

**`make test` vs `make test-race`:** both targets carry the same FTS5 build flags.
The race-detector variant adds `-race` for goroutine/data-race coverage. Use
`make test-race` as the canonical pre-merge validation gate. Bare
`go test -race ./...` (no flags) cannot work in this codebase because the
substrate's INSTEAD OF / AFTER triggers on `scheduled_wakes` and the
`lessons` view assume FTS5 is compiled in — see §3 (Substrate Defense
Triad / FTS5 integrity). The `make test-race` recipe is pinned by
`internal/core/build_config_invariants_test.go`.

**Required for FTS5** (silently broken otherwise):

- `CGO_CFLAGS=-DSQLITE_ENABLE_FTS5=1`
- `go build -tags fts5` (for `mattn/go-sqlite3`)

The Makefile sets both. `make install` validates `$HOME/.mpm/bin/` is canonical — no sudo, no `/usr/local` split.

## 2. Architecture & Projection Test

**Single shared `DatabaseManager`** (`internal/core/db.go`) is the only connection pool. Every write goes through it; background goroutines (synthesis, idle dream, lifecycle decay) share the same `*sql.DB`. Watch daemon was removed — file ingestion flows through `mpm cascade materialize` invoked from cron/systemd.

**No HTTP in `mpm`.** The binary is CLI-only; HTTP/MCP surfaces belong to consumer binaries (`mpm-agent`, `mpm-mcp`) which implement their own auth at the protocol boundary.

**Path resolution:** `MPM_WORKSPACE` env → compile-time default (`$HOME/.openclaw/workspace/projects/mpm`) → CWD fallback.

**GOMAXPROCS cap:** `main.go` caps at 32. Don't raise — default-unlimited can OOM the host.

### The Projection Test

Before adding a new table, column, cache, score, or summary:

1. Can this be computed from authoritative state at read time?
2. Will read latency be acceptable at expected rates?

If yes to both: **do not persist it.** Compute on read. A derived value may only be persisted if updated atomically with every authoritative mutation. *(Database indexes are not persistence — add freely.)*

## 3. Hard Invariants

Don't reach for a different shape; extend `mpm-lint --gate` instead. Code violating a fatal class will not commit; **never** `git commit --no-verify` to bypass.

### Substrate Defense Triad

1. **Atomic state swap** for cross-process files (scheduler heartbeat, lock sentinels, manifests). Marshal to `<target>.tmp` then `os.Rename`. See `internal/scheduler/state.go:persistState` for the canonical pattern.

2. **Defensive SQL aggregates** — wrap target columns in `COALESCE(AGG(...), fallback)` or bind to `sql.Null*`. Empty result sets return NULL for `MAX/MIN/AVG`; scanning NULL into a concrete Go primitive panics. The linter does not yet cover this class; review is the only enforcement.

3. **Write-path read-back assertions** — `INSERT` returning `nil` from `db.Exec` does not prove persistence (CHECK constraints, `INSTEAD OF` triggers, rolled-back txns). Every write must read back via `GetX(id)` before returning success. Pattern shown in `internal/core/db.go:AddLesson`.

### Single-connection discipline (the deadlock trap)

**Do not call `db.SetMaxOpenConns(1)`.** It looks like the cleanest enforcement of SQLite's single-writer guarantee but creates textbook deadlocks: a transaction holds the only connection, a helper runs a bare-`db` lookup inside, that lookup blocks forever waiting for the connection the transaction holds. The codebase uses `*sql.Tx` discipline — thread transactions through helpers, don't bare-`db` inside enclosing transactions. MPM relies on WAL + `busy_timeout=5000` + tx discipline, not a runtime choke-point. Static whitelist: `internal/core/sqlopen_owner_test.go`.

## 4. Current CLI/API Interface Notes

- **`projection` is canonical** on `mpm_memory query`, `mpm_lessons search`, `mpm_lessons list` (`summary|full`). `mode` is the deprecated alias (kept one release). `projection` wins when both are set; unknown values are rejected.
- **Compact wake projection** — `mpm wake --compact` (or `mpm_context read_wake_context {projection: "compact"}`) returns a compact id+summary payload (~1-2 KB).
- **Decisions read symmetry** — `mpm_decisions show|list|query` and the MCP tool mirror this. Don't FTS-query your own decisions.
- **Skill save aggregates errors** — `mpm_skills save` returns `{success:false, errors:[...]}` (all errors). Pass `body` instead of `content` for minimal-frontmatter shortcut.
- **`session_id is required`** errors name both recovery paths: `mpm_context read_wake_context` → `session_current_id`, or `MPM_SESSION_ID` env var.
- **`call.go`** is the universal machine interface — register new tools there so other processes can invoke them via `mpm call <name> --payload <json>`.
- **`mpm_context recent_activity`** is the factual semantic-activity feed — newest successful mutating actions across mpm tools. Default scope: agent + human + unknown; system/diagnostic/maintenance excluded. Use this when an agent prompt asks "what changed?" / "what did agents write recently?" / "what did other agents do?". It is NOT relevance-ranked — it answers "what semantic durable activity happened?" not "what matters now?". Filters: `framework_name`, `session_id`, `actor_kind` (`agent|human|unknown|all`), `since` (unix seconds), `artifact_type` (tool name). Result envelope: `count`, `scanned_rows`, `scan_limit`, `truncated`, `history_exhausted`. Do NOT use `mpm_memory list` as a global activity feed — `recent_activity` is the substrate's authoritative cross-tool activity surface.
- **`mpm_context contextual_candidates`** is the deterministic, bounded, POINTER-FIRST candidate set — "what artifacts might matter to me RIGHT NOW, and why?". It composes active work, the latest handoff, recent semantic activity, epistemic dependencies, cascade obligations, wakes, the active scratchpad, topic neighbors, and any explicit artifact ids the caller supplies (`params.artifact_ids`). Generation is observational — it does NOT mutate any persistent state and may be called repeatedly for identical outputs. Every candidate carries one or more bounded reason tokens (open_work, overdue_wake, explicit_reference, cascade_pending, handoff_for_current_context, supersession_chain, …) so downstream selectors can explain WHY each artifact surfaced. The result envelope carries per-source considered/emitted/final counts, `unresolved_explicit_refs`, `input_truncated`, `total_raw`, `total_after_dedup`, `global_cap_applied` — so callers can distinguish "no relevant data" from "generator failed silently" and "input truncated at ExplicitRefInputMax" from "id did not resolve". Full content is NOT materialized; candidates carry pointers + bounded metadata only. Use `mpm_resolve` or `mpm_memory show` with the candidate's pointer/id when you need the actual content. Default global cap is 50; per-source caps are tuned so the diversity policy guarantees explicit references, overdue wakes, pending cascades, open work, and handoff continuity survive even when the global cap is hit hard. **DISTINCTION**: `recent_activity` answers "what HAPPENED?" (chronological event log); `contextual_candidates` answers "what might matter RIGHT NOW?" (bounded pointer set with reasons). They are complementary, not replacements. Selection-stage ranking / wake-context integration is Stage 2E (NOT YET IMPLEMENTED).