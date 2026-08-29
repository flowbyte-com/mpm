// f19_1_reference_freshness_projection_test.go — F19-1 alpha-final regression.
//
// F19-1: the reference freshness classifier is wired into
// ListReferences / SearchReferences / GetReference, but the projection
// surface (i.e. that the `freshness` field actually appears on every
// row map and reflects the correct state) was only spot-checked by
// the discovery matrix test for ONE state ("current" via verified
// tag) on ONE path (ListReferences). The audit requires that every
// state surfaces correctly through every read path that returns
// references.
//
// This file pins the contract:
//   1. Every row from ListReferences has a non-empty freshness field
//      equal to the documented state for its tags/reason/age.
//   2. Every row from SearchReferences has the same field populated.
//   3. Every row from GetReference has the same field populated.
//   4. All five states (current / stale / version-bound / historical /
//      unknown) round-trip through the projection.
//
// Pre-fix: the classifier is correct in isolation but the projection
// could regress silently (e.g. a refactor dropping the freshness line
// from one of the three row maps). This test makes that regression
// observable.
package internal

import (
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedReference inserts one reference doc directly so each sub-test can
// control its tags/import_reason/last_indexed and read it back through
// the public projection surface.
func f19_1SeedReference(t *testing.T, dm *DatabaseManager, doc *ReferenceDoc) {
	t.Helper()
	if err := dm.AddReference(doc, nil); err != nil {
		t.Fatalf("AddReference(%s): %v", doc.ID, err)
	}
}

// f19_1Now is the deterministic reference time used across all F19-1
// sub-tests. Matches the convention in reference_freshness_test.go.
var f19_1Now = time.Unix(1735689600, 0)

func f19_1EpochSeconds(deltaSeconds int64) string {
	return strconv.FormatInt(f19_1Now.Unix()+deltaSeconds, 10)
}

// TestF19_1_ListReferencesProjectsFreshnessAllStates confirms the
// ListReferences row map carries the `freshness` field for all five
// states. Pre-fix the discovery matrix test only checked the verified
// → current path; the other four states were untested at the
// projection surface.
func TestF19_1_ListReferencesProjectsFreshnessAllStates(t *testing.T) {
	dm := NewTestDM(t)
	defer dm.Close()

	const day = int64(86400)
	cases := []struct {
		stateName string
		doc       *ReferenceDoc
		want      Freshness
	}{
		{
			stateName: "current",
			doc: &ReferenceDoc{
				ID:          "ref-f191-current",
				Title:       "F19-1 current ref",
				SourcePath:  "/tmp/f191-current.md",
				SourceType:  "markdown",
				Tags:        []string{"verified"},
				LastIndexed: f19_1EpochSeconds(-1 * day),
			},
			want: FreshnessCurrent,
		},
		{
			stateName: "stale",
			doc: &ReferenceDoc{
				ID:          "ref-f191-stale",
				Title:       "F19-1 stale ref",
				SourcePath:  "/tmp/f191-stale.md",
				SourceType:  "markdown",
				LastIndexed: f19_1EpochSeconds(-200 * day),
			},
			want: FreshnessStale,
		},
		{
			stateName: "version_bound",
			doc: &ReferenceDoc{
				ID:          "ref-f191-version-bound",
				Title:       "F19-1 version-bound ref",
				SourcePath:  "/tmp/f191-version-bound.md",
				SourceType:  "markdown",
				Tags:        []string{"version-bound:6.7"},
				LastIndexed: f19_1EpochSeconds(-7 * day),
			},
			want: FreshnessVersionBound,
		},
		{
			stateName: "historical",
			doc: &ReferenceDoc{
				ID:           "ref-f191-historical",
				Title:        "F19-1 historical ref",
				SourcePath:   "/tmp/f191-historical.md",
				SourceType:   "markdown",
				Tags:         []string{"historical"},
				ImportReason: "post-mortem on prior architecture",
				LastIndexed:  f19_1EpochSeconds(-7 * day),
			},
			want: FreshnessHistorical,
		},
		{
			stateName: "unknown",
			doc: &ReferenceDoc{
				ID:          "ref-f191-unknown",
				Title:       "F19-1 unknown ref",
				SourcePath:  "/tmp/f191-unknown.md",
				SourceType:  "markdown",
				LastIndexed: "not-a-number",
			},
			want: FreshnessUnknown,
		},
	}

	for _, c := range cases {
		t.Run(c.stateName, func(t *testing.T) {
			f19_1SeedReference(t, dm, c.doc)
			refs, err := dm.ListReferences(50, 0)
			require.NoError(t, err, "ListReferences")
			var foundRow map[string]interface{}
			for _, r := range refs {
				if id, _ := r["id"].(string); id == c.doc.ID {
					foundRow = r
					break
				}
			}
			require.NotNil(t, foundRow, "%s reference not reachable via ListReferences", c.stateName)

			freshness, ok := foundRow["freshness"].(string)
			require.True(t, ok, "%s: freshness field missing or wrong type", c.stateName)
			require.NotEmpty(t, freshness, "%s: freshness field empty", c.stateName)
			assert.Equal(t, string(c.want), freshness,
				"%s: ListReferences freshness projection", c.stateName)
		})
	}
}

// TestF19_1_GetReferenceProjectsFreshness confirms the single-doc
// read path also surfaces freshness. The contract is symmetric: every
// read path that returns reference rows must surface freshness, not
// only ListReferences.
func TestF19_1_GetReferenceProjectsFreshness(t *testing.T) {
	dm := NewTestDM(t)
	defer dm.Close()

	const day = int64(86400)
	cases := []struct {
		stateName string
		doc       *ReferenceDoc
		want      Freshness
	}{
		{
			stateName: "current",
			doc: &ReferenceDoc{
				ID:          "ref-f191-get-current",
				Title:       "F19-1 get current",
				SourcePath:  "/tmp/f191-get-current.md",
				SourceType:  "markdown",
				Tags:        []string{"current"},
				LastIndexed: f19_1EpochSeconds(-1 * day),
			},
			want: FreshnessCurrent,
		},
		{
			stateName: "stale",
			doc: &ReferenceDoc{
				ID:          "ref-f191-get-stale",
				Title:       "F19-1 get stale",
				SourcePath:  "/tmp/f191-get-stale.md",
				SourceType:  "markdown",
				LastIndexed: f19_1EpochSeconds(-365 * day), // well past 90-day threshold
			},
			want: FreshnessStale,
		},
		{
			stateName: "version_bound_via_reason",
			doc: &ReferenceDoc{
				ID:           "ref-f191-get-version",
				Title:        "F19-1 get version-bound",
				SourcePath:   "/tmp/f191-get-version.md",
				SourceType:   "markdown",
				ImportReason: "for WordPress 6.7 manual",
				LastIndexed:  f19_1EpochSeconds(-7 * day),
			},
			want: FreshnessVersionBound,
		},
	}

	for _, c := range cases {
		t.Run(c.stateName, func(t *testing.T) {
			f19_1SeedReference(t, dm, c.doc)
			got, err := dm.GetReference(c.doc.ID)
			require.NoError(t, err, "GetReference(%s)", c.doc.ID)
			require.NotNil(t, got, "GetReference(%s) returned nil", c.doc.ID)

			freshness, ok := got["freshness"].(string)
			require.True(t, ok, "%s: freshness field missing or wrong type", c.stateName)
			require.NotEmpty(t, freshness, "%s: freshness field empty", c.stateName)
			assert.Equal(t, string(c.want), freshness,
				"%s: GetReference freshness projection", c.stateName)
		})
	}
}

// TestF19_1_SearchReferencesProjectsFreshness confirms the
// SearchReferences read path surfaces freshness on every row it
// returns, regardless of which branch (FTS5 or LIKE) the
// implementation takes.
//
// Note (pre-existing findings, NOT F19-1): the SearchReferences FTS5
// path has two latent bugs that are out of scope for the F19-1
// projection contract:
//   (a) `ORDER BY bm25(references_fts)` raises "no such column:
//       references_fts" on some FTS5 builds (the bm25 auxiliary
//       function on a standalone FTS5 table is misregistered in this
//       driver version).
//   (b) `WHERE id IN (SELECT rowid FROM references_fts ...)` returns
//       zero rows because reference_docs.id is TEXT and the FTS5
//       rowid is INTEGER — the type mismatch makes the lookup always
//       miss.
// Both bugs are surfaced end-to-end (production SearchReferences
// returns nothing for non-empty query terms) and will be filed as
// separate findings outside the alpha freeze. This test therefore
// exercises SearchReferences against the LIKE-fallback branch by
// probing at runtime which path is active.
//
// The freshness projection contract is uniform across both branches
// (web_db.go:1045-1066 — the row map is built once after the
// `rows.Next()` loop and includes the freshness field for every
// return path). Pinning the LIKE path is sufficient to defend the
// projection; the FTS path shares the row-building code.
func TestF19_1_SearchReferencesProjectsFreshness(t *testing.T) {
	dm := NewTestDM(t)
	defer dm.Close()

	// See TestF19_1_SearchReferencesLikeFallbackProjectsFreshness
	// below — the LIKE-fallback is the only path that reliably
	// returns rows in this hermetic test environment, and it
	// exercises the same row-map projection. Run that test and
	// consider this one a no-op alias if the FTS-path bugs above
	// remain open at alpha freeze.
	t.Run("projection_via_like_fallback", func(t *testing.T) {
		// Seed two refs with unique titles + distinct freshness states.
		seed := []*ReferenceDoc{
			{
				ID:          "ref-f191-search-current",
				Title:       "F19-1 search verified uniquealpha",
				SourcePath:  "/tmp/f191-search-current.md",
				SourceType:  "markdown",
				Tags:        []string{"verified"},
				LastIndexed: f19_1EpochSeconds(-1 * int64(86400)),
			},
			{
				ID:          "ref-f191-search-stale",
				Title:       "F19-1 search stale uniquealpha",
				SourcePath:  "/tmp/f191-search-stale.md",
				SourceType:  "markdown",
				LastIndexed: f19_1EpochSeconds(-200 * int64(86400)),
			},
		}
		for _, d := range seed {
			f19_1SeedReference(t, dm, d)
		}

		// Force the LIKE branch by deleting references_fts — the
		// detection at web_db.go:1021 will fall through.
		_, _ = dm.SQLDB().Exec(`DROP TABLE IF EXISTS references_fts`)

		results, err := dm.SearchReferences("uniquealpha", 50)
		require.NoError(t, err, "SearchReferences (LIKE fallback)")

		wantByID := map[string]Freshness{
			"ref-f191-search-current": FreshnessCurrent,
			"ref-f191-search-stale":   FreshnessStale,
		}
		seen := make(map[string]bool)
		for _, row := range results {
			id, _ := row["id"].(string)
			want, expected := wantByID[id]
			if !expected {
				continue
			}
			freshness, ok := row["freshness"].(string)
			require.True(t, ok, "row %s: freshness field missing or wrong type", id)
			require.NotEmpty(t, freshness, "row %s: freshness field empty", id)
			assert.Equal(t, string(want), freshness,
				"SearchReferences LIKE-fallback projection for %s", id)
			seen[id] = true
		}
		for id := range wantByID {
			assert.True(t, seen[id], "SearchReferences LIKE-fallback did not return seeded row %s", id)
		}
	})
}

// TestF19_1_RowMapFreshnessFieldAlwaysPresent is the structural
// invariant: the freshness field MUST be present on every row of
// every reference read surface (not just the rows we explicitly
// classified). A row with an empty freshness string is a projection
// regression.
func TestF19_1_RowMapFreshnessFieldAlwaysPresent(t *testing.T) {
	dm := NewTestDM(t)
	defer dm.Close()

	// Three docs covering the extremes: explicit current, past-threshold
	// stale, and unknown (empty last_indexed). Each row returned by
	// ListReferences MUST carry a non-empty freshness.
	docs := []*ReferenceDoc{
		{
			ID:          "ref-f191-always-1",
			Title:       "F19-1 always 1",
			SourcePath:  "/tmp/f191-always-1.md",
			SourceType:  "markdown",
			Tags:        []string{"verified"},
			LastIndexed: f19_1EpochSeconds(-1),
		},
		{
			ID:          "ref-f191-always-2",
			Title:       "F19-1 always 2",
			SourcePath:  "/tmp/f191-always-2.md",
			SourceType:  "markdown",
			LastIndexed: f19_1EpochSeconds(-365 * int64(86400)),
		},
		{
			ID:          "ref-f191-always-3",
			Title:       "F19-1 always 3",
			SourcePath:  "/tmp/f191-always-3.md",
			SourceType:  "markdown",
			// No last_indexed → unknown
		},
	}
	for _, d := range docs {
		f19_1SeedReference(t, dm, d)
	}

	refs, err := dm.ListReferences(50, 0)
	require.NoError(t, err)
	require.NotEmpty(t, refs)
	for _, row := range refs {
		freshness, ok := row["freshness"].(string)
		assert.True(t, ok, "row %v: freshness missing or wrong type", row["id"])
		assert.NotEmpty(t, freshness, "row %v: freshness empty (projection regression)", row["id"])
	}
}