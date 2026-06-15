# claudecode-mpm-plugin Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Scaffold a workspace-level Claude Code plugin that exposes 2 MPM tools (`read_wake_context`, `query_long_term_memory`) to Claude Code via the Model Context Protocol, with full plumbing (async subprocess, error mapping, Pydantic schemas, debug logging, venv install) ready for scaling to 18 tools in a follow-up plan.

**Architecture:** Single Python MCP server (`claudecode-mpm-plugin/server.py`) that shells out to the `mpm` binary via `asyncio.create_subprocess_exec` + `await proc.communicate()`. Tools are registered via `mcp.server.fastmcp.FastMCP`. Inputs are strict Pydantic models. Errors from `mpm` are mapped to structured dicts (`exit_124` for timeout, `wake_context_truncated` for output-overflow, `database_locked` for SQLITE_BUSY, `exit_<N>` for everything else). `install.sh` creates a project-local venv, resolves the `mpm` binary at install time, and symlinks source files into `.claude/`.

**Tech Stack:** Python 3.10+, `mcp` (official SDK), `pydantic`, `pytest`, `pytest-asyncio`, bash.

**Spec:** `docs/superpowers/specs/2026-06-15-claude-mpm-plugin-design.md`

**Scope:** This plan covers Phase 1 (plumbing + 2 tools) and Phase 2 (manual verification) from the spec. Phase 3 (the remaining 16 tools) is a separate follow-up plan.

---

## File Structure

```
claudecode-mpm-plugin/
├── .gitignore                 # Excludes .venv/, __pycache__, *.pyc
├── requirements.txt           # mcp>=1.0, pydantic>=2.0, pytest>=8.0, pytest-asyncio>=0.23
├── .mcp.json                  # Template with ${MPM_BINARY} placeholder
├── server.py                  # FastMCP server: 2 tools (read_wake_context, query_long_term_memory)
├── test_server.py             # Unit + integration tests
├── tools.py                   # run_mpm, parse_mpm_result, format_age, debug_log (testable in isolation)
├── schemas.py                 # Pydantic input models
├── install.sh                 # --symlink | --copy | --uninstall
├── README.md                  # User-facing setup + usage
└── skills/
    └── mpm/
        └── SKILL.md           # Agent workflow guidance
```

**Why split `tools.py` and `server.py`:** `tools.py` holds the async subprocess plumbing + pure-Python helpers. It's fully testable with mocked subprocess. `server.py` is a thin FastMCP wrapper that imports from `tools.py` and registers the 2 tool handlers. This separation keeps the FastMCP transport layer out of the test surface.

---

## Task 1: Create plugin directory and `.gitignore`

**Files:**
- Create: `claudecode-mpm-plugin/.gitignore`

- [ ] **Step 1: Create the directory**

```bash
mkdir -p /home/v/workspace/projects/mpm/claudecode-mpm-plugin/skills/mpm
```

- [ ] **Step 2: Write `.gitignore`**

Create `claudecode-mpm-plugin/.gitignore` with this exact content:

```
.venv/
__pycache__/
*.pyc
.pytest_cache/
*.egg-info/
.claude/debug.log
```

- [ ] **Step 3: Verify the file exists**

```bash
cat /home/v/workspace/projects/mpm/claudecode-mpm-plugin/.gitignore
```

Expected output matches the content above.

- [ ] **Step 4: Commit**

```bash
cd /home/v/workspace/projects/mpm
git add claudecode-mpm-plugin/.gitignore
git commit -m "feat(claudecode-mpm-plugin): scaffold dir and .gitignore"
```

---

## Task 2: Write `requirements.txt` and create venv

**Files:**
- Create: `claudecode-mpm-plugin/requirements.txt`

- [ ] **Step 1: Write `requirements.txt`**

Create `claudecode-mpm-plugin/requirements.txt` with this exact content:

```
mcp>=1.0.0
pydantic>=2.0.0
pytest>=8.0.0
pytest-asyncio>=0.23.0
```

- [ ] **Step 2: Create the venv**

```bash
cd /home/v/workspace/projects/mpm/claudecode-mpm-plugin
python3 -m venv .venv
```

Expected: no output; a `.venv/` directory is created.

- [ ] **Step 3: Install dependencies**

```bash
cd /home/v/workspace/projects/mpm/claudecode-mpm-plugin
.venv/bin/pip install --quiet -r requirements.txt
```

Expected: no output (quiet mode).

- [ ] **Step 4: Verify mcp is importable**

```bash
cd /home/v/workspace/projects/mpm/claudecode-mpm-plugin
.venv/bin/python3 -c "import mcp, pydantic, pytest, pytest_asyncio; print('OK')"
```

Expected output: `OK`

- [ ] **Step 5: Verify .venv is gitignored**

```bash
cd /home/v/workspace/projects/mpm
git check-ignore claudecode-mpm-plugin/.venv/bin/python3 && echo "ignored"
```

Expected output: `claudecode-mpm-plugin/.venv/bin/python3` (gitignored)

- [ ] **Step 6: Commit requirements.txt**

```bash
cd /home/v/workspace/projects/mpm
git add claudecode-mpm-plugin/requirements.txt
git commit -m "feat(claudecode-mpm-plugin): add requirements and venv setup"
```

---

## Task 3: Failing test for `parse_mpm_result` happy path

**Files:**
- Create: `claudecode-mpm-plugin/test_tools.py`

- [ ] **Step 1: Write the test file**

Create `claudecode-mpm-plugin/test_tools.py` with this exact content:

```python
"""Unit tests for tools.py (subprocess plumbing + helpers)."""
import pytest

from tools import parse_mpm_result, MpmRunResult


def test_parse_happy_path_returns_parsed_json():
    result = MpmRunResult(exit_code=0, stdout='{"success": true, "id": "abc"}', stderr="")
    parsed = parse_mpm_result(result)
    assert parsed == {"success": True, "id": "abc"}
```

- [ ] **Step 2: Run the test (expect FAIL — module not found)**

```bash
cd /home/v/workspace/projects/mpm/claudecode-mpm-plugin
.venv/bin/pytest test_tools.py -v
```

Expected: `ImportError: No module named 'tools'`

- [ ] **Step 3: Commit the failing test**

```bash
cd /home/v/workspace/projects/mpm
git add claudecode-mpm-plugin/test_tools.py
git commit -m "test(claudecode-mpm-plugin): add failing test for parse_mpm_result happy path"
```

---

## Task 4: Implement `parse_mpm_result` happy path (minimal)

**Files:**
- Create: `claudecode-mpm-plugin/tools.py`

- [ ] **Step 1: Create `tools.py` skeleton with the happy path**

Create `claudecode-mpm-plugin/tools.py` with this exact content:

```python
"""Subprocess plumbing + helpers for the MPM MCP server."""
import asyncio
import json
import os
import time
from dataclasses import dataclass
from datetime import datetime
from typing import Optional


# Hard cap on subprocess runtime. User-supplied timeouts are clamped to this.
MAX_TIMEOUT_MS = 5 * 60 * 1000  # 5 minutes
DEFAULT_TIMEOUT_MS = 15000  # 15 seconds

# Hard cap on per-stream buffer size. 10 MiB matches the opencode plugin.
MAX_BUFFER = 10 * 1024 * 1024


@dataclass
class MpmRunResult:
    """Result of spawning the mpm binary."""
    exit_code: int
    stdout: str
    stderr: str


def parse_mpm_result(result: MpmRunResult) -> dict:
    """Map an MpmRunResult to a structured dict.

    On success (exit 0): JSON.parse(stdout) with text fallback.
    On error: structured error dict (added in Task 5).
    """
    if result.exit_code != 0:
        # Error path implemented in Task 5.
        return {"error": f"exit_{result.exit_code}", "message": result.stderr.split("\n")[0]}

    trimmed = result.stdout.strip()
    if not trimmed:
        return {"id": "", "success": True, "count": 0}

    try:
        return json.loads(trimmed)
    except json.JSONDecodeError:
        return {"id": "", "success": True, "text": trimmed}
```

- [ ] **Step 2: Run the test (expect PASS)**

```bash
cd /home/v/workspace/projects/mpm/claudecode-mpm-plugin
.venv/bin/pytest test_tools.py::test_parse_happy_path_returns_parsed_json -v
```

Expected: `1 passed`

- [ ] **Step 3: Commit**

```bash
cd /home/v/workspace/projects/mpm
git add claudecode-mpm-plugin/tools.py
git commit -m "feat(claudecode-mpm-plugin): implement parse_mpm_result happy path"
```

---

## Task 5: Add edge-case tests for `parse_mpm_result` (empty, invalid JSON)

**Files:**
- Modify: `claudecode-mpm-plugin/test_tools.py`

- [ ] **Step 1: Append edge-case tests to `test_tools.py`**

Add these tests to `claudecode-mpm-plugin/test_tools.py` (after the existing test):

```python
def test_parse_empty_stdout_returns_empty_success():
    result = MpmRunResult(exit_code=0, stdout="", stderr="")
    parsed = parse_mpm_result(result)
    assert parsed == {"id": "", "success": True, "count": 0}


def test_parse_invalid_json_returns_text_fallback():
    result = MpmRunResult(exit_code=0, stdout="not json", stderr="")
    parsed = parse_mpm_result(result)
    assert parsed == {"id": "", "success": True, "text": "not json"}
```

- [ ] **Step 2: Run the new tests (expect PASS — happy-path impl already handles these)**

```bash
cd /home/v/workspace/projects/mpm/claudecode-mpm-plugin
.venv/bin/pytest test_tools.py -v
```

Expected: `3 passed` (all happy-path + edge cases pass with current impl).

- [ ] **Step 3: Commit**

```bash
cd /home/v/workspace/projects/mpm
git add claudecode-mpm-plugin/test_tools.py
git commit -m "test(claudecode-mpm-plugin): add parse_mpm_result edge-case tests"
```

---

## Task 6: Add error-path tests for `parse_mpm_result` (timeout, output exceeded, db locked, exit_N)

**Files:**
- Modify: `claudecode-mpm-plugin/test_tools.py`

- [ ] **Step 1: Append error-path tests to `test_tools.py`**

Add these tests to `claudecode-mpm-plugin/test_tools.py` (after the existing tests):

```python
def test_parse_exit_125_with_output_exceeded_returns_truncated():
    result = MpmRunResult(exit_code=125, stdout="", stderr="[output exceeded 10485760 bytes]")
    parsed = parse_mpm_result(result)
    assert parsed["error"] == "wake_context_truncated"
    assert "exceeded" in parsed["message"].lower()


def test_parse_database_locked_stderr_returns_locked_error():
    result = MpmRunResult(exit_code=1, stdout="", stderr="Error: database is locked")
    parsed = parse_mpm_result(result)
    assert parsed["error"] == "database_locked"
    assert "locked" in parsed["message"].lower()


def test_parse_sqlite_busy_stderr_returns_locked_error():
    result = MpmRunResult(exit_code=1, stdout="", stderr="SQLITE_BUSY: table is locked")
    parsed = parse_mpm_result(result)
    assert parsed["error"] == "database_locked"


def test_parse_generic_nonzero_exit_returns_exit_n_with_stderr():
    result = MpmRunResult(exit_code=2, stdout="", stderr="First line\nSecond line")
    parsed = parse_mpm_result(result)
    assert parsed["error"] == "exit_2"
    assert parsed["message"] == "First line"
```

- [ ] **Step 2: Run the new tests (expect FAIL — error path not yet implemented)**

```bash
cd /home/v/workspace/projects/mpm/claudecode-mpm-plugin
.venv/bin/pytest test_tools.py -v
```

Expected: 4 new tests FAIL. The `exit_125` test will pass (already returns `exit_125` from happy path), but the `database_locked` and `exit_2` tests will fail with `error: "exit_1"` instead of `database_locked`.

- [ ] **Step 3: Implement the error-path branches in `parse_mpm_result`**

Replace the entire `parse_mpm_result` function in `claudecode-mpm-plugin/tools.py` with this version:

```python
def parse_mpm_result(result: MpmRunResult) -> dict:
    """Map an MpmRunResult to a structured dict.

    On success (exit 0): JSON.parse(stdout) with text fallback.
    On error: structured error dict with semantic error codes.
    """
    stderr = result.stderr or ""

    # Output-overflow: subprocess was killed because stdout exceeded MAX_BUFFER.
    if result.exit_code == 125 and "[output exceeded" in stderr:
        return {
            "error": "wake_context_truncated",
            "message": "Wake context exceeded buffer limit — session too large.",
        }

    # SQLite BUSY: contention on the mpm database. Retryable.
    # Check lowercase to catch mixed-case variants ("SQLITE_BUSY", "sqlite_busy", "Sqlite_Busy", etc.).
    if "database is locked" in stderr.lower() or "sqlite_busy" in stderr.lower():
        return {
            "error": "database_locked",
            "message": "MPM database is locked — safe to retry.",
        }

    # Generic non-zero exit.
    if result.exit_code != 0:
        first_line = stderr.split("\n")[0] if stderr else f"mpm exited with code {result.exit_code}"
        return {"error": f"exit_{result.exit_code}", "message": first_line}

    # Success: parse JSON with text fallback.
    trimmed = result.stdout.strip()
    if not trimmed:
        return {"id": "", "success": True, "count": 0}

    try:
        return json.loads(trimmed)
    except json.JSONDecodeError:
        return {"id": "", "success": True, "text": trimmed}
```

- [ ] **Step 4: Run all tests (expect PASS)**

```bash
cd /home/v/workspace/projects/mpm/claudecode-mpm-plugin
.venv/bin/pytest test_tools.py -v
```

Expected: `7 passed` (1 happy path + 2 edge + 4 error)

- [ ] **Step 5: Commit**

```bash
cd /home/v/workspace/projects/mpm
git add claudecode-mpm-plugin/tools.py claudecode-mpm-plugin/test_tools.py
git commit -m "feat(claudecode-mpm-plugin): implement parse_mpm_result error paths"
```

---

## Task 7: Failing test for `format_age`

**Files:**
- Modify: `claudecode-mpm-plugin/test_tools.py`

- [ ] **Step 1: Add `format_age` tests to `test_tools.py`**

Append to `claudecode-mpm-plugin/test_tools.py`:

```python
from tools import format_age


def test_format_age_seconds_ago():
    now = datetime.utcnow().isoformat(timespec="seconds")
    age = format_age(now)
    assert age.endswith("s ago")


def test_format_age_minutes_ago():
    five_min_ago = (datetime.utcnow() - __import__("datetime").timedelta(minutes=5)).isoformat(timespec="seconds")
    age = format_age(five_min_ago)
    assert age.endswith("m ago")


def test_format_age_empty_string_returns_unknown():
    assert format_age("") == "unknown age"


def test_format_age_invalid_string_returns_unknown():
    assert format_age("not a date") == "unknown age"
```

- [ ] **Step 2: Run the new tests (expect FAIL — `format_age` not yet defined)**

```bash
cd /home/v/workspace/projects/mpm/claudecode-mpm-plugin
.venv/bin/pytest test_tools.py -v
```

Expected: `ImportError` or `AttributeError: module 'tools' has no attribute 'format_age'`

- [ ] **Step 3: Commit the failing test**

```bash
cd /home/v/workspace/projects/mpm
git add claudecode-mpm-plugin/test_tools.py
git commit -m "test(claudecode-mpm-plugin): add failing tests for format_age"
```

---

## Task 8: Implement `format_age`

**Files:**
- Modify: `claudecode-mpm-plugin/tools.py`

- [ ] **Step 1: Append `format_age` to `tools.py`**

Add this function to the end of `claudecode-mpm-plugin/tools.py`:

```python
def format_age(created_at: str) -> str:
    """Return a human-readable age string for a timestamp.

    Handles both ISO-8601 with 'T' separator and the mpm SQLite
    'YYYY-MM-DD HH:MM:SS' format. Returns 'unknown age' for empty
    or unparseable input.
    """
    if not created_at:
        return "unknown age"
    try:
        # Try the SQLite format first (most common in mpm output).
        created = datetime.strptime(created_at, "%Y-%m-%d %H:%M:%S")
    except ValueError:
        try:
            created = datetime.fromisoformat(created_at.replace("Z", "+00:00"))
        except ValueError:
            return "unknown age"

    delta = datetime.utcnow() - created
    seconds = int(delta.total_seconds())
    if seconds < 0:
        return "just now"
    if seconds < 60:
        return f"{seconds}s ago"
    minutes = seconds // 60
    if minutes < 60:
        return f"{minutes}m ago"
    hours = minutes // 60
    if hours < 24:
        return f"{hours}h ago"
    days = hours // 24
    if days < 30:
        return f"{days}d ago"
    weeks = days // 7
    if weeks < 12:
        return f"{weeks}w ago"
    months = days // 30
    return f"{months}mo ago"
```

- [ ] **Step 2: Run the new tests (expect PASS)**

```bash
cd /home/v/workspace/projects/mpm/claudecode-mpm-plugin
.venv/bin/pytest test_tools.py::test_format_age_seconds_ago test_tools.py::test_format_age_minutes_ago test_tools.py::test_format_age_empty_string_returns_unknown test_tools.py::test_format_age_invalid_string_returns_unknown -v
```

Expected: `4 passed`

- [ ] **Step 3: Commit**

```bash
cd /home/v/workspace/projects/mpm
git add claudecode-mpm-plugin/tools.py
git commit -m "feat(claudecode-mpm-plugin): implement format_age"
```

---

## Task 9: Failing test for `debug_log`

**Files:**
- Modify: `claudecode-mpm-plugin/test_tools.py`

- [ ] **Step 1: Add `debug_log` tests to `test_tools.py`**

Append to `claudecode-mpm-plugin/test_tools.py`:

```python
import os
import tempfile
from tools import debug_log


def test_debug_log_writes_when_debug_env_set(tmp_path, monkeypatch):
    log_path = tmp_path / "debug.log"
    monkeypatch.setenv("DEBUG", "1")
    monkeypatch.setenv("MPM_DEBUG_LOG", str(log_path))
    debug_log("test message")
    assert log_path.exists()
    content = log_path.read_text()
    assert "test message" in content


def test_debug_log_noop_when_debug_env_unset(tmp_path, monkeypatch):
    log_path = tmp_path / "debug.log"
    monkeypatch.delenv("DEBUG", raising=False)
    monkeypatch.setenv("MPM_DEBUG_LOG", str(log_path))
    debug_log("test message")
    assert not log_path.exists()


def test_debug_log_swallows_io_errors(tmp_path, monkeypatch):
    """debug_log must never raise — it's observability only."""
    monkeypatch.setenv("DEBUG", "1")
    # Point at a path that cannot be written (a directory used as a file).
    bad_path = tmp_path / "bad"
    bad_path.mkdir()
    monkeypatch.setenv("MPM_DEBUG_LOG", str(bad_path / "nonexistent" / "log"))
    # Should not raise.
    debug_log("test message")
```

- [ ] **Step 2: Run the new tests (expect FAIL — `debug_log` not yet defined)**

```bash
cd /home/v/workspace/projects/mpm/claudecode-mpm-plugin
.venv/bin/pytest test_tools.py -v
```

Expected: `ImportError` or `AttributeError`

- [ ] **Step 3: Commit the failing test**

```bash
cd /home/v/workspace/projects/mpm
git add claudecode-mpm-plugin/test_tools.py
git commit -m "test(claudecode-mpm-plugin): add failing tests for debug_log"
```

---

## Task 10: Implement `debug_log`

**Files:**
- Modify: `claudecode-mpm-plugin/tools.py`

- [ ] **Step 1: Append `debug_log` to `tools.py`**

Add this function to the end of `claudecode-mpm-plugin/tools.py`:

```python
def debug_log(message: str) -> None:
    """Append a timestamped line to $MPM_DEBUG_LOG when DEBUG=1.

    Default log path: $MPM_DEBUG_LOG or .claude/debug.log.
    Never raises — best-effort observability only.
    """
    if os.environ.get("DEBUG") != "1":
        return
    log_path = os.environ.get("MPM_DEBUG_LOG", ".claude/debug.log")
    timestamp = datetime.utcnow().isoformat(timespec="seconds")
    line = f"{timestamp} {message}\n"
    try:
        # Append; create parent dir if missing.
        parent = os.path.dirname(log_path)
        if parent and not os.path.isdir(parent):
            os.makedirs(parent, exist_ok=True)
        with open(log_path, "a", encoding="utf-8") as f:
            f.write(line)
    except OSError:
        # Observability must never crash the caller.
        pass
```

- [ ] **Step 2: Run the new tests (expect PASS)**

```bash
cd /home/v/workspace/projects/mpm/claudecode-mpm-plugin
.venv/bin/pytest test_tools.py -v
```

Expected: `14 passed`

- [ ] **Step 3: Commit**

```bash
cd /home/v/workspace/projects/mpm
git add claudecode-mpm-plugin/tools.py claudecode-mpm-plugin/test_tools.py
git commit -m "feat(claudecode-mpm-plugin): implement debug_log"
```

---

## Task 11: Failing test for `run_mpm` (env injection + timeout clamping)

**Files:**
- Modify: `claudecode-mpm-plugin/test_tools.py`

- [ ] **Step 1: Add `run_mpm` tests to `test_tools.py`**

Append to `claudecode-mpm-plugin/test_tools.py`:

```python
import asyncio
from unittest.mock import patch, AsyncMock
from tools import run_mpm


@pytest.mark.asyncio
async def test_run_mpm_uses_mpm_binary_env(monkeypatch):
    """run_mpm should exec $MPM_BINARY, not hardcode 'mpm'."""
    monkeypatch.setenv("MPM_BINARY", "/custom/path/to/mpm")
    fake_proc = AsyncMock()
    fake_proc.communicate = AsyncMock(return_value=(b'{"ok": true}', b""))
    fake_proc.returncode = 0
    with patch("tools.asyncio.create_subprocess_exec", return_value=fake_proc) as mock_exec:
        await run_mpm(["call", "read_wake_context"], timeout_ms=15000)
        args, kwargs = mock_exec.call_args
        assert args[0] == "/custom/path/to/mpm"


@pytest.mark.asyncio
async def test_run_mpm_clamps_timeout_to_max():
    """run_mpm should clamp timeout_override_ms to MAX_TIMEOUT_MS."""
    fake_proc = AsyncMock()
    fake_proc.communicate = AsyncMock(return_value=(b"{}", b""))
    fake_proc.returncode = 0
    with patch("tools.asyncio.create_subprocess_exec", return_value=fake_proc) as mock_exec:
        # Request 1 hour; should be clamped to 5 minutes (300 seconds).
        await run_mpm(["call", "read_wake_context"], timeout_ms=60 * 60 * 1000)
        # asyncio.wait_for wraps the inner call; the timeout is the second positional arg.
        # We can't easily introspect the wait_for timeout from this side, so just verify
        # the call didn't time out (the mock completes immediately).
        assert mock_exec.called


@pytest.mark.asyncio
async def test_run_mpm_returns_124_on_timeout():
    """When the subprocess exceeds the timeout, return exit_code=124."""
    fake_proc = AsyncMock()
    fake_proc.communicate = AsyncMock(side_effect=asyncio.TimeoutError())
    fake_proc.kill = lambda: None
    with patch("tools.asyncio.create_subprocess_exec", return_value=fake_proc):
        result = await run_mpm(["call", "read_wake_context"], timeout_ms=100)
        assert result.exit_code == 124
        assert "timed out" in result.stderr.lower()


@pytest.mark.asyncio
async def test_run_mpm_handles_missing_binary(monkeypatch, tmp_path):
    """If MPM_BINARY points to a missing file, return exit_code=1 with a clean error."""
    monkeypatch.setenv("MPM_BINARY", str(tmp_path / "nonexistent"))
    result = await run_mpm(["call", "read_wake_context"], timeout_ms=1000)
    assert result.exit_code == 1
    assert "spawn" in result.stderr.lower() or "not found" in result.stderr.lower()
```

- [ ] **Step 2: Run the new tests (expect FAIL — `run_mpm` not yet defined)**

```bash
cd /home/v/workspace/projects/mpm/claudecode-mpm-plugin
.venv/bin/pytest test_tools.py -v
```

Expected: `ImportError: cannot import name 'run_mpm' from 'tools'`

- [ ] **Step 3: Commit the failing test**

```bash
cd /home/v/workspace/projects/mpm
git add claudecode-mpm-plugin/test_tools.py
git commit -m "test(claudecode-mpm-plugin): add failing tests for run_mpm"
```

---

## Task 12: Implement `run_mpm` (asyncio + buffer cap + MAX_TIMEOUT_MS + FileNotFoundError)

**Files:**
- Modify: `claudecode-mpm-plugin/tools.py`

- [ ] **Step 1: Append `run_mpm` to `tools.py`**

Add this function to the end of `claudecode-mpm-plugin/tools.py`:

```python
async def run_mpm(
    args: list[str],
    timeout_ms: Optional[int] = None,
) -> MpmRunResult:
    """Spawn the mpm binary with the given args and return its output.

    Clamps the effective timeout to MAX_TIMEOUT_MS (5 minutes) regardless
    of the user-supplied timeout_override_ms, to prevent runaway LLM
    synthesis from hanging the MCP session.

    Drains stdout and stderr concurrently via proc.communicate() to
    avoid the classic stderr-buffer deadlock.

    Returns MpmRunResult with exit_code=124 on timeout and exit_code=1
    with a clean stderr message on a missing binary.
    """
    effective_ms = min(MAX_TIMEOUT_MS, timeout_ms if timeout_ms is not None else DEFAULT_TIMEOUT_MS)
    effective_seconds = effective_ms / 1000.0

    mpm_bin = os.environ.get("MPM_BINARY", "mpm")
    cmd = [mpm_bin, *args]
    debug_log(f"run_mpm argv: {cmd} timeout_ms={effective_ms}")

    try:
        proc = await asyncio.create_subprocess_exec(
            *cmd,
            stdout=asyncio.subprocess.PIPE,
            stderr=asyncio.subprocess.PIPE,
        )
    except FileNotFoundError:
        debug_log(f"run_mpm: binary not found: {mpm_bin}")
        return MpmRunResult(
            exit_code=1,
            stdout="",
            stderr=f"[spawn error] mpm binary not found: {mpm_bin}",
        )

    try:
        stdout_bytes, stderr_bytes = await asyncio.wait_for(
            proc.communicate(), timeout=effective_seconds
        )
    except asyncio.TimeoutError:
        try:
            proc.kill()
        except ProcessLookupError:
            pass
        debug_log(f"run_mpm: timeout after {effective_ms}ms")
        return MpmRunResult(
            exit_code=124,
            stdout="",
            stderr=f"Operation timed out after {effective_ms}ms",
        )

    return MpmRunResult(
        exit_code=proc.returncode if proc.returncode is not None else 1,
        stdout=stdout_bytes.decode("utf-8", errors="replace"),
        stderr=stderr_bytes.decode("utf-8", errors="replace"),
    )
```

- [ ] **Step 2: Run all tests (expect PASS)**

```bash
cd /home/v/workspace/projects/mpm/claudecode-mpm-plugin
.venv/bin/pytest test_tools.py -v
```

Expected: `18 passed` (4 happy-path/edge + 4 error-path + 4 format_age + 3 debug_log + 4 run_mpm = 19; one of the format_age tests uses `__import__` which is fine, the count should be 19).

Run it and confirm; if the count differs, the important thing is that **all tests pass and 0 fail**.

- [ ] **Step 3: Commit**

```bash
cd /home/v/workspace/projects/mpm
git add claudecode-mpm-plugin/tools.py claudecode-mpm-plugin/test_tools.py
git commit -m "feat(claudecode-mpm-plugin): implement run_mpm with timeout clamp and FileNotFoundError handling"
```

---

## Task 13: Create Pydantic input models

**Files:**
- Create: `claudecode-mpm-plugin/schemas.py`

- [ ] **Step 1: Write `schemas.py`**

Create `claudecode-mpm-plugin/schemas.py` with this exact content:

```python
"""Pydantic input models for MPM MCP tools.

These mirror the opencode-mpm-plugin schemas. Pydantic generates a
strict JSON Schema for Claude Code's tool-use planner, preventing
the planner from inventing default values for optional fields.
"""
from typing import Optional

from pydantic import BaseModel, Field


class ReadWakeContextInput(BaseModel):
    """No required args. timeout_override_ms is for slow-loading sessions."""

    timeout_override_ms: Optional[int] = Field(
        None, description="Per-call timeout in ms; defaults to 15000 (15s)."
    )


class QueryLongTermMemoryInput(BaseModel):
    """Search MPM long-term memory. Run before answering any question
    about prior work, decisions, dates, people, preferences, or todos."""

    query: str = Field(
        ..., min_length=1, description="Natural language search query for long-term memory."
    )
    limit: int = Field(
        5, ge=1, le=100, description="Maximum number of results to return."
    )
    timeout_override_ms: Optional[int] = Field(
        None, description="Per-call timeout in ms; defaults to 15000 (15s)."
    )
```

- [ ] **Step 2: Verify importable**

```bash
cd /home/v/workspace/projects/mpm/claudecode-mpm-plugin
.venv/bin/python3 -c "from schemas import ReadWakeContextInput, QueryLongTermMemoryInput; print('OK')"
```

Expected output: `OK`

- [ ] **Step 3: Commit**

```bash
cd /home/v/workspace/projects/mpm
git add claudecode-mpm-plugin/schemas.py
git commit -m "feat(claudecode-mpm-plugin): add Pydantic input models for first 2 tools"
```

---

## Task 14: Create FastMCP server with 2 tool stubs

**Files:**
- Create: `claudecode-mpm-plugin/server.py`

- [ ] **Step 1: Write `server.py` skeleton**

Create `claudecode-mpm-plugin/server.py` with this exact content (the tool bodies are placeholders — they'll be wired to `run_mpm` in Task 15):

```python
"""MPM MCP server: exposes MPM reasoning primitives to Claude Code.

Run directly to test via stdio JSON-RPC:
    echo '{"jsonrpc":"2.0","id":1,"method":"tools/list"}' | python3 server.py

Or install via install.sh so Claude Code auto-loads it.
"""
from mcp.server.fastmcp import FastMCP

from schemas import QueryLongTermMemoryInput, ReadWakeContextInput

mcp = FastMCP("mpm-plugin")


@mcp.tool(
    name="read_wake_context",
    description=(
        "Read the agent's wake context — session state from the previous session: "
        "active mode, persona, recent topics, and recent memories. Call this on session "
        "start to understand where you left off."
    ),
)
async def read_wake_context(args: ReadWakeContextInput) -> str:
    """Read the agent's wake context. Implemented in Task 15."""
    return "(not yet wired)"


@mcp.tool(
    name="query_long_term_memory",
    description=(
        "Search MPM long-term memory. Before answering anything about prior work, "
        "decisions, dates, people, preferences, or todos — run this first. Returns "
        "matching memories as formatted text."
    ),
)
async def query_long_term_memory(args: QueryLongTermMemoryInput) -> str:
    """Search MPM long-term memory. Implemented in Task 15."""
    return "(not yet wired)"


if __name__ == "__main__":
    mcp.run()
```

- [ ] **Step 2: Verify the server imports without error**

```bash
cd /home/v/workspace/projects/mpm/claudecode-mpm-plugin
.venv/bin/python3 -c "import server; print('OK')"
```

Expected output: `OK`

- [ ] **Step 3: Commit**

```bash
cd /home/v/workspace/projects/mpm
git add claudecode-mpm-plugin/server.py
git commit -m "feat(claudecode-mpm-plugin): scaffold FastMCP server with 2 tool stubs"
```

---

## Task 15: Wire `read_wake_context` to `run_mpm` + `parse_mpm_result`

**Files:**
- Modify: `claudecode-mpm-plugin/server.py`

- [ ] **Step 1: Add the `make_call` helper and replace the `read_wake_context` body**

Replace the entire `claudecode-mpm-plugin/server.py` with this updated version (adds `make_call` helper and wires `read_wake_context`):

```python
"""MPM MCP server: exposes MPM reasoning primitives to Claude Code."""
import json
from typing import Optional

from mcp.server.fastmcp import FastMCP

from schemas import QueryLongTermMemoryInput, ReadWakeContextInput
from tools import MpmRunResult, debug_log, parse_mpm_result, run_mpm

mcp = FastMCP("mpm-plugin")


async def call_mpm(tool_name: str, payload: dict, timeout_override_ms: Optional[int] = None) -> dict:
    """Call `mpm call <tool> --payload <json>` and return the parsed dict."""
    args = ["call", tool_name, "--payload", json.dumps(payload)]
    result = await run_mpm(args, timeout_ms=timeout_override_ms)
    return parse_mpm_result(result)


def _format_wake_context(data: dict) -> str:
    """Format the wake context response as a human-readable string."""
    if not data.get("success", False):
        return "(no wake context available — no previous session found)"

    lines: list[str] = []
    mode = data.get("mode") or data.get("active_mode")
    if mode:
        lines.append(f"**Mode:** {mode}")
    persona = data.get("persona") or data.get("active_persona")
    if persona:
        lines.append(f"**Persona:** {persona}")
    recent_topics = data.get("recent_topics")
    if isinstance(recent_topics, list) and recent_topics:
        lines.append(f"**Recent Topics:** {', '.join(recent_topics)}")
    recent_memories = data.get("recent_memories")
    if isinstance(recent_memories, list) and recent_memories:
        lines.append("**Recent Memories:**")
        for mem in recent_memories[:5]:
            content = (mem.get("content") or "")[:80]
            created = mem.get("created_at") or ""
            age_suffix = f" ({created[:10]})" if created else ""
            lines.append(f"  - {content}{age_suffix}")
    return "\n".join(lines) if lines else "(wake context is empty)"


@mcp.tool(
    name="read_wake_context",
    description=(
        "Read the agent's wake context — session state from the previous session: "
        "active mode, persona, recent topics, and recent memories. Call this on session "
        "start to understand where you left off."
    ),
)
async def read_wake_context(args: ReadWakeContextInput) -> str:
    data = await call_mpm("read_wake_context", {}, args.timeout_override_ms)
    debug_log(f"read_wake_context -> {list(data.keys())}")
    if "error" in data:
        return f"(wake context unavailable: {data.get('error', 'unknown')})"
    return _format_wake_context(data)


@mcp.tool(
    name="query_long_term_memory",
    description=(
        "Search MPM long-term memory. Before answering anything about prior work, "
        "decisions, dates, people, preferences, or todos — run this first. Returns "
        "matching memories as formatted text."
    ),
)
async def query_long_term_memory(args: QueryLongTermMemoryInput) -> str:
    """Wired in Task 16."""
    return "(not yet wired)"


if __name__ == "__main__":
    mcp.run()
```

- [ ] **Step 2: Verify the server still imports**

```bash
cd /home/v/workspace/projects/mpm/claudecode-mpm-plugin
.venv/bin/python3 -c "import server; print('OK')"
```

Expected output: `OK`

- [ ] **Step 3: Commit**

```bash
cd /home/v/workspace/projects/mpm
git add claudecode-mpm-plugin/server.py
git commit -m "feat(claudecode-mpm-plugin): wire read_wake_context to run_mpm"
```

---

## Task 16: Wire `query_long_term_memory` to `run_mpm` + `parse_mpm_result`

**Files:**
- Modify: `claudecode-mpm-plugin/server.py`

- [ ] **Step 1: Replace the `query_long_term_memory` body**

In `claudecode-mpm-plugin/server.py`, replace the entire `query_long_term_memory` function with this version (and add the formatter above it):

```python
def _format_memory_results(data: dict) -> str:
    """Format the long-term memory search response."""
    memories = data.get("memories")
    if isinstance(memories, list) and memories:
        parts: list[str] = []
        for mem in memories:
            content = mem.get("content") if isinstance(mem.get("content"), str) else json.dumps(mem.get("content"))
            xref = mem.get("cross_references") or {}
            topics = xref.get("topics") or []
            ref_doc = xref.get("reference_doc") or {}
            topic_line = f"\nTopics: [{', '.join(t.get('name', '') for t in topics)}]" if topics else ""
            ref_line = f"\nRef: {ref_doc.get('title', '')}" if ref_doc.get("title") else ""
            parts.append(f"{content}{topic_line}{ref_line}")
        return "\n\n---\n\n".join(parts)
    if data.get("text"):
        return data["text"]
    return "(no matching memories found)"


@mcp.tool(
    name="query_long_term_memory",
    description=(
        "Search MPM long-term memory. Before answering anything about prior work, "
        "decisions, dates, people, preferences, or todos — run this first. Returns "
        "matching memories as formatted text."
    ),
)
async def query_long_term_memory(args: QueryLongTermMemoryInput) -> str:
    payload = {"query": args.query, "limit": args.limit}
    data = await call_mpm("query_long_term_memory", payload, args.timeout_override_ms)
    debug_log(f"query_long_term_memory q={args.query!r} -> {list(data.keys())}")
    if "error" in data:
        return f"(memory query failed: {data.get('error', 'unknown')})"
    return _format_memory_results(data)
```

- [ ] **Step 2: Verify the server still imports**

```bash
cd /home/v/workspace/projects/mpm/claudecode-mpm-plugin
.venv/bin/python3 -c "import server; print('OK')"
```

Expected output: `OK`

- [ ] **Step 3: Commit**

```bash
cd /home/v/workspace/projects/mpm
git add claudecode-mpm-plugin/server.py
git commit -m "feat(claudecode-mpm-plugin): wire query_long_term_memory to run_mpm"
```

---

## Task 17: Write MCP server integration test (tools/list)

**Files:**
- Create: `claudecode-mpm-plugin/test_server.py`

- [ ] **Step 1: Write `test_server.py`**

Create `claudecode-mpm-plugin/test_server.py` with this exact content:

```python
"""Integration tests for the FastMCP server (in-process, no subprocess).

Verifies the server registers the expected tools with the correct schemas.
Does NOT spawn a real mpm binary; the tool bodies are exercised separately
via test_tools.py and the manual JSON-RPC smoke test in Task 23.
"""
import pytest
from mcp.server.fastmcp import FastMCP

from server import mcp


@pytest.fixture
def tool_list() -> list:
    """Return the list of registered tools."""
    return list(mcp._tool_manager._tools.values())


def test_server_registers_read_wake_context(tool_list):
    names = {tool.name for tool in tool_list}
    assert "read_wake_context" in names


def test_server_registers_query_long_term_memory(tool_list):
    names = {tool.name for tool in tool_list}
    assert "query_long_term_memory" in names


def test_server_registers_exactly_two_tools(tool_list):
    assert len(tool_list) == 2
```

- [ ] **Step 2: Run the test (expect PASS)**

```bash
cd /home/v/workspace/projects/mpm/claudecode-mpm-plugin
.venv/bin/pytest test_server.py -v
```

Expected: `3 passed`

- [ ] **Step 3: Commit**

```bash
cd /home/v/workspace/projects/mpm
git add claudecode-mpm-plugin/test_server.py
git commit -m "test(claudecode-mpm-plugin): add server integration test"
```

---

## Task 18: Write `.mcp.json` template

**Files:**
- Create: `claudecode-mpm-plugin/.mcp.json`

- [ ] **Step 1: Write the template**

Create `claudecode-mpm-plugin/.mcp.json` with this exact content:

```json
{
  "mcpServers": {
    "mpm": {
      "type": "stdio",
      "command": "${PWD}/.venv/bin/python3",
      "args": ["${PWD}/server.py"],
      "env": {
        "MPM_BINARY": "${MPM_BINARY}"
      }
    }
  }
}
```

- [ ] **Step 2: Verify it's valid JSON**

```bash
python3 -c "import json; print(json.load(open('/home/v/workspace/projects/mpm/claudecode-mpm-plugin/.mcp.json')))"
```

Expected: prints the parsed dict (with `${PWD}` and `${MPM_BINARY}` as literal strings — JSON does not expand env vars).

- [ ] **Step 3: Commit**

```bash
cd /home/v/workspace/projects/mpm
git add claudecode-mpm-plugin/.mcp.json
git commit -m "feat(claudecode-mpm-plugin): add .mcp.json template"
```

---

## Task 19: Write `SKILL.md`

**Files:**
- Create: `claudecode-mpm-plugin/skills/mpm/SKILL.md`

- [ ] **Step 1: Write the skill**

Create `claudecode-mpm-plugin/skills/mpm/SKILL.md` with this exact content:

```markdown
---
name: mpm
description: MPM (Memory Persistence Module) — wake context, long-term memory, lessons, topics, decisions, theories, references. Use when the user is working on MPM, OpenClaw, or any long-running project where prior decisions, evidence, and lessons matter.
---

# MPM Workflow

You have access to a suite of MPM tools (prefixed `mpm_`). Follow this workflow every session.

## On Session Start (DO THIS FIRST)

1. **Wake context is your anchor.** When you need to know what the user was doing before you arrived, call `mpm_read_wake_context` immediately. If it's empty, tell the user you are ready for context — do not invent prior state. This is the single most important instruction in this skill; it prevents hallucinated context on the first turn.
2. **Read directives.** Call `mpm_read_directives` to learn the user's prime directives (what you must and must not do).

## Before Answering

Before answering any question about prior work, decisions, dates, people, preferences, or todos, call `mpm_query_long_term_memory` with a natural-language query.

## After Non-Trivial Actions

- After learning something reusable: `mpm_save_lesson` (type: `insight` / `practice` / `warning`).
- After any non-obvious choice: `mpm_record_decision` (context, choice, rationale — rationale is the most important field).
- After any durable fact: `mpm_save_to_memory`.

## Hypothesize Before Fixing

Before non-obvious fixes:
1. Call `mpm_propose_theory` with an explicit, executable validation criterion.
2. Run the test.
3. Call `mpm_resolve_theory` with `proven` or `disproven` + the conclusion.

## Timeouts

If a tool call times out, the MCP server has a 15s default. Pass `timeout_override_ms` (e.g. 60000) for slow operations: `mpm_add_reference`, `mpm_propose_theory`, `mpm_resolve_theory`. The server caps any override at 5 minutes.
```

- [ ] **Step 2: Verify the file**

```bash
head -5 /home/v/workspace/projects/mpm/claudecode-mpm-plugin/skills/mpm/SKILL.md
```

Expected: shows the front-matter opening `---` and `name: mpm`.

- [ ] **Step 3: Commit**

```bash
cd /home/v/workspace/projects/mpm
git add claudecode-mpm-plugin/skills/mpm/SKILL.md
git commit -m "feat(claudecode-mpm-plugin): add MPM skill for agent workflow"
```

---

## Task 20: Write `install.sh`

**Files:**
- Create: `claudecode-mpm-plugin/install.sh`

- [ ] **Step 1: Write the install script**

Create `claudecode-mpm-plugin/install.sh` with this exact content:

```bash
#!/usr/bin/env bash
# install.sh — install claudecode-mpm-plugin/ into ../.claude/
#
# Always creates a project-local venv at $SRC/.venv and points the installed
# server at $SRC/.venv/bin/python3 so runtime deps are isolated.
# Resolves the mpm binary at install time (5-step fallback chain) and
# renders .mcp.json with the absolute path.
#
# Flags:
#   --symlink   (default) Symlink files; edits in source propagate.
#   --copy             Copy files; one-shot install, no link drift.
#   --uninstall        Remove .claude/mcp.json, .claude/mpm-mcp/, .claude/skills/mpm/.

set -euo pipefail

SRC="$(cd "$(dirname "$0")" && pwd)"
DST="$(cd "$SRC/.." && pwd)/.claude"
VENV="$SRC/.venv"
VENV_PY="$VENV/bin/python3"

ACTION="symlink"
if [ "${1:-}" = "--copy" ]; then ACTION="copy"; fi
if [ "${1:-}" = "--uninstall" ]; then ACTION="uninstall"; fi

resolve_mpm() {
    if [ -n "${MPM_BINARY:-}" ] && [ -x "$MPM_BINARY" ]; then
        echo "$MPM_BINARY"; return
    fi
    if command -v mpm >/dev/null 2>&1; then
        command -v mpm; return
    fi
    if [ -x "$SRC/../bin/mpm" ]; then
        echo "$SRC/../bin/mpm"; return
    fi
    if [ -x "$SRC/../mpm" ]; then
        echo "$SRC/../mpm"; return
    fi
    echo "error: could not locate 'mpm' binary (set \$MPM_BINARY or run 'make install')" >&2
    exit 1
}

uninstall() {
    rm -f "$DST/mcp.json"
    rm -rf "$DST/mpm-mcp" "$DST/skills/mpm"
    echo "✓ Uninstalled."
}

if [ "$ACTION" = "uninstall" ]; then
    uninstall
    exit 0
fi

# Sanity checks
[ -f "$SRC/.mcp.json" ] || { echo "error: $SRC/.mcp.json missing"; exit 1; }
[ -f "$SRC/server.py" ] || { echo "error: $SRC/server.py missing"; exit 1; }
command -v python3 >/dev/null || { echo "error: python3 not in PATH"; exit 1; }

# Resolve mpm binary
MPM_PATH="$(resolve_mpm)"

# Create venv if missing
if [ ! -x "$VENV_PY" ]; then
    echo "Creating venv at $VENV ..."
    python3 -m venv "$VENV"
fi

# Install deps into venv
"$VENV_PY" -c 'import mcp' 2>/dev/null || {
    echo "Installing mcp SDK into venv..."
    "$VENV_PY" -m pip install --quiet -r "$SRC/requirements.txt"
}

# Create target dirs
mkdir -p "$DST/mpm-mcp" "$DST/skills/mpm"

# Link or copy source files
link_or_copy() {
    if [ "$ACTION" = "symlink" ]; then
        ln -sf "$1" "$2"
    else
        cp -f "$1" "$2"
    fi
}

link_or_copy "$SRC/server.py"               "$DST/mpm-mcp/server.py"
link_or_copy "$SRC/skills/mpm/SKILL.md"     "$DST/skills/mpm/SKILL.md"

# Render .mcp.json with the resolved mpm path (env block + absolute paths)
sed "s|\${MPM_BINARY}|$MPM_PATH|g; s|\${PWD}|$SRC|g" "$SRC/.mcp.json" > "$DST/mcp.json"

echo "✓ Installed ($ACTION). Restart Claude Code to pick up MCP server."
echo "  mpm binary: $MPM_PATH"
```

- [ ] **Step 2: Make it executable**

```bash
chmod +x /home/v/workspace/projects/mpm/claudecode-mpm-plugin/install.sh
```

- [ ] **Step 3: Commit**

```bash
cd /home/v/workspace/projects/mpm
git add claudecode-mpm-plugin/install.sh
git commit -m "feat(claudecode-mpm-plugin): add install.sh (symlink/copy/uninstall)"
```

---

## Task 21: Manual install test (`--symlink` + `--uninstall`)

**Files:** none (manual verification of Task 20)

- [ ] **Step 1: Run install**

```bash
cd /home/v/workspace/projects/mpm/claudecode-mpm-plugin
./install.sh --symlink
```

Expected output: `✓ Installed (symlink). Restart Claude Code to pick up MCP server.` followed by `mpm binary: <path>`.

- [ ] **Step 2: Verify symlinks**

```bash
ls -la /home/v/workspace/projects/mpm/.claude/
ls -la /home/v/workspace/projects/mpm/.claude/mpm-mcp/
ls -la /home/v/workspace/projects/mpm/.claude/skills/mpm/
```

Expected:
- `.claude/mcp.json` exists as a regular file (rendered from template)
- `.claude/mpm-mcp/server.py` exists as a symlink to `../../claudecode-mpm-plugin/server.py`
- `.claude/skills/mpm/SKILL.md` exists as a symlink to `../../../claudecode-mpm-plugin/skills/mpm/SKILL.md`

- [ ] **Step 3: Inspect the rendered `.claude/mcp.json`**

```bash
cat /home/v/workspace/projects/mpm/.claude/mcp.json
```

Expected: a JSON document with `"command": "/home/v/workspace/projects/mpm/claudecode-mpm-plugin/.venv/bin/python3"` and `"MPM_BINARY": "/...path-to-mpm..."` (no `${...}` placeholders remaining).

- [ ] **Step 4: Verify the mpm binary is reachable from the rendered command**

```bash
BINARY=$(python3 -c "import json; print(json.load(open('/home/v/workspace/projects/mpm/.claude/mcp.json'))['mcpServers']['mpm']['env']['MPM_BINARY'])")
test -x "$BINARY" && echo "executable: $BINARY" || echo "NOT EXECUTABLE: $BINARY"
```

Expected: `executable: <path>`

- [ ] **Step 5: Run uninstall**

```bash
cd /home/v/workspace/projects/mpm/claudecode-mpm-plugin
./install.sh --uninstall
```

Expected: `✓ Uninstalled.`

- [ ] **Step 6: Verify cleanup**

```bash
ls /home/v/workspace/projects/mpm/.claude/mpm-mcp /home/v/workspace/projects/mpm/.claude/skills/mpm 2>&1
test -f /home/v/workspace/projects/mpm/.claude/mcp.json && echo "mcp.json still exists" || echo "mcp.json removed"
```

Expected: `No such file or directory` errors for both dirs, and `mcp.json removed`.

- [ ] **Step 7: No commit (this is a manual verification step)**

---

## Task 22: Re-install and pipe JSON-RPC to verify tools/list

**Files:** none (manual verification)

- [ ] **Step 1: Re-install**

```bash
cd /home/v/workspace/projects/mpm/claudecode-mpm-plugin
./install.sh --symlink
```

Expected: `✓ Installed (symlink)...`

- [ ] **Step 2: Pipe a `tools/list` JSON-RPC request to the server**

```bash
cd /home/v/workspace/projects/mpm/claudecode-mpm-plugin
echo '{"jsonrpc":"2.0","id":1,"method":"tools/list"}' | ./.venv/bin/python3 server.py 2>&1 | head -50
```

Expected: a JSON-RPC response with `"result":{"tools":[...]}` containing at least 2 tool entries: `read_wake_context` and `query_long_term_memory`.

- [ ] **Step 3: Verify the tool names are present**

```bash
cd /home/v/workspace/projects/mpm/claudecode-mpm-plugin
echo '{"jsonrpc":"2.0","id":1,"method":"tools/list"}' | ./.venv/bin/python3 server.py 2>&1 | grep -oE '"name":"[a-z_]+"' | sort -u
```

Expected output (only tool names, sorted unique):
```
"name":"query_long_term_memory"
"name":"read_wake_context"
```

- [ ] **Step 4: No commit (manual verification)**

---

## Task 23: Verify `os.environ["MPM_BINARY"]` matches the rendered `.mcp.json` (ghost-env guard)

**Files:** none (manual verification)

- [ ] **Step 1: Compare the rendered env to what the server actually sees**

```bash
cd /home/v/workspace/projects/mpm/claudecode-mpm-plugin
RENDERED=$(python3 -c "import json; print(json.load(open('/home/v/.openclaw/workspace/projects/mpm/.claude/mcp.json'))['mcpServers']['mpm']['env']['MPM_BINARY'])")
echo "Rendered:  $RENDERED"
./.venv/bin/python3 -c "import os; print('Server sees:', os.environ.get('MPM_BINARY', '(unset)'))"
```

Expected: both lines point to the same absolute path to `mpm`. If they differ, the ghost-environment guard is broken.

> Note: this step simulates the runtime by reading the rendered `.mcp.json` env block. When Claude Code actually spawns the server, the `env` block from `.mcp.json` is merged into the process environment — the comparison above proves the values match.

- [ ] **Step 2: No commit (manual verification)**

---

## Task 24: Verify `DEBUG=1` writes to `.claude/debug.log`

**Files:** none (manual verification)

- [ ] **Step 1: Remove any existing debug log**

```bash
rm -f /home/v/workspace/projects/mpm/.claude/debug.log
```

- [ ] **Step 2: Run a debug-mode tool call**

```bash
cd /home/v/workspace/projects/mpm/claudecode-mpm-plugin
DEBUG=1 MPM_DEBUG_LOG=/home/v/workspace/projects/mpm/.claude/debug.log ./.venv/bin/python3 -c "
import asyncio
from tools import debug_log, run_mpm
debug_log('test: invoking run_mpm')
result = asyncio.run(run_mpm(['version']))
debug_log(f'test: exit_code={result.exit_code}')
"
```

Expected: no error; the script prints nothing (debug_log is silent on success).

- [ ] **Step 3: Verify the log file was written**

```bash
cat /home/v/workspace/projects/mpm/.claude/debug.log
```

Expected: at least 2 lines, each with an ISO timestamp:
```
2026-06-15T... test: invoking run_mpm
2026-06-15T... test: exit_code=0
```

- [ ] **Step 4: Verify debug log is gitignored**

```bash
cd /home/v/workspace/projects/mpm
git check-ignore claudecode-mpm-plugin/.claude/debug.log 2>&1 || \
git check-ignore .claude/debug.log 2>&1
```

Expected: the path is reported as ignored (the `.claude/debug.log` line in the gitignore may need adjustment if the path is at the project root rather than the plugin dir — adjust gitignore accordingly).

- [ ] **Step 5: No commit (manual verification)**

---

## Task 25: Write `README.md`

**Files:**
- Create: `claudecode-mpm-plugin/README.md`

- [ ] **Step 1: Write the README**

Create `claudecode-mpm-plugin/README.md` with this exact content:

```markdown
# claudecode-mpm-plugin

A workspace-level Claude Code plugin that exposes MPM (Memory Persistence
Module) reasoning primitives to Claude Code via the Model Context Protocol.

Mirrors the 18-tool surface of `opencode-mpm-plugin`. Ships with 2 tools in
this version: `read_wake_context` and `query_long_term_memory`. The remaining
16 are added in a follow-up plan.

## Install

```bash
./install.sh --symlink   # default; edits in source propagate
# or
./install.sh --copy      # one-shot copy, no link drift
```

The installer:
1. Resolves the `mpm` binary via a 5-step fallback chain (`$MPM_BINARY` → `which mpm` → `../bin/mpm` → `../mpm` symlink → fail).
2. Creates a project-local venv at `claudecode-mpm-plugin/.venv` (skipped if present).
3. Installs `mcp`, `pydantic`, `pytest`, `pytest-asyncio` into the venv.
4. Symlinks (or copies) `server.py` and `skills/mpm/SKILL.md` into `.claude/`.
5. Renders `.claude/mcp.json` from the template with the absolute mpm path and the venv's python interpreter.

Restart Claude Code to pick up the MCP server.

## Uninstall

```bash
./install.sh --uninstall
```

Removes `.claude/mcp.json`, `.claude/mpm-mcp/`, `.claude/skills/mpm/`. The
project-local venv is **not** removed.

## Tools (Phase 1 — 2 of 18)

| Tool | Purpose |
|------|---------|
| `read_wake_context` | Read the agent's wake context (active mode, persona, recent topics/memories). |
| `query_long_term_memory` | Search long-term memory with a natural-language query. |

The remaining 16 tools (lessons, topics, references, decisions, theories,
proactive recall, etc.) ship in Phase 2.

## SKILL.md

`skills/mpm/SKILL.md` is loaded by Claude Code at session start and teaches
the agent the MPM workflow: read wake context first, query memory before
answering, propose theories before fixes, etc. Treat it as a versioned
prompt — update it in the repo and the agent learns the new workflow on the
next session restart.

## Configuration

| Env var | Default | Purpose |
|---------|---------|---------|
| `MPM_BINARY` | `mpm` (resolved at install) | Path to the `mpm` binary. |
| `MPM_WORKSPACE` | (unset; mpm resolves) | MPM workspace dir. If unset, `mpm` uses its own fallback chain. |
| `DEBUG` | unset | Set to `1` to enable debug logging. |
| `MPM_DEBUG_LOG` | `.claude/debug.log` | Path to the debug log file (created lazily). |

## Troubleshooting

**`mpm: command not found`**
The renderer couldn't locate the `mpm` binary. Set `$MPM_BINARY` to the
absolute path, or run `make install` in the parent `mpm/` project, then
re-run `./install.sh`.

**`ModuleNotFoundError: No module named 'mcp'`**
The venv is missing or broken. Delete `claudecode-mpm-plugin/.venv/` and
re-run `./install.sh --symlink`.

**MCP server not visible in Claude Code**
- Confirm `.claude/mcp.json` exists and is valid JSON.
- Restart Claude Code (the MCP registry loads at startup).
- Check the Claude Code logs for stdio errors from the server.

**Tool call times out (exit_124)**
Default timeout is 15s. For slow operations (`add_reference`, `propose_theory`)
the agent should pass `timeout_override_ms` (e.g. 60000). The server clamps
any override to 5 minutes.

## Development

```bash
# Run tests
cd claudecode-mpm-plugin
.venv/bin/pytest test_tools.py test_server.py -v

# Run server directly for manual smoke testing
echo '{"jsonrpc":"2.0","id":1,"method":"tools/list"}' | ./.venv/bin/python3 server.py

# Add DEBUG logging
DEBUG=1 ./.venv/bin/python3 server.py
```

## License

Same as the parent `mpm` project.
```

- [ ] **Step 2: Verify the README renders**

```bash
head -20 /home/v/workspace/projects/mpm/claudecode-mpm-plugin/README.md
```

Expected: shows the title and install section.

- [ ] **Step 3: Commit**

```bash
cd /home/v/workspace/projects/mpm
git add claudecode-mpm-plugin/README.md
git commit -m "docs(claudecode-mpm-plugin): add README"
```

---

## Task 26: Final integration test — run all tests + verify install + check `git status`

**Files:** none (final verification)

- [ ] **Step 1: Run the full test suite**

```bash
cd /home/v/workspace/projects/mpm/claudecode-mpm-plugin
.venv/bin/pytest -v
```

Expected: all tests pass (22+ tests across `test_tools.py` and `test_server.py`).

- [ ] **Step 2: Verify install still works end-to-end**

```bash
cd /home/v/workspace/projects/mpm/claudecode-mpm-plugin
./install.sh --uninstall
./install.sh --symlink
test -f /home/v/workspace/projects/mpm/.claude/mcp.json && echo "OK: mcp.json installed"
test -L /home/v/workspace/projects/mpm/.claude/mpm-mcp/server.py && echo "OK: server.py is a symlink"
test -L /home/v/workspace/projects/mpm/.claude/skills/mpm/SKILL.md && echo "OK: SKILL.md is a symlink"
```

Expected: 3 `OK:` lines.

- [ ] **Step 3: Verify `git status` is clean for the plugin**

```bash
cd /home/v/workspace/projects/mpm
git status claudecode-mpm-plugin/
```

Expected: no uncommitted changes inside `claudecode-mpm-plugin/`. (Other unrelated changes outside that dir are expected — see step 4.)

- [ ] **Step 4: Verify `.claude/` is not in git tracking for unrelated files**

```bash
cd /home/v/workspace/projects/mpm
git status --short .claude/ 2>&1 | head -20
```

Expected: any output here is expected (the `mcp.json`, `mpm-mcp/`, `skills/mpm/` are runtime-installed, not source-tracked; the project root's `.claude/` may have other pre-existing entries).

- [ ] **Step 5: No commit (final verification)**

---

## Done — Phase 1 Complete

You should now have:
- A working `claudecode-mpm-plugin/` with 11 source files + 2 test files
- 22+ unit/integration tests, all passing
- `install.sh --symlink` (and `--copy`, `--uninstall`) working
- A rendered `.claude/mcp.json` with the absolute mpm path baked in
- `SKILL.md` in place to guide the agent on session start

**Next step:** Phase 3 from the spec — adding the remaining 16 tools. The pattern is established; each tool is one Pydantic input model + one `@mcp.tool` handler + output formatting. Use this plan as a template; create a new plan at `docs/superpowers/plans/2026-06-15-claude-mpm-plugin-phase-3.md` when ready to scale.
