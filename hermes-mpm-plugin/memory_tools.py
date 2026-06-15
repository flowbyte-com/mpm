from hermes_agent.tools import registry

from .base import run_mpm_call

MPM_TOOLSET = "mpm"


def _handle_query_long_term_memory(args, **kw):
    return run_mpm_call("query_long_term_memory", {
        "query": args.get("query", ""),
        "limit": args.get("limit", 5),
    })


def _handle_save_to_memory(args, **kw):
    payload = {
        "fact": args.get("fact", ""),
        "tags": args.get("tags") or [],
        "weight": args.get("weight", 0.5),
    }
    if args.get("ttl"):
        payload["ttl"] = args["ttl"]
    if args.get("collection"):
        payload["collection"] = args["collection"]
    return run_mpm_call("save_to_memory", payload)


def _handle_challenge_memory(args, **kw):
    return run_mpm_call("challenge_memory", {
        "memoryId": args.get("memory_id", ""),
        "evidence": args.get("evidence", ""),
    })


registry.register(
    name="query_long_term_memory",
    toolset=MPM_TOOLSET,
    schema={
        "name": "query_long_term_memory",
        "description": (
            "Search MPM long-term memory. "
            "Before answering anything about prior work, decisions, dates, people, "
            "preferences, or todos — run this first. "
            "Returns matching memories as formatted text."
        ),
        "parameters": {
            "type": "object",
            "properties": {
                "query": {"type": "string"},
                "limit": {"type": "integer", "default": 5},
            },
            "required": ["query"],
        },
    },
    handler=_handle_query_long_term_memory,
    emoji="🧠",
    max_result_size_chars=50_000,
)


registry.register(
    name="save_to_memory",
    toolset=MPM_TOOLSET,
    schema={
        "name": "save_to_memory",
        "description": (
            "Persist a fact, lesson, or decision to MPM long-term memory. "
            "After any non-trivial action, lesson learned, or decision — call this. "
            "Tags help later retrieval. Weight 0.5 by default; higher for important truths. "
            "Use TTL '24h' for session-scoped facts (e.g. current task context), "
            "use TTL '0' for permanent memories (e.g. user preferences, project state)."
        ),
        "parameters": {
            "type": "object",
            "properties": {
                "fact": {"type": "string"},
                "tags": {"type": "array", "items": {"type": "string"}},
                "weight": {"type": "number", "default": 0.5},
                "ttl": {"type": "string"},
                "collection": {"type": "string"},
            },
            "required": ["fact"],
        },
    },
    handler=_handle_save_to_memory,
    emoji="💾",
    max_result_size_chars=50_000,
)


registry.register(
    name="challenge_memory",
    toolset=MPM_TOOLSET,
    schema={
        "name": "challenge_memory",
        "description": (
            "Challenge an existing memory by presenting contradictory evidence. "
            "This weakens the memory, creates a pending theory, and logs a decision. "
            "Use when you discover that a stored memory is no longer accurate. "
            "Trigger: conversation or test results contradict a specific stored memory."
        ),
        "parameters": {
            "type": "object",
            "properties": {
                "memory_id": {"type": "string"},
                "evidence": {"type": "string"},
            },
            "required": ["memory_id", "evidence"],
        },
    },
    handler=_handle_challenge_memory,
    emoji="⚔️",
    max_result_size_chars=10_000,
)