#!/bin/bash

# ⚡ Persona Selector - Modern FZF Picker
# Usage: ./persona-select.sh

WORKSPACE="$HOME/.openclaw/workspace"
PERSONAS_DIR="$WORKSPACE/MPM/persona"
SCRIPT_DIR="$(cd "$(dirname "$(readlink -f "$0")")" && pwd)"
ACTIVE_FILE="$WORKSPACE/MPM/active.json"

if ! command -v fzf &> /dev/null; then
    echo -e "❌ \033[1;31mMissing Dependency:\033[0m The modern picker requires 'fzf'."
    exit 1
fi

CURRENT_ACTIVE=""
if [ -f "$ACTIVE_FILE" ]; then
    CURRENT_ACTIVE=$(grep -o '"persona":"[^"]*' "$ACTIVE_FILE" | cut -d'"' -f4)
fi

# Wrap the loop in a function to perfectly preserve newlines
build_list() {
    shopt -s nullglob
    for file in "$PERSONAS_DIR"/*.persona; do
        name=$(basename "$file" .persona)

        title=$(grep -m 1 "^title:" "$file" | cut -d':' -f2-)
        if [ -z "$title" ]; then
            title=$(grep -i -E "^[[:space:]]*(-[[:space:]]*)?(\*\*)?name\b" "$file" | head -1 | cut -d':' -f2-)
        fi
        title=$(echo "$title" | tr -d '*\r-' | xargs)
        title="${title:-${name^}}"

        if [ "$name" == "$CURRENT_ACTIVE" ]; then
            indicator="🟢 [ACTIVE]"
        else
            indicator="   "
        fi

        printf "%-15s %-20s %s\n" "$name" "$title" "$indicator"
    done
    shopt -u nullglob
}

# Capture the full output (newlines intact)
LIST=$(build_list)

if [ -z "$LIST" ]; then
    echo "❌ No personas found in $PERSONAS_DIR"
    exit 1
fi

SELECTION=$(echo -e "$LIST" | fzf \
    --prompt="🎭 Select Persona > " \
    --pointer="▶" \
    --layout=reverse \
    --border=rounded \
    --height=15 \
    --info=hidden \
    --color="border:#5eacd3,pointer:#5eacd3,prompt:#5eacd3")

if [ -n "$SELECTION" ]; then
    CHOICE=$(echo "$SELECTION" | awk '{print $1}')
    "$SCRIPT_DIR/persona-manager.sh" activate "$CHOICE"
else
    echo "Selection cancelled."
fi
