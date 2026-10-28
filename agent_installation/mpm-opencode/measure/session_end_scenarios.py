#!/usr/bin/env python3
"""session_end_scenarios.py — exercise the REAL OpenCode host at session
boundaries, through the isolated launcher, and record what it publishes.

WHAT THIS IS FOR.

The question is not "does OpenCode have a session-end event" — it is
"which of the candidate signals is OBSERVABLE, what does each one mean,
is it repeatable, and does any of them establish a session TERMINAL
boundary rather than a turn boundary".

The candidate signals under test:

    session.execution.succeeded
    session.execution.failed
    session.execution.interrupted
    session.idle
    session.archived
    session.deleted
    location.shutdown
    the plugin's synchronous cleanup callback

Each is probed INDEPENDENTLY: one scenario per signal, so a signal that
merely happens to appear alongside another is not mistaken for one.

THE HOST IS THE REAL ONE.

The launcher is `scripts/dev_host.sh`, which pins MPM_WORKSPACE,
MPM_DB_PATH and MPM_BINARY to disposable values, redirects all four XDG
roots, and registers THIS repository's plugin plus a witness probe. The
live installation is never touched.

WHAT IS RECORDED.

Event types, sequence numbers, timestamps, and the four small scalar
fields that carry a boundary's meaning (sessionID, reason, outcome,
archived). NO prompt text, NO assistant message, NO tool input, NO tool
output, NO compaction summary. The probe records data KEY NAMES for
everything else.

Usage:
    python3 measure/session_end_scenarios.py [--out DIR] [scenario ...]
"""

from __future__ import annotations

import argparse
import json
import os
import re
import signal
import subprocess
import sys
import time
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[3]
ADAPTER_DIR = REPO_ROOT / "agent_installation" / "mpm-opencode"
PROBE_DIR = ADAPTER_DIR / "measure" / "probe-session-end"
DEV_HOST = REPO_ROOT / "scripts" / "dev_host.sh"
OPENCODE_BIN = os.environ.get("OPENCODE_BIN", str(Path.home() / ".opencode/bin/opencode"))


def _mpm_binary() -> str:
    """The `mpm` binary under test.

    A worktree has no `.build/`, so the fallback walks up to the main
    checkout rather than pointing at a path that does not exist.
    """
    candidates = [
        os.environ.get("MPM_BINARY"),
        str(REPO_ROOT / ".build/bin/mpm"),
        str(Path.home() / "src/mpm-opencode/.build/bin/mpm"),
    ]
    for c in candidates:
        if c and Path(c).is_file() and os.access(c, os.X_OK):
            return c
    raise RuntimeError("no executable mpm binary found; set MPM_BINARY")


MPM_BINARY = _mpm_binary()

DEFAULT_OUT = Path("/tmp/opencode/session-end-scenarios")
PORT = int(os.environ.get("MPM_DEV_HOST_PORT", "49811"))


# ── the host ────────────────────────────────────────────────────────────


class Host:
    """The isolated development host, started through scripts/dev_host.sh."""

    def __init__(self, out: Path, label: str):
        self.out = out
        self.label = label
        self.run_dir = Path("/tmp/opencode/dev-host") / label
        self.workspace = out / label / "workspace"
        self.probe_log = self.run_dir / "out" / "probe.jsonl"
        self.proc: subprocess.Popen | None = None
        self.password: str | None = None
        self.url = f"http://127.0.0.1:{PORT}"

    def env(self) -> dict:
        env = dict(os.environ)
        env.update(
            {
                "MPM_WORKSPACE": str(self.workspace),
                "MPM_DB_PATH": str(self.workspace / "src/db/mpm.db"),
                "MPM_BINARY": MPM_BINARY,
                "MPM_DEV_HOST_PROBE": str(PROBE_DIR),
                "MPM_DEV_HOST_LABEL": self.label,
                "MPM_DEV_HOST_PORT": str(PORT),
                "MPM_DEV_HOST_ROOT": str(self.run_dir.parent),
                # The adapter's own diagnostic dump: the counters tell us
                # what the adapter SAW, which is what separates "the host
                # never published it" from "the adapter dropped it".
                "MPM_EVENT_DEBUG_DUMP": str(self.out / f"{self.label}.observer.jsonl"),
            }
        )
        return env

    def start(self) -> None:
        import shutil

        shutil.rmtree(self.run_dir, ignore_errors=True)
        (self.workspace / "src/db").mkdir(parents=True, exist_ok=True)
        self.out.mkdir(parents=True, exist_ok=True)
        log = open(self.out / f"{self.label}.serve.log", "w")
        # start_new_session: the server must outlive this process, and the
        # harness must be able to signal it directly.
        self.proc = subprocess.Popen(
            [str(DEV_HOST), "serve"],
            cwd=str(REPO_ROOT),
            env=self.env(),
            stdout=log,
            stderr=subprocess.STDOUT,
            start_new_session=True,
        )
        deadline = time.time() + 60
        while time.time() < deadline:
            if self.proc.poll() is not None:
                raise RuntimeError(f"the host exited early: see {self.out}/{self.label}.serve.log")
            text = (self.out / f"{self.label}.serve.log").read_text(errors="replace")
            m = re.search(r"server password (\S+)", text)
            if m:
                self.password = m.group(1)
                # NOTE: the plugin is loaded LAZILY — the server does not
                # run a plugin's setup() until the first agent execution.
                # So there is no probe log yet, and waiting for one here
                # would hang. The probe log is awaited AFTER the first
                # prompt instead.
                return
            time.sleep(0.25)
        raise RuntimeError("the host did not report a server password in 60s")

    def api(self, method: str, path: str, data: dict | None = None, timeout: int = 120) -> dict:
        cmd = [OPENCODE_BIN, "api", "--server", self.url, method, path]
        if data is not None:
            cmd += ["-d", json.dumps(data)]
        env = dict(os.environ)
        if self.password:
            env["OPENCODE_PASSWORD"] = self.password
        r = subprocess.run(cmd, capture_output=True, text=True, env=env, timeout=timeout)
        out = r.stdout.strip()
        try:
            parsed = json.loads(out)
        except Exception:
            return {"_raw": out, "_stderr": r.stderr.strip(), "_rc": r.returncode}
        # The API wraps successful results in {"data": …}.
        if isinstance(parsed, dict) and "data" in parsed and isinstance(parsed["data"], dict):
            return parsed["data"]
        return parsed

    def await_probe(self, timeout: float = 30.0) -> None:
        """Wait for the witness probe's log.

        The plugin loads on the FIRST agent execution, so the probe's log
        appears only after the first prompt.
        """
        deadline = time.time() + timeout
        while time.time() < deadline:
            if self.probe_log.exists() and self.probe_log.stat().st_size > 0:
                return
            time.sleep(0.1)

    def stop(self, sig=signal.SIGTERM) -> None:
        if self.proc and self.proc.poll() is None:
            try:
                os.killpg(os.getpgid(self.proc.pid), sig)
            except ProcessLookupError:
                pass
            try:
                self.proc.wait(timeout=20)
            except subprocess.TimeoutExpired:
                os.killpg(os.getpgid(self.proc.pid), signal.SIGKILL)
                self.proc.wait(timeout=10)

    def probe_rows(self) -> list[dict]:
        if not self.probe_log.exists():
            return []
        return [json.loads(l) for l in self.probe_log.read_text(errors="replace").splitlines() if l.strip()]


# ── the trace ───────────────────────────────────────────────────────────


def trace(rows: list[dict], since: int = 0) -> list[dict]:
    """The event rows after a marker index, plus the lifecycle rows."""
    out = []
    for r in rows[since:]:
        if r["kind"] == "event":
            out.append(r)
        elif r["kind"] in ("cleanup.callback", "process.signal", "process.exit", "stream.ended"):
            out.append(r)
    return out


def marker(rows: list[dict]) -> int:
    """The index just after the last event seen so far."""
    return len(rows)


def describe(rows: list[dict]) -> str:
    lines = []
    for r in rows:
        if r["kind"] == "event":
            b = r.get("boundary") or {}
            extra = "  " + json.dumps(b) if b else ""
            lines.append(f"    seq={r['seq']:<4} dt={r['dt']:<7} {r['type']}{extra}")
        else:
            lines.append(f"    ---- {r['kind']} {json.dumps({k: v for k, v in r.items() if k not in ('t', 'dt', 'pid', 'kind')})[:220]}")
    return "\n".join(lines)


# ── scenarios ───────────────────────────────────────────────────────────


def wait_for_idle(host: Host, session_id: str, timeout: int = 180, after: int | None = None) -> dict:
    """Wait until the host marks the session idle AGAIN.

    `Session.Info.time.idle` is the HOST's own idle marker timestamp —
    the same one that produces the `type: "idle"` session message. Using
    the host's own field avoids inventing an idle rule of our own.

    `after` is the idle timestamp already seen. It matters: `time.idle`
    is STICKY, so waiting for "idle is set" returns immediately on the
    second turn of a session and the scenario would end mid-turn.
    """
    deadline = time.time() + timeout
    last = {}
    while time.time() < deadline:
        info = host.api("GET", f"/api/session/{session_id}")
        if isinstance(info, dict) and "time" in info:
            last = info
            idle = info.get("time", {}).get("idle")
            if idle and (after is None or idle != after):
                return info
        time.sleep(0.4)
    return last


def idle_at(host: Host, session_id: str) -> int | None:
    info = host.api("GET", f"/api/session/{session_id}")
    if isinstance(info, dict):
        return info.get("time", {}).get("idle")
    return None


def new_session(host: Host, title: str) -> str:
    sid = host.api("POST", "/api/session", {"title": title}).get("id")
    if not sid:
        raise RuntimeError("the host did not return a session id")
    return sid


def prompt(host: Host, sid: str, text: str) -> None:
    host.api("POST", f"/api/session/{sid}/prompt", {"text": text}, timeout=60)


def sc_normal(host: Host, out: Path) -> dict:
    """One prompt in a fresh session, then a graceful host stop."""
    sid = new_session(host, "se-normal")
    rows = host.probe_rows()
    mark = marker(rows)
    prompt(host, sid, "Reply with exactly: OK")
    host.await_probe()
    wait_for_idle(host, sid)
    time.sleep(1.0)
    trace_rows = trace(host.probe_rows(), mark)
    host.stop(signal.SIGTERM)
    time.sleep(2.0)
    seen = {id(r) for r in trace_rows}
    trace_rows += [
        r
        for r in host.probe_rows()[mark:]
        if r["kind"] in ("cleanup.callback", "process.signal", "process.exit", "stream.ended") and id(r) not in seen
    ]
    return {"scenario": "normal", "session": sid, "trace": trace_rows}


def sc_multi_turn(host: Host, out: Path) -> dict:
    """TWO prompts in ONE session: does a completed execution end it?"""
    sid = new_session(host, "se-multi")
    rows = host.probe_rows()
    mark = marker(rows)
    for i in (1, 2):
        seen = idle_at(host, sid)
        prompt(host, sid, f"Reply with exactly: TURN{i}")
        host.await_probe()
        wait_for_idle(host, sid, after=seen)
        time.sleep(1.5)
    trace_rows = trace(host.probe_rows(), mark)
    return {"scenario": "multi-turn", "session": sid, "trace": trace_rows}


def sc_interrupted(host: Host, out: Path) -> dict:
    """Interrupt an in-flight execution."""
    sid = new_session(host, "se-interrupt")
    rows = host.probe_rows()
    mark = marker(rows)
    # A prompt long enough to still be running when the interrupt lands.
    prompt(host, sid, "Count slowly from 1 to 200, one number per line.")
    host.await_probe()
    time.sleep(5.0)
    resp = host.api("POST", f"/api/session/{sid}/interrupt", {})
    time.sleep(2.5)
    trace_rows = trace(host.probe_rows(), mark)
    return {"scenario": "interrupted", "session": sid, "interrupt_response": resp, "trace": trace_rows}


def sc_session_switch(host: Host, out: Path) -> dict:
    """Two sessions in the same host: does switching end the first?"""
    a = new_session(host, "se-switch-a")
    prompt(host, a, "Reply with exactly: A")
    host.await_probe()
    wait_for_idle(host, a)
    rows = host.probe_rows()
    mark = marker(rows)
    b = new_session(host, "se-switch-b")
    prompt(host, b, "Reply with exactly: B")
    wait_for_idle(host, b)
    time.sleep(1.0)
    trace_rows = trace(host.probe_rows(), mark)
    return {"scenario": "session-switch", "session_a": a, "session_b": b, "trace": trace_rows}


def sc_archived(host: Host, out: Path) -> dict:
    """Explicit session closure: archive it — then restore it.

    The restore matters: it shows whether the archive is a one-way
    boundary (it is not) and whether the host publishes the
    counterpart event.
    """
    sid = new_session(host, "se-archive")
    prompt(host, sid, "Reply with exactly: OK")
    host.await_probe()
    wait_for_idle(host, sid)
    rows = host.probe_rows()
    mark = marker(rows)
    resp = host.api("PATCH", f"/api/session/{sid}", {"archived": True})
    time.sleep(2.0)
    restore = host.api("PATCH", f"/api/session/{sid}", {"archived": False})
    time.sleep(2.0)
    trace_rows = trace(host.probe_rows(), mark)
    return {
        "scenario": "archived",
        "session": sid,
        "patch_response": resp,
        "restore_response": restore,
        "trace": trace_rows,
    }


def sc_deleted(host: Host, out: Path) -> dict:
    """Explicit session closure: delete it."""
    sid = new_session(host, "se-delete")
    prompt(host, sid, "Reply with exactly: OK")
    host.await_probe()
    wait_for_idle(host, sid)
    rows = host.probe_rows()
    mark = marker(rows)
    resp = host.api("DELETE", f"/api/session/{sid}")
    time.sleep(2.0)
    trace_rows = trace(host.probe_rows(), mark)
    return {"scenario": "deleted", "session": sid, "delete_response": resp, "trace": trace_rows}


def sc_compaction(host: Host, out: Path) -> dict:
    """Compaction: a boundary that is NOT a session end."""
    sid = new_session(host, "se-compact")
    prompt(host, sid, "Reply with exactly: OK")
    host.await_probe()
    wait_for_idle(host, sid)
    rows = host.probe_rows()
    mark = marker(rows)
    resp = host.api("POST", f"/api/session/{sid}/compact", {}, timeout=240)
    time.sleep(2.0)
    trace_rows = trace(host.probe_rows(), mark)
    return {"scenario": "compaction", "session": sid, "compact_response": resp, "trace": trace_rows}


def sc_abrupt_kill(host: Host, out: Path) -> dict:
    """Abrupt termination: SIGKILL the host mid-execution."""
    sid = new_session(host, "se-kill")
    prompt(host, sid, "Count slowly from 1 to 5000, one number per line.")
    host.await_probe()
    time.sleep(5.0)
    rows = host.probe_rows()
    mark = marker(rows)
    host.stop(signal.SIGKILL)
    time.sleep(2.0)
    trace_rows = trace(host.probe_rows(), mark)
    return {"scenario": "abrupt-kill", "session": sid, "trace": trace_rows}


def sc_shutdown_mid_execution(host: Host, out: Path) -> dict:
    """The host is stopped GRACEFULLY (SIGTERM) DURING an execution.

    The host's own reducer treats `session.execution.interrupted` with
    `reason === "shutdown"` specially — it does NOT synthesise an idle
    message for it. This scenario asks whether that event is published
    at all, and whether it precedes the plugin's cleanup callback.
    """
    sid = new_session(host, "se-shutdown-mid")
    prompt(host, sid, "Count slowly from 1 to 5000, one number per line.")
    host.await_probe()
    time.sleep(5.0)
    rows = host.probe_rows()
    mark = marker(rows)
    host.stop(signal.SIGTERM)
    time.sleep(2.5)
    trace_rows = trace(host.probe_rows(), mark)
    return {"scenario": "shutdown-mid-execution", "session": sid, "trace": trace_rows}


SCENARIOS = {
    "normal": sc_normal,
    "multi-turn": sc_multi_turn,
    "interrupted": sc_interrupted,
    "shutdown-mid-execution": sc_shutdown_mid_execution,
    "session-switch": sc_session_switch,
    "archived": sc_archived,
    "deleted": sc_deleted,
    "compaction": sc_compaction,
    "abrupt-kill": sc_abrupt_kill,
}


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--out", default=str(DEFAULT_OUT))
    ap.add_argument("scenarios", nargs="*", default=None)
    args = ap.parse_args()

    out = Path(args.out)
    out.mkdir(parents=True, exist_ok=True)
    wanted = args.scenarios or list(SCENARIOS)

    results = []
    for name in wanted:
        fn = SCENARIOS.get(name)
        if fn is None:
            print(f"unknown scenario: {name}", file=sys.stderr)
            return 2
        host = Host(out, f"se-{name}")
        print(f"\n=== scenario: {name} ===", flush=True)
        try:
            host.start()
            res = fn(host, out)
        except Exception as e:  # a failed scenario is a RESULT, not a crash
            res = {"scenario": name, "error": f"{type(e).__name__}: {e}", "trace": []}
            try:
                host.stop(signal.SIGKILL)
            except Exception:
                pass
        res["host_log"] = str(host.run_dir)
        results.append(res)
        print(describe(res.get("trace", [])), flush=True)
        (out / f"{name}.json").write_text(json.dumps(res, indent=2))
        try:
            host.stop(signal.SIGTERM)
        except Exception:
            pass

    (out / "summary.json").write_text(json.dumps(results, indent=2))
    print(f"\nartifacts: {out}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
