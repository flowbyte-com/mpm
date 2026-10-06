// workspace_cwd_independence_test.go — §4 / §8 end-to-end proof that a
// source checkout is not, and cannot become, a runtime workspace.
//
// # The property
//
// The migration this guards is "clone the source to ~/src/mpm, leave
// ~/.mpm as pure runtime state". For that to be safe, running the binary
// with MPM_WORKSPACE unset must resolve runtime state to $HOME/.mpm —
// even when the process is launched from a directory that *looks like* an
// MPM source checkout and *already contains* a plausible runtime layout.
//
// "Looks like" matters. While the checkout and the runtime root were one
// directory, every artifact the user might run from had
// `src/db/mpm.db` beside it. A resolver that probes the cwd therefore
// looks correct right up until the moment the checkout is empty of
// runtime state — and by then it has already created a DB there. This is
// the ghost-DB shape of 2026-07-21 (lesson 59fe3f8ff3e1549e), which
// mpmcli.ResolveWorkspace() still carried after config.GetWorkspace()
// had already dropped it.
//
// # Why this builds and runs a real binary
//
// Resolver-level tests (internal/core/mpmcli) pin the function. They
// cannot see a *second* resolver: cmd/mpm-main.go, the MCP server, or a
// handler that reaches for config.GetMPMDir() — the latter has an
// MkdirAll side effect and is not exercised by a pure return-value test
// at all. The question here is behavioural: after running an ordinary
// command from a checkout, what exists on disk, and where.
//
// # Isolation
//
// HOME points at a temp sentinel and the cwd is a temp fake checkout, so
// the assertion is about files the run CREATED in a tree that did not
// have them. The operator's real ~/.mpm and real checkout are never
// targets. Nothing here reads or writes production state.

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeSourceCheckout builds a directory that mimics a real MPM checkout:
// the source tree is present, and — critically — the runtime layout that
// used to sit beside it (src/db/, config/) is also present, so a
// cwd-probing resolver would find a home here rather than falling
// through. `dbPresent` controls whether mpm.db exists, letting the test
// cover both "checkout still holds old state" and "checkout is pure
// source".
func fakeSourceCheckout(t *testing.T, dbPresent bool) string {
	t.Helper()
	root := t.TempDir()
	for _, d := range []string{
		"src/db",
		"cmd/mpm",
		"internal/core",
		"config",
		"blobs",
		"run",
	} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	// Source-shaped files, so nothing can conclude "this is not a
	// checkout, therefore it is not a workspace".
	for f, content := range map[string]string{
		"go.mod":                     "module example.invalid/fake\n",
		"README.md":                  "# fake checkout\n",
		"internal/core/mode.go":      "package internal\n",
		"cmd/mpm/main.go":            "package main\n",
		"config/runtime-assets.json": "{}\n",
	} {
		if err := os.WriteFile(filepath.Join(root, f), []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", f, err)
		}
	}
	if dbPresent {
		if err := os.WriteFile(filepath.Join(root, "src", "db", "mpm.db"), []byte("stale"), 0o644); err != nil {
			t.Fatalf("write stale db: %v", err)
		}
	}
	return root
}

// listFiles returns every regular file and directory beneath root, as
// sorted slash-separated paths relative to root.
func listFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return out
}

// runMPMFromCheckout runs the real binary with a controlled HOME and cwd,
// MPM_WORKSPACE deliberately unset, and returns its combined output.
//
// Isolation is HOME redirection to a temp directory. That is the whole
// isolation mechanism, and it is deliberate: pinning MPM_WORKSPACE would
// make this guard pass for the wrong reason, because the $HOME/.mpm
// fallback — the property under test — would be unreachable. The ambient
// MPM_* overrides are stripped so a developer's own shell cannot supply
// what the test is meant to prove is supplied by neither.
func runMPMFromCheckout(t *testing.T, bin, home, checkout string, args ...string) string {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = checkout

	cmd.Env = withoutEnvPrefix(append(os.Environ(),
		"HOME="+home,
		"CGO_CFLAGS=-DSQLITE_ENABLE_FTS5=1",
	), "MPM_WORKSPACE=", "MPM_DB_PATH=", "MPM_ROUTE_WORKSPACE=")

	out, err := cmd.CombinedOutput()
	if err != nil && !commandMayExitNonZero(args) {
		t.Fatalf("mpm %s (cwd=%s): %v\n%s", strings.Join(args, " "), checkout, err, out)
	}
	// For commands that exit nonzero on findings, the run still happened —
	// which is all this test observes. Its assertion is about what appeared
	// on disk, not about whether the command succeeded.
	return string(out)
}

// commandMayExitNonZero lists commands whose exit status reports findings
// rather than failure to run. `mpm doctor` exits nonzero when it has
// something to report, which is normal on a freshly created workspace.
// `mpm drills list` exits nonzero when the drills directory does not
// exist — which is itself a symptom, so the assertion belongs on the
// path it prints, not on the status.
func commandMayExitNonZero(args []string) bool {
	if len(args) == 0 {
		return false
	}
	return args[0] == "doctor" || (args[0] == "drills" && len(args) > 1 && args[1] == "list")
}

// withoutEnvPrefix drops every entry beginning with any of the given
// variable prefixes, so the child cannot inherit an ambient override the
// test is asserting about.
func withoutEnvPrefix(env []string, prefixes ...string) []string {
	out := env[:0:0]
	for _, kv := range env {
		skip := false
		for _, prefix := range prefixes {
			if strings.HasPrefix(kv, prefix) {
				skip = true
				break
			}
		}
		if skip {
			continue
		}
		out = append(out, kv)
	}
	return out
}

func buildMPMForCwdTest(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "mpm-cwd-test")
	cmd := exec.Command("go", "build", "-tags", "fts5", "-o", bin, ".")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

// TestWorkspace_FromSourceCheckout_LandsInHome is §4/§8D: launched from a
// directory that is recognisably a checkout, with MPM_WORKSPACE unset,
// the runtime workspace is $HOME/.mpm.
func TestWorkspace_FromSourceCheckout_LandsInHome(t *testing.T) {
	bin := buildMPMForCwdTest(t)
	home := t.TempDir()
	checkout := fakeSourceCheckout(t, false)

	before := listFiles(t, checkout)
	out := runMPMFromCheckout(t, bin, home, checkout, "call", "mpm_wakes",
		"--payload", `{"action":"list"}`)
	t.Logf("output: %s", out)

	// §8E: nothing was created in the checkout.
	after := listFiles(t, checkout)
	created := difference(after, before)
	if len(created) > 0 {
		t.Fatalf("running mpm from %s created files in the checkout:\n  %s\n\n"+
			"The cwd is not a workspace. State created here is exactly the ghost copy\n"+
			"that the 2026-07-21 incident (lesson 59fe3f8ff3e1549e) was about, and it\n"+
			"reappears the moment the checkout is moved away from the runtime root.",
			checkout, strings.Join(created, "\n  "))
	}

	// And the real workspace did receive state.
	runtimeRoot := filepath.Join(home, ".mpm")
	if !dirExists(runtimeRoot) {
		t.Fatalf("no runtime state under %s; the command did not exercise the workspace", runtimeRoot)
	}
	if !dirExists(filepath.Join(runtimeRoot, "src", "db")) {
		t.Fatalf("runtime DB directory missing under %s", runtimeRoot)
	}
}

// TestWorkspace_CheckoutWithStaleDBIsNotAdopted covers the harder case: the
// checkout still carries the runtime layout it always had, including a
// mpm.db. This is the state every existing user is in right before the
// migration, and it is where a cwd probe would find a "valid" answer and
// quietly keep using it.
func TestWorkspace_CheckoutWithStaleDBIsNotAdopted(t *testing.T) {
	bin := buildMPMForCwdTest(t)
	home := t.TempDir()
	checkout := fakeSourceCheckout(t, true)
	staleDB := filepath.Join(checkout, "src", "db", "mpm.db")
	staleInfo, err := os.Stat(staleDB)
	if err != nil {
		t.Fatalf("stat stale db: %v", err)
	}

	runMPMFromCheckout(t, bin, home, checkout, "call", "mpm_wakes",
		"--payload", `{"action":"list"}`)

	// The checkout DB must be byte-identical: not opened, not migrated,
	// not vacuumed. A resolver that adopts it would at minimum rewrite
	// the WAL header.
	gotInfo, err := os.Stat(staleDB)
	if err != nil {
		t.Fatalf("stale db vanished: %v", err)
	}
	if !gotInfo.ModTime().Equal(staleInfo.ModTime()) || gotInfo.Size() != staleInfo.Size() {
		t.Errorf("checkout DB was modified (size %d→%d, mtime %s→%s); it was adopted as the workspace",
			staleInfo.Size(), gotInfo.Size(), staleInfo.ModTime(), gotInfo.ModTime())
	}

	// The real runtime DB is a different file.
	realDB := filepath.Join(home, ".mpm", "src", "db", "mpm.db")
	if realDB == staleDB {
		t.Fatal("test bug: paths collide")
	}
	if !fileExists(realDB) {
		t.Fatalf("no DB at %s; the run used the checkout instead of $HOME/.mpm", realDB)
	}
}

// TestWorkspace_DrillsPathDoesNotFollowCwd covers the resolver the
// previous test cannot reach.
//
// `mpm call ...` and `doctor` resolve their workspace through
// config.GetWorkspace(), which had already dropped its cwd fallback. So
// running them from a checkout exercises nothing about mpmcli — and a
// resolver-level test would call the function directly rather than
// proving the binary honours it.
//
// `mpm drills list` is different: it resolves $MPM_WORKSPACE/drills via
// mpmcli.ResolveWorkspace() (cmd/mpm/drill_cmds.go). That is the path
// where a "." default would put the drills directory inside a source
// checkout. The assertion is on the path the command PRINTS, which is
// the operator-visible symptom — "no drills found in <checkout>/drills"
// is how this defect shows up in practice.
func TestWorkspace_DrillsPathDoesNotFollowCwd(t *testing.T) {
	bin := buildMPMForCwdTest(t)
	home := t.TempDir()
	checkout := fakeSourceCheckout(t, false)

	out := runMPMFromCheckout(t, bin, home, checkout, "drills", "list")
	t.Logf("output: %s", out)

	runtimeDrills := filepath.Join(home, ".mpm", "drills")
	if !strings.Contains(out, runtimeDrills) {
		t.Fatalf("drills list did not report the runtime drills directory %s.\n"+
			"output: %s\n\nmpmcli.ResolveWorkspace() returned the cwd, so `mpm drills`\n"+
			"would read and write drills inside a source checkout.", runtimeDrills, out)
	}
	if strings.Contains(out, filepath.Join(checkout, "drills")) {
		t.Fatalf("drills list resolved into the checkout:\n%s", out)
	}
}

// TestWorkspace_MultipleCommandsDoNotAccumulateInCheckout is §8E at
// breadth: several ordinary commands in a row, from a checkout, must all
// land in one place. A resolver that consulted the cwd for some paths and
// the env for others would pass the single-command test and fail here.
func TestWorkspace_MultipleCommandsDoNotAccumulateInCheckout(t *testing.T) {
	bin := buildMPMForCwdTest(t)
	home := t.TempDir()
	checkout := fakeSourceCheckout(t, false)
	before := listFiles(t, checkout)

	// A write-path command is included deliberately: a resolver that
	// sent reads to $HOME/.mpm but writes to the cwd would pass a
	// read-only test. mpm_memory add exercises insert + blob + DB
	// creation.
	for _, args := range [][]string{
		{"call", "mpm_wakes", "--payload", `{"action":"list"}`},
		{"call", "mpm_memory", "--payload",
			`{"action":"save","params":{"fact":"cwd-independence probe: this must land under $HOME/.mpm, never in the checkout"}}`},
		{"doctor"},
	} {
		runMPMFromCheckout(t, bin, home, checkout, args...)
	}

	created := difference(listFiles(t, checkout), before)
	if len(created) > 0 {
		t.Fatalf("running 3 commands from the checkout created files there:\n  %s",
			strings.Join(created, "\n  "))
	}
}

// difference returns the entries of after that are not in before.
func difference(after, before []string) []string {
	seen := make(map[string]bool, len(before))
	for _, s := range before {
		seen[s] = true
	}
	var out []string
	for _, s := range after {
		if !seen[s] {
			out = append(out, s)
		}
	}
	return out
}

func dirExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

func fileExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}
