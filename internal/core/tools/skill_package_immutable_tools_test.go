// skill_package_immutable_tools_test.go — Stage P3: the MCP surface
// propagates the package-immutability refusal (R1).
//
// The MCP `mpm_skills save` action is a thin shim over
// `dm.SaveSkillWithInvocation`. The refusal must reach the agent as a
// DISTINCT, machine-readable refusal — not as a generic write error and
// not as a success payload that silently changed nothing.
//
// The test runs against an in-memory database built by
// `mpminternal.NewTestDM`, which pins an isolated workspace: the live
// MPM installation and its database are never touched.

package tools

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// p3ToolsSkillContent builds a minimal valid skill document.
func p3ToolsSkillContent(name, version, body string) string {
	return fmt.Sprintf("---\nname: %s\nversion: %s\ndescription: a test skill\nwhen_to_use: when a test needs this exact skill\n---\n%s",
		name, version, body)
}

// p3ToolsSeedPackagedSkill saves a skill and then gives it a package:
// the skill row, the blob, the package manifest row and the file row.
func p3ToolsSeedPackagedSkill(t *testing.T, dm *mpminternal.DatabaseManager, name, version string) string {
	t.Helper()

	content := p3ToolsSkillContent(name, version, "the original body")
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
	if _, err := dm.SQLDB().Exec(`
		INSERT INTO skill_packages (skill_id, name, version, version_source, entrypoint,
			entrypoint_sha256, manifest, package_hash)
		VALUES (?, ?, ?, 'declared', 'SKILL.md', ?, ?, ?)
	`, id, name, version, blobSHA, `{"format":"mpm-skill-package/1"}`, blobSHA); err != nil {
		t.Fatalf("insert package: %v", err)
	}
	if _, err := dm.SQLDB().Exec(`
		INSERT INTO skill_package_files (skill_id, path, sha256, size_bytes, content_type,
			role, secret_scanned, poison_scanned)
		VALUES (?, 'SKILL.md', ?, ?, 'text/markdown; charset=utf-8', 'entrypoint', 1, 1)
	`, id, blobSHA, len(blobBytes)); err != nil {
		t.Fatalf("insert file row: %v", err)
	}
	return id
}

// TestMCPSaveSkillRefusesToOverwriteAPackagedSkill pins that the MCP
// save action surfaces the refusal.
//
// The three properties that matter to an agent are pinned separately:
//
//   - the call FAILS (it does not return a success envelope);
//   - the error carries the stable `package_immutable` code, so an agent
//     can branch on it without parsing prose;
//   - the error classifies as a CONFLICT with the bounded code
//     `package_immutable`, so the dispatch audit records a policy
//     enforcement rather than a substrate fault.
func TestMCPSaveSkillRefusesToOverwriteAPackagedSkill(t *testing.T) {
	dm := mpminternal.NewTestDM(t)
	p3ToolsSeedPackagedSkill(t, dm, "packaged", "1.0.0")

	result, err := handleSaveSkill(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"name":    "packaged",
		"version": "1.0.0",
		"content": p3ToolsSkillContent("packaged", "1.0.0", "an OVERWRITTEN body"),
		"force":   true,
	})
	if err == nil {
		t.Fatalf("the MCP save succeeded against a packaged skill; result=%v", result)
	}
	if result != nil {
		t.Errorf("the refused call returned a result payload: %v", result)
	}
	if !errors.Is(err, mpminternal.ErrPackageImmutable) {
		t.Errorf("the error is %v, want the package_immutable sentinel", err)
	}
	if !strings.Contains(err.Error(), "package_immutable") {
		t.Errorf("the refusal message does not carry the code: %v", err)
	}

	class, code := mpminternal.ClassifyError(err)
	if class != mpminternal.OutcomeClassConflict {
		t.Errorf("the outcome class is %q, want %q", class, mpminternal.OutcomeClassConflict)
	}
	if code != "package_immutable" {
		t.Errorf("the outcome code is %q, want %q", code, "package_immutable")
	}
}

// TestMCPSaveSkillRefusalChangesNothing pins that the MCP refusal is
// FAIL CLOSED at the agent boundary too: the skill's content, the
// package manifest, the file row and the blob are all unchanged.
func TestMCPSaveSkillRefusalChangesNothing(t *testing.T) {
	dm := mpminternal.NewTestDM(t)
	id := p3ToolsSeedPackagedSkill(t, dm, "packaged", "1.0.0")

	readState := func() (string, string, string, string) {
		t.Helper()
		var content, manifest, files string
		if err := dm.SQLDB().QueryRow(`SELECT content FROM memories WHERE id = ?`, id).Scan(&content); err != nil {
			t.Fatalf("read the skill row: %v", err)
		}
		if err := dm.SQLDB().QueryRow(`SELECT manifest FROM skill_packages WHERE skill_id = ?`, id).Scan(&manifest); err != nil {
			t.Fatalf("read the package row: %v", err)
		}
		if err := dm.SQLDB().QueryRow(`SELECT path FROM skill_package_files WHERE skill_id = ?`, id).Scan(&files); err != nil {
			t.Fatalf("read the file row: %v", err)
		}
		var raw []byte
		if err := dm.SQLDB().QueryRow(
			`SELECT content FROM skill_package_blobs WHERE sha256 = (SELECT entrypoint_sha256 FROM skill_packages WHERE skill_id = ?)`,
			id).Scan(&raw); err != nil {
			t.Fatalf("read the blob: %v", err)
		}
		return content, manifest, files, string(raw)
	}

	beforeContent, beforeManifest, beforeFiles, beforeBlob := readState()

	if _, err := handleSaveSkill(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"name":    "packaged",
		"version": "1.0.0",
		"content": p3ToolsSkillContent("packaged", "1.0.0", "an OVERWRITTEN body"),
		"force":   true,
	}); err == nil {
		t.Fatal("the MCP save succeeded against a packaged skill")
	}

	afterContent, afterManifest, afterFiles, afterBlob := readState()
	if afterContent != beforeContent {
		t.Errorf("the refusal changed the skill content:\n  before: %q\n  after:  %q", beforeContent, afterContent)
	}
	if afterManifest != beforeManifest {
		t.Errorf("the refusal changed the package manifest:\n  before: %s\n  after:  %s", beforeManifest, afterManifest)
	}
	if afterFiles != beforeFiles {
		t.Errorf("the refusal changed the file row:\n  before: %s\n  after:  %s", beforeFiles, afterFiles)
	}
	if afterBlob != beforeBlob {
		t.Errorf("the refusal changed the blob:\n  before: %q\n  after:  %q", beforeBlob, afterBlob)
	}
}

// TestMCPSaveSkillStillOverwritesAnOrdinarySkill pins that the MCP
// surface's legitimate `force=true` behaviour is untouched: an ordinary
// skill — one with no package row — is still overwritten in place.
func TestMCPSaveSkillStillOverwritesAnOrdinarySkill(t *testing.T) {
	dm := mpminternal.NewTestDM(t)

	result, err := handleSaveSkill(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"name":    "ordinary",
		"version": "1.0.0",
		"content": p3ToolsSkillContent("ordinary", "1.0.0", "the original body"),
	})
	if err != nil {
		t.Fatalf("the initial MCP save failed: %v", err)
	}
	if result == nil {
		t.Fatal("the initial MCP save returned no payload")
	}

	if _, err := handleSaveSkill(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"name":    "ordinary",
		"version": "1.0.0",
		"content": p3ToolsSkillContent("ordinary", "1.0.0", "the updated body"),
		"force":   true,
	}); err != nil {
		t.Fatalf("the force-overwrite of an ordinary skill was refused: %v", err)
	}

	var content string
	if err := dm.SQLDB().QueryRow(`SELECT content FROM memories WHERE id = 'skill:ordinary-v1.0.0'`).Scan(&content); err != nil {
		t.Fatalf("read the skill row: %v", err)
	}
	if !strings.Contains(content, "the updated body") {
		t.Errorf("the ordinary skill was not overwritten: %q", content)
	}
}
