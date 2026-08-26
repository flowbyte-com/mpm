// fts5_query_test.go — regression tests for the FTS5 query builder.
//
// Specifically: hyphenated compounds and multi-word queries that hit the
// porter unicode61 tokenizer's split-on-hyphen behaviour. The pre-fix
// SearchLessons query treated `lessons_fts` as a column of the `lessons`
// view (it isn't), so the FTS5 path raised "no such column" and silently
// fell back to LIKE. The 2026-07-23 sessions have to fix both the query
// construction (BuildFTS5Query) and the SQL wiring (JOIN on lessons.rowid).

package internal

import (
	"strings"
	"testing"
)

func TestBuildFTS5Query_TokenizationMatchesIndexer(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"single word", "ecryptfs", "ecryptfs*"},
		{"hyphenated compound", "lazy-start", "lazy* start*"},
		{"space-separated", "lazy start", "lazy* start*"},
		{"three-word hyphenated", "lazy-start-mount", "lazy* start* mount*"},
		{"underscore compound", "lazy_start", "lazy* start*"},
		{"dot compound", "foo.bar", "foo* bar*"},
		{"technical identifier with slash+digits", "HTTP/1.1", "HTTP* 1* 1*"},
		{"comma separated terms", "alpha, beta", "alpha* beta*"},
		{"in-word apostrophe kept", "don't panic", "don't* panic*"},
		{"extra whitespace", "  hello   world  ", "hello* world*"},
		{"mixed separators", "lazy-start_mix.dot", "lazy* start* mix* dot*"},
		{"empty", "", ""},
		{"whitespace only", "   \t\n  ", ""},
		// unicode61 keeps in-word apostrophes as token characters; every
		// other FTS5-syntactic special (^ " ( ) : *) is now a separator.
		{"only FTS5-syntactic specials", "\"'^()*:", ""},
		// Substring inside a word is fine; FTS5 prefix-matches via the *.
		{"prefix wildcard applies", "ecrypt", "ecrypt*"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := BuildFTS5Query(tc.input)
			if got != tc.want {
				t.Errorf("BuildFTS5Query(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

// TestBuildFTS5Query_HyphenQueryableLessons is the end-to-end regression
// for the 2026-07-23 incident: a lesson tagged "lazy-start-architecture"
// was unreachable via search_lessons because the FTS5 query silently
// failed at the SQL layer (no such column: lessons_fts on the view). The
// fix is two-fold: (1) BuildFTS5Query tokenizes the user's hyphenated
// query the same way the index does, and (2) SearchLessons JOINs on
// lessons_fts.rowid instead of treating `lessons_fts` as a view column.
func TestBuildFTS5Query_HyphenQueryableLessons(t *testing.T) {
	dm, err := NewDatabaseManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewDatabaseManager failed: %v", err)
	}
	defer dm.Close()
	if _, err := dm.db.Exec("DELETE FROM lessons"); err != nil {
		t.Fatalf("clear lessons failed: %v", err)
	}
	if _, err := dm.db.Exec("DELETE FROM lessons_fts"); err != nil {
		t.Fatalf("clear lessons_fts failed: %v", err)
	}

	dm.AddLesson("Lazy-start architecture: fix at the agent wake layer, not the systemd layer", LessonTypeInsight, []string{"lazy-start-architecture", "ecryptfs", "agent-wake-fix"}, "")
	dm.AddLesson("Use gofmt for Go code formatting", LessonTypePractice, []string{"go", "style"}, "")

	cases := []struct {
		name  string
		query string
		want  int
	}{
		{"hyphenated compound", "lazy-start", 1},
		{"hyphenated tag", "lazy-start-architecture", 1},
		{"space-separated", "lazy start", 1},
		{"single keyword", "ecryptfs", 1},
		{"unrelated keyword", "gofmt", 1},
		{"empty query returns no results", "", 0},
		{"non-matching word", "xyzzy-no-match", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			results, err := dm.SearchLessons(tc.query, 10)
			if err != nil {
				t.Fatalf("SearchLessons(%q) failed: %v", tc.query, err)
			}
			if len(results) != tc.want {
				got := make([]string, len(results))
				for i, r := range results {
					got[i] = r.Content
				}
				t.Errorf("SearchLessons(%q) returned %d results, want %d: %v", tc.query, len(results), tc.want, got)
			}
		})
	}
}

// TestSearchLessons_FTS5PathLive explicitly verifies the FTS5 path is
// actually being exercised (not the LIKE fallback). The check: a query
// stemmed via Porter must match a document whose ONLY word is the stem.
// LIKE cannot match this; only FTS5 with Porter stemming can.
func TestSearchLessons_FTS5PathLive(t *testing.T) {
	dm, err := NewDatabaseManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewDatabaseManager failed: %v", err)
	}
	defer dm.Close()
	if _, err := dm.db.Exec("DELETE FROM lessons"); err != nil {
		t.Fatalf("clear lessons failed: %v", err)
	}
	if _, err := dm.db.Exec("DELETE FROM lessons_fts"); err != nil {
		t.Fatalf("clear lessons_fts failed: %v", err)
	}

	// "running" stems to "run" via Porter. The lesson only contains the
	// word "run" — a LIKE '%running%' would NOT match. FTS5 with Porter
	// stems both sides and matches. If this test passes, the FTS5 path
	// is alive (and the LIKE fallback is NOT being hit).
	dm.AddLesson("Always run the test suite before claiming a fix is shipped", LessonTypeInsight, []string{"testing"}, "")
	dm.AddLesson("Use gofmt for Go code formatting", LessonTypePractice, []string{"go", "style"}, "")

	results, err := dm.SearchLessons("running", 10)
	if err != nil {
		t.Fatalf("SearchLessons(running) failed: %v", err)
	}
	if len(results) != 1 {
		t.Errorf("SearchLessons(running) = %d results, want 1 (Porter-stemmed FTS5 match); FTS5 path is likely falling back to LIKE", len(results))
	}
	for _, r := range results {
		if r.Content == "Use gofmt for Go code formatting" {
			t.Errorf("SearchLessons(running) returned the wrong lesson: %q", r.Content)
		}
	}
}

// TestSearchLessons_EmptyQuery guards against the LIKE fallback returning
// all lessons when the query is empty. Pre-fix: empty query caused LIKE
// '%' which matches everything. Post-fix: empty query returns no results.
func TestSearchLessons_EmptyQuery(t *testing.T) {
	dm, err := NewDatabaseManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewDatabaseManager failed: %v", err)
	}
	defer dm.Close()
	if _, err := dm.db.Exec("DELETE FROM lessons"); err != nil {
		t.Fatalf("clear lessons failed: %v", err)
	}
	if _, err := dm.db.Exec("DELETE FROM lessons_fts"); err != nil {
		t.Fatalf("clear lessons_fts failed: %v", err)
	}

	dm.AddLesson("Always run the test suite", LessonTypeInsight, []string{"testing"}, "")

	results, err := dm.SearchLessons("", 10)
	if err != nil {
		t.Fatalf("SearchLessons(\"\") failed: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("SearchLessons(\"\") = %d results, want 0 (empty query must not return all lessons)", len(results))
	}
}

// TestBuildFTS5Query_SpecialCharactersStripped guards against FTS5
// syntax errors reaching the SQL layer. The previous fix wrapped in
// double quotes; the new fix strips the FTS5 syntactic specials at
// the helper. Mid-string `*` is treated as a regular character and
// stripped; the trailing `*` is added back per token.
func TestBuildFTS5Query_SpecialCharactersStripped(t *testing.T) {
	// Syntactic specials that would cause FTS5 to error or silently
	// no-op if they reach MATCH as literal characters (the trailing `*`
	// is the per-token prefix wildcard so it is allowed).
	syntacticSpecials := "\"^():"
	cases := []struct {
		in   string
		want string
	}{
		{`"hello"`, "hello*"},
		{"hello^world", "hello* world*"},
		{"hello(world)", "hello* world*"},
		{"hello:world", "hello* world*"},
		{"hello*world", "hello* world*"}, // mid-string * stripped
		{"^hello^", "hello*"},
		{"path/segment", "path* segment*"},   // slash separates like unicode61
		{"v1.2.3-rc4", "v1* 2* 3* rc4*"},     // mixed separators
		{"don't", "don't*"},                  // in-word apostrophe preserved
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got := BuildFTS5Query(tc.in)
			if strings.ContainsAny(got, syntacticSpecials) {
				t.Errorf("BuildFTS5Query(%q) = %q still contains FTS5 syntactical specials (any of %q)", tc.in, got, syntacticSpecials)
			}
			if got != tc.want {
				t.Errorf("BuildFTS5Query(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
