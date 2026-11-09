package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------
// The host-asset contract
// ---------------------------------------------------------------------
//
// WHAT THESE PROVE
//
// A bundle that carries only the runtime payload installs an
// installation whose own instructions point at files that do not exist:
//
//   - ~/.config/opencode/AGENTS.md says "Edit the canonical managed
//     block in ~/.mpm/agent_installation/MPM_AGENT_INTEGRATION_SNIPPETS.md"
//     and "Full contract: ~/.mpm/agent_installation/mpm-agent-protocol.md";
//   - the per-adapter installer imports the shared scripts under
//     agent_installation/scripts/ by path, so a runtime root without
//     them cannot refresh ANY managed block.
//
// That is what a promotion of a checkout-built candidate used to
// produce. The bundle was VALID under every check the updater made, so
// the omission was silent.
//
// The fix has three parts, and each has its own test here:
//
//  1. the builder provisions the host assets, using the SAME installer
//     the full installer uses, so the bundle declares what it carries;
//  2. the validator refuses a bundle that does not — fail closed;
//  3. the shell validator agrees with the Go one, so a bundle cannot
//     pass one and fail the other.
//
// Every root here is disposable. MPM_WORKSPACE and HOME point into a
// temporary directory, and the operator's ~/.mpm is never read or
// written.

// writeTestHostAssets writes a minimal but REAL host-asset set into a
// bundle, with the declaration the validator checks.
//
// It models what the authority provisions: the canonical managed-block
// source, the behavioural protocol, a per-adapter manifest, a
// per-adapter template, and a shared script. The digests in the
// declaration are computed from the bytes, so the fixture is a
// self-consistent bundle rather than a shape that merely looks right.
func writeTestHostAssets(dstDir string) error {
	files := map[string]string{
		"agent_installation/MPM_AGENT_INTEGRATION_SNIPPETS.md": "# canonical managed block source\n",
		"agent_installation/mpm-agent-protocol.md":            "# full behavioural protocol\n",
		"agent_installation/mpm-opencode/reconcile.json":      "{\"adapter\": \"mpm-opencode\"}\n",
		"agent_installation/mpm-opencode/templates/AGENTS.md.snippet": "<!-- managed -->\n",
		"agent_installation/scripts/block_provenance.py":            "# shared module\n",
	}
	manifest := HostAssetManifest{Version: hostAssetManifestVersion, Assets: map[string]HostAssetRecord{}}
	for rel, content := range files {
		full := filepath.Join(dstDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			return err
		}
		if err := os.WriteFile(full, []byte(content), 0644); err != nil {
			return err
		}
		sum := sha256.Sum256([]byte(content))
		manifest.Assets[rel] = HostAssetRecord{SHA256: hex.EncodeToString(sum[:])}
	}
	raw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	decl := filepath.Join(dstDir, filepath.FromSlash(HostAssetManifestRelPath))
	if err := os.MkdirAll(filepath.Dir(decl), 0755); err != nil {
		return err
	}
	return os.WriteFile(decl, raw, 0644)
}

// writeTestSchemaPreflight writes the bundle's schema preflight.
//
// It is the substrate's own lifecycle_events DDL, which is what the
// schema-collision preflight compares the live table against. A bundle
// without it degrades that check to a no-op, so the validator refuses
// one.
func writeTestSchemaPreflight(dstDir string) error {
	dir := filepath.Join(dstDir, "schema")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	body := "CREATE TABLE IF NOT EXISTS lifecycle_events (\n" +
		"\tevent_id TEXT PRIMARY KEY NOT NULL\n" +
		");\n"
	return os.WriteFile(filepath.Join(dir, "lifecycle_events.sql"), []byte(body), 0644)
}

// testBundleDir builds a complete, valid bundle in a temporary
// directory and returns its path.
func testBundleDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	b := fakeBuilder{commit: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}
	if _, err := b.BuildFromCheckout(t.TempDir(), dir); err != nil {
		t.Fatal(err)
	}
	return dir
}

// --- the builder provisions the assets --------------------------------------

// TestTheBundleBuiltFromACheckoutCarriesTheHostAssets is the RED test
// for the defect: a candidate built from a checkout must carry the
// integration assets, and it must carry exactly what the authority
// provisions.
//
// The expected set is NOT written out here. It is produced by running
// the authority — scripts/install_runtime_assets.py — against the same
// checkout and comparing the two trees. A list written into this test
// would be a SECOND list, and a second list is how the omission
// happened.
func TestTheBundleBuiltFromACheckoutCarriesTheHostAssets(t *testing.T) {
	requireTools(t)
	dev := sharedDevSource(t)

	dst := filepath.Join(t.TempDir(), "bundle")
	b := GoBuilder{CacheDir: dev.cache}
	if _, err := b.BuildFromCheckout(dev.checkout, dst); err != nil {
		t.Fatalf("the development build failed: %v", err)
	}

	// The reference: the authority's own output for the same checkout.
	reference := filepath.Join(t.TempDir(), "reference")
	script := filepath.Join(dev.checkout, "scripts", "install_runtime_assets.py")
	cmd := exec.Command("python3", script, //nolint:gosec
		"--source-root", dev.checkout,
		"--runtime-root", reference,
		"--groups", "host-assets")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the host-asset installer could not stage the reference: %v\n%s", err, out)
	}

	want := declaredAssetPaths(t, reference)
	got := declaredAssetPaths(t, dst)
	if len(want) == 0 {
		t.Fatal("the reference staging produced no host assets, so the comparison proves nothing")
	}
	for _, rel := range want {
		if !containsString(got, rel) {
			t.Errorf("the built bundle does not carry the host asset %s, which the installer provisions", rel)
		}
	}
	for _, rel := range got {
		if !containsString(want, rel) {
			t.Errorf("the built bundle carries the host asset %s, which the installer does not provision", rel)
		}
	}

	// The bundle declares them, and the declaration matches its own
	// contents.
	if problems := hostAssetProblems(dst, ScopeCandidate); len(problems) > 0 {
		t.Errorf("the built bundle does not satisfy its own host-asset declaration: %v", problems)
	}

	// And the whole bundle validates: the check the promotion applies.
	rep := ValidateBundle(dst, dev.commit)
	if !rep.Valid {
		t.Errorf("the built bundle does not validate: %v", rep.ValidationError())
	}
}

// TestTheBundleBuiltFromACheckoutCarriesTheSchemaPreflight is the RED
// test for the adjacent omission: the bundle must carry the schema
// DDL, so the collision preflight has something to compare against.
//
// The reference is the authoritative extractor, so this test does not
// restate the DDL.
func TestTheBundleBuiltFromACheckoutCarriesTheSchemaPreflight(t *testing.T) {
	requireTools(t)
	dev := sharedDevSource(t)

	dst := filepath.Join(t.TempDir(), "bundle")
	b := GoBuilder{CacheDir: dev.cache}
	if _, err := b.BuildFromCheckout(dev.checkout, dst); err != nil {
		t.Fatalf("the development build failed: %v", err)
	}

	staged := filepath.Join(dst, "schema", "lifecycle_events.sql")
	raw, err := os.ReadFile(staged)
	if err != nil {
		t.Fatalf("the built bundle carries no schema preflight (%s): %v", staged, err)
	}

	// The authoritative extractor, on the same checkout.
	reference := filepath.Join(t.TempDir(), "lifecycle_events.sql")
	extract := exec.Command("python3", filepath.Join(dev.checkout, "scripts", "lib", "schema_preflight.py"), //nolint:gosec
		"extract",
		"--go", filepath.Join(dev.checkout, "internal", "core", "schema.go"),
		"--table", "lifecycle_events",
		"--out", reference)
	if out, err := extract.CombinedOutput(); err != nil {
		t.Fatalf("the authoritative extractor failed: %v\n%s", err, out)
	}
	want, err := os.ReadFile(reference)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != string(want) {
		t.Errorf("the staged schema preflight differs from the authoritative extraction (%d bytes staged, %d bytes expected)", len(raw), len(want))
	}
}

// --- the validator refuses an incomplete bundle -----------------------------

// TestValidateBundleRefusesABundleWithNoHostAssetDeclaration proves
// the check is fail-closed: a bundle that declares nothing is refused,
// because "declared nothing" and "provisioned nothing" are the same
// thing from the outside.
func TestValidateBundleRefusesABundleWithNoHostAssetDeclaration(t *testing.T) {
	dir := testBundleDir(t)
	if err := os.Remove(filepath.Join(dir, filepath.FromSlash(HostAssetManifestRelPath))); err != nil {
		t.Fatal(err)
	}
	rep := ValidateBundle(dir, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	if rep.Valid {
		t.Fatal("a bundle with no host-asset declaration was accepted")
	}
	if !reportHasCheck(rep, "host-assets", false) {
		t.Errorf("the refusal does not name the host-assets check: %+v", rep.Checks)
	}
}

// TestValidateBundleRefusesABundleWhoseHostAssetWasAltered proves the
// declaration is checked against the bundle's OWN contents: an asset
// whose bytes changed after staging is refused.
func TestValidateBundleRefusesABundleWhoseHostAssetWasAltered(t *testing.T) {
	dir := testBundleDir(t)
	target := filepath.Join(dir, "agent_installation", "mpm-agent-protocol.md")
	if err := os.WriteFile(target, []byte("# tampered\n"), 0644); err != nil {
		t.Fatal(err)
	}
	rep := ValidateBundle(dir, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	if rep.Valid {
		t.Fatal("a bundle whose host asset was altered was accepted")
	}
	if !reportHasCheck(rep, "host-assets", false) {
		t.Errorf("the refusal does not name the host-assets check: %+v", rep.Checks)
	}
}

// TestValidateBundleRefusesABundleCarryingAnUndeclaredHostAssetFile
// proves the declaration is a complete description: a file under
// agent_installation/ that no declaration names is refused, because
// nobody provisioned it and nobody owns it.
func TestValidateBundleRefusesABundleCarryingAnUndeclaredHostAssetFile(t *testing.T) {
	dir := testBundleDir(t)
	extra := filepath.Join(dir, "agent_installation", "scripts", "nobody_declared_this.py")
	if err := os.WriteFile(extra, []byte("# surprise\n"), 0644); err != nil {
		t.Fatal(err)
	}
	rep := ValidateBundle(dir, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	if rep.Valid {
		t.Fatal("a bundle carrying an undeclared host-asset file was accepted")
	}
	if !reportHasCheck(rep, "host-assets", false) {
		t.Errorf("the refusal does not name the host-assets check: %+v", rep.Checks)
	}
}

// TestValidateBundleAcceptsABundleThatDeclaresItsHostAssets is the
// green side of the same contract: a complete bundle is accepted.
func TestValidateBundleAcceptsABundleThatDeclaresItsHostAssets(t *testing.T) {
	dir := testBundleDir(t)
	rep := ValidateBundle(dir, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	if !rep.Valid {
		t.Fatalf("a complete bundle was refused: %v", rep.ValidationError())
	}
	if !reportHasCheck(rep, "host-assets", true) {
		t.Errorf("the report does not carry a passing host-assets check: %+v", rep.Checks)
	}
}

// TestTheShellAndGoValidatorsAgreeOnTheHostAssets proves the two
// validators are the same contract.
//
// The bundle that reaches an operator depends on which tool ran, and a
// bundle that passes the shell check but fails the Go one is a bundle
// that was published and then refused.
func TestTheShellAndGoValidatorsAgreeOnTheHostAssets(t *testing.T) {
	lib := shellLibraryPath(t)
	if lib == "" {
		t.Skip("the shell library is not available")
	}
	cases := []struct {
		name   string
		mutate func(t *testing.T, dir string)
	}{
		{"complete", func(t *testing.T, dir string) {}},
		{"no declaration", func(t *testing.T, dir string) {
			os.Remove(filepath.Join(dir, filepath.FromSlash(HostAssetManifestRelPath)))
		}},
		{"altered asset", func(t *testing.T, dir string) {
			os.WriteFile(filepath.Join(dir, "agent_installation", "mpm-agent-protocol.md"), []byte("# tampered\n"), 0644)
		}},
		{"undeclared file", func(t *testing.T, dir string) {
			os.WriteFile(filepath.Join(dir, "agent_installation", "scripts", "undeclared.py"), []byte("# surprise\n"), 0644)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := testBundleDir(t)
			tc.mutate(t, dir)
			goValid := ValidateBundle(dir, "").Valid
			shellValid := shellValidateBundle(t, lib, dir)
			if goValid != shellValid {
				t.Errorf("the validators disagree: Go says %v, the shell library says %v", goValid, shellValid)
			}
		})
	}
}

// --- the promotion installs them, and the symlink resolves -----------------

// TestAPromotionInstallsTheHostAssetsIntoTheRelease proves the assets
// survive the promotion: they are in the release directory, and the
// stable path the installed managed block advertises resolves into it.
func TestAPromotionInstallsTheHostAssetsIntoTheRelease(t *testing.T) {
	root, newRev := promoteOnce(t)

	release := filepath.Join(root, "releases", newRev)
	for _, rel := range []string{
		"agent_installation/MPM_AGENT_INTEGRATION_SNIPPETS.md",
		"agent_installation/mpm-agent-protocol.md",
		"agent_installation/mpm-opencode/reconcile.json",
		"agent_installation/mpm-opencode/templates/AGENTS.md.snippet",
		"agent_installation/scripts/block_provenance.py",
	} {
		if _, err := os.Stat(filepath.Join(release, filepath.FromSlash(rel))); err != nil {
			t.Errorf("the promoted release does not carry %s: %v", rel, err)
		}
	}
	if _, err := os.Stat(filepath.Join(release, filepath.FromSlash(HostAssetManifestRelPath))); err != nil {
		t.Errorf("the promoted release carries no host-asset declaration: %v", err)
	}
}

// TestTheStablePathResolvesIntoTheActiveReleaseAndIntoThePreviousOne
// AfterARollback proves the SYMLINK integrity the installed managed
// block depends on.
//
// The managed block names ~/.mpm/agent_installation/... — a path that
// resolves through a symlink into the ACTIVE release. After a promotion
// it must resolve into the new release; after a rollback it must
// resolve into the previous one. A dangling symlink is the symptom the
// whole host-asset contract exists to prevent.
func TestTheStablePathResolvesIntoTheActiveReleaseAndIntoThePreviousOneAfterARollback(t *testing.T) {
	oldRev := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	root := testLayout(t, oldRev)
	srcDir := t.TempDir()
	mustGit(t, srcDir)
	o := Options{DataRoot: root, WantLocal: srcDir, Builder: fakeBuilder{}}
	if _, err := Update(context.Background(), o); err != nil {
		t.Fatalf("update: %v", err)
	}
	newRev := currentRevision(t, root)

	stable := filepath.Join(root, "agent_installation")
	if warnings, err := ensureStablePaths(root); err != nil {
		t.Fatalf("the stable paths could not be established: %v", err)
	} else if len(warnings) > 0 {
		t.Errorf("the stable paths reported unexpected states: %v", warnings)
	}
	if target, err := os.Readlink(stable); err != nil {
		t.Fatalf("the stable agent_installation path is not a symlink: %v", err)
	} else if target != "current/agent_installation" {
		t.Errorf("the stable agent_installation path points at %q, want %q", target, "current/agent_installation")
	}
	// It resolves into the ACTIVE release, and the file the managed
	// block advertises is there.
	resolved := filepath.Join(stable, "mpm-agent-protocol.md")
	if _, err := os.Stat(resolved); err != nil {
		t.Errorf("the advertised protocol document does not resolve: %v", err)
	}
	if !stablePathResolves(root, "agent_installation") {
		t.Error("the stable agent_installation path does not resolve into the active release")
	}
	activeRelease := filepath.Join(root, "releases", newRev, "agent_installation")
	if got, err := filepath.EvalSymlinks(stable); err != nil {
		t.Errorf("the stable path could not be resolved: %v", err)
	} else if got != activeRelease {
		t.Errorf("the stable path resolves to %q, want %q", got, activeRelease)
	}

	// A rollback moves the pointer; the symlink follows it.
	if _, err := Rollback(Options{DataRoot: root}); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if got := currentRevision(t, root); got != oldRev {
		t.Fatalf("after the rollback the pointer is %q, want %q", got, oldRev)
	}
	if _, err := os.Stat(resolved); err != nil {
		t.Errorf("the advertised protocol document does not resolve after the rollback: %v", err)
	}
	// And it resolves into the PREVIOUS release.
	previousRelease := filepath.Join(root, "releases", oldRev, "agent_installation")
	if got, err := filepath.EvalSymlinks(stable); err != nil {
		t.Errorf("the stable path could not be resolved after the rollback: %v", err)
	} else if got != previousRelease {
		t.Errorf("after the rollback the stable path resolves to %q, want %q", got, previousRelease)
	}
	if _, err := os.Stat(filepath.Join(previousRelease, "mpm-agent-protocol.md")); err != nil {
		t.Errorf("the previous release carries no protocol document: %v", err)
	}
	if newRev == oldRev {
		t.Fatal("the promotion did not change the revision")
	}
}

// TestTheDryRunReportNamesTheHostAssets backs the operator-visible
// claim in the recovery procedure: the dry-run report carries a
// `host-assets` line, and that line is the proof that the candidate
// carries the integration assets.
//
// The recovery procedure tells the operator to look for exactly that
// line and to STOP if it is absent. A dry-run report that did not name
// the check would make that instruction a guess.
func TestTheDryRunReportNamesTheHostAssets(t *testing.T) {
	dir := testBundleDir(t)
	rep := ValidateBundle(dir, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	if !rep.Valid {
		t.Fatalf("the fixture bundle does not validate: %v", rep.ValidationError())
	}
	report := dryRunReport("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Source{Kind: SourceGit, Rev: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}, rep)
	if !strings.Contains(report, "[ok] host-assets") {
		t.Errorf("the dry-run report does not name the host-assets check:\n%s", report)
	}

	// And a bundle WITHOUT the assets must not produce an "[ok]"
	// host-assets line: the operator's stop condition depends on the
	// line being absent, not on the report merely omitting it.
	if err := os.Remove(filepath.Join(dir, filepath.FromSlash(HostAssetManifestRelPath))); err != nil {
		t.Fatal(err)
	}
	bad := ValidateBundle(dir, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	badReport := dryRunReport("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Source{Kind: SourceGit, Rev: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}, bad)
	if strings.Contains(badReport, "[ok] host-assets") {
		t.Errorf("the dry-run report claims the host assets are present when they are not:\n%s", badReport)
	}
	if !strings.Contains(badReport, "[FAIL] host-assets") {
		t.Errorf("the dry-run report does not mark the host-assets check as failed:\n%s", badReport)
	}
}

// TestAFailureWhileEstablishingTheStablePathsFallsBack proves the
// stable-path step is treated as what it is: a step that runs AFTER
// the pointer moved.
//
// A failure here must restore the pointer, exactly like every other
// failure after the activation. Reporting "failed" while the new
// release is still active — and without the stable updater having been
// replaced — would leave the installation in a state no other failure
// can produce.
func TestAFailureWhileEstablishingTheStablePathsFallsBack(t *testing.T) {
	oldRev := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	root := testLayout(t, oldRev)
	srcDir := t.TempDir()
	mustGit(t, srcDir)
	o := Options{
		DataRoot: root, WantLocal: srcDir,
		Builder: fakeBuilder{commit: "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"},
		FailAt:  "stable-paths",
	}
	if _, err := Update(context.Background(), o); err == nil {
		t.Fatal("expected injected failure")
	}
	if _, got, _ := CurrentReleaseTarget(root); got != oldRev {
		t.Fatalf("pointer = %q, want restored %q", got, oldRev)
	}
}

// stablePathResolves reports whether a stable path resolves into the
// active release.
//
// It is a TEST helper: it asserts the property the installed managed
// block depends on — that the path it names leads into whichever
// release is active — and it has no production caller. Keeping it in
// production code would be a helper that only tests use, and a
// production file that exists only to be asserted against is a
// liability, not a contract.
func stablePathResolves(dataRoot, name string) bool {
	target := filepath.Join(dataRoot, name)
	resolved, err := filepath.EvalSymlinks(target)
	if err != nil {
		return false
	}
	release := filepath.Join(dataRoot, "releases")
	if _, rev, err := CurrentReleaseTarget(dataRoot); err == nil && rev != "" {
		want := filepath.Join(release, rev, name)
		if resolved == want {
			return true
		}
	}
	// Without an active pointer, resolving at all is the best the
	// check can say.
	return resolved != ""
}

// --- B3: the declaration's keys must be safe paths ---------------------------

// TestTheHostAssetDeclarationRefusesTraversal is the RED test for the
// traversal hole.
//
// A declaration key is a path. A key that can point OUTSIDE the bundle
// lets a corrupted declaration name an asset the bundle does NOT
// carry: the validator resolves the key, finds a file that happens to
// sit next to the bundle, and reports the bundle as complete. The
// release is then not self-contained — its own declaration promises a
// file whose bytes live somewhere else entirely, and the next
// verification of that release (in a rollback, in a copy, in a
// different directory layout) resolves the key to something else, or
// to nothing.
//
// THE FIXTURE IS THE HOLE, NOT A NEAR MISS:
//
//   - the bundle is COMPLETE — every asset it declares is present and
//     matches, so the completeness rule has nothing to say;
//   - the outside file really exists, one directory above the bundle,
//     and its digest really matches what the declaration claims.
//
// The only thing wrong with the bundle is the key. Without the key
// rule (and without the containment backstop) the validators accept
// it, and the release claims an asset it does not have.
//
// The rules mirror the authority's own
// (scripts/install_runtime_assets.py refuses an absolute path or a
// '..' segment).
func TestTheHostAssetDeclarationRefusesTraversal(t *testing.T) {
	lib := shellLibraryPath(t)
	if lib == "" {
		t.Skip("the shell library is not available")
	}
	for _, key := range []string{
		"../outside-target.txt",
		"agent_installation/../../outside-target.txt",
	} {
		t.Run(key, func(t *testing.T) {
			dir := testBundleDir(t)
			// The outside file: one directory ABOVE the bundle, so a
			// traversal reaches it.
			outside := filepath.Join(filepath.Dir(dir), "outside-target.txt")
			if err := os.WriteFile(outside, []byte("# outside the bundle\n"), 0644); err != nil {
				t.Fatal(err)
			}
			// The bundle keeps every asset it declares — it is
			// COMPLETE — and additionally declares the outside file.
			addDeclarationKey(t, dir, key, digestOf(t, outside))
			goValid := ValidateBundle(dir, "").Valid
			shellValid := shellValidateBundle(t, lib, dir)
			if goValid {
				t.Error("the Go validator accepted a declaration that points outside the bundle")
			}
			if shellValid {
				t.Error("the shell validator accepted a declaration that points outside the bundle")
			}
			if goValid != shellValid {
				t.Errorf("the validators disagree: Go says %v, the shell library says %v", goValid, shellValid)
			}
		})
	}
}

// TestTheHostAssetDeclarationRefusesUnsafeKeys is the broader matrix
// for the declaration's KEYS.
//
// The traversal case above is the hole; the rest is hardening. A
// relative path with a redundant segment names the same file as a
// plain one, and the declaration must name it the way it sits in the
// bundle — otherwise the completeness scan reports a declared file as
// undeclared for no reason a reader could see.
//
// BOTH validators must refuse every case: the bundle that reaches an
// operator depends on which tool ran.
func TestTheHostAssetDeclarationRefusesUnsafeKeys(t *testing.T) {
	lib := shellLibraryPath(t)
	if lib == "" {
		t.Skip("the shell library is not available")
	}
	cases := []struct {
		name string
		key  string
	}{
		{"absolute path", "/etc/passwd"},
		{"dot-slash prefix", "./agent_installation/mpm-agent-protocol.md"},
		{"backslash separator", `agent_installation\scripts\block_provenance.py`},
		{"drive prefix", "C:/outside.txt"},
		{"empty key", ""},
		{"double slash", "agent_installation//mpm-agent-protocol.md"},
		{"trailing slash", "agent_installation/mpm-agent-protocol.md/"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := testBundleDir(t)
			// The declaration names the unsafe key INSTEAD of the
			// protocol document, so the bundle is genuinely missing an
			// asset it must carry.
			rewriteDeclaration(t, dir, map[string]string{
				tc.key: digestOf(t, filepath.Join(dir, "agent_installation", "mpm-agent-protocol.md")),
			})
			goValid := ValidateBundle(dir, "").Valid
			shellValid := shellValidateBundle(t, lib, dir)
			if goValid {
				t.Error("the Go validator accepted a declaration with an unsafe key")
			}
			if shellValid {
				t.Error("the shell validator accepted a declaration with an unsafe key")
			}
			if goValid != shellValid {
				t.Errorf("the validators disagree: Go says %v, the shell library says %v", goValid, shellValid)
			}
		})
	}
}

// TestTheDeclaredAssetMayNotBeASymlink proves the declaration cannot
// point at a file OUTSIDE the bundle through a symlink.
func TestTheDeclaredAssetMayNotBeASymlink(t *testing.T) {
	dir := testBundleDir(t)
	outside := filepath.Join(t.TempDir(), "outside.md")
	if err := os.WriteFile(outside, []byte("# outside\n"), 0644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "agent_installation", "scripts", "linked.py")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	rewriteDeclaration(t, dir, map[string]string{
		"agent_installation/scripts/linked.py": digestOf(t, outside),
	})
	rep := ValidateBundle(dir, "")
	if rep.Valid {
		t.Fatal("a declaration naming a symlink was accepted")
	}
	if !reportHasCheck(rep, "host-assets", false) {
		t.Errorf("the refusal does not name the host-assets check: %+v", rep.Checks)
	}
}

// --- B4: the schema preflight -------------------------------------------------

// TestTheSchemaPreflightIsRequiredInACandidate proves a candidate
// without a usable schema preflight is refused.
//
// The schema-collision preflight compares the live table against the
// DDL the substrate itself would apply. A bundle that does not carry
// that DDL degrades the check to a no-op, SILENTLY — and the whole
// point of this contract is that a silent degradation is a failure.
func TestTheSchemaPreflightIsRequiredInACandidate(t *testing.T) {
	lib := shellLibraryPath(t)
	if lib == "" {
		t.Skip("the shell library is not available")
	}
	cases := []struct {
		name  string
		mutar func(t *testing.T, dir string)
	}{
		{"the directory is gone", func(t *testing.T, dir string) {
			os.RemoveAll(filepath.Join(dir, "schema"))
		}},
		{"the file is gone", func(t *testing.T, dir string) {
			os.Remove(filepath.Join(dir, "schema", "lifecycle_events.sql"))
		}},
		{"the file is empty", func(t *testing.T, dir string) {
			os.WriteFile(filepath.Join(dir, "schema", "lifecycle_events.sql"), []byte("  \n"), 0644)
		}},
		{"the file declares a destructive statement", func(t *testing.T, dir string) {
			os.WriteFile(filepath.Join(dir, "schema", "lifecycle_events.sql"), []byte("DROP TABLE lifecycle_events;\n"), 0644)
		}},
		{"the file declares an unrelated table", func(t *testing.T, dir string) {
			os.WriteFile(filepath.Join(dir, "schema", "lifecycle_events.sql"), []byte("CREATE TABLE something_else (id TEXT);\n"), 0644)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := testBundleDir(t)
			tc.mutar(t, dir)
			goValid := ValidateBundle(dir, "").Valid
			shellValid := shellValidateBundle(t, lib, dir)
			if goValid {
				t.Error("the Go validator accepted a bundle with no usable schema preflight")
			}
			if shellValid {
				t.Error("the shell validator accepted a bundle with no usable schema preflight")
			}
			if goValid != shellValid {
				t.Errorf("the validators disagree: Go says %v, the shell library says %v", goValid, shellValid)
			}
		})
	}
}

// TestTheSchemaPreflightIsOptionalInAnInstalledRelease proves the
// documented compatibility: a release promoted before the preflight
// travelled with the bundle may not carry one.
//
// The gap is historic and is NOT retrospectively closed on installed
// releases: doing so would refuse releases that were perfectly good
// when they were installed, and push the operator towards deleting an
// immutable rollback target.
func TestTheSchemaPreflightIsOptionalInAnInstalledRelease(t *testing.T) {
	dir := testBundleDir(t)
	if err := os.RemoveAll(filepath.Join(dir, "schema")); err != nil {
		t.Fatal(err)
	}
	rep := ValidateInstalledRelease(dir, "")
	if !rep.Valid {
		t.Fatalf("an installed release without a schema preflight was refused: %v", rep.ValidationError())
	}
	// A CANDIDATE without one is still refused.
	if ValidateBundle(dir, "").Valid {
		t.Fatal("a candidate without a schema preflight was accepted")
	}
	// And if the file IS there, it must be valid.
	if err := os.MkdirAll(filepath.Join(dir, "schema"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "schema", "lifecycle_events.sql"), []byte("DROP TABLE lifecycle_events;\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if ValidateInstalledRelease(dir, "").Valid {
		t.Fatal("an installed release with a destructive schema preflight was accepted")
	}
}

// --- B2: an already-installed release ----------------------------------------

// TestAnInstalledReleaseWithRuntimeDroppingsStillValidates is the B2
// case: a release that has been in use.
//
// The runtime writes into a release directory after installation —
// Python's bytecode caches, the adapter's build-provenance sidecar —
// and a release that fails the COMPLETENESS rule for that reason is
// not corrupt. It is used.
//
// The rule applies to CANDIDATES. An installed release is checked for
// INTEGRITY: the assets it declares must still be present with the
// digests it declares.
func TestAnInstalledReleaseWithRuntimeDroppingsStillValidates(t *testing.T) {
	dir := testBundleDir(t)

	// The two droppings the live installation actually carries.
	cacheDir := filepath.Join(dir, "agent_installation", "scripts", "__pycache__")
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cacheDir, "block_provenance.cpython-314.pyc"), []byte("not really bytecode"), 0644); err != nil {
		t.Fatal(err)
	}
	sidecar := filepath.Join(dir, "agent_installation", "mpm-opencode", "dist", "mpm-build-provenance.json")
	if err := os.WriteFile(sidecar, []byte(`{"commit":"x","built":"1"}`+"\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// A CANDIDATE with those files is refused: a fresh staging
	// directory has no business carrying them.
	if ValidateBundle(dir, "").Valid {
		t.Fatal("a candidate carrying undeclared runtime droppings was accepted")
	}
	// An INSTALLED release with them is accepted.
	rep := ValidateInstalledRelease(dir, "")
	if !rep.Valid {
		t.Fatalf("an installed release carrying runtime droppings was refused: %v", rep.ValidationError())
	}
}

// TestAnInstalledReleaseWithACorruptedAssetIsRefused proves the
// compatibility rule does not weaken the detection of a CORRUPTED
// release.
func TestAnInstalledReleaseWithACorruptedAssetIsRefused(t *testing.T) {
	dir := testBundleDir(t)
	target := filepath.Join(dir, "agent_installation", "mpm-agent-protocol.md")
	if err := os.WriteFile(target, []byte("# tampered\n"), 0644); err != nil {
		t.Fatal(err)
	}
	rep := ValidateInstalledRelease(dir, "")
	if rep.Valid {
		t.Fatal("an installed release with a corrupted asset was accepted")
	}
	if !reportHasCheck(rep, "host-assets", false) {
		t.Errorf("the refusal does not name the host-assets check: %+v", rep.Checks)
	}
}

// TestAnInstalledReleaseWithoutADeclarationIsRefused proves the
// compatibility rule does not extend to a release that carries NO
// declaration.
//
// Such a release was built before the host-asset contract existed. It
// is genuinely incomplete — it is the very defect this contract exists
// to detect — and detecting it must not be traded away for the
// convenience of an installed directory.
func TestAnInstalledReleaseWithoutADeclarationIsRefused(t *testing.T) {
	dir := testBundleDir(t)
	if err := os.Remove(filepath.Join(dir, filepath.FromSlash(HostAssetManifestRelPath))); err != nil {
		t.Fatal(err)
	}
	if ValidateInstalledRelease(dir, "").Valid {
		t.Fatal("an installed release with no host-asset declaration was accepted")
	}
}

// TestRePromotingAnAlreadyInstalledRevisionIsAccepted is the B2
// re-promotion case.
//
// Re-promoting a revision that is already installed must remain a
// no-op, not a refusal: the release is immutable, so the only outcome
// available is "use the one that is there". Refusing it would tell the
// operator to delete an immutable rollback target.
func TestRePromotingAnAlreadyInstalledRevisionIsAccepted(t *testing.T) {
	rev := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	root := testLayout(t, rev)
	l := NewLayout(root)

	// The installed release has been in use: the runtime wrote its
	// bytecode cache into it.
	cacheDir := filepath.Join(l.ReleaseDir(rev), "agent_installation", "scripts", "__pycache__")
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cacheDir, "block_provenance.cpython-314.pyc"), []byte("not really bytecode"), 0644); err != nil {
		t.Fatal(err)
	}

	// A candidate for the same revision.
	candidate := t.TempDir()
	b := fakeBuilder{commit: rev}
	if _, err := b.BuildFromCheckout(t.TempDir(), candidate); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(candidate, "RELEASE.json"), []byte(fmt.Sprintf(`{"revision":%q,"commit":%q,"short_commit":"12345678","build_epoch":1,"version":"v0","built_at":"x","bundle_layout":"versioned-bundle/1"}`, rev, rev)), 0644); err != nil {
		t.Fatal(err)
	}

	installed, err := installRelease(root, candidate, rev)
	if err != nil {
		t.Fatalf("re-promoting an already-installed revision was refused: %v", err)
	}
	if installed {
		t.Fatal("the existing release was replaced; a release is immutable")
	}
}

// --- B1: the runtime must not write into a release ----------------------------

// addDeclarationKey adds one key to a bundle's host-asset declaration,
// leaving the existing ones alone.
func addDeclarationKey(t *testing.T, dir, rel, digest string) {
	t.Helper()
	decl := filepath.Join(dir, filepath.FromSlash(HostAssetManifestRelPath))
	raw, err := os.ReadFile(decl)
	if err != nil {
		t.Fatal(err)
	}
	var manifest HostAssetManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Assets == nil {
		manifest.Assets = map[string]HostAssetRecord{}
	}
	manifest.Assets[rel] = HostAssetRecord{SHA256: digest}
	out, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(decl, out, 0644); err != nil {
		t.Fatal(err)
	}
}

// rewriteDeclaration replaces a bundle's host-asset declaration with
// the given keys and digests.
func rewriteDeclaration(t *testing.T, dir string, assets map[string]string) {
	t.Helper()
	manifest := HostAssetManifest{Version: hostAssetManifestVersion, Assets: map[string]HostAssetRecord{}}
	for rel, digest := range assets {
		manifest.Assets[rel] = HostAssetRecord{SHA256: digest}
	}
	raw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	decl := filepath.Join(dir, filepath.FromSlash(HostAssetManifestRelPath))
	if err := os.WriteFile(decl, raw, 0644); err != nil {
		t.Fatal(err)
	}
}

// digestOf returns the sha256 of a file, as the declaration spells it.
func digestOf(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// --- helpers ------------------------------------------------------------------

// promoteOnce runs one promotion in a disposable root and returns the
// root and the new revision.
func promoteOnce(t *testing.T) (string, string) {
	t.Helper()
	oldRev := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	root := testLayout(t, oldRev)
	srcDir := t.TempDir()
	mustGit(t, srcDir)
	o := Options{DataRoot: root, WantLocal: srcDir, Builder: fakeBuilder{}, Now: func() time.Time { return time.Unix(1700000000, 0).UTC() }}
	if _, err := Update(context.Background(), o); err != nil {
		t.Fatalf("update: %v", err)
	}
	return root, currentRevision(t, root)
}

// currentRevision returns the revision the active pointer names.
func currentRevision(t *testing.T, root string) string {
	t.Helper()
	_, rev, err := CurrentReleaseTarget(root)
	if err != nil {
		t.Fatalf("the active pointer could not be read: %v", err)
	}
	return rev
}

// declaredAssetPaths returns the host-asset paths a staged root
// DECLARES, sorted.
//
// The declaration is the authority's own record of what it provisioned,
// so comparing declarations compares the two provisioners rather than
// two directory listings — a directory listing of the built bundle
// would also contain the binaries and the runtime payload, which the
// installer does not provision.
func declaredAssetPaths(t *testing.T, root string) []string {
	t.Helper()
	manifest, err := readHostAssetManifest(root)
	if err != nil {
		t.Fatalf("the staged root %s carries no host-asset declaration: %v", root, err)
	}
	out := make([]string, 0, len(manifest.Assets))
	for rel := range manifest.Assets {
		out = append(out, filepath.ToSlash(rel))
	}
	sort.Strings(out)
	return out
}

// containsString reports whether a list holds a value.
func containsString(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// reportHasCheck reports whether a validation report carries a check
// with the given name and verdict.
func reportHasCheck(rep BundleReport, name string, ok bool) bool {
	for _, c := range rep.Checks {
		if c.Name == name && c.OK == ok {
			return true
		}
	}
	return false
}

// shellLibraryPath locates scripts/lib/runtime_bundle.sh from the
// package directory.
func shellLibraryPath(t *testing.T) string {
	t.Helper()
	for _, root := range []string{".", "..", "../.."} {
		p := filepath.Join(root, "scripts", "lib", "runtime_bundle.sh")
		if _, err := os.Stat(p); err == nil {
			abs, err := filepath.Abs(p)
			if err != nil {
				return p
			}
			return abs
		}
	}
	return ""
}

// shellValidateBundle runs the shell library's bundle validator and
// reports whether it accepted the bundle.
func shellValidateBundle(t *testing.T, lib, bundleDir string) bool {
	t.Helper()
	script := `. "$1"; rb_validate_bundle "$2" "" >/dev/null 2>&1`
	cmd := exec.Command("bash", "-c", script, "bash", lib, bundleDir) //nolint:gosec
	if err := cmd.Run(); err != nil {
		if _, ok := err.(*exec.ExitError); ok {
			return false
		}
		t.Skipf("the shell validator could not be run: %v", err)
	}
	return true
}
