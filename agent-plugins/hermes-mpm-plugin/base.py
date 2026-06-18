import subprocess
import json
import os

MPM_BINARY = os.environ.get("MPM_BINARY", "mpm")
MPM_WORKSPACE = os.environ.get("MPM_WORKSPACE", "")
MAX_BUFFER = 10 * 1024 * 1024


class MpmError(Exception):
    def __init__(self, message: str, exit_code: int = -1, stderr: str = ""):
        self.message = message
        self.exit_code = exit_code
        self.stderr = stderr
        super().__init__(self.message)


def _build_env():
    """Build env dict for mpm subprocess — injects MPM_WORKSPACE."""
    env = dict(os.environ)
    if MPM_WORKSPACE:
        env["MPM_WORKSPACE"] = MPM_WORKSPACE
    return env


def run_mpm(args: list[str], timeout: float = 15.0) -> dict:
    """Run an mpm command (legacy CLI args) and return parsed JSON output."""
    cmd = [MPM_BINARY] + args
    try:
        result = subprocess.run(
            cmd,
            capture_output=True,
            text=True,
            timeout=timeout,
            env=_build_env(),
        )
    except subprocess.TimeoutExpired:
        return {"error": "MPM engine timed out."}
    except FileNotFoundError:
        return {"error": f"MPM binary not found: {MPM_BINARY}"}

    if result.returncode != 0:
        stderr = (result.stderr or "").strip()
        if "database is locked" in stderr or "SQLITE_BUSY" in stderr:
            return {
                "success": False,
                "error": "database_locked",
                "message": "MPM database is locked — safe to retry.",
            }
        return {
            "success": False,
            "error": f"exit_{result.returncode}",
            "message": stderr.split("\n")[0] if stderr else f"MPM exited with code {result.returncode}",
        }

    trimmed = (result.stdout or "").strip()
    if not trimmed:
        return {"success": True, "count": 0}

    try:
        return json.loads(trimmed)
    except json.JSONDecodeError:
        return {"success": True, "text": trimmed}


def run_mpm_call(tool_name: str, payload: dict, timeout: float = 15.0) -> dict:
    """Universal router wrapper for the MPM engine.

    All 18 tools speak JSON via: mpm call <tool> --payload '<json>'
    Both Hermes and OpenClaw now use this interface — single source of truth.
    """
    try:
        payload_str = json.dumps(payload, ensure_ascii=False)
    except (TypeError, ValueError) as e:
        return {"success": False, "error": "json_encode_error", "message": str(e)}

    cmd = [MPM_BINARY, "call", tool_name, "--payload", payload_str]
    try:
        result = subprocess.run(
            cmd,
            capture_output=True,
            text=True,
            timeout=timeout,
            env=_build_env(),
        )
    except subprocess.TimeoutExpired:
        return {"success": False, "error": "timeout", "message": "MPM engine timed out."}
    except FileNotFoundError:
        return {"success": False, "error": "not_found", "message": f"MPM binary not found: {MPM_BINARY}"}

    if result.returncode != 0:
        stderr = (result.stderr or "").strip()
        if result.returncode == 125 and "[output exceeded" in stderr:
            return {
                "success": False,
                "error": "wake_context_truncated",
                "message": "Wake context exceeds buffer limit — session is too large.",
            }
        if "database is locked" in stderr or "SQLITE_BUSY" in stderr:
            return {
                "success": False,
                "error": "database_locked",
                "message": "MPM database is locked — safe to retry. The write was not made.",
            }
        return {
            "success": False,
            "error": f"exit_{result.returncode}",
            "message": stderr.split("\n")[0] if stderr else f"MPM exited with code {result.returncode}",
        }

    trimmed = (result.stdout or "").strip()
    if not trimmed:
        return {"success": True, "count": 0}

    try:
        return json.loads(trimmed)
    except json.JSONDecodeError:
        return {"success": True, "text": trimmed}