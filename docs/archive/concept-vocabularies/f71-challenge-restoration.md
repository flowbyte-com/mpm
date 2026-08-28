# Concept Vocabulary — F7.1 Challenge / Restoration

A synonym-rich anchor set for cross-session archaeology of the F7.1 alpha-p1 fix. Authored per `docs/concept-vocabulary-authoring.md`.

## Canonical title

F7.1 — Challenge / Restoration (alpha-p1 blocker)

## Problem

A challenged memory's confidence could remain at its pre-challenge high value even after the evidence that produced that confidence was invalidated. Restoring the memory therefore silently re-promoted trust without any new supporting evidence.

## Root cause

The challenge operation set a status flag and reduced weight, but did not neutralize the underlying evidence rows or drop confidence. Restoration reverted weight but left confidence untouched, allowing the prior high confidence to flow forward unchanged.

## Architectural invariant

A challenged memory cannot retain pre-challenge high confidence. Restoration does not silently re-promote trust. Fresh evidence is required to re-elevate a restored memory above the challenge floor.

## Fix

Inside one transaction the challenge operation neutralizes existing evidence rows (preserving them in the table but marking them expired) and drops the memory's confidence to the neutral floor. Restoration clears operational flags and restores prior weight, but explicitly does not re-elevate confidence — it leaves the memory at the floor.

## Implementation location

`internal/core/db.go:ChallengeMemory`, `cmd/mpm/handlers_challenge.go:runChallenge`, `cmd/mpm/handlers_challenge.go:runChallengeRestore`

## Regression tests

`internal/core/f71_f81_cancellation_challenge_test.go` (17 tests including `TestF71_*`)

## Validation

831 core tests pass after the fix. 17 new regression tests added. The F11 challenge suite continues to pass after the handler rewrite.

## Release impact

Test 7 — Fresh-Agent Archaeology on challenge/restoration: CRITICAL FAILURE → PASS. The fix closed the alpha release blocker on challenge-restoration.

## Concept vocabulary

These terms are anchors for paraphrased natural-language queries. They are deliberately distinct from the canonical identifier (F7.1), internal function names, and file paths recorded elsewhere — those are reachable by other surfaces.

### Problem-side terms

- challenged memory
- challenge restoration
- challenging a memory
- restoring a memory
- restoring challenged information
- restored from challenge
- challenge banner
- challenge status
- obsolete memory
- disputed memory
- invalidated memory
- trust re-promotion
- silent trust re-promotion
- silent re-verification
- false trust
- stale evidence
- invalidated evidence
- evidence neutralization
- confidence restoration
- confidence remains high
- pre-challenge confidence
- post-challenge confidence

### Solution-side terms

- evidence neutralization
- challenge floor
- neutral confidence
- forensic trail
- audit trail preservation
- fresh evidence required
- fresh supporting evidence
- challenged prior weight
- challenged prior confidence
- restoration must not promote
- restoration removes operational flag
- weight restoration
- weight not confidence

### Cross-domain synonyms

- dispute a memory
- mark a memory as wrong
- undo a challenge
- revert a challenge
- lift a challenge
- revive a challenged memory
- recall a memory's status
- clear a dispute
- memory's evidence invalidated
- memory evidence expired
- confidence dropped to neutral
- memory loses its trust
- memory regains trust only on proof
- memory cannot be trusted again automatically
