#!/usr/bin/env bash
# doc-gate-cli-limits.sh — positive-presence check for README §8 CLI
# Reference's "CLI-side input/output limits" table.
#
# WHY THIS GATE EXISTS
# --------------------
# README §8 (CLI Reference) catalogs every user-facing CLI cap: byte/char
# limits on input payloads and rendered output, plus row-count defaults
# for `--limit` flags. The §6.4 (MCP Integration) threshold table has its
# own parallel gate (scripts/doc-gate-threshold-table.sh) covering
# MCP-output caps. These two gates are deliberately separate: §6.4 and §8
# are distinct surfaces in the README's own narrative, and conflating
# them in tooling would blur a distinction the docs intentionally keep
# separate. This is the §8 counterpart.
#
# WHAT THIS GATE COVERS
# ---------------------
# Named `const` declarations in `cmd/mpm/` (NOT `cmd/mpm-mcp/` — that's
# §6.4's territory) that bound CLI input size, CLI rendered output size,
# or row-count defaults. Each must have a peer row in the §8 table.
#
# WHAT THIS GATE EXPLICITLY EXCLUDES
# -----------------------------------
# - Internal substrate thresholds (`ProvenanceThreshold`,
#   `HardConfidenceInvalidationThreshold`, `ClusterThreshold`,
#   `FreshnessAgeThreshold`, `slowQueryThreshold`) — these live in
#   `internal/core/`, never reach the CLI, and aren't user-facing.
# - Inline `fs.Int("limit", N, ...)` defaults — these are per-command
#   CLI flag defaults spread across 8+ files in `cmd/mpm/`. Each is
#   already represented in the auto-generated Command Catalogue block
#   (README §8.6) and the per-subcommand description prose; gating each
#   against a table row would be excessive and would duplicate the
#   Command Catalogue's role.
# - Constants already covered by the §6.4 gate
#   (`DefaultMaxInlineContentBytes`, `mpm_blob_*` ceilings, etc.).
#
# HOW IT WORKS
# ------------
# 1. For each entry in REQUIRED_CLI_LIMITS, run a source-existence regex
#    against `cmd/mpm/` (production, `_test.go` excluded).
# 2. Verify the README §8 CLI-side-limits table contains a row whose
#    `Where it lives` column cites the file:line of the constant.
# 3. Fail the gate if either check fails, naming the constant and the
#    missing piece.
#
# RUN
# ---
# Standalone:   bash scripts/doc-gate-cli-limits.sh
# In pre-commit: invoked by scripts/pre-commit after the §6.4 gate.
#
# Exit 0 if every constant has a peer row, 1 otherwise.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
README="$REPO_ROOT/README.md"

# Source root to scan. cmd/mpm/ only — cmd/mpm-mcp/ is §6.4 territory.
SRC_ROOTS=(
  "$REPO_ROOT/cmd/mpm"
)

# Required CLI-limit constants.
#
# Each entry: "label|source-existence-regex|anchor-phrase-in-table"
#
# The anchor is matched case-insensitively as a literal substring against
# the §8 CLI-side-limits table region. Choose anchors that won't collide
# with unrelated prose; the file:line citation is the most reliable
# choice since the table's `Where it lives` column already includes it.
REQUIRED_CLI_LIMITS=(
  "memoryMaxFileBytes|const memoryMaxFileBytes = 100 << 20|handlers_memory.go:23"
  "maxAddBytes|const maxAddBytes = 1 << 20|simple_cmds.go:97"
  "maxRefBytes|const maxRefBytes = 100 << 20|simple_cmds.go:921"
  "routeOutputCap|const routeOutputCap = 9500|route_render.go:179"
  "routeModeHardCap|const routeModeHardCap = 9000|route_render.go:180"
  "defaultResolveLimit|const defaultResolveLimit = 100|ops_resolve_contradictions.go:40"
  "defaultDecisionsListLimit|const defaultDecisionsListLimit = 20|handlers_epistemology.go:839"
)

# Extract the §8 CLI-side-limits table region. The table sits inside a
# `### CLI-side input/output limits` H3 subsection; that anchor phrase
# is what the gen-cli block always emits alongside, so it's stable.
# If the §8 structure is ever reorganized, this awk range will need
# updating — the alternative (parsing all of §8) is too noisy.
awk '
  /^### CLI-side input\/output limits/ { in_table=1; print; next }
  in_table && /^### / { exit }
  in_table { print }
' "$README" > /tmp/doc-gate-cli-limits.md

# If the section does not exist yet, fall back to searching the whole
# of §8. This is the bootstrap mode: the section is added as part of
# the same arc that introduces this gate, so before the section lands
# the gate will not find any anchors and will fail loudly with
# "no row in README §8" — that is the intended first run.
if [ ! -s /tmp/doc-gate-cli-limits.md ]; then
  awk '/^## 8\. CLI Reference/,/^## 9\./' "$README" > /tmp/doc-gate-cli-limits.md
  FALLBACK=1
else
  FALLBACK=0
fi

fail=0

for entry in "${REQUIRED_CLI_LIMITS[@]}"; do
  IFS='|' read -r label existence_regex anchor <<< "$entry"

  # 1. Source-existence check.
  if ! grep -rqE "$existence_regex" "${SRC_ROOTS[@]}" --include='*.go' 2>/dev/null; then
    echo "doc-gate: '${label}' listed in gate is missing from source." >&2
    echo "  Source-existence regex '${existence_regex}' matched no production file in cmd/mpm/." >&2
    echo "  Either the gate's REQUIRED_CLI_LIMITS list is stale (remove the" >&2
    echo "  entry) or a refactor renamed the constant (update both)." >&2
    fail=1
    continue
  fi

  # 2. README §8 row check. The anchor phrase must appear in the
  #    table region.
  if ! grep -qiF "$anchor" /tmp/doc-gate-cli-limits.md; then
    echo "doc-gate: CLI-side limit '${label}' has no row in README §8." >&2
    echo "  Anchor phrase '${anchor}' not found in the §8 CLI-side-limits table." >&2
    echo "  Add a row to README.md §8's CLI-side input/output limits table" >&2
    echo "  citing this constant's file:line and what it bounds, or update" >&2
    echo "  REQUIRED_CLI_LIMITS if the anchor needs to change." >&2
    fail=1
  fi
done

rm -f /tmp/doc-gate-cli-limits.md

if [ "$fail" -eq 0 ]; then
  if [ "$FALLBACK" -eq 1 ]; then
    echo "[doc-gate-cli-limits] OK (${#REQUIRED_CLI_LIMITS[@]} CLI-side limits each have a README §8 row; section anchor not yet created)"
  else
    echo "[doc-gate-cli-limits] OK (${#REQUIRED_CLI_LIMITS[@]} CLI-side limits each have a README §8 row)"
  fi
fi

exit "$fail"
