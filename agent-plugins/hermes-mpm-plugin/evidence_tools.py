from hermes_agent.tools import registry

from .base import run_mpm_call

MPM_TOOLSET = "mpm"


def _handle_add_evidence(args, **kw):
    payload = {
        "artifact_id": args.get("artifact_id", ""),
        "artifact_type": args.get("artifact_type", "memory"),
        "type": args.get("type", ""),
        "source_group": args.get("source_group", ""),
        "created_by": args.get("created_by", ""),
    }
    if args.get("strength") is not None:
        payload["strength"] = args["strength"]
    if args.get("notes"):
        payload["notes"] = args["notes"]
    return run_mpm_call("add_evidence", payload)


def _handle_list_evidence(args, **kw):
    return run_mpm_call("list_evidence", {
        "artifact_id": args.get("artifact_id", ""),
        "artifact_type": args.get("artifact_type", "memory"),
    })


def _handle_query_confidence_history(args, **kw):
    payload = {
        "artifact_id": args.get("artifact_id", ""),
        "artifact_type": args.get("artifact_type", "memory"),
    }
    if args.get("limit") is not None:
        payload["limit"] = args["limit"]
    return run_mpm_call("query_confidence_history", payload)


registry.register(
    name="add_evidence",
    toolset=MPM_TOOLSET,
    schema={
        "name": "add_evidence",
        "description": (
            "Record evidence supporting or challenging an existing memory, theory, decision, or lesson. "
            "Use this when you observe system behavior, run unit tests, or receive user feedback that "
            "validates or contradicts existing knowledge. The system will automatically recompute its "
            "confidence in the artifact based on this evidence. "
            "Use type='challenge' when evidence contradicts the artifact."
        ),
        "parameters": {
            "type": "object",
            "properties": {
                "artifact_id": {"type": "string"},
                "artifact_type": {
                    "type": "string",
                    "enum": ["memory", "theory", "decision", "lesson"],
                },
                "type": {
                    "type": "string",
                    "enum": [
                        "observation",
                        "test",
                        "reproduction",
                        "challenge",
                        "decision_outcome",
                        "external_reference",
                    ],
                },
                "source_group": {"type": "string"},
                "strength": {"type": "number"},
                "created_by": {"type": "string"},
                "notes": {"type": "string"},
            },
            "required": ["artifact_id", "artifact_type", "type", "source_group", "created_by"],
        },
    },
    handler=_handle_add_evidence,
    emoji="🧪",
    max_result_size_chars=50_000,
)


registry.register(
    name="list_evidence",
    toolset=MPM_TOOLSET,
    schema={
        "name": "list_evidence",
        "description": (
            "Retrieve the complete chronological ledger of evidence supporting or challenging a specific "
            "artifact. Use this to understand the specific observations and tests that form the foundation "
            "of the system's current confidence level."
        ),
        "parameters": {
            "type": "object",
            "properties": {
                "artifact_id": {"type": "string"},
                "artifact_type": {
                    "type": "string",
                    "enum": ["memory", "theory", "decision", "lesson"],
                },
            },
            "required": ["artifact_id", "artifact_type"],
        },
    },
    handler=_handle_list_evidence,
    emoji="📋",
    max_result_size_chars=50_000,
)


registry.register(
    name="query_confidence_history",
    toolset=MPM_TOOLSET,
    schema={
        "name": "query_confidence_history",
        "description": (
            "Retrieve the historical timeline of confidence score changes for a specific artifact. "
            "Use this to see the trajectory of an artifact's reliability — whether it is trending up "
            "(proven over time), trending down (recently challenged), or slowly decaying due to age."
        ),
        "parameters": {
            "type": "object",
            "properties": {
                "artifact_id": {"type": "string"},
                "artifact_type": {
                    "type": "string",
                    "enum": ["memory", "theory", "decision", "lesson"],
                },
                "limit": {"type": "integer", "default": 10},
            },
            "required": ["artifact_id", "artifact_type"],
        },
    },
    handler=_handle_query_confidence_history,
    emoji="📈",
    max_result_size_chars=50_000,
)
