from hermes_agent.tools import registry

from .base import run_mpm_call

MPM_TOOLSET = "mpm"


def _handle_record_decision(args, **kw):
    payload = {
        "context": args.get("context", ""),
        "choice": args.get("choice", ""),
        "rationale": args.get("rationale", ""),
        "tags": args.get("tags") or [],
        "weight": args.get("weight", 0.5),
    }
    if args.get("outcome"):
        payload["outcome"] = args["outcome"]
    return run_mpm_call("record_decision", payload)


def _handle_propose_theory(args, **kw):
    return run_mpm_call("propose_theory", {
        "hypothesis": args.get("hypothesis", ""),
        "validation_criteria": args.get("validation_criteria", ""),
        "tags": args.get("tags") or [],
    })


def _handle_resolve_theory(args, **kw):
    status = args.get("new_status", "")
    if status not in ("proven", "disproven"):
        return {
            "success": False,
            "error": "invalid_status",
            "message": "new_status must be 'proven' or 'disproven'.",
        }
    return run_mpm_call("resolve_theory", {
        "theoryId": args.get("theory_id", ""),
        "conclusion": args.get("conclusion", ""),
        "newStatus": status,
    })


registry.register(
    name="record_decision",
    toolset=MPM_TOOLSET,
    schema={
        "name": "record_decision",
        "description": (
            "Record an architectural decision, library choice, or any moment where you "
            "chose path A over path B. Call this BEFORE or AFTER the decision — not just "
            "after. The rationale is the most important field: it is what makes past "
            "decisions reusable when you encounter a similar problem weeks later. "
            "Trigger: whenever you weigh tradeoffs and choose one, or whenever you notice "
            "you just picked an approach without recording why."
        ),
        "parameters": {
            "type": "object",
            "properties": {
                "context": {"type": "string"},
                "choice": {"type": "string"},
                "rationale": {"type": "string"},
                "outcome": {"type": "string"},
                "tags": {"type": "array", "items": {"type": "string"}},
                "weight": {"type": "number", "default": 0.5},
            },
            "required": ["context", "choice", "rationale"],
        },
    },
    handler=_handle_record_decision,
    emoji="⚖️",
    max_result_size_chars=50_000,
)


registry.register(
    name="propose_theory",
    toolset=MPM_TOOLSET,
    schema={
        "name": "propose_theory",
        "description": (
            "Log a hypothesis about causality before writing a fix. "
            "When you think 'X is probably causing Y' — propose it, define the test, "
            "then run the test. Writing the validation criteria forces you to confront "
            "whether the assumption is actually testable, and often collapses a false "
            "hypothesis before it wastes an hour of debugging time. "
            "Trigger: whenever you form a 'I think X is causing Y' assumption during "
            "debugging or design work."
        ),
        "parameters": {
            "type": "object",
            "properties": {
                "hypothesis": {"type": "string"},
                "validation_criteria": {"type": "string"},
                "tags": {"type": "array", "items": {"type": "string"}},
            },
            "required": ["hypothesis", "validation_criteria"],
        },
    },
    handler=_handle_propose_theory,
    emoji="🧪",
    max_result_size_chars=50_000,
)


registry.register(
    name="resolve_theory",
    toolset=MPM_TOOLSET,
    schema={
        "name": "resolve_theory",
        "description": (
            "Close the loop on a pending theory after running its validation criteria. "
            "If the hypothesis was confirmed, save the confirmed result as a permanent "
            "memory (weight=1.0, include the theory_id as a tag for traceability). "
            "If it was disproven, record what actually caused the problem instead. "
            "Trigger: immediately after executing the test described in a pending theory's "
            "validationCriteria."
        ),
        "parameters": {
            "type": "object",
            "properties": {
                "theory_id": {"type": "string"},
                "conclusion": {"type": "string"},
                "new_status": {"type": "string"},
            },
            "required": ["theory_id", "conclusion", "new_status"],
        },
    },
    handler=_handle_resolve_theory,
    emoji="🔬",
    max_result_size_chars=50_000,
)