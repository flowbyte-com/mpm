// skill_prune_exemption_test.go — Stage P3: the blanket skill exclusion
// from age-based pruning (operator decision O17, approved; regression
// case T41).
//
// The exclusion is BLANKET: `collection = 'skills'` rows are never deleted
// by PruneOlderThan, whether or not they own a package. The design
// explicitly warns that the blast radius is wider than "packages", and
// this file pins that wider behaviour so it cannot drift back silently.
//
// Every test here runs against a DISPOSABLE database in a disposable
// workspace (see p3DM in skill_tx_test.go). The live MPM installation and
// its database are never touched.

package internal

import (
	"fmt"
	"testing"
	"time"
)

// p3PruneFixture is the T41 database: an old ordinary memory, an old
// single-file skill, an old packaged skill, several skill versions, an old
// directive and a recent ordinary memory.
//
// Every "old" row carries created_at = 1, so any cutoff above 1 makes it
// eligible. The recent row carries a current timestamp, so no cutoff below
// now can reach it.
type p3PruneFixture struct {
	oldMemoryID    string
	recentMemoryID string
	directiveID    string
	singleSkillID  string
	packagedID     string
	versionIDs     []string
}

// p3SeedPruneFixture builds the T41 database.
func p3SeedPruneFixture(t *testing.T, dm *DatabaseManager) p3PruneFixture {
	t.Helper()

	fx := p3PruneFixture{
		oldMemoryID:    "mem:old-ordinary",
		recentMemoryID: "mem:recent-ordinary",
		directiveID:    "mem:old-directive",
		singleSkillID:  "skill:plain-v1.0.0",
		packagedID:     "skill:packaged-v1.0.0",
	}

	// An old ordinary memory — eligible for the prune.
	if _, err := dm.SQLDB().Exec(
		`INSERT INTO memories (id, collection, content, created_at) VALUES (?, 'memories', ?, 1)`,
		fx.oldMemoryID, "an old ordinary memory"); err != nil {
		t.Fatalf("insert the old ordinary memory: %v", err)
	}

	// A recent ordinary memory — NOT eligible, whatever the cutoff is
	// below now.
	now := time.Now().Unix()
	if _, err := dm.SQLDB().Exec(
		`INSERT INTO memories (id, collection, content, created_at) VALUES (?, 'memories', ?, ?)`,
		fx.recentMemoryID, "a recent ordinary memory", now); err != nil {
		t.Fatalf("insert the recent ordinary memory: %v", err)
	}

	// An old directive — exempt by the PRE-EXISTING directive exclusion,
	// which the skills exclusion must not disturb.
	if _, err := dm.SQLDB().Exec(
		`INSERT INTO memories (id, collection, content, is_prime_directive, created_at)
		 VALUES (?, 'directives', ?, 1, 1)`,
		fx.directiveID, "an old directive"); err != nil {
		t.Fatalf("insert the old directive: %v", err)
	}

	// An old single-file skill — no package row. It must survive anyway:
	// the exclusion is BLANKET, not package-aware.
	if _, err := dm.SQLDB().Exec(
		`INSERT INTO memories (id, collection, content, created_at) VALUES (?, 'skills', ?, 1)`,
		fx.singleSkillID, "an old single-file skill"); err != nil {
		t.Fatalf("insert the old single-file skill: %v", err)
	}

	// An old packaged skill, with its blob, package row and file row.
	// p3SeedPackagedSkill saves the skill through the ordinary path; the
	// created_at is then forced back to 1 so the row is old enough for
	// the prune to reach if the exclusion were absent.
	packaged := p3SeedPackagedSkill(t, dm, "packaged", "1.0.0")
	fx.packagedID = packaged.skillID
	if _, err := dm.SQLDB().Exec(
		`UPDATE memories SET created_at = 1 WHERE id = ?`, fx.packagedID); err != nil {
		t.Fatalf("age the packaged skill: %v", err)
	}

	// Several versions of ONE skill name, all old. Every version must
	// receive the same protection — an age-based prune must not be able
	// to remove the old versions and leave the newest one behind.
	for _, v := range []string{"1.0.0", "1.1.0", "1.2.0"} {
		id := fmt.Sprintf("skill:multi-v%s", v)
		if _, err := dm.SQLDB().Exec(
			`INSERT INTO memories (id, collection, content, created_at) VALUES (?, 'skills', ?, 1)`,
			id, "a version of a multi-version skill"); err != nil {
			t.Fatalf("insert the multi-version skill %s: %v", v, err)
		}
		fx.versionIDs = append(fx.versionIDs, id)
	}

	return fx
}

// p3RowExists reports whether a memories row exists, tombstoned or not.
func p3RowExists(t *testing.T, dm *DatabaseManager, id string) bool {
	t.Helper()
	var n int
	if err := dm.SQLDB().QueryRow(`SELECT COUNT(*) FROM memories WHERE id = ?`, id).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", id, err)
	}
	return n > 0
}

// TestPruneOlderThanNeverRemovesASkill is the T41 regression: the
// age-based prune removes the eligible ordinary memory and NOTHING else.
//
// The fixture holds one eligible row (the old ordinary memory). The
// reported count is asserted to be exactly 1 — the exclusion is a
// predicate on the delete statement, so it counts the rows that were
// deleted, never the rows it skipped.
func TestPruneOlderThanNeverRemovesASkill(t *testing.T) {
	dm := p3DM(t)
	fx := p3SeedPruneFixture(t, dm)

	beforePkg, beforeFiles, beforeBlob := p3PackageState(t, dm, fx.packagedID)

	// A cutoff that makes every "old" row eligible.
	removed, err := dm.PruneOlderThan(1000)
	if err != nil {
		t.Fatalf("PruneOlderThan: %v", err)
	}
	if removed != 1 {
		t.Errorf("the prune reported %d deletions, want exactly 1 (the old ordinary memory)", removed)
	}

	// The eligible ordinary memory is gone.
	if p3RowExists(t, dm, fx.oldMemoryID) {
		t.Error("the old ordinary memory survived the prune")
	}

	// Every skill survives — packaged or not, one version or many.
	if !p3RowExists(t, dm, fx.singleSkillID) {
		t.Error("the old single-file skill was removed by the age-based prune")
	}
	if !p3RowExists(t, dm, fx.packagedID) {
		t.Error("the old packaged skill was removed by the age-based prune")
	}
	for _, id := range fx.versionIDs {
		if !p3RowExists(t, dm, id) {
			t.Errorf("the skill version %s was removed by the age-based prune", id)
		}
	}

	// The directive survives — the pre-existing exemption still works.
	if !p3RowExists(t, dm, fx.directiveID) {
		t.Error("the old directive was removed by the age-based prune")
	}

	// The recent ordinary memory survives.
	if !p3RowExists(t, dm, fx.recentMemoryID) {
		t.Error("the recent ordinary memory was removed by the age-based prune")
	}

	// The package manifest, the file row and the blob are intact.
	afterPkg, afterFiles, afterBlob := p3PackageState(t, dm, fx.packagedID)
	if afterPkg != beforePkg {
		t.Errorf("the prune changed the package row:\n  before: %s\n  after:  %s", beforePkg, afterPkg)
	}
	if afterFiles != beforeFiles {
		t.Errorf("the prune changed the file row:\n  before: %s\n  after:  %s", beforeFiles, afterFiles)
	}
	if afterBlob != beforeBlob {
		t.Errorf("the prune changed the blob:\n  before: %q\n  after:  %q", beforeBlob, afterBlob)
	}
}

// TestPruneOlderThanProtectsEverySkillVersion pins the per-version
// guarantee: every version of a skill is protected, not just the one the
// read path happens to resolve.
//
// An age-based prune that removed only the OLD versions would leave a
// name with a newer version and no history — a state the read path would
// silently present as complete.
func TestPruneOlderThanProtectsEverySkillVersion(t *testing.T) {
	dm := p3DM(t)
	fx := p3SeedPruneFixture(t, dm)

	if _, err := dm.PruneOlderThan(1000); err != nil {
		t.Fatalf("PruneOlderThan: %v", err)
	}

	for _, id := range fx.versionIDs {
		var n int
		if err := dm.SQLDB().QueryRow(
			`SELECT COUNT(*) FROM memories WHERE id = ? AND collection = 'skills'`, id).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", id, err)
		}
		if n != 1 {
			t.Errorf("the skill version %s did not survive the prune (count=%d)", id, n)
		}
	}
}

// TestPruneOlderThanLeavesDeliberateDeletionWorking pins that the
// exemption removes the UNINTENTIONAL bulk delete only: the operator's
// deliberate removal paths still work.
//
// `mpm skill delete` soft-shreds; `mpm skill shred` hard-deletes and the
// foreign keys cascade to the package row and the file rows. The blob
// becomes an unreferenced garbage-collection candidate — that is the
// design's §5.8 contract, and it is unchanged here.
func TestPruneOlderThanLeavesDeliberateDeletionWorking(t *testing.T) {
	dm := p3DM(t)
	fx := p3SeedPruneFixture(t, dm)

	// The soft shred tombstones the skill; the package row survives it,
	// which is exactly why the immutability guard still finds it.
	if err := dm.ShredSkill(fx.packagedID); err != nil {
		t.Fatalf("ShredSkill: %v", err)
	}
	if !p3RowExists(t, dm, fx.packagedID) {
		t.Fatal("the soft shred removed the skill row; it must tombstone it")
	}
	var pkgRows int
	if err := dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM skill_packages WHERE skill_id = ?`, fx.packagedID).Scan(&pkgRows); err != nil {
		t.Fatalf("count the package rows: %v", err)
	}
	if pkgRows != 1 {
		t.Errorf("the soft shred removed the package row (count=%d); the package must stay soft-shredded", pkgRows)
	}

	// The hard shred removes the skill row and cascades to the package
	// row and the file rows.
	if err := dm.PermanentlyShredSkill(fx.packagedID); err != nil {
		t.Fatalf("PermanentlyShredSkill: %v", err)
	}
	if p3RowExists(t, dm, fx.packagedID) {
		t.Error("the hard shred left the skill row behind")
	}
	if err := dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM skill_packages WHERE skill_id = ?`, fx.packagedID).Scan(&pkgRows); err != nil {
		t.Fatalf("count the package rows: %v", err)
	}
	if pkgRows != 0 {
		t.Errorf("the hard shred left %d package rows behind", pkgRows)
	}
	var fileRows int
	if err := dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM skill_package_files WHERE skill_id = ?`, fx.packagedID).Scan(&fileRows); err != nil {
		t.Fatalf("count the file rows: %v", err)
	}
	if fileRows != 0 {
		t.Errorf("the hard shred left %d file rows behind", fileRows)
	}
}

// TestSkillsExclusionClauseIsACollectionFilter pins the SHAPE of the
// exclusion, so a future change cannot quietly narrow it.
//
// The clause must be a plain `collection` comparison — not a
// package-aware NOT EXISTS subquery. The blanket form is the operator's
// approved decision (O17) and it is what makes the protection apply to
// ordinary skills too.
func TestSkillsExclusionClauseIsACollectionFilter(t *testing.T) {
	if SkillsExclusionClause != `collection != 'skills'` {
		t.Errorf("SkillsExclusionClause = %q, want the blanket collection filter", SkillsExclusionClause)
	}
	if containsSubquery(SkillsExclusionClause) {
		t.Errorf("SkillsExclusionClause = %q contains a subquery; the exclusion must be a plain collection filter", SkillsExclusionClause)
	}
}

// containsSubquery reports whether an SQL fragment contains a nested
// SELECT.
func containsSubquery(fragment string) bool {
	for i := 0; i+7 <= len(fragment); i++ {
		if fragment[i:i+7] == "SELECT " {
			return true
		}
	}
	return false
}
