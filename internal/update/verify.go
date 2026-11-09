package update

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// The canonical promotion order. RB_BINARIES in
// scripts/lib/runtime_bundle.sh defines the same list; the two must
// agree on what a complete runtime is.
//
// mpm-update is the sixth binary. It is deliberately NOT in this list:
// the five are the runtime the validator was written for, and the
// installer promotes exactly these five (BT_BINARIES). The updater
// validates mpm-update separately (see ValidateBundle) so a release
// built before mpm-update existed still validates — while a release
// that ships no updater is reported honestly rather than refused.
var runtimeBinaries = []string{"mpm", "mpm-scheduler", "mpm-critic", "mpm-mcp", "mpm-telemetry"}

// UpdaterBinaryName is the standalone recovery binary.
const UpdaterBinaryName = "mpm-update"

// forbiddenInBundle mirrors RB_FORBIDDEN_IN_BUNDLE in
// scripts/lib/runtime_bundle.sh. A bundle that carries operator state
// would make every version switch a chance to replace it, so the bundle
// is refused before it is exposed.
//
// REUSE CONTRACT: this list is a copy, not an import, because the shell
// library cannot be imported from Go. The parity test
// TestBundleValidationMatchesShellLibrary fails if either list names a
// path the other does not, so a one-sided edit is caught instead of
// silently diverging.
var forbiddenInBundle = []string{
	"src/db", "src/backups", "backups", "mpm_config.json",
	"mode", "persona", "drills", "active.json",
}

// BundleReport is the outcome of validating a candidate bundle.
type BundleReport struct {
	// Valid is true when every check passed.
	Valid bool
	// Release is the bundle's parsed identity.
	Release Release
	// HasUpdater reports whether the bundle ships mpm-update.
	HasUpdater bool
	// HasPayload reports whether the OpenCode runtime payload is
	// present and loadable.
	HasPayload bool
	// Checks is the per-check breakdown, in run order.
	Checks []BundleCheck
}

// BundleCheck is one validation step.
type BundleCheck struct {
	Name   string
	OK     bool
	Detail string
}

// ValidateBundle proves a CANDIDATE bundle before it is exposed.
//
// The five checks are the same five rb_validate_bundle performs, in the
// same order, with the same refusals:
//
//  1. the five binaries are present, regular and executable;
//  2. the bundle is Git-free;
//  3. the bundle carries no persistent state;
//  4. RELEASE.json is readable and names the expected revision;
//  5. the binaries report the revision they claim (anti-mixture).
//
// Plus the updater-specific checks:
//
//  6. the OpenCode runtime payload is present (fail-closed: a bundle
//     without a validated payload registers a plugin that loads
//     nothing — the exact silent failure the staging validator exists
//     to prevent);
//  7. mpm-update is present (reported, not required — see above);
//  8. the release-scoped host assets are declared and carried;
//  9. the schema preflight is present and declares the table it exists
//     to protect.
//
// Nothing outside the candidate directory is touched. A refusal here
// costs nothing: the running installation is exactly as it was.
func ValidateBundle(bundleDir, expectedRev string) BundleReport {
	return validateBundle(bundleDir, expectedRev, ScopeCandidate)
}

// ValidateInstalledRelease proves a bundle that was ALREADY
// INSTALLED.
//
// It asks a different question from ValidateBundle, and the difference
// matters:
//
//   - a CANDIDATE is checked for CONSTRUCTION: every file it carries
//     must be accounted for, and the schema preflight must be present.
//     A candidate is assembled in a fresh staging directory, so an
//     undeclared file there is a file nobody provisioned.
//
//   - an INSTALLED release is checked for INTEGRITY: the assets it
//     declares must still be present with the digests it declares.
//     A release directory is not immutable in practice — the runtime
//     writes into it (Python's bytecode caches, the adapter's
//     build-provenance sidecar) — so the construction rules would
//     refuse a release for being used, and refusing it would push the
//     operator towards deleting an immutable rollback target.
//
// The host-asset DECLARATION is still required in both scopes: a
// release that carries no declaration was built before the host-asset
// contract existed, and it is genuinely incomplete — that is the
// defect this contract exists to detect, and detecting it must not be
// traded away for the convenience of an installed directory.
func ValidateInstalledRelease(bundleDir, expectedRev string) BundleReport {
	return validateBundle(bundleDir, expectedRev, ScopeInstalled)
}

func validateBundle(bundleDir, expectedRev string, scope BundleScope) BundleReport {
	rep := BundleReport{}
	check := func(name, detail string, ok bool) {
		rep.Checks = append(rep.Checks, BundleCheck{Name: name, OK: ok, Detail: detail})
		if !ok {
			rep.Valid = false
		}
	}
	rep.Valid = true

	if fi, err := os.Stat(bundleDir); err != nil || !fi.IsDir() {
		check("directory", "bundle directory does not exist", false)
		return rep
	}

	// 1. The five binaries.
	for _, bin := range runtimeBinaries {
		p := filepath.Join(bundleDir, "bin", bin)
		fi, err := os.Stat(p)
		switch {
		case err != nil:
			check("bin/"+bin, "bundle is missing bin/"+bin, false)
		case !fi.Mode().IsRegular():
			check("bin/"+bin, "bundle's bin/"+bin+" is not a regular file", false)
		case fi.Mode().Perm()&0111 == 0:
			check("bin/"+bin, "bundle's bin/"+bin+" is not executable", false)
		default:
			check("bin/"+bin, "present and executable", true)
		}
	}

	// 2. No Git metadata.
	if hasGit(bundleDir) {
		check("git-free", "the bundle carries a .git directory — a runtime bundle must be Git-free", false)
	} else {
		check("git-free", "no .git anywhere inside the bundle", true)
	}

	// 3. No state.
	stateOK := true
	for _, rel := range forbiddenInBundle {
		if _, err := os.Stat(filepath.Join(bundleDir, rel)); err == nil {
			check("no-state", fmt.Sprintf("the bundle carries %q — a runtime bundle must contain no persistent state", rel), false)
			stateOK = false
		}
	}
	if stateOK {
		check("no-state", "the bundle carries no persistent state", true)
	}

	// 4. The identity.
	rel, err := ReadRelease(bundleDir)
	if err != nil {
		check("identity", err.Error(), false)
		return rep
	}
	rep.Release = rel
	if expectedRev != "" && rel.Revision != expectedRev {
		check("identity", fmt.Sprintf("bundle's RELEASE.json names revision %q but %q was expected", rel.Revision, expectedRev), false)
	} else {
		check("identity", fmt.Sprintf("revision %s commit %s", rel.Revision, ShortSHA(rel.Commit)), true)
	}

	// 5. Anti-mixture: the binaries report the revision they claim.
	if rel.Commit != "" && !IsPlaceholderCommit(rel.Commit) {
		reported := mpmReportedCommit(filepath.Join(bundleDir, "bin", "mpm"))
		switch {
		case reported == "":
			check("anti-mixture", "the bundle's mpm binary did not report a build commit", false)
		case reported != rel.Commit:
			check("anti-mixture", fmt.Sprintf("the bundle's mpm binary reports commit %q but RELEASE.json says %q (a mixture of two builds)", ShortSHA(reported), ShortSHA(rel.Commit)), false)
		default:
			check("anti-mixture", "the binaries report the revision they claim", true)
		}
	} else {
		check("anti-mixture", "no embedded commit to cross-check (placeholder revision)", true)
	}

	// 6. The OpenCode runtime payload (fail-closed).
	if ok, detail := validateBundlePayload(bundleDir); ok {
		rep.HasPayload = true
		check("payload", detail, true)
	} else {
		check("payload", detail, false)
	}

	// 7. The standalone updater (reported, not required).
	if fi, err := os.Stat(filepath.Join(bundleDir, "bin", UpdaterBinaryName)); err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0111 != 0 {
		rep.HasUpdater = true
		check("updater", "mpm-update is present", true)
	} else {
		check("updater", "mpm-update is absent (a release built before the updater existed; reported, not refused)", true)
	}

	// 8. The release-scoped host assets.
	//
	// FAIL-CLOSED, and deliberately so. The installed managed block
	// advertises ~/.mpm/agent_installation/MPM_AGENT_INTEGRATION_SNIPPETS.md
	// and ~/.mpm/agent_installation/mpm-agent-protocol.md, and the
	// per-adapter installers import the shared scripts under
	// agent_installation/scripts/ by path. A bundle that omits them
	// installs an installation whose own instructions point at files
	// that do not exist — and the omission is invisible until an agent
	// follows the pointer.
	//
	// The bundle declares its assets (config/runtime-assets.json) and
	// the check is against that declaration, so the validator needs no
	// source tree and the declaration is produced by the authority that
	// provisions the assets, not by a second list that could drift.
	if problems := hostAssetProblems(bundleDir, scope); len(problems) > 0 {
		for _, p := range problems {
			check("host-assets", p, false)
		}
	} else {
		check("host-assets", "the bundle declares and carries its host integration assets", true)
	}

	// 9. The schema preflight.
	//
	// REQUIRED for a candidate: a bundle with no preflight installs a
	// runtime whose schema-collision check degrades to a no-op, and
	// that degradation is silent.
	//
	// OPTIONAL for an installed release: a release promoted before the
	// preflight travelled with the bundle may not carry one. That is a
	// documented historic gap, not a corruption. If the file IS there,
	// it must be valid — an empty file or a file that does not declare
	// the table is refused in both scopes.
	if problems := schemaPreflightProblems(bundleDir, scope.Strict()); len(problems) > 0 {
		for _, p := range problems {
			check("schema-preflight", p, false)
		}
	} else {
		detail := "the bundle carries the substrate's lifecycle_events DDL for the schema-collision preflight"
		if !scope.Strict() {
			detail = "the bundle carries no schema preflight (a release promoted before the preflight travelled with the bundle; reported, not refused)"
		}
		check("schema-preflight", detail, true)
	}

	return rep
}

// failedCheckNames renders the names of the checks a report failed, so
// a refusal names WHICH property the bundle lacks.
func failedCheckNames(rep BundleReport) string {
	var failed []string
	seen := map[string]bool{}
	for _, c := range rep.Checks {
		if c.OK || seen[c.Name] {
			continue
		}
		seen[c.Name] = true
		failed = append(failed, c.Name)
	}
	if len(failed) == 0 {
		return "no named check failed"
	}
	return strings.Join(failed, ", ")
}

// ValidationError renders a failed report as one operator-readable
// refusal. Every failed check is listed; nothing is summarised away.
func (r BundleReport) ValidationError() error {
	var failed []string
	for _, c := range r.Checks {
		if !c.OK {
			failed = append(failed, c.Name+": "+c.Detail)
		}
	}
	return fmt.Errorf("the candidate bundle failed validation:\n  - %s", strings.Join(failed, "\n  - "))
}

func hasGit(bundleDir string) bool {
	if _, err := os.Stat(filepath.Join(bundleDir, ".git")); err == nil {
		return true
	}
	found := false
	filepath.Walk(bundleDir, func(p string, fi os.FileInfo, err error) error { //nolint:errcheck
		if err != nil {
			return nil
		}
		if fi.Name() == ".git" {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	return found
}

// validateBundlePayload proves the bundle's runtime package payload is
// present and loadable.
//
// PRIMARY RULE — the bundle carries its own declaration.
//
// The canonical manifest (scripts/runtime_payloads.json) is copied into
// the bundle when the payload is staged, so the check is data-driven:
// it names no adapter and no file list, and it depends on neither the
// source tree the bundle was built from nor the revision the updater
// binary was built from. A stale updater checking a newer bundle sees
// the newer bundle's own declaration.
//
// SECONDARY RULE — the package must actually load.
//
// Every relative import inside a staged package must resolve to a file
// in that package. The host registers a DIRECTORY, so a package whose
// module graph is broken loads NOTHING, silently: the registration
// succeeds and the plugin registry stays empty.
//
// FALLBACK — a bundle assembled before the declaration travelled with
// the bundle declares nothing. Such a bundle is checked STRUCTURALLY:
// each runtime package must carry the legacy OpenCode minimum (a
// nonempty server.js and dist/index.v2.js) and its module graph must
// resolve. That is weaker than the declaration (it cannot know which
// other files were required) and never weaker than the check it
// replaces.
func validateBundlePayload(bundleDir string) (bool, string) {
	manifestPath := filepath.Join(bundleDir, filepath.FromSlash(bundlePayloadManifestRelPath))
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		// The structural fallback is ONLY for bundles assembled
		// before the declaration travelled with them. A manifest
		// that cannot be read for any other reason — permissions,
		// a directory in its place, I/O failure — is a validation
		// failure, never a reason to check something else instead.
		if !os.IsNotExist(err) {
			return false, fmt.Sprintf("the bundle's payload declaration at %s could not be read: %v", manifestPath, err)
		}
		return validateBundlePayloadStructurally(bundleDir)
	}
	m, err := parsePayloadManifest(raw, manifestPath)
	if err != nil {
		return false, err.Error()
	}
	files := 0
	for _, p := range m.Payloads {
		pkgDir := filepath.Join(bundleDir, "agent_installation", p.Adapter)
		if err := validatePayloadPackage(pkgDir, p); err != nil {
			return false, err.Error()
		}
		files += len(p.Files)
	}
	return true, fmt.Sprintf("the runtime package payload is present and loadable (%d package(s), %d declared file(s))", len(m.Payloads), files)
}

// validateBundlePayloadStructurally checks a bundle that carries no
// declaration.
//
// It finds the runtime packages by their shape rather than by a file
// list — a package is a directory under agent_installation/ that
// declares itself with a package.json — and proves each one can load.
func validateBundlePayloadStructurally(bundleDir string) (bool, string) {
	aiDir := filepath.Join(bundleDir, "agent_installation")
	entries, err := os.ReadDir(aiDir)
	if err != nil {
		return false, "the bundle carries no OpenCode payload — refusing a bundle that would register an empty plugin"
	}
	packages := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pkgDir := filepath.Join(aiDir, e.Name())
		if _, err := os.Stat(filepath.Join(pkgDir, "package.json")); err != nil {
			continue // not a runtime package
		}
		packages++
		if err := validatePackageShape(pkgDir, e.Name()); err != nil {
			return false, err.Error()
		}
	}
	if packages == 0 {
		return false, "the bundle carries no OpenCode payload — refusing a bundle that would register an empty plugin"
	}
	return true, fmt.Sprintf("the runtime package payload is present and loadable (%d package(s); the bundle carries no declaration, so the packages were checked structurally)", packages)
}

// validatePackageShape proves a runtime package has a loader-resolvable
// root entry and a resolvable module graph.
//
// The root entry is what a directory plugin is resolved BY: OpenCode
// tries "server" then "index" at the package root, and a directory
// registered without one is dropped without a warning.
//
// LEGACY MINIMUM. A legacy OpenCode package carries its entrypoint at
// server.js and its compiled entry at dist/index.v2.js, both nonempty.
// That was the whole pre-manifest predicate, and the entry and
// module-graph checks below run IN ADDITION to it, not instead of it:
// a server.js with no imports and no compiled entry is a package that
// cannot load, and no graph check can see that.
func validatePackageShape(pkgDir, name string) error {
	for _, required := range []string{"server.js", filepath.Join("dist", "index.v2.js")} {
		full := filepath.Join(pkgDir, required)
		fi, err := os.Stat(full)
		switch {
		case err != nil:
			return fmt.Errorf("the runtime package %q is missing the required %s — a legacy OpenCode package carries a nonempty server.js and dist/index.v2.js", name, filepath.ToSlash(required))
		case fi.IsDir() || fi.Size() == 0:
			return fmt.Errorf("the runtime package %q carries an empty %s — a legacy OpenCode package carries a nonempty server.js and dist/index.v2.js", name, filepath.ToSlash(required))
		}
	}
	entry := ""
	entries, err := os.ReadDir(pkgDir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		base := e.Name()
		stem := strings.TrimSuffix(base, filepath.Ext(base))
		if stem != "server" && stem != "index" {
			continue
		}
		if fi, err := os.Stat(filepath.Join(pkgDir, base)); err == nil && fi.Mode().IsRegular() && fi.Size() > 0 {
			entry = base
			break
		}
	}
	if entry == "" {
		return fmt.Errorf("the runtime package %q has no loader-resolvable root entry (a server.* or index.* module at the package root) — the host would drop it silently", name)
	}
	unresolved, err := unresolvedRelativeImports(pkgDir)
	if err != nil {
		return err
	}
	if len(unresolved) > 0 {
		return fmt.Errorf("the runtime package %q has unresolved internal imports: %s", name, strings.Join(unresolved, ", "))
	}
	return nil
}

// mpmReportedCommit runs the candidate mpm --version and extracts the
// commit line. The candidate runs against NO state: --version performs
// no runtime validation and opens no database (see runVersionCommand),
// so executing it cannot migrate anything.
func mpmReportedCommit(mpmPath string) string {
	out, err := exec.Command(mpmPath, "--version").Output() //nolint:gosec
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "commit:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "commit:"))
		}
	}
	return ""
}
