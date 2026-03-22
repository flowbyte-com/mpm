#!/bin/bash

# ⚡ FlowByte Mode Stack Manager
# Handles activation, stacking, creation, and DB reads for behaviors

WORKSPACE="$HOME/.openclaw/workspace"
MODES_DIR="$WORKSPACE/MPM/mode"
DB_FILE="$MODES_DIR/m3.db"
SCRIPT_DIR="$(cd "$(dirname "$(readlink -f "$0")")" && pwd)"

# Ensure DB exists before reading
if [ ! -f "$DB_FILE" ]; then
    "$SCRIPT_DIR/mode-compile.sh" sync quiet >/dev/null 2>&1
fi

# Parse SymAI-style opcodes (~m+mode, ~m-mode, ~m.clr)
case "$1" in
    ~m+*) ACTION="activate"; NAME="${1#*~m+}";;
    ~m-*) ACTION="deactivate"; NAME="${1#*~m-}";;
    ~m.clr|~m--|clear) ACTION="clear"; NAME="";;
    ~m\?|list) ACTION="list"; NAME="";;
    *) ACTION="${1:-list}"; NAME="$2";;
esac

# Helper: Sync the active.json file for the Python Gateway
sync_active_json() {
    local p_active=$(sqlite3 "$WORKSPACE/MPM/persona/personas.db" "SELECT name FROM personas WHERE active=1 LIMIT 1;" 2>/dev/null)

    # Format modes as a proper JSON array of strings: ["pr","db"]
    local m_active=$(sqlite3 "$DB_FILE" "SELECT '\"' || name || '\"' FROM active_modes ORDER BY stack_order;" 2>/dev/null | tr '\n' ',' | sed 's/,$//')

    mkdir -p "$WORKSPACE/MPM"
    echo "{\"persona\":\"${p_active:-default}\",\"modes\":[${m_active}],\"updated\":\"$(date -Iseconds)\"}" > "$WORKSPACE/MPM/active.json"
}

# Helper: Print the current SymAI Pulse Stack
print_pulse() {
    local stack=$(sqlite3 "$DB_FILE" "SELECT name FROM active_modes ORDER BY stack_order;" 2>/dev/null | tr '\n' '&' | sed 's/&$//')
    if [ -z "$stack" ]; then
        echo -e "⚡ \033[1;36mSymAI Pulse:\033[0m ~m.base!act"
    else
        echo -e "⚡ \033[1;36mSymAI Pulse:\033[0m ~m.${stack}!act"
    fi
}

case "$ACTION" in
    list)
        echo -e "\033[1;36m🛠️ FlowByte Mode Stack\033[0m"
        echo "========================================"
        active_count=$(sqlite3 "$DB_FILE" "SELECT COUNT(*) FROM active_modes;" 2>/dev/null)

        if [ -n "$active_count" ] && [ "$active_count" -gt 0 ]; then
            sqlite3 -column "$DB_FILE" "SELECT '  ✅ ' || m.sym_id, m.title FROM active_modes a JOIN modes m ON a.name = m.name ORDER BY a.stack_order;" 2>/dev/null
        else
            echo "  (No active modes - Base Behavior)"
        fi
        echo "----------------------------------------"
        print_pulse
        ;;

    activate|add)
        if [ -z "$NAME" ]; then echo "Usage: mpm mode activate <name>"; exit 1; fi

        # 1. JIT Check: Does it exist in DB?
        exists=$(sqlite3 "$DB_FILE" "SELECT name FROM modes WHERE name='$NAME';" 2>/dev/null)
        if [ -z "$exists" ]; then
            # Try to JIT compile it just in case they just dropped the file
            "$SCRIPT_DIR/mode-compile.sh" compile "$NAME" >/dev/null 2>&1
            exists=$(sqlite3 "$DB_FILE" "SELECT name FROM modes WHERE name='$NAME';" 2>/dev/null)
        fi

        if [ -n "$exists" ]; then
            # Check if already active
            is_active=$(sqlite3 "$DB_FILE" "SELECT name FROM active_modes WHERE name='$NAME';" 2>/dev/null)
            if [ -n "$is_active" ]; then
                echo "⚠️ Mode '$NAME' is already in the stack."
            else
                # Calculate next stack order
                current_max=$(sqlite3 "$DB_FILE" "SELECT MAX(stack_order) FROM active_modes;" 2>/dev/null)
                next_order=$(( ${current_max:-0} + 1 ))

                sqlite3 "$DB_FILE" "INSERT INTO active_modes (name, stack_order) VALUES ('$NAME', $next_order);" 2>/dev/null
                sync_active_json
                print_pulse
            fi
        else
            echo "❌ Mode not found: $NAME"
            exit 1
        fi
        ;;

    deactivate|remove)
        if [ -z "$NAME" ]; then echo "Usage: mpm mode deactivate <name>"; exit 1; fi
        sqlite3 "$DB_FILE" "DELETE FROM active_modes WHERE name='$NAME';" 2>/dev/null
        sync_active_json
        print_pulse
        ;;

    clear)
        sqlite3 "$DB_FILE" "DELETE FROM active_modes;" 2>/dev/null
        sync_active_json
        print_pulse
        echo "✅ Cleared all modes. Returning to base behavior."
        ;;

    create)
        if [ -z "$NAME" ]; then echo "Usage: mpm mode create <name>"; exit 1; fi
        file="$MODES_DIR/${NAME}.mode"
        if [ -f "$file" ]; then echo "⚠️ Mode already exists: $file"; exit 1; fi

        cat <<EOF > "$file"
---
sym_id: ~m.${NAME}
title: ${NAME^}
---
# Mode: ${NAME^}
## 🛠️ Opcodes
- \`~m.${NAME}!action\`: Primary action description.

## 📝 Rules
- Rule 1
- Rule 2

## Checklist
- [ ] Task 1
- [ ] Task 2
EOF
        echo "✨ Created new mode blueprint: $file"
        "$SCRIPT_DIR/mode-compile.sh" compile "$NAME" >/dev/null 2>&1
        echo "✅ Compiled to DB. Type '~m+$NAME' to stack it!"
        ;;

    *)
        echo "Usage: mpm mode [list|activate|deactivate|clear|create] <name>"
        echo "Shortcuts: ~m+<name>, ~m-<name>, ~m.clr"
        exit 1
        ;;
esac
