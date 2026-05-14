"""
Synchronous MPM v2.0 tool wrappers for OpenClaw.
Uses subprocess to interface with the mpm binary directly.
"""

from __future__ import annotations

import json
import subprocess
import logging
from dataclasses import dataclass
from typing import Any

logger = logging.getLogger(__name__)


@dataclass
class MPMQueryResult:
    """Result of a memory recall query."""
    success: bool
    memories: list[dict[str, Any]]
    error: str | None = None


@dataclass
class MPMSaveResult:
    """Result of saving a memory."""
    success: bool
    memory_id: str | None = None
    error: str | None = None


class MPMExecutionError(Exception):
    """Raised when mpm binary returns a non-zero exit code."""
    def __init__(self, message: str, returncode: int, stderr: str = ""):
        super().__init__(message)
        self.returncode = returncode
        self.stderr = stderr


def execute_mpm_query(query: str, limit: int = 10) -> MPMQueryResult:
    """
    Execute mpm recall to search long-term memory.
    
    Args:
        query: Search string/keywords
        limit: Maximum results to return
    
    Returns:
        MPMQueryResult with memories list or error
    
    Raises:
        MPMExecutionError: If mpm binary is not found or not executable
    """
    cmd = ["mpm", "recall", query, "--limit", str(limit), "--json"]
    
    try:
        result = subprocess.run(
            cmd,
            capture_output=True,
            text=True,
            timeout=30,
        )
    except FileNotFoundError:
        return MPMQueryResult(
            success=False,
            memories=[],
            error="MPM binary not found in PATH"
        )
    except subprocess.TimeoutExpired:
        return MPMQueryResult(
            success=False,
            memories=[],
            error="MPM query timed out after 30 seconds"
        )
    
    if result.returncode != 0:
        return MPMQueryResult(
            success=False,
            memories=[],
            error=f"mpm recall failed: {result.stderr.strip()}"
        )
    
    try:
        # Handle case where mpm outputs multiple JSON lines (e.g., progress + result)
        first_line = result.stdout.strip().split("\n")[0]
        data = json.loads(first_line)
        return MPMQueryResult(
            success=True,
            memories=data.get("memories", []) if isinstance(data, dict) else data,
        )
    except json.JSONDecodeError as e:
        return MPMQueryResult(
            success=False,
            memories=[],
            error=f"Failed to parse mpm JSON output: {e}"
        )


def execute_mpm_save(
    fact: str,
    tags: list[str] | None = None,
    weight: int = 1,
    ttl: str | None = None,
) -> MPMSaveResult:
    """
    Execute mpm add to save an atomic fact to memory.
    
    Args:
        fact: The memory content to save
        tags: List of tags (up to 3 recommended)
        weight: Memory weight (default 1, LTM if >= 10)
        ttl: Optional TTL string (e.g., "7d", "30d")
    
    Returns:
        MPMSaveResult with memory_id on success
    
    Raises:
        MPMExecutionError: If mpm binary is not found or not executable
    """
    if tags is None:
        tags = []
    
    # Build command with up to 3 tags
    cmd = ["mpm", "add", fact, "--json"]
    
    for tag in tags[:3]:
        cmd.extend(["--tag", tag])
    
    if weight != 1:
        cmd.extend(["--weight", str(weight)])
    
    if ttl:
        cmd.extend(["--ttl", ttl])
    
    try:
        result = subprocess.run(
            cmd,
            capture_output=True,
            text=True,
            timeout=15,
        )
    except FileNotFoundError:
        return MPMSaveResult(
            success=False,
            error="MPM binary not found in PATH"
        )
    except subprocess.TimeoutExpired:
        return MPMSaveResult(
            success=False,
            error="MPM add timed out after 15 seconds"
        )
    
    if result.returncode != 0:
        return MPMSaveResult(
            success=False,
            error=f"mpm add failed: {result.stderr.strip()}"
        )
    
    try:
        first_line = result.stdout.strip().split("\n")[0]
        data = json.loads(first_line)
        return MPMSaveResult(
            success=True,
            memory_id=data.get("id") or data.get("memory_id"),
        )
    except json.JSONDecodeError as e:
        return MPMSaveResult(
            success=False,
            error=f"Failed to parse mpm response: {e}"
        )