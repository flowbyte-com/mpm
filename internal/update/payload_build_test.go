package update

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------
// The npm build
// ---------------------------------------------------------------------

// requireNpm skips a test when the toolchain it exercises is absent.
//
// The toolchain is a documented PREREQUISITE of a development build,
// not of the product: an operator who updates from a published release
// never needs Node. So a host without it must still be able to run the
// test suite.
func requireNpm(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("npm"); err != nil {
		t.Skip("npm is not available on this host")
	}
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is not available on this host")
	}
}

// TestTheBuildProducesTheWholePayloadFromAFreshCheckout is the
// development-build defect's own test.
//
// A fresh clone has no dist/ — the compiled plugin output is a build
// product and is gitignored. The updater must BUILD the payload in the
// checkout before it stages it, or there is no payload to stage.
func TestTheBuildProducesTheWholePayloadFromAFreshCheckout(t *testing.T) {
	requireNpm(t)
	root := fixtureCheckout(t, nil)

	// A fresh clone has no compiled output at all.
	dist := filepath.Join(root, "agent_installation", "mpm-opencode", "dist")
	if _, err := os.Stat(dist); err == nil {
		t.Fatal("the fixture checkout already carries compiled output; it is not a fresh checkout")
	}

	sources, err := buildRuntimePayloads(root, payloadBuildEnv{Scratch: t.TempDir(), Cache: t.TempDir()})
	if err != nil {
		t.Fatalf("the development build failed: %v", err)
	}
	built := sources["mpm-opencode"]
	if built == "" {
		t.Fatal("the build produced no source for the mpm-opencode payload")
	}
	m, err := loadPayloadManifest(filepath.Join(root, filepath.FromSlash(payloadManifestRelPath)))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range m.Payloads {
		for _, f := range p.Files {
			full := filepath.Join(built, filepath.FromSlash(f))
			fi, err := os.Stat(full)
			if err != nil {
				t.Errorf("the build did not produce the declared file %s: %v", f, err)
				continue
			}
			if fi.Size() == 0 {
				t.Errorf("the build produced an empty %s", f)
			}
		}
	}
}

// TestTheBuildStandsAlone proves the whole pipeline works on a fresh
// checkout: build, then stage, then validate.
func TestTheBuildStandsAlone(t *testing.T) {
	requireNpm(t)
	root := fixtureCheckout(t, nil)
	scratch := t.TempDir()
	sources, err := buildRuntimePayloads(root, payloadBuildEnv{Scratch: scratch, Cache: scratch})
	if err != nil {
		t.Fatalf("the development build failed: %v", err)
	}
	dst := filepath.Join(t.TempDir(), "bundle")
	if err := stagePayloads(root, dst, sources); err != nil {
		t.Fatalf("staging failed: %v", err)
	}
	ok, detail := validateBundlePayload(dst)
	if !ok {
		t.Fatalf("the built payload does not validate: %s", detail)
	}
}

// TestTheBuildRefusesWithoutNode proves a missing Node is reported as
// such.
//
// The message has to name the missing tool: "the build failed" tells an
// operator nothing they can act on.
func TestTheBuildRefusesWithoutNode(t *testing.T) {
	requireNpm(t)
	root := fixtureCheckout(t, nil)
	empty := t.TempDir()
	t.Setenv("PATH", empty)

	_, err := buildRuntimePayloads(root, payloadBuildEnv{Scratch: t.TempDir(), Cache: t.TempDir()})
	if err == nil {
		t.Fatal("a development build without node was accepted")
	}
	if !strings.Contains(err.Error(), "node") {
		t.Errorf("error %q does not name the missing tool", err)
	}
	if !strings.Contains(err.Error(), "published release") {
		t.Errorf("error %q does not mention the alternative that needs no build tools", err)
	}
}

// TestTheBuildRefusesWithoutNpm proves a missing npm is reported as
// such, separately from node.
func TestTheBuildRefusesWithoutNpm(t *testing.T) {
	requireNpm(t)
	root := fixtureCheckout(t, nil)

	// A PATH with node but without npm.
	bin := t.TempDir()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not available on this host")
	}
	if err := symlink(node, filepath.Join(bin, "node")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)

	_, err = buildRuntimePayloads(root, payloadBuildEnv{Scratch: t.TempDir(), Cache: t.TempDir()})
	if err == nil {
		t.Fatal("a development build without npm was accepted")
	}
	if !strings.Contains(err.Error(), "npm") {
		t.Errorf("error %q does not name the missing tool", err)
	}
}

// TestTheBuildRefusesWhenTheBuildFails proves a failing npm build
// stops the update.
//
// There is no fallback here: the alternative to a successful build is
// to stage compiled files that belong to some other revision, and a
// bundle that carries them is worse than no bundle.
func TestTheBuildRefusesWhenTheBuildFails(t *testing.T) {
	requireNpm(t)
	root := fixtureCheckout(t, func(root string) {
		build := filepath.Join(root, "agent_installation", "mpm-opencode", "build.mjs")
		if err := os.WriteFile(build, []byte("process.exit(3);\n"), 0644); err != nil {
			t.Fatal(err)
		}
	})

	_, err := buildRuntimePayloads(root, payloadBuildEnv{Scratch: t.TempDir(), Cache: t.TempDir()})
	if err == nil {
		t.Fatal("a failing npm build was accepted")
	}
	if !strings.Contains(err.Error(), "npm") {
		t.Errorf("error %q does not name the failing command", err)
	}
}

// TestTheBuildNeverStagesStaleCompiledFiles proves a stale build
// output cannot reach the bundle.
//
// The scenario: a developer built the payload earlier, then changed the
// source so the module is no longer produced. The old file is still on
// disk. Staging it would ship a module the revision does not contain.
//
// The build therefore removes the declared files that are not in source
// control before it runs, and the staging then refuses the file the
// build did not produce.
func TestTheBuildNeverStagesStaleCompiledFiles(t *testing.T) {
	requireNpm(t)
	root := fixtureCheckout(t, func(root string) {
		adapter := filepath.Join(root, "agent_installation", "mpm-opencode")
		// A build that no longer produces wake-context.js.
		build := filepath.Join(adapter, "build.mjs")
		raw, err := os.ReadFile(build)
		if err != nil {
			t.Fatal(err)
		}
		shortened := strings.Replace(string(raw), "  \"wake-context.js\": [],\n", "", 1)
		shortened = strings.Replace(shortened, "\"resolve-mpm-binary.js\", \"wake-context.js\", ", "\"resolve-mpm-binary.js\", ", 1)
		if err := os.WriteFile(build, []byte(shortened), 0644); err != nil {
			t.Fatal(err)
		}
	})
	// The stale output from the previous build: present on disk, NOT in
	// source control — exactly what a compiled artefact is.
	adapter := filepath.Join(root, "agent_installation", "mpm-opencode")
	if err := os.MkdirAll(filepath.Join(adapter, "dist"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(adapter, "dist", "wake-context.js"), []byte("export const stale = true;\n"), 0644); err != nil {
		t.Fatal(err)
	}

	sources, err := buildRuntimePayloads(root, payloadBuildEnv{Scratch: t.TempDir(), Cache: t.TempDir()})
	if err != nil {
		t.Fatalf("the development build failed: %v", err)
	}
	built := sources["mpm-opencode"]
	if _, err := os.Stat(filepath.Join(built, "dist", "wake-context.js")); err == nil {
		t.Error("the stale compiled file survived the build")
	}
	// And staging it is refused, because the manifest declares it.
	dst := filepath.Join(t.TempDir(), "bundle")
	if err := stagePayloads(root, dst, sources); err == nil {
		t.Fatal("a payload missing a declared file was staged")
	}
}

// TestTheBuildLeavesTheSourceCheckoutUntouched proves the build does
// not write into the tree it was given.
//
// The updater promises that a local source is read only: "the build
// writes only to the destination bundle directory". `npm ci` would
// break that promise twice over — it creates node_modules/ and it
// rewrites dist/ in a tree the operator owns. So the build runs in an
// isolated copy.
func TestTheBuildLeavesTheSourceCheckoutUntouched(t *testing.T) {
	requireNpm(t)
	root := fixtureCheckout(t, nil)
	adapter := filepath.Join(root, "agent_installation", "mpm-opencode")

	if _, err := buildRuntimePayloads(root, payloadBuildEnv{Scratch: t.TempDir(), Cache: t.TempDir()}); err != nil {
		t.Fatalf("the development build failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(adapter, "node_modules")); err == nil {
		t.Error("the build created node_modules/ in the source checkout")
	}
	if _, err := os.Stat(filepath.Join(adapter, "dist")); err == nil {
		t.Error("the build wrote compiled output into the source checkout")
	}
}

// symlink creates a symbolic link, or copies the file when symlinks are
// unavailable.
func symlink(target, link string) error {
	if err := os.Symlink(target, link); err == nil {
		return nil
	}
	raw, err := os.ReadFile(target)
	if err != nil {
		return err
	}
	return os.WriteFile(link, raw, 0755)
}
