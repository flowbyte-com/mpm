"""
test_managed_block_behaviour_matrix.py — The compact behaviour matrix's
contract, and the managed block's delivery to OpenCode.

Contract 4.0.0 (2026-10-28) replaced the managed block's ten prose
responsibilities with a **behaviour matrix**: a two-column table whose
left column names the SITUATION and whose right column names the MPM
destination that situation belongs in.

This suite pins:

  1. DELIVERY — the block reaches OpenCode's AGENTS.md through the
     adapter's own installer, and the installed file carries it.
  2. PARITY — the installed block is byte-identical to the canonical
     render. The canonical source is the only source of truth; the
     installed file is generated.
  3. USER CONTENT — everything outside the managed region survives
     reconciliation byte-for-byte.
  4. IDEMPOTENCY — installing twice does not duplicate the block.
  5. MEMORY vs HANDOFF — the block distinguishes a shared knowledge
     write from an execution-state handoff.
  6. PENDING vs TRUSTED — the block distinguishes an unverified
     hypothesis from an established fact.
  7. NO SESSION-END SIGNAL — the block does not require any host
     session-end signal, because OpenCode v2.1.4 exposes none
     (see docs/dev/session-c-session-end-continuity.md).

Every installer test here runs against a DISPOSABLE HOME. The live
installation is never read for mutation and never written.

The suite is deliberately behavioural rather than textual wherever a
behaviour can be checked: the delivery tests exercise the real
installer, and the matrix destinations are checked against the live
tool registry rather than against a hand-written list.
"""

from __future__ import annotations

import importlib.util
import os
import re
import shutil
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path


REPO_ROOT = Path(__file__).resolve().parent.parent.parent
AGENT_INSTALLATION = REPO_ROOT / "agent_installation"
RENDER_SH = AGENT_INSTALLATION / "scripts" / "render_managed_blocks.py"
CANONICAL_SOURCE = AGENT_INSTALLATION / "MPM_AGENT_INTEGRATION_SNIPPETS.md"
PROTOCOL = AGENT_INSTALLATION / "mpm-agent-protocol.md"
INSTALLER = (
    AGENT_INSTALLATION / "mpm-opencode" / "scripts"
    / "install_agents_instructions.py"
)
SNIPPET = AGENT_INSTALLATION / "mpm-opencode" / "templates" / "AGENTS.md.snippet"

# The live tool registry. The matrix's destinations are checked against
# it, so a renamed action or a retired tool fails here rather than in
# an agent's session.
REGISTRY = REPO_ROOT / "internal" / "core" / "tools" / "registry_list.go"

OUTER_BEGIN = "<!-- BEGIN MPM-MANAGED SECTION:opencode-instructions -->"
OUTER_END = "<!-- END MPM-MANAGED SECTION:opencode-instructions -->"


def _load_render_module():
    spec = importlib.util.spec_from_file_location(
        "_render_under_test", RENDER_SH,
    )
    render = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(render)
    return render


_render = _load_render_module()


def _canonical_block() -> str:
    return _render.extract_canonical_block(
        CANONICAL_SOURCE.read_text(encoding="utf-8"),
    )


def _flat(text: str) -> str:
    return re.sub(r"\s+", " ", text)


def _matrix_rows(block: str) -> list[tuple[str, str]]:
    rows: list[tuple[str, str]] = []
    for line in block.splitlines():
        stripped = line.strip()
        if not stripped.startswith("|"):
            continue
        if stripped.startswith("|---") or stripped.startswith("| Situation"):
            continue
        cells = [c.strip() for c in stripped.strip("|").split("|")]
        if len(cells) != 2:
            continue
        rows.append((cells[0], cells[1]))
    return rows


# ---------------------------------------------------------------------------
# 1–4: delivery, parity, user content, idempotency
# ---------------------------------------------------------------------------


class ManagedBlockDelivery(unittest.TestCase):
    """The managed block reaches OpenCode's AGENTS.md, and only the
    managed region is ever rewritten."""

    def setUp(self):
        self.tmpdir = Path(tempfile.mkdtemp(prefix="mpm-block-delivery-"))
        self.home = self.tmpdir / "home"
        self.home.mkdir(parents=True, exist_ok=True)
        self.target = self.home / ".config" / "opencode" / "AGENTS.md"
        self.env = os.environ.copy()
        self.env["HOME"] = str(self.home)

    def tearDown(self):
        shutil.rmtree(self.tmpdir, ignore_errors=True)

    def _install(self) -> subprocess.CompletedProcess:
        return subprocess.run(
            [
                sys.executable, str(INSTALLER),
                "--scope", "user",
                "--target", str(self.target),
                "--snippet", str(SNIPPET),
            ],
            capture_output=True, text=True, env=self.env, timeout=120,
        )

    # ---------- 1. delivery ----------

    def test_the_block_is_delivered_to_opencode(self):
        """A fresh install writes a file that carries the block."""
        proc = self._install()
        self.assertEqual(
            proc.returncode, 0,
            f"installer failed: {proc.stdout}\n{proc.stderr}",
        )
        self.assertTrue(self.target.is_file(), "the installer wrote no file")
        installed = self.target.read_text(encoding="utf-8")
        block = _canonical_block()
        for phrase in (
            "Reusable knowledge belongs in shared MPM records",
            "| Situation | Record |",
            "mpm_memory",
            "mpm_handoff",
            "No signal reliably marks a session end",
        ):
            self.assertIn(
                phrase, installed,
                f"the installed AGENTS.md must carry {phrase!r}",
            )
        # The inner managed block must be the canonical one, modulo
        # the install-time provenance comment.
        self.assertIn(
            block.strip().splitlines()[-1],
            installed,
            "the installed block must end with the canonical END marker",
        )

    def test_the_installed_block_matches_the_canonical_source(self):
        """The installed managed region is byte-identical to the
        canonical render (the provenance comment aside).

        This is the anti-drift property: the canonical source is the
        only place the text is authored, and the installed file is
        generated from it.
        """
        self._install()
        installed = self.target.read_text(encoding="utf-8")
        body = installed[installed.index(OUTER_BEGIN):]
        body = body[:body.index(OUTER_END) + len(OUTER_END)]
        # The provenance comment is per-install metadata, layered on
        # top of the canonical text by design.
        body = re.sub(
            r"<!-- mpm-provenance:[^\n]*-->\n", "", body,
        )
        expected = f"{OUTER_BEGIN}\n{SNIPPET.read_text(encoding='utf-8').rstrip()}\n{OUTER_END}"
        self.assertEqual(
            body, expected,
            "the installed managed region must be byte-identical to the "
            "canonical render",
        )

    # ---------- 3. user content survives ----------

    def test_user_authored_content_survives_reconciliation(self):
        """Content OUTSIDE the managed region is never rewritten."""
        self.target.parent.mkdir(parents=True, exist_ok=True)
        above = "# operator notes above\nkeep me exactly.\n"
        below = "\n# operator notes below\nkeep me too.\n"
        self.target.write_text(above + below, encoding="utf-8")
        self._install()
        installed = self.target.read_text(encoding="utf-8")
        self.assertTrue(installed.startswith(above), "content above was lost")
        self.assertIn(below, installed, "content below was lost")
        self.assertIn("keep me exactly.", installed)
        self.assertIn("keep me too.", installed)

    def test_user_authored_content_survives_an_upgrade(self):
        """A pre-existing managed block from an OLDER contract is
        replaced; the user's own text around it is not."""
        self.target.parent.mkdir(parents=True, exist_ok=True)
        above = "# operator notes above\nkeep me exactly.\n"
        below = "\n# operator notes below\nkeep me too.\n"
        stale = (
            f"{OUTER_BEGIN}\n"
            "## STALE managed block from an older contract\n"
            "- replaced by the installer\n"
            f"{OUTER_END}\n"
        )
        self.target.write_text(above + stale + below, encoding="utf-8")
        proc = self._install()
        self.assertEqual(proc.returncode, 0, proc.stderr)
        installed = self.target.read_text(encoding="utf-8")
        self.assertIn(above, installed)
        self.assertIn(below, installed)
        self.assertNotIn("STALE managed block", installed)
        self.assertIn("Reusable knowledge belongs in shared MPM records", installed)

    # ---------- 4. idempotency ----------

    def test_repeated_installation_does_not_duplicate(self):
        """Installing twice leaves exactly one managed region."""
        self._install()
        first = self.target.read_text(encoding="utf-8")
        proc = self._install()
        self.assertEqual(proc.returncode, 0, proc.stderr)
        second = self.target.read_text(encoding="utf-8")
        self.assertEqual(
            first, second,
            "a repeated install must be a byte-level no-op",
        )
        self.assertEqual(
            second.count(OUTER_BEGIN), 1,
            "the managed region must appear exactly once",
        )
        self.assertEqual(
            second.count("<!-- BEGIN MPM MANAGED BLOCK -->"), 1,
            "the inner managed block must appear exactly once",
        )
        self.assertEqual(
            second.count("Reusable knowledge belongs in shared MPM records"), 1,
            "the block's body must appear exactly once",
        )


# ---------------------------------------------------------------------------
# 5–7: the matrix's behavioural distinctions
# ---------------------------------------------------------------------------


class BehaviourMatrixContract(unittest.TestCase):
    """The matrix's distinctions, as the block states them."""

    def setUp(self):
        self.block = _canonical_block()
        self.flat = _flat(self.block)

    # ---------- 2 (matrix parity): the destinations are real ----------

    def test_matrix_destinations_are_live_tool_actions(self):
        """Every tool and action the matrix names must exist in the
        live registry.

        A matrix that names a retired tool or a renamed action is worse
        than no matrix: it sends an agent to a call that does not
        exist. The registry is the authority.
        """
        registry = REGISTRY.read_text(encoding="utf-8")
        for situation, destination in _matrix_rows(self.block):
            tools = re.findall(r"`(mpm_[a-z]\w*)`", destination)
            if not tools:
                continue  # the "no write" row
            for tool in tools:
                self.assertIn(
                    f'Name: "{tool}"', registry,
                    f"matrix row {situation!r} names {tool!r}, which is not "
                    f"in the live tool registry",
                )
                # The action must be in the tool's registered enum.
                m = re.search(
                    rf'Name: "{tool}".*?"action":\s*\{{"type":\s*"string",\s*"enum":\s*\[(.*?)\]',
                    registry,
                    re.DOTALL,
                )
                self.assertIsNotNone(
                    m, f"could not read {tool}'s action enum from the registry",
                )
                actions = set(re.findall(r'"([a-z_]+)"', m.group(1)))
                named = re.findall(r"`([a-z_]+)`", destination)
                for action in named:
                    if action == tool:
                        continue
                    self.assertIn(
                        action, actions,
                        f"matrix row {situation!r} names {tool} action "
                        f"{action!r}, which the registry does not offer "
                        f"(it offers {sorted(actions)})",
                    )

    def test_matrix_teaches_every_write_destination(self):
        """The matrix must teach a destination for each artifact kind
        the substrate keeps, and a "no write" row for the rest.

        Each destination must appear EXACTLY ONCE. That is what makes
        the matrix checkable: if the fact row were pointed at
        `mpm_lessons`, `mpm_lessons` would appear twice and
        `mpm_memory` not at all, and this test would say so. Without
        the exactly-once rule, a mis-pointed row is invisible — every
        tool/action pair it names is a real one.
        """
        rows = _matrix_rows(self.block)
        destinations = [d for _, d in rows]
        for tool, action in (
            ("mpm_memory", "save"),
            ("mpm_decisions", "record"),
            ("mpm_lessons", "save"),
            ("mpm_evidence", "add"),
            ("mpm_theories", "propose"),
            ("mpm_work", "create"),
            ("mpm_skills", "save"),
            ("mpm_references", "add"),
            ("mpm_handoff", "write"),
        ):
            matches = [
                d for d in destinations
                if f"`{tool}`" in d and f"`{action}`" in d
            ]
            self.assertEqual(
                len(matches), 1,
                f"the matrix must teach {tool} action {action} exactly "
                f"once; it teaches it {len(matches)} times ({matches}). A "
                f"destination that appears twice means a row is pointed "
                f"at the wrong artifact kind.",
            )
        self.assertTrue(
            any(d.strip().lower() == "nothing" for d in destinations),
            "the matrix must carry a 'no write' row",
        )
        # Every tool-bearing row names a tool from the required set —
        # no destination outside the contract's vocabulary.
        allowed = {t for t, _ in (
            ("mpm_memory", "save"),
            ("mpm_decisions", "record"),
            ("mpm_lessons", "save"),
            ("mpm_evidence", "add"),
            ("mpm_theories", "propose"),
            ("mpm_work", "create"),
            ("mpm_skills", "save"),
            ("mpm_references", "add"),
            ("mpm_handoff", "write"),
        )}
        for situation, destination in rows:
            for tool in re.findall(r"`(mpm_[a-z]\w*)`", destination):
                self.assertIn(
                    tool, allowed,
                    f"matrix row {situation!r} names {tool!r}, which is "
                    f"outside the contract's destination vocabulary",
                )

    # ---------- 5. memory vs handoff ----------

    def test_memory_writes_are_distinguished_from_handoffs(self):
        """A shared knowledge write and an execution-state handoff are
        different acts, and the block must say so.

        The confusion this prevents is the architectural one: a handoff
        that becomes a private knowledge silo, so a discovery is only
        findable by the session that made it.
        """
        self.assertIn(
            "Reusable knowledge belongs in shared MPM records", self.flat,
            "the block must state that reusable knowledge lives in the "
            "shared records",
        )
        self.assertIn(
            "A handoff carries execution state, not knowledge", self.flat,
            "the block must state that a handoff carries execution state "
            "rather than knowledge",
        )
        self.assertIn(
            "reference records, don't copy them", self.flat,
            "the block must require the handoff to REFERENCE shared "
            "records rather than duplicate them",
        )
        # And the two destinations must be different rows.
        rows = {d for _, d in _matrix_rows(self.block)}
        memory_row = next(d for d in rows if "`mpm_memory`" in d)
        handoff_row = next(d for d in rows if "`mpm_handoff`" in d)
        self.assertNotEqual(
            memory_row, handoff_row,
            "the matrix must give memory and handoff different rows",
        )

    # ---------- 6. pending vs trusted ----------

    def test_pending_knowledge_is_distinguished_from_trusted(self):
        """An unverified hypothesis must not be presented as an
        established fact or a trusted lesson."""
        self.assertIn(
            "Unverified hypothesis", self.flat,
            "the matrix must have a row for an unverified hypothesis",
        )
        self.assertIn(
            "never a fact or lesson", self.flat,
            "the block must say an untested hypothesis is not a fact or "
            "a lesson",
        )
        rows = {s for s, _ in _matrix_rows(self.block)}
        self.assertTrue(
            any("hypothesis" in s.lower() for s in rows),
            "the matrix must name the hypothesis situation",
        )
        # The protocol must carry the commitment in full.
        protocol = _flat(PROTOCOL.read_text(encoding="utf-8"))
        self.assertIn(
            "Never silently promote speculation into certainty", protocol,
            "the protocol must forbid promoting speculation into certainty",
        )

    # ---------- 7. no unreliable session-end signal ----------

    def test_no_unreliable_session_end_signal_is_required(self):
        """The block must not make a handoff depend on a host signal.

        OpenCode v2.1.4 exposes NO session-terminal event:
        `session.execution.succeeded` is a turn boundary, there is no
        `session.idle` event at all, `location.shutdown` and the plugin
        cleanup callback are host-process boundaries with no session id,
        and a SIGKILL publishes nothing. Measured — see
        docs/dev/session-c-session-end-continuity.md.

        So the block must trigger a handoff on AGENT-JUDGED grounds
        (transfer, substantial task finished, preparing to stop) and
        must explicitly refuse to treat a turn, an idle, an archive or a
        shutdown as a session end.
        """
        self.assertIn(
            "No signal reliably marks a session end", self.flat,
            "the block must state that no host signal reliably marks a "
            "session end",
        )
        # The four signals that are NOT session ends, named so the
        # agent cannot mistake one for the other.
        for signal in ("turn completion", "idle", "archive", "shutdown"):
            self.assertIn(
                signal, self.flat,
                f"the block must name {signal!r} as something that is "
                f"not a session end",
            )
        # And the handoff trigger must be agent-judged, not signal-driven.
        self.assertIn(
            "Write one for a transfer, a finished substantial task, or "
            "before you stop", self.flat,
            "the block must trigger a handoff on agent-judged grounds",
        )
        # The block must NOT instruct the agent to wait for an end signal.
        for forbidden in (
            "when the session ends",
            "when your session ends",
            "once the session ends",
        ):
            self.assertNotIn(
                forbidden, self.flat,
                f"the block must not make a handoff depend on a session-end "
                f"signal ({forbidden!r})",
            )

    def test_the_block_names_the_real_handoff_fields(self):
        """The handoff write surface persists exactly five fields:
        `summary`, `session_id`, `state`, `commitments` and
        `open_questions`. Naming the fields is what stops an agent
        writing `next_steps` and having it silently dropped."""
        registry = REGISTRY.read_text(encoding="utf-8")
        m = re.search(
            r'Name: "mpm_handoff".*?"required": \["summary"\]',
            registry, re.DOTALL,
        )
        self.assertIsNotNone(m, "the mpm_handoff schema must require summary")
        self.assertIn(
            '"commitments"', registry,
            "the mpm_handoff schema must declare commitments",
        )
        self.assertIn(
            '"open_questions"', registry,
            "the mpm_handoff schema must declare open_questions",
        )
        for field in ("summary", "commitments", "open_questions"):
            self.assertIn(
                f"`{field}`", self.flat,
                f"the block must name the handoff field {field!r}",
            )


if __name__ == "__main__":
    unittest.main()
