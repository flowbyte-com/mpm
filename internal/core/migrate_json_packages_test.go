package internal

// migrate_json_packages_test.go — the migration consumer's refusal on
// a document that carries package archives.
//
// The refusal exists because the migration reads the document's
// PROJECTED rows and nothing else: importing a package-bearing
// document would write the rows and silently drop every package's
// reference files, scripts and assets.
//
// The cases below are the ones where the refusal has to be FAITHFUL.
// Two numbers decide it, and both have to be read:
//
//   - `package_archives.count` — the archives the export PRODUCED;
//   - `packages_omitted.count` — the packages it could NOT carry.
//
// A guard that reads only the first is a fail-open: an export whose
// every package failed verification carries `package_archives.count =
// 0`, and the document that dropped the package would be imported
// normally. An unknown number is not zero, either.

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

// p7PackageDocumentWith writes a JSON export document with an explicit
// `package_archives` index AND an explicit `packages_omitted` marker,
// and returns its path.
func p7PackageDocumentWith(t *testing.T, index, omitted string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "export.json")
	body := `{"memories": [], "package_archives": ` + index +
		`, "packages_omitted": ` + omitted + `}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write the document: %v", err)
	}
	return path
}

// TestP7MigrateRefusesADocumentThatCarriesPackageArchives pins the
// refusal itself.
func TestP7MigrateRefusesADocumentThatCarriesPackageArchives(t *testing.T) {
	dm := p3DM(t)
	path := p7PackageDocumentWith(t,
		`{"count": 1, "directory": "export.json.packages", "archives": [{"file_name": "a-1.0.0.skill"}]}`,
		`{"count": 0, "packages": [], "scope": {"total": 1, "selected": 1, "carried": 1}}`)

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

// TestP7MigrateRefusesADocumentThatDroppedAPackage pins the SECOND
// number.
//
// This is the fail-open the earlier guard had: an export whose every
// package failed verification produces `package_archives.count = 0`
// and `packages_omitted.count = 1`. The document carries no archive
// index worth reading, and it says in its own marker that it dropped a
// package. Importing it would write the projected rows and leave the
// package's references, scripts and assets behind — the loss the
// refusal exists to prevent.
func TestP7MigrateRefusesADocumentThatDroppedAPackage(t *testing.T) {
	dm := p3DM(t)
	path := p7PackageDocumentWith(t,
		`{"count": 0, "directory": "export.json.packages", "archives": []}`,
		`{"count": 1, "packages": [{"skill_id": "skill:dropped", "reason": "integrity_failure"}], "scope": {"total": 1, "selected": 1, "carried": 0}}`)

	_, err := dm.IngestFromJsonFile(path, "p7-migrate-dropped", true)
	if err == nil {
		t.Fatalf("the migration accepted a document that DROPPED a package")
	}
	if !strings.Contains(err.Error(), "DROPPED") {
		t.Errorf("the refusal does not say the document dropped a package: %v", err)
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
		{"a string count", `{"count": "3", "directory": "d", "archives": []}`},
		{"a fractional count", `{"count": 3.7, "directory": "d", "archives": []}`},
		{"a string index", `"d"`},
		{"a null count", `{"count": null, "directory": "d", "archives": []}`},
		{"a missing count", `{"directory": "d", "archives": []}`},
		{"an array index", `[1, 2, 3]`},
		{"an object count", `{"count": {"n": 3}, "archives": []}`},
		{"a negative count", `{"count": -3, "directory": "d", "archives": []}`},
		{"a count that disagrees with its list", `{"count": 2, "directory": "d", "archives": [{"file_name": "a.skill"}]}`},
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

// TestP7MigrateRefusesAMalformedPackageOmissionMarker pins the same
// fail-closed rule on the SECOND number.
func TestP7MigrateRefusesAMalformedPackageOmissionMarker(t *testing.T) {
	cases := []struct {
		name    string
		omitted string
	}{
		{"a string count", `{"count": "1", "packages": []}`},
		{"a fractional count", `{"count": 1.5, "packages": []}`},
		{"a null count", `{"count": null, "packages": []}`},
		{"a missing count", `{"packages": []}`},
		{"a negative count", `{"count": -1, "packages": []}`},
		{"a count that disagrees with its list", `{"count": 2, "packages": [{"skill_id": "skill:a"}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dm := p3DM(t)
			path := p7PackageDocumentWith(t,
				`{"count": 0, "directory": "d", "archives": []}`, tc.omitted)

			_, err := dm.IngestFromJsonFile(path, "p7-migrate-bad-omitted", true)
			if err == nil {
				t.Fatalf("the migration accepted a document with a malformed omission marker")
			}
			if !strings.Contains(err.Error(), "MALFORMED") &&
				!strings.Contains(err.Error(), "package") {
				t.Errorf("the refusal does not say the marker is malformed: %v", err)
			}
		})
	}
}

// TestP7MigrateAcceptsADocumentThatCarriesNoPackage pins that the
// refusal is about PACKAGE-BEARING documents, not about the key's
// presence.
//
// A document whose index says zero archives and whose omission marker
// says zero omissions is a complete export of a workspace with no
// packages, and it is imported normally.
func TestP7MigrateAcceptsADocumentThatCarriesNoPackage(t *testing.T) {
	dm := p3DM(t)
	path := p7PackageDocumentWith(t,
		`{"count": 0, "directory": "", "archives": []}`,
		`{"count": 0, "packages": [], "scope": {"total": 0, "selected": 0, "carried": 0}}`)

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

// TestP7JsonPackageArchiveFactsReportsWhatItFound pins the helper's
// answers, because the refusal's behaviour rests on them.
func TestP7JsonPackageArchiveFactsReportsWhatItFound(t *testing.T) {
	cases := []struct {
		name           string
		document       string
		archives       int
		archivesListed int
		omitted        int
		malformed      bool
	}{
		{"a count", `{"memories": [], "package_archives": {"count": 1, "archives": [{"file_name": "a.skill"}]}}`,
			1, 1, 0, false},
		{"a zero count", `{"memories": [], "package_archives": {"count": 0, "archives": []}}`,
			0, 0, 0, false},
		{"no key", `{"memories": []}`, 0, 0, 0, false},
		{"an omission marker", `{"memories": [], "packages_omitted": {"count": 2, "packages": [{"skill_id": "a"}, {"skill_id": "b"}]}}`,
			0, 0, 2, false},
		{"a malformed count", `{"memories": [], "package_archives": {"count": "3", "archives": []}}`,
			0, 0, 0, true},
		{"no count", `{"memories": [], "package_archives": {"directory": "d", "archives": []}}`,
			0, 0, 0, true},
		{"a string index", `{"memories": [], "package_archives": "d"}`, 0, 0, 0, true},
		{"a negative count", `{"memories": [], "package_archives": {"count": -1, "archives": []}}`,
			0, 0, 0, true},
		{"a count that disagrees with its list", `{"memories": [], "package_archives": {"count": 2, "archives": []}}`,
			0, 0, 0, true},
		{"a malformed omission marker", `{"memories": [], "packages_omitted": {"count": "1", "packages": []}}`,
			0, 0, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			facts := jsonPackageArchiveFactsOf([]byte(tc.document))
			if facts.Archives != tc.archives {
				t.Errorf("the archive count is %d, want %d", facts.Archives, tc.archives)
			}
			if facts.ArchivesListed != tc.archivesListed {
				t.Errorf("the listed archive count is %d, want %d", facts.ArchivesListed, tc.archivesListed)
			}
			if facts.Omitted != tc.omitted {
				t.Errorf("the omission count is %d, want %d", facts.Omitted, tc.omitted)
			}
			if facts.Malformed != tc.malformed {
				t.Errorf("the facts report malformed=%v, want %v (reason: %s)",
					facts.Malformed, tc.malformed, facts.Reason)
			}
			if tc.malformed && facts.Reason == "" {
				t.Errorf("the facts report a malformed document without a reason")
			}
		})
	}
}
