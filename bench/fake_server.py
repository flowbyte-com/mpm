"""Fake OpenAI-compatible embedding server for hermetic testing.

This module exposes a small HTTP server that responds to
`POST /v1/embeddings` with a deterministic embedding for the input.
The server uses a content-hash-derived vector so the same input
always maps to the same vector. Vectors are L2-normalized so cosine
similarity is well-behaved.

The harness uses this server in --smoke and --selftest modes so
the MPM subprocess can embed queries without making real network
calls. The server is bound to 127.0.0.1 and only listens for the
duration of the test.

This is gate 2 of HAL's implementation gates: the hermetic smoke
test must not use a disabled provider or rely on a hash fallback;
it must use a fake provider or controlled vector fixtures, with
no real Ollama or external network.
"""

from __future__ import annotations

import hashlib
import json
import math
import struct
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Callable


def deterministic_vector(text: str, dim: int) -> list[float]:
    """Hash the text, expand to `dim` floats in [-1, 1], then L2-normalize.
    The same text always produces the same vector; similar texts (sharing
    many tokens) will share more of the hash-space but the result is not
    semantically meaningful. For the hermetic test, that's fine: we are
    verifying the harness, not the embedding model.

    The hash is SHA-256 of the UTF-8 text; we expand to `dim` by chaining
    SHA-256 outputs until we have enough bytes. Random SHA-256 bytes
    interpreted as float32 will produce NaN/Inf in roughly 0.4% of slots
    (exponent byte 0xff), which is invalid JSON and which Go's strict
    decoder refuses. We replace non-finite values with 0.0 before
    returning so the response is always valid JSON.
    """
    needed_bytes = dim * 4  # float32
    chunks: list[bytes] = []
    counter = 0
    while sum(len(c) for c in chunks) < needed_bytes:
        h = hashlib.sha256(f"{counter}:{text}".encode("utf-8")).digest()
        chunks.append(h)
        counter += 1
    raw = b"".join(chunks)[:needed_bytes]
    floats = list(struct.unpack(f"<{dim}f", raw))
    # Replace NaN / +-Inf with 0.0 so the JSON encoder produces valid
    # output. The L2 norm absorbs these as zero contributions.
    floats = [0.0 if not math.isfinite(x) else x for x in floats]
    norm = math.sqrt(sum(x * x for x in floats))
    if norm == 0.0:
        return floats
    return [x / norm for x in floats]


class FakeEmbeddingsHandler(BaseHTTPRequestHandler):
    """Responds to /v1/embeddings with deterministic vectors.

    The `model_name` and `dim` are class-level set by `make_server`.
    The `vector_fn` class attribute lets tests override the vector
    function (e.g. to inject controlled fixtures).

    `api_show_reply` mirrors Ollama's `POST /api/show` model-metadata
    endpoint so provenance capture can be tested without a real
    provider. When it is None the endpoint is not served.
    """

    model_name: str = "fake-embedding-v1"
    dim: int = 64
    vector_fn: Callable[[str, int], list[float]] = staticmethod(deterministic_vector)
    api_show_reply: dict | None = None
    api_tags_reply: dict | None = None

    def log_message(self, format: str, *args: object) -> None:  # noqa: A002
        # Silence default access logging; the harness records its own.
        return

    def _send_json(self, payload: dict) -> None:
        body_out = json.dumps(payload).encode("utf-8")
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body_out)))
        self.end_headers()
        self.wfile.write(body_out)

    def do_GET(self) -> None:  # noqa: N802
        if self.path.rstrip("/").endswith("/api/tags"):
            if self.api_tags_reply is None:
                self.send_error(404, f"unknown path {self.path}")
                return
            self._send_json(self.api_tags_reply)
            return
        self.send_error(404, f"unknown path {self.path}")

    def do_POST(self) -> None:  # noqa: N802
        if self.path.rstrip("/").endswith("/api/show"):
            if self.api_show_reply is None:
                self.send_error(404, f"unknown path {self.path}")
                return
            self._send_json(self.api_show_reply)
            return
        if not self.path.rstrip("/").endswith("/v1/embeddings"):
            self.send_error(404, f"unknown path {self.path}")
            return
        length = int(self.headers.get("Content-Length", "0"))
        body = self.rfile.read(length) if length > 0 else b""
        try:
            payload = json.loads(body)
        except json.JSONDecodeError:
            self.send_error(400, "invalid JSON")
            return
        inputs = payload.get("input")
        if isinstance(inputs, str):
            inputs = [inputs]
        if not isinstance(inputs, list) or not inputs:
            self.send_error(400, "input must be a string or non-empty list")
            return

        data = []
        for i, text in enumerate(inputs):
            vec = self.vector_fn(text, self.dim)
            data.append({"embedding": vec, "index": i, "object": "embedding"})
        resp = {"object": "list", "data": data, "model": self.model_name}
        body_out = json.dumps(resp).encode("utf-8")
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body_out)))
        self.end_headers()
        self.wfile.write(body_out)


def make_server(
    host: str = "127.0.0.1",
    port: int = 0,
    dim: int = 64,
    model_name: str = "fake-embedding-v1",
    vector_fn: Callable[[str, int], list[float]] | None = None,
    api_show_reply: dict | None = None,
    api_tags_reply: dict | None = None,
) -> tuple[ThreadingHTTPServer, str, int]:
    """Start a fake embeddings server. Returns (server, base_url, port).
    The server runs in a background thread; the caller is responsible
    for calling `server.shutdown()` when done.

    The returned base_url includes `/v1` so the OpenAI-compatible
    provider can append `/embeddings` and get a valid URL. The
    handler itself also accepts the un-versioned `/embeddings` path
    to match the live OpenAI-compatible behaviour.

    `api_show_reply` and `api_tags_reply`, when given, are served
    from `POST /api/show` and `GET /api/tags` (Ollama's model
    metadata shapes) so the harness's provenance capture can be
    exercised hermetically.
    """
    FakeEmbeddingsHandler.model_name = model_name
    FakeEmbeddingsHandler.dim = dim
    FakeEmbeddingsHandler.api_show_reply = api_show_reply
    FakeEmbeddingsHandler.api_tags_reply = api_tags_reply
    if vector_fn is not None:
        FakeEmbeddingsHandler.vector_fn = staticmethod(vector_fn)

    server = ThreadingHTTPServer((host, port), FakeEmbeddingsHandler)
    bound_port = server.server_address[1]
    base_url = f"http://{host}:{bound_port}/v1"
    t = threading.Thread(target=server.serve_forever, daemon=True)
    t.start()
    return server, base_url, bound_port
