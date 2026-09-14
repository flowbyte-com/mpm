// cmd/mpm-telemetry/help_inert_test.go — regression coverage for the
// 2026-09-14 release-pass telemetry help-safety contract.
//
// Every help surface in this binary must:
//   1. Exit 0.
//   2. NOT require MPM_WORKSPACE / MPM_TELEMETRY_SOCKET to be set.
//   3. NOT connect to a socket, open a database, or start serve.
//   4. NOT perform any side effect on the operator's filesystem
//      beyond reading its own help page from stdout.
//
// The test runs the in-process run* dispatchers with a temporary
// environment (MPM_WORKSPACE unset, MPM_TELEMETRY_SOCKET unset)
// to confirm that the help request short-circuits BEFORE any
// workspace/socket/DB resolution.

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runTelemetryHelpInProcess invokes the help dispatch in-process
// (no `go run` overhead) and returns the captured stdout. It
// runs each help request with MPM_WORKSPACE explicitly UNSET so
// a successful exit proves the help path is inert.
func runTelemetryHelpInProcess(t *testing.T, args ...string) string {
	t.Helper()

	// Build the binary once per test run so the assertions can
	// exercise the full dispatcher (main entry, not just the
	// run* functions). The binary is built fresh in a temp
	// directory so tests are hermetic and parallel-safe.
	binPath := filepath.Join(t.TempDir(), "mpm-telemetry-test")
	build := exec.Command("go", "build", "-tags", "fts5", "-o", binPath, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	// Run with MPM_WORKSPACE explicitly unset. pre-fix the help
	// surface raised `MPM_WORKSPACE is required` because the
	// workspace check ran before the help short-circuit.
	cmd := exec.Command(binPath, args...)
	cmd.Env = []string{"PATH=" + lookupPath()}
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("mpm-telemetry %s: %v\noutput:\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// lookupPath returns a minimal env PATH so exec.Command can find
// the binary when invoked. Empty PATH breaks some go runtimes.
func lookupPath() string {
	if p, err := exec.LookPath("go"); err == nil {
		// Best-effort: PATH-with-only-go is enough for our
		// binary which doesn't need any external tools.
		_ = p
	}
	return "/usr/bin:/bin:/usr/local/go/bin"
}

// newDiscard was previously used to swallow stderr from child
// processes but caused the `exec: Stderr already set` panic when
// version_stamping_test.go also assigns Stderr. The exec
// package's CombineOutput captures both stdout and stderr, so
// we no longer need a separate discard writer.

// TestTelemetry_Help_TopLevel pins `mpm-telemetry --help`:
// exits 0, prints the canonical heading, lists every operator
// subcommand, and uses the canonical `MPM · Telemetry` heading
// (matching the main mpm binary's visual grammar without code
// sharing).
func TestTelemetry_Help_TopLevel(t *testing.T) {
	for _, flag := range []string{"--help", "-h", "help"} {
		t.Run(flag, func(t *testing.T) {
			out := runTelemetryHelpInProcess(t, flag)
			if !strings.HasPrefix(out, "MPM · Telemetry") {
				t.Fatalf("top-level help must start with `MPM · Telemetry`; got:\n%s", out)
			}
			for _, sub := range []string{"serve", "ping", "query", "cost", "observe"} {
				if !strings.Contains(out, sub) {
					t.Errorf("top-level help missing subcommand %q in:\n%s", sub, out)
				}
			}
		})
	}
}

// TestTelemetry_Help_Serve asserts the serve subcommand's help
// exits 0 with no side effect.
func TestTelemetry_Help_Serve(t *testing.T) {
	for _, flag := range []string{"--help", "-h"} {
		t.Run(flag, func(t *testing.T) {
			out := runTelemetryHelpInProcess(t, "serve", flag)
			if !strings.HasPrefix(out, "MPM · Telemetry · serve") {
				t.Fatalf("serve help must start with `MPM · Telemetry · serve`; got:\n%s", out)
			}
			if !strings.Contains(out, "Run the telemetry collector") {
				t.Errorf("serve help must describe its function; got:\n%s", out)
			}
		})
	}
}

// TestTelemetry_Help_Ping asserts the ping subcommand's help
// exits 0 without requiring MPM_WORKSPACE. Pre-fix the help
// request short-circuited to `MPM_WORKSPACE is required`,
// which was the headline defect this release-pass closed.
func TestTelemetry_Help_Ping(t *testing.T) {
	for _, flag := range []string{"--help", "-h"} {
		t.Run(flag, func(t *testing.T) {
			out := runTelemetryHelpInProcess(t, "ping", flag)
			if !strings.HasPrefix(out, "MPM · Telemetry · ping") {
				t.Fatalf("ping help must start with `MPM · Telemetry · ping`; got:\n%s", out)
			}
			if strings.Contains(out, "MPM_WORKSPACE is required") {
				t.Fatalf("ping help must not require MPM_WORKSPACE; got:\n%s", out)
			}
		})
	}
}

// TestTelemetry_Help_Query asserts the query subcommand's help
// exits 0 across all three subcommands and without workspace
// validation.
func TestTelemetry_Help_Query(t *testing.T) {
	for _, flag := range []string{"--help", "-h"} {
		t.Run(flag, func(t *testing.T) {
			out := runTelemetryHelpInProcess(t, "query", flag)
			if !strings.HasPrefix(out, "MPM · Telemetry · query") {
				t.Fatalf("query help must start with `MPM · Telemetry · query`; got:\n%s", out)
			}
			if strings.Contains(out, "MPM_WORKSPACE is required") {
				t.Fatalf("query help must not require MPM_WORKSPACE; got:\n%s", out)
			}
		})
	}
	// Nested form: `mpm-telemetry query invocation --help` — the
	// release-pass brief lists this surface explicitly. Help must
	// also be inert on the nested form.
	t.Run("invocation_help", func(t *testing.T) {
		out := runTelemetryHelpInProcess(t, "query", "invocation", "--help")
		if !strings.HasPrefix(out, "MPM · Telemetry · query") {
			t.Fatalf("query invocation --help must render query help; got:\n%s", out)
		}
	})
}

// TestTelemetry_Help_Cost asserts the cost subcommand's help
// exits 0 without requiring `--pricing` or workspace. Pre-fix
// the help path raised `--pricing is required`.
func TestTelemetry_Help_Cost(t *testing.T) {
	for _, flag := range []string{"--help", "-h"} {
		t.Run(flag, func(t *testing.T) {
			out := runTelemetryHelpInProcess(t, "cost", flag)
			if !strings.HasPrefix(out, "MPM · Telemetry · cost") {
				t.Fatalf("cost help must start with `MPM · Telemetry · cost`; got:\n%s", out)
			}
			if strings.Contains(out, "is required") {
				t.Fatalf("cost help must not require any flag; got:\n%s", out)
			}
		})
	}
}

// TestTelemetry_Help_Observe asserts the observe subcommand's
// help exits 0 without side effects.
func TestTelemetry_Help_Observe(t *testing.T) {
	for _, flag := range []string{"--help", "-h"} {
		t.Run(flag, func(t *testing.T) {
			out := runTelemetryHelpInProcess(t, "observe", flag)
			if !strings.HasPrefix(out, "MPM · Telemetry · observe") {
				t.Fatalf("observe help must start with `MPM · Telemetry · observe`; got:\n%s", out)
			}
		})
	}
}

// TestTelemetry_Help_NoSideEffect asserts that running every
// help surface leaves no files in the operator's working
// directory. The brief requires: "Help must not connect to
// sockets, open databases, require MPM_WORKSPACE, or start
// serve." This test runs each help command in a fresh temp
// directory (cwd) and asserts no files were created beyond
// the implicit `.` directory entries.
func TestTelemetry_Help_NoSideEffect(t *testing.T) {
	binPath := filepath.Join(t.TempDir(), "mpm-telemetry-test")
	build := exec.Command("go", "build", "-tags", "fts5", "-o", binPath, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	for _, args := range [][]string{
		{"--help"},
		{"-h"},
		{"help"},
		{"serve", "--help"},
		{"serve", "-h"},
		{"ping", "--help"},
		{"ping", "-h"},
		{"query", "--help"},
		{"query", "-h"},
		{"query", "invocation", "--help"},
		{"cost", "--help"},
		{"cost", "-h"},
		{"observe", "--help"},
		{"observe", "-h"},
	} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			// Use a temp dir as cwd so any accidental file
			// creation surfaces in the listing.
			wd := t.TempDir()
			cmd := exec.Command(binPath, args...)
			cmd.Dir = wd
			cmd.Env = []string{"PATH=" + lookupPath()}
			if err := cmd.Run(); err != nil {
				t.Fatalf("mpm-telemetry %s: %v", strings.Join(args, " "), err)
			}
			entries, err := readDir(wd)
			if err != nil {
				t.Fatalf("readDir: %v", err)
			}
			if len(entries) != 0 {
				t.Fatalf("help %s must not create files in cwd; found %d: %v",
					strings.Join(args, " "), len(entries), entries)
			}
		})
	}
}

// readDir is a small wrapper that returns only the names of
// non-hidden entries in dir. Used by TestTelemetry_Help_NoSideEffect
// to assert no file artifacts after a help command.
func readDir(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.Name() == "." || e.Name() == ".." {
			continue
		}
		out = append(out, e.Name())
	}
	return out, nil
}

// TestTelemetry_Version asserts the new `version` subcommand
// and the top-level `--version` flag print the linker-stamped
// build identity. The stamp must match the default `dev` value
// when the binary is built without explicit ldflags, and must
// match the stamped value when the binary is built with the
// canonical `-X main.buildVersion=...` flag.
func TestTelemetry_Version(t *testing.T) {
	// Build a fresh binary so we control the stamp. Default stamp
	// is "dev" so the assertions pin that fallback.
	binPath := filepath.Join(t.TempDir(), "mpm-telemetry-default")
	build := exec.Command("go", "build", "-tags", "fts5", "-o", binPath, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	// Subcommand form.
	out, err := exec.Command(binPath, "version").Output()
	if err != nil {
		t.Fatalf("mpm-telemetry version: %v", err)
	}
	if !strings.Contains(string(out), "dev") {
		t.Errorf("default-stamp binary `version` must contain `dev`; got:\n%s", out)
	}
	if !strings.Contains(string(out), "MPM · Telemetry · version") {
		t.Errorf("`version` output must use the canonical heading; got:\n%s", out)
	}

	// Top-level `--version` flag.
	out, err = exec.Command(binPath, "--version").Output()
	if err != nil {
		t.Fatalf("mpm-telemetry --version: %v", err)
	}
	if !strings.Contains(string(out), "dev") {
		t.Errorf("default-stamp binary `--version` must contain `dev`; got:\n%s", out)
	}
	if !strings.Contains(string(out), "MPM · Telemetry · version") {
		t.Errorf("`--version` output must use the canonical heading; got:\n%s", out)
	}

	// Build a stamped binary and re-run both surfaces; the
	// stamped identity must replace `dev`.
	const stamp = "vTEST-release-pass-2026-09-14"
	stampedPath := filepath.Join(t.TempDir(), "mpm-telemetry-stamped")
	stampedBuild := exec.Command("go", "build", "-tags", "fts5",
		"-ldflags", "-X main.buildVersion="+stamp,
		"-o", stampedPath, ".")
	if out, err := stampedBuild.CombinedOutput(); err != nil {
		t.Fatalf("stamped go build: %v\n%s", err, out)
	}

	for _, args := range [][]string{{"version"}, {"--version"}, {"-V"}} {
		out, err := exec.Command(stampedPath, args...).Output()
		if err != nil {
			t.Fatalf("mpm-telemetry %s: %v", strings.Join(args, " "), err)
		}
		if !strings.Contains(string(out), stamp) {
			t.Errorf("stamped binary `mpm-telemetry %s` must report %q; got:\n%s",
				strings.Join(args, " "), stamp, out)
		}
	}
}

// TestTelemetry_NoFalseDefault asserts the top-level help page
// no longer claims `serve` is the default subcommand. The
// release-pass brief flagged the false "default if no
// subcommand" wording as a CLI inconsistency — bare
// `mpm-telemetry` is operator-facing help, not a serve
// invocation.
func TestTelemetry_NoFalseDefault(t *testing.T) {
	out := runTelemetryHelpInProcess(t, "--help")
	if strings.Contains(out, "default if no subcommand") {
		t.Fatalf("top-level help must not advertise `serve` as a default; got:\n%s", out)
	}
	// Bare `mpm-telemetry` must now print the top-level help
	// page and exit 0 (pre-fix: exited 2 with usage on stderr).
	binPath := filepath.Join(t.TempDir(), "mpm-telemetry-test")
	build := exec.Command("go", "build", "-tags", "fts5", "-o", binPath, ".")
	if b, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, b)
	}
	cmd := exec.Command(binPath)
	cmd.Env = []string{"PATH=" + lookupPath()}
	outBytes, err := cmd.Output()
	if err != nil {
		t.Fatalf("bare `mpm-telemetry` must exit 0; got %v", err)
	}
	if !strings.HasPrefix(string(outBytes), "MPM · Telemetry") {
		t.Fatalf("bare `mpm-telemetry` must print top-level help; got:\n%s", outBytes)
	}
}
