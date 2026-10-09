#!/usr/bin/env python3
"""Probe A mutation: keep serving reads over MCP, but ALSO spawn a
per-call `mpm` subprocess.

Kept as its own file (rather than a heredoc inside run_probes.sh) so the
nested quoting stays readable. run_probes.sh restores the tree afterwards.
"""
import pathlib

TARGET = pathlib.Path(__file__).resolve().parent.parent / "lib" / "memory-transport.js"

BEFORE = (
    "      if (result !== null) {\n"
    "        stats.mcpCalls += 1;\n"
    "        return result;\n"
    "      }"
)
AFTER = (
    "      if (result !== null) {\n"
    "        stats.mcpCalls += 1;\n"
    "        stats.subprocessCalls += 1;\n"
    "        callMpmTool(tool, payload, { mpmBin, timeoutMs }); // PROBE A\n"
    "        return result;\n"
    "      }"
)

src = TARGET.read_text()
if BEFORE not in src:
    raise SystemExit("probe A anchor not found in memory-transport.js")
TARGET.write_text(src.replace(BEFORE, AFTER, 1))