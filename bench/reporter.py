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
    raw_cosine_unprefixed_scoring: dict[str, Any] | None
    raw_cosine_prefixed_scoring: dict[str, Any] | None
    raw_cosine_delta: dict[str, Any] | None
    labelled_scoring: dict[str, Any] | None
    isolation: dict[str, Any]
    snapshot_logical_mutation: dict[str, Any]
    per_query_table: list[dict[str, Any]]
    raw_cosine_per_query_table: list[dict[str, Any]] | None
    raw_cosine_prefixed_per_query_table: list[dict[str, Any]] | None
    historical_note: str
    output_dir: Path

    def to_json(self) -> str:
        return json.dumps(
            {
                "corpus": self.corpus,
                "regeneration": self.regeneration,
                "participation": self.participation,
                "raw_cosine_participation": self.raw_cosine_participation,
                "raw_cosine_unprefixed_scoring": self.raw_cosine_unprefixed_scoring,
                "raw_cosine_prefixed_scoring": self.raw_cosine_prefixed_scoring,
                "raw_cosine_delta": self.raw_cosine_delta,
                "labelled_scoring": self.labelled_scoring,
                "isolation": self.isolation,
                "snapshot_logical_mutation": self.snapshot_logical_mutation,
                "per_query_table": self.per_query_table,
                "raw_cosine_per_query_table": self.raw_cosine_per_query_table,
                "raw_cosine_prefixed_per_query_table": self.raw_cosine_prefixed_per_query_table,
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

        lines.append("## Snapshot logical mutation (gate 7, v3 per-query)")
        sm = self.snapshot_logical_mutation
        lines.append(f"- Status: **{sm.get('status', 'unknown')}**")
        for d in sm.get("diffs", []):
            lines.append(
                f"  - `{d.get('table', '?')}`: "
                f"+{d.get('rows_added', 0)} / -{d.get('rows_removed', 0)} rows"
            )
        lines.append(
            "- Coverage: memories, reference_chunks, retrieval_metadata, "
            "contradiction_queue, challenges (v3)"
        )
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

        # Real labelled relevance scoring (priority 3 of HAL's review).
        if self.labelled_scoring is not None:
            lines.append("## Labelled relevance scoring (priority 3)")
            s = self.labelled_scoring
            agg = s.get("aggregates", {})
            lines.append(f"- top-k: `{s.get('k')}` (from corpus.scoring.top_k; v3)")
            lines.append(
                f"- Positive queries scored: `{s.get('positive_total')}` "
                f"(negative: `{s.get('negative_total')}`)"
            )
            lines.append(
                f"- Mean Precision@k: `{agg.get('mean_precision_at_k'):.4f}`"
            )
            lines.append(
                f"- Mean Recall@k:    `{agg.get('mean_recall_at_k'):.4f}`"
            )
            lines.append(
                f"- Mean MRR:         `{agg.get('mean_mrr'):.4f}`"
            )
            lines.append(
                f"- Mean nDCG@k:      `{agg.get('mean_ndcg_at_k'):.4f}`"
            )
            lines.append("")

        lines.append("## Isolation (C4 / gate 7)")
        for k, v in self.isolation.items():
            lines.append(f"- **{k}**: `{v}`")
        lines.append("")

        if self.raw_cosine_unprefixed_scoring is not None:
            lines.append("## Raw-cosine experiment (v3: unprefixed + prefixed)")
            ru = self.raw_cosine_unprefixed_scoring
            rag = ru.get("aggregates", {})
            lines.append(
                "- Method: independent raw-cosine ranking, NOT hybrid recall"
            )
            lines.append(
                f"- **Unprefixed** query embedding: Mean Precision@k="
                f"`{rag.get('mean_precision_at_k'):.4f}`, Recall@k="
                f"`{rag.get('mean_recall_at_k'):.4f}`, MRR="
                f"`{rag.get('mean_mrr'):.4f}`, nDCG@k="
                f"`{rag.get('mean_ndcg_at_k'):.4f}`"
            )
            if self.raw_cosine_prefixed_scoring is not None:
                rp = self.raw_cosine_prefixed_scoring
                pag = rp.get("aggregates", {})
                lines.append(
                    f"- **Prefixed** (`search_query: `) query embedding: Mean "
                    f"Precision@k=`{pag.get('mean_precision_at_k'):.4f}`, "
                    f"Recall@k=`{pag.get('mean_recall_at_k'):.4f}`, MRR="
                    f"`{pag.get('mean_mrr'):.4f}`, nDCG@k="
                    f"`{pag.get('mean_ndcg_at_k'):.4f}`"
                )
                if self.raw_cosine_delta is not None:
                    d = self.raw_cosine_delta.get("prefixed_minus_unprefixed", {})
                    lines.append(
                        f"- **Delta** (prefixed - unprefixed): "
                        f"ΔPrecision@k=`{d.get('mean_precision_at_k', 0.0):+.4f}`, "
                        f"ΔRecall@k=`{d.get('mean_recall_at_k', 0.0):+.4f}`, "
                        f"ΔMRR=`{d.get('mean_mrr', 0.0):+.4f}`, "
                        f"ΔnDCG@k=`{d.get('mean_ndcg_at_k', 0.0):+.4f}`"
                    )
            else:
                lines.append(
                    "- Prefixed comparison skipped: no embedding profile "
                    "available; the hash fallback would be a real-model "
                    "anti-pattern (v3)."
                )
            lines.append("")

        lines.append("## Per-query table (hybrid ranker)")
        lines.append("")
        lines.append("| query_id | class | n_rows | sources | vector_participated | search_mode |")
        lines.append("|---|---|---|---|---|---|")
        for row in self.per_query_table:
            lines.append(
                f"| `{row['query_id']}` | {row['class']} | {row['n_rows']} | "
                f"{row['sources']} | {row['vector_participated']} | "
                f"{row.get('search_mode', '')} |"
            )
        lines.append("")

        if self.raw_cosine_per_query_table is not None:
            lines.append("## Per-query table (raw-cosine experiment — unprefixed)")
            lines.append("")
            lines.append("| query_id | class | n_rows | top_id | top_score |")
            lines.append("|---|---|---|---|---|")
            for row in self.raw_cosine_per_query_table:
                lines.append(
                    f"| `{row['query_id']}` | {row['class']} | {row['n_rows']} | "
                    f"`{row['top_id']}` | {row['top_score']} |"
                )
            lines.append("")

        if self.raw_cosine_prefixed_per_query_table is not None:
            lines.append("## Per-query table (raw-cosine experiment — `search_query:` prefixed)")
            lines.append("")
            lines.append("| query_id | class | n_rows | top_id | top_score |")
            lines.append("|---|---|---|---|---|")
            for row in self.raw_cosine_prefixed_per_query_table:
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
