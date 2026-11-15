package internal

// migrate_json_packages_test.go — the migration consumer's refusal on
// a document that carries package archives.
//
// The refusal exists because the migration reads the document's
// PROJECTED rows and nothing else: importing a package-bearing
// document would write the rows and silently drop every package's
// reference files, scripts and assets.
//
// The cases below are the ones where the refusal has to be FAITHFUL:
// an index that cannot be read leaves the number of packages UNKNOWN,
// and an unknown number is not zero. A guard that treated a malformed
// index as "no packages" would be a fail-open in a fail-closed design.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// p7PackageDocument writes a JSON export document with the given
// `package_archives` index, and returns its path.
func p7PackageDocument(t *testing.T, index string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "export.json")
	body := `{"memories": [], "package_archives": ` + index + `}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write the document: %v", err)
	}
	return path
}

// TestP7MigrateRefusesADocumentThatCarriesPackageArchives pins the
// refusal itself.
func TestP7MigrateRefusesADocumentThatCarriesPackageArchives(t *testing.T) {
	dm := p3DM(t)
	path := p7PackageDocument(t, `{"count": 3, "directory": "export.json.packages"}`)

	_, err := dm.IngestFromJsonFile(path, "p7-migrate", true)
	if err == nil {
		t.Fatalf("the migration accepted a document that carries package archives")
	}
	if !strings.Contains(err.Error(), "package") {
		t.Errorf("the refusal does not say the document carries packages: %v", err)
	}
	if !strings.Contains(err.Error(), "silently") {
		t.Errorf("the refusal does not say what would be lost: %v", err)
	}
}

// TestP7MigrateRefusesAMalformedPackageArchiveIndex pins the FAIL-CLOSED
// half.
//
// The index's count decides whether the document carries packages. An
// index whose count cannot be read leaves that number UNKNOWN, and the
// safe reading of an unknown number is the one that refuses.
//
// Each case below is a way the count can be unreadable while the key
// is still present.
func TestP7MigrateRefusesAMalformedPackageArchiveIndex(t *testing.T) {
	cases := []struct {
		name  string
		index string
	}{
		{"a string count", `{"count": "3", "directory": "d"}`},
		{"a fractional count", `{"count": 3.7, "directory": "d"}`},
		{"a string index", `"d"`},
		{"a null count", `{"count": null, "directory": "d"}`},
		{"a missing count", `{"directory": "d"}`},
		{"an array index", `[1, 2, 3]`},
		{"an object count", `{"count": {"n": 3}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dm := p3DM(t)
			path := p7PackageDocument(t, tc.index)

			_, err := dm.IngestFromJsonFile(path, "p7-migrate-malformed", true)
			if err == nil {
				t.Fatalf("the migration accepted a document with a malformed archive index")
			}
			if !strings.Contains(err.Error(), "MALFORMED") &&
				!strings.Contains(err.Error(), "package") {
				t.Errorf("the refusal does not say the index is malformed: %v", err)
			}
		})
	}
}

// TestP7MigrateAcceptsADocumentThatCarriesNoPackage pins that the
// refusal is about PACKAGE-BEARING documents, not about the key's
// presence.
//
// A document whose index says zero archives is a complete export of a
// workspace with no packages, and it is imported normally.
func TestP7MigrateAcceptsADocumentThatCarriesNoPackage(t *testing.T) {
	dm := p3DM(t)
	path := p7PackageDocument(t, `{"count": 0, "directory": ""}`)

	if _, err := dm.IngestFromJsonFile(path, "p7-migrate-empty", true); err != nil {
		t.Fatalf("the migration refused a document that carries no package: %v", err)
	}
}

// TestP7MigrateAcceptsADocumentWithNoArchiveIndex pins that a document
// without the key at all is imported normally.
func TestP7MigrateAcceptsADocumentWithNoArchiveIndex(t *testing.T) {
	dm := p3DM(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "export.json")
	if err := os.WriteFile(path, []byte(`{"memories": []}`), 0o600); err != nil {
		t.Fatalf("write the document: %v", err)
	}

	if _, err := dm.IngestFromJsonFile(path, "p7-migrate-no-index", true); err != nil {
		t.Fatalf("the migration refused a document with no archive index: %v", err)
	}
}

// TestP7JsonPackageArchiveIndexReportsWhatItFound pins the helper's
// four answers, because the refusal's behaviour rests on them.
func TestP7JsonPackageArchiveIndexReportsWhatItFound(t *testing.T) {
	cases := []struct {
		name     string
		document string
		count    int
		present  bool
		readable bool
	}{
		{"a count", `{"memories": [], "package_archives": {"count": 3}}`, 3, true, true},
		{"a zero count", `{"memories": [], "package_archives": {"count": 0}}`, 0, true, true},
		{"no key", `{"memories": []}`, 0, false, true},
		{"a malformed count", `{"memories": [], "package_archives": {"count": "3"}}`, 0, true, false},
		{"no count", `{"memories": [], "package_archives": {"directory": "d"}}`, 0, true, false},
		{"a string index", `{"memories": [], "package_archives": "d"}`, 0, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			count, _, present, readable := jsonPackageArchiveIndex([]byte(tc.document))
			if count != tc.count {
				t.Errorf("the count is %d, want %d", count, tc.count)
			}
			if present != tc.present {
				t.Errorf("the index reports present=%v, want %v", present, tc.present)
			}
			if readable != tc.readable {
				t.Errorf("the index reports readable=%v, want %v", readable, tc.readable)
			}
		})
	}
}
