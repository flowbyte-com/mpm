# Architecture Split: MPM Core vs Agent Runtime

**Status:** ✅ Shipped 2026-07-07 (all 4 phases complete)
**Original proposal:** 2026-06-29 (this document, as Proposal)
**Source critique:** `external-review-chatgpt-2026-06-28.md` §5 — "Mixing two jobs"
**Implementation log:** `changelog.md` ("Phase 1-4", "Phase 1 — Module Boundary Rename", "Phase 2 — CoreDB Interface", "Phase 3 — Runtime Isolation", "Phase 4 — Standalone Core Module")
**Security audit that motivated the closure:** `audit.md` (2026-07-07, score 90/100)

---

## Outcome (the architectural state we now live in)

| Before (2026-06-29) | After (2026-07-07) |
|---|---|
| `DatabaseManager` concrete type shared by every caller | `CoreDB` interface (~130 methods) — runtime code depends on the interface, not the implementation |
| `internal/` mixed Core + Runtime | `internal/core/` is the standalone Go module `github.com/flowbyte-com/mpm-core` |
| Background goroutines competed for one `*sql.DB` | `NewSession() (CoreDB, error)` opens independent `*sql.DB` per call site |
| Hard to reuse Core in mpm-agent / OpenClaw / hermes / opencode | Each consumer imports `mpm-core` directly (or via local `replace` directive for development) |
| `go test ./internal/...` started watch / synthesis / idle_dream goroutines | `go test ./internal/core/...` runs Core in isolation |

Downstream consumers per `CLAUDE.md` ("Sibling projects" section):
- `../mpm-agent/` — agent shell, separate Go module
- 3 `agent-plugins/` entries: `hermes`, `openclaw`, `opencode` (LLM/chat integrations calling into mpm)

All four can now depend on `github.com/flowbyte-com/mpm-core` directly. The main `go.mod` keeps `replace github.com/flowbyte-com/mpm-core => ./internal/core` for local development — single-clone, single-build workflow until the module publishes.

---

## What Shipped (was "Migration Phases")

### Phase 1 — Module Boundary ✅
- `internal/` → `internal/core/`
- 7 subpackages moved (`config`, `tools`, `synth`, `usererror`, `logging`, `mpmcli`, `seed`)
- Import paths rewritten project-wide
- **No behavior change.** Pure restructuring.

### Phase 2 — DatabaseManager split ✅
- `CoreDB` interface defined in `internal/core/core.go`
- `var _ CoreDB = (*DatabaseManager)(nil)` compile-time assertion locks the implementation
- `WebServer.db`, `getDB()`, `openCallDM()`, all 60+ tool handlers, `MemoryStore.DM` switched to `CoreDB`
- Exported `AdmitResult` / `AdmitChainEntry` (were unexported `admitResult` / `admitChainEntry`)
- Tests can now mock `CoreDB` without booting a real SQLite

### Phase 3 — Runtime Isolation ✅
- `NewSession() (CoreDB, error)` added to `CoreDB` interface
- Implementation opens independent `*sql.DB` + inits schema + attaches shared DB
- Fire-and-forget synthesis goroutines use `dm.NewSession()` instead of `mpminternal.NewDatabaseManager("")`
- `AutoSynthesize`, `DetectNearMiss`, `logWatchdogOp` accept `CoreDB` not `*DatabaseManager`
- Watchdog middleware pulled out of Core (push to a Runtime middleware layer)

### Phase 4 — Standalone Module ✅
- Created `internal/core/go.mod` with `module github.com/flowbyte-com/mpm-core`
- ~70 files updated `mpm/internal/core` → `github.com/flowbyte-com/mpm-core` imports
- Main `go.mod` adds `replace github.com/flowbyte-com/mpm-core => ./internal/core` for local dev
- `Makefile` test target now runs both modules
- `go mod tidy` on both cleaned stale dependencies
- `CLAUDE.md` updated to reflect the new module layout

---

## Success Criteria — Verified

1. ✅ `internal/core/` has zero imports from `cmd/mpm/` or from a Runtime package
2. ✅ All Runtime components depend on `CoreDB` interface, not `DatabaseManager`
3. ✅ `go test ./internal/core/...` runs without starting watch, synthesis, or idle_dream goroutines
4. ✅ A new consumer (e.g., a minimal CLI agent) can import `internal/core` (or `github.com/flowbyte-com/mpm-core`) and call `SaveMemory` without importing `cmd/mpm`

---

## Risks Mitigated (vs. the original risk table)

| Risk (originally listed) | How it played out |
|---|---|
| Refactoring breaks existing tests | Phase 1 was pure rename + imports; each phase was tested in isolation; full suite passes with `-race` after Phase 4 |
| Connection ownership becomes ambiguous | `CoreDB.NewSession()` is the typed surface; the existing `sqlopen_owner_test.go` static-analysis invariant now has a typed equivalent in the interface |
| Performance regression from interface dispatch | Methods are coarse-grained (not per-row); dispatch cost is noise at this scale |
| Feature creep — the split never finishes | All four phases shipped in one morning; no partial state to reconcile |

---

## Retrospective — Why This Paid Off

The split earned its keep on the same day it shipped. The 2026-07-07 security audit (`audit.md`, score 90/100) was run against Core in isolation — `go test ./internal/core/...` exercises the scanner, the schema, the search, the lessons, the lessons validation, all without Runtime goroutines firing. That's directly why the audit could close 15 findings in a single morning: the audit surface was bounded by the split.

Secondary payoff: when `mpm-core` publishes, downstream consumers (`mpm-agent`, OpenClaw plugin, `hermes`, `opencode`) drop the local `replace` directive and import the published module — zero URL churn, zero version-bump migration.

---

## Source

External review that prompted the proposal: `external-review-chatgpt-2026-06-28.md` §5 — *"The binary mixes two distinct responsibilities under one `DatabaseManager`. A mode bug can corrupt memories, the Runtime's goroutines compete for the same shared connection, and Core is hard to reuse outside MPM."* Each of those complaints is now structurally addressed.
