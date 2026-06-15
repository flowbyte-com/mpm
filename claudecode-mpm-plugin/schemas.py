"""Pydantic input models for MPM MCP tools.

These mirror the opencode-mpm-plugin schemas. Pydantic generates a
strict JSON Schema for Claude Code's tool-use planner, preventing
the planner from inventing default values for optional fields.
"""
from typing import Optional

from pydantic import BaseModel, Field


class ReadWakeContextInput(BaseModel):
    """No required args. timeout_override_ms is for slow-loading sessions."""

    timeout_override_ms: Optional[int] = Field(
        None, description="Per-call timeout in ms; defaults to 15000 (15s)."
    )


class QueryLongTermMemoryInput(BaseModel):
    """Search MPM long-term memory. Run before answering any question
    about prior work, decisions, dates, people, preferences, or todos."""

    query: str = Field(
        ..., min_length=1, description="Natural language search query for long-term memory."
    )
    limit: int = Field(
        5, ge=1, le=100, description="Maximum number of results to return."
    )
    timeout_override_ms: Optional[int] = Field(
        None, description="Per-call timeout in ms; defaults to 15000 (15s)."
    )
