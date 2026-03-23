#!/bin/bash

# ⚡ Mode Selector - Modern FZF Picker (Multi-Select)
# Usage: ./mode-select.sh [--add|--remove|--clear]

# Detect correct workspace path
if [ -d "$HOME/.openclaw/workspace" ]; then
    WORKSPACE="$HOME/.openclaw/workspace"
elif [ -d "$HOME/.picoclaw/workspace" ]; then
    WORKSPACE="$HOME/.picoclaw/workspace"
else
    WORKSPACE="$HOME/.openclaw/workspace"
fi

MODES_DIR="$WORKSPACE/MPM/mode"
DB_FILE="$MODES_DIR/m3.db"
SCRIPT_DIR="$(cd "$(dirname "$(readlink -f "$0")")" && pwd)"
ACTION="${1:---add}"

case "$ACTION" in
    ~m+|~m?+) ACTION="--add" ;;
    ~m-|~m?-) ACTION="--remove" ;;
    ~m.clr|~m--|clr) ACTION="--clear" ;;
    ~m.ls|~m!ls|~m.act) ACTION="--list" ;;
esac

if [ "$ACTION" == "--clear" ]; then
    [ -f "$SCRIPT_DIR/mode-stack.sh" ] && "$SCRIPT_DIR/mode-stack.sh" clear
    exit 0
fi

if [ "$ACTION" == "--list" ]; then
    [ -f "$SCRIPT_DIR/mode-stack.sh" ] && "$SCRIPT_DIR/mode-stack.sh" list
    exit 0
fi

if ! command -v fzf &> /dev/null; then
    echo -e "❌ \033[1;31mMissing Dependency:\033[0m The modern picker requires 'fzf'."
    echo "   Install: sudo apt install fzf"
    echo ""
    echo "Fallback to list mode:"
    [ -f "$SCRIPT_DIR/mode-stack.sh" ] && "$SCRIPT_DIR/mode-stack.sh" list
    exit 1
fi

# Wrap the loop in a function to perfectly preserve newlines
build_list() {
    shopt -s nullglob
    for file in "$MODES_DIR"/*.mode; do
        name=$(basename "$file" .mode)

        title=$(grep -m 1 "^title:" "$file" | cut -d':' -f2-)
        if [ -z "$title" ]; then
            title=$(grep -m 1 "^# Mode:" "$file" | cut -d':' -f2-)
        fi
        title=$(echo "$title" | tr -d '*\r-' | xargs)
        title="${title:-${name^}}"

        orig_size=$(stat -c%s "$file" 2>/dev/null || stat -f%z "$file" 2>/dev/null)
        bytecode_size=0
        if [ -f "$DB_FILE" ]; then
            bytecode_size=$(sqlite3 "$DB_FILE" "SELECT length(bytecode_signature) FROM modes WHERE name='$name';" 2>/dev/null)
        fi
        bytecode_size=${bytecode_size:-0}

        density_str=""
        if [ "$orig_size" -gt 0 ] && [ "$bytecode_size" -gt 0 ]; then
            saved=$((orig_size - bytecode_size))
            pct=$((saved * 100 / orig_size))
            density_str=" [${pct}% Dense]"
        fi

        is_active=$(sqlite3 "$DB_FILE" "SELECT name FROM active_modes WHERE name='$name';" 2>/dev/null)
        if [ -n "$is_active" ]; then
            indicator="🟢 [STACKED]"
        else
            indicator="   "
        fi

        display_text="${title}${density_str}"
        printf "%-15s %-35s %s\n" "$name" "$display_text" "$indicator"
    done
    shopt -u nullglob
}

# Capture the full output (newlines intact)
LIST=$(build_list)

if [ -z "$LIST" ]; then
    echo "❌ No modes found in $MODES_DIR"
    echo ""
    echo "To create your first mode:"
    echo "  mpm mode create <name>"
    exit 1
fi

SELECTION=$(echo -e "$LIST" | fzf \
    --multi \
    --prompt="🛠️ Stack Modes > " \
    --header=" Use TAB to select multiple behaviors, then hit ENTER. " \
    --pointer="▶" \
    --layout=reverse \
    --border=rounded \
    --height=20 \
    --info=hidden \
    --color="border:#5eacd3,pointer:#5eacd3,prompt:#5eacd3,header:#888888")

FZF_EXIT=$?

if [ $FZF_EXIT -eq 0 ]; then
    # Clear current stack first (for re-selection mode)
    [ -f "$DB_FILE" ] && sqlite3 "$DB_FILE" "DELETE FROM active_modes;" 2>/dev/null

    if [ -n "$SELECTION" ]; then
        echo "$SELECTION" | while IFS= read -r line; do
            CHOICE=$(echo "$line" | awk '{print $1}')
            if [ -n "$CHOICE" ]; then
                [ -f "$SCRIPT_DIR/mode-stack.sh" ] && "$SCRIPT_DIR/mode-stack.sh" activate "$CHOICE" >/dev/null 2>&1
            fi
        done
    fi

    echo ""
    echo -e "\033[1;32m╔═══════════════════════════════════════════════════════════════╗\033[0m"
    echo -e "\033[1;32m║  ≈ MPM STATE UPDATED ≈                                       ║\033[0m"
    echo -e "\033[1;32m╚═══════════════════════════════════════════════════════════════╝\033[0m"
    [ -f "$SCRIPT_DIR/mode-stack.sh" ] && "$SCRIPT_DIR/mode-stack.sh" list
else
    echo "Selection cancelled."
fi
