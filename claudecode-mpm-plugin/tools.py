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


def format_age(created_at: str) -> str:
    """Return a human-readable age string for a timestamp.

    Handles both ISO-8601 with 'T' separator and the mpm SQLite
    'YYYY-MM-DD HH:MM:SS' format. Returns 'unknown age' for empty
    or unparseable input.
    """
    if not created_at:
        return "unknown age"
    try:
        # Try the SQLite format first (most common in mpm output).
        try:
            created = datetime.strptime(created_at, "%Y-%m-%d %H:%M:%S")
        except ValueError:
            created = datetime.fromisoformat(created_at.replace("Z", "+00:00"))
        # Subtract naive 'now' from parsed datetime; tz-aware input would TypeError here.
        delta = datetime.utcnow() - created
    except (ValueError, TypeError):
        return "unknown age"
    seconds = int(delta.total_seconds())
    if seconds < 0:
        return "just now"
    if seconds < 60:
        return f"{seconds}s ago"
    minutes = seconds // 60
    if minutes < 60:
        return f"{minutes}m ago"
    hours = minutes // 60
    if hours < 24:
        return f"{hours}h ago"
    days = hours // 24
    if days < 30:
        return f"{days}d ago"
    weeks = days // 7
    if weeks < 12:
        return f"{weeks}w ago"
    months = days // 30
    return f"{months}mo ago"


def debug_log(message: str) -> None:
    """Append a timestamped line to $MPM_DEBUG_LOG when DEBUG=1.

    Default log path: $MPM_DEBUG_LOG or .claude/debug.log.
    Never raises — best-effort observability only.
    """
    if os.environ.get("DEBUG") != "1":
        return
    log_path = os.environ.get("MPM_DEBUG_LOG", ".claude/debug.log")
    timestamp = datetime.utcnow().isoformat(timespec="seconds")
    line = f"{timestamp} {message}\n"
    try:
        # Append; create parent dir if missing.
        parent = os.path.dirname(log_path)
        if parent:
            os.makedirs(parent, exist_ok=True)
        with open(log_path, "a", encoding="utf-8") as f:
            f.write(line)
    except OSError:
        # Observability must never crash the caller.
        pass


async def run_mpm(
    args: list[str],
    timeout_ms: Optional[int] = None,
) -> MpmRunResult:
    """Spawn the mpm binary with the given args and return its output.

    Clamps the effective timeout to MAX_TIMEOUT_MS (5 minutes) regardless
    of the user-supplied timeout_override_ms, to prevent runaway LLM
    synthesis from hanging the MCP session.

    Drains stdout and stderr concurrently via proc.communicate() to
    avoid the classic stderr-buffer deadlock.

    Returns MpmRunResult with exit_code=124 on timeout and exit_code=1
    with a clean stderr message on a missing binary.
    """
    effective_ms = min(MAX_TIMEOUT_MS, timeout_ms if timeout_ms is not None else DEFAULT_TIMEOUT_MS)
    effective_seconds = effective_ms / 1000.0

    mpm_bin = os.environ.get("MPM_BINARY", "mpm")
    cmd = [mpm_bin, *args]
    debug_log(f"run_mpm argv: {cmd} timeout_ms={effective_ms}")

    try:
        proc = await asyncio.create_subprocess_exec(
            *cmd,
            stdout=asyncio.subprocess.PIPE,
            stderr=asyncio.subprocess.PIPE,
        )
    except FileNotFoundError:
        debug_log(f"run_mpm: binary not found: {mpm_bin}")
        return MpmRunResult(
            exit_code=1,
            stdout="",
            stderr=f"[spawn error] mpm binary not found: {mpm_bin}",
        )

    try:
        stdout_bytes, stderr_bytes = await asyncio.wait_for(
            proc.communicate(), timeout=effective_seconds
        )
    except asyncio.TimeoutError:
        try:
            proc.kill()
        except ProcessLookupError:
            pass
        debug_log(f"run_mpm: timeout after {effective_ms}ms")
        return MpmRunResult(
            exit_code=124,
            stdout="",
            stderr=f"Operation timed out after {effective_ms}ms",
        )

    return MpmRunResult(
        exit_code=proc.returncode if proc.returncode is not None else 1,
        stdout=stdout_bytes.decode("utf-8", errors="replace"),
        stderr=stderr_bytes.decode("utf-8", errors="replace"),
    )
