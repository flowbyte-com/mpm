#!/usr/bin/env bash
# pre_critic_snapshot.sh — atomic snapshot of MPM DB before a Critic audit cycle.
#
# Why: gives a hard undo button if the Critic goes rogue and shreds high-confidence
# memories. Costs ~zero (SQLite .backup handles WAL correctly and doesn't block writers).
#
# Usage: pre_critic_snapshot.sh [label]
#   label: optional suffix for the snapshot filename (default: epoch)

set -euo pipefail

# Resolve project root relative to this script's location.
SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &>/dev/null && pwd)"
PROJECT_ROOT="$(cd -- "${SCRIPT_DIR}/.." &>/dev/null && pwd)"
DB_PATH="${MPM_DB_PATH:-${PROJECT_ROOT}/src/db/mpm.db}"
BACKUP_DIR="${PROJECT_ROOT}/backups/critic-pre"
ROTATION_KEEP="${CRITIC_SNAPSHOT_KEEP:-7}"

mkdir -p "${BACKUP_DIR}"

if [[ ! -f "${DB_PATH}" ]]; then
    echo "ERR: DB not found at ${DB_PATH}" >&2
    exit 1
fi

LABEL="${1:-$(date +%s)}"
SNAPSHOT="${BACKUP_DIR}/mpm_pre_critic_${LABEL}.db"

# Atomic SQLite snapshot via the .backup command — handles WAL correctly and
# does not block concurrent writers. This is the right tool for snapshotting
# while MPM MCP is live.
sqlite3 "${DB_PATH}" ".backup '${SNAPSHOT}'"

# Verify the snapshot is sane before trusting it.
INTEGRITY=$(sqlite3 "${SNAPSHOT}" "PRAGMA integrity_check;" 2>&1 || true)
if [[ "${INTEGRITY}" != "ok" ]]; then
    echo "ERR: snapshot integrity_check failed: ${INTEGRITY}" >&2
    rm -f "${SNAPSHOT}"
    exit 1
fi

SIZE=$(stat -c%s "${SNAPSHOT}")
echo "OK: ${SNAPSHOT} (${SIZE} bytes, integrity=ok)"

# Rotation: keep only the most recent N snapshots. Sort by mtime, drop the rest.
#
# Pre-fix this used `SNAPSHOTS=($(ls -1t ...))`, which is two-fold
# broken: (a) the unquoted command substitution into an array performs
# word-splitting, mangling any snapshot filename that contains a
# space or glob metacharacter; (b) `2>/dev/null || true` swallows the
# error from a no-match glob, masking the "no snapshots" case from
# callers who want to know whether a rotation actually happened.
#
# `mapfile -t` reads lines into the array verbatim, preserving
# filenames exactly as `ls` reports them. We use `--` on `ls` to
# guard against snapshot names that begin with a dash. The
# no-match case becomes a single-element array containing the empty
# string, which we explicitly filter out below.
mapfile -t SNAPSHOTS < <(ls -1t -- "${BACKUP_DIR}"/mpm_pre_critic_*.db 2>/dev/null)
# Filter out empty entries (can happen when the glob has no matches
# and the process substitution emits an empty line).
FILTERED=()
for s in "${SNAPSHOTS[@]}"; do
    [[ -n "$s" ]] && FILTERED+=("$s")
done
if (( ${#FILTERED[@]} > ROTATION_KEEP )); then
    for OLD in "${FILTERED[@]:${ROTATION_KEEP}}"; do
        rm -f -- "${OLD}"
        echo "ROTATED: ${OLD}"
    done
fi