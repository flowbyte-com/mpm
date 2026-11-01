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

// ValidateBundle proves a candidate bundle before it is exposed.
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
// Plus two updater-specific checks:
//
//  6. the OpenCode runtime payload is present (fail-closed: a bundle
//     without a validated payload registers a plugin that loads
//     nothing — the exact silent failure the staging validator exists
//     to prevent);
//  7. mpm-update is present (reported, not required — see above).
//
// Nothing outside the candidate directory is touched. A refusal here
// costs nothing: the running installation is exactly as it was.
func ValidateBundle(bundleDir, expectedRev string) BundleReport {
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
	if hasPluginPayload(bundleDir) {
		rep.HasPayload = true
		check("payload", "the OpenCode runtime payload is present", true)
	} else {
		check("payload", "the bundle carries no OpenCode payload — refusing a bundle that would register an empty plugin", false)
	}

	// 7. The standalone updater (reported, not required).
	if fi, err := os.Stat(filepath.Join(bundleDir, "bin", UpdaterBinaryName)); err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0111 != 0 {
		rep.HasUpdater = true
		check("updater", "mpm-update is present", true)
	} else {
		check("updater", "mpm-update is absent (a release built before the updater existed; reported, not refused)", true)
	}

	return rep
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

func hasPluginPayload(bundleDir string) bool {
	server := filepath.Join(bundleDir, "agent_installation", "mpm-opencode", "server.js")
	index := filepath.Join(bundleDir, "agent_installation", "mpm-opencode", "dist", "index.v2.js")
	sfi, serr := os.Stat(server)
	ifi, ierr := os.Stat(index)
	return serr == nil && ierr == nil && !sfi.IsDir() && !ifi.IsDir() && sfi.Size() > 0 && ifi.Size() > 0
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
