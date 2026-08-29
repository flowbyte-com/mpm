# mpm-mcp ↔ Evidence/Confidence Tools — Design

**Date:** 2026-06-19
**Status:** Draft (pending user review)
**Author:** Brainstorming session with user
**Supersedes:** None

## Summary

The native Go MCP server (`cmd/mpm-mcp`) exposes 20 of the 29 tools available in the `mpm call` CLI universal interface. The 9 missing tools are the **Evidence / Confidence** surface added by commits `c90c0c0` (atomic evidence INSERT), `4bd6e38` (evidence store + trigger chain), and `4efdb99` (retrieval_priority/importance/confidence on Memory and Lesson).

This spec adds those 9 tools to the MCP server. To stay consistent with the established "thin wrapper" pattern (the comment at the top of `cmd/mpm-mcp/tools.go` and the `log_to_changelog` precedent in commit `7ae720b`), the inline SQL in the 9 `call*` handlers in `cmd/mpm/call.go` is lifted into new `DatabaseManager` methods in `internal/call_helpers.go`. Both surfaces then route through the same dm methods, eliminating the only remaining CLI/MCP parity gap.

## Goals

- Add 9 evidence/confidence tools to `cmd/mpm-mcp/tools.go`: `add_evidence`, `list_evidence`, `query_confidence_history`, `query_confidence_changes`, `query_confidence_trend`, `query_memory_quality`, `show_confidence`, `recompute_confidence`, `explain_confidence`
- Lift the inline SQL in the matching 9 `call*` handlers into new `dm` methods so both CLI and MCP share one implementation
- Match the existing handler shape: required-arg checks return `mcp.NewToolResultError`; DB errors return `mcp.NewToolResultErrorFromErr`; numeric defaults via `parseNum`
- Keep the `mpm call` payload schemas byte-identical so existing callers and tests do not change
- Keep the "thin wrapper" rule: the MCP tool description comment at the top of `tools.go` explicitly forbids drift; the count of `s.AddTool(...)` calls remains the canonical surface count

## Non-Goals

- New features (no new SQL, no new evidence types, no new confidence algorithms)
- Schema changes (the `evidence` and `confidence_history` tables already exist per `2026-06-16-confidence-evidence-foundation-design.md`)
- `mpm call` payload schema changes
- Tool description prose duplication (the comment forbids it)
- Refactoring of the existing 20 MCP tools
- Updates to `agent-plugins/opencode-mpm-plugin/src/index.ts` or `agent-plugins/openclaw-mpm-plugin/.../index.ts` (those plugins consume `mpm call` and pick up the new tools automatically)
- Documentation of the new tool count in README/CHANGELOG (the `tools.go` comment is the canonical source)

## Context

The native Go MCP server was added in commit `ee98188` and is the Claude Code integration (configured via `.mcp.json` → `./bin/mpm-mcp`). The 2026-06-15 spec described the older Python plugin; that plugin is deprecated and the Go server is the canonical surface. The Go server was last updated by `7ae720b` (added `log_to_changelog`) and `3131e29` (added `route`) — both routed through new dm methods, not inline SQL.

The CLI/MCP parity pattern is enforced by the file-level comment at the top of `cmd/mpm-mcp/tools.go`:

> "The list of s.AddTool(...) calls is the canonical count of the MCP tool surface — do not duplicate that count in prose, it will drift."
>
> "All handlers are thin shims: extract args via type assertion, call a dm method, wrap the result. The dm methods live in internal/call_helpers.go and back both this MCP server and the `mpm call <tool>` CLI (cmd/mpm/call.go)."

The 9 evidence/confidence handlers in `cmd/mpm/call.go` (lines 772–1125) violate this pattern: they open their own `DatabaseManager` via `openCallDM()`, do inline SQL with `QueryTracked`/`QueryRowTracked`, and use the `internal.AddEvidence` / `internal.QueryConfidenceChanges` / `internal.QueryConfidenceTrend` / `internal.QueryMemoryQualityBySource` / `internal.RecomputeConfidence` / `internal.ExplainConfidence` package-level functions directly. There is no `dm` method. Adding MCP handlers that mirror this inline SQL would propagate the parity gap; the right fix is to lift the SQL up first, then add the MCP shims.

## Design

### Architecture

```
Claude Code  ──┐
               │  JSON-RPC (stdio)
               ▼
       cmd/mpm-mcp/tools.go        ← new: 9 s.AddTool(...) + handleX(dm)
               │  thin shim
               ▼
   internal/call_helpers.go        ← new: 9 DatabaseManager methods
               │
               ▼
   internal/evidence.go            ← unchanged: AddEvidence, IsValidEvidenceType, ...
   internal/confidence.go          ← unchanged: RecomputeConfidence, ExplainConfidence, ...
   internal/confidence_query.go    ← unchanged: QueryConfidenceChanges, QueryConfidenceTrend, QueryMemoryQualityBySource
               │
               ▼
       SQLite (mpm.db)
```

### Components

**1. `internal/call_helpers.go` — 9 new `dm` methods**

| Method | Backed by | Returns |
|---|---|---|
| `AddEvidence(EvidenceInput) (float64, error)` | `internal.AddEvidence` | new confidence (0–1) |
| `ListEvidence(artifactID, artifactType string) ([]map[string]interface{}, error)` | new inline SQL on `evidence` table | rows |
| `QueryConfidenceHistory(artifactID, artifactType string, limit int) ([]map[string]interface{}, error)` | new inline SQL on `confidence_history` | timeline rows |
| `QueryConfidenceChanges(filter internal.ConfidenceChangesFilter) ([]map[string]interface{}, error)` | `internal.QueryConfidenceChanges` | change events |
| `QueryConfidenceTrend(artifactID, artifactType string, windowDays int) (interface{}, error)` | `internal.QueryConfidenceTrend` | trend struct |
| `QueryMemoryQuality() ([]map[string]interface{}, error)` | `internal.QueryMemoryQualityBySource` | per-creator stats |
| `ShowConfidence(artifactID, artifactType string) (map[string]interface{}, error)` | `dm.QueryConfidenceHistory` + inline `confidence` SELECT | `{current, history}` |
| `RecomputeConfidence(artifactID, artifactType string) (map[string]interface{}, error)` | `internal.RecomputeConfidence` + `dm.ShowConfidence` | `{current, history}` |
| `ExplainConfidence(artifactID, artifactType string) (interface{}, error)` | `internal.ExplainConfidence` | component breakdown |

`EvidenceInput` is a re-export of `internal.EvidenceInput` (the same struct the existing `callAddEvidence` builds inline at lines 805–814). `internal.ConfidenceChangesFilter` is already exported.

`ListEvidence`, `QueryConfidenceHistory` carry over the exact SQL already in `call.go` (lines 850–856 and 900–906 respectively) — no query changes. `ShowConfidence` is composed from `QueryConfidenceHistory` plus a single-row `SELECT confidence` against `artifactTable(artifactType)`.

`AddEvidence` is the validation boundary: it calls `internal.IsValidEvidenceType` (rejects unknown types) and `internal.DefaultStrength` (fills the strength default from the type). Both CLI and MCP callers see the same validation outcome. Validation does not run in the handler — it runs once, in the dm method, so the two surfaces cannot drift.

`EvidenceInput` is a re-export of `internal.EvidenceInput`. `internal.ConfidenceChangesFilter` is already exported.

**1a. `internal/db.go` (or new `internal/artifact_table.go`) — move `artifactTable`**

The `artifactTable(artifactType string) string` helper currently lives in `cmd/mpm/call.go` at line 1128:

```go
func artifactTable(artifactType string) string {
    if artifactType == "lesson" {
        return "lessons"
    }
    return "memories"
}
```

It maps `artifact_type` → SQLite table name. Both `dm.AddEvidence`, `dm.ShowConfidence`, and `dm.RecomputeConfidence` need it, and the new MCP handlers will too. It moves to `internal/` (either `db.go` next to other small helpers, or a new file) so both surfaces can import it. No behavior change.

**2. `cmd/mpm/call.go` — 9 handler refactors**

Each `call*` handler is reduced to: open dm, call dm method, return result. The `artifactTable(artifactType)` helper moves to `internal/` (see section 1a). The `internal.IsValidEvidenceType` / `DefaultStrength` checks in `callAddEvidence` (lines 781–796) move into the dm method so the same validation runs from both surfaces.

Net diff: `call.go` drops ~250 lines of inline SQL, replaces with one-line dispatches. The `toolRegistry` map and the `callX` symbol names are unchanged.

**3. `cmd/mpm-mcp/tools.go` — 9 new tool specs + handlers**

One `toolX()` spec and one `handleX(dm)` func per tool, registered in `RegisterAllTools`. The arg schemas are byte-identical to the CLI payload keys (`artifact_id`, `artifact_type`, `type`, `source_group`, `strength`, `independence_factor`, `created_by`, `notes`, `since`, `since_seconds_ago`, `limit`, `window_days`). Required flags use `mcp.Required()` exactly like the other tools.

The 9 new `s.AddTool(...)` calls join the existing 20 in `RegisterAllTools` (lines 43–62). No other change to `tools.go`. No change to `main.go`.

### Data Flow

For an `add_evidence` call:

```
MCP client → JSON-RPC: tools/call {name:"add_evidence", arguments:{...}}
         → handleAddEvidence(dm)              [cmd/mpm-mcp/tools.go]
              • args := req.GetArguments()
              • validate artifact_id / type / source_group / created_by
              • call dm.AddEvidence(EvidenceInput{...})
         → dm.AddEvidence                      [internal/call_helpers.go]
              • internal.IsValidEvidenceType
              • internal.AddEvidence            [internal/evidence.go]
              • SELECT confidence FROM <table>
              • return (confidence, nil)
         → jsonResult({success:true, confidence:0.83})
         → JSON-RPC: tools/call result
```

Identical to the `log_to_changelog` flow already documented at the top of `tools.go` (line 11 comment).

### Error Handling

- Missing required arg → `mcp.NewToolResultError("<field> is required")` — matches `handleListEvidence` line 838, `handleQueryConfidenceHistory` line 888, etc.
- Invalid evidence type → surfaced from `dm.AddEvidence` (which calls `internal.IsValidEvidenceType`) as `mpm.NewToolResultErrorFromErr`
- DB read/write error → `mcp.NewToolResultErrorFromErr("<tool> failed", err)`
- Limit clamping (e.g. `limit <= 0` → 50) — match the `if limit <= 0` pattern in `handleSearchReferences` (tools.go line 619)
- Numeric defaults via `parseNum(args["strength"], 0)` etc. — match `handleQueryLongTermMemory` (line 423)

### Testing

**`internal/call_helpers_test.go`** — new test file, table-driven, in-memory DB. Cases:
- `TestAddEvidence_HappyPath` — inserts, reads back new confidence
- `TestAddEvidence_RejectsInvalidType` — returns `IsValidEvidenceType` error
- `TestListEvidence_EmptyArtifact` — returns empty slice, no error
- `TestListEvidence_FiltersByType` — `artifact_type` clause works
- `TestQueryConfidenceHistory_DefaultLimit` — 50 rows max
- `TestQueryConfidenceChanges_SinceSecondsAgo` — time filter
- `TestQueryMemoryQuality_ReturnsPerSource` — group-by works
- `TestShowConfidence_ComposesHistory` — `{current, history:{history:[…]}}` shape
- `TestRecomputeConfidence_TriggersRecompute` — calls `internal.RecomputeConfidence` with `RecomputeReasonManual`
- `TestExplainConfidence_ComponentBreakdown` — non-nil explanation

**Build verification:**
```bash
go build -tags fts5 ./cmd/mpm-mcp
go build -tags fts5 ./cmd/mpm
go test -tags fts5 ./internal/... -run TestCallHelpers
```

**Smoke test:** run `bin/mpm-mcp` with a `tools/list` JSON-RPC request, confirm 29 tool names returned.

## Risks

- **Behavior drift in `callAddEvidence`'s validation** (lines 781–796) — `internal.IsValidEvidenceType` and `internal.DefaultStrength` checks currently run in the handler. They move into the new `dm.AddEvidence` method (the single validation boundary for both surfaces), not into `internal.AddEvidence`. Verify the new dm method returns the same errors the old handler did.
- **Result-shape stability for `ShowConfidence`** — current `callShowConfidence` returns `{current, history:{history:[…]}}` (nested `history` key). The new dm method and both CLI/MCP handlers must preserve this exact shape, or any downstream agent or test that flattens it will break. The `call_helpers_test.go` test asserts this shape.
- **`artifactTable` move** — moving the helper from `cmd/mpm/call.go` to `internal/` touches one import path (`call.go`) and adds two (`call_helpers.go`, `tools.go`). Low risk but worth flagging as a small, focused diff.
- **Tests for the existing 9 `call*` handlers** — none exist in the repo today. The new `internal/call_helpers_test.go` is the first test coverage for this code path; it should not be deferred.

## Out of Scope (Follow-Up)

- Documentation updates — README, CHANGELOG, and OpenClaw plugin `OPENCLAW.md` should mention "29 tools" once, but the comment in `tools.go` forbids counting in prose. Leave for a separate doc-cleanup pass.
- Python plugin removal — `.claude/mpm-mcp/server.py` is still referenced in some places and should be deleted in a follow-up. (The 2026-06-15 spec said the Python plugin is fully covered by the Go server.)
- Updates to `agent-plugins/opencode-mpm-plugin/src/index.ts` — that plugin calls `mpm call` over CLI, so it picks up the new tools automatically once `mpm call` is rebuilt.
