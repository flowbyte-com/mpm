"""Unit tests for tools.py (subprocess plumbing + helpers)."""
import pytest

from tools import parse_mpm_result, MpmRunResult


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


def test_parse_generic_nonzero_exit_returns_exit_n_with_stderr():
    result = MpmRunResult(exit_code=2, stdout="", stderr="First line\nSecond line")
    parsed = parse_mpm_result(result)
    assert parsed["error"] == "exit_2"
    assert parsed["message"] == "First line"
