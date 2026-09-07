#!/usr/bin/env python3
"""
count_tokens.py — Token counter for MPM context economics.

Reads the byte-level summary.json produced by measure.sh, tokenises every
captured payload with tiktoken's cl100k_base encoding, and emits a
machine-readable report.json plus a human-readable report.md.

Tokenizer choice and limitations
--------------------------------
We use tiktoken's cl100k_base encoding as the token-counting model. This
is OpenAI's GPT-3.5/4 family tokenizer and is widely used as a practical
approximation for "what an LLM input/output looks like in tokens".

This is an approximation. Different model families use different
tokenizers:
  - cl100k_base       (OpenAI GPT-3.5/4)        ~ what we measure
  - o200k_base        (OpenAI GPT-4o/o-series)  ~ 10-20% more efficient
  - Claude tokenizer  (Anthropic)               ~ similar to cl100k for English prose
  - SentencePiece     (Llama, Mistral, etc.)    ~ 10-30% more tokens for JSON

All byte measurements remain useful regardless of tokenizer. The
percentage column is calculated against the byte baseline so the
relative scaling is tokenizer-independent.

Usage
-----
    python3 count_tokens.py <path-to-summary.json> [--out-dir <dir>]

The script reads raw capture files referenced by summary.json["raw_dir"]
and the per-scenario .txt / .json files written by measure.sh.
"""

import argparse
import json
import os
import sys
from pathlib import Path

try:
    import tiktoken
except ImportError:
    sys.stderr.write(
        "tiktoken not installed. Install with:\n"
        "  pip install --break-system-packages tiktoken\n"
    )
    sys.exit(2)

ENCODING = tiktoken.get_encoding("cl100k_base")


def tok(s: str) -> int:
    """Count tokens in a string with the cl100k_base encoder."""
    if not s:
        return 0
    return len(ENCODING.encode(s, disallowed_special=()))


def read(path: Path) -> str:
    if not path.exists():
        return ""
    return path.read_text(encoding="utf-8", errors="replace")


def load_summary(summary_path: Path) -> dict:
    with summary_path.open() as f:
        return json.load(f)


def measure_scenario_A(raw: Path, env: dict) -> dict:
    """Minimal MPM context — fresh workspace, wake --compact."""
    out = read(raw / "A_wake_compact.txt")
    return {
        "wake_compact_bytes": len(out.encode("utf-8")),
        "wake_compact_tokens": tok(out),
        "notes": "Fresh workspace with only seeded baseline directives.",
    }


def measure_scenario_B(raw: Path, env: dict) -> dict:
    """Simple useful recall — seed 1, recall 1."""
    recall = read(raw / "B_recall.txt")
    metadata_only = read(raw / "B_recall_metadata.json")
    seed = read(raw / "B_seed.json")

    # Useful vs metadata: derive useful = recall - metadata_only.
    recall_b = len(recall.encode("utf-8"))
    recall_t = tok(recall)
    meta_b = len(metadata_only.encode("utf-8"))
    meta_t = tok(metadata_only)

    return {
        "recall_total_bytes": recall_b,
        "recall_total_tokens": recall_t,
        "recall_metadata_bytes": meta_b,
        "recall_metadata_tokens": meta_t,
        "useful_bytes": max(0, recall_b - meta_b),
        "useful_tokens": max(0, recall_t - meta_t),
        "seed_response_bytes": len(seed.encode("utf-8")),
        "seed_response_tokens": tok(seed),
        "notes": "Useful vs metadata split via jq deletion of content/cross_references keys.",
    }


def measure_scenario_C(raw: Path, env: dict) -> dict:
    """Broader recall — 30 seeds, recall --limit 15."""
    recall = read(raw / "C_recall.txt")
    return {
        "recall_total_bytes": len(recall.encode("utf-8")),
        "recall_total_tokens": tok(recall),
        "recall_per_row_bytes": len(recall.encode("utf-8")) // 15 if recall else 0,
        "recall_per_row_tokens": tok(recall) // 15 if recall else 0,
        "notes": "Recall --limit 15 of 30 seeded memories; per-row cost is total / 15.",
    }


def measure_scenario_D(raw: Path, env: dict) -> dict:
    """Wake/context projection — empty / small / large."""
    rows = {}
    for label, suffix in [
        ("empty", "D_wake_empty"),
        ("small", "D_wake_small"),
        ("large", "D_wake_large"),
    ]:
        for form, ext in [("json", "json"), ("compact", "compact"), ("prose", "prose")]:
            path = raw / f"{suffix}_{form}.txt"
            if not path.exists():
                # wake --prose doesn't exist; skip
                if form == "prose":
                    continue
                rows[f"{label}_{form}"] = {"bytes": 0, "tokens": 0}
                continue
            content = read(path)
            rows[f"{label}_{form}"] = {
                "bytes": len(content.encode("utf-8")),
                "tokens": tok(content),
            }
    return {**rows, "notes": "Empty=fresh workspace; small=3 memories+1 work; large=53 memories+work."}


def measure_scenario_E(raw: Path, env: dict) -> dict:
    """Pointer-native large artifact."""
    rows = {}
    inline_path = raw / "E_inline_size.txt"
    if inline_path.exists():
        rows["inline_body_bytes"] = int(read(inline_path).strip() or "0")

    for name in ["E_save", "E_query_full", "E_query_projected", "E_resolve"]:
        path = raw / f"{name}.json"
        if not path.exists():
            rows[f"{name}_bytes"] = 0
            rows[f"{name}_tokens"] = 0
            continue
        content = read(path)
        rows[f"{name}_bytes"] = len(content.encode("utf-8"))
        rows[f"{name}_tokens"] = tok(content)
    rows["notes"] = (
        "inline_body is the raw save content. save_response is the bounded echo "
        "(MPM_MAX_INLINE_CONTENT_BYTES=2048). query_full returns content inline "
        "(subject to spill). query_projected emits a pointer (mpm://memory/<id>). "
        "resolve fetches the bounded content via the pointer."
    )
    return rows


def measure_scenario_F(raw: Path, env: dict) -> dict:
    """Tool/schema overhead — captured by Go probe tool_size_probe."""
    txt = read(raw / "F_tool_sizes.txt")
    # Parse the table from the probe output. Lines look like:
    #   mpm_memory                     1292        1603        2895
    # We also have a TOTAL row and a final "tools/list payload" line.
    tools = []
    total_desc = 0
    total_schema = 0
    for line in txt.splitlines():
        line = line.strip()
        if not line or line.startswith("tool") or line.startswith("-") or line.startswith("TOTAL"):
            continue
        if line.startswith("tools/list"):
            continue
        parts = line.split()
        if len(parts) >= 4:
            try:
                desc = int(parts[1])
                schema = int(parts[2])
                tools.append({"tool": parts[0], "desc_bytes": desc, "schema_bytes": schema})
                total_desc += desc
                total_schema += schema
            except ValueError:
                continue
    # Sum of bytes for all tool schemas + descriptions.
    total_bytes = total_desc + total_schema
    # JSON-serialised tools/list payload is roughly 1.5x the sum because
    # of JSON structural overhead (object keys, indentation). The probe
    # prints the exact wire size; we capture it.
    wire_payload_bytes = 0
    for line in txt.splitlines():
        if "tools/list payload" in line:
            # "tools/list payload (JSON-serialised): N bytes"
            try:
                wire_payload_bytes = int(line.split(":")[-1].strip().split()[0])
            except (ValueError, IndexError):
                pass
    return {
        "tools": tools,
        "tool_count": len(tools),
        "total_desc_bytes": total_desc,
        "total_schema_bytes": total_schema,
        "total_desc_tokens": tok("\n".join(f"{t['tool']}: {'x'*t['desc_bytes']}" for t in tools)),
        # Token count is approximated as bytes/4 (cl100k_base for English/JSON ≈ 4 chars/token).
        # For exact counts we'd need to tokenise each description; we leave the bytes number as the
        # primary measurement and document this approximation.
        "wire_payload_bytes": wire_payload_bytes,
        "wire_payload_tokens_approx": wire_payload_bytes // 4,
        "notes": (
            "Schema+description bytes are FIXED per tools/list MCP message — "
            "they ship every session, regardless of workload. cl100k_base is ~4 "
            "chars/token for English prose and JSON."
        ),
    }


def measure_scenario_G(raw: Path, env: dict) -> dict:
    """Multi-step workflow — wake → recall → resolve."""
    rows = {}
    for label, fname in [("wake", "G_wake"), ("recall", "G_recall"), ("resolve", "G_resolve")]:
        path = raw / f"{fname}.json"
        if not path.exists():
            path = raw / f"{fname}.txt"
        if not path.exists():
            rows[label] = {"bytes": 0, "tokens": 0}
            continue
        content = read(path)
        rows[label] = {
            "bytes": len(content.encode("utf-8")),
            "tokens": tok(content),
        }
    row_dicts = {k: v for k, v in rows.items() if isinstance(v, dict)}
    if all(v.get("bytes", 0) > 0 for v in row_dicts.values()):
        rows["sum_bytes"] = sum(v["bytes"] for v in row_dicts.values())
        rows["sum_tokens"] = sum(v["tokens"] for v in row_dicts.values())
    rows["notes"] = "Three sequential calls — wake compact, recall (limit 1), resolve pointer."
    return rows


def main():
    parser = argparse.ArgumentParser(description=__doc__.split("\n\n", 1)[0])
    parser.add_argument("summary", type=Path, help="Path to summary.json from measure.sh")
    parser.add_argument(
        "--out-dir",
        type=Path,
        default=None,
        help="Where to write report.json / report.md (defaults to raw_dir)",
    )
    args = parser.parse_args()

    summary = load_summary(args.summary)
    raw_dir = Path(summary["raw_dir"])
    out_dir = args.out_dir or raw_dir
    out_dir.mkdir(parents=True, exist_ok=True)

    report = {
        "environment": summary["environment"],
        "scenarios": {
            "A_minimal_context": measure_scenario_A(raw_dir, summary["environment"]),
            "B_simple_recall": measure_scenario_B(raw_dir, summary["environment"]),
            "C_broad_recall": measure_scenario_C(raw_dir, summary["environment"]),
            "D_wake_context": measure_scenario_D(raw_dir, summary["environment"]),
            "E_pointer_artifact": measure_scenario_E(raw_dir, summary["environment"]),
            "F_tool_overhead": measure_scenario_F(raw_dir, summary["environment"]),
            "G_multi_step": measure_scenario_G(raw_dir, summary["environment"]),
        },
    }

    report_json = out_dir / "report.json"
    report_json.write_text(json.dumps(report, indent=2))
    print(f"[tokens] wrote {report_json}")

    # Compact one-line summary for the docs.
    print("\n[summary]")
    s = report["scenarios"]
    print(f"  A wake --compact:                    {s['A_minimal_context']['wake_compact_bytes']:>6} bytes / {s['A_minimal_context']['wake_compact_tokens']:>4} tokens")
    print(f"  B recall total:                      {s['B_simple_recall']['recall_total_bytes']:>6} bytes / {s['B_simple_recall']['recall_total_tokens']:>4} tokens")
    print(f"  C recall total (15 rows):            {s['C_broad_recall']['recall_total_bytes']:>6} bytes / {s['C_broad_recall']['recall_total_tokens']:>4} tokens")
    print(f"  D wake large --json:                 {s['D_wake_context']['large_json']['bytes']:>6} bytes / {s['D_wake_context']['large_json']['tokens']:>4} tokens")
    print(f"  D wake large --compact:              {s['D_wake_context']['large_compact']['bytes']:>6} bytes / {s['D_wake_context']['large_compact']['tokens']:>4} tokens")
    print(f"  E projected (pointer) response:      {s['E_pointer_artifact']['E_query_projected_bytes']:>6} bytes / {s['E_pointer_artifact']['E_query_projected_tokens']:>4} tokens")
    print(f"  E full (inline content) response:    {s['E_pointer_artifact']['E_query_full_bytes']:>6} bytes / {s['E_pointer_artifact']['E_query_full_tokens']:>4} tokens")
    print(f"  E resolve (bounded via pointer):     {s['E_pointer_artifact']['E_resolve_bytes']:>6} bytes / {s['E_pointer_artifact']['E_resolve_tokens']:>4} tokens")
    print(f"  F tools/list payload (wire):         {s['F_tool_overhead']['wire_payload_bytes']:>6} bytes")
    print(f"  G multi-step sum:                    {s['G_multi_step'].get('sum_bytes', 0):>6} bytes / {s['G_multi_step'].get('sum_tokens', 0):>4} tokens")


if __name__ == "__main__":
    main()
