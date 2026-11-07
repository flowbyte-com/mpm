package update

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------

// repoRoot finds the repository root from the test's working
// directory (internal/update → the module root).
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(payloadManifestRelPath))); err == nil {
				return dir
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatalf("could not find the repository root from %s", dir)
	return ""
}

// fixtureDistModules is the runtime module graph the OpenCode adapter
// really has: the compiled entrypoint imports six siblings, and two of
// those import each other.
//
// The graph is the whole point of the fixture. A package carrying only
// server.js and dist/index.v2.js — what the updater used to stage —
// has six unresolvable imports and loads NOTHING, silently.
var fixtureDistModules = map[string][]string{
	"event-envelope.js":     {},
	"event-mapper.js":       {"event-envelope.js"},
	"event-subscriber.js":   {"event-envelope.js", "event-mapper.js"},
	"resolve-mpm-binary.js": {},
	"wake-context.js":       {},
	"workspace.js":          {},
	"index.v2.js":           {"event-envelope.js", "event-mapper.js", "event-subscriber.js", "resolve-mpm-binary.js", "wake-context.js", "workspace.js"},
}

// fixtureBuildScript is the npm build the fixture adapter runs. It
// reproduces the real emit: one .js per module, each importing the
// modules the real ones import.
//
// It is written without template literals so it can live in a Go raw
// string unchanged.
const fixtureBuildScript = `import { mkdirSync, writeFileSync } from "node:fs";
mkdirSync("dist", { recursive: true });
const graph = {
  "event-envelope.js": [],
  "event-mapper.js": ["event-envelope.js"],
  "event-subscriber.js": ["event-envelope.js", "event-mapper.js"],
  "resolve-mpm-binary.js": [],
  "wake-context.js": [],
  "workspace.js": [],
  "index.v2.js": ["event-envelope.js", "event-mapper.js", "event-subscriber.js", "resolve-mpm-binary.js", "wake-context.js", "workspace.js"],
};
for (const [name, deps] of Object.entries(graph)) {
  const lines = deps.map((d) => "import { x } from \"./" + d + "\";");
  lines.push("export const name = \"" + name + "\";");
  writeFileSync("dist/" + name, lines.join("\n") + "\n");
}
`

// fixturePackageJSON is the adapter's npm package: a build script, and
// a "main" that names the compiled entrypoint.
const fixturePackageJSON = `{
  "name": "mpm-opencode",
  "version": "0.1.0",
  "type": "module",
  "main": "./dist/index.v2.js",
  "scripts": {
    "build": "node build.mjs"
  }
}
`

// fixturePackageLock is the lockfile npm ci requires. The fixture
// declares no dependencies, so it installs nothing and needs no
// network.
const fixturePackageLock = `{
  "name": "mpm-opencode",
  "version": "0.1.0",
  "lockfileVersion": 3,
  "requires": true,
  "packages": {
    "": {
      "name": "mpm-opencode",
      "version": "0.1.0"
    }
  }
}
`

// fixtureCheckout writes a throwaway source checkout that looks like an
// MPM tree: the canonical manifest, and an adapter whose runtime
// package is produced by an npm build.
//
// mutate (optional) runs after the tree is written and before it is
// staged in git, so a test can break exactly one thing.
func fixtureCheckout(t *testing.T, mutate func(root string)) string {
	t.Helper()
	root := t.TempDir()

	manifest := filepath.Join(root, filepath.FromSlash(payloadManifestRelPath))
	if err := os.MkdirAll(filepath.Dir(manifest), 0755); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), filepath.FromSlash(payloadManifestRelPath)))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifest, raw, 0644); err != nil {
		t.Fatal(err)
	}

	adapter := filepath.Join(root, "agent_installation", "mpm-opencode")
	if err := os.MkdirAll(adapter, 0755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"package.json":      fixturePackageJSON,
		"package-lock.json": fixturePackageLock,
		"build.mjs":         fixtureBuildScript,
		"server.js":         "export { default } from \"./dist/index.v2.js\";\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(adapter, name), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}

	if mutate != nil {
		mutate(root)
	}
	gitInit(t, root)
	return root
}

// gitInit turns a directory into a git repository with everything
// staged, so git ls-files reports the tree's tracked files.
func gitInit(t *testing.T, dir string) {
	t.Helper()
	cmds := [][]string{
		{"init", "--quiet"},
		{"add", "-A"},
	}
	for _, args := range cmds {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...) //nolint:gosec
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, out)
		}
	}
}

// writeFixtureManifest rewrites the fixture's manifest with the given
// records.
func writeFixtureManifest(t *testing.T, root string, payloads []map[string]interface{}) {
	t.Helper()
	doc := map[string]interface{}{
		"version":  1,
		"payloads": payloads,
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, filepath.FromSlash(payloadManifestRelPath))
	if err := os.WriteFile(path, raw, 0644); err != nil {
		t.Fatal(err)
	}
}

// ---------------------------------------------------------------------
// The manifest
// ---------------------------------------------------------------------

// TestTheCanonicalManifestIsTheOneTheUpdaterReads proves the updater
// and the shell staging path read the SAME declaration.
//
// The shell reads scripts/runtime_payloads.json (RB_PAYLOAD_MANIFEST)
// and the adapter's own OPENCODE_RUNTIME_MANIFEST must agree with it.
// If the updater read a different list, a bundle could pass the
// updater's checks and still be a bundle the installer refuses.
func TestTheCanonicalManifestIsTheOneTheUpdaterReads(t *testing.T) {
	root := repoRoot(t)
	m, err := loadPayloadManifest(filepath.Join(root, filepath.FromSlash(payloadManifestRelPath)))
	if err != nil {
		t.Fatalf("the canonical manifest does not load: %v", err)
	}
	if len(m.Payloads) == 0 {
		t.Fatal("the canonical manifest declares no payloads")
	}
	var found bool
	for _, p := range m.Payloads {
		if p.Adapter != "mpm-opencode" {
			continue
		}
		found = true
		// The nine files the adapter's installer declares. If the two
		// lists drift, one of the two paths stages a package the other
		// refuses.
		want := []string{
			"server.js",
			"dist/index.v2.js",
			"dist/wake-context.js",
			"dist/event-envelope.js",
			"dist/event-mapper.js",
			"dist/event-subscriber.js",
			"dist/workspace.js",
			"dist/resolve-mpm-binary.js",
			"package.json",
		}
		got := map[string]bool{}
		for _, f := range p.Files {
			got[f] = true
		}
		for _, f := range want {
			if !got[f] {
				t.Errorf("the canonical manifest does not declare %q", f)
			}
		}
		if len(p.Files) != len(want) {
			t.Errorf("the canonical manifest declares %d file(s), want %d: %v", len(p.Files), len(want), p.Files)
		}
	}
	if !found {
		t.Fatal("the canonical manifest declares no mpm-opencode payload")
	}
}

// TestTheManifestRejectsAnIncompleteDeclaration proves the manifest
// parser fails closed rather than reading an incomplete record as an
// empty one.
func TestTheManifestRejectsAnIncompleteDeclaration(t *testing.T) {
	cases := []struct {
		name    string
		payload map[string]interface{}
		want    string
	}{
		{"no adapter", map[string]interface{}{"plugin_id": "p", "validator": "v", "files": []string{"a"}}, "no adapter"},
		{"no plugin id", map[string]interface{}{"adapter": "a", "validator": "v", "files": []string{"a"}}, "no plugin id"},
		{"no validator", map[string]interface{}{"adapter": "a", "plugin_id": "p", "files": []string{"a"}}, "no validator"},
		{"no files", map[string]interface{}{"adapter": "a", "plugin_id": "p", "validator": "v"}, "no files"},
		{"escaping file", map[string]interface{}{"adapter": "a", "plugin_id": "p", "validator": "v", "files": []string{"../escape.js"}}, "outside the package"},
		{"absolute file", map[string]interface{}{"adapter": "a", "plugin_id": "p", "validator": "v", "files": []string{"/etc/passwd"}}, "absolute path"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := fixtureCheckout(t, nil)
			writeFixtureManifest(t, root, []map[string]interface{}{tc.payload})
			_, err := loadPayloadManifest(filepath.Join(root, filepath.FromSlash(payloadManifestRelPath)))
			if err == nil {
				t.Fatal("an incomplete manifest was accepted")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

// TestTheManifestRejectsAnUnreadableDeclaration proves a missing or
// malformed manifest is an error, not an empty payload set.
func TestTheManifestRejectsAnUnreadableDeclaration(t *testing.T) {
	root := fixtureCheckout(t, nil)
	path := filepath.Join(root, filepath.FromSlash(payloadManifestRelPath))
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := loadPayloadManifest(path); err == nil {
		t.Error("a missing manifest was accepted")
	}
	if err := os.WriteFile(path, []byte("{not json"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadPayloadManifest(path); err == nil {
		t.Error("a malformed manifest was accepted")
	}
	if err := os.WriteFile(path, []byte(`{"version":1,"payloads":[]}`), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadPayloadManifest(path); err == nil {
		t.Error("a manifest declaring no payloads was accepted")
	}
}

// ---------------------------------------------------------------------
// Staging
// ---------------------------------------------------------------------

// TestStagingStagesEveryDeclaredFile proves the updater stages the
// WHOLE declared payload, not a hand-maintained subset.
//
// The defect this pins: the updater staged server.js and
// dist/index.v2.js and called that a payload. The bundle it produced
// passed every check and installed a plugin that could not load.
func TestStagingStagesEveryDeclaredFile(t *testing.T) {
	root := fixtureCheckout(t, nil)
	// Build the fixture's payload the way the npm build would.
	buildFixturePayload(t, root)

	dst := filepath.Join(t.TempDir(), "bundle")
	if err := stagePayloads(root, dst, nil); err != nil {
		t.Fatalf("staging failed: %v", err)
	}

	m, err := loadPayloadManifest(filepath.Join(root, filepath.FromSlash(payloadManifestRelPath)))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range m.Payloads {
		for _, f := range p.Files {
			full := filepath.Join(dst, "agent_installation", p.Adapter, filepath.FromSlash(f))
			fi, err := os.Stat(full)
			if err != nil {
				t.Errorf("the declared file %s was not staged: %v", f, err)
				continue
			}
			if fi.Size() == 0 {
				t.Errorf("the declared file %s was staged empty", f)
			}
		}
	}
	// The bundle carries its own declaration.
	carried := filepath.Join(dst, filepath.FromSlash(bundlePayloadManifestRelPath))
	if _, err := os.Stat(carried); err != nil {
		t.Errorf("the bundle does not carry its payload declaration: %v", err)
	}
}

// TestStagingRefusesAnIncompleteManifest proves that a manifest which
// declares a SUBSET of what the package needs is refused.
//
// This is the half of the defect that a file-count check would miss:
// the manifest is present, the files it names are present, and the
// package still cannot load.
func TestStagingRefusesAnIncompleteManifest(t *testing.T) {
	root := fixtureCheckout(t, nil)
	buildFixturePayload(t, root)
	writeFixtureManifest(t, root, []map[string]interface{}{{
		"adapter":   "mpm-opencode",
		"plugin_id": "mpm-opencode",
		"validator": "mpm_validate_runtime_package_opencode",
		"files":     []string{"server.js", "dist/index.v2.js"},
	}})

	dst := filepath.Join(t.TempDir(), "bundle")
	err := stagePayloads(root, dst, nil)
	if err == nil {
		t.Fatal("an incomplete manifest was staged")
	}
	if !strings.Contains(err.Error(), "unresolved internal imports") {
		t.Errorf("error %q does not name the unresolved imports", err)
	}
}

// TestStagingRefusesAMissingDeclaredFile proves a declared file the
// checkout does not contain is a build error.
func TestStagingRefusesAMissingDeclaredFile(t *testing.T) {
	root := fixtureCheckout(t, nil)
	buildFixturePayload(t, root)
	// Remove a declared file the checkout is supposed to carry.
	if err := os.Remove(filepath.Join(root, "agent_installation", "mpm-opencode", "dist", "wake-context.js")); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(t.TempDir(), "bundle")
	err := stagePayloads(root, dst, nil)
	if err == nil {
		t.Fatal("a missing declared file was staged")
	}
	if !strings.Contains(err.Error(), "wake-context.js") {
		t.Errorf("error %q does not name the missing file", err)
	}
}

// TestStagingRefusesAnEmptyDeclaredFile proves an empty declared file
// is refused: an empty module resolves to nothing.
func TestStagingRefusesAnEmptyDeclaredFile(t *testing.T) {
	root := fixtureCheckout(t, nil)
	buildFixturePayload(t, root)
	empty := filepath.Join(root, "agent_installation", "mpm-opencode", "dist", "workspace.js")
	if err := os.WriteFile(empty, nil, 0644); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(t.TempDir(), "bundle")
	if err := stagePayloads(root, dst, nil); err == nil {
		t.Fatal("an empty declared file was staged")
	}
}

// ---------------------------------------------------------------------
// Loadability
// ---------------------------------------------------------------------

// TestValidationRejectsAMissingImportedModule proves the loadability
// check catches a package whose module graph is broken.
//
// The package is staged COMPLETELY and then one module is removed —
// exactly the state the old two-file staging produced.
func TestValidationRejectsAMissingImportedModule(t *testing.T) {
	root := fixtureCheckout(t, nil)
	buildFixturePayload(t, root)
	dst := filepath.Join(t.TempDir(), "bundle")
	if err := stagePayloads(root, dst, nil); err != nil {
		t.Fatalf("staging failed: %v", err)
	}

	removed := filepath.Join(dst, "agent_installation", "mpm-opencode", "dist", "wake-context.js")
	if err := os.Remove(removed); err != nil {
		t.Fatal(err)
	}
	ok, detail := validateBundlePayload(dst)
	if ok {
		t.Fatal("a bundle with a missing imported module was accepted")
	}
	if !strings.Contains(detail, "wake-context.js") {
		t.Errorf("error %q does not name the missing module", detail)
	}
}

// TestValidationRejectsAnEmptyDeclaredFile proves an empty file in a
// staged package is refused.
func TestValidationRejectsAnEmptyDeclaredFile(t *testing.T) {
	root := fixtureCheckout(t, nil)
	buildFixturePayload(t, root)
	dst := filepath.Join(t.TempDir(), "bundle")
	if err := stagePayloads(root, dst, nil); err != nil {
		t.Fatalf("staging failed: %v", err)
	}
	empty := filepath.Join(dst, "agent_installation", "mpm-opencode", "dist", "workspace.js")
	if err := os.WriteFile(empty, nil, 0644); err != nil {
		t.Fatal(err)
	}
	if ok, detail := validateBundlePayload(dst); ok {
		t.Fatalf("a bundle with an empty declared file was accepted: %s", detail)
	}
}

// TestValidationAcceptsACompletePayload proves the happy path: a
// complete, loadable package validates.
func TestValidationAcceptsACompletePayload(t *testing.T) {
	root := fixtureCheckout(t, nil)
	buildFixturePayload(t, root)
	dst := filepath.Join(t.TempDir(), "bundle")
	if err := stagePayloads(root, dst, nil); err != nil {
		t.Fatalf("staging failed: %v", err)
	}
	ok, detail := validateBundlePayload(dst)
	if !ok {
		t.Fatalf("a complete payload was refused: %s", detail)
	}
	if !strings.Contains(detail, "loadable") {
		t.Errorf("detail %q does not report the payload as loadable", detail)
	}
}

// TestValidationUsesTheBundlesOwnDeclaration proves the check reads
// the declaration the bundle carries.
//
// A bundle whose declaration names a file it does not carry is
// refused, even though the structural check alone would pass: the
// structural check cannot know which files were REQUIRED, and the
// requirement is exactly what the declaration states.
func TestValidationUsesTheBundlesOwnDeclaration(t *testing.T) {
	root := fixtureCheckout(t, nil)
	buildFixturePayload(t, root)
	dst := filepath.Join(t.TempDir(), "bundle")
	if err := stagePayloads(root, dst, nil); err != nil {
		t.Fatalf("staging failed: %v", err)
	}
	// Extend the bundle's own declaration with a file it does not
	// carry — the shape of a bundle assembled from a manifest that has
	// since grown.
	carried := filepath.Join(dst, filepath.FromSlash(bundlePayloadManifestRelPath))
	raw, err := os.ReadFile(carried)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]interface{}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	records := doc["payloads"].([]interface{})
	first := records[0].(map[string]interface{})
	files := first["files"].([]interface{})
	first["files"] = append(files, "dist/never-built.js")
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(carried, out, 0644); err != nil {
		t.Fatal(err)
	}
	if ok, detail := validateBundlePayload(dst); ok {
		t.Fatalf("a bundle missing a declared file was accepted: %s", detail)
	}
}

// TestValidationIgnoresBareSpecifiers proves the loadability check
// only requires what the PACKAGE owes itself.
//
// Bare specifiers ("@opencode/plugin", "node:fs") are supplied by the
// host framework or the runtime. They are unresolvable in the source
// checkout too, so requiring them would fail every correct package.
func TestValidationIgnoresBareSpecifiers(t *testing.T) {
	root := fixtureCheckout(t, nil)
	buildFixturePayload(t, root)
	adapter := filepath.Join(root, "agent_installation", "mpm-opencode")
	entry := filepath.Join(adapter, "dist", "index.v2.js")
	raw, err := os.ReadFile(entry)
	if err != nil {
		t.Fatal(err)
	}
	patched := "import { tool } from \"@opencode/plugin\";\nimport { path } from \"node:fs\";\n" + string(raw)
	if err := os.WriteFile(entry, []byte(patched), 0644); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(t.TempDir(), "bundle")
	if err := stagePayloads(root, dst, nil); err != nil {
		t.Fatalf("staging refused a package with host-provided imports: %v", err)
	}
}

// TestValidationFallsBackToTheStructureOfAnUndeclaredBundle proves a
// bundle assembled before the declaration travelled with it is still
// checked — structurally.
//
// The fallback is weaker (it cannot know which files were required)
// and never weaker than the check it replaces: a bundle with no
// loadable package is still refused.
func TestValidationFallsBackToTheStructureOfAnUndeclaredBundle(t *testing.T) {
	root := fixtureCheckout(t, nil)
	buildFixturePayload(t, root)
	dst := filepath.Join(t.TempDir(), "bundle")
	if err := stagePayloads(root, dst, nil); err != nil {
		t.Fatalf("staging failed: %v", err)
	}
	// Remove the declaration: the bundle now looks like one assembled
	// before it travelled with the bundle.
	if err := os.Remove(filepath.Join(dst, filepath.FromSlash(bundlePayloadManifestRelPath))); err != nil {
		t.Fatal(err)
	}
	ok, detail := validateBundlePayload(dst)
	if !ok {
		t.Fatalf("an undeclared but loadable bundle was refused: %s", detail)
	}
	if !strings.Contains(detail, "structurally") {
		t.Errorf("detail %q does not report the structural fallback", detail)
	}

	// And a bundle with no loadable package at all is still refused.
	broken := filepath.Join(t.TempDir(), "broken")
	if err := os.MkdirAll(filepath.Join(broken, "agent_installation", "mpm-opencode"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(broken, "agent_installation", "mpm-opencode", "package.json"), []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	if ok, _ := validateBundlePayload(broken); ok {
		t.Error("a bundle with no loadable package was accepted")
	}
}

// ---------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------

// buildFixturePayload runs the fixture's npm build so the checkout
// carries the compiled payload, the way a developer's tree does.
func buildFixturePayload(t *testing.T, root string) {
	t.Helper()
	adapter := filepath.Join(root, "agent_installation", "mpm-opencode")
	cmd := exec.Command("node", "build.mjs") //nolint:gosec
	cmd.Dir = adapter
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the fixture payload could not be built: %v\n%s", err, out)
	}
}
