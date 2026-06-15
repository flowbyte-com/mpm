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
