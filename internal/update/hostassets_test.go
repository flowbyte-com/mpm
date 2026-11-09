package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	if problems := hostAssetProblems(dst); len(problems) > 0 {
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
