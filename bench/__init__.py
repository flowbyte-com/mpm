"""MPM Embedding Benchmark Harness (Issue #16).

This is a Python tool that calls the existing `mpm` binary against disposable
workspaces. It does NOT modify MPM product code. See
`docs/EMBEDDING-BENCHMARK-HARNESS.md` for the user-facing documentation and
`docs/EMBEDDING-BENCHMARK-HARNESS-DESIGN.md` for the design rationale.

The harness can be invoked as:

    python -m bench --help
    python -m bench --selftest
    python -m bench --smoke
    python -m bench run --output-dir /tmp/run-001 --model-config bench/corpus/fixtures/minilm.json

Test entry point:

    python -m unittest discover -s bench/tests -t .
"""

__version__ = "0.1.0-issue16"
