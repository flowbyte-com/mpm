"""
LLM tool schemas for MPM v2.0 native integration.
OpenAI/Anthropic-compatible JSON schema definitions.
"""

QUERY_LONG_TERM_MEMORY_SCHEMA = {
    "name": "query_long_term_memory",
    "description": "Search OpenClaw's persistent SQLite memory layer for past context. "
                   "Use this when the user references a past project, makes ambiguous "
                   "references like 'the last one', 'that thing we discussed', or uses "
                   "acronyms/terms that could have multiple meanings. The agent should "
                   "NEVER guess — query the memory system to verify context before acting.",
    "parameters": {
        "type": "object",
        "properties": {
            "query": {
                "type": "string",
                "description": "Search query. Use specific keywords: project names, "
                               "technology stack, user handle, or exact phrases from past sessions."
            },
            "limit": {
                "type": "integer",
                "description": "Maximum number of results to return",
                "default": 5,
                "minimum": 1,
                "maximum": 20
            }
        },
        "required": ["query"]
    },
    "strict": True
}


SAVE_TO_MEMORY_SCHEMA = {
    "name": "save_to_memory",
    "description": "Permanently save an atomic fact, architecture decision, or learned "
                   "lesson to OpenClaw's persistent memory layer. The agent should call "
                   "this IMMEDIATELY when: the user states a preference, makes an "
                   "architecture decision, reveals a constraint, or when the agent "
                   "learns something valuable that should persist across sessions.",
    "parameters": {
        "type": "object",
        "properties": {
            "fact": {
                "type": "string",
                "description": "The atomic fact or lesson to save. Be concise and specific. "
                               "Prefer patterns like: 'User prefers X', 'Project uses Y for Z', "
                               "'Architecture decision: use A over B because C'."
            },
            "tags": {
                "type": "array",
                "items": {"type": "string"},
                "description": "Categorizing tags. Use lowercase with hyphens. "
                               "Examples: 'preferences', 'architecture', 'security', 'golang'",
                "maxItems": 3,
                "default": []
            },
            "weight": {
                "type": "integer",
                "description": "Memory weight. Set >= 10 for long-term retention (LTM). "
                               "Set > 10 for critical facts (e.g., user identity, security rules).",
                "default": 1,
                "minimum": 0,
                "maximum": 100
            },
            "ttl": {
                "type": "string",
                "description": "Optional TTL for ephemeral facts. Examples: '24h', '7d', '30d'. "
                               "Omit for permanent memories.",
            }
        },
        "required": ["fact"]
    },
    "strict": True
}


# Registry for all MPM tools
MPM_TOOL_SCHEMAS = [
    QUERY_LONG_TERM_MEMORY_SCHEMA,
    SAVE_TO_MEMORY_SCHEMA,
]


# =============================================================================
# System Prompt Directives
# =============================================================================

MPM_SYSTEM_PROMPT_DIRECTIVES = """
## MPM Memory Integration Directives

### Mandatory Recall Rule
You MUST call `query_long_term_memory` to verify context before acting on any:
- References to past projects, files, or decisions ("the project we built", "that API")
- Ambiguous pronouns ("the last one", "it", "that thing")
- Acronyms or shorthand without prior definition in the current conversation
- User preferences that may have been stated in previous sessions

**NEVER hallucinate past context.** If you're unsure whether something exists in memory, query it.
If `query_long_term_memory` returns no results, proceed cautiously and note the uncertainty.

### Mandatory Save Rule
Call `save_to_memory` IMMEDIATELY when the user:
- States a preference ("I prefer X over Y", "always use Z for this")
- Makes an architecture or technical decision ("we're using PostgreSQL", "the API goes here")
- Reveals a constraint ("we can't use external APIs", "it must work offline")
- Provides a project name, handle, or identifier for future reference

Also save proactively when:
- You learn something valuable about the codebase through debugging or exploration
- You discover a pattern that should be preserved (e.g., "the migration script must run before deployment")
- The user confirms something important with explicit acknowledgment

### Fact Format for `save_to_memory`
Format memories as atomic, search-friendly statements:
- GOOD: "User prefers TypeScript over JavaScript for new services"
- BAD:  "User talked about TypeScript preferences"
- GOOD: "Architecture: PostgreSQL for transactional data, Redis for cache"
- BAD:  "Postgres and redis are used somehow"

### Tag Taxonomy
Use consistent, lowercase tags with hyphens:
- `preferences` — user-stated preferences
- `architecture` — structural decisions  
- `security` — security-related rules or constraints
- `workflow` — process-related knowledge
- `tech-stack` — technology choices
"""