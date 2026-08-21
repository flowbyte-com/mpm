"""telemetry_adapter — async, non-blocking NDJSON sender for mpm-telemetry.

Wraps each LLM invocation into one v1 telemetry frame and pushes it to
the collector over its Unix socket. The collector's availability must
NEVER affect agent correctness (spec invariant 1), so:

- record_invocation() returns immediately. The actual socket send runs
  in a background daemon thread.
- A per-process ring buffer (capacity 1000) holds frames while the
  socket is unavailable; oldest frames are evicted on overflow.
- A 100ms connect timeout means a missing collector adds <100ms to a
  retry, then drops the frame.

This plugin ships 16 native MCP tools to Claude Code; it is a shell-out
wrapper that proxies JSON-RPC to mpm-mcp. The LLM call site is in
Claude Code's own runtime, not here. The adapter is therefore shipped
as a reusable library; future consumers (custom Claude Code wrappers,
mpm-agent hooks) import it and call TelemetryEmitter.record_invocation(...)
after their own LLM call.

Public surface:
    TelemetryEmitter(socket_path=...).record_invocation(...)
    TelemetryEmitter.shutdown()

See docs/superpowers/specs/2026-08-21-telemetry-binary-design.md §3, §4.
"""

import json
import os
import queue
import socket
import threading
import uuid
from typing import Any, Optional


SCHEMA_VERSION = "v1"
RING_CAPACITY = 1000
CONNECT_TIMEOUT_SEC = 0.1


def _frame_from_response(
    *,
    invocation_id: str,
    session_id: Optional[str],
    framework: str,
    framework_version: Optional[str],
    provider: str,
    model: str,
    model_revision: Optional[str] = None,
    started_at: int,
    completed_at: int,
    response: Any,
) -> dict:
    """Build a v1 frame from an LLM response object."""
    usage = response.usage
    stop_reason = getattr(response, "stop_reason", None)
    duration_ms = max(0, (completed_at - started_at) * 1000)
    return {
        "schema_version": SCHEMA_VERSION,
        "event_type": "invocation_completed",
        "invocation_id": invocation_id,
        "parent_invocation_id": None,
        "session_id": session_id,
        "framework": framework,
        "framework_version": framework_version,
        "provider": provider,
        "model": model,
        "model_revision": model_revision,
        "started_at": started_at,
        "completed_at": completed_at,
        "status": "completed",
        "stop_reason": stop_reason,
        "input_tokens": getattr(usage, "input_tokens", None),
        "output_tokens": getattr(usage, "output_tokens", None),
        "cache_read_tokens": getattr(usage, "cache_read_tokens", None),
        "cache_write_tokens": getattr(usage, "cache_write_tokens", None),
        "reasoning_tokens": getattr(usage, "reasoning_tokens", None),
        "duration_ms": duration_ms,
        "provider_metadata": {},
    }


def _socket_path_from_env() -> str:
    override = os.environ.get("MPM_TELEMETRY_SOCKET")
    if override:
        return override
    workspace = os.environ.get("MPM_WORKSPACE")
    if not workspace:
        # Adapter is best-effort; missing workspace means telemetry is disabled.
        return ""
    return os.path.join(workspace, "runtime", "mpm-telemetry.sock")


class TelemetryEmitter:
    def __init__(self, socket_path: Optional[str] = None):
        self.socket_path = socket_path or _socket_path_from_env()
        self._queue: "queue.Queue[dict]" = queue.Queue(maxsize=RING_CAPACITY)
        self._dropped = 0
        self._accepted = 0
        self._rejected = 0
        self._stop = threading.Event()
        self._thread = threading.Thread(target=self._drain, daemon=True)
        self._thread.start()

    def record_invocation(
        self,
        *,
        invocation_id: Optional[str] = None,
        session_id: Optional[str] = None,
        framework: str,
        framework_version: Optional[str] = None,
        provider: str,
        model: str,
        model_revision: Optional[str] = None,
        started_at: int,
        completed_at: int,
        response: Any,
    ) -> None:
        """Capture one LLM invocation. Returns immediately.

        If no invocation_id is provided, generates a UUIDv7-style string.
        Token fields default to None (not reported), preserving the
        spec invariant 4 (NULL != 0).
        """
        if not self.socket_path:
            return  # telemetry disabled (no workspace / socket path)
        if invocation_id is None:
            invocation_id = "inv_" + uuid.uuid4().hex
        frame = _frame_from_response(
            invocation_id=invocation_id, session_id=session_id,
            framework=framework, framework_version=framework_version,
            provider=provider, model=model, model_revision=model_revision,
            started_at=started_at, completed_at=completed_at,
            response=response,
        )
        try:
            self._queue.put_nowait(frame)
        except queue.Full:
            # Evict the oldest frame to make room.
            try:
                self._queue.get_nowait()
            except queue.Empty:
                pass
            try:
                self._queue.put_nowait(frame)
            except queue.Full:
                self._dropped += 1

    def shutdown(self, timeout: float = 2.0) -> None:
        self._stop.set()
        self._thread.join(timeout=timeout)

    @property
    def counters(self) -> dict:
        return {
            "accepted": self._accepted,
            "rejected": self._rejected,
            "dropped": self._dropped,
            "queue_depth": self._queue.qsize(),
        }

    def _drain(self) -> None:
        while not self._stop.is_set():
            try:
                frame = self._queue.get(timeout=0.5)
            except queue.Empty:
                continue
            self._send_one(frame)

    def _send_one(self, frame: dict) -> None:
        if not self.socket_path:
            return
        try:
            with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as s:
                s.settimeout(CONNECT_TIMEOUT_SEC)
                s.connect(self.socket_path)
                line = (json.dumps(frame) + "\n").encode("utf-8")
                s.sendall(line)
                # Read the response (truthful — see spec §5).
                buf = b""
                while not buf.endswith(b"\n"):
                    chunk = s.recv(4096)
                    if not chunk:
                        break
                    buf += chunk
                try:
                    resp = json.loads(buf.decode("utf-8").strip() or "{}")
                except json.JSONDecodeError:
                    self._dropped += 1
                    return
                status = resp.get("status")
                if status == "ACCEPTED":
                    self._accepted += 1
                elif status == "REJECTED":
                    self._rejected += 1
                else:
                    self._dropped += 1
        except (socket.error, OSError):
            self._dropped += 1
