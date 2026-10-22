// handlers_version_test.go — contracts for the `mpm version` surface.
//
// The version command is the operator's answer to "what am I actually
// running, and is it current?". Three properties are load-bearing and
// each has its own test:
//
//   - it reports REAL provenance, stamped at link time, not a guess;
//   - it never touches the database, the configuration or the network
//     unless the operator explicitly asks for an update check;
//   - it offers a machine-readable form that is valid JSON.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/flowbyte-com/mpm/internal/testenv"
)

// stampedBuildInfo is the provenance the test binaries are stamped with.
// The values are deliberately distinctive so a test can tell stamped
// output from the "dev"/"unknown" placeholders of an unstamped build.
const (
	stampedVersion     = "9.9.9-test"
	stampedCommit      = "abcdef1234567890abcdef1234567890abcdef12"
	stampedShortCommit = "abcdef12"
	stampedBuildType   = "development"
	stampedBuildEpoch  = "1792679720"
)

// stampedIdentity is the build identity the stamped binaries carry.
//
// It is written out rather than assembled from the constants above: the
// identity is a CONTRACT, and a test that recomputed it from the same
// inputs would agree with any change to the format.
const stampedIdentity = "9.9.9-test+gabcdef12.t1792679720"

const stampedLDFlags = "-X github.com/flowbyte-com/mpm/internal/buildinfo.Version=" + stampedVersion +
	" -X github.com/flowbyte-com/mpm/internal/buildinfo.Commit=" + stampedCommit +
	" -X github.com/flowbyte-com/mpm/internal/buildinfo.ShortCommit=" + stampedShortCommit +
	" -X github.com/flowbyte-com/mpm/internal/buildinfo.Dirty=false" +
	" -X github.com/flowbyte-com/mpm/internal/buildinfo.BuildType=" + stampedBuildType +
	" -X github.com/flowbyte-com/mpm/internal/buildinfo.BuildEpoch=" + stampedBuildEpoch

// buildStampedMpm returns the path to the production mpm binary stamped
// with the canonical build-provenance ldflags, exactly as the Makefile
// stamps them.
//
// The binary is the same one the build-provenance tests use, so it is
// built ONCE per test binary and shared by every test that needs it.
// `go build` dominates these tests' cost: the version command's
// behaviour can only be observed by running the real binary, and
// building it once per test would spend the same wall clock eight times
// over for no extra coverage. The cmd/mpm race suite already runs close
// to its wall-clock budget in CI, so the tests here keep their
// footprint to a single build.
//
// This is the "same process as install.sh" check: install.sh reaches its
// binaries through `make build`, so a binary stamped the Makefile's way
// must report its provenance.
func buildStampedMpm(t *testing.T) string {
	t.Helper()
	stamped, _ := buildProvenanceBinaries(t)
	return stamped["mpm"]
}

func runMpmVersion(t *testing.T, bin string, env []string, args ...string) (string, string, int) {
	t.Helper()
	return runMpmVersionAt(t, bin, "", env, args...)
}

// runMpmVersionAt runs the version surface of a binary in a given
// working directory.
//
// The working directory is empty for most callers, which leaves the
// child's cwd where the test runs. A caller that needs the binary to run
// somewhere else — outside any Git checkout, say — names that directory
// here.
func runMpmVersionAt(t *testing.T, bin, dir string, env []string, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	// The environment comes from testenv.WithExtra, which pins
	// MPM_WORKSPACE and HOME to temporary directories. A child that
	// inherited the ambient environment could resolve the operator's
	// live installation; the repository's subprocess-isolation guard
	// (internal/testenv) exists for exactly that reason. `env` entries
	// are appended last, so a caller can deliberately point
	// MPM_WORKSPACE somewhere else.
	cmd.Env = testenv.WithExtra(t, env...)
	if dir != "" {
		cmd.Dir = dir
	}
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	exit := 0
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			exit = ee.ExitCode()
		} else {
			t.Fatalf("run %s %v: %v", bin, args, err)
		}
	}
	return stdout.String(), stderr.String(), exit
}

func TestVersionCommand_ReportsTheStampedProvenance(t *testing.T) {
	bin := buildStampedMpm(t)

	stdout, stderr, exit := runMpmVersion(t, bin, nil, "version")
	if exit != 0 {
		t.Fatalf("mpm version exited %d\nstdout=%s\nstderr=%s", exit, stdout, stderr)
	}

	for _, want := range []string{
		stampedIdentity,
		stampedShortCommit,
		stampedCommit,
		stampedBuildType,
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("version output is missing %q:\n%s", want, stdout)
		}
	}

	// The identity is the FIRST line: it is the one line an operator
	// copies into a bug report.
	first := strings.SplitN(stdout, "\n", 2)[0]
	if want := "MPM " + stampedIdentity; first != want {
		t.Errorf("first line = %q, want %q", first, want)
	}
}

// TestVersionCommand_DashVAndDashVersionAgreeWithTheCommand pins that
// every accepted spelling reports the SAME build identity.
//
// The comparison is over the whole output, not just the first line: a
// flag that reported the same identity but a different commit, or a
// different dirty state, would be just as wrong.
func TestVersionCommand_DashVAndDashVersionAgreeWithTheCommand(t *testing.T) {
	bin := buildStampedMpm(t)

	base, _, exit := runMpmVersion(t, bin, nil, "version")
	if exit != 0 {
		t.Fatalf("mpm version exited %d", exit)
	}

	for _, flag := range []string{"-v", "--version", "-V"} {
		out, stderr, exit := runMpmVersion(t, bin, nil, flag)
		if exit != 0 {
			t.Fatalf("mpm %s exited %d\nstderr=%s", flag, exit, stderr)
		}
		if out != base {
			t.Errorf("mpm %s printed a different block than `mpm version`\ngot:\n%s\nwant:\n%s", flag, out, base)
		}
	}
}

// TestVersionCommand_TheIdentitySurvivesRelocation proves the identity
// is stamped INTO the binary rather than derived when it runs.
//
// An installed binary normally runs outside any Git checkout, so a
// version implementation that resolved the commit by running git would
// report nothing there — or worse, the commit of whatever repository
// happened to enclose the working directory.
//
// The test puts a FAKE git on PATH that answers every query with a
// plausible-looking revision, and runs the binary from a directory that
// is not a checkout. A binary that consulted git would report the fake
// revision; this one must report the identity it was built with.
func TestVersionCommand_TheIdentitySurvivesRelocation(t *testing.T) {
	bin := buildStampedMpm(t)

	original, stderr, exit := runMpmVersion(t, bin, nil, "-v")
	if exit != 0 {
		t.Fatalf("mpm -v exited %d\nstderr=%s", exit, stderr)
	}
	if !strings.Contains(original, stampedIdentity) {
		t.Fatalf("the stamped binary does not report its identity:\n%s", original)
	}

	// A directory that is not a Git checkout, and a fake git that would
	// answer with a different revision if anything asked it.
	relocated := t.TempDir()
	fakeBin := filepath.Join(relocated, "bin")
	if err := os.MkdirAll(fakeBin, 0o755); err != nil {
		t.Fatalf("create the fake bin directory: %v", err)
	}
	const fakeCommit = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	fakeGit := filepath.Join(fakeBin, "git")
	if err := os.WriteFile(fakeGit, []byte("#!/bin/sh\necho "+fakeCommit+"\n"), 0o755); err != nil {
		t.Fatalf("write the fake git: %v", err)
	}

	moved := filepath.Join(relocated, "mpm")
	raw, err := os.ReadFile(bin)
	if err != nil {
		t.Fatalf("read the binary: %v", err)
	}
	if err := os.WriteFile(moved, raw, 0o755); err != nil {
		t.Fatalf("copy the binary: %v", err)
	}

	got, stderr, exit := runMpmVersionAt(t, moved, relocated,
		[]string{"PATH=" + fakeBin + ":/usr/bin:/bin:/usr/local/go/bin"}, "-v")
	if exit != 0 {
		t.Fatalf("the relocated binary exited %d\nstderr=%s", exit, stderr)
	}
	if got != original {
		t.Errorf("the relocated binary reported a different identity\n got:\n%s\nwant:\n%s", got, original)
	}
	if strings.Contains(got, fakeCommit[:8]) {
		t.Errorf("the binary resolved its provenance through git at run time:\n%s", got)
	}
}

// TestVersionCommand_TheIdentityIsStableAcrossInvocations proves the
// build time is the one the build captured, not the clock at the moment
// the command runs.
//
// The two runs are more than a second apart, so a timestamp re-derived
// from the clock would differ between them. A build that stamped its
// build time answers the same every time.
func TestVersionCommand_TheIdentityIsStableAcrossInvocations(t *testing.T) {
	bin := buildStampedMpm(t)

	first, stderr, exit := runMpmVersion(t, bin, nil, "-v")
	if exit != 0 {
		t.Fatalf("mpm -v exited %d\nstderr=%s", exit, stderr)
	}

	// Cross a second boundary: a clock-derived build time would change.
	time.Sleep(1100 * time.Millisecond)

	second, stderr, exit := runMpmVersion(t, bin, nil, "-v")
	if exit != 0 {
		t.Fatalf("mpm -v exited %d\nstderr=%s", exit, stderr)
	}

	if first != second {
		t.Errorf("two invocations of the same binary reported different identities\n first:\n%s\nsecond:\n%s", first, second)
	}
	if !strings.Contains(second, ".t"+stampedBuildEpoch) {
		t.Errorf("the identity does not carry the stamped build time:\n%s", second)
	}
}

func TestVersionCommand_JSONIsValidAndComplete(t *testing.T) {
	bin := buildStampedMpm(t)

	stdout, stderr, exit := runMpmVersion(t, bin, nil, "version", "--json")
	if exit != 0 {
		t.Fatalf("mpm version --json exited %d\nstdout=%s\nstderr=%s", exit, stdout, stderr)
	}

	var got map[string]any
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("version --json is not valid JSON: %v\n%s", err, stdout)
	}

	for _, key := range []string{
		"version", "commit", "short_commit", "dirty", "dirty_state", "release",
		"build_type", "repository", "binary", "build_epoch", "build_identity",
	} {
		if _, ok := got[key]; !ok {
			t.Errorf("version --json is missing the %q key:\n%s", key, stdout)
		}
	}

	if got["version"] != stampedVersion {
		t.Errorf("version = %v, want %q", got["version"], stampedVersion)
	}
	if got["build_identity"] != stampedIdentity {
		t.Errorf("build_identity = %v, want %q", got["build_identity"], stampedIdentity)
	}
	if got["build_epoch"] != float64(1792679720) {
		t.Errorf("build_epoch = %v (%T), want the stamped epoch 1792679720", got["build_epoch"], got["build_epoch"])
	}
	if got["commit"] != stampedCommit {
		t.Errorf("commit = %v, want %q", got["commit"], stampedCommit)
	}
	if got["short_commit"] != stampedShortCommit {
		t.Errorf("short_commit = %v, want %q", got["short_commit"], stampedShortCommit)
	}
	if got["dirty"] != false {
		t.Errorf("dirty = %v (%T), want the boolean false", got["dirty"], got["dirty"])
	}
	if got["dirty_state"] != "clean" {
		t.Errorf("dirty_state = %v, want %q", got["dirty_state"], "clean")
	}
	if got["release"] != false {
		t.Errorf("release = %v (%T), want the boolean false", got["release"], got["release"])
	}
	if got["binary"] != "mpm" {
		t.Errorf("binary = %v, want %q", got["binary"], "mpm")
	}
}

func TestVersionCommand_TouchesNoDatabaseOrConfiguration(t *testing.T) {
	bin := buildStampedMpm(t)

	// A workspace that does not exist. If the version command touches the
	// configuration, it will create this directory.
	workspace := filepath.Join(t.TempDir(), "must-not-be-created")

	stdout, stderr, exit := runMpmVersion(t, bin,
		[]string{"MPM_WORKSPACE=" + workspace, "MPM_NO_AUTO_INIT=1"}, "version")
	if exit != 0 {
		t.Fatalf("mpm version exited %d\nstdout=%s\nstderr=%s", exit, stdout, stderr)
	}

	if _, err := os.Stat(workspace); err == nil {
		t.Errorf("the version command created %s; version reporting must not touch the configuration or the database", workspace)
	}
}

func TestVersionCommand_WithoutCheckMakesNoNetworkCall(t *testing.T) {
	bin := buildStampedMpm(t)

	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	stdout, stderr, exit := runMpmVersion(t, bin,
		[]string{"MPM_UPDATE_CHECK_API_BASE=" + srv.URL}, "version", "--json")
	if exit != 0 {
		t.Fatalf("mpm version --json exited %d\nstdout=%s\nstderr=%s", exit, stdout, stderr)
	}

	if hits != 0 {
		t.Errorf("the version command made %d network call(s); it must be offline unless --check is given", hits)
	}

	var got map[string]any
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("version --json is not valid JSON: %v\n%s", err, stdout)
	}
	if _, ok := got["update"]; ok {
		t.Errorf("version --json reported an update result without --check:\n%s", stdout)
	}
}

func TestVersionCommand_CheckReportsTheUpstreamDistance(t *testing.T) {
	bin := buildStampedMpm(t)

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/flowbyte-com/mpm/git/ref/heads/main", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"ref":"refs/heads/main","object":{"sha":"1111111aaaaaaa"}}`)
	})
	mux.HandleFunc("/repos/flowbyte-com/mpm/compare/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"status":"ahead","ahead_by":3,"behind_by":0,"total_commits":3}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	stdout, stderr, exit := runMpmVersion(t, bin,
		[]string{"MPM_UPDATE_CHECK_API_BASE=" + srv.URL}, "version", "--check", "--json")
	if exit != 0 {
		t.Fatalf("mpm version --check --json exited %d\nstdout=%s\nstderr=%s", exit, stdout, stderr)
	}

	var got struct {
		Update struct {
			Status         string `json:"status"`
			BehindBy       int    `json:"behind_by"`
			AheadBy        int    `json:"ahead_by"`
			UpstreamCommit string `json:"upstream_commit"`
		} `json:"update"`
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("version --check --json is not valid JSON: %v\n%s", err, stdout)
	}

	if got.Update.Status != "behind" {
		t.Errorf("update.status = %q, want %q", got.Update.Status, "behind")
	}
	if got.Update.BehindBy != 3 {
		t.Errorf("update.behind_by = %d, want 3", got.Update.BehindBy)
	}
	if got.Update.UpstreamCommit != "1111111aaaaaaa" {
		t.Errorf("update.upstream_commit = %q, want %q", got.Update.UpstreamCommit, "1111111aaaaaaa")
	}
}

func TestVersionCommand_CheckSurvivesAnUnreachableUpstream(t *testing.T) {
	bin := buildStampedMpm(t)

	srv := httptest.NewServer(http.NotFoundHandler())
	base := srv.URL
	srv.Close()

	stdout, stderr, exit := runMpmVersion(t, bin,
		[]string{"MPM_UPDATE_CHECK_API_BASE=" + base}, "version", "--check", "--json")
	if exit != 0 {
		t.Fatalf("an unreachable upstream must not fail the command; exited %d\nstdout=%s\nstderr=%s", exit, stdout, stderr)
	}

	var got struct {
		Update struct {
			Status string `json:"status"`
			Error  string `json:"error"`
		} `json:"update"`
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("version --check --json is not valid JSON: %v\n%s", err, stdout)
	}
	if got.Update.Status != "unknown" {
		t.Errorf("update.status = %q, want %q", got.Update.Status, "unknown")
	}
	if got.Update.Error == "" {
		t.Error("an unknown update status must carry an explanation")
	}
}

func TestVersionCommand_RejectsUnknownFlags(t *testing.T) {
	bin := buildStampedMpm(t)

	_, stderr, exit := runMpmVersion(t, bin, nil, "version", "--bogus")
	if exit == 0 {
		t.Errorf("mpm version --bogus must fail; stderr=%s", stderr)
	}
}
