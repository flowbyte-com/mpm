"""Unit tests for tools.py (subprocess plumbing + helpers)."""
import pytest

from tools import parse_mpm_result, MpmRunResult


def test_parse_happy_path_returns_parsed_json():
    result = MpmRunResult(exit_code=0, stdout='{"success": true, "id": "abc"}', stderr="")
    parsed = parse_mpm_result(result)
    assert parsed == {"success": True, "id": "abc"}
