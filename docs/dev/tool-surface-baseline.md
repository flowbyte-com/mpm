# G — Tool-surface evaluation baseline

> **Secondary priority.** This document establishes the baseline, the existing
> coverage and the evaluation plan. It does **not** claim that the tool
> surface works well — nor that it does not. It says what is measured, what
> is already tested, and what still has to be measured before anyone
> redesigns anything.

## 1. The baseline: what an agent actually sees

### 1.1 The default MCP surface is three tools

`cmd/mpm-mcp/main.go:48–98`:

> `defaultCoreTools` is the canonical initial MCP surface for MPM. Tools
> outside this set are filtered out of `tools/list` AND `tools/call` by the
> `server.WithToolFilter` option below; they remain registered in the
> substrate (so `mpm call <tool>` continues to work) but are not initially
> exposed to the model. The `mpm_help` discovery tool lets an agent reach
> specialists by name via the host shell. The full set is restored by setting
> `MPM_EXPOSE_ALL_TOOLS=1`.

| Tool | Absorbs |
|---|---|
| `mpm_memory` | persist / recall / show / shred, plus reinforce, weaken, snooze, patch, promote |
| `mpm_context` | wake / directives / route / handoff (write + read) |
| `mpm_help` | capability discovery — lists every registry entry with a `reach_via_cli` hint; `action=show` returns a tool's description **and** its registered input schema |

### 1.2 The specialist tools behind the filter

21 tools in `internal/core/tools/registry_list.go`:

```
mpm_memory  mpm_theories  mpm_decisions  mpm_lessons  mpm_topics
mpm_references  mpm_evidence  mpm_confidence  mpm_retrieval_diagnose
mpm_context  mpm_skills  mpm_wakes  mpm_handoff  mpm_scratchpad
mpm_system  mpm_log_to_changelog  mpm_request_review  mpm_resolve
mpm_blob_read  mpm_blob_search  mpm_work
```

plus `mpm_route` and `mpm_help` registered directly by the MCP server, and
`mpm_context` registered in a compact form.

### 1.3 Progressive discovery is a real mechanism, not a description

The chain is: **3 tools visible → `mpm_help` lists the registry → the agent
runs `mpm call <tool>` through the host shell → the specialist executes.**

`mpm_help action=show` returns the tool's description *and* its registered
input schema, so the discovery step does not require the agent to guess a
schema.

### 1.4 The filter is enforced at call time, not just list time

> The filter is applied at both `tools/list` and `tools/call` time (mcp-go
> enforces the latter); a tool that fails the filter is invisible **and**
> uncallable through the MCP surface.

This matters: a filtered tool cannot be reached by guessing its name through
MCP. The only route to a specialist is `mpm_help` → `mpm call`, which is
also what makes the *shell* dependency a documented design decision rather
than an accident.

## 2. Existing test coverage — substantial

| Area | Tests |
|---|---|
| Compact surface filter | `internal/core/tools/compact_surface_filter_test.go` |
| Compact `mpm_context` description contract | `cmd/mpm-mcp/mpm_context_compact_distinction_test.go` |
| `mpm_help` parity with the registry | `cmd/mpm-mcp/tools_help_parity_test.go` |
| Registry / dispatcher parity | `internal/core/tools/d3_memory_schema_dispatcher_parity_test.go` |
| CLI↔MCP contract alignment | `internal/core/tools/f0910_cli_contract_alignment_regression_test.go` |
| Output-policy and schema guards | `internal/core/tools/adapter_schema_guard_test.go` |
| CLI/MCP parity regressions | `internal/core/tools/d041_d101_surface_parity_regression_test.go`, `d006_theory_dispatch_test.go` |
| Removed-capability guard | `internal/core/tools/mpm_challenge_removed_test.go` |
| Protocol maintenance | `internal/core/tools/protocol_maintenance_operations_test.go` |
| Help surface | `internal/core/tools/help_test.go` |
| Blob boundary and offset parity | `internal/core/tools/blob_read_boundary_regression_test.go`, `blob_read_offset_parity_regression_test.go` |
| Context projection schema parity | `internal/core/tools/context_projection_schema_parity_test.go` |
| Wake-context format | `internal/core/tools/context_read_wake_context_format_regression_test.go` |

**Finding: the surface is heavily regression-tested at the *contract* level
— schema shape, parity, filtering, output policy.** What is **not** covered
is **behavioural**: whether an agent, given a real task, actually finds and
uses the right operation.

## 3. The evaluation questions, and what exists today

| Question | Existing mechanism | Measured today? |
|---|---|---|
| **Discovery round-trips** | `mpm_help` lists the registry; `action=show` returns description + schema | ⛔ no behavioural fixture |
| **Pointer resolution** | `mpm_resolve` resolves `mpm://memory/<id>` style pointers; `mpm_blob_read` / `mpm_blob_search` for blob-backed artifacts | ⛔ not measured as a round-trip cost |
| **Token overhead** | the compact 3-tool surface is *designed* to reduce it | ⛔ **not measured at all** — see §4 |
| **Tool calls per task** | — | ⛔ no instrumentation exists |
| **Tool-selection accuracy** | — | ⛔ no fixture |
| **Knowledge-type classification** | typed collections (`memories`, `lessons`, `decisions`, `theories`, `evidence`, `references`, `skills`, `handoffs`) | ⛔ no fixture |
| **Use of specialised capabilities** | `mpm_work`, `mpm_wakes`, `mpm_scratchpad`, `mpm_request_review` exist for specific circumstances | ⛔ no measurement |
| **Missed retrieval opportunities** | — | ⛔ no measurement |
| **Unsupported / duplicate knowledge creation** | the substrate validates; `mpm_contextual_candidates` has a `global_cap_applied` counter and `input_truncated` reporting | partly — the counters exist, the rate is unmeasured |

## 4. The one number worth measuring first

**Token overhead of the MCP surface.** It is the cheapest meaningful metric
and it is the one the compact surface was built to optimise.

The proxy is straightforward: the byte length of the tool descriptions the
client receives on `tools/list`, for the compact surface and for the full
surface, converted to tokens.

| Surface | Description source | Size |
|---|---|---|
| compact (3 tools) | `cmd/mpm-mcp/tools.go` — the compact `mpm_context` description plus the compact `mpm_memory` and `mpm_help` | to be measured |
| full (`MPM_EXPOSE_ALL_TOOLS=1`) | `internal/core/tools/registry_list.go` (60 699 bytes of registry text) plus the server-registered tools | to be measured |

**Proposed fixture:** a script that starts `mpm-mcp` against a scratch
workspace, issues `tools/list` with and without `MPM_EXPOSE_ALL_TOOLS`, and
reports the description bytes and an estimated token count for each. It must
run in an isolated workspace (§ F) and must not touch the live database.

**Acceptance criterion:** the compact surface must be materially smaller than
the full surface — that is the entire premise of the design — and the
difference must be large enough to matter at the scale of a session's tool
definitions.

## 5. How to distinguish a tool-surface problem from other causes

This is the distinction the brief insists on, and it is easy to get wrong:

| Symptom | Tool-surface cause? | More likely cause |
|---|---|---|
| The agent never calls MPM at all | no | the agent's instructions, the model, or the host's tool budget |
| The agent calls MPM but uses `mpm_memory` for everything | possibly — the compact surface under-signals the specialists | the agent's instructions |
| The agent calls a specialist with a wrong action | possibly — discovery returned an incomplete description | a missing contract test |
| The agent retrieves nothing relevant although the knowledge exists | no — this is retrieval quality | the ranking, the scoping, or the knowledge's own quality |
| The agent creates a memory where a lesson belonged | no — classification | the agent's instructions |

**Rule: a tool-surface conclusion requires a fixture in which the tool
surface is the only variable.** Every other explanation must be excluded
first. This is why §4 proposes a fixture rather than an opinion.

## 6. Deferred evaluation work

Deferred to **Session C**, whose territory this is (adapter and behavioural
evaluation), and sequenced after the event contract is agreed:

1. **Discovery round-trip fixture.** Give an agent a task whose solution
   requires a specialist (`mpm_work`, `mpm_wakes`, `mpm_request_review`).
   Measure: how many tool calls to reach it, whether `mpm_help` was used,
   whether the agent guessed a name instead.
2. **Pointer-resolution fixture.** Measure the cost of resolving a pointer
   versus re-querying, and the failure mode when a pointer is stale.
3. **Tool-call accounting.** The substrate already records every tool
   invocation in `tool_invocations` with `tool_name`, `action`,
   `duration_ms` and `outcome_class`. **A behavioural fixture can query
   `tool_invocations` directly** — no new instrumentation is needed. This is
   an existing mechanism and should be reused rather than instrumented
   again.
4. **Classification accuracy.** For a set of realistic statements, does the
   agent choose the right type? The answer is a property of the *instructions*
   at least as much as of the tool surface — both must be held constant.
5. **Missed-opportunity detection.** Where an agent had a durable insight and
   did not record it. This is the "retrospective missed-opportunity
   detection" that Session B owns — and it should consume the event stream
   from the contract, not a parallel mechanism.

## 7. Recommendation

**Preserve the current tool surface.** It is deliberately designed for
compactness, it is contract-tested at the schema and parity level, and
nothing in this baseline demonstrates a defect.

The **one** thing worth doing is the cheap measurement in §4, because it
either confirms the design's premise or falsifies it, and it costs an
afternoon. Everything else waits for Session C's fixtures.

**Explicitly not recommended:** a broad MCP API redesign, replacing the typed
operations with a generic memory interface, or changing the compact-surface
default. None is justified by anything measured so far, and all three would
be expensive to reverse.

## 8. Reproducibility record

For any future run of these measurements, record:

| Item | Value |
|---|---|
| Repository revision | `414b27651ee66ec5dcad9ba75cd43b6c4b2fac9c` |
| Branch | `integration/session-a` |
| Installed runtime | `~/.mpm` @ `3df7231`, binary `0.1.0-dev+g3df72315.t1792851233` |
| Model | MiniMax-M3.1-Flash (`minimax-coding-plan`), via OpenCode |
| MCP surface | compact (3 tools); full surface via `MPM_EXPOSE_ALL_TOOLS=1` |
| Workspace | isolated scratch (`MPM_WORKSPACE` + scratch `HOME`) — never the live database |
| Go / Node | Go 1.26.6; Node 20 |

**Do not claim tool effectiveness from invocation counts alone.** A high call
count may mean the surface is hard to use *or* that the agent is working
correctly. The fixtures in §6 are designed to separate those.
