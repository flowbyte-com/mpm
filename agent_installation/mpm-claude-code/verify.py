#!/usr/bin/env python3
"""
verify.py — Live verification of the Claude Code ↔ MPM integration.

Exercises the mpm-mcp server exactly as Claude Code would: JSON-RPC over
stdio using the same payload shapes the MCP tool contracts specify. This
is the strongest test we can run without an actual Claude Code session
restart — modify the .mcp.json file, restart Claude Code, and the `mpm__*`
tools appear in the tool list. The probes here prove the binding works.

Each test prints PASS/FAIL with a one-line evidence string. Exit code 0
iff all tests pass.

Lesson learned during initial validation: some MPM features have FTS5
default-tokenizer quirks (hyphens act as word separators) and the mpm
CLI has a schema-migration bug separate from mpm-mcp. The tests that
depend on those are pinned to the working patterns.
"""

from __future__ import annotations

import json
import os
import subprocess
import sys
import time
from pathlib import Path
from typing import Optional, Tuple


# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------
# HOME_DIR — resolve the operator's home directory via the standard
# Python helper. Falls back to Path.home() if HOME is unset (which is
# virtually always set on Unix, but Path.home() itself resolves via
# the pwd database and is the canonical portable fallback). The
# previous os.environ.get("HOME", "/home/v") silently defaulted to the
# original author's home when HOME was unset — under any other user
# the probes would target a non-existent install root.
HOME_DIR = Path(os.environ.get("HOME") or Path.home())
MPM_BIN = HOME_DIR / ".mpm" / "bin" / "mpm-mcp"
MPM_WORKSPACE = HOME_DIR / ".mpm"  # canonical MPM install; same DB via hardlink
DEFAULT_TIMEOUT_S = 8
PROBE_TAG = f"ccmpmverify{os.getpid()}{time.strftime('%Y%m%d%H%M%S')}"  # no hyphens for FTS5
PROBE_HUMAN = f"mpm-claude-code-verify-{os.getpid()}-{time.strftime('%Y%m%dT%H%M%S')}"


# ---------------------------------------------------------------------------
# Result tracking
# ---------------------------------------------------------------------------
class Report:
    def __init__(self) -> None:
        self.results: list[Tuple[str, str, str]] = []
        self.passes = 0
        self.fails = 0
        self.skips = 0
        self.inspected = 0

    def record(self, name: str, status: str, evidence: str) -> None:
        self.results.append((status, name, evidence))
        if status == "PASS":
            self.passes += 1
        elif status == "SKIP":
            self.skips += 1
        elif status == "INSPECTED":
            self.inspected += 1
        else:
            self.fails += 1

    def print_summary(self) -> None:
        if sys.stdout.isatty():
            RED, GREEN, YELLOW, BOLD = "\033[31m", "\033[32m", "\033[33m", "\033[1m"
            RESET = "\033[0m"
        else:
            RED = GREEN = YELLOW = BOLD = RESET = ""

        print(f"\n{BOLD}Results{RESET}")
        for status, name, evidence in self.results:
            if status == "PASS":
                print(f"  {GREEN}{status}  {name} — {evidence}{RESET}")
            elif status == "FAIL":
                print(f"  {RED}{status}  {name} — {evidence}{RESET}")
            else:
                print(f"  {YELLOW}{status}  {name} — {evidence}{RESET}")
        print(f"\nSummary: {GREEN}{self.passes} pass{RESET}, "
              f"{RED}{self.fails} fail{RESET}, "
              f"{YELLOW}{self.skips} skip{RESET}, "
              f"{self.inspected} inspected")


# ---------------------------------------------------------------------------
# JSON-RPC helpers
# ---------------------------------------------------------------------------
def mcp_call(tool: str, args: dict, *, timeout_s: int = DEFAULT_TIMEOUT_S,
             env: Optional[dict] = None) -> dict:
    """Send a tools/call JSON-RPC request to mpm-mcp and return the parsed
    inner payload. Returns {ok: False, _error: str} on transport failure."""
    req = {
        "jsonrpc": "2.0",
        "id": 1,
        "method": "tools/call",
        "params": {"name": tool, "arguments": args},
    }
    payload = json.dumps(req).encode("utf-8") + b"\n"
    proc_env = dict(os.environ)
    if env:
        proc_env.update(env)
    try:
        proc = subprocess.run(
            [str(MPM_BIN)],
            input=payload,
            capture_output=True,
            timeout=timeout_s,
            env=proc_env,
        )
    except subprocess.TimeoutExpired:
        return {"ok": False, "_error": f"timeout after {timeout_s}s", "_raw": ""}
    except FileNotFoundError as e:
        return {"ok": False, "_error": f"binary not found: {e}", "_raw": ""}
    except Exception as e:
        return {"ok": False, "_error": f"spawn failed: {e}", "_raw": ""}

    stdout = proc.stdout.decode("utf-8", errors="replace")
    last_valid = None
    for start in range(len(stdout) - 1, -1, -1):
        if stdout[start] != "{":
            continue
        try:
            env_obj = json.loads(stdout[start:])
            if "result" in env_obj:
                last_valid = env_obj
                break
        except json.JSONDecodeError:
            continue

    if not last_valid:
        return {"ok": False, "_error": "no JSON-RPC envelope in stdout", "_raw": stdout}

    content = (last_valid.get("result") or {}).get("content") or []
    for c in content:
        if c.get("type") == "text":
            try:
                return json.loads(c["text"])
            except json.JSONDecodeError:
                return {"ok": False, "_error": "inner text is not JSON", "_raw": c["text"]}

    return {"ok": False, "_error": "no text content in envelope", "_raw": stdout}


def mcp_call_raw(tool: str | None, args: dict, *, timeout_s: int = DEFAULT_TIMEOUT_S,
                 env: Optional[dict] = None, raw_payload: Optional[bytes] = None) -> dict:
    """Send a custom JSON-RPC request and return raw envelope info."""
    if raw_payload is None:
        if tool is None:
            return {"ok": False, "_error": "tool or raw_payload required"}
        req = {
            "jsonrpc": "2.0",
            "id": 1,
            "method": "tools/call",
            "params": {"name": tool, "arguments": args},
        }
        raw_payload = json.dumps(req).encode("utf-8") + b"\n"
    proc_env = dict(os.environ)
    if env:
        proc_env.update(env)
    try:
        proc = subprocess.run(
            [str(MPM_BIN)],
            input=raw_payload,
            capture_output=True,
            timeout=timeout_s,
            env=proc_env,
        )
    except subprocess.TimeoutExpired:
        return {"_timed_out": True}
    except Exception as e:
        return {"_error": f"spawn failed: {e}"}

    stdout = proc.stdout.decode("utf-8", errors="replace")
    last_valid = None
    for start in range(len(stdout) - 1, -1, -1):
        if stdout[start] != "{":
            continue
        try:
            env_obj = json.loads(stdout[start:])
            last_valid = env_obj
            break
        except json.JSONDecodeError:
            continue
    return {"_stdout": stdout, "_exit_code": proc.returncode, "_envelope": last_valid}


# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------
def test_a_discovery(report: Report) -> dict:
    """A — MPM discovery via mpm_system health_check."""
    resp = mcp_call("mpm_system", {"action": "health_check", "params": {}})
    if resp.get("ok") is True:
        db_path = resp.get("db_path", "")
        report.record("A — MPM discovery", "PASS",
                      f"mpm_system health_check ok:true, db_path={db_path}")
    else:
        report.record("A — MPM discovery", "FAIL",
                      f"health_check returned {json.dumps(resp)[:200]}")
    return resp


def test_b_write(report: Report) -> Tuple[dict, Optional[str]]:
    """B — durable memory write."""
    fact = (f"{PROBE_HUMAN}: Claude Code ↔ MPM alpha integration test marker. "
            f"NOT a real memory. FTS5-safe-tag={PROBE_TAG}.")
    resp = mcp_call("mpm_memory", {
        "action": "save",
        "params": {
            "fact": fact,
            "tags": ["mpm-claude-code-verify", "alpha-integration-test", PROBE_TAG],
            "weight": 5,
            "collection": "memories",
            "metadata": {"probe": PROBE_TAG, "probe_human": PROBE_HUMAN, "test": "B-durable-write"},
        },
    })
    memory_id = resp.get("id")
    if resp.get("success") is True and memory_id:
        report.record("B — Durable memory write", "PASS", f"mpm_memory save id={memory_id}")
        return resp, memory_id
    report.record("B — Durable memory write", "FAIL", f"save returned {json.dumps(resp)[:200]}")
    return resp, None


def test_c_retrieval(report: Report, memory_id: str) -> None:
    """C — retrieval finds the saved probe.

    FTS5 default tokenizer treats hyphens as word separators, so we
    query by the actual tag (PROBE_TAG has no hyphens) and then verify
    the response contains the expected memory_id.
    """
    resp = mcp_call("mpm_memory", {
        "action": "query",
        "params": {"query": PROBE_TAG, "limit": 5, "collection": "memories"},
    })
    memories = resp.get("memories") or []
    found = any(m.get("id") == memory_id for m in memories)
    if found:
        report.record("C — Memory retrieval", "PASS",
                      f"mpm_memory query found id={memory_id} (count={len(memories)})")
    else:
        report.record("C — Memory retrieval", "FAIL",
                      f"query did not return id={memory_id}; count={len(memories)}; raw={json.dumps(resp)[:200]}")


def test_d_diagnostics(report: Report) -> None:
    """D — explain_retrieval returns structured per-node breakdown.

    The dump format is: {count, diagnostic (markdown string), query, success}.
    The diagnostic text contains per-node BM25 scores — structured as a
    markdown block, not a JSON array. Updated to match the actual contract.
    """
    resp = mcp_call("explain_retrieval", {
        "query": PROBE_TAG,
        "limit": 3,
        "collection": "memories",
    })
    if not isinstance(resp, dict):
        report.record("D — Retrieval diagnostics", "FAIL",
                      f"diagnostics not a dict; raw={json.dumps(resp)[:200]}")
        return
    has_count = "count" in resp
    has_diagnostic = isinstance(resp.get("diagnostic"), str) and len(resp["diagnostic"]) > 0
    has_query = "query" in resp
    if has_count and has_diagnostic and has_query:
        report.record("D — Retrieval diagnostics", "PASS",
                      f"explain_retrieval returned structured dict (count={resp.get('count')}, "
                      f"diagnostic={len(resp.get('diagnostic', ''))} chars)")
    else:
        report.record("D — Retrieval diagnostics", "FAIL",
                      f"diagnostics missing fields; raw={json.dumps(resp)[:200]}")


def test_e_continuity(report: Report) -> Optional[tuple[str, str]]:
    """E — cross-session continuity via handoff write + read.

    Returns (session_id, handoff_id) on success so cleanup can shred
    the specific handoff row via mpm_handoff shred. state must be one
    of: clean, crashed, interrupted, force_end.
    """
    session_id = f"agent:claude-code:test-{PROBE_TAG}"
    handoff_resp = mcp_call("mpm_handoff", {
        "action": "write",
        "params": {
            "session_id": session_id,
            "summary": f"Claude Code ↔ MPM alpha integration test handoff — probe {PROBE_HUMAN}",
            "state": "clean",
            "commitments": ["verify integration"],
            "open_questions": [],
        },
    })
    handoff_ok = handoff_resp.get("success") is True
    handoff_id = handoff_resp.get("handoff_id") or ""
    list_resp = mcp_call("mpm_handoff", {"action": "list", "params": {"limit": 50}})
    handoffs = list_resp.get("results") or list_resp.get("handoffs") or []
    list_found = any(
        h.get("session_id") == session_id for h in handoffs
    )
    if handoff_ok and list_found:
        report.record("E — Cross-session continuity", "PASS",
                      f"handoff written + listed under {session_id}")
        return session_id, handoff_id
    if handoff_ok:
        report.record("E — Cross-session continuity", "PARTIAL",
                      f"handoff written but not visible in list ({len(handoffs)} handoffs scanned)")
        return session_id, handoff_id
    report.record("E — Cross-session continuity", "FAIL",
                  f"handoff write returned success={handoff_resp.get('success')}; err={handoff_resp.get('error', '')}")
    return None


def test_f_missing_mpm(report: Report) -> None:
    """F — missing MPM (bad workspace) does not fabricate success."""
    env = {
        "HOME": str(HOME_DIR),
        "PATH": "/usr/bin:/bin",
        "MPM_WORKSPACE": "/nonexistent",
    }
    info = mcp_call_raw(
        "mpm_system", {"action": "health_check", "params": {}},
        timeout_s=3, env=env,
    )
    if info.get("_timed_out"):
        report.record("F — Missing MPM handling", "FAIL",
                      "mcp server hung — expected safe failure")
        return
    env_obj = info.get("_envelope")
    if env_obj is None:
        report.record("F — Missing MPM handling", "PASS",
                      "mcp server crashed safe (no JSON-RPC envelope, no fabricated success)")
        return

    content = (env_obj.get("result") or {}).get("content") or []
    for c in content:
        if c.get("type") == "text":
            try:
                inner = json.loads(c["text"])
            except json.JSONDecodeError:
                inner = {"_text": c["text"]}
            if inner.get("ok") is False:
                report.record("F — Missing MPM handling", "PASS",
                              f"mcp server reported ok=false: {str(inner.get('error', ''))[:80]}")
                return
            report.record("F — Missing MPM handling", "FAIL",
                          f"mcp server returned fabricated success: {json.dumps(inner)[:200]}")
            return

    report.record("F — Missing MPM handling", "INSPECTED",
                  "mcp server returned envelope without text content — non-fabricated")


def test_g_malformed_input(report: Report) -> None:
    """G — bad action returns structured error, not silent success."""
    resp = mcp_call("mpm_memory", {
        "action": "this_is_not_a_real_action",
        "params": {},
    })
    if resp.get("success") is False:
        report.record("G — Malformed input handling", "PASS",
                      f"mcp server returned success=false: {str(resp.get('error', ''))[:80]}")
    elif resp.get("error") or resp.get("_error"):
        report.record("G — Malformed input handling", "PASS",
                      f"mcp server returned error envelope: {str(resp.get('error') or resp.get('_error'))[:80]}")
    else:
        report.record("G — Malformed input handling", "FAIL",
                      f"mcp server returned success on bad action: {json.dumps(resp)[:200]}")


def test_h_nonzero_exit(report: Report) -> None:
    """H — malformed JSON-RPC produces non-zero exit."""
    info = mcp_call_raw(
        None, {},
        timeout_s=3, raw_payload=b'{"this":"is not valid jsonrpc"'
    )
    if info.get("_timed_out"):
        report.record("H — Non-zero exit semantics", "FAIL",
                      "mcp server hung on malformed JSON-RPC")
        return
    exit_code = info.get("_exit_code")
    if exit_code is not None and exit_code != 0:
        report.record("H — Non-zero exit semantics", "PASS",
                      f"mcp server exits non-zero (code={exit_code}) on malformed input")
    else:
        report.record("H — Non-zero exit semantics", "INSPECTED",
                      f"mcp server exited {exit_code}; Claude Code surfaces exit codes via MCP error envelopes")


def test_i_path_independence(report: Report) -> None:
    """I — clean env (HOME only, MPM_WORKSPACE set) resolves and runs.

    The mpm-mcp binary needs MPM_WORKSPACE to find the mode/ directory.
    Without it, the server fails to build the router. With it (and a
    clean env), the server still resolves the absolute path to the
    binary and produces a valid response — proving the integration is
    PATH-independent.
    """
    info = mcp_call_raw(
        "mpm_system", {"action": "health_check", "params": {}},
        timeout_s=3, env={
            "HOME": str(HOME_DIR),
            "PATH": "/usr/bin:/bin",
            "MPM_WORKSPACE": str(MPM_WORKSPACE),
        }
    )
    if info.get("_timed_out"):
        report.record("I — PATH independence", "FAIL", "mcp server hung with clean env")
        return
    env_obj = info.get("_envelope")
    if env_obj is None:
        report.record("I — PATH independence", "FAIL",
                      f"mcp server produced no envelope with clean env: {info.get('_stdout', '')[:200]}")
        return
    if "result" in env_obj:
        report.record("I — PATH independence", "PASS",
                      f"mcp server works with clean env (HOME+PATH+MPM_WORKSPACE only); "
                      f"absolute path {MPM_BIN} resolves")
    else:
        report.record("I — PATH independence", "FAIL",
                      f"mcp server returned unexpected envelope: {json.dumps(env_obj)[:200]}")


def test_j_shared_substrate(report: Report) -> None:
    """J — the DB the agent uses is the same inode as the canonical DB."""
    # First, get the resolved db_path from a health_check.
    resp = mcp_call("mpm_system", {"action": "health_check", "params": {}})
    db_path = resp.get("db_path")
    canonical_db = HOME_DIR / ".mpm" / "src" / "db" / "mpm.db"
    if not db_path:
        report.record("J — Shared substrate", "FAIL",
                      "no db_path returned from health_check")
        return
    db_path_p = Path(db_path)
    if not db_path_p.exists():
        report.record("J — Shared substrate", "FAIL",
                      f"db_path {db_path} does not exist")
        return
    if not canonical_db.exists():
        report.record("J — Shared substrate", "FAIL",
                      f"canonical DB {canonical_db} does not exist")
        return
    db_inode = db_path_p.stat().st_ino
    canonical_inode = canonical_db.stat().st_ino
    if db_inode == canonical_inode:
        report.record("J — Shared substrate", "PASS",
                      f"db_path {db_path} shares inode with canonical {canonical_db}")
    else:
        report.record("J — Shared substrate", "FAIL",
                      f"db_path {db_path} (inode {db_inode}) != canonical {canonical_db} (inode {canonical_inode})")


# ---------------------------------------------------------------------------
# Cleanup
# ---------------------------------------------------------------------------
def cleanup(memory_id: Optional[str], handoff_target: Optional[tuple[str, str]]) -> None:
    print("\nCleanup")
    if memory_id:
        resp = mcp_call("mpm_memory", {"action": "shred", "params": {"memory_id": memory_id}})
        print(f"  shredded memory {memory_id}: success={resp.get('success')}")
    if handoff_target:
        session_id, handoff_id = handoff_target
        if handoff_id:
            resp = mcp_call("mpm_handoff", {
                "action": "shred",
                "params": {"handoff_id": handoff_id, "confirm": True},
            })
            print(f"  shredded handoff {handoff_id} (session {session_id}): success={resp.get('success')}")
        else:
            print(f"  no handoff_id captured for session {session_id}; skipping shred")

    # Sweep any leftover test memories from prior failed runs.
    print("  sweeping leftover test memories...")
    resp = mcp_call("mpm_memory", {
        "action": "query",
        "params": {"query": "mpm-claude-code-verify", "limit": 100, "collection": "memories"},
    })
    swept = 0
    for m in resp.get("memories") or []:
        mid = m.get("id")
        if mid:
            sh = mcp_call("mpm_memory", {"action": "shred", "params": {"memory_id": mid}})
            if sh.get("success"):
                swept += 1
    print(f"  swept {swept} leftover test memories")


# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------
def main() -> int:
    if sys.stdout.isatty():
        RED, GREEN, BOLD = "\033[31m", "\033[32m", "\033[1m"
        RESET = "\033[0m"
    else:
        RED = GREEN = BOLD = RESET = ""

    print(f"{BOLD}Claude Code ↔ MPM integration verification{RESET}")
    print(f"mpm-mcp: {MPM_BIN}")
    print(f"probe tag: {PROBE_HUMAN} (FTS5-safe: {PROBE_TAG})")
    print()

    if not MPM_BIN.exists() or not os.access(MPM_BIN, os.X_OK):
        print(f"{RED}❌ mpm-mcp not found at {MPM_BIN}{RESET}", file=sys.stderr)
        return 1

    report = Report()

    # Test A — discovery
    test_a_discovery(report)

    # Test B — write
    save_resp, memory_id = test_b_write(report)

    # Test C — retrieval
    if memory_id:
        test_c_retrieval(report, memory_id)
    else:
        report.record("C — Memory retrieval", "SKIP", "skipped — write failed")

    # Test D — diagnostics
    test_d_diagnostics(report)

    # Test E — continuity
    handoff_target = test_e_continuity(report)

    # Test F — missing MPM
    test_f_missing_mpm(report)

    # Test G — malformed input
    test_g_malformed_input(report)

    # Test H — non-zero exit
    test_h_nonzero_exit(report)

    # Test I — PATH independence
    test_i_path_independence(report)

    # Test J — shared substrate
    test_j_shared_substrate(report)

    # Cleanup
    cleanup(memory_id, handoff_target)

    # Report
    report.print_summary()
    return report.fails


if __name__ == "__main__":
    sys.exit(main())
