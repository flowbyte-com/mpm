// skill_package_export_loss_test.go — the export loss marker (R11).
//
// Design of record: PR #222 §6.3 ("The export contract: no silent loss")
// and §13 T44.
//
// The marker is what makes a skills export HONEST while the full package
// export writer does not yet exist. Before P7, `mpm export --collection
// skills` carries the projected skill row only, and a package's
// reference files, scripts and assets are not in the export. The bytes
// exist nowhere else, so the export must SAY SO, by name.

package internal

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// p4ExportPackage imports a small package and returns its skill id.
func p4ExportPackage(t *testing.T, dm *DatabaseManager, name string) string {
	t.Helper()
	root := t.TempDir()
	entry := p4SkillDoc(name, "1.0.0", "body\n")
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
	result, err := dm.ImportSkillPackageFromDir(PackageImportOptions{Path: root})
	if err != nil {
		t.Fatalf("import %s: %v", name, err)
	}
	return result.SkillID
}

// TestP4_T44_TheExportNamesEveryPackageItDidNotCarry pins T44.
//
// The loss marker carries the COUNT and the IDENTITY of every package
// the export did not carry.
func TestP4_T44_TheExportNamesEveryPackageItDidNotCarry(t *testing.T) {
	dm := p3DM(t)

	skillID := p4ExportPackage(t, dm, "lossy-package")

	loss, err := dm.GetSkillPackageExportLoss("skills", "", "")
	if err != nil {
		t.Fatalf("the loss marker: %v", err)
	}
	if loss.Count != 1 {
		t.Fatalf("the loss marker reports %d omitted packages, want 1", loss.Count)
	}
	if len(loss.Packages) != 1 {
		t.Fatalf("the loss marker names %d packages, want 1", len(loss.Packages))
	}
	omitted := loss.Packages[0]
	if omitted.SkillID != skillID {
		t.Errorf("the omitted package is %q, want %q", omitted.SkillID, skillID)
	}
	if omitted.Name != "lossy-package" {
		t.Errorf("the omitted package's name = %q, want lossy-package", omitted.Name)
	}
	if omitted.Version != "1.0.0" {
		t.Errorf("the omitted package's version = %q, want 1.0.0", omitted.Version)
	}
	if omitted.FileCount != 2 {
		t.Errorf("the omitted package's file count = %d, want 2", omitted.FileCount)
	}
	if len(omitted.PackageHash) != 64 {
		t.Errorf("the omitted package's hash = %q, want a 64-character SHA-256", omitted.PackageHash)
	}

	// The notice NAMES the affected package. The marker is not a warning
	// that can be missed.
	notice := loss.Notice()
	if !strings.Contains(notice, skillID) {
		t.Errorf("the notice does not name the omitted package:\n%s", notice)
	}
	if !strings.Contains(notice, "1 skill package(s)") {
		t.Errorf("the notice does not carry the count:\n%s", notice)
	}
}

// TestP4_T44_AnExportOfOrdinarySkillsCarriesNoLoss pins the other half:
// an export whose scope carries no package reports a count of zero.
func TestP4_T44_AnExportOfOrdinarySkillsCarriesNoLoss(t *testing.T) {
	dm := p3DM(t)

	// An ordinary skill, with no package.
	doc := p4SkillDoc("ordinary-only", "1.0.0", "body\n")
	if _, err := dm.SaveSkill("ordinary-only", "1.0.0", doc, "cli", false); err != nil {
		t.Fatalf("save: %v", err)
	}

	loss, err := dm.GetSkillPackageExportLoss("skills", "", "")
	if err != nil {
		t.Fatalf("the loss marker: %v", err)
	}
	if loss.Count != 0 {
		t.Errorf("the loss marker reports %d omitted packages, want 0", loss.Count)
	}
	if loss.Notice() != "" {
		t.Errorf("the notice is not empty for an export that lost nothing:\n%s", loss.Notice())
	}
}

// TestP4_T44_AnExportOfAnotherCollectionCarriesNoLoss pins that only the
// skills collection can lose a package.
func TestP4_T44_AnExportOfAnotherCollectionCarriesNoLoss(t *testing.T) {
	dm := p3DM(t)

	p4ExportPackage(t, dm, "lossy-but-not-exported")

	loss, err := dm.GetSkillPackageExportLoss("memories", "", "")
	if err != nil {
		t.Fatalf("the loss marker: %v", err)
	}
	if loss.Count != 0 {
		t.Errorf("a memories export reports %d omitted packages, want 0", loss.Count)
	}
}

// TestP4_T44_TheMarkerIsAlwaysPresent pins the response's shape: the
// marker exists even when nothing was omitted, so a consumer never has to
// branch on whether the field is there.
func TestP4_T44_TheMarkerIsAlwaysPresent(t *testing.T) {
	dm := p3DM(t)

	loss, err := dm.GetSkillPackageExportLoss("", "", "")
	if err != nil {
		t.Fatalf("the loss marker: %v", err)
	}
	if loss == nil {
		t.Fatal("the loss marker is nil; it must always be present")
	}
	if loss.Packages == nil {
		t.Errorf("the marker carries a nil package list; it must be an empty list")
	}
}

// TestP4_T44_EveryPackageIsNamed pins that the marker names ALL of them,
// not just the first.
func TestP4_T44_EveryPackageIsNamed(t *testing.T) {
	dm := p3DM(t)

	first := p4ExportPackage(t, dm, "lossy-one")
	second := p4ExportPackage(t, dm, "lossy-two")
	third := p4ExportPackage(t, dm, "lossy-three")

	loss, err := dm.GetSkillPackageExportLoss("skills", "", "")
	if err != nil {
		t.Fatalf("the loss marker: %v", err)
	}
	if loss.Count != 3 {
		t.Fatalf("the loss marker reports %d omitted packages, want 3", loss.Count)
	}
	notice := loss.Notice()
	for _, id := range []string{first, second, third} {
		if !strings.Contains(notice, id) {
			t.Errorf("the notice does not name %s:\n%s", id, notice)
		}
	}
}
