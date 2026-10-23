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
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
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

// The binaries these tests run are built ONCE per test binary and shared.
//
// Building a Go binary is the dominant cost of these tests, and the
// cmd/mpm race suite already runs close to its wall-clock budget in CI.
// The five binaries are therefore built once in each of their two forms
// — stamped and unstamped — and reused by every test that needs them.
//
// Because the binaries have to outlive the test that built them, they
// cannot live in a t.TempDir() and they cannot be removed by a
// per-test cleanup. They live in a package-level temporary directory
// that the package's TestMain removes once every test has finished —
// the same pattern the canonical-provenance build directory already
// uses (see build_identity_test.go).
//
// That directory used to survive every run of this package, leaving
// ~148 MB behind each time.
var (
	provenanceBuildOnce sync.Once
	provenanceBuildErr  error
	provenanceStamped   map[string]string
	provenanceUnstamped map[string]string

	// provenanceBuildDir holds the shared stamped and unstamped
	// binaries.
	//
	// It is recorded the moment the directory is created — BEFORE any
	// build — so that a build that fails halfway still leaves a
	// directory the package teardown can remove. Recording it only
	// after a successful build would leak precisely in the case that
	// needs cleaning up most.
	provenanceBuildDir string
)

// newProvenanceBuildDir creates the shared build directory and records
// it, so the package teardown can remove it.
//
// The recording happens inside this function, before the caller builds
// anything, and that ordering is the contract.
func newProvenanceBuildDir() (string, error) {
	dir, err := os.MkdirTemp("", "mpm-provenance-bin-")
	if err != nil {
		return "", err
	}
	provenanceBuildDir = dir
	return dir, nil
}

// removeProvenanceBuildDir removes the shared build directory.
//
// It is called from the package's TestMain, after the tests have run.
// A removal failure is reported rather than swallowed: the directory
// holds ~148 MB, and a silent failure turns into a slow exhaustion of
// the temporary filesystem.
func removeProvenanceBuildDir() error {
	return removeBuildDir(provenanceBuildDir)
}

// buildProvenanceBinary builds one MPM binary from the repository root
// with the given linker stamp.
func buildProvenanceBinary(pkg, outPath, ldflags string) error {
	args := []string{"build", "-tags", "fts5"}
	if ldflags != "" {
		args = append(args, "-ldflags", ldflags)
	}
	args = append(args, "-o", outPath, pkg)

	cmd := exec.Command("go", args...)
	// The test runs in cmd/mpm; the module root is two levels up.
	cmd.Dir = "../.."
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("go build %s: %v\n%s", pkg, err, out)
	}
	return nil
}

// buildProvenanceBinaries builds every MPM binary in its stamped and
// unstamped forms, once each, and returns the paths keyed by the name
// each binary reports for itself.
func buildProvenanceBinaries(t *testing.T) (stamped, unstamped map[string]string) {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}

	provenanceBuildOnce.Do(func() {
		dir, err := newProvenanceBuildDir()
		if err != nil {
			provenanceBuildErr = err
			return
		}
		stamped = map[string]string{}
		unstamped = map[string]string{}
		for _, b := range provenanceBinaries {
			bin := filepath.Join(dir, b.name)
			if err := buildProvenanceBinary(b.pkg, bin, stampedLDFlags); err != nil {
				provenanceBuildErr = err
				return
			}
			stamped[b.name] = bin
		}
		for _, b := range provenanceBinaries {
			bin := filepath.Join(dir, b.name+"-unstamped")
			if err := buildProvenanceBinary(b.pkg, bin, ""); err != nil {
				provenanceBuildErr = err
				return
			}
			unstamped[b.name] = bin
		}
		provenanceStamped = stamped
		provenanceUnstamped = unstamped
	})

	if provenanceBuildErr != nil {
		t.Fatalf("build the MPM binaries: %v", provenanceBuildErr)
	}
	return provenanceStamped, provenanceUnstamped
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
	stamped, _ := buildProvenanceBinaries(t)

	for _, b := range provenanceBinaries {
		b := b
		t.Run(b.name, func(t *testing.T) {
			bin := stamped[b.name]

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
	stamped, _ := buildProvenanceBinaries(t)

	for _, b := range provenanceBinaries {
		b := b
		t.Run(b.name, func(t *testing.T) {
			bin := stamped[b.name]

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
	_, unstamped := buildProvenanceBinaries(t)

	for _, b := range provenanceBinaries {
		b := b
		t.Run(b.name, func(t *testing.T) {
			bin := unstamped[b.name]

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
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
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

	// This binary carries a DIFFERENT stamp from the shared ones (the
	// canonical payload rather than the hand-written one), so it needs
	// its own build. It is built once and shared with the identity
	// tests.
	bin := buildCanonicalProvenanceBinary(t).Path

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

// TestBuildProvenance_AnExportedSourceTreeReportsUnknownProvenance is
// the end-to-end proof for the "no Git metadata" case.
//
// It builds a REAL binary with the exact ldflags the canonical script
// produces for an MPM source EXPORT — the sources with no .git
// directory — and asserts the binary reports what it can establish and
// nothing it cannot: the product version (from the VERSION file), and
// "unknown" for every Git-derived fact.
//
// The export is nested inside an UNRELATED repository, because that is
// where the mistake hides: Git resolves a repository by searching
// parent directories, so without an explicit check the export would
// inherit the enclosing project's commit — a commit that says nothing
// about MPM, stamped into an MPM binary where an operator could not
// tell it was wrong.
func TestBuildProvenance_AnExportedSourceTreeReportsUnknownProvenance(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	// An unrelated repository, with a commit of its own.
	outer := t.TempDir()
	runGit(t, outer, "init", "-q", "-b", "main")
	runGit(t, outer, "-c", "user.email=provenance@example.invalid",
		"-c", "user.name=Provenance Test", "commit", "-q", "--allow-empty", "-m", "enclosing commit")
	outerHead := strings.TrimSpace(runGit(t, outer, "rev-parse", "HEAD"))

	// The MPM source export inside it: the sources, the provenance
	// script, and no Git metadata of its own.
	export := filepath.Join(outer, "mpm-export")
	script := filepath.Join(export, "scripts", "build-provenance.sh")
	if err := os.MkdirAll(filepath.Dir(script), 0o755); err != nil {
		t.Fatalf("create the export tree: %v", err)
	}
	repoScript, err := os.ReadFile(filepath.Join("..", "..", "scripts", "build-provenance.sh"))
	if err != nil {
		t.Fatalf("read the canonical provenance script: %v", err)
	}
	if err := os.WriteFile(script, repoScript, 0o755); err != nil {
		t.Fatalf("write the export's provenance script: %v", err)
	}
	if err := os.WriteFile(filepath.Join(export, "VERSION"), []byte("1.2.3\n"), 0o644); err != nil {
		t.Fatalf("write the export's VERSION: %v", err)
	}
	if err := os.WriteFile(filepath.Join(export, "go.mod"),
		[]byte("module github.com/flowbyte-com/mpm\n\ngo 1.26.6\n"), 0o644); err != nil {
		t.Fatalf("write the export's go.mod: %v", err)
	}

	// The canonical payload for that tree — the same one the Makefile
	// and install.sh consume.
	out, err := exec.Command("bash", script, "ldflags").Output()
	if err != nil {
		t.Fatalf("build-provenance.sh ldflags on the export: %v", err)
	}
	canonicalLDFlags := strings.TrimSpace(string(out))
	for _, want := range []string{
		".Version=1.2.3",
		".Commit=unknown",
		".ShortCommit=unknown",
		".Dirty=unknown",
		".BuildType=development",
	} {
		if !strings.Contains(canonicalLDFlags, want) {
			t.Errorf("the export's ldflags payload does not carry %q:\n%s", want, canonicalLDFlags)
		}
	}
	if strings.Contains(canonicalLDFlags, outerHead) {
		t.Errorf("the enclosing repository's commit leaked into the payload:\n%s", canonicalLDFlags)
	}

	bin := filepath.Join(t.TempDir(), "mpm")
	if err := buildProvenanceBinary("./cmd/mpm", bin, canonicalLDFlags); err != nil {
		t.Fatalf("%v", err)
	}

	stdout, stderr, exit := runProvenanceBinary(t, bin, nil, "version", "--json")
	if exit != 0 {
		t.Fatalf("mpm version --json exited %d\nstdout=%s\nstderr=%s", exit, stdout, stderr)
	}

	var got struct {
		Version    string `json:"version"`
		Commit     string `json:"commit"`
		Dirty      bool   `json:"dirty"`
		DirtyState string `json:"dirty_state"`
		Release    bool   `json:"release"`
		BuildType  string `json:"build_type"`
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("version --json is not valid JSON: %v\n%s", err, stdout)
	}

	if got.Version != "1.2.3" {
		t.Errorf("version = %q, want %q (the VERSION file is still authoritative)", got.Version, "1.2.3")
	}
	if got.Commit != "unknown" {
		t.Errorf("commit = %q, want %q", got.Commit, "unknown")
	}
	if got.Dirty {
		t.Error("dirty = true, want false (an unverifiable tree is not known to be dirty)")
	}
	if got.DirtyState != "unknown" {
		t.Errorf("dirty_state = %q, want %q", got.DirtyState, "unknown")
	}
	if got.Release {
		t.Error("release = true, want false (an unverifiable build is not a release)")
	}
	if got.BuildType != "development" {
		t.Errorf("build_type = %q, want %q", got.BuildType, "development")
	}
}

// runGit runs a git command in dir and returns its stdout.
func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return string(out)
}

// ---------------------------------------------------------------------------
// The lifecycle of the shared build directory (issue #50).
//
// The provenance assertions above are about the CONTENT of the binaries.
// These are about the FILESYSTEM they live in: the directory has to be
// owned by the package, recorded before anything is built into it, and
// removed when the package's tests are done with it.
// ---------------------------------------------------------------------------

// TestBuildProvenance_TheSharedBuildDirectoryIsOwnedByThePackage proves
// every shared binary lives in a directory the package teardown knows
// about.
//
// If a future change wrote one of them somewhere else, the teardown
// would leave it behind and the residue would return.
func TestBuildProvenance_TheSharedBuildDirectoryIsOwnedByThePackage(t *testing.T) {
	stamped, unstamped := buildProvenanceBinaries(t)

	if provenanceBuildDir == "" {
		t.Fatal("the shared build directory was not recorded, so the package teardown cannot remove it")
	}
	if _, err := os.Stat(provenanceBuildDir); err != nil {
		t.Fatalf("the recorded build directory does not exist: %v", err)
	}

	for name, path := range stamped {
		if filepath.Dir(path) != provenanceBuildDir {
			t.Errorf("the %s binary lives in %q, not in the recorded build directory %q",
				name, filepath.Dir(path), provenanceBuildDir)
		}
	}
	for name, path := range unstamped {
		if filepath.Dir(path) != provenanceBuildDir {
			t.Errorf("the unstamped %s binary lives in %q, not in the recorded build directory %q",
				name, filepath.Dir(path), provenanceBuildDir)
		}
	}
}

// TestBuildProvenance_TheBuildDirectoryIsRecordedBeforeTheBuild proves
// the recording happens BEFORE anything is built into the directory.
//
// A directory recorded only after a successful build leaks precisely in
// the case that needs cleaning up most: a build that failed halfway.
func TestBuildProvenance_TheBuildDirectoryIsRecordedBeforeTheBuild(t *testing.T) {
	// The shared pointer is saved and restored: the real directory
	// holds the binaries the other tests in this package still need.
	saved := provenanceBuildDir
	defer func() { provenanceBuildDir = saved }()

	dir, err := newProvenanceBuildDir()
	if err != nil {
		t.Fatalf("create the shared build directory: %v", err)
	}
	if provenanceBuildDir != dir {
		t.Fatalf("the build directory was not recorded: recorded %q, created %q",
			provenanceBuildDir, dir)
	}

	// A build that fails must not discard the recording.
	if err := buildProvenanceBinary("./cmd/this-package-does-not-exist",
		filepath.Join(dir, "mpm"), ""); err == nil {
		t.Error("building a package that does not exist must fail")
	}
	if provenanceBuildDir != dir {
		t.Errorf("a failed build discarded the recorded directory: %q != %q",
			provenanceBuildDir, dir)
	}

	// And the package teardown removes it.
	if err := removeProvenanceBuildDir(); err != nil {
		t.Fatalf("remove the recorded build directory: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("the recorded build directory %q still exists after the teardown", dir)
	}
}

// TestBuildProvenance_ATeardownFailureCannotLeaveThePackageGreen proves
// the package teardown turns a cleanup failure into a FAILING package
// result.
//
// A swallowed cleanup failure is how a temporary filesystem fills up
// slowly: the directory holds ~148 MB and nothing says so. The
// original exit code is preserved when the tests already failed.
func TestBuildProvenance_ATeardownFailureCannotLeaveThePackageGreen(t *testing.T) {
	if got := teardownFailureCode(0); got == 0 {
		t.Error("a cleanup failure must turn a green package result into a failing one")
	}
	if got := teardownFailureCode(3); got != 3 {
		t.Errorf("an already-failing package must keep its exit code, got %d", got)
	}
}
