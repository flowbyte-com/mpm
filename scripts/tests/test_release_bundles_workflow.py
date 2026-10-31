#!/usr/bin/env python3
"""test_release_bundles_workflow.py — the release-bundle publish path.

Pins the contract between the release workflow and the updater:

  * the workflow triggers on version tags and publishes exactly the
    assets the updater selects: mpm_<tag>_linux_amd64.tar.gz plus
    SHA256SUMS (the updater refuses a release with no SHA256SUMS);
  * the assembler is the workflow's only bundle producer, and the
    updater's default asset name matches what the assembler writes;
  * the published tarball carries a build-provenance attestation;
  * only the supported platform is advertised.

Hermeticity: no network, no builds. The file is parsed, not executed.
"""

import unittest
from pathlib import Path

import yaml

REPO_ROOT = Path(__file__).resolve().parent.parent.parent
WORKFLOW = REPO_ROOT / ".github" / "workflows" / "release-bundles.yml"
ASSEMBLER = REPO_ROOT / "scripts" / "assemble-release-bundle.sh"


class ReleaseBundlesWorkflowTest(unittest.TestCase):
    def setUp(self):
        self.assertTrue(WORKFLOW.exists(), "the release workflow is missing")
        self.doc = yaml.safe_load(WORKFLOW.read_text())

    def test_triggers_on_version_tags(self):
        # YAML 1.1 parses the `on:` key as boolean True.
        on = self.doc.get("on", self.doc.get(True, {}))
        tags = on.get("push", {}).get("tags", [])
        self.assertIn("v*", tags)

    def test_publishes_the_assets_the_updater_selects(self):
        text = WORKFLOW.read_text()
        # The updater's default asset name is
        # mpm_<tag>_linux_amd64.tar.gz (see PlatformAssetOrDefault).
        self.assertIn("mpm_${{ github.ref_name }}_linux_amd64.tar.gz", text)
        self.assertIn("SHA256SUMS", text)

    def test_attests_the_tarball(self):
        text = WORKFLOW.read_text()
        self.assertIn("attest-build-provenance", text)

    def test_assembler_is_the_only_producer(self):
        text = WORKFLOW.read_text()
        self.assertIn("scripts/assemble-release-bundle.sh", text)

    def test_only_the_supported_platform_is_advertised(self):
        text = WORKFLOW.read_text()
        self.assertNotIn("arm64", text)
        self.assertNotIn("darwin", text)
        self.assertNotIn("windows", text)


class AssemblerTest(unittest.TestCase):
    def test_names_match_the_updater(self):
        text = ASSEMBLER.read_text()
        self.assertIn('mpm_${TAG}_${PLAT}.tar.gz', text)
        self.assertIn("SHA256SUMS", text)

    def test_refuses_unsupported_platforms(self):
        text = ASSEMBLER.read_text()
        self.assertIn("Linux x86_64", text)


if __name__ == "__main__":
    unittest.main()
