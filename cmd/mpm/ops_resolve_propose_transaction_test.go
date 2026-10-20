// ops_resolve_propose_transaction_test.go — the TRANSACTION contract of
// `mpm ops resolve-contradictions --propose` (issue #21, Phase B
// corrective commit).
//
// The contract is about CERTAINTY, and it is the part of the command an
// operator has to be able to trust:
//
//   - candidates_proposed counts CONFIRMED COMMITTED rows. It is zero
//     unless the transaction confirmed its commit.
//   - A failed COMMIT whose ROLLBACK was CONFIRMED is a CONFIRMED
//     ROLLBACK, not an uncertain outcome. SQLite refuses to roll back a
//     transaction that has already committed, so a successful rollback
//     after a failed commit PROVES the commit did not take effect.
//   - A failed ROLLBACK is identified explicitly, WHATEVER triggered it,
//     and the outcome is uncertain: once the rollback has failed, nothing
//     proves the queue is unchanged.
//   - The human output and the JSON output agree about all of it.
//
// The end-to-end tests drive the real transaction through the core's
// fault-injection seam, so what is asserted is what the transaction
// actually did — not a synthetic TransactionResult.
//
// Every test here runs against a disposable database under t.TempDir(). The
// live MPM database is never opened.

package main

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
	"github.com/stretchr/testify/require"
)

// ── a failed COMMIT with a confirmed ROLLBACK ──────────────────────

// A failed COMMIT whose ROLLBACK was confirmed is a CONFIRMED ROLLBACK.
//
// The commit failed, so the batch is not in the queue — and the
// rollback that followed succeeded, which is the proof: SQLite refuses
// to roll back a transaction that has already committed. Reporting this
// as an uncertain outcome would tell the operator to go and check a queue
// that is provably unchanged.
func TestResolveContradictions_FailedCommitWithConfirmedRollbackIsAConfirmedRollback(t *testing.T) {
	dm := setupResolveProposeTest(t)

	seedProposeMemory(t, dm, "", "c-a", "the deploy pipeline runs on tuesday", unitVector(1, 0, 0))
	seedProposeMemory(t, dm, "", "c-b", "the deploy pipeline runs on tuesdays", unitVector(1, 0, 0))

	// The COMMIT statement fails; the ROLLBACK that follows is the real
	// one and succeeds.
	mpminternal.SetProposalTxFaultsForTest(
		func(context.Context, *sql.Conn) error { return errors.New("injected commit failure") },
		nil,
	)
	t.Cleanup(func() { mpminternal.SetProposalTxFaultsForTest(nil, nil) })

	var out string
	var code int
	captureUserErrorOutput(t, func() {
		out = captureStdout(t, func() {
			code = handleOpsResolveContradictions([]string{"--json", "--propose"})
		})
	})
	require.Equal(t, 1, code, "a failed commit is a failure")

	env := decodeSingleEnvelope(t, out)
	propose := envelopeObject(t, env, "propose")
	require.Equal(t, "db_commit_rolled_back", envelopeString(t, propose, "completion"))
	require.Equal(t, "rolled_back", envelopeString(t, propose, "commit_state"))
	require.Equal(t, 0.0, envelopeNumber(t, propose, "candidates_proposed"),
		"an unconfirmed write is not a write")
	require.Nil(t, propose["rollback_error"], "the rollback succeeded")
	require.Contains(t, envelopeString(t, propose, "error"), "injected commit failure")

	// The queue is provably unchanged.
	queue, err := dm.CountContradictionQueue()
	require.NoError(t, err)
	require.Equal(t, 0, queue)
}

// ── a failed ROLLBACK ──────────────────────────────────────────────

// A failed ROLLBACK leaves the outcome uncertain, and says so.
//
// The commit failed and then the rollback failed, so nothing proves
// whether the batch reached the queue. The completion names the rollback
// failure explicitly, the commit state is "uncertain", and the rollback
// error is preserved alongside the primary error.
func TestResolveContradictions_FailedRollbackLeavesTheOutcomeUncertain(t *testing.T) {
	dm := setupResolveProposeTest(t)

	seedProposeMemory(t, dm, "", "u-a", "the deploy pipeline runs on tuesday", unitVector(1, 0, 0))
	seedProposeMemory(t, dm, "", "u-b", "the deploy pipeline runs on tuesdays", unitVector(1, 0, 0))

	mpminternal.SetProposalTxFaultsForTest(
		func(context.Context, *sql.Conn) error { return errors.New("injected commit failure") },
		func(context.Context, *sql.Conn) error { return errors.New("injected rollback failure") },
	)
	t.Cleanup(func() { mpminternal.SetProposalTxFaultsForTest(nil, nil) })

	var out string
	var code int
	captureUserErrorOutput(t, func() {
		out = captureStdout(t, func() {
			code = handleOpsResolveContradictions([]string{"--json", "--propose"})
		})
	})
	require.Equal(t, 1, code)

	env := decodeSingleEnvelope(t, out)
	propose := envelopeObject(t, env, "propose")
	require.Equal(t, "rollback_failed", envelopeString(t, propose, "completion"))
	require.Equal(t, "uncertain", envelopeString(t, propose, "commit_state"))
	require.Equal(t, 0.0, envelopeNumber(t, propose, "candidates_proposed"))

	// BOTH errors are preserved.
	rollbackErr, ok := propose["rollback_error"].(string)
	require.True(t, ok, "the rollback error must be present")
	require.Contains(t, rollbackErr, "injected rollback failure")
	combined := envelopeString(t, propose, "error")
	require.Contains(t, combined, "injected commit failure")
	require.Contains(t, combined, "injected rollback failure")
}

// A failed ROLLBACK is identified even when a BUDGET refusal triggered it.
//
// This is the case that is easiest to get wrong. The refusal itself is
// clean — nothing was written — and the classification would be
// "write_budget" if the rollback had succeeded. But the rollback is the
// operation that PROVES nothing was written, and once it has failed
// nothing proves it. The outcome is uncertain and the report must say so.
func TestResolveContradictions_RollbackFailureAfterABudgetRefusalIsStillIdentified(t *testing.T) {
	dm := setupResolveProposeTest(t)

	for i := 0; i < 6; i++ {
		seedProposeMemory(t, dm, "", "b-"+itoa(i),
			"the workspace must use pnpm for every install", unitVector(1, 0, 0))
	}

	mpminternal.SetProposalTxFaultsForTest(
		nil,
		func(context.Context, *sql.Conn) error { return errors.New("injected rollback failure") },
	)
	t.Cleanup(func() { mpminternal.SetProposalTxFaultsForTest(nil, nil) })

	var out string
	var code int
	captureUserErrorOutput(t, func() {
		out = captureStdout(t, func() {
			// 15 pairs qualify; the write budget of 5 refuses the batch
			// INSIDE the transaction, so the rollback is attempted.
			code = handleOpsResolveContradictions(
				[]string{"--json", "--propose", "--write-budget=5"})
		})
	})
	require.Equal(t, 1, code)

	env := decodeSingleEnvelope(t, out)
	propose := envelopeObject(t, env, "propose")
	require.Equal(t, "rollback_failed", envelopeString(t, propose, "completion"),
		"a failed rollback dominates the budget refusal that triggered it")
	require.Equal(t, "uncertain", envelopeString(t, propose, "commit_state"))
	require.Equal(t, 0.0, envelopeNumber(t, propose, "candidates_proposed"))

	rollbackErr, ok := propose["rollback_error"].(string)
	require.True(t, ok)
	require.Contains(t, rollbackErr, "injected rollback failure")
	require.Contains(t, envelopeString(t, propose, "error"), "injected rollback failure")
}

// ── the human path agrees with the JSON path ──────────────────────

// The human output and the JSON output agree about transaction
// certainty.
//
// An operator reading only the terminal, and a script reading only the
// envelope, must reach the same conclusion about whether the queue
// changed. The uncertain case is the dangerous one: "nothing was written"
// is a claim about durable state, and it must not be printed when the
// state is unknown.
func TestResolveContradictions_HumanAndJSONAgreeAboutTransactionCertainty(t *testing.T) {
	dm := setupResolveProposeTest(t)

	seedProposeMemory(t, dm, "", "h-a", "the deploy pipeline runs on tuesday", unitVector(1, 0, 0))
	seedProposeMemory(t, dm, "", "h-b", "the deploy pipeline runs on tuesdays", unitVector(1, 0, 0))

	mpminternal.SetProposalTxFaultsForTest(
		func(context.Context, *sql.Conn) error { return errors.New("injected commit failure") },
		func(context.Context, *sql.Conn) error { return errors.New("injected rollback failure") },
	)
	t.Cleanup(func() { mpminternal.SetProposalTxFaultsForTest(nil, nil) })

	var stderr string
	var code int
	stdout := captureStdout(t, func() {
		stderr = captureUserErrorOutput(t, func() {
			code = handleOpsResolveContradictions([]string{"--propose"})
		})
	})
	require.Equal(t, 1, code)

	require.Contains(t, stdout, "Proposal did NOT complete (rollback_failed)",
		"the human path must name the failure the JSON path names")
	require.Contains(t, strings.ToLower(stdout), "verify the queue",
		"the human path must say the outcome is unknown")
	require.NotContains(t, stdout, "Nothing was written",
		"the human path must not claim nothing was written when it is unknown")
	require.Contains(t, stderr, "injected commit failure")
	require.Contains(t, stderr, "injected rollback failure")
}

// ── the classification function ────────────────────────────────────

// A failed ROLLBACK dominates every other classification.
//
// The rollback is the last thing that can go wrong, and its failure is
// the one fact that decides whether the queue's state is knowable. A
// classification that ignored it would name a clean refusal for a
// transaction whose outcome is unknown.
func TestClassifyProposalFailure_FailedRollbackDominatesEveryOtherClassification(t *testing.T) {
	tx := mpminternal.TransactionResult{
		CommitState:   mpminternal.CommitStateUncertain,
		RollbackError: errors.New("injected rollback failure"),
	}

	// A validation refusal whose rollback failed.
	require.Equal(t, "rollback_failed", classifyProposalFailure(tx,
		&mpminternal.ErrInvalidCandidate{
			MemoryA:    "mem-a",
			MemoryB:    "mem-b",
			Underlying: errors.New("self-contradiction"),
		}))

	// A budget refusal whose rollback failed.
	require.Equal(t, "rollback_failed", classifyProposalFailure(tx,
		&mpminternal.ErrWriteBudgetExceeded{Eligible: 3, WriteBudget: 1}))

	// An unusable budget whose rollback failed.
	require.Equal(t, "rollback_failed", classifyProposalFailure(tx,
		&mpminternal.ErrInvalidWriteBudget{WriteBudget: 0, Candidates: 3}))

	// A queue that could not be reached, whose rollback failed.
	require.Equal(t, "rollback_failed", classifyProposalFailure(tx,
		&mpminternal.ErrSharedUnavailable{}))

	// An insert failure whose rollback failed.
	require.Equal(t, "rollback_failed", classifyProposalFailure(tx,
		&mpminternal.ErrProposalInsertion{Phase: "insert", Underlying: errors.New("disk full")}))
}

// A failed COMMIT with a CONFIRMED rollback is a confirmed rollback, not
// an uncertain commit outcome.
func TestClassifyProposalFailure_FailedCommitWithConfirmedRollbackIsNotUncertain(t *testing.T) {
	err := &mpminternal.ErrProposalInsertion{
		Phase:      "commit",
		Underlying: errors.New("injected commit failure"),
	}
	tx := mpminternal.TransactionResult{CommitState: mpminternal.CommitStateRolledBack}

	require.Equal(t, "db_commit_rolled_back", classifyProposalFailure(tx, err))
}

// A failed COMMIT with a FAILED rollback is uncertain, and the rollback
// failure is what identifies it.
func TestClassifyProposalFailure_FailedCommitWithFailedRollbackIsUncertain(t *testing.T) {
	err := &mpminternal.ErrProposalInsertion{
		Phase:      "commit",
		Underlying: errors.New("injected commit failure"),
	}
	tx := mpminternal.TransactionResult{
		CommitState:   mpminternal.CommitStateUncertain,
		RollbackError: errors.New("injected rollback failure"),
	}

	require.Equal(t, "rollback_failed", classifyProposalFailure(tx, err))
}

// The failure detail preserves BOTH the primary error and the rollback
// error.
//
// Dropping either would leave the reader with a claim the evidence does
// not support: the primary error alone hides the uncertainty, and the
// rollback error alone hides what went wrong.
func TestProposalFailureDetail_PreservesThePrimaryAndTheRollbackError(t *testing.T) {
	primary := errors.New("injected commit failure")
	rollback := errors.New("injected rollback failure")

	detail := proposalFailureDetail(primary, rollback)
	require.Contains(t, detail, "injected commit failure")
	require.Contains(t, detail, "injected rollback failure")

	require.Equal(t, "injected commit failure", proposalFailureDetail(primary, nil))
	require.Equal(t, "rollback failed: injected rollback failure",
		proposalFailureDetail(nil, rollback))
	require.Equal(t, "", proposalFailureDetail(nil, nil))
}

// ── the human summary ──────────────────────────────────────────────

// A failed rollback is named in the human summary, and nothing is
// claimed about the queue.
func TestPrintProposalSummary_RollbackFailureIsNamedAndNothingIsNotClaimed(t *testing.T) {
	summary := proposeSummary{
		Requested:             true,
		EntriesScanned:        10,
		CandidatesFoundInScan: 2,
		Completion:            "rollback_failed",
		CommitState:           "uncertain",
		RollbackError:         strPtr("injected rollback failure"),
	}

	out := captureStdout(t, func() {
		printProposalSummary(summary)
	})

	require.Contains(t, out, "rollback_failed")
	require.NotContains(t, out, "Nothing was written")
	require.Contains(t, strings.ToLower(out), "verify the queue")
}

// A confirmed rollback says that nothing was written, and names the
// phase that failed.
func TestPrintProposalSummary_ConfirmedRollbackAfterAFailedCommitSaysNothingWasWritten(t *testing.T) {
	summary := proposeSummary{
		Requested:             true,
		EntriesScanned:        10,
		CandidatesFoundInScan: 2,
		Completion:            "db_commit_rolled_back",
		CommitState:           "rolled_back",
	}

	out := captureStdout(t, func() {
		printProposalSummary(summary)
	})

	require.Contains(t, out, "db_commit_rolled_back")
	require.Contains(t, out, "Nothing was written")
}
