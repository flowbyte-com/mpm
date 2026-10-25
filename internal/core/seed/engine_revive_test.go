// engine_revive_test.go — the restore path's contract for a
// soft-deleted seeded directive.
//
// The seed contract has always been "ApplyDirectives never overwrites a
// local edit". The LIVE-row path honoured it: a directive whose content
// diverged from the seed kept the operator's text and was flagged.
//
// The RESTORE path did not: reviving a soft-deleted directive overwrote
// content, tags and metadata with the seed's values, so an operator who
// had edited a baseline directive lost the edit on the next seed run —
// silently — and the summary reported the row as freshly "Created", the
// same label used for a row that had never existed.
//
// These tests pin the four states a seeded directive can be in:
//
//	pristine + live       → skipped
//	modified + live       → drifted, the edit preserved
//	pristine + deleted    → revived, the seed content restored
//	modified + deleted    → revived, the operator's content preserved
//
// Every test uses a disposable database. See engine_test.go for the same
// pattern.

package seed_test

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/flowbyte-com/mpm-core/seed"
)

const operatorEditMarker = "OPERATOR EDIT: tighter phrasing for this workspace."

// softDeleteDirective soft-deletes one seeded directive, the way a decay
// sweep or an accidental shred would.
func softDeleteDirective(t *testing.T, dm interface {
	SQLDB() *sql.DB
}, stableID string) {
	t.Helper()
	_, err := dm.SQLDB().Exec(
		`UPDATE memories SET deleted_at = 1786610894 WHERE id = ?`, stableID)
	require.NoError(t, err)
}

// TestApplyDirectives_RevivePristineRestoresTheSeedContent pins that a
// soft-deleted directive whose content still matches the seed is
// restored exactly.
//
// Nothing was customised, so nothing is lost: the baseline comes back
// verbatim and the row is reported under Created.
func TestApplyDirectives_RevivePristineRestoresTheSeedContent(t *testing.T) {
	dm := newTestDM(t)

	_, err := seed.ApplyDirectives(dm)
	require.NoError(t, err)
	require.NotEmpty(t, seed.SeedDirectives, "the seed registry is empty — nothing to test")

	first := seed.SeedDirectives[0]
	softDeleteDirective(t, dm, first.StableID)

	summary, err := seed.ApplyDirectives(dm)
	require.NoError(t, err)

	var content string
	var deletedAt *int64
	err = dm.SQLDB().QueryRow(
		`SELECT content, deleted_at FROM memories WHERE id = ?`, first.StableID,
	).Scan(&content, &deletedAt)
	require.NoError(t, err, "the revived directive must exist")
	require.Nil(t, deletedAt, "the revived directive must be live")
	require.Equal(t, first.Content, content, "a pristine directive must be restored exactly")

	require.Contains(t, summary.Created, first.StableID,
		"a pristine revival is reported under Created")
}

// TestApplyDirectives_RevivePreservesTheOperatorEdit pins that reviving
// a soft-deleted directive does NOT destroy the operator's customised
// content.
//
// This is the defect: the operator edited a baseline directive, the
// directive was later soft-deleted, and the next seed run overwrote the
// edit without saying so.
func TestApplyDirectives_RevivePreservesTheOperatorEdit(t *testing.T) {
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

	var content string
	var deletedAt *int64
	err = dm.SQLDB().QueryRow(
		`SELECT content, deleted_at FROM memories WHERE id = ?`, first.StableID,
	).Scan(&content, &deletedAt)
	require.NoError(t, err, "the revived directive must exist")
	require.Nil(t, deletedAt, "the revived directive must be live again")
	require.Contains(t, content, operatorEditMarker,
		"the operator's edit must survive the restore, got content=%q", content)

	// The report must NOT claim a fresh creation for a row that carried
	// operator content.
	require.NotContains(t, summary.Created, first.StableID,
		"a drifted revival must not be reported as a fresh creation")
}

// TestApplyDirectives_RevivePreservesTheOperatorTags pins that the
// operator's tags survive the restore too.
//
// The restore overwrote content, tags AND metadata with the seed's
// values. Content is the visible part; tags are the part that decides
// which directives an agent surfaces.
func TestApplyDirectives_RevivePreservesTheOperatorTags(t *testing.T) {
	dm := newTestDM(t)

	_, err := seed.ApplyDirectives(dm)
	require.NoError(t, err)
	require.NotEmpty(t, seed.SeedDirectives, "the seed registry is empty — nothing to test")

	first := seed.SeedDirectives[0]
	_, err = dm.SQLDB().Exec(
		`UPDATE memories SET tags = ? WHERE id = ?`,
		`["operator-custom-tag"]`, first.StableID)
	require.NoError(t, err)

	softDeleteDirective(t, dm, first.StableID)

	_, err = seed.ApplyDirectives(dm)
	require.NoError(t, err)

	var tags string
	err = dm.SQLDB().QueryRow(
		`SELECT tags FROM memories WHERE id = ?`, first.StableID,
	).Scan(&tags)
	require.NoError(t, err)
	require.Contains(t, tags, "operator-custom-tag",
		"the operator's tags must survive the restore, got tags=%q", tags)
}

// TestApplyDirectives_ReviveReindexesFTSAndRecordsARevision pins that
// the restore still fires the FTS reindex and the memory-revision
// trigger.
//
// The UNDELETE is an UPDATE, and both of those triggers hang off it. If
// the restore stopped being an UPDATE, the revived directive would be
// live but invisible to search, and the revision ledger would lose the
// transition.
func TestApplyDirectives_ReviveReindexesFTSAndRecordsARevision(t *testing.T) {
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

	_, err = seed.ApplyDirectives(dm)
	require.NoError(t, err)

	// The FTS index must carry the revived row.
	var indexed int
	err = dm.SQLDB().QueryRow(`
		SELECT COUNT(*) FROM memories_fts
		WHERE rowid = (SELECT rowid FROM memories WHERE id = ?)`,
		first.StableID,
	).Scan(&indexed)
	require.NoError(t, err)
	require.Equal(t, 1, indexed, "the revived directive must be reindexed for search")

	// The revision ledger must carry the un-delete transition.
	var revisions int
	err = dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM memory_revisions WHERE memory_id = ?`, first.StableID,
	).Scan(&revisions)
	require.NoError(t, err)
	require.GreaterOrEqual(t, revisions, 2,
		"the restore must record a revision (the edit plus the un-delete)")
}
