// skill_package_read_tools_test.go — Stage P6: the MCP `read` action
// carries the package provenance (R6 / T40).
//
// Design of record: PR #222 §8.2.1 and §13 T40.
//
// The `read` action is the ONE place where package content is inlined
// without the caller asking for a file by name: it returns the entrypoint
// projection's body straight into the agent's context. So the response for
// a packaged skill must carry the provenance of the bytes it carries —
// the ORIGINAL entrypoint's hash and the entrypoint's ACTUAL scan status.
//
// The two halves are pinned separately:
//
//   - a PACKAGED skill's response ALWAYS carries the `package` object;
//   - an ORDINARY skill's response is byte-identical to today's: the
//     object is ABSENT, and every other field keeps its value and its
//     structure.
//
// Every test here owns its workspace: `p3ToolsDM` pins HOME and
// MPM_WORKSPACE to disposable directories before the manager is built.

package tools

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// p6ToolsPackageTree writes a package tree into a fresh temporary
// directory and returns its path.
func p6ToolsPackageTree(t *testing.T, files map[string][]byte) string {
	t.Helper()
	root := t.TempDir()
	for rel, content := range files {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", full, err)
		}
		if err := os.WriteFile(full, content, 0o600); err != nil {
			t.Fatalf("write %s: %v", full, err)
		}
	}
	return root
}

// p6ToolsImportPackage imports a package and returns the import result.
func p6ToolsImportPackage(t *testing.T, dm *mpminternal.DatabaseManager, name, version string, files map[string][]byte) *mpminternal.PackageImportResult {
	t.Helper()
	tree := map[string][]byte{}
	for k, v := range files {
		tree[k] = v
	}
	frontmatter := "---\nname: " + name + "\ndescription: a test skill\nwhen_to_use: when a test needs this exact skill\n"
	if version != "" {
		frontmatter += "version: " + version + "\n"
	}
	tree["SKILL.md"] = []byte(frontmatter + "---\nthe original entrypoint body\n")
	result, err := dm.ImportSkillPackageFromDir(mpminternal.PackageImportOptions{
		Path: p6ToolsPackageTree(t, tree),
	})
	if err != nil {
		t.Fatalf("import the package: %v", err)
	}
	return result
}

// p6ToolsResponseKeys returns a response map's keys, sorted.
func p6ToolsResponseKeys(response map[string]interface{}) []string {
	keys := make([]string, 0, len(response))
	for k := range response {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// TestMCPReadSkillCarriesThePackageProvenance pins T40.
//
// The packaged skill's read response ALWAYS carries the `package` object,
// and the object carries the ORIGINAL entrypoint's hash, the package hash,
// the file count and the entrypoint's ACTUAL scan status.
func TestMCPReadSkillCarriesThePackageProvenance(t *testing.T) {
	dm := p3ToolsDM(t)
	imported := p6ToolsImportPackage(t, dm, "packaged", "1.0.0", map[string][]byte{
		"references/templates.md": []byte("the reference body\n"),
	})

	raw, err := handleReadSkill(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"skill_id": imported.SkillID,
	})
	if err != nil {
		t.Fatalf("handleReadSkill: %v", err)
	}
	response, ok := raw.(map[string]interface{})
	if !ok {
		t.Fatalf("the read returned %T, want a map", raw)
	}

	// The provenance object is PRESENT.
	carried, ok := response["package"]
	if !ok {
		t.Fatalf("the read of a packaged skill carries no package object; keys=%v",
			p6ToolsResponseKeys(response))
	}
	provenance, ok := carried.(*mpminternal.SkillPackageProvenance)
	if !ok {
		t.Fatalf("the package object is %T, want a provenance", carried)
	}

	if provenance.Entrypoint != "SKILL.md" {
		t.Errorf("the provenance's entrypoint is %q, want SKILL.md", provenance.Entrypoint)
	}
	if provenance.EntrypointSHA256 != imported.EntrypointSHA256 {
		t.Errorf("the provenance's entrypoint hash is %s, want %s",
			provenance.EntrypointSHA256, imported.EntrypointSHA256)
	}
	if provenance.PackageHash != imported.PackageHash {
		t.Errorf("the provenance's package hash is %s, want %s",
			provenance.PackageHash, imported.PackageHash)
	}
	if provenance.FileCount != 2 {
		t.Errorf("the provenance's file count is %d, want 2", provenance.FileCount)
	}
	if !provenance.Scanned {
		t.Errorf("the provenance's scan status is %+v, want the entrypoint to be scanned",
			provenance.ScanStatus)
	}
	if !provenance.ScanStatus.Secret || !provenance.ScanStatus.Poison {
		t.Errorf("the provenance's scan status is %+v, want both scanners to have run",
			provenance.ScanStatus)
	}
	if provenance.VersionOverride == nil {
		t.Errorf("the provenance carries no version override")
	} else if provenance.VersionOverride.Effective != "1.0.0" {
		t.Errorf("the provenance's version override is %+v", provenance.VersionOverride)
	}

	// Every existing field keeps its value.
	if response["success"] != true {
		t.Errorf("the response's success is %v, want true", response["success"])
	}
	if response["id"] != imported.SkillID {
		t.Errorf("the response's id is %v, want %s", response["id"], imported.SkillID)
	}
	if response["name"] != "packaged" {
		t.Errorf("the response's name is %v, want packaged", response["name"])
	}
	if response["version"] != "1.0.0" {
		t.Errorf("the response's version is %v, want 1.0.0", response["version"])
	}
	body, ok := response["body"].(string)
	if !ok || body == "" {
		t.Errorf("the response's body is %v, want the entrypoint projection's body", response["body"])
	}

	// The body is the entrypoint PROJECTION's body — and no supporting
	// file is inlined beside it.
	for _, forbidden := range []string{"the reference body", "references/templates.md"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("the read inlined %q beside the body", forbidden)
		}
	}
}

// TestMCPReadSkillOmitsThePackageObjectForAnOrdinarySkill pins the other
// half of T40: an ordinary skill's response is byte-identical to today's.
//
// The object is ABSENT — not present and empty — and the key set is
// exactly the one the action had before the package object was added.
func TestMCPReadSkillOmitsThePackageObjectForAnOrdinarySkill(t *testing.T) {
	dm := p3ToolsDM(t)
	id, err := dm.SaveSkill("ordinary", "1.0.0", p3ToolsSkillContent("ordinary", "1.0.0", "an ordinary body"),
		"p6-test-agent", false)
	if err != nil {
		t.Fatalf("SaveSkill: %v", err)
	}

	raw, err := handleReadSkill(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"skill_id": id,
	})
	if err != nil {
		t.Fatalf("handleReadSkill: %v", err)
	}
	response, ok := raw.(map[string]interface{})
	if !ok {
		t.Fatalf("the read returned %T, want a map", raw)
	}

	if carried, ok := response["package"]; ok {
		t.Errorf("the read of an ordinary skill carries a package object: %v", carried)
	}

	want := []string{
		"body", "constraints", "domain", "id", "is_global", "name",
		"steps", "success", "version", "when_to_use",
	}
	got := p6ToolsResponseKeys(response)
	if len(got) != len(want) {
		t.Fatalf("the response's keys are %v, want exactly %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("the response's key %d is %q, want %q", i, got[i], want[i])
		}
	}
}

// TestMCPReadSkillProvenanceNamesTheOriginalEntrypoint pins that the
// provenance's entrypoint hash describes the ORIGINAL bytes and not the
// derived projection.
//
// The distinction is only VISIBLE for a package that declares no version:
// §8.2 says that a package which DOES declare a version has a projection
// byte-identical to the original file, so the two hashes coincide there.
func TestMCPReadSkillProvenanceNamesTheOriginalEntrypoint(t *testing.T) {
	dm := p3ToolsDM(t)
	imported := p6ToolsImportPackage(t, dm, "versionless", "", nil)

	if imported.EntrypointSHA256 == imported.ProjectionSHA256 {
		t.Fatalf("the fixture did not produce a distinct projection")
	}

	raw, err := handleReadSkill(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"skill_id": imported.SkillID,
	})
	if err != nil {
		t.Fatalf("handleReadSkill: %v", err)
	}
	response := raw.(map[string]interface{})
	provenance, ok := response["package"].(*mpminternal.SkillPackageProvenance)
	if !ok {
		t.Fatalf("the read carries no provenance: %v", response["package"])
	}
	if provenance.EntrypointSHA256 != imported.EntrypointSHA256 {
		t.Errorf("the provenance's entrypoint hash is %s, want the ORIGINAL's %s",
			provenance.EntrypointSHA256, imported.EntrypointSHA256)
	}
	if provenance.EntrypointSHA256 == imported.ProjectionSHA256 {
		t.Errorf("the provenance's entrypoint hash describes the PROJECTION, not the original bytes")
	}
	// The version override is reported, because the package declares no
	// version and MPM assigned one.
	if provenance.VersionOverride == nil {
		t.Fatalf("the provenance carries no version override")
	}
	if provenance.VersionOverride.Reason != mpminternal.PackageVersionSourceUnversioned {
		t.Errorf("the provenance's version override reason is %q, want %q",
			provenance.VersionOverride.Reason, mpminternal.PackageVersionSourceUnversioned)
	}
	if provenance.VersionOverride.Effective != "1.0.0-unversioned" {
		t.Errorf("the provenance's effective version is %q, want 1.0.0-unversioned",
			provenance.VersionOverride.Effective)
	}
}

// TestMCPReadSkillReadsAPackageByName pins that the read action's own
// selector still resolves a packaged skill, and that the provenance
// follows it.
func TestMCPReadSkillReadsAPackageByName(t *testing.T) {
	dm := p3ToolsDM(t)
	imported := p6ToolsImportPackage(t, dm, "byname", "1.0.0", nil)

	raw, err := handleReadSkill(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"name":    "byname",
		"version": "1.0.0",
	})
	if err != nil {
		t.Fatalf("handleReadSkill: %v", err)
	}
	response := raw.(map[string]interface{})
	provenance, ok := response["package"].(*mpminternal.SkillPackageProvenance)
	if !ok {
		t.Fatalf("the read by name carries no provenance: %v", response["package"])
	}
	if provenance.EntrypointSHA256 != imported.EntrypointSHA256 {
		t.Errorf("the provenance's entrypoint hash is %s, want %s",
			provenance.EntrypointSHA256, imported.EntrypointSHA256)
	}
}
