# MPM — Pointer-Indirection Exhaustive Sweep (2026-09-05)

> **Status:** audit-only. No fixes applied. Awaiting confirmation on which
> items to act on, same commit-per-concern discipline as the prior pass
> (`d534ed7`, `81168ed`, `be0d765`, `59ffd3d`, `1527952`).
>
> This is the Section 3 sweep called out by the follow-up to
> `docs/pointer-indirection-audit-2026-09-05.md`. Methodology:
> mechanical grep for numeric size/length bound symbols across the
> whole codebase, cross-referenced against the now-six-plus documented
> thresholds, plus a targeted pass for unaccounted-for return paths
> with realistically-large payloads.

## §1. The reported thresholds (recap)

The previous audit identified four layered thresholds; the README
table at §6.4 MCP Integration (`1527952`) lists all of them plus the
two found during that follow-up:

| Threshold | Value | Source file |
|---|---|---|
| MCP transport spill boundary | 20480 B | `internal/core/tools/output_policy.go:38` |
| `BoundInlineContent` echo cap | 2048 B | `internal/core/output_limits.go:23` |
| Pointer resolver default `maxBytes` | 512 B | `cmd/mpm-mcp/tools.go` resolver functions |
| `mpm_blob_read` server ceiling | 256 KiB | `internal/core/tools/handlers.go:5642` |
| CLI snippet cap | 500 chars | `cmd/mpm/handlers_memory.go:527, 677` |
| Projection summary cap | 256 chars | `internal/core/summarize.go` |

## §2. Sweep methodology

Two greps with cross-reference:

1. **Bound-symbol grep:** `grep -rnE '(MaxBytes|maxBytes|Threshold|Bound|MaxLen|maxLen|Limit)' --include='*.go' .` — 421 hits. Categorised against the known six thresholds. The full list is preserved in the working notes; only the *new* items appear below.

2. **Return-path grep:** every handler-style function, every CLI
   command's handle function, and every `mpm_*` tool handler, looked at
   for "writes to caller without bound." Five candidates survived
   cross-reference; three were already documented (above) and two are
   flagged below.

## §3. New findings (audit-only)

### Finding F-S-1 — `MaxWakeContextBytes = 32 KiB` is undocumented

`internal/core/wake_context.go:295` defines `MaxWakeContextBytes = 32 * 1024`. The `EnforceSizeLimit` function (line 206) marshals `WakeContextData`, checks against this cap, and on overflow sheds the heaviest arrays first (`available_skills`, then `recent_topics`), setting `*_Truncated` flags so the agent can tell a cap-induced drop from "checked, none found."

**Severity:** low (already instrumented; shed-and-flush is gentle; tier ordering is documented; flags are first-class).

**Why audit missed it originally:** the original audit's grep was for *output* thresholds (spill, inline, summary, serverMax), not for *internal* gather-layer caps. `MaxWakeContextBytes` is a different layer — it lives in the wake-context assembler, not in the MCP transport layer.

**Recommendation:** add to the README §6.4 table with a note that it is enforced *before* the spill check, so a wake-context payload that crosses 32 KiB is shed before it ever reaches the MCP transport boundary. (Do not act in this pass — flagged here for confirmation.)

### Finding F-S-2 — `mpm decisions` (no-args, legacy listing) is unbounded

`cmd/mpm/handlers_epistemology.go:880` calls `dm.GetMemoriesForExport("decisions", "", "")` with no `--limit`. The internal function (`internal/core/web_db.go:1673`) has no `LIMIT` clause in its SQL.

**Current state on this host:**

```
decisions: 139 rows in the live DB
```

**Realistic size:** each row produces ~250 bytes of formatted output (CONTEXT + CHOICE + RATIONALE + date separator + divider). At 139 rows ≈ 35 KB. Not catastrophic today.

**Why this is a real finding, not a non-finding:**
1. The output scales *linearly* with the user's accumulated decisions. A multi-year collection reaches megabytes.
2. `mpm decisions query` already has a `--limit` flag; `mpm decisions list` reads through `handleDecisionsList` which has its own limit path; `mpm ls` has `--limit` defaulting to 20. **The legacy no-args path is the odd one out.**
3. There is no escape route: a caller cannot paginate the legacy listing because the CLI does not accept any flag for it. There is no spill/pointer mechanism on the CLI side — the entire result goes to stdout in one shot.

**Severity:** medium in scale (megabytes possible), low in likelihood (operator has to accumulate many decisions).

**Why audit missed it originally:** the original audit's cli sweep was driven by the `mpm call` choke-point (where spill doesn't apply) and focus was on the MCP path. `mpm decisions` legacy listing was not enumerated.

**Recommendation:** add a `--limit` flag (default 20) to the no-args path. Or, since the modern equivalents have `--limit`, mark the legacy behavior as deprecated and point operators at `mpm decisions list`. (Do not act in this pass — flagged here for confirmation, same as F-S-1.)

### Finding F-S-3 — `mpm_blob_search` scan window 50 KiB (NOT a finding)

`internal/core/tools/handlers.go:5654` defines `effectiveMaxBytes := 50 * 1024` as the default `max_bytes` for `mpm_blob_search`. This is the *scan window* over the blob (how many bytes of the underlying blob the regex/literal search reads through), not the size of the response itself. The response is sized by `max_matches` (default 20) × snippet length (~80 chars typical), well under the 20 KiB spill threshold.

**Why noted:** the parameter is named `max_bytes` in the JSON schema (`internal/core/tools/registry_list.go:467`), which suggests a response-size cap to a casual reader. Calling it out so it doesn't get mis-applied during a future audit.

**Severity:** none — informational only.

## §4. Items I checked and dismissed as already-bounded

| Path | Bound mechanism | Action |
|---|---|---|
| `mpm ls` (`cmd/mpm/simple_cmds.go:174`) | `--limit` default 20, post-truncate after `GetMemoriesForExport` returns the full collection (wasteful but not unbounded output) | already bounded; not a finding |
| `mpm decisions query` (`cmd/mpm/handlers_epistemology.go:1080`) | `--limit` accepted; falls through to `dm.QueryDecisions(query, limit)` which caps | already bounded |
| `mpm decisions list` (subcommand of `mpm decisions`) | uses `handleDecisionsList(dm, ...)` with its own limit path | already bounded |
| `mpm cascade materialize` (`cmd/mpm/handlers_cascade.go:128`) | fixed-format summary line `"materialized=%d failed=%d pending_after=%d elapsed=%s\n"` — payload is independent of materialize batch size | was already classified (d) "no mechanism"; output is a constant-format summary, not a content dump |
| `mpm ops confidence show/changes/trend` (`cmd/mpm/ops_confidence_cmds.go:42`) | each subcommand has `--limit` (caller-controlled) | already bounded |
| `mpm recall --token-budget` (`cmd/mpm/recall.go:455`) | caller-supplied cumulative token cap with `truncated` flag | already bounded + indicated |
| `mpm recall --limit` (default 15) | row count cap | already bounded |
| `mpm wake_eval`, `mpm status` | fixed-format report lines | already bounded |
| `mpm_mcp.exe SpillEnvelope{Bounded,ServerMax}` | server-side ceiling, see prior audit | already documented |
| `MaxEventWakesPerPull = 100` (`internal/core/broadcast.go:50`) | per-tick cap, not output | not an output bound; skip |
| `mpm_critic HighTokenThreshold = 100000` | alert threshold for low-token activities | not an output bound; skip |
| `consolidationLimit = 500` (`internal/core/memory.go:1621`) | batched background-work size | not an output bound; skip |
| `mpm_lint defaultThresholds` | lint rule pass/fail thresholds | not an output bound; skip |
| `ProvenanceThreshold = 0.10`, `HardConfidenceInvalidationThreshold = 0.3`, `ClusterThreshold = 3`, `FractureThreshold = 3`, `observationVerifyThreshold = 0.7`, `slowQueryThreshold = 100ms` | scoring / operational thresholds | not output bounds; skip |

## §5. The `mpm call` question

> Original audit noted: *"mpm call inline-only — CLI is not where a follow-up pointer round trip helps."* The follow-up asked: confirm that's still the right call **now that we're looking for gaps rather than auditing what already exists**.

Confirmation: **yes, "no round-trip benefit" is still the right call for `mpm call`.** The `mpm call` path uses `writeEnvelope(os.Stdout, result)` (`cmd/mpm/call.go:236`) and writes the full tool result to the operator's stdout. There is no equivalent of the MCP spill/pointer machinery on this path.

**That said, the lack of "no round-trip benefit" and "no bound at all" are different claims.** A CLI command with no bound can still write megabytes of unfilterable output to a terminal. The two findings above (F-S-1, F-S-2) are bounds that should exist *regardless of* the spill/pointer question, on user-experience grounds. F-S-2 in particular — `mpm decisions` legacy listing — is a user-experience problem today and will get worse with use; the spill machinery is irrelevant to the fix.

The follow-up's distinction is the right one to draw: `mpm call` is **correctly inline-only**, but individual CLI handlers can still have user-experience / output-size problems independent of that design choice. F-S-2 is one such. Other CLI handlers are well-bounded.

## §6. Recommended actions (awaiting confirmation)

If the project lead wants to act on F-S-1 and F-S-2, the work is small and isolated:

| Finding | Proposed change | Complexity |
|---|---|---|
| F-S-1 | Add `MaxWakeContextBytes` to the README §6.4 threshold table (information only) | small doc edit |
| F-S-2 | Add `--limit N` flag to legacy `mpm decisions` (default 20), wire to a row-cap path in `GetMemoriesForExport` or post-truncate like `mpm ls` | small code change + test |

Both can be one commit each. **Neither is done in this pass**; flagged here for the project lead's call.

If F-S-3 is worth flagging in the README schema doc as "max_bytes means scan window, not response size," that's also a one-line doc edit. Information only.

---

*End of sweep. Audit-only deliverable. Total of two genuine findings
(F-S-1 and F-S-2) and one informational note (F-S-3). The README
threshold table and the original audit's classifications are otherwise
complete.*
