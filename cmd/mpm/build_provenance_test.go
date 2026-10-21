// build_provenance_test.go — the build-provenance contract across all
// five MPM binaries.
//
// The provenance (product version, source commit, dirty flag, build
// type) is stamped at LINK time into internal/buildinfo, the package
// every MPM binary links. Three properties are load-bearing:
//
//   - all five binaries carry IDENTICAL provenance, so an operator
//     never sees two halves of one installation disagreeing about what
//     version they are;
//   - each binary names ITSELF, so `mpm --version` and
//     `mpm-mcp --version` can be told apart;
//   - asking a daemon for its version does not start the daemon: no
//     workspace, no database, no lock file.
//
// The stamp used here is the canonical one the Makefile applies, so
// this also covers "the version is correctly embedded in binaries
// built using the same process as install.sh" — install.sh reaches
// its binaries through `make build`.
package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/flowbyte-com/mpm/internal/testenv"
)

// provenanceBinaries is the canonical set of MPM executables and the
// name each one reports for itself.
var provenanceBinaries = []struct {
	pkg  string
	name string
}{
	{"./cmd/mpm", "mpm"},
	{"./cmd/mpm-mcp", "mpm-mcp"},
	{"./cmd/mpm-scheduler", "mpm-scheduler"},
	{"./cmd/mpm-critic", "mpm-critic"},
	{"./cmd/mpm-telemetry", "mpm-telemetry"},
}

// buildProvenanceBinary builds one MPM binary from the repository root
// with the given linker stamp.
func buildProvenanceBinary(t *testing.T, pkg, outPath, ldflags string) {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}

	args := []string{"build", "-tags", "fts5"}
	if ldflags != "" {
		args = append(args, "-ldflags", ldflags)
	}
	args = append(args, "-o", outPath, pkg)

	cmd := exec.Command("go", args...)
	// The test runs in cmd/mpm; the module root is two levels up.
	cmd.Dir = "../.."
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build %s: %v\n%s", pkg, err, out)
	}
}

// runProvenanceBinary runs a built binary with the given arguments and
// returns stdout, stderr and the exit code.
//
// The environment comes from testenv.WithExtra, which pins
// MPM_WORKSPACE and HOME to temporary directories. A child that
// inherited the developer's shell environment could resolve the
// operator's live installation; the repository's subprocess-isolation
// guard (internal/testenv) exists for exactly that reason. `env`
// entries are appended last, so a caller can deliberately point
// MPM_WORKSPACE somewhere else.
func runProvenanceBinary(t *testing.T, bin string, env []string, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = testenv.WithExtra(t, env...)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	exit := 0
	if err := cmd.Run(); err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("run %s %v: %v", bin, args, err)
		}
		exit = ee.ExitCode()
	}
	return stdout.String(), stderr.String(), exit
}

func TestBuildProvenance_AllFiveBinariesCarryIdenticalProvenance(t *testing.T) {
	dir := t.TempDir()

	for _, b := range provenanceBinaries {
		b := b
		t.Run(b.name, func(t *testing.T) {
			bin := filepath.Join(dir, b.name)
			buildProvenanceBinary(t, b.pkg, bin, stampedLDFlags)

			stdout, stderr, exit := runProvenanceBinary(t, bin, nil, "--version")
			if exit != 0 {
				t.Fatalf("%s --version exited %d\nstdout=%s\nstderr=%s", b.name, exit, stdout, stderr)
			}

			for _, want := range []string{
				stampedVersion,
				stampedCommit,
				stampedShortCommit,
				b.name,
			} {
				if !strings.Contains(stdout, want) {
					t.Errorf("%s --version does not report %q:\n%s", b.name, want, stdout)
				}
			}
		})
	}
}

func TestBuildProvenance_TheVersionSurfaceDoesNotStartTheDaemon(t *testing.T) {
	dir := t.TempDir()

	for _, b := range provenanceBinaries {
		b := b
		t.Run(b.name, func(t *testing.T) {
			bin := filepath.Join(dir, b.name)
			buildProvenanceBinary(t, b.pkg, bin, stampedLDFlags)

			// A workspace that does not exist. A daemon that boots
			// would materialise it; the version surface must not.
			workspace := filepath.Join(t.TempDir(), "must-not-be-created")

			stdout, stderr, exit := runProvenanceBinary(t, bin,
				[]string{"MPM_WORKSPACE=" + workspace}, "--version")
			if exit != 0 {
				t.Fatalf("%s --version exited %d\nstdout=%s\nstderr=%s", b.name, exit, stdout, stderr)
			}

			if _, err := os.Stat(workspace); err == nil {
				t.Errorf("%s --version created %s; asking a binary for its version must not boot it", b.name, workspace)
			}
		})
	}
}

func TestBuildProvenance_UnstampedBinariesReportVisiblePlaceholders(t *testing.T) {
	dir := t.TempDir()

	for _, b := range provenanceBinaries {
		b := b
		t.Run(b.name, func(t *testing.T) {
			bin := filepath.Join(dir, b.name)
			buildProvenanceBinary(t, b.pkg, bin, "")

			stdout, stderr, exit := runProvenanceBinary(t, bin, nil, "--version")
			if exit != 0 {
				t.Fatalf("%s --version exited %d\nstdout=%s\nstderr=%s", b.name, exit, stdout, stderr)
			}

			// An unstamped build must be visible as such, not
			// silently indistinguishable from a real one.
			for _, want := range []string{"dev", "unknown"} {
				if !strings.Contains(stdout, want) {
					t.Errorf("%s --version does not report the %q placeholder:\n%s", b.name, want, stdout)
				}
			}
		})
	}
}

// TestBuildProvenance_TheCanonicalScriptStampIsEmbeddedInTheBinary is
// the "same process as install.sh" check, done for real.
//
// Every other test in this file stamps the binaries with a
// HAND-WRITTEN ldflags string. That proves the linker mechanism works,
// but it does not prove that the CONVENTION — the one install.sh and
// `make build` actually use — produces a binary that reports the right
// version.
//
// This test closes that gap: it asks
// scripts/build-provenance.sh for the ldflags payload (the exact
// string the Makefile passes to `go build`, and therefore the exact
// stamp install.sh's binaries carry), builds a binary with it, and
// asserts the binary reports the VERSION file's version and this
// checkout's HEAD commit.
//
// The consequence it protects: if the convention ever drifts from the
// mechanism — a renamed package path, a typo in a variable name, a
// VERSION file that stops being read — the binary would still COMPILE
// (an -X flag naming a symbol that does not exist is silently ignored)
// and would report the "dev"/"unknown" placeholders. Only this test
// notices.
func TestBuildProvenance_TheCanonicalScriptStampIsEmbeddedInTheBinary(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}

	repoRoot := "../.."

	// The canonical payload, exactly as the Makefile consumes it.
	script := filepath.Join(repoRoot, "scripts", "build-provenance.sh")
	out, err := exec.Command("bash", script, "ldflags").Output()
	if err != nil {
		t.Fatalf("build-provenance.sh ldflags: %v", err)
	}
	canonicalLDFlags := strings.TrimSpace(string(out))
	if canonicalLDFlags == "" {
		t.Fatal("build-provenance.sh ldflags produced an empty payload")
	}

	// The version the payload must carry: the VERSION file's content.
	versionOut, err := exec.Command("bash", script, "version").Output()
	if err != nil {
		t.Fatalf("build-provenance.sh version: %v", err)
	}
	wantVersion := strings.TrimSpace(string(versionOut))

	// The commit the payload must carry: this checkout's HEAD.
	commitOut, err := exec.Command("git", "rev-parse", "HEAD").Output()
	if err != nil {
		t.Skipf("not a git checkout: %v", err)
	}
	wantCommit := strings.TrimSpace(string(commitOut))

	bin := filepath.Join(t.TempDir(), "mpm")
	buildProvenanceBinary(t, "./cmd/mpm", bin, canonicalLDFlags)

	stdout, stderr, exit := runProvenanceBinary(t, bin, nil, "--version")
	if exit != 0 {
		t.Fatalf("mpm --version exited %d\nstdout=%s\nstderr=%s", exit, stdout, stderr)
	}

	if !strings.Contains(stdout, wantVersion) {
		t.Errorf("the canonically stamped binary does not report the VERSION file's version %q:\n%s", wantVersion, stdout)
	}
	if !strings.Contains(stdout, wantCommit) {
		t.Errorf("the canonically stamped binary does not report this checkout's HEAD commit %q:\n%s", wantCommit, stdout)
	}
}
