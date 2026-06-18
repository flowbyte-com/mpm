from hermes_agent.tools import registry

from .base import run_mpm_call

MPM_TOOLSET = "mpm"


def _handle_create_topic(args, **kw):
    return run_mpm_call("create_topic", {
        "name": args.get("name", ""),
        "description": args.get("description") or "",
    })


def _handle_search_topics(args, **kw):
    return run_mpm_call("search_topics", {
        "query": args.get("query", ""),
        "limit": args.get("limit", 20),
    })


def _handle_link_topic(args, **kw):
    return run_mpm_call("link_topic", {
        "memory_id": args.get("memory_id", ""),
        "topic_id": args.get("topic_id", ""),
    })


registry.register(
    name="create_topic",
    toolset=MPM_TOOLSET,
    schema={
        "name": "create_topic",
        "description": (
            "Create a topic in MPM to organize related memories and knowledge. "
            "Topics can be searched and serve as memory clusters."
        ),
        "parameters": {
            "type": "object",
            "properties": {
                "name": {"type": "string"},
                "description": {"type": "string"},
            },
            "required": ["name"],
        },
    },
    handler=_handle_create_topic,
    emoji="🏷️",
    max_result_size_chars=50_000,
)


registry.register(
    name="search_topics",
    toolset=MPM_TOOLSET,
    schema={
        "name": "search_topics",
        "description": (
            "Search MPM topics for relevant knowledge clusters. "
            "Topics group related memories and provide context for the agent."
        ),
        "parameters": {
            "type": "object",
            "properties": {
                "query": {"type": "string"},
                "limit": {"type": "integer", "default": 20},
            },
        },
    },
    handler=_handle_search_topics,
    emoji="🔍",
    max_result_size_chars=50_000,
)


registry.register(
    name="link_topic",
    toolset=MPM_TOOLSET,
    schema={
        "name": "link_topic",
        "description": (
            "Link an existing memory to an existing topic. "
            "Use this after saving a memory and seeing topic suggestions. "
            "The memory and topic must both already exist."
        ),
        "parameters": {
            "type": "object",
            "properties": {
                "memory_id": {"type": "string"},
                "topic_id": {"type": "string"},
            },
            "required": ["memory_id", "topic_id"],
        },
    },
    handler=_handle_link_topic,
    emoji="🔗",
    max_result_size_chars=10_000,
)