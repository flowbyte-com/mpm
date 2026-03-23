#!/bin/bash

# Persona Activate - Activate a persona (show content for adoption)
# Usage: ./persona-activate.sh <persona-name>

# Detect correct workspace path
if [ -d "$HOME/.openclaw/workspace" ]; then
    WORKSPACE="$HOME/.openclaw/workspace"
elif [ -d "$HOME/.picoclaw/workspace" ]; then
    WORKSPACE="$HOME/.picoclaw/workspace"
else
    WORKSPACE="$HOME/.openclaw/workspace"
fi

PERSONAS_DIR="$WORKSPACE/MPM/persona"
SCRIPT_DIR="$(dirname "$(readlink -f "$0")")"

# Validate identifier
validate_id() {
    local id="$1"
    if [[ ! "$id" =~ ^[a-zA-Z0-9_-]+$ ]]; then
        echo "❌ Invalid persona name: '$id'" >&2
        return 1
    fi
    return 0
}

# Support SymAI-style opcodes (~p.name)
case "$1" in
    ~p.clear|~p.none|~p.reset)
        DB_FILE="$PERSONAS_DIR/personas.db"
        if [ -f "$DB_FILE" ]; then
            sqlite3 "$DB_FILE" "UPDATE personas SET active=0;"
        fi
        echo "🎭 Persona cleared"
        echo -e "\033[1;36m≈ FLOWBYTE ≈\033[0m ~p. (base)"
        exit 0
        ;;
    ~p.*)
        # Extract name after ~p.
        NAME="${1#*~p.}"
        ;;
    *)
        NAME="$1"
        ;;
esac

if [ -z "$NAME" ]; then
    cat <<'EOF'
Usage: persona-activate.sh <persona-name>
       ~p.<shortcut>

Shortcuts:
  ~p.<name>     Direct activate
  ~p.clear      Clear persona (base mode)

Available personas:
EOF
    if [ -f "$SCRIPT_DIR/persona-ls.sh" ]; then
        "$SCRIPT_DIR/persona-ls.sh" 2>/dev/null | head -20
    else
        ls -1 "$PERSONAS_DIR"/*.persona 2>/dev/null | while read -r f; do echo "  $(basename "$f" .persona)"; done
    fi
    exit 1
fi

# Validate name
validate_id "$NAME" || exit 1

FILE="$PERSONAS_DIR/${NAME}.persona"

if [ ! -f "$FILE" ]; then
    echo "❌ Persona not found: $NAME"
    echo ""
    echo "Available personas:"
    ls -1 "$PERSONAS_DIR"/*.persona 2>/dev/null | while read -r f; do echo "  $(basename "$f" .persona)"; done | head -10
    exit 1
fi

# Let the robust Manager handle the DB, active.json, and the Terminal Pulse!
if [ -f "$SCRIPT_DIR/persona-manager.sh" ]; then
    "$SCRIPT_DIR/persona-manager.sh" activate "$NAME"
fi

# Show Vibe Check (first section)
echo -e "\033[1;36m🎭 Current Frequency:\033[0m ~p.$NAME"
echo -e "\033[0;36m═══════════════════════════════════════════════════════════════\033[0m"

# Extract and show Identity section
id_name=$(grep -E '^\- ?\*\*Name:\*\*' "$FILE" | head -1 | sed -E 's/^\- ?\*\*[A-Za-z]+:\*\* ?//')
id_vibe=$(grep -E '^\- ?\*\*Vibe:\*\*' "$FILE" | head -1 | sed -E 's/^\- ?\*\*[A-Za-z]+:\*\* ?//')
id_emoji=$(grep -E '^\- ?\*\*Emoji:\*\*' "$FILE" | head -1 | sed -E 's/^\- ?\*\*[A-Za-z]+:\*\* ?//')
[ -n "$id_name" ] && echo "📝 $id_name ${id_emoji:+ $id_emoji}"
[ -n "$id_vibe" ] && echo "🎨 $id_vibe"
echo ""

echo -e "\033[0;36m📄 Persona Active\033[0m"
echo -e "\033[0;36m═══════════════════════════════════════════════════════════════\033[0m"

# Export to show full content for adoption
head -50 "$FILE" | sed 's/^/   /'
[ "$(wc -l < "$FILE")" -gt 50 ] && echo "   ... (truncated)"

echo ""
echo -e "\033[1;36m≈ FLOWBYTE ≈\033[0m ~p.$NAME"
echo ""
echo "Mode shortcuts: ~m+<name>  |  ~m.clr  |  mpm status"
