#!/usr/bin/env bash
# scripts/context_economics/measure.sh
#
# Reproducible measurement harness for MPM model-facing context economics.
#
# What this script does:
#   1. Creates a disposable workspace under a temp directory.
#   2. Builds the mpm binary in ./bin/mpm (or uses a pre-built one).
#   3. Captures model-facing surfaces for each scenario in the brief:
#        A. Minimal context (no useful recall content)
#        B. Simple useful recall
#        C. Broader recall
#        D. Wake context (compact + JSON + prose, with varying payloads)
#        E. Pointer-native large artifact
#        F. Tool/schema overhead (measured via separate Go probe)
#        G. Multi-step workflow
#   4. Writes raw stdout to /tmp/mpm-econ/<scenario>.txt
#   5. Writes the byte counts to /tmp/mpm-econ/summary.json
#
# A separate Python script (count_tokens.py) reads the captured text and
# applies tiktoken's cl100k_base encoding to produce token counts, then
# merges with the summary.json to produce the final report.
#
# Why two scripts: bash for orchestration is portable and the harness
# stays readable; Python for tokenization because tiktoken is the most
# defensible cross-vendor token counter available.

set -euo pipefail

PROJECT_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
BIN="${PROJECT_ROOT}/bin/mpm"
TMPDIR_OUT="${TMPDIR:-/tmp}/mpm-econ-$$"
WORKSPACE="${TMPDIR_OUT}/workspace"
RAW_DIR="${TMPDIR_OUT}/raw"
mkdir -p "${RAW_DIR}"

# Force a fresh build at the pinned commit so the measurements reproduce
# from this tree alone.
echo "[econ] building mpm at HEAD..."
(cd "${PROJECT_ROOT}" && go build -tags fts5 -o "${BIN}" ./cmd/mpm/) >/dev/null

echo "[econ] workspace: ${WORKSPACE}"
echo "[econ] raw captures: ${RAW_DIR}"
mkdir -p "${WORKSPACE}"

export MPM_WORKSPACE="${WORKSPACE}"
export MPM_QUIET_LOGS=1

# Helper: capture a mpm invocation's stdout + measure its bytes.
capture() {
  local label="$1"; shift
  local out="${RAW_DIR}/${label}.txt"
  "${BIN}" "$@" >"${out}" 2>/dev/null || true
  local bytes
  bytes=$(wc -c <"${out}")
  echo "${bytes}"
}

# --------------------------------------------------------------------------
# Scenario A — minimal MPM context.
# Fresh workspace. mpm wake --compact emits the smallest wake envelope
# the substrate produces (no useful content). This is the substrate floor.
# --------------------------------------------------------------------------

echo "[econ] A: minimal context (fresh workspace, wake --compact)"
W_COMPACT_BYTES=$(capture "A_wake_compact" wake --compact)

# --------------------------------------------------------------------------
# Scenario B — simple useful recall.
# Seed ONE memory, then recall it. The recall row IS the useful content.
# Useful = `content` text; metadata = everything else in the envelope.
# --------------------------------------------------------------------------

echo "[econ] B: simple recall (seed 1, recall 1)"
"${BIN}" add --json "single-test-fact-the-quick-brown-fox-jumps-over-the-lazy-dog" >"${RAW_DIR}/B_seed.json" 2>/dev/null
SEED_ID=$(jq -r .id "${RAW_DIR}/B_seed.json")
B_RECALL_BYTES=$(capture "B_recall" recall --json --limit 1 "fox jumps")

# Useful vs metadata split: re-render without content to get metadata size.
# We do this by piping the recall through jq and dropping the content key.
jq 'del(.memories[].content) | del(.memories[].cross_references)' \
  "${RAW_DIR}/B_recall.txt" > "${RAW_DIR}/B_recall_metadata.json" 2>/dev/null || \
  cp "${RAW_DIR}/B_recall.txt" "${RAW_DIR}/B_recall_metadata.json"
B_METADATA_BYTES=$(wc -c <"${RAW_DIR}/B_recall_metadata.json")
# Useful content bytes = total - envelope - per-row metadata. We approximate
# the envelope as the bytes before the first "content" key.

# --------------------------------------------------------------------------
# Scenario C — broader recall.
# Seed 30 memories; recall returns multiple rows. Captures scaling of
# fixed-cost overhead (envelope) vs variable (per-row content).
# --------------------------------------------------------------------------

echo "[econ] C: broader recall (seed 30, recall many)"
SEED_FILE="${RAW_DIR}/C_seeds.txt"
: >"${SEED_FILE}"
for i in $(seq 1 30); do
  "${BIN}" add --json "broader-recall-fact-number-${i}-alpha-bravo-charlie-delta-echo" \
    >>"${SEED_FILE}" 2>/dev/null || true
done
C_RECALL_BYTES=$(capture "C_recall" recall --json --limit 15 "alpha bravo")

# --------------------------------------------------------------------------
# Scenario D — wake/context projection.
# Three sizes: empty, small, large. Captures fixed framing vs payload.
# --------------------------------------------------------------------------

echo "[econ] D: wake context (empty / small / large)"
# D1: empty — the workspace already has only baseline directives.
D1_JSON_BYTES=$(capture "D_wake_empty_json" wake --json)
D1_COMPACT_BYTES=$(capture "D_wake_empty_compact" wake --compact)

# D2: small — seed 3 memories + 1 open work + 1 decision, then wake.
"${BIN}" add "wake-context-small-fact-A" >/dev/null 2>&1
"${BIN}" add "wake-context-small-fact-B" >/dev/null 2>&1
"${BIN}" add "wake-context-small-fact-C" >/dev/null 2>&1
"${BIN}" call mpm_work --payload '{"action":"create","params":{"title":"small-work-item"}}' \
  >/dev/null 2>&1 || true
D2_JSON_BYTES=$(capture "D_wake_small_json" wake --json)
D2_COMPACT_BYTES=$(capture "D_wake_small_compact" wake --compact)
D2_PROSE_BYTES=$(capture "D_wake_small_prose" wake)

# D3: large — seed 50 more memories, multiple open works.
for i in $(seq 1 50); do
  "${BIN}" add "wake-context-large-fact-${i}-additional-content-padding-the-wake-payload" \
    >/dev/null 2>&1
done
D3_JSON_BYTES=$(capture "D_wake_large_json" wake --json)
D3_COMPACT_BYTES=$(capture "D_wake_large_compact" wake --compact)
D3_PROSE_BYTES=$(capture "D_wake_large_prose" wake)

# --------------------------------------------------------------------------
# Scenario E — pointer-native large artifact.
# Two paths:
#   E_inline: mpm call mpm_memory save with a large content body, capture
#             the inline echo (which is bounded by MPM_MAX_INLINE_CONTENT_BYTES
#             default 2048) vs the full body.
#   E_pointer: mpm call mpm_memory query with --projected, get a pointer row.
# --------------------------------------------------------------------------

echo "[econ] E: pointer-native large artifact"
# 1 MiB of deterministic, non-sensitive content.
LARGE_BODY=$(python3 - <<'PY'
import sys
chunk = "abcdefghij" * 100   # 1000 chars
content = (chunk + "\n") * 1100   # ~1.1 MiB
sys.stdout.write(content)
PY
)
echo "${#LARGE_BODY}" > "${RAW_DIR}/E_inline_size.txt"

# Save the large body via mpm_memory action=save (MCP substrate).
# mpm_memory.save requires `fact` (canonical field name), not `content`.
PAYLOAD=$(python3 -c "import json,sys; sys.stdout.write(json.dumps({'action':'save','params':{'fact':open('/dev/stdin').read()}}))" <<<"${LARGE_BODY}")
echo "${PAYLOAD}" | "${BIN}" call mpm_memory --payload-file=/dev/stdin \
  >"${RAW_DIR}/E_save.json" 2>/dev/null || true
LARGE_ID=$(jq -r '.id // .result.id // empty' "${RAW_DIR}/E_save.json" 2>/dev/null || echo "")
E_SAVE_BYTES=$(wc -c <"${RAW_DIR}/E_save.json")

# Query via projected (pointer-native) — mpm_memory.query with
# projection=summary emits one row per match with a 256-char summary
# and an mpm://memory/<id> pointer instead of the full content.
if [ -n "${LARGE_ID}" ]; then
  "${BIN}" call mpm_memory --payload '{"action":"query","params":{"query":"abcdefghij","projection":"summary","limit":1}}' \
    >"${RAW_DIR}/E_query_projected.json" 2>/dev/null || true
  E_PROJECTED_BYTES=$(wc -c <"${RAW_DIR}/E_query_projected.json")
fi

# For comparison: same query with projection=full emits the full content
# inline. This is the "what it would look like without pointer
# projection" measurement — the entire 1.1 MB body is in the response.
if [ -n "${LARGE_ID}" ]; then
  "${BIN}" call mpm_memory --payload '{"action":"query","params":{"query":"abcdefghij","projection":"full","limit":1}}' \
    >"${RAW_DIR}/E_query_full.json" 2>/dev/null || true
  E_FULL_BYTES=$(wc -c <"${RAW_DIR}/E_query_full.json")
fi

# Resolve the pointer and capture the bounded content. mpm_resolve
# requires `uri` at the TOP level of the payload (not nested under
# params like mpm_memory), and `max_bytes` controls the materialisation
# ceiling (default 512).
if [ -n "${LARGE_ID}" ]; then
  "${BIN}" call mpm_resolve --payload "{\"uri\":\"mpm://memory/${LARGE_ID}\",\"max_bytes\":512}" \
    >"${RAW_DIR}/E_resolve.json" 2>/dev/null || true
  E_RESOLVE_BYTES=$(wc -c <"${RAW_DIR}/E_resolve.json")
fi

# --------------------------------------------------------------------------
# Scenario F — tool/schema overhead.
# Run a Go probe that registers the MCP tools and serialises the schemas
# the way the MCP server does at boot. Captures the actual bytes that
# reach the model boundary via tools/list.
# --------------------------------------------------------------------------

echo "[econ] F: tool/schema overhead"
# The tool_size_probe lives in the internal/core/tools module, which has
# its own go.mod (see CLAUDE.md §3). Run it from inside that module so
# go can find its dependencies.
(cd "${PROJECT_ROOT}/internal/core/tools" && go run ./tool_size_probe) \
  >"${RAW_DIR}/F_tool_sizes.txt" 2>/dev/null || \
  echo "F probe unavailable" > "${RAW_DIR}/F_tool_sizes.txt"

# --------------------------------------------------------------------------
# Scenario G — multi-step workflow.
# Sequence: wake → recall → resolve.
# --------------------------------------------------------------------------

echo "[econ] G: multi-step workflow"
G_WAKE_BYTES=$(capture "G_wake" wake --compact)
G_RECALL_BYTES=$(capture "G_recall" recall --json --limit 1 "fox jumps")
if [ -n "${SEED_ID:-}" ]; then
  "${BIN}" call mpm_resolve \
    --payload "{\"uri\":\"mpm://memory/${SEED_ID}\",\"max_bytes\":512}" \
    >"${RAW_DIR}/G_resolve.json" 2>/dev/null || true
  G_RESOLVE_BYTES=$(wc -c <"${RAW_DIR}/G_resolve.json")
fi

# --------------------------------------------------------------------------
# Write summary.json — bytes only; tokens are added by count_tokens.py.
# --------------------------------------------------------------------------

cat > "${RAW_DIR}/summary.json" <<EOF
{
  "environment": {
    "commit": "$(git -C "${PROJECT_ROOT}" rev-parse HEAD)",
    "go_version": "$(go version | awk '{print $3}')",
    "os": "$(uname -srm | tr ' ' '_')",
    "date": "$(date -u +%Y-%m-%dT%H:%M:%SZ)",
    "tokenizer": "tiktoken.cl100k_base (target: OpenAI gpt-3.5/4 family approximation)"
  },
  "scenarios": {
    "A_minimal_context": {
      "wake_compact_bytes": ${W_COMPACT_BYTES}
    },
    "B_simple_recall": {
      "recall_bytes": ${B_RECALL_BYTES},
      "recall_metadata_only_bytes": ${B_METADATA_BYTES}
    },
    "C_broad_recall": {
      "recall_bytes": ${C_RECALL_BYTES}
    },
    "D_wake_context": {
      "empty_json_bytes": ${D1_JSON_BYTES},
      "empty_compact_bytes": ${D1_COMPACT_BYTES},
      "small_json_bytes":  ${D2_JSON_BYTES},
      "small_compact_bytes":${D2_COMPACT_BYTES},
      "small_prose_bytes":  ${D2_PROSE_BYTES},
      "large_json_bytes":   ${D3_JSON_BYTES},
      "large_compact_bytes":${D3_COMPACT_BYTES},
      "large_prose_bytes":  ${D3_PROSE_BYTES}
    },
    "E_pointer_artifact": {
      "inline_body_bytes":   $(cat "${RAW_DIR}/E_inline_size.txt"),
      "save_response_bytes": ${E_SAVE_BYTES},
      "projected_bytes":     ${E_PROJECTED_BYTES:-0},
      "full_query_bytes":    ${E_FULL_BYTES:-0},
      "resolve_bytes":       ${E_RESOLVE_BYTES:-0}
    },
    "G_multi_step": {
      "wake_bytes":     ${G_WAKE_BYTES},
      "recall_bytes":   ${G_RECALL_BYTES},
      "resolve_bytes":  ${G_RESOLVE_BYTES:-0}
    }
  },
  "raw_dir": "${RAW_DIR}"
}
EOF

echo "[econ] raw captures written to ${RAW_DIR}"
echo "[econ] byte summary written to ${RAW_DIR}/summary.json"
echo "[econ] next: run scripts/context_economics/count_tokens.py ${RAW_DIR}/summary.json"
