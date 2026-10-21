// version_provenance_test.go — the version surface at the release gate.
//
// The version surface is what an operator reaches for when something is
// wrong, so the release gate holds it to the same standard as the rest
// of the public CLI:
//
//   - every binary reports the SAME provenance (one build, one stamp);
//   - reporting the version touches no database and no configuration;
//   - a binary reports its provenance with NO git checkout present,
//     which is the normal case for an installed binary;
//   - the opt-in update check classifies against VERIFIED ancestry
//     (the upstream comparison API), never against SHA strings.
//
// The update check is exercised against a local httptest server through
// MPM_UPDATE_CHECK_API_BASE. Nothing here reaches the public internet.
package release_acceptance_test

import (
	"bytes"
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

// releaseBinaries are the five binaries `make build` produces. All five
// must carry the same provenance: they are built from one source tree
// by one Makefile target, and an operator must not have to guess which
// of them is the stale one.
var releaseBinaries = []string{
	"mpm",
	"mpm-mcp",
	"mpm-scheduler",
	"mpm-critic",
	"mpm-telemetry",
}

// buildDir is the Makefile's BUILD_DIR, relative to this package.
const buildDir = "../.build/bin"

// findReleaseBinary resolves one developer build artifact and FAILS
// when it is absent.
//
// It deliberately does not fall back to $HOME/.mpm/bin — that would
// exercise the installed production binary instead of the code under
// test, and report a false pass. `make test-release` declares `build`
// as a prerequisite, so the artifact is guaranteed present on the gate
// that runs this suite.
func findReleaseBinary(t *testing.T, name string) string {
	t.Helper()
	candidate := filepath.Join(buildDir, name)
	if _, err := os.Stat(candidate); err != nil {
		abs, absErr := filepath.Abs(candidate)
		if absErr != nil {
			abs = candidate
		}
		t.Fatalf("developer build artifact missing: %s (%v)\n"+
			"  Run `make build` from the repository root first (`make test-release` "+
			"does this for you).\n"+
			"  This suite will not fall back to $HOME/.mpm/bin: doing so would "+
			"exercise the installed production binary instead of the code under test.",
			abs, err)
	}
	abs, err := filepath.Abs(candidate)
	if err != nil {
		t.Fatalf("abs path: %v", err)
	}
	return abs
}

// versionInfo mirrors the documented machine-readable provenance
// contract. It is redeclared here rather than imported: the release
// acceptance module deliberately depends on the published contract,
// not on the implementation package.
type versionInfo struct {
	Version     string `json:"version"`
	Commit      string `json:"commit"`
	ShortCommit string `json:"short_commit"`
	Dirty       bool   `json:"dirty"`
	DirtyState  string `json:"dirty_state"`
	Release     bool   `json:"release"`
	BuildType   string `json:"build_type"`
	Repository  string `json:"repository"`
	Binary      string `json:"binary"`
}

// runVersionIn runs a binary's version surface with a hermetic
// environment rooted at `workspace`, and returns its stdout and stderr.
//
// MPM_WORKSPACE and HOME are pinned to the temporary workspace so the
// child can never resolve the operator's live installation. The
// repository's subprocess-isolation guard (internal/testenv) requires
// exactly this of every test that launches an MPM binary: an unpinned
// child would fall back to $HOME/.mpm and silently create a second
// database there.
func runVersionIn(t *testing.T, bin, workspace string, args []string, extraEnv ...string) (string, string) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = append(append(os.Environ(),
		"MPM_WORKSPACE="+workspace,
		"HOME="+workspace,
	), extraEnv...)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("%s %v failed: %v\nstdout=%s\nstderr=%s",
			filepath.Base(bin), args, err, stdout.String(), stderr.String())
	}
	return stdout.String(), stderr.String()
}

// runVersion runs a binary's version surface in a throwaway workspace.
func runVersion(t *testing.T, bin string, args []string, extraEnv ...string) (string, string) {
	t.Helper()
	return runVersionIn(t, bin, t.TempDir(), args, extraEnv...)
}

// runVersionJSON runs a binary's version surface in JSON form.
func runVersionJSON(t *testing.T, bin string, extraEnv ...string) versionInfo {
	t.Helper()
	stdout, _ := runVersion(t, bin, []string{"version", "--json"}, extraEnv...)
	var info versionInfo
	if err := json.Unmarshal([]byte(stdout), &info); err != nil {
		t.Fatalf("%s version --json did not emit valid JSON: %v\nraw=%s",
			filepath.Base(bin), err, stdout)
	}
	return info
}

// TestVersionSurface_HumanAndJSONFormsAgree proves the human-readable
// and machine-readable forms describe the same build.
//
// The human form is what an operator reads; the JSON form is what a
// script reads. If they disagree, one of them is lying.
func TestVersionSurface_HumanAndJSONFormsAgree(t *testing.T) {
	bin := findReleaseBinary(t, "mpm")

	info := runVersionJSON(t, bin)

	if info.Version == "" || info.Version == "dev" {
		t.Errorf("version = %q; the release build must carry the VERSION file's value", info.Version)
	}
	if info.Commit == "" || info.Commit == "unknown" {
		t.Errorf("commit = %q; the release build must carry a source commit", info.Commit)
	}
	if len(info.Commit) != 40 {
		t.Errorf("commit = %q; want a full 40-character SHA", info.Commit)
	}
	if info.ShortCommit == "" || info.ShortCommit == "unknown" {
		t.Errorf("short_commit = %q; want an abbreviated SHA", info.ShortCommit)
	}
	if !strings.HasPrefix(info.Commit, info.ShortCommit) {
		t.Errorf("short_commit %q is not a prefix of commit %q", info.ShortCommit, info.Commit)
	}
	if info.BuildType == "" {
		t.Error("build_type is empty")
	}
	switch info.DirtyState {
	case "clean", "dirty", "unknown":
	default:
		t.Errorf("dirty_state = %q; want one of clean, dirty, unknown", info.DirtyState)
	}
	// A release is a formally tagged revision built from a CLEAN tree.
	// A locally modified build must never present itself as one, and a
	// build whose dirty state could not be established certainly must
	// not.
	wantRelease := info.BuildType == "release" && info.DirtyState == "clean"
	if info.Release != wantRelease {
		t.Errorf("release = %v but build_type = %q and dirty_state = %q; "+
			"release must be true only for a clean build of a tagged revision",
			info.Release, info.BuildType, info.DirtyState)
	}

	// The human form.
	human, _ := runVersion(t, bin, []string{"--version"})

	if !strings.Contains(human, info.Version) {
		t.Errorf("the human form does not mention the version %q:\n%s", info.Version, human)
	}
	if !strings.Contains(human, info.Commit) {
		t.Errorf("the human form does not mention the commit %q:\n%s", info.Commit, human)
	}
	if !strings.Contains(human, info.ShortCommit) {
		t.Errorf("the human form does not mention the short commit %q:\n%s", info.ShortCommit, human)
	}
	if !strings.Contains(human, "binary:") || !strings.Contains(human, "mpm") {
		t.Errorf("the human form does not name the binary:\n%s", human)
	}

	// The human form must carry the same dirty state as the JSON form.
	// An operator reading the human form must be able to tell "the
	// tree was clean" from "nobody could tell".
	wantDirty := map[string]string{
		"clean":   "dirty:   no",
		"dirty":   "dirty:   yes",
		"unknown": "dirty:   unknown",
	}[info.DirtyState]
	if wantDirty != "" && !strings.Contains(human, wantDirty) {
		t.Errorf("the human form does not report dirty_state %q as %q:\n%s",
			info.DirtyState, wantDirty, human)
	}

	// `-v` and the explicit `version` subcommand must agree with
	// `--version`. These are the spellings that already existed, and
	// breaking one of them is a breaking change.
	for _, spell := range [][]string{{"-v"}, {"version"}} {
		got, _ := runVersion(t, bin, spell)
		if got != human {
			t.Errorf("`mpm %v` differs from `mpm --version`:\n--- %v ---\n%s\n--- --version ---\n%s",
				spell, spell, got, human)
		}
	}
}

// TestVersionSurface_AllFiveBinariesCarryTheSameProvenance proves the
// stamp reaches every binary.
//
// One build, one provenance. If the MCP server or the scheduler
// answered with a different commit than the CLI, an operator reading
// two of them would be reading two different builds.
func TestVersionSurface_AllFiveBinariesCarryTheSameProvenance(t *testing.T) {
	// The CLI is the reference: it is the one binary with a JSON
	// surface.
	cliBin := findReleaseBinary(t, "mpm")
	reference := runVersionJSON(t, cliBin)

	for _, name := range releaseBinaries {
		t.Run(name, func(t *testing.T) {
			bin := findReleaseBinary(t, name)
			human, _ := runVersion(t, bin, []string{"--version"})

			if !strings.Contains(human, reference.Version) {
				t.Errorf("%s does not carry the CLI's version %q:\n%s", name, reference.Version, human)
			}
			if !strings.Contains(human, reference.Commit) {
				t.Errorf("%s does not carry the CLI's commit %q:\n%s", name, reference.Commit, human)
			}
			if !strings.Contains(human, reference.BuildType) {
				t.Errorf("%s does not carry the CLI's build type %q:\n%s", name, reference.BuildType, human)
			}
		})
	}
}

// TestVersionSurface_TouchesNoDatabaseOrConfiguration proves version
// reporting is side-effect-free.
//
// An operator runs `mpm --version` when the installation is suspect.
// If the version command opened the database or created the
// configuration directory, it would fail exactly when it is needed and
// it would mutate state on the way past.
func TestVersionSurface_TouchesNoDatabaseOrConfiguration(t *testing.T) {
	bin := findReleaseBinary(t, "mpm")
	workspace := t.TempDir()

	// The workspace starts empty. After the version command it must
	// still be empty — no database, no config file, no directory.
	before := listWorkspace(t, workspace)
	if len(before) != 0 {
		t.Fatalf("the fixture workspace was not empty to begin with: %v", before)
	}

	runVersionIn(t, bin, workspace, []string{"version", "--json"})

	after := listWorkspace(t, workspace)
	if len(after) != 0 {
		t.Errorf("the version command created %d path(s) in the workspace: %v", len(after), after)
	}
}

// listWorkspace returns every path under root, relative to root.
func listWorkspace(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if path == root {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		out = append(out, rel)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return out
}

// TestVersionSurface_ReportsProvenanceOutsideAnyGitCheckout proves the
// provenance is EMBEDDED, not resolved.
//
// An installed binary has no .git directory next to it. If the version
// command asked git for its own provenance it would report "unknown"
// for every real installation.
func TestVersionSurface_ReportsProvenanceOutsideAnyGitCheckout(t *testing.T) {
	bin := findReleaseBinary(t, "mpm")
	reference := runVersionJSON(t, bin)

	// Copy the binary somewhere with no git checkout above it and run
	// it from there.
	elsewhere := t.TempDir()
	copied := filepath.Join(elsewhere, "mpm")
	payload, err := os.ReadFile(bin)
	if err != nil {
		t.Fatalf("read %s: %v", bin, err)
	}
	if err := os.WriteFile(copied, payload, 0o700); err != nil {
		t.Fatalf("write %s: %v", copied, err)
	}

	workspace := t.TempDir()
	cmd := exec.Command(copied, "version", "--json")
	cmd.Dir = elsewhere
	cmd.Env = append(os.Environ(),
		"MPM_WORKSPACE="+workspace,
		"HOME="+workspace,
	)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("the copied binary failed: %v", err)
	}
	var info versionInfo
	if err := json.Unmarshal(out, &info); err != nil {
		t.Fatalf("the copied binary did not emit valid JSON: %v\nraw=%s", err, string(out))
	}

	if info.Version != reference.Version {
		t.Errorf("version = %q outside a checkout, want %q", info.Version, reference.Version)
	}
	if info.Commit != reference.Commit {
		t.Errorf("commit = %q outside a checkout, want %q", info.Commit, reference.Commit)
	}
	if info.Commit == "unknown" || info.Commit == "" {
		t.Error("the provenance was not embedded; it was resolved at run time")
	}
}

// TestVersionSurface_UpdateCheckClassifiesAgainstVerifiedAncestry
// proves the opt-in update check reports a classification that comes
// from the upstream comparison API.
//
// The mock serves the same two endpoints the real service serves: the
// main-branch reference, then the comparison between the installed
// commit and the upstream head. Each case asserts both the
// classification AND the distance.
func TestVersionSurface_UpdateCheckClassifiesAgainstVerifiedAncestry(t *testing.T) {
	bin := findReleaseBinary(t, "mpm")
	installed := runVersionJSON(t, bin)
	if installed.Commit == "" || installed.Commit == "unknown" {
		t.Skip("this build carries no source commit; there is nothing to compare")
	}

	cases := []struct {
		name         string
		compareState string
		aheadBy      int
		behindBy     int
		wantStatus   string
		wantBehind   int
		wantAhead    int
	}{
		{name: "current", compareState: "identical", wantStatus: "current"},
		{name: "behind", compareState: "ahead", aheadBy: 3, wantStatus: "behind", wantBehind: 3},
		{name: "ahead", compareState: "behind", behindBy: 2, wantStatus: "ahead", wantAhead: 2},
		{name: "diverged", compareState: "diverged", aheadBy: 4, behindBy: 2, wantStatus: "diverged", wantBehind: 4, wantAhead: 2},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case strings.Contains(r.URL.Path, "/git/ref/heads/main"):
					fmt.Fprint(w, `{"ref":"refs/heads/main","object":{"sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}`)
				case strings.Contains(r.URL.Path, "/compare/"):
					fmt.Fprintf(w, `{"status":%q,"ahead_by":%d,"behind_by":%d}`,
						tc.compareState, tc.aheadBy, tc.behindBy)
				default:
					w.WriteHeader(http.StatusNotFound)
					fmt.Fprint(w, `{"message":"Not Found"}`)
				}
			}))
			defer srv.Close()

			stdout, _ := runVersion(t, bin, []string{"version", "--check", "--json"},
				"MPM_UPDATE_CHECK_API_BASE="+srv.URL)

			var payload struct {
				versionInfo
				Update struct {
					Status   string `json:"status"`
					BehindBy int    `json:"behind_by"`
					AheadBy  int    `json:"ahead_by"`
				} `json:"update"`
			}
			if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
				t.Fatalf("not valid JSON: %v\nraw=%s", err, stdout)
			}

			if payload.Update.Status != tc.wantStatus {
				t.Errorf("status = %q, want %q", payload.Update.Status, tc.wantStatus)
			}
			if payload.Update.BehindBy != tc.wantBehind {
				t.Errorf("behind_by = %d, want %d", payload.Update.BehindBy, tc.wantBehind)
			}
			if payload.Update.AheadBy != tc.wantAhead {
				t.Errorf("ahead_by = %d, want %d", payload.Update.AheadBy, tc.wantAhead)
			}
		})
	}
}

// TestVersionSurface_UpdateCheckDegradesCleanlyWhenUpstreamIsAbsent
// proves an unreachable upstream is reported as unknown, not as a
// failure and not as a guess.
//
// A check that cannot establish the relationship must say so. A check
// that reports "0 commits behind" when it simply could not reach the
// service would be worse than useless.
func TestVersionSurface_UpdateCheckDegradesCleanlyWhenUpstreamIsAbsent(t *testing.T) {
	bin := findReleaseBinary(t, "mpm")

	// A closed server stands in for an offline host.
	srv := httptest.NewServer(http.NotFoundHandler())
	base := srv.URL
	srv.Close()

	stdout, _ := runVersion(t, bin, []string{"version", "--check", "--json"},
		"MPM_UPDATE_CHECK_API_BASE="+base)

	var payload struct {
		Update struct {
			Status string `json:"status"`
			Error  string `json:"error"`
		} `json:"update"`
	}
	if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
		t.Fatalf("not valid JSON: %v\nraw=%s", err, stdout)
	}
	if payload.Update.Status != "unknown" {
		t.Errorf("status = %q, want %q", payload.Update.Status, "unknown")
	}
	if payload.Update.Error == "" {
		t.Error("an unknown result must carry an explanation")
	}
}

// TestVersionSurface_OrdinaryVersionReportingMakesNoNetworkCall
// proves the ordinary version command is offline.
//
// `mpm --version` is on the critical path of every support conversation
// and every script's startup. It must not depend on a network that may
// be down, throttled or firewalled.
//
// The check is proved by pointing the API base at a server that records
// every request it receives, and asserting it received none.
func TestVersionSurface_OrdinaryVersionReportingMakesNoNetworkCall(t *testing.T) {
	bin := findReleaseBinary(t, "mpm")

	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	for _, spell := range [][]string{{"--version"}, {"-v"}, {"version"}, {"version", "--json"}} {
		runVersion(t, bin, spell, "MPM_UPDATE_CHECK_API_BASE="+srv.URL)
	}

	if requests != 0 {
		t.Errorf("the ordinary version command made %d network request(s); it must make none", requests)
	}
}
