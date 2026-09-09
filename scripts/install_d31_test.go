// install_d31_test.go — alpha-5 D-3.1 regression.
//
// D-3.1: install.sh's post-install validation step used the wrong
// form — `mpm call health_check` (literal action name). The mpm call
// interface is the universal CLI for the MCP-prefixed tool names
// (`mpm_system`, `mpm_memory`, etc.). The action lives inside the
// payload: `mpm call mpm_system --payload '{"action":"health_check"}'`.
//
// The pre-fix form returned an "unknown tool" error from mpm call
// (because no `health_check` tool exists; health_check is an action
// of mpm_system). The fix is a one-liner; this test pins it so a
// future refactor of the install script can't silently regress it.
//
// We run this test as part of the Go test suite (scripts/ has a
// trivial go.mod just to make the test runner pick it up). The test
// only inspects the install.sh script's source text — it does not
// execute the script (which would require a live install and a real
// daemon, neither of which is appropriate for a unit test).

package scripts

import (
	"os"
	"strings"
	"testing"
)

// TestInstallSh_HealthCheckValidationForm pins the literal command
// line used by install.sh's CLI-wrapper validation step. The canonical
// form is:
//
//	"$PREFIX/bin/mpm" call mpm_system --payload '{"action":"health_check"}'
//
// Pre-fix this was `mpm call health_check` which the call dispatcher
// rejects (no such tool). We allow `mpm` instead of `$PREFIX/bin/mpm`
// for resilience to the wrapper-script-prefix change in D-3.2.
func TestInstallSh_HealthCheckValidationForm(t *testing.T) {
	data, err := os.ReadFile("../scripts/install.sh")
	if err != nil {
		// Try a sibling path in case the working dir differs.
		if data, err = os.ReadFile("install.sh"); err != nil {
			t.Fatalf("read install.sh: %v", err)
		}
	}
	body := string(data)

	// Required: the post-fix command line must appear, with the
	// canonical mpm_system --payload '{"action":"health_check"}' form.
	// We allow either `mpm` or `"$PREFIX/bin/mpm"` so a future
	// refactor of the variable naming doesn't fail this test.
	canonical := []string{
		`call mpm_system --payload '{"action":"health_check"}'`,
	}
	for _, want := range canonical {
		if !strings.Contains(body, want) {
			t.Errorf("install.sh must contain %q (D-3.1 canonical form); not found", want)
		}
	}

	// Negative: the pre-fix form MUST NOT appear. If a future
	// refactor accidentally reintroduces `mpm call health_check`
	// (without the tool prefix), the validation step would silently
	// fail at install time.
	preFix := []string{
		`call health_check`,
	}
	for _, bad := range preFix {
		if strings.Contains(body, bad) {
			t.Errorf("install.sh contains pre-fix form %q; must use mpm_system --payload '{...}' instead", bad)
		}
	}
}

// TestInstallSh_AcceptsMPMWorkspaceOverride pins the D-3.2 / D-9.1
// fix: the wrapper script must honour MPM_WORKSPACE from the
// environment, falling back to the install-time default only when
// unset. Pre-fix the wrapper hardcoded the install path with no
// override, so any test/dev environment with a different workspace
// crashed.
func TestInstallSh_AcceptsMPMWorkspaceOverride(t *testing.T) {
	data, err := os.ReadFile("install.sh")
	if err != nil {
		t.Fatalf("read install.sh: %v", err)
	}
	body := string(data)

	// Required: at least one wrapper-style `${MPM_WORKSPACE:-...}`
	// default-fallback expression. The exact wrapper location is
	// allowed to vary — we just require the env-override form is
	// present somewhere in the script.
	if !strings.Contains(body, "${MPM_WORKSPACE:-") {
		t.Errorf("install.sh must honour MPM_WORKSPACE env override via ${MPM_WORKSPACE:-<default>}; not found")
	}
}

// TestInstallSh_ReadDirectivesValidationForm pins the canonical command
// line used by install.sh's prime-directive presence check (the
// `phase_validate` step). The canonical form is:
//
//	"$PREFIX/bin/mpm" call mpm_context --payload '{"action":"read_directives","params":{}}'
//
// Pre-fix this was `mpm call read_directives --payload '{}'` — but
// `read_directives` is an action of the `mpm_context` aggregator
// tool, not a top-level tool name. The dispatcher rejects it with
// "unknown tool: read_directives" (verified empirically), and the
// `2>/dev/null || true` masking means the directive-presence check
// always misfired (warning "no prime directives found" even after the
// operator had run `mpm ops init directives`). This test fails on
// either the missing canonical form or any reintroduction of the
// pre-fix form, mirroring the D-3.1 health-check pin in the same file.
func TestInstallSh_ReadDirectivesValidationForm(t *testing.T) {
	data, err := os.ReadFile("install.sh")
	if err != nil {
		t.Fatalf("read install.sh: %v", err)
	}
	body := string(data)

	// Required: the canonical mpm_context --payload form. Allow either
	// `mpm` or `"$PREFIX/bin/mpm"` so a future variable-name refactor
	// does not fail this pin (matches the D-3.1 test's permissiveness).
	canonical := []string{
		`call mpm_context --payload '{"action":"read_directives","params":{}}'`,
	}
	for _, want := range canonical {
		if !strings.Contains(body, want) {
			t.Errorf("install.sh must contain %q (canonical read_directives form); not found", want)
		}
	}

	// Negative: the pre-fix form `call read_directives` MUST NOT appear.
	// If a future refactor reintroduces it without the mpm_context
	// aggregator prefix, the validation silently regresses.
	preFix := []string{
		`call read_directives --payload`,
	}
	for _, bad := range preFix {
		if strings.Contains(body, bad) {
			t.Errorf("install.sh contains pre-fix form %q; must use mpm_context --payload '{\"action\":\"read_directives\",...}' instead", bad)
		}
	}
}
