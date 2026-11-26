# Continuity-001 Recovery Implementation — Report

Baseline: `3c1f7f85` (Merge PR #268, CI green).
Contract: frozen v6 plus C1/C2 amendments (design phase closed; no retuning).

## Scope discipline

- No merge performed by this implementation; stops at the independent review gate.
- Frozen pilot (`/home/v/projects/mpm-continuity-001-pilot`) untouched and disabled.
- No live database migration, no real supersession/cancellation side effects
  outside disposable test databases, no inference or provider calls,
  no dispatcher enablement, no production installation.
- No updater/scheduler changes; no modification of `/home/v/src/mpm`;
  no Skills P8 implementation.

## What was built

- Supersession service (`internal/core/assignment/supersession.go`):
  lineage-first guarded write, deferred FKs, budget-conditioned child
  inserts, idempotency, `ErrSupersessionConflict` field naming,
  `ErrReservedKeyCollision`, A4 codes.
- Claim/Renew/Deliver/Acknowledge/Accept/Start/Complete/Cancel/Refuse/
  Handoff/RecordApproval/BeginAttempt/Execute/ExecuteReview guards per the
  lifecycle matrix (TR11 + T1/T3/T42/T67/T85, R13 service/storage parity).
- Trigger DDL (`migration_agent_task_supersessions*.go`): TR1–TR20 enforced
  on INSERT and UPDATE (21 trigger statements), global-scope alias fix,
  restored OwnAmb arm, corrected stale-reviewer predicate, TR11 narrowed to
  the ambiguity arm, canonical-body comparison.
- Restore pipeline (`restore_canonical/classify/rows/validate/plan.go`):
  canonical inventory + normalisation, table/sentinel classification,
  missing/modified/duplicate/unexpected verification, dependency-ordered
  resequencing, semantic + temporal validator (T105–T109, T112–T127, C1/C2),
  pre-destructive refusal with the source database untouched.
- CLI: `mpm assignment supersede` (operator-only, absent from the agent
  tool registry); `show` now returns the execution-aware view
  (`effective_state`); `tree` returns the `TreeView` (`tree_status`,
  `recovery_status`, per-task `effective_state`, immutable lineages);
  new `lineage` verb (by id, or by superseded+attempt); `dispatch-status`
  items carry `effective_state` and the report carries per-tree `TreeStatus`.
- Read surfaces share one implementation with every guard
  (`EffectiveState`/`TreeStatus`): no second opinion.

## Verification

- `internal/core/assignment`: green (includes 30 supersession service tests,
  read-view/lineage tests, T94/T96-T97/T20/T5 arms).
- `internal/core/dispatch`: green (includes early-review T5 refusal with
  zero runtime launches, status effective-state/tree agreement).
- `cmd/mpm` restore + CLI subsets: green (T91 multi-generation round-trip,
  T92 missing-dependency/cycle, T93, T105–T108, T112–T117, T120–T127,
  C2, misordered-claim, CLI read verbs).
- Full `internal/core ./...`: every package green except one pin,
  `TestControls_ThePublishedCountsAreInTheDocuments`, which counted 77 test
  functions in the four suites against the published 76 — the drift is the
  new `TestAssignmentCLIReadVerbsCarryExecutionState` CLI test, so the pin
  constant and the design note's two current-count statements move 76→77
  (round narratives untouched). Re-run green.
- Full `cmd/mpm` (`-timeout 20m`, the Makefile's `CMD_TEST_TIMEOUT`):
  all green except `TestTheStorePublicationCarriesTheNewestReview`, a stale
  engineering test that recorded verdicts without host-captured evidence —
  refused by the T84 rule the contract requires. Updated to the contract
  (capture-then-verdict, as the file's own sibling tests do); the test's
  original point (newest review published) is unchanged. Full re-run
  executed; result recorded in the PR body. (Note: the default 10m `go test`
  timeout is insufficient for this suite — the build-identity/provenance
  tests compile binaries repeatedly — so a 10m run's timeout is an artifact
  of the invocation, not a hang.)
- `agent_installation` (586 tests): 1 failure —
  `test_build_output_directory_exists` — which fails identically on a clean
  `3c1f7f85` worktree without `make build` (environmental, pre-existing).
  The retired-host check passes on both trees.

## Traceability (tests → contract)

- T5: `TestAnEarlyReviewIsRefusedBeforeTheRuntimeRunsIsT5` (dispatch),
  `TestEarlyReviewRefusedBeforeRuntimeIsT5`.
- T10/T21/T74: `TestUnrelatedTreeUnaffectedIsT10`,
  `TestRootCancelAllowedUnderAmbiguityIsT21T74`.
- T20: `TestNoteAllowedOnBlockedWorkIsT20` (note atomicity: state/epoch
  unchanged, ledger appends `note`).
- T30/T34/T35/T37/T38/T40: supersede refusal/idempotency/fault-rollback
  tests.
- T52/T55/T56: `TestReadOriginalAfterSupersessionIsT52`,
  `TestRecoveryReachesCompleteIsT55T56`.
- T58/T71/T72/T82/T84/T88/T89: reserved keys, budget, evidence, legacy
  shapes, active intent.
- T91: `TestRestoreMultiGenerationRoundTripIsT91`.
- T92: `TestRestoreMissingDependencyIsT92`, `TestRestoreCycleIsRefusedT92`.
- T93: `TestRestoreReservedKeyWithoutLineageIsT93`.
- T94: `TestStartOnTerminalIsT94`.
- T96/T97: `TestOrdinaryWritesCarryNoBaseShapeIsT96T97`.
- T101/T102/T103/T104: reviewer cancel/verdict/evidence tests.
- T105–T109: `TestRestoreLegacyShapesAreRefusedT105`,
  `TestRestoreCompletedWithoutSuccessIsT106DB`,
  `TestRestoreAmbiguityConflictIsT107DB`,
  `TestRestorePreAmbiguitySiblingAcceptedIsT108`.
- T112/T113/T115/T116: trigger verification tests.
- T117: `TestRestorePreFeaturePR3IsT117`, `TestRestorePreFeaturePR1IsT117b`.
- T120/T121/T122: schema/collision tests.
- T123/T124/T125: `TestRestoreScopedBoundsAreEnforced` (replacement /
  obsolete-reviewer / historical-descendant).
- T126: `TestRestoreAttemptBeforeAuthorisationIsT126`.
- C2: `TestRestorePostSupersessionOnOriginalIsC2`.
- T127 (+C1 indeterminate): `TestRestoreIndeterminateOrderingIsT127`,
  `TestRestoreMisorderedClaimIsRefused`.
- T129: `TestRestoreScopedAttemptInBoundsIsT129`.
- Read surfaces: `TestReadViewsCarryExecutionState`,
  `TestStatusCarriesEffectiveStateAndTrees`,
  `TestAssignmentCLIReadVerbsCarryExecutionState`.

## Request

Independent review of this implementation against the frozen contract.
No merge, no deploy, no pilot changes requested.
