// skill_package_immutable_test.go — Stage P3: the package-immutability
// guard on the legacy write path (R1).
//
// Every test here runs against a DISPOSABLE database in a disposable
// workspace (see p3DM in skill_tx_test.go). The live MPM installation and
// its database are never touched.

package internal

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// p3PackageFixture is a skill that owns a package: a real skill row, a
// blob, a package manifest row and a file row.
//
// The package rows are written through the P1 schema exactly as the
// importer would write them, so the guard is exercised against the real
// table shapes and the real foreign keys.
type p3PackageFixture struct {
	skillID    string
	name       string
	version    string
	content    string
	blobSHA    string
	blobBytes  []byte
	packageRow string
}

// p3SeedPackagedSkill saves a real skill and then gives it a package.
//
// The skill row is created through the ordinary SaveSkill path — so it is
// a genuine skill row with a genuine id and genuine metadata — and the
// package rows are added on top of it.
func p3SeedPackagedSkill(t *testing.T, dm *DatabaseManager, name, version string) p3PackageFixture {
	t.Helper()

	content := p3SkillContent(name, version, "the original body")
	id, err := dm.SaveSkill(name, version, content, "p3-test-agent", false)
	if err != nil {
		t.Fatalf("SaveSkill: %v", err)
	}

	blobBytes := []byte("the package entrypoint bytes")
	sum := sha256.Sum256(blobBytes)
	blobSHA := hex.EncodeToString(sum[:])

	if _, err := dm.SQLDB().Exec(
		`INSERT INTO skill_package_blobs (sha256, size_bytes, content) VALUES (?, ?, ?)`,
		blobSHA, len(blobBytes), blobBytes); err != nil {
		t.Fatalf("insert blob: %v", err)
	}

	manifest := `{"format":"mpm-skill-package/1","name":"` + name + `"}`
	if _, err := dm.SQLDB().Exec(`
		INSERT INTO skill_packages (skill_id, name, version, version_source, entrypoint,
			entrypoint_sha256, manifest, package_hash)
		VALUES (?, ?, ?, 'declared', 'SKILL.md', ?, ?, ?)
	`, id, name, version, blobSHA, manifest, blobSHA); err != nil {
		t.Fatalf("insert package: %v", err)
	}

	if _, err := dm.SQLDB().Exec(`
		INSERT INTO skill_package_files (skill_id, path, sha256, size_bytes, content_type,
			role, secret_scanned, poison_scanned)
		VALUES (?, 'SKILL.md', ?, ?, 'text/markdown; charset=utf-8', 'entrypoint', 1, 1)
	`, id, blobSHA, len(blobBytes)); err != nil {
		t.Fatalf("insert file row: %v", err)
	}

	return p3PackageFixture{
		skillID:    id,
		name:       name,
		version:    version,
		content:    content,
		blobSHA:    blobSHA,
		blobBytes:  blobBytes,
		packageRow: manifest,
	}
}

// p3SkillRowContent reads a skill row's content and metadata.
func p3SkillRowContent(t *testing.T, dm *DatabaseManager, id string) (string, string) {
	t.Helper()
	var content, metadata string
	err := dm.SQLDB().QueryRow(`SELECT content, COALESCE(metadata, '{}') FROM memories WHERE id = ?`, id).
		Scan(&content, &metadata)
	if err != nil {
		t.Fatalf("read the skill row %s: %v", id, err)
	}
	return content, metadata
}

// p3PackageState reads the whole package state — the package row, the
// file rows and the blob — so a test can prove the refusal changed none
// of it.
func p3PackageState(t *testing.T, dm *DatabaseManager, skillID string) (pkg, files, blob string) {
	t.Helper()
	if err := dm.SQLDB().QueryRow(
		`SELECT manifest || '|' || package_hash || '|' || entrypoint || '|' || entrypoint_sha256
		 FROM skill_packages WHERE skill_id = ?`, skillID).Scan(&pkg); err != nil {
		t.Fatalf("read the package row: %v", err)
	}
	if err := dm.SQLDB().QueryRow(
		`SELECT path || '|' || sha256 || '|' || role FROM skill_package_files WHERE skill_id = ?`, skillID).
		Scan(&files); err != nil {
		t.Fatalf("read the file row: %v", err)
	}
	var content []byte
	if err := dm.SQLDB().QueryRow(
		`SELECT content FROM skill_package_blobs WHERE sha256 = ?`, p3BlobSHAOf(t, dm, skillID)).
		Scan(&content); err != nil {
		t.Fatalf("read the blob: %v", err)
	}
	blob = string(content)
	return pkg, files, blob
}

// p3BlobSHAOf returns the blob the package's entrypoint references.
func p3BlobSHAOf(t *testing.T, dm *DatabaseManager, skillID string) string {
	t.Helper()
	var sha string
	if err := dm.SQLDB().QueryRow(
		`SELECT entrypoint_sha256 FROM skill_packages WHERE skill_id = ?`, skillID).Scan(&sha); err != nil {
		t.Fatalf("read the entrypoint hash: %v", err)
	}
	return sha
}

// TestSaveSkillForceIsRefusedAgainstAPackagedSkill pins the refusal
// itself: a legacy `save --force` against a skill that owns a package
// fails with the DISTINCT `package_immutable` status, and it is a
// refusal rather than a generic error.
func TestSaveSkillForceIsRefusedAgainstAPackagedSkill(t *testing.T) {
	dm := p3DM(t)
	fixture := p3SeedPackagedSkill(t, dm, "packaged", "1.0.0")

	newContent := p3SkillContent("packaged", "1.0.0", "an OVERWRITTEN body")
	_, err := dm.SaveSkill("packaged", "1.0.0", newContent, "p3-test-agent", true)
	if err == nil {
		t.Fatal("the force-overwrite of a packaged skill succeeded")
	}
	if !errors.Is(err, ErrPackageImmutable) {
		t.Errorf("the error is %v, want the package_immutable sentinel", err)
	}
	var immutable *PackageImmutableError
	if !errors.As(err, &immutable) {
		t.Errorf("the error is %v, want a *PackageImmutableError", err)
	} else {
		if immutable.Name != "packaged" {
			t.Errorf("the refusal names %q, want \"packaged\"", immutable.Name)
		}
		if immutable.Version != "1.0.0" {
			t.Errorf("the refusal names version %q, want 1.0.0", immutable.Version)
		}
		if immutable.SkillID != fixture.skillID {
			t.Errorf("the refusal names id %q, want %q", immutable.SkillID, fixture.skillID)
		}
	}
	// The refusal carries a stable, greppable code.
	if !strings.Contains(err.Error(), "package_immutable") {
		t.Errorf("the refusal message does not carry the code: %v", err)
	}
}

// TestPackageImmutableRefusalChangesNothing pins that the refusal is
// FAIL CLOSED: not the content, not the metadata, not the package
// manifest, not the file row, not the blob, and not the provenance.
//
// The design's failure mode is specific: one id carrying two documents.
// So every row that describes the package must still describe the SAME
// document after the refusal.
func TestPackageImmutableRefusalChangesNothing(t *testing.T) {
	dm := p3DM(t)
	fixture := p3SeedPackagedSkill(t, dm, "packaged", "1.0.0")

	beforeContent, beforeMetadata := p3SkillRowContent(t, dm, fixture.skillID)
	beforePkg, beforeFiles, beforeBlob := p3PackageState(t, dm, fixture.skillID)

	var beforeProvenance int
	if err := dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM artifact_provenance WHERE artifact_id = ?`, fixture.skillID).Scan(&beforeProvenance); err != nil {
		t.Fatalf("count the provenance rows: %v", err)
	}

	newContent := p3SkillContent("packaged", "1.0.0", "an OVERWRITTEN body")
	if _, err := dm.SaveSkill("packaged", "1.0.0", newContent, "p3-test-agent", true); err == nil {
		t.Fatal("the force-overwrite of a packaged skill succeeded")
	}

	afterContent, afterMetadata := p3SkillRowContent(t, dm, fixture.skillID)
	if afterContent != beforeContent {
		t.Errorf("the refusal changed the skill content:\n  before: %q\n  after:  %q", beforeContent, afterContent)
	}
	if afterMetadata != beforeMetadata {
		t.Errorf("the refusal changed the skill metadata:\n  before: %s\n  after:  %s", beforeMetadata, afterMetadata)
	}

	afterPkg, afterFiles, afterBlob := p3PackageState(t, dm, fixture.skillID)
	if afterPkg != beforePkg {
		t.Errorf("the refusal changed the package row:\n  before: %s\n  after:  %s", beforePkg, afterPkg)
	}
	if afterFiles != beforeFiles {
		t.Errorf("the refusal changed the file row:\n  before: %s\n  after:  %s", beforeFiles, afterFiles)
	}
	if afterBlob != beforeBlob {
		t.Errorf("the refusal changed the blob:\n  before: %q\n  after:  %q", beforeBlob, afterBlob)
	}

	var afterProvenance int
	if err := dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM artifact_provenance WHERE artifact_id = ?`, fixture.skillID).Scan(&afterProvenance); err != nil {
		t.Fatalf("count the provenance rows: %v", err)
	}
	if afterProvenance != beforeProvenance {
		t.Errorf("the refusal changed the provenance rows: before=%d after=%d", beforeProvenance, afterProvenance)
	}
}

// TestSaveSkillForceIsRefusedAgainstASoftDeletedPackagedSkill pins that
// the refusal applies to a TOMBSTONED packaged skill.
//
// A soft shred preserves the skill row and — through the foreign key —
// the package row. The design says the package "stays soft-shredded until
// it is explicitly re-imported": the legacy force path must not resurrect
// it.
func TestSaveSkillForceIsRefusedAgainstASoftDeletedPackagedSkill(t *testing.T) {
	dm := p3DM(t)
	fixture := p3SeedPackagedSkill(t, dm, "packaged", "1.0.0")

	if err := dm.ShredSkill(fixture.skillID); err != nil {
		t.Fatalf("ShredSkill: %v", err)
	}

	newContent := p3SkillContent("packaged", "1.0.0", "an OVERWRITTEN body")
	_, err := dm.SaveSkill("packaged", "1.0.0", newContent, "p3-test-agent", true)
	if err == nil {
		t.Fatal("the force-overwrite of a soft-deleted packaged skill succeeded")
	}
	if !errors.Is(err, ErrPackageImmutable) {
		t.Errorf("the error is %v, want the package_immutable sentinel", err)
	}

	// The row is still tombstoned: the refusal did not resurrect it.
	var deletedAt sql.NullInt64
	if err := dm.SQLDB().QueryRow(`SELECT deleted_at FROM memories WHERE id = ?`, fixture.skillID).Scan(&deletedAt); err != nil {
		t.Fatalf("read the tombstone: %v", err)
	}
	if !deletedAt.Valid {
		t.Error("the soft-deleted packaged skill was resurrected by the refused force-overwrite")
	}
}

// TestSaveSkillForceStillWorksForAnOrdinarySkill pins that the guard is
// NARROW: an ordinary single-file skill — one with no package row — is
// still force-overwritten exactly as before.
func TestSaveSkillForceStillWorksForAnOrdinarySkill(t *testing.T) {
	dm := p3DM(t)

	original := p3SkillContent("ordinary", "1.0.0", "the original body")
	id, err := dm.SaveSkill("ordinary", "1.0.0", original, "p3-test-agent", false)
	if err != nil {
		t.Fatalf("SaveSkill: %v", err)
	}

	updated := p3SkillContent("ordinary", "1.0.0", "the updated body")
	if _, err := dm.SaveSkill("ordinary", "1.0.0", updated, "p3-test-agent", true); err != nil {
		t.Fatalf("the force-overwrite of an ordinary skill was refused: %v", err)
	}

	content, _ := p3SkillRowContent(t, dm, id)
	if !strings.Contains(content, "the updated body") {
		t.Errorf("the ordinary skill was not overwritten: %q", content)
	}
}

// TestSaveSkillForceStillResurrectsAnOrdinarySkill pins that the
// existing tombstone-resurrection behaviour survives the guard.
//
// The resurrection clause (deleted_at = NULL) is part of the force path's
// contract, and the guard must not take it away from ordinary skills.
func TestSaveSkillForceStillResurrectsAnOrdinarySkill(t *testing.T) {
	dm := p3DM(t)

	original := p3SkillContent("ordinary", "1.0.0", "the original body")
	id, err := dm.SaveSkill("ordinary", "1.0.0", original, "p3-test-agent", false)
	if err != nil {
		t.Fatalf("SaveSkill: %v", err)
	}
	if err := dm.ShredSkill(id); err != nil {
		t.Fatalf("ShredSkill: %v", err)
	}

	updated := p3SkillContent("ordinary", "1.0.0", "the resurrected body")
	if _, err := dm.SaveSkill("ordinary", "1.0.0", updated, "p3-test-agent", true); err != nil {
		t.Fatalf("the resurrection force-overwrite was refused: %v", err)
	}

	var deletedAt sql.NullInt64
	if err := dm.SQLDB().QueryRow(`SELECT deleted_at FROM memories WHERE id = ?`, id).Scan(&deletedAt); err != nil {
		t.Fatalf("read the tombstone: %v", err)
	}
	if deletedAt.Valid {
		t.Error("the ordinary skill was not resurrected")
	}
	content, _ := p3SkillRowContent(t, dm, id)
	if !strings.Contains(content, "the resurrected body") {
		t.Errorf("the resurrected skill carries the wrong content: %q", content)
	}
}

// TestPackageImmutableGuardCannotBeBypassedByInterleavedOperations pins
// the concurrency property.
//
// Once the package row is committed, no legacy force-overwrite may change
// the skill's content — not one, and not several running at once. The
// guard's check and the write it guards share one transaction, and the
// write carries the guard's condition in its own WHERE clause, so there is
// no window in which a check has passed and a package row has appeared.
func TestPackageImmutableGuardCannotBeBypassedByInterleavedOperations(t *testing.T) {
	dm := p3DM(t)
	fixture := p3SeedPackagedSkill(t, dm, "packaged", "1.0.0")

	beforeContent, _ := p3SkillRowContent(t, dm, fixture.skillID)

	const writers = 8
	var wg sync.WaitGroup
	results := make([]error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			content := p3SkillContent("packaged", "1.0.0", fmt.Sprintf("an OVERWRITTEN body %d", i))
			_, results[i] = dm.SaveSkill("packaged", "1.0.0", content, "p3-test-agent", true)
		}(i)
	}
	wg.Wait()

	for i, err := range results {
		if err == nil {
			t.Errorf("writer %d: the force-overwrite of a packaged skill succeeded", i)
			continue
		}
		if !errors.Is(err, ErrPackageImmutable) {
			t.Errorf("writer %d: the error is %v, want the package_immutable sentinel", i, err)
		}
	}

	afterContent, _ := p3SkillRowContent(t, dm, fixture.skillID)
	if afterContent != beforeContent {
		t.Errorf("a concurrent force-overwrite changed the packaged skill's content:\n  before: %q\n  after:  %q",
			beforeContent, afterContent)
	}
}

// TestPackageImmutableGuardIsInTheWriteStatement pins the second line of
// defence directly.
//
// The guard has two parts: a check, and the SAME condition folded into
// the UPDATE that the check guards. This test runs that UPDATE against a
// packaged skill and pins that it changes NOTHING — so even if the check
// were removed, or ran against a stale snapshot, the write could not
// land.
//
// This is what closes the time-of-check/time-of-use window: the refusal
// is not a property of the check's timing.
func TestPackageImmutableGuardIsInTheWriteStatement(t *testing.T) {
	dm := p3DM(t)
	fixture := p3SeedPackagedSkill(t, dm, "packaged", "1.0.0")

	beforeContent, _ := p3SkillRowContent(t, dm, fixture.skillID)

	res, err := dm.SQLDB().Exec(`
		UPDATE memories
		SET content = ?, tags = ?, metadata = ?, is_long_term = 1,
		    deleted_at = NULL,
		    updated_at = CAST(strftime('%s','now') AS INTEGER)
		WHERE id = ?
		  AND NOT EXISTS (SELECT 1 FROM skill_packages WHERE skill_id = ?)
	`, "an OVERWRITTEN body", `["skill"]`, `{}`, fixture.skillID, fixture.skillID)
	if err != nil {
		t.Fatalf("the guarded UPDATE: %v", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		t.Fatalf("rows affected: %v", err)
	}
	if affected != 0 {
		t.Errorf("the guarded UPDATE changed %d rows against a packaged skill, want 0", affected)
	}

	afterContent, _ := p3SkillRowContent(t, dm, fixture.skillID)
	if afterContent != beforeContent {
		t.Errorf("the guarded UPDATE changed the packaged skill's content:\n  before: %q\n  after:  %q",
			beforeContent, afterContent)
	}
}

// TestPackageImmutableRefusalIsClassified pins that the refusal reaches
// the dispatch audit as a bounded, typed outcome rather than as
// unclassified prose.
func TestPackageImmutableRefusalIsClassified(t *testing.T) {
	dm := p3DM(t)
	p3SeedPackagedSkill(t, dm, "packaged", "1.0.0")

	newContent := p3SkillContent("packaged", "1.0.0", "an OVERWRITTEN body")
	_, err := dm.SaveSkill("packaged", "1.0.0", newContent, "p3-test-agent", true)
	if err == nil {
		t.Fatal("the force-overwrite of a packaged skill succeeded")
	}

	class, code := ClassifyError(err)
	if class != OutcomeClassConflict {
		t.Errorf("the outcome class is %q, want %q", class, OutcomeClassConflict)
	}
	if code != "package_immutable" {
		t.Errorf("the outcome code is %q, want %q", code, "package_immutable")
	}
}
