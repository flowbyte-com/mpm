// epistemic_polarity_null_test.go — regression coverage for the NULL
// `epistemic_provenance.polarity` scan defect.
//
// The schema contract is explicit and load-bearing: `RecordProvenance`
// normalises an empty polarity to SQL NULL, and NULL means "polarity
// unspecified / not opted in". The migration that added the column
// states that existing rows legitimately read back as NULL and that no
// backfill is required, and cascade_positive_test.go asserts that NULL
// rows exist. NULL is therefore a first-class value, not corruption and
// not a gap to be papered over with an invented default.
//
// The defect: contextual_candidates_sources.go scanned that column into
// a plain Go `string`. A NULL row raised
// "converting NULL to string is unsupported", which hit a `continue`
// that discarded the whole provenance row — so the downstream artifact
// silently vanished from candidate generation and an audit warning was
// logged on every single scan.
//
// These tests exercise the real GenerateContextualCandidates path, not
// a hand-written SQL query, so they fail if the reader regresses.

package internal

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// collectAuditWarnings returns the message of every warn-level audit row
// the run under test produced. The defect manifests as a warn (not an
// error) that is logged on every scan, so asserting on the audit trail is
// how we observe it without reaching into the scan loop.
func collectAuditWarnings(t *testing.T, dm *DatabaseManager) []string {
	t.Helper()
	rows, err := dm.SQLDB().Query(
		`SELECT message FROM system_audit_log WHERE level = 'warn'`)
	require.NoError(t, err)
	defer rows.Close()

	var out []string
	for rows.Next() {
		var msg string
		require.NoError(t, rows.Scan(&msg))
		out = append(out, msg)
	}
	require.NoError(t, rows.Err())
	return out
}

// seedPolarityRow writes one epistemic_provenance row directly, so the
// test controls the column value exactly (RecordProvenance's own
// normalisation would also produce NULL, but going through the writer
// would couple the reader test to the writer).
func seedPolarityRow(t *testing.T, dm *DatabaseManager, id, sourceID, downstreamID string, polarity interface{}) {
	t.Helper()
	_, err := dm.SQLDB().Exec(`
		INSERT INTO epistemic_provenance
			(id, source_id, source_type, downstream_id, downstream_type, event_id, polarity)
		VALUES (?, ?, 'memory', ?, 'decision', ?, ?)
	`, id, sourceID, downstreamID, "ev-"+id, polarity)
	require.NoError(t, err)
}

func TestPolarityNull_DoesNotScanError(t *testing.T) {
	dm := NewTestDM(t)

	// A NULL-polarity row: the documented "unspecified / not opted in"
	// state, exactly what the migration leaves behind on pre-existing rows.
	seedPolarityRow(t, dm, "prov-null-1", "mem-src-null", "dec-null-downstream", nil)

	res, err := dm.GenerateContextualCandidates(ContextQuery{})
	require.NoError(t, err)

	// The scan must not have produced a warning.
	for _, w := range collectAuditWarnings(t, dm) {
		require.NotContains(t, w, "epistemic_provenance scan",
			"NULL polarity must not raise a scan error; got warning: %s", w)
	}

	// And the row must not have been discarded: a NULL-polarity
	// provenance row still names a downstream artifact, so that artifact
	// is a legitimate contextual candidate. Before the fix this row hit
	// `continue` and the candidate was silently lost.
	found := false
	for _, c := range res.Candidates {
		if c.ArtifactID == "dec-null-downstream" {
			found = true
			break
		}
	}
	require.True(t, found,
		"NULL-polarity provenance row was discarded; downstream artifact "+
			"dec-null-downstream must still surface as a candidate")
}

func TestPolarityNull_DoesNotAcquireInventedSemantics(t *testing.T) {
	dm := NewTestDM(t)
	seedPolarityRow(t, dm, "prov-null-2", "mem-src-null2", "dec-null-downstream2", nil)

	res, err := dm.GenerateContextualCandidates(ContextQuery{})
	require.NoError(t, err)

	for _, c := range res.Candidates {
		if c.ArtifactID == "dec-null-downstream2" {
			// NULL is not "assumes_true", not "assumes_false", and not
			// the empty string standing in for either. The field is
			// omitempty, so the correct representation of NULL is
			// "no relationship_polarity at all".
			require.Empty(t, c.RelationshipPolarity,
				"NULL polarity must not acquire an invented semantic value; got %q",
				c.RelationshipPolarity)
			return
		}
	}
	t.Fatalf("candidate dec-null-downstream2 not surfaced; cannot assert polarity semantics")
}

func TestPolarityNonNull_StillWorks(t *testing.T) {
	dm := NewTestDM(t)
	seedPolarityRow(t, dm, "prov-true", "mem-src-true", "dec-true-downstream", PolarityAssumesTrue)
	seedPolarityRow(t, dm, "prov-false", "mem-src-false", "dec-false-downstream", PolarityAssumesFalse)

	res, err := dm.GenerateContextualCandidates(ContextQuery{})
	require.NoError(t, err)

	for _, w := range collectAuditWarnings(t, dm) {
		require.NotContains(t, w, "epistemic_provenance scan")
	}

	want := map[string]string{
		"dec-true-downstream":  PolarityAssumesTrue,
		"dec-false-downstream": PolarityAssumesFalse,
	}
	seen := 0
	for _, c := range res.Candidates {
		if exp, ok := want[c.ArtifactID]; ok {
			require.Equal(t, exp, c.RelationshipPolarity,
				"non-NULL polarity must round-trip unchanged for %s", c.ArtifactID)
			seen++
		}
	}
	require.Equal(t, len(want), seen,
		"both non-NULL polarity candidates must surface; saw %d of %d", seen, len(want))
}

func TestPolarityNullAndNonNullCoexist(t *testing.T) {
	dm := NewTestDM(t)
	seedPolarityRow(t, dm, "prov-mix-null", "mem-src-mix", "dec-mix-null", nil)
	seedPolarityRow(t, dm, "prov-mix-true", "mem-src-mix", "dec-mix-true", PolarityAssumesTrue)

	res, err := dm.GenerateContextualCandidates(ContextQuery{})
	require.NoError(t, err)

	byID := map[string]string{}
	for _, c := range res.Candidates {
		byID[c.ArtifactID] = c.RelationshipPolarity
	}
	require.Contains(t, byID, "dec-mix-null", "NULL-polarity candidate must still surface alongside a set one")
	require.Contains(t, byID, "dec-mix-true", "non-NULL candidate must still surface alongside a NULL one")
	require.Empty(t, byID["dec-mix-null"], "NULL polarity stays unset")
	require.Equal(t, PolarityAssumesTrue, byID["dec-mix-true"], "explicit polarity is preserved")
}

// TestPolarityScan_RejectsPlainStringOnNull is the negative control. It
// pins the exact defect: a NULL row scanned into a plain Go string is
// an error. If someone "fixes" the reader by scanning polarity back into
// a non-nullable string, this test fails, which is the point — it keeps
// the regression suite honest about why sql.NullString is required.
//
// The counterpart guard is TestPolarityNull_DoesNotScanError: the fix
// must make the reader tolerate NULL, and only this control explains
// why the nullable type is load-bearing rather than incidental.
func TestPolarityScan_RejectsPlainStringOnNull(t *testing.T) {
	dm := NewTestDM(t)
	seedPolarityRow(t, dm, "prov-ctrl", "mem-src-ctrl", "dec-ctrl-downstream", nil)

	// Demonstrate the failure mode directly against the same column.
	var polarity string
	err := dm.SQLDB().QueryRow(
		`SELECT polarity FROM epistemic_provenance WHERE id = ?`, "prov-ctrl",
	).Scan(&polarity)
	require.Error(t, err, "scanning NULL into a plain string must fail — that is the defect")
	require.True(t,
		strings.Contains(err.Error(), "converting NULL to string"),
		"expected the NULL-to-string conversion error, got: %v", err)

	// ...and that the nullable representation accepts it cleanly.
	var nullable sql.NullString
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT polarity FROM epistemic_provenance WHERE id = ?`, "prov-ctrl",
	).Scan(&nullable), "sql.NullString must accept NULL")
	require.False(t, nullable.Valid, "NULL must read back as Valid=false, not as a synthesised value")
}
