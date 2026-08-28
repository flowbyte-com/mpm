#!/usr/bin/env bash
# dogfood-skill-workshop.sh — end-to-end dogfood: publish a real
# skill, then surface it via proactive_recall_hint to prove the
# discovery loop works.
#
# Usage: ./scripts/dogfood-skill-workshop.sh

set -euo pipefail

# Ensure the worktree-built mpm (with skill workshop) is on PATH.
PATH="$HOME/.mpm/bin:$PATH"

# 1. Publish a non-obvious recurring procedure (e.g., the docs
# reorganization pattern that produced the workshop spec itself).
PAYLOAD=$(mktemp)
trap 'rm -f "$PAYLOAD"' EXIT
cat > "$PAYLOAD" <<EOF
{
  "mode": "form",
  "decision_model": {"reusability": 2, "non_obviousness": 2, "stability": 2, "leverage": 2, "boundary": "procedure"},
  "proposal": {
    "name": "doc-archive-cross-reference-sweep",
    "version": "1.0.0",
    "domain": "docs",
    "description": "sweep cross-references after moving docs into an archive subdirectory",
    "when_to_use": "reorganizing documentation, moving files into archive subdirectory, updating cross-references, fixing git mv renamed file references",
    "steps": [{"call": "git mv docs/ docs/archive/"}, {"call": "grep -rl old-path docs/ | xargs sed -i ..."}]
  }
}
EOF

RESULT=$(cat "$PAYLOAD" | mpm skill workshop 2>/dev/null)
OUTCOME=$(echo "$RESULT" | jq -r .outcome)
if [ "$OUTCOME" != "published" ]; then
    echo "FATAL: workshop did not publish: $RESULT" >&2
    exit 1
fi
SKILL_NAME=$(echo "$RESULT" | jq -r '.skill_id' | sed 's/^skill://' | sed 's/-v[0-9].*$//')
echo "OK: published $SKILL_NAME"

# 2. Phrase the same topic differently and ask proactive_recall_hint
# to surface the skill — proves discovery works.
# Response shape: {success, count, hints: [{name, version, content, score, type}, ...]}
# Items use .name not .skill_id; wrapped in .hints[] not top-level array.
HINT=$(mpm call mpm_context --payload '{"action": "proactive_recall_hint", "params": {"conversation_text": "I need to reorganize the docs folder and update all cross-references to the moved files", "max_hints": 5}}')
SURFACED=$(echo "$HINT" | jq -r --arg name "$SKILL_NAME" '.hints[] | select(.name == $name) | .name')
if [ -z "$SURFACED" ]; then
    echo "FATAL: proactive_recall_hint did not surface $SKILL_NAME" >&2
    echo "Hint response: $HINT" >&2
    exit 1
fi
echo "OK: $SKILL_NAME surfaced via proactive_recall_hint"

echo "OK: dogfood passed end-to-end"
