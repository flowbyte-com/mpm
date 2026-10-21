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
)

// stampedBuildInfo is the provenance the test binaries are stamped with.
// The values are deliberately distinctive so a test can tell stamped
// output from the "dev"/"unknown" placeholders of an unstamped build.
const (
	stampedVersion     = "9.9.9-test"
	stampedCommit      = "abcdef1234567890abcdef1234567890abcdef12"
	stampedShortCommit = "abcdef12"
	stampedBuildType   = "development"
)

const stampedLDFlags = "-X github.com/flowbyte-com/mpm/internal/buildinfo.Version=" + stampedVersion +
	" -X github.com/flowbyte-com/mpm/internal/buildinfo.Commit=" + stampedCommit +
	" -X github.com/flowbyte-com/mpm/internal/buildinfo.ShortCommit=" + stampedShortCommit +
	" -X github.com/flowbyte-com/mpm/internal/buildinfo.Dirty=false" +
	" -X github.com/flowbyte-com/mpm/internal/buildinfo.BuildType=" + stampedBuildType

// buildStampedMpm builds the production mpm binary with the canonical
// build-provenance ldflags, exactly as the Makefile stamps them.
//
// This is the "same process as install.sh" check: install.sh reaches its
// binaries through `make build`, so a binary stamped the Makefile's way
// must report its provenance.
func buildStampedMpm(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}

	binPath := filepath.Join(t.TempDir(), "mpm")
	cmd := exec.Command("go", "build", "-tags", "fts5",
		"-ldflags", stampedLDFlags, "-o", binPath, ".")
	cmd.Dir = "."
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return binPath
}

func runMpmVersion(t *testing.T, bin string, env []string, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), env...)
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
		"MPM v" + stampedVersion,
		stampedShortCommit,
		stampedCommit,
		stampedBuildType,
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("version output is missing %q:\n%s", want, stdout)
		}
	}
}

func TestVersionCommand_DashVAndDashVersionAgreeWithTheCommand(t *testing.T) {
	bin := buildStampedMpm(t)

	base, _, exit := runMpmVersion(t, bin, nil, "version")
	if exit != 0 {
		t.Fatalf("mpm version exited %d", exit)
	}
	baseFirst := strings.SplitN(base, "\n", 2)[0]

	for _, flag := range []string{"-v", "--version"} {
		out, stderr, exit := runMpmVersion(t, bin, nil, flag)
		if exit != 0 {
			t.Fatalf("mpm %s exited %d\nstderr=%s", flag, exit, stderr)
		}
		gotFirst := strings.SplitN(out, "\n", 2)[0]
		if gotFirst != baseFirst {
			t.Errorf("mpm %s printed %q, want %q (the flags must agree with `mpm version`)", flag, gotFirst, baseFirst)
		}
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
		"version", "commit", "short_commit", "dirty", "release",
		"build_type", "repository", "binary",
	} {
		if _, ok := got[key]; !ok {
			t.Errorf("version --json is missing the %q key:\n%s", key, stdout)
		}
	}

	if got["version"] != stampedVersion {
		t.Errorf("version = %v, want %q", got["version"], stampedVersion)
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
