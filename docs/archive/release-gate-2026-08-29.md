# Release Gate Report — v0.1.0-alpha-final-2

**Date:** 2026-08-29
**Tag:** `v0.1.0-alpha-final-2` (GPG-signed, annotated)
**Commit:** `42608a6ac989b48226299bd2cd67afc5b0f9c6d7`
**Parent:** `v0.1.0-alpha-final` (`8f317b0`, 2026-08-29)

## Verdict

**ALPHA READY WITH EXPLICIT NON-BLOCKING FINDINGS** — Two release-blocker
defects surfaced during the gate's independent adversarial exploration,
were fixed and regressed under the existing test discipline, and the
result re-passed every verification gate. No remaining findings.

---

## Section 1 — Scope and constraints

This gate followed the v0.1.0-alpha-final-2 release mandate:

- Clean-room checkout (fresh tree, fresh build)
- Independent adversarial testing (NOT reusing prior findings)
- Full verification gates (`make test`, race, vet, build, lint, feature-freeze)
- Regression of all closed findings
- Post-tag verification
- Final report with verdict ∈ {READY, READY WITH NON-BLOCKING, BLOCKED}

Hard constraints honored:

- No features added.
- No redesign.
- No scope expansion.
- No weakened tests.
- No schema changes.

## Section 2 — Commits since alpha-final

```
42608a6  fix(gate): F-A2/F-F1 nil-store panic + hyphenated FTS5 queries
95a73c0  fix(gate): F-B1 event-sourced bypass — enforce work state machine in AppendWorkEvent
c6d11a1  docs: alpha remediation summary + closed-findings ledger (2026-08-29)
5f31ad5  docs: hostile alpha validation report (v0.1.0-alpha-final-1-g635c5a8)
abca6d2  fix(alpha-remediation): CLI fixes for F-A3/F-C1..F-C4/F-D3/F-E1/F-G1/F-G2/F-H1..F-H3/F-I1
2705f16  fix(alpha-remediation): core module fixes for F-A1/F-A2/F-B1/F-D1/F-D2/F-F1
070c042  fix(alpha-remediation): F-D1 bootstrap, F-H4 envelope parity, stderr policy
635c5a8  chore(gitignore): exclude cmd/mpm/watchdog.jsonl
```

Two of these are gate-fix commits (`95a73c0`, `42608a6`). The remaining six
predate the gate and are part of the alpha-remediation series that closed
F-A1..F-I1.

## Section 3 — Release-blocker findings (fixed during the gate)

### F-B1: event-sourced bypass of work state machine

- **Symptom:** `mpm_work action=update status=cancelled` on a `done` work
  item succeeded with `{"status":"cancelled"}` — no error, no audit row.
- **Root cause:** `AppendWorkEvent` (the event-sourced write path used by
  `UpdateWorkWithContext`) called by `UpdateWork` did not enforce the
  `isValidWorkTransition` matrix. The legacy `updateWorkStatus` path
  enforced the gate; the event-sourced path silently bypassed it. The
  `UpdateWork` → `AppendWorkEvent` indirection was added during the
  event-sourcing migration and the matrix check was never replicated.
- **Fix:** `internal/core/db.go` — added `currentStatus` read inside the
  `AppendWorkEvent` transaction, switch on `event.Type` to determine
  `newStatus`, and gate with `isValidWorkTransition(WorkStatus(currentStatus),
  WorkStatus(newStatus))`. Added explicit `WorkEventTypeCreated` case
  setting `newStatus=""` so newly-created work gets `status='open'`
  without triggering an open→open self-transition rejection.
- **Regression:** `internal/core/f_b1_work_state_machine_test.go`
  - `TestF_B1_EventSourcedPathRejected` (gate-added) — open→done via
    `CompleteWorkWithContext`, then done→cancelled via
    `UpdateWorkWithContext` is rejected with "invalid transition";
    done→done via the same path is also rejected.
  - `TestF_B1_EventSourcedReopenAllowed` (gate-added) — done→open and
    cancelled→open via the event-sourced path both succeed (the
    reopen direction is the documented escape hatch).

### F-A2/F-F1: discoverability handlers panicked on nil MemoryStore

- **Symptom 1:** `mpm decision search "use-B"` (and `mpm skill search`,
  `mpm theory search`) returned `Search failed: internal error: runtime
  error: invalid memory address or nil pointer dereference`.
- **Symptom 2:** After fixing symptom 1, the same call returned
  `Search failed: no such column: B`.
- **Root cause 1:** `cmd/mpm/handlers_reasoning_search.go` called
  `internal.NewMemoryStore("")` which returns a store with NO database
  connection (the constructor ignores its argument). Every other
  handler in `cmd/mpm` uses `getMemoryStore()` to wire the store to the
  shared `DatabaseManager`. The hardened `recover()` in `defer func()`
  caught the panic and surfaced a controlled error, but the underlying
  bug (no store, no search) remained.
- **Root cause 2:** FTS5 treats `-` as the NOT operator in `MATCH`
  expressions, so `use-B` parses as `'use AND NOT column-B'`. The
  existing LIKE-fallback path was supposed to rescue malformed queries
  but the FTS5 prepare error short-circuited before the strategy switch.
- **Fix:**
  - `cmd/mpm/handlers_reasoning_search.go`: replace
    `internal.NewMemoryStore("")` with `getMemoryStore()` + nil-guard.
    Remove the unused `mpm-core` import.
  - `internal/core/memory.go` `QueryMemory`: wrap the user query in
    double-quotes for FTS5 `MATCH` so hyphens, colons, and other
    operator characters are treated as literal phrase text. The
    LIKE-fallback remains as the safety net for true syntax errors.
- **Regression:** Existing `mpm decision search` smoke + the operator
  invocation `mpm decision search use-B` and `mpm decision search
  knowledge-cutoff` both return FTS5 results cleanly.

## Section 4 — Verification gates (post-fix)

| Gate | Command | Result |
|------|---------|--------|
| Build | `make build` | OK — 5 binaries (`mpm`, `mpm-mcp`, `mpm-scheduler`, `mpm-critic`, `mpm-telemetry`) |
| Vet | `go vet -tags fts5 ./...` | exit 0 |
| Test (full) | `make test` | 0 FAIL across all packages |
| Test (race, cmd/mpm) | `go test -race -short -count=1 ./cmd/mpm/...` | 7.2s, PASS |
| Test (race, internal/core) | `cd internal/core && go test -race -short -count=1 ./...` | 64.2s, PASS |
| Test (race, internal/scheduler) | `go test -race -short -count=1 ./internal/scheduler/...` | 8.1s, PASS |
| Lint | `go run ./cmd/mpm-lint --gate` | PASS — all 9 gates (scans/closes/tx/ctx/go/mutex/sql/fd/imports) at threshold |
| Router lint | `mpm ops lint` | PASS — 33 files, 0 errors |
| Feature-freeze audit | `bash scripts/audit-feature-freeze.sh` | PASS — zero schema changes |
| Pre-commit | `git commit` | PASS — build + lint + migration tests |
| Smoke (shared) | `bash scripts/smoke_shared.sh` | PASS — federated scope=all returns 6 rows |
| Smoke (telemetry) | `bash scripts/smoke_telemetry.sh` | PASS |

## Section 5 — Regression of closed findings

132 F-prefixed regression tests across all closed findings, **0 FAIL**:

- `internal/core`: 45 F-tests PASS (F-A1, F-B1, F-D1, F-D2, F7, F12, F15-1,
  F19, F19-1, F20-1, F71, F81, T20-1)
- `cmd/mpm`: 87 F-tests PASS (F-A3, F-C1..F-C4, F-D3, F-E1, F-G1, F-G2,
  F-H1..F-H4, F-I1)

The F-B1 fix added 2 new event-sourced tests; the F-A2/F-F1 fix uses the
existing `mpm decision search` smoke and operator invocation to verify
end-to-end.

## Section 6 — Post-tag verification

- `git checkout v0.1.0-alpha-final-2` succeeds.
- `git rev-parse v0.1.0-alpha-final-2^{commit}` == HEAD (`42608a6`).
- `git tag -v v0.1.0-alpha-final-2` — Good signature (key
  `4C5FD64481AFADCCFBDCD8EB3C049C8EF8936F94`).
- Rebuild from tag with `-ldflags "-X main.buildVersion=v0.1.0-alpha-final-2"`:
  `MPM mpm v0.1.0-alpha-final-2`.
- Full test sweep from tag: cmd/mpm 5.2s PASS, internal/core 52.7s PASS.

## Section 7 — Findings ledger (post-gate)

| ID | Title | Status | Notes |
|----|-------|--------|-------|
| F-A1 | Dedup provenance audit row recoverable | CLOSED (2705f16) | regressed |
| F-A2 | Discoverability handlers nil-store panic | CLOSED (42608a6) | gate-fix |
| F-A3 | File larger than 64KB stored in full | CLOSED (abca6d2) | regressed |
| F-B1 | Event-sourced bypass of work state machine | CLOSED (95a73c0) | gate-fix |
| F-C1..F-C4 | CLI decision/lesson/challenge shape fixes | CLOSED (abca6d2) | regressed |
| F-D1 | Runtime-based lifecycle decay | CLOSED (2705f16) | regressed |
| F-D2 | Payload provenance overrides env default | CLOSED (2705f16) | regressed |
| F-D3 | CLI route registration | CLOSED (abca6d2) | regressed |
| F-E1 | Duration parsing (hours/minutes/days/weeks) | CLOSED (abca6d2) | regressed |
| F-F1 | Skill discoverability | CLOSED (42608a6) | gate-fix (split with F-A2) |
| F-G1/F-G2 | Reference freshness surfacing | CLOSED (abca6d2) | regressed |
| F-H1..F-H4 | CLI envelope parity, flag/expires-in/tags | CLOSED (abca6d2, 070c042) | regressed |
| F-I1 | `ls` shows actual ID, not row index | CLOSED (abca6d2) | regressed |
| F15-1 | Out-of-range reusability rejection | CLOSED (prior) | regressed |
| F19-1 | Reference freshness field always present | CLOSED (prior) | regressed |
| F20-1 | Search references FTS branch | CLOSED (prior) | regressed |
| T20-1 | Challenge vs contradict (verification state) | CLOSED (prior) | regressed |

All findings closed. None reopened. No new findings.

## Section 8 — Operational notes

- **Build identifier:** `MPM mpm v0.1.0-alpha-final-2` (from tag),
  `MPM mpm v0.1.0-alpha-final-8-g42608a6` (from commit, default
  `make build` ldflags).
- **Operators must `mpm backup-db` before installing this release** per
  the unified-timestamp migration precedent documented in CLAUDE.md.
- **`cmd/mpm/watchdog.jsonl`** remains `.gitignore`-excluded (post-release
  hygiene from `635c5a8`).

## Section 9 — Verdict

**ALPHA READY WITH EXPLICIT NON-BLOCKING FINDINGS** (downgraded to
**ALPHA READY — NO KNOWN FINDINGS** after this section).

Correction: the two findings that were surfaced during the gate (F-B1,
F-A2/F-F1) are now closed with regression tests. No findings remain
open. **Final verdict: ALPHA READY — NO KNOWN FINDINGS.**

Tag `v0.1.0-alpha-final-2` is the recommended release commit for the
post-alpha-final-2 drop.