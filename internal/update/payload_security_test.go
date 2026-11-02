package update

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------
// Issue 1: manifest adapter names must be single safe directory segments
// ---------------------------------------------------------------------

// TestManifestRejectsATraversalAdapterName proves a manifest whose
// adapter escapes its package directory is refused at parse time —
// before any path is built from it, let alone written.
//
// stagePayloads, buildRuntimePayloads and bundle validation all join
// agent_installation/ with the adapter name, so "../.." or
// "../../outside" would redirect filesystem operations outside the
// authorised roots. The file list already gets this treatment
// (checkPayloadFileName); the adapter did not.
func TestManifestRejectsATraversalAdapterName(t *testing.T) {
	adapters := []string{
		"../..",
		"../../outside",
		"..",
		".",
		"/absolute",
		"a/b",
		`a\b`,
		"mpm-opencode/",
		"mpm-opencode/.",
	}
	for _, adapter := range adapters {
		t.Run("adapter="+adapter, func(t *testing.T) {
			raw, err := json.Marshal(map[string]interface{}{
				"version": 1,
				"payloads": []map[string]interface{}{
					{"adapter": adapter, "plugin_id": "p", "validator": "v", "files": []string{"server.js"}},
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := parsePayloadManifest(raw, "test"); err == nil {
				t.Fatalf("a manifest declaring the adapter %q was accepted", adapter)
			}
		})
	}
}

// TestManifestRejectsDuplicateFileNames proves a package that declares
// the same file twice is refused. A duplicated entry is either a
// mistake or a smuggled second meaning for one name; either way the
// parser must not silently accept it.
func TestManifestRejectsDuplicateFileNames(t *testing.T) {
	raw, err := json.Marshal(map[string]interface{}{
		"version": 1,
		"payloads": []map[string]interface{}{
			{"adapter": "a", "plugin_id": "p", "validator": "v", "files": []string{"server.js", "server.js"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parsePayloadManifest(raw, "test"); err == nil {
		t.Fatal("a manifest declaring the same file twice was accepted")
	}
}

// TestStagingRefusesATraversalAdapterBeforeAnyWrite proves the
// traversal is stopped before any external path is read or written.
//
// The fixture checkout carries a file OUTSIDE agent_installation/ and
// a manifest whose adapter names that outside directory. Refusing at
// parse time means staging fails with the checkout's outside file
// untouched and the escape destination unwritten.
func TestStagingRefusesATraversalAdapterBeforeAnyWrite(t *testing.T) {
	root := fixtureCheckout(t, nil)
	outside := filepath.Join(root, "escape-pkg", "server.js")
	if err := os.MkdirAll(filepath.Dir(outside), 0755); err != nil {
		t.Fatal(err)
	}
	sentinel := "export const untouched = 1;\n"
	if err := os.WriteFile(outside, []byte(sentinel), 0644); err != nil {
		t.Fatal(err)
	}
	writeFixtureManifest(t, root, []map[string]interface{}{
		{"adapter": "../escape-pkg", "plugin_id": "p", "validator": "v", "files": []string{"server.js"}},
	})

	dst := filepath.Join(t.TempDir(), "bundle")
	if err := stagePayloads(root, dst, nil); err == nil {
		t.Fatal("a manifest with a traversal adapter was staged")
	}
	// The escape destination — outside the authorised package root
	// dst/agent_installation/escape-pkg — must not exist.
	if _, err := os.Stat(filepath.Join(dst, "escape-pkg", "server.js")); !os.IsNotExist(err) {
		t.Fatalf("staging wrote outside the authorised package root: %v", err)
	}
	// And the checkout's outside file must be byte-identical.
	raw, err := os.ReadFile(outside)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != sentinel {
		t.Fatal("the checkout's outside file was modified by a refused stage")
	}
}

// ---------------------------------------------------------------------
// Issue 2: relative imports must stay inside the staged package
// ---------------------------------------------------------------------

// TestRelativeImportOutsideThePackageIsUnresolved proves an import
// that resolves outside the package root is reported even when the
// external target genuinely exists.
//
// A nonexistent target proves nothing about containment — it fails
// for the wrong reason. Here the outside file is real, so acceptance
// would prove the escape, and refusal proves containment.
func TestRelativeImportOutsideThePackageIsUnresolved(t *testing.T) {
	root := t.TempDir()
	pkg := filepath.Join(root, "agent_installation", "mpm-opencode")
	if err := os.MkdirAll(pkg, 0755); err != nil {
		t.Fatal(err)
	}
	// One level above the package: exactly where "../outside.js" in
	// server.js resolves. The file genuinely exists, so acceptance
	// would prove the escape and refusal proves containment — a
	// missing target would fail for the wrong reason.
	outside := filepath.Join(root, "agent_installation", "outside.js")
	if err := os.WriteFile(outside, []byte("export const x = 1;\n"), 0644); err != nil {
		t.Fatal(err)
	}
	server := filepath.Join(pkg, "server.js")
	if err := os.WriteFile(server, []byte("import { x } from \"../outside.js\";\nexport const y = x;\n"), 0644); err != nil {
		t.Fatal(err)
	}

	if resolvesInPackage(pkg, server, "../outside.js") {
		t.Fatal("an import resolving outside the package was treated as resolved")
	}
	bad, err := unresolvedRelativeImports(pkg)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, b := range bad {
		if strings.Contains(b, "../outside.js") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the escaping import was not reported as unresolved: %v", bad)
	}
}

// TestRelativeImportThroughASymlinkIsUnresolved proves filesystem
// indirection cannot smuggle an external file into the package: a
// symlink inside the package that points outside must not count as a
// resolved import, even though the link target exists.
func TestRelativeImportThroughASymlinkIsUnresolved(t *testing.T) {
	root := t.TempDir()
	pkg := filepath.Join(root, "agent_installation", "mpm-opencode")
	if err := os.MkdirAll(pkg, 0755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "outside.js")
	if err := os.WriteFile(outside, []byte("export const x = 1;\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(pkg, "linked.js")); err != nil {
		t.Fatal(err)
	}
	inner := filepath.Join(pkg, "inner.js")
	if err := os.WriteFile(inner, []byte("import { x } from \"./linked.js\";\nexport const y = x;\n"), 0644); err != nil {
		t.Fatal(err)
	}

	if resolvesInPackage(pkg, inner, "./linked.js") {
		t.Fatal("an import reaching outside the package through a symlink was treated as resolved")
	}
}

// TestRelativeImportsWithinThePackageStillResolve pins the
// legitimate shapes: ./ siblings and ../ modules that stay inside
// the package root must keep validating.
func TestRelativeImportsWithinThePackageStillResolve(t *testing.T) {
	root := t.TempDir()
	pkg := filepath.Join(root, "agent_installation", "mpm-opencode")
	sub := filepath.Join(pkg, "dist")
	if err := os.MkdirAll(sub, 0755); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"server.js":         "import { x } from \"./dist/index.v2.js\";\nexport const y = x;\n",
		"shared.js":         "export const s = 1;\n",
		"dist/index.v2.js":  "import { s } from \"../shared.js\";\nimport { w } from \"./wake.js\";\nexport const x = s + w;\n",
		"dist/wake.js":      "export const w = 2;\n",
		"dist/uses-node.js": "import { readFileSync } from \"node:fs\";\nimport { tool } from \"@opencode/plugin\";\nexport const z = 3;\n",
	} {
		if err := os.WriteFile(filepath.Join(pkg, filepath.FromSlash(name)), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if bad, err := unresolvedRelativeImports(pkg); err != nil || len(bad) != 0 {
		t.Fatalf("legitimate in-package imports were reported unresolved: %v, %v", bad, err)
	}
}

// ---------------------------------------------------------------------
// Issue 3: legacy bundles keep the mandatory two-file minimum
// ---------------------------------------------------------------------

// writeLegacyPackage writes a manifestless legacy OpenCode package:
// package.json plus a nonempty server.js with no imports, and
// optionally a nonempty dist/index.v2.js.
func writeLegacyPackage(t *testing.T, bundleDir string, withIndex bool) {
	t.Helper()
	pkg := filepath.Join(bundleDir, "agent_installation", "mpm-opencode")
	if err := os.MkdirAll(filepath.Join(pkg, "dist"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkg, "package.json"), []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkg, "server.js"), []byte("export const loaded = true;\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if withIndex {
		if err := os.WriteFile(filepath.Join(pkg, "dist", "index.v2.js"), []byte("export const entry = true;\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}
}

// TestLegacyBundleWithoutTheCompiledEntryIsRefused proves the
// structural fallback keeps the pre-PR guarantee: a legacy bundle
// with a nonempty server.js but no dist/index.v2.js is refused, even
// when server.js has no imports to trip the module-graph check.
func TestLegacyBundleWithoutTheCompiledEntryIsRefused(t *testing.T) {
	bundle := filepath.Join(t.TempDir(), "bundle")
	writeLegacyPackage(t, bundle, false)
	if ok, detail := validateBundlePayload(bundle); ok {
		t.Fatalf("a legacy bundle without dist/index.v2.js was accepted: %s", detail)
	}
}

// TestLegacyBundleWithBothEntryFilesIsAccepted pins compatibility: a
// legacy bundle carrying both historically required files still
// validates through the structural fallback.
func TestLegacyBundleWithBothEntryFilesIsAccepted(t *testing.T) {
	bundle := filepath.Join(t.TempDir(), "bundle")
	writeLegacyPackage(t, bundle, true)
	ok, detail := validateBundlePayload(bundle)
	if !ok {
		t.Fatalf("a legacy bundle with both entry files was refused: %s", detail)
	}
	if !strings.Contains(detail, "structurally") {
		t.Errorf("detail %q does not report the structural fallback", detail)
	}
}

// TestUnreadableManifestIsNotAFallback proves a manifest that cannot
// be READ is a validation failure, not a reason to fall back.
//
// The bundle below carries a complete legacy package, so the
// structural fallback WOULD accept it. The manifest path exists but is
// a directory, so reading it fails with something other than
// not-exist: only a genuinely absent manifest may fall back.
func TestUnreadableManifestIsNotAFallback(t *testing.T) {
	bundle := filepath.Join(t.TempDir(), "bundle")
	writeLegacyPackage(t, bundle, true)
	manifestPath := filepath.Join(bundle, filepath.FromSlash(bundlePayloadManifestRelPath))
	if err := os.MkdirAll(manifestPath, 0755); err != nil {
		t.Fatal(err)
	}
	ok, detail := validateBundlePayload(bundle)
	if ok {
		t.Fatalf("a bundle with an unreadable manifest fell back instead of failing: %s", detail)
	}
}

// TestMalformedManifestIsNotAFallback pins the adjacent guarantee: a
// manifest that parses as nothing is a failure, even beside a package
// the structural check would accept.
func TestMalformedManifestIsNotAFallback(t *testing.T) {
	bundle := filepath.Join(t.TempDir(), "bundle")
	writeLegacyPackage(t, bundle, true)
	manifestPath := filepath.Join(bundle, filepath.FromSlash(bundlePayloadManifestRelPath))
	if err := os.MkdirAll(filepath.Dir(manifestPath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, []byte("{not json"), 0644); err != nil {
		t.Fatal(err)
	}
	ok, detail := validateBundlePayload(bundle)
	if ok {
		t.Fatalf("a bundle with a malformed manifest was accepted: %s", detail)
	}
	if !strings.Contains(detail, "does not parse") {
		t.Errorf("detail %q does not name the parse failure", detail)
	}
}
