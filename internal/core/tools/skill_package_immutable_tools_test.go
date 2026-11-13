// skill_package_immutable_tools_test.go — Stage P3: the MCP surface
// propagates the package-immutability refusal (R1).
//
// The MCP `mpm_skills save` action is a thin shim over
// `dm.SaveSkillWithInvocation`. The refusal must reach the agent as a
// DISTINCT, machine-readable refusal — not as a generic write error and
// not as a success payload that silently changed nothing.
//
// **Every test here owns its workspace.** The database is in-memory, but
// that is not isolation: workspace-scoped FILES — `toxicphrases.txt`,
// `active.json` and its cross-process lock — are written through
// `config.GetMPMDir()`, which resolves the AMBIENT `MPM_WORKSPACE`. A
// test that merely inherits a safe `MPM_WORKSPACE` from its parent
// environment can therefore write into whatever that environment points
// at, and the in-memory database keeps only the ROWS out of harm's way.
//
// So the workspace is pinned by the test itself, before the manager is
// constructed, and it is pinned unconditionally: the test owns its
// workspace whether or not the parent environment already defines
// `MPM_WORKSPACE`.

package tools

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// p3ToolsDM opens a hermetic in-memory database manager whose WORKSPACE
// the test owns.
//
// The pins come FIRST and they are unconditional:
//
//   - HOME is pinned to a disposable directory, so the production
//     workspace for this test — `$HOME/.mpm` — is a directory the test
//     owns too, and any leak into it is detectable rather than silent;
//   - MPM_WORKSPACE is pinned to a DIFFERENT disposable directory, so
//     the test's own workspace is unambiguously not the production one.
//
// `t.Setenv` restores the previous value when the test ends, so the pin
// does not leak into the next test.
func p3ToolsDM(t *testing.T) *mpminternal.DatabaseManager {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("MPM_WORKSPACE", t.TempDir())
	return mpminternal.NewTestDM(t)
}

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
	dm := p3ToolsDM(t)
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
	dm := p3ToolsDM(t)
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
	dm := p3ToolsDM(t)

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

// TestMCPSaveSkillIsIsolatedFromAHostileAmbientWorkspace pins the
// isolation property the MCP tests exist to have: the test owns its
// workspace, so an inherited environment variable cannot aim it at the
// operator's production workspace.
//
// The setup is deliberately hostile. The ambient `MPM_WORKSPACE` points
// at a production-shaped directory — `$HOME/.mpm` for a disposable fake
// HOME — which is exactly the shape that must never be written. The test
// then runs the ordinary MCP save flow.
//
// Two things are asserted:
//
//   - the flow SUCCEEDS, because the test pinned its own workspace
//     before constructing the manager; and
//   - the fake production workspace is still EMPTY, because nothing the
//     flow wrote went there.
//
// The in-memory database is not the isolation: workspace-scoped files
// (`toxicphrases.txt`, `active.json`) are written through
// `config.GetMPMDir()`, which resolves the ambient workspace. This test
// is the regression for that.
func TestMCPSaveSkillIsIsolatedFromAHostileAmbientWorkspace(t *testing.T) {
	// A disposable fake HOME, and a production-shaped workspace inside
	// it. Nothing here is the operator's real installation.
	fakeHome := t.TempDir()
	hostile := filepath.Join(fakeHome, ".mpm")
	if err := os.MkdirAll(hostile, 0o700); err != nil {
		t.Fatalf("create the fake production workspace: %v", err)
	}
	t.Setenv("HOME", fakeHome)
	t.Setenv("MPM_WORKSPACE", hostile)

	// p3ToolsDM overrides both pins with disposable directories the
	// test owns.
	dm := p3ToolsDM(t)

	if _, err := handleSaveSkill(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"name":    "isolated",
		"version": "1.0.0",
		"content": p3ToolsSkillContent("isolated", "1.0.0", "body"),
	}); err != nil {
		t.Fatalf("the MCP save failed under a hostile ambient workspace: %v", err)
	}

	// The fake production workspace must be untouched.
	entries, err := os.ReadDir(hostile)
	if err != nil {
		t.Fatalf("read the fake production workspace: %v", err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("the MCP flow wrote %d entries into the fake production workspace %s: %v",
			len(entries), hostile, names)
	}
}

// TestMCPWorkspacePinIsRestoredAfterTheTest pins that the ownership pin
// does not leak: the ambient MPM_WORKSPACE a test inherited must be back
// in place when the test ends.
//
// `t.Setenv` is the mechanism — it restores the previous value at test
// end — and this test pins that the helper uses it rather than
// `os.Setenv`, which would leak for the rest of the process.
func TestMCPWorkspacePinIsRestoredAfterTheTest(t *testing.T) {
	original := t.TempDir()
	t.Setenv("MPM_WORKSPACE", original)

	t.Run("inner", func(t *testing.T) {
		dm := p3ToolsDM(t)
		if _, err := handleSaveSkill(dm, mpminternal.ActiveContext{}, map[string]interface{}{
			"name":    "restored",
			"version": "1.0.0",
			"content": p3ToolsSkillContent("restored", "1.0.0", "body"),
		}); err != nil {
			t.Fatalf("the MCP save failed: %v", err)
		}
		// Inside the subtest the pin is the subtest's own workspace.
		if got := os.Getenv("MPM_WORKSPACE"); got == original {
			t.Errorf("the subtest did not take ownership of the workspace; it is still %q", got)
		}
	})

	// After the subtest, the outer value is back.
	if got := os.Getenv("MPM_WORKSPACE"); got != original {
		t.Errorf("the workspace pin leaked: MPM_WORKSPACE = %q, want %q", got, original)
	}
}
