// skill_package_export_cli_test.go — Stage P7: the COMPLETE skills
// export at the CLI boundary.
//
// Design of record: PR #222 §5.6 (import and export fidelity), §6.3
// (the export contract: no silent loss) and §13 (T07, T11, T44).
//
// Before P7 the skills export carried the PROJECTED skill row only, and
// a marker named every package whose content had been dropped. P7 makes
// the export COMPLETE: the archives are written beside the document, and
// the marker names only what could NOT be carried.
//
// So the contract this file pins is:
//
//   - a package-bearing export writes the document AND a sibling
//     directory holding one `.skill` archive per packaged skill;
//   - the document carries a machine-readable `package_archives` index —
//     the file names, the skill ids, the package hashes and the archive
//     digests;
//   - the `packages_omitted` marker reports ZERO omissions only when
//     every selected package was actually exported, and it says how many
//     were in scope so a zero is a positive statement about a known
//     denominator;
//   - a package that cannot be carried is NAMED, with its reason, while
//     the rest of the export completes;
//   - a destination that cannot carry the archives is REFUSED BEFORE
//     anything is emitted — standard output, CSV and the deprecated
//     bare-array format;
//   - an existing export is never silently overwritten, and a symlink is
//     never followed;
//   - `mpm migrate --from` FAILS CLOSED on a document that carries
//     package archives.
//
// The tests run against the package's disposable test database (the
// package's TestMain pins HOME to the temporary root), never against the
// operator's installation.
//
// ─────────────────────────────────────────────────────────────────────────────
// WHY THE EXPECTATIONS ARE COMPUTED, NOT HARDCODED
// ─────────────────────────────────────────────────────────────────────────────
//
// The CLI's database is a PROCESS-WIDE SINGLETON: every test in this
// package shares it, and so does every iteration of a repeated run
// ("go test -count=2"). A hardcoded count therefore makes the test
// depend on how many packages some OTHER test happened to import first.
//
// So the expected scope is computed from the database, and every package
// the export names is checked against the packages the database actually
// holds. The assertion is about ACCURACY — the export carries exactly
// the packages in scope, or names exactly the ones it could not — which
// is the property that matters, and it holds no matter what else is in
// the database.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// ─── Fixtures ──────────────────────────────────────────────────────────────

// p7CLIExportDoc is the JSON document the export emits.
//
// The three top-level keys are the contract: `memories` (unchanged),
// `packages_omitted` (the marker, with its scope) and
// `package_archives` (the index of the produced archives).
type p7CLIExportDoc struct {
	Memories []map[string]interface{} `json:"memories"`

	Packages struct {
		Count    int `json:"count"`
		Packages []struct {
			SkillID     string `json:"skill_id"`
			Name        string `json:"name"`
			Version     string `json:"version"`
			FileCount   int    `json:"file_count"`
			PackageHash string `json:"package_hash"`
			Reason      string `json:"reason"`
			Detail      string `json:"detail"`
		} `json:"packages"`
		Scope struct {
			Total    int `json:"total"`
			Selected int `json:"selected"`
			Carried  int `json:"carried"`
		} `json:"scope"`
	} `json:"packages_omitted"`

	Archives struct {
		Count     int    `json:"count"`
		Directory string `json:"directory"`
		Archives  []struct {
			SkillID     string `json:"skill_id"`
			Name        string `json:"name"`
			Version     string `json:"version"`
			PackageHash string `json:"package_hash"`
			FileName    string `json:"file_name"`
			SHA256      string `json:"sha256"`
			SizeBytes   int64  `json:"size_bytes"`
			FileCount   int    `json:"file_count"`
		} `json:"archives"`
	} `json:"package_archives"`
}

// p7CLICounter makes every fixture name unique, so a repeated run
// ("go test -count=2") never collides with the previous iteration's rows
// in the process-wide singleton database.
var p7CLICounter atomic.Int64

// p7CLIImportPackage imports a small package into the CLI's database and
// returns its skill id and its name.
//
// The name is unique per call, so the helper is safe to call from a
// repeated run.
//
// The package's supporting file carries UNIQUE content, and that is
// load-bearing rather than decorative: blobs are content-addressed, so
// two packages with the same supporting file SHARE one blob row. A
// fixture that corrupted a shared blob would corrupt every package with
// that content — including the ones the other fixtures depend on. The
// unique content keeps each package's bytes its own, so an adversarial
// fixture can damage exactly one package.
func p7CLIImportPackage(t *testing.T, name string) (string, string) {
	t.Helper()
	dm := getDBConcrete()
	if dm == nil {
		t.Skip("DB unavailable")
	}
	unique := fmt.Sprintf("%s-%d-%d", name, os.Getpid(), p7CLICounter.Add(1))
	root := t.TempDir()
	entry := "---\nname: " + unique + "\nversion: 1.0.0\ndescription: a test skill\n" +
		"when_to_use: when a test needs this exact skill\n---\nbody\n"
	if err := os.WriteFile(filepath.Join(root, "SKILL.md"), []byte(entry), 0o600); err != nil {
		t.Fatalf("write the entrypoint: %v", err)
	}
	refs := filepath.Join(root, "references")
	if err := os.MkdirAll(refs, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(refs, "a.md"), []byte("# "+unique+"\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	result, err := dm.ImportSkillPackageFromDir(mpminternal.PackageImportOptions{Path: root})
	if err != nil {
		t.Fatalf("import %s: %v", unique, err)
	}
	return result.SkillID, unique
}

// p7CLIPackagesInScope lists the packages the skills export covers, with
// their file counts.
//
// It is the EXPECTED scope, computed from the database rather than
// hardcoded.
func p7CLIPackagesInScope(t *testing.T, dm *mpminternal.DatabaseManager) map[string]int {
	t.Helper()
	out := map[string]int{}
	rows, err := dm.SQLDB().Query(`
		SELECT p.skill_id,
		       (SELECT COUNT(*) FROM skill_package_files f WHERE f.skill_id = p.skill_id)
		FROM skill_packages p
		JOIN memories m ON m.id = p.skill_id
		WHERE m.deleted_at IS NULL AND m.collection = 'skills'
	`)
	if err != nil {
		t.Fatalf("count the packages: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var files int
		if err := rows.Scan(&id, &files); err != nil {
			t.Fatalf("scan the packages: %v", err)
		}
		out[id] = files
	}
	return out
}

// p7CLIShredPackage removes a package from the shared CLI database.
//
// The shared database is a process-wide singleton, so an adversarial
// fixture that leaves a DAMAGED package behind would make every later
// assertion about the export's completeness depend on test order. The
// fixture therefore cleans up after itself.
func p7CLIShredPackage(t *testing.T, dm *mpminternal.DatabaseManager, skillID string) {
	t.Helper()
	if _, err := dm.SQLDB().Exec(`DELETE FROM memories WHERE id = ?`, skillID); err != nil {
		t.Fatalf("shred the package %s: %v", skillID, err)
	}
}

// p7CLIRestoreBlob puts a package file's blob back the way it was.
//
// It matters more than it looks. A blob is CONTENT-ADDRESSED and
// SHARED: the same bytes are stored once, and every package with those
// bytes points at the same row. So a fixture that corrupts a blob
// damages EVERY package that carries the same content — and the damage
// OUTLIVES the fixture, because the blob row survives the package that
// used it.
//
// So the adversarial fixture restores the blob. Without that, the next
// import of the same content fails with a hash mismatch, and the failure
// lands in an unrelated test.
func p7CLIRestoreBlob(t *testing.T, dm *mpminternal.DatabaseManager, skillID, path string) {
	t.Helper()
	var sha string
	if err := dm.SQLDB().QueryRow(
		`SELECT f.sha256 FROM skill_package_files f WHERE f.skill_id = ? AND f.path = ?`,
		skillID, path).Scan(&sha); err != nil {
		t.Fatalf("read the file row: %v", err)
	}
	var content []byte
	if err := dm.SQLDB().QueryRow(
		`SELECT content FROM skill_package_blobs WHERE sha256 = ?`, sha).Scan(&content); err != nil {
		t.Fatalf("read the blob: %v", err)
	}
	if _, err := dm.SQLDB().Exec(
		`UPDATE skill_package_blobs SET content = ?, size_bytes = ? WHERE sha256 = ?`,
		content, len(content), sha); err != nil {
		t.Fatalf("restore the blob: %v", err)
	}
}

// p7CLIReadDoc reads an export document.
func p7CLIReadDoc(t *testing.T, path string) p7CLIExportDoc {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the export: %v", err)
	}
	var doc p7CLIExportDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("the export is not the expected JSON document: %v\n%s", err, body)
	}
	return doc
}

// p7CLIDigest is the lowercase hex SHA-256 of some bytes.
func p7CLIDigest(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// p7CLICaptureStdout runs a function with standard output redirected to
// a temporary file, and returns what was written.
//
// The redirect is through `os.Stdout` itself, because the export writes
// the document to `os.Stdout` directly.
func p7CLICaptureStdout(t *testing.T, fn func() int) (string, int) {
	t.Helper()
	original := os.Stdout
	path := filepath.Join(t.TempDir(), "stdout")
	file, err := os.Create(path)
	if err != nil {
		t.Fatalf("create the stdout capture: %v", err)
	}
	os.Stdout = file
	rc := fn()
	os.Stdout = original
	if err := file.Close(); err != nil {
		t.Fatalf("close the stdout capture: %v", err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the stdout capture: %v", err)
	}
	return string(content), rc
}

// ─── T44: the export carries every selected package ───────────────────────

// TestP7_CLIExportCarriesEveryPackage pins the central claim of the
// stage (T44): a skills export carries EVERY selected package.
//
// The export writes the document and a sibling directory; the document
// names every archive in a machine-readable index; and the marker reports
// no omissions when nothing was lost.
func TestP7_CLIExportCarriesEveryPackage(t *testing.T) {
	dm := getDBConcrete()
	if dm == nil {
		t.Skip("DB unavailable")
	}
	skillID, name := p7CLIImportPackage(t, "cli-complete")
	expected := p7CLIPackagesInScope(t, dm)

	dir := t.TempDir()
	outPath := filepath.Join(dir, "export.json")
	if rc := handleExport([]string{"export", "--format", "json", "--collection", "skills", "--output", outPath}); rc != 0 {
		t.Fatalf("handleExport returned %d", rc)
	}

	doc := p7CLIReadDoc(t, outPath)

	// The scope is the packages the export was about.
	if doc.Packages.Scope.Selected != len(expected) {
		t.Fatalf("the export reports %d selected packages, want %d",
			doc.Packages.Scope.Selected, len(expected))
	}
	if doc.Packages.Scope.Total < doc.Packages.Scope.Selected {
		t.Errorf("the export reports %d packages in the store, fewer than the %d it selected",
			doc.Packages.Scope.Total, doc.Packages.Scope.Selected)
	}

	// The index names one archive per carried package.
	if doc.Archives.Count != len(doc.Archives.Archives) {
		t.Errorf("the index reports %d archives, want %d", doc.Archives.Count, len(doc.Archives.Archives))
	}
	if doc.Archives.Directory == "" {
		t.Errorf("the index names no directory")
	}

	// The sibling directory holds the archives.
	packageDir := outPath + packageExportDirectorySuffix
	entries, err := os.ReadDir(packageDir)
	if err != nil {
		t.Fatalf("read the package directory: %v", err)
	}
	if len(entries) != doc.Archives.Count {
		t.Errorf("the package directory holds %d file(s), the index names %d",
			len(entries), doc.Archives.Count)
	}

	// Every package in scope is either carried or NAMED as omitted —
	// never silently dropped.
	carried := map[string]bool{}
	for _, archive := range doc.Archives.Archives {
		carried[archive.SkillID] = true
		if archive.PackageHash == "" {
			t.Errorf("the index's entry for %s carries no package hash", archive.SkillID)
		}
		if archive.SHA256 == "" {
			t.Errorf("the index's entry for %s carries no archive digest", archive.SkillID)
		}
		if archive.FileName == "" || !strings.HasSuffix(archive.FileName, ".skill") {
			t.Errorf("the index's entry for %s has the file name %q", archive.SkillID, archive.FileName)
		}

		// The archive is ON DISK, and it hashes to what the index says.
		published := filepath.Join(packageDir, archive.FileName)
		content, err := os.ReadFile(published)
		if err != nil {
			t.Errorf("read the published archive %q: %v", published, err)
			continue
		}
		if got := p7CLIDigest(content); got != archive.SHA256 {
			t.Errorf("the published archive %q hashes to %s, the index says %s",
				archive.FileName, got, archive.SHA256)
		}
		if int64(len(content)) != archive.SizeBytes {
			t.Errorf("the published archive %q is %d bytes, the index says %d",
				archive.FileName, len(content), archive.SizeBytes)
		}
	}
	omitted := map[string]bool{}
	for _, o := range doc.Packages.Packages {
		omitted[o.SkillID] = true
		if o.Reason == "" {
			t.Errorf("the export names %s as omitted, without a reason", o.SkillID)
		}
	}
	for id := range expected {
		if !carried[id] && !omitted[id] {
			t.Errorf("the export neither carried nor named the package %s", id)
		}
		if carried[id] && omitted[id] {
			t.Errorf("the export both carried and omitted the package %s", id)
		}
	}

	// The marker and the scope agree: carried + omitted == selected.
	if doc.Packages.Count != len(omitted) {
		t.Errorf("the marker reports %d omissions, want %d", doc.Packages.Count, len(omitted))
	}
	if doc.Packages.Scope.Carried != len(carried) {
		t.Errorf("the scope reports %d carried packages, want %d",
			doc.Packages.Scope.Carried, len(carried))
	}

	// The package just imported is carried, with its own name.
	var found bool
	for _, archive := range doc.Archives.Archives {
		if archive.SkillID == skillID {
			found = true
			if archive.Name != name {
				t.Errorf("the index names the package %q, want %q", archive.Name, name)
			}
		}
	}
	if !found {
		t.Errorf("the export did not carry the imported package %s", skillID)
	}

	// The projected skill row is in the export too.
	var rowFound bool
	for _, m := range doc.Memories {
		if id, _ := m["id"].(string); id == skillID {
			rowFound = true
		}
	}
	if !rowFound {
		t.Errorf("the exported skill row %s is missing from the export", skillID)
	}
}

// TestP7_CLIExportOfAnOrdinarySkillCarriesNoArchive pins that an
// ordinary skill is never reported as a package.
//
// It has no package row, so it is not in the scope, so it is not carried
// and not named.
func TestP7_CLIExportOfAnOrdinarySkillCarriesNoArchive(t *testing.T) {
	dm := getDBConcrete()
	if dm == nil {
		t.Skip("DB unavailable")
	}
	unique := fmt.Sprintf("cli-ordinary-only-%d-%d", os.Getpid(), p7CLICounter.Add(1))
	document := "---\nname: " + unique + "\nversion: 1.0.0\ndescription: a test skill\n" +
		"when_to_use: when a test needs this exact skill\n---\nbody\n"
	ordinaryID, err := dm.SaveSkill(unique, "1.0.0", document, "cli", false)
	if err != nil {
		t.Fatalf("save: %v", err)
	}

	outPath := filepath.Join(t.TempDir(), "export.json")
	if rc := handleExport([]string{"export", "--format", "json", "--collection", "skills", "--output", outPath}); rc != 0 {
		t.Fatalf("handleExport returned %d", rc)
	}
	doc := p7CLIReadDoc(t, outPath)

	for _, archive := range doc.Archives.Archives {
		if archive.SkillID == ordinaryID {
			t.Errorf("the export carried the ordinary skill %s as a package", ordinaryID)
		}
	}
	for _, o := range doc.Packages.Packages {
		if o.SkillID == ordinaryID {
			t.Errorf("the export names the ordinary skill %s as an omitted package", ordinaryID)
		}
	}
}

// TestP7_CLIExportNamesAPackageItCannotCarry pins the honest-partial
// case: a package that does not verify is NAMED, with its reason, while
// the rest of the export completes.
func TestP7_CLIExportNamesAPackageItCannotCarry(t *testing.T) {
	dm := getDBConcrete()
	if dm == nil {
		t.Skip("DB unavailable")
	}
	brokenID, _ := p7CLIImportPackage(t, "cli-broken")
	healthyID, _ := p7CLIImportPackage(t, "cli-healthy")

	// Corrupt the broken package's blob, so it does not verify.
	//
	// The blob is RESTORED afterwards, and the package is shredded:
	// blobs are shared and content-addressed, so a corruption left
	// behind would outlive this test and break every later import of
	// the same bytes.
	defer p7CLIShredPackage(t, dm, brokenID)
	defer p7CLIRestoreBlob(t, dm, brokenID, "references/a.md")
	if _, err := dm.SQLDB().Exec(`UPDATE skill_package_blobs SET content = ? WHERE sha256 = (
		SELECT f.sha256 FROM skill_package_files f
		WHERE f.skill_id = ? AND f.path = 'references/a.md')`,
		[]byte("# tampered\n"), brokenID); err != nil {
		t.Fatalf("corrupt the blob: %v", err)
	}

	outPath := filepath.Join(t.TempDir(), "export.json")
	if rc := handleExport([]string{"export", "--format", "json", "--collection", "skills", "--output", outPath}); rc != 0 {
		t.Fatalf("handleExport returned %d", rc)
	}
	doc := p7CLIReadDoc(t, outPath)

	// The broken package is NAMED, with a reason.
	var named *struct {
		SkillID     string `json:"skill_id"`
		Name        string `json:"name"`
		Version     string `json:"version"`
		FileCount   int    `json:"file_count"`
		PackageHash string `json:"package_hash"`
		Reason      string `json:"reason"`
		Detail      string `json:"detail"`
	}
	for i := range doc.Packages.Packages {
		if doc.Packages.Packages[i].SkillID == brokenID {
			named = &doc.Packages.Packages[i]
		}
	}
	if named == nil {
		t.Fatalf("the export did not name the damaged package %s", brokenID)
	}
	if named.Reason == "" {
		t.Errorf("the export names the damaged package without a reason")
	}
	if named.Detail == "" {
		t.Errorf("the export names the damaged package without a detail")
	}

	// The healthy package is CARRIED: the export completes.
	var carried bool
	for _, archive := range doc.Archives.Archives {
		if archive.SkillID == healthyID {
			carried = true
		}
		if archive.SkillID == brokenID {
			t.Errorf("the export carried the damaged package %s", brokenID)
		}
	}
	if !carried {
		t.Errorf("the export did not carry the healthy package %s", healthyID)
	}
}

// ─── The destination policy ────────────────────────────────────────────────

// TestP7_CLIExportToStdoutIsRefusedWhenItCarriesAPackages pins that a
// package-bearing export to standard output is REFUSED, and refused
// BEFORE anything is emitted.
//
// A partial export on standard output would be a document whose index
// names archives the caller does not have — the exact loss R11 exists to
// prevent.
func TestP7_CLIExportToStdoutIsRefusedWhenItCarriesAPackages(t *testing.T) {
	p7CLIImportPackage(t, "cli-stdout-refused")

	out, rc := p7CLICaptureStdout(t, func() int {
		return handleExport([]string{"export", "--format", "json", "--collection", "skills"})
	})
	if rc == 0 {
		t.Fatalf("a package-bearing export to standard output was accepted")
	}
	if strings.TrimSpace(out) != "" {
		t.Errorf("the refused export emitted %d byte(s) on standard output:\n%s", len(out), out)
	}
}

// TestP7_CLIExportToCSVIsRefusedWhenItCarriesAPackages pins that the CSV
// format refuses a package-bearing export rather than claiming an
// incomplete export is complete.
func TestP7_CLIExportToCSVIsRefusedWhenItCarriesAPackages(t *testing.T) {
	p7CLIImportPackage(t, "cli-csv-refused")

	outPath := filepath.Join(t.TempDir(), "export.csv")
	var rc int
	out := captureUserError(t, func() int {
		rc = handleExport([]string{"export", "--format", "csv", "--collection", "skills", "--output", outPath})
		return rc
	})
	if rc == 0 {
		t.Fatalf("a package-bearing CSV export was accepted")
	}
	if !strings.Contains(out, "CSV") {
		t.Errorf("the refusal does not name the format:\n%s", out)
	}
	if _, err := os.Stat(outPath); err == nil {
		t.Errorf("the refused CSV export wrote a file")
	}
}

// TestP7_CLIExportToTheLegacyArrayFormatIsRefusedWhenItCarriesAPackages
// pins that the DEPRECATED bare-array format refuses a package-bearing
// export rather than silently dropping the archives.
func TestP7_CLIExportToTheLegacyArrayFormatIsRefusedWhenItCarriesAPackages(t *testing.T) {
	p7CLIImportPackage(t, "cli-legacy-refused")

	outPath := filepath.Join(t.TempDir(), "export.json")
	var rc int
	out := captureUserError(t, func() int {
		rc = handleExport([]string{"export", "--format", "json-array", "--collection", "skills", "--output", outPath})
		return rc
	})
	if rc == 0 {
		t.Fatalf("a package-bearing legacy-array export was accepted")
	}
	if !strings.Contains(out, exportLegacyArrayFormat) {
		t.Errorf("the refusal does not name the format:\n%s", out)
	}
	if !strings.Contains(out, "json") {
		t.Errorf("the refusal does not point at the structured format:\n%s", out)
	}
	if _, err := os.Stat(outPath); err == nil {
		t.Errorf("the refused legacy export wrote a document")
	}
}

// TestP7_CLIExportToStdoutIsAllowedWhenItCarriesNoPackage pins the other
// half: an export with no package still goes to standard output.
func TestP7_CLIExportToStdoutIsAllowedWhenItCarriesNoPackage(t *testing.T) {
	out, rc := p7CLICaptureStdout(t, func() int {
		return handleExport([]string{"export", "--format", "json", "--collection", "directives"})
	})
	if rc != 0 {
		t.Fatalf("a package-free export to standard output was refused")
	}
	var doc p7CLIExportDoc
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("the export is not the expected JSON document: %v\n%s", err, out)
	}
	if doc.Archives.Count != 0 {
		t.Errorf("a package-free export names %d archive(s)", doc.Archives.Count)
	}
}

// ─── The filesystem safety ─────────────────────────────────────────────────

// TestP7_CLIExportRefusesAnExistingPackageDirectory pins that an export
// never SILENTLY overwrites an existing export's archives.
func TestP7_CLIExportRefusesAnExistingPackageDirectory(t *testing.T) {
	p7CLIImportPackage(t, "cli-existing-dir")

	dir := t.TempDir()
	outPath := filepath.Join(dir, "export.json")
	packageDir := outPath + packageExportDirectorySuffix
	if err := os.MkdirAll(packageDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	existing := filepath.Join(packageDir, "previous.skill")
	if err := os.WriteFile(existing, []byte("the previous export's bytes\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	var rc int
	out := captureUserError(t, func() int {
		rc = handleExport([]string{"export", "--format", "json", "--collection", "skills", "--output", outPath})
		return rc
	})
	if rc == 0 {
		t.Fatalf("the export overwrote an existing package directory")
	}
	if !strings.Contains(out, packageDir) {
		t.Errorf("the refusal does not name the directory:\n%s", out)
	}

	// The existing archive is UNTOUCHED.
	content, err := os.ReadFile(existing)
	if err != nil {
		t.Fatalf("the existing archive was removed: %v", err)
	}
	if string(content) != "the previous export's bytes\n" {
		t.Errorf("the existing archive was overwritten: %q", content)
	}
}

// TestP7_CLIExportRefusesASymlinkedOutput pins that the export never
// follows a symlink out of the directory the operator named.
//
// A symlink at the output path could point anywhere — including at the
// live substrate.
func TestP7_CLIExportRefusesASymlinkedOutput(t *testing.T) {
	p7CLIImportPackage(t, "cli-symlink")

	dir := t.TempDir()
	elsewhere := filepath.Join(t.TempDir(), "target.json")
	outPath := filepath.Join(dir, "export.json")
	if err := os.Symlink(elsewhere, outPath); err != nil {
		t.Skipf("symlinks are unavailable: %v", err)
	}

	var rc int
	out := captureUserError(t, func() int {
		rc = handleExport([]string{"export", "--format", "json", "--collection", "skills", "--output", outPath})
		return rc
	})
	if rc == 0 {
		t.Fatalf("the export followed a symlinked output path")
	}
	if !strings.Contains(out, "symlink") {
		t.Errorf("the refusal does not say the path is a symlink:\n%s", out)
	}
	if _, err := os.Stat(elsewhere); err == nil {
		t.Errorf("the export wrote through the symlink")
	}
}

// TestP7_CLIExportLeavesNoStagingBehind pins that a successful export
// leaves nothing but its own output behind.
//
// The staging directory is a sibling of the output, so a leaked staging
// directory would be visible in the output's own directory.
func TestP7_CLIExportLeavesNoStagingBehind(t *testing.T) {
	p7CLIImportPackage(t, "cli-staging")

	dir := t.TempDir()
	outPath := filepath.Join(dir, "export.json")
	if rc := handleExport([]string{"export", "--format", "json", "--collection", "skills", "--output", outPath}); rc != 0 {
		t.Fatalf("handleExport returned %d", rc)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read the output directory: %v", err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
		if strings.HasPrefix(entry.Name(), ".mpm-export") {
			t.Errorf("the export left staging output behind: %v", names)
		}
	}
	sort.Strings(names)
	want := []string{"export.json", "export.json" + packageExportDirectorySuffix}
	if len(names) != len(want) {
		t.Fatalf("the output directory holds %v, want exactly %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Errorf("the output directory holds %v, want exactly %v", names, want)
		}
	}
}

// ─── The round trip ────────────────────────────────────────────────────────

// TestP7_CLITheProducedArchiveReimportsAndPreservesThePackageHash pins
// the transport property at the CLI boundary: the archive the export
// PRODUCED is accepted by the P5 importer, and the re-imported package
// has the same identity.
//
// The importer used here is the substrate's own — the check is against
// P5's reader, not against a second interpretation of `archive/zip`.
func TestP7_CLITheProducedArchiveReimportsAndPreservesThePackageHash(t *testing.T) {
	dm := getDBConcrete()
	if dm == nil {
		t.Skip("DB unavailable")
	}
	skillID, _ := p7CLIImportPackage(t, "cli-roundtrip")

	outPath := filepath.Join(t.TempDir(), "export.json")
	if rc := handleExport([]string{"export", "--format", "json", "--collection", "skills", "--output", outPath}); rc != 0 {
		t.Fatalf("handleExport returned %d", rc)
	}
	doc := p7CLIReadDoc(t, outPath)

	var published string
	var packageHash string
	for _, archive := range doc.Archives.Archives {
		if archive.SkillID == skillID {
			published = filepath.Join(outPath+packageExportDirectorySuffix, archive.FileName)
			packageHash = archive.PackageHash
		}
	}
	if published == "" {
		t.Fatalf("the export did not carry the imported package %s", skillID)
	}

	// The import goes into a DISPOSABLE database, so the shared CLI
	// database does not gain a second copy of the package.
	target := mpminternal.NewTestDM(t)
	result, err := target.ImportSkillPackageFromArchive(mpminternal.PackageImportOptions{
		Path: published,
	})
	if err != nil {
		t.Fatalf("the exported archive is NOT accepted by the importer: %v", err)
	}
	if result.PackageHash != packageHash {
		t.Errorf("the re-imported package hashes to %s, want %s",
			result.PackageHash, packageHash)
	}
}

// ─── The migration consumer ────────────────────────────────────────────────

// TestP7_CLIMigrateFailsClosedOnAPackageBearingDocument pins that
// `mpm migrate --from` refuses a document that carries package
// archives.
//
// The migration consumer reads the document's PROJECTED rows. Importing
// such a document would write the projected rows and silently drop every
// package's reference files, scripts and assets — the exact loss R11
// exists to prevent. So the consumer refuses, with a diagnostic that
// says what is missing.
func TestP7_CLIMigrateFailsClosedOnAPackageBearingDocument(t *testing.T) {
	dm := getDBConcrete()
	if dm == nil {
		t.Skip("DB unavailable")
	}
	p7CLIImportPackage(t, "cli-migrate-refused")

	outPath := filepath.Join(t.TempDir(), "export.json")
	if rc := handleExport([]string{"export", "--format", "json", "--collection", "skills", "--output", outPath}); rc != 0 {
		t.Fatalf("handleExport returned %d", rc)
	}

	// The document really does carry archives — otherwise this test
	// would prove nothing.
	doc := p7CLIReadDoc(t, outPath)
	if doc.Archives.Count == 0 {
		t.Fatalf("the export carried no archive; the refusal cannot be exercised")
	}

	_, err := dm.IngestFromJsonFile(outPath, "p7-migrate-test", true)
	if err == nil {
		t.Fatalf("migrate --from accepted a package-bearing document")
	}
	if !strings.Contains(err.Error(), "package") {
		t.Errorf("the refusal does not say the document carries packages: %v", err)
	}
	if !strings.Contains(err.Error(), "silently") {
		t.Errorf("the refusal does not say what would be lost: %v", err)
	}
}

// TestP7_CLIMigrateAcceptsADocumentThatCarriesNoPackage pins the other
// half: a document with an EMPTY package index is imported normally.
//
// The refusal is about a package-bearing document, not about the key's
// presence.
func TestP7_CLIMigrateAcceptsADocumentThatCarriesNoPackage(t *testing.T) {
	dm := getDBConcrete()
	if dm == nil {
		t.Skip("DB unavailable")
	}

	outPath := filepath.Join(t.TempDir(), "export.json")
	if rc := handleExport([]string{"export", "--format", "json", "--collection", "directives", "--output", outPath}); rc != 0 {
		t.Fatalf("handleExport returned %d", rc)
	}

	// The document carries the key, with a count of zero.
	doc := p7CLIReadDoc(t, outPath)
	if doc.Archives.Count != 0 {
		t.Fatalf("the export names %d archive(s)", doc.Archives.Count)
	}

	if _, err := dm.IngestFromJsonFile(outPath, "p7-migrate-empty", true); err != nil {
		t.Fatalf("migrate --from refused a document that carries no package: %v", err)
	}
}

// ─── The failure and recovery paths ────────────────────────────────────────

// TestP7_CLIAnEmptySelectionCarriesNothing pins that a selection that
// matches nothing is an EMPTY export, not a refusal.
//
// A package-bearing export is refused when it has no destination that
// can carry the archives. An export that carries NO package has nothing
// to carry, so the refusal does not apply — and an operator asking for
// "nothing since the future" gets nothing, cleanly.
func TestP7_CLIAnEmptySelectionCarriesNothing(t *testing.T) {
	out, rc := p7CLICaptureStdout(t, func() int {
		return handleExport([]string{"export", "--format", "json", "--collection", "skills", "--since", "2999-01-01"})
	})
	if rc != 0 {
		t.Fatalf("an empty selection was refused")
	}
	var doc p7CLIExportDoc
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("the export is not the expected JSON document: %v\n%s", err, out)
	}
	if len(doc.Memories) != 0 {
		t.Errorf("an empty selection carried %d row(s)", len(doc.Memories))
	}
	if doc.Archives.Count != 0 {
		t.Errorf("an empty selection carried %d archive(s)", doc.Archives.Count)
	}
	if doc.Packages.Count != 0 {
		t.Errorf("an empty selection reported %d omission(s)", doc.Packages.Count)
	}
	if doc.Packages.Scope.Selected != 0 {
		t.Errorf("an empty selection reported %d selected package(s)", doc.Packages.Scope.Selected)
	}
}

// TestP7_CLIAFailedWriteLeavesNothingBehind pins that a write that
// FAILS leaves the destination exactly as it was.
//
// The staging directory is created in the output's own directory, so a
// failure to create it is the first thing that can go wrong. The
// operator's previous output must survive it, and no staging directory
// may be left behind.
func TestP7_CLIAFailedWriteLeavesNothingBehind(t *testing.T) {
	p7CLIImportPackage(t, "cli-failed-write")

	dir := t.TempDir()
	outPath := filepath.Join(dir, "export.json")

	// Make the output's directory unwritable, so the staging directory
	// cannot be created.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	defer os.Chmod(dir, 0o700)

	var rc int
	out := captureUserError(t, func() int {
		rc = handleExport([]string{"export", "--format", "json", "--collection", "skills", "--output", outPath})
		return rc
	})
	if rc == 0 {
		t.Fatalf("the export reported success despite failing to write")
	}
	if out == "" {
		t.Errorf("the failure was not reported")
	}

	// Restore the permissions and check the destination.
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if _, err := os.Stat(outPath); err == nil {
		t.Errorf("the failed export wrote a document")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read the output directory: %v", err)
	}
	for _, entry := range entries {
		t.Errorf("the failed export left %q behind", entry.Name())
	}
}

// TestP7_CLIAnInterruptedPublicationIsRecoverable pins the RECOVERY path
// for the publication that is NOT one atomic operation.
//
// The export publishes the sibling directory first and the document
// second, so an interruption between the two renames leaves a directory
// of archives and no document. That intermediate state must be safe:
// the archives it holds are intact and importable, and the operator can
// recover by removing the directory and running the export again.
func TestP7_CLIAnInterruptedPublicationIsRecoverable(t *testing.T) {
	dm := getDBConcrete()
	if dm == nil {
		t.Skip("DB unavailable")
	}
	skillID, _ := p7CLIImportPackage(t, "cli-interrupted")

	dir := t.TempDir()
	outPath := filepath.Join(dir, "export.json")
	if rc := handleExport([]string{"export", "--format", "json", "--collection", "skills", "--output", outPath}); rc != 0 {
		t.Fatalf("handleExport returned %d", rc)
	}

	// The archives the interruption left behind are INTACT and
	// importable — the interruption did not damage anything.
	//
	// The document is read BEFORE it is removed, because it is the
	// index that says which archive belongs to which package.
	doc := p7CLIReadDoc(t, outPath)
	var published string
	for _, archive := range doc.Archives.Archives {
		if archive.SkillID == skillID {
			published = filepath.Join(outPath+packageExportDirectorySuffix, archive.FileName)
		}
	}
	if published == "" {
		t.Fatalf("the export did not carry the imported package %s", skillID)
	}

	// Simulate the interruption: the directory was published, the
	// document was not.
	if err := os.Remove(outPath); err != nil {
		t.Fatalf("remove the document: %v", err)
	}

	if _, err := os.Stat(published); err != nil {
		t.Fatalf("the interrupted publication left no archive: %v", err)
	}
	target := mpminternal.NewTestDM(t)
	result, err := target.ImportSkillPackageFromArchive(mpminternal.PackageImportOptions{Path: published})
	if err != nil {
		t.Fatalf("the archive left by the interrupted publication is not importable: %v", err)
	}
	if result.SkillID != skillID {
		t.Errorf("the re-imported package has the id %q, want %q", result.SkillID, skillID)
	}

	// The recovery: the export refuses to run into the orphaned
	// directory, and says how to recover.
	var rc int
	out := captureUserError(t, func() int {
		rc = handleExport([]string{"export", "--format", "json", "--collection", "skills", "--output", outPath})
		return rc
	})
	if rc == 0 {
		t.Fatalf("the export overwrote the orphaned package directory")
	}
	if !strings.Contains(out, "Remove it") {
		t.Errorf("the refusal does not say how to recover:\n%s", out)
	}

	// After the recovery, the export succeeds.
	if err := os.RemoveAll(outPath + packageExportDirectorySuffix); err != nil {
		t.Fatalf("remove the orphaned directory: %v", err)
	}
	if rc := handleExport([]string{"export", "--format", "json", "--collection", "skills", "--output", outPath}); rc != 0 {
		t.Fatalf("the export after the recovery returned %d", rc)
	}
	if _, err := os.Stat(outPath); err != nil {
		t.Fatalf("the export after the recovery wrote no document: %v", err)
	}
}

// TestP7_CLIExportWithAnOutputPathWritesNothingToStdout pins defect B's
// intent behaviourally.
//
// The export's document must go to the file when --output is set, and
// NOTHING may go to standard output: a consumer that redirects stdout
// and reads the file would otherwise get two documents, or a truncated
// file beside a stdout copy.
func TestP7_CLIExportWithAnOutputPathWritesNothingToStdout(t *testing.T) {
	p7CLIImportPackage(t, "cli-stdout-clean")

	outPath := filepath.Join(t.TempDir(), "export.json")
	out, rc := p7CLICaptureStdout(t, func() int {
		return handleExport([]string{"export", "--format", "json", "--collection", "skills", "--output", outPath})
	})
	if rc != 0 {
		t.Fatalf("handleExport returned %d", rc)
	}
	if strings.TrimSpace(out) != "" {
		t.Errorf("the export wrote %d byte(s) to standard output alongside its file:\n%s", len(out), out)
	}
	body, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read the export: %v", err)
	}
	var doc p7CLIExportDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("the export file is not the expected JSON document: %v\n%s", err, body)
	}
	if doc.Archives.Count == 0 {
		t.Errorf("the export file carries no archive")
	}
}

// TestP7_CLITheDocumentKeepsTheModeTheExportHasAlwaysWritten pins the
// document's mode.
//
// The export document is a shareable artefact: it is the file an
// operator hands to a colleague. Its mode is `0644`, as the export has
// always written it, and the mode is set EXPLICITLY — the atomic write
// stages the document through a temporary file, which is `0600`, so a
// silent `rename` would change the published file's mode without
// saying so.
//
// The ARCHIVES stay private: they are the packages' contents.
func TestP7_CLITheDocumentKeepsTheModeTheExportHasAlwaysWritten(t *testing.T) {
	p7CLIImportPackage(t, "cli-mode")

	outPath := filepath.Join(t.TempDir(), "export.json")
	if rc := handleExport([]string{"export", "--format", "json", "--collection", "skills", "--output", outPath}); rc != 0 {
		t.Fatalf("handleExport returned %d", rc)
	}

	info, err := os.Stat(outPath)
	if err != nil {
		t.Fatalf("stat the document: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o644 {
		t.Errorf("the export document's mode is %o, want 644", got)
	}
}

// TestP7_CLITheOmissionsAreSortedGlobally pins that the omissions are
// reported in ONE order.
//
// They arrive from two sources — the export's scope and the exporter's
// own failures — and each is sorted on its own. Without a global sort
// the two lists interleave differently depending on which source an
// omission came from, so the same set of omissions would be reported
// differently on different runs.
func TestP7_CLITheOmissionsAreSortedGlobally(t *testing.T) {
	dm := getDBConcrete()
	if dm == nil {
		t.Skip("DB unavailable")
	}
	firstID, _ := p7CLIImportPackage(t, "cli-sorted-broken-a")
	secondID, _ := p7CLIImportPackage(t, "cli-sorted-broken-b")

	// The restore runs BEFORE the shred: the shred removes the file
	// row the restore reads. (Deferred functions run last-in-first-out,
	// so the shred is registered LAST.)
	for _, id := range []string{firstID, secondID} {
		defer p7CLIShredPackage(t, dm, id)
		defer p7CLIRestoreBlob(t, dm, id, "references/a.md")
	}
	for _, id := range []string{firstID, secondID} {
		if _, err := dm.SQLDB().Exec(`UPDATE skill_package_blobs SET content = ? WHERE sha256 = (
			SELECT f.sha256 FROM skill_package_files f
			WHERE f.skill_id = ? AND f.path = 'references/a.md')`,
			[]byte("# tampered\n"), id); err != nil {
			t.Fatalf("corrupt the blob: %v", err)
		}
	}

	outPath := filepath.Join(t.TempDir(), "export.json")
	if rc := handleExport([]string{"export", "--format", "json", "--collection", "skills", "--output", outPath}); rc != 0 {
		t.Fatalf("handleExport returned %d", rc)
	}
	doc := p7CLIReadDoc(t, outPath)

	var ids []string
	for _, o := range doc.Packages.Packages {
		ids = append(ids, o.SkillID)
	}
	if len(ids) < 2 {
		t.Fatalf("the export named %d omission(s); the fixture breaks two packages", len(ids))
	}
	sorted := append([]string(nil), ids...)
	sort.Strings(sorted)
	for i := range ids {
		if ids[i] != sorted[i] {
			t.Fatalf("the omissions are not sorted: %v", ids)
		}
	}
}

// TestP7_CLIAFailureAfterTheDirectoryIsPublishedSaysSo pins the honest
// reporting of a PARTIAL publication.
//
// The publication is two renames. Once the directory's rename has
// happened, the directory is on disk holding a complete set of
// archives — with no document beside it. An error that said only "the
// export failed" would leave the operator with a directory they did
// not know was there, and the next export would refuse to run into it.
//
// The failure itself is hard to produce deterministically (it needs the
// second rename to fail), so the MESSAGE is pinned directly: it names
// the published directory and says how to recover.
func TestP7_CLIAFailureAfterTheDirectoryIsPublishedSaysSo(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "export.json.packages")
	err := publishedDirectoryError(errors.New("the rename failed"), directory, "publish the export document")
	if err == nil {
		t.Fatalf("the helper returned no error")
	}
	message := err.Error()
	if !strings.Contains(message, directory) {
		t.Errorf("the error does not name the published directory:\n%s", message)
	}
	if !strings.Contains(message, "was published") {
		t.Errorf("the error does not say the directory was published:\n%s", message)
	}
	if !strings.Contains(message, "Remove it and run the export again") {
		t.Errorf("the error does not say how to recover:\n%s", message)
	}
	if !strings.Contains(message, "the rename failed") {
		t.Errorf("the error does not carry the underlying cause:\n%s", message)
	}
}

// TestP7_CLIThePackageFreeDocumentKeepsTheSameMode pins that the
// export document's mode does not depend on whether the export
// carried packages.
//
// The package-bearing path and the package-free path use the SAME
// writer now, and the writer sets the mode explicitly before the
// rename: a temporary file is 0600, because that is what a temporary
// file is, and a rename publishes that mode. Without the explicit
// chmod, the package-free path published a PRIVATE document — a
// silent change an operator would only notice when a colleague could
// not read the file.
func TestP7_CLIThePackageFreeDocumentKeepsTheSameMode(t *testing.T) {
	outPath := filepath.Join(t.TempDir(), "export.json")
	// The `memories` collection carries no packages, so this export
	// takes the package-free path.
	if rc := handleExport([]string{"export", "--format", "json", "--collection", "memories", "--output", outPath}); rc != 0 {
		t.Fatalf("handleExport returned %d", rc)
	}

	info, err := os.Stat(outPath)
	if err != nil {
		t.Fatalf("stat the document: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o644 {
		t.Errorf("the package-free export document's mode is %o, want 644", got)
	}
}

// TestP7_CLIMigrateRefusesADocumentThatDroppedAPackage pins the second
// number at the CLI boundary.
//
// The export writes `package_archives.count` — the archives it
// PRODUCED — and `packages_omitted.count` — the packages it could NOT
// carry. A guard that reads only the first imports a document that
// dropped a package, and the package's content is left behind.
func TestP7_CLIMigrateRefusesADocumentThatDroppedAPackage(t *testing.T) {
	dm := getDBConcrete()
	if dm == nil {
		t.Skip("DB unavailable")
	}
	brokenID, _ := p7CLIImportPackage(t, "cli-dropped")
	defer p7CLIShredPackage(t, dm, brokenID)
	defer p7CLIRestoreBlob(t, dm, brokenID, "references/a.md")
	if _, err := dm.SQLDB().Exec(`UPDATE skill_package_blobs SET content = ? WHERE sha256 = (
		SELECT f.sha256 FROM skill_package_files f
		WHERE f.skill_id = ? AND f.path = 'references/a.md')`,
		[]byte("# tampered\n"), brokenID); err != nil {
		t.Fatalf("corrupt the blob: %v", err)
	}

	outPath := filepath.Join(t.TempDir(), "export.json")
	if rc := handleExport([]string{"export", "--format", "json", "--collection", "skills", "--output", outPath}); rc != 0 {
		t.Fatalf("handleExport returned %d", rc)
	}
	doc := p7CLIReadDoc(t, outPath)

	// The export could not carry the package, and the document SAYS
	// so: the omission marker names it, with the reason.
	//
	// The count is not asserted exactly: the suite's database holds
	// packages from other tests, and a blob is shared by every package
	// whose file has the same content, so corrupting one can drop
	// more than one. What matters is that THIS package is named.
	named := false
	for _, p := range doc.Packages.Packages {
		if strings.HasPrefix(p.Name, "cli-dropped") {
			named = true
			if p.Reason == "" {
				t.Errorf("the omitted package carries no reason: %+v", p)
			}
		}
	}
	if !named {
		t.Fatalf("the export did not name the package it dropped; the omissions are %+v", doc.Packages.Packages)
	}

	// And the migration refuses that document.
	source := filepath.Join(t.TempDir(), "import.json")
	content, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read the document: %v", err)
	}
	if err := os.WriteFile(source, content, 0o600); err != nil {
		t.Fatalf("write the source: %v", err)
	}
	if rc := handleMigrate([]string{"migrate", "--from", source}); rc == 0 {
		t.Fatalf("the migration accepted a document that DROPPED a package")
	}
}
