#!/usr/bin/env python3
"""block_behaviour_comparison.py — old block vs new block, on the REAL
OpenCode host, with IDENTICAL scenarios.

WHAT THIS IS.

Contract 4.0.0 replaced the managed block's ten prose responsibilities
with a compact behaviour matrix (2,009 → 599 tokens). An instruction
change is only worth anything if it changes BEHAVIOUR, so this harness
measures the behaviour, not the text.

It runs the real installed `opencode` host, in a disposable project,
against a disposable MPM workspace and database, and compares what the
agent PERSISTS under each block variant.

THE VARIANTS.

  old — the 3.0.0 contract block, verbatim (fixtures/block-3.0.0.md).
  new — the 4.0.0 compact contract block, the canonical source's own
        last managed block.

THE SCENARIOS.

Five identical prompts, each aimed at one behaviour the contract
names. The prompts never mention MPM: they are ordinary work, so what
is measured is the block's influence and not the prompt's.

  reusable-discovery — find and prove a project constraint
  uncertain-hypothesis — give an estimate and state the confidence
  routine-observation — count the lines in a file
  transfer — write up the state for the next engineer
  acknowledgement — "ok"

WHAT IS MEASURED.

The rows the agent created in the disposable store: memories,
decisions, theories, lessons, handoffs, work items. Counted per
collection, with a bounded excerpt of what was written.

WHAT IT DOES NOT DO.

It does not modify the live installation, the live database or the live
OpenCode configuration. Every XDG root, the workspace and the database
are redirected into a temporary directory that is removed afterwards.

A NOTE ON WHAT THIS CAN AND CANNOT SHOW.

One run per (variant, scenario) is ONE observation. It is evidence,
not proof: model behaviour varies, and a single miss is not a verdict.
The report that uses this harness says so, and reports the misses as
well as the hits.

Usage:
    python3 agent_installation/measure/block_behaviour_comparison.py
    python3 agent_installation/measure/block_behaviour_comparison.py --json OUT
"""

from __future__ import annotations

import argparse
import json
import os
import re
import shlex
import shutil
import sqlite3
import subprocess
import sys
import tempfile
import time
from pathlib import Path


REPO_ROOT = Path(__file__).resolve().parents[2]
AGENT_INSTALLATION = REPO_ROOT / "agent_installation"
ADAPTER_DIR = AGENT_INSTALLATION / "mpm-opencode"
FIXTURES = AGENT_INSTALLATION / "measure" / "fixtures"
CANONICAL_SOURCE = AGENT_INSTALLATION / "MPM_AGENT_INTEGRATION_SNIPPETS.md"

OPENCODE_BIN = os.environ.get(
    "OPENCODE_BIN", str(Path.home() / ".opencode/bin/opencode"),
)
MODEL = os.environ.get(
    "MPM_DEV_HOST_MODEL", "minimax-coding-plan/MiniMax-M3.1-Flash",
)

# ---------------------------------------------------------------------------
# The contamination detector.
#
# TWO BUCKETS, because one bucket would be dishonest.
#
# BLOCKING — the agent read something that would change what it does,
# so the cell is not a measurement of the block:
#   * the harness's own source (it would learn the scenarios, the
#     block variants, and how the result is counted);
#   * a block fixture or the other arm's reference stack (it would
#     learn the contract it is being compared against);
#   * the real repository's canonical source or protocol.
#
# NOTED — the agent reached the real HOME. This is recorded but does
# NOT invalidate the cell, because two benign causes dominate:
#   * the host's own binary and runtime paths appear in the
#     transcript because the agent's shell prints them;
#   * the SUBSTRATE seeds a prime directive on first use — "On every
#     session start, after read_wake_context, run `systemctl --user
#     is-active mpm-scheduler`" — so a health check against the real
#     scheduler unit is expected in EVERY cell of EVERY arm.
# Neither leaks the block under test.
#
# A cell with a BLOCKING hit is reported as CONTAMINATED and excluded
# from the primary comparison. A cell with only NOTED hits is
# reported, with the reach listed.
# ---------------------------------------------------------------------------

# Artefacts whose CONTENT would change the agent's behaviour.
#
# MATCHED ON ABSOLUTE PATHS, NOT ON BARE FILENAMES.
#
# The managed block names `agent_installation/MPM_AGENT_INTEGRATION_SNIPPETS.md`
# (in its provenance comment) and
# `~/.mpm/agent_installation/mpm-agent-protocol.md` (in its last line). Both
# strings are therefore in EVERY cell's system prompt, and the agent's own
# legitimate `cat` of the served protocol puts the bare filename in the
# transcript too. A bare-filename marker would flag every cell — including
# the ones that read exactly what they were told to read.
#
# So the markers here are the harness's own artefacts BY ABSOLUTE PATH. The
# cell's HOME is a temporary directory, so an absolute path under the real
# repository means the agent reached outside its cell.
BLOCKING_MARKERS = (
    str(REPO_ROOT / "agent_installation" / "MPM_AGENT_INTEGRATION_SNIPPETS.md"),
    str(REPO_ROOT / "agent_installation" / "mpm-agent-protocol.md"),
    str(REPO_ROOT / "README.md"),
    str(REPO_ROOT / "agent_installation" / "measure" / "block_behaviour_comparison.py"),
    str(REPO_ROOT / "agent_installation" / "measure" / "mutation_probe.py"),
    str(REPO_ROOT / "agent_installation" / "measure" / "installer_regression.py"),
    str(REPO_ROOT / "agent_installation" / "measure" / "fixtures"),
    str(REPO_ROOT / "agent_installation" / "tests"),
    str(REPO_ROOT / "agent_installation" / "scripts"),
    str(REPO_ROOT / "docs" / "dev"),
)

# The harness's own directories. The OpenCode adapter directory is
# deliberately NOT here: the host legitimately references the plugin
# path (it is registered in the cell's own config), so a path under
# it proves nothing.
BLOCKING_PREFIXES = ()


# Absolute paths, for the outside-the-cell detector.
_ABSOLUTE_PATH_RE = re.compile(r"/(?:[A-Za-z0-9._-]+/)+[A-Za-z0-9._-]+")


# ---------------------------------------------------------------------------
# The per-arm reference stack.
#
# THE ARM IS NOT JUST THE BLOCK.
#
# The 3.0.0 block points at
# `~/.mpm/agent_installation/mpm-agent-protocol.md`, and an agent that
# follows that pointer READS IT. In a real installation `~/.mpm` is the
# MPM checkout, so the pointer resolves. In the harness `~/.mpm` did not
# exist — and a measured run of the OLD arm did exactly what the block
# tells it to do: it went looking for the protocol, found the REAL
# repository's copy, and read the 4.0.0 commitments it was supposed to
# be compared against.
#
# So each arm is given the reference stack it would actually have had:
#
#   old arm — the 3.0.0 block, the 3.0.0 protocol, the 3.0.0 canonical
#             source. That is the world that shipped.
#   new arm — the 4.0.0 block, the 4.0.0 protocol, the 4.0.0 canonical
#             source. That is the world this branch proposes.
#
# Comparing the two worlds is the question the release decision asks.
# Comparing two blocks over one fixed reference stack would flatter
# the old arm: the 4.0.0 protocol carries the C1–C12 commitments that
# the old block used to carry in prose, so serving it to the old arm
# would hand that arm the very content under test.
# ---------------------------------------------------------------------------


def _isolated_path(tmp: Path, mpm_binary: str) -> str:
    """A PATH whose `mpm` is the binary under test, and nothing else.

    Every PATH directory that carries an `mpm` is dropped, so the
    agent cannot reach the live installation's binary by name.
    """
    bindir = tmp / "bin"
    bindir.mkdir(parents=True, exist_ok=True)
    link = bindir / "mpm"
    if link.is_symlink() or link.exists():
        link.unlink()
    link.symlink_to(mpm_binary)
    kept = [
        d for d in os.environ.get("PATH", "").split(os.pathsep)
        if d and not (Path(d) / "mpm").exists()
    ]
    return os.pathsep.join([str(bindir), *kept])


def _reference_stack(variant: str, stage: Path) -> dict[str, str]:
    """The files that arm's `~/.mpm/agent_installation/` must contain."""
    if variant == "old":
        return {
            "mpm-agent-protocol.md": (
                stage / "fixtures" / "protocol-3.0.0.md"
            ).read_text(encoding="utf-8"),
            "MPM_AGENT_INTEGRATION_SNIPPETS.md": (
                stage / "fixtures" / "snippets-3.0.0.md"
            ).read_text(encoding="utf-8"),
        }
    return {
        "mpm-agent-protocol.md": (
            stage / "mpm-agent-protocol.md"
        ).read_text(encoding="utf-8"),
        "MPM_AGENT_INTEGRATION_SNIPPETS.md": (
            stage / CANONICAL_SOURCE.name
        ).read_text(encoding="utf-8"),
    }


def _mpm_binary() -> str:
    candidates = [
        os.environ.get("MPM_BINARY"),
        str(REPO_ROOT / ".build/bin/mpm"),
        str(Path.home() / "src/mpm-opencode/.build/bin/mpm"),
    ]
    for c in candidates:
        if c and Path(c).is_file() and os.access(c, os.X_OK):
            return c
    raise RuntimeError("no executable mpm binary found; set MPM_BINARY")


# ---------------------------------------------------------------------------
# The synthetic project. Identical for every variant, so the ONLY
# difference between two runs is the managed block.
# ---------------------------------------------------------------------------

RENDERER = '''\
"""A minimal renderer with one deliberate constraint."""
import re
import sys

BLOCK_RE = re.compile(
    r"<!-- BEGIN MPM MANAGED BLOCK -->\\n.*?<!-- END MPM MANAGED BLOCK -->\\n",
    re.DOTALL,
)


def render(path):
    text = open(path, encoding="utf-8").read()
    m = BLOCK_RE.search(text)
    if not m:
        raise SystemExit("no managed block found")
    # The constraint: the extracted block must END WITH A NEWLINE.
    # A bare `.*?` would accept a block whose last character is the
    # END marker's ">", and the byte-for-byte parity check then
    # fails against the checked-in snippet.
    if not m.group(0).endswith("\\n"):
        raise SystemExit(
            "FAIL: the managed block must end with a newline after the "
            "END marker"
        )
    return m.group(0)


if __name__ == "__main__":
    render(sys.argv[1])
    print("OK")
'''

BLOCK_MD = """\
<!-- BEGIN MPM MANAGED BLOCK -->
<!-- source: canonical.md -->
# Example managed block

This block is a placeholder for the renderer's fixture.
<!-- END MPM MANAGED BLOCK -->
"""

CANONICAL_MD = """\
# Canonical managed-block source (fixture)

This is the document `block.md` was extracted from. It holds one
managed block, between the `<!-- BEGIN MPM MANAGED BLOCK -->` and
`<!-- END MPM MANAGED BLOCK -->` markers, and the renderer requires
the END marker to be followed immediately by a newline.

<!-- BEGIN MPM MANAGED BLOCK -->
<!-- source: canonical.md -->
# Example managed block

This block is a placeholder for the renderer's fixture.
<!-- END MPM MANAGED BLOCK -->
"""


NOTES_TXT = "\n".join(f"note line {i:03d}" for i in range(1, 41)) + "\n"

# THE FIXTURE MUST ANSWER EVERY QUESTION IT RAISES.
#
# `block.md`'s provenance comment names `canonical.md`. If that file is
# not in the fixture, the agent looks for it — and the only `canonical.md`
# on this machine is inside the harness's own repository, so it finds
# the production tree, reads the canonical source, the scripts and the
# tests, and the cell becomes a measurement of the harness.
#
# That happened, and it was measured: the new arm's `reusable-discovery`
# cell came back with the harness's own repository in its transcript.
#
# So `canonical.md` IS in the fixture, and it is a plausible document
# rather than a stub. Every name the fixture mentions resolves inside
# the fixture, and the agent has no reason to leave it.
PROJECT_FILES = {
    "renderer.py": RENDERER,
    "block.md": BLOCK_MD,
    "canonical.md": CANONICAL_MD,
    "notes.txt": NOTES_TXT,
    "README.md": (
        "# Fixture project\n\n"
        "`renderer.py` extracts the managed block from `block.md` and\n"
        "checks it. Run it with `python3 renderer.py block.md`.\n\n"
        "`canonical.md` is the document `block.md` was extracted from.\n"
    ),
}


SCENARIOS = {
    "reusable-discovery": (
        "Run `python3 renderer.py block.md`. It fails. Find the root "
        "cause, prove it by reading the code, and report the constraint "
        "in one sentence."
    ),
    "uncertain-hypothesis": (
        "Estimate, to the nearest hundred, how many tokens `block.md` "
        "costs when it is loaded into an agent's system prompt. State "
        "your confidence, and say what would make the estimate exact."
    ),
    "routine-observation": (
        "How many lines does `notes.txt` have? Report the number only."
    ),
    "transfer": (
        "Another engineer takes over this project tomorrow. Write up "
        "what they need to continue: what state the work is in, what is "
        "unfinished, what is blocked, and what to do next."
    ),
    "acknowledgement": "ok",
}


# ---------------------------------------------------------------------------
# The block variants.
# ---------------------------------------------------------------------------


def _extract_last_block(text: str) -> str:
    import re
    matches = list(re.finditer(
        r"<!-- BEGIN MPM MANAGED BLOCK -->\n.*?<!-- END MPM MANAGED BLOCK -->\n",
        text, re.DOTALL,
    ))
    if not matches:
        raise RuntimeError("no managed block found")
    return matches[-1].group(0)


# ---------------------------------------------------------------------------
# THE STAGE.
#
# EVERY INPUT THE HARNESS USES IS COPIED INTO A PRIVATE STAGING DIRECTORY
# BEFORE THE RUN, AND THE CELLS SEE ONLY THE STAGE.
#
# WHY. The harness lives inside a git worktree, and the worktree lives
# under a directory the agent can explore. A cell's `opencode.jsonc`
# names the plugin directory by ABSOLUTE PATH — so the agent could read
# its own configuration, learn the repository's location, and walk into
# it. That happened: a measured cell ran
#
#     cd /tmp/opencode/managed-block/agent_installation/scripts && \
#         grep -n "BEGIN MPM MANAGED" render_managed_blocks.py
#
# which is the production renderer, read out of the harness's own
# repository. The cell was measuring the harness.
#
# The stage fixes the pointer: the cell's plugin payload is a COPY of
# the adapter's runtime files (package.json, server.js, dist/) in a
# temporary directory, so the configuration names a path with no
# repository behind it. The canonical source, the protocol and the 3.0.0
# fixtures are copied in too, so the run is reproducible even if the
# working tree changes under it.
#
# A blind filesystem-wide search can still find the real tree. The
# contamination detector catches that, and the report reports the
# cell as contaminated rather than counting it. What the stage removes
# is the POINTER, which is what turned a local task into a repository
# excavation.
# ---------------------------------------------------------------------------


def _stage_inputs(stage: Path) -> None:
    """Copy every harness input into the private stage."""
    stage.mkdir(parents=True, exist_ok=True)

    # The canonical source and the protocol: the new arm's reference
    # stack, and the new arm's block.
    shutil.copy2(CANONICAL_SOURCE, stage / CANONICAL_SOURCE.name)
    shutil.copy2(
        AGENT_INSTALLATION / "mpm-agent-protocol.md",
        stage / "mpm-agent-protocol.md",
    )

    # The 3.0.0 fixtures: the old arm's reference stack and its block.
    fixtures_stage = stage / "fixtures"
    fixtures_stage.mkdir(exist_ok=True)
    for name in ("block-3.0.0.md", "protocol-3.0.0.md", "snippets-3.0.0.md"):
        shutil.copy2(FIXTURES / name, fixtures_stage / name)

    # The plugin payload: exactly what OpenCode loads, and nothing else.
    # `templates/AGENTS.md.snippet` is deliberately NOT copied — it
    # carries the managed block, and the cell must not be able to read
    # the block from the adapter's directory.
    plugin_stage = stage / "plugin"
    plugin_stage.mkdir(exist_ok=True)
    shutil.copy2(ADAPTER_DIR / "package.json", plugin_stage / "package.json")
    shutil.copy2(ADAPTER_DIR / "server.js", plugin_stage / "server.js")
    if (ADAPTER_DIR / "dist").is_dir():
        shutil.copytree(
            ADAPTER_DIR / "dist", plugin_stage / "dist", dirs_exist_ok=True,
        )


def _variants(stage: Path) -> dict[str, str]:
    old = (stage / "fixtures" / "block-3.0.0.md").read_text(encoding="utf-8")
    new = _extract_last_block(
        (stage / CANONICAL_SOURCE.name).read_text(encoding="utf-8")
    )
    return {"old": old, "new": new}


# ---------------------------------------------------------------------------
# The runner.
# ---------------------------------------------------------------------------


def _measure(db_path: Path) -> dict:
    """Count the rows the agent created, by collection."""
    if not db_path.exists():
        return {"memories": 0, "decisions": 0, "theories": 0,
                "lessons": 0, "handoffs": 0, "works": 0, "excerpts": []}
    conn = sqlite3.connect(f"file:{db_path}?mode=ro", uri=True)
    try:
        def count(table: str, where: str = "") -> int:
            try:
                return conn.execute(
                    f"SELECT COUNT(*) FROM {table} {where}",
                ).fetchone()[0]
            except sqlite3.Error:
                return 0

        out = {
            "memories": count("memories", "WHERE collection='memories'"),
            "decisions": count("memories", "WHERE collection='decisions'"),
            "theories": count("memories", "WHERE collection='theories'"),
            "lessons": count("lessons"),
            "handoffs": count("session_handoffs"),
            "works": count("works"),
            # The substrate seeds its own prime directives on first
            # use. They are agent-facing behaviour instructions, so
            # they are a CONFOUND for a behavioural comparison — but
            # they are identical in both variants, so the comparison
            # stays controlled. They are counted so the report can
            # say so rather than leave it implicit.
            "directives": count("memories", "WHERE collection='directives'"),
            "excerpts": [],
        }
        for row in conn.execute(
            "SELECT collection, substr(content, 1, 300) FROM memories "
            "WHERE collection != 'directives' ORDER BY created_at",
        ):
            out["excerpts"].append({"collection": row[0], "content": row[1]})
        for row in conn.execute(
            "SELECT substr(content, 1, 300) FROM lessons ORDER BY created",
        ):
            out["excerpts"].append({"collection": "lessons", "content": row[0]})
        for row in conn.execute(
            "SELECT substr(title, 1, 300) FROM works ORDER BY created_at",
        ):
            out["excerpts"].append({"collection": "work", "content": row[0]})
        for row in conn.execute(
            "SELECT substr(summary, 1, 300), substr(commitments, 1, 200), "
            "substr(open_questions, 1, 200) FROM session_handoffs "
            "ORDER BY created_at",
        ):
            out["excerpts"].append({
                "collection": "handoff",
                "content": f"summary={row[0]!r} commitments={row[1]!r} "
                            f"open_questions={row[2]!r}",
            })
        return out
    finally:
        conn.close()


def _strip_legitimate(transcript: str, variant: str, block: str,
                      stage: Path) -> str:
    """Remove the arm's OWN reading from the transcript.

    The agent is EXPECTED to read the block it was given and the
    protocol that block points at. Those reads are the contract
    working, not contamination — and the block names both files, so
    their names appear in every cell's system prompt.

    The removal has to survive the transcript's encoding: OpenCode
    stores messages as JSON, so a document's newlines appear as the
    two-character sequence `\n`. Each legitimate body is therefore
    stripped in three forms — verbatim, JSON-escaped, and with all
    whitespace removed — because the compact form matches regardless
    of how the store chose to encode it.
    """
    out = transcript
    stack = _reference_stack(variant, stage)
    for body in (
        block,
        stack.get("mpm-agent-protocol.md", ""),
        stack.get("MPM_AGENT_INTEGRATION_SNIPPETS.md", ""),
    ):
        if not body:
            continue
        for needle in (
            body,
            body.replace("\n", "\\n"),
            re.sub(r"\s+", "", body),
        ):
            if needle:
                out = out.replace(needle, " ")
    return out


def _scan_transcript(tmp: Path) -> str:
    """The agent's FULL transcript, not just the host's stdout.

    The host's stdout contains the agent's final message and the
    host's own narration. It does NOT contain the agent's tool calls
    and their outputs — and an agent that reads a harness artefact
    through a shell command sees it only in that tool output.

    So a contamination detector that scans stdout alone has a blind
    spot: it reports a cell that NARRATED an artefact and misses a
    cell that READ one. That asymmetry makes a `contaminated: false`
    flag no evidence at all.

    The cell's own OpenCode data directory holds the session store:
    every message and every tool-call part. This walks it and
    concatenates the text, so the detector sees what the agent
    actually saw.
    """
    data = tmp / "data" / "opencode"
    if not data.is_dir():
        return ""
    chunks: list[str] = []
    for path in sorted(data.rglob("*")):
        if not path.is_file():
            continue
        try:
            if path.stat().st_size > 8 * 1024 * 1024:
                continue
            chunks.append(path.read_text(encoding="utf-8", errors="replace"))
        except OSError:
            continue
    return "\n".join(chunks)


def _run_one(variant: str, block: str, scenario: str, prompt: str,
              mpm_binary: str, timeout: int = 600,
              transcript_dir: Path | None = None,
              stage: Path | None = None) -> dict:
    assert stage is not None, "the harness must run against a staged copy"
    tmp = Path(tempfile.mkdtemp(prefix=f"mpm-behav-{variant}-{scenario}-"))
    try:
        project = tmp / "project"
        project.mkdir(parents=True)
        for name, content in PROJECT_FILES.items():
            (project / name).write_text(content, encoding="utf-8")

        workspace = tmp / "workspace"
        (workspace / "src" / "db").mkdir(parents=True)
        db_path = workspace / "src" / "db" / "mpm.db"

        # The arm's reference stack, AT THE PATH THE BLOCK NAMES.
        #
        # `~/.mpm` in a real installation IS the MPM checkout, so the
        # pointer `~/.mpm/agent_installation/mpm-agent-protocol.md`
        # resolves. Here it resolves to this arm's protocol — and to
        # nothing else, so the agent cannot wander into the real
        # repository and read the other arm's contract.
        #
        # HOME IS `tmp`, so `~` IS `tmp` and the served path is
        # `tmp/.mpm/agent_installation/`. An earlier revision served it
        # under `tmp/home/.mpm/…`, which is NOT where `~` pointed: the
        # pointer did not resolve, the agent went looking for it, found
        # the real repository, and read the other arm's contract. A
        # harness that does not resolve the pointer it is testing is
        # measuring a filesystem search, not a contract.
        mpm_home = tmp / ".mpm" / "agent_installation"
        mpm_home.mkdir(parents=True, exist_ok=True)
        for name, body in _reference_stack(variant, stage).items():
            (mpm_home / name).write_text(body, encoding="utf-8")

        # The user-scope instruction file. XDG_CONFIG_HOME is
        # redirected, so this is the ONLY managed block the host can
        # see.
        config = tmp / "config" / "opencode"
        config.mkdir(parents=True)
        (config / "AGENTS.md").write_text(block, encoding="utf-8")

        # Register THIS repository's plugin, so the agent has the
        # MPM tools.
        # The plugin is the STAGED copy. The cell's configuration must
        # not name a path with a repository behind it: the agent can
        # read its own configuration, and a repository path in it is an
        # invitation to go and read that repository.
        (config / "opencode.jsonc").write_text(
            "{\n"
            '  "$schema": "https://opencode.ai/config.json",\n'
            f'  "plugins": ["file://{stage / "plugin"}"]\n'
            "}\n",
            encoding="utf-8",
        )

        env = {
            **os.environ,
            "HOME": str(tmp),
            "XDG_CONFIG_HOME": str(tmp / "config"),
            "XDG_DATA_HOME": str(tmp / "data"),
            "XDG_STATE_HOME": str(tmp / "state"),
            "XDG_CACHE_HOME": str(tmp / "cache"),
            "OPENCODE_CONFIG_DIR": str(config),
            "OPENCODE_DISABLE_AUTOUPDATE": "1",
            "MPM_WORKSPACE": str(workspace),
            "MPM_DB_PATH": str(db_path),
            "MPM_BINARY": mpm_binary,
            # PATH isolation. The block tells the agent that
            # `mpm call <tool>` reaches what the host does not expose,
            # so the agent WILL run `mpm`. On this machine `mpm` on
            # PATH is the LIVE installation (`~/.local/bin/mpm` ->
            # `~/.mpm/bin/mpm`), and a measured run used it. The
            # agent's writes still landed in the disposable store
            # (MPM_WORKSPACE/MPM_DB_PATH were inherited), so no live
            # data was touched — but the cell was running a different
            # binary than the one under test, so the cell is
            # contaminated by the harness's own detector.
            #
            # The fix: a cell-local `bin/` whose `mpm` is the binary
            # under test, and every PATH directory that carries an
            # `mpm` removed.
            "PATH": _isolated_path(tmp, mpm_binary),
        }
        # These mark the process as "running inside opencode". A
        # nested opencode that inherits them can attach to the
        # OUTER session's project instead of resolving its own, so
        # they are stripped.
        for inherited in (
            "OPENCODE", "OPENCODE_SESSION_ID", "OPENCODE_TERMINAL",
            "AI_AGENT", "MPM_PROVENANCE_FRAMEWORK",
        ):
            env.pop(inherited, None)

        # THE LAUNCH FORM MATTERS — measured, not assumed.
        #
        # `opencode run --standalone` spawns a private server, and the
        # server's working directory is NOT reliably the client's. When
        # the CLI is exec'd with a changed cwd (`subprocess.run(cwd=…)`)
        # the agent's shell lands in an unrelated directory — in one
        # measured run it landed in the harness's own parent, and the
        # agent read the harness source, the 3.0.0 block fixture and
        # the real token counts. That is a contaminated measurement,
        # not a behavioural result.
        #
        # Launching through a shell with an explicit `cd` puts the
        # agent's working directory inside the fixture project. The
        # contamination detector below then proves it per cell.
        cmd = [
            OPENCODE_BIN, "run", "--standalone", "--auto",
            "-m", MODEL, prompt,
        ]
        line = (
            "cd " + shlex.quote(str(project)) + " && "
            + " ".join(shlex.quote(c) for c in cmd)
        )
        try:
            proc = subprocess.run(
                line, shell=True, env=env, cwd=str(project),
                capture_output=True, text=True, timeout=timeout,
            )
        except subprocess.TimeoutExpired as exc:
            # A cell that times out is a MISS, not an abort. The
            # harness must still produce a full result table: a
            # comparison that silently drops the slow cells is a
            # comparison with a hole in it.
            measured = _measure(db_path)
            measured["stdout_tail"] = (exc.stdout or b"").decode(
                "utf-8", "replace",
            )[-600:] if isinstance(exc.stdout, bytes) else (exc.stdout or "")[-600:]
            measured["returncode"] = -1
            measured["stderr_tail"] = f"TIMEOUT after {timeout}s"
            measured["contaminated"] = False
            measured["contamination_hits"] = []
            measured["environment_reach"] = []
            return measured
        measured = _measure(db_path)
        measured["stdout_tail"] = proc.stdout.strip()[-600:]
        measured["returncode"] = proc.returncode
        measured["stderr_tail"] = proc.stderr.strip()[-600:]
        # CONTAMINATION DETECTOR — see the two-bucket note above.
        #
        # The scan covers the HOST's output AND the agent's full
        # transcript (its tool calls and their outputs). Scanning only
        # the host's output has a blind spot: an agent that READS a
        # harness artefact through a shell command never mentions it
        # in its final message, so it would not be flagged. A
        # `contaminated: false` flag is only as good as this scan.
        transcript = _strip_legitimate(
            proc.stdout + proc.stderr + _scan_transcript(tmp),
            variant, block, stage,
        )
        # KEEP THE EVIDENCE.
        #
        # The contamination detector is only as good as the scan, and a
        # scan nobody can inspect is a scan nobody can trust. The
        # transcript of every cell is written next to the JSON
        # envelope so a reviewer can check the verdict by hand — and so
        # a wrong verdict can be diagnosed instead of re-run.
        if transcript_dir is not None:
            transcript_dir.mkdir(parents=True, exist_ok=True)
            (transcript_dir / f"{variant}-{scenario}.txt").write_text(
                transcript, encoding="utf-8", errors="replace",
            )
        blocking = [
            needle for needle in BLOCKING_MARKERS
            if needle in transcript
        ]
        for prefix in BLOCKING_PREFIXES:
            if prefix in transcript:
                blocking.append(f"HARNESS-REPO:{prefix}")
        # The real home: noted, not blocking.
        real_home = str(Path.home())
        reach = sorted({
            path for path in _ABSOLUTE_PATH_RE.findall(transcript)
            if path.startswith(real_home)
        })
        measured["contaminated"] = bool(blocking)
        measured["contamination_hits"] = sorted(set(blocking))
        measured["environment_reach"] = reach[:12]
        return measured
    finally:
        shutil.rmtree(tmp, ignore_errors=True)


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--json")
    ap.add_argument("--scenarios", nargs="*")
    ap.add_argument("--variants", nargs="*")
    ap.add_argument(
        "--timeout", type=int, default=420,
        help="per-cell wall-clock budget in seconds (default 420)",
    )
    ap.add_argument(
        "--transcripts", default=None,
        help="directory to write each cell's transcript into, so the "
             "contamination verdict can be checked by hand",
    )
    args = ap.parse_args()

    transcript_dir = (
        Path(args.transcripts) if args.transcripts else None
    )

    mpm_binary = _mpm_binary()
    stage = Path(tempfile.mkdtemp(prefix="mpm-behav-stage-"))
    _stage_inputs(stage)
    variants = _variants(stage)
    scenarios = args.scenarios or list(SCENARIOS)
    which = args.variants or list(variants)

    # MERGE, DO NOT OVERWRITE.
    #
    # A resumed run must not destroy the cells it already completed.
    # The previous shape rewrote the whole file from this
    # invocation's in-memory list, so resuming with a subset of the
    # scenarios silently threw the other cells away — a data-loss
    # trap in a harness whose whole purpose is to produce a table.
    #
    # The merge is keyed on (variant, scenario): a re-run of a cell
    # replaces it; cells this invocation did not touch are kept.
    results: list[dict] = []
    if args.json and Path(args.json).is_file():
        try:
            existing = json.loads(Path(args.json).read_text(encoding="utf-8"))
            if isinstance(existing, list):
                results = [r for r in existing if isinstance(r, dict)]
        except (OSError, ValueError):
            results = []

    # The scenarios are the OUTER loop and the variants the inner one.
    # Running the two arms of a scenario back to back means a transient
    # provider slowdown, a rate limit or a cold cache lands on BOTH
    # arms of the same scenario rather than on every cell of one arm —
    # which is the difference between a controlled comparison and a
    # confounded one.
    for scenario in scenarios:
        for variant in which:
            print(f"=== {variant} / {scenario} ===", flush=True)
            measured = _run_one(
                variant, variants[variant], scenario,
                SCENARIOS[scenario], mpm_binary, timeout=args.timeout,
                transcript_dir=transcript_dir, stage=stage,
            )
            row = {
                "variant": variant,
                "scenario": scenario,
                "memories": measured["memories"],
                "decisions": measured["decisions"],
                "theories": measured["theories"],
                "lessons": measured["lessons"],
                "handoffs": measured["handoffs"],
                "works": measured["works"],
                "directives": measured.get("directives", 0),
                "excerpts": measured["excerpts"],
                "stdout_tail": measured.get("stdout_tail", ""),
                "stderr_tail": measured.get("stderr_tail", ""),
                "returncode": measured.get("returncode", -1),
                "contaminated": measured.get("contaminated", False),
                "contamination_hits": measured.get("contamination_hits", []),
                "environment_reach": measured.get("environment_reach", []),
            }
            # Upsert: a re-run of a cell replaces the earlier one.
            results = [
                r for r in results
                if not (r.get("variant") == row["variant"]
                        and r.get("scenario") == row["scenario"])
            ]
            results.append(row)
            print(
                f"    memories={row['memories']} decisions={row['decisions']} "
                f"theories={row['theories']} lessons={row['lessons']} "
                f"handoffs={row['handoffs']} works={row['works']} "
                f"(directives={row['directives']})"
                + ("  **CONTAMINATED**" if row["contaminated"] else "")
                + (f"  {row['contamination_hits']}"
                   if row["contamination_hits"] else ""),
                flush=True,
            )
            for ex in row["excerpts"]:
                print(f"      [{ex['collection']}] {ex['content'][:200]}",
                      flush=True)
            # Write the partial result after EVERY cell, so a harness
            # that is interrupted still leaves a usable artefact
            # rather than nothing.
            if args.json:
                Path(args.json).write_text(
                    json.dumps(results, indent=2), encoding="utf-8",
                )

    if args.json:
        Path(args.json).write_text(json.dumps(results, indent=2), encoding="utf-8")
        print(f"\nartifacts: {args.json}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
