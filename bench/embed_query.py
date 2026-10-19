"""Provider-agnostic query embedding (priority 4 of HAL's review).

The raw-cosine experiment in `bench/runner.py:_raw_cosine_ranking`
previously used `deterministic_vector` to embed the query text.
`deterministic_vector` is a SHA-256-hash expansion to floats — the
same function the hermetic fake server uses, so it works for the
fake server. In a real run against Ollama or any other real
provider, the document vectors come from the provider, and the
query vector MUST also come from the same provider; comparing
SHA-256-hash vectors to provider vectors is a meaningless
"is the harness even running?" smoke, not a measurement.

This module provides `embed_query`, a small stdlib HTTP client
that calls the configured provider's embedding endpoint:

  - `openai-compatible` / `openai` / `openrouter`:
        POST {base_url}/embeddings
        body:   {"input": <text>, "model": <model>}
        reply:  {"data":[{"embedding":[...]}]}
  - `ollama`:
        POST {base_url}/api/embed
        body:   {"model": <model>, "input": <text>}
        reply:  {"embeddings":[[...]]}

This mirrors the production adapter behaviour at
`internal/core/embeddings.go:87-138` (openai-compatible) and
`:189-` (ollama). It is a deliberate, minimal re-implementation
of the two providers' HTTP shapes — NOT a call into the
`internal/core` package, because the harness is hermetic and
must not import Go code.

The output is the raw float vector. The caller is responsible
for L2-normalizing (so cosine and dot-product coincide) and for
matching the dimension declared in the embedding profile (the
harness asserts the returned length matches the snapshot's
declared dimension before ranking).
"""

from __future__ import annotations

import json
import urllib.error
import urllib.request
from typing import Any


class EmbedError(RuntimeError):
    """Raised when the configured provider fails to return an
    embedding. The message includes the provider, the URL, and
    a short body excerpt so an operator can triage."""


def _http_post_json(url: str, body: dict[str, Any], api_key: str) -> dict[str, Any]:
    """POST `body` as JSON to `url` with the (optional) bearer
    token. Returns the decoded JSON reply. Raises EmbedError on
    network or decode failure."""
    payload = json.dumps(body).encode("utf-8")
    req = urllib.request.Request(
        url,
        data=payload,
        method="POST",
        headers={"Content-Type": "application/json"},
    )
    if api_key:
        req.add_header("Authorization", f"Bearer {api_key}")
    try:
        with urllib.request.urlopen(req, timeout=30) as resp:  # noqa: S310
            raw = resp.read()
    except (urllib.error.URLError, urllib.error.HTTPError) as e:
        excerpt = ""
        if hasattr(e, "read"):
            try:
                excerpt = e.read().decode("utf-8", errors="replace")[:200]
            except Exception:
                pass
        raise EmbedError(
            f"POST {url} failed: {e}; body_excerpt={excerpt!r}"
        ) from e
    try:
        return json.loads(raw)
    except json.JSONDecodeError as e:
        raise EmbedError(
            f"POST {url} returned non-JSON: {e}; raw={raw[:200]!r}"
        ) from e


def embed_query(
    text: str,
    provider: str,
    model: str,
    base_url: str,
    api_key: str = "",
) -> list[float]:
    """Embed a single text via the configured provider. Returns
    the raw float vector (NOT L2-normalized; the caller does
    that).

    `provider` is one of: ollama, openai-compatible, openai,
    openrouter. The OpenAI-shape family is handled by the same
    branch — the three names differ only in how the rest of
    MPM interprets the result, not in how the HTTP request is
    shaped.
    """
    if not text:
        raise EmbedError("embed_query: text must be non-empty")
    if not base_url:
        raise EmbedError("embed_query: base_url must be non-empty")
    if not model:
        raise EmbedError("embed_query: model must be non-empty")

    p = provider.lower().strip()
    base = base_url.rstrip("/")

    if p == "ollama":
        # Ollama modern endpoint: POST {base}/api/embed
        # body {"model","input"} -> {"embeddings":[[...]]}
        # The harness accepts a base_url that already includes
        # /api/embed; normalize so the URL we POST to is sane.
        if base.endswith("/api/embed") or base.endswith("/api/embeddings"):
            url = base
        else:
            url = base + "/api/embed"
        reply = _http_post_json(
            url,
            {"model": model, "input": text},
            "",
        )
        try:
            vec = reply["embeddings"][0]
        except (KeyError, IndexError, TypeError) as e:
            raise EmbedError(
                f"ollama response missing 'embeddings[0]': {e}; "
                f"reply={reply!r}"
            ) from e
        return [float(x) for x in vec]

    if p in ("openai-compatible", "openai", "openrouter"):
        # OpenAI shape: POST {base}/embeddings
        # body {"input","model"} -> {"data":[{"embedding":[...]}]}
        # The harness's base_url may or may not include /v1; the
        # OpenAI-compatible adapter in production tolerates both.
        if base.endswith("/embeddings"):
            url = base
        else:
            url = base + "/embeddings"
        reply = _http_post_json(
            url,
            {"input": text, "model": model},
            api_key,
        )
        try:
            vec = reply["data"][0]["embedding"]
        except (KeyError, IndexError, TypeError) as e:
            raise EmbedError(
                f"openai-compatible response missing 'data[0].embedding': {e}; "
                f"reply={reply!r}"
            ) from e
        return [float(x) for x in vec]

    raise EmbedError(
        f"embed_query: unsupported provider {provider!r}; "
        f"supported: ollama, openai-compatible, openai, openrouter"
    )


def l2_normalize(v: list[float]) -> list[float]:
    """L2-normalize a float vector. Returns a vector of the same
    length. A zero-norm input returns the zero vector (we never
    NaN — that would corrupt downstream cosine computations)."""
    norm_sq = sum(x * x for x in v)
    if norm_sq <= 0.0:
        return [0.0] * len(v)
    import math
    inv = 1.0 / math.sqrt(norm_sq)
    return [x * inv for x in v]
