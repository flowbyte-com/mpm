"""
test_install_sh_contracts.py — static source-text contracts for install.sh.

This file is the former scripts/install_d31_test.go, ported.

WHY IT MOVED
------------

scripts/install_d31_test.go lived in scripts/, which is a nested Go module
(`go.mod`, module github.com/flowbyte-com/mpm/scripts) created for
exactly one reason: so the Go test runner would pick up that one file.
No Makefile target, CI job, or pre-commit hook runs that module.
`make test` enumerates ./cmd/..., ./internal/..., test-testenv,
test-tools, test-scripts and test-agent-installation, and none of them
descend into a nested module.

So the file was not a gate. It was 857 lines of assertions that no
supported entry point executed, and one of them was already stale:
TestInstallSh_NoWrapperAndNoMpmReal pinned the literal `. "$BT_LIB"`,
which commit 93b11cc ("make the transactional promotion loadable under
every shell") deliberately replaced with a lazy `load_promotion_lib()`
so that phase_validate and --dry-run do not need the library at all.
install.sh still sources it (`. "$lib"`); the test's literal no longer
matched, and it had been failing since that commit with nothing to say so.

The one place `make test` did touch this area made the situation worse
rather than better. cmd/mpm/release_pass_20260914_test.go contains
TestRelease_20260914_InstallPATHWarn, whose entire body is:

    _ = "see TestInstallSh_PATHShadowDetection in scripts/install_d31_test.go"

It asserts nothing, and its comment claims the detailed test "requires
scripts/go.mod, which is invoked by `go test ./scripts/...`". Nothing
invokes that. A green gate pointing at a suite it never ran — the same
failure mode the Makefile's own comment on `make test-scripts` records
this repository paying for once already.

WHAT MOVED HERE
---------------

Every assertion that was still valid and is not already covered by a
gated test. One was dropped rather than ported:

  - TestInstallSh_DataDirMode0700 — already asserted statically by
    test_install_created_vs_verified.py, which additionally pins the
    ordering of the 0700 directories against the chmod that follows.

Two were rewritten rather than copied:

  - The `. "$BT_LIB"` literal became a check on the actual contract:
    install.sh must source binary_transaction.sh and delegate to
    mpm_promote_binaries. The literal was an accident of one
    implementation, not the requirement.
  - The same-inode guard is pinned as it is, in phase_binaries. Note
    the promotion transaction deliberately dropped its own -ef skip
    (test_install_phase_binaries.py documents why), so the two
    code paths are independent and both need covering.

Everything here is source-text inspection. Nothing executes install.sh,
touches a service, a database, or the network.
"""

from __future__ import annotations

import re
import unittest
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[2]
INSTALL_SH = REPO_ROOT / "install.sh"
PROMOTION_LIB = REPO_ROOT / "scripts" / "lib" / "binary_transaction.sh"
MAKEFILE = REPO_ROOT / "Makefile"

# User-facing emission verbs. A bare `phase_validate` in a comment is fine —
# the function has to be called somewhere; only rendered output must not
# leak internal names or stale terminology.
EMISSION_PREFIXES = ('log "', 'warn "', 'err "', 'note "', 'printf "')


def _install_sh() -> str:
    return INSTALL_SH.read_text()


def _user_facing_lines() -> list[str]:
    """Lines whose content is rendered to the operator."""
    return [
        line
        for line in _install_sh().splitlines()
        if any(line.strip().startswith(p) for p in EMISSION_PREFIXES)
    ]


class HealthCheckValidationForm(unittest.TestCase):
    """D-3.1 — the validation step must use the `mpm call` payload form.

    Pre-fix it read `mpm call health_check`, a literal action name. The
    dispatcher rejects it ("unknown tool: health_check") because
    health_check is an action of the mpm_system aggregator.
    """

    def test_uses_canonical_mpm_system_payload_form(self):
        self.assertIn(
            """call mpm_system --payload '{"action":"health_check"}'""", _install_sh()
        )

    def test_pre_fix_literal_action_form_is_absent(self):
        self.assertNotIn("call health_check", _install_sh())


class ReadDirectivesValidationForm(unittest.TestCase):
    """read_directives is an action of mpm_context, not a tool name.

    The pre-fix form sat behind `2>/dev/null || true`, so the
    directive-presence check misfired and warned "no prime directives
    found" even after the operator had seeded them.
    """

    def test_uses_canonical_mpm_context_payload_form(self):
        self.assertIn(
            """call mpm_context --payload '{"action":"read_directives","params":{}}'""",
            _install_sh(),
        )

    def test_pre_fix_literal_action_form_is_absent(self):
        self.assertNotIn("call read_directives --payload", _install_sh())


class NoWrapperHeredoc(unittest.TestCase):
    """The env-override contract belongs to the Go binary, not a wrapper.

    A shell wrapper written to $PREFIX/bin/mpm re-created the bin/mpm
    contamination that made `go build -o bin/mpm` refuse to overwrite a
    non-object file on already-installed checkouts.
    """

    def test_no_wrapper_heredoc(self):
        self.assertNotIn("<<WRAPPER", _install_sh())

    def test_does_not_cat_over_the_build_artifact(self):
        self.assertNotIn("""cat > "$PREFIX/bin/mpm\"""", _install_sh())

    def test_workspace_override_is_still_documented(self):
        self.assertIn("MPM_WORKSPACE", _install_sh())


class LegacyWrapperCleanup(unittest.TestCase):
    """Older wrapper-based installs must be cleaned up on re-install."""

    def test_cleans_up_legacy_artifacts(self):
        body = _install_sh()
        for artifact in ("$PREFIX/bin/mpm.real", "$PREFIX/bin/mpm.pre-wrapper."):
            self.assertIn(artifact, body, f"no cleanup for {artifact}")

    def test_same_inode_guard_in_phase_binaries(self):
        """source == install prefix must not copy a file onto itself.

        Distinct from the promotion transaction, which deliberately has
        no -ef skip; see test_install_phase_binaries.py.
        """
        self.assertIn('[ "$src" -ef "$dst" ]', _install_sh())


class PromotionDelegatesToSharedTransaction(unittest.TestCase):
    """One transaction, two surfaces.

    Two independent promotion implementations is exactly how the
    five-binary order drifted apart and left a mixed release behind when
    one copy failed.

    Note this asserts the CONTRACT — install.sh sources the shared
    library and delegates to mpm_promote_binaries — not the literal
    `. "$BT_LIB"` the deleted Go test pinned. That literal was an
    accident of one implementation: commit 93b11cc made the library load
    lazily via load_promotion_lib() so --dry-run and phase_validate do
    not need it, which broke the literal while keeping the requirement.
    """

    def test_install_sh_sources_the_shared_library(self):
        body = _install_sh()
        self.assertIn("binary_transaction.sh", body)
        # assertRegex's third positional argument is `msg`, not `flags` —
        # passing re.M there silently searches without MULTILINE, so `^`
        # only matches the start of the file and this would pass vacuously.
        self.assertIsNotNone(
            re.search(r'^\s*\. "\$(?:BT_LIB|lib)"', body, re.M),
            "install.sh must dot-source the promotion library; no `. \"$lib\"` "
            "or `. \"$BT_LIB\"` line found",
        )

    def test_install_sh_delegates_to_the_shared_promoter(self):
        self.assertIn("mpm_promote_binaries", _install_sh())

    def test_makefile_delegates_to_the_same_promoter(self):
        self.assertIn("mpm_promote_binaries", MAKEFILE.read_text())

    def test_canonical_five_binary_order_is_defined_once(self):
        self.assertIn(
            'BT_DEFAULT_BINARIES="mpm mpm-scheduler mpm-critic mpm-mcp mpm-telemetry"',
            PROMOTION_LIB.read_text(),
        )

    def test_install_sh_does_not_copy_mpm_real_separately(self):
        self.assertNotIn(
            """install -m 0755 "$PROJECT_ROOT/bin/mpm" "$PREFIX/bin/mpm.real\"""",
            _install_sh(),
        )


class DieHelperContract(unittest.TestCase):
    """die() must render its message without leaking the exit code.

    Pre-fix it was `die() { err "$*"; exit "${2:-1}"; }`, so
    `die "Go not found in PATH" 1` printed "ERROR: Go not found in PATH 1".
    """

    def test_uses_first_argument_only_for_the_message(self):
        self.assertIn("""die()  { err "$1"; exit "${2:-1}"; }""", _install_sh())

    def test_pre_fix_star_form_is_absent(self):
        self.assertNotIn("""die()  { err "$*"; exit "${2:-1}"; }""", _install_sh())


class GoDiscoveryFallback(unittest.TestCase):
    """Go must be found on hosts where it is installed off PATH.

    Go extracted to /usr/local/go is not on PATH for non-login shells —
    the standard install path on Ubuntu, Mint, Debian and many CI images.
    The Makefile already handled it; the installer was the lagging
    surface, and it failed outright rather than falling back.
    """

    def test_includes_the_usr_local_go_fallback(self):
        self.assertIn("/usr/local/go/bin/go", _install_sh())

    def test_resolves_go_once_into_a_reused_variable(self):
        """A second, independent lookup could disagree with the first."""
        body = _install_sh()
        self.assertIn("GO_BIN=", body)
        self.assertRegex(body, r"\$\{?GO_BIN\}?")

    def test_pre_fix_path_only_preflight_is_absent(self):
        self.assertNotIn(
            """command -v go >/dev/null 2>&1 || die "Go not found in PATH" 1""",
            _install_sh(),
        )


class PathShadowDetection(unittest.TestCase):
    """A shadowing mpm on PATH must be reported, never removed.

    install.sh symlinks ~/.local/bin/mpm -> $PREFIX/bin/mpm. If some
    other mpm is found first, the operator is told what is shadowing
    what. Deleting someone else's binary is not the installer's call.
    """

    def test_detects_and_reports_the_shadow(self):
        body = _install_sh()
        for required in (
            "command -v mpm",
            "readlink -f",
            "expected canonical install",
            "will NOT be removed automatically",
        ):
            self.assertIn(required, body)

    def test_never_removes_the_shadowing_binary(self):
        body = _install_sh()
        for banned in (
            """rm -f "$(command -v mpm)\"""",
            "rm -f $(command -v mpm)",
            "rm -f $(which mpm)",
            """rm -f "$(which mpm)\"""",
        ):
            self.assertNotIn(banned, body)


class NextStepsUseCanonicalPath(unittest.TestCase):
    """Rendered guidance must be runnable in the shell that just installed.

    Bare `mpm` only resolves where ~/.local/bin is already on PATH. The
    installer runs in a non-login shell where it is not, so the operator
    copies the command and gets `mpm: command not found`.

    The same rule applies to the TOOL NAME. `read_wake_context` is an
    action of the `mpm_context` tool, not a tool of its own, so
    `mpm call read_wake_context` fails with "unknown tool". The runnable
    spelling is the payload form:

        mpm call mpm_context --payload '{"action":"read_wake_context"}'
    """

    # Canonical-path spellings of the two verification commands. These are
    # matched against install.sh SOURCE text, so the shell's escaped quotes
    # are literal here.
    STATUS_FORMS = (
        "$cli status",
        r"""\"$PREFIX/bin/mpm\" status""",
        '"$PREFIX/bin/mpm" status',
    )
    WAKE_FORMS = (
        r"""$cli call mpm_context --payload '{\"action\":\"read_wake_context\"}'""",
        r"""\"$PREFIX/bin/mpm\" call mpm_context --payload '{\"action\":\"read_wake_context\"}'""",
        r""""$PREFIX/bin/mpm" call mpm_context --payload '{\"action\":\"read_wake_context\"}'""",
    )
    # Spellings that resolve to no tool at all. `read_wake_context` is an
    # ACTION of the `mpm_context` tool, not a tool of its own, so
    # `mpm call read_wake_context` exits 1 with "unknown tool".
    DEAD_WAKE_FORMS = (
        "$cli call read_wake_context",
        r"""\"$PREFIX/bin/mpm\" call read_wake_context""",
        '"$PREFIX/bin/mpm" call read_wake_context',
    )

    def test_uses_a_canonical_path_for_the_verification_commands(self):
        body = _install_sh()
        self.assertTrue(
            any(f in body for f in self.STATUS_FORMS),
            "next steps must render a canonical-path `status` command",
        )
        self.assertTrue(
            any(f in body for f in self.WAKE_FORMS),
            "next steps must render a canonical-path read_wake_context command",
        )

    def test_next_steps_never_advertise_a_command_that_names_no_tool(self):
        """The canonical path is necessary but not sufficient.

        `mpm call read_wake_context` exits 1 with
        `{"error":"unknown tool: read_wake_context"}`; the supported
        spelling is the payload form. A canonical path does not rescue a
        command that names no tool, so the dead spellings must never be
        rendered.
        """
        body = _install_sh()
        for dead in self.DEAD_WAKE_FORMS:
            self.assertNotIn(
                dead,
                body,
                f"next steps advertise a command that names no tool: {dead!r}",
            )

    def test_pre_fix_bare_mpm_next_steps_are_absent(self):
        body = _install_sh()
        for banned in (
            'log "  mpm status                # verify DB reachable"',
            'log "  mpm call read_wake_context   # first agent tool call"',
            'log "  mpm ops init directives   # seed prime directives (cognitive rules)"',
        ):
            self.assertNotIn(banned, body)

    def test_conditional_seeding_hint_is_preserved(self):
        """It belongs in the missing-directives branch, not unconditionally."""
        self.assertIn("ops init directives", _install_sh())


class PathWarningWording(unittest.TestCase):
    """The PATH warning must promise only what actually happens.

    ~/.profile conditionally adds ~/.local/bin, but a new terminal inside
    an existing graphical login inherits the desktop environment and does
    not process .profile. The pre-fix wording ("new shells will pick it
    up automatically") was empirically false on the fresh `x` profile:
    `bash -ic 'command -v mpm'` failed while `bash -lc` succeeded.
    """

    def test_pre_fix_promise_is_absent(self):
        self.assertNotIn("new shells will pick it up automatically", _install_sh())

    def test_names_the_reliable_fix(self):
        body = _install_sh()
        self.assertIn("export PATH=", body, "the actionable hint is missing")
        self.assertIn("login", body, "a login session is the reliable fix")
        self.assertIn(
            "this shell", body, "the export must be framed as shell-scoped"
        )


class DirectivesWording(unittest.TestCase):
    """Success-path output must use current terminology.

    The term moved from "prime directives" to "directives". Comments,
    column names and metadata keys are stable technical surfaces and are
    deliberately out of scope.
    """

    def test_success_path_uses_current_term(self):
        self.assertIn("directives present (", _install_sh())

    def test_obsolete_wording_is_absent_from_user_facing_output(self):
        body = _install_sh()
        for banned in (
            'log "  ✓ prime directives present (',
            'warn "  ! no prime directives found"',
        ):
            self.assertNotIn(banned, body)


class CompletionTipWording(unittest.TestCase):
    """The INSTALL COMPLETE tip must be accurate and leak no internals."""

    def test_inaccurate_new_shells_promise_is_absent(self):
        self.assertNotIn(
            "NEW shells", _install_sh(), "empirically inaccurate inside a graphical login"
        )

    def test_user_facing_output_leaks_no_internal_names(self):
        for line in _user_facing_lines():
            self.assertNotIn(
                "phase_validate", line, f"internal impl name leaked: {line.strip()}"
            )

    def test_user_facing_output_leaks_no_obsolete_terminology(self):
        for line in _user_facing_lines():
            self.assertNotIn(
                "prime directives", line, f"stale terminology leaked: {line.strip()}"
            )

    def test_names_the_accurate_fix(self):
        self.assertIn("new login session", _install_sh())

    def test_keeps_the_runnable_canonical_path(self):
        """Stale wording must be removed, not the runnable guidance with it."""
        self.assertIn("$cli", _install_sh())


if __name__ == "__main__":
    unittest.main()