// service_doctor_challenged.go — observational doctor check for the
// "status='challenged'" residue. Issue #20: the 41 memories left in
// challenged state by the retired auto-challenge defect had no
// diagnostics surface, which is the worst property durable degraded
// state can have. This check makes the population count visible in
// `mpm doctor` output, with a verdict that escalates to WARN whenever
// the count is non-zero.
//
// Like every other check in service_doctor.go, this one is
// observational only: it calls s.dm.ListChallengedMemories() and
// inspects the result. No writes, no schema changes, no projection
// materialization. The list primitive reads through the same database
// handle the rest of the doctor service uses, so the post-MPM #28
// read-only contract is preserved by construction — the doctor
// service already holds the read-only handle for non-mutating
// diagnostics (see healthCheck / checkEmbeddings for the pattern).

package main

import (
	"fmt"

	internal "github.com/flowbyte-com/mpm-core"
)

// checkChallengedMemories — observational count of rows in
// status='challenged'.
//
// Status:
//   - PASS when the count is 0 (no residue; nothing to do).
//   - WARN when the count is > 0 (operator-visible durable
//     degraded state, regardless of size — even 1 row is
//     reported as WARN because the population is durably
//     reduced and the operator needs to know).
//
// Why WARN and not INFO: a count of 1 still means "a memory
// row is in a state that is invisible to retrieval and cannot
// recover without an operator decision." That is not an
// informational state; it is a degraded one. The check
// deliberately does NOT use FAIL — the database is healthy,
// the schema is healthy, the row state is exactly as the
// retired defect left it. The right response is "the operator
// should look at this," not "the system is broken."
//
// The check uses the production list primitive
// (DatabaseManager.ListChallengedMemories) rather than a raw
// SELECT against metadata. Two reasons:
//  1. The primitive already handles local+shared table
//     iteration, the same way `mpm challenge list` does.
//     Re-implementing it here would create a divergence.
//  2. The primitive is the contract other surfaces (the
//     CLI) read against. If the doctor check disagreed
//     with the CLI, the operator would lose trust in both.
func (s *DoctorService) checkChallengedMemories() DoctorCheck {
	check := DoctorCheck{Name: "Challenged memories"}

	rows, err := s.dm.ListChallengedMemories()
	if err != nil {
		check.Status = "WARN"
		check.Message = fmt.Sprintf("could not enumerate challenged memories: %v", err)
		return check
	}

	count := len(rows)
	if count == 0 {
		check.Status = "PASS"
		check.Message = "0 memories in status='challenged'"
		return check
	}

	// Break down by restore-weight-effect, using the
	// recorded prior weight as the truth source (NOT
	// current weight — a row clamped from prior 2.0 to
	// current 1.0 has a weight INCREASE pending on
	// restore, not a no-op). The four buckets are:
	//   - atFloor: prior == current (genuine no-op on weight)
	//   - wouldMoveOnRestore: prior > current (weight moves up)
	//   - noPriorWeight: !PriorWeightPresent (no prior weight;
	//     the row may still be restorable via retrieval_priority
	//     if that baseline is present)
	//   - unrestorable: !Restoreable (no prior weight AND no prior
	//     retrieval priority — the restore path refuses such rows
	//     with "cannot be reversed through MPM"; a strict subset
	//     of noPriorWeight)
	// HAL pre-PR check: the doctor must distinguish "missing
	// prior weight" from "genuinely unrestorable" because the
	// two operator responses are different. A row with no prior
	// weight but with prior retrieval_priority CAN be restored
	// (the memory half, weight-delta undefined); a row with
	// neither is unrestorable. The audit-time 41 are all in the
	// restorable population — every one of them carries
	// challenged_prior_weight.
	atFloor, wouldMoveOnRestore, noPriorWeight, unrestorable := classifyChallengeWeight(rows)

	check.Status = "WARN"
	switch {
	case noPriorWeight > 0 && unrestorable > 0:
		check.Message = fmt.Sprintf(
			"%d memories in status='challenged' (%d at the weight floor, %d would move on restore, %d with no recorded prior weight, %d unrestorable through MPM); see 'mpm challenge list'",
			count, atFloor, wouldMoveOnRestore, noPriorWeight, unrestorable,
		)
	case noPriorWeight > 0:
		check.Message = fmt.Sprintf(
			"%d memories in status='challenged' (%d at the weight floor, %d would move on restore, %d with no recorded prior weight); see 'mpm challenge list'",
			count, atFloor, wouldMoveOnRestore, noPriorWeight,
		)
	default:
		check.Message = fmt.Sprintf(
			"%d memories in status='challenged' (%d at the weight floor, %d would move on restore); see 'mpm challenge list'",
			count, atFloor, wouldMoveOnRestore,
		)
	}
	check.Details = []string{
		"Run 'mpm challenge list' to enumerate each row.",
		"Run 'mpm challenge restore <id>' to restore one at a time.",
		"Per-row semantics are documented in docs/RECALL-MUTATION.md (issue #20).",
	}
	return check
}

// classifyChallengeWeight buckets the rows by restore-weight-effect.
//
// The classification MUST key off the recorded prior weight, not
// the current weight. CurrentWeight == 1.0 alone is not a no-op
// signal: a row clamped from prior 2.0 down to current 1.0 has a
// 1.0 weight INCREASE pending on restore. Only the comparison
// between recorded prior weight and current weight tells the
// operator what restore would actually do.
//
// Four buckets:
//   - atFloor: prior == current (a real 0.0 delta; restore
//     would be a no-op on weight, only the status flag clears).
//   - wouldMoveOnRestore: prior > current (a positive weight
//     delta; restore brings the row back up).
//   - noPriorWeight: !PriorWeightPresent. This bucket is
//     coarser than "unrestorable" — a row in this bucket MAY
//     still be restorable via the retrieval_priority baseline
//     alone. Surfacing the count separately lets the operator
//     see "how many rows I cannot reason about by weight".
//   - unrestorable: !Restoreable — the strict subset of
//     noPriorWeight that also has no prior retrieval_priority.
//     The restore path refuses these rows with the existing
//     "cannot be reversed through MPM" error, and they appear
//     in `mpm challenge list` with Restoreable=false and
//     `[!] no prior` in the weight column. The pre-PR HAL check
//     required this bucket to be distinct from noPriorWeight
//     because the operator's response differs: an unrestorable
//     row is a permanent residue, a no-prior-weight row that is
//     still restorable via retrieval_priority can be acted on.
func classifyChallengeWeight(rows []internal.ChallengedMemory) (atFloor, wouldMoveOnRestore, noPriorWeight, unrestorable int) {
	for _, r := range rows {
		// Genuinely unrestorable: no prior weight AND no
		// prior retrieval priority. The restore path
		// refuses such rows outright, and the listing
		// shows them as Restoreable=false. This is the
		// strictest bucket and is a subset of noPriorWeight.
		if !r.Restoreable {
			unrestorable++
			// An unrestorable row also has no prior
			// weight by definition (Restoreable
			// requires PriorWeightPresent ||
			// PriorRetrievalPriorityPresent, and
			// failing that without RP means no prior
			// weight either), but be explicit so a
			// future refactor that changes the
			// Restoreable definition does not silently
			// undercount noPriorWeight.
			if !r.PriorWeightPresent {
				noPriorWeight++
			}
			continue
		}
		// Row is restorable. If it has no prior weight
		// recorded, fall into the noPriorWeight bucket
		// and skip the weight-delta switch — comparing
		// a real current weight to the zero default of
		// an absent prior would falsely report a
		// "weight decrease on restore" for a row that
		// simply has no recorded baseline.
		if !r.PriorWeightPresent {
			noPriorWeight++
			continue
		}
		switch {
		case r.PriorWeight == r.CurrentWeight:
			atFloor++
		case r.PriorWeight > r.CurrentWeight:
			wouldMoveOnRestore++
		default:
			// PriorWeight < CurrentWeight is not expected
			// given the floor clamp, but if it ever
			// happens, the row's restore would DECREASE
			// weight. Count it as would-move (delta != 0)
			// and let the operator see the actual values
			// in `mpm challenge list`.
			wouldMoveOnRestore++
		}
	}
	return
}
