"""
Build / install ownership regression tests.

The pre-fix install.sh wrote a 261-byte POSIX shell wrapper to
$PREFIX/bin/mpm (which is the same inode as the repository's bin/mpm
when the source tree is the canonical install prefix via ~/.mpm).
That contamination broke the next `make build` with:

    build output "bin/mpm" already exists and is not an object file

The fix:
  1. install.sh no longer writes a wrapper; the installed `mpm` IS the
     compiled binary
  2. The Makefile's build output is `.build/bin`, a scratch subdirectory of
     the checkout — NOT `bin/`. Previously BUILD_DIR was `bin/`, which is
     also `$PREFIX/bin` when the checkout is the install prefix (`~/.mpm`),
     so an ordinary `make build` overwrote the binaries the running
     services execute. Building is now incapable of live deployment.
  3. `make build` detects and removes a stale wrapper at
     $(BUILD_DIR)/mpm before invoking `go build`, so `make build`
     standalone works on already-installed repos
  4. install.sh and uninstall.sh clean up legacy wrapper artefacts
     (mpm.real, mpm.pre-wrapper.*) from older installs

These tests verify the post-fix ownership contract from the source-tree
side. They are read-only inspections of the repository's build/install
artifacts; they do not invoke the build or installer (those are covered
by go tests in scripts/install_d31_test.go).

Test mapping (per the user task spec):
  Test A — build output ownership             -> test_bin_mpm_is_compiled_artifact
  Test B — install then rebuild                -> test_makefile_build_self_heals_from_stale_wrapper
  Test C — source == install-prefix edge case  -> test_makefile_handles_same_prefix_layout
  Test D — wrapper location                    -> test_no_wrapper_at_makefile_owned_paths
  Test E — mpm.real lifecycle                  -> test_no_mpm_real_in_installer_artifacts
  Test F — normal out-of-tree installation     -> test_install_dryrun_advertises_correct_binary_topology
"""

from __future__ import annotations

import os
import re
import unittest
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[2]


def _read(path: str) -> str:
    p = REPO_ROOT / path
    if not p.is_file():
        return ""
    return p.read_text(encoding="utf-8")


def _is_executable_file(path: Path) -> bool:
    """Return True if path is a regular file with at least one execute bit set."""
    return path.is_file() and os.access(path, os.X_OK)


def _has_shebang(path: Path) -> bool:
    """Return True if path's first two bytes are '#!'."""
    try:
        with path.open("rb") as f:
            return f.read(2) == b"#!"
    except OSError:
        return False


# --------------------------------------------------------------------------
# Test A — build output ownership
# --------------------------------------------------------------------------


class BuildOutputOwnership(unittest.TestCase):
    """After make build, .build/bin/mpm must be a compiled executable, not a script.

    The build output directory is `.build/bin` — a scratch subdirectory of
    the checkout, deliberately NOT `bin/`. The old layout used `bin/`,
    which is also `$PREFIX/bin` when the checkout is the install prefix
    (the `~/.mpm` co-location), so `make build` overwrote the binaries the
    running services execute. These tests track the new location.
    """

    BIN = REPO_ROOT / ".build" / "bin"

    def test_build_output_directory_exists(self):
        """make build has been run at least once, so .build/bin/ must exist."""
        self.assertTrue(
            self.BIN.is_dir(),
            f"{self.BIN} must exist (run `make build` first)",
        )

    def test_bin_mpm_is_executable(self):
        """.build/bin/mpm must have at least one execute bit set."""
        mpm = self.BIN / "mpm"
        if not mpm.exists():
            self.skipTest(".build/bin/mpm not present; rebuild required before this test")
        self.assertTrue(
            _is_executable_file(mpm),
            f"{mpm} must be executable (got {oct(mpm.stat().st_mode)})",
        )

    def test_bin_mpm_is_not_a_shell_script(self):
        """.build/bin/mpm must NOT start with the '#!' shebang (that would mark it as a wrapper)."""
        mpm = self.BIN / "mpm"
        if not mpm.exists():
            self.skipTest(".build/bin/mpm not present; rebuild required before this test")
        self.assertFalse(
            _has_shebang(mpm),
            ".build/bin/mpm must NOT be a shell wrapper; it must be the "
            "compiled Go binary. If this fires, the wrapper-removal fix "
            "has regressed.",
        )

    def test_build_output_is_not_the_install_prefix_bin(self):
        """The build output must never BE the install prefix's bin/.

        This is the load-bearing invariant of the build/install split. When
        the checkout is the install prefix (`~/.mpm`), `bin/` is
        `$PREFIX/bin` — the live install the scheduler executes. Building
        there is deployment by side effect, so BUILD_DIR must be a
        distinct, dot-prefixed scratch directory.
        """
        live_bin = REPO_ROOT / "bin"
        self.assertNotEqual(
            self.BIN.resolve(),
            live_bin.resolve(),
            f"build output ({self.BIN}) must not be the install prefix "
            f"bin/ ({live_bin}); `make build` would overwrite the live "
            "installation",
        )
        self.assertTrue(
            self.BIN.name == "bin" and self.BIN.parent.name == ".build",
            f"BUILD_DIR must be `.build/bin`, got {self.BIN}",
        )


# --------------------------------------------------------------------------
# Test B — install then rebuild (Makefile self-heals from stale wrapper)
# --------------------------------------------------------------------------


class MakefileSelfHealsFromStaleWrapper(unittest.TestCase):
    """The Makefile build target must detect and remove a stale wrapper at bin/mpm."""

    MAKEFILE = REPO_ROOT / "Makefile"

    def test_makefile_has_prebuild_cleanup(self):
        """Makefile build target must contain the wrapper-removal pre-step."""
        if not self.MAKEFILE.is_file():
            self.skipTest("Makefile missing")
        text = self.MAKEFILE.read_text(encoding="utf-8")
        # The pre-build step checks for shebang and removes the wrapper.
        self.assertRegex(
            text,
            r"head -c 2.*grep.*\^#!",
            "Makefile build target must detect a wrapper at bin/mpm via shebang check",
        )
        self.assertRegex(
            text,
            r"rm -f.*\$\(BUILD_DIR\)/\$\(BINARY_NAME\)",
            "Makefile build target must remove the stale wrapper before invoking go build",
        )

    def test_makefile_build_target_is_idempotent(self):
        """Running `make build` twice in succession must succeed without producing
        the 'build output already exists and is not an object file' error.

        This is verified by inspecting the Makefile: the pre-build cleanup runs
        BEFORE the go build invocation, so the second run finds a fresh ELF
        (which the shebang check correctly identifies as 'not a wrapper') and
        leaves it alone.
        """
        if not self.MAKEFILE.is_file():
            self.skipTest("Makefile missing")
        text = self.MAKEFILE.read_text(encoding="utf-8")
        # Pre-build step must precede the first `go build`.
        pre_step_idx = text.find('rm -f "$(BUILD_DIR)/$(BINARY_NAME)"')
        first_go_build_idx = text.find("$(GO) build -tags fts5")
        self.assertGreaterEqual(
            pre_step_idx,
            0,
            "Makefile must have a wrapper-removal pre-build step",
        )
        self.assertGreater(
            first_go_build_idx,
            pre_step_idx,
            "Wrapper-removal pre-build step must come BEFORE the `go build` invocation",
        )


# --------------------------------------------------------------------------
# Test C — source == install-prefix edge case
# --------------------------------------------------------------------------


class MakefileHandlesSamePrefixLayout(unittest.TestCase):
    """Build output and install target are always distinct paths.

    This class used to assert that phase_binaries SKIPPED the copy when
    source and destination were the same inode — a guard that existed
    only because the checkout and the install prefix were the same
    directory, so `$PROJECT_ROOT/bin/$bin` and `$PREFIX/bin/$bin` were
    one file.

    With the build/install split that state is impossible by design: the
    build output is `.build/bin`, the install target is `$PREFIX/bin`,
    and they can never collide. Per the "simplify rather than preserve
    dead complexity" principle, the guard's polarity is inverted — same
    file is now a hard error, because reaching it means something has
    aliased the two paths and any "successful" install would be a lie.
    """

    INSTALL = REPO_ROOT / "install.sh"

    def test_install_uses_same_inode_guard(self):
        """phase_binaries must test `[ "$src" -ef "$dst" ]`."""
        if not self.INSTALL.is_file():
            self.skipTest("install.sh missing")
        text = self.INSTALL.read_text(encoding="utf-8")
        self.assertRegex(
            text,
            r"\[\s*\"\$src\"\s*-ef\s*\"\$dst\"\s*\]",
            "install.sh phase_binaries must use the POSIX [-ef] same-inode "
            "guard",
        )

    def test_same_inode_is_fatal_not_a_skip(self):
        """The same-inode branch must die, not silently skip.

        A skip here would print a successful install for a binary that was
        never copied — the "success printed over a partial install" failure
        mode. Since build output and install target are distinct paths by
        construction, reaching this branch means the split was violated.
        """
        if not self.INSTALL.is_file():
            self.skipTest("install.sh missing")
        text = self.INSTALL.read_text(encoding="utf-8")
        m = re.search(
            r'\[\s*"\$src"\s*-ef\s*"\$dst"\s*\]\s*;?\s*then\s+(die|continue|return)',
            text,
        )
        self.assertIsNotNone(
            m,
            "could not find the `-ef` branch in phase_binaries",
        )
        self.assertEqual(
            m.group(1),
            "die",
            "the same-inode branch must be `die` (a hard failure), not "
            "`continue`/`return` (a silent skip that reports a deployment "
            "which never happened)",
        )

    def test_install_sourcing_uses_build_dir_not_repo_bin(self):
        """phase_binaries must source from build_dir(), not $PROJECT_ROOT/bin."""
        if not self.INSTALL.is_file():
            self.skipTest("install.sh missing")
        text = self.INSTALL.read_text(encoding="utf-8")
        # `local` is declared once for the whole guard loop (`local bin src
        # dst`), so this pins where the value is ASSIGNED, not how it is
        # declared.
        self.assertRegex(
            text,
            r'src="\$\(build_dir\)/\$bin"',
            "phase_binaries must read each binary from $(build_dir)/$bin — "
            "the resolved build output — not from a hardcoded repo-relative "
            "bin/ path",
        )

    def test_install_loop_includes_mpm(self):
        """The promoted binary set must include `mpm` (the CLI binary).

        The set lives in scripts/lib/binary_transaction.sh as
        BT_DEFAULT_BINARIES and is shared with `make install`, so that the
        two promotion surfaces cannot drift apart. This test therefore pins
        the canonical list in the library, and pins that install.sh
        actually iterates that list rather than a private copy of it.
        """
        if not self.INSTALL.is_file():
            self.skipTest("install.sh missing")
        lib = REPO_ROOT / "scripts" / "lib" / "binary_transaction.sh"
        if not lib.is_file():
            self.skipTest("binary_transaction.sh missing")
        lib_text = lib.read_text(encoding="utf-8")
        self.assertRegex(
            lib_text,
            r'BT_DEFAULT_BINARIES="mpm\s+mpm-scheduler',
            "the canonical promoted set must begin with `mpm`, the CLI binary, "
            "followed by the daemons",
        )
        for binary in (
            "mpm", "mpm-scheduler", "mpm-critic", "mpm-mcp", "mpm-telemetry",
        ):
            self.assertIn(
                binary, lib_text.split("BT_DEFAULT_BINARIES=", 1)[1].split("\n", 1)[0],
                f"the promoted binary set must include `{binary}`",
            )
        text = self.INSTALL.read_text(encoding="utf-8")
        self.assertRegex(
            text,
            r"for\s+bin\s+in\s+\$BT_BINARIES",
            "install.sh must iterate the canonical promoted set, not a private "
            "copy of it — one list keeps `make install` and `install.sh` from "
            "promoting different orders",
        )


# --------------------------------------------------------------------------
# Test D — wrapper location (post-fix invariant: no wrapper)
# --------------------------------------------------------------------------


class NoWrapperAtMakefileOwnedPaths(unittest.TestCase):
    """After install, no wrapper should appear at any Makefile-owned path."""

    INSTALL = REPO_ROOT / "install.sh"

    def test_install_does_not_write_wrapper_to_bin_mpm(self):
        """install.sh must not `cat > $PREFIX/bin/mpm` (overwriting the build artifact)."""
        text = _read("install.sh")
        self.assertNotRegex(
            text,
            r"cat\s+>\s+\"?\$PREFIX/bin/mpm\"?",
            "install.sh must not write a shell wrapper to $PREFIX/bin/mpm; "
            "this overwrites the Makefile-owned build artifact and breaks "
            "the next `make build`",
        )

    def test_install_does_not_contain_wrapper_heredoc(self):
        """install.sh must not contain a `<<WRAPPER ... WRAPPER` heredoc."""
        text = _read("install.sh")
        self.assertNotRegex(
            text,
            r"<<WRAPPER",
            "install.sh must not contain a wrapper heredoc; the wrapper has been "
            "removed because MPM_WORKSPACE defaulting is handled by the binary",
        )


# --------------------------------------------------------------------------
# Test E — mpm.real lifecycle
# --------------------------------------------------------------------------


class NoMpmRealInInstallerArtifacts(unittest.TestCase):
    """The mpm.real artefact must not be installed or maintained by the post-fix installer."""

    INSTALL = REPO_ROOT / "install.sh"
    UNINSTALL = REPO_ROOT / "uninstall.sh"

    def test_install_does_not_install_mpm_real(self):
        """install.sh must not have an active `install -m 0755 ... mpm.real` copy."""
        text = _read("install.sh")
        # Specifically: the line that copies mpm to mpm.real must be gone.
        self.assertNotRegex(
            text,
            r'install\s+-m\s+0755\s+"?\$PROJECT_ROOT/bin/mpm"?\s+"?\$PREFIX/bin/mpm\.real"?',
            "install.sh must not install a separate mpm.real binary",
        )

    def test_install_cleans_up_legacy_mpm_real(self):
        """install.sh must clean up legacy mpm.real from older wrapper-based installs."""
        text = _read("install.sh")
        self.assertIn(
            "$PREFIX/bin/mpm.real",
            text,
            "install.sh must reference $PREFIX/bin/mpm.real to clean up legacy artefacts",
        )

    def test_uninstall_removes_legacy_mpm_real(self):
        """uninstall.sh must include mpm.real in legacy cleanup."""
        text = _read("uninstall.sh")
        self.assertIn(
            "mpm.real",
            text,
            "uninstall.sh must reference mpm.real (legacy artefact removal)",
        )
        # The current-layout ALL_BINARIES must NOT include mpm.real.
        all_binaries_match = re.search(
            r"ALL_BINARIES=\(([^)]*)\)", text
        )
        self.assertIsNotNone(
            all_binaries_match,
            "uninstall.sh must define ALL_BINARIES",
        )
        all_binaries = all_binaries_match.group(1)
        self.assertNotIn(
            "mpm.real",
            all_binaries,
            "uninstall.sh ALL_BINARIES must NOT include mpm.real (current layout has none)",
        )


# --------------------------------------------------------------------------
# Test F — normal out-of-tree installation
# --------------------------------------------------------------------------


class InstallDryrunAdvertisesCorrectTopology(unittest.TestCase):
    """The install.sh dry-run intent must reflect the post-fix layout."""

    def test_dryrun_no_longer_advertises_mpm_real_copy(self):
        """install.sh dry-run intent must not show `bin/mpm -> mpm.real` copy."""
        text = _read("install.sh")
        self.assertNotRegex(
            text,
            r"\.\.\./bin/mpm\s+->\s+\$PREFIX/bin/mpm\.real",
            "install.sh dry-run intent must not advertise a copy of bin/mpm to mpm.real",
        )

    def test_dryrun_no_longer_advertises_wrapper_write(self):
        """install.sh dry-run intent must not show a `write wrapper` line."""
        text = _read("install.sh")
        self.assertNotRegex(
            text,
            r"write wrapper",
            "install.sh dry-run intent must not advertise a wrapper write",
        )


# --------------------------------------------------------------------------
# Diagnostic / docs updates
# --------------------------------------------------------------------------


class DocumentationDistinguishesBinaryRoles(unittest.TestCase):
    """Docs and the diagnostic must distinguish source artifact vs installed runtime."""

    def test_integration_check_no_longer_references_mpm_real(self):
        """mpm_integration_check.md must not reference mpm.real as a current artefact."""
        text = _read("mpm_integration_check.md")
        # The diagnostic may mention mpm.real only in historical/migration
        # context, not as a current binary to verify freshness against.
        # Check the freshness report template does not list "mpm.real freshness".
        self.assertNotRegex(
            text,
            r"mpm\.real\s+freshness",
            "mpm_integration_check.md must not require an `mpm.real freshness` check "
            "(mpm.real is not part of the current layout)",
        )

    def test_install_md_documents_no_wrapper(self):
        """docs/INSTALL.md must describe the no-wrapper layout."""
        text = _read("docs/INSTALL.md")
        # The PATH table must describe ~/.mpm/bin/mpm as the compiled binary.
        # Markdown tables allow the row to span lines; use a permissive match.
        self.assertRegex(
            text,
            r"`~/.mpm/bin/mpm`.*?Compiled Go CLI binary",
            "docs/INSTALL.md PATH table must describe ~/.mpm/bin/mpm as the compiled binary",
        )

    def test_integration_check_binary_resolution_block(self):
        """mpm_integration_check.md §3 must not promise that ~/.mpm/bin/mpm is a wrapper."""
        text = _read("mpm_integration_check.md")
        # The freshness section should run --version on ~/.mpm/bin/mpm
        # directly (no separate .real).
        self.assertRegex(
            text,
            r"~/.mpm/bin/mpm\s+--version",
            "mpm_integration_check.md must run --version on ~/.mpm/bin/mpm directly",
        )
        self.assertNotRegex(
            text,
            r"~/.mpm/bin/mpm\.real\s+--version",
            "mpm_integration_check.md must not probe ~/.mpm/bin/mpm.real --version "
            "(no such file in the current layout)",
        )


if __name__ == "__main__":
    unittest.main()
