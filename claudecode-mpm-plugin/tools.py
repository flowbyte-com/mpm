"""Subprocess plumbing + helpers for the MPM MCP server."""
import asyncio
import json
import os
import time
from dataclasses import dataclass
from datetime import datetime
from typing import Optional


# Hard cap on subprocess runtime. User-supplied timeouts are clamped to this.
MAX_TIMEOUT_MS = 5 * 60 * 1000  # 5 minutes
DEFAULT_TIMEOUT_MS = 15000  # 15 seconds

# Hard cap on per-stream buffer size. 10 MiB matches the opencode plugin.
MAX_BUFFER = 10 * 1024 * 1024


@dataclass
class MpmRunResult:
    """Result of spawning the mpm binary."""
    exit_code: int
    stdout: str
    stderr: str


def parse_mpm_result(result: MpmRunResult) -> dict:
    """Map an MpmRunResult to a structured dict.

    On success (exit 0): JSON.parse(stdout) with text fallback.
    On error: structured error dict (added in Task 5).
    """
    if result.exit_code != 0:
        # Error path implemented in Task 5.
        return {"error": f"exit_{result.exit_code}", "message": result.stderr.split("\n")[0]}

    trimmed = result.stdout.strip()
    if not trimmed:
        return {"id": "", "success": True, "count": 0}

    try:
        return json.loads(trimmed)
    except json.JSONDecodeError:
        return {"id": "", "success": True, "text": trimmed}
