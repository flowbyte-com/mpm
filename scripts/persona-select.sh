#!/bin/bash

# ⚡ Persona Selector - Modern FZF Picker
# Usage: ./persona-select.sh [--current|--load]

# Detect correct workspace path
if [ -d "$HOME/.openclaw/workspace" ]; then
    WORKSPACE="$HOME/.openclaw/workspace"
elif [ -d "$HOME/.picoclaw/workspace" ]; then
    WORKSPACE="$HOME/.picoclaw/workspace"
else
    WORKSPACE="$HOME/.openclaw/workspace"
fi

PERSONAS_DIR="$WORKSPACE/MPM/persona"
SCRIPT_DIR="$(cd "$(dirname "$(readlink -f "$0")")" && pwd)"
ACTIVE_FILE="$WORKSPACE/MPM/active.json"

if ! command -v fzf &> /dev/null; then
    echo -e "❌ \033[1;31mMissing Dependency:\033[0m The modern picker requires 'fzf'."
    echo "   Install: sudo apt install fzf"
    echo ""
    echo "Fallback to basic mode:"
    ls -1 "$PERSONAS_DIR"/*.persona 2>/dev/null | while read -r file; do
        basename "$file" .persona
    done
    exit 1
fi

CURRENT_ACTIVE=""
if [ -f "$ACTIVE_FILE" ]; then
    CURRENT_ACTIVE=$(grep -o '"persona":"[^"]*' "$ACTIVE_FILE" | cut -d'"' -f4)
fi

# Show current active persona
if [ "${1:-}" = "--current" ]; then
    if [ -n "$CURRENT_ACTIVE" ] && [ "$CURRENT_ACTIVE" != "default" ]; then
        echo -e "\033[1;36m🎭 Active Persona:\033[0m $CURRENT_ACTIVE"
        echo ""
        echo "To change: mpm persona select"
        echo "Or use:   ~p"
    else
        echo -e "\033[1;33m⚠️  No active persona\033[0m"
        echo ""
        echo "To activate: mpm persona select"
        echo "Or use:      ~p"
    fi
    exit 0
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
    echo ""
    echo "To create your first persona:"
    echo "  mpm persona create <name>"
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
    if [ -f "$SCRIPT_DIR/persona-activate.sh" ]; then
        "$SCRIPT_DIR/persona-activate.sh" activate "$CHOICE"
    else
        # Fallback: update DB directly
        DB="$PERSONAS_DIR/personas.db"
        if [ -f "$DB" ]; then
            sqlite3 "$DB" "UPDATE personas SET active=0;"
            sqlite3 "$DB" "UPDATE personas SET active=1 WHERE name='$CHOICE';"
            echo "✅ Activated persona: $CHOICE"
        fi
    fi
else
    echo "Selection cancelled."
fi
