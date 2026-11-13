// skill_package_immutable_cli_test.go — Stage P3: the CLI surface
// propagates the package-immutability refusal (R1).
//
// `mpm save-skill --force` against a skill that owns a package must
// FAIL with the distinct `package_immutable` refusal, and it must change
// nothing.
//
// The test drives the REAL binary in a disposable workspace (`mpmRun`
// pins both MPM_WORKSPACE and HOME to fresh temp dirs), so the operator's
// live installation, its database and its services are never touched.

package main

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

// p3CLISkillDoc is the skill document the CLI saves.
const p3CLISkillDoc = `---
name: p3-packaged
version: 1.0.0
description: a packaged skill
when_to_use: when the CLI must refuse to overwrite a package
---
the original body
`

// p3CLIWorkspaceDB opens the disposable workspace's database directly.
//
// The package tables have no CLI surface yet — the importer is a later
// stage — so the fixture writes the package row through SQL, exactly as
// the importer will. The path mirrors the production layout:
// `<workspace>/src/db/mpm.db`.
func p3CLIWorkspaceDB(t *testing.T, ws string) *sql.DB {
	t.Helper()
	path := filepath.Join(ws, "src", "db", "mpm.db")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the workspace database is missing at %s: %v", path, err)
	}
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatalf("open the workspace database: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// p3CLISeedPackage gives the saved skill a package: the blob, the
// package manifest row and the file row.
func p3CLISeedPackage(t *testing.T, db *sql.DB, skillID string) {
	t.Helper()

	blobBytes := []byte("the package entrypoint bytes")
	sum := sha256.Sum256(blobBytes)
	blobSHA := hex.EncodeToString(sum[:])

	if _, err := db.Exec(
		`INSERT INTO skill_package_blobs (sha256, size_bytes, content) VALUES (?, ?, ?)`,
		blobSHA, len(blobBytes), blobBytes); err != nil {
		t.Fatalf("insert blob: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO skill_packages (skill_id, name, version, version_source, entrypoint,
			entrypoint_sha256, manifest, package_hash)
		VALUES (?, 'p3-packaged', '1.0.0', 'declared', 'SKILL.md', ?, ?, ?)
	`, skillID, blobSHA, `{"format":"mpm-skill-package/1"}`, blobSHA); err != nil {
		t.Fatalf("insert package: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO skill_package_files (skill_id, path, sha256, size_bytes, content_type,
			role, secret_scanned, poison_scanned)
		VALUES (?, 'SKILL.md', ?, ?, 'text/markdown; charset=utf-8', 'entrypoint', 1, 1)
	`, skillID, blobSHA, len(blobBytes)); err != nil {
		t.Fatalf("insert file row: %v", err)
	}
}

// TestCLISaveSkillForceIsRefusedAgainstAPackagedSkill pins the CLI
// behaviour end to end.
//
// The sequence is the operator's real one: save the skill, publish a
// package for it, then try to overwrite it through the legacy path.
func TestCLISaveSkillForceIsRefusedAgainstAPackagedSkill(t *testing.T) {
	bin := mpmCmd(t)
	ws := t.TempDir()

	dir := t.TempDir()
	mdPath := filepath.Join(dir, "SKILL.md")
	if err := os.WriteFile(mdPath, []byte(p3CLISkillDoc), 0644); err != nil {
		t.Fatalf("write the skill document: %v", err)
	}

	// 1. The ordinary save works.
	out, code := mpmRun(t, bin, ws, "save-skill", "--file", mdPath)
	if code != 0 {
		t.Fatalf("the initial save failed (code=%d). Output:\n%s", code, out)
	}

	db := p3CLIWorkspaceDB(t, ws)

	var skillID string
	if err := db.QueryRow(
		`SELECT id FROM memories WHERE collection = 'skills' AND id LIKE 'skill:p3-packaged%'`).Scan(&skillID); err != nil {
		t.Fatalf("read the saved skill id: %v", err)
	}
	if skillID != "skill:p3-packaged-v1.0.0" {
		t.Fatalf("the saved skill id is %q, want skill:p3-packaged-v1.0.0", skillID)
	}

	// 2. The skill gets a package.
	p3CLISeedPackage(t, db, skillID)

	var beforeContent string
	if err := db.QueryRow(`SELECT content FROM memories WHERE id = ?`, skillID).Scan(&beforeContent); err != nil {
		t.Fatalf("read the skill content: %v", err)
	}

	// 3. The legacy force-overwrite is refused.
	out, code = mpmRun(t, bin, ws, "save-skill", "--file", mdPath, "--force")
	if code == 0 {
		t.Fatalf("the force-overwrite of a packaged skill succeeded. Output:\n%s", out)
	}
	if !strings.Contains(out, "package_immutable") {
		t.Errorf("the refusal does not carry the package_immutable code. Output:\n%s", out)
	}

	// 4. Nothing changed — not the skill, not the package.
	var afterContent, manifest string
	if err := db.QueryRow(`SELECT content FROM memories WHERE id = ?`, skillID).Scan(&afterContent); err != nil {
		t.Fatalf("read the skill content: %v", err)
	}
	if afterContent != beforeContent {
		t.Errorf("the refused force-overwrite changed the skill content:\n  before: %q\n  after:  %q",
			beforeContent, afterContent)
	}
	if err := db.QueryRow(`SELECT manifest FROM skill_packages WHERE skill_id = ?`, skillID).Scan(&manifest); err != nil {
		t.Fatalf("read the package manifest: %v", err)
	}
	if manifest != `{"format":"mpm-skill-package/1"}` {
		t.Errorf("the refused force-overwrite changed the package manifest: %s", manifest)
	}
	var fileRows, blobRows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM skill_package_files WHERE skill_id = ?`, skillID).Scan(&fileRows); err != nil {
		t.Fatalf("count the file rows: %v", err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM skill_package_blobs`).Scan(&blobRows); err != nil {
		t.Fatalf("count the blob rows: %v", err)
	}
	if fileRows != 1 {
		t.Errorf("the file row count is %d, want 1", fileRows)
	}
	if blobRows != 1 {
		t.Errorf("the blob row count is %d, want 1", blobRows)
	}
}

// TestCLISaveSkillForceStillWorksForAnOrdinarySkill pins that the CLI's
// legitimate `force=true` behaviour survives the guard.
//
// The ordinary path is the operator's most common repair action — an
// agent re-saving a corrected skill under the same version — and it must
// keep working.
func TestCLISaveSkillForceStillWorksForAnOrdinarySkill(t *testing.T) {
	bin := mpmCmd(t)
	ws := t.TempDir()

	dir := t.TempDir()
	mdPath := filepath.Join(dir, "SKILL.md")
	if err := os.WriteFile(mdPath, []byte(p3CLISkillDoc), 0644); err != nil {
		t.Fatalf("write the skill document: %v", err)
	}

	if out, code := mpmRun(t, bin, ws, "save-skill", "--file", mdPath); code != 0 {
		t.Fatalf("the initial save failed (code=%d). Output:\n%s", code, out)
	}

	// A DIFFERENT body under the same name+version — the force-overwrite.
	updated := strings.Replace(p3CLISkillDoc, "the original body", "the updated body", 1)
	if err := os.WriteFile(mdPath, []byte(updated), 0644); err != nil {
		t.Fatalf("rewrite the skill document: %v", err)
	}

	out, code := mpmRun(t, bin, ws, "save-skill", "--file", mdPath, "--force")
	if code != 0 {
		t.Fatalf("the force-overwrite of an ordinary skill was refused (code=%d). Output:\n%s", code, out)
	}

	db := p3CLIWorkspaceDB(t, ws)
	var content string
	if err := db.QueryRow(`SELECT content FROM memories WHERE id = 'skill:p3-packaged-v1.0.0'`).Scan(&content); err != nil {
		t.Fatalf("read the skill content: %v", err)
	}
	if !strings.Contains(content, "the updated body") {
		t.Errorf("the ordinary skill was not overwritten: %q", content)
	}
}

// TestCLISkillDeleteThenForceAddResurrects pins the resurrection
// contract the CLI advertises.
//
// `mpm skill delete` prints "recoverable via mpm skill add --force", so
// that re-add must actually work. It is the same `--force` path the
// package-immutability guard sits behind, which is why it is pinned here:
// a repair that made `--force` reachable must not have broken the
// resurrection it enables.
//
// This test also pins the blast radius of the `--force` repair: an
// ordinary skill is resurrected, and a PACKAGED skill is not — its
// refusal is the whole point of R1.
func TestCLISkillDeleteThenForceAddResurrects(t *testing.T) {
	bin := mpmCmd(t)
	ws := t.TempDir()

	dir := t.TempDir()
	mdPath := filepath.Join(dir, "SKILL.md")
	if err := os.WriteFile(mdPath, []byte(p3CLISkillDoc), 0644); err != nil {
		t.Fatalf("write the skill document: %v", err)
	}

	if out, code := mpmRun(t, bin, ws, "save-skill", "--file", mdPath); code != 0 {
		t.Fatalf("the initial save failed (code=%d). Output:\n%s", code, out)
	}

	// Retire the skill.
	if out, code := mpmRun(t, bin, ws, "skill", "delete", "skill:p3-packaged-v1.0.0"); code != 0 {
		t.Fatalf("the skill delete failed (code=%d). Output:\n%s", code, out)
	}

	db := p3CLIWorkspaceDB(t, ws)
	var deletedAt sql.NullInt64
	if err := db.QueryRow(`SELECT deleted_at FROM memories WHERE id = 'skill:p3-packaged-v1.0.0'`).Scan(&deletedAt); err != nil {
		t.Fatalf("read the tombstone: %v", err)
	}
	if !deletedAt.Valid {
		t.Fatal("the skill was not tombstoned")
	}

	// The force-add resurrects it.
	if out, code := mpmRun(t, bin, ws, "skill", "add", "--file", mdPath, "--force"); code != 0 {
		t.Fatalf("the force-add did not resurrect the skill (code=%d). Output:\n%s", code, out)
	}

	if err := db.QueryRow(`SELECT deleted_at FROM memories WHERE id = 'skill:p3-packaged-v1.0.0'`).Scan(&deletedAt); err != nil {
		t.Fatalf("read the tombstone: %v", err)
	}
	if deletedAt.Valid {
		t.Error("the skill was not resurrected")
	}

	// The SAME sequence against a PACKAGED skill is refused: the
	// package stays soft-shredded until it is explicitly re-imported.
	p3CLISeedPackage(t, db, "skill:p3-packaged-v1.0.0")
	if out, code := mpmRun(t, bin, ws, "skill", "delete", "skill:p3-packaged-v1.0.0"); code != 0 {
		t.Fatalf("the skill delete failed (code=%d). Output:\n%s", code, out)
	}
	out, code := mpmRun(t, bin, ws, "skill", "add", "--file", mdPath, "--force")
	if code == 0 {
		t.Fatalf("the force-add resurrected a PACKAGED skill. Output:\n%s", out)
	}
	if !strings.Contains(out, "package_immutable") {
		t.Errorf("the refusal does not carry the package_immutable code. Output:\n%s", out)
	}
	if err := db.QueryRow(`SELECT deleted_at FROM memories WHERE id = 'skill:p3-packaged-v1.0.0'`).Scan(&deletedAt); err != nil {
		t.Fatalf("read the tombstone: %v", err)
	}
	if !deletedAt.Valid {
		t.Error("the refused force-add resurrected the packaged skill")
	}
}
