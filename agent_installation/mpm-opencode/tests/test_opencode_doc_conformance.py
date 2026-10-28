"""
test_opencode_doc_conformance.py — the OpenCode documentation must agree
with the OpenCode source.

WHY THIS EXISTS
---------------
The 2026-10-27 OpenCode-only release audit found the OpenCode
documentation describing a plugin that no longer exists:

  * the adapter's own README and the canonical `agent_installation/INSTALL.md`
    told the operator to create a SYMLINK at
    `~/.config/opencode/plugin/mpm-opencode` and claimed "there is no
    shell-based install.sh for this adapter". The loadable plugin is
    registered as a `plugins` ARRAY ENTRY in `~/.config/opencode/opencode.jsonc`
    pointing at the package DIRECTORY; `install.sh` is the installer and
    does the registration, the build, and the AGENTS.md managed block;
  * both documents named the OpenCode **V1** hooks
    (`experimental.chat.system.transform`, `shell.env`) as if they were
    the live surface. The V2 plugin registers `ctx.session.hook("context")`
    and `ctx.shell.hook("create.before")`;
  * both documents claimed the tools are `mpm__`-prefixed. The OpenCode
    adapter registers the BARE canonical names (`mpm_memory`, ...) — the
    `mpm__` prefix belongs to hosts that namespace MCP tools;
  * `agent_installation/INSTALL.md` said the binary is resolved from `$PATH`
    and that `MPM_WORKSPACE` defaults to `<cwd>`. The source resolves the
    binary deterministically and defaults the workspace to `$HOME/.mpm`;
  * `agent_installation/INSTALL.md` said "Node.js 18+"; `package.json`
    declares `engines.node: ">=20"`.

Every one of those statements was a *documentation* defect: the code was
right and the prose was wrong. A prose defect in an installation document
is not cosmetic — it is the thing an operator follows.

WHAT THIS PINS
--------------
This file derives the facts from the SOURCE (the plugin, the resolver,
package.json) and asserts the documents state them. It is the same
"doc-conformance" pattern the existing installer test already uses for
the AGENTS.md layout, extended to the claims that actually drifted.

It also pins the retirement of `AUTO_AGENT_INSTALL.md`: the file must not
come back, and no tracked file may reference it (a dangling pointer to a
deleted document is a defect of the same class).

Run with:
    python3 -m unittest agent_installation.mpm-opencode.tests.test_opencode_doc_conformance
"""

from __future__ import annotations

import json
import re
import unittest
from pathlib import Path

SCRIPT_DIR = Path(__file__).resolve().parent.parent
AGENT_INSTALLATION = SCRIPT_DIR.parent
REPO_ROOT = AGENT_INSTALLATION.parent

PLUGIN_SOURCE = SCRIPT_DIR / "src" / "index.v2.ts"
LEGACY_PLUGIN_SOURCE = SCRIPT_DIR / "src" / "index.ts"
RESOLVER_SOURCE = SCRIPT_DIR / "src" / "resolve-mpm-binary.ts"
WORKSPACE_SOURCE = SCRIPT_DIR / "src" / "workspace.ts"
PACKAGE_JSON = SCRIPT_DIR / "package.json"
ADAPTER_README = SCRIPT_DIR / "README.md"
INSTALL_DOC = AGENT_INSTALLATION / "INSTALL.md"
BUNDLE_README = AGENT_INSTALLATION / "README.md"
RENDERER = AGENT_INSTALLATION / "scripts" / "render_managed_blocks.py"

RETIRED_DOC = "AUTO_AGENT_INSTALL.md"


def _tool_array_source(text: str, name: str) -> str:
    """Return the body of `const <name> = [ ... ]` from the plugin source."""
    match = re.search(
        r"const\s+" + name + r"\s*(?::[^=]+)?=\s*\[(.*?)\n\];", text, re.S
    )
    if not match:
        raise AssertionError(f"could not locate the {name} array in the plugin source")
    return match.group(1)


def _section(text: str, heading: str) -> str:
    """Return the body of one `## heading` section of a Markdown document.

    Assertions about a *host* must be scoped to that host's section:
    `agent_installation/INSTALL.md` is a multi-host document, and the
    shared protocol section legitimately names other hosts' tool-name
    prefixes. A whole-document assertion would either miss real drift or
    fire on correct text about a different host.
    """
    start = re.search(rf"^## {re.escape(heading)}\s*$", text, re.M)
    if not start:
        raise AssertionError(f"no '## {heading}' section found")
    rest = text[start.end() :]
    end = re.search(r"^## ", rest, re.M)
    return rest[: end.start()] if end else rest


def _paragraphs(text: str) -> list[str]:
    """Split a document into blank-line-separated blocks.

    A labelling sentence may sit on the line *after* the one that names a
    retired hook, so the V1-name check runs per paragraph, not per line.
    """
    return [block for block in re.split(r"\n\s*\n", text) if block.strip()]


class ToolSurfaceFacts(unittest.TestCase):
    """The facts about the tool surface, derived from the source."""

    @classmethod
    def setUpClass(cls) -> None:
        source = PLUGIN_SOURCE.read_text()
        cls.domain = re.findall(
            r'name:\s*"(mpm_[a-z_]+)"', _tool_array_source(source, "DOMAIN_TOOLS")
        )
        cls.standalone = re.findall(
            r'name:\s*"(mpm_[a-z_]+)"', _tool_array_source(source, "STANDALONE_TOOLS")
        )
        cls.all = cls.domain + cls.standalone

    def test_tool_counts_are_stable(self) -> None:
        # If a tool is added or removed, these counts change and the
        # documents must be updated with them. That is the point: the
        # documents are pinned to the source, not to a remembered number.
        self.assertEqual(len(self.domain), 14, "domain tool count changed")
        self.assertEqual(len(self.standalone), 3, "standalone tool count changed")
        self.assertEqual(len(self.all), 17, "total tool count changed")

    def test_tool_names_are_bare_not_prefixed(self) -> None:
        # The OpenCode adapter registers the canonical names verbatim.
        # A `mpm__` prefix would mean the plugin and the managed block
        # disagree about how the agent addresses a tool.
        for name in self.all:
            self.assertFalse(
                name.startswith("mpm__"),
                f"{name} is prefixed; the OpenCode adapter registers bare names",
            )

    def test_renderer_agrees_on_the_opencode_tool_prefix(self) -> None:
        # `render_managed_blocks.py` decides whether the managed block
        # tells the agent to call `mpm_memory` or `mpm__mpm_memory`. The
        # plugin must agree with it.
        text = RENDERER.read_text()
        entry = re.search(
            r'"name":\s*"mpm-opencode".*?"tool_prefix":\s*"([^"]*)"', text, re.S
        )
        self.assertIsNotNone(entry, "the renderer no longer registers mpm-opencode")
        self.assertEqual(
            entry.group(1),
            "",
            "the OpenCode tool prefix changed; the plugin and the managed "
            "block must be updated together",
        )


def _tool_descriptions(path: Path) -> dict[str, str]:
    """Map every registered tool to its description literal.

    The two plugin sources declare their tools in DIFFERENT shapes:

      * `src/index.v2.ts` — a `DOMAIN_TOOLS` / `STANDALONE_TOOLS`
        array of `{ name: "mpm_x", description: ... }` objects. The
        domain descriptions are template literals; the standalone ones
        are double-quoted strings.
      * `src/index.ts` — 14 `registerDomainTool(bin, tools, { name:
        "mpm_x", description: ... })` calls plus 3
        `tools.mpm_x = tool({ description: ... })` assignments.

    So the parser looks for a tool NAME, then takes the FIRST
    `description:` literal after it. That covers all four shapes
    without hard-coding any of them.
    """
    text = path.read_text()
    out: dict[str, str] = {}
    for match in re.finditer(
        r'(?:name:\s*"|tools\.)(mpm_[a-z_]+)"?', text
    ):
        name = match.group(1)
        if name in out:
            continue
        rest = text[match.end() :]
        desc = re.search(
            r'description:\s*(?:`([^`]*)`|"((?:[^"\\]|\\.)*)")',
            rest,
            re.S,
        )
        if desc:
            out[name] = desc.group(1) if desc.group(1) is not None else desc.group(2)
    return out


class LegacyPluginSourceAgrees(unittest.TestCase):
    """The legacy plugin source must not teach a different tool surface.

    WHY THIS EXISTS
    ---------------
    The adapter carries TWO implementations of the plugin's tool
    surface:

      * `src/index.v2.ts` — the 2026-10-16 port to the OpenCode 2
        plugin API. This is the module OpenCode loads: `package.json`'s
        `main` is `./dist/index.v2.js`, and the root `server.js`
        re-exports it.
      * `src/index.ts` — the earlier implementation, in a different
        shape (14 `registerDomainTool` calls plus 3 `tools.mpm_x =
        tool({...})` assignments). It compiles to `dist/index.js`,
        which **nothing in the default installation loads**.

    Both register all 17 tools, and today every description is
    identical — but nothing ENFORCES that. So it is a trap: it is the
    file a maintainer is most likely to edit by mistake (it sorts
    first, and it carries the older layout), and a correction applied
    ONLY to it compiles cleanly, passes every test in this repository,
    and **never reaches the agent**.

    The 2026-10-28 session-C work hit exactly that: a `mpm_handoff`
    description correction was applied to `src/index.ts` first, and
    the built `dist/index.v2.js` — the file OpenCode actually loads —
    still carried the wrong text.

    WHAT THIS PINS
    --------------
    For every tool both files register, the description text is
    identical. A correction must be applied to both, and this test is
    what says so.
    """

    @classmethod
    def setUpClass(cls) -> None:
        cls.loaded = _tool_descriptions(PLUGIN_SOURCE)
        cls.legacy = _tool_descriptions(LEGACY_PLUGIN_SOURCE)

    def test_the_legacy_source_is_not_the_loaded_entry_point(self) -> None:
        # The premise of this class. If the loaded entry point ever
        # changes, the premise changes and the class must be revisited
        # rather than silently asserting the wrong thing.
        package = json.loads(PACKAGE_JSON.read_text())
        self.assertEqual(
            package.get("main"), "./dist/index.v2.js",
            "the loaded plugin entry point changed; this test's premise "
            "must be re-derived",
        )
        server = (SCRIPT_DIR / "server.js").read_text()
        self.assertIn("./dist/index.v2.js", server)

    def test_both_sources_register_the_same_tools(self) -> None:
        self.assertEqual(
            sorted(self.legacy),
            sorted(self.loaded),
            "the two plugin sources register different tool sets",
        )
        self.assertEqual(
            len(self.loaded), 17,
            "the loaded plugin's tool count changed",
        )

    def test_the_shared_tool_descriptions_are_identical(self) -> None:
        shared = sorted(set(self.loaded) & set(self.legacy))
        self.assertTrue(
            shared,
            "no tool is registered by both sources; the parsers or the "
            "sources changed shape",
        )
        for name in shared:
            self.assertEqual(
                self.loaded[name],
                self.legacy[name],
                f"{name}: the loaded plugin (src/index.v2.ts) and the "
                f"legacy plugin (src/index.ts) describe it differently. "
                f"A correction applied to only one of them compiles "
                f"cleanly and never reaches the agent.",
            )


class DocumentationAgreesWithSource(unittest.TestCase):
    """The prose must state what the source does."""

    @classmethod
    def setUpClass(cls) -> None:
        cls.adapter_readme = ADAPTER_README.read_text()
        cls.install_doc = INSTALL_DOC.read_text()
        cls.bundle_readme = BUNDLE_README.read_text()
        # The OpenCode-scoped view of the canonical install document.
        cls.install_doc_opencode = _section(cls.install_doc, "OpenCode")

    def test_only_opencode_is_marked_supported(self) -> None:
        # The release supports one host. The bundle README's "Supported
        # hosts" table is the place that claim is made, so it is the place
        # that must be unambiguous: exactly one row may claim support, and
        # it must be OpenCode.
        section = _section(self.bundle_readme, "Supported hosts")
        rows = [line for line in section.splitlines() if line.startswith("|")]
        # skip the header and the separator row
        data_rows = [r for r in rows if not re.match(r"^\|[\s\-:|]+\|$", r)]
        data_rows = data_rows[1:]
        self.assertTrue(data_rows, "the Supported hosts table has no data rows")
        supported = []
        for row in data_rows:
            cells = [c.strip() for c in row.strip("|").split("|")]
            host = cells[0].replace("*", "")
            status = cells[1].lower()
            if "not supported" in status:
                continue
            if "supported" in status:
                supported.append(host)
        self.assertEqual(
            supported,
            ["OpenCode"],
            f"exactly one host may be marked supported; found {supported}",
        )

    def test_bundle_readme_entry_point_is_the_installer(self) -> None:
        # The bundle README carried the same wrong symlink instruction as
        # the adapter README, and it is the document the root README
        # links to as "Supported integrations".
        self.assertNotIn(
            "ln -s",
            self.bundle_readme,
            "the agent_installation README must not instruct a plugin symlink",
        )
        self.assertNotIn(
            ".config/opencode/plugin/mpm-opencode",
            self.bundle_readme,
            "the agent_installation README must not point at the "
            "non-existent symlink path",
        )
        self.assertIn(
            "mpm-opencode && ./install.sh",
            self.bundle_readme,
            "the agent_installation README must name the adapter installer",
        )

    def test_documents_state_the_current_tool_count(self) -> None:
        source = PLUGIN_SOURCE.read_text()
        domain = len(
            re.findall(
                r'name:\s*"(mpm_[a-z_]+)"', _tool_array_source(source, "DOMAIN_TOOLS")
            )
        )
        standalone = len(
            re.findall(
                r'name:\s*"(mpm_[a-z_]+)"', _tool_array_source(source, "STANDALONE_TOOLS")
            )
        )
        self.assertIn(
            f"{domain} Domain Tools",
            self.adapter_readme,
            "the adapter README must state the domain tool count",
        )
        self.assertIn(
            f"{domain} Domain Tools",
            self.install_doc,
            "INSTALL.md must state the domain tool count",
        )
        for name, text in (
            ("adapter README", self.adapter_readme),
            ("INSTALL.md", self.install_doc_opencode),
        ):
            self.assertIn(
                "17 tool registrations",
                text,
                f"the {name} must state the total tool count",
            )

    def test_documents_do_not_claim_an_mpm_prefixed_tool_surface(self) -> None:
        # `mpm__mpm_memory` is what a Claude Code / OpenClaw agent sees.
        # An OpenCode agent sees `mpm_memory`. Telling the operator to
        # verify `mpm__`-prefixed registrations makes a healthy install
        # look broken.
        for name, text in (
            ("adapter README", self.adapter_readme),
            ("INSTALL.md (OpenCode section)", self.install_doc_opencode),
        ):
            self.assertNotIn(
                "mpm__mpm_",
                text,
                f"the {name} must not claim an mpm__-prefixed tool surface",
            )
            self.assertNotIn(
                "mpm__ prefixed",
                text,
                f"the {name} must not claim an mpm__-prefixed tool surface",
            )

    def test_documents_do_not_instruct_a_plugin_symlink(self) -> None:
        # The OpenCode 2 loader reads the `plugins` array in
        # opencode.jsonc. A symlink under ~/.config/opencode/plugin/ is
        # not consulted, so instructing the operator to create one
        # produces a broken install that *looks* installed.
        for name, text in (
            ("adapter README", self.adapter_readme),
            ("INSTALL.md (OpenCode section)", self.install_doc_opencode),
        ):
            self.assertNotIn(
                "ln -s",
                text,
                f"the {name} must not instruct a plugin symlink",
            )
            self.assertNotIn(
                ".config/opencode/plugin/mpm-opencode",
                text,
                f"the {name} must not point at the non-existent symlink path",
            )

    def test_documents_name_the_registration_mechanism(self) -> None:
        for name, text in (
            ("adapter README", self.adapter_readme),
            ("INSTALL.md (OpenCode section)", self.install_doc_opencode),
        ):
            self.assertIn(
                "opencode.jsonc",
                text,
                f"the {name} must name the file the plugin is registered in",
            )
            self.assertIn(
                "plugins",
                text,
                f"the {name} must name the plugins array the entry lives in",
            )

    def test_documents_name_the_live_v2_hooks(self) -> None:
        self.assertIn(
            'ctx.session.hook("context")',
            self.adapter_readme,
            "the adapter README must name the live wake-context hook",
        )
        self.assertIn(
            'ctx.shell.hook("create.before")',
            self.adapter_readme,
            "the adapter README must name the live provenance hook",
        )
        self.assertIn(
            'ctx.session.hook("context")',
            self.install_doc_opencode,
            "INSTALL.md must name the live wake-context hook",
        )

    def test_v1_hook_names_appear_only_in_labelled_history(self) -> None:
        # The V1 names are allowed to appear, but only where the document
        # says they are the V1 (unloadable) names. A bare assertion that
        # the plugin "delivers wake context via
        # experimental.chat.system.transform" is the drift this pins.
        v1_names = ("experimental.chat.system.transform", "shell.env")
        for name, text in (
            ("adapter README", self.adapter_readme),
            ("INSTALL.md (OpenCode section)", self.install_doc_opencode),
        ):
            for paragraph in _paragraphs(text):
                for v1 in v1_names:
                    if v1 not in paragraph:
                        continue
                    self.assertRegex(
                        paragraph,
                        r"(?i)\bV1\b|historical|not loadable",
                        f"the {name} mentions the V1 hook '{v1}' without "
                        f"labelling it as historical: {paragraph.strip()[:120]!r}",
                    )

    def test_documents_do_not_link_a_retired_adapter(self) -> None:
        # The OpenCode adapter is not documented as a chapter of another
        # host's integration. A link to a directory that is being removed
        # from the release is a dangling pointer.
        self.assertNotIn(
            "mpm-claude-code/CLAUDE_CODE_INTEGRATION.md",
            self.adapter_readme,
            "the adapter README must not depend on another host's document",
        )
        self.assertNotIn(
            "mpm-memory-openclaw",
            self.adapter_readme,
            "the adapter README must not depend on another host's directory",
        )

    def test_documents_state_the_deterministic_binary_resolution(self) -> None:
        # The source resolves the binary deterministically; the document
        # must not tell the operator that $PATH is enough.
        resolver = RESOLVER_SOURCE.read_text()
        self.assertIn("MPM_BINARY", resolver)
        for token in (".mpm/bin/mpm", ".local/bin/mpm", "command -v mpm"):
            self.assertIn(
                token,
                resolver,
                f"the resolver's documented order no longer contains {token}",
            )
        for name, text in (
            ("adapter README", self.adapter_readme),
            ("INSTALL.md (OpenCode section)", self.install_doc_opencode),
        ):
            self.assertIn(
                "MPM_BINARY",
                text,
                f"the {name} must document the MPM_BINARY override",
            )
            self.assertIn(
                "$HOME/.mpm/bin/mpm",
                text,
                f"the {name} must document the canonical binary location",
            )

    def test_documents_state_the_workspace_default(self) -> None:
        workspace = WORKSPACE_SOURCE.read_text()
        self.assertIn(
            'path.join(os.homedir(), ".mpm")',
            workspace,
            "the workspace default changed",
        )
        for name, text in (
            ("adapter README", self.adapter_readme),
            ("INSTALL.md (OpenCode section)", self.install_doc_opencode),
        ):
            self.assertIn(
                "MPM_WORKSPACE",
                text,
                f"the {name} must document MPM_WORKSPACE",
            )
        self.assertIn(
            "defaults to `$HOME/.mpm`",
            self.install_doc_opencode,
            "INSTALL.md must state the real workspace default ($HOME/.mpm, not <cwd>)",
        )

    def test_documents_state_the_declared_node_requirement(self) -> None:
        engines = json.loads(PACKAGE_JSON.read_text()).get("engines", {})
        node = str(engines.get("node", ""))
        match = re.search(r">=(\d+)", node)
        self.assertIsNotNone(match, f"unparsable engines.node: {node!r}")
        minimum = int(match.group(1))
        self.assertIn(
            f"Node.js {minimum}+",
            self.install_doc_opencode,
            "INSTALL.md must state the Node.js version package.json declares",
        )

    def test_documents_point_at_the_adapter_installer(self) -> None:
        # The adapter ships install.sh; the documents must use it rather
        # than re-implementing a manual sequence that bypasses the build
        # and the JSONC-safe config writer.
        for name, text in (
            ("adapter README", self.adapter_readme),
            ("INSTALL.md (OpenCode section)", self.install_doc_opencode),
        ):
            self.assertIn(
                "install.sh",
                text,
                f"the {name} must point at the adapter's own installer",
            )
        self.assertFalse(
            "There is no shell-based `install.sh` for this adapter" in self.adapter_readme,
            "the adapter README still denies that install.sh exists",
        )


class RetiredDocument(unittest.TestCase):
    """AUTO_AGENT_INSTALL.md was retired; it must not come back, and
    nothing may point at it."""

    def test_the_document_does_not_exist(self) -> None:
        self.assertFalse(
            (REPO_ROOT / RETIRED_DOC).exists(),
            f"{RETIRED_DOC} was retired in favour of "
            f"agent_installation/INSTALL.md — do not reintroduce it",
        )

    def test_nothing_references_the_retired_document(self) -> None:
        # The guard files themselves necessarily name the retired document.
        # They are the only allowed references; everything else must have
        # been cleaned up in the same change that retired it.
        allowed = {
            "agent_installation/mpm-opencode/tests/test_opencode_doc_conformance.py",
            "agent_installation/tests/test_naming_invariants.py",
        }
        offenders: list[str] = []
        for path in sorted(REPO_ROOT.rglob("*")):
            if not path.is_file():
                continue
            rel = path.relative_to(REPO_ROOT)
            parts = rel.parts
            if parts[0] in {".git", "node_modules", "__pycache__"}:
                continue
            if any(p in {"node_modules", "__pycache__", "dist", ".build"} for p in parts):
                continue
            if path.suffix in {".png", ".jpg", ".gif", ".pdf", ".pyc", ".so"}:
                continue
            if rel.as_posix() in allowed:
                continue
            try:
                text = path.read_text(encoding="utf-8")
            except (UnicodeDecodeError, OSError):
                continue
            if RETIRED_DOC in text:
                offenders.append(rel.as_posix())
        self.assertEqual(
            offenders,
            [],
            f"{RETIRED_DOC} is retired but is still referenced by: {offenders}",
        )

    def test_the_canonical_install_doc_carries_the_agent_procedure(self) -> None:
        # The retired document was the agent-directed entry point. Its
        # still-necessary content was folded into the canonical
        # installation authority; these are its load-bearing rules.
        text = INSTALL_DOC.read_text()
        self.assertIn(
            "## Installing MPM from an agent",
            text,
            "the canonical install doc must carry the agent-directed procedure",
        )
        for phrase in (
            "Never overwrite user working instructions",
            "Verify behavioural adoption",
            "Verify idempotency",
            "Verify safe failure behaviour",
            "The architecture-idempotency contract",
            "If no adapter exists for your host",
            "Source-of-truth hierarchy",
        ):
            self.assertIn(
                phrase,
                text,
                f"the canonical install doc lost the '{phrase}' rule",
            )


if __name__ == "__main__":
    unittest.main()
