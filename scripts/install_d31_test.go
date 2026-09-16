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
//
// TestInstallSh_DieHelperMessageContract pins the die() helper
// contract. The helper must:
//   - print the FIRST argument verbatim as the error message
//   - exit with the SECOND argument as the exit code (default 1)
//
// Pre-fix the helper was `die() { err "$*"; exit "${2:-1}"; }` which
// joined ALL positional arguments with a space, leaking the exit
// code into the rendered message:
//
//	die "Go not found in PATH" 1
//	→ ERROR: Go not found in PATH 1
//	→ exit 1
//
// (The "1" at the end of the message was the trailing exit-code
// argument, joined by "$*".) The fix is `$1` for the message and
// `${2:-1}` for the exit code. This test pins both halves so a
// future refactor can't reintroduce either bug.
//
// Required literal: `die()  { err "$1"; exit "${2:-1}"; }` (with
// the double-space style match so this isn't tied to whitespace
// formatting decisions in unrelated edits).
func TestInstallSh_DieHelperMessageContract(t *testing.T) {
	data, err := os.ReadFile("../scripts/install.sh")
	if err != nil {
		if data, err = os.ReadFile("install.sh"); err != nil {
			t.Fatalf("read install.sh: %v", err)
		}
	}
	body := string(data)

	// Required: the post-fix helper form must appear. Use the exact
	// spacing of the current style (double space between `die()` and
	// `{`) so this test is stable against cosmetic edits that don't
	// change the helper's semantics.
	const fixedHelper = `die()  { err "$1"; exit "${2:-1}"; }`
	if !strings.Contains(body, fixedHelper) {
		t.Errorf("install.sh must contain the fixed die helper %q (use $1 for message, ${2:-1} for exit code); not found", fixedHelper)
	}

	// Negative: the pre-fix broken form must NOT appear. If a future
	// refactor reintroduces `"$*"` as the message, the trailing exit
	// code will leak back into the rendered output.
	const brokenHelper = `die()  { err "$*"; exit "${2:-1}"; }`
	if strings.Contains(body, brokenHelper) {
		t.Errorf("install.sh contains the pre-fix die helper %q (uses $* which joins all args into the message — leaks exit code); must use $1 for message", brokenHelper)
	}
}

// TestInstallSh_GoDiscoveryFallback pins the installer's Go discovery
// behaviour to mirror the Makefile (Makefile:53). install.sh must:
//  1. Prefer Go found via PATH (`command -v go`).
//  2. Fall back to `/usr/local/go/bin/go` when not on PATH but the
//     binary is executable. This is the standard install path on
//     Ubuntu / Linux Mint / Debian and many CI images — Go is
//     extracted to /usr/local/go but the directory is not on PATH
//     for non-login shells.
//  3. Fail only if NEITHER location has a usable `go`.
//  4. Store the resolved executable in a `GO_BIN` variable and use
//     it consistently (no second `command -v go` lookup that could
//     disagree with the first).
//
// Pre-fix install.sh only did `command -v go`, which fails on a
// fresh Mint/Ubuntu install where `/usr/local/go/bin/go` exists but
// isn't yet on PATH. The Makefile already handled this case; the
// installer was the lagging surface.
func TestInstallSh_GoDiscoveryFallback(t *testing.T) {
	data, err := os.ReadFile("../scripts/install.sh")
	if err != nil {
		if data, err = os.ReadFile("install.sh"); err != nil {
			t.Fatalf("read install.sh: %v", err)
		}
	}
	body := string(data)

	// Positive 1: the fallback path must appear at least once with
	// the standard location. The exact form may vary (test -x,
	// [ -x ], command -v fallback chain) but the literal path must
	// be present so a future refactor can't drop the fallback.
	if !strings.Contains(body, "/usr/local/go/bin/go") {
		t.Errorf("install.sh must include the /usr/local/go/bin/go fallback path (matches Makefile:53); not found")
	}

	// Positive 2: a GO_BIN variable must be defined and used. The
	// variable name is part of the contract — the message says
	// "resolved the executable into a variable". We accept either
	// `GO_BIN=` or `${GO_BIN}` form to be resilient to quoting
	// style changes.
	hasDef := strings.Contains(body, "GO_BIN=")
	hasUse := strings.Contains(body, "${GO_BIN}") || strings.Contains(body, "$GO_BIN")
	if !hasDef || !hasUse {
		t.Errorf("install.sh must define and use a GO_BIN variable (def=%v, use=%v); expected both halves of the contract", hasDef, hasUse)
	}

	// Negative: the pre-fix form `command -v go >/dev/null 2>&1 || die ...`
	// MUST NOT appear as the SOLE prereq check. A two-line form that
	// resolves via a variable is fine; the bare PATH-only check that
	// fails on /usr/local/go installs must be gone.
	barePreflightOnly := []string{
		`command -v go >/dev/null 2>&1 || die "Go not found in PATH" 1`,
	}
	for _, bad := range barePreflightOnly {
		if strings.Contains(body, bad) {
			t.Errorf("install.sh contains the pre-fix Go-only check %q; must accept /usr/local/go/bin/go as a fallback", bad)
		}
	}
}

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

// TestInstallSh_NextStepsUseCanonicalPath pins the post-install
// completion guidance so it is internally consistent with the rest
// of install.sh's contract:
//
//   - The success-path "next steps" commands must use the canonical
//     binary path (resolved via a `cli="$PREFIX/bin/mpm"` local var,
//     which renders as e.g. `/home/v/.mpm/bin/mpm`), not bare `mpm`.
//     Bare `mpm` only resolves in shells where ~/.local/bin is already
//     on PATH; on a fresh Linux Mint / Ubuntu host the installer ran
//     in a non-login shell where that directory is NOT yet on PATH.
//     The on-PATH warning was emitted during phase_symlinks, but the
//     next-steps message itself must be runnable in that same shell
//     — otherwise the user copies a command, pastes it, and gets
//     `mpm: command not found`.
//
//   - The "mpm ops init directives" line must NOT appear in the
//     success-path next-steps. phase_validate already emits a
//     warn-only hint with the canonical path when directives are
//     absent (so the seeding command is reachable exactly when
//     needed). Printing it unconditionally after a successful
//     install contradicts the validation step's
//     "✓ prime directives present" status and prompts a redundant
//     command.
//
// The test accepts any of three acceptable forms in install.sh
// (whichever the current style uses): `"$PREFIX/bin/mpm"`,
// `\"$PREFIX/bin/mpm\"` (escaped inside a double-quoted log arg),
// or `$cli` (local-var indirection). All three render to the same
// runnable canonical path. We just need to ensure the bare `mpm`
// form does NOT appear in the next-steps block.
func TestInstallSh_NextStepsUseCanonicalPath(t *testing.T) {
	data, err := os.ReadFile("../scripts/install.sh")
	if err != nil {
		if data, err = os.ReadFile("install.sh"); err != nil {
			t.Fatalf("read install.sh: %v", err)
		}
	}
	body := string(data)

	// Positive: the next-steps block must contain at least one
	// canonical-path form for each verification command. Accept
	// any of the three idioms the script may use; the contract is
	// "rendered output is runnable", not "exact variable name".
	canonicalForms := []string{
		// Most common: a local `cli="$PREFIX/bin/mpm"` variable.
		`$cli status`,
		`$cli call read_wake_context`,
		// Inline (no local var): escaped quotes inside a double-quoted
		// log string.
		`\"$PREFIX/bin/mpm\" status`,
		`\"$PREFIX/bin/mpm\" call read_wake_context`,
		// Inline (no local var, no escaping needed because the path
		// is the start of a shell command, not inside a log arg).
		`"$PREFIX/bin/mpm" status`,
		`"$PREFIX/bin/mpm" call read_wake_context`,
	}
	statusFound := false
	readWakeFound := false
	for _, form := range canonicalForms {
		if strings.Contains(body, form) {
			if strings.Contains(form, "status") {
				statusFound = true
			}
			if strings.Contains(form, "read_wake_context") {
				readWakeFound = true
			}
		}
	}
	if !statusFound {
		t.Errorf("install.sh next-steps must contain a canonical-path form of `... status` (runnable in any shell); none of the acceptable forms were found")
	}
	if !readWakeFound {
		t.Errorf("install.sh next-steps must contain a canonical-path form of `... call read_wake_context` (runnable in any shell); none of the acceptable forms were found")
	}

	// Negative: the pre-fix bare-mpm forms MUST NOT appear as
	// next-steps lines. (The bare `mpm` string is allowed in
	// comments, conditional seeding hints, and the phase_symlinks
	// PATH-shadow detection — we only pin the next-steps block.)
	preFixNextSteps := []string{
		`log "  mpm status                # verify DB reachable"`,
		`log "  mpm call read_wake_context   # first agent tool call"`,
		`log "  mpm ops init directives   # seed prime directives (cognitive rules)"`,
	}
	for _, bad := range preFixNextSteps {
		if strings.Contains(body, bad) {
			t.Errorf("install.sh contains pre-fix bare-mpm form %q; next-steps must use the canonical binary path and the seeding line is redundant after a successful validation", bad)
		}
	}

	// Positive (preservation): the conditional seeding hint that
	// the validation step emits WHEN directives are missing must
	// still exist. That's the only place the seeding command
	// belongs — visible exactly when needed.
	if !strings.Contains(body, "ops init directives") {
		t.Errorf("install.sh must preserve the conditional seeding hint (phase_validate emits it when directives are missing); not found")
	}
}

// TestInstallSh_WrapperHeredocHasNoCommandSubstitution pins the
// wrapper heredoc in install.sh against accidental command-
// substitution constructs.
//
// The wrapper heredoc uses an UNQUOTED delimiter (`<<WRAPPER`)
// so that ${DATA_ROOT} and ${PREFIX} expand at install time and
// the installed wrapper can route to the install-time binary
// path. The unquoted delimiter, however, also means Bash performs
// $(...) and `...` substitution inside the heredoc — a stray
// backtick (or $() ) in what looks like a comment will try to
// execute the contents during install.
//
// INSTALL-003: the wrapper comment used Markdown-style backticks
// around an example invocation:
//
//	# Override at invocation: `MPM_WORKSPACE=/tmp/foo mpm call …`
//
// Bash tried to execute `MPM_WORKSPACE=/tmp/foo mpm call …` during
// install, failing with "line N: mpm: command not found" on hosts
// where `mpm` was not yet on PATH (i.e. the entire target user
// base — fresh Linux Mint / Ubuntu hosts running the installer for
// the first time).
//
// This test pins:
//  1. No backticks inside the wrapper heredoc (would be command
//     substitution; the wrapper file is not the only casualty —
//     the install aborts).
//  2. No $(...) patterns inside the wrapper heredoc (same hazard
//     class).
//  3. The override example remains as inert comment text (without
//     the backticks that previously broke install).
//  4. The intended install-time and runtime variable expansions
//     (${DATA_ROOT}, ${PREFIX}, \${MPM_WORKSPACE:-...}, "\$@")
//     remain in the heredoc.
func TestInstallSh_WrapperHeredocHasNoCommandSubstitution(t *testing.T) {
	data, err := os.ReadFile("../scripts/install.sh")
	if err != nil {
		if data, err = os.ReadFile("install.sh"); err != nil {
			t.Fatalf("read install.sh: %v", err)
		}
	}
	body := string(data)

	// Extract the wrapper heredoc content. The heredoc is the only
	// `<<WRAPPER ... WRAPPER` block in install.sh. The start
	// marker is `<<WRAPPER`; the end marker is a line whose first
	// non-whitespace token is `WRAPPER` (no leading whitespace per
	// shell heredoc semantics).
	const startMarker = "<<WRAPPER"
	const endMarker = "WRAPPER"

	startIdx := strings.Index(body, startMarker)
	if startIdx == -1 {
		t.Fatalf("install.sh must contain the wrapper heredoc start marker %q", startMarker)
	}
	afterStart := startIdx + len(startMarker)

	var heredoc strings.Builder
	foundEnd := false
	for _, line := range strings.Split(body[afterStart:], "\n") {
		if strings.TrimSpace(line) == endMarker {
			foundEnd = true
			break
		}
		heredoc.WriteString(line)
		heredoc.WriteString("\n")
	}
	if !foundEnd {
		t.Fatalf("install.sh must contain the wrapper heredoc end marker line %q", endMarker)
	}
	heredocContent := heredoc.String()

	// (1) No backticks inside the heredoc.
	if strings.Contains(heredocContent, "`") {
		t.Errorf("wrapper heredoc must not contain backticks (would be interpreted as command substitution since the heredoc delimiter is unquoted); offending heredoc:\n%s", heredocContent)
	}

	// (2) No $(...) patterns inside the heredoc.
	if strings.Contains(heredocContent, "$(") {
		t.Errorf("wrapper heredoc must not contain $(...) patterns (would be interpreted as command substitution since the heredoc delimiter is unquoted); offending heredoc:\n%s", heredocContent)
	}

	// (3) The override example is preserved as inert comment text.
	//    We assert the line is still a comment and still contains
	//    the example invocation, without the substituted-backtick
	//    variant that triggered INSTALL-003.
	requiredComment := []string{
		"# Override at invocation:",
		"MPM_WORKSPACE=/tmp/foo mpm call",
	}
	for _, want := range requiredComment {
		if !strings.Contains(heredocContent, want) {
			t.Errorf("wrapper heredoc must preserve the override example as inert comment text; missing %q in:\n%s", want, heredocContent)
		}
	}
	// Negative: the pre-fix Markdown backtick form must NOT appear.
	const preFixBackticks = "`MPM_WORKSPACE=/tmp/foo mpm call"
	if strings.Contains(heredocContent, preFixBackticks) {
		t.Errorf("wrapper heredoc contains the pre-fix INSTALL-003 form %q (backticks trigger command substitution); offending heredoc:\n%s", preFixBackticks, heredocContent)
	}

	// (4) Install-time expansions must be preserved.
	installTimeExpansions := []string{
		"${DATA_ROOT}",
		"${PREFIX}/bin/mpm.real",
	}
	for _, want := range installTimeExpansions {
		if !strings.Contains(heredocContent, want) {
			t.Errorf("wrapper heredoc must preserve install-time expansion %q (do not quote the heredoc delimiter or disturb the intentional expansions); missing in:\n%s", want, heredocContent)
		}
	}

	// (5) Runtime escaped expansions must be preserved.
	runtimeExpansions := []string{
		`\$@`,
		`\${MPM_WORKSPACE:-${DATA_ROOT}}`,
	}
	for _, want := range runtimeExpansions {
		if !strings.Contains(heredocContent, want) {
			t.Errorf("wrapper heredoc must preserve runtime-escaped expansion %q; missing in:\n%s", want, heredocContent)
		}
	}
}
