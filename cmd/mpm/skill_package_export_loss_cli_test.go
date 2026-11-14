// skill_package_export_loss_cli_test.go — the export loss marker at the
// CLI boundary, and the JSON export compatibility policy.
//
// Design of record: PR #222 §6.3 (R11) and §13 T44.
//
// The marker must be present in BOTH places the design names: the export
// RESPONSE and the CLI's visible output. This file pins the CLI half —
// the JSON document carries the structured marker, and the terminal shows
// a notice that names every affected package.
//
// It also pins the COMPATIBILITY POLICY: the structured object is the
// default, the deprecated bare-array format is available only when
// nothing is omitted, and the structured document stays consumable by
// "mpm migrate --from".
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
// So the expected count is computed from the database, and every package
// the marker names is checked against the packages the database actually
// holds. The assertion is about ACCURACY — the marker names exactly the
// packages whose skill rows the export carried — which is the property
// that matters, and it holds no matter what else is in the database.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// p4CLIExportDoc is the JSON document the export emits.
type p4CLIExportDoc struct {
	Memories []map[string]interface{} `json:"memories"`
	Packages struct {
		Count    int `json:"count"`
		Packages []struct {
			SkillID     string `json:"skill_id"`
			Name        string `json:"name"`
			Version     string `json:"version"`
			FileCount   int    `json:"file_count"`
			PackageHash string `json:"package_hash"`
		} `json:"packages"`
	} `json:"packages_omitted"`
}

// p4CLICounter makes every fixture name unique, so a repeated run
// ("go test -count=2") never collides with the previous iteration's rows
// in the process-wide singleton database.
var p4CLICounter atomic.Int64

// p4CLIImportPackage imports a small package into the CLI's database and
// returns its skill id and its name.
//
// The name is unique per call, so the helper is safe to call from a
// repeated run.
func p4CLIImportPackage(t *testing.T, name string) (string, string) {
	t.Helper()
	dm := getDBConcrete()
	if dm == nil {
		t.Skip("DB unavailable")
	}
	unique := fmt.Sprintf("%s-%d-%d", name, os.Getpid(), p4CLICounter.Add(1))
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
	if err := os.WriteFile(filepath.Join(refs, "a.md"), []byte("# A\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	result, err := dm.ImportSkillPackageFromDir(mpminternal.PackageImportOptions{Path: root})
	if err != nil {
		t.Fatalf("import %s: %v", unique, err)
	}
	return result.SkillID, unique
}

// p4CLIPackagesInScope counts the packages the skills export carries, the
// same way the marker counts them.
//
// It is the EXPECTED value the marker is checked against, computed from
// the database rather than hardcoded.
func p4CLIPackagesInScope(t *testing.T, dm *mpminternal.DatabaseManager) map[string]int {
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

// TestP4_CLIExportCarriesThePackageLossMarker pins that the skills
// export's JSON response carries the marker: the count and every
// affected package's identity.
func TestP4_CLIExportCarriesThePackageLossMarker(t *testing.T) {
	dm := getDBConcrete()
	if dm == nil {
		t.Skip("DB unavailable")
	}
	skillID, name := p4CLIImportPackage(t, "cli-lossy-package")
	expected := p4CLIPackagesInScope(t, dm)

	outPath := filepath.Join(t.TempDir(), "export.json")
	if rc := handleExport([]string{"export", "--format", "json", "--collection", "skills", "--output", outPath}); rc != 0 {
		t.Fatalf("handleExport returned %d", rc)
	}

	body, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read the export: %v", err)
	}
	var doc p4CLIExportDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("the export is not the expected JSON document: %v\n%s", err, body)
	}

	// The count is the number of packages in scope — computed, not
	// hardcoded, so a repeated run cannot break it.
	if doc.Packages.Count != len(expected) {
		t.Fatalf("the export reports %d omitted packages, want %d\n%s",
			doc.Packages.Count, len(expected), body)
	}
	if len(doc.Packages.Packages) != len(expected) {
		t.Fatalf("the export names %d omitted packages, want %d", len(doc.Packages.Packages), len(expected))
	}

	// Every package in scope is named, with its identity and its file
	// count.
	seen := map[string]bool{}
	for _, omitted := range doc.Packages.Packages {
		seen[omitted.SkillID] = true
		files, ok := expected[omitted.SkillID]
		if !ok {
			t.Errorf("the export names %s, which is not a package in scope", omitted.SkillID)
			continue
		}
		if omitted.FileCount != files {
			t.Errorf("the export reports %d files omitted for %s, want %d",
				omitted.FileCount, omitted.SkillID, files)
		}
		if omitted.Name == "" || omitted.Version == "" || omitted.PackageHash == "" {
			t.Errorf("the export's entry for %s is incomplete: %+v", omitted.SkillID, omitted)
		}
	}
	for id := range expected {
		if !seen[id] {
			t.Errorf("the export does not name the package %s", id)
		}
	}

	// The package just imported is named, with its own name.
	var found bool
	for _, omitted := range doc.Packages.Packages {
		if omitted.SkillID == skillID {
			found = true
			if omitted.Name != name {
				t.Errorf("the export names the package %q, want %q", omitted.Name, name)
			}
			if omitted.FileCount != 2 {
				t.Errorf("the export reports %d files omitted, want 2", omitted.FileCount)
			}
			if omitted.PackageHash == "" {
				t.Errorf("the export carries no package hash")
			}
		}
	}
	if !found {
		t.Errorf("the export does not name the imported package %s", skillID)
	}

	// The projected skill row IS in the export — the marker says the
	// CONTENT was omitted, not the row.
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

// TestP4_CLIExportPrintsTheLossNotice pins that the loss is visible on
// the terminal, so a caller who never reads the response still sees it.
func TestP4_CLIExportPrintsTheLossNotice(t *testing.T) {
	skillID, _ := p4CLIImportPackage(t, "cli-noticed-package")

	outPath := filepath.Join(t.TempDir(), "export.json")
	var rc int
	// The notice goes through usererror, whose writer is captured
	// explicitly — it is bound to os.Stderr at init time, so replacing
	// os.Stderr does not redirect it.
	notice := captureUserError(t, func() int {
		rc = handleExport([]string{"export", "--format", "json", "--collection", "skills", "--output", outPath})
		return rc
	})
	if rc != 0 {
		t.Fatalf("handleExport returned %d", rc)
	}

	if !strings.Contains(notice, skillID) {
		t.Errorf("the export output does not name the omitted package %s:\n%s", skillID, notice)
	}
	if !strings.Contains(notice, "omitted") {
		t.Errorf("the export output does not report the omission:\n%s", notice)
	}
}

// TestP4_CLIExportOfOrdinarySkillsCarriesNoLoss pins that an ordinary
// skill is never reported as an omitted PACKAGE.
//
// The CLI's database is a process-wide singleton, so the marker is
// checked for ACCURACY against the database rather than against an
// absolute count: it names exactly the packages whose skill rows the
// export carried, and an ordinary skill is never among them.
func TestP4_CLIExportOfOrdinarySkillsCarriesNoLoss(t *testing.T) {
	dm := getDBConcrete()
	if dm == nil {
		t.Skip("DB unavailable")
	}
	// An ordinary skill with no package. The name is unique per call, so
	// a repeated run does not collide with the previous iteration's row.
	unique := fmt.Sprintf("cli-ordinary-only-%d-%d", os.Getpid(), p4CLICounter.Add(1))
	doc := "---\nname: " + unique + "\nversion: 1.0.0\ndescription: a test skill\n" +
		"when_to_use: when a test needs this exact skill\n---\nbody\n"
	ordinaryID, err := dm.SaveSkill(unique, "1.0.0", doc, "cli", false)
	if err != nil {
		t.Fatalf("save: %v", err)
	}

	outPath := filepath.Join(t.TempDir(), "export.json")
	if rc := handleExport([]string{"export", "--format", "json", "--collection", "skills", "--output", outPath}); rc != 0 {
		t.Fatalf("handleExport returned %d", rc)
	}
	body, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read the export: %v", err)
	}
	var doc2 p4CLIExportDoc
	if err := json.Unmarshal(body, &doc2); err != nil {
		t.Fatalf("the export is not the expected JSON document: %v\n%s", err, body)
	}

	// The marker is accurate: it names exactly the packages whose skill
	// rows the export carried.
	expected := p4CLIPackagesInScope(t, dm)
	if doc2.Packages.Count != len(expected) {
		t.Errorf("the export reports %d omitted packages, want %d", doc2.Packages.Count, len(expected))
	}
	for _, p := range doc2.Packages.Packages {
		if p.SkillID == ordinaryID {
			t.Errorf("the marker names the ordinary skill %s as an omitted package", ordinaryID)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// THE COMPATIBILITY POLICY
// ─────────────────────────────────────────────────────────────────────────────

// TestP4_CLITheStructuredExportStaysConsumableByMigrate pins the
// coupling between the export writer and the migrate parser.
//
// The export's top-level key is "memories", and `mpm migrate --from`
// reads a "memories" array. That coupling is REAL and it is what keeps
// an export file a valid input for the migrate command — but it was
// incidental, and nothing pinned it.
//
// This test pins it: the document the export actually emits is fed to
// the substrate's own JSON parser, and the rows come back.
func TestP4_CLITheStructuredExportStaysConsumableByMigrate(t *testing.T) {
	dm := getDBConcrete()
	if dm == nil {
		t.Skip("DB unavailable")
	}
	skillID, unique := p4CLIImportPackage(t, "cli-migrateable")

	outPath := filepath.Join(t.TempDir(), "export.json")
	if rc := handleExport([]string{"export", "--format", "json", "--output", outPath}); rc != 0 {
		t.Fatalf("handleExport returned %d", rc)
	}
	body, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read the export: %v", err)
	}

	// The document is an OBJECT, and the object carries "memories".
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(body, &probe); err != nil {
		t.Fatalf("the export is not a JSON object: %v\n%s", err, body)
	}
	if _, ok := probe["memories"]; !ok {
		t.Fatalf("the export document has no \"memories\" key: %s", body)
	}
	if _, ok := probe["packages_omitted"]; !ok {
		t.Fatalf("the export document has no \"packages_omitted\" key: %s", body)
	}

	// And the migrate parser accepts it.
	facts, err := mpminternal.ParseJsonFacts(string(body), outPath)
	if err != nil {
		t.Fatalf("the export document is NOT consumable by mpm migrate --from: %v", err)
	}
	if len(facts) == 0 {
		t.Fatalf("the migrate parser read no facts from the export")
	}
	t.Logf("mpm migrate --from reads %d facts from the export", len(facts))

	// The exported package's skill row is among them: the migrate parser
	// reads each entry's "content", which for a skill row is the
	// projected document.
	var found bool
	for _, f := range facts {
		if strings.Contains(f.Content, unique) {
			found = true
		}
	}
	if !found {
		t.Errorf("the migrated facts do not include the exported skill row %s", skillID)
	}
}

// TestP4_CLITheLegacyArrayFormatIsRefusedWhenContentWouldBeOmitted pins
// the safe half of the compatibility policy.
//
// The deprecated bare array has nowhere to carry the loss marker. If a
// package's content would be omitted, emitting the bare array would drop
// those bytes SILENTLY — the exact loss R11 exists to prevent. So the
// legacy format is REFUSED in that case, with an error that names the
// packages and points at the structured format.
func TestP4_CLITheLegacyArrayFormatIsRefusedWhenContentWouldBeOmitted(t *testing.T) {
	skillID, _ := p4CLIImportPackage(t, "cli-legacy-refused")

	outPath := filepath.Join(t.TempDir(), "export.json")
	var rc int
	out := captureUserError(t, func() int {
		rc = handleExport([]string{"export", "--format", "json-array", "--collection", "skills", "--output", outPath})
		return rc
	})
	if rc == 0 {
		t.Fatalf("the legacy array format was accepted while a package would be omitted")
	}
	if !strings.Contains(out, skillID) {
		t.Errorf("the refusal does not name the omitted package %s:\n%s", skillID, out)
	}
	if !strings.Contains(out, "json") {
		t.Errorf("the refusal does not point at the structured format:\n%s", out)
	}

	// Nothing was written.
	if _, err := os.Stat(outPath); err == nil {
		body, _ := os.ReadFile(outPath)
		if len(strings.TrimSpace(string(body))) != 0 {
			t.Errorf("the refused legacy export wrote a document:\n%s", body)
		}
	}
}

// TestP4_CLITheLegacyArrayFormatWorksWhenNothingIsOmitted pins the
// usable half of the compatibility policy.
//
// An export of a collection that cannot lose a package — anything but
// "skills" — has nothing to omit, so the deprecated bare array is
// available and produces the legacy shape.
func TestP4_CLITheLegacyArrayFormatWorksWhenNothingIsOmitted(t *testing.T) {
	outPath := filepath.Join(t.TempDir(), "export.json")
	var rc int
	out := captureUserError(t, func() int {
		rc = handleExport([]string{"export", "--format", "json-array", "--collection", "directives", "--output", outPath})
		return rc
	})
	if rc != 0 {
		t.Fatalf("the legacy array format was refused for a collection that omits nothing: %s", out)
	}

	body, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read the export: %v", err)
	}
	// The legacy shape is a BARE ARRAY.
	var rows []map[string]interface{}
	if err := json.Unmarshal(body, &rows); err != nil {
		t.Fatalf("the legacy export is not a bare JSON array: %v\n%s", err, body)
	}
	t.Logf("the legacy export carried %d row(s)", len(rows))

	// And the deprecation is announced, on standard error, so the
	// document on standard output stays parseable.
	if !strings.Contains(out, "DEPRECATED") {
		t.Errorf("the legacy format did not announce its deprecation:\n%s", out)
	}
}

// TestP4_CLIAnUnknownExportFormatIsRefused pins that a mistyped format
// is an error rather than a silent empty export.
func TestP4_CLIAnUnknownExportFormatIsRefused(t *testing.T) {
	outPath := filepath.Join(t.TempDir(), "export.json")
	var rc int
	out := captureUserError(t, func() int {
		rc = handleExport([]string{"export", "--format", "jsonl", "--output", outPath})
		return rc
	})
	if rc == 0 {
		t.Fatalf("an unknown export format was accepted")
	}
	if !strings.Contains(out, "jsonl") {
		t.Errorf("the refusal does not name the bad format:\n%s", out)
	}
}
