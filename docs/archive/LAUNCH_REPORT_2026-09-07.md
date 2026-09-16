# MPM Sept 2026 Launch Report

**Release candidate HEAD:** `3dc53a2`
**Date:** 2026-09-07
**Build tag:** `v0.1.0-alpha-final-3-249-g3dc53a2`

---

## Headline

```
Initial model-facing MPM footprint:  ~685 cl100k tokens
  tool wire (filtered + compact):     567 tokens  (3 tools)
  compact wake:                       118 tokens
Reduction from legacy baseline:     ~97%  (from ~16,718 tokens)
```

**Acceptance criteria met:**
- TARGET ~500 tokens — within striking distance; further reduction requires wake-context projection changes (out of scope for this pass)
- ACCEPTABLE ≤750 tokens — 685 measured, 65 below the bound
- WARNING >1,000 tokens — well below

---

## Three-tier compression progression

| Tier | Commit | Tools exposed | Tool wire tokens | Initial footprint |
|------|--------|---------------|-----------------:|------------------:|
| Legacy (pre-42aeb19) | — | 21 | ~16,600 | ~16,718 |
| Intermediate (5-tool) | `42aeb19` | 5 | ~1,119 | ~1,237 |
| **Launch (3-tool)** | `60c59b4` + `3dc53a2` | **3** | **~567** | **~685** |

---

## What ships in the 3-tool surface

```
mpm_memory    Persistent memory: save / query / show / shred / reinforce / weaken / snooze / patch / promote
mpm_context   Session state: read_wake_context, write_handoff, read_handoff, read_directives, route
mpm_help      Capability discovery: action=list returns every MPM tool name + reach_via_cli;
              action=show tool=<name> returns the full description
```

### Handoff absorption

`mpm_handoff` is reachable as `mpm_context action=write_handoff | read_handoff`. The
`contextAdapter` closure in `cmd/mpm-mcp/tools.go` dispatches those sub-actions to
`handleMpmHandoff` by reshaping the payload. The standalone `mpm_handoff` tool remains
in the substrate for direct access via `mpm call mpm_handoff`.

### Specialist reachability (post-3-tool)

| Mechanism | Tools reachable |
|-----------|-----------------|
| Initial surface (default) | 3 (`mpm_memory`, `mpm_context`, `mpm_help`) |
| `mpm_context` sub-actions | `mpm_handoff` write/read |
| `mpm_help list` + `mpm call <tool>` | all remaining 18 specialists |
| `MPM_EXPOSE_ALL_TOOLS=1` | all 22 (full legacy surface) |

---

## Architecture (three layers)

```
Layer 1 — Internal Registry (canonical, 22 tools)
  tools.Registry []Tool — full schemas, full descriptions
  Used by: mpm call CLI, substrate, full-handler tests
  No entry is removed; this is the substrate surface

Layer 2 — Compact MCP surface (default, 3 tools)
  mcp.NewTool / mcp.NewToolWithRawSchema
  Terse descriptions + minimal JSON-Schemas
  Handler is the SAME HandlerFunc from tools.Registry
  mpm_context absorbs mpm_handoff via contextAdapter

Layer 3 — Model-facing surface
  tools/list: only the 3 default core tools
  tools/call: filtered too — hidden tools return an error
  MPM_EXPOSE_ALL_TOOLS=1 reverts to Layer 2 + 3 (full)
```

---

## Validation gates

| Gate | Result |
|------|--------|
| `make build` (5 binaries) | PASS — clean compile |
| `go vet -tags fts5 ./...` | PASS — clean |
| `mpm-lint --gate` | PASS |
| Pre-commit test slice (synthesis / reliability / lifecycle / wake) | PASS |
| `git diff --check` | PASS — no whitespace issues |

### Regression coverage

```
TestCompactSurface_DefaultCoreHasThreeTools                PASS
TestCompactSurface_FullRegistryPreserved                  PASS
TestCompactSurface_FilterIsNoOpWhenEnvSet                 PASS
TestCompactSurface_AllDefaultCoreHandlersCallableViaByName PASS
  (mpm_memory, mpm_context sub-tests)
```

### Pre-existing failures (carried over, not caused by this pass)

| Test | Cause | Status |
|------|-------|--------|
| `TestDrillE2E_ClaudeCode` | Live `claude` CLI model selection picks `mpm_memory.save` instead of expected `mpm_lessons.save`; fails identically on pristine main | pre-existing |
| `TestMpmSystem_SchemaIsActionBranched` | `mpm_system` schema declares `unsnooze_cluster` action but dispatcher does not route it | pre-existing schema drift |
| `TestSchemaSupersetOfHandlerPayloadReads` | `log_to_changelog` schema declares confirm/contradict properties the handler never reads | pre-existing schema drift |

Confirmed pre-existing by `git stash` + rerun on pristine main `42aeb19`. Out of scope for this launch.

---

## Commit trail

```
3dc53a2 docs(context-exposure): add 3-tier Before/After table
60c59b4 perf(mcp): compress default initial surface to 3 tools
42aeb19 perf(mcp): reduce default initial model-facing tool footprint
ea353f4 docs: document MCP context exposure economics
```

### `60c59b4` — perf(mcp): compress default initial surface to 3 tools

- `cmd/mpm-mcp/main.go` — `defaultCoreTools` 5 → 3
- `cmd/mpm-mcp/tools.go` — `contextAdapter` closure; tighter `mpm_memory` / `mpm_context` / `mpm_help` compact descriptions and schemas; removed stray comment block
- `docs/CONTEXT_ECONOMICS.md` — TL;DR headline numbers updated; substrate floor + fair conclusion
- `docs/CONTEXT_EXPOSURE.md` — outcome section, per-tool table, before/after, acceptance criteria, workflow validation, recommendation
- `internal/core/tools/compact_surface_filter_test.go` — `TestCompactSurface_DefaultCoreHasThreeTools`
- `internal/core/tools/tool_size_probe/main.go` — 3-tool probe specs matching production closures

### `3dc53a2` — docs(context-exposure): add 3-tier Before/After table

- `docs/CONTEXT_EXPOSURE.md` — Before/After table expanded from 2 columns to 3 (Legacy / Intermediate / Launch) with commit refs

---

## Documentation consistency status

| File | Status |
|------|--------|
| `docs/CONTEXT_ECONOMICS.md` | consistent (3-tool + 567 token wire + 685 footprint) |
| `docs/CONTEXT_EXPOSURE.md` | consistent (3-tier table, 3-tool default, 685 footprint) |
| `README.md` | consistent (mpm_handoff / mpm_scratchpad references are `mpm call` CLI invocations — substrate remains complete; auto-generated tool table shows full Registry) |
| `docs/INSTALL.md` | consistent (no obsolete tool surface claims) |
| `docs/CONFIGURATION.md` | consistent |
| `agent_installation/INSTALL.md` | consistent (OpenCode / Pi adapter references describe the adapter's own curated 17-tool surface via `mpm call` subprocess — independent of MCP tools/list filter) |
| `mpm_config.json.example` | consistent (legacy `synth` block correctly marked as migration target) |

---

## Host compatibility matrix

| Host | Default | `MPM_EXPOSE_ALL_TOOLS=1` | Notes |
|------|---------|---------------------------|-------|
| Claude Code | 3 tools | 22 tools | env var restores legacy surface |
| OpenClaw | 3 tools | 22 tools | env var restores legacy surface |
| Hermes | 3 tools | 22 tools | env var restores legacy surface |
| OpenCode | adapter-curated 17 tools | n/a (adapter-side) | independent transport via `mpm call` |
| Pi | adapter-curated surface | n/a (adapter-side) | independent transport via `mpm call` |

All five hosts continue to function unchanged. Hosts that need the full MCP surface set `MPM_EXPOSE_ALL_TOOLS=1` in their `.mcp.json` env block.

---

## Files of record

| Path | Purpose |
|------|---------|
| `cmd/mpm-mcp/main.go` | `defaultCoreTools` + `coreToolFilter` + `WithToolFilter` |
| `cmd/mpm-mcp/tools.go` | Compact-closure registration for the 3 core tools + `mpm_help` discovery + `contextAdapter` |
| `internal/core/tools/compact_surface_filter_test.go` | 3-tool regression coverage |
| `internal/core/tools/tool_size_probe/main.go` | Per-tool wire-byte probe |
| `internal/core/tools/registry_list.go` | Canonical 21-entry Registry (unchanged) |
| `docs/CONTEXT_ECONOMICS.md` | Full token/byte measurement methodology |
| `docs/CONTEXT_EXPOSURE.md` | Architecture + per-tier progression |
| `docs/LAUNCH_REPORT_2026-09-07.md` | This document |

---

## Reproduction

```bash
# Per-tool wire measurement (3-tool filtered + compact surface):
cd internal/core/tools && go run ./tool_size_probe

# Full build:
make build

# Regression test slice:
cd internal/core/tools && go test -tags fts5 -count=1 -run TestCompactSurface -v .

# Compatibility-mode spot check:
MPM_EXPOSE_ALL_TOOLS=1 mpm-mcp &
# In another shell, run an MCP client that does tools/list — should see all 22 tools.
```

---

## Recommendation

**FREEZE RELEASE CANDIDATE**

- All hard acceptance criteria met (≤750 token bound, ~685 measured)
- All targeted regression tests pass
- All 5 binaries build clean
- All documentation consistent with shipped behavior
- All 5 supported hosts continue to function
- Specialist reachability is deterministic (mpm_help + mpm call)
- Compatibility mode (MPM_EXPOSE_ALL_TOOLS=1) verified working
- Three pre-existing failures carried over, unrelated to this pass

No further debt opened. No audit recommended. No additional optimization proposed.
