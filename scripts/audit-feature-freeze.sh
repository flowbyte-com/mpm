#!/usr/bin/env bash
# audit-feature-freeze.sh — verify the workshop introduces zero new
# tables/columns. Runs the workshop through one published outcome
# and diffs sqlite_master before/after.
#
# Usage: ./scripts/audit-feature-freeze.sh

set -euo pipefail

# Ensure the worktree-built mpm (with skill workshop) is on PATH.
PATH="$HOME/.mpm/bin:$PATH"

WORKSPACE="${MPM_WORKSPACE:-$HOME/.mpm}"
DB="$WORKSPACE/src/db/mpm.db"
if [ ! -f "$DB" ]; then
    echo "FATAL: $DB does not exist — run \`mpm init\` first" >&2
    exit 1
fi

# Snapshot sqlite_master before.
BEFORE=$(sqlite3 "$DB" "SELECT type, name, sql FROM sqlite_master ORDER BY type, name")
BEFORE_HASH=$(echo "$BEFORE" | sha256sum)

# Run the workshop: build a small request payload that should
# publish a low-stakes skill.
PAYLOAD=$(mktemp)
trap 'rm -f "$PAYLOAD"' EXIT
cat > "$PAYLOAD" <<EOF
{
  "mode": "form",
  "decision_model": {"reusability": 2, "non_obviousness": 2, "stability": 2, "leverage": 2, "boundary": "procedure"},
  "proposal": {
    "name": "audit-fixture-skill",
    "version": "1.0.0",
    "domain": "audit",
    "description": "fixture for feature-freeze audit",
    "when_to_use": "running the feature-freeze audit, sweeping sqlite_master, checking for schema changes in the workshop pipeline",
    "steps": [{"call": "step1"}]
  }
}
EOF

# Submit via mpm skill workshop (CLI) and capture outcome.
RESULT=$(cat "$PAYLOAD" | mpm skill workshop 2>/dev/null)
OUTCOME=$(echo "$RESULT" | jq -r .outcome)
if [ "$OUTCOME" != "published" ]; then
    echo "FATAL: workshop did not publish: $RESULT" >&2
    exit 1
fi

# Snapshot sqlite_master after.
AFTER=$(sqlite3 "$DB" "SELECT type, name, sql FROM sqlite_master ORDER BY type, name")
AFTER_HASH=$(echo "$AFTER" | sha256sum)

if [ "$BEFORE_HASH" != "$AFTER_HASH" ]; then
    echo "FATAL: schema changed after workshop run" >&2
    diff <(echo "$BEFORE") <(echo "$AFTER") >&2
    exit 1
fi

echo "OK: feature-freeze audit passed (zero schema changes after workshop run)"
