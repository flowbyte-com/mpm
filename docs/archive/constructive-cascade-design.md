# Constructive Cascade — Design Investigation (2026-09-07)

**Status:** Design investigation, no implementation. Companion to
`docs/archive/epistemic-cascades.md` §"Why positive resolutions don't
cascade (known, deliberate gap)". That section deferred this feature on
three prerequisites (polarity schema, opt-in per-artifact flag,
empirical validation); this document specifies what each of those would
actually look like, and ends with a clear recommendation on whether
to build now, build after empirical evidence, or hold entirely.

---

## Step 1: Polarity schema design

### The problem with the current schema

Two paths feed cascade discovery today (`internal/core/cascade_outbox.go`):

1. **`memories.dependencies` JSON array** — bare IDs (sometimes with
   display prefixes like `"Lesson 2b22765cd1b13a81 (the original bug
   report)"`). Used as one of the upstream-discovery paths in
   `discoverCascadeTargets` (lines 569–609).

2. **`epistemic_provenance` table** — typed rows with `source_id`,
   `source_type`, `downstream_id`, `downstream_type`, `event_id`. The
   other upstream-discovery path (lines 611–642).

Neither carries **polarity** — there is no field saying whether the
downstream is reasoning *because* the upstream is true, *because* the
upstream is false, or *because* it just needs to know if the upstream
changes. The only information is "this artifact references that one."

A positive-direction cascade trigger that fires on every proven theory
and walks every dependent — irrespective of polarity — would re-evaluate
the entire downstream of every proven theory, on every prove. That is
the alert-fatigue scenario the design must avoid.

### Three polarity values

The user-spec requires at minimum three polarities. Mapping them onto
what the cascade actually does:

| Polarity | Meaning | Existing invalidation cascade | New positive cascade |
|---|---|---|---|
| `NULL` (default, backwards compat) | Unspecified / ambiguous | fires (preserves today's behavior) | **must never fire** |
| `assumes_true` | Downstream's conclusion rests on the upstream being true | fires (already works) | does NOT fire — the downstream's claim is now *more* supported, no re-evaluation needed |
| `assumes_false` | Downstream's conclusion rests on the upstream being false / unresolved / unproven | fires | **fires** — the new gap |
| `cares_about_change` (optional, future) | Downstream genuinely doesn't care which way, just wants to know on transition | fires | fires |

The third "genuinely doesn't care which" case is the smallest user-spec
requirement; the second ("assumes false") is the load-bearing case for
the new feature. "Cares about change" is left as a possible later
extension if operators want symmetric treatment — not required for the
gap.

### Where polarity lives: `epistemic_provenance` column, not `dependencies` JSON

The user-spec asked me to weigh three shapes (extend `dependencies`,
new column on `memories`, separate join table) against the existing
`epistemic_provenance` table.

**Recommendation: extend `epistemic_provenance` with a `polarity` column.**

Reasons:

1. **The project's typed-provenance pattern.** `epistemic_provenance`
   is the canonical typed-citation table; every other MPM table uses
   columns not JSON when a field is structured (`evidence`, `blobs`,
   `artifact_provenance`, `scheduled_wakes`). Extending the typed
   table matches the pattern; reshaping `dependencies` (a free-text
   JSON array) into structured objects would introduce the first
   "JSON-with-shape" path in memories metadata.

2. **Backwards compatibility at the row level.** Adding a column with
   `DEFAULT NULL` is a one-statement migration; existing rows have
   polarity=NULL, which by design never fires the positive cascade.
   Migrating `dependencies` JSON from `["abc"]` to `[{id:"abc",
   polarity:"assumes_false"}]` is a per-row data migration that
   *could* also be backwards-compatible (treat bare strings as
   polarity=NULL), but it's a JSON parser concern not a SQL concern,
   and the existing 1 row in `dependencies` has display prefixes
   like `"Lesson ... (the original bug report)"` which would need
   parsing back to bare IDs.

3. **The positive cascade should run on a smaller, more curated
   surface than the invalidation cascade.** `dependencies` JSON is
   loosely maintained; `epistemic_provenance` is created explicitly
   via `RecordProvenance` calls in application code (see
   `internal/core/cascade_provenance.go`). Forcing operators to
   opt-in via a structured path that already exists is more
   consistent with the project's "explicit-only" stance from
   `docs/epistemic-confirmation.md`.

**What `dependencies` JSON keeps doing:** unchanged. It still feeds
the *invalidation* cascade (current behavior). It just doesn't
participate in the positive cascade. The `NULL`-polarity invariant
holds for both discovery paths.

### Migration shape (matches the H.1 pattern)

```sql
-- Additive, backwards-compatible. Existing rows have polarity NULL.
ALTER TABLE epistemic_provenance
    ADD COLUMN polarity TEXT
        CHECK (polarity IS NULL OR polarity IN
              ('assumes_true', 'assumes_false', 'cares_about_change'));
```

Plus a migration test that:
- Inserts a provenance row with `polarity='assumes_false'` — positive
  cascade discovery picks it up.
- Inserts a provenance row with `polarity=NULL` — positive cascade
  discovery does not pick it up.
- Runs against a DB built with the old shape (no `polarity` column)
  before the migration, simulates the migration, verifies the new
  queries work and old behavior is preserved.

---

## Step 2: Trigger condition + opt-in mechanism

### Trigger conditions

| Event | Existing invalidation | New positive |
|---|---|---|
| `ResolveTheory(theoryID, conclusion, "proven")` | n/a | **fire** if any dependent has `polarity='assumes_false'` |
| `ResolveTheory(theoryID, conclusion, "disproven")` | fire | unchanged (existing) |
| `RecomputeConfidence` crosses below 0.3 (HardConfidenceInvalidationThreshold) | fire | unchanged |
| `RecomputeConfidence` crosses above **0.8** (proposed PositiveConfidenceThreshold) | n/a | **fire** if any dependent has `polarity='assumes_false'` |

**Why 0.8 for the symmetric threshold:** The existing 0.3 floor is
"very unconfident, almost certainly wrong." The system's default
confidence for new memories is 0.8 (per `docs/mpm-invariants.md`), so
0.8 represents "the default level of trust." Crossing *above* the
default trust threshold is the meaningful event — a previously-below-
default theory is now well-established. There's no specific reason to
pick 0.7 or 0.9 over 0.8; 0.8 has the conceptual justification of
matching the system-wide default. If we discover in practice that
0.8 is too sensitive (most re-evaluations land just above the
default and cause noise), 0.85 or 0.9 would be reasonable
adjustments. Pick 0.8 as the documented default; revisit if
investigation finds it's too noisy.

### Opt-in is the load-bearing safety property

Per the user-spec: a bare, unpolarized dependency (the existing
default, and what all current data has) must **never** trigger a
positive cascade. This is non-negotiable — anything weaker
invalidates the safety rationale for the whole feature.

Concretely:

- Positive cascade discovery joins through `epistemic_provenance` rows
  where `polarity='assumes_false'`. Rows with `polarity IS NULL` or
  `polarity='assumes_true'` or `polarity='cares_about_change'` (the
  last until/unless we add support) are filtered out.
- The existing `dependencies` JSON path **does not** contribute to
  positive cascade discovery at all. Even if a row in `dependencies`
  is later updated to a richer object, the positive cascade does not
  read it. (This is a stronger safety property than just "treat bare
  IDs as NULL"; it removes the path entirely.)
- A regression test — written **before any production code** —
  proves this: `TestPositiveCascade_OldShapeDependencyNeverFires`.

The asymmetric case ("`assumes_false` triggers on positive resolution,
but does it also trigger on invalidation?") needs its own test
because the answer is yes: if downstream's reasoning assumed upstream
was false and the upstream is *disproven* (now known to be false in
fact, not just unproven), the downstream's claim is still false too
in a different sense, and re-evaluation is appropriate. So the
*positive* cascade trigger is the *only* new condition; the
*invalidation* cascade still fires for `assumes_false` polarity.
This is consistent — same artifact, both events are interesting.

### Reusing the existing cascade infrastructure

The user-spec asked whether `epistemic_cascade_outbox`'s schema can
represent a positive-direction intent with a new `reason` value
rather than a parallel table. **Yes**, this is the right call.

Current `reason` values used in the outbox (per
`internal/core/cascade_outbox.go:677`):
- `theory_disproven`
- `memory_shredded`
- `confidence_floor` (crossing the 0.3 floor downward)
- `foundation invalidated` (used in the materializer's
  `internal/core/cascade_materializer.go:289`)

Adding two new values is the minimal change:
- `foundation_proven` — fired on `ResolveTheory(proven)` for
  dependents with `polarity='assumes_false'`
- `confidence_ceiling` — fired on confidence crossing above 0.8 for
  dependents with `polarity='assumes_false'`

Both can use the existing schema:
- `dead_artifact_id` / `dead_artifact_type` — the proven artifact
- `downstream_artifact_id` / `downstream_artifact_type` — the
  dependent (unchanged)
- `reason` — the new value
- `trigger_evidence_id` — the `epistemic_provenance` row that declared
  the polarity (so the audit trail traces back to the explicit
  declaration, not inference)
- All other fields unchanged

No new table, no new schema. Same outbox, same materializer, same
wake injection path, same depth limit (`MaxCascadeDepth = 3`),
same dead-letter handling. The materializer will need a small
branch to format the re-evaluation theory's hypothesis text for
the new reasons (current hypothesis template at
`internal/core/cascade_materializer.go:289` reads from
"foundation invalidated"; the new path reads from "foundation
proven").

---

## Step 3: Empirical validation plan

The prior audit found 0 of 26 recently-proven theories had any
downstream dependent — but the absence of downstream dependents is
exactly *because* there's no polarity metadata. Operators couldn't
declare dependents-with-polarity even if they wanted to. The question
is whether the *reasoning patterns* that would benefit from this
feature exist in the corpus, even if the structured polarity data
doesn't.

### Corpus read methodology

Goal: estimate how often the existing 94 pending theories (and a
sample of resolved) contain reasoning patterns of the form "downstream
artifact's conclusion rests on upstream being false / uncertain /
unknown." This is a textual pattern match on hypothesis text, not a
structured query.

**Search patterns in hypothesis + validation_criteria text:**

1. **Explicit "if X is true / proven" reasoning** — patterns like:
   - "if X turns out to be true"
   - "if we ever confirm X"
   - "would be invalidated if X is proven"
   - "assuming X is unknown"
   - "X remains unproven"
   - "lacks evidence for X"
2. **Phrasing that explicitly hedges on upstream's truth value** —
   patterns like:
   - "may not hold if X"
   - "the conclusion depends on X being false"
   - "X is currently an open question"
3. **References to other theories/lessons with a "negation" or
   "exclusion" connotation** — patterns like:
   - "the absence of X"
   - "X has not been observed"
   - "without evidence of X"

**Scope:** all 94 currently-pending theories + 30 disproven test
theories from Cluster B (for context) + a sample of 10-20
disproven-with-substantive-conclusions theories from before Sep 5.

**Operator-execution cost:** ~30 minutes of human attention. The
patterns above are mechanical to grep; the interesting part is the
*human* judgment of "is this *actually* reasoning-against-the-
upstream, or is it just using similar phrasing for unrelated
reasons?"

**Decision threshold:** if the corpus read finds:

- **≥20% of pending theories** with a real "depends on upstream being
  false/uncertain" pattern → real evidence the feature has value;
  build after Step 3 evidence.
- **5–20%** → build but defer until after a second corpus read at
  +90 days to confirm trend; build the schema now, ship the trigger
  later.
- **<5%** → hold. The pattern is too rare to be worth the schema
  cost. Re-investigate only when one of the triggers below fires.

### Concrete definitions for the re-investigation triggers

Per the prior investigation, three triggers warrant reopening the
gap:

1. **Real incident** — a documented case where:
   - An artifact has explicit reasoning contingent on a foundation's
     unknown/false status (verifiable in hypothesis text or
     provenance).
   - The foundation later gets proven true.
   - The dependent artifact's conclusion becomes incorrect or
     becomes a stale assertion in a way an operator reports and
     traces back to the missing positive cascade.

   "Reported" here means: a human or an automated finding (e.g., a
   critic audit) flags the dependency and the trace is logged. One
   such documented case is sufficient.

2. **Dependency density shift** — the average number of explicit
   downstream dependents per proven artifact exceeds 2 (currently
   0). Measured by: `SELECT AVG(dep_count) FROM (SELECT COUNT(*) AS
   dep_count FROM epistemic_provenance WHERE polarity =
   'assumes_false' GROUP BY downstream_id)` over the trailing
   90-day window. Threshold chosen to mean "this is happening
   regularly enough to be worth the infrastructure."

3. **Recurring operator cost** — three or more distinct reports in a
   90-day window from operators describing having to manually check
   downstream dependents after a `ResolveTheory(proven)` call.
   "Distinct" meaning: three different operators or three
   different artifacts, not one operator reporting the same pain
   three times. Measured by: a manually-maintained log of such
   reports, or by detecting the pattern in operator-issued theories
   tagged `polarity='assumes_false'`-missing (a meta-tracking
   theory created when an operator notices the gap).

---

## Step 4: Test design (sketch, not implemented)

Matching the rigor of `TestEpistemicConfirmation_*` /
`TestEpistemicContradiction_*` in `internal/core/cascade_invalidation_test.go`:

1. **`TestPositiveCascade_OldShapeDependencyNeverFires`** — THE
   load-bearing safety property.
   - Set up a theory T2 with `dependencies = ["T1"]` (bare ID, the
     existing format).
   - Provenance row referencing T1 with `polarity IS NULL` (the
     default after migration).
   - Call `ResolveTheory(T1, "...", "proven")`.
   - Assert: no cascade intent for T2 is enqueued.

2. **`TestPositiveCascade_PolarityAssumesFalse_TriggersOnProven`** —
   the happy path.
   - Provenance row referencing T1 with `polarity='assumes_false'`.
   - Call `ResolveTheory(T1, "...", "proven")`.
   - Assert: cascade intent for the downstream is enqueued with
     `reason='foundation_proven'`, `dead_artifact_id=T1`,
     `trigger_evidence_id=<the provenance row id>`.

3. **`TestPositiveCascade_PolarityAssumesFalse_StillTriggersOnInvalidation`** —
   the asymmetric case.
   - Same setup as #2.
   - Call `ResolveTheory(T1, "...", "disproven")` instead.
   - Assert: cascade intent for the downstream is enqueued with
     `reason='theory_disproven'` (existing behavior, unchanged).

4. **`TestPositiveCascade_ConfidenceCeiling_TriggersOnCrossingUp`** —
   the confidence-half trigger.
   - Theory T1 with confidence 0.75 (below 0.8 ceiling).
   - Provenance row referencing T1 with `polarity='assumes_false'`.
   - Add `reproduction` evidence (strength +0.85, per the confirm
     hook at `internal/core/evidence_store.go`).
   - RecomputeConfidence crosses above 0.8.
   - Assert: cascade intent for the downstream is enqueued with
     `reason='confidence_ceiling'`.

5. **`TestPositiveCascade_NoKeywordMatching`** — the explicit-only
   invariant.
   - Theory T1 with hypothesis text containing "if X is proven
     then ...".
   - Theory T2 with hypothesis text that mentions T1 by id in its
     hypothesis (no provenance row, no dependency).
   - Call `ResolveTheory(T1, "...", "proven")`.
   - Assert: no cascade intent for T2 — text mention does not count,
     matching the `epistemic-confirmation.md` explicit-only stance.

6. **`TestPositiveCascade_AtomicityOnPartialFailure`** — same
   atomicity guarantees as the invalidation cascade.
   - Multiple dependents with `polarity='assumes_false'`.
   - Simulate one dependent's downstream cascade failing
     (depth=3 limit reached, dead-lettered).
   - Assert: the other dependents' cascade intents are still
     enqueued atomically (or rolled back consistently with the
     invalidation cascade's behavior — verify the existing
     atomicity pattern and match it).

7. **`TestPositiveCascade_PolarityMigration_BackwardCompatible`** —
   the migration test.
   - Create an `epistemic_provenance` row before the migration runs
     (no `polarity` column).
   - Run the migration (add `polarity TEXT` column with NULL default).
   - Assert: the old row has `polarity IS NULL`.
   - Run a `ResolveTheory(proven)` against an upstream the old row
     references.
   - Assert: no cascade intent — same as #1, the load-bearing
     invariant holds across the migration.

---

## Recommendation

**Hold.** Build the feature only after the Step 3 corpus read
establishes that the pattern is real and recurring (≥20% of pending
theories show "downstream reasoning contingent on upstream
uncertainty" patterns).

### Why hold

- **No observed failure mode.** The 0-of-26 audit was on the wrong
  axis (it checked for *dependents*, but the question is about
  *reasoning patterns* — a dependent might exist conceptually
  without being recorded in structured data, which is exactly what
  the corpus read is for).
- **Schema change is non-trivial.** Additive, but the typed-provenance
  pattern requires careful migration testing, and the explicit-only
  opt-in surface requires operator UX work (`RecordProvenance` would
  gain a new optional `polarity` parameter, etc.).
- **Test rigor required before any production code.** Seven test
  sketches above, including the load-bearing safety property. None
  can be skipped. The total test surface is roughly equivalent to
  the existing `TestEpistemicConfirmation_*` / `TestEpistemicContradiction_*`
  work, which was multi-day.
- **Explicit-only pattern is the right default.** Per
  `docs/epistemic-confirmation.md`, the project has consistently
  chosen explicit operator assertions over keyword matching and
  inference. A positive cascade that fires on every prove would
  violate that pattern unless operators explicitly opt in. The
  opt-in gate *is* the feature — without it, the feature is a
  regression. With it, the cost-benefit ratio depends on whether
  anyone opts in.

### The path from hold to build

If the corpus read returns ≥20% prevalence, the recommendation
flips to **build**, with the design above as the spec:

1. Implement the migration (`ALTER TABLE epistemic_provenance ADD
   COLUMN polarity TEXT CHECK (...)`).
2. Implement the positive cascade trigger in
   `internal/core/cascade_outbox.go` (new `reason` values).
3. Implement the materializer branch for the new reasons (one
   small function in `internal/core/cascade_materializer.go`).
4. Land the seven tests above as the gating regression set.
5. Document the operator surface (`RecordProvenance polarity=...`,
   `mpm_decisions.record source_ids=... polarity=...`, etc.) in the
   relevant CLI/MCP docs.

If the corpus read returns 5–20% prevalence, the recommendation is
**build the schema + tests now, defer the trigger logic until a
second corpus read at +90 days confirms trend**. The trigger logic
is small (one new condition in the existing cascade trigger path);
the schema + tests are the load-bearing work and have value
regardless of trigger timing.

If the corpus read returns <5% prevalence, **hold entirely** until
one of the three re-investigation triggers fires. The design doc
above remains valid as the spec when one does.

### On "build now" as an option

The argument for build now is "pol symmetry is cost is the goal."
That argument is wrong — symmetry is not the goal; correct epistemic
behavior is. Today's existing invalidation cascade is *not* symmetric
either: it doesn't fire on `ResolveTheory(proven)`, doesn't fire on
confidence-rising past any threshold, doesn't fire on memory-merge
events. The invalidation cascade is one specific tool for one
specific failure mode; the failure mode it addresses (downstream
built on a now-invalidated foundation) is real and observed. The
failure mode a positive cascade would address (downstream built on
uncertainty that has now resolved positively) is theoretical and
unobserved. Building for an unobserved failure mode is symmetry for
symmetry's sake.

---

## Cross-references

- `docs/archive/epistemic-cascades.md` — the deferred-feature section
  this design implements
- `docs/epistemic-confirmation.md` — the explicit-only stance this
  design follows
- `internal/core/cascade_outbox.go` — current cascade discovery
  (paths at lines 569 and 611)
- `internal/core/cascade_materializer.go` — current cascade-to-theory
  materialization (hypothesis template at line 289)
- `internal/core/epistemology_tools.go` — `ResolveTheory` at line 291
  (canonical resolution function)
- `internal/core/wake_tools.go` — confidence-related state transitions
- Commit `83c0b3a` (Sep 5) — explicit-only confirmation hook
- Commit `d8dbd92` (Sep 5) — symmetric contradiction hook
---

## Implementation status (2026-09-07)

**Status: SHIPPED.** Despite the design's HOLD recommendation, the
operator chose to build the positive-direction cascade anyway (see
commit `a0bcae0` deviation note). All four design steps are implemented
and verified end-to-end:

- **Step 1 (schema)** — `/internal/core/migration_epistemic_provenance_polarity.go`
  adds `polarity TEXT CHECK (polarity IS NULL OR polarity IN ('assumes_true', 'assumes_false'))`
  to `epistemic_provenance`. Inline CHECK, idempotent via the
  `schema_migrations` sentinel pattern. NULL default preserves the
  back-compat invariant: pre-migration rows read back with
  polarity=NULL and never fire the positive cascade.

- **Step 2 (trigger + opt-in)** — `internal/core/cascade_outbox.go`
  defines `PolarityAssumesTrue` / `PolarityAssumesFalse` constants,
  `HardConfidenceProvenThreshold = 0.8`, and the
  `EnqueueCascadeFoundationProven` helper (mirror of
  `EnqueueCascadeInvalidation`). The trigger surfaces are wired into
  `ResolveTheory(proven)` (`epistemology_tools.go`) and
  `RecomputeConfidence` confidence-ceiling crossing
  (`evidence_store.go`). The bare-dependency safety property the
  design called for is enforced by `discoverPositiveCascadeTargets`
  filtering on the typed `epistemic_provenance` rows only — the
  `dependencies` JSON path is NOT consulted for positive cascades, so
  bare-ID citations stay inert.

- **Step 3 (empirical validation)** — corpus read completed (see
  commit `dbc6f72`). Result: **0% prevalence** of the
  reasoning-against-uncertainty pattern in the 94-pending corpus.
  Per the design's decision thresholds, this would have triggered HOLD.
  The operator's deviation note (commit `a0bcae0`) explains why the
  build proceeded anyway: historical text absence doesn't evidence
  against the underlying pattern; the schema/test cost is small and
  bounded; the feature composes with the existing outbox rather than
  adding new substrate; and the explicit-only opt-in keeps the
  blast-radius at zero for any citation that doesn't actively choose
  to participate.

- **Step 4 (tests)** — `/internal/core/cascade_positive_test.go`
  implements all seven tests from the design sketch in priority
  order, with one addition (test 5 exercises the
  `confidence_ceiling` trigger end-to-end through
  `RecomputeConfidence`). The load-bearing
  `TestPositiveCascade_OldShapeDependencyNeverFires` is the first
  test in the file — any future maintainer reads the back-compat
  invariant before reading anything else.

The implementation respects every constraint the design called out:
explicit-only opt-in, NULL default for back-compat, no keyword
matching, no parallel outbox table (reuses `epistemic_cascade_outbox`
with new reason values), atomicity guarantee on partial failure,
materializer branch produces direction-appropriate hypothesis text.

Future work that the design flagged but isn't built:
- A real-incident trigger (currently no automatic mechanism to
  revisit a held decision after the underlying incident is closed).
- A recurring-operator-cost trigger (currently no metric that
  counts "operator invalidated decision X because it was
  reasoning-against-uncertainty about a now-proven theorem").
- An assumes_true trigger surface (the constant exists, the
  CHECK allows it, but no production code path writes
  `'assumes_true'` polarity — it's reserved for forward
  compatibility).

