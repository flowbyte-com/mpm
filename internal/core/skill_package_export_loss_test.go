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
// The export's SCOPE carries the COUNT and the IDENTITY of every package
// the export is about.
//
// P7 changed what this method reports. Before P7 the export carried no
// package content at all, so the scope and the omissions were the same
// list. P7 makes the export COMPLETE: the scope is what the export is
// ABOUT, and the omissions are what it could not carry — a difference
// the export reports through the marker it now builds from its own
// omissions. The scope is what the marker is measured against, so it is
// what this test pins.
func TestP4_T44_TheExportNamesEveryPackageItDidNotCarry(t *testing.T) {
	dm := p3DM(t)

	skillID := p4ExportPackage(t, dm, "lossy-package")

	scope, err := dm.SkillPackageExportScope("skills", "", "")
	if err != nil {
		t.Fatalf("the export scope: %v", err)
	}
	if len(scope.Packages) != 1 {
		t.Fatalf("the export scope names %d packages, want 1", len(scope.Packages))
	}
	omitted := scope.Packages[0]
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
	loss := &SkillPackageExportLoss{Count: len(scope.Packages), Packages: scope.Packages}
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

	scope, err := dm.SkillPackageExportScope("skills", "", "")
	if err != nil {
		t.Fatalf("the export scope: %v", err)
	}
	if len(scope.Packages) != 0 {
		t.Errorf("the export scope names %d packages, want 0", len(scope.Packages))
	}
	loss := &SkillPackageExportLoss{Count: 0, Packages: scope.Packages}
	if loss.Notice() != "" {
		t.Errorf("the notice is not empty for an export that lost nothing:\n%s", loss.Notice())
	}
}

// TestP4_T44_AnExportOfAnotherCollectionCarriesNoLoss pins that only the
// skills collection can lose a package.
func TestP4_T44_AnExportOfAnotherCollectionCarriesNoLoss(t *testing.T) {
	dm := p3DM(t)

	p4ExportPackage(t, dm, "lossy-but-not-exported")

	scope, err := dm.SkillPackageExportScope("memories", "", "")
	if err != nil {
		t.Fatalf("the export scope: %v", err)
	}
	if len(scope.Packages) != 0 {
		t.Errorf("a memories export names %d packages, want 0", len(scope.Packages))
	}
}

// TestP4_T44_TheScopeCountsTheStoreWhateverTheCollection pins the
// scope's TOTAL.
//
// The total is the number of packages in the STORE — every package,
// whatever the export's filters chose. It is the denominator that makes
// an omission count readable, so it has to be the store's count and not
// the selected count.
//
// The early return for a non-skills collection used to skip the count,
// so the scope reported `total: 0` for a store that held packages. A
// report that says "0 packages are in the store" when three are is a
// false statement about the store.
func TestP4_T44_TheScopeCountsTheStoreWhateverTheCollection(t *testing.T) {
	dm := p3DM(t)

	p4ExportPackage(t, dm, "counted-one")
	p4ExportPackage(t, dm, "counted-two")
	p4ExportPackage(t, dm, "counted-three")

	skills, err := dm.SkillPackageExportScope("skills", "", "")
	if err != nil {
		t.Fatalf("the skills export scope: %v", err)
	}
	if skills.Total != 3 {
		t.Errorf("the skills scope reports %d package(s) in the store, want 3", skills.Total)
	}

	memories, err := dm.SkillPackageExportScope("memories", "", "")
	if err != nil {
		t.Fatalf("the memories export scope: %v", err)
	}
	if memories.Total != 3 {
		t.Errorf("a memories export reports %d package(s) in the store, want 3: "+
			"the total is the STORE's count, whatever the filters chose",
			memories.Total)
	}
	if len(memories.Packages) != 0 {
		t.Errorf("a memories export names %d packages, want 0", len(memories.Packages))
	}
}

// TestP4_T44_TheMarkerIsAlwaysPresent pins the response's shape: the
// marker exists even when nothing was omitted, so a consumer never has to
// branch on whether the field is there.
func TestP4_T44_TheMarkerIsAlwaysPresent(t *testing.T) {
	dm := p3DM(t)

	scope, err := dm.SkillPackageExportScope("", "", "")
	if err != nil {
		t.Fatalf("the export scope: %v", err)
	}
	if scope == nil {
		t.Fatal("the export scope is nil; it must always be present")
	}
	if scope.Packages == nil {
		t.Errorf("the scope carries a nil package list; it must be an empty list")
	}
}

// TestP4_T44_EveryPackageIsNamed pins that the marker names ALL of them,
// not just the first.
func TestP4_T44_EveryPackageIsNamed(t *testing.T) {
	dm := p3DM(t)

	first := p4ExportPackage(t, dm, "lossy-one")
	second := p4ExportPackage(t, dm, "lossy-two")
	third := p4ExportPackage(t, dm, "lossy-three")

	scope, err := dm.SkillPackageExportScope("skills", "", "")
	if err != nil {
		t.Fatalf("the export scope: %v", err)
	}
	if len(scope.Packages) != 3 {
		t.Fatalf("the export scope names %d packages, want 3", len(scope.Packages))
	}
	loss := &SkillPackageExportLoss{Count: len(scope.Packages), Packages: scope.Packages}
	notice := loss.Notice()
	for _, id := range []string{first, second, third} {
		if !strings.Contains(notice, id) {
			t.Errorf("the notice does not name %s:\n%s", id, notice)
		}
	}
}
