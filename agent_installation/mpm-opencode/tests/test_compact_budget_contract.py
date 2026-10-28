"""
test_compact_budget_contract.py — the advertised compaction budget must equal
the implemented one.

WHY THIS EXISTS
---------------
`mpm_system action=compact` publishes a per-invocation safety budget. The
implementation clamps it; the description tells the agent what it will get. If
the two disagree, the agent plans against a number that does not exist.

They disagreed. On 2026-10-28 the shipped OpenCode plugin advertised

    max_batches (default 20, hard cap 100, silently clamped)

while `internal/core/compact.go` declares

    compactDrainMaxBatchesDefault     = 8
    compactDrainMaxBatchesHardCap     = 8

and `internal/core/synth/safeguard.go` clamps the same budget again at
`MaxSemanticStagesPerInvocation = 8`. An agent that believed "20, up to 100"
would invoke the drain three times expecting more work and misread
`max_batches_reached` as a transient condition rather than the ceiling.

The unit was wrong too, and that is the part that makes the number dangerous:
the budget counts SEMANTIC STAGES, not batches. A successful batch costs one
stage; a refused batch costs three (the attempt plus up to two split children).
So "8" can mean eight batches, or two refusals and a success. A bare "8" is not
self-explanatory and "20 batches" is not the same shape of resource at all.

WHAT THIS PINS
--------------
The numbers are derived from the Go source, never hardcoded here, and asserted
against every surface that advertises them:

  1. the shipped plugin source      (`src/index.v2.ts`)
  2. the legacy plugin source       (`src/index.ts`)
  3. the substrate registry         (`internal/core/tools/registry_list.go`)

`dist/` is generated and untracked, so it is not read here; it is rebuilt from
`src/` by `npm run build`.

This is a drift guard, not a behaviour test: it fails when a future edit changes
the constant without changing the sentence, or the sentence without changing the
constant.

Run with:
    python3 -m unittest agent_installation.mpm-opencode.tests.test_compact_budget_contract
"""

from __future__ import annotations

import re
import unittest
from pathlib import Path

SCRIPT_DIR = Path(__file__).resolve().parent.parent
AGENT_INSTALLATION = SCRIPT_DIR.parent
REPO_ROOT = AGENT_INSTALLATION.parent

PLUGIN_SOURCE = SCRIPT_DIR / "src" / "index.v2.ts"
LEGACY_PLUGIN_SOURCE = SCRIPT_DIR / "src" / "index.ts"
COMPACT_IMPL = REPO_ROOT / "internal" / "core" / "compact.go"
SAFEGUARD_IMPL = REPO_ROOT / "internal" / "core" / "synth" / "safeguard.go"
REGISTRY = REPO_ROOT / "internal" / "core" / "tools" / "registry_list.go"

# The sentence shape every surface uses. Kept as one pattern so the plugin and
# the registry cannot drift into different phrasings that this test silently
# stops matching — a regex that matches nothing must fail, not pass.
_LIMITS_RE = re.compile(
    r"max_batches\s*\(default\s+(\d+)\s*,\s*hard cap\s+(\d+)\s*,\s*silently clamped\)"
)


def _go_int(path: Path, name: str) -> int:
    """Read `const <name> = <int>` (optionally typed) out of a Go file."""
    text = path.read_text(encoding="utf-8")
    match = re.search(
        # `const NAME = 8`, `const NAME int = 8`, or a `const ( NAME = 8 )` group.
        r"^\s*(?:const\s+)?" + re.escape(name) + r"\s*(?:int\s*)?=\s*(\d+)\s*$",
        text,
        re.M,
    )
    if not match:
        raise AssertionError(f"{path}: could not find integer constant {name}")
    return int(match.group(1))


def _advertised_limits(path: Path) -> tuple[int, int]:
    """Return (default, hard_cap) as advertised in a description string."""
    text = path.read_text(encoding="utf-8")
    match = _LIMITS_RE.search(text)
    if not match:
        raise AssertionError(
            f"{path}: no 'max_batches (default N, hard cap M, silently clamped)' "
            "advertisement found. If the wording changed, update _LIMITS_RE — do "
            "not delete this assertion."
        )
    return int(match.group(1)), int(match.group(2))


class TestCompactBudgetContract(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.impl_default = _go_int(COMPACT_IMPL, "compactDrainMaxBatchesDefault")
        cls.impl_hard_cap = _go_int(COMPACT_IMPL, "compactDrainMaxBatchesHardCap")
        cls.stage_cap = _go_int(SAFEGUARD_IMPL, "MaxSemanticStagesPerInvocation")

    def test_source_constants_are_internally_consistent(self):
        """A default above the hard cap would clamp before it ever applied."""
        self.assertLessEqual(
            self.impl_default,
            self.impl_hard_cap,
            "the default budget exceeds the hard cap, so the default can never "
            "take effect as written",
        )

    def test_hard_cap_cannot_exceed_the_stage_cap(self):
        """compact.go clamps against MaxSemanticStagesPerInvocation as well."""
        self.assertLessEqual(
            self.impl_hard_cap,
            self.stage_cap,
            "compactDrainMaxBatchesHardCap exceeds MaxSemanticStagesPerInvocation; "
            "the drain would be clamped a second time, so the advertised hard cap "
            "would be unreachable",
        )

    def test_shipped_plugin_advertises_the_implemented_limits(self):
        advertised = _advertised_limits(PLUGIN_SOURCE)
        self.assertEqual(
            advertised,
            (self.impl_default, self.impl_hard_cap),
            "the shipped plugin advertises max_batches "
            f"{advertised[0]}/{advertised[1]} but the implementation clamps at "
            f"{self.impl_default}/{self.impl_hard_cap}",
        )

    def test_legacy_plugin_source_agrees(self):
        self.assertEqual(
            _advertised_limits(LEGACY_PLUGIN_SOURCE),
            (self.impl_default, self.impl_hard_cap),
            "the legacy (non-shipped) plugin source advertises a different budget "
            "from the implementation; it is still compiled and still drifts",
        )

    def test_registry_advertises_the_implemented_limits(self):
        self.assertEqual(
            _advertised_limits(REGISTRY),
            (self.impl_default, self.impl_hard_cap),
            "the registry description is the canonical tool vocabulary and must "
            "agree with the implementation",
        )

    def test_the_budget_is_described_as_semantic_stages(self):
        """The unit is what makes the number meaningful."""
        for path in (PLUGIN_SOURCE, REGISTRY):
            with self.subTest(path=str(path)):
                text = path.read_text(encoding="utf-8")
                self.assertRegex(
                    text,
                    r"SEMANTIC STAGES",
                    "the budget counts semantic stages (a refusal costs 3), not "
                    "batches; a bare number misdescribes the resource",
                )

    # ── negative control ────────────────────────────────────────────────────
    def test_guard_rejects_the_old_wrong_advertisement(self):
        """Prove the assertion is non-vacuous: feed it the defect it was built for."""
        stale = (
            "Optional params: force (default false), "
            "max_batches (default 20, hard cap 100, silently clamped)."
        )
        match = _LIMITS_RE.search(stale)
        self.assertIsNotNone(match, "the guard no longer recognises the shape")
        advertised = (int(match.group(1)), int(match.group(2)))
        self.assertNotEqual(
            advertised,
            (self.impl_default, self.impl_hard_cap),
            "the guard would have accepted the 20/100 advertisement it exists to "
            "reject",
        )


if __name__ == "__main__":
    unittest.main()
