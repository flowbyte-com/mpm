# Concept Vocabulary — F8.1 Cancel / Verification

A synonym-rich anchor set for cross-session archaeology of the F8.1 alpha-p1 fix. Authored per `docs/concept-vocabulary-authoring.md`.

## Canonical title

F8.1 — Cancel / Verification Coupling (alpha-p1 blocker)

## Problem

A cancelled piece of work could carry `verification='verified'` if strong evidence existed at the moment of cancellation. Cancellation was effectively being treated as proof of completion.

## Root cause

The verification derivation was evidence-only and was not re-run when lifecycle status changed. The verification column could therefore lag behind the cancellation. Cancellation and verification were written and read independently, with no shared invariant.

## Architectural invariant

A cancelled status locks verification below `verified` regardless of evidence pattern. Verified is reserved for `open` or `done` status. Lifecycle state gates verification; verification does not imply lifecycle completion.

## Fix

The verification derivation now consults the work's status as its first check. Cancelled status locks the result below verified (contradictory evidence still surfaces as contradicted; all other patterns yield unverified). Every terminal-lifecycle transition now re-runs the derivation so the verification column can never lag the status column.

## Implementation location

`internal/core/db.go:DeriveWorkVerification`, lifecycle transitions `CancelWork` / `CancelWorkWithContext` / `CompleteWorkWithContext` / `ReopenWorkWithContext` / `UpdateWorkWithContext` / legacy `updateWorkStatus`

## Regression tests

`internal/core/f71_f81_cancellation_challenge_test.go` (17 tests including `TestF81_*`), and the rewritten `internal/core/f6_f7_f12_evidence_regression_test.go::TestF12_ReopenEvidenceChangeRecomputeIdempotent`

## Validation

831 core tests pass after the fix. 17 new regression tests added. The F12 recompute-idempotent test was rewritten because the prior version codified the F8.1 bug.

## Release impact

Test 8 — Fresh-Agent Archaeology on cancel/verification: PASS WITH ANOMALIES → PASS. Test 20 — Alpha Readiness: NOT ALPHA READY → PASS. The fix closed both release blockers.

## Concept vocabulary

These terms are anchors for paraphrased natural-language queries. They are deliberately distinct from the canonical identifier (F8.1), internal function names, and file paths recorded elsewhere.

### Problem-side terms

- cancelled work
- cancelled task
- cancelled job
- cancelled operation
- abandoned work
- abandoned task
- abandoned operation
- stopped work
- stopped task
- stopped operation
- terminated work
- terminated task
- terminated operation
- killed work
- killed task
- aborted work
- aborted task
- rejection
- undo completion
- undo verification
- false success
- false completion
- false verification
- stale verification
- verification lags status
- verification drift
- post-cancel evidence
- evidence after cancellation

### Solution-side terms

- verification lifecycle gate
- lifecycle status gate
- status first
- status gates verification
- re-derive on transition
- derivation coupled to status
- verified reserved for open or done
- post-cancel evidence cannot promote
- contradicted evidence wins
- verification cannot lag
- verification cannot drift
- structural coupling
- lifecycle verification coupling
- lifecycle trust coupling
- status-coupled verification
- lifecycle integrity
- lifecycle consistency

### Cross-domain synonyms

- a stopped task still looks successful
- an abandoned task still appears successful
- a terminated task still counted as done
- cancel implies verified
- cancel treated as completion
- cancel treated as proof
- lifecycle termination is not completion
- lifecycle termination separate from verification
- verification should not survive cancellation
- evidence cannot promote a cancelled item
- a stopped operation should not be verified
- a killed task should not be marked verified
- an aborted task should not show as done
- the system treats cancellation as success
