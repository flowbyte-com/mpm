// engine_revive_summary_test.go — the SUMMARY contract for a revived
// seeded directive.
//
// A drifted revival must never be reported as a fresh creation. The
// restore used to report it under Created — the same label used for a
// row that had never existed — so an operator whose edit had been
// destroyed was told the row was new.
//
// This file pins the bucket the drifted revival is reported under. The
// behavioural contract (the edit survives, the row is live) lives in
// engine_revive_test.go.

package seed_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/flowbyte-com/mpm-core/seed"
)

// TestApplyDirectives_DriftedRevivalIsReportedUnderRevived pins that a
// soft-deleted directive with operator content is reported under its own
// bucket, and never under Created.
func TestApplyDirectives_DriftedRevivalIsReportedUnderRevived(t *testing.T) {
	dm := newTestDM(t)

	_, err := seed.ApplyDirectives(dm)
	require.NoError(t, err)
	require.NotEmpty(t, seed.SeedDirectives, "the seed registry is empty — nothing to test")

	first := seed.SeedDirectives[0]
	_, err = dm.SQLDB().Exec(
		`UPDATE memories SET content = ? WHERE id = ?`,
		first.Content+"\n\n-- "+operatorEditMarker, first.StableID)
	require.NoError(t, err)

	softDeleteDirective(t, dm, first.StableID)

	summary, err := seed.ApplyDirectives(dm)
	require.NoError(t, err)

	require.Contains(t, summary.Revived, first.StableID,
		"a drifted revival must be reported under Revived, got Created=%v Updated=%v Revived=%v",
		summary.Created, summary.Updated, summary.Revived)
	require.NotContains(t, summary.Created, first.StableID,
		"a drifted revival must not be reported as a fresh creation")
	require.NotContains(t, summary.Updated, first.StableID,
		"a drifted revival is a revival, not a live-row drift")
}
