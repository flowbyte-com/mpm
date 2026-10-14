"""
test_reference_discovery_contract.py — regression coverage for the
agent reference-discovery defect (issue #25).

The incident, in the shape an agent experienced it:

    user:   "does MPM have the WordPress API docs?"
    agent:  searches memories -> nothing -> "no, MPM has no
            WordPress documentation"
    truth:  reference_docs row ebcd27dfc078ed31, 33 WordPress
            documents, the REST API reference including
            wp_update_nav_menu_item, sitting in the corpus the whole
            time.

Nothing about retrieval was broken. The document was findable. The
defect is that no surface an agent reads BEFORE deciding what to look
at told it the reference corpus is a separate subsystem you query
yourself. The managed block taught seven invariants, none about
references. The mpm_references MCP description was written entirely
from the ingestion side and said nothing about searching or reading.
An agent that had no other reason to look there did not look there,
and correctly reasoned from the wrong subsystem.

The fix lives at the canonical integration source, not in generated
files: agent_installation/MPM_AGENT_INTEGRATION_SNIPPETS.md gains
item 8, which the renderer propagates to every adapter. These tests
pin that the guidance actually reaches agents, survives install, and
does not drift into documentation of something that does not exist.

The seven properties this file pins, one per requirement:

  1. The generated managed block includes actionable
     reference-search guidance.
  2. The commands and tool names documented actually exist.
  3. Installing or refreshing the integration retains the guidance.
  4. Repeated installation remains idempotent.
  5. User-authored instructions outside the managed block are
     preserved.
  6. The guidance reaches every supported host through that host's
     REAL installation path (its own installer, driven the way the
     root installer drives it).
  7. These tests run through the canonical gated suite rather than an
     orphaned target.

Isolation: every test that installs or reconciles sets HOME to a
fresh tmp dir, so the operator's real ~/.claude/CLAUDE.md,
~/.pi/agent/AGENTS.md, ~/.config/opencode/AGENTS.md and
~/.openclaw/workspace/SOUL.md are never read as inputs and never
written. Tests 6 and 5 assert on sandbox files only.

The Go-side counterpart — pinning the mpm_references MCP description
so the discovery fix cannot regress independently of the block — is
internal/core/tools/registry_reference_discovery_test.go.
"""

from __future__ import annotations

import importlib.util
import json
import os
import re
import shutil
import sqlite3
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path


AGENT_INSTALLATION = Path(__file__).resolve().parent.parent
REPO_ROOT = AGENT_INSTALLATION.parent
RENDER_SCRIPT = AGENT_INSTALLATION / "scripts" / "render_managed_blocks.py"
RECONCILE_SCRIPT = AGENT_INSTALLATION / "scripts" / "reconcile_managed_blocks.py"
CANONICAL_SOURCE = AGENT_INSTALLATION / "MPM_AGENT_INTEGRATION_SNIPPETS.md"
MPM_BIN = REPO_ROOT / ".build" / "bin" / "mpm"


def _load_render_module():
    spec = importlib.util.spec_from_file_location(
        "render_managed_blocks_refdisc", RENDER_SCRIPT,
    )
    assert spec and spec.loader, "render script must be importable"
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


_render = _load_render_module()


def _canonical_block() -> str:
    return _render.extract_canonical_block(
        CANONICAL_SOURCE.read_text(encoding="utf-8"),
    )


def _unwrapped(text: str) -> str:
    """Collapse the block's hard wrapping for phrase matching.

    The managed block is hard-wrapped at ~66 columns with a 3-space
    continuation indent, so a sentence like "separate subsystems" is
    split across a newline and its continuation indent in the file. A
    phrase assertion against the raw bytes therefore fails on the
    wrapping rather than on the wording, which makes these tests
    sensitive to line breaks nobody cares about. Collapsing
    whitespace compares the sentence an agent reads, not the column
    the author happened to wrap at.
    """
    return re.sub(r"\s+", " ", text)


def _block_contains(phrase: str) -> bool:
    return _unwrapped(_canonical_block()).find(_unwrapped(phrase)) != -1


def _rendered_block_for(adapter_name: str) -> str:
    adapter = next(a for a in _render.ADAPTERS if a["name"] == adapter_name)
    return _render.render_for_host(_canonical_block(), adapter["tool_prefix"])


# The stable label. Referenced by the invariant test in
# test_render_managed_blocks.py; kept here so a wording change to the
# body does not require hunting through two files to find out what
# changed.
GUIDANCE_LABEL = "Search references before concluding external material is"


def _has_mcp_binary() -> bool:
    return MPM_BIN.is_file() and os.access(MPM_BIN, os.X_OK)


class ReferenceGuidanceIsPresent(unittest.TestCase):
    """Property 1 — the guidance is actionable, not merely present.

    "Actionable" is doing real work in that sentence. A block that
    says references exist teaches an agent nothing; what changes the
    incident is naming the two operations that answer the question
    (search, then read the document), naming both the CLI and the MCP
    route, and saying why the memory subsystem cannot stand in for
    the reference subsystem.
    """

    def test_canonical_block_carries_the_guidance_label(self):
        self.assertIn(
            GUIDANCE_LABEL, _canonical_block(),
            "the canonical managed block must carry the reference-search "
            "invariant; without it an agent has no instruction to consult "
            "the reference corpus at all",
        )

    def test_canonical_block_states_the_two_subsystems_are_separate(self):
        self.assertTrue(
            _block_contains(
                "Memories and references are separate subsystems"
            ),
            "the block must state that memories and references are different "
            "subsystems — that inference is the incident",
        )
        self.assertTrue(
            _block_contains("an empty memory search is not evidence of absence"),
            "the block must say an empty memory search does not establish "
            "absence; without it the two subsystems are indistinguishable",
        )

    def test_canonical_block_names_both_lookup_operations(self):
        block = _canonical_block()
        # Search alone is not enough: an agent that searches and acts
        # on the snippet it got back has still not read the document.
        self.assertIn("`mpm reference search", block,
                      "the block must name the CLI search command")
        self.assertIn("`mpm reference show", block,
                      "the block must name the CLI read command; a search "
                      "snippet is not the document")
        self.assertIn("action `search`", block,
                      "the block must name the MCP search action")
        self.assertIn("action `read`", block,
                      "the block must name the MCP read action")

    def test_canonical_block_requires_reading_the_document(self):
        self.assertTrue(
            _block_contains("not a search-result summary"),
            "the block must tell the agent to read the document, not stop "
            "at the search-result summary",
        )

    def test_canonical_block_requires_a_freshness_check(self):
        self.assertIn(
            "Check version and freshness", _canonical_block(),
            "the block must require a freshness check before technical "
            "documentation is relied on — a stale reference is a wrong "
            "answer that looks like a right one",
        )


class ReferenceGuidanceReachesEveryHost(unittest.TestCase):
    """The block is generated per host; the guidance must survive
    generation for all of them.

    Generation rewrites tool names per host's transport prefix. A
    guidance sentence that names a tool by its bare registry name in
    the canonical source renders correctly only if the renderer's
    token rule fires — and if it does not, the host receives a
    sentence naming a tool it cannot call. Both failure directions
    matter, so each host is asserted on the PREFIXED name.
    """

    def test_every_adapter_render_carries_the_guidance(self):
        for adapter in _render.ADAPTERS:
            with self.subTest(adapter=adapter["name"]):
                rendered = _rendered_block_for(adapter["name"])
                self.assertIn(
                    GUIDANCE_LABEL, rendered,
                    f"{adapter['name']}: the generated block lost the "
                    f"reference-search guidance",
                )

    def test_every_adapter_render_names_its_own_prefixed_tool(self):
        for adapter in _render.ADAPTERS:
            prefix = adapter["tool_prefix"]
            expected = f"{prefix}mpm_references"
            with self.subTest(adapter=adapter["name"]):
                rendered = _rendered_block_for(adapter["name"])
                self.assertIn(
                    expected, rendered,
                    f"{adapter['name']}: the block must name {expected} — "
                    f"the tool as THIS host's transport actually spells it",
                )
                if prefix:
                    # A bare name alongside the prefixed one means the
                    # renderer rewrote one occurrence and missed the
                    # other, or the source drifted. Either way the host
                    # is being told about a tool name it may not have.
                    self.assertNotIn(
                        "`mpm_references`", rendered,
                        f"{adapter['name']}: unprefixed mpm_references "
                        f"survived rendering for a {prefix!r} host",
                    )

    def test_every_adapter_render_keeps_the_cli_form_unprefixed(self):
        """The CLI spelling contains a space, so the renderer's token
        rule cannot rewrite it — and must not. An agent told to run
        `mcp__mpm__ reference search` would run a command that does not
        exist.

        The check is scoped to backticked spans naming a reference
        SUBCOMMAND. A broad `mpm__` sweep would flag legitimate MCP
        tool names like `mcp__mpm__mpm_context`, which are supposed to
        carry the prefix.
        """
        for adapter in _render.ADAPTERS:
            prefix = adapter["tool_prefix"]
            with self.subTest(adapter=adapter["name"]):
                rendered = _rendered_block_for(adapter["name"])
                self.assertIn("`mpm reference search", rendered)
                self.assertIn("`mpm reference show", rendered)
                if not prefix:
                    continue
                for mangled in re.findall(r"`[^`]*reference (?:search|show)[^`]*`",
                                          rendered):
                    # Look for the prefix verbatim. `rstrip("_")` would
                    # reduce "mpm__" to "mpm" and then match the CORRECT
                    # spelling, so the separator has to stay.
                    self.assertNotIn(
                        prefix, mangled,
                        f"{adapter['name']}: a CLI reference subcommand was "
                        f"tool-prefixed into {mangled!r}, which is not a "
                        f"command that exists",
                    )
                    self.assertTrue(
                        mangled.lstrip("`").startswith("mpm reference"),
                        f"{adapter['name']}: the CLI command must start with "
                        f"the literal `mpm reference`, got {mangled!r}",
                    )

    def test_generated_snippet_files_are_in_parity(self):
        """The adapter snippets committed to the repo are what the
        installers actually feed to hosts, so they are checked as
        files, not recomputed.
        """
        for adapter in _render.ADAPTERS:
            snippet = (
                AGENT_INSTALLATION / adapter["name"] / "templates"
                / _snippet_name_for(adapter["name"])
            )
            with self.subTest(adapter=adapter["name"]):
                self.assertTrue(snippet.is_file(), f"missing snippet {snippet}")
                text = snippet.read_text(encoding="utf-8")
                self.assertIn(GUIDANCE_LABEL, text)
                self.assertIn(f"{adapter['tool_prefix']}mpm_references", text)

    def test_renderer_check_gate_passes(self):
        """`render_managed_blocks.py --check` is what CI runs to prove
        the committed snippets still match the canonical source. If
        this passes, the guidance in the snippets is the guidance in
        the source; if it fails, someone hand-edited a generated file.
        """
        result = subprocess.run(
            [sys.executable, str(RENDER_SCRIPT), "--check",
             "--repo-root", str(REPO_ROOT)],
            capture_output=True, text=True, timeout=60,
        )
        self.assertEqual(
            result.returncode, 0,
            f"generated snippets have drifted from the canonical source:\n"
            f"{result.stdout}\n{result.stderr}",
        )


def _snippet_name_for(adapter_name: str) -> str:
    return {
        "mpm-claude-code": "CLAUDE.md.snippet",
        "mpm-opencode": "AGENTS.md.snippet",
        "mpm-pi": "AGENTS.md.snippet",
        "mpm-hermes": "hermes.md.snippet",
        "mpm-memory-openclaw": "SOUL.md.snippet",
    }[adapter_name]


class DocumentedCommandsActuallyExist(unittest.TestCase):
    """Property 2 — the guidance names real commands and real tools.

    This is the property that keeps an instruction fix honest. A block
    that teaches an agent to run a command MPM does not have is worse
    than no guidance: it converts a gap into a confident dead end. So
    the names in item 8 are checked against the two places that define
    them — the CLI router and the tool registry — and, when a build is
    present, against the binary itself.

    Both a static and a live check run. The static one works on a
    clean checkout; the live one proves the binary agrees with the
    source it was built from.
    """

    def test_cli_reference_subcommands_exist_in_the_router(self):
        router = (REPO_ROOT / "cmd" / "mpm" / "router.go").read_text(
            encoding="utf-8",
        )
        self.assertIn(
            '"reference": {Name: "reference"', router,
            "the reference command must be registered in the CLI router",
        )
        self.assertIn(
            '"reference":   true,', router,
            "reference must dispatch subcommands, which is what "
            "`mpm reference search` and `mpm reference show` rely on",
        )

    def test_cli_reference_search_and_show_are_dispatched(self):
        source = (REPO_ROOT / "cmd" / "mpm" / "simple_cmds.go").read_text(
            encoding="utf-8",
        )
        body = _function_body(source, "handleRef")
        self.assertIn('case "search":', body,
                      "handleRef must dispatch a search subcommand")
        self.assertIn("handleRefSearch", body)
        self.assertIn('case "show"', body,
                      "handleRef must dispatch a show subcommand")
        self.assertIn("handleRefShow", body)

    def test_documented_mcp_tool_is_registered(self):
        registry = (REPO_ROOT / "internal" / "core" / "tools" / "registry_list.go").read_text(
            encoding="utf-8",
        )
        # The registry is a package-level slice literal, not a function,
        # so the entry is located by scanning for the tool's own Name
        # field and taking the enclosing element.
        start = registry.find('Name: "mpm_references"')
        self.assertNotEqual(
            start, -1,
            "the tool name named in the block must be a registered MCP "
            "tool; registry_list.go does not declare it",
        )
        entry_end = registry.find("Handler:", start)
        entry = registry[start:entry_end if entry_end != -1 else start + 4000]
        for action in ("add", "read", "search", "list"):
            self.assertIn(
                f'"{action}"', entry,
                f"action {action!r} is documented but the schema does not "
                f"enumerate it, so a client validating against the schema "
                f"would reject it",
            )

    @unittest.skipUnless(_has_mcp_binary(), "mpm binary not built; run `make build`")
    def test_binary_advertises_both_documented_subcommands(self):
        out = _run_mpm(["reference", "--help"])
        self.assertIn("search <query>", out,
                      "the CLI must advertise `reference search` — the block "
                      "names it")
        self.assertIn("show <id>", out,
                      "the CLI must advertise `reference show` — the block "
                      "names it")

    @unittest.skipUnless(_has_mcp_binary(), "mpm binary not built; run `make build`")
    def test_binary_knows_the_documented_mcp_tool(self):
        """`mpm call` names every registered tool when it rejects an
        unknown one, so the registry's own vocabulary confirms the
        block's tool name without needing a corpus.
        """
        with tempfile.TemporaryDirectory(prefix="mpm-refdisc-tool-") as ws:
            proc = subprocess.run(
                [str(MPM_BIN), "call", "mpm_definitely_not_a_tool",
                 "--payload", '{"action":"list"}'],
                capture_output=True, text=True, timeout=60,
                env={**os.environ, "MPM_WORKSPACE": ws},
            )
        payload = json.loads(proc.stdout)
        listed = payload.get("error", "")
        self.assertIn(
            "mpm_references", listed,
            "the built binary must know the tool the block names; its "
            "registry vocabulary is reported in the unknown-tool error",
        )


class ReferenceGuidanceSurvivesInstallation(unittest.TestCase):
    """Properties 3, 4 and 5 — install, refresh, repeat, preserve.

    These drive each host's REAL installer: the same script, the same
    arguments, and the same argv-construction the root installer uses
    (reconcile.json + `~` expansion + `@/` resolution). Generating the
    right text and shipping the wrong text are different failures, and
    only the second one bites an operator.

    Hermes is the exception, and it is an exception by design rather
    than by omission: it has no persistent managed block and its
    reconcile.json declares no installer, so the real path for Hermes
    is its own documented manual script, invoked directly.
    """

    def setUp(self):
        self.tmpdir = Path(tempfile.mkdtemp(prefix="mpm-refdisc-install-"))
        self.home = self.tmpdir / "home"
        self.home.mkdir(parents=True)

    def tearDown(self):
        shutil.rmtree(self.tmpdir, ignore_errors=True)

    # ---------- helpers ----------

    def _install(self, adapter_name: str) -> Path:
        """Invoke an adapter's real installer exactly as the root
        installer does, and return the file it wrote.
        """
        manifest = json.loads(
            (AGENT_INSTALLATION / adapter_name / "reconcile.json").read_text(
                encoding="utf-8",
            )
        )
        installer = AGENT_INSTALLATION / adapter_name / manifest["installer"]
        argv = [sys.executable, str(installer)]
        for raw in manifest["args"]:
            if raw.startswith("@/"):
                argv.append(str(AGENT_INSTALLATION / adapter_name / raw[2:]))
            elif raw.startswith("~"):
                argv.append(str(self.home / raw[2:].lstrip("/"))
                            if raw != "~" else str(self.home))
            else:
                argv.append(raw)

        env = {**os.environ, "HOME": str(self.home)}
        proc = subprocess.run(
            argv, capture_output=True, text=True, env=env, timeout=120,
        )
        self.assertEqual(
            proc.returncode, 0,
            f"{adapter_name}: its real installer must succeed.\n"
            f"argv={argv}\nstdout={proc.stdout}\nstderr={proc.stderr}",
        )
        return Path(argv[argv.index("--target") + 1]) if "--target" in argv \
            else self._installed_path_for(adapter_name)

    def _installed_path_for(self, adapter_name: str) -> Path:
        return {
            "mpm-pi": self.home / ".pi" / "agent" / "AGENTS.md",
            "mpm-claude-code": self.home / ".claude" / "CLAUDE.md",
            "mpm-opencode": self.home / ".config" / "opencode" / "AGENTS.md",
            "mpm-memory-openclaw": self.home / ".openclaw" / "workspace" / "SOUL.md",
            "mpm-hermes": self.home / ".hermes.md",
        }[adapter_name]

    # ---------- property 6: reaches every host ----------

    def test_every_host_installer_delivers_the_guidance(self):
        """Each host's own installer, run with that host's own
        manifest arguments, must land the guidance in the file that
        host actually reads.
        """
        for adapter_name in ("mpm-claude-code", "mpm-opencode", "mpm-pi",
                             "mpm-memory-openclaw"):
            with self.subTest(adapter=adapter_name):
                self._install(adapter_name)
                installed = self._installed_path_for(adapter_name)
                self.assertTrue(
                    installed.is_file(),
                    f"{adapter_name}: installer wrote no file at {installed}",
                )
                text = installed.read_text(encoding="utf-8")
                self.assertIn(
                    GUIDANCE_LABEL, text,
                    f"{adapter_name}: the installed block lacks the "
                    f"reference-search guidance",
                )
                adapter = next(
                    a for a in _render.ADAPTERS if a["name"] == adapter_name
                )
                self.assertIn(
                    f"{adapter['tool_prefix']}mpm_references", text,
                    f"{adapter_name}: installed block does not name the "
                    f"tool as this host spells it",
                )

    def test_hermes_manual_installer_delivers_the_guidance(self):
        """Hermes has no persistent managed block by design — its
        reconcile.json declares no installer and its SKILL.md documents
        a manual flow. The real path for Hermes is therefore
        install_hermes_instructions.py, driven the way SKILL.md drives
        it. It is asserted here rather than skipped, because "no
        managed block" must not become "no guidance".
        """
        installer = (AGENT_INSTALLATION / "mpm-hermes" / "scripts"
                     / "install_hermes_instructions.py")
        snippet = (AGENT_INSTALLATION / "mpm-hermes" / "templates"
                   / "hermes.md.snippet")
        target = self.home / "project" / ".hermes.md"
        target.parent.mkdir(parents=True, exist_ok=True)

        proc = subprocess.run(
            [sys.executable, str(installer),
             "--target", str(target), "--snippet", str(snippet)],
            capture_output=True, text=True,
            env={**os.environ, "HOME": str(self.home)}, timeout=120,
        )
        self.assertEqual(proc.returncode, 0, proc.stderr)
        text = target.read_text(encoding="utf-8")
        self.assertIn(GUIDANCE_LABEL, text)
        self.assertIn("mcp__mpm__mpm_references", text,
                      "hermes is an mcp__mpm__ host; the guidance must name "
                      "the tool under that transport")

    # ---------- property 3: install and refresh retain it ----------

    def test_reinstall_after_an_upgrade_retains_the_guidance(self):
        """The operator's realistic path is not a fresh install: it is
        an existing block written by an older MPM, reconciled forward.
        A stale block predating the guidance must converge on a block
        that carries it, with everything around it untouched.
        """
        for adapter_name in ("mpm-claude-code", "mpm-opencode", "mpm-pi"):
            with self.subTest(adapter=adapter_name):
                self._install(adapter_name)
                installed = self._installed_path_for(adapter_name)
                first = installed.read_text(encoding="utf-8")
                self.assertIn(GUIDANCE_LABEL, first)

                # Simulate an older MPM having written this file: strip
                # the guidance from the managed region while leaving the
                # user's own content intact, then reconcile forward.
                #
                # Anchored to the label that opens the invariant and to
                # the structural END marker rather than to the last line
                # of the prose: item 8 is the final invariant, so the
                # marker bounds it exactly, and a reworded tail must not
                # silently turn this fixture into a no-op.
                stripped = re.sub(
                    r"\n8\. \*\*Search references before concluding external "
                    r"material is\n(?:.*\n)*?"
                    r"(?=<!-- END MPM MANAGED BLOCK -->)",
                    "\n", first,
                )
                self.assertNotIn(
                    GUIDANCE_LABEL, stripped,
                    f"{adapter_name}: stale fixture failed to strip the "
                    f"guidance, so this test would pass without reconciling",
                )
                installed.write_text(stripped, encoding="utf-8")

                self._install(adapter_name)
                refreshed = installed.read_text(encoding="utf-8")
                self.assertIn(
                    GUIDANCE_LABEL, refreshed,
                    f"{adapter_name}: refresh did not restore the guidance",
                )

    # ---------- property 4: idempotency ----------

    def test_repeated_installation_is_idempotent(self):
        for adapter_name in ("mpm-claude-code", "mpm-opencode", "mpm-pi",
                             "mpm-memory-openclaw"):
            with self.subTest(adapter=adapter_name):
                self._install(adapter_name)
                installed = self._installed_path_for(adapter_name)
                before = installed.read_bytes()
                backups_before = sorted(
                    p.name for p in installed.parent.glob(installed.name + ".bak*")
                )

                self._install(adapter_name)

                self.assertEqual(
                    installed.read_bytes(), before,
                    f"{adapter_name}: a second install rewrote the file; "
                    f"the guidance survives but the block is not stable",
                )
                backups_after = sorted(
                    p.name for p in installed.parent.glob(installed.name + ".bak*")
                )
                self.assertEqual(
                    backups_after, backups_before,
                    f"{adapter_name}: a no-op install created a backup, so "
                    f"repeated installs accumulate copies forever",
                )

    # ---------- property 5: user content preserved ----------

    def test_user_content_around_the_managed_block_survives(self):
        """The managed block is delimited by host-specific markers.
        The operator's own text on both sides must survive an install
        that introduces the guidance.

        The assertion is on the operator's TEXT, not on byte-equality
        of the concatenation. Each installer replaces the managed
        region with `block.rstrip()` and re-emits the surrounding file,
        so the blank-line count immediately outside a marker is
        normalised. That normalisation is existing, intended behaviour
        and is not what this test is about; pinning byte-equality
        across the marker boundary would fail on it while saying
        nothing about whether the user's notes survived.
        """
        cases = {
            "mpm-claude-code": (".claude/CLAUDE.md",
                                "<!-- BEGIN MPM-MANAGED SECTION:claude-code-instructions -->",
                                "<!-- END MPM-MANAGED SECTION:claude-code-instructions -->"),
            "mpm-opencode": (".config/opencode/AGENTS.md",
                             "<!-- BEGIN MPM-MANAGED SECTION:opencode-instructions -->",
                             "<!-- END MPM-MANAGED SECTION:opencode-instructions -->"),
            "mpm-pi": (".pi/agent/AGENTS.md",
                       "<!-- BEGIN MPM-MANAGED SECTION:pi-instructions -->",
                       "<!-- END MPM-MANAGED SECTION:pi-instructions -->"),
        }
        for adapter_name, (rel, begin, end) in cases.items():
            with self.subTest(adapter=adapter_name):
                target = self.home / rel
                target.parent.mkdir(parents=True, exist_ok=True)
                above_line = f"# operator notes above the block ({adapter_name})"
                below_line = f"# operator notes below the block ({adapter_name})"
                stale = f"{begin}\nSTALE BODY from an older MPM\n{end}\n"
                target.write_text(
                    above_line + "\n" + stale + "\n" + below_line + "\n",
                    encoding="utf-8",
                )

                self._install(adapter_name)

                text = target.read_text(encoding="utf-8")
                self.assertIn(GUIDANCE_LABEL, text)
                self.assertNotIn("STALE BODY", text,
                                 f"{adapter_name}: the stale body survived")

                head = text[:text.index(begin)]
                tail = text[text.index(end) + len(end):]
                self.assertIn(
                    above_line, head,
                    f"{adapter_name}: operator content above the managed "
                    f"block was lost. head={head!r}",
                )
                self.assertIn(
                    below_line, tail,
                    f"{adapter_name}: operator content below the managed "
                    f"block was lost. tail={tail!r}",
                )
                # The user's lines must still be the outermost content:
                # nothing the installer wrote may be interleaved into
                # either region.
                self.assertTrue(
                    head.strip().endswith(above_line),
                    f"{adapter_name}: content was inserted between the "
                    f"operator's notes and the managed block. head={head!r}",
                )
                self.assertTrue(
                    tail.strip().startswith(below_line),
                    f"{adapter_name}: content was inserted between the "
                    f"managed block and the operator's notes. tail={tail!r}",
                )

    # ---------- property 7: the gated suite reaches it ----------

    def test_this_file_is_collected_by_the_canonical_gate(self):
        """`make test-agent-installation` discovers tests by pattern in
        agent_installation/tests/. A file the pattern does not match is
        an orphaned target: it passes when run by hand and is never run
        by CI, which is how a regression suite quietly stops running.

        Collection is asserted by LOADING the suite rather than running
        it. An earlier version of this test shelled out to
        `unittest discover -p <this file>`, which discovers this test
        too, which runs it again — recursion with no base case. Loading
        answers the same question (does the gate's own invocation find
        these tests?) without executing anything.
        """
        loader = unittest.TestLoader()
        suite = loader.discover(
            start_dir=str(AGENT_INSTALLATION / "tests"),
            pattern="test_reference_discovery_contract.py",
            top_level_dir=str(AGENT_INSTALLATION / "tests"),
        )
        collected = _count_cases(suite)
        self.assertEqual(
            collected["errors"], [],
            f"unittest discovery failed to load this file:\n"
            f"{collected['errors']}",
        )
        self.assertEqual(
            len(collected["ids"]), len(set(collected["ids"])),
            f"duplicate test ids collected, which means the file was "
            f"loaded twice: {collected['ids']}",
        )
        # Every test defined in this module must be reachable through the
        # gate's discovery pattern. A test that exists here but is not
        # collected is precisely the orphaned-target failure. Collected ids
        # carry a module prefix; compare on Class.method (see
        # _defined_test_ids) so the result is independent of it.
        collected_pairs = {".".join(i.split(".")[-2:]) for i in collected["ids"]}
        missing = _defined_test_ids() - collected_pairs
        self.assertEqual(
            sorted(missing), [],
            "these tests are defined but the canonical gate's discovery "
            "pattern does not collect them",
        )
        self.assertGreater(
            len(collected["ids"]), 20,
            f"only {len(collected['ids'])} tests collected; the seven "
            f"required properties are not being covered",
        )


class WordPressScenarioIsRetrievable(unittest.TestCase):
    """The original scenario, end to end, on an isolated corpus.

    The user asked specifically about WordPress. So the scenario is
    reproduced: a WordPress-shaped document goes in, the query that was
    actually asked goes in, and the documented two-step workflow
    (search, then read the document) has to return the function.

    Isolation is not incidental here. Search is a retrieval operation
    with an audit trail behind it, and the live corpus is the user's
    data with 33 real WordPress documents in it. This suite runs
    against a scratch workspace, so it proves the workflow without
    writing an interaction row against anyone's real corpus. What is
    NOT proved here, and is reported separately, is that an autonomous
    agent now performs this lookup — that is a behavioural claim and
    needs a behavioural test.
    """

    @unittest.skipUnless(_has_mcp_binary(), "mpm binary not built; run `make build`")
    def test_documented_workflow_finds_the_documented_function(self):
        workspace = Path(tempfile.mkdtemp(prefix="mpm-refdisc-wp-"))
        try:
            doc = workspace / "wordpress-rest-api.md"
            doc.write_text(
                "# WordPress REST API Reference\n"
                "\n"
                "## Nav Menu\n"
                "\n"
                "### wp_update_nav_menu_item\n"
                "\n"
                "Updates a single nav menu item. Requires an assigned\n"
                "menu, menu_item_id, and optionally menu_id.\n"
                "\n"
                "Returns the updated menu item object on success.\n",
                encoding="utf-8",
            )
            env = {**os.environ, "MPM_WORKSPACE": str(workspace)}

            added = subprocess.run(
                [str(MPM_BIN), "reference", "add", str(doc), "--tag", "wordpress"],
                capture_output=True, text=True, env=env, timeout=180,
            )
            self.assertEqual(added.returncode, 0,
                             f"ingest failed:\n{added.stdout}\n{added.stderr}")

            searched = subprocess.run(
                [str(MPM_BIN), "reference", "search", "wp_update_nav_menu_item"],
                capture_output=True, text=True, env=env, timeout=180,
            )
            self.assertEqual(searched.returncode, 0, searched.stderr)
            self.assertIn(
                "wp_update_nav_menu_item", searched.stdout,
                "`mpm reference search` — the command the block tells the "
                "agent to run — did not find the function. The incident "
                "reproduces.",
            )

            # Follow the pointer the search output itself prints, which
            # is the step the guidance insists on.
            match = re.search(r"mpm reference show ([0-9a-f]+)", searched.stdout)
            self.assertIsNotNone(
                match,
                f"search did not emit a show pointer, so an agent cannot "
                f"complete the documented workflow:\n{searched.stdout}",
            )
            shown = subprocess.run(
                [str(MPM_BIN), "reference", "show", match.group(1)],
                capture_output=True, text=True, env=env, timeout=180,
            )
            self.assertEqual(shown.returncode, 0, shown.stderr)
            self.assertIn(
                "wp_update_nav_menu_item", shown.stdout,
                "reading the document — the step the block requires and the "
                "one the incident skipped — did not return the function "
                "body",
            )
        finally:
            shutil.rmtree(workspace, ignore_errors=True)

    @unittest.skipUnless(_has_mcp_binary(), "mpm binary not built; run `make build`")
    def test_the_live_corpus_is_readable_without_being_written(self):
        """Read-only confirmation that the document the agent claimed
        was absent is present on this machine.

        Opened with mode=ro, so SQLite itself refuses a write rather
        than trusting this test to behave. If the corpus is not where
        the default layout says, the test says so and stops — it does
        not fall back to creating one, which would turn a finding about
        the operator's data into a fixture.
        """
        candidates = [
            Path(os.environ.get("MPM_WORKSPACE", "")) / "src" / "db" / "mpm.db",
            Path.home() / ".mpm" / "src" / "db" / "mpm.db",
        ]
        db = next((c for c in candidates if c.is_file() and c.stat().st_size > 0), None)
        if db is None:
            self.skipTest(f"no populated MPM database among {candidates}")

        uri = f"file:{db}?mode=ro"
        conn = sqlite3.connect(uri, uri=True)
        try:
            hits = conn.execute(
                "SELECT doc_id, chunk_index FROM reference_chunks "
                "WHERE content LIKE ? LIMIT 5",
                ("%wp_update_nav_menu_item%",),
            ).fetchall()
        finally:
            conn.close()

        self.assertTrue(
            hits,
            f"no reference chunk in {db} mentions wp_update_nav_menu_item. "
            f"The original incident assumed this document was present; if "
            f"it is not, the root cause is different and needs revisiting.",
        )


def _function_body(source: str, func_name: str) -> str:
    """Extract a top-level Go function body by brace counting.

    Deliberately not a regex over the whole file: these names also
    appear in comments and in unrelated call sites, and a test that
    greps for a string will happily pass on a comment.
    """
    start = re.search(rf"^func {re.escape(func_name)}\(", source, re.M)
    if not start:
        return ""
    brace = source.index("{", start.end())
    depth = 0
    for i in range(brace, len(source)):
        if source[i] == "{":
            depth += 1
        elif source[i] == "}":
            depth -= 1
            if depth == 0:
                return source[brace:i]
    return ""


def _run_mpm(args: list[str]) -> str:
    with tempfile.TemporaryDirectory(prefix="mpm-refdisc-help-") as ws:
        proc = subprocess.run(
            [str(MPM_BIN), *args],
            capture_output=True, text=True, timeout=60,
            env={**os.environ, "MPM_WORKSPACE": ws},
        )
    return proc.stdout


def _count_cases(suite) -> dict:
    """Flatten a loaded suite into ids plus any load errors.

    `_FailedTest` instances carry the import error as the test itself,
    so they are separated out rather than counted as passing cases —
    otherwise a module that fails to import looks like a suite that
    collected tests.
    """
    ids: list[str] = []
    errors: list[str] = []

    def walk(node):
        for item in node:
            if isinstance(item, unittest.TestSuite):
                walk(item)
                continue
            tid = item.id()
            if "unittest.loader._FailedTest" in tid or type(item).__name__ == "_FailedTest":
                errors.append(str(item))
                continue
            ids.append(tid)

    walk(suite)
    return {"ids": ids, "errors": errors}


def _defined_test_ids() -> set[str]:
    """Every test method defined in this module, as `Class.method` pairs.

    Walks the TestCase subclasses in this module's namespace so the
    comparison against what discovery collected is like-for-like: a
    method on a class is only reachable through discovery if discovery
    finds the class too, which is the half that actually breaks.

    Deliberately does NOT include the module name. This module is
    reachable as `test_reference_discovery_contract` under the
    canonical `python3 -m unittest discover tests` and as
    `tests.test_reference_discovery_contract` when invoked by dotted
    path, and discovery's collected ids carry whichever prefix the
    importing context produced. Comparing on `Class.method` makes this
    assertion mean the same thing under both invocations, instead of
    reporting a false "not collected" purely because of the prefix.
    """
    module = sys.modules[__name__]
    found = set()
    for name, obj in vars(module).items():
        if not (isinstance(obj, type) and issubclass(obj, unittest.TestCase)):
            continue
        for attr in dir(obj):
            if attr.startswith("test_"):
                found.add(f"{name}.{attr}")
    return found


if __name__ == "__main__":
    unittest.main(verbosity=2)