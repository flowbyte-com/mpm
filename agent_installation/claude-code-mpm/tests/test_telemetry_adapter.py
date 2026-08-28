import os
import sys
sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "src"))

import json
import socket
import tempfile
import threading
import time
import unittest

from telemetry_adapter import TelemetryEmitter, _frame_from_response


class FakeUsage:
    def __init__(self, input_tokens=100, output_tokens=50,
                 cache_read_tokens=None, cache_write_tokens=200,
                 reasoning_tokens=0):
        self.input_tokens = input_tokens
        self.output_tokens = output_tokens
        self.cache_read_tokens = cache_read_tokens
        self.cache_write_tokens = cache_write_tokens
        self.reasoning_tokens = reasoning_tokens


class FakeResponse:
    def __init__(self, usage):
        self.usage = usage
        self.stop_reason = "end_turn"


class FrameFromResponseTests(unittest.TestCase):
    def test_basic_fields(self):
        f = _frame_from_response(
            invocation_id="inv_1", session_id="sess_1",
            framework="claude-code", framework_version="1.0",
            provider="anthropic", model="claude-fable-5",
            started_at=1756000000, completed_at=1756000012,
            response=FakeResponse(FakeUsage()),
        )
        self.assertEqual(f["invocation_id"], "inv_1")
        self.assertEqual(f["input_tokens"], 100)
        self.assertIsNone(f["cache_read_tokens"])
        self.assertEqual(f["status"], "completed")
        self.assertEqual(f["schema_version"], "v1")

    def test_null_tokens_remain_null(self):
        f = _frame_from_response(
            invocation_id="inv_2", session_id="sess_2",
            framework="claude-code", framework_version="1.0",
            provider="anthropic", model="claude-fable-5",
            started_at=1756000000, completed_at=1756000012,
            response=FakeResponse(FakeUsage(cache_read_tokens=0)),
        )
        # explicitly 0 (reported) must persist as 0, not None
        self.assertEqual(f["cache_read_tokens"], 0)


class EmitterSendTests(unittest.TestCase):
    def test_emitter_does_not_block(self):
        # Start a fake collector that accepts and never replies.
        tmp = tempfile.mkdtemp()
        sock_path = os.path.join(tmp, "test.sock")
        srv = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        srv.bind(sock_path)
        srv.listen(1)

        def accept_loop():
            conn, _ = srv.accept()
            # read all frames; do not reply
            while True:
                data = conn.recv(65536)
                if not data:
                    break

        t = threading.Thread(target=accept_loop, daemon=True)
        t.start()

        emitter = TelemetryEmitter(socket_path=sock_path)
        start = time.monotonic()
        emitter.record_invocation(
            invocation_id="inv_x", session_id="sess_x",
            framework="claude-code", framework_version="1.0",
            provider="anthropic", model="claude-fable-5",
            started_at=1, completed_at=2,
            response=FakeResponse(FakeUsage()),
        )
        elapsed = time.monotonic() - start
        self.assertLess(elapsed, 0.05, "record_invocation must return immediately")
        emitter.shutdown()
        srv.close()


if __name__ == "__main__":
    unittest.main()
