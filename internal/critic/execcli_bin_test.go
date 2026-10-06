package critic

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The critic must never let PATH ordering decide which MPM it audits.
//
// ExecCLI.MPMPath used to default to the bare name "mpm", so under a systemd
// user unit the audit ran against whichever mpm came first on the manager's
// PATH — a stale developer build or an unrelated project — while writing
// findings that looked entirely normal. Every test here pins an ABSOLUTE path
// and asserts on it, because an assertion that merely "some mpm ran" is
// satisfied by the very defect these tests exist to catch.

// fakeMPM writes an executable shell script that records its own argv0 and
// arguments, so a test can assert which physical binary was executed.
//
// Returns the absolute path to the script.
func fakeMPM(t *testing.T, dir, name, logPath string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	body := "#!/bin/sh\nprintf '%s\\n' \"$0 $*\" >> " + shellQuote(logPath) + "\nexit 0\n"
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write fake mpm %s: %v", path, err)
	}
	return path
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// hostileMPM writes a fake mpm that records invocation to a SEPARATE log, so
// a test can prove the hostile binary was never reached.
func hostileMPM(t *testing.T, dir, logPath string) string {
	t.Helper()
	path := filepath.Join(dir, "mpm")
	body := "#!/bin/sh\nprintf 'HOSTILE %s\\n' \"$0 $*\" >> " + shellQuote(logPath) + "\nexit 0\n"
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write hostile mpm: %v", err)
	}
	return path
}

func readLog(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatalf("read log: %v", err)
	}
	return string(b)
}

func payload() map[string]interface{} {
	return map[string]interface{}{"k": "v"}
}

func TestExecCLI_ExplicitMPMPathWins(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "calls.log")
	explicit := fakeMPM(t, dir, "explicit-mpm", log)
	// A different binary is also configured; explicit must win.
	t.Setenv("MPM_BIN", fakeMPM(t, dir, "env-mpm", log))

	c := &ExecCLI{MPMPath: explicit, Timeout: 10 * time.Second}
	if err := c.Call(context.Background(), "mpm_memory", "challenge", payload()); err != nil {
		t.Fatalf("Call: %v", err)
	}

	got := readLog(t, log)
	if !strings.Contains(got, explicit+" call mpm_memory") {
		t.Errorf("call log = %q, want it to name the explicit binary %q", got, explicit)
	}
}

func TestExecCLI_MPMBinUsedWhenExplicitAbsent(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "calls.log")
	envBin := fakeMPM(t, dir, "env-mpm", log)
	t.Setenv("MPM_BIN", envBin)

	c := &ExecCLI{Timeout: 10 * time.Second}
	if err := c.Call(context.Background(), "mpm_memory", "challenge", payload()); err != nil {
		t.Fatalf("Call: %v", err)
	}

	if got := readLog(t, log); !strings.Contains(got, envBin+" call mpm_memory") {
		t.Errorf("call log = %q, want it to name MPM_BIN %q", got, envBin)
	}
}

func TestExecCLI_SiblingBinaryResolves(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "calls.log")
	// A non-standard PREFIX install: mpm and mpm-critic side by side, with
	// MPM_BIN unset. Nothing about $HOME is assumed.
	sibling := fakeMPM(t, dir, "mpm", log)
	self := filepath.Join(dir, "mpm-critic")
	if err := os.WriteFile(self, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write self: %v", err)
	}
	t.Setenv("MPM_BIN", "")

	c := &ExecCLI{selfPath: self, Timeout: 10 * time.Second}
	if err := c.Call(context.Background(), "mpm_memory", "challenge", payload()); err != nil {
		t.Fatalf("Call: %v", err)
	}

	if got := readLog(t, log); !strings.Contains(got, sibling+" call mpm_memory") {
		t.Errorf("call log = %q, want it to name the sibling binary %q", got, sibling)
	}
}

func TestExecCLI_HostilePathMpmIsIgnored(t *testing.T) {
	dir := t.TempDir()
	goodLog := filepath.Join(dir, "good.log")
	hostileLog := filepath.Join(dir, "hostile.log")

	good := fakeMPM(t, dir, "good-mpm", goodLog)
	hostileDir := t.TempDir()
	hostileMPM(t, hostileDir, hostileLog) // placed FIRST on PATH
	t.Setenv("PATH", hostileDir)
	t.Setenv("MPM_BIN", good)

	c := &ExecCLI{Timeout: 10 * time.Second}
	if err := c.Call(context.Background(), "mpm_memory", "challenge", payload()); err != nil {
		t.Fatalf("Call: %v", err)
	}

	if got := readLog(t, goodLog); !strings.Contains(got, good+" call mpm_memory") {
		t.Errorf("call log = %q, want the pinned binary %q", got, good)
	}
	if got := readLog(t, hostileLog); got != "" {
		t.Errorf("hostile PATH mpm was executed: %q", got)
	}
}

// TestExecCLI_NoConfigAndNoSiblingFails is the anti-vacuity guard for the
// whole contract: with PATH scrubbed and no sibling, resolution must fail
// rather than quietly discover something.
func TestExecCLI_NoConfigAndNoSiblingFails(t *testing.T) {
	dir := t.TempDir()
	hostileLog := filepath.Join(dir, "hostile.log")
	hostileDir := t.TempDir()
	hostileMPM(t, hostileDir, hostileLog)
	t.Setenv("PATH", hostileDir)
	t.Setenv("MPM_BIN", "")

	// selfPath in a directory with no sibling mpm.
	self := filepath.Join(dir, "mpm-critic")
	if err := os.WriteFile(self, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write self: %v", err)
	}
	c := &ExecCLI{selfPath: self, Timeout: 10 * time.Second}

	err := c.Call(context.Background(), "mpm_memory", "challenge", payload())
	if err == nil {
		t.Fatal("Call succeeded with no configured or sibling binary; PATH fallback still exists")
	}
	if got := readLog(t, hostileLog); got != "" {
		t.Errorf("hostile PATH mpm was executed as a fallback: %q", got)
	}
	if !strings.Contains(err.Error(), "MPM_BIN") {
		t.Errorf("error = %v, want it to name MPM_BIN so the operator can fix it", err)
	}
}

func TestExecCLI_MissingConfiguredBinaryFails(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "not-installed-mpm")
	t.Setenv("MPM_BIN", missing)

	c := &ExecCLI{Timeout: 10 * time.Second}
	err := c.Call(context.Background(), "mpm_memory", "challenge", payload())
	if err == nil {
		t.Fatal("Call succeeded with a nonexistent MPM_BIN")
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("error = %v, want it to name the missing path %q", err, missing)
	}
}

func TestExecCLI_NonExecutableBinaryFails(t *testing.T) {
	dir := t.TempDir()
	notExec := filepath.Join(dir, "mpm")
	if err := os.WriteFile(notExec, []byte("#!/bin/sh\nexit 0\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Setenv("MPM_BIN", notExec)

	c := &ExecCLI{Timeout: 10 * time.Second}
	err := c.Call(context.Background(), "mpm_memory", "challenge", payload())
	if err == nil {
		t.Fatal("Call succeeded with a non-executable MPM_BIN")
	}
	if !strings.Contains(err.Error(), "not executable") {
		t.Errorf("error = %v, want it to say the binary is not executable", err)
	}
}

func TestExecCLI_DirectoryPathFails(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MPM_BIN", dir)

	c := &ExecCLI{Timeout: 10 * time.Second}
	err := c.Call(context.Background(), "mpm_memory", "challenge", payload())
	if err == nil {
		t.Fatal("Call succeeded with MPM_BIN pointing at a directory")
	}
	if !strings.Contains(err.Error(), "directory") {
		t.Errorf("error = %v, want it to say the path is a directory", err)
	}
}

// TestExecCLI_BareNameIsRejected pins that even an explicitly configured
// bare name is refused rather than handed to exec for PATH resolution.
func TestExecCLI_BareNameIsRejected(t *testing.T) {
	dir := t.TempDir()
	hostileLog := filepath.Join(dir, "hostile.log")
	hostileDir := t.TempDir()
	hostileMPM(t, hostileDir, hostileLog)
	t.Setenv("PATH", hostileDir)

	c := &ExecCLI{MPMPath: "mpm", Timeout: 10 * time.Second}
	err := c.Call(context.Background(), "mpm_memory", "challenge", payload())
	if err == nil {
		t.Fatal("Call accepted a bare \"mpm\" path")
	}
	if got := readLog(t, hostileLog); got != "" {
		t.Errorf("bare name was resolved through PATH: %q", got)
	}
}

func TestExecCLI_CancellationStillKillsSubprocess(t *testing.T) {
	dir := t.TempDir()
	// `exec` replaces the shell so the process exec.CommandContext kills IS
	// the sleeper. Without exec, the shell forks `sleep` and the killed
	// child's grandchild keeps the output pipe open, so the read blocks
	// until sleep exits and the test appears to hang.
	slow := filepath.Join(dir, "mpm")
	if err := os.WriteFile(slow, []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Setenv("MPM_BIN", slow)

	c := &ExecCLI{Timeout: 300 * time.Millisecond}
	start := time.Now()
	err := c.Call(context.Background(), "mpm_memory", "challenge", payload())
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Call returned no error; the timeout did not fire")
	}
	if elapsed > 10*time.Second {
		t.Errorf("Call took %v; the subprocess was not killed promptly", elapsed)
	}
}

// TestExecCLI_ResolutionIsStableAcrossCalls pins that one ExecCLI cannot use
// two different binaries within a cycle — the identity a cycle's findings are
// attributed to must not change mid-run.
func TestExecCLI_ResolutionIsStableAcrossCalls(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "calls.log")
	first := fakeMPM(t, dir, "first-mpm", log)
	c := &ExecCLI{MPMPath: first, Timeout: 10 * time.Second}

	if err := c.Call(context.Background(), "mpm_memory", "challenge", payload()); err != nil {
		t.Fatalf("first Call: %v", err)
	}
	// Repoint MPM_BIN at a different binary; the cached identity must win.
	t.Setenv("MPM_BIN", fakeMPM(t, dir, "second-mpm", log))
	if err := c.Call(context.Background(), "mpm_memory", "challenge", payload()); err != nil {
		t.Fatalf("second Call: %v", err)
	}

	got := readLog(t, log)
	if strings.Count(got, "second-mpm") != 0 {
		t.Errorf("second call used a different binary:\n%s", got)
	}
	if n := strings.Count(got, "call mpm_memory"); n != 2 {
		t.Errorf("call count = %d, want 2:\n%s", n, got)
	}
}
