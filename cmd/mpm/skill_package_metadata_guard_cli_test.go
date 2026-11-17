// skill_package_metadata_guard_cli_test.go — the CLI and MCP surfaces
// propagate the package's metadata refusal.
//
// The metadata guard is only worth having if it is visible where the
// mutation is actually attempted: `mpm patch-memory` (the operator's
// repair tool) and `mpm call mpm_memory --payload '{"action":"patch"}'`
// (the agent's path). Both must carry the bounded code, and neither may
// change anything.
//
// The test drives the REAL binary in a disposable workspace (`mpmRun` pins
// both MPM_WORKSPACE and HOME to fresh temp dirs), so the operator's live
// installation, its database and its services are never touched.

package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// p3CLIMetadataSkillDoc is the skill document the CLI saves.
//
// The name is distinct from the force-path fixture's so the two suites can
// share a database shape without colliding on the deterministic skill id.
const p3CLIMetadataSkillDoc = `---
name: p3-metadata
version: 1.0.0
description: a packaged skill whose metadata is guarded
when_to_use: when the CLI must refuse to rewrite the package's metadata
---
the original body
`

// p3CLIMetadataSeedPackage gives the saved skill a package: the blob, the
// package manifest row and the file row.
func p3CLIMetadataSeedPackage(t *testing.T, db *sql.DB, skillID string) {
	t.Helper()
	p3CLISeedPackageNamed(t, db, skillID, "p3-metadata")
}

// TestCLIPatchMemoryIsRefusedWhenItWouldChangeThePackageVersion pins the
// CLI surface end to end.
//
// The sequence is the operator's real one: save the skill, publish a
// package for it, then try to rewrite the package's version through the
// legacy metadata patch.
func TestCLIPatchMemoryIsRefusedWhenItWouldChangeThePackageVersion(t *testing.T) {
	bin := mpmCmd(t)
	ws := t.TempDir()

	dir := t.TempDir()
	mdPath := filepath.Join(dir, "SKILL.md")
	if err := os.WriteFile(mdPath, []byte(p3CLIMetadataSkillDoc), 0644); err != nil {
		t.Fatalf("write the skill document: %v", err)
	}

	if out, code := mpmRun(t, bin, ws, "save-skill", "--file", mdPath); code != 0 {
		t.Fatalf("the initial save failed (code=%d). Output:\n%s", code, out)
	}

	db := p3CLIWorkspaceDB(t, ws)
	var skillID string
	if err := db.QueryRow(
		`SELECT id FROM memories WHERE collection = 'skills' AND id LIKE 'skill:p3-metadata%'`).Scan(&skillID); err != nil {
		t.Fatalf("read the saved skill id: %v", err)
	}
	p3CLIMetadataSeedPackage(t, db, skillID)

	var beforeMetadata string
	if err := db.QueryRow(`SELECT metadata FROM memories WHERE id = ?`, skillID).Scan(&beforeMetadata); err != nil {
		t.Fatalf("read the skill metadata: %v", err)
	}

	// The legacy metadata patch tries to rewrite the package's version.
	out, code := mpmRun(t, bin, ws, "patch-memory", skillID, `{"version":"9.9.9"}`)
	if code == 0 {
		t.Fatalf("the metadata patch of a packaged skill succeeded. Output:\n%s", out)
	}
	if !strings.Contains(out, "package_metadata_immutable") {
		t.Errorf("the refusal does not carry the package_metadata_immutable code. Output:\n%s", out)
	}

	var afterMetadata string
	if err := db.QueryRow(`SELECT metadata FROM memories WHERE id = ?`, skillID).Scan(&afterMetadata); err != nil {
		t.Fatalf("read the skill metadata: %v", err)
	}
	if afterMetadata != beforeMetadata {
		t.Errorf("the refused patch changed the skill metadata:\n  before: %s\n  after:  %s",
			beforeMetadata, afterMetadata)
	}
}

// TestCLIPatchMemoryOfUnrelatedMetadataIsPermittedOnAPackagedSkill pins
// that the CLI's legitimate repair tool still works on a packaged skill.
//
// The guard is narrow: a patch that touches any other key is exactly as
// patchable as it always was.
func TestCLIPatchMemoryOfUnrelatedMetadataIsPermittedOnAPackagedSkill(t *testing.T) {
	bin := mpmCmd(t)
	ws := t.TempDir()

	dir := t.TempDir()
	mdPath := filepath.Join(dir, "SKILL.md")
	if err := os.WriteFile(mdPath, []byte(p3CLIMetadataSkillDoc), 0644); err != nil {
		t.Fatalf("write the skill document: %v", err)
	}

	if out, code := mpmRun(t, bin, ws, "save-skill", "--file", mdPath); code != 0 {
		t.Fatalf("the initial save failed (code=%d). Output:\n%s", code, out)
	}

	db := p3CLIWorkspaceDB(t, ws)
	var skillID string
	if err := db.QueryRow(
		`SELECT id FROM memories WHERE collection = 'skills' AND id LIKE 'skill:p3-metadata%'`).Scan(&skillID); err != nil {
		t.Fatalf("read the saved skill id: %v", err)
	}
	p3CLIMetadataSeedPackage(t, db, skillID)

	out, code := mpmRun(t, bin, ws, "patch-memory", skillID, `{"status":"reviewed"}`)
	if code != 0 {
		t.Fatalf("a metadata patch of an unrelated key was refused (code=%d). Output:\n%s", code, out)
	}

	var status string
	if err := db.QueryRow(
		`SELECT COALESCE(json_extract(metadata, '$.status'), '') FROM memories WHERE id = ?`, skillID).Scan(&status); err != nil {
		t.Fatalf("read the patched metadata: %v", err)
	}
	if status != "reviewed" {
		t.Errorf("the unrelated metadata did not land: %q", status)
	}
}

// TestMCPMemoryPatchIsRefusedWhenItWouldChangeThePackageVersion pins the
// AGENT's path.
//
// `mpm_memory`'s `patch` action reaches the same legacy patch primitive
// the CLI does, so the refusal must reach it too — with its bounded code,
// so the agent sees a typed refusal rather than a generic failure.
func TestMCPMemoryPatchIsRefusedWhenItWouldChangeThePackageVersion(t *testing.T) {
	bin := mpmCmd(t)
	ws := t.TempDir()

	dir := t.TempDir()
	mdPath := filepath.Join(dir, "SKILL.md")
	if err := os.WriteFile(mdPath, []byte(p3CLIMetadataSkillDoc), 0644); err != nil {
		t.Fatalf("write the skill document: %v", err)
	}

	if out, code := mpmRun(t, bin, ws, "save-skill", "--file", mdPath); code != 0 {
		t.Fatalf("the initial save failed (code=%d). Output:\n%s", code, out)
	}

	db := p3CLIWorkspaceDB(t, ws)
	var skillID string
	if err := db.QueryRow(
		`SELECT id FROM memories WHERE collection = 'skills' AND id LIKE 'skill:p3-metadata%'`).Scan(&skillID); err != nil {
		t.Fatalf("read the saved skill id: %v", err)
	}
	p3CLIMetadataSeedPackage(t, db, skillID)

	var beforeMetadata string
	if err := db.QueryRow(`SELECT metadata FROM memories WHERE id = ?`, skillID).Scan(&beforeMetadata); err != nil {
		t.Fatalf("read the skill metadata: %v", err)
	}

	out, code := mpmRun(t, bin, ws, "call", "mpm_memory",
		"--payload", `{"action":"patch","params":{"memory_id":"`+skillID+`","patch":{"version":"9.9.9"}}}`)
	if code == 0 {
		t.Fatalf("the agent's metadata patch of a packaged skill succeeded. Output:\n%s", out)
	}
	if !strings.Contains(out, "package_metadata_immutable") {
		t.Errorf("the refusal does not carry the package_metadata_immutable code. Output:\n%s", out)
	}

	var afterMetadata string
	if err := db.QueryRow(`SELECT metadata FROM memories WHERE id = ?`, skillID).Scan(&afterMetadata); err != nil {
		t.Fatalf("read the skill metadata: %v", err)
	}
	if afterMetadata != beforeMetadata {
		t.Errorf("the refused patch changed the skill metadata:\n  before: %s\n  after:  %s",
			beforeMetadata, afterMetadata)
	}
}
