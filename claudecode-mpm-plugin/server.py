"""MPM MCP server: exposes MPM reasoning primitives to Claude Code."""
import json
from typing import Optional

from mcp.server.fastmcp import FastMCP

from schemas import QueryLongTermMemoryInput, ReadWakeContextInput
from tools import debug_log, parse_mpm_result, run_mpm

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
