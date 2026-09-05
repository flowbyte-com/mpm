#!/usr/bin/env bash
# doc-gate-threshold-table.sh — positive-presence check for README §6.4
# "Output policy and pointer mechanics" threshold table.
#
# WHY THIS GATE EXISTS
# --------------------
# The threshold table in README.md §6.4 catalogs every layered cap that
# bounds MCP tool output size (transport spill, inline-echo, pointer
# resolver, blob-read ceiling, blob-search scan window, etc.). The audit
# that motivated the table's creation (commit 1527952 "document the four
# layered thresholds as a set") was re-derived from a grep sweep of source
# constants. Three subsequent passes each independently re-flagged the
# mpm_blob_search row as missing — not because the row had been dropped,
# but because the table had never included it (F-S-3 in
# docs/archive/pointer-indirection-sweep-2026-09-05.md explicitly classified
# those limits as a scan window, not a response-size cap; that classification
# was correct, but the reasoning was not visible to readers of the table
# alone).
#
# The pattern is: each pass manually re-derives "what constants exist" and
# "what rows are present" and checks one against the other. That's a
# negative check ("no stale phrase") that the existing gates already cover.
# What was missing is the positive check: every MCP-output-cap constant in
# production source MUST have a peer row in the §6.4 table.
#
# THIS IS THE POSITIVE CHECK. It is the gate that should have caught the
# mpm_blob_search omission the first time, and that catches any future
# threshold constant added to source without a corresponding row.
#
# HOW IT WORKS
# ------------
# 1. For each entry in REQUIRED_THRESHOLDS, run a source-existence regex
#    against production source (internal/core, internal/core/tools,
#    cmd/mpm, cmd/mpm-mcp). Test files are excluded.
# 2. Verify the README §6.4 threshold table contains a row mentioning the
#    anchor phrase (case-insensitive literal substring match).
# 3. Fail the gate if either check fails, naming the constant and the
#    missing piece.
#
# SCOPE
# -----
# Production-only (excludes _test.go). Test files declare their own
# `const serverMax = 256 * 1024` (e.g. spill_roundtrip_test.go) but those
# are intentional test fixtures, not live behavior.
#
# Scope is MCP-output caps only. CLI input/output limits (e.g.
# `memoryMaxFileBytes`, `maxAddBytes`, `defaultResolveLimit`) belong in the
# CLI reference (§8), not the §6.4 output-policy table. Adding them here
# would conflate two surfaces that the §6.4 narrative deliberately separates.
#
# RUN
# ---
# Standalone:   bash scripts/doc-gate-threshold-table.sh
# In pre-commit: invoked by scripts/pre-commit after the mpm-lint gate.
#
# Exit 0 if every constant has a peer row, 1 otherwise.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
README="$REPO_ROOT/README.md"

# Source roots to scan (production only — _test.go is excluded).
SRC_ROOTS=(
  "$REPO_ROOT/internal/core"
  "$REPO_ROOT/internal/core/tools"
  "$REPO_ROOT/cmd/mpm"
  "$REPO_ROOT/cmd/mpm-mcp"
)

# Required MCP-output-cap constants.
#
# Each entry: "Symbol|source-existence-regex|anchor-phrase-in-table"
#
# The anchor is matched case-insensitively as a literal substring against
# the §6.4 table region. Choose anchors that won't collide with unrelated
# prose. Disambiguate repeated names (e.g. `serverMax`, `serverMaxMatches`,
# `serverMaxBytes`) by file:line in the source-existence regex.
REQUIRED_THRESHOLDS=(
  "MaxWakeContextBytes|MaxWakeContextBytes|MaxWakeContextBytes"
  "DefaultMaxInlineContentBytes|DefaultMaxInlineContentBytes|output_limits.go:23"
  "defaultThreshold|defaultThreshold = 20480|output_policy.go"
  "mpm_blob_read server ceiling|const serverMax = 256 \* 1024|handleMpmBlobRead"
  "mpm_blob_search serverMaxMatches|const serverMaxMatches = 100|serverMaxMatches"
  "mpm_blob_search serverMaxBytes|const serverMaxBytes = 256 \* 1024|serverMaxBytes"
  "maxQueryLimit|const maxQueryLimit = 200|maxQueryLimit"
)

# Extract the §6.4 threshold-table region (lines between the H3 header and
# the next H3). Anything outside the table is not a valid row location.
awk '/^### 6\.4 MCP Integration/,/^### 6\.5/' "$README" > /tmp/doc-gate-6_4.md

fail=0

for entry in "${REQUIRED_THRESHOLDS[@]}"; do
  IFS='|' read -r label existence_regex anchor <<< "$entry"

  # 1. Source-existence check. Run the existence regex against production
  #    source; if absent, the gate author needs to remove the entry.
  if ! grep -rqE "$existence_regex" "${SRC_ROOTS[@]}" --include='*.go' 2>/dev/null; then
    echo "doc-gate: '${label}' listed in gate is missing from source." >&2
    echo "  Source-existence regex '${existence_regex}' matched no production file." >&2
    echo "  Either the gate's REQUIRED_THRESHOLDS list is stale (remove the" >&2
    echo "  entry) or a refactor renamed the constant (update both)." >&2
    fail=1
    continue
  fi

  # 2. README §6.4 row check. The anchor phrase must appear in the table.
  if ! grep -qiF "$anchor" /tmp/doc-gate-6_4.md; then
    echo "doc-gate: MCP-output cap '${label}' has no row in README §6.4." >&2
    echo "  Anchor phrase '${anchor}' not found in the §6.4 threshold table." >&2
    echo "  Add a row to README.md §6.4 citing this constant's file:line" >&2
    echo "  and what it bounds, or update REQUIRED_THRESHOLDS if the anchor" >&2
    echo "  needs to change." >&2
    fail=1
  fi
done

rm -f /tmp/doc-gate-6_4.md

if [ "$fail" -eq 0 ]; then
  echo "[doc-gate-threshold-table] OK (${#REQUIRED_THRESHOLDS[@]} MCP-output-cap constants each have a README §6.4 row)"
fi

exit "$fail"
