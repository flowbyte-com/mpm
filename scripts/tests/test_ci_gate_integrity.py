#!/usr/bin/env python3
"""CI gate integrity: the workflow's steps must actually be able to fail.

A gate that cannot fail is not a gate. This file pins the properties that
make the workflow's jobs real:

  * a step whose body pipes into another command must run under a shell
    with `pipefail`, or the pipeline's exit status is the LAST command's
    (usually `tee`, always 0) and the step cannot report the failure of
    the command that was actually being verified;
  * a workflow that runs the same verification for many commits must
    cancel the runs that a newer commit has superseded;
  * the three gating jobs, and the checks each is responsible for, must
    still exist, and no job or step may be marked `continue-on-error`.

The `tee` case is not hypothetical. GitHub Actions runs a `run:` step with
the default shell `bash -e {0}` — errexit only, NO pipefail. It applies
`-o pipefail` ONLY when the step opts in with `shell: bash`
(`bash --noprofile --norc -eo pipefail {0}`). A step that read

    go test -tags fts5 ./... 2>&1 | tee test-output.log

under the default shell therefore reported `tee`'s exit status, so the
"Full test suite" job could read `success` while its own log showed
`FAIL github.com/flowbyte-com/mpm/cmd/mpm`. The sweep printed a log; it
did not gate anything.

This file exists so that the next person who reaches for `| tee` does not
silently remove the gate again.

The workflow is scanned structurally rather than through a YAML parser,
following the approach already used by
`scripts/tests/test_release_gate_bootstrap_wiring.py`: the runner image is
not assumed to carry PyYAML, and the workflow's indentation is fixed and
well understood.
"""

import re
import unittest
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parent.parent.parent
WORKFLOW = REPO_ROOT / ".github" / "workflows" / "build-test.yml"


def _workflow_text() -> str:
    return WORKFLOW.read_text(encoding="utf-8")


def _job_block(text: str, job_name: str) -> str:
    """Return the body of a `job_name:` top-level mapping in the workflow.

    Returns '' when the job is absent. Structural, not a parsed AST: the
    scan stops at the next `  <name>:` line at the same indentation, which
    matches the workflow's two-space indent for top-level job keys.
    """
    pattern = re.compile(rf"^  {re.escape(job_name)}:\s*$", re.MULTILINE)
    match = pattern.search(text)
    if not match:
        return ""
    lines: list[str] = []
    for raw_line in text[match.end():].splitlines():
        if raw_line.startswith("    ") or raw_line == "":
            lines.append(raw_line)
            continue
        break
    return "\n".join(lines)


def _step_blocks(job_text: str):
    """Yield (step_name, step_text) for every named step in a job body.

    Steps are `      - name: X` (six spaces, dash) with their keys at
    eight. A step's text runs until the next step or the end of the job.
    """
    lines = job_text.splitlines()
    starts = []
    for i, raw in enumerate(lines):
        m = re.match(r"^      - name:\s*(.+?)\s*$", raw)
        if m:
            starts.append((i, m.group(1)))
    for idx, (i, name) in enumerate(starts):
        end = starts[idx + 1][0] if idx + 1 < len(starts) else len(lines)
        yield name, "\n".join(lines[i:end])


def _strip_comments(text: str) -> str:
    """Remove whole-line YAML comments and trailing ' # ...' comments.

    Used where a check is about a KEY, not about prose: the workflow's
    header discusses `continue-on-error` at length precisely because it
    was removed, and that discussion must not read as its presence.
    """
    out = []
    for raw in text.splitlines():
        stripped = raw.strip()
        if stripped.startswith("#"):
            continue
        # A trailing comment only starts at a '#' that is outside quotes.
        # The workflow has no '#' inside a value except in shell comments,
        # which are inside block scalars and never follow a value on the
        # same line.
        out.append(re.sub(r"\s+#.*$", "", raw))
    return "\n".join(out)


def _has_pipeline(body: str) -> bool:
    """True if the body contains a shell pipeline.

    A '|' inside a quoted string is not a pipeline: the workflow uses
    `-run "TestA|TestB|TestC"`, where the alternation belongs to the
    regular expression, not to the shell. Quoted spans are therefore
    removed before the search.

    The check is per line. A quoted span may cover several lines (a
    YAML block scalar's content, for instance), and removing it as one
    region would join the lines on either side of it and invent a
    pipeline that is not there.
    """
    for raw in body.splitlines():
        line = re.sub(r'"[^"\n]*"|\'[^\'\n]*\'', "", raw)
        if re.search(r"\S\s*\|\s*\S", line):
            return True
    return False


class TestPipelineStepsCanFail(unittest.TestCase):
    """Every step that pipes must be able to report the piped failure."""

    def test_no_step_pipes_without_pipefail(self):
        offenders = []
        text = _workflow_text()
        for job_name in ("gate", "full-suite", "release-gate"):
            job_text = _job_block(text, job_name)
            self.assertTrue(job_text, f"the {job_name} job is absent")
            for step_name, step_text in _step_blocks(job_text):
                body = _strip_comments(step_text)
                # A pipeline exists only if a command is piped INTO
                # something; a bare '|' in a quoted regex is not.
                if not _has_pipeline(body):
                    continue
                # `shell: bash` gives `bash --noprofile --norc -eo
                # pipefail`, the documented way to make a pipeline's
                # status the failure of any element in it.
                if re.search(r"^\s*shell:\s*bash\s*$", body, re.MULTILINE):
                    continue
                # An explicit `set -o pipefail` in the body works too.
                if re.search(r"set\s+-[a-zA-Z]*o\s+pipefail", body):
                    continue
                offenders.append(f"{job_name} / {step_name}")
        self.assertEqual(
            offenders, [],
            "these steps pipe a verification command into another command "
            "without enabling pipefail, so the step reports the LAST "
            "command's exit status and cannot fail on the verification "
            "itself: " + ", ".join(offenders),
        )

    def test_the_full_sweep_step_opts_into_pipefail(self):
        """Pin the fix for the step that was actually vacuous."""
        job_text = _job_block(_workflow_text(), "full-suite")
        block = ""
        for step_name, step_text in _step_blocks(job_text):
            if step_name == "Full test sweep":
                block = step_text
                break
        self.assertTrue(
            block,
            "the full-suite job no longer has a 'Full test sweep' step",
        )
        self.assertRegex(
            block,
            r"(?m)^\s*shell:\s*bash\s*$",
            "the 'Full test sweep' step must declare `shell: bash`; the "
            "default `bash -e {0}` has no pipefail, so `go test | tee` "
            "could not fail the step.",
        )
        self.assertIn("| tee test-output.log", block)
        self.assertIn("| tee tools-output.log", block)


class TestSupersededRunsAreCancelled(unittest.TestCase):
    """A push must not leave the previous commit's run burning minutes."""

    def test_workflow_declares_a_concurrency_group(self):
        self.assertRegex(
            _strip_comments(_workflow_text()),
            r"(?m)^concurrency:\s*$",
            "the workflow declares no concurrency group, so every push "
            "leaves the previous commit's run running to a conclusion "
            "nobody reads.",
        )

    def test_concurrency_group_is_scoped_to_the_ref(self):
        """A per-ref group: two PRs must never cancel each other."""
        block = self._concurrency_block()
        self.assertRegex(
            block,
            r"github\.ref",
            "the concurrency group must include github.ref, otherwise a "
            "push to one branch would cancel an unrelated branch's run.",
        )

    def test_superseded_runs_are_cancelled(self):
        block = self._concurrency_block()
        self.assertRegex(
            block,
            r"cancel-in-progress:\s*true",
            "cancel-in-progress must be true, otherwise a superseded run "
            "still runs to completion.",
        )

    @staticmethod
    def _concurrency_block() -> str:
        text = _workflow_text()
        match = re.search(r"^concurrency:\s*$", text, re.MULTILINE)
        if not match:
            return ""
        lines = []
        for raw in text[match.end():].splitlines():
            if not raw.strip():
                continue
            if not raw.startswith((" ", "\t")):
                break
            lines.append(raw.strip())
        return "\n".join(lines)


class TestGatingJobsStillExist(unittest.TestCase):
    """The three jobs, and what each is responsible for, must survive."""

    def test_the_three_gating_jobs_are_present(self):
        text = _workflow_text()
        for job in ("gate", "full-suite", "release-gate"):
            self.assertTrue(
                _job_block(text, job),
                f"the workflow no longer defines the {job} job",
            )

    def test_the_gate_job_still_runs_the_installer_contract_suite(self):
        self.assertIn(
            "make test-scripts", _job_block(_workflow_text(), "gate"),
            "the installer-contract suite is no longer run by the gate job",
        )

    def test_the_gate_job_still_runs_the_race_sweep_with_coverage(self):
        block = _job_block(_workflow_text(), "gate")
        self.assertIn("-race", block,
                      "the gate job no longer runs a race-detector sweep")
        self.assertIn("-coverprofile=coverage.out", block,
                      "the gate job no longer produces a coverage profile")

    def test_the_release_gate_job_still_runs_the_release_gate(self):
        self.assertIn(
            "make release-gate", _job_block(_workflow_text(), "release-gate"),
            "the release gate is no longer run",
        )

    def test_no_job_or_step_is_marked_continue_on_error(self):
        """`continue-on-error` hides a red job behind a green conclusion.

        The workflow's own header records that this happened once: a job
        "reads `success` while its own log shows `FAIL`". The same
        masking is achievable with an unguarded pipeline, which is what
        the pipefail test above exists to prevent.
        """
        text = _strip_comments(_workflow_text())
        self.assertNotRegex(
            text,
            r"(?m)^\s*continue-on-error:\s*true\s*$",
            "a job or step is marked continue-on-error; its conclusion "
            "would read success while its log shows a failure",
        )


if __name__ == "__main__":
    unittest.main()
