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
	data, err := os.ReadFile("../install.sh")
	if err != nil {
		// Try a sibling path in case the working dir differs.
		if data, err = os.ReadFile("../install.sh"); err != nil {
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
// TestInstallSh_AcceptsMPMWorkspaceOverride pins the post-fix MPM_WORKSPACE
// override contract.
//
// Pre-fix the installer wrote a shell wrapper at $PREFIX/bin/mpm whose
// sole purpose was `exec env MPM_WORKSPACE=${MPM_WORKSPACE:-...} mpm.real`.
// Removing the wrapper changes which file owns the env-override logic:
// it now lives in the Go binary itself (see
// internal/core/config.GetMPMDir at internal/core/config/config.go:614,
// which returns $MPM_WORKSPACE if set and falls back to $HOME/.mpm).
//
// The installer must NOT re-introduce a wrapper that handles the env
// override, because the binary already does and reintroducing the wrapper
// would re-create the bin/mpm contamination that broke `make build`.
//
// This test pins:
//  1. install.sh does NOT contain a shell wrapper rewrite at $PREFIX/bin/mpm
//  2. The override contract is still documented in install.sh (the binary
//     honours the env var; the user can set it at invocation)
func TestInstallSh_AcceptsMPMWorkspaceOverride(t *testing.T) {
	data, err := os.ReadFile("../install.sh")
	if err != nil {
		if data, err = os.ReadFile("../install.sh"); err != nil {
			t.Fatalf("read install.sh: %v", err)
		}
	}
	body := string(data)

	// (1) No shell-wrapper heredoc / `cat > $PREFIX/bin/mpm` write.
	if strings.Contains(body, "<<WRAPPER") {
		t.Errorf("install.sh must not contain a wrapper heredoc; the env-override contract is owned by the Go binary (GetMPMDir), not the installer")
	}
	if strings.Contains(body, `cat > "$PREFIX/bin/mpm"`) {
		t.Errorf("install.sh must not write a shell wrapper to $PREFIX/bin/mpm; the override contract lives in the binary")
	}

	// (2) The override must still be documented somewhere — either in
	//     install.sh's own text (for operator-facing docs) or in a code
	//     comment explaining where the override is honoured.
	overrideMarkers := []string{
		"MPM_WORKSPACE",
	}
	for _, marker := range overrideMarkers {
		if !strings.Contains(body, marker) {
			t.Errorf("install.sh must reference %q so operators can find the override contract", marker)
		}
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
	data, err := os.ReadFile("../install.sh")
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
	data, err := os.ReadFile("../install.sh")
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
	data, err := os.ReadFile("../install.sh")
	if err != nil {
		if data, err = os.ReadFile("../install.sh"); err != nil {
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
	data, err := os.ReadFile("../install.sh")
	if err != nil {
		if data, err = os.ReadFile("../install.sh"); err != nil {
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
	data, err := os.ReadFile("../install.sh")
	if err != nil {
		if data, err = os.ReadFile("../install.sh"); err != nil {
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
	data, err := os.ReadFile("../install.sh")
	if err != nil {
		if data, err = os.ReadFile("../install.sh"); err != nil {
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

// TestInstallSh_NoWrapperAndNoMpmReal pins the post-fix ownership
// invariant: the installer must NOT write a shell wrapper to a
// Makefile-owned build path, and must NOT maintain a separate
// mpm.real binary alongside mpm.
//
// The pre-fix layout wrote a 261-byte POSIX shell wrapper to
// $PREFIX/bin/mpm (which is bin/mpm when source == install prefix
// via the canonical ~/.mpm symlink) and the compiled binary to
// $PREFIX/bin/mpm.real. This caused `make build` to fail on
// already-installed repos with:
//
//	build output "bin/mpm" already exists and is not an object file
//
// because `go build -o bin/mpm` refused to overwrite a non-object
// file at that path.
//
// The fix removes the wrapper entirely. The compiled binary at
// bin/mpm IS the runtime entry point; MPM_WORKSPACE defaulting to
// $HOME/.mpm is handled internally by GetMPMDir() in
// internal/core/config/config.go. No wrapper is needed.
//
// This test pins:
//  1. install.sh does not contain the wrapper heredoc (`<<WRAPPER`)
//  2. install.sh does not write mpm.real
//  3. install.sh does clean up legacy wrapper artefacts from older
//     installs (mpm.real, mpm.pre-wrapper.*)
//  4. install.sh handles the source == install prefix edge case
//     via the same-inode skip in the binary install loop
func TestInstallSh_NoWrapperAndNoMpmReal(t *testing.T) {
	data, err := os.ReadFile("../install.sh")
	if err != nil {
		if data, err = os.ReadFile("../install.sh"); err != nil {
			t.Fatalf("read install.sh: %v", err)
		}
	}
	body := string(data)

	// (1) No wrapper heredoc must exist.
	if strings.Contains(body, "<<WRAPPER") {
		t.Errorf("install.sh must not contain the wrapper heredoc (`<<WRAPPER`); the wrapper was the source of the bin/mpm contamination that broke `make build` after install")
	}
	if strings.Contains(body, `cat > "$PREFIX/bin/mpm"`) {
		t.Errorf("install.sh must not `cat > $PREFIX/bin/mpm` — overwriting the Makefile-owned build artifact with wrapper content is the bug being fixed")
	}

	// (2) install.sh must not install a separate mpm.real binary.
	if strings.Contains(body, `install -m 0755 "$src" "$dst"`) &&
		strings.Contains(body, "mpm.real") {
		// Specifically: the line that copies mpm to mpm.real must
		// be gone. We accept a mention of mpm.real in comments
		// (e.g. the legacy cleanup block) but not in an active copy.
		if strings.Contains(body, `install -m 0755 "$PROJECT_ROOT/bin/mpm" "$PREFIX/bin/mpm.real"`) {
			t.Errorf("install.sh must not install a separate mpm.real binary; mpm.real is dead weight (the Go binary defaults MPM_WORKSPACE internally)")
		}
	}

	// (3) Legacy cleanup block must exist and target the right
	// artefacts.
	legacyTargets := []string{
		"$PREFIX/bin/mpm.real",
		"$PREFIX/bin/mpm.pre-wrapper.",
	}
	for _, want := range legacyTargets {
		if !strings.Contains(body, want) {
			t.Errorf("install.sh must clean up the legacy artefact %q from older wrapper-based installs", want)
		}
	}

	// (4) Same-inode skip for the install loop.
	//     phase_binaries must guard `install -m 0755` with `-ef`
	//     so the same-prefix case (PROJECT_ROOT/bin/mpm -ef PREFIX/bin/mpm)
	//     does not attempt to copy a file onto itself.
	if !strings.Contains(body, `[ "$src" -ef "$dst" ]`) {
		t.Errorf("install.sh phase_binaries must guard the install with `[ $src -ef $dst ]` to handle the source == install prefix case")
	}

	// (5) The mpm binary must be in the promoted set (no longer handled by
	//     a separate wrapper-writing block).
	//
	//     2026-10-07: the promotion order moved out of install.sh into
	//     scripts/lib/binary_transaction.sh, shared with `make install`.
	//     The old pin asserted a literal `for bin in mpm mpm-scheduler ...`
	//     loop in install.sh; that loop no longer exists because promotion
	//     is now a stage/backup/rename/rollback transaction rather than
	//     five independent `install -m` calls.
	//
	//     The requirement is unchanged and now pins the canonical order in
	//     the one place that defines it. Both promotion surfaces must use
	//     it — if either reorders independently, a partial promotion would
	//     leave a different mixture depending on which surface ran.
	libBody, err := os.ReadFile("../scripts/lib/binary_transaction.sh")
	if err != nil {
		t.Fatalf("cannot read the transactional promotion library: %v", err)
	}
	if !strings.Contains(string(libBody), `BT_DEFAULT_BINARIES="mpm mpm-scheduler mpm-critic mpm-mcp mpm-telemetry"`) {
		t.Errorf("the promotion library must define the canonical five-binary order " +
			"as `BT_DEFAULT_BINARIES=\"mpm mpm-scheduler mpm-critic mpm-mcp mpm-telemetry\"` " +
			"(this is what replaced install.sh's inline promotion loop)")
	}

	// (6) Both promotion surfaces must delegate to the shared transaction
	//     rather than copying binaries independently. Two independent
	//     implementations is exactly how the order drifted apart before.
	if !strings.Contains(body, `mpm_promote_binaries`) {
		t.Errorf("install.sh phase_binaries must delegate to mpm_promote_binaries " +
			"(scripts/lib/binary_transaction.sh); five independent `install -m` calls " +
			"leave a mixed release behind when one of them fails")
	}
	if !strings.Contains(body, `. "$BT_LIB"`) {
		t.Errorf("install.sh must source the shared promotion library ($BT_LIB) so " +
			"install.sh and `make install` cannot implement divergent transactions")
	}
	makeBody, err := os.ReadFile("../Makefile")
	if err != nil {
		t.Fatalf("cannot read the Makefile: %v", err)
	}
	if !strings.Contains(string(makeBody), `mpm_promote_binaries`) {
		t.Errorf("the Makefile `install:` target must delegate to mpm_promote_binaries; " +
			"it is a second promotion surface and must share one transaction")
	}
}

// TestInstallSh_PathWarningDoesNotPromiseNewShellsFixPath pins the
// 2026-09-16 fresh-profile fix for the installer's PATH warning
// wording. Pre-fix the on-PATH warning said:
//
//	~/.local/bin is NOT on your current PATH
//	new shells will pick it up automatically (XDG default)
//
// This was inaccurate. ~/.profile conditionally adds ~/.local/bin
// to PATH, but ordinary new terminal windows inside an existing
// graphical login inherit the desktop environment and do NOT
// necessarily process .profile. So `bash -ic 'command -v mpm'`
// failed on the fresh `x` profile while `bash -lc 'command -v mpm'`
// succeeded. The wording promised a fix that wasn't reliably real.
//
// The fix tells the operator what actually works: a new LOGIN
// session will normally pick it up (because that's when the
// shell sources .profile), and for the installer's shell they
// can either export PATH or invoke the canonical binary path
// directly.
//
// The test:
//
//  1. Negatively pins the pre-fix wording (must NOT appear).
//  2. Positively pins the post-fix wording fragments:
//     - "a new login session will normally pick it up" (or
//     equivalent accurate description)
//     - "export PATH=" (the actionable export instruction)
//     - "this shell" / "for this shell" (frames the export as
//     shell-scoped, not global environment mutation)
func TestInstallSh_PathWarningDoesNotPromiseNewShellsFixPath(t *testing.T) {
	data, err := os.ReadFile("../install.sh")
	if err != nil {
		if data, err = os.ReadFile("../install.sh"); err != nil {
			t.Fatalf("read install.sh: %v", err)
		}
	}
	body := string(data)

	// Negative: the pre-fix wording promised new shells would
	// pick the PATH up automatically. That promise was empirically
	// false on the fresh `x` profile (graphical login inherits
	// env from the desktop session, .profile is not processed).
	preFixPromises := []string{
		"new shells will pick it up automatically",
	}
	for _, bad := range preFixPromises {
		if strings.Contains(body, bad) {
			t.Errorf("install.sh contains pre-fix promise %q (empirically inaccurate on graphical-login hosts); wording must describe what actually works (new login session, not generic new shells)", bad)
		}
	}

	// Positive: the post-fix wording frames the fix as scoped to
	// the current shell AND points at a new login session as the
	// reliable longer-term fix. The exact phrasing wording is allowed
	// to vary (so a future copy-edit doesn't fail the pin) — we just
	// require the actionable hints are present.
	requiredHints := []string{
		// Shell-scoped export instruction.
		"export PATH=",
		// Reliable fix framing: a fresh login session is what
		// processes .profile. "login session" / "login shell" / "login
		// again" all qualify; we pin the substring "login" so any
		// of the natural phrasings passes.
		"login",
		// Frames the export as scoped to this session (NOT a
		// permanent environment mutation by the installer).
		"this shell",
	}
	for _, want := range requiredHints {
		if !strings.Contains(body, want) {
			t.Errorf("install.sh PATH warning must contain %q (actionable fix guidance); not found", want)
		}
	}
}

// TestInstallSh_NoDirectiveWordingInSuccessPath pins the 2026-09-16
// fresh-profile fix for the install-validation messaging: the success
// path must say "✓ directives present (N)" — not the obsolete
// "prime directives present (N)". The terminology has moved from
// "prime directives" to "directives"; the install script's user-facing
// output must reflect the current term.
//
// The pre-fix wording lives in phase_validate. The seeding hint
// (when directives are missing) still says "to seed the baseline,
// run: $PREFIX/bin/mpm ops init directives" — that line uses the
// current term and is preserved.
//
// The test:
//
//  1. Asserts the success-path wording "directives present" exists.
//  2. Negatively pins the obsolete "prime directives" wording in
//     user-facing log/warn strings (NOT in code comments, the DB
//     column name, the metadata key, or internal Go identifiers —
//     those are stable technical surfaces and out of scope).
func TestInstallSh_NoDirectiveWordingInSuccessPath(t *testing.T) {
	data, err := os.ReadFile("../install.sh")
	if err != nil {
		if data, err = os.ReadFile("../install.sh"); err != nil {
			t.Fatalf("read install.sh: %v", err)
		}
	}
	body := string(data)

	// Positive: the success-path wording must exist.
	positive := []string{
		`directives present (`,
	}
	for _, want := range positive {
		if !strings.Contains(body, want) {
			t.Errorf("install.sh must contain current terminology %q in phase_validate success path; not found", want)
		}
	}

	// Negative: the obsolete "prime directives" wording must NOT
	// appear in user-facing log/warn strings. We pin the exact
	// log lines (which were the actual surface the user saw).
	preFixUserFacing := []string{
		`log "  ✓ prime directives present (`,
		`warn "  ! no prime directives found"`,
	}
	for _, bad := range preFixUserFacing {
		if strings.Contains(body, bad) {
			t.Errorf("install.sh contains obsolete user-facing wording %q (must use 'directives' / 'no directives' current terminology)", bad)
		}
	}
}

// TestInstallSh_CompletionTipNoNewShellsLeak pins the 2026-09-16
// fresh-profile follow-up: the INSTALL COMPLETE block previously
// contained a multi-line tip claiming `~/.local/bin/mpm is on PATH
// for NEW shells (XDG default)`. That wording was empirically
// inaccurate (a fresh Linux Mint terminal inside a graphical login
// inherits the desktop PATH and does NOT process .profile) and
// also leaked the internal `phase_validate` function name and the
// obsolete `prime directives` terminology into user-facing output.
//
// The post-fix wording is shorter and accurate: a new login session
// normally picks the PATH up; until then, use the canonical binary
// path directly. No internal implementation names, no stale
// terminology, no redundant directive-seeding guidance (the
// validation step already prints that in its missing-directives
// branch).
//
// Required negative pins (any of these in install.sh source = fail):
//   - "NEW shells"           — inaccurate PATH promise
//   - "phase_validate" in any log/warn string  — internal impl name
//     leak (the function NAME itself is fine — it has to be called
//     somewhere; only the user-facing log lines must not contain it)
//   - "prime directives"     — obsolete terminology
//
// Required positive pin:
//   - "new login session"    — the accurate fix
//   - "$cli" still present   — canonical binary path guidance
func TestInstallSh_CompletionTipNoNewShellsLeak(t *testing.T) {
	data, err := os.ReadFile("../install.sh")
	if err != nil {
		if data, err = os.ReadFile("../install.sh"); err != nil {
			t.Fatalf("read install.sh: %v", err)
		}
	}
	body := string(data)

	// Negative pin 1: "NEW shells" must not appear ANYWHERE in the
	// script — there is no legitimate use of this exact phrase.
	if strings.Contains(body, "NEW shells") {
		t.Errorf("install.sh must not contain %q (inaccurate PATH promise — new interactive shells inside a graphical login do NOT process .profile)", "NEW shells")
	}

	// Negative pin 2: "phase_validate" must not appear in any
	// user-facing log/warn/printf string. The function name itself
	// is fine (it's how mode_install calls the phase); only the
	// user-visible output must not leak it. We extract every
	// `log "..."` / `warn "..."` argument body and assert none of
	// them contain the banned substring.
	if strings.Contains(body, "NEW shells") {
		// Re-run the line scan only if we found NEW shells (above);
		// otherwise the line-scan below is the substantive one.
	}
	phaseValidateLeak := false
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, `log "`) && !strings.HasPrefix(trimmed, `warn "`) && !strings.HasPrefix(trimmed, `printf "`) {
			continue
		}
		if strings.Contains(line, "phase_validate") {
			phaseValidateLeak = true
			t.Errorf("install.sh user-facing log/warn line leaks internal impl name %q:\n  %s", "phase_validate", line)
		}
	}
	if !phaseValidateLeak {
		_ = phaseValidateLeak // silence unused-var check if no leak
	}

	// Negative pin 3: "prime directives" must not appear in any
	// user-facing log/warn/printf string. We use the same line-scan
	// approach — the term may appear in internal comments.
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, `log "`) && !strings.HasPrefix(trimmed, `warn "`) && !strings.HasPrefix(trimmed, `printf "`) {
			continue
		}
		if strings.Contains(line, "prime directives") {
			t.Errorf("install.sh user-facing log/warn line uses obsolete terminology %q:\n  %s", "prime directives", line)
		}
	}

	// Positive pins — the post-fix guidance fragments must appear.
	// "new login session" is the accurate framing. "$cli" — the
	// local variable that holds the canonical binary path — keeps
	// the helper path-runnable advice runnable.
	required := []string{
		"new login session",
	}
	for _, want := range required {
		if !strings.Contains(body, want) {
			t.Errorf("install.sh INSTALL COMPLETE tip must contain %q (accurate PATH fix framing)", want)
		}
	}

	// The canonical-path next-step commands (which the previous
	// canonical-path fix pinned) must remain runnable — i.e. the
	// tip must still mention the runnable path form, not strip it
	// while removing the stale wording.
	if !strings.Contains(body, "$cli") {
		t.Errorf("install.sh INSTALL COMPLETE tip must reference the canonical $cli path variable (runnable until ~/.local/bin is on PATH)")
	}
}
