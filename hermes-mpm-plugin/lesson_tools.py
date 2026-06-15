from hermes_agent.tools import registry

from .base import run_mpm_call

MPM_TOOLSET = "mpm"


def _handle_save_lesson(args, **kw):
    return run_mpm_call("save_lesson", {
        "fact": args.get("fact", ""),
        "type": args.get("type", "insight"),
        "tags": args.get("tags") or [],
    })


def _handle_search_lessons(args, **kw):
    return run_mpm_call("search_lessons", {
        "query": args.get("query", ""),
    })


def _handle_list_lessons(args, **kw):
    payload = {}
    if args.get("type"):
        payload["type"] = args["type"]
    return run_mpm_call("list_lessons", payload)


registry.register(
    name="save_lesson",
    toolset=MPM_TOOLSET,
    schema={
        "name": "save_lesson",
        "description": (
            "Persist a lesson to MPM — what was learned, observed, or should be remembered. "
            "Three types: 'warning' (don't do X), 'practice' (do Y), 'insight' (X leads to Y). "
            "Lessons accumulate as the agent's learned experience."
        ),
        "parameters": {
            "type": "object",
            "properties": {
                "fact": {"type": "string"},
                "type": {"type": "string", "default": "insight"},
                "tags": {"type": "array", "items": {"type": "string"}},
            },
            "required": ["fact"],
        },
    },
    handler=_handle_save_lesson,
    emoji="📚",
    max_result_size_chars=50_000,
)


registry.register(
    name="search_lessons",
    toolset=MPM_TOOLSET,
    schema={
        "name": "search_lessons",
        "description": (
            "Search MPM lessons for relevant learned knowledge. "
            "Use this to recall warnings, best practices, and insights before acting."
        ),
        "parameters": {
            "type": "object",
            "properties": {
                "query": {"type": "string"},
            },
            "required": ["query"],
        },
    },
    handler=_handle_search_lessons,
    emoji="🔍",
    max_result_size_chars=50_000,
)


registry.register(
    name="list_lessons",
    toolset=MPM_TOOLSET,
    schema={
        "name": "list_lessons",
        "description": (
            "List all lessons in MPM, optionally filtered by type. "
            "Types: 'warning' (don't do X), 'practice' (do Y), 'insight' (X leads to Y)."
        ),
        "parameters": {
            "type": "object",
            "properties": {
                "type": {"type": "string"},
            },
        },
    },
    handler=_handle_list_lessons,
    emoji="📋",
    max_result_size_chars=50_000,
)