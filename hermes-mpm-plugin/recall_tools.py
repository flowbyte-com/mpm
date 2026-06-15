from hermes_agent.tools import registry

from .base import run_mpm_call

MPM_TOOLSET = "mpm"


def _handle_proactive_recall_hint(args, **kw):
    text = args.get("conversation_text", "")
    if not text.strip():
        return []
    result = run_mpm_call("proactive_recall_hint", {
        "conversation_text": text,
        "max_hints": args.get("max_hints", 3),
        "min_score": args.get("min_score", -3.0),
    })
    hints = result if isinstance(result, list) else []
    scored = []
    for h in hints:
        content = h.get("content", "")
        meta = h.get("metadata") or {}
        status = meta.get("status", "")
        conclusion = meta.get("conclusion", "")
        choice = ""
        rationale = ""
        hypothesis = ""
        if h.get("collection") == "decisions":
            first_line = (content.split("\n")[0] or "")
            if first_line.upper().startswith("CHOICE: "):
                choice = first_line[len("CHOICE: "):]
            for line in content.split("\n"):
                ul = line.upper()
                if ul.startswith("RATIONALE: "):
                    rationale = line[len("RATIONALE: "):]
        elif h.get("collection") == "theories":
            for line in content.split("\n"):
                ul = line.upper()
                if ul.startswith("HYPOTHESIS: "):
                    hypothesis = line[len("HYPOTHESIS: "):]
        scored.append({
            "memory_id": h.get("id", ""),
            "content": content,
            "collection": h.get("collection", ""),
            "score": h.get("score", 0),
            "choice": choice,
            "rationale": rationale,
            "hypothesis": hypothesis,
            "status": status,
            "conclusion": conclusion,
        })
    scored.sort(key=lambda x: x["score"], reverse=True)
    return scored[: args.get("max_hints", 3)]


registry.register(
    name="proactive_recall_hint",
    toolset=MPM_TOOLSET,
    schema={
        "name": "proactive_recall_hint",
        "description": (
            "Checks recent conversation context for overlap with decisions and theories. "
            "Returns a structured Recall Hint if a semantic match is found. "
            "Call this after each user message — one hint per turn max. "
            "Surface only the top hint (rank 0) when hints are returned."
        ),
        "parameters": {
            "type": "object",
            "properties": {
                "conversation_text": {"type": "string"},
                "max_hints": {"type": "integer", "default": 3},
                "min_score": {"type": "number", "default": -3.0},
            },
            "required": ["conversation_text"],
        },
    },
    handler=_handle_proactive_recall_hint,
    emoji="💡",
    max_result_size_chars=20_000,
)