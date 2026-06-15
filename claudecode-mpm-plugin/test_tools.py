"""Unit tests for tools.py (subprocess plumbing + helpers)."""
import asyncio
import os
import tempfile
from datetime import datetime
from unittest.mock import patch, AsyncMock

import pytest

from tools import parse_mpm_result, MpmRunResult, format_age, debug_log, run_mpm


def test_parse_happy_path_returns_parsed_json():
    result = MpmRunResult(exit_code=0, stdout='{"success": true, "id": "abc"}', stderr="")
    parsed = parse_mpm_result(result)
    assert parsed == {"success": True, "id": "abc"}


def test_parse_empty_stdout_returns_empty_success():
    result = MpmRunResult(exit_code=0, stdout="", stderr="")
    parsed = parse_mpm_result(result)
    assert parsed == {"id": "", "success": True, "count": 0}


def test_parse_invalid_json_returns_text_fallback():
    result = MpmRunResult(exit_code=0, stdout="not json", stderr="")
    parsed = parse_mpm_result(result)
    assert parsed == {"id": "", "success": True, "text": "not json"}


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


def test_parse_lowercase_sqlite_busy_stderr_returns_locked_error():
    """Regression: stderr may arrive in any case; the check must be case-insensitive."""
    result = MpmRunResult(exit_code=1, stdout="", stderr="sqlite_busy: table is locked")
    parsed = parse_mpm_result(result)
    assert parsed["error"] == "database_locked"
    assert "locked" in parsed["message"].lower()


def test_parse_generic_nonzero_exit_returns_exit_n_with_stderr():
    result = MpmRunResult(exit_code=2, stdout="", stderr="First line\nSecond line")
    parsed = parse_mpm_result(result)
    assert parsed["error"] == "exit_2"
    assert parsed["message"] == "First line"


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


def test_format_age_tz_aware_iso_returns_unknown():
    """Regression: tz-aware timestamps must not crash on naive datetime.utcnow() subtraction."""
    assert format_age("2026-06-15T12:34:56+00:00") == "unknown age"


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
    # Create a FILE where a directory is needed: makedirs(parent) will fail
    # because the parent is a file, exercising the OSError swallow path.
    bad_path = tmp_path / "bad"
    bad_path.touch()
    monkeypatch.setenv("MPM_DEBUG_LOG", str(bad_path / "log"))
    # Should not raise.
    debug_log("test message")


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
    """run_mpm should pass a clamped timeout (in seconds) to asyncio.wait_for."""
    fake_proc = AsyncMock()
    fake_proc.communicate = AsyncMock(return_value=(b"{}", b""))
    fake_proc.returncode = 0
    captured: dict = {}
    real_wait_for = asyncio.wait_for

    async def fake_wait_for(awaitable, timeout):
        captured["timeout"] = timeout
        return await real_wait_for(awaitable, timeout=None)  # no real wait; coroutine resolves immediately

    with patch("tools.asyncio.create_subprocess_exec", return_value=fake_proc), \
         patch("tools.asyncio.wait_for", side_effect=fake_wait_for):
        await run_mpm(["call", "read_wake_context"], timeout_ms=60 * 60 * 1000)
    # 1 hour requested (3600s); should be clamped to MAX_TIMEOUT_MS / 1000 = 300s.
    from tools import MAX_TIMEOUT_MS
    assert captured["timeout"] == MAX_TIMEOUT_MS / 1000


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
