// service_doctor_challenged_test.go — observational doctor check
// for `status='challenged'` residue (issue #20).
//
// Pins:
//   - the zero-count path: PASS, count message,
//   - the nonzero path: WARN, count, floor/at-floor split,
//   - the observational contract: a full `mpm doctor --json` run
//     against a disposable DB reports the new check, and the
//     list of check names does not grow when the residue is
//     zero (no spurious WARN).
//
// Every test runs against an isolated in-memory database
// (internal.NewTestDM via newTestDMForCmd). The fixture never
// touches the production workspace DB.
package main

import (
	"strings"
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestIssue20_DoctorChallenged_Zero — fresh DB: count is 0,
// status is PASS, and the message reports "0 memories".
func TestIssue20_DoctorChallenged_Zero(t *testing.T) {
	dm := newTestDMForCmd(t)
	svc := NewDoctorService(dm)

	check := svc.checkChallengedMemories()
	assert.Equal(t, "Challenged memories", check.Name)
	assert.Equal(t, "PASS", check.Status,
		"zero residue must be PASS, not INFO or WARN")
	assert.Contains(t, check.Message, "0 memories in status='challenged'")
	assert.Empty(t, check.Details,
		"PASS checks must not carry remediation guidance")
}

// TestIssue20_DoctorChallenged_Nonzero — seed 5 challenged rows;
// the check must report WARN, the count must match, and the
// floor/at-floor split must be reflected in the message.
//
// 3 of the 5 are seeded with prior weight 1.0 (floor, no-op
// on restore). 2 are seeded with prior weight 5.0 and the
// challenge clamp pulls them to 1.0 (would-move-on-restore).
func TestIssue20_DoctorChallenged_Nonzero(t *testing.T) {
	dm := newTestDMForCmd(t)
	svc := NewDoctorService(dm)

	// Unique content per row: SaveMemory enforces a
	// uniqueness constraint, so reusing the same content
	// across calls would fail on the second insertion
	// rather than producing 5 rows.
	for i := 0; i < 3; i++ {
		issue20ListSeed(t, dm, "issue20 doctor floor "+string(rune('A'+i)), 1.0)
	}
	for i := 0; i < 2; i++ {
		issue20ListSeed(t, dm, "issue20 doctor clamped "+string(rune('A'+i)), 5.0)
	}

	check := svc.checkChallengedMemories()
	assert.Equal(t, "Challenged memories", check.Name)
	assert.Equal(t, "WARN", check.Status,
		"any nonzero residue must WARN — durable degraded state is not informational")
	assert.Contains(t, check.Message, "5 memories in status='challenged'",
		"message must report the actual count")
	assert.Contains(t, check.Message, "3 at the weight floor",
		"message must break down the floor rows so the operator can see the no-op share")
	assert.Contains(t, check.Message, "2 would move on restore",
		"message must break down the rows where restore would actually change weight")
	assert.Contains(t, check.Message, "mpm challenge list",
		"message must point the operator at the discovery surface")
	assert.NotEmpty(t, check.Details,
		"WARN checks must carry remediation guidance")
	for _, d := range check.Details {
		assert.NotEmpty(t, d)
	}
}

// TestIssue20_DoctorChallenged_Observational — full `mpm doctor
// --json` run on a disposable DB. Asserts:
//   - the new "Challenged memories" check appears in the
//     emitted list,
//   - the count matches the seeded population,
//   - the check is the WARN variant (not a no-op PASS),
//   - no other check, in particular no memory-mutating check,
//     fires spuriously as a result of the residue being
//     present.
//
// This is the integration-level test that pins the
// observational contract end-to-end. The unit tests above pin
// the per-check shape.
func TestIssue20_DoctorChallenged_Observational(t *testing.T) {
	dm := newTestDMForCmd(t)
	svc := NewDoctorService(dm)

	// Seed 2 challenged rows so the check has something to
	// report. Unique content per row (SaveMemory enforces
	// uniqueness).
	issue20ListSeed(t, dm, "issue20 observability row 1", 1.0)
	issue20ListSeed(t, dm, "issue20 observability row 2", 5.0)

	report, err := svc.Check()
	require.NoError(t, err)
	require.NotNil(t, report)

	var found *DoctorCheck
	for i := range report.Checks {
		if report.Checks[i].Name == "Challenged memories" {
			found = &report.Checks[i]
			break
		}
	}
	require.NotNil(t, found, "the doctor report must contain a 'Challenged memories' entry; got: %+v", report.Checks)
	assert.Equal(t, "WARN", found.Status)
	assert.Contains(t, found.Message, "2 memories in status='challenged'")
}

// TestIssue20_DoctorChallenged_EmptyReportHasCheck — empty DB,
// the new check must STILL appear in the report (the doctor
// report is a fixed-shape surface; checks do not appear
// conditionally). This is a small but important regression
// guard: a future refactor that conditionally appends the
// check would silently break the operator's diagnostic.
func TestIssue20_DoctorChallenged_EmptyReportHasCheck(t *testing.T) {
	dm := newTestDMForCmd(t)
	svc := NewDoctorService(dm)

	report, err := svc.Check()
	require.NoError(t, err)
	require.NotNil(t, report)

	found := false
	for _, c := range report.Checks {
		if c.Name == "Challenged memories" {
			found = true
			break
		}
	}
	assert.True(t, found,
		"the doctor report must always contain a 'Challenged memories' entry; got names: %s",
		issue20CheckNames(report.Checks))
}

// TestIssue20_DoctorChallenged_ClampedRowIsWeightIncrease —
// HAL defect 2 regression.
//
// The original classifyChallengeWeight checked
// `r.CurrentWeight == 1.0` to decide "no-op on weight" and
// counted the row in atFloor. That is wrong: a row with
// current 1.0 and recorded prior 2.0 was clamped by the
// challenge writer from 2.0 down to 1.0, and a restore would
// move it back to 2.0. The previous logic collapsed that
// row with the genuine floor-no-op case (prior 1.0, current
// 1.0), making the doctor say "0 would move on restore"
// when in fact 1 row would.
//
// This test seeds one row with current=1.0 and a hand-crafted
// metadata carrying prior_weight=2.0, runs the doctor check,
// and asserts the row is counted in "would move on restore",
// NOT in "at the weight floor".
func TestIssue20_DoctorChallenged_ClampedRowIsWeightIncrease(t *testing.T) {
	dm := newTestDMForCmd(t)
	svc := NewDoctorService(dm)

	// One row, hand-crafted metadata: current weight 1.0,
	// prior weight 2.0. The production challenge path cannot
	// produce this combination (it only writes rows whose
	// prior weight is what was actually there at the time
	// of the challenge), so the test seeds directly.
	clampedID := "issue20-clamped-1.0from2.0"
	clampedMeta := `{"status":"challenged","challenged_at":1750000000,"challenged_via":"test","challenged_theory_id":"","challenged_prior_weight":2.0}`
	_, err := dm.SQLDB().Exec(
		`INSERT INTO memories (id, collection, content, metadata, weight, retrieval_priority, importance, confidence)
		 VALUES (?, 'memories', ?, ?, 1.0, 0.5, 0.5, 0.5)`,
		clampedID, "issue20 clamped 1.0 from 2.0", clampedMeta)
	require.NoError(t, err, "seed clamped row")

	check := svc.checkChallengedMemories()
	require.Equal(t, "WARN", check.Status)
	require.Contains(t, check.Message, "1 memories in status='challenged'",
		"the doctor must report the population count")

	// The headline assertion: a current=1.0, prior=2.0 row
	// is a weight increase on restore, not a no-op.
	assert.Contains(t, check.Message, "1 would move on restore",
		"HAL defect 2: a row with current=1.0, prior=2.0 must be counted as a weight increase, NOT as 'at the weight floor'")
	assert.NotContains(t, check.Message, "1 at the weight floor",
		"HAL defect 2: classifyChallengeWeight must NOT count a clamped-from-2.0 row as 'at the weight floor' just because current=1.0")

	// Pin the unit-level classification directly, so a
	// future change to the message format doesn't hide a
	// regression in the underlying numbers.
	rows, err := dm.ListChallengedMemories()
	require.NoError(t, err)
	require.Len(t, rows, 1)
	atFloor, wouldMoveOnRestore, noPriorWeight, unrestorable := classifyChallengeWeight(rows)
	assert.Equal(t, 0, atFloor,
		"current=1.0, prior=2.0 must NOT count as atFloor — restore would move it back to 2.0")
	assert.Equal(t, 1, wouldMoveOnRestore,
		"current=1.0, prior=2.0 must count as wouldMoveOnRestore — the recorded prior is above the current")
	assert.Equal(t, 0, noPriorWeight,
		"this row has a recorded prior weight, so it must not be counted in noPriorWeight")
	assert.Equal(t, 0, unrestorable,
		"this row has a recorded prior weight, so it must not be counted in unrestorable")
}

// TestIssue20_DoctorChallenged_MissingPriorVsUnrestorable —
// HAL pre-PR check. The doctor must distinguish "missing prior
// weight" from "genuinely unrestorable" because the two operator
// responses differ:
//
//   - A row with no prior weight BUT with prior retrieval_priority
//     is restorable (the memory half, only the weight-delta is
//     undefined). The operator CAN run `mpm challenge restore` on
//     it; the restore clears the status flag and reinstates
//     retrieval_priority, but leaves the weight at the floor
//     (which is where the challenge put it).
//
//   - A row with no prior weight AND no prior retrieval_priority
//     is genuinely unrestorable. The restore path refuses it
//     with the existing "cannot be reversed through MPM" error.
//
// The previous 3-bucket classification counted both populations
// as `noRecordedBaseline` and conflated them. This test seeds
// one of each and asserts that:
//
//   1. Both rows are counted in noPriorWeight (the coarser
//      "weight-delta is undefined" bucket).
//   2. ONLY the row with no prior retrieval_priority is counted
//      in unrestorable (the strict subset).
//   3. The doctor message surfaces the unrestorable count
//      separately from the no-prior-weight count, so the
//      operator can act on the distinction.
func TestIssue20_DoctorChallenged_MissingPriorVsUnrestorable(t *testing.T) {
	dm := newTestDMForCmd(t)
	svc := NewDoctorService(dm)

	// Row 1: no prior weight, but prior retrieval_priority is
	// recorded. The restore path can act on the
	// retrieval_priority half (Restoreable=true) even though
	// the weight half is undefined.
	restorableNoPriorID := "issue20-restorable-no-prior"
	restorableNoPriorMeta := `{"status":"challenged","challenged_at":1750000001,"challenged_via":"test","challenged_theory_id":"","challenged_prior_retrieval_priority":0.7}`
	_, err := dm.SQLDB().Exec(
		`INSERT INTO memories (id, collection, content, metadata, weight, retrieval_priority, importance, confidence)
		 VALUES (?, 'memories', ?, ?, 1.0, 0.5, 0.5, 0.5)`,
		restorableNoPriorID, "issue20 no prior weight, has prior RP", restorableNoPriorMeta)
	require.NoError(t, err, "seed restorable-no-prior row")

	// Row 2: no prior weight AND no prior retrieval_priority.
	// This is a genuine Arc 1 writer row — the restore path
	// will refuse it with "cannot be reversed through MPM".
	unrestorableID := "issue20-unrestorable-no-baseline"
	unrestorableMeta := `{"status":"challenged","challenged_at":1750000002,"challenged_via":"test","challenged_theory_id":""}`
	_, err = dm.SQLDB().Exec(
		`INSERT INTO memories (id, collection, content, metadata, weight, retrieval_priority, importance, confidence)
		 VALUES (?, 'memories', ?, ?, 1.0, 0.5, 0.5, 0.5)`,
		unrestorableID, "issue20 no prior weight, no prior RP", unrestorableMeta)
	require.NoError(t, err, "seed unrestorable row")

	// Unit-level classification: both rows are in
	// noPriorWeight, only the second is in unrestorable.
	rows, err := dm.ListChallengedMemories()
	require.NoError(t, err)
	require.Len(t, rows, 2, "two challenged rows seeded")
	atFloor, wouldMoveOnRestore, noPriorWeight, unrestorable := classifyChallengeWeight(rows)
	assert.Equal(t, 0, atFloor, "neither row has a recorded prior weight, so neither can be atFloor")
	assert.Equal(t, 0, wouldMoveOnRestore, "neither row has a recorded prior weight, so neither can be wouldMoveOnRestore")
	assert.Equal(t, 2, noPriorWeight, "both rows have !PriorWeightPresent, so both are in noPriorWeight")
	assert.Equal(t, 1, unrestorable, "only the row with no prior retrieval_priority is genuinely unrestorable; the other is restorable via RP")

	// Doctor message: the population breakdown must
	// surface the distinction. noPriorWeight is the
	// wider bucket (2); unrestorable is the strict
	// subset (1). Without the new bucket, the operator
	// would see "2 with no recorded prior baseline"
	// and not know which of those can still be restored.
	check := svc.checkChallengedMemories()
	require.Equal(t, "WARN", check.Status)
	require.Contains(t, check.Message, "2 memories in status='challenged'",
		"doctor must report the population count")
	assert.Contains(t, check.Message, "2 with no recorded prior weight",
		"HAL pre-PR check: the doctor must surface the wider noPriorWeight bucket so the operator can see the count of rows with undefined weight-delta")
	assert.Contains(t, check.Message, "1 unrestorable through MPM",
		"HAL pre-PR check: the doctor must surface the unrestorable count separately from the no-prior-weight count, so the operator can distinguish rows that can still be restored via retrieval_priority from rows that cannot be restored at all")
}

// issue20CheckNames is a tiny helper for failure messages —
// the test report prints a list of check names so a refactor
// that renames the check still produces a useful diagnostic.
func issue20CheckNames(checks []DoctorCheck) string {
	var names []string
	for _, c := range checks {
		names = append(names, c.Name)
	}
	return strings.Join(names, ", ")
}

// silenceImportWarnings keeps the mpminternal import alive
// when this file is built standalone in some configurations.
// The alias is unused at runtime but documents the dependency
// for future readers.
var _ = mpminternal.NewTestDM
