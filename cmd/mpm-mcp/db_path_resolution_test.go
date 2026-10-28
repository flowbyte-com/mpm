// db_path_resolution_test.go — the D4 contract at the EXECUTABLE level.
//
// WHY THIS FILE EXISTS
//
// The D4 startup gates are wired into four binaries that open a
// database: mpm, mpm-scheduler, mpm-critic and mpm-mcp. Three of them
// had an executable-level proof (cmd/mpm/db_path_resolution_test.go and
// its siblings). The fourth — the MCP server — did not, and the
// resolver's unit tests in internal/core/config cannot substitute for
// it.
//
// WHY THE UNIT TESTS CANNOT SUBSTITUTE
//
// The defect these gates close lived in the WIRING, not in the
// resolver. A resolver that is correct and never called is not a fix.
// The MCP server is the surface where that difference is most costly:
// it is a long-running process that an agent host starts and leaves
// running, so a server that booted against the WRONG database would
// serve every subsequent tool call from it — silently, and for as long
// as the host keeps the process.
//
// THE CONTRACT (D4)
//
// MPM_WORKSPACE is the canonical workspace selector. MPM_DB_PATH may
// only RESTATE the workspace's own database. Anything else is an
// explicit conflict error that names both paths.
//
// A malformed configuration is a validation error, not a panic. The MCP
// server has no configuration-repair command, so there is nothing to
// exempt: it refuses to boot, naming the file that is wrong.
//
// THE ENVIRONMENT
//
// Every subprocess here runs with testenv.WithExtra(t, …), the
// repository's sanctioned hermetic environment. It pins MPM_WORKSPACE
// and HOME to temporary directories and does NOT inherit the ambient
// environment, so a developer's exported MPM_DB_PATH cannot reach the
// child and change the result. The repository-wide subprocess-isolation
// scanner (internal/testenv) enforces that rule over every test file
// in the tree, including this one.

package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/flowbyte-com/mpm/internal/testenv"
)

// buildMCP builds the mpm-mcp binary under test.
func buildMCP(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}
	bin := filepath.Join(t.TempDir(), "mpm-mcp")
	cmd := exec.Command("go", "build", "-tags", "fts5", "-o", bin, ".")
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("go build mpm-mcp: %v", err)
	}
	return bin
}

// runMCP starts the server and waits for it to exit — but not forever.
//
// stdin is closed immediately, so a server that gets PAST its gates
// exits on EOF instead of blocking forever on the stdio protocol. The
// exit status of that case is not asserted anywhere: what matters is
// whether the GATE fired, and the gate's message is distinctive.
//
// The wait is BOUNDED, and the process is killed when the bound
// expires: a server that did not refuse would otherwise sit there
// holding the test open until the package's timeout, which reads like
// an infrastructure failure rather than a missing refusal.
func runMCP(t *testing.T, cmd *exec.Cmd, limit time.Duration) (string, error, bool) {
	t.Helper()
	cmd.Stdin = strings.NewReader("")

	var buf syncBuffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Start(); err != nil {
		t.Fatalf("start mpm-mcp: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		return buf.String(), err, true
	case <-time.After(limit):
		_ = cmd.Process.Kill()
		<-done
		return buf.String(), nil, false
	}
}

// syncBuffer is a bytes.Buffer that is safe to read while a child
// process is writing to it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestD4_DivergentEnvOverrideIsRefused pins the headline case: an
// MPM_DB_PATH that names a database other than the workspace's own
// must stop the server from booting.
func TestD4_DivergentEnvOverrideIsRefused(t *testing.T) {
	bin := buildMCP(t)

	ws := t.TempDir()
	bad := filepath.Join(t.TempDir(), "other.db")

	cmd := exec.Command(bin)
	cmd.Env = testenv.WithExtra(t,
		"MPM_WORKSPACE="+ws,
		"MPM_DB_PATH="+bad)
	out, err, exited := runMCP(t, cmd, 120*time.Second)

	if !exited {
		t.Fatalf("the server did not refuse: it kept running, so the gate is missing\n%s", out)
	}
	if err == nil {
		t.Fatalf("a divergent MPM_DB_PATH must stop the server booting; it exited 0:\n%s", out)
	}
	msg := out
	if !strings.Contains(msg, "database path conflict") {
		t.Errorf("the refusal must say it is a database path conflict; got:\n%s", msg)
	}
	if !strings.Contains(msg, bad) {
		t.Errorf("the refusal must name the divergent path %q; got:\n%s", bad, msg)
	}
	canonical := filepath.Join(ws, "src", "db", "mpm.db")
	if !strings.Contains(msg, canonical) {
		t.Errorf("the refusal must name the workspace's own database %q; got:\n%s", canonical, msg)
	}
}

// TestD4_ConsistentEnvironmentIsAccepted is the control: the live
// installation's exact configuration must NOT be refused.
//
// The server may still exit non-zero afterwards — it reads EOF on
// stdin — so this test asserts on the absence of the gate's messages,
// not on the exit status.
func TestD4_ConsistentEnvironmentIsAccepted(t *testing.T) {
	bin := buildMCP(t)

	ws := t.TempDir()
	canonical := filepath.Join(ws, "src", "db", "mpm.db")

	cmd := exec.Command(bin)
	cmd.Env = testenv.WithExtra(t,
		"MPM_WORKSPACE="+ws,
		"MPM_DB_PATH="+canonical)
	out, _, _ := runMCP(t, cmd, 120*time.Second)

	if strings.Contains(out, "database path conflict") {
		t.Fatalf("a consistent MPM_DB_PATH must be accepted; got:\n%s", out)
	}
}

// TestD4_MalformedConfigurationIsRefusedNotPanicked pins the second
// gate: a configuration file that does not unmarshal used to take the
// server down with a SEGMENTATION FAULT, because the load error was
// discarded and the nil config was dereferenced while resolving the
// storage paths.
func TestD4_MalformedConfigurationIsRefusedNotPanicked(t *testing.T) {
	bin := buildMCP(t)

	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "mpm_config.json"),
		[]byte(`{"components": {"memory": true}}`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cmd := exec.Command(bin)
	cmd.Env = testenv.WithExtra(t, "MPM_WORKSPACE="+ws)
	out, err, exited := runMCP(t, cmd, 120*time.Second)

	if !exited {
		t.Fatalf("the server did not refuse: it kept running, so the gate is missing\n%s", out)
	}
	if err == nil {
		t.Fatalf("a malformed configuration must stop the server booting; it exited 0:\n%s", out)
	}
	msg := out
	if strings.Contains(msg, "panic") || strings.Contains(msg, "SIGSEGV") {
		t.Errorf("a malformed configuration must be a validation error, not a panic; got:\n%s", msg)
	}
	if !strings.Contains(msg, "not valid JSON") {
		t.Errorf("the refusal must say the configuration is not valid JSON; got:\n%s", msg)
	}
	if !strings.Contains(msg, "mpm_config.json") {
		t.Errorf("the refusal must name the configuration file; got:\n%s", msg)
	}
}

// TestD4_VersionSurfaceStaysFreeOfTheGate pins that the version
// interception still runs BEFORE the gates: `mpm-mcp --version` must
// answer even when the configuration and the database selectors are
// both broken, because it is the command an operator runs when
// something is wrong.
func TestD4_VersionSurfaceStaysFreeOfTheGate(t *testing.T) {
	bin := buildMCP(t)

	ws := t.TempDir()
	bad := filepath.Join(t.TempDir(), "other.db")
	if err := os.WriteFile(filepath.Join(ws, "mpm_config.json"),
		[]byte(`{"components": {"memory": true}}`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cmd := exec.Command(bin, "--version")
	cmd.Env = testenv.WithExtra(t,
		"MPM_WORKSPACE="+ws,
		"MPM_DB_PATH="+bad)
	out, err := cmd.CombinedOutput()

	if err != nil {
		t.Fatalf("--version must succeed regardless of the configuration; got %v:\n%s", err, out)
	}
	msg := string(out)
	if !strings.Contains(msg, "commit:") {
		t.Errorf("--version must report the embedded build identity; got:\n%s", msg)
	}
	if strings.Contains(msg, "database path conflict") {
		t.Errorf("--version must not be gated on the database target; got:\n%s", msg)
	}
}
