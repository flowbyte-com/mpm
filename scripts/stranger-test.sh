#!/usr/bin/env bash
#
# scripts/stranger-test.sh — permanent release gate.
#
# Simulates a fresh user discovering MPM for the first time. Boots
# MPM against an isolated $HOME and $MPM_WORKSPACE, then walks the
# docs/SPEC.md §5.3 quickstart end to end. Exits non-zero on the first
# command that fails to produce the documented behavior.
#
# Usage:
#   ./scripts/stranger-test.sh                    # default: uses ./bin/mpm
#   MPM_BIN=/abs/path/to/mpm ./scripts/stranger-test.sh
#   STRANGER_VERBOSE=1 ./scripts/stranger-test.sh # print every command
#   STRANGER_KEEP=1 ./scripts/stranger-test.sh    # keep scratch dir for forensics
#
# Exit codes:
#   0 — all quickstart steps behaved as documented
#   1 — at least one step failed (the failing step prints to stderr)
#   2 — environment error (binary missing, mktemp failed, etc.)
#
# The script is intentionally hermetic: no global state mutated,
# no config written to the user's real $HOME. Every file it creates
# lives under a single mktemp directory that is removed on exit
# (unless STRANGER_KEEP=1).
#
# It does NOT cover daemon mode (§5.2), MCP wiring, or the synthesis
# pipeline. Those have separate gate tests (scheduler smoke, MCP round-trip,
# synthesis isolation). This script's job is the first 60 seconds of UX.

set -u

# --- locate binary ------------------------------------------------------------

if [[ -z "${MPM_BIN:-}" ]]; then
    if [[ -x "./bin/mpm" ]]; then
        MPM_BIN="$(pwd)/bin/mpm"
    elif [[ -x "$(dirname "$0")/../bin/mpm" ]]; then
        MPM_BIN="$(cd "$(dirname "$0")/.." && pwd)/bin/mpm"
    else
        echo "❌ MPM_BIN not set and ./bin/mpm not found. Build with 'make build' first." >&2
        exit 2
    fi
fi

if [[ ! -x "$MPM_BIN" ]]; then
    echo "❌ MPM_BIN=$MPM_BIN is not executable" >&2
    exit 2
fi

VERBOSE="${STRANGER_VERBOSE:-0}"

# --- isolated workspace -------------------------------------------------------

SCRATCH="$(mktemp -d -t mpm-stranger.XXXXXX)" || { echo "❌ mktemp failed" >&2; exit 2; }
export HOME="$SCRATCH/home"
export MPM_WORKSPACE="$SCRATCH/workspace"
mkdir -p "$HOME" "$MPM_WORKSPACE"

cleanup() {
    if [[ "${STRANGER_KEEP:-0}" = "1" ]]; then
        echo "[stranger] scratch kept at $SCRATCH (STRANGER_KEEP=1)"
    else
        rm -rf "$SCRATCH"
    fi
}
trap cleanup EXIT

# --- helpers ------------------------------------------------------------------

# Run an mpm command, fail loudly on unexpected exit or output.
# Usage:
#   step "label" "expected-substring" -- mpm <args...>            # exit 0 expected
#   step "label" "expected-substring" "expected-exit-code" -- mpm <args...>
#   step "label" -- mpm <args...>                                  # no substring check
#   step "label" "" "expected-exit-code" -- mpm <args...>          # exit code only
step() {
    local label="$1"; shift
    local expected=""
    local expected_exit="0"
    # Two-pass: peel off expected-substring and/or expected-exit-code before the "--".
    for _ in 1 2; do
        if [[ "${1:-}" = "--" ]]; then
            shift
            break
        fi
        # If the next arg is a single digit (0-9), treat as exit code; otherwise substring.
        if [[ "${1:-}" =~ ^[0-9]+$ ]]; then
            expected_exit="$1"; shift
        else
            expected="$1"; shift
        fi
    done
    # Consume the trailing "--" separator if it's still the first arg.
    [[ "${1:-}" = "--" ]] && shift
    if [[ "${VERBOSE}" = "1" ]]; then
        echo "[stranger] $label"
        echo "[stranger]   \$ $* (expected exit $expected_exit)"
    fi
    local out
    local rc
    out="$("$@" 2>&1)"
    rc=$?
    if [[ "$rc" != "$expected_exit" ]]; then
        echo "❌ [$label] unexpected exit code: got $rc, expected $expected_exit" >&2
        echo "  command: $*" >&2
        echo "  output:" >&2
        echo "$out" | sed 's/^/    /' >&2
        exit 1
    fi
    if [[ -n "$expected" ]] && ! grep -qF "$expected" <<<"$out"; then
        echo "❌ [$label] output missing expected substring: $expected" >&2
        echo "  command: $*" >&2
        echo "  output:" >&2
        echo "$out" | sed 's/^/    /' >&2
        exit 1
    fi
}

# --- walk the docs/SPEC.md §5.3 quickstart -----------------------------------------

echo "[stranger] binary=$MPM_BIN"
echo "[stranger] HOME=$HOME"
echo "[stranger] MPM_WORKSPACE=$MPM_WORKSPACE"
echo

# 1. status on a brand-new install — must not error, must report empty store.
step "1. status (empty install)" "0 total" -- "$MPM_BIN" status

# 2. add a memory — must succeed and return an id.
step "2. mpm add (store memory)" "Added memory" -- \
    "$MPM_BIN" add "Stranger-test memory: token alpha bravo charlie"

# 3. single-token recall — must return the just-added memory.
step "3. mpm recall <token>" "alpha bravo charlie" -- \
    "$MPM_BIN" recall "alpha"

# 4. --semantic without an embedding provider must fail clean (exit 1,
#    human-readable error) rather than crash or silently fall through.
#    The earlier type-mismatch bug and silent FTS5 fallback were both
#    silent UX failures for first-time users.
step "4. mpm recall --semantic (no embedding provider → clean error)" \
    "No embedding provider configured" "1" -- \
    "$MPM_BIN" recall --semantic "alpha"

# 5. mpm wake — must return recent context containing the stored memory.
step "5. mpm wake (last context)" "alpha bravo charlie" -- \
    "$MPM_BIN" wake

# 6. mpm ops stats — must report non-zero memory count.
step "6. mpm ops stats" "Memories:" -- \
    "$MPM_BIN" ops stats

# 7. cognitive verb: propose_theory — must succeed and return a theory id.
step "7. mpm propose_theory" "Theory proposed" -- \
    "$MPM_BIN" propose_theory "HYPOTHESIS: stranger test passes
VALIDATION_CRITERIA: this script exits 0
STATUS: pending"

# 8. cognitive verb: record_decision — must succeed.
step "8. mpm record_decision" "Decision recorded" -- \
    "$MPM_BIN" record_decision "CONTEXT: stranger test running
CHOICE: validate quickstart
RATIONALE: gate alpha release"

# 9. config profile add (non-interactive creates empty profile).
step "9. mpm config profile add (non-interactive)" 'profile "stranger" added' -- \
    "$MPM_BIN" config profile add stranger

# 10. config profile list shows the new profile.
step "10. mpm config profile list" "stranger" -- \
    "$MPM_BIN" config profile list

# 11. status reflects the writes from steps 2, 7, 8, 9.
step "11. mpm status (after writes)" "Memories:" -- \
    "$MPM_BIN" status

# 12. doctor — final health check.
#     Per cmd/mpm/handlers_doctor.go:12-18 the Wave 2 doctor encodes
#     WARN as exit 1 by design ("This lets scripts use `mpm doctor &&
#     proceed` reliably AND `mpm doctor || handle-warning`"). A fresh
#     install legitimately reports warnings (no embedding provider
#     configured, spaced-review backlog from prior tests) — these are
#     correct signals, not failures. Exit 2 (any FAIL) is a genuine
#     failure and still aborts the test.
if [[ "${VERBOSE}" = "1" ]]; then
    echo "[stranger] 12. mpm doctor"
    echo "[stranger]   \$ $MPM_BIN doctor (expected exit 0 or 1; 2 = abort)"
fi
doctor_out="$("$MPM_BIN" doctor 2>&1)"
doctor_rc=$?
if [[ "$doctor_rc" -eq 2 ]]; then
    echo "❌ [12. mpm doctor] unexpected exit code: got $doctor_rc (FAIL)" >&2
    echo "  command: $MPM_BIN doctor" >&2
    echo "  output:" >&2
    echo "$doctor_out" | sed 's/^/    /' >&2
    exit 1
fi
if [[ "$doctor_rc" -ne 0 && "$doctor_rc" -ne 1 ]]; then
    echo "❌ [12. mpm doctor] unexpected exit code: got $doctor_rc (expected 0, 1, or 2)" >&2
    echo "  command: $MPM_BIN doctor" >&2
    echo "  output:" >&2
    echo "$doctor_out" | sed 's/^/    /' >&2
    exit 1
fi

echo
echo "✅ stranger test passed (12 steps, scratch cleaned up)"
echo
echo "Resolved during the 2026-08-08 alpha audit:"
echo "  - Semantic search type mismatch: weight column scanned as int"
echo "    despite SQLite REAL storage. Fixed by switching ftsEntry.Weight,"
echo "    HybridResult.Weight, and hybridEntry.Weight to float64."
echo "  - Silent --semantic fallback: when no embedding provider was set,"
echo "    HybridSearch returned a degraded FTS5 result without telling the"
echo "    user. Fixed by checking DefaultEmbeddingConfig().ProviderName"
echo "    and returning a clean error before the search runs."
echo "  - LIKE fallback Scan mismatch: when FTS5 fails to init (hermetic"
echo "    install / no compile flag), the LIKE fallback SELECT had 9"
echo "    columns but the Scan destination in handleRecall has 10 args."
echo "    The first mpm recall on a fresh install crashed at runtime."
echo "    Fixed by adding metadata to the LIKE SELECT to match the FTS5"
echo "    column order."
