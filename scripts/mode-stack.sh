#!/bin/bash

# ⚡ FlowByte Mode Stack Manager
# Handles activation, stacking, creation, and DB reads for behaviors

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

# Validate identifier format
validate_id() {
    local id="$1"
    if [[ ! "$id" =~ ^[a-zA-Z0-9_-]+$ ]]; then
        echo "❌ Invalid mode name: '$id' - only alphanumeric, underscore, and hyphen allowed"
        return 1
    fi
    return 0
}

# Escape SQL string
escape_sql() {
    printf '%s' "$1" | sed "s/'/''/g"
}

# Ensure DB exists before reading
if [ ! -f "$DB_FILE" ]; then
    if [ -f "$SCRIPT_DIR/mode-compile.sh" ]; then
        "$SCRIPT_DIR/mode-compile.sh" sync quiet >/dev/null 2>&1
    fi
fi

# Parse SymAI-style opcodes (~m+mode, ~m-mode, ~m.clr)
ACTION="${1:-list}"
NAME="$2"

case "$1" in
    ~m+*) ACTION="activate"; NAME="${1#*~m+}";;
    ~m-*) ACTION="deactivate"; NAME="${1#*~m-}";;
    ~m.clr|~m--|clear) ACTION="clear"; NAME="";;
    ~m\?|list) ACTION="list"; NAME="";;
esac

# Helper: Sync the active.json file for the Python Gateway
sync_active_json() {
    local personadb="$WORKSPACE/MPM/persona/personas.db"
    local p_active=""
    
    if [ -f "$personadb" ]; then
        p_active=$(sqlite3 "$personadb" "SELECT name FROM personas WHERE active=1 LIMIT 1;" 2>/dev/null)
    fi

    # Format modes as a proper JSON array of strings: ["pr","db"]
    local m_active=""
    if [ -f "$DB_FILE" ]; then
        m_active=$(sqlite3 "$DB_FILE" "SELECT '\"' || name || '\"' FROM active_modes ORDER BY stack_order;" 2>/dev/null | tr '\n' ',' | sed 's/,$//')
    fi

    mkdir -p "$WORKSPACE/MPM"
    
    # Build proper JSON
    local json_modes="[${m_active}]"
    [ "$m_active" = "" ] && json_modes="[]"
    
    printf '{"persona":"%s","modes":%s,"updated":"%s"}\n' \
        "${p_active:-default}" "$json_modes" "$(date -Iseconds)" > "$WORKSPACE/MPM/active.json"
}

# Helper: Print the current SymAI Pulse Stack
print_pulse() {
    local stack=""
    if [ -f "$DB_FILE" ]; then
        stack=$(sqlite3 "$DB_FILE" "SELECT name FROM active_modes ORDER BY stack_order;" 2>/dev/null | tr '\n' '&' | sed 's/&$/''/')
    fi
    
    if [ -z "$stack" ]; then
        echo -e "⚡ \033[1;36mSymAI Pulse:\033[0m ~m.base!act"
    else
        echo -e "⚡ \033[1;36mSymAI Pulse:\033[0m ~m.${stack}!act"
    fi
}

case "$ACTION" in
    list|ls|~m!ls|~m?)
        echo -e "\033[1;36m🛠️ FlowByte Mode Stack\033[0m"
        echo "========================================"
        
        if [ ! -f "$DB_FILE" ]; then
            echo "  (No modes database - run 'mpm mode compile')"
            echo "----------------------------------------"
            print_pulse
            exit 0
        fi
        
        local active_count=0
        active_count=$(sqlite3 "$DB_FILE" "SELECT COUNT(*) FROM active_modes;" 2>/dev/null)

        if [ -n "$active_count" ] && [ "$active_count" -gt 0 ]; then
            sqlite3 -column "$DB_FILE" "SELECT '  ✅ ' || m.sym_id, m.title FROM active_modes a JOIN modes m ON a.name = m.name ORDER BY a.stack_order;" 2>/dev/null || echo "  (Active mode data unavailable)"
        else
            echo "  (No active modes - Base Behavior)"
        fi
        echo "----------------------------------------"
        print_pulse
        ;;

    activate|add|~m+|stack)
        if [ -z "$NAME" ]; then 
            cat <<'EOF'
Usage: mpm mode activate <name>
       ~m+<name>

Quicklinks:
  ~m+<name>     Add mode to stack
  ~m.<name>     Load mode (clear others)
  ~m.clr        Clear all modes
EOF
            exit 1
        fi
        
        # Validate mode name
        validate_id "$NAME" || exit 1

        if [ ! -f "$DB_FILE" ]; then
            echo "❌ Mode database not found. Run: mpm mode compile"
            exit 1
        fi

        # 1. JIT Check: Does it exist in DB?
        local exists
        exists=$(sqlite3 "$DB_FILE" "SELECT name FROM modes WHERE name='$NAME';")
        if [ -z "$exists" ]; then
            # Try to JIT compile
            if [ -f "$SCRIPT_DIR/mode-compile.sh" ]; then
                "$SCRIPT_DIR/mode-compile.sh" compile "$NAME" >/dev/null 2>&1
                exists=$(sqlite3 "$DB_FILE" "SELECT name FROM modes WHERE name='$NAME';")
            fi
        fi

        if [ -n "$exists" ]; then
            # Check if already active
            local is_active
            is_active=$(sqlite3 "$DB_FILE" "SELECT name FROM active_modes WHERE name='$NAME';")
            if [ -n "$is_active" ]; then
                echo "⚠️ Mode '$NAME' is already in the stack."
            else
                # ATOMIC stack insertion - calculate next order in-query
                sqlite3 "$DB_FILE" "INSERT INTO active_modes (name, stack_order) VALUES ('$NAME', (SELECT COALESCE(MAX(stack_order), 0) + 1 FROM active_modes));"
                local result=$?
                if [ $result -eq 0 ]; then
                    sync_active_json
                    print_pulse
                    echo "✅ Added mode: $NAME"
                else
                    echo "❌ Failed to add mode (database error)"
                    exit 1
                fi
            fi
        else
            echo "❌ Mode not found: $NAME"
            echo "   Available modes:"
            sqlite3 "$DB_FILE" "SELECT name FROM modes ORDER BY name LIMIT 10;" 2>/dev/null | sed 's/^/   - /' || echo "   (no modes loaded)"
            exit 1
        fi
        ;;

    deactivate|remove|~m-)
        if [ -z "$NAME" ]; then 
            echo "Usage: mpm mode deactivate <name>"
            echo "       ~m-<name>"
            exit 1
        fi
        
        validate_id "$NAME" || exit 1
        
        if [ ! -f "$DB_FILE" ]; then
            echo "❌ No mode database found"
            exit 1
        fi
        
        sqlite3 "$DB_FILE" "DELETE FROM active_modes WHERE name='$NAME';"
        if [ $? -eq 0 ]; then
            sync_active_json
            print_pulse
            echo "✅ Removed mode: $NAME"
        else
            echo "❌ Failed to remove mode"
            exit 1
        fi
        ;;

    clear|~m.clr|~m--)
        if [ ! -f "$DB_FILE" ]; then
            echo "✅ No active modes to clear"
            print_pulse
            exit 0
        fi
        
        sqlite3 "$DB_FILE" "DELETE FROM active_modes;"
        sync_active_json
        print_pulse
        echo "✅ Cleared all modes. Returning to base behavior."
        ;;

    create)
        if [ -z "$NAME" ]; then 
            echo "Usage: mpm mode create <name>"
            echo ""
            echo "Creates a new mode template file."
            exit 1
        fi
        
        validate_id "$NAME" || exit 1
        
        local file="$MODES_DIR/${NAME}.mode"
        if [ -f "$file" ]; then 
            echo "⚠️ Mode already exists: $file"
            exit 1
        fi

        cat <<EOF > "$file"
---
sym_id: ~m.${NAME}
title: ${NAME^}
---
# Mode: ${NAME^}

## Purpose
[Brief description of what this mode does]

## 🛠️ Opcodes
- \`~m.${NAME}!action\`: Primary action description.

## 📝 Rules
- Rule 1
- Rule 2

## Checklist
- [ ] Task 1
- [ ] Task 2

## Best Practices
- Practice 1
- Practice 2

## Anti-Patterns
- Anti-pattern to avoid
EOF
        echo "✨ Created new mode blueprint: $file"
        "$SCRIPT_DIR/mode-compile.sh" compile "$NAME" >/dev/null 2>&1
        echo "✅ Compiled to DB. Type 'mpm mode activate $NAME' or '~m+$NAME' to stack it!"
        ;;

    help|--help|-h)
        cat <<'EOF'
⚡ Mode Stack Manager

USAGE:
  mpm mode [activate|deactivate|list|clear|create] [name]
  ~m[+|-][name]              SymAI shorthand

COMMANDS:
  activate <name>, ~m+<name>  Add mode to stack
  deactivate <name>, ~m-<name> Remove mode from stack  
  clear, ~m.clr                Clear all active modes
  list, ~m!ls, ~m?             Show active stack
  create <name>                Create new mode template

EXAMPLES:
  mpm mode activate review      Add "review" mode to stack
  ~m+review                   Same, using SymAI opcode
  ~m+pr+debug                 Stack multiple modes
  ~m.clr                      Clear all modes

QUICKLINKS:
  ~m                          Open interactive mode selector
  ~m?                         Show this help
  ~m!ls                       List all available modes
  ~m.act                      Show active modes
EOF
        ;;

    *)
        echo "Usage: mpm mode [activate|deactivate|list|clear|create] <name>"
        echo "Shortcuts: ~m+<name>, ~m-<name>, ~m.clr, ~m?"
        echo ""
        echo "Run 'mpm mode help' for full documentation."
        exit 1
        ;;
esac
