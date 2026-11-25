package opencode

// Relay readiness: the production launcher must wait for the relay and must
// not run the target when the relay never becomes ready.
//
// These tests exercise the REAL containment (unshare + bubblewrap) and the
// REAL relay implementation. Two relays are used:
//
//   - the test binary acting as a relay helper (TestR3RelayHelperProcess),
//     which calls opencode.RunRelay directly and can be told to delay or to
//     die. It is a real separate process inside the namespace, and it runs
//     the same entry point cmd/mpm's RelayModeFromEnv branch runs.
//   - the compiled cmd/mpm binary, built once by relayBinary, used by
//     TestLauncherWithRealMPMBinary so at least one test runs the shipped
//     binary end to end.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// TestR3RelayHelperProcess is not a test in the parent process. When the
// launcher starts the relay inside the namespace it passes R3_RELAY_HELPER=1
// and -test.run=^TestR3RelayHelperProcess$, so this function IS the relay.
func TestR3RelayHelperProcess(t *testing.T) {
	if os.Getenv("R3_RELAY_HELPER") != "1" {
		return
	}
	if os.Getenv("R3_RELAY_DIE") == "1" {
		fmt.Fprintln(os.Stderr, "r3 relay helper: instructed to fail")
		os.Exit(1)
	}
	if ms := os.Getenv("R3_RELAY_DELAY_MS"); ms != "" {
		var d int
		_, _ = fmt.Sscanf(ms, "%d", &d)
		time.Sleep(time.Duration(d) * time.Millisecond)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := RunRelay(ctx, RelayConfig{UpstreamSocket: os.Getenv(RelayEnvVar)}); err != nil {
		fmt.Fprintln(os.Stderr, "r3 relay helper:", err)
		os.Exit(1)
	}
	os.Exit(0)
}

// relayBinary builds the real cmd/mpm binary once per test process and
// returns its path. It is what the production launcher names as MPMBin.
func relayBinary(t *testing.T) string {
	t.Helper()
	relayOnce.Do(func() {
		if _, err := exec.LookPath("go"); err != nil {
			relayErr = fmt.Errorf("the go tool is not available: %w", err)
			return
		}
		_, file, _, ok := runtime.Caller(0)
		if !ok {
			relayErr = errors.New("cannot locate this test's source file")
			return
		}
		root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", ".."))
		dir, err := os.MkdirTemp("", "r3-relay-")
		if err != nil {
			relayErr = err
			return
		}
		out := filepath.Join(dir, "mpm")
		cmd := exec.Command("go", "build", "-tags", "fts5", "-o", out, "./cmd/mpm")
		cmd.Dir = root
		cmd.Env = os.Environ()
		if b, err := cmd.CombinedOutput(); err != nil {
			relayErr = fmt.Errorf("build cmd/mpm: %w: %s", err, b)
			return
		}
		relayPath = out
	})
	if relayErr != nil {
		// An environment without a usable Go toolchain cannot build the
		// shipped binary. That is an environmental limitation, and it is
		// reported rather than hidden.
		t.Skipf("the real MPM relay binary cannot be built here: %v", relayErr)
	}
	return relayPath
}

var (
	relayOnce sync.Once
	relayPath string
	relayErr  error
)

// requireContainment skips ONLY for a narrowly defined, independently
// verified environmental limitation. Any other failure is a test failure.
func requireContainment(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skip("bubblewrap is not installed on this host")
	}
	if _, err := exec.LookPath("unshare"); err != nil {
		t.Skip("util-linux unshare is not installed on this host")
	}
	probe := exec.Command("unshare", "--user", "--map-root-user", "--net", "--", "/bin/true")
	if out, err := probe.CombinedOutput(); err != nil {
		t.Skipf("this host cannot create unprivileged user+network namespaces: %v: %s", err, out)
	}
}

// sidecar is a disposable local inference-sidecar stand-in. It accepts one
// connection at a time, records the request, and answers a fixed, local
// response. No provider, no credential, no network.
type sidecar struct {
	ln      net.Listener
	mu      chan struct{}
	request string
}

func startSidecar(t *testing.T) (*sidecar, string) {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "sidecar.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := &sidecar{ln: ln, mu: make(chan struct{}, 1)}
	s.mu <- struct{}{}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 4096)
				_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
				n, _ := c.Read(buf)
				<-s.mu
				s.request = string(buf[:n])
				s.mu <- struct{}{}
				// A single marker line after the status line: the target
				// reads exactly two lines and does not wait for EOF.
				_, _ = c.Write([]byte("HTTP/1.1 200 OK\r\nR3-SIDECAR-MARKER\r\n\r\n"))
			}(c)
		}
	}()
	return s, sock
}

func (s *sidecar) sawRequest() string {
	<-s.mu
	defer func() { s.mu <- struct{}{} }()
	return s.request
}

// relaySpecWith builds the containment exactly as Runtime.Launch does, with
// the relay binary and relay argv supplied by the caller.
func relaySpecWith(t *testing.T, sock, mpmBin string, relayArgs []string, extraEnv map[string]string, command []string) Spec {
	t.Helper()
	root := filepath.Join(t.TempDir(), "root")
	ws := filepath.Join(t.TempDir(), "ws")
	if err := os.MkdirAll(ws, 0o700); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{
		"HOME": PrefixHome, "TMPDIR": PrefixTmp, "PATH": sandboxPath,
		"XDG_CONFIG_HOME": PrefixOCfg, "XDG_DATA_HOME": PrefixOData,
		"XDG_CACHE_HOME": PrefixOCache, "XDG_STATE_HOME": PrefixOState,
		"LANG": "C.UTF-8", "TERM": "dumb",
		RelayEnvVar: PrefixSocket,
	}
	for k, v := range extraEnv {
		env[k] = v
	}
	spec := Spec{
		Root:          root,
		Workspace:     ws,
		OpenCodeBin:   mpmBin,
		MPMBin:        mpmBin,
		SidecarSocket: sock,
		Env:           env,
		RelayCommand:  relayArgs,
		Command:       command,
	}
	if err := BuildTree(spec); err != nil {
		t.Fatalf("BuildTree: %v", err)
	}
	return spec
}

// helperRelay is the RelayCommand for the test-binary relay.
func helperRelay() []string {
	return []string{"/opt/mpm/mpm", "-test.run=^TestR3RelayHelperProcess$"}
}

// helpersEnv marks the test binary as the relay.
func helpersEnv(extra map[string]string) map[string]string {
	out := map[string]string{"R3_RELAY_HELPER": "1"}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

// runSpec executes the launcher argv and returns its combined output and the
// wall-clock duration.
func runSpec(t *testing.T, spec Spec, timeout time.Duration) (string, time.Duration, error) {
	t.Helper()
	argv, err := spec.Argv()
	if err != nil {
		t.Fatalf("Argv: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = []string{
		"PATH=" + sandboxPath,
		"HOME=" + filepath.Join(spec.Root, "home"),
		// The containment REPLACES this environment inside the namespace
		// (--clearenv + --setenv), so nothing here reaches the sandbox.
		// MPM_WORKSPACE is pinned because the repository-wide subprocess
		// guard (internal/testenv) requires every test subprocess to be
		// isolated from the operator's real workspace.
		"MPM_WORKSPACE=" + t.TempDir(),
	}
	start := time.Now()
	out, err := cmd.CombinedOutput()
	return string(out), time.Since(start), err
}

// probeCommand speaks HTTP to the relay and records what it saw in the
// sandbox's findings directory (host-visible at <root>/findings).
const probeCommand = `set -u
mkdir -p /sandbox/findings
exec 3<>/dev/tcp/127.0.0.1/18777 || { echo CONNECT-FAIL > /sandbox/findings/verdict; exit 3; }
printf 'GET /v1/probe HTTP/1.1\r\nHost: relay\r\n\r\n' >&3
IFS= read -r -t 3 status <&3 || { echo NO-STATUS > /sandbox/findings/verdict; exit 4; }
IFS= read -r -t 3 marker <&3 || { echo NO-MARKER > /sandbox/findings/verdict; exit 5; }
printf '%s\n%s\n' "$status" "$marker" > /sandbox/findings/response
case "$marker" in *R3-SIDECAR-MARKER*) echo OK > /sandbox/findings/verdict ;;
  *) echo BAD > /sandbox/findings/verdict ;; esac
`

func readFinding(spec Spec, name string) string {
	b, err := os.ReadFile(filepath.Join(spec.Root, "findings", name))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// assertNoResidue fails if any process still names this sandbox root: the
// bwrap argv carries every bind path, so a surviving process is visible.
func assertNoResidue(t *testing.T, root string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		var found []string
		entries, _ := os.ReadDir("/proc")
		for _, e := range entries {
			if _, err := fmt.Sscanf(e.Name(), "%d", new(int)); err != nil {
				continue
			}
			b, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
			if err != nil || len(b) == 0 {
				continue
			}
			if strings.Contains(string(b), root) {
				found = append(found, e.Name())
			}
		}
		if len(found) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("processes still reference the sandbox after the launch: %v", found)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestContainmentReadinessProbeUsesHostPortForm pins the address form the
// readiness probe must use. bash's /dev/tcp takes host/port; host:port is
// treated as a filename.
func TestContainmentReadinessProbeUsesHostPortForm(t *testing.T) {
	spec := Spec{
		Root: "/tmp/r3-root", Workspace: "/tmp/r3-ws",
		OpenCodeBin:   "/bin/true",
		MPMBin:        "/bin/true",
		SidecarSocket: "/tmp/r3-sidecar.sock",
		Env:           map[string]string{"HOME": PrefixHome, RelayEnvVar: PrefixSocket},
		RelayCommand:  []string{"/opt/mpm/mpm"},
		Command:       []string{"/bin/true"},
	}
	argv, err := spec.Argv()
	if err != nil {
		t.Fatalf("Argv: %v", err)
	}
	script := argv[len(argv)-1]
	if !strings.Contains(script, "/dev/tcp/127.0.0.1/18777") {
		t.Errorf("the readiness probe does not use bash's host/port form.\nwant: /dev/tcp/127.0.0.1/18777\ngot script: %s", script)
	}
	if strings.Contains(script, "/dev/tcp/127.0.0.1:18777") {
		t.Errorf("the readiness probe uses the invalid host:port form: %s", script)
	}
	// The probe must be bounded per attempt, and readiness must gate the
	// target rather than precede it unconditionally.
	if !strings.Contains(script, "timeout 0.25 bash -c") {
		t.Errorf("each probe attempt is not bounded: %s", script)
	}
	// The target is exec'd only after the readiness branch. Compare against
	// the TARGET's own exec, not the first "exec" in the script (the probe
	// itself contains one).
	// The target is exec'd only after the readiness branch. The target's is
	// the LAST exec in the script (the probe contains one too, and the outer
	// script embeds the inner one shell-quoted).
	if i, j := strings.Index(script, "exit 70"), strings.LastIndex(script, "exec "); i < 0 || j < 0 || i > j {
		t.Errorf("the readiness failure branch does not precede the target: %s", script)
	}
}

// TestLauncherReadinessIsPromptAndExecutes is the real end-to-end success
// case: containment, relay, sidecar, and a target that talks through them.
func TestLauncherReadinessIsPromptAndExecutes(t *testing.T) {
	requireContainment(t)
	sc, sock := startSidecar(t)
	spec := relaySpecWith(t, sock, os.Args[0], helperRelay(), helpersEnv(nil),
		[]string{"/usr/bin/bash", "-c", probeCommand})

	out, took, err := runSpec(t, spec, 60*time.Second)
	t.Logf("healthy relay: launch took %s", took)
	if err != nil {
		t.Fatalf("the launcher failed: %v\n%s", err, out)
	}
	if got := readFinding(spec, "verdict"); got != "OK" {
		t.Fatalf("the target did not reach the sidecar through the relay: verdict=%q response=%q\n%s",
			got, readFinding(spec, "response"), out)
	}
	if req := sc.sawRequest(); !strings.Contains(req, "GET /v1/probe") {
		t.Errorf("the sidecar did not observe the target's request: %q", req)
	}
	// A prompt readiness is sub-second. The old behaviour always consumed
	// the whole 10 s budget. 5 s separates them without being fragile.
	if took > 5*time.Second {
		t.Errorf("a healthy relay still cost the readiness timeout: %s", took)
	}
	assertNoResidue(t, spec.Root)
}

// TestLauncherWithRealMPMBinary runs the SHIPPED binary as the relay, in real
// containment, with a synthetic sidecar.
func TestLauncherWithRealMPMBinary(t *testing.T) {
	requireContainment(t)
	bin := relayBinary(t)
	sc, sock := startSidecar(t)
	spec := relaySpecWith(t, sock, bin, []string{"/opt/mpm/mpm"}, nil,
		[]string{"/usr/bin/bash", "-c", probeCommand})

	out, took, err := runSpec(t, spec, 60*time.Second)
	t.Logf("real mpm relay binary: launch took %s", took)
	if err != nil {
		t.Fatalf("the launcher failed with the real relay: %v\n%s", err, out)
	}
	if got := readFinding(spec, "verdict"); got != "OK" {
		t.Fatalf("the target did not reach the sidecar through the real relay: verdict=%q\n%s", got, out)
	}
	if req := sc.sawRequest(); !strings.Contains(req, "GET /v1/probe") {
		t.Errorf("the sidecar did not observe the target's request: %q", req)
	}
	assertNoResidue(t, spec.Root)
}

// TestLauncherDelayedReadiness covers a relay that becomes ready after a
// controlled delay: the target must wait for it and then run.
func TestLauncherDelayedReadiness(t *testing.T) {
	requireContainment(t)
	_, sock := startSidecar(t)
	spec := relaySpecWith(t, sock, os.Args[0], helperRelay(),
		helpersEnv(map[string]string{"R3_RELAY_DELAY_MS": "2000"}),
		[]string{"/usr/bin/bash", "-c", probeCommand})

	out, took, err := runSpec(t, spec, 60*time.Second)
	t.Logf("2 s delayed relay: launch took %s", took)
	if err != nil {
		t.Fatalf("the launcher failed: %v\n%s", err, out)
	}
	if got := readFinding(spec, "verdict"); got != "OK" {
		t.Fatalf("the target ran before the relay was ready: verdict=%q\n%s", got, out)
	}
	// It must wait for the relay (~2 s) and must NOT consume the whole budget.
	if took > 8*time.Second {
		t.Errorf("a 2 s relay delay consumed the readiness budget: %s", took)
	}
	if took < 1500*time.Millisecond {
		t.Errorf("the target ran before the relay could have been ready: %s", took)
	}
	assertNoResidue(t, spec.Root)
}

// TestLauncherFailsClosedWhenRelayNeverStarts is the R3b regression: the
// target must not execute when the relay never becomes ready.
func TestLauncherFailsClosedWhenRelayNeverStarts(t *testing.T) {
	requireContainment(t)
	_, sock := startSidecar(t)
	spec := relaySpecWith(t, sock, os.Args[0], helperRelay(),
		helpersEnv(map[string]string{"R3_RELAY_DIE": "1"}),
		[]string{"/usr/bin/bash", "-c", probeCommand})

	out, took, err := runSpec(t, spec, 60*time.Second)
	t.Logf("relay never starts: launch took %s", took)
	if err == nil {
		t.Fatalf("the launcher succeeded although the relay never started:\n%s", out)
	}
	if got := readFinding(spec, "verdict"); got != "" {
		t.Fatalf("the target EXECUTED although the relay never started: verdict=%q", got)
	}
	if got := readFinding(spec, "response"); got != "" {
		t.Fatalf("the target produced output although the relay never started: %q", got)
	}
	if !strings.Contains(out, "the relay did not become ready") {
		t.Errorf("the readiness failure was not reported: %q", out)
	}
	assertNoResidue(t, spec.Root)
}

// TestLauncherFailsClosedWhenRelayIsLate covers a relay that would become
// ready only after the budget: the launcher must give up, not proceed.
func TestLauncherFailsClosedWhenRelayIsLate(t *testing.T) {
	requireContainment(t)
	_, sock := startSidecar(t)
	spec := relaySpecWith(t, sock, os.Args[0], helperRelay(),
		helpersEnv(map[string]string{"R3_RELAY_DELAY_MS": "15000"}),
		[]string{"/usr/bin/bash", "-c", probeCommand})

	out, took, err := runSpec(t, spec, 60*time.Second)
	t.Logf("relay late: launch took %s", took)
	if err == nil {
		t.Fatalf("the launcher succeeded although the relay was not ready in time:\n%s", out)
	}
	if got := readFinding(spec, "verdict"); got != "" {
		t.Fatalf("the target EXECUTED although the relay was late: verdict=%q", got)
	}
	if took < 9*time.Second {
		t.Errorf("the readiness budget was not actually spent (%s); a faster failure here "+
			"would mean the probe or the loop changed in an unexpected way", took)
	}
	assertNoResidue(t, spec.Root)
}

// TestLauncherDoesNotLeakCallerEnvironment proves the correction introduces
// no new credential exposure: a marker credential in the caller's
// environment must not appear in any process inside the namespace.
func TestLauncherDoesNotLeakCallerEnvironment(t *testing.T) {
	requireContainment(t)
	const marker = "R3-SYNTHETIC-CREDENTIAL-MARKER"
	t.Setenv("R3_LEAK_MARKER", marker)

	_, sock := startSidecar(t)
	cmd := `set -u
mkdir -p /sandbox/findings
found=0
for p in /proc/[0-9]*; do
  if tr '\0' '\n' < "$p/environ" 2>/dev/null | grep -q '` + marker + `'; then found=1; fi
done
echo "$found" > /sandbox/findings/leak
tr '\0' '\n' < /proc/1/environ > /sandbox/findings/pid1
`
	spec := relaySpecWith(t, sock, os.Args[0], helperRelay(), helpersEnv(nil),
		[]string{"/usr/bin/bash", "-c", cmd})
	out, _, err := runSpec(t, spec, 60*time.Second)
	if err != nil {
		t.Fatalf("the launcher failed: %v\n%s", err, out)
	}
	if got := readFinding(spec, "leak"); got != "0" {
		t.Fatalf("a caller-environment credential marker reached the namespace (leak=%q)\n%s", got, out)
	}
	// The relay receives exactly the sanctioned environment and no marker.
	if got := readFinding(spec, "pid1"); strings.Contains(got, marker) {
		t.Fatalf("the marker appears in the namespace's init environment:\n%s", got)
	}
}

// TestLauncherCancellationLeavesNoResidue cancels a running launch and
// requires the containment to take the relay with it.
func TestLauncherCancellationLeavesNoResidue(t *testing.T) {
	requireContainment(t)
	_, sock := startSidecar(t)
	spec := relaySpecWith(t, sock, os.Args[0], helperRelay(), helpersEnv(nil),
		[]string{"/usr/bin/bash", "-c", "mkdir -p /sandbox/findings; echo started > /sandbox/findings/started; sleep 60"})

	argv, err := spec.Argv()
	if err != nil {
		t.Fatalf("Argv: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	c := exec.CommandContext(ctx, argv[0], argv[1:]...)
	c.Env = []string{
		"PATH=" + sandboxPath,
		"HOME=" + filepath.Join(spec.Root, "home"),
		// The containment REPLACES this environment inside the namespace
		// (--clearenv + --setenv), so nothing here reaches the sandbox.
		// MPM_WORKSPACE is pinned because the repository-wide subprocess
		// guard (internal/testenv) requires every test subprocess to be
		// isolated from the operator's real workspace.
		"MPM_WORKSPACE=" + t.TempDir(),
	}
	if err := c.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	// Wait until the target is genuinely running, then cancel.
	deadline := time.Now().Add(15 * time.Second)
	for readFinding(spec, "started") == "" && time.Now().Before(deadline) {
		time.Sleep(25 * time.Millisecond)
	}
	if readFinding(spec, "started") == "" {
		cancel()
		_ = c.Wait()
		t.Fatalf("the target never started")
	}
	cancel()
	_ = c.Wait()
	assertNoResidue(t, spec.Root)
}
