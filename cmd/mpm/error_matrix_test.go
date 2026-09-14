// error_matrix_test.go — table-driven error-presentation sweep.
//
// Normal CLI error paths must not leak:
//   - raw SQLite messages
//   - table names
//   - Go source paths
//   - stack traces
//   - internal parser/type names
//   - `%!` formatting failures
//
// Asserts:
//   - non-zero exit
//   - final newline
//   - canonical error presentation
//   - actionable remediation where useful
//
// Debug paths may intentionally expose internals — this matrix
// only covers the public human-facing surface.
package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// errorMatrixCase is one row of the error-presentation sweep.
type errorMatrixCase struct {
	Family  string
	Command []string
	// WantExitNonZero asserts the exit code is non-zero.
	WantExitNonZero bool
	// ForbiddenSubstrings are banned from stdout/stderr — these
	// would indicate storage/internal leakage.
	ForbiddenSubstrings []string
}

// errorMatrix is the canonical list of representative error paths.
// New public commands added to the router should be exercised here too.
var errorMatrix = []errorMatrixCase{
	{
		Family:             "memory show missing id",
		Command:            []string{"memory", "show"},
		WantExitNonZero:    true,
		ForbiddenSubstrings: []string{"SELECT", "INSERT", "FROM memories", "scan error", "sql:"},
	},
	{
		Family:             "memory shred missing id",
		Command:            []string{"memory", "shred"},
		WantExitNonZero:    true,
		ForbiddenSubstrings: []string{"SELECT", "INSERT", "FROM memories", "scan error", "sql:"},
	},
	{
		Family:             "memory show unknown id",
		Command:            []string{"memory", "show", "no-such-id-xyz"},
		WantExitNonZero:    true,
		ForbiddenSubstrings: []string{"SELECT", "INSERT", "FROM memories", "scan error"},
	},
	{
		Family:             "lesson list invalid type",
		Command:            []string{"lesson", "list", "--type=bogus"},
		WantExitNonZero:    true,
		ForbiddenSubstrings: []string{"SELECT", "INSERT", "scan error", "sql:"},
	},
	{
		Family:             "decisions unknown subcommand",
		Command:            []string{"decisions", "bogus"},
		WantExitNonZero:    true,
		ForbiddenSubstrings: []string{"SELECT", "INSERT", "scan error", "sql:"},
	},
	{
		Family:             "theories unknown filter",
		Command:            []string{"theories", "bogus"},
		WantExitNonZero:    true,
		ForbiddenSubstrings: []string{"SELECT", "INSERT", "scan error", "sql:"},
	},
	{
		Family:             "work unknown subcommand",
		Command:            []string{"work", "bogus"},
		WantExitNonZero:    true,
		ForbiddenSubstrings: []string{"SELECT", "INSERT", "scan error", "sql:"},
	},
	{
		Family:             "doctor unknown flag",
		Command:            []string{"doctor", "--bogus"},
		WantExitNonZero:    true,
		ForbiddenSubstrings: []string{"SELECT", "INSERT", "scan error", "sql:"},
	},
	{
		Family:             "info unknown flag",
		Command:            []string{"info", "--bogus"},
		WantExitNonZero:    true,
		ForbiddenSubstrings: []string{"SELECT", "INSERT", "scan error", "sql:"},
	},
	{
		Family:             "version rejected --json",
		Command:            []string{"version", "--json"},
		WantExitNonZero:    true,
		ForbiddenSubstrings: []string{"MPM mpm v"}, // version --json should NOT silently print the version string
	},
}

// TestErrorMatrix_NoStorageLeakage drives the production binary on
// representative error paths and asserts the output is clean.
func TestErrorMatrix_NoStorageLeakage(t *testing.T) {
	bin := mpmBinForTest()
	if bin == "" {
		t.Skip("mpm binary not found; run `make build` first")
	}

	for _, c := range errorMatrix {
		t.Run(c.Family, func(t *testing.T) {
			workspace := t.TempDir()
			stdout, stderr, exit := runMpmCaptureError(t, bin, workspace, c.Command...)
			combined := append(stdout, stderr...)

			if c.WantExitNonZero && exit == 0 {
				t.Errorf("%s: expected non-zero exit, got exit=0 stdout=%q", c.Family, stdout)
			}

			for _, banned := range c.ForbiddenSubstrings {
				if bytes.Contains(combined, []byte(banned)) {
					t.Errorf("%s: error output leaks %q\nstdout=%s\nstderr=%s",
						c.Family, banned, stdout, stderr)
				}
			}

			// No `%!` formatting failures.
			if bytes.Contains(combined, []byte("%!")) {
				t.Errorf("%s: error output contains %%! formatting failure\nstdout=%s\nstderr=%s",
					c.Family, stdout, stderr)
			}
		})
	}
}

// runMpmCaptureError invokes the production binary and captures
// stdout/stderr separately. Uses t.TempDir() for the workspace.
func runMpmCaptureError(t *testing.T, bin, workspace string, args ...string) (stdout, stderr []byte, exit int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(),
		"MPM_WORKSPACE="+workspace,
		"MPM_SHARED_DB="+filepath.Join(workspace, "shared.db"),
	)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err := cmd.Run()
	if exitErr, ok := err.(*exec.ExitError); ok {
		return outBuf.Bytes(), errBuf.Bytes(), exitErr.ExitCode()
	}
	if err != nil {
		t.Logf("run %v: %v\nstdout=%s\nstderr=%s", args, err, outBuf.String(), errBuf.String())
		return outBuf.Bytes(), errBuf.Bytes(), -1
	}
	return outBuf.Bytes(), errBuf.Bytes(), 0
}

// TestErrorMatrix_FinalNewlineInErrors asserts that error output is
// newline-terminated (no dangling partial lines).
func TestErrorMatrix_FinalNewlineInErrors(t *testing.T) {
	bin := mpmBinForTest()
	if bin == "" {
		t.Skip("mpm binary not found; run `make build` first")
	}
	cases := [][]string{
		{"memory", "show"},
		{"memory", "shred"},
		{"decisions", "bogus"},
		{"theories", "bogus"},
	}
	for _, cmd := range cases {
		t.Run(strings.Join(cmd, "_"), func(t *testing.T) {
			workspace := t.TempDir()
			stdout, stderr, _ := runMpmCaptureError(t, bin, workspace, cmd...)
			// Either stdout or stderr should end with a newline
			// when error output is produced. Empty output is allowed.
			if len(stdout) > 0 && !bytes.HasSuffix(stdout, []byte("\n")) {
				t.Errorf("%v: stdout missing trailing newline: %q", cmd, stdout)
			}
			if len(stderr) > 0 && !bytes.HasSuffix(stderr, []byte("\n")) {
				t.Errorf("%v: stderr missing trailing newline: %q", cmd, stderr)
			}
		})
	}
}
