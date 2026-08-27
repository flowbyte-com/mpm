package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestCLI_UnchangedAfterPhase1 is a regression test that verifies the core
// CLI commands still work after Phase 1 pointer architecture changes.
// These commands are exercised via the actual mpm binary to ensure the
// CLI surface is unchanged.
func TestCLI_UnchangedAfterPhase1(t *testing.T) {
	// Build a fresh mpm binary into a temp dir. The previous fallback
	// (../../bin/mpm then $PATH lookup) silently failed in CI — neither
	// path is guaranteed to exist, so every run failed with "executable
	// file not found in $PATH" instead of exercising the CLI surface.
	// Building on demand makes the test self-contained and CI-stable.
	binDir := t.TempDir()
	binPath := filepath.Join(binDir, "mpm")
	buildCmd := exec.Command("go", "build", "-tags", "fts5", "-o", binPath, ".")
	buildCmd.Dir = "." // cmd/mpm is the package dir
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("build mpm binary: %v\n%s", err, out)
	}

	// Create a temporary workspace to avoid polluting the real one.
	tmpDir := t.TempDir()
	workspace := filepath.Join(tmpDir, "workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	// Commands to test: some commands don't support --json, so we test with
	// and without flags as appropriate.
	commands := []struct {
		name        string
		args        []string
		expectEmpty bool // if true, command may return exit 1 with warnings but output should be non-empty
	}{
		{"help", []string{"help"}, false},
		{"version", []string{"version"}, false},
		// doctor runs but may exit 1 due to warnings (embeddings missing, etc.)
		// which is expected in a fresh temp workspace
		{"doctor", []string{"doctor"}, true},
	}

	for _, tc := range commands {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(binPath, tc.args...)
			cmd.Env = append(os.Environ(),
				"MPM_WORKSPACE="+workspace,
				"MPM_NO_AUTO_INIT=1",
			)
			out, err := cmd.CombinedOutput()
			output := string(out)

			// If expectEmpty is set, we accept exit 1 (due to warnings) but
			// output must still contain useful information.
			if tc.expectEmpty && err != nil {
				// Exit error is expected; check output is non-empty and contains expected text.
				if len(out) == 0 {
					t.Errorf("%s: expected non-empty output on exit 1, got empty", tc.name)
				}
				// For doctor, expect to see the Doctor report.
				if tc.name == "doctor" && !strings.Contains(output, "MPM") {
					t.Errorf("%s: expected 'MPM' in output, got: %s", tc.name, output)
				}
				return
			}

			if err != nil {
				t.Errorf("%s failed: %v\noutput: %s", tc.name, err, output)
				return
			}

			// Basic sanity: output should be non-empty.
			if len(out) == 0 {
				t.Errorf("%s: empty output", tc.name)
			}
		})
	}
}
