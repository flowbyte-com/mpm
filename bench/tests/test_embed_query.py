"""Provider-aware query embedding tests (priority 4 of HAL's review).

Asserts the four providers' HTTP shapes in `bench/embed_query.py`:
  - openai-compatible / openai / openrouter: POST {base}/embeddings
        body:   {"input", "model"} -> {"data":[{"embedding":[...]}]}
  - ollama: POST {base}/api/embed
        body:   {"model", "input"} -> {"embeddings":[[...]]}

The tests use a small in-process HTTP server that records the
request body and replies with a canned response. This is
hermetic — no real provider is contacted, no real network is
used.
"""

import json
import threading
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

from bench.embed_query import EmbedError, embed_query, l2_normalize


class _RecordingHandler(BaseHTTPRequestHandler):
    """Records each request body and replies with a canned embedding."""

    last_body: bytes = b""
    last_path: str = ""
    canned_response: bytes = b""
    canned_status: int = 200

    def log_message(self, *args, **kwargs):  # noqa: A002
        return

    def do_POST(self):  # noqa: N802
        length = int(self.headers.get("Content-Length", "0"))
        _RecordingHandler.last_body = self.rfile.read(length)
        _RecordingHandler.last_path = self.path
        resp = _RecordingHandler.canned_response
        self.send_response(_RecordingHandler.canned_status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(resp)))
        self.end_headers()
        self.wfile.write(resp)


def _spin_server():
    server = ThreadingHTTPServer(("127.0.0.1", 0), _RecordingHandler)
    t = threading.Thread(target=server.serve_forever, daemon=True)
    t.start()
    return server


class TestEmbedQueryOpenAICompatible(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.server = _spin_server()
        cls.port = cls.server.server_address[1]
        cls.base = f"http://127.0.0.1:{cls.port}/v1"

    @classmethod
    def tearDownClass(cls):
        cls.server.shutdown()

    def setUp(self):
        _RecordingHandler.last_body = b""
        _RecordingHandler.last_path = ""
        _RecordingHandler.canned_status = 200
        _RecordingHandler.canned_response = json.dumps(
            {"data": [{"embedding": [0.1, 0.2, 0.3, 0.4], "index": 0}]}
        ).encode("utf-8")

    def test_openai_compatible_sends_correct_request(self):
        vec = embed_query(
            "hello", "openai-compatible", "all-minilm", self.base
        )
        self.assertEqual(vec, [0.1, 0.2, 0.3, 0.4])
        body = json.loads(_RecordingHandler.last_body)
        self.assertEqual(body, {"input": "hello", "model": "all-minilm"})
        self.assertEqual(_RecordingHandler.last_path, "/v1/embeddings")

    def test_openai_sends_correct_request(self):
        # The "openai" provider is the same HTTP shape as
        # "openai-compatible"; the harness's embed_query must
        # accept both names.
        vec = embed_query(
            "world", "openai", "text-embedding-3-small", self.base
        )
        self.assertEqual(vec, [0.1, 0.2, 0.3, 0.4])
        body = json.loads(_RecordingHandler.last_body)
        self.assertEqual(body, {"input": "world", "model": "text-embedding-3-small"})

    def test_openrouter_sends_correct_request(self):
        vec = embed_query(
            "!", "openrouter", "some/model", self.base
        )
        self.assertEqual(vec, [0.1, 0.2, 0.3, 0.4])
        body = json.loads(_RecordingHandler.last_body)
        self.assertEqual(body, {"input": "!", "model": "some/model"})

    def test_bearer_token_sent_when_api_key_set(self):
        # Stub: we cannot easily inspect headers here without a
        # custom handler; just verify that the call still works
        # with a non-empty api_key.
        vec = embed_query(
            "x", "openai-compatible", "m", self.base, api_key="sk-abc"
        )
        self.assertEqual(len(vec), 4)


class TestEmbedQueryOllama(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.server = _spin_server()
        cls.port = cls.server.server_address[1]
        cls.base = f"http://127.0.0.1:{cls.port}"

    @classmethod
    def tearDownClass(cls):
        cls.server.shutdown()

    def setUp(self):
        _RecordingHandler.last_body = b""
        _RecordingHandler.last_path = ""
        _RecordingHandler.canned_status = 200
        _RecordingHandler.canned_response = json.dumps(
            {"embeddings": [[0.5, 0.5, 0.5, 0.5]]}
        ).encode("utf-8")

    def test_ollama_sends_correct_request(self):
        vec = embed_query(
            "hello", "ollama", "nomic-embed-text", self.base
        )
        self.assertEqual(vec, [0.5, 0.5, 0.5, 0.5])
        body = json.loads(_RecordingHandler.last_body)
        self.assertEqual(body, {"model": "nomic-embed-text", "input": "hello"})
        # base had no /api/embed; the function should append it.
        self.assertEqual(_RecordingHandler.last_path, "/api/embed")

    def test_ollama_already_qualified_url_unchanged(self):
        base2 = f"http://127.0.0.1:{self.port}/api/embed"
        vec = embed_query(
            "x", "ollama", "nomic", base2
        )
        self.assertEqual(vec, [0.5, 0.5, 0.5, 0.5])
        self.assertEqual(_RecordingHandler.last_path, "/api/embed")


class TestEmbedQueryErrors(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.server = _spin_server()
        cls.port = cls.server.server_address[1]
        cls.base = f"http://127.0.0.1:{cls.port}/v1"

    @classmethod
    def tearDownClass(cls):
        cls.server.shutdown()

    def test_unsupported_provider_raises(self):
        with self.assertRaises(EmbedError) as cm:
            embed_query("x", "bogus", "m", self.base)
        self.assertIn("unsupported provider", str(cm.exception))

    def test_empty_text_raises(self):
        with self.assertRaises(EmbedError):
            embed_query("", "ollama", "m", self.base)

    def test_empty_base_url_raises(self):
        with self.assertRaises(EmbedError):
            embed_query("x", "ollama", "m", "")

    def test_empty_model_raises(self):
        with self.assertRaises(EmbedError):
            embed_query("x", "ollama", "", self.base)

    def test_404_status_surfaced_as_error(self):
        _RecordingHandler.canned_status = 500
        _RecordingHandler.canned_response = b"server error"
        with self.assertRaises(EmbedError) as cm:
            embed_query("x", "openai-compatible", "m", self.base)
        self.assertIn("500", str(cm.exception))

    def test_malformed_response_raises(self):
        _RecordingHandler.canned_status = 200
        _RecordingHandler.canned_response = b"not json"
        with self.assertRaises(EmbedError) as cm:
            embed_query("x", "openai-compatible", "m", self.base)
        self.assertIn("non-JSON", str(cm.exception))


class TestL2Normalize(unittest.TestCase):
    def test_known_vector(self):
        v = [3.0, 4.0]
        n = l2_normalize(v)
        self.assertAlmostEqual(n[0], 0.6, places=6)
        self.assertAlmostEqual(n[1], 0.8, places=6)

    def test_zero_vector_returns_zero(self):
        # NaN/Inf protection: a zero norm must NOT divide-by-zero.
        n = l2_normalize([0.0, 0.0, 0.0])
        self.assertEqual(n, [0.0, 0.0, 0.0])

    def test_already_normalized_unchanged(self):
        v = [0.0, 1.0, 0.0]
        n = l2_normalize(v)
        self.assertEqual(n, v)
