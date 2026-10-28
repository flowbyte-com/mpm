"""
test_cross_agent_discoverability.py — The substrate properties the
compact managed block's contract depends on.

The managed block tells an agent two things that are only true if the
substrate makes them true:

  1. **Reusable knowledge belongs in shared records** — any agent can
     retrieve a discovery without knowing which session made it, and
     without reading that session's handoff.
  2. **A handoff carries execution state** — it preserves what a
     successor needs to continue the work.

This suite proves both against the REAL `mpm` binary and a REAL
SQLite store, in a disposable workspace. It is not a text test: the
question is whether the substrate behaves that way, not whether the
instructions say so.

The four scenarios are the ones the contract names:

  A. Shared knowledge      — a fact written in one session is found in
                             another, with no handoff involved.
  B. Operational continuity — a handoff written by one session carries
                             its execution state to another.
  C. Negative control      — a read-only exchange writes nothing.
  D. Uncertainty           — an unverified hypothesis is stored as a
                             PENDING theory, not as a trusted memory.

Every call runs against a disposable workspace and a disposable
database. The live installation and the live database are never
touched.
"""

from __future__ import annotations

import json
import os
import shutil
import subprocess
import tempfile
import unittest
from pathlib import Path


REPO_ROOT = Path(__file__).resolve().parent.parent.parent
MPM_BIN = REPO_ROOT / ".build" / "bin" / "mpm"

# The marker that makes a token unmistakable in a full-text search and
# impossible to confuse with ordinary prose.
TOKEN = "ZEBRAFISH"


def _has_mpm_binary() -> bool:
    return MPM_BIN.is_file() and os.access(MPM_BIN, os.X_OK)


class CrossAgentDiscoverability(unittest.TestCase):
    """The substrate properties the managed block's contract assumes."""

    def setUp(self):
        if not _has_mpm_binary():
            self.skipTest(
                f"{MPM_BIN} is not built; run `make build` first",
            )
        self.tmpdir = Path(tempfile.mkdtemp(prefix="mpm-discoverability-"))
        self.workspace = self.tmpdir / "workspace"
        (self.workspace / "src" / "db").mkdir(parents=True, exist_ok=True)
        self.db = self.workspace / "src" / "db" / "mpm.db"

    def tearDown(self):
        shutil.rmtree(self.tmpdir, ignore_errors=True)

    # ---------- the substrate driver ----------

    def _call(self, tool: str, payload: dict, session_id: str = "mpm-session") -> dict:
        """Invoke one MPM tool through the public CLI boundary."""
        env = {
            **os.environ,
            "MPM_WORKSPACE": str(self.workspace),
            "MPM_DB_PATH": str(self.db),
            "MPM_SESSION_ID": session_id,
        }
        proc = subprocess.run(
            [
                str(MPM_BIN), "call", tool,
                "--payload", json.dumps(payload),
            ],
            capture_output=True, text=True, env=env, timeout=120,
        )
        # The CLI prints a JSON envelope on stdout; a leading status
        # line may precede it.
        out = proc.stdout.strip()
        start = out.find("{")
        self.assertNotEqual(
            start, -1,
            f"{tool} produced no JSON envelope.\n"
            f"stdout={proc.stdout!r}\nstderr={proc.stderr!r}",
        )
        parsed = json.loads(out[start:])
        self.assertNotIn(
            "error", parsed,
            f"{tool} returned an error: {parsed}",
        )
        return parsed

    # ---------- scenario A: shared knowledge ----------

    def test_scenario_a_shared_knowledge_is_found_without_the_handoff(self):
        """A fact written in one session is retrievable in another,
        without knowing the writing session and without reading any
        handoff.

        This is the property that makes shared MPM records shared. If it
        did not hold, the managed block's central instruction — "reusable
        knowledge belongs in shared records" — would be an instruction
        the substrate cannot honour, and a discovery would be reachable
        only by the session that made it.
        """
        fact = (
            f"{TOKEN}-CONSTRAINT: the render script must end the block "
            f"with a newline after the END marker"
        )
        written = self._call(
            "mpm_memory",
            {"action": "save", "params": {"fact": fact, "tags": ["probe"]}},
            session_id="mpm-SESSION-A",
        )
        self.assertTrue(written.get("success"), written)
        memory_id = written.get("id")
        self.assertTrue(memory_id, f"the save must return an id: {written}")

        # No handoff was written at all.
        handoffs = self._call(
            "mpm_handoff", {"action": "list", "params": {"limit": 10}},
            session_id="mpm-SESSION-B",
        )
        self.assertEqual(
            handoffs.get("count", 0), 0,
            "the scenario must prove discoverability WITHOUT a handoff; "
            f"a handoff exists: {handoffs}",
        )

        # A DIFFERENT session, which has never heard of session A,
        # retrieves the fact by content.
        found = self._call(
            "mpm_memory",
            {"action": "query", "params": {"query": TOKEN, "limit": 5}},
            session_id="mpm-SESSION-B",
        )
        self.assertEqual(
            found.get("count"), 1,
            f"the fact must be retrievable from another session: {found}",
        )
        row = found["memories"][0]
        self.assertEqual(row["id"], memory_id)
        self.assertIn(TOKEN, row.get("summary", ""))

        # And it is retrievable by id, which is what the wake payload's
        # pointers depend on.
        shown = self._call(
            "mpm_memory",
            {"action": "show", "params": {"id": memory_id}},
            session_id="mpm-SESSION-B",
        )
        self.assertTrue(shown.get("success"), shown)
        self.assertIn(TOKEN, json.dumps(shown))

    # ---------- scenario B: operational continuity ----------

    def test_scenario_b_a_handoff_carries_the_execution_state(self):
        """A handoff written by one session carries its execution state
        to another: what was done, what is unfinished, what is blocked,
        and the next action.

        The five fields the substrate persists are the contract: an
        agent that writes `next_steps` — a field that does not exist —
        has it silently dropped, and the successor receives nothing.
        """
        reference_id = self._call(
            "mpm_memory",
            {
                "action": "save",
                "params": {
                    "fact": f"{TOKEN}-REFERENCE: the shared record a "
                            f"handoff points at",
                    "tags": ["probe"],
                },
            },
            session_id="mpm-SESSION-A",
        )["id"]

        summary = (
            f"{TOKEN}-HANDOFF: implemented the behaviour matrix; "
            f"shared record {reference_id} holds the discovery"
        )
        written = self._call(
            "mpm_handoff",
            {
                "action": "write",
                "params": {
                    "summary": summary,
                    "state": "clean",
                    "commitments": [
                        "finish the behavioural comparison",
                        "branch: feat/compact-block",
                    ],
                    "open_questions": [
                        "should the skills row stay in the matrix?",
                    ],
                },
            },
            session_id="mpm-SESSION-A",
        )
        self.assertTrue(written.get("success"), written)
        handoff_id = written["handoff_id"]

        # A DIFFERENT session reads the handoff back.
        read = self._call(
            "mpm_handoff",
            {"action": "read", "params": {"handoff_id": handoff_id}},
            session_id="mpm-SESSION-B",
        )
        handoff = read.get("handoff") or {}
        self.assertEqual(handoff.get("summary"), summary)
        self.assertEqual(
            handoff.get("ended_state"), "clean",
            "the handoff must record how the session ended",
        )
        self.assertIn(
            "finish the behavioural comparison", handoff.get("commitments", []),
            "unfinished work must survive into the successor's view",
        )
        self.assertTrue(
            any("feat/compact-block" in c for c in handoff.get("commitments", [])),
            "the branch must survive into the successor's view",
        )
        self.assertTrue(
            any("should the skills row" in q for q in handoff.get("open_questions", [])),
            "blockers must survive into the successor's view",
        )
        # The handoff REFERENCES the shared record; it does not carry a
        # second copy of it.
        self.assertIn(
            reference_id, handoff.get("summary", ""),
            "the handoff must reference the shared record it relied on",
        )

        # And the referenced record is still independently retrievable —
        # the handoff did not become the only way to find it.
        found = self._call(
            "mpm_memory",
            {"action": "query", "params": {"query": TOKEN, "limit": 5}},
            session_id="mpm-SESSION-B",
        )
        self.assertEqual(
            found.get("count"), 1,
            "the referenced shared record must remain independently "
            f"retrievable: {found}",
        )

    def test_scenario_b_a_field_the_schema_does_not_have_is_dropped(self):
        """The handoff write surface persists exactly five fields.

        This test records the trap the managed block's field naming
        exists to prevent: a payload key that is not in the schema is
        accepted and SILENTLY DROPPED. An agent told "put the next
        action in the handoff" without being told WHICH field will
        guess `next_steps`, and the successor will receive a handoff
        with no next action in it.
        """
        written = self._call(
            "mpm_handoff",
            {
                "action": "write",
                "params": {
                    "summary": f"{TOKEN}-DROP: probing a field that does not exist",
                    "next_steps": "this field does not exist and is dropped",
                },
            },
            session_id="mpm-SESSION-A",
        )
        handoff = written.get("handoff") or {}
        self.assertNotIn(
            "next_steps", handoff,
            "if the substrate ever grew a next_steps field, this test "
            "would need to change — and the block's field naming would "
            "need to change with it",
        )
        self.assertEqual(
            handoff.get("commitments", []), [],
            "an unknown field must not leak into another field",
        )
        self._call(
            "mpm_handoff",
            {"action": "shred", "params": {"id": written["handoff_id"]}},
        )

    # ---------- scenario C: the negative control ----------

    def test_scenario_c_a_read_only_exchange_writes_nothing(self):
        """Reading and querying must not create records.

        The compact block's "no write" row ("Routine or transient
        information | nothing") is an instruction to the AGENT. This
        test is the substrate-side half of the negative control: even
        when the agent does nothing, the substrate must not write on
        its own. An auto-writing substrate would make the instruction
        pointless and would grow the store on every turn.
        """
        import sqlite3

        def counts() -> dict:
            if not self.db.exists():
                return {"memories": 0, "lessons": 0, "session_handoffs": 0, "works": 0}
            conn = sqlite3.connect(f"file:{self.db}?mode=ro", uri=True)
            try:
                out = {}
                for table in ("memories", "lessons", "session_handoffs", "works"):
                    try:
                        out[table] = conn.execute(
                            f"SELECT COUNT(*) FROM {table}",
                        ).fetchone()[0]
                    except sqlite3.Error:
                        out[table] = None
                return out
            finally:
                conn.close()

        # Prime the store so the counts are non-trivial and the
        # database file exists. The prime is a WRITE, and it happens
        # BEFORE the snapshot.
        self._call(
            "mpm_memory",
            {"action": "save", "params": {"fact": f"{TOKEN}-PRIME", "tags": ["probe"]}},
            session_id="mpm-SESSION-C",
        )
        before = counts()
        self.assertGreater(
            before.get("memories") or 0, 0,
            "the prime write must have created a record, so the negative "
            "control has something to protect",
        )

        # A pure read: query, show, list, wake context.
        self._call(
            "mpm_memory",
            {"action": "query", "params": {"query": TOKEN, "limit": 3}},
            session_id="mpm-SESSION-C",
        )
        self._call(
            "mpm_context",
            {"action": "read_wake_context", "params": {}},
            session_id="mpm-SESSION-C",
        )
        self._call(
            "mpm_handoff",
            {"action": "list", "params": {"limit": 5}},
            session_id="mpm-SESSION-C",
        )
        after = counts()
        self.assertEqual(
            before, after,
            "a read-only exchange must not change any durable record "
            "count — the substrate must not write on the agent's behalf",
        )

    # ---------- scenario D: uncertainty ----------

    def test_scenario_d_an_unverified_hypothesis_is_pending_not_trusted(self):
        """A hypothesis with no test is stored as a PENDING theory, and
        it does not appear as an established memory.

        This is the distinction the managed block's matrix encodes
        ("Unverified hypothesis — never a fact or lesson"), and it is
        the one that keeps speculation out of the trusted collection.
        """
        hypothesis = f"{TOKEN}-HYPOTHESIS: the renderer may be miscounting rows"
        proposed = self._call(
            "mpm_theories",
            {
                "action": "propose",
                "params": {
                    "hypothesis": hypothesis,
                    "validation_criteria": "count the rendered rows by hand",
                    "tags": ["probe"],
                },
            },
            session_id="mpm-SESSION-A",
        )
        self.assertTrue(proposed.get("success"), proposed)
        theory_id = proposed.get("id")
        self.assertTrue(theory_id, proposed)
        self.assertEqual(
            proposed.get("status"), "pending",
            "a proposed theory must start pending",
        )

        # It is NOT in the memories collection: a hypothesis is not an
        # established fact.
        as_memory = self._call(
            "mpm_memory",
            {
                "action": "query",
                "params": {"query": TOKEN, "collection": "memories", "limit": 5},
            },
            session_id="mpm-SESSION-B",
        )
        self.assertEqual(
            as_memory.get("count"), 0,
            "a pending theory must not appear as an established memory: "
            f"{as_memory}",
        )

        # It IS in the theories collection, and it is still pending.
        as_theory = self._call(
            "mpm_theories",
            {"action": "query", "params": {"query": TOKEN, "limit": 5}},
            session_id="mpm-SESSION-B",
        )
        self.assertEqual(
            as_theory.get("count"), 1,
            f"the theory must be retrievable as a theory: {as_theory}",
        )
        row = as_theory["theories"][0]
        self.assertEqual(row.get("id"), theory_id)
        metadata = row.get("metadata") or {}
        if isinstance(metadata, str):
            metadata = json.loads(metadata)
        self.assertEqual(
            metadata.get("status"), "pending",
            "the theory must still be pending — nothing in this scenario "
            "resolves it",
        )


if __name__ == "__main__":
    unittest.main()
