from hermes_agent.tools import registry

from .base import run_mpm_call

MPM_TOOLSET = "mpm"


def _handle_add_reference(args, **kw):
    payload = {"filepath": args.get("filepath", "")}
    if args.get("title"):
        payload["title"] = args["title"]
    return run_mpm_call("add_reference", payload)


def _handle_search_references(args, **kw):
    return run_mpm_call("search_references", {
        "query": args.get("query", ""),
        "limit": args.get("limit", 5),
    })


def _handle_list_references(args, **kw):
    return run_mpm_call("list_references", {
        "limit": args.get("limit", 50),
        "offset": args.get("offset", 0),
    })


registry.register(
    name="add_reference",
    toolset=MPM_TOOLSET,
    schema={
        "name": "add_reference",
        "description": (
            "Ingest a document as a reference into MPM. "
            "Supported formats: .txt, .md, .html, .epub, .pdf. "
            "The document is chunked and indexed for semantic search."
        ),
        "parameters": {
            "type": "object",
            "properties": {
                "filepath": {"type": "string"},
                "title": {"type": "string"},
            },
            "required": ["filepath"],
        },
    },
    handler=_handle_add_reference,
    emoji="📄",
    max_result_size_chars=50_000,
)


registry.register(
    name="search_references",
    toolset=MPM_TOOLSET,
    schema={
        "name": "search_references",
        "description": (
            "Search content within ingested reference documents. "
            "Returns matching chunks from the reference library."
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
    handler=_handle_search_references,
    emoji="🔍",
    max_result_size_chars=50_000,
)


registry.register(
    name="list_references",
    toolset=MPM_TOOLSET,
    schema={
        "name": "list_references",
        "description": (
            "List all ingested reference documents in MPM. "
            "Shows document titles, chunk counts, and tags."
        ),
        "parameters": {
            "type": "object",
            "properties": {
                "limit": {"type": "integer", "default": 50},
                "offset": {"type": "integer", "default": 0},
            },
        },
    },
    handler=_handle_list_references,
    emoji="📚",
    max_result_size_chars=50_000,
)