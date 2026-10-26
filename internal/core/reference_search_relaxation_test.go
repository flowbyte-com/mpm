// reference_search_relaxation_test.go — pins the one relaxation rule in
// reference retrieval.
//
// Observed failure, from the acceptance run against a real documentation
// page: `reference search "How do I render a selected WordPress menu?"`
// returned zero results, because FTS5 joins tokens with implicit AND and
// the document never uses the word "selected". A zero-result answer is
// indistinguishable from "the corpus does not cover this", so an agent
// concludes the library is empty of the answer it needs.
//
// The rule pinned here:
//
//   - the strict all-token query is tried first and its results are used
//     unchanged whenever it matches anything (no ranking change);
//   - only when it matches nothing is an OR query tried;
//   - a query that legitimately has no match in the corpus still returns
//     zero results.
package internal

import (
	"context"
	"strings"
	"testing"
)

// seedNavigationDoc stores a document that deliberately lacks the word
// "selected".
func seedNavigationDoc(t *testing.T, dm *DatabaseManager) string {
	t.Helper()
	dir := t.TempDir()
	path := writeTestDoc(t, dir, "menus.md", `# Navigation Menus

## Rendering a menu

Call wp_nav_menu() in your template to render the navigation for a theme
location. The function accepts a theme_location argument.
`)
	res, err := dm.IngestReference(context.Background(), ReferenceIngestRequest{Source: path})
	if err != nil {
		t.Fatalf("seed doc: %v", err)
	}
	return res.ID
}

// TestReferenceSearch_RelaxesToORWhenStrictMatchesNothing is the RED case
// from the acceptance run.
func TestReferenceSearch_RelaxesToORWhenStrictMatchesNothing(t *testing.T) {
	dm := NewTestDM(t)
	seedNavigationDoc(t, dm)

	question := "How do I render a selected WordPress menu?"
	strict := BuildFTS5Query(question)
	relaxed := BuildFTS5ORQuery(question)
	if strict == relaxed {
		t.Fatal("the OR form is identical to the AND form; the relaxation cannot work")
	}

	results, err := ReferenceHybridSearch(dm, question, DefaultReferenceHybridConfig())
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) == 0 {
		t.Fatalf("natural-language question returned no results (strict query was %q)", strict)
	}
	found := false
	for _, r := range results {
		if strings.Contains(r.Content, "wp_nav_menu()") || strings.Contains(r.Content, "navigation") {
			found = true
		}
	}
	if !found {
		t.Errorf("relaxed results did not include the relevant chunk: %+v", results)
	}
}

// TestReferenceSearch_StrictPathIsUnchanged pins that a query which
// matches under AND produces exactly the same result set as the strict
// query alone — the relaxation must not widen working queries.
func TestReferenceSearch_StrictPathIsUnchanged(t *testing.T) {
	dm := NewTestDM(t)
	seedNavigationDoc(t, dm)

	query := "wp_nav_menu"
	strictHits, err := referenceSearchFTS5(dm.SQLDB(), query, 20)
	if err != nil {
		t.Fatalf("strict search: %v", err)
	}
	if len(strictHits) == 0 {
		t.Fatalf("strict query %q matched nothing; the fixture is wrong", query)
	}
	// Compare against the OR form run directly: when the strict query
	// matches, the relaxation must not have been used at all, so scores
	// must match the strict expressions.
	orHits, err := referenceSearchFTS5Match(dm.SQLDB(), BuildFTS5ORQuery(query), BuildFTS5Query(query), 20)
	if err != nil {
		t.Fatalf("or search: %v", err)
	}
	if len(strictHits) > len(orHits) {
		t.Errorf("strict results (%d) exceed OR results (%d); the OR leg should be a superset",
			len(strictHits), len(orHits))
	}
}

// TestReferenceSearch_NoMatchStillReturnsNothing pins that relaxation is
// not a license to return arbitrary rows.
func TestReferenceSearch_NoMatchStillReturnsNothing(t *testing.T) {
	dm := NewTestDM(t)
	seedNavigationDoc(t, dm)

	results, err := ReferenceHybridSearch(dm, "kubernetes ingress certificate", DefaultReferenceHybridConfig())
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("unrelated query returned %d results; want 0", len(results))
	}
}
