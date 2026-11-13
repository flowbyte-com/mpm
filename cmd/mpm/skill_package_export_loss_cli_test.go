// skill_package_export_loss_cli_test.go — the export loss marker at the
// CLI boundary.
//
// Design of record: PR #222 §6.3 (R11) and §13 T44.
//
// The marker must be present in BOTH places the design names: the export
// RESPONSE and the CLI's visible output. This file pins the CLI half —
// the JSON document carries the structured marker, and the terminal shows
// a notice that names every affected package.
//
// The tests run against the package's disposable test database (the
// package's TestMain pins HOME to the temporary root), never against the
// operator's installation.

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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

// p4CLIImportPackage imports a small package into the CLI's database and
// returns its skill id.
func p4CLIImportPackage(t *testing.T, name string) string {
	t.Helper()
	dm := getDBConcrete()
	if dm == nil {
		t.Skip("DB unavailable")
	}
	root := t.TempDir()
	entry := "---\nname: " + name + "\nversion: 1.0.0\ndescription: a test skill\n" +
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
		t.Fatalf("import %s: %v", name, err)
	}
	return result.SkillID
}

// TestP4_CLIExportCarriesThePackageLossMarker pins that the skills
// export's JSON response carries the marker: the count and every
// affected package's identity.
func TestP4_CLIExportCarriesThePackageLossMarker(t *testing.T) {
	skillID := p4CLIImportPackage(t, "cli-lossy-package")

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

	if doc.Packages.Count != 1 {
		t.Fatalf("the export reports %d omitted packages, want 1\n%s", doc.Packages.Count, body)
	}
	if len(doc.Packages.Packages) != 1 {
		t.Fatalf("the export names %d omitted packages, want 1", len(doc.Packages.Packages))
	}
	omitted := doc.Packages.Packages[0]
	if omitted.SkillID != skillID {
		t.Errorf("the export names %q, want %q", omitted.SkillID, skillID)
	}
	if omitted.Name != "cli-lossy-package" {
		t.Errorf("the export names the package %q, want cli-lossy-package", omitted.Name)
	}
	if omitted.FileCount != 2 {
		t.Errorf("the export reports %d files omitted, want 2", omitted.FileCount)
	}
	if omitted.PackageHash == "" {
		t.Errorf("the export carries no package hash")
	}

	// The projected skill row IS in the export — the marker says the
	// CONTENT was omitted, not the row.
	var found bool
	for _, m := range doc.Memories {
		if id, _ := m["id"].(string); id == skillID {
			found = true
		}
	}
	if !found {
		t.Errorf("the exported skill row %s is missing from the export", skillID)
	}
}

// TestP4_CLIExportPrintsTheLossNotice pins that the loss is visible on
// the terminal, so a caller who never reads the response still sees it.
func TestP4_CLIExportPrintsTheLossNotice(t *testing.T) {
	skillID := p4CLIImportPackage(t, "cli-noticed-package")

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
	// An ordinary skill with no package.
	doc := "---\nname: cli-ordinary-only\nversion: 1.0.0\ndescription: a test skill\n" +
		"when_to_use: when a test needs this exact skill\n---\nbody\n"
	ordinaryID, err := dm.SaveSkill("cli-ordinary-only", "1.0.0", doc, "cli", false)
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
	var expected int
	if err := dm.SQLDB().QueryRow(`
		SELECT COUNT(*) FROM skill_packages p
		JOIN memories m ON m.id = p.skill_id
		WHERE m.deleted_at IS NULL AND m.collection = 'skills'
	`).Scan(&expected); err != nil {
		t.Fatalf("count the packages: %v", err)
	}
	if doc2.Packages.Count != expected {
		t.Errorf("the export reports %d omitted packages, want %d", doc2.Packages.Count, expected)
	}
	for _, p := range doc2.Packages.Packages {
		if p.SkillID == ordinaryID {
			t.Errorf("the marker names the ordinary skill %s as an omitted package", ordinaryID)
		}
	}
}
