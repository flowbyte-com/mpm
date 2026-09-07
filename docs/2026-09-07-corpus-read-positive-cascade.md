# 2026-09-07 — Corpus read for positive-direction cascade prevalence

**Status:** Step 3 of the design investigation from
`docs/constructive-cascade-design.md`. Read-only classification of all
94 currently-pending theories. No theories resolved, modified, or
touched.

## Method recap

Pulled full hypothesis + validation_criteria text for all 94 pending
theories plus a calibration sample of 60 recently-resolved theories
(proven, disproven, challenged, resolved — most recent first).
Classified each into one of four buckets per the design brief:

- **A — Contingent-on-uncertainty**: hypothesis reasons from "since X
  is unknown/unproven/not yet established" — the exact pattern the
  design is meant to catch.
- **B — Contingent-on-falseness**: reasons from "since X is presumed
  false/wrong" specifically.
- **C — Unrelated structure, similar wording**: uses conditional
  phrasing but isn't actually reasoning about the epistemic status
  of another artifact.
- **D — No dependency structure at all**: standalone claims, test
  scaffolding, auto-generated challenges.

Err-toward-C discipline applied on borderline cases.

## Result

| Bucket | Count | Of 94 |
|---|---:|---:|
| **A** — Contingent-on-uncertainty | **0** | 0.0% |
| **B** — Contingent-on-falseness | **0** | 0.0% |
| **C** — Similar wording, unrelated | 2 | 2.1% |
| **D** — No dependency structure | 92 | 97.9% |
| **Total** | 94 | 100% |

**Prevalence (A+B)/94 = 0/94 = 0.0%**
**B-only prevalence = 0/94 = 0.0%**

## Auditable list — A and B classifications

**Zero theories in either bucket.** The `corpus_classification.json`
artefact (this run's analysis output, gitignored) carries the full
per-theory classification with the justifying phrase for every entry.

## C-bucket near-misses (representative)

These two theories use conditional phrasing ("if X is true") but the
conditional is NOT on the epistemic status of a substrate artifact
(memory/lesson/decision/theory). Listed so the classification is
auditable, not just asserted.

| ID | hypothesis excerpt | why C, not A or B |
|---|---|---|
| `4d8b53d16cad9bae` | "If I generate one digest per week for 4 consecutive weeks, the digest output will become structurally predictable enough..." | conditional is on the *operator's future action* (their own digest-running cadence), not on any substrate artifact's truth value |
| `2ba93baf595da329` | "If MPM is alpha-ready, all 20 tests will complete with findings classified as H or I, with no P0 correctness bugs" | conditional is on a property of MPM itself (alpha-readiness), not on a substrate artifact's truth value |

## Borderline cases considered and rejected from A/B (with reasoning)

| ID | summary |
|---|---|
| `74a89aa3febd0f84` | "synthesis_config.model is empty string" — the dependency is on a config setting, not a substrate artifact. Cascade doc and design doc scope to artifacts (memory/lesson/decision/theory); config state is out of scope. The synthesis-activation bug is real but separate from the positive-cascade question. |
| `11f1e2baa09ed7c0` | "downstream artifact requires re-evaluation because foundation has been invalidated" — WAS contingent reasoning, but on a foundation being *invalidated*, which is exactly the existing invalidation cascade. The positive-direction gap would only have helped if the foundation had been *proven*, not *disproven*. |
| `effd6ad369b86c7b` | "theory depending on the strong-contradiction lesson" — same as above; invalidation cascade pattern, not the positive gap. |

## Calibration from resolved corpus

Of 16 substantive resolved theories examined (excluding auto-generated
critic-cycle and c4/s7 test scaffolding):

- Most are bug claims about MPM behaviors (not contingent on another
  artifact's epistemic status)
- A few are cascade re-evaluations (contingent on upstream being
  *invalidated*, the existing invalidation pattern)
- None show "downstream conclusion contingent on upstream being
  uncertain" — the exact positive-direction pattern

The cascade re-evaluations in the resolved corpus (11f1e2baa09ed7c0,
effd6ad369b86c7b) ARE contingent reasoning — but the upstream was
*disproven*, not *proven*. The existing invalidation cascade handles
that case correctly. The positive-direction gap would only matter if
the upstream had been *proven true* (or crossed above 0.8 confidence)
after the downstream was written. No such case exists in either the
pending or resolved corpus.

## Recommendation: HOLD

Prevalence (A+B)/94 = 0% falls below the **<5% threshold** specified
in `docs/constructive-cascade-design.md`. The decision rule:

> "If the corpus read returns <5% prevalence, **hold entirely** until
> one of the three re-investigation triggers fires; design doc remains
> valid as the spec when one does."

**No implementation.** The design in
`docs/constructive-cascade-design.md` remains valid as a spec for
when one of the three re-investigation triggers fires:

1. **Real incident** — a documented case where dependent reasoning
   was demonstrably wrong because a positive cascade wasn't fired.
2. **Dependency density shift** — avg dependents per proven artifact
   exceeds 2 (currently 0).
3. **Recurring operator cost** — 3+ distinct reports in 90 days of
   operators manually checking downstream after a
   `ResolveTheory(proven)` call.

## Audit trail

The full per-theory classification with justifying phrases for all 94
theories is available in the run's analysis artefact:
`tool-results/corpus_classification.json`. This file is gitignored
(runtime artefact) but available for the operator's audit if needed.

## What this means for the original `openclaw doctor` cleanup

This is the second-to-last item from the original `openclaw doctor`
investigation (`wakes_overdue=9407`):

| Step | Status |
|---|---|
| Step 1: SQL type-coercion fix | ✅ Done (`4ed04b4`) |
| Step 2: Non-canonical scheduled_tasks removal | ✅ Done (`aa2ddb6`) |
| Step 3: Stale cron + cascade wake cleanup | ✅ Done (`36092a4`) |
| Step 4: Malformed-metadata wake cleanup | ✅ Done (`8371fec`) |
| Theory triage Part 1 (Cluster A + B) | ✅ Done (Part 1 of clean-up pass) |
| Theory triage Part 2 (live-state check) | ✅ Done (`74a89aa3febd0f84` flagged live) |
| Theory triage Part 3 (positive-cascade investigation) | ✅ Design completed (`constructive-cascade-design.md`, `0447944`) |
| Corpus read for positive-cascade prevalence | ✅ **Done** (this doc) — **0% prevalence; HOLD** |

The original investigation is closed. The synthesis-activation bug
(`74a89aa3febd0f84`) remains as a flagged live defect for the
operator's separate attention; the positive-cascade feature remains
on hold pending one of the three re-investigation triggers.