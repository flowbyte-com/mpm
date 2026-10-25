// reference_identity_legacy_red_test.go — the RED half of the
// source-identity fix, written against the pre-fix API so it compiles
// and fails on a checkout of the parent revision.
//
// On origin/main these two tests fail:
//
//	TestAddReferenceFromFile_PathSpellingsAreOneDocument
//	  4 documents stored for one file (want 1)
//	TestAddReferenceFromFile_UnchangedReimportReportsUnchanged
//	  re-import of identical bytes reported imported, not unchanged
//
// They pass after the identity work, and they keep passing because the
// legacy adapter delegates to the same pipeline as every other surface.
package internal

import (
	"path/filepath"
	"testing"
)

// TestAddReferenceFromFile_PathSpellingsAreOneDocument imports one file
// through four equivalent spellings of its own path.
func TestAddReferenceFromFile_PathSpellingsAreOneDocument(t *testing.T) {
	dm := NewTestDM(t)
	corpus := t.TempDir()
	absolute := writeTestDoc(t, filepath.Join(corpus, "docs"), "wordpress-menus.md", menusMarkdown)

	for _, spelling := range []string{
		filepath.Base(absolute),               // bare name, from inside docs/
		filepath.Join("docs", "wordpress-menus.md"), // relative, from corpus/
		"./docs/../docs/wordpress-menus.md",   // dot-dot path
		absolute,                              // absolute
	} {
		source := spelling
		if !filepath.IsAbs(spelling) {
			if spelling == filepath.Base(absolute) {
				t.Chdir(filepath.Dir(absolute))
			} else {
				t.Chdir(corpus)
			}
		}
		res, err := dm.AddReferenceFromFile(source, "")
		if err != nil {
			t.Fatalf("AddReferenceFromFile(%q): %v", source, err)
		}
		if res["success"] != true {
			t.Fatalf("AddReferenceFromFile(%q) did not report success: %v", source, res)
		}
	}

	if n := referenceDocCount(t, dm); n != 1 {
		t.Fatalf("stored %d documents for one file imported through 4 spellings; want 1", n)
	}
}

// TestAddReferenceFromFile_UnchangedReimportReportsUnchanged re-imports
// identical bytes through a different spelling of the same path and
// requires the caller to be told nothing changed. On origin/main the
// second import creates a second document, so it reports neither the same
// id nor "unchanged".
func TestAddReferenceFromFile_UnchangedReimportReportsUnchanged(t *testing.T) {
	dm := NewTestDM(t)
	corpus := t.TempDir()
	writeTestDoc(t, filepath.Join(corpus, "docs"), "menus.md", menusMarkdown)

	first, err := dm.AddReferenceFromFile(filepath.Join(corpus, "docs", "menus.md"), "")
	if err != nil {
		t.Fatalf("first import: %v", err)
	}
	t.Chdir(filepath.Join(corpus, "docs"))
	second, err := dm.AddReferenceFromFile("menus.md", "")
	if err != nil {
		t.Fatalf("second import: %v", err)
	}
	if second["id"] != first["id"] {
		t.Fatalf("re-import created a new document (%v) instead of reusing %v", second["id"], first["id"])
	}
	if second["unchanged"] != true {
		t.Errorf("identical re-import did not report unchanged: %v", second)
	}
}
