// namespace_cmds_test.go — the runtime namespaces' contract.
//
// WHAT THIS FILE PINS
// -------------------
// Three properties, each of which is a mistake an operator can make by
// typing the obvious thing:
//
//  1. DISCOVERY IS INERT. `mpm critic`, `mpm critic --help`,
//     `mpm scheduler`, `mpm scheduler --help` and `mpm critic version`
//     must never run an audit or start a daemon. The delegated binary
//     is replaced by a recording stub, so "did not run" is a fact about
//     argv, not an inference from output.
//
//  2. THE CHILD COMES FROM THIS BUNDLE. A decoy earlier on PATH must
//     never answer for the runtime the operator is running.
//
//  3. THE CHILD'S INTERFACE SURVIVES THE HOP. Argument order, stdin,
//     stdout, stderr and exit codes are the child's, not the
//     namespace's; a signal aimed at the wrapper reaches the child.
//
// The scheduler half is exercised against a MOCK systemctl/journalctl.
// Nothing in this file can change the real user services: the fakes are
// the only `systemctl` on the child's PATH.
package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/flowbyte-com/mpm/internal/testenv"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// namespaceBinaries are the four executables a real bundle carries.
var namespaceBinaries = []string{"mpm", "mpm-telemetry", "mpm-critic", "mpm-scheduler"}

// namespaceBundle lays out a temporary runtime bundle shaped like the
// installed one (<dir>/bin/{mpm,mpm-telemetry,mpm-critic,mpm-scheduler})
// and returns the directory holding mpm.
//
// The binaries are COPIED out of .build/bin rather than run in place so
// that the test can (a) replace a sibling with a recording stub and
// (b) prove resolution is relative to the running mpm. `make test`
// builds them first; a developer running `go test` alone skips.
func namespaceBundle(t *testing.T) string {
	t.Helper()
	src := mpmBinForTest()
	if src == "" {
		t.Skip("mpm binary not found; run `make build` first")
	}
	srcDir := filepath.Dir(src)
	dir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir bundle: %v", err)
	}
	for _, name := range namespaceBinaries {
		copyExecutable(t, filepath.Join(srcDir, name), filepath.Join(dir, name))
	}
	return dir
}

func copyExecutable(t *testing.T, src, dst string) {
	t.Helper()
	body, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read %s (run `make build`): %v", src, err)
	}
	if err := os.WriteFile(dst, body, 0o755); err != nil {
		t.Fatalf("write %s: %v", dst, err)
	}
}

// recordingStub replaces path with a shell script that appends its argv
// to record (one argument per line) and then runs tail.
func recordingStub(t *testing.T, path, record, tail string) {
	t.Helper()
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString(": > " + shellQuote(record) + "\n")
	b.WriteString("for a in \"$@\"; do printf '%s\\n' \"$a\" >> " + shellQuote(record) + "; done\n")
	if tail != "" {
		b.WriteString(tail)
		b.WriteString("\n")
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o755); err != nil {
		t.Fatalf("write stub %s: %v", path, err)
	}
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// readRecord returns the recorded argv, or nil when the stub never ran.
func readRecord(t *testing.T, record string) []string {
	t.Helper()
	body, err := os.ReadFile(record)
	if err != nil {
		return nil
	}
	lines := strings.Split(strings.TrimRight(string(body), "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil
	}
	return lines
}

// namespaceEnvExtra returns the entries this test adds on top of the
// sanctioned isolation.
//
// The BASE environment is testenv.WithExtra, not a hand-rolled one: it
// pins MPM_WORKSPACE and HOME to two independent temporary roots, blanks
// every provider key and disables the scheduler nudge. A subprocess that
// could open the MPM database must never inherit the developer's shell
// (internal/testenv.TestNoUnisolatedMPMSubprocess is the regression guard
// for exactly that).
//
// pathOverride, when non-empty, REPLACES the inherited PATH. Go uses the
// last value for a duplicated key, so appending is an override. Two tests
// need it: one puts a decoy earlier on PATH, the other removes a tool
// from it entirely.
func namespaceEnvExtra(pathOverride string) []string {
	extra := []string{"MPM_NO_AUTO_INIT=1", "MPM_LOG=error"}
	if pathOverride != "" {
		extra = append(extra, "PATH="+pathOverride)
	}
	return extra
}

type namespaceRun struct {
	stdout, stderr string
	code           int
}

func runNamespace(t *testing.T, binDir string, extraEnv []string, stdin string, args ...string) namespaceRun {
	t.Helper()
	cmd := exec.Command(filepath.Join(binDir, "mpm"), args...)
	cmd.Env = testenv.WithExtra(t, extraEnv...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out, errBuf strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	err := cmd.Run()
	code := 0
	if exitErr, ok := err.(*exec.ExitError); ok {
		code = exitErr.ExitCode()
	} else if err != nil {
		t.Fatalf("run %v: %v\n%s", args, err, errBuf.String())
	}
	return namespaceRun{stdout: out.String(), stderr: errBuf.String(), code: code}
}

// ---------------------------------------------------------------------------
// 1. same-bundle resolution
// ---------------------------------------------------------------------------

func TestSiblingBinaryFrom_ResolvesBesideTheExecutable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mpm-telemetry")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := siblingBinaryFrom(filepath.Join(dir, "mpm"), "mpm-telemetry")
	if err != nil {
		t.Fatalf("siblingBinaryFrom: %v", err)
	}
	if got != path {
		t.Errorf("resolved %q, want %q", got, path)
	}
}

func TestSiblingBinaryFrom_ReportsWhyItCouldNotResolve(t *testing.T) {
	dir := t.TempDir()
	subdir := filepath.Join(dir, "mpm-critic")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	notExec := filepath.Join(dir, "mpm-scheduler")
	if err := os.WriteFile(notExec, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name   string
		exe    string
		binary string
		want   string
	}{
		{"missing", filepath.Join(dir, "mpm"), "mpm-telemetry", "not found"},
		{"directory", filepath.Join(dir, "mpm"), "mpm-critic", "is a directory"},
		{"not executable", filepath.Join(dir, "mpm"), "mpm-scheduler", "is not executable"},
		{"empty executable path", "", "mpm-telemetry", "empty"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := siblingBinaryFrom(c.exe, c.binary)
			if err == nil {
				t.Fatalf("expected an error for %q/%q", c.exe, c.binary)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q does not mention %q", err.Error(), c.want)
			}
			// The message must name the path that was checked: an
			// incomplete bundle is diagnosed by where we looked.
			if c.exe != "" && !strings.Contains(err.Error(), filepath.Dir(c.exe)) {
				t.Errorf("error does not name the directory searched:\n%s", err.Error())
			}
		})
	}
}

func TestNamespace_DoesNotFallBackToPath(t *testing.T) {
	binDir := namespaceBundle(t)
	os.Remove(filepath.Join(binDir, "mpm-telemetry"))

	decoyDir := t.TempDir()
	decoy := filepath.Join(decoyDir, "mpm-telemetry")
	rec := filepath.Join(decoyDir, "record")
	recordingStub(t, decoy, rec, "echo DECOY-ANSWERED; exit 0")

	run := runNamespace(t, binDir, namespaceEnvExtra(decoyDir+":/usr/bin:/bin"), "", "telemetry")
	if run.code == 0 {
		t.Fatalf("a missing sibling must not be answered from PATH:\n%s", run.stdout+run.stderr)
	}
	if strings.Contains(run.stdout, "DECOY-ANSWERED") {
		t.Fatal("the PATH decoy answered for this runtime's telemetry sidecar")
	}
	if !strings.Contains(run.stderr, "not available beside this mpm") {
		t.Errorf("the error must explain the same-bundle rule:\n%s", run.stderr)
	}
	if readRecord(t, rec) != nil {
		t.Error("the PATH decoy was executed")
	}
}

func TestNamespace_UsesTheSiblingFromThisBundle(t *testing.T) {
	binDir := namespaceBundle(t)
	rec := filepath.Join(t.TempDir(), "telemetry-argv")
	recordingStub(t, filepath.Join(binDir, "mpm-telemetry"), rec, "echo FROM-THIS-BUNDLE; exit 0")

	decoyDir := t.TempDir()
	recordingStub(t, filepath.Join(decoyDir, "mpm-telemetry"), filepath.Join(decoyDir, "r"), "echo DECOY; exit 0")

	run := runNamespace(t, binDir, namespaceEnvExtra(decoyDir+":/usr/bin:/bin"), "", "telemetry", "version")
	if run.code != 0 {
		t.Fatalf("exit %d:\n%s", run.code, run.stderr)
	}
	if !strings.Contains(run.stdout, "FROM-THIS-BUNDLE") {
		t.Errorf("the sibling beside this mpm did not answer:\n%s", run.stdout)
	}
	// Two releases can be installed at once; the answer must come from
	// the bundle the operator actually ran, never from PATH.
	if strings.Contains(run.stdout, "DECOY") {
		t.Errorf("the PATH decoy answered instead of this bundle's sibling:\n%s", run.stdout)
	}
	if got := readRecord(t, rec); len(got) != 1 || got[0] != "version" {
		t.Errorf("delegated argv = %q, want [version]", got)
	}
}

// ---------------------------------------------------------------------------
// 2. discovery is inert
// ---------------------------------------------------------------------------

func TestNamespace_CriticDoesNotAuditOnDiscovery(t *testing.T) {
	binDir := namespaceBundle(t)
	rec := filepath.Join(t.TempDir(), "critic-argv")
	recordingStub(t, filepath.Join(binDir, "mpm-critic"), rec, "exit 0")

	for _, args := range [][]string{
		{"critic"},
		{"critic", "--help"},
		{"critic", "-h"},
		{"critic", "help"},
		{"critic", "run", "--help"}, // help wins over an explicit run
	} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			os.Remove(rec)
			run := runNamespace(t, binDir, nil, "", args...)
			if run.code != 0 {
				t.Fatalf("exit %d:\n%s", run.code, run.stderr)
			}
			if trimmed := strings.TrimSpace(run.stdout); trimmed == "" {
				t.Error("a help request must print something")
			}
			if got := readRecord(t, rec); got != nil {
				t.Errorf("`mpm %s` executed the critic with %q; an audit must require `critic run`",
					strings.Join(args, " "), got)
			}
		})
	}
}

func TestNamespace_CriticRequiresExplicitRun(t *testing.T) {
	binDir := namespaceBundle(t)
	rec := filepath.Join(t.TempDir(), "critic-argv")
	recordingStub(t, filepath.Join(binDir, "mpm-critic"), rec, "exit 0")

	cases := []struct {
		args []string
		want []string
	}{
		// `run` is stripped: the critic's flag parser stops at the
		// first positional, so a forwarded "run" would swallow every
		// flag after it.
		{[]string{"critic", "run"}, []string{}},
		{[]string{"critic", "run", "--telemetry-hunt"}, []string{"--telemetry-hunt"}},
		{[]string{"critic", "run", "--telemetry-hunt", "--db", "/tmp/x.db"},
			[]string{"--telemetry-hunt", "--db", "/tmp/x.db"}},
		{[]string{"critic", "version"}, []string{"version"}},
	}
	for _, c := range cases {
		t.Run(strings.Join(c.args, "_"), func(t *testing.T) {
			os.Remove(rec)
			run := runNamespace(t, binDir, nil, "", c.args...)
			if run.code != 0 {
				t.Fatalf("exit %d:\n%s", run.code, run.stderr)
			}
			got := readRecord(t, rec)
			if strings.Join(got, " ") != strings.Join(c.want, " ") {
				t.Errorf("`mpm %s` delegated %q, want %q",
					strings.Join(c.args, " "), got, c.want)
			}
		})
	}
}

func TestNamespace_SchedulerNeverLaunchesTheDaemon(t *testing.T) {
	binDir := namespaceBundle(t)
	schedRec := filepath.Join(t.TempDir(), "scheduler-argv")
	recordingStub(t, filepath.Join(binDir, "mpm-scheduler"), schedRec, "echo DAEMON-STARTED; exit 0")

	ctlDir := t.TempDir()
	ctlRec := filepath.Join(ctlDir, "systemctl-argv")
	jrnRec := filepath.Join(ctlDir, "journalctl-argv")
	recordingStub(t, filepath.Join(ctlDir, "systemctl"), ctlRec, "exit 0")
	recordingStub(t, filepath.Join(ctlDir, "journalctl"), jrnRec, "exit 0")

	for _, args := range [][]string{
		{"scheduler"},
		{"scheduler", "--help"},
		{"scheduler", "help"},
		{"scheduler", "status"},
		{"scheduler", "logs"},
		{"scheduler", "start"},
		{"scheduler", "stop"},
		{"scheduler", "restart"},
	} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			os.Remove(schedRec)
			run := runNamespace(t, binDir, namespaceEnvExtra(ctlDir), "", args...)
			if strings.Contains(run.stdout, "DAEMON-STARTED") {
				t.Fatalf("`mpm %s` executed the scheduler binary", strings.Join(args, " "))
			}
			if got := readRecord(t, schedRec); got != nil {
				t.Errorf("`mpm %s` launched the daemon process with %q",
					strings.Join(args, " "), got)
			}
		})
	}

	// The one path that does execute it must be the read-only one.
	os.Remove(schedRec)
	if run := runNamespace(t, binDir, namespaceEnvExtra(ctlDir), "", "scheduler", "version"); run.code != 0 {
		t.Fatalf("scheduler version exited %d:\n%s", run.code, run.stderr)
	}
	if got := readRecord(t, schedRec); len(got) != 1 || got[0] != "version" {
		t.Errorf("scheduler version delegated %q, want [version]", got)
	}
}

func TestNamespace_TelemetryDiscoveryDoesNotStartACommand(t *testing.T) {
	binDir := namespaceBundle(t)
	rec := filepath.Join(t.TempDir(), "telemetry-argv")
	// A stub that records argv and treats anything as success. The
	// property under test is WHAT was asked for, not what it did.
	recordingStub(t, filepath.Join(binDir, "mpm-telemetry"), rec, "exit 0")

	for _, args := range [][]string{{"telemetry"}, {"telemetry", "--help"}, {"telemetry", "help"}} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			os.Remove(rec)
			run := runNamespace(t, binDir, nil, "", args...)
			if run.code != 0 {
				t.Fatalf("exit %d:\n%s", run.code, run.stderr)
			}
			for _, a := range readRecord(t, rec) {
				switch a {
				case "", "help", "--help", "-h":
					// a help request, not a command
				default:
					t.Fatalf("`mpm %s` asked the sidecar to run %q; discovery must not run a subcommand",
						strings.Join(args, " "), a)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 3. the child's interface survives the hop
// ---------------------------------------------------------------------------

func TestNamespace_TelemetryPreservesStreamsAndExitCode(t *testing.T) {
	binDir := namespaceBundle(t)
	recordingStub(t, filepath.Join(binDir, "mpm-telemetry"),
		filepath.Join(t.TempDir(), "argv"),
		`cat
echo "to-stdout"
echo "to-stderr" >&2
exit 7`)

	run := runNamespace(t, binDir, namespaceEnvExtra(""), "piped-stdin\n", "telemetry", "query", "invocation", "abc")
	if run.code != 7 {
		t.Errorf("exit = %d, want the child's 7", run.code)
	}
	if !strings.Contains(run.stdout, "piped-stdin") {
		t.Errorf("stdin did not reach the child:\n%s", run.stdout)
	}
	if !strings.Contains(run.stdout, "to-stdout") {
		t.Errorf("the child's stdout did not reach the caller:\n%s", run.stdout)
	}
	if !strings.Contains(run.stderr, "to-stderr") {
		t.Errorf("the child's stderr did not reach the caller:\n%s", run.stderr)
	}
}

func TestNamespace_TelemetryPassesEveryArgumentThrough(t *testing.T) {
	binDir := namespaceBundle(t)
	rec := filepath.Join(t.TempDir(), "argv")
	recordingStub(t, filepath.Join(binDir, "mpm-telemetry"), rec, "exit 0")

	args := []string{"telemetry", "query", "session", "--since", "2026-10-01", "--json"}
	if run := runNamespace(t, binDir, nil, "", args...); run.code != 0 {
		t.Fatalf("exit %d:\n%s", run.code, run.stderr)
	}
	want := []string{"query", "session", "--since", "2026-10-01", "--json"}
	if got := readRecord(t, rec); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("delegated %q, want %q", got, want)
	}
}

func TestNamespace_SignalReachesTheChild(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a child and signals it")
	}
	binDir := namespaceBundle(t)
	marker := filepath.Join(t.TempDir(), "caught")
	recordingStub(t, filepath.Join(binDir, "mpm-telemetry"),
		filepath.Join(t.TempDir(), "argv"),
		"trap 'printf caught > "+shellQuote(marker)+"; exit 130' INT\n"+
			"while :; do sleep 0.05; done")

	cmd := exec.Command(filepath.Join(binDir, "mpm"), "telemetry", "serve")
	cmd.Env = testenv.WithExtra(t, namespaceEnvExtra("")...)
	var out, errBuf strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = cmd.Process.Kill() }()

	// Let the shell install its trap before the signal is aimed at it.
	time.Sleep(time.Second)

	// Signal the WRAPPER only — the case a terminal Ctrl-C does not
	// cover (a supervisor stopping the namespace process by pid).
	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatalf("signal wrapper: %v", err)
	}
	err := cmd.Wait()
	if _, statErr := os.Stat(marker); statErr != nil {
		t.Fatalf("the signal did not reach the child (no marker, err=%v):\nstdout=%s\nstderr=%s",
			statErr, out.String(), errBuf.String())
	}
	code := 0
	if exitErr, ok := err.(*exec.ExitError); ok {
		code = exitErr.ExitCode()
	}
	if code != 130 {
		t.Errorf("wrapper exit = %d, want 130 (the child's interrupt status)", code)
	}
}

// ---------------------------------------------------------------------------
// 4. scheduler: systemd management, mocked
// ---------------------------------------------------------------------------

func TestNamespace_SchedulerDelegatesToSystemctl(t *testing.T) {
	binDir := namespaceBundle(t)
	ctlDir := t.TempDir()
	ctlRec := filepath.Join(ctlDir, "systemctl-argv")
	jrnRec := filepath.Join(ctlDir, "journalctl-argv")
	recordingStub(t, filepath.Join(ctlDir, "systemctl"), ctlRec, "exit 0")
	recordingStub(t, filepath.Join(ctlDir, "journalctl"), jrnRec, "exit 0")

	cases := []struct {
		args []string
		want []string
	}{
		{[]string{"scheduler", "status"}, []string{"--user", "status", schedulerUnit}},
		{[]string{"scheduler", "start"}, []string{"--user", "start", schedulerUnit}},
		{[]string{"scheduler", "stop"}, []string{"--user", "stop", schedulerUnit}},
		{[]string{"scheduler", "restart"}, []string{"--user", "restart", schedulerUnit}},
	}
	for _, c := range cases {
		t.Run(strings.Join(c.args, "_"), func(t *testing.T) {
			os.Remove(ctlRec)
			run := runNamespace(t, binDir, namespaceEnvExtra(ctlDir), "", c.args...)
			if run.code != 0 {
				t.Fatalf("exit %d:\n%s", run.code, run.stderr)
			}
			if got := readRecord(t, ctlRec); strings.Join(got, " ") != strings.Join(c.want, " ") {
				t.Errorf("systemctl argv = %q, want %q", got, c.want)
			}
		})
	}

	os.Remove(jrnRec)
	if run := runNamespace(t, binDir, namespaceEnvExtra(ctlDir), "", "scheduler", "logs"); run.code != 0 {
		t.Fatalf("logs exited %d:\n%s", run.code, run.stderr)
	}
	if got := readRecord(t, jrnRec); strings.Join(got, " ") != "--user -u "+schedulerUnit {
		t.Errorf("journalctl argv = %q, want [--user -u %s]", got, schedulerUnit)
	}
}

func TestNamespace_SchedulerStatusExitContract(t *testing.T) {
	binDir := namespaceBundle(t)
	ctlDir := t.TempDir()
	rec := filepath.Join(ctlDir, "systemctl-argv")

	cases := []struct {
		name       string
		systemdRC  int
		wantCode   int
		wantPhrase string
	}{
		// systemctl status: 0 running, 3 not running, 4 no such unit.
		{"running", 0, 0, ""},
		{"inactive is a successful inspection", 3, 0, ""},
		{"unit not installed", 4, 1, "is not installed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			recordingStub(t, filepath.Join(ctlDir, "systemctl"), rec,
				"exit "+strconv.Itoa(c.systemdRC))
			run := runNamespace(t, binDir, namespaceEnvExtra(ctlDir), "", "scheduler", "status")
			if run.code != c.wantCode {
				t.Errorf("exit = %d, want %d:\n%s", run.code, c.wantCode, run.stderr)
			}
			if c.wantPhrase != "" && !strings.Contains(run.stderr, c.wantPhrase) {
				t.Errorf("stderr does not explain the failure (%q):\n%s", c.wantPhrase, run.stderr)
			}
		})
	}
}

func TestNamespace_SchedulerMutationReportsAMissingUnit(t *testing.T) {
	binDir := namespaceBundle(t)
	ctlDir := t.TempDir()
	rec := filepath.Join(ctlDir, "systemctl-argv")

	for _, verb := range []string{"start", "stop", "restart"} {
		t.Run(verb, func(t *testing.T) {
			// systemctl exits 5 for all three verbs when the unit does
			// not exist.
			recordingStub(t, filepath.Join(ctlDir, "systemctl"), rec, "exit 5")
			run := runNamespace(t, binDir, namespaceEnvExtra(ctlDir), "", "scheduler", verb)
			if run.code == 0 {
				t.Fatalf("a missing unit must fail:\n%s", run.stdout+run.stderr)
			}
			if !strings.Contains(run.stderr, "is not installed") ||
				!strings.Contains(run.stderr, schedulerUnit) {
				t.Errorf("stderr must name the missing unit and say it is not installed:\n%s", run.stderr)
			}
			if !strings.Contains(run.stderr, "install.sh") {
				t.Errorf("stderr must say how to install it:\n%s", run.stderr)
			}
		})
	}
}

func TestNamespace_SchedulerWithoutSystemctl(t *testing.T) {
	binDir := namespaceBundle(t)
	// A PATH with no systemctl at all.
	emptyDir := t.TempDir()
	run := runNamespace(t, binDir, namespaceEnvExtra(emptyDir), "", "scheduler", "status")
	if run.code == 0 {
		t.Fatalf("status without systemctl must fail:\n%s", run.stdout)
	}
	if !strings.Contains(run.stderr, "systemctl is not on PATH") {
		t.Errorf("the error must name the missing tool:\n%s", run.stderr)
	}
}

func TestNamespace_SchedulerRejectsArguments(t *testing.T) {
	binDir := namespaceBundle(t)
	ctlDir := t.TempDir()
	rec := filepath.Join(ctlDir, "systemctl-argv")
	recordingStub(t, filepath.Join(ctlDir, "systemctl"), rec, "exit 0")

	// A forwarded token could turn `status` into something else, or aim
	// a service mutation at a different unit.
	for _, args := range [][]string{
		{"scheduler", "status", "mpm-telemetry.service"},
		{"scheduler", "logs", "--no-pager"},
		{"scheduler", "start", "--now"},
		{"scheduler", "restart", "--user"},
	} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			os.Remove(rec)
			run := runNamespace(t, binDir, namespaceEnvExtra(ctlDir), "", args...)
			if run.code == 0 {
				t.Fatalf("`mpm %s` must be rejected", strings.Join(args, " "))
			}
			if got := readRecord(t, rec); got != nil {
				t.Errorf("the rejected command still ran systemctl with %q", got)
			}
			if !strings.Contains(run.stderr, "takes no arguments") {
				t.Errorf("the rejection must say why:\n%s", run.stderr)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 5. catalogue integration
// ---------------------------------------------------------------------------

func TestNamespace_CommandsAreInTheCatalogue(t *testing.T) {
	r := NewRouter()
	for _, name := range []string{"telemetry", "critic", "scheduler"} {
		cmd, ok := r.Commands[name]
		if !ok {
			t.Errorf("%q is not registered in the router", name)
			continue
		}
		if cmd.Description == "" {
			t.Errorf("%q has no description; `mpm help --all` would show a blank line", name)
		}
		if cmd.Help == nil {
			t.Errorf("%q has no Help hook; `mpm %s --help` would print the generic page", name, name)
		}
	}
}

func TestNamespace_HelpAllListsTheNamespaces(t *testing.T) {
	binDir := namespaceBundle(t)
	run := runNamespace(t, binDir, nil, "", "help", "--all")
	if run.code != 0 {
		t.Fatalf("help --all exited %d:\n%s", run.code, run.stderr)
	}
	for _, name := range []string{"telemetry", "critic", "scheduler"} {
		if !strings.Contains(run.stdout, name) {
			t.Errorf("`mpm help --all` does not mention %q", name)
		}
	}
}

func TestNamespace_RegistryEntryIsOneLine(t *testing.T) {
	// The README/SPEC catalogue generator parses the r.Commands map with
	// go/ast and silently skips an entry that spans lines. The rule is
	// enforced here the same way onboard_cmds_test.go enforces it.
	path, err := filepath.Abs("router.go")
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"telemetry", "critic", "scheduler"} {
		// gofmt pads the key column, so match the shape rather than a
		// byte-exact prefix.
		re := regexp.MustCompile(`(?m)^\s*"` + name + `":\s+\{Name: "` + name + `"`)
		if !re.MatchString(string(body)) {
			t.Errorf("registry entry for %q must be one line beginning `\"%s\": {Name: \"%s\"`",
				name, name, name)
		}
	}
}
