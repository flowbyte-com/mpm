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
