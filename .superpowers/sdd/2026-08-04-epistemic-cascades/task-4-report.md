# Task 4 Report — Integrate explicit invalidation paths

**Plan:** `docs/superpowers/plans/2026-08-04-epistemic-cascades.md` (Task 4)
**Brief:** `.superpowers/sdd/2026-08-04-epistemic-cascades/task-4-brief.md`
**Spec:** `docs/superpowers/specs/2026-08-04-epistemic-cascades-design.md`
**Status:** SHIPPED

## Status

COMPLETE. The three explicit invalidation triggers from the design
spec (disproven theory resolution, memory shred, defined hard
confidence invalidation transition) are now wired into the cascade
outbox through a single tx-aware integration point
(`EnqueueCascadeInvalidation` on `*DatabaseManager`). Per the brief,
no materializer and no wake throttle are built — Task 5 / 6 land
those.

## Commit

- `854f5ac` — *feat: hook cascades into invalidation paths* on
  branch `worktree-agent-aaebaa877cd2e7e88`. 10 files changed,
  1144 insertions(+), 26 deletions(-).

```
854f5ac feat: hook cascades into invalidation paths
da00459 fix(cascade): address Task 3 review — semantic-key dedup, depth validation, rollback semantics
2ec8f31 feat: enqueue epistemic cascade intents
6a165ad fix: address Task 2 review — shared/local propagation, transactional atomicity, cleanup
e69ad3b feat: persist reasoning provenance edges
4214cbb fix(cascade): address task-1 review feedback
1fd7425 feat: add epistemic cascade storage
d9cad9e docs: specify epistemic cascades
```

The seven commits below `854f5ac` are the parent branch state
(Task 1 schema + Task 2 provenance + Task 3 outbox + design docs).
My Task 4 work is the single `854f5ac` commit on top. The brief
said the parent branch should be the reviewed state through
`da00459`; this commit is built on top of `da00459` and the
schema/provenance/outbox surfaces are unchanged.

## Brief requirements — coverage

- [x] **Step 1: Write failing tests** — 10 tests in
  `cascade_invalidation_test.go` covering disprove enqueue, proven
  no-op, repeated disprove idempotency, shred memory enqueue,
  lesson shred no-op, standalone shred enqueue, hard confidence
  crossing, ordinary weakening no-op, no crossing on repeated
  recompute, and atomicity (outbox failure rolls back root
  mutation).

- [x] **Step 2: Transaction-aware invalidation helper** —
  `EnqueueCascadeInvalidation(tx, deadArtifactID, deadArtifactType,
  reason, triggerEvidenceID, depth)` on `*DatabaseManager`. Mints
  the invalidation event, discovers downstream targets via the
  tx-aware variant helpers (passing the supplied tx so SQLite's
  table-locked isolation is observed without opening a second
  connection), and enqueues intents — all inside the supplied
  `*sql.Tx`.

- [x] **Step 3: ResolveTheory emits only on disprove** — refactored
  to wrap status update + reinforcement + cascade enqueue in one
  `WithTx`. The `UPDATE` filters on `status='pending'` so a real
  transition to `disproven` is the trigger; proven resolutions and
  repeated disprove calls update zero rows and skip the cascade.

- [x] **Step 4: Refactor lesson-aware and memory shred wrappers** —
  `ShredMemoryWithCascade` (lesson-aware, production path) and
  `ShredMemory` (standalone, CLI path) both enqueue cascade intents
  inside the same tx as the root DELETE. Lesson path remains
  non-cascading. Topic membership cleanup, challenged-theory
  purge, idempotency, and existing return maps are preserved
  (`cascade_intents` is added to `ShredMemoryWithCascade`'s result
  map).

- [x] **Step 5: Hard confidence threshold transition** —
  `RecomputeConfidence` detects the cross of
  `HardConfidenceInvalidationThreshold` (exported, 0.3). The
  transition detector compares old (pre-recompute read) vs new
  (post-recompute value); only the cross enqueues intents. Ordinary
  weakening and recomputes that leave the artifact below the
  threshold are no-ops. The hook only fires when `node.Tx() != nil`,
  so standalone callers (e.g., manual CLI recompute) are
  unaffected.

- [x] **Step 6: Run focused tests and commit** — `feat: hook
  cascades into invalidation paths` on branch
  `worktree-agent-aaebaa877cd2e7e88`.

## Public surface

The brief pinned `tx *sql.Tx` on the internal helpers. The
integration point is exported on the `CoreDB` interface:

```go
// cascade_outbox.go
func (dm *DatabaseManager) EnqueueCascadeInvalidation(
    tx *sql.Tx,
    deadArtifactID, deadArtifactType, reason, triggerEvidenceID string,
    depth int,
) (int, error)
```

Plus one new exported constant on the cascade outbox:

```go
// Hard confidence invalidation threshold (default 0.3).
const HardConfidenceInvalidationThreshold = 0.3
```

The constant is shared by the recompute path
(`evidence_store.go`) and the tests (`cascade_invalidation_test.go`)
so a future threshold change lands in one place.

Existing public signatures are preserved:

- `RecordProvenance(sourceID, sourceType, downstreamID, downstreamType, eventID string) error`
- `ListDownstreamCitations(sourceID string, allowedTypes []string) ([]ProvenanceCitation, error)`
- `ResolveTheory(theoryID, conclusion, newStatus string) (map[string]interface{}, error)`
- `ShredMemoryWithCascade(memoryID string) (map[string]interface{}, error)`
- `ShredMemory(id string) error`
- `RecomputeConfidence(artifactID, artifactType string) (map[string]interface{}, error)`
- `RecomputeConfidence(node DBNode, artifactID, artifactType string, reason RecomputeReason) error`

## Implementation notes

### Tx-aware variants for the discovery path

SQLite's default isolation holds an exclusive lock on the
`memories` table while a transaction is open. Reading
`discoverCascadeTargets` (or `ListDownstreamCitations`) via
`dm.db.Query` from inside a `WithTx` callback deadlocks with the
in-flight tx. The fix is structural — both helpers gain tx-aware
variants:

```go
// cascade_outbox.go
func (dm *DatabaseManager) discoverCascadeTargets(tx *sql.Tx, deadArtifactID string) ([]ProvenanceTarget, error)

// cascade_provenance.go
func (dm *DatabaseManager) listDownstreamCitationsOn(tx *sql.Tx, sourceID string, allowedTypes []string) ([]ProvenanceCitation, error)
```

When `tx != nil`, the read uses `tx.Query` (same connection,
observes in-flight state). When nil, falls through to `dm.db.Query`
for the standalone path. The public `ListDownstreamCitations`
signature is preserved (it now delegates to the tx-aware variant
with `tx=nil`). Tests in `cascade_outbox_test.go` were updated to
pass `nil` (5 call sites — `sed` migration).

The brief's "without opening a second connection" requirement is
met because both variants land on the same `*sql.Tx` the caller
already owns — no new connection is opened.

### DBNode interface extension

Two new methods on the `DBNode` interface:

```go
type DBNode interface {
    ExecTracked(query string, retries int, args ...interface{}) (sql.Result, error)
    QueryTracked(query string, args ...interface{}) (*sql.Rows, error)
    QueryRowTracked(query string, args ...interface{}) *sql.Row
    Tx() *sql.Tx        // exposes *sql.Tx for tx-pinned helpers
    DM() *DatabaseManager  // exposes owning DM for higher-level calls
}
```

- `Tx()` on `*txNode` returns the captured `*sql.Tx`; on
  `*DatabaseManager` returns `nil` (programming-error path).
- `DM()` returns the receiver for `*DatabaseManager` (identity) and
  the captured `*DatabaseManager` for `*txNode`.

The compile-time assertion `var _ DBNode = (*DatabaseManager)(nil)`
still holds, so any drift from the interface fails at build time.

### Hard confidence threshold semantics

`HardConfidenceInvalidationThreshold = 0.3` (exported) was chosen
as the structural "the artifact is no longer trustworthy" point:
well below the natural confidence range for evidence-backed memories
(0.6–0.9) so ordinary weakening doesn't cascade; well above the
no-positive-evidence asymptote (~0.12) so a single negative
evidence row doesn't accidentally trip it. Memory initial confidence
is 0.8, and one -1.0 evidence row drops to ~0.6; two rows drop to
~0.35; three rows drop to ~0.17 (crossing the floor). The test
fixture uses three `-1.0` rows to drive a deterministic crossing.

The transition detector:

```go
crossed := (!hasOldConf || oldConf >= HardConfidenceInvalidationThreshold) &&
    conf < HardConfidenceInvalidationThreshold
```

`!hasOldConf` covers the edge case where the prior confidence
read returns no row (e.g., a brand-new artifact whose confidence
column is being set for the first time inside this tx). The
short-circuit treats the prior value as "not below the floor" so
the canonical `oldConf >= threshold && new < threshold` rule
fires whenever the recompute lands the artifact below the floor
— even on the first ever recompute. `oldConf >= threshold &&
new < threshold` is the canonical cross for the steady-state
case. Repeated recomputes
below the threshold see `oldConf < threshold` and skip the
cascade.

The hook only fires when `node.Tx() != nil` so standalone callers
(`mpm ops confidence recompute`, etc.) are unaffected. The
production trigger path (`AddEvidence` → `RecomputeConfidence`
inside `WithTx`) is the surface that fires the cascade.

### Shred idempotency + atomicity

`ShredMemoryWithCascade` cascades for the memory path only:
lessons route through the existing standalone `ShredMemory(db,
id)` helper that fires the `lessons` view INSTEAD OF DELETE
trigger. The cascade intent enqueue happens BEFORE the memory
DELETE so `discoverCascadeTargets` can still observe the
dependencies JSON and `epistemic_provenance` rows pointing at
the about-to-be-deleted id.

`ShredMemory` (standalone) inlines its own DELETE rather than
calling the standalone `ShredMemory(db, id)` helper because the
helper opens its own internal tx; the cascade hook needs to
share the same tx. The lesson-aware path still routes through
the standalone helper.

The schema's UNIQUE key
`(dead_artifact_id, downstream_artifact_id, invalidation_event_id)`
collapses any re-shred against the same id to a clean no-op, so
the cascade enqueue is idempotent. The `ShredMemoryWithCascade`
result map adds `cascade_intents` (zero is a clean no-op when
no downstream dependents exist).

### `ResolveTheory` transition detector

The `UPDATE ... WHERE json_extract(metadata, '$.status') =
'pending'` clause is the transition detector. A real
`pending → disproven` transition updates 1 row; the cascade hook
fires inside the same `WithTx`. A `pending → proven` transition
also updates 1 row, but the cascade hook is gated on
`newStatus == "disproven"` so the proven path is a no-op. A
repeated `disproven → disproven` call updates 0 rows and skips
the cascade. The `+1` weight reinforcement is folded into the
same UPDATE so it participates in the transition filter (no
reinforcement on a no-op resolve).

## Test summary

10 cascade invalidation tests, all passing:

```
TestInvalidation_DisproveTheoryEnqueuesIntents                PASS
TestInvalidation_ProvenTheoryDoesNotEnqueue                   PASS
TestInvalidation_RepeatedDisproveDoesNotReEnqueue             PASS
TestInvalidation_ShredMemoryEnqueuesIntents                   PASS
TestInvalidation_ShredMemoryLessonDoesNotEnqueue              PASS
TestInvalidation_ShredMemoryStandaloneWrapperEnqueues         PASS
TestInvalidation_HardConfidenceCrossingEnqueues               PASS
TestInvalidation_OrdinaryConfidenceDecreaseDoesNotEnqueue     PASS
TestInvalidation_NoCrossingOnRepeatedRecomputeBelowThreshold  PASS
TestInvalidation_OutboxFailureRollsBackRootMutation           PASS
```

Plus regression coverage on the existing cascade-related test
suites (all pass on the refactor):

- 17 outbox tests (`TestOutbox_*`)
- 20 provenance tests (`TestProvenance_*`)
- 8 shred wrapper tests (`TestShredMemoryWithCascade_*`,
  `TestCallHelpers_ShredMemory_*`, `TestShredMemory_Standalone_*`)
- 4 cascading-decay tests (`TestCascadingDecay_*`,
  `TestStaleFoundationWake_*`)

→ **49 cascade-related tests PASS**.

Full `internal/core` suite: 608 PASS, 6 pre-existing failures
unchanged from the Task 3 baseline:

```
TestBackfillSnapshots_StampsValidationFromEvidence
TestBackfillSnapshots_ContradictedFromChallenge
TestLogWatchdog_TriggersRotationAtThreshold
TestLintRouterDirectories_CleanFiles
TestRouter_Evaluate
TestAuditSummary_KnownClusterAlsoMatchesRecentDecision
```

No new regressions introduced by this task.

## Files touched

### Created

- `/home/v/workspace/projects/mpm/.claude/worktrees/agent-aaebaa877cd2e7e88/internal/core/cascade_invalidation_test.go`
  — 540 lines, 10 tests covering disprove / proven / repeated
  disprove / shred memory / lesson shred / standalone shred /
  hard confidence crossing / ordinary weakening / repeated
  recompute no-crossing / atomicity.

### Modified

- `/home/v/workspace/projects/mpm/.claude/worktrees/agent-aaebaa877cd2e7e88/internal/core/cascade_outbox.go`
  — new `EnqueueCascadeInvalidation` helper (193 lines including
  doc comments), `HardConfidenceInvalidationThreshold` constant,
  tx-aware `discoverCascadeTargets(tx, ...)` signature.

- `/home/v/workspace/projects/mpm/.claude/worktrees/agent-aaebaa877cd2e7e88/internal/core/cascade_provenance.go`
  — `ListDownstreamCitations` now delegates to a tx-aware variant
  `listDownstreamCitationsOn(tx, ...)`. Public signature preserved.

- `/home/v/workspace/projects/mpm/.claude/worktrees/agent-aaebaa877cd2e7e88/internal/core/cascade_outbox_test.go`
  — 5 call sites updated to pass `nil` for the new `tx` parameter
  on `discoverCascadeTargets`.

- `/home/v/workspace/projects/mpm/.claude/worktrees/agent-aaebaa877cd2e7e88/internal/core/core.go`
  — `EnqueueCascadeInvalidation` added to the `CoreDB` interface.

- `/home/v/workspace/projects/mpm/.claude/worktrees/agent-aaebaa877cd2e7e88/internal/core/db.go`
  — `DBNode` interface gains `Tx()` and `DM()` methods; both
  implementations provided.

- `/home/v/workspace/projects/mpm/.claude/worktrees/agent-aaebaa877cd2e7e88/internal/core/epistemology_tools.go`
  — `ResolveTheory` refactored: status UPDATE + reinforcement +
  cascade enqueue in one `WithTx`; transition detector on
  `status='pending'`.

- `/home/v/workspace/projects/mpm/.claude/worktrees/agent-aaebaa877cd2e7e88/internal/core/evidence_store.go`
  — `RecomputeConfidence` snapshots the old confidence, computes
  the new value, detects the threshold cross, and enqueues cascade
  intents inside the same tx when the cross fires.

- `/home/v/workspace/projects/mpm/.claude/worktrees/agent-aaebaa877cd2e7e88/internal/core/memory_tools.go`
  — `ShredMemoryWithCascade` enqueues cascade intents inside the
  existing tx; `cascade_intents` added to the result map.

- `/home/v/workspace/projects/mpm/.claude/worktrees/agent-aaebaa877cd2e7e88/internal/core/web_db.go`
  — `ShredMemory` (standalone) inlines its own tx with cascade
  intent enqueue; lesson path still routes through the standalone
  `ShredMemory(db, id)` helper.

## Concerns / notes for the reviewer

1. **Threshold value choice.** `HardConfidenceInvalidationThreshold
   = 0.3` is a substrate-wide default; future patches can expose
   this as a configuration knob (e.g., a row in `system_config`).
   The constant is exported so the recompute path and the tests
   read the same number.

2. **`ShredMemory` standalone path duplicates DELETE logic.** The
   standalone `ShredMemory(db, id)` helper opens its own internal
   tx; the cascade hook needs to share the tx, so the standalone
   wrapper inlines its own DELETE + cascade enqueue + commit.
   The standalone `ShredMemory(db, id)` helper is unchanged and
   still used by the lesson path. Future refactor could split the
   standalone helper into a "with-tx callback" variant and a
   "wrapped-in-tx" variant to reduce duplication.

3. **`node.Tx() == nil` is a programming-error path, not a
   runtime error.** Standalone callers of `RecomputeConfidence`
   silently skip the cascade hook. The production trigger path
   (`AddEvidence` → `WithTx` → `RecomputeConfidence`) is the only
   surface that fires the cascade; manual CLI recompute is
   unaffected (consistent with the brief's "do not enqueue on
   ordinary weakening" policy — a manual recompute is exactly the
   kind of one-shot operation that should not silently spawn
   cascade intents).

4. **DBNode interface is now load-bearing for two consumers.**
   `Tx()` and `DM()` were added so the cascade hook can be
   reached from inside a `WithTx` callback. Any future helper
   that needs the underlying tx (e.g., the cascade materializer
   in Task 5) should follow the same pattern.

5. **Cascade intent writes on shared schema propagation.** The
   brief said shared propagation is preserved — the existing
   `EnqueueCascadeIntents` already writes to both local and
   shared inside the supplied tx, so a cascade enqueue from the
   invalidation hook propagates the intent to the shared schema
   atomically with the root mutation. This matches the Task 2
   federated contract for citation edges.

6. **Pre-existing baseline failures are unchanged.** The 6
   pre-existing failures documented in the Task 3 report persist
   after this commit (verified by full-suite run). They are
   unrelated to cascade invalidation.
