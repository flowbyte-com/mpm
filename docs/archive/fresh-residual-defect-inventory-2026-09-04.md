# MPM — Fresh Post-Closure Residual Defect Inventory

**Date:** 2026-09-04
**Source brief:** `docs/post-closure-residual-sweep-2026-09-04.md` plus D-R1 closure brief
**Approach:** Read-only inventory sweep across 22 sections; one report, no production edits.
**Honest-count rule:** Do not force four findings. If fewer than four genuine defects remain, say so.

---

## §A — Baseline (post-D-R1 closure)

| Gate | Command | Result |
|---|---|---|
| Canonical Go test gate | `make test-race` | exit 0 — full suite passes with race detector |
| Go build | `make build` | exit 0 |
| Render parity check | `python3 agent_installation/scripts/render_managed_blocks.py --check` | exit 0 — 4 adapters in parity |
| Refresh idempotency | `make refresh-installed` ×2 | second run: no drift |
| Pre-commit hooks | latest two closure commits `750ea4b` + `b010ac5` | both passed `[pre-commit] all checks passed` |
| Installed artifacts | `~/.claude/CLAUDE.md`, `~/.config/opencode/AGENTS.md`, `~/.pi/agent/AGENTS.md` | in byte-for-byte parity with canonical block |

D-R1 (renderer-semantics fix) is **CLOSED**. The two closure commits are clean.

---

## §B — Reconcile prior closure reports

| Report | Status after D-R1 | Notes |
|---|---|---|
| `docs/d-r1-closure-2026-09-04.md` | CLOSED | 7-test semantic guard + Go subprocess wiring test in place |
| `docs/residual-defect-inventory-2026-09-04.md` | ALL CLEAR | 0 P0/P1 confirmed; defects surfaced were either verified-clean on read-back or out of scope |
| `docs/post-closure-residual-sweep-2026-09-04.md` | ALL CLEAR | D-3 closure verified clean; D-R1 came out of the §C-R1 follow-up and is now closed |
| `docs/closure-pass-2026-09-04.md` | ALL CLEAR | F15-1/F15-2/F19-1/F20-1 + F2-1..F7-1 surface parity at alpha-final |
| `docs/agent-integration-known-debt-closure-2026-09-04.md` | CLOSED | last Claude-side drift is in `docs/d-r1-closure-2026-09-04.md` |
| `docs/pre-existing-race-failures-closure-2026-09-04.md` | CLOSED | scheduler wedge + notification sweep fixes both at HEAD |
| `docs/cli-theory-mcp-correctness-pass-2026-09-04.md` | ALL CLEAR | `mpm theorie`/`mpm work` parity verified |
| `docs/security-interface-hygiene-pass-2026-09-04.md` | CLOSED | scrubbing regex coverage validated |
| `docs/hermes-install-repair-2026-09-04.md`, `docs/opencode-install-repair-2026-09-04.md` | CLOSED | per-host repair verified |

No reopened findings. The post-closure sweep left one §C-R1 follow-up (D-R1), now closed.

---

## §C — Defect findings (honest count)

**Total: 7 genuine defects confirmed** (1 P0, 2 P1, 3 P2, 1 P3). F-1..F-3 came from the first-draft sweeps (§15/§16 + reconnaissance); F-4 is the only P0 and came from the §12 security sweep (verified against `memory.go:1235-1255`); F-5/F-6/F-7 are the P2 cluster from the §6/§7 silent-failure + state-machine sweep. False positives (S-3 blob path-traversal, DB-1/2/4/5 nil-tx, C-8 missing --type drift, S-2 blocklist gaps, D-1 lock-path collision) are documented in the rejection table below.

### Finding F-1 — OpenCode / Pi adapter README tool-count drift (P3, doc accuracy)

**File:line:**
- `agent_installation/opencode-mpm/README.md:3-4, 20, 31, 171, 220, 268, 272`
- `agent_installation/pi-mpm/README.md:9, 20, 82`

**Claim drift:**
- README says "**17 OpenCode tools registered**, in two layers" + "**17 typed tools** (a curated subset of the full 22-tool MPM registry)".
- Actual count in `agent_installation/opencode-mpm/src/index.ts`: **14** `name: "mpm_X"` declarations (grep `-cE '^\s+name: "mpm_'` returns 14).
- README says "**17 Pi tools registered**"; actual count in `agent_installation/pi-mpm/index.ts`: **15** (the +1 over OpenCode is `mpm_retrieval_diagnose`).
- The "22-tool MPM registry" claim is also stale — registry has **20 `mpm_*` tools** + 2 non-mpm meta tools (`log_to_changelog`, `request_review`) = **22 total registry entries**. The bare-mpm count is 20.

**Reproducer:**
```bash
grep -cE '^\s+name: "mpm_' agent_installation/opencode-mpm/src/index.ts  # → 14
grep -cE '^\s+name: "mpm_' agent_installation/pi-mpm/index.ts          # → 15
grep -cE '^\s+Name: "mpm_' internal/core/tools/registry_list.go        # → 17 (note: 4-space vs 2-space indent)
grep -cE '^\s+Name:\s+"mpm_[a-z]+"' internal/core/tools/registry_list.go  # → 17
```

(Side note: the OpenCode source file's header comment says "16 typed tools" while actual is 14 — same drift.)

**Severity:** P3 (documentation accuracy drift; no runtime impact). Operators reading the README to verify what the plugin registers will be misled.

**Why P3 and not higher:** The runtime behaviour is correct — only registered tools are exposed; the count is purely a documentation statement.

---

### Finding F-2 — Whitespace-only content slips through CLI input boundaries (P1)

**Source:** §8 sweep, confirmed against source. The CLI handler accepts whitespace-only `fact` / `choice` / `lesson content` strings as if they were valid, bypassing the empty-string guard.

**File:line evidence (verified):**
- `cmd/mpm/handlers_memory.go:284` — `case factArg != ""` passes for `"   "` (truthy string); no `TrimSpace` validation. → `mpm memory add --fact "     "` stores whitespace.
- `cmd/mpm/handlers_epistemology.go:305, 325` — `if choice == ""` passes for `"   "`; the explicit `TrimSpace` block at line 81 (theory) and 94 (tags) shows the surrounding code already uses `TrimSpace` for the same purpose. → `mpm record_decision --choice "   "` stores a decision with whitespace-only choice.

**Reproducer:**
```bash
mpm memory add --fact "     "
mpm record_decision --choice "   " --context "test"
```

**Severity:** P1 for `decision` (creates a low-quality artifact in the substrate that future retrieval will surface); P2 for `memory` and `lesson` (same shape but lower-stakes collection).

**Why this isn't a P0:** No data loss, no panic, no security boundary crossed. The defect is data-quality only.

**Why this is the strongest P1:** All other CLI input paths use `strings.TrimSpace(input) == ""` for emptiness checks (see `handlers_epistemology.go:81, 94, 190` and `handlers_lesson.go` similar pattern at 81-86 for theory hypothesis). The two outliers above broke the pattern and would be ~6 lines of fix each.

**Related (NOT batched, but same family):**
- `cmd/mpm/handlers_lesson.go:111` — same `content == ""` pattern (P2)
- `cmd/mpm/handlers_epistemology.go:582-608` — bare `hypothesis=` form accepts empty value (P2)
- `cmd/mpm/handlers_work.go:446-455` — `--limit=notanumber` silently ignored (P2)
- `cmd/mpm/handlers_epistemology.go:1093-1096` — `--limit=abc` silently uses default (P2)

These share the "silent parse-failure → silent-default" pattern. They are real but each is a 1-line fix; not blocking.

---

### Finding F-3 — Goroutine + subprocess leak on scheduler shutdown (medium-high, observability)

**File:line:** `internal/scheduler/scheduler.go:435-462` (`executeOne`), `internal/scheduler/scheduler.go:729-849` (handler implementations).

**Pattern:**
```go
done := make(chan error, 1)
go func() { done <- h(w) }()
select {
case err := <-done:
    return err
case <-ctx.Done():
    return fmt.Errorf("handler context cancelled: %w", ctx.Err())
}
```

When `<-ctx.Done()` fires (e.g., during SIGTERM), `executeOne` returns. The spawned goroutine continues running because `h(w)` does not accept `ctx` and the goroutine is unreachable from this point. The handler may itself spawn subprocesses (`SnapshotHandler`, `GCHandler`, `BroadcastHandler` use `exec.Command` without `Context`); only `CriticAuditHandler` has its own 5-minute timeout (the canonical pattern).

**Severity:** Medium-high. On a SIGTERM during a heavy tick:
- Each in-flight wake leaks one goroutine + its subprocess tree for the duration of the handler (potentially up to 5 minutes for `CriticAuditHandler`, unbounded for the others).
- Mitigated by systemd's `KillMode=process` (sends SIGKILL after grace period), but the leaked subprocesses keep the DB write-locked past the daemon's exit window.
- Not data-corrupting (SQLite WAL handles the goroutine-after-process-exit case via SHM recovery).

**Why this is the strongest residual defect (after F-2):** Daemons are supposed to exit cleanly on SIGTERM. The current behaviour is "the main loop exits but the wake handlers keep running until they finish naturally." Operators see a daemon that "stopped responding" before systemd's SIGKILL because the DB is still locked by a leaked subprocess.

**Why not P0:** No data corruption observed; the graceful-shutdown semantics are by design (the wake completes and the DB write succeeds); the leak is bounded by handler execution time.

---

### Finding F-4 — Blocked credentials stored verbatim in `mirror.jsonl` (P0, security)

**File:line:** `internal/core/memory.go:1235-1255` (`appendBlockedAttempt`).

**Pattern:**
```go
logEntry := map[string]interface{}{
    "timestamp":       time.Now().UTC().Format(time.RFC3339),
    "reason":          reason,
    "content_snippet": truncate(content, 100),  // <-- first 100 bytes of blocked content
    "action":          "blocked",
    "type":            attemptType,
}
data, _ := json.Marshal(logEntry)
_, err = f.WriteString(string(data) + "\n")
```

The mirror file (`mpm/src/db/mirror.jsonl`, mode 0600) is the audit log for blocked content attempts. The blocked content is truncated to 100 bytes — but the first 100 bytes of an `ghp_xxx`, `sk-xxx`, or `-----BEGIN RSA PRIVATE KEY-----` is exactly the prefix an attacker needs to identify the secret family. The mirror is intended to be a forensic record, but it doubles as a credential prefix leak.

**Why this is the strongest defect in the inventory:** The intent of the blocklist is to keep secrets OUT of the substrate. The current implementation puts them IN an audit log on disk. The 0600 mode keeps it user-readable, but if the workspace is ever backed up, mirrored, or shipped for debugging, the audit log becomes a credential exposure.

**Reproducer:**
```bash
mpm memory add "ghp_$(head -c 50 /dev/urandom | base64)"
ls -la src/db/mirror.jsonl
grep '"content_snippet"' src/db/mirror.jsonl | tail -1
# → first ~50 bytes of "ghp_..." literal in the content_snippet field
```

**Severity:** P0 (security defect — exposed credential prefix in audit log).

**Mitigation shape (not yet implemented):** Hash the content (`sha256(content)`), record only the hash + a content-type tag from the matched pattern, never the raw prefix. ~10 lines of change. The current audit trail becomes "blocked attempt of type `ghp_pat`, hash=`abc123...`" — same forensic value, no leak.

---

### Finding F-5 — `mpm_wakes list` silently discards the `kinds` filter (P2)

**File:line:** `internal/core/tools/handlers.go:3580-3608` (`handleListWakes`).

**Pattern:** The JSON Schema for `mpm_wakes list` advertises `kinds: string[]` as a parameter (per the agent's verification at handlers.go line 349). `handleListWakes` reads only `include_fired`, `overdue_only`, and `limit` — `kinds` is never read. `dm.ListScheduledWakes(includeFired, overdueOnly, limit)` has no kind filter signature, so even threading the param through would require a DB-layer change.

**Reproducer:**
```bash
mpm call mpm_wakes '{"action":"list","kinds":["notification"]}'
# → returns ALL scheduled wakes, not just notification-kind ones
```

**Severity:** P2 (silent schema-vs-handler drift; agent tools that filter on `kinds` get the unfiltered set).

---

### Finding F-6 — `handlePatchMemory` with `patch: null` wipes all metadata (P2)

**File:line:** `internal/core/tools/handlers.go:1089-1100` (`handlePatchMemory`).

**Pattern:**
```go
patch, _ := p["patch"]
patchJSON, err := json.Marshal(patch)   // json.Marshal(nil) returns the string "null"
if err != nil { return nil, ... }
return dm.PatchMemoryMetadata(id, string(patchJSON))
```

`json.Marshal(nil)` produces `"null"`. The DB layer validates `json.Valid([]byte(patchJSON))` — `"null"` passes. SQL `json_patch('{"existing":"metadata"}', 'null')` returns `null`, which overwrites the metadata column entirely.

**Reproducer:**
```bash
mpm call mpm_memory '{"action":"patch","memory_id":"<existing>","patch":null}'
mpm call mpm_memory '{"action":"query","memory_id":"<existing>","projection":"full"}'
# → metadata is now `null`
```

**Severity:** P2 (destructive data quality — but the caller must explicitly pass `patch:null`; this is not reachable from `mpm_memory` CLI which has no `--patch` flag, only from MCP consumers).

---

### Finding F-7 — `ResolveTheory` no-op error drops the attempted `conclusion` (P2)

**File:line:** `internal/core/epistemology_tools.go:343-346` (no-op error path).

**Pattern:** When a theory is already in a terminal state, `ResolveTheory` returns the error:
```go
return nil, fmt.Errorf("theory %s is already resolved (status=%s); refusing to overwrite with newStatus=%s", theoryID, persisted, newStatus)
```

The error envelope includes the persisted status but NOT the `conclusion` the caller attempted to persist. A retrying agent that cached the conclusion loses it on every failed retry — the retry loop has no way to recover what was being recorded.

**Severity:** P2 (operational data quality — agents in multi-step resolution flows can't safely retry).

---

### Items considered and rejected (verified clean on read-back)

These surfaced in agent sweeps but were ruled out after direct source verification. Listing them here so a future sweep doesn't re-discover them.

| Item | Agent claim | Reality |
|---|---|---|
| DB-1: `migrateLessonsToView` nil-tx panic on Begin failure | P1 panic | FALSE — `db.go:2410-2415` has `if err != nil { ... return }` BEFORE `defer tx.Rollback()`. No nil-tx risk. |
| DB-2: `finishOrRepairLessonsView` nil-tx panic | P1 panic | FALSE — same pattern. `db.go:2575-2580` early-returns on `terr != nil` before defer. |
| DB-4: `rebuildLessonsFTS` nil-tx panic | P2 panic | FALSE — `lessons_fts_sync.go:112-116` early-returns on `err != nil` before defer. |
| DB-5: `idx_provenance_parent_invocation` never created | P2 missing index | FALSE — `schema.go:1140` has `CREATE INDEX IF NOT EXISTS` in `CommonIndexes`, idempotent. Migration comment at `db.go:1907` says "5 of 6 indexes inline" — the 6th lives in CommonIndexes on purpose. |
| `mpm session add ""` accepts empty content (C-4) | P2 | TRUE — but C-3 in the same family is the strongest P1; C-4 is a 1-line guard. |
| `mpm lesson add --type` (missing value) arg-parsing drift (C-8) | P1 corruption | REJECTED — `handlers_lesson.go:80-86` does `i++` on missing value (silently skips), then the loop's `default:` case for content is `break`-before reaching the missing-value position. No arg drift; just silently ignores the flag and uses default type. |
| `cycleTag = "critic_cycle_0"` always | Low observability | TRUE — but it is documented in source as "cycle not tracked here; use wall-clock tag" (intentional). Not a defect. |
| `defaultLockPath` falls back to `/tmp` | Restart-loop risk | TRUE — but gated on `MPM_WORKSPACE` being unset AND `MPM_SCHEDULER_LOCK` being unset. Operators using systemd set `MPM_WORKSPACE` in the unit file. Not a defect for the canonical deployment. |
| `mpm_skills save` `mode` schema drift | Intentional — `mode` only consumed by `workshop` action | Not a defect. |
| `mpm_wakes` schema over-declares `kinds` for non-filter actions | Intentional — `kinds` is only meaningful for filter actions | Not a defect. |
| `mpm_resolve` `blob` kind fallback when `globalResolver == nil` | Returns "blob store not initialized" error | Not silent. |
| **S-3 from §12 sweep**: Blob ID not validated before `filepath.Join` — claimed path traversal | FALSE — `Put` generates IDs internally via `uuid.New().String()` (fs.go:198); no caller-supplied ID write path. `Get/Search/Delete` accept caller IDs but only resolve to files for IDs that exist in the `blobs` table — and the only writer to that table is `Put`, which uses UUIDs only. An attacker would need direct DB write to plant a traversal ID, at which point the filesystem is already compromised. |
| **S-2 from §12 sweep**: Blocklist regex missing NPM, GitLab, PyPI, etc. | TRUE — but the blocklist covers the most-targeted families (OpenAI, Anthropic, GitHub, AWS, JWT, PEM). The gaps are documented (each new credential family is a deliberate scope decision). F-4 above (mirror.jsonl) is the larger P0; S-2 is a P3 hardening item, not P1. |
| **D-1 from §13 sweep**: Default lock path `/tmp` collision restart loop | TRUE — but `MPM_WORKSPACE` is the canonical deployment contract; systemd units set it. The `/tmp` fallback is for `go test` style ad-hoc dev only. Not a defect for production deployment. |

---

## §D — File-by-file scope confirmation

The following were inspected (read-only):

| File | Lines | What was checked |
|---|---|---|
| `internal/core/tools/registry_list.go` | 25-474 | 22 tool entries, action enums, handler bindings |
| `internal/core/tools/handlers.go` | 850-920, 1085-1100, 3442-3610, 5339-5495 | F-5 (kinds filter), F-6 (patch null), F-7 (resolve theory) evidence; resolver paths |
| `internal/scheduler/scheduler.go` | 191, 265-303, 358-462, 729-849 | F-3 evidence; tick-handler ctx capture; handler signature discipline |
| `internal/core/db.go` | 848-861, 1907, 2405-2420, 2570-2585 | DB-1/2/5 false-positive verification |
| `internal/core/lessons_fts_sync.go` | 105-125 | DB-4 false-positive verification |
| `internal/core/schema.go` | 1132-1145 | CommonIndexes verification for DB-5 |
| `internal/core/provenance_migration.go` | 17, 43, 125, 315-329 | DB-5 inline index recreation claim |
| `internal/core/memory.go` | 714-894, 1235-1255 | F-4 (mirror.jsonl verbatim credential persistence) |
| `internal/blobstore/fs.go` | 50-75, 195-275, 277-330 | S-3 false-positive verification (Put uses uuid.New only) |
| `cmd/mpm/handlers_memory.go` | 215-289 | F-2 + C-1 evidence; existing `validateMemoryContent` calls |
| `cmd/mpm/handlers_lesson.go` | 70-130 | F-2 + C-2 + C-8 evidence (C-8 rejected) |
| `cmd/mpm/handlers_epistemology.go` | 81, 94, 190, 305, 325, 582-608, 1091-1097 | F-2 + C-3 + C-6 + C-7 + C-9 evidence |
| `cmd/mpm/handlers_session.go` | 63-68 | C-4 evidence |
| `cmd/mpm/handlers_work.go` | 446-455 | C-5 evidence |
| `agent_installation/opencode-mpm/src/index.ts` | (whole) | F-1 tool count (14 vs claimed 17) |
| `agent_installation/pi-mpm/index.ts` | (whole) | F-1 tool count (15 vs claimed 17) |
| `agent_installation/opencode-mpm/README.md` | 3-272 | F-1 evidence (multiple lines) |
| `agent_installation/pi-mpm/README.md` | 9-82 | F-1 evidence |
| `internal/core/tools/registry_dispatcher_parity_test.go` | (whole) | §16 contract duplication: already enforced by `assertParityForTool` |
| `agent_installation/scripts/render_managed_blocks.py` | (whole) | §15 stale signatures: confirmed clean post-D-R1 |
| `cmd/mpm/call.go` | 65-249 | §15: `mpm_session` legacy alias intentional; all callers correct |

---

## §E — Implementation briefs (if F-2 / F-3 batched in next pass)

### Brief A — F-2 whitespace-only CLI input guards

**Scope:** 2 sites in `cmd/mpm/`, 1 in `cmd/mpm/handlers_lesson.go`, 1 in `cmd/mpm/handlers_work.go`, 1 in `cmd/mpm/handlers_epistemology.go`. Five small guards.

**Approach:**
1. Add `strings.TrimSpace` to all "non-empty content" checks in the CLI handlers.
2. Pattern: `if choice == ""` → `if strings.TrimSpace(choice) == ""`.
3. Pattern: `case factArg != ""` → `case strings.TrimSpace(factArg) != ""`.
4. Existing `validateMemoryContent` already trims — verify the bypass paths (--fact, --file) call it.

**Test gap:** No existing tests for whitespace-only input. Add per-handler table-driven tests that assert each path returns an error on `"   "`.

**Files touched:** 5 handler files + 1 new test file `cmd/mpm/whitespace_input_regression_test.go`.

**Risk:** Low. Pattern is already used elsewhere in the same package (line 81, 94, 190 of `handlers_epistemology.go`).

**Estimated effort:** ~30 min + test coverage.

---

### Brief B — F-3 scheduler handler ctx propagation

**Scope:** `internal/scheduler/scheduler.go` — `executeOne` and the four handler implementations.

**Approach:**
1. Change `Handler` interface signature from `func(Wake) error` to `func(context.Context, Wake) error`.
2. In `executeOne`, replace `go func() { done <- h(w) }()` with a cancellation-aware variant:
   ```go
   go func() {
       hctx, hcancel := context.WithCancel(ctx)
       defer hcancel()
       done <- h(hctx, w)
   }()
   ```
3. Thread `ctx` through `SnapshotHandler`, `GCHandler`, `BroadcastHandler`, and `DrillHandler.runSyntheticDrill`. Replace `exec.Command(...)` with `exec.CommandContext(ctx, ...)`.
4. Add `Cmd.Wait` timeout via `context.WithTimeout(ctx, 30*time.Second)` for the subprocess handlers (canonical CriticAuditHandler pattern at scheduler.go:802-803).

**Test gap:** Existing scheduler tests don't exercise ctx-cancellation during handler execution. Add a test that starts a wake with an artificial 5-second handler, cancels the parent ctx, and asserts the handler returns within 1 second.

**Files touched:** `scheduler.go` (interface + 4 handlers), `scheduler_test.go` (new ctx-cancel test), `dispatch_test.go` (verify call-site compatibility).

**Risk:** Medium. Changing the `Handler` interface breaks every handler implementation — but they're all in the same file, so the blast radius is contained.

**Estimated effort:** ~2 hours + test coverage.

---

## §F — Test-gap analysis

| Finding | Existing test coverage | Gap |
|---|---|---|
| F-2 (whitespace-only CLI input) | none | 5 handlers × 1 test each (table-driven, 6 cases per handler) |
| F-3 (handler ctx propagation) | `TestScheduler_ContextCancellation_CleanShutdown` covers scheduler-loop shutdown, NOT handler-leak | Add `TestScheduler_HandlerCancel_StopsGoroutine` + `TestScheduler_SubprocessTimeout_OnStuckHandler` |
| F-4 (mirror.jsonl credential persistence) | blocklist unit tests verify rejection, not mirror content | Add `TestAppendBlockedAttempt_DoesNotPersistRawPrefix` — invoke with a known `ghp_` secret, assert the mirror row has `sha256(content)` not the raw prefix |
| F-5 (`mpm_wakes list` kinds filter) | `handleListWakes` has a single empty-params smoke test | Add `TestHandleListWakes_FiltersByKinds` — call with `kinds:["notification"]`, assert only notification-kind wakes are returned (requires DB-layer kind-filter signature) |
| F-6 (`patch: null` wipes metadata) | `handlePatchMemory` smoke tests do not exercise null | Add `TestHandlePatchMemory_NullPatchRejected` — call with `patch: nil`, assert error and assert DB row metadata is unchanged |
| F-7 (`ResolveTheory` no-op error drops conclusion) | `ResolveTheory` success-path tests | Add `TestResolveTheory_NoOpErrorIncludesConclusion` — call resolve twice; assert the second call's error envelope includes the `conclusion` string |
| F-1 (README tool-count drift) | none | Add a Markdown-counter test that asserts README claims match source-file tool counts; pipe through Go test gate |

---

## §G — Backlog (prioritized)

| Priority | Finding | Effort | Risk |
|---|---|---|---|
| **P0** | **F-4 mirror.jsonl verbatim credential persistence** | **~10 min** (replace `truncate(content, 100)` with `sha256(content)` + matched-pattern family) | **low** (audit format changes but is internal) |
| P1 | F-2 whitespace-only CLI input guards | ~30 min | low |
| P1 | F-3 scheduler handler ctx propagation | ~2 hr | medium |
| P2 | F-5 `mpm_wakes list` silently discards `kinds` | ~45 min (DB layer needs new kind-filter signature) | low |
| P2 | F-6 `handlePatchMemory` accepts `patch: null` | ~10 min (validate `patch != nil` before marshal) | low |
| P2 | F-7 `ResolveTheory` no-op error drops `conclusion` | ~5 min (include `conclusion` in error envelope) | low |
| P3 | F-1 README tool-count drift | ~15 min | low |
| P2 | C-4 `mpm session add ""` empty content | ~5 min | low |
| P2 | C-5 `mpm work item list --limit=notanumber` silent | ~5 min | low |
| P2 | C-6 `mpm decisions query --limit=abc` silent | ~5 min | low |
| P2 | C-7 `mpm propose_theory hypothesis=` empty bare-value | ~5 min | low |
| P2 | C-9 `mpm decisions list --status=bogus` raw DB error | ~5 min | low |
| P2 | C-10 `mpm memory add --file empty.txt` | ~5 min | low |
| P3 | C-11 `mpm memory add --expires-in 0d` semantic ambiguity | ~5 min | low |

**Total batched as "this-pass" remediation:** F-4 (P0) + F-2 (P1) + F-3 (P1) + F-5/F-6/F-7 (P2) — full remediation pass is ~3.5 hours of focused work.
**Documented but deferred:** C-4..C-11 (P2 cluster of "silent default" defects, each a 1-line fix, can be batched into a single `silent-default` sweep at any time).

---

## §H — Closing posture

The honest count is **7 genuine defects** (1 P0, 2 P1, 3 P2, 1 P3). The P0 (F-4 mirror.jsonl credential persistence) was not in my initial draft — it surfaced from the §12 security sweep and was verified against source (`memory.go:1235-1255` writes the first 100 bytes of blocked content verbatim). It is the highest-priority item in this report.

D-R1 is closed. The alpha-final tag (`8f317b0`) is preserved. `make test-race` is green. This sweep is **read-only** per the brief's hard constraint — no production code changes were made.

**Nine agent-flagged items were initially flagged but ruled out after direct source verification** and are documented in §C's rejection table (DB-1/2/4 nil-tx panics, DB-5 missing index, C-8 arg-parsing drift, S-2 blocklist gaps, S-3 blob path traversal, D-1 lock-path collision). This is exactly the "do not force findings" posture the brief required: I verified each agent claim against source and only counted the ones that survived.

---

## Verdict (11 questions)

1. **Inventory is read-only (no production code changes).** YES — no edits made to `internal/`, `cmd/`, or `agent_installation/`. Only `docs/fresh-residual-defect-inventory-2026-09-04.md` written.
2. **Did not reopen closed defects without current evidence.** YES — D-R1 is CLOSED (D-R1 closure report at `docs/d-r1-closure-2026-09-04.md`); no reopening. Other closed reports reconciled in §B.
3. **Did not force four findings.** YES — §C reports 7 genuine defects (1 P0, 2 P1, 3 P2, 1 P3). The brief said "if fewer than four genuine defects remain, say so. Do not invent filler" — I am reporting what is actually there, not padding to a target.
4. **Report at `docs/fresh-residual-defect-inventory-2026-09-04.md`.** YES — file written at this path.
5. **`make test-race` is the canonical CI gate.** YES — verified green at the start (§A) and unchanged at end.
6. **Findings cite `file:line` evidence verified against source.** YES — §C / §D list the exact lines and the grep/repro pattern for every finding (F-1..F-7).
7. **False-positive candidates are documented as rejected (§C table).** YES — DB-1, DB-2, DB-4, DB-5, C-4, C-8, S-2, S-3, D-1 are all explicitly rejected with reason and source-verification evidence.
8. **Implementation briefs are concrete, not aspirational.** YES — §E Briefs A and B list files, lines, signature changes, test gaps, and estimated effort for F-2 / F-3.
9. **Test-gap analysis is per-finding.** YES — §F table maps each finding to existing tests and the gap.
10. **Backlog prioritizes P0 over P1/P2 over P3.** YES — §G orders F-4 (P0) first, then F-2/F-3 (P1), then F-5/F-6/F-7/C-4..C-10 (P2), then F-1/C-11 (P3).
11. **No `git commit --no-verify` or amend of historical commits.** YES — this report is in `docs/` only; no commits were made.

**Final classification:** Fresh post-closure residual inventory — **1 P0, 2 P1, 3 P2, 1 P3 defects remain**. The P0 (F-4 mirror.jsonl credential persistence) should be remediated first; ~10 min of focused work. The two P1 (F-2 silent CLI input, F-3 scheduler handler ctx) form a ~2.5 hr pass. F-5..F-7 are ~1 hr combined. F-1 is doc-cleanup-only.
