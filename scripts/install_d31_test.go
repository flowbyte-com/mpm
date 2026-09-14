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

// TestInstallSh_DataDirMode0700 pins the D-3.3 fix: install.sh's
// `phase_data_dir` step must create the runtime data directories
// (`$DATA_ROOT/src/db` and `$DATA_ROOT/backups/critic-pre`) at mode
// 0700, not 0755. These directories hold the SQLite database
// (`mpm.db` + WAL/SHM sidecars), the audit mirror, and the backup
// tree — all treated as confidential application data per
// `docs/SECURITY.md` §"A note specifically about memory contents".
//
// The runtime layer already enforces 0700/0600 via
// `AssertUserDirPerms0700` + `TightenFilePerms0600` on the data root,
// and the SQLite file modes are auto-tightened to 0600. But the
// enclosing directory traversal mode (the `0755` that the installer's
// `install -d` had been creating) was not addressed — a permissive
// `src/db/` allows other local users to enumerate the database
// filename and probe sidecars even when the parent data root is 0700.
//
// The fix mirrors the canonical blobstore pattern
// (`internal/blobstore/fs.go`): MkdirAll + explicit `chmod 0700` so a
// pre-existing permissive directory is corrected on re-install
// (idempotence must not preserve an insecure state).
//
// This test fails if the installer uses any of the known-pre-fix
// `0755` patterns for the data directories, or if it omits the
// `0700` enforcement required by the new contract.
func TestInstallSh_DataDirMode0700(t *testing.T) {
	data, err := os.ReadFile("install.sh")
	if err != nil {
		t.Fatalf("read install.sh: %v", err)
	}
	body := string(data)

	// Negative: the pre-fix `install -d -m 0755 "$DATA_ROOT/src/db"`
	// (or the two-arg variant that bundles both data dirs) MUST NOT
	// appear. The test accepts any of several equivalent positive
	// forms; the negative pin keeps a refactor honest — if someone
	// quietly widens the mode back to 0755 (e.g., copy-pasting from
	// the `install -d -m 0755 "$PREFIX/bin"` line above), this test
	// will fail.
	preFixPatterns := []string{
		`install -d -m 0755 "$DATA_ROOT/src/db"`,
		`install -d -m 0755 "$DATA_ROOT/src/db" "$DATA_ROOT/backups/critic-pre"`,
	}
	for _, bad := range preFixPatterns {
		if strings.Contains(body, bad) {
			t.Errorf("install.sh contains pre-fix data-dir mode %q; runtime data dirs must be 0700", bad)
		}
	}

	// Positive: at least one of the documented secure forms must be
	// present. The canonical install-time fix is `install -d -m 0700
	// "$DATA_ROOT/src/db" "$DATA_ROOT/backups/critic-pre"`. We also
	// accept a defensive chmod-after-mkdir variant (`chmod 0700` on
	// the same paths) so a refactor that splits create + harden into
	// two phases (mirroring the blobstore `os.MkdirAll` + `os.Chmod`
	// pattern) does not break this pin.
	positive := []string{
		`install -d -m 0700 "$DATA_ROOT/src/db"`,
		`install -d -m 0700 "$DATA_ROOT/backups/critic-pre"`,
		`chmod 0700 "$DATA_ROOT/src/db"`,
		`chmod 0700 "$DATA_ROOT/backups/critic-pre"`,
	}
	found := false
	for _, want := range positive {
		if strings.Contains(body, want) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("install.sh must enforce 0700 on $DATA_ROOT/src/db and $DATA_ROOT/backups/critic-pre; "+
			"none of the canonical forms found: %v", positive)
	}
}

// TestInstallSh_PATHShadowDetection (2026-09-14 release-pass) pins
// the PATH-shadow warning in install.sh. After symlinking
// ~/.local/bin/mpm -> $PREFIX/bin/mpm, the script must check
// `command -v mpm` and compare canonicalized paths
// (readlink -f). If they differ, the script must warn the
// operator showing both resolved and expected paths. The
// shadowing binary is NEVER removed automatically.
//
// Required literals (must all appear in install.sh):
//   - `command -v mpm`
//   - `readlink -f`
//   - `expected canonical install` (warning label)
//   - `will NOT be removed automatically` (safety contract)
func TestInstallSh_PATHShadowDetection(t *testing.T) {
	data, err := os.ReadFile("../scripts/install.sh")
	if err != nil {
		if data, err = os.ReadFile("install.sh"); err != nil {
			t.Fatalf("read install.sh: %v", err)
		}
	}
	body := string(data)

	required := []string{
		"command -v mpm",
		"readlink -f",
		"expected canonical install",
		"will NOT be removed automatically",
	}
	for _, want := range required {
		if !strings.Contains(body, want) {
			t.Errorf("install.sh must contain %q (PATH-shadow detection); not found", want)
		}
	}

	// Negative: the shadowing binary must NEVER be auto-removed.
	// A regex match is too fragile for shell; we check for the
	// absence of any `rm.*$(command -v mpm|which mpm)` pattern
	// anywhere in the script. A casual reader sees the
	// `will NOT be removed automatically` warning above; this
	// negative check is the safety-belt.
	banned := []string{
		`rm -f "$(command -v mpm)"`,
		`rm -f $(command -v mpm)`,
		`rm -f $(which mpm)`,
		`rm -f "$(which mpm)"`,
	}
	for _, bad := range banned {
		if strings.Contains(body, bad) {
			t.Errorf("install.sh must NEVER auto-remove shadowing binary: %q must not appear", bad)
		}
	}
}
