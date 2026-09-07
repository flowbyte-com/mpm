# MCP Tool Exposure Economics

> Investigation of whether MPM can materially reduce its ~16.6K-token
> fixed tool-schema cost without degrading discoverability, correctness,
> or normal agent workflows.

This document accompanies `docs/CONTEXT_ECONOMICS.md`. That document
measures what MPM currently costs in model-facing context; this one
asks whether **any of that cost should be reduced** for the alpha launch
and what the trade-off looks like.

The answer below is **the architecture was implemented**. The
production numbers come from the implementation commit. The earlier
"do not optimize yet" recommendation is preserved in git history
(commit `ea353f4`) for context.

---

## 1. Outcome (Sept 2026 launch polish)

```
Default initial surface (MPM_EXPOSE_ALL_TOOLS unset):
  5 tools (filtered from 22)
  desc+schema bytes (raw):     3,456
  JSON wire payload:           4,477 bytes
  cl100k_base tokens (est):     ~1,119

Full surface (MPM_EXPOSE_ALL_TOOLS=1) — compatibility mode:
  22 tools (21 Registry + 1 mpm_help closure)
  desc+schema bytes (raw):     ~43,127
  JSON wire payload:           ~66,359 bytes
  cl100k_base tokens (est):     ~16,600

Reduction from baseline to default initial surface: ~93%
```

**Target**: ≤ 2,000 tokens initial footprint, target ≈ 1,500.
**Achieved**: ~1,119 tokens initial footprint (well under the upper
bound; ~25% under the target). **Hard acceptance criterion met.**

### Per-tool wire (default core, filtered + compact)

| Tool           | desc bytes | schema bytes | total | ~tokens |
| -------------- | ---------: | -----------: | ----: | ------: |
| `mpm_memory`   |        398 |          522 |   920 |     230 |
| `mpm_context`  |        340 |          408 |   748 |     187 |
| `mpm_handoff`  |        289 |          364 |   653 |     163 |
| `mpm_scratchpad`|       311 |          277 |   588 |     147 |
| `mpm_help`     |        343 |          204 |   547 |     137 |
| **TOTAL**      |    **1,681** |      **1,775** | **3,456** | **~864** |

`mpm_help` is the capability-discovery tool — it returns the full
catalogue with terse one-liners when the agent asks, and routes to
the host-shell `mpm call <tool>` escape hatch for specialists not in
the default initial surface.

---

## 2. Architecture (implemented)

The launch-block target required an architecture that:

1. Keeps the **full internal Registry** unchanged (22 tools) so the
   CLI `mpm call <tool>` and the substrate remain complete.
2. Exposes a **compact initial surface** at `tools/list` time so the
   model sees a small, decision-relevant subset.
3. Provides a **deterministic discovery path** for specialists that
   does not require host-side `listChanged` support (no host in the
   supported matrix consumes `listChanged` today).

### Three layers

```
┌─────────────────────────────────────────────────────────────┐
│ Layer 1 — Internal Registry (canonical, 22 tools)            │
│   tools.Registry []Tool — full schemas, full descriptions   │
│   Used by: `mpm call` CLI, substrate, full-handler tests.    │
│   No entry is removed; this is the substrate surface.        │
└─────────────────────────────────────────────────────────────┘
                         │
                         │ Closure wiring in
                         │ cmd/mpm-mcp/tools.go
                         ▼
┌─────────────────────────────────────────────────────────────┐
│ Layer 2 — Compact MCP surface (default, 5 tools)            │
│   mcp.NewToolWithRawSchema / mcp.NewTool                   │
│   Terse descriptions + minimal JSON-Schemas.                 │
│   Handler is the SAME HandlerFunc from tools.Registry —     │
│   the closure binds tools.MustByName(...).Handler.            │
└─────────────────────────────────────────────────────────────┘
                         │
                         │ server.WithToolFilter (mcp-go)
                         │ applied at both tools/list
                         │ and tools/call time
                         ▼
┌─────────────────────────────────────────────────────────────┐
│ Layer 3 — Model-facing surface                               │
│   tools/list: only the 5 default core tools.                │
│   tools/call: filtered too — hidden tools return an error.  │
│   MPM_EXPOSE_ALL_TOOLS=1 reverts to Layer 2 + 3 (full).      │
└─────────────────────────────────────────────────────────────┘
```

### Discovery: `mpm_help` + `mpm call` escape hatch

The model-facing surface includes `mpm_help` (5th tool). Its
contract:

- `mpm_help action=list` → every registered tool name with a
  terse one-liner, plus per-row `reach_via_cli: "mpm call <tool>
  --payload '...'"` instructions.
- `mpm_help action=show tool=<name>` → full description for one tool.

The model is therefore never without a path to a specialist tool:
either the tool is in the initial surface, or the agent can ask
`mpm_help list` to discover it and `mpm call` to invoke it via the
host shell. Both paths are already part of the documented MPM
behavioural contract.

### Compatibility mode

```bash
MPM_EXPOSE_ALL_TOOLS=1 mpm-mcp
```

Restores the legacy 22-tool surface. Every host integration that
needs the full surface (e.g., a custom OpenClaw adapter) can set
this env var. The default is unchanged — operators who don't set
the env get the compact surface.

---

## 3. Hard acceptance criteria — status

```
[x] Initial model-facing MPM footprint ≤ 2,000 tokens
    → ~1,119 tokens measured (1,481 below the bound)
[x] Target approximately 1,500 tokens
    → 381 under target
[x] Full internal MPM capability surface remains intact
    → tools.Registry has 22 entries (21 + mpm_help)
[x] Core workflows remain straightforward
    → mpm_memory, mpm_context, mpm_handoff, mpm_scratchpad
      cover wake / persist / handoff / working state
[x] Specialist capabilities remain discoverable
    → mpm_help list returns every tool with reach_via_cli
[x] Specialist capabilities remain callable
    → mpm call <tool> --payload '<json>' works for any of the 22
      tools, regardless of exposure policy
[x] No important parameter/safety semantics removed
    → compact schemas keep type, enum, required, validation,
      action-level parameter names; full schemas remain in
      tools.Registry for the CLI / substrate
[x] Existing host integrations remain functional
    → MPM_EXPOSE_ALL_TOOLS=1 restores legacy surface
[x] No silent capability loss
    → every tool is reachable; documented in mpm_help list
[x] Context Economics contains measured before/after figures
    → see docs/CONTEXT_ECONOMICS.md and this document
[x] Representative workflow costs are documented
    → see Section 5 below
```

---

## 4. Before / after

| Measure                      |  Before | After |
| ---------------------------- | ------: | ----: |
| Tools exposed initially      |      21 |     5 |
| `tools/list` bytes (raw)     |  42,580 | 3,456 |
| `tools/list` bytes (wire)    |  66,359 | 4,477 |
| `tools/list` tokens (~cl100k)| ~16,600 | ~1,119 |
| Initial MPM footprint (5 core + wake) | ~16,718 | ~1,237 |
| Internal Registry (unchanged)|      21 |    22 |
| Specialists reachable via CLI|      21 |    21 |
| MPM_EXPOSE_ALL_TOOLS=1 fallback |   n/a | works |

---

## 5. Workflow validation

### Core workflow

```text
1. wake / context    → mpm_context read_wake_context
2. remember / persist → mpm_memory save
3. recall / query    → mpm_memory query (projection=summary)
4. show              → mpm_memory show
5. handoff at close  → mpm_handoff write
```

All 5 steps use the **default initial surface only**. No
`mpm_help` call, no `mpm call` subprocess. Total tool catalogue
cost: ~1,119 tokens. Total initial MPM footprint (core + compact
wake): ~1,237 tokens.

### Epistemic workflow

```text
1. discover specialist tools      → mpm_help action=list
2. inspect decision schema       → mpm_help action=show tool=mpm_decisions
3. invoke decision tool          → mpm call mpm_decisions --payload '...'
                                   (or set MPM_EXPOSE_ALL_TOOLS=1 and
                                    invoke mpm_decisions directly)
4. read decision by id           → mpm call mpm_decisions --payload
                                    '{"action":"show","params":{"id":"..."}}'
5. record lesson                 → mpm call mpm_lessons --payload '...'
```

Discovery cost: one `mpm_help list` round-trip (~600 bytes) +
one `mpm_help show` round-trip (~200 bytes). **Per-session total
discovery cost: under 1 KB.** Workflow requires the host to support
shell execution for `mpm call`; every supported host in the matrix
already does.

### Work workflow

```text
1. discover         → mpm_help action=list  (mpm_work is listed)
2. create work item → mpm call mpm_work --payload '{"action":"create",...}'
3. complete + note  → mpm call mpm_work --payload '{"action":"note",...}'
4. complete         → mpm call mpm_work --payload '{"action":"complete",...}'
```

Same pattern: one `mpm_help list` for discovery, then `mpm call`
for invocations. The model never needs the full `mpm_work` schema
in its context — it discovers the name, asks `mpm_help show
mpm_work` if needed, then invokes via CLI.

### Specialist workflow (e.g., evidence + confidence)

```text
1. discover evidence + confidence  → mpm_help list (both shown)
2. add evidence                    → mpm call mpm_evidence --payload '...'
3. inspect confidence              → mpm call mpm_confidence --payload '...'
4. diagnose retrieval              → mpm call mpm_retrieval_diagnose --payload '...'
```

### Multi-step representative workflow

```
wake                 (mpm_context read_wake_context)
save a fact          (mpm_memory save)
discover work tools  (mpm_help list — 1 round-trip)
create work item     (mpm call mpm_work create)
hypothesis record    (mpm call mpm_theories propose)
evidence add         (mpm call mpm_evidence add)
handoff at close     (mpm_handoff write)
```

Total discovery overhead for the multi-step workflow: **one
`mpm_help list` round-trip (~600 bytes JSON)**. Per-session cost
of moving from the 5-tool initial surface to specialist capability
is bounded and known.

---

## 6. Schema integrity

The compact schemas keep everything required for correct
invocation:

| Field            | In compact schema? | Notes |
| ---------------- | :----------------: | ----- |
| `action` (enum)  | yes — full         | required, with all valid values |
| `params` (object)| yes — minimal      | common keys enumerated; full schema referenced via `mpm_help show` |
| `required` array | yes                | preserves validation contract |
| `enum` on values | yes                | preserves valid input space |
| per-property `description` | dropped | human-readable prose; redundant for tool selection |
| lifecycle safety notes | yes (terse) | "shred is permanent" preserved in mpm_memory compact |
| safety warnings  | yes (terse) | "summary must be non-empty" preserved in mpm_handoff |

The full schemas remain in `tools.Registry` (used by the CLI,
substrate, and the existing test suite). Tests that pin schema
shape against the Registry still pass; the MCP wire is the
compressed projection.

---

## 7. Compatibility

| Host       | Status                                                                                          |
| ---------- | ----------------------------------------------------------------------------------------------- |
| OpenClaw   | PASS — full 22-tool surface via `MPM_EXPOSE_ALL_TOOLS=1`. Default 5-tool surface is the new default. |
| Claude     | PASS — same: default 5-tool surface; opt-in to 22 via env. The mcp.json template does not need to change. |
| Hermes     | PASS — same as Claude / OpenClaw. |
| OpenCode   | PASS — the existing 17-tool adapter-side subset is now redundant (the server-side compact surface is smaller) but continues to work. |
| Pi         | PASS — same as OpenCode. |

Each of these hosts can set `MPM_EXPOSE_ALL_TOOLS=1` in their MCP
config to restore the legacy 22-tool surface. The MCP `initialize`
response advertises `capabilities.tools = {listChanged: false}`,
unchanged from the previous release.

---

## 8. Files

| Path                                              | Purpose                                          |
| ------------------------------------------------- | ------------------------------------------------ |
| `docs/CONTEXT_EXPOSURE.md`                        | This document                                    |
| `docs/CONTEXT_ECONOMICS.md`                       | Updated baseline measurements                    |
| `cmd/mpm-mcp/main.go`                             | `defaultCoreTools` + `coreToolFilter` + `WithToolFilter` |
| `cmd/mpm-mcp/tools.go`                            | Compact-closure registration for the 4 core tools + mpm_help discovery |
| `internal/core/tools/registry.go`                 | `MustByName` helper                              |
| `internal/core/tools/registry_list.go`            | `MustByName` helper                               |
| `internal/core/tools/compact_surface_filter_test.go` | TestCompactSurface_* regression coverage       |
| `internal/core/tools/tool_size_probe/main.go`     | Probe that computes filtered + compact wire sizes |

---

## 9. Reproduction

```bash
# Per-tool wire measurement (filtered + compact surface):
cd internal/core/tools && go run ./tool_size_probe

# Full context-economics re-measurement:
bash scripts/context_economics/measure.sh
python3 scripts/context_economics/count_tokens.py /tmp/mpm-econ-*/raw/summary.json
```

Both harnesses are disposable-workspace safe; they never touch the
operator's real `~/.mpm`.

### Compatibility-mode spot check

```bash
MPM_EXPOSE_ALL_TOOLS=1 mpm-mcp &
# … in another shell, run an MCP client that does tools/list — should
# see all 22 tools.
```

---

## 10. Recommendation

**Ship with optimization.**

- Initial MPM footprint at the model boundary is ~1,119 tokens (vs
  ~16,600 baseline; ~93% reduction). Target was ~1,500 / upper
  bound 2,000. Both met.
- Full internal capability surface remains intact. No tool is
  removed.
- Specialist discoverability is deterministic (mpm_help +
  reach_via_cli).
- Compatibility mode (`MPM_EXPOSE_ALL_TOOLS=1`) restores the
  legacy 22-tool surface for any host that needs it.
- The OpenCode / Pi adapter-side 17-tool subsets continue to work;
  they are now redundant for hosts that don't need the full
  surface — the server-side compact surface is smaller.

The trade is honest: the model sees fewer schemas initially, and
pays a small per-session discovery cost (~1 KB) when it needs a
specialist tool. That trade is well below the savings.
