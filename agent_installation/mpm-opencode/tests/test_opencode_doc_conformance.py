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
REGISTRY_SOURCE = REPO_ROOT / "internal" / "core" / "tools" / "registry_list.go"
LEGACY_PLUGIN_SOURCE = SCRIPT_DIR / "src" / "index.ts"
RESOLVER_SOURCE = SCRIPT_DIR / "src" / "resolve-mpm-binary.ts"
WORKSPACE_SOURCE = SCRIPT_DIR / "src" / "workspace.ts"
PACKAGE_JSON = SCRIPT_DIR / "package.json"
ADAPTER_README = SCRIPT_DIR / "README.md"
SNIPPET = SCRIPT_DIR / "templates" / "AGENTS.md.snippet"
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


class PluginDescriptionsCoverTheRegistry(unittest.TestCase):
    """Every action the registry offers must be declared in the plugin's
    description for that tool.

    WHY THIS EXISTS
    ---------------
    The plugin's tool descriptions are the only place an OpenCode agent
    learns what a tool can do — the schema the host shows is
    `params: {type: object, ...}`, a free-form envelope. So an action
    the description does not mention is an action the agent cannot
    discover, and one it cannot discover is one it will never use.

    The 2026-10-28 session-C work found thirty-one registry actions,
    across eleven of the seventeen tools, declared nowhere:

      * `mpm_references` did not declare `read` — while the managed
        block instructs the agent to "read the underlying source rather
        than relying solely on a search-result summary". The block
        promised an action the tool description did not offer.
      * `mpm_references add` said "Required params.filepath", omitting
        the `url` form the registry supports.
      * `mpm_decisions` did not declare `supersede` — while the block
        says "supersede or flag the affected record".
      * `mpm_decisions record` said "Required params.context,
        params.choice, params.rationale"; the registry requires only
        `choice`, and a live call with `choice` alone succeeds.
      * `mpm_memory` advertised `shred`, which is OPERATOR-ONLY and is
        refused on every agent transport.

    WHAT THIS PINS
    --------------
    For every tool the plugin registers, every action in that tool's
    registry action enum is DECLARED AS AN ACTION LINE in the plugin's
    description, and no description advertises an operator-only action.
    """

    @classmethod
    def setUpClass(cls) -> None:
        registry = REGISTRY_SOURCE.read_text()
        cls.actions: dict[str, list[str]] = {}
        parts = re.split(r'\n\t\tName:\s*"(mpm_[a-z_]+)"', registry)
        for i in range(1, len(parts), 2):
            name = parts[i]
            body = parts[i + 1]
            enum = re.search(
                r'"action":\s*\{[^}]*?"enum":\s*\[([^\]]*)\]', body, re.S
            )
            if enum:
                cls.actions[name] = re.findall(r'"([a-z_]+)"', enum.group(1))
        cls.descriptions = _tool_descriptions(PLUGIN_SOURCE)

    def test_the_registry_was_parsed(self) -> None:
        self.assertGreater(
            len(self.actions), 10,
            "the registry's action enums were not parsed; the guard "
            "would pass without checking anything",
        )

    def test_every_registered_action_is_declared_in_the_description(self) -> None:
        # The check is against the DECLARED ACTION LINES — the
        # "  <action> — ..." convention every description uses — and not
        # a bare substring.
        #
        # A substring check is a weaker guard than it looks: `delete`
        # occurs inside "Soft-delete a memory", so a description that
        # never declared a `delete` action still passed. It also missed
        # a real formatting defect — an appended action line merged onto
        # the previous one, so the action was present as text but not
        # as a line an agent would read as an action.
        for name, actions in sorted(self.actions.items()):
            description = self.descriptions.get(name)
            if description is None:
                # The plugin does not register this tool (mpm_work and
                # friends are CLI-only). That is a deliberate choice,
                # and the managed block names the CLI fallback for them.
                continue
            declared = set(
                re.findall(r"^\s{2}([a-z_]+)\s+—", description, re.M)
            )
            for action in actions:
                self.assertIn(
                    action,
                    declared,
                    f"{name}: the registry offers action `{action}`, but "
                    f"the plugin's description never declares it as an "
                    f"action line. An agent cannot discover an action it "
                    f"is never told about.",
                )

    def test_no_description_advertises_an_operator_only_action(self) -> None:
        # `mpm_memory action=shred` is refused on every agent transport
        # (`internal/core/tools/handlers.go`, operatorOnlyShredError), so
        # advertising it is a dead end. `mpm_handoff action=shred` is a
        # DIFFERENT action and IS agent-accessible — the guard is scoped
        # to the one that is actually refused.
        memory = self.descriptions.get("mpm_memory", "")
        self.assertNotIn(
            "  shred — ",
            memory,
            "mpm_memory advertises a `shred` action, which is "
            "operator-only and refused on every agent transport",
        )
        self.assertIn(
            "operator-only",
            memory,
            "mpm_memory should say that shred is operator-only and point "
            "at action=delete instead",
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


def _strip_comments(text: str) -> str:
    """Return the source with its comments removed.

    Used where a guard must reason about what the code DOES rather
    than about what the code SAYS about itself. A plugin header that
    documents a hook name is not a registration of it.
    """
    out = []
    in_block = False
    for line in text.splitlines():
        if in_block:
            if "*/" in line:
                line = line.split("*/", 1)[1]
                in_block = False
            else:
                continue
        while "/*" in line:
            before, after = line.split("/*", 1)
            if "*/" in after:
                line = before + after.split("*/", 1)[1]
            else:
                line = before
                in_block = True
                break
        idx = line.find("//")
        if idx >= 0:
            line = line[:idx]
        out.append(line)
    return "\n".join(out)


class ManagedSnippetNamesTheLiveHook(unittest.TestCase):
    """The managed snippet must name the hook the plugin ACTUALLY registers.

    The managed block is delivered into EVERY agent's system prompt, on
    every turn, forever. It used to name

        experimental.chat.system.transform

    — the OpenCode **V1** hook name. The V1 plugin is kept only as a
    historical reference; OpenCode 2 cannot load it, and the loadable
    plugin registers `ctx.session.hook("context")` instead. The snippet
    therefore described a hook that does not exist, in the one surface
    every agent reads.

    The hook name is DERIVED from the plugin source rather than
    hardcoded here, so a future change to the registered hook is caught
    by this guard instead of silently diverging from the instructions.

    The derivation reads the plugin's CODE, not its comments. The
    plugin's own header comment carries an architectural mapping that
    mentions `ctx.session.hook("context")` — a mention, not a
    registration. Deriving from comments would let the guard pass
    after the real registration was deleted.
    """

    @classmethod
    def setUpClass(cls) -> None:
        cls.plugin = PLUGIN_SOURCE.read_text()
        cls.plugin_code = _strip_comments(cls.plugin)
        cls.snippet = SNIPPET.read_text()

    def test_the_live_session_hook_is_named(self) -> None:
        hooks = re.findall(
            r'ctx\.session\.hook\(\s*"([^"]+)"', self.plugin_code
        )
        self.assertTrue(
            hooks,
            "no ctx.session.hook(\"...\") found in the plugin's CODE — the "
            "hook's shape changed and this guard would pass vacuously",
        )
        for hook in hooks:
            self.assertIn(
                f'ctx.session.hook("{hook}")',
                self.snippet,
                "the rendered managed snippet does not name the session hook "
                f"the plugin registers (ctx.session.hook(\"{hook}\"))",
            )

    def test_no_unlabelled_v1_hook_name_in_the_snippet(self) -> None:
        # The V1 names may appear, but only where the text says they are
        # historical — the same rule the adapter README and INSTALL.md
        # already carry.
        for v1 in ("experimental.chat.system.transform", "shell.env"):
            for paragraph in _paragraphs(self.snippet):
                if v1 not in paragraph:
                    continue
                self.assertRegex(
                    paragraph,
                    r"(?i)\bV1\b|historical|not loadable",
                    f"the rendered managed snippet mentions the retired hook "
                    f"'{v1}' without labelling it as historical: "
                    f"{paragraph.strip()[:120]!r}",
                )

    def test_no_v1_hook_name_in_the_canonical_source(self) -> None:
        # The canonical source is the operator-facing copy/paste
        # procedure. It must not send an operator looking for a hook the
        # shipped plugin does not register.
        canonical = (AGENT_INSTALLATION / "MPM_AGENT_INTEGRATION_SNIPPETS.md").read_text()
        section = _section(canonical, "OpenCode")
        for v1 in ("experimental.chat.system.transform",):
            self.assertNotIn(
                v1,
                section,
                f"the canonical source's OpenCode section names the retired "
                f"hook '{v1}' — the shipped plugin registers "
                f"ctx.session.hook(\"context\")",
            )


class PluginWakesDescriptionNamesTheOwnershipRefusals(unittest.TestCase):
    """The plugin's `mpm_wakes` description must name the same refusals
    as the registry's.

    WHY THIS EXISTS
    ---------------
    The registry's `mpm_wakes` description and the plugin's are two
    copies of one contract, for two transports. The existing V1/V2
    byte-identity rule keeps the two PLUGIN copies together; nothing
    kept the plugin and the REGISTRY together.

    That gap showed: the registry's description was corrected to name
    the `corrupt_metadata` refusal, and the plugin's was corrected for
    `corrupt_metadata` only — it still did not name `not_a_wake`, the
    refusal an agent hits most often (the wake is scheduler-owned).
    An OpenCode agent therefore got a different contract from a
    `mpm call` agent, from the same substrate.

    WHAT THIS PINS
    --------------
    The set of refusal statuses named in the registry's `mpm_wakes`
    description and the set named in the plugin's are EQUAL, and
    non-empty. The statuses are DERIVED from the two descriptions, so
    a refusal added to one surface and forgotten on the other fails
    here instead of drifting.
    """

    @classmethod
    def setUpClass(cls) -> None:
        cls.registry_desc = cls._registry_description("mpm_wakes")
        cls.plugin_desc = _tool_descriptions(PLUGIN_SOURCE).get("mpm_wakes", "")

    @staticmethod
    def _registry_description(tool: str) -> str:
        """The registered description literal for one registry tool."""
        src = REGISTRY_SOURCE.read_text()
        m = re.search(r'\n\t\tName: "%s",' % re.escape(tool), src)
        assert m, "the registry does not register %s" % tool
        body = src[m.start():]
        d = re.search(r"Description:\s*`([^`]*)`", body, re.S)
        assert d, "the registry entry for %s has no description" % tool
        return d.group(1)

    @staticmethod
    def _named_statuses(text: str) -> set[str]:
        """The refusal statuses a description names.

        The pattern is "status <name>". The prose also contains
        "success=false with a status and leaves the row untouched", so
        the English connective that follows a bare "status" is dropped
        — otherwise it would be compared as if it were a status.
        """
        found = set(re.findall(r"status ([a-z_]+)", text))
        found.discard("and")
        return found

    def test_the_registry_description_was_found(self) -> None:
        self.assertTrue(
            self.registry_desc.strip(),
            "the registry's mpm_wakes description was not found",
        )
        self.assertTrue(
            self.plugin_desc.strip(),
            "the plugin's mpm_wakes description was not found",
        )

    def test_both_descriptions_name_the_same_refusals(self) -> None:
        registry = self._named_statuses(self.registry_desc)
        plugin = self._named_statuses(self.plugin_desc)
        self.assertTrue(
            registry,
            "the registry's mpm_wakes description names no refusal status — "
            "this guard would compare two empty sets and pass vacuously",
        )
        self.assertEqual(
            registry,
            plugin,
            "the registry's mpm_wakes description names the refusal "
            f"statuses {sorted(registry)}, but the plugin's names "
            f"{sorted(plugin)}. The two surfaces teach different contracts "
            f"for the same tool.",
        )


class PluginTheoriesDescriptionNamesTheOutcome(unittest.TestCase):
    """The plugin's `mpm_theories` resolve line must name the OUTCOME param.

    WHY THIS EXISTS
    ---------------
    The plugin's tool descriptions ARE the contract for an OpenCode
    agent: the host publishes a generic passthrough envelope
    (`params: {type: object, ...}`), so the prose is the only place the
    required params and their vocabulary appear.

    The resolve line used to read:

        resolve — Resolve a pending theory. Required params.theoryId,
        params.conclusion ("confirmed"|"disproven"). Optional:
        params.newStatus ("proven"|"disproven"), params.winner_id.

    That attaches the OUTCOME ENUM to `conclusion`. The handler has
    always read `conclusion` as free text — what the evidence showed —
    and taken the outcome from `status`. So an agent that followed the
    line sent the outcome in the wrong field and was refused:

        resolve theory: status is required and must be 'proven' or 'disproven'

    The registry's published schema and the contract table were both
    corrected to the handler's real requirements; this third surface
    was not, so the plugin taught a call that fails.

    WHAT THIS PINS
    --------------
    The plugin's resolve line names `params.status` as the outcome, and
    does not attach an inline enum to `params.conclusion`. The canonical
    `params.id` leads, with the alias named beside it, so the alias is
    still discoverable.
    """

    @classmethod
    def setUpClass(cls) -> None:
        cls.description = _tool_descriptions(PLUGIN_SOURCE).get("mpm_theories", "")

    def _resolve_line(self) -> str:
        m = re.search(r"^\s*resolve\s+—.*$", self.description, re.M)
        self.assertIsNotNone(
            m,
            "the plugin's mpm_theories description has no resolve action line",
        )
        return m.group(0)

    def test_the_description_was_found(self) -> None:
        self.assertTrue(
            self.description.strip(),
            "the plugin's mpm_theories description was not found",
        )

    def test_the_resolve_line_names_the_identity_param(self) -> None:
        line = self._resolve_line()
        self.assertIn(
            "params.id",
            line,
            "the plugin's mpm_theories resolve line does not name "
            "params.id. The canonical identity param is `id`; the "
            "handler reads `id` first, and only falls back to the "
            "aliases.",
        )
        self.assertIn(
            "params.theoryId",
            line,
            "the plugin's mpm_theories resolve line does not name the "
            "params.theoryId alias. The alias is still supported by the "
            "handler, so an agent that only knows the alias must not be "
            "told it does not exist.",
        )

    def test_the_resolve_line_names_the_outcome_param(self) -> None:
        line = self._resolve_line()
        self.assertIn(
            "params.status",
            line,
            "the plugin's mpm_theories resolve line does not name "
            "params.status as the outcome. The handler reads the outcome "
            "from `status`, so an agent that does not know about it sends "
            "the outcome in the wrong field and is refused.",
        )

    def test_the_outcome_enum_is_not_attached_to_conclusion(self) -> None:
        line = self._resolve_line()
        self.assertIsNone(
            re.search(r"conclusion\s*\(\s*\"", line),
            "the plugin's mpm_theories resolve line attaches an inline enum "
            "to params.conclusion. The handler reads `conclusion` as free "
            "text and takes the outcome from `status`, so an enum on "
            f"conclusion tells the agent to put the outcome in the wrong "
            f"field: {line.strip()!r}",
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
