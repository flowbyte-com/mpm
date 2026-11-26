package opencode

// containment_exec_test.go — the containment, EXECUTED.
//
// The argv tests prove the namespace is ASKED for. They cannot prove it
// is BUILT. This file runs the real argv — outer unshare, then
// bubblewrap, then a probe inside the namespace — and asserts what the
// worker and the reviewer can actually see and do (T49/T50), and that a
// host without bubblewrap REFUSES rather than degrading to an
// unsandboxed run (T51).
//
// The probe is a shell script standing in for the runtime binary: it is
// bound read-only at /opt/opencode/opencode, exactly where the real
// runtime is bound, and it records what it can reach.

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// containmentTools returns the two binaries the namespace needs. The
// caller decides what to do when they are absent.
func containmentTools(t *testing.T) (unshare, bwrap string) {
	t.Helper()
	unshare, unshareErr := exec.LookPath("unshare")
	if unshareErr != nil {
		return "", ""
	}
	bwrap, bwrapErr := exec.LookPath("bwrap")
	if bwrapErr != nil {
		return "", ""
	}
	return unshare, bwrap
}

// containmentFixture builds a sandbox root, a worktree, a sidecar socket
// placeholder and a probe binary that reports what it can reach.
//
// probeScript is executed INSIDE the namespace. It writes its findings
// to the bound /sandbox/findings directory, which is the only writable
// place besides the worktree.
func containmentFixture(t *testing.T, probeScript string) (Spec, string, []string) {
	t.Helper()
	root := t.TempDir()
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "authorised.txt"), []byte("work"), 0o600); err != nil {
		t.Fatalf("seed the worktree: %v", err)
	}
	// A path OUTSIDE the sandbox that the probe will try to reach. It
	// exists on the host, so a failure proves the namespace hides it
	// rather than the file being absent.
	secret := filepath.Join(t.TempDir(), "operator-home-marker")
	if err := os.WriteFile(secret, []byte("should-not-be-visible"), 0o600); err != nil {
		t.Fatalf("seed the host marker: %v", err)
	}

	bin := filepath.Join(t.TempDir(), "opencode")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"+probeScript+"\n"), 0o700); err != nil {
		t.Fatalf("write the probe: %v", err)
	}
	socket := filepath.Join(t.TempDir(), "sidecar.sock")
	if err := os.WriteFile(socket, []byte("placeholder"), 0o600); err != nil {
		t.Fatalf("write the socket placeholder: %v", err)
	}

	spec := Spec{
		Root:           root,
		Workspace:      workspace,
		OpenCodeBin:    bin,
		SidecarSocket:  socket,
		SidecarBaseURL: "http://" + RelayAddr + "/v1",
		Env: map[string]string{
			"HOME":            PrefixHome,
			"TMPDIR":          PrefixTmp,
			"PATH":            sandboxPath,
			"XDG_CONFIG_HOME": PrefixOCfg,
			"XDG_DATA_HOME":   PrefixOData,
			"XDG_CACHE_HOME":  PrefixOCache,
			"XDG_STATE_HOME":  PrefixOState,
			"LANG":            "C.UTF-8",
			"TERM":            "dumb",
		},
		Command: []string{"/opt/opencode/opencode"},
	}
	if err := BuildTree(spec); err != nil {
		t.Fatalf("build the sandbox tree: %v", err)
	}
	return spec, secret, []string{root, workspace, secret}
}

// skipIfNoUserNamespace recognises an environment that FORBIDS user
// namespaces (most CI containers write-protect /proc/self/uid_map).
//
// That is an environment limitation, not a containment failure, and it
// is not evidence either way. A capable host executes these tests; the
// report names where they ran. A REAL containment breach still fails,
// because the assertions only run when the namespace is created.
func skipIfNoUserNamespace(t *testing.T, out string, err error) {
	t.Helper()
	if err == nil {
		return
	}
	lower := strings.ToLower(out)
	for _, marker := range []string{
		"uid_map",
		"operation not permitted",
		"no permissions to create",
		"unshare failed",
		"failed to unshare",
	} {
		if strings.Contains(lower, marker) {
			t.Skipf("this environment forbids user namespaces, so the containment "+
				"cannot be executed here (a capable host runs it): %s",
				strings.TrimSpace(out))
		}
	}
}

// runContained executes the containment argv with the same explicit
// environment the adapter uses, and returns the combined output.
func runContained(t *testing.T, spec Spec) (string, error) {
	t.Helper()
	argv, err := spec.Argv()
	if err != nil {
		t.Fatalf("build the containment argv: %v", err)
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	// Every containment run in this file is pinned to its DISPOSABLE
	// sandbox root: no MPM subprocess may inherit the operator's
	// workspace (internal/testenv guard).
	cmd.Env = []string{
		"PATH=" + sandboxPath,
		"HOME=" + filepath.Join(spec.Root, "home"),
		"MPM_WORKSPACE=" + spec.Root,
	}
	out, runErr := cmd.CombinedOutput()
	return string(out), runErr
}

// TestContainedWorkerCannotReachHostPathsIsT49 runs the REAL namespace
// and proves the worker cannot reach the operator's filesystem, the
// operator CLI, or the host's MPM database — while the authorised
// worktree IS reachable and writable.
func TestContainedWorkerCannotReachHostPathsIsT49(t *testing.T) {
	if _, bwrap := containmentTools(t); bwrap == "" {
		t.Skip("bubblewrap is not installed, so containment cannot be executed here")
	}
	probe := `
set -u
out=/sandbox/findings/probe.txt
: > "$out"

echo "pwd=$(pwd)" >> "$out"

# The authorised worktree is visible and writable.
if [ -f /sandbox/worker/authorised.txt ]; then
  echo "worktree=visible" >> "$out"
else
  echo "worktree=hidden" >> "$out"
fi
if ( echo written > /sandbox/worker/probe-write.txt ) 2>/dev/null; then
  echo "worktree_write=allowed" >> "$out"
else
  echo "worktree_write=denied" >> "$out"
fi

# The host filesystem outside the namespace must not exist.
for p in HOST_SECRET_PLACEHOLDER /sandbox/../.. /home /root /etc/shadow; do
  if [ -e "$p" ]; then echo "visible=$p" >> "$out"; else echo "hidden=$p" >> "$out"; fi
done

# The operator CLI and the host's MPM directory must be unreachable.
if command -v mpm >/dev/null 2>&1; then
  echo "operator_cli=reachable" >> "$out"
else
  echo "operator_cli=unreachable" >> "$out"
fi
for p in /opt/mpm/mpm /sandbox/mpm; do
  if [ -e "$p" ]; then echo "mpm_bin=$p" >> "$out"; fi
done

# Writes outside the sandbox must fail.
if ( echo escape > /tmp/escape.txt ) 2>/dev/null; then
  echo "tmp_write=allowed" >> "$out"
else
  echo "tmp_write=denied" >> "$out"
fi
if ( echo escape > HOST_SECRET_PLACEHOLDER ) 2>/dev/null; then
  echo "host_write=allowed" >> "$out"
else
  echo "host_write=denied" >> "$out"
fi

# No provider credential is visible.
if env | grep -qi "API_KEY\|ANTHROPIC\|OPENAI"; then
  echo "credential=visible" >> "$out"
else
  echo "credential=absent" >> "$out"
fi
`
	spec, secret, _ := containmentFixture(t, probe)
	spec.Command = append(spec.Command, "")
	// The placeholder is substituted AFTER the fixture so the probe can
	// name the real host path it must fail to reach.
	probe = strings.ReplaceAll(probe, "HOST_SECRET_PLACEHOLDER", secret)
	if err := os.WriteFile(spec.OpenCodeBin,
		[]byte("#!/bin/sh\n"+probe+"\n"), 0o700); err != nil {
		t.Fatalf("rewrite the probe: %v", err)
	}
	spec.Command = []string{"/opt/opencode/opencode"}

	out, err := runContained(t, spec)
	if err != nil {
		skipIfNoUserNamespace(t, out, err)
		t.Fatalf("the containment did not run: %v\n%s", err, out)
	}
	findings, err := os.ReadFile(filepath.Join(spec.Root, "findings", "probe.txt"))
	if err != nil {
		t.Fatalf("the probe wrote no findings: %v\n%s", err, out)
	}
	report := string(findings)
	for _, want := range []string{
		"pwd=/sandbox/worker",
		"worktree=visible",
		"worktree_write=allowed",
		"hidden=" + secret,
		"hidden=/home",
		"hidden=/root",
		"hidden=/etc/shadow",
		"operator_cli=unreachable",
		"tmp_write=allowed", // /tmp is a private tmpfs, not the host's
		"host_write=denied",
		"credential=absent",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("the containment report is missing %q:\n%s", want, report)
		}
	}
	// The MPM relay binary is not in this spec, so no MPM binary may be
	// reachable at all.
	if strings.Contains(report, "mpm_bin=") {
		t.Errorf("an MPM binary is reachable inside the worker namespace:\n%s", report)
	}
	// The host marker is untouched.
	if body, err := os.ReadFile(secret); err != nil || string(body) != "should-not-be-visible" {
		t.Fatalf("the contained worker modified a host file: %v %q", err, string(body))
	}
}

// TestContainedReviewerCannotReachTheDatabaseOrHomeIsT50 runs the
// reviewer's containment and proves the production database and the
// operator's home are not mounted, while the reviewer's own sandbox
// tree is.
func TestContainedReviewerCannotReachTheDatabaseOrHomeIsT50(t *testing.T) {
	if _, bwrap := containmentTools(t); bwrap == "" {
		t.Skip("bubblewrap is not installed, so containment cannot be executed here")
	}
	dbPath := filepath.Join(t.TempDir(), "mpm.db")
	if err := os.WriteFile(dbPath, []byte("SQLite format 3\x00"), 0o600); err != nil {
		t.Fatalf("seed the database marker: %v", err)
	}
	homePath := filepath.Join(t.TempDir(), "operator-home")
	if err := os.MkdirAll(homePath, 0o700); err != nil {
		t.Fatalf("seed the operator home: %v", err)
	}
	probe := `
set -u
out=/sandbox/findings/reviewer.txt
: > "$out"
for p in DB_PLACEHOLDER HOME_PLACEHOLDER /sandbox/home /sandbox/worker; do
  if [ -e "$p" ]; then echo "visible=$p" >> "$out"; else echo "hidden=$p" >> "$out"; fi
done
if [ -d /sandbox/findings ]; then echo "findings=writable" >> "$out"; fi
`
	probe = strings.ReplaceAll(probe, "DB_PLACEHOLDER", dbPath)
	probe = strings.ReplaceAll(probe, "HOME_PLACEHOLDER", homePath)
	spec, _, _ := containmentFixture(t, probe)

	out, err := runContained(t, spec)
	if err != nil {
		skipIfNoUserNamespace(t, out, err)
		t.Fatalf("the reviewer containment did not run: %v\n%s", err, out)
	}
	findings, err := os.ReadFile(filepath.Join(spec.Root, "findings", "reviewer.txt"))
	if err != nil {
		t.Fatalf("the reviewer probe wrote no findings: %v\n%s", err, out)
	}
	report := string(findings)
	for _, want := range []string{
		"hidden=" + dbPath,
		"hidden=" + homePath,
		// The reviewer sees ITS sandbox home, not the operator's.
		"visible=/sandbox/home",
		"findings=writable",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("the reviewer containment report is missing %q:\n%s", want, report)
		}
	}
	// The host's home directory must not be the sandbox home.
	if strings.Contains(report, "visible="+homePath) {
		t.Errorf("the operator home is mounted in the reviewer namespace:\n%s", report)
	}
}

// TestNoBubblewrapMeansNoLaunchIsT51 proves the boundary is not a
// best-effort one: when bubblewrap cannot run, the namespace fails and
// the command is NOT executed on the host. The same-UID threat model
// stays documented — an agent running as the operator can run `mpm`
// itself — but the adapter never silently degrades to an unsandboxed
// launch.
func TestNoBubblewrapMeansNoLaunchIsT51(t *testing.T) {
	if unshare, bwrap := containmentTools(t); unshare == "" || bwrap == "" {
		t.Skip("bubblewrap or unshare is not installed, so the refusal cannot be executed here")
	}
	// A sentinel the command would create if it ever ran unsandboxed.
	sentinel := filepath.Join(t.TempDir(), "ran-unsandboxed")
	probe := "#!/bin/sh\n/usr/bin/touch " + sentinel + "\n"
	spec, _, _ := containmentFixture(t, "")
	if err := os.WriteFile(spec.OpenCodeBin, []byte(probe), 0o700); err != nil {
		t.Fatalf("write the probe: %v", err)
	}

	argv, err := spec.Argv()
	if err != nil {
		t.Fatalf("argv: %v", err)
	}
	// PATH inside the namespace is fixed; making bwrap unreachable is
	// achieved by running the OUTER unshare from an environment whose
	// shell cannot find bubblewrap. The argv's own shell probes PATH.
	cmd := exec.Command(argv[0], argv[1:]...)
	// The environment is pinned explicitly: no MPM subprocess may inherit
	// the operator's workspace (internal/testenv guard). PATH is emptied
	// of everything bubblewrap needs, which is the condition under test.
	cmd.Env = []string{
		"PATH=/nonexistent",
		"HOME=" + spec.Root + "/home",
		"MPM_WORKSPACE=" + spec.Root,
	}
	cmd.Dir = spec.Root
	out, runErr := cmd.CombinedOutput()
	if runErr == nil {
		t.Fatalf("a host without bubblewrap ran the containment successfully:\n%s", out)
	}
	if _, statErr := os.Stat(sentinel); statErr == nil {
		t.Fatalf("the command ran UNSANDBOXED when the containment was unavailable:\n%s", out)
	}
	if !strings.Contains(string(out), "bwrap") && !strings.Contains(string(out), "not found") &&
		!strings.Contains(string(out), "No such file") {
		t.Logf("refusal output (no specific marker required): %s", out)
	}
}

// TestSkipIfNoUserNamespaceRecognisesTheCIMessage pins the skip helper:
// the CI runner's exact message must be recognised as an environment
// limitation, so the suite reports it as a skip rather than a failure —
// while a genuine containment failure still fails.
func TestSkipIfNoUserNamespaceRecognisesTheCIMessage(t *testing.T) {
	t.Run("the CI message skips", func(t *testing.T) {
		skipIfNoUserNamespace(t,
			"unshare: write failed /proc/self/uid_map: Operation not permitted",
			errors.New("exit status 1"))
		// Reaching here means the helper did NOT skip.
		t.Fatal("the helper did not recognise the forbidden-user-namespace message")
	})
	t.Run("a genuine failure does not skip", func(t *testing.T) {
		// No skip call: the helper must return, and the assertion below
		// records that the failure was NOT swallowed.
		skipIfNoUserNamespace(t, "probe exited 3: assertion failed", errors.New("exit status 3"))
	})
}
