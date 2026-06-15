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
    On error: structured error dict with semantic error codes.
    """
    stderr = result.stderr or ""
    stderr_lc = stderr.lower()

    # Output-overflow: subprocess was killed because stdout exceeded MAX_BUFFER.
    if result.exit_code == 125 and "[output exceeded" in stderr:
        return {
            "error": "wake_context_truncated",
            "message": "Wake context exceeded buffer limit — session too large.",
        }

    # SQLite BUSY: contention on the mpm database. Retryable.
    # Check lowercase to catch mixed-case variants ("SQLITE_BUSY", "sqlite_busy", "Sqlite_Busy", etc.).
    if "database is locked" in stderr_lc or "sqlite_busy" in stderr_lc:
        return {
            "error": "database_locked",
            "message": "MPM database is locked — safe to retry.",
        }

    # Generic non-zero exit.
    if result.exit_code != 0:
        first_line = stderr.split("\n")[0] if stderr else f"mpm exited with code {result.exit_code}"
        return {"error": f"exit_{result.exit_code}", "message": first_line}

    # Success: parse JSON with text fallback.
    trimmed = result.stdout.strip()
    if not trimmed:
        return {"id": "", "success": True, "count": 0}

    try:
        return json.loads(trimmed)
    except json.JSONDecodeError:
        return {"id": "", "success": True, "text": trimmed}
