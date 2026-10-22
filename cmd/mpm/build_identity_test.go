// build_identity_test.go — the BUILD IDENTITY at the level of the build
// convention that produces it.
//
// The identity of a binary is assembled at LINK time from four stamped
// facts: the product version, the source commit, the source tree's
// dirty state and the build time. This file exercises the convention
// itself — scripts/build-provenance.sh and the Makefile wiring that
// feeds it to `go build`.
//
// The properties under test are the ones a build system can silently
// break without failing to compile:
//
//   - the canonical payload stamps the build time at all;
//   - SOURCE_DATE_EPOCH wins, so two builds of the same source produce
//     the same identity;
//   - an unparsable SOURCE_DATE_EPOCH is reported as unknown rather
//     than silently replaced by the clock.
//
// The binary-level consequences — the identity inside a real binary, and
// its survival outside a Git checkout — are covered in
// handlers_version_test.go.

package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// provenanceScriptPath is the canonical build-provenance script,
// relative to the module root.
const provenanceScriptPath = "scripts/build-provenance.sh"

// The canonical binary is built ONCE and shared.
//
// `go build` dominates the cost of these tests, and the canonical stamp
// is a package-wide fact: every test that needs it needs the SAME
// binary.
//
// The binary therefore has to outlive the test that built it, so it
// cannot live in a t.TempDir() — that directory is removed when the
// test ends. It lives in a package-level temporary directory instead,
// and the package's TestMain removes it at teardown. A per-test
// cleanup would be wrong here: it would delete the binary while other
// tests still needed it.
var (
	canonicalBinaryOnce sync.Once
	canonicalBuildInfo  canonicalBuild
	canonicalBinaryErr  error

	// canonicalProvenanceDir holds the canonically stamped binary. It
	// is recorded when the directory is created, so the teardown can
	// remove it even when the build itself failed.
	canonicalProvenanceDir string
)

// removeCanonicalProvenanceDir removes the temporary directory that
// holds the canonically stamped binary.
//
// It is called from the package's TestMain, after the tests have run.
// A removal failure is reported rather than swallowed: the directory
// holds a ~25 MB binary, and a silent failure would turn into a slow
// exhaustion of the temporary filesystem.
func removeCanonicalProvenanceDir() error {
	return removeBuildDir(canonicalProvenanceDir)
}

// removeBuildDir removes a temporary build directory.
//
// An empty path is a no-op: nothing was built, so nothing has to be
// removed.
func removeBuildDir(dir string) error {
	if dir == "" {
		return nil
	}
	return os.RemoveAll(dir)
}

// canonicalBuild is the canonically stamped binary together with the
// facts it was stamped with.
//
// The facts are captured at the moment the payload was produced, not
// read again later: the build time is the moment the payload was
// produced, so a later read would report a different one and the test
// would be comparing two different builds.
type canonicalBuild struct {
	Path    string
	Version string
	Short   string
	Epoch   string
	Dirty   string
}

// buildCanonicalProvenanceBinary returns an mpm binary stamped with the
// canonical ldflags payload — the exact string the Makefile passes to
// `go build`, and therefore the exact stamp install.sh's binaries carry.
func buildCanonicalProvenanceBinary(t *testing.T) canonicalBuild {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}

	canonicalBinaryOnce.Do(func() {
		payload, _, exit := runProvenanceScript(t, nil, "ldflags")
		if exit != 0 {
			canonicalBinaryErr = errScriptFailed
			return
		}
		payload = strings.TrimSpace(payload)

		// The facts are read OUT OF THE PAYLOAD, not from a second run
		// of the script. The payload is what was stamped; the build
		// time in particular belongs to the moment the payload was
		// produced, so reading it again a moment later would compare two
		// different builds.
		// The directory is recorded BEFORE the build, so a build that
		// fails still leaves something the teardown can remove.
		dir, err := os.MkdirTemp("", "mpm-canonical-provenance-")
		if err != nil {
			canonicalBinaryErr = err
			return
		}
		canonicalProvenanceDir = dir

		info := canonicalBuild{
			Path:    filepath.Join(dir, "mpm"),
			Version: payloadField(t, payload, "Version"),
			Short:   payloadField(t, payload, "ShortCommit"),
			Epoch:   payloadField(t, payload, "BuildEpoch"),
			Dirty:   payloadField(t, payload, "Dirty"),
		}

		if err := buildProvenanceBinary("./cmd/mpm", info.Path, payload); err != nil {
			canonicalBinaryErr = err
			return
		}
		canonicalBuildInfo = info
	})

	if canonicalBinaryErr != nil {
		t.Fatalf("build the canonically stamped binary: %v", canonicalBinaryErr)
	}
	return canonicalBuildInfo
}

// payloadField reads one stamped value out of an ldflags payload.
func payloadField(t *testing.T, payload, name string) string {
	t.Helper()

	needle := "." + name + "="
	i := strings.Index(payload, needle)
	if i < 0 {
		t.Fatalf("the payload does not stamp %s:\n%s", name, payload)
	}
	value := payload[i+len(needle):]
	if j := strings.IndexByte(value, ' '); j >= 0 {
		value = value[:j]
	}
	return value
}

// errScriptFailed marks a failure to read the canonical payload.
var errScriptFailed = errors.New("the canonical build-provenance script failed")

// runProvenanceScript runs the canonical provenance script with the
// given environment and returns stdout, stderr and the exit code.
func runProvenanceScript(t *testing.T, env []string, args ...string) (string, string, int) {
	t.Helper()

	cmd := exec.Command("bash", append([]string{provenanceScriptPath}, args...)...)
	cmd.Dir = "../.."
	cmd.Env = append(os.Environ(), env...)

	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	exit := 0
	if err := cmd.Run(); err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("run %s %v: %v", provenanceScriptPath, args, err)
		}
		exit = ee.ExitCode()
	}
	return stdout.String(), stderr.String(), exit
}

// TestBuildIdentity_TheCanonicalScriptStampsTheBuildTime proves the
// canonical payload carries the build time.
//
// A payload that omitted it would still compile — an -X flag naming a
// symbol that does not exist is ignored — and the binary would report a
// build time of "unknown" forever. Only this test notices.
func TestBuildIdentity_TheCanonicalScriptStampsTheBuildTime(t *testing.T) {
	stdout, stderr, exit := runProvenanceScript(t, nil, "ldflags")
	if exit != 0 {
		t.Fatalf("build-provenance.sh ldflags exited %d\nstderr=%s", exit, stderr)
	}
	payload := strings.TrimSpace(stdout)

	if !strings.Contains(payload, ".BuildEpoch=") {
		t.Fatalf("the canonical payload does not stamp the build time:\n%s", payload)
	}

	epoch := payload[strings.Index(payload, ".BuildEpoch=")+len(".BuildEpoch="):]
	if i := strings.IndexByte(epoch, ' '); i >= 0 {
		epoch = epoch[:i]
	}
	n, err := strconv.ParseInt(epoch, 10, 64)
	if err != nil {
		t.Fatalf("the stamped build time %q is not a Unix epoch: %v", epoch, err)
	}
	if n <= 0 {
		t.Errorf("the stamped build time %d is not a plausible epoch", n)
	}
}

// TestBuildIdentity_SourceDateEpochWins proves the reproducible-builds
// convention is honoured.
//
// This is what makes a build verifiable: a builder who publishes the
// source and the epoch can have their build matched exactly.
func TestBuildIdentity_SourceDateEpochWins(t *testing.T) {
	const epoch = "1700000000"

	stdout, stderr, exit := runProvenanceScript(t, []string{"SOURCE_DATE_EPOCH=" + epoch}, "build-epoch")
	if exit != 0 {
		t.Fatalf("build-provenance.sh build-epoch exited %d\nstderr=%s", exit, stderr)
	}
	if got := strings.TrimSpace(stdout); got != epoch {
		t.Errorf("build-epoch = %q, want %q (SOURCE_DATE_EPOCH must win)", got, epoch)
	}

	// The value has to reach the linker payload too, not just the
	// script's own answer.
	stdout, stderr, exit = runProvenanceScript(t, []string{"SOURCE_DATE_EPOCH=" + epoch}, "ldflags")
	if exit != 0 {
		t.Fatalf("build-provenance.sh ldflags exited %d\nstderr=%s", exit, stderr)
	}
	if !strings.Contains(stdout, ".BuildEpoch="+epoch) {
		t.Errorf("the payload does not carry SOURCE_DATE_EPOCH:\n%s", stdout)
	}
}

// TestBuildIdentity_AnInvalidSourceDateEpochIsNotGuessed proves a
// malformed SOURCE_DATE_EPOCH is reported rather than ignored.
//
// Ignoring it would stamp the current clock into a binary whose builder
// asked for a reproducible one — the exact opposite of what they asked
// for, and invisible in the result.
func TestBuildIdentity_AnInvalidSourceDateEpochIsNotGuessed(t *testing.T) {
	stdout, stderr, exit := runProvenanceScript(t, []string{"SOURCE_DATE_EPOCH=not-a-number"}, "build-epoch")
	if exit != 0 {
		t.Fatalf("build-provenance.sh build-epoch exited %d\nstderr=%s", exit, stderr)
	}
	if got := strings.TrimSpace(stdout); got != "unknown" {
		t.Errorf("build-epoch = %q, want %q (a malformed SOURCE_DATE_EPOCH must not be replaced by the clock)", got, "unknown")
	}
	if !strings.Contains(stderr, "SOURCE_DATE_EPOCH") {
		t.Errorf("a malformed SOURCE_DATE_EPOCH must be reported on stderr:\n%s", stderr)
	}
}

// TestBuildIdentity_TheMakefilePassesTheBuildTimeToTheBuild proves the
// Makefile — the thing install.sh actually calls — honours
// SOURCE_DATE_EPOCH.
//
// The script is the convention, but the Makefile is what consumes it.
// A Makefile that dropped the payload would build binaries with no
// build time while the script still reported one.
func TestBuildIdentity_TheMakefilePassesTheBuildTimeToTheBuild(t *testing.T) {
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("make not available")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}

	const epoch = "1700000000"

	cmd := exec.Command("make", "print-build-provenance")
	cmd.Dir = "../.."
	cmd.Env = append(os.Environ(), "SOURCE_DATE_EPOCH="+epoch)

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("make print-build-provenance: %v\n%s", err, out)
	}

	if !strings.Contains(string(out), "MPM_BUILD_EPOCH="+epoch) {
		t.Errorf("the Makefile did not pass SOURCE_DATE_EPOCH through:\n%s", out)
	}
}

// TestBuildIdentity_TheBuildDirectoryIsRemovedAtTeardown proves the
// teardown removes the temporary directory that holds the canonically
// stamped binary — and that a removal FAILURE is reported rather than
// swallowed.
//
// A swallowed removal failure is how a temporary filesystem fills up
// slowly: the directory holds a ~25 MB binary, and nothing says so.
func TestBuildIdentity_TheBuildDirectoryIsRemovedAtTeardown(t *testing.T) {
	dir, err := os.MkdirTemp("", "mpm-canonical-provenance-remove-")
	if err != nil {
		t.Fatalf("create the temporary directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "mpm"), []byte("binary"), 0o755); err != nil {
		t.Fatalf("write the marker: %v", err)
	}

	if err := removeBuildDir(dir); err != nil {
		t.Fatalf("remove the temporary directory: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("the temporary directory %q still exists after the teardown", dir)
	}

	// Nothing built, nothing to remove.
	if err := removeBuildDir(""); err != nil {
		t.Errorf("removing an empty path must be a no-op, got %v", err)
	}

	// A directory that cannot be removed must REPORT that.
	//
	// A read-only directory is the portable way to make removal fail
	// for an unprivileged user. The root user ignores the permission
	// bits, so the assertion is skipped there rather than made
	// vacuous.
	if os.Geteuid() == 0 {
		t.Log("running as root; the removal-failure path cannot be provoked this way")
		return
	}
	locked, err := os.MkdirTemp("", "mpm-canonical-provenance-locked-")
	if err != nil {
		t.Fatalf("create the locked directory: %v", err)
	}
	defer func() {
		os.Chmod(locked, 0o700)
		os.RemoveAll(locked)
	}()
	// The directory holds a file: removing an EMPTY read-only directory
	// succeeds, because the parent's write permission is what governs
	// the removal of the directory entry itself.
	if err := os.WriteFile(filepath.Join(locked, "mpm"), []byte("binary"), 0o755); err != nil {
		t.Fatalf("write the marker: %v", err)
	}
	if err := os.Chmod(locked, 0o500); err != nil {
		t.Fatalf("make the directory read-only: %v", err)
	}
	if err := removeBuildDir(locked); err == nil {
		t.Error("removing a read-only directory must report a failure, not swallow it")
	}
}

// TestBuildIdentity_TheCanonicalStampEmbedsTheIdentity proves a binary
// built the canonical way carries the canonical identity.
//
// The binary is built with the exact payload the Makefile passes to
// `go build`, so this covers the whole chain: script → payload → linker
// → binary → operator's screen.
func TestBuildIdentity_TheCanonicalStampEmbedsTheIdentity(t *testing.T) {
	canonical := buildCanonicalProvenanceBinary(t)

	stdout, stderr, exit := runProvenanceBinary(t, canonical.Path, nil, "--version")
	if exit != 0 {
		t.Fatalf("mpm --version exited %d\nstdout=%s\nstderr=%s", exit, stdout, stderr)
	}

	first := strings.SplitN(stdout, "\n", 2)[0]

	// The identity carries the product version, the commit and the
	// build time. The dirty marker is checked separately, because it
	// depends on the state of the tree the payload was produced from.
	for _, want := range []string{
		canonical.Version,
		"g" + canonical.Short,
		".t" + canonical.Epoch,
	} {
		if !strings.Contains(first, want) {
			t.Errorf("the canonical identity %q does not carry %q", first, want)
		}
	}

	switch canonical.Dirty {
	case "true":
		if !strings.Contains(first, ".dirty") {
			t.Errorf("a dirty tree must be marked in the identity: %q", first)
		}
	case "false":
		if strings.Contains(first, ".dirty") {
			t.Errorf("a clean tree must not be marked dirty in the identity: %q", first)
		}
	default:
		if !strings.Contains(first, ".dirty-unknown") {
			t.Errorf("an unverifiable tree must be marked in the identity: %q", first)
		}
	}
}
