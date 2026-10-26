#!/usr/bin/env python3
"""Session C — MPM MCP tool-surface measurement harness.

Measures the byte/token cost of the tool surface the model actually
receives on `tools/list`, for the compact (3-tool) surface and the full
surface (`MPM_EXPOSE_ALL_TOOLS=1`).

The measurement is read-only with respect to MPM: it starts the MCP
server, performs the protocol handshake, issues `tools/list`, and exits.
It never calls a tool.

Isolation: the caller supplies MPM_WORKSPACE / MPM_DB_PATH. This script
defaults to the shared dogfooding workspace and never touches `~/.mpm`.

Usage:
    measure_tool_surface.py --mcp <path-to-mpm-mcp> [--json out.json]

Reproducibility record is written into the JSON output.
"""
from __future__ import annotations

import argparse
import json
import os
import subprocess
import sys
import time

PROTOCOL_VERSION = "2024-11-05"


def _rpc(framing: str) -> None:
    return None


def read_message(stream, framing: str):
    """Read one JSON-RPC message from a stdio MCP stream."""
    if framing == "content-length":
        headers = {}
        while True:
            line = stream.readline()
            if not line:
                return None
            line = line.decode("utf-8", "replace")
            if line in ("\r\n", "\n"):
                break
            if ":" in line:
                k, v = line.split(":", 1)
                headers[k.strip().lower()] = v.strip()
        n = int(headers.get("content-length", "0"))
        if n == 0:
            return None
        return json.loads(stream.read(n).decode("utf-8"))
    # newline-delimited JSON
    while True:
        line = stream.readline()
        if not line:
            return None
        line = line.strip()
        if not line:
            continue
        try:
            return json.loads(line.decode("utf-8"))
        except json.JSONDecodeError:
            continue


def write_message(proc, msg, framing: str):
    data = json.dumps(msg).encode("utf-8")
    if framing == "content-length":
        proc.stdin.write(f"Content-Length: {len(data)}\r\n\r\n".encode("utf-8"))
        proc.stdin.write(data)
    else:
        proc.stdin.write(data + b"\n")
    proc.stdin.flush()


def measure(mcp_bin: str, env_extra: dict, timeout_s: float = 40.0) -> dict:
    env = dict(os.environ)
    env.update(env_extra)
    # mcp-go's stdio transport is newline-delimited JSON (MCP stdio spec);
    # content-length framing is tried only as a fallback.
    for framing in ("newline", "content-length"):
        proc = subprocess.Popen(
            [mcp_bin],
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            env=env,
        )
        try:
            write_message(
                proc,
                {
                    "jsonrpc": "2.0",
                    "id": 1,
                    "method": "initialize",
                    "params": {
                        "protocolVersion": PROTOCOL_VERSION,
                        "capabilities": {},
                        "clientInfo": {"name": "session-c-measure", "version": "1"},
                    },
                },
                framing,
            )
            deadline = time.time() + timeout_s
            init_ok = False
            while time.time() < deadline:
                msg = read_message(proc.stdout, framing)
                if msg is None:
                    break
                if msg.get("id") == 1:
                    init_ok = "result" in msg
                    break
            if not init_ok:
                proc.kill()
                err = proc.stderr.read().decode("utf-8", "replace")[-500:]
                proc.wait(timeout=5)
                last_error = err
                continue

            try:
                proc.stdin.write(
                    (json.dumps({"jsonrpc": "2.0", "method": "notifications/initialized"}) + "\n").encode()
                )
                proc.stdin.flush()
            except Exception:
                pass

            write_message(proc, {"jsonrpc": "2.0", "id": 2, "method": "tools/list", "params": {}}, framing)

            tools = None
            while time.time() < deadline:
                msg = read_message(proc.stdout, framing)
                if msg is None:
                    break
                if msg.get("id") == 2:
                    if "error" in msg:
                        tools = {"__error": msg["error"]}
                    else:
                        tools = msg.get("result", {}).get("tools", [])
                    break

            proc.kill()
            try:
                proc.wait(timeout=5)
            except Exception:
                pass
            if tools is not None:
                return {"framing": framing, "tools": tools}
        except Exception as exc:  # noqa: BLE001
            try:
                proc.kill()
            except Exception:
                pass
            last_error = str(exc)
    raise RuntimeError(f"could not complete MCP handshake: {last_error if 'last_error' in dir() else 'unknown'}")


def describe(tools: list) -> dict:
    rows = []
    for t in tools:
        desc = t.get("description") or ""
        schema = t.get("inputSchema") or {}
        schema_bytes = len(json.dumps(schema, separators=(",", ":")))
        rows.append(
            {
                "name": t.get("name"),
                "description_bytes": len(desc.encode("utf-8")),
                "schema_bytes": schema_bytes,
                "description": desc,
            }
        )
    desc_total = sum(r["description_bytes"] for r in rows)
    schema_total = sum(r["schema_bytes"] for r in rows)
    return {
        "tool_count": len(rows),
        "tools": rows,
        "description_bytes_total": desc_total,
        "schema_bytes_total": schema_total,
        "surface_bytes_total": desc_total + schema_total,
        "description_tokens_est": round(desc_total / 4),
        "surface_tokens_est": round((desc_total + schema_total) / 4),
    }


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--mcp", required=True)
    ap.add_argument("--json", default=None)
    ap.add_argument("--workspace", default=os.environ.get("MPM_WORKSPACE", os.path.expanduser("~/.mpm-dogfood")))
    ap.add_argument("--db", default=os.environ.get("MPM_DB_PATH", os.path.expanduser("~/.mpm-dogfood/src/db/mpm.db")))
    args = ap.parse_args()

    base_env = {"MPM_WORKSPACE": args.workspace, "MPM_DB_PATH": args.db}

    compact = describe(measure(args.mcp, base_env)["tools"])
    full = describe(measure(args.mcp, {**base_env, "MPM_EXPOSE_ALL_TOOLS": "1"})["tools"])

    result = {
        "reproducibility": {
            "mcp_binary": args.mcp,
            "workspace": args.workspace,
            "db_path": args.db,
            "measured_at_utc": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
            "estimator": "bytes/4 (documented approximation, not a tokenizer)",
        },
        "compact": compact,
        "full": full,
        "delta": {
            "tool_count": full["tool_count"] - compact["tool_count"],
            "description_bytes": full["description_bytes_total"] - compact["description_bytes_total"],
            "surface_bytes": full["surface_bytes_total"] - compact["surface_bytes_total"],
            "surface_tokens_est": full["surface_tokens_est"] - compact["surface_tokens_est"],
            "ratio_surface": round(full["surface_bytes_total"] / max(1, compact["surface_bytes_total"]), 2),
        },
    }

    text = json.dumps(result, indent=2)
    if args.json:
        with open(args.json, "w") as fh:
            fh.write(text + "\n")
    c, f = compact, full
    print(f"compact : {c['tool_count']:>3} tools | desc {c['description_bytes_total']:>6} B | "
          f"surface {c['surface_bytes_total']:>6} B | ~{c['surface_tokens_est']:>5} tok")
    print(f"full    : {f['tool_count']:>3} tools | desc {f['description_bytes_total']:>6} B | "
          f"surface {f['surface_bytes_total']:>6} B | ~{f['surface_tokens_est']:>5} tok")
    print(f"delta   : {result['delta']['tool_count']:>3} tools | "
          f"{result['delta']['surface_bytes']:>6} B | ~{result['delta']['surface_tokens_est']:>5} tok "
          f"| ratio {result['delta']['ratio_surface']}x")
    for r in compact["tools"]:
        print(f"  compact tool: {r['name']:<14} desc {r['description_bytes']:>5} B  schema {r['schema_bytes']:>5} B")
    return 0


if __name__ == "__main__":
    sys.exit(main())
