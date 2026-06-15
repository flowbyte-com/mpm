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
