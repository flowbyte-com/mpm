# MCP Tool Exposure Economics

> Investigation of whether MPM can materially reduce its ~16.6K-token
> fixed tool-schema cost without degrading discoverability, correctness,
> or normal agent workflows.

This document accompanies `docs/CONTEXT_ECONOMICS.md`. That document
measures what MPM currently costs in model-facing context; this one
asks whether **any of that cost should be reduced** for the alpha launch
and what the trade-off looks like.

The answer below is **D — Do not optimize yet**. The full reasoning
and the measured numbers are recorded so a future launch window can
revisit this without redoing the survey.

---

## 1. Baseline (measured at HEAD `4ed56a4`)

```
21 tools registered
Wire payload (JSON-serialised tools/list): 66,359 bytes
Token estimate (cl100k_base): ~16,600 tokens
Schema bytes total (raw):  27,824
Description bytes total:   14,756
Schema + Description:      42,580
Wire inflation factor:     ~1.56x (66,359 / 42,580)
```

### Per-tool ranking (by total schema + description bytes)

| Tool                  | Desc bytes | Schema bytes | Total | ~Tokens |
| --------------------- | ---------: | -----------: | ----: | ------: |
| `mpm_system`          |      1,865 |        8,029 | 9,894 |  2,473 |
| `mpm_work`            |        782 |        3,795 | 4,577 |  1,144 |
| `log_to_changelog`    |      1,312 |        1,857 | 3,169 |    792 |
| `mpm_memory`          |      1,292 |        1,603 | 2,895 |    723 |
| `mpm_context`         |      1,242 |        1,487 | 2,729 |    682 |
| `mpm_handoff`         |        772 |        1,957 | 2,729 |    682 |
| `mpm_skills`          |        942 |        1,256 | 2,198 |    549 |
| `mpm_wakes`           |        665 |        1,071 | 1,736 |    434 |
| `mpm_evidence`        |        476 |        1,009 | 1,485 |    371 |
| `mpm_theories`        |        458 |          970 | 1,428 |    357 |
| `mpm_lessons`         |        632 |          630 | 1,262 |    315 |
| `mpm_retrieval_diagnose` | 531 |            675 | 1,206 |    301 |
| `request_review`      |        545 |          605 | 1,150 |    287 |
| `mpm_decisions`       |        418 |          604 | 1,022 |    255 |
| `mpm_references`      |        402 |          547 |   949 |    237 |
| `mpm_scratchpad`      |        612 |          285 |   897 |    224 |
| `mpm_topics`          |        365 |          516 |   881 |    220 |
| `mpm_resolve`         |        422 |          243 |   665 |    166 |
| `mpm_blob_search`     |        314 |          313 |   627 |    156 |
| `mpm_confidence`      |        392 |          212 |   604 |    151 |
| `mpm_blob_read`       |        317 |          160 |   477 |    119 |
| **TOTAL**             |   **14,756** |    **27,824** | **42,580** | **10,645** |

Schema bytes dominate the description bytes by roughly 2:1. The
single biggest contributor is `mpm_system` (24% of the total) which
documents 10 lifecycle actions with their params.

---

## 2. Architecture inspection

The full survey was done with `internal/core/tools/registry.go` and
`internal/core/tools/registry_list.go` as ground truth, plus
`cmd/mpm-mcp/main.go` and `cmd/mpm-mcp/tools.go`.

### MCP server

- **Library**: `github.com/mark3labs/mcp-go v0.55.1` (per `go.mod:17`).
- **Transport**: stdio only (`server.ServeStdio(s)` at
  `cmd/mpm-mcp/main.go:208`). No HTTP, no SSE.
- **Server construction**: only one option is passed — `WithInstructions`.
  No `WithToolFilter`, no `WithToolCapabilities(true)`.
- **Registration**: `RegisterAllTools` iterates `tools.Registry` and
  calls `s.AddTool(...)` once at boot. After boot, the tool set is
  frozen in mcp-go's internal `s.tools` map.
- **tools/list**: not implemented in MPM — it is implemented by
  mcp-go via `handleListTools`. MPM only contributes tools through
  `AddTool`.

### Existing capability layer

`internal/core/config/config.go` has `Config.Capabilities`, `Config.Components`,
`Config.Profiles`, and helpers (`CapabilityFor`, `ResolveComponents`).
**Used only for LLM routing** (`request_review` and `mpm_context route`).
**Not used for tool gating.** No `default_tools`, `DefaultTools`,
`expose_tools`, `ExposeTools`, `filter_tools`, or `FilterTools`
symbol exists anywhere in the repository.

### Protocol features available but unused

- `WithToolFilter(fn)` — a `func(ctx, []mcp.Tool) []mcp.Tool` that
  filters tools at both `tools/list` and `tools/call` time. MPM does
  not pass this option.
- `WithToolCapabilities(true)` — enables `listChanged` notifications.
  MPM does not pass this option.
- `SendNotificationToSpecificClient(...)` — would be required to
  actually emit a `listChanged` notification. MPM has zero hits.

### Host adapters — cache/refresh behavior

From `agent_installation/INSTALL.md` and the per-host READMEs:

| Host          | Tool list refresh trigger |
| ------------- | ------------------------- |
| OpenClaw      | Gateway restart only — "Gateway caches MCP servers at startup" (INSTALL.md:460) |
| Claude Code   | Session restart only — "mcpServers and CLAUDE.md are loaded at session start" (INSTALL.md:389) |
| Hermes        | Session restart only — "config changes don't apply mid-conversation" (INSTALL.md:461) |
| Pi            | Restart only — Extension re-loaded on settings.json change |
| OpenCode      | Hand-curated 17-tool subset at the adapter layer (drops `mpm_work`, `mpm_resolve`, `mpm_blob_read`, `mpm_blob_search`) |

**No host supports `listChanged` notifications as a feature.** No
documentation references MCP's `listChanged` capability anywhere in
MPM. The OpenCode and Pi adapters curate at the *host* layer, not at
the MPM server.

---

## 3. Strategies evaluated

### Strategy A — Smaller default tool set (server-side filter)

**Hook point**: `server.WithToolFilter(...)` at `cmd/mpm-mcp/main.go:188`.

**Approach**: keep `tools.Registry` unchanged; install a per-call
filter that whitelists a "core" tool set at boot. Specialist tools
remain registered (so CLI `mpm call <tool>` and the substrate still
work) but are hidden from `tools/list` for sessions that don't opt in.

**Measured savings** (rough estimate, picking 5 tools that the seven
integration invariants imply are always-needed: `mpm_memory`,
`mpm_context`, `mpm_handoff`, `mpm_work`, `mpm_skills`):

| Set                | Tools | Total desc+schema bytes | ~Tokens |
| ------------------ | ----: | ----------------------: | ------: |
| Current (all 21)   |    21 |                  42,580 | 10,645 |
| Core 5 only         |     5 |                  15,128 |  3,782 |
| Core 7 (+ lessons, theories) | 7 |              ~17,800 |  4,450 |

**Reduction**: ~5,900-7,000 tokens from the schema floor.

**Compatibility impact**: HIGH. Every host integration would need
re-verification:

- Hosts that already curate at the adapter layer (OpenCode, Pi):
  filter would have no effect (they already subset).
- Hosts that currently see all 21 (Claude Code, Hermes, OpenClaw MCP
  bundle): filter would hide tools they currently have. Operators
  relying on `mpm__mpm_system`, `mpm__mpm_blob_read`,
  `mpm__mpm_blob_search` would silently lose access at the MCP
  surface. The CLI fallback (`mpm call <tool>`) still works, but the
  agent must know the tool name out-of-band.
- `listChanged` is not used; the change applies at server restart.
  Hosts that already start with a small curated set won't see any
  difference.

**Discovery cost**: agents that need a hidden tool must know its name
via the integration docs (which list all 21), the project README, or
human prompt. No runtime discovery exists.

**Implementation complexity**: a new `ToolExposure` config block,
filter wiring, host-by-host integration verification, fall-back path
("expose all" config), regression tests for the filter.

**Verdict**: possible, with measurable savings, but high compatibility
risk in the alpha window. The OpenCode/Pi precedent shows host-layer
filtering works without any server-side change — but those hosts
filter at install time, not per session, and never expose a way to
re-introduce hidden tools without an adapter edit.

### Strategy B — Domain/capability tool groups

**Approach**: emit per-tool `category` / `domain` annotations; teach
the `route` MCP tool to answer "what tools do you have for X?"

**Measured impact**: zero. Annotation bytes add to the wire payload
(so this makes the floor worse, not better). The agent can already
ask `tools/list` to discover the surface — that is precisely what
`tools/list` exists for. Adding a queryable index on top of an
already-queryable list is duplication.

**Verdict**: pure overhead. Skip.

### Strategy C — Compact tool descriptions

**Approach**: rewrite tool descriptions to remove redundant prose
while preserving parameter semantics, enum meanings, safety
constraints, and lifecycle warnings.

**Measured savings** (rough): if every tool shed 20% of its
description bytes, total descriptions drop from 14,756 to ~11,800.
That's ~740 tokens of model-facing reduction. Schema bytes (~27,824)
are unaffected because JSON-Schema parameter constraints must stay.

**Risk**: low-to-moderate. The "Use when: ... Do not use when: ..."
pattern, the "Lifecycle asymmetry:" paragraphs, and the
projection/bounded-echo notes are **safety-critical** for tool
selection. Trimming them risks degrading tool-use correctness, which
would be a worse outcome than the ~740-token savings.

**Compatibility impact**: zero — pure description change.

**Implementation complexity**: every tool author has to re-review
their description. 21 tools × ~700 bytes each = ~15K bytes of prose
to rewrite. This is **content work**, not engineering work, and is
better done by the tool authors as part of normal maintenance.

**Verdict**: a future, ongoing, low-priority cleanup. Not blocking
alpha. Not worth a focused pass.

### Strategy D — Two-level discovery (initial core + dynamic expand)

**Approach**: server exposes a small core set at boot. When the
agent needs a specialist tool, it asks for it; the server emits a
`listChanged` notification and the new tool appears.

**Measured savings**: same as Strategy A.

**Compatibility impact**: BREAKING for every host. No host in MPM's
supported set supports `listChanged` notifications as a feature:

- OpenClaw: "Gateway caches MCP servers at startup" — no runtime
  refresh.
- Claude Code: "mcpServers and CLAUDE.md are loaded at session start;
  without restart, the `mpm__*` tools will not appear".
- Hermes: "config changes don't apply mid-conversation".
- Pi: extension re-loaded on settings.json change.
- OpenCode/Pi: hand-curated subsets, static for the life of the
  adapter.

The MCP protocol *permits* `listChanged`, but the actual host
ecosystem MPM ships against today does not consume it. Implementing
Strategy D would require either (a) convincing the hosts to add
support (out of scope for an MPM alpha), or (b) MPM shipping
non-standard protocol behaviour that no host understands.

**Discovery cost**: prohibitive for an alpha. The first time an
agent needs a hidden tool, the tool isn't visible and there's no
way to ask for it.

**Verdict**: not implementable against the current host ecosystem
without invasive changes outside MPM. Document for a later launch
window when the host landscape has caught up.

---

## 4. Comparison table

| Strategy             | Initial tools | Initial tokens | Reduction | Discovery cost | Compatibility |
| -------------------- | ------------: | -------------: | --------: | -------------: | ------------- |
| **Current (baseline)** |          21 |       ~16,600 |         — |              — | All hosts see all tools |
| **A — smaller default** |     5-7 |        3,800-4,450 | **~12,000 tokens** | None — hidden tools reachable only via `mpm call` CLI or out-of-band knowledge | HIGH — every host re-verified |
| **B — domain groups**  |         21 |          higher |       negative | n/a (annotation bytes add cost) | zero |
| **C — compact descriptions** |  21 |       ~15,900 |       ~740 | n/a | zero — but content work, not engineering |
| **D — two-level discovery** | 5-7 |        3,800-4,450 |       ~12,000 tokens | **Prohibitive** — no host supports `listChanged` | BREAKING for all current hosts |

Strategy A's reduction (the biggest of the four) is real but not
worth the alpha compatibility risk. Strategy B is counterproductive.
Strategy C is content work, not engineering. Strategy D is blocked
on the host ecosystem.

---

## 5. Recommendation

**D — Do not optimize yet.**

Reasoning:

1. The 16.6K-token cost is bounded and predictable. It is paid once
   per session. A 100K-token model loses ~16.6% of its input budget;
   a 16K-context model loses its full budget. The first case is
   acceptable; the second is a different problem (host choice, not
   MPM).
2. The biggest reduction (Strategy A) is real but the implementation
   changes the universal CLI fallback contract in a way that requires
   host-by-host re-verification. Alpha-launch timing does not
   accommodate that.
3. The two hosts that DO filter today (OpenCode, Pi) do so at the
   adapter layer. Their 17/21 subset pattern works in production and
   demonstrates that **filtering is achievable for the agents that
   need it** without any server-side change.
4. The OpenCode/Pi precedent is the right "do not optimize yet"
   pattern: each host curates for its own context budget. MPM
   continues to expose the full surface; hosts that need a smaller
   one filter at install time.
5. The CLI escape hatch (`mpm call <tool>`) is documented and
   universal. If a tool is hidden from `tools/list`, the agent can
   still reach it via CLI — provided it knows the tool name from
   some other source (the integration docs list all 21).

When to revisit:

- When a host integration explicitly requests a smaller default.
- When `listChanged` notification support becomes common across the
  supported host matrix.
- When token budgets shrink below 32K and the floor becomes a
  significant fraction of usable context.

What to revisit **without** waiting:

- Strategy C (compact descriptions) is a low-priority, ongoing
  cleanup. Tool authors can tighten their descriptions during normal
  maintenance without changing the contract.

---

## 6. No production change made

This pass is documentation-only. No code changes. The existing
`docs/CONTEXT_ECONOMICS.md` continues to be authoritative for the
baseline numbers.

---

## 7. Files

| Path                                    | Purpose                                          |
| --------------------------------------- | ------------------------------------------------ |
| `docs/CONTEXT_EXPOSURE.md`              | This document                                    |
| `docs/CONTEXT_ECONOMICS.md`             | Baseline measurements (unchanged)                |
| `scripts/context_economics/measure.sh`  | Reproducible baseline measurement               |
| `internal/core/tools/tool_size_probe/`  | Per-tool schema/description bytes probe         |

---

## 8. Reproduction

```bash
# Per-tool baseline (re-run anytime)
cd internal/core/tools && go run ./tool_size_probe

# Full context-economics re-measurement
bash scripts/context_economics/measure.sh
python3 scripts/context_economics/count_tokens.py /tmp/mpm-econ-*/raw/summary.json
```

Both harnesses are disposable-workspace safe; they never touch the
operator's real `~/.mpm`.
