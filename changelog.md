# Changelog

## 2026-08-13 — Wake-Context Bridge, OpenClaw Flush Bridge, Synth Auth, History Rewrite

> **[!] WARNING — git history was rewritten on this date.** Local and remote
> commit SHAs on `main` changed because ~323 MB of accidentally-tracked
> binaries, literary corpora, and runtime artifacts were purged from
> history. **Anyone with an existing clone must rebase or re-clone before
> continuing work** — a standard `git pull` will produce a tree of conflicts
> on the first commit it tries to advance:
>
> ```bash
> git fetch && git reset --hard origin/main
> ```
>
> (If you have local commits, the equivalent is
> `git rebase --onto origin/main <upstream-of-your-branch>`. The active
> `worktree-agent-*` worktrees are in a forked state until rebased.)
>
> End state: `.git` is **358 MB → 35 MB** (-90 %). See
> *Repo Hygiene — History Rewrite* at the end of this entry for what was
> changed and why it had to be three filter-repo passes.

### Wake-Context Bridge (`read_wake_context` / `mpm_context`)
- **`fix(wake-context): overdue_wakes always-on field`** — `WakeContextData` gained an `OverdueWakes []OverdueWake` slice surfaced via a new `gatherOverdueWakes()` method (capped at the 5 most-overdue, pure read, no mutation). Rendered in `formatWakeContext` as `**Overdue Wakes (n, capped at 5):**` with `id + [kind] + reason (truncated 100 chars) + overdue duration`. Wired into `handleReadWakeContext`'s MCP response map (`internal/core/tools/handlers.go`) which had been projecting the struct via hand-built `map[string]interface{}` — the new field was structurally absent before.
- Field is **always-on** in JSON (no `omitempty`) so the agent can distinguish "no overdue work" from "this signal isn't wired." Each row carries `id, target_time, reason, kind, overdue_secs` so the agent can branch on kind (`task` / `reminder` / `drill` / etc.) without parsing reason text.
- Closed a **silent-failure class** at the bootstrap surface: scheduled wakes from `mpm_call ScheduleWake` were invisible to the agent on the wake-event itself. Companion second compounding bug discovered — `CheckPendingWakes(time.Now(), nil)` defaults to `kind='notification'` only, so reminder / drill / task kinds never auto-dispatched either. `overdue_wakes` now catches ALL overdue kinds via pure read.

### OpenClaw Memory-Flush Bridge
- **`feat(openclaw-mpm-memory): real `flushPlanResolver`** — plugin no longer returns `{ kind: "noop" }`. The new plan writes flushed content to `relativePath: ".mpm/run/ingest.md"` (resolves to `/home/v/.mpm/run/ingest.md` for the main session workspace). Dist-contract verified against OpenClaw's `agent-runner.runtime` `runMemoryFlushIfNeeded` — schema consumes `relativePath`, `systemPrompt`, and the token-threshold fields together.
- **`feat(scheduler): `openclaw_ingest` tick handler`** — per-tick file watcher drains `/home/v/.mpm/run/ingest.md` into `scheduled_wakes` with `kind: "ephemeral_compaction_ready"` and `metadata.source = "openclaw_ingest"`. **7 safety rails** (because this is a filesystem→agent-context vector, see the AgentBaiting lesson `9b349f3869c7eb07`):
  1. Path allowlist (hardcoded const, not configurable per-instance in production)
  2. `Lstat` symlink refusal — symlink to `/etc/passwd` or any system file would be a textbook filesystem-to-context injection
  3. 64 KB hard size cap; oversize quarantines to `*.rejected` (rename, not delete) for v inspection
  4. Atomic rename to `*.processing` before read so a concurrent writer can't corrupt the read
  5. World-readable file rejection (`perm & 0o077 != 0`) per AGENTS.md "External vs Internal" directory discipline
  6. Source attribution `metadata.source = "openclaw_ingest"` distinguishes bridge ingests from explicit `mpm_call ScheduleWake` invocations in audit trails
  7. Non-fatal throughout; every failure path logs and returns nil so the scheduler tick cannot die from this handler
- **7 tests** cover no-file / normal / symlink / oversize / world-readable / directory-target / suffix-shape. Full 11-package test suite green post-merge.
- **chore(openclaw): enable `agents.defaults.compaction.memoryFlush.enabled = true`** in `~/.openclaw/openclaw.json` (live runtime config, not in repo).
- **Operational note for operators:** if `mpm-scheduler` is not running, the plugin will write to `~/.mpm/run/ingest.md` and the file will silently pile up — there is no automatic cleanup. The scheduler tick is the only mechanism that drains the path. Run `journalctl --user -u mpm-scheduler -n 20` to confirm the handler is alive after `systemctl --user enable --now mpm-scheduler`.

### Synth Auth ProfileFor Fallback
- **`fix(synth): fallback `cfg.LLM.Default.APIKey` when `cfg.Synth.APIKey` empty`** at `internal/core/synth/client.go`. The synth client's `NewSynthClient` now also walks `cfg.ProfileFor("synth")` (Components binding → Profiles[default] → legacy Synth block) before falling back to env vars (`MINIMAX_API_KEY` / `OPENROUTER_API_KEY` / `OPENAI_API_KEY`). One-line change at the existing "key resolution" stage.
- Plus a **loud `slog.Error`** when every source is dry, with the full `surface_exhausted` list: `synth.api_key`, `profile.api_key`, `MINIMAX_API_KEY` (default wire), `OPENROUTER_API_KEY` (openai wire), `OPENAI_API_KEY` (openai wire). Without this log, "no API key configured" failures would surface only at request time when the LLM call returns 401 — well after the substrate has been alive long enough to appear healthy. Loud failure beats silent spinning.
- **Note for future operators:** the canonical key location is `cfg.Profiles[default]` under the `llm` block. The legacy `synth.api_key` block was retained as a backwards-compat path; the unblock path when auth fails is to populate `Profiles[default].api_key` (or set `MINIMAX_API_KEY`).

### Documentation & Substrate Hygiene
- **6 lessons + 1 decision** saved to MPM (the substrate's own long-term memory) across this work:
  - `aa77c528a691937e` — MiniMax retention verdict (MiniMax retained as primary; hybrid routing is the structural answer to the 5h pain, not a provider swap)
  - `1d333bf9e0d34d1c` — Architectural gap (wake-context bridge never queried `scheduled_wakes` at all)
  - `cb1fbd8be04b87c8` — Shell-escaping gotcha (bashed `$1` ate `$14`; always single-quote MPM fact content)
  - `eb43d421038e76bb` — Second compounding bug (CheckPendingWakes kind filter)
  - `f36abc8072986f47` — Compaction `force=true` requirement
  - `60a36a83df2179ed` — Structural-auth canonical-resolver pattern (don't patch legacy secret blocks; route through the canonical resolver instead)
  - `688c00a1a3ff39bc` — git-filter-repo multi-pass traps ([conflicted] suffix, prefix-not-matching rule, gc step)
  - Decision record `ad518f4ac00943ee` — wake-context bridge patch rationale
- 4 docs/superpowers files (`docs/superpowers/plans/2026-08-05-adaptive-drain-yielding.md`, plus 3 specs) untracked via `git rm --cached`. Files kept on disk per the existing `docs/superpowers/` `.gitignore` policy (ephemeral plan scaffolding; git log narrates the outcome).

### Repo Hygiene — Git History Rewrite
- **`.git`: 358 MB → 35 MB** (-90 %). Re-clone window follows the WARNING at the top of this entry.
- 3 filter-repo passes were required because the first two missed paths that the post-filter verification surfaced. See lesson `688c00a1a3ff39bc` for the gotcha taxonomy (`[conflicted]` suffix on the SHA1-collision rename; `--path X` does NOT also match `X-sibling/...`). **This is exactly why the WARNING at the top of this entry is loud — don't trust a single-pass filter-repo.**
- Classes purged:
  - `bin/mpm`, `bin/mpm-808`, root-level `mpm` / `mpm.bak` / `mpm-test` — accidental commit of build outputs
  - `src/db/mpm.db.pre-handoff-ddl-1786301641.bak` (15 MB SQLite backup), `internal/core/src/db/mpm.db` (15 MB), `src/db/mirror.jsonl` (logged runtime mirror) — runtime artifacts
  - `openclaw/mpm-plugin/node_modules/*` (npm-installed `typescript` + `esbuild` bundles — rebuildable from `package.json`)
  - `reference/`, `reference-sources/`, `gutenberg_sources/` — literary reference corpora
- Backup tag `pre-filter-repo-cleanup-2026-08-13` was created before the rewrite and deleted after verification — no permanent rollback path through git itself (but the pre-rewrite working tree was preserved on disk).

### Test Suite
- Full suite still green: 11 internal packages + 4 cmd binaries. New tests added in this commit-sequence:
  - `internal/core/wake_context_overdue_wakes_test.go` — 11 tests for `gatherOverdueWakes` (path allowlist / kind extraction / overdue_secs floor / cap / empty-reason skip / size limit / ordering / etc.)
  - `internal/core/tools/handlers_test.go` — `TestHandleReadWakeContext_IncludesOverdueWakes` pins the handler-layer projection contract (the second drift point that the original bridge patch caught)
  - `internal/scheduler/ingest_test.go` — 7 tests for the file-watcher safety rails

### Local Commit Manifest (post-rewrite SHAs)
The 5 original commits were rewritten with new SHAs by `git-filter-repo`; only the SHA values changed, the commit-message text was preserved:

| Original SHA | New SHA | Subject |
|---|---|---|
| `412b035` | `7c6bfb7` | `fix(wake-context): surface overdue scheduled_wakes at bootstrap` |
| `a9099fd` | `a24e917` | `fix(synth): fallback API key to Profiles[default] + loud fail on dual-empty` |
| `eb3ffc4` | `23762da` | `feat(openclaw-mpm-memory): replace noop flushPlanResolver with real plan` |
| `fd799d1` | `e859711` | `feat(scheduler): openclaw memory-flush ingest handler` |
| `48d19c8` | `65ea6e8` | `chore(docs): untrack four completed-work specs/plans from docs/superpowers/` |

## 2026-08-09 — mpm-alpha: Epistemic Cascades, Universal Scheduler, Provenance

### Epistemic Cascades (#2, #5, #6)
- **feat(core): dependency-aware invalidation** — `epistemic_cascade_outbox` + `MaterializeCascadeIntents`; cascades propagate invalidations transitively (max depth 3, max retries 3) so shredding a foundational directive re-materializes dependents
- **refactor(core): stateless materializer** — dropped the goroutine-pool lifecycle (`Start`/`Stop`/`<-stopC`) in favor of a pure `NewCascadeMaterializer` factory + single-batch `MaterializeBatch`; tests moved to a state-machine model
- **feat(cli): `mpm cascade materialize`** — out-of-band drain with flock lockfile (`cascade.lock`) to prevent concurrent drains; `mpm cascade list-dead-letters` for dead-letter inspection
- **feat(scheduler): `cascade_drain` tick handler** — per-tick yield budget (30s time budget, batch size from `system_config.cascade_drain.max_intents_per_tick`, default 50) with yield-reason taxonomy (`queue_empty` / `budget_exhausted` / `context_cancelled` / `error`); panic-safety wrapper; audit row per batch; `cascade_summary` wake kind with idle-tick dedupe
- Docs: runbook for scheduler-driven cascade drain; adaptive-yield specification (stair-step omitted per design review)

### Universal Scheduler (`mpm-scheduler`)
- New binary: universal wake executor with flock singleton (`scheduler.lock`), 60s ticker, wake dispatch (`snapshot`, `critic_audit`, `gc`, `broadcast`, `cascade_summary`, `cascade_drain`)
- `RegisterTickHandler` for unconditional per-tick work; tick handlers run sequentially after wake dispatch so system wakes always fire on cadence
- Agentic Cron: `scheduled_tasks` polled each tick; `next_run_at` rollover + wake injection in one transaction (crash-safe)

### Artifact Provenance (alpha telemetry)
- `artifact_provenance` table + analytics views (`v_model_memory_yield`, …); SAVEPOINT-isolated writer with validation; env-var resolver with priority chain
- Hooks on `saveMemoryRow` / `AddLesson`; cascade materializer threads `parent_artifact_id`; CLI surface via `mpm provenance`
- Legacy `metadata.provenance` JSON injection removed

### Capability System
- CS-3 `mpm capability grant-operator` shipped — operator bootstrapping sequence complete

### mpm-lint AST Engine & Audit Gates
- Unified `mpm-lint` AST engine replacing the legacy per-audit binaries; gates for transactions, contexts, goroutines, mutex, sql, file descriptors, imports (architectural boundary rule), and scan-error handling
- Wired into pre-commit (consolidated gate run) and GitHub Actions (violations as PR annotations, `--gate` in build-test job)
- `scripts/stranger-test.sh` added as a permanent release gate; 67/68 silent-continue error sites hardened

### Database Reliability
- Foreign keys enforced on all pooled SQLite connections; connection-pool leak patches (`audit-closes` gate)
- Phase 7 of timestamp migration: `ErrTimestampsMigrationDeferred` deferral stripped, full INTEGER rebuild; recall/semantic-search LIKE-fallback column/scan alignment fixes

### Security
- Data-plane file permissions tightened to 0600; startup permissions check with auto-heal; auto-created directories restricted to 0700
- AGPL-3.0 license surfaced at top of README

### CLI / UX / CI
- Install default flipped to user-space (`~/bin`); mode/persona set tightened to 3+3 with safe fallback
- Route system: three-state gate (blank/auto/manual) for `handleRoute`; route_render supersedes route_apply; hermetic fixtures for route/recall tests
- CI: coverage reporting in gate job; `CGO_LDFLAGS=-lm` for FTS5 bm25; hermetic `TestCallRoute_ReturnsReport`; ingest no longer leaks roadmap text
- OpenClaw agent plugins introduced (auto-route, memory) under `agent-plugins/`

### Repo Hygiene
- Untracked `.opencode/`, `reference/` corpus, and `CLAUDE.md` from git (gitignored; kept locally); scrubbed runtime logs, duplicate phrase lists, ephemeral smoke scripts

## 2026-07-30 — Unix-Epoch Timestamp Migration

- **feat(core): unify all timestamp columns on unix-epoch seconds** — All 37 (table, column) pairs across 19 tables migrated to INTEGER seconds via `timestamps_unified_v1` sentinel. Go struct fields become `int64` / `*int64`. CLI inputs accept both RFC3339 and unix-epoch integers. Display formatting centralized at `FormatUnixSeconds` / `FormatOptionalUnixSeconds`. **Operators must take `mpm backup-db` before installing this release.**

## 2026-07-07 — Architecture Split & Security Audit

### Audit & Remediation (15 findings patched)

**Critical**
- `handlers_backup.go` — replaced `exec.Command(sqlite3, ".read " + path)` with `os.ReadFile` + `db.Exec`
- `web.go` — added `http.Server` timeouts (Read/Write 30s, Idle 60s)
- `web_handlers.go` — added `http.MaxBytesReader` (10 MiB cap) to all 5 body-reading handlers

**High**
- `web.go` — added `withSecurityHeaders` middleware (X-Content-Type-Options, X-Frame-Options, Referrer-Policy); CORS preflight; `MkdirAll` before port file write
- `embeddings.go` — fixed JSON injection via `json.Marshal` instead of string concat; `sync.Once` caching for config probe

**Medium**
- `app.js` — token storage moved from `localStorage` to `sessionStorage` (with fallback)
- `web.go` / `web_handlers.go` — `serverError()` helper replaced 15 raw error-leak call sites
- `memory.go` / `web_db.go` — all `CURRENT_TIMESTAMP` comparisons against `expires_at` converted to `strftime('%s','now')`
- `handlers.go` — `getDB()` singleton via `sync.Once` with `closeDB()` helper
- `db.go` — startup auto-rotation for `watchdog.jsonl` and `mirror.jsonl`

**Low**
- `app.js` — `esc()` now escapes single quotes
- `web.go` — added `Access-Control-Allow-Origin: *` + OPTIONS preflight
- `handlers.go` — `closeDB()` logs errors via `usererror.Warn`
- `web.go` — `os.MkdirAll` before port file write

**Score: 90/100** (full report in `audit.md`)

### LessonType Validation
- Added `ValidateLessonType()` with strict allowlist map in `internal/core/db.go`
- Validated at all 4 entry points: core `AddLesson`, web API, CLI, MCP tool

### Phase 1 — Module Boundary Rename
- Renamed `internal/` → `internal/core/`
- Moved all 7 subpackages (`config`, `tools`, `synth`, `usererror`, `logging`, `mpmcli`, `seed`)
- Updated all import paths project-wide; build + tests pass

### Phase 2 — CoreDB Interface
- Defined `CoreDB` interface (~130 methods) in `internal/core/core.go` with `var _ CoreDB = (*DatabaseManager)(nil)` compile-time assertion
- Switched `WebServer.db`, `getDB()`, `openCallDM()`, `HandlerFunc`, all 60+ tool handlers, `MemoryStore.DM` to use `CoreDB`
- Exported `AdmitResult` / `AdmitChainEntry` (were unexported `admitResult` / `admitChainEntry`); build + tests pass

### Phase 3 — Runtime Isolation
- Added `NewSession() (CoreDB, error)` to `CoreDB` interface; `DatabaseManager` implementation opens an independent `*sql.DB` + inits schema + attaches shared DB
- Updated fire-and-forget synthesis goroutines to use `dm.NewSession()` instead of `mpminternal.NewDatabaseManager("")`
- Changed `AutoSynthesize`, `DetectNearMiss`, `logWatchdogOp` to accept `CoreDB` instead of `*DatabaseManager`
- Fixed `sqlopen_owner_test.go` directory paths; build + tests pass

### Phase 4 — Standalone Core Module
- Created `internal/core/go.mod` with `module github.com/flowbyte-com/mpm-core`
- Updated all `mpm/internal/core` → `github.com/flowbyte-com/mpm-core` import paths (~70 files)
- Main `go.mod` uses `replace github.com/flowbyte-com/mpm-core => ./internal/core` for local development
- Updated `Makefile` test target to run both modules; `go mod tidy` on both cleaned stale dependencies
- Updated `CLAUDE.md` paths, test commands, and module structure

### Files Changed
- `internal/core/` — new standalone Go module (moved from `internal/`)
- `internal/core/core.go` — `CoreDB` interface (new, ~130 methods)
- `internal/core/admission.go` — exported `AdmitResult`, `AdmitChainEntry`
- `internal/core/db.go` — `NewSession()`, `ValidateLessonType()`, `ValidLessonTypes`
- `internal/core/go.mod` — standalone module definition
- `cmd/mpm/` — all files updated to import from `github.com/flowbyte-com/mpm-core`
- `cmd/mpm-mcp/` — updated imports; `main.go` uses `getDB().NewSession()` for synthesis
- `go.mod` — added `replace` directive; removed stale dependencies
- `Makefile` — test target split across both modules
- `docs/ARCHITECTURE_SPLIT.md` — 4-phase migration plan (new)
- `audit.md` — comprehensive audit report (new)
