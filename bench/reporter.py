"""Render the run report.

The reporter takes the structured outputs from the rest of the
harness (corpus summary, regeneration verification, per-query
participation) and writes a human-readable Markdown report plus
a machine-readable JSON report.
"""

from __future__ import annotations

import json
from dataclasses import dataclass
from pathlib import Path
from typing import Any


@dataclass
class RunReport:
    corpus: dict[str, Any]
    regeneration: dict[str, Any]
    participation: dict[str, Any]
    raw_cosine_participation: dict[str, Any] | None
    isolation: dict[str, Any]
    snapshot_logical_mutation: dict[str, Any]
    live_store: dict[str, Any]
    per_query_table: list[dict[str, Any]]
    raw_cosine_per_query_table: list[dict[str, Any]] | None
    historical_note: str
    output_dir: Path

    def to_json(self) -> str:
        return json.dumps(
            {
                "corpus": self.corpus,
                "regeneration": self.regeneration,
                "participation": self.participation,
                "raw_cosine_participation": self.raw_cosine_participation,
                "isolation": self.isolation,
                "snapshot_logical_mutation": self.snapshot_logical_mutation,
                "live_store": self.live_store,
                "per_query_table": self.per_query_table,
                "raw_cosine_per_query_table": self.raw_cosine_per_query_table,
                "historical_note": self.historical_note,
                "output_dir": str(self.output_dir),
            },
            indent=2,
        )

    def to_markdown(self) -> str:
        lines: list[str] = []
        lines.append("# Embedding Benchmark Harness Run Report")
        lines.append("")
        lines.append("## Corpus")
        for k, v in self.corpus.items():
            lines.append(f"- **{k}**: `{v}`")
        lines.append("")
        lines.append(self.historical_note)
        lines.append("")

        lines.append("## Regeneration verification (gate 1)")
        ok = "OK" if self.regeneration.get("ok") else "FAILED"
        lines.append(f"- Status: **{ok}**")
        lines.append(f"- Expected: `{self.regeneration.get('expected')}`")
        if self.regeneration.get("failures"):
            lines.append("- Failures:")
            for f in self.regeneration["failures"]:
                lines.append(f"  - {f}")
        lines.append("")

        lines.append("## Snapshot logical mutation (gate 7)")
        lines.append(f"- Status: **{self.snapshot_logical_mutation.get('status', 'unknown')}**")
        for k, v in self.snapshot_logical_mutation.items():
            if k != "status":
                lines.append(f"- {k}: `{v}`")
        lines.append("")

        lines.append("## Live store sentinel (gate 7)")
        lines.append(f"- Status: **{self.live_store.get('status', 'unknown')}**")
        for k, v in self.live_store.items():
            if k != "status":
                lines.append(f"- {k}: `{v}`")
        lines.append("")

        lines.append("## Vector participation (C5 / gate 4 v2)")
        p = self.participation
        lines.append(
            f"- Positive queries: {p.get('positive_vector_participated')}/{p.get('positive_total')} vector-participated"
        )
        if p.get("positive_failures"):
            lines.append("- Positive failures:")
            for f in p["positive_failures"]:
                lines.append(f"  - `{f.get('query_id')}`: {f.get('reason')}")
        n = p.get("negative_outcomes", {})
        lines.append(
            f"- Negative queries: {p.get('negative_total')} total — "
            f"correctly_empty={len(n.get('correctly_empty', []))}, "
            f"vector_participated={len(n.get('vector_participated', []))}, "
            f"fts5_only={len(n.get('fts5_only', []))}"
        )
        lines.append("")

        lines.append("## Isolation (C4 / gate 7)")
        for k, v in self.isolation.items():
            lines.append(f"- **{k}**: `{v}`")
        lines.append("")

        if self.raw_cosine_participation is not None:
            lines.append("## Raw-cosine prefix experiment (gate 4 of impl gates)")
            rp = self.raw_cosine_participation
            lines.append(
                f"- Method: independent raw-cosine ranking, NOT hybrid recall"
            )
            lines.append(
                f"- Positive queries: {rp.get('positive_vector_participated')}/{rp.get('positive_total')} participated"
            )
            lines.append("")

        lines.append("## Per-query table (hybrid ranker)")
        lines.append("")
        lines.append("| query_id | class | n_rows | sources | vector_participated |")
        lines.append("|---|---|---|---|---|")
        for row in self.per_query_table:
            lines.append(
                f"| `{row['query_id']}` | {row['class']} | {row['n_rows']} | "
                f"{row['sources']} | {row['vector_participated']} |"
            )
        lines.append("")

        if self.raw_cosine_per_query_table is not None:
            lines.append("## Per-query table (raw-cosine experiment)")
            lines.append("")
            lines.append("| query_id | class | n_rows | top_id | top_score |")
            lines.append("|---|---|---|---|---|")
            for row in self.raw_cosine_per_query_table:
                lines.append(
                    f"| `{row['query_id']}` | {row['class']} | {row['n_rows']} | "
                    f"`{row['top_id']}` | {row['top_score']} |"
                )
            lines.append("")

        return "\n".join(lines) + "\n"


def write_report(report: RunReport) -> tuple[Path, Path]:
    """Write the JSON and Markdown reports to the output dir. Returns
    (json_path, md_path)."""
    report.output_dir.mkdir(parents=True, exist_ok=True, mode=0o700)
    json_path = report.output_dir / "report.json"
    md_path = report.output_dir / "report.md"
    json_path.write_text(report.to_json())
    md_path.write_text(report.to_markdown())
    return json_path, md_path
