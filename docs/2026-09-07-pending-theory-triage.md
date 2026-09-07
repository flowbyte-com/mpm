# 2026-09-07 — Pending theory triage (Parts 1 & 2)

**Status:** Step 5 of the 2026-09-07 `openclaw doctor` cleanup.
The 128 pending theories identified in the original investigation have
been triaged per the operator's three-part prompt.

## Part 1: Cluster A + Cluster B + Cluster C.3 (32 theories → disproven)

- **Cluster A** (1 theory, `11f1e2baa09ed7c0`): the cascade-generated
  re-evaluation theory from the Sep 5 live-probe work. Conclusion note
  records the `deadbeef...` placeholder-commit origin (same evidence
  trail as the 2 cascade wakes deleted in Step 3).
- **Cluster C.3 effd6ad369b86c7b** (the downstream artifact of Cluster A's
  cascade): same test-pollution origin, resolved with the same
  reasoning. Reclassified from "ambiguous" to "test pollution" based on
  the cascade trace.
- **Cluster B** (30 theories, Sep 6 15:08–17:56): all 30 follow the
  auto-generated "Memory X is obsolete" template (24× `s7-chal-...`,
  6× `f-c4-...`), confirmed from sample inspection. Conclusion note
  records the S6/S7 cognitive-verb parity testing wave origin.

Each theory's metadata was updated via `json_patch` matching the
canonical `DatabaseManager.ResolveTheory` UPDATE shape: status,
conclusion (note text), resolved_by = `call:resolve_theory`,
resolved_by_via = `manual`, resolved_at. Cascade enqueue was
explicitly skipped — verified upstream that none of the 32 are
referenced by any other artifact as a dependency, so cascade would
be a no-op.

## Part 2: Live-state check on 3 C.2 theories

| theory | outcome | reason |
|---|---|---|
| `74a89aa3febd0f84` — synthesis activation bug | **LEFT PENDING — live bug** | `mpm_config.json` `synth.model` is still `""` in the active profile. Hypothesis is correct, bug is live. Flagged separately for the operator. |
| `b4c7a2fad1e1e41b` — pointer-first retrieval | **resolved proven** | Phase 1 architecture shipped and verified: `BoundInlineContent` 2048 B cap (≈500 tokens default), pointer resolver maxBytes 512 B, MCP spill threshold 20480 B, mpm_resolve + mpm_blob_read + mpm_blob_search tools (commit `5c2a7ce`), byte-for-byte round-trip proofs (`81168ed`), mpm_blob_read offset parity across CLI and MCP (`85defd65`), pointer-indirection audit/sweep (`docs/pointer-indirection-{audit,sweep}-2026-09-05.md`). Validation criteria effectively met by today's audit work. |
| `07ca079bb9c7d500` — pointer architecture keeps context bounded | **resolved proven** | Same Phase 1 audit work confirmed: spill mechanism, BoundInlineContent, MaxWakeContextBytes 32 KiB shed-and-flush, mpm_blob_search scan window bounded. All bounded, context stays manageable, detailed retrieval remains available via mpm://work/<id> + mpm_resolve/mpm_blob_read. |

The synthesis activation theory is left pending for the operator to
decide how to handle. It is a real, currently-live defect:
`synth.model = ""` in `mpm_config.json` means the synthesis worker
has no LLM configured and any auto-synthesis attempt would fail. The
hypothesis's root-cause claim (empty model string) is verified by direct
inspection. The validation criteria ("synthesis never triggers
automatically because...") is observably true today.

No other C.2 or C.3 theories were touched in this pass, per the
operator's instruction.

## Post-triage state

| metric | before | after |
|---|---:|---:|
| Pending theories | 128 | 94 |
| Disproven (Sep 7) | 0 | 34 |
| Live bugs newly flagged | 0 | 1 (`74a89aa3febd0f84` synthesis activation) |

## Part 3: Positive-direction cascade trigger — Branch B (documented gap)

Investigation in `docs/archive/epistemic-cascades.md` under new
section "Why positive resolutions don't cascade (known, deliberate gap)".

**Branch chosen: B — document the gap, do not build the trigger.**

Reasoning:

1. **Substrate does not have polarity metadata.** `memories.dependencies`
   is a flat JSON array of IDs; `epistemic_provenance` has
   `source_id`/`downstream_id` but no polarity. Adding a trigger that
   fires on every `ResolveTheory(proven)` would re-evaluate every
   dependent regardless of whether they were reasoning against the
   foundation — cascade fatigue is its own failure mode.
2. **Empirical audit: 0 of 26 recently-proven theories had any
   downstream dependent.** No observed instance of the failure mode.
3. **Consistent with `docs/epistemic-confirmation.md`.** The
   confirmation/contradiction hooks (`83c0b3a`, `d8dbd92`) shipped
   deliberately opt-in for the upstream direction. Positive-direction
   cascade would silently undo that choice; the doc's design intent
   should propagate.

The new doc section also lists the prerequisites that would need to
exist before any future implementation (polarity schema, opt-in
per-artifact flag, empirical validation), and the trigger conditions
that would warrant re-investigation (real incident, dependency
density shift, or recurring operator cost).

## Cross-references

- Step 1 commit: `4ed04b4` — SQL type-coercion fix
- Step 2 commit: `aa2ddb6` — non-canonical scheduled_tasks removal
- Step 3 commit: `36092a4` — stale cron + cascade wake cleanup
- Step 4 commit: `8371fec` — malformed-metadata wake cleanup
- Investigation docs: `docs/2026-09-07-{cron-stale-row-cleanup,stale-wake-cleanup,malformed-wake-cleanup}.md`
- Pointer audit work: `docs/pointer-indirection-{audit,sweep}-2026-09-05.md`,
  commits `60734bd`, `81168ed`, `be0d765`, `59ffd3d`, `85defd65`
- Confirmation/contradiction design: `docs/epistemic-confirmation.md`,
  commits `83c0b3a`, `d8dbd92`
- MPM lesson (silent time.Time → TEXT coercion): `7de0fa2df7898109`