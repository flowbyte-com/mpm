from hermes_agent.tools import registry

from .base import run_mpm_call

MPM_TOOLSET = "mpm"


def _handle_read_wake_context(args, **kw):
    return run_mpm_call("read_wake_context", {})


def _handle_read_directives(args, **kw):
    return run_mpm_call("read_directives", {})


registry.register(
    name="read_wake_context",
    toolset=MPM_TOOLSET,
    schema={
        "name": "read_wake_context",
        "description": (
            "Read the agent's wake context — session state from the previous session including "
            "active mode, active persona, recent topics, and recent memories. "
            "This is the first thing to check on session start to understand where you left off. "
            "Call this immediately on session start before doing anything else."
        ),
        "parameters": {
            "type": "object",
            "properties": {},
        },
    },
    handler=_handle_read_wake_context,
    emoji="🌅",
    max_result_size_chars=10_000,
)


registry.register(
    name="read_directives",
    toolset=MPM_TOOLSET,
    schema={
        "name": "read_directives",
        "description": (
            "Read the agent's prime directives — behavioral rules and operating principles. "
            "These define what the agent must and must not do."
        ),
        "parameters": {
            "type": "object",
            "properties": {},
        },
    },
    handler=_handle_read_directives,
    emoji="📜",
    max_result_size_chars=10_000,
)